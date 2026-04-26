"""Tests for services.account_service."""

import asyncio

import pytest

from services.account_service import AccountService


def _run(coro):
    return asyncio.run(coro)


class TestGenerateIds:
    def test_player_id_format(self):
        pid = AccountService.generate_player_id()
        assert pid.startswith("p_")
        assert len(pid) == 14  # "p_" + 12 hex chars

    def test_player_id_unique(self):
        ids = {
            AccountService.generate_player_id()
            for _ in range(100)
        }
        assert len(ids) == 100

    def test_friend_code_format(self):
        code = AccountService._generate_friend_code()
        assert len(code) == 6
        assert code.isupper() or any(c.isdigit() for c in code)
        for c in code:
            assert c.isalnum()


class TestCreate:
    def test_creates_with_minimal_args(self, aws_mock):
        service = AccountService()
        profile = _run(
            service.create(
                player_id="p_test001",
                display_name="Tester",
            )
        )
        assert profile.player_id == "p_test001"
        assert profile.display_name == "Tester"
        assert len(profile.friend_code) == 6
        assert not profile.is_anonymous
        assert profile.created_at > 0
        assert profile.updated_at == profile.created_at

    def test_creates_anonymous(self, aws_mock):
        service = AccountService()
        profile = _run(
            service.create(
                player_id="p_anon001",
                display_name="Anon",
                is_anonymous=True,
                device_id="device-abc",
            )
        )
        assert profile.is_anonymous is True
        assert profile.device_id == "device-abc"

    def test_creates_with_oauth_providers(self, aws_mock):
        service = AccountService()
        profile = _run(
            service.create(
                player_id="p_oauth001",
                display_name="OAuth User",
                auth_providers={"google": "g_id_1"},
                provider_display_names={
                    "google": "Google Name"
                },
                provider_profile_images={
                    "google": "https://example.com/img"
                },
            )
        )
        assert profile.auth_providers == {"google": "g_id_1"}
        assert (
            profile.provider_display_names["google"]
            == "Google Name"
        )

    def test_creates_with_consent(self, aws_mock):
        service = AccountService()
        profile = _run(
            service.create(
                player_id="p_consent",
                display_name="Consenter",
                consent_accepted_at=1234567890,
                consent_legal_version="v1.2",
            )
        )
        assert profile.consent_accepted_at == 1234567890
        assert profile.consent_legal_version == "v1.2"


class TestGet:
    def test_returns_none_for_missing(self, aws_mock):
        service = AccountService()
        result = _run(service.get("p_does_not_exist"))
        assert result is None

    def test_round_trips_full_profile(self, aws_mock):
        service = AccountService()
        _run(
            service.create(
                player_id="p_round",
                display_name="RoundTrip",
                auth_providers={"steam": "76561"},
                provider_display_names={"steam": "SteamName"},
                consent_accepted_at=42,
                consent_legal_version="v9",
                profile_image_url="https://example.com/i.png",
                primary_locale="en",
            )
        )
        result = _run(service.get("p_round"))
        assert result is not None
        assert result.player_id == "p_round"
        assert result.display_name == "RoundTrip"
        assert result.auth_providers == {"steam": "76561"}
        assert result.consent_legal_version == "v9"
        assert result.primary_locale == "en"


class TestUpdates:
    def test_update_display_name(self, aws_mock):
        service = AccountService()
        _run(
            service.create(
                player_id="p_dn", display_name="Old"
            )
        )
        _run(
            service.update_display_name("p_dn", "New")
        )
        result = _run(service.get("p_dn"))
        assert result.display_name == "New"

    def test_update_locale(self, aws_mock):
        service = AccountService()
        _run(
            service.create(
                player_id="p_loc", display_name="L"
            )
        )
        _run(service.update_locale("p_loc", "fr"))
        result = _run(service.get("p_loc"))
        assert result.primary_locale == "fr"

    def test_update_consent(self, aws_mock):
        service = AccountService()
        _run(
            service.create(
                player_id="p_c", display_name="C"
            )
        )
        _run(
            service.update_consent("p_c", 1700000000, "v3")
        )
        result = _run(service.get("p_c"))
        assert result.consent_accepted_at == 1700000000
        assert result.consent_legal_version == "v3"

    def test_add_provider(self, aws_mock):
        service = AccountService()
        _run(
            service.create(
                player_id="p_prov",
                display_name="Prov",
                auth_providers={"google": "g_1"},
                provider_display_names={"google": "GName"},
                provider_profile_images={"google": "g.png"},
            )
        )
        _run(
            service.add_provider(
                "p_prov", "steam", "s_1", "SName", "s.png"
            )
        )
        result = _run(service.get("p_prov"))
        assert result.auth_providers == {
            "google": "g_1",
            "steam": "s_1",
        }
        assert (
            result.provider_display_names["steam"] == "SName"
        )

    def test_remove_provider(self, aws_mock):
        service = AccountService()
        _run(
            service.create(
                player_id="p_rm",
                display_name="Rm",
                auth_providers={
                    "google": "g_1",
                    "steam": "s_1",
                },
                provider_display_names={
                    "google": "g",
                    "steam": "s",
                },
                provider_profile_images={
                    "google": "g.png",
                    "steam": "s.png",
                },
            )
        )
        _run(service.remove_provider("p_rm", "google"))
        result = _run(service.get("p_rm"))
        assert result.auth_providers == {"steam": "s_1"}
        assert "google" not in result.provider_display_names

    def test_update_last_active(self, aws_mock):
        service = AccountService()
        _run(
            service.create(
                player_id="p_la", display_name="LA"
            )
        )
        before = _run(service.get("p_la"))
        # Wait a tick so the timestamp changes.
        import time
        time.sleep(1.1)
        _run(service.update_last_active("p_la"))
        after = _run(service.get("p_la"))
        assert after.last_active > before.last_active


class TestDelete:
    def test_delete_removes_row(self, aws_mock):
        service = AccountService()
        _run(
            service.create(
                player_id="p_del", display_name="Del"
            )
        )
        assert _run(service.get("p_del")) is not None
        _run(service.delete("p_del"))
        assert _run(service.get("p_del")) is None

    def test_delete_missing_is_noop(self, aws_mock):
        service = AccountService()
        # Should not raise.
        _run(service.delete("p_does_not_exist"))
