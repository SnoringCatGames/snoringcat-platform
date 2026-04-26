"""Shared test fixtures for backend tests."""

import os
import sys
import json
import pytest
import boto3
from contextlib import contextmanager
from unittest.mock import AsyncMock, MagicMock, patch
from moto import mock_aws

# Add src/ to path so imports work like they do in Lambda.
sys.path.insert(
    0, os.path.join(os.path.dirname(__file__), "..", "src")
)

from tests.constants import TEST_JWT_SECRET, TEST_REGION


@pytest.fixture(autouse=True)
def _aws_env(monkeypatch):
    """Set required environment variables for all tests."""
    monkeypatch.setenv("AWS_DEFAULT_REGION", TEST_REGION)
    monkeypatch.setenv("AWS_ACCESS_KEY_ID", "testing")
    monkeypatch.setenv("AWS_SECRET_ACCESS_KEY", "testing")
    monkeypatch.setenv("AWS_SECURITY_TOKEN", "testing")
    monkeypatch.setenv("AWS_SESSION_TOKEN", "testing")
    # Legacy table env vars (still consumed by player_service,
    # provider_mapping_service, etc.). Bound to the legacy
    # hopnbop-* names that conftest's _create_dynamodb_tables
    # creates.
    monkeypatch.setenv("PLAYERS_TABLE", "hopnbop-players")
    monkeypatch.setenv(
        "PROVIDER_MAPPINGS_TABLE", "hopnbop-provider-mappings"
    )
    monkeypatch.setenv(
        "MATCH_HISTORY_TABLE", "hopnbop-match-history"
    )
    monkeypatch.setenv("GAME_VERSION", "0.1.0")
    monkeypatch.setenv("POWERTOOLS_TRACE_DISABLED", "1")
    monkeypatch.setenv("POWERTOOLS_METRICS_NAMESPACE", "test")
    monkeypatch.setenv(
        "SETTINGS_TABLE", "hopnbop-settings"
    )
    monkeypatch.setenv(
        "LEADERBOARD_TABLE", "hopnbop-leaderboard"
    )
    monkeypatch.setenv(
        "CONSENT_AUDIT_TABLE", "hopnbop-consent-audit"
    )
    monkeypatch.setenv(
        "FRIENDS_TABLE", "hopnbop-friends"
    )
    monkeypatch.setenv(
        "PARTIES_TABLE", "hopnbop-parties"
    )
    monkeypatch.setenv(
        "FLEET_STATE_TABLE", "hopnbop-fleet-state"
    )
    # Empty FLEET_ID disables real GameLift calls in tests.
    monkeypatch.setenv("FLEET_ID", "")
    monkeypatch.setenv("GAMELIFT_LOCATION", TEST_REGION)
    # New platform env vars consumed by the new services
    # (account_service, profile_service, game_config_service,
    # presence_service). These bind to dedicated tables created
    # alongside the legacy ones below.
    monkeypatch.setenv(
        "ACCOUNTS_TABLE", "snoringcat-accounts"
    )
    monkeypatch.setenv(
        "IDENTITIES_TABLE", "snoringcat-identities"
    )
    monkeypatch.setenv(
        "GAME_PROFILES_TABLE", "snoringcat-game-profiles"
    )
    monkeypatch.setenv(
        "GAMES_TABLE", "snoringcat-games"
    )
    monkeypatch.setenv(
        "PRESENCE_TABLE", "snoringcat-presence"
    )
    monkeypatch.setenv("DEFAULT_GAME_ID", "hopnbop")


@pytest.fixture
def aws_mock():
    """Provide a moto mock_aws context with DynamoDB tables
    and Secrets Manager secrets pre-created."""
    with mock_aws():
        _create_dynamodb_tables()
        _create_secrets()

        # Clear secrets_service cache so it picks up the
        # mocked Secrets Manager client.
        from services import secrets_service

        secrets_service.clear_cache()
        secrets_service._client = None

        # Re-initialize module-level service instances in
        # handlers so they use mocked DynamoDB/Secrets.
        _reinit_handler_services()

        yield

        # Clean up cached clients after the test so they
        # do not leak into subsequent tests.
        secrets_service.clear_cache()
        secrets_service._client = None


# =================================================================
# httpx mock helper
# =================================================================


@contextmanager
def mock_httpx_client(responses):
    """Mock httpx.AsyncClient for provider auth tests.

    Args:
        responses: A single MagicMock response, or a list of
            them. When a list is given, successive calls to
            client.get / client.post return successive items.

    Yields:
        The mock client instance (for additional assertions).

    Usage::

        resp = MagicMock(status_code=200)
        resp.json.return_value = {"id": "123"}
        with mock_httpx_client(resp) as client:
            result = _run(service._auth_epic("token"))
        client.get.assert_called_once()
    """
    mock_client = AsyncMock()
    mock_client.__aenter__ = AsyncMock(
        return_value=mock_client
    )
    mock_client.__aexit__ = AsyncMock(return_value=False)

    if isinstance(responses, list):
        # Mixed get/post: assign side_effect to both so the
        # caller doesn't have to know which HTTP method is used
        # internally. Each call pops from the same list.
        call_iter = iter(responses)

        async def _next_response(*args, **kwargs):
            return next(call_iter)

        mock_client.get = AsyncMock(side_effect=_next_response)
        mock_client.post = AsyncMock(
            side_effect=_next_response
        )
    else:
        mock_client.get = AsyncMock(return_value=responses)
        mock_client.post = AsyncMock(return_value=responses)

    with patch(
        "services.auth_service.httpx.AsyncClient"
    ) as mock_cls:
        mock_cls.return_value = mock_client
        yield mock_client


def make_response(status_code, json_body=None):
    """Build a fake httpx response."""
    resp = MagicMock()
    resp.status_code = status_code
    if json_body is not None:
        resp.json.return_value = json_body
    return resp


# =================================================================
# Internal helpers
# =================================================================


def _reinit_handler_services():
    """Re-create service instances inside the moto mock
    so they use the mocked DynamoDB and Secrets Manager."""
    import importlib

    # Reload service modules so they get fresh boto3 clients.
    from services import player_service as ps_mod
    from services import provider_mapping_service as pm_mod

    importlib.reload(ps_mod)
    importlib.reload(pm_mod)

    # Patch the handler module-level service instances.
    try:
        from handlers import auth_handler
        from services import profile_service as prof_mod

        importlib.reload(prof_mod)

        auth_handler.player_service = ps_mod.PlayerService()
        auth_handler.profile_service = (
            prof_mod.ProfileService()
        )
        auth_handler.provider_mapping_service = (
            pm_mod.ProviderMappingService()
        )
    except ImportError:
        pass

    try:
        from handlers import matchmaking_handler

        matchmaking_handler.player_service = (
            ps_mod.PlayerService()
        )
    except ImportError:
        pass

    try:
        from services import match_service as ms_mod
        from handlers import match_handler

        importlib.reload(ms_mod)
        match_handler.match_service = ms_mod.MatchService()
        match_handler.player_service = ps_mod.PlayerService()
    except ImportError:
        pass

    try:
        from services import settings_service as ss_mod

        importlib.reload(ss_mod)
    except ImportError:
        pass

    try:
        from services import leaderboard_service as ls_mod

        importlib.reload(ls_mod)
    except ImportError:
        pass

    try:
        from handlers import player_handler

        player_handler.player_service = (
            ps_mod.PlayerService()
        )
        player_handler.match_service = ms_mod.MatchService()
        try:
            player_handler.settings_service = (
                ss_mod.SettingsService()
            )
        except NameError:
            pass
    except (ImportError, AttributeError):
        pass

    try:
        from services import friends_service as fs_mod
        from handlers import friends_handler

        importlib.reload(fs_mod)
        friends_handler.friends_service = (
            fs_mod.FriendsService()
        )
        friends_handler.player_service = (
            ps_mod.PlayerService()
        )
    except ImportError:
        fs_mod = None

    try:
        from services import party_service as pty_mod
        from handlers import party_handler

        importlib.reload(pty_mod)
        party_handler.party_service = (
            pty_mod.PartyService()
        )
        if fs_mod is not None:
            party_handler.friends_service = (
                fs_mod.FriendsService()
            )
    except ImportError:
        pass


def _create_dynamodb_tables():
    """Create all DynamoDB tables used by the backend."""
    dynamodb = boto3.resource(
        "dynamodb", region_name=TEST_REGION
    )

    dynamodb.create_table(
        TableName="hopnbop-players",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"}
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "player_id",
                "AttributeType": "S",
            },
            {
                "AttributeName": "rating_partition",
                "AttributeType": "S",
            },
            {
                "AttributeName": "rating",
                "AttributeType": "N",
            },
            {
                "AttributeName": "friend_code",
                "AttributeType": "S",
            },
        ],
        GlobalSecondaryIndexes=[
            {
                "IndexName": "rating-index",
                "KeySchema": [
                    {
                        "AttributeName": "rating_partition",
                        "KeyType": "HASH",
                    },
                    {
                        "AttributeName": "rating",
                        "KeyType": "RANGE",
                    },
                ],
                "Projection": {
                    "ProjectionType": "INCLUDE",
                    "NonKeyAttributes": [
                        "display_name",
                        "matches_played",
                        "wins",
                        "losses",
                    ],
                },
            },
            {
                "IndexName": "friend-code-index",
                "KeySchema": [
                    {
                        "AttributeName": "friend_code",
                        "KeyType": "HASH",
                    },
                ],
                "Projection": {
                    "ProjectionType": "KEYS_ONLY",
                },
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="hopnbop-provider-mappings",
        KeySchema=[
            {
                "AttributeName": "provider_composite",
                "KeyType": "HASH",
            }
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "provider_composite",
                "AttributeType": "S",
            }
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="hopnbop-match-history",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"},
            {
                "AttributeName": "match_timestamp",
                "KeyType": "RANGE",
            },
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "player_id",
                "AttributeType": "S",
            },
            {
                "AttributeName": "match_timestamp",
                "AttributeType": "N",
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="hopnbop-settings",
        KeySchema=[
            {
                "AttributeName": "player_id",
                "KeyType": "HASH",
            }
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "player_id",
                "AttributeType": "S",
            }
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="hopnbop-friends",
        KeySchema=[
            {
                "AttributeName": "player_id",
                "KeyType": "HASH",
            },
            {
                "AttributeName": "friend_id",
                "KeyType": "RANGE",
            },
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "player_id",
                "AttributeType": "S",
            },
            {
                "AttributeName": "friend_id",
                "AttributeType": "S",
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="hopnbop-parties",
        KeySchema=[
            {
                "AttributeName": "party_id",
                "KeyType": "HASH",
            },
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "party_id",
                "AttributeType": "S",
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="hopnbop-leaderboard",
        KeySchema=[
            {
                "AttributeName": "leaderboard_id",
                "KeyType": "HASH",
            },
            {
                "AttributeName": "score_player",
                "KeyType": "RANGE",
            },
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "leaderboard_id",
                "AttributeType": "S",
            },
            {
                "AttributeName": "score_player",
                "AttributeType": "S",
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="hopnbop-fleet-state",
        KeySchema=[
            {
                "AttributeName": "state_key",
                "KeyType": "HASH",
            }
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "state_key",
                "AttributeType": "S",
            }
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    # ----- New platform tables -----
    # These match the schemas in template.yaml. The legacy
    # hopnbop-* tables above continue to coexist so existing
    # tests don't break during the migration window.

    dynamodb.create_table(
        TableName="snoringcat-accounts",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"},
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "player_id",
                "AttributeType": "S",
            },
            {
                "AttributeName": "friend_code",
                "AttributeType": "S",
            },
        ],
        GlobalSecondaryIndexes=[
            {
                "IndexName": "friend-code-index",
                "KeySchema": [
                    {
                        "AttributeName": "friend_code",
                        "KeyType": "HASH",
                    },
                ],
                "Projection": {
                    "ProjectionType": "KEYS_ONLY",
                },
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="snoringcat-identities",
        KeySchema=[
            {
                "AttributeName": "provider_composite",
                "KeyType": "HASH",
            },
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "provider_composite",
                "AttributeType": "S",
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="snoringcat-game-profiles",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"},
            {"AttributeName": "game_id", "KeyType": "RANGE"},
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "player_id",
                "AttributeType": "S",
            },
            {
                "AttributeName": "game_id",
                "AttributeType": "S",
            },
            {
                "AttributeName": "rating_partition",
                "AttributeType": "S",
            },
            {
                "AttributeName": "rating",
                "AttributeType": "N",
            },
        ],
        GlobalSecondaryIndexes=[
            {
                "IndexName": "rating-index",
                "KeySchema": [
                    {
                        "AttributeName": "rating_partition",
                        "KeyType": "HASH",
                    },
                    {
                        "AttributeName": "rating",
                        "KeyType": "RANGE",
                    },
                ],
                "Projection": {
                    "ProjectionType": "INCLUDE",
                    "NonKeyAttributes": [
                        "display_name",
                        "matches_played",
                        "wins",
                        "losses",
                        "game_id",
                    ],
                },
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="snoringcat-games",
        KeySchema=[
            {"AttributeName": "game_id", "KeyType": "HASH"},
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "game_id",
                "AttributeType": "S",
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="snoringcat-presence",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"},
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "player_id",
                "AttributeType": "S",
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    # Per-game new tables (mirrors of the legacy hopnbop-* per-game
    # tables; created so the migration script tests can write to
    # them, and so eventual handler refactors can target the new
    # names).
    dynamodb.create_table(
        TableName="snoringcat-friends",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"},
            {"AttributeName": "friend_id", "KeyType": "RANGE"},
        ],
        AttributeDefinitions=[
            {"AttributeName": "player_id", "AttributeType": "S"},
            {"AttributeName": "friend_id", "AttributeType": "S"},
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="snoringcat-match-history",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"},
            {
                "AttributeName": "match_timestamp",
                "KeyType": "RANGE",
            },
        ],
        AttributeDefinitions=[
            {"AttributeName": "player_id", "AttributeType": "S"},
            {
                "AttributeName": "match_timestamp",
                "AttributeType": "N",
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="snoringcat-leaderboard",
        KeySchema=[
            {
                "AttributeName": "leaderboard_id",
                "KeyType": "HASH",
            },
            {
                "AttributeName": "score_player",
                "KeyType": "RANGE",
            },
        ],
        AttributeDefinitions=[
            {
                "AttributeName": "leaderboard_id",
                "AttributeType": "S",
            },
            {
                "AttributeName": "score_player",
                "AttributeType": "S",
            },
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="snoringcat-parties",
        KeySchema=[
            {"AttributeName": "party_id", "KeyType": "HASH"},
        ],
        AttributeDefinitions=[
            {"AttributeName": "party_id", "AttributeType": "S"},
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="snoringcat-active-sessions",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"},
        ],
        AttributeDefinitions=[
            {"AttributeName": "player_id", "AttributeType": "S"},
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="snoringcat-fleet-state",
        KeySchema=[
            {"AttributeName": "state_key", "KeyType": "HASH"},
        ],
        AttributeDefinitions=[
            {"AttributeName": "state_key", "AttributeType": "S"},
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="snoringcat-settings",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"},
        ],
        AttributeDefinitions=[
            {"AttributeName": "player_id", "AttributeType": "S"},
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="snoringcat-consent-audit",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"},
        ],
        AttributeDefinitions=[
            {"AttributeName": "player_id", "AttributeType": "S"},
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    # Legacy hopnbop-* tables that the migration reads but conftest
    # didn't yet create (active-sessions, consent-audit). Without
    # these, the migration script bails on ResourceNotFoundException.
    dynamodb.create_table(
        TableName="hopnbop-active-sessions",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"},
        ],
        AttributeDefinitions=[
            {"AttributeName": "player_id", "AttributeType": "S"},
        ],
        BillingMode="PAY_PER_REQUEST",
    )

    dynamodb.create_table(
        TableName="hopnbop-consent-audit",
        KeySchema=[
            {"AttributeName": "player_id", "KeyType": "HASH"},
        ],
        AttributeDefinitions=[
            {"AttributeName": "player_id", "AttributeType": "S"},
        ],
        BillingMode="PAY_PER_REQUEST",
    )


def _create_secrets():
    """Create Secrets Manager secrets used by the backend."""
    client = boto3.client(
        "secretsmanager", region_name=TEST_REGION
    )

    client.create_secret(
        Name="hopnbop/jwt-signing-key",
        SecretString=TEST_JWT_SECRET,
    )

    client.create_secret(
        Name="hopnbop/server-api-key",
        SecretString="test-server-api-key",
    )

    for provider in [
        "steam",
        "epic",
        "google",
        "facebook",
        "apple",
    ]:
        client.create_secret(
            Name=f"hopnbop/oauth/{provider}",
            SecretString=json.dumps(
                {
                    "client_id": f"test-{provider}-client-id",
                    "client_secret": f"test-{provider}-secret",
                    "api_key": f"test-{provider}-api-key",
                    "team_id": "TEAM123",
                    "key_id": "KEY123",
                    "private_key": "",
                }
            ),
        )
