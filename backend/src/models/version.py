"""Request/response models for /v1/version."""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict, Field


class _BaseModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class VersionResponse(_BaseModel):
    """GET /v1/version response.

    Unauthenticated. Clients fetch this at app startup to verify
    they're protocol-compatible with the platform before
    attempting auth.
    """

    status: str = Field(default="success")
    platform_version: str = Field(
        description=(
            "Display version of the platform backend (semver)."
        ),
    )
    # Legacy single-game versions kept for backward
    # compatibility. New per-game versions are in the games
    # config table; clients should look those up via
    # /v1/games (TODO: add when that endpoint exists).
    game_version: str = Field(
        description=(
            "Legacy GAME_VERSION env var on the backend. "
            "Per-game versions live in the games config table."
        ),
    )
    protocol_version: int = Field(
        description=(
            "Legacy single-game protocol_version. Per-game "
            "protocol_versions are in the games config table."
        ),
    )
