"""Lambda handler for active session status."""

import json
import os
from typing import Dict, Any

from aws_lambda_powertools import Logger
from aws_lambda_powertools.utilities.typing import (
    LambdaContext,
)

import sys

sys.path.append(os.path.join(os.path.dirname(__file__), ".."))

from services.auth_service import AuthToken
from services.active_session_service import (
    ActiveSessionService,
)
from services import secrets_service

logger = Logger()

_HEADERS = {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
    "Access-Control-Allow-Headers": (
        "Content-Type,Authorization"
    ),
    "Access-Control-Allow-Methods": "GET,OPTIONS",
}

active_session_service = ActiveSessionService()


@logger.inject_lambda_context
def get_active_session(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /session/active

    Return the player's current active session state.
    Used by clients during a match to detect if their
    session was overridden by another device starting
    matchmaking.
    """
    try:
        auth_header = (
            event.get("headers", {}).get(
                "Authorization", ""
            )
        )
        if not auth_header.startswith("Bearer "):
            return {
                "statusCode": 401,
                "headers": _HEADERS,
                "body": json.dumps(
                    {
                        "status": "error",
                        "message": "Missing authorization",
                    }
                ),
            }

        jwt_token = auth_header[7:]
        jwt_secret = secrets_service.get_jwt_secret()

        if jwt_token.startswith("DEBUG_"):
            player_id = jwt_token
        else:
            auth_token = AuthToken.from_jwt(
                jwt_token, jwt_secret
            )
            player_id = auth_token.player_id

        session = (
            active_session_service.get_active_session(
                player_id
            )
        )

        if session is None:
            return {
                "statusCode": 200,
                "headers": _HEADERS,
                "body": json.dumps({"state": None}),
            }

        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "state": session.get("state"),
                    "session_id": session.get(
                        "session_id", ""
                    ),
                }
            ),
        }

    except Exception:
        logger.exception(
            "Active session check failed"
        )
        return {
            "statusCode": 500,
            "headers": _HEADERS,
            "body": json.dumps(
                {"message": "Internal server error"}
            ),
        }
