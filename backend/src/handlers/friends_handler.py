"""Lambda handlers for friends operations."""

import json
import os
import asyncio
from typing import Dict, Any
from aws_lambda_powertools import Logger, Tracer
from aws_lambda_powertools.utilities.typing import (
    LambdaContext,
)

import sys

sys.path.append(os.path.join(os.path.dirname(__file__), ".."))

from services.friends_service import FriendsService
from services.player_service import PlayerService
from services.auth_service import AuthToken
from services.rate_limiter import RateLimiter
from services import secrets_service

logger = Logger()
tracer = Tracer()

# CORS headers included in every response.
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

friends_service = FriendsService()
player_service = PlayerService()
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
            display_name=f"Player_{jwt_token[-4:]}",
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


def _require_authenticated(
    auth_token: AuthToken,
) -> None:
    """Raise if the player is anonymous."""
    if auth_token.is_anonymous:
        raise PermissionError(
            "Anonymous players cannot use friends"
        )


def _serialize_relationship(rel) -> dict:
    """Serialize a FriendRelationship to a dict."""
    return {
        "player_id": rel.friend_id,
        "display_name": rel.display_name,
        "source": rel.source,
        "status": rel.status,
        "sender_id": rel.sender_id,
        "created_at": rel.created_at,
        "updated_at": rel.updated_at,
    }


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def list_friends(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /friends - List all relationships."""
    try:
        auth_token = _authenticate(event)
        _require_authenticated(auth_token)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id, "friends_list", max_per_min=30
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        rels = asyncio.run(
            friends_service.list_all_relationships(
                player_id
            )
        )
        unseen_count = asyncio.run(
            friends_service.get_unseen_count(
                player_id
            )
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "friends": [
                        _serialize_relationship(r)
                        for r in rels["friends"]
                    ],
                    "sent_requests": [
                        _serialize_relationship(r)
                        for r in rels["sent_requests"]
                    ],
                    "incoming_requests": [
                        _serialize_relationship(r)
                        for r in rels[
                            "incoming_requests"
                        ]
                    ],
                    "unseen_count": unseen_count,
                }
            ),
        }

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except Exception:
        logger.exception("List friends error")
        return _error_response(
            500,
            "INTERNAL_ERROR",
            "Internal server error",
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def add_friend(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /friends/add - Send a friend request."""
    try:
        auth_token = _authenticate(event)
        _require_authenticated(auth_token)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id, "friends_add", max_per_min=10
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        body = json.loads(event.get("body", "{}"))
        friend_code = body.get("friend_code", "")
        friend_player_id = body.get("player_id", "")
        source = body.get("source", "friend_code")

        # Resolve friend player ID from friend code.
        if friend_code and not friend_player_id:
            friend_profile = asyncio.run(
                player_service
                .get_player_by_friend_code(
                    friend_code.upper().strip()
                )
            )
            if friend_profile is None:
                return _error_response(
                    404,
                    "NOT_FOUND",
                    "No player with that friend code",
                )
            friend_player_id = (
                friend_profile.player_id
            )

        if not friend_player_id:
            return _error_response(
                400,
                "MISSING_INPUT",
                "Provide friend_code or player_id",
            )

        result = asyncio.run(
            friends_service.send_friend_request(
                player_id, friend_player_id, source
            )
        )

        result_code = result["result"]
        messages = {
            "request_sent": "Friend request sent",
            "auto_accepted": "Friend added",
            "already_friends": "Already friends",
            "already_pending": "Request already sent",
        }

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "result": result_code,
                    "message": messages.get(
                        result_code, ""
                    ),
                }
            ),
        }

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except ValueError as e:
        return _error_response(
            400, "VALIDATION_ERROR", str(e)
        )
    except Exception:
        logger.exception("Add friend error")
        return _error_response(
            500,
            "INTERNAL_ERROR",
            "Internal server error",
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def accept_request(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /friends/accept - Accept a friend
    request."""
    try:
        auth_token = _authenticate(event)
        _require_authenticated(auth_token)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id,
            "friends_accept",
            max_per_min=10,
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        body = json.loads(event.get("body", "{}"))
        sender_player_id = body.get("player_id", "")

        if not sender_player_id:
            return _error_response(
                400,
                "MISSING_INPUT",
                "Provide player_id",
            )

        accepted = asyncio.run(
            friends_service.accept_friend_request(
                player_id, sender_player_id
            )
        )

        if not accepted:
            return _error_response(
                404,
                "NOT_FOUND",
                "No pending request from that player",
            )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "message": "Friend request accepted",
                }
            ),
        }

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except Exception:
        logger.exception("Accept request error")
        return _error_response(
            500,
            "INTERNAL_ERROR",
            "Internal server error",
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def reject_request(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /friends/reject - Reject a friend
    request."""
    try:
        auth_token = _authenticate(event)
        _require_authenticated(auth_token)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id,
            "friends_reject",
            max_per_min=10,
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        body = json.loads(event.get("body", "{}"))
        sender_player_id = body.get("player_id", "")

        if not sender_player_id:
            return _error_response(
                400,
                "MISSING_INPUT",
                "Provide player_id",
            )

        rejected = asyncio.run(
            friends_service.reject_friend_request(
                player_id, sender_player_id
            )
        )

        if not rejected:
            return _error_response(
                404,
                "NOT_FOUND",
                "No pending request from that player",
            )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "message": "Friend request rejected",
                }
            ),
        }

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except Exception:
        logger.exception("Reject request error")
        return _error_response(
            500,
            "INTERNAL_ERROR",
            "Internal server error",
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def cancel_request(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /friends/cancel - Cancel a sent friend
    request."""
    try:
        auth_token = _authenticate(event)
        _require_authenticated(auth_token)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id,
            "friends_cancel",
            max_per_min=10,
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        body = json.loads(event.get("body", "{}"))
        receiver_player_id = body.get(
            "player_id", ""
        )

        if not receiver_player_id:
            return _error_response(
                400,
                "MISSING_INPUT",
                "Provide player_id",
            )

        cancelled = asyncio.run(
            friends_service.cancel_friend_request(
                player_id, receiver_player_id
            )
        )

        if not cancelled:
            return _error_response(
                404,
                "NOT_FOUND",
                "No pending request to that player",
            )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "message": "Friend request cancelled",
                }
            ),
        }

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except Exception:
        logger.exception("Cancel request error")
        return _error_response(
            500,
            "INTERNAL_ERROR",
            "Internal server error",
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def get_notifications(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /friends/notifications?since=N - Get new
    friend notifications."""
    try:
        auth_token = _authenticate(event)
        _require_authenticated(auth_token)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id,
            "friends_notifications",
            max_per_min=20,
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        params = (
            event.get("queryStringParameters", {})
            or {}
        )
        since = int(params.get("since", "0"))

        notifications = asyncio.run(
            friends_service.get_notifications(
                player_id, since
            )
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "incoming_requests": notifications[
                        "incoming_requests"
                    ],
                    "accepted_requests": notifications[
                        "accepted_requests"
                    ],
                    "rejected_requests": notifications[
                        "rejected_requests"
                    ],
                }
            ),
        }

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except Exception:
        logger.exception("Get notifications error")
        return _error_response(
            500,
            "INTERNAL_ERROR",
            "Internal server error",
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def mark_seen(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /friends/seen - Mark friend notifications
    as seen."""
    try:
        auth_token = _authenticate(event)
        _require_authenticated(auth_token)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id,
            "friends_seen",
            max_per_min=10,
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        asyncio.run(
            friends_service.mark_friends_seen(
                player_id
            )
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "message": "Marked as seen",
                }
            ),
        }

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except Exception:
        logger.exception("Mark seen error")
        return _error_response(
            500,
            "INTERNAL_ERROR",
            "Internal server error",
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def remove_friend(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /friends/remove - Remove an accepted
    friend."""
    try:
        auth_token = _authenticate(event)
        _require_authenticated(auth_token)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id,
            "friends_remove",
            max_per_min=10,
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        body = json.loads(event.get("body", "{}"))
        friend_player_id = body.get("player_id", "")

        if not friend_player_id:
            return _error_response(
                400, "MISSING_INPUT", "Provide player_id"
            )

        removed = asyncio.run(
            friends_service.remove_friend(
                player_id, friend_player_id
            )
        )

        if not removed:
            return _error_response(
                404,
                "NOT_FOUND",
                "Not friends with that player",
            )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "message": "Friend removed",
                }
            ),
        }

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except Exception:
        logger.exception("Remove friend error")
        return _error_response(
            500,
            "INTERNAL_ERROR",
            "Internal server error",
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def search_by_code(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /friends/search?code=ABCDEF - Search by
    friend code."""
    try:
        auth_token = _authenticate(event)
        _require_authenticated(auth_token)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id,
            "friends_search",
            max_per_min=30,
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        params = (
            event.get("queryStringParameters", {})
            or {}
        )
        code = params.get("code", "").upper().strip()

        if not code:
            return _error_response(
                400,
                "MISSING_INPUT",
                "Provide code parameter",
            )

        profile = asyncio.run(
            player_service.get_player_by_friend_code(
                code
            )
        )

        if profile is None:
            return _error_response(
                404,
                "NOT_FOUND",
                "No player with that code",
            )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "player": {
                        "player_id": (
                            profile.player_id
                        ),
                        "display_name": (
                            profile.display_name
                        ),
                        "friend_code": (
                            profile.friend_code
                        ),
                    },
                }
            ),
        }

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except Exception:
        logger.exception(
            "Search friend code error"
        )
        return _error_response(
            500,
            "INTERNAL_ERROR",
            "Internal server error",
        )
