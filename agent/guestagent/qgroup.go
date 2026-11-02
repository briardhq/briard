package guestagent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// THE QUOTA TREE: EVERY SUBVOLUME IS BORN INTO IT.
//
// The volume is formatted with simple quotas (nodestorage.go), and its subvolumes sit in a
// three-level tree:
//
//	level 2  tier aggregates   2/(1<<40) service      2/(2<<40) user
//	level 1  per app           1/(tier<<40 | app40)   tier 1 = service, 2 = user, 0 reserved
//	level 0  subvolumes        the app's live area, its ring members, its restores
//
// A limit on a level-0 group would RESET on every restore-by-swap: the new subvolume allocates
// fresh, while the deleted original keeps its charge as a space holder. The level-1 group keeps
// counting across the swap, which is why the limit lives there or above, and why every creation
// passes `-i` -- a subvolume assigned after the fact is one a crash can leave outside the tree.
//
// app40 is the first 40 bits of SHA-256 of the app's name: deterministic, so nothing is stored
// or allocated and two nodes agree without asking each other. A prefix of the name's own bytes
// collides at once (jellyfin, jellyseerr).
//
// The tree is read, never assumed: `qgroup create` and `qgroup assign` both fail on an id that
// already exists, and a container's start (a separate process) may race an install for the
// same group. So the missing parts are made, and a re-read decides whether the tree is right.

const (
	tierService = 1
	tierUser    = 2
)

// userRoot is where every app's user area lives: a plain directory holding one subvolume per
// app, beside the service areas rather than inside them, so a service rollback never reaches it.
func userRoot() string { return filepath.Join(dataMountRoot, "user") }

// userArea is one app's user area.
func userArea(app string) string { return filepath.Join(userRoot(), app) }

func appQgroup(tier uint64, app string) string {
	sum := sha256.Sum256([]byte(app))
	return "1/" + strconv.FormatUint(tier<<40|binary.BigEndian.Uint64(sum[:8])>>24, 10)
}

func tierQgroup(tier uint64) string { return "2/" + strconv.FormatUint(tier<<40, 10) }

// areaOf reads the tier and the app off a live area's path. The path is what decides it: a
// service area is a child of the volume's root, a user area a child of userRoot.
func areaOf(area string) (tier uint64, app string, err error) {
	app = filepath.Base(area)
	switch filepath.Dir(filepath.Clean(area)) {
	case dataMountRoot:
		if app == "user" {
			return 0, "", fmt.Errorf("%s is the user tier's root, not an app's area", area)
		}
		return tierService, app, nil
	case userRoot():
		return tierUser, app, nil
	}
	return 0, "", fmt.Errorf("%s is not an app's area on the volume", area)
}

// qgroup is one row of the tree: what it holds, its limit, and its parents.
type qgroup struct {
	referenced int64
	limit      int64 // -1 when it has none
	parents    []string
	row        string // `<id> <referenced> <exclusive> <parents>`, what the soak reads
}

// qgroupTree is `btrfs qgroup show -pr --raw`, keyed by id.
type qgroupTree map[string]qgroup

// qgroupShow is the one read of the tree: parents (-p) and limits (-r), in bytes.
var qgroupShow = []string{"qgroup", "show", "-pr", "--raw", dataMountRoot}

func readQgroups(ctx context.Context, x Executor) (qgroupTree, error) {
	out, err := x.Run(ctx, "btrfs", qgroupShow...)
	if err != nil {
		return nil, fmt.Errorf("btrfs qgroup show: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseQgroups(out), nil
}

// parseQgroups reads `Qgroupid Referenced Exclusive Max-referenced Parent Path`. The header and
// its rule are dropped (they start with a letter and a dash), and so is the Path column: an
// aggregate's cell carries a non-UTF-8 byte, and nothing reads it.
func parseQgroups(out []byte) qgroupTree {
	tree := qgroupTree{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0][0] < '0' || f[0][0] > '9' {
			continue
		}
		g := qgroup{limit: -1, row: strings.Join([]string{f[0], f[1], f[2], f[4]}, " ")}
		g.referenced, _ = strconv.ParseInt(f[1], 10, 64)
		if f[3] != "none" {
			g.limit, _ = strconv.ParseInt(f[3], 10, 64)
		}
		if f[4] != "-" {
			g.parents = strings.Split(f[4], ",")
		}
		tree[f[0]] = g
	}
	return tree
}

// rows is the tree as the soak reads it, one `<id> <referenced> <exclusive> <parents>` per group.
func (t qgroupTree) rows() []string {
	var rows []string
	for _, g := range t {
		rows = append(rows, g.row)
	}
	slices.Sort(rows)
	return rows
}

func (t qgroupTree) holds(group, parent string) bool {
	_, agg := t[parent]
	return agg && slices.Contains(t[group].parents, parent)
}

// areaGroup is the level-1 group a subvolume of this area is born into, made -- with its tier
// aggregate -- if it is not there yet.
func areaGroup(ctx context.Context, x Executor, area string) (string, error) {
	tier, app, err := areaOf(area)
	if err != nil {
		return "", err
	}
	group, agg := appQgroup(tier, app), tierQgroup(tier)
	tree, err := readQgroups(ctx, x)
	if err != nil {
		return "", err
	}
	if tree.holds(group, agg) {
		return group, nil
	}
	var errs []error
	try := func(args ...string) {
		if out, err := x.Run(ctx, "btrfs", append([]string{"qgroup"}, args...)...); err != nil {
			errs = append(errs, fmt.Errorf("btrfs qgroup %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out))))
		}
	}
	if _, ok := tree[agg]; !ok {
		try("create", agg, dataMountRoot)
	}
	if _, ok := tree[group]; !ok {
		try("create", group, dataMountRoot)
	}
	try("assign", group, agg, dataMountRoot)
	if tree, err = readQgroups(ctx, x); err != nil {
		return "", err
	}
	if tree.holds(group, agg) {
		return group, nil // whatever failed above, a racer had already done it
	}
	why := errors.Join(errs...)
	if why == nil {
		why = errors.New("every step reported success")
	}
	return "", fmt.Errorf("%s is not in %s under %s after making it: %w", area, group, agg, why)
}
