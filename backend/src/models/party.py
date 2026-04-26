"""Request/response models for /v1/party/* endpoints.

Parties are per-game: each row in the parties table carries a
game_id attribute (Phase 1g). Cross-game invites are rejected
server-side (party_service.CrossGameInviteError).
"""

from __future__ import annotations

from typing import Optional

from pydantic import BaseModel, ConfigDict, Field


class _BaseModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


# --------- Sub-models -------------------------------------------


class PartyMember(_BaseModel):
    """One row in the party.members or party.invited list when
    rendered for the client. Display name is fetched from the
    accounts table at response-build time."""

    player_id: str
    display_name: str
    profile_image_url: Optional[str] = None


class PartyInfo(_BaseModel):
    """Common party state returned by create/invite/join/status/etc."""

    party_id: str
    leader_id: str
    game_id: str = Field(
        description=(
            "The game this party is bound to. Cross-game "
            "invites are rejected server-side."
        ),
    )
    members: list[PartyMember]
    invited: list[PartyMember]
    status: str = Field(
        description=(
            "'lobby', 'matchmaking', 'in_match', or 'ended'."
        ),
    )
    matchmaking_ticket_id: Optional[str] = None
    created_at: int
    expires_at: int


# --------- Requests ---------------------------------------------


class InviteToPartyRequest(_BaseModel):
    party_id: str
    invitee_id: str


class JoinPartyRequest(_BaseModel):
    party_id: str


class LeavePartyRequest(_BaseModel):
    party_id: str


class KickFromPartyRequest(_BaseModel):
    party_id: str
    target_player_id: str


class StartPartyMatchmakingRequest(_BaseModel):
    party_id: str
    # Per-game matchmaking parameters (level preference, etc.).
    # Free-form so each game can pass its own knobs.
    matchmaking_attributes: dict = Field(
        default_factory=dict,
    )


# --------- Responses --------------------------------------------


class PartyResponse(_BaseModel):
    """Returned by create/invite/join/leave/status."""

    status: str = Field(default="success")
    party: Optional[PartyInfo] = Field(
        default=None,
        description=(
            "Null when the player is not currently in any "
            "party (used by /party/status)."
        ),
    )


class StartMatchmakingResponse(_BaseModel):
    status: str = Field(default="success")
    party: PartyInfo
    matchmaking_ticket_id: str
