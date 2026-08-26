package quota

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	usageAnalysisBucketWindow       = 5 * time.Minute
	usageAnalysisRetention          = 366 * 24 * time.Hour
	usageAnalysisWriterFallbackTime = 250 * time.Millisecond
	analysisReaderRetryInterval     = time.Minute
	installationSettingsName        = "installation_settings_v1"
)

var openUsageAnalysisReader = sql.Open

// Store contains only the current Key dollar-meter schema.
type Store struct {
	db                     *sql.DB
	databasePath           string
	analysisDB             *sql.DB
	analysisReaderDSN      string
	analysisReaderDegraded bool
	analysisReaderRetryAt  time.Time
	mu                     sync.Mutex
	analysisMu             sync.RWMutex
	lock                   *databaseLock
}

func OpenStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" {
		return nil, fmt.Errorf("quota database must use a persistent filesystem path")
	}
	canonical, err := canonicalDatabasePath(path)
	if err != nil {
		return nil, err
	}
	lock, err := acquireDatabaseLock(canonical)
	if err != nil {
		return nil, err
	}
	dsn := canonical + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		_ = lock.release()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, databasePath: canonical, lock: lock}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		_ = lock.release()
		return nil, err
	}
	if err := secureDatabaseFiles(canonical); err != nil {
		_ = db.Close()
		_ = lock.release()
		return nil, err
	}
	analysisURI := (&url.URL{Scheme: "file", Path: canonical, RawQuery: "mode=ro&_busy_timeout=5000"}).String()
	store.analysisReaderDSN = analysisURI
	analysisDB, readerErr := openAnalysisReader(analysisURI, usageAnalysisQueryTimeout)
	if readerErr != nil {
		if analysisDB != nil {
			_ = analysisDB.Close()
		}
		store.analysisReaderDegraded = true
	} else {
		store.analysisDB = analysisDB
	}
	return store, nil
}

func openAnalysisReader(dsn string, timeout time.Duration) (*sql.DB, error) {
	db, err := openUsageAnalysisReader("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("open sqlite analysis reader returned no database")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (store *Store) RestoreAnalysisReader() bool {
	if store == nil {
		return false
	}
	store.analysisMu.Lock()
	defer store.analysisMu.Unlock()
	if store.analysisDB != nil || !store.analysisReaderDegraded || store.analysisReaderDSN == "" {
		return false
	}
	now := time.Now().UTC()
	if now.Before(store.analysisReaderRetryAt) {
		return false
	}
	store.analysisReaderRetryAt = now.Add(analysisReaderRetryInterval)
	db, err := openAnalysisReader(store.analysisReaderDSN, time.Second)
	if err != nil {
		return false
	}
	store.analysisDB = db
	store.analysisReaderDegraded = false
	return true
}

func canonicalDatabasePath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	parent := filepath.Dir(abs)
	if err := secureDatabaseDirectory(parent); err != nil {
		return "", err
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve database directory: %w", err)
	}
	canonical := filepath.Join(resolvedParent, filepath.Base(abs))
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		return resolved, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve database file: %w", err)
	}
	return canonical, nil
}

func secureDatabaseDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("restrict database directory permissions: %w", err)
	}
	return nil
}

func secureDatabaseFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("restrict database file permissions: %w", err)
		}
	}
	return nil
}

func (store *Store) migrate() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, err := store.db.Exec(`
CREATE TABLE IF NOT EXISTS plugin_metadata (
  name TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS key_policies (
  key_id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  key_sha256 TEXT NOT NULL UNIQUE,
  key_suffix TEXT NOT NULL DEFAULT '',
  five_hour_budget_usd REAL NOT NULL DEFAULT 0,
  seven_day_budget_usd REAL NOT NULL DEFAULT 0,
  allowed_models TEXT NOT NULL DEFAULT '[]',
  access_rules TEXT NOT NULL DEFAULT '[]',
  access_timezone TEXT NOT NULL DEFAULT 'UTC',
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_buckets (
  scope TEXT NOT NULL CHECK(scope IN ('key_actual','key_cost')),
  scope_id TEXT NOT NULL,
  auth_id TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  bucket_at INTEGER NOT NULL,
  units INTEGER NOT NULL DEFAULT 0,
  request_count INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  metered_by TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(scope, scope_id, model, bucket_at)
);
CREATE INDEX IF NOT EXISTS idx_usage_key_time ON usage_buckets(scope_id, bucket_at);
CREATE TABLE IF NOT EXISTS usage_analysis_buckets (
  key_id TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  bucket_at INTEGER NOT NULL,
  units INTEGER NOT NULL DEFAULT 0,
  request_count INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  input_cost_micros INTEGER NOT NULL DEFAULT 0,
  cached_cost_micros INTEGER NOT NULL DEFAULT 0,
  output_cost_micros INTEGER NOT NULL DEFAULT 0,
  cost_micros INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(key_id, model, bucket_at)
);
CREATE INDEX IF NOT EXISTS idx_analysis_key_time ON usage_analysis_buckets(key_id, bucket_at);
CREATE TABLE IF NOT EXISTS key_actual_token_totals (
  key_id TEXT PRIMARY KEY,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS key_budget_cycles (
  key_id TEXT PRIMARY KEY,
  five_hour_started_at INTEGER,
  seven_day_started_at INTEGER,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pending_request_markers (
  key_id TEXT NOT NULL,
  auth_id TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  request_content TEXT NOT NULL DEFAULT '',
  requested_at INTEGER NOT NULL,
  managed INTEGER NOT NULL DEFAULT 1,
  rate_input_micros INTEGER NOT NULL DEFAULT 0,
  rate_cached_micros INTEGER NOT NULL DEFAULT 0,
  rate_output_micros INTEGER NOT NULL DEFAULT 0,
  rate_profile_json TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(key_id, auth_id, model, requested_at)
);
CREATE TABLE IF NOT EXISTS request_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key_id TEXT NOT NULL,
  key_suffix TEXT NOT NULL DEFAULT '',
  auth_id TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  request_content TEXT NOT NULL DEFAULT '',
  matched_term TEXT NOT NULL DEFAULT '',
  matched_category TEXT NOT NULL DEFAULT '',
  requested_at INTEGER NOT NULL,
  decision TEXT NOT NULL,
  status_code INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  units INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  input_cost_micros INTEGER NOT NULL DEFAULT 0,
  cached_cost_micros INTEGER NOT NULL DEFAULT 0,
  output_cost_micros INTEGER NOT NULL DEFAULT 0,
  cost_micros INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_request_logs_key_time ON request_logs(key_id, requested_at DESC);
CREATE TABLE IF NOT EXISTS operational_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  occurred_at INTEGER NOT NULL,
  level TEXT NOT NULL,
  event TEXT NOT NULL,
  message TEXT NOT NULL,
  auth_id TEXT NOT NULL DEFAULT '',
  key_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_operational_logs_time ON operational_logs(occurred_at DESC);
CREATE TABLE IF NOT EXISTS model_catalog (
  model_id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  owner TEXT NOT NULL DEFAULT '',
  available INTEGER NOT NULL DEFAULT 1,
  synced_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS model_rates (
  model TEXT PRIMARY KEY,
  input_micros_per_million INTEGER NOT NULL DEFAULT 0,
  cached_micros_per_million INTEGER NOT NULL DEFAULT 0,
  output_micros_per_million INTEGER NOT NULL DEFAULT 0,
  profile_json TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS content_filter_settings (
  singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
  enabled INTEGER NOT NULL DEFAULT 1,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS content_filter_terms (
  term_id TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  normalized_value TEXT NOT NULL,
  category TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_filter_normalized ON content_filter_terms(normalized_value, enabled);
`); err != nil {
		return fmt.Errorf("create current quota schema: %w", err)
	}
	// Additive columns keep an existing installation usable while the complete
	// rate profile moves beyond the former single cached-input price.
	for _, column := range []struct{ table, name, definition string }{
		{"model_rates", "profile_json", "TEXT NOT NULL DEFAULT ''"},
		{"pending_request_markers", "rate_profile_json", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureSQLiteColumn(store.db, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	if err := store.seedContentFilterLocked(); err != nil {
		return err
	}
	return nil
}

func ensureSQLiteColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("inspect %s columns: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func (store *Store) LoadOrCreateInstallationSettings(seed InstallationSettings, legacySecret string) (storedInstallationSettings, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var raw string
	err := store.db.QueryRow(`SELECT value FROM plugin_metadata WHERE name = ?`, installationSettingsName).Scan(&raw)
	if err == nil {
		var settings storedInstallationSettings
		if json.Unmarshal([]byte(raw), &settings) != nil {
			return storedInstallationSettings{}, fmt.Errorf("decode installation settings")
		}
		return normalizeInstallationSettings(settings.InstallationSettings, settings.KeyHMACSecret)
	}
	if err != sql.ErrNoRows {
		return storedInstallationSettings{}, fmt.Errorf("load installation settings: %w", err)
	}
	secret := strings.TrimSpace(legacySecret)
	if secret == "" {
		secret, err = generateInstallationSecret()
		if err != nil {
			return storedInstallationSettings{}, err
		}
	}
	settings, err := normalizeInstallationSettings(seed, secret)
	if err != nil {
		return storedInstallationSettings{}, err
	}
	if err := store.saveInstallationSettingsLocked(settings); err != nil {
		return storedInstallationSettings{}, err
	}
	return settings, nil
}

func generateInstallationSecret() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate installation secret: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

func (store *Store) saveInstallationSettingsLocked(settings storedInstallationSettings) error {
	raw, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode installation settings: %w", err)
	}
	_, err = store.db.Exec(`INSERT INTO plugin_metadata(name, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(name) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, installationSettingsName, string(raw), time.Now().UTC().UnixMilli())
	return err
}

func (store *Store) SaveInstallation(settings InstallationSettings, secret string) (storedInstallationSettings, error) {
	normalized, err := normalizeInstallationSettings(settings, secret)
	if err != nil {
		return storedInstallationSettings{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.saveInstallationSettingsLocked(normalized); err != nil {
		return storedInstallationSettings{}, err
	}
	return normalized, nil
}

func (store *Store) InsertMissingPolicies(policies []KeyPolicy) error {
	for _, policy := range policies {
		if _, err := store.insertPolicy(policy, true); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) UpsertPolicy(policy KeyPolicy) error {
	_, err := store.insertPolicy(policy, false)
	return err
}

func (store *Store) insertPolicy(policy KeyPolicy, ignore bool) (sql.Result, error) {
	validated, err := normalizePolicy(policy)
	if err != nil {
		return nil, err
	}
	models, _ := json.Marshal(validated.AllowedModels)
	rules, _ := json.Marshal(validated.AccessRules)
	now := time.Now().UTC().UnixMilli()
	store.mu.Lock()
	defer store.mu.Unlock()
	if ignore {
		return store.db.Exec(`INSERT OR IGNORE INTO key_policies(key_id,name,key_sha256,key_suffix,five_hour_budget_usd,seven_day_budget_usd,allowed_models,access_rules,access_timezone,enabled,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, validated.ID, validated.Name, validated.KeySHA256, validated.KeySuffix, validated.FiveHourBudgetUSD,
			validated.SevenDayBudgetUSD, string(models), string(rules), validated.AccessTimezone, boolToInt(validated.Enabled), now, now)
	}
	return store.db.Exec(`INSERT INTO key_policies(key_id,name,key_sha256,key_suffix,five_hour_budget_usd,seven_day_budget_usd,allowed_models,access_rules,access_timezone,enabled,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(key_id) DO UPDATE SET name=excluded.name,key_sha256=excluded.key_sha256,key_suffix=excluded.key_suffix,
five_hour_budget_usd=excluded.five_hour_budget_usd,seven_day_budget_usd=excluded.seven_day_budget_usd,allowed_models=excluded.allowed_models,
access_rules=excluded.access_rules,access_timezone=excluded.access_timezone,enabled=excluded.enabled,updated_at=excluded.updated_at`,
		validated.ID, validated.Name, validated.KeySHA256, validated.KeySuffix, validated.FiveHourBudgetUSD, validated.SevenDayBudgetUSD,
		string(models), string(rules), validated.AccessTimezone, boolToInt(validated.Enabled), now, now)
}

func (store *Store) LoadPolicies() ([]KeyPolicy, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT key_id,name,key_sha256,key_suffix,five_hour_budget_usd,seven_day_budget_usd,allowed_models,access_rules,access_timezone,enabled FROM key_policies ORDER BY key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]KeyPolicy, 0)
	for rows.Next() {
		var policy KeyPolicy
		var models, rules string
		var enabled int
		if err := rows.Scan(&policy.ID, &policy.Name, &policy.KeySHA256, &policy.KeySuffix, &policy.FiveHourBudgetUSD,
			&policy.SevenDayBudgetUSD, &models, &rules, &policy.AccessTimezone, &enabled); err != nil {
			return nil, err
		}
		policy.Enabled = enabled != 0
		if err := json.Unmarshal([]byte(models), &policy.AllowedModels); err != nil {
			return nil, fmt.Errorf("decode allowed models for %q: %w", policy.ID, err)
		}
		if err := json.Unmarshal([]byte(rules), &policy.AccessRules); err != nil {
			return nil, fmt.Errorf("decode access rules for %q: %w", policy.ID, err)
		}
		result = append(result, policy)
	}
	return result, rows.Err()
}

func (store *Store) ResetPolicyUsage(keyID string) error { return store.deletePolicyData(keyID, false) }
func (store *Store) DeletePolicy(keyID string) error     { return store.deletePolicyData(keyID, true) }

func (store *Store) deletePolicyData(keyID string, deletePolicy bool) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"usage_buckets", "usage_analysis_buckets", "key_actual_token_totals", "key_budget_cycles", "pending_request_markers"} {
		column := "scope_id"
		if table != "usage_buckets" {
			column = "key_id"
		}
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE `+column+` = ?`, keyID); err != nil {
			return err
		}
	}
	if deletePolicy {
		if _, err := tx.Exec(`DELETE FROM key_policies WHERE key_id = ?`, keyID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (store *Store) Close() error {
	if store == nil {
		return nil
	}
	store.analysisMu.Lock()
	if store.analysisDB != nil {
		_ = store.analysisDB.Close()
		store.analysisDB = nil
	}
	store.analysisMu.Unlock()
	var first error
	if store.db != nil {
		if err := store.db.Close(); err != nil {
			first = err
		}
	}
	if store.lock != nil {
		if err := store.lock.release(); first == nil && err != nil {
			first = err
		}
	}
	return first
}
