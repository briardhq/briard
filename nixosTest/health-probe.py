"""What each candidate health signal says when Home Assistant is broken in a given way ([B.167b]).

A discovery probe, not a gate: for each fault it restores a known-good copy of the data, injects
the fault, restarts Home Assistant the way converge does, and samples every candidate signal for a
window. Prints one JSON document on stdout (the matrix); progress goes to stderr.
"""

import http.client
import json
import subprocess
import sys
import time

HOST, PORT = "127.0.0.1", 8123
TOKEN_FILE = "/run/briard/home-assistant/token"
ROOT = "/var/lib/briard/home-assistant"
CONFIG = ROOT + "/app"
PRISTINE = "/var/lib/briard/.probe-pristine"
UNITS = open("/run/briard/fixture/units").read().split()
APP = UNITS[-1]
WINDOW, EVERY = 180, 5


def log(*a):
    print(*a, file=sys.stderr, flush=True)


def sh(*cmd, check=True):
    return subprocess.run(cmd, capture_output=True, text=True, check=check).stdout


def write(rel, text):
    path = f"{CONFIG}/{rel}"
    sh("mkdir", "-p", path.rsplit("/", 1)[0])
    with open(path, "w") as f:
        f.write(text)


def append(rel, text):
    with open(f"{CONFIG}/{rel}", "a") as f:
        f.write(text)


def custom(domain, init):
    """A custom integration switched on from YAML, so its setup runs at every start."""
    manifest = {"domain": domain, "name": domain, "version": "1.0.0", "documentation": "",
                "requirements": [], "codeowners": [], "iot_class": "local_polling"}
    write(f"custom_components/{domain}/manifest.json", json.dumps(manifest))
    write(f"custom_components/{domain}/__init__.py", init)
    append("configuration.yaml", f"\n{domain}:\n")


def garble(rel):
    write(rel, '{"version": 1, "data": {"entries": [')


FAULTS = {
    "baseline": lambda: None,
    "yaml-syntax": lambda: append("configuration.yaml", "\nbroken: [unclosed\n"),
    "yaml-schema": lambda: append("configuration.yaml", "\ncounter:\n  bad:\n    initial: not-a-number\n"),
    "automations": lambda: write("automations.yaml", "- id: x\n  triggers:\n    - trigger: no_such_platform\n  actions: []\n"),
    "storage-config-entries": lambda: garble(".storage/core.config_entries"),
    "storage-entity-registry": lambda: garble(".storage/core.entity_registry"),
    "storage-auth": lambda: garble(".storage/auth"),
    "custom-import-error": lambda: custom("probe_import", "raise ImportError('probe: broken on purpose')\n"),
    "custom-setup-false": lambda: custom("probe_setup", "async def async_setup(hass, config):\n    return False\n"),
    "custom-crash": lambda: custom("probe_crash", "import os\nos._exit(3)\n"),
    "custom-hang": lambda: custom("probe_hang",
                                  "import time\n\nasync def async_setup(hass, config):\n"
                                  "    time.sleep(3600)\n    return True\n"),
    "recorder-db": lambda: sh("dd", "if=/dev/urandom", f"of={CONFIG}/home-assistant_v2.db",
                              "bs=4096", "count=4", "conv=notrunc"),
}


def get(path, token=None, timeout=5):
    """(status, body, seconds) -- status is 0 when nothing answered in time."""
    t0 = time.time()
    try:
        c = http.client.HTTPConnection(HOST, PORT, timeout=timeout)
        headers = {"Authorization": f"Bearer {token}"} if token else {}
        c.request("GET", path, headers=headers)
        r = c.getresponse()
        body = r.read()
        c.close()
        return r.status, body, time.time() - t0
    except (OSError, http.client.HTTPException):
        return 0, b"", time.time() - t0


def access():
    try:
        c = http.client.HTTPConnection(HOST, PORT, timeout=5)
        refresh = open(TOKEN_FILE).read().strip()
        c.request("POST", "/auth/token", f"grant_type=refresh_token&refresh_token={refresh}",
                  {"Content-Type": "application/x-www-form-urlencoded"})
        r = c.getresponse()
        body = r.read()
        return json.loads(body)["access_token"] if r.status == 200 else None
    except (OSError, http.client.HTTPException, ValueError, KeyError):
        return None


def unit(prop):
    return sh("systemctl", "show", "-p", prop, "--value", APP, check=False).strip()


def observe():
    s = {"t": None}
    st, _, lat = get("/manifest.json")
    s["door"], s["door_ms"] = st, round(lat * 1000)
    tok = access()
    s["auth"] = tok is not None
    if tok:
        st, body, _ = get("/api/config", tok)
        if st == 200:
            cfg = json.loads(body)
            s["state"], s["recovery"], s["safe"] = cfg.get("state"), cfg.get("recovery_mode"), cfg.get("safe_mode")
        st, body, _ = get("/api/config/config_entries/entry", tok)
        if st == 200:
            counts = {}
            for e in json.loads(body):
                counts[e.get("state")] = counts.get(e.get("state"), 0) + 1
            s["entries"] = counts
        st, body, _ = get("/api/error_log", tok)
        if st == 200:
            s["errors"] = sum(1 for line in body.decode(errors="replace").splitlines() if " ERROR " in line)
    s["unit"] = unit("ActiveState")
    s["restarts"] = int(unit("NRestarts") or 0)
    return s


def restart_onto(fault):
    """Stop, restore the pristine data, inject, start in the renderer's order -- converge by hand
    (hass-upgrade-rollback records why never `systemctl restart`)."""
    sh("systemctl", "stop", APP, check=False)
    sh("btrfs", "subvolume", "delete", ROOT)
    sh("btrfs", "subvolume", "snapshot", PRISTINE, ROOT)
    FAULTS[fault]()
    sh("sync")
    sh("systemctl", "reset-failed", APP, check=False)
    for u in UNITS:
        sh("systemctl", "start", "--no-block", u, check=False)


def main():
    only = sys.argv[1:] or list(FAULTS)
    matrix = {}
    for fault in only:
        log(f"== {fault}")
        restart_onto(fault)
        t0, samples = time.time(), []
        while time.time() - t0 < WINDOW:
            s = observe()
            s["t"] = round(time.time() - t0)
            samples.append(s)
            log(json.dumps(s))
            time.sleep(EVERY)
        up = [s["t"] for s in samples if s["door"] == 200]
        tail = [s for s in samples if s["t"] >= WINDOW - 60]
        matrix[fault] = {
            "door_first_200_s": up[0] if up else None,
            "door_200_share_last_60s": round(sum(s["door"] == 200 for s in tail) / len(tail), 2),
            "restarts": samples[-1]["restarts"],
            "last": samples[-1],
        }
    print(json.dumps(matrix))


if __name__ == "__main__":
    main()
