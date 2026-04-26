"""Tests for services.profile_service."""

import asyncio

import pytest

from services.profile_service import ProfileService


def _run(coro):
    return asyncio.run(coro)


class TestCreate:
    def test_creates_with_defaults(self, aws_mock):
        service = ProfileService()
        profile = _run(
            service.create(
                player_id="p_1", game_id="hopnbop"
            )
        )
        assert profile.player_id == "p_1"
        assert profile.game_id == "hopnbop"
        assert profile.rating == 1500
        assert profile.matches_played == 0
        assert profile.wins == 0
        assert profile.losses == 0
        assert profile.first_played > 0
        assert profile.last_played == profile.first_played
        assert profile.game_stats == {}

    def test_creates_with_display_name_override(
        self, aws_mock
    ):
        service = ProfileService()
        profile = _run(
            service.create(
                player_id="p_1",
                game_id="hopnbop",
                display_name="HoppyName",
            )
        )
        assert profile.display_name == "HoppyName"

    def test_creates_with_initial_stats(self, aws_mock):
        service = ProfileService()
        profile = _run(
            service.create(
                player_id="p_1",
                game_id="hopnbop",
                game_stats={"total_kills": 5},
            )
        )
        assert profile.game_stats == {"total_kills": 5}

    def test_two_games_for_same_player_independent(
        self, aws_mock
    ):
        service = ProfileService()
        _run(
            service.create(
                player_id="p_1", game_id="hopnbop"
            )
        )
        _run(
            service.create(
                player_id="p_1", game_id="nextgame"
            )
        )
        hop = _run(service.get("p_1", "hopnbop"))
        nxt = _run(service.get("p_1", "nextgame"))
        assert hop is not None
        assert nxt is not None
        assert hop.game_id == "hopnbop"
        assert nxt.game_id == "nextgame"


class TestGet:
    def test_returns_none_when_missing(self, aws_mock):
        service = ProfileService()
        result = _run(service.get("p_x", "hopnbop"))
        assert result is None

    def test_get_or_create_creates_when_missing(
        self, aws_mock
    ):
        service = ProfileService()
        result = _run(
            service.get_or_create("p_new", "hopnbop")
        )
        assert result.matches_played == 0
        # Second call returns the existing row.
        result2 = _run(
            service.get_or_create("p_new", "hopnbop")
        )
        assert result2.created_at == result.created_at

    def test_list_for_player(self, aws_mock):
        service = ProfileService()
        _run(
            service.create("p_multi", "hopnbop")
        )
        _run(
            service.create("p_multi", "nextgame")
        )
        _run(
            service.create("p_multi", "thirdgame")
        )
        all_profiles = _run(
            service.list_for_player("p_multi")
        )
        assert len(all_profiles) == 3
        game_ids = {p.game_id for p in all_profiles}
        assert game_ids == {
            "hopnbop",
            "nextgame",
            "thirdgame",
        }


class TestDisplayName:
    def test_set_and_clear_override(self, aws_mock):
        service = ProfileService()
        _run(service.create("p_1", "hopnbop"))
        _run(
            service.update_display_name(
                "p_1", "hopnbop", "Override"
            )
        )
        result = _run(service.get("p_1", "hopnbop"))
        assert result.display_name == "Override"

        _run(
            service.update_display_name(
                "p_1", "hopnbop", ""
            )
        )
        result = _run(service.get("p_1", "hopnbop"))
        assert result.display_name == ""


class TestMatchUpdate:
    def test_win_increments_correctly(self, aws_mock):
        service = ProfileService()
        _run(service.create("p_1", "hopnbop"))
        _run(
            service.update_after_match(
                "p_1", "hopnbop",
                rating_delta=25, won=True,
            )
        )
        result = _run(service.get("p_1", "hopnbop"))
        assert result.matches_played == 1
        assert result.wins == 1
        assert result.losses == 0
        assert result.rating == 1525

    def test_loss_decrements_correctly(self, aws_mock):
        service = ProfileService()
        _run(service.create("p_1", "hopnbop"))
        _run(
            service.update_after_match(
                "p_1", "hopnbop",
                rating_delta=-10, won=False,
            )
        )
        result = _run(service.get("p_1", "hopnbop"))
        assert result.matches_played == 1
        assert result.wins == 0
        assert result.losses == 1
        assert result.rating == 1490

    def test_per_game_isolation(self, aws_mock):
        """Match in hopnbop must not affect nextgame profile."""
        service = ProfileService()
        _run(service.create("p_1", "hopnbop"))
        _run(service.create("p_1", "nextgame"))
        _run(
            service.update_after_match(
                "p_1", "hopnbop",
                rating_delta=50, won=True,
            )
        )
        nxt = _run(service.get("p_1", "nextgame"))
        assert nxt.rating == 1500
        assert nxt.matches_played == 0


class TestGameStats:
    def test_merge_adds_numeric_keys(self, aws_mock):
        service = ProfileService()
        _run(
            service.create(
                "p_1", "hopnbop",
                game_stats={"kills": 5, "deaths": 2},
            )
        )
        _run(
            service.merge_game_stats(
                "p_1", "hopnbop",
                {"kills": 3, "bumps": 7},
            )
        )
        result = _run(service.get("p_1", "hopnbop"))
        assert result.game_stats["kills"] == 8
        assert result.game_stats["deaths"] == 2
        assert result.game_stats["bumps"] == 7

    def test_merge_non_numeric_overwrites(self, aws_mock):
        service = ProfileService()
        _run(
            service.create(
                "p_1", "hopnbop",
                game_stats={"favorite_color": "red"},
            )
        )
        _run(
            service.merge_game_stats(
                "p_1", "hopnbop",
                {"favorite_color": "blue"},
            )
        )
        result = _run(service.get("p_1", "hopnbop"))
        assert result.game_stats["favorite_color"] == "blue"

    def test_merge_empty_is_noop(self, aws_mock):
        service = ProfileService()
        _run(
            service.create(
                "p_1", "hopnbop",
                game_stats={"x": 1},
            )
        )
        _run(
            service.merge_game_stats(
                "p_1", "hopnbop", {}
            )
        )
        result = _run(service.get("p_1", "hopnbop"))
        assert result.game_stats == {"x": 1}


class TestDeletion:
    def test_delete_all_for_player(self, aws_mock):
        service = ProfileService()
        _run(service.create("p_d", "hopnbop"))
        _run(service.create("p_d", "nextgame"))
        _run(service.create("p_d", "thirdgame"))
        deleted = _run(
            service.delete_all_for_player("p_d")
        )
        assert deleted == 3
        remaining = _run(service.list_for_player("p_d"))
        assert remaining == []

    def test_delete_other_player_unaffected(self, aws_mock):
        service = ProfileService()
        _run(service.create("p_a", "hopnbop"))
        _run(service.create("p_b", "hopnbop"))
        _run(service.delete_all_for_player("p_a"))
        b = _run(service.get("p_b", "hopnbop"))
        assert b is not None
