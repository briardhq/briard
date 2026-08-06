"""Briard Canary — a controllable HA config-entry test integration.

Two jobs, both in service of testing the health-gate:

1. **A real, self-inflicted regressed migration.** The integration loads cleanly on
   the `from` HA release and fails its config-entry *migration* on the `to` release, so
   the upgrade drives a real ``migration_error`` through HA's own config-entry state
   machine (see config_flow.py for the version-conditional VERSION). The code is
   identical across the upgrade — only the running HA version differs — so the only
   "synthetic" part is that we authored the integration; the state transition is HA's.

2. **A headless state reporter.** On startup it writes every config entry's *live*
   state (``entry.state.value``, straight from ``hass.config_entries``) to
   ``/config/briard_entry_states.json``, so the test reads HA's real verdict without
   authenticating to the API.

Loaded via a ``briard_canary:`` line in configuration.yaml, so ``async_setup`` always
runs (registering the reporter + creating the entry through an import flow, which lets
HA persist the entry in its own native format — robust across the version pair).
"""

import json
import logging
from datetime import timedelta

from homeassistant.config_entries import SOURCE_IMPORT, ConfigEntry
from homeassistant.const import EVENT_HOMEASSISTANT_STARTED
from homeassistant.core import Event, HomeAssistant
from homeassistant.helpers.event import async_track_time_interval

_LOGGER = logging.getLogger(__name__)

DOMAIN = "briard_canary"
STATE_FILE = "/config/briard_entry_states.json"


async def async_setup(hass: HomeAssistant, config: dict) -> bool:
    """Register the state reporter and ensure our config entry exists."""

    def _write(entries: list[dict]) -> None:
        with open(STATE_FILE, "w") as fh:
            json.dump(entries, fh)

    async def _dump(_) -> None:
        # Read every entry's live state from HA's real config-entry manager — this is
        # the exact signal the production ReadinessAssessor will fetch from the API.
        entries = [
            {"entry_id": e.entry_id, "domain": e.domain, "state": e.state.value}
            for e in hass.config_entries.async_entries()
        ]
        await hass.async_add_executor_job(_write, entries)

    # Dump once at startup for promptness, then keep re-dumping: the file must converge
    # to the *settled* state (the import flow may finish loading our entry after STARTED
    # fires, and post-upgrade the state flips to migration_error only once migration
    # runs). The test waits for the file to show the expected settled state.
    hass.bus.async_listen_once(EVENT_HOMEASSISTANT_STARTED, _dump)
    async_track_time_interval(hass, _dump, timedelta(seconds=3))

    # First boot: create our entry via an import flow so HA writes it natively.
    if not hass.config_entries.async_entries(DOMAIN):
        hass.async_create_task(
            hass.config_entries.flow.async_init(
                DOMAIN, context={"source": SOURCE_IMPORT}, data={}
            )
        )
    return True


async def async_setup_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Load cleanly. The regression is injected via migration, not setup."""
    return True


async def async_unload_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    return True


async def async_migrate_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Refuse migration → HA marks the entry ``migration_error``.

    Reached only on the `to` release, where config_flow.VERSION (2) exceeds the seeded
    entry's version (1); on `from` the versions match, so no migration runs and the
    entry stays ``loaded``.
    """
    _LOGGER.error(
        "briard_canary: refusing migration from v%s (deliberate test regression)",
        entry.version,
    )
    return False
