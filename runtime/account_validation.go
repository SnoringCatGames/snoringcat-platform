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

// avatarHostSuffixes mirrors the client allowlist (H60): the
// host itself or any subdomain may serve avatars.
var avatarHostSuffixes = []string{
	"googleusercontent.com",
	"fbcdn.net",
	"fbsbx.com",
	"steamstatic.com",
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
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" {
		return runtime.NewError("avatar URL must be https", 3)
	}
	host := strings.ToLower(parsed.Hostname())
	for _, suffix := range avatarHostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return nil
		}
	}
	return runtime.NewError("avatar host is not allowed", 3)
}
