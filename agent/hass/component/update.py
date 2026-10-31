"""briard's update platform — the stub's second file, as inert as the first.

Home Assistant imports an integration's platform only from the integration's own package, so
this file is in the stub for that alone: it hands the platform to the implementation that
`__init__` already imported, and does nothing when there is none. Only the implementation asks
Home Assistant for this platform, so on a Home Assistant without briard — a backup restored
somewhere else — this file is never imported at all. Like `__init__`, it is a permanent ABI: it
may meet an implementation many releases newer than itself, and they agree on a function name.
"""

import sys

from . import IMPL_MODULE


async def async_setup_platform(hass, config, async_add_entities, discovery_info=None):
    """Hand the platform to the implementation, if it is loaded and has one."""
    impl = sys.modules.get(IMPL_MODULE)
    setup = getattr(impl, "async_setup_update_platform", None)
    if setup is not None:
        await setup(hass, async_add_entities)
