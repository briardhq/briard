"""Config flow for briard_canary — a config-entry VERSION that depends on the running HA.

The whole regression mechanism lives in this one conditional:

- On the `from` release (< 2025.12): ``VERSION = 1`` — matches the seeded entry, so no
  migration runs and ``async_setup_entry`` loads it → state ``loaded``.
- On the `to` release (>= 2025.12): ``VERSION = 2`` — exceeds the seeded entry (still
  v1, since `from` never migrated it), so HA calls ``async_migrate_entry``, which refuses
  → state ``migration_error``.

Same bytes on both sides; only ``homeassistant.const.__version__`` differs. That is a real
regressed migration through HA's real state machine, authored deterministically.
"""

from awesomeversion import AwesomeVersion

from homeassistant.config_entries import ConfigFlow
from homeassistant.const import __version__ as HA_VERSION

DOMAIN = "briard_canary"
_REGRESS_AT = AwesomeVersion("2025.12.0")


class BriardCanaryConfigFlow(ConfigFlow, domain=DOMAIN):
    """Minimal flow: it only needs a version and an import step to seed the entry."""

    VERSION = 2 if AwesomeVersion(HA_VERSION) >= _REGRESS_AT else 1

    async def async_step_import(self, import_data: dict | None = None):
        await self.async_set_unique_id(DOMAIN)
        self._abort_if_unique_id_configured()
        return self.async_create_entry(title="Briard Canary", data={})

    async def async_step_user(self, user_input: dict | None = None):
        return await self.async_step_import(user_input)
