# Navidrome: the music server, signed in by the reverse proxy by header, trusting its address, on a
# lone node, with a plain machine on the household's LAN beside it.
#
# What this catches, none of which a unit test can see, each being a property of the real app or
# the real network rather than of our code:
#
#   1. Navidrome takes what briard tells it from the file agent/navidrome writes into its own
#      directory: the trusted source is the pod network's gateway alone, as a /32;
#   2. the user gets in with the dashboard session and nothing else: a stranger is sent to the
#      dashboard (its first-admin form included), and a trusted browser is signed in with no
#      dialog as the record's username, which Navidrome's own tool, reading an empty Navidrome
#      while it runs, leaves the first sign-in to make its admin;
#   3. the header counts only from the reverse proxy's address: from another pod, and from a LAN
#      host that routes into the pod network (which arrives under its own address, never the
#      gateway's), Navidrome refuses it and says from where;
#   4. the Subsonic API is open and signs nobody in: through the reverse proxy the user header is
#      taken off (at the pod, from the gateway, it would sign in), and Navidrome's own password,
#      as the dashboard's "On your phone" shows it (the generated one replaced by three words, read
#      back off Navidrome's database by the sqlite3 in its container, through its API from the guest),
#      is what a phone's client signs in with;
#   5. music written into the library from another mount namespace, as the drive writes it, is in
#      the library within seconds, without a full scan;
#   6. a rename inside Navidrome is followed: once the reverse proxy's answer has run out, its next
#      check reads the oldest admin off Navidrome's own data under its new name, and no second
#      account is made.
{ pkgs, guestModule, navidromeFixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    fixtures = [ navidromeFixture ];
    replicated = false;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
in
pkgs.testers.runNixOSTest {
  name = "navidrome";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      virtualisation.memorySize = 1024;
    };
  # A household machine on the LAN, with nothing of briard's. Named to sort after node1, whose
  # node number lib.nix ties to its address.
  nodes.pc =
    { ... }:
    {
      environment.systemPackages = [ pkgs.curl ];
    };

  testScript = ''
    ${h.fixtureHelpers}
    import hashlib, json, re, shlex, time, urllib.parse
    VIP = "192.168.1.100"
    ND = "briard-navidrome.local"

    start_all()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=600)
    name_the_flock(node1)
    node1.succeed("briard-test-storage --seed")
    node1.succeed("systemctl start briard-chain.target")
    node1.wait_until_succeeds(f"curl -fsS http://{VIP}/healthz", timeout=180)

    install_fixture(node1, app="navidrome")
    for unit in fixture_units(node1, app="navidrome"):
        node1.wait_for_unit(unit, timeout=300)
    table = json.loads(node1.succeed("cat /run/briard/routes.json"))
    route = next(a for a in table["apps"] if a["name"] == "navidrome")
    assert route.get("gateByAddress") is True and "gate" not in route and route.get("open") == ["/rest/"], f"Navidrome is not gated by address with its API open: {route}"
    pod_ip = route["address"]
    pod = pod_ip + ":" + route["routes"][0]["to"].rsplit(":", 1)[1]
    node1.wait_until_succeeds(f"curl -fsS http://{pod}/ping", timeout=300)

    # (1) The settings, from the file, trusting the gateway alone.
    pool = node1.succeed("cat /run/briard/pod.subnet").strip()
    conf = node1.succeed("cat /run/briard/navidrome/navidrome.toml")
    assert f'TrustedSources = "{pool}.1/32"' in conf, conf
    # Read whole: piped into an early-exiting grep, the writer's SIGPIPE fails the pipeline.
    nd_log = node1.succeed("podman logs briard-navidrome-app 2>&1")
    assert re.search(r"Loaded configuration.*file=/briard/navidrome.toml", nd_log) and "Insight Collector is DISABLED" in nd_log, "Navidrome did not take briard's settings"

    def through_proxy(path, cookie="", extra="", host=ND):
        """One request through the reverse proxy, following nothing: (status, headers, body)."""
        c = f"-H 'Cookie: {cookie}'" if cookie else ""
        out = node1.succeed(f"curl -sS -D /tmp/h -o /tmp/b -H 'Host: {host}' {c} {extra} {shlex.quote('http://' + VIP + path)}; cat /tmp/h; echo ===; cat /tmp/b")
        headers, _, body = out.partition("===\n")
        return int(headers.splitlines()[0].split()[1]), headers, body

    def header(headers, name):
        return next(l.split(":", 1)[1].strip() for l in headers.splitlines() if l.lower().startswith(name.lower() + ":"))

    def signed_in(page):
        """The user Navidrome's page says it is signed in as, or None: the app's config, embedded
        in the page as an escaped string, carries `auth` only for a signed-in request."""
        m = re.search(r'"auth":\{[^}]*"username":"([^"]*)"[^}]*\}', page.replace('\\"', '"'))
        return (m.group(1), '"isAdmin":true' in m.group(0)) if m else None

    def refused_from(ip):
        """Navidrome's own word that a header came from an address it does not trust (logged quoted,
        with the port: proxy-ip="10.12.99.3:57266")."""
        node1.wait_until_succeeds(f"podman logs briard-navidrome-app > /tmp/nd.log 2>&1; grep 'IP is not whitelisted' /tmp/nd.log | grep 'proxy-ip=\"{ip}:' > /dev/null", timeout=30)

    # (2) No session: the pages go to the dashboard, and the first-admin form is not reachable.
    code, hd, _ = through_proxy("/app/")
    assert code == 303 and header(hd, "Location").startswith("http://briard.local/open/navidrome?"), hd
    code, _, _ = through_proxy("/auth/createAdmin", extra="-X POST -H 'Content-Type: application/json' -d '{\"username\":\"x\",\"password\":\"y\"}'")
    assert code == 401, f"the first-admin form answered {code} without a session"

    # (3) A header from another pod: not signed in, and Navidrome says from where.
    image = node1.succeed("podman inspect briard-navidrome-app --format '{{.ImageName}}'").strip()
    probe = f"hostname -i; wget -qO- --header 'X-Forwarded-User: kostas' http://{pod}/app/"
    out = node1.succeed(f"podman run --rm --network briard --entrypoint sh {image} -c {shlex.quote(probe)}")
    other, _, page = out.partition("\n")
    other = other.split()[0]
    assert other.startswith(pool + ".") and other != pool + ".1", other
    assert signed_in(page) is None, "a header from another pod signed in"
    refused_from(re.escape(other))

    # (3) A LAN host routing into the pod network arrives under its own address.
    pc.succeed(f"ip route add {pool}.0/24 via {VIP}")
    pc_ip = pc.succeed("ip -4 -o addr show eth1 | awk '{print $4}' | cut -d/ -f1").strip()
    page = pc.succeed(f"curl -fsS --max-time 10 -H 'X-Forwarded-User: kostas' http://{pod}/app/")
    assert signed_in(page) is None, "a header from a LAN host signed in"
    refused_from(re.escape(pc_ip))

    # (2) The dashboard's session, as `briard open` leaves one, then Navidrome opened from it.
    issued = node1.succeed("date -u +%Y-%m-%dT%H:%M:%SZ").strip()
    hand = json.dumps({"code": "c0ffee" * 10 + "0123", "name": "Kostas", "username": "kostas", "language": "en", "issued": issued})
    node1.succeed("mkdir -p -m 0700 /run/briard/dashboard")
    node1.succeed(f"printf %s {shlex.quote(hand)} > /run/briard/dashboard/handoff.json && chmod 0600 /run/briard/dashboard/handoff.json")
    code, hd, _ = through_proxy("/?code=" + "c0ffee" * 10 + "0123", host="briard.local")
    session = header(hd, "Set-Cookie").split(";")[0]
    assert code == 303 and session.startswith("briard_session="), hd
    q = urllib.parse.urlencode({"host": ND, "to": "/app/"})
    code, hd, body = through_proxy(f"/open/navidrome?{q}", session, host="briard.local")
    assert code == 303, f"open = {code}: {body}"
    loc = urllib.parse.urlparse(header(hd, "Location"))
    code, hd, _ = through_proxy(loc.path + "?" + loc.query)
    app_cookie = header(hd, "Set-Cookie").split(";")[0]
    assert code == 303 and app_cookie == session, hd
    code, _, page = through_proxy("/app/", app_cookie)
    assert code == 200 and signed_in(page) == ("kostas", True), f"signed in as {signed_in(page)}, want kostas, the record's, an admin"

    # (4) The open path signs nobody in: the header the gateway's address would sign in with...
    ping = "/rest/ping.view?v=1.16.1&c=test&f=json"
    status = lambda body: json.loads(body)["subsonic-response"]["status"]
    assert status(node1.succeed(f"curl -fsS -H 'X-Forwarded-User: kostas' 'http://{pod}{ping}'")) == "ok", "the gateway's header did not sign in at the pod"
    # ...is taken off by the reverse proxy, with the dashboard's cookie, and nobody is signed in.
    code, _, body = through_proxy(ping, app_cookie, "-H 'X-Forwarded-User: kostas'")
    assert code == 200 and status(body) == "failed", f"the open path signed someone in: {code} {body}"

    # (4) The phone's password, as the dashboard's "On your phone" shows it: the one Navidrome
    # generated replaced by three words and read back off its database, the same at the next press.
    press = lambda: through_proxy("/phone/navidrome", session, "-X POST -H 'HX-Request: true'", host="briard.local")
    code, _, body = press()
    keys = re.findall(r'<p class="key">([^<]*)</p>', body)
    assert code == 200 and len(keys) == 3 and keys[:2] == ["http://" + ND, "kostas"], f"On your phone answered {code}: {body}"
    phone_pass = keys[2]
    assert re.fullmatch(r"[a-z]+(-[a-z]+){2,3}", phone_pass), f"the password is not three words: {phone_pass!r}"
    assert re.findall(r'<p class="key">([^<]*)</p>', press()[2]) == keys, "a second press showed something else"
    salt = "c19b2d"
    token = hashlib.md5((phone_pass + salt).encode()).hexdigest()
    for creds in [f"u=kostas&p={phone_pass}", f"u=kostas&t={token}&s={salt}"]:
        code, _, body = through_proxy(ping + "&" + creds)
        assert code == 200 and status(body) == "ok", f"{creds}: {code} {body}"
    code, _, body = through_proxy(ping + "&u=kostas&p=wrong")
    assert status(body) == "failed", body

    # (5) An album written from another mount namespace is in the library within seconds.
    music = "/var/lib/briard/files/navidrome/music"
    node1.succeed(f"podman run --rm -v {music}:/out --entrypoint sh {image} -c 'mkdir -p /out/Briard && ffmpeg -loglevel error -f lavfi -i sine=d=3 -metadata title=Hello -metadata artist=Briard -metadata album=Test /out/Briard/hello.mp3'")
    search = f"/rest/search3.view?v=1.16.1&c=test&f=json&u=kostas&p={phone_pass}&query=Hello"
    node1.wait_until_succeeds(f"curl -fsS -H 'Host: {ND}' 'http://{VIP}{search}' | grep -q '\"title\":\"Hello\"'", timeout=60)

    # (6) A rename inside Navidrome is followed once the reverse proxy's answer has run out (half
    # a minute), with the same cookie, and no account is made on the way.
    me = next(u for u in json.loads(through_proxy("/api/user", app_cookie)[2]) if u["userName"] == "kostas")
    put = json.dumps({"id": me["id"], "userName": "kc", "name": me["name"], "email": me["email"], "isAdmin": True})
    code, _, body = through_proxy(f"/api/user/{me['id']}", app_cookie, f"-X PUT -H 'Content-Type: application/json' -d {shlex.quote(put)}")
    assert code == 200, f"the rename answered {code}: {body}"
    time.sleep(31)
    code, _, page = through_proxy("/app/", app_cookie)
    assert code == 200 and signed_in(page) == ("kc", True), f"signed in as {signed_in(page)} after the rename, want kc, an admin"
    names = sorted(u["userName"] for u in json.loads(through_proxy("/api/user", app_cookie)[2]))
    assert names == ["kc"], f"Navidrome's accounts after the rename: {names}"
  '';
}
