# Mealie: the recipe manager, signed in by the reverse proxy by header, on a lone node.
#
# WHAT THIS EXISTS TO CATCH, none of which a unit test can see, because each is a property of the
# real app rather than of our code:
#
#   1. MEALIE TAKES WHAT BRIARD TELLS IT FROM FILES: the settings agent/mealie writes into the
#      directory bound at /run/secrets are in effect -- the password form off, the header sign-in
#      on -- and Mealie's uid can read them;
#   2. THE FIRST ACCOUNT IS OPEN UNTIL THE FIRST OPEN, and closed after it: the password every
#      Mealie ships with works on its API before, and neither it nor anything else does after;
#   3. THE USER GETS IN WITH THE DASHBOARD'S SESSION AND NOTHING ELSE: a stranger is sent to the
#      dashboard, a request at the pod without the reverse proxy's secret is refused, and a trusted
#      browser is signed in as Mealie's first account -- an admin, keeping its username, with the
#      record's name and placeholder email -- on the `.local` name and on the casa name, into the
#      same account;
#   4. THE EMAIL IS THE USER'S TO CHANGE: changed inside Mealie, the next sign-in lands on the same
#      account;
#   5. NEVER LOCKED IN: the same data, under a plain Mealie with none of briard's settings, gives
#      the user a working admin account with Mealie's own recovery script;
#   6. A MEALIE BACK AT ITS FIRST ACCOUNT IS TAKEN OVER AGAIN without an open from the dashboard:
#      a browser that kept its cookie is signed in, and the shipped password is shut again;
#   7. A RENAME INSIDE MEALIE IS FOLLOWED: the reverse proxy signs in whoever Mealie's accounts say;
#   8. BRIARD'S OWN ACCOUNT IS MADE AGAIN WHENEVER IT IS GONE, an admin by Mealie's own tool run in
#      its container -- the one thing here no fake can stand in for.
{ pkgs, guestModule, mealieFixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    fixtures = [ mealieFixture ];
    replicated = false;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  # The casa name's certificate, self-signed and long-lived (a cert minted in a derivation is
  # reused from the store for as long as its inputs do not change).
  casaCert = pkgs.runCommand "briard-test-casa-cert" { nativeBuildInputs = [ pkgs.openssl ]; } ''
    mkdir -p $out
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
      -keyout $out/key.pem -out $out/fullchain.pem -days 36500 -subj "/CN=brave-elf.briard.casa" \
      -addext "subjectAltName=DNS:brave-elf.briard.casa,DNS:*.brave-elf.briard.casa"
  '';
in
pkgs.testers.runNixOSTest {
  name = "mealie";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      # Mealie settles at about 220 MB, and its image is about 1 GB unpacked.
      virtualisation.memorySize = 1536;
      virtualisation.diskSize = 4096;
    };

  testScript = ''
    ${h.fixtureHelpers}
    import json, shlex, time, urllib.parse
    VIP = "192.168.1.100"
    CASA = "brave-elf.briard.casa"
    MEALIE_CASA = "mealie." + CASA
    MEALIE = "briard-mealie.local"

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service", timeout=600)
    name_the_flock(node1)
    node1.succeed("briard-test-storage --seed")
    node1.succeed("systemctl start briard-chain.target")
    node1.wait_until_succeeds(f"curl -fsS http://{VIP}/healthz", timeout=180)
    node1.succeed("mkdir -p /var/lib/briard/tls && cp ${casaCert}/fullchain.pem ${casaCert}/key.pem /var/lib/briard/tls/")

    install_fixture(node1, app="mealie")
    for unit in fixture_units(node1, app="mealie"):
        node1.wait_for_unit(unit, timeout=300)
    table = json.loads(node1.succeed("cat /run/briard/routes.json"))
    route = next(a for a in table["apps"] if a["name"] == "mealie")
    assert route.get("gate") == "/run/briard/mealie/settings/REVERSE_PROXY_AUTH_SECRET" and route.get("gateHeader") == "X-Mealie-Reverse-Proxy-Secret", f"Mealie is not gated by header: {route}"
    pod = route["address"] + ":" + route["routes"][0]["to"].rsplit(":", 1)[1]
    node1.wait_until_succeeds(f"curl -fsS http://{pod}/api/app/about", timeout=300)

    def at_pod(path, extra=""):
        """A request straight at the pod, around the reverse proxy: (status, body)."""
        out = node1.succeed(f"curl -sS -w '\\n%{{http_code}}' {extra} {shlex.quote('http://' + pod + path)}")
        body, _, code = out.rpartition("\n")
        return int(code), body

    def through_proxy(host, path, cookie="", extra="", https=False):
        """One browser request through the reverse proxy, following nothing: (status, headers, body)."""
        c = f"-H 'Cookie: {cookie}'" if cookie else ""
        if https:
            url = f"https://{host}{path}"
            how = f"--resolve {host}:443:{VIP} --cacert /var/lib/briard/tls/fullchain.pem"
        else:
            url = f"http://{VIP}{path}"
            how = f"-H 'Host: {host}'"
        out = node1.succeed(f"curl -sS -D /tmp/h -o /tmp/b {how} {c} {extra} {shlex.quote(url)}; cat /tmp/h; echo ===; cat /tmp/b")
        headers, _, body = out.partition("===\n")
        return int(headers.splitlines()[0].split()[1]), headers, body

    def header(headers, name):
        return next(l.split(":", 1)[1].strip() for l in headers.splitlines() if l.lower().startswith(name.lower() + ":"))

    def seed_login(user="admin", password="MyPassword"):
        return at_pod("/api/auth/token", f"-X POST -d username={user} -d password={password}")[0]

    # (1) THE SETTINGS, from the files, as Mealie reports them.
    about = json.loads(at_pod("/api/app/about")[1])
    assert about["allowPasswordLogin"] is False and about["enableReverseProxyAuth"] is True, about
    node1.succeed("test $(stat -c %u:%a /run/briard/mealie/settings/REVERSE_PROXY_AUTH_SECRET) = 911:400")

    # (2) BEFORE THE FIRST OPEN, the first account answers the shipped password -- the window the
    # takeover closes -- and Mealie says it is a first login.
    assert seed_login() == 200, "Mealie's first account did not answer its shipped password"
    assert json.loads(at_pod("/api/app/about/startup-info")[1])["isFirstLogin"] is True

    # (3) THE WAY IN. Around the reverse proxy, a header without the secret is refused.
    code, _ = at_pod("/api/auth/reverse-proxy", "-H 'X-Forwarded-User: admin'")
    assert code == 401, f"the pod signed in a header without the reverse proxy's secret: {code}"
    # Through the reverse proxy with no session: sent to the dashboard to open it.
    code, hd, _ = through_proxy(MEALIE, "/")
    assert code == 303 and header(hd, "Location").startswith("http://briard.local/open/mealie?"), hd

    # The dashboard's session, as `briard open` leaves one: the host's one-time code, redeemed.
    def handoff(code):
        issued = node1.succeed("date -u +%Y-%m-%dT%H:%M:%SZ").strip()
        h = json.dumps({"code": code, "name": "Kostas", "username": "kostas", "language": "en", "issued": issued})
        node1.succeed("mkdir -p -m 0700 /run/briard/dashboard")
        node1.succeed(f"printf %s {shlex.quote(h)} > /run/briard/dashboard/handoff.json && chmod 0600 /run/briard/dashboard/handoff.json")
    handoff("c0ffee" * 10 + "0123")
    code, hd, _ = through_proxy("briard.local", "/?code=" + "c0ffee" * 10 + "0123")
    session = header(hd, "Set-Cookie").split(";")[0]
    assert code == 303 and session.startswith("briard_session="), hd

    def sign_in(dash_host, app_host, cookie, https=False):
        """Open Mealie from the dashboard and sign in as its login page does: (user, token)."""
        q = urllib.parse.urlencode({"host": app_host, "to": "/"})
        code, hd, body = through_proxy(dash_host, f"/open/mealie?{q}", cookie, https=https)
        assert code == 303, f"open = {code}: {body}"
        loc = urllib.parse.urlparse(header(hd, "Location"))
        assert loc.netloc == app_host and "briard_hop=" in loc.query, hd
        code, hd, _ = through_proxy(app_host, loc.path + "?" + loc.query, https=https)
        app_cookie = header(hd, "Set-Cookie").split(";")[0]
        assert code == 303 and app_cookie == cookie, f"{hd}\nwant the dashboard's own token"
        code, _, body = through_proxy(app_host, "/api/auth/reverse-proxy", app_cookie, https=https)
        assert code == 200, f"the header sign-in answered {code}: {body}"
        tok = json.loads(body)["access_token"]
        code, _, body = through_proxy(app_host, "/api/users/self", app_cookie, f"-H 'Authorization: Bearer {tok}'", https=https)
        assert code == 200, body
        return json.loads(body), tok, app_cookie

    me, tok, app_cookie = sign_in("briard.local", MEALIE, session)
    assert (me["username"], me["fullName"], me["email"], me["admin"], me["authMethod"]) == ("admin", "Kostas", "kostas@briard.local", True, "Mealie"), me
    first_id = me["id"]

    # (2) AFTER IT: the shipped password opens nothing, under either name, and the first-login
    # wizard is over.
    assert seed_login() == 401, "the first account still answers the shipped password"
    assert json.loads(at_pod("/api/app/about/startup-info")[1])["isFirstLogin"] is False

    # (4) THE EMAIL IS A FIELD: changed inside Mealie, the next sign-in is the same account.
    me["email"] = "kostas@example.org"
    body = shlex.quote(json.dumps(me))
    code, _, out = through_proxy(MEALIE, f"/api/users/{first_id}", app_cookie, f"-X PUT -H 'Authorization: Bearer {tok}' -H 'Content-Type: application/json' -d {body}")
    assert code == 200, f"changing the email answered {code}: {out}"
    again, _, _ = sign_in("briard.local", MEALIE, session)
    assert again["id"] == first_id and again["email"] == "kostas@example.org", again

    # (3) ON THE CASA NAME: its own session on the dashboard's casa name, the same account.
    handoff("feed" * 16)
    code, hd, _ = through_proxy(CASA, "/?code=" + "feed" * 16, https=True)
    casa_session = header(hd, "Set-Cookie").split(";")[0]
    assert code == 303 and casa_session.startswith("briard_session="), hd
    casa_me, _, _ = sign_in(CASA, MEALIE_CASA, casa_session, https=True)
    assert casa_me["id"] == first_id and casa_me["username"] == "admin", casa_me

    # (5) NEVER LOCKED IN: briard's Mealie stopped, the same image on the same data with nothing of
    # briard's, and Mealie's own recovery script gives the account a password.
    image = node1.succeed("podman inspect briard-mealie-app --format '{{.ImageName}}'").strip()
    for unit in fixture_units(node1, app="mealie"):
        node1.succeed(f"systemctl stop {unit}")
    node1.succeed(f"podman run -d --name plain --network host -v /var/lib/briard/data/mealie/app:/app/data {image}")
    node1.wait_until_succeeds("curl -fsS http://127.0.0.1:9000/api/app/about", timeout=300)
    plain = json.loads(node1.succeed("curl -fsS http://127.0.0.1:9000/api/app/about"))
    assert plain["allowPasswordLogin"] is True and plain["enableReverseProxyAuth"] is False, plain
    node1.succeed("printf 'kostas@example.org\\nnew-password-1\\nnew-password-1\\n\\n' | podman exec -i plain /opt/mealie/bin/python -m mealie.scripts.change_password")
    out = node1.succeed("curl -fsS -X POST -d username=admin -d password=new-password-1 http://127.0.0.1:9000/api/auth/token")
    tok = json.loads(out)["access_token"]
    me = json.loads(node1.succeed(f"curl -fsS -H 'Authorization: Bearer {tok}' http://127.0.0.1:9000/api/users/self"))
    assert me["id"] == first_id and me["admin"] is True, me

    # (6) BACK AT THE FIRST ACCOUNT -- an undo to before the takeover, a reinstall without the data
    # -- and a browser that kept its cookie, never opening from the dashboard: the reverse proxy's
    # next check takes the account over again, so the user is signed in and the shipped password
    # is shut.
    node1.succeed("podman rm -f plain && find /var/lib/briard/data/mealie/app -mindepth 1 -delete")
    for unit in fixture_units(node1, app="mealie"):
        node1.succeed(f"systemctl start {unit}")
    node1.wait_until_succeeds(f"curl -fsS http://{pod}/api/app/about", timeout=300)
    assert json.loads(at_pod("/api/app/about/startup-info")[1])["isFirstLogin"] is True, "the emptied Mealie is not at its first account"
    # The reverse proxy may still hold its last answer, `admin`, for half a minute, and that name is
    # the new first account's too: a sign-in in that window lands on it before the takeover, which
    # the next check makes. So: signed in, and taken over, by requests the browser keeps making.
    deadline = time.monotonic() + 120
    while True:
        code, _, body = through_proxy(MEALIE, "/api/auth/reverse-proxy", app_cookie)
        if code == 200 and seed_login() == 401:
            break
        assert time.monotonic() < deadline, f"the emptied Mealie was not taken over under the kept cookie: {code} {body}"
        time.sleep(5)
    tok = json.loads(body)["access_token"]
    code, _, body = through_proxy(MEALIE, "/api/users/self", app_cookie, f"-H 'Authorization: Bearer {tok}'")
    me = json.loads(body)
    assert (me["username"], me["fullName"], me["admin"]) == ("admin", "Kostas", True) and me["id"] != first_id, me

    def accounts(tok):
        """Every Mealie account, as its admin page lists them, through the reverse proxy as the user."""
        code, _, body = through_proxy(MEALIE, "/api/admin/users?perPage=-1", app_cookie, f"-H 'Authorization: Bearer {tok}'")
        assert code == 200, f"listing Mealie's accounts answered {code}: {body}"
        return {u["username"]: u for u in json.loads(body)["items"]}

    def signed_in_as(want_id):
        """The kept cookie signed in, past the half minute the reverse proxy keeps an answer: the account."""
        time.sleep(31)
        deadline = time.monotonic() + 90
        while True:
            code, _, body = through_proxy(MEALIE, "/api/auth/reverse-proxy", app_cookie)
            if code == 200:
                t = json.loads(body)["access_token"]
                _, _, body = through_proxy(MEALIE, "/api/users/self", app_cookie, f"-H 'Authorization: Bearer {t}'")
                me = json.loads(body)
                if me["id"] == want_id:
                    return me, t
            assert time.monotonic() < deadline, f"the kept cookie is not signed in to {want_id}: {code} {body}"
            time.sleep(5)

    # (6) and nothing else was made on the way: the first account and briard's own.
    assert sorted(accounts(tok)) == ["admin", "briard-admin"], sorted(accounts(tok))

    # (7) A RENAME INSIDE MEALIE IS FOLLOWED: the reverse proxy's next check reads the user's
    # account off Mealie, so the kept cookie lands on the same account under its new name.
    me["username"] = "kchatzi"
    body = shlex.quote(json.dumps(me))
    code, _, out = through_proxy(MEALIE, f"/api/users/{me['id']}", app_cookie, f"-X PUT -H 'Authorization: Bearer {tok}' -H 'Content-Type: application/json' -d {body}")
    assert code == 200, f"the rename answered {code}: {out}"
    me, tok = signed_in_as(me["id"])
    assert me["username"] == "kchatzi" and me["admin"] is True, me

    # (8) BRIARD'S OWN ACCOUNT IS MADE AGAIN WHENEVER IT IS GONE: deleted by the user, the next check
    # signs in as it, which makes it an ordinary account, and Mealie's own admin tool, run in its
    # container, makes it an admin again -- and the user is signed in throughout.
    own = accounts(tok)["briard-admin"]
    code, _, out = through_proxy(MEALIE, f"/api/admin/users/{own['id']}", app_cookie, f"-X DELETE -H 'Authorization: Bearer {tok}'")
    assert code == 200, f"deleting briard's own account answered {code}: {out}"
    assert "briard-admin" not in accounts(tok)
    me, tok = signed_in_as(me["id"])
    again = accounts(tok)["briard-admin"]
    assert again["admin"] is True and again["id"] != own["id"], again
  '';
}
