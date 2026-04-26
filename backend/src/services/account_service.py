"""Global account service.

Reads and writes the `accounts` table — the cross-game half of what
the legacy `player_service` did. An account row is shared across
every Snoring Cat game; per-game stats and the per-game display
name override live in the `game_profiles` table (see
profile_service.py).

This service is intended to coexist with player_service for the
duration of the migration. Handlers are migrated to use it one at
a time. While both exist, both write to the same DynamoDB table
(snoringcat-accounts) — account_service writes the strict
cross-game subset, player_service continues to write per-game
stats columns. Once every handler is migrated, player_service is
deleted and the per-game columns drop out of new writes.
"""

import os
import secrets
import uuid
from dataclasses import dataclass, field
from datetime import datetime
from typing import Optional

import boto3


# Friend code length and max generation retries (collision avoidance).
_FRIEND_CODE_LENGTH = 6
_FRIEND_CODE_MAX_RETRIES = 5


@dataclass
class AccountProfile:
    """A row from the `accounts` table.

    Holds only fields that span every game the player has touched.
    Per-game stats, ratings, and display-name overrides live in
    `game_profiles` rows keyed by (player_id, game_id).
    """

    player_id: str
    # Default display name shown in friend lists and friend-of-friend
    # lookups. Per-game UIs can show a per-game override read from
    # game_profiles[player_id, game_id].display_name.
    display_name: str
    friend_code: str = ""
    is_anonymous: bool = False
    device_id: str = ""
    profile_image_url: str = ""
    primary_locale: str = ""
    auth_providers: dict = field(default_factory=dict)
    provider_display_names: dict = field(
        default_factory=dict
    )
    provider_profile_images: dict = field(
        default_factory=dict
    )
    consent_accepted_at: int = 0
    consent_legal_version: str = ""
    created_at: int = 0
    updated_at: int = 0
    last_active: int = 0


class AccountService:
    """DynamoDB ops on the `accounts` table."""

    def __init__(self):
        self.dynamodb = boto3.resource("dynamodb")
        self.table_name = os.environ.get(
            "ACCOUNTS_TABLE", "snoringcat-accounts"
        )
        self.table = self.dynamodb.Table(self.table_name)

    @staticmethod
    def generate_player_id() -> str:
        """Generate a stable UUID-based player ID."""
        return f"p_{uuid.uuid4().hex[:12]}"

    @staticmethod
    def _generate_friend_code() -> str:
        """Random 6-character uppercase alphanumeric code."""
        alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
        return "".join(
            secrets.choice(alphabet)
            for _ in range(_FRIEND_CODE_LENGTH)
        )

    async def get(
        self, player_id: str
    ) -> Optional[AccountProfile]:
        """Retrieve an account by player_id."""
        response = self.table.get_item(
            Key={"player_id": player_id}
        )
        item = response.get("Item")
        if item is None:
            return None
        return self._from_item(item)

    async def create(
        self,
        player_id: str,
        display_name: str,
        auth_providers: Optional[dict] = None,
        is_anonymous: bool = False,
        device_id: str = "",
        consent_accepted_at: int = 0,
        consent_legal_version: str = "",
        profile_image_url: str = "",
        primary_locale: str = "",
        provider_display_names: Optional[dict] = None,
        provider_profile_images: Optional[dict] = None,
    ) -> AccountProfile:
        """Create a new account row.

        Caller is responsible for ensuring no row already exists at
        this player_id (use generate_player_id() for new accounts).
        Per-game profile creation is a separate call into
        ProfileService.create_for_game(player_id, game_id).
        """
        now = int(datetime.now().timestamp())
        friend_code = self._generate_friend_code()
        profile = AccountProfile(
            player_id=player_id,
            display_name=display_name,
            friend_code=friend_code,
            is_anonymous=is_anonymous,
            device_id=device_id,
            profile_image_url=profile_image_url,
            primary_locale=primary_locale,
            auth_providers=auth_providers or {},
            provider_display_names=provider_display_names or {},
            provider_profile_images=provider_profile_images or {},
            consent_accepted_at=consent_accepted_at,
            consent_legal_version=consent_legal_version,
            created_at=now,
            updated_at=now,
            last_active=now,
        )

        item = {
            "player_id": profile.player_id,
            "display_name": profile.display_name,
            "friend_code": profile.friend_code,
            "is_anonymous": profile.is_anonymous,
            "auth_providers": profile.auth_providers,
            "provider_display_names": (
                profile.provider_display_names
            ),
            "provider_profile_images": (
                profile.provider_profile_images
            ),
            "created_at": profile.created_at,
            "updated_at": profile.updated_at,
            "last_active": profile.last_active,
        }
        # Only write optional fields when populated, so we don't
        # leave unhelpful empty strings in the row.
        if device_id:
            item["device_id"] = device_id
        if profile_image_url:
            item["profile_image_url"] = profile_image_url
        if primary_locale:
            item["primary_locale"] = primary_locale
        if consent_accepted_at:
            item["consent_accepted_at"] = consent_accepted_at
            item["consent_legal_version"] = (
                consent_legal_version
            )

        self.table.put_item(Item=item)
        return profile

    async def update_display_name(
        self, player_id: str, display_name: str
    ) -> None:
        """Update the cross-game default display name."""
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "SET display_name = :n, updated_at = :t"
            ),
            ExpressionAttributeValues={
                ":n": display_name,
                ":t": int(datetime.now().timestamp()),
            },
        )

    async def update_profile_image_url(
        self, player_id: str, profile_image_url: str
    ) -> None:
        """Update profile image URL."""
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "SET profile_image_url = :url, "
                "updated_at = :t"
            ),
            ExpressionAttributeValues={
                ":url": profile_image_url,
                ":t": int(datetime.now().timestamp()),
            },
        )

    async def update_locale(
        self, player_id: str, locale: str
    ) -> None:
        """Update primary locale."""
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "SET primary_locale = :l, updated_at = :t"
            ),
            ExpressionAttributeValues={
                ":l": locale,
                ":t": int(datetime.now().timestamp()),
            },
        )

    async def update_consent(
        self,
        player_id: str,
        consent_accepted_at: int,
        consent_legal_version: str,
    ) -> None:
        """Record a consent acceptance."""
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "SET consent_accepted_at = :a, "
                "consent_legal_version = :v, "
                "updated_at = :t"
            ),
            ExpressionAttributeValues={
                ":a": consent_accepted_at,
                ":v": consent_legal_version,
                ":t": int(datetime.now().timestamp()),
            },
        )

    async def add_provider(
        self,
        player_id: str,
        provider: str,
        provider_id: str,
        display_name: str = "",
        profile_image_url: str = "",
    ) -> None:
        """Link an OAuth identity to this account."""
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "SET auth_providers.#p = :id, "
                "provider_display_names.#p = :dn, "
                "provider_profile_images.#p = :pi, "
                "updated_at = :t"
            ),
            ExpressionAttributeNames={"#p": provider},
            ExpressionAttributeValues={
                ":id": provider_id,
                ":dn": display_name,
                ":pi": profile_image_url,
                ":t": int(datetime.now().timestamp()),
            },
        )

    async def remove_provider(
        self, player_id: str, provider: str
    ) -> None:
        """Unlink an OAuth identity."""
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "REMOVE auth_providers.#p, "
                "provider_display_names.#p, "
                "provider_profile_images.#p "
                "SET updated_at = :t"
            ),
            ExpressionAttributeNames={"#p": provider},
            ExpressionAttributeValues={
                ":t": int(datetime.now().timestamp()),
            },
        )

    async def update_last_active(
        self, player_id: str
    ) -> None:
        """Bump last_active to now."""
        self.table.update_item(
            Key={"player_id": player_id},
            UpdateExpression="SET last_active = :t",
            ExpressionAttributeValues={
                ":t": int(datetime.now().timestamp()),
            },
        )

    async def delete(self, player_id: str) -> None:
        """Delete the account row.

        Per-game profile rows must be deleted separately by the
        caller (ProfileService.delete_all_for_player). Linked
        identity rows in `identities` table must also be removed
        separately by the caller.
        """
        self.table.delete_item(Key={"player_id": player_id})

    @staticmethod
    def _from_item(item: dict) -> AccountProfile:
        return AccountProfile(
            player_id=item["player_id"],
            display_name=item["display_name"],
            friend_code=item.get("friend_code", ""),
            is_anonymous=item.get("is_anonymous", False),
            device_id=item.get("device_id", ""),
            profile_image_url=item.get(
                "profile_image_url", ""
            ),
            primary_locale=item.get("primary_locale", ""),
            auth_providers=item.get("auth_providers", {}),
            provider_display_names=item.get(
                "provider_display_names", {}
            ),
            provider_profile_images=item.get(
                "provider_profile_images", {}
            ),
            consent_accepted_at=int(
                item.get("consent_accepted_at", 0)
            ),
            consent_legal_version=item.get(
                "consent_legal_version", ""
            ),
            created_at=int(item.get("created_at", 0)),
            updated_at=int(item.get("updated_at", 0)),
            last_active=int(item.get("last_active", 0)),
        )
