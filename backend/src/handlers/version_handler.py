"""Lambda handler for version check endpoint."""

import json
import os
from typing import Dict, Any

from aws_lambda_powertools import Logger
from aws_lambda_powertools.utilities.typing import LambdaContext

logger = Logger()

_HEADERS = {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
}


@logger.inject_lambda_context
def get_version(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """GET /version - Return protocol and game version."""
    try:
        return {
            "statusCode": 200,
            "headers": _HEADERS,
            "body": json.dumps(
                {
                    "protocol_version": int(
                        os.environ.get(
                            "PROTOCOL_VERSION", "1"
                        )
                    ),
                    "game_version": os.environ.get(
                        "GAME_VERSION", "0.1.0"
                    ),
                }
            ),
        }
    except Exception:
        logger.exception("Version check failed")
        return {
            "statusCode": 500,
            "headers": _HEADERS,
            "body": json.dumps(
                {"message": "Internal server error"}
            ),
        }
