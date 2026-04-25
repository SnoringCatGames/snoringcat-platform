"""Friends service for DynamoDB operations."""

import os
import boto3
from typing import Dict, List, Optional
from dataclasses import dataclass
from datetime import datetime


# Relationship status constants.
STATUS_ACCEPTED = "accepted"
STATUS_PENDING_SENT = "pending_sent"
STATUS_PENDING_RECEIVED = "pending_received"
STATUS_REJECTED = "rejected"


@dataclass
class FriendRelationship:
    """A single friend or pending relationship."""

    friend_id: str
    display_name: str
    source: str
    status: str
    sender_id: str
    created_at: int
    updated_at: int


class FriendsService:
    """DynamoDB friends operations."""

    def __init__(self):
        self.dynamodb = boto3.resource("dynamodb")
        self.friends_table_name = os.environ.get(
            "FRIENDS_TABLE", "hopnbop-friends"
        )
        self.players_table_name = os.environ.get(
            "PLAYERS_TABLE", "hopnbop-players"
        )
        self.friends_table = self.dynamodb.Table(
            self.friends_table_name
        )
        self.players_table = self.dynamodb.Table(
            self.players_table_name
        )

    def _validate_players(
        self, player_id: str, friend_id: str
    ) -> None:
        """Validate both players exist and are not
        anonymous. Raises ValueError on failure."""
        if player_id == friend_id:
            raise ValueError(
                "Cannot add yourself as a friend"
            )

        player = self.players_table.get_item(
            Key={"player_id": player_id},
            ProjectionExpression="player_id, is_anonymous",
        )
        friend = self.players_table.get_item(
            Key={"player_id": friend_id},
            ProjectionExpression="player_id, is_anonymous",
        )
        if "Item" not in player:
            raise ValueError("Player not found")
        if "Item" not in friend:
            raise ValueError("Friend not found")
        if player["Item"].get("is_anonymous", False):
            raise ValueError(
                "Anonymous players cannot add friends"
            )
        if friend["Item"].get("is_anonymous", False):
            raise ValueError(
                "Cannot add an anonymous player"
                " as a friend"
            )

    def _get_relationship(
        self, player_id: str, friend_id: str
    ) -> Optional[dict]:
        """Get a single relationship row, or None."""
        response = self.friends_table.get_item(
            Key={
                "player_id": player_id,
                "friend_id": friend_id,
            },
        )
        return response.get("Item")

    def _resolve_display_names(
        self, friend_ids: List[str]
    ) -> Dict[str, str]:
        """Batch-get display names for a list of
        player IDs."""
        if not friend_ids:
            return {}
        keys = [
            {"player_id": fid} for fid in friend_ids
        ]
        batch_response = self.dynamodb.batch_get_item(
            RequestItems={
                self.players_table_name: {
                    "Keys": keys,
                    "ProjectionExpression": (
                        "player_id, display_name"
                    ),
                }
            }
        )
        name_map = {}
        for item in batch_response.get(
            "Responses", {}
        ).get(self.players_table_name, []):
            name_map[item["player_id"]] = item.get(
                "display_name", ""
            )
        return name_map

    async def send_friend_request(
        self,
        sender_id: str,
        receiver_id: str,
        source: str = "friend_code",
    ) -> dict:
        """Send a friend request or auto-accept if
        the other player already sent one.

        Returns a dict with a "result" key:
        - "request_sent": new pending request created.
        - "auto_accepted": mutual request, now friends.
        - "already_friends": already accepted friends.
        - "already_pending": request already sent.
        """
        self._validate_players(sender_id, receiver_id)

        existing = self._get_relationship(
            sender_id, receiver_id
        )

        if existing is not None:
            status = existing.get(
                "status", STATUS_ACCEPTED
            )
            if status == STATUS_ACCEPTED:
                return {"result": "already_friends"}
            if status == STATUS_PENDING_SENT:
                return {"result": "already_pending"}
            if status == STATUS_PENDING_RECEIVED:
                # The receiver already sent us a
                # request. Auto-accept both directions.
                now = int(datetime.now().timestamp())
                self.friends_table.update_item(
                    Key={
                        "player_id": sender_id,
                        "friend_id": receiver_id,
                    },
                    UpdateExpression=(
                        "SET #s = :accepted,"
                        " updated_at = :now"
                    ),
                    ExpressionAttributeNames={
                        "#s": "status",
                    },
                    ExpressionAttributeValues={
                        ":accepted": STATUS_ACCEPTED,
                        ":now": now,
                    },
                )
                self.friends_table.update_item(
                    Key={
                        "player_id": receiver_id,
                        "friend_id": sender_id,
                    },
                    UpdateExpression=(
                        "SET #s = :accepted,"
                        " updated_at = :now"
                    ),
                    ExpressionAttributeNames={
                        "#s": "status",
                    },
                    ExpressionAttributeValues={
                        ":accepted": STATUS_ACCEPTED,
                        ":now": now,
                    },
                )
                return {"result": "auto_accepted"}

        now = int(datetime.now().timestamp())

        with self.friends_table.batch_writer() as batch:
            batch.put_item(
                Item={
                    "player_id": sender_id,
                    "friend_id": receiver_id,
                    "status": STATUS_PENDING_SENT,
                    "sender_id": sender_id,
                    "source": source,
                    "created_at": now,
                    "updated_at": now,
                }
            )
            batch.put_item(
                Item={
                    "player_id": receiver_id,
                    "friend_id": sender_id,
                    "status": STATUS_PENDING_RECEIVED,
                    "sender_id": sender_id,
                    "source": source,
                    "created_at": now,
                    "updated_at": now,
                }
            )

        return {"result": "request_sent"}

    async def accept_friend_request(
        self,
        receiver_id: str,
        sender_id: str,
    ) -> bool:
        """Accept a pending friend request.

        Returns True if accepted, False if no pending
        request found.
        """
        existing = self._get_relationship(
            receiver_id, sender_id
        )
        if existing is None:
            return False
        status = existing.get(
            "status", STATUS_ACCEPTED
        )
        if status != STATUS_PENDING_RECEIVED:
            return False

        now = int(datetime.now().timestamp())
        self.friends_table.update_item(
            Key={
                "player_id": receiver_id,
                "friend_id": sender_id,
            },
            UpdateExpression=(
                "SET #s = :accepted,"
                " updated_at = :now"
            ),
            ExpressionAttributeNames={
                "#s": "status",
            },
            ExpressionAttributeValues={
                ":accepted": STATUS_ACCEPTED,
                ":now": now,
            },
        )
        self.friends_table.update_item(
            Key={
                "player_id": sender_id,
                "friend_id": receiver_id,
            },
            UpdateExpression=(
                "SET #s = :accepted,"
                " updated_at = :now"
            ),
            ExpressionAttributeNames={
                "#s": "status",
            },
            ExpressionAttributeValues={
                ":accepted": STATUS_ACCEPTED,
                ":now": now,
            },
        )
        return True

    async def reject_friend_request(
        self,
        receiver_id: str,
        sender_id: str,
    ) -> bool:
        """Reject a pending friend request.

        Updates the sender's row to "rejected" so the
        sender receives a notification. Deletes the
        receiver's row. Rejected rows are cleaned up
        in mark_friends_seen().

        Returns True if rejected, False if no pending
        request found.
        """
        existing = self._get_relationship(
            receiver_id, sender_id
        )
        if existing is None:
            return False
        status = existing.get(
            "status", STATUS_ACCEPTED
        )
        if status != STATUS_PENDING_RECEIVED:
            return False

        now = int(datetime.now().timestamp())

        # Update sender's row to "rejected" so it
        # appears in their notifications.
        self.friends_table.update_item(
            Key={
                "player_id": sender_id,
                "friend_id": receiver_id,
            },
            UpdateExpression=(
                "SET #s = :rejected,"
                " updated_at = :now"
            ),
            ExpressionAttributeNames={
                "#s": "status",
            },
            ExpressionAttributeValues={
                ":rejected": STATUS_REJECTED,
                ":now": now,
            },
        )
        # Delete the receiver's row.
        self.friends_table.delete_item(
            Key={
                "player_id": receiver_id,
                "friend_id": sender_id,
            }
        )
        return True

    async def cancel_friend_request(
        self,
        sender_id: str,
        receiver_id: str,
    ) -> bool:
        """Cancel a sent friend request by deleting
        both rows.

        Returns True if cancelled, False if no pending
        request found.
        """
        existing = self._get_relationship(
            sender_id, receiver_id
        )
        if existing is None:
            return False
        status = existing.get(
            "status", STATUS_ACCEPTED
        )
        if status != STATUS_PENDING_SENT:
            return False

        with self.friends_table.batch_writer() as batch:
            batch.delete_item(
                Key={
                    "player_id": sender_id,
                    "friend_id": receiver_id,
                }
            )
            batch.delete_item(
                Key={
                    "player_id": receiver_id,
                    "friend_id": sender_id,
                }
            )
        return True

    async def remove_friend(
        self,
        player_id: str,
        friend_id: str,
    ) -> bool:
        """Remove an accepted friend relationship.

        Returns True if removed, False if not an
        accepted friend.
        """
        existing = self._get_relationship(
            player_id, friend_id
        )
        if existing is None:
            return False
        status = existing.get(
            "status", STATUS_ACCEPTED
        )
        if status != STATUS_ACCEPTED:
            return False

        with self.friends_table.batch_writer() as batch:
            batch.delete_item(
                Key={
                    "player_id": player_id,
                    "friend_id": friend_id,
                }
            )
            batch.delete_item(
                Key={
                    "player_id": friend_id,
                    "friend_id": player_id,
                }
            )
        return True

    async def list_all_relationships(
        self, player_id: str
    ) -> Dict[str, List[FriendRelationship]]:
        """List all relationships grouped by status.

        Returns a dict with keys "friends",
        "sent_requests", "incoming_requests".
        """
        response = self.friends_table.query(
            KeyConditionExpression=(
                boto3.dynamodb.conditions.Key(
                    "player_id"
                ).eq(player_id)
            ),
        )

        items = response["Items"]
        friend_ids = [
            item["friend_id"] for item in items
        ]
        name_map = self._resolve_display_names(
            friend_ids
        )

        friends = []
        sent_requests = []
        incoming_requests = []

        for item in items:
            fid = item["friend_id"]
            status = item.get(
                "status", STATUS_ACCEPTED
            )
            rel = FriendRelationship(
                friend_id=fid,
                display_name=name_map.get(fid, ""),
                source=item.get("source", ""),
                status=status,
                sender_id=item.get("sender_id", ""),
                created_at=int(
                    item.get("created_at", 0)
                ),
                updated_at=int(
                    item.get("updated_at", 0)
                ),
            )
            if status == STATUS_ACCEPTED:
                friends.append(rel)
            elif status == STATUS_PENDING_SENT:
                sent_requests.append(rel)
            elif status == STATUS_PENDING_RECEIVED:
                incoming_requests.append(rel)
            elif status == STATUS_REJECTED:
                # Rejected rows are excluded from all
                # lists. Cleaned up in mark_friends_seen.
                pass
            else:
                # Legacy rows without status field.
                friends.append(rel)

        return {
            "friends": friends,
            "sent_requests": sent_requests,
            "incoming_requests": incoming_requests,
        }

    async def get_notifications(
        self,
        player_id: str,
        since_timestamp: int,
    ) -> dict:
        """Get new friend notifications since a
        timestamp.

        Returns incoming pending requests and recently
        accepted relationships where this player was
        the sender (i.e., someone accepted our request).
        """
        response = self.friends_table.query(
            KeyConditionExpression=(
                boto3.dynamodb.conditions.Key(
                    "player_id"
                ).eq(player_id)
            ),
            FilterExpression=(
                boto3.dynamodb.conditions.Attr(
                    "updated_at"
                ).gt(since_timestamp)
            ),
        )

        items = response["Items"]
        friend_ids = [
            item["friend_id"] for item in items
        ]
        name_map = self._resolve_display_names(
            friend_ids
        )

        incoming_requests = []
        accepted_requests = []
        rejected_requests = []

        for item in items:
            fid = item["friend_id"]
            status = item.get(
                "status", STATUS_ACCEPTED
            )
            entry = {
                "friend_id": fid,
                "display_name": name_map.get(fid, ""),
                "sender_id": item.get(
                    "sender_id", ""
                ),
                "status": status,
                "updated_at": int(
                    item.get("updated_at", 0)
                ),
            }
            if status == STATUS_PENDING_RECEIVED:
                incoming_requests.append(entry)
            elif (
                status == STATUS_ACCEPTED
                and item.get("sender_id", "")
                == player_id
            ):
                # We sent the request and they
                # accepted it.
                accepted_requests.append(entry)
            elif status == STATUS_REJECTED:
                # We sent the request and they
                # rejected it.
                rejected_requests.append(entry)

        return {
            "incoming_requests": incoming_requests,
            "accepted_requests": accepted_requests,
            "rejected_requests": rejected_requests,
        }

    async def get_unseen_count(
        self, player_id: str
    ) -> int:
        """Count unseen friend notifications."""
        last_seen = await self.get_last_seen_timestamp(
            player_id
        )
        notifications = await self.get_notifications(
            player_id, last_seen
        )
        return (
            len(notifications["incoming_requests"])
            + len(notifications["accepted_requests"])
            + len(notifications["rejected_requests"])
        )

    async def mark_friends_seen(
        self, player_id: str
    ) -> None:
        """Update last_seen_friends_at on the player
        record and clean up rejected rows."""
        now = int(datetime.now().timestamp())
        self.players_table.update_item(
            Key={"player_id": player_id},
            UpdateExpression=(
                "SET last_seen_friends_at = :now"
            ),
            ExpressionAttributeValues={
                ":now": now,
            },
        )

        # Delete any rejected rows so they don't
        # reappear in future notification polls.
        response = self.friends_table.query(
            KeyConditionExpression=(
                boto3.dynamodb.conditions.Key(
                    "player_id"
                ).eq(player_id)
            ),
            FilterExpression=(
                boto3.dynamodb.conditions.Attr(
                    "status"
                ).eq(STATUS_REJECTED)
            ),
            ProjectionExpression=(
                "player_id, friend_id"
            ),
        )
        if response["Items"]:
            with self.friends_table.batch_writer() \
                    as batch:
                for item in response["Items"]:
                    batch.delete_item(
                        Key={
                            "player_id": item[
                                "player_id"
                            ],
                            "friend_id": item[
                                "friend_id"
                            ],
                        }
                    )

    async def get_last_seen_timestamp(
        self, player_id: str
    ) -> int:
        """Get the last_seen_friends_at timestamp for
        a player. Returns 0 if not set."""
        response = self.players_table.get_item(
            Key={"player_id": player_id},
            ProjectionExpression="last_seen_friends_at",
        )
        item = response.get("Item", {})
        return int(
            item.get("last_seen_friends_at", 0)
        )

    async def delete_all_friends(
        self, player_id: str
    ) -> None:
        """Delete all relationships for a player.

        Used for GDPR account deletion. Removes both
        directions for every relationship regardless
        of status.
        """
        response = self.friends_table.query(
            KeyConditionExpression=(
                boto3.dynamodb.conditions.Key(
                    "player_id"
                ).eq(player_id)
            ),
        )

        with self.friends_table.batch_writer() as batch:
            for item in response["Items"]:
                fid = item["friend_id"]
                batch.delete_item(
                    Key={
                        "player_id": player_id,
                        "friend_id": fid,
                    }
                )
                batch.delete_item(
                    Key={
                        "player_id": fid,
                        "friend_id": player_id,
                    }
                )

    async def migrate_friends(
        self,
        from_player_id: str,
        to_player_id: str,
    ) -> None:
        """Migrate all relationships from one player
        to another.

        For each relationship of from_player_id:
        - If accepted and target doesn't already have
          a relationship with that friend, migrate it.
        - Pending requests are dropped during migration.
        - Removes all from_player_id relationships.
        Used during account merges.
        """
        from_rels = await self.list_all_relationships(
            from_player_id
        )
        to_rels = await self.list_all_relationships(
            to_player_id
        )

        # Build set of all friend_ids the target
        # already has any relationship with.
        to_friends_set = set()
        for group in to_rels.values():
            for rel in group:
                to_friends_set.add(rel.friend_id)

        with self.friends_table.batch_writer() as batch:
            # Process all relationship types.
            all_rels = (
                from_rels["friends"]
                + from_rels["sent_requests"]
                + from_rels["incoming_requests"]
            )
            for rel in all_rels:
                fid = rel.friend_id
                # Remove the old relationship.
                batch.delete_item(
                    Key={
                        "player_id": from_player_id,
                        "friend_id": fid,
                    }
                )
                batch.delete_item(
                    Key={
                        "player_id": fid,
                        "friend_id": from_player_id,
                    }
                )
                # Only migrate accepted friendships.
                if rel.status != STATUS_ACCEPTED:
                    continue
                if (
                    fid == to_player_id
                    or fid in to_friends_set
                ):
                    continue
                # Add accepted friendship to target.
                now = int(datetime.now().timestamp())
                batch.put_item(Item={
                    "player_id": to_player_id,
                    "friend_id": fid,
                    "status": STATUS_ACCEPTED,
                    "sender_id": rel.sender_id,
                    "source": rel.source,
                    "created_at": rel.created_at,
                    "updated_at": now,
                })
                batch.put_item(Item={
                    "player_id": fid,
                    "friend_id": to_player_id,
                    "status": STATUS_ACCEPTED,
                    "sender_id": rel.sender_id,
                    "source": rel.source,
                    "created_at": rel.created_at,
                    "updated_at": now,
                })

    async def get_accepted_friend_ids(
        self, player_id: str
    ) -> List[str]:
        """Return just the friend_ids of accepted
        relationships for a player. Used by presence
        to avoid fetching display names."""
        response = self.friends_table.query(
            KeyConditionExpression=(
                boto3.dynamodb.conditions.Key(
                    "player_id"
                ).eq(player_id)
            ),
            FilterExpression=(
                boto3.dynamodb.conditions.Attr(
                    "status"
                ).eq(STATUS_ACCEPTED)
            ),
            ProjectionExpression="friend_id",
        )
        return [
            item["friend_id"]
            for item in response["Items"]
        ]

    async def get_friends_data_for_export(
        self, player_id: str
    ) -> List[dict]:
        """Get friends data for GDPR export."""
        rels = await self.list_all_relationships(
            player_id
        )
        result = []
        for group in rels.values():
            for rel in group:
                result.append({
                    "friend_id": rel.friend_id,
                    "display_name": rel.display_name,
                    "source": rel.source,
                    "status": rel.status,
                    "sender_id": rel.sender_id,
                    "created_at": rel.created_at,
                    "updated_at": rel.updated_at,
                })
        return result
