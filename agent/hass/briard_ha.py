"""briard's Home Assistant integration — the implementation, mounted outside /config.

WHY ANY CODE INSIDE HOME ASSISTANT AT ALL. Everything briard does to HA from outside runs in the
stopped window the s6 `run` wrapper provides, and talks HTTP to a process it is already inside
of. That is the right shape for exactly one thing — the token, which has to exist at t=0 with no
return channel — and the wrong shape for everything else. In here there is no waiting for HA to
serve, no token to exchange and no flow API: `hass` is in hand.

THE MINT STAYS OUTSIDE, deliberately. An integration can only mint once HA is up and loaded, so
the value would arrive late and have to be pushed back out to the guest agent — the return
channel the stopped-window mint exists to avoid. The split that dissolves the trade is that this
side does not mint at all: it needs no credential to act, and the token goes on serving the
consumers OUTSIDE HA (the readiness gate). One minter, one in-process actor, no second
credential.

FAILURE IS CONTAINED BY HOME ASSISTANT ITSELF, which is what makes code in here affordable on a
fleet: a component's setup runs inside `(CancelledError, SystemExit, Exception)`, so HA logs a
setup error and keeps booting. Briard's features degrade; the household never loses Home
Assistant. Everything below holds itself to the same bar one level down — a step that fails is
logged and dropped, never raised at setup.
"""

import asyncio
import logging
import socket

from homeassistant.config_entries import SOURCE_USER
from homeassistant.core import CoreState
from homeassistant.data_entry_flow import FlowResultType
from homeassistant.helpers.start import async_at_started

_LOGGER = logging.getLogger(__name__)

DOMAIN = "briard"

# The event the node fires on Home Assistant's own bus when something OUTSIDE Home Assistant
# changed that this integration may want to act on — today, a broker that was installed next to an
# HA already running ([B.131]). The other half of the contract is agent/hass/nudge.go's, and it is
# that the event carries NOTHING: it means "reconsider", and everything below re-derives its world
# from scratch, so a lost, duplicated or unrelated one all cost the same.
EVENT_RECONSIDER = "briard_reconsider"

# The broker briard installs, as Home Assistant reaches it: every service shares the guest's
# network namespace or publishes into it, so the address is loopback and the config is anonymous.
# The port is substituted by the node from the OTHER service's package, because it belongs to
# that one and a second copy here would be a second thing to keep in step with the catalog.
BROKER_HOST = "127.0.0.1"
BROKER_PORT = @MQTT_PORT@


async def async_setup(hass, config):
    """Set up briard from its `briard:` line in configuration.yaml."""

    # ONE AT A TIME. There are two ways in — the start below and a nudge from the node — and they
    # can land together, which without this is two flows racing the same empty slot. HA's
    # `single_config_entry: true` would refuse the second, so the cost is a confusing log line
    # rather than a duplicate entry; a lock is cheaper than explaining that.
    running = asyncio.Lock()

    async def _reconsider():
        async with running:
            try:
                await _wire_mqtt(hass)
            except Exception:  # noqa: BLE001 — nothing in here may cost a household its Home Assistant
                _LOGGER.warning("briard: could not offer the node's MQTT broker", exc_info=True)

    async def _started(_hass):
        await _reconsider()

    async def _nudged(_event):
        # ONLY ONCE HA IS FULLY UP. A nudge that arrives mid-boot is not lost work: `_started`
        # below has not run yet and will, against a settled Home Assistant. Acting now would mean
        # starting a config flow into a boot that is still setting entries up, for no gain.
        # `hass.is_running` is deliberately NOT the test — it is true during `starting` too.
        if hass.state is not CoreState.running:
            _LOGGER.debug("briard: nudged during startup; the start hook will do this")
            return
        await _reconsider()

    # Started, not set-up: config entries are loaded by then, so "does an mqtt entry exist" has a
    # truthful answer, and starting a flow is not competing with the rest of the boot.
    async_at_started(hass, _started)
    # And the same work on demand, for everything that changes outside Home Assistant while it is
    # running. The listener is never removed: this integration lives for the lifetime of the
    # process, and `async_setup` has no unload counterpart to remove it in.
    hass.bus.async_listen(EVENT_RECONSIDER, _nudged)
    return True


def _broker_listening():
    """Is there actually a broker on the node? Blocking; called in the executor."""
    try:
        with socket.create_connection((BROKER_HOST, BROKER_PORT), timeout=5):
            return True
    except OSError:
        return False


async def _wire_mqtt(hass):
    """Point Home Assistant at the node's broker, once, if nothing else has.

    THE ONLY RULE, and it is deliberately blunt: if Home Assistant holds ANY mqtt config entry —
    ours, the household's, disabled, ignored, broken — this does nothing, ever. Only a completely
    empty slot is filled.

      * mqtt is `single_config_entry: true`, so the household gets exactly one. A household
        pointing Home Assistant at their own broker must never find that slot taken by a
        localhost entry they did not ask for, and "any entry at all" is the cheapest rule that
        cannot get that wrong.
      * It is how the refusal is STORED rather than inferred. Disabling the entry leaves it in
        place — `async_entries` counts ignored and disabled ones — so a household that disables
        ours never gets it back. That durable no lives in Home Assistant's own store, on the
        replicated volume, rather than in a marker beside it that could disagree.
      * DELETING is not disabling, and that is the sharp edge worth knowing: a deleted entry
        leaves an empty slot, so the next Home Assistant start puts it back. It is close to what
        upstream does for a discovered integration (removing one fires rediscovery), and the
        cadence is the point — the trigger is an HA start, not a timer, so nothing "pops" while
        somebody is looking at the screen.

    A FLOW AND NOT A PLANTED ENTRY, for the reason the flow API version had: HA's own
    `try_connection` runs on submit, so a broker that is not answering yields an error instead of
    an entry pointing at nothing, and nothing here writes a storage format we do not own.
    """
    if hass.config_entries.async_entries("mqtt"):
        return
    # A BROKER MUST ACTUALLY BE THERE. Checked with a socket rather than by starting a flow and
    # letting it fail: an abandoned flow is visible clutter in the household's UI, and a node with
    # no broker installed would leave one at every start forever.
    if not await hass.async_add_executor_job(_broker_listening):
        return

    result = await hass.config_entries.flow.async_init("mqtt", context={"source": SOURCE_USER})
    flow_id = result["flow_id"]
    try:
        result = await hass.config_entries.flow.async_configure(
            flow_id, {"broker": BROKER_HOST, "port": BROKER_PORT}
        )
    except Exception:
        _abort(hass, flow_id)
        raise
    if result["type"] != FlowResultType.CREATE_ENTRY:
        # A form back means the broker refused the connection between the socket check and the
        # submit. Never leave the half-finished flow behind: it shows up in the household's UI as
        # something they have to dismiss, which is what this design exists to avoid.
        _abort(hass, flow_id)
        _LOGGER.warning("briard: Home Assistant did not accept the broker: %s", result.get("type"))
        return
    _LOGGER.info("briard: pointed Home Assistant at the node's MQTT broker")


def _abort(hass, flow_id):
    """Drop a flow that is still in progress; a finished one is already gone."""
    try:
        hass.config_entries.flow.async_abort(flow_id)
    except Exception:  # noqa: BLE001 — UnknownFlow is the normal case here
        pass
