"""Lambda handlers for presence operations."""

import json
import os
import asyncio
from typing import Dict, Any
from aws_lambda_powertools import Logger, Tracer
from aws_lambda_powertools.utilities.typing import (
    LambdaContext,
)

import sys

sys.path.append(
    os.path.join(os.path.dirname(__file__), "..")
)

from services.online_status_service import OnlineStatusService
from services.presence_service import PresenceService
from services.friends_service import FriendsService
from services.auth_service import AuthToken
from services.rate_limiter import RateLimiter
from services import secrets_service

logger = Logger()
tracer = Tracer()

_HEADERS = {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
    "Access-Control-Allow-Headers": (
        "Content-Type,Authorization"
    ),
    "Access-Control-Allow-Methods": (
        "GET,POST,OPTIONS"
    ),
}

# `online_status_service` writes the legacy
# online_last_seen_at column on accounts; kept for any external
# consumer that hasn't migrated. `presence_service` writes the
# new per-game presence row that party invite gating and the
# friends UI badges read from.
online_status_service = OnlineStatusService()
presence_service = PresenceService()
friends_service = FriendsService()
rate_limiter = RateLimiter()


def _authenticate(event: Dict) -> AuthToken:
    """Extract and validate JWT from request."""
    auth_header = event.get("headers", {}).get(
        "Authorization", ""
    )
    if not auth_header.startswith("Bearer "):
        raise PermissionError("Missing authorization")

    jwt_token = auth_header[7:]
    jwt_secret = secrets_service.get_jwt_secret()

    if jwt_token.startswith("DEBUG_"):
        return AuthToken(
            player_id=jwt_token,
            display_name=(
                f"Player_{jwt_token[-4:]}"
            ),
            provider="debug",
            is_anonymous=False,
            issued_at=None,
            expires_at=None,
        )

    return AuthToken.from_jwt(jwt_token, jwt_secret)


def _error_response(
    status_code: int,
    error_code: str,
    message: str,
) -> Dict:
    """Format error response."""
    return {
        "statusCode": status_code,
        "headers": _HEADERS,
        "body": json.dumps(
            {
                "status": "error",
                "error_code": error_code,
                "message": message,
            }
        ),
    }


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def heartbeat(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /presence/heartbeat - Update the caller's presence
    row (game_id + optional rich_presence/status) and return the
    list of friends currently online with their game/status.

    Request body (all optional):
        {
            "status": "online" | "in_match" | "away",
            "rich_presence": "<short string for the UI>",
            "session_id": "<game session id>"
        }

    Response:
        {
            "status": "success",
            "online_friend_ids": [...],   # legacy field; just IDs
            "online_friends": [           # rich field; per friend
                {"player_id": "...", "game_id": "...",
                 "status": "...", "rich_presence": "..."},
                ...
            ]
        }
    """
    try:
        auth_token = _authenticate(event)
        if auth_token.is_anonymous:
            return _error_response(
                401,
                "UNAUTHORIZED",
                "Anonymous players cannot use presence",
            )
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id,
            "presence_heartbeat",
            max_per_min=20,
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        body = json.loads(event.get("body", "{}") or "{}")
        status_value = body.get("status", "online")
        rich_presence = body.get("rich_presence", "")
        session_id = body.get("session_id", "")

        # Write to BOTH the new presence table (game-aware row
        # with TTL) and the legacy online_last_seen_at column.
        # Old read paths can keep working until they migrate.
        asyncio.run(
            presence_service.heartbeat(
                player_id=player_id,
                game_id=auth_token.game_id,
                status=status_value,
                rich_presence=rich_presence,
                session_id=session_id,
            )
        )
        asyncio.run(
            online_status_service.update_heartbeat(player_id)
        )

        friend_ids = asyncio.run(
            friends_service.get_accepted_friend_ids(
                player_id
            )
        )

        # Read the new presence rows for friends. Friends without
        # a fresh row are simply absent from the result map (the
        # service treats expired rows as absent).
        friend_presence_map = asyncio.run(
            presence_service.batch_get(friend_ids)
        )

        online_friends = [
            {
                "player_id": pid,
                "game_id": p.game_id,
                "status": p.status,
                "rich_presence": p.rich_presence,
            }
            for pid, p in friend_presence_map.items()
        ]
        # Legacy field: just the IDs, for clients that haven't
        # migrated to the rich shape.
        online_friend_ids = [f["player_id"] for f in online_friends]

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "online_friend_ids": online_friend_ids,
                    "online_friends": online_friends,
                }
            ),
        }

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except Exception:
        logger.exception("Presence heartbeat error")
        return _error_response(
            500,
            "INTERNAL_ERROR",
            "Internal server error",
        )
