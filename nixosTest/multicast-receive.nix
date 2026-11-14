# A testScript helper spliced into install-macvtap, install-bridge and install-wifi (one assertion
# for every substrate, so they cannot drift): does a group the guest joined reach it from the
# household L2?
#
# mDNS, asserted on its own, is not enough: it is one group, and the mDNS name is only the
# visible casualty when the macvtap drops every group the guest joined; SSDP and the discovery
# Chromecast, Sonos and HomeKit ride go silently with it. This pins the general property to a
# second, routable group (SSDP), which bridge snooping treats differently from the link-local
# 224.0.0.x block mDNS lives in.
#
# The guest kernel is the listener. The guest joins over the debug shell (an `autojoin` address,
# iproute2 alone: the image carries no listener of ours, and a rig that baked one in would test a
# different image); the observer sends a group-specific IGMP query, which reaches that kernel only
# if every inbound filter passed a frame addressed to the group MAC, and a member answers with a
# report. The join is what an integration does in the product; it grants no datapath the product
# lacks.
#
# ⚠️ Not 224.0.0.1: a general query to all-hosts needs no join and would pass on a node whose child
# lacks ALLMULTI, because the host kernel joins all-hosts on the macvtap/ipvtap child itself (it has
# IPv4 enabled), so that MAC is in the child filter whatever the guest does.
#
# ⚠️ When the observer listens is part of the assertion, as with mDNS announcements: a join sends
# unsolicited reports that would satisfy a capture taken too early. So the capture is shown silent
# before the query, and after the guest leaves a query must draw nothing: the answer came from the
# guest membership, not from anyone else on the L2.
#
# The address is `scope link` so the VIP reads of the guest, which take the first scope-global
# address on the device, never see it.
{ pkgs }:
''
  SSDP_GROUP = "239.255.255.250"

  def guest_shell(line, wait=5):
      """One command line in the guest's root shell; what the console relayed."""
      host.succeed(
          f"(printf '\\n'; sleep 2; printf '%s\\n' '{line}'; sleep {wait}; printf '\\035') "
          "| /usr/local/bin/briard debug shell > /tmp/guest-shell.out 2>&1"
      )
      return host.succeed("tr -d '\\r' < /tmp/guest-shell.out")

  def assert_guest_receives_multicast(observer, iface, source, vip, ns=""):
      """observer (a machine, or `host` with ns="ip netns exec lan ") sends from source on iface."""

      def reports(query):
          cmd = (
              f"{ns}timeout 8 ${pkgs.tcpdump}/bin/tcpdump -i {iface} -l -n -v "
              f"'igmp and not src host {source}' > /tmp/igmp.out 2>/dev/null & sleep 2; "
          )
          if query:
              cmd += f"{ns}${pkgs.python3}/bin/python3 ${./igmp-query.py} {source} {SSDP_GROUP}; "
          out = observer.succeed(cmd + "wait; cat /tmp/igmp.out")
          return [ln for ln in out.splitlines() if "report" in ln and SSDP_GROUP in ln]

      joined = guest_shell(
          f"d=$(ip -o -4 addr show to {vip}/32 | cut -d\" \" -f2); "
          f"ip addr add {SSDP_GROUP}/32 dev $d scope link autojoin && echo JOINED=$d"
      )
      # By substring, not by word: the console glues terminal escapes (bracketed paste) to the
      # front of the answer. It echoes the typed line too, whose `JOINED=$d` reads as no name.
      dev = ""
      for chunk in joined.split("JOINED=")[1:]:
          name = ""
          for c in chunk:
              if not (c.isalnum() or c in "._-"):
                  break
              name += c
          dev = name or dev
      assert dev, f"the guest did not join {SSDP_GROUP} on the device holding {vip}:\n{joined}"
      observer.sleep(5)  # the join's own unsolicited reports go out first

      quiet = reports(query=False)
      assert not quiet, (
          f"the guest reported {SSDP_GROUP} unasked after its join had settled: {quiet}. The "
          f"capture below can now be satisfied by a report nobody queried for"
      )
      heard = reports(query=True)
      assert heard, (
          f"the guest joined {SSDP_GROUP} on {dev} and did not answer a query for it: inbound "
          f"multicast to a joined group does not reach the guest. On macvtap/ipvtap the child "
          f"filters it unless it carries ALLMULTI"
      )
      print(f"the guest received a query for {SSDP_GROUP} on {dev} and answered: {heard[0].strip()}")

      guest_shell(f"ip addr del {SSDP_GROUP}/32 dev {dev}")
      observer.sleep(5)  # and the leave's own
      after = reports(query=True)
      assert not after, f"something answers for {SSDP_GROUP} with the guest gone from it: {after}"
''
