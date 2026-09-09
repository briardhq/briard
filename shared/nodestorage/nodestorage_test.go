package nodestorage

import (
	"strings"
	"testing"
)

// good is a spec that must pass, so every case below can say what it broke rather than restating
// a whole document.
func good() Spec {
	return Spec{
		Tiers:    []Tier{{Name: TierData, Device: "/dev/vdb", VG: "briard", LV: "data", Mode: ModeAuto}},
		Resource: Resource{Name: "r0", Config: "resource r0 {}\n", FreshInit: true},
	}
}

func TestMapper(t *testing.T) {
	if got := (Tier{VG: "briard", LV: "data"}).Mapper(); got != "/dev/mapper/briard-data" {
		t.Errorf("Mapper() = %q; DRBD is pointed at /dev/mapper/briard-data and would attach nothing", got)
	}
}

// The whole point of refusing a dash: device-mapper doubles it, so Mapper would name a device
// that does not exist. Prove the refusal rather than the escaping -- we do not implement the
// escaping, which is why the character has to be refused.
func TestValidateRefusesEscapedNames(t *testing.T) {
	for _, name := range []string{"br-iard", "br/iard", ""} {
		s := good()
		s.Tiers[0].VG = name
		if err := s.Validate(); err == nil {
			t.Errorf("VG %q accepted; Mapper() would be %q, which is not the device lvcreate built", name, s.Tiers[0].Mapper())
		}
		s = good()
		s.Tiers[0].LV = name
		if err := s.Validate(); err == nil {
			t.Errorf("LV %q accepted; Mapper() would be %q", name, s.Tiers[0].Mapper())
		}
	}
}

func TestValidate(t *testing.T) {
	dup := good()
	dup.Tiers = append(dup.Tiers, dup.Tiers[0])

	noTier := good()
	noTier.Tiers = nil

	witnessWithTier := good()
	witnessWithTier.Resource.Diskless = true
	witnessWithTier.Resource.FreshInit = false

	seedingWitness := good()
	seedingWitness.Tiers = nil
	seedingWitness.Resource.Diskless = true

	badMode := good()
	badMode.Tiers[0].Mode = "aes"

	emptyMode := good()
	emptyMode.Tiers[0].Mode = ""

	relDevice := good()
	relDevice.Tiers[0].Device = "vdb"

	noRes := good()
	noRes.Resource.Name = ""

	noResCfg := good()
	noResCfg.Resource.Config = ""

	unnamed := good()
	unnamed.Tiers[0].Name = ""

	for _, tc := range []struct {
		name string
		spec Spec
		want string // a fragment of the refusal, so the message stays worth reading
	}{
		{"two tiers with one name", dup, "both called"},
		{"diskful with no tier", noTier, "nothing for r0 to attach"},
		{"a witness carrying a tier", witnessWithTier, "builds none"},
		{"a witness seeding a flock", seedingWitness, "cannot seed"},
		{"an unknown cipher", badMode, "not an encryption mode"},
		{"no mode at all", emptyMode, "not an encryption mode"},
		{"a device that is not a path", relDevice, "not a device path"},
		{"no resource", noRes, "names no resource"},
		{"a resource with no .res", noResCfg, "carries no .res"},
		{"a tier with no name", unnamed, "has no name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil {
				t.Fatalf("accepted; the guest would run luksFormat/lvcreate against it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refused with %q, which does not say %q", err, tc.want)
			}
		})
	}

	if err := good().Validate(); err != nil {
		t.Errorf("the shipped shape was refused: %v", err)
	}
	// A witness IS a legitimate spec: the .res and the attach, and no tier at all.
	w := Spec{Resource: Resource{Name: "r0", Config: "resource r0 {}\n", Diskless: true}}
	if err := w.Validate(); err != nil {
		t.Errorf("a diskless witness was refused: %v", err)
	}
}

// Marshal validates, so a spec that could not be built is never written to a file something else
// will act on -- the failure lands on the writer, where the bad input is, not on the reader.
func TestMarshalRefusesAnInvalidSpec(t *testing.T) {
	bad := good()
	bad.Tiers[0].Mode = ""
	if _, err := bad.Marshal(); err == nil {
		t.Error("an invalid spec marshalled; the guest would have been handed it")
	}
}

func TestParseRoundTrip(t *testing.T) {
	raw, err := good().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(raw)
	if err != nil {
		t.Fatalf("the document we just wrote did not parse: %v", err)
	}
	tier, ok := back.Tier(TierData)
	if !ok {
		t.Fatalf("the data tier did not survive the round trip: %+v", back)
	}
	if tier != good().Tiers[0] || back.Resource != good().Resource {
		t.Errorf("round trip changed the spec:\n got %+v\nwant %+v", back, good())
	}
}

// Unknown fields are refused because the host that writes this and the pushed agent that reads it
// ship together: a field one side does not understand means they have drifted, and the operation
// on the far side is destructive.
func TestParseRefusesUnknownFields(t *testing.T) {
	raw := []byte(`{"tiers":[{"name":"data","device":"/dev/vdb","vg":"briard","lv":"data","mode":"auto","stripes":4}],"resource":{"name":"r0","config":"x"}}`)
	if _, err := Parse(raw); err == nil {
		t.Error("a spec with an unknown `stripes` field parsed; a second dm layer would have arrived unnoticed")
	}
}

// Parse validates too: a syntactically fine document that says nothing buildable is refused at
// the point it is read, not at the point it is used.
func TestParseValidates(t *testing.T) {
	if _, err := Parse([]byte(`{"tiers":[],"resource":{"name":"r0","config":"x"}}`)); err == nil {
		t.Error("a diskful spec with no tiers parsed")
	}
}
