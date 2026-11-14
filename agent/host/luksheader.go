package host

import (
	"bytes"
	"io"
	"os"
	"path/filepath"

	"briard.io/shared/atomicfile"
)

// The data volume's LUKS header, kept beside the volume: ~16 MB whose loss is the loss of
// everything on the volume, and it sits below DRBD, so unlike the data it does not replicate --
// losing it costs one node and a resync on a flock, and a lone anchor everything. LUKS2 already
// keeps a redundant second header with checksums and recovers from the primary's loss on its own,
// so this defends against the rest: the whole prefix gone, or a disk that came back with its first
// megabytes zeroed.
//
// The host does it, rather than the guest running `cryptsetup luksHeaderBackup` and handing the
// file over, because a header backup is the volume's prefix from byte 0 to the data offset, and
// that offset is not inherited: the guest's storage seam pins it at luksFormat (`--offset`), a
// stated product constant restated here the way /run/briard's paths are. Copying host-side needs
// no new channel verb, no cryptsetup on a Windows box, and no guest up when the copy is wanted.
const (
	// luksHeaderSize is the pinned data offset: everything before it is header + keyslots.
	// It must equal luksDataOffset (32768 512-byte sectors) in agent/guestagent/nodestorage.go,
	// the `--offset` its luksFormat passes.
	luksHeaderSize = 16 << 20
	// luksHeaderName is the backup's name, beside the data image it belongs to, because the two
	// travel together: a data.img moved to another disk without its header is unopenable.
	luksHeaderName = "data-header.img"
)

// luksMagic is LUKS2's on-disk magic at offset 0. Reading it is how the host knows whether this
// node is encrypted at all without asking anyone: a node whose CPU had no AES runs its volume in
// the clear (the AES axis), and there is no header there to keep.
var luksMagic = []byte{'L', 'U', 'K', 'S', 0xba, 0xbe}

// backupLUKSHeader copies the data image's LUKS header beside it, once, and deliberately never
// re-takes it: the header changes only when a keyslot does, a keyslot only when a node is armed,
// and arming owns re-taking it. Re-copying on every bring-up would give a torn live header a route
// over a good backup, the one thing a backup must not have. Best-effort and never fatal to
// bring-up: a node that refuses to serve because it could not write a backup file is a worse node
// than one that serves and says so, loudly.
func backupLUKSHeader(dataDisk string, logf func(string, ...any)) {
	if dataDisk == "" {
		return // no data image (a witness, or a harness that supplies its own disks)
	}
	dst := filepath.Join(filepath.Dir(dataDisk), luksHeaderName)
	if fi, err := os.Stat(dst); err == nil && fi.Size() == luksHeaderSize {
		return // already kept
	}
	f, err := os.Open(dataDisk)
	if err != nil {
		logf("luks header backup: cannot read %s: %v", dataDisk, err)
		return
	}
	defer f.Close()
	head := make([]byte, len(luksMagic))
	if _, err := io.ReadFull(f, head); err != nil {
		logf("luks header backup: cannot read %s: %v", dataDisk, err)
		return
	}
	if !bytes.Equal(head, luksMagic) {
		return // an unencrypted volume has no header to keep -- the AES-less path, not a failure
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		logf("luks header backup: %v", err)
		return
	}
	header := make([]byte, luksHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		logf("luks header backup: %s is shorter than its own header: %v", dataDisk, err)
		return
	}
	// 0600: the header carries the keyslots. They are argon2-wrapped, but on an un-armed node the
	// passphrase that opens them sits in plaintext in a LUKS2 token in this same header: this
	// file opens the volume, and is not one to leave world-readable.
	if err := atomicfile.Write(dst, header, 0o600, 0o700); err != nil {
		logf("luks header backup: could not write %s: %v", dst, err)
		return
	}
	logf("kept the data volume's LUKS header at %s (%d bytes) -- back it up with the flock id", dst, luksHeaderSize)
}
