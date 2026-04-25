"""Per-game configuration service.

Reads the `games` DynamoDB table, which holds runtime config for
every game registered with the platform: display name, icon URL,
GameLift fleet ID, matchmaker name, ruleset, physics tick rate,
DNS zone, supported transports, latest protocol version, and
arbitrary feature flags.

Caches config in module-level memory for the lifetime of the warm
Lambda container, with a configurable TTL so config changes
propagate without redeploy. Default TTL is 5 minutes.
"""

import os
import time
from dataclasses import dataclass, field
from typing import Optional

import boto3


# Module-level cache for Lambda container reuse.
# Key: game_id; value: (cached_at_unix, GameConfig instance).
_cache: dict[str, tuple[float, "GameConfig"]] = {}
_client = None
_table = None

# How long a cached games-table row remains valid in warm container
# memory. Override with GAMES_CONFIG_TTL_SEC env var. The default of
# 300s means changes take at most 5 minutes to propagate.
_TTL_SEC = float(os.environ.get("GAMES_CONFIG_TTL_SEC", "300"))


@dataclass
class GameConfig:
    """A row from the `games` config table.

    Optional fields default to a no-op equivalent so handlers can
    code against the dataclass without per-field None-checks.
    """

    game_id: str
    display_name: str = ""
    icon_url: str = ""

    # GameLift integration. Empty fleet_id disables fleet operations
    # (warmup, scaling, idle check) — useful for offline-only games.
    fleet_id: str = ""
    gamelift_location: str = ""
    matchmaker_name: str = ""
    matchmaker_ruleset: str = ""

    # Game-server protocol.
    physics_fps: int = 60
    protocol_version: int = 1
    latest_game_version: str = ""
    supported_transports: list[str] = field(
        default_factory=lambda: ["enet"]
    )

    # DNS zone for game-session hostnames (e.g. game.snoringcat.games).
    base_dns_zone: str = ""

    # Arbitrary game-specific feature flags (e.g. {"is_gore_enabled":
    # true}). Read by per-game handlers as needed.
    feature_flags: dict = field(default_factory=dict)

    # Audit timestamps (Unix seconds).
    created_at: int = 0
    updated_at: int = 0

    @classmethod
    def from_item(cls, item: dict) -> "GameConfig":
        """Build a GameConfig from a DynamoDB item dict."""
        return cls(
            game_id=item["game_id"],
            display_name=item.get("display_name", ""),
            icon_url=item.get("icon_url", ""),
            fleet_id=item.get("fleet_id", ""),
            gamelift_location=item.get("gamelift_location", ""),
            matchmaker_name=item.get("matchmaker_name", ""),
            matchmaker_ruleset=item.get("matchmaker_ruleset", ""),
            physics_fps=int(item.get("physics_fps", 60)),
            protocol_version=int(
                item.get("protocol_version", 1)
            ),
            latest_game_version=item.get(
                "latest_game_version", ""
            ),
            supported_transports=list(
                item.get("supported_transports", ["enet"])
            ),
            base_dns_zone=item.get("base_dns_zone", ""),
            feature_flags=dict(item.get("feature_flags", {})),
            created_at=int(item.get("created_at", 0)),
            updated_at=int(item.get("updated_at", 0)),
        )


class GameNotRegisteredError(Exception):
    """Raised when a game_id has no row in the games table."""


def _get_table():
    global _client, _table
    if _table is None:
        _client = boto3.resource("dynamodb")
        _table = _client.Table(
            os.environ.get("GAMES_TABLE", "snoringcat-games")
        )
    return _table


def get(game_id: str) -> GameConfig:
    """Fetch a game's config, using the warm-container cache.

    Raises GameNotRegisteredError if the game_id is not in the
    games table.
    """
    now = time.time()
    cached = _cache.get(game_id)
    if cached and now - cached[0] < _TTL_SEC:
        return cached[1]

    response = _get_table().get_item(Key={"game_id": game_id})
    item = response.get("Item")
    if item is None:
        raise GameNotRegisteredError(
            f"game_id={game_id!r} not registered in games table"
        )

    config = GameConfig.from_item(item)
    _cache[game_id] = (now, config)
    return config


def try_get(game_id: str) -> Optional[GameConfig]:
    """Same as get() but returns None instead of raising."""
    try:
        return get(game_id)
    except GameNotRegisteredError:
        return None


def invalidate(game_id: Optional[str] = None) -> None:
    """Clear the in-memory cache for a game_id, or all games.

    Used by tests and by any future admin endpoint that updates
    game config (so the next request picks up the new row
    without waiting for the TTL).
    """
    if game_id is None:
        _cache.clear()
    else:
        _cache.pop(game_id, None)


def upsert(config: GameConfig) -> None:
    """Write a games row.

    Used by the registration script (one-time per game) and by
    test fixtures. Production handlers do not write to this table.
    """
    if not config.created_at:
        config.created_at = int(time.time())
    config.updated_at = int(time.time())
    _get_table().put_item(
        Item={
            "game_id": config.game_id,
            "display_name": config.display_name,
            "icon_url": config.icon_url,
            "fleet_id": config.fleet_id,
            "gamelift_location": config.gamelift_location,
            "matchmaker_name": config.matchmaker_name,
            "matchmaker_ruleset": config.matchmaker_ruleset,
            "physics_fps": config.physics_fps,
            "protocol_version": config.protocol_version,
            "latest_game_version": config.latest_game_version,
            "supported_transports": config.supported_transports,
            "base_dns_zone": config.base_dns_zone,
            "feature_flags": config.feature_flags,
            "created_at": config.created_at,
            "updated_at": config.updated_at,
        }
    )
    invalidate(config.game_id)


def list_all() -> list[GameConfig]:
    """Read all rows from the games table.

    Used by the friends presence display (to render friend's
    current game name + icon) and by the scheduled fleet
    idle-check Lambda (to iterate every game's fleet).

    Caches each row by game_id as a side effect.
    """
    response = _get_table().scan()
    items = response.get("Items", [])
    now = time.time()
    configs = []
    for item in items:
        config = GameConfig.from_item(item)
        _cache[config.game_id] = (now, config)
        configs.append(config)
    return configs
