package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Crash and error reports from game clients and dedicated game
// servers: the `crash_report` RPC, its storage, and its retention
// cron.
//
// Callers:
//   - Players, through their Nakama session. Anonymous accounts
//     count: a guest session works the same as a linked one. The
//     report's role must be "client".
//   - Dedicated game servers, through the runtime HTTP key
//     (`?http_key=`). Servers read it from NAKAMA_HTTP_KEY in their
//     container environment; client builds must never carry it.
//     The report's role must be "server".
//
// Privacy: a report carries no account id, IP address, or device
// id, and the runtime adds none. The caller's user id is used only
// as the key of the in-memory per-user rate limiter below; it is
// never stored or logged. The message and log excerpt are redacted
// again here (JWTs, bearer and basic credentials, secret-named
// parameters and JSON fields, UUIDs, home directory user names,
// long hex and numeric ids, emails, IPv4 addresses) on top of the
// game client's own redaction, because game logs from before SDK
// 0.5.2 carried session and refresh tokens.
//
// Storage: one object per report in the `crash_reports`
// collection, owned by the system user, with permission read=0 and
// write=0, so no client can read or write one. Keys start with the
// UTC receive time in crashReportKeyTimeLayout plus a random
// suffix, so they sort chronologically. Operators read reports
// with SQL against Nakama's storage table; see
// PLATFORM_ARCHITECTURE.md "Crash reports".
//
// Retention: a goroutine started from InitModule deletes reports
// older than crashReportRetention every hour, the same mechanism
// as account_cron.go, and the oldest ones past crashReportMaxStored.

const (
	crashReportCollection = "crash_reports"

	// systemUserID owns server-only storage objects. Nakama
	// stores runtime writes with UserID "" under this id.
	systemUserID = "00000000-0000-0000-0000-000000000000"

	crashReportRoleClient = "client"
	crashReportRoleServer = "server"

	// crashReportMaxPayloadBytes caps the raw RPC payload. Game
	// clients trim their log excerpt to fit under it.
	crashReportMaxPayloadBytes = 16 * 1024
	// crashReportMaxMessageRunes caps the one-line message. It is
	// counted in characters because GDScript truncates by
	// character.
	crashReportMaxMessageRunes = 512
	// crashReportMaxExcerptBytes caps the log excerpt. The payload
	// cap above binds first in practice; this keeps a bound on
	// the excerpt alone.
	crashReportMaxExcerptBytes = 12 * 1024
	crashReportMaxOSBytes      = 64
	crashReportMaxProtocol     = 1_000_000

	// crashReportUserLimit reports per crashReportUserWindow per
	// session user. A healthy client sends none; a client in an
	// error loop dedupes by signature and caps itself at the same
	// number per run.
	crashReportUserLimit  = 5
	crashReportUserWindow = time.Hour
	// crashReportServerDailyCap is the per-UTC-day budget shared
	// by every HTTP-key caller (all dedicated game servers, and
	// anyone else holding the key).
	crashReportServerDailyCap = 200
	// crashReportGlobalDailyCap is the per-UTC-day budget for
	// session users. HTTP-key callers draw only on the server
	// cap above (SP61), so a client flood cannot starve the
	// servers; the storage bound is the sum of both caps, and
	// crashReportMaxStored is the hard bound regardless.
	// (30 days at both caps and
	// the 16 KiB payload limit is under 250 MB.
	crashReportGlobalDailyCap = 500
	// crashReportLimiterSweepSize is the number of tracked users
	// above which the limiter drops users whose window is empty,
	// so the map cannot grow without bound.
	crashReportLimiterSweepSize = 10_000

	// crashReportRetention is how long a report is kept.
	crashReportRetention = 30 * 24 * time.Hour
	// crashReportMaxStored caps how many reports are kept, whatever
	// their age. The daily caps bound normal growth, but they live
	// in memory and reset whenever Nakama restarts (every runtime
	// deploy restarts it), so this is the hard bound: 20k reports at
	// the 16 KiB payload limit is about 320 MB. The prune drops the
	// oldest reports past it.
	crashReportMaxStored = 20_000
	// crashReportCronInterval is how often the prune runs. The
	// first run is at boot.
	crashReportCronInterval = time.Hour
	// crashReportPruneBatch and crashReportPruneMaxPages bound one
	// prune pass: at most 10k expired reports, far above the 500
	// a day the global cap allows. Anything left over goes on the
	// next pass.
	crashReportPruneBatch    = 100
	crashReportPruneMaxPages = 100

	// crashReportKeyTimeLayout is the fixed-width UTC timestamp a
	// key starts with. Fixed width makes string order match time
	// order. The trailing Z is a literal.
	crashReportKeyTimeLayout = "2006-01-02T15:04:05.000000Z"
)

var (
	// crashReportSignaturePattern: the first 16 hex characters of
	// a SHA-256 over the report's normalized first error lines,
	// computed by the game.
	crashReportSignaturePattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
	crashReportVersionPattern   = regexp.MustCompile(
		`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,31}$`)
	crashReportPlatformPattern = regexp.MustCompile(`^[a-z0-9_]{1,16}$`)
)

// crashReportRedactions run, in order, over the message and the
// log excerpt before storage.
var crashReportRedactions = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// Nakama session and refresh tokens are JWTs; the header of
	// every JWT encodes to "eyJ".
	{
		regexp.MustCompile(
			`eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]*\.[A-Za-z0-9_-]*`),
		"<jwt>",
	},
	// An Authorization value. Requiring a digit or one of +/=
	// keeps the English word "basic" from eating the next word.
	{
		regexp.MustCompile(
			`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/-]*[0-9+/=]` +
				`[A-Za-z0-9._~+/=-]*`),
		"${1} <redacted>",
	},
	// name=value and "name": "value" pairs whose name says secret
	// or account id. A device id logs its account in, so it is a
	// credential (the SDK's preview matchmaking logs one as
	// device_id=preview_<machine id>_C1).
	{
		regexp.MustCompile(
			`(?i)\b([a-z_]*(?:key|token|secret|password|device_id|` +
				`player_id|user_id))=[^&\s"',;]+`),
		"${1}=<redacted>",
	},
	{
		regexp.MustCompile(`(?i)([?&](?:code|state))=[^&\s"',;]+`),
		"${1}=<redacted>",
	},
	// The token in a signed signaling URL
	// (wss://signaling.../connect/<token>) admits its holder to a
	// match for five minutes.
	{
		regexp.MustCompile(`(/connect/)[A-Za-z0-9_=-]{16,}`),
		"${1}<redacted>",
	},
	{
		regexp.MustCompile(
			`(?i)"([a-z_]*(?:key|token|secret|password|device_id|` +
				`user_id|player_id|username|display_name|email))"` +
				`\s*:\s*"[^"]*"`),
		`"${1}":"<redacted>"`,
	},
	// Nakama user ids, session ids, and match ids are UUIDs. No
	// word boundaries: an id glued to a prefix ("party_<uuid>",
	// "preview_<uuid>_C1") is still an id.
	{
		regexp.MustCompile(
			`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-` +
				`[0-9a-f]{12}`),
		"<uuid>",
	},
	// The OS account name in a home directory path, such as
	// C:/Users/<name>/AppData/Roaming/Godot. It is often a real
	// name.
	{
		regexp.MustCompile(
			`(?i)\b([a-z]:[\\/](?:users|documents and settings)[\\/])` +
				`[^\\/\r\n"'<>|:*?]+`),
		"${1}<user>",
	},
	{
		regexp.MustCompile(`(/home/|/Users/)[^/\s"']+`),
		"${1}<user>",
	},
	// Anonymous players' device ids are 32 hex characters, glued
	// to a prefix or not.
	{
		regexp.MustCompile(`[0-9A-Fa-f]{32,}`),
		"<hex>",
	},
	// Steam, Facebook, and Google account ids are long numbers.
	{
		regexp.MustCompile(`\b\d{15,}\b`),
		"<number>",
	},
	{
		regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`),
		"<email>",
	},
	{
		regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`),
		"<ip>",
	},
}

// redactCrashReportText applies crashReportRedactions to s.
func redactCrashReportText(s string) string {
	for _, r := range crashReportRedactions {
		s = r.pattern.ReplaceAllString(s, r.replacement)
	}
	return s
}

// crashReportRequest is the RPC payload. Fields other than these
// are ignored, never stored. ProtocolVersion decodes as a float
// because GDScript's JSON round trip turns 2 into 2.0; it must
// still be a whole number.
type crashReportRequest struct {
	GameID          string  `json:"game_id"`
	GameVersion     string  `json:"game_version"`
	ProtocolVersion float64 `json:"protocol_version"`
	Platform        string  `json:"platform"`
	OS              string  `json:"os"`
	Role            string  `json:"role"`
	Message         string  `json:"message"`
	Signature       string  `json:"signature"`
	LogExcerpt      string  `json:"log_excerpt"`
}

// crashReportRecord is the stored value: the validated, redacted
// request plus the receive time. It has no field that identifies
// the caller.
type crashReportRecord struct {
	GameID          string `json:"game_id"`
	GameVersion     string `json:"game_version"`
	ProtocolVersion int    `json:"protocol_version"`
	Platform        string `json:"platform"`
	OS              string `json:"os"`
	Role            string `json:"role"`
	Message         string `json:"message"`
	Signature       string `json:"signature"`
	LogExcerpt      string `json:"log_excerpt"`
	ReceivedAt      string `json:"received_at"`
}

type crashReportResponse struct {
	Accepted bool   `json:"accepted"`
	ID       string `json:"id"`
}

// crashReportInvalid is the INVALID_ARGUMENT error every
// validation failure returns. Game clients treat code 3 as final
// and drop the report instead of retrying it.
func crashReportInvalid(format string, args ...any) error {
	return runtime.NewError(
		"invalid crash report: "+fmt.Sprintf(format, args...), 3)
}

// parseCrashReport decodes and validates a payload and returns the
// record to store. isSession says whether the caller came through
// a client session (true) or the HTTP key (false); sessionGameID
// is the game_id the session was minted with, "" when it has none.
func parseCrashReport(
	payload string,
	games *perGameConfig,
	isSession bool,
	sessionGameID string,
	now time.Time,
) (*crashReportRecord, error) {
	if payload == "" {
		return nil, crashReportInvalid("payload required")
	}
	if len(payload) > crashReportMaxPayloadBytes {
		return nil, crashReportInvalid(
			"payload is %d bytes; the limit is %d",
			len(payload), crashReportMaxPayloadBytes)
	}
	req := crashReportRequest{}
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return nil, crashReportInvalid("payload is not JSON: %v", err)
	}

	role := strings.TrimSpace(req.Role)
	switch {
	case isSession && role != crashReportRoleClient:
		return nil, crashReportInvalid(
			"role must be %q for a session caller",
			crashReportRoleClient)
	case !isSession && role != crashReportRoleServer:
		return nil, crashReportInvalid(
			"role must be %q for an HTTP-key caller",
			crashReportRoleServer)
	}

	gameID := strings.TrimSpace(req.GameID)
	if gameID == "" {
		return nil, crashReportInvalid("game_id required")
	}
	if isSession && sessionGameID != "" && sessionGameID != gameID {
		return nil, crashReportInvalid(
			"game_id %q does not match the session's game", gameID)
	}
	if _, ok := games.Get(gameID); !ok {
		return nil, crashReportInvalid("unknown game_id %q", gameID)
	}

	gameVersion := strings.TrimSpace(req.GameVersion)
	if !crashReportVersionPattern.MatchString(gameVersion) {
		return nil, crashReportInvalid(
			"game_version must be 1-32 characters of [0-9A-Za-z.+_-]")
	}
	if req.ProtocolVersion < 0 ||
		req.ProtocolVersion > crashReportMaxProtocol ||
		req.ProtocolVersion != math.Trunc(req.ProtocolVersion) {
		return nil, crashReportInvalid(
			"protocol_version must be a whole number from 0 to %d",
			crashReportMaxProtocol)
	}
	platform := strings.TrimSpace(req.Platform)
	if !crashReportPlatformPattern.MatchString(platform) {
		return nil, crashReportInvalid(
			"platform must be 1-16 characters of [a-z0-9_]")
	}
	osName := sanitizeCrashReportLine(req.OS)
	if osName == "" || len(osName) > crashReportMaxOSBytes {
		return nil, crashReportInvalid(
			"os must be 1-%d bytes", crashReportMaxOSBytes)
	}
	signature := strings.TrimSpace(req.Signature)
	if !crashReportSignaturePattern.MatchString(signature) {
		return nil, crashReportInvalid(
			"signature must be 16 lowercase hex characters")
	}
	message := sanitizeCrashReportLine(req.Message)
	if message == "" {
		return nil, crashReportInvalid("message required")
	}
	if utf8.RuneCountInString(message) > crashReportMaxMessageRunes {
		return nil, crashReportInvalid(
			"message is over %d characters",
			crashReportMaxMessageRunes)
	}
	excerpt := sanitizeCrashReportExcerpt(req.LogExcerpt)
	if len(excerpt) > crashReportMaxExcerptBytes {
		return nil, crashReportInvalid(
			"log_excerpt is over %d bytes", crashReportMaxExcerptBytes)
	}

	return &crashReportRecord{
		GameID:          gameID,
		GameVersion:     gameVersion,
		ProtocolVersion: int(req.ProtocolVersion),
		Platform:        platform,
		OS:              osName,
		Role:            role,
		Message:         redactCrashReportText(message),
		Signature:       signature,
		LogExcerpt:      redactCrashReportText(excerpt),
		ReceivedAt:      now.UTC().Format(time.RFC3339),
	}, nil
}

// sanitizeCrashReportLine turns every control character into a
// space and trims the result, so a message or OS name is one
// printable line.
func sanitizeCrashReportLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

// sanitizeCrashReportExcerpt keeps newlines and tabs and drops
// every other control character.
func sanitizeCrashReportExcerpt(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// newCrashReportKey returns a storage key that sorts by receive
// time: the UTC timestamp, then 8 random hex characters so two
// reports in the same microsecond don't collide.
func newCrashReportKey(now time.Time) (string, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	return now.UTC().Format(crashReportKeyTimeLayout) + "-" +
		hex.EncodeToString(suffix), nil
}

// crashReportLimiter holds the RPC's rate limits: a sliding window
// per session user, and per-UTC-day counters for HTTP-key callers
// and for everyone. State is in memory only, like friendsLimiter:
// a Nakama restart grants everyone a fresh budget, which is
// harmless at these sizes.
type crashReportLimiter struct {
	mu sync.Mutex

	// perUser maps a session user id to the unix-nano times of
	// its accepted reports inside the current window, oldest
	// first. The id is the map key only; it is never stored.
	perUser map[string][]int64
	// day is the UTC date the daily counters belong to.
	day         string
	serverToday int
	totalToday  int
	// serverCapHit and globalCapHit record that a daily cap has
	// rejected a report today, so the first rejection is logged and
	// the rest are not.
	serverCapHit bool
	globalCapHit bool

	// now is a clock indirection for tests.
	now func() time.Time
}

func newCrashReportLimiter() *crashReportLimiter {
	return &crashReportLimiter{
		perUser: map[string][]int64{},
		now:     time.Now,
	}
}

// reserve counts one report against every cap that applies to the
// caller, or returns a RESOURCE_EXHAUSTED (8) error and counts
// nothing when any cap is full. userID "" is an HTTP-key caller.
// firstCapHit names a daily cap ("global" or "server") on its first
// rejection of the UTC day, so the caller can log it once: a full
// global cap turns away every player's report until midnight.
func (l *crashReportLimiter) reserve(
	userID string,
) (firstCapHit string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.rollDay(now)
	// The global cap binds session users only (SP61):
	// servers draw on their own budget below.
	if userID != "" && l.totalToday >= crashReportGlobalDailyCap {
		if !l.globalCapHit {
			l.globalCapHit = true
			firstCapHit = "global"
		}
		return firstCapHit, runtime.NewError(
			"crash report daily capacity reached; try again tomorrow",
			8)
	}
	if userID == "" {
		if l.serverToday >= crashReportServerDailyCap {
			if !l.serverCapHit {
				l.serverCapHit = true
				firstCapHit = "server"
			}
			return firstCapHit, runtime.NewError(
				"crash report daily server capacity reached;"+
					" try again tomorrow", 8)
		}
		l.serverToday++
	} else {
		stamps := l.windowFor(userID, now)
		if len(stamps) >= crashReportUserLimit {
			return "", runtime.NewError(
				"crash report rate limit reached; try again later", 8)
		}
		l.perUser[userID] = append(stamps, now.UnixNano())
	}
	if userID != "" {
		l.totalToday++
	}
	return "", nil
}

// release gives back a reservation whose report was not stored.
func (l *crashReportLimiter) release(userID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if userID == "" {
		if l.serverToday > 0 {
			l.serverToday--
		}
		return
	}
	if l.totalToday > 0 {
		l.totalToday--
	}
	stamps := l.perUser[userID]
	switch {
	case len(stamps) > 1:
		l.perUser[userID] = stamps[:len(stamps)-1]
	case len(stamps) == 1:
		delete(l.perUser, userID)
	}
}

// rollDay resets the daily counters when the UTC date changes.
// Caller holds mu.
func (l *crashReportLimiter) rollDay(now time.Time) {
	day := now.UTC().Format(time.DateOnly)
	if day == l.day {
		return
	}
	l.day = day
	l.serverToday = 0
	l.totalToday = 0
	l.serverCapHit = false
	l.globalCapHit = false
}

// windowFor returns userID's timestamps still inside the window
// and writes the pruned slice back. When the map is large it also
// drops every user whose window has emptied. Caller holds mu.
func (l *crashReportLimiter) windowFor(
	userID string,
	now time.Time,
) []int64 {
	cutoff := now.Add(-crashReportUserWindow).UnixNano()
	if len(l.perUser) > crashReportLimiterSweepSize {
		for id, stamps := range l.perUser {
			if len(stamps) == 0 || stamps[len(stamps)-1] < cutoff {
				delete(l.perUser, id)
			}
		}
	}
	stamps := l.perUser[userID]
	first := 0
	for first < len(stamps) && stamps[first] < cutoff {
		first++
	}
	stamps = stamps[first:]
	if len(stamps) == 0 {
		delete(l.perUser, userID)
	} else {
		l.perUser[userID] = stamps
	}
	return stamps
}

// crashReportRpcFactory returns the `crash_report` RPC handler.
//
// Request: a JSON object with game_id, game_version,
// protocol_version, platform, os, role, message, signature, and
// optionally log_excerpt (see crashReportRequest).
// Response: {"accepted": true, "id": "<storage key>"}.
// Errors: INVALID_ARGUMENT (3) for any validation failure,
// RESOURCE_EXHAUSTED (8) for a rate limit, INTERNAL (13) when the
// write fails.
func crashReportRpcFactory(
	games *perGameConfig,
	limiter *crashReportLimiter,
) func(
	context.Context, runtime.Logger, *sql.DB,
	runtime.NakamaModule, string,
) (string, error) {
	return func(
		ctx context.Context,
		logger runtime.Logger,
		_ *sql.DB,
		nk runtime.NakamaModule,
		payload string,
	) (string, error) {
		userID, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
		vars, _ := ctx.Value(runtime.RUNTIME_CTX_VARS).(map[string]string)
		now := limiter.now()
		record, err := parseCrashReport(
			payload, games, userID != "", gameIDFromVars(vars), now)
		if err != nil {
			return "", err
		}
		capHit, err := limiter.reserve(userID)
		if capHit != "" {
			logger.Warn(
				"crash_report %s daily cap reached; rejecting those"+
					" reports until 00:00 UTC", capHit)
		}
		if err != nil {
			return "", err
		}
		key, err := newCrashReportKey(now)
		if err != nil {
			limiter.release(userID)
			return "", runtime.NewError("failed to create report key", 13)
		}
		value, err := json.Marshal(record)
		if err != nil {
			limiter.release(userID)
			return "", runtime.NewError("failed to encode report", 13)
		}
		if _, err := nk.StorageWrite(ctx, []*runtime.StorageWrite{{
			Collection:      crashReportCollection,
			Key:             key,
			UserID:          "",
			Value:           string(value),
			Version:         "*",
			PermissionRead:  0,
			PermissionWrite: 0,
		}}); err != nil {
			limiter.release(userID)
			logger.Error("crash_report storage write: %v", err)
			return "", runtime.NewError("failed to store crash report", 13)
		}
		logger.WithFields(map[string]interface{}{
			"crash_report_key": key,
			"game_id":          record.GameID,
			"game_version":     record.GameVersion,
			"platform":         record.Platform,
			"role":             record.Role,
			"signature":        record.Signature,
		}).Info("crash_report accepted")
		out, err := json.Marshal(crashReportResponse{
			Accepted: true,
			ID:       key,
		})
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
}

// startCrashReportCron launches the retention goroutine. Like
// account_cron it uses context.Background(), because the
// InitModule context is cancelled as soon as InitModule returns,
// and plugin reload is the only stop signal.
func startCrashReportCron(
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
) {
	go runCrashReportCron(
		context.Background(), logger, nk, crashReportStoredCounter(db))
}

// crashReportStoredCounter counts stored reports with one
// read-only query. Listing them through StorageList would read
// every value just to count them.
func crashReportStoredCounter(
	db *sql.DB,
) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		stored := 0
		err := db.QueryRowContext(
			ctx,
			`SELECT count(*) FROM storage
WHERE collection = $1 AND user_id = $2`,
			crashReportCollection, systemUserID,
		).Scan(&stored)
		return stored, err
	}
}

// runCrashReportCron prunes at boot and then every
// crashReportCronInterval.
func runCrashReportCron(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	countStored func(context.Context) (int, error),
) {
	logger.Info(
		"crash_report_cron started (interval=%s retention=%s max=%d)",
		crashReportCronInterval, crashReportRetention,
		crashReportMaxStored)
	pruneCrashReports(
		ctx, logger, nk, countStored, crashReportMaxStored, time.Now())
	ticker := time.NewTicker(crashReportCronInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("crash_report_cron stopped: %v", ctx.Err())
			return
		case <-ticker.C:
			pruneCrashReports(
				ctx, logger, nk, countStored, crashReportMaxStored,
				time.Now())
		}
	}
}

// pruneCrashReports deletes reports received more than
// crashReportRetention before now, then, when more than maxStored
// are left (countStored says how many; nil skips this step), the
// oldest of those. Returns how many it deleted.
//
// Nakama lists one owner's objects in key order (StorageList pages
// with a key cursor), and keys start with the receive time, so the
// age scan stops at the first well-formed key inside the retention
// window. A key this file did not write (no timestamp prefix) is
// left alone.
func pruneCrashReports(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	countStored func(context.Context) (int, error),
	maxStored int,
	now time.Time,
) int {
	deleted := pruneExpiredCrashReports(ctx, logger, nk, now)
	if countStored == nil {
		return deleted
	}
	stored, err := countStored(ctx)
	if err != nil {
		logger.Warn("crash_report_cron count: %v", err)
		return deleted
	}
	if excess := stored - maxStored; excess > 0 {
		dropped := deleteOldestCrashReports(ctx, logger, nk, excess)
		logger.Warn(
			"crash_report_cron dropped the %d oldest report(s): %d"+
				" were stored, the cap is %d",
			dropped, stored, maxStored)
		deleted += dropped
	}
	return deleted
}

// pruneExpiredCrashReports is the age half of pruneCrashReports.
func pruneExpiredCrashReports(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	now time.Time,
) int {
	cutoff := now.Add(-crashReportRetention)
	cursor := ""
	deleted := 0
	for page := 0; page < crashReportPruneMaxPages; page++ {
		objects, next, err := nk.StorageList(
			ctx, "", systemUserID, crashReportCollection,
			crashReportPruneBatch, cursor)
		if err != nil {
			logger.Warn("crash_report_cron list: %v", err)
			break
		}
		expired, reachedRetained := expiredCrashReports(objects, cutoff)
		if len(expired) > 0 {
			if err := nk.StorageDelete(ctx, expired); err != nil {
				logger.Warn("crash_report_cron delete: %v", err)
				break
			}
			deleted += len(expired)
		}
		if reachedRetained || next == "" {
			break
		}
		cursor = next
	}
	if deleted > 0 {
		logger.Info(
			"crash_report_cron pruned %d report(s) received before %s",
			deleted, cutoff.UTC().Format(time.RFC3339))
	}
	return deleted
}

// deleteOldestCrashReports deletes up to count reports, oldest
// first, skipping keys this file did not write. Returns how many it
// deleted.
func deleteOldestCrashReports(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	count int,
) int {
	cursor := ""
	deleted := 0
	for page := 0; page < crashReportPruneMaxPages && deleted < count; page++ {
		objects, next, err := nk.StorageList(
			ctx, "", systemUserID, crashReportCollection,
			min(crashReportPruneBatch, count-deleted), cursor)
		if err != nil {
			logger.Warn("crash_report_cron list: %v", err)
			break
		}
		doomed := make([]*runtime.StorageDelete, 0, len(objects))
		for _, obj := range objects {
			if _, ok := crashReportKeyTime(obj.GetKey()); !ok {
				continue
			}
			doomed = append(doomed, &runtime.StorageDelete{
				Collection: crashReportCollection,
				Key:        obj.GetKey(),
				UserID:     obj.GetUserId(),
			})
		}
		if len(doomed) > 0 {
			if err := nk.StorageDelete(ctx, doomed); err != nil {
				logger.Warn("crash_report_cron delete: %v", err)
				break
			}
			deleted += len(doomed)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return deleted
}

// expiredCrashReports returns deletes for the objects whose key
// time is before cutoff, and whether the page reached a report
// that is still inside the retention window.
func expiredCrashReports(
	objects []*api.StorageObject,
	cutoff time.Time,
) ([]*runtime.StorageDelete, bool) {
	expired := make([]*runtime.StorageDelete, 0, len(objects))
	for _, obj := range objects {
		received, ok := crashReportKeyTime(obj.GetKey())
		if !ok {
			continue
		}
		if !received.Before(cutoff) {
			return expired, true
		}
		expired = append(expired, &runtime.StorageDelete{
			Collection: crashReportCollection,
			Key:        obj.GetKey(),
			UserID:     obj.GetUserId(),
		})
	}
	return expired, false
}

// crashReportKeyTime parses the receive time a key starts with.
func crashReportKeyTime(key string) (time.Time, bool) {
	if len(key) < len(crashReportKeyTimeLayout) {
		return time.Time{}, false
	}
	t, err := time.Parse(
		crashReportKeyTimeLayout, key[:len(crashReportKeyTimeLayout)])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
