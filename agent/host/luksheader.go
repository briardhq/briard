package host

import (
	"bytes"
	"io"
	"os"
	"path/filepath"

	"briard.io/shared/atomicfile"
)

// THE DATA VOLUME'S LUKS HEADER, KEPT BESIDE THE VOLUME ([V3b.33](c)).
//
// ~16 MB whose loss is the loss of everything on the volume, and it sits BELOW DRBD, so unlike
// the data it does not replicate: losing it costs one node and a resync on a flock, and costs a
// lone anchor everything. LUKS2 already keeps a redundant SECOND header with checksums and
// recovers from the primary's loss on its own, so what this defends against is the rest -- the
// whole prefix gone, or a disk that came back with its first megabytes zeroed.
//
// WHY THE HOST DOES IT, rather than the guest running `cryptsetup luksHeaderBackup` and handing
// the file over. A header backup IS the prefix of the volume from byte 0 to the data offset, and
// the data offset is not something we inherit -- the guest's seam unit PINS it at luksFormat
// (`--offset`), so it is a stated product constant, restated here the way /run/briard's paths
// are. Copying it host-side needs no new channel verb, no cryptsetup on a Windows box, and no
// guest that is up at the moment the copy is wanted.
const (
	// luksHeaderSize is the pinned data offset: everything before it is header + keyslots.
	// It must equal the guest seam unit's `--offset` (guest-image/configuration.nix).
	luksHeaderSize = 16 << 20
	// luksHeaderName is the backup's name, beside the data image it belongs to, because the two
	// travel together: a data.img moved to another disk without its header is unopenable.
	luksHeaderName = "data-header.img"
)

// luksMagic is LUKS2's on-disk magic at offset 0. Reading it is how the host knows whether this
// node is encrypted at all WITHOUT asking anyone: a node whose CPU had no AES runs its volume in
// the clear (the AES axis), and there is no header there to keep.
var luksMagic = []byte{'L', 'U', 'K', 'S', 0xba, 0xbe}

// backupLUKSHeader copies the data image's LUKS header beside it, ONCE.
//
// ONCE, and deliberately never re-taken: the header changes only when a keyslot does, and a
// keyslot changes only when a node is ARMED (v5, and arming owns re-taking it). Re-copying on
// every bring-up would give a torn live header a route over a good backup, which is the one thing
// a backup must not have.
//
// Best-effort and never fatal to bring-up: a node that refuses to serve because it could not
// write a backup file is a worse node than one that serves and says so. It says so loudly.
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
	// 0600: the header carries the keyslots. They are argon2-wrapped, and on an un-armed node the
	// passphrase that opens them is in a token on the volume anyway -- but a file whose whole
	// purpose is to be the last copy of a key store is not one to leave world-readable.
	if err := atomicfile.Write(dst, header, 0o600, 0o700); err != nil {
		logf("luks header backup: could not write %s: %v", dst, err)
		return
	}
	logf("kept the data volume's LUKS header at %s (%d bytes) -- back it up with the flock id", dst, luksHeaderSize)
}
