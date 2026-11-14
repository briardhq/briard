# TLS termination that serves the cert and survives failover: the serving half of "reachable by
# name with TLS". briard-reverse-proxy terminates HTTPS at the VIP; its cert/key live on the DRBD
# volume, so a survivor serves the same cert at the same VIP after a failover. Nothing runs behind
# the proxy, deliberately: termination, replication and hot-reload need no backend.
#
# Also the delivery end: a renewed cert dropped on the volume is hot-reloaded live (no restart),
# and the survivor serves it after failover. The certs are build-time self-signed (the renewal
# scheduler is the controller's, not this repo's); this proves serving and delivery hermetically.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # Two independent self-signed certs for the fixed VIP, each its own CA (so `curl --cacert A`
  # validates only what A signed): A is the initial cert, B stands in for a renewal.
  #
  # The validity must outlive the store path, not the test run: the inputs never change, so nix
  # builds it once, the clock starts at build time, and the test may run any number of days
  # later. The nightly force-rerun does not help, by design: it invalidates the `vm-test-run-*`
  # outputs, and these certs are a dependency of those, not a referrer, so they stay warm with
  # the guest image and are never rebuilt. A short-dated fixture expires in the store and fails a
  # test that asserts nothing about expiry -- taking the two `--cacert` negatives with it, which
  # pass vacuously once every request fails. The other fixtures here are long-dated likewise.
  mkCert = name: pkgs.runCommand name { nativeBuildInputs = [ pkgs.openssl ]; } ''
    mkdir -p $out
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
      -keyout $out/key.pem -out $out/fullchain.pem -days 36500 \
      -subj "/CN=briard.test" -addext "subjectAltName=DNS:briard.test,IP:192.168.1.100"
  '';
  testCert = mkCert "briard-test-cert-a";
  testCertB = mkCert "briard-test-cert-b";
  resource = h.mkResource [
    { name = "node1"; id = 0; }
    { name = "node2"; id = 1; }
    { name = "witness"; id = 2; diskless = true; }
  ];
  diskNode = h.mkNode { inherit resource; };
  witnessNode = h.mkNode { inherit resource; diskless = true; promoter = false; };
in
pkgs.testers.runNixOSTest {
  name = "tls-serving";
  skipTypeCheck = true;

  nodes = {
    node1 = diskNode;
    node2 = diskNode;
    witness = witnessNode;
  };

  testScript = ''
    disk_nodes = [node1, node2]
    machines = [node1, node2, witness]
    start_all()
    for m in machines:
        m.wait_for_unit("multi-user.target")
        m.succeed("modprobe drbd")
    for m in disk_nodes:
        m.succeed("briard-test-storage --seed" if m == node1 else "briard-test-storage")
    # The witness has no tier to build but still needs its `.res` and its attach:
    # briard-node-storage runs on every node, so one call covers both.
    witness.succeed("briard-test-storage")
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")
    # The reverse proxy answers plain HTTP at the VIP for itself: no app runs, the shipped state
    # and all a cert test needs.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100/healthz")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    primary = next(m for m in disk_nodes if role(m) == "Primary")
    survivor = next(m for m in disk_nodes if m != primary)
    print(f"primary={primary.name} survivor={survivor.name}")

    # The cert/key onto the DRBD volume, flushed -- the durability the pin needs too: a failover
    # right after must find the cert on the peer.
    primary.succeed("mkdir -p /var/lib/briard/tls")
    primary.succeed("cp ${testCert}/fullchain.pem ${testCert}/key.pem /var/lib/briard/tls/")
    primary.succeed("sync")

    # briard-reverse-proxy (a promoter chain member, after briard-vip) hot-reloads the cert
    # and serves HTTPS at the VIP — https://<name> is true.
    primary.wait_until_succeeds("curl -fsS --cacert ${testCert}/fullchain.pem https://192.168.1.100/healthz")
    print("HTTPS served at the VIP, terminated by briard-reverse-proxy")
    # Plain http on :80 still answers: the only door a free node has (:443 needs a cert, a cert
    # needs a domain), and one stable URL lets the host agent probe the same address everywhere.
    primary.succeed("curl -fsS http://192.168.1.100/healthz")
    # Cert A is served, not cert B (yet): --cacert B must reject it.
    primary.fail("curl -fsS --cacert ${testCertB}/fullchain.pem https://192.168.1.100/healthz")

    # ---- renewal delivery: a new cert on the volume -> hot-reloaded, no restart ----
    # What a controller-pushed DirectiveCert lands, here by a direct write (the cert.write verb
    # and the agent->guest path are unit-tested).
    primary.succeed("cp ${testCertB}/fullchain.pem ${testCertB}/key.pem /var/lib/briard/tls/")
    primary.succeed("sync")
    # Now cert B is served (hot-reload) and cert A no longer validates — no restart.
    primary.wait_until_succeeds("curl -fsS --cacert ${testCertB}/fullchain.pem https://192.168.1.100/healthz")
    primary.fail("curl -fsS --cacert ${testCert}/fullchain.pem https://192.168.1.100/healthz")
    print("renewed cert hot-reloaded live at the VIP (no restart)")

    # ---- TLS survives failover: the renewed cert (replicated), same VIP, on the survivor ----
    # The crash must find an UpToDate replica. The seed path syncs a joiner for real, so a replica
    # is a SyncTarget, which cannot promote, until the initial resync completes; "replicated" is
    # the claim under test, so the disk state is asserted before the only UpToDate copy goes.
    survivor.wait_until_succeeds("drbdadm dstate r0 | grep -q '^UpToDate'", timeout=300)
    primary.crash()
    survivor.wait_until_succeeds("curl -fsS --cacert ${testCertB}/fullchain.pem https://192.168.1.100/healthz")
    print("after failover the survivor serves the renewed cert at the SAME VIP — TLS + renewal survived")
  '';
}
