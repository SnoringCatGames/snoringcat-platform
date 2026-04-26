"""Request/response models for /v1/fleet/* endpoints.

Both endpoints are unauthenticated (the client-side warmup
flow runs before sign-in completes). They look up the fleet
ID for the requesting game from the games config table.
"""

from __future__ import annotations

from typing import Optional

from pydantic import BaseModel, ConfigDict, Field


class _BaseModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class FleetWarmupRequest(_BaseModel):
    """POST /v1/fleet/warmup body."""

    source: str = Field(
        default="unknown",
        description=(
            "Where the warmup was triggered from "
            "('app_startup', 'lobby_toggle', 'manual'). "
            "Used for telemetry / debugging."
        ),
    )


class FleetStatusResponse(_BaseModel):
    """GET /v1/fleet/status response (also returned by warmup).

    Status values:
      - 'cold'      — fleet is at DESIRED=0; warmup may scale it up.
      - 'warming'   — fleet is scaling up; not yet ready.
      - 'ready'     — fleet has at least one ACTIVE instance with
                       an IDLE game session slot.
      - 'unavailable' — no fleet configured for this game (the
                       games config row has fleet_id="").
    """

    status: str = Field(default="success")
    fleet_status: str
    estimated_remaining_sec: Optional[int] = Field(
        default=None,
        description=(
            "Remaining seconds before fleet is expected ready. "
            "Null when status is 'ready' or 'unavailable'."
        ),
    )
