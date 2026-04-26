"""Request/response models for /v1/presence/* endpoints."""

from __future__ import annotations

from typing import Optional

from pydantic import BaseModel, ConfigDict, Field


class _BaseModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class PresenceHeartbeatRequest(_BaseModel):
    """PUT /v1/presence body.

    Clients call this every 60-90 seconds while running. The
    server stores the row with a TTL, so missing two heartbeats
    decays the player to offline.
    """

    game_id: str = Field(
        description=(
            "The game the player is currently in. Required."
        ),
    )
    status: str = Field(
        default="online",
        description=(
            "Free-form status string. Standard values: "
            "'online', 'in_match', 'away'."
        ),
    )
    rich_presence: Optional[str] = Field(
        default=None,
        description=(
            "Free-text presence the friends UI may surface "
            "(e.g. 'In lobby', 'Playing 1v1')."
        ),
    )
    session_id: Optional[str] = Field(
        default=None,
        description=(
            "Optional GameLift game-session ID for friends "
            "who can join."
        ),
    )


class PresenceBatchReadRequest(_BaseModel):
    """POST /v1/presence/batch body.

    Returns presence rows for all supplied player_ids that have
    a fresh row. Players with no row (offline) are omitted from
    the response, not returned as nulls.
    """

    player_ids: list[str] = Field(
        description=(
            "Up to 100 player IDs to look up. Larger requests "
            "are chunked server-side."
        ),
        max_length=1000,
    )


class PresenceInfo(_BaseModel):
    player_id: str
    game_id: str
    status: str
    rich_presence: str = ""
    session_id: str = ""
    updated_at: int


class PresenceHeartbeatResponse(_BaseModel):
    status: str = Field(default="success")
    presence: PresenceInfo


class PresenceBatchReadResponse(_BaseModel):
    status: str = Field(default="success")
    presence: list[PresenceInfo] = Field(
        description=(
            "One entry per online player; offline players are "
            "omitted."
        ),
    )
