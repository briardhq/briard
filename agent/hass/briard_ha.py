"""briard's Home Assistant integration — the implementation, mounted outside /config.

WHY ANY CODE INSIDE HOME ASSISTANT AT ALL. Everything briard does to HA from outside runs in the
stopped window the s6 `run` wrapper provides, and talks HTTP to a process it is already inside
of. That is the right shape for exactly one thing — the token, which has to exist at t=0 with no
return channel — and the wrong shape for everything else. In here there is no waiting for HA to
serve, no token to exchange and no flow API: `hass` is in hand.

THE CONTROL TOKEN'S MINT STAYS OUTSIDE, deliberately. An integration can only mint once HA is
up and loaded, so that value would arrive late and have to be pushed back out to the guest
agent — the return channel the stopped-window mint exists to avoid. This side needs no
credential to act, and the token goes on serving the consumers OUTSIDE HA (the readiness gate).
The one thing minted IN here is the other way round: the household's own LOGIN (`LoginView`),
asked for over HTTP by a caller that is already holding the control token and waiting for the
answer — no return channel, and a live auth manager, which is the thing no outside call has.

FAILURE IS CONTAINED BY HOME ASSISTANT ITSELF, which is what makes code in here affordable on a
fleet: a component's setup runs inside `(CancelledError, SystemExit, Exception)`, so HA logs a
setup error and keeps booting. Briard's features degrade; the household never loses Home
Assistant. Everything below holds itself to the same bar one level down — a step that fails is
logged and dropped, never raised at setup.
"""

import asyncio
from http import HTTPStatus
import logging
import socket

from homeassistant.auth.providers import homeassistant as auth_ha
from homeassistant.components.auth import create_auth_code
from homeassistant.components.http import KEY_HASS, KEY_HASS_USER, HomeAssistantView
from homeassistant.config_entries import SOURCE_USER
from homeassistant.core import CoreState
from homeassistant.data_entry_flow import FlowResultType
from homeassistant.helpers.start import async_at_started

_LOGGER = logging.getLogger(__name__)

DOMAIN = "briard"


class LoginView(HomeAssistantView):
    """POST /api/briard/login — an auth code that logs a browser in as Home Assistant's OWNER.

    THE ONE THING THIS INTEGRATION MINTS, and why it may ([V3b.31a](e), [V3b.31d]): the household
    dashboard has already authenticated the browser — a trusted device, by proof of access to the
    `briard` CLI — so handing it a login to the Home Assistant it owns is delegated auth, not a
    bypass. The shape is HA's own onboarding's: `create_auth_code` is what the user step calls,
    and the code exchanges at /auth/token like any other, bound to the client_id given here.

    ALWAYS THE OWNER, NEVER ANOTHER USER. HA's `is_owner` flag, not ours: stateless, and as true
    of an adopted or restored HA as of one we set up. With no owner — the flag is deletable — this
    REFUSES and says so; it never picks an admin instead, which would be asserting an identity
    nobody authenticated ([V3b.31]).

    Behind HA's own auth and gated on admin. The caller is the node's control channel, whose
    system user is admin; that token already drives the whole HA API, so nothing here widens it.
    """

    url = "/api/briard/login"
    name = "api:briard:login"
    requires_auth = True

    async def post(self, request):
        """Mint a code for the owner, bound to the caller's client_id."""
        if not request[KEY_HASS_USER].is_admin:
            return self.json_message("admin only", HTTPStatus.FORBIDDEN)
        try:
            body = await request.json()
        except ValueError:
            return self.json_message("invalid JSON", HTTPStatus.BAD_REQUEST)
        client_id = body.get("client_id") if isinstance(body, dict) else None
        if not isinstance(client_id, str) or not client_id:
            return self.json_message("client_id required", HTTPStatus.BAD_REQUEST)
        hass = request.app[KEY_HASS]
        owner = await hass.auth.async_get_owner()
        if owner is None:
            return self.json_message("Home Assistant has no owner account", HTTPStatus.CONFLICT, "no_owner")
        credential = next(iter(owner.credentials), None)
        if credential is None:
            return self.json_message("the owner has no credential to log in with", HTTPStatus.CONFLICT, "no_credential")
        return self.json({"auth_code": create_auth_code(hass, client_id, credential)})


class PasswordView(HomeAssistantView):
    """POST /api/briard/password — set a new password on the OWNER's Home Assistant login.

    "Reset and show once" ([V3b.31a](e), [V3b.31e]): the password briard chose at the first open
    is a starting credential the household never typed, and a stored copy of it goes stale the
    moment they change it in Home Assistant — which briard cannot see. So briard keeps no copy:
    the dashboard generates a fresh one, sets it here, shows it once, and the companion app logs
    in with it. HA's own `admin_change_password` is this exact call (`provider.async_change_
    password`), and like it, it REVOKES NOTHING — the sessions the household holds keep working.

    The same owner rule and the same admin gate as LoginView; the same refusals by name.
    """

    url = "/api/briard/password"
    name = "api:briard:password"
    requires_auth = True

    async def post(self, request):
        """Set the owner's password to the one given."""
        if not request[KEY_HASS_USER].is_admin:
            return self.json_message("admin only", HTTPStatus.FORBIDDEN)
        try:
            body = await request.json()
        except ValueError:
            return self.json_message("invalid JSON", HTTPStatus.BAD_REQUEST)
        password = body.get("password") if isinstance(body, dict) else None
        if not isinstance(password, str) or len(password) < 8:
            return self.json_message("password required (8+ characters)", HTTPStatus.BAD_REQUEST)
        hass = request.app[KEY_HASS]
        owner = await hass.auth.async_get_owner()
        if owner is None:
            return self.json_message("Home Assistant has no owner account", HTTPStatus.CONFLICT, "no_owner")
        provider = auth_ha.async_get_provider(hass)
        username = next(
            (c.data["username"] for c in owner.credentials if c.auth_provider_type == provider.type),
            None,
        )
        if username is None:
            return self.json_message("the owner has no password login to reset", HTTPStatus.CONFLICT, "no_credential")
        await provider.async_change_password(username, password)
        return self.json({"username": username})

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

    # The login minter and the password reset, from the moment HA serves: they need nothing
    # loaded but the auth manager.
    hass.http.register_view(LoginView())
    hass.http.register_view(PasswordView())

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
