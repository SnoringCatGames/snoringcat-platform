"""Tests for level_selection_service.py - pure logic, no AWS."""

import random

import pytest

from services.level_selection_service import (
    LevelPreference,
    SessionPreference,
    parse_level_preference,
    parse_session_preference,
    select_level_for_match,
    validate_level_preference,
    get_available_level_ids,
)


LEVELS = ["level_a", "level_b", "level_c"]


class TestParseLevelPreference:
    def test_none_returns_empty(self):
        pref = parse_level_preference(None)
        assert pref.inclusion == []
        assert pref.exclusion == []
        assert pref.preferred == ""

    def test_parses_all_fields(self):
        data = {
            "inclusion": ["level_a"],
            "exclusion": ["level_b"],
            "preferred": "level_a",
        }
        pref = parse_level_preference(data)
        assert pref.inclusion == ["level_a"]
        assert pref.exclusion == ["level_b"]
        assert pref.preferred == "level_a"


class TestParseSessionPreference:
    def test_none_returns_defaults(self):
        pref = parse_session_preference(None)
        assert pref.critters_enabled is True
        assert pref.cheats_enabled is True
        assert pref.level.preferred == ""

    def test_parses_all_fields(self):
        data = {
            "preferred": "level_a",
            "critters_enabled": False,
            "cheats_enabled": False,
        }
        pref = parse_session_preference(data)
        assert pref.critters_enabled is False
        assert pref.cheats_enabled is False
        assert pref.level.preferred == "level_a"


class TestSelectLevelForMatch:
    def test_no_preferences_returns_available(self):
        result = select_level_for_match(
            [], available_levels=LEVELS
        )
        assert result in LEVELS

    def test_single_preferred(self):
        prefs = [LevelPreference(preferred="level_b")]
        result = select_level_for_match(
            prefs, available_levels=LEVELS
        )
        assert result == "level_b"

    def test_vetoed_level_excluded(self):
        prefs = [LevelPreference(exclusion=["level_a"])]
        random.seed(42)
        results = set()
        for _ in range(20):
            results.add(
                select_level_for_match(
                    prefs, available_levels=LEVELS
                )
            )
        assert "level_a" not in results

    def test_votes_decide_winner(self):
        prefs = [
            LevelPreference(preferred="level_a"),
            LevelPreference(preferred="level_b"),
            LevelPreference(preferred="level_b"),
        ]
        result = select_level_for_match(
            prefs, available_levels=LEVELS
        )
        assert result == "level_b"

    def test_all_vetoed_returns_fallback(self):
        prefs = [
            LevelPreference(
                exclusion=["level_a", "level_b", "level_c"]
            )
        ]
        result = select_level_for_match(
            prefs, available_levels=LEVELS
        )
        # Falls back to first available level.
        assert result == "level_a"

    def test_empty_available_returns_default(self):
        result = select_level_for_match(
            [], available_levels=[]
        )
        assert result == "default_level"

    def test_inclusion_filters(self):
        prefs = [
            LevelPreference(inclusion=["level_b", "level_c"])
        ]
        random.seed(42)
        results = set()
        for _ in range(20):
            results.add(
                select_level_for_match(
                    prefs, available_levels=LEVELS
                )
            )
        assert results <= {"level_b", "level_c"}


class TestValidateLevelPreference:
    def test_valid_preference(self):
        available = get_available_level_ids()
        pref = LevelPreference(preferred=available[0])
        errors = validate_level_preference(pref)
        assert errors == []

    def test_unknown_level_in_inclusion(self):
        pref = LevelPreference(
            inclusion=["nonexistent_level"]
        )
        errors = validate_level_preference(pref)
        assert any("Unknown level" in e for e in errors)

    def test_preferred_in_exclusion_contradiction(self):
        available = get_available_level_ids()
        pref = LevelPreference(
            preferred=available[0],
            exclusion=[available[0]],
        )
        errors = validate_level_preference(pref)
        assert any("cannot be in exclusion" in e for e in errors)
