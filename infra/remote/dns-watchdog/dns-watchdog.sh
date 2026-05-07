#!/bin/bash
# Hourly cleanup of stale per-deploy DNS A records.
#
# Each Edgegap game-server container creates a Cloudflare DNS
# A record `s-<ip-with-dashes>.<SERVER_DNS_BASE>` -> public IP
# at startup (see infra/game-server/entrypoint.sh in the
# consuming game's repo) and removes it via a TERM/EXIT trap.
# Edgegap can SIGKILL containers though, in which case the
# record is orphaned.
#
# This watchdog scans the zone for `s-*.<SERVER_DNS_BASE>`
# records and deletes any whose `comment` carries a
# `created=<ISO timestamp>` older than $MAX_RECORD_AGE_HOURS
# (default 4h: longer than any plausible match, shorter than a
# day so stale records don't cost us at the next cost-review).
#
# Reads /opt/snoringcat/dns-watchdog/.env for token + zone ID.
# Required env:
#   CLOUDFLARE_DNS_TOKEN     CF API token, scoped Zone:DNS:Edit
#                            on the SERVER_DNS_BASE zone.
#   CLOUDFLARE_DNS_ZONE_ID   CF zone ID for that zone.
#   SERVER_DNS_BASE          The apex (e.g. game.hopnbop.net).
# Optional:
#   MAX_RECORD_AGE_HOURS     Default 4. Records with no comment
#                            or unparseable timestamp are
#                            *kept* (we'd rather leak a record
#                            than delete a real deploy's host).

set -euo pipefail

ENV_FILE="/opt/snoringcat/dns-watchdog/.env"

[[ -f "$ENV_FILE" ]] || { echo "Missing $ENV_FILE"; exit 1; }
# shellcheck disable=SC1090
set -a; source "$ENV_FILE"; set +a

: "${CLOUDFLARE_DNS_TOKEN:?CLOUDFLARE_DNS_TOKEN not set}"
: "${CLOUDFLARE_DNS_ZONE_ID:?CLOUDFLARE_DNS_ZONE_ID not set}"
: "${SERVER_DNS_BASE:?SERVER_DNS_BASE not set (e.g. game.hopnbop.net)}"
MAX_AGE_HOURS="${MAX_RECORD_AGE_HOURS:-4}"

# Append a structured entry to SERVICE_STATUS_LOG (JSONL). Silent
# no-op when the env var isn't set; populated via phase-b.ps1's
# Step-DnsWatchdog. The daily LLM consolidator SSH-drains the file.
post_status() {
	local level="$1" summary="$2"
	local details="${3:-{\}}"
	[[ -n "${SERVICE_STATUS_LOG:-}" ]] || return 0
	local ts; ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	mkdir -p "$(dirname "$SERVICE_STATUS_LOG")"
	jq -nc \
		--arg ts "$ts" \
		--arg source "dns-watchdog" \
		--arg level "$level" \
		--arg summary "$summary" \
		--argjson details "$details" \
		'{ts: $ts, source: $source, level: $level,
			summary: $summary, details: $details}' \
		>> "$SERVICE_STATUS_LOG"
}

cutoff_epoch=$(date -u -d "${MAX_AGE_HOURS} hours ago" +%s)

# List candidate records (s-* under the configured zone).
# Cloudflare's API returns up to 100 records per page by default;
# we'd need pagination for >100, but a healthy fleet should not
# come anywhere near that. If we ever hit the cap, the
# subsequent run picks up what was missed.
list_url="https://api.cloudflare.com/client/v4/zones/${CLOUDFLARE_DNS_ZONE_ID}/dns_records"
list_url="${list_url}?type=A&name.contains=s-&per_page=100"

list_response=$(curl -fsS --max-time 15 \
	"$list_url" \
	-H "Authorization: Bearer ${CLOUDFLARE_DNS_TOKEN}")

# Filter to records actually under SERVER_DNS_BASE (the
# `name.contains=s-` query above is a substring match and
# can sweep in records under unrelated subdomains).
record_count=$(printf '%s' "$list_response" | jq -r \
	--arg suffix ".${SERVER_DNS_BASE}" \
	'[.result[] | select(.name | endswith($suffix))] | length')

echo "Scanning $record_count s-*.${SERVER_DNS_BASE} record(s); cutoff = ${cutoff_epoch} ($(date -u -d "@${cutoff_epoch}" +%FT%TZ))"

deleted=0
kept=0
unparseable=0

while IFS=$'\t' read -r record_id record_name record_comment; do
	# Extract `created=<iso>` from comment. Format set in
	# entrypoint.sh: "edgegap deploy=<id> created=<iso>".
	created_iso=$(printf '%s' "$record_comment" \
		| grep -oE 'created=[0-9TZ:-]+' \
		| head -n1 \
		| sed 's/^created=//' \
		|| true)

	if [[ -z "$created_iso" ]]; then
		# No timestamp — leave alone. Could be a manually-
		# placed record or one written by an older entrypoint.
		unparseable=$((unparseable + 1))
		continue
	fi

	created_epoch=$(date -u -d "$created_iso" +%s 2>/dev/null || echo 0)
	if (( created_epoch == 0 )); then
		# Couldn't parse — skip rather than risk deleting a
		# good record.
		unparseable=$((unparseable + 1))
		continue
	fi

	if (( created_epoch < cutoff_epoch )); then
		echo "DELETE $record_name (created $created_iso, ${record_id})"
		curl -fsS --max-time 10 -X DELETE \
			"https://api.cloudflare.com/client/v4/zones/${CLOUDFLARE_DNS_ZONE_ID}/dns_records/${record_id}" \
			-H "Authorization: Bearer ${CLOUDFLARE_DNS_TOKEN}" \
			>/dev/null \
			&& deleted=$((deleted + 1)) \
			|| echo "WARN: delete failed for $record_id"
	else
		kept=$((kept + 1))
	fi
done < <(printf '%s' "$list_response" | jq -r \
	--arg suffix ".${SERVER_DNS_BASE}" \
	'.result[]
		| select(.name | endswith($suffix))
		| [.id, .name, (.comment // "")]
		| @tsv')

echo "dns-watchdog summary: deleted=$deleted kept=$kept unparseable=$unparseable"
post_status "info" "swept $deleted stale, $kept kept, $unparseable unparseable" \
	"$(jq -nc \
		--argjson deleted "$deleted" \
		--argjson kept "$kept" \
		--argjson unparseable "$unparseable" \
		--argjson total "$record_count" \
		'{deleted: $deleted, kept: $kept,
			unparseable: $unparseable, total: $total}')"
