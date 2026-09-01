"""Point Home Assistant at the node's MQTT broker, once, if nothing else has ([V3b.30](c)).

RUN DETACHED FROM THE s6 `run` WRAPPER, because the wrapper's moment is the one moment this
cannot use: `run` executes with Home Assistant STOPPED, and the config-flow API needs a serving
one. So the wrapper spawns this and hands over; this waits, acts, and exits.

WHY A FLOW AND NOT A PLANTED FILE. Driving Home Assistant's own config-flow API means
`async_validate_broker_settings` runs on submit, so a broker that is not answering yields an
error instead of an entry pointing at nothing. Planting `.storage/core.config_entries` in the
stopped window would skip exactly that check, and write an internal format we do not own.

THE ONLY RULE, and it is deliberately blunt: if Home Assistant holds ANY mqtt config entry --
ours, the household's, disabled, ignored, broken -- this does nothing, ever. Only a completely
empty slot is filled.

  * mqtt is `single_config_entry: true`, so the household gets exactly one. A household pointing
    Home Assistant at their own broker must never find that slot taken by a localhost entry they
    did not ask for, and "any entry at all" is the cheapest rule that cannot get that wrong.
  * It is how the refusal is STORED rather than inferred. Disabling the entry leaves it in place
    -- MEASURED: the API lists disabled and ignored entries, `async_entries()` defaults to
    `include_ignore=True, include_disabled=True` -- so a household that disables ours never gets
    it back. That is the durable no, and it lives in Home Assistant's own store, on the
    replicated volume, rather than in a marker beside it that could disagree.
  * DELETING is not the same as disabling, and this is the sharp edge worth knowing: a deleted
    entry leaves an empty slot, so the next Home Assistant start puts it back. That is close to
    what upstream does for a discovered integration (removing one fires rediscovery), and the
    cadence is the point -- the trigger is an HA restart, not a timer, so nothing "pops" while
    somebody is looking at the screen.

Failure is silent by design. Everything here is a convenience; nothing about Home Assistant
running depends on it, and a household must never lose HA because briard could not reach a
broker.
"""

import json
import socket
import sys
import time
import urllib.error
import urllib.request

BASE = "http://127.0.0.1:@HA_PORT@"
BROKER_HOST = "127.0.0.1"
BROKER_PORT = @MQTT_PORT@
# Home Assistant's first boot builds the recorder database and the onboarding store before it
# serves, and a household's node is not fast. Generous, because the cost of being early is doing
# nothing at all until the next restart.
SERVE_DEADLINE = 600
POLL = 5


def _request(path, token=None, data=None, method=None):
    body = json.dumps(data).encode() if data is not None else None
    req = urllib.request.Request(BASE + path, data=body, method=method)
    if token:
        req.add_header("Authorization", "Bearer " + token)
    if body:
        req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=30) as resp:
        raw = resp.read()
    return json.loads(raw) if raw else None


def _access_token(refresh):
    """The documented refresh -> access exchange, client_id omitted for a system token."""
    body = ("grant_type=refresh_token&refresh_token=" + refresh).encode()
    req = urllib.request.Request(BASE + "/auth/token", data=body, method="POST")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read())["access_token"]


def main():
    with open(sys.argv[1]) as fh:
        refresh = fh.read().strip()
    if not refresh:
        return

    # Wait for a SERVING Home Assistant. It is also the only restore-in-flight signal that cannot
    # lie: the restore path never serves the API, and its `.HA_RESTORE` marker is unlinked before
    # the wipe, so nothing else can be checked from out here.
    deadline = time.monotonic() + SERVE_DEADLINE
    while time.monotonic() < deadline:
        try:
            _request("/manifest.json")
            break
        except Exception:
            time.sleep(POLL)
    else:
        return

    token = _access_token(refresh)

    # THE SLOT MUST BE EMPTY. Asked with ?domain=mqtt so the answer is about one integration.
    entries = _request("/api/config/config_entries/entry?domain=mqtt", token)
    if entries:
        return

    # AND A BROKER MUST ACTUALLY BE THERE. Checked with a socket rather than by starting a flow
    # and letting it fail: an abandoned flow is visible clutter in the household's UI, and a node
    # with no broker installed would leave one at every restart forever.
    try:
        with socket.create_connection((BROKER_HOST, BROKER_PORT), timeout=5):
            pass
    except OSError:
        return

    flow = _request("/api/config/config_entries/flow", token, {"handler": "mqtt"})
    flow_id = flow["flow_id"]
    try:
        # Two fields, because every service shares the guest's network namespace or publishes to
        # it, so Home Assistant reaches the broker on loopback, and the config is anonymous.
        result = _request(
            "/api/config/config_entries/flow/" + flow_id,
            token,
            {"broker": BROKER_HOST, "port": BROKER_PORT},
        )
        if result.get("type") != "create_entry":
            raise RuntimeError("flow did not create an entry: %s" % result.get("type"))
    except Exception:
        # Never leave a half-finished flow behind: it shows up in the household's UI as something
        # they have to dismiss, which is precisely the noise this design exists to avoid.
        try:
            _request("/api/config/config_entries/flow/" + flow_id, token, method="DELETE")
        except Exception:
            pass
        raise


try:
    main()
except Exception:
    sys.exit(0)
