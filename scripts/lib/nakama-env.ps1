# Renders /opt/nakama/.env, which docker compose reads to fill every
# ${VAR} in infra/remote/nakama/docker-compose.yml. phase-a (first
# boot) and phase-b (the NakamaStack redeploy) both write the whole
# file, so both dot-source this and render it here.
#
# Until 2026-10-11 each script kept its own hand list, and the live
# file had been completed by hand on the host. A script-built host got
# no Edgegap, signaling-domain, or local-allocator entries, so it came
# up with matchmaking off. phase-b's rewrite dropped
# SIGNALING_HMAC_SECRET. phase-a's lacked GRAFANA_ADMIN_PASSWORD, so a
# rebuilt Grafana initialized its fresh database with the default
# admin password, which a later change to .env never resets.
#
# Get-NakamaEnvLines checks its keys against the compose file in both
# directions. A new ${VAR} in docker-compose.yml fails the next phase-a
# or phase-b run here, instead of reaching the host as a blank.
#
# Expects credentials.env already loaded into the process env.

# Static: the Pulumi DNS record `signaling-a` in main.go. Override via
# the env if the platform ever moves to another zone.
$DefaultSignalingDomain = "signaling.snoringcat.games"

# Keys docker-compose.yml reads that prod deliberately leaves blank.
$NakamaEnvBlank = @(
	# Compliance-test mode: the runtime synthesizes deploys instead of
	# allocating real servers. Must stay off in prod.
	"EDGEGAP_MOCK_DEPLOY",
	# Vestigial version_check fallback for calls without a game_id.
	# Every client passes one, and the per-game config wins (hopnbop's
	# CLAUDE.md, "Version Management").
	"NAKAMA_GAME_VERSION",
	"NAKAMA_PROTOCOL_VERSION",
	# Gates the retired Phase E bulk-import RPC.
	"BULK_IMPORT_ENABLED",
	# Nothing in the runtime reads these. cost-monitor has its own .env.
	"GITHUB_ORG",
	"GITHUB_TOKEN"
)

function Get-NakamaEnvLines {
	param(
		# The host's public IPv4. The local Docker allocator hands it to
		# clients as the game-server address.
		[Parameter(Mandatory)][string]$PublicIp,
		[Parameter(Mandatory)][string]$ComposeFile
	)
	$values = [ordered]@{}
	foreach ($name in @(
		"POSTGRES_PASSWORD",
		"NAKAMA_CONSOLE_PASSWORD",
		"NAKAMA_SERVER_KEY",
		"NAKAMA_SESSION_ENCRYPTION_KEY",
		"NAKAMA_REFRESH_ENCRYPTION_KEY",
		"NAKAMA_HTTP_KEY",
		"NAKAMA_CONSOLE_SIGNING_KEY",
		"GOOGLE_OAUTH_CLIENT_ID",
		"GOOGLE_OAUTH_CLIENT_SECRET",
		"FACEBOOK_APP_ID",
		"FACEBOOK_APP_SECRET",
		# Shared by the signaling-proxy service and the runtime hook
		# that signs the URLs the proxy verifies. Must match exactly.
		"SIGNALING_HMAC_SECRET",
		"GRAFANA_ADMIN_PASSWORD",
		"DISCORD_WEBHOOK_URL",
		"EDGEGAP_TOKEN",
		# Bootstrap fallbacks: a registered game's per-game config
		# wins, so don't bump these on deploys. They still have to be
		# set, because the runtime refuses to load when EDGEGAP_TOKEN
		# is set and either one is blank.
		"EDGEGAP_APP_NAME",
		"EDGEGAP_APP_VERSION"
	)) {
		$values[$name] = [Environment]::GetEnvironmentVariable($name)
	}
	$values["SIGNALING_DOMAIN"] = if ($env:SIGNALING_DOMAIN) {
		$env:SIGNALING_DOMAIN
	} else {
		$DefaultSignalingDomain
	}
	# Local Docker allocator, for games with allocator_mode local or
	# hybrid. The port ranges must match the Hetzner firewall rules in
	# infra/pulumi/snoringcat-platform/main.go.
	$values["LOCAL_PUBLIC_IP"] = $PublicIp
	$values["LOCAL_UDP_PORT_RANGE"] = "30000-30099"
	$values["LOCAL_TCP_PORT_RANGE"] = "30100-30199"
	foreach ($name in $NakamaEnvBlank) {
		$values[$name] = ""
	}

	$empty = @($values.Keys | Where-Object {
		$NakamaEnvBlank -notcontains $_ -and -not $values[$_]
	})
	if ($empty.Count -gt 0) {
		throw ("/opt/nakama/.env would leave these blank: " +
			($empty -join ", ") + ". Add them to credentials.env.")
	}

	# Compose never interpolates comments (the file's own comments
	# mention `${VAR}`), so strip them first. `$${...}` is compose's
	# escape for a literal `${...}`, so skip that too.
	$compose = (Get-Content $ComposeFile |
		ForEach-Object { $_ -replace '(^|\s)#.*$', '' }) -join "`n"
	$referenced = @([regex]::Matches(
		$compose, '(?<!\$)\$\{([A-Za-z_][A-Za-z0-9_]*)') |
		ForEach-Object { $_.Groups[1].Value } |
		Sort-Object -Unique)
	$missing = @($referenced | Where-Object { -not $values.Contains($_) })
	$unread = @($values.Keys | Where-Object { $referenced -notcontains $_ })
	if ($missing.Count -gt 0 -or $unread.Count -gt 0) {
		throw ("Get-NakamaEnvLines is out of step with " +
			"docker-compose.yml. Read by compose but not rendered: [" +
			($missing -join ", ") + "]. Rendered but never read: [" +
			($unread -join ", ") + "].")
	}
	return @($values.Keys | ForEach-Object { "$_=$($values[$_])" })
}
