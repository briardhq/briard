"""briard's Home Assistant integration — the STUB, and the only half that lives in /config.

Home Assistant scans `custom_components` at boot and only then, so anything briard runs inside
HA has to have a directory there before HA starts. What briard puts there is this stub and
nothing else: a package that imports the implementation from OUTSIDE /config and delegates to
it. The implementation never enters /config, so it never enters a backup either — a backup
cannot carry briard code that later runs somewhere it should not.

THIS FILE IS A PERMANENT ABI, and it is the one artifact that outlives the briard that wrote it.
A household's backup carries it, and restoring that backup — onto a much newer briard, or onto a
Home Assistant with no briard at all — brings this exact file back. So it does exactly one
thing, delegate `async_setup`, and it has to keep doing only that.

WHY A PACKAGE AND NOT A SYMLINK INTO THE MOUNT. A symlink is what the sketch decided, and it has
a hole: a household that takes its backup outside briard restores a DANGLING one, and HA's scan
is `[entry for … in path.iterdir() if entry.is_dir()]` — a broken link answers False, the domain
never resolves, and the `briard:` line in configuration.yaml turns into a permanent red
"Integration 'briard' not found". A real package answers the scan whatever is mounted, and when
the implementation is absent it says so once, in words the household can act on, and lets Home
Assistant carry on.
"""

import importlib
import logging
import sys

_LOGGER = logging.getLogger(__name__)

DOMAIN = "briard"

# Where briard mounts the implementation, read-only, from the node's tmpfs. Part of the ABI
# above: this stub may meet an implementation many releases newer than itself, so the two agree
# on a PATH and a MODULE NAME and on nothing else.
IMPL_PATH = "/briard/integration"
IMPL_MODULE = "briard_ha"


async def async_setup(hass, config):
    """Hand over to the implementation, or say plainly that there is none."""
    # APPENDED, never prepended: on a collision Home Assistant's own modules must win. The
    # directory holds one distinctively-named module, which is what keeps this bounded.
    if IMPL_PATH not in sys.path:
        sys.path.append(IMPL_PATH)
    try:
        impl = importlib.import_module(IMPL_MODULE)
    except ImportError:
        _LOGGER.warning(
            "briard is not installed on this Home Assistant, so the `%s:` line in "
            "configuration.yaml does nothing — comment it out to silence this",
            DOMAIN,
        )
        return True
    return await impl.async_setup(hass, config)
