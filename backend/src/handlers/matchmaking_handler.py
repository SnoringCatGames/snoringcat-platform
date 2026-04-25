"""Lambda handlers for matchmaking operations."""

import json
import os
import asyncio
from typing import Dict, Any
from aws_lambda_powertools import Logger, Metrics, Tracer
from aws_lambda_powertools.metrics import MetricUnit
from aws_lambda_powertools.utilities.typing import LambdaContext

import sys

sys.path.append(os.path.join(os.path.dirname(__file__), ".."))

from services.gamelift_service import (
    GameLiftService,
    MatchmakingPlayer,
)
from services.auth_service import AuthService, AuthToken
from services.player_service import PlayerService
from services.rate_limiter import RateLimiter
from services.level_selection_service import (
    parse_level_preference,
    parse_session_preference,
    select_level_for_match,
)
from services import secrets_service
from services.dns_service import DnsService
from services.active_session_service import ActiveSessionService

logger = Logger()
tracer = Tracer()
metrics = Metrics()

# Offset from the game session's primary port (ENet UDP)
# to the WSS port. With 2 container ports [4433/UDP,
# 4433/TCP], GameLift maps them to consecutive host ports.
# The primary port (ProcessReady) maps to UDP; the WSS
# port (Godot TLS) is the next one (TCP).
_WSS_PORT_OFFSET = 1

# CORS headers included in every response.
_HEADERS = {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
    "Access-Control-Allow-Headers": "Content-Type,Authorization",
    "Access-Control-Allow-Methods": "GET,POST,OPTIONS",
}

# Initialize services (cached across invocations).
gamelift = GameLiftService(
    region=os.environ.get("AWS_REGION", "us-west-2"),
    poll_interval_sec=1.0,
    max_poll_time_sec=120.0,
)
player_service = PlayerService()
rate_limiter = RateLimiter()
dns_service = DnsService()
active_session_service = ActiveSessionService()


def _resolve_server_address(result):
    """Resolve server address and port for the match result.

    For WebSocket and WebRTC matches, returns the pre-warmed
    DNS hostname + WSS host port. WebRTC signaling uses a
    brief WebSocket connection through the same nginx path.
    The DNS A record was created at container startup by
    entrypoint.sh, so it is already propagated by the time
    clients connect. The hostname is derived
    deterministically from the server IP.
    For ENet matches (native-only), returns the raw IP.
    """
    if result.transport_type in ("websocket", "webrtc"):
        hostname = _hostname_from_ip(result.server_ip)
        wss_port = result.server_port + _WSS_PORT_OFFSET
        return hostname, wss_port
    return result.server_ip, result.server_port


def _hostname_from_ip(ip: str) -> str:
    """Derive a DNS hostname from a server IP.

    Example: 35.91.191.229 -> s-35-91-191-229.game.hopnbop.net
    Must match the hostname created in entrypoint.sh.
    """
    label = "s-" + ip.replace(".", "-")
    return f"{label}.game.hopnbop.net"


def _lookup_server_ip(game_session_id: str) -> str:
    """Look up the server IP for a game session via GameLift."""
    try:
        response = gamelift.client.describe_game_sessions(
            GameSessionId=game_session_id
        )
        sessions = response.get("GameSessions", [])
        if sessions:
            return sessions[0].get("IpAddress", "")
    except Exception:
        logger.warning(
            "Failed to look up server IP for %s",
            game_session_id,
        )
    return ""


# In-memory store for session preferences keyed by ticket ID.
# Lambda instances are short-lived so this only works when the
# same instance handles both /start and /status. For
# production, consider DynamoDB or ElastiCache.
_pending_session_prefs: Dict[str, Dict] = {}


@tracer.capture_lambda_handler
@logger.inject_lambda_context
@metrics.log_metrics
def join_matchmaking(event: Dict[str, Any], context: LambdaContext) -> Dict:
    """
    POST /matchmaking/join
    Simplified endpoint that starts matchmaking and polls until complete.
    """
    try:
        # Extract JWT from Authorization header.
        auth_header = event.get("headers", {}).get("Authorization", "")
        if not auth_header.startswith("Bearer "):
            return error_response(401, "MISSING_AUTH", "Missing authorization")

        jwt_token = auth_header[7:]
        jwt_secret = secrets_service.get_jwt_secret()

        # For preview mode, accept debug tokens.
        if jwt_token.startswith("DEBUG_"):
            auth_token = AuthToken(
                player_id=jwt_token,
                display_name=f"Player_{jwt_token[-4:]}",
                provider="debug",
                is_anonymous=False,
                issued_at=None,
                expires_at=None,
            )
        else:
            auth_token = AuthToken.from_jwt(jwt_token, jwt_secret)

        player_id = auth_token.player_id
        is_guest = auth_token.is_guest

        # Rate limiting (skip for guests — each has a
        # unique ephemeral ID so rate limiting is
        # ineffective; abuse is mitigated server-side).
        if not is_guest:
            if not rate_limiter.check_limit(
                player_id, "matchmaking", max_per_min=5
            ):
                return error_response(
                    429,
                    "RATE_LIMIT",
                    "Too many requests",
                    retry_after=60,
                )

        # Parse request body.
        body = json.loads(event.get("body", "{}"))
        player_count = body.get("player_count", 1)
        client_id = body.get("client_id", "unknown")
        platform = body.get("platform", "native")
        session_prefs_data = body.get(
            "session_preferences", {}
        )
        level_prefs_data = session_prefs_data

        # Validate input.
        if player_count < 1 or player_count > 4:
            return error_response(
                400, "INVALID_INPUT", "player_count must be 1-4"
            )

        # Parse session preferences.
        session_prefs = parse_session_preference(
            session_prefs_data
        )
        level_prefs = session_prefs.level

        logger.info(
            f"Matchmaking request from {player_id}: "
            f"{player_count} player(s), client {client_id}, "
            f"session prefs: {session_prefs_data}"
        )

        # Guest players have no persistent profile.
        # Use a default rating and skip DB operations.
        if is_guest:
            skill_rating = 1500
        else:
            player_profile = asyncio.run(
                player_service.get_or_create_player(
                    player_id,
                    auth_token.display_name,
                    {},
                )
            )
            skill_rating = player_profile.rating

        # Determine authentication status for FlexMatch
        # preference matching.
        is_authenticated = (
            0
            if auth_token.provider in (
                "anonymous", "guest", "debug"
            )
            else 1
        )

        # Guard: reject if player is in an active match.
        # Skip for guests — their IDs are ephemeral and
        # they have no persistent session record.
        if not is_guest:
            allowed, old_ticket, retry_after_seconds = (
                active_session_service.try_start_matchmaking(
                    player_id, "pending"
                )
            )
            if not allowed:
                wait_msg = (
                    "Please wait %ds before re-queuing."
                    % retry_after_seconds
                    if retry_after_seconds > 0
                    else (
                        "Please finish or wait for it"
                        " to end."
                    )
                )
                return error_response(
                    409,
                    "CONCURRENT_SESSION",
                    "You are already in an active match. "
                    + wait_msg,
                    retry_after_seconds=(
                        retry_after_seconds
                    ),
                )
            if old_ticket and old_ticket != "pending":
                try:
                    asyncio.run(
                        gamelift.cancel_matchmaking(
                            old_ticket
                        )
                    )
                except Exception:
                    logger.warning(
                        "Failed to cancel old ticket %s",
                        old_ticket,
                    )

        # Create matchmaking players (one per local player).
        players = [
            MatchmakingPlayer(
                player_id=f"{player_id}_{i}",
                skill_rating=skill_rating,
                region="us-west-2",
                latency_map={"us-west-2": 50},
                platform=platform,
                is_authenticated=is_authenticated,
            )
            for i in range(player_count)
        ]

        # Start matchmaking. Clear the session lock on
        # failure so the player is not stuck waiting for
        # TTL expiry (only applies to non-guest players).
        config_name = os.environ.get(
            "MATCHMAKING_CONFIG", "hopnbop-ffa-matchmaker"
        )
        try:
            ticket_id = asyncio.run(
                gamelift.start_matchmaking(
                    config_name=config_name, players=players
                )
            )
        except Exception:
            if not is_guest:
                active_session_service.clear_session(
                    player_id
                )
            raise

        if not is_guest:
            # Update the session record with the real
            # ticket ID.
            active_session_service.update_ticket_id(
                player_id, ticket_id
            )

        logger.info(f"Started matchmaking: {ticket_id}")

        # Poll until complete (blocking call).
        try:
            result = asyncio.run(
                gamelift.poll_matchmaking(ticket_id)
            )

            logger.info(
                f"Matchmaking complete: "
                f"{result.game_session_id}"
            )
            metrics.add_metric(
                name="player_connected",
                unit=MetricUnit.Count,
                value=1,
            )

            if not is_guest:
                # Transition session state to in_match now
                # that the game session ID is known.
                active_session_service.transition_to_in_match(
                    player_id, result.game_session_id
                )

            # Select level based on player preferences.
            selected_level_id = select_level_for_match(
                [level_prefs]
            )
            logger.info(f"Selected level: {selected_level_id}")

            server_address, server_port = (
                _resolve_server_address(result)
            )

            return {
                "statusCode": 200,
                "headers": _HEADERS,
                "body": json.dumps(
                    {
                        "status": "success",
                        "server_version": os.environ.get(
                            "GAME_VERSION", "0.1.0"
                        ),
                        "protocol_version": int(
                            os.environ.get(
                                "PROTOCOL_VERSION", "1"
                            )
                        ),
                        "game_session_id": (
                            result.game_session_id
                        ),
                        "server_ip": server_address,
                        "server_port": server_port,
                        "player_session_ids": (
                            result.player_session_ids
                        ),
                        "selected_level_id": (
                            selected_level_id
                        ),
                        "transport_type": (
                            result.transport_type
                        ),
                    }
                ),
            }

        except TimeoutError as e:
            logger.warning(
                f"Matchmaking timeout: {ticket_id}"
            )
            if not is_guest:
                active_session_service.clear_session(
                    player_id
                )
            return error_response(
                408, "MATCHMAKING_TIMEOUT", str(e)
            )

    except ValueError as e:
        logger.error(f"Validation error: {e}")
        return error_response(400, "VALIDATION_ERROR", str(e))
    except Exception as e:
        logger.exception("Matchmaking error")
        return error_response(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def start_matchmaking(event: Dict[str, Any], context: LambdaContext) -> Dict:
    """
    POST /matchmaking/start
    Start matchmaking and return ticket ID for polling.
    """
    try:
        # Extract JWT.
        auth_header = event.get("headers", {}).get("Authorization", "")
        if not auth_header.startswith("Bearer "):
            return error_response(401, "MISSING_AUTH", "Missing authorization")

        jwt_token = auth_header[7:]
        jwt_secret = secrets_service.get_jwt_secret()

        # For preview mode, accept debug tokens.
        if jwt_token.startswith("DEBUG_"):
            auth_token = AuthToken(
                player_id=jwt_token,
                display_name=f"Player_{jwt_token[-4:]}",
                provider="debug",
                is_anonymous=False,
                issued_at=None,
                expires_at=None,
            )
        else:
            auth_token = AuthToken.from_jwt(jwt_token, jwt_secret)

        player_id = auth_token.player_id
        is_guest = auth_token.is_guest

        # Rate limiting (skip for guests).
        if not is_guest:
            if not rate_limiter.check_limit(
                player_id, "matchmaking", max_per_min=5
            ):
                return error_response(
                    429,
                    "RATE_LIMIT",
                    "Too many requests",
                    retry_after=60,
                )

        # Parse request body.
        body = json.loads(event.get("body", "{}"))
        player_count = body.get("player_count", 1)
        platform = body.get("platform", "native")

        # Validate input.
        if player_count < 1 or player_count > 4:
            return error_response(
                400, "INVALID_INPUT", "player_count must be 1-4"
            )

        # Guest players have no persistent profile.
        if is_guest:
            skill_rating = 1500
        else:
            player_profile = asyncio.run(
                player_service.get_or_create_player(
                    player_id,
                    auth_token.display_name,
                    {},
                )
            )
            skill_rating = player_profile.rating

        # Determine authentication status for FlexMatch
        # preference matching.
        is_authenticated = (
            0
            if auth_token.provider in (
                "anonymous", "guest", "debug"
            )
            else 1
        )

        # Guard: reject if player is in an active match.
        # Skip for guests.
        if not is_guest:
            allowed, old_ticket, retry_after_seconds = (
                active_session_service.try_start_matchmaking(
                    player_id, "pending"
                )
            )
            if not allowed:
                wait_msg = (
                    "Please wait %ds before re-queuing."
                    % retry_after_seconds
                    if retry_after_seconds > 0
                    else (
                        "Please finish or wait for it"
                        " to end."
                    )
                )
                return error_response(
                    409,
                    "CONCURRENT_SESSION",
                    "You are already in an active match. "
                    + wait_msg,
                    retry_after_seconds=(
                        retry_after_seconds
                    ),
                )
            if old_ticket and old_ticket != "pending":
                try:
                    asyncio.run(
                        gamelift.cancel_matchmaking(
                            old_ticket
                        )
                    )
                except Exception:
                    logger.warning(
                        "Failed to cancel old ticket %s",
                        old_ticket,
                    )

        # Create matchmaking players.
        players = [
            MatchmakingPlayer(
                player_id=f"{player_id}_{i}",
                skill_rating=skill_rating,
                region="us-west-2",
                latency_map={"us-west-2": 50},
                platform=platform,
                is_authenticated=is_authenticated,
            )
            for i in range(player_count)
        ]

        # Parse session preferences for level selection.
        session_prefs_data = body.get(
            "session_preferences", {}
        )

        # Start matchmaking. Clear the session lock on
        # failure (only for non-guest players).
        config_name = os.environ.get(
            "MATCHMAKING_CONFIG", "hopnbop-ffa-matchmaker"
        )
        try:
            ticket_id = asyncio.run(
                gamelift.start_matchmaking(
                    config_name=config_name, players=players
                )
            )
        except Exception:
            if not is_guest:
                active_session_service.clear_session(
                    player_id
                )
            raise

        if not is_guest:
            # Update the session record with the real
            # ticket ID.
            active_session_service.update_ticket_id(
                player_id, ticket_id
            )

        logger.info(
            f"Started matchmaking for {player_id}: "
            f"{ticket_id}"
        )

        # Store session preferences for level selection
        # when polling completes.
        _pending_session_prefs[ticket_id] = (
            session_prefs_data
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "ticket_id": ticket_id,
                    "estimated_wait_ms": 5000,
                }
            ),
        }

    except ValueError as e:
        return error_response(400, "VALIDATION_ERROR", str(e))
    except Exception as e:
        logger.exception("Matchmaking error")
        return error_response(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def get_matchmaking_status(event: Dict[str, Any], context: LambdaContext) -> Dict:
    """
    GET /matchmaking/status/{ticket_id}
    Poll matchmaking ticket status.
    """
    try:
        # Auth.
        auth_header = event.get("headers", {}).get("Authorization", "")
        jwt_token = auth_header[7:]
        jwt_secret = secrets_service.get_jwt_secret()

        # Accept debug tokens. Capture player_id for session
        # cleanup on terminal status.
        is_guest = False
        if jwt_token.startswith("DEBUG_"):
            player_id = jwt_token
        else:
            auth_token = AuthToken.from_jwt(
                jwt_token, jwt_secret
            )
            player_id = auth_token.player_id
            is_guest = auth_token.is_guest

        # Get ticket ID from path.
        ticket_id = (
            event.get("pathParameters", {}).get("ticket_id")
        )
        if not ticket_id:
            return error_response(
                400, "MISSING_TICKET", "Missing ticket_id"
            )

        # Try non-blocking status check first.
        status = asyncio.run(
            gamelift.get_ticket_status(ticket_id)
        )

        # If still in progress, return current status.
        if status["status"] in [
            "queued", "searching", "placing"
        ]:
            return {
                "statusCode": 200,
                "headers": _HEADERS,
                "body": json.dumps(status),
            }

        # If completed, do full poll to get connection info.
        if status["status"] == "completed":
            result = asyncio.run(
                gamelift.poll_matchmaking(ticket_id)
            )

            if not is_guest:
                # Transition session state from matchmaking
                # to in_match now that the session ID is
                # known.
                active_session_service.transition_to_in_match(
                    player_id, result.game_session_id
                )

            # Select level from stored session preferences.
            session_prefs_data = _pending_session_prefs.pop(
                ticket_id, {}
            )
            session_prefs = parse_session_preference(
                session_prefs_data
            )
            selected_level_id = select_level_for_match(
                [session_prefs.level]
            )

            server_address, server_port = (
                _resolve_server_address(result)
            )

            return {
                "statusCode": 200,
                "headers": _HEADERS,
                "body": json.dumps(
                    {
                        "status": "success",
                        "ticket_id": ticket_id,
                        "server_version": os.environ.get(
                            "GAME_VERSION", "0.1.0"
                        ),
                        "protocol_version": int(
                            os.environ.get(
                                "PROTOCOL_VERSION", "1"
                            )
                        ),
                        "game_session_id": (
                            result.game_session_id
                        ),
                        "server_ip": server_address,
                        "server_port": server_port,
                        "player_session_ids": (
                            result.player_session_ids
                        ),
                        "selected_level_id": (
                            selected_level_id
                        ),
                        "transport_type": (
                            result.transport_type
                        ),
                    }
                ),
            }

        # Failed/cancelled/timeout.
        error_code = status["status"].upper()
        message = f"Matchmaking {status['status']}"

        # Check if cancellation was caused by another
        # device starting matchmaking for the same
        # account. If so, do not clear the session
        # (it belongs to the other device now).
        is_concurrent_override = False
        if (
            not is_guest
            and status["status"] == "cancelled"
        ):
            current = (
                active_session_service
                .get_active_session(player_id)
            )
            if (
                current is not None
                and current.get("session_id")
                    != ticket_id
                and current.get("session_id")
                    != "pending"
            ):
                is_concurrent_override = True
                error_code = (
                    "CONCURRENT_SESSION_OVERRIDE"
                )
                message = (
                    "Matchmaking cancelled because"
                    " your account started"
                    " matchmaking on another device."
                )

        if not is_guest and not is_concurrent_override:
            active_session_service.clear_session(
                player_id
            )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "failed",
                    "error_code": error_code,
                    "message": message,
                }
            ),
        }

    except ValueError as e:
        return error_response(400, "VALIDATION_ERROR", str(e))
    except Exception as e:
        logger.exception("Status check error")
        return error_response(500, "INTERNAL_ERROR", "Internal server error")


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def leave_matchmaking(event: Dict[str, Any], context: LambdaContext) -> Dict:
    """
    POST /matchmaking/leave
    Release the active session lock so the player can re-queue.
    Cancels a pending GameLift ticket if one exists.
    """
    try:
        # Auth.
        auth_header = event.get("headers", {}).get("Authorization", "")
        if not auth_header.startswith("Bearer "):
            return error_response(401, "MISSING_AUTH", "Missing authorization")

        jwt_token = auth_header[7:]
        jwt_secret = secrets_service.get_jwt_secret()

        if jwt_token.startswith("DEBUG_"):
            player_id = jwt_token
        else:
            auth_token = AuthToken.from_jwt(jwt_token, jwt_secret)
            player_id = auth_token.player_id

        # Read current session to cancel any pending
        # matchmaking ticket before deleting the record.
        session = active_session_service.get_active_session(
            player_id
        )
        if session and session.get("state") == "matchmaking":
            ticket_id = session.get("session_id", "")
            if ticket_id and ticket_id != "pending":
                try:
                    asyncio.run(
                        gamelift.cancel_matchmaking(ticket_id)
                    )
                except Exception:
                    logger.warning(
                        "Failed to cancel ticket %s",
                        ticket_id,
                    )

        active_session_service.clear_session(player_id)

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps({"status": "ok"}),
        }

    except ValueError as e:
        return error_response(400, "VALIDATION_ERROR", str(e))
    except Exception as e:
        logger.exception("Leave matchmaking error")
        return error_response(500, "INTERNAL_ERROR", "Internal server error")


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def warmup_dns(event: Dict[str, Any], context: LambdaContext) -> Dict:
    """
    POST /internal/dns/warmup
    Pre-create Route 53 DNS record for a game session.
    Called by the game server when the session starts, well
    before clients poll for results. This gives DNS time to
    propagate so web clients can connect immediately.
    """
    try:
        # Validate server API key.
        headers = event.get("headers", {})
        server_key = (
            headers.get("X-Server-Key", "")
            or headers.get("x-server-key", "")
        )
        if not server_key:
            return error_response(
                401, "MISSING_AUTH", "Missing server key"
            )
        expected = secrets_service.get_secret_string(
            "hopnbop/server-api-key"
        )
        if server_key != expected:
            return error_response(
                401, "UNAUTHORIZED", "Invalid server key"
            )

        body = json.loads(event.get("body", "{}"))
        game_session_id = body.get("game_session_id", "")

        if not game_session_id:
            return error_response(
                400,
                "INVALID_INPUT",
                "game_session_id required",
            )

        # Look up the server IP from GameLift.
        server_ip = body.get("server_ip", "")
        if not server_ip:
            server_ip = _lookup_server_ip(
                game_session_id
            )
        if not server_ip:
            return error_response(
                404,
                "SESSION_NOT_FOUND",
                "Could not resolve server IP",
            )

        hostname = dns_service.create_game_session_record(
            game_session_id, server_ip
        )
        logger.info(
            "DNS warmup complete",
            extra={
                "hostname": hostname,
                "server_ip": server_ip,
            },
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {"status": "ok", "hostname": hostname}
            ),
        }

    except Exception as e:
        logger.exception("DNS warmup error")
        return error_response(
            500, "INTERNAL_ERROR", "DNS warmup failed"
        )


def error_response(
    status_code: int,
    error_code: str,
    message: str,
    retry_after: int = None,
    retry_after_seconds: int = 0,
) -> Dict:
    """Format error response."""
    body = {
        "status": "error",
        "error_code": error_code,
        "message": message,
    }

    if retry_after_seconds > 0:
        body["retry_after_seconds"] = retry_after_seconds

    headers = dict(_HEADERS)
    if retry_after:
        headers["Retry-After"] = str(retry_after)

    return {
        "statusCode": status_code,
        "headers": headers,
        "body": json.dumps(body),
    }
