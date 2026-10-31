package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Personal-data rules shared by account deletion (account.go), the
// account cron (account_cron.go), and the hooks registered here:
//
//   - No email. Nakama copies the email out of a Google, Facebook, or
//     Apple profile on sign-in and on linking, and nothing on the
//     platform uses it. Clients stopped asking providers for it in
//     SDK 0.6.0, but older builds still do, so every sign-in, link,
//     and session refresh drops an email that isn't a login
//     credential. Email sign-in and email linking, which would store
//     one as a credential, are refused: the platform offers neither.
//   - A pending deletion keeps its profile clear. The deletion request
//     clears the profile, but a provider sign-in during the grace
//     window refills an empty avatar from the provider's profile, so
//     the same hooks re-apply the clear, and the account cron does
//     too every hour.
//   - Nothing names a deleted player. Rows that aren't the player's
//     own (their friend code, their entries in other players'
//     recent-players lists, the notifications they sent) go at the
//     deletion request and at the hard delete, however it happens.
//
// Clears made in the background (hooks, cron) leave update_time
// alone: player-metrics-daily counts an account as active from it.

// errEmailSignInUnavailable refuses email sign-in and linking.
var errEmailSignInUnavailable = runtime.NewError(
	"email sign-in is not available", 7)

// profileScrubAssignments is what a deletion request clears from the
// account row. A password would make the email a login credential,
// which a sign-back-in cancellation needs, so that one would stay;
// the platform refuses email sign-in, so in practice every email
// goes. Sign-in ids (provider ids, devices, custom id) stay for the
// same reason. The username stays: Nakama requires one, and it is a
// random handle Nakama generated, not something the player chose.
const profileScrubAssignments = `
	display_name = $2,
	avatar_url = NULL,
	email = CASE WHEN password IS NULL THEN NULL ELSE email END,
	lang_tag = 'en',
	location = NULL,
	timezone = NULL,
	metadata = '{}'::jsonb`

// profileNeedsScrub is true when the row holds anything the scrub
// clears, so re-applying the scrub to a clean row writes nothing.
const profileNeedsScrub = `(
	display_name IS DISTINCT FROM $2
	OR avatar_url IS NOT NULL
	OR (email IS NOT NULL AND password IS NULL)
	OR lang_tag <> 'en'
	OR location IS NOT NULL
	OR timezone IS NOT NULL
	OR metadata <> '{}'::jsonb)`

// scrubDeletedAccountProfile clears the profile when a deletion is
// requested. The display name becomes the "[deleted]" marker.
func scrubDeletedAccountProfile(
	ctx context.Context,
	db *sql.DB,
	userID string,
) error {
	if db == nil {
		return errors.New("no database handle")
	}
	_, err := db.ExecContext(
		ctx,
		`UPDATE users SET`+profileScrubAssignments+`,
		     update_time = now()
		 WHERE id = $1::uuid`,
		userID,
		anonymizedDisplayName,
	)
	return err
}

// enforcePendingDeletionProfile re-applies the deletion-request
// scrub while the player's deletion is still pending. The pending
// check sits in the same statement, so a cancellation that removes
// the queue row can't be undone by a scrub already in flight.
// Reports whether a row changed.
func enforcePendingDeletionProfile(
	ctx context.Context,
	db *sql.DB,
	userID string,
) (bool, error) {
	if db == nil {
		return false, errors.New("no database handle")
	}
	result, err := db.ExecContext(
		ctx,
		`UPDATE users SET`+profileScrubAssignments+`
		 WHERE id = $1::uuid
		   AND `+profileNeedsScrub+`
		   AND EXISTS (
		       SELECT 1 FROM storage
		       WHERE collection = $3 AND key = $4
		         AND user_id = $1::uuid)`,
		userID,
		anonymizedDisplayName,
		accountDeletionCollection,
		accountDeletionKey,
	)
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	return changed > 0, nil
}

// clearImportedEmail drops an email a sign-in provider's profile put
// on the account. One that is a login credential (the account has a
// password) stays.
func clearImportedEmail(
	ctx context.Context,
	db *sql.DB,
	userID string,
) error {
	if db == nil {
		return errors.New("no database handle")
	}
	_, err := db.ExecContext(
		ctx,
		`UPDATE users SET email = NULL
		 WHERE id = $1::uuid
		   AND email IS NOT NULL AND password IS NULL`,
		userID,
	)
	return err
}

// minimizeDeletionQueueRow strips the profile copy that queue rows
// written before 2026-10-11 kept (original_username,
// original_display_name). One statement, so it can't resurrect a row
// a cancellation just deleted.
func minimizeDeletionQueueRow(
	ctx context.Context,
	db *sql.DB,
	userID string,
) error {
	if db == nil {
		return errors.New("no database handle")
	}
	_, err := db.ExecContext(
		ctx,
		`UPDATE storage
		 SET value = value - 'original_username'
		                   - 'original_display_name',
		     version = md5((value - 'original_username'
		                         - 'original_display_name')::text)
		 WHERE collection = $1 AND key = $2 AND user_id = $3::uuid
		   AND ((value ->> 'original_username') IS NOT NULL
		        OR (value ->> 'original_display_name') IS NOT NULL)`,
		accountDeletionCollection,
		accountDeletionKey,
		userID,
	)
	return err
}

// deleteRecentPlayerReferences deletes the rows naming the player in
// other players' recent-players lists. Each row is keyed by the
// other player's id and holds their username and display name
// (recent_players.go), and it is owned by the list's owner, so
// neither the storage scrub nor Nakama's own account delete reaches
// it.
func deleteRecentPlayerReferences(
	ctx context.Context,
	db *sql.DB,
	userID string,
) error {
	if db == nil {
		return errors.New("no database handle")
	}
	_, err := db.ExecContext(
		ctx,
		`DELETE FROM storage WHERE collection = $1 AND key = $2`,
		recentPlayersCollection,
		userID,
	)
	return err
}

// deleteSentNotifications deletes the notifications the player's
// actions left in other players' inboxes: a friend request, for one,
// names them. Nakama's account delete only cascades a notification's
// recipient.
func deleteSentNotifications(
	ctx context.Context,
	db *sql.DB,
	userID string,
) error {
	if db == nil {
		return errors.New("no database handle")
	}
	_, err := db.ExecContext(
		ctx,
		`DELETE FROM notification WHERE sender_id = $1::uuid`,
		userID,
	)
	return err
}

// deleteUserActivityRows deletes the player's notifications, the
// chat messages they sent (Nakama also records group join, leave,
// and promote events as messages sent by the member), and their
// wallet history.
func deleteUserActivityRows(
	ctx context.Context,
	db *sql.DB,
	userID string,
) error {
	if db == nil {
		return errors.New("no database handle")
	}
	statements := []string{
		`DELETE FROM notification WHERE user_id = $1::uuid`,
		`DELETE FROM message WHERE sender_id = $1::uuid`,
		`DELETE FROM wallet_ledger WHERE user_id = $1::uuid`,
	}
	var errs []error
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement, userID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// leaderboardIDsForOwner lists every board the player has a record
// on, including boards no registered game lists any more.
func leaderboardIDsForOwner(
	ctx context.Context,
	db *sql.DB,
	userID string,
) ([]string, error) {
	if db == nil {
		return nil, errors.New("no database handle")
	}
	rows, err := db.QueryContext(
		ctx,
		`SELECT DISTINCT leaderboard_id FROM leaderboard_record
		 WHERE owner_id = $1::uuid ORDER BY leaderboard_id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// purgeUserReferences removes what names a player outside their own
// rows: their friend code (system-owned rows), their entries in
// other players' recent-players lists, and the notifications they
// sent. Best-effort; failures log.
func purgeUserReferences(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	userID string,
) {
	deleteFriendCode(ctx, logger, nk, userID)
	if err := deleteRecentPlayerReferences(ctx, db, userID); err != nil {
		logger.Warn(
			"delete recent-player rows naming %s: %v", userID, err)
	}
	if err := deleteSentNotifications(ctx, db, userID); err != nil {
		logger.Warn(
			"delete notifications sent by %s: %v", userID, err)
	}
}

// cancelPendingDeletion deletes the player's queue row and clears the
// "[deleted]" display name marker in one transaction, so a profile
// re-scrub can't land between the two. Reports the deletion's
// scheduled time and whether there was a row to cancel. A name set
// during the grace window survives: only the marker is cleared.
func cancelPendingDeletion(
	ctx context.Context,
	db *sql.DB,
	userID string,
) (int64, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var value string
	err = tx.QueryRowContext(
		ctx,
		`DELETE FROM storage
		 WHERE collection = $1 AND key = $2 AND user_id = $3::uuid
		 RETURNING value`,
		accountDeletionCollection,
		accountDeletionKey,
		userID,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE users SET display_name = NULL, update_time = now()
		 WHERE id = $1::uuid AND display_name = $2`,
		userID,
		anonymizedDisplayName,
	); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	committed = true
	// A malformed row is cancelled all the same; the time is only
	// for the log.
	var record deletionQueueRecord
	_ = json.Unmarshal([]byte(value), &record)
	return record.ScheduledFor, true, nil
}

// enforceSignInPrivacy runs after every sign-in, link, and session
// refresh: it drops an imported email, and re-clears the profile of
// an account whose deletion is pending. After-hook errors never reach
// the client, so failures only log.
func enforceSignInPrivacy(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
) {
	userID, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if userID == "" {
		return
	}
	if err := clearImportedEmail(ctx, db, userID); err != nil {
		logger.Warn("clear imported email for %s: %v", userID, err)
	}
	if _, err := enforcePendingDeletionProfile(
		ctx, db, userID); err != nil {
		logger.Warn(
			"re-clear pending deletion profile for %s: %v",
			userID, err)
	}
}

// registerPrivacyHooks wires the hooks described in the file comment.
func registerPrivacyHooks(initializer runtime.Initializer) error {
	if err := initializer.RegisterBeforeAuthenticateEmail(
		func(
			context.Context, runtime.Logger, *sql.DB,
			runtime.NakamaModule, *api.AuthenticateEmailRequest,
		) (*api.AuthenticateEmailRequest, error) {
			return nil, errEmailSignInUnavailable
		}); err != nil {
		return fmt.Errorf("before authenticate email: %w", err)
	}
	if err := initializer.RegisterBeforeLinkEmail(
		func(
			context.Context, runtime.Logger, *sql.DB,
			runtime.NakamaModule, *api.AccountEmail,
		) (*api.AccountEmail, error) {
			return nil, errEmailSignInUnavailable
		}); err != nil {
		return fmt.Errorf("before link email: %w", err)
	}

	// Links and session refreshes. Sign-in itself runs
	// enforceSignInPrivacy inside the one after-authenticate
	// function per provider (registerAfterAuthenticateHooks,
	// SP106); Nakama keeps only one such function, so it
	// cannot also be registered here. Each closure only
	// adapts Nakama's per-request signature.
	registrations := []func() error{
		func() error {
			return initializer.RegisterAfterLinkGoogle(func(
				ctx context.Context, logger runtime.Logger, db *sql.DB,
				_ runtime.NakamaModule, _ *api.AccountGoogle,
			) error {
				enforceSignInPrivacy(ctx, logger, db)
				return nil
			})
		},
		func() error {
			return initializer.RegisterAfterLinkFacebook(func(
				ctx context.Context, logger runtime.Logger, db *sql.DB,
				_ runtime.NakamaModule, _ *api.LinkFacebookRequest,
			) error {
				enforceSignInPrivacy(ctx, logger, db)
				return nil
			})
		},
		func() error {
			return initializer.RegisterAfterLinkApple(func(
				ctx context.Context, logger runtime.Logger, db *sql.DB,
				_ runtime.NakamaModule, _ *api.AccountApple,
			) error {
				enforceSignInPrivacy(ctx, logger, db)
				return nil
			})
		},
		func() error {
			return initializer.RegisterAfterSessionRefresh(func(
				ctx context.Context, logger runtime.Logger, db *sql.DB,
				nk runtime.NakamaModule, _ *api.Session,
				_ *api.SessionRefreshRequest,
			) error {
				// A resumed session runs no after-authenticate
				// hook, so the SP108 naming for nameless
				// accounts happens here too, inside this one
				// registered function (SP106's rule). Naming
				// first: the pending-deletion re-clear inside
				// enforceSignInPrivacy has the last word.
				nameExistingIfNameless(ctx, logger, nk)
				enforceSignInPrivacy(ctx, logger, db)
				return nil
			})
		},
		// Nakama's own immediate delete (DELETE /v2/account) cascades
		// the player's rows but not the references purged here.
		func() error {
			return initializer.RegisterAfterDeleteAccount(func(
				ctx context.Context, logger runtime.Logger, db *sql.DB,
				nk runtime.NakamaModule,
			) error {
				userID, _ := ctx.Value(
					runtime.RUNTIME_CTX_USER_ID).(string)
				if userID != "" {
					purgeUserReferences(ctx, logger, db, nk, userID)
				}
				return nil
			})
		},
	}
	for _, register := range registrations {
		if err := register(); err != nil {
			return err
		}
	}
	return nil
}
