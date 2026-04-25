"""Service for managing GameLift fleet warmup and idle shutdown.

The fleet runs with DESIRED=0 at rest to save Spot instance hours.
A client-triggered warmup endpoint scales it to 1, and a scheduled
Lambda scales it back to 0 after 30 minutes of inactivity.
"""

import os
import time
from typing import Any, Dict

import boto3
from botocore.exceptions import ClientError


# Seconds of idle (no matches, no warmup) before scale-to-0.
_IDLE_THRESHOLD_SEC = 30 * 60

# Estimated seconds from desired=0 to fleet ACTIVE with an
# IDLE game session slot. Used to surface a countdown to the
# client. Real cold starts vary 180-360 seconds.
_WARMUP_ESTIMATE_SEC = 300

# Single-item sentinel key in the fleet state table.
_STATE_KEY = "fleet_state"


class FleetService:
    """Encapsulates GameLift fleet warmup, status, and idle operations."""

    def __init__(self):
        self._dynamodb = boto3.resource("dynamodb")
        self._table = self._dynamodb.Table(
            os.environ.get(
                "FLEET_STATE_TABLE",
                "hopnbop-fleet-state",
            )
        )
        self._gamelift = boto3.client("gamelift")
        self._fleet_id = os.environ.get("FLEET_ID", "")
        self._location = os.environ.get(
            "GAMELIFT_LOCATION", "us-west-2"
        )

    def _get_fleet_capacity(self) -> Dict[str, int]:
        """Return DESIRED/ACTIVE/PENDING/IDLE instance counts."""
        default = {
            "desired": 0,
            "active": 0,
            "pending": 0,
            "idle": 0,
            "minimum": 0,
            "maximum": 1,
        }
        if not self._fleet_id:
            return default
        try:
            response = (
                self._gamelift.describe_fleet_location_capacity(
                    FleetId=self._fleet_id,
                    Location=self._location,
                )
            )
            counts = response["FleetCapacity"]["InstanceCounts"]
            return {
                "desired": counts.get("DESIRED", 0),
                "active": counts.get("ACTIVE", 0),
                "pending": counts.get("PENDING", 0),
                "idle": counts.get("IDLE", 0),
                "minimum": counts.get("MINIMUM", 0),
                "maximum": counts.get("MAXIMUM", 1),
            }
        except ClientError:
            return default

    def _count_active_game_sessions(self) -> int:
        """Return the number of ACTIVE game sessions on the fleet."""
        if not self._fleet_id:
            return 0
        try:
            response = self._gamelift.describe_game_sessions(
                FleetId=self._fleet_id,
                StatusFilter="ACTIVE",
            )
            return len(response.get("GameSessions", []))
        except ClientError:
            return 0

    def _get_state(self) -> Dict[str, Any]:
        """Read the activity state item from DynamoDB."""
        try:
            response = self._table.get_item(
                Key={"state_key": _STATE_KEY}
            )
            return response.get("Item", {})
        except ClientError:
            return {}

    def update_activity(self, activity_type: str) -> None:
        """Update last_activity_at to now.

        Called on warmup requests, match start, match end, and
        any time the fleet should be considered alive.
        """
        now = int(time.time())
        try:
            self._table.put_item(
                Item={
                    "state_key": _STATE_KEY,
                    "last_activity_at": now,
                    "last_activity_type": activity_type,
                    "updated_at": now,
                }
            )
        except ClientError:
            # Best-effort. A missed activity update means a
            # potential premature scale-down, which the next
            # client warmup call will correct.
            pass

    def _scale_up_if_idle(self) -> bool:
        """Scale fleet to desired=1 if currently 0.

        Returns True if a scale-up call was issued.
        """
        if not self._fleet_id:
            return False
        capacity = self._get_fleet_capacity()
        if capacity["desired"] > 0:
            return False
        try:
            self._gamelift.update_fleet_capacity(
                FleetId=self._fleet_id,
                DesiredInstances=1,
                MinSize=0,
                MaxSize=1,
                Location=self._location,
            )
            return True
        except ClientError:
            return False

    def warm_up(self, source: str = "client") -> Dict[str, Any]:
        """Record activity and scale up if needed.

        Returns the current status dict for immediate client UI.
        """
        self.update_activity(f"warmup:{source}")
        self._scale_up_if_idle()
        return self.build_status_response()

    def build_status_response(self) -> Dict[str, Any]:
        """Return a dict describing current fleet readiness.

        Status values:
          "ready"   - At least one ACTIVE instance.
          "warming" - DESIRED >= 1 but no ACTIVE instance yet.
          "cold"    - DESIRED == 0, instance not running.
        """
        capacity = self._get_fleet_capacity()
        state = self._get_state()
        last_activity_at = int(state.get("last_activity_at", 0))
        now = int(time.time())
        seconds_since_activity = (
            now - last_activity_at if last_activity_at else 0
        )

        is_ready = (
            capacity["active"] > 0 and capacity["idle"] > 0
        )
        is_warming = (
            capacity["desired"] > 0 and capacity["active"] == 0
        ) or (
            capacity["active"] > 0 and capacity["idle"] == 0
        )

        if is_ready:
            status = "ready"
            estimated_seconds_remaining = 0
        elif is_warming:
            status = "warming"
            estimated_seconds_remaining = max(
                0,
                _WARMUP_ESTIMATE_SEC - seconds_since_activity,
            )
        else:
            status = "cold"
            estimated_seconds_remaining = _WARMUP_ESTIMATE_SEC

        return {
            "status": status,
            "desired_instances": capacity["desired"],
            "active_instances": capacity["active"],
            "pending_instances": capacity["pending"],
            "idle_instances": capacity["idle"],
            "estimated_seconds_remaining": (
                estimated_seconds_remaining
            ),
            "last_activity_at": last_activity_at,
            "seconds_since_activity": seconds_since_activity,
            "warmup_estimate_sec": _WARMUP_ESTIMATE_SEC,
            "idle_threshold_sec": _IDLE_THRESHOLD_SEC,
        }

    def scale_down_if_idle(self) -> Dict[str, Any]:
        """Scale to 0 if no active sessions AND idle > threshold.

        Called by the scheduled idle-check Lambda every 5 minutes.
        """
        if not self._fleet_id:
            return {
                "action": "skipped",
                "reason": "no_fleet_id",
            }

        capacity = self._get_fleet_capacity()
        if capacity["desired"] == 0:
            return {
                "action": "skipped",
                "reason": "already_at_zero",
            }

        active_sessions = self._count_active_game_sessions()
        if active_sessions > 0:
            # Active sessions count as activity. Refresh the
            # timestamp so the idle window starts after the
            # match ends rather than when it started.
            self.update_activity("match_active")
            return {
                "action": "skipped",
                "reason": "active_sessions",
                "active_sessions": active_sessions,
            }

        state = self._get_state()
        last_activity_at = int(state.get("last_activity_at", 0))
        now = int(time.time())
        if last_activity_at == 0:
            # First run after deploy. Seed the timestamp so
            # the next idle check is measured from now, not
            # from epoch, otherwise we would instantly scale
            # a freshly-deployed fleet back to 0.
            self.update_activity("bootstrap")
            return {
                "action": "skipped",
                "reason": "bootstrap",
            }
        seconds_since_activity = now - last_activity_at

        if seconds_since_activity < _IDLE_THRESHOLD_SEC:
            return {
                "action": "skipped",
                "reason": "within_idle_window",
                "seconds_since_activity": (
                    seconds_since_activity
                ),
                "idle_threshold_sec": _IDLE_THRESHOLD_SEC,
            }

        try:
            self._gamelift.update_fleet_capacity(
                FleetId=self._fleet_id,
                DesiredInstances=0,
                MinSize=0,
                MaxSize=1,
                Location=self._location,
            )
            return {
                "action": "scaled_down",
                "seconds_since_activity": (
                    seconds_since_activity
                ),
            }
        except ClientError as e:
            return {
                "action": "error",
                "error": str(e),
            }
