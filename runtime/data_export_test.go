package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestExportAccountFromCopiesEveryIdentifier(t *testing.T) {
	account := &api.Account{
		User: &api.User{
			Id:                    "user-1",
			Username:              "pPALcZnDQj",
			DisplayName:           "Ada",
			AvatarUrl:             "https://example.invalid/a.png",
			LangTag:               "fr",
			Location:              "Lyon",
			Timezone:              "Europe/Paris",
			Metadata:              `{"theme":"dark"}`,
			GoogleId:              "g-1",
			FacebookId:            "f-1",
			FacebookInstantGameId: "fi-1",
			AppleId:               "a-1",
			SteamId:               "s-1",
			GamecenterId:          "gc-1",
			CreateTime:            &timestamppb.Timestamp{Seconds: 100},
			UpdateTime:            &timestamppb.Timestamp{Seconds: 200},
		},
		Wallet:      `{"coins":3}`,
		Email:       "ada@example.invalid",
		Devices:     []*api.AccountDevice{{Id: "device-1"}, {Id: ""}},
		CustomId:    "custom-1",
		VerifyTime:  &timestamppb.Timestamp{Seconds: 300},
		DisableTime: &timestamppb.Timestamp{Seconds: 400},
	}

	got := exportAccountFrom(account, "ABCD2345")

	want := exportSignInIDs{
		DeviceIDs:           []string{"device-1"},
		CustomID:            "custom-1",
		Google:              "g-1",
		Facebook:            "f-1",
		FacebookInstantGame: "fi-1",
		Apple:               "a-1",
		Steam:               "s-1",
		GameCenter:          "gc-1",
	}
	if !reflect.DeepEqual(got.SignInIDs, want) {
		t.Errorf("sign-in ids = %+v, want %+v", got.SignInIDs, want)
	}
	if got.Email != "ada@example.invalid" {
		t.Errorf("email = %q", got.Email)
	}
	if got.UserID != "user-1" || got.Username != "pPALcZnDQj" ||
		got.DisplayName != "Ada" ||
		got.AvatarURL != "https://example.invalid/a.png" {
		t.Errorf("profile = %+v", got)
	}
	if got.LangTag != "fr" || got.Location != "Lyon" ||
		got.Timezone != "Europe/Paris" {
		t.Errorf("locale fields = %+v", got)
	}
	if got.CreatedAt != 100 || got.UpdatedAt != 200 ||
		got.VerifiedAt != 300 || got.DisabledAt != 400 {
		t.Errorf("timestamps = %+v", got)
	}
	if got.FriendCode != "ABCD2345" {
		t.Errorf("friend code = %q", got.FriendCode)
	}
	assertJSON(t, got.Metadata, `{"theme":"dark"}`)
	assertJSON(t, got.Wallet, `{"coins":3}`)
}

func TestExportAccountFromNilAccountIsEmptyNotNull(t *testing.T) {
	got := exportAccountFrom(nil, "")
	if got.SignInIDs.DeviceIDs == nil {
		t.Fatal("device_ids must encode as [], not null")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"user_id", "email", "sign_in_ids", "friend_code",
		"metadata", "wallet", "created_at",
	} {
		if _, ok := fields[key]; !ok {
			t.Errorf("account block lacks %q: %s", key, encoded)
		}
	}
	// Never-set times are left out rather than reported as 1970.
	for _, key := range []string{"verified_at", "disabled_at"} {
		if _, ok := fields[key]; ok {
			t.Errorf("unset %q should be omitted: %s", key, encoded)
		}
	}
}

func TestExportDeletionFrom(t *testing.T) {
	queue := exportStorageObject{
		Collection: accountDeletionCollection,
		Key:        accountDeletionKey,
		Value: jsonValue(`{"user_id":"u","requested_at":10,` +
			`"scheduled_for":20}`),
	}
	got := exportDeletionFrom([]exportStorageObject{queue})
	want := exportAccountDeletion{
		Pending: true, RequestedAt: 10, ScheduledFor: 20,
	}
	if got != want {
		t.Errorf("pending = %+v, want %+v", got, want)
	}

	// A row queued before the profile copy was dropped still reads.
	legacy := queue
	legacy.Value = jsonValue(`{"user_id":"u","requested_at":10,` +
		`"scheduled_for":20,"original_username":"x",` +
		`"original_display_name":"Ada"}`)
	if got := exportDeletionFrom(
		[]exportStorageObject{legacy}); got != want {
		t.Errorf("legacy row = %+v, want %+v", got, want)
	}

	other := exportStorageObject{Collection: "settings", Key: "global"}
	if got := exportDeletionFrom(
		[]exportStorageObject{other}); got.Pending {
		t.Errorf("no queue row should not be pending: %+v", got)
	}
}

func TestExportIPAddressFrom(t *testing.T) {
	stored := exportStorageObject{
		Collection: clientIPCollection,
		Key:        clientIPKey,
		Value:      jsonValue(`{"ip":"203.0.113.5","ts":1234}`),
	}
	got := exportIPAddressFrom([]exportStorageObject{stored})
	if got == nil || got.IP != "203.0.113.5" || got.RecordedAt != 1234 {
		t.Errorf("stored IP = %+v", got)
	}
	if got := exportIPAddressFrom(nil); got != nil {
		t.Errorf("no record should be nil, got %+v", got)
	}
	empty := stored
	empty.Value = jsonValue(`{"ip":"","ts":1}`)
	if got := exportIPAddressFrom(
		[]exportStorageObject{empty}); got != nil {
		t.Errorf("empty IP should be nil, got %+v", got)
	}
}

func TestExportFriendFromNamesEveryState(t *testing.T) {
	cases := map[int32]string{
		0: "friend",
		1: "invite_sent",
		2: "invite_received",
		3: "blocked",
		9: "unknown",
	}
	for state, name := range cases {
		friend := &api.Friend{
			User: &api.User{
				Id: "f", Username: "fu", DisplayName: "Friend",
			},
			State:      &wrapperspb.Int32Value{Value: state},
			UpdateTime: &timestamppb.Timestamp{Seconds: 5},
		}
		got := exportFriendFrom(friend)
		if got.State != state || got.StateName != name {
			t.Errorf("state %d: got %d %q, want %q",
				state, got.State, got.StateName, name)
		}
		if got.UserID != "f" || got.Username != "fu" ||
			got.DisplayName != "Friend" || got.UpdatedAt != 5 {
			t.Errorf("state %d: fields = %+v", state, got)
		}
	}
}

func TestExportGroupFrom(t *testing.T) {
	party := &api.UserGroupList_UserGroup{
		Group: &api.Group{
			Id:         "g1",
			Name:       partyGroupPrefix + "abc",
			CreatorId:  "me",
			CreateTime: &timestamppb.Timestamp{Seconds: 7},
		},
		State: &wrapperspb.Int32Value{Value: 0},
	}
	got := exportGroupFrom(party, "me")
	if !got.IsParty || !got.IsCreator ||
		got.MembershipStateName != "superadmin" || got.CreatedAt != 7 {
		t.Errorf("own party = %+v", got)
	}

	club := &api.UserGroupList_UserGroup{
		Group: &api.Group{Id: "g2", Name: "club", CreatorId: "other"},
		State: &wrapperspb.Int32Value{Value: 2},
	}
	got = exportGroupFrom(club, "me")
	if got.IsParty || got.IsCreator || got.MembershipStateName != "member" {
		t.Errorf("joined group = %+v", got)
	}
}

func TestChatChannelFrom(t *testing.T) {
	cases := []struct {
		mode                       int32
		subject, descriptor, label string
		wantType, wantTarget       string
	}{
		{streamModeRoom, "", "", "lobby", "room", "lobby"},
		{streamModeGroup, "g1", "", "", "group", "g1"},
		{streamModeDirect, "me", "you", "", "direct", "you"},
		{streamModeDirect, "you", "me", "", "direct", "you"},
		{7, "x", "y", "z", "unknown", ""},
	}
	for _, c := range cases {
		gotType, gotTarget := chatChannelFrom(
			c.mode, c.subject, c.descriptor, c.label, "me")
		if gotType != c.wantType || gotTarget != c.wantTarget {
			t.Errorf("mode %d: got (%q, %q), want (%q, %q)",
				c.mode, gotType, gotTarget, c.wantType, c.wantTarget)
		}
	}
}

func TestChatMessageTypeNames(t *testing.T) {
	if stateName(chatMessageTypeNames, 0) != "chat" ||
		stateName(chatMessageTypeNames, 3) != "group_join" ||
		stateName(chatMessageTypeNames, 5) != "group_leave" ||
		stateName(chatMessageTypeNames, 99) != "unknown" {
		t.Error("chat message type names drifted from Nakama's codes")
	}
}

func TestJSONValue(t *testing.T) {
	assertJSON(t, jsonValue(`{"a":1}`), `{"a":1}`)
	assertJSON(t, jsonValue("  [1,2] "), `[1,2]`)
	assertJSON(t, jsonValue(""), `{}`)
	if got := jsonValue("not json"); got != "not json" {
		t.Errorf("non-JSON text should stay a string, got %#v", got)
	}
}

func TestUnixSeconds(t *testing.T) {
	if got := unixSeconds(time.Time{}); got != 0 {
		t.Errorf("zero time = %d, want 0", got)
	}
	if got := unixSeconds(time.Unix(0, 0)); got != 0 {
		t.Errorf("Nakama's epoch sentinel = %d, want 0", got)
	}
	if got := unixSeconds(time.Unix(1760000000, 0)); got != 1760000000 {
		t.Errorf("real time = %d", got)
	}
}

// TestExportEnvelopeListsEveryCategory locks the top-level contract
// the compliance suite and the privacy policy describe.
func TestExportEnvelopeListsEveryCategory(t *testing.T) {
	encoded, err := json.Marshal(playerDataExport{
		Friends:            []exportFriend{},
		Groups:             []exportGroup{},
		LeaderboardRecords: []exportLeaderboardRecord{},
		StorageObjects:     []exportStorageObject{},
		Notifications:      []exportNotification{},
		ChatMessages:       []exportChatMessage{},
		WalletLedger:       []exportWalletLedgerEntry{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(fields))
	for key := range fields {
		got = append(got, key)
	}
	sort.Strings(got)
	want := []string{
		"account", "account_deletion", "chat_messages", "format_version",
		"friends", "generated_at", "groups", "last_ip_address",
		"leaderboard_records", "notifications", "storage_objects",
		"wallet_ledger",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("export keys = %v, want %v", got, want)
	}
	// No stored IP encodes as null, not as a missing key.
	if string(fields["last_ip_address"]) != "null" {
		t.Errorf("last_ip_address = %s, want null",
			fields["last_ip_address"])
	}
}

// assertJSON checks that value encodes to the same JSON as want.
func assertJSON(t *testing.T, value any, want string) {
	t.Helper()
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %#v: %v", value, err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode want %s: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("JSON = %s, want %s", got, want)
	}
}
