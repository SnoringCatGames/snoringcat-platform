"""Tests for match_service.py - match recording and queries."""

import asyncio
import pytest

from tests.constants import TEST_JWT_SECRET


class TestCalculateRatingDelta:
    """Unit tests for Elo rating calculation."""

    def test_equal_rating_win(self):
        from services.match_service import MatchService

        # Equal ratings, winner gets +16.
        delta = MatchService._calculate_rating_delta(
            1500, 1500, True
        )
        assert delta == 16

    def test_equal_rating_loss(self):
        from services.match_service import MatchService

        delta = MatchService._calculate_rating_delta(
            1500, 1500, False
        )
        assert delta == -16

    def test_higher_rated_wins_less(self):
        from services.match_service import MatchService

        # Higher-rated player wins, gains fewer points.
        delta = MatchService._calculate_rating_delta(
            1800, 1500, True
        )
        assert delta < 16
        assert delta > 0

    def test_lower_rated_wins_more(self):
        from services.match_service import MatchService

        # Lower-rated player wins, gains more points.
        delta = MatchService._calculate_rating_delta(
            1200, 1500, True
        )
        assert delta > 16

    def test_symmetry(self):
        from services.match_service import MatchService

        # Win delta + loss delta should sum to zero
        # for equal matchups.
        win_delta = MatchService._calculate_rating_delta(
            1500, 1500, True
        )
        loss_delta = MatchService._calculate_rating_delta(
            1500, 1500, False
        )
        assert win_delta + loss_delta == 0


class TestRecordMatchResult:
    """Integration tests for recording match results."""

    def test_writes_match_history(self, aws_mock):
        from services.match_service import MatchService
        from services.player_service import PlayerService

        ps = PlayerService()
        ms = MatchService()

        # Create two players.
        asyncio.run(
            ps.create_player(
                "p_aaa", "Alice", {"steam": "s1"}
            )
        )
        asyncio.run(
            ps.create_player(
                "p_bbb", "Bob", {"steam": "s2"}
            )
        )

        ms.record_match_result(
            game_session_id="gsess-001",
            match_duration_sec=90.5,
            level_id="level_0",
            player_results=[
                {
                    "player_id": "p_aaa",
                    "rank": 1,
                    "score": 100,
                    "kill_count": 3,
                    "death_count": 1,
                },
                {
                    "player_id": "p_bbb",
                    "rank": 2,
                    "score": 50,
                    "kill_count": 1,
                    "death_count": 3,
                },
            ],
        )

        # Verify match history was written.
        alice_matches = ms.get_recent_matches("p_aaa")
        assert len(alice_matches) == 1
        assert alice_matches[0]["game_session_id"] == "gsess-001"
        assert alice_matches[0]["rank"] == 1
        assert alice_matches[0]["is_win"] is True

        bob_matches = ms.get_recent_matches("p_bbb")
        assert len(bob_matches) == 1
        assert bob_matches[0]["rank"] == 2
        assert bob_matches[0]["is_win"] is False

    def test_updates_player_stats(self, aws_mock):
        from services.match_service import MatchService
        from services.player_service import PlayerService

        ps = PlayerService()
        ms = MatchService()

        asyncio.run(
            ps.create_player(
                "p_aaa", "Alice", {"steam": "s1"}
            )
        )
        asyncio.run(
            ps.create_player(
                "p_bbb", "Bob", {"steam": "s2"}
            )
        )

        ms.record_match_result(
            game_session_id="gsess-002",
            match_duration_sec=60.0,
            level_id="level_1",
            player_results=[
                {"player_id": "p_aaa", "rank": 1},
                {"player_id": "p_bbb", "rank": 2},
            ],
        )

        alice = asyncio.run(ps.get_player("p_aaa"))
        assert alice.matches_played == 1
        assert alice.wins == 1
        assert alice.losses == 0
        assert alice.rating > 1500  # Winner gains rating.

        bob = asyncio.run(ps.get_player("p_bbb"))
        assert bob.matches_played == 1
        assert bob.wins == 0
        assert bob.losses == 1
        assert bob.rating < 1500  # Loser loses rating.

    def test_rating_floor(self, aws_mock):
        """Rating should not drop below 100."""
        from services.match_service import MatchService
        from services.player_service import PlayerService

        ps = PlayerService()
        ms = MatchService()

        asyncio.run(
            ps.create_player(
                "p_low", "LowPlayer", {"steam": "s1"}
            )
        )
        # Manually set very low rating.
        ps.table.update_item(
            Key={"player_id": "p_low"},
            UpdateExpression="SET rating = :r",
            ExpressionAttributeValues={":r": 105},
        )

        asyncio.run(
            ps.create_player(
                "p_high", "HighPlayer", {"steam": "s2"}
            )
        )
        ps.table.update_item(
            Key={"player_id": "p_high"},
            UpdateExpression="SET rating = :r",
            ExpressionAttributeValues={":r": 2000},
        )

        ms.record_match_result(
            game_session_id="gsess-003",
            match_duration_sec=30.0,
            level_id="level_0",
            player_results=[
                {"player_id": "p_high", "rank": 1},
                {"player_id": "p_low", "rank": 2},
            ],
        )

        low = asyncio.run(ps.get_player("p_low"))
        assert low.rating >= 100


class TestLeaderboard:
    def test_returns_sorted_by_rating(self, aws_mock):
        from services.match_service import MatchService
        from services.player_service import PlayerService

        ps = PlayerService()
        ms = MatchService()

        # Create players with different ratings.
        for name, rating in [
            ("Alice", 1800),
            ("Bob", 1600),
            ("Carol", 1900),
        ]:
            pid = f"p_{name.lower()}"
            asyncio.run(
                ps.create_player(pid, name, {"steam": name})
            )
            ps.table.update_item(
                Key={"player_id": pid},
                UpdateExpression=(
                    "SET rating = :r,"
                    " rating_partition = :all"
                ),
                ExpressionAttributeValues={
                    ":r": rating,
                    ":all": "all",
                },
            )

        board = ms.get_leaderboard(limit=10)
        assert len(board) == 3
        assert board[0]["display_name"] == "Carol"
        assert board[0]["rank"] == 1
        assert board[1]["display_name"] == "Alice"
        assert board[2]["display_name"] == "Bob"

    def test_respects_limit(self, aws_mock):
        from services.match_service import MatchService
        from services.player_service import PlayerService

        ps = PlayerService()
        ms = MatchService()

        for i in range(5):
            pid = f"p_{i}"
            asyncio.run(
                ps.create_player(
                    pid, f"Player{i}", {"steam": f"s{i}"}
                )
            )
            ps.table.update_item(
                Key={"player_id": pid},
                UpdateExpression=(
                    "SET rating = :r,"
                    " rating_partition = :all"
                ),
                ExpressionAttributeValues={
                    ":r": 1500 + i * 100,
                    ":all": "all",
                },
            )

        board = ms.get_leaderboard(limit=3)
        assert len(board) == 3

    def test_get_player_rank(self, aws_mock):
        from services.match_service import MatchService
        from services.player_service import PlayerService

        ps = PlayerService()
        ms = MatchService()

        for name, rating in [
            ("Alice", 1800),
            ("Bob", 1600),
            ("Carol", 1900),
        ]:
            pid = f"p_{name.lower()}"
            asyncio.run(
                ps.create_player(pid, name, {"steam": name})
            )
            ps.table.update_item(
                Key={"player_id": pid},
                UpdateExpression=(
                    "SET rating = :r,"
                    " rating_partition = :all"
                ),
                ExpressionAttributeValues={
                    ":r": rating,
                    ":all": "all",
                },
            )

        # Carol (1900) should be rank 1.
        assert ms.get_player_rank("p_carol", 1900) == 1
        # Alice (1800) should be rank 2.
        assert ms.get_player_rank("p_alice", 1800) == 2
        # Bob (1600) should be rank 3.
        assert ms.get_player_rank("p_bob", 1600) == 3
