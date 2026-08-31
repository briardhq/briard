// Command quadlet-render is a TEST HELPER (nixosTest/service-install.nix), not a product binary.
//
// It exists so the integration test drives the REAL renderer rather than a hand-written copy of
// its output — the mistake the quadlet spike deliberately accepted ("here they stand in for that
// output, so the spike tests the mechanism and not our renderer") and which this test exists to
// close. Everything downstream of it — podman, quadlet, systemd, drbd-reactor — is real.
//
// It stands in for the host agent's orchestration, which cannot run here: the install path drives
// the guest over the virtio-serial channel, and in the hermetic harness the node IS the guest with
// no host on the other end. That orchestration is unit-tested (agent/host/service_test.go); what
// this test adds is that the renderer's output is valid quadlet a real promoter can drive.
//
//	quadlet-render <manifest.json> <outdir>
//
// writes the rendered unit files into outdir, plus the sidecars a test reads: `units` (the
// service units in start order), `images` (the .image warm units), `identity`, `dataroot` and
// `subdirs` (what the install path's provision step creates).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"briard.io/agent/quadlet"
	"briard.io/shared/manifest"
)

func main() {
	if len(os.Args) != 3 {
		fatal("usage: quadlet-render <manifest.json> <outdir>")
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fatal("read manifest: %v", err)
	}
	// Parse, not just unmarshal: the test must exercise the validating path, so a manifest that
	// would be refused in production is refused here too.
	m, id, err := manifest.Parse(raw)
	if err != nil {
		fatal("parse manifest: %v", err)
	}
	// The address a node would allocate. The harness is not the allocator, so it passes the
	// loopback a host-networked service gets anyway and a fixed pod address otherwise -- enough to
	// render, and never mistaken for what a real node would choose.
	addr := "127.0.0.1"
	if !m.HostNetwork() {
		addr = "10.12.0.2"
	}
	r, err := quadlet.Render(m, addr)
	if err != nil {
		fatal("render: %v", err)
	}
	out := os.Args[2]
	if err := os.MkdirAll(out, 0o755); err != nil {
		fatal("mkdir: %v", err)
	}
	for name, body := range r.Files {
		if err := os.WriteFile(filepath.Join(out, name), []byte(body), 0o644); err != nil {
			fatal("write %s: %v", name, err)
		}
	}
	write := func(name string, lines []string) {
		if err := os.WriteFile(filepath.Join(out, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			fatal("write %s: %v", name, err)
		}
	}
	// The service units, in start order. NOT a promoter chain: since [V3b.3](f) the chain is
	// static (data -> services -> vip) and these are not members of it -- briard-services starts
	// them, which is what keeps a crashed container from demoting the node.
	write("units", r.Units)
	write("images", r.ImageUnits)
	write("identity", []string{string(id)})
	write("dataroot", []string{quadlet.DataRoot(m.Name)})
	// The per-container subdirectories the install path's provision step creates inside that
	// subvolume. A harness that has to stand in for `service.provision` needs the same list, and
	// deriving it a second time in a test is how the two drift.
	write("subdirs", quadlet.Subdirs(m))
	fmt.Printf("rendered %s (%s): %d files\n", m.Name, id, len(r.Files))
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "quadlet-render: "+format+"\n", a...)
	os.Exit(1)
}
