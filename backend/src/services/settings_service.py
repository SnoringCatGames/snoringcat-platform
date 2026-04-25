"""Settings service for cloud-synced player settings."""

import os
from datetime import datetime
from typing import Optional

import boto3


class SettingsService:
    """DynamoDB operations for player settings."""

    def __init__(self):
        self.dynamodb = boto3.resource("dynamodb")
        self.table_name = os.environ.get(
            "SETTINGS_TABLE", "hopnbop-settings"
        )
        self.table = self.dynamodb.Table(self.table_name)

    def get_settings(
        self, player_id: str
    ) -> Optional[dict]:
        """Retrieve cloud settings for a player.

        Returns the full item dict or None.
        """
        response = self.table.get_item(
            Key={"player_id": player_id}
        )
        if "Item" not in response:
            return None
        item = response["Item"]
        return {
            "settings": item.get("settings", {}),
            "updated_at": int(
                item.get("updated_at", 0)
            ),
        }

    def save_settings(
        self,
        player_id: str,
        settings: dict,
        updated_at: int = 0,
    ) -> None:
        """Save settings blob to DynamoDB."""
        if not updated_at:
            updated_at = int(
                datetime.now().timestamp()
            )
        self.table.put_item(
            Item={
                "player_id": player_id,
                "settings": settings,
                "updated_at": updated_at,
            }
        )

    def delete_settings(self, player_id: str) -> None:
        """Delete settings for a player."""
        self.table.delete_item(
            Key={"player_id": player_id}
        )
