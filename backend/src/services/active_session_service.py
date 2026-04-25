"""Service for tracking active matchmaking and game sessions.

Provides atomic compare-and-set operations so that only one
device can enter matchmaking or a match per player at a time.
"""

import os
import time
import boto3
from botocore.exceptions import ClientError

# Seconds before a matchmaking record is considered stale.
_MATCHMAKING_TTL_SEC = 180

# Seconds before an in-match record is considered stale.
_IN_MATCH_TTL_SEC = 900

# Seconds after an in-match record is created before it
# can be overridden. Prevents match-dodging.
_IN_MATCH_COOLDOWN_SEC = 30

_STATE_MATCHMAKING = "matchmaking"
_STATE_IN_MATCH = "in_match"


class ActiveSessionService:

    def __init__(self):
        self._dynamodb = boto3.resource("dynamodb")
        self._table = self._dynamodb.Table(
            os.environ.get(
                "ACTIVE_SESSIONS_TABLE",
                "hopnbop-active-sessions",
            )
        )

    def _is_record_stale(self, item: dict) -> bool:
        """Return True if the record has passed its logical
        expiry, regardless of whether DynamoDB TTL has
        reaped it yet.
        """
        return int(time.time()) >= int(item.get("expires_at", 0))

    def try_start_matchmaking(
        self, player_id: str, ticket_id: str
    ) -> "tuple[bool, str | None, int]":
        """Attempt to create a matchmaking session record.

        Returns (True, old_ticket_id_or_None, 0) if the record
        was written successfully. The caller should cancel the
        old ticket if one is returned. Returns (False, None,
        retry_after_seconds) if the player has a live in-match
        session that cannot be overridden yet.

        Matchmaking-state sessions are always overridable so
        that a player who closes the page and reopens can
        immediately re-queue.

        In-match sessions are overridable after the cooldown
        period (_IN_MATCH_COOLDOWN_SEC) to allow players who
        disconnected to re-queue without waiting the full TTL.

        Uses a two-step read-then-conditional-write to close
        the race window between concurrent callers.
        """
        now = int(time.time())

        # Step 1: Read current record.
        response = self._table.get_item(
            Key={"player_id": player_id}
        )
        existing = response.get("Item")

        old_ticket_id = None
        if existing and not self._is_record_stale(existing):
            if existing.get("state") == _STATE_IN_MATCH:
                created_at = int(
                    existing.get("created_at", 0)
                )
                cooldown_remaining = (
                    (created_at + _IN_MATCH_COOLDOWN_SEC)
                    - now
                )
                if cooldown_remaining > 0:
                    return False, None, cooldown_remaining
                # Past cooldown. Allow override.
            else:
                # Matchmaking state. Allow override but
                # remember the old ticket so the caller
                # can cancel it.
                old_ticket_id = existing.get("session_id")

        # Step 2: Conditional write. Succeeds if the item is
        # absent, expired, in matchmaking state, or in_match
        # past the cooldown period.
        cooldown_threshold = now - _IN_MATCH_COOLDOWN_SEC
        expires_at = now + _MATCHMAKING_TTL_SEC
        try:
            self._table.put_item(
                Item={
                    "player_id": player_id,
                    "state": _STATE_MATCHMAKING,
                    "session_id": ticket_id,
                    "created_at": now,
                    "expires_at": expires_at,
                },
                ConditionExpression=(
                    "attribute_not_exists(player_id)"
                    " OR expires_at <= :now"
                    " OR #s = :matchmaking"
                    " OR (#s = :in_match"
                    " AND created_at <= :cooldown_threshold)"
                ),
                ExpressionAttributeValues={
                    ":now": now,
                    ":matchmaking": _STATE_MATCHMAKING,
                    ":in_match": _STATE_IN_MATCH,
                    ":cooldown_threshold": cooldown_threshold,
                },
                ExpressionAttributeNames={"#s": "state"},
            )
        except ClientError as exc:
            if (
                exc.response["Error"]["Code"]
                == "ConditionalCheckFailedException"
            ):
                return False, None, _IN_MATCH_COOLDOWN_SEC
            raise
        return True, old_ticket_id, 0

    def update_ticket_id(
        self, player_id: str, ticket_id: str
    ) -> None:
        """Update the session_id field with the real GameLift
        ticket ID after it is returned by StartMatchmaking.

        Called immediately after try_start_matchmaking, which
        writes a placeholder "pending" value.
        """
        self._table.update_item(
            Key={"player_id": player_id},
            UpdateExpression="SET session_id = :tid",
            ExpressionAttributeValues={":tid": ticket_id},
        )

    def transition_to_in_match(
        self, player_id: str, game_session_id: str
    ) -> None:
        """Upgrade the record to in_match state once
        GameLift matchmaking completes.

        Uses unconditional put_item so it is safe to call
        from concurrent Lambda instances (e.g., join and
        status poll both complete on the same ticket). The
        last writer wins but writes the same logical state.
        """
        now = int(time.time())
        self._table.put_item(
            Item={
                "player_id": player_id,
                "state": _STATE_IN_MATCH,
                "session_id": game_session_id,
                "created_at": now,
                "expires_at": now + _IN_MATCH_TTL_SEC,
            }
        )

    def clear_session(self, player_id: str) -> None:
        """Delete the active session record for a player.

        Idempotent: deleting a non-existent item is a no-op
        in DynamoDB.
        """
        self._table.delete_item(Key={"player_id": player_id})

    def get_active_session(
        self, player_id: str
    ) -> dict | None:
        """Return the active session item, or None if absent
        or stale.
        """
        response = self._table.get_item(
            Key={"player_id": player_id}
        )
        item = response.get("Item")
        if item is None:
            return None
        if self._is_record_stale(item):
            return None
        return item
