package guestagent

import (
	"context"
	"log"
	"os"
	"strings"
	"time"
)

// THE VIP ANNOUNCER: a gratuitous ARP for the service address on every link-up of its NIC, and
// on a slow beat for as long as the address is held.
//
// The claim announces once (briard-vip's ExecStartPost) and an address change announces once
// (the DHCP hook). Neither covers a link that drops and comes back with the address unchanged,
// and that is the case that leaves the guest unreachable: a bridge in front of it -- the Windows
// bridge over a radio, measured -- forgets where the address lives when its member's media
// cycles, and the guest, holding the same address, has no reason to say anything. Every peer
// that had it cached is then cut off until its own ARP entry expires, 35-46 s on a radio
// re-association while the host itself was back in 3 s.
//
// THE BEAT IS FOR WHAT THE GUEST CANNOT SEE. A member swap or an upstream switch forgetting its
// table changes nothing on the guest's own link, and the beat bounds the darkness those leave.
// A gratuitous ARP for an address we hold is harmless when nobody needed it, so the cadence errs
// towards more.
//
// It runs as its own unit, PartOf briard-vip and not a promoter chain member: an announcement
// that fails must never be able to demote a serving node.

const (
	// announceBurst is how many announcements a link-up sends, one per tick: the first can
	// land while the port in front of the guest is still coming up.
	announceBurst = 3
	// announceEvery is the beat while the link is up.
	announceEvery = 30 * time.Second
	// announceTick is how often the link is read.
	announceTick = time.Second
)

// announcer decides, one tick at a time, whether to announce now. It holds the link as last
// read, the burst still owed, and when the last announcement went out.
type announcer struct {
	up    bool
	burst int
	last  time.Time
}

// step takes this tick's carrier reading and says whether to announce. A link-up -- including
// the first reading at start, the claim's own moment -- owes a burst; a link that stays up owes
// one every announceEvery; a link that is down owes nothing, since nothing it sends would leave.
func (a *announcer) step(now time.Time, carrier bool) bool {
	if carrier && !a.up {
		a.burst = announceBurst
	}
	a.up = carrier
	switch {
	case !carrier:
		return false
	case a.burst > 0:
		a.burst--
	case now.Sub(a.last) < announceEvery:
		return false
	}
	a.last = now
	return true
}

// RunVIPAnnounce is `run --vip-announce`, briard-vip-announce.service's ExecStart. VIP_DEV
// comes from the same vip.env briard-vip reads, so it announces on the NIC that was claimed;
// the address itself is briard-vip-arping's to read, from the live file, as at the claim.
func RunVIPAnnounce(ctx context.Context, x Executor) error {
	dev := os.Getenv("VIP_DEV")
	if dev == "" {
		log.Printf("vip announce: no VIP_DEV in the environment; nothing to announce on")
		<-ctx.Done()
		return nil
	}
	var a announcer
	failing := false
	t := time.NewTicker(announceTick)
	defer t.Stop()
	for {
		b, err := x.ReadFile("/sys/class/net/" + dev + "/carrier")
		carrier := err == nil && strings.TrimSpace(string(b)) == "1"
		if carrier && !a.up {
			log.Printf("vip announce: %s has its link; announcing the VIP", dev)
		}
		if a.step(time.Now(), carrier) {
			// One line when announcing starts failing and one when it recovers, never one per
			// beat: a VIP not yet claimed fails here every 30 s and is not news.
			if out, err := x.Run(ctx, tool("briard-vip-arping")); err != nil && !failing {
				failing = true
				log.Printf("vip announce: %v: %s", err, strings.TrimSpace(string(out)))
			} else if err == nil && failing {
				failing = false
				log.Printf("vip announce: announcing again")
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
