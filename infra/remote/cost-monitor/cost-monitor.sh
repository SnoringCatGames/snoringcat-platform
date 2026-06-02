#!/bin/bash
# Hourly cost monitor for the snoringcat-platform stack.
#
# Each run: compute MTD spend, decide whether to alert.
# - Threshold crossings (LOW / MID / HIGH / EMERGENCY) fire a
#   Discord ping immediately, gated by a state file so we don't
#   re-alert every hour for the same threshold.
# - One routine "Daily cost" summary per day, posted at
#   $DAILY_SUMMARY_HOUR_UTC (default 09:00).
# - Other runs are silent.
#
# The EMERGENCY threshold also scales the Edgegap fleet to 0
# (capacity_max=0 via PATCH), halting new allocations.
#
# Reads /opt/snoringcat/cost-monitor/.env for tokens + thresholds.

set -euo pipefail

ENV_FILE="/opt/snoringcat/cost-monitor/.env"
STATE_FILE_DEFAULT="/var/lib/snoringcat/cost-monitor-state.json"

[[ -f "$ENV_FILE" ]] || { echo "Missing $ENV_FILE"; exit 1; }
# shellcheck disable=SC1090
set -a; source "$ENV_FILE"; set +a

EMERGENCY_CAP="${EMERGENCY_CAP:-50}"
BUDGET_WARN_LOW="${BUDGET_WARN_LOW:-20}"
BUDGET_WARN_MID="${BUDGET_WARN_MID:-40}"
BUDGET_WARN_HIGH="${BUDGET_WARN_HIGH:-80}"
EDGEGAP_ACTIVE_WARN="${EDGEGAP_ACTIVE_WARN:-5}"
EDGEGAP_ACTIVE_HARD="${EDGEGAP_ACTIVE_HARD:-15}"
# mCPU allocated per Edgegap deployment of this app's version.
# Read from app-version's req_cpu. Hop'n'Bop's Tier 1.1 cost-
# reduction work (2026-06-01) right-sized req_cpu from 1024 to
# 512 mCPU after measuring per-match CPU. Set the env override
# if a game uses a different size.
EDGEGAP_MCPU_PER_DEPLOY="${EDGEGAP_MCPU_PER_DEPLOY:-512}"
# USD per mCPU-minute of Deployment Compute. Verified against
# May 2026 invoice S95TLZML-0002: $0.00115 per 1,000 mCPU-minutes
# = $0.00000115 per mCPU-minute. Set to `0` to disable dollar
# estimation entirely.
EDGEGAP_RATE_USD_PER_MCPU_MIN="${EDGEGAP_RATE_USD_PER_MCPU_MIN:-0.00000115}"
DAILY_SUMMARY_HOUR_UTC="${DAILY_SUMMARY_HOUR_UTC:-9}"
DISCORD_USER_ID="${DISCORD_USER_ID:-}"
STATE_FILE="${STATE_FILE:-$STATE_FILE_DEFAULT}"

# GitHub Actions usage tracking (org-level). Requires a token
# with `manage_billing:actions` scope (classic PAT) or fine-
# grained PAT with Plan:read access on the org. Without these,
# the GitHub block in this script is silently skipped.
GITHUB_ORG="${GITHUB_ORG:-snoringcatgames}"

CURRENT_MONTH="$(date -u +%Y-%m)"
CURRENT_DAY="$(date -u +%Y-%m-%d)"
CURRENT_HOUR_UTC="$(date -u +%-H)"
NOW_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
NOW_EPOCH="$(date -u +%s)"
MONTH_START_EPOCH="$(date -u -d "${CURRENT_MONTH}-01" +%s)"
MONTH_LABEL="$(date -u -d "${CURRENT_MONTH}-01" "+%B %Y")"

mkdir -p "$(dirname "$STATE_FILE")"

# ---------------------------------------------------------------------
# State
# ---------------------------------------------------------------------
state="{}"
if [[ -f "$STATE_FILE" ]]; then
	state=$(cat "$STATE_FILE")
fi
state_month=$(echo "$state" | jq -r '.month // ""')
# Carry the previous summary's total into a sticky field. The
# daily summary uses this to show day-over-day deltas so big
# jumps (server recreates, pricing-API blips, month rollover)
# are visible at a glance instead of looking like a fresh
# monthly start.
prev_summary_total_usd=$(echo "$state" | jq -r '.last_summary_total_usd // ""')
prev_summary_hetzner_usd=$(echo "$state" | jq -r '.last_summary_hetzner_usd // ""')
prev_summary_edgegap_usd=$(echo "$state" | jq -r '.last_summary_edgegap_usd // ""')
prev_summary_gh_paid_usd=$(echo "$state" | jq -r '.last_summary_gh_paid_usd // ""')
prev_month_total_usd=""
prev_month_label=""
if [[ "$state_month" != "$CURRENT_MONTH" ]]; then
	# Capture the just-closed month's last-known total before
	# resetting state. Stored in `prev_month_*` and surfaced on
	# the first daily summary of the new month.
	if [[ -n "$state_month" ]]; then
		prev_month_total_usd="$prev_summary_total_usd"
		prev_month_label="$(date -u -d "${state_month}-01" "+%B %Y" 2>/dev/null || echo "$state_month")"
	fi
	state=$(jq -n \
		--arg m "$CURRENT_MONTH" \
		--arg pmt "$prev_month_total_usd" \
		--arg pml "$prev_month_label" \
		'{
			month: $m,
			thresholds_crossed: [],
			last_summary_day: "",
			last_summary_total_usd: "",
			last_summary_hetzner_usd: "",
			last_summary_edgegap_usd: "",
			last_summary_gh_paid_usd: "",
			edgegap_tracked: {},
			edgegap_completed_minutes: 0,
			hetzner_servers_active: {},
			hetzner_servers_settled: [],
			prev_month_total_usd: $pmt,
			prev_month_label: $pml
		}')
	# After reset, prev_summary_*_usd are no longer meaningful
	# for day-over-day comparison (different month).
	prev_summary_total_usd=""
	prev_summary_hetzner_usd=""
	prev_summary_edgegap_usd=""
	prev_summary_gh_paid_usd=""
fi
# The first summary after a rollover surfaces the carried-over
# values; later summaries within the same month don't need them.
carry_prev_month_total_usd=$(echo "$state" | jq -r '.prev_month_total_usd // ""')
carry_prev_month_label=$(echo "$state" | jq -r '.prev_month_label // ""')

# ---------------------------------------------------------------------
# Hetzner spend (MTD).
# Real model: Hetzner caps at the monthly rate. Pro-rata up to the
# cap, per server, since the later of (server creation, start of
# month). Use net price (no VAT for US accounts).
#
# Why the per-server ledger: `/v1/servers` only returns currently-
# existing servers. Servers created and deleted within the month
# (Phase migrations, dev experiments, transient Pulumi resizes) are
# invisible by the time of any later poll. May 2026 invoice
# undercounted by ~$4 because of this. The ledger settles each
# server's eur cost on first disappearance so transient resources
# survive in `hetzner_servers_settled`.
# ---------------------------------------------------------------------
hetzner_eur="0.00"
servers_json=$(curl -fsS \
	-H "Authorization: Bearer $HCLOUD_TOKEN" \
	"https://api.hetzner.cloud/v1/servers")
pricing_json=$(curl -fsS \
	-H "Authorization: Bearer $HCLOUD_TOKEN" \
	"https://api.hetzner.cloud/v1/pricing")

# Step 1: snapshot the current server set in a normalized form.
current_servers=$(echo "$servers_json" | jq --arg ms "$MONTH_START_EPOCH" '
	[.servers[] | {
		id: (.id | tostring),
		type: .server_type.name,
		loc: .datacenter.location.name,
		created_epoch: (.created | sub("\\.[0-9]+\\+"; "+") | fromdate),
		start: ([(.created | sub("\\.[0-9]+\\+"; "+") | fromdate),
			($ms | tonumber)] | max)
	}] | INDEX(.id)
')

# Step 2: ledger update.
# - Servers in active but not in current: settle into hetzner_servers_settled.
# - Servers in current but not in active: add to active.
# - Servers in both: bump last_seen.
# Eur cost for a settled entry is captured at settle time so a
# later pricing-API change doesn't retroactively shift bills.
hetzner_active_in=$(echo "$state" | jq -c '.hetzner_servers_active // {}')
hetzner_settled_in=$(echo "$state" | jq -c '.hetzner_servers_settled // []')
ledger=$(jq -n \
	--argjson active "$hetzner_active_in" \
	--argjson settled "$hetzner_settled_in" \
	--argjson current "$current_servers" \
	--argjson pricing "$pricing_json" \
	--arg now "$NOW_EPOCH" '
	def hourly($t; $l):
		($pricing.pricing.server_types[]
			| select(.name == $t) | .prices[]
			| select(.location == $l) | .price_hourly.net | tonumber);
	def monthly($t; $l):
		($pricing.pricing.server_types[]
			| select(.name == $t) | .prices[]
			| select(.location == $l) | .price_monthly.net | tonumber);
	def eur(hours; t; l): ([hours * hourly(t; l), monthly(t; l)] | min);
	# Settle disappeared servers.
	reduce ($active | to_entries[]) as $e (
		{active: $active, settled: $settled};
		if ($current | has($e.key)) then .
		else
			(($e.value.last_seen - $e.value.start) / 3600) as $h |
			.settled += [{
				id: $e.key, type: $e.value.type, loc: $e.value.loc,
				start: $e.value.start, end: $e.value.last_seen,
				hours: $h, eur: eur($h; $e.value.type; $e.value.loc)
			}] |
			.active = (.active | del(.[$e.key]))
		end
	)
	# Add new + bump existing.
	| reduce ($current | to_entries[]) as $c (.;
		if (.active | has($c.key)) then
			.active[$c.key].last_seen = ($now | tonumber)
		else
			.active[$c.key] = {
				type: $c.value.type,
				loc: $c.value.loc,
				start: $c.value.start,
				last_seen: ($now | tonumber)
			}
		end
	)')
hetzner_active_out=$(echo "$ledger" | jq -c '.active')
hetzner_settled_out=$(echo "$ledger" | jq -c '.settled')

# Step 3: total eur = settled-eur + active-running-cost.
hetzner_eur=$(jq -n \
	--argjson active "$hetzner_active_out" \
	--argjson settled "$hetzner_settled_out" \
	--argjson pricing "$pricing_json" \
	--arg now "$NOW_EPOCH" -r '
	def hourly($t; $l):
		($pricing.pricing.server_types[]
			| select(.name == $t) | .prices[]
			| select(.location == $l) | .price_hourly.net | tonumber);
	def monthly($t; $l):
		($pricing.pricing.server_types[]
			| select(.name == $t) | .prices[]
			| select(.location == $l) | .price_monthly.net | tonumber);
	def eur(hours; t; l): ([hours * hourly(t; l), monthly(t; l)] | min);
	([$active[] | eur((($now | tonumber) - .start) / 3600; .type; .loc)]
		| add // 0)
	+ ([$settled[] | .eur] | add // 0)' 2>/dev/null || echo "0")
hetzner_usd=$(awk -v e="$hetzner_eur" 'BEGIN { printf "%.2f", e * 1.08 }')

# Persist ledger.
state=$(echo "$state" | jq \
	--argjson active "$hetzner_active_out" \
	--argjson settled "$hetzner_settled_out" \
	'.hetzner_servers_active = $active
	| .hetzner_servers_settled = $settled')

# ---------------------------------------------------------------------
# Edgegap MTD usage + cost estimate.
#
# Edgegap exposes no public billing endpoint (every documented
# `/v1/billing/*` and `/v1/wallet/*` path 404s as of 2026-05-06,
# returning a marketing landing page). The `/v1/deployments`
# listing only returns currently-active containers — terminated
# deployments fall off, even with `?status=Terminated`. So we
# accumulate usage ourselves across hourly runs:
#
#   state.edgegap_tracked = { request_id: { start: epoch,
#                                            last_seen: epoch } }
#   state.edgegap_completed_minutes = number
#
# Each run:
#   1. Fetch current active deployments.
#   2. For tracked deployments missing from current active:
#      add (last_seen - start) / 60 to completed_minutes, drop
#      them from tracked. (Up to ~1h tail per terminated
#      deployment is lost since the cost-monitor timer is
#      hourly.)
#   3. For currently-active deployments:
#      - new ones get added with start = parsed start_time,
#        last_seen = now.
#      - existing ones get last_seen bumped to now.
#   4. MTD minutes = completed + sum((last_seen - start) for
#      tracked), each entry's start clamped at month_start so
#      cross-month deployments don't backfill prior months.
#
# Dollar estimate uses EDGEGAP_MCPU_PER_DEPLOY (default 1024 =
# 1 vCPU; matches hopnbop-server) and EDGEGAP_RATE_USD_PER_MCPU_MIN
# (default $0.00000115, verified against May 2026 invoice
# S95TLZML-0002 line "Deployment Compute" at $0.00115/1000 mCPU-min).
#
# Known limitation: this poller misses matches that start AND end
# between two timer firings. With 5-min cadence we catch matches >5
# min, but Hop'n'Bop's median match length is likely 3-8 min so
# the floor of uncounted minutes is real. A definitive fix is to
# have the Nakama runtime ledger each deployment lifecycle event
# (start in fleet_allocator.go, end in match_lifecycle.go) into a
# JSONL the cost-monitor reads instead of polling. May 2026 invoice
# was ~3.5x the cost-monitor estimate after this fix, almost
# entirely attributable to the polling gap.
# ---------------------------------------------------------------------
edgegap_active_count=0
edgegap_completed_minutes=$(echo "$state" | jq -r \
	'.edgegap_completed_minutes // 0')
edgegap_tracked=$(echo "$state" | jq -c '.edgegap_tracked // {}')

if edgegap_resp=$(curl -fsS \
		-H "Authorization: Token $EDGEGAP_TOKEN" \
		"https://api.edgegap.com/v1/deployments?limit=100" \
		2>/dev/null); then

	# Build "active_starts.txt" lines of <id>\t<start_epoch>.
	# `date -d` parses Edgegap's RFC3339 (with sub-second and
	# TZ offset) where jq's fromdateiso8601 fails.
	active_lines=""
	while IFS=$'\t' read -r rid start_iso; do
		[[ -z "$rid" ]] && continue
		start_epoch=$(date -d "$start_iso" +%s 2>/dev/null || echo 0)
		[[ "$start_epoch" -gt 0 ]] || continue
		active_lines+="${rid}	${start_epoch}"$'\n'
	done < <(echo "$edgegap_resp" \
		| jq -r '.data[] | "\(.request_id)\t\(.start_time)"' \
			2>/dev/null)

	# Active-set as JSON for jq diffs.
	active_set_json=$(printf "%s" "$active_lines" \
		| jq -R -s 'split("\n") | map(select(length > 0))
			| map(split("\t"))
			| map({(.[0]): (.[1] | tonumber)})
			| add // {}')
	edgegap_active_count=$(echo "$active_set_json" \
		| jq 'length')

	# Settle terminated tracked entries. For each tracked
	# request_id missing from the active set, fold its
	# (last_seen - start) into completed_minutes (clamped to
	# month_start so a cross-month deployment doesn't
	# backfill prior months) and drop it.
	settle=$(jq -n \
		--argjson tracked "$edgegap_tracked" \
		--argjson active "$active_set_json" \
		--arg ms "$MONTH_START_EPOCH" \
		--arg completed "$edgegap_completed_minutes" '
		def contrib(start; last_seen; ms_):
			((last_seen - ([start, ms_] | max)) / 60)
			| (if . < 0 then 0 else . end);
		reduce ($tracked | to_entries[]) as $e (
			{ tracked: $tracked,
				completed: ($completed | tonumber) };
			if ($active | has($e.key)) then .
			else
				.completed = (.completed
					+ contrib($e.value.start;
						$e.value.last_seen;
						($ms | tonumber)))
				| .tracked = (.tracked | del(.[$e.key]))
			end
		)
		| .completed = (.completed * 10 | floor / 10)')
	edgegap_tracked=$(echo "$settle" | jq -c '.tracked')
	edgegap_completed_minutes=$(echo "$settle" \
		| jq -r '.completed')

	# Add new actives, bump last_seen on known actives.
	edgegap_tracked=$(echo "$edgegap_tracked" | jq -c \
		--argjson active "$active_set_json" \
		--arg now "$NOW_EPOCH" '
		reduce ($active | to_entries[]) as $a (.;
			if has($a.key) then
				.[$a.key].last_seen = ($now | tonumber)
			else
				.[$a.key] = {
					start: $a.value,
					last_seen: ($now | tonumber)
				}
			end)')
fi

# Currently-running minutes: sum (now - start) for tracked,
# each entry clamped at month_start.
edgegap_active_minutes=$(echo "$edgegap_tracked" \
	| jq --arg now "$NOW_EPOCH" --arg ms "$MONTH_START_EPOCH" '
		[.[] | (($now | tonumber)
			- ([.start, ($ms | tonumber)] | max)) / 60]
		| add // 0
		| (. * 10 | floor / 10)')

edgegap_mtd_minutes=$(awk \
	-v c="$edgegap_completed_minutes" \
	-v a="$edgegap_active_minutes" \
	'BEGIN { printf "%.1f", c + a }')
edgegap_active_hours=$(awk -v m="$edgegap_active_minutes" \
	'BEGIN { printf "%.1f", m / 60 }')
edgegap_mtd_hours=$(awk -v m="$edgegap_mtd_minutes" \
	'BEGIN { printf "%.1f", m / 60 }')

edgegap_usd="0.00"
if awk -v r="$EDGEGAP_RATE_USD_PER_MCPU_MIN" \
		'BEGIN { exit !(r > 0) }'; then
	edgegap_usd=$(awk \
		-v m="$edgegap_mtd_minutes" \
		-v c="$EDGEGAP_MCPU_PER_DEPLOY" \
		-v r="$EDGEGAP_RATE_USD_PER_MCPU_MIN" \
		'BEGIN { printf "%.2f", m * c * r }')
fi

# Persist Edgegap tracking state. Threshold checks below run
# against a `total_usd` that includes Edgegap only when a rate
# is configured; otherwise dollar thresholds remain Hetzner-only
# and the EDGEGAP_ACTIVE_* thresholds catch runaway allocation.
state=$(echo "$state" | jq \
	--argjson tracked "$edgegap_tracked" \
	--arg completed "$edgegap_completed_minutes" \
	'.edgegap_tracked = $tracked
	| .edgegap_completed_minutes = ($completed | tonumber)')

if awk -v r="$EDGEGAP_RATE_USD_PER_MCPU_MIN" \
		'BEGIN { exit !(r > 0) }'; then
	total_usd=$(awk -v a="$hetzner_usd" -v b="$edgegap_usd" \
		'BEGIN { printf "%.2f", a + b }')
else
	total_usd="$hetzner_usd"
fi

# ---------------------------------------------------------------------
# Cloudflare R2 storage usage.
# Free tier: 10 GB storage / 1M class-A ops / 10M class-B / mo.
# Egress is free. Storage is the only realistic overage risk for
# this project. Class-A/B requests would need millions/day to
# matter — not modeled here.
#
# We use list-objects + sum because the /usage endpoint
# aggregates with hourly+ lag and reports 0 for bucket changes
# that happened in the last hour. List-objects is real-time.
# Pagination caps at 1000 per call; for our 4-file bucket we
# fit in one page comfortably.
# ---------------------------------------------------------------------
r2_bytes="0"
r2_gb="0.00"
if [[ -n "${CLOUDFLARE_API_TOKEN:-}" \
		&& -n "${CLOUDFLARE_ACCOUNT_ID:-}" \
		&& -n "${R2_BUCKET:-}" ]]; then
	cursor=""
	r2_bytes=0
	while :; do
		url="https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/r2/buckets/$R2_BUCKET/objects?per_page=1000"
		[[ -n "$cursor" ]] && url="${url}&cursor=${cursor}"
		if ! resp=$(curl -fsS \
				-H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
				"$url" 2>/dev/null); then
			break
		fi
		page_sum=$(echo "$resp" | jq '[.result[].size] | add // 0' 2>/dev/null || echo 0)
		r2_bytes=$((r2_bytes + page_sum))
		cursor=$(echo "$resp" | jq -r '.result_info.cursor // ""' 2>/dev/null)
		[[ -z "$cursor" || "$cursor" == "null" ]] && break
	done
	r2_gb=$(awk -v b="$r2_bytes" 'BEGIN { printf "%.2f", b / (1024*1024*1024) }')
fi

# ---------------------------------------------------------------------
# Cloudflare Pages build count (account-wide).
# Free tier: 500 builds / month across all projects on the
# account. Bandwidth and requests are effectively unlimited on
# the free plan and are not modeled.
#
# Pages has no aggregate-by-month endpoint. We list projects,
# then for each project page through deployments newest-first,
# counting those with created_on >= start of current month.
# Stop paging a project once we see a deployment older than the
# month boundary. Token must have Account.Pages:Read (the
# existing CLOUDFLARE_PAGES_TOKEN with Pages:Edit qualifies).
# ---------------------------------------------------------------------
cf_pages_tracked=false
cf_pages_builds_used=0
if [[ -n "${CLOUDFLARE_API_TOKEN:-}" \
		&& -n "${CLOUDFLARE_ACCOUNT_ID:-}" ]]; then
	# Pages list-projects rejects pagination params (HTTP 400
	# "Invalid list options"), so call without them.
	if projects_resp=$(curl -fsS \
			-H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
			"https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/pages/projects" \
			2>/dev/null); then
		while IFS= read -r project; do
			[[ -z "$project" ]] && continue
			page=1
			while :; do
				url="https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/pages/projects/$project/deployments?page=$page&per_page=25"
				if ! resp=$(curl -fsS \
						-H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
						"$url" 2>/dev/null); then
					break
				fi
				count=$(echo "$resp" | jq -r --arg ms "$MONTH_START_EPOCH" '
					[.result[]
						| select(
							(.created_on
								| sub("\\.[0-9]+Z"; "Z")
								| fromdate)
								>= ($ms | tonumber))]
					| length' 2>/dev/null || echo 0)
				cf_pages_builds_used=$((cf_pages_builds_used + count))
				has_old=$(echo "$resp" | jq -r --arg ms "$MONTH_START_EPOCH" '
					any(.result[];
						(.created_on
							| sub("\\.[0-9]+Z"; "Z")
							| fromdate)
							< ($ms | tonumber))' 2>/dev/null \
					|| echo "true")
				page_count=$(echo "$resp" | jq -r '.result | length' \
					2>/dev/null || echo 0)
				if [[ "$has_old" == "true" || "$page_count" -lt 25 ]]; then
					break
				fi
				page=$((page + 1))
			done
		done < <(echo "$projects_resp" | jq -r '.result[].name' 2>/dev/null)
		cf_pages_tracked=true
	fi
fi

# ---------------------------------------------------------------------
# GitHub Actions billing (new "enhanced billing" API, GA 2025).
# Endpoint:
#   /organizations/{org}/settings/billing/usage?year=Y&month=M
# Returns an array of `usageItems` with one row per
# (date, product, sku, repo) combination. We aggregate
# client-side to derive:
#   gh_minutes_used  — sum of quantity for unitType=Minutes
#   gh_storage_gbh   — sum of quantity for unitType=GigabyteHours
#   gh_paid_usd      — sum of netAmount across all rows (anything
#                      past the included free tier)
#
# The legacy `/orgs/{org}/settings/billing/{actions,shared-storage}`
# endpoints retired with HTTP 410 ("This endpoint has been moved",
# https://gh.io/billing-api-updates-org).
#
# Required token scope: `admin:org` is sufficient for org-billed
# accounts (the existing token has it). Block silently skips on
# failure.
# ---------------------------------------------------------------------
gh_tracked=false
gh_minutes_used=0
gh_storage_gbh="0.00"
gh_paid_usd="0.00"
if [[ -n "${GITHUB_TOKEN:-}" && -n "${GITHUB_ORG:-}" ]]; then
	gh_year=$(date -u +%Y)
	gh_month=$(date -u +%-m)
	if usage_resp=$(curl -fsS \
			-H "Authorization: Bearer $GITHUB_TOKEN" \
			-H "Accept: application/vnd.github+json" \
			"https://api.github.com/organizations/$GITHUB_ORG/settings/billing/usage?year=$gh_year&month=$gh_month" \
			2>/dev/null); then
		gh_minutes_used=$(echo "$usage_resp" | jq -r '
			[.usageItems[]
				| select(.product == "actions" and .unitType == "Minutes")
				| .quantity] | add // 0 | floor')
		gh_storage_gbh=$(echo "$usage_resp" | jq -r '
			[.usageItems[]
				| select(.product == "actions" and .unitType == "GigabyteHours")
				| .quantity] | add // 0 | . * 100 | floor / 100')
		gh_paid_usd=$(echo "$usage_resp" | jq -r '
			[.usageItems[] | .netAmount] | add // 0
			| . * 100 | floor / 100')
		gh_paid_usd=$(awk -v p="$gh_paid_usd" 'BEGIN { printf "%.2f", p }')
		gh_tracked=true
	fi
fi

# ---------------------------------------------------------------------
# Threshold crossings.
# ---------------------------------------------------------------------
already_crossed=$(echo "$state" | jq -r '.thresholds_crossed[]' 2>/dev/null \
	| sort -u)
declare -a new_crossings=()
for entry in "low:$BUDGET_WARN_LOW" "mid:$BUDGET_WARN_MID" \
		"high:$BUDGET_WARN_HIGH" "emergency:$EMERGENCY_CAP"; do
	name="${entry%:*}"
	value="${entry#*:}"
	if awk -v t="$total_usd" -v v="$value" 'BEGIN { exit !(t >= v) }'; then
		if ! echo "$already_crossed" | grep -qx "$name"; then
			new_crossings+=("$name:$value")
			state=$(echo "$state" | jq --arg n "$name" \
				'.thresholds_crossed += [$n]')
		fi
	fi
done

# R2 size thresholds. Use the same crossings array so a single
# Discord message covers everything.
for entry in "r2_warn:${R2_WARN_GB:-8}" "r2_hard:${R2_HARD_GB:-9.5}"; do
	name="${entry%:*}"
	value="${entry#*:}"
	if awk -v t="$r2_gb" -v v="$value" 'BEGIN { exit !(t >= v) }'; then
		if ! echo "$already_crossed" | grep -qx "$name"; then
			new_crossings+=("$name:${value}GB")
			state=$(echo "$state" | jq --arg n "$name" \
				'.thresholds_crossed += [$n]')
		fi
	fi
done

# Cloudflare Pages build count thresholds. Free tier is 500/mo
# account-wide. Defaults: warn at 80%, hard at 95%.
if $cf_pages_tracked; then
	for entry in "cf_pages_warn:${CF_PAGES_WARN_BUILDS:-400}" \
			"cf_pages_hard:${CF_PAGES_HARD_BUILDS:-475}"; do
		name="${entry%:*}"
		value="${entry#*:}"
		if awk -v t="$cf_pages_builds_used" -v v="$value" \
				'BEGIN { exit !(t >= v) }'; then
			if ! echo "$already_crossed" | grep -qx "$name"; then
				new_crossings+=("$name:${value} builds")
				state=$(echo "$state" | jq --arg n "$name" \
					'.thresholds_crossed += [$n]')
			fi
		fi
	done
fi

# GitHub overage threshold. The new billing API gives a single
# `netAmount` per row (already discounted for included-tier
# usage), so any positive total means we've gone past the free
# quota on at least one SKU. The single threshold key covers
# both compute (Minutes) and storage (GigabyteHours).
if $gh_tracked; then
	if awk -v p="$gh_paid_usd" 'BEGIN { exit !(p > 0) }'; then
		if ! echo "$already_crossed" | grep -qx "gh_paid"; then
			new_crossings+=("gh_paid:\$${gh_paid_usd}")
			state=$(echo "$state" | jq \
				'.thresholds_crossed += ["gh_paid"]')
		fi
	fi
fi

# Edgegap active-deployment thresholds. Replaces the old dollar-
# based emergency trigger now that we can't read Edgegap dollars.
# A normal Hop'n'Bop match is one container at a time; sustained
# concurrent counts above the warn threshold suggest leaked
# match_end teardowns or a runaway allocation loop. The hard
# threshold PATCHes capacity_max=0 the same way the old
# `emergency` did.
for entry in "edgegap_active_warn:${EDGEGAP_ACTIVE_WARN:-5}" \
		"edgegap_active_hard:${EDGEGAP_ACTIVE_HARD:-15}"; do
	name="${entry%:*}"
	value="${entry#*:}"
	if awk -v t="$edgegap_active_count" -v v="$value" \
			'BEGIN { exit !(t >= v) }'; then
		if ! echo "$already_crossed" | grep -qx "$name"; then
			new_crossings+=("$name:${value} active")
			state=$(echo "$state" | jq --arg n "$name" \
				'.thresholds_crossed += [$n]')
		fi
	fi
done

# ---------------------------------------------------------------------
# Emergency action.
# ---------------------------------------------------------------------
emergency_msg=""
if (( ${#new_crossings[@]} > 0 )); then
	for c in "${new_crossings[@]}"; do
		case "${c%:*}" in
			emergency|edgegap_active_hard)
				emergency_msg+=$'\n**EMERGENCY** Scaling Edgegap fleet to 0.'
				if [[ -n "${EDGEGAP_APP_NAME:-}" ]]; then
					curl -fsS -X PATCH \
						-H "Authorization: Token $EDGEGAP_TOKEN" \
						-H "Content-Type: application/json" \
						"https://api.edgegap.com/v1/app/$EDGEGAP_APP_NAME" \
						-d '{"capacity_max": 0}' >/dev/null || true
				fi
				;;
			r2_hard)
				# Active enforcement happens in
				# deploy-cf-pages.ps1, which queries this same
				# /usage endpoint before each upload. The alert
				# here just makes sure we notice.
				emergency_msg+=$'\n**R2 HARD CAP** Bucket is at the configured limit. New deploys will refuse to upload until you free space (or raise R2_HARD_GB).'
				;;
			cf_pages_hard)
				emergency_msg+=$'\n**CF PAGES HARD CAP** Account is near the 500 builds/month free-tier limit. Cloudflare will queue further builds until next billing cycle (or raise CF_PAGES_HARD_BUILDS).'
				;;
		esac
	done
fi

# ---------------------------------------------------------------------
# Discord routing.
# ---------------------------------------------------------------------
mention=""
if [[ -n "$DISCORD_USER_ID" ]]; then
	mention="<@${DISCORD_USER_ID}> "
fi

last_summary_day=$(echo "$state" | jq -r '.last_summary_day // ""')
should_summarize=0
if [[ "$CURRENT_HOUR_UTC" == "$DAILY_SUMMARY_HOUR_UTC" \
		&& "$last_summary_day" != "$CURRENT_DAY" ]]; then
	should_summarize=1
	state=$(echo "$state" | jq --arg d "$CURRENT_DAY" '.last_summary_day = $d')
fi

post_discord() {
	local content="$1"
	curl -fsS -X POST \
		-H "Content-Type: application/json" \
		-d "$(jq -n --arg c "$content" '{content:$c}')" \
		"$DISCORD_WEBHOOK_URL" >/dev/null
}

# Append a structured status entry to SERVICE_STATUS_LOG (a JSONL
# file SSH-drained by the daily/weekly LLM consolidators on the
# operator's machine). Silent no-op when SERVICE_STATUS_LOG isn't
# set (preserves backwards compat with hosts that haven't been
# re-provisioned via phase-b yet).
#
# Usage: post_status <source> <level> <summary> [<details_json>]
#   source         e.g. 'cost-monitor', 'pg-backup'
#   level          'info' | 'warn' | 'red' | 'green'
#   summary        one-line headline
#   details_json   optional JSON object string; defaults to {}
post_status() {
	local source="$1"
	local level="$2"
	local summary="$3"
	local details="${4:-{\}}"
	[[ -n "${SERVICE_STATUS_LOG:-}" ]] || return 0
	local ts; ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	mkdir -p "$(dirname "$SERVICE_STATUS_LOG")"
	jq -nc \
		--arg ts "$ts" \
		--arg source "$source" \
		--arg level "$level" \
		--arg summary "$summary" \
		--argjson details "$details" \
		'{ts: $ts, source: $source, level: $level,
			summary: $summary, details: $details}' \
		>> "$SERVICE_STATUS_LOG"
}

# Day-over-day delta for the daily summary. Returns "" when no
# previous-summary value is available (first summary ever, first
# after a month rollover, or first run after the schema added a
# new last_summary_*_usd field) so the formatter can drop the
# delta column instead of misreporting current == day. Clamps to
# 0 if a transient pricing-API blip briefly lowers MTD.
compute_day_delta() {
	local current="$1"
	local previous="$2"
	if [[ -z "$previous" ]]; then
		echo ""
	else
		awk -v c="$current" -v p="$previous" \
			'BEGIN { d = c - p; if (d < 0) d = 0; printf "%.2f", d }'
	fi
}

# Format "$daily ($mtd MTD)" or fall back to "$mtd MTD" when no
# delta is available. Used in both the headline and provider
# lines.
fmt_pair() {
	local daily="$1"
	local mtd="$2"
	if [[ -z "$daily" ]]; then
		echo "\$$mtd MTD"
	else
		echo "\$$daily (\$$mtd MTD)"
	fi
}

# Render an Edgegap line with active count, MTD hours, and
# (if a rate is configured) MTD dollar estimate. "MTD" here is
# our locally-tracked sum of completed + still-running minutes;
# see the Edgegap section above for accuracy notes.
fmt_edgegap_line() {
	local line="- Edgegap: ${edgegap_active_count} active"
	line+=", ${edgegap_mtd_hours}h MTD"
	if awk -v r="$EDGEGAP_RATE_USD_PER_MCPU_MIN" \
			'BEGIN { exit !(r > 0) }'; then
		line+=" (~\$${edgegap_usd} @ ${EDGEGAP_MCPU_PER_DEPLOY} mCPU/deploy)"
	fi
	echo "$line"
}

# Common body lines shared between threshold-crossing and daily
# summary messages. Each tracked provider gets its own line so
# adding more later doesn't bunch up the formatting.
provider_lines="- Hetzner: \$$hetzner_usd
$(fmt_edgegap_line)
- R2 storage: ${r2_gb} GB / 10 GB free tier"
if $cf_pages_tracked; then
	provider_lines+="
- CF Pages: ${cf_pages_builds_used} / 500 builds free tier"
fi
if $gh_tracked; then
	provider_lines+="
- GH Actions: ${gh_minutes_used} min · ${gh_storage_gbh} GB-h storage"
	# Only call out paid-tier dollars if there are any. Keeps
	# the happy-path summary terse.
	if awk -v p="$gh_paid_usd" 'BEGIN { exit !(p > 0) }'; then
		provider_lines+=" (paid: \$${gh_paid_usd})"
	fi
fi

# First summary of a new month carries the closing total of the
# previous month forward, so the headline drop from rollover
# doesn't look mysterious.
prev_month_line=""
if [[ -n "$carry_prev_month_total_usd" \
		&& -n "$carry_prev_month_label" ]]; then
	prev_month_line=$'\n'"$carry_prev_month_label closed at \$$carry_prev_month_total_usd"
fi

# Threshold crossings → immediate ping. Threshold crossings are
# the one cost-monitor signal that bypasses the daily-consolidator
# pipeline because they imply user action is needed *now* (esp.
# the EMERGENCY threshold which already PATCHed Edgegap to
# capacity_max=0). We also write a 'warn'-level status entry so
# the doc has the audit trail.
if (( ${#new_crossings[@]} > 0 )); then
	crossed_str=""
	for c in "${new_crossings[@]}"; do
		label="$(echo "${c%:*}" | tr 'a-z' 'A-Z' | tr '_' ' ')"
		crossed_str+="$label @ ${c#*:} | "
	done
	crossed_str="${crossed_str% | }"
	post_discord "${mention}**Threshold crossed: $crossed_str**
- Hetzner MTD: \$$total_usd ($MONTH_LABEL)
$provider_lines${emergency_msg}"
	post_status "cost-monitor" "warn" \
		"threshold crossed: $crossed_str" \
		"$(jq -nc \
			--arg total "$total_usd" \
			--arg hetzner "$hetzner_usd" \
			--arg edgegap "$edgegap_usd" \
			--arg crossings "$crossed_str" \
			'{total_usd: $total, hetzner_usd: $hetzner,
				edgegap_usd: $edgegap, crossings: $crossings}')"
fi

# Daily summary.
if (( should_summarize )); then
	total_day_usd=$(compute_day_delta \
		"$total_usd" "$prev_summary_total_usd")
	hetzner_day_usd=$(compute_day_delta \
		"$hetzner_usd" "$prev_summary_hetzner_usd")
	edgegap_day_usd=$(compute_day_delta \
		"$edgegap_usd" "$prev_summary_edgegap_usd")
	gh_paid_day_usd=$(compute_day_delta \
		"$gh_paid_usd" "$prev_summary_gh_paid_usd")

	# Render an Edgegap line that adds day-over-day delta when
	# a rate is configured AND a previous summary's edgegap_usd
	# is on file; otherwise just the MTD form rendered for
	# threshold pings.
	fmt_edgegap_daily_line() {
		local has_rate=0
		awk -v r="$EDGEGAP_RATE_USD_PER_MCPU_MIN" \
			'BEGIN { exit !(r > 0) }' && has_rate=1
		if (( has_rate )) && [[ -n "$edgegap_day_usd" ]]; then
			echo "- Edgegap: ${edgegap_active_count} active, ${edgegap_mtd_hours}h MTD ($(fmt_pair "$edgegap_day_usd" "$edgegap_usd"))"
		else
			fmt_edgegap_line
		fi
	}

	# Daily-summary provider lines show day-over-day deltas with
	# MTD in parens, falling back to MTD-only when no previous
	# value is available (e.g. first run after a month rollover
	# or after a state-schema change). Threshold-crossing pings
	# always use MTD-only.
	daily_provider_lines="- Hetzner: $(fmt_pair "$hetzner_day_usd" "$hetzner_usd")
$(fmt_edgegap_daily_line)
- R2 storage: ${r2_gb} GB / 10 GB free tier"
	if $cf_pages_tracked; then
		daily_provider_lines+="
- CF Pages: ${cf_pages_builds_used} / 500 builds free tier"
	fi
	if $gh_tracked; then
		daily_provider_lines+="
- GH Actions: ${gh_minutes_used} min · ${gh_storage_gbh} GB-h storage"
		if awk -v p="$gh_paid_usd" 'BEGIN { exit !(p > 0) }'; then
			daily_provider_lines+=" (paid: $(fmt_pair "$gh_paid_day_usd" "$gh_paid_usd"))"
		fi
	fi

	# Daily summary is informational and goes to the local
	# service-status JSONL queue. The daily LLM consolidator
	# SSH-fetches and folds it into the single morning Discord
	# post. (Pre-2026-05-07 this was a direct post_discord call
	# at 09:00 UTC; redirecting to JSONL is the user-requested
	# noise-reduction change.)
	post_status "cost-monitor" "info" \
		"daily mtd \$$total_usd ($MONTH_LABEL)" \
		"$(jq -nc \
			--arg total_day "$total_day_usd" \
			--arg total_mtd "$total_usd" \
			--arg hetzner_day "$hetzner_day_usd" \
			--arg hetzner_mtd "$hetzner_usd" \
			--arg edgegap_day "$edgegap_day_usd" \
			--arg edgegap_mtd "$edgegap_usd" \
			--arg edgegap_active "$edgegap_active_count" \
			--arg edgegap_hours "$edgegap_mtd_hours" \
			--arg r2_gb "$r2_gb" \
			--arg cf_pages_builds "$cf_pages_builds_used" \
			--arg gh_minutes "$gh_minutes_used" \
			--arg gh_paid "$gh_paid_usd" \
			--arg month "$MONTH_LABEL" \
			'{total_day_usd: $total_day, total_mtd_usd: $total_mtd,
				hetzner_day_usd: $hetzner_day, hetzner_mtd_usd: $hetzner_mtd,
				edgegap_day_usd: $edgegap_day, edgegap_mtd_usd: $edgegap_mtd,
				edgegap_active: $edgegap_active, edgegap_mtd_hours: $edgegap_hours,
				r2_gb: $r2_gb, cf_pages_builds: $cf_pages_builds,
				gh_minutes: $gh_minutes, gh_paid_usd: $gh_paid,
				month_label: $month}')"
	# Capture this summary's headline numbers so the next daily
	# summary can compute day-over-day deltas, and so the next
	# month rollover can carry the total forward as the closing
	# total. Also clear the carry-forward fields once they've
	# been displayed.
	state=$(echo "$state" | jq \
		--arg t "$total_usd" \
		--arg h "$hetzner_usd" \
		--arg e "$edgegap_usd" \
		--arg gp "$gh_paid_usd" \
		'.last_summary_total_usd = $t
		| .last_summary_hetzner_usd = $h
		| .last_summary_edgegap_usd = $e
		| .last_summary_gh_paid_usd = $gp
		| .prev_month_total_usd = ""
		| .prev_month_label = ""')
fi

# Persist state.
echo "$state" > "$STATE_FILE"

echo "[cost-monitor] $NOW_ISO total=\$$total_usd hetzner=\$$hetzner_usd edgegap=\$${edgegap_usd} edgegap_active=${edgegap_active_count} edgegap_mtd_hours=${edgegap_mtd_hours} r2=${r2_gb}GB cf_pages=${cf_pages_builds_used} gh_min=${gh_minutes_used} gh_storage_gbh=${gh_storage_gbh} gh_paid=\$${gh_paid_usd} new_crossings=${new_crossings[*]:-none} summary=$should_summarize"
