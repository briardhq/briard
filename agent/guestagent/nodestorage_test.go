package guestagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"briard.io/shared/nodestorage"
)

// aesCPU and plainCPU are the two /proc/cpuinfo answers the AES axis turns on. Real shapes: the
// flag lives on a `flags` (x86) or `Features` (aarch64) line, and the model name is free text --
// which is why plainCPU names a CPU with "aes" in its NAME and must still read as no-AES.
const (
	aesCPU   = "processor\t: 0\nmodel name\t: Common KVM\nflags\t\t: fpu vme pse aes xsave\n"
	plainCPU = "processor\t: 0\nmodel name\t: Praesidio aes 9000\nflags\t\t: fpu vme pse xsave\n"
)

// storageFake is a fakeExec wired for the block-device questions nodeStorage asks: is this device
// LUKS, is that device node there, and does create-md refuse. Everything else succeeds.
type storageFake struct {
	*fakeExec
	isLuks   bool            // `cryptsetup isLuks <dev>` exit 0
	present  map[string]bool // `test -b <path>` exit 0
	mdRefuse bool            // create-md WITHOUT --force refuses (a node with a replica)
}

func newStorageFake(cpuinfo string) *storageFake {
	s := &storageFake{fakeExec: &fakeExec{}, present: map[string]bool{}}
	_ = s.WriteFile("/proc/cpuinfo", []byte(cpuinfo))
	s.runFn = s.storageRun
	return s
}

// storageRun is the default behaviour, kept a method so a test that overrides runFn for one
// command can delegate the rest here rather than restating it.
func (s *storageFake) storageRun(name string, args []string) ([]byte, error) {
	switch {
	case name == "cryptsetup" && len(args) > 1 && args[0] == "isLuks":
		if !s.isLuks {
			return nil, errors.New("exit status 1")
		}
	case name == "test" && len(args) == 2 && args[0] == "-b":
		if !s.present[args[1]] {
			return nil, errors.New("exit status 1")
		}
	case name == "drbdadm" && len(args) > 0 && args[0] == "create-md":
		// The blank-probe: without --force it refuses on a device that already holds metadata
		// (the prompt meets a closed stdin), which is how a returning node is recognised. With
		// --force there is nothing to refuse.
		if s.mdRefuse && args[1] != "--force" {
			return []byte("v09 meta data already in place"), errors.New("exit status 20")
		}
	}
	return nil, nil
}

// ran reports whether the fake saw a command starting with these words.
func (s *storageFake) ran(words ...string) bool {
	for _, r := range s.runs {
		if len(r) < len(words) {
			continue
		}
		if strings.Join(r[:len(words)], " ") == strings.Join(words, " ") {
			return true
		}
	}
	return false
}

// argsOf returns the first run whose words all appear in order at the front, joined -- so a test
// can ask for the luksFormat call rather than the first cryptsetup call of any kind.
func (s *storageFake) argsOf(words ...string) string {
	for _, r := range s.runs {
		if len(r) < len(words) {
			continue
		}
		if strings.Join(r[:len(words)], " ") == strings.Join(words, " ") {
			return strings.Join(r, " ")
		}
	}
	return ""
}

func demoTier(mode nodestorage.Mode) nodestorage.Tier {
	return nodestorage.Tier{Name: nodestorage.TierData, Device: "/dev/vdb", VG: "briardservice", LV: "data", Mode: mode}
}

func demoSpec(mode nodestorage.Mode, fresh bool) nodestorage.Spec {
	return nodestorage.Spec{
		Tiers:    []nodestorage.Tier{demoTier(mode)},
		Resource: nodestorage.Resource{Name: "r0", Config: "RES", FreshInit: fresh},
	}
}

// A BLANK DISK ON AES HARDWARE IS THE SHIPPED PATH: format encrypted, build the seam on the crypt
// device, create metadata, attach, and -- being the seed -- declare UpToDate and arm the one-time
// format.
func TestNodeStorageFreshEncrypted(t *testing.T) {
	f := newStorageFake(aesCPU)
	if err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAuto, true)); err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]string{
		{"modprobe", "dm-mirror"},
		{"cryptsetup", "luksFormat"},
		{"cryptsetup", "token", "import", "--token-id", "0"},
		{"cryptsetup", "open"},
		{"pvcreate", "/dev/mapper/briardservice-crypt"},
		{"vgcreate", "briardservice", "/dev/mapper/briardservice-crypt"},
		{"lvcreate", "-l", "100%FREE", "-n", "data", "briardservice"},
		{"drbdadm", "create-md", "--force", "r0"},
		{"systemctl", "start", "drbd@r0.target"},
		{"drbdadm", "new-current-uuid", "--clear-bitmap", "r0/0"},
	} {
		if !f.ran(want...) {
			t.Errorf("never ran %v; runs = %v", want, f.runs)
		}
	}
	if f.files[resPath("r0")] != "RES" {
		t.Errorf(".res = %q, want the host's rendering", f.files[resPath("r0")])
	}
	if _, ok := f.files[dataFormatMarker]; !ok {
		t.Errorf("the seed left no %s; the volume would never be formatted", dataFormatMarker)
	}
	// The cipher and the pinned data offset are the two things the HOST also depends on: its
	// header backup copies exactly this prefix (agent/host/luksheader.go).
	args := f.argsOf("cryptsetup", "luksFormat")
	if !strings.Contains(args, "--cipher aes-xts-plain64") || !strings.Contains(args, "--offset "+luksDataOffset) {
		t.Errorf("luksFormat args = %q", args)
	}
	// The passphrase is written to a file created 0600 FIRST -- WriteFile's own mode is 0644, so
	// the pre-creation is what keeps a volume key off a world-readable path.
	if !f.ran("install", "-m", "0600", "/dev/null", luksKeyPath) {
		t.Errorf("the key file was not pre-created 0600; runs = %v", f.runs)
	}
	if !f.ran("rm", "-f", luksKeyPath) {
		t.Error("the key file was left behind")
	}
	// ...and the token holds the same passphrase, which is the whole of the clear-key design.
	if !strings.Contains(f.files[luksTokenPath], clearKeyToken) ||
		!strings.Contains(f.files[luksTokenPath], f.files[luksKeyPath]) {
		t.Errorf("token = %q, key = %q", f.files[luksTokenPath], f.files[luksKeyPath])
	}
}

// AES-LESS HARDWARE RUNS IN THE CLEAR, and says so elsewhere (the report card). The load-bearing
// negative: no cryptsetup call at all, and the PV is the raw disk.
func TestNodeStorageFreshWithoutAES(t *testing.T) {
	f := newStorageFake(plainCPU)
	if err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAuto, true)); err != nil {
		t.Fatal(err)
	}
	if f.ran("cryptsetup", "luksFormat") {
		t.Error("a CPU with no aes flag was formatted encrypted")
	}
	if !f.ran("pvcreate", "/dev/vdb") {
		t.Errorf("the PV is not the raw disk; runs = %v", f.runs)
	}
}

// `off` is somebody deciding, and it has to beat the hardware: an AES-capable CPU must still
// leave the volume in the clear.
func TestNodeStorageModeOffBeatsTheHardware(t *testing.T) {
	f := newStorageFake(aesCPU)
	if err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeOff, true)); err != nil {
		t.Fatal(err)
	}
	if f.ran("cryptsetup", "luksFormat") {
		t.Error("mode=off encrypted the volume anyway")
	}
	if !f.ran("pvcreate", "/dev/vdb") {
		t.Errorf("the PV is not the raw disk; runs = %v", f.runs)
	}
}

// Adiantum is the AES-less cipher (c) specified and could not build for want of a channel to the
// guest at boot. This is that channel working.
func TestNodeStorageAdiantum(t *testing.T) {
	f := newStorageFake(plainCPU)
	if err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAdiantum, true)); err != nil {
		t.Fatal(err)
	}
	args := f.argsOf("cryptsetup", "luksFormat")
	if !strings.Contains(args, "--cipher xchacha12,aes-adiantum-plain64") {
		t.Errorf("luksFormat args = %q, want the Adiantum cipher on a CPU with no AES", args)
	}
	if !f.ran("pvcreate", "/dev/mapper/briardservice-crypt") {
		t.Errorf("the PV is not the crypt device; runs = %v", f.runs)
	}
}

// ★ A RETURNING NODE IS NEVER REFORMATTED. Its VG is on its disk, so activating it is the whole
// job: nothing destructive runs, create-md goes WITHOUT --force (and refuses), and the seed-only
// steps are skipped even though this spec still says FreshInit -- because the metadata was not
// created by this run.
func TestNodeStorageReturningNodeTouchesNothing(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.present["/dev/mapper/briardservice-data"] = true
	f.mdRefuse = true
	if err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAuto, true)); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]string{
		{"cryptsetup", "luksFormat"},
		{"pvcreate"},
		{"vgcreate"},
		{"lvcreate"},
		{"drbdadm", "create-md", "--force"},
		{"drbdadm", "new-current-uuid"},
	} {
		if f.ran(forbidden...) {
			t.Errorf("a returning node ran %v; runs = %v", forbidden, f.runs)
		}
	}
	if !f.ran("vgchange", "-ay", "briardservice") {
		t.Errorf("the VG was never activated; runs = %v", f.runs)
	}
	if !f.ran("drbdadm", "create-md", "r0") || !f.ran("systemctl", "start", "drbd@r0.target") {
		t.Errorf("the resource was not brought up; runs = %v", f.runs)
	}
	if _, ok := f.files[dataFormatMarker]; ok {
		t.Error("a returning node armed the one-time format; a reboot would reformat the volume")
	}
}

// A RETURNING ENCRYPTED NODE OPENS ITSELF from the clear-key token, with nothing supplied by
// anybody -- and does not reformat on the way.
func TestNodeStorageReturningEncryptedNodeOpensItself(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.isLuks = true
	f.mdRefuse = true
	// The token cryptsetup will export, and the LV that appears once the VG is activated. The
	// crypt device is NOT present: that is what makes this the open path.
	f.runFn = func(name string, args []string) ([]byte, error) {
		if name == "cryptsetup" && len(args) > 1 && args[0] == "token" && args[1] == "export" {
			_ = f.WriteFile(luksTokenPath, []byte(`{"type":"briard-clear","keyslots":["0"],"briard_passphrase":"s3cret"}`))
			return nil, nil
		}
		if name == "vgchange" {
			f.present["/dev/mapper/briardservice-data"] = true
			return nil, nil
		}
		return f.storageRun(name, args)
	}
	if err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAuto, false)); err != nil {
		t.Fatal(err)
	}
	if !f.ran("cryptsetup", "open", "--key-file", luksKeyPath, "/dev/vdb", "briardservice-crypt") {
		t.Errorf("the volume was not opened from its own token; runs = %v", f.runs)
	}
	if f.files[luksKeyPath] != "s3cret" {
		t.Errorf("the passphrase handed to cryptsetup was %q, not the token's", f.files[luksKeyPath])
	}
	if f.ran("cryptsetup", "luksFormat") {
		t.Error("a returning encrypted node was reformatted")
	}
}

// An ARMED node (v5) has destroyed the clear-key token, so there is nothing to read: refuse and
// say why, rather than opening with an empty passphrase and reporting a corrupt header.
func TestNodeStorageRefusesWhenTheClearKeyIsGone(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.isLuks = true
	f.runFn = func(name string, args []string) ([]byte, error) {
		if name == "cryptsetup" && len(args) > 1 && args[0] == "token" && args[1] == "export" {
			_ = f.WriteFile(luksTokenPath, []byte(`{"type":"systemd-tpm2","keyslots":["1"]}`))
			return nil, nil
		}
		return f.storageRun(name, args)
	}
	err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAuto, false))
	if err == nil || !strings.Contains(err.Error(), clearKeyToken) {
		t.Fatalf("err = %v, want a refusal naming the missing %s token", err, clearKeyToken)
	}
}

// A WITNESS BUILDS NO TIER and still comes up: the .res is written and the resource attached, and
// nothing touches a block device -- there is none.
func TestNodeStorageWitness(t *testing.T) {
	f := newStorageFake(aesCPU)
	spec := nodestorage.Spec{Resource: nodestorage.Resource{Name: "r0", Config: "RES", Diskless: true}}
	if err := nodeStorage(context.Background(), f, spec); err != nil {
		t.Fatal(err)
	}
	if f.files[resPath("r0")] != "RES" {
		t.Errorf(".res = %q", f.files[resPath("r0")])
	}
	for _, forbidden := range [][]string{{"modprobe"}, {"drbdadm", "create-md"}, {"pvcreate"}, {"cryptsetup"}} {
		if f.ran(forbidden...) {
			t.Errorf("a witness ran %v", forbidden)
		}
	}
	if !f.ran("systemctl", "start", "drbd@r0.target") {
		t.Errorf("a witness must still connect; runs = %v", f.runs)
	}
}

// NodeStorage reads the spec off disk, so a document this build cannot understand stops the unit
// rather than being half carried out.
func TestNodeStorageRefusesABadSpec(t *testing.T) {
	f := newStorageFake(aesCPU)
	_ = f.WriteFile(nodestorage.Path, []byte(`{"tiers":[],"resource":{"name":"r0","config":"RES"}}`))
	if err := NodeStorage(context.Background(), f); err == nil {
		t.Error("a diskful spec with no tiers was carried out")
	}
	if len(f.runs) != 0 {
		t.Errorf("a refused spec still ran %v", f.runs)
	}
}

func TestNodeStorageRefusesAMissingSpec(t *testing.T) {
	f := newStorageFake(aesCPU)
	if err := NodeStorage(context.Background(), f); err == nil {
		t.Error("a node with no spec built storage anyway")
	}
}
