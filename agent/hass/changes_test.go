package hass

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

func sig(kv ...string) map[string][]byte {
	out := map[string][]byte{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = []byte(kv[i+1])
	}
	return out
}

const entriesHue = `{"data":{"entries":[{"domain":"hue","title":"Philips Hue"},{"domain":"met","title":"Home"}]}}`
const entriesHueZha = `{"data":{"entries":[{"domain":"hue","title":"Philips Hue"},{"domain":"met","title":"Home"},{"domain":"zha","title":"Zigbee"}]}}`

// TestDetectSeesNothingInANoisySample: the whole reason the signals are a closed list. Home
// Assistant rewrites its history and entity state constantly, and none of it is a signal -- so two
// samples whose signals match report nothing however much else differs.
func TestDetectSeesNothingInANoisySample(t *testing.T) {
	a := sig("automations.yaml", "- id: 1\n", configEntries, entriesHue)
	b := sig("automations.yaml", "- id: 1\n", configEntries, entriesHue)
	if got := Detect(a, b, false); len(got) != 0 {
		t.Errorf("Detect(same signals) = %v, want nothing", got)
	}
}

// TestDetectNamesEachSignal: one phrase per kind of change, integrations first, then the YAML.
func TestDetectNamesEachSignal(t *testing.T) {
	prev := sig(
		"automations.yaml", "- id: 1\n",
		"configuration.yaml", "default_config:\n",
		configEntries, entriesHue,
		"custom_components/hacs/manifest.json", `{"name":"HACS","version":"1.0"}`,
		"custom_components/old/manifest.json", `{"name":"Old Thing","version":"1"}`,
	)
	next := sig(
		"automations.yaml", "- id: 1\n- id: 2\n",
		"configuration.yaml", "default_config:\n",
		configEntries, entriesHueZha,
		"custom_components/hacs/manifest.json", `{"name":"HACS","version":"2.0"}`,
		"custom_components/frigate/manifest.json", `{"name":"Frigate","version":"5.0"}`,
	)
	want := []string{
		"Installed Frigate",
		"Updated HACS to 2.0",
		"Removed Old Thing",
		"Added the Zigbee integration",
		"Changed automations",
	}
	if got := Detect(prev, next, false); !slices.Equal(got, want) {
		t.Errorf("Detect = %q\nwant      %q", got, want)
	}
}

// TestDetectSaysWhenACustomIntegrationIsNotLoadedYet: a sample taken while Home Assistant RUNS sees
// the files on disk before Home Assistant has loaded them.
func TestDetectSaysWhenACustomIntegrationIsNotLoadedYet(t *testing.T) {
	next := sig("custom_components/frigate/manifest.json", `{"name":"Frigate"}`)
	got := Detect(sig(), next, true)
	if len(got) != 1 || got[0] != "Installed Frigate (takes effect at next restart)" {
		t.Errorf("Detect(running) = %q", got)
	}
}

// TestDetectCountsWhatIsTooLongToList: onboarding adds a handful of integrations at once, and a row
// that names them all is a row nobody reads.
func TestDetectCountsWhatIsTooLongToList(t *testing.T) {
	next := sig(configEntries, `{"data":{"entries":[{"domain":"a","title":"A"},{"domain":"b","title":"B"},{"domain":"c","title":"C"}]}}`)
	if got := Detect(sig(), next, false); len(got) != 1 || got[0] != "Added 3 integrations" {
		t.Errorf("Detect = %q", got)
	}
}

// TestDetectReadsPackagesAsConfiguration: packages/ is configuration.yaml split into files, so it
// reads as the same change -- once, however many files moved.
func TestDetectReadsPackagesAsConfiguration(t *testing.T) {
	prev := sig("configuration.yaml", "a", "packages/lights.yaml", "x")
	next := sig("configuration.yaml", "b", "packages/lights.yaml", "y", "packages/new.yaml", "z")
	if got := Detect(prev, next, false); !slices.Equal(got, []string{"Changed configuration"}) {
		t.Errorf("Detect = %q", got)
	}
}

// TestSentenceReadsAsOneLine is the row's text.
func TestSentenceReadsAsOneLine(t *testing.T) {
	got := Sentence([]string{"Added the Zigbee integration", "Changed automations"})
	if got != "Added the Zigbee integration, changed automations" {
		t.Errorf("Sentence = %q", got)
	}
}

// fakeFS is a config directory as Signals reads it.
type fakeFS map[string]string

func (f fakeFS) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	dir := args[len(args)-1]
	if name == "find" {
		dir = args[0]
	}
	var out []string
	for p := range f {
		rest, ok := strings.CutPrefix(p, dir+"/")
		if !ok {
			continue
		}
		if name == "ls" {
			d, _, _ := strings.Cut(rest, "/")
			if !slices.Contains(out, d) {
				out = append(out, d)
			}
		} else if strings.HasSuffix(p, ".yaml") {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, os.ErrNotExist
	}
	return []byte(strings.Join(out, "\n")), nil
}
func (fakeFS) WriteFile(string, []byte) error { return nil }
func (f fakeFS) ReadFile(p string) ([]byte, error) {
	if v, ok := f[p]; ok {
		return []byte(v), nil
	}
	return nil, os.ErrNotExist
}

// TestSignalsReadsOnlyTheSignals: the ring member is Home Assistant's whole config directory, and
// the read must take the three signals out of it and nothing else.
func TestSignalsReadsOnlyTheSignals(t *testing.T) {
	d := "/m/app"
	fs := fakeFS{
		d + "/automations.yaml":                                 "a",
		d + "/" + configEntries:                                 entriesHue,
		d + "/custom_components/hacs/manifest.json":             "{}",
		d + "/custom_components/hacs/__init__.py":               "code",
		d + "/packages/x.yaml":                                  "p",
		d + "/home-assistant_v2.db":                             "history",
		d + "/.storage/core.restore_state":                      "state",
		d + "/.storage/auth.corrupt.2026-09-25T10:00:00":        "garbage",
		d + "/home-assistant_v2.db.corrupt.2026-09-25T10:00:00": "a whole database",
	}
	got, err := Signals(context.Background(), fs, d)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{".storage/auth.corrupt.2026-09-25T10:00:00", configEntries, "automations.yaml", "custom_components/hacs/manifest.json",
		"home-assistant_v2.db.corrupt.2026-09-25T10:00:00", "packages/x.yaml"}
	if !slices.Equal(keys, want) {
		t.Errorf("Signals read %q\nwant           %q", keys, want)
	}
}

// TestDetectCleansWhatTheWorkloadWrote: a custom integration names itself, and that name reaches a
// terminal. An escape sequence in it must not.
func TestDetectCleansWhatTheWorkloadWrote(t *testing.T) {
	next := sig("custom_components/evil/manifest.json", `{"name":"Evil\u001b[2J\u009bThing"}`)
	got := Detect(sig(), next, false)
	if len(got) != 1 || strings.ContainsAny(got[0], "\x1b\u009b") || got[0] != "Installed Evil[2JThing" {
		t.Errorf("Detect = %q", got)
	}
}

// TestSignalsNamesACorruptRenameWithoutReadingIt: the file set aside may be the recorder
// database, and its name is the whole signal ([B.167d]).
func TestSignalsNamesACorruptRenameWithoutReadingIt(t *testing.T) {
	d := "/m/app"
	got, err := Signals(context.Background(), fakeFS{d + "/home-assistant_v2.db.corrupt.2026-09-25T10:00:00": "a whole database"}, d)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := got["home-assistant_v2.db.corrupt.2026-09-25T10:00:00"]; !ok || b != nil {
		t.Errorf("Signals = %q; want the rename named and its content unread", got)
	}
}

// TestResetsNamesWhatWasLostOnce: a rename new since the previous sample is a reset, named for what
// the household lost; one already there is not found again, and a second reset of the same store
// is ([B.167d], the renames hass-health-probe measured).
func TestResetsNamesWhatWasLostOnce(t *testing.T) {
	old := ".storage/core.config_entries.corrupt.2026-09-24T09:00:00"
	prev := sig(old, "")
	next := sig(old, "",
		".storage/core.config_entries.corrupt.2026-09-25T10:00:00", "",
		"home-assistant_v2.db.corrupt.2026-09-25T10:00:00", "",
		".storage/core.restore_state", "noise")
	if got, want := Resets(prev, next), "Home Assistant could not read its integrations and history and started without them"; got != want {
		t.Errorf("Resets = %q\nwant     %q", got, want)
	}
	if got := Resets(next, next); got != "" {
		t.Errorf("Resets(same) = %q; an old rename was found again", got)
	}
	if got := Resets(sig(), sig(".storage/some_store.corrupt.x", "")); got != "Home Assistant could not read its some_store and started without them" {
		t.Errorf("an unlisted store reads %q", got)
	}
}
