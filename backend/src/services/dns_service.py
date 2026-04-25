"""Route 53 DNS record management for game sessions.

When a web match is found, the backend creates an A record
under *.game.hopnbop.net pointing to the game server IP.
This lets web clients connect via WSS with a valid TLS
certificate (the wildcard cert for *.game.hopnbop.net).
"""

import os
import re
import boto3
from botocore.exceptions import ClientError
from aws_lambda_powertools import Logger

logger = Logger(child=True)

_HOSTED_ZONE_ID = os.environ.get("HOSTED_ZONE_ID", "")
_GAME_DOMAIN = "game.hopnbop.net"
_RECORD_TTL = 30


class DnsService:
    """Manages Route 53 A records for game sessions."""

    def __init__(self, region: str = "us-west-2"):
        self.route53 = boto3.client(
            "route53", region_name=region
        )

    def create_game_session_record(
        self, session_id: str, server_ip: str
    ) -> str:
        """Create an A record for a game session.

        Returns the hostname (e.g.,
        gsess-abc123.game.hopnbop.net).
        """
        label = _sanitize_dns_label(session_id)
        hostname = f"{label}.{_GAME_DOMAIN}"

        try:
            self.route53.change_resource_record_sets(
                HostedZoneId=_HOSTED_ZONE_ID,
                ChangeBatch={
                    "Changes": [
                        {
                            "Action": "UPSERT",
                            "ResourceRecordSet": {
                                "Name": hostname,
                                "Type": "A",
                                "TTL": _RECORD_TTL,
                                "ResourceRecords": [
                                    {"Value": server_ip}
                                ],
                            },
                        }
                    ],
                },
            )
            logger.info(
                "Created DNS record",
                extra={
                    "hostname": hostname,
                    "server_ip": server_ip,
                },
            )
        except ClientError:
            logger.exception(
                "Failed to create DNS record",
                extra={"hostname": hostname},
            )
            raise

        return hostname

    def delete_game_session_record(
        self, session_id: str, server_ip: str
    ) -> None:
        """Delete the A record for a completed game session.

        Best-effort. Swallows errors since stale records
        are harmless (they point to IPs that no longer
        accept connections).
        """
        label = _sanitize_dns_label(session_id)
        hostname = f"{label}.{_GAME_DOMAIN}"

        try:
            self.route53.change_resource_record_sets(
                HostedZoneId=_HOSTED_ZONE_ID,
                ChangeBatch={
                    "Changes": [
                        {
                            "Action": "DELETE",
                            "ResourceRecordSet": {
                                "Name": hostname,
                                "Type": "A",
                                "TTL": _RECORD_TTL,
                                "ResourceRecords": [
                                    {"Value": server_ip}
                                ],
                            },
                        }
                    ],
                },
            )
            logger.info(
                "Deleted DNS record",
                extra={"hostname": hostname},
            )
        except ClientError:
            logger.warning(
                "Failed to delete DNS record"
                " (best-effort cleanup)",
                extra={"hostname": hostname},
            )


def _sanitize_dns_label(session_id: str) -> str:
    """Convert a GameLift session ID to a valid DNS label.

    GameLift session IDs look like:
      arn:aws:gamelift:...:gamesession/fleet-xxx/gsess-xxx

    Extracts the final segment and normalizes it for DNS
    (lowercase, alphanumeric + hyphens, max 63 characters).
    """
    # Extract the last path segment.
    label = session_id.rsplit("/", 1)[-1]
    # Replace non-alphanumeric chars (except hyphens)
    # with hyphens.
    label = re.sub(r"[^a-z0-9-]", "-", label.lower())
    # Collapse consecutive hyphens.
    label = re.sub(r"-+", "-", label)
    # Strip leading/trailing hyphens.
    label = label.strip("-")
    # DNS labels are max 63 characters.
    return label[:63]
