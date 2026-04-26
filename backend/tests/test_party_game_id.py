"""Tests for the game_id additions to party_service."""

import asyncio

import pytest


def _run(coro):
    return asyncio.run(coro)


def _service():
    """Re-import after aws_mock has reloaded the module.

    The conftest aws_mock fixture calls importlib.reload on
    party_service, which produces a NEW class object for
    CrossGameInviteError. Tests that need the raised exception
    type must look it up here, post-reload, instead of importing
    at module load time.
    """
    from services.party_service import PartyService

    return PartyService()


def _cross_game_invite_error():
    from services.party_service import CrossGameInviteError

    return CrossGameInviteError


class TestCreate:
    def test_party_records_game_id(self, aws_mock):
        service = _service()
        party = _run(
            service.create_party(
                "p_leader", game_id="hopnbop"
            )
        )
        assert party.game_id == "hopnbop"

    def test_party_persists_game_id(self, aws_mock):
        service = _service()
        party = _run(
            service.create_party(
                "p_leader", game_id="nextgame"
            )
        )
        # Round-trip through DynamoDB.
        loaded = _run(service.get_party(party.party_id))
        assert loaded.game_id == "nextgame"

    def test_legacy_party_has_empty_game_id(self, aws_mock):
        """Calls with no game_id keep the legacy shape."""
        service = _service()
        party = _run(service.create_party("p_leader"))
        assert party.game_id == ""

        loaded = _run(service.get_party(party.party_id))
        assert loaded.game_id == ""


class TestCrossGameInviteRejection:
    def test_same_game_invite_succeeds(self, aws_mock):
        service = _service()
        party = _run(
            service.create_party(
                "p_leader", game_id="hopnbop"
            )
        )
        result = _run(
            service.invite_player(
                party.party_id,
                inviter_id="p_leader",
                invitee_id="p_friend",
                invitee_game_id="hopnbop",
            )
        )
        assert "p_friend" in result.invited

    def test_different_game_invite_raises(self, aws_mock):
        service = _service()
        party = _run(
            service.create_party(
                "p_leader", game_id="hopnbop"
            )
        )
        with pytest.raises(_cross_game_invite_error()):
            _run(
                service.invite_player(
                    party.party_id,
                    inviter_id="p_leader",
                    invitee_id="p_friend",
                    invitee_game_id="nextgame",
                )
            )

    def test_invite_without_game_id_check_legacy(
        self, aws_mock
    ):
        """Old call sites that pass no invitee_game_id work."""
        service = _service()
        party = _run(
            service.create_party(
                "p_leader", game_id="hopnbop"
            )
        )
        # No invitee_game_id supplied → cross-game check skipped.
        result = _run(
            service.invite_player(
                party.party_id,
                inviter_id="p_leader",
                invitee_id="p_friend",
            )
        )
        assert "p_friend" in result.invited

    def test_legacy_party_accepts_any_game(self, aws_mock):
        """A party with no game_id (legacy) accepts anyone."""
        service = _service()
        party = _run(service.create_party("p_leader"))
        result = _run(
            service.invite_player(
                party.party_id,
                inviter_id="p_leader",
                invitee_id="p_friend",
                invitee_game_id="anygame",
            )
        )
        assert "p_friend" in result.invited
