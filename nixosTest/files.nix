# Files: the household's window onto every app's directories, as a catalogued app.
#
# WHAT THIS EXISTS TO CATCH, none of which a unit test can see, because each is a property of the
# kernel, podman or the real app rather than of our code:
#
#   1. THE VIEW REACHES THE RUNNING CONTAINER: the guest agent binds an app's directories under
#      /run/briard/files-view after Files started, and slave propagation carries them in;
#   2. THE TIERS KEEP THEIR MODES: data and unversioned are the household's to edit, files and
#      its Versions are read-only -- refused by the kernel, through the real app;
#   3. A RESTORE UNDERNEATH IS SEEN AT ONCE, without Files restarting: the area is swapped the
#      way a restore swaps it, converge binds the new one, and what Files reads is the restored
#      copy -- the stale bind is where an edit would otherwise vanish;
#   4. A VERSION APPEARS under its absolute name;
#   5. THE HOUSEHOLD GETS IN WITH THE DASHBOARD'S SESSION AND NOTHING ELSE: from the other
#      machine, through the door -- a stranger is sent to the dashboard, a trusted browser is
#      signed in with no login of the app's own -- while a forged token is refused both at the
#      pod and at the door;
#   6. THE VIEW LETS GO OF THE VOLUME when the chain stops.
#
# node2 is the household's other machine: every browser request is made from it, to the VIP.
{ pkgs, guestModule, fixture, filesFixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  node = h.mkNode {
    fixtures = [ fixture filesFixture ];
    resource = h.mkResource [
      { name = "node1"; id = 0; }
      { name = "node2"; id = 1; }
    ];
  };
in
pkgs.testers.runNixOSTest {
  name = "files";

  nodes = {
    node1 = node;
    node2 = node;
  };

  testScript = ''
    ${h.fixtureHelpers}
    import json, shlex, urllib.parse
    VIP = "192.168.1.100"
    VIEW = "/run/briard/files-view"

    start_all()
    for m in [node1, node2]:
        m.wait_for_unit("multi-user.target")
        m.wait_for_unit("briard-test-fixture-install.service", timeout=600)
        name_the_flock(m)
        m.succeed("modprobe drbd")
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 1")
    for m in [node1, node2]:
        m.succeed("systemctl start drbd-reactor.service")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    node1.wait_until_succeeds(f"curl -fsS http://{VIP}/healthz", timeout=300)
    primary = next(m for m in [node1, node2] if role(m) == "Primary")
    browser = node2 if primary == node1 else node1

    # The other app first, so its areas exist, then Files -- the order a household's would be.
    install_fixture(primary, app="dummy")
    install_fixture(primary, app="files")
    for unit in fixture_units(primary, app="files"):
        primary.wait_for_unit(unit, timeout=300)
    table = json.loads(primary.succeed("cat /run/briard/routes.json"))
    files_route = next(a for a in table["apps"] if a["name"] == "files")
    pod = files_route["address"]
    assert files_route.get("gate") == "/run/briard/files/gate.key", f"Files is not gated: {files_route}"
    primary.wait_until_succeeds(f"curl -fsS http://{pod}/health", timeout=180)
    files_started = primary.succeed("systemctl show -p ActiveEnterTimestampMonotonic briard-files-app.service").strip()

    # (1)+(2) THE VIEW, as the container sees it: a folder per app, its manifest's directories in
    # it, each tier in its mode -- read from inside the running container, where the claim lives.
    primary.succeed("echo hello > /var/lib/briard/data/dummy/app/hello.txt")
    primary.succeed("echo kept > /var/lib/briard/unversioned/dummy/keep/kept.txt")
    primary.succeed("echo media > /var/lib/briard/files/dummy/media/song.txt")
    primary.wait_until_succeeds("podman exec briard-files-app cat /srv/Dummy/app/hello.txt | grep -qx hello", timeout=60)
    listing = primary.succeed("podman exec briard-files-app ls -1 /srv/Dummy").split()
    assert sorted(listing) == ["app", "keep", "media"], f"the view of Dummy is {listing}, want its data, unversioned and files directories and no cache"
    primary.succeed("podman exec briard-files-app touch /srv/Dummy/app/written-by-files")
    primary.succeed("test -e /var/lib/briard/data/dummy/app/written-by-files")
    primary.fail("podman exec briard-files-app touch /srv/Dummy/media/written-by-files")
    top = primary.succeed("podman exec briard-files-app ls -1 /srv").split()
    assert top == ["Dummy"], f"the view's top is {top}; Files shows every app but itself"

    # (5) THE WAY IN, from the other machine. A browser with no session for Files' name is sent to
    # the dashboard to open it, and nothing reaches the app.
    files_host = "briard-files.local"
    def curl(host, path, cookie="", extra=""):
        c = f"-H 'Cookie: {cookie}'" if cookie else ""
        return browser.succeed(f"curl -sS -D - -o /tmp/body -H 'Host: {host}' {c} {extra} {shlex.quote('http://' + VIP + path)}")
    def status(headers):
        return int(headers.splitlines()[0].split()[1])
    def header(headers, name):
        return next(l.split(":", 1)[1].strip() for l in headers.splitlines() if l.lower().startswith(name.lower() + ":"))
    hd = curl(files_host, "/files/Dummy")
    assert status(hd) == 303 and header(hd, "Location").startswith("http://briard.local/open/files?"), hd
    # A token the browser brings is not a way in either: the door sends it to the dashboard too.
    hd = curl(files_host, "/api/resources?source=Apps&path=/", extra="-H 'X-JWT-Assertion: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJob3VzZWhvbGQifQ.forged'")
    assert status(hd) == 303, hd
    # And at the pod itself, around the door: no token is 401, a token not signed with the key 403.
    assert primary.succeed(f"curl -s -o /dev/null -w '%{{http_code}}' http://{pod}/api/resources?source=Apps&path=/").strip() == "401"
    forged = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJob3VzZWhvbGQiLCJleHAiOjQxMDI0NDQ4MDB9.c2lnbmVkLWJ5LW5vYm9keQ"
    assert primary.succeed(f"curl -s -o /dev/null -w '%{{http_code}}' -H 'X-JWT-Assertion: {forged}' http://{pod}/api/resources?source=Apps&path=/").strip() == "403"

    # The dashboard's session, as `briard open` leaves one: the host's one-time code, redeemed.
    code0 = "c0ffee" * 10 + "0123"
    issued = primary.succeed("date -u +%Y-%m-%dT%H:%M:%SZ").strip()
    handoff = json.dumps({"code": code0, "name": "Kostas", "username": "kostas", "language": "en", "issued": issued})
    primary.succeed("mkdir -p -m 0700 /run/briard/dashboard")
    primary.succeed(f"printf %s {shlex.quote(handoff)} > /run/briard/dashboard/handoff.json && chmod 0600 /run/briard/dashboard/handoff.json")
    hd = curl("briard.local", f"/?code={code0}")
    session = header(hd, "Set-Cookie").split(";")[0]
    assert session.startswith("briard_session="), hd
    # The card opens it; opening sends the browser to Files' name with a one-time code ...
    browser.wait_until_succeeds(f"curl -fsS -H 'Host: briard.local' -H 'Cookie: {session}' http://{VIP}/apps | grep -q 'href=\"/open/files\"'", timeout=60)
    hd = curl("briard.local", "/open/files", session)
    loc = urllib.parse.urlparse(header(hd, "Location"))
    assert status(hd) == 303 and loc.netloc == files_host and "briard_hop=" in loc.query, hd
    # ... which becomes Files' own cookie, the device's same token ...
    hd = curl(files_host, loc.path + "?" + loc.query)
    files_cookie = header(hd, "Set-Cookie").split(";")[0]
    assert status(hd) == 303 and files_cookie == session, f"{hd}\nwant the dashboard's own token, {session}"
    # ... and the household is in, as itself, with no login of the app's own.
    hd = curl(files_host, "/api/resources?source=Apps&path=/", files_cookie)
    assert status(hd) == 200, hd
    folders = [f["name"] for f in json.loads(browser.succeed("cat /tmp/body")).get("folders", [])]
    assert folders == ["Dummy"], f"Files shows {folders}"
    hd = curl(files_host, "/api/users?username=self", files_cookie)
    me = json.loads(browser.succeed("cat /tmp/body"))
    assert me["username"] == "household" and me["permissions"]["admin"], me

    # (2) THROUGH THE APP: an edit to the data lands on the volume; one to the files tier is refused.
    hd = curl(files_host, "/api/resources?source=Apps&path=/Dummy/app/hello.txt&override=true", files_cookie, "-X POST --data-binary edited")
    assert status(hd) == 200, hd
    primary.succeed("grep -qx edited /var/lib/briard/data/dummy/app/hello.txt")
    hd = curl(files_host, "/api/resources?source=Apps&path=/Dummy/media/song.txt&override=true", files_cookie, "-X POST --data-binary nope")
    assert status(hd) != 200, hd
    primary.succeed("grep -qx media /var/lib/briard/files/dummy/media/song.txt")

    # (3) A RESTORE UNDERNEATH, the way the product swaps an area: the restore point snapshotted
    # into place beside the live one, the live one deleted, the copy renamed over it -- then the
    # converge every restore ends with. Files reads the restored copy, and was never restarted.
    primary.succeed("mkdir -p /var/lib/briard/data/.snapshots")
    primary.succeed("btrfs subvolume snapshot -r /var/lib/briard/data/dummy /var/lib/briard/data/.snapshots/dummy-test-point")
    hd = curl(files_host, "/api/resources?source=Apps&path=/Dummy/app/hello.txt&override=true", files_cookie, "-X POST --data-binary after")
    assert status(hd) == 200, hd
    primary.succeed("btrfs subvolume snapshot /var/lib/briard/data/.snapshots/dummy-test-point /var/lib/briard/data/dummy.restoring")
    primary.succeed("btrfs subvolume delete /var/lib/briard/data/dummy && mv /var/lib/briard/data/dummy.restoring /var/lib/briard/data/dummy")
    primary.succeed("briard-guest-agent --converge")
    hd = curl(files_host, "/api/raw?source=Apps&file=/Dummy/app/hello.txt", files_cookie)
    got = browser.succeed("cat /tmp/body").strip()
    assert status(hd) == 200 and got == "edited", f"Files reads {got!r} after the restore, want the restored 'edited'"
    assert primary.succeed("systemctl show -p ActiveEnterTimestampMonotonic briard-files-app.service").strip() == files_started, \
        "Files was restarted to see the restore"

    # (4) A VERSION, under the name a person reads: the member as the clock takes it, read-only.
    primary.succeed("mkdir -p /var/lib/briard/files/.snapshots")
    primary.succeed("btrfs subvolume snapshot -r /var/lib/briard/files/dummy /var/lib/briard/files/.snapshots/dummy-clock-20261105T130000Z")
    primary.succeed("briard-guest-agent --converge")
    name = primary.succeed("date -d 2026-11-05T13:00:00Z '+%Y-%m-%d %a %H.%M'").strip()
    primary.succeed(f"podman exec briard-files-app cat {shlex.quote('/srv/Dummy/Versions/' + name + '/media/song.txt')} | grep -qx media")
    primary.fail(f"podman exec briard-files-app touch {shlex.quote('/srv/Dummy/Versions/' + name + '/media/x')}")

    # (6) THE CHAIN STOPS, AND THE VIEW LETS GO: no bind under the view holds the volume.
    primary.succeed("systemctl stop briard-apps.service")
    primary.fail(f"grep -q ' {VIEW}/' /proc/self/mountinfo")
  '';
}
