"""Request/response models for /v1/auth/* endpoints.

Models:
  POST /v1/auth/login    OAuthLoginRequest    -> AuthSuccessResponse
  POST /v1/auth/anon     AnonLoginRequest     -> AuthSuccessResponse
  POST /v1/auth/guest    GuestLoginRequest    -> AuthSuccessResponse
  POST /v1/auth/refresh  RefreshRequest       -> AuthSuccessResponse
  POST /v1/auth/link     LinkAccountRequest   -> SuccessResponse
  POST /v1/auth/unlink   UnlinkAccountRequest -> SuccessResponse
  POST /v1/auth/merge    MergeRequest         -> AuthSuccessResponse
  DELETE /v1/auth/account                     -> SuccessResponse

Errors return ErrorResponse from common.py.
"""

from __future__ import annotations

from typing import Optional

from pydantic import BaseModel, ConfigDict, Field


class _BaseModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


# --------- Requests ---------------------------------------------


class OAuthLoginRequest(_BaseModel):
    """POST /v1/auth/login body."""

    provider: str = Field(
        description=(
            "OAuth provider id (steam, epic, google, "
            "facebook, apple)."
        ),
    )
    auth_code: str = Field(
        description=(
            "Provider-issued authorization code or token."
        ),
    )
    redirect_uri: Optional[str] = Field(
        default=None,
        description=(
            "Redirect URI used during browser OAuth flows "
            "(google, facebook, apple). Required for those "
            "providers; ignored for steam/epic."
        ),
    )
    game_id: Optional[str] = Field(
        default=None,
        description=(
            "The game the user is signing in from. Bound into "
            "the issued JWT so per-game handlers can reject "
            "cross-game token reuse. Empty/missing falls back "
            "to the platform's DEFAULT_GAME_ID."
        ),
    )
    consent_accepted_at: Optional[int] = Field(
        default=None,
        description=(
            "Unix seconds when the user accepted the legal "
            "consent. Stored on the account; used by the "
            "account-export endpoint."
        ),
    )
    consent_legal_version: Optional[str] = Field(
        default=None,
        description=(
            "Version string of the legal documents the user "
            "accepted (e.g. 'v1.2')."
        ),
    )


class AnonLoginRequest(_BaseModel):
    """POST /v1/auth/anon body."""

    device_id: str = Field(
        description=(
            "A stable per-device identifier the client "
            "generates and persists. Repeat sign-ins from the "
            "same device return the same player_id."
        ),
    )
    game_id: Optional[str] = Field(default=None)
    consent_accepted_at: Optional[int] = Field(default=None)
    consent_legal_version: Optional[str] = Field(default=None)


class GuestLoginRequest(_BaseModel):
    """POST /v1/auth/guest body. Body is optional."""

    game_id: Optional[str] = Field(default=None)


class RefreshRequest(_BaseModel):
    """POST /v1/auth/refresh body."""

    player_id: str
    refresh_token: str
    game_id: Optional[str] = Field(
        default=None,
        description=(
            "Carry the original game_id forward into the "
            "rotated JWT."
        ),
    )


class LinkAccountRequest(_BaseModel):
    """POST /v1/auth/link body. Adds an OAuth identity to the
    currently-signed-in (anonymous or guest) account."""

    provider: str
    auth_code: str
    redirect_uri: Optional[str] = None


class UnlinkAccountRequest(_BaseModel):
    """POST /v1/auth/unlink body."""

    provider: str = Field(
        description="Provider to unlink (e.g. 'google').",
    )


class MergeRequest(_BaseModel):
    """POST /v1/auth/merge body."""

    provider: str
    auth_code: str
    redirect_uri: Optional[str] = None


# --------- Responses --------------------------------------------


class AuthSuccessResponse(_BaseModel):
    """Returned by login / anon / guest / refresh / merge."""

    status: str = Field(default="success")
    jwt_token: str = Field(
        description=(
            "Bearer token. Pass as 'Authorization: Bearer "
            "<jwt_token>' on subsequent requests."
        ),
    )
    refresh_token: str = Field(
        description=(
            "Use with POST /v1/auth/refresh to rotate the JWT "
            "without re-signing in. Single-use; rotates on "
            "every refresh."
        ),
    )
    player_id: str
    display_name: str
    is_anonymous: bool
    rating: int = Field(
        description=(
            "Per-game rating from the legacy account row. "
            "After Phase 1f-Q the per-game rating moves to "
            "the game_profiles row; this response field stays "
            "for backward compatibility."
        ),
    )
    game_version: str = Field(
        description=(
            "Server-side display version. Legacy single-game "
            "field; per-game versions live in the games table."
        ),
    )
    protocol_version: int = Field(
        description=(
            "Server-side network protocol version. Legacy "
            "single-game field."
        ),
    )
    expires_at: int = Field(
        description="JWT expiry as Unix seconds.",
    )
    linked_providers: list[str] = Field(
        description=(
            "Names of OAuth providers linked to this account "
            "(e.g. ['google', 'steam'])."
        ),
    )
    consent_accepted_at: int = Field(default=0)
    consent_legal_version: str = Field(default="")
    profile_image_url: Optional[str] = Field(default=None)
