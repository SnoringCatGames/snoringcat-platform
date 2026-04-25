"""Player service for DynamoDB operations."""

import os
import secrets
import uuid
import boto3
import bcrypt
from decimal import Decimal
from typing import Optional
from dataclasses import dataclass, field
from datetime import datetime, timedelta

from boto3.dynamodb.conditions import Key


# Refresh tokens expire after 30 days.
_REFRESH_TOKEN_DAYS = 30

# Maximum number of concurrent refresh tokens per player
# (one per device/session).
_MAX_REFRESH_TOKENS = 5

# Friend code length and max generation retries.
_FRIEND_CODE_LENGTH = 6
_FRIEND_CODE_MAX_RETRIES = 5


@dataclass
class PlayerProfile:
    """Player profile data."""

    player_id: str
    display_name: str
    rating: int
    matches_played: int
    wins: int
    losses: int
    created_at: int
    last_active: int
    auth_providers: dict = field(default_factory=dict)
    is_anonymous: bool = False
    device_id: str = ""
    consent_accepted_at: int = 0
    consent_legal_version: str = ""
    friend_code: str = ""
    first_play_time: int = 0
    last_play_time: int = 0
    total_time_played_sec: float = 0.0
    updated_at: int = 0
    total_kills: int = 0
    total_deaths: int = 0
    total_bumps: int = 0
    total_crown_time_sec: float = 0.0
    total_regicide_count: int = 0
    total_jumps: int = 0
    total_water_count: int = 0
    total_ice_count: int = 0
    total_spring_count: int = 0
    total_direction_changes: int = 0
    total_snail_crushes: int = 0
    total_cricket_disturbances: int = 0
    total_fish_disturbances: int = 0
    total_butterfly_disturbances: int = 0
    total_fly_proximity_time_sec: float = 0.0
    total_poop_count: int = 0
    profile_image_url: str = ""
    # Per-provider display names and profile images.
    # Keys are provider strings (e.g. "google", "steam").
    provider_display_names: dict = field(
        default_factory=dict
    )
    provider_profile_images: dict = field(
        default_factory=dict
    )


class PlayerService:
    """DynamoDB player operations."""

    def __init__(self):
        self.dynamodb = boto3.resource("dynamodb")
        self.table_name = os.environ.get(
            "PLAYERS_TABLE", "hopnbop-players"
        )
        self.table = self.dynamodb.Table(self.table_name)

    @staticmethod
    def generate_player_id() -> str:
        """Generate a stable UUID-based player ID."""
        return f"p_{uuid.uuid4().hex[:12]}"

    async def get_player(
        self, player_id: str
    ) -> Optional[PlayerProfile]:
        """Retrieve player profile."""
        response = self.table.get_item(
            Key={"player_id": player_id}
        )

        if "Item" not in response:
            return None

        item = response["Item"]
        return PlayerProfile(
            player_id=item["player_id"],
            display_name=item["display_name"],
            rating=int(item.get("rating", 1500)),
            matches_played=int(
                item.get("matches_played", 0)
            ),
            wins=int(item.get("wins", 0)),
            losses=int(item.get("losses", 0)),
            created_at=int(item.get("created_at", 0)),
            last_active=int(
                item.get("last_active", 0)
            ),
            auth_providers=item.get("auth_providers", {}),
            is_anonymous=item.get("is_anonymous", False),
            device_id=item.get("device_id", ""),
            consent_accepted_at=int(
                item.get("consent_accepted_at", 0)
            ),
            consent_legal_version=item.get(
                "consent_legal_version", ""
            ),
            friend_code=item.get("friend_code", ""),
            first_play_time=int(
                item.get("first_play_time", 0)
            ),
            last_play_time=int(
                item.get("last_play_time", 0)
            ),
            total_time_played_sec=float(
                item.get("total_time_played_sec", 0)
            ),
            updated_at=int(item.get("updated_at", 0)),
            total_kills=int(
                item.get("total_kills", 0)
            ),
            total_deaths=int(
                item.get("total_deaths", 0)
            ),
            total_bumps=int(
                item.get("total_bumps", 0)
            ),
            total_crown_time_sec=float(
                item.get("total_crown_time_sec", 0)
            ),
            total_regicide_count=int(
                item.get("total_regicide_count", 0)
            ),
            total_jumps=int(
                item.get("total_jumps", 0)
            ),
            total_water_count=int(
                item.get("total_water_count", 0)
            ),
            total_ice_count=int(
                item.get("total_ice_count", 0)
            ),
            total_spring_count=int(
                item.get("total_spring_count", 0)
            ),
            total_direction_changes=int(
                item.get("total_direction_changes", 0)
            ),
            total_snail_crushes=int(
                item.get("total_snail_crushes", 0)
            ),
            total_cricket_disturbances=int(
                item.get("total_cricket_disturbances", 0)
            ),
            total_fish_disturbances=int(
                item.get("total_fish_disturbances", 0)
            ),
            total_butterfly_disturbances=int(
                item.get(
                    "total_butterfly_disturbances", 0
                )
            ),
            total_fly_proximity_time_sec=float(
                item.get(
                    "total_fly_proximity_time_sec", 0
                )
            ),
            total_poop_count=int(
                item.get("total_poop_count", 0)
            ),
            profile_image_url=item.get(
                "profile_image_url", ""
            ),
            provider_display_names=item.get(
                "provider_display_names", {}
            ),
            provider_profile_images=item.get(
                "provider_profile_images", {}
            ),
        )

    @staticmethod
    def _generate_friend_code() -> str:
        """Generate a random 6-character uppercase alphanumeric code."""
        alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
        return "".join(
            secrets.choice(alphabet)
            for _ in range(_FRIEND_CODE_LENGTH)
        )

    async def create_player(
        self,
        player_id: str,
        display_name: str,
        auth_providers: dict,
        is_anonymous: bool = False,
        device_id: str = "",
        consent_accepted_at: int = 0,
        consent_legal_version: str = "",
        profile_image_url: str = "",
        provider_display_names: dict = None,
        provider_profile_images: dict = None,
    ) -> PlayerProfile:
        """Create new player profile."""
        now = int(datetime.now().timestamp())
        friend_code = self._generate_friend_code()

        pdn = provider_display_names or {}
        ppi = provider_profile_images or {}
        profile = PlayerProfile(
            player_id=player_id,
            display_name=display_name,
            rating=1500,
            matches_played=0,
            wins=0,
            losses=0,
            created_at=now,
            last_active=now,
            auth_providers=auth_providers,
            is_anonymous=is_anonymous,
            device_id=device_id,
            consent_accepted_at=consent_accepted_at,
            consent_legal_version=consent_legal_version,
            friend_code=friend_code,
            first_play_time=now,
            updated_at=now,
            profile_image_url=profile_image_url,
            provider_display_names=pdn,
            provider_profile_images=ppi,
        )

        item = {
            "player_id": profile.player_id,
            "display_name": profile.display_name,
            "rating": profile.rating,
            "matches_played": profile.matches_played,
            "wins": profile.wins,
            "losses": profile.losses,
            "created_at": profile.created_at,
            "last_active": profile.last_active,
            "auth_providers": profile.auth_providers,
            "is_anonymous": profile.is_anonymous,
            "rating_partition": "all",
            "friend_code": profile.friend_code,
            "first_play_time": profile.first_play_time,
            "updated_at": profile.updated_at,
            "provider_display_names": pdn,
            "provider_profile_images": ppi,
        }
        if device_id:
            item["device_id"] = device_id
        if profile_image_url:
            item["profile_image_url"] = profile_image_url
        if consent_accepted_at:
            item["consent_accepted_at"] = (
                consent_accepted_at
            )
            item["consent_legal_version"] = (
                consent_legal_version
            )

        self.table.put_item(Item=item)
        return profile

    async def update_profile_image_url(
        self, player_id: str, profile_image_url: str
    ) -> None:
        """Update player's profile image URL."""
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "SET profile_image_url = :url"
            ),
            ExpressionAttributeValues={
                ":url": profile_image_url,
            },
        )

    async def update_last_active(
        self, player_id: str
    ) -> None:
        """Update player's last active timestamp."""
        now = int(datetime.now().timestamp())
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression="SET last_active = :now",
            ExpressionAttributeValues={":now": now},
        )

    async def add_provider(
        self,
        player_id: str,
        provider: str,
        provider_id: str,
        display_name: str = "",
        profile_image_url: str = "",
    ) -> None:
        """Add a provider to a player's auth_providers map.

        Optionally stores the provider-specific display name
        and profile image URL in per-provider maps.
        Initializes those maps if they do not exist yet.
        """
        # Ensure the per-provider display maps exist on
        # this item (no-op for players created after the
        # initial migration).
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "SET provider_display_names"
                " = if_not_exists("
                "provider_display_names, :em),"
                " provider_profile_images"
                " = if_not_exists("
                "provider_profile_images, :em)"
            ),
            ExpressionAttributeValues={":em": {}},
        )

        expr = (
            "SET auth_providers.#prov = :pid,"
            " is_anonymous = :false"
        )
        attr_values: dict = {
            ":pid": provider_id,
            ":false": False,
        }
        if display_name:
            expr += (
                ", provider_display_names.#prov = :dn"
                ", display_name = :dn"
            )
            attr_values[":dn"] = display_name
        if profile_image_url:
            expr += (
                ", provider_profile_images.#prov = :pi"
                ", profile_image_url = :pi"
            )
            attr_values[":pi"] = profile_image_url

        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=expr,
            ExpressionAttributeNames={"#prov": provider},
            ExpressionAttributeValues=attr_values,
        )

    async def remove_provider(
        self,
        player_id: str,
        provider: str,
    ) -> None:
        """Remove a provider from a player's auth_providers map."""
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "REMOVE auth_providers.#prov"
            ),
            ExpressionAttributeNames={"#prov": provider},
        )

    async def get_or_create_player(
        self,
        player_id: str,
        display_name: str,
        auth_providers: dict,
        is_anonymous: bool = False,
        device_id: str = "",
        consent_accepted_at: int = 0,
        consent_legal_version: str = "",
        profile_image_url: str = "",
    ) -> PlayerProfile:
        """Get existing player or create if not exists."""
        profile = await self.get_player(player_id)

        if profile is None:
            profile = await self.create_player(
                player_id,
                display_name,
                auth_providers,
                is_anonymous=is_anonymous,
                device_id=device_id,
                consent_accepted_at=consent_accepted_at,
                consent_legal_version=consent_legal_version,
                profile_image_url=profile_image_url,
            )
        else:
            # Refresh per-provider display info on each
            # login in case the user changed their name
            # or avatar on the platform.
            if auth_providers:
                provider = next(iter(auth_providers))
                await self.add_provider(
                    player_id,
                    provider,
                    auth_providers[provider],
                    display_name=display_name,
                    profile_image_url=profile_image_url,
                )
                if profile_image_url:
                    profile.profile_image_url = (
                        profile_image_url
                    )
            elif profile_image_url:
                await self.update_profile_image_url(
                    player_id, profile_image_url
                )
                profile.profile_image_url = (
                    profile_image_url
                )
            if consent_accepted_at > 0:
                await self.store_consent(
                    player_id,
                    consent_accepted_at,
                    consent_legal_version,
                )
                profile.consent_accepted_at = (
                    consent_accepted_at
                )
                profile.consent_legal_version = (
                    consent_legal_version
                )

        await self.update_last_active(player_id)
        return profile

    async def store_consent(
        self,
        player_id: str,
        consent_accepted_at: int,
        consent_legal_version: str,
    ) -> None:
        """Store consent timestamp and legal version."""
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "SET consent_accepted_at = :cat,"
                " consent_legal_version = :ver"
            ),
            ExpressionAttributeValues={
                ":cat": consent_accepted_at,
                ":ver": consent_legal_version,
            },
        )

    # --- Refresh token methods ---

    async def store_refresh_token(
        self,
        player_id: str,
        refresh_token: str,
    ) -> int:
        """Hash and append a refresh token. Returns expiry
        timestamp. Keeps up to _MAX_REFRESH_TOKENS entries,
        evicting the oldest when full. Expired entries are
        pruned on every write."""
        token_hash = bcrypt.hashpw(
            refresh_token.encode(), bcrypt.gensalt()
        ).decode()
        now = int(datetime.now().timestamp())
        expires_at = int(
            (
                datetime.now()
                + timedelta(days=_REFRESH_TOKEN_DAYS)
            ).timestamp()
        )

        # Read existing tokens.
        response = self.table.get_item(
            Key={"player_id": player_id},
            ProjectionExpression="refresh_tokens",
        )
        item = response.get("Item", {})
        tokens: list = item.get("refresh_tokens", [])

        # Prune expired entries.
        tokens = [
            t for t in tokens
            if int(t.get("expires_at", 0)) > now
        ]

        # Append new entry.
        tokens.append({
            "hash": token_hash,
            "expires_at": expires_at,
        })

        # Evict oldest if over the cap.
        if len(tokens) > _MAX_REFRESH_TOKENS:
            tokens = tokens[-_MAX_REFRESH_TOKENS:]

        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression="SET refresh_tokens = :tokens",
            ExpressionAttributeValues={
                ":tokens": tokens,
            },
        )
        return expires_at

    async def verify_refresh_token(
        self, player_id: str, refresh_token: str
    ) -> bool:
        """Verify a refresh token against all stored
        hashes. Returns True if any non-expired entry
        matches."""
        response = self.table.get_item(
            Key={"player_id": player_id},
            ProjectionExpression="refresh_tokens",
        )
        item = response.get("Item")
        if not item:
            return False

        tokens: list = item.get("refresh_tokens", [])
        if not tokens:
            return False

        now = int(datetime.now().timestamp())
        encoded = refresh_token.encode()
        for entry in tokens:
            expires_at = int(entry.get("expires_at", 0))
            if now >= expires_at:
                continue
            stored_hash = entry.get("hash", "")
            if not stored_hash:
                continue
            if bcrypt.checkpw(
                encoded, stored_hash.encode()
            ):
                return True
        return False

    async def rotate_refresh_token(
        self,
        player_id: str,
        old_refresh_token: str,
        new_refresh_token: str,
    ) -> int:
        """Atomically remove the old refresh token and
        store a new one.  Returns expiry timestamp."""
        # Read existing tokens.
        response = self.table.get_item(
            Key={"player_id": player_id},
            ProjectionExpression="refresh_tokens",
        )
        item = response.get("Item", {})
        tokens: list = item.get("refresh_tokens", [])

        now = int(datetime.now().timestamp())
        encoded_old = old_refresh_token.encode()

        # Remove old token and prune expired entries.
        kept: list = []
        for t in tokens:
            if int(t.get("expires_at", 0)) <= now:
                continue
            stored_hash = t.get("hash", "")
            if stored_hash and bcrypt.checkpw(
                encoded_old, stored_hash.encode()
            ):
                continue  # drop the old token
            kept.append(t)

        # Append new token.
        token_hash = bcrypt.hashpw(
            new_refresh_token.encode(), bcrypt.gensalt()
        ).decode()
        expires_at = int(
            (
                datetime.now()
                + timedelta(days=_REFRESH_TOKEN_DAYS)
            ).timestamp()
        )
        kept.append({
            "hash": token_hash,
            "expires_at": expires_at,
        })

        # Evict oldest if over the cap.
        if len(kept) > _MAX_REFRESH_TOKENS:
            kept = kept[-_MAX_REFRESH_TOKENS:]

        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "SET refresh_tokens = :tokens"
            ),
            ExpressionAttributeValues={
                ":tokens": kept,
            },
        )
        return expires_at

    async def clear_refresh_token(
        self, player_id: str
    ) -> None:
        """Remove all stored refresh tokens."""
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression="REMOVE refresh_tokens",
        )

    async def get_player_by_friend_code(
        self, friend_code: str
    ) -> Optional[PlayerProfile]:
        """Look up a player by their friend code using the GSI."""
        response = self.table.query(
            IndexName="friend-code-index",
            KeyConditionExpression=Key("friend_code").eq(
                friend_code
            ),
            Limit=1,
        )
        items = response.get("Items", [])
        if not items:
            return None
        # GSI is KEYS_ONLY so we need a full get_item.
        return await self.get_player(
            items[0]["player_id"]
        )

    async def merge_players(
        self,
        primary_id: str,
        secondary_id: str,
    ) -> Optional[PlayerProfile]:
        """Merge the secondary player into the primary.

        Stats are combined (sums for counters, max for
        rating, min/max for timestamps). All providers
        from the secondary are added to the primary.
        The secondary record is NOT deleted here; the
        caller is responsible for cleanup.

        Returns the updated primary profile, or None if
        either player is not found.
        """
        primary = await self.get_player(primary_id)
        secondary = await self.get_player(secondary_id)

        if primary is None or secondary is None:
            return None

        # Merge auth providers (primary takes precedence).
        merged_providers = {
            **secondary.auth_providers,
            **primary.auth_providers,
        }
        merged_display_names = {
            **secondary.provider_display_names,
            **primary.provider_display_names,
        }
        merged_profile_images = {
            **secondary.provider_profile_images,
            **primary.provider_profile_images,
        }

        # Merge scalar fields.
        merged_rating = max(
            primary.rating, secondary.rating
        )
        merged_created_at = min(
            primary.created_at, secondary.created_at
        )
        p_first = primary.first_play_time
        s_first = secondary.first_play_time
        first_times = [t for t in [p_first, s_first] if t]
        merged_first_play_time = (
            min(first_times) if first_times else 0
        )
        merged_last_play_time = max(
            primary.last_play_time, secondary.last_play_time
        )
        merged_last_active = max(
            primary.last_active, secondary.last_active
        )
        if (
            secondary.consent_accepted_at
            > primary.consent_accepted_at
        ):
            merged_consent_at = (
                secondary.consent_accepted_at
            )
            merged_consent_version = (
                secondary.consent_legal_version
            )
        else:
            merged_consent_at = primary.consent_accepted_at
            merged_consent_version = (
                primary.consent_legal_version
            )
        merged_device_id = (
            primary.device_id or secondary.device_id
        )
        merged_image_url = (
            primary.profile_image_url
            or secondary.profile_image_url
        )

        now = int(datetime.now().timestamp())
        self.table.update_item(
            Key={"player_id": primary_id},
            UpdateExpression=(
                "SET rating = :r,"
                " matches_played = :mp,"
                " wins = :w,"
                " losses = :l,"
                " total_kills = :tk,"
                " total_deaths = :td,"
                " total_bumps = :tb,"
                " total_crown_time_sec = :tct,"
                " total_regicide_count = :trc,"
                " total_jumps = :tj,"
                " total_water_count = :twc,"
                " total_ice_count = :tic,"
                " total_spring_count = :tsc,"
                " total_direction_changes = :tdc,"
                " total_snail_crushes = :tsn,"
                " total_cricket_disturbances = :tcr,"
                " total_fish_disturbances = :tfd,"
                " total_butterfly_disturbances = :tbd,"
                " total_fly_proximity_time_sec = :tfp,"
                " total_poop_count = :tpc,"
                " total_time_played_sec = :ttp,"
                " auth_providers = :ap,"
                " provider_display_names = :pdn,"
                " provider_profile_images = :ppi,"
                " is_anonymous = :false,"
                " device_id = :did,"
                " profile_image_url = :piu,"
                " created_at = :ca,"
                " first_play_time = :fpt,"
                " last_play_time = :lpt,"
                " last_active = :la,"
                " consent_accepted_at = :cat,"
                " consent_legal_version = :cv,"
                " updated_at = :now"
            ),
            ExpressionAttributeValues={
                ":r": merged_rating,
                ":mp": (
                    primary.matches_played
                    + secondary.matches_played
                ),
                ":w": primary.wins + secondary.wins,
                ":l": (
                    primary.losses + secondary.losses
                ),
                ":tk": (
                    primary.total_kills
                    + secondary.total_kills
                ),
                ":td": (
                    primary.total_deaths
                    + secondary.total_deaths
                ),
                ":tb": (
                    primary.total_bumps
                    + secondary.total_bumps
                ),
                ":tct": Decimal(str(
                    primary.total_crown_time_sec
                    + secondary.total_crown_time_sec
                )),
                ":trc": (
                    primary.total_regicide_count
                    + secondary.total_regicide_count
                ),
                ":tj": (
                    primary.total_jumps
                    + secondary.total_jumps
                ),
                ":twc": (
                    primary.total_water_count
                    + secondary.total_water_count
                ),
                ":tic": (
                    primary.total_ice_count
                    + secondary.total_ice_count
                ),
                ":tsc": (
                    primary.total_spring_count
                    + secondary.total_spring_count
                ),
                ":tdc": (
                    primary.total_direction_changes
                    + secondary.total_direction_changes
                ),
                ":tsn": (
                    primary.total_snail_crushes
                    + secondary.total_snail_crushes
                ),
                ":tcr": (
                    primary.total_cricket_disturbances
                    + secondary.total_cricket_disturbances
                ),
                ":tfd": (
                    primary.total_fish_disturbances
                    + secondary.total_fish_disturbances
                ),
                ":tbd": (
                    primary.total_butterfly_disturbances
                    + secondary.total_butterfly_disturbances
                ),
                ":tfp": Decimal(str(
                    primary.total_fly_proximity_time_sec
                    + secondary.total_fly_proximity_time_sec
                )),
                ":tpc": (
                    primary.total_poop_count
                    + secondary.total_poop_count
                ),
                ":ttp": Decimal(str(
                    primary.total_time_played_sec
                    + secondary.total_time_played_sec
                )),
                ":ap": merged_providers,
                ":pdn": merged_display_names,
                ":ppi": merged_profile_images,
                ":false": False,
                ":did": merged_device_id,
                ":piu": merged_image_url,
                ":ca": merged_created_at,
                ":fpt": merged_first_play_time,
                ":lpt": merged_last_play_time,
                ":la": merged_last_active,
                ":cat": merged_consent_at,
                ":cv": merged_consent_version,
                ":now": now,
            },
        )

        return await self.get_player(primary_id)

    async def delete_player(self, player_id: str) -> None:
        """Delete a player record entirely."""
        self.table.delete_item(
            Key={"player_id": player_id}
        )
