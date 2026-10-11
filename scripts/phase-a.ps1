# Phase A orchestrator: provisions the Hetzner host via Pulumi, brings
# up the single-host Docker Compose stack (Postgres + Nakama + Caddy +
# signaling-proxy + geoip-sidecar + the obs subset; see
# infra/remote/nakama/docker-compose.yml), and verifies the install
# end-to-end.
#
# Each step is idempotent. Rerunning the script is safe: Pulumi up is
# idempotent, S3 bucket creation is gated on existence, generated
# secrets are read from credentials.env if already present.
#
# Usage:
#   pwsh -File scripts/phase-a.ps1                # Full run
#   pwsh -File scripts/phase-a.ps1 -SkipPreview   # Skip preview gate
#   pwsh -File scripts/phase-a.ps1 -StartAt Nakama     # Resume mid-run
#   pwsh -File scripts/phase-a.ps1 -RestoreBackup # DR rebuild, one
#                                  # command: new host, then the newest
#                                  # R2 dump restored before Nakama
#                                  # boots. Interactive: HostKeys asks
#                                  # you to confirm the new host's
#                                  # fingerprint out-of-band.
#   pwsh -File scripts/phase-a.ps1 -StartAt HostKeys -StopAt HostKeys
#                                  # Re-verify + re-pin the host's SSH
#                                  # keys on their own.
#
# The -StartAt step names are: PulumiSetup, S3, PulumiLogin, Preview,
# Up, WaitBoot, HostKeys, Postgres, Nakama, Verify, Reencrypt, Complete.
#
# Single-host stack: since the 2026-05-06 consolidation Postgres is a
# compose service next to Nakama, so WaitBoot and Postgres skip the old
# separate postgres-prod-1 host whenever the Pulumi stack no longer
# exports one. They used to read hetzner_postgres_ip unconditionally,
# which hard-failed a single-host DR rebuild (DR drill 2026-10-07,
# Gap A). The two-host paths stay for forward compatibility only.
#
# Host keys: a rebuilt host has new SSH host keys, and every scheduled
# prod job verifies the host against claude-config/jobs/known_hosts
# with StrictHostKeyChecking=yes. HostKeys (right after WaitBoot, so
# before any secret is copied to the host) has you confirm the new
# ed25519 fingerprint in the Hetzner web console, then rewrites that
# file and ~/.hopnbop-migration/known_hosts to match (DR drill
# 2026-10-07, Gap B).

[CmdletBinding()]
param(
	[switch]$SkipPreview,
	# DR rebuild: restore the newest R2 pg-backup dump into Postgres
	# before Nakama first boots (see Invoke-PgRestore). When R2
	# already holds backups and neither switch is given, Step-Nakama
	# refuses to bring the stack up — that combination means a
	# rebuild of an existing deployment, and booting Nakama against
	# an empty DB races the restore (DR drill 2026-08-02, Gap B).
	[switch]$RestoreBackup,
	# Deliberately skip the restore and the backup check (brand-new
	# deployment / intentional reset to an empty database).
	[switch]$NoRestore,
	# Skip HostKeys' out-of-band verification and re-pin. Only for
	# when you cannot reach the Hetzner console right now: the run
	# continues on phase-a's own trust-on-first-use key, and every
	# scheduled prod job fails host-key verification until you run
	# `-StartAt HostKeys -StopAt HostKeys`.
	[switch]$SkipHostKeyPin,
	[ValidateSet(
		"PulumiSetup", "S3", "PulumiLogin", "Preview", "Up",
		"WaitBoot", "HostKeys", "Postgres", "Nakama", "Verify",
		"Reencrypt", "Complete"
	)]
	[string]$StartAt = "PulumiSetup",
	[ValidateSet(
		"PulumiSetup", "S3", "PulumiLogin", "Preview", "Up",
		"WaitBoot", "HostKeys", "Postgres", "Nakama", "Verify",
		"Reencrypt", "Complete"
	)]
	[string]$StopAt = "Complete"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

# --------------------------------------------------------------------
# Paths
# --------------------------------------------------------------------
$RepoRoot   = (Resolve-Path "$PSScriptRoot\..").Path
$MigDir     = "$HOME\.hopnbop-migration"
$StateFile  = "$MigDir\state.json"
$CredsFile  = "$MigDir\credentials.env"
$PulumiDir  = "$RepoRoot\infra\pulumi\snoringcat-platform"
$RemoteSrc  = "$RepoRoot\infra\remote"
$NakamaKey  = "$MigDir\ssh\nakama"
$PostgresKey= "$MigDir\ssh\postgres"
# phase-a's own host verification (trust-on-first-use while a fresh
# host boots); HostKeys overwrites the nakama entries in it with the
# out-of-band-verified keys, so both files agree from then on.
$MigKnownHosts    = "$MigDir\known_hosts"
# The synced, human-verified pin every scheduled prod job and skill
# checks with StrictHostKeyChecking=yes (claude-config's
# rules/scheduling.md, "Host-key pinning").
$ClaudeConfigDir  = "$HOME\Repositories\claude-config"
$PinnedKnownHosts = "$ClaudeConfigDir\jobs\known_hosts"
$PinnedHostName   = "nakama.snoringcat.games"

$env:PATH = "$HOME\.pulumi\bin;$env:PATH"
$env:AWS_PROFILE = "hopnbop"
$env:AWS_REGION  = "us-west-2"

# --------------------------------------------------------------------
# Logging
# --------------------------------------------------------------------
function Log {
	param([string]$msg)
	$ts = Get-Date -Format "HH:mm:ss"
	Write-Host "[$ts] $msg" -ForegroundColor Cyan
}

function Note {
	param([string]$msg)
	Log $msg
	if (Test-Path $StateFile) {
		$s = Get-Content $StateFile -Raw | ConvertFrom-Json
		$entry = "[$(Get-Date -Format 'o')] $msg"
		# Append note (works whether notes is empty or populated).
		$existing = @($s.phases.A.notes)
		$s.phases.A.notes = @($existing + $entry)
		Save-State $s
	}
}

function Save-State {
	param($state)
	$state | ConvertTo-Json -Depth 12 | Out-File -Encoding ASCII $StateFile
}

function Read-State { Get-Content $StateFile -Raw | ConvertFrom-Json }

# --------------------------------------------------------------------
# Native exit-code helpers
# --------------------------------------------------------------------
function Invoke-Checked {
	param([string]$desc, [scriptblock]$action)
	& $action
	if ($LASTEXITCODE -ne 0) {
		throw "$desc failed (exit $LASTEXITCODE)"
	}
}

# --------------------------------------------------------------------
# Source credentials.env into the environment
# --------------------------------------------------------------------
function Source-Credentials {
	if (-not (Test-Path $CredsFile)) {
		throw "credentials.env missing at $CredsFile"
	}
	Get-Content $CredsFile | ForEach-Object {
		if ($_ -match '^([A-Z_][A-Z0-9_]*)=(.*)$') {
			Set-Item "Env:$($Matches[1])" $Matches[2]
		}
	}
}

# Ensure a generated secret exists (env > creds file > generate).
function Ensure-Secret {
	param([string]$varName)
	$current = [Environment]::GetEnvironmentVariable($varName)
	if ($current) { return }
	$existing = Get-Content $CredsFile | Where-Object { $_ -match "^$varName=(.+)$" }
	if ($existing) {
		$val = ($existing -split '=', 2)[1]
		Set-Item "Env:$varName" $val
		return
	}
	$bytes = New-Object byte[] 32
	[System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
	$val = [Convert]::ToBase64String($bytes)
	Set-Item "Env:$varName" $val
	Add-Content -Encoding ASCII $CredsFile "$varName=$val"
	Note "Generated $varName, appended to credentials.env"
}

# --------------------------------------------------------------------
# SSH wrappers
# --------------------------------------------------------------------
function Ssh-Run {
	param(
		[string]$ip,
		[string]$keyPath,
		[string]$cmd
	)
	# This file is CRLF in a Windows checkout (autocrlf), and so is
	# any multi-line here-string built in it. Remote bash keeps the
	# CR: `set -e<CR>` is an invalid option, `fi<CR>` is not `fi`,
	# and `&&<CR>` chains onto a bogus `$'\r'` command, so later
	# lines run even when earlier ones failed. No remote command
	# ever needs a literal CR (same fix as claude-config's
	# Invoke-RemoteCommand.ps1).
	$cmd = $cmd -replace "`r`n", "`n"
	ssh -i $keyPath `
		-o StrictHostKeyChecking=accept-new `
		-o UserKnownHostsFile="$MigKnownHosts" `
		-o ConnectTimeout=15 `
		-o BatchMode=yes `
		"root@$ip" $cmd
}

function Scp-Up {
	param(
		[string]$ip,
		[string]$keyPath,
		[string]$src,
		[string]$dst,
		[switch]$Recurse
	)
	$args = @(
		"-i", $keyPath,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=$MigKnownHosts"
	)
	if ($Recurse) { $args += "-r" }
	$args += @($src, "root@${ip}:${dst}")
	& scp @args
}

function Wait-CloudInit {
	param([string]$ip, [string]$keyPath, [string]$label)
	Log "Waiting for cloud-init bootstrap on $label ($ip). This includes apt upgrade + Docker install; can take 5-10 min."
	$deadline = (Get-Date).AddMinutes(20)
	while ((Get-Date) -lt $deadline) {
		$out = Ssh-Run $ip $keyPath 'test -f /var/lib/cloud/snoringcat-bootstrap-done && echo READY' 2>&1
		$exit = $LASTEXITCODE
		if ($exit -eq 0 -and "$out" -match 'READY') {
			Note "$label cloud-init complete"
			return
		}
		# sshd is up but presents a different key than the one
		# known_hosts recorded for this IP. Retrying cannot fix
		# that, and it used to burn the full 20 minutes silently.
		if ("$out" -match 'REMOTE HOST IDENTIFICATION HAS CHANGED|Host key verification failed') {
			throw ("$label ($ip) presents a different SSH host key than " +
				"$MigKnownHosts recorded for that IP. Expected only if a " +
				"rebuilt host reused the IP; otherwise treat it as a " +
				"possible MITM. For a rebuild: ssh-keygen -R $ip -f " +
				"`"$MigKnownHosts`", then re-run with -StartAt WaitBoot " +
				"(HostKeys re-verifies the new key out-of-band before " +
				"any secret is copied to the host).")
		}
		Start-Sleep -Seconds 20
	}
	throw "Timed out waiting for cloud-init on $label ($ip)"
}

# --------------------------------------------------------------------
# State helpers
# --------------------------------------------------------------------
# Set-StrictMode makes reading a property that state.json lacks throw,
# and assigning one silently requires it to exist. These read and
# write state fields whether or not they are present.
function Get-StateField {
	param($obj, [string]$name)
	if ($null -eq $obj) { return $null }
	$p = $obj.PSObject.Properties[$name]
	if ($null -eq $p) { return $null }
	return $p.Value
}

function Set-StateField {
	param($obj, [string]$name, $value)
	if ($obj.PSObject.Properties[$name]) {
		$obj.$name = $value
	} else {
		$obj | Add-Member -NotePropertyName $name -NotePropertyValue $value
	}
}

# The separate postgres-prod-1 host's IP, or $null on the single-host
# stack. Step-Up blanks the field whenever the Pulumi stack stops
# exporting postgres_public_ip, so a value left over from the
# pre-consolidation era cannot send WaitBoot/Postgres to a dead IP.
function Get-PostgresHostIp {
	param($state)
	$ip = Get-StateField (Get-StateField $state 'infrastructure') 'hetzner_postgres_ip'
	if ([string]::IsNullOrWhiteSpace("$ip")) { return $null }
	return "$ip".Trim()
}

# --------------------------------------------------------------------
# DR restore (R2 pg-backup dumps)
# --------------------------------------------------------------------
function Get-R2BackupBucket {
	if ($env:R2_BUCKET) { return $env:R2_BUCKET }
	return "hopnbop-pulumi-state-r2"
}

function Get-LatestPgBackupKey {
	# Newest pg-backups/postgres-YYYY-MM-DD.sql.gz key in R2, or
	# $null when none exist (genuinely fresh deployment). The
	# date-stamped names sort lexicographically, so newest == last.
	$env:AWS_ACCESS_KEY_ID = $env:R2_ACCESS_KEY_ID
	$env:AWS_SECRET_ACCESS_KEY = $env:R2_SECRET_ACCESS_KEY
	$listing = aws s3 ls "s3://$(Get-R2BackupBucket)/pg-backups/" `
		--endpoint-url $env:R2_ENDPOINT 2>$null
	if ($LASTEXITCODE -ne 0 -or -not $listing) { return $null }
	$names = @($listing | ForEach-Object { ($_ -split '\s+')[-1] } |
		Where-Object { $_ -match '^postgres-\d{4}-\d{2}-\d{2}\.sql\.gz$' } |
		Sort-Object)
	if ($names.Count -eq 0) { return $null }
	return "pg-backups/$($names[-1])"
}

function Invoke-PgRestore {
	# Replays the newest R2 pg_dumpall dump into the stack's
	# Postgres while Nakama is still down. Called from Step-Nakama
	# between `compose build` and the first `compose up`, because
	# `up -d caddy` cascades via depends_on into Nakama's first
	# boot — a restore attempted any later races Nakama's
	# migrate-up and live writes (DR drill 2026-08-02, Gap B).
	#
	# The dump is pg_dumpall output: it re-CREATEs the nakama role
	# + database and \connect's into it, so psql must target the
	# `postgres` maintenance DB, NOT -d nakama. Role-exists errors
	# during replay are expected noise (the fresh container already
	# created the nakama role), hence ON_ERROR_STOP=0 plus the
	# table-count verification at the end.
	param([string]$ip)

	if (-not $env:POSTGRES_PASSWORD) {
		throw "POSTGRES_PASSWORD missing from credentials.env; cannot restore"
	}
	$key = Get-LatestPgBackupKey
	if (-not $key) {
		throw "-RestoreBackup given but R2 has no pg-backups/postgres-*.sql.gz"
	}
	Note "Restore: replaying $key (Nakama still down)"

	# Pull locally, then scp up: at this point in phase-a the fresh
	# host has neither an aws CLI config nor R2 creds (phase-b's
	# pg-backup step provisions those later).
	$tmpDump = Join-Path ([System.IO.Path]::GetTempPath()) "dr-restore.sql.gz"
	Invoke-Checked "aws s3 cp (dump from R2)" {
		$env:AWS_ACCESS_KEY_ID = $env:R2_ACCESS_KEY_ID
		$env:AWS_SECRET_ACCESS_KEY = $env:R2_SECRET_ACCESS_KEY
		aws s3 cp "s3://$(Get-R2BackupBucket)/$key" $tmpDump `
			--endpoint-url $env:R2_ENDPOINT --no-progress | Out-Null
	}
	try {
		Invoke-Checked "scp dump to host" {
			Scp-Up $ip $NakamaKey $tmpDump "/tmp/dr-restore.sql.gz" | Out-Null
		}
	} finally {
		Remove-Item $tmpDump -Force -ErrorAction SilentlyContinue
	}

	# Postgres alone first — deliberately NOT an `up -d` on the
	# whole stack. Same health-poll pattern as Step-Postgres.
	Invoke-Checked "compose up postgres (pre-restore)" {
		Ssh-Run $ip $NakamaKey "cd /opt/nakama && docker compose up -d postgres" | Out-Null
	}
	$deadline = (Get-Date).AddMinutes(3)
	$healthy = $false
	while ((Get-Date) -lt $deadline) {
		$st = Ssh-Run $ip $NakamaKey "docker inspect --format '{{.State.Health.Status}}' postgres 2>/dev/null" 2>&1
		if ($LASTEXITCODE -eq 0 -and "$st".Trim() -eq "healthy") { $healthy = $true; break }
		Start-Sleep -Seconds 5
	}
	if (-not $healthy) { throw "Postgres did not become healthy before restore" }

	# Base64 the remote script to defeat the PowerShell -> ssh ->
	# bash quoting layers (same trick as claude-config's
	# Invoke-RemoteSql). `docker stop nakama` covers idempotent
	# re-runs where a prior partial run already booted Nakama;
	# WITH (FORCE) clears any straggler sessions (postgres-exporter
	# reconnects on its own).
	$restoreScript = @"
set -e
docker stop nakama >/dev/null 2>&1 || true
docker exec -e PGPASSWORD='$env:POSTGRES_PASSWORD' postgres \
  psql --no-password -h 127.0.0.1 -U nakama -d postgres \
  -c 'DROP DATABASE IF EXISTS nakama WITH (FORCE)' >/dev/null
gunzip -c /tmp/dr-restore.sql.gz \
  | docker exec -i -e PGPASSWORD='$env:POSTGRES_PASSWORD' postgres \
    psql --no-password -h 127.0.0.1 -U nakama -d postgres \
    -v ON_ERROR_STOP=0 -q >/dev/null 2>&1
rm -f /tmp/dr-restore.sql.gz
docker exec -e PGPASSWORD='$env:POSTGRES_PASSWORD' postgres \
  psql --no-password -h 127.0.0.1 -U nakama -d nakama -tA \
  -c "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'"
"@
	# The here-string inherits this file's CRLF checkout, and base64
	# carries the CRs through Ssh-Run's own normalization. Under bash,
	# `set -e<CR>` fails, and each `\<CR>` escapes the CR instead of
	# continuing the line, so `docker exec ... postgres` runs with a
	# stray CR argument and the psql half runs on the bare host. The
	# DROP and the replay never happen, and the run dies at the
	# table-count check below. Strip the CRs before encoding.
	$restoreScript = $restoreScript -replace "`r`n", "`n"
	$b64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($restoreScript))
	$tableCount = Ssh-Run $ip $NakamaKey "echo $b64 | base64 -d | bash"
	if ($LASTEXITCODE -ne 0) {
		throw "Restore pipeline failed (exit $LASTEXITCODE)"
	}
	$count = 0
	[void][int]::TryParse("$tableCount".Trim(), [ref]$count)
	if ($count -lt 1) {
		throw "Restore verification failed: nakama DB has no public tables"
	}
	Note "Restore complete: $count tables in nakama DB (from $key)"
}

# --------------------------------------------------------------------
# Host-key pinning (Step-HostKeys)
# --------------------------------------------------------------------
$PinBlockBegin = "# --- current pin (rewritten by phase-a.ps1 Step-HostKeys) ---"
$PinBlockEnd   = "# --- end current pin ---"
$HostKeyTypeOrder = @(
	"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384",
	"ecdsa-sha2-nistp521", "ssh-rsa"
)

# OpenSSH's fingerprint of a base64 key blob: "SHA256:" + the unpadded
# base64 SHA-256 of the blob, i.e. what `ssh-keygen -lf` prints.
function Get-KeyFingerprint {
	param([string]$b64)
	$sha = [Security.Cryptography.SHA256]::Create()
	try {
		$hash = $sha.ComputeHash([Convert]::FromBase64String($b64))
	} finally { $sha.Dispose() }
	return "SHA256:" + [Convert]::ToBase64String($hash).TrimEnd('=')
}

# The ed25519 host key the host presents on the wire, as
# "ssh-ed25519 <base64>". Captured by letting ssh itself record it
# into a fresh, throwaway known_hosts: Windows' bundled ssh-keyscan
# (Win32-OpenSSH 9.5) negotiates sntrup761x25519 with Ubuntu 24.04's
# sshd and then dies with "choose_kex: unsupported KEX method". ssh
# records the key before user auth, so the file is read whatever the
# exit code.
function Get-PresentedEd25519Key {
	param([string]$ip)
	$tmpKh = New-TemporaryFile
	try {
		ssh -i $NakamaKey `
			-o StrictHostKeyChecking=accept-new `
			-o "UserKnownHostsFile=$($tmpKh.FullName)" `
			-o HostKeyAlgorithms=ssh-ed25519 `
			-o HashKnownHosts=no `
			-o UpdateHostKeys=no `
			-o ConnectTimeout=15 `
			-o BatchMode=yes `
			-o LogLevel=ERROR `
			"root@$ip" true 2>&1 | Out-Null
		foreach ($l in (Get-Content $tmpKh.FullName)) {
			$parts = "$l".Trim() -split '\s+'
			if ($parts.Count -ge 3 -and $parts[1] -eq "ssh-ed25519") {
				return "ssh-ed25519 $($parts[2])"
			}
		}
	} finally {
		Remove-Item $tmpKh.FullName -Force -ErrorAction SilentlyContinue
	}
	throw "Could not capture an ed25519 host key from $ip (is sshd up?)"
}

# Every host key type, read from /etc/ssh/*.pub over an SSH session
# that accepts ONLY the verified ed25519 key. The ecdsa/rsa keys thus
# inherit the out-of-band verification instead of being trusted on
# first use: an attacker able to answer for the IP could otherwise
# plant their own ecdsa key next to the genuine ed25519 one and then
# negotiate ecdsa. Returns an ordered type -> base64 map.
function Get-HostKeysOverPinnedSession {
	param([string]$ip, [string]$edKey)
	$tmpKh = New-TemporaryFile
	try {
		[IO.File]::WriteAllText($tmpKh.FullName, "$ip $edKey`n",
			[Text.UTF8Encoding]::new($false))
		$remote = 'for t in ed25519 ecdsa rsa; do f=/etc/ssh/ssh_host_${t}_key.pub; test -f $f && cat $f; done; true'
		$out = @(ssh -i $NakamaKey `
			-o StrictHostKeyChecking=yes `
			-o "UserKnownHostsFile=$($tmpKh.FullName)" `
			-o HostKeyAlgorithms=ssh-ed25519 `
			-o ConnectTimeout=15 `
			-o BatchMode=yes `
			"root@$ip" $remote)
		if ($LASTEXITCODE -ne 0) {
			throw "Reading /etc/ssh/*.pub over the pinned session failed (exit $LASTEXITCODE)"
		}
	} finally {
		Remove-Item $tmpKh.FullName -Force -ErrorAction SilentlyContinue
	}
	$found = @{}
	foreach ($l in $out) {
		$parts = "$l".Trim() -split '\s+'
		if ($parts.Count -ge 2 -and $parts[0] -match '^(ssh-|ecdsa-)') {
			$found[$parts[0]] = $parts[1]
		}
	}
	if ($found["ssh-ed25519"] -ne ($edKey -split ' ')[1]) {
		throw "/etc/ssh/ssh_host_ed25519_key.pub does not match the verified key"
	}
	$keys = [ordered]@{}
	foreach ($t in $HostKeyTypeOrder) {
		if ($found.ContainsKey($t)) { $keys[$t] = $found[$t] }
	}
	foreach ($t in ($found.Keys | Sort-Object)) {
		if (-not $keys.Contains($t)) { $keys[$t] = $found[$t] }
	}
	return $keys
}

# Splits a known_hosts file into its leading comment header (kept
# verbatim), the host field of its entries, and type -> base64 key.
function Read-PinnedKnownHosts {
	param([string]$path)
	$header = [System.Collections.Generic.List[string]]::new()
	$keys = [ordered]@{}
	$hostField = ""
	if (Test-Path $path) {
		$inHeader = $true
		foreach ($line in (Get-Content $path)) {
			if ($inHeader -and ($line -match '^\s*(#|$)')) {
				$header.Add($line)
				continue
			}
			$inHeader = $false
			$parts = $line.Trim() -split '\s+'
			if ($parts.Count -lt 3 -or $parts[0].StartsWith("#")) { continue }
			$hostField = $parts[0]
			$keys[$parts[1]] = $parts[2]
		}
	}
	return [pscustomobject]@{
		Header    = @($header)
		HostField = $hostField
		Hosts     = @(if ($hostField) { $hostField -split ',' })
		Keys      = $keys
	}
}

function Format-KnownHostsEntries {
	param([string]$hostField, $keys)
	$lines = @()
	foreach ($t in $keys.Keys) { $lines += "$hostField $t $($keys[$t])" }
	return $lines
}

function Write-LfFile {
	param([string]$path, [string[]]$lines)
	[IO.File]::WriteAllText($path, (($lines -join "`n") + "`n"),
		[Text.UTF8Encoding]::new($false))
}

# Rewrites the synced pin: header kept verbatim except for the
# provenance block between the pin markers (appended if missing),
# then one entry per key type for "<hostname>,<ip>".
function Write-PinnedKnownHosts {
	param([string]$path, [string[]]$header, [string]$hostField, $keys,
		[string]$fingerprint)
	$block = @(
		$PinBlockBegin,
		"# Captured $(Get-Date -Format 'yyyy-MM-dd') by phase-a.ps1 Step-HostKeys: the operator",
		"# confirmed the ed25519 fingerprint out-of-band in the Hetzner web",
		"# console, and the other key types were read from /etc/ssh/*.pub over",
		"# an SSH session that accepted only that ed25519 key.",
		"#   $fingerprint",
		$PinBlockEnd
	)
	$out = [System.Collections.Generic.List[string]]::new()
	$skipping = $false
	$placed = $false
	foreach ($line in $header) {
		if ($line -eq $PinBlockBegin) {
			$skipping = $true
			$out.AddRange([string[]]$block)
			$placed = $true
			continue
		}
		if ($skipping) {
			if ($line -eq $PinBlockEnd) { $skipping = $false }
			continue
		}
		$out.Add($line)
	}
	if (-not $placed) { $out.AddRange([string[]]$block) }
	$out.AddRange([string[]](Format-KnownHostsEntries $hostField $keys))
	Write-LfFile $path @($out)
}

# Mirrors the verified keys into phase-a's own known_hosts: drops every
# entry naming one of $dropHosts (the pinned hostname, the new IP, the
# previously pinned IP), keeps the rest, appends the new entries.
function Update-MigKnownHosts {
	param([string]$path, [string[]]$dropHosts, [string]$hostField, $keys)
	$kept = [System.Collections.Generic.List[string]]::new()
	if (Test-Path $path) {
		foreach ($line in (Get-Content $path)) {
			$first = ($line.Trim() -split '\s+')[0]
			$named = @($first -split ',' | Where-Object { $dropHosts -contains $_ })
			if ($named.Count -eq 0) { $kept.Add($line) }
		}
	}
	$kept.AddRange([string[]](Format-KnownHostsEntries $hostField $keys))
	Write-LfFile $path @($kept)
}

# Split out so tests can stub it: Read-Host cannot prompt anyone when
# stdin is redirected (scheduled / piped runs).
function Test-OperatorInput { return -not [Console]::IsInputRedirected }

function Confirm-HostKeyOutOfBand {
	param([string]$ip, [string]$fp, [string]$pinnedFp)
	Write-Host ""
	Write-Host ("=" * 72) -ForegroundColor Yellow
	Write-Host "nakama-prod-1 ($ip) presents an SSH host key that is not pinned." -ForegroundColor Yellow
	if ($pinnedFp) { Write-Host "Currently pinned ed25519: $pinnedFp (expected to differ after a rebuild)" }
	Write-Host ""
	Write-Host "  ed25519 fingerprint on the wire:  $fp" -ForegroundColor Yellow
	Write-Host ""
	Write-Host "Verify it OUT-OF-BAND before anything trusts it:"
	Write-Host "  1. https://console.hetzner.com -> project snoringcat-platform ->"
	Write-Host "     server nakama-prod-1 -> Console (the >_ web console)."
	Write-Host "  2. Log in as root. No root password on a fresh host? Use the"
	Write-Host "     server's Rescue tab -> 'Reset root password' first."
	Write-Host "  3. Run:  ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub"
	Write-Host "  4. Its SHA256:... must match the fingerprint above exactly."
	Write-Host ""
	Write-Host "A mismatch means something other than the new host answered on"
	Write-Host "$ip. Stop and investigate. Do not confirm."
	Write-Host ("=" * 72) -ForegroundColor Yellow
	if (-not (Test-OperatorInput)) {
		throw ("HostKeys needs an interactive operator to confirm $fp " +
			"out-of-band. Re-run interactively, or pass -SkipHostKeyPin " +
			"(scheduled prod jobs then fail until you re-pin).")
	}
	$answer = Read-Host "Type 'yes' if the console printed exactly $fp"
	if ("$answer".Trim() -ne "yes") {
		throw ("Host key not confirmed; stopped before any secret was " +
			"copied to the host. If the console showed a different " +
			"fingerprint, treat it as a possible MITM.")
	}
	Note "HostKeys: operator confirmed $fp out-of-band"
}

function Show-RepinFollowUps {
	param([string[]]$oldHosts, [string]$ip)
	Write-Host ""
	Write-Host "claude-config/jobs/known_hosts now pins $PinnedHostName,$ip." -ForegroundColor Green
	Write-Host "It reaches the scheduled jobs and the other machine through"
	Write-Host "claude-config: the desktop's hourly sync-claude-config job commits"
	Write-Host "and pushes it within the hour. To push it now (and always on the"
	Write-Host "laptop, which runs no scheduled jobs):"
	Write-Host "  & `"$ClaudeConfigDir\sync-if-changed.ps1`""
	Write-Host "Until it is pushed and pulled, prod jobs keep failing host-key checks."
	# The pin covers the hostname, but jobs that SSH to a hardcoded IP
	# still point at the old (now dead or reassigned) address.
	$oldIps = @($oldHosts | Where-Object { $_ -match '^\d{1,3}(\.\d{1,3}){3}$' -and $_ -ne $ip })
	foreach ($old in $oldIps) {
		$hits = @(Get-ChildItem -Path $ClaudeConfigDir -Recurse -File -ErrorAction SilentlyContinue |
			Where-Object { $_.FullName -notmatch '\\(\.git|status|logs|state|secrets)\\' -and $_.Name -ne "known_hosts" } |
			Select-String -SimpleMatch $old -List -ErrorAction SilentlyContinue |
			ForEach-Object { $_.Path })
		if ($hits.Count -gt 0) {
			Write-Host ""
			Write-Host "The host IP changed ($old -> $ip). These claude-config files still" -ForegroundColor Yellow
			Write-Host "SSH to (or document) the old IP; update them before the jobs recover:" -ForegroundColor Yellow
			foreach ($h in $hits) { Write-Host "  $h" }
			Note "HostKeys: $($hits.Count) claude-config file(s) still reference the old IP $old"
		}
	}
}

# --------------------------------------------------------------------
# Step ordering
# --------------------------------------------------------------------
$StepOrder = @(
	"PulumiSetup", "S3", "PulumiLogin", "Preview", "Up",
	"WaitBoot", "HostKeys", "Postgres", "Nakama", "Verify",
	"Reencrypt", "Complete"
)
$StartIdx = $StepOrder.IndexOf($StartAt)
$StopIdx  = $StepOrder.IndexOf($StopAt)
function Should-Run {
	param([string]$step)
	$idx = $StepOrder.IndexOf($step)
	return ($idx -ge $StartIdx -and $idx -le $StopIdx)
}

# --------------------------------------------------------------------
# Steps
# --------------------------------------------------------------------
function Step-PulumiSetup {
	if (-not (Should-Run "PulumiSetup")) { return }
	Note "Step: PulumiSetup"
	Push-Location $PulumiDir
	try {
		if (-not (Test-Path "$PulumiDir\go.sum")) {
			Log "go get + go mod tidy (first-time)"
			Invoke-Checked "go get pulumi sdk"   { go get github.com/pulumi/pulumi/sdk/v3 }
			Invoke-Checked "go get hcloud sdk"   { go get github.com/pulumi/pulumi-hcloud/sdk }
			Invoke-Checked "go get cloudflare sdk" { go get github.com/pulumi/pulumi-cloudflare/sdk/v5 }
			Invoke-Checked "go mod tidy"         { go mod tidy }
		}
		Invoke-Checked "go build ." { go build . }
	} finally { Pop-Location }
}

function Step-S3 {
	if (-not (Should-Run "S3")) { return }
	Note "Step: S3 backend bucket — SKIPPED (state on R2)"
	# Pulumi state migrated off AWS S3 to Cloudflare R2 on
	# 2026-05-04 (Phase F+). Bucket: hopnbop-pulumi-state-r2.
	# Pulumi reads R2 S3-compat creds via the standard
	# AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY env vars
	# (Step-PulumiLogin sets those from R2_*).
	Log "S3 bucket creation skipped — Pulumi state lives on R2."
}

function Step-PulumiLogin {
	if (-not (Should-Run "PulumiLogin")) { return }
	Note "Step: Pulumi login + stack init + config"
	Push-Location $PulumiDir
	try {
		# Default to R2 backend (S3-compatible, no AWS dependency).
		# Pulumi reads AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
		# under the hood; route the R2_* creds through those env
		# vars for the duration of this step.
		if ([string]::IsNullOrEmpty($env:R2_ACCESS_KEY_ID) `
				-or [string]::IsNullOrEmpty($env:R2_SECRET_ACCESS_KEY) `
				-or [string]::IsNullOrEmpty($env:R2_ENDPOINT)) {
			throw ("R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, and " +
				"R2_ENDPOINT must be set in credentials.env. Create " +
				"the R2 API token in the Cloudflare dashboard " +
				"(R2 -> Manage R2 API Tokens -> Create), scoped to " +
				"the hopnbop-pulumi-state-r2 bucket with " +
				"object_read_and_write.")
		}
		$env:AWS_ACCESS_KEY_ID = $env:R2_ACCESS_KEY_ID
		$env:AWS_SECRET_ACCESS_KEY = $env:R2_SECRET_ACCESS_KEY
		$backend = $env:PULUMI_BACKEND_URL
		if ([string]::IsNullOrEmpty($backend)) {
			$backend = ("s3://hopnbop-pulumi-state-r2" `
				+ "?endpoint=$($env:R2_ENDPOINT)" `
				+ "&region=auto&s3ForcePathStyle=true")
		}
		Invoke-Checked "pulumi login (R2)" {
			pulumi login $backend | Out-Null
		}
		# Create or select stack 'prod'.
		$stacksJson = pulumi stack ls --json 2>$null
		$hasProd = $false
		if ($LASTEXITCODE -eq 0 -and $stacksJson) {
			$stacks = $stacksJson | ConvertFrom-Json
			foreach ($st in $stacks) {
				if ($st.name -eq "prod") { $hasProd = $true; break }
			}
		}
		if (-not $hasProd) {
			Log "Initializing stack 'prod'"
			Invoke-Checked "pulumi stack init" { pulumi stack init prod | Out-Null }
		} else {
			Log "Stack 'prod' already exists; selecting"
			Invoke-Checked "pulumi stack select" { pulumi stack select prod | Out-Null }
		}
		# Provider config (idempotent — overwrites).
		Invoke-Checked "pulumi config set hcloud:token" {
			pulumi config set hcloud:token $env:HCLOUD_TOKEN --secret | Out-Null
		}
		Invoke-Checked "pulumi config set cloudflare:apiToken" {
			pulumi config set cloudflare:apiToken $env:CLOUDFLARE_API_TOKEN --secret | Out-Null
		}
	} finally { Pop-Location }
}

function Step-Preview {
	if (-not (Should-Run "Preview")) { return }
	if ($SkipPreview) { Log "SkipPreview set, skipping"; return }
	Note "Step: pulumi preview"
	Push-Location $PulumiDir
	try {
		pulumi preview --diff
		if ($LASTEXITCODE -ne 0) { throw "pulumi preview failed (exit $LASTEXITCODE)" }
	} finally { Pop-Location }
}

function Step-Up {
	if (-not (Should-Run "Up")) { return }
	Note "Step: pulumi up"
	Push-Location $PulumiDir
	try {
		Invoke-Checked "pulumi up" { pulumi up --yes --skip-preview }
		Log "Capturing stack outputs"
		$json = pulumi stack output --json --show-secrets
		if ($LASTEXITCODE -ne 0) { throw "pulumi stack output failed" }
		$outs = $json | ConvertFrom-Json
		$outProps = $outs.PSObject.Properties.Name
		$s = Read-State
		$s.infrastructure.hetzner_nakama_server_id    = "$($outs.nakama_server_id)"
		$s.infrastructure.hetzner_nakama_ip           = $outs.nakama_public_ip
		$s.infrastructure.hetzner_private_network_id  = "$($outs.private_network_id)"
		# postgres_* outputs were dropped in the 2026-05-06
		# single-host consolidation. Tolerate their absence so
		# the script keeps working on the consolidated stack and
		# remains forward-compatible if anyone re-introduces a
		# split tier later. Absent means "no such host": blank the
		# fields rather than keep a stale value. A pre-consolidation
		# state.json still named the destroyed postgres-prod-1
		# (5.78.118.214), so WaitBoot would have polled a dead IP.
		$pgId = if ($outProps -contains "postgres_server_id") { "$($outs.postgres_server_id)" } else { "" }
		$pgIp = if ($outProps -contains "postgres_public_ip") { "$($outs.postgres_public_ip)" } else { "" }
		Set-StateField $s.infrastructure "hetzner_postgres_server_id" $pgId
		Set-StateField $s.infrastructure "hetzner_postgres_ip" $pgIp
		if (-not $pgIp) {
			Log "Stack exports no separate postgres host (single-host stack)"
		}
		Save-State $s
		Note "Stack outputs persisted to state.json"
	} finally { Pop-Location }
}

function Step-WaitBoot {
	if (-not (Should-Run "WaitBoot")) { return }
	Note "Step: WaitBoot (cloud-init)"
	$s = Read-State
	$pgIp = Get-PostgresHostIp $s
	if ($pgIp) {
		Wait-CloudInit $pgIp $PostgresKey "postgres-prod-1"
	} else {
		Log ("WaitBoot: no separate postgres host in state.json " +
			"(single-host stack); waiting on nakama-prod-1 only")
	}
	Wait-CloudInit $s.infrastructure.hetzner_nakama_ip $NakamaKey "nakama-prod-1"
}

function Step-HostKeys {
	if (-not (Should-Run "HostKeys")) { return }
	Note "Step: HostKeys (verify + pin nakama-prod-1's SSH host keys)"
	if ($SkipHostKeyPin) {
		$bar = "!" * 72
		Write-Host ""
		Write-Host $bar -ForegroundColor Red
		Write-Host "!! -SkipHostKeyPin: nakama-prod-1's SSH host keys were NOT verified" -ForegroundColor Red
		Write-Host "!! out-of-band and NOT pinned. This run trusts whichever key" -ForegroundColor Red
		Write-Host "!! answered first, and EVERY scheduled prod job (prod-liveness-ping," -ForegroundColor Red
		Write-Host "!! slo-burn-rate, job-watchdog, service-status-sync, backup-restore-" -ForegroundColor Red
		Write-Host "!! smoke, ...) fails host-key verification against" -ForegroundColor Red
		Write-Host "!! claude-config/jobs/known_hosts until you re-pin with:" -ForegroundColor Red
		Write-Host "!!   pwsh -File scripts/phase-a.ps1 -StartAt HostKeys -StopAt HostKeys" -ForegroundColor Red
		Write-Host $bar -ForegroundColor Red
		Note "HostKeys SKIPPED (-SkipHostKeyPin): host keys unverified; claude-config/jobs/known_hosts NOT updated"
		return
	}
	$s = Read-State
	$ip = "$($s.infrastructure.hetzner_nakama_ip)".Trim()
	if (-not $ip) {
		throw "state.json has no infrastructure.hetzner_nakama_ip; run the Up step first"
	}
	$hostField = "$PinnedHostName,$ip"
	$edKey = Get-PresentedEd25519Key $ip
	$edB64 = ($edKey -split ' ')[1]
	$fp = Get-KeyFingerprint $edB64
	$pinned = Read-PinnedKnownHosts $PinnedKnownHosts
	$pinnedEd = $pinned.Keys["ssh-ed25519"]
	$dropHosts = @($pinned.Hosts) + @($PinnedHostName, $ip)

	# Normal re-run against an unchanged host: nothing to verify.
	# Still re-mirror into phase-a's own file, which may predate the
	# pin (it used to hold ed25519 alone, keyed by IP).
	if ($pinnedEd -eq $edB64 -and $pinned.HostField -eq $hostField) {
		Update-MigKnownHosts $MigKnownHosts $dropHosts $hostField $pinned.Keys
		Log "HostKeys: $hostField already pinned ($fp); claude-config/jobs/known_hosts unchanged"
		return
	}

	if ($pinnedEd -eq $edB64) {
		# Same verified identity, different host field (the IP moved
		# but the keys came along): re-pin without a console check.
		Log "HostKeys: ed25519 unchanged ($fp); re-pinning '$($pinned.HostField)' as '$hostField'"
	} else {
		$pinnedFp = if ($pinnedEd) { Get-KeyFingerprint $pinnedEd } else { "" }
		Confirm-HostKeyOutOfBand $ip $fp $pinnedFp
	}

	$keys = Get-HostKeysOverPinnedSession $ip $edKey
	Write-PinnedKnownHosts $PinnedKnownHosts $pinned.Header $hostField $keys $fp
	Update-MigKnownHosts $MigKnownHosts $dropHosts $hostField $keys
	Note ("HostKeys: pinned $hostField ($fp; $(@($keys.Keys) -join ', ')) " +
		"in claude-config/jobs/known_hosts and $MigKnownHosts")
	Show-RepinFollowUps @($pinned.Hosts) $ip
}

function Step-Postgres {
	if (-not (Should-Run "Postgres")) { return }
	Note "Step: Postgres compose up"
	# Needed either way: on the single-host stack Step-Nakama renders
	# it into /opt/nakama/.env for the postgres compose service.
	Ensure-Secret "POSTGRES_PASSWORD"
	$s = Read-State
	$ip = Get-PostgresHostIp $s
	if (-not $ip) {
		Note ("Postgres: SKIPPED, single-host stack (no separate " +
			"postgres host in the Pulumi outputs). Postgres starts as " +
			"a compose service in Step-Nakama.")
		return
	}
	Invoke-Checked "ssh mkdir /opt/postgres" {
		Ssh-Run $ip $PostgresKey "mkdir -p /opt/postgres" | Out-Null
	}
	Invoke-Checked "scp docker-compose.yml" {
		Scp-Up $ip $PostgresKey "$RemoteSrc\postgres\docker-compose.yml" "/opt/postgres/" | Out-Null
	}
	Invoke-Checked "scp pg_hba.conf" {
		Scp-Up $ip $PostgresKey "$RemoteSrc\postgres\pg_hba.conf" "/opt/postgres/" | Out-Null
	}
	# Render .env.
	$envFile = New-TemporaryFile
	"POSTGRES_PASSWORD=$env:POSTGRES_PASSWORD" | Out-File -Encoding ASCII -FilePath $envFile.FullName -NoNewline
	Invoke-Checked "scp .env" {
		Scp-Up $ip $PostgresKey $envFile.FullName "/opt/postgres/.env" | Out-Null
	}
	Remove-Item $envFile.FullName -Force
	Invoke-Checked "docker compose up -d (postgres)" {
		Ssh-Run $ip $PostgresKey "cd /opt/postgres && docker compose up -d" | Out-Null
	}
	# Poll for healthy.
	$deadline = (Get-Date).AddMinutes(3)
	$ok = $false
	while ((Get-Date) -lt $deadline) {
		$st = Ssh-Run $ip $PostgresKey "docker inspect --format '{{.State.Health.Status}}' postgres 2>/dev/null" 2>&1
		if ($LASTEXITCODE -eq 0 -and "$st".Trim() -eq "healthy") { $ok = $true; break }
		Start-Sleep -Seconds 5
	}
	if (-not $ok) { throw "Postgres did not become healthy" }
	Note "Postgres healthy"
}

function Step-Nakama {
	if (-not (Should-Run "Nakama")) { return }
	Note "Step: Nakama + Caddy compose up"
	foreach ($v in @(
		# Also ensured by Step-Postgres; repeated so a run that
		# starts here (-StartAt Nakama) still has it.
		"POSTGRES_PASSWORD",
		"NAKAMA_CONSOLE_PASSWORD",
		"NAKAMA_SERVER_KEY",
		"NAKAMA_SESSION_ENCRYPTION_KEY",
		"SIGNALING_HMAC_SECRET"
	)) {
		Ensure-Secret $v
	}
	if (-not $env:SIGNALING_DOMAIN) {
		# Static value, baked alongside the Pulumi DNS record
		# `signaling-a` in main.go. Override via env if anyone
		# ever forks the platform under a different zone.
		Set-Item Env:SIGNALING_DOMAIN "signaling.snoringcat.games"
	}
	$s = Read-State
	$ip = $s.infrastructure.hetzner_nakama_ip
	$privPg = "10.0.1.20"

	Invoke-Checked "ssh mkdir /opt/nakama" {
		Ssh-Run $ip $NakamaKey "mkdir -p /opt/nakama" | Out-Null
	}
	# Render config.yml (literal substitution; no regex).
	# Every ${...} placeholder in config.yml must be replaced —
	# Nakama itself does NOT expand ${...} in config.yml, it just
	# passes the literal string through to runtime.env. If we
	# leave EDGEGAP_APP_NAME/VERSION unsubstituted, the runtime
	# logs `app=${EDGEGAP_APP_NAME} version=${EDGEGAP_APP_VERSION}`
	# and the matchmaker hook silently allocates against the wrong
	# Edgegap app. (Verified the hard way 2026-05-03.)
	$cfg = Get-Content "$RemoteSrc\nakama\config.yml" -Raw
	foreach ($v in @(
		"GOOGLE_OAUTH_CLIENT_ID",
		"GOOGLE_OAUTH_CLIENT_SECRET",
		"FACEBOOK_APP_ID",
		"FACEBOOK_APP_SECRET",
		"EDGEGAP_TOKEN",
		"EDGEGAP_APP_NAME",
		"EDGEGAP_APP_VERSION",
		"NAKAMA_GAME_VERSION",
		"NAKAMA_PROTOCOL_VERSION",
		"SIGNALING_DOMAIN",
		"SIGNALING_HMAC_SECRET"
	)) {
		$val = [Environment]::GetEnvironmentVariable($v)
		$cfg = $cfg.Replace("`${$v}", $val)
	}
	if ($cfg -match '\$\{') {
		$leftover = ([regex]::Matches($cfg, '\$\{[^}]+\}') | ForEach-Object { $_.Value }) -join ', '
		throw "config.yml has unresolved placeholders: $leftover"
	}
	$tmpCfg = New-TemporaryFile
	$cfg | Out-File -Encoding ASCII -FilePath $tmpCfg.FullName
	Invoke-Checked "scp config.yml" {
		Scp-Up $ip $NakamaKey $tmpCfg.FullName "/opt/nakama/config.yml" | Out-Null
	}
	Remove-Item $tmpCfg.FullName -Force

	Invoke-Checked "scp docker-compose.yml" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\nakama\docker-compose.yml" "/opt/nakama/" | Out-Null
	}
	Invoke-Checked "scp Caddyfile" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\nakama\Caddyfile" "/opt/nakama/" | Out-Null
	}
	# caddy/ build context. The compose has `image:
	# caddy-with-ratelimit:local` with `build: ./caddy`, so the
	# Dockerfile must be on disk before any `docker compose up`.
	Invoke-Checked "scp caddy/ build context" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\nakama\caddy" "/opt/nakama/" -Recurse | Out-Null
	}
	# signaling-proxy/ build context. The compose stack has
	# `image: snoringcat-signaling-proxy:local` with
	# `build: ./signaling-proxy`, so the Dockerfile + sources
	# must be on disk before any `docker compose up`. Lives
	# under /opt/nakama/ next to the compose file so the
	# build path resolves cleanly.
	Invoke-Checked "scp signaling-proxy/ build context" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\signaling-proxy" "/opt/nakama/" -Recurse | Out-Null
	}
	# geoip-sidecar/ build context. Same pattern as
	# signaling-proxy. Bind-mounts /var/lib/snoringcat/geoip
	# (populated by the geoip-refresh systemd timer) into the
	# container at /data so the matchmaker hook's HTTP calls
	# get a fresh MMDB.
	Invoke-Checked "scp geoip-sidecar/ build context" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\geoip-sidecar" "/opt/nakama/" -Recurse | Out-Null
	}
	# Render .env for compose. Docker-compose substitutes
	# ${VAR} in docker-compose.yml from this file. The
	# entrypoint references NAKAMA_HTTP_KEY,
	# NAKAMA_REFRESH_ENCRYPTION_KEY, and
	# NAKAMA_CONSOLE_SIGNING_KEY via $$VAR (shell escape past
	# compose's first round of substitution), so they have to
	# be present here.
	foreach ($v in @(
		"NAKAMA_HTTP_KEY",
		"NAKAMA_REFRESH_ENCRYPTION_KEY",
		"NAKAMA_CONSOLE_SIGNING_KEY"
	)) {
		Ensure-Secret $v
	}
	$envLines = @(
		"POSTGRES_PASSWORD=$env:POSTGRES_PASSWORD",
		"POSTGRES_PRIVATE_IP=$privPg",
		"NAKAMA_CONSOLE_PASSWORD=$env:NAKAMA_CONSOLE_PASSWORD",
		"NAKAMA_SERVER_KEY=$env:NAKAMA_SERVER_KEY",
		"NAKAMA_SESSION_ENCRYPTION_KEY=$env:NAKAMA_SESSION_ENCRYPTION_KEY",
		"NAKAMA_REFRESH_ENCRYPTION_KEY=$env:NAKAMA_REFRESH_ENCRYPTION_KEY",
		"NAKAMA_HTTP_KEY=$env:NAKAMA_HTTP_KEY",
		"NAKAMA_CONSOLE_SIGNING_KEY=$env:NAKAMA_CONSOLE_SIGNING_KEY",
		"GOOGLE_OAUTH_CLIENT_ID=$env:GOOGLE_OAUTH_CLIENT_ID",
		"GOOGLE_OAUTH_CLIENT_SECRET=$env:GOOGLE_OAUTH_CLIENT_SECRET",
		"FACEBOOK_APP_ID=$env:FACEBOOK_APP_ID",
		"FACEBOOK_APP_SECRET=$env:FACEBOOK_APP_SECRET",
		# Shared by the signaling-proxy compose service and
		# the Nakama runtime hook (which signs URLs the proxy
		# verifies). MUST match exactly between both.
		"SIGNALING_HMAC_SECRET=$env:SIGNALING_HMAC_SECRET"
	)
	$tmpEnv = New-TemporaryFile
	($envLines -join "`n") | Out-File -Encoding ASCII -FilePath $tmpEnv.FullName
	Invoke-Checked "scp .env" {
		Scp-Up $ip $NakamaKey $tmpEnv.FullName "/opt/nakama/.env" | Out-Null
	}
	Remove-Item $tmpEnv.FullName -Force

	# Build the signaling-proxy image (and rebuild Caddy +
	# anything else that changed) before bringing services up.
	# `compose up --build` is idempotent: no-op when image
	# digests match the build context.
	Invoke-Checked "compose build signaling-proxy" {
		Ssh-Run $ip $NakamaKey "cd /opt/nakama && docker compose build signaling-proxy" | Out-Null
	}

	# DR gate + optional restore. Must happen HERE — after the
	# compose files + .env are on the host, before the first
	# `compose up`: `up -d caddy` cascades via depends_on into
	# Nakama's first boot, and Nakama would migrate-up + write an
	# empty DB that races any later restore (DR drill 2026-08-02,
	# Gap B).
	if ($RestoreBackup) {
		Invoke-PgRestore $ip
	} elseif (-not $NoRestore) {
		if (Get-Command aws -ErrorAction SilentlyContinue) {
			$latestKey = Get-LatestPgBackupKey
			if ($latestKey) {
				throw ("R2 already holds pg backups (latest: $latestKey), " +
					"so this looks like a rebuild of an existing deployment. " +
					"Re-run with -RestoreBackup to restore it before Nakama " +
					"first boots, or -NoRestore to deliberately start from " +
					"an empty database.")
			}
		} else {
			Log "aws CLI not found locally; skipping the pre-boot R2 backup check"
		}
	}

	# Bring Caddy up first so it starts the cert flow, then
	# the signaling-proxy + Nakama. Caddy's depends_on lists
	# both nakama and signaling-proxy so docker-compose will
	# start them when caddy starts; explicit `up -d` for each
	# keeps the failure mode obvious.
	Invoke-Checked "compose up caddy" {
		Ssh-Run $ip $NakamaKey "cd /opt/nakama && docker compose up -d caddy" | Out-Null
	}
	Log "Sleeping 30s for Caddy ACME challenge"
	Start-Sleep -Seconds 30
	Invoke-Checked "compose up signaling-proxy" {
		Ssh-Run $ip $NakamaKey "cd /opt/nakama && docker compose up -d signaling-proxy" | Out-Null
	}
	Invoke-Checked "compose up nakama" {
		Ssh-Run $ip $NakamaKey "cd /opt/nakama && docker compose up -d nakama" | Out-Null
	}
	Note "Nakama + Caddy + signaling-proxy compose up complete"
}

function Step-Verify {
	if (-not (Should-Run "Verify")) { return }
	Note "Step: Verify (healthcheck + anonymous auth)"
	$url = "https://nakama.snoringcat.games/healthcheck"
	$ok = $false
	for ($i = 0; $i -lt 30; $i++) {
		try {
			$r = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 5
			if ($r.StatusCode -eq 200) { $ok = $true; break }
		} catch { }
		Start-Sleep -Seconds 5
	}
	if (-not $ok) { throw "Healthcheck never returned 200" }
	$s = Read-State
	$s.verification.phase_a_healthcheck_at = (Get-Date -Format 'o')
	Save-State $s
	Note "Healthcheck OK"

	# Anonymous device auth via REST.
	$basic = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes("$($env:NAKAMA_SERVER_KEY):"))
	$body = @{ id = "phase-a-smoke-$(Get-Random)" } | ConvertTo-Json -Compress
	$resp = $null
	try {
		$resp = Invoke-RestMethod `
			-Uri "https://nakama.snoringcat.games/v2/account/authenticate/device?create=true" `
			-Method Post `
			-Headers @{ Authorization = "Basic $basic" } `
			-ContentType "application/json" `
			-Body $body `
			-TimeoutSec 15
	} catch {
		throw "Anon auth failed: $_"
	}
	if (-not $resp.token) { throw "Anon auth response had no token" }
	Note "Anon auth smoke test OK (token issued)"
}

function Step-Reencrypt {
	if (-not (Should-Run "Reencrypt")) { return }
	Note "Step: Re-encrypt credentials.env"
	$age = (Get-ChildItem "$env:LOCALAPPDATA\Microsoft\WinGet\Packages\FiloSottile.age_*\age\age.exe" -ErrorAction Stop).FullName
	$secrets = "$HOME\Repositories\claude-config\secrets"
	$recipients = "$secrets\hopnbop-migration.recipients"
	if (-not (Test-Path $recipients)) {
		throw "Recipients file missing: $recipients"
	}
	Invoke-Checked "age encrypt" {
		& $age -R $recipients -o "$secrets\hopnbop-migration.env.age" $CredsFile | Out-Null
	}
	Note "Re-encrypted credentials.env -> claude-config (auto-push hook propagates)"
}

function Step-Complete {
	if (-not (Should-Run "Complete")) { return }
	Note "Step: Mark Phase A complete"
	$s = Read-State
	$s.phases.A.status       = "completed"
	$s.phases.A.completed_at = (Get-Date -Format 'o')
	$s.current_phase         = "B"
	# Pin the Nakama version we deployed.
	$s.infrastructure.nakama_version = "3.25.0"
	Save-State $s
	Note "Phase A complete"

	# Discord summary.
	$disc = "$HOME\Repositories\claude-config\jobs\Send-Discord.ps1"
	if (Test-Path $disc) {
		$msg = "Phase A complete. Nakama healthy at $($s.infrastructure.nakama_url). Generated secrets re-encrypted to claude-config."
		& $disc -Message $msg -JobName "Migration: Phase A"
	} else {
		Log "Send-Discord.ps1 not found at $disc; skipping Discord summary"
	}
}

# --------------------------------------------------------------------
# Main flow
# --------------------------------------------------------------------
try {
	Source-Credentials
	Note "===== Phase A start (StartAt=$StartAt) ====="
	Step-PulumiSetup
	Step-S3
	Step-PulumiLogin
	Step-Preview
	Step-Up
	Step-WaitBoot
	Step-HostKeys
	Step-Postgres
	Step-Nakama
	Step-Verify
	Step-Reencrypt
	Step-Complete
	Note "===== Phase A end ====="
} catch {
	$msg = "Phase A FAILED: $($_.Exception.Message)"
	Log $msg
	if (Test-Path $StateFile) {
		$s = Read-State
		$s.phases.A.status = "failed"
		$existing = @($s.phases.A.notes)
		$s.phases.A.notes = @($existing + "[$(Get-Date -Format 'o')] $msg")
		Save-State $s
	}
	throw
}
