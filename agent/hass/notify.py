"""Tell the guest agent this service is starting, and wait for it to say go.

THE WAIT IS THE WHOLE POINT ([B.143]). briard's s6 `run` wrapper is the one moment Home
Assistant is stopped and its files are closed at every boundary that matters -- container
start, every `homeassistant.restart` exit-100, and the boot after a config restore -- and the
agent needs that window to take an application-consistent snapshot. Blocking here before the
wrapper `exec`s the real entrypoint is what makes the window real rather than a race.

IT DECIDES NOTHING, which is the design: whether to take a member at all, what to call it,
whether the last one was recent enough to skip -- all of that is the agent's, derived from the
ring on disk. This sends a verb and prints what came back. Everything that used to be proposed
for this side (remember whether we have run before in this container, read the restore marker,
carry a rate limit) lives there instead, where it is testable and where a container cannot
tamper with it.

IT NEVER FAILS THE SERVICE. Every path here exits 0, and the wrapper calls it with `|| true`
besides. A household losing Home Assistant because a snapshot socket did not answer would be a
far worse trade than a ring missing one member -- the same trade the token mint and the
integration planter next door already make.

IT CANNOT SAY WHO IT IS, and that is not an omission. It presents a TOKEN the node minted for
this service and mounted read-only here; the agent maps that back to a name. A field naming a
service would be a field a hostile custom component could set, and a token it can only have if
it was given one.
"""

import json
import socket
import sys

# ⚠️ SIBLINGS OF /briard, not children: /briard is a READ-ONLY bind, and a mount destination
# inside it cannot be created by the runtime. agent/services carries the full trace.
SOCKET = "/briard-inbound.sock"
TOKEN = "/briard-inbound.token"

# Generous but finite. The agent's work is one btrfs snapshot of this service's subvolume, which
# is milliseconds; anything approaching this means something is wrong on the other side, and
# waiting longer would hold a household's Home Assistant down for no benefit.
TIMEOUT = 30


def main():
    try:
        with open(TOKEN) as f:
            token = f.read().strip()
    except OSError as e:
        # No token is the ordinary case on a service that was never given the channel, and on any
        # node whose agent predates it. Neither is worth more than a line.
        print(f"briard: no inbound token ({e}); starting without a snapshot", file=sys.stderr)
        return
    try:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(TIMEOUT)
        s.connect(SOCKET)
    except OSError as e:
        # No socket is the ordinary case on a service that was never given one, and on any node
        # whose agent is older than this wrapper. Neither is worth a word in the log.
        print(f"briard: no inbound channel ({e}); starting without a snapshot", file=sys.stderr)
        return
    try:
        with s:
            s.sendall(json.dumps({"verb": "service.starting", "token": token}).encode() + b"\n")
            # One line, then done. The agent closes after answering, so an empty read is the
            # other end going away rather than a message we should keep waiting for.
            buf = b""
            while not buf.endswith(b"\n"):
                chunk = s.recv(4096)
                if not chunk:
                    break
                buf += chunk
        reply = json.loads(buf.decode() or "{}")
    except (OSError, ValueError) as e:
        print(f"briard: inbound channel did not answer ({e})", file=sys.stderr)
        return
    if reply.get("error"):
        print(f"briard: {reply['error']}", file=sys.stderr)
    elif reply.get("detail"):
        print(f"briard: {reply['detail']}", file=sys.stderr)


main()
