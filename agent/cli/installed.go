package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"briard.io/shared/manifest"
)

// What is on this node, in the household's words.
//
// It lives on `briard open`, the verb the Linux install's closing calls, rather than in the
// installer: it reads the node's own state to say something to a person, the shape of a thing
// that changes -- and the installer is the one file no release can reach. Here it answers the same
// question when a household runs `sudo briard open` a month later.
//
// ⚠️ The manifest's own parser, not a second reader of the format: the cache holds each app's
// manifest verbatim (the bytes the catalog published and the agent verified), so shared/manifest
// reads them, at no cost — agent/cli already links net/http, and the guest binary does not link
// agent/cli at all. A `grep` for `"name"` would be a second implementation of a format whose
// identity is its content hash.

// appCacheDir is where the agent keeps one manifest per installed app. It mirrors
// ConfigFromEnv's APP_CACHE default (agent/host/config.go); a test asserts the two agree.
var appCacheDir = filepath.Join(stateDir, "apps")

// installedApps names what this node runs, sorted, reading the cache the agent rebuilds itself
// from at every start. Best-effort by contract, like everything else in this package that reads
// node state: a cache that cannot be read is a node we cannot say anything about, reported as
// nothing rather than as an error -- the caller is printing a courtesy line, not gating on it. A
// manifest that will not parse still counts as an app, under a vaguer name: it is installed, and
// refusing to mention it because we could not read its title would be the one answer that is
// certainly wrong.
func installedApps(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if m, _, err := manifest.Parse(raw); err == nil && m.Name != "" {
			names = append(names, m.Name)
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names
}

// installedLine is the sentence, or "" when nothing is installed: the line is a courtesy, and
// with no apps `briard open` prints only the link.
func installedLine(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0] + " is installed on this machine"
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] +
			" are installed on this machine"
	}
}
