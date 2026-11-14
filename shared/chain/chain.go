// Package chain names the guest's promoter chain: the ordered units drbd-reactor starts on the
// node it has promoted, and the static target a lone node starts instead of a promoter.
//
// IT IS ONE LIST BECAUSE IT HAS TO BE. The order is a dependency, not a preference: the host
// hands it to drbd-reactor as the start-list, and the guest agent writes it into every member's
// `Requires=`/`After=`, into the lone node's target and into its hold. Two spellings of one
// order is two things that can disagree, and the way they disagree is a promotion that
// half-starts.
//
// ⚠️ THE RIG'S COPY IS THE ONE THIS CANNOT REACH. nixosTest/lib.nix writes its own reactor
// snippet in nix, which cannot import Go -- so internal/arch asserts the two are equal rather
// than trusting them to be kept in step by hand.
package chain

// Target is the lone node's chain target: the promoter chain with no promoter. A home
// with one diskful member runs no DRBD, so nothing generates drbd-services@r0.target for it and
// this static target carries the identical member list in the identical order.
const Target = "briard-chain.target"

// Members is the ordered promoter chain for a data node: mount the DRBD volume -> claim the VIP
// -> converge this node to what the volume says it runs -> answer on it -> serve the dashboard.
//
// THE ADDRESS COMES BEFORE THE APPS, because the appliance is set up before anything runs on it.
// An app reads the network it starts on, often exactly once: Home Assistant picks the interface
// it announces itself on and builds its own URL from the source address the kernel gives it for
// the mDNS group, at boot. An app started before the VIP exists sees a network without the
// household LAN on it and keeps that answer until its next restart. The VIP needs only the mount
// (a remembered address is read from the volume), so nothing it depends on comes later.
//
// IT IS STATIC, and that is what makes converge-at-promotion possible. The chain is
// what drbd-reactor promotes WITH, but the volume is only readable AFTER promotion -- so the
// start-list cannot name the apps themselves. briard-apps is the unit that, once the
// mount exists, reads the manifests, renders and starts them. The runtime-installed apps are
// therefore NOT members, which is also what makes "an app error alerts but never demotes"
// mechanically true: drbd-reactor never sees them, so a crashed container cannot deactivate the
// target.
//
// Nothing here is conditional on an app existing, and nothing may be: naming a unit the guest
// does not define fails the WHOLE ordered chain. briard-apps is rendered unconditionally, as
// briard-primary-storage and briard-vip are, so a zero-app node names only units it has, and the
// chain is the same units on every anchor in the fleet.
//
// THE REVERSE PROXY IS A MEMBER AND CARRIES THE HOUSEHOLD'S mDNS NAMES. On a
// node with no `.casa` domain the reverse proxy routes `byHost` against the app's own `.local`
// names, so a request to the bare VIP matches nothing: the NAME is the only path to an app, and a
// node that cannot publish is unreachable while every other part of it reports healthy. That is
// what the promoter is for. Publishing and answering being one member is also what makes them
// impossible to disagree -- the reverse proxy answers for exactly the names it routes. Membership
// means it never runs on the node that LOST the promotion race: reactor gives every member
// `Requires=drbd-promote@<res>.service`, so its job fails with 'dependency' and never executes,
// where a `wantedBy` binding would start it into a node that had claimed no address.
//
// ORDER IS THE DEPENDENCY: each member Requires= and After= the PREVIOUS one, so the reverse proxy
// starts only once briard-vip holds the address its names resolve to. Publishing a name that
// nothing answers is the failure mode the order exists to avoid.
func Members() []string {
	return []string{
		"briard-primary-storage.service",
		"briard-vip.service",
		"briard-apps.service",
		"briard-reverse-proxy.service",
		"briard-dashboard.service",
	}
}
