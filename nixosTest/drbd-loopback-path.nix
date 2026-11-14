# Gating experiment for the cloud-witness loopback-label scheme (the witness would give each
# anchor a loopback identity label the proxy source-binds to): does DRBD accept a loopback
# per-path address via plain drbdsetup, with no drbd-proxy `proxy {}` section? If it binds one and
# goes Connecting (not EADDRNOTAVAIL "Configured local address not found"), the scheme is viable.
#
# One diskless node, configured entirely via drbdsetup (no .res, no drbdadm): one peer, path
# 127.0.0.1 -> 127.0.0.2. Nobody listens on the remote, so a healthy result is "Connecting"
# (accepted + trying), not an address rejection.
{ pkgs, guestModule }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };
  # A dummy r0.res just to satisfy mkNode; never loaded (we drive drbdsetup on resource "loop").
  resource = ''resource r0 { on n { node-id 0; volume 0 { device /dev/drbd0; disk none; } } }'';
in
pkgs.testers.runNixOSTest {
  name = "drbd-loopback-path";
  skipTypeCheck = true;

  nodes.n = h.mkNode { inherit resource; diskless = true; promoter = false; };

  testScript = ''
    n.start()
    n.wait_for_unit("multi-user.target")
    n.succeed("modprobe drbd")

    # Build a resource + one peer + a loopback path, purely via drbdsetup. Print each step's output
    # so any drbdsetup error is legible in one run.
    for cmd in [
        "drbdsetup new-resource loop 0",
        "drbdsetup new-minor loop 5 0",
        "drbdsetup new-peer loop 1 --_name=peer1 --protocol=C",
        "drbdsetup new-path loop 1 ipv4:127.0.0.1:7999 ipv4:127.0.0.2:7999",
        "drbdsetup connect loop 1",
    ]:
        status, out = n.execute(cmd + " 2>&1")
        print(f"[exit {status}] {cmd}\n{out}")
    n.sleep(5)

    print("STATUS:", n.succeed("drbdsetup status loop || true"))
    print("DMESG:", n.succeed("dmesg | grep -iE 'drbd|local address' | tail -30 || true"))

    # The verdict: DRBD accepted the loopback local path address (no EADDRNOTAVAIL) and is Connecting.
    n.fail("dmesg | grep -q 'Configured local address not found'")
    n.succeed("drbdsetup status loop | grep -q Connecting")
  '';
}
