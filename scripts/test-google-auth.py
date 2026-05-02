#!/usr/bin/env python3
"""Simulate the Phase D Google OAuth + Nakama auth flow end-to-end.

Bypasses Godot so we can iterate on auth bugs without rebuilding
the web/desktop client. Mirrors src/core/auth_client.gd's
_do_provider_auth path:

    1. Build Google OAuth URL with PKCE challenge.
    2. Open user's browser, capture redirect via loopback HTTP.
    3. Exchange code for id_token. Default mode posts to our
       Cloudflare Pages broker (matches the shipped client);
       --direct-google posts to Google directly with client_secret;
       --pkce-only posts directly without a secret (will 401 for
       Web-Application OAuth clients).
    4. POST returned id_token to Nakama
       /v2/account/authenticate/google.
    5. Print results from each step.

Usage:
    pip install httpx
    python scripts/test-google-auth.py                  # via broker (default, matches client)
    python scripts/test-google-auth.py --direct-google  # direct + client_secret (sanity)
    python scripts/test-google-auth.py --pkce-only      # direct, no secret (will fail for Web client)
    python scripts/test-google-auth.py --port 9876
    python scripts/test-google-auth.py --skip-nakama    # Stop after id_token
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import http.server
import secrets
import socketserver
import sys
import threading
import time
import urllib.parse
import webbrowser
from pathlib import Path

import httpx


NAKAMA_URL = "https://nakama.snoringcat.games"
BROKER_URL = "https://hopnbop.net/api/oauth/google/exchange"


# --------------------------------------------------------------
# Helpers
# --------------------------------------------------------------


def load_creds() -> dict[str, str]:
    p = Path.home() / ".hopnbop-migration" / "credentials.env"
    creds: dict[str, str] = {}
    for line in p.read_text().splitlines():
        if "=" in line and not line.lstrip().startswith("#"):
            k, _, v = line.partition("=")
            creds[k.strip()] = v.strip()
    return creds


def b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


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
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.end_headers()
            self.wfile.write(
                b"<!doctype html><meta charset=utf-8><title>Captured</title>"
                b"<body style='font-family:sans-serif;padding:2em'>"
                b"<h1>OAuth code captured.</h1>"
                b"<p>You can close this tab.</p></body>"
            )
        elif "error" in params:
            CallbackHandler.captured = {
                "error": params["error"][0],
                "error_description": params.get("error_description", [""])[0],
            }
            self.send_response(400)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.end_headers()
            self.wfile.write(
                f"<h1>OAuth error: {params['error'][0]}</h1>".encode()
            )
        else:
            self.send_response(400)
            self.end_headers()

    def log_message(self, *args, **kwargs) -> None:  # noqa: ARG002
        return  # Silence access logs


# --------------------------------------------------------------
# Main flow
# --------------------------------------------------------------


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument(
        "--direct-google",
        action="store_true",
        help=(
            "Skip the Pages broker; POST directly to Google with "
            "client_secret. Use this to sanity-check the OAuth client "
            "setup independent of the broker."
        ),
    )
    mode.add_argument(
        "--pkce-only",
        action="store_true",
        help=(
            "Skip the Pages broker; POST directly to Google without "
            "client_secret. Will 401 for OAuth clients of type "
            "'Web Application'."
        ),
    )
    parser.add_argument(
        "--port",
        type=int,
        default=9876,
        help="Loopback port (default 9876, matches auth_client.gd).",
    )
    parser.add_argument(
        "--skip-nakama",
        action="store_true",
        help="Stop after Google /token; don't call Nakama.",
    )
    args = parser.parse_args()

    creds = load_creds()
    client_id = creds.get("GOOGLE_OAUTH_CLIENT_ID", "")
    client_secret = creds.get("GOOGLE_OAUTH_CLIENT_SECRET", "")
    nakama_server_key = creds.get("NAKAMA_SERVER_KEY", "defaultkey")
    if not client_id:
        print("FAIL: GOOGLE_OAUTH_CLIENT_ID missing in credentials.env")
        return 1

    redirect_uri = f"http://127.0.0.1:{args.port}/"
    verifier = b64url(secrets.token_bytes(32))
    challenge = b64url(hashlib.sha256(verifier.encode()).digest())
    state = secrets.token_urlsafe(16)

    if args.direct_google:
        mode_label = "direct + client_secret"
    elif args.pkce_only:
        mode_label = "direct PKCE-only (will 401 for Web client)"
    else:
        mode_label = f"via broker ({BROKER_URL})"
    print(f"Mode: {mode_label}")
    print(f"Loopback: {redirect_uri}")
    print(f"Client ID: {client_id[:20]}...")
    print()

    # ---- 1. Start loopback server ----
    socketserver.TCPServer.allow_reuse_address = True
    server = socketserver.TCPServer(
        ("127.0.0.1", args.port), CallbackHandler
    )
    threading.Thread(target=server.serve_forever, daemon=True).start()

    auth_url = (
        "https://accounts.google.com/o/oauth2/v2/auth"
        f"?client_id={urllib.parse.quote(client_id)}"
        f"&redirect_uri={urllib.parse.quote(redirect_uri)}"
        "&response_type=code"
        f"&scope={urllib.parse.quote('openid profile email')}"
        f"&state={state}"
        f"&code_challenge={challenge}"
        "&code_challenge_method=S256"
    )

    # ---- 2. Open browser ----
    print("[1/4] Opening browser to Google OAuth...")
    print(f"      ({auth_url[:100]}...)")
    webbrowser.open(auth_url)

    deadline = time.time() + 300
    while not CallbackHandler.captured and time.time() < deadline:
        time.sleep(0.1)
    server.shutdown()

    if not CallbackHandler.captured:
        print("FAIL: timed out waiting for OAuth redirect")
        return 1
    if "error" in CallbackHandler.captured:
        print(
            f"FAIL: Google returned error: "
            f"{CallbackHandler.captured['error']} "
            f"({CallbackHandler.captured.get('error_description', '')})"
        )
        return 1
    code = CallbackHandler.captured["code"]
    received_state = CallbackHandler.captured["state"]
    if received_state != state:
        print(
            f"FAIL: state mismatch (sent={state} got={received_state})"
        )
        return 1
    print(f"[2/4] Captured code: {code[:20]}... (state OK)")

    # ---- 3. Token exchange ----
    print()
    if args.direct_google or args.pkce_only:
        endpoint = "https://oauth2.googleapis.com/token"
        print(f"[3/4] POST {endpoint}")
        form: dict[str, str] = {
            "code": code,
            "client_id": client_id,
            "redirect_uri": redirect_uri,
            "grant_type": "authorization_code",
            "code_verifier": verifier,
        }
        if args.direct_google:
            if not client_secret:
                print(
                    "FAIL: --direct-google set but "
                    "GOOGLE_OAUTH_CLIENT_SECRET missing"
                )
                return 1
            form["client_secret"] = client_secret
        r = httpx.post(endpoint, data=form, timeout=30.0)
    else:
        print(f"[3/4] POST {BROKER_URL}")
        r = httpx.post(
            BROKER_URL,
            json={
                "code": code,
                "redirect_uri": redirect_uri,
                "code_verifier": verifier,
            },
            timeout=30.0,
        )
    print(f"      HTTP {r.status_code}")
    if r.status_code != 200:
        print(f"      Body: {r.text}")
        return 1
    tok = r.json()
    id_token = tok.get("id_token", "")
    if not id_token:
        print(f"      Body has no id_token: {tok}")
        return 1
    print(f"      id_token: {id_token[:40]}...")
    print(f"      access_token: {tok.get('access_token', '')[:20]}...")
    print(f"      expires_in: {tok.get('expires_in')}")

    if args.skip_nakama:
        print()
        print("Stopping before Nakama (--skip-nakama).")
        return 0

    # ---- 4. Nakama authenticate_google ----
    print()
    print(f"[4/4] POST {NAKAMA_URL}/v2/account/authenticate/google")
    print(f"      server_key: {nakama_server_key[:8]}...")
    basic = base64.b64encode(
        f"{nakama_server_key}:".encode()
    ).decode()
    n = httpx.post(
        f"{NAKAMA_URL}/v2/account/authenticate/google?create=true",
        headers={
            "Authorization": f"Basic {basic}",
            "Content-Type": "application/json",
        },
        json={"token": id_token},
        timeout=30.0,
    )
    print(f"      HTTP {n.status_code}")
    if n.status_code != 200:
        print(f"      Body: {n.text}")
        return 1
    nakama_data = n.json()
    print(f"      session_token: {nakama_data.get('token', '')[:40]}...")
    refresh = nakama_data.get("refresh_token", "")
    if refresh:
        print(f"      refresh_token: {refresh[:40]}...")
    print()
    print("SUCCESS — full chain works end-to-end.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
