"""Lambda handlers for match results and leaderboard."""

import json
import os
import asyncio
from typing import Dict, Any

from aws_lambda_powertools import Logger, Metrics, Tracer
from aws_lambda_powertools.metrics import MetricUnit
from aws_lambda_powertools.utilities.typing import LambdaContext

import sys

sys.path.append(os.path.join(os.path.dirname(__file__), ".."))

from services.auth_service import AuthToken
from services.match_service import MatchService
from services.player_service import PlayerService
from services.leaderboard_service import (
    LeaderboardService,
    _current_iso_week,
)
from services import secrets_service
from services.active_session_service import ActiveSessionService
from services.fleet_service import FleetService

logger = Logger()
tracer = Tracer()
metrics = Metrics()

# Initialize services.
match_service = MatchService()
player_service = PlayerService()
leaderboard_service = LeaderboardService()
active_session_service = ActiveSessionService()
fleet_service = FleetService()

# CORS headers included in every response.
_HEADERS = {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
}

# Required fields in each player result entry.
_REQUIRED_PLAYER_FIELDS = ["player_id", "rank"]


def _validate_server_key(event: Dict) -> bool:
    """Validate the X-Server-Key header."""
    headers = event.get("headers", {})
    server_key = (
        headers.get("X-Server-Key", "")
        or headers.get("x-server-key", "")
    )
    if not server_key:
        return False
    expected = secrets_service.get_secret_string(
        "hopnbop/server-api-key"
    )
    return server_key == expected


def _validate_jwt(event: Dict):
    """Extract and validate JWT from Authorization header.

    Returns AuthToken on success, or an error response dict.
    """
    auth_header = (
        event.get("headers", {}).get("Authorization", "")
        or event.get("headers", {}).get("authorization", "")
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
@metrics.log_metrics
def submit_match_result(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /matches/result - Record match results (server-only)."""
    try:
        if not _validate_server_key(event):
            return _error(
                401, "UNAUTHORIZED", "Invalid server key"
            )

        body = json.loads(event.get("body", "{}"))
        game_session_id = body.get("game_session_id", "")
        match_duration_sec = body.get(
            "match_duration_sec", 0
        )
        level_id = body.get("level_id", "")
        player_results = body.get("player_results", [])

        if not game_session_id or not player_results:
            return _error(
                400,
                "BAD_REQUEST",
                "Missing game_session_id or player_results",
            )

        # Validate each player result has required fields.
        for pr in player_results:
            for field in _REQUIRED_PLAYER_FIELDS:
                if field not in pr:
                    return _error(
                        400,
                        "BAD_REQUEST",
                        f"Missing field: {field}",
                    )

        # Guest player IDs are ephemeral and have no
        # persistent profile. Record only non-guest players.
        persistent_results = [
            pr for pr in player_results
            if not pr["player_id"].startswith("PL_guest_")
        ]

        if persistent_results:
            match_service.record_match_result(
                game_session_id=game_session_id,
                match_duration_sec=float(match_duration_sec),
                level_id=level_id,
                player_results=persistent_results,
            )

        # Release active session locks so players can
        # matchmake again. Non-fatal: TTL cleans up stragglers.
        for pr in player_results:
            if pr["player_id"].startswith("PL_guest_"):
                continue
            try:
                active_session_service.clear_session(
                    pr["player_id"]
                )
            except Exception:
                logger.warning(
                    "Failed to clear session for %s",
                    pr["player_id"],
                )

        logger.info(
            "Match result recorded",
            extra={
                "game_session_id": game_session_id,
                "player_count": len(player_results),
            },
        )
        metrics.add_dimension(
            name="level_id", value=level_id or "unknown"
        )
        metrics.add_metric(
            name="match_completed",
            unit=MetricUnit.Count,
            value=1,
        )

        # Refresh fleet activity timestamp. The scheduled
        # idle-check Lambda uses this to delay scale-to-0
        # for 30 minutes after the last match ends.
        try:
            fleet_service.update_activity("match_end")
        except Exception:
            logger.warning(
                "Failed to update fleet activity"
            )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "message": "Match result recorded",
                }
            ),
        }

    except Exception:
        logger.exception("Submit match result error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def get_leaderboard(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /leaderboard - Get leaderboard entries.

    Query params:
    - type: "alltime" (default) or "weekly"
    - scope: "global" (default) or a level_id
    - limit: page size (default 50)
    - cursor: opaque cursor for next page
    - player_id: return context page for this player

    Authentication is optional. When a valid JWT is
    provided, the response includes the requesting
    player's rank and rating.
    """
    try:
        # Auth is optional for the leaderboard.
        token = None
        token_or_error = _validate_jwt(event)
        if not isinstance(token_or_error, dict):
            token = token_or_error

        params = event.get("queryStringParameters") or {}

        lb_type = params.get("type", "alltime")
        if lb_type not in ("alltime", "weekly"):
            lb_type = "alltime"

        scope = params.get("scope", "global")

        try:
            limit = int(params.get("limit", 50))
        except (ValueError, TypeError):
            limit = 50
        limit = min(limit, 100)

        cursor = params.get("cursor")
        target_player_id = params.get("player_id")

        # Build leaderboard_id.
        if lb_type == "weekly":
            week = _current_iso_week()
            if scope == "global":
                lb_id = f"weekly#{week}"
            else:
                lb_id = f"weekly#{scope}#{week}"
        else:
            if scope == "global":
                lb_id = "alltime#global"
            else:
                lb_id = f"alltime#{scope}"

        # If target_player_id requested, return context.
        if target_player_id:
            player = asyncio.run(
                player_service.get_player(
                    target_player_id
                )
            )
            if player:
                context_data = (
                    leaderboard_service.get_player_context(
                        lb_id,
                        target_player_id,
                        player.rating,
                    )
                )
                return {
                    "statusCode": 200,
                    "headers": _HEADERS,
                    "body": json.dumps(
                        {
                            "status": "success",
                            "leaderboard": (
                                context_data["entries"]
                            ),
                            "player_rank": (
                                context_data["rank"]
                            ),
                        }
                    ),
                }

        # Try dedicated leaderboard table first.
        entries, next_cursor = (
            leaderboard_service.get_page(
                lb_id, limit=limit, cursor=cursor
            )
        )

        # Fallback to rating-index GSI for alltime
        # global if the dedicated table is empty.
        if (
            not entries
            and lb_id == "alltime#global"
            and not cursor
        ):
            entries = match_service.get_leaderboard(
                limit=limit
            )
            next_cursor = None

        response_body = {
            "status": "success",
            "leaderboard": entries,
        }
        if next_cursor:
            response_body["next_cursor"] = next_cursor

        # Include personalized rank when authenticated.
        if token is not None:
            player = asyncio.run(
                player_service.get_player(
                    token.player_id
                )
            )
            your_rank = 0
            your_rating = 0
            if player:
                your_rating = player.rating
                your_rank = (
                    leaderboard_service.get_player_rank(
                        lb_id,
                        token.player_id,
                        player.rating,
                    )
                )
            response_body["your_rank"] = your_rank
            response_body["your_rating"] = your_rating

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(response_body),
        }

    except Exception:
        logger.exception("Get leaderboard error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def get_player_stats(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /players/{player_id}/stats - Get player stats."""
    try:
        token_or_error = _validate_jwt(event)
        if isinstance(token_or_error, dict):
            return token_or_error

        path_params = event.get("pathParameters") or {}
        player_id = path_params.get("player_id", "")
        if not player_id:
            return _error(
                400, "BAD_REQUEST", "Missing player_id"
            )

        player = asyncio.run(
            player_service.get_player(player_id)
        )
        if player is None:
            return _error(
                404, "NOT_FOUND", "Player not found"
            )

        # Get rank.
        rank = match_service.get_player_rank(
            player_id, player.rating
        )

        # Get recent matches.
        matches = match_service.get_recent_matches(
            player_id
        )

        # Calculate win rate.
        total = player.wins + player.losses
        win_rate = (
            round(player.wins / total * 100, 1)
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
                        "player_id": player.player_id,
                        "display_name": player.display_name,
                        "rating": player.rating,
                        "rank": rank,
                        "matches_played": (
                            player.matches_played
                        ),
                        "wins": player.wins,
                        "losses": player.losses,
                        "win_rate": win_rate,
                        "created_at": player.created_at,
                    },
                    "recent_matches": matches,
                }
            ),
        }

    except Exception:
        logger.exception("Get player stats error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


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
