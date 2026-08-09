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
{ pkgs, guestDisk, agent, qemuBundle }:
let
  # Same staging as install-bringup, plus the macvtap launch wrapper the substrate needs.
  staging = pkgs.runCommand "briard-install-staging-macvtap" { } ''
    mkdir -p "$out/qemu"
    cp ${agent}/bin/briard-agent "$out/briard-agent"
    cp ${../scripts/briard-net-wrap.sh} "$out/briard-net-wrap"
    cp -r ${qemuBundle}/. "$out/qemu/"
    cp ${guestDisk}/nixos.qcow2 "$out/nixos.qcow2"
  '';
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
        environment.systemPackages = [ pkgs.iproute2 pkgs.iputils pkgs.kmod pkgs.curl ];
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
            f"tr -d '\\r' < /tmp/guest-serial.log 2>/dev/null | grep -aE '{pattern}' | tail -40 || true"
        )

    def diagnose(where):
        # EVERY briard-vip line from EVERY boot, untailed. The console appends across guest
        # incarnations now, so the story spans boots -- and tailing it would keep only the last
        # one, which is the boot that already told us it found nothing. What is wanted is the
        # EARLIER boot, where the address was acquired and should have been remembered.
        print(f"=== {where}: briard-vip, all boots ===")
        print(host.succeed(
            "tr -d '\\r' < /tmp/guest-serial.log 2>/dev/null | grep -a 'briard-vip:' || echo '(no briard-vip lines at all)'"
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

    # Capture the guest's console from the FIRST boot onwards. The host cannot reach the guest over
    # macvtap, so this is the only witness to anything that happens inside it -- and a drop-in laid
    # down before install.sh writes the unit is picked up when it daemon-reloads, so the very first
    # guest is captured rather than only the ones after a restart.
    host.succeed(
        "mkdir -p /run/systemd/system/briard-agent.service.d && "
        "printf '[Service]\\nEnvironment=GUEST_SERIAL=/tmp/guest-serial.log\\n' "
        "> /run/systemd/system/briard-agent.service.d/serial.conf"
    )

    # --- refuse-with-fix / never-a-half-install: a bogus NIC dies before touching networking ---
    # (The report-card refusals themselves are proven in report-card.nix; here we prove the
    # installer aborts cleanly at the networking step, leaving no macvtap behind. Under macvtap
    # that step is not irreversible the way the bridge enslave is -- install-bridge.nix keeps the
    # sharper version of this check -- but "refuses and changes nothing" must hold on the default
    # substrate too, so it is asserted here in the mode's own terms.)
    host.fail(
        "BRIARD_ARTIFACTS=${staging} BRIARD_NET_MODE=macvtap BRIARD_NIC=nope999 sh ${installScript}"
    )
    host.fail("ip link show briard0")       # nothing half-built
    host.fail("ip link show briard-drbd0")
    client.succeed("ping -c1 -W2 192.168.1.1")  # host still on the LAN

    # --- the install on the macvtap substrate: one command -> green ---
    # BRIARD_UNIT_DIR=/run/systemd/system: NixOS's /etc/systemd/system is a read-only store
    # symlink (a stock host's is writable), so the hermetic test drops the units in /run.
    host.succeed(
        "BRIARD_ARTIFACTS=${staging} BRIARD_NIC=eth1 BRIARD_NET_MODE=macvtap "
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

    # What a stranger actually gets. The install ships NO service, so the front door is
    # what answers -- and it says so, rather than the node looking broken or serving a workload
    # nobody chose. This is the assertion that would catch a payload sneaking back into the
    # shipped disk.
    page = client.succeed(f"curl -fsS http://{vip}/")
    assert "No service is installed" in page, f"the VIP served: {page!r}"
    health = client.succeed(f"curl -fsS http://{vip}/healthz")
    assert "no service installed" in health, f"/healthz said: {health!r}"

    # --- V3.20: the NAME, and the three identifiers behind it ---
    # Until this item one string (`guest`) was the API identity, the guest hostname, DRBD's
    # `on <name>` AND the mDNS label, so every install on earth answered to briard-guest.local.
    flock_name = host.succeed("cat /var/lib/briard/flock-name").strip()
    node_id = host.succeed("cat /var/lib/briard/node-id").strip()
    print(f"flock name={flock_name!r} node id={node_id!r}")
    assert node_id.startswith("briard-node-"), f"node id is {node_id!r}"
    assert node_id != "guest", "the node id is still the hardcoded literal this item removed"
    assert len(flock_name.split("-")) == 2, f"the flock name {flock_name!r} is not two words"
    assert flock_name != node_id, "the visible name and the node id are the same string again"

    # THE ASSERTION THE ITEM EXISTS FOR: the name resolves OFF-BOX, from a client using the same
    # mdns4_minimal resolver a household has. Publishing is not resolving -- V3.19d measured a name
    # that published fine and was unlookupable -- so this asks the client, not the guest.
    resolved = client.wait_until_succeeds(
        f"avahi-resolve-host-name -4 briard-{flock_name}.local", timeout=120
    )
    assert vip in resolved, (
        f"briard-{flock_name}.local resolved to {resolved!r}, not to the leased VIP {vip} -- "
        f"a name pointing somewhere the address is not is the V3.19 failure with a new face"
    )
    # And the whole way through: the name a household types actually serves.
    client.wait_until_succeeds(f"curl -fsS http://briard-{flock_name}.local/healthz", timeout=120)
    print(f"off-box client reached http://briard-{flock_name}.local/ by NAME")

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

    # Reinstall: the SAME one command. It re-lays /opt from staging, recreates a FRESH guest overlay
    # (cattle), and does NOT recreate the pet data.img. net-up.sh is idempotent, so it adopts the
    # macvtaps that are already up rather than re-creating them.
    host.succeed(
        "BRIARD_ARTIFACTS=${staging} BRIARD_NIC=eth1 BRIARD_NET_MODE=macvtap "
        "BRIARD_UNIT_DIR=/run/systemd/system sh ${installScript}"
    )
    host.succeed("test -x /opt/briard/qemu/bin/qemu-system-x86_64")  # cattle re-fetched

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
        """Stop briard and bring it back: a fresh guest boot, so promotion runs again and
        briard-vip re-resolves the service address from scratch. The guest is a SIBLING transient
        unit, so stopping the agent alone would leave qemu holding the overlay."""
        host.succeed("systemctl stop briard-agent.service briard-guest.service")
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
  '';
}
