"""Tests for scripts/migrate-from-hopnbop.py.

Exercises the per-table transforms and the idempotency guard
against moto-mocked DynamoDB. Does not run the CLI; calls the
underlying functions directly so we can assert on outputs.
"""

import importlib.util
import os
import sys

import boto3
import pytest


_SCRIPT_PATH = os.path.join(
    os.path.dirname(__file__),
    "..",
    "scripts",
    "migrate-from-hopnbop.py",
)


def _load_script_module():
    """Load the migrate script as a module (its filename has a
    hyphen so a normal `import` won't work)."""
    spec = importlib.util.spec_from_file_location(
        "migrate_script", _SCRIPT_PATH
    )
    mod = importlib.util.module_from_spec(spec)
    # Register in sys.modules BEFORE exec_module so dataclasses
    # can resolve ForwardRef annotations via cls.__module__.
    sys.modules["migrate_script"] = mod
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture
def migrate(aws_mock):
    """The migrate-from-hopnbop module, loaded fresh per test."""
    return _load_script_module()


@pytest.fixture
def dynamodb(aws_mock):
    """A boto3 DynamoDB resource bound to moto.

    Uses the same TEST_REGION the conftest creates tables in;
    using a different region looks like a different DynamoDB
    namespace to moto and the tables come back missing.
    """
    from tests.constants import TEST_REGION

    return boto3.resource(
        "dynamodb", region_name=TEST_REGION
    )


# ---------------------------------------------------------------------
# Source-row builders
# ---------------------------------------------------------------------


def _put_legacy_player(dynamodb, **overrides):
    """Insert a representative legacy hopnbop-players row."""
    item = {
        "player_id": "p_test001",
        "display_name": "Tester",
        "rating": 1500,
        "matches_played": 10,
        "wins": 6,
        "losses": 4,
        "created_at": 1700000000,
        "updated_at": 1700000100,
        "last_active": 1700000200,
        "auth_providers": {"google": "g_id"},
        "provider_display_names": {"google": "GName"},
        "provider_profile_images": {"google": "g.png"},
        "is_anonymous": False,
        "friend_code": "ABC123",
        "first_play_time": 1699000000,
        "last_play_time": 1700000200,
        "rating_partition": "all",
        "total_kills": 42,
        "total_deaths": 7,
        "total_bumps": 100,
        "total_snail_crushes": 5,
        "profile_image_url": "https://example.com/p.png",
    }
    item.update(overrides)
    dynamodb.Table("hopnbop-players").put_item(Item=item)
    return item


def _put_legacy_provider_mapping(dynamodb, **overrides):
    item = {
        "provider_composite": "google#g_id_xyz",
        "player_id": "p_test001",
    }
    item.update(overrides)
    dynamodb.Table(
        "hopnbop-provider-mappings"
    ).put_item(Item=item)
    return item


def _put_legacy_friends(dynamodb, **overrides):
    item = {
        "player_id": "p_a",
        "friend_id": "p_b",
        "status": "accepted",
        "created_at": 1700000000,
    }
    item.update(overrides)
    dynamodb.Table("hopnbop-friends").put_item(Item=item)
    return item


def _put_legacy_match_history(dynamodb, **overrides):
    item = {
        "player_id": "p_test001",
        "match_timestamp": 1700001000,
        "result": "win",
        "rating_delta": 10,
    }
    item.update(overrides)
    dynamodb.Table(
        "hopnbop-match-history"
    ).put_item(Item=item)
    return item


def _put_legacy_leaderboard(dynamodb, **overrides):
    item = {
        "leaderboard_id": "global",
        "score_player": "1500#p_test001",
        "rating": 1500,
        "display_name": "Tester",
    }
    item.update(overrides)
    dynamodb.Table("hopnbop-leaderboard").put_item(Item=item)
    return item


def _put_legacy_party(dynamodb, **overrides):
    item = {
        "party_id": "pty_abc",
        "leader_id": "p_test001",
        "members": ["p_test001"],
        "invited": [],
        "status": "lobby",
        "created_at": 1700000000,
        "expires_at": 1700003600,
    }
    item.update(overrides)
    dynamodb.Table("hopnbop-parties").put_item(Item=item)
    return item


def _put_legacy_active_session(dynamodb, **overrides):
    item = {
        "player_id": "p_test001",
        "session_id": "sess_xyz",
        "fleet_id": "fleet_abc",
        "expires_at": 1700003600,
    }
    item.update(overrides)
    dynamodb.Table(
        "hopnbop-active-sessions"
    ).put_item(Item=item)
    return item


def _put_legacy_fleet_state(dynamodb, **overrides):
    item = {
        "state_key": "main",
        "last_activity_at": 1700000000,
    }
    item.update(overrides)
    dynamodb.Table(
        "hopnbop-fleet-state"
    ).put_item(Item=item)
    return item


# ---------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------


class TestPlayerSplit:
    def test_writes_account_and_profile_rows(
        self, migrate, dynamodb
    ):
        _put_legacy_player(dynamodb)
        plans = migrate._build_transforms("hopnbop")
        plan = next(p for p in plans if p.name == "players")

        result = migrate._run_plan(
            plan, dynamodb, apply=True
        )

        assert result.source_rows_read == 1
        assert result.rows_to_write.get("accounts") == 1
        assert (
            result.rows_to_write.get("game_profiles") == 1
        )

        # accounts row only has cross-game fields.
        accounts = dynamodb.Table("snoringcat-accounts")
        acct = accounts.get_item(
            Key={"player_id": "p_test001"}
        )["Item"]
        assert acct["display_name"] == "Tester"
        assert acct["friend_code"] == "ABC123"
        assert acct["auth_providers"] == {"google": "g_id"}
        # Per-game stats must NOT be on the accounts row.
        assert "rating" not in acct
        assert "total_kills" not in acct

        # game_profiles row has typed per-game fields + game_stats.
        profiles = dynamodb.Table(
            "snoringcat-game-profiles"
        )
        prof = profiles.get_item(
            Key={
                "player_id": "p_test001",
                "game_id": "hopnbop",
            }
        )["Item"]
        assert int(prof["rating"]) == 1500
        assert int(prof["matches_played"]) == 10
        assert int(prof["wins"]) == 6
        assert int(prof["losses"]) == 4
        assert prof["rating_partition"] == "hopnbop#all"
        # first_play_time → first_played rename.
        assert int(prof["first_played"]) == 1699000000
        assert int(prof["last_played"]) == 1700000200
        # game_stats absorbs everything not in the typed lists.
        gs = prof["game_stats"]
        assert int(gs["total_kills"]) == 42
        assert int(gs["total_snail_crushes"]) == 5

    def test_dry_run_writes_nothing(
        self, migrate, dynamodb
    ):
        _put_legacy_player(dynamodb)
        plans = migrate._build_transforms("hopnbop")
        plan = next(p for p in plans if p.name == "players")

        result = migrate._run_plan(
            plan, dynamodb, apply=False
        )

        assert result.source_rows_read == 1
        # Counts populate even in dry-run.
        assert result.rows_to_write.get("accounts") == 1
        # But destinations are empty.
        accounts = dynamodb.Table("snoringcat-accounts")
        assert (
            "Item"
            not in accounts.get_item(
                Key={"player_id": "p_test001"}
            )
        )

    def test_idempotent_re_run(self, migrate, dynamodb):
        _put_legacy_player(dynamodb)
        plans = migrate._build_transforms("hopnbop")
        plan = next(p for p in plans if p.name == "players")

        first = migrate._run_plan(
            plan, dynamodb, apply=True
        )
        assert first.rows_to_write.get("accounts") == 1
        assert (
            first.rows_already_existed.get("accounts", 0)
            == 0
        )

        second = migrate._run_plan(
            plan, dynamodb, apply=True
        )
        # Second run reports the row as already migrated.
        assert (
            second.rows_to_write.get("accounts", 0) == 0
        )
        assert (
            second.rows_already_existed.get("accounts")
            == 1
        )


class TestProviderMappings:
    def test_renamed_unchanged(self, migrate, dynamodb):
        _put_legacy_provider_mapping(dynamodb)
        plans = migrate._build_transforms("hopnbop")
        plan = next(
            p
            for p in plans
            if p.name == "provider_mappings"
        )

        migrate._run_plan(plan, dynamodb, apply=True)

        identities = dynamodb.Table(
            "snoringcat-identities"
        )
        item = identities.get_item(
            Key={"provider_composite": "google#g_id_xyz"}
        )["Item"]
        assert item["player_id"] == "p_test001"


class TestFriends:
    def test_copies_unchanged(self, migrate, dynamodb):
        _put_legacy_friends(dynamodb)
        plans = migrate._build_transforms("hopnbop")
        plan = next(p for p in plans if p.name == "friends")

        migrate._run_plan(plan, dynamodb, apply=True)

        friends = dynamodb.Table("snoringcat-friends")
        item = friends.get_item(
            Key={"player_id": "p_a", "friend_id": "p_b"}
        )["Item"]
        assert item["status"] == "accepted"


class TestMatchHistory:
    def test_adds_game_id_attribute(
        self, migrate, dynamodb
    ):
        _put_legacy_match_history(dynamodb)
        plans = migrate._build_transforms("hopnbop")
        plan = next(
            p for p in plans if p.name == "match_history"
        )

        migrate._run_plan(plan, dynamodb, apply=True)

        match_history = dynamodb.Table(
            "snoringcat-match-history"
        )
        item = match_history.get_item(
            Key={
                "player_id": "p_test001",
                "match_timestamp": 1700001000,
            }
        )["Item"]
        assert item["game_id"] == "hopnbop"
        assert item["result"] == "win"


class TestLeaderboard:
    def test_rewrites_id(self, migrate, dynamodb):
        _put_legacy_leaderboard(dynamodb)
        plans = migrate._build_transforms("hopnbop")
        plan = next(
            p for p in plans if p.name == "leaderboard"
        )

        migrate._run_plan(plan, dynamodb, apply=True)

        leaderboard = dynamodb.Table(
            "snoringcat-leaderboard"
        )
        item = leaderboard.get_item(
            Key={
                "leaderboard_id": "hopnbop#global",
                "score_player": "1500#p_test001",
            }
        )["Item"]
        assert int(item["rating"]) == 1500
        assert item["display_name"] == "Tester"

    def test_does_not_double_prefix(
        self, migrate, dynamodb
    ):
        """A row that already has a # in the id is left alone."""
        _put_legacy_leaderboard(
            dynamodb, leaderboard_id="hopnbop#weekly"
        )
        plans = migrate._build_transforms("hopnbop")
        plan = next(
            p for p in plans if p.name == "leaderboard"
        )

        migrate._run_plan(plan, dynamodb, apply=True)

        leaderboard = dynamodb.Table(
            "snoringcat-leaderboard"
        )
        item = leaderboard.get_item(
            Key={
                "leaderboard_id": "hopnbop#weekly",
                "score_player": "1500#p_test001",
            }
        )["Item"]
        assert item is not None


class TestParties:
    def test_adds_game_id_attribute(
        self, migrate, dynamodb
    ):
        _put_legacy_party(dynamodb)
        plans = migrate._build_transforms("hopnbop")
        plan = next(p for p in plans if p.name == "parties")

        migrate._run_plan(plan, dynamodb, apply=True)

        parties = dynamodb.Table("snoringcat-parties")
        item = parties.get_item(
            Key={"party_id": "pty_abc"}
        )["Item"]
        assert item["game_id"] == "hopnbop"
        assert item["leader_id"] == "p_test001"


class TestActiveSessions:
    def test_adds_game_id_attribute(
        self, migrate, dynamodb
    ):
        _put_legacy_active_session(dynamodb)
        plans = migrate._build_transforms("hopnbop")
        plan = next(
            p
            for p in plans
            if p.name == "active_sessions"
        )

        migrate._run_plan(plan, dynamodb, apply=True)

        sessions = dynamodb.Table(
            "snoringcat-active-sessions"
        )
        item = sessions.get_item(
            Key={"player_id": "p_test001"}
        )["Item"]
        assert item["game_id"] == "hopnbop"


class TestFleetState:
    def test_rewrites_state_key(self, migrate, dynamodb):
        _put_legacy_fleet_state(dynamodb)
        plans = migrate._build_transforms("hopnbop")
        plan = next(
            p for p in plans if p.name == "fleet_state"
        )

        migrate._run_plan(plan, dynamodb, apply=True)

        fleet_state = dynamodb.Table(
            "snoringcat-fleet-state"
        )
        item = fleet_state.get_item(
            Key={"state_key": "game#hopnbop"}
        )["Item"]
        assert int(item["last_activity_at"]) == 1700000000


class TestEndToEnd:
    def test_all_plans_run_clean_with_no_data(
        self, migrate, dynamodb
    ):
        """Empty source tables → no errors, zero rows."""
        plans = migrate._build_transforms("hopnbop")
        for plan in plans:
            result = migrate._run_plan(
                plan, dynamodb, apply=True
            )
            assert result.source_rows_read == 0

    def test_full_migration_with_one_player(
        self, migrate, dynamodb
    ):
        """Sanity: one player + supporting rows migrate cleanly."""
        _put_legacy_player(dynamodb)
        _put_legacy_provider_mapping(dynamodb)
        _put_legacy_friends(dynamodb)
        _put_legacy_match_history(dynamodb)
        _put_legacy_leaderboard(dynamodb)
        _put_legacy_party(dynamodb)
        _put_legacy_active_session(dynamodb)
        _put_legacy_fleet_state(dynamodb)

        plans = migrate._build_transforms("hopnbop")
        for plan in plans:
            migrate._run_plan(plan, dynamodb, apply=True)

        # Spot checks on a few destinations.
        assert dynamodb.Table(
            "snoringcat-accounts"
        ).get_item(
            Key={"player_id": "p_test001"}
        ).get(
            "Item"
        )
        assert dynamodb.Table(
            "snoringcat-game-profiles"
        ).get_item(
            Key={
                "player_id": "p_test001",
                "game_id": "hopnbop",
            }
        ).get(
            "Item"
        )
        assert dynamodb.Table(
            "snoringcat-fleet-state"
        ).get_item(
            Key={"state_key": "game#hopnbop"}
        ).get(
            "Item"
        )

    def test_custom_game_id(self, migrate, dynamodb):
        """Backfill works with any game_id value."""
        _put_legacy_player(dynamodb)
        plans = migrate._build_transforms("nextgame")
        plan = next(p for p in plans if p.name == "players")
        migrate._run_plan(plan, dynamodb, apply=True)

        prof = dynamodb.Table(
            "snoringcat-game-profiles"
        ).get_item(
            Key={
                "player_id": "p_test001",
                "game_id": "nextgame",
            }
        )[
            "Item"
        ]
        assert prof["rating_partition"] == "nextgame#all"
