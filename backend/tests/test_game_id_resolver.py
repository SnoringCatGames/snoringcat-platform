"""Tests for utils.game_id_resolver."""

import os
import sys

import pytest


# Reach the src directory for imports.
sys.path.insert(
    0,
    os.path.join(
        os.path.dirname(__file__), "..", "src"
    ),
)

# Establish DEFAULT_GAME_ID before importing.
os.environ.setdefault("DEFAULT_GAME_ID", "hopnbop")

from utils.game_id_resolver import (  # noqa: E402
    GameIdMismatchError,
    GameIdMissingError,
    resolve_game_id,
)


def _event(
    *,
    path_game_id=None,
    header_game_id=None,
    header_key="X-Game-ID",
):
    event = {"pathParameters": {}, "headers": {}}
    if path_game_id is not None:
        event["pathParameters"]["game_id"] = path_game_id
    if header_game_id is not None:
        event["headers"][header_key] = header_game_id
    return event


class TestPrecedence:
    def test_jwt_alone_resolves(self):
        event = _event()
        assert (
            resolve_game_id(
                event, jwt_claims={"game_id": "nextgame"}
            )
            == "nextgame"
        )

    def test_path_used_when_no_jwt(self):
        event = _event(path_game_id="hopnbop")
        assert resolve_game_id(event) == "hopnbop"

    def test_header_used_when_no_jwt_or_path(self):
        event = _event(header_game_id="hopnbop")
        assert resolve_game_id(event) == "hopnbop"

    def test_path_beats_header(self):
        event = _event(
            path_game_id="hopnbop",
            header_game_id="nextgame",
        )
        assert resolve_game_id(event) == "hopnbop"

    def test_default_used_when_nothing_present(self):
        event = _event()
        # DEFAULT_GAME_ID set in the env at module load.
        assert resolve_game_id(event) == "hopnbop"


class TestMismatch:
    def test_jwt_vs_path_mismatch_raises(self):
        event = _event(path_game_id="nextgame")
        with pytest.raises(GameIdMismatchError):
            resolve_game_id(
                event, jwt_claims={"game_id": "hopnbop"}
            )

    def test_jwt_vs_header_mismatch_raises(self):
        event = _event(header_game_id="nextgame")
        with pytest.raises(GameIdMismatchError):
            resolve_game_id(
                event, jwt_claims={"game_id": "hopnbop"}
            )

    def test_jwt_matching_path_is_fine(self):
        event = _event(path_game_id="hopnbop")
        assert (
            resolve_game_id(
                event, jwt_claims={"game_id": "hopnbop"}
            )
            == "hopnbop"
        )


class TestRequire:
    def test_missing_with_require_raises(self):
        event = _event()
        with pytest.raises(GameIdMissingError):
            resolve_game_id(event, require=True)

    def test_missing_without_require_returns_default(self):
        event = _event()
        assert resolve_game_id(event) == "hopnbop"

    def test_with_jwt_require_does_not_raise(self):
        event = _event()
        assert (
            resolve_game_id(
                event,
                jwt_claims={"game_id": "x"},
                require=True,
            )
            == "x"
        )


class TestHeaderCasing:
    @pytest.mark.parametrize(
        "header_key",
        ["X-Game-ID", "x-game-id", "X-Game-Id"],
    )
    def test_case_variants_recognized(self, header_key):
        event = _event(
            header_game_id="hopnbop", header_key=header_key
        )
        assert resolve_game_id(event) == "hopnbop"


class TestMissingFields:
    def test_no_path_parameters_key(self):
        event = {"headers": {}}
        assert resolve_game_id(event) == "hopnbop"

    def test_null_path_parameters(self):
        event = {"pathParameters": None, "headers": {}}
        assert resolve_game_id(event) == "hopnbop"

    def test_null_headers(self):
        event = {"pathParameters": {}, "headers": None}
        assert resolve_game_id(event) == "hopnbop"
