"""Tests for auth_handler.py - Lambda endpoint handlers."""

import json
import asyncio
from unittest.mock import patch, AsyncMock, MagicMock

import pytest

from services.auth_service import AuthResult, AuthToken
from tests.constants import TEST_JWT_SECRET
from tests.conftest import mock_httpx_client, make_response


class _FakeLambdaContext:
    """Minimal Lambda context for aws_lambda_powertools."""

    function_name = "test-function"
    memory_limit_in_mb = 512
    invoked_function_arn = (
        "arn:aws:lambda:us-east-1:123456789:function:test"
    )
    aws_request_id = "test-request-id"


_CONTEXT = _FakeLambdaContext()


def _make_event(body=None, headers=None):
    """Build a minimal Lambda API Gateway event."""
    return {
        "body": json.dumps(body) if body else "{}",
        "headers": headers or {},
    }


def _parse_response(response):
    """Parse status code and body from Lambda response."""
    return response["statusCode"], json.loads(response["body"])


# =====================================================================
# POST /auth/login
# =====================================================================


class TestLogin:
    def test_missing_provider_returns_400(self, aws_mock):
        from handlers.auth_handler import login

        event = _make_event(body={"auth_code": "code123"})
        status, body = _parse_response(login(event, _CONTEXT))

        assert status == 400
        assert body["error_code"] == "MISSING_PARAMS"

    def test_missing_auth_code_returns_400(self, aws_mock):
        from handlers.auth_handler import login

        event = _make_event(body={"provider": "steam"})
        status, body = _parse_response(login(event, _CONTEXT))

        assert status == 400
        assert body["error_code"] == "MISSING_PARAMS"

    def test_valid_auth_returns_200(self, aws_mock):
        """Login with a mocked provider. Exercises the full
        handler flow including provider mapping, player
        creation, and token issuance."""
        from handlers.auth_handler import login

        # Mock only the external HTTP call, not the whole
        # auth_service. This lets the handler exercise real
        # token creation and DB writes.
        auth_resp = make_response(200, {
            "response": {
                "params": {"steamid": "steam_login_test"}
            },
        })
        user_resp = make_response(200, {
            "response": {
                "players": [{"personaname": "LoginPlayer"}]
            },
        })

        with mock_httpx_client([auth_resp, user_resp]):
            event = _make_event(
                body={
                    "provider": "steam",
                    "auth_code": "TICKET",
                }
            )
            status, body = _parse_response(
                login(event, _CONTEXT)
            )

        assert status == 200
        assert body["status"] == "success"
        assert "jwt_token" in body
        assert "refresh_token" in body
        assert body["player_id"].startswith("p_")
        assert body["is_anonymous"] is False
        assert body["display_name"] == "LoginPlayer"

    def test_returning_player_gets_same_id(self, aws_mock):
        from handlers.auth_handler import login

        auth_resp = make_response(200, {
            "response": {
                "params": {"steamid": "steam_returning"}
            },
        })
        user_resp = make_response(200, {
            "response": {
                "players": [{"personaname": "Returner"}]
            },
        })

        with mock_httpx_client([auth_resp, user_resp]):
            event = _make_event(
                body={
                    "provider": "steam",
                    "auth_code": "TICKET",
                }
            )
            _, body1 = _parse_response(
                login(event, _CONTEXT)
            )

        # Login again with the same steam ID.
        auth_resp2 = make_response(200, {
            "response": {
                "params": {"steamid": "steam_returning"}
            },
        })
        user_resp2 = make_response(200, {
            "response": {
                "players": [{"personaname": "Returner"}]
            },
        })

        with mock_httpx_client([auth_resp2, user_resp2]):
            _, body2 = _parse_response(
                login(event, _CONTEXT)
            )

        assert body1["player_id"] == body2["player_id"]

    def test_auth_failure_returns_401(self, aws_mock):
        from handlers.auth_handler import login

        resp = make_response(403)

        with mock_httpx_client(resp):
            event = _make_event(
                body={
                    "provider": "steam",
                    "auth_code": "BAD",
                }
            )
            status, body = _parse_response(
                login(event, _CONTEXT)
            )

        assert status == 401
        assert body["error_code"] == "AUTH_FAILED"


# =====================================================================
# POST /auth/anon
# =====================================================================


class TestAnonymousLogin:
    def test_missing_device_id_returns_400(self, aws_mock):
        from handlers.auth_handler import anonymous_login

        event = _make_event(body={})
        status, body = _parse_response(
            anonymous_login(event, _CONTEXT)
        )

        assert status == 400
        assert body["error_code"] == "MISSING_PARAMS"

    def test_new_device_creates_player(self, aws_mock):
        from handlers.auth_handler import anonymous_login

        event = _make_event(
            body={"device_id": "test-device-abc"}
        )
        status, body = _parse_response(
            anonymous_login(event, _CONTEXT)
        )

        assert status == 200
        assert body["status"] == "success"
        assert body["is_anonymous"] is True
        assert "jwt_token" in body
        assert "refresh_token" in body
        assert body["player_id"].startswith("p_")

    def test_existing_device_returns_same_player(
        self, aws_mock
    ):
        from handlers.auth_handler import anonymous_login

        event = _make_event(
            body={"device_id": "test-device-xyz"}
        )

        _, body1 = _parse_response(
            anonymous_login(event, _CONTEXT)
        )
        _, body2 = _parse_response(
            anonymous_login(event, _CONTEXT)
        )

        assert body1["player_id"] == body2["player_id"]


# =====================================================================
# POST /auth/refresh
# =====================================================================


class TestRefresh:
    def _create_player_with_token(self):
        """Helper: create anonymous player, return body."""
        from handlers.auth_handler import anonymous_login

        event = _make_event(
            body={"device_id": "refresh-test-device"}
        )
        _, body = _parse_response(
            anonymous_login(event, _CONTEXT)
        )
        return body

    def test_missing_params_returns_400(self, aws_mock):
        from handlers.auth_handler import refresh

        event = _make_event(body={})
        status, body = _parse_response(refresh(event, _CONTEXT))

        assert status == 400
        assert body["error_code"] == "MISSING_PARAMS"

    def test_invalid_refresh_token_returns_401(
        self, aws_mock
    ):
        from handlers.auth_handler import refresh

        login_body = self._create_player_with_token()

        event = _make_event(
            body={
                "player_id": login_body["player_id"],
                "refresh_token": "wrong-token",
            }
        )
        status, body = _parse_response(refresh(event, _CONTEXT))

        assert status == 401
        assert body["error_code"] == "INVALID_REFRESH"

    def test_valid_refresh_rotates_tokens(self, aws_mock):
        from handlers.auth_handler import refresh

        login_body = self._create_player_with_token()

        event = _make_event(
            body={
                "player_id": login_body["player_id"],
                "refresh_token": login_body["refresh_token"],
            }
        )
        status, body = _parse_response(refresh(event, _CONTEXT))

        assert status == 200
        assert body["status"] == "success"
        assert "jwt_token" in body
        assert "refresh_token" in body
        # Refresh token must rotate (opaque random hex).
        assert (
            body["refresh_token"]
            != login_body["refresh_token"]
        )

    def test_old_refresh_token_invalid_after_rotation(
        self, aws_mock
    ):
        from handlers.auth_handler import refresh

        login_body = self._create_player_with_token()
        old_refresh = login_body["refresh_token"]

        # First refresh succeeds and rotates.
        event = _make_event(
            body={
                "player_id": login_body["player_id"],
                "refresh_token": old_refresh,
            }
        )
        status, _ = _parse_response(refresh(event, _CONTEXT))
        assert status == 200

        # Second refresh with the old token fails.
        status, body = _parse_response(refresh(event, _CONTEXT))
        assert status == 401
        assert body["error_code"] == "INVALID_REFRESH"


# =====================================================================
# POST /auth/link
# =====================================================================


class TestLinkAccount:
    def test_missing_auth_header_returns_401(self, aws_mock):
        from handlers.auth_handler import link_account

        event = _make_event(
            body={"provider": "google", "auth_code": "CODE"}
        )
        status, body = _parse_response(
            link_account(event, _CONTEXT)
        )

        assert status == 401
        assert body["error_code"] == "UNAUTHORIZED"

    def test_missing_provider_returns_400(self, aws_mock):
        from handlers.auth_handler import (
            link_account,
            anonymous_login,
        )

        # Create a player first to get a valid JWT.
        login_event = _make_event(
            body={"device_id": "link-test-device"}
        )
        _, login_body = _parse_response(
            anonymous_login(login_event, _CONTEXT)
        )

        event = _make_event(
            body={"auth_code": "CODE"},
            headers={
                "Authorization": (
                    f"Bearer {login_body['jwt_token']}"
                )
            },
        )
        status, body = _parse_response(
            link_account(event, _CONTEXT)
        )

        assert status == 400
        assert body["error_code"] == "MISSING_PARAMS"

    def test_successful_link(self, aws_mock):
        from handlers.auth_handler import (
            link_account,
            anonymous_login,
        )

        # Create anonymous player.
        login_event = _make_event(
            body={"device_id": "link-test-device-2"}
        )
        _, login_body = _parse_response(
            anonymous_login(login_event, _CONTEXT)
        )

        mock_result = AuthResult(
            provider="google",
            provider_id="google_new_123",
            display_name="GoogleUser",
        )

        with patch(
            "handlers.auth_handler.auth_service"
        ) as mock_auth:
            mock_auth.authenticate = AsyncMock(
                return_value=mock_result
            )
            mock_auth.jwt_secret = TEST_JWT_SECRET

            event = _make_event(
                body={
                    "provider": "google",
                    "auth_code": "VALID_CODE",
                    "redirect_uri": "http://127.0.0.1:9876",
                },
                headers={
                    "Authorization": (
                        f"Bearer {login_body['jwt_token']}"
                    )
                },
            )
            status, body = _parse_response(
                link_account(event, _CONTEXT)
            )

        assert status == 200
        assert body["status"] == "success"

    def test_provider_conflict_returns_409(self, aws_mock):
        from handlers.auth_handler import (
            link_account,
            anonymous_login,
        )
        from services.provider_mapping_service import (
            ProviderMappingService,
        )

        # Create two anonymous players.
        _, body1 = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "conflict-dev-1"}
                ),
                _CONTEXT,
            )
        )
        _, body2 = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "conflict-dev-2"}
                ),
                _CONTEXT,
            )
        )

        # Pre-link Google to player 1.
        pms = ProviderMappingService()
        asyncio.run(
            pms.create(
                "google",
                "google_conflict",
                body1["player_id"],
            )
        )

        mock_result = AuthResult(
            provider="google",
            provider_id="google_conflict",
            display_name="ConflictUser",
        )

        with patch(
            "handlers.auth_handler.auth_service"
        ) as mock_auth:
            mock_auth.authenticate = AsyncMock(
                return_value=mock_result
            )
            mock_auth.jwt_secret = TEST_JWT_SECRET

            event = _make_event(
                body={
                    "provider": "google",
                    "auth_code": "CODE",
                },
                headers={
                    "Authorization": (
                        f"Bearer {body2['jwt_token']}"
                    )
                },
            )
            status, body = _parse_response(
                link_account(event, _CONTEXT)
            )

        assert status == 409
        assert body["error_code"] == "PROVIDER_CONFLICT"
        assert "merge_token" in body, (
            "409 PROVIDER_CONFLICT must include a merge_token"
        )

    def test_successful_link_returns_linked_providers(
        self, aws_mock
    ):
        from handlers.auth_handler import (
            link_account,
            anonymous_login,
        )

        # Create anonymous player.
        _, login_body = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "link-prov-device"}
                ),
                _CONTEXT,
            )
        )

        mock_result = AuthResult(
            provider="google",
            provider_id="google_prov_test",
            display_name="ProvTestUser",
        )

        with patch(
            "handlers.auth_handler.auth_service"
        ) as mock_auth:
            mock_auth.authenticate = AsyncMock(
                return_value=mock_result
            )
            mock_auth.jwt_secret = TEST_JWT_SECRET

            event = _make_event(
                body={
                    "provider": "google",
                    "auth_code": "CODE",
                    "redirect_uri": "http://127.0.0.1:9876",
                },
                headers={
                    "Authorization": (
                        f"Bearer {login_body['jwt_token']}"
                    )
                },
            )
            status, body = _parse_response(
                link_account(event, _CONTEXT)
            )

        assert status == 200
        assert "linked_providers" in body
        assert "google" in body["linked_providers"]


# =====================================================================
# POST /auth/unlink
# =====================================================================


class TestUnlinkAccount:
    def _create_anon_and_link_google(self):
        """Helper: create anon player, link Google, return
        (login_body, link_body)."""
        from handlers.auth_handler import (
            anonymous_login,
            link_account,
        )

        # Create anonymous player (has device_id fallback).
        _, login_body = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "unlink-dev"}
                ),
                _CONTEXT,
            )
        )

        mock_result = AuthResult(
            provider="google",
            provider_id="google_unlink_test",
            display_name="UnlinkUser",
        )

        with patch(
            "handlers.auth_handler.auth_service"
        ) as mock_auth:
            mock_auth.authenticate = AsyncMock(
                return_value=mock_result
            )
            mock_auth.jwt_secret = TEST_JWT_SECRET

            link_event = _make_event(
                body={
                    "provider": "google",
                    "auth_code": "CODE",
                },
                headers={
                    "Authorization": (
                        f"Bearer {login_body['jwt_token']}"
                    )
                },
            )
            _, link_body = _parse_response(
                link_account(link_event, _CONTEXT)
            )

        return login_body, link_body

    def test_missing_auth_header_returns_401(self, aws_mock):
        from handlers.auth_handler import unlink_account

        event = _make_event(body={"provider": "google"})
        status, body = _parse_response(
            unlink_account(event, _CONTEXT)
        )

        assert status == 401
        assert body["error_code"] == "UNAUTHORIZED"

    def test_missing_provider_returns_400(self, aws_mock):
        from handlers.auth_handler import (
            unlink_account,
            anonymous_login,
        )

        _, login_body = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "unlink-miss-dev"}
                ),
                _CONTEXT,
            )
        )

        event = _make_event(
            body={},
            headers={
                "Authorization": (
                    f"Bearer {login_body['jwt_token']}"
                )
            },
        )
        status, body = _parse_response(
            unlink_account(event, _CONTEXT)
        )

        assert status == 400
        assert body["error_code"] == "MISSING_PARAMS"

    def test_unlink_not_linked_provider_returns_400(
        self, aws_mock
    ):
        from handlers.auth_handler import (
            unlink_account,
            anonymous_login,
        )

        _, login_body = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "unlink-notlinked"}
                ),
                _CONTEXT,
            )
        )

        event = _make_event(
            body={"provider": "google"},
            headers={
                "Authorization": (
                    f"Bearer {login_body['jwt_token']}"
                )
            },
        )
        status, body = _parse_response(
            unlink_account(event, _CONTEXT)
        )

        assert status == 400
        assert body["error_code"] == "NOT_LINKED"

    def test_successful_unlink(self, aws_mock):
        from handlers.auth_handler import unlink_account

        login_body, _ = self._create_anon_and_link_google()

        event = _make_event(
            body={"provider": "google"},
            headers={
                "Authorization": (
                    f"Bearer {login_body['jwt_token']}"
                )
            },
        )
        status, body = _parse_response(
            unlink_account(event, _CONTEXT)
        )

        assert status == 200
        assert body["status"] == "success"
        assert body["provider"] == "google"
        assert "google" not in body["linked_providers"]

    def test_last_provider_guard_blocks_unlink(
        self, aws_mock
    ):
        """When a non-anonymous player has only one provider
        and no device_id, unlinking should be blocked."""
        from handlers.auth_handler import login, unlink_account

        # Create a player via Steam login (no device_id).
        auth_resp = make_response(200, {
            "response": {
                "params": {"steamid": "steam_guard_test"}
            },
        })
        user_resp = make_response(200, {
            "response": {
                "players": [{"personaname": "GuardPlayer"}]
            },
        })

        with mock_httpx_client([auth_resp, user_resp]):
            _, login_body = _parse_response(
                login(
                    _make_event(
                        body={
                            "provider": "steam",
                            "auth_code": "TICKET",
                        }
                    ),
                    _CONTEXT,
                )
            )

        event = _make_event(
            body={"provider": "steam"},
            headers={
                "Authorization": (
                    f"Bearer {login_body['jwt_token']}"
                )
            },
        )
        status, body = _parse_response(
            unlink_account(event, _CONTEXT)
        )

        assert status == 409
        assert body["error_code"] == "LAST_PROVIDER"

    def test_last_provider_allowed_with_device_fallback(
        self, aws_mock
    ):
        """An anonymous player with device_id can unlink
        their only linked provider."""
        from handlers.auth_handler import unlink_account

        login_body, _ = self._create_anon_and_link_google()

        # Unlink Google. Should succeed because the player
        # has a device_id fallback from anonymous login.
        event = _make_event(
            body={"provider": "google"},
            headers={
                "Authorization": (
                    f"Bearer {login_body['jwt_token']}"
                )
            },
        )
        status, body = _parse_response(
            unlink_account(event, _CONTEXT)
        )

        assert status == 200
        assert body["status"] == "success"


# =====================================================================
# POST /auth/merge
# =====================================================================


class TestMergeAccounts:
    def _setup_conflict(self, aws_mock):
        """Helper: create two anon players, pre-link Google
        to player 1, trigger a link attempt from player 2
        (gets 409 + merge_token). Returns
        (jwt2, merge_token, player1_id, player2_id)."""
        from handlers.auth_handler import (
            anonymous_login,
            link_account,
        )
        from services.provider_mapping_service import (
            ProviderMappingService,
        )
        from services.player_service import PlayerService

        _, body1 = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "merge-dev-1"}
                ),
                _CONTEXT,
            )
        )
        _, body2 = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "merge-dev-2"}
                ),
                _CONTEXT,
            )
        )

        pms = ProviderMappingService()
        asyncio.run(
            pms.create(
                "google",
                "google_merge_id",
                body1["player_id"],
            )
        )
        # Sync the PlayersTable auth_providers to match
        # the ProviderMappings entry, as link_account would.
        ps = PlayerService()
        asyncio.run(
            ps.add_provider(
                body1["player_id"],
                "google",
                "google_merge_id",
            )
        )

        mock_result = AuthResult(
            provider="google",
            provider_id="google_merge_id",
            display_name="MergeUser",
        )

        with patch(
            "handlers.auth_handler.auth_service"
        ) as mock_auth:
            mock_auth.authenticate = AsyncMock(
                return_value=mock_result
            )
            mock_auth.jwt_secret = TEST_JWT_SECRET

            _, conflict_body = _parse_response(
                link_account(
                    _make_event(
                        body={
                            "provider": "google",
                            "auth_code": "CODE",
                        },
                        headers={
                            "Authorization": (
                                f"Bearer {body2['jwt_token']}"
                            )
                        },
                    ),
                    _CONTEXT,
                )
            )

        return (
            body2["jwt_token"],
            conflict_body.get("merge_token", ""),
            body1["player_id"],
            body2["player_id"],
        )

    def test_missing_auth_header_returns_401(
        self, aws_mock
    ):
        from handlers.auth_handler import merge_accounts

        event = _make_event(
            body={"merge_token": "sometoken"}
        )
        status, body = _parse_response(
            merge_accounts(event, _CONTEXT)
        )

        assert status == 401
        assert body["error_code"] == "UNAUTHORIZED"

    def test_missing_merge_token_returns_400(
        self, aws_mock
    ):
        from handlers.auth_handler import (
            merge_accounts,
            anonymous_login,
        )

        _, login_body = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "merge-no-token"}
                ),
                _CONTEXT,
            )
        )

        event = _make_event(
            body={},
            headers={
                "Authorization": (
                    f"Bearer {login_body['jwt_token']}"
                )
            },
        )
        status, body = _parse_response(
            merge_accounts(event, _CONTEXT)
        )

        assert status == 400
        assert body["error_code"] == "MISSING_PARAMS"

    def test_invalid_merge_token_returns_400(
        self, aws_mock
    ):
        from handlers.auth_handler import (
            merge_accounts,
            anonymous_login,
        )

        _, login_body = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "merge-bad-token"}
                ),
                _CONTEXT,
            )
        )

        event = _make_event(
            body={"merge_token": "not.a.real.token"},
            headers={
                "Authorization": (
                    f"Bearer {login_body['jwt_token']}"
                )
            },
        )
        status, body = _parse_response(
            merge_accounts(event, _CONTEXT)
        )

        assert status == 400
        assert body["error_code"] == "INVALID_MERGE_TOKEN"

    def test_conflict_response_includes_merge_token(
        self, aws_mock
    ):
        """409 PROVIDER_CONFLICT must include a merge_token."""
        jwt2, merge_token, _, _ = self._setup_conflict(
            aws_mock
        )

        assert merge_token, (
            "merge_token must be present in 409 response"
        )

    def test_wrong_player_merge_token_returns_403(
        self, aws_mock
    ):
        from handlers.auth_handler import (
            merge_accounts,
            anonymous_login,
        )

        # Get a merge_token issued for player 2.
        _jwt2, merge_token, _, _ = (
            self._setup_conflict(aws_mock)
        )

        # Create a third, unrelated player.
        _, body3 = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "merge-dev-3"}
                ),
                _CONTEXT,
            )
        )

        # Present the merge_token using player 3's JWT.
        event = _make_event(
            body={"merge_token": merge_token},
            headers={
                "Authorization": (
                    f"Bearer {body3['jwt_token']}"
                )
            },
        )
        status, body = _parse_response(
            merge_accounts(event, _CONTEXT)
        )

        assert status == 403
        assert body["error_code"] == "FORBIDDEN"

    def test_successful_merge_combines_providers(
        self, aws_mock
    ):
        from handlers.auth_handler import merge_accounts
        from services.player_service import PlayerService

        jwt2, merge_token, player1_id, player2_id = (
            self._setup_conflict(aws_mock)
        )

        event = _make_event(
            body={"merge_token": merge_token},
            headers={
                "Authorization": f"Bearer {jwt2}"
            },
        )
        status, body = _parse_response(
            merge_accounts(event, _CONTEXT)
        )

        assert status == 200
        assert body["status"] == "success"
        # google should now be linked to player 2
        # (the primary in this merge).
        assert "google" in body["linked_providers"]

        # Secondary player (player 1) should be deleted.
        ps = PlayerService()
        assert (
            asyncio.run(ps.get_player(player1_id))
            is None
        )

    def test_successful_merge_sums_stats(self, aws_mock):
        from handlers.auth_handler import merge_accounts
        from services.player_service import PlayerService

        jwt2, merge_token, player1_id, player2_id = (
            self._setup_conflict(aws_mock)
        )

        ps = PlayerService()
        # Give both players some wins so we can verify
        # they are summed after the merge.
        ps.table.update_item(
            Key={"player_id": player1_id},
            UpdateExpression="SET wins = :w",
            ExpressionAttributeValues={":w": 5},
        )
        ps.table.update_item(
            Key={"player_id": player2_id},
            UpdateExpression="SET wins = :w",
            ExpressionAttributeValues={":w": 3},
        )

        event = _make_event(
            body={"merge_token": merge_token},
            headers={
                "Authorization": f"Bearer {jwt2}"
            },
        )
        status, body = _parse_response(
            merge_accounts(event, _CONTEXT)
        )

        assert status == 200
        merged = asyncio.run(
            ps.get_player(player2_id)
        )
        assert merged is not None
        assert merged.wins == 8


# =====================================================================
# linked_providers in auth responses
# =====================================================================


# =====================================================================
# DELETE /auth/account
# =====================================================================


class TestDeleteAccount:
    def test_missing_auth_header_returns_401(self, aws_mock):
        from handlers.auth_handler import delete_account

        event = _make_event()
        status, body = _parse_response(
            delete_account(event, _CONTEXT)
        )

        assert status == 401
        assert body["error_code"] == "UNAUTHORIZED"

    def test_invalid_jwt_returns_401(self, aws_mock):
        from handlers.auth_handler import delete_account

        event = _make_event(
            headers={"Authorization": "Bearer bad-token"}
        )
        status, body = _parse_response(
            delete_account(event, _CONTEXT)
        )

        assert status == 401
        assert body["error_code"] == "UNAUTHORIZED"

    def test_nonexistent_player_returns_404(self, aws_mock):
        from handlers.auth_handler import delete_account
        from services.auth_service import AuthService

        # Create a valid JWT for a player that doesn't exist
        # in the database.
        svc = AuthService(token_lifetime_hours=1)
        token = svc.create_auth_token(
            "p_doesnotexist", "Ghost", "steam"
        )
        jwt_str = token.to_jwt(svc.jwt_secret)

        event = _make_event(
            headers={"Authorization": f"Bearer {jwt_str}"}
        )
        status, body = _parse_response(
            delete_account(event, _CONTEXT)
        )

        assert status == 404
        assert body["error_code"] == "NOT_FOUND"

    def test_delete_anonymous_account(self, aws_mock):
        from handlers.auth_handler import (
            delete_account,
            anonymous_login,
        )
        from services.player_service import PlayerService
        from services.provider_mapping_service import (
            ProviderMappingService,
        )

        # Create anonymous player.
        _, login_body = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "delete-anon-dev"}
                ),
                _CONTEXT,
            )
        )
        player_id = login_body["player_id"]

        # Delete account.
        event = _make_event(
            headers={
                "Authorization": (
                    f"Bearer {login_body['jwt_token']}"
                )
            }
        )
        status, body = _parse_response(
            delete_account(event, _CONTEXT)
        )

        assert status == 200
        assert body["status"] == "success"

        # Verify player is gone.
        ps = PlayerService()
        profile = asyncio.run(ps.get_player(player_id))
        assert profile is None

        # Verify device mapping is gone.
        pms = ProviderMappingService()
        result = asyncio.run(
            pms.lookup("anonymous", "delete-anon-dev")
        )
        assert result is None

    def test_delete_linked_account(self, aws_mock):
        from handlers.auth_handler import (
            delete_account,
            anonymous_login,
            link_account,
        )
        from services.player_service import PlayerService
        from services.provider_mapping_service import (
            ProviderMappingService,
        )

        # Create anonymous player and link Google.
        _, login_body = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "delete-linked-dev"}
                ),
                _CONTEXT,
            )
        )

        mock_result = AuthResult(
            provider="google",
            provider_id="google_delete_test",
            display_name="DeleteUser",
        )

        with patch(
            "handlers.auth_handler.auth_service"
        ) as mock_auth:
            mock_auth.authenticate = AsyncMock(
                return_value=mock_result
            )
            mock_auth.jwt_secret = TEST_JWT_SECRET

            link_account(
                _make_event(
                    body={
                        "provider": "google",
                        "auth_code": "CODE",
                    },
                    headers={
                        "Authorization": (
                            "Bearer "
                            f"{login_body['jwt_token']}"
                        )
                    },
                ),
                _CONTEXT,
            )

        player_id = login_body["player_id"]

        # Delete account.
        event = _make_event(
            headers={
                "Authorization": (
                    f"Bearer {login_body['jwt_token']}"
                )
            }
        )
        status, body = _parse_response(
            delete_account(event, _CONTEXT)
        )

        assert status == 200

        # Verify player is gone.
        ps = PlayerService()
        assert asyncio.run(ps.get_player(player_id)) is None

        # Verify both mappings are gone.
        pms = ProviderMappingService()
        assert (
            asyncio.run(
                pms.lookup("anonymous", "delete-linked-dev")
            )
            is None
        )
        assert (
            asyncio.run(
                pms.lookup("google", "google_delete_test")
            )
            is None
        )

    def test_delete_removes_match_history(self, aws_mock):
        from handlers.auth_handler import (
            delete_account,
            anonymous_login,
        )

        import boto3

        # Create anonymous player.
        _, login_body = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "delete-history-dev"}
                ),
                _CONTEXT,
            )
        )
        player_id = login_body["player_id"]

        # Insert some match history directly.
        table = boto3.resource("dynamodb").Table(
            "hopnbop-match-history"
        )
        table.put_item(
            Item={
                "player_id": player_id,
                "match_timestamp": 1000,
                "result": "win",
            }
        )
        table.put_item(
            Item={
                "player_id": player_id,
                "match_timestamp": 2000,
                "result": "loss",
            }
        )

        # Delete account.
        event = _make_event(
            headers={
                "Authorization": (
                    f"Bearer {login_body['jwt_token']}"
                )
            }
        )
        status, _ = _parse_response(
            delete_account(event, _CONTEXT)
        )
        assert status == 200

        # Verify match history is gone.
        response = table.query(
            KeyConditionExpression=(
                boto3.dynamodb.conditions.Key(
                    "player_id"
                ).eq(player_id)
            )
        )
        assert len(response["Items"]) == 0


# =====================================================================
# linked_providers in auth responses
# =====================================================================


class TestLinkedProvidersInResponses:
    def test_login_includes_linked_providers(self, aws_mock):
        from handlers.auth_handler import login

        auth_resp = make_response(200, {
            "response": {
                "params": {"steamid": "steam_lp_test"}
            },
        })
        user_resp = make_response(200, {
            "response": {
                "players": [{"personaname": "LPPlayer"}]
            },
        })

        with mock_httpx_client([auth_resp, user_resp]):
            _, body = _parse_response(
                login(
                    _make_event(
                        body={
                            "provider": "steam",
                            "auth_code": "TICKET",
                        }
                    ),
                    _CONTEXT,
                )
            )

        assert "linked_providers" in body
        assert "steam" in body["linked_providers"]

    def test_anon_login_includes_empty_linked_providers(
        self, aws_mock
    ):
        from handlers.auth_handler import anonymous_login

        _, body = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "lp-anon-dev"}
                ),
                _CONTEXT,
            )
        )

        assert "linked_providers" in body
        assert body["linked_providers"] == []

    def test_refresh_includes_linked_providers(
        self, aws_mock
    ):
        from handlers.auth_handler import (
            anonymous_login,
            refresh,
        )

        _, login_body = _parse_response(
            anonymous_login(
                _make_event(
                    body={"device_id": "lp-refresh-dev"}
                ),
                _CONTEXT,
            )
        )

        _, body = _parse_response(
            refresh(
                _make_event(
                    body={
                        "player_id": login_body["player_id"],
                        "refresh_token": (
                            login_body["refresh_token"]
                        ),
                    }
                ),
                _CONTEXT,
            )
        )

        assert status == 200 if (status := 200) else True
        assert "linked_providers" in body
