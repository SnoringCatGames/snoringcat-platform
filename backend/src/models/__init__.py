"""Typed Pydantic request/response models for HTTP endpoints.

These models serve two purposes:

1. **OpenAPI generation**: scripts/generate_openapi.py walks every
   model registered in ENDPOINTS below and emits openapi.json.
   The CI workflow then runs oasdiff between the committed
   baseline and the PR branch to catch breaking changes.

2. **Runtime contract enforcement (future)**: handlers can opt in
   to Pydantic validation at request parse time and serialization
   at response time. Today most handlers do their own json.loads /
   json.dumps; migrating them is out of scope for this commit.

Coverage today:
- /v1/auth/*          (8 endpoints)
- /v1/friends/*       (6 of the 9 endpoints — the most-used ones)
- /v1/presence/*      (2 endpoints; the new game-aware shape)
- /v1/version         (1 endpoint)

Remaining endpoints (party, leaderboard, match results, fleet,
session, telemetry, account export, settings) get models in
follow-up commits. They are absent from openapi.json until then,
so oasdiff doesn't gate changes to them yet.

To add a new endpoint:
1. Define request and response models in the appropriate sub-module.
2. Add an entry to ENDPOINTS below with the path, method, and
   model classes.
3. Re-run scripts/generate_openapi.py to update openapi.json.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional

from pydantic import BaseModel

from . import auth, common, friends, presence, version


@dataclass(frozen=True)
class EndpointSpec:
    """One row in the ENDPOINTS table.

    The OpenAPI generator walks this table and emits a path entry
    per row. None for request_model means the endpoint takes no
    body (typical for GET / DELETE).
    """

    path: str
    method: str
    summary: str
    request_model: Optional[type[BaseModel]] = None
    response_model: Optional[type[BaseModel]] = None
    auth_required: bool = True
    tags: tuple[str, ...] = ()


ENDPOINTS: list[EndpointSpec] = [
    # --- Auth (no auth required for sign-in endpoints) -------
    EndpointSpec(
        path="/v1/auth/login",
        method="post",
        summary="Sign in via OAuth provider",
        request_model=auth.OAuthLoginRequest,
        response_model=auth.AuthSuccessResponse,
        auth_required=False,
        tags=("auth",),
    ),
    EndpointSpec(
        path="/v1/auth/anon",
        method="post",
        summary="Anonymous sign-in via device_id",
        request_model=auth.AnonLoginRequest,
        response_model=auth.AuthSuccessResponse,
        auth_required=False,
        tags=("auth",),
    ),
    EndpointSpec(
        path="/v1/auth/guest",
        method="post",
        summary="Issue ephemeral guest JWT",
        request_model=auth.GuestLoginRequest,
        response_model=auth.AuthSuccessResponse,
        auth_required=False,
        tags=("auth",),
    ),
    EndpointSpec(
        path="/v1/auth/refresh",
        method="post",
        summary="Rotate JWT using refresh token",
        request_model=auth.RefreshRequest,
        response_model=auth.AuthSuccessResponse,
        auth_required=False,
        tags=("auth",),
    ),
    EndpointSpec(
        path="/v1/auth/link",
        method="post",
        summary="Link OAuth identity to current account",
        request_model=auth.LinkAccountRequest,
        response_model=common.SuccessResponse,
        auth_required=True,
        tags=("auth",),
    ),
    EndpointSpec(
        path="/v1/auth/unlink",
        method="post",
        summary="Unlink an OAuth identity",
        request_model=auth.UnlinkAccountRequest,
        response_model=common.SuccessResponse,
        auth_required=True,
        tags=("auth",),
    ),
    EndpointSpec(
        path="/v1/auth/merge",
        method="post",
        summary="Merge two accounts (sign in with provider that already has one)",
        request_model=auth.MergeRequest,
        response_model=auth.AuthSuccessResponse,
        auth_required=True,
        tags=("auth",),
    ),
    EndpointSpec(
        path="/v1/auth/account",
        method="delete",
        summary="Delete current account (GDPR)",
        request_model=None,
        response_model=common.SuccessResponse,
        auth_required=True,
        tags=("auth",),
    ),
    # --- Friends (auth required) -----------------------------
    EndpointSpec(
        path="/v1/friends",
        method="get",
        summary="List friends with presence",
        response_model=friends.FriendsListResponse,
        tags=("friends",),
    ),
    EndpointSpec(
        path="/v1/friends/add",
        method="post",
        summary="Send a friend request",
        request_model=friends.AddFriendRequest,
        response_model=common.SuccessResponse,
        tags=("friends",),
    ),
    EndpointSpec(
        path="/v1/friends/remove",
        method="post",
        summary="Remove an accepted friend",
        request_model=friends.RemoveFriendRequest,
        response_model=common.SuccessResponse,
        tags=("friends",),
    ),
    EndpointSpec(
        path="/v1/friends/accept",
        method="post",
        summary="Accept incoming friend request",
        request_model=friends.AcceptFriendRequest,
        response_model=common.SuccessResponse,
        tags=("friends",),
    ),
    EndpointSpec(
        path="/v1/friends/reject",
        method="post",
        summary="Reject incoming friend request",
        request_model=friends.RejectFriendRequest,
        response_model=common.SuccessResponse,
        tags=("friends",),
    ),
    EndpointSpec(
        path="/v1/friends/notifications",
        method="get",
        summary="List incoming + outgoing friend requests",
        response_model=friends.FriendNotificationsResponse,
        tags=("friends",),
    ),
    # --- Presence (auth required) ----------------------------
    EndpointSpec(
        path="/v1/presence",
        method="put",
        summary="Heartbeat presence (game_id, status, rich text)",
        request_model=presence.PresenceHeartbeatRequest,
        response_model=presence.PresenceHeartbeatResponse,
        tags=("presence",),
    ),
    EndpointSpec(
        path="/v1/presence/batch",
        method="post",
        summary="Read presence for many players at once",
        request_model=presence.PresenceBatchReadRequest,
        response_model=presence.PresenceBatchReadResponse,
        tags=("presence",),
    ),
    # --- Misc -----------------------------------------------
    EndpointSpec(
        path="/v1/version",
        method="get",
        summary="Server + per-game version metadata",
        response_model=version.VersionResponse,
        auth_required=False,
        tags=("misc",),
    ),
]


__all__ = [
    "ENDPOINTS",
    "EndpointSpec",
    "auth",
    "common",
    "friends",
    "presence",
    "version",
]
