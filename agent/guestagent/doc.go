// Package guestagent is the host<->guest control API over the channel
// : the guest-side dispatch (Handler/Serve) that runs DRBD bring-up and
// reads status through an Executor, plus the host-side Client that calls those
// verbs. The guest half is "dumb hands" -- it executes and reports; every
// decision stays in the host's core. Run guest-side as `briard run --guest`.
package guestagent
