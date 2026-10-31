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

// clampDisplayName returns the name cut to the display
// name bound, or "" when it already fits. Sign-in providers
// import names without the update hook, so a name that
// predates the bound (or arrives over it) is cut to fit
// after authentication instead of failing later updates.
func clampDisplayName(name string) string {
	trimmed := strings.TrimSpace(name)
	runes := []rune(trimmed)
	if len(runes) <= maxDisplayNameRunes {
		return ""
	}
	out := strings.TrimSpace(
		string(runes[:maxDisplayNameRunes]))
	if out == reservedDisplayName {
		out = string([]rune(out)[:maxDisplayNameRunes-1])
	}
	return out
}

// registerAuthNameClamp installs the after-authenticate
// clamp for the providers that import a profile name at
// sign-in (Facebook, Google, Steam).
func registerAuthNameClamp(
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
		clamped := clampDisplayName(name)
		if clamped == "" {
			return
		}
		if err := nk.AccountUpdateId(
			ctx, userID, "", nil, clamped, "", "", "", "",
		); err != nil {
			logger.Warn(
				"name clamp: account update user=%s err=%v",
				userID, err)
		}
	}
	if err := initializer.RegisterAfterAuthenticateFacebook(
		func(
			ctx context.Context,
			logger runtime.Logger,
			_ *sql.DB,
			nk runtime.NakamaModule,
			out *api.Session,
			_ *api.AuthenticateFacebookRequest,
		) error {
			clamp(ctx, logger, nk,
			ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string))
			return nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterAfterAuthenticateGoogle(
		func(
			ctx context.Context,
			logger runtime.Logger,
			_ *sql.DB,
			nk runtime.NakamaModule,
			out *api.Session,
			_ *api.AuthenticateGoogleRequest,
		) error {
			clamp(ctx, logger, nk,
			ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string))
			return nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterAfterAuthenticateSteam(
		func(
			ctx context.Context,
			logger runtime.Logger,
			_ *sql.DB,
			nk runtime.NakamaModule,
			out *api.Session,
			_ *api.AuthenticateSteamRequest,
		) error {
			clamp(ctx, logger, nk,
			ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string))
			return nil
		}); err != nil {
		return err
	}
	return nil
}

// validateDisplayName checks one proposed display name.
func validateDisplayName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return runtime.NewError("display name is empty", 3)
	}
	if utf8.RuneCountInString(trimmed) > maxDisplayNameRunes {
		return runtime.NewError("display name is too long", 3)
	}
	if trimmed == reservedDisplayName {
		return runtime.NewError("display name is reserved", 3)
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return runtime.NewError(
				"display name has control characters", 3)
		}
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
