package main

import (
	"testing"

	"briard.io/shared/routes"
)

// table is the shape converge writes: a flock with a name, one service the door fronts and one
// that announces itself to appliances. Built here the way the node builds it -- names through
// routes.HostName -- so a change to the naming rule fails these tests rather than being restated.
func table(flock string) routes.Table {
	return routes.Table{Services: []routes.Service{
		{
			Name:   "home-assistant",
			Hosts:  []string{routes.HostName(flock, "home-assistant")},
			Routes: []routes.Route{{Listen: routes.ListenName, To: "http://127.0.0.1:8123"}},
		},
		{
			Name:  "mosquitto",
			Hosts: []string{routes.HostName(flock, "mosquitto")},
			Announce: []routes.Announcement{{
				Name: routes.InstanceName(flock, "mosquitto"), Type: "_mqtt._tcp", Port: 1883,
			}},
		},
	}}
}

// What a household gets: the flock's own name, one per service, all at the VIP -- and the SRV
// record pointed at the SERVICE's own name, never at the flock's and never at a hostname. The
// last part is the one that cost a measurement: a node-scoped target sends every device on the
// LAN to the machine that has just stopped being Primary (V3.20).
func TestTheWorldIsTheFlockNameEveryServiceNameAndTheDeclaredRecords(t *testing.T) {
	w := mdnsWorldFor("192.168.1.100", "brave-elf", table("brave-elf"))

	want := []string{
		"briard-brave-elf-home-assistant.local",
		"briard-brave-elf-mosquitto.local",
		"briard-brave-elf.local",
	}
	if len(w.names) != len(want) {
		t.Fatalf("published %v, want %v", w.names, want)
	}
	for i, n := range want {
		if w.names[i] != n {
			t.Errorf("name %d is %q, want %q", i, w.names[i], n)
		}
	}
	if w.addr != "192.168.1.100" {
		t.Errorf("names resolve to %q, not the VIP", w.addr)
	}
	if len(w.svcs) != 1 {
		t.Fatalf("published %d service record(s), want mosquitto's one: %+v", len(w.svcs), w.svcs)
	}
	got := w.svcs[0]
	if got.typ != "_mqtt._tcp" || got.port != 1883 {
		t.Errorf("announced %s:%d, want _mqtt._tcp:1883", got.typ, got.port)
	}
	if got.host != "briard-brave-elf-mosquitto.local" {
		t.Errorf("the SRV target is %q -- it must be the service's OWN name, so a device is not "+
			"sent to a node that has stopped being Primary", got.host)
	}
	if got.instance != "briard-brave-elf-mosquitto" {
		t.Errorf("the instance label is %q, not the flock-scoped name a household picks from", got.instance)
	}
}

// A node with no minted flock name publishes NOTHING rather than a guess: `briard-.local` is worse
// than silence, and the rule is routes.FlockHostName's. The services have no names either, because
// the agent materialises those from the same empty flock.
func TestAnUnnamedFlockPublishesNothing(t *testing.T) {
	w := mdnsWorldFor("192.168.1.100", "", table(""))
	if !w.empty() {
		t.Fatalf("an unnamed flock published %s", w.describe())
	}
	if len(w.svcs) != 0 {
		t.Errorf("it announced %+v with no name to point the record at", w.svcs)
	}
}

// And a node with no VIP publishes nothing either. A name resolving to an address nobody holds is
// worse than a name that does not resolve: the household gets a connection that hangs.
func TestNoVIPPublishesNothing(t *testing.T) {
	if w := mdnsWorldFor("", "brave-elf", table("brave-elf")); !w.empty() {
		t.Fatalf("with no VIP it published %s", w.describe())
	}
}

// The shipped state: a named flock that has converged to nothing still answers its own name, so
// the household can reach the dashboard before installing anything.
func TestAConvergedToNothingNodeStillAnswersTheFlockName(t *testing.T) {
	w := mdnsWorldFor("192.168.1.100", "brave-elf", routes.Table{})
	if w.empty() {
		t.Fatal("a named node with no services published nothing at all")
	}
	if len(w.names) != 1 || w.names[0] != "briard-brave-elf.local" {
		t.Fatalf("published %v, want just the flock's own name", w.names)
	}
}

// An announcement with no name to point at is skipped rather than given a composed fallback:
// routes.Validate already refuses that combination, and a second rule here would be a second
// place for it to be wrong.
func TestAnAnnouncementWithNoHostIsNotPublished(t *testing.T) {
	tbl := routes.Table{Services: []routes.Service{{
		Name:     "mosquitto",
		Announce: []routes.Announcement{{Name: "x", Type: "_mqtt._tcp", Port: 1883}},
	}}}
	if w := mdnsWorldFor("192.168.1.100", "brave-elf", tbl); len(w.svcs) != 0 {
		t.Fatalf("published %+v with no SRV target", w.svcs)
	}
}

// same is what decides whether the responder is torn down and rebuilt, so it has to notice every
// field. A world that compares equal while differing is a name that never appears.
func TestSameNoticesEveryChangeThatMatters(t *testing.T) {
	base := mdnsWorldFor("192.168.1.100", "brave-elf", table("brave-elf"))
	if !base.same(mdnsWorldFor("192.168.1.100", "brave-elf", table("brave-elf"))) {
		t.Fatal("two identical worlds compared different, so the responder would rebuild forever")
	}
	for _, tc := range []struct {
		what string
		w    mdnsWorld
	}{
		{"the VIP moved", mdnsWorldFor("192.168.1.101", "brave-elf", table("brave-elf"))},
		{"the flock was renamed", mdnsWorldFor("192.168.1.100", "brave-yak", table("brave-yak"))},
		{"a service was installed", mdnsWorldFor("192.168.1.100", "brave-elf", routes.Table{
			Services: append(table("brave-elf").Services, routes.Service{
				Name: "immich", Hosts: []string{routes.HostName("brave-elf", "immich")},
			}),
		})},
		{"an announcement changed port", func() mdnsWorld {
			tbl := table("brave-elf")
			tbl.Services[1].Announce[0].Port = 8883
			return mdnsWorldFor("192.168.1.100", "brave-elf", tbl)
		}()},
	} {
		if base.same(tc.w) {
			t.Errorf("%s and the responder would not rebuild: %s vs %s", tc.what, base.describe(), tc.w.describe())
		}
	}
}

// Two nodes of one flock publish BYTE-IDENTICAL records, which is why failover needs no
// announcement to correct anything: the VIP's address does not change when it moves, so the only
// thing a promotion changes is which machine answers. This is the assumption the decision to ship
// no announcer rests on ([B.152]), so it is asserted rather than remembered.
func TestBothNodesOfAFlockPublishTheSameRecords(t *testing.T) {
	primary := mdnsWorldFor("192.168.1.100", "brave-elf", table("brave-elf"))
	peer := mdnsWorldFor("192.168.1.100", "brave-elf", table("brave-elf"))
	if !primary.same(peer) {
		t.Fatalf("the peer would publish %s where the primary publishes %s -- a failover would "+
			"then need an announcement to correct a record, and this ships without one",
			peer.describe(), primary.describe())
	}
}
