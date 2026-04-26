"""Request/response models for /v1/matchmaking/* endpoints."""

from __future__ import annotations

from typing import Optional

from pydantic import BaseModel, ConfigDict, Field


class _BaseModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


# --------- Sub-models -------------------------------------------


class GameSessionInfo(_BaseModel):
    """Connection details for a placed game session."""

    server_ip: str = Field(
        description="Public IPv4 of the GameLift container.",
    )
    server_port: int = Field(
        description=(
            "ENet UDP port. WebSocket / WebRTC signaling lives "
            "at server_port + 1 (TCP, behind nginx)."
        ),
    )
    server_hostname: str = Field(
        description=(
            "DNS hostname for the session "
            "(e.g. s-{ip}.game.snoringcat.games). Web clients "
            "must use the hostname for valid TLS."
        ),
    )
    player_session_ids: list[str] = Field(
        description=(
            "GameLift player session IDs for the matched "
            "players. Pass these to the server on connect "
            "for validation."
        ),
    )
    game_session_id: str
    transport_type: str = Field(
        description="One of 'enet', 'webrtc', 'websocket'.",
    )
    matchmaking_ticket_id: str


# --------- Requests ---------------------------------------------


class StartMatchmakingRequest(_BaseModel):
    """POST /v1/matchmaking/start body."""

    matchmaking_attributes: dict = Field(
        default_factory=dict,
        description=(
            "Free-form attributes passed to FlexMatch (level "
            "preferences, latency hints, etc.)."
        ),
    )


class LeaveMatchmakingRequest(_BaseModel):
    """POST /v1/matchmaking/leave body."""

    ticket_id: str


# --------- Responses --------------------------------------------


class StartMatchmakingResponse(_BaseModel):
    """POST /v1/matchmaking/start response."""

    status: str = Field(default="success")
    ticket_id: str = Field(
        description="Poll /v1/matchmaking/status/{ticket_id}.",
    )


class MatchmakingStatusResponse(_BaseModel):
    """GET /v1/matchmaking/status/{ticket_id} response.

    `match_state` cycles through: queued → searching →
    placing → completed (or cancelled / timed_out / failed).
    `session` is null until match_state == "completed".
    """

    status: str = Field(default="success")
    ticket_id: str
    match_state: str
    session: Optional[GameSessionInfo] = None
