"""Lambda handler for client telemetry ingestion."""

import json
import os
from typing import Dict, Any

from aws_lambda_powertools import Logger, Metrics, Tracer
from aws_lambda_powertools.metrics import MetricUnit
from aws_lambda_powertools.utilities.typing import LambdaContext

logger = Logger()
tracer = Tracer()
metrics = Metrics()

# CORS headers included in every response.
_HEADERS = {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
}

_MAX_MESSAGE_LENGTH = 4096


@tracer.capture_lambda_handler
@logger.inject_lambda_context
@metrics.log_metrics
def handle_crash_report(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /telemetry/crash - Ingest client crash report."""
    try:
        body = json.loads(event.get("body") or "{}")

        error_message = body.get("error_message", "")
        if not error_message:
            return _error(
                400,
                "BAD_REQUEST",
                "Missing error_message",
            )

        # Truncate long messages.
        error_message = error_message[:_MAX_MESSAGE_LENGTH]

        is_fatal = body.get("is_fatal", False)

        # Log as structured JSON for CloudWatch Logs
        # Insights queries.
        logger.info(
            "Client crash report",
            extra={
                "crash_error_message": error_message,
                "crash_is_fatal": is_fatal,
                "crash_game_version": body.get(
                    "game_version", ""
                ),
                "crash_operating_system": body.get(
                    "operating_system", ""
                ),
                "crash_player_id": body.get(
                    "player_id", ""
                ),
                "crash_is_server": body.get(
                    "is_server", False
                ),
                "crash_server_frame_index": body.get(
                    "server_frame_index", 0
                ),
                "crash_render_fps": body.get(
                    "render_fps", 0
                ),
                "crash_physics_fps": body.get(
                    "physics_fps", 0
                ),
                "crash_network_ping_ms": body.get(
                    "network_ping_ms", 0
                ),
            },
        )

        # Emit CloudWatch metrics.
        metrics.add_metric(
            name="crash_report",
            unit=MetricUnit.Count,
            value=1,
        )
        if is_fatal:
            metrics.add_metric(
                name="fatal_crash",
                unit=MetricUnit.Count,
                value=1,
            )

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps({"status": "success"}),
        }

    except json.JSONDecodeError:
        return _error(
            400, "BAD_REQUEST", "Invalid JSON body"
        )
    except Exception:
        logger.exception("Telemetry handler error")
        return _error(
            500,
            "INTERNAL_ERROR",
            "Internal server error",
        )


def _error(
    status_code: int, error_code: str, message: str
) -> Dict:
    """Format error response."""
    return {
        "statusCode": status_code,
        "headers": _HEADERS,
        "body": json.dumps(
            {
                "status": "error",
                "error_code": error_code,
                "message": message,
            }
        ),
    }
