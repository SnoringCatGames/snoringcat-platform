#!/bin/bash
# Nightly Postgres backup → Cloudflare R2.
#
# Replaces the operational role of Hetzner snapshots in the
# stripped single-host stack: a disk failure on nakama-prod-1
# would otherwise wipe all Nakama state (users, friends,
# leaderboards, match history) since we no longer have a
# separate Postgres host. R2 is the same bucket family used by
# Pulumi state and hopnbop-assets.
#
# Reads /opt/snoringcat/pg-backup/.env for:
#   POSTGRES_PASSWORD          — DB credential (same as nakama).
#   R2_ACCESS_KEY_ID           — R2 S3-compat access key.
#   R2_SECRET_ACCESS_KEY       — R2 S3-compat secret.
#   R2_ENDPOINT                — e.g. https://<account>.r2.cloudflarestorage.com
#   R2_BUCKET                  — bucket name. Co-tenanted in
#                                `hopnbop-pulumi-state-r2` under
#                                a `pg-backups/` prefix because
#                                Cloudflare's public R2 API
#                                doesn't expose S3-compat key
#                                creation; the existing
#                                R2_ACCESS_KEY_ID is already
#                                scoped to that bucket. Different
#                                prefix keeps state and backups
#                                cleanly separated.
#   DISCORD_WEBHOOK_URL        — optional; pinged on failure only.
#   PG_BACKUP_RETENTION_DAYS   — defaults to 7.
#
# Layout in R2:
#   <bucket>/pg-backups/postgres-YYYY-MM-DD.sql.gz
#
# Daily summary in cost-monitor's Discord pings should match
# the prod-health-check skill's expectation of finding today's
# object via HEAD on $R2_BUCKET/postgres/postgres-$DATE.sql.gz.
#
# Idempotent: re-running on the same day just overwrites the
# day's object.

set -euo pipefail

ENV_FILE="/opt/snoringcat/pg-backup/.env"
[[ -f "$ENV_FILE" ]] || { echo "Missing $ENV_FILE"; exit 1; }
# shellcheck disable=SC1090
set -a; source "$ENV_FILE"; set +a

PG_BACKUP_RETENTION_DAYS="${PG_BACKUP_RETENTION_DAYS:-7}"
DATE_TODAY="$(date -u +%Y-%m-%d)"
DATE_CUTOFF="$(date -u -d "${PG_BACKUP_RETENTION_DAYS} days ago" +%Y-%m-%d)"
OBJECT_KEY="pg-backups/postgres-${DATE_TODAY}.sql.gz"
TMP_FILE="$(mktemp --suffix=.sql.gz)"
trap 'rm -f "$TMP_FILE"' EXIT

# Dump using pg_dumpall to capture roles + ACLs along with the
# `nakama` database. Run from the postgres container so we
# don't need a postgres-client install on the host.
PGPASSWORD="$POSTGRES_PASSWORD" docker exec -e PGPASSWORD postgres \
	pg_dumpall --no-password -h 127.0.0.1 -U nakama \
	| gzip -9 > "$TMP_FILE"

# Append an entry to SERVICE_STATUS_LOG (JSONL) for the daily LLM
# consolidator to pick up via SSH-drain. Silent no-op when the env
# var isn't set; the host gets it via phase-b.ps1's Step-PgBackup.
post_status() {
	local level="$1" summary="$2"
	local details="${3:-{\}}"
	[[ -n "${SERVICE_STATUS_LOG:-}" ]] || return 0
	local ts; ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	mkdir -p "$(dirname "$SERVICE_STATUS_LOG")"
	jq -nc \
		--arg ts "$ts" \
		--arg source "pg-backup" \
		--arg level "$level" \
		--arg summary "$summary" \
		--argjson details "$details" \
		'{ts: $ts, source: $source, level: $level,
			summary: $summary, details: $details}' \
		>> "$SERVICE_STATUS_LOG"
}

DUMP_SIZE_BYTES=$(stat -c%s "$TMP_FILE")
if [[ "$DUMP_SIZE_BYTES" -lt 1024 ]]; then
	# Sanity check: an empty/failed dump is much smaller than
	# this. Refuse to upload and alert; otherwise we'd
	# overwrite a good earlier-day backup with garbage. Failure
	# is an immediate-action item — keep direct Discord and ALSO
	# log a 'red' status entry for audit-trail completeness.
	msg="pg-backup: dump suspiciously small (${DUMP_SIZE_BYTES} bytes); refusing to upload"
	echo "$msg" >&2
	if [[ -n "${DISCORD_WEBHOOK_URL:-}" ]]; then
		curl -fsS -X POST -H "Content-Type: application/json" \
			-d "$(jq -n --arg c "$msg" '{content:$c}')" \
			"$DISCORD_WEBHOOK_URL" >/dev/null || true
	fi
	post_status "red" "dump too small" \
		"$(jq -nc --argjson size "$DUMP_SIZE_BYTES" \
			'{size: $size}')"
	exit 1
fi

# AWS CLI v2 is the smallest installable S3-compat client we
# can rely on Ubuntu 24.04 having (`apt install awscli`). We
# point it at R2's S3-compat endpoint via env vars set in the
# .env file.
AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" \
AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY" \
	aws s3 cp "$TMP_FILE" "s3://${R2_BUCKET}/${OBJECT_KEY}" \
	--endpoint-url "$R2_ENDPOINT" \
	--no-progress

# Retention: list everything under postgres/, drop anything
# older than the cutoff date. Pure string-compare on the date
# in the key works because YYYY-MM-DD sorts lexicographically.
AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" \
AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY" \
	aws s3 ls "s3://${R2_BUCKET}/pg-backups/" \
	--endpoint-url "$R2_ENDPOINT" \
	| awk '{print $4}' \
	| while read -r key; do
		[[ -z "$key" ]] && continue
		# Extract date portion from postgres-YYYY-MM-DD.sql.gz.
		key_date="${key#postgres-}"
		key_date="${key_date%.sql.gz}"
		if [[ "$key_date" < "$DATE_CUTOFF" ]]; then
			AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" \
			AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY" \
				aws s3 rm "s3://${R2_BUCKET}/pg-backups/${key}" \
				--endpoint-url "$R2_ENDPOINT" >/dev/null
			echo "expired: $key"
		fi
	done

post_status "info" "ok size=${DUMP_SIZE_BYTES}" \
	"$(jq -nc \
		--argjson size "$DUMP_SIZE_BYTES" \
		--arg key "$OBJECT_KEY" \
		--arg bucket "$R2_BUCKET" \
		'{size: $size, key: $key, bucket: $bucket}')"

echo "[pg-backup] $(date -u -Is) ok size=${DUMP_SIZE_BYTES} key=${OBJECT_KEY}"
