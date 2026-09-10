package guestagent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"briard.io/shared/nodestorage"
)

// THE NODE'S BLOCK STORAGE, BUILT FROM A SPEC THE HOST WROTE ([V3b.33](d)).
//
// briard-node-storage.service's ExecStart, on every node at every bring-up. It builds each tier
// the spec names -- open or create LUKS, activate or create the two-LV VG, format the seed's
// volume once -- and then, once for the resource, writes the host-rendered `.res`, creates its
// external metadata and attaches by starting the stock `drbd@<res>.target`. It ends at
// `/dev/drbd0` attached and goes no further: the mount is briard-primary-storage's, on the one
// node that promoted.
//
// IT REPLACES A BOOT UNIT, and the reason is scope rather than tidiness. [V3b.33](b)/(c) built
// the seam from `multi-user.target` with no inputs but what it could read off the machine, so
// there was nowhere for a decision to arrive -- which is why (c) could not build the Adiantum
// opt-in it specified. Storage policy is a node-scoped fact the host holds durably and pushes at
// bring-up (AGENTS §5), so the host writes nodestorage.Path and starts this.
//
// AND IT REPLACES drbd.provision/drbd.up/drbd.init-uptodate, which is what retires
// /run/briard/data.fresh: "is this LV brand new" was an inference from `drbdadm create-md`'s exit
// code, an inference ENCRYPTION BROKE (dm-crypt returns ciphertext for sectors nobody wrote, so
// the blank-probe found "some data" on every fresh encrypted node, [V3b.33](c)). One program now
// runs both `lvcreate` and `create-md`, so it knows in-process and no marker carries the fact
// between two components.

const (
	// luksKeyPath is where the volume key lives for the seconds between generating it and
	// handing it to `cryptsetup luksFormat`. On tmpfs, and removed immediately -- the same
	// bytes then live in the LUKS2 token on the device, which is the whole clear-key design.
	luksKeyPath = "/run/briard/luks.key"
	// luksTokenPath is where `cryptsetup token export` puts the clear-key token for reading.
	// A FILE rather than stdout: Run gives us stdout and stderr combined, and a single warning
	// on stderr would leave us parsing something that is no longer JSON.
	luksTokenPath = "/run/briard/luks-token.json"
	// clearKeyToken is the LUKS2 token type carrying slot 0's passphrase in plaintext JSON, and
	// `cryptsetup luksDump` reading it back is the is-this-node-armed audit surface: an ARMED
	// node (v5) has a keyslot worth stretching and no token of this type.
	clearKeyToken = "briard-clear"
	// luksDataOffset PINS the data offset at 16 MiB in 512-byte sectors, rather than inheriting
	// LUKS2's default. That makes the header size a stated product constant, which is what lets
	// the HOST keep the header backup by copying exactly that prefix (agent/host/luksheader.go)
	// -- no new channel verb, no cryptsetup on a Windows box, no guest that has to be up.
	// ⚠️ The two must agree; luksheader.go's luksHeaderSize is the same number in bytes.
	luksDataOffset = "32768"
	// topologyEnvPath is the shell-readable topology word, beside vip.env. PAIRED with
	// guest-image/configuration.nix's topologyEnvPath.
	topologyEnvPath = "/run/briard/topology.env"
	// loneProbeDevice is the device name drbdmeta wants for its lock file when the metadata
	// probe runs on a node that has no DRBD device at all. It names nothing that exists.
	loneProbeDevice = "/dev/drbd0"
	// chainTarget is the lone node's promotion: the static target carrying the seven chain
	// members in the reactor's order (guest-image/configuration.nix). PAIRED with that name.
	chainTarget = "briard-chain.target"
	// chainRoot is the chain's first member, the mount. Every other member Requires= it
	// transitively (the fold in configuration.nix), so stopping it is the one stop that waits
	// for the whole chain to be down.
	chainRoot = "briard-primary-storage.service"
)

// NodeStorage builds this node's storage from the spec at nodestorage.Path.
//
// The spec is READ rather than passed on the wire because the unit is a separate process from the
// agent that serves the verb: the host writes the document, starts the unit, and systemd owns the
// result -- its status, its journal, its exit code.
func NodeStorage(ctx context.Context, x Executor) error {
	raw, err := x.ReadFile(nodestorage.Path)
	if err != nil {
		return fmt.Errorf("read %s: %w", nodestorage.Path, err)
	}
	spec, err := nodestorage.Parse(raw)
	if err != nil {
		return err
	}
	return nodeStorage(ctx, x, spec)
}

func nodeStorage(ctx context.Context, x Executor, spec nodestorage.Spec) error {
	run := func(name string, args ...string) error {
		out, err := x.Run(ctx, name, args...)
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	// `pvmove`'s transient mirror is a dm-mirror target, and LVM cannot autoload it here: it
	// shells out to /sbin/modprobe, which does not exist on a NixOS guest, so the first
	// conversion dies with "Required device-mapper target(s) not detected in your kernel"
	// (measured, [V3b.33](a)). Loading it now means the module a conversion needs is never the
	// reason one cannot start. A witness builds no tier and converts nothing.
	if len(spec.Tiers) > 0 {
		if err := run("modprobe", "dm-mirror"); err != nil {
			return err
		}
	}

	// FRESH means "this run created the LVs the resource attaches", which is the fact create-md
	// cannot read off an encrypted device.
	fresh := false
	for _, t := range spec.Tiers {
		made, err := buildTier(ctx, x, run, t, spec.Resource.MaxPeers)
		if err != nil {
			return fmt.Errorf("tier %s: %w", t.Name, err)
		}
		if t.Name == nodestorage.TierData {
			fresh = made
		}
	}

	// THE FORMAT, HERE AND ONLY HERE ([B.145a]). The two facts that make it safe are both known
	// in this process: the data LV was created a moment ago by the `lvcreate` above (so there is
	// nothing on it to lose), and the host designated this node the seed of a NEW flock (a
	// joiner's blank LV is left blank, to be filled by the resync). "A reboot can never format"
	// is therefore a property of the branch structure -- `lvcreate` runs only on a disk with no
	// VG -- rather than of a marker carried between two units.
	//
	// It runs BEFORE the attach because DRBD opens its backing device exclusively, and it can run
	// on the backing at all because the metadata is EXTERNAL: byte 0 of the data LV is byte 0 of
	// the replicated device, so the filesystem written here is the one the Primary mounts.
	// Nothing here promotes (architectural invariant 2): this is a block device the node owns
	// outright for a few more lines, not the resource.
	if fresh && spec.Resource.FreshInit {
		data, _ := spec.Tier(nodestorage.TierData)
		if err := run("mkfs.btrfs", "-f", data.Mapper()); err != nil {
			return err
		}
	}

	// THE TOPOLOGY, FOR THE UNITS THAT CANNOT READ THE SPEC ([B.145c]): the hold unit's steps are
	// shell, and they need one word -- flock or alone. Beside vip.env, same lifetime, and written
	// on every node so the word is never missing on the one that reads it.
	if err := x.WriteFile(topologyEnvPath, []byte(topologyEnv(spec.Resource.Replicated))); err != nil {
		return err
	}

	// THE PROBE IS THE DRBD MAGIC, NOT BLANKNESS ([B.145]). Both LVs sit above LUKS, so a never-
	// written metadata LV reads as ciphertext ([B.126]'s trap one layer up); "metadata present"
	// is `drbdmeta dump-md` succeeding, which is exact on garbage. It is what tells the spec ×
	// disk rows apart -- a returning node from a converting one, a plain lone node from a
	// forgotten flock -- and it is not asked on LVs this run just made (garbage by construction)
	// or on a witness (no LV at all).
	mdPresent := false
	if !fresh && !spec.Resource.Diskless {
		mdPresent = metadataPresent(ctx, x, spec)
	}
	if !spec.Resource.Replicated {
		return loneNode(ctx, x, run, spec, mdPresent)
	}

	if err := x.WriteFile(resPath(spec.Resource.Name), []byte(spec.Resource.Config)); err != nil {
		return err
	}

	// THE REPLICATED ROWS ([B.145d]), by what the disk says:
	//   metadata present            -> attach: a returning node, its replica on the persisted
	//                                  volume; never re-created, never re-seeded (a blind
	//                                  --force would split-brain against the peer that kept
	//                                  serving);
	//   no metadata, LVs just made  -> create: a seed or a blank joiner (today's first init);
	//   no metadata, LVs existing   -> CONVERT: a lone node joining its first peer, whose data
	//                                  is THE data ([B.145]); or a blank re-joiner whose old LVs
	//                                  are discarded by the resync. Same command either way.
	// `--force`, because the probe above has already said there is nothing to protect -- the
	// refusal create-md would otherwise raise is the same fact read less precisely.
	//
	// --max-peers EXPLICITLY, because the number is baked into the metadata and drbdadm's
	// default is "the peers this .res names" -- one slot for a node installed alone, and the
	// flock it grows into would need its metadata recreated (shared/nodestorage.MetadataBytes).
	created := false
	if !spec.Resource.Diskless && !mdPresent {
		if err := run("drbdadm", "create-md", "--max-peers="+strconv.Itoa(spec.Resource.MaxPeers), "--force", spec.Resource.Name); err != nil {
			return err
		}
		created = true
	}

	// Declare UpToDate (skip the initial sync) only on a TRUE first init: the designated seed
	// AND metadata this run created -- the seed of a new flock, or the lone node converting
	// (its only copy is the data by definition). A joiner is hard-wired FreshInit=false.
	//
	// ⚠️ BEFORE ANY PEER CAN CONNECT, which is why the disk is attached on its own first and
	// the stock target (attach + connect) comes after. `--clear-bitmap` with a peer CONNECTED
	// declares that peer UpToDate too, with no sync -- the [B.145a] harness lesson, and on a
	// conversion the joiner may already be up and dialling. Attached but not connected, the
	// same command marks only this disk, and the joiner then syncs from it for real.
	if created && spec.Resource.FreshInit {
		if err := run("drbdadm", "attach", spec.Resource.Name); err != nil {
			return err
		}
		if err := run("drbdadm", "new-current-uuid", "--clear-bitmap", spec.Resource.Name+"/0"); err != nil {
			return err
		}
	}

	// Attach + connect, through the STOCK unit: it is what loads the module and what a node
	// administered by hand would use. Idempotent over the attach above (it is `drbdadm adjust`).
	return run("systemctl", "start", "drbd@"+spec.Resource.Name+".target")
}

// metadataPresent is the probe: the DRBD magic on the metadata LV, read by `drbdmeta dump-md`.
// The device argument only names drbdmeta's lock file, so it is the same word whether or not a
// DRBD device exists on this node.
func metadataPresent(ctx context.Context, x Executor, spec nodestorage.Spec) bool {
	data, _ := spec.Tier(nodestorage.TierData)
	_, err := x.Run(ctx, "drbdmeta", loneProbeDevice, "v09", data.MetaMapper(), "flex-external", "dump-md")
	return err == nil
}

// loneNode is the spec × disk rows for a node that runs no DRBD ([B.145c], [B.145d]). The LVs
// are up and, on a first init, formatted, so there is nothing left to build: the mount is
// briard-primary-storage's, exactly as on a flock, off the data LV the spec names. What differs
// is what the metadata LV says:
//   - no metadata -> plain: nothing to do;
//   - metadata, no intent -> REFUSE. A node that was in a flock and whose host has forgotten it
//     (a lost mesh cache degrades to the configured single-peer mesh) looks exactly like this,
//     and mounting the data LV underneath metadata a peer may still be replicating against is a
//     split-brain factory. Bring-up stops here, loudly, until the pairing is restored or the
//     removal is asserted;
//   - metadata, convert=disable -> DISABLE: the host asserted, from the removal verb, that the
//     flock ended with this node its serving, up-to-date member. The metadata is WIPED -- not a
//     courtesy: stale metadata would be found and attached by the next enable -- and the `.res`
//     the flock left in /run goes with it.
func loneNode(ctx context.Context, x Executor, run func(string, ...string) error, spec nodestorage.Spec, mdPresent bool) error {
	if !mdPresent {
		return nil
	}
	data, _ := spec.Tier(nodestorage.TierData)
	if spec.Resource.Convert != nodestorage.ConvertDisable {
		return fmt.Errorf("node storage: %s holds DRBD metadata but the spec says this node is alone -- refusing to bring the volume up outside the flock that metadata belongs to (a forgotten pairing? restore it, or convert explicitly)", data.MetaMapper())
	}
	if err := run("drbdmeta", "--force", loneProbeDevice, "v09", data.MetaMapper(), "flex-external", "wipe-md"); err != nil {
		return err
	}
	return run("rm", "-f", resPath(spec.Resource.Name))
}

// topologyEnv is the one-word file the guest's shell units key on: the hold unit's steps differ
// between a flock (mask the promoter target, demote) and a lone node (stop the chain target,
// unmount), and a unit cannot parse the spec. PAIRED with guest-image/configuration.nix's
// topologyEnvPath and the BRIARD_TOPOLOGY values its scripts compare against.
func topologyEnv(replicated bool) string {
	if replicated {
		return "BRIARD_TOPOLOGY=flock\n"
	}
	return "BRIARD_TOPOLOGY=alone\n"
}

// replicated reads the topology back off the spec the host wrote: the ONE source for "does this
// node run DRBD", consulted by the status verb, the promoter verbs and the deadman. An error is
// "no spec yet" -- a guest the host has not brought up -- and each reader says what it does
// with that.
func replicated(x Executor) (bool, error) {
	raw, err := x.ReadFile(nodestorage.Path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", nodestorage.Path, err)
	}
	spec, err := nodestorage.Parse(raw)
	if err != nil {
		return false, err
	}
	return spec.Resource.Replicated, nil
}

// alone is replicated's answer for the verbs that have a sensible default when the spec is
// missing: only a spec that SAYS alone makes a node alone; no spec reads as a flock, which is
// what every verb did before there were lone nodes.
func alone(x Executor) bool {
	rep, err := replicated(x)
	return err == nil && !rep
}

// buildTier brings one tier up to "the LVs exist", and reports whether it CREATED them.
//
// The two paths are a returning node and a blank disk, and telling them apart is the whole of the
// safety here: a returning node's VG is on its disk (which is why this uses LVM rather than a
// table something would have to rebuild), so activating it is enough, and nothing destructive
// runs. Only a device with no VG on it reaches the format branch.
func buildTier(ctx context.Context, x Executor, run func(string, ...string) error, t nodestorage.Tier, maxPeers int) (bool, error) {
	crypt := cryptDevice(t)

	// A RETURNING ENCRYPTED NODE OPENS ITSELF, and this is the whole of what "clear key" means:
	// slot 0's passphrase is in a LUKS2 token on the same disk, in plaintext JSON. A keyslot
	// cannot hold the volume key unwrapped, but the token area can hold the passphrase that
	// unwraps it -- which is where clevis parks its JWE and systemd-cryptenroll its TPM2
	// metadata -- so a later boot needs nothing from anybody.
	//
	// ⚠️ THE HONEST FRAMING IS "READY TO BE ARMED", NEVER "PROTECTED". An un-armed volume resists
	// physical loss only (theft, RMA, resale); the key is right there. What it buys is that
	// arming later is a keyslot operation instead of a multi-hour migration nobody opts into.
	if isLuks(ctx, x, t.Device) && !blockExists(ctx, x, crypt) {
		pass, err := clearKey(ctx, x, t.Device)
		if err != nil {
			return false, err
		}
		if err := withKeyFile(x, pass, func(key string) error {
			return run("cryptsetup", "open", "--key-file", key, t.Device, cryptName(t))
		}); err != nil {
			return false, err
		}
	}

	// A RETURNING NODE CARRIES ITS VG ON THE DISK -- that is the point of using LVM rather than a
	// table this would have to rebuild -- so activate and stop. A device with no VG makes this a
	// no-op, hence the ignored error: "there is nothing to activate" is the blank-disk case, not
	// a failure.
	_, _ = x.Run(ctx, "vgchange", "-ay", t.VG)
	if blockExists(ctx, x, t.Mapper()) {
		// A volume built before the metadata LV existed has a data LV and nowhere for DRBD to
		// keep its metadata. Refusing here, by name, beats the attach failing three steps later
		// on a path that reads like a broken .res -- and the answer is the alpha's: reinstall.
		if !blockExists(ctx, x, t.MetaMapper()) {
			return false, fmt.Errorf("%s exists but %s does not: this volume predates the metadata LV and cannot be attached; reinstall the node", t.Mapper(), t.MetaMapper())
		}
		return false, nil
	}

	// ── A BLANK DISK ─────────────────────────────────────────────────────────────────────────
	pv := t.Device
	cipher, keyBits, err := resolveCipher(x, t.Mode)
	if err != nil {
		return false, err
	}
	if cipher != "" {
		if err := luksFormat(ctx, x, run, t, cipher, keyBits); err != nil {
			return false, err
		}
		pv = crypt
	}

	// `pvcreate` WITHOUT -f is the blank probe, and it is [B.126]'s idiom rather than a second
	// one: it refuses on ANY existing signature (the prompt hits a closed stdin and aborts) and
	// it fails on a device it cannot read. So "pvcreate succeeded" means the device was readable
	// AND blank -- the distinction a `blkid ||` probe cannot make, which is the mistake that once
	// reformatted a household's replicated volume.
	//
	// A node installed before the seam therefore FAILS here rather than having its volume
	// claimed: its disk already holds DRBD metadata. That is the alpha reinstall-only policy
	// working as intended, not a gap.
	if err := run("pvcreate", pv); err != nil {
		return false, err
	}
	if err := run("vgcreate", t.VG, pv); err != nil {
		return false, err
	}
	// TWO LVs, DATA FIRST ([B.145a]). LVM hands out the lowest free extents, so creating the data
	// LV at "everything but the metadata's share" and then the metadata LV as 100%FREE lands the
	// metadata at the END of the PV with no extent arithmetic -- the placement DRBD gives its
	// internal metadata, spelled in LVM. The share is computed from the whole VG rather than
	// from the data LV it will serve, which over-provisions by the metadata's own footprint and
	// errs in the only direction drbdmeta accepts.
	extent, total, err := vgExtents(ctx, x, t.VG)
	if err != nil {
		return false, err
	}
	meta := metadataExtents(total*extent, extent, maxPeers)
	if meta >= total {
		return false, fmt.Errorf("%s has %d extents of %d bytes and the metadata alone needs %d: the disk is too small for a data volume", t.VG, total, extent, meta)
	}
	if err := run("lvcreate", "-l", strconv.FormatInt(total-meta, 10), "-n", t.LV, t.VG); err != nil {
		return false, err
	}
	if err := run("lvcreate", "-l", "100%FREE", "-n", t.MetaLV, t.VG); err != nil {
		return false, err
	}
	return true, nil
}

// vgExtents reads the VG's extent size (bytes) and extent count, the two numbers the LV split is
// computed from.
//
// The LAST line is parsed rather than the first: Run hands back stdout and stderr together, and
// LVM puts its warnings on stderr ahead of the report -- a leading "WARNING: ..." would otherwise
// be read as the numbers.
func vgExtents(ctx context.Context, x Executor, vg string) (extent, count int64, err error) {
	out, err := x.Run(ctx, "vgs", "--noheadings", "--nosuffix", "--units", "b", "-o", "vg_extent_size,vg_extent_count", vg)
	if err != nil {
		return 0, 0, fmt.Errorf("vgs %s: %w: %s", vg, err, strings.TrimSpace(string(out)))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("vgs %s: cannot read extent size and count from %q", vg, strings.TrimSpace(string(out)))
	}
	if extent, err = strconv.ParseInt(fields[0], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("vgs %s: extent size %q: %w", vg, fields[0], err)
	}
	if count, err = strconv.ParseInt(fields[1], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("vgs %s: extent count %q: %w", vg, fields[1], err)
	}
	return extent, count, nil
}

// metadataExtents is the metadata LV's size in extents: DRBD's requirement for a data device of
// dataBytes with maxPeers slots, rounded up to whole extents, plus one. The extra extent is the
// margin the formula's comment promises -- "computed" is not "byte-tight".
func metadataExtents(dataBytes, extent int64, maxPeers int) int64 {
	need := nodestorage.MetadataBytes(dataBytes, maxPeers)
	return (need+extent-1)/extent + 1
}

// luksFormat formats the tier's raw device and opens it, leaving the passphrase behind in a
// plaintext token so no later boot needs anything from anybody.
func luksFormat(ctx context.Context, x Executor, run func(string, ...string) error, t nodestorage.Tier, cipher, keyBits string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generate the volume key: %w", err)
	}
	pass := base64.StdEncoding.EncodeToString(raw)

	return withKeyFile(x, pass, func(key string) error {
		// ⚠️ A DELIBERATELY CHEAP KDF, and it costs nothing, because the passphrase it stretches
		// is 256 bits of urandom stored IN THE CLEAR two hundred bytes away. A KDF makes GUESSING
		// expensive; nobody has to guess this one. What the default would cost is real: LUKS2's
		// argon2id targets ~1 GiB and two seconds, at every boot of every node, on guests that
		// have 2 GB. Arming (v5) adds a slot with a secret worth stretching and destroys this
		// one, so no future keyslot inherits these parameters.
		if err := run("cryptsetup", "luksFormat", "--type", "luks2", "--batch-mode",
			"--cipher", cipher, "--key-size", keyBits, "--offset", luksDataOffset,
			"--pbkdf", "argon2id", "--pbkdf-force-iterations", "4", "--pbkdf-memory", "32",
			"--pbkdf-parallel", "1", "--key-file", key, t.Device); err != nil {
			return err
		}
		// The token, marshalled by encoding/json so the passphrase is escaped by something that
		// knows the rules. `type` and `keyslots` are LUKS2's mandatory fields; the rest is ours,
		// and cryptsetup stores an unknown token type verbatim without trying to interpret it.
		//
		// --json-file rather than a pipe, because the Executor gives commands /dev/null stdin.
		tok, err := json.Marshal(map[string]any{
			"type":              clearKeyToken,
			"keyslots":          []string{"0"},
			"briard_passphrase": pass,
		})
		if err != nil {
			return err
		}
		if err := writePrivate(x, luksTokenPath, tok); err != nil {
			return err
		}
		defer func() { _, _ = x.Run(ctx, "rm", "-f", luksTokenPath) }()
		if err := run("cryptsetup", "token", "import", "--token-id", "0",
			"--json-file", luksTokenPath, t.Device); err != nil {
			return err
		}
		return run("cryptsetup", "open", "--key-file", key, t.Device, cryptName(t))
	})
}

// resolveCipher turns the tier's MODE into the cipher this guest will actually format with, or
// "" for a tier formatted in the clear. Returned rather than branched on at the call site because
// the AES axis and the operator's choice have to end in one answer.
func resolveCipher(x Executor, mode nodestorage.Mode) (cipher, keyBits string, err error) {
	switch mode {
	case nodestorage.ModeOff:
		return "", "", nil
	case nodestorage.ModeAdiantum:
		// Adiantum is a KERNEL cipher that needs no AES instructions -- the fallback for
		// hardware that has none. A documented opt-in, never promoted: a fleet that splits on
		// the AES axis splits on something hardware-determined and reportable in the machine
		// card, and one that splits on taste does not.
		return "xchacha12,aes-adiantum-plain64", "256", nil
	case nodestorage.ModeAuto:
		aes, err := hasAES(x)
		if err != nil {
			return "", "", err
		}
		if !aes {
			// Hardware without AES acceleration -- Pi 4 and older, pre-AES-NI x86, any TCG
			// host -- runs the volume in the CLEAR and says so in the report card, because a
			// software cipher on the write path of a household's data is a worse trade than an
			// honest report.
			return "", "", nil
		}
		return "aes-xts-plain64", "512", nil
	}
	return "", "", fmt.Errorf("%q is not an encryption mode", mode)
}

// hasAES is THE AES AXIS, checked inside the guest because that is the one place silicon,
// accelerator and CPU model compose into a single answer: under KVM `-cpu max` passes the host's
// own CPU through, under TCG or WHPX it does not, and qemu's default qemu64 hides `aes` outright.
//
// Only the capability lines are read -- `flags` on x86, `Features` on aarch64 -- rather than the
// whole file: a model name is free text, and a CPU whose NAME contained the word would otherwise
// report acceleration it does not have.
func hasAES(x Executor) (bool, error) {
	raw, err := x.ReadFile("/proc/cpuinfo")
	if err != nil {
		return false, fmt.Errorf("read /proc/cpuinfo: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "flags", "Features":
			for _, f := range strings.Fields(rest) {
				if f == "aes" {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// clearKey reads slot 0's passphrase back out of the LUKS2 token a previous boot imported.
func clearKey(ctx context.Context, x Executor, device string) (string, error) {
	if _, err := x.Run(ctx, "cryptsetup", "token", "export", "--token-id", "0",
		"--json-file", luksTokenPath, device); err != nil {
		return "", fmt.Errorf("export the %s token from %s: %w", clearKeyToken, device, err)
	}
	defer func() { _, _ = x.Run(ctx, "rm", "-f", luksTokenPath) }()
	raw, err := x.ReadFile(luksTokenPath)
	if err != nil {
		return "", err
	}
	var tok struct {
		Type       string `json:"type"`
		Passphrase string `json:"briard_passphrase"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return "", fmt.Errorf("parse the token on %s: %w", device, err)
	}
	// An ARMED node (v5) has destroyed this token and holds a secret worth stretching, so it has
	// no clear key to read: refusing here says "this node needs a key from somewhere" rather
	// than opening with an empty passphrase and reporting a corrupt header.
	if tok.Type != clearKeyToken || tok.Passphrase == "" {
		return "", fmt.Errorf("%s holds no %s token: this node cannot open its own volume", device, clearKeyToken)
	}
	return tok.Passphrase, nil
}

// withKeyFile hands fn a path holding pass, and removes it afterwards whatever happens.
//
// A FILE because the Executor gives every command /dev/null stdin, deliberately: `--key-file -`
// has nothing to read. It is created 0600 BEFORE anything is written to it -- WriteFile's own
// mode is 0644, and os.WriteFile applies a mode only when it creates the file, so pre-creating it
// private is what keeps the passphrase off a world-readable path even for the seconds it exists.
func withKeyFile(x Executor, pass string, fn func(path string) error) error {
	if err := writePrivate(x, luksKeyPath, []byte(pass)); err != nil {
		return err
	}
	defer func() { _, _ = x.Run(context.Background(), "rm", "-f", luksKeyPath) }()
	return fn(luksKeyPath)
}

// writePrivate creates path 0600 and writes data to it. See withKeyFile for why the two steps.
func writePrivate(x Executor, path string, data []byte) error {
	if _, err := x.Run(context.Background(), "install", "-m", "0600", "/dev/null", path); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	return x.WriteFile(path, data)
}

// cryptName is the dm name the tier's dm-crypt device takes, and cryptDevice the path it appears
// at. Derived from the tier so a second tier cannot collide with the first, and equal to what the
// seam has always used for the data tier.
//
// ⚠️ The seam invariant tells this device from the LV by its dm TARGET (`crypt` vs `linear`), not
// by its name -- INVARIANTS §13 allows a crypt device and never requires one, because arming is
// per-node (the AES axis) and not fleet-wide.
func cryptName(t nodestorage.Tier) string { return t.VG + "-crypt" }

func cryptDevice(t nodestorage.Tier) string { return "/dev/mapper/" + cryptName(t) }

// isLuks answers "has this device been formatted", which is the question that decides whether a
// node opens what is there or formats something new.
func isLuks(ctx context.Context, x Executor, device string) bool {
	_, err := x.Run(ctx, "cryptsetup", "isLuks", device)
	return err == nil
}

// blockExists is how both idempotency checks are spelled: the crypt device is already open, or
// the LV is already there. `test -b` rather than a stat, because the Executor is the one surface
// the guest's shelling-out goes through and a device node is exactly what we mean.
func blockExists(ctx context.Context, x Executor, path string) bool {
	_, err := x.Run(ctx, "test", "-b", path)
	return err == nil
}
