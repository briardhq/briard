# An app named after the user takes the name when it starts, and never starts without one.
#
# A catalog entry may name the user in a container's env (`${BRIARD_USERNAME}`,
# shared/manifest). The unit tests prove the rendered unit carries the placeholder as written and
# names the file the name is in; this proves the rest on a booted guest, each a property of the
# guest's own quadlet and systemd rather than of our code:
#
#   1. quadlet passes the placeholder into the generated unit untouched, and systemd replaces it
#      with the file's value inside one argument, so the container is handed `--user kostas`;
#   2. on a node with no user record the app is not started at all, while the node goes on
#      serving;
#   3. a unit whose file is gone does not start: it never starts with an empty name.
#
# The record is written here as the dashboard writes it (its own tests cover the open that does).
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  fixture = import ./fixture-app.nix {
    inherit pkgs;
    env = { WHO = "--user \${BRIARD_USERNAME}"; };
  };
  node = h.mkNode {
    inherit fixture;
    replicated = false;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };
  unit = "briard-dummy-app.service";
  ctr = "briard-dummy-app";
in
pkgs.testers.runNixOSTest {
  name = "app-user-name";
  nodes.node1 = {
    imports = [ node ];
  };

  testScript = ''
    ${h.fixtureHelpers}
    import json
    unit = "${unit}"
    FILE = "/run/briard/user.env"

    def who():
        env = json.loads(node1.succeed("podman inspect ${ctr} --format '{{json .Config.Env}}'"))
        return [e for e in env if e.startswith("WHO=")]

    node1.start()
    node1.wait_for_unit("multi-user.target")
    node1.wait_for_unit("briard-test-fixture-install.service")
    node1.succeed("briard-test-storage --seed")
    node1.succeed("systemctl start briard-chain.target")
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz", timeout=120)

    # The fixture is what the test thinks it is: the placeholder, as a catalog entry writes it.
    manifest = json.loads(node1.succeed(f"cat {fixture_dir()}/manifest.json"))
    assert manifest["containers"][0]["env"] == {"WHO": "--user ''${BRIARD_USERNAME}"}, manifest

    # (2) No record: converge leaves the app out and the node serves on.
    node1.fail("test -e /var/lib/briard/dashboard/account.json")
    install_fixture(node1)
    node1.fail(f"systemctl is-active {unit}")
    node1.fail("podman container exists ${ctr}")
    node1.fail(f"test -e {FILE}")
    node1.succeed("curl -fsS http://192.168.1.100/healthz")
    source = node1.succeed("cat /run/containers/systemd/${ctr}.container")
    assert 'Environment="WHO=--user ''${BRIARD_USERNAME}"' in source and f"EnvironmentFile={FILE}\n" in source, source

    # (1) With a record the app starts under the record's username.
    record = json.dumps({"id": "abcdefghijklmnop", "username": "kostas", "name": "Kostas", "language": "en"})
    node1.succeed(f"install -d -m 0700 /var/lib/briard/dashboard && printf %s '{record}' > /var/lib/briard/dashboard/account.json")
    node1.succeed("briard-guest-agent --converge")
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    assert node1.succeed(f"cat {FILE}") == "BRIARD_USERNAME=kostas\n"
    assert who() == ["WHO=--user kostas"], who()

    # (3) The file gone: the unit does not start, and the next converge brings the app back.
    node1.succeed(f"rm {FILE}")
    node1.execute(f"systemctl restart {unit}")
    state = node1.succeed(f"systemctl show -p ActiveState --value {unit}").strip()
    assert state != "active", f"{unit} is {state} with no file to take the name from"
    node1.fail("curl -fsS --max-time 5 http://192.168.1.100:8080/healthz")
    node1.succeed("briard-guest-agent --converge")
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz", timeout=120)
    assert who() == ["WHO=--user kostas"], who()
  '';
}
