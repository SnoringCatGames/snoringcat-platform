"""Resolve `game_id` from an API Gateway event.

The platform identifies which game is making each request via three
mechanisms, in order of precedence:

1. **URL path parameter.** Per-game routes follow the shape
   `/v1/games/{game_id}/...` and API Gateway puts `game_id` in
   `event["pathParameters"]`.
2. **JWT claim.** Authenticated requests carry a JWT signed by
   `auth_service`, which embeds the `game_id` the user signed in
   from. Reads via `event["requestContext"]["authorizer"]` (when
   custom authorizers attach), or by parsing the Authorization
   header directly when handlers do their own auth.
3. **`X-Game-ID` request header.** Used by unauthenticated endpoints
   that still need to know which game (e.g. fleet warmup, version
   check). Lower trust than the JWT claim — the client could lie —
   so this is only honored when there is no JWT to disagree with.

If all three are missing, the resolver falls back to the
`DEFAULT_GAME_ID` env var (set to "hopnbop" during the migration
window so legacy unauthenticated endpoints still work).

Usage:

    from utils.game_id_resolver import resolve_game_id

    def handler(event, context):
        game_id = resolve_game_id(event)
        config = game_config_service.get(game_id)
        ...

A future variant `require_game_id_match(event, jwt_game_id)` will
enforce that the path/header agrees with the JWT claim, rejecting
cross-game token reuse. Phase 1e wires that in.
"""

import os
from typing import Optional


# A request can never be served if no game_id can be resolved.
class GameIdMissingError(Exception):
    """Raised when no game_id can be resolved from an event."""


# A request whose path/header game_id disagrees with its JWT claim.
class GameIdMismatchError(Exception):
    """Raised when game_id sources conflict (e.g. JWT vs path)."""


_DEFAULT_GAME_ID = os.environ.get("DEFAULT_GAME_ID", "hopnbop")


def resolve_game_id(
    event: dict,
    jwt_claims: Optional[dict] = None,
    require: bool = False,
) -> str:
    """Resolve game_id from an API Gateway event.

    Args:
        event: API Gateway proxy event dict.
        jwt_claims: Optional decoded JWT payload. If supplied and
            it contains a `game_id`, this becomes the authoritative
            source. Conflicts with path/header raise
            GameIdMismatchError.
        require: If True, raise GameIdMissingError when no game_id
            can be resolved (instead of falling back to the
            DEFAULT_GAME_ID env var).

    Returns:
        The resolved game_id string.

    Raises:
        GameIdMissingError: when require=True and no source provides
            a game_id.
        GameIdMismatchError: when a JWT-claimed game_id disagrees
            with a path or header game_id.
    """
    path_game_id = _from_path(event)
    header_game_id = _from_header(event)
    jwt_game_id = (jwt_claims or {}).get("game_id")

    # JWT claim is authoritative when present. Reject any conflict.
    if jwt_game_id:
        for source_name, source_value in (
            ("path", path_game_id),
            ("X-Game-ID header", header_game_id),
        ):
            if source_value and source_value != jwt_game_id:
                raise GameIdMismatchError(
                    f"JWT game_id={jwt_game_id!r} disagrees with "
                    f"{source_name} game_id={source_value!r}"
                )
        return jwt_game_id

    # No JWT → trust the path, then header, then default.
    if path_game_id:
        return path_game_id
    if header_game_id:
        return header_game_id

    if require:
        raise GameIdMissingError(
            "no game_id in path, header, or JWT"
        )
    return _DEFAULT_GAME_ID


def _from_path(event: dict) -> Optional[str]:
    path_params = event.get("pathParameters") or {}
    return path_params.get("game_id")


def _from_header(event: dict) -> Optional[str]:
    headers = event.get("headers") or {}
    # API Gateway is case-insensitive on the wire but case-sensitive
    # in event["headers"]. Most clients send mixed case; check
    # the most common renderings.
    return (
        headers.get("X-Game-ID")
        or headers.get("x-game-id")
        or headers.get("X-Game-Id")
    )
