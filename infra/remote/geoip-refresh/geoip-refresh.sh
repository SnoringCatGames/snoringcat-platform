#!/bin/bash
# Monthly GeoIP MMDB refresh for the snoringcat-platform Nakama
# runtime's hybridAllocatorChoice.
#
# Source: DB-IP IP-to-Country Lite, MMDB format, MIT-licensed,
# no signup required. New file published on the 1st of each
# month at:
#   https://download.db-ip.com/free/dbip-country-lite-<YYYY>-<MM>.mmdb.gz
#
# Pulls the current month's file, gunzips, atomically swaps
# into place so the Nakama runtime (which keeps an mmap'd
# reader open) sees a consistent file. The runtime's
# initGeoIPFromEnv reads from the same path at process start;
# a future enhancement would inotify the file for hot-reload,
# but until that lands a Nakama container restart is the
# rollover boundary (acceptable: at-most-once-monthly cost).
#
# systemd timer fires on the 5th of each month so the upstream
# publishing window has cleared. Hand-runnable too.

set -euo pipefail

DEST_DIR="/var/lib/snoringcat/geoip"
DEST="$DEST_DIR/dbip-country-lite.mmdb"
TMP="$DEST.tmp.$$"

YEAR=$(date -u +%Y)
MONTH=$(date -u +%m)
URL="https://download.db-ip.com/free/dbip-country-lite-$YEAR-$MONTH.mmdb.gz"

mkdir -p "$DEST_DIR"

echo "[geoip-refresh] fetching $URL"
if ! curl -fsSL --retry 3 --retry-delay 5 "$URL" \
		| gunzip -c > "$TMP"; then
	echo "[geoip-refresh] download failed; preserving existing $DEST"
	rm -f "$TMP"
	exit 1
fi

# Sanity-check the file isn't empty / truncated. A valid MMDB
# is at least a few MB; the country DB is consistently ~7 MB.
SIZE=$(stat -c%s "$TMP")
if [[ "$SIZE" -lt 1000000 ]]; then
	echo "[geoip-refresh] downloaded file suspiciously small ($SIZE bytes); aborting"
	rm -f "$TMP"
	exit 1
fi

mv -f "$TMP" "$DEST"
echo "[geoip-refresh] wrote $DEST ($SIZE bytes)"

# Best-effort: tell the Nakama container to reload so it
# picks up the new file without waiting for the next deploy.
# The runtime's initGeoIPFromEnv only runs at boot, so until
# a hot-reload RPC lands we need a process restart. This is
# safe: Nakama is HA-friendly internally (graceful shutdown +
# state in Postgres). Skipped silently if docker is missing
# (e.g. running this script outside the production host for
# a dry-run test).
if command -v docker >/dev/null 2>&1; then
	docker compose -f /opt/nakama/docker-compose.yml \
		restart nakama 2>/dev/null \
		|| docker restart nakama 2>/dev/null \
		|| true
fi
