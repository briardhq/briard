"""What an hourly clock sample costs Home Assistant ([B.167a]).

Seeds the recorder with synthetic history, keeps writing at each rate given, takes samples through
the product's own path (`briard-guest-agent --clock`), and meanwhile times a probe automation.
Prints one JSON verdict on stdout; everything else goes to stderr.

The pause a household would notice is the recorder lock: getting it (a truncating WAL checkpoint
behind the recorder's queue) and holding it across the snapshot. The product reports both
(acquire_ms, hold_ms). The space an hourly member pins is extrapolated from the churn between two
samples here, which are seconds apart rather than an hour.
"""

import argparse
import os
import http.client
import json
import re
import statistics
import subprocess
import sys
import threading
import time

HOST, PORT = "127.0.0.1", 8123
TOKEN_FILE = "/run/briard/home-assistant/token"
RING = "/var/lib/briard/.snapshots/"
DB = "/var/lib/briard/home-assistant/app/home-assistant_v2.db"


def log(*a):
    print(*a, file=sys.stderr, flush=True)


class HA:
    """One keep-alive connection, re-authenticating when the 30-minute access token expires."""

    def __init__(self):
        self.refresh = open(TOKEN_FILE).read().strip()
        self.conn = None
        self.access = None

    def _connect(self):
        self.conn = http.client.HTTPConnection(HOST, PORT, timeout=30)

    def _exchange(self):
        c = http.client.HTTPConnection(HOST, PORT, timeout=30)
        body = f"grant_type=refresh_token&refresh_token={self.refresh}"
        c.request("POST", "/auth/token", body, {"Content-Type": "application/x-www-form-urlencoded"})
        r = c.getresponse()
        self.access = json.loads(r.read())["access_token"]
        c.close()

    def call(self, method, path, body=None):
        for _ in range(3):
            if self.access is None:
                self._exchange()
            if self.conn is None:
                self._connect()
            headers = {"Authorization": f"Bearer {self.access}", "Content-Type": "application/json"}
            try:
                self.conn.request(method, path, json.dumps(body) if body is not None else None, headers)
                r = self.conn.getresponse()
                data = r.read()
            except (OSError, http.client.HTTPException):
                self.conn = None
                continue
            if r.status == 401:
                self.access = None
                continue
            if r.status >= 300:
                raise RuntimeError(f"{method} {path}: {r.status} {data[:200]!r}")
            return json.loads(data) if data else None
        raise RuntimeError(f"{method} {path}: gave up")

    def set_state(self, entity, state):
        return self.call("POST", f"/api/states/{entity}", {"state": str(state)})

    def state(self, entity):
        return self.call("GET", f"/api/states/{entity}")["state"]


def seed(entities, states):
    ha, n, t0 = HA(), 0, time.time()
    for i in range(states):
        for e in range(entities):
            ha.set_state(f"sensor.cost_{e}", i)
            n += 1
        if i % 100 == 0:
            log(f"seed: {n} states, {n / (time.time() - t0):.0f}/s")
    return n


class Writer(threading.Thread):
    """Keeps writing states at `rate` per second across the seeded entities until stopped."""

    def __init__(self, entities, rate):
        super().__init__(daemon=True)
        self.entities, self.rate, self.stop, self.n = entities, rate, threading.Event(), 0

    def run(self):
        ha, t0 = HA(), time.time()
        while not self.stop.is_set():
            ha.set_state(f"sensor.cost_{self.n % self.entities}", time.time())
            self.n += 1
            lag = t0 + self.n / self.rate - time.time()
            if lag > 0:
                time.sleep(lag)

    def achieved(self, seconds):
        return self.n / seconds


class Prober(threading.Thread):
    """Changes sensor.probe and times until the probe automation has incremented counter.probe."""

    def __init__(self):
        super().__init__(daemon=True)
        self.stop, self.results = threading.Event(), []  # (start, seconds)

    def run(self):
        ha, i = HA(), 0
        while not self.stop.is_set():
            before = int(float(ha.state("counter.probe")))
            i += 1
            start = time.time()
            ha.set_state("sensor.probe", i)
            while int(float(ha.state("counter.probe"))) <= before:
                if time.time() - start > 30:
                    break
                time.sleep(0.01)
            self.results.append((start, time.time() - start))
            time.sleep(0.5)


def exclusive_bytes(member):
    out = subprocess.run(["btrfs", "filesystem", "du", "-s", "--raw", RING + member],
                         capture_output=True, text=True, check=True).stdout.strip().splitlines()
    return int(out[-1].split()[1])


def sample():
    t0 = time.time()
    out = subprocess.run(["briard-guest-agent", "--clock=home-assistant"],
                         capture_output=True, text=True, check=True)
    t1 = time.time()
    m = re.search(r"took (\S+) \(held=(\w+) acquire_ms=(\d+) hold_ms=(\d+)", out.stdout + out.stderr)
    if not m:
        raise RuntimeError(f"no timing in the sample's output: {out.stdout!r} {out.stderr!r}")
    return {"member": m.group(1), "held": m.group(2) == "true", "acquire_ms": int(m.group(3)),
            "hold_ms": int(m.group(4)), "start": t0, "end": t1}


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(p / 100 * (len(xs) - 1))))] if xs else None


def measure(entities, rate, samples, gap, limits):
    writer, prober = Writer(entities, rate), Prober()
    writer.start()
    prober.start()
    time.sleep(gap)  # the writer's first interval, before the first sample
    t0, runs, churn, prev, prev_at = time.time(), [], [], None, None
    for k in range(samples):
        if prev:
            churn.append(exclusive_bytes(prev) / (time.time() - prev_at))
        s = sample()
        runs.append(s)
        prev, prev_at = s["member"], s["end"]
        log(f"rate {rate}: sample {k + 1}/{samples} held={s['held']} "
            f"acquire={s['acquire_ms']}ms hold={s['hold_ms']}ms")
        time.sleep(gap)
    writer.stop.set()
    prober.stop.set()
    writer.join(60)
    prober.join(60)
    elapsed = time.time() - t0

    during = [d for (st, d) in prober.results if any(r["start"] <= st <= r["end"] for r in runs)]
    outside = [d for (st, d) in prober.results if not any(r["start"] - 1 <= st <= r["end"] + 1 for r in runs)]
    pause = [r["acquire_ms"] + r["hold_ms"] for r in runs]
    res = {
        "rate": rate,
        "achieved_rate": round(writer.achieved(elapsed + gap), 1),
        "samples": len(runs),
        "held_rate": sum(r["held"] for r in runs) / len(runs),
        "acquire_ms": {"p50": pct([r["acquire_ms"] for r in runs], 50), "p95": pct([r["acquire_ms"] for r in runs], 95),
                       "max": max(r["acquire_ms"] for r in runs)},
        "hold_ms": {"p50": pct([r["hold_ms"] for r in runs], 50), "p95": pct([r["hold_ms"] for r in runs], 95),
                    "max": max(r["hold_ms"] for r in runs)},
        "pause_ms_p95": pct(pause, 95),
        "probe_ms": {"during_n": len(during), "during_p95": round(1000 * pct(during, 95)) if during else None,
                     "outside_n": len(outside), "outside_p95": round(1000 * pct(outside, 95)) if outside else None},
        "mb_per_hour": round(statistics.median(churn) * 3600 / 1e6, 1) if churn else None,
    }
    failures = []
    if res["pause_ms_p95"] > limits["pause_ms"]:
        failures.append(f"p95 pause {res['pause_ms_p95']}ms > {limits['pause_ms']}ms")
    if res["held_rate"] < limits["held_rate"]:
        failures.append(f"held on {res['held_rate']:.0%} of samples < {limits['held_rate']:.0%}")
    if not during or not outside:
        failures.append("the probe did not land both inside and outside a sample; widen --gap")
    elif res["probe_ms"]["during_p95"] - res["probe_ms"]["outside_p95"] > limits["probe_ms"]:
        failures.append(f"automation p95 {res['probe_ms']['during_p95']}ms during a sample vs "
                        f"{res['probe_ms']['outside_p95']}ms outside > +{limits['probe_ms']}ms")
    res["failures"] = failures
    return res


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--entities", type=int, default=100)
    p.add_argument("--states", type=int, default=1000)
    p.add_argument("--rates", default="10,100")
    p.add_argument("--samples", type=int, default=15)
    p.add_argument("--gap", type=float, default=10)
    p.add_argument("--pause-ms", type=int, default=2000)
    p.add_argument("--held-rate", type=float, default=0.99)
    p.add_argument("--probe-ms", type=int, default=250)
    a = p.parse_args()
    limits = {"pause_ms": a.pause_ms, "held_rate": a.held_rate, "probe_ms": a.probe_ms}

    seeded = seed(a.entities, a.states)
    time.sleep(5)  # the recorder commits on its own interval
    verdict = {"seeded": seeded, "db_bytes": os.path.getsize(DB), "limits": limits, "rates": []}
    for rate in [int(r) for r in a.rates.split(",")]:
        verdict["rates"].append(measure(a.entities, rate, a.samples, a.gap, limits))
    verdict["pass"] = not any(r["failures"] for r in verdict["rates"])
    print(json.dumps(verdict))


if __name__ == "__main__":
    main()
