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
# ---------------------------------------------------------------------
hetzner_eur="0.00"
servers_json=$(curl -fsS \
	-H "Authorization: Bearer $HCLOUD_TOKEN" \
	"https://api.hetzner.cloud/v1/servers")
pricing_json=$(curl -fsS \
	-H "Authorization: Bearer $HCLOUD_TOKEN" \
	"https://api.hetzner.cloud/v1/pricing")

hetzner_eur=$(echo "$servers_json" | jq -r --arg now "$NOW_EPOCH" \
	--arg month_start "$MONTH_START_EPOCH" \
	--argjson pricing "$pricing_json" '
	def hourly_net($srv_type; $loc):
		($pricing.pricing.server_types[]
			| select(.name == $srv_type) | .prices[]
			| select(.location == $loc) | .price_hourly.net | tonumber);
	def monthly_net($srv_type; $loc):
		($pricing.pricing.server_types[]
			| select(.name == $srv_type) | .prices[]
			| select(.location == $loc) | .price_monthly.net | tonumber);
	[.servers[] | {
		name: .name,
		type: .server_type.name,
		loc: .datacenter.location.name,
		created_epoch: (.created | sub("\\.[0-9]+\\+"; "+") | fromdate),
	} | . + {
		hours_this_month: (
			((($now | tonumber)
				- ([.created_epoch, ($month_start | tonumber)] | max))
				/ 3600) | floor
		),
		hourly: hourly_net(.type; .loc),
		monthly_cap: monthly_net(.type; .loc),
	} | . + {
		eur: ([.hours_this_month * .hourly, .monthly_cap] | min)
	}] | map(.eur) | add // 0' 2>/dev/null || echo "0")
hetzner_usd=$(awk -v e="$hetzner_eur" 'BEGIN { printf "%.2f", e * 1.08 }')

# ---------------------------------------------------------------------
# Edgegap MTD spend.
# ---------------------------------------------------------------------
edgegap_usd="0.00"
if edgegap_resp=$(curl -fsS \
		-H "Authorization: Token $EDGEGAP_TOKEN" \
		"https://api.edgegap.com/v1/billing/current_month" 2>/dev/null); then
	amount=$(echo "$edgegap_resp" | jq -r '.amount // .total // 0' 2>/dev/null || echo 0)
	edgegap_usd=$(awk -v a="$amount" 'BEGIN { printf "%.2f", a }')
fi

total_usd=$(awk -v a="$hetzner_usd" -v b="$edgegap_usd" \
	'BEGIN { printf "%.2f", a + b }')

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

# ---------------------------------------------------------------------
# Emergency action.
# ---------------------------------------------------------------------
emergency_msg=""
if (( ${#new_crossings[@]} > 0 )); then
	for c in "${new_crossings[@]}"; do
		case "${c%:*}" in
			emergency)
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

# Day-over-day delta for the daily summary. If there's no
# previous-summary value (first summary ever, or first after a
# month rollover), day == MTD. Clamp to 0 so a transient
# pricing-API blip that briefly lowers MTD doesn't surface a
# negative.
compute_day_delta() {
	local current="$1"
	local previous="$2"
	if [[ -z "$previous" ]]; then
		echo "$current"
	else
		awk -v c="$current" -v p="$previous" \
			'BEGIN { d = c - p; if (d < 0) d = 0; printf "%.2f", d }'
	fi
}

# Common body lines shared between threshold-crossing and daily
# summary messages. Each tracked provider gets its own line so
# adding more later doesn't bunch up the formatting.
provider_lines="- Hetzner: \$$hetzner_usd
- Edgegap: \$$edgegap_usd
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

# Threshold crossings → immediate ping.
if (( ${#new_crossings[@]} > 0 )); then
	crossed_str=""
	for c in "${new_crossings[@]}"; do
		label="$(echo "${c%:*}" | tr 'a-z' 'A-Z' | tr '_' ' ')"
		crossed_str+="$label @ ${c#*:} | "
	done
	crossed_str="${crossed_str% | }"
	post_discord "${mention}**Threshold crossed: $crossed_str**
- Hetzner+Edgegap MTD: \$$total_usd ($MONTH_LABEL)
$provider_lines${emergency_msg}"
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

	# Daily-summary provider lines show day-over-day deltas with
	# MTD in parens. Threshold-crossing pings keep MTD-only since
	# they fire mid-day, not on a daily cadence.
	daily_provider_lines="- Hetzner: \$$hetzner_day_usd (\$$hetzner_usd MTD)
- Edgegap: \$$edgegap_day_usd (\$$edgegap_usd MTD)
- R2 storage: ${r2_gb} GB / 10 GB free tier"
	if $cf_pages_tracked; then
		daily_provider_lines+="
- CF Pages: ${cf_pages_builds_used} / 500 builds free tier"
	fi
	if $gh_tracked; then
		daily_provider_lines+="
- GH Actions: ${gh_minutes_used} min · ${gh_storage_gbh} GB-h storage"
		if awk -v p="$gh_paid_usd" 'BEGIN { exit !(p > 0) }'; then
			daily_provider_lines+=" (paid: \$${gh_paid_day_usd}, MTD \$${gh_paid_usd})"
		fi
	fi

	post_discord "**Billing status: \$$total_day_usd (\$$total_usd MTD)**$prev_month_line
$daily_provider_lines
- Thresholds — low \$$BUDGET_WARN_LOW · mid \$$BUDGET_WARN_MID · high \$$BUDGET_WARN_HIGH · emergency \$$EMERGENCY_CAP · R2 warn ${R2_WARN_GB:-8}GB · R2 hard ${R2_HARD_GB:-9.5}GB · CF Pages warn ${CF_PAGES_WARN_BUILDS:-400} · CF Pages hard ${CF_PAGES_HARD_BUILDS:-475}"
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

echo "[cost-monitor] $NOW_ISO total=\$$total_usd hetzner=\$$hetzner_usd edgegap=\$$edgegap_usd r2=${r2_gb}GB cf_pages=${cf_pages_builds_used} gh_min=${gh_minutes_used} gh_storage_gbh=${gh_storage_gbh} gh_paid=\$${gh_paid_usd} new_crossings=${new_crossings[*]:-none} summary=$should_summarize"
