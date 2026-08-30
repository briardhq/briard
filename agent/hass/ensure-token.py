"""Ensure briard's control token in Home Assistant's auth store.

Run one-shot, on the image's own python, in a STOPPED window — the wrapper runs it from s6's `run`
script, before Home Assistant is launched. It cannot run against a live HA: the auth store is
memory-cached, so a write under a running HA is invisible to it until restart and is clobbered by
its next save. That is why the mint lives at the service-start boundary and nowhere else.

It ENSURES rather than creates. The value is chosen by the caller (briard writes it to tmpfs at
converge), so consumers know the token from t=0 and need no return channel; validating a refresh
token is a store lookup of the string, so a chosen value is as good as a generated one. And it
PRUNES every other token on our user, so a token resurrected by a backup restore is revoked as a
side effect of the next start.

⚠️ THE BOOTSTRAP IS BORROWED, NOT COPIED, and that is the whole robustness story here.

`homeassistant.scripts.auth.run_command` is the setup HA's own `hass --script auth` runs: build a
HomeAssistant for a config dir, bring up the registries, construct the auth manager, initialise the
provider, then hand all of it to a callback. We pass our mint as that callback and let HA do every
step before it.

The alternative — reimplementing those five lines — is what this file used to do, and it broke
exactly the way copies break. HA 2026 added a `device_registry.async_setup` call ahead of the
registry load, and the auth store's own `async_load` now needs it; releases before that have no
such function at all. So a hand-written copy raises AttributeError on the old images and
RuntimeError on the new ones, and this package necessarily spans BOTH — an upgrade mints under the
old image to take the readiness baseline and under the new one afterwards. Borrowing the bootstrap
means the version-sensitive part is maintained by the people who change it, and our file carries no
version branch at all.

What is left below is only the part HA has no verb for: minting a system token with a value we
chose. That surface is the auth manager's public shape, plus one assignment.
"""

import argparse
import asyncio
import sys

from homeassistant import runner
from homeassistant.auth.const import GROUP_ID_ADMIN
from homeassistant.auth.models import TOKEN_TYPE_SYSTEM
from homeassistant.const import EVENT_HOMEASSISTANT_FINAL_WRITE
from homeassistant.scripts import auth as ha_auth

# The system user briard owns. System-generated, so it is hidden from the People page and can only
# ever hold system tokens, which never expire.
USER = "briard"

# Shorter than this is not a token we wrote. The guard matters because an empty file would
# otherwise mint the empty string as a valid credential.
MIN_TOKEN_LEN = 32


def minter(token):
    """Build the callback run_command will hand the bootstrapped hass to."""

    async def mint(hass, _provider, _args):
        user = next(
            (
                u
                for u in await hass.auth.async_get_users()
                if u.name == USER and u.system_generated
            ),
            None,
        )
        # THE ADMIN GROUP IS FOR ACTING, not for reading. Measured against 2026.7.1: the health
        # gate's own signal — `GET /api/config/config_entries/entry` — carries no `@require_admin`
        # and a group-less system user reads it with a 200. What is admin-gated is everything that
        # acts: `/api/services/...` returns 401 without it, config and options flows are decorated,
        # and that is the half this channel exists to keep available to its consumers. A system
        # user is only admin by virtue of this group (`User.is_admin`), so it has to be asked for
        # here or not at all.
        if user is None:
            user = await hass.auth.async_create_system_user(USER, group_ids=[GROUP_ID_ADMIN])
        elif not user.is_admin:
            await hass.auth.async_update_user(user, group_ids=[GROUP_ID_ADMIN])

        for stale in list(user.refresh_tokens.values()):
            hass.auth.async_remove_refresh_token(stale)
        refresh = await hass.auth.async_create_refresh_token(user, token_type=TOKEN_TYPE_SYSTEM)
        refresh.token = token

        # The store saves on a DELAY, and run_command ends with `hass.async_stop()`, which returns
        # immediately on a HomeAssistant that was never started (`core.py`: `if self.state is
        # CoreState.not_running: return`) — so it flushes nothing. Firing the final-write event is
        # what the delayed save is waiting for, and it has to happen here, inside the callback,
        # while the store still exists.
        hass.bus.async_fire(EVENT_HOMEASSISTANT_FINAL_WRITE)
        await hass.async_block_till_done()

    return mint


def main():
    config_dir, token_file = sys.argv[1], sys.argv[2]
    with open(token_file, encoding="utf-8") as f:
        token = f.read().strip()
    if len(token) < MIN_TOKEN_LEN:
        raise SystemExit(f"{token_file}: not a briard token")
    asyncio.set_event_loop_policy(runner.HassEventLoopPolicy(False))
    # run_command reads exactly two attributes off the namespace — verified against both ends of
    # the range we span — so this is the whole of its input.
    asyncio.run(ha_auth.run_command(argparse.Namespace(config=config_dir, func=minter(token))))


main()
