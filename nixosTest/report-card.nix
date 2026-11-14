# The machine report card on a real host. Proves the CLI + Gather path end to end:
# `briard-agent --report-card` inspects the live host, emits every gate, and exits
# self-consistently (0 iff admitted). The refuse-with-fix logic is proven failable in the
# agent/reportcard unit tests; this proves the impure reader on real Linux.
{ pkgs, agent }:

pkgs.testers.runNixOSTest {
  name = "report-card";
  skipTypeCheck = true;

  nodes.machine = { ... }: {
    # 2 GB, deliberately under the gate's 4 GB floor, so the memory check REFUSEs: the refusal is
    # the branch worth a VM. `Assess` is pure and its arithmetic unit-tested; only a real host
    # shows the reader read this machine, the refusal carries its fix, and the process exits
    # non-zero. Asserting PASS would cost 10 GB the guest never touches, for a branch the unit
    # tests already own. iproute2 makes the `ip` gate PASS; KVM/tun depend on the runner, so the
    # verdict is asserted for self-consistency only.
    virtualisation.memorySize = 2048;
    environment.systemPackages = [ agent pkgs.iproute2 ];
    # nss-mdns, so the mdns gate has a deterministic PASS. It is the one gate read from a config
    # file rather than /proc or /sys, and the unit tests reach only the pure check: this puts the
    # reader itself in front of a real /etc/nsswitch.conf.
    services.avahi = { enable = true; nssmdns4 = true; };
  };

  testScript = ''
    machine.wait_for_unit("multi-user.target")

    status, out = machine.execute("briard-agent --report-card")
    print(f"report-card exit={status}:\n{out}")

    # Each named gate is emitted (disk and the address checks are not in this list).
    for name in ["kvm", "tun", "iproute2", "systemd", "memory", "network", "mdns", "encryption"]:
        assert name in out, f"report card is missing the {name} check"

    # Deterministic gates on this node PASS (concrete, non-vacuous): iproute2 installed, systemd
    # booted, a wired virtio NIC, nss-mdns configured.
    import re
    def gate(name):
        m = re.search(r"\[(\w+)\s*\]\s+" + name + r"\b", out)
        return m.group(1) if m else None
    assert gate("iproute2") == "PASS", f"iproute2 gate = {gate('iproute2')}, want PASS"
    # This VM is systemd-booted, so /run/systemd/system exists. The gate refuses an OpenRC/runit
    # host before anything is written, not after ~400 MB has been downloaded and unpacked.
    assert gate("systemd") == "PASS", f"systemd gate = {gate('systemd')}, want PASS (this VM is systemd-booted)"
    # The memory refusal end to end: the reader read this machine (2 GB, under the 3584 MB floor)
    # and the refusal carries its fix. The disk gate refuses too (the VM root has under a gigabyte
    # free), so the exit-code check below runs its non-zero branch either way; the memory refusal
    # is the deliberate one, its input set by the test rather than by how full the builder is.
    assert gate("memory") == "REFUSE", f"memory gate = {gate('memory')}, want REFUSE (2 GB is under the 4 GB floor)"
    assert "add RAM to at least 4 GB" in out, f"a REFUSE must carry its fix, not just a verdict:\n{out}"
    assert "result: REFUSED" in out, f"the summary line must report the refusal:\n{out}"
    assert gate("network") == "PASS", f"network gate = {gate('network')}, want PASS (wired virtio)"
    # The gatherer really read this machine's nsswitch: nss-mdns is configured above, so a WARN
    # here means hasMDNSResolver failed to see a resolver that is demonstrably present.
    assert gate("mdns") == "PASS", f"mdns gate = {gate('mdns')}, want PASS (nss-mdns is configured)"
    machine.succeed("grep -qE '^hosts:.*mdns' /etc/nsswitch.conf")
    # The gate must agree with /proc/cpuinfo, so a gatherer that stopped reading the AES flag is
    # caught either way round (KVM on an AES builder, TCG on one that hides it). Never a REFUSE:
    # an AES-less box is a perfectly good node.
    aes = machine.execute("grep -qwE 'aes' /proc/cpuinfo")[0] == 0
    assert gate("encryption") == ("PASS" if aes else "WARN"), \
        f"encryption gate = {gate('encryption')} but /proc/cpuinfo aes={aes}"

    # The exit code is self-consistent with the verdict: 0 iff no REFUSE (admitted).
    refused = "REFUSE" in out
    if refused:
        assert status != 0, "a REFUSE verdict must exit non-zero"
    else:
        assert status == 0, "an admitted (no-REFUSE) verdict must exit 0"
  '';
}
