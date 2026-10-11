// Package chain names the guest's promoter chain: the ordered units drbd-reactor starts on the
// node it has promoted, and the static target a lone node starts instead of a promoter.
//
// IT IS ONE LIST BECAUSE IT HAS TO BE. The order is a dependency, not a preference: the host
// hands it to drbd-reactor as the start-list, and the guest agent writes it into every member's
// `Requires=`/`After=` and into the lone node's target. Two spellings of one order is
// two things that can disagree, and the way they disagree is a promotion that half-starts. It
// used to live in four places -- the host's reactor config, the image's `chainMembers`, the
// image's target, and the image's hold -- kept in step by hand; the ones in the image are gone
// now that the agent renders the units, and the remaining two Go consumers read this.
//
// ⚠️ THE RIG'S COPY IS THE ONE THIS CANNOT REACH. nixosTest/lib.nix writes its own reactor
// snippet in nix, which cannot import Go -- so internal/arch asserts the two are equal rather
// than trusting the comment that used to say "kept in step BY HAND".
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
// start-list cannot name the services themselves. briard-services is the unit that, once the
// mount exists, reads the manifests, renders and starts them. The runtime-installed services are
// therefore NOT members, which is also what makes "a service error alerts but never demotes"
// mechanically true: drbd-reactor never sees them, so a crashed container cannot deactivate the
// target.
//
// Nothing here is conditional on a service existing. The old conditional membership existed
// because naming a unit the guest does not define fails the WHOLE ordered chain, and a
// zero-service node has no service unit to name -- but briard-services is rendered
// unconditionally, exactly as briard-primary-storage and briard-vip are, so there is nothing
// left to make conditional. It takes no arguments, and that is the end state converge-at-promotion was
// after: the chain is the same units on every anchor in the fleet.
//
// THE FRONT DOOR IS A MEMBER AND CARRIES THE HOUSEHOLD'S mDNS NAMES. On a
// node with no `.casa` domain the door routes `byHost` against the service's own `.local` names,
// so a request to the bare VIP matches nothing: the NAME is the only path to a service, and a
// node that cannot publish is unreachable while every other part of it reports healthy. That is
// what the promoter is for. Publishing and answering being one member is also what makes them
// impossible to disagree -- the door answers for exactly the names it routes. Membership means
// it never runs on the node that LOST the promotion race: reactor gives every member
// `Requires=drbd-promote@<res>.service`, so its job fails with 'dependency' and never executes,
// where a `wantedBy` binding would start it into a node that had claimed no address.
//
// ORDER IS THE DEPENDENCY: each member Requires= and After= the PREVIOUS one, so the door starts
// only once briard-vip holds the address its names resolve to. Publishing a name that nothing
// answers is the failure mode the order exists to avoid.
func Members() []string {
	return []string{
		"briard-primary-storage.service",
		"briard-vip.service",
		"briard-services.service",
		"briard-reverse-proxy.service",
		"briard-dashboard.service",
	}
}
