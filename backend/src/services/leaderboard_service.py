"""Leaderboard service for DynamoDB operations."""

import os
from datetime import datetime, timezone
from typing import Optional

import boto3
from boto3.dynamodb.conditions import Key


# Zero-pad scores to this width for sort key ordering.
_SCORE_PAD_WIDTH = 8


def _make_score_player(score: int, player_id: str) -> str:
    """Build composite sort key: zero-padded score + player_id."""
    return f"{score:0{_SCORE_PAD_WIDTH}d}#{player_id}"


def _current_iso_week() -> str:
    """Return current ISO week string like '2026-W10'."""
    now = datetime.now(timezone.utc)
    return f"{now.isocalendar()[0]}-W{now.isocalendar()[1]:02d}"


def build_leaderboard_ids(
    level_id: str = "",
) -> list[str]:
    """Return the list of leaderboard IDs to update."""
    week = _current_iso_week()
    ids = [
        "alltime#global",
        f"weekly#{week}",
    ]
    if level_id:
        ids.append(f"alltime#{level_id}")
        ids.append(f"weekly#{level_id}#{week}")
    return ids


class LeaderboardService:
    """DynamoDB operations for leaderboards."""

    def __init__(self):
        self.dynamodb = boto3.resource("dynamodb")
        self.table_name = os.environ.get(
            "LEADERBOARD_TABLE", "hopnbop-leaderboard"
        )
        self.table = self.dynamodb.Table(self.table_name)

    def update_score(
        self,
        leaderboard_id: str,
        player_id: str,
        old_rating: int,
        new_rating: int,
        display_name: str,
        profile_image_url: str = "",
    ) -> None:
        """Update a player's leaderboard entry.

        Deletes the old score_player key and writes a new
        one with the updated rating.
        """
        old_key = _make_score_player(
            old_rating, player_id
        )
        new_key = _make_score_player(
            new_rating, player_id
        )

        # Delete old entry (ignore if missing).
        self.table.delete_item(
            Key={
                "leaderboard_id": leaderboard_id,
                "score_player": old_key,
            }
        )

        # Write new entry.
        item = {
            "leaderboard_id": leaderboard_id,
            "score_player": new_key,
            "player_id": player_id,
            "score": new_rating,
            "display_name": display_name,
        }
        if profile_image_url:
            item["profile_image_url"] = (
                profile_image_url
            )
        self.table.put_item(Item=item)

    def get_page(
        self,
        leaderboard_id: str,
        limit: int = 50,
        cursor: Optional[str] = None,
    ) -> tuple[list[dict], Optional[str]]:
        """Query a page of leaderboard entries (descending).

        Returns (entries, next_cursor). Each entry has
        rank, player_id, display_name, score.
        """
        kwargs = {
            "KeyConditionExpression": Key(
                "leaderboard_id"
            ).eq(leaderboard_id),
            "ScanIndexForward": False,
            "Limit": limit,
        }
        if cursor:
            kwargs["ExclusiveStartKey"] = {
                "leaderboard_id": leaderboard_id,
                "score_player": cursor,
            }

        response = self.table.query(**kwargs)
        items = response.get("Items", [])

        # Compute ranks. If we have a cursor, we need
        # to count how many items precede it.
        rank_offset = 0
        if cursor:
            rank_offset = self._count_above(
                leaderboard_id, cursor
            )

        entries = []
        for i, item in enumerate(items):
            entry = {
                "rank": rank_offset + i + 1,
                "player_id": item.get(
                    "player_id", ""
                ),
                "display_name": item.get(
                    "display_name", ""
                ),
                "score": int(
                    item.get("score", 0)
                ),
                "profile_image_url": item.get(
                    "profile_image_url", ""
                ),
            }
            entries.append(entry)

        next_cursor = None
        last_key = response.get("LastEvaluatedKey")
        if last_key:
            next_cursor = last_key["score_player"]

        return entries, next_cursor

    def get_player_context(
        self,
        leaderboard_id: str,
        player_id: str,
        rating: int,
        context_size: int = 5,
    ) -> dict:
        """Return the player's rank and surrounding entries."""
        score_player = _make_score_player(
            rating, player_id
        )

        # Count entries above this player.
        rank = self._count_above(
            leaderboard_id, score_player
        ) + 1

        # Query entries around the player.
        entries, _ = self.get_page(
            leaderboard_id,
            limit=context_size * 2 + 1,
            cursor=_make_score_player(
                rating + 1, ""
            ),
        )

        return {
            "rank": rank,
            "entries": entries,
        }

    def remove_player(self, player_id: str) -> None:
        """Remove a player from all leaderboard partitions.

        Scans the entire table filtered by player_id.
        This is acceptable because account deletion is
        infrequent.
        """
        scan_kwargs = {
            "FilterExpression": Key("player_id").eq(
                player_id
            ),
            "ProjectionExpression": (
                "leaderboard_id, score_player"
            ),
        }

        while True:
            response = self.table.scan(**scan_kwargs)
            items = response.get("Items", [])
            with self.table.batch_writer() as batch:
                for item in items:
                    batch.delete_item(
                        Key={
                            "leaderboard_id": item[
                                "leaderboard_id"
                            ],
                            "score_player": item[
                                "score_player"
                            ],
                        }
                    )
            if "LastEvaluatedKey" not in response:
                break
            scan_kwargs["ExclusiveStartKey"] = (
                response["LastEvaluatedKey"]
            )

    def delete_weekly_entries(self) -> None:
        """Delete all entries from weekly leaderboards."""
        scan_kwargs = {
            "FilterExpression": Key(
                "leaderboard_id"
            ).begins_with("weekly#"),
            "ProjectionExpression": (
                "leaderboard_id, score_player"
            ),
        }

        while True:
            response = self.table.scan(**scan_kwargs)
            items = response.get("Items", [])
            if not items:
                break
            with self.table.batch_writer() as batch:
                for item in items:
                    batch.delete_item(
                        Key={
                            "leaderboard_id": item[
                                "leaderboard_id"
                            ],
                            "score_player": item[
                                "score_player"
                            ],
                        }
                    )
            if "LastEvaluatedKey" not in response:
                break
            scan_kwargs["ExclusiveStartKey"] = (
                response["LastEvaluatedKey"]
            )

    def get_player_rank(
        self,
        leaderboard_id: str,
        player_id: str,
        rating: int,
    ) -> int:
        """Return the 1-based rank for the player in
        this specific leaderboard.

        Returns 0 if the player has no entry in the
        leaderboard (e.g. has not played this week for
        a weekly leaderboard).
        """
        score_player = _make_score_player(
            rating, player_id
        )
        check = self.table.get_item(
            Key={
                "leaderboard_id": leaderboard_id,
                "score_player": score_player,
            }
        )
        if not check.get("Item"):
            return 0
        return (
            self._count_above(
                leaderboard_id, score_player
            )
            + 1
        )

    def _count_above(
        self,
        leaderboard_id: str,
        score_player: str,
    ) -> int:
        """Count entries with score_player greater than given."""
        response = self.table.query(
            KeyConditionExpression=(
                Key("leaderboard_id").eq(leaderboard_id)
                & Key("score_player").gt(score_player)
            ),
            Select="COUNT",
        )
        return response.get("Count", 0)
