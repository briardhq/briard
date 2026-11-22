package main

import "briard.io/agent/host"

// The drive: the household's files as a drive letter on the machine Briard runs on. The agent
// says where the drive is and what opens it (the drive directive); the icon maps it, because a
// mapping belongs to a logon session and the icon is the part of Briard that runs in the
// person's. This file is the decision, kept apart from the Windows calls that carry it out
// (drive_windows.go), so it is testable anywhere.

// driveLetters is where the drive goes, in order of preference: B, then the first free letter
// after it. B names nothing on a machine made this century, and it is Briard's initial. A is
// left alone; C is the system's almost everywhere and is asked like any other.
var driveLetters = []string{"B:", "C:", "D:", "E:", "F:", "G:", "H:", "I:", "J:", "K:", "L:", "M:", "N:",
	"O:", "P:", "Q:", "R:", "S:", "T:", "U:", "V:", "W:", "X:", "Y:", "Z:"}

// driveMap is a mapping the icon made or found: the letter and the share it names.
type driveMap struct {
	letter, remote string
	// left says the person disconnected it. The icon does not map it again for as long as it
	// runs: the drive is offered, never insisted on.
	left bool
}

// driveRemote is the share's name as Windows writes it.
func driveRemote(d host.LocalDrive) string { return `\\` + d.Address + `\` + d.Share }

// driveNext decides what one poll does about the drive: `cancel` is a letter to disconnect
// first ("" for none), `add` a mapping to make (zero for none), and `keep` what the icon holds
// from now on if nothing is made. have is what it holds; conn is the share a letter names now
// ("" when it names none), and free reports a letter that names nothing at all.
//
// A drive that is not up is left as it is: Windows shows it as disconnected and brings it back
// by itself, and removing it would close the folders a person has open.
func driveNext(d host.LocalDrive, have driveMap, conn func(letter string) string, free func(letter string) bool) (keep driveMap, cancel string, add driveMap) {
	if !d.Up || have.left {
		return have, "", driveMap{}
	}
	want := driveRemote(d)
	if have.letter != "" {
		switch conn(have.letter) {
		case want:
			return have, "", driveMap{}
		case have.remote:
			// Still the icon's own mapping, and the address moved: the same letter, the new address.
			return have, have.letter, driveMap{letter: have.letter, remote: want}
		case "":
			// The letter names no share any more, and only a person does that.
			have.left = true
			return have, "", driveMap{}
		}
		// The letter names someone else's share now: it is theirs, and the drive goes elsewhere.
	}
	for _, l := range driveLetters {
		if conn(l) == want {
			return driveMap{letter: l, remote: want}, "", driveMap{} // already there, from an icon before this one
		}
	}
	for _, l := range driveLetters {
		if free(l) {
			return have, "", driveMap{letter: l, remote: want}
		}
	}
	return have, "", driveMap{}
}
