"""Plant briard's integration into Home Assistant's /config.

Run one-shot, on the image's own python, in a STOPPED window — the wrapper runs it from s6's
`run` script, before Home Assistant is launched. It has to be that window and no other: HA scans
`custom_components` at boot and reads configuration.yaml once, so a directory or a line that
appears later is a directory or a line HA will not see until something restarts it.

It plants THREE things, all of them repeatedly, because every one of them is wiped by a config
restore and re-planting is how they come back:

  * Home Assistant's own default configuration.yaml, if there is none yet. Not authored —
    RELAYED: `async_ensure_config_exists` is the very function HA's own startup calls a moment
    later, so what lands is HA's default and everything that goes with it (secrets.yaml,
    automations.yaml, .HA_VERSION). Writing our own would be the mistake the `run` script and the
    mint each found independently: a copy is correct on the version you tested it against and
    silently wrong on the next.
  * The `briard:` line, which is what makes HA load the integration at all. ⚠️ Said out loud
    rather than discovered: it is re-planted at EVERY start, so a household commenting it out
    inside briard does not make it stick. That is the one place briard overrides an edit to a
    file the household owns, and it is deliberate — this is plumbing, not a household decision,
    and removing briard from an HA it manages is a real operation with its own verb.
  * The stub package. The implementation stays outside /config; only the inert stub is planted,
    so a backup can never carry briard code that later runs where it should not.

WHY THE DEFAULT CONFIG HAS TO BE ENSURED HERE AND NOT LEFT TO HA. On a node's very first boot
/config is empty: HA creates configuration.yaml during its own startup and reads it in the same
pass, so a line appended before that start cannot be in the file it reads, and the integration
would first load one restart later — which on a fresh install is the difference between "the
broker is wired" and "the broker is wired whenever somebody happens to restart". Ensuring it
here closes the gap with HA's own code.

Failure is the caller's to swallow (`|| true` in the wrapper): a household losing Home Assistant
because briard could not plant a file would be a far worse trade than an integration that is not
there.
"""

import os
import shutil
import sys

DOMAIN = "briard"
YAML = "configuration.yaml"


def ensure_default_config(config_dir):
    """Relay HA's own default-config creation, which is a no-op once the file exists."""
    if os.path.isfile(os.path.join(config_dir, YAML)):
        return
    # Imported LAZILY, and that is worth the two lines: importing homeassistant.config costs
    # seconds at every single Home Assistant start, and this branch is taken once in a node's
    # life. Everything else in this file is stdlib and starts in milliseconds.
    import asyncio

    from homeassistant.config import async_ensure_config_exists
    from homeassistant.core import HomeAssistant

    async def run():
        # The same construction `hass --script auth` uses: a HomeAssistant that is never started,
        # for the sake of the one thing that needs `hass.config.config_dir`.
        hass = HomeAssistant(config_dir)
        if not await async_ensure_config_exists(hass):
            raise SystemExit(f"{config_dir}: Home Assistant declined to write its default config")

    asyncio.run(run())


def ensure_yaml_key(config_dir):
    """Ensure the `briard:` line exists — the whole of how the integration is switched on.

    YAML AND NOT A CONFIG ENTRY, and that is the point: a config entry would be UI-visible and
    deletable, while a YAML-only integration with no `config_flow` has no card and no disable
    switch. That is right for plumbing and wrong for a household's decisions, which is exactly
    the line the announce-don't-inject policy draws.
    """
    path = os.path.join(config_dir, YAML)
    if not os.path.isfile(path):
        return
    with open(path, encoding="utf-8") as fh:
        body = fh.read()
    # ANY top-level line that OPENS the key, not just the bare one we write. A household that
    # gave it a value (`briard: {}`) still owns the key, and appending a second one would leave
    # configuration.yaml with a duplicate top-level key -- a config error in a file we do not own,
    # caused by briard, at every start.
    for line in body.splitlines():
        if line.startswith(DOMAIN + ":"):
            return
    # A leading newline because the file may end mid-line, and a top-level key that got appended
    # onto the end of somebody else's line is a config error rather than a config.
    with open(path, "a", encoding="utf-8") as fh:
        fh.write("\n# Added by briard. Removing this line does not uninstall briard.\n")
        fh.write(DOMAIN + ":\n")


def plant_stub(config_dir, src):
    """Copy the stub package into custom_components, overwriting whatever is there.

    UNCONDITIONALLY, because the stub is the one artifact a restore can bring back from an older
    briard: overwriting is what makes that drift heal itself instead of accumulating.
    """
    dst = os.path.join(config_dir, "custom_components", DOMAIN)
    os.makedirs(dst, exist_ok=True)
    for name in sorted(os.listdir(src)):
        if not os.path.isfile(os.path.join(src, name)):
            continue
        shutil.copyfile(os.path.join(src, name), os.path.join(dst, name))


def main():
    config_dir, stub = sys.argv[1], sys.argv[2]
    ensure_default_config(config_dir)
    ensure_yaml_key(config_dir)
    plant_stub(config_dir, stub)


main()
