#!/bin/bash
# Host-side dead-man for the operator desktop's scheduled jobs.
#
# Every job that watches prod (prod-liveness-ping, slo-burn-rate,
# job-watchdog, backup-restore-smoke, ...) runs on Levi's desktop under
# Task Scheduler with LogonType Interactive. When that machine is off,
# asleep, or logged out, all of them stop, and so does their alerting:
# 2026-09-19 -> 09-25 went six days with nothing watching and nothing
# saying so (claude-config rules/scheduling.md, "Update-reboot blind
# window"). Only something off the desktop can notice that.
#
# Signal: the desktop's daily service-status-sync (07:03 PT) drains
# this host's status queue. Its phase A moves
# /var/log/snoringcat/service-status.jsonl aside to *.draining and
# touches a fresh empty queue; phase B deletes *.draining once the
# lines are durable on the desktop. So after a healthy drain the queue
# holds only entries written since, and an entry left undrained for
# longer than DRAIN_WATCH_STALE_HOURS (default 36) means the drain has
# stopped: the desktop is dark, or the drain itself is broken. Both
# deserve a page. *.draining is read too, so a drain that keeps dying
# after phase A (entries stuck in staging) also reads stale. No desktop
# change is needed; the drain already is the heartbeat.
#
# Why 36h, checked daily at 16:37 UTC: pg-backup queues an entry
# nightly at 03:11 UTC and the drain runs at 14:03 UTC (15:03 UTC in
# winter). At the check, one missed drain leaves the oldest entry
# ~13.5h old, which is fine (the desktop may just have slept through
# the morning). Two consecutive misses leave it ~37.5h old: page.
#
# Blind spot, by design: an empty queue reads fresh. If the host's
# producers (pg-backup, cost-monitor) also stopped writing, nothing is
# waiting to age, but those have their own Discord failure alerts.
#
# At most one page per DRAIN_WATCH_REALERT_HOURS (default 20), so the
# daily timer pages once a day while stale and a Persistent=true
# catch-up run after a reboot cannot double-page. Recovery clears the
# stamp and queues a 'green' status entry (no Discord post).
#
# Usage:
#   desktop-drain-watch.sh                 # what the timer runs
#   desktop-drain-watch.sh --dry-run       # report only
#   desktop-drain-watch.sh --dry-run --queue /tmp/q.jsonl \
#       --stamp /tmp/stamp --now <epoch>   # exercise each branch
# --dry-run never posts to Discord and never appends to the queue.
# Without --stamp it also swaps in a throwaway stamp under /tmp, so a
# dry run can never suppress a real page.
#
# Reads /opt/snoringcat/desktop-drain-watch/.env (rendered by
# phase-b.ps1 Step-DrainWatch): DISCORD_WEBHOOK_URL,
# SERVICE_STATUS_LOG, DRAIN_WATCH_STALE_HOURS (integer hours).

set -Eeuo pipefail

ENV_FILE="/opt/snoringcat/desktop-drain-watch/.env"
if [[ -f "$ENV_FILE" ]]; then
	# shellcheck disable=SC1090
	set -a; source "$ENV_FILE"; set +a
fi

QUEUE="${SERVICE_STATUS_LOG:-/var/log/snoringcat/service-status.jsonl}"
STAMP="${DRAIN_WATCH_STAMP:-/var/lib/snoringcat/desktop-drain-watch.stamp}"
STALE_HOURS="${DRAIN_WATCH_STALE_HOURS:-36}"
REALERT_HOURS="${DRAIN_WATCH_REALERT_HOURS:-20}"
NOW="$(date -u +%s)"
DRY_RUN=0
STAMP_GIVEN=0

while (( $# > 0 )); do
	case "$1" in
		--dry-run) DRY_RUN=1 ;;
		--queue) QUEUE="$2"; shift ;;
		--stamp) STAMP="$2"; STAMP_GIVEN=1; shift ;;
		--now) NOW="$2"; shift ;;
		*)
			echo "usage: $0 [--dry-run] [--queue FILE] [--stamp FILE] [--now EPOCH]" >&2
			exit 2
			;;
	esac
	shift
done
if (( DRY_RUN && ! STAMP_GIVEN )); then
	STAMP="/tmp/desktop-drain-watch.dry-run.stamp"
fi
STAGING="${QUEUE}.draining"
HOST="$(hostname -s)"

log() { echo "[desktop-drain-watch] $*"; }

# Same failure treatment as cost-monitor / pg-backup: a broken
# dead-man pages instead of failing quietly. errtrace runs this in
# command-substitution subshells too, and a failure there fires it
# twice (subshell, then parent), so a marker file keeps it to one
# page (a variable set in the subshell never reaches the parent).
ALERT_MARKER="$(mktemp -u)"
trap 'rm -f "$ALERT_MARKER"' EXIT
on_error() {
	local line="$1"
	[[ -e "$ALERT_MARKER" ]] && return 0
	: > "$ALERT_MARKER" 2>/dev/null || true
	echo "[desktop-drain-watch] FAILED at line ${line}" >&2
	(( DRY_RUN )) && return 0
	[[ -n "${DISCORD_WEBHOOK_URL:-}" ]] || return 0
	local msg="⚠️ desktop-drain-watch FAILED at line ${line} on ${HOST}: the desktop dead-man is not checking. \`journalctl -u desktop-drain-watch.service -n 30\`"
	curl -fsS -m 20 -X POST -H "Content-Type: application/json" \
		-d "$(jq -n --arg c "$msg" '{content: $c}')" \
		"$DISCORD_WEBHOOK_URL" >/dev/null 2>&1 || true
}
trap 'on_error "$LINENO"' ERR

post_discord() {
	local content="$1"
	if (( DRY_RUN )); then
		log "[dry-run] would post to Discord:"
		printf '%s\n' "$content" | sed 's/^/    /'
		return 0
	fi
	if [[ -z "${DISCORD_WEBHOOK_URL:-}" ]]; then
		log "ERROR: DISCORD_WEBHOOK_URL unset; cannot page" >&2
		return 1
	fi
	curl -fsS -m 20 -X POST -H "Content-Type: application/json" \
		-d "$(jq -n --arg c "$content" '{content: $c}')" \
		"$DISCORD_WEBHOOK_URL" >/dev/null
}

# Queue a status entry for the desktop to drain into its trail, in the
# same shape cost-monitor / pg-backup write.
post_status() {
	local level="$1" summary="$2" details="${3:-}"
	[[ -n "$details" ]] || details='{}'
	if (( DRY_RUN )); then
		log "[dry-run] would queue status: ${level} ${summary} ${details}"
		return 0
	fi
	local ts; ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	jq -nc \
		--arg ts "$ts" \
		--arg source "desktop-drain-watch" \
		--arg level "$level" \
		--arg summary "$summary" \
		--argjson details "$details" \
		'{ts: $ts, source: $source, level: $level,
			summary: $summary, details: $details}' \
		>> "$QUEUE"
}

# Oldest undrained entry across staging + queue, as
# "<count> <oldest ts> <oldest epoch>". Lines that are not JSON objects
# with an RFC3339 UTC `ts` are skipped rather than fatal.
JQ_OLDEST='
[split("\n")[] | select(length > 0)
	| (try fromjson catch null) | objects | .ts | strings
	| {ts: ., e: (try (sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) catch null)}
	| select(.e != null)]
| if length == 0 then "0 - 0"
	else (min_by(.e)) as $o | "\(length) \($o.ts) \($o.e)" end
'
files=()
for f in "$STAGING" "$QUEUE"; do
	if [[ -s "$f" ]]; then files+=("$f"); fi
done
count=0
oldest_ts="-"
oldest_epoch=0
if (( ${#files[@]} > 0 )); then
	out="$(cat "${files[@]}" | jq -R -r -s "$JQ_OLDEST")"
	read -r count oldest_ts oldest_epoch <<< "$out"
fi

staging_note=""
if [[ -s "$STAGING" ]]; then
	staging_note=" ($(basename "$STAGING") present: the drain stalls after moving the queue aside)"
fi
stale=0
age_h=0
if (( count > 0 )); then
	age_s=$(( NOW - oldest_epoch ))
	age_h=$(( age_s / 3600 ))
	if (( age_s > STALE_HOURS * 3600 )); then stale=1; fi
fi

# Whole hours since a stamp was written, never negative (a --now in
# the past, or clock skew, would otherwise print "-21h ago").
hours_since() {
	local h=$(( (NOW - $(stat -c %Y "$1")) / 3600 ))
	if (( h < 0 )); then h=0; fi
	echo "$h"
}

if (( ! stale )); then
	if [[ -f "$STAMP" ]]; then
		since_h=$(hours_since "$STAMP")
		rm -f "$STAMP"
		post_status "green" "desktop drain recovered (last paged ${since_h}h ago)" \
			"$(jq -nc --argjson n "$count" '{undrained: $n}')"
		log "recovered: the queue is being drained again (last page ${since_h}h ago); stamp cleared"
	fi
	if (( count == 0 )); then
		log "fresh: queue empty (nothing waiting for the desktop)"
	else
		log "fresh: ${count} undrained, oldest ${oldest_ts} (${age_h}h, threshold ${STALE_HOURS}h)"
	fi
	exit 0
fi

summary="${count} undrained, oldest ${oldest_ts} (${age_h}h > ${STALE_HOURS}h)${staging_note}"
if [[ -f "$STAMP" ]]; then
	last=$(stat -c %Y "$STAMP")
	if (( NOW - last < REALERT_HOURS * 3600 )); then
		log "STALE: ${summary}; already paged $(hours_since "$STAMP")h ago, not re-paging"
		exit 0
	fi
fi

msg="⚠️ The desktop looks dark: ${HOST}'s service-status queue has not been drained for ${age_h}h (oldest undrained entry ${oldest_ts}, ${count} waiting; page threshold ${STALE_HOURS}h)${staging_note}.
service-status-sync on DESKTOP-GIVJS3D drains it daily at 07:03 PT, so the desktop's scheduled prod checks (prod-liveness-ping, slo-burn-rate, job-watchdog, backup-restore-smoke, ...) have most likely stopped too: the machine is off, asleep, or logged out, or the drain itself is failing.
Recover it over Tailscale RDP, then check claude-config/jobs/logs. This repeats once a day until the queue drains."

if post_discord "$msg"; then
	touch -d "@${NOW}" "$STAMP"
	staging_present=false
	if [[ -s "$STAGING" ]]; then staging_present=true; fi
	post_status "red" "desktop has not drained the status queue for ${age_h}h" \
		"$(jq -nc \
			--arg oldest "$oldest_ts" \
			--argjson n "$count" \
			--argjson h "$age_h" \
			--argjson staging "$staging_present" \
			'{oldest_undrained: $oldest, undrained: $n,
				age_hours: $h, staging_present: $staging}')"
	if (( DRY_RUN )); then
		log "STALE: ${summary}; dry run, nothing posted"
	else
		log "STALE: ${summary}; paged Discord"
	fi
	exit 0
fi
log "STALE: ${summary}; the Discord post FAILED, retrying next run" >&2
exit 1
