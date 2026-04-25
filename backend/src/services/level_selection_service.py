"""Level selection service for matchmaking."""

import random
from dataclasses import dataclass, field
from typing import List, Dict, Optional, Set


@dataclass
class LevelPreference:
    """Player's level preferences for matchmaking."""

    inclusion: List[str] = field(default_factory=list)
    exclusion: List[str] = field(default_factory=list)
    preferred: str = ""


@dataclass
class SessionPreference:
    """Player's session preferences for matchmaking.

    Bundles level preferences with gameplay
    toggles (critters, cheats).
    """

    level: LevelPreference = field(
        default_factory=LevelPreference
    )
    critters_enabled: bool = True
    cheats_enabled: bool = True


# Available levels registry.
# In production, this could be loaded from a configuration file or database.
AVAILABLE_LEVELS: Dict[str, Dict] = {
    "default_level": {
        "id": "default_level",
        "display_name": "Classic Arena",
        "min_players": 2,
        "max_players": 4,
        "enabled": True,
    },
    # Add more levels here as they are created.
}


def get_available_level_ids() -> List[str]:
    """Get list of all enabled level IDs."""
    return [
        level_id
        for level_id, info in AVAILABLE_LEVELS.items()
        if info.get("enabled", True)
    ]


def parse_level_preference(
    data: Optional[Dict],
) -> LevelPreference:
    """Parse level preference from request data."""
    if not data:
        return LevelPreference()

    return LevelPreference(
        inclusion=data.get("inclusion", []),
        exclusion=data.get("exclusion", []),
        preferred=data.get("preferred", ""),
    )


def parse_session_preference(
    data: Optional[Dict],
) -> SessionPreference:
    """Parse session preference from request data.

    The dict uses a flat format with level keys
    alongside critter/cheat keys.
    """
    if not data:
        return SessionPreference()

    return SessionPreference(
        level=parse_level_preference(data),
        critters_enabled=data.get(
            "critters_enabled", True
        ),
        cheats_enabled=data.get(
            "cheats_enabled", True
        ),
    )


def select_level_for_match(
    player_preferences: List[LevelPreference],
    available_levels: Optional[List[str]] = None,
) -> str:
    """
    Select a level based on combined player preferences.

    Algorithm:
    1. Start with all available levels
    2. Remove levels in ANY player's exclusion list (union of vetoes)
    3. If any player has inclusion lists, keep only intersection
    4. Count preferred votes from all players
    5. Pick level with most votes (ties broken randomly)
    6. If no preferences match, pick random from remaining
    7. If no valid level exists, return default

    Args:
        player_preferences: List of preferences from all players in match.
        available_levels: Optional list of level IDs. Defaults to all enabled.

    Returns:
        Selected level ID.
    """
    if available_levels is None:
        available_levels = get_available_level_ids()

    if not available_levels:
        return "default_level"

    # Start with all available levels.
    candidates: Set[str] = set(available_levels)

    # Apply exclusions (union - any player can veto).
    for prefs in player_preferences:
        for excluded in prefs.exclusion:
            candidates.discard(excluded)

    # Apply inclusions (intersection - all must agree).
    inclusion_sets = [
        set(p.inclusion) for p in player_preferences if p.inclusion
    ]
    if inclusion_sets:
        intersection = set.intersection(*inclusion_sets)
        # Only filter by intersection if it's non-empty.
        if intersection:
            candidates &= intersection

    if not candidates:
        # No compatible level - return default.
        return available_levels[0] if available_levels else "default_level"

    # Count preferred votes.
    preferred_votes: Dict[str, int] = {}
    for prefs in player_preferences:
        if prefs.preferred and prefs.preferred in candidates:
            preferred_votes[prefs.preferred] = (
                preferred_votes.get(prefs.preferred, 0) + 1
            )

    if preferred_votes:
        # Find max votes.
        max_votes = max(preferred_votes.values())
        top_choices = [
            level_id
            for level_id, votes in preferred_votes.items()
            if votes == max_votes
        ]
        # Random tiebreaker among top choices.
        return random.choice(top_choices)

    # No preferences - random from remaining candidates.
    return random.choice(list(candidates))


def validate_level_preference(prefs: LevelPreference) -> List[str]:
    """
    Validate level preference values.

    Returns list of error messages (empty if valid).
    """
    errors = []
    available = set(get_available_level_ids())

    # Check inclusion levels exist.
    for level_id in prefs.inclusion:
        if level_id not in available:
            errors.append(f"Unknown level in inclusion: {level_id}")

    # Check exclusion levels exist.
    for level_id in prefs.exclusion:
        if level_id not in available:
            errors.append(f"Unknown level in exclusion: {level_id}")

    # Check preferred level.
    if prefs.preferred and prefs.preferred not in available:
        errors.append(f"Unknown preferred level: {prefs.preferred}")

    # Check for contradictions.
    if prefs.preferred and prefs.preferred in prefs.exclusion:
        errors.append("Preferred level cannot be in exclusion list")

    return errors
