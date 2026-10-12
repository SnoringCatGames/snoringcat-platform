extends GutTest
## The platform stores no email (runtime/privacy.go):
##
##   - Email sign-in and email linking are refused, so no email is
##     ever stored as a login credential.
##   - Nakama copies the email out of a Google, Facebook, or Apple
##     profile on sign-in and on linking. Clients no longer ask the
##     providers for one (SDK 0.6.0), and every sign-in, link, and
##     session refresh drops one that older clients brought in.
##
## Provider sign-in needs real provider tokens, so these tests put
## an email on the account through the console, the way a provider
## sign-in would have, and sign in with the device id, which runs
## the same after-sign-in hook.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _HTTP_OK := 200
const _HTTP_FORBIDDEN := 403
## Long enough for Nakama's 8-character minimum.
const _PASSWORD := "compliance-password"

var _helper
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func after_each() -> void:
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


func test_email_sign_in_is_refused() -> void:
	if not _can_run():
		return
	var body := {
		"email": "%s@example.invalid" % _run_id("signin"),
		"password": _PASSWORD,
	}
	var game_id: String = _helper.game_id()
	if not game_id.is_empty():
		body["vars"] = {"game_id": game_id}
	var result: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/email?create=true",
		body,
		"basic_server_key")
	assert_eq(
		result.status_code, _HTTP_FORBIDDEN,
		"email sign-in should be refused: status=%d" % result.status_code)


func test_email_link_is_refused() -> void:
	if not _can_run():
		return
	var user := await _new_user("compliance-email-link")
	if user.is_empty():
		return
	var result: Dictionary = await _helper.http_post(
		"/v2/account/link/email",
		{
			"email": "%s@example.invalid" % _run_id("link"),
			"password": _PASSWORD,
		},
		"bearer:" + str(user.token))
	assert_eq(
		result.status_code, _HTTP_FORBIDDEN,
		"email linking should be refused: status=%d" % result.status_code)
	var account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + str(user.token))
	var stored_email := ""
	if account.body is Dictionary:
		stored_email = str(account.body.get("email", ""))
	assert_eq(stored_email, "", "a refused link stored an email")


func test_sign_in_drops_an_imported_email() -> void:
	if not _can_run():
		return
	if not _helper.get_console_refusal().is_empty():
		pending(_helper.get_console_refusal())
		return
	var user := await _new_user("compliance-email-drop")
	if user.is_empty():
		return
	var user_id := str(user.user_id)
	var email := "%s@example.invalid" % _run_id("drop")

	assert_true(
		await _helper.console_update_account(user_id, {"email": email}),
		"console could not set the email")
	assert_eq(
		await _account_email(user_id), email,
		"the console did not store the email")

	var signed_in: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/device?create=false",
		_helper.device_auth_body(str(user.device_id)),
		"basic_server_key")
	assert_eq(signed_in.status_code, _HTTP_OK, "device sign-in failed")
	assert_eq(
		await _account_email(user_id), "",
		"signing in kept the imported email")


func test_session_refresh_drops_an_imported_email() -> void:
	if not _can_run():
		return
	if not _helper.get_console_refusal().is_empty():
		pending(_helper.get_console_refusal())
		return
	var user := await _new_user("compliance-email-refresh")
	if user.is_empty():
		return
	var user_id := str(user.user_id)

	assert_true(
		await _helper.console_update_account(
			user_id, {"email": "%s@example.invalid" % _run_id("refresh")}),
		"console could not set the email")
	var refreshed: Dictionary = await _helper.http_post(
		"/v2/account/session/refresh",
		_helper.session_refresh_body(str(user.refresh_token)),
		"basic_server_key")
	assert_eq(refreshed.status_code, _HTTP_OK, "session refresh failed")
	assert_eq(
		await _account_email(user_id), "",
		"a session refresh kept the imported email")


func _can_run() -> bool:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return false
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return false
	return true


## Mints a one-shot anonymous account, deleted in after_each.
## Returns {} (and pends) when sign-in fails.
func _new_user(prefix: String) -> Dictionary:
	var users: Array = await _helper.multi_session_anon(1, prefix)
	if users.size() != 1:
		pending("multi_session_anon did not return a user")
		return {}
	_users.append_array(users)
	return users[0]


## The account's stored email, read through the console ("" when
## none, "<unreadable>" when the read fails).
func _account_email(user_id: String) -> String:
	var account: Dictionary = await _helper.console_get_account(user_id)
	if account.is_empty():
		return "<unreadable>"
	return str(account.get("email", ""))


func _run_id(label: String) -> String:
	return "compliance-%s-%d-%d" % [
		label, Time.get_unix_time_from_system() as int, randi() % 100000]
