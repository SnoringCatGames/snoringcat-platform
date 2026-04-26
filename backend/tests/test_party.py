"""Tests for party system (service and handler)."""

import json
import asyncio

import pytest
import boto3

from tests.constants import TEST_REGION


class _FakeLambdaContext:
    """Minimal Lambda context for aws_lambda_powertools."""

    function_name = "test-function"
    memory_limit_in_mb = 512
    invoked_function_arn = (
        "arn:aws:lambda:us-east-1:123456789"
        ":function:test"
    )
    aws_request_id = "test-request-id"


_CONTEXT = _FakeLambdaContext()


def _make_event(
    body=None, headers=None, query_params=None
):
    """Build a minimal Lambda API Gateway event."""
    event = {
        "body": json.dumps(body) if body else "{}",
        "headers": headers or {},
    }
    if query_params:
        event["queryStringParameters"] = query_params
    return event


def _parse_response(response):
    """Parse status code and body."""
    return (
        response["statusCode"],
        json.loads(response["body"]),
    )


def _auth_headers(player_id):
    """Build debug auth headers."""
    return {
        "Authorization": f"Bearer DEBUG_{player_id}"
    }


def _create_player(
    player_id,
    display_name="TestPlayer",
    friend_code="ABC123",
):
    """Create a player in the test table."""
    dynamodb = boto3.resource(
        "dynamodb", region_name=TEST_REGION
    )
    table = dynamodb.Table("hopnbop-players")
    table.put_item(
        Item={
            "player_id": f"DEBUG_{player_id}",
            "display_name": display_name,
            "is_anonymous": False,
            "friend_code": friend_code,
        }
    )


def _create_friendship(player_a, player_b):
    """Create a mutual friend relationship."""
    dynamodb = boto3.resource(
        "dynamodb", region_name=TEST_REGION
    )
    table = dynamodb.Table("hopnbop-friends")
    for a, b in [(player_a, player_b),
                 (player_b, player_a)]:
        table.put_item(
            Item={
                "player_id": f"DEBUG_{a}",
                "friend_id": f"DEBUG_{b}",
                "status": "accepted",
            }
        )


def _run(coro):
    """Run an async function synchronously."""
    return asyncio.run(coro)


# ===================================================
# Service-level tests
# ===================================================


class TestCreateParty:
    def test_creates_party(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )

        assert party.leader_id == "DEBUG_alice"
        assert party.members == ["DEBUG_alice"]
        assert party.invited == []
        assert party.status == "lobby"
        assert party.party_id.startswith("pty_")

    def test_get_party_for_player(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        found = _run(
            svc.get_party_for_player("DEBUG_alice")
        )

        assert found is not None
        assert found.party_id == party.party_id


class TestInvitePlayer:
    def test_invite_adds_to_list(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        updated = _run(
            svc.invite_player(
                party.party_id,
                "DEBUG_alice",
                "DEBUG_bob",
            )
        )

        assert "DEBUG_bob" in updated.invited
        assert "DEBUG_bob" not in updated.members

    def test_non_leader_cannot_invite(
        self, aws_mock
    ):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        # Add bob as a member first.
        party.members.append("DEBUG_bob")
        svc._update_party(party)

        with pytest.raises(PermissionError):
            _run(
                svc.invite_player(
                    party.party_id,
                    "DEBUG_bob",
                    "DEBUG_charlie",
                )
            )

    def test_already_member_fails(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )

        with pytest.raises(ValueError, match="Already a member"):
            _run(
                svc.invite_player(
                    party.party_id,
                    "DEBUG_alice",
                    "DEBUG_alice",
                )
            )

    def test_already_invited_fails(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        _run(
            svc.invite_player(
                party.party_id,
                "DEBUG_alice",
                "DEBUG_bob",
            )
        )

        with pytest.raises(ValueError, match="Already invited"):
            _run(
                svc.invite_player(
                    party.party_id,
                    "DEBUG_alice",
                    "DEBUG_bob",
                )
            )

    def test_full_party_fails(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        # Fill to 4: 1 member + 3 invited.
        for name in ["bob", "charlie", "dave"]:
            _run(
                svc.invite_player(
                    party.party_id,
                    "DEBUG_alice",
                    f"DEBUG_{name}",
                )
            )

        with pytest.raises(ValueError, match="full"):
            _run(
                svc.invite_player(
                    party.party_id,
                    "DEBUG_alice",
                    "DEBUG_eve",
                )
            )


class TestJoinParty:
    def test_join_moves_to_members(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        _run(
            svc.invite_player(
                party.party_id,
                "DEBUG_alice",
                "DEBUG_bob",
            )
        )
        updated = _run(
            svc.join_party(
                party.party_id, "DEBUG_bob"
            )
        )

        assert "DEBUG_bob" in updated.members
        assert "DEBUG_bob" not in updated.invited

    def test_join_without_invite_fails(
        self, aws_mock
    ):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )

        with pytest.raises(ValueError, match="No pending invite"):
            _run(
                svc.join_party(
                    party.party_id, "DEBUG_bob"
                )
            )


class TestLeaveParty:
    def test_member_leaves(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        _run(
            svc.invite_player(
                party.party_id,
                "DEBUG_alice",
                "DEBUG_bob",
            )
        )
        _run(
            svc.join_party(
                party.party_id, "DEBUG_bob"
            )
        )
        updated = _run(
            svc.leave_party(
                party.party_id, "DEBUG_bob"
            )
        )

        assert updated is not None
        assert "DEBUG_bob" not in updated.members
        assert "DEBUG_alice" in updated.members

    def test_leader_leaves_disbands(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        result = _run(
            svc.leave_party(
                party.party_id, "DEBUG_alice"
            )
        )

        assert result is None
        # Party should be deleted.
        found = _run(
            svc.get_party(party.party_id)
        )
        assert found is None


class TestKickPlayer:
    def test_kick_removes_member(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        _run(
            svc.invite_player(
                party.party_id,
                "DEBUG_alice",
                "DEBUG_bob",
            )
        )
        _run(
            svc.join_party(
                party.party_id, "DEBUG_bob"
            )
        )
        updated = _run(
            svc.kick_player(
                party.party_id,
                "DEBUG_alice",
                "DEBUG_bob",
            )
        )

        assert "DEBUG_bob" not in updated.members
        assert "DEBUG_alice" in updated.members

    def test_non_leader_cannot_kick(
        self, aws_mock
    ):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        _run(
            svc.invite_player(
                party.party_id,
                "DEBUG_alice",
                "DEBUG_bob",
            )
        )
        _run(
            svc.join_party(
                party.party_id, "DEBUG_bob"
            )
        )

        with pytest.raises(PermissionError):
            _run(
                svc.kick_player(
                    party.party_id,
                    "DEBUG_bob",
                    "DEBUG_alice",
                )
            )

    def test_kick_non_member_fails(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )

        with pytest.raises(ValueError, match="not a member"):
            _run(
                svc.kick_player(
                    party.party_id,
                    "DEBUG_alice",
                    "DEBUG_bob",
                )
            )

    def test_kick_self_fails(self, aws_mock):
        from services.party_service import (
            PartyService,
        )

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )

        with pytest.raises(ValueError, match="Cannot kick yourself"):
            _run(
                svc.kick_player(
                    party.party_id,
                    "DEBUG_alice",
                    "DEBUG_alice",
                )
            )


# ===================================================
# Handler-level tests
# ===================================================


class TestCreatePartyHandler:
    def test_creates_party(self, aws_mock):
        from handlers import party_handler

        _create_player("alice", "Alice", "AAA")
        event = _make_event(
            headers=_auth_headers("alice")
        )
        status, body = _parse_response(
            party_handler.create_party(
                event, _CONTEXT
            )
        )

        assert status == 200
        assert body["status"] == "success"
        assert body["party"]["leader_id"] == (
            "DEBUG_alice"
        )

    def test_returns_existing_party(self, aws_mock):
        from handlers import party_handler

        _create_player("alice", "Alice", "AAA")
        event = _make_event(
            headers=_auth_headers("alice")
        )
        _, body1 = _parse_response(
            party_handler.create_party(
                event, _CONTEXT
            )
        )
        _, body2 = _parse_response(
            party_handler.create_party(
                event, _CONTEXT
            )
        )

        assert (
            body1["party"]["party_id"]
            == body2["party"]["party_id"]
        )


class TestInviteHandler:
    def test_requires_friendship(self, aws_mock):
        from handlers import party_handler

        _create_player("alice", "Alice", "AAA")
        _create_player("bob", "Bob", "BBB")

        # Create party first.
        create_event = _make_event(
            headers=_auth_headers("alice")
        )
        _, create_body = _parse_response(
            party_handler.create_party(
                create_event, _CONTEXT
            )
        )
        party_id = create_body["party"]["party_id"]

        # Try to invite without friendship.
        invite_event = _make_event(
            body={
                "party_id": party_id,
                "player_id": "DEBUG_bob",
            },
            headers=_auth_headers("alice"),
        )
        status, body = _parse_response(
            party_handler.invite_to_party(
                invite_event, _CONTEXT
            )
        )

        assert status == 400
        assert body["error_code"] == "NOT_FRIENDS"

    def test_invite_with_friend(self, aws_mock):
        from handlers import party_handler

        _create_player("alice", "Alice", "AAA")
        _create_player("bob", "Bob", "BBB")
        _create_friendship("alice", "bob")

        # Create party.
        create_event = _make_event(
            headers=_auth_headers("alice")
        )
        _, create_body = _parse_response(
            party_handler.create_party(
                create_event, _CONTEXT
            )
        )
        party_id = create_body["party"]["party_id"]

        # Invite friend.
        invite_event = _make_event(
            body={
                "party_id": party_id,
                "player_id": "DEBUG_bob",
            },
            headers=_auth_headers("alice"),
        )
        status, body = _parse_response(
            party_handler.invite_to_party(
                invite_event, _CONTEXT
            )
        )

        assert status == 200
        assert "DEBUG_bob" in body["party"]["invited"]

    def test_invite_rejected_when_invitee_in_other_game(
        self, aws_mock,
    ):
        """Cross-game invites: handler reads invitee's presence,
        passes invitee_game_id to the service, returns
        CROSS_GAME_INVITE 400 when the games don't match.
        """
        from handlers import party_handler
        from services.party_service import PartyService

        _create_player("alice", "Alice", "AAA")
        _create_player("bob", "Bob", "BBB")
        _create_friendship("alice", "bob")

        # Bypass create_party (DEBUG token has empty game_id) and
        # construct a party with an explicit game_id directly.
        party_svc = PartyService()
        party = _run(
            party_svc.create_party(
                "DEBUG_alice", game_id="hopnbop"
            )
        )
        # Reload module-level service so the handler reads the
        # party we just wrote.
        party_handler.party_service = party_svc

        # Bob's presence says he's in a different game.
        dynamodb = boto3.resource(
            "dynamodb", region_name=TEST_REGION
        )
        presence_table = dynamodb.Table("snoringcat-presence")
        # Far-future TTL so the row is fresh.
        presence_table.put_item(
            Item={
                "player_id": "DEBUG_bob",
                "game_id": "nextgame",
                "status": "online",
                "rich_presence": "",
                "session_id": "",
                "updated_at": 9999999999,
                "ttl": 9999999999,
            }
        )

        # Alice tries to invite Bob.
        invite_event = _make_event(
            body={
                "party_id": party.party_id,
                "player_id": "DEBUG_bob",
            },
            headers=_auth_headers("alice"),
        )
        status, body = _parse_response(
            party_handler.invite_to_party(
                invite_event, _CONTEXT
            )
        )

        assert status == 400, (
            f"expected 400, got {status} body={body}"
        )
        assert body["error_code"] == "CROSS_GAME_INVITE"
        assert "nextgame" in body["message"]
        assert "hopnbop" in body["message"]


class TestKickHandler:
    def test_kick_success(self, aws_mock):
        from handlers import party_handler
        from services.party_service import (
            PartyService,
        )

        _create_player("alice", "Alice", "AAA")
        _create_player("bob", "Bob", "BBB")

        # Create party and add bob as member.
        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        _run(
            svc.invite_player(
                party.party_id,
                "DEBUG_alice",
                "DEBUG_bob",
            )
        )
        _run(
            svc.join_party(
                party.party_id, "DEBUG_bob"
            )
        )

        event = _make_event(
            body={
                "party_id": party.party_id,
                "player_id": "DEBUG_bob",
            },
            headers=_auth_headers("alice"),
        )
        status, body = _parse_response(
            party_handler.kick_from_party(
                event, _CONTEXT
            )
        )

        assert status == 200
        assert "DEBUG_bob" not in (
            body["party"]["members"]
        )

    def test_kick_not_leader(self, aws_mock):
        from handlers import party_handler
        from services.party_service import (
            PartyService,
        )

        _create_player("alice", "Alice", "AAA")
        _create_player("bob", "Bob", "BBB")

        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        _run(
            svc.invite_player(
                party.party_id,
                "DEBUG_alice",
                "DEBUG_bob",
            )
        )
        _run(
            svc.join_party(
                party.party_id, "DEBUG_bob"
            )
        )

        event = _make_event(
            body={
                "party_id": party.party_id,
                "player_id": "DEBUG_alice",
            },
            headers=_auth_headers("bob"),
        )
        status, body = _parse_response(
            party_handler.kick_from_party(
                event, _CONTEXT
            )
        )

        assert status == 403
        assert body["error_code"] == "NOT_LEADER"


class TestGetPartyStatus:
    def test_no_party_returns_null(self, aws_mock):
        from handlers import party_handler

        _create_player("alice", "Alice", "AAA")
        event = _make_event(
            headers=_auth_headers("alice")
        )
        status, body = _parse_response(
            party_handler.get_party_status(
                event, _CONTEXT
            )
        )

        assert status == 200
        assert body["party"] is None
        assert body["pending_invites"] == []

    def test_pending_invites_include_display_name(
        self, aws_mock
    ):
        from handlers import party_handler
        from services.party_service import (
            PartyService,
        )

        _create_player("alice", "Alice", "AAA")
        _create_player("bob", "Bob", "BBB")

        # Create party as alice and invite bob.
        svc = PartyService()
        party = _run(
            svc.create_party("DEBUG_alice")
        )
        _run(
            svc.invite_player(
                party.party_id,
                "DEBUG_alice",
                "DEBUG_bob",
            )
        )

        # Check bob's status.
        event = _make_event(
            headers=_auth_headers("bob")
        )
        status, body = _parse_response(
            party_handler.get_party_status(
                event, _CONTEXT
            )
        )

        assert status == 200
        assert body["party"] is None
        assert len(body["pending_invites"]) == 1
        invite = body["pending_invites"][0]
        assert invite["party_id"] == party.party_id
        assert invite["leader_display_name"] == (
            "Alice"
        )
        assert invite["member_count"] == 1
