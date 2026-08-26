package quota

import (
	"encoding/json"
	"fmt"
	"time"
)

// ReplacePendingRequests checkpoints only terminal-callback correlation data.
// It contains no raw API Key, account quota, entitlement, or budget estimate.
func (store *Store) ReplacePendingRequests(markers []pendingRequest) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM pending_request_markers`); err != nil {
		return err
	}
	statement, err := tx.Prepare(`INSERT INTO pending_request_markers(key_id,auth_id,model,request_content,requested_at,managed,
rate_input_micros,rate_cached_micros,rate_output_micros,rate_profile_json) VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, marker := range markers {
		if marker.KeyID == "" || marker.RequestedAt.IsZero() {
			continue
		}
		profile, err := json.Marshal(marker.Rate)
		if err != nil {
			return fmt.Errorf("encode pending request rate: %w", err)
		}
		if _, err := statement.Exec(marker.KeyID, marker.AuthID, marker.Model, marker.Content, marker.RequestedAt.UTC().UnixMilli(),
			boolToInt(marker.Managed), marker.Rate.inputMicrosPerMillion, marker.Rate.cacheReadMicrosPerMillion, marker.Rate.outputMicrosPerMillion, string(profile)); err != nil {
			return fmt.Errorf("checkpoint pending request: %w", err)
		}
	}
	return tx.Commit()
}

func (store *Store) LoadPendingRequests(now time.Time) ([]pendingRequest, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	cutoff := now.UTC().Add(-pendingRequestTTL).UnixMilli()
	if _, err := store.db.Exec(`DELETE FROM pending_request_markers WHERE requested_at < ?`, cutoff); err != nil {
		return nil, err
	}
	rows, err := store.db.Query(`SELECT key_id,auth_id,model,request_content,requested_at,managed,rate_input_micros,rate_cached_micros,rate_output_micros,rate_profile_json
FROM pending_request_markers ORDER BY requested_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	markers := make([]pendingRequest, 0)
	for rows.Next() {
		var marker pendingRequest
		var requestedAt int64
		var managed int
		var input, cached, output int64
		var profile string
		if err := rows.Scan(&marker.KeyID, &marker.AuthID, &marker.Model, &marker.Content, &requestedAt, &managed, &input, &cached, &output, &profile); err != nil {
			return nil, err
		}
		marker.RequestedAt = time.UnixMilli(requestedAt).UTC()
		marker.Managed = managed != 0
		marker.Checkpointed = true
		marker.Rate = rateFromStored(marker.Model, input, cached, output, marker.RequestedAt)
		if profile != "" {
			if err := json.Unmarshal([]byte(profile), &marker.Rate); err != nil {
				return nil, fmt.Errorf("decode pending request rate: %w", err)
			}
			marker.Rate.Model = marker.Model
			marker.Rate, err = normalizeModelRate(marker.Rate)
			if err != nil {
				return nil, fmt.Errorf("normalize pending request rate: %w", err)
			}
		}
		markers = append(markers, marker)
	}
	return markers, rows.Err()
}

func (store *Store) DeletePendingRequest(marker pendingRequest) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	_, err := store.db.Exec(`DELETE FROM pending_request_markers WHERE key_id=? AND auth_id=? AND model=? AND requested_at=?`,
		marker.KeyID, marker.AuthID, marker.Model, marker.RequestedAt.UTC().UnixMilli())
	return err
}
