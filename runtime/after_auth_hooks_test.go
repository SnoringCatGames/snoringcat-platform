//lint:file-ignore SA1029 nakama-common uses bare strings as context keys.
package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
)

// SP106: Nakama keeps one after-authenticate function per
// provider, so every hook InitModule registers must be
// registered at most once. recordingInitializer captures
// every registration the InitModule register calls make.
type recordingInitializer struct {
	runtime.Initializer

	counts   map[string]int
	googleFn func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.Session,
		*api.AuthenticateGoogleRequest,
	) error
	steamFn func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.Session,
		*api.AuthenticateSteamRequest,
	) error
	deviceFn func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.Session,
		*api.AuthenticateDeviceRequest,
	) error
	refreshFn func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.Session,
		*api.SessionRefreshRequest,
	) error
}

func (c *recordingInitializer) record(name string) {
	c.counts[name]++
}

func (c *recordingInitializer) RegisterBeforeAuthenticateDevice(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AuthenticateDeviceRequest) (*api.AuthenticateDeviceRequest, error)) error {
	c.record("BeforeAuthenticateDevice")
	return nil
}

func (c *recordingInitializer) RegisterBeforeAuthenticateGoogle(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AuthenticateGoogleRequest) (*api.AuthenticateGoogleRequest, error)) error {
	c.record("BeforeAuthenticateGoogle")
	return nil
}

func (c *recordingInitializer) RegisterBeforeAuthenticateFacebook(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AuthenticateFacebookRequest) (*api.AuthenticateFacebookRequest, error)) error {
	c.record("BeforeAuthenticateFacebook")
	return nil
}

func (c *recordingInitializer) RegisterBeforeAuthenticateApple(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AuthenticateAppleRequest) (*api.AuthenticateAppleRequest, error)) error {
	c.record("BeforeAuthenticateApple")
	return nil
}

func (c *recordingInitializer) RegisterBeforeAuthenticateSteam(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AuthenticateSteamRequest) (*api.AuthenticateSteamRequest, error)) error {
	c.record("BeforeAuthenticateSteam")
	return nil
}

func (c *recordingInitializer) RegisterBeforeAuthenticateEmail(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AuthenticateEmailRequest) (*api.AuthenticateEmailRequest, error)) error {
	c.record("BeforeAuthenticateEmail")
	return nil
}

func (c *recordingInitializer) RegisterBeforeSessionRefresh(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.SessionRefreshRequest) (*api.SessionRefreshRequest, error)) error {
	c.record("BeforeSessionRefresh")
	return nil
}

func (c *recordingInitializer) RegisterBeforeLinkEmail(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AccountEmail) (*api.AccountEmail, error)) error {
	c.record("BeforeLinkEmail")
	return nil
}

func (c *recordingInitializer) RegisterBeforeAddFriends(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AddFriendsRequest) (*api.AddFriendsRequest, error)) error {
	c.record("BeforeAddFriends")
	return nil
}

func (c *recordingInitializer) RegisterBeforeAddGroupUsers(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AddGroupUsersRequest) (*api.AddGroupUsersRequest, error)) error {
	c.record("BeforeAddGroupUsers")
	return nil
}

func (c *recordingInitializer) RegisterBeforeBanGroupUsers(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.BanGroupUsersRequest) (*api.BanGroupUsersRequest, error)) error {
	c.record("BeforeBanGroupUsers")
	return nil
}

func (c *recordingInitializer) RegisterBeforePromoteGroupUsers(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.PromoteGroupUsersRequest) (*api.PromoteGroupUsersRequest, error)) error {
	c.record("BeforePromoteGroupUsers")
	return nil
}

func (c *recordingInitializer) RegisterBeforeDemoteGroupUsers(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.DemoteGroupUsersRequest) (*api.DemoteGroupUsersRequest, error)) error {
	c.record("BeforeDemoteGroupUsers")
	return nil
}

func (c *recordingInitializer) RegisterBeforeUpdateGroup(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.UpdateGroupRequest) (*api.UpdateGroupRequest, error)) error {
	c.record("BeforeUpdateGroup")
	return nil
}

func (c *recordingInitializer) RegisterBeforeDeleteGroup(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.DeleteGroupRequest) (*api.DeleteGroupRequest, error)) error {
	c.record("BeforeDeleteGroup")
	return nil
}

func (c *recordingInitializer) RegisterBeforeJoinGroup(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.JoinGroupRequest) (*api.JoinGroupRequest, error)) error {
	c.record("BeforeJoinGroup")
	return nil
}

func (c *recordingInitializer) RegisterBeforeCreateGroup(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.CreateGroupRequest) (*api.CreateGroupRequest, error)) error {
	c.record("BeforeCreateGroup")
	return nil
}

func (c *recordingInitializer) RegisterBeforeKickGroupUsers(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.KickGroupUsersRequest) (*api.KickGroupUsersRequest, error)) error {
	c.record("BeforeKickGroupUsers")
	return nil
}

func (c *recordingInitializer) RegisterBeforeUpdateAccount(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.UpdateAccountRequest) (*api.UpdateAccountRequest, error)) error {
	c.record("BeforeUpdateAccount")
	return nil
}

func (c *recordingInitializer) RegisterBeforeWriteStorageObjects(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.WriteStorageObjectsRequest) (*api.WriteStorageObjectsRequest, error)) error {
	c.record("BeforeWriteStorageObjects")
	return nil
}

func (c *recordingInitializer) RegisterBeforeRt(id string, fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *rtapi.Envelope) (*rtapi.Envelope, error)) error {
	c.record("BeforeRt")
	return nil
}

func (c *recordingInitializer) RegisterAfterAuthenticateGoogle(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.Session, *api.AuthenticateGoogleRequest) error) error {
	c.record("AfterAuthenticateGoogle")
	c.googleFn = fn
	return nil
}

func (c *recordingInitializer) RegisterAfterAuthenticateFacebook(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.Session, *api.AuthenticateFacebookRequest) error) error {
	c.record("AfterAuthenticateFacebook")
	return nil
}

func (c *recordingInitializer) RegisterAfterAuthenticateSteam(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.Session, *api.AuthenticateSteamRequest) error) error {
	c.record("AfterAuthenticateSteam")
	c.steamFn = fn
	return nil
}

func (c *recordingInitializer) RegisterAfterAuthenticateApple(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.Session, *api.AuthenticateAppleRequest) error) error {
	c.record("AfterAuthenticateApple")
	return nil
}

func (c *recordingInitializer) RegisterAfterAuthenticateDevice(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.Session, *api.AuthenticateDeviceRequest) error) error {
	c.record("AfterAuthenticateDevice")
	c.deviceFn = fn
	return nil
}

func (c *recordingInitializer) RegisterAfterAuthenticateCustom(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.Session, *api.AuthenticateCustomRequest) error) error {
	c.record("AfterAuthenticateCustom")
	return nil
}

func (c *recordingInitializer) RegisterAfterAddGroupUsers(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AddGroupUsersRequest) error) error {
	c.record("AfterAddGroupUsers")
	return nil
}

func (c *recordingInitializer) RegisterAfterJoinGroup(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.JoinGroupRequest) error) error {
	c.record("AfterJoinGroup")
	return nil
}

func (c *recordingInitializer) RegisterAfterLeaveGroup(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.LeaveGroupRequest) error) error {
	c.record("AfterLeaveGroup")
	return nil
}

func (c *recordingInitializer) RegisterAfterKickGroupUsers(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.KickGroupUsersRequest) error) error {
	c.record("AfterKickGroupUsers")
	return nil
}

func (c *recordingInitializer) RegisterAfterLinkGoogle(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AccountGoogle) error) error {
	c.record("AfterLinkGoogle")
	return nil
}

func (c *recordingInitializer) RegisterAfterLinkFacebook(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.LinkFacebookRequest) error) error {
	c.record("AfterLinkFacebook")
	return nil
}

func (c *recordingInitializer) RegisterAfterLinkApple(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.AccountApple) error) error {
	c.record("AfterLinkApple")
	return nil
}

func (c *recordingInitializer) RegisterAfterSessionRefresh(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule, *api.Session, *api.SessionRefreshRequest) error) error {
	c.record("AfterSessionRefresh")
	c.refreshFn = fn
	return nil
}

func (c *recordingInitializer) RegisterAfterDeleteAccount(fn func(context.Context, runtime.Logger, *sql.DB, runtime.NakamaModule) error) error {
	c.record("AfterDeleteAccount")
	return nil
}

// registerAllHooks runs every hook registration InitModule
// makes, in InitModule's order, through one initializer.
func registerAllHooks(t *testing.T, c *recordingInitializer) {
	t.Helper()
	games := newTestGames(t, nil)
	steps := []func() error{
		func() error { return registerAuthHooks(c, games) },
		func() error { return registerAfterAuthenticateHooks(c) },
		func() error { return registerPartyKickHook(c) },
		func() error { return registerPartyGroupGuards(c) },
		func() error { return registerStorageAllowlistHook(c, games) },
		func() error { return registerAccountValidationHook(c) },
		func() error {
			return registerPartyChatHook(c, newFriendsLimiter())
		},
		func() error { return registerPartyGroupHooks(c) },
		func() error { return registerPrivacyHooks(c) },
		func() error {
			return registerFriendsLimitHook(c, newFriendsLimiter())
		},
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("register step %d: err = %v", i, err)
		}
	}
}

func TestNoHookRegisteredTwice(t *testing.T) {
	initializer := &recordingInitializer{
		counts: map[string]int{},
	}
	registerAllHooks(t, initializer)
	for name, count := range initializer.counts {
		if count != 1 {
			t.Errorf("hook %s registered %d times, want 1",
				name, count)
		}
	}
	for _, provider := range []string{
		"Google", "Facebook", "Steam", "Apple", "Device", "Custom",
	} {
		if got := initializer.counts["AfterAuthenticate"+provider]; got != 1 {
			t.Errorf("AfterAuthenticate%s count = %d, want 1",
				provider, got)
		}
	}
}

// fakeAfterAuthNk serves one stored display name and
// records every name written back.
type fakeAfterAuthNk struct {
	runtime.NakamaModule

	displayName string
	updated     []string
}

func (f *fakeAfterAuthNk) AccountGetId(
	_ context.Context, _ string,
) (*api.Account, error) {
	return &api.Account{
		User: &api.User{DisplayName: f.displayName},
	}, nil
}

func (f *fakeAfterAuthNk) AccountUpdateId(
	_ context.Context, _, _ string,
	_ map[string]interface{},
	displayName, _, _, _, _ string,
) error {
	f.updated = append(f.updated, displayName)
	return nil
}

func afterAuthCtx() context.Context {
	return context.WithValue(
		context.Background(),
		runtime.RUNTIME_CTX_USER_ID, "user-1")
}

func TestCreatedGoogleAccountGetsGeneratedName(t *testing.T) {
	initializer := &recordingInitializer{
		counts: map[string]int{},
	}
	if err := registerAfterAuthenticateHooks(initializer); err != nil {
		t.Fatalf("register: err = %v", err)
	}
	nk := &fakeAfterAuthNk{displayName: "Provider Person"}
	err := initializer.googleFn(
		afterAuthCtx(), testLogger{}, nil, nk,
		&api.Session{Created: true},
		&api.AuthenticateGoogleRequest{})
	if err != nil {
		t.Fatalf("hook: err = %v", err)
	}
	if len(nk.updated) != 1 {
		t.Fatalf("updates = %v, want one generated name", nk.updated)
	}
	got := nk.updated[0]
	if got == "Provider Person" || got == "" {
		t.Errorf("stored name = %q, want a generated name", got)
	}
	if err := validateDisplayName(got); err != nil {
		t.Errorf("generated name %q fails validation: %v", got, err)
	}
	i := strings.LastIndex(got, "_")
	if i < 0 || len(got)-i-1 != 4 {
		t.Errorf("generated name = %q, want the _NNNN shape", got)
	}
}

func TestCreatedSteamAccountGetsGeneratedName(t *testing.T) {
	initializer := &recordingInitializer{
		counts: map[string]int{},
	}
	if err := registerAfterAuthenticateHooks(initializer); err != nil {
		t.Fatalf("register: err = %v", err)
	}
	if initializer.steamFn == nil {
		t.Fatal("no Steam after-auth hook was registered")
	}
	nk := &fakeAfterAuthNk{displayName: "Provider Person"}
	err := initializer.steamFn(
		afterAuthCtx(), testLogger{}, nil, nk,
		&api.Session{Created: true},
		&api.AuthenticateSteamRequest{})
	if err != nil {
		t.Fatalf("hook: err = %v", err)
	}
	if len(nk.updated) != 1 {
		t.Fatalf("updates = %v, want one generated name", nk.updated)
	}
	got := nk.updated[0]
	if got == "Provider Person" || got == "" {
		t.Errorf("stored name = %q, want a generated name", got)
	}
	if err := validateDisplayName(got); err != nil {
		t.Errorf("generated name %q fails validation: %v", got, err)
	}
}

func TestExistingNamelessDeviceAccountGetsGeneratedName(t *testing.T) {
	initializer := &recordingInitializer{
		counts: map[string]int{},
	}
	if err := registerAfterAuthenticateHooks(initializer); err != nil {
		t.Fatalf("register: err = %v", err)
	}
	if initializer.deviceFn == nil {
		t.Fatal("no Device after-auth hook was registered")
	}
	nk := &fakeAfterAuthNk{displayName: ""}
	err := initializer.deviceFn(
		afterAuthCtx(), testLogger{}, nil, nk,
		&api.Session{Created: false},
		&api.AuthenticateDeviceRequest{})
	if err != nil {
		t.Fatalf("hook: err = %v", err)
	}
	if len(nk.updated) != 1 {
		t.Fatalf("updates = %v, want one generated name", nk.updated)
	}
	if err := validateDisplayName(nk.updated[0]); err != nil {
		t.Errorf("generated name %q fails validation: %v",
			nk.updated[0], err)
	}
}

func TestExistingNamedDeviceAccountKeepsItsName(t *testing.T) {
	initializer := &recordingInitializer{
		counts: map[string]int{},
	}
	if err := registerAfterAuthenticateHooks(initializer); err != nil {
		t.Fatalf("register: err = %v", err)
	}
	nk := &fakeAfterAuthNk{displayName: "BraveBunny42"}
	err := initializer.deviceFn(
		afterAuthCtx(), testLogger{}, nil, nk,
		&api.Session{Created: false},
		&api.AuthenticateDeviceRequest{})
	if err != nil {
		t.Fatalf("hook: err = %v", err)
	}
	if len(nk.updated) != 0 {
		t.Errorf("updates = %v, want the stored name kept", nk.updated)
	}
}

func TestSessionRefreshNamesNamelessAccount(t *testing.T) {
	initializer := &recordingInitializer{
		counts: map[string]int{},
	}
	if err := registerPrivacyHooks(initializer); err != nil {
		t.Fatalf("register: err = %v", err)
	}
	if initializer.refreshFn == nil {
		t.Fatal("no after-session-refresh hook was registered")
	}
	nk := &fakeAfterAuthNk{displayName: ""}
	err := initializer.refreshFn(
		afterAuthCtx(), testLogger{}, nil, nk,
		&api.Session{}, &api.SessionRefreshRequest{})
	if err != nil {
		t.Fatalf("hook: err = %v", err)
	}
	if len(nk.updated) != 1 {
		t.Fatalf("updates = %v, want one generated name", nk.updated)
	}
	if err := validateDisplayName(nk.updated[0]); err != nil {
		t.Errorf("generated name %q fails validation: %v",
			nk.updated[0], err)
	}
}

func TestSessionRefreshKeepsStoredName(t *testing.T) {
	initializer := &recordingInitializer{
		counts: map[string]int{},
	}
	if err := registerPrivacyHooks(initializer); err != nil {
		t.Fatalf("register: err = %v", err)
	}
	nk := &fakeAfterAuthNk{displayName: "BraveBunny42"}
	err := initializer.refreshFn(
		afterAuthCtx(), testLogger{}, nil, nk,
		&api.Session{}, &api.SessionRefreshRequest{})
	if err != nil {
		t.Fatalf("hook: err = %v", err)
	}
	if len(nk.updated) != 0 {
		t.Errorf("updates = %v, want the stored name kept", nk.updated)
	}
}

func TestExistingGoogleAccountGetsClampedName(t *testing.T) {
	initializer := &recordingInitializer{
		counts: map[string]int{},
	}
	if err := registerAfterAuthenticateHooks(initializer); err != nil {
		t.Fatalf("register: err = %v", err)
	}
	nk := &fakeAfterAuthNk{
		displayName: strings.Repeat("é", maxDisplayNameRunes+8),
	}
	err := initializer.googleFn(
		afterAuthCtx(), testLogger{}, nil, nk,
		&api.Session{Created: false},
		&api.AuthenticateGoogleRequest{})
	if err != nil {
		t.Fatalf("hook: err = %v", err)
	}
	if len(nk.updated) != 1 {
		t.Fatalf("updates = %v, want one clamped name", nk.updated)
	}
	got := nk.updated[0]
	if n := utf8.RuneCountInString(got); n != maxDisplayNameRunes {
		t.Errorf("clamped name = %d runes, want %d",
			n, maxDisplayNameRunes)
	}
	if err := validateDisplayName(got); err != nil {
		t.Errorf("clamped name %q fails validation: %v", got, err)
	}
}
