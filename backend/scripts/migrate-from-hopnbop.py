#!/usr/bin/env python3
"""Migrate Hop 'n Bop legacy tables into the new platform schema.

The new platform stack creates fresh `snoringcat-*` DynamoDB tables
alongside the existing `hopnbop-*` ones. This script copies data
across, performing the necessary key/attribute transformations:

    hopnbop-players          → snoringcat-accounts
                             + snoringcat-game-profiles
                               (game_id="hopnbop")
    hopnbop-provider-mappings → snoringcat-identities
    hopnbop-friends          → snoringcat-friends
    hopnbop-match-history    → snoringcat-match-history
                               (add game_id="hopnbop" attribute)
    hopnbop-leaderboard      → snoringcat-leaderboard
                               (leaderboard_id rewritten to
                                "hopnbop#<board>")
    hopnbop-parties          → snoringcat-parties
                               (add game_id="hopnbop" attribute)
    hopnbop-active-sessions  → snoringcat-active-sessions
                               (add game_id="hopnbop" attribute)
    hopnbop-fleet-state      → snoringcat-fleet-state
                               (state_key rewritten to
                                "game#hopnbop")
    hopnbop-settings         → snoringcat-settings
    hopnbop-consent-audit    → snoringcat-consent-audit

The script is **idempotent**: rerunning it after a partial run
catches any rows that didn't make it across, without producing
duplicates.

Usage:

    # Always start with a dry-run. Reads from prod, writes
    # nothing, prints counts and a few sample transformations.
    python migrate-from-hopnbop.py --dry-run

    # Apply for real. Writes go to the new tables only; the old
    # hopnbop-* tables are read but never modified.
    python migrate-from-hopnbop.py --apply

    # Migrate one table only (useful for re-runs after a fix):
    python migrate-from-hopnbop.py --apply --only players

    # Different game_id for the backfill (default: "hopnbop"):
    python migrate-from-hopnbop.py --apply --game-id hopnbop

The script does NOT:
- Touch any `hopnbop-*` table for writes.
- Delete the old tables. Run that separately once you've verified
  the new stack is serving traffic.
- Switch DNS / API base URLs. That happens client-side in Phase 2.

Safety:
- `--dry-run` is the default if you forget to pass `--apply`.
- Per-row writes use ConditionExpression `attribute_not_exists`
  on the partition key so a re-run never overwrites a row that
  was already migrated. Existing target rows are reported as
  "already migrated" instead.
- The script reports counts and a few sample input/output rows
  before any writes happen.
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
from collections.abc import Callable, Iterator
from dataclasses import dataclass, field
from decimal import Decimal
from typing import Any

import boto3
from botocore.exceptions import ClientError


logger = logging.getLogger("migrate")


# ---------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------


class _DecimalEncoder(json.JSONEncoder):
    """JSON encoder that handles boto3's Decimal values."""

    def default(self, obj):
        if isinstance(obj, Decimal):
            if obj % 1 == 0:
                return int(obj)
            return float(obj)
        return super().default(obj)


def _scan_all(table) -> Iterator[dict]:
    """Yield every row from a DynamoDB table, paging as needed."""
    last_key = None
    while True:
        kwargs = {}
        if last_key:
            kwargs["ExclusiveStartKey"] = last_key
        response = table.scan(**kwargs)
        for item in response.get("Items", []):
            yield item
        last_key = response.get("LastEvaluatedKey")
        if last_key is None:
            return


def _put_if_absent(
    table,
    item: dict,
    partition_key_name: str,
    sort_key_name: str | None = None,
) -> str:
    """PutItem with a "not exists on PK" condition.

    Returns:
        "wrote"   if the row was created.
        "exists"  if the row already existed.
    """
    condition = f"attribute_not_exists({partition_key_name})"
    if sort_key_name:
        condition += (
            f" AND attribute_not_exists({sort_key_name})"
        )
    try:
        table.put_item(
            Item=item, ConditionExpression=condition
        )
        return "wrote"
    except ClientError as e:
        code = e.response.get("Error", {}).get("Code", "")
        if code == "ConditionalCheckFailedException":
            return "exists"
        raise


# ---------------------------------------------------------------------
# Per-table migration plans
# ---------------------------------------------------------------------


@dataclass
class MigrationPlan:
    """Defines how to migrate one source table into one or more
    destination tables."""

    name: str  # User-facing name passed to --only.
    source_table: str
    # transform yields zero or more (dest_table, item, pk, sk) tuples
    # per source item. dest_table identifies the boto3 Table to use.
    transform: Callable[
        [dict], Iterator[tuple[str, dict, str, str | None]]
    ]
    dest_tables: list[str]
    description: str = ""
    # Maximum number of source rows to print as samples in --dry-run.
    sample_count: int = 2


@dataclass
class MigrationResult:
    """Counters from running a single MigrationPlan."""

    source_rows_read: int = 0
    rows_to_write: dict[str, int] = field(
        default_factory=dict
    )
    rows_already_existed: dict[str, int] = field(
        default_factory=dict
    )
    samples: list[dict] = field(default_factory=list)


# ---------------------------------------------------------------------
# Transforms — one per source table
# ---------------------------------------------------------------------


def _build_transforms(game_id: str):
    """Return all MigrationPlan definitions parameterized by game_id."""

    # --- players → accounts + game_profiles -------------------------

    # The legacy `players` row has both global account fields and
    # per-game stats. Split into:
    #   accounts row:    cross-game identity, friend_code, providers,
    #                    consent, locale, profile image, etc.
    #   game_profile row: rating, matches_played, wins, losses, plus
    #                    a `game_stats` blob that absorbs the
    #                    per-game stat fields (kills, bumps, snail
    #                    crushes, etc.).

    _ACCOUNT_FIELDS = {
        "player_id",
        "display_name",
        "friend_code",
        "is_anonymous",
        "device_id",
        "profile_image_url",
        "primary_locale",
        "auth_providers",
        "provider_display_names",
        "provider_profile_images",
        "consent_accepted_at",
        "consent_legal_version",
        "created_at",
        "updated_at",
        "last_active",
    }

    # Per-game *typed* fields kept as first-class attributes on the
    # game_profile row.
    _PROFILE_TYPED_FIELDS = {
        "rating",
        "matches_played",
        "wins",
        "losses",
        "first_play_time",
        "last_play_time",
        "total_time_played_sec",
    }

    def _players_transform(item):
        player_id = item["player_id"]

        # Build the accounts row.
        account_item = {"player_id": player_id}
        for f in _ACCOUNT_FIELDS:
            if f != "player_id" and f in item:
                account_item[f] = item[f]
        # `display_name` is required; if missing in the source,
        # synthesize one (rare/never expected, but safe).
        if "display_name" not in account_item:
            account_item["display_name"] = (
                f"Player_{player_id[2:10]}"
            )
        yield ("accounts", account_item, "player_id", None)

        # Build the game_profiles row. The PK is composite
        # (player_id, game_id).
        profile_item = {
            "player_id": player_id,
            "game_id": game_id,
            "rating_partition": f"{game_id}#all",
        }
        for f in _PROFILE_TYPED_FIELDS:
            if f in item:
                profile_item[f] = item[f]
        # first_played / last_played use the new field names.
        if "first_play_time" in item:
            profile_item["first_played"] = item[
                "first_play_time"
            ]
        if "last_play_time" in item:
            profile_item["last_played"] = item[
                "last_play_time"
            ]
        # game_stats absorbs every other per-game field.
        game_stats = {}
        for k, v in item.items():
            if (
                k in _ACCOUNT_FIELDS
                or k in _PROFILE_TYPED_FIELDS
                or k
                in {
                    "rating_partition",
                    "first_play_time",
                    "last_play_time",
                }
            ):
                continue
            game_stats[k] = v
        if game_stats:
            profile_item["game_stats"] = game_stats
        # Audit timestamps.
        if "created_at" in item:
            profile_item["created_at"] = item["created_at"]
        if "updated_at" in item:
            profile_item["updated_at"] = item["updated_at"]
        yield (
            "game_profiles",
            profile_item,
            "player_id",
            "game_id",
        )

    # --- provider_mappings → identities ----------------------------

    def _identities_transform(item):
        # No schema change; just a rename.
        yield (
            "identities",
            dict(item),
            "provider_composite",
            None,
        )

    # --- friends → friends -----------------------------------------

    def _friends_transform(item):
        yield (
            "friends",
            dict(item),
            "player_id",
            "friend_id",
        )

    # --- match_history → match_history (+ game_id attr) ------------

    def _match_history_transform(item):
        new = dict(item)
        new.setdefault("game_id", game_id)
        yield (
            "match_history",
            new,
            "player_id",
            "match_timestamp",
        )

    # --- leaderboard → leaderboard (rewrite id) --------------------

    def _leaderboard_transform(item):
        new = dict(item)
        leaderboard_id = item.get("leaderboard_id", "")
        # Only prepend "{game_id}#" if it isn't already there.
        if "#" not in leaderboard_id:
            new["leaderboard_id"] = (
                f"{game_id}#{leaderboard_id}"
            )
        yield (
            "leaderboard",
            new,
            "leaderboard_id",
            "score_player",
        )

    # --- parties → parties (+ game_id) -----------------------------

    def _parties_transform(item):
        new = dict(item)
        new.setdefault("game_id", game_id)
        yield ("parties", new, "party_id", None)

    # --- active_sessions → active_sessions (+ game_id) -------------

    def _active_sessions_transform(item):
        new = dict(item)
        new.setdefault("game_id", game_id)
        yield (
            "active_sessions",
            new,
            "player_id",
            None,
        )

    # --- fleet_state → fleet_state (rewrite state_key) -------------

    def _fleet_state_transform(item):
        new = dict(item)
        state_key = item.get("state_key", "")
        if not state_key.startswith("game#"):
            new["state_key"] = f"game#{game_id}"
        yield ("fleet_state", new, "state_key", None)

    # --- settings → settings (no change) ---------------------------

    def _settings_transform(item):
        yield ("settings", dict(item), "player_id", None)

    # --- consent_audit → consent_audit (no change) -----------------

    def _consent_audit_transform(item):
        yield (
            "consent_audit",
            dict(item),
            "player_id",
            None,
        )

    return [
        MigrationPlan(
            name="players",
            source_table="hopnbop-players",
            transform=_players_transform,
            dest_tables=["accounts", "game_profiles"],
            description=(
                "Split into global accounts row + per-game "
                "game_profiles row (game_id="
                + repr(game_id)
                + ")."
            ),
        ),
        MigrationPlan(
            name="provider_mappings",
            source_table="hopnbop-provider-mappings",
            transform=_identities_transform,
            dest_tables=["identities"],
            description="Rename only (no schema change).",
        ),
        MigrationPlan(
            name="friends",
            source_table="hopnbop-friends",
            transform=_friends_transform,
            dest_tables=["friends"],
            description=(
                "Friends table is global; copy as-is."
            ),
        ),
        MigrationPlan(
            name="match_history",
            source_table="hopnbop-match-history",
            transform=_match_history_transform,
            dest_tables=["match_history"],
            description=(
                "Add game_id attribute. SK refactor "
                "(game_id#timestamp) is a separate future migration."
            ),
        ),
        MigrationPlan(
            name="leaderboard",
            source_table="hopnbop-leaderboard",
            transform=_leaderboard_transform,
            dest_tables=["leaderboard"],
            description=(
                "Rewrite leaderboard_id to "
                "{game_id}#{board_name}."
            ),
        ),
        MigrationPlan(
            name="parties",
            source_table="hopnbop-parties",
            transform=_parties_transform,
            dest_tables=["parties"],
            description="Add game_id attribute.",
        ),
        MigrationPlan(
            name="active_sessions",
            source_table="hopnbop-active-sessions",
            transform=_active_sessions_transform,
            dest_tables=["active_sessions"],
            description="Add game_id attribute.",
        ),
        MigrationPlan(
            name="fleet_state",
            source_table="hopnbop-fleet-state",
            transform=_fleet_state_transform,
            dest_tables=["fleet_state"],
            description=(
                "Rewrite state_key to game#{game_id}."
            ),
        ),
        MigrationPlan(
            name="settings",
            source_table="hopnbop-settings",
            transform=_settings_transform,
            dest_tables=["settings"],
            description=(
                "No schema change in this migration. SK "
                "(scope) refactor is a future step."
            ),
        ),
        MigrationPlan(
            name="consent_audit",
            source_table="hopnbop-consent-audit",
            transform=_consent_audit_transform,
            dest_tables=["consent_audit"],
            description="Rename only.",
        ),
    ]


# Mapping from logical destination name → physical table name.
_DEST_TABLE_NAMES = {
    "accounts": "snoringcat-accounts",
    "identities": "snoringcat-identities",
    "game_profiles": "snoringcat-game-profiles",
    "friends": "snoringcat-friends",
    "presence": "snoringcat-presence",
    "match_history": "snoringcat-match-history",
    "leaderboard": "snoringcat-leaderboard",
    "parties": "snoringcat-parties",
    "active_sessions": "snoringcat-active-sessions",
    "fleet_state": "snoringcat-fleet-state",
    "settings": "snoringcat-settings",
    "consent_audit": "snoringcat-consent-audit",
}


# ---------------------------------------------------------------------
# Runner
# ---------------------------------------------------------------------


def _run_plan(
    plan: MigrationPlan,
    dynamodb,
    apply: bool,
    sample_only: int | None = None,
) -> MigrationResult:
    """Execute one MigrationPlan. Reports a MigrationResult."""

    result = MigrationResult()
    src = dynamodb.Table(plan.source_table)
    dest_tables = {
        name: dynamodb.Table(_DEST_TABLE_NAMES[name])
        for name in plan.dest_tables
    }

    rows_seen = 0
    for source_item in _scan_all(src):
        rows_seen += 1
        result.source_rows_read = rows_seen

        for (
            dest_name,
            dest_item,
            pk,
            sk,
        ) in plan.transform(source_item):
            # Sample for the dry-run report.
            if (
                len(result.samples) < plan.sample_count
                and (
                    sample_only is None
                    or rows_seen <= sample_only
                )
            ):
                result.samples.append(
                    {
                        "dest": dest_name,
                        "in": source_item,
                        "out": dest_item,
                    }
                )

            if not apply:
                # Dry-run: just count.
                result.rows_to_write[dest_name] = (
                    result.rows_to_write.get(dest_name, 0)
                    + 1
                )
                continue

            outcome = _put_if_absent(
                dest_tables[dest_name],
                dest_item,
                partition_key_name=pk,
                sort_key_name=sk,
            )
            if outcome == "wrote":
                result.rows_to_write[dest_name] = (
                    result.rows_to_write.get(dest_name, 0)
                    + 1
                )
            else:
                result.rows_already_existed[dest_name] = (
                    result.rows_already_existed.get(
                        dest_name, 0
                    )
                    + 1
                )

    return result


def _print_report(plan: MigrationPlan, result: MigrationResult, apply: bool):
    print()
    print(
        f"=== {plan.name} (source: {plan.source_table}) ==="
    )
    print(f"  {plan.description}")
    print(
        f"  Source rows read: {result.source_rows_read}"
    )
    for dest_name in plan.dest_tables:
        wrote = result.rows_to_write.get(dest_name, 0)
        exists = result.rows_already_existed.get(
            dest_name, 0
        )
        verb = (
            "Would write"
            if not apply
            else "Wrote"
        )
        line = (
            f"  {verb}: {wrote} → "
            f"{_DEST_TABLE_NAMES[dest_name]}"
        )
        if apply and exists:
            line += f"  (skipped {exists} already-migrated)"
        print(line)
    if result.samples:
        print(
            f"  Sample transforms (showing up to "
            f"{len(result.samples)}):"
        )
        for s in result.samples:
            print(
                f"    --- → {_DEST_TABLE_NAMES[s['dest']]} ---"
            )
            print(
                "    in:  "
                + json.dumps(
                    s["in"], cls=_DecimalEncoder
                )[:200]
            )
            print(
                "    out: "
                + json.dumps(
                    s["out"], cls=_DecimalEncoder
                )[:200]
            )


# ---------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------


def main():
    parser = argparse.ArgumentParser(
        description=(
            "Migrate hopnbop-* DynamoDB tables to the new "
            "snoringcat-* schema."
        )
    )
    parser.add_argument(
        "--apply",
        action="store_true",
        help=(
            "Actually write to the new tables. Without this, "
            "runs in dry-run mode (default)."
        ),
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "Explicit dry-run flag (default behavior even "
            "without it)."
        ),
    )
    parser.add_argument(
        "--game-id",
        default="hopnbop",
        help="game_id to backfill onto migrated rows.",
    )
    parser.add_argument(
        "--only",
        action="append",
        default=[],
        help=(
            "Run only the named plan(s). Repeat to include "
            "multiple. Default: run all."
        ),
    )
    parser.add_argument(
        "--profile",
        default=None,
        help="AWS profile (default: env / instance role).",
    )
    parser.add_argument(
        "--region",
        default="us-west-2",
        help="AWS region (default: us-west-2).",
    )
    parser.add_argument(
        "--samples",
        type=int,
        default=None,
        help=(
            "Limit sample collection to the first N source "
            "rows per plan."
        ),
    )
    parser.add_argument(
        "--verbose",
        action="store_true",
        help="Verbose logging.",
    )
    args = parser.parse_args()

    apply = args.apply and not args.dry_run

    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(levelname)s: %(message)s",
    )

    session = (
        boto3.Session(profile_name=args.profile)
        if args.profile
        else boto3.Session()
    )
    dynamodb = session.resource(
        "dynamodb", region_name=args.region
    )

    plans = _build_transforms(args.game_id)
    if args.only:
        wanted = set(args.only)
        plans = [p for p in plans if p.name in wanted]
        missing = wanted - {p.name for p in plans}
        if missing:
            parser.error(
                "unknown --only plan(s): "
                + ", ".join(sorted(missing))
            )

    print("=" * 60)
    print(
        "Hop 'n Bop → Snoring Cat Platform migration"
    )
    print(
        f"Mode: {'APPLY (writes)' if apply else 'DRY-RUN (no writes)'}"
    )
    print(f"Region: {args.region}")
    print(f"Profile: {args.profile or '(default)'}")
    print(f"Backfill game_id: {args.game_id}")
    print(
        "Plans: " + ", ".join(p.name for p in plans)
    )
    print("=" * 60)

    if not apply:
        print()
        print(
            "DRY-RUN: source tables read; nothing written."
        )
        print(
            "Pass --apply to perform writes."
        )

    summary: list[tuple[str, MigrationResult]] = []
    for plan in plans:
        try:
            result = _run_plan(
                plan,
                dynamodb,
                apply=apply,
                sample_only=args.samples,
            )
        except ClientError as e:
            code = (
                e.response.get("Error", {}).get(
                    "Code", ""
                )
            )
            if code == "ResourceNotFoundException":
                print()
                print(
                    f"=== {plan.name} ==="
                )
                print(
                    f"  SKIPPED: source table "
                    f"{plan.source_table} does not exist."
                )
                continue
            raise
        _print_report(plan, result, apply)
        summary.append((plan.name, result))

    print()
    print("=" * 60)
    print("Summary")
    print("=" * 60)
    total_read = 0
    total_wrote: dict[str, int] = {}
    total_existed: dict[str, int] = {}
    for name, r in summary:
        total_read += r.source_rows_read
        for dest, n in r.rows_to_write.items():
            total_wrote[dest] = (
                total_wrote.get(dest, 0) + n
            )
        for dest, n in r.rows_already_existed.items():
            total_existed[dest] = (
                total_existed.get(dest, 0) + n
            )
    print(f"Source rows read total: {total_read}")
    verb = "Would write" if not apply else "Wrote"
    for dest in sorted(total_wrote):
        line = (
            f"  {verb}: {total_wrote[dest]} → "
            f"{_DEST_TABLE_NAMES[dest]}"
        )
        if apply and total_existed.get(dest):
            line += (
                f"  (skipped {total_existed[dest]} "
                f"already-migrated)"
            )
        print(line)

    if not apply:
        print()
        print(
            "Nothing was written. Re-run with --apply when ready."
        )


if __name__ == "__main__":
    sys.exit(main())
