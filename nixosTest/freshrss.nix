# FreshRSS: the feed reader, signed in by the reverse proxy by header, trusting its address, on a
# lone node, with a plain machine on the household's LAN beside it.
#
# What this catches, none of which a unit test can see, each being a property of the real app or
# the real network rather than of our code:
#
#   1. FreshRSS takes what briard tells it from the file agent/freshrss writes into its own
#      directory and binds over the image's p/i/.htaccess: the trusted address is the pod network's
#      gateway alone, as a /32, and the user comes from the reverse proxy's X-Forwarded-User;
#   2. the user gets in with the dashboard session and nothing else: a stranger is sent to the
#      dashboard, and a trusted browser is signed in with no dialog as `owner`, the default user
#      its installer made, read by FreshRSS's own tool, with an admin's pages open to it;
#   3. the header counts only from the reverse proxy's address: from another pod, and from a LAN
#      host that routes into the pod network, neither X-Forwarded-User nor FreshRSS's own
#      Remote-User signs in -- the one FreshRSS release that stopped reading the file's
#      TRUSTED_PROXY would fail here, not in production;
#   4. a signed-in browser cannot name someone else: its own Remote-User is replaced by the
#      reverse proxy's user, and no account is made for the name it sent;
#   5. the Google Reader API is open, and only it: the API password "On your phone" sets (three
#      words, kept beside the user and shown again at the next press) signs a phone's reader in
#      through the reverse proxy, a wrong one does not, and the Fever API stays behind the gate.
#   6. the QR code "On your phone" shows, read as a phone's camera reads it, opens the copy page
#      through the reverse proxy with no session: the same values, never cached, no referrer.
{ pkgs, guestModule, freshrssFixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    fixtures = [ freshrssFixture ];
    replicated = false;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
in
pkgs.testers.runNixOSTest {
  name = "freshrss";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      virtualisation.memorySize = 1024;
      # zbarimg reads the dialog's QR code, as a phone's camera would.
      environment.systemPackages = [ pkgs.zbar ];
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
    import html, json, re, shlex, urllib.parse
    VIP = "192.168.1.100"
    FR = "briard-freshrss.local"
    CTR = "briard-freshrss-app"

    start_all()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=600)
    name_the_flock(node1)
    node1.succeed("briard-test-storage --seed")
    node1.succeed("systemctl start briard-chain.target")
    node1.wait_until_succeeds(f"curl -fsS http://{VIP}/healthz", timeout=180)

    install_fixture(node1, app="freshrss")
    for unit in fixture_units(node1, app="freshrss"):
        node1.wait_for_unit(unit, timeout=300)
    table = json.loads(node1.succeed("cat /run/briard/routes.json"))
    route = next(a for a in table["apps"] if a["name"] == "freshrss")
    assert route.get("gateByAddress") is True and "gate" not in route and route.get("open") == ["/api/greader.php/"], f"FreshRSS is not gated by address with its API open: {route}"
    pod_ip = route["address"]
    pod = pod_ip + ":" + route["routes"][0]["to"].rsplit(":", 1)[1]
    node1.wait_until_succeeds(f"curl -fsS http://{pod}/api/", timeout=300)

    # (1) The file, trusting the gateway alone, and the one FreshRSS's Apache reads.
    pool = node1.succeed("cat /run/briard/pod.subnet").strip()
    conf = node1.succeed("cat /run/briard/freshrss/freshrss.conf")
    assert f"SetEnv TRUSTED_PROXY {pool}.1/32\n" in conf, conf
    assert node1.succeed(f"podman exec {CTR} cat /var/www/FreshRSS/p/i/.htaccess") == conf, "FreshRSS's hook is not briard's file"

    def through_proxy(path, cookie="", extra="", host=FR):
        """One request through the reverse proxy, following nothing: (status, headers, body)."""
        c = f"-H 'Cookie: {cookie}'" if cookie else ""
        out = node1.succeed(f"curl -sS -D /tmp/h -o /tmp/b -H 'Host: {host}' {c} {extra} {shlex.quote('http://' + VIP + path)}; cat /tmp/h; echo ===; cat /tmp/b")
        headers, _, body = out.partition("===\n")
        return int(headers.splitlines()[0].split()[1]), headers, body

    def header(headers, name):
        return next(l.split(":", 1)[1].strip() for l in headers.splitlines() if l.lower().startswith(name.lower() + ":"))

    def signed_in(page):
        """The user FreshRSS's page says it is signed in as, or None: its menu names the account
        only for a signed-in request."""
        m = re.search(r'<span class="signout">\(([^)]*)\)</span>', page)
        return m.group(1) if m else None

    def users():
        return sorted(u["user"] for u in json.loads(node1.succeed(f"podman exec --user apache {CTR} /var/www/FreshRSS/cli/user-info.php --json")))

    # The positive control for (3): at the pod, from the gateway, the reverse proxy's header signs in.
    assert signed_in(node1.succeed(f"curl -fsS -H 'X-Forwarded-User: owner' http://{pod}/i/")) == "owner", "the gateway's header did not sign in at the pod"

    # (2) No session: the pages go to the dashboard, and so does the Fever API.
    for path in ["/i/", "/api/fever.php?api"]:
        code, hd, _ = through_proxy(path)
        assert code == 303 and header(hd, "Location").startswith("http://briard.local/open/freshrss?"), f"{path}: {hd}"

    # (3) A header from another pod, either one: not signed in.
    image = node1.succeed(f"podman inspect {CTR} --format '{{{{.ImageName}}}}'").strip()
    for h in ["X-Forwarded-User", "Remote-User"]:
        probe = f"hostname -i; wget -qO- --header '{h}: owner' http://{pod}/i/ || true"
        out = node1.succeed(f"podman run --rm --network briard --entrypoint sh {image} -c {shlex.quote(probe)}")
        other, _, page = out.partition("\n")
        other = other.split()[0]
        assert other.startswith(pool + ".") and other != pool + ".1", other
        assert signed_in(page) is None, f"{h} from another pod signed in"

    # (3) A LAN host routing into the pod network arrives under its own address.
    pc.succeed(f"ip route add {pool}.0/24 via {VIP}")
    for h in ["X-Forwarded-User", "Remote-User"]:
        page = pc.succeed(f"curl -sS --max-time 10 -H '{h}: owner' http://{pod}/i/")
        assert signed_in(page) is None, f"{h} from a LAN host signed in"

    # (2) The dashboard's session, as `briard open` leaves one, then FreshRSS opened from it.
    issued = node1.succeed("date -u +%Y-%m-%dT%H:%M:%SZ").strip()
    hand = json.dumps({"code": "c0ffee" * 10 + "0123", "name": "Kostas", "username": "kostas", "language": "en", "issued": issued})
    node1.succeed("mkdir -p -m 0700 /run/briard/dashboard")
    node1.succeed(f"printf %s {shlex.quote(hand)} > /run/briard/dashboard/handoff.json && chmod 0600 /run/briard/dashboard/handoff.json")
    code, hd, _ = through_proxy("/?code=" + "c0ffee" * 10 + "0123", host="briard.local")
    session = header(hd, "Set-Cookie").split(";")[0]
    assert code == 303 and session.startswith("briard_session="), hd
    q = urllib.parse.urlencode({"host": FR, "to": "/i/"})
    code, hd, body = through_proxy(f"/open/freshrss?{q}", session, host="briard.local")
    assert code == 303, f"open = {code}: {body}"
    loc = urllib.parse.urlparse(header(hd, "Location"))
    code, hd, _ = through_proxy(loc.path + "?" + loc.query)
    app_cookie = header(hd, "Set-Cookie").split(";")[0]
    assert code == 303 and app_cookie == session, hd
    code, _, page = through_proxy("/i/", app_cookie)
    assert code == 200 and signed_in(page) == "owner", f"signed in as {signed_in(page)}, want owner, the installer's"
    code, _, page = through_proxy("/i/?c=user&a=manage", app_cookie)
    assert code == 200 and "Error 403" not in page, f"owner is not an admin: {code}"

    # (4) A signed-in browser's own Remote-User is replaced, and makes no account.
    code, _, page = through_proxy("/i/", app_cookie, "-H 'Remote-User: stranger' -H 'X-WebAuth-User: stranger'")
    assert code == 200 and signed_in(page) == "owner", f"a browser's own header signed in as {signed_in(page)}"
    assert users() == ["owner"], f"FreshRSS's users: {users()}"

    # (5) The phone's password, as "On your phone" shows it: three words set at the first press,
    # kept, and the same at the next.
    press = lambda: through_proxy("/phone/freshrss", session, "-X POST -H 'HX-Request: true'", host="briard.local")
    code, _, body = press()
    keys = re.findall(r'<p class="key"[^>]*>([^<]*)</p>', body)
    assert code == 200 and len(keys) == 3 and keys[:2] == ["http://" + FR + "/api/greader.php", "owner"], f"On your phone answered {code}: {body}"
    phone_pass = keys[2]
    assert re.fullmatch(r"[a-z]+ [a-z]+ [a-z]+", phone_pass), f"the password is not three words: {phone_pass!r}"
    again = press()[2]
    assert re.findall(r'<p class="key"[^>]*>([^<]*)</p>', again) == keys, "a second press showed something else"
    kept = node1.succeed(f"podman exec --user apache {CTR} cat /var/www/FreshRSS/data/users/owner/briard-phone")
    assert kept == phone_pass, f"kept {kept!r}, shown {phone_pass!r}"

    # (6) The dialog's QR code, read as a phone's camera reads it, opens the copy page through
    # the reverse proxy with no session: the same three values, never cached, no referrer.
    m = re.search(r'<img class="qr" src="data:image/png;base64,([^"]+)"', again)
    assert m, f"the dialog has no QR code: {again}"
    qr = html.unescape(m.group(1))
    node1.succeed(f"echo {qr} | base64 -d > /tmp/qr.png")
    url = node1.succeed("zbarimg -q --raw /tmp/qr.png").strip()
    assert url.startswith("http://briard.local/copy/"), f"the QR code reads {url!r}"
    code, hd, page = through_proxy(url[len("http://briard.local"):], host="briard.local")
    assert code == 200 and re.findall(r'<p class="key"[^>]*>([^<]*)</p>', page) == keys, f"the copy page answered {code}: {page}"
    assert header(hd, "Cache-Control") == "no-store" and header(hd, "Referrer-Policy") == "no-referrer", hd

    # (5) The Google Reader API, through the reverse proxy, as a phone's reader signs in.
    login = lambda pw: through_proxy("/api/greader.php/accounts/ClientLogin", extra="-X POST --data-urlencode Email=owner --data-urlencode " + shlex.quote("Passwd=" + pw))
    code, _, body = login(phone_pass)
    assert code == 200 and "Auth=owner/" in body, f"the kept words did not sign in: {code} {body}"
    code, _, _ = login("wrong words here")
    assert code == 401, f"a wrong password answered {code}"
  '';
}
