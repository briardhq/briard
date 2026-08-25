# The free-local `curl | sh` install on the MACVTAP substrate -- the DEFAULT substrate as of
# the test that owns the WHOLE install chain end to end:
#   report-card gate -> BUNDLED qemu (no distro qemu on the host) boots the guest -> single-node
#   DRBD data volume -> payload served at the VIP -> an OFF-BOX LAN client reaches it.
#
# The macvtap-specific deltas on top of that chain:
#   1. NO bridge is created and the host's IP never leaves the physical NIC (the invasiveness
#      win — the SSH-risk moment and net guard are gone).
#   2. the guest's NICs are macvtap children of the host NIC (L2 citizens, no bridge).
#   3. qemu is launched behind the fd-passing wrapper (briard-net-wrap) and really holds the
#      /dev/tap<ifindex> chardevs on the inherited fds — the mechanism ifname= can't provide.
#   4. an OFF-BOX LAN client still reaches the payload at the VIP through the macvtap (the guest
#      is a full L2 citizen), which is the whole point.
#   5. and so does the INSTALL HOST -- by the VIP and by the name -- over the private link, which
#      is the one machine macvtap would otherwise hide the household's own service from ([V3b.19]).
#
# It also proves assertion (d) -- cattle/pet reinstall: after green, `rm -rf /opt/briard`
# (the cattle) + reinstall reaches green AGAIN with the guest DATA intact (the payload's persisted
# tick counter, on the pet /var/lib/briard data volume, does not reset). The failable control is
# sharp -- a wiped/reformatted volume would restart the counter at ~0.
#
# It carries the mode-independent half (install-bridge.nix, cut down
# to the bridge deltas). It had accumulated on the FALLBACK's test purely because bridge was the
# original default -- so the default substrate was the thin one, and dropping the fallback would
# have silently taken the reinstall proof with it. Now install-bridge.nix is a pure delta: when the
# bridge fallback goes, deleting that file loses nothing mode-independent.
#
# agent-bringup.nix already proves the agent MECHANISM (nested guest, DRBD, VIP) with the host
# itself as the client. This test proves the INSTALLER around it: it runs scripts/install.sh
# verbatim, uses only the bundled qemu (pkgs.qemu is deliberately absent), and a second node on
# the shared L2 -- not the install host -- curls the VIP, so reachability is genuinely off-box.
#
# Heavy (two nested guest boots + a multi-GB guest disk) -> rides the `install` nightly tag
# alongside install-bridge, qemu-bundle + report-card. Run:
#   nix build .#tests.install-macvtap -L
{ pkgs, guestDisk, agent, qemuBundle, selfupdateStub }:
let
  # THE RELEASE CHANNEL, BUILT THE WAY A RELEASE IS BUILT.
  #
  # This used to be a `staging` dir of loose uncompressed files handed to install.sh through
  # BRIARD_ARTIFACTS -- a shape nothing ships: the qemu bundle as a DIRECTORY rather than the
  # tarball a real install unpacks, and no manifest, signature or compression anywhere. So the
  # installer's actual first act on a stranger's machine (fetch a signed, compressed set over
  # HTTP and verify it before touching the disk) was the one link in the chain no test ran, and
  # BRIARD_ARTIFACTS -- an escape hatch -- was what every install test proved instead.
  #
  # Everything here mirrors scripts/publish-release.sh, and the manifest is written by the REAL
  # writer (`briard-agent --stage-manifest`) -- the same binary that reads it back during the
  # install below. A format change that breaks that round trip now fails HERE instead of at a
  # stranger's first install.
  channel = pkgs.runCommand "briard-test-channel" {
    nativeBuildInputs = [ pkgs.zstd pkgs.openssl pkgs.gnutar ];
  } ''
    mkdir -p "$out"
    install -m0755 ${agent}/bin/briard-agent        "$out/briard-agent"
    install -m0755 ${../scripts/briard-net-wrap.sh} "$out/briard-net-wrap"
    # Deterministic tar, same flags as the release script: the bundle is a directory in the store
    # and the channel contract wants one file.
    tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner \
        -cf qemu-bundle.tar -C ${qemuBundle} .
    zstd -19 -q --rm qemu-bundle.tar -o "$out/qemu-bundle.tar.zst"
    zstd -19 -q ${guestDisk}/nixos.qcow2 -o "$out/nixos.qcow2.zst"
    chmod 0644 "$out"/*.zst
    # The production writer, not a re-implementation in Nix -- which would have tested this file
    # against itself and proven nothing about what a release actually publishes.
    "$out/briard-agent" --stage-manifest "$out"
  '';
  # ⚠️ THE CHANNEL IS SIGNED AT RUNTIME, NOT HERE, and that is a constraint rather than a
  # preference: a signing key committed to the repo trips `TestNoSecretMaterial` (internal/arch),
  # the ★ guard that refuses a PRIVATE KEY block anywhere in the tree. It is right to refuse one
  # -- a throwaway test key is still a private key sitting in a public repo, and the guard cannot
  # tell the difference, which is exactly why it should not try. So the test mints a keypair
  # inside the VM and signs there.
  #
  # The VM serves a SYMLINK FARM over the store rather than a copy: the channel is ~400 MB and
  # only one small file in it (the signature) has to be writable, so copying it would cost the
  # test half a gigabyte of VM disk to add 64 bytes.
  # What install.sh needs to reach the channel: where it lives, and the release public key it
  # verifies the manifest against. Exactly the two the shipped one-liner sets. The keyring is
  # minted into /root at test start (see the signing step in the script).
  channelEnv = "BRIARD_CHANNEL_URL=http://127.0.0.1:8099 BRIARD_KEYRING=/root/keyring.pem";
  installScript = ../scripts/install.sh;
in
pkgs.testers.runNixOSTest {
  name = "install-macvtap";
  skipTypeCheck = true; # dynamic asserts, systemd units created at runtime

  nodes = {
    host =
      { ... }:
      {
        virtualisation.memorySize = 6144; # report-card 4 GB floor + the 2 GB nested guest
        virtualisation.cores = 4;
        # 14 GB, not 10: this test runs install.sh TWICE, and the first (must-fail) invocation stages
        # ~2 GB before dying at the NIC check -- so the real run's report card measures a disk the
        # previous run already ate into. Against the product's own 8 GB floor that left under a GB of
        # margin, and V3.19's artifact growth tipped it: the card refused with "7 GB free" and the
        # install never ran. Headroom, so the test stops measuring the admission floor by accident.
        virtualisation.diskSize = 14336;
        virtualisation.vlans = [ 1 ]; # eth1 on the shared 192.168.1.0/24 L2 (the LAN)
        virtualisation.qemu.options = [ "-cpu" "host" ]; # vmx -> nested KVM in L1
        networking.useDHCP = false;
        networking.interfaces.eth1.ipv4.addresses = [
          { address = "192.168.1.1"; prefixLength = 24; }
        ];
        # IPv6 OFF on the install host, permanently and on purpose ([V3b.26b]). A stranger may have
        # disabled v6 before installing -- it is their machine and their setting -- and DESIGN §4.3
        # puts our addressing on v4 INDEFINITELY, so nothing we ship may quietly need v6 to work.
        #
        # This is not hypothetical: it caught one. With the private link briefly unnumbered, avahi
        # answered mDNS on it over IPv6 only (it joins the IPv4 group on an interface only if that
        # interface HAS a v4 address), so [V3b.19]'s name half silently depended on the HOST having
        # a v6 link-local on the tap. With v6 on, the name resolved and everything looked correct;
        # with v6 off, the household's own machine could not find its own node while every other
        # assertion in this file still passed. A rig that leaves v6 enabled cannot tell those apart.
        boot.kernel.sysctl."net.ipv6.conf.all.disable_ipv6" = 1;
        boot.kernel.sysctl."net.ipv6.conf.default.disable_ipv6" = 1;
        environment.systemPackages = [ pkgs.iproute2 pkgs.iputils pkgs.kmod pkgs.curl pkgs.avahi ];
        # An mDNS resolver ON THE INSTALL HOST, which is what a desktop install actually is
        # ([V3b.19] was measured on one). It is here to make a dependency VISIBLE rather than to
        # flatter the result: resolving the guest's name from this machine needs the household's
        # own machine to speak mDNS, and a host that speaks none resolves no .local name from
        # anywhere -- with or without us. Encoding it as rig config is how that stays honest.
        #
        # nssmdns4 as well as the daemon, because the gesture being fixed is a person typing the
        # name into a browser, and a browser goes through nsswitch. The assertions below use both:
        # avahi-resolve-host-name proves the QUERY reaches the guest over the private link and is
        # answered there, and a curl by name proves the household's actual gesture works.
        services.avahi = { enable = true; nssmdns4 = true; };
      };

    # The off-box client that must reach the VIP through the guest's macvtap.
    client =
      { ... }:
      {
        virtualisation.vlans = [ 1 ];
        networking.useDHCP = false;
        networking.interfaces.eth1.ipv4.addresses = [
          { address = "192.168.1.2"; prefixLength = 24; }
        ];
        environment.systemPackages = [ pkgs.curl pkgs.iputils pkgs.avahi ];
        # An mDNS RESOLVER, so the NAME can be exercised the way a household uses it instead of by
        # reading our own config back. nssmdns4 puts `mdns4_minimal` into nsswitch -- the very
        # resolver V3.19d measured on the real Ubuntu desktop, and the reason the published name is
        # single-label. Without this the test could only assert the name we asked for, which proves
        # nothing: the failure being guarded against is a name that publishes fine and resolves
        # nowhere.
        services.avahi = { enable = true; nssmdns4 = true; };
      };

    # THE ROUTER -- the piece this L2 never had, and the reason V3.19c's DHCP path could only be
    # argued for ([B.78]). Every other test DECLARES the service address, so they all exercise the
    # operator-named source and none of them exercises the one the product now defaults to.
    #
    # A POOL, deliberately, with no reservation for our MAC: the test must not know the address in
    # advance, because "we never depended on knowing it" is the property under test. It is
    # discovered the way a household would discover it -- by reading the router's own lease table.
    #
    # The range starts at .100 so it cannot collide with the driver-assigned statics (host .1,
    # client .2, router .3), and nothing else on this segment leases.
    router =
      { ... }:
      {
        virtualisation.vlans = [ 1 ];
        networking.useDHCP = false;
        networking.firewall.enable = false;
        networking.interfaces.eth1.ipv4.addresses = [
          { address = "192.168.1.3"; prefixLength = 24; }
        ];
        # On PATH so the test can run a SECOND server by hand with a different pool -- the module's
        # config is a store path, and the point of that step is to change the pool underneath a
        # running node.
        environment.systemPackages = [ pkgs.dnsmasq ];
        services.dnsmasq = {
          enable = true;
          settings = {
            interface = "eth1";
            bind-interfaces = true;
            # DHCP only. port=0 disables the DNS half: the guest resolves over its own WAN path
            # (eth0, SLIRP), and a resolver here would be one more thing to explain if it broke.
            port = 0;
            # A TWO MINUTE lease, not the household 12h. A client holding a valid lease does not
            # talk to a server until T1 (half the term), so with a realistic term the node would
            # never notice a pool change inside a test's lifetime -- which is not a product
            # limitation, it is DHCP working. Shortening the term is what makes the renewal path
            # reachable at all; it also means the address genuinely EXPIRES while the router is
            # stopped, so --lastleaseextend stops being an argument and becomes an assertion.
            dhcp-range = "192.168.1.100,192.168.1.150,2m";
            dhcp-authoritative = true;
          };
        };
      };
  };

  testScript = ''
    import time

    # The guest's address is the ROUTER's to know, so this is how the test learns it -- the same
    # place a household would look. Returns (ip, mac, hostname, client-id) from dnsmasq's lease
    # table, whose columns are: <expiry> <mac> <ip> <hostname> <client-id>.
    #
    # It waits because the lease is drawn at PROMOTION, not at install: install.sh returns as soon
    # as the agent is up, and the guest has to boot and promote before it asks for anything.
    def wait_for_lease(timeout=180):
        deadline = time.time() + timeout
        while time.time() < deadline:
            leases = router.succeed("cat /var/lib/dnsmasq/dnsmasq.leases || true")
            for line in leases.strip().splitlines():
                f = line.split()
                # Any lease at all is enough to return -- the hostname is asserted by the CALLER,
                # so a missing one fails as "the router recorded no name" rather than hiding here
                # as a timeout that says the guest never asked.
                if len(f) >= 5:
                    return f[2], f[1], f[3], f[4]
            time.sleep(2)
        # Diagnose HERE, not only at the later steps: "no lease" is the least informative failure
        # in this test -- it is consistent with the guest never booting, never promoting, never
        # asking, and asking into silence -- and the guest console is the only witness that can
        # tell those apart.
        diagnose("no lease")
        raise Exception("no DHCP lease was ever handed out -- the guest never asked, or never got one")

    def guest_console(pattern):
        """Read the guest's serial console, which is a TTY stream: \\r-terminated lines render as
        blank in a captured log, and dumping it raw once made a 94 KB file look empty. Strip the
        carriage returns and select, rather than trusting the shape of a wall of text."""
        return host.succeed(
            f"tr -d '\\r' < /var/log/briard-guest-console.log 2>/dev/null | grep -aE '{pattern}' | tail -40 || true"
        )

    def diagnose(where):
        # EVERY briard-vip line from EVERY boot, untailed. The console appends across guest
        # incarnations now, so the story spans boots -- and tailing it would keep only the last
        # one, which is the boot that already told us it found nothing. What is wanted is the
        # EARLIER boot, where the address was acquired and should have been remembered.
        print(f"=== {where}: briard-vip, all boots ===")
        print(host.succeed(
            "tr -d '\\r' < /var/log/briard-guest-console.log 2>/dev/null | grep -a 'briard-vip:' || echo '(no briard-vip lines at all)'"
        ))
        print(f"=== {where}: what the guest said about its address ===")
        print(guest_console("briard-vip|dhcpcd|briard-data|eth0|eth2|Failed|error"))
        print(f"=== {where}: guest reached multi-user? ===")
        print(guest_console("Reached target|Startup finished|briard-guest"))
        print(f"=== {where}: dnsmasq ===")
        print(router.succeed("journalctl -u dnsmasq --no-pager | tail -30 || true"))
        print(f"=== {where}: agent journal ===")
        print(host.succeed("journalctl -u briard-agent --no-pager | tail -40 || true"))

    start_all()
    host.wait_for_unit("multi-user.target")
    client.wait_for_unit("multi-user.target")
    router.wait_for_unit("dnsmasq.service")
    host.succeed("ls -l /dev/kvm")
    client.wait_until_succeeds("ping -c1 -W2 192.168.1.1", timeout=30)

    # The guest console. The host cannot reach the guest over macvtap, so this is the only witness to
    # anything that happens inside it -- which is why THE INSTALLER now wires it, and why this test
    # no longer lays down its own drop-in.
    #
    # The drop-in was a vacuous green of the exact shape [V3.7g-1] warned about: it granted this rig
    # the witness that every real install went without, so the console every field diagnosis depended
    # on (V3.20-V3.23) was one the product never actually shipped, and the test could not have said
    # so. Reading the installer's own path is what makes the capture a tested property instead of a
    # local convenience.

    # --- refuse-with-fix / never-a-half-install: a bogus NIC dies before touching networking ---
    # (The report-card refusals themselves are proven in report-card.nix; here we prove the
    # installer aborts cleanly at the networking step, leaving no macvtap behind. Under macvtap
    # that step is not irreversible the way the bridge enslave is -- install-bridge.nix keeps the
    # sharper version of this check -- but "refuses and changes nothing" must hold on the default
    # substrate too, so it is asserted here in the mode's own terms.)
    # The release channel, served over HTTP for the rest of this test. Every install below goes
    # through the REAL network path -- fetch the signed manifest, verify it against the keyring,
    # verify each artifact's hash, expand the compressed ones -- which is what a stranger's
    # machine does and what BRIARD_ARTIFACTS was quietly standing in for.
    # Mint a release keypair and sign the manifest the channel derivation already wrote. The
    # signed bytes are the ones the writer produced -- signing does not re-serialise them, so what
    # the agent verifies is exactly what `--stage-manifest` emitted.
    stub = "${selfupdateStub}/bin/briard-selfupdate-stub"
    host.succeed(f"{stub} keygen /root/release.key /root/keyring.pem")
    # A symlink farm over the read-only store: only the signature needs to be a real file here.
    host.succeed("mkdir -p /srv && ln -sf ${channel}/* /srv/")
    host.succeed(f"{stub} sign /root/release.key /srv/manifest.json | base64 -d > /srv/manifest.json.sig")
    host.succeed("test -s /srv/manifest.json.sig")

    host.succeed(
        f"systemd-run --unit=briard-channel --collect {stub} serve 127.0.0.1:8099 /srv"
    )
    host.wait_until_succeeds("curl -sf http://127.0.0.1:8099/manifest.json -o /dev/null", timeout=30)
    # The manifest is SIGNED and the artifacts are COMPRESSED -- assert the shape before relying
    # on it, so a channel that silently went back to loose plaintext files cannot pass as green.
    host.succeed("curl -sf http://127.0.0.1:8099/manifest.json.sig -o /dev/null")
    host.succeed("curl -sf http://127.0.0.1:8099/nixos.qcow2.zst -o /dev/null")
    host.fail("curl -sf http://127.0.0.1:8099/nixos.qcow2 -o /dev/null")

    host.fail(
        "${channelEnv} BRIARD_NET_MODE=macvtap BRIARD_NIC=nope999 sh ${installScript}"
    )
    host.fail("ip link show briard0")       # nothing half-built
    host.fail("ip link show briard-drbd0")
    client.succeed("ping -c1 -W2 192.168.1.1")  # host still on the LAN

    # --- the install on the macvtap substrate: one command -> green ---
    # BRIARD_UNIT_DIR=/run/systemd/system: NixOS's /etc/systemd/system is a read-only store
    # symlink (a stock host's is writable), so the hermetic test drops the units in /run.
    host.succeed(
        "${channelEnv} BRIARD_NIC=eth1 BRIARD_NET_MODE=macvtap "
        "BRIARD_UNIT_DIR=/run/systemd/system sh ${installScript}"
    )

    # DELTA 1: NO bridge, and the host IP NEVER left eth1 (macvtap's invasiveness win).
    host.fail("ip link show br-briard")
    host.succeed("ip -o -4 addr show dev eth1 | grep -qw 192.168.1.1")
    client.succeed("ping -c1 -W2 192.168.1.1")  # host never lost its footing

    # DELTA 2: the guest's NICs are macvtap children of eth1 (not taps on a bridge).
    host.succeed("ip -d link show briard-drbd0 | grep -q macvtap")
    host.succeed("ip -d link show briard0 | grep -q macvtap")
    host.succeed("ip -o link show briard-drbd0 | grep -q 'briard-drbd0@eth1'")

    # DELTA 2b [B.106]: the host end of each macvtap holds NO IPv6 address. The device carries the
    # MAC the wrapper pins -- the GUEST's -- so a host left to autoconfigure derives the same
    # EUI-64 identifier the guest derives on the same L2, and its avahi joins mDNS on the guest's
    # segment. Not vacuous in a hermetic test with no router: a link-local needs no advertisement,
    # it appears from the device coming up alone (measured on real hardware 2026-08-19), so this
    # fails on any installer without the procfs write.
    for dev in ("briard0", "briard-drbd0"):
        host.succeed(f"grep -qx 1 /proc/sys/net/ipv6/conf/{dev}/disable_ipv6")
        assert host.succeed(f"ip -6 addr show dev {dev}").strip() == "", (
            f"{dev} holds an IPv6 address -- the host is speaking on the guest's identity"
        )

    # The guest converges on the bundled qemu, launched behind the fd-passing wrapper.
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64")

    # DELTA 3: the guest unit was started THROUGH briard-net-wrap, and qemu really holds the
    # macvtap chardevs on the inherited fds — the fd-passing mechanism ifname= cannot provide.
    host.succeed("systemctl show briard-guest.service -p ExecStart | grep -q briard-net-wrap")
    qpid = host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64").strip().split()[0]
    fds = host.succeed(f"ls -l /proc/{qpid}/fd/")
    print("qemu fds:\n" + fds)
    assert "/dev/tap" in fds, "qemu is not holding any /dev/tap<ifindex> chardev (fd-passing failed)"

    # DELTA 4 (THE PROOF): the OFF-BOX client reaches Briard at the VIP through the macvtap --
    # AT AN ADDRESS NOBODY IN THIS TEST CHOSE.
    #
    # BRIARD_VIP is unset above, so the guest asked the router and the router decided. We find out
    # the way a household would: by reading the lease table. Discovering it here rather than
    # asserting a constant is the whole point -- a test that knows the address in advance cannot
    # tell "we acquired one" from "we claimed the one we always claimed", which is exactly the
    # confusion V3.19 was.
    vip, mac, name, clientid = wait_for_lease()
    print(f"the router leased the guest {vip} (mac={mac} name={name} client-id={clientid})")

    # The flags reached the wire, which is the only place they can be confirmed. -h gives the
    # household a recognisable entry in its router's client list (and something to pin a static
    # reservation against); -I "" makes the client-id the hardware address, which is what keeps
    # ONE flock presenting ONE identity so the lease survives a failover.
    assert name.startswith("briard-"), f"the router recorded the client as {name!r}, not briard-*"
    assert clientid.lower() == "01:" + mac.lower(), (
        f"client-id is {clientid!r}, not the hardware address -- a per-host DUID here is what "
        f"would give the two nodes of one flock different leases"
    )

    client.wait_until_succeeds(f"curl -fsS http://{vip}/healthz", timeout=120)
    print("off-box client reached the leased VIP over the macvtap substrate")

    # THE CONSOLE CAPTURE ITSELF, asserted rather than assumed. Every helper above reads this file
    # through `2>/dev/null ... || true`, because a diagnostic must not become the reason a test
    # fails -- which means a MISSING file is indistinguishable from a quiet boot, and the capture
    # could silently stop shipping exactly the way it silently never shipped. So it is asserted once,
    # here, where it must fail if the installer stops wiring GUEST_SERIAL.
    #
    # Non-empty AND recognisable: a file qemu merely created proves the env var arrived, not that the
    # guest's console is on the other end of it. The kernel banner is the cheapest thing only a real
    # ttyS0 stream contains.
    host.succeed("test -s /var/log/briard-guest-console.log")
    host.succeed(
        "tr -d '\\r' < /var/log/briard-guest-console.log | grep -aqE 'Linux version|NixOS|systemd'"
    )
    # It must not be world-readable: it carries the household's hostnames and addresses.
    mode = host.succeed("stat -c%a /var/log/briard-guest-console.log").strip()
    assert mode in ("600", "640"), f"the guest console is mode {mode} -- readable beyond root"
    print("the INSTALLER's own guest-console capture is live, non-empty and not world-readable")

    # What a stranger actually gets. The install ships NO service, so the front door is
    # what answers -- and it says so, rather than the node looking broken or serving a workload
    # nobody chose. This is the assertion that would catch a payload sneaking back into the
    # shipped disk.
    page = client.succeed(f"curl -fsS http://{vip}/")
    assert "Nothing is routed to this address" in page, f"the VIP served: {page!r}"
    health = client.succeed(f"curl -fsS http://{vip}/healthz")
    assert "no backend configured" in health, f"/healthz said: {health!r}"

    # --- V3.20: the NAME, and the three identifiers behind it ---
    # Until this item one string (`guest`) was the API identity, the guest hostname, DRBD's
    # `on <name>` AND the mDNS label, so every install on earth answered to briard-guest.local.
    flock_name = host.succeed("cat /var/lib/briard/flock-name").strip()
    node_id = host.succeed("cat /var/lib/briard/node-id").strip()
    print(f"flock name={flock_name!r} node id={node_id!r}")

    # THE DRAWN SUBNETS ([V3b.26f]). Neither number is ours to spell any more: both are drawn per
    # home at install and checked against the network the host can see, so every assertion below
    # reads what this install actually chose. A rig that kept spelling 10.0.0.1 would not merely
    # fail -- the NEGATIVE assertions ("the name must not resolve to the private address") would
    # pass VACUOUSLY, which is the one way a test lies while staying green.
    subnets = host.succeed("cat /var/lib/briard/subnets")
    system_subnet = host.succeed("sed -n 's/^SYSTEM_SUBNET=//p' /var/lib/briard/subnets").strip()
    priv_subnet = host.succeed("sed -n 's/^PRIV_SUBNET=//p' /var/lib/briard/subnets").strip()
    print(f"drawn subnets: {subnets.strip()!r}")
    for label, drawn in (("system", system_subnet), ("private link", priv_subnet)):
        assert drawn.startswith("10.") and drawn.count(".") == 2, (
            f"the {label} subnet is {drawn!r}, not a bare 10.a.b -- install.sh built the substrate "
            f"out of something the draw did not produce"
        )
    # The draw HAPPENED, rather than a constant surviving under a new name. Both old values are in
    # the exclusion table, so neither can come back out of the draw by chance.
    assert system_subnet != "10.0.0", "the system subnet is still the hardcoded 10.0.0"
    assert priv_subnet != "10.11.9", "the private link is still the hardcoded 10.11.9"
    # The link pool is 10.11.x and the flock pool excludes it, which is what keeps a node's private
    # link off its own flock's subnet no matter what either draw returns.
    assert priv_subnet.startswith("10.11."), f"the private link {priv_subnet!r} is outside its pool"
    assert not system_subnet.startswith("10.11."), (
        f"the system subnet {system_subnet!r} landed in the private link's pool"
    )
    node_ip = f"{system_subnet}.1"
    priv_guest_ip = f"{priv_subnet}.2"
    assert node_id.startswith("briard-node-"), f"node id is {node_id!r}"
    assert node_id != "guest", "the node id is still the hardcoded literal this item removed"
    assert len(flock_name.split("-")) == 2, f"the flock name {flock_name!r} is not two words"
    assert flock_name != node_id, "the visible name and the node id are the same string again"

    # THE ASSERTION THE ITEM EXISTS FOR: the name resolves OFF-BOX, from a client using the same
    # mdns4_minimal resolver a household has. Publishing is not resolving -- V3.19d measured a name
    # that published fine and was unlookupable -- so this asks the client, not the guest.
    #
    # Instrumented BEFORE it is asserted, deliberately. The first two attempts at this assertion
    # failed with nothing to read: the diagnose() helper hangs off wait_for_lease, so an mDNS
    # failure printed one line and left the guest's side of it invisible. A publisher that refuses
    # (avahi returning "Not permitted") and a publisher that never runs look identical from the
    # client, and guessing between them costs a full runner round-trip each time.
    def mdns_state(where):
        print(f"=== {where}: the guest's publisher and daemon ===")
        print(host.succeed(
            "tr -d '\\r' < /var/log/briard-guest-console.log 2>/dev/null | "
            "grep -aiE 'avahi|mdns|entry group|Established|Not permitted|briard-identity' | tail -40 "
            "|| echo '(nothing about mdns on the guest console)'"
        ))

    mdns_state("before resolving")
    resolved = ""
    for _ in range(24):
        rc, out = client.execute(f"avahi-resolve-host-name -4 briard-{flock_name}.local")
        if rc == 0 and vip in out:
            resolved = out
            break
        client.sleep(5)
    if not resolved:
        mdns_state("after the resolve window")
        print("=== what the LAN is actually advertising ===")
        print(client.execute("avahi-browse -art --no-db-lookup 2>&1 | head -40")[1])
    assert vip in resolved, (
        f"briard-{flock_name}.local resolved to {resolved!r}, not to the leased VIP {vip} -- "
        f"a name pointing somewhere the address is not is the V3.19 failure with a new face"
    )
    # And the whole way through: the name a household types actually serves.
    client.wait_until_succeeds(f"curl -fsS http://briard-{flock_name}.local/healthz", timeout=120)
    print(f"off-box client reached http://briard-{flock_name}.local/ by NAME")

    # DELTA 5 [V3b.19]: THE INSTALL HOST REACHES ITS OWN GUEST -- the one machine on this L2 that
    # could not. Everything above is proved from `client`, deliberately, and that is exactly how
    # the defect survived: the substrate isolates a parent NIC from its own children and a switch
    # will not reflect a frame to the port it came from, so the installer handed its user an
    # address and a name that worked everywhere except on the machine reading them.
    #
    # The user-facing addresses are UNCHANGED -- the VIP and the flock name, the same two strings
    # the client just used. The private link is transport and is never published: its address appears
    # in the ROUTE below and nowhere a household would look.
    host.wait_until_succeeds(f"curl -fsS http://{vip}/healthz", timeout=180)
    print(f"the install host reached its own guest at http://{vip}/")

    # THE NODE IP, on a LONE node ([V3b.26b]). A standalone install is a single-node flock, so it
    # is node-id 0 and its guest takes .1 on the system subnet -- assigned HERE, at install, not on
    # a cloud pairing that a free-tier island never has. Before this, eth1 was deliberately left
    # unaddressed single-node, which made the lone node the one shape in the fleet with no address
    # of its own and left the reboot gate with nothing to aim at but a baked private-link constant.
    #
    # Observed from the OFF-BOX client, not asked of the guest agent: the claim is that something
    # outside the guest reaches the guest AT this address, and asking the guest whether it
    # configured itself is asking the actor. The client is the right observer and the host is not
    # -- the system subnet rides the LAN L2, so any other machine reaches it on-link exactly like
    # this, while the host is the one machine macvtap isolates (its own path is the private link's
    # /32, which lands with the rest of the demotion).
    client.succeed(f"ip route replace {system_subnet}.0/24 dev eth1")
    client.wait_until_succeeds(f"ping -c1 -W2 {node_ip}", timeout=60)
    print(f"the lone node holds its node IP at {node_ip}, reached from off-box")

    # ...AND THE HOST REACHES IT TOO, which is the half macvtap otherwise denies. Not over the LAN
    # -- the substrate isolates the host from its own guest -- but over the private link, on a /32
    # the agent installed with a PERMANENT neighbour entry pinning the guest's derived link MAC.
    # Without that entry the host would ARP for a node IP that lives on eth1 while the request
    # arrives on eth3, and arp_ignore=1 ([B.101]) makes the guest answer nothing.
    host.wait_until_succeeds(f"ping -c1 -W2 {node_ip}", timeout=60)
    nroute = host.succeed(f"ip route get {node_ip}")
    print(f"host route to the node IP: {nroute.strip()}")
    assert "briard-priv0" in nroute, f"the host reaches the node IP some other way: {nroute!r}"
    # The entry is `permanent`, not merely present: a reachable/stale one would expire and then
    # re-ARP into the silence above, so the path would die minutes after passing this test.
    neigh = host.succeed(f"ip neigh show {node_ip} dev briard-priv0")
    print(f"host neighbour entry: {neigh.strip()}")
    assert "PERMANENT" in neigh.upper(), f"the neighbour entry is not permanent: {neigh!r}"

    route = host.succeed(f"ip route get {vip}")
    print(f"host route to the VIP: {route.strip()}")
    assert "briard-priv0" in route, f"the host reaches {vip} some other way than the private link: {route!r}"
    # `via` the guest's end, never on-link. The guest answers ARP only on the interface holding the
    # address asked for (arp_ignore=1, [B.101]), so an on-link /32 installs cleanly and then
    # black-holes -- a regression that would look like a working route in every `ip route` listing.
    assert node_ip in route, (
        f"the route to {vip} is not via the guest's NODE IP: {route!r} -- an on-link route cannot "
        f"resolve the VIP's MAC under arp_ignore=1, and the node IP is the one address on that "
        f"link the host can resolve, because the agent pinned a permanent neighbour entry for it"
    )

    # NON-VACUITY, and self-healing, in one move. Take the route away and the host is returned to
    # exactly the state measured on the stranger's desktop; a restarted agent must put it back on
    # its own. Asserting it here rather than racing the agent at boot is what makes the failure
    # REAL: it proves the route is the CAUSE, not merely present alongside the result.
    #
    # The agent is stopped for the negative half deliberately, and not to be tidy -- its observe
    # cadence is 10s, so a reconcile could otherwise land inside the curl's own timeout and turn
    # this into a flake that fails at nobody's fault. Stopping it leaves the GUEST serving (the
    # guest is a detached sibling unit, as the cattle-reset below relies on), so what is measured
    # is the isolation and nothing else.
    host.succeed("systemctl stop briard-agent.service")
    host.succeed(f"ip route del {vip}/32 dev briard-priv0")
    host.fail(f"curl -fsS --max-time 5 http://{vip}/healthz")
    print(f"without the route the host cannot reach {vip} -- the isolation is real")
    host.succeed("systemctl start briard-agent.service")
    host.wait_until_succeeds(f"curl -fsS http://{vip}/healthz", timeout=180)
    print("the agent re-established the route on its own")

    # THE NAME, from the host. Its mDNS query can only have been answered over the private link --
    # the guest's multicast on the macvtap never comes back to this machine -- so this fails if
    # avahi is denied eth3 in the guest, which is what it is here to catch.
    host_resolved = ""
    for _ in range(24):
        rc, out = host.execute(f"avahi-resolve-host-name -4 briard-{flock_name}.local")
        if rc == 0 and vip in out:
            host_resolved = out
            break
        host.sleep(5)
    assert vip in host_resolved, (
        f"from the install host, briard-{flock_name}.local resolved to {host_resolved!r}, not to "
        f"{vip} -- the household's own machine still cannot find the household's own node"
    )
    # And it must resolve to the VIP, NOT to the private address. The name is flock-scoped and
    # survives a failover; the private-link address is node-scoped and does not, so a name pointing at
    # it would be
    # the V3.20 incoherence restored in a place nobody would look for it.
    assert priv_guest_ip not in host_resolved, (
        f"the name resolved to the private link address ({host_resolved!r}) -- transport must "
        f"never become identity"
    )
    host.wait_until_succeeds(f"curl -fsS http://briard-{flock_name}.local/healthz", timeout=120)
    print(f"the install host reached http://briard-{flock_name}.local/ by NAME")

    # The DHCP hostname deliberately does NOT follow the flock name (V3.20 decision 2): option 12
    # stays derived in-guest from the NIC's own MAC, because changing a hostname mid-lease is a
    # change no one can predict an arbitrary server's reaction to, and a rename must never risk the
    # address. Asserted so the divergence stays a DECISION and cannot decay into a bug either way.
    assert name != f"briard-{flock_name}", (
        f"the router's client name {name!r} now tracks the flock name -- that coupling was "
        f"removed on purpose: it puts the address at risk every time someone renames"
    )

    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'role=anchor primary=true quorate=true.*healthy=true'",
        timeout=60,
    )

    # FHS: pet volume under /var/lib, cattle under /opt (the cattle/pet split assertion d builds on).
    host.succeed("test -f /var/lib/briard/data.img")   # pet
    host.succeed("test -f /opt/briard/guest.qcow2")     # cattle overlay
    host.succeed("test -x /opt/briard/qemu/bin/qemu-system-x86_64")
    print(host.succeed("journalctl -u briard-agent | tail -20"))

    # --- assertion (d): cattle/pet reinstall ---
    # The pet (/var/lib/briard) must survive a cattle (/opt/briard) wipe: `rm -rf /opt/briard` +
    # reinstall re-fetches the cattle and reaches green again with the GUEST DATA intact -- proving
    # the guest REATTACHES the existing data volume (blkid-guarded mkfs skipped, create-md without
    # --force refuses to re-seed) rather than reformatting it.
    #
    # The handle is the btrfs FILESYSTEM UUID on the pet volume, read straight off the backing
    # file: mkfs generates a fresh one, so a reformat across the reinstall changes it and an
    # honest reattach cannot. The handle was once the fixture's monotonic tick counter,
    # which was strictly stronger (it proved committed *content* crossed, not just that the
    # filesystem was the same one) -- but the shipped node runs no service, so there is nothing
    # writing data to compare. A runtime service install gets the stronger proof back by installing a service at
    # runtime and resuming the tick comparison on top of this one.
    def fsid(m):
        # btrfs primary superblock at 0x10000: fsid at +0x20, magic ("_BHRfS_M") at +0x40.
        # Asserting the magic keeps this honest -- wrong offsets would otherwise compare two
        # identical blobs of zeroes and "pass".
        magic = m.succeed(
            "dd if=/var/lib/briard/data.img bs=1 skip=65600 count=8 status=none | od -An -c | tr -d ' \\n'"
        ).strip()
        assert magic == "_BHRfS_M", f"no btrfs superblock where expected (read {magic!r})"
        return m.succeed(
            "dd if=/var/lib/briard/data.img bs=1 skip=65568 count=16 status=none | od -An -tx1 | tr -d ' \\n'"
        ).strip()

    pre = fsid(host)
    print(f"pre-wipe data volume fsid={pre}")

    # [V3b.19] The route's WITHDRAWAL is proven in agent-recover, not here, and the reason is worth
    # recording: a withdrawal needs the agent alive to perform it, but an agent alive when its guest
    # unit stops does what it is built to do and RELAUNCHES the guest -- straight into the cattle
    # reset below. (It also leaves nothing to stop: a stopped transient unit is garbage-collected,
    # so naming it again is exit 5. Measured, on the first L0 run of this file.) agent-recover
    # already kills the guest with the agent running and waits out the relaunch, so the assertion
    # belongs where that perturbation already lives.

    # The honest cattle-reset gesture: stop briard (the agent AND its detached guest unit -- the
    # guest runs as a sibling transient service, so stopping the agent alone leaves
    # qemu holding the overlay AND the macvtap chardev), then remove ONLY /opt/briard.
    host.succeed("systemctl stop briard-agent.service briard-guest.service")
    host.succeed("rm -rf /opt/briard")
    host.fail("test -e /opt/briard/qemu/bin/qemu-system-x86_64")  # cattle really gone
    host.succeed("test -f /var/lib/briard/data.img")               # pet survives the wipe
    # The live macvtaps (kernel state) survive the cattle wipe just as the bridge did on the old
    # path -- net-up.sh is gone, but `ip link` state is not owned by /opt. And the host's own
    # address never moved in the first place, so there is nothing to restore.
    host.succeed("ip -d link show briard0 | grep -q macvtap")
    host.succeed("ip -o -4 addr show dev eth1 | grep -qw 192.168.1.1")
    # Non-vacuity for the re-green proof below: with the guest gone the VIP no longer answers.
    client.wait_until_fails(f"curl -fsS --max-time 3 http://{vip}/healthz", timeout=60)
    # [B.106] arm the repair path: an install predating the fix left its macvtaps autoconfiguring,
    # and net-up.sh adopts devices that already exist rather than re-creating them. Put one back the
    # way such a host would have it -- the reinstall below must flush it, which is the whole reason
    # the write sits outside net-up.sh's create branch.
    host.succeed("echo 0 > /proc/sys/net/ipv6/conf/briard0/disable_ipv6")
    host.wait_until_succeeds("ip -6 addr show dev briard0 | grep -q inet6", timeout=30)

    # Reinstall: the SAME one command. It re-lays /opt from staging, recreates a FRESH guest overlay
    # (cattle), and does NOT recreate the pet data.img. net-up.sh is idempotent, so it adopts the
    # macvtaps that are already up rather than re-creating them.
    host.succeed(
        "${channelEnv} BRIARD_NIC=eth1 BRIARD_NET_MODE=macvtap "
        "BRIARD_UNIT_DIR=/run/systemd/system sh ${installScript}"
    )
    host.succeed("test -x /opt/briard/qemu/bin/qemu-system-x86_64")  # cattle re-fetched
    # [B.106] the repair landed on the device that was already up, not just on freshly created ones.
    host.succeed("grep -qx 1 /proc/sys/net/ipv6/conf/briard0/disable_ipv6")
    assert host.succeed("ip -6 addr show dev briard0").strip() == "", (
        "the reinstall adopted briard0 but left it autoconfiguring on the guest's MAC"
    )

    # Green again on the re-fetched bundle: the OFF-BOX client reaches the VIP -- AT THE SAME
    # ADDRESS. The flock id is PET state (/var/lib/briard/flock-id), so it survived the cattle
    # wipe; the service MAC derives from it, the client-id is that MAC, and the router therefore
    # recognises the same client and returns the same lease. That is the chain install.sh promises
    # when it says "keep it to keep your address", and until this test it was only a claim.
    client.wait_until_succeeds(f"curl -fsS http://{vip}/healthz", timeout=600)
    host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64")
    again, _, _, _ = wait_for_lease()
    assert again == vip, f"the address moved across a cattle reinstall ({vip} -> {again}) -- the pet flock id did not carry it"

    # The NAME is pet too, and for the same reason the address is: a household that reinstalls must
    # not find its bookmark dead. node-id additionally MUST survive -- DRBD wrote `on <name>` into
    # the metadata on the pet data volume, so a regenerated one would leave the guest unable to
    # recognise its own replica.
    assert host.succeed("cat /var/lib/briard/flock-name").strip() == flock_name, (
        "the flock name changed across a cattle reinstall -- the name on the LAN is pet state"
    )
    assert host.succeed("cat /var/lib/briard/node-id").strip() == node_id, (
        "the node id was regenerated by a cattle reinstall -- DRBD's `on <name>` is keyed to it, "
        "so the guest would no longer recognise the metadata on the volume it just kept"
    )
    # And the SUBNETS are pet, for a reason the flock name's does not cover ([V3b.26f]): re-running
    # the installer is routine in this alpha, and a re-run that redrew would renumber a live node
    # -- silently, while its peers still hold the old address. The draw happens once, on a machine
    # that has never drawn.
    assert host.succeed("cat /var/lib/briard/subnets") == subnets, (
        f"the subnets were redrawn by a re-run ({subnets.strip()!r} -> "
        f"{host.succeed('cat /var/lib/briard/subnets').strip()!r}) -- a re-run must not renumber a node"
    )
    host.succeed(f"ip route get {node_ip} | grep -q briard-priv0")
    client.wait_until_succeeds(f"curl -fsS http://briard-{flock_name}.local/healthz", timeout=180)
    print(f"the name survived the cattle reinstall: http://briard-{flock_name}.local/")
    print(f"reinstall reached green again on the re-fetched cattle, at the same leased {vip}")

    # THE PROOF (assertion d): the guest re-attached the existing data volume rather than
    # reformatting it -- same filesystem, not a fresh one wearing the same path. A reformat
    # would mint a new fsid, which is the sharp failable control.
    post = fsid(host)
    print(f"post-reinstall data volume fsid={post} (pre-wipe={pre})")
    assert post == pre, f"PET LOST: data volume reformatted across reinstall ({pre} -> {post})"
    print("pet volume survived the cattle wipe: the guest reattached it")

    def restart_node():
        """POWER-CUT the node and bring it back: a fresh guest boot, so promotion runs again and
        briard-vip re-resolves the service address from scratch. The guest is a SIBLING transient
        unit, so stopping the agent alone would leave qemu holding the overlay.

        The SIGKILL is load-bearing, not a shortcut [V3.26a]. What this sets up is the
        unplanned-failover case, and [V3.23]'s entire argument for storing the address is that an
        unplanned failover is, in the field, usually a power cut. Once the guest unit grew an
        ExecStop, a plain `systemctl stop` became a CLEAN shutdown -- which unmounts the volume and
        therefore flushes .vip-address whether or not the product ever fsynced it. Keeping the
        graceful stop here would have left the assertion below green against a reverted V3.23,
        testing the harness's good manners instead of the product's durability."""
        host.succeed("systemctl stop briard-agent.service")
        host.succeed("systemctl kill --signal=SIGKILL briard-guest.service")
        host.wait_until_fails("systemctl is-active briard-guest.service", timeout=60)
        client.wait_until_fails(f"curl -fsS --max-time 3 http://{vip}/healthz", timeout=60)
        host.succeed("systemctl start briard-agent.service")

    # --- COMING BACK MUST NOT DEPEND ON THE HOUSEHOLD ROUTER --------------------------------
    # The design's central claim, and until now only an argument. The address is replicated flock
    # state (.vip-address on the DRBD volume), so promotion APPLIES it rather than ASKING for it.
    # Kill the only DHCP server on the segment, restart the node, and it must come back serving on
    # the same address with nobody to ask.
    #
    # This is the case that matters: an unplanned failover happens exactly when a household's
    # network is least likely to be answering, and a promotion that waited for a DHCP ACK would
    # make the router a dependency of recovery.
    router.succeed("systemctl stop dnsmasq.service")
    router.fail("systemctl is-active dnsmasq.service")   # the segment really has no server now
    restart_node()
    try:
        client.wait_until_succeeds(f"curl -fsS http://{vip}/healthz", timeout=300)
    except Exception:
        diagnose("router-down restart")
        raise
    print(f"came back on {vip} with no DHCP server on the segment -- the flock's stored address carried it")

    # --- AND WHEN THE ANSWER CHANGES, THE NODE FOLLOWS ---------------------------------------
    # The other half: the stored address is a starting point, not a lease we own forever. Bring the
    # router back with a pool that EXCLUDES the address we hold, so it stops being willing to give
    # us that one -- whether it refuses the request outright or simply offers a different address,
    # dhcpcd ends up holding something other than what we applied. That is the trigger the
    # address-changed hook exists for, and the hook has to withdraw ours, take theirs, re-announce
    # it and update the flock's store. One path for every cause; until now only exercised against
    # stubs.
    #
    # Run by hand rather than through the module: the pool has to change, and the module's config
    # is a store path. dhcp-authoritative keeps the server decisive about an address outside it.
    router.succeed(
        "dnsmasq --interface=eth1 --bind-interfaces --port=0 --dhcp-authoritative "
        "--dhcp-range=192.168.1.120,192.168.1.130,2m "
        "--dhcp-leasefile=/tmp/moved.leases --pid-file=/tmp/dnsmasq-moved.pid"
    )
    restart_node()

    def wait_for_moved_lease(timeout=300):
        deadline = time.time() + timeout
        while time.time() < deadline:
            for line in router.succeed("cat /tmp/moved.leases || true").strip().splitlines():
                f = line.split()
                if len(f) >= 5:
                    return f[2], f[3], f[4]
            time.sleep(2)
        raise Exception("the node never took an address from the new pool")

    moved, moved_name, moved_id = wait_for_moved_lease()
    assert moved != vip, f"the node kept {vip}, but that address is no longer leasable"
    assert moved.startswith("192.168.1.12") or moved.startswith("192.168.1.13"), \
        f"{moved} is not from the new pool"
    # Identity is flock-scoped, so it must be UNCHANGED by the address moving underneath it.
    assert moved_name == name and moved_id.lower() == clientid.lower(), (
        f"identity drifted with the address: {moved_name}/{moved_id} was {name}/{clientid}"
    )
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    client.wait_until_fails(f"curl -fsS --max-time 3 http://{vip}/healthz", timeout=60)
    print(f"followed the router from {vip} to {moved}, same identity, and stopped answering at the old one")

    # [V3b.19] The HOST's route follows the address too -- and the stale one goes. Two /32s to one
    # guest would both work, which is exactly why the old one must not survive: it would keep
    # answering long after the household's address had changed, and nothing would notice until a
    # failover made it wrong. This is the re-lease path the reconcile exists for, on a real lease.
    host.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    host.wait_until_fails(f"ip route get {vip} | grep -q briard-priv0", timeout=120)
    host.succeed(f"ip route get {moved} | grep -q briard-priv0")
    print(f"the host route followed the lease from {vip} to {moved}, leaving nothing behind")

    # ---- THE SELF-UPDATE PIVOT, ON THE UNIT install.sh ACTUALLY WROTE [B.84] ---------------
    # agent-selfupdate.nix proves this mechanism on a unit it constructs ITSELF, and that is
    # exactly the gap B.84 named: it stayed green while the SHIPPED install had the Go half
    # switched on (a keyring is bundled, so newSelfUpdater builds a live updater) and none of the
    # on-disk half it acts through. An agent-update staged a binary into a directory the unit did
    # not run from, armed a flag nothing consumed, restarted onto the same binary and reported
    # success -- and, since nothing cleared the flag, did it again every cycle.
    # So these assertions deliberately read the INSTALLED unit and drive the INSTALLED wrappers
    # ([[verification-assertions-must-fail]]: a test that builds its own environment does not
    # prove the shipped one).
    host.succeed("test -x /opt/briard/agent/briard-exec")
    host.succeed("test -x /opt/briard/agent/briard-commit")
    host.succeed("test -x /opt/briard/agent/briard-agent")
    unit = host.succeed("systemctl cat briard-agent.service")
    for want in (
        "Type=notify",
        "ExecStart=/opt/briard/agent/briard-exec",
        "ExecStartPost=/opt/briard/agent/briard-commit",
        # The third leg: the layout the agent stages into must BE the directory ExecStart runs
        # from. Left at its default it was /var/lib/briard while the unit ran out of /opt --
        # so a commit would be a cross-filesystem rename at best and a no-op at worst.
        "Environment=UPDATE_BASE=/opt/briard/agent",
        # A failed trial is by construction a burst of rapid start failures; without this it can
        # trip systemd's start limiter and leave the node down for the one reason self-update
        # exists to avoid.
        "StartLimitIntervalSec=0",
    ):
        assert want in unit, f"the shipped unit is missing {want!r} — self-update has no on-disk half"

    # A GOOD UPDATE COMMITS. The candidate is a copy of the running agent, so it genuinely reaches
    # READY rather than standing in for something that would. The proof is the rename: briard-commit
    # moves .next onto the committed path and clears the marker, so both flags end up gone.
    host.succeed("cp -a /opt/briard/agent/briard-agent /opt/briard/agent/briard-agent.next")
    host.succeed("touch /run/briard/update")
    host.succeed("systemctl restart briard-agent.service")
    host.wait_until_succeeds("test ! -e /opt/briard/agent/briard-agent.next", timeout=120)
    host.succeed("test ! -e /run/briard/trial")
    host.succeed("test ! -e /run/briard/update")
    host.succeed("systemctl is-active briard-agent.service")
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)

    # A BROKEN UPDATE REVERTS, timerlessly. The flag is single-use, so the restart that follows the
    # failed start finds none and falls back to the committed binary. Nothing has to remember to
    # undo anything -- the property that lets the pivot stay frozen.
    host.succeed("sha256sum /opt/briard/agent/briard-agent > /tmp/committed.sha")
    host.succeed(
        "printf '#!/bin/sh\\nexit 1\\n' > /opt/briard/agent/briard-agent.next && "
        "chmod +x /opt/briard/agent/briard-agent.next"
    )
    host.succeed("touch /run/briard/update")
    # The start itself FAILS -- that is the gate doing its job -- so systemctl returns non-zero.
    host.succeed("systemctl restart briard-agent.service || true")
    host.wait_until_succeeds("systemctl is-active briard-agent.service", timeout=180)
    host.succeed("sha256sum -c /tmp/committed.sha")             # came back on the COMMITTED binary
    host.succeed("test -e /opt/briard/agent/briard-agent.next")  # the broken one was NOT committed
    host.succeed("test ! -e /run/briard/trial")                 # and cannot be re-trialled
    host.succeed("test ! -e /run/briard/update")
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    host.succeed("rm -f /opt/briard/agent/briard-agent.next")
    print("the shipped unit commits a good agent update and reverts a broken one")
  '';
}
