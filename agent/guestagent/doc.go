// Package guestagent is the host<->guest control API over the channel: the guest-side dispatch
// (Serve) that runs DRBD bring-up, services and converge and reads status through an Executor,
// plus the host-side Client that calls those verbs. The guest half is "dumb hands" -- it
// executes and reports; every decision stays in the host's core. Run guest-side as
// `briard-guest-agent run --guest` ([B.137]).
//
// IT IS PUSHED, NOT BAKED ([B.139]). The guest image carries agent/guestfirmware alone -- the
// framing, the handshake, the three push verbs and os.poweroff -- and the host dresses every
// guest with this package's binary at bring-up. That is what keeps the 400 MB guest chain still
// while the agent moves: the image's inputs hash covers the firmware's import graph and nothing
// here.
package guestagent
