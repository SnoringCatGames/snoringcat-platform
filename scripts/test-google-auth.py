#!/usr/bin/env python3
"""Check the Google sign-in chain end to end, from a real browser login.

Runs the steps the SDK's desktop Google sign-in runs, without a Godot
build in the loop (addons/snoringcat_platform_client/core/
auth_api_client.gd: _start_browser_oauth, _poll_oauth_redirect,
_exchange_google_code_for_id_token, _do_provider_auth):

    1. Open Google's consent page with a PKCE challenge and capture the
       redirect on the client's loopback redirect URI,
       http://127.0.0.1:9876. Google matches it exactly: a trailing
       slash or another port gets redirect_uri_mismatch.
    2. Exchange the code for an id_token. The default posts to the
       game's Cloudflare Pages broker, as the client does.
       --direct-google posts to Google with GOOGLE_OAUTH_CLIENT_SECRET,
       to tell a broker fault from an OAuth-client fault. --pkce-only
       posts to Google with no secret, which Google refuses for a Web
       Application client (a negative control).
    3. Sign in to Nakama with the id_token through
       /v2/account/authenticate/google, passing vars.game_id. The
       runtime's BeforeAuthenticateGoogle hook refuses a sign-in
       without a registered game_id.

Nothing else covers this chain. The SDK compliance suite signs in with
device ids, and a Google sign-in needs a person at a browser. Run this
after changing the OAuth client or its redirect URIs, the broker, or
the broker's GOOGLE_OAUTH_CLIENT_SECRET Pages secret.

Step 3 signs in to production as the Google account you pick, and
creates that account if it has none, exactly as the game does.
--skip-nakama stops after step 2. The output shows token claims, never
a token.

Reads GOOGLE_OAUTH_CLIENT_ID, NAKAMA_SERVER_KEY, and (for
--direct-google only) GOOGLE_OAUTH_CLIENT_SECRET from
~/.hopnbop-migration/credentials.env. Standard library only.

Usage:
    python scripts/test-google-auth.py                  # as the client does
    python scripts/test-google-auth.py --direct-google  # skip the broker
    python scripts/test-google-auth.py --pkce-only      # negative control
    python scripts/test-google-auth.py --skip-nakama    # stop after step 2
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import html
import http.server
import json
import secrets
import socketserver
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser
from pathlib import Path


NAKAMA_URL = "https://nakama.snoringcat.games"

# The consuming game's values, as its client passes them
# (hopnbop/src/core/global.gd): its registered game_id and its
# Cloudflare Pages token broker (hopnbop/web/functions/api/oauth/
# google/exchange.js).
GAME_ID = "hopnbop"
BROKER_URL = "https://hopnbop.net/api/oauth/google/exchange"

GOOGLE_AUTH_URL = "https://accounts.google.com/o/oauth2/v2/auth"
GOOGLE_TOKEN_URL = "https://oauth2.googleapis.com/token"

# auth_api_client.gd's _LOOPBACK_HOST and _LOOPBACK_PORT. The redirect
# URI has no trailing slash, as in the client.
LOOPBACK_HOST = "127.0.0.1"
LOOPBACK_PORT = 9876
REDIRECT_URI = f"http://{LOOPBACK_HOST}:{LOOPBACK_PORT}"

REDIRECT_TIMEOUT_SEC = 300
HTTP_TIMEOUT_SEC = 30
USER_AGENT = "snoringcat-platform-test-google-auth"

CREDENTIALS_PATH = Path.home() / ".hopnbop-migration" / "credentials.env"


# --------------------------------------------------------------
# Helpers
# --------------------------------------------------------------


def load_creds() -> dict[str, str]:
    creds: dict[str, str] = {}
    for line in CREDENTIALS_PATH.read_text().splitlines():
        if "=" in line and not line.lstrip().startswith("#"):
            key, _, value = line.partition("=")
            creds[key.strip()] = value.strip()
    return creds


def b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def jwt_claims(token: str) -> dict:
    """Returns a JWT's payload claims, unverified, or {} if malformed."""
    parts = token.split(".")
    if len(parts) != 3:
        return {}
    try:
        payload = base64.urlsafe_b64decode(
            parts[1] + "=" * (-len(parts[1]) % 4))
        claims = json.loads(payload)
    except ValueError:
        return {}
    return claims if isinstance(claims, dict) else {}


def post(
    url: str,
    *,
    json_body: dict | None = None,
    form: dict[str, str] | None = None,
    headers: dict[str, str] | None = None,
) -> tuple[int, str]:
    """POSTs a JSON or form body. Returns (status, body), errors too."""
    if json_body is not None:
        data = json.dumps(json_body).encode()
        content_type = "application/json"
    else:
        data = urllib.parse.urlencode(form or {}).encode()
        content_type = "application/x-www-form-urlencoded"
    request = urllib.request.Request(
        url,
        data=data,
        method="POST",
        headers={
            "Content-Type": content_type,
            "User-Agent": USER_AGENT,
            **(headers or {}),
        },
    )
    try:
        with urllib.request.urlopen(
            request, timeout=HTTP_TIMEOUT_SEC
        ) as response:
            return response.status, response.read().decode(
                "utf-8", "replace")
    except urllib.error.HTTPError as err:
        return err.code, err.read().decode("utf-8", "replace")


def build_auth_url(client_id: str, state: str, challenge: str) -> str:
    """Google's consent URL, with the parameters the client sends."""
    query = urllib.parse.urlencode(
        {
            "client_id": client_id,
            "redirect_uri": REDIRECT_URI,
            "response_type": "code",
            # The SDK's scopes: no email, which nothing on the
            # platform uses.
            "scope": "openid profile",
            "state": state,
            "code_challenge": challenge,
            "code_challenge_method": "S256",
        },
        quote_via=urllib.parse.quote,
    )
    return f"{GOOGLE_AUTH_URL}?{query}"


def exchange_code(
    mode: str,
    code: str,
    verifier: str,
    client_id: str,
    client_secret: str = "",
) -> tuple[int, str]:
    """Trades an auth code for Google's token response.

    mode is "broker" (as the client does), "direct" (Google, with the
    client secret), or "pkce" (Google, no secret).
    """
    if mode == "broker":
        return post(
            BROKER_URL,
            json_body={
                "code": code,
                "redirect_uri": REDIRECT_URI,
                "code_verifier": verifier,
            },
        )
    form = {
        "code": code,
        "client_id": client_id,
        "redirect_uri": REDIRECT_URI,
        "grant_type": "authorization_code",
        "code_verifier": verifier,
    }
    if mode == "direct":
        form["client_secret"] = client_secret
    return post(GOOGLE_TOKEN_URL, form=form)


def authenticate_nakama(server_key: str, id_token: str) -> tuple[int, str]:
    """Signs in to Nakama with a Google id_token, as the client does."""
    basic = base64.b64encode(f"{server_key}:".encode()).decode()
    return post(
        f"{NAKAMA_URL}/v2/account/authenticate/google?create=true",
        headers={"Authorization": f"Basic {basic}"},
        json_body={"token": id_token, "vars": {"game_id": GAME_ID}},
    )


# --------------------------------------------------------------
# Loopback callback server
# --------------------------------------------------------------


class CallbackHandler(http.server.BaseHTTPRequestHandler):
    captured: dict[str, str] = {}

    def do_GET(self) -> None:  # noqa: N802
        parsed = urllib.parse.urlparse(self.path)
        params = urllib.parse.parse_qs(parsed.query)
        if "code" in params:
            CallbackHandler.captured = {
                "code": params["code"][0],
                "state": params.get("state", [""])[0],
            }
            self._reply(200, "OAuth code captured. You can close this tab.")
        elif "error" in params:
            CallbackHandler.captured = {
                "error": params["error"][0],
                "error_description": params.get(
                    "error_description", [""])[0],
            }
            self._reply(400, f"OAuth error: {params['error'][0]}")
        else:
            self.send_response(400)
            self.end_headers()

    def _reply(self, status: int, message: str) -> None:
        self.send_response(status)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.end_headers()
        self.wfile.write(
            (
                "<!doctype html><meta charset=utf-8><title>test-google-auth"
                "</title><body style='font-family:sans-serif;padding:2em'>"
                f"<h1>{html.escape(message)}</h1></body>"
            ).encode()
        )

    def log_message(self, *args, **kwargs) -> None:  # noqa: ARG002
        return  # Silence access logs.


# --------------------------------------------------------------
# Main flow
# --------------------------------------------------------------


def main() -> int:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    mode_group = parser.add_mutually_exclusive_group()
    mode_group.add_argument(
        "--direct-google",
        action="store_true",
        help=(
            "Skip the broker; POST to Google with client_secret. Tells a "
            "broker fault from an OAuth-client fault."
        ),
    )
    mode_group.add_argument(
        "--pkce-only",
        action="store_true",
        help=(
            "Skip the broker; POST to Google without client_secret. "
            "Google refuses this for a Web Application client."
        ),
    )
    parser.add_argument(
        "--skip-nakama",
        action="store_true",
        help="Stop after the token exchange; don't sign in to Nakama.",
    )
    args = parser.parse_args()

    creds = load_creds()
    client_id = creds.get("GOOGLE_OAUTH_CLIENT_ID", "")
    client_secret = creds.get("GOOGLE_OAUTH_CLIENT_SECRET", "")
    server_key = creds.get("NAKAMA_SERVER_KEY", "")
    missing = [
        name
        for name, value, needed in (
            ("GOOGLE_OAUTH_CLIENT_ID", client_id, True),
            ("GOOGLE_OAUTH_CLIENT_SECRET", client_secret, args.direct_google),
            ("NAKAMA_SERVER_KEY", server_key, not args.skip_nakama),
        )
        if needed and not value
    ]
    if missing:
        print(f"FAIL: missing in {CREDENTIALS_PATH}: {', '.join(missing)}")
        return 1

    if args.direct_google:
        mode, mode_label = "direct", "Google directly, with client_secret"
    elif args.pkce_only:
        mode, mode_label = "pkce", "Google directly, no secret (should fail)"
    else:
        mode, mode_label = "broker", f"broker ({BROKER_URL})"
    print(f"Token exchange: {mode_label}")
    print(f"Redirect URI:   {REDIRECT_URI}")
    print(f"Game id:        {GAME_ID}")
    print()

    verifier = b64url(secrets.token_bytes(32))
    challenge = b64url(hashlib.sha256(verifier.encode()).digest())
    state = secrets.token_urlsafe(16)

    # ---- 1. Consent and loopback redirect ----
    socketserver.TCPServer.allow_reuse_address = True
    try:
        server = socketserver.TCPServer(
            (LOOPBACK_HOST, LOOPBACK_PORT), CallbackHandler)
    except OSError as err:
        print(f"FAIL: can't listen on {REDIRECT_URI}: {err}")
        return 1
    threading.Thread(target=server.serve_forever, daemon=True).start()

    # The URL carries the public client id and the PKCE challenge,
    # never the verifier.
    auth_url = build_auth_url(client_id, state, challenge)
    print("[1/3] Opening the browser to Google's consent page. If it")
    print(f"      doesn't open, visit:\n      {auth_url}")
    webbrowser.open(auth_url)

    deadline = time.time() + REDIRECT_TIMEOUT_SEC
    while not CallbackHandler.captured and time.time() < deadline:
        time.sleep(0.1)
    server.shutdown()

    captured = CallbackHandler.captured
    if not captured:
        print(
            f"FAIL: no redirect within {REDIRECT_TIMEOUT_SEC}s. If Google "
            "showed redirect_uri_mismatch, the OAuth client no longer "
            f"lists {REDIRECT_URI}."
        )
        return 1
    if "error" in captured:
        print(
            f"FAIL: Google returned {captured['error']}"
            f" ({captured.get('error_description', '')})"
        )
        return 1
    if captured["state"] != state:
        print("FAIL: state mismatch on the redirect")
        return 1
    print("      Captured the code (state OK).")

    # ---- 2. Code for id_token ----
    print()
    print(f"[2/3] Exchanging the code: {mode_label}")
    status, body = exchange_code(
        mode, captured["code"], verifier, client_id, client_secret)
    print(f"      HTTP {status}")
    if status != 200:
        # Google's and the broker's errors carry no tokens.
        print(f"      Body: {body}")
        return 1
    try:
        token_response = json.loads(body)
    except ValueError:
        print("      FAIL: the response isn't JSON")
        return 1
    id_token = str(token_response.get("id_token", ""))
    if not id_token:
        print(
            "      FAIL: no id_token; response fields: "
            f"{sorted(token_response)}"
        )
        return 1
    google_claims = jwt_claims(id_token)
    # Nakama's CheckGoogleToken refuses a token that lacks any of
    # these; Google documents all four as present in every ID token.
    missing = [
        claim for claim in ("iss", "sub", "azp", "aud")
        if not google_claims.get(claim)
    ]
    print(
        "      id_token"
        f" (aud matches client: {google_claims.get('aud') == client_id},"
        f" claims Nakama needs missing: {missing or 'none'},"
        f" email claim present: {'email' in google_claims},"
        f" expires in {token_response.get('expires_in')}s)"
    )
    if missing:
        print("      FAIL: Nakama would refuse this id_token")
        return 1

    if args.skip_nakama:
        print()
        print("Stopping before Nakama (--skip-nakama).")
        return 0

    # ---- 3. Nakama sign-in ----
    print()
    print(f"[3/3] POST {NAKAMA_URL}/v2/account/authenticate/google")
    status, body = authenticate_nakama(server_key, id_token)
    print(f"      HTTP {status}")
    if status != 200:
        # Nakama's errors carry no tokens.
        print(f"      Body: {body}")
        return 1
    try:
        session = json.loads(body)
    except ValueError:
        print("      FAIL: the response isn't JSON")
        return 1
    session_claims = jwt_claims(str(session.get("token", "")))
    session_vars = session_claims.get("vrs") or {}
    print(
        f"      Signed in as user {session_claims.get('uid', '?')}"
        f" ({session_claims.get('usn', '?')}),"
        f" new account: {bool(session.get('created'))}"
    )
    print(f"      Session vars: {session_vars}")
    if session_vars.get("game_id") != GAME_ID:
        print(f"FAIL: the session doesn't carry game_id={GAME_ID}")
        return 1
    if not session.get("refresh_token"):
        print("FAIL: no refresh_token in the session")
        return 1
    print()
    print("SUCCESS: the whole chain works.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
