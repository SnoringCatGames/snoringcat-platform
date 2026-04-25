"""Tests for telemetry_handler.py - crash report ingestion."""

import json
import os

import pytest

# Set required env vars before importing handler.
os.environ.setdefault(
    "POWERTOOLS_METRICS_NAMESPACE", "HopNBop"
)
os.environ.setdefault("POWERTOOLS_TRACE_DISABLED", "true")

from handlers.telemetry_handler import handle_crash_report


class _FakeLambdaContext:
    """Minimal Lambda context for aws_lambda_powertools."""

    function_name = "test-function"
    memory_limit_in_mb = 256
    invoked_function_arn = (
        "arn:aws:lambda:us-east-1:123456789:function:test"
    )
    aws_request_id = "test-request-id"


_CONTEXT = _FakeLambdaContext()


def _make_event(body=None):
    """Build a minimal Lambda API Gateway event."""
    return {
        "body": json.dumps(body) if body else None,
        "headers": {},
    }


def _parse_response(response):
    """Parse status code and body from Lambda response."""
    return (
        response["statusCode"],
        json.loads(response["body"]),
    )


class TestHandleCrashReport:
    """Tests for POST /telemetry/crash."""

    def test_valid_crash_report_returns_200(self):
        event = _make_event(
            {
                "error_message": "FATAL ERROR: null ref",
                "is_fatal": True,
                "game_version": "0.5.1",
                "operating_system": "Windows 11",
                "player_id": "p_abc123",
                "is_server": False,
                "server_frame_index": 54321,
                "render_fps": 58.2,
                "physics_fps": 60.1,
                "network_ping_ms": 45.3,
            }
        )
        status, body = _parse_response(
            handle_crash_report(event, _CONTEXT)
        )
        assert status == 200
        assert body["status"] == "success"

    def test_missing_error_message_returns_400(self):
        event = _make_event(
            {"is_fatal": False, "game_version": "0.5.1"}
        )
        status, body = _parse_response(
            handle_crash_report(event, _CONTEXT)
        )
        assert status == 400
        assert body["error_code"] == "BAD_REQUEST"

    def test_empty_error_message_returns_400(self):
        event = _make_event({"error_message": ""})
        status, body = _parse_response(
            handle_crash_report(event, _CONTEXT)
        )
        assert status == 400
        assert body["error_code"] == "BAD_REQUEST"

    def test_empty_body_returns_400(self):
        event = _make_event({})
        status, body = _parse_response(
            handle_crash_report(event, _CONTEXT)
        )
        assert status == 400
        assert body["error_code"] == "BAD_REQUEST"

    def test_null_body_returns_400(self):
        event = {"body": None, "headers": {}}
        status, body = _parse_response(
            handle_crash_report(event, _CONTEXT)
        )
        assert status == 400
        assert body["error_code"] == "BAD_REQUEST"

    def test_invalid_json_returns_400(self):
        event = {"body": "not valid json", "headers": {}}
        status, body = _parse_response(
            handle_crash_report(event, _CONTEXT)
        )
        assert status == 400
        assert body["error_code"] == "BAD_REQUEST"

    def test_long_message_gets_truncated(self):
        long_message = "x" * 10000
        event = _make_event(
            {"error_message": long_message}
        )
        status, body = _parse_response(
            handle_crash_report(event, _CONTEXT)
        )
        # Should succeed (truncation is internal, not
        # an error).
        assert status == 200
        assert body["status"] == "success"

    def test_minimal_payload_returns_200(self):
        event = _make_event(
            {"error_message": "something broke"}
        )
        status, body = _parse_response(
            handle_crash_report(event, _CONTEXT)
        )
        assert status == 200
        assert body["status"] == "success"

    def test_non_fatal_crash_returns_200(self):
        event = _make_event(
            {
                "error_message": "ERROR: minor issue",
                "is_fatal": False,
            }
        )
        status, body = _parse_response(
            handle_crash_report(event, _CONTEXT)
        )
        assert status == 200
        assert body["status"] == "success"

    def test_cors_headers_present(self):
        event = _make_event(
            {"error_message": "test error"}
        )
        response = handle_crash_report(event, _CONTEXT)
        assert (
            response["headers"]["Access-Control-Allow-Origin"]
            == "*"
        )
        assert (
            response["headers"]["Content-Type"]
            == "application/json"
        )
