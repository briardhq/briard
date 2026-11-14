#!/bin/sh
# briard's shadow of Home Assistant's s6 `run` script.
#
# s6 re-executes `run` at every s6 service start: container start, every exit-100 restart
# (`homeassistant.restart`), and the boot after a config restore, each a window with Home
# Assistant stopped. That is the whole reason briard's stopped-window steps hang here: they heal
# at every boundary, with no extra restart and no race.
#
# No step here holds state or decides anything, as a rule rather than by accident: everything a
# step needs, the guest agent can derive on its own side (from the ring on disk, the manifest on
# the volume, the container unit's own start time), where it is testable and a container cannot
# tamper with it. So this file stays three calls and an exec. One exception, a fact about this
# container rather than a decision: whether `run` has run in it before. The first run follows a
# container start, which the unit's pre-start has already snapshotted with the clean-stop
# marker's answer; notifying again would take the same bytes twice. The flag lives in the
# container's own /run, so it dies with the container and nothing can go stale. Tampering with it
# gains nothing: a container can already call the socket or not, as it likes.
#
# No step can ever fail the app. A household losing Home Assistant because briard could not write
# a token would be a far worse trade than a health gate that degrades to liveness-only, exactly
# what an absent token leaves behind; the same trade, one notch smaller, holds for an
# integration not planted or a snapshot not taken.
#
# The last line hands over to the image's own run script, extracted byte-for-byte from the pinned
# image at converge. We relay Home Assistant's bootstrap, never author it: a hand-written copy
# would drift the day upstream moves the s6 furniture, on every node at once.
#
# ⚠️ The shebang is load-bearing and its absence is not subtle: s6-supervise reports
# `unable to spawn ./run (waiting 60 seconds): Exec format error` and retries forever, so Home
# Assistant simply never starts.
#
# The notify step goes first, and it blocks: the guest agent is told this app is starting and
# gets to act before Home Assistant opens a single file, which makes a member taken there
# application-consistent by construction rather than crash-consistent by luck. Deliberately ahead
# of the two steps below: they write into /config, and a rollback point is worth more taken
# before our own writes than after them. Not on the container's first run (above). ⚠️ Fail
# towards the snapshot: a flag that cannot be written notifies, because the naive
# `[ -e ] || touch` would skip every run forever and say nothing. An extra member is what the
# agent already replaces.
if [ ! -e /run/briard-started ] && touch /run/briard-started 2>/dev/null; then
    :
else
    python3 /briard/notify.py || true
fi

python3 /briard/ensure-token.py /config /briard/token || true

# briard's own integration: the stub package into /config/custom_components,
# the `briard:` line into configuration.yaml, and — on a node's first boot only — Home
# Assistant's own default config, so that line has a file to live in before HA reads it.
python3 /briard/plant.py /config /briard/stub || true

exec /briard/run.original
