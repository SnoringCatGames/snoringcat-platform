"""Match service for recording results and querying stats."""

import logging
import math
import os
from datetime import datetime
from decimal import Decimal
from typing import Any, Optional

import boto3
from boto3.dynamodb.conditions import Key

from services.leaderboard_service import (
    LeaderboardService,
    build_leaderboard_ids,
)

logger = logging.getLogger(__name__)


# Elo rating constants.
_ELO_K = 32
_ELO_FLOOR = 100

# Leaderboard defaults.
_DEFAULT_LEADERBOARD_LIMIT = 50
_MAX_LEADERBOARD_LIMIT = 100

# Match history defaults.
_DEFAULT_HISTORY_LIMIT = 20

# Extended stat keys sent by newer clients.
_EXTENDED_STAT_KEYS = [
    "jump_count",
    "water_time_sec",
    "water_jump_count",
    "ice_time_sec",
    "spring_launch_count",
    "direction_change_count",
    "snail_crush_count",
    "cricket_disturb_count",
    "fish_disturb_count",
    "butterfly_disturb_count",
    "fly_proximity_time_sec",
    "poop_count",
    "average_height",
]


class MatchService:
    """DynamoDB operations for match results and leaderboards."""

    def __init__(self):
        self.dynamodb = boto3.resource("dynamodb")
        self.players_table_name = os.environ.get(
            "PLAYERS_TABLE", "hopnbop-players"
        )
        self.history_table_name = os.environ.get(
            "MATCH_HISTORY_TABLE", "hopnbop-match-history"
        )
        self.players_table = self.dynamodb.Table(
            self.players_table_name
        )
        self.history_table = self.dynamodb.Table(
            self.history_table_name
        )
        self.leaderboard_service = LeaderboardService()

    def record_match_result(
        self,
        game_session_id: str,
        match_duration_sec: float,
        level_id: str,
        player_results: list[dict],
    ) -> None:
        """Record match results for all players.

        Writes one match history item per player and
        atomically updates each player's stats.
        """
        now = int(datetime.now().timestamp())
        player_count = len(player_results)

        # Calculate average rating for Elo.
        ratings = []
        for pr in player_results:
            player = self.players_table.get_item(
                Key={"player_id": pr["player_id"]},
                ProjectionExpression="rating",
            )
            item = player.get("Item", {})
            ratings.append(int(item.get("rating", 1500)))

        avg_rating = (
            sum(ratings) / len(ratings) if ratings else 1500
        )

        # Write match history and update stats per player.
        for i, pr in enumerate(player_results):
            player_id = pr["player_id"]
            is_win = pr["rank"] == 1
            player_rating = ratings[i]
            rating_delta = self._calculate_rating_delta(
                player_rating, avg_rating, is_win
            )

            # Write match history item with all stats.
            history_item = {
                "player_id": player_id,
                "match_timestamp": now,
                "game_session_id": game_session_id,
                "level_id": level_id,
                "match_duration_sec": Decimal(
                    str(round(match_duration_sec, 1))
                ),
                "rank": pr["rank"],
                "player_count": player_count,
                "score": pr.get("score", 0),
                "kill_count": pr.get("kill_count", 0),
                "death_count": pr.get("death_count", 0),
                "bump_count": pr.get("bump_count", 0),
                "crown_time_sec": Decimal(
                    str(
                        round(
                            pr.get("crown_time_sec", 0),
                            1,
                        )
                    )
                ),
                "regicide_count": pr.get(
                    "regicide_count", 0
                ),
                "is_win": is_win,
            }
            # Extended stats (may be absent from older
            # clients).
            for stat_key in _EXTENDED_STAT_KEYS:
                if stat_key in pr:
                    val = pr[stat_key]
                    if isinstance(val, float):
                        history_item[stat_key] = Decimal(
                            str(round(val, 2))
                        )
                    else:
                        history_item[stat_key] = val
            self.history_table.put_item(Item=history_item)

            # Atomically update player stats and lifetime
            # totals.
            win_expr = (
                "wins = wins + :one"
                if is_win
                else "losses = losses + :one"
            )
            new_rating = max(
                _ELO_FLOOR, player_rating + rating_delta
            )

            # Build ADD expression for lifetime stat
            # increments. DynamoDB ADD treats missing
            # attributes as 0.
            add_parts = []
            attr_values = {
                ":one": 1,
                ":new_rating": new_rating,
                ":now": now,
                ":all": "all",
                ":zero": Decimal("0"),
                ":duration": Decimal(
                    str(round(match_duration_sec, 1))
                ),
            }

            stat_mappings = [
                ("total_kills", "kill_count"),
                ("total_deaths", "death_count"),
                ("total_bumps", "bump_count"),
                ("total_regicide_count", "regicide_count"),
                ("total_jumps", "jump_count"),
                (
                    "total_water_count",
                    "water_jump_count",
                ),
                (
                    "total_spring_count",
                    "spring_launch_count",
                ),
                (
                    "total_direction_changes",
                    "direction_change_count",
                ),
                (
                    "total_snail_crushes",
                    "snail_crush_count",
                ),
                (
                    "total_cricket_disturbances",
                    "cricket_disturb_count",
                ),
                (
                    "total_fish_disturbances",
                    "fish_disturb_count",
                ),
                (
                    "total_butterfly_disturbances",
                    "butterfly_disturb_count",
                ),
                ("total_poop_count", "poop_count"),
            ]

            for total_key, src_key in stat_mappings:
                val = pr.get(src_key, 0)
                if val:
                    placeholder = f":{total_key}"
                    add_parts.append(
                        f"{total_key} {placeholder}"
                    )
                    attr_values[placeholder] = val

            # Float stats use SET with if_not_exists.
            float_stat_mappings = [
                (
                    "total_crown_time_sec",
                    "crown_time_sec",
                ),
                ("total_ice_count", "ice_time_sec"),
                (
                    "total_fly_proximity_time_sec",
                    "fly_proximity_time_sec",
                ),
            ]

            float_set_parts = []
            for total_key, src_key in float_stat_mappings:
                val = pr.get(src_key, 0)
                if val:
                    placeholder = f":{total_key}"
                    attr_values[placeholder] = Decimal(
                        str(round(val, 2))
                    )
                    float_set_parts.append(
                        f"{total_key} ="
                        f" if_not_exists({total_key},"
                        f" :zero) + {placeholder}"
                    )

            add_expr = ""
            if add_parts:
                add_expr = " ADD " + ", ".join(add_parts)

            set_parts = [
                "matches_played ="
                " matches_played + :one",
                win_expr,
                "rating = :new_rating",
                "last_active = :now",
                "rating_partition = :all",
                "total_time_played_sec ="
                " if_not_exists("
                "total_time_played_sec, :zero)"
                " + :duration",
                "last_play_time = :now",
                "updated_at = :now",
            ]
            set_parts.extend(float_set_parts)

            update_expr = (
                "SET " + ", ".join(set_parts)
            )
            if add_parts:
                update_expr += (
                    " ADD " + ", ".join(add_parts)
                )

            self.players_table.update_item(
                Key={"player_id": player_id},
                UpdateExpression=update_expr,
                ExpressionAttributeValues=attr_values,
            )

            # Update leaderboard entries.
            display_name = pr.get("display_name", "")
            profile_image_url = ""
            if not display_name:
                # Fetch from DB if not in result.
                # Also check is_anonymous to skip
                # leaderboard writes for anon players.
                player_item = (
                    self.players_table.get_item(
                        Key={"player_id": player_id},
                        ProjectionExpression=(
                            "display_name,"
                            " profile_image_url,"
                            " is_anonymous"
                        ),
                    )
                )
                db_item = player_item.get("Item", {})
                if db_item.get("is_anonymous", False):
                    continue
                display_name = db_item.get(
                    "display_name", ""
                )
                profile_image_url = db_item.get(
                    "profile_image_url", ""
                )
            try:
                lb_ids = build_leaderboard_ids(level_id)
                for lb_id in lb_ids:
                    self.leaderboard_service.update_score(
                        leaderboard_id=lb_id,
                        player_id=player_id,
                        old_rating=player_rating,
                        new_rating=new_rating,
                        display_name=display_name,
                        profile_image_url=(
                            profile_image_url
                        ),
                    )
            except Exception:
                logger.exception(
                    "Leaderboard update failed for %s",
                    player_id,
                )

    def get_recent_matches(
        self,
        player_id: str,
        limit: int = _DEFAULT_HISTORY_LIMIT,
    ) -> list[dict]:
        """Query recent match history for a player."""
        response = self.history_table.query(
            KeyConditionExpression=Key("player_id").eq(
                player_id
            ),
            ScanIndexForward=False,
            Limit=limit,
        )
        items = response.get("Items", [])
        # Convert Decimal to float/int for JSON.
        return [self._convert_decimals(item) for item in items]

    def get_leaderboard(
        self,
        limit: int = _DEFAULT_LEADERBOARD_LIMIT,
    ) -> list[dict]:
        """Query top players by rating."""
        capped_limit = min(limit, _MAX_LEADERBOARD_LIMIT)
        response = self.players_table.query(
            IndexName="rating-index",
            KeyConditionExpression=Key(
                "rating_partition"
            ).eq("all"),
            ScanIndexForward=False,
            Limit=capped_limit,
            ProjectionExpression=(
                "player_id, display_name, rating,"
                " matches_played, wins, losses,"
                " profile_image_url"
            ),
        )
        items = response.get("Items", [])
        result = []
        for rank, item in enumerate(items, start=1):
            result.append(
                {
                    "rank": rank,
                    "player_id": item["player_id"],
                    "display_name": item.get(
                        "display_name", ""
                    ),
                    "rating": int(item.get("rating", 1500)),
                    "matches_played": int(
                        item.get("matches_played", 0)
                    ),
                    "wins": int(item.get("wins", 0)),
                    "losses": int(item.get("losses", 0)),
                    "profile_image_url": item.get(
                        "profile_image_url", ""
                    ),
                }
            )
        return result

    def get_player_rank(
        self, player_id: str, rating: int
    ) -> int:
        """Count players with rating higher than given value.

        Returns 1-based rank.
        """
        response = self.players_table.query(
            IndexName="rating-index",
            KeyConditionExpression=(
                Key("rating_partition").eq("all")
                & Key("rating").gt(rating)
            ),
            Select="COUNT",
        )
        return response.get("Count", 0) + 1

    @staticmethod
    def _calculate_rating_delta(
        player_rating: int,
        avg_opponent_rating: int,
        is_win: bool,
    ) -> int:
        """Calculate Elo rating change.

        Uses standard Elo formula with K=32.
        """
        expected = 1.0 / (
            1.0
            + math.pow(
                10,
                (avg_opponent_rating - player_rating)
                / 400.0,
            )
        )
        actual = 1.0 if is_win else 0.0
        return round(_ELO_K * (actual - expected))

    @staticmethod
    def _convert_decimals(
        item: dict,
    ) -> dict:
        """Convert Decimal values to int or float."""
        result = {}
        for key, value in item.items():
            if isinstance(value, Decimal):
                if value == int(value):
                    result[key] = int(value)
                else:
                    result[key] = float(value)
            else:
                result[key] = value
        return result
