package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"briard.io/shared/manifest"
)

// WHAT IS ON THIS NODE, in the household's words.
//
// install.sh used to answer this with `grep -o '"name":"[^"]*"' | head -1` over the cache, to end
// the install with "home-assistant is already installed on it and is coming back up" rather than
// the flat lie "no service is installed on it yet" that a reinstall would otherwise print. The
// sentence was right and its home was wrong ([B.157]): it reads the node's own state to say
// something to a person, which is the shape of a thing that changes -- and the installer is the one
// file no release can reach.
//
// So it is here, on the verb the installer already calls at exactly that moment, and it answers the
// SAME question when a household runs `sudo briard dashboard` a month later.
//
// ⚠️ THE MANIFEST'S OWN PARSER, not a second reader of the format. The cache holds each service's
// manifest verbatim -- the bytes the catalog published and the agent verified -- so shared/manifest
// is what should read them, and it costs nothing here: agent/cli already links net/http, and the
// guest binary does not link agent/cli at all. A `grep` for `"name"` would have been a second
// implementation of a format whose identity is its content hash.

// serviceCacheDir is where the agent keeps one manifest per installed service. It mirrors
// ConfigFromEnv's SERVICE_CACHE default (agent/host/config.go); a test asserts the two agree.
const serviceCacheDir = "/var/lib/briard/services"

// installedServices names what this node runs, sorted, reading the cache the agent rebuilds itself
// from at every start.
//
// Best-effort by contract, like everything else in this package that reads node state: a cache that
// cannot be read is a node we cannot say anything about, and that is reported as nothing rather
// than as an error -- the caller is printing a courtesy line, not gating on it.
//
// A manifest that will not parse still counts as a service, under a vaguer name. It IS installed;
// refusing to mention it because we could not read its title would be the one answer that is
// certainly wrong.
func installedServices(dir string) []string {
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

// installedLine is the sentence, or "" when there is nothing to say. Empty rather than "no service
// is installed" because the two callers want different things from that case: the installer says it
// out loud (a fresh node SHOULD hear that it is a node first and a Home Assistant box second), and
// a household running the verb a month later does not need to be told twice.
func installedLine(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0] + " is installed on this node"
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] +
			" are installed on this node"
	}
}
