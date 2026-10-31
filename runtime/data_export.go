package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Player data export (GDPR access and portability, CCPA right to
// know): the export_player_data RPC returns everything the platform
// stores about the calling player, read from every place it is
// stored:
//
//   - account: the Nakama account row, including any email, every
//     sign-in id (provider ids, device ids, custom id), profile
//     fields, timestamps, and the friend code (a system-owned row
//     keyed by the player, so not in storage_objects).
//   - account_deletion: the pending deletion request, if any.
//   - last_ip_address: the IP that record_client_ip stored, if one
//     is still held.
//   - friends: friends, sent and received requests, and blocked
//     players.
//   - groups: party memberships, with each party's leader, game
//     mode, and level and cheat preferences.
//   - leaderboard_records: every leaderboard record the player owns,
//     on every board, including boards from other games and legacy
//     boards.
//   - storage_objects: every storage object the player owns (match
//     history, recent players, settings, presence, party ready
//     flags, the stored IP, the deletion request, ...).
//   - notifications: notifications stored for the player.
//   - chat_messages: chat messages the player sent, including the
//     group join and leave events Nakama records under their id.
//   - wallet_ledger: wallet changes. Nothing writes the wallet
//     today; listed so the export stays complete if a game starts.
//
// A section that can't be read fails the whole export with a retry-
// able error. A partial export would tell the player they had seen
// everything when they hadn't.
//
// Crash reports are not included: they carry no account id
// (crash_report.go), so nothing links them to a player.

const (
	// exportFormatVersion versions the JSON shape below. Bump it when
	// a field is renamed or removed; adding a field doesn't need it.
	exportFormatVersion = 2

	// exportFriendsPageSize and exportGroupsPageSize are Nakama's
	// limits for FriendsList (1000) and UserGroupsList (100).
	exportFriendsPageSize = 1000
	exportGroupsPageSize  = 100

	// exportMaxPages bounds the friend and group pagination. No real
	// account comes close (100k friends); hitting it fails the export
	// rather than truncating it.
	exportMaxPages = 100

	// exportIndent pretty-prints the export. Players open the file
	// themselves, and the game saves the server's text verbatim.
	exportIndent = "  "
)

// Nakama's stream modes for chat channels (server/tracker.go).
const (
	streamModeNotifications = 0
	streamModeRoom          = 2
	streamModeGroup         = 3
	streamModeDirect        = 4
)

// Nakama's user_edge states (friend state).
var friendStateNames = map[int32]string{
	0: "friend",
	1: "invite_sent",
	2: "invite_received",
	3: "blocked",
}

// Nakama's group_edge states (membership state).
var groupStateNames = map[int32]string{
	0: "superadmin",
	1: "admin",
	2: "member",
	3: "join_request",
	4: "banned",
}

// Nakama's channel message codes (server/pipeline_channel.go).
var chatMessageTypeNames = map[int32]string{
	0: "chat",
	1: "chat_update",
	2: "chat_remove",
	3: "group_join",
	4: "group_add",
	5: "group_leave",
	6: "group_kick",
	7: "group_promote",
	8: "group_ban",
	9: "group_demote",
}

type playerDataExport struct {
	FormatVersion      int                       `json:"format_version"`
	GeneratedAt        string                    `json:"generated_at"`
	Account            exportAccount             `json:"account"`
	AccountDeletion    exportAccountDeletion     `json:"account_deletion"`
	LastIPAddress      *exportIPAddress          `json:"last_ip_address"`
	Friends            []exportFriend            `json:"friends"`
	Groups             []exportGroup             `json:"groups"`
	LeaderboardRecords []exportLeaderboardRecord `json:"leaderboard_records"`
	StorageObjects     []exportStorageObject     `json:"storage_objects"`
	Notifications      []exportNotification      `json:"notifications"`
	ChatMessages       []exportChatMessage       `json:"chat_messages"`
	WalletLedger       []exportWalletLedgerEntry `json:"wallet_ledger"`
}

type exportAccount struct {
	UserID      string          `json:"user_id"`
	Username    string          `json:"username"`
	DisplayName string          `json:"display_name"`
	AvatarURL   string          `json:"avatar_url"`
	Email       string          `json:"email"`
	LangTag     string          `json:"lang_tag"`
	Location    string          `json:"location"`
	Timezone    string          `json:"timezone"`
	Metadata    any             `json:"metadata"`
	Wallet      any             `json:"wallet"`
	CreatedAt   int64           `json:"created_at"`
	UpdatedAt   int64           `json:"updated_at"`
	VerifiedAt  int64           `json:"verified_at,omitempty"`
	DisabledAt  int64           `json:"disabled_at,omitempty"`
	SignInIDs   exportSignInIDs `json:"sign_in_ids"`
	FriendCode  string          `json:"friend_code"`
}

// exportSignInIDs lists every identifier the account can sign in
// with. Empty strings are identities that aren't linked.
type exportSignInIDs struct {
	DeviceIDs           []string `json:"device_ids"`
	CustomID            string   `json:"custom_id"`
	Google              string   `json:"google"`
	Facebook            string   `json:"facebook"`
	FacebookInstantGame string   `json:"facebook_instant_game"`
	Apple               string   `json:"apple"`
	Steam               string   `json:"steam"`
	GameCenter          string   `json:"game_center"`
}

type exportAccountDeletion struct {
	Pending      bool  `json:"pending"`
	RequestedAt  int64 `json:"requested_at,omitempty"`
	ScheduledFor int64 `json:"scheduled_for,omitempty"`
}

type exportIPAddress struct {
	IP         string `json:"ip"`
	RecordedAt int64  `json:"recorded_at"`
}

type exportFriend struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	State       int32  `json:"state"`
	StateName   string `json:"state_name"`
	UpdatedAt   int64  `json:"updated_at"`
}

type exportGroup struct {
	GroupID             string `json:"group_id"`
	Name                string `json:"name"`
	IsParty             bool   `json:"is_party"`
	MembershipState     int32  `json:"membership_state"`
	MembershipStateName string `json:"membership_state_name"`
	IsCreator           bool   `json:"is_creator"`
	CreatedAt           int64  `json:"created_at"`
	PartyLeaderID       string `json:"party_leader_id,omitempty"`
	PartyGameMode       string `json:"party_game_mode,omitempty"`
	PartyLevelPrefs     any    `json:"party_level_prefs,omitempty"`
	PartyCheatPrefs     any    `json:"party_cheat_prefs,omitempty"`
}

type exportLeaderboardRecord struct {
	LeaderboardID string `json:"leaderboard_id"`
	Score         int64  `json:"score"`
	Subscore      int64  `json:"subscore"`
	NumScore      int64  `json:"num_score"`
	Metadata      any    `json:"metadata"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
	ExpiresAt     int64  `json:"expires_at,omitempty"`
}

type exportStorageObject struct {
	Collection string `json:"collection"`
	Key        string `json:"key"`
	Value      any    `json:"value"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

type exportNotification struct {
	ID        string `json:"id"`
	Subject   string `json:"subject"`
	Content   any    `json:"content"`
	Code      int32  `json:"code"`
	SenderID  string `json:"sender_id"`
	CreatedAt int64  `json:"created_at"`
}

type exportChatMessage struct {
	MessageID     string `json:"message_id"`
	Type          string `json:"type"`
	Code          int32  `json:"code"`
	ChannelType   string `json:"channel_type"`
	ChannelTarget string `json:"channel_target"`
	Content       any    `json:"content"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

type exportWalletLedgerEntry struct {
	ID        string `json:"id"`
	Changeset any    `json:"changeset"`
	Metadata  any    `json:"metadata"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// exportPlayerDataRpcFactory threads the per-game config store
// through so the handler can enforce game_id on the session. The
// export is cross-game: it covers the whole identity, and the
// game_id check only asserts the session's identity.
func exportPlayerDataRpcFactory(
	games *perGameConfig,
) func(
	context.Context, runtime.Logger, *sql.DB,
	runtime.NakamaModule, string,
) (string, error) {
	return func(
		ctx context.Context,
		logger runtime.Logger,
		db *sql.DB,
		nk runtime.NakamaModule,
		_ string,
	) (string, error) {
		userID, err := requireClientSession(ctx)
		if err != nil {
			return "", err
		}
		if _, err := requireGameID(ctx, games); err != nil {
			return "", err
		}
		export, err := collectPlayerExport(ctx, db, nk, userID)
		if err != nil {
			logger.Error("export_player_data user=%s: %v", userID, err)
			return "", runtime.NewError(
				"could not export your data; please try again", 13)
		}
		out, err := json.MarshalIndent(export, "", exportIndent)
		if err != nil {
			logger.Error(
				"export_player_data encode user=%s: %v", userID, err)
			return "", runtime.NewError(
				"could not export your data; please try again", 13)
		}
		return string(out), nil
	}
}

// collectPlayerExport reads every section of the export. Any read
// error aborts it; see the file comment.
func collectPlayerExport(
	ctx context.Context,
	db *sql.DB,
	nk runtime.NakamaModule,
	userID string,
) (*playerDataExport, error) {
	if db == nil {
		return nil, fmt.Errorf("no database handle")
	}
	account, err := nk.AccountGetId(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("account: %w", err)
	}
	friendCode, err := readFriendCode(ctx, nk, userID)
	if err != nil {
		return nil, fmt.Errorf("friend code: %w", err)
	}
	storage, err := queryExportStorage(ctx, db, userID)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	friends, err := listExportFriends(ctx, nk, userID)
	if err != nil {
		return nil, fmt.Errorf("friends: %w", err)
	}
	groups, err := listExportGroups(ctx, nk, userID)
	if err != nil {
		return nil, fmt.Errorf("groups: %w", err)
	}
	records, err := queryExportLeaderboardRecords(ctx, db, userID)
	if err != nil {
		return nil, fmt.Errorf("leaderboard records: %w", err)
	}
	notifications, err := queryExportNotifications(ctx, db, userID)
	if err != nil {
		return nil, fmt.Errorf("notifications: %w", err)
	}
	messages, err := queryExportChatMessages(ctx, db, userID)
	if err != nil {
		return nil, fmt.Errorf("chat messages: %w", err)
	}
	ledger, err := queryExportWalletLedger(ctx, db, userID)
	if err != nil {
		return nil, fmt.Errorf("wallet ledger: %w", err)
	}
	return &playerDataExport{
		FormatVersion:      exportFormatVersion,
		GeneratedAt:        time.Now().UTC().Format(time.RFC3339),
		Account:            exportAccountFrom(account, friendCode),
		AccountDeletion:    exportDeletionFrom(storage),
		LastIPAddress:      exportIPAddressFrom(storage),
		Friends:            friends,
		Groups:             groups,
		LeaderboardRecords: records,
		StorageObjects:     storage,
		Notifications:      notifications,
		ChatMessages:       messages,
		WalletLedger:       ledger,
	}, nil
}

// exportAccountFrom maps a Nakama account onto the export's account
// block. Proto getters are nil-safe, so a nil account yields an
// empty block rather than a panic.
func exportAccountFrom(
	account *api.Account,
	friendCode string,
) exportAccount {
	user := account.GetUser()
	out := exportAccount{
		UserID:      user.GetId(),
		Username:    user.GetUsername(),
		DisplayName: user.GetDisplayName(),
		AvatarURL:   user.GetAvatarUrl(),
		Email:       account.GetEmail(),
		LangTag:     user.GetLangTag(),
		Location:    user.GetLocation(),
		Timezone:    user.GetTimezone(),
		Metadata:    jsonValue(user.GetMetadata()),
		Wallet:      jsonValue(account.GetWallet()),
		CreatedAt:   user.GetCreateTime().GetSeconds(),
		UpdatedAt:   user.GetUpdateTime().GetSeconds(),
		VerifiedAt:  account.GetVerifyTime().GetSeconds(),
		DisabledAt:  account.GetDisableTime().GetSeconds(),
		SignInIDs: exportSignInIDs{
			DeviceIDs:           []string{},
			CustomID:            account.GetCustomId(),
			Google:              user.GetGoogleId(),
			Facebook:            user.GetFacebookId(),
			FacebookInstantGame: user.GetFacebookInstantGameId(),
			Apple:               user.GetAppleId(),
			Steam:               user.GetSteamId(),
			GameCenter:          user.GetGamecenterId(),
		},
		FriendCode: friendCode,
	}
	for _, device := range account.GetDevices() {
		if id := device.GetId(); id != "" {
			out.SignInIDs.DeviceIDs = append(out.SignInIDs.DeviceIDs, id)
		}
	}
	return out
}

// exportDeletionFrom reports the pending deletion request found among
// the player's storage objects.
func exportDeletionFrom(
	objects []exportStorageObject,
) exportAccountDeletion {
	for _, obj := range objects {
		if obj.Collection != accountDeletionCollection ||
			obj.Key != accountDeletionKey {
			continue
		}
		raw, _ := json.Marshal(obj.Value)
		var record deletionQueueRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return exportAccountDeletion{Pending: true}
		}
		return exportAccountDeletion{
			Pending:      true,
			RequestedAt:  record.RequestedAt,
			ScheduledFor: record.ScheduledFor,
		}
	}
	return exportAccountDeletion{}
}

// exportIPAddressFrom returns the stored client IP found among the
// player's storage objects, or nil when none is held.
func exportIPAddressFrom(objects []exportStorageObject) *exportIPAddress {
	for _, obj := range objects {
		if obj.Collection != clientIPCollection || obj.Key != clientIPKey {
			continue
		}
		raw, _ := json.Marshal(obj.Value)
		var record clientIPRecord
		if err := json.Unmarshal(raw, &record); err != nil ||
			record.IP == "" {
			return nil
		}
		return &exportIPAddress{IP: record.IP, RecordedAt: record.Timestamp}
	}
	return nil
}

func exportFriendFrom(friend *api.Friend) exportFriend {
	state := friend.GetState().GetValue()
	user := friend.GetUser()
	return exportFriend{
		UserID:      user.GetId(),
		Username:    user.GetUsername(),
		DisplayName: user.GetDisplayName(),
		State:       state,
		StateName:   stateName(friendStateNames, state),
		UpdatedAt:   friend.GetUpdateTime().GetSeconds(),
	}
}

// exportGroupFrom maps one of the player's group memberships. Party
// settings are filled in by the caller, which has to read storage.
func exportGroupFrom(
	membership *api.UserGroupList_UserGroup,
	userID string,
) exportGroup {
	group := membership.GetGroup()
	state := membership.GetState().GetValue()
	return exportGroup{
		GroupID:             group.GetId(),
		Name:                group.GetName(),
		IsParty:             strings.HasPrefix(group.GetName(), partyGroupPrefix),
		MembershipState:     state,
		MembershipStateName: stateName(groupStateNames, state),
		IsCreator:           group.GetCreatorId() == userID,
		CreatedAt:           group.GetCreateTime().GetSeconds(),
	}
}

// chatChannelFrom names the channel a stored message belongs to: the
// room's name, the group's id, or the other person in a direct
// conversation.
func chatChannelFrom(
	mode int32,
	subject, descriptor, label, senderID string,
) (channelType string, target string) {
	switch mode {
	case streamModeRoom:
		return "room", label
	case streamModeGroup:
		return "group", subject
	case streamModeDirect:
		if subject == senderID {
			return "direct", descriptor
		}
		return "direct", subject
	default:
		return "unknown", ""
	}
}

// stateName looks a Nakama enum value up in names, falling back to
// "unknown" for values newer than this code.
func stateName(names map[int32]string, state int32) string {
	if name, ok := names[state]; ok {
		return name
	}
	return "unknown"
}

// jsonValue embeds stored JSON as JSON in the export, so a player
// reads `{"volume": 3}` instead of an escaped string. Text that isn't
// JSON is kept as a string, and empty text as an empty object (what
// Nakama stores by default).
func jsonValue(raw string) any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	return raw
}

// unixSeconds converts a database timestamp to unix seconds, mapping
// Nakama's "never" sentinel (the unix epoch) to 0.
func unixSeconds(t time.Time) int64 {
	if t.IsZero() || t.Unix() <= 0 {
		return 0
	}
	return t.Unix()
}

// readFriendCode returns the player's friend code without creating
// one: an export must not add data. "" when the player has none.
func readFriendCode(
	ctx context.Context,
	nk runtime.NakamaModule,
	userID string,
) (string, error) {
	objects, err := nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: friendCodeCollection,
		Key:        friendCodeReverseKey(userID),
		UserID:     "",
	}})
	if err != nil {
		return "", err
	}
	for _, obj := range objects {
		var parsed struct {
			Code string `json:"code"`
		}
		if json.Unmarshal([]byte(obj.GetValue()), &parsed) == nil {
			return parsed.Code, nil
		}
	}
	return "", nil
}

func listExportFriends(
	ctx context.Context,
	nk runtime.NakamaModule,
	userID string,
) ([]exportFriend, error) {
	out := []exportFriend{}
	cursor := ""
	for page := 0; page < exportMaxPages; page++ {
		friends, next, err := nk.FriendsList(
			ctx, userID, exportFriendsPageSize, nil, cursor)
		if err != nil {
			return nil, err
		}
		for _, friend := range friends {
			if friend.GetUser().GetId() == "" {
				continue
			}
			out = append(out, exportFriendFrom(friend))
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return nil, fmt.Errorf("more than %d pages", exportMaxPages)
}

func listExportGroups(
	ctx context.Context,
	nk runtime.NakamaModule,
	userID string,
) ([]exportGroup, error) {
	out := []exportGroup{}
	cursor := ""
	for page := 0; page < exportMaxPages; page++ {
		memberships, next, err := nk.UserGroupsList(
			ctx, userID, exportGroupsPageSize, nil, cursor)
		if err != nil {
			return nil, err
		}
		for _, membership := range memberships {
			if membership.GetGroup().GetId() == "" {
				continue
			}
			group := exportGroupFrom(membership, userID)
			if group.IsParty {
				if err := addPartySettings(
					ctx, nk, &group,
					membership.GetGroup().GetCreatorId()); err != nil {
					return nil, err
				}
			}
			out = append(out, group)
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return nil, fmt.Errorf("more than %d pages", exportMaxPages)
}

// addPartySettings fills in the party's settings, which the runtime
// keeps in system-owned storage rows keyed by the party (party.go).
func addPartySettings(
	ctx context.Context,
	nk runtime.NakamaModule,
	group *exportGroup,
	creatorID string,
) error {
	leader, err := resolvePartyLeader(ctx, nk, group.GroupID, creatorID)
	if err != nil {
		return err
	}
	group.PartyLeaderID = leader
	mode, _, err := loadPartyMode(ctx, nk, group.GroupID)
	if err != nil {
		return err
	}
	group.PartyGameMode = mode
	levelPrefs, ok, err := loadPartyLevelPrefs(ctx, nk, group.GroupID)
	if err != nil {
		return err
	}
	if ok {
		group.PartyLevelPrefs = jsonValue(levelPrefs)
	}
	cheatPrefs, ok, err := loadPartyCheatPrefs(ctx, nk, group.GroupID)
	if err != nil {
		return err
	}
	if ok {
		group.PartyCheatPrefs = jsonValue(cheatPrefs)
	}
	return nil
}

// queryExportStorage reads every storage object the player owns.
// Direct SQL rather than nk.StorageList, whose query filters on one
// collection (an empty collection matches nothing; see the matching
// note in account.go). No row cap: an export must be complete.
func queryExportStorage(
	ctx context.Context,
	db *sql.DB,
	userID string,
) ([]exportStorageObject, error) {
	rows, err := db.QueryContext(
		ctx,
		`SELECT collection, key, value, create_time, update_time
		 FROM storage
		 WHERE user_id = $1::uuid
		 ORDER BY collection, key`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []exportStorageObject{}
	for rows.Next() {
		var (
			obj      exportStorageObject
			value    string
			created  time.Time
			modified time.Time
		)
		if err := rows.Scan(
			&obj.Collection, &obj.Key, &value,
			&created, &modified); err != nil {
			return nil, err
		}
		obj.Value = jsonValue(value)
		obj.CreatedAt = unixSeconds(created)
		obj.UpdatedAt = unixSeconds(modified)
		out = append(out, obj)
	}
	return out, rows.Err()
}

// queryExportLeaderboardRecords reads the player's records on every
// board straight from Nakama's table, so boards that no registered
// game lists any more (the legacy bare "ffa", say) are included too.
func queryExportLeaderboardRecords(
	ctx context.Context,
	db *sql.DB,
	userID string,
) ([]exportLeaderboardRecord, error) {
	rows, err := db.QueryContext(
		ctx,
		`SELECT leaderboard_id, score, subscore, num_score, metadata,
		        create_time, update_time, expiry_time
		 FROM leaderboard_record
		 WHERE owner_id = $1::uuid
		 ORDER BY leaderboard_id, expiry_time`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []exportLeaderboardRecord{}
	for rows.Next() {
		var (
			record   exportLeaderboardRecord
			metadata string
			created  time.Time
			modified time.Time
			expiry   time.Time
		)
		if err := rows.Scan(
			&record.LeaderboardID, &record.Score, &record.Subscore,
			&record.NumScore, &metadata,
			&created, &modified, &expiry); err != nil {
			return nil, err
		}
		record.Metadata = jsonValue(metadata)
		record.CreatedAt = unixSeconds(created)
		record.UpdatedAt = unixSeconds(modified)
		record.ExpiresAt = unixSeconds(expiry)
		out = append(out, record)
	}
	return out, rows.Err()
}

func queryExportNotifications(
	ctx context.Context,
	db *sql.DB,
	userID string,
) ([]exportNotification, error) {
	rows, err := db.QueryContext(
		ctx,
		`SELECT id, subject, content, code, sender_id, create_time
		 FROM notification
		 WHERE user_id = $1::uuid
		 ORDER BY create_time, id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []exportNotification{}
	for rows.Next() {
		var (
			notification exportNotification
			content      string
			created      time.Time
		)
		if err := rows.Scan(
			&notification.ID, &notification.Subject, &content,
			&notification.Code, &notification.SenderID,
			&created); err != nil {
			return nil, err
		}
		notification.Content = jsonValue(content)
		notification.CreatedAt = unixSeconds(created)
		out = append(out, notification)
	}
	return out, rows.Err()
}

func queryExportChatMessages(
	ctx context.Context,
	db *sql.DB,
	userID string,
) ([]exportChatMessage, error) {
	rows, err := db.QueryContext(
		ctx,
		`SELECT id, code, stream_mode, stream_subject,
		        stream_descriptor, stream_label, content,
		        create_time, update_time
		 FROM message
		 WHERE sender_id = $1::uuid
		 ORDER BY create_time, id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []exportChatMessage{}
	for rows.Next() {
		var (
			message    exportChatMessage
			mode       int32
			subject    string
			descriptor string
			label      string
			content    string
			created    time.Time
			modified   time.Time
		)
		if err := rows.Scan(
			&message.MessageID, &message.Code, &mode, &subject,
			&descriptor, &label, &content,
			&created, &modified); err != nil {
			return nil, err
		}
		message.Type = stateName(chatMessageTypeNames, message.Code)
		message.ChannelType, message.ChannelTarget = chatChannelFrom(
			mode, subject, descriptor, label, userID)
		message.Content = jsonValue(content)
		message.CreatedAt = unixSeconds(created)
		message.UpdatedAt = unixSeconds(modified)
		out = append(out, message)
	}
	return out, rows.Err()
}

func queryExportWalletLedger(
	ctx context.Context,
	db *sql.DB,
	userID string,
) ([]exportWalletLedgerEntry, error) {
	rows, err := db.QueryContext(
		ctx,
		`SELECT id, changeset, metadata, create_time, update_time
		 FROM wallet_ledger
		 WHERE user_id = $1::uuid
		 ORDER BY create_time, id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []exportWalletLedgerEntry{}
	for rows.Next() {
		var (
			entry     exportWalletLedgerEntry
			changeset string
			metadata  string
			created   time.Time
			modified  time.Time
		)
		if err := rows.Scan(
			&entry.ID, &changeset, &metadata,
			&created, &modified); err != nil {
			return nil, err
		}
		entry.Changeset = jsonValue(changeset)
		entry.Metadata = jsonValue(metadata)
		entry.CreatedAt = unixSeconds(created)
		entry.UpdatedAt = unixSeconds(modified)
		out = append(out, entry)
	}
	return out, rows.Err()
}
