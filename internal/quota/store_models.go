package quota

import (
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
	rows, err := store.db.Query(`SELECT model,input_micros_per_million,cached_micros_per_million,output_micros_per_million,updated_at FROM model_rates ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ModelRate, 0)
	for rows.Next() {
		var model string
		var input, cached, output, updated int64
		if err := rows.Scan(&model, &input, &cached, &output, &updated); err != nil {
			return nil, err
		}
		items = append(items, rateFromStored(model, input, cached, output, time.UnixMilli(updated).UTC()))
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
	statement, err := tx.Prepare(`INSERT INTO model_rates(model,input_micros_per_million,cached_micros_per_million,output_micros_per_million,updated_at) VALUES(?,?,?,?,?)`)
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
		if _, err := statement.Exec(normalized.Model, normalized.inputMicrosPerMillion, normalized.cachedMicrosPerMillion, normalized.outputMicrosPerMillion, now); err != nil {
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
	statement, err := tx.Prepare(`INSERT INTO model_rates(model,input_micros_per_million,cached_micros_per_million,output_micros_per_million,updated_at) VALUES(?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	now := time.Now().UTC().UnixMilli()
	for _, rate := range normalized {
		if strings.TrimSpace(rate.Model) == "" {
			continue
		}
		if _, err := statement.Exec(rate.Model, rate.inputMicrosPerMillion, rate.cachedMicrosPerMillion, rate.outputMicrosPerMillion, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
