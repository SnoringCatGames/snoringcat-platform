package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestValidateDisplayName(t *testing.T) {
	good := []string{
		"BraveBunny42",
		"Ada Lovelace",
		"x",
		strings.Repeat("n", maxDisplayNameRunes),
	}
	for _, name := range good {
		if err := validateDisplayName(name); err != nil {
			t.Errorf("name %q: err = %v, want nil", name, err)
		}
	}
	bad := []string{
		"",
		"   ",
		"[deleted]",
		"[Deleted]",
		"[DELETED]",
		"[deleted]\u200b",
		"\u200b\u200b",
		"\u202eevil",
		"!!!",
		"line\nbreak",
		strings.Repeat("n", maxDisplayNameRunes+1),
		strings.Repeat(" ", 40) +
			strings.Repeat("n", maxDisplayNameRunes+1),
	}
	for _, name := range bad {
		if err := validateDisplayName(name); errorCode(err) != 3 {
			t.Errorf("name %q: err = %v, want code 3", name, err)
		}
	}
}

type capturingUpdateAccountInitializer struct {
	runtime.Initializer

	fn func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.UpdateAccountRequest,
	) (*api.UpdateAccountRequest, error)
}

func (c *capturingUpdateAccountInitializer) RegisterBeforeUpdateAccount(
	fn func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.UpdateAccountRequest,
	) (*api.UpdateAccountRequest, error),
) error {
	c.fn = fn
	return nil
}

func TestAccountValidationHookStoresCheckedName(t *testing.T) {
	initializer := &capturingUpdateAccountInitializer{}
	if err := registerAccountValidationHook(initializer); err != nil {
		t.Fatalf("register: err = %v", err)
	}
	if initializer.fn == nil {
		t.Fatal("no BeforeUpdateAccount hook was registered")
	}
	// An accepted name is stored trimmed, padding included
	// in what the client sent but not in what is kept.
	in := &api.UpdateAccountRequest{
		DisplayName: &wrapperspb.StringValue{
			Value: "  Ada Lovelace  ",
		},
	}
	out, err := initializer.fn(
		context.Background(), testLogger{}, nil, nil, in)
	if err != nil {
		t.Fatalf("hook: err = %v, want nil", err)
	}
	if got := out.DisplayName.Value; got != "Ada Lovelace" {
		t.Errorf("stored name = %q, want the trimmed name", got)
	}
	// Padding does not count toward the length bound.
	padded := &api.UpdateAccountRequest{
		DisplayName: &wrapperspb.StringValue{
			Value: strings.Repeat(" ", 40) +
				strings.Repeat("n", maxDisplayNameRunes),
		},
	}
	out, err = initializer.fn(
		context.Background(), testLogger{}, nil, nil, padded)
	if err != nil {
		t.Fatalf("padded hook: err = %v, want nil", err)
	}
	if n := utf8.RuneCountInString(out.DisplayName.Value); n != maxDisplayNameRunes {
		t.Errorf("stored padded = %d runes, want %d",
			n, maxDisplayNameRunes)
	}
}

func TestValidateAvatarURL(t *testing.T) {
	good := []string{
		"",
		"https://lh3.googleusercontent.com/a/avatar.png",
		"https://scontent.fbcdn.net/a.png",
		"https://scontent-lax3-1.xx.fbcdn.net/a.png",
		"https://platform-lookaside.fbsbx.com/a.png",
		"https://avatars.steamstatic.com/a.jpg",
		"https://avatars.akamai.steamstatic.com/a.jpg",
		"https://avatars.fastly.steamstatic.com/a.jpg",
		"https://avatars.cloudflare.steamstatic.com/a.jpg",
	}
	for _, raw := range good {
		if err := validateAvatarURL(raw); err != nil {
			t.Errorf("url %q: err = %v, want nil", raw, err)
		}
	}
	bad := []string{
		"http://lh3.googleusercontent.com/a.png",
		"https://tracker.invalid/pixel.png",
		"https://googleusercontent.com.evil.invalid/a.png",
		"https://cdn.googleusercontent.com/a.png",
		"https://lhx.googleusercontent.com/a.png",
		"https://www.facebook.com/a.png",
		"https://steamstatic.com/a.jpg",
		"https://user@lh3.googleusercontent.com/a.png",
		"https://lh3.googleusercontent.com:8443/a.png",
		"https://lh3.googleusercontent.com/a.png#x",
		"https://lh3.googleusercontent.com?x=1",
		"https://lh3.googleusercontent.com\\a.png",
		"https://lh3.googleusercontent.com/" +
			strings.Repeat("a", maxAvatarURLBytes),
	}
	for _, raw := range bad {
		if err := validateAvatarURL(raw); errorCode(err) != 3 {
			t.Errorf("url: err = %v, want code 3", err)
		}
	}
}

func TestValidateAvatarURLRefusesNonASCIIHost(t *testing.T) {
	// U+212A (Kelvin sign) folds to 'k' when lowercased;
	// non-ASCII host characters are refused before that.
	raw := "https://avatars.a\u212aamai.steamstatic.com/a.jpg"
	if err := validateAvatarURL(raw); errorCode(err) != 3 {
		t.Errorf("url: err = %v, want code 3", err)
	}
}

func TestClampDisplayName(t *testing.T) {
	if got := clampDisplayName("Short Name"); got != "" {
		t.Errorf("clamp short = %q, want no change", got)
	}
	long := strings.Repeat("é", maxDisplayNameRunes+8)
	got := clampDisplayName(long)
	if n := utf8.RuneCountInString(got); n != maxDisplayNameRunes {
		t.Errorf("clamp long = %d runes, want %d",
			n, maxDisplayNameRunes)
	}
}

func TestClampDisplayNameReservedLookAlike(t *testing.T) {
	// An imported name that matches the reserved name in
	// any case is shortened so it no longer matches.
	got := clampDisplayName("[Deleted]")
	if got == "" {
		t.Fatal("clamp = no change, want a shortened name")
	}
	if strings.EqualFold(got, reservedDisplayName) {
		t.Errorf("clamp = %q, want a non-reserved name", got)
	}
}

func TestClampDisplayNameReservedAfterTrim(t *testing.T) {
	// Over the bound, with the first bound's worth of
	// runes trimming back to the reserved name.
	name := reservedDisplayName +
		strings.Repeat(" ", maxDisplayNameRunes) + "x"
	got := clampDisplayName(name)
	if n := utf8.RuneCountInString(got); n > maxDisplayNameRunes {
		t.Errorf("clamp = %d runes, want at most %d",
			n, maxDisplayNameRunes)
	}
	if strings.ContainsRune(got, 0) {
		t.Errorf("clamp = %q, want no NUL runes", got)
	}
	if got == reservedDisplayName {
		t.Errorf("clamp = %q, want a non-reserved name", got)
	}
}

type capturingAuthClampInitializer struct {
	runtime.Initializer

	googleFn func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.Session,
		*api.AuthenticateGoogleRequest,
	) error
}

func (c *capturingAuthClampInitializer) RegisterAfterAuthenticateFacebook(
	func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.Session,
		*api.AuthenticateFacebookRequest,
	) error,
) error {
	return nil
}

func (c *capturingAuthClampInitializer) RegisterAfterAuthenticateSteam(
	func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.Session,
		*api.AuthenticateSteamRequest,
	) error,
) error {
	return nil
}
func (c *capturingAuthClampInitializer) RegisterAfterAuthenticateGoogle(
	fn func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.Session,
		*api.AuthenticateGoogleRequest,
	) error,
) error {
	c.googleFn = fn
	return nil
}

func TestAuthNameClampWithoutUserIDIsANoOp(t *testing.T) {
	initializer := &capturingAuthClampInitializer{}
	if err := registerAuthNameClamp(initializer); err != nil {
		t.Fatalf("register: err = %v", err)
	}
	if initializer.googleFn == nil {
		t.Fatal("no Google after-auth hook was registered")
	}
	// No user id in context: the hook logs and returns
	// without touching the module.
	err := initializer.googleFn(
		context.Background(), testLogger{}, nil, nil,
		&api.Session{}, &api.AuthenticateGoogleRequest{})
	if err != nil {
		t.Fatalf("hook without user id: err = %v", err)
	}
}
