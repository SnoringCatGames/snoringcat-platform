"""Per-game profile service.

Reads and writes the `game_profiles` table — one row per
(player_id, game_id) pair. Holds per-game stats, rating,
matches_played/wins/losses, and an optional per-game display_name
override on top of the global default in the accounts table.

Game-specific stat fields (e.g. Hop 'n Bop's snail crushes,
cricket disturbances) live inside the `game_stats` JSON blob so
this service stays game-agnostic. Games read/write their stat
schemas client-side or via a per-game stats overlay; the platform
just persists the blob unchanged.
"""

import os
from dataclasses import dataclass, field
from datetime import datetime
from decimal import Decimal
from typing import Optional

import boto3
from boto3.dynamodb.conditions import Key


# Default starting rating for a new per-game profile.
_DEFAULT_RATING = 1500


@dataclass
class GameProfile:
    """A row from the `game_profiles` table."""

    player_id: str
    game_id: str
    # Per-game display_name override. Empty string means
    # "fall back to the account's default display_name".
    display_name: str = ""
    rating: int = _DEFAULT_RATING
    matches_played: int = 0
    wins: int = 0
    losses: int = 0
    first_played: int = 0
    last_played: int = 0
    total_time_played_sec: float = 0.0
    # Game-specific stats live here as a free-form dict so the
    # platform stays game-agnostic. Hop 'n Bop, for example,
    # stores total_kills, total_bumps, total_snail_crushes, etc.
    game_stats: dict = field(default_factory=dict)
    created_at: int = 0
    updated_at: int = 0


class ProfileService:
    """DynamoDB ops on the `game_profiles` table."""

    def __init__(self):
        self.dynamodb = boto3.resource("dynamodb")
        self.table_name = os.environ.get(
            "GAME_PROFILES_TABLE",
            "snoringcat-game-profiles",
        )
        self.table = self.dynamodb.Table(self.table_name)

    async def get(
        self, player_id: str, game_id: str
    ) -> Optional[GameProfile]:
        """Fetch a profile for one (player, game) pair."""
        response = self.table.get_item(
            Key={
                "player_id": player_id,
                "game_id": game_id,
            }
        )
        item = response.get("Item")
        if item is None:
            return None
        return self._from_item(item)

    async def list_for_player(
        self, player_id: str
    ) -> list[GameProfile]:
        """All game profiles for one player.

        Used by the account-export (GDPR) endpoint and by any UI
        that wants to show "games this player has played."
        """
        response = self.table.query(
            KeyConditionExpression=Key("player_id").eq(
                player_id
            )
        )
        return [
            self._from_item(item)
            for item in response.get("Items", [])
        ]

    async def create(
        self,
        player_id: str,
        game_id: str,
        display_name: str = "",
        game_stats: Optional[dict] = None,
    ) -> GameProfile:
        """Create a new per-game profile row."""
        now = int(datetime.now().timestamp())
        profile = GameProfile(
            player_id=player_id,
            game_id=game_id,
            display_name=display_name,
            game_stats=game_stats or {},
            first_played=now,
            last_played=now,
            created_at=now,
            updated_at=now,
        )

        item = {
            "player_id": profile.player_id,
            "game_id": profile.game_id,
            "rating": profile.rating,
            "matches_played": profile.matches_played,
            "wins": profile.wins,
            "losses": profile.losses,
            "first_played": profile.first_played,
            "last_played": profile.last_played,
            "total_time_played_sec": Decimal(
                str(profile.total_time_played_sec)
            ),
            "game_stats": profile.game_stats,
            "created_at": profile.created_at,
            "updated_at": profile.updated_at,
            # Composite GSI partition for the rating-index leaderboard.
            # Tier-based segmentation can extend this later
            # (e.g. "{game_id}#{tier}"); for now everyone shares
            # one partition per game.
            "rating_partition": f"{game_id}#all",
        }
        if display_name:
            item["display_name"] = display_name

        self.table.put_item(Item=item)
        return profile

    async def get_or_create(
        self,
        player_id: str,
        game_id: str,
        display_name: str = "",
    ) -> GameProfile:
        """Fetch an existing profile or create a fresh one."""
        existing = await self.get(player_id, game_id)
        if existing is not None:
            return existing
        return await self.create(
            player_id, game_id, display_name=display_name
        )

    async def update_display_name(
        self, player_id: str, game_id: str, display_name: str
    ) -> None:
        """Update the per-game display name override.

        Pass an empty string to clear the override (the account's
        default display_name applies when the override is absent
        or empty).
        """
        if display_name:
            self.table.update_item(
                Key={
                    "player_id": player_id,
                    "game_id": game_id,
                },
                UpdateExpression=(
                    "SET display_name = :n, updated_at = :t"
                ),
                ExpressionAttributeValues={
                    ":n": display_name,
                    ":t": int(datetime.now().timestamp()),
                },
            )
        else:
            self.table.update_item(
                Key={
                    "player_id": player_id,
                    "game_id": game_id,
                },
                UpdateExpression=(
                    "REMOVE display_name "
                    "SET updated_at = :t"
                ),
                ExpressionAttributeValues={
                    ":t": int(datetime.now().timestamp()),
                },
            )

    async def update_after_match(
        self,
        player_id: str,
        game_id: str,
        rating_delta: int,
        won: bool,
    ) -> None:
        """Apply a single match's result to the profile.

        Bumps matches_played, wins or losses, last_played, and
        rating by the supplied delta.
        """
        now = int(datetime.now().timestamp())
        result_field = "wins" if won else "losses"
        self.table.update_item(
            Key={
                "player_id": player_id,
                "game_id": game_id,
            },
            UpdateExpression=(
                "ADD matches_played :one, "
                f"{result_field} :one, "
                "rating :delta "
                "SET last_played = :t, updated_at = :t"
            ),
            ExpressionAttributeValues={
                ":one": 1,
                ":delta": rating_delta,
                ":t": now,
            },
        )

    async def merge_game_stats(
        self,
        player_id: str,
        game_id: str,
        delta_stats: dict,
    ) -> None:
        """Add `delta_stats` into the existing game_stats blob.

        Each numeric key in `delta_stats` is added to the existing
        value (creating it if absent). Non-numeric values overwrite.
        Used by per-game match-result reporting to roll up totals.
        """
        if not delta_stats:
            return
        existing = await self.get(player_id, game_id)
        merged = dict(existing.game_stats) if existing else {}
        for key, value in delta_stats.items():
            if isinstance(value, (int, float)) and isinstance(
                merged.get(key, 0), (int, float, Decimal)
            ):
                merged[key] = (
                    type(value)(merged.get(key, 0)) + value
                )
            else:
                merged[key] = value
        self.table.update_item(
            Key={
                "player_id": player_id,
                "game_id": game_id,
            },
            UpdateExpression=(
                "SET game_stats = :s, updated_at = :t"
            ),
            ExpressionAttributeValues={
                ":s": merged,
                ":t": int(datetime.now().timestamp()),
            },
        )

    async def delete_all_for_player(
        self, player_id: str
    ) -> int:
        """Delete every game_profiles row for one player.

        Used by the GDPR account-deletion flow. Returns the number
        of rows deleted.
        """
        profiles = await self.list_for_player(player_id)
        with self.table.batch_writer() as batch:
            for p in profiles:
                batch.delete_item(
                    Key={
                        "player_id": p.player_id,
                        "game_id": p.game_id,
                    }
                )
        return len(profiles)

    @staticmethod
    def _from_item(item: dict) -> GameProfile:
        return GameProfile(
            player_id=item["player_id"],
            game_id=item["game_id"],
            display_name=item.get("display_name", ""),
            rating=int(
                item.get("rating", _DEFAULT_RATING)
            ),
            matches_played=int(
                item.get("matches_played", 0)
            ),
            wins=int(item.get("wins", 0)),
            losses=int(item.get("losses", 0)),
            first_played=int(item.get("first_played", 0)),
            last_played=int(item.get("last_played", 0)),
            total_time_played_sec=float(
                item.get("total_time_played_sec", 0)
            ),
            game_stats=dict(item.get("game_stats", {})),
            created_at=int(item.get("created_at", 0)),
            updated_at=int(item.get("updated_at", 0)),
        )
