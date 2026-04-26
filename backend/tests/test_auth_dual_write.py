"""Verify auth_handler dual-writes to both legacy and new tables.

When a player signs in (anonymous or OAuth), auth_handler must:
- Continue to write the legacy account row via player_service
  (so existing readers keep working).
- Additionally create the per-game profile row via profile_service
  (so the new game-aware schema populates without a backfill).

These tests exercise the handler end-to-end against moto and assert
both rows exist after sign-in.
"""

import asyncio
import json

import pytest


def _run(coro):
    return asyncio.run(coro)


def _profile_service():
    """Re-import after aws_mock has reloaded the module."""
    from services.profile_service import ProfileService

    return ProfileService()


def _player_service():
    from services.player_service import PlayerService

    return PlayerService()


def _make_event(body):
    """API Gateway proxy event with a JSON body."""
    return {
        "body": json.dumps(body),
        "headers": {},
        "pathParameters": {},
    }


class _FakeContext:
    function_name = "test"
    function_version = "1"
    invoked_function_arn = (
        "arn:aws:lambda:us-west-2:000000000000:function:test"
    )
    memory_limit_in_mb = 128
    aws_request_id = "test"
    log_group_name = "/aws/lambda/test"
    log_stream_name = "test"

    def get_remaining_time_in_millis(self):
        return 60_000


class TestAnonymousLogin:
    def test_writes_to_both_tables(self, aws_mock):
        from handlers import auth_handler

        event = _make_event(
            {
                "device_id": "device-dual-1",
                "game_id": "hopnbop",
            }
        )
        response = auth_handler.anonymous_login(
            event, _FakeContext()
        )
        assert response["statusCode"] == 200
        body = json.loads(response["body"])
        player_id = body["player_id"]

        # Legacy player row.
        legacy = _run(
            _player_service().get_player(player_id)
        )
        assert legacy is not None

        # New per-game profile row.
        profile = _run(
            _profile_service().get(player_id, "hopnbop")
        )
        assert profile is not None
        assert profile.game_id == "hopnbop"

    def test_falls_back_to_default_game_id(self, aws_mock):
        """Clients that omit game_id get the default."""
        from handlers import auth_handler

        event = _make_event({"device_id": "device-dual-2"})
        response = auth_handler.anonymous_login(
            event, _FakeContext()
        )
        assert response["statusCode"] == 200
        player_id = json.loads(response["body"])["player_id"]

        # DEFAULT_GAME_ID = "hopnbop" in test conftest.
        profile = _run(
            _profile_service().get(player_id, "hopnbop")
        )
        assert profile is not None

    def test_repeated_login_is_idempotent(self, aws_mock):
        """Two sign-ins with the same device produce one of each row."""
        from handlers import auth_handler

        event = _make_event(
            {
                "device_id": "device-dual-3",
                "game_id": "hopnbop",
            }
        )
        first = auth_handler.anonymous_login(
            event, _FakeContext()
        )
        second = auth_handler.anonymous_login(
            event, _FakeContext()
        )
        assert first["statusCode"] == 200
        assert second["statusCode"] == 200
        first_pid = json.loads(first["body"])["player_id"]
        second_pid = json.loads(second["body"])["player_id"]
        assert first_pid == second_pid

        # Only one profile row exists.
        profiles = _run(
            _profile_service().list_for_player(first_pid)
        )
        assert len(profiles) == 1

    def test_two_games_get_two_profiles(self, aws_mock):
        """Same device, two different game_ids → two profile rows."""
        from handlers import auth_handler

        # Sign in from game A.
        first = auth_handler.anonymous_login(
            _make_event(
                {
                    "device_id": "device-dual-4",
                    "game_id": "hopnbop",
                }
            ),
            _FakeContext(),
        )
        # Sign in from game B with the same device.
        second = auth_handler.anonymous_login(
            _make_event(
                {
                    "device_id": "device-dual-4",
                    "game_id": "nextgame",
                }
            ),
            _FakeContext(),
        )

        first_pid = json.loads(first["body"])["player_id"]
        second_pid = json.loads(second["body"])["player_id"]
        # Same account spans both games.
        assert first_pid == second_pid

        profiles = _run(
            _profile_service().list_for_player(first_pid)
        )
        game_ids = {p.game_id for p in profiles}
        assert game_ids == {"hopnbop", "nextgame"}


class TestDualWriteFailure:
    def test_profile_write_failure_does_not_block_signin(
        self, aws_mock, monkeypatch
    ):
        """Sign-in must still succeed if profile_service throws."""
        from handlers import auth_handler

        # Force profile_service.get_or_create to raise.
        async def _boom(*args, **kwargs):
            raise RuntimeError("simulated DDB outage")

        monkeypatch.setattr(
            auth_handler.profile_service,
            "get_or_create",
            _boom,
        )

        response = auth_handler.anonymous_login(
            _make_event(
                {
                    "device_id": "device-dual-5",
                    "game_id": "hopnbop",
                }
            ),
            _FakeContext(),
        )
        # Sign-in returns success; the legacy row was written.
        assert response["statusCode"] == 200
        player_id = json.loads(response["body"])["player_id"]
        assert (
            _run(_player_service().get_player(player_id))
            is not None
        )
        # Profile row was NOT created (write was blocked).
        profile = _run(
            _profile_service().get(player_id, "hopnbop")
        )
        assert profile is None
