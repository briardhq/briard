package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"briard.io/shared/dashboard"
)

// The record a seed makes, for every shape an OS account arrives in.
func TestTheUsersRecordFromItsSeed(t *testing.T) {
	for _, c := range []struct {
		why                  string
		seed                 dashboard.Handoff
		user, name, language string
	}{
		{"a Linux account with a passwd name", dashboard.Handoff{Username: "kostas", Name: "Kostas Chatzikokolakis", Language: "el"}, "kostas", "Kostas Chatzikokolakis", "el"},
		{"a Windows account named in capitals", dashboard.Handoff{Username: "Kostas", Name: "Kostas C", Language: "EL"}, "kostas", "Kostas C", "el"},
		{"no display name: the capitalised username", dashboard.Handoff{Username: "ana", Language: "pt"}, "ana", "Ana", "pt"},
		{"the display name is only the username (os/user's fallback)", dashboard.Handoff{Username: "ana", Name: "ana"}, "ana", "Ana", "en"},
		{"root says nothing about the person", dashboard.Handoff{Username: "root", Name: "System administrator", Language: "en"}, "owner", "Owner", "en"},
		{"nothing known", dashboard.Handoff{}, "owner", "Owner", "en"},
		{"a username in another script keeps its real name", dashboard.Handoff{Username: "Κώστας", Name: "Κώστας Χ.", Language: "el"}, "owner", "Κώστας Χ.", "el"},
		{"what apps refuse is dropped, a letter first", dashboard.Handoff{Username: "9John.Doe-2_x"}, "johndoe-2_x", "Johndoe-2_x", "en"},
		{"a language that is not two letters", dashboard.Handoff{Username: "bo", Language: "eng"}, "bo", "Bo", "en"},
		{"a language with a digit", dashboard.Handoff{Username: "bo", Language: "e1"}, "bo", "Bo", "en"},
	} {
		acct, err := newAccount(c.seed)
		if err != nil {
			t.Fatal(err)
		}
		if acct.Username != c.user || acct.Name != c.name || acct.Language != c.language {
			t.Errorf("%s: got %q / %q / %q, want %q / %q / %q", c.why, acct.Username, acct.Name, acct.Language, c.user, c.name, c.language)
		}
		if !regexp.MustCompile(`^[a-z2-7]{16}$`).MatchString(acct.ID) {
			t.Errorf("%s: id %q is not sixteen lowercase base32 characters", c.why, acct.ID)
		}
	}
	a, _ := newAccount(dashboard.Handoff{})
	b, _ := newAccount(dashboard.Handoff{})
	if a.ID == b.ID {
		t.Error("two records were minted the same id")
	}
}

// THE RECORD IS WRITTEN ONCE: a later open's seed is ignored, and a file without an id -- the
// handoff-shaped account an older dashboard kept -- is no record and is replaced.
func TestTheUsersRecordIsWrittenOnce(t *testing.T) {
	a := newApp("", "", t.TempDir(), "")
	if _, err := a.account(); err == nil {
		t.Fatal("a fresh dashboard has a record")
	}
	old := []byte(`{"code":"","name":"Kostas","username":"kostas","language":"el"}`)
	if err := os.WriteFile(filepath.Join(a.statePath, accountFile), old, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.account(); err == nil {
		t.Fatal("a record with no id counted")
	}
	if err := a.keepAccount(dashboard.Handoff{Username: "kostas", Name: "Kostas", Language: "el"}); err != nil {
		t.Fatal(err)
	}
	first, err := a.account()
	if err != nil || first.Username != "kostas" {
		t.Fatalf("after the first open: %+v, %v", first, err)
	}
	if err := a.keepAccount(dashboard.Handoff{Username: "someone", Name: "Someone Else", Language: "fr"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := a.account(); again != first {
		t.Fatalf("a later open rewrote the record: %+v, was %+v", again, first)
	}
}
