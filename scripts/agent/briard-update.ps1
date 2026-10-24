# briard-update.ps1: converge this node to the release channel -- the Windows twin of
# briard-update, run by the briard-update scheduled task as SYSTEM, one run at a time.
#
# SHIPPED VERBATIM and FROZEN: a signed artifact of the briard chain's windows arm that the install
# copies into place. The freeze is the safety property -- this is dumb, agent-INDEPENDENT script, so
# a bug in the volatile agent can never wedge the thing that replaces it -- and it is frozen by
# necessity: it launches the replacement of the binary, so no release can be what fixes it.
#
# IT HAS NO PRODUCT KNOWLEDGE, and that is the whole design: four steps -- read the channel and
# keyring, read the target, download the pointer's agent, run it with --update -- and the verdict
# written down. The fetched binary does the whole update (stop, swap, start, commit or revert), so
# nothing in the product can make this script need a new release.
#
# Windows PowerShell 5.1 -- what every Windows Home ships -- so nothing newer is used here.
$ErrorActionPreference = 'Stop'

# The node's own configuration, when it has one: CHANNEL_URL and UPDATE_KEYRING exactly as the
# operator set them at install, read the way the agent reads config.env (KEY=value, both halves
# trimmed, nothing quoted).
#
# THE TWO DEFAULTS BELOW ARE A DELIBERATE SECOND COPY of agent/host/config.go's (owner's call).
# config.env carries a key only when somebody named one, so on an ordinary install there is
# nothing to read -- and this script must not ask the agent, which is the thing it exists to
# replace. Pinned by a test that reads both files.
$Prefix = 'C:\ProgramData\briard'
$conf = @{}
if (Test-Path "$Prefix\config.env") {
	foreach ($l in [IO.File]::ReadAllLines("$Prefix\config.env")) {
		if ($l -match '^\s*(CHANNEL_URL|UPDATE_KEYRING)\s*=\s*(.*?)\s*$') { $conf[$Matches[1]] = $Matches[2] }
	}
}
$Channel = if ($conf.CHANNEL_URL) { $conf.CHANNEL_URL } else { 'https://get.briard.io' }
$Keyring = if ($conf.UPDATE_KEYRING) { $conf.UPDATE_KEYRING } else { "$Prefix\keyring.pem" }
$Base = "$Prefix\agent"
$Run = "$Prefix\run"
$Log = "$Prefix\log\briard-update.log"
# The updater's own log, written as it runs and appended to $Log after it. A run killed before
# that leaves it behind, and the next run appends it first: the lines of a run cut short are the
# ones worth reading.
$Err = "$Prefix\log\briard-update.err"
New-Item -ItemType Directory -Force $Run, "$Prefix\log" | Out-Null
if (Test-Path $Err) { [IO.File]::AppendAllText($Log, [IO.File]::ReadAllText($Err)); Remove-Item -Force $Err }
# The verdict: `ok <line>` or `failed <line>`, left for `briard update` and the agent's update
# alerter, and the line itself in the log. UTF-8 without a byte-order mark, as the agent reads it.
function Report($word, $line) {
	[IO.File]::WriteAllText("$Run\update-result", "$word $line`n")
	[IO.File]::AppendAllText($Log, "$(Get-Date -Format s) briard-update: $word $line`r`n")
}
# (1) The target, as a message: stable | latest | an exact id. Absent means stable.
$target = 'stable'
if (Test-Path "$Run\update-target") {
	$target = ([IO.File]::ReadAllLines("$Run\update-target"))[0].Trim()
	Remove-Item -Force "$Run\update-target"
}
# (2) A fresh agent FROM THE TARGET runs the update: never the committed file itself, which the update
#     replaces. Under the prefix, which only SYSTEM and the administrators can write.
$tmp = "$Prefix\.update." + [IO.Path]::GetRandomFileName()
New-Item -ItemType Directory $tmp | Out-Null
$rc = 1
try {
	#     Most runs find the target's agent byte-identical to the committed one, so curl asks for it
	#     only if it differs: the channel's ETag for a pointer's file is its MD5, and a 304 means the
	#     committed binary IS the target's, so its copy runs -- hashed after copying, so what runs
	#     is what was compared. Any other ETag (an exact id's multipart upload) just downloads. The
	#     header goes through a file: 5.1 passes a native argument's inner quotes unescaped.
	$url = "$Channel/briard/$target/windows-amd64/briard-agent.exe"
	$hdr = @()
	if (Test-Path "$Base\briard-agent.exe") {
		Copy-Item "$Base\briard-agent.exe" "$tmp\have.exe"
		$md5 = (Get-FileHash -Algorithm MD5 "$tmp\have.exe").Hash.ToLower()
		[IO.File]::WriteAllText("$tmp\hdr", "If-None-Match: `"$md5`"`n")
		$hdr = @('-H', "@$tmp\hdr")
	}
	$code = & curl.exe -fsSL @hdr -w '%{http_code}' $url -o "$tmp\briard-agent.exe"
	if ($LASTEXITCODE -ne 0) { throw "could not fetch a bootstrap agent from $url" }
	if ($code -eq '304') { Move-Item -Force "$tmp\have.exe" "$tmp\briard-agent.exe" }
	# (3) Its log goes to the update log (stderr); its last stdout line is the verdict.
	$env:BRIARD_CHANNEL_URL = $Channel
	$env:BRIARD_KEYRING = $Keyring
	$env:UPDATE_BASE = $Base
	$env:UPDATE_RUN_DIR = $Run
	$p = Start-Process -FilePath "$tmp\briard-agent.exe" -ArgumentList '--update', $target -NoNewWindow -Wait -PassThru `
		-RedirectStandardOutput "$tmp\out" -RedirectStandardError $Err
	[IO.File]::AppendAllText($Log, [IO.File]::ReadAllText($Err))
	Remove-Item -Force $Err
	$lines = @([IO.File]::ReadAllLines("$tmp\out") | Where-Object { $_ })
	$line = if ($lines.Count) { $lines[-1] } else { '' }
	if ($p.ExitCode -eq 0) { Report ok $line; $rc = 0 }
	elseif ($line) { Report failed $line }
	else { Report failed "the update exited $($p.ExitCode) with no verdict" }
} catch {
	Report failed "$_"
} finally {
	Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
exit $rc
