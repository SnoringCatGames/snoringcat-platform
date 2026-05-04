# Phase A orchestrator: provisions Nakama + Postgres on Hetzner via
# Pulumi, brings up Caddy + Nakama Docker Compose stack, and verifies
# the install end-to-end.
#
# Each step is idempotent. Rerunning the script is safe: Pulumi up is
# idempotent, S3 bucket creation is gated on existence, generated
# secrets are read from credentials.env if already present.
#
# Usage:
#   pwsh -File scripts/phase-a.ps1                # Full run
#   pwsh -File scripts/phase-a.ps1 -SkipPreview   # Skip preview gate
#   pwsh -File scripts/phase-a.ps1 -StartAt Postgres   # Resume mid-run
#
# The -StartAt step names are: PulumiSetup, S3, PulumiLogin, Preview,
# Up, WaitBoot, Postgres, Nakama, Verify, Reencrypt, Complete.

[CmdletBinding()]
param(
	[switch]$SkipPreview,
	[ValidateSet(
		"PulumiSetup", "S3", "PulumiLogin", "Preview", "Up",
		"WaitBoot", "Postgres", "Nakama", "Verify",
		"Reencrypt", "Complete"
	)]
	[string]$StartAt = "PulumiSetup",
	[ValidateSet(
		"PulumiSetup", "S3", "PulumiLogin", "Preview", "Up",
		"WaitBoot", "Postgres", "Nakama", "Verify",
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
		if ($_ -match '^([A-Z_]+)=(.*)$') {
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
	ssh -i $keyPath `
		-o StrictHostKeyChecking=accept-new `
		-o UserKnownHostsFile="$MigDir\known_hosts" `
		-o ConnectTimeout=15 `
		-o BatchMode=yes `
		"root@$ip" $cmd
}

function Scp-Up {
	param(
		[string]$ip,
		[string]$keyPath,
		[string]$src,
		[string]$dst
	)
	scp -i $keyPath `
		-o StrictHostKeyChecking=accept-new `
		-o UserKnownHostsFile="$MigDir\known_hosts" `
		$src "root@${ip}:${dst}"
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
		Start-Sleep -Seconds 20
	}
	throw "Timed out waiting for cloud-init on $label ($ip)"
}

# --------------------------------------------------------------------
# Step ordering
# --------------------------------------------------------------------
$StepOrder = @(
	"PulumiSetup", "S3", "PulumiLogin", "Preview", "Up",
	"WaitBoot", "Postgres", "Nakama", "Verify",
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
		$s = Read-State
		$s.infrastructure.hetzner_nakama_server_id    = "$($outs.nakama_server_id)"
		$s.infrastructure.hetzner_nakama_ip           = $outs.nakama_public_ip
		$s.infrastructure.hetzner_postgres_server_id  = "$($outs.postgres_server_id)"
		$s.infrastructure.hetzner_postgres_ip         = $outs.postgres_public_ip
		$s.infrastructure.hetzner_private_network_id  = "$($outs.private_network_id)"
		Save-State $s
		Note "Stack outputs persisted to state.json"
	} finally { Pop-Location }
}

function Step-WaitBoot {
	if (-not (Should-Run "WaitBoot")) { return }
	Note "Step: WaitBoot (cloud-init on both servers)"
	$s = Read-State
	Wait-CloudInit $s.infrastructure.hetzner_postgres_ip $PostgresKey "postgres-prod-1"
	Wait-CloudInit $s.infrastructure.hetzner_nakama_ip   $NakamaKey   "nakama-prod-1"
}

function Step-Postgres {
	if (-not (Should-Run "Postgres")) { return }
	Note "Step: Postgres compose up"
	Ensure-Secret "POSTGRES_PASSWORD"
	$s = Read-State
	$ip = $s.infrastructure.hetzner_postgres_ip
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
		"NAKAMA_CONSOLE_PASSWORD",
		"NAKAMA_SERVER_KEY",
		"NAKAMA_SESSION_ENCRYPTION_KEY"
	)) {
		Ensure-Secret $v
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
		"NAKAMA_GAME_VERSION"
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
	# Render .env for compose.
	$envLines = @(
		"POSTGRES_PASSWORD=$env:POSTGRES_PASSWORD",
		"POSTGRES_PRIVATE_IP=$privPg",
		"NAKAMA_CONSOLE_PASSWORD=$env:NAKAMA_CONSOLE_PASSWORD",
		"NAKAMA_SERVER_KEY=$env:NAKAMA_SERVER_KEY",
		"NAKAMA_SESSION_ENCRYPTION_KEY=$env:NAKAMA_SESSION_ENCRYPTION_KEY",
		"GOOGLE_OAUTH_CLIENT_ID=$env:GOOGLE_OAUTH_CLIENT_ID",
		"GOOGLE_OAUTH_CLIENT_SECRET=$env:GOOGLE_OAUTH_CLIENT_SECRET",
		"FACEBOOK_APP_ID=$env:FACEBOOK_APP_ID",
		"FACEBOOK_APP_SECRET=$env:FACEBOOK_APP_SECRET"
	)
	$tmpEnv = New-TemporaryFile
	($envLines -join "`n") | Out-File -Encoding ASCII -FilePath $tmpEnv.FullName
	Invoke-Checked "scp .env" {
		Scp-Up $ip $NakamaKey $tmpEnv.FullName "/opt/nakama/.env" | Out-Null
	}
	Remove-Item $tmpEnv.FullName -Force

	# Bring Caddy up first so it starts the cert flow, then Nakama.
	Invoke-Checked "compose up caddy" {
		Ssh-Run $ip $NakamaKey "cd /opt/nakama && docker compose up -d caddy" | Out-Null
	}
	Log "Sleeping 30s for Caddy ACME challenge"
	Start-Sleep -Seconds 30
	Invoke-Checked "compose up nakama" {
		Ssh-Run $ip $NakamaKey "cd /opt/nakama && docker compose up -d nakama" | Out-Null
	}
	Note "Nakama + Caddy compose up complete"
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
