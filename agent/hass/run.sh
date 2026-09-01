#!/bin/sh
# briard's shadow of Home Assistant's s6 `run` script.
#
# s6 re-executes `run` at EVERY service start — container start, every exit-100
# restart (`homeassistant.restart`), and the boot after a config restore — and each of
# those is a window with Home Assistant stopped. That is the whole reason the mint
# hangs here: it heals the token at every boundary, with no extra restart and no race.
#
# The mint can never fail the service. A household losing Home Assistant because
# briard could not write a token would be a far worse trade than a health gate that
# degrades to liveness-only, which is exactly what an absent token leaves behind.
#
# The last line hands over to the image's OWN run script, extracted byte-for-byte from
# the pinned image at converge. We relay Home Assistant's bootstrap, never author it:
# a hand-written copy would drift the day upstream moves the s6 furniture, and it would
# drift on every node at once.
#
# ⚠️ THE SHEBANG IS LOAD-BEARING AND ITS ABSENCE IS NOT SUBTLE: s6-supervise reports
# `unable to spawn ./run (waiting 60 seconds): Exec format error` and retries forever,
# so Home Assistant simply never starts. Measured, by losing it to a careless edit.
python3 /briard/ensure-token.py /config /briard/token || true

# The mqtt wiring ([V3b.30](c)), DETACHED, because this moment is the one it cannot
# use: the config-flow API needs a SERVING Home Assistant, and `run` is precisely the
# window where there is none. So it is spawned to wait on its own while we hand over
# below — which also makes an HA restart the household's way to ask for it again.
#
# Backgrounded and fully redirected so it can neither hold the service open nor write
# into s6's stream, and it swallows its own failures, for the same reason the mint has
# `|| true`: nothing briard does here may cost a household its Home Assistant.
python3 /briard/wire-mqtt.py /briard/token >/dev/null 2>&1 &

exec /briard/run.original
