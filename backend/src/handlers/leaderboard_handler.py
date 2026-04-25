"""Lambda handler for leaderboard maintenance tasks."""

import os
import sys
from typing import Dict, Any

from aws_lambda_powertools import Logger
from aws_lambda_powertools.utilities.typing import LambdaContext

sys.path.append(os.path.join(os.path.dirname(__file__), ".."))

from services.leaderboard_service import LeaderboardService

logger = Logger()

leaderboard_service = LeaderboardService()


@logger.inject_lambda_context
def reset_weekly_leaderboard(
    event: Dict[str, Any], context: LambdaContext
) -> Dict:
    """Scheduled handler: reset weekly leaderboards.

    Triggered by CloudWatch Events every Monday at
    00:00 UTC.
    """
    try:
        leaderboard_service.delete_weekly_entries()
        logger.info("Weekly leaderboard reset complete")
        return {"status": "success"}
    except Exception:
        logger.exception("Weekly leaderboard reset failed")
        raise
