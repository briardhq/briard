package guestagent

import (
	"context"
	"errors"
	"testing"
)

// TestTheTimezoneIsLinkedByReplacement: the zone must be in the image's database, and the link is
// made beside the old one and renamed over it.
func TestTheTimezoneIsLinkedByReplacement(t *testing.T) {
	f := &fakeExec{}
	run := func(name string, args ...string) error {
		_, err := f.Run(context.Background(), name, args...)
		return err
	}
	if err := setTimezone(context.Background(), f, run, "Europe/Athens"); err != nil {
		t.Fatal(err)
	}
	if !f.ran("test", "-f", "/etc/zoneinfo/Europe/Athens") {
		t.Errorf("the zone was not looked up in the database: %v", f.runs)
	}
	if !f.ran("ln", "-sfn", "/etc/zoneinfo/Europe/Athens", "/etc/localtime.briard") || !f.ran("mv", "-Tf", "/etc/localtime.briard", "/etc/localtime") {
		t.Errorf("/etc/localtime was not replaced by rename: %v", f.runs)
	}
}

// TestATimezoneThatIsNotANameIsRefused: the name becomes a path, so anything that is not an IANA
// name is refused before anything runs.
func TestATimezoneThatIsNotANameIsRefused(t *testing.T) {
	for _, zone := range []string{"", "../../etc/shadow", "/etc/zoneinfo/UTC", "Europe/../UTC", "UTC; rm -rf /", "Europe//Athens"} {
		f := &fakeExec{}
		if err := setTimezone(context.Background(), f, func(string, ...string) error { return nil }, zone); err == nil {
			t.Errorf("%q was accepted", zone)
		}
		if len(f.runs) != 0 {
			t.Errorf("%q ran %v before being refused", zone, f.runs)
		}
	}
}

// TestAZoneMissingFromTheImageIsAnError: a link to nothing reads as UTC everywhere, so it is never
// made.
func TestAZoneMissingFromTheImageIsAnError(t *testing.T) {
	f := &fakeExec{runFn: func(name string, _ []string) ([]byte, error) {
		if name == "test" {
			return nil, errors.New("exit status 1")
		}
		return nil, nil
	}}
	run := func(name string, args ...string) error {
		_, err := f.Run(context.Background(), name, args...)
		return err
	}
	if err := setTimezone(context.Background(), f, run, "Mars/Olympus_Mons"); err == nil {
		t.Fatal("a zone the image does not have was linked")
	}
	if f.ran("mv", "-Tf", "/etc/localtime.briard", "/etc/localtime") {
		t.Fatal("/etc/localtime was replaced")
	}
}
