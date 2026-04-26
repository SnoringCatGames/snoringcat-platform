"""Online-status service.

Legacy heartbeat tracker on the players/accounts table. Tracks
only "has the player heartbeated recently" — there is no game_id
or rich presence here. The richer cross-game `presence_service`
in the same package is the new home for those concepts.

This file is kept under the renamed name `OnlineStatusService`
during the migration window so the existing presence_handler
continues to work without changes. New handlers should consume
the new presence_service instead.
"""

import os
import boto3
from datetime import datetime
from typing import List


ONLINE_THRESHOLD_SEC = 90


class OnlineStatusService:
    """DynamoDB operations for the legacy online-only heartbeat."""

    def __init__(self):
        self.dynamodb = boto3.resource("dynamodb")
        self.players_table_name = os.environ.get(
            "PLAYERS_TABLE", "hopnbop-players"
        )
        self.players_table = self.dynamodb.Table(
            self.players_table_name
        )

    async def update_heartbeat(
        self, player_id: str
    ) -> None:
        """Write the current timestamp to
        online_last_seen_at on the player record."""
        now = int(datetime.now().timestamp())
        self.players_table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "SET online_last_seen_at = :now"
            ),
            ExpressionAttributeValues={":now": now},
        )

    async def get_online_friend_ids(
        self, friend_ids: List[str]
    ) -> List[str]:
        """Return which of the given player IDs are
        currently online (seen within
        ONLINE_THRESHOLD_SEC seconds)."""
        if not friend_ids:
            return []

        keys = [
            {"player_id": fid} for fid in friend_ids
        ]
        batch_response = self.dynamodb.batch_get_item(
            RequestItems={
                self.players_table_name: {
                    "Keys": keys,
                    "ProjectionExpression": (
                        "player_id,"
                        " online_last_seen_at"
                    ),
                }
            }
        )

        threshold = (
            int(datetime.now().timestamp())
            - ONLINE_THRESHOLD_SEC
        )
        online_ids = []
        items = (
            batch_response.get("Responses", {})
            .get(self.players_table_name, [])
        )
        for item in items:
            last_seen = int(
                item.get("online_last_seen_at", 0)
            )
            if last_seen >= threshold:
                online_ids.append(item["player_id"])
        return online_ids
