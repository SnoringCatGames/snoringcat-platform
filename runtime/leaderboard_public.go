package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// The public leaderboard: an unauthenticated, read-only HTTP route on
// Nakama's client API port (behind Caddy at
// https://nakama.snoringcat.games), for the game websites:
//
//	GET /snoringcat/leaderboard?game_id=hopnbop&leaderboard=ffa&limit=50
//
// A static page can't hold a credential, and the runtime HTTP key
// must never reach a browser, so the route is registered with
// RegisterHttp, which Nakama does not authenticate (like
// versionCheckHTTPPath).
//
// Privacy. The response carries, per player, only a rank, the
// player's display name, and their score. Never a user id, username,
// avatar URL, device id, IP address, record time, or record metadata.
// A player is listed only when all of these hold:
//   - the account still exists;
//   - it is not banned (no disable_time);
//   - it is not queued for deletion (no account_deletion_queue row),
//     whatever its display name says;
//   - its display name is not delete_account's "[deleted]" marker;
//   - it has a display name. Anonymous (device-only) accounts have
//     none, and the in-game leaderboard hides them the same way;
//   - its username doesn't start with "probe-", the
//     synthetic-match-probe's identities (the same filter
//     claude-config's player-metrics-daily uses).
//
// Ranks count listed players only, so a hidden account holds no
// place. Players with equal scores share a rank (1, 2, 2, 4); within
// a tie the order is Nakama's.
//
// Load. Each (game, leaderboard) pair is read from Postgres at most
// once per publicLeaderboardCacheTTL, by one request while concurrent
// ones wait for its result, whatever their limit. A failed read is
// retried no more often than publicLeaderboardRetryInterval, and in
// the meantime the last good copy is served while it is younger than
// publicLeaderboardStaleTTL. Caddy also rate-limits the route per IP
// (infra/remote/nakama/Caddyfile).

// publicLeaderboardHTTPPath is the route. It sits outside /v2/ so it
// can never collide with Nakama's own routes.
const publicLeaderboardHTTPPath = "/snoringcat/leaderboard"

// publicLeaderboardVersion is the response schema version. Bump it
// only for a change existing clients can't read; adding a field is
// not one.
const publicLeaderboardVersion = 1

const (
	// publicLeaderboardDefaultLimit is the entry count when the
	// request names none.
	publicLeaderboardDefaultLimit = 50
	// publicLeaderboardMaxLimit is the largest limit accepted, and
	// the number of entries each cached copy holds.
	publicLeaderboardMaxLimit = 100
	// publicLeaderboardCacheTTL is how long a copy is served before
	// it is read again. Also the response's Cache-Control max-age.
	publicLeaderboardCacheTTL = 60 * time.Second
	// publicLeaderboardStaleTTL bounds how old a copy served after a
	// failed read may be. It also bounds how long a deletion made
	// just before a Postgres outage could stay listed.
	publicLeaderboardStaleTTL = 10 * time.Minute
	// publicLeaderboardRetryInterval is the wait after a failed read
	// before the next read is attempted.
	publicLeaderboardRetryInterval = 10 * time.Second
	// publicLeaderboardReadTimeout caps one read of the leaderboard
	// and its players.
	publicLeaderboardReadTimeout = 5 * time.Second
	// publicLeaderboardPageSize is the record count fetched per page.
	publicLeaderboardPageSize = 100
	// publicLeaderboardMaxPages caps the pages one read scans while
	// collecting publicLeaderboardMaxLimit listed players.
	publicLeaderboardMaxPages = 5
)

// probeUsernamePrefix marks the synthetic-match-probe's accounts
// (claude-config's _synthetic-match-probe-bin/probe.go sets the
// username at first sign-in).
const probeUsernamePrefix = "probe-"

// publicLeaderboardAllowedMethods is the Allow header value.
const publicLeaderboardAllowedMethods = "GET, HEAD, OPTIONS"

// errPublicLeaderboardBackoff is the error a read gets while the
// retry interval after a failed read hasn't passed.
var errPublicLeaderboardBackoff = errors.New(
	"waiting to retry after a failed read")

// publicLeaderboardEntry is one listed player. These three fields are
// everything the route reveals about a player.
type publicLeaderboardEntry struct {
	Rank        int64  `json:"rank"`
	DisplayName string `json:"display_name"`
	Score       int64  `json:"score"`
}

// publicLeaderboardResponse is the 200 response body.
type publicLeaderboardResponse struct {
	Version     int    `json:"version"`
	GameID      string `json:"game_id"`
	Leaderboard string `json:"leaderboard"`
	// SortOrder is "desc" (higher scores rank first) or "asc".
	SortOrder string `json:"sort_order"`
	// Operator says how a player's score combines their results:
	// "best" (best single result), "set" (latest), "incr" or "decr"
	// (running total).
	Operator string `json:"operator"`
	// NextResetAt is when the board next resets (RFC 3339, UTC), or
	// null for a board that never resets.
	NextResetAt *string `json:"next_reset_at"`
	// GeneratedAt is when this copy was read (RFC 3339, UTC).
	GeneratedAt string                   `json:"generated_at"`
	Entries     []publicLeaderboardEntry `json:"entries"`
}

// publicLeaderboardError is the body of every non-200 response.
type publicLeaderboardError struct {
	Version int    `json:"version"`
	Error   string `json:"error"`
	Message string `json:"message"`
}

// publicLeaderboardRequest is a validated request.
type publicLeaderboardRequest struct {
	gameID      string
	leaderboard string
	limit       int
}

// nakamaID is the board's Nakama ID.
func (r publicLeaderboardRequest) nakamaID() string {
	return gameScopedLeaderboardID(r.gameID, r.leaderboard)
}

// publicLeaderboardSnapshot is one read of a board. Never modified
// after it is cached, so requests share it without locking.
type publicLeaderboardSnapshot struct {
	sortOrder string
	operator  string
	// nextReset is zero for a board that never resets.
	nextReset   time.Time
	generatedAt time.Time
	// entries are the top listed players, ranked, at most
	// publicLeaderboardMaxLimit.
	entries []publicLeaderboardEntry
}

// publicLeaderboardCache holds one snapshot per board.
type publicLeaderboardCache struct {
	ttl           time.Duration
	staleTTL      time.Duration
	retryInterval time.Duration
	// now is the clock. Tests replace it.
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*publicLeaderboardCacheEntry
}

// publicLeaderboardCacheEntry is one board's slot. mu is held for the
// whole of a read, which is what makes concurrent requests share it.
type publicLeaderboardCacheEntry struct {
	mu          sync.Mutex
	snapshot    *publicLeaderboardSnapshot
	lastFailure time.Time
}

func newPublicLeaderboardCache() *publicLeaderboardCache {
	return &publicLeaderboardCache{
		ttl:           publicLeaderboardCacheTTL,
		staleTTL:      publicLeaderboardStaleTTL,
		retryInterval: publicLeaderboardRetryInterval,
		now:           time.Now,
		entries:       map[string]*publicLeaderboardCacheEntry{},
	}
}

// get returns key's snapshot, calling read when the cached one is
// older than ttl. The snapshot is stamped with the time the read
// started. After a failed read, read isn't called again until
// retryInterval has passed, and the old snapshot is returned while it
// is younger than staleTTL. Keys come only from configured boards, so
// the map stays as small as the number of boards.
func (c *publicLeaderboardCache) get(
	key string,
	read func() (*publicLeaderboardSnapshot, error),
) (*publicLeaderboardSnapshot, error) {
	c.mu.Lock()
	entry, ok := c.entries[key]
	if !ok {
		entry = &publicLeaderboardCacheEntry{}
		c.entries[key] = entry
	}
	c.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	now := c.now()
	if entry.snapshot != nil &&
		now.Sub(entry.snapshot.generatedAt) < c.ttl {
		return entry.snapshot, nil
	}
	err := errPublicLeaderboardBackoff
	if entry.lastFailure.IsZero() ||
		now.Sub(entry.lastFailure) >= c.retryInterval {
		var fresh *publicLeaderboardSnapshot
		fresh, err = read()
		if err == nil {
			fresh.generatedAt = now
			entry.snapshot = fresh
			entry.lastFailure = time.Time{}
			return fresh, nil
		}
		entry.lastFailure = now
	}
	if entry.snapshot != nil &&
		now.Sub(entry.snapshot.generatedAt) < c.staleTTL {
		return entry.snapshot, nil
	}
	return nil, err
}

// publicLeaderboardHTTPHandler serves publicLeaderboardHTTPPath.
func publicLeaderboardHTTPHandler(
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	cache *publicLeaderboardCache,
) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		// Public data for any page on any site. Nakama's CORS
		// middleware sets the same Allow-Origin and answers real
		// preflights before this handler runs; setting it here keeps
		// the contract independent of that middleware.
		header.Set("Access-Control-Allow-Origin", "*")
		// Lets a page under Cross-Origin-Embedder-Policy load the
		// response even without CORS.
		header.Set("Cross-Origin-Resource-Policy", "cross-origin")
		// Display names are player-chosen; never let a browser sniff
		// the body as anything but JSON.
		header.Set("X-Content-Type-Options", "nosniff")

		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodOptions:
			header.Set("Allow", publicLeaderboardAllowedMethods)
			header.Set("Access-Control-Allow-Methods",
				publicLeaderboardAllowedMethods)
			header.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			header.Set("Allow", publicLeaderboardAllowedMethods)
			writePublicLeaderboardError(w, http.StatusMethodNotAllowed,
				"method_not_allowed", "use GET")
			return
		}

		req, status, failure := parsePublicLeaderboardRequest(
			r.URL.Query(), games)
		if failure != nil {
			writePublicLeaderboardError(
				w, status, failure.Error, failure.Message)
			return
		}
		snapshot, err := cache.get(req.nakamaID(),
			func() (*publicLeaderboardSnapshot, error) {
				// Not the request's context: every waiting request
				// shares this read, so one client hanging up must not
				// cancel it.
				ctx, cancel := context.WithTimeout(
					context.Background(), publicLeaderboardReadTimeout)
				defer cancel()
				snapshot, err := readPublicLeaderboard(
					ctx, nk, req.nakamaID())
				if err != nil {
					logger.Warn("public leaderboard %s: read failed: %v",
						req.nakamaID(), err)
				}
				return snapshot, err
			})
		if err != nil {
			header.Set("Retry-After",
				strconv.Itoa(int(publicLeaderboardRetryInterval.Seconds())))
			writePublicLeaderboardError(w, http.StatusServiceUnavailable,
				"unavailable", "the leaderboard is temporarily unavailable")
			return
		}
		out, err := json.Marshal(
			buildPublicLeaderboardResponse(req, snapshot))
		if err != nil {
			writePublicLeaderboardError(w,
				http.StatusInternalServerError, "internal",
				"could not encode the leaderboard")
			return
		}
		age := cache.now().Sub(snapshot.generatedAt)
		if age < 0 {
			age = 0
		}
		header.Set("Content-Type", "application/json")
		// Set, not Add: Nakama adds "no-store, no-cache,
		// must-revalidate" to every response before this handler runs.
		header.Set("Cache-Control", fmt.Sprintf("public, max-age=%d",
			int(publicLeaderboardCacheTTL.Seconds())))
		// How old the copy already is, so a browser keeps it only for
		// the rest of its 60 seconds rather than 60 more.
		header.Set("Age", strconv.Itoa(int(age.Seconds())))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}
}

// parsePublicLeaderboardRequest validates the query: game_id and
// leaderboard are required and must name a registered game and one
// of the boards its config declares; limit is optional, a whole
// number from 1 to publicLeaderboardMaxLimit. On failure it returns
// the HTTP status and the error body. Error messages never echo the
// request's values.
func parsePublicLeaderboardRequest(
	query url.Values,
	games *perGameConfig,
) (publicLeaderboardRequest, int, *publicLeaderboardError) {
	fail := func(status int, code, message string) (
		publicLeaderboardRequest, int, *publicLeaderboardError,
	) {
		return publicLeaderboardRequest{}, status, &publicLeaderboardError{
			Version: publicLeaderboardVersion,
			Error:   code,
			Message: message,
		}
	}
	req := publicLeaderboardRequest{
		gameID:      query.Get("game_id"),
		leaderboard: query.Get("leaderboard"),
		limit:       publicLeaderboardDefaultLimit,
	}
	if req.gameID == "" {
		return fail(http.StatusBadRequest, "missing_game_id",
			"game_id is required")
	}
	if req.leaderboard == "" {
		return fail(http.StatusBadRequest, "missing_leaderboard",
			"leaderboard is required")
	}
	if raw, ok := query["limit"]; ok {
		limit, err := strconv.Atoi(firstValue(raw))
		if err != nil || limit < 1 || limit > publicLeaderboardMaxLimit {
			return fail(http.StatusBadRequest, "invalid_limit",
				fmt.Sprintf("limit must be a whole number from 1 to %d",
					publicLeaderboardMaxLimit))
		}
		req.limit = limit
	}
	gc, ok := games.Get(req.gameID)
	if !ok {
		return fail(http.StatusNotFound, "unknown_game",
			"no game is registered under that game_id")
	}
	if _, ok := findLeaderboardConfig(gc, req.leaderboard); !ok {
		return fail(http.StatusNotFound, "unknown_leaderboard",
			"that game declares no leaderboard with that id")
	}
	return req, http.StatusOK, nil
}

// firstValue returns values[0], or "" when there is none.
func firstValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// buildPublicLeaderboardResponse slices a snapshot to the request's
// limit.
func buildPublicLeaderboardResponse(
	req publicLeaderboardRequest,
	snapshot *publicLeaderboardSnapshot,
) publicLeaderboardResponse {
	entries := snapshot.entries
	if len(entries) > req.limit {
		entries = entries[:req.limit]
	}
	if entries == nil {
		// An empty board is [], never null.
		entries = []publicLeaderboardEntry{}
	}
	var nextReset *string
	if !snapshot.nextReset.IsZero() {
		formatted := snapshot.nextReset.UTC().Format(time.RFC3339)
		nextReset = &formatted
	}
	return publicLeaderboardResponse{
		Version:     publicLeaderboardVersion,
		GameID:      req.gameID,
		Leaderboard: req.leaderboard,
		SortOrder:   snapshot.sortOrder,
		Operator:    snapshot.operator,
		NextResetAt: nextReset,
		GeneratedAt: snapshot.generatedAt.UTC().Format(time.RFC3339),
		Entries:     entries,
	}
}

// writePublicLeaderboardError writes an error body. Errors are never
// cached.
func writePublicLeaderboardError(
	w http.ResponseWriter,
	status int,
	code string,
	message string,
) {
	out, _ := json.Marshal(publicLeaderboardError{
		Version: publicLeaderboardVersion,
		Error:   code,
		Message: message,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// publicLeaderboardCandidate is a record that passed the listing
// rules, before ranking.
type publicLeaderboardCandidate struct {
	displayName string
	score       int64
	subscore    int64
}

// readPublicLeaderboard reads a board's settings and its top listed
// players. Records come back in board order; pages are read until
// publicLeaderboardMaxLimit players are listed, the board runs out,
// or publicLeaderboardMaxPages pages have been scanned.
func readPublicLeaderboard(
	ctx context.Context,
	nk runtime.NakamaModule,
	nakamaID string,
) (*publicLeaderboardSnapshot, error) {
	boards, err := nk.LeaderboardsGetId(ctx, []string{nakamaID})
	if err != nil {
		return nil, err
	}
	if len(boards) == 0 || boards[0] == nil {
		return nil, fmt.Errorf(
			"%w: %s was never created", runtime.ErrLeaderboardNotFound,
			nakamaID)
	}
	board := boards[0]
	snapshot := &publicLeaderboardSnapshot{
		sortOrder: leaderboardSortOrderName(board.GetSortOrder()),
		operator:  leaderboardOperatorName(board.GetOperator()),
	}
	if next := board.GetNextReset(); next != 0 {
		snapshot.nextReset = time.Unix(int64(next), 0).UTC()
	}

	listed := make([]publicLeaderboardCandidate, 0,
		publicLeaderboardMaxLimit)
	cursor := ""
	for page := 0; page < publicLeaderboardMaxPages &&
		len(listed) < publicLeaderboardMaxLimit; page++ {
		records, _, next, _, err := nk.LeaderboardRecordsList(
			ctx, nakamaID, nil, publicLeaderboardPageSize, cursor, 0)
		if err != nil {
			return nil, err
		}
		kept, err := listablePublicRecords(ctx, nk, records)
		if err != nil {
			return nil, err
		}
		listed = append(listed, kept...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(listed) > publicLeaderboardMaxLimit {
		listed = listed[:publicLeaderboardMaxLimit]
	}
	snapshot.entries = rankPublicLeaderboard(listed)
	return snapshot, nil
}

// listablePublicRecords keeps the records whose owners may be listed
// (see the file comment), in order, with one account read and one
// storage read for the whole page.
func listablePublicRecords(
	ctx context.Context,
	nk runtime.NakamaModule,
	records []*api.LeaderboardRecord,
) ([]publicLeaderboardCandidate, error) {
	if len(records) == 0 {
		return nil, nil
	}
	ownerIDs := make([]string, 0, len(records))
	for _, record := range records {
		ownerIDs = append(ownerIDs, record.GetOwnerId())
	}
	accounts, err := nk.AccountsGetId(ctx, ownerIDs)
	if err != nil {
		return nil, err
	}
	accountByID := make(map[string]*api.Account, len(accounts))
	for _, account := range accounts {
		if id := account.GetUser().GetId(); id != "" {
			accountByID[id] = account
		}
	}
	pending, err := ownersPendingDeletion(ctx, nk, ownerIDs)
	if err != nil {
		return nil, err
	}
	kept := make([]publicLeaderboardCandidate, 0, len(records))
	for _, record := range records {
		owner := record.GetOwnerId()
		name, ok := publicDisplayName(accountByID[owner], pending[owner])
		if !ok {
			continue
		}
		kept = append(kept, publicLeaderboardCandidate{
			displayName: name,
			score:       record.GetScore(),
			subscore:    record.GetSubscore(),
		})
	}
	return kept, nil
}

// ownersPendingDeletion returns the users among userIDs that have an
// account_deletion_queue row (account.go), in one storage read.
func ownersPendingDeletion(
	ctx context.Context,
	nk runtime.NakamaModule,
	userIDs []string,
) (map[string]bool, error) {
	reads := make([]*runtime.StorageRead, 0, len(userIDs))
	for _, id := range userIDs {
		reads = append(reads, &runtime.StorageRead{
			Collection: accountDeletionCollection,
			Key:        accountDeletionKey,
			UserID:     id,
		})
	}
	objects, err := nk.StorageRead(ctx, reads)
	if err != nil {
		return nil, err
	}
	pending := make(map[string]bool, len(objects))
	for _, object := range objects {
		pending[object.GetUserId()] = true
	}
	return pending, nil
}

// publicDisplayName returns the name to list an account under, and
// false when the account may not be listed at all (see the file
// comment for the rules).
func publicDisplayName(
	account *api.Account,
	pendingDeletion bool,
) (string, bool) {
	user := account.GetUser()
	if user == nil || pendingDeletion {
		return "", false
	}
	if account.GetDisableTime().GetSeconds() != 0 {
		return "", false
	}
	name := strings.TrimSpace(user.GetDisplayName())
	if name == "" || name == anonymizedDisplayName {
		return "", false
	}
	if strings.HasPrefix(
		strings.ToLower(user.GetUsername()), probeUsernamePrefix) {
		return "", false
	}
	return name, true
}

// rankPublicLeaderboard numbers candidates already in board order.
// Equal (score, subscore) pairs share a rank, and the next distinct
// result takes its position: 1, 2, 2, 4.
func rankPublicLeaderboard(
	candidates []publicLeaderboardCandidate,
) []publicLeaderboardEntry {
	entries := make([]publicLeaderboardEntry, len(candidates))
	for i, candidate := range candidates {
		rank := int64(i + 1)
		if i > 0 &&
			candidate.score == candidates[i-1].score &&
			candidate.subscore == candidates[i-1].subscore {
			rank = entries[i-1].Rank
		}
		entries[i] = publicLeaderboardEntry{
			Rank:        rank,
			DisplayName: candidate.displayName,
			Score:       candidate.score,
		}
	}
	return entries
}
