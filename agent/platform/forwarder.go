package platform

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ForwarderSpec configures the anchor-side witness-forwarder: the host-held
// mTLS hop that tunnels the guest's DRBD cloud-witness connection to the cloud witness-proxy, so the
// DRBD kernel socket speaks only plaintext over the trusted private guest↔host link and never the
// internet ([[cloud-witness-v3-1d]], [[logic-on-host-by-default]]). Listen is the private address
// the guest's DRBD dials (= the witness peer's mesh Address, e.g. "10.9.9.1:7789"); Target is the
// cloud proxy; Cert/Key/CA are the host-held anchor identity (the cert path).
type ForwarderSpec struct {
	Binary     string // witness-forwarder binary path
	Listen     string // ip:port to listen on (the guest reaches this over the private link)
	Target     string // the cloud witness-proxy address to tunnel to
	Cert       string // PEM client certificate (the anchor's identity)
	Key        string // PEM private key for Cert
	CA         string // PEM CA bundle the witness-proxy server cert must chain to
	ServerName string // expected SAN on the witness-proxy server cert
	Unit       string // transient unit name; empty = ForwarderUnit
}

// ForwarderUnit is the transient systemd service the witness-forwarder runs as. Like the guest
// , it is detached from the agent's cgroup so an agent restart leaves the witness hop
// serving -- dropping it would break the DRBD connection to the cloud witness and cost quorum.
const ForwarderUnit = "briard-witness-forwarder.service"

func (s ForwarderSpec) unit() string {
	if s.Unit != "" {
		return s.Unit
	}
	return ForwarderUnit
}

// StartForwarder brings up the witness-forwarder as a transient systemd service (systemd-run). It is
// idempotent: a forwarder already serving this pairing is left running, so re-delivering a pair
// directive never bounces the live witness hop (announce-before-act re-applies cleanly). Restart=
// always keeps the hop up across a transient proxy blip.
func StartForwarder(ctx context.Context, s ForwarderSpec) error {
	if s.Binary == "" || s.Listen == "" || s.Target == "" || s.Cert == "" || s.Key == "" || s.CA == "" || s.ServerName == "" {
		return fmt.Errorf("platform: witness-forwarder spec incomplete (%+v)", s)
	}
	unit := s.unit()
	if forwarderRunning(ctx, unit) {
		return nil
	}
	args := []string{
		"--unit=" + unit,
		"--collect", // GC a prior dead instance so re-start doesn't hit a lingering failed unit
		"-p", "Restart=always",
		"-p", "Description=Briard witness forwarder (" + unit + ")",
		"--", s.Binary,
		"-addr", s.Listen,
		"-target", s.Target,
		"-cert", s.Cert,
		"-key", s.Key,
		"-ca", s.CA,
		"-servername", s.ServerName,
	}
	if out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("platform: systemd-run witness-forwarder: %w: %s", err, out)
	}
	return nil
}

// forwarderRunning reports whether the forwarder's transient service is currently active -- the
// idempotency probe (any non-"active" state reads as false, so the caller starts fresh).
func forwarderRunning(ctx context.Context, unit string) bool {
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	return strings.TrimSpace(string(out)) == "active"
}
