"""Tests for the new cross-game presence_service."""

import asyncio
import time

import pytest


def _run(coro):
    return asyncio.run(coro)


def _service(default_ttl_sec=300):
    """Re-import after aws_mock so we use the moto-bound module."""
    from services.presence_service import PresenceService

    return PresenceService(default_ttl_sec=default_ttl_sec)


class TestHeartbeat:
    def test_writes_row_with_game_id(self, aws_mock):
        service = _service()
        result = _run(
            service.heartbeat(
                player_id="p_1", game_id="hopnbop"
            )
        )
        assert result.player_id == "p_1"
        assert result.game_id == "hopnbop"
        assert result.status == "online"
        assert result.updated_at > 0
        assert result.ttl > result.updated_at

    def test_round_trips(self, aws_mock):
        service = _service()
        _run(
            service.heartbeat(
                player_id="p_1",
                game_id="hopnbop",
                rich_presence="In lobby",
                session_id="sess-123",
            )
        )
        loaded = _run(service.get("p_1"))
        assert loaded is not None
        assert loaded.game_id == "hopnbop"
        assert loaded.rich_presence == "In lobby"
        assert loaded.session_id == "sess-123"

    def test_overwrites_previous(self, aws_mock):
        service = _service()
        _run(
            service.heartbeat(
                player_id="p_1", game_id="hopnbop"
            )
        )
        _run(
            service.heartbeat(
                player_id="p_1", game_id="nextgame"
            )
        )
        loaded = _run(service.get("p_1"))
        assert loaded.game_id == "nextgame"


class TestGet:
    def test_missing_returns_none(self, aws_mock):
        service = _service()
        assert _run(service.get("p_does_not_exist")) is None

    def test_expired_returns_none(self, aws_mock):
        """A row whose TTL is in the past reads as None."""
        service = _service(default_ttl_sec=-1)
        _run(
            service.heartbeat(
                player_id="p_expired", game_id="hopnbop"
            )
        )
        # The row exists physically (DynamoDB hasn't swept it
        # yet) but read-time TTL check filters it out.
        assert _run(service.get("p_expired")) is None


class TestClear:
    def test_removes_row(self, aws_mock):
        service = _service()
        _run(
            service.heartbeat(
                player_id="p_1", game_id="hopnbop"
            )
        )
        _run(service.clear("p_1"))
        assert _run(service.get("p_1")) is None

    def test_missing_is_noop(self, aws_mock):
        service = _service()
        # Should not raise.
        _run(service.clear("p_does_not_exist"))


class TestBatchGet:
    def test_empty_list_returns_empty_dict(self, aws_mock):
        service = _service()
        result = _run(service.batch_get([]))
        assert result == {}

    def test_returns_only_present_players(self, aws_mock):
        service = _service()
        _run(service.heartbeat("p_1", "hopnbop"))
        _run(service.heartbeat("p_2", "nextgame"))
        result = _run(
            service.batch_get(
                ["p_1", "p_2", "p_missing"]
            )
        )
        assert set(result.keys()) == {"p_1", "p_2"}
        assert result["p_1"].game_id == "hopnbop"
        assert result["p_2"].game_id == "nextgame"

    def test_dedupes_input(self, aws_mock):
        service = _service()
        _run(service.heartbeat("p_1", "hopnbop"))
        result = _run(
            service.batch_get(["p_1", "p_1", "p_1"])
        )
        assert list(result.keys()) == ["p_1"]

    def test_skips_expired_rows(self, aws_mock):
        service = _service(default_ttl_sec=-1)
        _run(service.heartbeat("p_1", "hopnbop"))
        result = _run(service.batch_get(["p_1"]))
        assert result == {}


class TestRichPresence:
    def test_empty_rich_presence_omitted(self, aws_mock):
        service = _service()
        _run(
            service.heartbeat(
                "p_1", "hopnbop", rich_presence=""
            )
        )
        loaded = _run(service.get("p_1"))
        assert loaded.rich_presence == ""

    def test_status_defaults_to_online(self, aws_mock):
        service = _service()
        _run(service.heartbeat("p_1", "hopnbop"))
        loaded = _run(service.get("p_1"))
        assert loaded.status == "online"

    def test_custom_status(self, aws_mock):
        service = _service()
        _run(
            service.heartbeat(
                "p_1", "hopnbop", status="in_match"
            )
        )
        loaded = _run(service.get("p_1"))
        assert loaded.status == "in_match"
