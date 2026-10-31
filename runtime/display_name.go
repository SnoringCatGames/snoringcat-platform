package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Display names are generated, never taken from a provider
// profile (H59, DN-1): a Google profile name is often the
// player's real name, and it used to become the public display
// and leaderboard name. Players never enter a name anywhere;
// every account gets a generated one. Social providers hand
// Nakama a profile name when the account is created, so the
// after-authenticate hooks (registerAfterAuthenticateHooks)
// replace it at creation time (out.Created), before anything
// can display it. The name is then stable: later sign-ins
// keep the stored one.

var displayNameAdjectives = []string{
	"Swift", "Brave", "Calm", "Daring", "Eager", "Fancy",
	"Gentle", "Happy", "Jolly", "Keen", "Lucky", "Merry",
	"Nimble", "Plucky", "Quick", "Quiet", "Sunny", "Witty",
	"Zesty", "Cosy",
}

var displayNameCreatures = []string{
	"Otter", "Badger", "Bunny", "Falcon", "Ferret", "Finch",
	"Fox", "Heron", "Lynx", "Mole", "Newt", "Owl",
	"Panda", "Quail", "Robin", "Stoat", "Toad", "Vole",
	"Weasel", "Wren",
}

// generatedDisplayName mints one name, e.g. "SwiftOtter_4821".
func generatedDisplayName() (string, error) {
	adj, err := pickDisplayWord(displayNameAdjectives)
	if err != nil {
		return "", err
	}
	creature, err := pickDisplayWord(displayNameCreatures)
	if err != nil {
		return "", err
	}
	num, err := rand.Int(rand.Reader, big.NewInt(9000))
	if err != nil {
		return "", fmt.Errorf("display name number: %w", err)
	}
	return fmt.Sprintf("%s%s_%d", adj, creature, num.Int64()+1000), nil
}

func pickDisplayWord(words []string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return "", fmt.Errorf("display name word: %w", err)
	}
	return words[n.Int64()], nil
}

// assignGeneratedNameOnCreate gives a just-created account a
// generated display name (H59), replacing any provider-supplied
// one. Device and custom accounts supply none, so without this
// they would keep an empty name (H101). A failure only warns:
// the account must still sign in.
func assignGeneratedNameOnCreate(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	created bool,
) error {
	if !created {
		return nil
	}
	userID, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if userID == "" {
		return nil
	}
	name, err := generatedDisplayName()
	if err != nil {
		logger.Warn("generated display name: %v", err)
		return nil
	}
	if err := nk.AccountUpdateId(
		ctx, userID, "", nil, name, "", "", "", "",
	); err != nil {
		logger.Warn(
			"assign generated display name for %s: %v", userID, err)
	}
	return nil
}

// nameExistingIfNameless gives an existing account whose
// stored display name is empty a generated one (SP108).
// Accounts created while the naming hooks were shadowed
// (SP106) never got a name, and the public leaderboard
// hides a nameless account; this heals them at their next
// sign-in or session refresh. A stored name is never
// replaced: the deletion marker included, which sign-in
// privacy re-clears after this runs.
func nameExistingIfNameless(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
) {
	userID, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if userID == "" {
		return
	}
	account, err := nk.AccountGetId(ctx, userID)
	if err != nil || account == nil || account.GetUser() == nil {
		logger.Warn(
			"name nameless account: read user=%s err=%v",
			userID, err)
		return
	}
	if strings.TrimSpace(
		account.GetUser().GetDisplayName()) != "" {
		return
	}
	name, err := generatedDisplayName()
	if err != nil {
		logger.Warn("generated display name: %v", err)
		return
	}
	if err := nk.AccountUpdateId(
		ctx, userID, "", nil, name, "", "", "", "",
	); err != nil {
		logger.Warn(
			"name nameless account for %s: %v", userID, err)
	}
}
