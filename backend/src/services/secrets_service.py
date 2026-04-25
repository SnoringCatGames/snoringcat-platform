"""Secrets Manager service with Lambda container caching."""

import json
import boto3
from typing import Any, Optional

# Module-level cache for Lambda container reuse.
_cache: dict[str, Any] = {}
_client = None


def _get_client():
    """Lazy-initialize the Secrets Manager client."""
    global _client
    if _client is None:
        _client = boto3.client("secretsmanager")
    return _client


def get_secret_string(secret_name: str) -> str:
    """Retrieve a secret string, cached for container lifetime."""
    if secret_name not in _cache:
        response = _get_client().get_secret_value(
            SecretId=secret_name
        )
        _cache[secret_name] = response["SecretString"]
    return _cache[secret_name]


def get_secret_json(secret_name: str) -> dict:
    """Retrieve a secret as parsed JSON."""
    return json.loads(get_secret_string(secret_name))


def get_jwt_secret() -> str:
    """Get the JWT signing key."""
    return get_secret_string("hopnbop/jwt-signing-key")


def get_oauth_config(provider: str) -> dict:
    """Get OAuth config for a provider.

    Returns a dict with provider-specific fields like
    client_id, client_secret, etc. Returns empty dict if
    the secret contains no valid JSON.
    """
    try:
        return get_secret_json(f"hopnbop/oauth/{provider}")
    except (json.JSONDecodeError, Exception):
        return {}


def clear_cache() -> None:
    """Clear the secrets cache. Useful for testing."""
    _cache.clear()
