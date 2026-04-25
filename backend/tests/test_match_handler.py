"""Tests for match_handler.py - Lambda endpoint handlers."""

import json
import asyncio
from datetime import datetime, timedelta

import pytest

from services.auth_service import AuthToken
from tests.constants import (
    TEST_JWT_SECRET,
    TEST_SERVER_API_KEY,
)


class _FakeLambdaContext:
    """Minimal Lambda context for aws_lambda_powertools."""

    function_name = "test-function"
    memory_limit_in_mb = 512
    invoked_function_arn = (
        "arn:aws:lambda:us-east-1:123456789:function:test"
    )
    aws_request_id = "test-request-id"


_CONTEXT = _FakeLambdaContext()


def _make_event(
    body=None,
    headers=None,
    query_params=None,
    path_params=None,
):
    """Build a minimal Lambda API Gateway event."""
    event = {
        "body": json.dumps(body) if body else "{}",
        "headers": headers or {},
    }
    if query_params is not None:
        event["queryStringParameters"] = query_params
    if path_params is not None:
        event["pathParameters"] = path_params
    return event


def _parse_response(response):
    """Parse status code and body from Lambda response."""
    return (
        response["statusCode"],
        json.loads(response["body"]),
    )


def _make_jwt(player_id="p_testplayer"):
    """Create a valid JWT for testing."""
    now = datetime.now()
    token = AuthToken(
        player_id=player_id,
        display_name="TestPlayer",
        provider="steam",
        is_anonymous=False,
        issued_at=now,
        expires_at=now + timedelta(hours=24),
    )
    return token.to_jwt(TEST_JWT_SECRET)


def _server_headers():
    """Build headers with server API key."""
    return {"X-Server-Key": TEST_SERVER_API_KEY}


def _auth_headers(player_id="p_testplayer"):
    """Build headers with valid JWT."""
    return {"Authorization": f"Bearer {_make_jwt(player_id)}"}


def _create_test_players(player_service, player_ids):
    """Create test players in DynamoDB."""
    for pid in player_ids:
        asyncio.run(
            player_service.create_player(
                pid,
                f"Player_{pid}",
                {"steam": f"steam_{pid}"},
            )
        )


# =============================================================
# POST /matches/result
# =============================================================


class TestSubmitMatchResult:
    def test_missing_server_key_returns_401(self, aws_mock):
        from handlers.match_handler import submit_match_result

        event = _make_event(body={"game_session_id": "gs1"})
        status, body = _parse_response(
            submit_match_result(event, _CONTEXT)
        )

        assert status == 401
        assert body["error_code"] == "UNAUTHORIZED"

    def test_invalid_server_key_returns_401(self, aws_mock):
        from handlers.match_handler import submit_match_result

        event = _make_event(
            body={"game_session_id": "gs1"},
            headers={"X-Server-Key": "wrong-key"},
        )
        status, body = _parse_response(
            submit_match_result(event, _CONTEXT)
        )

        assert status == 401

    def test_missing_game_session_id_returns_400(
        self, aws_mock
    ):
        from handlers.match_handler import submit_match_result

        event = _make_event(
            body={"player_results": [{"player_id": "p_a", "rank": 1}]},
            headers=_server_headers(),
        )
        status, body = _parse_response(
            submit_match_result(event, _CONTEXT)
        )

        assert status == 400

    def test_missing_player_results_returns_400(
        self, aws_mock
    ):
        from handlers.match_handler import submit_match_result

        event = _make_event(
            body={"game_session_id": "gs1"},
            headers=_server_headers(),
        )
        status, body = _parse_response(
            submit_match_result(event, _CONTEXT)
        )

        assert status == 400

    def test_missing_rank_field_returns_400(self, aws_mock):
        from handlers.match_handler import submit_match_result

        event = _make_event(
            body={
                "game_session_id": "gs1",
                "player_results": [
                    {"player_id": "p_a"}
                ],
            },
            headers=_server_headers(),
        )
        status, body = _parse_response(
            submit_match_result(event, _CONTEXT)
        )

        assert status == 400
        assert "rank" in body["message"]

    def test_valid_submission_returns_200(self, aws_mock):
        from handlers.match_handler import submit_match_result
        from services.player_service import PlayerService

        ps = PlayerService()
        _create_test_players(ps, ["p_aaa", "p_bbb"])

        event = _make_event(
            body={
                "game_session_id": "gsess-test",
                "match_duration_sec": 90.0,
                "level_id": "level_0",
                "player_results": [
                    {
                        "player_id": "p_aaa",
                        "rank": 1,
                        "score": 100,
                        "kill_count": 3,
                    },
                    {
                        "player_id": "p_bbb",
                        "rank": 2,
                        "score": 50,
                        "kill_count": 1,
                    },
                ],
            },
            headers=_server_headers(),
        )
        status, body = _parse_response(
            submit_match_result(event, _CONTEXT)
        )

        assert status == 200
        assert body["status"] == "success"

    def test_updates_player_stats(self, aws_mock):
        from handlers.match_handler import submit_match_result
        from services.player_service import PlayerService

        ps = PlayerService()
        _create_test_players(ps, ["p_aaa", "p_bbb"])

        event = _make_event(
            body={
                "game_session_id": "gsess-test",
                "match_duration_sec": 60.0,
                "level_id": "level_0",
                "player_results": [
                    {"player_id": "p_aaa", "rank": 1},
                    {"player_id": "p_bbb", "rank": 2},
                ],
            },
            headers=_server_headers(),
        )
        submit_match_result(event, _CONTEXT)

        alice = asyncio.run(ps.get_player("p_aaa"))
        assert alice.matches_played == 1
        assert alice.wins == 1
        assert alice.rating > 1500

        bob = asyncio.run(ps.get_player("p_bbb"))
        assert bob.matches_played == 1
        assert bob.losses == 1
        assert bob.rating < 1500


# =============================================================
# GET /leaderboard
# =============================================================


class TestGetLeaderboard:
    def test_missing_auth_returns_leaderboard(self, aws_mock):
        from handlers.match_handler import get_leaderboard

        # Leaderboard is publicly accessible without auth.
        event = _make_event()
        status, body = _parse_response(
            get_leaderboard(event, _CONTEXT)
        )

        assert status == 200
        assert body["status"] == "success"
        assert "your_rank" not in body
        assert "your_rating" not in body

    def test_returns_sorted_leaderboard(self, aws_mock):
        from handlers.match_handler import get_leaderboard
        from services.player_service import PlayerService

        ps = PlayerService()
        # Create players with different ratings.
        for pid, name, rating in [
            ("p_aaa", "Alice", 1800),
            ("p_bbb", "Bob", 1600),
            ("p_ccc", "Carol", 1900),
        ]:
            asyncio.run(
                ps.create_player(
                    pid, name, {"steam": f"s_{pid}"}
                )
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

        event = _make_event(
            headers=_auth_headers("p_aaa"),
        )
        status, body = _parse_response(
            get_leaderboard(event, _CONTEXT)
        )

        assert status == 200
        assert body["status"] == "success"
        board = body["leaderboard"]
        assert len(board) == 3
        assert board[0]["display_name"] == "Carol"
        assert board[1]["display_name"] == "Alice"
        assert board[2]["display_name"] == "Bob"

    def test_includes_your_rank(self, aws_mock):
        from handlers.match_handler import get_leaderboard
        from services.player_service import PlayerService
        from services.leaderboard_service import (
            LeaderboardService,
        )

        ps = PlayerService()
        asyncio.run(
            ps.create_player(
                "p_me", "Me", {"steam": "s_me"}
            )
        )
        ps.table.update_item(
            Key={"player_id": "p_me"},
            UpdateExpression=(
                "SET rating = :r,"
                " rating_partition = :all"
            ),
            ExpressionAttributeValues={
                ":r": 1500,
                ":all": "all",
            },
        )

        # Insert a leaderboard entry so
        # leaderboard_service.get_player_rank can find
        # the player.
        ls = LeaderboardService()
        ls.update_score(
            leaderboard_id="alltime#global",
            player_id="p_me",
            old_rating=1500,
            new_rating=1500,
            display_name="Me",
        )

        event = _make_event(
            headers=_auth_headers("p_me"),
        )
        status, body = _parse_response(
            get_leaderboard(event, _CONTEXT)
        )

        assert status == 200
        assert body["your_rank"] == 1
        assert body["your_rating"] == 1500

    def test_respects_limit(self, aws_mock):
        from handlers.match_handler import get_leaderboard
        from services.player_service import PlayerService

        ps = PlayerService()
        for i in range(5):
            pid = f"p_{i}"
            asyncio.run(
                ps.create_player(
                    pid, f"P{i}", {"steam": f"s{i}"}
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

        event = _make_event(
            headers=_auth_headers("p_0"),
            query_params={"limit": "3"},
        )
        status, body = _parse_response(
            get_leaderboard(event, _CONTEXT)
        )

        assert status == 200
        assert len(body["leaderboard"]) == 3


# =============================================================
# GET /players/{player_id}/stats
# =============================================================


class TestGetPlayerStats:
    def test_missing_auth_returns_401(self, aws_mock):
        from handlers.match_handler import get_player_stats

        event = _make_event(
            path_params={"player_id": "p_aaa"},
        )
        status, body = _parse_response(
            get_player_stats(event, _CONTEXT)
        )

        assert status == 401

    def test_player_not_found_returns_404(self, aws_mock):
        from handlers.match_handler import get_player_stats

        event = _make_event(
            headers=_auth_headers("p_requester"),
            path_params={"player_id": "p_nonexistent"},
        )
        status, body = _parse_response(
            get_player_stats(event, _CONTEXT)
        )

        assert status == 404
        assert body["error_code"] == "NOT_FOUND"

    def test_returns_player_stats(self, aws_mock):
        from handlers.match_handler import get_player_stats
        from services.player_service import PlayerService

        ps = PlayerService()
        asyncio.run(
            ps.create_player(
                "p_aaa", "Alice", {"steam": "s1"}
            )
        )

        event = _make_event(
            headers=_auth_headers("p_aaa"),
            path_params={"player_id": "p_aaa"},
        )
        status, body = _parse_response(
            get_player_stats(event, _CONTEXT)
        )

        assert status == 200
        assert body["status"] == "success"
        player = body["player"]
        assert player["player_id"] == "p_aaa"
        assert player["display_name"] == "Alice"
        assert player["rating"] == 1500
        assert player["matches_played"] == 0

    def test_includes_recent_matches(self, aws_mock):
        from handlers.match_handler import (
            get_player_stats,
            submit_match_result,
        )
        from services.player_service import PlayerService

        ps = PlayerService()
        _create_test_players(ps, ["p_aaa", "p_bbb"])

        # Record a match first.
        submit_event = _make_event(
            body={
                "game_session_id": "gsess-stats",
                "match_duration_sec": 60.0,
                "level_id": "level_0",
                "player_results": [
                    {"player_id": "p_aaa", "rank": 1},
                    {"player_id": "p_bbb", "rank": 2},
                ],
            },
            headers=_server_headers(),
        )
        submit_match_result(submit_event, _CONTEXT)

        # Now query stats.
        event = _make_event(
            headers=_auth_headers("p_aaa"),
            path_params={"player_id": "p_aaa"},
        )
        status, body = _parse_response(
            get_player_stats(event, _CONTEXT)
        )

        assert status == 200
        assert len(body["recent_matches"]) == 1
        assert body["recent_matches"][0]["is_win"] is True
        assert body["player"]["wins"] == 1

    def test_win_rate_calculation(self, aws_mock):
        from handlers.match_handler import get_player_stats
        from services.player_service import PlayerService

        ps = PlayerService()
        asyncio.run(
            ps.create_player(
                "p_aaa", "Alice", {"steam": "s1"}
            )
        )
        # Manually set stats.
        ps.table.update_item(
            Key={"player_id": "p_aaa"},
            UpdateExpression=(
                "SET wins = :w, losses = :l,"
                " matches_played = :m,"
                " rating_partition = :all"
            ),
            ExpressionAttributeValues={
                ":w": 7,
                ":l": 3,
                ":m": 10,
                ":all": "all",
            },
        )

        event = _make_event(
            headers=_auth_headers("p_aaa"),
            path_params={"player_id": "p_aaa"},
        )
        status, body = _parse_response(
            get_player_stats(event, _CONTEXT)
        )

        assert status == 200
        assert body["player"]["win_rate"] == 70.0
