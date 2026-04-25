"""GameLift service wrapper for matchmaking operations."""

import asyncio
import json
import boto3
from typing import List, Dict, Optional
from dataclasses import dataclass
from datetime import datetime
from botocore.exceptions import ClientError


@dataclass
class MatchmakingPlayer:
    """Player data for matchmaking request."""

    player_id: str
    skill_rating: int
    region: str
    latency_map: Dict[str, int]
    platform: str = "native"
    is_authenticated: int = 0


@dataclass
class MatchResult:
    """Result of completed matchmaking."""

    ticket_id: str
    game_session_id: str
    server_ip: str
    server_port: int
    player_session_ids: List[str]
    transport_type: str = "enet"


class GameLiftService:
    """Wrapper for AWS GameLift API operations."""

    def __init__(
        self,
        region: str = "us-west-2",
        poll_interval_sec: float = 1.0,
        max_poll_time_sec: float = 120.0,
    ):
        self.client = boto3.client("gamelift", region_name=region)
        self.poll_interval = poll_interval_sec
        self.max_poll_time = max_poll_time_sec

    async def start_matchmaking(
        self,
        config_name: str,
        players: List[MatchmakingPlayer],
        ticket_id: Optional[str] = None,
    ) -> str:
        """
        Start FlexMatch matchmaking.

        Returns:
            ticket_id (str): Matchmaking ticket ID for polling
        """
        player_dicts = [
            {
                "PlayerId": p.player_id,
                "PlayerAttributes": {
                    "skill": {"N": p.skill_rating},
                    "region": {"S": p.region},
                    "is_web": {
                        "N": 1 if p.platform == "web" else 0,
                    },
                    "is_authenticated": {
                        "N": p.is_authenticated,
                    },
                },
                "LatencyInMs": p.latency_map,
            }
            for p in players
        ]

        try:
            kwargs = {
                "ConfigurationName": config_name,
                "Players": player_dicts,
            }
            if ticket_id is not None:
                kwargs["TicketId"] = ticket_id
            response = self.client.start_matchmaking(**kwargs)
            return response["MatchmakingTicket"]["TicketId"]

        except ClientError as e:
            error_code = e.response["Error"]["Code"]
            if error_code == "InvalidRequestException":
                raise ValueError(f"Invalid matchmaking request: {e}")
            elif error_code == "UnsupportedRegionException":
                raise ValueError(f"Region not supported: {e}")
            else:
                raise

    async def poll_matchmaking(self, ticket_id: str) -> MatchResult:
        """
        Poll matchmaking ticket until COMPLETED or failure.

        Returns:
            MatchResult with connection info and player_session_ids

        Raises:
            TimeoutError: Polling exceeded max_poll_time
            ValueError: Matchmaking failed, timed out, or cancelled
        """
        start_time = datetime.now()

        while True:
            # Check timeout.
            elapsed = (datetime.now() - start_time).total_seconds()
            if elapsed > self.max_poll_time:
                # Cancel stale ticket.
                await self.cancel_matchmaking(ticket_id)
                raise TimeoutError(
                    f"Matchmaking polling timeout after {elapsed}s"
                )

            # Poll ticket status.
            try:
                response = self.client.describe_matchmaking(
                    TicketIds=[ticket_id]
                )
            except ClientError as e:
                raise ValueError(f"Failed to describe ticket: {e}")

            if not response["TicketList"]:
                raise ValueError(f"Ticket {ticket_id} not found")

            ticket = response["TicketList"][0]
            status = ticket["Status"]

            # Success - match found.
            if status == "COMPLETED":
                conn_info = ticket["GameSessionConnectionInfo"]
                game_session_id = (
                    conn_info["GameSessionArn"].split("/")[-1]
                )
                transport = self._determine_transport_from_session(
                    conn_info["GameSessionArn"]
                )

                return MatchResult(
                    ticket_id=ticket_id,
                    game_session_id=game_session_id,
                    server_ip=conn_info["IpAddress"],
                    server_port=conn_info["Port"],
                    player_session_ids=[
                        mp["PlayerSessionId"]
                        for mp in conn_info["MatchedPlayerSessions"]
                    ],
                    transport_type=transport,
                )

            # Terminal failure states.
            if status == "FAILED":
                reason = ticket.get("StatusReason", "Unknown")
                raise ValueError(f"Matchmaking failed: {reason}")

            if status == "TIMED_OUT":
                raise TimeoutError("GameLift matchmaking timeout")

            if status == "CANCELLED":
                raise ValueError("Matchmaking cancelled")

            # Still in progress (QUEUED, SEARCHING, PLACING).
            await asyncio.sleep(self.poll_interval)

    def _determine_transport_from_session(
        self, game_session_arn: str
    ) -> str:
        """Determine transport from the game session's
        MatchmakerData, which contains ALL matched players.

        The ticket's Players list only has this ticket's
        players, not the full match. MatchmakerData has
        every player's attributes, so we can correctly
        detect web players even when polling from a
        desktop-only ticket.
        """
        try:
            response = self.client.describe_game_sessions(
                GameSessionId=game_session_arn
            )
            sessions = response.get("GameSessions", [])
            if not sessions:
                return "enet"
            matchmaker_data = sessions[0].get(
                "MatchmakerData", ""
            )
            if not matchmaker_data:
                return "enet"
            data = json.loads(matchmaker_data)
            for team in data.get("teams", []):
                for player in team.get("players", []):
                    attrs = player.get("attributes", {})
                    is_web = attrs.get("is_web", {})
                    value = is_web.get(
                        "valueAttribute", 0
                    )
                    if value == 1.0:
                        return "webrtc"
        except (ClientError, json.JSONDecodeError, KeyError):
            pass
        return "enet"

    async def cancel_matchmaking(self, ticket_id: str) -> None:
        """Cancel active matchmaking ticket."""
        try:
            self.client.stop_matchmaking(TicketId=ticket_id)
        except ClientError as e:
            error_code = e.response["Error"]["Code"]
            if error_code != "NotFoundException":
                raise

    async def get_ticket_status(self, ticket_id: str) -> Dict:
        """Get current ticket status without blocking."""
        try:
            response = self.client.describe_matchmaking(
                TicketIds=[ticket_id]
            )
        except ClientError as e:
            raise ValueError(f"Failed to get ticket: {e}")

        if not response["TicketList"]:
            raise ValueError(f"Ticket {ticket_id} not found")

        ticket = response["TicketList"][0]

        result = {
            "status": ticket["Status"].lower(),
            "ticket_id": ticket_id,
        }

        # Add estimated wait time if available.
        if "EstimatedWaitTime" in ticket:
            result["estimated_wait_ms"] = ticket["EstimatedWaitTime"]

        # Add match progress if available.
        if "Players" in ticket:
            result["players_needed"] = len(ticket["Players"])

        return result
