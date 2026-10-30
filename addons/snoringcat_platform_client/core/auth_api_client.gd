class_name PlatformAuthApiClient
extends Node
## Nakama-backed authentication client.
##
## Handles login (OAuth + anonymous), token refresh, account
## linking / unlinking, account deletion, and player-data
## export. Supports three OAuth flows:
## - Loopback: Desktop clients open browser, local TCP server
##   captures redirect (Google, Facebook).
## - Popup: Web builds open a popup window, a static callback page
##   sends the code via BroadcastChannel.
## - Platform: Steam/Epic provide tokens via their SDK; the caller
##   submits the ticket directly.
##
## Reads `Platform.{nakama_host, nakama_port, nakama_scheme,
## nakama_server_key, oauth_callback_url, google_token_broker_url,
## google_oauth_client_id, facebook_oauth_client_id}`,
## `Platform.token_store`, and the shared
## `Platform.get_nakama_client()` / `Platform.build_session_
## from_store()` helpers. All must be populated before any auth
## method is called (the consuming game's bootstrap ensures this
## by calling `Platform.initialize(...)` with the full config).


## Emitted on successful authentication.
signal auth_completed(success: bool, error: String)

## Emitted when this client signs the player out on its own: Nakama
## rejected the stored refresh token (invalid or expired, account
## banned or gone), or the stored one could not be used, and the
## token store's session has been cleared. The player's consent stays
## stored (PlatformAuthTokenStore.clear_session). Fires right after
## that refresh's auth_completed(false, error), so code awaiting the
## refresh finishes first. Not emitted when game code clears the token
## store itself. Games send the player back to sign-in.
signal signed_out(error: String)

## Emitted when account linking completes.
signal link_completed(
	success: bool,
	error: String,
	provider: String,
)

## Emitted when account unlinking completes.
signal unlink_completed(
	success: bool,
	error: String,
	provider: String,
)

## Emitted when account deletion completes.
signal delete_completed(success: bool, error: String)

## Emitted when data export completes. Save last_export_json, not a
## re-encoding of `data`.
signal export_completed(
	success: bool,
	error: String,
	data: Dictionary,
)

## Emitted when cancel_account_deletion() completes.
signal deletion_cancel_completed(success: bool, error: String)

## Emitted when an ephemeral guest JWT has been
## obtained (or failed) for an anonymous player.
signal guest_jwt_obtained(success: bool, error: String)

## Status updates for UI feedback.
signal auth_status_changed(status: String)

## Emitted when backend reports a game version mismatch.
signal version_mismatch(
	client_version: String,
	server_version: String,
)

enum Provider {
	STEAM,
	EPIC,
	GOOGLE,
	FACEBOOK,
	APPLE,
	ANONYMOUS,
}

## Platforms where auth is implied (SDK provides token).
const PLATFORM_PROVIDERS := [
	Provider.STEAM,
	Provider.EPIC,
]

const _LOOPBACK_PORT := 9876
const _LOOPBACK_HOST := "127.0.0.1"
## Providers that require HTTPS redirect URIs and cannot
## use the direct loopback flow on desktop. These use a
## two-hop pattern: redirect to the hosted callback page
## first, which then forwards to the loopback server.
const _HTTPS_REDIRECT_PROVIDERS := [
	Provider.FACEBOOK,
]
const _REFRESH_COOLDOWN_SEC := 60.0
## Backoff between automatic refresh attempts after a failure that
## may be transient (network error, timeout, 5xx). It doubles with
## each consecutive failure, from the initial delay up to the max,
## and resets once a session is stored.
const _REFRESH_RETRY_INITIAL_DELAY_SEC := 5.0
const _REFRESH_RETRY_MAX_DELAY_SEC := 300.0
const _REFRESH_RETRY_BACKOFF_MULTIPLIER := 2.0
## gRPC status codes Nakama puts in its error responses.
const _GRPC_NOT_FOUND := 5
const _GRPC_PERMISSION_DENIED := 7
const _GRPC_UNAUTHENTICATED := 16
## Codes with which Nakama rejects a session refresh for good: the
## refresh token is invalid or expired, the account is banned, or
## the account no longer exists. Retrying cannot turn any of them
## into a session.
const _REFRESH_REJECTED_GRPC_CODES := [
	_GRPC_UNAUTHENTICATED,
	_GRPC_PERMISSION_DENIED,
	_GRPC_NOT_FOUND,
]

# Endpoint constants are opaque dispatch tags consumed by
# _send_auth_request to pick the matching Nakama call. Their
# string values are kept stable to minimize call-site churn.
const _AUTH_ENDPOINT := "/auth/login"
const _ANON_ENDPOINT := "/auth/anon"
const _GUEST_ENDPOINT := "/auth/guest"
const _REFRESH_ENDPOINT := "/auth/refresh"
const _LINK_ENDPOINT := "/auth/link"
const _UNLINK_ENDPOINT := "/auth/unlink"
const _DELETE_ACCOUNT_ENDPOINT := "/auth/account"
const _EXPORT_ENDPOINT := "/player/export"
const _POPUP_TIMEOUT_SEC := 300.0
const _CANCEL_DELETION_RPC := "cancel_account_deletion"
## OAuth scopes requested from Google, URL-encoded. `openid` gets the
## ID token, whose `sub` claim is the account id Nakama signs in
## with, and `profile` its `name` and `picture`. No `email`: nothing
## on the platform uses one, so the game doesn't ask for it.
const _GOOGLE_OAUTH_SCOPE := "openid%20profile"

## Maps Provider enum to string name sent to backend.
const _PROVIDER_NAMES := {
	Provider.STEAM: "steam",
	Provider.EPIC: "epic",
	Provider.GOOGLE: "google",
	Provider.FACEBOOK: "facebook",
	Provider.APPLE: "apple",
	Provider.ANONYMOUS: "anonymous",
}

## Providers that use browser-based OAuth flow.
const _BROWSER_PROVIDERS := [
	Provider.GOOGLE,
	Provider.FACEBOOK,
]

## The last export's JSON text, exactly as the server sent it. Set
## before export_completed(true, ...) fires and cleared when an export
## starts. Save this rather than re-encoding `data`: Godot's JSON
## parser turns every number into a float, so a re-encoded export
## reads 1500.0 for a score of 1500 and rounds long ids.
var last_export_json := ""

var _http_request: HTTPRequest
var _tcp_server: TCPServer
var _oauth_state: String
var _oauth_provider: Provider
var _is_awaiting_oauth := false
var _last_refresh_time := 0.0
var _is_refreshing := false
# Unix time before which _check_auto_refresh won't retry a failed
# refresh, and the wait the next transient failure will impose.
var _refresh_retry_time := 0.0
var _refresh_retry_delay_sec := _REFRESH_RETRY_INITIAL_DELAY_SEC

# PKCE verifier for the in-flight Google OAuth attempt. Set when
# the OAuth URL is built, consumed by the post-callback token
# exchange. Cleared after use.
var _pkce_verifier := ""

# Account linking state.
var _is_linking := false
var _link_provider_name := ""

# Account unlinking state.
var _is_unlinking := false
var _unlink_provider_name := ""

# Account deletion state.
var _is_deleting := false
var _is_cancelling_deletion := false

# The display name and picture from the ID token of the latest
# Google sign-in, with the user_id it signed in to, in memory only.
# A deletion request clears the profile on the server and keeps no
# copy, so cancel_account_deletion() puts these back.
var _provider_profile: Dictionary = {}

# Data export state.
var _is_exporting := false
var _export_http_request: HTTPRequest

# Popup OAuth state (web platform).
var _is_awaiting_web_oauth := false
var _popup_timeout_timer: Timer


func _ready() -> void:
	_http_request = HTTPRequest.new()
	_http_request.timeout = 30.0
	add_child(_http_request)

	_export_http_request = HTTPRequest.new()
	_export_http_request.timeout = 60.0
	add_child(_export_http_request)


func _process(_delta: float) -> void:
	if _is_awaiting_oauth and _tcp_server != null:
		_poll_oauth_redirect()

	if _is_awaiting_web_oauth:
		_poll_web_oauth()

	_check_auto_refresh()


## Returns true if running in a web browser.
static func is_web_platform() -> bool:
	return OS.has_feature("web")


## Returns the implied platform provider, or -1 if none.
static func get_platform_provider() -> int:
	if OS.has_feature("steam"):
		return Provider.STEAM
	# Epic detection would go here.
	return -1


## Start login flow for the given provider.
func login_with_provider(provider: Provider) -> void:
	if provider == Provider.ANONYMOUS:
		login_anonymous()
		return

	if provider in _BROWSER_PROVIDERS:
		if is_web_platform():
			_start_web_oauth(provider)
		else:
			_start_browser_oauth(provider)
	else:
		# Steam and Epic send platform tokens directly.
		auth_status_changed.emit(
			"Waiting for %s token..."
			% _PROVIDER_NAMES[provider]
		)


## Submit a platform token (Steam ticket or Epic
## access token) directly.
func submit_platform_token(
	provider: Provider,
	token: String,
) -> void:
	auth_status_changed.emit("Authenticating...")
	var body := {
		"provider": _PROVIDER_NAMES[provider],
		"auth_code": token,
		"consent_accepted_at": (
			Platform.token_store.consent_accepted_at
		),
		"consent_legal_version": (
			Platform.token_store.consent_legal_version
		),
	}
	_send_auth_request(_AUTH_ENDPOINT, body)


## Anonymous login — creates a local-only identity
## without any backend call. No JWT is issued here.
## An ephemeral guest JWT is obtained on-demand via
## get_guest_jwt() when the player starts matchmaking.
func login_anonymous() -> void:
	var store: PlatformAuthTokenStore = Platform.token_store
	store.is_anonymous = true
	store.display_name = _generate_anonymous_name()
	store.local_player_id = _generate_state_nonce()
	store.save_tokens()
	auth_completed.emit(true, "")


## Obtain a Nakama session for the current anonymous
## player. The session token is stored in
## Platform.token_store. Emits guest_jwt_obtained when
## complete. If a valid token already exists, emits
## immediately.
func get_guest_jwt() -> void:
	if Platform.token_store.is_token_valid():
		guest_jwt_obtained.emit(true, "")
		return
	# _do_anon_auth runs Nakama device auth and emits
	# guest_jwt_obtained itself.
	await _do_anon_auth()


## Refresh the current JWT using the stored refresh
## token. Emits auth_completed when done. If Nakama
## rejects the refresh token (invalid or expired,
## account banned or gone), the stored session is
## cleared first, keeping the player's consent, which
## leaves the player signed out, and signed_out
## follows auth_completed.
func refresh_token() -> void:
	if _is_refreshing:
		return

	var store: PlatformAuthTokenStore = Platform.token_store
	if store.refresh_token.is_empty():
		auth_completed.emit(false, "No refresh token")
		return

	_is_refreshing = true
	auth_status_changed.emit("Refreshing session...")
	var body := {
		"player_id": store.player_id,
		"refresh_token": store.refresh_token,
	}
	_send_auth_request(_REFRESH_ENDPOINT, body)


## Guests get a server session before account operations
## that need one (H57/H58): link, export, and delete act on
## the guest's device account, which a local-only guest has
## not authenticated yet. Non-guests (and guests already
## holding a session) return at once. Returns whether a
## valid session exists afterwards.
func _ensure_guest_session() -> bool:
	if not Platform.token_store.is_anonymous:
		return true
	await get_guest_jwt()
	return Platform.token_store.is_token_valid()


## _ensure_guest_session for the link flow, whose failure
## routes through _emit_failure like every other link error.
func _ensure_guest_session_for_link() -> bool:
	if await _ensure_guest_session():
		return true
	_emit_failure("Could not start a guest session")
	return false


## Link a new OAuth provider to the current account.
func link_provider(provider: Provider) -> void:
	print(
		"[PlatformAuthApiClient] link_provider: %s"
		% _PROVIDER_NAMES[provider]
	)
	_is_linking = true
	_link_provider_name = _PROVIDER_NAMES[provider]
	if not await _ensure_guest_session_for_link():
		return
	if provider in _BROWSER_PROVIDERS:
		if is_web_platform():
			_start_web_oauth(provider)
		else:
			_start_browser_oauth(provider, true)
	else:
		auth_status_changed.emit(
			"Use submit_platform_link() for %s"
			% _PROVIDER_NAMES[provider]
		)


## Submit a platform token for account linking.
func submit_platform_link(
	provider: Provider,
	token: String,
) -> void:
	_is_linking = true
	_link_provider_name = _PROVIDER_NAMES[provider]
	auth_status_changed.emit("Linking account...")
	var body := {
		"provider": _PROVIDER_NAMES[provider],
		"auth_code": token,
	}
	_send_auth_request(
		_LINK_ENDPOINT, body, true
	)


## Unlink a provider from the current account.
func unlink_provider(provider: Provider) -> void:
	_is_unlinking = true
	_unlink_provider_name = _PROVIDER_NAMES[provider]
	# Nakama refuses to unlink an account's last sign-in
	# identity; answer locally with the code callers expect
	# (LK-3). The device identity ("anonymous") counts, so a
	# linked guest can always drop a provider again.
	var remaining: Array = (
		Platform.token_store.linked_providers.duplicate())
	remaining.erase(_PROVIDER_NAMES[provider])
	if remaining.is_empty():
		_emit_failure("LAST_PROVIDER")
		return
	auth_status_changed.emit("Unlinking account...")
	var body := {
		"provider": _PROVIDER_NAMES[provider],
	}
	_send_auth_request(
		_UNLINK_ENDPOINT, body, true
	)


## Delete the current account and all associated data.
func delete_account() -> void:
	if _is_deleting:
		return
	_is_deleting = true
	auth_status_changed.emit("Deleting account...")
	# Deletion applies to a guest's device account too
	# (H58); a local-only guest needs the session first.
	if not Platform.token_store.is_token_valid():
		await _ensure_guest_session()
	if not Platform.token_store.is_token_valid():
		_emit_failure("Not authenticated")
		return
	_send_delete_request()


## Cancel the signed-in player's pending account deletion. The
## deletion request cleared the profile on the server and kept no
## copy, so on success this puts back the display name and profile
## image from the player's Google sign-in this session, when there
## was one, and then refreshes the token store's profile from the
## account. Emits deletion_cancel_completed.
func cancel_account_deletion() -> void:
	if _is_cancelling_deletion:
		return
	var session := Platform.build_session_from_store()
	if session == null:
		deletion_cancel_completed.emit(false, "Not authenticated")
		return
	_is_cancelling_deletion = true
	var rpc_result: NakamaAPI.ApiRpc = (
		await Platform.get_nakama_client().rpc_async(
			session, _CANCEL_DELETION_RPC, ""))
	if rpc_result.is_exception():
		_is_cancelling_deletion = false
		deletion_cancel_completed.emit(
			false, _describe_nakama_exception(rpc_result.get_exception()))
		return
	if str(_provider_profile.get("user_id", "")) == session.user_id:
		await _apply_provider_profile(session, _provider_profile)
	var profile := await _fetch_account_profile_dict(session)
	if not profile.is_empty():
		Platform.token_store.display_name = str(
			profile.get("display_name", ""))
		Platform.token_store.profile_image_url = str(
			profile.get("profile_image_url", ""))
		Platform.token_store.save_tokens()
	_is_cancelling_deletion = false
	deletion_cancel_completed.emit(true, "")


## Export all player data as JSON. Routes through the Nakama
## runtime RPC `export_player_data` (registered in
## snoringcat-platform's runtime/data_export.go). On success the
## server's text is in last_export_json.
func export_player_data() -> void:
	if _is_exporting:
		return
	last_export_json = ""

	if not Platform.token_store.is_token_valid():
		# A local-only guest has no session yet; export
		# still applies to their account (H58).
		await _ensure_guest_session()
	if not Platform.token_store.is_token_valid():
		push_warning(
			"[PlatformAuthApiClient] Export failed: not authenticated",
		)
		export_completed.emit(
			false, "Not authenticated", {},
		)
		return

	_is_exporting = true

	var session := Platform.build_session_from_store()
	if session == null:
		_is_exporting = false
		export_completed.emit(false, "Not authenticated", {})
		return

	var rpc_result: NakamaAPI.ApiRpc = (
		await Platform.get_nakama_client().rpc_async(
			session, "export_player_data", "{}"))
	_is_exporting = false
	if rpc_result.is_exception():
		var ex := rpc_result.get_exception()
		export_completed.emit(
			false, _describe_nakama_exception(ex), {})
		return
	var json := JSON.new()
	if json.parse(rpc_result.payload) == OK and json.data is Dictionary:
		last_export_json = rpc_result.payload
		export_completed.emit(true, "", json.data)
	else:
		export_completed.emit(
			false, "Invalid export payload", {})


# =============================================================
# Desktop loopback OAuth flow
# =============================================================


func _start_browser_oauth(
	provider: Provider,
	_is_link := false,
) -> void:
	_oauth_provider = provider
	_oauth_state = _generate_state_nonce()

	# Start loopback TCP server.
	_tcp_server = TCPServer.new()
	var err := _tcp_server.listen(
		_LOOPBACK_PORT, _LOOPBACK_HOST
	)
	if err != OK:
		_emit_failure(
			"Failed to start loopback server"
		)
		return

	_is_awaiting_oauth = true

	# Providers that require HTTPS redirect URIs use the
	# hosted callback page, which then forwards the code
	# to the loopback server via a client-side redirect.
	var redirect_uri: String
	if provider in _HTTPS_REDIRECT_PROVIDERS:
		redirect_uri = Platform.oauth_callback_url
	else:
		redirect_uri = (
			"http://%s:%d"
			% [_LOOPBACK_HOST, _LOOPBACK_PORT]
		)

	var auth_url := _build_oauth_url(
		provider, redirect_uri, _oauth_state
	)
	auth_status_changed.emit(
		"Opening browser for %s..."
		% _PROVIDER_NAMES[provider]
	)
	OS.shell_open(auth_url)


func _poll_oauth_redirect() -> void:
	if not _tcp_server.is_connection_available():
		return

	var connection := _tcp_server.take_connection()
	if connection == null:
		return

	# Wait briefly for data.
	var data := ""
	var start := Time.get_ticks_msec()
	while (
		connection.get_status()
			== StreamPeerTCP.STATUS_CONNECTED
		and Time.get_ticks_msec() - start < 2000
	):
		if connection.get_available_bytes() > 0:
			data += connection.get_utf8_string(
				connection.get_available_bytes()
			)
			if "\r\n\r\n" in data:
				break
		await get_tree().process_frame

	# Send styled response HTML matching the web
	# callback page. Browsers block window.close()
	# for tabs not opened by JavaScript, so we just
	# tell the user to close the tab manually.
	var html := (
		"HTTP/1.1 200 OK\r\n"
		+ "Content-Type: text/html\r\n\r\n"
		+ "<!DOCTYPE html><html><head>"
		+ "<meta charset=\"utf-8\">"
		+ "<style>"
		+ "body{font-family:system-ui,sans-serif;"
		+ "display:flex;justify-content:center;"
		+ "align-items:center;min-height:100vh;"
		+ "margin:0;background:#1a1a2e;color:#eee}"
		+ ".card{text-align:center;padding:2rem;"
		+ "border-radius:12px;background:#16213e;"
		+ "box-shadow:0 4px 24px rgba(0,0,0,.3);"
		+ "max-width:400px}"
		+ ".icon{font-size:3rem;color:#4CAF50}"
		+ "p{margin-top:1rem;line-height:1.5}"
		+ "</style></head><body>"
		+ "<div class=\"card\">"
		+ "<div class=\"icon\">&#10004;</div>"
		+ "<p>Authentication successful!</p>"
		+ "<p style=\"color:#888;font-size:.85rem\">"
		+ "You can close this tab.</p>"
		+ "</div></body></html>"
	)
	connection.put_data(html.to_utf8_buffer())
	connection.disconnect_from_host()

	_cleanup_tcp_server()
	_is_awaiting_oauth = false

	# Parse auth code from request.
	var code := _parse_query_param(data, "code")
	var state := _parse_query_param(data, "state")
	print(
		"[PlatformAuthApiClient] OAuth redirect: code=%s state_ok=%s"
		% [
			"present" if not code.is_empty()
				else "empty",
			state == _oauth_state,
		]
	)

	if code.is_empty():
		_emit_failure("No auth code received")
		return

	if state != _oauth_state:
		_emit_failure("OAuth state mismatch")
		return

	# The redirect_uri sent to the backend must match the
	# one used in the initial authorization request.
	var redirect_uri: String
	if _oauth_provider in _HTTPS_REDIRECT_PROVIDERS:
		redirect_uri = Platform.oauth_callback_url
	else:
		redirect_uri = (
			"http://%s:%d"
			% [_LOOPBACK_HOST, _LOOPBACK_PORT]
		)

	# Determine endpoint.
	var endpoint := _AUTH_ENDPOINT
	if _is_linking:
		endpoint = _LINK_ENDPOINT
	print(
		"[PlatformAuthApiClient] Sending to %s (linking=%s)"
		% [endpoint, _is_linking]
	)

	auth_status_changed.emit("Authenticating...")
	var body := {
		"provider": _PROVIDER_NAMES[_oauth_provider],
		"auth_code": code,
		"redirect_uri": redirect_uri,
	}
	if endpoint == _AUTH_ENDPOINT:
		body["consent_accepted_at"] = (
			Platform.token_store.consent_accepted_at
		)
		body["consent_legal_version"] = (
			Platform.token_store.consent_legal_version
		)

	_send_auth_request(
		endpoint, body, endpoint == _LINK_ENDPOINT
	)


func _cleanup_tcp_server() -> void:
	if _tcp_server != null:
		_tcp_server.stop()
		_tcp_server = null


# =============================================================
# Web popup OAuth flow (postMessage)
# =============================================================


func _start_web_oauth(
	provider: Provider,
) -> void:
	_oauth_provider = provider
	# Prefix state with "popup:" so the callback page
	# knows to use BroadcastChannel instead of the
	# loopback redirect.
	_oauth_state = "popup:" + _generate_state_nonce()

	var redirect_uri := Platform.oauth_callback_url
	var auth_url := _build_oauth_url(
		provider, redirect_uri, _oauth_state
	)

	auth_status_changed.emit(
		"Opening %s login..."
		% _PROVIDER_NAMES[provider]
	)

	# Set up BroadcastChannel to receive the OAuth code.
	# Uses polling instead of callbacks because
	# JavaScriptBridge callbacks are unreliable in
	# threaded builds (Godot runs in a pthread).
	_setup_web_oauth_channel()

	# Open popup. Must happen synchronously from user
	# interaction to avoid popup blockers.
	# Note: with COOP headers (threaded builds), the
	# return value of window.open() is a restricted
	# proxy that Godot sees as null, even when the
	# popup actually opens. We cannot rely on it.
	var js_code := (
		"window.open('%s', 'oauth_popup',"
		+ " 'width=500,height=700')"
	) % auth_url.replace("'", "\\'")
	JavaScriptBridge.eval(js_code)

	_is_awaiting_web_oauth = true
	auth_status_changed.emit(
		"Complete sign-in in the popup..."
	)

	# Start a timeout timer.
	_start_popup_timeout()


func _setup_web_oauth_channel() -> void:
	_cleanup_web_oauth_channel()

	# Create a BroadcastChannel and store received
	# messages as a JSON string in a global variable.
	# Polled from _poll_web_oauth() each frame.
	# BroadcastChannel works across same-origin pages
	# regardless of COOP headers.
	JavaScriptBridge.eval(
		"window._hopnbop_oauth_result = null;"
		+ "window._hopnbop_oauth_channel"
		+ " = new BroadcastChannel("
		+ "'hopnbop_oauth');"
		+ "window._hopnbop_oauth_channel.onmessage"
		+ " = function(e) {"
		+ " window._hopnbop_oauth_result"
		+ " = JSON.stringify(e.data);"
		+ "};"
	)


func _poll_web_oauth() -> void:
	var result_json: Variant = JavaScriptBridge.eval(
		"window._hopnbop_oauth_result"
	)
	if result_json == null:
		return

	# Clear it so we don't process twice.
	JavaScriptBridge.eval(
		"window._hopnbop_oauth_result = null;"
	)

	_is_awaiting_web_oauth = false

	var json := JSON.new()
	var err := json.parse(result_json)
	if err != OK:
		_cleanup_web_oauth_channel()
		_emit_failure("Invalid OAuth response")
		return

	var data: Dictionary = json.data
	var msg_type: String = data.get("type", "")
	if msg_type != "oauth_callback":
		return

	var code: String = data.get("code", "")
	var state: String = data.get("state", "")

	_cleanup_web_oauth_channel()

	if code.is_empty():
		_emit_failure("No auth code received")
		return

	if state != _oauth_state:
		_emit_failure("OAuth state mismatch")
		return

	var redirect_uri := Platform.oauth_callback_url

	auth_status_changed.emit("Authenticating...")
	var body := {
		"provider": _PROVIDER_NAMES[_oauth_provider],
		"auth_code": code,
		"redirect_uri": redirect_uri,
	}

	# Determine endpoint.
	var endpoint := _AUTH_ENDPOINT
	if _is_linking:
		endpoint = _LINK_ENDPOINT

	if endpoint == _AUTH_ENDPOINT:
		body["consent_accepted_at"] = (
			Platform.token_store.consent_accepted_at
		)
		body["consent_legal_version"] = (
			Platform.token_store.consent_legal_version
		)

	_send_auth_request(
		endpoint, body, endpoint == _LINK_ENDPOINT
	)


func _cleanup_web_oauth_channel() -> void:
	_is_awaiting_web_oauth = false
	JavaScriptBridge.eval(
		"if (window._hopnbop_oauth_channel) {"
		+ " window._hopnbop_oauth_channel.close();"
		+ " window._hopnbop_oauth_channel = null;"
		+ "}"
		+ "window._hopnbop_oauth_result = null;"
	)

	if _popup_timeout_timer != null:
		_popup_timeout_timer.stop()
		_popup_timeout_timer.queue_free()
		_popup_timeout_timer = null


func _start_popup_timeout() -> void:
	if _popup_timeout_timer != null:
		_popup_timeout_timer.queue_free()

	_popup_timeout_timer = Timer.new()
	_popup_timeout_timer.wait_time = _POPUP_TIMEOUT_SEC
	_popup_timeout_timer.one_shot = true
	_popup_timeout_timer.timeout.connect(
		_on_popup_timeout
	)
	add_child(_popup_timeout_timer)
	_popup_timeout_timer.start()


func _on_popup_timeout() -> void:
	_cleanup_web_oauth_channel()
	_emit_failure("Sign-in timed out")


# =============================================================
# HTTP requests
# =============================================================


func _send_auth_request(
	endpoint: String,
	body: Dictionary,
	include_auth_header := false,
) -> void:
	print("[PlatformAuthApiClient] Nakama auth dispatch: %s" % endpoint)
	# Dispatch to the matching Nakama call. Each branch awaits the
	# Nakama SDK, synthesizes a response dict so the downstream
	# _handle_*_success helpers don't have to change.
	if endpoint == _ANON_ENDPOINT or endpoint == _GUEST_ENDPOINT:
		await _do_anon_auth()
	elif endpoint == _AUTH_ENDPOINT:
		await _do_provider_auth(body)
	elif endpoint == _REFRESH_ENDPOINT:
		await _do_session_refresh(body)
	elif endpoint == _LINK_ENDPOINT:
		await _do_link(body)
	elif endpoint == _UNLINK_ENDPOINT:
		await _do_unlink(body)
	else:
		_emit_failure("Unknown auth endpoint: %s" % endpoint)


func _send_delete_request() -> void:
	# Routes through the custom `delete_account` Nakama RPC
	# (snoringcat-platform runtime/account.go) so the soft-delete-
	# with-grace-period flow runs: queue the deletion, clear the
	# profile, and cascade-clear friends / groups / presence /
	# leaderboards / storage / notifications / chat. The account
	# itself is hard-deleted 30 days later unless the player signs
	# back in and cancels. Nakama's built-in DELETE /v2/account
	# would hard-delete immediately with no chance to cancel.
	var session := Platform.build_session_from_store()
	if session == null:
		_emit_failure("Not authenticated")
		return
	var rpc_result = await Platform.get_nakama_client().rpc_async(
		session, "delete_account", "")
	if rpc_result.is_exception():
		_emit_failure(
			_describe_nakama_exception(
				rpc_result.get_exception()))
		return
	_handle_delete_success()


# --------------------------------------------------------------
# Session helpers.
# --------------------------------------------------------------

# Vars bundled into every authenticate / refresh call so they ride
# along in the issued session token. The snoringcat-platform
# runtime's BeforeAuthenticate* hooks read `game_id` here to
# validate the call, and every stateful RPC reads it back off the
# session via RUNTIME_CTX_VARS to scope reads/writes per game.
# See MULTI_GAME_ROADMAP.md §"Stage 2.5".
func _build_session_vars() -> Dictionary:
	return {
		"game_id": Platform.game_id,
	}


# Translates a Nakama session into the dict shape that
# _handle_auth_success expects.
func _session_to_response_dict(
	session: NakamaSession,
	provider_name: String,
) -> Dictionary:
	return {
		"status": "success",
		"player_id": session.user_id,
		"jwt_token": session.token,
		"refresh_token": session.refresh_token,
		"expires_at": session.expire_time,
		"provider": provider_name,
		"is_anonymous": provider_name == "anonymous",
		# Backfilled later via NakamaClient.get_account_async.
		"display_name": Platform.token_store.display_name,
		# A copy: the store clears its own list before refilling it
		# from this dict, so passing the list itself emptied it on
		# every refresh.
		"linked_providers": (
			Platform.token_store.linked_providers.duplicate()),
		"rating": Platform.token_store.rating,
		"consent_accepted_at": Platform.token_store.consent_accepted_at,
		"consent_legal_version": (
			Platform.token_store.consent_legal_version),
		"profile_image_url": Platform.token_store.profile_image_url,
		# Skip the protocol-version handshake here — runtime modules
		# gate on it server-side.
		"protocol_version": -1,
	}


func _describe_nakama_exception(ex: NakamaException) -> String:
	if ex == null:
		return "Unknown Nakama error"
	return "%s (status=%d, grpc=%d)" % [
		ex.message, ex.status_code, ex.grpc_status_code]


## Maps a failed Nakama link call to the error string callers
## switch on. AlreadyExists (gRPC 6: the provider identity
## belongs to another account) becomes PROVIDER_CONFLICT, the
## contract the game's link row handles; anything else keeps
## the described exception text.
func _describe_link_exception(ex: NakamaException) -> String:
	if ex != null and ex.grpc_status_code == 6:
		return "PROVIDER_CONFLICT"
	return _describe_nakama_exception(ex)


# After a successful link/login, fetches the Nakama account so
# we can pick up display_name and avatar_url from whatever the
# provider supplied (Google sends both via the id_token; Nakama
# stores them on the user). Returns an empty dict on failure;
# callers should treat missing fields as "leave existing values
# alone" via _update_profile_from_response.
func _fetch_account_profile_dict(
	session: NakamaSession,
) -> Dictionary:
	var account = (
		await Platform.get_nakama_client().get_account_async(session))
	if account.is_exception():
		push_warning(
			"[PlatformAuthApiClient] get_account_async failed: %s"
			% _describe_nakama_exception(account.get_exception()))
		return {}
	var u = account.user
	return {
		"display_name": u.display_name,
		"profile_image_url": u.avatar_url,
		# token_store.linked_providers is otherwise only populated
		# by link/unlink responses; on a fresh sign-in we have
		# to seed it from the account model or the account panel's
		# Link/Unlink rows render in the wrong state.
		"linked_providers": _account_to_provider_list(account),
	}


# Compute the linked-providers list from a fresh Nakama
# ApiAccount: anonymous (devices), and each OAuth identity that has
# a non-empty *_id on account.user. The account's email is not a
# provider: the platform offers no email sign-in, and an email on an
# account only ever came from a provider's profile.
func _account_to_provider_list(account) -> Array:
	var out: Array = []
	if account.devices and account.devices.size() > 0:
		out.append("anonymous")
	var u = account.user
	if u and not u.google_id.is_empty():
		out.append("google")
	if u and not u.facebook_id.is_empty():
		out.append("facebook")
	if u and not u.apple_id.is_empty():
		out.append("apple")
	if u and not u.steam_id.is_empty():
		out.append("steam")
	return out


# --------------------------------------------------------------
# Nakama auth dispatch implementations.
# --------------------------------------------------------------

func _do_anon_auth() -> void:
	var store: PlatformAuthTokenStore = Platform.token_store
	var device_id := store.local_player_id
	if device_id.is_empty():
		device_id = _generate_state_nonce()
		store.local_player_id = device_id
		store.save_tokens()
	var session: NakamaSession = (
		await Platform.get_nakama_client().authenticate_device_async(
			device_id, null, true, _build_session_vars()))
	if session.is_exception():
		_is_refreshing = false
		guest_jwt_obtained.emit(
			false,
			_describe_nakama_exception(session.get_exception()))
		return
	var data := _session_to_response_dict(session, "anonymous")
	store.store_from_response(data)
	guest_jwt_obtained.emit(true, "")


func _do_provider_auth(body: Dictionary) -> void:
	var provider_name := str(body.get("provider", ""))
	var auth_code := str(body.get("auth_code", ""))
	var redirect_uri := str(body.get("redirect_uri", ""))
	var client := Platform.get_nakama_client()
	var session: NakamaSession
	var vars := _build_session_vars()
	# Profile claims from a Google ID token, kept for
	# cancel_account_deletion() once the session's user_id is known.
	var provider_profile: Dictionary = {}
	match provider_name:
		"google":
			# Nakama validates Google ID tokens, not OAuth codes.
			# Exchange code for id_token via Google's token
			# endpoint (PKCE flow, no client_secret needed).
			var id_token := await _exchange_google_code_for_id_token(
				auth_code, redirect_uri)
			if id_token.is_empty():
				_emit_failure(
					"Failed to exchange Google OAuth code for ID token")
				return
			provider_profile = _provider_profile_from_claims(
				_parse_jwt_claims(id_token))
			session = await client.authenticate_google_async(
				id_token, null, true, vars)
		"facebook":
			# Facebook returns an access token (not an OAuth
			# code) for the implicit-grant flow Nakama expects.
			# If you switch Facebook to code flow, mirror the
			# Google branch above.
			session = await client.authenticate_facebook_async(
				auth_code, null, true, true, vars)
		"apple":
			session = await client.authenticate_apple_async(
				auth_code, null, true, vars)
		"steam":
			session = await client.authenticate_steam_async(
				auth_code, null, true, vars)
		_:
			_emit_failure(
				"Unsupported provider: %s" % provider_name)
			return
	if session.is_exception():
		_emit_failure(_describe_nakama_exception(
			session.get_exception()))
		return
	provider_profile["user_id"] = session.user_id
	_provider_profile = provider_profile
	var data := _session_to_response_dict(session, provider_name)
	# Pick up display_name + avatar_url from Nakama (which got
	# them from the provider's id_token).
	data.merge(
		await _fetch_account_profile_dict(session), true)
	_handle_auth_success(data)


func _do_session_refresh(_body: Dictionary) -> void:
	var current := Platform.build_session_from_store()
	if current == null or current.refresh_token.is_empty():
		# Nakama's session parser can't read the stored refresh
		# token, so no request could ever refresh it either.
		_handle_refresh_failure(
			"No refresh token",
			true,
			Platform.token_store.refresh_token,
		)
		return
	var refresh_token_used := current.refresh_token
	# Pass vars on refresh too so an old token minted before the
	# game_id-vars rollout picks up the value at next refresh.
	# Nakama merges the request's vars onto the existing
	# session's vars; the new session carries the merged map.
	var session: NakamaSession = (
		await Platform.get_nakama_client().session_refresh_async(
			current, _build_session_vars()))
	if session.is_exception():
		var ex := session.get_exception()
		_handle_refresh_failure(
			_describe_nakama_exception(ex),
			_is_refresh_token_rejected(ex),
			refresh_token_used,
		)
		return
	# Cleared on success too. Leaving it set made refresh_token()
	# and _check_auto_refresh no-ops for the rest of the process
	# after the first successful refresh.
	_is_refreshing = false
	_handle_auth_success(_session_to_response_dict(
		session, Platform.token_store.provider))


func _do_link(body: Dictionary) -> void:
	var session := Platform.build_session_from_store()
	if session == null:
		_emit_failure("Not authenticated")
		return
	var provider_name := str(body.get("provider", ""))
	var auth_code := str(body.get("auth_code", ""))
	var redirect_uri := str(body.get("redirect_uri", ""))
	var client := Platform.get_nakama_client()
	var result: NakamaAsyncResult
	# Captured for the post-link account-update step (Google only).
	var google_id_token := ""
	match provider_name:
		"google":
			# Same exchange step as _do_provider_auth. Nakama's
			# link_google_async wants an id_token, not an OAuth
			# code.
			var id_token := await _exchange_google_code_for_id_token(
				auth_code, redirect_uri)
			if id_token.is_empty():
				_emit_failure(
					"Failed to exchange Google OAuth code for ID token")
				return
			google_id_token = id_token
			result = await client.link_google_async(session, id_token)
		"facebook":
			result = await client.link_facebook_async(session, auth_code)
		"apple":
			result = await client.link_apple_async(session, auth_code)
		"steam":
			result = await client.link_steam_async(session, auth_code)
		_:
			_emit_failure(
				"Unsupported link provider: %s" % provider_name)
			return
	if result.is_exception():
		_emit_failure(_describe_link_exception(
			result.get_exception()))
		return
	# Pick up the Google avatar on link. The display name is
	# never taken from the provider (H59): accounts keep the
	# generated name they were created with.
	if not google_id_token.is_empty():
		var profile := _provider_profile_from_claims(
			_parse_jwt_claims(google_id_token))
		await _apply_provider_profile(session, profile)
		profile["user_id"] = session.user_id
		_provider_profile = profile
	# Synthesize a success dict so the downstream handler updates
	# linked_providers in the token store consistently.
	var linked: Array = Platform.token_store.linked_providers.duplicate()
	if not linked.has(provider_name):
		linked.append(provider_name)
	var data := {
		"status": "success",
		"linked_providers": linked,
		"player_id": Platform.token_store.player_id,
		"jwt_token": Platform.token_store.jwt_token,
		"refresh_token": Platform.token_store.refresh_token,
		"expires_at": Platform.token_store.expires_at,
		"display_name": Platform.token_store.display_name,
		"rating": Platform.token_store.rating,
		"protocol_version": -1,
	}
	# Pick up display_name + avatar_url that Nakama just stored
	# from the provider's id_token; without this the player's
	# avatar wouldn't show up until the next session.
	data.merge(
		await _fetch_account_profile_dict(session), true)
	_handle_auth_success(data)


func _do_unlink(body: Dictionary) -> void:
	var session := Platform.build_session_from_store()
	if session == null:
		_emit_failure("Not authenticated")
		return
	var provider_name := str(body.get("provider", ""))
	var client := Platform.get_nakama_client()
	var result: NakamaAsyncResult
	# Nakama's unlink calls take a token (provider-issued) for
	# providers that require it. We don't have one here, so we
	# pass an empty string; Nakama will use the linked identity
	# as recorded on the account.
	match provider_name:
		"google":
			result = await client.unlink_google_async(session, "")
		"facebook":
			result = await client.unlink_facebook_async(session, "")
		"apple":
			result = await client.unlink_apple_async(session, "")
		"steam":
			result = await client.unlink_steam_async(session, "")
		_:
			_emit_failure(
				"Unsupported unlink provider: %s" % provider_name)
			return
	if result.is_exception():
		_emit_failure(_describe_nakama_exception(
			result.get_exception()))
		return
	var linked: Array = Platform.token_store.linked_providers.duplicate()
	linked.erase(provider_name)
	_handle_unlink_success({
		"status": "success",
		"linked_providers": linked,
		"player_id": Platform.token_store.player_id,
	})


func _handle_auth_success(data: Dictionary) -> void:
	# Check protocol version.
	var server_protocol: int = data.get(
		"protocol_version", -1)
	if server_protocol > 0:
		var client_protocol: int = (
			ProjectSettings.get_setting(
				"application/config/"
				+ "protocol_version",
				1,
			))
		if server_protocol != client_protocol:
			version_mismatch.emit(
				str(client_protocol),
				str(server_protocol),
			)

	# Update linked providers from response. Copy the list before
	# clearing the store's, in case it is the store's own array.
	if data.has("linked_providers"):
		var lp: Array = data.get("linked_providers", [])
		lp = lp.duplicate()
		Platform.token_store.linked_providers.clear()
		for p in lp:
			Platform.token_store.linked_providers.append(
				str(p)
			)
		Platform.token_store.save_tokens()

	if _is_linking:
		_is_linking = false
		var provider_name := _link_provider_name
		_link_provider_name = ""
		_update_profile_from_response(data)
		# A guest who links keeps their account and stops
		# being a guest (H57): the linked provider is their
		# sign-in from now on.
		if Platform.token_store.is_anonymous:
			Platform.token_store.is_anonymous = false
			Platform.token_store.provider = provider_name
			Platform.token_store.save_tokens()
		link_completed.emit(true, "", provider_name)
		return

	# Store tokens.
	if data.has("jwt_token"):
		Platform.token_store.store_from_response(data)
		_last_refresh_time = (
			Time.get_unix_time_from_system()
		)
		_reset_refresh_backoff()

	auth_status_changed.emit("Authenticated")
	auth_completed.emit(true, "")


func _handle_unlink_success(data: Dictionary) -> void:
	_is_unlinking = false
	var provider_name := _unlink_provider_name
	_unlink_provider_name = ""

	# Update linked providers from response.
	if data.has("linked_providers"):
		Platform.token_store.linked_providers.clear()
		var lp: Array = data.get("linked_providers", [])
		for p in lp:
			Platform.token_store.linked_providers.append(
				str(p)
			)
		Platform.token_store.save_tokens()

	unlink_completed.emit(true, "", provider_name)


## Update display_name and profile_image_url from a
## link response, if the server included
## them. Always persists to disk.
func _update_profile_from_response(
	data: Dictionary,
) -> void:
	var new_name: String = data.get(
		"display_name", "")
	if not new_name.is_empty():
		Platform.token_store.display_name = new_name
	var new_image: String = data.get(
		"profile_image_url", "")
	if not new_image.is_empty():
		Platform.token_store.profile_image_url = (
			new_image)
	Platform.token_store.save_tokens()


func _handle_delete_success() -> void:
	_is_deleting = false
	_provider_profile = {}
	# Forgets the player's consent too, unlike an automatic sign-out.
	Platform.token_store.clear_tokens()
	delete_completed.emit(true, "")


func _emit_failure(error: String) -> void:
	push_warning(
		"[PlatformAuthApiClient] Failure: %s (linking=%s)"
		% [error, _is_linking]
	)
	if _is_linking:
		_is_linking = false
		var provider_name := _link_provider_name
		_link_provider_name = ""
		link_completed.emit(false, error, provider_name)
	elif _is_unlinking:
		_is_unlinking = false
		var provider_name := _unlink_provider_name
		_unlink_provider_name = ""
		unlink_completed.emit(false, error, provider_name)
	elif _is_deleting:
		_is_deleting = false
		delete_completed.emit(false, error)
	else:
		auth_completed.emit(false, error)


# =============================================================
# Auto-refresh
# =============================================================


func _check_auto_refresh() -> void:
	if not Platform.token_store.needs_refresh():
		return
	if _is_refreshing:
		return
	var now := Time.get_unix_time_from_system()
	if now - _last_refresh_time < _REFRESH_COOLDOWN_SEC:
		return
	if now < _refresh_retry_time:
		return
	refresh_token()


## Finishes a failed session refresh. `refresh_token_used` is the
## refresh token the attempt read from the store.
##
## A rejected token signs the player out: the cleared store no
## longer needs_refresh(), so _check_auto_refresh stops, the next
## auth screen offers sign-in, and signed_out tells the game, after
## auth_completed. The player did not ask for this sign-out, so the
## store keeps their consent (PlatformAuthTokenStore.clear_session).
## The store is left alone if a newer sign-in replaced the token
## while the request was in flight. Any other failure backs off
## before the next automatic attempt. Explicit refresh_token() calls
## skip the backoff: their callers await auth_completed, so they must
## get an answer.
##
## Reports through auth_completed directly. _emit_failure would
## hand a background refresh failure to whichever link / unlink /
## delete flow happened to be in progress.
func _handle_refresh_failure(
	error: String,
	is_token_rejected: bool,
	refresh_token_used: String,
) -> void:
	_is_refreshing = false
	var is_signed_out := false
	if is_token_rejected:
		push_warning(
			"[PlatformAuthApiClient] Refresh token rejected: %s"
			% error,
		)
		_reset_refresh_backoff()
		if Platform.token_store.refresh_token == refresh_token_used:
			Platform.token_store.clear_session()
			is_signed_out = true
	else:
		push_warning(
			(
				"[PlatformAuthApiClient] Session refresh failed,"
				+ " retrying in %d s: %s"
			) % [int(_refresh_retry_delay_sec), error],
		)
		_refresh_retry_time = (
			Time.get_unix_time_from_system()
			+ _refresh_retry_delay_sec)
		_refresh_retry_delay_sec = minf(
			_refresh_retry_delay_sec * _REFRESH_RETRY_BACKOFF_MULTIPLIER,
			_REFRESH_RETRY_MAX_DELAY_SEC,
		)
	auth_completed.emit(false, error)
	if is_signed_out:
		signed_out.emit(error)


func _reset_refresh_backoff() -> void:
	_refresh_retry_time = 0.0
	_refresh_retry_delay_sec = _REFRESH_RETRY_INITIAL_DELAY_SEC


## True when a failed refresh means the refresh token can never
## work again. Only Nakama's own verdict counts: a failure without
## one of its rejection codes (network error, timeout, 5xx, a
## proxy's error page) is treated as transient.
static func _is_refresh_token_rejected(ex: NakamaException) -> bool:
	if ex == null:
		return false
	return ex.grpc_status_code in _REFRESH_REJECTED_GRPC_CODES


# =============================================================
# OAuth URL builders
# =============================================================


func _build_oauth_url(
	provider: Provider,
	redirect_uri: String,
	state: String,
) -> String:
	match provider:
		Provider.GOOGLE:
			return _build_google_auth_url(
				redirect_uri, state
			)
		Provider.FACEBOOK:
			return _build_facebook_auth_url(
				redirect_uri, state
			)
		_:
			return ""


func _build_google_auth_url(
	redirect_uri: String,
	state: String,
) -> String:
	var client_id := Platform.google_oauth_client_id
	# PKCE: generate a fresh verifier per attempt and include the
	# corresponding challenge in the auth URL. The verifier is
	# stored on the client and posted back to Google's token
	# endpoint after we receive the auth code. Avoids needing a
	# Google client_secret embedded in the client.
	_pkce_verifier = _generate_pkce_verifier()
	var challenge := _pkce_challenge_s256(_pkce_verifier)
	return (
		"https://accounts.google.com/o/oauth2/v2/auth"
		+ "?client_id=%s" % client_id
		+ "&redirect_uri=%s" % redirect_uri.uri_encode()
		+ "&response_type=code"
		+ "&scope=%s" % _GOOGLE_OAUTH_SCOPE
		+ "&state=%s" % state
		+ "&code_challenge=%s" % challenge
		+ "&code_challenge_method=S256"
	)


# Parse the payload (claims) section of a JWT without verifying
# the signature. We use this to pull `name` and `picture` from
# Google's id_token, so we can push them into Nakama's user record:
# after a link (Nakama only auto-fills display_name when it was
# previously empty, so anonymous users who later link Google keep
# the auto-generated handle without this step), and after a
# cancelled deletion (cancel_account_deletion). Returns an empty
# dict if the token isn't a valid 3-segment JWT.
func _parse_jwt_claims(jwt: String) -> Dictionary:
	var parts := jwt.split(".")
	if parts.size() != 3:
		return {}
	# base64url -> base64 (with padding) for Marshalls.
	var payload: String = (
		parts[1].replace("-", "+").replace("_", "/")
	)
	while payload.length() % 4 != 0:
		payload += "="
	var bytes := Marshalls.base64_to_raw(payload)
	if bytes.is_empty():
		return {}
	var data: Variant = (
		JSON.parse_string(bytes.get_string_from_utf8()))
	if not (data is Dictionary):
		return {}
	return data


# The picture a Google ID token's claims carry, under the key
# _apply_provider_profile reads. The `name` claim is
# deliberately NOT carried (H59): display names are
# generated, never the provider's (often a real name), and
# players never enter one. The runtime assigns a generated
# name when a social account is created.
func _provider_profile_from_claims(claims: Dictionary) -> Dictionary:
	return {
		"profile_image_url": str(claims.get("picture", "")),
	}


# Writes a provider profile's display name and picture to the
# account. Nakama's update_account_async treats null as "leave alone"
# but "" as "set to empty", so a field the provider didn't send is
# passed as null. Does nothing when both are empty. A failure only
# warns: the account works without a profile.
func _apply_provider_profile(
	session: NakamaSession,
	profile: Dictionary,
) -> void:
	var display_name := str(profile.get("display_name", ""))
	var image_url := str(profile.get("profile_image_url", ""))
	if display_name.is_empty() and image_url.is_empty():
		return
	var name_arg: Variant = (
		display_name if not display_name.is_empty() else null)
	var image_arg: Variant = (
		image_url if not image_url.is_empty() else null)
	var result: NakamaAsyncResult = (
		await Platform.get_nakama_client().update_account_async(
			session,
			null,  # username.
			name_arg,
			image_arg,
			null,  # lang_tag.
			null,  # location.
			null,  # timezone.
		))
	if result.is_exception():
		push_warning(
			"[PlatformAuthApiClient] update_account failed: %s"
			% _describe_nakama_exception(result.get_exception()))


# --------------------------------------------------------------
# PKCE helpers (RFC 7636).
# --------------------------------------------------------------

func _generate_pkce_verifier() -> String:
	# 32 random bytes encoded as base64url is 43 chars, well
	# inside RFC 7636's 43..128 range.
	var bytes := Crypto.new().generate_random_bytes(32)
	return _base64url_encode(bytes)


func _pkce_challenge_s256(verifier: String) -> String:
	var ctx := HashingContext.new()
	ctx.start(HashingContext.HASH_SHA256)
	ctx.update(verifier.to_utf8_buffer())
	return _base64url_encode(ctx.finish())


func _base64url_encode(bytes: PackedByteArray) -> String:
	# Marshalls.raw_to_base64 returns standard base64; convert to
	# the URL-safe variant PKCE wants (no padding, +→-, /→_).
	return Marshalls.raw_to_base64(bytes) \
		.replace("+", "-") \
		.replace("/", "_") \
		.replace("=", "")


# Posts the OAuth authorization code + PKCE verifier to the
# game-supplied Cloudflare Pages Function broker, which adds the
# Google client_secret server-side and exchanges with Google's
# token endpoint. Returns the id_token string, or empty on
# failure (the failure is also logged).
#
# Why not call Google directly? Google's "Web Application" OAuth
# client type requires client_secret on every code-exchange
# request, even with PKCE. We don't want client_secret embedded
# in either the web or desktop binary. The broker holds the
# secret as a Pages env var; the client posts only what it has
# (code + verifier). Same code path on both platforms.
func _exchange_google_code_for_id_token(
	code: String,
	redirect_uri: String,
) -> String:
	if _pkce_verifier.is_empty():
		push_warning(
			"[PlatformAuthApiClient] PKCE verifier missing for token exchange")
		return ""
	var verifier := _pkce_verifier
	_pkce_verifier = ""  # Single-use.

	var http := HTTPRequest.new()
	http.timeout = 30.0
	add_child(http)

	var payload := JSON.stringify({
		"code": code,
		"redirect_uri": redirect_uri,
		"code_verifier": verifier,
	})
	var err := http.request(
		Platform.google_token_broker_url,
		["Content-Type: application/json"],
		HTTPClient.METHOD_POST,
		payload,
	)
	if err != OK:
		push_warning(
			"[PlatformAuthApiClient] Broker request error: %d" % err
		)
		http.queue_free()
		return ""

	var result: Array = await http.request_completed
	http.queue_free()
	var response_code: int = result[1]
	var body_bytes: PackedByteArray = result[3]
	if response_code != 200:
		push_warning(
			"[PlatformAuthApiClient] Broker HTTP %d: %s"
			% [response_code, body_bytes.get_string_from_utf8()]
		)
		return ""
	var data: Variant = JSON.parse_string(body_bytes.get_string_from_utf8())
	if not (data is Dictionary):
		push_warning("[PlatformAuthApiClient] Broker returned non-JSON")
		return ""
	return str(data.get("id_token", ""))


func _build_facebook_auth_url(
	redirect_uri: String,
	state: String,
) -> String:
	var client_id := Platform.facebook_oauth_client_id
	return (
		"https://www.facebook.com/v19.0/dialog/oauth"
		+ "?client_id=%s" % client_id
		+ "&redirect_uri=%s" % redirect_uri.uri_encode()
		+ "&response_type=code"
		+ "&scope=public_profile"
		+ "&state=%s" % state
	)


# =============================================================
# Utilities
# =============================================================


func _generate_anonymous_name() -> String:
	# Produce a display name like "Bunny_4821" using
	# the game's theme. Each session generates a new
	# name; returning players keep their stored name.
	var suffixes := [
		"Dott", "Jiffy", "Fizz", "Pip",
		"Zap", "Mop", "Nib", "Fuzz",
		"Bop", "Hop",
	]
	var suffix: String = (
		suffixes[randi() % suffixes.size()]
	)
	return suffix + "_" + str(randi() % 9000 + 1000)


func _generate_state_nonce() -> String:
	var bytes := PackedByteArray()
	bytes.resize(16)
	for i in bytes.size():
		bytes[i] = randi() % 256
	return bytes.hex_encode()


func _parse_query_param(
	http_request_text: String,
	param_name: String,
) -> String:
	# Extract a query parameter from an HTTP GET
	# request line like "GET /?code=abc&state=xyz".
	var lines := http_request_text.split("\r\n")
	if lines.is_empty():
		return ""
	var first_line := lines[0]
	var parts := first_line.split(" ")
	if parts.size() < 2:
		return ""
	var path := parts[1]
	var query_start := path.find("?")
	if query_start < 0:
		return ""
	var query := path.substr(query_start + 1)
	var pairs := query.split("&")
	for pair in pairs:
		var kv := pair.split("=", true, 1)
		if kv.size() == 2 and kv[0] == param_name:
			return kv[1].uri_decode()
	return ""
