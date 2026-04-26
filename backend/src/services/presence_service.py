"""Cross-game presence service.

Tracks which game each player is currently in, optional rich
presence text, and a status enum (online/lobby/in-match/etc.).
The friends UI fans out across this table to render presence
badges; party_service queries it to reject cross-game invites.

Schema (one row per player; PK = player_id):

    player_id        — string, partition key
    game_id          — string, e.g. "hopnbop"
    status           — string, one of STATUS_*
    rich_presence    — string, free-text from the game
                       (e.g. "In lobby", "Playing 1v1")
    session_id       — string, optional GameLift session
    updated_at       — number, Unix seconds
    ttl              — number, Unix seconds for DynamoDB
                       to auto-delete stale rows

The TTL keeps the table self-cleaning. Clients are expected to
heartbeat every 60-90 seconds; the default TTL is 300 seconds so
two missed heartbeats expire the row.
"""

import os
import time
from dataclasses import dataclass, field
from typing import List, Optional

import boto3


# Defaults for stale-row expiry. Override via env if needed.
DEFAULT_TTL_SEC = int(
    os.environ.get("PRESENCE_TTL_SEC", "300")
)

# Status values used by clients and the friends UI. Free-form
# strings beyond these are accepted; consumers should treat
# unknown values as "online".
STATUS_OFFLINE = "offline"  # Not actually written; absence
                            # of a row means offline.
STATUS_ONLINE = "online"   # In lobby / menu, not in a match.
STATUS_IN_MATCH = "in_match"
STATUS_AWAY = "away"


@dataclass
class Presence:
    """A row from the presence table."""

    player_id: str
    game_id: str = ""
    status: str = STATUS_ONLINE
    rich_presence: str = ""
    session_id: str = ""
    updated_at: int = 0
    ttl: int = 0


class PresenceService:
    """DynamoDB ops for the cross-game presence table."""

    def __init__(self, default_ttl_sec: Optional[int] = None):
        self._dynamodb = boto3.resource("dynamodb")
        self._table_name = os.environ.get(
            "PRESENCE_TABLE", "snoringcat-presence"
        )
        self._table = self._dynamodb.Table(self._table_name)
        self._default_ttl_sec = (
            default_ttl_sec
            if default_ttl_sec is not None
            else DEFAULT_TTL_SEC
        )

    async def heartbeat(
        self,
        player_id: str,
        game_id: str,
        status: str = STATUS_ONLINE,
        rich_presence: str = "",
        session_id: str = "",
    ) -> Presence:
        """Upsert presence row for a player.

        Called by the client every minute or so. The row's TTL
        refreshes on each call so a player who stops heartbeating
        decays to offline within DEFAULT_TTL_SEC seconds.
        """
        now = int(time.time())
        presence = Presence(
            player_id=player_id,
            game_id=game_id,
            status=status,
            rich_presence=rich_presence,
            session_id=session_id,
            updated_at=now,
            ttl=now + self._default_ttl_sec,
        )
        item = {
            "player_id": presence.player_id,
            "game_id": presence.game_id,
            "status": presence.status,
            "updated_at": presence.updated_at,
            "ttl": presence.ttl,
        }
        if rich_presence:
            item["rich_presence"] = rich_presence
        if session_id:
            item["session_id"] = session_id
        self._table.put_item(Item=item)
        return presence

    async def clear(self, player_id: str) -> None:
        """Mark a player offline by removing the row.

        Used by sign-out and by the GameLift session-end hook so
        presence drops immediately rather than waiting for TTL.
        """
        self._table.delete_item(
            Key={"player_id": player_id}
        )

    async def get(
        self, player_id: str
    ) -> Optional[Presence]:
        """Read one player's presence row.

        Returns None if the row is missing OR has expired (TTL
        is in the past). DynamoDB does the actual deletion of
        expired rows asynchronously, but this method treats
        already-expired rows as absent so consumers can rely on
        the result being fresh.
        """
        response = self._table.get_item(
            Key={"player_id": player_id}
        )
        item = response.get("Item")
        if item is None:
            return None
        return self._from_item_if_fresh(item)

    async def batch_get(
        self, player_ids: List[str]
    ) -> dict[str, Presence]:
        """Read presence for many players at once.

        Returns a dict mapping player_id → Presence. Players with
        no row (or whose row has expired) are absent from the dict.

        Used by the friends UI to render presence badges and by
        party_service to look up an invitee's current game_id.
        DynamoDB BatchGetItem caps at 100 keys per request; this
        method chunks larger requests automatically.
        """
        result: dict[str, Presence] = {}
        if not player_ids:
            return result

        unique_ids = list(set(player_ids))
        for i in range(0, len(unique_ids), 100):
            chunk = unique_ids[i : i + 100]
            keys = [{"player_id": pid} for pid in chunk]
            response = self._dynamodb.batch_get_item(
                RequestItems={
                    self._table_name: {"Keys": keys}
                }
            )
            items = (
                response.get("Responses", {})
                .get(self._table_name, [])
            )
            for item in items:
                presence = self._from_item_if_fresh(item)
                if presence is not None:
                    result[presence.player_id] = presence
        return result

    @staticmethod
    def _from_item_if_fresh(
        item: dict,
    ) -> Optional[Presence]:
        """Decode a row, returning None if its TTL has passed.

        DynamoDB's TTL feature usually deletes expired rows
        within ~48 hours, so we double-check at read time.
        """
        ttl = int(item.get("ttl", 0))
        if ttl and ttl < int(time.time()):
            return None
        return Presence(
            player_id=item["player_id"],
            game_id=item.get("game_id", ""),
            status=item.get("status", STATUS_ONLINE),
            rich_presence=item.get("rich_presence", ""),
            session_id=item.get("session_id", ""),
            updated_at=int(item.get("updated_at", 0)),
            ttl=ttl,
        )
