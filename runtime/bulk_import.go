package main

// bulk_import RPC — accepts batches of legacy DDB records,
// writes them into Nakama Storage (and the leaderboard for the
// "leaderboards" type). Idempotent: each record's storage key is
// derived from the legacy primary key, so re-running the
// migration overwrites a record with itself.
//
// Called from scripts/migrate_ddb_to_nakama.py via the HTTP
// gateway with `?http_key=...`. The HTTP key is a Nakama runtime
// config value (set in infra/remote/nakama/docker-compose.yml).
//
// Payload shape:
//   { "type": "<players|friends|...>",
//     "namespace": "" | "staging-",
//     "records": [...] }
//
// Response:
//   { "written": N, "failed": M, "errors": [...] }

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/heroiclabs/nakama-common/runtime"
)

type bulkImportRequest struct {
	Type      string                   `json:"type"`
	Namespace string                   `json:"namespace"`
	Records   []map[string]interface{} `json:"records"`
}

type bulkImportResponse struct {
	Written int      `json:"written"`
	Failed  int      `json:"failed"`
	Errors  []string `json:"errors,omitempty"`
}

func bulkImportRpc(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	payload string,
) (string, error) {
	if err := requireServerToServer(ctx); err != nil {
		return "", err
	}
	req := bulkImportRequest{}
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	logger.Info("bulk_import: type=%s ns=%q n=%d",
		req.Type, req.Namespace, len(req.Records))

	var resp bulkImportResponse
	switch req.Type {
	case "players":
		resp = importPlayers(ctx, nk, req.Namespace, req.Records)
	case "settings":
		resp = importSettings(ctx, nk, req.Namespace, req.Records)
	case "match_history":
		resp = importMatchHistory(
			ctx, nk, req.Namespace, req.Records)
	case "leaderboards":
		resp = importLeaderboards(
			ctx, nk, req.Namespace, req.Records)
	case "parties", "friends":
		// Friends + parties need their own targeted graph writes.
		// Stubbed for now (returns failed=N) so the migration
		// script logs them rather than silently succeeding.
		resp = bulkImportResponse{
			Failed: len(req.Records),
			Errors: []string{
				fmt.Sprintf(
					"%s import not implemented yet", req.Type)},
		}
	default:
		return "", runtime.NewError(
			"unknown bulk_import type: "+req.Type, 3)
	}

	out, _ := json.Marshal(resp)
	return string(out), nil
}

func importPlayers(
	ctx context.Context,
	nk runtime.NakamaModule,
	ns string,
	records []map[string]interface{},
) bulkImportResponse {
	resp := bulkImportResponse{}
	writes := make([]*runtime.StorageWrite, 0, len(records))
	for _, r := range records {
		pid, _ := r["player_id"].(string)
		if pid == "" {
			resp.Failed++
			continue
		}
		v, err := json.Marshal(r)
		if err != nil {
			resp.Failed++
			continue
		}
		writes = append(writes, &runtime.StorageWrite{
			Collection:      ns + "players",
			Key:             pid,
			UserID:          "",
			Value:           string(v),
			PermissionRead:  2,
			PermissionWrite: 0,
		})
	}
	if len(writes) > 0 {
		_, err := nk.StorageWrite(ctx, writes)
		if err != nil {
			resp.Failed += len(writes)
			resp.Errors = append(resp.Errors, err.Error())
		} else {
			resp.Written += len(writes)
		}
	}
	return resp
}

func importSettings(
	ctx context.Context,
	nk runtime.NakamaModule,
	ns string,
	records []map[string]interface{},
) bulkImportResponse {
	resp := bulkImportResponse{}
	writes := make([]*runtime.StorageWrite, 0, len(records))
	for _, r := range records {
		pid, _ := r["player_id"].(string)
		scope, _ := r["scope"].(string)
		if scope == "" {
			scope = "global"
		}
		if pid == "" {
			resp.Failed++
			continue
		}
		val := r["value"]
		v, _ := json.Marshal(val)
		writes = append(writes, &runtime.StorageWrite{
			Collection:      ns + "settings",
			Key:             scope,
			UserID:          pid,
			Value:           string(v),
			PermissionRead:  2,
			PermissionWrite: 1,
		})
	}
	if len(writes) > 0 {
		_, err := nk.StorageWrite(ctx, writes)
		if err != nil {
			resp.Failed += len(writes)
			resp.Errors = append(resp.Errors, err.Error())
		} else {
			resp.Written += len(writes)
		}
	}
	return resp
}

func importMatchHistory(
	ctx context.Context,
	nk runtime.NakamaModule,
	ns string,
	records []map[string]interface{},
) bulkImportResponse {
	resp := bulkImportResponse{}
	writes := make([]*runtime.StorageWrite, 0, len(records))
	for _, r := range records {
		pid, _ := r["player_id"].(string)
		mid, _ := r["match_id"].(string)
		if pid == "" || mid == "" {
			resp.Failed++
			continue
		}
		v, _ := json.Marshal(r)
		writes = append(writes, &runtime.StorageWrite{
			Collection:      ns + "match_history",
			Key:             mid,
			UserID:          pid,
			Value:           string(v),
			PermissionRead:  2,
			PermissionWrite: 0,
		})
	}
	if len(writes) > 0 {
		_, err := nk.StorageWrite(ctx, writes)
		if err != nil {
			resp.Failed += len(writes)
			resp.Errors = append(resp.Errors, err.Error())
		} else {
			resp.Written += len(writes)
		}
	}
	return resp
}

func importLeaderboards(
	ctx context.Context,
	nk runtime.NakamaModule,
	ns string,
	records []map[string]interface{},
) bulkImportResponse {
	resp := bulkImportResponse{}
	for _, r := range records {
		pid, _ := r["player_id"].(string)
		boardID, _ := r["leaderboard_id"].(string)
		if boardID == "" {
			boardID = "ffa"
		}
		if ns != "" {
			boardID = ns + boardID
		}
		score := int64(0)
		switch s := r["score"].(type) {
		case float64:
			score = int64(s)
		case int64:
			score = s
		case int:
			score = int64(s)
		}
		if pid == "" {
			resp.Failed++
			continue
		}
		_, err := nk.LeaderboardRecordWrite(
			ctx, boardID, pid, "", score, 0, r, nil)
		if err != nil {
			resp.Failed++
			resp.Errors = append(resp.Errors, err.Error())
		} else {
			resp.Written++
		}
	}
	return resp
}
