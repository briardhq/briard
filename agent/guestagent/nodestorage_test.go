package guestagent

import (
	"context"
	"errors"
	"slices"
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
	isLuks    bool            // `cryptsetup isLuks <dev>` exit 0
	present   map[string]bool // `test -b <path>` exit 0
	mdPresent bool            // `drbdmeta ... dump-md` succeeds: the metadata LV holds DRBD metadata ([B.145c])
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
	case name == "vgs":
		// A 256 MiB PV: 4 MiB extents, 63 of them once LVM has taken its label and metadata area.
		// Two lines, because the real command warns on stderr ahead of the report and Run hands
		// both back together -- the parser has to read the LAST line.
		return []byte("  WARNING: nothing to see here\n  4194304 63\n"), nil
	case name == "drbdmeta" && slices.Contains(args, "dump-md"):
		// THE probe: the DRBD magic on the metadata LV. Garbage (a never-written LV above LUKS)
		// fails it, which is the common case; a wipe-md is answered like any other command.
		if !s.mdPresent {
			return []byte("No valid meta data found"), errors.New("exit status 1")
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
	return nodestorage.Tier{Name: nodestorage.TierData, Device: "/dev/vdb", VG: "briardservice", LV: "data", MetaLV: "metadata", Mode: mode}
}

func demoSpec(mode nodestorage.Mode, fresh bool) nodestorage.Spec {
	return nodestorage.Spec{
		Tiers:    []nodestorage.Tier{demoTier(mode)},
		Resource: nodestorage.Resource{Name: "r0", Device: "/dev/drbd0", Replicated: true, Config: "RES", FreshInit: fresh, MaxPeers: 4},
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
		{"lvcreate", "-l", "61", "-n", "data", "briardservice"},
		{"lvcreate", "-l", "100%FREE", "-n", "metadata", "briardservice"},
		{"mkfs.btrfs", "-f", "/dev/mapper/briardservice-data"},
		{"drbdadm", "create-md", "--max-peers=4", "--force", "r0"},
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
	// THE ORDER IS THE SAFETY: the format runs after the LVs exist and BEFORE DRBD attaches the
	// backing exclusively -- and only this once, on a volume this run created.
	var lv, mkfs, attach int
	for i, r := range f.runs {
		switch {
		case r[0] == "lvcreate" && slices.Contains(r, "metadata"):
			lv = i
		case r[0] == "mkfs.btrfs":
			mkfs = i
		case r[0] == "systemctl" && slices.Contains(r, "drbd@r0.target"):
			attach = i
		}
	}
	if !(lv < mkfs && mkfs < attach) {
		t.Errorf("format out of order (lvcreate %d, mkfs %d, attach %d): %v", lv, mkfs, attach, f.runs)
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
	f.present["/dev/mapper/briardservice-metadata"] = true
	f.mdPresent = true
	if err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAuto, true)); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]string{
		{"cryptsetup", "luksFormat"},
		{"pvcreate"},
		{"vgcreate"},
		{"lvcreate"},
		{"mkfs.btrfs"},
		{"drbdadm", "create-md", "--max-peers=4", "--force"},
		{"drbdadm", "new-current-uuid"},
	} {
		if f.ran(forbidden...) {
			t.Errorf("a returning node ran %v; runs = %v", forbidden, f.runs)
		}
	}
	if !f.ran("vgchange", "-ay", "briardservice") {
		t.Errorf("the VG was never activated; runs = %v", f.runs)
	}
	if !f.ran("drbdmeta", loneProbeDevice, "v09", "/dev/mapper/briardservice-metadata", "flex-external", "dump-md") || !f.ran("systemctl", "start", "drbd@r0.target") {
		t.Errorf("the resource was not brought up; runs = %v", f.runs)
	}
}

// A RETURNING ENCRYPTED NODE OPENS ITSELF from the clear-key token, with nothing supplied by
// anybody -- and does not reformat on the way.
func TestNodeStorageReturningEncryptedNodeOpensItself(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.isLuks = true
	f.mdPresent = true
	// The token cryptsetup will export, and the LV that appears once the VG is activated. The
	// crypt device is NOT present: that is what makes this the open path.
	f.runFn = func(name string, args []string) ([]byte, error) {
		if name == "cryptsetup" && len(args) > 1 && args[0] == "token" && args[1] == "export" {
			_ = f.WriteFile(luksTokenPath, []byte(`{"type":"briard-clear","keyslots":["0"],"briard_passphrase":"s3cret"}`))
			return nil, nil
		}
		if name == "vgchange" {
			f.present["/dev/mapper/briardservice-data"] = true
			f.present["/dev/mapper/briardservice-metadata"] = true
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
	spec := nodestorage.Spec{Resource: nodestorage.Resource{Name: "r0", Device: "/dev/drbd0", Replicated: true, Config: "RES", Diskless: true}}
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

// A JOINER'S FRESH LVs ARE LEFT BLANK: the same blank-disk path builds them, and neither the
// format nor the UpToDate declaration runs, because the resync from the primary is what fills
// them. FreshInit=false is the whole of the difference, and it is the host's to state.
func TestNodeStorageJoinerBuildsButNeverFormats(t *testing.T) {
	f := newStorageFake(aesCPU)
	if err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAuto, false)); err != nil {
		t.Fatal(err)
	}
	if !f.ran("lvcreate", "-l", "61", "-n", "data", "briardservice") || !f.ran("lvcreate", "-l", "100%FREE", "-n", "metadata", "briardservice") {
		t.Errorf("the joiner did not build its LVs; runs = %v", f.runs)
	}
	if !f.ran("drbdadm", "create-md", "--max-peers=4", "--force", "r0") {
		t.Errorf("fresh metadata was not created with --force; runs = %v", f.runs)
	}
	for _, forbidden := range [][]string{{"mkfs.btrfs"}, {"drbdadm", "new-current-uuid"}} {
		if f.ran(forbidden...) {
			t.Errorf("a joiner ran %v; it would split-brain against the primary", forbidden)
		}
	}
}

// A VOLUME FROM BEFORE THE METADATA LV has a data LV and nowhere for DRBD to write: refuse by
// name rather than fail three steps later on a path that reads like a broken .res. The alpha's
// answer is a reinstall, and the message says so.
func TestNodeStorageRefusesAVolumeWithoutAMetadataLV(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.present["/dev/mapper/briardservice-data"] = true
	err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAuto, false))
	if err == nil || !strings.Contains(err.Error(), "reinstall") {
		t.Fatalf("err = %v, want a refusal that says to reinstall", err)
	}
	for _, forbidden := range [][]string{{"lvcreate"}, {"drbdadm"}, {"systemctl"}, {"mkfs.btrfs"}} {
		if f.ran(forbidden...) {
			t.Errorf("ran %v on a volume it had refused", forbidden)
		}
	}
}

// The split is DRBD's requirement rounded up to extents plus one -- so it grows with the disk
// and the peer count, and never rounds to zero.
func TestMetadataExtents(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		dataBytes, extent int64
		peers             int
		want              int64
	}{
		{256 * mib, 4 * mib, 4, 2},       // ~1 MiB needed -> 1 extent, plus the margin
		{1 << 40, 4 * mib, 4, 34},        // 1 TiB x 4 peers = 128 MiB of bitmap, +1 MiB, +1 extent
		{1 << 40, 4 * mib, 1, 10},        // ...and a quarter of that for one peer
		{10 * mib, 4 * mib, 4, 2},        // tiny disks still get a whole extent and the margin
		{3 * (1 << 40), 32 * mib, 4, 14}, // bigger extents round up the same way
	} {
		if got := metadataExtents(tc.dataBytes, tc.extent, tc.peers); got != tc.want {
			t.Errorf("metadataExtents(%d, %d, %d) = %d, want %d", tc.dataBytes, tc.extent, tc.peers, got, tc.want)
		}
	}
}

// A disk too small to hold even the metadata is refused before anything is created on it.
func TestNodeStorageRefusesADiskTooSmallForData(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.runFn = func(name string, args []string) ([]byte, error) {
		if name == "vgs" {
			return []byte("  4194304 2\n"), nil
		}
		return f.storageRun(name, args)
	}
	err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeOff, true))
	if err == nil || !strings.Contains(err.Error(), "too small") {
		t.Fatalf("err = %v, want a refusal naming the size", err)
	}
	if f.ran("lvcreate") {
		t.Error("an LV was created on a disk that cannot hold the layout")
	}
}

// loneSpec is a node that runs no DRBD ([B.145c]): the data LV is the device, and there is no .res.
func loneSpec(fresh bool) nodestorage.Spec {
	s := demoSpec(nodestorage.ModeAuto, fresh)
	s.Resource.Replicated = false
	s.Resource.Config = ""
	s.Resource.Device = s.Tiers[0].Mapper()
	return s
}

// ★ THE LONE ROW: the LVs are built and (as the seed) formatted, the topology word says alone,
// and then NOTHING of DRBD's happens -- no .res, no create-md, no attach, no new-current-uuid.
// The mount is briard-primary-storage's, off the LV the spec names, exactly as on a flock.
func TestNodeStorageLoneNodeRunsNoDRBD(t *testing.T) {
	f := newStorageFake(aesCPU)
	if err := nodeStorage(context.Background(), f, loneSpec(true)); err != nil {
		t.Fatal(err)
	}
	if !f.ran("mkfs.btrfs", "-f", "/dev/mapper/briardservice-data") {
		t.Errorf("the seed's LV was not formatted; runs = %v", f.runs)
	}
	if got := f.files[topologyEnvPath]; got != "BRIARD_TOPOLOGY=alone\n" {
		t.Errorf("topology.env = %q, want the word the hold unit keys on", got)
	}
	if _, ok := f.files[resPath("r0")]; ok {
		t.Error("a lone node wrote a .res it will never attach")
	}
	for _, forbidden := range [][]string{{"drbdadm"}, {"systemctl", "start", "drbd@r0.target"}, {"modprobe", "drbd"}} {
		if f.ran(forbidden...) {
			t.Errorf("a lone node ran %v; runs = %v", forbidden, f.runs)
		}
	}
	// No probe on LVs this run made: garbage by construction, and nothing to refuse over.
	if f.ran("drbdmeta") {
		t.Errorf("a fresh lone node probed its brand-new metadata LV; runs = %v", f.runs)
	}
}

// A returning lone node activates its VG and stops: nothing destructive, and still no DRBD.
func TestNodeStorageReturningLoneNode(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.present["/dev/mapper/briardservice-data"] = true
	f.present["/dev/mapper/briardservice-metadata"] = true
	if err := nodeStorage(context.Background(), f, loneSpec(true)); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]string{{"lvcreate"}, {"mkfs.btrfs"}, {"drbdadm"}} {
		if f.ran(forbidden...) {
			t.Errorf("a returning lone node ran %v; runs = %v", forbidden, f.runs)
		}
	}
	if !f.ran("vgchange", "-ay", "briardservice") {
		t.Errorf("the VG was not activated; runs = %v", f.runs)
	}
}

// ★ THE REFUSE CELL: "alone" in the spec and DRBD metadata on the metadata LV is a node whose
// host has forgotten its flock, and mounting the LV underneath metadata a peer may still be
// replicating against is a split-brain factory. Bring-up stops; the volume is not mounted.
func TestNodeStorageLoneNodeRefusesForeignMetadata(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.present["/dev/mapper/briardservice-data"] = true
	f.present["/dev/mapper/briardservice-metadata"] = true
	f.mdPresent = true
	err := nodeStorage(context.Background(), f, loneSpec(true))
	if err == nil || !strings.Contains(err.Error(), "holds DRBD metadata") {
		t.Fatalf("err = %v; a forgotten flock's data was brought up alone", err)
	}
}

// The word is written on a flock too, so the unit that reads it never finds it missing.
func TestNodeStorageFlockWritesTheTopologyWord(t *testing.T) {
	f := newStorageFake(aesCPU)
	if err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAuto, true)); err != nil {
		t.Fatal(err)
	}
	if got := f.files[topologyEnvPath]; got != "BRIARD_TOPOLOGY=flock\n" {
		t.Errorf("topology.env = %q", got)
	}
}

// ★ THE CONVERT ROW ([B.145d]): a lone node joining its first peer. Its LVs exist and hold THE
// data, the metadata LV holds nothing, and the spec now says replicated and seed. So: no mkfs,
// create-md --force on the metadata LV, the disk up, disconnected and declared UpToDate BEFORE the stock
// target connects anything -- the order that keeps a joiner already dialling from being marked
// UpToDate without a sync ([B.145a]'s lesson).
func TestNodeStorageConvertsALoneNodeToReplicated(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.present["/dev/mapper/briardservice-data"] = true
	f.present["/dev/mapper/briardservice-metadata"] = true
	if err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAuto, true)); err != nil {
		t.Fatal(err)
	}
	if f.ran("mkfs.btrfs") || f.ran("lvcreate") {
		t.Errorf("the conversion touched the data; runs = %v", f.runs)
	}
	var create, up, disconnect, uuid, target int = -1, -1, -1, -1, -1
	for i, r := range f.runs {
		switch {
		case r[0] == "drbdadm" && r[1] == "create-md":
			create = i
			if !slices.Contains(r, "--force") || !slices.Contains(r, "--max-peers=4") {
				t.Errorf("create-md without --force/--max-peers: %v", r)
			}
		case r[0] == "drbdadm" && r[1] == "up":
			up = i
		case r[0] == "drbdadm" && r[1] == "disconnect":
			disconnect = i
		case r[0] == "drbdadm" && r[1] == "new-current-uuid":
			uuid = i
		case r[0] == "systemctl" && slices.Contains(r, "drbd@r0.target"):
			target = i
		}
	}
	if !(create >= 0 && create < up && up < disconnect && disconnect < uuid && uuid < target) {
		t.Errorf("convert out of order (create %d, up %d, disconnect %d, uuid %d, target %d): %v", create, up, disconnect, uuid, target, f.runs)
	}
	if got := f.files[topologyEnvPath]; got != "BRIARD_TOPOLOGY=flock\n" {
		t.Errorf("topology.env = %q", got)
	}
}

// A blank re-joiner with old LVs (a former member wiped by a removal) creates metadata and
// attaches, and NEVER declares itself UpToDate: its data is discarded by the resync, which is
// what "blank join" means. FreshInit=false is what separates it from the convert row.
func TestNodeStorageRejoinerWithOldLVsNeverSeeds(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.present["/dev/mapper/briardservice-data"] = true
	f.present["/dev/mapper/briardservice-metadata"] = true
	if err := nodeStorage(context.Background(), f, demoSpec(nodestorage.ModeAuto, false)); err != nil {
		t.Fatal(err)
	}
	if !f.ran("drbdadm", "create-md", "--max-peers=4", "--force", "r0") {
		t.Errorf("no metadata was created; runs = %v", f.runs)
	}
	if f.ran("drbdadm", "new-current-uuid") || f.ran("mkfs.btrfs") {
		t.Errorf("a re-joiner seeded or formatted; runs = %v", f.runs)
	}
}

// ★ THE DISABLE ROW ([B.145d]): alone, metadata present, and the one-shot intent asserted by an
// unpair. The metadata is wiped (mandatory, not a courtesy: the next enable would find and attach
// it) and the flock's .res goes with it; nothing else of DRBD runs.
func TestNodeStorageDisableWipesTheMetadata(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.present["/dev/mapper/briardservice-data"] = true
	f.present["/dev/mapper/briardservice-metadata"] = true
	f.mdPresent = true
	spec := loneSpec(true)
	spec.Resource.Convert = nodestorage.ConvertDisable
	if err := nodeStorage(context.Background(), f, spec); err != nil {
		t.Fatal(err)
	}
	if !f.ran("drbdmeta", "--force", loneProbeDevice, "v09", "/dev/mapper/briardservice-metadata", "flex-external", "wipe-md") {
		t.Errorf("the metadata was not wiped; runs = %v", f.runs)
	}
	if !f.ran("rm", "-f", resPath("r0")) {
		t.Errorf("the flock's .res was left behind; runs = %v", f.runs)
	}
	for _, forbidden := range [][]string{{"drbdadm"}, {"systemctl", "start", "drbd@r0.target"}, {"mkfs.btrfs"}} {
		if f.ran(forbidden...) {
			t.Errorf("the disable row ran %v; runs = %v", forbidden, f.runs)
		}
	}
	// ...and without the intent the same disk is still refused: the intent is the whole lift.
	g := newStorageFake(aesCPU)
	g.present["/dev/mapper/briardservice-data"] = true
	g.present["/dev/mapper/briardservice-metadata"] = true
	g.mdPresent = true
	if err := nodeStorage(context.Background(), g, loneSpec(true)); err == nil || g.ran("drbdmeta", "--force") {
		t.Errorf("alone + metadata + no intent was not refused (err=%v, runs=%v)", err, g.runs)
	}
}

// The probe reads ABSENT off one phrase and nothing else: metadata a Primary left "unclean"
// fails dump-md too (measured on a rebooted failover survivor, [B.145d]) and is metadata all the
// same -- so a lone spec over it is refused, and only the phrase that means garbage lets it pass.
func TestNodeStorageUncleanMetadataIsStillMetadata(t *testing.T) {
	f := newStorageFake(aesCPU)
	f.present["/dev/mapper/briardservice-data"] = true
	f.present["/dev/mapper/briardservice-metadata"] = true
	f.runFn = func(name string, args []string) ([]byte, error) {
		if name == "drbdmeta" && slices.Contains(args, "dump-md") {
			return []byte("Found meta data is \"unclean\", please apply-al first"), errors.New("exit status 255")
		}
		return f.storageRun(name, args)
	}
	if err := nodeStorage(context.Background(), f, loneSpec(true)); err == nil || !strings.Contains(err.Error(), "holds DRBD metadata") {
		t.Fatalf("err = %v; unclean metadata was read as none, and a forgotten flock's data would have been mounted alone", err)
	}
}
