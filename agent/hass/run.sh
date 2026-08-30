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
python3 /briard/ensure-token.py /config /briard/token || true
exec /briard/run.original
