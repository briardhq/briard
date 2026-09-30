# HOW DOES HOME ASSISTANT BEHAVE IN A 1 GB GUEST WITH ZRAM? -- a measurement, run by hand.
#
# The guest's memory is fixed at boot and the default (2 GB) was picked when Home Assistant was
# the only service. Whether a smaller guest is viable, and what its memory looks like when it is
# not, is a question for evidence: this boots the shipped guest module at 1 GB, installs the real
# Home Assistant image onto the volume the way the product does, lets it serve, and prints a
# timeline of the numbers the resources verb reports (the same kernel files, read the same way):
#
#   avail/total    MemAvailable / MemTotal -- counts reclaimable cache, knows nothing of swap
#   anon           AnonPages: what only swap can reclaim
#   zram a->b      what zram holds (uncompressed) -> the RAM that costs
#   swpin/swpout   cumulative pages; a CLIMBING swpin is parked memory coming back
#   psi_some/full  avg60 of the memory stall -- the cost users feel
#   top cgroups    the two services holding the most anonymous memory
#
# It asserts only what makes the numbers mean anything: the guest really has ~1 GB, and zram is
# really active as swap (the image's module list is forced, so a zram module that was never
# loaded would silently turn this into a no-swap run). Everything else is printed, not judged.
{ pkgs, guestModule, fixture }:

let
  h = import ./lib.nix { inherit pkgs guestModule; };

  node = h.mkNode {
    inherit fixture;
    resource = h.mkResource [ { name = "node1"; id = 0; } ];
  };

  sample = pkgs.writeShellScript "mem-sample" ''
    awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} /^AnonPages:/{n=$2}
         END{printf "avail=%dM/%dM anon=%dM ", a/1024, t/1024, n/1024}' /proc/meminfo
    if [ -r /sys/block/zram0/mm_stat ]; then
      awk '{printf "zram=%dM->%dM ", $1/1048576, $3/1048576}' /sys/block/zram0/mm_stat
    else
      printf 'zram=none '
    fi
    awk '/^pswpin /{i=$2} /^pswpout /{o=$2} END{printf "swpin=%d swpout=%d ", i, o}' /proc/vmstat
    awk '{split($3, v, "="); printf "psi_%s=%s ", $1, v[2]}' /proc/pressure/memory
    printf 'top:'
    for f in /sys/fs/cgroup/system.slice/*.service/memory.stat; do
      printf '%s=%sM\n' "$(basename "$(dirname "$f")")" "$(awk '/^anon /{print int($2/1048576)}' "$f")"
    done | sort -t= -k2 -rn | head -2 | tr '\n' ' '
    echo
  '';
in
pkgs.testers.runNixOSTest {
  name = "hass-lowmem";

  nodes.node1 =
    { ... }:
    {
      imports = [ node ];
      virtualisation.memorySize = 1024;
      virtualisation.diskSize = 20480;
    };

  skipTypeCheck = true;

  testScript = ''
    ${h.fixtureHelpers}
    import time

    t0 = time.monotonic()
    def s(label):
        line = node1.succeed("${sample}").strip()
        print(f"MEM {time.monotonic() - t0:7.0f}s {label:<14} {line}")

    node1.start()
    node1.wait_for_unit("multi-user.target")
    total_mb = int(node1.succeed("awk '/^MemTotal:/{print int($2/1024)}' /proc/meminfo"))
    assert 800 < total_mb < 1100, f"MemTotal is {total_mb} MB: this is not a 1 GB guest"
    node1.wait_until_succeeds("swapon --show=NAME --noheadings | grep -q zram0", timeout=60)
    print("swap: " + node1.succeed("swapon --show --bytes").strip().replace("\n", " | "))
    print("swappiness: " + node1.succeed("sysctl -n vm.swappiness").strip())
    s("booted")

    node1.wait_for_unit("briard-test-fixture-install.service", timeout=1800) # the 2.4 GB image
    s("image-loaded")
    node1.succeed("modprobe drbd")
    node1.succeed("briard-test-storage --seed")
    node1.succeed("systemctl start drbd-reactor.service")
    node1.wait_until_succeeds("drbdadm role r0 | grep -q Primary", timeout=60)
    node1.wait_until_succeeds("systemctl is-active briard-primary-storage.service", timeout=120)
    s("volume-up")

    install_fixture(node1)
    t_install = time.monotonic()
    node1.wait_until_succeeds("curl -fsS -o /dev/null http://192.168.1.100:8123/manifest.json", timeout=900)
    print(f"HA serving {time.monotonic() - t_install:.0f}s after install")
    s("ha-serving")

    # Settle: Home Assistant's first minutes are its busiest (onboarding store, recorder, the
    # integrations' first setup), then it idles -- the two shapes a household sees.
    for i in range(20):
        time.sleep(30)
        s(f"idle+{(i + 1) * 30}s")

    ooms = node1.succeed("journalctl -k --no-pager | grep -c 'Out of memory' || true").strip()
    serving = node1.succeed("curl -fsS -o /dev/null -w '%{http_code}' http://192.168.1.100:8123/manifest.json || true").strip()
    print(f"RESULT oom_kills={ooms} ha_manifest_http={serving}")
  '';
}
