"""The recorder-check rig's hands on Home Assistant and on its database.

  seed N                      N states across ten sensors, written through the REST API so the
                              recorder writes them as it would any state
  set ENTITY STATE            one state
  repack                      recorder.purge with repack: Home Assistant's full read (a VACUUM)
  ready                       the briard integration's quiesce view answers, which the clock
                              sample needs and which comes up after the API does
  row DB SNAP ENTITY STATE    the state_id of that state in DB, or "" (SNAP=1 opens a read-only
                              member with immutable=1)
  quick DB SNAP               PRAGMA quick_check, or "error: ..." when the open or read fails
  leaf DB                     a leaf page of `states` from the middle of its tree, and the page
                              size: far from the first row the startup check reads and the last
                              rows new states are written next to
"""

import http.client
import json
import sqlite3
import sys

HOST, PORT = "127.0.0.1", 8123
TOKEN_FILE = "/run/briard/home-assistant/token"


def access():
    refresh = open(TOKEN_FILE).read().strip()
    c = http.client.HTTPConnection(HOST, PORT, timeout=30)
    c.request("POST", "/auth/token", f"grant_type=refresh_token&refresh_token={refresh}",
              {"Content-Type": "application/x-www-form-urlencoded"})
    r = c.getresponse()
    body = r.read()
    if r.status != 200:
        sys.exit(f"token exchange: {r.status} {body[:200]!r}")
    return json.loads(body)["access_token"]


class HA:
    def __init__(self):
        self.tok = access()
        self.conn = http.client.HTTPConnection(HOST, PORT, timeout=60)

    def post(self, path, body):
        for attempt in range(3):
            try:
                self.conn.request("POST", path, json.dumps(body),
                                  {"Authorization": f"Bearer {self.tok}", "Content-Type": "application/json"})
                r = self.conn.getresponse()
                data = r.read()
                break
            except (OSError, http.client.HTTPException):
                # A dropped keep-alive: a new connection, and the request again.
                self.conn = http.client.HTTPConnection(HOST, PORT, timeout=60)
        else:
            sys.exit(f"POST {path}: no answer")
        if r.status >= 300:
            sys.exit(f"POST {path}: {r.status} {data[:200]!r}")


def db(path, snap):
    if snap == "1":
        return sqlite3.connect(f"file:{path}?mode=ro&immutable=1", uri=True)
    return sqlite3.connect(path)


def main():
    cmd, args = sys.argv[1], sys.argv[2:]
    if cmd == "seed":
        ha = HA()
        for i in range(int(args[0])):
            ha.post(f"/api/states/sensor.b31_{i % 10}", {"state": str(i)})
    elif cmd == "set":
        HA().post(f"/api/states/{args[0]}", {"state": args[1]})
    elif cmd == "repack":
        HA().post("/api/services/recorder/purge", {"keep_days": 365, "repack": True})
    elif cmd == "ready":
        HA().post("/api/briard/quiesce", {"hold": False})
    elif cmd == "row":
        r = db(args[0], args[1]).execute(
            "SELECT s.state_id FROM states s JOIN states_meta m ON s.metadata_id = m.metadata_id "
            "WHERE m.entity_id = ? AND s.state = ? ORDER BY s.state_id LIMIT 1", (args[2], args[3])).fetchone()
        print(r[0] if r else "")
    elif cmd == "quick":
        try:
            print("; ".join(r[0] for r in db(args[0], args[1]).execute("PRAGMA quick_check(5)")))
        except sqlite3.DatabaseError as e:
            print(f"error: {e}")
    elif cmd == "leaf":
        c = sqlite3.connect(args[0])
        pages = [r[0] for r in c.execute(
            "SELECT pageno FROM dbstat WHERE name = 'states' AND pagetype = 'leaf' ORDER BY path")]
        if len(pages) < 10:
            sys.exit(f"`states` has only {len(pages)} leaf pages; seed more history")
        print(pages[len(pages) // 2], c.execute("PRAGMA page_size").fetchone()[0])
    else:
        sys.exit(f"unknown command {cmd}")


main()
