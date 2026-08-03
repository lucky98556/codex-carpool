package quota

import (
	"fmt"
	"time"
)

func (store *Store) seedContentFilterLocked() error {
	now := time.Now().UTC().UnixMilli()
	if _, err := store.db.Exec(`INSERT OR IGNORE INTO content_filter_settings(singleton, enabled, updated_at) VALUES (1, 0, ?)`, now); err != nil {
		return fmt.Errorf("seed content-filter settings: %w", err)
	}
	for _, term := range builtinContentFilterTerms {
		if _, err := store.db.Exec(`INSERT OR IGNORE INTO content_filter_terms(term_id, value, normalized_value, category, source, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, term.ID, term.Value, normalizeContentFilterText(term.Value), term.Category, contentFilterSourceBuiltin, boolToInt(term.Enabled), now, now); err != nil {
			return fmt.Errorf("seed content-filter term %q: %w", term.ID, err)
		}
	}
	return nil
}

func (store *Store) LoadContentFilterSettings() (ContentFilterSettings, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var enabled int
	if err := store.db.QueryRow(`SELECT enabled FROM content_filter_settings WHERE singleton = 1`).Scan(&enabled); err != nil {
		return ContentFilterSettings{}, fmt.Errorf("load content-filter settings: %w", err)
	}
	rows, err := store.db.Query(`SELECT term_id, value, category, source, enabled FROM content_filter_terms ORDER BY source, value, term_id`)
	if err != nil {
		return ContentFilterSettings{}, fmt.Errorf("load content-filter terms: %w", err)
	}
	defer rows.Close()
	settings := ContentFilterSettings{Enabled: enabled != 0, Terms: make([]ContentFilterTerm, 0)}
	for rows.Next() {
		var term ContentFilterTerm
		var active int
		if err := rows.Scan(&term.ID, &term.Value, &term.Category, &term.Source, &active); err != nil {
			return ContentFilterSettings{}, fmt.Errorf("scan content-filter term: %w", err)
		}
		term.Enabled = active != 0
		settings.Terms = append(settings.Terms, term)
	}
	if err := rows.Err(); err != nil {
		return ContentFilterSettings{}, fmt.Errorf("iterate content-filter terms: %w", err)
	}
	return normalizeContentFilterSettings(settings)
}

func (store *Store) SaveContentFilterSettings(settings ContentFilterSettings) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin content-filter update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().UnixMilli()
	if _, err := tx.Exec(`UPDATE content_filter_settings SET enabled = ?, updated_at = ? WHERE singleton = 1`, boolToInt(settings.Enabled), now); err != nil {
		return fmt.Errorf("update content-filter setting: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM content_filter_terms`); err != nil {
		return fmt.Errorf("replace content-filter terms: %w", err)
	}
	statement, err := tx.Prepare(`INSERT INTO content_filter_terms(term_id, value, normalized_value, category, source, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare content-filter terms: %w", err)
	}
	defer statement.Close()
	for _, term := range settings.Terms {
		if _, err := statement.Exec(term.ID, term.Value, normalizeContentFilterText(term.Value), term.Category, term.Source, boolToInt(term.Enabled), now, now); err != nil {
			return fmt.Errorf("save content-filter term %q: %w", term.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit content-filter update: %w", err)
	}
	return nil
}
