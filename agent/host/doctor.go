// The agent's half of `briard doctor`: the node judged live, from the agent's own view.
//
// The report card gates the install and nothing re-ran it afterwards, so a node a month later
// had only its history (`briard alerts`, `briard logs`) to answer "what is wrong NOW". The CLI
// reads what the host can see by itself (agent/reportcard, doctor.go); this half answers what
// only the agent can -- the network it built, the guest behind the channel it holds, and what
// the guest says -- through the same snapshot the report loop sends upward, so the doctor and
// the cloud judge one sample, never two readings that could disagree.
//
// Local-only (localOnlyKinds): the answer is for the person at the machine, and must never
// become a second, wider path out of the house than NodeStatus.
package host

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"briard.io/agent/nic"
	"briard.io/agent/reportcard"
	"briard.io/shared/api"
	"briard.io/shared/model"
	"briard.io/shared/routes"
)

// doctorBudget bounds the whole agent half. The snapshot's own reads are 5 s each; this is the
// ceiling on the loop being held, since the directive runs inside it.
const doctorBudget = 20 * time.Second

// certWarnBefore is when an expiring cert becomes a warning: renewal is the cloud's, and two
// weeks is time for a person to notice it did not happen.
const certWarnBefore = 14 * 24 * time.Hour

// certReader reads the serving cert back from the volume (guestagent.Client.ReadCert).
type certReader interface {
	ReadCert(ctx context.Context) (string, error)
}

// storageFreeReader measures the guest's store (guestagent.Client.StorageFree).
type storageFreeReader interface {
	StorageFree(ctx context.Context) (free, total int64, err error)
	SupportsStorageFree() bool
}

// doctorFacts is what judgeDoctor judges -- gathered by applyDoctor, fabricated by the tests.
type doctorFacts struct {
	Parent   string // the device the guest's L2 hangs off; "" when this agent built no network
	Bridge   bool   // Parent is a bridge (the substrate fork)
	Ipvtap   bool   // Parent is a wireless station (the fork's third answer)
	ParentUp bool
	// Refused/RefusedFix are the copier's refusal of the address the router handed the guest
	// (ipvtap.go); "" when there is none.
	Refused, RefusedFix string
	Diskless            bool // a witness: no volume, no address, no service
	Peers               int  // other members of the flock; 0 is a lone node
	DataDisk            string
	DiskFound           bool

	GuestErr  error // the snapshot could not ask the guest
	Cluster   model.Cluster
	Probe     string // the front door the snapshot probed; "" when it had no address to probe
	Healthy   bool
	HeldVIP   string // what the VIP device holds, read on a node that is NOT serving
	FlockName string
	Published string
	Other     string // another briard heard answering the shared briard.local at bring-up; "" when none

	VolAsked        bool
	VolFree, VolTot int64
	VolErr          error

	CertAsked bool
	CertPEM   string
	CertErr   error
	Now       time.Time
}

// applyDoctor gathers doctorFacts and answers with the judged checks as JSON.
func (cfg Config) applyDoctor(ctx context.Context, d api.Directive, r guestReader) api.DirectiveOutcome {
	ctx, cancel := context.WithTimeout(ctx, doctorBudget)
	defer cancel()
	f := doctorFacts{
		Diskless:  cfg.Diskless,
		Peers:     max(len(cfg.Resource.Peers)-1, 0),
		DataDisk:  cfg.DataDisk,
		FlockName: cfg.FlockName,
		Now:       time.Now(),
	}
	if cfg.net != nil {
		f.Parent, f.Bridge, f.Ipvtap = cfg.net.Parent, cfg.net.Bridge, cfg.net.Ipvtap
		f.ParentUp = f.Parent != "" && nic.Up(f.Parent)
	}
	f.Refused, f.RefusedFix = cfg.ipvtap.refusal()
	if f.DataDisk != "" {
		_, err := os.Stat(f.DataDisk)
		f.DiskFound = err == nil
	}
	if r == nil {
		f.GuestErr = errors.New("this agent holds no guest channel")
		return doctorOutcome(d, judgeDoctor(f))
	}
	st, cl, probe, err := cfg.snapshot(ctx, r, "")
	mdnsNames(ctx, r, &st)
	f.GuestErr, f.Cluster, f.Probe, f.Healthy, f.Published, f.Other = err, cl, probe, st.Healthy, st.PublishedName, st.OtherBriard
	if err == nil && !cfg.Diskless {
		if !cl.Serving() && cfg.VIPDev != "" {
			f.HeldVIP, _ = r.VIP(ctx, cfg.VIPDev)
		}
		// The volume is mounted only where the node serves, so only there can it be asked.
		if cl.Serving() {
			if s, ok := r.(storageFreeReader); ok && s.SupportsStorageFree() {
				f.VolAsked = true
				f.VolFree, f.VolTot, f.VolErr = s.StorageFree(ctx)
			}
			if c, ok := r.(certReader); ok {
				f.CertAsked = true
				f.CertPEM, f.CertErr = c.ReadCert(ctx)
			}
		}
	}
	return doctorOutcome(d, judgeDoctor(f))
}

// doctorOutcome carries the checks home as the outcome's Detail.
func doctorOutcome(d api.Directive, cs []reportcard.Check) api.DirectiveOutcome {
	out, err := json.Marshal(cs)
	if err != nil {
		return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeFailed, Detail: err.Error()}
	}
	return api.DirectiveOutcome{ID: d.ID, State: api.OutcomeDone, Detail: string(out)}
}

// judgeDoctor maps the facts to checks. Pure, so every verdict is a unit test. Refuse here reads
// FAIL: something the household can feel is broken now, not a machine turned away.
func judgeDoctor(f doctorFacts) []reportcard.Check {
	var cs []reportcard.Check
	add := func(name string, s reportcard.Status, detail, fix string) {
		cs = append(cs, reportcard.Check{Name: name, Status: s, Detail: detail, Fix: fix})
	}

	if f.Parent != "" {
		kind := "macvtap"
		switch {
		case f.Bridge:
			kind = "bridge"
		case f.Ipvtap:
			kind = "ipvtap: wireless, so this node cannot take a peer until it is wired"
		}
		if f.ParentUp {
			add("network", reportcard.Pass, fmt.Sprintf("%s is up (the guest's network hangs off it, %s)", f.Parent, kind), "")
		} else {
			add("network", reportcard.Refuse, fmt.Sprintf("%s is gone or down, so the guest is unreachable", f.Parent),
				"reconnect it; a replacement on the same network is picked up on its own within 30 minutes, and `briard alerts` says what the agent is waiting for")
		}
	}
	if f.DataDisk != "" {
		if f.DiskFound {
			add("data-disk", reportcard.Pass, f.DataDisk+" is present", "")
		} else {
			add("data-disk", reportcard.Refuse, f.DataDisk+" is missing -- your data volume lives on it", "reconnect the disk, then restart the agent: `systemctl restart briard-agent`")
		}
	}
	if f.GuestErr != nil {
		// Everything below is the guest's answer, so there is nothing more to judge honestly.
		add("guest", reportcard.Refuse, fmt.Sprintf("the guest does not answer on its control channel: %v", f.GuestErr),
			"`briard logs` shows its console; if it does not come back, `briard rescue -yes` rebuilds it (your data volume is untouched)")
		return cs
	}
	add("guest", reportcard.Pass, "the guest answers on its control channel", "")

	cl := f.Cluster
	if f.Diskless {
		if cl.Quorate {
			add("role", reportcard.Pass, "witness, and it sees the flock", "")
		} else {
			add("role", reportcard.Refuse, "witness, but it cannot see the flock", "check the network between this machine and the others")
		}
		return cs
	}

	serving := cl.Serving()
	switch {
	case serving && f.Peers == 0:
		add("role", reportcard.Pass, "serving your home (a lone node, nothing to replicate to)", "")
	case serving:
		add("role", reportcard.Pass, "serving your home (DRBD Primary, quorate)", "")
	case cl.Primary:
		add("role", reportcard.Refuse, "DRBD Primary without quorum: it refuses writes until a peer or the witness returns",
			"bring the other machine back; this is the node protecting your data, not a fault in it")
	case cl.PeerCanTakeOver():
		add("role", reportcard.Pass, "standing by: a peer serves your home and this node can take over", "")
	default:
		add("role", reportcard.Refuse, "nobody holds your home: this node is not serving and no peer can take over",
			"`briard alerts` and `briard logs` say why")
	}

	if f.Peers > 0 {
		switch {
		case cl.Connected == 0:
			add("replication", reportcard.Warn, "no peer connected: your data has one copy until one returns", "check the other machine is on and on the network")
		case cl.Connected < f.Peers:
			add("replication", reportcard.Warn, fmt.Sprintf("%d of %d peers connected", cl.Connected, f.Peers), "check the missing machine is on and on the network")
		case !cl.UpToDate:
			add("replication", reportcard.Warn, "this node's copy is not up to date yet (still syncing)", "wait for the sync; nothing to do unless it stays like this")
		default:
			add("replication", reportcard.Pass, fmt.Sprintf("%d of %d peers connected, this copy is up to date", cl.Connected, f.Peers), "")
		}
	}

	// THE ADDRESS AGAINST THE ROLE: a probe alone cannot tell a handover from a fault,
	// so what is expected depends on whether this node serves.
	switch {
	case f.Refused != "":
		// Ahead of the generic "no address" line, which it would otherwise read as: this is WHY
		// there is none, and the remedy is not the router's free addresses.
		add("address", reportcard.Refuse, f.Refused, f.RefusedFix)
	case serving && f.Probe == "":
		add("address", reportcard.Refuse, "serving, but holding no address, so nothing in your home can reach it",
			"if your router hands out the address, check it has free addresses; `briard logs` shows the attempt")
	case serving && !f.Healthy:
		add("address", reportcard.Refuse, fmt.Sprintf("the front door at %s does not answer", f.Probe), "`briard logs` shows why")
	case serving:
		add("address", reportcard.Pass, fmt.Sprintf("the front door answers at %s", f.Probe), "")
	case f.HeldVIP != "":
		add("address", reportcard.Refuse, fmt.Sprintf("not serving, yet holding the home's address %s", f.HeldVIP),
			"two machines may be answering for one address; `briard logs` on both")
	default:
		add("address", reportcard.Pass, "not serving, and holding no address (the serving node has it)", "")
	}
	if !serving {
		return cs
	}

	// THE NAME THE DOOR REPORTS IS THE ONE IT WAS GIVEN: the responder probes nothing and renames
	// nothing, so Published is either the flock name or empty, and "published as some other name"
	// is not a state it can be in. A second flock answering the bare `briard.local` is a
	// different fact: that name is ambiguous by design (routes.BareHostName), nothing is wrong
	// and nothing yields, so it is a warning that says which name is unambiguously this one.
	if f.FlockName != "" {
		want := routes.FlockHostName(f.FlockName)
		switch {
		case f.Published == "":
			add("name", reportcard.Warn, want+" is not published", "`briard logs` shows the front door's side")
		case f.Other != "":
			add("name", reportcard.Warn, fmt.Sprintf("another briard at %s also answers %s, so that name may open either one", f.Other, routes.BareFlockHostName),
				"this one is always "+want+"; use that name when it matters which")
		default:
			add("name", reportcard.Pass, "published as "+want+" and "+routes.BareFlockHostName, "")
		}
	}

	if f.VolAsked {
		switch {
		case f.VolErr != nil:
			add("volume", reportcard.Warn, fmt.Sprintf("could not measure the data volume: %v", f.VolErr), "")
		case f.VolTot > 0 && f.VolFree*10 < f.VolTot:
			add("volume", reportcard.Warn, fmt.Sprintf("%d MB free of %d MB (under 10%%)", f.VolFree>>20, f.VolTot>>20),
				"remove apps you no longer use; a full volume stops updates and history")
		default:
			add("volume", reportcard.Pass, fmt.Sprintf("%d MB free of %d MB", f.VolFree>>20, f.VolTot>>20), "")
		}
	}

	if f.CertAsked {
		cs = append(cs, certCheck(f.CertPEM, f.CertErr, f.Now))
	}
	return cs
}

// certCheck judges the serving cert's validity window. No cert is a pass: a node without a Briard
// account is never issued one, and its page is served on the local address.
func certCheck(pemText string, err error, now time.Time) reportcard.Check {
	c := reportcard.Check{Name: "cert"}
	if err != nil {
		c.Status, c.Detail = reportcard.Warn, fmt.Sprintf("could not read the certificate: %v", err)
		return c
	}
	if pemText == "" {
		c.Status, c.Detail = reportcard.Pass, "no certificate (none is issued without a Briard account)"
		return c
	}
	block, _ := pem.Decode([]byte(pemText))
	var cert *x509.Certificate
	if block != nil {
		cert, err = x509.ParseCertificate(block.Bytes)
	}
	if block == nil || err != nil {
		c.Status, c.Detail = reportcard.Refuse, "the certificate on the volume does not parse"
		c.Fix = "it is replaced at the next renewal; until then browsers will refuse the page"
		return c
	}
	until := cert.NotAfter.UTC().Format("2006-01-02")
	switch {
	case now.Before(cert.NotBefore):
		c.Status, c.Detail = reportcard.Refuse, "the certificate is not valid yet -- this machine's clock is behind"
		c.Fix = "see the clock check"
	case !now.Before(cert.NotAfter):
		c.Status, c.Detail = reportcard.Refuse, "the certificate expired on "+until
		c.Fix = "renewal did not happen; `briard alerts` may say why"
	case cert.NotAfter.Sub(now) < certWarnBefore:
		c.Status, c.Detail = reportcard.Warn, "the certificate expires on "+until
		c.Fix = "renewal is due; `briard alerts` if it does not happen"
	default:
		c.Status, c.Detail = reportcard.Pass, "the certificate is valid until "+until
	}
	return c
}
