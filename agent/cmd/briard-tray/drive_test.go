package main

import (
	"testing"

	"briard.io/agent/host"
)

// letters is a machine's drive letters for a test: each letter to the share it names, "" for a
// letter that is a local disk. A letter absent from it names nothing.
type letters map[string]string

func (m letters) conn(l string) string { return m[l] }
func (m letters) free(l string) bool   { _, used := m[l]; return !used }

var up = host.LocalDrive{Up: true, Address: "192.168.1.50", Share: "Briard", User: "host", Password: "p"}

const there = `\\192.168.1.50\Briard`

func TestDriveNext(t *testing.T) {
	moved := up
	moved.Address = "192.168.1.77"
	for _, tc := range []struct {
		name    string
		d       host.LocalDrive
		have    driveMap
		machine letters
		keep    driveMap
		cancel  string
		add     driveMap
	}{
		{"nothing mapped, B free: B", up, driveMap{}, letters{"C:": ""},
			driveMap{}, "", driveMap{letter: "B:", remote: there}},
		{"B is a disk: the next free letter", up, driveMap{}, letters{"B:": "", "C:": "", "D:": ""},
			driveMap{}, "", driveMap{letter: "E:", remote: there}},
		{"B is someone's share: the next free letter", up, driveMap{}, letters{"B:": `\\nas\backup`, "C:": ""},
			driveMap{}, "", driveMap{letter: "D:", remote: there}},
		{"mapped and still there: nothing", up, driveMap{letter: "B:", remote: there}, letters{"B:": there, "C:": ""},
			driveMap{letter: "B:", remote: there}, "", driveMap{}},
		{"an icon before this one mapped it: adopted, not mapped twice", up, driveMap{}, letters{"C:": "", "D:": there},
			driveMap{letter: "D:", remote: there}, "", driveMap{}},
		{"the address moved: the same letter, cancelled and mapped again", moved, driveMap{letter: "B:", remote: there}, letters{"B:": there, "C:": ""},
			driveMap{letter: "B:", remote: there}, "B:", driveMap{letter: "B:", remote: `\\192.168.1.77\Briard`}},
		{"the person disconnected it: left alone", up, driveMap{letter: "B:", remote: there}, letters{"C:": ""},
			driveMap{letter: "B:", remote: there, left: true}, "", driveMap{}},
		{"and stays left alone on the polls after", up, driveMap{letter: "B:", remote: there, left: true}, letters{"C:": ""},
			driveMap{letter: "B:", remote: there, left: true}, "", driveMap{}},
		{"the letter was given to another share: theirs is kept, the drive goes elsewhere", up, driveMap{letter: "B:", remote: there}, letters{"B:": `\\nas\backup`, "C:": ""},
			driveMap{letter: "B:", remote: there}, "", driveMap{letter: "D:", remote: there}},
		{"the drive is not up: a mapping is left as it is", host.LocalDrive{}, driveMap{letter: "B:", remote: there}, letters{"B:": there},
			driveMap{letter: "B:", remote: there}, "", driveMap{}},
		{"the drive is not up and nothing is mapped: nothing", host.LocalDrive{}, driveMap{}, letters{},
			driveMap{}, "", driveMap{}},
		{"no letter is free: nothing", up, driveMap{}, func() letters {
			m := letters{}
			for _, l := range driveLetters {
				m[l] = ""
			}
			return m
		}(), driveMap{}, "", driveMap{}},
	} {
		keep, cancel, add := driveNext(tc.d, tc.have, tc.machine.conn, tc.machine.free)
		if keep != tc.keep || cancel != tc.cancel || add != tc.add {
			t.Errorf("%s:\n got keep=%+v cancel=%q add=%+v\nwant keep=%+v cancel=%q add=%+v", tc.name, keep, cancel, add, tc.keep, tc.cancel, tc.add)
		}
	}
}

// A is never taken, and B is asked first.
func TestTheDriveGoesToBFirstAndNeverToA(t *testing.T) {
	if driveLetters[0] != "B:" {
		t.Errorf("the first letter asked is %s, want B:", driveLetters[0])
	}
	for _, l := range driveLetters {
		if l == "A:" {
			t.Error("A: is among the letters the drive may take")
		}
	}
}
