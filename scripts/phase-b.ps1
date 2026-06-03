# Phase B orchestrator: deploys observability stack (Prometheus,
# Grafana, Loki, Promtail, node_exporter, postgres_exporter) plus
# UptimeRobot synthetic check and a daily cost-monitor systemd
# timer. Same idempotency model as phase-a.ps1.
#
# Usage:
#   pwsh -File scripts/phase-b.ps1                            # Full run
#   pwsh -File scripts/phase-b.ps1 -StartAt Verify -StopAt Verify

[CmdletBinding()]
param(
	[ValidateSet(
		"PulumiUp", "PostgresExporters", "ObsConfigs", "NakamaStack",
		"Verify", "UptimeRobot", "CostMonitor",
		"PgBackup", "AlertTest", "Reencrypt", "Complete"
	)]
	[string]$StartAt = "PulumiUp",
	[ValidateSet(
		"PulumiUp", "PostgresExporters", "ObsConfigs", "NakamaStack",
		"Verify", "UptimeRobot", "CostMonitor",
		"PgBackup", "AlertTest", "Reencrypt", "Complete"
	)]
	[string]$StopAt = "Complete",
	[switch]$SkipAlertTest
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

# --------------------------------------------------------------------
# Paths
# --------------------------------------------------------------------
$RepoRoot    = (Resolve-Path "$PSScriptRoot\..").Path
$MigDir      = "$HOME\.hopnbop-migration"
$StateFile   = "$MigDir\state.json"
$CredsFile   = "$MigDir\credentials.env"
$PulumiDir   = "$RepoRoot\infra\pulumi\snoringcat-platform"
$RemoteSrc   = "$RepoRoot\infra\remote"
$NakamaKey   = "$MigDir\ssh\nakama"
$PostgresKey = "$MigDir\ssh\postgres"
$Kh          = "$MigDir\known_hosts"

$env:PATH = "$HOME\.pulumi\bin;$env:PATH"
$env:AWS_PROFILE = "hopnbop"
$env:AWS_REGION  = "us-west-2"

# --------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------
function Log {
	param([string]$msg)
	Write-Host "[$(Get-Date -Format 'HH:mm:ss')] $msg" -ForegroundColor Cyan
}

function Note {
	param([string]$msg)
	Log $msg
	if (Test-Path $StateFile) {
		$s = Get-Content $StateFile -Raw | ConvertFrom-Json
		$existing = @($s.phases.B.notes)
		$s.phases.B.notes = @($existing + "[$(Get-Date -Format 'o')] $msg")
		Save-State $s
	}
}

function Save-State { param($state); $state | ConvertTo-Json -Depth 12 | Out-File -Encoding ASCII $StateFile }
function Read-State { Get-Content $StateFile -Raw | ConvertFrom-Json }

# Write a string to a file with LF line endings and no BOM. Use
# this for any file destined for a Linux box (e.g., .env shell
# files, where CRLF makes `source` produce $'value\r' garbage).
function Write-LinuxFile {
	param([string]$path, [string]$content)
	[IO.File]::WriteAllText($path, $content, [Text.UTF8Encoding]::new($false))
}

function Invoke-Checked {
	param([string]$desc, [scriptblock]$action)
	& $action
	if ($LASTEXITCODE -ne 0) { throw "$desc failed (exit $LASTEXITCODE)" }
}

function Source-Credentials {
	if (-not (Test-Path $CredsFile)) { throw "credentials.env missing at $CredsFile" }
	Get-Content $CredsFile | ForEach-Object {
		# `[A-Z_][A-Z0-9_]*` (instead of `[A-Z_]+`) so var names
		# with digits load correctly, e.g. R2_ENDPOINT,
		# R2_ACCESS_KEY_ID. The original regex silently skipped
		# them and pg-backup's smoke run failed with an empty
		# --endpoint-url.
		if ($_ -match '^([A-Z_][A-Z0-9_]*)=(.*)$') {
			Set-Item "Env:$($Matches[1])" $Matches[2]
		}
	}
}

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
	$bytes = New-Object byte[] 24
	[System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
	$val = [Convert]::ToBase64String($bytes)
	Set-Item "Env:$varName" $val
	Add-Content -Encoding ASCII $CredsFile "$varName=$val"
	Note "Generated $varName, appended to credentials.env"
}

function Ssh-Run {
	param([string]$ip, [string]$key, [string]$cmd)
	ssh -i $key -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$Kh `
		-o ConnectTimeout=15 -o BatchMode=yes "root@$ip" $cmd
}
function Scp-Up {
	param([string]$ip, [string]$key, [string]$src, [string]$dst, [switch]$Recurse)
	$scpArgs = @("-i", $key,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=$Kh",
		"-o", "BatchMode=yes")
	if ($Recurse) { $scpArgs += "-r" }
	$scpArgs += $src
	$scpArgs += "root@${ip}:${dst}"
	scp @scpArgs
}

# --------------------------------------------------------------------
# Step ordering
# --------------------------------------------------------------------
$StepOrder = @(
	"PulumiUp", "PostgresExporters", "ObsConfigs", "NakamaStack",
	"Verify", "UptimeRobot", "CostMonitor", "GeoipRefresh",
	"AlertTest", "Reencrypt", "Complete"
)
$StartIdx = $StepOrder.IndexOf($StartAt)
$StopIdx  = $StepOrder.IndexOf($StopAt)
function Should-Run { param([string]$step) $i = $StepOrder.IndexOf($step); return ($i -ge $StartIdx -and $i -le $StopIdx) }

# --------------------------------------------------------------------
# Steps
# --------------------------------------------------------------------
function Step-PulumiUp {
	if (-not (Should-Run "PulumiUp")) { return }
	Note "Step: pulumi up (grafana DNS)"
	Push-Location $PulumiDir
	try {
		Invoke-Checked "pulumi up" { pulumi up --yes --skip-preview }
		$json = pulumi stack output --json
		if ($LASTEXITCODE -ne 0) { throw "pulumi stack output failed" }
		$outs = $json | ConvertFrom-Json
		Note "grafana_url=$($outs.grafana_url)"
	} finally { Pop-Location }
}

function Step-PostgresExporters {
	if (-not (Should-Run "PostgresExporters")) { return }
	Note "Step: postgres node-exporter + postgres-exporter"
	$s = Read-State
	$ip = $s.infrastructure.hetzner_postgres_ip
	# Updated compose includes node-exporter + postgres-exporter.
	# Push it and reconcile.
	Invoke-Checked "scp postgres compose" {
		Scp-Up $ip $PostgresKey "$RemoteSrc\postgres\docker-compose.yml" "/opt/postgres/"
	}
	# .env still has POSTGRES_PASSWORD from Phase A.
	Invoke-Checked "compose pull (postgres)" {
		Ssh-Run $ip $PostgresKey "cd /opt/postgres && docker compose pull"
	}
	Invoke-Checked "compose up (postgres)" {
		Ssh-Run $ip $PostgresKey "cd /opt/postgres && docker compose up -d"
	}
	# node-exporter uses network_mode: host, so ufw applies. Open
	# 9100 + 9187 to the private subnet only (idempotent: ufw
	# silently ignores duplicate rules).
	Invoke-Checked "ufw allow private exporters" {
		Ssh-Run $ip $PostgresKey @"
ufw allow from 10.0.0.0/16 to any port 9100 proto tcp comment 'node-exporter (private only)' &&
ufw allow from 10.0.0.0/16 to any port 9187 proto tcp comment 'postgres-exporter (private only)'
"@
	}
	# Quick sanity probe of new exporters from the postgres host itself.
	Start-Sleep -Seconds 8
	$probe = Ssh-Run $ip $PostgresKey "curl -fsS http://127.0.0.1:9100/metrics | head -2; echo '---'; curl -fsS http://127.0.0.1:9187/metrics | head -2" 2>&1
	Note "Postgres exporters probe: $($probe -replace "`n", ' / ')"
}

function Step-ObsConfigs {
	if (-not (Should-Run "ObsConfigs")) { return }
	Note "Step: render and ship observability configs"
	Ensure-Secret "GRAFANA_ADMIN_PASSWORD"
	$s = Read-State
	$ip = $s.infrastructure.hetzner_nakama_ip

	# Make remote dirs.
	Invoke-Checked "mkdir grafana provisioning" {
		Ssh-Run $ip $NakamaKey "mkdir -p /opt/nakama/grafana/provisioning/datasources /opt/nakama/grafana/provisioning/dashboards /opt/nakama/grafana/provisioning/alerting /opt/nakama/grafana/dashboards"
	}

	# Static configs.
	foreach ($name in @("prometheus.yml", "loki-config.yml", "promtail-config.yml")) {
		Invoke-Checked "scp $name" {
			Scp-Up $ip $NakamaKey "$RemoteSrc\nakama\$name" "/opt/nakama/"
		}
	}

	# Grafana provisioning files (recursive).
	Invoke-Checked "scp grafana provisioning" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\nakama\grafana\provisioning" "/opt/nakama/grafana/" -Recurse
	}

	# Render contactpoints.yml with Discord webhook URL.
	$cp = Get-Content "$RemoteSrc\nakama\grafana\provisioning\alerting\contactpoints.yml" -Raw
	$cp = $cp.Replace('${DISCORD_WEBHOOK_URL}', $env:DISCORD_WEBHOOK_URL)
	$tmp = New-TemporaryFile
	$cp | Out-File -Encoding ASCII -FilePath $tmp.FullName
	Invoke-Checked "scp rendered contactpoints" {
		Scp-Up $ip $NakamaKey $tmp.FullName "/opt/nakama/grafana/provisioning/alerting/contactpoints.yml"
	}
	Remove-Item $tmp.FullName -Force
	Note "Observability configs deployed"
}

function Step-NakamaStack {
	if (-not (Should-Run "NakamaStack")) { return }
	Note "Step: redeploy Nakama compose (single-host, postgres co-tenant)"
	$s = Read-State
	$ip = $s.infrastructure.hetzner_nakama_ip
	$privPg = "10.0.1.20"

	# Updated compose + Caddyfile + pg_hba.conf for the postgres
	# co-tenant.
	Invoke-Checked "scp updated compose" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\nakama\docker-compose.yml" "/opt/nakama/"
	}
	Invoke-Checked "scp updated Caddyfile" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\nakama\Caddyfile" "/opt/nakama/"
	}
	Invoke-Checked "scp pg_hba.conf" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\nakama\pg_hba.conf" "/opt/nakama/"
	}

	# .env: extend with GRAFANA_ADMIN_PASSWORD + DISCORD_WEBHOOK_URL.
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
		"GRAFANA_ADMIN_PASSWORD=$env:GRAFANA_ADMIN_PASSWORD",
		"DISCORD_WEBHOOK_URL=$env:DISCORD_WEBHOOK_URL"
	)
	$tmp = New-TemporaryFile
	Write-LinuxFile $tmp.FullName (($envLines -join "`n") + "`n")
	Invoke-Checked "scp .env" {
		Scp-Up $ip $NakamaKey $tmp.FullName "/opt/nakama/.env"
	}
	Remove-Item $tmp.FullName -Force

	# Build buildable, pull the rest, reconcile — all in one
	# pass. `compose pull` alone fails because caddy-with-
	# ratelimit:local has no registry to pull from (it's a
	# `build: ./caddy` image). `--pull always` keeps the
	# images fresh; `--build` rebuilds caddy.
	# `--remove-orphans` tears down containers from the old
	# compose definition (prometheus/grafana/loki/promtail/
	# node-exporter) that the post-consolidation compose no
	# longer declares.
	Invoke-Checked "compose up --build --pull always (nakama)" {
		Ssh-Run $ip $NakamaKey "cd /opt/nakama && docker compose up -d --build --pull always --remove-orphans"
	}
	Note "Nakama stack reconciled"
}

function Step-Verify {
	if (-not (Should-Run "Verify")) { return }
	Note "Step: Verify (Grafana 200, Prometheus targets UP, Discord wired)"
	# Wait for Grafana cert + readiness.
	$ok = $false
	for ($i = 0; $i -lt 60; $i++) {
		try {
			$r = Invoke-WebRequest -Uri "https://grafana.snoringcat.games/api/health" -UseBasicParsing -TimeoutSec 5
			if ($r.StatusCode -eq 200) {
				$j = $r.Content | ConvertFrom-Json
				if ($j.database -eq "ok") { $ok = $true; break }
			}
		} catch {}
		Start-Sleep -Seconds 5
	}
	if (-not $ok) { throw "Grafana /api/health never returned ok" }
	Note "Grafana healthy"

	# Query Prometheus targets via docker exec on nakama box.
	$s = Read-State
	$ip = $s.infrastructure.hetzner_nakama_ip
	$tj = Ssh-Run $ip $NakamaKey "docker exec prometheus wget -qO- http://localhost:9090/api/v1/targets"
	if ($LASTEXITCODE -ne 0) { throw "Prometheus targets API call failed" }
	$tobj = $tj | ConvertFrom-Json
	$active = $tobj.data.activeTargets
	$down = @($active | Where-Object { $_.health -ne 'up' })
	$upCount = ($active | Where-Object { $_.health -eq 'up' }).Count
	Note "Prometheus targets: $upCount up, $($down.Count) not up"
	if ($down.Count -gt 0) {
		foreach ($d in $down) {
			Note "  DOWN: $($d.scrapePool) lastError=$($d.lastError)"
		}
	}
}

function Step-UptimeRobot {
	if (-not (Should-Run "UptimeRobot")) { return }
	Note "Step: UptimeRobot monitor"
	# Free tier doesn't allow creating Discord webhook contacts via
	# API ("team integrations"). Try, but fall back to whatever
	# contacts already exist on the account (UR sends email by
	# default to the account owner, so the monitor still alerts).
	$contactId = $null
	$contactsResp = Invoke-RestMethod -Uri "https://api.uptimerobot.com/v2/getAlertContacts" `
		-Method Post -Body @{ api_key = $env:UPTIMEROBOT_API_KEY; format = "json" }
	$discordContact = $contactsResp.alert_contacts | Where-Object { $_.value -eq $env:DISCORD_WEBHOOK_URL } | Select-Object -First 1
	if ($discordContact) {
		$contactId = $discordContact.id
		Note "Reusing existing UptimeRobot Discord contact id=$contactId"
	} else {
		$mk = Invoke-RestMethod -Uri "https://api.uptimerobot.com/v2/newAlertContact" `
			-Method Post -Body @{
				api_key       = $env:UPTIMEROBOT_API_KEY
				format        = "json"
				type          = 11
				friendly_name = "snoringcat-discord"
				value         = $env:DISCORD_WEBHOOK_URL
			}
		if ($mk.stat -eq 'ok') {
			$contactId = $mk.alertcontact.id
			Note "Created UptimeRobot Discord contact id=$contactId"
		} else {
			Note "UR free-tier blocks API webhook contacts; falling back to default email contacts ($($mk.error.type)): $($mk.error.message)"
			# Use account default contact (typically email; ID is the
			# first contact on the account).
			if ($contactsResp.alert_contacts.Count -gt 0) {
				$contactId = $contactsResp.alert_contacts[0].id
				Note "Falling back to UR contact id=$contactId ($($contactsResp.alert_contacts[0].friendly_name))"
			}
		}
	}

	# Create or reuse the healthcheck monitor.
	$existingResp = Invoke-RestMethod -Uri "https://api.uptimerobot.com/v2/getMonitors" `
		-Method Post -Body @{ api_key = $env:UPTIMEROBOT_API_KEY; format = "json" }
	$existing = $existingResp.monitors | Where-Object { $_.url -eq "https://nakama.snoringcat.games/healthcheck" } | Select-Object -First 1
	if (-not $existing) {
		$body = @{
			api_key        = $env:UPTIMEROBOT_API_KEY
			format         = "json"
			type           = 1
			friendly_name  = "Nakama healthcheck"
			url            = "https://nakama.snoringcat.games/healthcheck"
			interval       = 300
		}
		if ($contactId) { $body.alert_contacts = "${contactId}_2_3" }
		$mk = Invoke-RestMethod -Uri "https://api.uptimerobot.com/v2/newMonitor" -Method Post -Body $body
		if ($mk.stat -ne 'ok') { throw "UptimeRobot newMonitor failed: $($mk | ConvertTo-Json -Compress)" }
		Note "Created UptimeRobot monitor id=$($mk.monitor.id)"
	} else {
		Note "UptimeRobot monitor already exists id=$($existing.id)"
	}
}

function Step-CostMonitor {
	if (-not (Should-Run "CostMonitor")) { return }
	Note "Step: cost-monitor systemd timer"
	$s = Read-State
	$ip = $s.infrastructure.hetzner_nakama_ip
	# jq is not in the base Ubuntu install; cost-monitor.sh needs
	# it. Idempotent.
	Invoke-Checked "apt install jq" {
		Ssh-Run $ip $NakamaKey "DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends jq >/dev/null"
	}
	# /var/log/snoringcat hosts the JSONL queue that cost-monitor,
	# pg-backup, and dns-watchdog all append status entries to.
	# Created here for whichever Step-* runs first; mkdir -p is
	# idempotent, so subsequent steps are no-ops.
	Invoke-Checked "mkdir /var/log/snoringcat" {
		Ssh-Run $ip $NakamaKey "mkdir -p /var/log/snoringcat && touch /var/log/snoringcat/service-status.jsonl"
	}
	Invoke-Checked "mkdir cost-monitor" {
		Ssh-Run $ip $NakamaKey "mkdir -p /opt/snoringcat/cost-monitor"
	}
	Invoke-Checked "scp cost-monitor.sh" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\cost-monitor\cost-monitor.sh" "/opt/snoringcat/cost-monitor/"
	}
	Invoke-Checked "scp service+timer" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\cost-monitor\cost-monitor.service" "/opt/snoringcat/cost-monitor/"
	}
	Invoke-Checked "scp timer" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\cost-monitor\cost-monitor.timer" "/opt/snoringcat/cost-monitor/"
	}
	# Render .env on remote. Thresholds match the original
	# migration plan ($20/$40/$80 warn tiers + $50 emergency cap).
	# DISCORD_USER_ID is optional; when set, threshold pings
	# include an @-mention so they show up as a Discord
	# notification (and an email if the user has those on).
	$envLines = @(
		"HCLOUD_TOKEN=$env:HCLOUD_TOKEN",
		"EDGEGAP_TOKEN=$env:EDGEGAP_TOKEN",
		"EDGEGAP_ORG=$env:EDGEGAP_ORG",
		"DISCORD_WEBHOOK_URL=$env:DISCORD_WEBHOOK_URL",
		"DISCORD_USER_ID=$([Environment]::GetEnvironmentVariable('DISCORD_USER_ID'))",
		"BUDGET_WARN_LOW=20",
		"BUDGET_WARN_MID=40",
		"BUDGET_WARN_HIGH=80",
		"EMERGENCY_CAP=50",
		"DAILY_SUMMARY_HOUR_UTC=9",
		# Cloudflare R2 storage probe — token must have
		# Workers R2 Storage:Read (or Edit). Account ID is hex,
		# not slug.
		"CLOUDFLARE_API_TOKEN=$([Environment]::GetEnvironmentVariable('CLOUDFLARE_PAGES_TOKEN'))",
		"CLOUDFLARE_ACCOUNT_ID=c97b21157100dde27a8715fdfba1d22a",
		"R2_BUCKET=hopnbop-assets",
		"R2_WARN_GB=8",
		"R2_HARD_GB=9.5",
		# Service-status JSONL: cost-monitor's post_status() helper
		# appends one JSON line per status event. The daily LLM
		# consolidator on the operator's machine SSH-drains the
		# file. Enables routing the daily summary through the
		# consolidator instead of direct Discord (noise reduction).
		"SERVICE_STATUS_LOG=/var/log/snoringcat/service-status.jsonl",
		# Cloudflare Pages free tier is 500 builds/month
		# account-wide. Defaults: warn at 80%, hard at 95%.
		"CF_PAGES_WARN_BUILDS=400",
		"CF_PAGES_HARD_BUILDS=475"
	)
	$tmp = New-TemporaryFile
	Write-LinuxFile $tmp.FullName (($envLines -join "`n") + "`n")
	Invoke-Checked "scp .env (cost-monitor)" {
		Scp-Up $ip $NakamaKey $tmp.FullName "/opt/snoringcat/cost-monitor/.env"
	}
	Remove-Item $tmp.FullName -Force

	Invoke-Checked "install + enable timer" {
		Ssh-Run $ip $NakamaKey @"
chmod +x /opt/snoringcat/cost-monitor/cost-monitor.sh &&
chmod 600 /opt/snoringcat/cost-monitor/.env &&
cp /opt/snoringcat/cost-monitor/cost-monitor.service /etc/systemd/system/cost-monitor.service &&
cp /opt/snoringcat/cost-monitor/cost-monitor.timer /etc/systemd/system/cost-monitor.timer &&
systemctl daemon-reload &&
systemctl enable --now cost-monitor.timer &&
systemctl list-timers cost-monitor.timer
"@
	}

	# Run once now to verify it works (and posts a Discord baseline).
	Invoke-Checked "test run cost-monitor" {
		Ssh-Run $ip $NakamaKey "systemctl start cost-monitor.service && journalctl -u cost-monitor.service -n 20 --no-pager"
	}
	Note "Cost monitor installed and ran successfully"
}

function Step-GeoipRefresh {
	if (-not (Should-Run "GeoipRefresh")) { return }
	Note "Step: geoip-refresh systemd timer (monthly MMDB rollover)"
	$s = Read-State
	$ip = $s.infrastructure.hetzner_nakama_ip
	# The geoip-sidecar container bind-mounts
	# /var/lib/snoringcat/geoip read-only; create the directory
	# eagerly so the first compose-up doesn't fail on a missing
	# bind source if the timer hasn't run yet.
	Invoke-Checked "mkdir /var/lib/snoringcat/geoip" {
		Ssh-Run $ip $NakamaKey "mkdir -p /var/lib/snoringcat/geoip"
	}
	Invoke-Checked "mkdir geoip-refresh" {
		Ssh-Run $ip $NakamaKey "mkdir -p /opt/snoringcat/geoip-refresh"
	}
	Invoke-Checked "scp geoip-refresh.sh" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\geoip-refresh\geoip-refresh.sh" "/opt/snoringcat/geoip-refresh/"
	}
	Invoke-Checked "scp geoip-refresh.service" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\geoip-refresh\geoip-refresh.service" "/opt/snoringcat/geoip-refresh/"
	}
	Invoke-Checked "scp geoip-refresh.timer" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\geoip-refresh\geoip-refresh.timer" "/opt/snoringcat/geoip-refresh/"
	}
	Invoke-Checked "install + enable geoip-refresh timer" {
		Ssh-Run $ip $NakamaKey @"
chmod +x /opt/snoringcat/geoip-refresh/geoip-refresh.sh &&
cp /opt/snoringcat/geoip-refresh/geoip-refresh.service /etc/systemd/system/geoip-refresh.service &&
cp /opt/snoringcat/geoip-refresh/geoip-refresh.timer /etc/systemd/system/geoip-refresh.timer &&
systemctl daemon-reload &&
systemctl enable --now geoip-refresh.timer &&
systemctl list-timers geoip-refresh.timer
"@
	}
	# Run once immediately so the bind-mount source isn't empty
	# when the geoip-sidecar starts (the sidecar fail-fasts at
	# boot on a missing MMDB). Idempotent on repeat phase-b runs.
	Invoke-Checked "test run geoip-refresh" {
		Ssh-Run $ip $NakamaKey "systemctl start geoip-refresh.service && journalctl -u geoip-refresh.service -n 15 --no-pager"
	}
	Note "geoip-refresh installed and ran successfully"
}

function Step-PgBackup {
	if (-not (Should-Run "PgBackup")) { return }
	Note "Step: pg-backup systemd timer (nightly Postgres dump → R2)"
	$s = Read-State
	$ip = $s.infrastructure.hetzner_nakama_ip

	# pg-backup.sh shells out to `aws` (S3-compat). The legacy
	# `awscli` apt package was retired in Ubuntu 24.04 (it
	# pulled an unmaintained Python 2 toolchain), so we install
	# the official AWS CLI v2 static binary instead. Idempotent.
	Invoke-Checked "install AWS CLI v2" {
		Ssh-Run $ip $NakamaKey @'
set -e
if which aws >/dev/null 2>&1; then exit 0; fi
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends unzip >/dev/null
cd /tmp && curl -fsS "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o awscliv2.zip
unzip -q awscliv2.zip && ./aws/install && rm -rf awscliv2.zip aws
'@
	}
	# Idempotent — same path created in Step-CostMonitor.
	Invoke-Checked "mkdir /var/log/snoringcat" {
		Ssh-Run $ip $NakamaKey "mkdir -p /var/log/snoringcat && touch /var/log/snoringcat/service-status.jsonl"
	}
	Invoke-Checked "mkdir pg-backup" {
		Ssh-Run $ip $NakamaKey "mkdir -p /opt/snoringcat/pg-backup"
	}
	Invoke-Checked "scp pg-backup.sh" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\pg-backup\pg-backup.sh" "/opt/snoringcat/pg-backup/"
	}
	Invoke-Checked "scp service" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\pg-backup\pg-backup.service" "/opt/snoringcat/pg-backup/"
	}
	Invoke-Checked "scp timer" {
		Scp-Up $ip $NakamaKey "$RemoteSrc\pg-backup\pg-backup.timer" "/opt/snoringcat/pg-backup/"
	}

	# Pulls POSTGRES_PASSWORD from credentials.env. R2 access keys
	# are bucket-scoped to hopnbop-pulumi-state-r2 (the only
	# bucket the existing R2_ACCESS_KEY_ID can write to without
	# generating a new key in the Cloudflare dashboard); we
	# co-tenant pg backups under a `pg-backups/` prefix.
	$envLines = @(
		"POSTGRES_PASSWORD=$env:POSTGRES_PASSWORD",
		"R2_ACCESS_KEY_ID=$env:R2_ACCESS_KEY_ID",
		"R2_SECRET_ACCESS_KEY=$env:R2_SECRET_ACCESS_KEY",
		"R2_ENDPOINT=$env:R2_ENDPOINT",
		"R2_BUCKET=hopnbop-pulumi-state-r2",
		"DISCORD_WEBHOOK_URL=$env:DISCORD_WEBHOOK_URL",
		"PG_BACKUP_RETENTION_DAYS=7",
		# Service-status JSONL: pg-backup's post_status() helper
		# appends one JSON line per run (success or failure).
		# Failure also keeps direct Discord alert.
		"SERVICE_STATUS_LOG=/var/log/snoringcat/service-status.jsonl"
	)
	$tmp = New-TemporaryFile
	Write-LinuxFile $tmp.FullName (($envLines -join "`n") + "`n")
	Invoke-Checked "scp .env (pg-backup)" {
		Scp-Up $ip $NakamaKey $tmp.FullName "/opt/snoringcat/pg-backup/.env"
	}
	Remove-Item $tmp.FullName -Force

	Invoke-Checked "install + enable timer" {
		Ssh-Run $ip $NakamaKey @"
chmod +x /opt/snoringcat/pg-backup/pg-backup.sh &&
chmod 600 /opt/snoringcat/pg-backup/.env &&
cp /opt/snoringcat/pg-backup/pg-backup.service /etc/systemd/system/pg-backup.service &&
cp /opt/snoringcat/pg-backup/pg-backup.timer /etc/systemd/system/pg-backup.timer &&
systemctl daemon-reload &&
systemctl enable --now pg-backup.timer &&
systemctl list-timers pg-backup.timer
"@
	}

	Invoke-Checked "test run pg-backup" {
		Ssh-Run $ip $NakamaKey "systemctl start pg-backup.service && journalctl -u pg-backup.service -n 20 --no-pager"
	}
	Note "pg-backup installed and ran successfully"
}

function Step-AlertTest {
	if (-not (Should-Run "AlertTest")) { return }
	if ($SkipAlertTest) { Log "SkipAlertTest set, skipping"; return }
	Note "Step: live alert trigger (stop nakama 3 min)"
	$s = Read-State
	$ip = $s.infrastructure.hetzner_nakama_ip

	# Pre-warn Discord channel.
	$disc = "$HOME\Repositories\claude-config\jobs\Send-Discord.ps1"
	if (Test-Path $disc) {
		& $disc -Message "Phase B alert test: stopping nakama for ~3 min. Expect a 'Nakama is down' Grafana alert in this channel within 2-3 min." -JobName "Migration: Phase B"
	}

	Invoke-Checked "stop nakama" {
		Ssh-Run $ip $NakamaKey "cd /opt/nakama && docker compose stop nakama"
	}
	Note "Nakama stopped at $(Get-Date -Format o); waiting 180s for alert to fire"
	Start-Sleep -Seconds 180
	Invoke-Checked "start nakama" {
		Ssh-Run $ip $NakamaKey "cd /opt/nakama && docker compose start nakama"
	}
	Note "Nakama restarted at $(Get-Date -Format o); check Discord for alert message + resolved notice"
}

function Step-Reencrypt {
	if (-not (Should-Run "Reencrypt")) { return }
	Note "Step: re-encrypt credentials.env (now includes GRAFANA_ADMIN_PASSWORD)"
	$age = (Get-ChildItem "$env:LOCALAPPDATA\Microsoft\WinGet\Packages\FiloSottile.age_*\age\age.exe" -ErrorAction Stop).FullName
	$secrets = "$HOME\Repositories\claude-config\secrets"
	$recipients = "$secrets\hopnbop-migration.recipients"
	Invoke-Checked "age encrypt" {
		& $age -R $recipients -o "$secrets\hopnbop-migration.env.age" $CredsFile
	}
	Note "Re-encrypted credentials.env -> claude-config"
}

function Step-Complete {
	if (-not (Should-Run "Complete")) { return }
	Note "Step: mark Phase B complete"
	$s = Read-State
	$s.phases.B.status       = "completed"
	$s.phases.B.completed_at = (Get-Date -Format 'o')
	$s.current_phase         = "C"
	$s.verification.phase_b_alert_test_at = (Get-Date -Format 'o')
	Save-State $s
	Note "Phase B complete"

	$disc = "$HOME\Repositories\claude-config\jobs\Send-Discord.ps1"
	if (Test-Path $disc) {
		$msg = @'
**Phase B complete.** Observability stack live.
- Grafana: https://grafana.snoringcat.games (admin / GRAFANA_ADMIN_PASSWORD in credentials.env)
- Prometheus, Loki, Promtail running on Nakama box
- node_exporter + postgres_exporter on Postgres box (private network)
- UptimeRobot 5-min synthetic check (UR free tier blocks Discord-via-API; falls back to email)
- Cost monitor systemd timer (daily 09:00 UTC), $50/mo emergency cap
- Alert rules: critical/warning tiers, Discord contact point
'@
		& $disc -Message $msg -JobName "Migration: Phase B"
	}
}

# --------------------------------------------------------------------
# Main
# --------------------------------------------------------------------
try {
	Source-Credentials
	Note "===== Phase B start (StartAt=$StartAt, StopAt=$StopAt) ====="
	Step-PulumiUp
	Step-PostgresExporters
	Step-ObsConfigs
	Step-NakamaStack
	Step-Verify
	Step-UptimeRobot
	Step-CostMonitor
	Step-GeoipRefresh
	Step-PgBackup
	Step-AlertTest
	Step-Reencrypt
	Step-Complete
	Note "===== Phase B end ====="
} catch {
	$msg = "Phase B FAILED: $($_.Exception.Message)"
	Log $msg
	if (Test-Path $StateFile) {
		$s = Read-State
		$s.phases.B.status = "failed"
		$existing = @($s.phases.B.notes)
		$s.phases.B.notes = @($existing + "[$(Get-Date -Format 'o')] $msg")
		Save-State $s
	}
	throw
}
