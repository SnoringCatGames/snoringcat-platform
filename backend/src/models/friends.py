"""Request/response models for /v1/friends/* endpoints.

Friends are a global cross-game graph. The `presence` field on
each friend, when populated, includes the game_id the friend is
currently in (so the friends UI can render a per-game badge).
"""

from __future__ import annotations

from typing import Optional

from pydantic import BaseModel, ConfigDict, Field


class _BaseModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


# --------- Sub-models -------------------------------------------


class FriendPresence(_BaseModel):
    """A friend's current presence (from snoringcat-presence).

    Absent / null means the friend is offline.
    """

    game_id: str = Field(
        description=(
            "The game the friend is currently in (e.g. "
            "'hopnbop'). Empty if the friend is online but "
            "not in any specific game."
        ),
    )
    status: str = Field(
        description=(
            "Free-form status string. Standard values: "
            "'online', 'in_match', 'away'."
        ),
    )
    rich_presence: str = Field(
        default="",
        description=(
            "Game-supplied free-text presence string "
            "(e.g. 'In lobby', 'Playing 1v1')."
        ),
    )
    updated_at: int = Field(
        description="Unix seconds the presence was last refreshed.",
    )


class FriendInfo(_BaseModel):
    """One row in the friends list."""

    player_id: str
    display_name: str
    friend_code: str = Field(default="")
    profile_image_url: Optional[str] = None
    presence: Optional[FriendPresence] = Field(
        default=None,
        description=(
            "Null when the friend is offline (no recent "
            "presence heartbeat). Populated otherwise."
        ),
    )


class FriendRequestInfo(_BaseModel):
    """A pending friend request (incoming or outgoing)."""

    player_id: str = Field(
        description=(
            "The other player's ID (sender for incoming, "
            "recipient for outgoing)."
        ),
    )
    display_name: str
    direction: str = Field(
        description="Either 'incoming' or 'outgoing'.",
    )
    created_at: int


# --------- Requests ---------------------------------------------


class AddFriendRequest(_BaseModel):
    """POST /v1/friends/add body."""

    friend_code: Optional[str] = Field(
        default=None,
        description=(
            "Add by 6-character friend_code. Either friend_code "
            "or player_id is required."
        ),
    )
    player_id: Optional[str] = Field(
        default=None,
        description=(
            "Add by player_id directly. Useful when the UI "
            "already has the ID (e.g. invite from a match)."
        ),
    )


class RemoveFriendRequest(_BaseModel):
    """POST /v1/friends/remove body."""

    friend_id: str


class AcceptFriendRequest(_BaseModel):
    """POST /v1/friends/accept body."""

    friend_id: str = Field(
        description="Player who sent the request to accept.",
    )


class RejectFriendRequest(_BaseModel):
    """POST /v1/friends/reject body."""

    friend_id: str


# --------- Responses --------------------------------------------


class FriendsListResponse(_BaseModel):
    """GET /v1/friends response."""

    status: str = Field(default="success")
    friends: list[FriendInfo]


class FriendNotificationsResponse(_BaseModel):
    """GET /v1/friends/notifications response."""

    status: str = Field(default="success")
    incoming_requests: list[FriendRequestInfo]
    outgoing_requests: list[FriendRequestInfo]


class SearchFriendCodeResponse(_BaseModel):
    """GET /v1/friends/search response."""

    status: str = Field(default="success")
    found: bool
    player: Optional[FriendInfo] = None
