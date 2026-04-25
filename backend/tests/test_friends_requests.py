"""Tests for friend request confirmation flow."""

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
    player_id, display_name="TestPlayer",
    is_anonymous=False, friend_code="ABC123",
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
            "is_anonymous": is_anonymous,
            "friend_code": friend_code,
        }
    )


def _run(coro):
    """Run an async function synchronously."""
    return asyncio.run(coro)


# ===================================================
# Service-level tests
# ===================================================


class TestSendFriendRequest:
    def test_creates_pending_rows(self, aws_mock):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        result = _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )

        assert result["result"] == "request_sent"

        rels = _run(
            svc.list_all_relationships("DEBUG_alice")
        )
        assert len(rels["sent_requests"]) == 1
        assert rels["sent_requests"][0].friend_id == (
            "DEBUG_bob"
        )
        assert len(rels["friends"]) == 0

        rels_bob = _run(
            svc.list_all_relationships("DEBUG_bob")
        )
        assert len(rels_bob["incoming_requests"]) == 1
        assert (
            rels_bob["incoming_requests"][0].friend_id
            == "DEBUG_alice"
        )

    def test_already_friends(self, aws_mock):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        # Send and accept.
        _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )
        _run(
            svc.accept_friend_request(
                "DEBUG_bob", "DEBUG_alice"
            )
        )

        # Try again.
        result = _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )
        assert result["result"] == "already_friends"

    def test_already_pending(self, aws_mock):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )

        result = _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )
        assert result["result"] == "already_pending"

    def test_mutual_request_auto_accepts(
        self, aws_mock
    ):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        # Alice sends to Bob.
        _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )
        # Bob sends to Alice (auto-accept).
        result = _run(
            svc.send_friend_request(
                "DEBUG_bob", "DEBUG_alice"
            )
        )
        assert result["result"] == "auto_accepted"

        # Both should be friends now.
        rels_alice = _run(
            svc.list_all_relationships("DEBUG_alice")
        )
        assert len(rels_alice["friends"]) == 1
        assert len(rels_alice["sent_requests"]) == 0

        rels_bob = _run(
            svc.list_all_relationships("DEBUG_bob")
        )
        assert len(rels_bob["friends"]) == 1
        assert (
            len(rels_bob["incoming_requests"]) == 0
        )

    def test_cannot_add_self(self, aws_mock):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")

        svc = FriendsService()
        with pytest.raises(ValueError):
            _run(
                svc.send_friend_request(
                    "DEBUG_alice", "DEBUG_alice"
                )
            )

    def test_anonymous_cannot_send(self, aws_mock):
        from services.friends_service import (
            FriendsService,
        )

        _create_player(
            "alice", "Alice", True, "AAA"
        )
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        with pytest.raises(ValueError):
            _run(
                svc.send_friend_request(
                    "DEBUG_alice", "DEBUG_bob"
                )
            )


class TestAcceptFriendRequest:
    def test_accept_updates_to_accepted(
        self, aws_mock
    ):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )

        accepted = _run(
            svc.accept_friend_request(
                "DEBUG_bob", "DEBUG_alice"
            )
        )
        assert accepted is True

        rels = _run(
            svc.list_all_relationships("DEBUG_bob")
        )
        assert len(rels["friends"]) == 1
        assert (
            len(rels["incoming_requests"]) == 0
        )

    def test_accept_no_pending_returns_false(
        self, aws_mock
    ):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        accepted = _run(
            svc.accept_friend_request(
                "DEBUG_bob", "DEBUG_alice"
            )
        )
        assert accepted is False


class TestRejectFriendRequest:
    def test_reject_deletes_both_rows(
        self, aws_mock
    ):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )

        rejected = _run(
            svc.reject_friend_request(
                "DEBUG_bob", "DEBUG_alice"
            )
        )
        assert rejected is True

        rels_alice = _run(
            svc.list_all_relationships("DEBUG_alice")
        )
        assert len(rels_alice["sent_requests"]) == 0

        rels_bob = _run(
            svc.list_all_relationships("DEBUG_bob")
        )
        assert (
            len(rels_bob["incoming_requests"]) == 0
        )


class TestCancelFriendRequest:
    def test_cancel_deletes_both_rows(
        self, aws_mock
    ):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )

        cancelled = _run(
            svc.cancel_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )
        assert cancelled is True

        rels_alice = _run(
            svc.list_all_relationships("DEBUG_alice")
        )
        assert len(rels_alice["sent_requests"]) == 0

    def test_cancel_fails_if_not_sender(
        self, aws_mock
    ):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )

        # Bob tries to cancel (he's the receiver).
        cancelled = _run(
            svc.cancel_friend_request(
                "DEBUG_bob", "DEBUG_alice"
            )
        )
        assert cancelled is False


class TestRemoveFriend:
    def test_remove_only_accepted(self, aws_mock):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        # Pending request, not accepted.
        _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )

        # Cannot remove a pending request.
        removed = _run(
            svc.remove_friend(
                "DEBUG_alice", "DEBUG_bob"
            )
        )
        assert removed is False

        # Accept then remove.
        _run(
            svc.accept_friend_request(
                "DEBUG_bob", "DEBUG_alice"
            )
        )
        removed = _run(
            svc.remove_friend(
                "DEBUG_alice", "DEBUG_bob"
            )
        )
        assert removed is True


class TestListAllRelationships:
    def test_separates_by_status(self, aws_mock):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")
        _create_player(
            "carol", "Carol", False, "CCC"
        )

        svc = FriendsService()
        # Alice and Bob are friends.
        _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_bob"
            )
        )
        _run(
            svc.accept_friend_request(
                "DEBUG_bob", "DEBUG_alice"
            )
        )
        # Alice sends request to Carol (pending).
        _run(
            svc.send_friend_request(
                "DEBUG_alice", "DEBUG_carol"
            )
        )

        rels = _run(
            svc.list_all_relationships("DEBUG_alice")
        )
        assert len(rels["friends"]) == 1
        assert len(rels["sent_requests"]) == 1
        assert len(rels["incoming_requests"]) == 0


class TestNotifications:
    def test_returns_new_since_timestamp(
        self, aws_mock
    ):
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        svc = FriendsService()
        # Bob sends request to Alice.
        _run(
            svc.send_friend_request(
                "DEBUG_bob", "DEBUG_alice"
            )
        )

        # Alice checks notifications since epoch.
        notifications = _run(
            svc.get_notifications("DEBUG_alice", 0)
        )
        assert (
            len(notifications["incoming_requests"])
            == 1
        )
        assert (
            notifications["incoming_requests"][0][
                "friend_id"
            ]
            == "DEBUG_bob"
        )


class TestBackwardCompat:
    def test_missing_status_treated_as_accepted(
        self, aws_mock
    ):
        """Legacy rows without status should appear
        as accepted friends."""
        from services.friends_service import (
            FriendsService,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        # Write a legacy row without status field.
        dynamodb = boto3.resource(
            "dynamodb", region_name=TEST_REGION
        )
        table = dynamodb.Table("hopnbop-friends")
        table.put_item(
            Item={
                "player_id": "DEBUG_alice",
                "friend_id": "DEBUG_bob",
                "source": "friend_code",
                "created_at": 1000,
            }
        )

        svc = FriendsService()
        rels = _run(
            svc.list_all_relationships("DEBUG_alice")
        )
        assert len(rels["friends"]) == 1
        assert (
            rels["friends"][0].status == "accepted"
        )


# ===================================================
# Handler-level tests
# ===================================================


class TestAddFriendHandler:
    def test_send_request_via_handler(
        self, aws_mock
    ):
        from handlers.friends_handler import (
            add_friend,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        event = _make_event(
            body={"player_id": "DEBUG_bob"},
            headers=_auth_headers("alice"),
        )
        status, body = _parse_response(
            add_friend(event, _CONTEXT)
        )

        assert status == 200
        assert body["result"] == "request_sent"

    def test_auto_accept_via_handler(
        self, aws_mock
    ):
        from handlers.friends_handler import (
            add_friend,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        # Alice sends to Bob.
        event_a = _make_event(
            body={"player_id": "DEBUG_bob"},
            headers=_auth_headers("alice"),
        )
        add_friend(event_a, _CONTEXT)

        # Bob sends to Alice (auto-accept).
        event_b = _make_event(
            body={"player_id": "DEBUG_alice"},
            headers=_auth_headers("bob"),
        )
        status, body = _parse_response(
            add_friend(event_b, _CONTEXT)
        )

        assert status == 200
        assert body["result"] == "auto_accepted"


class TestAcceptHandler:
    def test_accept_via_handler(self, aws_mock):
        from handlers.friends_handler import (
            add_friend,
            accept_request,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        # Alice sends request.
        add_friend(
            _make_event(
                body={"player_id": "DEBUG_bob"},
                headers=_auth_headers("alice"),
            ),
            _CONTEXT,
        )

        # Bob accepts.
        status, body = _parse_response(
            accept_request(
                _make_event(
                    body={
                        "player_id": "DEBUG_alice"
                    },
                    headers=_auth_headers("bob"),
                ),
                _CONTEXT,
            )
        )

        assert status == 200
        assert body["status"] == "success"


class TestRejectHandler:
    def test_reject_via_handler(self, aws_mock):
        from handlers.friends_handler import (
            add_friend,
            reject_request,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        add_friend(
            _make_event(
                body={"player_id": "DEBUG_bob"},
                headers=_auth_headers("alice"),
            ),
            _CONTEXT,
        )

        status, body = _parse_response(
            reject_request(
                _make_event(
                    body={
                        "player_id": "DEBUG_alice"
                    },
                    headers=_auth_headers("bob"),
                ),
                _CONTEXT,
            )
        )

        assert status == 200
        assert body["status"] == "success"


class TestCancelHandler:
    def test_cancel_via_handler(self, aws_mock):
        from handlers.friends_handler import (
            add_friend,
            cancel_request,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")

        add_friend(
            _make_event(
                body={"player_id": "DEBUG_bob"},
                headers=_auth_headers("alice"),
            ),
            _CONTEXT,
        )

        status, body = _parse_response(
            cancel_request(
                _make_event(
                    body={"player_id": "DEBUG_bob"},
                    headers=_auth_headers("alice"),
                ),
                _CONTEXT,
            )
        )

        assert status == 200
        assert body["status"] == "success"


class TestListFriendsHandler:
    def test_list_separates_sections(
        self, aws_mock
    ):
        from handlers.friends_handler import (
            add_friend,
            accept_request,
            list_friends,
        )

        _create_player("alice", "Alice", False, "AAA")
        _create_player("bob", "Bob", False, "BBB")
        _create_player(
            "carol", "Carol", False, "CCC"
        )

        # Alice and Bob become friends.
        add_friend(
            _make_event(
                body={"player_id": "DEBUG_bob"},
                headers=_auth_headers("alice"),
            ),
            _CONTEXT,
        )
        accept_request(
            _make_event(
                body={"player_id": "DEBUG_alice"},
                headers=_auth_headers("bob"),
            ),
            _CONTEXT,
        )

        # Alice sends request to Carol.
        add_friend(
            _make_event(
                body={"player_id": "DEBUG_carol"},
                headers=_auth_headers("alice"),
            ),
            _CONTEXT,
        )

        # List Alice's relationships.
        status, body = _parse_response(
            list_friends(
                _make_event(
                    headers=_auth_headers("alice"),
                ),
                _CONTEXT,
            )
        )

        assert status == 200
        assert len(body["friends"]) == 1
        assert len(body["sent_requests"]) == 1
        assert len(body["incoming_requests"]) == 0
