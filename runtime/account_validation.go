package main

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Display-name and avatar validation (SP42, DN-2). Nakama's
// UpdateAccount API accepted any name and any avatar URL, and
// every other client fetched the URL as given. The
// BeforeUpdateAccount hook rejects updates whose name or
// avatar breaks the same rules the client cache enforces
// (H60). Authentication never runs this hook, so accounts
// whose stored names predate the rules still sign in; only
// new updates must comply.

const (
	// maxDisplayNameRunes bounds display names on update.
	maxDisplayNameRunes = 32
	// maxAvatarURLBytes bounds avatar URLs on update.
	maxAvatarURLBytes = 512
	// reservedDisplayName is the soft-delete placeholder; a
	// live player may not take it.
	reservedDisplayName = "[deleted]"
)

// avatarURLAllowedHost reports whether a host may serve
// avatars. The patterns are the client's
// (ProfileImageCache, SP88): exact provider hosts, so
// other subdomains of the same parents do not qualify.
func avatarURLAllowedHost(host string) bool {
	switch host {
	case "platform-lookaside.fbsbx.com",
		"avatars.steamstatic.com",
		"avatars.akamai.steamstatic.com",
		"avatars.fastly.steamstatic.com",
		"avatars.cloudflare.steamstatic.com":
		return true
	}
	if strings.HasSuffix(host, ".googleusercontent.com") {
		label := strings.TrimSuffix(
			host, ".googleusercontent.com")
		if !strings.HasPrefix(label, "lh") {
			return false
		}
		digits := label[2:]
		if digits == "" {
			return false
		}
		for _, r := range digits {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	if strings.HasSuffix(host, ".fbcdn.net") {
		head := strings.TrimSuffix(host, ".fbcdn.net")
		return head == "scontent" ||
			strings.HasPrefix(head, "scontent.") ||
			strings.HasPrefix(head, "scontent-")
	}
	return false
}

// registerAccountValidationHook installs the
// BeforeUpdateAccount hook.
func registerAccountValidationHook(
	initializer runtime.Initializer,
) error {
	return initializer.RegisterBeforeUpdateAccount(
		func(
			_ context.Context,
			_ runtime.Logger,
			_ *sql.DB,
			_ runtime.NakamaModule,
			in *api.UpdateAccountRequest,
		) (*api.UpdateAccountRequest, error) {
			if in.DisplayName != nil {
				if err := validateDisplayName(
					in.DisplayName.Value); err != nil {
					return nil, err
				}
				// Store the name as checked: trimmed,
				// so what is kept is what was validated.
				in.DisplayName.Value = strings.TrimSpace(
					in.DisplayName.Value)
			}
			if in.AvatarUrl != nil {
				if err := validateAvatarURL(
					in.AvatarUrl.Value); err != nil {
					return nil, err
				}
			}
			return in, nil
		})
}

// clampDisplayName returns the name as it may be stored
// and whether that differs from the input: trimmed, cut
// to the display name bound, invisible and control
// characters dropped, and never the reserved name in any
// case. The result always passes validateDisplayName,
// except when nothing valid is left (empty, or no letter
// or digit survives cleaning): then it returns ("", true)
// and the caller stores a generated name instead, so an
// imported name can neither stay invalid nor diverge
// from what a later update would accept. Sign-in
// providers import names without the update hook, so a
// name that predates the rules (or arrives over the
// bound) is rewritten after authentication instead of
// failing later updates.
func clampDisplayName(name string) (string, bool) {
	trimmed := strings.TrimSpace(stripInvisibleRunes(name))
	runes := []rune(trimmed)
	if len(runes) > maxDisplayNameRunes {
		trimmed = strings.TrimSpace(
			string(runes[:maxDisplayNameRunes]))
		runes = []rune(trimmed)
	}
	if strings.EqualFold(trimmed, reservedDisplayName) &&
		len(runes) > 0 {
		trimmed = string(runes[:len(runes)-1])
	}
	if validateDisplayName(trimmed) != nil {
		return "", true
	}
	return trimmed, trimmed != name
}

// stripInvisibleRunes drops format and control characters
// and the line/paragraph separators: they render as
// nothing (or reorder neighbouring text) in a display
// name. A zero-width joiner or non-joiner between letters
// (or marks) is kept: Persian and Indic names need them
// to render correctly, as do ZWJ emoji sequences, so
// only a joiner outside that context drops.
func stripInvisibleRunes(name string) string {
	runes := []rune(name)
	var b strings.Builder
	b.Grow(len(name))
	for i, r := range runes {
		if isKeptJoiner(runes, i) {
			b.WriteRune(r)
			continue
		}
		if unicode.Is(unicode.Cf, r) ||
			unicode.Is(unicode.Zl, r) ||
			unicode.Is(unicode.Zp, r) ||
			unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isKeptJoiner reports whether the rune at i is a
// zero-width joiner or non-joiner in a context where it
// does orthographic work: between letters or marks
// (Persian ZWNJ, Indic conjuncts), or, for the joiner,
// between emoji in a ZWJ sequence. At a name edge or
// next to anything else it is invisible noise.
func isKeptJoiner(runes []rune, i int) bool {
	r := runes[i]
	if r != '\u200c' && r != '\u200d' {
		return false
	}
	if i == 0 || i == len(runes)-1 {
		return false
	}
	prev, next := runes[i-1], runes[i+1]
	if (unicode.IsLetter(prev) || unicode.Is(unicode.M, prev)) &&
		(unicode.IsLetter(next) || unicode.Is(unicode.M, next)) {
		return true
	}
	return r == '\u200d' &&
		unicode.Is(unicode.So, prev) &&
		unicode.Is(unicode.So, next)
}

// registerAfterAuthenticateHooks installs the one
// after-authenticate function for each provider. Nakama
// keeps a single function per provider: a later
// RegisterAfterAuthenticate* call for the same provider
// replaces the earlier one (SP106). Every after-sign-in
// behavior therefore runs inside these functions:
//
//   - a created account gets a generated display name
//     (H59), never the provider's profile name;
//   - an existing account whose stored name breaks the
//     display-name rules gets the clamped name (SP88);
//   - sign-in privacy runs on every sign-in
//     (enforceSignInPrivacy).
//
// Google, Facebook and Steam do all three. Apple and
// device generate and run privacy. Custom generates
// only.
func registerAfterAuthenticateHooks(
	initializer runtime.Initializer,
) error {
	clamp := func(
		ctx context.Context,
		logger runtime.Logger,
		nk runtime.NakamaModule,
		userID string,
	) {
		account, err := nk.AccountGetId(ctx, userID)
		if err != nil || account == nil ||
			account.GetUser() == nil {
			logger.Warn(
				"name clamp: account read user=%s err=%v",
				userID, err)
			return
		}
		name := account.GetUser().GetDisplayName()
		clamped, changed := clampDisplayName(name)
		if !changed {
			return
		}
		if clamped == "" {
			// Nothing valid survived cleaning: store a
			// generated name, like every new account gets.
			generated, genErr := generatedDisplayName()
			if genErr != nil {
				logger.Warn(
					"name clamp: generated name user=%s err=%v",
					userID, genErr)
				return
			}
			clamped = generated
		}
		if err := nk.AccountUpdateId(
			ctx, userID, "", nil, clamped, "", "", "", "",
		); err != nil {
			logger.Warn(
				"name clamp: account update user=%s err=%v",
				userID, err)
		}
	}
	clampRun := func(
		ctx context.Context,
		logger runtime.Logger,
		nk runtime.NakamaModule,
	) {
		userID, ok := ctx.Value(
			runtime.RUNTIME_CTX_USER_ID).(string)
		if !ok || userID == "" {
			logger.Warn("name clamp: no user id in context")
			return
		}
		clamp(ctx, logger, nk, userID)
	}
	// work runs one provider's after-sign-in behaviors.
	work := func(
		generates, clamps, privacy bool,
	) func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.Session,
	) {
		return func(
			ctx context.Context,
			logger runtime.Logger,
			db *sql.DB,
			nk runtime.NakamaModule,
			out *api.Session,
		) {
			if generates && out.GetCreated() {
				_ = assignGeneratedNameOnCreate(
					ctx, logger, nk, out.GetCreated())
			} else if clamps {
				clampRun(ctx, logger, nk)
			}
			if privacy {
				enforceSignInPrivacy(ctx, logger, db)
			}
		}
	}
	if err := initializer.RegisterAfterAuthenticateGoogle(
		func(
			ctx context.Context,
			logger runtime.Logger,
			db *sql.DB,
			nk runtime.NakamaModule,
			out *api.Session,
			_ *api.AuthenticateGoogleRequest,
		) error {
			work(true, true, true)(ctx, logger, db, nk, out)
			return nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterAfterAuthenticateFacebook(
		func(
			ctx context.Context,
			logger runtime.Logger,
			db *sql.DB,
			nk runtime.NakamaModule,
			out *api.Session,
			_ *api.AuthenticateFacebookRequest,
		) error {
			work(true, true, true)(ctx, logger, db, nk, out)
			return nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterAfterAuthenticateSteam(
		func(
			ctx context.Context,
			logger runtime.Logger,
			db *sql.DB,
			nk runtime.NakamaModule,
			out *api.Session,
			_ *api.AuthenticateSteamRequest,
		) error {
			work(true, true, true)(ctx, logger, db, nk, out)
			return nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterAfterAuthenticateApple(
		func(
			ctx context.Context,
			logger runtime.Logger,
			db *sql.DB,
			nk runtime.NakamaModule,
			out *api.Session,
			_ *api.AuthenticateAppleRequest,
		) error {
			work(true, false, true)(ctx, logger, db, nk, out)
			return nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterAfterAuthenticateDevice(
		func(
			ctx context.Context,
			logger runtime.Logger,
			db *sql.DB,
			nk runtime.NakamaModule,
			out *api.Session,
			_ *api.AuthenticateDeviceRequest,
		) error {
			work(true, false, true)(ctx, logger, db, nk, out)
			return nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterAfterAuthenticateCustom(
		func(
			ctx context.Context,
			logger runtime.Logger,
			db *sql.DB,
			nk runtime.NakamaModule,
			out *api.Session,
			_ *api.AuthenticateCustomRequest,
		) error {
			work(true, false, false)(ctx, logger, db, nk, out)
			return nil
		}); err != nil {
		return err
	}
	return nil
}

// validateDisplayName checks one proposed display name
// in the form it would be stored: trimmed of surrounding
// space, within the bound, with no control or invisible
// characters (a joiner between letters or emoji does
// orthographic work and is not counted invisible), at
// least one letter or digit, and not the reserved name
// in any case.
func validateDisplayName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return runtime.NewError("display name is empty", 3)
	}
	if utf8.RuneCountInString(trimmed) > maxDisplayNameRunes {
		return runtime.NewError("display name is too long", 3)
	}
	if strings.EqualFold(trimmed, reservedDisplayName) {
		return runtime.NewError("display name is reserved", 3)
	}
	hasLetterOrDigit := false
	trimmedRunes := []rune(trimmed)
	for i, r := range trimmedRunes {
		if isKeptJoiner(trimmedRunes, i) {
			continue
		}
		if unicode.IsControl(r) ||
			unicode.Is(unicode.Cf, r) ||
			unicode.Is(unicode.Zl, r) ||
			unicode.Is(unicode.Zp, r) {
			return runtime.NewError(
				"display name has invisible characters", 3)
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			hasLetterOrDigit = true
		}
	}
	if !hasLetterOrDigit {
		return runtime.NewError(
			"display name needs a letter or digit", 3)
	}
	return nil
}

// validateAvatarURL checks one proposed avatar URL. An empty
// value clears the avatar and is always allowed.
func validateAvatarURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > maxAvatarURLBytes {
		return runtime.NewError("avatar URL is too long", 3)
	}
	if strings.Contains(raw, "#") ||
		strings.Contains(raw, "\\") {
		return runtime.NewError("avatar URL is malformed", 3)
	}
	if !strings.HasPrefix(raw, "https://") {
		return runtime.NewError("avatar URL must be https", 3)
	}
	if _, err := url.Parse(raw); err != nil {
		return runtime.NewError("avatar URL is malformed", 3)
	}
	// The host is the text before the first '/'; it may
	// contain only ASCII hostname characters. The check
	// runs before lowercasing, so a non-ASCII character
	// cannot fold into an ASCII one.
	authority := raw[len("https://"):]
	if i := strings.Index(authority, "/"); i >= 0 {
		authority = authority[:i]
	}
	for _, r := range authority {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') &&
			(r < '0' || r > '9') && r != '.' && r != '-' {
			return runtime.NewError(
				"avatar URL is malformed", 3)
		}
	}
	host := strings.ToLower(authority)
	if !avatarURLAllowedHost(host) {
		return runtime.NewError(
			"avatar host is not allowed", 3)
	}
	return nil
}
