#!/bin/sh
# briard's shadow of Home Assistant's s6 `run` script.
#
# s6 re-executes `run` at EVERY service start — container start, every exit-100
# restart (`homeassistant.restart`), and the boot after a config restore — and each of
# those is a window with Home Assistant stopped. That is the whole reason briard's two
# stopped-window steps hang here: they heal at every boundary, with no extra restart
# and no race.
#
# Neither step can ever fail the service. A household losing Home Assistant because
# briard could not write a token would be a far worse trade than a health gate that
# degrades to liveness-only, which is exactly what an absent token leaves behind — and
# the same trade, one notch smaller, for an integration that is not planted.
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

# briard's own integration ([B.124]): the stub package into /config/custom_components,
# the `briard:` line into configuration.yaml, and — on a node's first boot only — Home
# Assistant's own default config, so that line has a file to live in before HA reads it.
python3 /briard/plant.py /config /briard/stub || true

exec /briard/run.original
