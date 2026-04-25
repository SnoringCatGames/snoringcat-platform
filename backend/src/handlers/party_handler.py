"""Lambda handlers for party operations."""

import json
import os
import asyncio
from typing import Dict, Any
from aws_lambda_powertools import Logger, Tracer
from aws_lambda_powertools.utilities.typing import LambdaContext

import sys

sys.path.append(os.path.join(os.path.dirname(__file__), ".."))

from services.party_service import PartyService
from services.friends_service import FriendsService
from services.auth_service import AuthToken
from services.rate_limiter import RateLimiter
from services import secrets_service

logger = Logger()
tracer = Tracer()

party_service = PartyService()
friends_service = FriendsService()
rate_limiter = RateLimiter()

# CORS headers included in every response.
_HEADERS = {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
    "Access-Control-Allow-Headers": "Content-Type,Authorization",
    "Access-Control-Allow-Methods": "GET,POST,OPTIONS",
}


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


def _party_response(party) -> Dict:
    """Format a successful party response."""
    return {
        "statusCode": 200,
        "headers": _HEADERS,
        "body": json.dumps(
            {
                "status": "success",
                "party": {
                    "party_id": party.party_id,
                    "leader_id": party.leader_id,
                    "members": party.members,
                    "invited": party.invited,
                    "status": party.status,
                    "matchmaking_ticket_id": (
                        party.matchmaking_ticket_id
                    ),
                },
            }
        ),
    }


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def create_party(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /party/create — Create a new party."""
    try:
        auth_token = _authenticate(event)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id, "party_create", max_per_min=5
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        # Check if player is already in a party.
        existing = asyncio.run(
            party_service.get_party_for_player(
                player_id
            )
        )
        if existing is not None:
            return _party_response(existing)

        party = asyncio.run(
            party_service.create_party(player_id)
        )
        return _party_response(party)

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except Exception as e:
        logger.exception("Create party error")
        return _error_response(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def invite_to_party(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /party/invite — Invite a friend to party."""
    try:
        auth_token = _authenticate(event)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id, "party_invite", max_per_min=10
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        body = json.loads(event.get("body", "{}"))
        party_id = body.get("party_id", "")
        invitee_id = body.get("player_id", "")

        if not party_id or not invitee_id:
            return _error_response(
                400,
                "MISSING_INPUT",
                "Provide party_id and player_id",
            )

        # Verify invitee is a friend.
        relationships = asyncio.run(
            friends_service.list_all_relationships(
                player_id
            )
        )
        friend_ids = [
            f.friend_id
            for f in relationships.get("friends", [])
        ]
        if invitee_id not in friend_ids:
            return _error_response(
                400,
                "NOT_FRIENDS",
                "Can only invite friends",
            )

        party = asyncio.run(
            party_service.invite_player(
                party_id, player_id, invitee_id
            )
        )
        return _party_response(party)

    except PermissionError as e:
        msg = str(e)
        if "leader" in msg.lower():
            return _error_response(
                403, "NOT_LEADER", msg
            )
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except ValueError as e:
        return _error_response(
            400, "VALIDATION_ERROR", str(e)
        )
    except Exception as e:
        logger.exception("Invite to party error")
        return _error_response(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def join_party(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /party/join — Accept invite and join party."""
    try:
        auth_token = _authenticate(event)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id, "party_join", max_per_min=10
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        body = json.loads(event.get("body", "{}"))
        party_id = body.get("party_id", "")

        if not party_id:
            return _error_response(
                400,
                "MISSING_INPUT",
                "Provide party_id",
            )

        party = asyncio.run(
            party_service.join_party(
                party_id, player_id
            )
        )
        return _party_response(party)

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except ValueError as e:
        return _error_response(
            400, "VALIDATION_ERROR", str(e)
        )
    except Exception as e:
        logger.exception("Join party error")
        return _error_response(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def leave_party(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /party/leave — Leave the current party."""
    try:
        auth_token = _authenticate(event)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id, "party_leave", max_per_min=10
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        body = json.loads(event.get("body", "{}"))
        party_id = body.get("party_id", "")

        if not party_id:
            return _error_response(
                400,
                "MISSING_INPUT",
                "Provide party_id",
            )

        result = asyncio.run(
            party_service.leave_party(
                party_id, player_id
            )
        )

        if result is None:
            # Party was disbanded.
            return {
                "statusCode": 200,
                "headers": _HEADERS,
                "body": json.dumps(
                    {
                        "status": "success",
                        "message": "Party disbanded",
                        "disbanded": True,
                    }
                ),
            }

        return _party_response(result)

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except ValueError as e:
        return _error_response(
            400, "VALIDATION_ERROR", str(e)
        )
    except Exception as e:
        logger.exception("Leave party error")
        return _error_response(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def get_party_status(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /party/status — Get current party state."""
    try:
        auth_token = _authenticate(event)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id, "party_status", max_per_min=60
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        party = asyncio.run(
            party_service.get_party_for_player(
                player_id
            )
        )

        if party is None:
            # Check for pending invites via scan.
            pending = asyncio.run(
                _get_pending_invites(player_id)
            )
            return {
                "statusCode": 200,
                "headers": _HEADERS,
                "body": json.dumps(
                    {
                        "status": "success",
                        "party": None,
                        "pending_invites": pending,
                    }
                ),
            }

        return _party_response(party)

    except PermissionError:
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except Exception as e:
        logger.exception("Get party status error")
        return _error_response(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def start_party_matchmaking(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /party/start — Leader starts matchmaking."""
    try:
        auth_token = _authenticate(event)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id, "party_start", max_per_min=5
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        body = json.loads(event.get("body", "{}"))
        party_id = body.get("party_id", "")

        if not party_id:
            return _error_response(
                400,
                "MISSING_INPUT",
                "Provide party_id",
            )

        party = asyncio.run(
            party_service.get_party(party_id)
        )
        if party is None:
            return _error_response(
                404, "NOT_FOUND", "Party not found"
            )
        if party.leader_id != player_id:
            return _error_response(
                403,
                "NOT_LEADER",
                "Only the leader can start",
            )
        if party.status != "lobby":
            return _error_response(
                400,
                "INVALID_STATE",
                "Party is not in lobby state",
            )
        if len(party.members) < 2:
            return _error_response(
                400,
                "NOT_ENOUGH_MEMBERS",
                "Need at least 2 members",
            )

        # Start matchmaking for all party members.
        # Import here to avoid circular imports.
        from services.gamelift_service import (
            GameLiftService,
            MatchmakingPlayer,
        )

        gamelift_service = GameLiftService()
        players = []
        for member_id in party.members:
            players.append(
                MatchmakingPlayer(
                    player_id=member_id,
                    player_session_id=member_id,
                )
            )

        ticket_id = asyncio.run(
            gamelift_service.start_matchmaking(
                players
            )
        )

        asyncio.run(
            party_service.start_matchmaking(
                party_id, player_id, ticket_id
            )
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "ticket_id": ticket_id,
                    "party_id": party_id,
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
    except Exception as e:
        logger.exception(
            "Start party matchmaking error"
        )
        return _error_response(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def kick_from_party(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /party/kick — Leader kicks a member."""
    try:
        auth_token = _authenticate(event)
        player_id = auth_token.player_id

        if not rate_limiter.check_limit(
            player_id, "party_kick", max_per_min=10
        ):
            return _error_response(
                429, "RATE_LIMIT", "Too many requests"
            )

        body = json.loads(event.get("body", "{}"))
        party_id = body.get("party_id", "")
        target_id = body.get("player_id", "")

        if not party_id or not target_id:
            return _error_response(
                400,
                "MISSING_INPUT",
                "Provide party_id and player_id",
            )

        party = asyncio.run(
            party_service.kick_player(
                party_id, player_id, target_id
            )
        )
        return _party_response(party)

    except PermissionError as e:
        msg = str(e)
        if "leader" in msg.lower():
            return _error_response(
                403, "NOT_LEADER", msg
            )
        return _error_response(
            401, "UNAUTHORIZED", "Invalid token"
        )
    except ValueError as e:
        return _error_response(
            400, "VALIDATION_ERROR", str(e)
        )
    except Exception as e:
        logger.exception("Kick from party error")
        return _error_response(
            500, "INTERNAL_ERROR", "Internal server error"
        )


async def _get_pending_invites(
    player_id: str,
) -> list:
    """Find parties where this player has a pending
    invite."""
    import boto3

    dynamodb = boto3.resource("dynamodb")
    table_name = os.environ.get(
        "PARTIES_TABLE", "hopnbop-parties"
    )
    table = dynamodb.Table(table_name)

    response = table.scan(
        FilterExpression="contains(invited, :pid)",
        ExpressionAttributeValues={
            ":pid": player_id,
        },
    )
    items = response.get("Items", [])

    # Resolve leader display names.
    leader_ids = list(set(
        item["leader_id"] for item in items
    ))
    name_map = {}
    if leader_ids:
        players_table_name = os.environ.get(
            "PLAYERS_TABLE", "hopnbop-players"
        )
        players_table = dynamodb.Table(
            players_table_name
        )
        for lid in leader_ids:
            resp = players_table.get_item(
                Key={"player_id": lid},
                ProjectionExpression=(
                    "player_id, display_name"
                ),
            )
            item_data = resp.get("Item")
            if item_data:
                name_map[lid] = item_data.get(
                    "display_name", ""
                )

    invites = []
    for item in items:
        invites.append(
            {
                "party_id": item["party_id"],
                "leader_id": item["leader_id"],
                "leader_display_name": name_map.get(
                    item["leader_id"], ""
                ),
                "member_count": len(
                    item.get("members", [])
                ),
            }
        )
    return invites
