// Package apps is the per-app registry: which catalogued apps the product knows
// something about beyond their manifest, and what it knows.
//
// Why a registry: a manifest describes what an app is in terms any node can execute without
// judgement, and its schema withholds host binds so that a catalog entry cannot reach the host.
// Some apps still need something the schema cannot say (Home Assistant a control channel minted
// into its container, mosquitto a config file in front of an image that takes no configuration
// from the environment). That knowledge belongs in the product, keyed on the catalog name: a
// curated catalog is exactly the licence to hold it, the same call made for readiness assessors
// (agent/host/readiness.go), for the same reason.
//
// The default is always nothing: an unknown name gets no binds, no preparation and the plain
// HTTP reach line, exactly what its manifest describes. An entry here is how an app gets more;
// nothing here can take anything away. One table in this file holds one entry per app, so what
// the product knows about an app is read in one place, and the functions below read it.
// Deliberately closed and not a plugin system: nothing outside this file adds an entry, the set
// is small by design, and a table keyed on strings that anyone can extend is the shape that
// turns a curated catalog into an expression language.
package apps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"briard.io/agent/appdir"
	"briard.io/agent/files"
	"briard.io/agent/freshrss"
	"briard.io/agent/hass"
	"briard.io/agent/mealie"
	"briard.io/agent/mosquitto"
	"briard.io/agent/navidrome"
	"briard.io/shared/api"
	"briard.io/shared/dashboard"
	"briard.io/shared/manifest"
	"briard.io/shared/routes"
)

// Executor is the guest-side command runner the preparation steps need. Its method set matches
// what each app package declares for itself, so the value passes straight through.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadFile(path string) ([]byte, error)
	Stat(path string) (fs.FileInfo, error)
	ListDir(path string) ([]string, error)
}

// The node-side layout: one directory per app, one socket for the agent.
//
// ⚠️ One socket and a per-app token, not a socket per app. Per-app sockets would make a caller's
// identity a property of the transport, but they make the listener set a function of the app
// list, which only converge knows, and that forces systemd to own the binds, with all the
// machinery that brings (agent/guestagent/inbound.go has the full trade). A token buys the same
// property for a fraction of it: minted per app at converge, written where only that app's
// container can read it, resolved by the agent at request time, so the agent needs no advance
// knowledge of the app list at all. It lives here rather than in agent/hass because it is not one
// app's knowledge, and not in agent/guestagent because that package imports this one and the
// renderer needs the paths.
//
// One directory per app, named for it, mounted read-only as /briard inside that app's container
// and holding everything the product hands that app. The token is simply one of the things in
// it, which is why it costs no mount of its own, the same way Home Assistant's control token, its
// planted integration and its s6 wrapper ride that directory. "The directory name is the mapping"
// is what lets the agent resolve a caller by reading /run/briard rather than keeping a table.
const (
	defaultRunDir = "/run/briard"
	// AppMount is where an app's own directory appears inside its container.
	AppMount = "/briard"
	// InboundTokenName is that directory's token, so InboundTokenMount is simply inside it.
	InboundTokenName  = "inbound.token"
	InboundTokenMount = AppMount + "/" + InboundTokenName
	// InboundMount is the agent's socket inside the container. ⚠️ A sibling of /briard, never a
	// child of it, and the difference is whether the container starts at all. The socket must be
	// read-write (connect(2) needs write permission) while /briard is read-only, and a bind whose
	// destination sits inside another bind's destination makes the runtime create that
	// destination under an already-mounted parent: EROFS here, the container never starts, and
	// podman has left a root-owned directory where a socket belongs, which poisons the path for
	// every later attempt (the exact failure Prepare's own comment describes). /briard/inbound.sock looks right and is exactly this bug;
	// TestNoBindNestsInsideAnother is what keeps the class out rather than this comment.
	InboundMount = "/briard-agent.sock"
)

// RunDir is overridable for tests exactly as agent/guestagent's unitDir is, and for the same
// reason: every path here is absolute on a real guest, so a test that could not move them could
// only ever assert a refusal.
func RunDir() string {
	if d := os.Getenv("BRIARD_RUN_DIR"); d != "" {
		return d
	}
	return defaultRunDir
}

// AppDir is one app's own directory on the node — what AppMount shows it.
func AppDir(app string) string { return RunDir() + "/" + app }

// InboundSocket is the one listener, owned by the long-running guest agent. Beside the app
// directories rather than inside any of them: it is the agent's, shared by every caller.
func InboundSocket() string { return RunDir() + "/agent.sock" }

// UserEnv is the file a container whose env names the user (manifest.UserPlaceholder) takes the
// username from, when it starts: one line, written by Prepare from the user's record. The unit
// names it only for such a container, and a container whose file is missing does not start.
func UserEnv() string { return RunDir() + "/user.env" }

// userEnvName is the variable manifest.UserPlaceholder names.
var userEnvName = strings.TrimSuffix(strings.TrimPrefix(manifest.UserPlaceholder, "${"), "}")

// Username is the username in the user's record, for an app whose first account is named after
// the user. A node nobody has opened has no record and so no name to give. A record whose
// username is not the shape every record is written in is refused: the name is put where an
// app's own start reads it, and nothing wider than that shape is safe there.
func Username(x Executor) (string, error) {
	raw, err := x.ReadFile(dashboard.AccountPath)
	if err != nil {
		return "", dashboard.ErrNoAccount
	}
	acct, err := dashboard.ParseAccount(raw)
	if err != nil {
		return "", err
	}
	if !dashboard.ValidUsername(acct.Username) {
		return "", fmt.Errorf("the user record's username %q is not one an app can be given", acct.Username)
	}
	return acct.Username, nil
}

// InboundTokenPath is where one app's token lives on the node.
func InboundTokenPath(app string) string { return AppDir(app) + "/" + InboundTokenName }

// entry is what the product knows about one app beyond its manifest. Every field's zero value
// is nothing, which is all an app with no entry gets. The functions that read each field say
// what it means and why it is the product's to hold.
type entry struct {
	// volumes are the app's own binds, per container, and prepare writes what they name, on
	// this node, before the container starts (Volumes, Prepare). podGateway is the address the
	// reverse proxy reaches the pod network from.
	volumes func(m manifest.Manifest, c manifest.Container) []string
	prepare func(ctx context.Context, x Executor, m manifest.Manifest, podGateway string) error
	// appDir gives the primary container the app's own directory at AppMount (NeedsAppDir);
	// inbound gives it the inbound channel, and that directory with it (WantsInbound).
	appDir, inbound bool
	// user is who the primary container runs as (User), and capabilities what it gets beyond
	// podman's default set (Capabilities).
	user, capabilities string
	// How the reverse proxy signs the user in. gate is the file of the secret it signs with
	// (Gate), gateHeader the header an app takes the secret itself in (GateHeader),
	// gateByAddress an app that checks no secret but the sender's address (GateByAddress); open
	// are the paths forwarded without the dashboard's session (Open).
	gate, gateHeader string
	gateByAddress    bool
	open             []string
	// householdWrites are the files-tier folders the household fills (HouseholdWrites).
	householdWrites []string
	// unfronted keeps the reverse proxy from serving the app under its name (Fronted); protocol
	// is what a client speaks to its published port (Reach); announce is what the app says of
	// itself on the household LAN, the instance name filled in when asked (Announce).
	unfronted bool
	protocol  string
	announce  []routes.Announcement
	// What the history asks of an app: to hold still for a member (Quiesce), its own health
	// check (HealthOverride), the files that say a restore of its own is in flight
	// (RestoreMarkers), what changed between two members (Detect), what a new member keeps
	// (Sampled), and what it set aside as undecodable (Corrupt).
	quiesce        func(ctx context.Context, x Executor, port int, container string) (release func(context.Context) (bool, error), err error)
	health         func(ctx context.Context, x Executor, port int) Health
	restoreMarkers func(m manifest.Manifest) []string
	detect         func(ctx context.Context, x Executor, m manifest.Manifest, prev, next string, running bool) (changed string, resets []string, err error)
	sampled        func(ctx context.Context, x Executor, m manifest.Manifest, ring []RingMember, member RingMember) error
	corrupt        func(ctx context.Context, x Executor, m manifest.Manifest, root string) []string
}

// known is the registry: one entry per app the product knows something about, keyed on its
// catalog name.
var known = map[string]entry{
	hass.Name: {
		volumes: hass.Volumes,
		// The broker's port travels from one app's package to the other's here, the only place
		// that sees both: Home Assistant needs to be told where the broker is, and neither
		// package may reach into the other (agent/hass would then carry mosquitto knowledge, and
		// a second copy of the port is a second thing to keep in step with the catalog).
		prepare: func(ctx context.Context, x Executor, m manifest.Manifest, _ string) error {
			return hass.Prepare(ctx, x, m, mosquitto.MQTTPort)
		},
		appDir:         true,
		inbound:        true,
		capabilities:   "NET_RAW NET_ADMIN",
		quiesce:        hassQuiesce,
		health:         hassHealth,
		restoreMarkers: hassRestoreMarkers,
		detect:         hassDetect,
		sampled:        hassSampled,
		corrupt:        hassCorrupt,
	},
	mosquitto.Name: {
		volumes: mosquitto.Volumes,
		prepare: func(ctx context.Context, x Executor, m manifest.Manifest, _ string) error {
			return mosquitto.Prepare(ctx, x, m)
		},
		unfronted: true,
		protocol:  mosquitto.Protocol,
		announce:  []routes.Announcement{{Type: mosquitto.ServiceType, Port: mosquitto.MQTTPort}},
		quiesce:   mosquittoQuiesce,
	},
	files.Name: {
		volumes: files.Volumes,
		prepare: func(ctx context.Context, x Executor, m manifest.Manifest, _ string) error {
			return files.Prepare(ctx, x, m)
		},
		appDir: true,
		user:   "0",
		gate:   files.KeyPath,
	},
	mealie.Name: {
		volumes: mealie.Volumes,
		prepare: func(ctx context.Context, x Executor, m manifest.Manifest, _ string) error {
			return mealie.Prepare(ctx, x, m)
		},
		gate:       mealie.KeyPath,
		gateHeader: mealie.SecretHeader,
	},
	navidrome.Name: {
		// The address the reverse proxy reaches the pod network from is the one Navidrome trusts
		// to name its user. Converge knows the pool; nothing in an app package does.
		prepare: func(ctx context.Context, x Executor, m manifest.Manifest, podGateway string) error {
			return navidrome.Prepare(ctx, x, m, podGateway)
		},
		appDir:          true,
		gateByAddress:   true,
		open:            navidrome.Open,
		householdWrites: []string{navidrome.Music},
	},
	freshrss.Name: {
		volumes: freshrss.Volumes,
		// The same address, for the same reason: FreshRSS trusts it to name its user.
		prepare: func(ctx context.Context, x Executor, m manifest.Manifest, podGateway string) error {
			return freshrss.Prepare(ctx, x, m, podGateway)
		},
		gateByAddress: true,
		open:          freshrss.Open,
	},
}

// WantsInbound reports whether an app's container gets the inbound channel. Opt-in, per app, and
// narrow on purpose: the socket is a channel from a workload to the agent, so every container
// that gets one widens what a compromised app can reach. Home Assistant has it because it
// restarts itself far more often than its container does and nothing outside can see those
// restarts. An app with no such boundary needs none, and the generic container-start hook
// already covers it.
func WantsInbound(m manifest.Manifest, c manifest.Container) bool {
	return known[m.Name].inbound && c.Primary
}

// Volumes returns the host binds one container of one app needs beyond its own data. Called by
// the renderer (agent/quadlet), which stays a pure function of the manifest: the same manifest
// renders the same units on every node, and these binds are a property of the app's name rather
// than of anything a publisher wrote.
func Volumes(m manifest.Manifest, c manifest.Container) []string {
	var out []string
	if e := known[m.Name]; e.volumes != nil {
		out = e.volumes(m, c)
	}
	// The app's own directory, read-only, and it is the product's general shape rather than
	// one app's arrangement -- which is why it lives here and not in agent/hass. Everything
	// briard hands an app goes in it: Home Assistant's control token, its planted integration,
	// its s6 wrapper, Files' config, and the inbound token for any app that has one. An app
	// needing none of that gets no directory and no mount.
	if NeedsAppDir(m, c) {
		out = append(out, AppDir(m.Name)+":"+AppMount+":ro")
	}
	if WantsInbound(m, c) {
		// ⚠️ The socket bind is read-write, and it has to be: connect(2) needs write permission
		// on the socket file, so a read-only bind makes the channel unreachable from inside the
		// container rather than merely read-only. Its token needs no bind at all (it is inside
		// the directory above). The file, never its directory: a directory mounted rw would let
		// the container unlink or replace the socket, and this socket is shared by every
		// participating app, so that would be one workload taking the channel from the others.
		out = append(out, InboundSocket()+":"+InboundMount+":rw")
	}
	return out
}

// Quiesce asks an app to hold still so a member taken while it runs is application-consistent,
// and returns the release that ends the window. The default is nothing, as everywhere here: an
// app with no way to hold still returns a nil release and an error saying so, the caller takes
// its member anyway, and the member says crash-consistent.
//
// mosquitto cannot hold still but can flush: its durability is an autosave interval, and SIGUSR1
// writes the persistence database now. So its member is still labelled crash-consistent, which it
// is, and misses at most what arrived in the few seconds after the flush rather than up to an
// autosave interval of it. Low risk, so nothing more: mosquitto has barely any state, and a lost
// message is tolerated.
//
// ⚠️ The release reports whether the lock held for the whole window, which is the caller's input
// for the member's class rather than a courtesy: Home Assistant breaks its own lock if its
// buffered backlog grows while held, so a release that answers false means the app resumed
// writing under the snapshot. It takes the port because that is how an app is reached on the
// guest's loopback, and the primary container because that is what a signal is sent to; only
// the manifest knows either.
func Quiesce(ctx context.Context, x Executor, m manifest.Manifest, port int, container string) (release func(context.Context) (bool, error), err error) {
	if e := known[m.Name]; e.quiesce != nil {
		return e.quiesce(ctx, x, port, container)
	}
	return nil, fmt.Errorf("apps: %s has no way to hold still", m.Name)
}

// hassQuiesce holds Home Assistant's writes, and its release lets them go.
func hassQuiesce(ctx context.Context, x Executor, port int, _ string) (func(context.Context) (bool, error), error) {
	if err := hass.Hold(ctx, x, port); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (bool, error) { return hass.Release(ctx, x, port) }, nil
}

// mosquittoQuiesce flushes the broker, which is all it can do.
func mosquittoQuiesce(ctx context.Context, x Executor, _ int, container string) (func(context.Context) (bool, error), error) {
	if err := mosquitto.Flush(ctx, x, container); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("apps: %s flushed its persistence database; it has no way to hold still", mosquitto.Name)
}

// Health is app health: whether the app works. It is one notion, asked by the status report, the
// app-update gate and the history alike; node health (api.NodeStatus.Healthy) and the S1
// readiness gate are different questions with their own names.
//
// Four values, each one plain fact: healthy; unhealthy; starting, which means still booting and
// nothing else; and unknown, which means the app has told us nothing (our login to it failing, or
// it stopping). Starting is not a kind of unknown: an app still starting once HealthGate has
// passed is not working (Settled). Healthy, unhealthy and starting are the status report's own
// spellings (api.StateHealthy, api.StateUnhealthy, api.StateStarting), so the report carries them
// without translating; unknown is the empty string, which the report already reads as "not asked
// or could not tell".
type Health string

const (
	Healthy       Health = api.StateHealthy
	Unhealthy     Health = api.StateUnhealthy
	Starting      Health = api.StateStarting
	HealthUnknown Health = ""
)

// HealthGate bounds how long an app gets to become healthy after a start: the app-update gate's
// deadline, and how long a start sample's evaluation waits for the boot's verdict.
const HealthGate = 5 * time.Minute

// Settled is the verdict once HealthGate has passed since the start: an app still starting is
// unhealthy, and every other answer stands. The gate reverts on unhealthy and never on unknown,
// and the history records the same verdict the gate acts on.
func (h Health) Settled() Health {
	if h == Starting {
		return Unhealthy
	}
	return h
}

// HealthOverride is a curated app's own health check, or ok=false for an app that has none, whose
// health is then its manifest's healthPath answering 200. Home Assistant's reads /api/config with
// our token, because recovery mode keeps its healthPath answering while the app is down (measured
// by probe; agent/hass/health.go says what each answer means).
func HealthOverride(ctx context.Context, x Executor, m manifest.Manifest, port int) (h Health, ok bool) {
	e := known[m.Name]
	if e.health == nil {
		return HealthUnknown, false
	}
	return e.health(ctx, x, port), true
}

// hassHealth is Home Assistant's own answer, in this package's words.
func hassHealth(ctx context.Context, x Executor, port int) Health {
	switch hass.Health(ctx, x, port) {
	case hass.VerdictHealthy:
		return Healthy
	case hass.VerdictStarting:
		return Starting
	case hass.VerdictUnhealthy:
		return Unhealthy
	}
	return HealthUnknown
}

// RestoreMarkers names the files an app writes inside its own data to say "a restore of my
// backups is in flight": paths relative to that app's data root, one per container that keeps
// state. The default, as always here, is nothing.
//
// Two callers, one fact, which is why it is one function: the guest's inbound take reads them to
// recognise a household's own restore (its hass-restore-before sample), and the restore path
// sweeps them out of a member as it is materialised, so that putting such a member back does not
// replay the restore it was taken around. Relative, never absolute: the caller owns the root. The
// guest resolves them against the live subvolume and the restore against a staged copy of it, and
// a verb carrying absolute paths would be a verb that names a path, which is the thing the
// inbound channel's trust rules forbid.
func RestoreMarkers(m manifest.Manifest) []string {
	if e := known[m.Name]; e.restoreMarkers != nil {
		return e.restoreMarkers(m)
	}
	return nil
}

func hassRestoreMarkers(m manifest.Manifest) []string {
	if _, dir, ok := hass.ConfigDir(m); ok {
		return []string{dir + "/" + hass.RestoreMarker}
	}
	return nil
}

// Detect says what a household changed in an app between two ring members, as the one line its
// history shows, or "". prev and next are member paths, which hold the data area's layout: one
// directory per data mount. running says next was taken while the app ran.
//
// Optional per app, and the default is nothing, as everywhere here: an app with no detector still
// has quiet time, which is what catches what no detector sees. Only Home Assistant has one. Two
// answers, because the history tells them apart: what the household changed, as its line or "",
// and what the app reset on its own (Home Assistant setting an undecodable store aside), as the
// renames' paths in Corrupt's form. The caller phrases those (ResetPhrase), because a start's
// boot adds renames of its own that no member holds.
func Detect(ctx context.Context, x Executor, m manifest.Manifest, prev, next string, running bool) (changed string, resets []string, err error) {
	if e := known[m.Name]; e.detect != nil {
		return e.detect(ctx, x, m, prev, next, running)
	}
	return "", nil, nil
}

func hassDetect(ctx context.Context, x Executor, m manifest.Manifest, prev, next string, running bool) (changed string, resets []string, err error) {
	_, dir, ok := hass.ConfigDir(m)
	if !ok {
		return "", nil, nil
	}
	before, err := hass.Signals(ctx, x, prev+"/"+dir)
	if err != nil {
		return "", nil, err
	}
	after, err := hass.Signals(ctx, x, next+"/"+dir)
	if err != nil {
		return "", nil, err
	}
	for _, r := range hass.Resets(before, after) {
		resets = append(resets, dir+"/"+r)
	}
	return hass.Sentence(hass.Detect(before, after, running)), resets, nil
}

// RingMember is one ring member as an app's sampling sees it. The ring's own type lives in
// agent/quadlet, which imports this package.
type RingMember struct {
	Path     string    // the member's subvolume
	App      string    // its app sidecar (quadlet.AppSidecar), which may not exist
	At       time.Time // when it was taken
	Quiesced bool
}

// Sampled is what an app does with a ring member as it is taken: after the member's sidecar is
// written and before the prune, so a hold it places counts in that prune. ring is the app's
// members, the new one among them. The default is nothing; Home Assistant retains a quiesced
// sample every few hours as a restore candidate for its recorder database (agent/hass/dbcheck.go).
func Sampled(ctx context.Context, x Executor, m manifest.Manifest, ring []RingMember, member RingMember) error {
	if e := known[m.Name]; e.sampled != nil {
		return e.sampled(ctx, x, m, ring, member)
	}
	return nil
}

func hassSampled(ctx context.Context, x Executor, m manifest.Manifest, ring []RingMember, member RingMember) error {
	_, dir, ok := hass.ConfigDir(m)
	if !ok {
		return nil
	}
	conv := func(r RingMember) hass.Member {
		return hass.Member{Path: r.Path, Dir: r.Path + "/" + dir, App: r.App, At: r.At, Quiesced: r.Quiesced}
	}
	var all []hass.Member
	for _, r := range ring {
		all = append(all, conv(r))
	}
	return hass.Sampled(ctx, x, all, conv(member))
}

// Corrupt names what the app has set aside as undecodable under root -- a member, or the live
// data root -- as paths relative to it, each under its mount's directory.
func Corrupt(ctx context.Context, x Executor, m manifest.Manifest, root string) []string {
	if e := known[m.Name]; e.corrupt != nil {
		return e.corrupt(ctx, x, m, root)
	}
	return nil
}

func hassCorrupt(ctx context.Context, x Executor, m manifest.Manifest, root string) []string {
	_, dir, ok := hass.ConfigDir(m)
	if !ok {
		return nil
	}
	var out []string
	for _, r := range hass.Corrupt(ctx, x, root+"/"+dir) {
		out = append(out, dir+"/"+r)
	}
	return out
}

// ResetPhrase is the history's one phrase for a set of Corrupt-form renames, or "".
func ResetPhrase(resets []string) string {
	var rels []string
	for _, r := range resets {
		_, rel, _ := strings.Cut(r, "/") // the mount's directory, which a household never sees
		rels = append(rels, rel)
	}
	return hass.ResetPhrase(rels)
}

// Prepare materialises whatever Volumes promised, on this node, before the container starts.
// Called by converge, which contains a failure here to the one app: the rendered unit names bind
// sources this writes, and podman creates a missing source as a root-owned directory, so an app
// that cannot be prepared must not be started at all. podGateway is the address the reverse proxy
// reaches the pod network from, "" on a node with none.
func Prepare(ctx context.Context, x Executor, m manifest.Manifest, podGateway string) error {
	// The inbound token, for any app that gets the channel. Minted here because Prepare is the
	// one step that runs on every converge (the installing primary, the survivor promoting into
	// an app it was never told about, and every guest reboot), exactly the set of moments the
	// mount it lands in is (re)made. It rotates per converge, where the HA control token is kept
	// for the boot: both sides read the file at every call (agent/hass/notify.py, the agent's
	// resolveCaller), so a fresh value needs no restart, no return channel and nothing to
	// refresh. /run is tmpfs, so a value cannot outlive the boot that minted it, and a token read
	// out of a backup or a snapshot is worth nothing.
	if WantsInboundAny(m) {
		if err := mintInboundToken(ctx, x, m.Name); err != nil {
			return fmt.Errorf("apps: mint %s's inbound token: %w", m.Name, err)
		}
	}
	// The user's name, for an app whose env names it. Here for the token's reason: every converge
	// reads it from the record again, so the file never outlives what it was read from. Written
	// whole or not at all, since a file without the line would start the app with an empty name;
	// with no record the app is not started, which is what an app with no name to take needs.
	if m.NamesUser() {
		user, err := Username(x)
		if err != nil {
			return fmt.Errorf("apps: %s takes the user's name: %w", m.Name, err)
		}
		if _, err := x.Run(ctx, "mkdir", "-p", RunDir()); err != nil {
			return fmt.Errorf("apps: %s: %w", RunDir(), err)
		}
		if err := appdir.Write(ctx, x, UserEnv(), []byte(userEnvName+"="+user+"\n"), "", "0644"); err != nil {
			return fmt.Errorf("apps: the user's name for %s: %w", m.Name, err)
		}
	}
	if e := known[m.Name]; e.prepare != nil {
		return e.prepare(ctx, x, m, podGateway)
	}
	return nil
}

// User is the user a container runs as, "" for its image's own. Files runs as root because what
// it shows is root's: containers here run without a user namespace and every app's area is
// created root-owned, so the image's own uid could read Home Assistant's configuration and write
// none of it -- and a file Files saves keeps the owner its app gave it only when Files writes as
// that owner.
func User(m manifest.Manifest, c manifest.Container) string {
	if c.Primary {
		return known[m.Name].user
	}
	return ""
}

// Capabilities are the Linux capabilities a container gets beyond podman's default set, "" for
// none -- the value of quadlet's AddCapability=. From the product, never the manifest: a catalog
// entry that could name a capability could name all of them.
//
// Home Assistant gets NET_RAW and NET_ADMIN, both of which podman's default set leaves out.
// Without NET_RAW the integrations that read the network below TCP degrade without failing: DHCP
// discovery cannot open its packet socket, so a new device is never offered as discovered, and the
// ping and nmap/ARP presence trackers lose their raw sockets. NET_ADMIN is for the Bluetooth
// integration, which loads by default and checks for both at every start: the guest has no
// Bluetooth to manage, but without them it logs an ERROR each time, which reads to a household as
// something broken. Home Assistant OS runs Core fully privileged, so this is far short of what it
// is tested under.
func Capabilities(m manifest.Manifest, c manifest.Container) string {
	if c.Primary {
		return known[m.Name].capabilities
	}
	return ""
}

// Gate is where the secret lives that the reverse proxy signs the user into this app with, "" for
// an app the reverse proxy forwards to unsigned. Gated means the reverse proxy lets a browser
// through only with the household dashboard's session, and then tells the app who it is in a way
// the app checks against the secret. Files and Mealie are gated: Files holds every app's data,
// and Mealie has no sign-in of its own briard would not have to tell the user a password for.
// Navidrome and FreshRSS are gated with no secret (GateByAddress). Home Assistant keeps its own login because its
// companion app has no browser session to bring.
func Gate(m manifest.Manifest) string { return known[m.Name].gate }

// GateHeader is the header a gated app takes the secret itself in, beside the username, for an app
// that reads its user from a reverse proxy's header; "" is Files' signed token.
func GateHeader(m manifest.Manifest) string { return known[m.Name].gateHeader }

// GateByAddress reports whether an app is gated with no secret at all: it trusts the address the
// reverse proxy reaches the pod network from, and the reverse proxy names the user alone.
// Navidrome and FreshRSS are, having no way to check a secret; what else shares that address can
// sign in to them too, which is accepted for a music server and a feed reader (agent/navidrome,
// agent/freshrss).
func GateByAddress(m manifest.Manifest) bool { return known[m.Name].gateByAddress }

// Open are a gated app's paths the reverse proxy forwards without the dashboard's session: an API
// a client app calls with the app's own credential: Navidrome's Subsonic API, FreshRSS's Google
// Reader API.
func Open(m manifest.Manifest) []string { return known[m.Name].open }

// HouseholdWrites reports whether a files-tier folder of an app is the household's to write, in
// Files' view and the drive, where every other files-tier folder is read-only because its app
// writes it. Navidrome's library is: the household fills it, and Navidrome only reads it.
func HouseholdWrites(m manifest.Manifest, dir string) bool {
	return slices.Contains(known[m.Name].householdWrites, dir)
}

// Reach is the sentence `briard app install` ends on: where the household now finds the
// thing it just installed.
//
// Lead with the name, the doctrine the installer's hand-over prints under: the name stays true if
// the address moves, and under DHCP the VIP is acquired in-guest, so a plausible-but-wrong address
// is the failure this function exists to end. No published name (a witness, or FLOCK_NAME unset)
// means no URL to promise: say the port and stop. The name is the app's own, on the reverse
// proxy's :80, rather than the flock name and the app's port: the bare alias `briard-<app>.local`
// is printed, and only when the flock-scoped `briard-<flock>-<app>.local` exists. That is the
// whole user-facing payoff of the routing table.
//
// The default assumes the manifest's port is the app's HTTP entry point. mosquitto is where it
// is not: what it publishes for the household is MQTT on 1883, while the manifest's port is the
// management endpoint the liveness floor probes, bound inside its pod. Telling a user to open that
// in a browser would hand them a dead link for an app that is working perfectly -- and no
// reverse proxy can front it either, which is why its name resolves but does not serve.
func Reach(m manifest.Manifest, flock string) string {
	// The bare alias (routes.BareHostName) is what is printed: the short name a household types.
	// The flock-scoped name gates it, so an unnamed flock still promises no name at all.
	host := ""
	if routes.HostName(flock, m.Name) != "" {
		host = routes.BareHostName(m.Name)
	}
	// An app the reverse proxy does not front is reached at a published port, and saying which
	// one is the whole of its address (mosquitto's MQTT, not the manifest's management port).
	if !Fronted(m) {
		port := 0
		if len(m.Ports) > 0 {
			port = m.Ports[0]
		}
		if port == 0 {
			// Nothing published and nothing fronted: there is no address to promise, and inventing
			// one is the plausible-but-wrong answer this function exists to end.
			return "it publishes no address of its own yet"
		}
		// Name the protocol where we know it. A port on its own is an address a user cannot act on:
		// nothing in the manifest says what to speak to it, because the schema deliberately carries
		// ports and never protocols. The qualifier is therefore product knowledge, keyed on the
		// app name like everything else here, and it degrades to the bare sentence for an
		// app we have nothing to say about rather than guessing from the port number.
		qual := ""
		if p := protocol(m); p != "" {
			qual = p + " "
		}
		if host == "" {
			return fmt.Sprintf("it accepts %sconnections on port %d", qual, port)
		}
		return fmt.Sprintf("point %sclients at %s:%d", qual, host, port)
	}
	if host == "" {
		return fmt.Sprintf("it answers on port %d", m.Primary().Port)
	}
	return fmt.Sprintf("reach it at http://%s/", host)
}

// protocol is what a client speaks to an app's published port, or "" when we have nothing to
// say. Keyed on the app name, in the same place and for the same reason as Prepare's switch:
// this is the one file that may hold knowledge of both apps, and a protocol is a fact about an
// app rather than about the manifest that describes it.
func protocol(m manifest.Manifest) string { return known[m.Name].protocol }

// Fronted reports whether the reverse proxy may serve this app under its own name. It decides
// whether converge emits a route for the app, not a flag the reverse proxy then has to honour: an
// app this returns false for is named and probed, and the reverse proxy is handed no way to reach
// it at all. Absence rather than a checked condition, for the same reason shared/manifest grants
// capability by omission.
//
// True by default, because a catalogued app's primary port is normally the HTTP entry point a
// household is meant to open (Home Assistant's :8123 is the whole example). False for mosquitto,
// as a security property rather than a nicety: its manifest `port` is the broker's management
// API, which `mosquitto.conf` binds inside the container on purpose, with the reason written at
// the listener: "it is still not exposed, because nothing outside this guest has any business
// reading it". The reverse proxy runs inside that guest, so routing to it by name would republish
// that endpoint on the LAN, undoing a deliberate bind through a mechanism that never mentions it.
// What a household actually reaches on the broker is MQTT on 1883, which no reverse proxy can
// serve anyway.
//
// So this is not the same question as health: a not-fronted app still has an address and is
// still probed there every cycle (mosquitto's management endpoint is exactly what the liveness
// floor GETs, and the guest can reach it because the guest is where the bind is). This decides
// what the reverse proxy may serve, never what the node may ask. The keying is the catalog name,
// product-side, code we ship and review, as for everything else in this package: a manifest can
// declare its networking and its published ports but never this, deliberately, because a
// publisher must not be able to hand itself the reverse proxy.
func Fronted(m manifest.Manifest) bool { return !known[m.Name].unfronted }

// Announce is what this app wants said about itself on the household LAN: the mDNS
// service records converge writes into the routing table for the reverse proxy's mDNS responder
// to answer.
//
// The default is silence, like everything else here. An app that answers HTTP is already
// reachable by the name the reverse proxy serves it under, and browsing types it into a category
// it may not belong in; the announcement exists for the apps a household's other devices go
// looking for, which today is one: mosquitto, and not as a nicety. A broker is the rare app whose
// clients are appliances rather than people: Tasmota and ESPHome firmware browse `_mqtt._tcp` to
// find a broker, the image cannot advertise itself (agent/mosquitto), and a broker on a LAN
// announcing nothing is simply an app being impolite. The port is MQTT's, never the manifest's
// (the manifest names the management endpoint the health floor probes, which is bound inside the
// pod and is nobody else's business). The instance label is the flock-scoped name, composed once
// in shared/routes, so what a device offers a household to pick from names the flock it belongs
// to.
func Announce(m manifest.Manifest, flock string) []routes.Announcement {
	name := routes.InstanceName(flock, m.Name)
	if name == "" {
		// No flock name yet means no name to point an SRV record at, and announcing a broker at a
		// host nothing resolves is worse than announcing nothing. The same rule already governs
		// the app's own A record.
		return nil
	}
	var out []routes.Announcement
	for _, a := range known[m.Name].announce {
		a.Name = name
		out = append(out, a)
	}
	return out
}

// WantsInboundAny reports whether any container of an app wants the inbound channel -- what
// Prepare asks before minting the app's inbound token, which is per app rather than per container.
func WantsInboundAny(m manifest.Manifest) bool {
	for _, c := range m.Containers {
		if WantsInbound(m, c) {
			return true
		}
	}
	return false
}

// mintInboundToken writes a fresh secret into the app's own directory, 0600. 32 bytes of
// crypto/rand: this is a bearer credential for a channel that acts on a household's data, so it
// is sized like one rather than like an identifier. The directory is created here because Prepare
// is the earliest thing on every path that needs it, and because that same directory is what the
// container sees at AppMount, so the token reaches it with no mount of its own.
func mintInboundToken(ctx context.Context, x Executor, app string) error {
	if _, err := x.Run(ctx, "mkdir", "-p", AppDir(app)); err != nil {
		return err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	if err := x.WriteFile(InboundTokenPath(app), []byte(hex.EncodeToString(buf))); err != nil {
		return err
	}
	// The token is a credential, and /run is world-readable by default. The bind mount is what
	// carries it into one container; the mode is what keeps it from every other reader on the
	// node.
	_, err := x.Run(ctx, "chmod", "0600", InboundTokenPath(app))
	return err
}

// NeedsAppDir reports whether a container gets its app's own directory at AppMount.
// True for anything the product hands something to: Home Assistant's control channel, Files' and
// Navidrome's config, or the inbound token. An app needing none of these gets no directory and no
// mount.
func NeedsAppDir(m manifest.Manifest, c manifest.Container) bool {
	return (known[m.Name].appDir && c.Primary) || WantsInbound(m, c)
}
