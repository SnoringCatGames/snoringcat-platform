"""Party system service for managing player parties."""

import os
import uuid
import time
from dataclasses import dataclass, field
from typing import List, Optional, Dict, Any

import boto3
from boto3.dynamodb.conditions import Key

_PARTY_TTL_SECONDS = 3600  # 1 hour.
_MAX_PARTY_SIZE = 4


@dataclass
class Party:
    """Represents a party."""

    party_id: str
    leader_id: str
    members: List[str] = field(default_factory=list)
    invited: List[str] = field(default_factory=list)
    status: str = "lobby"
    matchmaking_ticket_id: Optional[str] = None
    created_at: int = 0
    expires_at: int = 0


class PartyService:
    """DynamoDB operations for party management."""

    def __init__(self) -> None:
        self._dynamodb = boto3.resource("dynamodb")
        table_name = os.environ.get(
            "PARTIES_TABLE", "hopnbop-parties"
        )
        self._table = self._dynamodb.Table(table_name)

    async def create_party(
        self, leader_id: str
    ) -> Party:
        """Create a new party with the given leader."""
        now = int(time.time())
        party = Party(
            party_id=f"pty_{uuid.uuid4().hex[:12]}",
            leader_id=leader_id,
            members=[leader_id],
            invited=[],
            status="lobby",
            created_at=now,
            expires_at=now + _PARTY_TTL_SECONDS,
        )
        self._table.put_item(
            Item=self._to_item(party)
        )
        return party

    async def get_party(
        self, party_id: str
    ) -> Optional[Party]:
        """Get a party by ID."""
        response = self._table.get_item(
            Key={"party_id": party_id}
        )
        item = response.get("Item")
        if item is None:
            return None
        return self._from_item(item)

    async def get_party_for_player(
        self, player_id: str
    ) -> Optional[Party]:
        """Find the active party for a player.

        Scans for parties where the player is a
        member. This is acceptable because parties
        are short-lived and the table is small.
        """
        response = self._table.scan(
            FilterExpression=(
                "contains(members, :pid)"
            ),
            ExpressionAttributeValues={
                ":pid": player_id,
            },
        )
        items = response.get("Items", [])
        if not items:
            return None
        # Return the most recent party.
        items.sort(
            key=lambda x: x.get("created_at", 0),
            reverse=True,
        )
        return self._from_item(items[0])

    async def invite_player(
        self,
        party_id: str,
        inviter_id: str,
        invitee_id: str,
    ) -> Party:
        """Invite a player to the party."""
        party = await self.get_party(party_id)
        if party is None:
            raise ValueError("Party not found")
        if party.leader_id != inviter_id:
            raise PermissionError(
                "Only the leader can invite"
            )
        if invitee_id in party.members:
            raise ValueError("Already a member")
        if invitee_id in party.invited:
            raise ValueError("Already invited")
        total = (
            len(party.members) + len(party.invited)
        )
        if total >= _MAX_PARTY_SIZE:
            raise ValueError("Party is full")

        party.invited.append(invitee_id)
        self._update_party(party)
        return party

    async def join_party(
        self,
        party_id: str,
        player_id: str,
    ) -> Party:
        """Accept an invite and join the party."""
        party = await self.get_party(party_id)
        if party is None:
            raise ValueError("Party not found")
        if player_id not in party.invited:
            raise ValueError(
                "No pending invite for this player"
            )
        if len(party.members) >= _MAX_PARTY_SIZE:
            raise ValueError("Party is full")

        party.invited.remove(player_id)
        party.members.append(player_id)
        self._update_party(party)
        return party

    async def leave_party(
        self,
        party_id: str,
        player_id: str,
    ) -> Optional[Party]:
        """Leave the party. If leader leaves, party
        is disbanded. Returns the updated party or
        None if disbanded.
        """
        party = await self.get_party(party_id)
        if party is None:
            raise ValueError("Party not found")
        if player_id not in party.members:
            raise ValueError("Not a member")

        if player_id == party.leader_id:
            # Leader leaves. Disband.
            self._table.delete_item(
                Key={"party_id": party_id}
            )
            return None

        party.members.remove(player_id)
        self._update_party(party)
        return party

    async def kick_player(
        self,
        party_id: str,
        leader_id: str,
        target_id: str,
    ) -> Party:
        """Kick a player from the party. Only the
        leader can kick. Returns the updated party.
        """
        party = await self.get_party(party_id)
        if party is None:
            raise ValueError("Party not found")
        if party.leader_id != leader_id:
            raise PermissionError(
                "Only the leader can kick"
            )
        if target_id == leader_id:
            raise ValueError("Cannot kick yourself")
        if target_id not in party.members:
            raise ValueError(
                "Player is not a member"
            )

        party.members.remove(target_id)
        self._update_party(party)
        return party

    async def start_matchmaking(
        self,
        party_id: str,
        player_id: str,
        ticket_id: str,
    ) -> Party:
        """Mark the party as matchmaking."""
        party = await self.get_party(party_id)
        if party is None:
            raise ValueError("Party not found")
        if party.leader_id != player_id:
            raise PermissionError(
                "Only the leader can start"
            )
        if party.status != "lobby":
            raise ValueError(
                "Party is not in lobby state"
            )

        party.status = "matchmaking"
        party.matchmaking_ticket_id = ticket_id
        self._update_party(party)
        return party

    async def update_status(
        self,
        party_id: str,
        status: str,
    ) -> None:
        """Update the party status."""
        self._table.update_item(
            Key={"party_id": party_id},
            UpdateExpression="SET #s = :s",
            ExpressionAttributeNames={
                "#s": "status",
            },
            ExpressionAttributeValues={
                ":s": status,
            },
        )

    async def delete_party(
        self, party_id: str
    ) -> None:
        """Delete a party."""
        self._table.delete_item(
            Key={"party_id": party_id}
        )

    def _update_party(self, party: Party) -> None:
        """Write updated party back to DynamoDB."""
        self._table.put_item(
            Item=self._to_item(party)
        )

    @staticmethod
    def _to_item(party: Party) -> Dict[str, Any]:
        """Convert Party to DynamoDB item."""
        item: Dict[str, Any] = {
            "party_id": party.party_id,
            "leader_id": party.leader_id,
            "members": party.members,
            "invited": party.invited,
            "status": party.status,
            "created_at": party.created_at,
            "expires_at": party.expires_at,
        }
        if party.matchmaking_ticket_id:
            item["matchmaking_ticket_id"] = (
                party.matchmaking_ticket_id
            )
        return item

    @staticmethod
    def _from_item(item: Dict[str, Any]) -> Party:
        """Convert DynamoDB item to Party."""
        return Party(
            party_id=item["party_id"],
            leader_id=item["leader_id"],
            members=item.get("members", []),
            invited=item.get("invited", []),
            status=item.get("status", "lobby"),
            matchmaking_ticket_id=item.get(
                "matchmaking_ticket_id"
            ),
            created_at=int(
                item.get("created_at", 0)
            ),
            expires_at=int(
                item.get("expires_at", 0)
            ),
        )
