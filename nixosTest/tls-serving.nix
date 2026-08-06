# TLS termination that serves the cert, and survives failover. Completes v0's
# deferred half of "reachable by name WITH TLS" (proved issuance; nothing served it).
# briard-reverse-proxy terminates HTTPS at the VIP and forwards to the payload; its cert/key
# live on the DRBD volume, so they replicate and a survivor serves the SAME cert at the
# SAME VIP after a failover (the deferred "TLS survives failover", folded in here).
#
# Also covers the DELIVERY end: dropping a renewed cert on the vol is hot-reloaded live
# (no restart), and the survivor serves the renewed cert after failover. The certs are
# build-time self-signed (the controller-side renewal SCHEDULER
# is unit-tested in controller/renew_test.go) — this proves the SERVING + delivery layers
# hermetically.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # Two independent self-signed certs for the fixed VIP (each its own CA, so `curl
  # --cacert A` validates only what A signed) — cert A is the initial cert, cert B stands
  # in for a renewal. Generated deterministically at build.
  mkCert = name: pkgs.runCommand name { nativeBuildInputs = [ pkgs.openssl ]; } ''
    mkdir -p $out
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
      -keyout $out/key.pem -out $out/fullchain.pem -days 30 \
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
        m.succeed("drbdadm create-md --force r0")
    for m in machines:
        m.succeed("systemctl start drbd@r0.target")
    node1.wait_until_succeeds("test $(drbdadm cstate r0 | grep -c Connected) -ge 2")
    node1.succeed("drbdadm new-current-uuid --clear-bitmap r0/0")
    for m in disk_nodes:
        m.succeed("systemctl start drbd-reactor.service")
    # The payload comes up on the primary and answers plain HTTP at the VIP.
    node1.wait_until_succeeds("curl -fsS http://192.168.1.100:8080/healthz")

    def role(m):
        return m.execute("drbdadm role r0")[1].strip()

    primary = next(m for m in disk_nodes if role(m) == "Primary")
    survivor = next(m for m in disk_nodes if m != primary)
    print(f"primary={primary.name} survivor={survivor.name}")

    # Drop the cert/key onto the DRBD volume (replicates to the survivor) and flush — the
    # same durability the pin needs: a failover right after must find the cert on the peer.
    primary.succeed("mkdir -p /var/lib/briard/tls")
    primary.succeed("cp ${testCert}/fullchain.pem ${testCert}/key.pem /var/lib/briard/tls/")
    primary.succeed("sync")

    # briard-reverse-proxy (woven into the promoter chain via briard-vip) hot-reloads the cert
    # and serves HTTPS at the VIP, proxying to the payload — https://<name> is now true.
    primary.wait_until_succeeds("curl -fsS --cacert ${testCert}/fullchain.pem https://192.168.1.100/healthz")
    print("HTTPS served at the VIP, terminated by briard-reverse-proxy -> payload")
    # The front door also answers PLAIN http on :80 — the only door a free node has,
    # since :443 needs a cert and a cert needs a domain. Its /healthz reports the payload's
    # own answer, which is what lets the host agent probe one stable URL either way.
    primary.succeed("curl -fsS http://192.168.1.100/healthz")
    # The payload's own port is still directly reachable behind it.
    primary.succeed("curl -fsS http://192.168.1.100:8080/healthz")
    # Cert A is served, not cert B (yet): --cacert B must reject it.
    primary.fail("curl -fsS --cacert ${testCertB}/fullchain.pem https://192.168.1.100/healthz")

    # ---- renewal delivery: drop a NEW cert on the vol -> hot-reloaded, no restart ----
    # This is what a controller-pushed DirectiveCert lands (here via a direct write; the
    # cert.write verb + the whole controller->agent->guest path are unit-tested).
    primary.succeed("cp ${testCertB}/fullchain.pem ${testCertB}/key.pem /var/lib/briard/tls/")
    primary.succeed("sync")
    # Now cert B is served (hot-reload) and cert A no longer validates — no restart.
    primary.wait_until_succeeds("curl -fsS --cacert ${testCertB}/fullchain.pem https://192.168.1.100/healthz")
    primary.fail("curl -fsS --cacert ${testCert}/fullchain.pem https://192.168.1.100/healthz")
    print("renewed cert hot-reloaded live at the VIP (no restart)")

    # ---- TLS survives failover: the RENEWED cert (replicated), same VIP, on the survivor ----
    primary.crash()
    survivor.wait_until_succeeds("curl -fsS --cacert ${testCertB}/fullchain.pem https://192.168.1.100/healthz")
    print("after failover the survivor serves the renewed cert at the SAME VIP — TLS + renewal survived")
  '';
}
