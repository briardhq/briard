package hass

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CHANGE DETECTION ([B.167]): what a household did to Home Assistant between two samples, read
// off the two members rather than watched live.
//
// A CLOSED LIST OF SIGNALS, never "the bytes differ". Home Assistant writes its history and entity
// state continuously, so every two samples differ; a detector that said so would put a row on
// every sample. These three are the ones a household changes on purpose, and the list stays
// three: the day event is the safety net for everything else, and an addition needs an argument
// of its own.
//
//   - custom_components/*/manifest.json — a custom integration installed, removed or updated,
//     which covers HACS and manual drops without knowing about HACS.
//   - the YAML a household edits: configuration.yaml, automations.yaml, scripts.yaml, scenes.yaml
//     and packages/.
//   - the domains in .storage/core.config_entries — an integration added or removed in the UI.

// signalYAML maps each top-level YAML signal to the phrase a change to it reads as.
var signalYAML = map[string]string{
	"configuration.yaml": "Changed configuration",
	"automations.yaml":   "Changed automations",
	"scripts.yaml":       "Changed scripts",
	"scenes.yaml":        "Changed scenes",
}

const (
	customComponents = "custom_components"
	packagesDir      = "packages"
	configEntries    = ".storage/core.config_entries"
)

// Signals reads the files Detect compares out of one copy of Home Assistant's config directory —
// a ring member's, which is read-only and does not move under the read. Keys are paths relative
// to dir. A signal that is absent is simply not in the map: a fresh install has no packages/, and
// that is not an error.
func Signals(ctx context.Context, x Executor, dir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	read := func(rel string) {
		if b, err := x.ReadFile(dir + "/" + rel); err == nil {
			out[rel] = b
		}
	}
	for rel := range signalYAML {
		read(rel)
	}
	read(configEntries)
	// A missing directory fails the listing, and means there is nothing in it to compare.
	if ls, err := x.Run(ctx, "ls", "-1", dir+"/"+customComponents); err == nil {
		for _, d := range strings.Fields(string(ls)) {
			read(customComponents + "/" + d + "/manifest.json")
		}
	}
	if found, err := x.Run(ctx, "find", dir+"/"+packagesDir, "-type", "f", "-name", "*.yaml"); err == nil {
		for _, p := range strings.Fields(string(found)) {
			if rel, ok := strings.CutPrefix(p, dir+"/"); ok {
				read(rel)
			}
		}
	}
	return out, nil
}

// Detect names what changed between two samples' signals, as phrases a household reads ("Added
// the Hue integration"), or nothing. Pure: the reading is Signals'.
//
// running says the later sample was taken while Home Assistant RAN, so a custom integration that
// arrived is on disk but not loaded yet; a sample at a start is taken just before it loads.
func Detect(prev, next map[string][]byte, running bool) []string {
	var out []string
	later := ""
	if running {
		later = " (takes effect at next restart)"
	}

	// Custom integrations, keyed by their directory, which is their domain.
	prevCC, nextCC := customs(prev), customs(next)
	var installed, removed, updated []string
	for _, d := range sortedKeys(nextCC) {
		n := nextCC[d]
		switch p, ok := prevCC[d]; {
		case !ok:
			installed = append(installed, n.Name)
		case p.Version != n.Version:
			updated = append(updated, n.Name+" to "+n.Version)
		}
	}
	for _, d := range sortedKeys(prevCC) {
		if _, ok := nextCC[d]; !ok {
			removed = append(removed, prevCC[d].Name)
		}
	}
	out = appendGroup(out, "Installed", installed, "custom integrations", later)
	out = appendGroup(out, "Updated", updated, "custom integrations", later)
	out = appendGroup(out, "Removed", removed, "custom integrations", later)

	// UI integrations, by domain; the first entry's title names it.
	prevIn, nextIn := integrations(prev[configEntries]), integrations(next[configEntries])
	var added, dropped []string
	for _, d := range sortedKeys(nextIn) {
		if _, ok := prevIn[d]; !ok {
			added = append(added, "the "+nextIn[d]+" integration")
		}
	}
	for _, d := range sortedKeys(prevIn) {
		if _, ok := nextIn[d]; !ok {
			dropped = append(dropped, "the "+prevIn[d]+" integration")
		}
	}
	out = appendGroup(out, "Added", added, "integrations", "")
	out = appendGroup(out, "Removed", dropped, "integrations", "")

	// The YAML. configuration.yaml and packages/ are one thing to a household: its configuration.
	var yaml []string
	for rel, phrase := range signalYAML {
		if !same(prev, next, rel) && !slices.Contains(yaml, phrase) {
			yaml = append(yaml, phrase)
		}
	}
	for _, rel := range sortedKeys(union(prev, next)) {
		if strings.HasPrefix(rel, packagesDir+"/") && !same(prev, next, rel) && !slices.Contains(yaml, signalYAML["configuration.yaml"]) {
			yaml = append(yaml, signalYAML["configuration.yaml"])
		}
	}
	sort.Strings(yaml)
	return append(out, yaml...)
}

// Sentence joins Detect's phrases into the one line a history row shows: "Added the Hue
// integration, changed automations".
func Sentence(changes []string) string {
	for i := 1; i < len(changes); i++ {
		r, size := utf8.DecodeRuneInString(changes[i])
		changes[i] = string(unicode.ToLower(r)) + changes[i][size:]
	}
	return strings.Join(changes, ", ")
}

// appendGroup names one or two things, and counts more: a row that lists seven integrations is a
// row nobody reads.
func appendGroup(out []string, verb string, names []string, plural, suffix string) []string {
	switch len(names) {
	case 0:
		return out
	case 1:
		return append(out, verb+" "+names[0]+suffix)
	case 2:
		return append(out, verb+" "+names[0]+" and "+names[1]+suffix)
	}
	return append(out, fmt.Sprintf("%s %d %s%s", verb, len(names), plural, suffix))
}

type custom struct{ Name, Version string }

// customs reads every custom integration's manifest out of a sample's signals. One whose manifest
// does not parse is still there, and is named by its directory.
func customs(sig map[string][]byte) map[string]custom {
	out := map[string]custom{}
	for rel, b := range sig {
		rest, ok := strings.CutPrefix(rel, customComponents+"/")
		if !ok {
			continue
		}
		domain, _, _ := strings.Cut(rest, "/")
		var m struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		_ = json.Unmarshal(b, &m)
		if m.Name == "" {
			m.Name = domain
		}
		out[domain] = custom{clean(m.Name), clean(m.Version)}
	}
	return out
}

// integrations reads the configured domains out of core.config_entries, each named by the title
// of its first entry. An unreadable file reads as none, which can only ever cost a missed row.
func integrations(b []byte) map[string]string {
	var store struct {
		Data struct {
			Entries []struct {
				Domain string `json:"domain"`
				Title  string `json:"title"`
			} `json:"entries"`
		} `json:"data"`
	}
	out := map[string]string{}
	if json.Unmarshal(b, &store) != nil {
		return out
	}
	for _, e := range store.Data.Entries {
		if _, ok := out[e.Domain]; ok || e.Domain == "" {
			continue
		}
		title := e.Title
		if title == "" {
			title = e.Domain
		}
		out[e.Domain] = clean(title)
	}
	return out
}

func same(a, b map[string][]byte, rel string) bool {
	x, okA := a[rel]
	y, okB := b[rel]
	return okA == okB && string(x) == string(y)
}

func union(a, b map[string][]byte) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// clean makes text Home Assistant wrote fit for a history row. It is WRITTEN BY THE WORKLOAD — a
// custom integration names itself, a config entry is titled by whoever set it up — and it ends up
// in a terminal (`briard app history`), so a control character in it is an escape sequence in
// somebody's shell. Unprintables go and the length is capped, as backupName does for HA's marker.
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || b.Len() >= 60 {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
