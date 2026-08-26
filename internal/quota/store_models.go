package quota

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

func (store *Store) ReplaceModelCatalog(models []ModelCatalogEntry, syncedAt time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE model_catalog SET available=0,synced_at=?`, syncedAt.UTC().UnixMilli()); err != nil {
		return err
	}
	statement, err := tx.Prepare(`INSERT INTO model_catalog(model_id,display_name,owner,available,synced_at) VALUES(?,?,?,?,?)
ON CONFLICT(model_id) DO UPDATE SET display_name=excluded.display_name,owner=excluded.owner,available=excluded.available,synced_at=excluded.synced_at`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, model := range models {
		if _, err := statement.Exec(model.ID, model.DisplayName, model.Owner, boolToInt(model.Available), syncedAt.UTC().UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (store *Store) ListModelCatalog() ([]ModelCatalogEntry, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT model_id,display_name,owner,available,synced_at FROM model_catalog ORDER BY model_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ModelCatalogEntry, 0)
	for rows.Next() {
		var item ModelCatalogEntry
		var available int
		var synced int64
		if err := rows.Scan(&item.ID, &item.DisplayName, &item.Owner, &available, &synced); err != nil {
			return nil, err
		}
		item.Available = available != 0
		item.SyncedAt = time.UnixMilli(synced).UTC()
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *Store) ListModelRates() ([]ModelRate, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT model,input_micros_per_million,cached_micros_per_million,output_micros_per_million,profile_json,updated_at FROM model_rates ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ModelRate, 0)
	for rows.Next() {
		var model string
		var input, cached, output, updated int64
		var profile string
		if err := rows.Scan(&model, &input, &cached, &output, &profile, &updated); err != nil {
			return nil, err
		}
		item := rateFromStored(model, input, cached, output, time.UnixMilli(updated).UTC())
		if strings.TrimSpace(profile) != "" {
			if err := json.Unmarshal([]byte(profile), &item); err != nil {
				return nil, fmt.Errorf("decode model rate %q: %w", model, err)
			}
			item.Model, item.UpdatedAt = model, time.UnixMilli(updated).UTC()
			item, err = normalizeModelRate(item)
			if err != nil {
				return nil, fmt.Errorf("normalize stored model rate %q: %w", model, err)
			}
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// SeedDefaultModelRates initializes seed data only when the rate card is empty.
func (store *Store) SeedDefaultModelRates(rates []ModelRate) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM model_rates`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.Prepare(`INSERT INTO model_rates(model,input_micros_per_million,cached_micros_per_million,output_micros_per_million,profile_json,updated_at) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	now := time.Now().UTC().UnixMilli()
	for _, rate := range rates {
		normalized, err := normalizeModelRate(rate)
		if err != nil {
			return err
		}
		profile, err := json.Marshal(normalized)
		if err != nil {
			return err
		}
		if _, err := statement.Exec(normalized.Model, normalized.inputMicrosPerMillion, normalized.cacheReadMicrosPerMillion, normalized.outputMicrosPerMillion, string(profile), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (store *Store) ReplaceModelRates(rates []ModelRate) error {
	normalized := make([]ModelRate, 0, len(rates))
	seen := make(map[string]struct{}, len(rates))
	for _, rate := range rates {
		item, err := normalizeModelRate(rate)
		if err != nil {
			return err
		}
		if _, exists := seen[item.Model]; exists {
			return fmt.Errorf("model rate %q is duplicated", item.Model)
		}
		seen[item.Model] = struct{}{}
		normalized = append(normalized, item)
	}
	sort.Slice(normalized, func(left, right int) bool { return normalized[left].Model < normalized[right].Model })
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM model_rates`); err != nil {
		return err
	}
	statement, err := tx.Prepare(`INSERT INTO model_rates(model,input_micros_per_million,cached_micros_per_million,output_micros_per_million,profile_json,updated_at) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	now := time.Now().UTC().UnixMilli()
	for _, rate := range normalized {
		if strings.TrimSpace(rate.Model) == "" {
			continue
		}
		profile, err := json.Marshal(rate)
		if err != nil {
			return err
		}
		if _, err := statement.Exec(rate.Model, rate.inputMicrosPerMillion, rate.cacheReadMicrosPerMillion, rate.outputMicrosPerMillion, string(profile), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReconcileSynchronizedModelRates atomically replaces one synchronization
// source and its status while preserving every manually maintained rate.
func (store *Store) ReconcileSynchronizedModelRates(source string, rates []ModelRate, status ModelRateSyncStatus) ([]string, ModelRateSyncStatus, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, status, fmt.Errorf("model rate synchronization source is required")
	}
	normalized := make([]ModelRate, 0, len(rates))
	keep := make(map[string]struct{}, len(rates))
	for _, raw := range rates {
		rate, err := normalizeModelRate(raw)
		if err != nil {
			return nil, status, err
		}
		if rate.Source != source {
			return nil, status, fmt.Errorf("model rate %q has synchronization source %q, want %q", rate.Model, rate.Source, source)
		}
		if _, exists := keep[rate.Model]; exists {
			return nil, status, fmt.Errorf("model rate %q is duplicated", rate.Model)
		}
		keep[rate.Model] = struct{}{}
		normalized = append(normalized, rate)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return nil, status, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT model,profile_json FROM model_rates`)
	if err != nil {
		return nil, status, err
	}
	retired := make([]string, 0)
	for rows.Next() {
		var model, profile string
		if err := rows.Scan(&model, &profile); err != nil {
			_ = rows.Close()
			return nil, status, err
		}
		var stored ModelRate
		if json.Unmarshal([]byte(profile), &stored) == nil && stored.Source == source {
			if _, exists := keep[model]; !exists {
				retired = append(retired, model)
			}
		}
	}
	if err := rows.Close(); err != nil {
		return nil, status, err
	}
	for _, model := range retired {
		if _, err := tx.Exec(`DELETE FROM model_rates WHERE model=?`, model); err != nil {
			return nil, status, err
		}
	}
	statement, err := tx.Prepare(`INSERT INTO model_rates(model,input_micros_per_million,cached_micros_per_million,output_micros_per_million,profile_json,updated_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(model) DO UPDATE SET input_micros_per_million=excluded.input_micros_per_million,
cached_micros_per_million=excluded.cached_micros_per_million,output_micros_per_million=excluded.output_micros_per_million,
profile_json=excluded.profile_json,updated_at=excluded.updated_at`)
	if err != nil {
		return nil, status, err
	}
	defer statement.Close()
	for _, rate := range normalized {
		profile, err := json.Marshal(rate)
		if err != nil {
			return nil, status, err
		}
		updatedAt := rate.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = time.Now().UTC()
		}
		if _, err := statement.Exec(rate.Model, rate.inputMicrosPerMillion, rate.cacheReadMicrosPerMillion, rate.outputMicrosPerMillion, string(profile), updatedAt.UnixMilli()); err != nil {
			return nil, status, err
		}
	}
	sort.Strings(retired)
	status.RetiredModels = len(retired)
	statusJSON, err := json.Marshal(status)
	if err != nil {
		return nil, status, err
	}
	if _, err := tx.Exec(`INSERT INTO plugin_metadata(name,value,updated_at) VALUES(?,?,?)
ON CONFLICT(name) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, modelRateSyncMetadata, string(statusJSON), time.Now().UTC().UnixMilli()); err != nil {
		return nil, status, err
	}
	if err := tx.Commit(); err != nil {
		return nil, status, err
	}
	return retired, status, nil
}
