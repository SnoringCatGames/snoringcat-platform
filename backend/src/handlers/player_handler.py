"""Lambda handlers for player data operations."""

import json
import os
import asyncio
from datetime import datetime
from decimal import Decimal
from typing import Dict, Any
import boto3
from aws_lambda_powertools import Logger, Tracer
from aws_lambda_powertools.utilities.typing import LambdaContext

import sys

sys.path.append(os.path.join(os.path.dirname(__file__), ".."))

from services.auth_service import AuthService, AuthToken
from services.player_service import PlayerService
from services.match_service import MatchService
from services.settings_service import SettingsService
from services.friends_service import FriendsService
from services import secrets_service

logger = Logger()
tracer = Tracer()

player_service = PlayerService()
match_service = MatchService()
settings_service = SettingsService()
friends_service = FriendsService()

# CORS headers included in every response.
_HEADERS = {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
}

# Maximum settings payload size (bytes).
_MAX_SETTINGS_SIZE = 10240


def _validate_jwt(event: Dict):
    """Extract and validate JWT from Authorization header.

    Returns AuthToken on success, or an error response dict.
    """
    auth_header = (
        event.get("headers", {}).get("Authorization", "")
        or event.get("headers", {}).get(
            "authorization", ""
        )
    )
    if not auth_header.startswith("Bearer "):
        return _error(
            401, "UNAUTHORIZED", "Missing auth token"
        )
    token_str = auth_header[7:]
    try:
        return AuthToken.from_jwt(
            token_str, secrets_service.get_jwt_secret()
        )
    except ValueError as e:
        return _error(401, "UNAUTHORIZED", str(e))


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def get_player_profile(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /player/profile - Full player profile with stats."""
    try:
        token_or_error = _validate_jwt(event)
        if isinstance(token_or_error, dict):
            return token_or_error
        token = token_or_error

        profile = asyncio.run(
            player_service.get_player(token.player_id)
        )
        if profile is None:
            return _error(
                404, "NOT_FOUND", "Player not found"
            )

        rank = match_service.get_player_rank(
            token.player_id, profile.rating
        )

        recent = match_service.get_recent_matches(
            token.player_id, limit=5
        )

        total = profile.wins + profile.losses
        win_rate = (
            round(profile.wins / total * 100, 1)
            if total > 0
            else 0.0
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "player": {
                        "player_id": profile.player_id,
                        "display_name": (
                            profile.display_name
                        ),
                        "friend_code": (
                            profile.friend_code
                        ),
                        "rating": profile.rating,
                        "rank": rank,
                        "matches_played": (
                            profile.matches_played
                        ),
                        "wins": profile.wins,
                        "losses": profile.losses,
                        "win_rate": win_rate,
                        "created_at": (
                            profile.created_at
                        ),
                        "first_play_time": (
                            profile.first_play_time
                        ),
                        "last_play_time": (
                            profile.last_play_time
                        ),
                        "total_time_played_sec": (
                            profile.total_time_played_sec
                        ),
                        "total_kills": (
                            profile.total_kills
                        ),
                        "total_deaths": (
                            profile.total_deaths
                        ),
                        "total_bumps": (
                            profile.total_bumps
                        ),
                        "total_crown_time_sec": (
                            profile.total_crown_time_sec
                        ),
                        "total_regicide_count": (
                            profile.total_regicide_count
                        ),
                        "total_jumps": (
                            profile.total_jumps
                        ),
                        "total_water_count": (
                            profile.total_water_count
                        ),
                        "total_ice_count": (
                            profile.total_ice_count
                        ),
                        "total_spring_count": (
                            profile.total_spring_count
                        ),
                        "total_direction_changes": (
                            profile.total_direction_changes
                        ),
                        "total_snail_crushes": (
                            profile.total_snail_crushes
                        ),
                        "total_cricket_disturbances": (
                            profile.total_cricket_disturbances
                        ),
                        "total_fish_disturbances": (
                            profile.total_fish_disturbances
                        ),
                        "total_butterfly_disturbances": (
                            profile.total_butterfly_disturbances
                        ),
                        "total_fly_proximity_time_sec": (
                            profile.total_fly_proximity_time_sec
                        ),
                        "total_poop_count": (
                            profile.total_poop_count
                        ),
                    },
                    "recent_matches": recent,
                }
            ),
        }

    except Exception:
        logger.exception("Get player profile error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def get_player_settings(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /player/settings - Read cloud settings."""
    try:
        token_or_error = _validate_jwt(event)
        if isinstance(token_or_error, dict):
            return token_or_error
        token = token_or_error

        data = settings_service.get_settings(
            token.player_id
        )
        if data is None:
            return _error(
                404,
                "NOT_FOUND",
                "No cloud settings found",
            )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "settings": data["settings"],
                    "updated_at": data["updated_at"],
                }
            ),
        }

    except Exception:
        logger.exception("Get player settings error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def save_player_settings(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """PUT /player/settings - Save cloud settings."""
    try:
        token_or_error = _validate_jwt(event)
        if isinstance(token_or_error, dict):
            return token_or_error
        token = token_or_error

        body = json.loads(event.get("body", "{}"))
        settings = body.get("settings")
        if not isinstance(settings, dict):
            return _error(
                400,
                "BAD_REQUEST",
                "settings must be a JSON object",
            )

        # Size guard.
        settings_str = json.dumps(settings)
        if len(settings_str) > _MAX_SETTINGS_SIZE:
            return _error(
                400,
                "BAD_REQUEST",
                "Settings payload too large",
            )

        updated_at = body.get(
            "updated_at",
            int(datetime.now().timestamp()),
        )

        settings_service.save_settings(
            player_id=token.player_id,
            settings=settings,
            updated_at=updated_at,
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "message": "Settings saved",
                }
            ),
        }

    except Exception:
        logger.exception("Save player settings error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def get_match_history(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /player/history - Recent match history."""
    try:
        token_or_error = _validate_jwt(event)
        if isinstance(token_or_error, dict):
            return token_or_error
        token = token_or_error

        matches = match_service.get_recent_matches(
            token.player_id, limit=5
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "matches": matches,
                }
            ),
        }

    except Exception:
        logger.exception("Get match history error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def export_player_data(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /player/export - Export all player data."""
    try:
        token_or_error = _validate_jwt(event)
        if isinstance(token_or_error, dict):
            return token_or_error
        token = token_or_error

        player_id = token.player_id

        profile = asyncio.run(
            player_service.get_player(player_id)
        )
        if profile is None:
            return _error(
                404, "NOT_FOUND", "Player not found"
            )

        match_history = _get_full_match_history(
            player_id
        )

        # Include cloud settings in export.
        cloud_settings = settings_service.get_settings(
            player_id
        )

        # Include friends list in export.
        friends_data = asyncio.run(
            friends_service.get_friends_data_for_export(
                player_id
            )
        )

        export_data = {
            "status": "success",
            "exported_at": int(
                datetime.now().timestamp()
            ),
            "player": {
                "player_id": profile.player_id,
                "display_name": profile.display_name,
                "friend_code": profile.friend_code,
                "rating": profile.rating,
                "matches_played": (
                    profile.matches_played
                ),
                "wins": profile.wins,
                "losses": profile.losses,
                "created_at": profile.created_at,
                "last_active": profile.last_active,
                "is_anonymous": profile.is_anonymous,
                "linked_providers": list(
                    profile.auth_providers.keys()
                ),
                "consent_accepted_at": (
                    profile.consent_accepted_at
                ),
                "consent_legal_version": (
                    profile.consent_legal_version
                ),
                "first_play_time": (
                    profile.first_play_time
                ),
                "last_play_time": (
                    profile.last_play_time
                ),
                "total_time_played_sec": (
                    profile.total_time_played_sec
                ),
                "total_kills": profile.total_kills,
                "total_deaths": profile.total_deaths,
                "total_bumps": profile.total_bumps,
                "total_crown_time_sec": (
                    profile.total_crown_time_sec
                ),
                "total_regicide_count": (
                    profile.total_regicide_count
                ),
                "total_jumps": profile.total_jumps,
            },
            "match_history": match_history,
            "friends": friends_data,
            "settings": cloud_settings,
        }

        logger.info(f"Data exported: {player_id}")

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                export_data, default=_default_json
            ),
        }

    except Exception:
        logger.exception("Export player data error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


def _get_full_match_history(
    player_id: str,
) -> list:
    """Query all match history for a player."""
    dynamodb = boto3.resource("dynamodb")
    table_name = os.environ.get(
        "MATCH_HISTORY_TABLE", "hopnbop-match-history"
    )
    table = dynamodb.Table(table_name)

    items = []
    response = table.query(
        KeyConditionExpression=(
            boto3.dynamodb.conditions.Key(
                "player_id"
            ).eq(player_id)
        ),
    )
    items.extend(response.get("Items", []))

    while "LastEvaluatedKey" in response:
        response = table.query(
            KeyConditionExpression=(
                boto3.dynamodb.conditions.Key(
                    "player_id"
                ).eq(player_id)
            ),
            ExclusiveStartKey=response[
                "LastEvaluatedKey"
            ],
        )
        items.extend(response.get("Items", []))

    results = []
    for item in items:
        entry = {}
        for key, value in item.items():
            if key == "player_id":
                continue
            if isinstance(value, Decimal):
                entry[key] = (
                    int(value)
                    if value == int(value)
                    else float(value)
                )
            else:
                entry[key] = value
        results.append(entry)

    return results


def _default_json(obj):
    """JSON serializer for Decimal types."""
    if isinstance(obj, Decimal):
        if obj == int(obj):
            return int(obj)
        return float(obj)
    raise TypeError(f"Object of type {type(obj)} is not JSON serializable")


def _error(
    status_code: int, error_code: str, message: str
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
