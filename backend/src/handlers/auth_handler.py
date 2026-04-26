"""Lambda handlers for authentication operations."""

import json
import os
import secrets
import asyncio
import uuid
from datetime import datetime, timedelta
from typing import Dict, Any
import boto3
import jwt
from aws_lambda_powertools import Logger, Metrics, Tracer
from aws_lambda_powertools.metrics import MetricUnit
from aws_lambda_powertools.utilities.typing import LambdaContext

import sys

sys.path.append(os.path.join(os.path.dirname(__file__), ".."))

from services.auth_service import AuthService, AuthToken
from services.player_service import PlayerService
from services.profile_service import ProfileService
from services.provider_mapping_service import ProviderMappingService
from services.settings_service import SettingsService
from services.leaderboard_service import LeaderboardService
from services.friends_service import FriendsService

logger = Logger()
tracer = Tracer()
metrics = Metrics()

# Initialize services.
auth_service = AuthService(token_lifetime_hours=24)
player_service = PlayerService()
# profile_service writes to the new game_profiles table; auth_handler
# dual-writes alongside player_service so the new table populates
# as players sign in. account_service is intentionally NOT used as a
# writer yet — player_service remains the sole writer to the accounts
# row so the two services don't fight over the same item.
profile_service = ProfileService()
provider_mapping_service = ProviderMappingService()
settings_service = SettingsService()
leaderboard_service = LeaderboardService()
friends_service = FriendsService()

_GAME_VERSION = os.environ.get("GAME_VERSION", "0.1.0")
_PROTOCOL_VERSION = int(os.environ.get("PROTOCOL_VERSION", "1"))
_DEFAULT_GAME_ID = os.environ.get("DEFAULT_GAME_ID", "hopnbop")

# CORS headers included in every response.
_HEADERS = {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
}


@tracer.capture_lambda_handler
@logger.inject_lambda_context
@metrics.log_metrics
def login(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /auth/login - Authenticate with an OAuth provider."""
    try:
        body = json.loads(event.get("body", "{}"))
        provider = body.get("provider", "")
        auth_code = body.get("auth_code", "")
        redirect_uri = body.get("redirect_uri", "")
        # game_id binds the issued JWT to a specific game so per-game
        # handlers reject cross-game token reuse. Older clients that
        # don't yet send a game_id get DEFAULT_GAME_ID baked in.
        game_id = body.get("game_id", "")

        if not provider or not auth_code:
            return _error(
                400,
                "MISSING_PARAMS",
                "Missing provider or auth_code",
            )

        # Authenticate with provider. Returns provider_id
        # and display_name.
        auth_result = asyncio.run(
            auth_service.authenticate(
                provider, auth_code, redirect_uri
            )
        )

        # Look up canonical player_id.
        player_id = asyncio.run(
            provider_mapping_service.lookup(
                auth_result.provider,
                auth_result.provider_id,
            )
        )

        if player_id is None:
            # New player.
            player_id = PlayerService.generate_player_id()
            asyncio.run(
                provider_mapping_service.create(
                    auth_result.provider,
                    auth_result.provider_id,
                    player_id,
                )
            )

        # Read consent fields from request.
        consent_accepted_at = int(
            body.get("consent_accepted_at", 0)
        )
        consent_legal_version = body.get(
            "consent_legal_version", ""
        )

        # Get or create player profile.
        player_profile = asyncio.run(
            player_service.get_or_create_player(
                player_id,
                auth_result.display_name,
                {auth_result.provider: auth_result.provider_id},
                consent_accepted_at=consent_accepted_at,
                consent_legal_version=consent_legal_version,
                profile_image_url=(
                    auth_result.profile_image_url
                ),
            )
        )

        # Dual-write the per-game profile row. This is the new
        # game-aware view of the same player. Eventually replaces
        # the per-game fields on the legacy accounts row, but for
        # now both exist side by side. Use the resolved game_id
        # (caller-supplied, falls back to DEFAULT_GAME_ID) so the
        # row is keyed correctly.
        try:
            asyncio.run(
                profile_service.get_or_create(
                    player_id,
                    game_id or _DEFAULT_GAME_ID,
                )
            )
        except Exception as e:
            # Profile dual-write failures must not block sign-in
            # — the legacy player row was already written above.
            # Log loudly so we notice.
            logger.warning(
                "profile_service dual-write failed: "
                f"player_id={player_id} game_id={game_id} "
                f"err={e}"
            )

        # Issue tokens.
        auth_token = auth_service.create_auth_token(
            player_id,
            auth_result.display_name,
            auth_result.provider,
            game_id=game_id,
        )
        jwt_token = auth_token.to_jwt(
            auth_service.jwt_secret
        )
        refresh_token = secrets.token_hex(32)
        asyncio.run(
            player_service.store_refresh_token(
                player_id, refresh_token
            )
        )

        logger.info(
            f"User authenticated: {player_id} "
            f"via {provider}"
        )
        metrics.add_dimension(
            name="provider", value=provider
        )
        metrics.add_metric(
            name="auth_success",
            unit=MetricUnit.Count,
            value=1,
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "jwt_token": jwt_token,
                    "refresh_token": refresh_token,
                    "player_id": player_id,
                    "display_name": auth_result.display_name,
                    "is_anonymous": False,
                    "rating": player_profile.rating,
                    "game_version": _GAME_VERSION,
                    "protocol_version": _PROTOCOL_VERSION,
                    "expires_at": int(
                        auth_token.expires_at.timestamp()
                    ),
                    "linked_providers": list(
                        player_profile.auth_providers.keys()
                    ),
                    "consent_accepted_at": (
                        player_profile.consent_accepted_at
                    ),
                    "consent_legal_version": (
                        player_profile.consent_legal_version
                    ),
                    "profile_image_url": (
                        player_profile.profile_image_url
                    ),
                }
            ),
        }

    except ValueError as e:
        logger.error(f"Authentication failed: {e}")
        metrics.add_dimension(
            name="provider", value=provider
        )
        metrics.add_metric(
            name="auth_failure",
            unit=MetricUnit.Count,
            value=1,
        )
        return _error(401, "AUTH_FAILED", str(e))
    except Exception:
        logger.exception("Login error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
@metrics.log_metrics
def anonymous_login(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /auth/anon - Create or retrieve anonymous session."""
    try:
        body = json.loads(event.get("body", "{}"))
        device_id = body.get("device_id", "")
        # game_id binds the issued JWT to a specific game.
        game_id = body.get("game_id", "")

        if not device_id:
            return _error(
                400,
                "MISSING_PARAMS",
                "Missing device_id",
            )

        # Look up existing anonymous player by device_id
        # via provider mapping (provider="anonymous").
        player_id = asyncio.run(
            provider_mapping_service.lookup(
                "anonymous", device_id
            )
        )

        if player_id is None:
            player_id = PlayerService.generate_player_id()
            asyncio.run(
                provider_mapping_service.create(
                    "anonymous", device_id, player_id
                )
            )

        # Read consent fields from request.
        consent_accepted_at = int(
            body.get("consent_accepted_at", 0)
        )
        consent_legal_version = body.get(
            "consent_legal_version", ""
        )

        display_name = f"Player_{player_id[2:10]}"

        player_profile = asyncio.run(
            player_service.get_or_create_player(
                player_id,
                display_name,
                {},
                is_anonymous=True,
                device_id=device_id,
                consent_accepted_at=consent_accepted_at,
                consent_legal_version=consent_legal_version,
            )
        )

        # Dual-write the per-game profile row (see login() comment).
        try:
            asyncio.run(
                profile_service.get_or_create(
                    player_id,
                    game_id or _DEFAULT_GAME_ID,
                )
            )
        except Exception as e:
            logger.warning(
                "profile_service dual-write failed: "
                f"player_id={player_id} game_id={game_id} "
                f"err={e}"
            )

        # Issue tokens.
        auth_token = auth_service.create_auth_token(
            player_id,
            display_name,
            "anonymous",
            is_anonymous=True,
            game_id=game_id,
        )
        jwt_token = auth_token.to_jwt(
            auth_service.jwt_secret
        )
        refresh_token = secrets.token_hex(32)
        asyncio.run(
            player_service.store_refresh_token(
                player_id, refresh_token
            )
        )

        logger.info(f"Anonymous login: {player_id}")
        metrics.add_dimension(
            name="provider", value="anonymous"
        )
        metrics.add_metric(
            name="auth_success",
            unit=MetricUnit.Count,
            value=1,
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "jwt_token": jwt_token,
                    "refresh_token": refresh_token,
                    "player_id": player_id,
                    "display_name": display_name,
                    "is_anonymous": True,
                    "rating": player_profile.rating,
                    "game_version": _GAME_VERSION,
                    "protocol_version": _PROTOCOL_VERSION,
                    "expires_at": int(
                        auth_token.expires_at.timestamp()
                    ),
                    "linked_providers": list(
                        player_profile.auth_providers.keys()
                    ),
                    "consent_accepted_at": (
                        player_profile.consent_accepted_at
                    ),
                    "consent_legal_version": (
                        player_profile.consent_legal_version
                    ),
                    "profile_image_url": "",
                }
            ),
        }

    except Exception:
        metrics.add_dimension(
            name="provider", value="anonymous"
        )
        metrics.add_metric(
            name="auth_failure",
            unit=MetricUnit.Count,
            value=1,
        )
        logger.exception("Anonymous login error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
@metrics.log_metrics
def guest_login(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /auth/guest - Issue an ephemeral guest JWT.

    Creates a short-lived token for an anonymous player
    without writing anything to the database. Used when
    an anonymous client starts matchmaking.
    """
    try:
        body = json.loads(event.get("body", "{}"))
        # game_id binds the issued JWT to a specific game.
        game_id = body.get("game_id", "") or os.environ.get(
            "DEFAULT_GAME_ID", "hopnbop"
        )

        player_id = f"PL_guest_{uuid.uuid4().hex[:16]}"

        now = datetime.now()
        expires_at = now + timedelta(hours=1)
        auth_token = AuthToken(
            player_id=player_id,
            display_name="",
            provider="guest",
            is_anonymous=True,
            is_guest=True,
            issued_at=now,
            expires_at=expires_at,
            game_id=game_id,
        )
        jwt_secret = auth_service.jwt_secret
        jwt_token = auth_token.to_jwt(jwt_secret)

        logger.info(f"Guest login: {player_id}")
        metrics.add_dimension(
            name="provider", value="guest"
        )
        metrics.add_metric(
            name="auth_success",
            unit=MetricUnit.Count,
            value=1,
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "jwt_token": jwt_token,
                    "player_id": player_id,
                    "game_version": _GAME_VERSION,
                    "protocol_version": (
                        _PROTOCOL_VERSION
                    ),
                    "expires_at": int(
                        expires_at.timestamp()
                    ),
                }
            ),
        }

    except Exception:
        logger.exception("Guest login error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def refresh(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /auth/refresh - Refresh an expired JWT."""
    try:
        body = json.loads(event.get("body", "{}"))
        player_id = body.get("player_id", "")
        refresh_token = body.get("refresh_token", "")
        # Carry game_id forward into the rotated token. Clients
        # typically pass the game_id they used at sign-in time.
        game_id = body.get("game_id", "")

        if not player_id or not refresh_token:
            return _error(
                400,
                "MISSING_PARAMS",
                "Missing player_id or refresh_token",
            )

        # Verify refresh token.
        is_valid = asyncio.run(
            player_service.verify_refresh_token(
                player_id, refresh_token
            )
        )
        if not is_valid:
            return _error(
                401,
                "INVALID_REFRESH",
                "Invalid or expired refresh token",
            )

        # Get player profile for display name and provider.
        profile = asyncio.run(
            player_service.get_player(player_id)
        )
        if profile is None:
            return _error(
                404, "NOT_FOUND", "Player not found"
            )

        # Determine primary provider.
        provider = "anonymous"
        if profile.auth_providers:
            provider = next(iter(profile.auth_providers))

        # Rotate: issue new tokens and invalidate old.
        auth_token = auth_service.create_auth_token(
            player_id,
            profile.display_name,
            provider,
            is_anonymous=profile.is_anonymous,
            game_id=game_id,
        )
        new_jwt = auth_token.to_jwt(
            auth_service.jwt_secret
        )
        new_refresh = secrets.token_hex(32)
        asyncio.run(
            player_service.rotate_refresh_token(
                player_id, refresh_token, new_refresh
            )
        )

        logger.info(f"Token refreshed: {player_id}")

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "jwt_token": new_jwt,
                    "refresh_token": new_refresh,
                    "player_id": player_id,
                    "display_name": profile.display_name,
                    "is_anonymous": profile.is_anonymous,
                    "rating": profile.rating,
                    "game_version": _GAME_VERSION,
                    "protocol_version": _PROTOCOL_VERSION,
                    "expires_at": int(
                        auth_token.expires_at.timestamp()
                    ),
                    "linked_providers": list(
                        profile.auth_providers.keys()
                    ),
                    "consent_accepted_at": (
                        profile.consent_accepted_at
                    ),
                    "consent_legal_version": (
                        profile.consent_legal_version
                    ),
                    "profile_image_url": (
                        profile.profile_image_url
                    ),
                }
            ),
        }

    except Exception:
        logger.exception("Refresh error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def link_account(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /auth/link - Link a new provider to an existing account."""
    try:
        # Validate JWT from Authorization header.
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
            current_token = AuthToken.from_jwt(
                token_str, auth_service.jwt_secret
            )
        except ValueError as e:
            return _error(401, "UNAUTHORIZED", str(e))

        body = json.loads(event.get("body", "{}"))
        provider = body.get("provider", "")
        auth_code = body.get("auth_code", "")
        redirect_uri = body.get("redirect_uri", "")

        if not provider or not auth_code:
            return _error(
                400,
                "MISSING_PARAMS",
                "Missing provider or auth_code",
            )

        # Authenticate with the new provider.
        auth_result = asyncio.run(
            auth_service.authenticate(
                provider, auth_code, redirect_uri
            )
        )

        # Check if this provider ID is already mapped.
        existing_player_id = asyncio.run(
            provider_mapping_service.lookup(
                auth_result.provider,
                auth_result.provider_id,
            )
        )

        if existing_player_id is not None:
            if existing_player_id == current_token.player_id:
                # Already linked to this account.
                return {
                    "statusCode": 200,
                    "headers": _HEADERS,
                    "body": json.dumps(
                        {
                            "status": "success",
                            "message": "Provider already linked",
                        }
                    ),
                }
            else:
                # Issue a short-lived merge token so
                # the client can confirm a merge without
                # re-running OAuth (codes are single-use).
                merge_token = _make_merge_token(
                    current_token.player_id,
                    existing_player_id,
                    auth_service.jwt_secret,
                )
                return {
                    "statusCode": 409,
                    "headers": _HEADERS,
                    "body": json.dumps({
                        "status": "error",
                        "error_code": "PROVIDER_CONFLICT",
                        "message": (
                            "This provider account is"
                            " already linked to a"
                            " different player"
                        ),
                        "merge_token": merge_token,
                    }),
                }

        # Add provider to current player.
        asyncio.run(
            provider_mapping_service.create(
                auth_result.provider,
                auth_result.provider_id,
                current_token.player_id,
            )
        )
        asyncio.run(
            player_service.add_provider(
                current_token.player_id,
                auth_result.provider,
                auth_result.provider_id,
                display_name=auth_result.display_name,
                profile_image_url=(
                    auth_result.profile_image_url
                ),
            )
        )

        # Fetch updated profile for linked_providers.
        updated_profile = asyncio.run(
            player_service.get_player(
                current_token.player_id
            )
        )

        logger.info(
            f"Linked {provider} to {current_token.player_id}"
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "message": "Provider linked",
                    "provider": auth_result.provider,
                    "linked_providers": list(
                        updated_profile.auth_providers.keys()
                    ) if updated_profile else [],
                    "display_name": (
                        updated_profile.display_name
                        if updated_profile
                        else auth_result.display_name
                    ),
                    "profile_image_url": (
                        updated_profile.profile_image_url
                        if updated_profile
                        else ""
                    ),
                }
            ),
        }

    except ValueError as e:
        logger.error(f"Link failed: {e}")
        return _error(401, "AUTH_FAILED", str(e))
    except Exception:
        logger.exception("Link account error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def unlink_account(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /auth/unlink - Unlink a provider from an account."""
    try:
        # Validate JWT from Authorization header.
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
            current_token = AuthToken.from_jwt(
                token_str, auth_service.jwt_secret
            )
        except ValueError as e:
            return _error(401, "UNAUTHORIZED", str(e))

        body = json.loads(event.get("body", "{}"))
        provider = body.get("provider", "")

        if not provider:
            return _error(
                400,
                "MISSING_PARAMS",
                "Missing provider",
            )

        # Get current profile.
        profile = asyncio.run(
            player_service.get_player(
                current_token.player_id
            )
        )
        if profile is None:
            return _error(
                404, "NOT_FOUND", "Player not found"
            )

        # Check provider is actually linked.
        if provider not in profile.auth_providers:
            return _error(
                400,
                "NOT_LINKED",
                "Provider is not linked to this account",
            )

        # Last-provider safety guard. Cannot unlink if
        # this is the only auth method and the player has
        # no device_id fallback.
        provider_count = len(profile.auth_providers)
        has_device_fallback = bool(profile.device_id)
        if provider_count <= 1 and not has_device_fallback:
            return _error(
                409,
                "LAST_PROVIDER",
                "Cannot unlink the only auth provider",
            )

        # Remove provider mapping.
        provider_id = profile.auth_providers[provider]
        asyncio.run(
            provider_mapping_service.delete(
                provider, provider_id
            )
        )

        # Remove provider from player profile.
        asyncio.run(
            player_service.remove_provider(
                current_token.player_id, provider
            )
        )

        # Fetch updated profile for linked_providers.
        updated_profile = asyncio.run(
            player_service.get_player(
                current_token.player_id
            )
        )

        logger.info(
            f"Unlinked {provider} from "
            f"{current_token.player_id}"
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "message": "Provider unlinked",
                    "provider": provider,
                    "linked_providers": list(
                        updated_profile.auth_providers.keys()
                    ) if updated_profile else [],
                }
            ),
        }

    except Exception:
        logger.exception("Unlink account error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def merge_accounts(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /auth/merge - Merge another account into this one.

    Requires a merge_token issued by POST /auth/link when a
    PROVIDER_CONFLICT is detected. The token encodes both
    player IDs and expires in 5 minutes.
    """
    try:
        # Validate Bearer JWT (primary account).
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
            current_token = AuthToken.from_jwt(
                token_str, auth_service.jwt_secret
            )
        except ValueError as e:
            return _error(401, "UNAUTHORIZED", str(e))

        body = json.loads(event.get("body", "{}"))
        merge_token_str = body.get("merge_token", "")
        if not merge_token_str:
            return _error(
                400,
                "MISSING_PARAMS",
                "Missing merge_token",
            )

        # Verify merge token.
        try:
            merge_payload = jwt.decode(
                merge_token_str,
                auth_service.jwt_secret,
                algorithms=["HS256"],
            )
        except jwt.ExpiredSignatureError:
            return _error(
                400,
                "MERGE_TOKEN_EXPIRED",
                "Merge token has expired",
            )
        except jwt.InvalidTokenError:
            return _error(
                400,
                "INVALID_MERGE_TOKEN",
                "Invalid merge token",
            )

        if merge_payload.get("sub") != "merge":
            return _error(
                400,
                "INVALID_MERGE_TOKEN",
                "Invalid merge token",
            )

        primary_player_id = merge_payload.get("primary")
        secondary_player_id = merge_payload.get("secondary")

        if primary_player_id != current_token.player_id:
            return _error(
                403,
                "FORBIDDEN",
                "Merge token does not match current player",
            )

        # Fetch secondary profile before any writes so we
        # have the device_id for cleanup.
        secondary_profile = asyncio.run(
            player_service.get_player(secondary_player_id)
        )
        if secondary_profile is None:
            return _error(
                404,
                "NOT_FOUND",
                "Secondary player not found",
            )

        # Merge stats from secondary into primary.
        merged_profile = asyncio.run(
            player_service.merge_players(
                primary_player_id, secondary_player_id
            )
        )
        if merged_profile is None:
            return _error(
                404,
                "NOT_FOUND",
                "Primary player not found",
            )

        # Re-point all secondary provider mappings to
        # the primary player.
        secondary_mappings = asyncio.run(
            provider_mapping_service.list_by_player(
                secondary_player_id
            )
        )
        for mapping in secondary_mappings:
            asyncio.run(
                provider_mapping_service.create(
                    mapping["provider"],
                    mapping["provider_id"],
                    primary_player_id,
                )
            )

        # Migrate secondary's friends to primary.
        asyncio.run(
            friends_service.migrate_friends(
                secondary_player_id, primary_player_id
            )
        )

        # Remove secondary from leaderboard.
        leaderboard_service.remove_player(
            secondary_player_id
        )

        # Delete secondary player data.
        _delete_match_history(secondary_player_id)
        settings_service.delete_settings(
            secondary_player_id
        )
        if secondary_profile.device_id:
            asyncio.run(
                provider_mapping_service.delete(
                    "anonymous",
                    secondary_profile.device_id,
                )
            )
        asyncio.run(
            player_service.delete_player(secondary_player_id)
        )

        logger.info(
            f"Merged {secondary_player_id}"
            f" into {primary_player_id}"
        )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps({
                "status": "success",
                "message": "Accounts merged",
                "linked_providers": list(
                    merged_profile.auth_providers.keys()
                ),
                "display_name": (
                    merged_profile.display_name
                ),
                "profile_image_url": (
                    merged_profile.profile_image_url
                ),
            }),
        }

    except Exception:
        logger.exception("Merge accounts error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def delete_account(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """DELETE /auth/account - Delete player and all associated data."""
    try:
        # Validate JWT from Authorization header.
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
            current_token = AuthToken.from_jwt(
                token_str, auth_service.jwt_secret
            )
        except ValueError as e:
            return _error(401, "UNAUTHORIZED", str(e))

        player_id = current_token.player_id

        # Get player profile to find linked providers.
        profile = asyncio.run(
            player_service.get_player(player_id)
        )
        if profile is None:
            return _error(
                404, "NOT_FOUND", "Player not found"
            )

        # Archive consent record before deletion.
        if profile.consent_accepted_at > 0:
            _archive_consent(
                player_id,
                profile.consent_accepted_at,
                profile.consent_legal_version,
            )

        # Delete all provider mappings.
        for provider, provider_id in (
            profile.auth_providers.items()
        ):
            asyncio.run(
                provider_mapping_service.delete(
                    provider, provider_id
                )
            )

        # Delete anonymous device mapping if present.
        if profile.device_id:
            asyncio.run(
                provider_mapping_service.delete(
                    "anonymous", profile.device_id
                )
            )

        # Delete match history.
        _delete_match_history(player_id)

        # Delete cloud settings.
        settings_service.delete_settings(player_id)

        # Delete leaderboard entries.
        leaderboard_service.remove_player(player_id)

        # Delete all friend relationships.
        asyncio.run(
            friends_service.delete_all_friends(player_id)
        )

        # Delete player profile.
        asyncio.run(player_service.delete_player(player_id))

        logger.info(f"Account deleted: {player_id}")

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "status": "success",
                    "message": "Account deleted",
                }
            ),
        }

    except Exception:
        logger.exception("Delete account error")
        return _error(
            500, "INTERNAL_ERROR", "Internal server error"
        )


# 3 years in seconds.
_CONSENT_AUDIT_TTL_SECONDS = 3 * 365 * 24 * 60 * 60


def _archive_consent(
    player_id: str,
    consent_accepted_at: int,
    consent_legal_version: str,
) -> None:
    """Write consent record to audit table with TTL."""
    dynamodb = boto3.resource("dynamodb")
    table_name = os.environ.get(
        "CONSENT_AUDIT_TABLE", "hopnbop-consent-audit"
    )
    table = dynamodb.Table(table_name)
    now = int(datetime.now().timestamp())
    table.put_item(
        Item={
            "player_id": player_id,
            "consent_accepted_at": consent_accepted_at,
            "consent_legal_version": (
                consent_legal_version
            ),
            "deleted_at": now,
            "expires_at": now + _CONSENT_AUDIT_TTL_SECONDS,
        }
    )


def _delete_match_history(player_id: str) -> None:
    """Delete all match history entries for a player."""
    dynamodb = boto3.resource("dynamodb")
    table_name = os.environ.get(
        "MATCH_HISTORY_TABLE", "hopnbop-match-history"
    )
    table = dynamodb.Table(table_name)

    # Query all match entries for this player.
    response = table.query(
        KeyConditionExpression=(
            boto3.dynamodb.conditions.Key("player_id").eq(
                player_id
            )
        ),
        ProjectionExpression=(
            "player_id, match_timestamp"
        ),
    )

    # Batch delete all items.
    with table.batch_writer() as batch:
        for item in response.get("Items", []):
            batch.delete_item(
                Key={
                    "player_id": item["player_id"],
                    "match_timestamp": item[
                        "match_timestamp"
                    ],
                }
            )

        # Handle pagination.
        while "LastEvaluatedKey" in response:
            response = table.query(
                KeyConditionExpression=(
                    boto3.dynamodb.conditions.Key(
                        "player_id"
                    ).eq(player_id)
                ),
                ProjectionExpression=(
                    "player_id, match_timestamp"
                ),
                ExclusiveStartKey=response[
                    "LastEvaluatedKey"
                ],
            )
            for item in response.get("Items", []):
                batch.delete_item(
                    Key={
                        "player_id": item["player_id"],
                        "match_timestamp": item[
                            "match_timestamp"
                        ],
                    }
                )


def _make_merge_token(
    primary_player_id: str,
    secondary_player_id: str,
    secret: str,
) -> str:
    """Issue a short-lived JWT for confirming an account merge.

    The token encodes both player IDs and expires in 5
    minutes. The client presents it to POST /auth/merge
    after the user confirms, avoiding a second OAuth round-
    trip (OAuth codes are single-use).
    """
    now = datetime.now()
    payload = {
        "sub": "merge",
        "primary": primary_player_id,
        "secondary": secondary_player_id,
        "exp": int(
            (now + timedelta(minutes=5)).timestamp()
        ),
    }
    return jwt.encode(payload, secret, algorithm="HS256")


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
