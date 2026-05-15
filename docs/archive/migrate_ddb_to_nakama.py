#!/usr/bin/env python3
"""DDB → Nakama Storage / leaderboard migration (Phase E).

Reads each legacy hopnbop-* table, transforms records to Nakama
shape, and writes via a single bulk-import RPC on the Nakama
runtime side. Idempotent: the runtime uses if_not_exists writes,
so re-running the script is safe.

Modes:
    --dry-run        Scan only. Logs every record to JSONL but
                     never calls the Nakama RPC.
    --staging        Writes go into a "staging-" prefixed Nakama
                     storage namespace and a "staging-" leaderboard
                     suffix, for verification before the
                     destructive flip.
    (default = real)

Usage:
    pip install boto3 requests
    python scripts/migrate_ddb_to_nakama.py --dry-run
    python scripts/migrate_ddb_to_nakama.py --staging
    python scripts/migrate_ddb_to_nakama.py    # real flip

The Nakama RPC URL + HTTP key come from credentials.env:
    NAKAMA_URL       (default https://nakama.snoringcat.games)
    NAKAMA_HTTP_KEY  (must be set in
                     infra/remote/nakama/docker-compose.yml
                     entrypoint via --runtime.http_key)
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import boto3
import requests

# --------------------------------------------------------------
# Config
# --------------------------------------------------------------

AWS_PROFILE = "hopnbop"
AWS_REGION = "us-west-2"
DEFAULT_NAKAMA_URL = "https://nakama.snoringcat.games"
BATCH_SIZE = 50  # records per bulk_import RPC call
LOG_DIR = Path.home() / ".hopnbop-migration" / "phase-e-logs"

TABLES = [
    "hopnbop-players",
    "hopnbop-friends",
    "hopnbop-parties",
    "hopnbop-leaderboards",
    "hopnbop-settings",
    "hopnbop-match-history",
]

# Maps DDB table name → bulk_import "type" tag understood by the
# nakama-runtime bulk_import RPC.
TYPE_BY_TABLE = {
    "hopnbop-players": "players",
    "hopnbop-friends": "friends",
    "hopnbop-parties": "parties",
    "hopnbop-leaderboards": "leaderboards",
    "hopnbop-settings": "settings",
    "hopnbop-match-history": "match_history",
}


# --------------------------------------------------------------
# Helpers
# --------------------------------------------------------------


def load_credentials() -> dict[str, str]:
    """Source ~/.hopnbop-migration/credentials.env into a dict."""
    creds: dict[str, str] = {}
    creds_path = Path.home() / ".hopnbop-migration" / "credentials.env"
    if not creds_path.exists():
        sys.exit(f"credentials.env missing at {creds_path}")
    for line in creds_path.read_text().splitlines():
        if "=" in line and not line.lstrip().startswith("#"):
            k, _, v = line.partition("=")
            creds[k.strip()] = v.strip()
    return creds


@dataclass
class Args:
    dry_run: bool
    staging: bool
    only: str | None
    page_size: int


def parse_args() -> Args:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--dry-run", action="store_true",
                   help="Scan only; don't write to Nakama.")
    p.add_argument("--staging", action="store_true",
                   help="Write to staging- prefixed namespace.")
    p.add_argument("--only", default=None,
                   help="Migrate only this table name "
                        "(e.g. hopnbop-players).")
    p.add_argument("--page-size", type=int, default=200,
                   help="DDB scan page size.")
    a = p.parse_args()
    return Args(a.dry_run, a.staging, a.only, a.page_size)


# --------------------------------------------------------------
# Per-table transforms
# --------------------------------------------------------------


def transform(record_type: str, item: dict[str, Any]) -> dict[str, Any]:
    """Convert a DDB item to a Nakama-shaped dict.

    These transforms preserve as much of the original shape as
    possible. The runtime's bulk_import RPC interprets per-type
    fields and writes to Storage / leaderboards / friends graph.
    """
    if record_type == "players":
        return {
            "player_id": item.get("player_id", ""),
            "display_name": item.get("display_name", ""),
            "rating": int(item.get("rating", 1500)),
            "consent_accepted_at": int(
                item.get("consent_accepted_at", 0)),
            "consent_legal_version": item.get(
                "consent_legal_version", ""),
            "linked_providers": item.get("linked_providers", []),
            "created_at": int(item.get("created_at", 0)),
            "raw": item,
        }
    if record_type == "friends":
        return {
            "player_id": item.get("player_id", ""),
            "friend_id": item.get("friend_id", ""),
            "state": item.get("state", "friend"),
            "created_at": int(item.get("created_at", 0)),
        }
    if record_type == "parties":
        return {
            "party_id": item.get("party_id", ""),
            "leader_id": item.get("leader_id", ""),
            "members": item.get("members", []),
            "created_at": int(item.get("created_at", 0)),
        }
    if record_type == "leaderboards":
        return {
            "leaderboard_id": item.get("leaderboard_id", "ffa"),
            "player_id": item.get("player_id", ""),
            "score": int(item.get("score", 0)),
            "kills": int(item.get("kills", 0)),
            "bumps": int(item.get("bumps", 0)),
            "updated_at": int(item.get("updated_at", 0)),
        }
    if record_type == "settings":
        return {
            "player_id": item.get("player_id", ""),
            "scope": item.get("scope", "global"),
            "value": item.get("value", {}),
        }
    if record_type == "match_history":
        return {
            "player_id": item.get("player_id", ""),
            "match_id": item.get("match_id", ""),
            "result": item.get("result", {}),
            "ended_at": int(item.get("ended_at", 0)),
        }
    return item


# --------------------------------------------------------------
# DDB scanning
# --------------------------------------------------------------


def scan_table(
    ddb,
    table_name: str,
    page_size: int,
) -> Iterable[dict[str, Any]]:
    table = ddb.Table(table_name)
    last_key = None
    while True:
        kwargs: dict[str, Any] = {"Limit": page_size}
        if last_key is not None:
            kwargs["ExclusiveStartKey"] = last_key
        resp = table.scan(**kwargs)
        for item in resp.get("Items", []):
            yield item
        last_key = resp.get("LastEvaluatedKey")
        if last_key is None:
            return


# --------------------------------------------------------------
# Nakama RPC bulk import
# --------------------------------------------------------------


def post_bulk_import(
    nakama_url: str,
    http_key: str,
    record_type: str,
    namespace: str,
    records: list[dict[str, Any]],
) -> dict[str, Any]:
    """Calls the bulk_import RPC. Returns parsed payload dict."""
    url = f"{nakama_url}/v2/rpc/bulk_import?http_key={http_key}"
    body = json.dumps(json.dumps({
        "type": record_type,
        "namespace": namespace,
        "records": records,
    }))
    # Nakama's HTTP RPC gateway expects a quoted JSON string body.
    r = requests.post(
        url,
        data=body,
        headers={"Content-Type": "application/json"},
        timeout=30,
    )
    r.raise_for_status()
    payload = r.json().get("payload", "")
    if not payload:
        return {}
    return json.loads(payload)


# --------------------------------------------------------------
# Main
# --------------------------------------------------------------


def main() -> int:
    args = parse_args()
    creds = load_credentials()

    nakama_url = creds.get("NAKAMA_URL", DEFAULT_NAKAMA_URL)
    http_key = creds.get("NAKAMA_HTTP_KEY", "")
    namespace = "staging-" if args.staging else ""

    if not args.dry_run and not http_key:
        sys.exit(
            "NAKAMA_HTTP_KEY missing in credentials.env. "
            "Set it (32+ random bytes), redeploy Nakama with "
            "--runtime.http_key, then re-run this script.")

    LOG_DIR.mkdir(parents=True, exist_ok=True)
    run_id = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    log_path = LOG_DIR / f"migrate-{run_id}.jsonl"
    log = log_path.open("w", encoding="utf-8")

    session = boto3.session.Session(
        profile_name=AWS_PROFILE, region_name=AWS_REGION)
    ddb = session.resource("dynamodb")

    summary: dict[str, dict[str, int]] = {}

    tables = [args.only] if args.only else TABLES
    for table_name in tables:
        record_type = TYPE_BY_TABLE.get(table_name)
        if record_type is None:
            print(f"[skip] unknown table: {table_name}")
            continue

        scanned = 0
        written = 0
        failed = 0
        batch: list[dict[str, Any]] = []

        print(f"\n=== {table_name} (type={record_type}) ===")
        for item in scan_table(ddb, table_name, args.page_size):
            scanned += 1
            transformed = transform(record_type, item)
            log.write(json.dumps({
                "table": table_name,
                "type": record_type,
                "ddb": item,
                "nakama": transformed,
            }, default=str) + "\n")

            batch.append(transformed)
            if len(batch) >= BATCH_SIZE:
                if not args.dry_run:
                    try:
                        result = post_bulk_import(
                            nakama_url, http_key, record_type,
                            namespace, batch)
                        written += int(result.get("written",
                                                  len(batch)))
                        failed += int(result.get("failed", 0))
                    except Exception as e:
                        failed += len(batch)
                        log.write(json.dumps({
                            "error": str(e),
                            "batch_size": len(batch),
                        }) + "\n")
                else:
                    written += len(batch)  # nominal
                batch = []

        # Flush trailing batch.
        if batch:
            if not args.dry_run:
                try:
                    result = post_bulk_import(
                        nakama_url, http_key, record_type,
                        namespace, batch)
                    written += int(result.get("written",
                                              len(batch)))
                    failed += int(result.get("failed", 0))
                except Exception as e:
                    failed += len(batch)
                    log.write(json.dumps({
                        "error": str(e),
                        "batch_size": len(batch),
                    }) + "\n")
            else:
                written += len(batch)

        print(
            f"  scanned={scanned} written={written} failed={failed}"
        )
        summary[table_name] = {
            "scanned": scanned,
            "written": written,
            "failed": failed,
        }

    log.close()

    print("\n=== summary ===")
    print(json.dumps(summary, indent=2))
    print(f"\nLog: {log_path}")
    print(
        "\nNext step: spot-check 100 records of each type in "
        "Nakama console / pgAdmin, then re-run with reconciliation:\n"
        "    python scripts/migrate_ddb_to_nakama.py --reconcile\n"
        "(reconcile mode is a TODO — for now, hand-spot-check.)\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
