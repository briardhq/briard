# briard one-command install, Windows.
#
#   irm https://get.briard.io/install.ps1 | iex
#
# Brings a Windows Home desktop to GREEN: a guest VM on our BUNDLED qemu holding the VIP, its
# network hung off the machine's own NIC through a bridge, a data volume the guest lays out, and
# Briard answering at the VIP on the LAN. No cloud, no name. One restart.
#
# THIS SCRIPT IS OUTSIDE THE VERSIONING SYSTEM, like install.sh, whose twin it is: fetched from the
# channel root and run once, so nothing it does can be reached by a release afterwards. It does
# only what has to happen before an agent can run on this machine -- be an administrator, get an
# agent onto the box, put the two things in place the agent cannot (a kernel driver someone else
# signs, a Windows feature), call the agent, and ask for the restart.
#
# EVERYTHING ELSE IS THE AGENT'S -- `briard-agent install` (agent/host/install.go,
# install_windows.go): the machine report card, the signed fetch, the %ProgramData%\briard layout,
# config.env, the tree's ACL, the service and its recovery, the firewall hole, `briard` on PATH,
# the network bridge, and the notification-area icon, which at the installing account's next logon
# opens the Briard page once the node serves. Adding something back here means showing it has to
# happen before an agent can run.
#
# BRIARD_ARTIFACTS=<dir> installs from a local, already-verified staging dir (the rig, and the
# Store's exe, which carries the windows-amd64 arm in itself). Unset = the signed network fetch over
# the channel.
#
# -NoRestart is the exe's ending: no prompt and no restart, and the script's exit code says whether
# the restart is owed -- 3010, Windows' "reboot required", which the Store turns into its own
# restart message -- or not (0, and briard is started at once).
#
# Windows PowerShell 5.1 -- what every Windows Home ships -- so nothing newer is used here.
param([switch]$NoRestart)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest's progress bar slows a download tenfold
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

# ---- what this script needs to know -------------------------------------------------
# THE KNOBS ARE NOT HERE, and adding one back is the mistake. Every `BRIARD_*` in the environment
# reaches the agent, which copies it into config.env with the prefix stripped, so this script
# carries no list of them, no defaults for them and no documentation of them (install.sh says the
# same). Below is the short list this script READS, because it acts on it before any agent runs.
#
# Settings reach the agent from an ADMINISTRATOR prompt. An elevation relaunch (below) starts from
# a fresh environment, so a one-liner run from an ordinary prompt installs with the defaults.
$Prefix = 'C:\ProgramData\briard'
# A CONSTANT: the agent's own roots derive from it, and the service is registered on a path under it.

# The signed release channel root, and WHICH release off it -- read here only to fetch the
# bootstrap agent from the target's own path. The agent reads the same two to fetch the rest.
$Channel = if ($env:BRIARD_CHANNEL_URL) { $env:BRIARD_CHANNEL_URL } else { 'https://get.briard.io' }
$Release = if ($env:BRIARD_RELEASE) { $env:BRIARD_RELEASE } else { 'stable' }
# The release public key: the verify root, before anything is on disk to trust.
$Keyring = if ($env:BRIARD_UPDATE_KEYRING) { $env:BRIARD_UPDATE_KEYRING } else { "$Prefix\keyring.pem" }

# The release signing public key(s), embedded at release time: this script is fetched over TLS from
# the channel, so the key travels with it (the installer-carries-the-pubkey pattern). A placeholder
# in the source tree that the release pipeline fills. Used only when no keyring is on disk.
$RELEASE_KEYRING_PEM = '__BRIARD_RELEASE_KEYRING_PEM__'

# THE TAP DRIVER: tap-windows6, OpenVPN's signed build. A kernel driver is signed by whoever
# Microsoft attests, and that is OpenVPN Inc., not us, so these are their bytes, pinned by hash
# (the same pin the release pipeline fetches them by) and served from the release's windows-amd64
# arm beside the agent, so an install touches one origin -- the install refuses anything else.
# 9.24.7 is the newest standalone installer; later builds ship only as merge modules for someone
# else's MSI. Its own installer creates the one adapter the agent's bridge wants.
$TapSha256 = '1c44e77ab148ddfb174de8041100efc38c4f23500183faff88f9badac5782e3c'
$TapDescription = 'TAP-Windows Adapter V9' # how the driver describes its adapters, whatever they are named

# The console task an earlier installer registered to run the closing at the next logon: removed
# here, so a reinstall over a node still waiting for that logon opens no console.
$ResumeTask = 'briard-install'

function Say($m) { Write-Host "briard: $m" }
function Die($m) { Unlock; throw "briard: ERROR: $m" }
# The install lock (step 0), given back the moment this run stops: the one-liner's window stays
# open after it, and a lock held by an open window would refuse the retry it asks for.
function Unlock { if ($script:InstallLock) { try { $script:InstallLock.ReleaseMutex() } catch {}; $script:InstallLock = $null } }
# Run a program of ours and return its exit code, its output shown as it comes. Its stderr is
# merged and turned back into text: with this script's stderr redirected (a log, an ssh session)
# PowerShell 5 wraps a program's stderr lines as error records, and under 'Stop' the first one
# would end the script before the exit code was read.
function Run($exe, [string[]]$a) {
	$ErrorActionPreference = 'Continue'
	& $exe @a 2>&1 | ForEach-Object { "$_" } | Write-Host
	return $LASTEXITCODE
}
function IsAdmin {
	([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
		[Security.Principal.WindowsBuiltInRole]::Administrator)
}
function TapInstalled { @(Get-NetAdapter | Where-Object InterfaceDescription -like "$TapDescription*").Count -gt 0 }

# ---- 0. an administrator ------------------------------------------------------------
# Not elevated: relaunch elevated, in a window that stays, and let that run be the install. The
# `irm | iex` form has no file to re-run, so it re-fetches this script from the channel root; a
# saved copy re-runs the copy.
if (-not (IsAdmin)) {
	$cmd = if ($PSCommandPath) { "& '$PSCommandPath'" + $(if ($NoRestart) { ' -NoRestart' }) } else { "irm $Channel/install.ps1 | iex" }
	Say 'asking for administrator rights (the install registers a service and builds a network bridge)'
	Start-Process powershell.exe -Verb RunAs -ArgumentList '-NoExit', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-Command', $cmd
	return
}
# ONE INSTALL AT A TIME, machine-wide. Two at once both find no TAP adapter and both run its
# installer, which adds an adapter every time it runs -- two, seconds apart, and every install
# after refuses the ambiguity. A second install stops here, before touching anything; from the
# exe with Windows installers' code for "another installation is in progress", which the Store
# maps to its own message. A lock left by a run that died is taken over.
$script:InstallLock = New-Object System.Threading.Mutex($false, 'Global\briard-install')
$held = $false
try { $held = $script:InstallLock.WaitOne(0) }
catch {
	# PowerShell 5 wraps a .NET method's exception; the abandoned lock is the inner one.
	if ($_.Exception.InnerException -is [System.Threading.AbandonedMutexException]) { $held = $true } else { throw }
}
if (-not $held) {
	$script:InstallLock = $null
	$m = 'another Briard install is already running on this computer: let it finish, and run this again only if it fails'
	if ($NoRestart) { Say $m; exit 1618 }
	Die $m
}

# ---- 1. the agent that installs ----------------------------------------------------
# The ONLY artifact this script fetches: everything heavy (the qemu bundle, the guest image) is the
# agent's to fetch, AFTER its report card has admitted the machine. Asking before taking is what
# lets a refusal say "nothing was installed" truthfully.
$Boot = ''
# The release keyring, on both paths: the network fetch verifies against it here, and on either path
# the installed agent's own fetches do -- its self-updates, and the guest image its first start
# fetches when the install laid none.
New-Item -ItemType Directory -Force $Prefix | Out-Null
if (-not (Test-Path $Keyring)) {
	if ($RELEASE_KEYRING_PEM -notlike '*BEGIN PUBLIC KEY*') {
		Die "no release keyring at $Keyring (the embedded key is a build placeholder; set BRIARD_UPDATE_KEYRING)"
	}
	Set-Content -Path $Keyring -Value $RELEASE_KEYRING_PEM -Encoding ascii
}
if ($env:BRIARD_ARTIFACTS) {
	# Offline / rig path: the staging dir's own agent installs from it.
	$Agent = "$env:BRIARD_ARTIFACTS\briard-agent.exe"
	if (-not (Test-Path $Agent)) { Die "staging dir $env:BRIARD_ARTIFACTS has no briard-agent.exe" }
} else {
	# Signed network fetch. Bootstrap a briard-agent over TLS -- the channel's integrity anchors
	# this FIRST binary -- and let it fetch and verify the whole set against the bundled release
	# keyring. The bootstrap only RUNS the verified fetch; what lands under the prefix is the
	# Ed25519-verified set, so a compromised bootstrap cannot seed bad cattle.
	Say "bootstrapping the installer agent from $Channel (briard/$Release) ..."
	# Fetched from the TARGET's path, never a fixed one: the bootstrap is the binary that parses
	# the manifest and lays the release down, and a stale one that cannot is exactly the
	# forward-compat bricking the channel layout exists to prevent. `briard-agent.exe` is the
	# one artifact the channel duplicates under its pointers for this fetch.
	$Boot = "$Prefix\bootstrap-agent.exe"
	try { Invoke-WebRequest -UseBasicParsing -Uri "$Channel/briard/$Release/windows-amd64/briard-agent.exe" -OutFile $Boot }
	catch { Die "could not fetch the bootstrap agent from $Channel/briard/$Release/windows-amd64 ($_)" }
	# Fail with the REASON. A bootstrap that will not run (wrong arch, a blocked download) is not a
	# verification failure, and reporting it as one sends the reader hunting for a bad signature.
	$rc = 1
	try { $rc = Run $Boot help 6> $null } catch {}
	if ($rc -ne 0) { Die "the bootstrap agent at $Boot will not run on this machine; nothing installed" }
	$Agent = $Boot
}

# ---- 2. what the agent cannot put in place for itself -----------------------------------
# Both are in place BEFORE the agent's report card runs, which refuses a machine without them and
# names this script as what installs them.
if (TapInstalled) {
	Say 'the TAP driver is installed'
} else {
	# From the staging dir on the offline path, else from the release the bootstrap came from.
	if ($env:BRIARD_ARTIFACTS) {
		$exe = "$env:BRIARD_ARTIFACTS\tap-windows.exe"
		if (-not (Test-Path $exe)) { Die "staging dir $env:BRIARD_ARTIFACTS has no tap-windows.exe" }
	} else {
		# Resolved against the manifest's own version, as every artifact is: a pointer (`stable`)
		# serves a manifest and the bootstrap agent, nothing else. The manifest is read unverified
		# because it only names the path -- the bytes are held to the pinned hash below.
		$m = "$Channel/briard/$Release/windows-amd64/manifest.json"
		try { $version = (Invoke-RestMethod -UseBasicParsing -Uri $m).version }
		catch { Die "could not read the release manifest at $m ($_)" }
		if (-not $version) { Die "the release manifest at $m names no version" }
		$exe = "$Prefix\tap-windows.exe"
		$url = "$Channel/briard/$version/windows-amd64/tap-windows.exe"
		Say "fetching the TAP network driver (tap-windows6, OpenVPN's signed build) ..."
		try { Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $exe }
		catch { Die "could not fetch the TAP driver from $url ($_)" }
	}
	$got = (Get-FileHash -Algorithm SHA256 $exe).Hash.ToLower()
	if ($got -ne $TapSha256) {
		if (-not $env:BRIARD_ARTIFACTS) { Remove-Item $exe }
		Die "the TAP driver at $exe does not match the pinned hash (got $got); nothing installed"
	}
	Say 'installing the TAP network driver ...'
	# /S is the NSIS silent install; it creates the one TAP adapter. Windows trusts the driver's
	# own signature at load, which is what Secure Boot evaluates -- nothing of ours is involved.
	$p = Start-Process $exe -ArgumentList '/S' -PassThru -Wait
	if (-not $env:BRIARD_ARTIFACTS) { Remove-Item $exe }
	if ($p.ExitCode -ne 0) { Die "the TAP driver installer exited $($p.ExitCode); nothing installed" }
	if (-not (TapInstalled)) { Die 'the TAP driver installed but no TAP adapter appeared; nothing installed' }
	# Said where it happened, and only there: the uninstall removes the driver package on this
	# record alone, so one already on the machine -- another program's -- is never taken. Under
	# state\, which an uninstall keeps and a reinstall finds.
	New-Item -ItemType Directory -Force "$Prefix\state" | Out-Null
	Set-Content -Path "$Prefix\state\tap-installed" -Value (Get-Date -Format o) -Encoding ascii
}
# The Windows Hypervisor Platform: what qemu accelerates the guest with (WHPX). Turned on here,
# running only after the restart -- the same restart the bridge needs, so it costs nothing extra.
# The report card reads the hypervisor itself, so it knows "on, pending" from "running".
if ((Get-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform).State -eq 'Enabled') {
	Say 'the Windows Hypervisor Platform is on'
} else {
	Say 'turning on the Windows Hypervisor Platform (starts at the restart) ...'
	Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All -NoRestart | Out-Null
}

# ---- 3. the install itself: the agent's ---------------------------------------------
# The report card first (a refusal with the fix named, before a byte of the release is fetched),
# then the verified set laid down, config.env, the service, the bridge. It says what it is doing
# as it goes, and why when it stops.
#
# The bootstrap goes either way: on a refusal it is the one thing we put down, and "nothing was
# installed" must be true while it sits there; on success the verified agent has replaced it.
#
# 3010 is the agent saying the install is done and the bridge it built still owes the restart.
$rc = Run $Agent install
if ($Boot) { Remove-Item $Boot -ErrorAction SilentlyContinue }
if ($rc -ne 0 -and $rc -ne 3010) { Die 'the install did not finish (the agent said why above)' }

# ---- 4. the hand-over, after the restart ----------------------------------------------
# The service starts by itself at boot, with no window. The hand-over is the agent's, laid down
# above: at the installing account's next logon its notification-area icon starts once with
# --open, says Briard is starting, and opens the Briard page in that person's browser once the
# node serves. Nothing is left to arrange here but removing an earlier installer's console task.
$exe = "$Prefix\agent\briard-agent.exe"
Unregister-ScheduledTask -TaskName $ResumeTask -Confirm:$false -ErrorAction SilentlyContinue

Unlock # installed: what follows is the ending, and a retry no longer races anything

# ---- 5. the one restart ----------------------------------------------------------------
# The exe's ending: say it, and leave the restart to whoever ran us -- the Store shows its own
# restart message for 3010. Owing none (a reinstall over a node that has restarted since its
# bridge was built), the node starts now, as the restart would have started it.
if ($NoRestart) {
	if ($rc -eq 3010) {
		Say 'installed. Restart Windows to finish (Restart, not Shut down): Briard starts by itself afterwards.'
		Say 'when you log on again, your Briard page opens in your browser once it is ready.'
		exit 3010
	}
	$rc = Run $exe start
	if ($rc -ne 0) { Die 'installed, but briard did not start (the agent said why above)' }
	Say 'installed, and briard is starting.'
	exit 0
}
# The bridge carries the guest only once Windows has started with it, and the hypervisor runs
# only after a restart: one restart, asked for, never a shutdown (Fast Startup's resume is not a
# start). Briard finishes on its own afterwards; the link appears when an administrator logs on.
Say 'installed. Windows must restart once for the network bridge and the hypervisor.'
Say 'after the restart, log on: Briard finishes starting and opens your Briard page in your browser.'
if ([Environment]::UserInteractive -and $Host.Name -eq 'ConsoleHost') {
	Read-Host 'press Enter to restart now (Ctrl-C to restart later yourself)' | Out-Null
}
shutdown /r /t 5 /f /c 'Briard: restarting once to finish the install'
