// The RPC handler tests inject the caller through Nakama's
// string-typed context keys, as auth_test.go does.
//
//lint:file-ignore SA1029 nakama-common uses bare strings as context keys.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

const (
	testCrashUserID    = "1f3c2d4e-5a6b-4c7d-8e9f-0a1b2c3d4e5f"
	testCrashSignature = "0123456789abcdef"
)

var testCrashNow = time.Date(2026, 10, 11, 18, 30, 5, 123456000, time.UTC)

// validCrashReport returns a well-formed payload as a map, so a
// test can change one field before encoding it.
func validCrashReport(role string) map[string]any {
	return map[string]any{
		"game_id":          "hopnbop",
		"game_version":     "0.53.0",
		"protocol_version": 2,
		"platform":         "windows",
		"os":               "Windows 10.0.26300",
		"role":             role,
		"message":          "ERROR  : Player # not found",
		"signature":        testCrashSignature,
		"log_excerpt":      "[  1.000] lobby ready\n[  2.000] ERROR  : x",
	}
}

func encodeCrashReport(t *testing.T, report map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(raw)
}

func testCrashGames(t *testing.T) *perGameConfig {
	t.Helper()
	return newTestGames(t, map[string]string{"hopnbop": hopnbopConfigJSON})
}

// errorCode (admin_key_test.go) returns a runtime.Error's gRPC code.

// --- Validation -----------------------------------------------

func TestParseCrashReportAcceptsClientReport(t *testing.T) {
	payload := encodeCrashReport(t, validCrashReport("client"))
	got, err := parseCrashReport(
		payload, testCrashGames(t), true, "hopnbop", testCrashNow)
	if err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
	want := crashReportRecord{
		GameID:          "hopnbop",
		GameVersion:     "0.53.0",
		ProtocolVersion: 2,
		Platform:        "windows",
		OS:              "Windows 10.0.26300",
		Role:            "client",
		Message:         "ERROR  : Player # not found",
		Signature:       testCrashSignature,
		LogExcerpt:      "[  1.000] lobby ready\n[  2.000] ERROR  : x",
		ReceivedAt:      "2026-10-11T18:30:05Z",
	}
	if *got != want {
		t.Fatalf("record:\n got %+v\nwant %+v", *got, want)
	}
}

func TestParseCrashReportAcceptsServerReport(t *testing.T) {
	payload := encodeCrashReport(t, validCrashReport("server"))
	got, err := parseCrashReport(
		payload, testCrashGames(t), false, "", testCrashNow)
	if err != nil {
		t.Fatalf("valid server report rejected: %v", err)
	}
	if got.Role != "server" {
		t.Fatalf("role = %q, want server", got.Role)
	}
}

func TestParseCrashReportAcceptsWholeFloatProtocol(t *testing.T) {
	// A report a Godot client read back from its queue file
	// carries 2.0, because Godot's JSON parser makes every number
	// a float.
	payload := strings.Replace(
		encodeCrashReport(t, validCrashReport("client")),
		`"protocol_version":2`, `"protocol_version":2.0`, 1)
	if !strings.Contains(payload, `"protocol_version":2.0`) {
		t.Fatalf("fixture did not get a 2.0: %s", payload)
	}
	got, err := parseCrashReport(
		payload, testCrashGames(t), true, "hopnbop", testCrashNow)
	if err != nil {
		t.Fatalf("2.0 rejected: %v", err)
	}
	if got.ProtocolVersion != 2 {
		t.Fatalf("protocol_version = %d, want 2", got.ProtocolVersion)
	}
}

func TestParseCrashReportAcceptsSessionWithoutGameVar(t *testing.T) {
	// Tokens minted before the game_id vars rollout carry none;
	// the payload's registered game_id is enough.
	payload := encodeCrashReport(t, validCrashReport("client"))
	if _, err := parseCrashReport(
		payload, testCrashGames(t), true, "", testCrashNow); err != nil {
		t.Fatalf("report rejected: %v", err)
	}
}

func TestParseCrashReportRejectsInvalidPayloads(t *testing.T) {
	type tc struct {
		name          string
		isSession     bool
		sessionGameID string
		mutate        func(map[string]any)
		raw           string
	}
	cases := []tc{
		{name: "empty", isSession: true, raw: ""},
		{name: "not-json", isSession: true, raw: "{not json"},
		{name: "json-string", isSession: true, raw: `"report"`},
		{
			name:      "oversized",
			isSession: true,
			mutate: func(r map[string]any) {
				r["log_excerpt"] = strings.Repeat(
					"x", crashReportMaxPayloadBytes)
			},
		},
		{
			name:      "session-claims-server",
			isSession: true,
			mutate:    func(r map[string]any) { r["role"] = "server" },
		},
		{
			name:      "http-key-claims-client",
			isSession: false,
			mutate:    func(r map[string]any) { r["role"] = "client" },
		},
		{
			name:      "unknown-role",
			isSession: true,
			mutate:    func(r map[string]any) { r["role"] = "admin" },
		},
		{
			name:      "missing-game-id",
			isSession: true,
			mutate:    func(r map[string]any) { delete(r, "game_id") },
		},
		{
			name:      "unknown-game-id",
			isSession: true,
			mutate:    func(r map[string]any) { r["game_id"] = "ghost" },
		},
		{
			name:          "other-game-than-session",
			isSession:     true,
			sessionGameID: "another",
		},
		{
			name:      "bad-game-version",
			isSession: true,
			mutate: func(r map[string]any) {
				r["game_version"] = "0.53 beta"
			},
		},
		{
			name:      "long-game-version",
			isSession: true,
			mutate: func(r map[string]any) {
				r["game_version"] = strings.Repeat("1", 33)
			},
		},
		{
			name:      "negative-protocol",
			isSession: true,
			mutate:    func(r map[string]any) { r["protocol_version"] = -1 },
		},
		{
			name:      "fractional-protocol",
			isSession: true,
			mutate:    func(r map[string]any) { r["protocol_version"] = 2.5 },
		},
		{
			name:      "string-protocol",
			isSession: true,
			mutate:    func(r map[string]any) { r["protocol_version"] = "2" },
		},
		{
			name:      "uppercase-platform",
			isSession: true,
			mutate:    func(r map[string]any) { r["platform"] = "Windows" },
		},
		{
			name:      "missing-os",
			isSession: true,
			mutate:    func(r map[string]any) { r["os"] = " " },
		},
		{
			name:      "long-os",
			isSession: true,
			mutate: func(r map[string]any) {
				r["os"] = strings.Repeat("o", crashReportMaxOSBytes+1)
			},
		},
		{
			name:      "short-signature",
			isSession: true,
			mutate:    func(r map[string]any) { r["signature"] = "abc123" },
		},
		{
			name:      "uppercase-signature",
			isSession: true,
			mutate: func(r map[string]any) {
				r["signature"] = "0123456789ABCDEF"
			},
		},
		{
			name:      "missing-message",
			isSession: true,
			mutate:    func(r map[string]any) { r["message"] = "\n\t" },
		},
		{
			name:      "long-message",
			isSession: true,
			mutate: func(r map[string]any) {
				r["message"] = strings.Repeat(
					"é", crashReportMaxMessageRunes+1)
			},
		},
		{
			name:      "long-excerpt",
			isSession: true,
			mutate: func(r map[string]any) {
				r["log_excerpt"] = strings.Repeat(
					"y", crashReportMaxExcerptBytes+1)
			},
		},
	}
	games := testCrashGames(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := c.raw
			if c.raw == "" && c.name != "empty" {
				role := "client"
				if !c.isSession {
					role = "server"
				}
				report := validCrashReport(role)
				if c.mutate != nil {
					c.mutate(report)
				}
				payload = encodeCrashReport(t, report)
			}
			_, err := parseCrashReport(
				payload, games, c.isSession, c.sessionGameID,
				testCrashNow)
			if err == nil {
				t.Fatal("invalid report accepted")
			}
			if code := errorCode(err); code != 3 {
				t.Fatalf("code = %d, want 3 (%v)", code, err)
			}
		})
	}
}

func TestParseCrashReportRejectsEveryReportBeforeBootstrap(t *testing.T) {
	// Unlike requireGameID, there is no empty-registry pass-
	// through: an unregistered game_id is always rejected.
	payload := encodeCrashReport(t, validCrashReport("client"))
	_, err := parseCrashReport(
		payload, newTestGames(t, nil), true, "", testCrashNow)
	if errorCode(err) != 3 {
		t.Fatalf("empty registry: err = %v, want code 3", err)
	}
}

func TestParseCrashReportStoresNoCallerIdentity(t *testing.T) {
	report := validCrashReport("client")
	report["player_id"] = testCrashUserID
	report["user_id"] = testCrashUserID
	report["device_id"] = "device-1234"
	report["ip"] = "203.0.113.7"
	got, err := parseCrashReport(
		encodeCrashReport(t, report), testCrashGames(t), true,
		"hopnbop", testCrashNow)
	if err != nil {
		t.Fatalf("report rejected: %v", err)
	}
	stored, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{
		"player_id", "user_id", "device_id", "\"ip\"",
		testCrashUserID, "device-1234", "203.0.113.7",
	} {
		if strings.Contains(string(stored), leak) {
			t.Errorf("stored record contains %q: %s", leak, stored)
		}
	}
}

func TestParseCrashReportSanitizesControlCharacters(t *testing.T) {
	report := validCrashReport("client")
	report["message"] = "first line\nsecond\tline\r"
	report["log_excerpt"] = "a\x00b\nc\td\x1b[0m"
	got, err := parseCrashReport(
		encodeCrashReport(t, report), testCrashGames(t), true,
		"hopnbop", testCrashNow)
	if err != nil {
		t.Fatalf("report rejected: %v", err)
	}
	if got.Message != "first line second line" {
		t.Errorf("message = %q", got.Message)
	}
	if got.LogExcerpt != "ab\nc\td[0m" {
		t.Errorf("excerpt = %q", got.LogExcerpt)
	}
}

func TestParseCrashReportRedactsBeforeStoring(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJ1aWQiOiJ4In0.c2lnbmF0dXJl"
	report := validCrashReport("client")
	report["message"] = "auth failed for " + testCrashUserID
	report["log_excerpt"] = "Authorization: Bearer " + jwt +
		"\nGET /v2/rpc/x?http_key=s3cr3t%2Fkey&unwrap=true"
	got, err := parseCrashReport(
		encodeCrashReport(t, report), testCrashGames(t), true,
		"hopnbop", testCrashNow)
	if err != nil {
		t.Fatalf("report rejected: %v", err)
	}
	for _, secret := range []string{testCrashUserID, jwt, "s3cr3t"} {
		if strings.Contains(got.Message+got.LogExcerpt, secret) {
			t.Errorf("secret %q survived: message=%q excerpt=%q",
				secret, got.Message, got.LogExcerpt)
		}
	}
}

// --- Redaction ------------------------------------------------

func TestRedactCrashReportText(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"token eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjF9.c2ln end",
			"token <jwt> end",
		},
		{
			`{"Authorization": "Basic ZGVmYXVsdGtleTo="}`,
			`{"Authorization": "Basic <redacted>"}`,
		},
		{
			"Authorization: Bearer abc.def-ghi_123",
			"Authorization: Bearer <redacted>",
		},
		{
			"/v2/rpc/match_end?http_key=VVU3A%2Fq3QzQ%3D&unwrap=true",
			"/v2/rpc/match_end?http_key=<redacted>&unwrap=true",
		},
		{"refresh_token=abc123, next", "refresh_token=<redacted>, next"},
		{
			"GET /?code=4/0AbCd&state=f00d HTTP/1.1",
			"GET /?code=<redacted>&state=<redacted> HTTP/1.1",
		},
		{
			`{"id":"x","user_id":"u1","display_name":"Alice Smith"}`,
			`{"id":"x","user_id":"<redacted>","display_name":"<redacted>"}`,
		},
		{"user " + testCrashUserID + " left", "user <uuid> left"},
		{
			"device 0123456789abcdef0123456789abcdef ok",
			"device <hex> ok",
		},
		{"steam 76561198000000000 ok", "steam <number> ok"},
		{"mail alice.smith@example.com now", "mail <email> now"},
		{"peer 203.0.113.7:4433 joined", "peer <ip>:4433 joined"},
		{
			`[0] _run (C:/Users/Alice Smith/AppData/x.gd:54)`,
			`[0] _run (C:/Users/<user>/AppData/x.gd:54)`,
		},
		{
			`open C:\Users\asmith\AppData\Roaming\Godot failed`,
			`open C:\Users\<user>\AppData\Roaming\Godot failed`,
		},
		{"/home/asmith/.local/share/godot", "/home/<user>/.local/share/godot"},
		{"/Users/asmith/Library/Godot", "/Users/<user>/Library/Godot"},
		{
			"[PlatformMatchmaking] Preview device_id=preview_" +
				"9f3c2d4e-5a6b-4c7d-8e9f-0a1b2c3d4e5f_C1 uid=x",
			"[PlatformMatchmaking] Preview device_id=<redacted> uid=x",
		},
		{"group party_" + testCrashUserID, "group party_<uuid>"},
		{
			"wss://signaling.snoringcat.games/connect/" +
				"MTAuMC4wLjE6NDQzNDoxNzAwMDAwMDAwOmFiY2Q",
			"wss://signaling.snoringcat.games/connect/<redacted>",
		},
	}
	for _, c := range cases {
		if got := redactCrashReportText(c.in); got != c.want {
			t.Errorf("redact(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRedactCrashReportTextKeepsOrdinaryLogLines(t *testing.T) {
	for _, line := range []string{
		"basic configuration loaded",
		"[  12.345|  678][CS] Version: 0.53.0",
		"SCRIPT ERROR: res://src/core/game_panel.gd:2527 - Invalid call",
		"CrashHandlerException: Program crashed with signal 11",
		"Engine version: Godot Engine v4.7.2.stable.official",
		"[1] error(-1): no debug info in PE/COFF executable",
		"frame 1234 rollback to 1200",
	} {
		if got := redactCrashReportText(line); got != line {
			t.Errorf("ordinary line changed:\n got %q\nwant %q", got, line)
		}
	}
}

// --- Rate limiting --------------------------------------------

func newTestCrashLimiter() (*crashReportLimiter, *fakeClock) {
	clock := &fakeClock{t: testCrashNow}
	limiter := newCrashReportLimiter()
	limiter.now = clock.now
	return limiter, clock
}

// reserveErr is reserve without its first-cap-hit notice.
func reserveErr(limiter *crashReportLimiter, userID string) error {
	_, err := limiter.reserve(userID)
	return err
}

func TestCrashReportLimiterNamesEachDailyCapOnce(t *testing.T) {
	limiter, clock := newTestCrashLimiter()
	for i := 0; i < crashReportServerDailyCap; i++ {
		_ = reserveErr(limiter, "")
	}
	if capHit, err := limiter.reserve(""); capHit != "server" || err == nil {
		t.Fatalf("first server-cap rejection: %q, %v", capHit, err)
	}
	if capHit, _ := limiter.reserve(""); capHit != "" {
		t.Fatalf("second server-cap rejection named %q again", capHit)
	}
	for i := 0; limiter.totalToday < crashReportGlobalDailyCap; i++ {
		_ = reserveErr(limiter, fmt.Sprintf("u%d", i))
	}
	if capHit, err := limiter.reserve("late"); capHit != "global" ||
		err == nil {
		t.Fatalf("first global-cap rejection: %q, %v", capHit, err)
	}
	if capHit, _ := limiter.reserve("later"); capHit != "" {
		t.Fatalf("second global-cap rejection named %q again", capHit)
	}
	clock.advance(6 * time.Hour)
	if capHit, err := limiter.reserve(""); capHit != "" || err != nil {
		t.Fatalf("next day: %q, %v", capHit, err)
	}
}

func TestCrashReportLimiterPerUser(t *testing.T) {
	limiter, _ := newTestCrashLimiter()
	for i := 0; i < crashReportUserLimit; i++ {
		if err := reserveErr(limiter, "u1"); err != nil {
			t.Fatalf("report %d rejected: %v", i+1, err)
		}
	}
	err := reserveErr(limiter, "u1")
	if errorCode(err) != 8 {
		t.Fatalf("report over the limit: err = %v, want code 8", err)
	}
	if err := reserveErr(limiter, "u2"); err != nil {
		t.Fatalf("another user's budget was shared: %v", err)
	}
}

func TestCrashReportLimiterWindowSlides(t *testing.T) {
	limiter, clock := newTestCrashLimiter()
	for i := 0; i < crashReportUserLimit; i++ {
		_ = reserveErr(limiter, "u1")
	}
	clock.advance(crashReportUserWindow - time.Second)
	if reserveErr(limiter, "u1") == nil {
		t.Fatal("window slid early")
	}
	clock.advance(2 * time.Second)
	if err := reserveErr(limiter, "u1"); err != nil {
		t.Fatalf("window did not slide: %v", err)
	}
}

func TestCrashReportLimiterRejectionDoesNotConsume(t *testing.T) {
	limiter, clock := newTestCrashLimiter()
	for i := 0; i < crashReportUserLimit; i++ {
		_ = reserveErr(limiter, "u1")
	}
	for i := 0; i < 20; i++ {
		_ = reserveErr(limiter, "u1")
	}
	if limiter.totalToday != crashReportUserLimit {
		t.Fatalf("totalToday = %d, want %d",
			limiter.totalToday, crashReportUserLimit)
	}
	clock.advance(crashReportUserWindow + time.Second)
	if err := reserveErr(limiter, "u1"); err != nil {
		t.Fatalf("rejected attempts used up the next window: %v", err)
	}
}

func TestCrashReportLimiterServerDailyCap(t *testing.T) {
	limiter, clock := newTestCrashLimiter()
	for i := 0; i < crashReportServerDailyCap; i++ {
		if err := reserveErr(limiter, ""); err != nil {
			t.Fatalf("server report %d rejected: %v", i+1, err)
		}
	}
	if errorCode(reserveErr(limiter, "")) != 8 {
		t.Fatal("server cap not enforced")
	}
	if err := reserveErr(limiter, "u1"); err != nil {
		t.Fatalf("server cap blocked a session user: %v", err)
	}
	// testCrashNow is 18:30 UTC; six hours on is the next UTC day.
	clock.advance(6 * time.Hour)
	if err := reserveErr(limiter, ""); err != nil {
		t.Fatalf("server cap did not reset at UTC midnight: %v", err)
	}
}

func TestCrashReportLimiterGlobalDailyCap(t *testing.T) {
	limiter, clock := newTestCrashLimiter()
	accepted := 0
	for i := 0; accepted < crashReportGlobalDailyCap; i++ {
		user := fmt.Sprintf("u%d", i)
		for j := 0; j < crashReportUserLimit &&
			accepted < crashReportGlobalDailyCap; j++ {
			if err := reserveErr(limiter, user); err != nil {
				t.Fatalf("report %d rejected: %v", accepted+1, err)
			}
			accepted++
		}
	}
	if errorCode(reserveErr(limiter, "fresh-user")) != 8 {
		t.Fatal("global cap not enforced for a session user")
	}
	// SP61: the global cap no longer starves HTTP-key
	// callers; servers draw on their own budget.
	if err := reserveErr(limiter, ""); err != nil {
		t.Fatalf("global cap starved a server report: %v", err)
	}
	clock.advance(6 * time.Hour)
	if err := reserveErr(limiter, "fresh-user"); err != nil {
		t.Fatalf("global cap did not reset: %v", err)
	}
}

func TestCrashReportLimiterRelease(t *testing.T) {
	limiter, _ := newTestCrashLimiter()
	for i := 0; i < crashReportUserLimit; i++ {
		_ = reserveErr(limiter, "u1")
	}
	limiter.release("u1")
	if err := reserveErr(limiter, "u1"); err != nil {
		t.Fatalf("released slot not reusable: %v", err)
	}
	_ = reserveErr(limiter, "")
	limiter.release("")
	if limiter.serverToday != 0 {
		t.Fatalf("serverToday = %d after release, want 0",
			limiter.serverToday)
	}
}

func TestCrashReportLimiterSweepsStaleUsers(t *testing.T) {
	limiter, clock := newTestCrashLimiter()
	for i := 0; i <= crashReportLimiterSweepSize; i++ {
		limiter.perUser[fmt.Sprintf("old%d", i)] = []int64{
			clock.now().UnixNano(),
		}
	}
	clock.advance(crashReportUserWindow + time.Second)
	if err := reserveErr(limiter, "new"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if len(limiter.perUser) != 1 {
		t.Fatalf("tracked users = %d after sweep, want 1",
			len(limiter.perUser))
	}
}

// --- RPC handler ----------------------------------------------

// fakeCrashNk records storage calls. Embedding the interface
// satisfies runtime.NakamaModule; any method the code under test
// should not call panics on the nil embed.
type fakeCrashNk struct {
	runtime.NakamaModule

	writes   []*runtime.StorageWrite
	writeErr error

	// objects is kept sorted by key, like Nakama's listing.
	objects   []*api.StorageObject
	listCalls []string
	listErr   error
	deletes   []*runtime.StorageDelete
}

func (f *fakeCrashNk) StorageWrite(
	_ context.Context,
	writes []*runtime.StorageWrite,
) ([]*api.StorageObjectAck, error) {
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	f.writes = append(f.writes, writes...)
	return nil, nil
}

func (f *fakeCrashNk) StorageList(
	_ context.Context,
	_, userID, collection string,
	limit int,
	cursor string,
) ([]*api.StorageObject, string, error) {
	f.listCalls = append(f.listCalls, userID+"/"+collection)
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	start := sort.Search(len(f.objects), func(i int) bool {
		return f.objects[i].Key > cursor
	})
	end := start + limit
	if end >= len(f.objects) {
		return f.objects[start:], "", nil
	}
	return f.objects[start:end], f.objects[end-1].Key, nil
}

func (f *fakeCrashNk) StorageDelete(
	_ context.Context,
	deletes []*runtime.StorageDelete,
) error {
	f.deletes = append(f.deletes, deletes...)
	doomed := map[string]bool{}
	for _, d := range deletes {
		doomed[d.Key] = true
	}
	kept := f.objects[:0]
	for _, obj := range f.objects {
		if !doomed[obj.Key] {
			kept = append(kept, obj)
		}
	}
	f.objects = kept
	return nil
}

type capturedLogEntry struct {
	level   string
	message string
	fields  map[string]interface{}
}

// captureLogger records every log call with the fields attached
// through WithField(s).
type captureLogger struct {
	entries *[]capturedLogEntry
	fields  map[string]interface{}
}

func newCaptureLogger() captureLogger {
	return captureLogger{entries: &[]capturedLogEntry{}}
}

func (l captureLogger) add(level, format string, args []interface{}) {
	*l.entries = append(*l.entries, capturedLogEntry{
		level:   level,
		message: fmt.Sprintf(format, args...),
		fields:  l.fields,
	})
}

func (l captureLogger) Debug(f string, a ...interface{}) { l.add("debug", f, a) }
func (l captureLogger) Info(f string, a ...interface{})  { l.add("info", f, a) }
func (l captureLogger) Warn(f string, a ...interface{})  { l.add("warn", f, a) }
func (l captureLogger) Error(f string, a ...interface{}) { l.add("error", f, a) }
func (l captureLogger) WithField(k string, v interface{}) runtime.Logger {
	return l.WithFields(map[string]interface{}{k: v})
}
func (l captureLogger) WithFields(f map[string]interface{}) runtime.Logger {
	merged := map[string]interface{}{}
	for k, v := range l.fields {
		merged[k] = v
	}
	for k, v := range f {
		merged[k] = v
	}
	return captureLogger{entries: l.entries, fields: merged}
}
func (l captureLogger) Fields() map[string]interface{} { return l.fields }

func sessionCtx(userID, gameID string) context.Context {
	ctx := context.WithValue(
		context.Background(), runtime.RUNTIME_CTX_USER_ID, userID)
	return context.WithValue(
		ctx, runtime.RUNTIME_CTX_VARS,
		map[string]string{"game_id": gameID})
}

func newTestCrashRpc(t *testing.T) (
	func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, string,
	) (string, error),
	*crashReportLimiter,
	*fakeClock,
) {
	t.Helper()
	limiter, clock := newTestCrashLimiter()
	return crashReportRpcFactory(testCrashGames(t), limiter), limiter, clock
}

func TestCrashReportRpcStoresSessionReport(t *testing.T) {
	rpc, _, _ := newTestCrashRpc(t)
	nk := &fakeCrashNk{}
	logger := newCaptureLogger()
	out, err := rpc(
		sessionCtx(testCrashUserID, "hopnbop"), logger, nil, nk,
		encodeCrashReport(t, validCrashReport("client")))
	if err != nil {
		t.Fatalf("rpc: %v", err)
	}
	if len(nk.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(nk.writes))
	}
	w := nk.writes[0]
	if w.Collection != crashReportCollection || w.UserID != "" ||
		w.Version != "*" || w.PermissionRead != 0 ||
		w.PermissionWrite != 0 {
		t.Fatalf("write = %+v, want system-owned, create-only,"+
			" server-only", *w)
	}
	received, ok := crashReportKeyTime(w.Key)
	if !ok || !received.Equal(testCrashNow) {
		t.Fatalf("key %q does not start with the receive time", w.Key)
	}
	if strings.Contains(w.Value, testCrashUserID) {
		t.Fatalf("stored value names the caller: %s", w.Value)
	}
	var record crashReportRecord
	if err := json.Unmarshal([]byte(w.Value), &record); err != nil {
		t.Fatalf("stored value is not a record: %v", err)
	}
	if record.Role != "client" || record.Signature != testCrashSignature {
		t.Fatalf("stored record = %+v", record)
	}
	var resp crashReportResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("response %q: %v", out, err)
	}
	if !resp.Accepted || resp.ID != w.Key {
		t.Fatalf("response = %+v, want accepted with id %q", resp, w.Key)
	}
}

func TestCrashReportRpcLogsOneLinePerReport(t *testing.T) {
	rpc, _, _ := newTestCrashRpc(t)
	logger := newCaptureLogger()
	_, err := rpc(
		sessionCtx(testCrashUserID, "hopnbop"), logger, nil,
		&fakeCrashNk{}, encodeCrashReport(t, validCrashReport("client")))
	if err != nil {
		t.Fatalf("rpc: %v", err)
	}
	if len(*logger.entries) != 1 {
		t.Fatalf("log lines = %d, want 1: %+v",
			len(*logger.entries), *logger.entries)
	}
	entry := (*logger.entries)[0]
	if entry.level != "info" || entry.message != "crash_report accepted" {
		t.Fatalf("log line = %+v", entry)
	}
	for field, want := range map[string]string{
		"signature":    testCrashSignature,
		"game_version": "0.53.0",
		"platform":     "windows",
		"role":         "client",
	} {
		if entry.fields[field] != want {
			t.Errorf("field %s = %v, want %s",
				field, entry.fields[field], want)
		}
	}
	for field, value := range entry.fields {
		if fmt.Sprint(value) == testCrashUserID {
			t.Errorf("log field %s names the caller", field)
		}
	}
}

func TestCrashReportRpcLogsTheFirstDailyCapHit(t *testing.T) {
	rpc, limiter, _ := newTestCrashRpc(t)
	for i := 0; i < crashReportServerDailyCap; i++ {
		_ = reserveErr(limiter, "")
	}
	logger := newCaptureLogger()
	payload := encodeCrashReport(t, validCrashReport("server"))
	for i := 0; i < 3; i++ {
		_, err := rpc(context.Background(), logger, nil, &fakeCrashNk{}, payload)
		if errorCode(err) != 8 {
			t.Fatalf("call %d: err = %v, want code 8", i+1, err)
		}
	}
	warnings := 0
	for _, entry := range *logger.entries {
		if entry.level == "warn" &&
			strings.Contains(entry.message, "server daily cap") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("cap warnings = %d, want 1: %+v", warnings, *logger.entries)
	}
}

func TestCrashReportRpcStoresServerReport(t *testing.T) {
	rpc, _, _ := newTestCrashRpc(t)
	nk := &fakeCrashNk{}
	_, err := rpc(
		context.Background(), testLogger{}, nil, nk,
		encodeCrashReport(t, validCrashReport("server")))
	if err != nil {
		t.Fatalf("rpc: %v", err)
	}
	if len(nk.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(nk.writes))
	}
}

func TestCrashReportRpcRateLimitsSessionUser(t *testing.T) {
	rpc, _, _ := newTestCrashRpc(t)
	nk := &fakeCrashNk{}
	ctx := sessionCtx(testCrashUserID, "hopnbop")
	payload := encodeCrashReport(t, validCrashReport("client"))
	for i := 0; i < crashReportUserLimit; i++ {
		if _, err := rpc(ctx, testLogger{}, nil, nk, payload); err != nil {
			t.Fatalf("report %d: %v", i+1, err)
		}
	}
	_, err := rpc(ctx, testLogger{}, nil, nk, payload)
	if errorCode(err) != 8 {
		t.Fatalf("over the limit: err = %v, want code 8", err)
	}
	if len(nk.writes) != crashReportUserLimit {
		t.Fatalf("writes = %d, want %d", len(nk.writes),
			crashReportUserLimit)
	}
}

func TestCrashReportRpcInvalidReportsDoNotConsumeBudget(t *testing.T) {
	rpc, limiter, _ := newTestCrashRpc(t)
	nk := &fakeCrashNk{}
	ctx := sessionCtx(testCrashUserID, "hopnbop")
	bad := validCrashReport("client")
	bad["signature"] = "nope"
	for i := 0; i < 20; i++ {
		_, err := rpc(
			ctx, testLogger{}, nil, nk, encodeCrashReport(t, bad))
		if errorCode(err) != 3 {
			t.Fatalf("bad report %d: err = %v, want code 3", i, err)
		}
	}
	if limiter.totalToday != 0 || len(nk.writes) != 0 {
		t.Fatalf("invalid reports counted: total=%d writes=%d",
			limiter.totalToday, len(nk.writes))
	}
}

func TestCrashReportRpcReleasesBudgetWhenWriteFails(t *testing.T) {
	rpc, limiter, _ := newTestCrashRpc(t)
	nk := &fakeCrashNk{writeErr: errors.New("db down")}
	ctx := sessionCtx(testCrashUserID, "hopnbop")
	payload := encodeCrashReport(t, validCrashReport("client"))
	_, err := rpc(ctx, testLogger{}, nil, nk, payload)
	if errorCode(err) != 13 {
		t.Fatalf("write failure: err = %v, want code 13", err)
	}
	if limiter.totalToday != 0 {
		t.Fatalf("failed write kept its reservation: total=%d",
			limiter.totalToday)
	}
}

// --- Keys and pruning -----------------------------------------

func TestCrashReportKeysSortByReceiveTime(t *testing.T) {
	earlier, err := newCrashReportKey(testCrashNow)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	later, err := newCrashReportKey(testCrashNow.Add(time.Microsecond))
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if earlier >= later {
		t.Fatalf("keys out of order: %q >= %q", earlier, later)
	}
	again, _ := newCrashReportKey(testCrashNow)
	if again == earlier {
		t.Fatalf("two keys in the same microsecond collided: %q", again)
	}
	got, ok := crashReportKeyTime(earlier)
	if !ok || !got.Equal(testCrashNow) {
		t.Fatalf("crashReportKeyTime(%q) = %v, %t", earlier, got, ok)
	}
	if !strings.HasPrefix(earlier, "2026-10-11T18:30:05.123456Z-") {
		t.Fatalf("key %q does not use the documented layout", earlier)
	}
}

// storedCrashReports returns fake system-owned objects, one per
// age, sorted by key.
func storedCrashReports(ages ...time.Duration) []*api.StorageObject {
	objects := []*api.StorageObject{}
	for i, age := range ages {
		key := testCrashNow.Add(-age).UTC().Format(
			crashReportKeyTimeLayout) + fmt.Sprintf("-%08x", i)
		objects = append(objects, &api.StorageObject{
			Collection: crashReportCollection,
			Key:        key,
			UserId:     systemUserID,
		})
	}
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})
	return objects
}

func TestPruneCrashReportsDeletesOnlyExpired(t *testing.T) {
	day := 24 * time.Hour
	nk := &fakeCrashNk{
		objects: storedCrashReports(40*day, 31*day, 29*day, day),
	}
	deleted := pruneCrashReports(
		context.Background(), testLogger{}, nk, nil, 0, testCrashNow)
	if deleted != 2 || len(nk.objects) != 2 {
		t.Fatalf("deleted = %d, left = %d; want 2 and 2",
			deleted, len(nk.objects))
	}
	for _, obj := range nk.objects {
		received, _ := crashReportKeyTime(obj.Key)
		if testCrashNow.Sub(received) > crashReportRetention {
			t.Errorf("expired report %q survived", obj.Key)
		}
	}
	if len(nk.listCalls) != 1 ||
		nk.listCalls[0] != systemUserID+"/"+crashReportCollection {
		t.Fatalf("list calls = %v, want one system-user listing",
			nk.listCalls)
	}
}

func TestPruneCrashReportsPagesThroughABacklog(t *testing.T) {
	ages := []time.Duration{}
	for i := 0; i < 250; i++ {
		ages = append(ages, 60*24*time.Hour+time.Duration(i)*time.Minute)
	}
	for i := 0; i < 10; i++ {
		ages = append(ages, time.Duration(i)*time.Hour)
	}
	nk := &fakeCrashNk{objects: storedCrashReports(ages...)}
	deleted := pruneCrashReports(
		context.Background(), testLogger{}, nk, nil, 0, testCrashNow)
	if deleted != 250 || len(nk.objects) != 10 {
		t.Fatalf("deleted = %d, left = %d; want 250 and 10",
			deleted, len(nk.objects))
	}
}

func TestPruneCrashReportsLeavesForeignKeys(t *testing.T) {
	nk := &fakeCrashNk{objects: storedCrashReports(45 * 24 * time.Hour)}
	nk.objects = append([]*api.StorageObject{{
		Collection: crashReportCollection,
		Key:        "0-operator-note",
		UserId:     systemUserID,
	}}, nk.objects...)
	deleted := pruneCrashReports(
		context.Background(), testLogger{}, nk, nil, 0, testCrashNow)
	if deleted != 1 || len(nk.objects) != 1 ||
		nk.objects[0].Key != "0-operator-note" {
		t.Fatalf("deleted = %d, left = %+v", deleted, nk.objects)
	}
}

func TestPruneCrashReportsCapsStoredReports(t *testing.T) {
	ages := []time.Duration{}
	for i := 0; i < 8; i++ {
		ages = append(ages, time.Duration(i)*time.Hour)
	}
	nk := &fakeCrashNk{objects: storedCrashReports(ages...)}
	newest := nk.objects[len(nk.objects)-1].Key
	countStored := func(context.Context) (int, error) {
		return len(nk.objects), nil
	}
	deleted := pruneCrashReports(
		context.Background(), testLogger{}, nk, countStored, 5,
		testCrashNow)
	if deleted != 3 || len(nk.objects) != 5 {
		t.Fatalf("deleted = %d, left = %d; want 3 and 5",
			deleted, len(nk.objects))
	}
	if nk.objects[len(nk.objects)-1].Key != newest {
		t.Fatal("the cap dropped the newest report instead of the oldest")
	}
	for _, d := range nk.deletes {
		received, _ := crashReportKeyTime(d.Key)
		if testCrashNow.Sub(received) < 5*time.Hour {
			t.Errorf("deleted a report only %s old",
				testCrashNow.Sub(received))
		}
	}
}

func TestPruneCrashReportsSkipsTheCapWhenCountFails(t *testing.T) {
	nk := &fakeCrashNk{objects: storedCrashReports(time.Hour, 2*time.Hour)}
	countStored := func(context.Context) (int, error) {
		return 0, errors.New("db down")
	}
	if deleted := pruneCrashReports(
		context.Background(), testLogger{}, nk, countStored, 1,
		testCrashNow); deleted != 0 {
		t.Fatalf("deleted = %d without a count", deleted)
	}
}

func TestPruneCrashReportsStopsOnListError(t *testing.T) {
	nk := &fakeCrashNk{listErr: errors.New("db down")}
	if deleted := pruneCrashReports(
		context.Background(), testLogger{}, nk, nil, 0, testCrashNow); deleted != 0 {
		t.Fatalf("deleted = %d on a list error", deleted)
	}
	if len(nk.deletes) != 0 {
		t.Fatalf("deleted on a list error: %+v", nk.deletes)
	}
}

// TestCrashReportConstantsStable: these numbers are documented in
// PLATFORM_ARCHITECTURE.md "Crash reports" and in the game's
// privacy policy (30-day retention). Change them there too.
func TestCrashReportConstantsStable(t *testing.T) {
	checks := []struct {
		name      string
		got, want int64
	}{
		{"max payload bytes", crashReportMaxPayloadBytes, 16384},
		{"per-user limit", crashReportUserLimit, 5},
		{"per-user window (s)",
			int64(crashReportUserWindow / time.Second), 3600},
		{"server daily cap", crashReportServerDailyCap, 200},
		{"global daily cap", crashReportGlobalDailyCap, 500},
		{"retention (days)",
			int64(crashReportRetention / (24 * time.Hour)), 30},
		{"max stored reports", crashReportMaxStored, 20000},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d; update the docs if this is"+
				" deliberate", c.name, c.got, c.want)
		}
	}
}
