package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"briard.io/agent/guest"
	"briard.io/agent/reportcard"
	"briard.io/shared/api"
	"briard.io/shared/atomicfile"
)

// `briard config set`: CHANGING A SETTING ON A RUNNING NODE.
//
// The installer writes the settings once, into config.env, and the agent reads them once. A
// setting that the household has to change later -- the service address is the first, and the one
// the ipvtap refusal asks them to change -- would otherwise need an uninstall and a reinstall. This
// writes the same key into the same file, so config.env stays the one place a setting lives
// (a reinstall that names it overwrites it, as before), and applies it the one way a bring-up
// fact is applied: restart the guest, so the next bring-up carries it.
//
// Three keys: `vip`, applied by a guest restart, and the backup's two (`backup-dir`,
// `backup-key-saved`, backup.go), applied in place -- the host is the only thing that reads them.
// The command is named for the general act, so a key joins without a second verb.

// configSetBudget bounds the change: the address probe, the write, and a guest restart.
const configSetBudget = 10 * time.Minute

// vipAddr is the service address as it stands now -- see Config.vip.
func (cfg Config) vipAddr() string {
	if cfg.vip != nil {
		return *cfg.vip
	}
	return cfg.VIPAddr
}

// applyConfigSet handles a DirectiveConfigSet.
func (cfg Config) applyConfigSet(ctx context.Context, d api.Directive, r guest.VIPReader, rb guestRebooter, logf func(string, ...any)) api.DirectiveOutcome {
	failed := func(format string, a ...any) api.DirectiveOutcome {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: fmt.Sprintf(format, a...)}
	}
	var s api.ConfigSetting
	if err := json.Unmarshal([]byte(d.Payload), &s); err != nil {
		return failed("bad setting payload: %v", err)
	}
	if s.Key != "vip" {
		return failed("unknown setting %q: the settings that can be changed are vip, backup-dir and backup-key-saved", s.Key)
	}
	// THE ADDRESS IS THE FLOCK'S. Changed on one node of a pair, the next failover would move the
	// service to a different address than the one the household uses; changing it everywhere at
	// once waits for the flock's own settings.
	if len(cfg.Resource.Peers) > 1 {
		return failed("this node has peers, and the service address belongs to all of them: it cannot be changed on one node")
	}
	hostCIDR := ""
	if cfg.net != nil {
		hostCIDR = reportcard.HostCIDR(cfg.net.Parent)
	}
	cctx, cancel := cfg.beat.budget(ctx, configSetBudget)
	defer cancel()
	live := ""
	if cfg.VIPDev != "" {
		cidr, _ := r.VIP(cctx, cfg.VIPDev)
		live, _, _ = strings.Cut(cidr, "/")
	}
	want, err := vipSetting(s.Value, hostCIDR, live)
	if err != nil {
		return failed("%v", err)
	}
	was := cfg.vipAddr()
	if want == was {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: "vip is already " + vipWord(want) + "; nothing changed"}
	}
	path := cfg.configPathForMessage()
	if err := setConfigKey(path, "VIP_ADDR", want, true); err != nil {
		return failed("could not record it in %s: %v", path, err)
	}
	if cfg.vip != nil {
		*cfg.vip = want
	}
	// A refusal was about the address the router handed out, and the household has now named one.
	cfg.ipvtap.clear()
	logf("config: vip %s -> %s, recorded in %s; restarting the guest to apply it", vipWord(was), vipWord(want), path)
	if err := rb.RebootGuest(cctx); err != nil {
		return failed("vip recorded as %s, but the guest restart failed (the next restart applies it): %v", vipWord(want), err)
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone,
		Detail: fmt.Sprintf("vip is now %s (was %s); the guest restarted with it", vipWord(want), vipWord(was))}
}

// vipSetting turns what the household typed into VIP_ADDR's value: "" for dhcp, else an address in
// CIDR form, judged by the install's own gate. A bare address takes the prefix of the host's LAN.
// live is the address the guest holds now, which is the one address allowed to answer.
func vipSetting(value, hostCIDR, live string) (string, error) {
	if value == "dhcp" {
		return "", nil
	}
	var cidr string
	if p, err := netip.ParsePrefix(value); err == nil {
		cidr = p.String()
	} else if a, err := netip.ParseAddr(value); err == nil {
		h, err := netip.ParsePrefix(hostCIDR)
		if err != nil {
			return "", fmt.Errorf("%s has no prefix, and this machine's own could not be read: give one, e.g. %s/24", value, value)
		}
		cidr = netip.PrefixFrom(a, h.Bits()).String()
	} else {
		return "", fmt.Errorf("%q is neither an address nor dhcp", value)
	}
	// The arithmetic first, then the probe -- the probe costs a wait, and only a plausible address
	// is worth it. The address the guest holds now answers by definition, so it is not probed.
	if err := vipRefusal(reportcard.VIPCheck(hostCIDR, cidr, false)); err != nil {
		return "", err
	}
	if addr, _, _ := strings.Cut(cidr, "/"); addr != live && answers(cidr) {
		return "", vipRefusal(reportcard.VIPCheck(hostCIDR, cidr, true))
	}
	return cidr, nil
}

// answers is reportcard.AddressAnswers, a variable so the tests do not probe a network.
var answers = reportcard.AddressAnswers

// vipRefusal is the gate's Refuse, if it gave one, in this command's words.
func vipRefusal(cs []reportcard.Check) error {
	for _, c := range cs {
		if c.Status == reportcard.Refuse {
			return fmt.Errorf("%s: pick a free address on this machine's LAN, outside your router's DHCP range", c.Detail)
		}
	}
	return nil
}

// vipWord names a VIP_ADDR value for a person.
func vipWord(v string) string {
	if v == "" {
		return "dhcp"
	}
	return v
}

// setConfigKey rewrites config.env with key set to value. An empty value removes the line when
// dropEmpty -- absent is that key's default, VIP_ADDR's dhcp -- and writes `key=` otherwise, for a
// key whose empty value is a decision of its own (BACKUP_DIR, off; config.go's `declared`). Every
// other line is kept as it was. tmp + fsync + rename: the file is the setting's only copy.
func setConfigKey(path, key, value string, dropEmpty bool) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var out []string
	for l := range strings.SplitSeq(strings.TrimRight(string(b), "\n"), "\n") {
		if k, _, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == key {
			continue
		}
		if l != "" || len(out) > 0 {
			out = append(out, l)
		}
	}
	if value != "" || !dropEmpty {
		out = append(out, key+"="+value)
	}
	return atomicfile.Write(path, []byte(strings.Join(out, "\n")+"\n"), 0o600, 0o755)
}
