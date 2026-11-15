# The test that owns the whole install chain end to end: report-card gate -> bundled qemu (no
# distro qemu on the host) boots the guest -> the data volume (a lone node runs no DRBD) -> app
# served at the VIP -> an off-box LAN client reaches it. It carries the mode-independent half;
# install-bridge.nix is a pure delta over it, so nothing mode-independent depends on the bridge rig.
# agent-bringup.nix proves the agent mechanism (nested guest, DRBD, VIP) with the host as client;
# this proves the installer around it: scripts/install.sh run verbatim, only the bundled qemu
# (pkgs.qemu deliberately absent), and a second node on the shared L2 curls the VIP, so
# reachability is genuinely off-box.
#
# The macvtap deltas:
#   1. no bridge, and the host IP never leaves the physical NIC (the invasiveness win: no SSH-risk
#      moment, no net guard);
#   2. the guest NICs are macvtap children of the host NIC (L2 citizens);
#   3. qemu runs behind the fd-passing wrapper (briard-net-wrap) and really holds the
#      /dev/tap<ifindex> chardevs on the inherited fds -- the mechanism ifname= can not provide;
#   4. an off-box LAN client reaches the app at the VIP through the macvtap -- the whole point;
#   5. so does the install host, by VIP and by name, over the private link: the one machine
#      macvtap would otherwise hide the household apps from.
#
# Also assertion (d), cattle/pet reinstall: after green, `rm -rf /opt/briard` (the cattle) +
# reinstall reaches green again with the guest data intact -- the pet /var/lib/briard data volume
# LUKS2 header UUID, read off the backing file, survives; a wipe or reformat would mint a new one.
# The cattle/pet block below says why no app tick counter backs it up.
#
# Heavy (many nested guest boots + a multi-GB guest disk) -> rides the `install` nightly tag
# alongside install-bridge, qemu-bundle + report-card. Run:
#   nix build .#tests.install-macvtap -L
{ pkgs, guestDisk, agent, qemuBundle, guestBundle, selfupdateStub, catalog }:
let
  # The release channel, built the way a release is built (mirroring scripts/publish-release.sh),
  # not a BRIARD_ARTIFACTS dir of loose files: the installer's first act on a stranger's machine
  # is to fetch a signed, compressed set over HTTP and verify it before touching the disk, and an
  # escape hatch that skips that proves a shape nothing ships. The manifest is written by the real
  # writer (`briard-agent --stage-manifest`), the binary that reads it back below, so a format
  # change that breaks the round trip fails here, not at a first install. The layout is the tree
  # the channel publishes: one directory per chain, one versioned directory under each; the
  # pointers (`stable`, `latest`) are laid at runtime beside the signatures, because a pointer is
  # a byte-copy of a signed manifest.
  channel = pkgs.runCommand "briard-test-channel" {
    nativeBuildInputs = [ pkgs.zstd pkgs.openssl pkgs.gnutar ];
  } ''
    V=${agent.version}; GV="vm.''${V#*.}"
    H="$out/briard/$V/linux-amd64"; G="$out/vm/$GV/amd64"
    mkdir -p "$H" "$G"
    install -m0755 ${agent}/bin/briard-agent        "$H/briard-agent"
    install -m0755 ${../scripts/briard-net-wrap.sh} "$H/briard-net-wrap"
    # The units, shipped verbatim as ordinary artifacts: hashed by the manifest, covered by the
    # signature, and copied by the install out of the verified set rather than rendered -- the
    # property this channel exists to exercise.
    install -m0644 ${../scripts/units/briard-agent.service}  "$H/briard-agent.service"
    install -m0644 ${../scripts/units/briard-update.service} "$H/briard-update.service"
    install -m0644 ${../scripts/units/briard-update.timer}   "$H/briard-update.timer"
    install -m0755 ${../scripts/agent/briard-update}  "$H/briard-update"
    # Deterministic tar, same flags as the release script: the bundle is a directory in the store
    # and the channel contract wants one file.
    tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner \
        -cf qemu-bundle.tar -C ${qemuBundle} .
    zstd -19 -q --rm qemu-bundle.tar -o "$H/qemu-bundle.tar.zst"
    # The guest bundle: the guest's briard binaries, in the briard chain, pushed into the
    # guest at bring-up. Same shape as the qemu bundle.
    tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner \
        -cf guest-bundle.tar -C ${guestBundle} .
    zstd -19 -q --rm guest-bundle.tar -o "$H/guest-bundle.tar.zst"
    zstd -19 -q ${guestDisk}/nixos.qcow2 -o "$G/nixos.qcow2.zst"
    chmod 0644 "$H"/*.zst "$G"/*.zst
    # The production writer, not a re-implementation in Nix -- which would have tested this file
    # against itself and proven nothing about what a release actually publishes.
    "$H/briard-agent" --stage-manifest "$H" --chain briard --platform linux-amd64 --release "$V" --vm "$GV"
    # The guest manifest names the closure the image boots and the oldest host that tolerates
    # it -- this release's own, as publish-release.sh stamps it.
    "$H/briard-agent" --stage-manifest "$G" --chain vm --platform amd64 --release "$GV" \
        --system ${guestDisk.system} --min-briard "$V"
  '';
  # ⚠️ The channel is signed at runtime, not here, by constraint: a committed signing key trips
  # `TestNoSecretMaterial` (internal/arch), the ★ guard refusing a PRIVATE KEY block anywhere in
  # the tree -- rightly, since a throwaway test key is still a private key in a public repo, and
  # the guard cannot tell the difference and should not try. So the test mints a keypair in the
  # VM and serves a symlink farm over the store: the channel is ~400 MB and only the 64-byte
  # signature must be writable, so a copy would cost half a gigabyte of VM disk.
  #
  # channelEnv: what install.sh needs to reach the channel -- where it lives and the release
  # public key (the two the shipped one-liner sets) -- plus the catalog URL, which the one-liner
  # leaves at its default. The keyring is minted into /root at test start (the signing step in the
  # script); the catalog served is the real list, signed with the same key, with no version file
  # beside it: the page offers what the list says is installable, and the install that follows
  # fails closed on the manifest it cannot get.
  channelEnv = "BRIARD_CHANNEL_URL=http://127.0.0.1:8099 BRIARD_UPDATE_KEYRING=/root/keyring.pem BRIARD_CATALOG_URL=http://127.0.0.1:8099/catalog/latest.json";
  catalogIndex = "${catalog}/catalog/latest.json";
  installScript = ../scripts/install.sh;
in
pkgs.testers.runNixOSTest {
  name = "install-macvtap";
  skipTypeCheck = true; # dynamic asserts, systemd units created at runtime

  nodes = {
    host =
      { ... }:
      {
        virtualisation.memorySize = 6144; # report-card 4 GB floor + room for the nested guest
        virtualisation.cores = 4;
        # 14 GB, so the test does not measure the admission floor by accident: the report card
        # refuses below what the install uses (6 GB fresh: the image, a 4 GB data volume, a 1 GB
        # state disk, all thick) and warns below that plus a Home Assistant and advised free space.
        virtualisation.diskSize = 14336;
        # eth1 on the shared 192.168.1.0/24 L2 (the LAN); eth2 a second uplink on the same LAN, held
        # down until the carrier re-parent block near the end.
        virtualisation.vlans = [ 1 1 ];
        virtualisation.qemu.options = [ "-cpu" "host" ]; # vmx -> nested KVM in L1
        networking.useDHCP = false;
        networking.interfaces.eth1.ipv4.addresses = [
          { address = "192.168.1.1"; prefixLength = 24; }
        ];
        # ⚠️ The default route this rig needs is added by the test script (see its top for why),
        # not declared here: `networking.defaultGateway` looks right and does nothing -- on these
        # VMs no `network-setup.service` line reaches the boot log -- and the only symptom is the
        # report card refusing with "no default route" a hundred lines later, reading as a product
        # bug. An `ip route add` in the script is visible, asserts itself, and cannot fail quietly.
        # IPv6 off on the install host, permanently and on purpose: a stranger may have disabled v6
        # (their machine, their setting), and our addressing is v4 indefinitely, so nothing we ship
        # may quietly need v6. The failure it guards: avahi joins the IPv4 group only on an
        # interface with a v4 address, so an unnumbered private link answers mDNS over IPv6 only and
        # the name half silently depends on the host holding a v6 link-local on the tap. With v6
        # on the name resolves and everything looks correct; with v6 off the household machine
        # cannot find its own node while every other assertion passes. A rig with v6 on cannot
        # tell those apart.
        boot.kernel.sysctl."net.ipv6.conf.all.disable_ipv6" = 1;
        boot.kernel.sysctl."net.ipv6.conf.default.disable_ipv6" = 1;
        # tar + zstd: the host-bundle section below re-publishes the qemu bundle with one file added,
        # the way the release script builds it, and the shipped update verb unpacks it with tar(1).
        environment.systemPackages = [ pkgs.iproute2 pkgs.iputils pkgs.kmod pkgs.curl pkgs.avahi pkgs.gnutar pkgs.zstd pkgs.e2fsprogs ];
        # An mDNS resolver on the install host, as on a desktop install (where the name half was
        # measured), to make a dependency visible rather than flatter the result: a host that
        # speaks no mDNS resolves no .local name from anywhere, with or without us. nssmdns4 as
        # well as the daemon, because the gesture is a person typing the name into a browser, which
        # goes through nsswitch. The assertions use both: avahi-resolve-host-name proves the query
        # reaches the guest over the private link and is answered there; a curl by name proves the
        # household gesture works.
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
        # tcpdump is for one assertion, a premise rather than a diagnostic: the guest must say
        # nothing unsolicited, or the cold-cache resolve below can be answered by a multicast
        # nobody asked for.
        environment.systemPackages = [ pkgs.curl pkgs.iputils pkgs.avahi pkgs.tcpdump ];
        # An mDNS resolver, so the name is exercised the way a household uses it, not by reading
        # our config back -- the failure guarded is a name that publishes fine and resolves
        # nowhere. nssmdns4 puts `mdns4_minimal` into nsswitch: the resolver measured on the real
        # Ubuntu desktop, and the reason the published name is single-label.
        services.avahi = { enable = true; nssmdns4 = true; };
      };

    # The router, so the service address comes from DHCP (the product default), not from an
    # operator-named address.
    # A pool, deliberately with no reservation for our MAC: "we never depended on knowing the
    # address" is the property under test, so the test discovers it as a household would, from the
    # router lease table. The range starts at .100, clear of the driver-assigned statics (host .1,
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
        # On PATH so the test can run a second server by hand with a different pool under a
        # running node -- the module config is a store path.
        environment.systemPackages = [ pkgs.dnsmasq ];
        services.dnsmasq = {
          enable = true;
          settings = {
            interface = "eth1";
            bind-interfaces = true;
            # DHCP only. port=0 disables the DNS half: the guest resolves over its own WAN path
            # (eth0, SLIRP), and a resolver here would be one more thing to explain if it broke.
            port = 0;
            # A two-minute lease, not the household 12h: a client with a valid lease does not talk
            # to a server until T1 (half the term), so with a realistic term the node would never
            # notice a pool change within the test -- DHCP working, not a product limit. The short
            # term makes the renewal path reachable, and makes the address genuinely expire while
            # the router is stopped, so --lastleaseextend becomes an assertion, not an argument.
            dhcp-range = "192.168.1.100,192.168.1.150,2m";
            dhcp-authoritative = true;
          };
        };
      };
  };

  testScript = ''
    import time
    ${import ./multicast-receive.nix { inherit pkgs; }}

    # The guest's address is the router's to know, so the test reads it where a household would:
    # (ip, mac, hostname, client-id) from dnsmasq's lease table (<expiry> <mac> <ip> <hostname>
    # <client-id>). It waits because the lease is drawn at promotion, not install: the guest boots
    # and promotes first. (The installer waits for the agent's healthy line before printing the
    # link, but bounded, so the lease is still not implied.)
    def wait_for_lease(timeout=180):
        deadline = time.time() + timeout
        while time.time() < deadline:
            leases = router.succeed("cat /var/lib/dnsmasq/dnsmasq.leases || true")
            for line in leases.strip().splitlines():
                f = line.split()
                # Any lease returns: the caller asserts the hostname, so a missing one fails as
                # "the router recorded no name", not as a timeout saying the guest never asked.
                if len(f) >= 5:
                    return f[2], f[1], f[3], f[4]
            time.sleep(2)
        # Diagnose here too: "no lease" is the least informative failure in this test (never
        # booted, never promoted, never asked, asked into silence), and only the guest console can
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
        # Every briard-vip line from every boot, untailed: the console appends across guest
        # incarnations, and a tail would keep only the last boot, which already said it found
        # nothing; what is wanted is the earlier boot that acquired the address and should have
        # remembered it.
        print(f"=== {where}: briard-vip, all boots ===")
        print(host.succeed(
            "tr -d '\\r' < /var/log/briard-guest-console.log 2>/dev/null | grep -a 'briard-vip:' || echo '(no briard-vip lines at all)'"
        ))
        print(f"=== {where}: what the guest said about its address ===")
        print(guest_console("briard-vip|dhcpcd|briard-primary-storage|eth0|eth2|Failed|error"))
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

    # A default route on the install host, via the router that serves this LAN DHCP, as a real
    # household host has. A precondition twice over: the agent selects the device holding the main
    # table default route, so without one every install here would pin BRIARD_NIC and the shipped
    # selection would never run; and the LAN fingerprint gateway MAC, the strong signal the
    # re-parent tiers are paced by, comes from it, so without it the re-parent block could never
    # reach the same-wire tier. Asserted at once: a missing route surfaces a hundred lines later
    # as the report card refusing, which reads as a product bug, not a rig one.
    host.succeed("ip route replace default via 192.168.1.3 dev eth1")
    host.succeed("ip -o -4 route show default | grep -qw eth1")
    # The second uplink stays admin-down until the carrier block below: a NIC with a link and no
    # default route holds every cold start's choice for a settle window, and nothing before that
    # block is about two uplinks.
    host.succeed("ip link set eth2 down")

    # The guest console is the only witness to anything inside the guest (the host cannot reach it
    # over macvtap), so the agent captures it by default (GUEST_SERIAL, agent/host/config.go) and
    # this test lays down no capture of its own: a rig-only capture would be a vacuous green,
    # granting a witness a real install might lack. Reading the shipped default path makes the
    # capture a tested property, not a local convenience.

    # --- refuse-with-fix / never-a-half-install: a bogus NIC dies before touching networking ---
    # (report-card.nix proves the card refusals; here the installer aborts cleanly on a bogus NIC,
    # refused by the card before anything is fetched, leaving no macvtap. Under macvtap that step
    # is not irreversible like the bridge enslave -- install-bridge.nix keeps the sharper check --
    # but "refuses and changes nothing" must hold on the default substrate too.)
    # The release channel, served over HTTP for the rest of the test: every install goes the real
    # network path a stranger's machine does -- signed manifest verified against the keyring, each
    # artifact hash verified, compressed ones expanded. Signing does not re-serialise the manifest
    # the channel derivation wrote, so the agent verifies exactly what `--stage-manifest` emitted.
    stub = "${selfupdateStub}/bin/briard-selfupdate-stub"
    host.succeed(f"{stub} keygen /root/release.key /root/keyring.pem")
    # A symlink farm over the read-only store, one versioned directory per chain; only signatures
    # and pointers are real files. A pointer is what `publish-release.sh latest`/`promote` lay: a
    # byte-copy of the signed manifest, plus the bootstrap agent on the briard chain (install.sh
    # curls it from the pointer path).
    V = "${agent.version}"
    GV = "vm." + V.split(".", 1)[1]
    # Every chain carries the platform level (linux-amd64 on briard, amd64 on vm). Both pointers
    # name this one release: install.sh and `briard update` follow `stable`, and the shipped update
    # unit is exercised against both below.
    for chain, ver, arm in (("briard", V + "/linux-amd64", "/linux-amd64"), ("vm", GV + "/amd64", "/amd64")):
        d = f"/srv/{chain}/{ver}"
        host.succeed(f"mkdir -p {d} && ln -sf ${channel}/{chain}/{ver}/* {d}/")
        host.succeed(f"{stub} sign /root/release.key {d}/manifest.json | base64 -d > {d}/manifest.json.sig")
        host.succeed(f"test -s {d}/manifest.json.sig")
        for ptr in ("stable", "latest"):
            host.succeed(f"mkdir -p /srv/{chain}/{ptr}{arm} && cp {d}/manifest.json {d}/manifest.json.sig /srv/{chain}/{ptr}{arm}/")
    for ptr in ("stable", "latest"):
        host.succeed(f"ln -sf ${channel}/briard/{V}/linux-amd64/briard-agent /srv/briard/{ptr}/linux-amd64/briard-agent")
    # The catalog's list, as published: the real one, signed here with the rig's key. No
    # version file is served beside it (see channelEnv).
    host.succeed("mkdir -p /srv/catalog && cp ${catalogIndex} /srv/catalog/latest.json")
    host.succeed(f"{stub} sign /root/release.key /srv/catalog/latest.json | base64 -d > /srv/catalog/latest.json.sig")

    host.succeed(
        f"systemd-run --unit=briard-channel --collect {stub} serve 127.0.0.1:8099 /srv"
    )
    host.wait_until_succeeds("curl -sf http://127.0.0.1:8099/briard/stable/linux-amd64/manifest.json -o /dev/null", timeout=30)
    # Assert the shape before relying on it -- manifests signed, artifacts compressed and under the
    # versioned directories only -- so a channel of loose plaintext files, or one with a second
    # image under a pointer, cannot pass as green.
    host.succeed("curl -sf http://127.0.0.1:8099/briard/stable/linux-amd64/manifest.json.sig -o /dev/null")
    host.succeed("curl -sf http://127.0.0.1:8099/briard/stable/linux-amd64/briard-agent -o /dev/null")
    host.succeed("curl -sf http://127.0.0.1:8099/vm/stable/amd64/manifest.json.sig -o /dev/null")
    host.succeed(f"curl -sf http://127.0.0.1:8099/vm/{GV}/amd64/nixos.qcow2.zst -o /dev/null")
    host.fail(f"curl -sf http://127.0.0.1:8099/vm/{GV}/amd64/nixos.qcow2 -o /dev/null")
    host.fail("curl -sf http://127.0.0.1:8099/vm/stable/amd64/nixos.qcow2.zst -o /dev/null")

    # The host footprint, for the uninstall residue check at the end: every place install.sh or
    # the agent can leave something (files, units, links, addresses, routes), one entry per line,
    # compared as sets against this baseline. Taken before the refused installs below, which leave
    # empty dirs. Routes keep only destination, device and table: expiry and linkdown come and go.
    def footprint():
        return set(host.succeed(
            "for d in /opt /var/lib /var/lib/systemd/timers /var/log /usr/local/bin /run "
            "/run/systemd/system /run/systemd/transient; do find $d -mindepth 1 -maxdepth 1 2>/dev/null || true; done; "
            "find /run/systemd/system -mindepth 2 -maxdepth 2 2>/dev/null || true; "
            "ip -o link show | awk -F': ' '{print \"link \" $2}' | cut -d@ -f1; "
            "ip -o addr show | awk '{print \"addr \" $2 \" \" $4}'; "
            "ip route show table all | awk '{r=$1\" \"$2; for (i=2; i<NF; i++) "
            "if ($i==\"dev\" || $i==\"table\") r=r\" \"$(i+1); print \"route \" r}'; "
            "systemctl list-units --all --plain --no-legend 'briard*' | awk '{print \"unit \" $1}'"
        ).splitlines())
    pristine = footprint()

    host.fail(
        "${channelEnv} BRIARD_NIC=nope999 sh ${installScript}"
    )
    host.fail("ip link show briard0")       # nothing half-built
    host.fail("ip link show briard-drbd0")
    client.succeed("ping -c1 -W2 192.168.1.1")  # host still on the LAN

    # ---- The macvtap probe refuses a device that cannot carry one -----------------------------
    # The failure guarded: a device that exists, is up, holds an address and still cannot parent a
    # macvtap -- a laptop on a full-tunnel VPN hands us tun0 as its default route. A tun device is
    # the honest stand-in (ARPHRD_NONE, so macvlan refuses it): the refusal must come from the
    # kernel, not a name we blacklisted. Without the probe the install completes and leaves a guest
    # that boots, reports healthy and answers nobody; so it must refuse before writing -- no taps --
    # and its message names the device, the reason and the way out (the setting, not an example
    # device: the selection could not vouch for one).
    host.succeed("ip tuntap add briard-vpn0 mode tun && ip link set briard-vpn0 up")
    out = host.fail(
        "${channelEnv} BRIARD_NIC=briard-vpn0 sh ${installScript} 2>&1"
    )
    for want in ("briard-vpn0", "macvtap could not be created", "BRIARD_NIC"):
        assert want in out, f"the refusal is the whole safety margin and it is missing {want!r}: {out}"
    host.fail("ip link show briard0")
    host.fail("ip link show briard-drbd0")
    host.fail("ip link show briard-probe0")  # the probe cleans up after itself
    host.succeed("ip link del briard-vpn0")

    # --- the install on the macvtap substrate: one command -> green ---
    # ⚠️ No BRIARD_NIC: the device is selected from the main table default route, so this install
    # exercises the shipped path rather than being told the answer (the negative cases above keep
    # the override, because naming a device is what they test). BRIARD_UNIT_DIR: NixOS's
    # /etc/systemd/system is a read-only store symlink (a stock host's is writable), so the units
    # go in /run.
    install_out = host.succeed(
        "${channelEnv} "
        "BRIARD_UNIT_DIR=/run/systemd/system sh ${installScript}"
    )
    # The installer ends with the link: it waited for the agent's own healthy line and minted once,
    # so its last lines carry a one-time dashboard link at the bare `briard.local` (the alias every
    # flock answers beside its own name, and the one line a household reads), not merely the verb
    # that would get one. Redeemed below once the VIP is leased (the name does not resolve from the
    # client, so the reverse proxy is reached by address under the link name, as a browser with
    # mDNS would). It is the only address printed: an "it will answer at" line would name a node
    # still booting.
    import re
    m = re.search(r"http://(briard\.local)/\?code=([0-9a-f]{64})", install_out)
    assert m, f"the installer printed no one-time link; its closing lines: {install_out.strip().splitlines()[-6:]}"
    install_link_host, install_code = m.group(1), m.group(2)
    assert "sudo briard open" in install_out, "the installer no longer says how to get another link"
    assert "it will answer at" not in install_out, "the installer promised an address before the node answered"
    # The footprint sees an install, one entry of every kind it scans, or the residue check at the
    # end would compare two scans blind to the thing it is about.
    installed = footprint() - pristine
    for want in ("/opt/briard", "/var/lib/briard", "/usr/local/bin/briard", "/run/briard",
                 "/var/log/briard-guest-console.log", "/run/systemd/system/briard-agent.service",
                 "/run/systemd/transient/briard-guest.service", "link briard0", "link briard-priv0",
                 "unit briard-guest.service"):
        assert want in installed, f"the footprint cannot see {want!r}, so it cannot prove it gone: {sorted(installed)}"
    assert any(e.startswith("addr briard-priv0 ") for e in installed), sorted(installed)
    assert any(e.startswith("route ") and " briard-priv0" in e for e in installed), sorted(installed)
    # ⚠️ The closing block is the agent's own words: install.sh prints what `briard open` said
    # rather than re-rendering it in shell, so the "what is on this node" sentence lives in
    # versioned Go. The exact phrasing is the assertion, so a shell-rendered block (say, "open this
    # on any device on your network") fails here rather than reading identically to a human.
    assert "open this in a browser within" in install_out, (
        f"the installer re-renders the dashboard block instead of printing the verb's output; "
        f"its closing lines: {install_out.strip().splitlines()[-6:]}"
    )

    # DELTA 1: no bridge, and the host IP never left eth1 (macvtap's invasiveness win).
    host.fail("ip link show br-briard")
    host.succeed("ip -o -4 addr show dev eth1 | grep -qw 192.168.1.1")
    client.succeed("ping -c1 -W2 192.168.1.1")  # host never lost its footing

    # DELTA 2: the guest's NICs are macvtap children of eth1 (not taps on a bridge).
    host.succeed("ip -d link show briard-drbd0 | grep -q macvtap")
    host.succeed("ip -d link show briard0 | grep -q macvtap")
    host.succeed("ip -o link show briard-drbd0 | grep -q 'briard-drbd0@eth1'")

    # DELTA 2b: the host end of each macvtap holds no IPv6 address. The device carries the MAC
    # the wrapper pins, the guest MAC, so an autoconfiguring host derives the same EUI-64 as the
    # guest on the same L2 and its avahi joins mDNS on the guest segment; a link-local needs no
    # advertisement, only the device coming up (measured on real hardware). On this rig, though,
    # the host sets net.ipv6.conf.default.disable_ipv6=1, so new devices start with IPv6 off and
    # this half passes without the agent procfs write (nic.ensureMacvtap); the repair block at the
    # cattle reset below is the one that fails without it.
    for dev in ("briard0", "briard-drbd0"):
        host.succeed(f"grep -qx 1 /proc/sys/net/ipv6/conf/{dev}/disable_ipv6")
        assert host.succeed(f"ip -6 addr show dev {dev}").strip() == "", (
            f"{dev} holds an IPv6 address -- the host is speaking on the guest's identity"
        )

    # The guest converges on the bundled qemu, launched behind the fd-passing wrapper.
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q CONVERGED", timeout=900)
    host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64")

    # DELTA 3: the guest unit was started through briard-net-wrap, and qemu really holds the
    # macvtap chardevs on the inherited fds — the fd-passing mechanism ifname= cannot provide.
    host.succeed("systemctl show briard-guest.service -p ExecStart | grep -q briard-net-wrap")
    qpid = host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64").strip().split()[0]
    fds = host.succeed(f"ls -l /proc/{qpid}/fd/")
    print("qemu fds:\n" + fds)
    assert "/dev/tap" in fds, "qemu is not holding any /dev/tap<ifindex> chardev (fd-passing failed)"

    # DELTA 4 (the proof): the off-box client reaches Briard at the VIP through the macvtap, at an
    # address nobody in this test chose. BRIARD_VIP_ADDR is unset, so the router decided, and the
    # test reads its lease table as a household would. That is the whole point: a test that knows
    # the address in advance cannot tell "we acquired one" from "we claimed the one we always
    # claimed" -- exactly the confusion the baked-default VIP was.
    vip, mac, name, clientid = wait_for_lease()
    print(f"the router leased the guest {vip} (mac={mac} name={name} client-id={clientid})")

    # The dhcpcd flags, confirmed on the wire, the only place they can be: -h gives the household a
    # recognisable router client-list entry (and something to pin a reservation against);
    # -I 01:<mac> makes the client-id the hardware address, so one flock presents one identity and
    # the lease survives a failover.
    assert name.startswith("briard-"), f"the router recorded the client as {name!r}, not briard-*"
    assert clientid.lower() == "01:" + mac.lower(), (
        f"client-id is {clientid!r}, not the hardware address -- a per-host DUID here is what "
        f"would give the two nodes of one flock different leases"
    )

    client.wait_until_succeeds(f"curl -fsS http://{vip}/healthz", timeout=120)
    print("off-box client reached the leased VIP over the macvtap substrate")

    # The console capture itself, asserted rather than assumed: every helper reads it through
    # `2>/dev/null ... || true` (a diagnostic must not fail a test), so a missing file looks like a
    # quiet boot and the capture could silently stop shipping. This fails if the agent stops
    # capturing by default (GUEST_SERIAL). Non-empty and recognisable: a file qemu merely created
    # proves the env var arrived, not that the guest console is on the other end; the kernel
    # banner is the cheapest thing only a real ttyS0 stream contains.
    host.succeed("test -s /var/log/briard-guest-console.log")
    host.succeed(
        "tr -d '\\r' < /var/log/briard-guest-console.log | grep -aqE 'Linux version|NixOS|systemd'"
    )
    # Not world-readable: it carries the household hostnames and addresses. The agent sets the mode
    # at every launch before qemu opens the file, so an existing world-readable one is tightened.
    mode = host.succeed("stat -c%a /var/log/briard-guest-console.log").strip()
    assert mode in ("600", "640"), f"the guest console is mode {mode} -- readable beyond root"
    print("the INSTALLER's own guest-console capture is live, non-empty and not world-readable")

    # ...and the operator can reach it by the verb, a different claim from the file being right.
    # `briard logs` asks systemd where config.env is (BRIARD_CONFIG on the unit) and config.env
    # where the console is (GUEST_SERIAL, defaulted when absent); asking systemd for GUEST_SERIAL
    # directly would tell every node it captured no console while the capture sat here. The unit
    # test stubs the lookup with an answer it wrote itself, so only a real install proves it.
    # Asserted on content, not exit status: the verb reports an unreadable surface without
    # failing, by design, so a status check would pass on exactly this bug.
    logs = host.succeed("/opt/briard/agent/briard-agent logs -guest -n 40")
    assert "does not capture the guest console" not in logs, (
        f"`briard logs` cannot find this node's console:\n{logs}"
    )
    assert "guessed" not in logs, f"`briard logs` fell back to guessing the path:\n{logs}"
    assert "/var/log/briard-guest-console.log" in logs, (
        f"`briard logs` does not name the console it read:\n{logs}"
    )
    print("`briard logs` resolves the console through config.env on a real install")

    # `briard doctor` end to end: the local-only directive through the real socket, the
    # agent's own snapshot, the guest's answer. Asserted per line, on a lone node that serves --
    # not on the exit status, which the rig's clock (no time server here) and disk may move. The
    # checks named are the agent's half, which a broken socket, dispatch or guest would lose.
    doctor = host.execute("/opt/briard/agent/briard-agent doctor")[1]
    print(doctor)
    for check in ("agent", "network", "guest", "role", "address"):
        assert re.search(rf"^\[OK  \] {check} ", doctor, re.M), (
            f"`briard doctor` does not pass {check} on a healthy lone node:\n{doctor}"
        )
    print("`briard doctor` judges a real serving node through the agent")
    # ...and says which briard it is about, as `briard version` does: the stamp this release was
    # built with and the VM release the install recorded -- both, or a pasted report is unplaceable.
    versions = f"briard {V}\nvm     {GV}\n"
    assert versions in doctor, f"`briard doctor` does not name {versions!r}:\n{doctor}"
    assert host.succeed("/usr/local/bin/briard version") == versions

    # What a stranger gets: the bare address is a name the reverse proxy does not route, so it
    # forwards to the dashboard, which refuses a browser with no session and names nothing (401,
    # one instruction, no inventory) -- reaching the VIP is not authentication. The install ships
    # no app, and /healthz says so: the assertion that catches an app sneaking back into the disk.
    refused = client.succeed(f"curl -sS -o /tmp/vip.html -w '%{{http_code}}' http://{vip}/").strip()
    page = client.succeed("cat /tmp/vip.html")
    assert refused == "401" and "not yet trusted" in page, f"the VIP served {refused}: {page!r}"
    assert "home-assistant" not in page and "Apps on this machine" not in page, f"the refusal names the inventory: {page!r}"
    health = client.succeed(f"curl -fsS http://{vip}/healthz")
    assert "no apps routed" in health, f"/healthz said: {health!r}"
    # The installer link, redeemed as the first browser would, under the name it carries (the one
    # the "installed." line printed): a 303 home and an HttpOnly session cookie, and the same code
    # refused the second time.
    assert f"http://{install_link_host}/" in install_out, f"the link's name {install_link_host} is not the one the installer printed"
    redeemed = client.succeed(
        f"curl -sS -D - -o /dev/null -H 'Host: {install_link_host}' 'http://{vip}/?code={install_code}'"
    )
    assert " 303 " in redeemed.splitlines()[0] and "briard_session=" in redeemed.lower() and "httponly" in redeemed.lower(), redeemed
    client.fail(f"curl -fsS -o /dev/null -H 'Host: {install_link_host}' 'http://{vip}/?code={install_code}'")
    print(f"the installer's link redeemed once at {install_link_host}")

    # The guest admin port, end to end: the trusted browser presses "Install Home Assistant", the
    # dashboard relays an app-install directive over the second virtio-serial port, and the host
    # agent dispatches it as it would the CLI one. The catalog is the signed list with no version
    # file, so the install fails -- and that is the measurement: the page must carry the host
    # refusal back, proving the whole wire (device -> qemu -> host acceptor -> dispatch -> outcome
    # -> device -> page), not a page saying "installing" forever. (The alert list copy across the
    # channel is measured where this rig opens an alert: "a node that has stopped updating" below.)
    # The page offers nothing until the host has read the index (its catalog watcher's first read)
    # and pushed the list to the guest, so that is waited for, not assumed.
    session = [l.split(":", 1)[1].split(";")[0].strip() for l in redeemed.splitlines() if l.lower().startswith("set-cookie: briard_session=")][0]
    def node_page_offering():
        p = client.succeed(f"curl -fsS -H 'Host: {install_link_host}' -H 'Cookie: {session}' http://{vip}/")
        return p if 'action="/install"' in p and 'name="app" value="home-assistant" checked' in p else None
    deadline = time.time() + 120
    node_page = None
    while time.time() < deadline and node_page is None:
        node_page = node_page_offering()
        if node_page is None:
            time.sleep(5)
    assert node_page, "the page never offered Home Assistant: the host did not read the index, or the list did not reach the guest"
    host.succeed("journalctl -u briard-agent | grep -q 'catalog: index read: '")
    host.succeed("test -S /run/briard-admin.sock")  # qemu serves the host end
    # The chooser's one Install, with Home Assistant ticked -- the form the household submits.
    pressed = client.succeed(
        f"curl -sS -o /dev/null -w '%{{http_code}}' -X POST -H 'Host: {install_link_host}' -H 'Cookie: {session}' "
        f"--data-urlencode app=home-assistant http://{vip}/install"
    ).strip()
    assert pressed == "303", f"the button answered {pressed}"
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'admin port: directive kind=app-install submitted by the guest'", timeout=30)
    def outcome_on_page():
        p = client.succeed(f"curl -fsS -H 'Host: {install_link_host}' -H 'Cookie: {session}' http://{vip}/")
        return p if "Could not install Home Assistant" in p else None
    deadline = time.time() + 300
    page_after = None
    while time.time() < deadline and page_after is None:
        page_after = outcome_on_page()
        if page_after is None:
            time.sleep(5)
    assert page_after, "the host's outcome never reached the page (the dashboard still says installing, or forgot the ask)"
    # The reason, the CLI, and the choice still there to press again.
    assert "sudo briard app install home-assistant" in page_after and 'name="app" value="home-assistant" checked' in page_after, page_after
    assert "no admin port" not in page_after, "the dashboard found no port -- qemu did not expose it to the guest"
    print("the admin port carried the button's directive to the host and the host's refusal back")

    # --- The name, and the three identifiers behind it ---
    # One string for the API identity, the guest hostname, DRBD's `on <name>` and the mDNS label
    # would make every install on earth answer to the same name; the asserts below keep them apart.
    flock_name = host.succeed("cat /var/lib/briard/flock-name").strip()
    node_id = host.succeed("cat /var/lib/briard/node-id").strip()
    print(f"flock name={flock_name!r} node id={node_id!r}")

    # ⚠️ The agent minted them, not install.sh -- asserted from its log (said once, on the boot that
    # minted) because the files cannot say who wrote them, and who is the point: a Windows host
    # runs the same Go and can reuse none of a POSIX script.
    host.succeed("journalctl -u briard-agent | grep -q 'identity: this install is called'")
    host.succeed(f"journalctl -u briard-agent | grep -q 'identity: this machine is {node_id}'")
    # ...and minted once. The VM UUID, and so the guest machine-id, derives from the node id (and
    # in a flock DRBD names the node by it), so a re-mint would leave the guest unable to
    # recognise its own state -- and the reinstall below is exactly that second start.
    assert host.succeed("grep -c . /var/lib/briard/node-id").strip() == "1", "the node id is not one line"

    # The drawn subnets. Both are drawn per home and checked against the network the host sees,
    # so every assertion below reads what this install chose; a rig spelling 10.0.0.1 would make
    # the negative assertions ("the name must not resolve to the private address") pass
    # vacuously, the one way a test lies while green. The agent drew them, not install.sh: the
    # agent says it numbered this node, and config.env carries no address it could have been told
    # -- either alone is satisfiable by an installer-numbered node, which no release can renumber
    # and no re-parent can re-check.
    host.succeed("journalctl -u briard-agent | grep -q 'this node numbers itself from'")
    host.fail("grep -q '^SYSTEM_CIDR=' /opt/briard/config.env")

    # ---- config.env carries two kinds of line and no third ------------------------------------
    # What the install (`briard-agent install`, called by install.sh) computed about this host, and
    # what the operator said (the environment, BRIARD_ prefix stripped). No defaults: an unnamed
    # key is answered from agent/host/config.go, which a release can change and a line here could
    # not. (The writer has two more kinds, absent here: BACKUP_DIR for who installed it, written
    # only for a SUDO_USER other than root, and this rig installs as root; and what `briard config
    # set` changed, carried over by an install over a node, and this is a first install.) A
    # whitelist, not a list of keys that must be absent, because the failure guarded is a new
    # default written here out of habit, which then fails by name -- as does a stray BRIARD_* from
    # the invoking environment.
    keys = sorted(
        l.split("=")[0]
        for l in host.succeed("cat /opt/briard/config.env").splitlines()
        if l and not l.startswith("#")
    )
    # ⚠️ No identifiers: the agent mints NODE, FLOCK_ID and FLOCK_NAME into pet state at its first
    # start, so the computed lines are only paths to what this install laid down or the agent
    # makes. Their appearing here would mean the installer took back a decision a Windows host has
    # to make too.
    computed = {
        "QEMU", "QEMU_DATADIR", "NET_WRAP_BIN",                     # staged here
        "GUEST_IMAGE", "DATA_DISK", "STATE_DISK",                   # image laid here; disks the agent makes
    }
    copied = {"CHANNEL_URL", "UPDATE_KEYRING", "CATALOG_URL"}  # exactly what channelEnv carried
    assert set(keys) == computed | copied, (
        f"config.env is {keys}: unexpected {sorted(set(keys) - computed - copied)}, "
        f"missing {sorted((computed | copied) - set(keys))}"
    )
    print(f"config.env is {len(keys)} lines, all computed or copied: {keys}")

    subnets = host.succeed("cat /var/lib/briard/subnets")
    system_subnet = host.succeed("sed -n 's/^SYSTEM_SUBNET=//p' /var/lib/briard/subnets").strip()
    priv_subnet = host.succeed("sed -n 's/^PRIV_SUBNET=//p' /var/lib/briard/subnets").strip()
    print(f"drawn subnets: {subnets.strip()!r}")
    for label, drawn in (("system", system_subnet), ("private link", priv_subnet)):
        assert drawn.startswith("10.") and drawn.count(".") == 2, (
            f"the {label} subnet is {drawn!r}, not a bare 10.a.b -- install.sh built the substrate "
            f"out of something the draw did not produce"
        )
    # The draw happened, rather than a constant surviving under a new name. 10.0.0 is in the flock
    # pool exclusion table, so it cannot be drawn; 10.11.9 is not excluded from the private-link
    # draw (10.11.R, R uniform), so it is a legal draw one time in 256.
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

    # The assertion this block exists for: the name resolves off-box, from a client with the
    # mdns4_minimal resolver a household has. Publishing is not resolving (a desktop measured a name
    # that published fine and was unlookupable), so this asks the client, not the guest.
    # Instrumented before it is asserted, deliberately: diagnose() hangs off wait_for_lease, so an
    # mDNS failure would print one line and hide the guest side; a publisher that refuses (avahi
    # "Not permitted") and one that never runs look identical from the client, and guessing costs
    # a full runner round-trip each time.
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
        f"a name pointing somewhere the address is not is the baked-default failure with a new face"
    )
    # And the whole way through: the name a household types actually serves.
    client.wait_until_succeeds(f"curl -fsS http://briard-{flock_name}.local/healthz", timeout=120)
    print(f"off-box client reached http://briard-{flock_name}.local/ by NAME")
    # The bare alias, beside it: `briard.local` is what the installer printed and what the
    # household types, answered by the same responder for the same VIP. Read the output, never
    # $? (avahi-resolve-host-name exits 0 on a timeout).
    bare = client.succeed("avahi-resolve-host-name -4 briard.local")
    assert vip in bare, f"briard.local resolved to {bare!r}, not to the leased VIP {vip}"
    client.succeed("curl -fsS -m 20 http://briard.local/healthz")
    print("off-box client reached http://briard.local/ by the bare NAME")

    # ⚠️ The premise of everything below: the guest says nothing unsolicited. Asserted, because
    # the cold-cache assertion is only as strong as it: an announcement is the same packet as a
    # response, so a client asking while one is in flight can be answered without its query ever
    # reaching the guest -- hiding inbound mDNS dropped on the macvtap per-child mc_filter while
    # egress works, so the name works until every client cache goes cold. The responder answers
    # and announces nothing, so the window does not exist; if an announcer is ever added (a
    # standing option), this fails first, and the fix is an announcement-tail wait before the
    # cold-cache resolve (40s cleared avahi's tail), not deleting the line that noticed. Counted
    # off the wire from the client side, with nothing on this node querying, so every packet is
    # one the guest sent unasked; tcpdump chatter goes to stderr, and `|| true` keeps a zero count
    # (grep's exit 1) from reading as a failure.
    quiet = client.succeed(
        f"timeout 12 tcpdump -i eth1 -l -n 'udp port 5353 and src host {vip}' 2>/dev/null "
        "| grep -c 'IP' || true"
    ).strip()
    assert quiet == "0", (
        f"the guest sent {quiet} unsolicited mDNS packet(s) in 12s of silence. Something announces "
        f"now, so the cold-cache assertion below can be answered by a multicast nobody asked for "
        f"-- restore an announcement-tail wait before it"
    )

    # The same name from a cold cache, the half with teeth: the warm resolve above can be satisfied
    # by anything the client holds; this only by a query that reached the guest and an answer that
    # came back. Egress needs nothing from us, so a warm-cache rig cannot fail on inbound mDNS
    # being dropped -- the failure a household meets hours later. The restart is the flush, and
    # with nothing announcing it is all this needs: no cached record for a TTL to expire, no
    # announcement tail to outlast. It means something only if it fails with ALLMULTI removed from
    # the agent (nic.ensureMacvtap, which sets it on each macvtap child).
    client.succeed("systemctl restart avahi-daemon")
    client.sleep(5)
    # avahi-resolve-host-name exits 0 even when it fails ("Failed to resolve host name ...: Timeout
    # reached" on stderr), so read the output, never $?.
    cold = client.succeed(f"avahi-resolve-host-name -4 briard-{flock_name}.local")
    if vip not in cold:
        mdns_state("after the cold-cache resolve failed")
    assert vip in cold, (
        f"from a COLD cache briard-{flock_name}.local resolved to {cold!r}, not to {vip} -- the "
        f"name is reachable only while the guest's announcement is still cached, so inbound mDNS "
        f"is not reaching the guest. The macvtap drops it on macvlan_broadcast()'s per-child "
        f"mc_filter unless the child carries ALLMULTI (install.sh sets it at device creation)"
    )
    client.succeed(f"curl -fsS -m 20 http://briard-{flock_name}.local/healthz")
    print(f"off-box client reached http://briard-{flock_name}.local/ by NAME from a COLD cache")

    # ...and a group that is not mDNS. ALLMULTI is all-or-nothing, so the name already depends on
    # it; this pins the property the household relies on, every group the guest joins, not just
    # the one whose failure happens to be visible.
    assert_guest_receives_multicast(client, "eth1", "192.168.1.2", vip)

    # DELTA 5: the install host reaches its own guest, the one machine on this L2 that otherwise
    # could not. Everything above is proved from `client`, deliberately, and that alone would miss
    # it: macvtap isolates a parent NIC from its children and a switch will not reflect a frame to
    # its source port, so without the private link the user gets an address and a name that work
    # everywhere but on the machine reading them. The user-facing strings are unchanged (the VIP
    # and the flock name the client used); the private link is transport, never published -- its
    # address appears in the route below and nowhere a household would look.
    host.wait_until_succeeds(f"curl -fsS http://{vip}/healthz", timeout=180)
    print(f"the install host reached its own guest at http://{vip}/")

    # The node IP, on a lone node: a standalone install is a single-node flock, node-id 0, so its
    # guest takes .1 on the system subnet the agent drew, with no cloud pairing (which a free-tier
    # island never has) -- an address of its own for the reboot gate to aim at besides a
    # private-link address. Observed from the off-box client, not asked of the guest (that is
    # asking the actor): the system subnet rides the LAN L2, so any other machine reaches it
    # on-link like this, while the host is the one machine macvtap isolates (its own path is the
    # private link /32, which lands with the rest of the demotion).
    client.succeed(f"ip route replace {system_subnet}.0/24 dev eth1")
    client.wait_until_succeeds(f"ping -c1 -W2 {node_ip}", timeout=60)
    print(f"the lone node holds its node IP at {node_ip}, reached from off-box")

    # ...and the host reaches it too, the half macvtap otherwise denies: over the private link, on
    # a /32 the agent installed with a permanent neighbour entry pinning the guest's derived link
    # MAC. Without it the host would ARP for a node IP living on eth1 while the request arrives
    # on eth3, and arp_ignore=1 makes the guest answer nothing.
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
    # `via` the guest's end, never on-link: the guest answers ARP only on the interface holding the
    # address (arp_ignore=1), so an on-link /32 installs cleanly and then black-holes -- a
    # regression that looks like a working route in every `ip route` listing.
    assert node_ip in route, (
        f"the route to {vip} is not via the guest's NODE IP: {route!r} -- an on-link route cannot "
        f"resolve the VIP's MAC under arp_ignore=1, and the node IP is the one address on that "
        f"link the host can resolve, because the agent pinned a permanent neighbour entry for it"
    )

    # Non-vacuity and self-healing in one move: without the route the host is back in exactly the
    # state measured on the stranger's desktop, and a restarted agent must restore it on its own --
    # proving the route is the cause, not merely present beside the result (racing the agent at
    # boot would not). The agent is stopped for the negative half, not to be tidy: its route
    # reconcile (agent/host/viproute.go) acts only when the guest VIP changes, so a running agent
    # would not notice a deleted route, and the restart makes a fresh reconcile install it. The
    # guest keeps serving (a detached sibling unit, as the cattle reset relies on), so only the
    # isolation is measured.
    host.succeed("systemctl stop briard-agent.service")
    host.succeed(f"ip route del {vip}/32 dev briard-priv0")
    host.fail(f"curl -fsS --max-time 5 http://{vip}/healthz")
    print(f"without the route the host cannot reach {vip} -- the isolation is real")
    host.succeed("systemctl start briard-agent.service")
    host.wait_until_succeeds(f"curl -fsS http://{vip}/healthz", timeout=180)
    print("the agent re-established the route on its own")

    # The name, from the host: the query can only be answered over the private link (the guest
    # multicast on the macvtap never returns here), so this catches avahi denied eth3 in the guest.
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
    # To the VIP, never the private address: the name is flock-scoped and survives a failover, the
    # private-link address is node-scoped and does not, so a name pointing at it would restore the
    # one-string-for-three-identities incoherence where nobody would look.
    assert priv_guest_ip not in host_resolved, (
        f"the name resolved to the private link address ({host_resolved!r}) -- transport must "
        f"never become identity"
    )
    host.wait_until_succeeds(f"curl -fsS http://briard-{flock_name}.local/healthz", timeout=120)
    print(f"the install host reached http://briard-{flock_name}.local/ by NAME")

    # The DHCP hostname deliberately does not follow the flock name: option 12 stays derived
    # in-guest from the flock MAC the NIC carries, because no one can predict how an arbitrary
    # server reacts to a hostname change mid-lease, and a rename must never risk the address.
    # Asserted so the divergence stays a decision and cannot decay into a bug either way.
    assert name != f"briard-{flock_name}", (
        f"the router's client name {name!r} now tracks the flock name -- that coupling was "
        f"removed on purpose: it puts the address at risk every time someone renames"
    )

    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'role=anchor primary=true quorate=true.*healthy=true'",
        timeout=60,
    )

    # --- A role cycle, observed off-box (its twin is DELTA 5 in install-bridge.nix, where the ⚠️
    # explains what an earlier, vacuous version of this block wrongly claimed). Proven: a demote
    # really stops serving, seen off-box, and re-promotion brings the VIP back on the same MAC. Not
    # proven: the two-node standby discipline (a Secondary teaching the switch the wrong port for
    # the flock MAC), which needs a two-node rig -- a lone node has no wrong port.
    # ⚠️ Placed on a freshly converged node, not after the self-update section, which restarts the
    # agent repeatedly: `systemctl is-active` does not mean the admin socket is back -- serveLocal
    # runs in a goroutine started after READY=1, so the two are not ordered, and the CLI honestly
    # says "cannot reach the agent at /run/briard/admin.sock" at a moment of the test's own
    # making. The socket wait below is kept anyway, because a restart precedes nothing here by
    # accident.
    host.wait_until_succeeds("test -S /run/briard/admin.sock", timeout=60)
    serving_mac = client.succeed(f"ip -4 neigh show {vip} dev eth1").split()
    serving_mac = serving_mac[serving_mac.index("lladdr") + 1]
    host.succeed("/opt/briard/agent/briard-agent handover -keep-masked")
    client.wait_until_fails(f"curl -fsS --max-time 3 http://{vip}/healthz", timeout=120)
    print("a demoted node stops serving, seen from off-box")

    host.succeed("/opt/briard/agent/briard-agent handover -unmask")
    try:
        client.wait_until_succeeds(f"curl -fsS http://{vip}/healthz", timeout=300)
    except Exception:
        # The re-promotion is the first start of every chain member through the guest pivot after
        # a dress; if the house does not come back, the console is where the picker, the commit
        # and the units say why.
        print("=== re-promotion failed: the guest's units and pivot ===")
        print(guest_console("briard-bin|briard-guest-agent|briard-reverse-proxy|briard-vip|drbd-reactor|drbd-promote|Control process|Failed|failed"))
        print("=== re-promotion failed: the host's guest-bundle lines ===")
        print(host.succeed("journalctl -u briard-agent | grep -E 'guest bundle|reconnected|status node=' | tail -30"))
        raise
    client.succeed("ip neigh flush dev eth1")
    client.wait_until_succeeds(f"ping -c1 -W2 {vip}")
    back = client.succeed(f"ip -4 neigh show {vip} dev eth1").split()
    print(f"off-box after re-promotion: {' '.join(back)}")
    assert "lladdr" in back and back[back.index("lladdr") + 1] == serving_mac, (
        f"the VIP came back on a different MAC ({' '.join(back)}, was {serving_mac}) -- the "
        f"service identity is supposed to be the one thing a role change leaves unchanged"
    )

    # FHS: pet volume under /var/lib, cattle under /opt (the cattle/pet split assertion d builds on).
    host.succeed("test -f /var/lib/briard/data.img")   # pet
    host.succeed("test -f /opt/briard/guest-image/nixos.qcow2")  # cattle: the image, attached read-only
    host.fail("test -e /opt/briard/guest.qcow2")                 # and no OS-disk overlay beside it
    host.succeed("test -x /opt/briard/qemu/bin/qemu-system-x86_64")
    print(host.succeed("journalctl -u briard-agent | tail -20"))

    # --- assertion (d): cattle/pet reinstall ---
    # The pet (/var/lib/briard) must survive a cattle (/opt/briard) wipe: `rm -rf /opt/briard` +
    # reinstall re-fetches the cattle and reaches green with the guest data intact, proving the
    # guest reattaches the existing data volume (a LUKS header it finds is opened and its volume
    # group activated; only a device with none reaches the format branch) rather than reformatting.
    # The handle is the volume UUID, read off the backing file: a reformat mints a fresh one, an
    # honest reattach cannot. An app tick counter would be strictly stronger (it proves committed
    # *content* crossed, not just the same volume), but the shipped node runs no app, so nothing
    # writes data to compare, and no test installs one before a cattle wipe.
    def fsid(m):
        # The volume is encrypted, so no btrfs magic is findable in the file (the point, asserted
        # below); the handle is the LUKS2 header UUID, at a fixed offset in a layout that is
        # LUKS2's, not ours (magic at 0, uuid at 168, 40 bytes NUL-padded). Asserting the magic
        # keeps it honest: wrong offsets would otherwise compare two identical blobs of zeroes and
        # "pass".
        magic = m.succeed(
            "dd if=/var/lib/briard/data.img bs=1 count=6 status=none | od -An -tx1 | tr -d ' \\n'"
        ).strip()
        assert magic == "4c554b53babe", f"the data volume is not LUKS2 (read {magic!r})"
        return m.succeed(
            "dd if=/var/lib/briard/data.img bs=1 skip=168 count=40 status=none | tr -d '\\0'"
        ).strip()

    pre = fsid(host)
    print(f"pre-wipe data volume LUKS uuid={pre}")

    # Encrypted at rest, asked of the disk: a default `curl | sh` install on a host with AES makes
    # a volume whose btrfs is not findable in the backing file -- nobody chooses it, nobody types a
    # passphrase, and the node comes up green. `grep -c`, not `grep -q`: -q exits at the first match
    # and the pipeline status becomes that of the SIGPIPE-killed producer, inverting the answer on
    # exactly the assertion that must not invert.
    hits = host.succeed("head -c 67108864 /var/lib/briard/data.img >/tmp/rawprobe; "
                        "grep -ac _BHRfS_M /tmp/rawprobe || true").strip()
    assert hits == "0", f"the data volume is READABLE in the backing file ({hits} btrfs superblocks)"
    host.succeed("rm -f /tmp/rawprobe")

    # ...and the host kept the header beside it, once, at bring-up: ~16 MB whose loss is the loss
    # of everything on the volume, and it sits below DRBD so it never replicates.
    host.succeed("test -s /var/lib/briard/data-header.img")
    assert host.succeed("stat -c %s /var/lib/briard/data-header.img").strip() == "16777216"
    assert host.succeed("dd if=/var/lib/briard/data-header.img bs=1 count=6 status=none "
                        "| od -An -tx1 | tr -d ' \\n'").strip() == "4c554b53babe", \
        "the kept header is not a LUKS header"

    # The route withdrawal is proven in agent-recover, not here: a withdrawal needs the agent alive,
    # and an agent alive when its guest unit stops relaunches the guest -- straight into the cattle
    # reset below. It also leaves nothing to stop: a stopped transient unit is garbage-collected,
    # so naming it again is exit 5. agent-recover already kills the guest under a running agent and
    # waits out the relaunch.

    # The honest cattle-reset gesture: stop the agent and its detached guest unit (a sibling
    # transient service, so stopping the agent alone leaves qemu holding the image and the macvtap
    # chardev), then remove only /opt/briard.
    host.succeed("systemctl stop briard-agent.service briard-guest.service")
    host.succeed("rm -rf /opt/briard")
    host.fail("test -e /opt/briard/qemu/bin/qemu-system-x86_64")  # cattle really gone
    host.succeed("test -f /var/lib/briard/data.img")               # pet survives the wipe
    host.succeed("test -f /var/lib/briard/state.img")              # so does the state disk
    # ...carrying the filesystem the first guest made (ext4 magic at 1080), whose UUID the
    # reinstall's guest must find and keep rather than format again.
    assert host.succeed("dd if=/var/lib/briard/state.img bs=1 skip=1080 count=2 2>/dev/null | od -An -tx1 | tr -d ' \\n'").strip() == "53ef", "the state disk carries no ext4 filesystem after the first install"
    state_uuid = host.succeed("dd if=/var/lib/briard/state.img bs=1 skip=1128 count=16 2>/dev/null | od -An -tx1 | tr -d ' \\n'").strip()
    # The live macvtaps (kernel state, not owned by /opt) survive the wipe, and the host address
    # never moved, so there is nothing to restore.
    host.succeed("ip -d link show briard0 | grep -q macvtap")
    host.succeed("ip -o -4 addr show dev eth1 | grep -qw 192.168.1.1")
    # Non-vacuity for the re-green proof below: with the guest gone the VIP no longer answers.
    client.wait_until_fails(f"curl -fsS --max-time 3 http://{vip}/healthz", timeout=60)
    # Arm the repair path: convergence adopts existing devices rather than re-creating them, so a
    # macvtap left autoconfiguring stays so unless the agent repairs it in place -- the reason its
    # IPv6 write sits outside the create branch (agent/nic/converge.go, ensureMacvtap).
    host.succeed("echo 0 > /proc/sys/net/ipv6/conf/briard0/disable_ipv6")
    host.wait_until_succeeds("ip -6 addr show dev briard0 | grep -q inet6", timeout=30)

    # Reinstall, the same one command: it re-lays /opt and the image from the channel (cattle), does
    # not recreate the pet data.img, and the agent adopts the macvtaps already up.
    host.succeed(
        "${channelEnv} "
        "BRIARD_UNIT_DIR=/run/systemd/system sh ${installScript}"
    )
    host.succeed("test -x /opt/briard/qemu/bin/qemu-system-x86_64")  # cattle re-fetched
    # The state disk on the shipped install: made thick by the agent before its first launch,
    # handed to the guest by serial, formatted by the guest exactly once (first boot of the first
    # install; the reinstall guest found and kept it), and the same machine on both boots because
    # its machine-id comes from the VM UUID the host derives from the node. Read from what the
    # running qemu was handed, not the STATE_DISK line in config.env: a file line proves what the
    # installer wrote, not what the guest got.
    host.succeed("pgrep -af qemu-system-x86_64 | grep -q '/var/lib/briard/state.img'")
    host.succeed("pgrep -af qemu-system-x86_64 | grep -q 'serial=briard-state'")
    host.succeed("pgrep -af qemu-system-x86_64 | grep -q -- '-uuid '")
    assert state_uuid == host.succeed("dd if=/var/lib/briard/state.img bs=1 skip=1128 count=16 2>/dev/null | od -An -tx1 | tr -d ' \\n'").strip(), "the reinstall's guest reformatted the state disk"
    NODE, STATE_IMG = host.succeed("cat /var/lib/briard/node-id").strip(), "/var/lib/briard/state.img"
    # ⚠️ The reinstall is the agent's second start, the boot that could re-mint, and must not: the
    # VM UUID derives from the node id (and in a flock DRBD names the node by it), so a fresh one
    # is a guest that no longer recognises the pet state kept here. (The state-disk check above
    # would catch it too, by a longer route.)
    assert NODE == node_id, f"the reinstall re-minted the node id: {node_id!r} -> {NODE!r}"
    # The guest machine-id, read off the state disk (/var/log/journal/<machine-id>/ is
    # /journal/<machine-id>/ there), must be the UUID the host derives from the node name
    # (host.deriveUUID, mirrored here), which systemd.machine_id=firmware takes from the VM DMI
    # product UUID. PID 1's "Initializing machine ID" line is kmsg-only and never reaches the
    # console, so the value is the proof.
    import hashlib
    _h = bytearray(hashlib.sha256(("briard-machine-uuid:" + NODE).encode()).digest()[:16])
    _h[6] = (_h[6] & 0x0F) | 0x50
    _h[8] = (_h[8] & 0x3F) | 0x80
    want_id = _h.hex()
    # Read through a wait: the host looks at the image while the guest still flushes it (journald
    # moves the runtime journal after the state disk mounts; ext4 commits reach the host file on
    # the guest commit interval), so a read seconds after CONVERGED can find no directory
    # (measured: one run in three). The wait is on the exact id, so a wrong machine-id still times
    # out and the assertion below says what was found.
    host.wait_until_succeeds(f"debugfs -R 'ls -p /journal' {STATE_IMG} 2>/dev/null | cut -d/ -f6 | grep -qx {want_id}", timeout=120)
    journals = host.succeed(f"debugfs -R 'ls -p /journal' {STATE_IMG} 2>/dev/null | cut -d/ -f6 | grep -E '^[0-9a-f]{{32}}$' || true").split()
    assert journals == [want_id], f"the guest's journal on the state disk is under {journals}; want the machine-id derived from the node name, {want_id}"
    # The repair landed on the device already up, not just on fresh ones. A bounded wait, because
    # the repair is the agent convergence tick, and the install returns as soon as the node is
    # healthy, before the next tick. It still fails if convergence stops asking: `Converged` checks
    # disable_ipv6 precisely so an existing device, never re-created, still gets repaired.
    host.wait_until_succeeds("grep -qx 1 /proc/sys/net/ipv6/conf/briard0/disable_ipv6", timeout=60)
    assert host.succeed("ip -6 addr show dev briard0").strip() == "", (
        "the agent adopted briard0 but left it autoconfiguring on the guest's MAC"
    )

    # Green again on the re-fetched bundle, at the same address: the flock id is pet state
    # (/var/lib/briard/flock-id), so it survived the wipe; the service MAC derives from it, the
    # client-id is that MAC, so the router returns the same lease -- the chain the agent identity
    # line promises with "keep this file to keep your address".
    client.wait_until_succeeds(f"curl -fsS http://{vip}/healthz", timeout=600)
    host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64")
    again, _, _, _ = wait_for_lease()
    assert again == vip, f"the address moved across a cattle reinstall ({vip} -> {again}) -- the pet flock id did not carry it"

    # The name is pet too, so a household that reinstalls must not find its bookmark dead. node-id
    # must also survive: the VM UUID derives from it, and in a flock DRBD names the node by it (`on
    # <name>`), so a regenerated one leaves the guest unable to recognise its own state. (This lone
    # node runs no DRBD.)
    assert host.succeed("cat /var/lib/briard/flock-name").strip() == flock_name, (
        "the flock name changed across a cattle reinstall -- the name on the LAN is pet state"
    )
    assert host.succeed("cat /var/lib/briard/node-id").strip() == node_id, (
        "the node id was regenerated by a cattle reinstall -- DRBD's `on <name>` is keyed to it, "
        "so the guest would no longer recognise the metadata on the volume it just kept"
    )
    # The subnets are pet for another reason: re-running the installer is routine in this alpha,
    # and a redraw would silently renumber a live node while its peers hold the old address. The
    # draw happens once, on a machine that has never drawn.
    assert host.succeed("cat /var/lib/briard/subnets") == subnets, (
        f"the subnets were redrawn by a re-run ({subnets.strip()!r} -> "
        f"{host.succeed('cat /var/lib/briard/subnets').strip()!r}) -- a re-run must not renumber a node"
    )
    host.succeed(f"ip route get {node_ip} | grep -q briard-priv0")
    client.wait_until_succeeds(f"curl -fsS http://briard-{flock_name}.local/healthz", timeout=180)
    print(f"the name survived the cattle reinstall: http://briard-{flock_name}.local/")
    print(f"reinstall reached green again on the re-fetched cattle, at the same leased {vip}")

    # The proof (assertion d): the guest re-attached the same volume, not a fresh one wearing the
    # same path; a reformat would mint a new UUID, the sharp failable control.
    post = fsid(host)
    print(f"post-reinstall data volume LUKS uuid={post} (pre-wipe={pre})")
    assert post == pre, f"PET LOST: data volume reformatted across reinstall ({pre} -> {post})"
    print("pet volume survived the cattle wipe: the guest reattached it")

    def restart_node():
        """POWER-CUT the node and bring it back: a fresh guest boot, so promotion runs again and
        briard-vip re-resolves the service address from scratch. The guest is a SIBLING transient
        unit, so stopping the agent alone would leave qemu holding the image.

        The SIGKILL is load-bearing, not a shortcut. What this sets up is the
        unplanned-failover case, and the entire argument for storing the address is that an
        unplanned failover is, in the field, usually a power cut. Once the guest unit grew an
        ExecStop, a plain `systemctl stop` became a CLEAN shutdown -- which unmounts the volume and
        therefore flushes .vip-address whether or not the product ever fsynced it. Keeping the
        graceful stop here would have left the assertion below green against a reverted address store,
        testing the harness's good manners instead of the product's durability."""
        host.succeed("systemctl stop briard-agent.service")
        host.succeed("systemctl kill --signal=SIGKILL briard-guest.service")
        host.wait_until_fails("systemctl is-active briard-guest.service", timeout=60)
        client.wait_until_fails(f"curl -fsS --max-time 3 http://{vip}/healthz", timeout=60)
        host.succeed("systemctl start briard-agent.service")

    # --- Coming back must not depend on the household router --------------------------------
    # The design's central claim: the address is flock state (.vip-address on the data volume,
    # replicated where there is a flock), so promotion applies it rather than asking for it. With
    # the only DHCP server killed, a restarted node must serve on the same address. It matters
    # because an unplanned failover happens exactly when the household's network is least likely
    # to be answering, and a promotion waiting for a DHCP ACK would make the router a dependency of
    # recovery.
    router.succeed("systemctl stop dnsmasq.service")
    router.fail("systemctl is-active dnsmasq.service")   # the segment really has no server now
    restart_node()
    try:
        client.wait_until_succeeds(f"curl -fsS http://{vip}/healthz", timeout=300)
    except Exception:
        diagnose("router-down restart")
        raise
    print(f"came back on {vip} with no DHCP server on the segment -- the flock's stored address carried it")

    # --- And when the answer changes, the node follows ---------------------------------------
    # The stored address is a starting point, not a lease owned forever. The router returns with a
    # pool excluding our address; whether it refuses the request or offers another address,
    # dhcpcd ends up holding something other than what we applied -- the trigger of the
    # address-changed hook, which must withdraw ours, take theirs, re-announce it and update the
    # flock store. One path for every cause. Run by hand because the pool must change and the
    # module config is a store path; dhcp-authoritative keeps the server decisive about an address
    # outside the pool.
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
    # Identity is flock-scoped, so it must be unchanged by the address moving underneath it.
    assert moved_name == name and moved_id.lower() == clientid.lower(), (
        f"identity drifted with the address: {moved_name}/{moved_id} was {name}/{clientid}"
    )
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    client.wait_until_fails(f"curl -fsS --max-time 3 http://{vip}/healthz", timeout=60)
    print(f"followed the router from {vip} to {moved}, same identity, and stopped answering at the old one")

    # The host route follows the address, and the stale one goes: two /32s to one guest would both
    # work, so the old one would keep answering after the household address changed, unnoticed
    # until a failover made it wrong -- the re-lease path the reconcile exists for, on a real lease.
    host.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    host.wait_until_fails(f"ip route get {vip} | grep -q briard-priv0", timeout=120)
    host.succeed(f"ip route get {moved} | grep -q briard-priv0")
    print(f"the host route followed the lease from {vip} to {moved}, leaving nothing behind")

    # ---- Self-update, on the units and the script the install actually laid -----------------
    # agent-selfupdate.nix proves the procedure on units it constructs itself, which does not prove
    # the shipped ones; so these read the installed units and script, and the updates below run
    # through them.
    host.succeed("test -x /opt/briard/agent/briard-agent")

    # The units were copied out of the verified set, not rendered: byte-identical to what the
    # channel published (which `briard-agent install` verified against the signed manifest before
    # laying it down). The one assertion that fails the moment a heredoc returns: a rendered unit
    # can match every directive below and still not be the file the signature covered.
    for u in ("briard-agent.service", "briard-update.service", "briard-update.timer"):
        host.succeed(f"cmp /run/systemd/system/{u} /srv/briard/{V}/linux-amd64/{u}")
    # ...and so was the frozen update script, where it matters most: it launches every self-update,
    # so "the file on disk is the file that was signed" is what the whole update rests on.
    host.succeed(f"cmp /opt/briard/agent/briard-update /srv/briard/{V}/linux-amd64/briard-update")
    host.succeed("test -x /opt/briard/agent/briard-update")
    print("the units and the agent's update script are byte-identical to the signed channel's")

    unit = host.succeed("systemctl cat briard-agent.service")
    for want in (
        "Type=notify",
        # The committed binary, directly: the updater swaps it by rename beneath this name.
        "ExecStart=/opt/briard/agent/briard-agent run",
        # A failed update followed by its revert is a burst of starts; without this it can trip
        # systemd's start limiter and leave the node down for the one reason self-update exists
        # to avoid.
        "StartLimitIntervalSec=0",
    ):
        assert want in unit, f"the shipped unit is missing {want!r}"
    # The third leg: the directory the updater swaps in must be the one ExecStart runs from, or a
    # swap is a cross-filesystem rename at best and a no-op at worst. The update script bakes it.
    host.succeed("grep -q '^BASE=/opt/briard/agent$' /opt/briard/agent/briard-update")
    host.succeed("systemctl cat briard-update.timer | grep -q '^OnBootSec='")

    # ---- The network is the agent's ----------------------------------------------------------
    # No generated script, no oneshot unit, and -- the part that would take the node dark when it
    # failed -- no [Unit] dependency on one: a cable out at boot must leave an agent running and
    # answerable, not a unit that never started.
    host.fail("test -e /opt/briard/net-up.sh")
    host.fail("systemctl cat briard-net.service")
    # Directives only -- `unit` is the raw file, and its comments discuss the dependencies it
    # deliberately does not have.
    directives = [l for l in unit.splitlines() if l and not l.startswith(("#", ";"))]
    deps = [l for l in directives if l.startswith(("Requires=", "After=", "Wants=", "BindsTo=", "PartOf="))]
    assert not deps, f"the agent unit grew a start dependency {deps} -- a network failure can take the node dark again"
    # ...and the agent says which device it hung the guest's L2 off, which is the line an operator
    # reads when the answer surprises them.
    host.succeed("journalctl -u briard-agent | grep -q \"the guest's L2 hangs off eth1\"")
    # The devices the agent built, with the two flags that are not kernel defaults. ALLMULTI is
    # the one with teeth: without it the guest still answers unicast and its answers still leave,
    # so a client holding the record resolves the name until it expires -- an install looks
    # correct when it finishes and fails later, off-box, so it is asserted rather than trusted.
    for tap in ("briard0", "briard-drbd0"):
        host.succeed(f"ip -d link show {tap} | grep -q macvtap")
        host.succeed(f"ip link show {tap} | grep -q ALLMULTI")
        host.succeed(f"test $(cat /proc/sys/net/ipv6/conf/{tap}/disable_ipv6) = 1")
    host.succeed("ip -d link show briard-priv0 | grep -q tun")

    # ---- The unit carries no decisions -------------------------------------------------------
    # The point of the config file: a unit written at install is frozen where no release reaches
    # it, so nothing the agent decides from may live there. Three `Environment=` lines remain, each
    # execution environment, not configuration: PATH (systemd sets one, so a file value would
    # correctly lose), BRIARD_CONFIG (where the file is), GOTRACEBACK (read by the Go runtime
    # before our code runs). A whitelist, because the failure guarded is a new value added out of
    # habit: any other key (STATE_DISK, VIP_ADDR, SYSTEM_TAP...) fails here by name.
    stray = [
        l.split("=")[1]
        for l in unit.splitlines()
        if l.startswith("Environment=")
        and l.split("=")[1] not in ("PATH", "BRIARD_CONFIG", "GOTRACEBACK")
    ]
    assert not stray, f"the unit froze configuration the agent must be able to rewrite: {stray}"
    # That the agent reads the file needs no assertion of its own: with no values on the unit,
    # everything already proven green (the guest booted off GUEST_IMAGE, the state disk attached
    # by serial, the VIP answering) came from config.env or from nowhere.

    # The shipped update unit below the agent. agent-selfupdate.nix proves the mechanism with stub
    # candidates; this proves the install shipped it: the frozen script, the oneshot, the active
    # timer, the installed manifests the verb compares against, and one real `briard update`
    # round trip on this node -- bootstrap pulled from the test channel pointer, manifest verified,
    # compared, "already at" the installed release. Not a no-op: a version the channel does not
    # carry must fail through the same path.
    host.succeed("test -x /opt/briard/agent/briard-update")
    # is-active, not is-enabled: units in /run/systemd/system (see BRIARD_UNIT_DIR above) cannot be
    # enabled, so install.sh starts the timer instead. Active is what both install modes promise,
    # and what a persistent install gets from `enable --now`.
    host.succeed("systemctl is-active briard-update.timer")
    host.succeed("systemctl cat briard-update.service | grep -q '^Type=oneshot'")
    # ⚠️ The EnvironmentFile is load-bearing from here down, easy to misread: briard-update ships
    # verbatim with no channel URL in it, and reads CHANNEL_URL from config.env, which the unit
    # delivers. Every `update` round trip below reaches the stub at :8099 only that way; break the
    # EnvironmentFile and the script falls back to its https://get.briard.io default, which this VM
    # cannot reach, and they fail.
    host.succeed("systemctl cat briard-update.service | grep -q '^EnvironmentFile=-/opt/briard/config.env'")
    host.succeed("grep -q '^CHANNEL_URL=http://127.0.0.1:8099$' /opt/briard/config.env")
    host.fail("grep -q '127.0.0.1:8099' /opt/briard/agent/briard-update")
    host.succeed("test -s /opt/briard/agent/manifest.json && test -s /opt/briard/guest-image/manifest.json")
    host.succeed(f"grep -q '\"version\":\"{V}\"' /opt/briard/agent/manifest.json")
    host.succeed(f"grep -q '\"version\":\"{GV}\"' /opt/briard/guest-image/manifest.json")
    out = host.succeed("/opt/briard/agent/briard-agent update").strip()
    assert f"already at {V}" in out, f"briard update said: {out!r}"
    host.succeed("test ! -e /run/briard/update-target")                        # the unit consumed its target
    host.succeed(f"grep -q '^ok already at {V}' /run/briard/update-result")  # the verdict stays, for the alerter
    host.fail("/opt/briard/agent/briard-agent update -to v3.99010100.nothere")
    host.succeed("journalctl -u briard-update | grep -q 'could not fetch a bootstrap agent'")
    print("the shipped update unit, timer and CLI round-trip against the channel; an unknown pin fails loudly")

    # ---- A node that has stopped updating says so ---------------------------------------------
    # The CLI hands the verdict to whoever typed it; the timer, the whole of updating on a
    # standalone home, starts the unit directly, so a failed run ends in that unit's journal and
    # nothing reads it -- a household silently stopped updating looks current, the failure the
    # upgrade floor prevents, arriving by the other door. So the failure is induced on the timer's
    # own path: `systemctl start` with no target message is literally what briard-update.timer
    # does, and its verdict is the one the agent reads.
    host.succeed("systemctl reset-failed briard-update.service && rm -f /run/briard/update-result")
    host.succeed("mv /srv/briard/stable/linux-amd64/briard-agent /root/hidden-agent")
    host.fail("systemctl start briard-update.service")
    host.succeed("systemctl is-failed briard-update.service")
    host.succeed("grep -q '^failed could not fetch a bootstrap agent' /run/briard/update-result")
    # ⚠️ The assertion is the alert, not the failed run (the updater journal, green above, proves
    # only it complained to itself): the agent noticed and raised the household alert in the node
    # alert store, which `briard alerts` answers from -- asserted on its "open now" section, since
    # "stopped updating" is a condition, open until a run completes. The agent is restarted
    # because the watcher reads the verdict at start and then on a slow cadence, and an agent
    # coming up on a node already failing must report it: the no-priming half of the design.
    host.succeed("systemctl restart briard-agent.service")
    host.wait_until_succeeds(
        "/opt/briard/agent/briard-agent alerts | sed -n '/open now/,/history/p' | grep -qF 'this machine has stopped updating'",
        timeout=120,
    )
    # The run's last line rides along, because the remedy lives there: here "could not fetch", on
    # a floored node the refusal naming reinstall. "Something failed" gives the owner nothing to do.
    alerts = host.succeed("/opt/briard/agent/briard-agent alerts")
    assert "The update run said:" in alerts, f"the alert does not carry the run's own line:\n{alerts}"
    assert "could not fetch a bootstrap agent" in alerts, f"the alert does not carry the refusal:\n{alerts}"
    # The same alert on the page: the host copies its store across the guest channel
    # (dashboard.alerts) when it moves and at the start of every connection, and the home page
    # lists what is open under "Needs attention" -- no card while nothing is open, so this alert
    # makes the copy observable. Through a fresh `briard open` link, at the moved address.
    opened = host.wait_until_succeeds("/opt/briard/agent/briard-agent open", timeout=60)
    m = re.search(r"http://([^/\s]+)/\?code=([0-9a-f]{64})", opened)
    assert m, f"`briard open` printed no link:\n{opened}"
    again = client.succeed(f"curl -sS -D - -o /dev/null -H 'Host: {m.group(1)}' 'http://{moved}/?code={m.group(2)}'")
    assert " 303 " in again.splitlines()[0], again
    session = [l.split(":", 1)[1].split(";")[0].strip() for l in again.splitlines() if l.lower().startswith("set-cookie: briard_session=")][0]
    # The page is read whole, then searched: `curl | grep -q` fails under pipefail whenever grep
    # matches and exits before curl has written the rest ("Failed writing body"), which a page
    # longer than a pipe buffer makes a race.
    client.wait_until_succeeds(
        f"curl -fsS -H 'Host: {m.group(1)}' -H 'Cookie: {session}' -o /tmp/home.html http://{moved}/ "
        "&& grep -q 'Needs attention' /tmp/home.html "
        "&& grep -qF 'this machine has stopped updating' /tmp/home.html",
        timeout=60,
    )
    print("the open alert crossed the guest channel and is on the home page")

    # Put the channel back and clear the unit, so the steps below meet the node they expect.
    host.succeed("mv /root/hidden-agent /srv/briard/stable/linux-amd64/briard-agent")
    host.succeed("systemctl reset-failed briard-update.service")
    host.succeed("systemctl start briard-update.service")
    host.fail("systemctl is-failed briard-update.service")
    print("a run that failed on the timer's own path reached the household as an alert, carrying the updater's own last line")

    # ---- The host bundle on the shipped layout ----------------------------------------------
    # qemu is reached through a link: the public /opt/briard/qemu (baked into the bundle ELF
    # interpreter) is a fixed link onto /opt/briard/agent/qemu, pointing at the installed release
    # tree inside the directory self-update swaps in. The updater moves the link by rename, and
    # smoke-tests a staged tree before it stops anything.
    host.succeed("test -L /opt/briard/qemu && test -L /opt/briard/agent/qemu")
    assert host.succeed("readlink /opt/briard/qemu").strip() == "agent/qemu"
    assert host.succeed("readlink /opt/briard/agent/qemu").strip() == f"qemu-{V}", "the installed tree is not named by its release"
    host.succeed(f"test -x /opt/briard/agent/qemu-{V}/bin/qemu-system-x86_64")
    host.succeed("systemctl cat briard-agent.service | grep -q '^TimeoutStartSec=60'")
    qpid_before = host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64").strip().split()[0]

    # A release that changes qemu: the same bundle re-tarred with one file added, so the hash
    # differs and the updater must smoke-test a real qemu tree through its own loader (PT_INTERP
    # names the committed prefix) before stopping the agent. The agent bytes are the installed ones:
    # this proves the bundle path, not a new agent. publish_pin: `agent`, when given, replaces the
    # installed agent as the versioned artifact; `pointer` names a pointer the release is also
    # published under, carrying the installed agent as bootstrap -- the divergence
    # agent-selfupdate.nix explains, the only way to swap in a candidate that is not itself a
    # working updater.
    def publish_pin(version, bundle_dir, guest_dir=None, agent=None, pointer=None):
        d = f"/srv/briard/{version}/linux-amd64"
        host.succeed(f"mkdir -p {d} && cp -L /srv/briard/{V}/linux-amd64/briard-agent /srv/briard/{V}/linux-amd64/briard-net-wrap {d}/")
        host.succeed(f"tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -cf - -C {bundle_dir} . | zstd -q -3 -o {d}/qemu-bundle.tar.zst")
        # The guest bundle rides every host release: the installed one's bytes unless a
        # pin brings its own (the bad-bundle case below).
        if guest_dir:
            host.succeed(f"tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -cf - -C {guest_dir} . | zstd -q -3 -o {d}/guest-bundle.tar.zst")
        else:
            host.succeed(f"cp -L /srv/briard/{V}/linux-amd64/guest-bundle.tar.zst {d}/")
        host.succeed(f"chmod 0644 {d}/*.zst && {d}/briard-agent --stage-manifest {d} --chain briard --platform linux-amd64 --release {version} --vm {GV}")
        if agent:
            # The manifest above was written by the real agent over the real bytes; restage it over
            # the candidate's, so the signature covers what is served.
            host.succeed(f"install -m755 {agent} {d}/briard-agent && /srv/briard/{V}/linux-amd64/briard-agent --stage-manifest {d} --chain briard --platform linux-amd64 --release {version} --vm {GV}")
        host.succeed(f"{stub} sign /root/release.key {d}/manifest.json | base64 -d > {d}/manifest.json.sig")
        if pointer:
            p = f"/srv/briard/{pointer}/linux-amd64"
            # A pointer is a copy of its release: the launcher checks the agent it fetches against
            # the manifest beside it. Its files may be store links (the channel set-up above):
            # replace, never write through.
            host.succeed(f"mkdir -p {p} && rm -f {p}/manifest.json {p}/manifest.json.sig {p}/briard-agent && cp {d}/manifest.json {d}/manifest.json.sig {p}/ && cp {d}/briard-agent {p}/briard-agent")
    def agent_invocation():
        return host.succeed("systemctl show -p InvocationID --value briard-agent.service").strip()
    V2 = "v3.99123000.b86b0000"
    host.succeed(f"mkdir -p /root/bundle2 && zstd -dc < /srv/briard/{V}/linux-amd64/qemu-bundle.tar.zst | tar -xf - -C /root/bundle2")
    host.succeed("echo b86b > /root/bundle2/PROVENANCE.b86b")
    publish_pin(V2, "/root/bundle2")
    inv = agent_invocation()
    out = host.succeed(f"/opt/briard/agent/briard-agent update -to {V2}").strip()
    assert out == f"updated to {V2} (agent, qemu)", f"briard update said: {out!r}"   # net-wrap unchanged: not fetched
    assert agent_invocation() != inv, "the agent was not stopped and started by the update"
    host.succeed("systemctl is-active briard-agent.service")
    host.succeed("journalctl -u briard-update | grep -q 'qemu smoke test: scratch machine running'")
    assert host.succeed("readlink /opt/briard/agent/qemu").strip() == f"qemu-{V2}", "the qemu link did not move"
    host.succeed("test -e /opt/briard/qemu/PROVENANCE.b86b")   # the public path follows the link
    host.succeed(f"grep -q '\"version\":\"{V2}\"' /opt/briard/agent/manifest.json")
    host.succeed(f"test -d /opt/briard/agent/qemu-{V}")        # N-1 stays until a guest launch prunes it
    host.succeed("! ls /opt/briard/agent | grep -E '[.](next|prev)$'")
    # The guest never noticed: the same qemu process, on the tree it was launched from, still serving.
    assert host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64").strip().split()[0] == qpid_before, "the guest was relaunched by a qemu update"
    client.succeed(f"curl -fsS http://{moved}/healthz")
    print(f"{V2}: the updater smoke-tested the staged qemu, swapped the bundle in and committed it; the guest kept serving")

    # A release whose qemu does not run here is refused at stage time, whole, before anything
    # stops: the agent untouched, the staged set gone, the verdict saying why. The smoke test must
    # actually refuse, and be seen to.
    V3 = "v3.99123100.b86bbad0"
    host.succeed("mkdir -p /root/bundle3/bin && printf '#!/bin/sh\\nexit 1\\n' > /root/bundle3/bin/qemu-system-x86_64 && chmod 755 /root/bundle3/bin/qemu-system-x86_64")
    publish_pin(V3, "/root/bundle3")
    inv = agent_invocation()
    out = host.fail(f"/opt/briard/agent/briard-agent update -to {V3} 2>&1")
    assert "refused on this host: qemu smoke test: -version" in out, f"briard update said: {out!r}"
    assert agent_invocation() == inv, "a release refused at stage time stopped the agent"
    assert host.succeed("readlink /opt/briard/agent/qemu").strip() == f"qemu-{V2}", "a refused qemu was swapped in"
    host.succeed(f"grep -q '\"version\":\"{V2}\"' /opt/briard/agent/manifest.json")
    for n in ("briard-agent", "manifest.json", "briard-net-wrap", "qemu", "guest"):
        host.fail(f"test -e /opt/briard/agent/{n}.next")
    # ...and the extracted tree survives: the expensive part (downloaded, verified, unpacked), which
    # a retry of the same release reuses by name rather than pulling again. The link goes, not the
    # tree.
    host.succeed(f"test -d /opt/briard/agent/qemu-{V3}")
    client.succeed(f"curl -fsS http://{moved}/healthz")
    print(f"{V3}: a qemu that does not run here refused the whole release with the agent untouched")

    # An agent that does not start is reverted whole, on the shipped units: swapped in, its start
    # fails, the committed release goes back and starts, and the returning agent escalates what the
    # run left it; the guest never notices. The candidate is the real agent for every verb but
    # `run`, because the launcher runs the release agent as the updater and `--verify-agent` proves
    # the candidate before anything stops -- failing those is refused before the swap, a different
    # path. Only its start fails.
    V3b = "v3.99123100.b86bdead"
    host.succeed(f"cp -L /srv/briard/{V}/linux-amd64/briard-agent /root/real-agent")
    host.succeed("printf '#!/bin/sh\\n[ \"$1\" = run ] && exit 1\\nexec /root/real-agent \"$@\"\\n' > /root/dead-agent && chmod 755 /root/dead-agent")
    publish_pin(V3b, "/root/bundle2", agent="/root/dead-agent", pointer="latest")
    host.succeed("sha256sum /opt/briard/agent/briard-agent > /tmp/committed.sha")
    out = host.fail("/opt/briard/agent/briard-agent update -to latest 2>&1")
    assert "did not start on this host and was reverted" in out, f"briard update said: {out!r}"
    host.succeed("sha256sum -c /tmp/committed.sha")
    host.succeed(f"grep -q '\"version\":\"{V2}\"' /opt/briard/agent/manifest.json")
    host.succeed("systemctl is-active briard-agent.service")
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'host bundle update to .* failed and rolled back'", timeout=120)
    host.fail("test -e /run/briard/update-failure")             # taken by the agent that came back
    host.succeed("! ls /opt/briard/agent | grep -E '[.](next|prev)$'")
    assert host.succeed("pgrep -f /opt/briard/qemu/bin/qemu-system-x86_64").strip().split()[0] == qpid_before, "the guest was relaunched by a reverted update"
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    print(f"{V3b}: an agent that does not start was reverted to {V2}; the guest kept serving")

    # ---- The guest bundle on the shipped node ------------------------------------------------
    # Every briard binary the guest runs rides the briard chain: the install laid guest-<V>/bin/
    # and the `guest` link beside qemu's. The image boots on its firmware (briard-guest-firmware,
    # the push protocol and the one binary it bakes); the guest agent, reverse proxy and dashboard
    # exist only once pushed. The first bring-up dressed it: the host staged the release set,
    # proved each copy with its own --test-launch, armed the set and restarted the agent, whose
    # trial start is the verdict on the reverse proxy and dashboard; the next handshake reported
    # the bundle. Both host updates above carried the same bundle bytes, hash-skipped ("updated to
    # ... (agent, qemu)", no guest), so the guest still runs the tree the install laid, and is
    # reported as running the current release bundle, which it is.
    def dressed_count(release):
        return int(host.succeed(f"journalctl -u briard-agent | grep -c 'guest bundle: the guest runs {release} (dressed)' || true").strip())
    def console_count(pattern):
        return int(host.succeed(f"tr -d '\\r' < /var/log/briard-guest-console.log | grep -acE '{pattern}' || true").strip())
    host.succeed("test -L /opt/briard/agent/guest")
    assert host.succeed("readlink /opt/briard/agent/guest").strip() == f"guest-{V}", "the guest bundle tree is not named by its release"
    for b in ["briard-guest-agent", "briard-reverse-proxy", "briard-dashboard"]:
        host.succeed(f"test -x /opt/briard/agent/guest-{V}/bin/{b}")
    host.succeed(f"journalctl -u briard-agent | grep -q 'guest bundle: the guest runs its firmware; dressing it with {V}'")
    host.succeed(f"journalctl -u briard-agent | grep -q 'guest bundle: {V} staged, proven and activated'")
    assert dressed_count(V) >= 1, "the first bring-up never reported the guest dressed"
    # Reported as the running agent release: the status line names the bundle by the release whose
    # bundle the guest runs, the host's own version on the committed tree. This rig's pins re-use
    # {V}'s agent bytes, so the agent, and the bundle it reports, stays {V}; the tree name is what
    # the update moved.
    try:
        host.wait_until_succeeds(f"journalctl -u briard-agent | grep 'status node=' | tail -1 | grep -q 'bundle={V}'", timeout=60)
    except Exception:
        print("=== the guest's pivot and units ===")
        print(guest_console("briard-bin|briard-guest-agent|briard-reverse-proxy|briard-dashboard|Control process|Failed"))
        print(host.succeed("journalctl -u briard-agent | grep -E 'guest bundle|status node=' | tail -12"))
        raise
    # ...and the pivot agrees: every guest boot starts as firmware (the picker says `baked` once
    # per boot for the agent unit, naming briard-guest-firmware, the only binary in the image).
    # This rig boots the guest several times (install, cattle reinstall, router-down restart, lease
    # move); each the host dressed from firmware exactly once, and each committed the whole set
    # once, naming every binary. Within a boot every guest-agent restart ran the committed pushed
    # binary, never the firmware. Read from the console, which holds every boot (qemu appends to
    # it and never truncates).
    boots = console_count("briard-bin-exec: briard-guest-agent: baked .*briard-guest-firmware")
    firmware_dresses = int(host.succeed("journalctl -u briard-agent | grep -c 'guest bundle: the guest runs its firmware' || true").strip())
    pushed_starts = console_count("briard-bin-exec: briard-guest-agent: pushed")
    # The commit is the guest agent's own line ("bin: committed <release>: <names>").
    commits = console_count("bin: committed")
    set_commits = console_count("bin: committed .*briard-dashboard.*briard-reverse-proxy.*briard-guest-agent")
    if boots < 1 or firmware_dresses != boots or commits != boots or set_commits != commits or pushed_starts < 1:
        print("=== the guest's pivot and units ===")
        print(guest_console("briard-bin|briard-guest-agent|briard-reverse-proxy|briard-dashboard|Control process|Failed"))
        raise Exception(f"pivot account: {boots} boots, {firmware_dresses} dresses from firmware, {commits} commits ({set_commits} of the whole set), {pushed_starts} pushed starts -- want one dress and one whole-set commit per boot, and restarts on the pushed binary")
    print(f"{boots} guest boots, each dressed from firmware once and committed as ONE set in the guest; {pushed_starts} guest-agent restarts ran the pushed binary")

    # The reverse proxy and the dashboard have no firmware: they run only pushed copies -- the
    # picker never said `baked` for either, nor NOT DRESSED YET, since the host dresses before the
    # node can promote.
    for name in ["briard-reverse-proxy", "briard-dashboard"]:
        assert console_count(f"briard-bin-exec: {name}: baked") == 0, f"{name} ran a baked copy: the image is not supposed to have one"
        assert console_count(f"briard-bin-exec: {name}: NOT DRESSED YET") == 0, f"{name} started before the guest was dressed"
        assert console_count(f"briard-bin-exec: {name}: pushed") >= 1, f"{name} never ran a pushed copy"
    print("the reverse proxy and the dashboard ran only pushed copies -- the image bakes the firmware alone")

    # A guest relaunch lands on firmware and is dressed again: the guest scratch is emptied every
    # boot, so nothing pushed survives. Stopped, because the host recovery relaunches a stopped
    # guest through its own bring-up, which dresses it.
    before = dressed_count(V)
    # CONVERGED is already in the journal, so a bare grep proves nothing about this promotion:
    # count past what is there, and count before the stop, because the relaunched guest converges
    # within a second of being dressed (measured: 0.9 s), inside the poll interval of the wait
    # below, so a count taken after it already includes the converge it should detect.
    converges = int(host.succeed("journalctl -u briard-agent | grep -c CONVERGED || true").strip())
    host.succeed("systemctl stop briard-guest.service")
    host.wait_until_succeeds(f"[ $(journalctl -u briard-agent | grep -c 'guest bundle: the guest runs {V} (dressed)') -gt {before} ]", timeout=600)
    host.wait_until_succeeds(f"[ $(journalctl -u briard-agent | grep -c CONVERGED) -gt {converges} ]", timeout=600)
    try:
        client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    except Exception:
        print("=== the re-dressed guest: pivot, reverse proxy, dashboard and chain ===")
        print(guest_console("briard-bin|briard-guest-agent|briard-reverse-proxy|briard-dashboard|briard-vip|briard-apps|drbd-reactor|drbd-promote|promotion-hold|masked|Failed|failed"))
        print(host.succeed("journalctl -u briard-agent | grep -E 'guest bundle|status node=|CONVERGED' | tail -20"))
        raise
    print("a restarted guest came up as firmware and was dressed again")

    # The cheap gate: a staged copy failing its own --test-launch refuses the dress before
    # anything is armed. The pin carries a briard-dashboard that fails the test launch, and nothing
    # else broken: `bin.test` runs each copy, the dashboard exits 1, the guest discards the whole
    # staged set, nothing is armed or restarted, the house keeps serving on its set, and the host
    # records the refusal like any other.
    V4 = "v3.99123100.b138gate"
    host.succeed(f"mkdir -p /root/gbundle4/bin && cp /opt/briard/agent/guest-{V}/bin/briard-guest-agent /opt/briard/agent/guest-{V}/bin/briard-reverse-proxy /root/gbundle4/bin/")
    host.succeed("printf '#!/bin/sh\\necho \"dashboard: test launch: no\" >&2\\nexit 1\\n' > /root/gbundle4/bin/briard-dashboard && chmod 755 /root/gbundle4/bin/*")
    publish_pin(V4, "/root/bundle2", "/root/gbundle4")
    trials_before = console_count("briard-bin-exec: briard-guest-agent: TRIAL")
    out = host.succeed(f"/opt/briard/agent/briard-agent update -to {V4}").strip()
    assert out == f"updated to {V4} (agent, guest)", f"briard update said: {out!r}"   # qemu unchanged since V2: not fetched
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'guest bundle: PUSH REFUSED'", timeout=300)
    host.succeed("journalctl -u briard-agent | grep 'PUSH REFUSED' | grep -q 'briard-dashboard failed its test launch'")
    host.succeed(f"test \"$(cat /opt/briard/agent/guest.reverted)\" = {V4}")
    assert console_count("briard-bin-exec: briard-guest-agent: TRIAL") == trials_before, "the refused set was armed and trialled anyway"
    host.wait_until_succeeds(f"journalctl -u briard-agent | grep 'status node=' | tail -1 | grep -q 'bundle={V}'", timeout=60)
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    print(f"{V4}: a staged binary that failed its test launch refused the dress with nothing armed; the house kept serving on {V}'s set")

    # The trial verdict: a copy that passes the cheap gate and fails its real launch is caught by
    # the trial agent on the primary (which a single node always is). The pin briard-reverse-proxy
    # answers --test-launch 0 and exits 1 otherwise: staged, proven, armed; the agent restarts as a
    # trial and try-restarts the proxy, whose start fails; the agent exits 1 without opening the
    # port and the committed agent returns. The proxy reverted by its own auto-restart (flag
    # consumed), the returning agent discards the staged set, and nothing demoted: one failed
    # start is well inside the unit budget, and briard-promotion-hold fires only once it is spent.
    V5 = "v3.99123100.b138door"
    # The baseline for "nothing masked the promoter target", counted so an earlier mask line in
    # the console cannot fail it. On this lone node the chain target is briard-chain.target, not
    # drbd-services@r0.target, and the hold masks nothing alone (configuration.nix,
    # briard-hold-refuse), so the count is 0 before and after whatever the guest does.
    masked_before = console_count("drbd-services@r0.target is masked")
    # ...and the same for "the reverse proxy came back on the committed binary": it has run pushed
    # copies all run long, so only an increase across the trial says anything.
    proxy_pushed_before = console_count("briard-bin-exec: briard-reverse-proxy: pushed")
    host.succeed(f"mkdir -p /root/gbundle5/bin && cp /opt/briard/agent/guest-{V}/bin/briard-guest-agent /opt/briard/agent/guest-{V}/bin/briard-dashboard /root/gbundle5/bin/")
    host.succeed("printf '#!/bin/sh\\ncase \"$1\" in --test-launch) echo ok; exit 0;; esac\\necho \"reverse-proxy: not today\" >&2\\nexit 1\\n' > /root/gbundle5/bin/briard-reverse-proxy && chmod 755 /root/gbundle5/bin/*")
    publish_pin(V5, "/root/bundle2", "/root/gbundle5")
    out = host.succeed(f"/opt/briard/agent/briard-agent update -to {V5}").strip()
    assert out == f"updated to {V5} (agent, guest)", f"briard update said: {out!r}"
    assert host.succeed("readlink /opt/briard/agent/guest").strip() == f"guest-{V5}", "the guest link did not move on commit"
    host.wait_until_succeeds(f"journalctl -u briard-agent | grep -q 'guest bundle: {V5} staged, proven and activated'", timeout=300)
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'guest bundle: PUSH REVERTED'", timeout=300)
    host.succeed(f"journalctl -u briard-agent | grep 'PUSH REVERTED' | grep -q 'came back on {V}, not {V5}'")
    host.succeed(f"grep -q '\"version\":\"{V5}\"' /opt/briard/agent/manifest.json")   # the host DID commit V5
    # The guest's account: the trial ran, the proxy refused it, the returning agent cleaned up.
    # Either refusal wording counts -- a race the product handles both ways: if the restart job
    # reports the failed start, the verdict says "failed its real launch"; if the auto-restart
    # repaired the unit first, the job succeeds and only the picker's marker gives it away ("running
    # the pushed binary, not the staged one"). This rig sees the second; asserting only the first
    # would pass on one timing and fail on the other.
    try:
        # `grep -c`, never `grep -q`, on this file: -q ends grep at the first hit and the `tr`
        # feeding it dies on the closed pipe, which the shell reports as a failed command even
        # though the line is there.
        host.wait_until_succeeds("[ $(tr -d '\\r' < /var/log/briard-guest-console.log | grep -ac 'trial of .* REFUSED: briard-reverse-proxy' || true) -gt 0 ]", timeout=120)
        host.wait_until_succeeds("[ $(tr -d '\\r' < /var/log/briard-guest-console.log | grep -ac 'a staged set .* was left behind by a trial that did not commit' || true) -gt 0 ]", timeout=120)
        # ...and the set did not commit: no commit line names the refused release.
        assert console_count(f"bin: committed {V5}") == 0, f"{V5} committed despite a refused trial"
    except Exception:
        print(guest_console("briard-bin|trial|briard-guest-agent|briard-reverse-proxy|briard-dashboard|Failed"))
        raise
    # A failed upgrade never demotes, and this is the assertion that says so. The hold is a guest
    # unit, so it is read from the guest console (a host-journal check could never fail). The
    # mechanism: the proxy restarts in direct mode, which skips OnFailure, so it fires
    # briard-promotion-hold only once its start budget is spent (agent/guestagent/units.go,
    # chainMemberFailure), and a trial costs one failed start (agent/guestfirmware/bin.go). Assert
    # the outcome, not the mechanism: the node still holds the house. On this lone node the masked
    # count cannot move (see its baseline); primary=true on the last status line and the proxy
    # back on its committed binary are what this rig can see.
    try:
        assert console_count("drbd-services@r0.target is masked") == masked_before, "a failed guest upgrade masked the promoter target -- the node demoted and cannot take the house back"
        host.succeed("journalctl -u briard-agent | grep 'status node=' | tail -1 | grep -q 'primary=true'")
        # The proxy is back on the committed binary by its own restart: a new `pushed` line since
        # the baseline (counted, not `grep -q`, for the closed-pipe reason above).
        assert console_count("briard-bin-exec: briard-reverse-proxy: pushed") > proxy_pushed_before, "the reverse proxy never came back on the committed binary after the refused trial"
    except Exception:
        print("=== what the guest did around the refused trial ===")
        print(host.succeed(
            "tr -d '\\r' < /var/log/briard-guest-console.log 2>/dev/null | "
            "grep -aE 'briard-promotion-hold|promotion hold|masked|Condition|drbd-promote|drbd-services|demote|briard-reverse-proxy|briard-dashboard|briard-vip|briard-apps|trial|briard-bin' "
            "| tail -120 || true"
        ))
        raise
    # The house kept serving, the proxy answering at the VIP on its committed binary: the assertion
    # the whole item is for, so its failure prints the guest account of the units, not just a
    # timeout.
    try:
        client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    except Exception:
        print("=== the guest's pivot, units and chain after the refused trial ===")
        print(guest_console("briard-bin|trial|briard-guest-agent|briard-reverse-proxy|briard-dashboard|briard-vip|briard-apps|drbd-reactor|drbd-promote|Failed|failed|start-limit"))
        print("=== the host's account ===")
        print(host.succeed("journalctl -u briard-agent | grep -E 'guest bundle|status node=|CONVERGED|promotion' | tail -25"))
        raise
    host.wait_until_succeeds(f"journalctl -u briard-agent | grep 'status node=' | tail -1 | grep -q 'bundle={V}'", timeout=60)
    print(f"{V5}: a reverse proxy that passed the cheap gate and failed its real launch refused the trial; it reverted itself, the node never demoted, and the house served throughout")

    # The revert is permanent. The guest fallback lasts one launch; the host record outlives it:
    # the refusal written beside the tree, the last good tree recorded, and a fresh launch dressed
    # with the good bundle -- never {V5} again, never left on firmware. Alerted once. Proven by
    # relaunching the guest.
    host.succeed(f"test \"$(cat /opt/briard/agent/guest.reverted)\" = {V5}")
    assert host.succeed("cat /opt/briard/agent/guest.good").strip() == V, "the last good tree is not recorded"
    # Alerted, and the alert names this refusal -- not one alert per refused release: the observe
    # loop raises it off the durable record, so a refusal superseded before the next tick (V4's,
    # seconds later, by V5's) is never announced, rightly, since what the household needs telling
    # is the state it is in. A bare grep would also match V4's alert text, so match the release.
    host.wait_until_succeeds(f"journalctl -u briard-agent | grep 'Briard: guest bundle push failed' | tail -1 | grep -q '{V5}'", timeout=60)
    before = dressed_count(V)
    reverts = int(host.succeed("journalctl -u briard-agent | grep -c 'PUSH REVERTED' || true").strip())
    relaunched = int(host.succeed("journalctl -u briard-agent | grep -c 'guest-recovery: guest relaunched and converged' || true").strip())
    host.succeed("systemctl stop briard-guest.service")
    host.wait_until_succeeds(f"journalctl -u briard-agent | grep -q '{V5} reverted before; dressing the guest with the last good bundle {V} instead'", timeout=600)
    host.wait_until_succeeds(f"[ $(journalctl -u briard-agent | grep -c 'guest bundle: the guest runs {V} (dressed)') -gt {before} ]", timeout=600)
    # The loop is back, not only the guest. The relaunch is the recovery ladder's, which answers
    # the door until it returns, refusing every verb with what it is doing, so its "converged" line
    # is not yet a usable door; the observe loop's first status line after it is. Counted, so an
    # earlier relaunch's line cannot satisfy the wait, and never a bare CONVERGED, which every
    # earlier bring-up logged.
    host.wait_until_succeeds(
        f"[ $(journalctl -u briard-agent | grep -c 'guest-recovery: guest relaunched and converged') -gt {relaunched} ] && "
        "journalctl -u briard-agent | grep -E 'guest-recovery: guest relaunched and converged|status node=' | tail -1 | grep -q 'status node='",
        timeout=600)
    assert int(host.succeed("journalctl -u briard-agent | grep -c 'PUSH REVERTED' || true").strip()) == reverts, "the refused bundle was pushed again after a relaunch"
    host.wait_until_succeeds(f"journalctl -u briard-agent | grep 'status node=' | tail -1 | grep -q 'bundle={V}'", timeout=60)
    host.succeed(f"test -d /opt/briard/agent/guest-{V}")   # the good tree survives the launch's prune
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    print(f"a relaunch after the refusal was dressed with the last good bundle {V}, not {V5} and not the firmware; the refusal was alerted once")

    # ---- The guest chain on the shipped node -------------------------------------------------
    # The installed guest manifest names the closure the image boots; the install seeded the
    # node-local record from it; the config carries the channel root. `briard update -vm` resolves
    # vm/stable, verifies it, compares min_briard with this briard and the closure with what the
    # guest runs, and says this node is already running it.
    host.succeed("grep -q '\"system\":\"${guestDisk.system}\"' /opt/briard/guest-image/manifest.json")
    host.succeed(f"grep -q '\"min_briard\":\"{V}\"' /opt/briard/guest-image/manifest.json")
    host.succeed("cmp /opt/briard/guest-image/manifest.json /var/lib/briard/guest-release.json")
    host.succeed("grep -q '^CHANNEL_URL=http://127.0.0.1:8099$' /opt/briard/config.env")
    out = host.succeed("/opt/briard/agent/briard-agent update -vm").strip()
    assert f"already running {GV}" in out, f"briard update -vm said: {out!r}"
    # A guest release this host is too old for is refused before anything is staged, naming the
    # remedy: the support window closing on a node must never be silent. And the check can fail:
    # an unknown pin fails too.
    GNEW = "vm.99123100.b86d0000"
    d = f"/srv/vm/{GNEW}/amd64"
    host.succeed(f"mkdir -p {d} && echo not-an-image > {d}/nixos.qcow2.zst")  # a real file (the writer skips links); never fetched, the refusal comes first
    host.succeed(f"/opt/briard/agent/briard-agent --stage-manifest {d} --chain vm --platform amd64 --release {GNEW} --system ${guestDisk.system} --min-briard v3.99123100.zzzzzzz")
    host.succeed(f"{stub} sign /root/release.key {d}/manifest.json | base64 -d > {d}/manifest.json.sig")
    # ⚠️ Wait for the admin door before knocking: after the refused push and relaunch above the
    # agent may still be re-opening its socket, and `host.fail` passes on any non-zero exit,
    # including "cannot reach the agent" -- the refusal never happens, and only the journal grep
    # below notices.
    host.wait_until_succeeds("test -S /run/briard/admin.sock", timeout=60)
    host.wait_until_succeeds("journalctl -u briard-agent | grep 'status node=' | tail -1 | grep -q 'healthy=true'", timeout=120)
    # The refusal's own words, not just a non-zero exit, so it cannot pass for the wrong reason: an
    # unreachable agent, a mistyped release, a verb that no longer exists all fail here.
    out = host.fail(f"/opt/briard/agent/briard-agent update -vm -to {GNEW} 2>&1")
    assert "older than the vm release requires" in out, f"the verb failed, but not on min_briard: {out!r}"
    # And the escalation reached the journal -- a refusal the owner never hears about is the
    # failure mode the alert exists for.
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'guest OS update.*failed; nothing changed.*older than the vm release requires'", timeout=30)
    host.fail("/opt/briard/agent/briard-agent update -vm -to vm.99010100.nothere")
    host.succeed("cmp /opt/briard/guest-image/manifest.json /var/lib/briard/guest-release.json")  # the record never moved
    print("the shipped node resolves its vm chain: already running the installed release; a release needing a newer briard is refused loudly")

    # ---- Convergence restores drift, it does not undo an act -----------------------------------
    # An administratively down device stays down. Not a nicety: a fault harness severs a DRBD link
    # with `ip link set <dev> down`, and an agent re-upping it within a tick heals the partition
    # before DRBD notices. NM flushing our address is drift and is restored; `ip link set X down`
    # is somebody's decision. A real partition is carrier-down, and a carrier-down device is still
    # IFF_UP, so this never fought a pulled cable either way. briard-drbd0, not briard0: a lone
    # node runs no DRBD, so briard-drbd0 carries only its node IP and downing it costs the
    # household (served at the VIP on briard0) nothing. The sleep is meant to be long enough for a
    # re-up: status ticks are 5 s (STATUS_EVERY) but network checks run every third tick
    # (netCheckTicks: 15 s), so 12 s may hold no check.
    host.succeed("ip link set briard-drbd0 down")
    host.sleep(12)  # several status ticks (STATUS_EVERY=5s): long enough for a re-up to happen
    assert "state DOWN" in host.succeed("ip -o link show briard-drbd0"), (
        "convergence re-upped a device something deliberately downed -- this silently heals the "
        "fleet's partition fault and DRBD never sees a peer drop"
    )
    host.succeed("ip link set briard-drbd0 up")

    # ---- Re-parenting: the parent goes away and the guest comes back ---------------------------
    # A rename, not a deletion: the stronger probe. A macvtap child survives its parent being
    # renamed (it re-points as `briard0@eth9`, measured on 6.18), so the devices stay up and
    # attached to the wrong name -- the case `nic.Rebuild`'s delete-first exists for, which an
    # `ip link del eth1` would never reach: Converge is check-first, so an existing child would be
    # reported converged, hanging off a parent the record no longer names.
    # ⚠️ Renaming an up device does not disturb the host L3: address and default route follow the
    # ifindex, so 192.168.1.1 and the gateway stay put -- the point, and also why a re-parent
    # cannot cost the agent its path to the cloud (nothing here moves a host address or route).
    # The test fixture collapses the tier rather than waiting out the shipped five minutes, which
    # buys nothing the first second does not. REPARENT_TIER shrinks durations only: "different
    # subnet" and "cannot tell" stay unreachable, so it cannot prove a re-parent the product
    # forbids.
    host.succeed("printf 'REPARENT_TIER=2s\\n' >> /opt/briard/config.env")
    host.succeed("systemctl restart briard-agent.service")
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    # The LAN fingerprint was recorded at bring-up and names the device we are about to remove.
    host.succeed("grep -q '\"parent\":\"eth1\"' /var/lib/briard/network.json")

    host.succeed("ip link set eth1 name eth9")
    host.succeed("ip -o link show eth9")          # the device is there, under a new name
    host.fail("ip link show eth1")                # ...and the recorded parent is gone
    host.succeed("ip -d link show briard0 | grep -q 'briard0@eth9'")  # the child followed it

    # The tick notices -- qemu never will, which is the whole reason this lives on the tick.
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 're-parenting the guest.s L2 from eth1 onto eth9'",
        timeout=180,
    )
    # The devices were rebuilt on the new parent, not adopted where they lay: same names, and
    # `@eth9` says the delete-and-recreate happened.
    host.wait_until_succeeds("ip -d link show briard0 | grep -q 'briard0@eth9'", timeout=120)
    host.succeed("ip -d link show briard-drbd0 | grep -q 'briard-drbd0@eth9'")
    host.succeed("ip link show briard0 | grep -q ALLMULTI")  # and came back with their flags
    host.succeed("grep -qx 1 /proc/sys/net/ipv6/conf/briard0/disable_ipv6")

    # A re-parent is never a quiet self-heal: the household apps just moved segments, and a node
    # that healed silently is one whose next problem starts with nobody knowing.
    # ⚠️ Waited for, not read once: the alert and the record land at the end of the move, after
    # RescueGuest stopped and relaunched the guest, while the devices above are rebuilt before it;
    # read the instant the re-parent line appears, they are a whole guest boot too early.
    host.wait_until_succeeds(
        "/opt/briard/agent/briard-agent alerts | grep -q 'different network device'",
        timeout=600,
    )

    # The record now names the new parent, so a later re-parent paces itself against this LAN
    # rather than against one that no longer exists.
    host.wait_until_succeeds("grep -q '\"parent\":\"eth9\"' /var/lib/briard/network.json", timeout=600)

    # And the household gets its apps back, off-box, at the same address -- what the block exists
    # to make non-vacuous: the guest was relaunched onto devices that did not exist when it last
    # ran.
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=600)
    print("the guest's L2 re-parented from eth1 onto eth9 and the VIP answers off-box again")

    # ---- Re-parenting on a pulled cable: carrier-down counts, with a successor in hand ---------
    # A pulled cable leaves the parent present and IFF_UP, carrying nothing, so the live rule reads
    # carrier: "gone" is absent or without it. eth2 is a second uplink on the same LAN at a worse
    # metric. In order: a cable coming in moves nothing; a restart under the standing guest keeps
    # its parent; out and back inside the tier moves nothing; out for the tier moves the guest to
    # eth2; the cable back after that moves nothing again. block()/unblock() are the driver's
    # set_link on the first LAN NIC, eth9 since the rename.
    host.succeed(
        "ip addr flush dev eth2",  # the framework numbers every vlan NIC, and eth2 drew the client's .2
        "ip addr add 192.168.1.4/24 dev eth2",
        "ip link set eth2 up",
        "ip route add default via 192.168.1.3 dev eth2 metric 200",
    )
    host.wait_until_succeeds("grep -qx 1 /sys/class/net/eth2/carrier", timeout=30)
    # The reparenter runs on the network checks, every third status tick (15 s): 12 s may hold none.
    host.sleep(12)  # several status ticks
    host.fail("journalctl -u briard-agent | grep -q 'onto eth2'")
    host.succeed("ip -d link show briard0 | grep -q 'briard0@eth9'")

    # A tier long enough to pull and return the cable inside it. A restart under the live guest
    # takes the hot path, which reads the parent off the device rather than selecting.
    host.succeed("sed -i 's/^REPARENT_TIER=.*/REPARENT_TIER=60s/' /opt/briard/config.env")
    restarted_at = host.succeed("date '+%Y-%m-%d %H:%M:%S'").strip()
    host.succeed("systemctl restart briard-agent.service")
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since '{restarted_at}' | grep -q 'already up on eth9'", timeout=120)
    # The record is what paces the move: taken with the cable in, so the gateway MAC is in it and
    # the move below is the same-wire tier's.
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since '{restarted_at}' | grep -q 'recorded this LAN \\[eth9 .*gw=192.168.1.3/52:'",
        timeout=300)
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)

    host.block()
    host.wait_until_succeeds("grep -qx 0 /sys/class/net/eth9/carrier", timeout=30)
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'eth9 has lost its link'", timeout=60)
    host.unblock()
    host.wait_until_succeeds("journalctl -u briard-agent | grep -q 'eth9 is back; nothing to re-parent'", timeout=60)
    host.sleep(70)  # past the tier the pull started: the return cancelled it
    host.fail("journalctl -u briard-agent | grep -q 'onto eth2'")
    host.succeed("ip -d link show briard0 | grep -q 'briard0@eth9'")

    host.block()
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 're-parenting the guest.s L2 from eth9 onto eth2'", timeout=240)
    host.wait_until_succeeds("ip -d link show briard0 | grep -q 'briard0@eth2'", timeout=120)
    host.succeed("ip -d link show briard-drbd0 | grep -q 'briard-drbd0@eth2'")
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'hung off eth9, which disappeared or lost its link'",
        timeout=600,
    )
    host.wait_until_succeeds("grep -q '\"parent\":\"eth2\"' /var/lib/briard/network.json", timeout=600)
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=600)

    unblocked_at = host.succeed("date '+%Y-%m-%d %H:%M:%S'").strip()
    host.unblock()
    host.wait_until_succeeds("grep -qx 1 /sys/class/net/eth9/carrier", timeout=30)
    # As above, 12 s may hold no network check; a move back would also wait out the 60 s tier.
    host.sleep(12)  # several status ticks
    host.succeed("ip -d link show briard0 | grep -q 'briard0@eth2'")
    host.fail(f"journalctl -u briard-agent --since '{unblocked_at}' | grep -q 'onto eth9'")
    # The VIP may have moved, by the rig lease, not the re-parent: the 2-minute lease expires during
    # a pull past the tier, dhcpcd keeps the expired address while it DISCOVERs, so dnsmasq's ping
    # finds it taken and offers the next one. Re-read the lease; the reinstall below resumes it.
    moved, _, _ = wait_for_moved_lease()
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=300)
    print("a pulled cable re-parented the guest onto the second uplink after the tier, and nothing else moved it")

    # ---- Uninstall keeps your data, and a reinstall resumes it --------------------------------
    # The default uninstall, run last because it ends the node -- one updated, restarted and
    # re-parented, as a real one is by removal time. Everything install.sh and the agent made must
    # be gone except the pet dir; eth9 and eth2 lines are the test rename and second uplink.
    uninstalled_at = host.succeed("date '+%Y-%m-%d %H:%M:%S'").strip()
    host.succeed("/usr/local/bin/briard uninstall -yes")
    residue = {e for e in footprint() - pristine
               if e != "/var/lib/briard" and "eth9" not in e and "eth2" not in e}
    assert not residue, f"uninstall left these behind: {sorted(residue)}"
    # Kept, and kept whole: the same volume, and the identity it is keyed to.
    assert fsid(host) == pre, "the data volume changed across an uninstall that was told to keep it"
    assert host.succeed("cat /var/lib/briard/flock-name").strip() == flock_name
    # ...and the guest was powered down off it, not cut: the unit's ExecStop said so. From the
    # uninstall on only -- the cattle reset above stopped the same unit the same way.
    host.succeed(f"journalctl -u briard-guest.service --since '{uninstalled_at}' | grep -q 'guest powered off cleanly'")
    client.wait_until_fails(f"curl -fsS --max-time 3 http://{moved}/healthz", timeout=60)

    # What the kept dir is for: the same one command brings the same home back -- its name, its
    # address (the flock id keys the lease) and its volume -- on a host with nothing else of ours.
    host.succeed(
        "${channelEnv} "
        "BRIARD_UNIT_DIR=/run/systemd/system sh ${installScript}"
    )
    client.wait_until_succeeds(f"curl -fsS http://{moved}/healthz", timeout=600)
    assert fsid(host) == pre, "the reinstall did not reattach the volume the uninstall kept"
    print("uninstall kept the data; a reinstall resumed the same home at the same address")
  '';
}
