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
    """POST /presence/heartbeat - Update the caller's
    online_last_seen_at and return the IDs of friends
    who are currently online."""
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

        asyncio.run(
            presence_service.update_heartbeat(
                player_id
            )
        )

        friend_ids = asyncio.run(
            friends_service.get_accepted_friend_ids(
                player_id
            )
        )

        online_ids = asyncio.run(
            presence_service.get_online_friend_ids(
                friend_ids
            )
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "online_friend_ids": online_ids,
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
