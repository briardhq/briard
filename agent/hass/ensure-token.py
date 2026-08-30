"""Ensure briard's control token in Home Assistant's auth store.

Run one-shot, on the image's own python, in a STOPPED window — the wrapper runs it
from s6's `run` script, before Home Assistant is launched. It cannot run against a
live HA: the auth store is memory-cached, so a write under a running HA is invisible
to it until restart and is clobbered by HA's next save. That is why the mint lives at
the service-start boundary and nowhere else.

It ENSURES rather than creates. The value is chosen by the caller (briard writes it to
tmpfs at converge), so consumers know the token from t=0 and need no return channel;
validating a refresh token is a store lookup of the string, so a chosen value is as
good as a generated one. And it PRUNES every other token on our user, so a token
resurrected by a backup restore is revoked as a side effect of the next start.

Reached through Home Assistant's own bootstrap — the same `auth_manager_from_config`
path `homeassistant/scripts/auth.py` uses — rather than by writing `.storage/auth`
ourselves: the format is private and the image already owns the code that maintains it.
"""

import asyncio
import sys

from homeassistant import runner
from homeassistant.auth import auth_manager_from_config
from homeassistant.auth.const import GROUP_ID_ADMIN
from homeassistant.auth.models import TOKEN_TYPE_SYSTEM
from homeassistant.const import EVENT_HOMEASSISTANT_FINAL_WRITE
from homeassistant.core import HomeAssistant
from homeassistant.helpers import device_registry as dr, entity_registry as er

# The system user briard owns. System-generated, so it is hidden from the People page
# and can only ever hold system tokens, which never expire.
USER = "briard"

# Shorter than this is not a token we wrote. The guard matters because an empty file
# would otherwise mint the empty string as a valid credential.
MIN_TOKEN_LEN = 32


async def ensure(config_dir: str, token: str) -> None:
    hass = HomeAssistant(config_dir)
    # The registries have to be up before the auth manager: removing a refresh token reaches
    # into the device registry, and on a release that expects it and does not have it the mint
    # dies with "Device registry not set up".
    #
    # GUARDED BECAUSE THE STEP IS NEWER THAN THE RELEASES WE UPGRADE FROM, and this package
    # necessarily spans two of them: an upgrade mints under the old image for the baseline and
    # under the new one afterwards. Measured across 2025.11.0 / 2025.12.0 / 2026.7.1 -- the older
    # pair has no `async_setup` at all and raises AttributeError if it is called, the newer one
    # raises RuntimeError if it is not. Presence is the only thing that separates them.
    if hasattr(dr, "async_setup"):
        dr.async_setup(hass)
    await asyncio.gather(dr.async_load(hass), er.async_load(hass))
    hass.auth = await auth_manager_from_config(hass, [{"type": "homeassistant"}], [])

    user = next(
        (
            u
            for u in await hass.auth.async_get_users()
            if u.name == USER and u.system_generated
        ),
        None,
    )
    # THE ADMIN GROUP IS FOR ACTING, not for reading. Measured against 2026.7.1: the
    # health gate's own signal — `GET /api/config/config_entries/entry` — carries no
    # `@require_admin` and a group-less system user reads it with a 200. What is
    # admin-gated is everything that acts: `/api/services/...` returns 401 without it,
    # config and options flows are decorated, and that is the half this channel exists to
    # keep available to its consumers. A system user is only admin by virtue of this
    # group (`User.is_admin`), so it has to be asked for here or not at all.
    if user is None:
        user = await hass.auth.async_create_system_user(USER, group_ids=[GROUP_ID_ADMIN])
    elif not user.is_admin:
        await hass.auth.async_update_user(user, group_ids=[GROUP_ID_ADMIN])

    for stale in list(user.refresh_tokens.values()):
        hass.auth.async_remove_refresh_token(stale)
    refresh = await hass.auth.async_create_refresh_token(user, token_type=TOKEN_TYPE_SYSTEM)
    refresh.token = token

    # The store saves on a DELAY, and `hass.async_stop()` — what HA's own auth script
    # ends with — returns immediately on a HomeAssistant that was never started
    # (`core.py`: `if self.state is CoreState.not_running: return`), so it flushes
    # nothing. Firing the final-write event is what the delayed save is waiting for.
    hass.bus.async_fire(EVENT_HOMEASSISTANT_FINAL_WRITE)
    await hass.async_block_till_done()


def main() -> None:
    config_dir, token_file = sys.argv[1], sys.argv[2]
    with open(token_file, encoding="utf-8") as f:
        token = f.read().strip()
    if len(token) < MIN_TOKEN_LEN:
        raise SystemExit(f"{token_file}: not a briard token")
    asyncio.set_event_loop_policy(runner.HassEventLoopPolicy(False))
    asyncio.run(ensure(config_dir, token))


main()
