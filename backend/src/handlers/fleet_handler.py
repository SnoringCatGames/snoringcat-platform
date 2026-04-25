"""Lambda handlers for fleet warmup, status, and scheduled idle check.

The warmup/status endpoints are unauthenticated by design so the
client can call them on app startup before any auth flow. Abuse
cost is bounded: fleet MAX=1 so scale-up is capped, and the
Lambda itself is inexpensive.
"""

import json
import os
import sys
from typing import Any, Dict

from aws_lambda_powertools import Logger, Tracer
from aws_lambda_powertools.utilities.typing import LambdaContext

sys.path.append(os.path.join(os.path.dirname(__file__), ".."))

from services.fleet_service import FleetService

logger = Logger()
tracer = Tracer()

fleet_service = FleetService()

_HEADERS = {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
}


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def warm_up_fleet(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """POST /fleet/warmup - Request fleet warmup and return status."""
    try:
        source = "client"
        body = event.get("body")
        if body:
            try:
                parsed = json.loads(body)
                if isinstance(parsed, dict):
                    source = parsed.get("source", "client")
            except (json.JSONDecodeError, TypeError):
                pass

        response = fleet_service.warm_up(source=source)
        logger.info(
            "Fleet warmup requested",
            extra={
                "source": source,
                "status": response["status"],
                "desired": response["desired_instances"],
                "active": response["active_instances"],
            },
        )
        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(response),
        }
    except Exception:
        logger.exception("Fleet warmup error")
        return _error(500, "Internal server error")


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def get_fleet_status(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /fleet/status - Read current fleet readiness.

    Does not trigger scale-up. Intended for periodic polling
    from lobby screens without incurring extra state updates.
    """
    try:
        response = fleet_service.build_status_response()
        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(response),
        }
    except Exception:
        logger.exception("Fleet status error")
        return _error(500, "Internal server error")


@tracer.capture_lambda_handler
@logger.inject_lambda_context
def scheduled_idle_check(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """EventBridge scheduled handler.

    Scales the fleet to 0 if no active game sessions and more
    than 30 minutes since the last activity.
    """
    try:
        result = fleet_service.scale_down_if_idle()
        logger.info("Idle check completed", extra=result)
        return result
    except Exception:
        logger.exception("Idle check error")
        return {"action": "error", "error": "internal"}


def _error(status_code: int, message: str) -> Dict:
    return {
        "statusCode": status_code,
        "headers": _HEADERS,
        "body": json.dumps(
            {
                "status": "error",
                "message": message,
            }
        ),
    }
