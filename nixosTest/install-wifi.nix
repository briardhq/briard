# The free-local install on a WIRELESS station: the ipvtap substrate, over real 802.11 framing.
#
# A pure delta over install-macvtap.nix, which owns the mode-independent chain. What this proves:
#   1. THE SUBSTRATE IS DERIVED FROM THE DEVICE. Nothing tells the install it is on Wi-Fi: the
#      host's default route is on wlan0, a genuine 802.11 station, so the selector picks it and the
#      agent builds ipvtap children on it. The report card ADMITS it, yellow, with the reason.
#   2. THE ACCESS POINT IS REAL. mac80211_hwsim radios, hostapd as the access point and
#      wpa_supplicant on the host's station. The access point carries one MAC per station by
#      itself; nothing here imitates that rule. A control on the same radio shows it: a macvlan
#      child (a MAC of its own) cannot reach the router, an ipvlan child (the station's MAC) can.
#   3. THE FLOCK HOLDS ITS OWN LEASE BEHIND THE HOST'S MAC, from a router that keys leases on
#      chaddr alone (dnsmasq --dhcp-ignore-clid, the shape of an ISP router). The host asks for it
#      under the flock's chaddr from the station's L2 source, and hands it to the guest, whose own
#      DHCP client never runs on this substrate.
#   4. THE COPY WORKS: the household reaches the guest at its VIP, and the lease RENEWS -- by
#      INIT-REBOOT from the host, whose broadcast ACK needs nothing copied.
#   5. THE HOST STAYS ON ITS LAN, and reaches its guest over the private link.
#
# With `refuse`, as install-wifi-refuse, the router leases only machines it knows (dnsmasq
# --dhcp-ignore=tag:!known, the station's MAC known), so it leases the flock's chaddr nothing.
# That proves the refusal end to end -- the installer ends on the agent's words, nothing is copied,
# the host keeps its LAN, the doctor says why -- and then the remedy those words name:
# `briard config set vip`, and the named address serves.
#
# The household (router, access point, a client) lives in a network namespace inside the one VM,
# holding the second radio: hwsim radios share one medium, so a second VM would hear nothing.
#
# What a radio here cannot say, and a real access point must: real drivers and firmware, and an
# access point that proxy-ARPs for its stations. The host's DHCP client here is dhcpcd.
#
# Heavy (a nested guest boot on the bundled qemu), rides the `install` nightly tag.
# Run: nix build .#tests.install-wifi -L (or .#tests.install-wifi-refuse)
{ pkgs, guestDisk, agent, qemuBundle, guestBundle, refuse ? false }:
let
  # The staging dir install.sh reads with BRIARD_ARTIFACTS, laid out as install-bridge.nix's.
  staging = pkgs.runCommand "briard-install-staging-wifi" {
    nativeBuildInputs = [ pkgs.gnutar ];
  } ''
    mkdir -p "$out/qemu"
    cp ${agent}/bin/briard-agent "$out/briard-agent"
    cp ${../scripts/briard-net-wrap.sh} "$out/briard-net-wrap"
    cp ${../scripts/units/briard-agent.service}  "$out/briard-agent.service"
    cp ${../scripts/units/briard-update.service} "$out/briard-update.service"
    cp ${../scripts/units/briard-update.timer}   "$out/briard-update.timer"
    cp ${../scripts/agent/briard-update}  "$out/briard-update"
    cp -r ${qemuBundle}/. "$out/qemu/"
    tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner \
        -cf "$out/guest-bundle.tar" -C ${guestBundle} .
    cp ${guestDisk}/nixos.qcow2 "$out/nixos.qcow2"
  '';
  installScript = ../scripts/install.sh;
  # An OPEN network: the access point's one-MAC-per-station rule is the same with or without a
  # passphrase, and a passphrase in the tree is a secret a scanner cannot tell from a real one.
  ssid = "briard-test";
  hostapdConf = pkgs.writeText "hostapd.conf" ''
    interface=wlan1
    driver=nl80211
    ssid=${ssid}
    hw_mode=g
    channel=6
  '';
in
pkgs.testers.runNixOSTest {
  name = if refuse then "install-wifi-refuse" else "install-wifi";
  skipTypeCheck = true; # dynamic asserts, systemd units created at runtime

  nodes.host =
    { lib, ... }:
    {
      virtualisation.memorySize = 6144; # clears the report card's 4 GB floor + the nested guest
      virtualisation.cores = 4;
      virtualisation.diskSize = 12288;
      virtualisation.qemu.options = [ "-cpu" "host" ]; # vmx -> nested KVM in L1
      virtualisation.vlans = [ ]; # no wired LAN at all: the station is the only way out
      # Two radios on one simulated medium: wlan0 is the host's station, wlan1 the household's
      # access point (moved into the `lan` namespace by the test script).
      boot.kernelModules = [ "mac80211_hwsim" ];
      networking.useDHCP = false;
      # mkOverride 0: qemu-vm.nix disables wireless with mkVMOverride.
      networking.wireless = {
        enable = lib.mkOverride 0 true;
        interfaces = [ "wlan0" ];
        networks.${ssid} = { };
      };
      # ⚠️ THE DEFAULT ROUTE IS THE ASSERTION: it is what makes the selector pick wlan0, and so
      # what derives the substrate. It comes from the household's router, by DHCP, as at home.
      networking.interfaces.wlan0.useDHCP = true;
      environment.systemPackages = [
        pkgs.iproute2 pkgs.iputils pkgs.socat pkgs.kmod pkgs.curl pkgs.iw pkgs.hostapd pkgs.dnsmasq
      ];
    };

  testScript = ''
    ${import ./multicast-receive.nix { inherit pkgs; }}
    host.wait_for_unit("multi-user.target")
    host.succeed("ls -l /dev/kvm")  # nested KVM present in L1 (the report card gates on it)

    # --- THE HOUSEHOLD: router + access point + a client, in their own namespace, on radio 2 ---
    host.succeed("ip netns add lan && ip -n lan link set lo up")
    phy = host.succeed("cat /sys/class/net/wlan1/phy80211/name").strip()
    host.succeed(f"iw phy {phy} set netns name lan")
    host.succeed("ip -n lan addr add 192.168.7.1/24 dev wlan1")
    host.succeed("ip netns exec lan hostapd -B -P /run/lan-hostapd.pid ${hostapdConf}")
    # A two-minute lease (dnsmasq's floor), so a renewal falls inside the test: T1 is at one minute.
    # THE ROUTER IS THE FORK between the two tests. Both key leases on chaddr alone, ignoring
    # client-ids as an ISP router does. The refusing one also leases only machines it knows, and
    # it knows only the station -- so it leases the flock's chaddr nothing.
    station_mac = host.succeed("cat /sys/class/net/wlan0/address").strip()
    router = "--dhcp-ignore-clid " + (
        f"--dhcp-host={station_mac} --dhcp-ignore=tag:!known " if ${if refuse then "True" else "False"} else ""
    )
    host.succeed(
        "ip netns exec lan dnsmasq --interface=wlan1 --bind-interfaces --port=0 --no-resolv "
        "--dhcp-range=192.168.7.100,192.168.7.150,2m --dhcp-option=3,192.168.7.1 --dhcp-authoritative "
        f"{router}"
        "--dhcp-leasefile=/tmp/lan.leases --log-dhcp --log-facility=/tmp/lan-dnsmasq.log "
        "--pid-file=/run/lan-dnsmasq.pid --user=root"
    )

    # The host associates and takes its own lease, and its default route is now on the station.
    host.wait_until_succeeds("iw dev wlan0 link | grep -q 'Connected to'", timeout=120)
    host.wait_until_succeeds("ip -4 route show default | grep -qw wlan0", timeout=120)
    host_ip = host.succeed("ip -o -4 addr show dev wlan0 | awk '{print $4}' | cut -d/ -f1").strip()
    host.succeed("test -e /sys/class/net/wlan0/phy80211")  # genuinely wireless: no forcing variable
    host.succeed(f"ip netns exec lan ping -c1 -W2 {host_ip}")
    print(f"host on the household's Wi-Fi at {host_ip}, station {station_mac}")

    # --- THE CONTROL PAIR, on the same radio, before briard exists ---
    # A child with a MAC of its own (macvlan: what a macvtap gives the guest) cannot reach the
    # router through the access point. A child on the station's MAC (ipvlan: what ipvtap gives
    # it) can. Both have a static address and a namespace of their own, so neither is the host.
    # One after the other: a parent cannot be a macvlan port and an ipvlan port at once.
    host.succeed(
        "ip netns add ctl-mac && ip link add link wlan0 name ctlmac type macvlan mode bridge && "
        "ip link set ctlmac netns ctl-mac && ip -n ctl-mac addr add 192.168.7.200/24 dev ctlmac && "
        "ip -n ctl-mac link set ctlmac up"
    )
    host.fail("ip netns exec ctl-mac ping -c3 -W2 192.168.7.1")
    host.succeed("ip -n ctl-mac link del ctlmac && ip netns del ctl-mac")
    host.succeed(
        "ip netns add ctl-ipv && ip link add link wlan0 name ctlipv type ipvlan mode l2 bridge && "
        "ip link set ctlipv netns ctl-ipv && ip -n ctl-ipv addr add 192.168.7.201/24 dev ctlipv && "
        "ip -n ctl-ipv link set ctlipv up"
    )
    host.wait_until_succeeds("ip netns exec ctl-ipv ping -c1 -W2 192.168.7.1", timeout=30)
    host.succeed("ip -n ctl-ipv link del ctlipv && ip netns del ctl-ipv")
    print("control: a second MAC dies at the access point; the station's MAC carries an ipvlan child")

    # --- THE INSTALL: one command, no BRIARD_NIC, no BRIARD_VIP_ADDR -- the VIP is DHCP's ---
    def install():
        code, out = host.execute(
            "BRIARD_ARTIFACTS=${staging} BRIARD_UNIT_DIR=/run/systemd/system "
            "sh ${installScript} 2>&1"
        )
        print(out)
        # What the household's router saw, and the guest's own side of the DHCP exchange, BEFORE
        # any verdict: a lease question is only answerable from both ends.
        print(host.succeed("cat /tmp/lan-dnsmasq.log /tmp/lan.leases"))
        print(host.execute("grep -a -i 'dhcpcd\\|briard-vip' /var/log/briard-guest-console.log | tail -60")[1])
        return code, out

    code, out = install()
    # DELTA 1: the card admitted the station with a WARN and the one Wi-Fi sentence, and the agent built
    # ipvtap children in the holding namespace -- absent from the host's own.
    assert "wlan0 is Wi-Fi" in out and "Wi-Fi can be unstable; we recommend a wired connection" in out, \
        "the report card did not warn about the wireless station with its advice"
    assert "[WARN  ] network" in out, "the network check is not a WARN"
    host.succeed("journalctl -u briard-agent | grep -q \"the guest's L2 hangs off wlan0 (ipvtap\"")
    for dev in ("briard-drbd0", "briard0"):
        host.succeed(f"ip -n briard-hold -d link show {dev} | grep -q 'ipvtap  mode l2 bridge'")
        host.fail(f"ip link show {dev}")
    host.succeed("ip link show briard-priv0")  # the private link, exactly as macvtap has it
'' + (if refuse then ''

    # --- THE REFUSAL: the router leases the flock's chaddr nothing. ---
    assert code != 0 and "cannot serve your home" in out and "briard config set vip <address>/24" in out, \
        f"the installer did not end on the agent's refusal (exit {code})"
    assert "your router leases Briard nothing of its own" in out, "the refusal does not say why"
    # The host DID ask, under a chaddr that is not the station's -- the router logged ignoring it.
    import re
    asked = set(re.findall(r"DHCPDISCOVER\(wlan1\) ([0-9a-f:]{17}) ignored", host.succeed("cat /tmp/lan-dnsmasq.log")))
    assert asked and station_mac not in asked, f"no DISCOVER under the flock's chaddr was refused: {asked}"
    # The guest was given no address and started no client of its own. Waited for, not read once:
    # the agent logs the refusal -- which ends the installer's wait -- before it pushes the guest
    # its network, and the push is what prints this line.
    host.wait_until_succeeds("grep -a -q 'the host holds this address.s lease and has none to give' /var/log/briard-guest-console.log", timeout=60)
    # Nothing was copied, and the host is still on its LAN, still holding its lease.
    host.fail("ip -n briard-hold -4 addr show dev briard0 | grep -q inet")
    host.succeed(f"ip netns exec lan ping -c1 -W2 {host_ip}")
    host.succeed("ip -o -4 addr show dev wlan0 | grep -qw " + host_ip)
    # The doctor says why and what to do.
    code, doc = host.execute("/usr/local/bin/briard doctor 2>&1")
    print(doc)
    assert code != 0 and "leases Briard nothing of its own" in doc and "config set vip" in doc, \
        "the doctor does not carry the refusal"
    print("refused, said so at install, kept the host on its LAN")

    # --- THE REMEDY, as the refusal words it: name an address outside the router's DHCP range.
    # An address the install's own gate refuses is refused here too, and changes nothing. ---
    code, said = host.execute("/usr/local/bin/briard config set vip 10.9.9.9/24 2>&1")
    print(said)
    assert code != 0 and "not on this machine's LAN" in said, "an off-LAN address was accepted"
    host.fail("grep -q VIP_ADDR /opt/briard/config.env")
    said = host.succeed("/usr/local/bin/briard config set vip 192.168.7.50 2>&1")
    print(said)
    assert "vip is now 192.168.7.50/24" in said, "the bare address did not take the LAN's prefix"
    host.succeed("grep -qx 'VIP_ADDR=192.168.7.50/24' /opt/briard/config.env")
    # The guest restarts, promotes and claims it; the next tick copies it. Reach is the proof.
    host.wait_until_succeeds("ip netns exec lan curl -fsS --max-time 5 http://192.168.7.50/healthz", timeout=180)
    host.succeed("ip -n briard-hold -4 addr show dev briard0 | grep -qw 192.168.7.50/32")
    host.succeed(f"ip netns exec lan ping -c1 -W2 {host_ip}")
    host.succeed("curl -fsS --max-time 5 http://192.168.7.50/healthz")
    print("the named address serves the household over the access point")
'' else ''

    assert code == 0, f"the installer exited {code}"
    assert "network: refused" not in host.succeed("journalctl -u briard-agent -o cat"), \
        "the agent refused the address the router gave its guest"
    host.wait_until_succeeds(
        "journalctl -u briard-agent | grep -q 'primary=true quorate=true.*healthy=true'", timeout=900
    )
    vip = host.succeed(
        "journalctl -u briard-agent -o cat | sed -n 's/.*vip route: \\([0-9.]*\\) reachable.*/\\1/p' | tail -1"
    ).strip()
    assert vip.startswith("192.168.7."), f"no VIP from the household's router: {vip!r}"
    assert vip != host_ip, f"the guest holds the host's own address {vip}"
    print(f"the guest holds {vip}")

    # DELTA 3: two chaddrs behind one station MAC. The host's lease is the station's, the flock's
    # is its own MAC's -- on a router that ignores client-ids -- and it is the HOST that asked:
    # the guest was handed the address and ran no client.
    leases = host.succeed("cat /tmp/lan.leases")
    rows = {f[2]: f for f in (l.split() for l in leases.splitlines()) if len(f) >= 5}
    assert vip in rows and host_ip in rows, f"missing a lease (vip={vip} host={host_ip}): {rows}"
    assert rows[host_ip][1] == station_mac, f"the host's lease is not the station's: {rows[host_ip]}"
    assert rows[vip][1] != station_mac, f"the flock's lease carries the station's chaddr: {rows[vip]}"
    assert rows[vip][3].startswith("briard-"), f"the lease is not the flock's: {rows[vip]}"
    host.succeed(f"journalctl -u briard-agent -o cat | grep -q 'vip lease: {vip}/24 from 192.168.7.1'")
    host.succeed(f"grep -a -q 'briard-vip: dev=.* configured={vip}/24' /var/log/briard-guest-console.log")

    # DELTA 4: the copy. The VIP is held on the service child, and the household reaches the guest.
    host.succeed(f"ip -n briard-hold -4 addr show dev briard0 | grep -qw {vip}/32")
    host.wait_until_succeeds(f"ip netns exec lan curl -fsS --max-time 5 http://{vip}/healthz", timeout=120)
    print("the household reached the guest at its VIP, over the access point")
    # Inbound multicast over the air: the access point sends a group's frames to every station,
    # and the ipvtap child filters them by group exactly as a macvtap child does.
    assert_guest_receives_multicast(host, "wlan1", "192.168.7.1", vip, ns="ip netns exec lan ")
    # ...and the lease renews: the host's INIT-REBOOT at T1, a minute into a two-minute lease. The
    # address does not move, so the guest is not restarted for it.
    host.wait_until_succeeds(
        f"test $(grep -c 'DHCPACK(wlan1) {vip} ' /tmp/lan-dnsmasq.log) -ge 2", timeout=240
    )
    host.succeed(f"ip netns exec lan curl -fsS --max-time 5 http://{vip}/healthz")
    host.fail("journalctl -u briard-agent -o cat | grep -q 'vip lease: restarting the guest'")
    print("the host renewed the flock's lease and the guest still serves")

    # DELTA 5: the host is still on its LAN and reaches its own guest over the private link.
    host.succeed(f"ip netns exec lan ping -c1 -W2 {host_ip}")
    host.succeed(f"curl -fsS --max-time 5 http://{vip}/healthz")
    host.succeed("ip -o -4 addr show dev wlan0 | grep -qw " + host_ip)
    print("the host kept its address and its LAN, and reaches its guest")

    # --- THE LEASE'S PROBE ON IPVTAP: its control, then the probe itself ---
    # THE CONTROL: with the guest's VIP silent, nothing on the LAN gets an ARP reply for it. The
    # probe rests on that -- an ARP-checking router must find the address free -- and so does a
    # relaunch's DISCOVER. The lever is the probe's own (QMP set_link on the service NIC). With the
    # link down only the guest is silenced, so a reply here would be the holding namespace's /32
    # copy answering -- which would answer for a stopped guest too.
    def qmp(cmd):
        host.succeed("printf '%s%s' '{\"execute\":\"qmp_capabilities\"}' '" + cmd + "' | "
                     "socat -t2 - UNIX-CONNECT:/run/briard/qmp/guest.sock")
    def arp():
        return int(host.succeed(f"ip netns exec lan arping -c 3 -w 4 -I wlan1 {vip} | grep -c 'reply from' || true").strip())
    assert arp() == 3, "the VIP does not answer ARP with its link up: the control would witness nothing"
    qmp('{"execute":"set_link","arguments":{"name":"net2","up":false}}')
    host.sleep(1)
    assert arp() == 0, "something answers ARP for the VIP while the guest is silent"
    qmp('{"execute":"set_link","arguments":{"name":"net2","up":true}}')
    host.wait_until_succeeds(f"ip netns exec lan arping -c 1 -w 2 -I wlan1 {vip} | grep -q 'reply from'", timeout=30)
    print("with the guest's link down nothing answers for the VIP; with it up the guest does")

    # THE PROBE: the router reboots having kept its leases in RAM, and is no longer authoritative,
    # so the host's INIT-REBOOT for the VIP meets silence until the lease expires. The router is up
    # (it answers ARP), so the host silences the VIP, DISCOVERs with it as the hint, and renews it
    # in place -- no restart of the guest, the same address.
    host.succeed("kill $(cat /run/lan-dnsmasq.pid); rm -f /tmp/lan.leases")
    host.succeed(
        "ip netns exec lan dnsmasq --interface=wlan1 --bind-interfaces --port=0 --no-resolv "
        "--dhcp-range=192.168.7.100,192.168.7.150,2m --dhcp-option=3,192.168.7.1 --dhcp-ignore-clid "
        "--dhcp-leasefile=/tmp/lan.leases --log-dhcp --log-facility=/tmp/lan-dnsmasq.log "
        "--pid-file=/run/lan-dnsmasq.pid --user=root"
    )
    host.wait_until_succeeds(
        f"journalctl -u briard-agent -o cat | grep -q 'vip lease: {vip}/24 renewed in place'", timeout=420
    )
    host.succeed(f"journalctl -u briard-agent -o cat | grep -q 'vip lease: {vip}/24 expired and the router does not answer for it'")
    host.fail("journalctl -u briard-agent -o cat | grep -q 'vip lease: restarting the guest'")
    host.wait_until_succeeds(f"ip netns exec lan curl -fsS --max-time 5 http://{vip}/healthz", timeout=60)
    print("the router forgot the lease; the host asked afresh with the VIP silent and renewed it in place")

    # --- AN AGENT RESTART UNDER ITS WI-FI GUEST KEEPS THE PARENT IT STANDS ON ---
    # A wire with a lower metric than the station's appears beside it (a veth here: a macvtap
    # builds on it, so a re-selection would take it). The children are in briard-hold, so the
    # hot path must find them there and read their parent off the @ifN, never re-select: no
    # macvtap on the wire, the copy and the lease still the agent's. Read as an INIT-REBOOT ACKed
    # after the restart, since a lease the agent stopped holding would never be asked again.
    host.succeed(
        "ip netns add wire && ip link add wire-a type veth peer name wire-b && ip link set wire-b netns wire",
        "ip -n wire addr add 10.77.0.1/24 dev wire-b && ip -n wire link set wire-b up",
        "ip addr add 10.77.0.2/24 dev wire-a && ip link set wire-a up",
        "ip route add default via 10.77.0.1 dev wire-a metric 1",
    )
    acks = int(host.succeed(f"grep -c 'DHCPACK(wlan1) {vip} ' /tmp/lan-dnsmasq.log").strip())
    restarted = host.succeed("date '+%Y-%m-%d %H:%M:%S'").strip()
    host.succeed("systemctl restart briard-agent.service")
    host.wait_until_succeeds(
        f"journalctl -u briard-agent --since '{restarted}' | grep -q 'already up on wlan0 (ipvtap'", timeout=120)
    host.fail(f"journalctl -u briard-agent --since '{restarted}' | grep -q 'hangs off wire-a'")
    host.fail("ip link show briard0")  # nothing built on the wire in the host's namespace
    host.wait_until_succeeds(
        f"test $(grep -c 'DHCPACK(wlan1) {vip} ' /tmp/lan-dnsmasq.log) -gt {acks}", timeout=240)
    host.succeed(f"ip netns exec lan curl -fsS --max-time 5 http://{vip}/healthz")
    host.succeed("ip route del default via 10.77.0.1 dev wire-a; ip link del wire-a; ip netns del wire")
    print("restarted under its Wi-Fi guest beside a better wire, the agent kept the station")
'');
}
