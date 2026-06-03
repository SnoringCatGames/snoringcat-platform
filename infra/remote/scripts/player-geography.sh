#!/usr/bin/env bash
# Aggregate recent client_ip records by country to inform the
# hybrid allocator geo policy.
#
# Reads `client_ip / latest` records from Nakama storage,
# resolves each IP via the local geoip-sidecar (DB-IP MMDB),
# prints a country -> count summary. **IPs are not echoed** —
# only country totals — so the output is safe to paste into
# planning docs.
#
# Designed to be SSH'd onto nakama-prod-1 and run ad-hoc. The
# matchmaker writes one record per authenticated user (overwriting
# on each session), so this is a rough proxy for the unique-user
# distribution over the retention window. The runtime drops IPs
# older than 1h when matching, but storage retains them
# indefinitely; this script lets the operator pick a wider window.
#
# Usage on the host:
#   /opt/nakama/scripts/player-geography.sh             # last 30 days
#   /opt/nakama/scripts/player-geography.sh '7 days'    # last 7 days
#   /opt/nakama/scripts/player-geography.sh '1 hour'    # last hour
#
# Exit codes:
#   0  success (or empty result set)
#   1  postgres or geoip-sidecar unreachable
#   2  argument parse error

set -euo pipefail

WINDOW="${1:-30 days}"

ENV_FILE="${NAKAMA_ENV_FILE:-/opt/nakama/.env}"
if [[ ! -f "$ENV_FILE" ]]; then
	echo "missing $ENV_FILE (set NAKAMA_ENV_FILE to override)" >&2
	exit 2
fi

PG_PASS=$(grep -E "^POSTGRES_PASSWORD=" "$ENV_FILE" | cut -d= -f2-)
if [[ -z "$PG_PASS" ]]; then
	echo "POSTGRES_PASSWORD not found in $ENV_FILE" >&2
	exit 2
fi

# The sidecar is distroless (no shell), so we can't docker exec
# into it for lookups. Instead, we find its IP on the
# nakama_nakama-net bridge network and curl directly from the
# host.
SIDECAR_IP=$(docker inspect geoip-sidecar \
	--format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
	2>/dev/null || true)
if [[ -z "$SIDECAR_IP" ]]; then
	echo "geoip-sidecar container not running" >&2
	exit 1
fi
GEOIP_URL="${GEOIP_SIDECAR_URL:-http://${SIDECAR_IP}:8080}"

QUERY="SELECT (value::jsonb->>'ip')
       FROM storage
       WHERE collection = 'client_ip'
         AND key = 'latest'
         AND update_time > now() - interval '${WINDOW}';"

IPS=$(docker exec -e PGPASSWORD="$PG_PASS" postgres \
	psql -U nakama -d nakama -t -A -c "$QUERY" 2>/dev/null || true)

if [[ -z "$IPS" ]]; then
	echo "No client_ip records in the last ${WINDOW}." >&2
	exit 0
fi

declare -A COUNTRY_COUNT
TOTAL=0
ERRORS=0
while IFS= read -r ip; do
	[[ -z "$ip" ]] && continue
	TOTAL=$((TOTAL+1))
	# Hit the sidecar over the docker bridge. The sidecar's
	# own /lookup logs the IP, which is fine — this is the
	# same path the matchmaker takes.
	resp=$(curl -sf --max-time 2 \
		"${GEOIP_URL}/lookup?ip=${ip}" 2>/dev/null || true)
	cc=$(printf '%s' "$resp" | sed -nE 's/.*"country":"([^"]*)".*/\1/p')
	if [[ -z "$cc" ]]; then
		cc="?"
		ERRORS=$((ERRORS+1))
	fi
	COUNTRY_COUNT["$cc"]=$(( ${COUNTRY_COUNT["$cc"]:-0} + 1 ))
done <<< "$IPS"

echo "client_ip records in last ${WINDOW}: ${TOTAL}"
echo "geoip lookup errors: ${ERRORS}"
echo ""
echo "country  count  share"
echo "-------  -----  -----"
for cc in "${!COUNTRY_COUNT[@]}"; do
	count=${COUNTRY_COUNT[$cc]}
	pct=$(awk -v c="$count" -v t="$TOTAL" \
		'BEGIN { printf "%.1f", (c/t)*100 }')
	printf '%-7s  %5d  %4s%%\n' "$cc" "$count" "$pct"
done | sort -k2 -nr
