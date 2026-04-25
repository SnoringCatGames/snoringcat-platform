"""Rate limiting service for API throttling."""

from typing import Dict
from datetime import datetime, timedelta
from collections import defaultdict


class RateLimiter:
    """Simple in-memory rate limiter."""

    def __init__(self):
        # Store: key -> [(timestamp, count)]
        self._buckets: Dict[str, list] = defaultdict(list)

    def check_limit(
        self,
        identifier: str,
        operation: str,
        max_per_min: int = 60,
        window_sec: int = 60,
    ) -> bool:
        """
        Check if request is within rate limit.

        Args:
            identifier: Unique ID (player_id, IP, etc.)
            operation: Operation name (e.g., 'matchmaking')
            max_per_min: Maximum requests per window
            window_sec: Time window in seconds

        Returns:
            True if request is allowed, False if rate limited
        """
        now = datetime.now()
        key = f"{identifier}:{operation}"

        # Clean old entries.
        cutoff = now - timedelta(seconds=window_sec)
        self._buckets[key] = [
            ts for ts in self._buckets[key] if ts > cutoff
        ]

        # Check current count.
        if len(self._buckets[key]) >= max_per_min:
            return False

        # Add new request.
        self._buckets[key].append(now)
        return True

    def get_remaining(
        self, identifier: str, operation: str, max_per_min: int = 60
    ) -> int:
        """Get remaining requests in current window."""
        key = f"{identifier}:{operation}"
        current_count = len(self._buckets.get(key, []))
        return max(0, max_per_min - current_count)
