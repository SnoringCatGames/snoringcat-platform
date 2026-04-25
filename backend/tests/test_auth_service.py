"""Tests for auth_service.py - AuthToken and AuthService."""

import asyncio
from datetime import datetime, timedelta

import jwt
import pytest

from services.auth_service import AuthService, AuthToken, AuthResult
from tests.constants import TEST_JWT_SECRET
from tests.conftest import mock_httpx_client, make_response


def _run(coro):
    """Run an async coroutine synchronously."""
    return asyncio.run(coro)


# =====================================================================
# AuthToken
# =====================================================================


class TestAuthToken:
    def test_round_trip(self):
        now = datetime.now()
        token = AuthToken(
            player_id="p_abc123",
            display_name="TestPlayer",
            provider="steam",
            is_anonymous=False,
            issued_at=now,
            expires_at=now + timedelta(hours=24),
        )

        encoded = token.to_jwt(TEST_JWT_SECRET)
        decoded = AuthToken.from_jwt(encoded, TEST_JWT_SECRET)

        assert decoded.player_id == "p_abc123"
        assert decoded.display_name == "TestPlayer"
        assert decoded.provider == "steam"
        assert decoded.is_anonymous is False

    def test_anonymous_round_trip(self):
        now = datetime.now()
        token = AuthToken(
            player_id="p_anon999",
            display_name="AnonPlayer",
            provider="anonymous",
            is_anonymous=True,
            issued_at=now,
            expires_at=now + timedelta(hours=24),
        )

        encoded = token.to_jwt(TEST_JWT_SECRET)
        decoded = AuthToken.from_jwt(encoded, TEST_JWT_SECRET)
        assert decoded.is_anonymous is True
        assert decoded.provider == "anonymous"

    def test_expired_token_raises(self):
        now = datetime.now()
        token = AuthToken(
            player_id="p_expired",
            display_name="Expired",
            provider="steam",
            is_anonymous=False,
            issued_at=now - timedelta(hours=25),
            expires_at=now - timedelta(hours=1),
        )

        encoded = token.to_jwt(TEST_JWT_SECRET)

        with pytest.raises(ValueError, match="Token expired"):
            AuthToken.from_jwt(encoded, TEST_JWT_SECRET)

    def test_tampered_token_raises(self):
        now = datetime.now()
        token = AuthToken(
            player_id="p_tampered",
            display_name="Tampered",
            provider="steam",
            is_anonymous=False,
            issued_at=now,
            expires_at=now + timedelta(hours=24),
        )

        encoded = token.to_jwt(TEST_JWT_SECRET)
        tampered = encoded[:-5] + "XXXXX"

        with pytest.raises(ValueError, match="Invalid token"):
            AuthToken.from_jwt(tampered, TEST_JWT_SECRET)

    def test_wrong_secret_raises(self):
        now = datetime.now()
        token = AuthToken(
            player_id="p_wrong",
            display_name="WrongKey",
            provider="steam",
            is_anonymous=False,
            issued_at=now,
            expires_at=now + timedelta(hours=24),
        )

        encoded = token.to_jwt(TEST_JWT_SECRET)

        with pytest.raises(ValueError, match="Invalid token"):
            AuthToken.from_jwt(encoded, "wrong-secret")


# =====================================================================
# AuthService.create_auth_token
# =====================================================================


class TestCreateAuthToken:
    def test_correct_lifetime(self):
        service = AuthService(token_lifetime_hours=12)
        token = service.create_auth_token(
            "p_test123", "TestName", "google"
        )

        assert token.player_id == "p_test123"
        assert token.display_name == "TestName"
        assert token.provider == "google"
        assert token.is_anonymous is False
        delta = token.expires_at - token.issued_at
        assert abs(delta.total_seconds() - 12 * 3600) < 2

    def test_anonymous_flag(self):
        service = AuthService()
        token = service.create_auth_token(
            "p_anon", "Anon", "anonymous", is_anonymous=True
        )
        assert token.is_anonymous is True


# =====================================================================
# AuthService.authenticate - dispatch
# =====================================================================


class TestAuthenticateDispatch:
    def test_unsupported_provider_raises(self, aws_mock):
        service = AuthService()
        with pytest.raises(
            ValueError, match="Unsupported provider"
        ):
            _run(service.authenticate("foobar", "code123"))


# =====================================================================
# Steam
# =====================================================================


class TestAuthSteam:
    def test_valid_ticket(self, aws_mock):
        auth_resp = make_response(200, {
            "response": {
                "params": {"steamid": "76561198012345678"}
            },
        })
        user_resp = make_response(200, {
            "response": {
                "players": [{"personaname": "SteamPlayer"}]
            },
        })

        service = AuthService()
        with mock_httpx_client([auth_resp, user_resp]):
            result = _run(
                service._auth_steam("FAKE_TICKET")
            )

        assert result.provider == "steam"
        assert result.provider_id == "76561198012345678"
        assert result.display_name == "SteamPlayer"

    def test_invalid_ticket_raises(self, aws_mock):
        resp = make_response(403)

        service = AuthService()
        with mock_httpx_client(resp):
            with pytest.raises(
                ValueError,
                match="Steam authentication failed",
            ):
                _run(service._auth_steam("BAD_TICKET"))


# =====================================================================
# Google
# =====================================================================


class TestAuthGoogle:
    def test_valid_code(self, aws_mock):
        fake_id_token = jwt.encode(
            {"sub": "google_user_123", "name": "GoogleUser"},
            "not-verified",
            algorithm="HS256",
        )
        resp = make_response(200, {
            "id_token": fake_id_token,
            "access_token": "fake_access",
        })

        service = AuthService()
        with mock_httpx_client(resp):
            result = _run(
                service._auth_google(
                    "AUTH_CODE",
                    "http://127.0.0.1:9876/callback",
                )
            )

        assert result.provider == "google"
        assert result.provider_id == "google_user_123"
        assert result.display_name == "GoogleUser"

    def test_invalid_code_raises(self, aws_mock):
        resp = make_response(403)

        service = AuthService()
        with mock_httpx_client(resp):
            with pytest.raises(
                ValueError,
                match="Google token exchange failed",
            ):
                _run(
                    service._auth_google(
                        "BAD_CODE",
                        "http://127.0.0.1:9876/callback",
                    )
                )


# =====================================================================
# Epic
# =====================================================================


class TestAuthEpic:
    def test_valid_token(self, aws_mock):
        resp = make_response(200, {
            "account_id": "epic_acc_321",
            "display_name": "EpicGamer",
        })

        service = AuthService()
        with mock_httpx_client(resp):
            result = _run(
                service._auth_epic("FAKE_ACCESS_TOKEN")
            )

        assert result.provider == "epic"
        assert result.provider_id == "epic_acc_321"
        assert result.display_name == "EpicGamer"

    def test_invalid_token_raises(self, aws_mock):
        resp = make_response(401)

        service = AuthService()
        with mock_httpx_client(resp):
            with pytest.raises(
                ValueError,
                match="Epic authentication failed",
            ):
                _run(service._auth_epic("BAD_TOKEN"))


# =====================================================================
# Apple
# =====================================================================


class TestAuthApple:
    def test_valid_code(self, aws_mock):
        fake_id_token = jwt.encode(
            {
                "sub": "apple_user_001",
                "email": "apple@example.com",
            },
            "not-verified",
            algorithm="HS256",
        )
        resp = make_response(200, {
            "id_token": fake_id_token,
            "access_token": "apple_access",
        })

        service = AuthService()
        # Apple uses ES256 to sign the client_secret JWT.
        # With an empty private_key in test secrets, jwt.encode
        # will fail. Patch it to skip client_secret generation.
        with mock_httpx_client(resp):
            # Patch jwt.encode for the client_secret only
            # (the id_token decode uses options=no_verify).
            import unittest.mock as um

            original_encode = jwt.encode

            def _patched_encode(payload, key, **kwargs):
                if kwargs.get("algorithm") == "ES256":
                    return "fake-apple-client-secret"
                return original_encode(payload, key, **kwargs)

            with um.patch(
                "services.auth_service.jwt.encode",
                side_effect=_patched_encode,
            ):
                result = _run(
                    service._auth_apple(
                        "AUTH_CODE",
                        "http://127.0.0.1:9876/callback",
                    )
                )

        assert result.provider == "apple"
        assert result.provider_id == "apple_user_001"
        assert result.display_name == "apple@example.com"

    def test_token_exchange_failure_raises(self, aws_mock):
        resp = make_response(403)

        service = AuthService()
        import unittest.mock as um

        with mock_httpx_client(resp):
            with um.patch(
                "services.auth_service.jwt.encode",
                return_value="fake-apple-client-secret",
            ):
                with pytest.raises(
                    ValueError,
                    match="Apple token exchange failed",
                ):
                    _run(
                        service._auth_apple(
                            "BAD_CODE",
                            "http://127.0.0.1:9876/callback",
                        )
                    )
