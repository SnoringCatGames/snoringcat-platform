package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Party chat abuse limits (SP37). Chat rides Nakama group
// channels, which no API Before hook covers, so the checks run
// in a BeforeRt hook on ChannelMessageSend:
//
//   - Length: the text is capped at partyChatMaxRunes, the
//     client's own limit (party_chat_panel.gd). The client cap
//     alone left the wire open to any other client.
//   - Rate: one sliding-window budget per sender, shared with
//     the friend-add limiter's shape (10 per minute).
//   - Blocks: a message is refused when another active party
//     member has blocked the sender.
//
// Only party group channels are limited. Room and direct
// channels pass through, as do group channels that are not
// parties.

const (
	// partyChatMaxRunes mirrors the client's message cap.
	partyChatMaxRunes = 500
	// partyChatMaxContentBytes bounds the whole JSON payload,
	// text field or not.
	partyChatMaxContentBytes = 4096
)

// registerPartyChatHook installs the BeforeRt chat hook. The
// limiter is dedicated to chat, so chat bursts never burn the
// friend-add budget or the other way round.
func registerPartyChatHook(
	initializer runtime.Initializer,
	limiter *friendsLimiter,
) error {
	return initializer.RegisterBeforeRt(
		"ChannelMessageSend",
		func(
			ctx context.Context,
			logger runtime.Logger,
			_ *sql.DB,
			nk runtime.NakamaModule,
			in *rtapi.Envelope,
		) (*rtapi.Envelope, error) {
			senderID, _ := ctx.Value(
				runtime.RUNTIME_CTX_USER_ID).(string)
			if senderID == "" {
				return in, nil
			}
			msg := in.GetChannelMessageSend()
			if msg == nil {
				return in, nil
			}
			if err := checkPartyChat(
				ctx, nk, limiter, senderID,
				msg.GetChannelId(), msg.GetContent(),
			); err != nil {
				logger.Warn(
					"party chat refused user=%s: %v",
					senderID, err)
				return nil, err
			}
			return in, nil
		})
}

// checkPartyChat applies the SP37 limits to one message.
// Non-party channels return nil at once.
func checkPartyChat(
	ctx context.Context,
	nk runtime.NakamaModule,
	limiter *friendsLimiter,
	senderID, channelID, content string,
) error {
	partyID, isParty := partyIDForChannel(ctx, nk, channelID)
	if !isParty {
		return nil
	}
	if len(content) > partyChatMaxContentBytes {
		return runtime.NewError("message too long", 3)
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &body); err == nil {
		if utf8.RuneCountInString(body.Text) > partyChatMaxRunes {
			return runtime.NewError("message too long", 3)
		}
	}
	if limiter != nil && !limiter.allowFriendCodeCall(senderID) {
		return runtime.NewError(
			"chat rate limit exceeded; wait a minute"+
				" and try again", 8)
	}
	// The block check fails open: a storage error must not
	// silence the whole party.
	blocked, err := senderBlockedByMember(ctx, nk, partyID, senderID)
	if err == nil && blocked {
		return runtime.NewError("blocked by a party member", 7)
	}
	return nil
}

// partyIDForChannel resolves a channel id to its party group,
// tolerating Nakama's channel-id decorations ("group." /
// "group:" prefixes, or the bare group id). Returns isParty
// false for any channel that is not a party group channel.
func partyIDForChannel(
	ctx context.Context,
	nk runtime.NakamaModule,
	channelID string,
) (string, bool) {
	candidates := []string{channelID}
	for _, prefix := range []string{"group.", "group:"} {
		if strings.HasPrefix(channelID, prefix) {
			candidates = append(
				candidates, strings.TrimPrefix(channelID, prefix))
		}
	}
	if idx := strings.LastIndexAny(channelID, ".:"); idx >= 0 {
		candidates = append(candidates, channelID[idx+1:])
	}
	for _, candidate := range candidates {
		if !isUUIDLike(candidate) {
			continue
		}
		// Nakama refuses malformed group ids, and a lookup
		// error only means this candidate is not the group:
		// resolution fails open so chat never breaks on it.
		groups, err := nk.GroupsGetId(ctx, []string{candidate})
		if err != nil || len(groups) == 0 {
			continue
		}
		if strings.HasPrefix(groups[0].GetName(), partyGroupPrefix) {
			return candidate, true
		}
		return "", false
	}
	return "", false
}

// isUUIDLike reports whether s has the 8-4-4-4-12 hex shape
// of a Nakama group id.
func isUUIDLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') ||
				(c >= 'a' && c <= 'f') ||
				(c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// senderBlockedByMember reports whether any active member of
// the party other than the sender has blocked the sender
// (a state=3 row on that member's friend list).
func senderBlockedByMember(
	ctx context.Context,
	nk runtime.NakamaModule,
	partyID, senderID string,
) (bool, error) {
	members, _, err := nk.GroupUsersList(ctx, partyID, 100, nil, "")
	if err != nil {
		return false, err
	}
	state := friendStateBanned
	for _, m := range members {
		if m.User == nil || m.User.Id == "" || m.User.Id == senderID {
			continue
		}
		if m.State != nil && m.State.Value == 3 {
			continue
		}
		friends, _, err := nk.FriendsList(
			ctx, m.User.Id, 100, &state, "")
		if err != nil {
			return false, err
		}
		for _, f := range friends {
			if f.User != nil && f.User.Id == senderID {
				return true, nil
			}
		}
	}
	return false, nil
}
