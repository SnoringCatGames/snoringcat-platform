"""Authentication service with OAuth2 provider integration."""

import logging
import os
import jwt
import httpx
from datetime import datetime, timedelta
from typing import Optional
from dataclasses import dataclass

from services import secrets_service

logger = logging.getLogger(__name__)


@dataclass
class AuthResult:
    """Result of authenticating with a provider.

    Contains the provider-specific user ID and display name,
    before mapping to a canonical player_id.
    """

    provider: str
    provider_id: str
    display_name: str
    profile_image_url: str = ""


@dataclass
class AuthToken:
    """JWT token with player information."""

    player_id: str
    display_name: str
    provider: str
    is_anonymous: bool
    issued_at: datetime
    expires_at: datetime
    is_guest: bool = False

    def to_jwt(self, secret: str) -> str:
        """Encode as JWT."""
        payload = {
            "sub": self.player_id,
            "name": self.display_name,
            "provider": self.provider,
            "anon": self.is_anonymous,
            "guest": self.is_guest,
            "iat": int(self.issued_at.timestamp()),
            "exp": int(self.expires_at.timestamp()),
        }
        return jwt.encode(payload, secret, algorithm="HS256")

    @classmethod
    def from_jwt(cls, token: str, secret: str) -> "AuthToken":
        """Decode and validate JWT."""
        try:
            payload = jwt.decode(
                token, secret, algorithms=["HS256"]
            )
        except jwt.ExpiredSignatureError:
            raise ValueError("Token expired")
        except jwt.InvalidTokenError:
            raise ValueError("Invalid token")

        return cls(
            player_id=payload["sub"],
            display_name=payload["name"],
            provider=payload["provider"],
            is_anonymous=payload.get("anon", False),
            issued_at=datetime.fromtimestamp(payload["iat"]),
            expires_at=datetime.fromtimestamp(payload["exp"]),
            is_guest=payload.get("guest", False),
        )


class AuthService:
    """OAuth2 provider integration."""

    # Providers that use Authorization Code flow and need
    # a redirect_uri for token exchange.
    BROWSER_PROVIDERS = {
        "google", "facebook", "apple",
    }

    def __init__(self, token_lifetime_hours: int = 24):
        self.token_lifetime = timedelta(
            hours=token_lifetime_hours
        )

    @property
    def jwt_secret(self) -> str:
        """Lazy-load JWT secret from Secrets Manager."""
        return secrets_service.get_jwt_secret()

    # --- Provider authentication ---

    async def authenticate(
        self,
        provider: str,
        auth_code: str,
        redirect_uri: str = "",
    ) -> AuthResult:
        """Dispatch to the correct provider authenticator.

        Returns an AuthResult with provider_id and display_name.
        The caller is responsible for mapping to a canonical
        player_id via ProviderMappingService.
        """
        if provider == "steam":
            return await self._auth_steam(auth_code)
        elif provider == "epic":
            return await self._auth_epic(auth_code)
        elif provider == "google":
            return await self._auth_google(
                auth_code, redirect_uri
            )
        elif provider == "facebook":
            return await self._auth_facebook(
                auth_code, redirect_uri
            )
        elif provider == "apple":
            return await self._auth_apple(
                auth_code, redirect_uri
            )
        else:
            raise ValueError(
                f"Unsupported provider: {provider}"
            )

    async def _auth_steam(
        self, steam_ticket: str
    ) -> AuthResult:
        """Validate Steam session ticket."""
        config = secrets_service.get_oauth_config("steam")
        api_key = config.get(
            "api_key",
            os.environ.get("STEAM_API_KEY", ""),
        )
        app_id = os.environ.get("STEAM_APP_ID", "")

        async with httpx.AsyncClient() as client:
            response = await client.get(
                "https://api.steampowered.com"
                "/ISteamUserAuth"
                "/AuthenticateUserTicket/v1/",
                params={
                    "key": api_key,
                    "appid": app_id,
                    "ticket": steam_ticket,
                },
            )

        if response.status_code != 200:
            raise ValueError("Steam authentication failed")

        data = response.json()
        params = (
            data.get("response", {}).get("params")
        )
        if not params:
            raise ValueError("Invalid Steam response")

        steam_id = params["steamid"]
        steam_info = await self._get_steam_player_information(
            api_key, steam_id
        )

        return AuthResult(
            provider="steam",
            provider_id=steam_id,
            display_name=steam_info["display_name"],
            profile_image_url=steam_info.get(
                "profile_image_url", ""
            ),
        )

    async def _auth_epic(
        self, access_token: str
    ) -> AuthResult:
        """Validate Epic Games OAuth token."""
        async with httpx.AsyncClient() as client:
            response = await client.get(
                "https://api.epicgames.dev"
                "/epic/oauth/v1/verify",
                headers={
                    "Authorization": f"Bearer {access_token}"
                },
            )

        if response.status_code != 200:
            raise ValueError("Epic authentication failed")

        data = response.json()
        epic_id = data["account_id"]
        display_name = data.get(
            "display_name", f"Player_{epic_id[:8]}"
        )

        return AuthResult(
            provider="epic",
            provider_id=epic_id,
            display_name=display_name,
        )

    async def _auth_google(
        self, auth_code: str, redirect_uri: str
    ) -> AuthResult:
        """Exchange Google auth code for user info."""
        config = secrets_service.get_oauth_config("google")
        client_id = config.get("client_id", "")
        client_secret = config.get("client_secret", "")

        async with httpx.AsyncClient() as client:
            response = await client.post(
                "https://oauth2.googleapis.com/token",
                data={
                    "code": auth_code,
                    "client_id": client_id,
                    "client_secret": client_secret,
                    "redirect_uri": redirect_uri,
                    "grant_type": "authorization_code",
                },
            )

        if response.status_code != 200:
            logger.error(
                "Google token exchange failed:"
                " status=%d body=%s",
                response.status_code,
                response.text,
            )
            raise ValueError(
                "Google token exchange failed"
            )

        tokens = response.json()
        id_token = tokens.get("id_token", "")

        # Decode id_token without verification for now.
        # Google id_tokens are signed JWTs. For production,
        # verify with Google JWKS.
        payload = jwt.decode(
            id_token,
            options={"verify_signature": False},
        )

        google_id = payload["sub"]
        display_name = payload.get(
            "name",
            payload.get(
                "email", f"Player_{google_id[:8]}"
            ),
        )
        profile_image_url = payload.get("picture", "")

        return AuthResult(
            provider="google",
            provider_id=google_id,
            display_name=display_name,
            profile_image_url=profile_image_url,
        )

    async def _auth_facebook(
        self, auth_code: str, redirect_uri: str
    ) -> AuthResult:
        """Exchange Facebook auth code for user info."""
        config = secrets_service.get_oauth_config("facebook")
        client_id = config.get("client_id", "")
        client_secret = config.get("client_secret", "")

        async with httpx.AsyncClient() as client:
            response = await client.get(
                "https://graph.facebook.com/v19.0"
                "/oauth/access_token",
                params={
                    "code": auth_code,
                    "client_id": client_id,
                    "client_secret": client_secret,
                    "redirect_uri": redirect_uri,
                },
            )

        if response.status_code != 200:
            logger.error(
                "Facebook token exchange failed:"
                " status=%d body=%s",
                response.status_code,
                response.text,
            )
            raise ValueError(
                "Facebook token exchange failed"
            )

        access_token = response.json()["access_token"]

        # Fetch user info.
        async with httpx.AsyncClient() as client:
            response = await client.get(
                "https://graph.facebook.com/v19.0/me",
                params={
                    "access_token": access_token,
                    "fields": "id,name,picture.type(large)",
                },
            )

        if response.status_code != 200:
            logger.error(
                "Facebook user info fetch failed:"
                " status=%d body=%s",
                response.status_code,
                response.text,
            )
            raise ValueError(
                "Facebook user info fetch failed"
            )

        user = response.json()
        fb_id = user["id"]
        display_name = user.get(
            "name", f"Player_{fb_id[:8]}"
        )
        profile_image_url = (
            user.get("picture", {})
            .get("data", {})
            .get("url", "")
        )

        return AuthResult(
            provider="facebook",
            provider_id=fb_id,
            display_name=display_name,
            profile_image_url=profile_image_url,
        )

    async def _auth_apple(
        self, auth_code: str, redirect_uri: str
    ) -> AuthResult:
        """Exchange Apple auth code for user info."""
        config = secrets_service.get_oauth_config("apple")
        client_id = config.get("client_id", "")
        team_id = config.get("team_id", "")
        key_id = config.get("key_id", "")
        private_key = config.get("private_key", "")

        # Generate client secret JWT for Apple.
        now = datetime.now()
        client_secret = jwt.encode(
            {
                "iss": team_id,
                "iat": int(now.timestamp()),
                "exp": int(
                    (now + timedelta(minutes=5)).timestamp()
                ),
                "aud": "https://appleid.apple.com",
                "sub": client_id,
            },
            private_key,
            algorithm="ES256",
            headers={"kid": key_id},
        )

        async with httpx.AsyncClient() as client:
            response = await client.post(
                "https://appleid.apple.com/auth/token",
                data={
                    "code": auth_code,
                    "client_id": client_id,
                    "client_secret": client_secret,
                    "redirect_uri": redirect_uri,
                    "grant_type": "authorization_code",
                },
            )

        if response.status_code != 200:
            raise ValueError(
                "Apple token exchange failed"
            )

        tokens = response.json()
        id_token = tokens.get("id_token", "")

        payload = jwt.decode(
            id_token,
            options={"verify_signature": False},
        )

        apple_id = payload["sub"]
        # Apple only sends email on first auth. Use it
        # as display name fallback.
        display_name = payload.get(
            "email", f"Player_{apple_id[:8]}"
        )

        return AuthResult(
            provider="apple",
            provider_id=apple_id,
            display_name=display_name,
        )

    # --- Token creation ---

    def create_auth_token(
        self,
        player_id: str,
        display_name: str,
        provider: str,
        is_anonymous: bool = False,
    ) -> AuthToken:
        """Create an AuthToken for an authenticated player."""
        now = datetime.now()
        return AuthToken(
            player_id=player_id,
            display_name=display_name,
            provider=provider,
            is_anonymous=is_anonymous,
            issued_at=now,
            expires_at=now + self.token_lifetime,
        )

    # --- Helpers ---

    async def _get_steam_player_information(
        self, api_key: str, steam_id: str
    ) -> dict:
        """Fetch Steam display name and avatar via ISteamUser API."""
        async with httpx.AsyncClient() as client:
            response = await client.get(
                "https://api.steampowered.com"
                "/ISteamUser/GetPlayerSummaries/v2/",
                params={
                    "key": api_key,
                    "steamids": steam_id,
                },
            )

        data = response.json()
        players = (
            data.get("response", {}).get("players", [])
        )
        if players:
            player = players[0]
            return {
                "display_name": player.get(
                    "personaname",
                    f"Player_{steam_id[:8]}",
                ),
                "profile_image_url": player.get(
                    "avatarfull", ""
                ),
            }

        return {
            "display_name": f"Player_{steam_id[:8]}",
            "profile_image_url": "",
        }
