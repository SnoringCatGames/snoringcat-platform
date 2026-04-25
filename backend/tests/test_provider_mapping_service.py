"""Tests for provider_mapping_service.py."""

import asyncio

import pytest

from services.provider_mapping_service import (
    ProviderMappingService,
)


def _run(coro):
    """Run an async coroutine synchronously."""
    return asyncio.run(coro)


class TestMakeComposite:
    def test_format(self):
        assert (
            ProviderMappingService.make_composite(
                "steam", "76561198012345678"
            )
            == "steam#76561198012345678"
        )

    def test_anonymous_format(self):
        assert (
            ProviderMappingService.make_composite(
                "anonymous", "device-abc"
            )
            == "anonymous#device-abc"
        )


class TestLookup:
    def test_nonexistent_returns_none(self, aws_mock):
        pms = ProviderMappingService()
        result = _run(
            pms.lookup("steam", "nonexistent_id")
        )
        assert result is None

    def test_existing_returns_player_id(self, aws_mock):
        pms = ProviderMappingService()
        _run(
            pms.create("steam", "steam_123", "p_player1")
        )
        result = _run(pms.lookup("steam", "steam_123"))
        assert result == "p_player1"


class TestCreate:
    def test_different_providers_same_id(self, aws_mock):
        pms = ProviderMappingService()
        _run(
            pms.create("steam", "user_123", "p_steam_player")
        )
        _run(
            pms.create(
                "google", "user_123", "p_google_player"
            )
        )

        assert (
            _run(pms.lookup("steam", "user_123"))
            == "p_steam_player"
        )
        assert (
            _run(pms.lookup("google", "user_123"))
            == "p_google_player"
        )


class TestDelete:
    def test_delete_removes_mapping(self, aws_mock):
        pms = ProviderMappingService()
        _run(
            pms.create(
                "facebook", "fb_456", "p_player2"
            )
        )
        assert (
            _run(pms.lookup("facebook", "fb_456"))
            == "p_player2"
        )

        _run(pms.delete("facebook", "fb_456"))
        assert (
            _run(pms.lookup("facebook", "fb_456")) is None
        )

    def test_delete_nonexistent_no_error(self, aws_mock):
        pms = ProviderMappingService()
        # Should not raise.
        _run(pms.delete("steam", "does_not_exist"))
