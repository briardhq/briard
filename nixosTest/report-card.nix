# The machine report card on a real host. Proves the CLI + Gather path
# end-to-end: `briard-agent --report-card` inspects the live host, emits every gate, and exits
# self-consistently (0 iff admitted). The refuse-with-fix *logic* is proven failable in the Go unit
# tests (agent/reportcard); this proves the impure reader works on a real Linux host.
{ pkgs, agent }:

pkgs.testers.runNixOSTest {
  name = "report-card";
  skipTypeCheck = true;

  nodes.machine = { ... }: {
    # 10 GB so MemTotal clears the 8 GB recommended threshold (the kernel reserves a slice, so an
    # 8192 MB VM reports just under 8 GB and would WARN); iproute2 present so the `ip` gate PASSes.
    # KVM/tun depend on the runner, so the test asserts self-consistency for the overall verdict.
    virtualisation.memorySize = 10240;
    environment.systemPackages = [ agent pkgs.iproute2 ];
  };

  testScript = ''
    machine.wait_for_unit("multi-user.target")

    status, out = machine.execute("briard-agent --report-card")
    print(f"report-card exit={status}:\n{out}")

    # Every gate in the closed set is emitted.
    for name in ["kvm", "tun", "iproute2", "memory", "network"]:
        assert name in out, f"report card is missing the {name} check"

    # Deterministic gates on this node PASS (concrete, non-vacuous): iproute2 installed, 8 GB RAM,
    # a wired virtio NIC.
    import re
    def gate(name):
        m = re.search(r"\[(\w+)\s*\]\s+" + name + r"\b", out)
        return m.group(1) if m else None
    assert gate("iproute2") == "PASS", f"iproute2 gate = {gate('iproute2')}, want PASS"
    assert gate("memory") == "PASS", f"memory gate = {gate('memory')}, want PASS (8 GB)"
    assert gate("network") == "PASS", f"network gate = {gate('network')}, want PASS (wired virtio)"

    # The exit code is self-consistent with the verdict: 0 iff no REFUSE (admitted).
    refused = "REFUSE" in out
    if refused:
        assert status != 0, "a REFUSE verdict must exit non-zero"
    else:
        assert status == 0, "an admitted (no-REFUSE) verdict must exit 0"
  '';
}
