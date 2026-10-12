package platform

import (
	"slices"
	"testing"
)

// The guest unit's real rendering, read back: the name it is re-adopted by, its log, and an exec
// that is exactly what follows "--" -- including qemu argv that happens to look like an option.
func TestParseTransientReadsLaunchArgs(t *testing.T) {
	s := QEMUSpec{Binary: "qemu", Accel: "whpx:tcg", ControlSock: "/s", QMPSock: "/q", UnitLog: `C:\log\g.log`}
	got, err := parseTransient(launchArgs(s))
	if err != nil {
		t.Fatal(err)
	}
	if got.unit != GuestUnit {
		t.Errorf("unit = %q, want %q", got.unit, GuestUnit)
	}
	if got.log != `C:\log\g.log` {
		t.Errorf("log = %q, want the spec's UnitLog", got.log)
	}
	if want := launchExec(s); !slices.Equal(got.exec, want) {
		t.Errorf("exec = %v, want launchExec's %v", got.exec, want)
	}
}

// No UnitLog, no log property: the manager's default (the journal on Linux) is left alone.
func TestLaunchArgsNoUnitLogNoProperty(t *testing.T) {
	got, err := parseTransient(launchArgs(QEMUSpec{Binary: "qemu", Accel: "kvm", ControlSock: "/s"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.log != "" {
		t.Errorf("log = %q, want none", got.log)
	}
}

func TestParseTransientRefusesHalfAUnit(t *testing.T) {
	for _, args := range [][]string{
		{"--unit=x"},                       // nothing to run
		{"-p", "Restart=no", "--", "qemu"}, // nothing to find it by
	} {
		if _, err := parseTransient(args); err == nil {
			t.Errorf("parseTransient(%q) accepted half a unit", args)
		}
	}
}
