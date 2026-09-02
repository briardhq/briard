# The machine report card on a real host. Proves the CLI + Gather path
# end-to-end: `briard-agent --report-card` inspects the live host, emits every gate, and exits
# self-consistently (0 iff admitted). The refuse-with-fix *logic* is proven failable in the Go unit
# tests (agent/reportcard); this proves the impure reader works on a real Linux host.
{ pkgs, agent }:

pkgs.testers.runNixOSTest {
  name = "report-card";
  skipTypeCheck = true;

  nodes.machine = { ... }: {
    # 2 GB, DELIBERATELY UNDER THE GATE'S 4 GB FLOOR, so the memory check REFUSES -- and the
    # refusal is the branch worth spending a VM on. `Assess` is pure and its PASS/WARN/REFUSE
    # arithmetic is unit-tested; what only a real host can show is that the reader read THIS
    # machine, that a refusal carries its FIX rather than a bare verdict, and that the process
    # exits non-zero. Asserting PASS instead cost 10 GB the guest never touched (659 MB resident
    # of 10240 declared, [B.127]) to exercise the one branch the unit tests already own.
    # iproute2 present so the `ip` gate PASSes; KVM/tun depend on the runner, so the overall
    # verdict is asserted for self-consistency rather than for a value.
    virtualisation.memorySize = 2048;
    environment.systemPackages = [ agent pkgs.iproute2 ];
    # nss-mdns in nsswitch, so the mdns gate has a deterministic PASS to assert. It is the one
    # gate whose fact is read out of a CONFIG FILE rather than /proc or /sys ([V3b.19]), and the
    # unit tests can only exercise the pure check above it -- this is what puts the reader itself
    # in front of a real /etc/nsswitch.conf.
    services.avahi = { enable = true; nssmdns4 = true; };
  };

  testScript = ''
    machine.wait_for_unit("multi-user.target")

    status, out = machine.execute("briard-agent --report-card")
    print(f"report-card exit={status}:\n{out}")

    # Every gate in the closed set is emitted.
    for name in ["kvm", "tun", "iproute2", "systemd", "memory", "network", "mdns"]:
        assert name in out, f"report card is missing the {name} check"

    # Deterministic gates on this node PASS (concrete, non-vacuous): iproute2 installed, 8 GB RAM,
    # a wired virtio NIC.
    import re
    def gate(name):
        m = re.search(r"\[(\w+)\s*\]\s+" + name + r"\b", out)
        return m.group(1) if m else None
    assert gate("iproute2") == "PASS", f"iproute2 gate = {gate('iproute2')}, want PASS"
    # Deterministic here for the same reason iproute2 is: this VM is booted by systemd, so
    # /run/systemd/system exists. The gate refuses a host booted with OpenRC/runit BEFORE anything
    # is written -- install.sh had the guard already, but as its last branch, after ~400 MB landed.
    assert gate("systemd") == "PASS", f"systemd gate = {gate('systemd')}, want PASS (this VM is systemd-booted)"
    # THE MEMORY REFUSAL, END TO END: the reader read THIS machine (2 GB, under the 3584 MB floor),
    # and the refusal carries the FIX that makes it actionable rather than a bare verdict. Asserting
    # PASS here instead cost 10 GB the guest never touched, to exercise the one branch `Assess`'s
    # unit tests already own ([B.127]).
    #
    # The overall verdict was ALREADY refused before this, and not by the memory gate: the test
    # VM's root has under a gigabyte free, so the disk gate refuses and the exit-code assertion
    # below has always run its non-zero branch. This adds a SECOND, deliberate refusal whose input
    # the test controls, rather than one that depends on how full the builder happens to be.
    assert gate("memory") == "REFUSE", f"memory gate = {gate('memory')}, want REFUSE (2 GB is under the 4 GB floor)"
    assert "add RAM to at least 4 GB" in out, f"a REFUSE must carry its fix, not just a verdict:\n{out}"
    assert "result: REFUSED" in out, f"the summary line must report the refusal:\n{out}"
    assert gate("network") == "PASS", f"network gate = {gate('network')}, want PASS (wired virtio)"
    # The gatherer really read this machine's nsswitch: nss-mdns is configured above, so a WARN
    # here means hasMDNSResolver failed to see a resolver that is demonstrably present.
    assert gate("mdns") == "PASS", f"mdns gate = {gate('mdns')}, want PASS (nss-mdns is configured)"
    machine.succeed("grep -qE '^hosts:.*mdns' /etc/nsswitch.conf")

    # The exit code is self-consistent with the verdict: 0 iff no REFUSE (admitted).
    refused = "REFUSE" in out
    if refused:
        assert status != 0, "a REFUSE verdict must exit non-zero"
    else:
        assert status == 0, "an admitted (no-REFUSE) verdict must exit 0"
  '';
}
