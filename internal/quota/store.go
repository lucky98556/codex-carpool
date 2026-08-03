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
	legacyCompactionBatchRows = 50_000
	// The request-path ledger remains minute-granular and uses configured
	// retention. This coarser, actual-token-only copy makes calendar analysis
	// useful for a full year without retaining up to 525,600 minute rows per
	// Key. Five minutes still preserves every IANA calendar boundary exactly.
	usageAnalysisBucketWindow = 5 * time.Minute
	usageAnalysisRetention    = 366 * 24 * time.Hour
	// A management chart must never hold the sole settlement connection for a
	// full chart timeout when its optional reader is unavailable.
	usageAnalysisWriterFallbackTimeout = 250 * time.Millisecond
	analysisReaderRetryInterval        = time.Minute
)

// openUsageAnalysisReader is kept separate from the writer connection so the
// management-only chart cannot monopolize settlement. The indirection gives
// the Linux-only test suite a way to verify its safe fallback path.
var openUsageAnalysisReader = sql.Open

const (
	installationSettingsMetadataName       = "installation_settings_v1"
	accountGroupsMetadataName              = "account_groups_v1" // legacy migration only
	usageAnalysisBackfillMetadataName      = "usage_analysis_backfill_v1"
	keyActualTotalsBackfillMetadataName    = "key_actual_token_totals_backfill_v1"
	alignedQuotaCalibrationMetadataName    = "official_quota_calibration_v2"
	// officialXLedgerMetadataName marks the one-time transition from the old
	// Token-capacity allocation rows to official-percentage x accounting.
	// Historical Token analytics remain intact; only the incompatible quota
	// guard rows are restarted.
	officialXLedgerMetadataName = "official_x_ledger_v1"
)

type officialXReconciliationState struct {
	AuthID           string
	WindowResetAt    int64
	UsedPercent      float64
	AccountCapacityX float64
	// ObservedAt is the inclusive minute-bucket attribution watermark, not the
	// raw network response timestamp.
	ObservedAt time.Time
}

type officialXCharge struct {
	KeyID         string
	AuthID        string
	WindowResetAt int64
	BucketAt      int64
	Units         int64
	CapacityUnits int64
}

// Store is the durable audit layer. WAL plus a process lease makes the compact
// SQLite deployment safe for one CLIProxyAPI instance. A second instance using
// the same database fails during startup instead of silently double-admitting.
type Store struct {
	db                     *sql.DB
	analysisDB             *sql.DB
	analysisReaderDSN      string
	analysisReaderDegraded bool
	analysisReaderRetryAt  time.Time
	mu                     sync.Mutex
	analysisMu             sync.RWMutex
	lock                   *databaseLock
}

// OpenStore creates the database, holds its singleton lease, and applies the
// small forward-only schema.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is required")
	}
	if path == ":memory:" {
		return nil, fmt.Errorf("quota database must use a persistent filesystem path, not :memory:")
	}
	canonicalPath, err := canonicalDatabasePath(path)
	if err != nil {
		return nil, err
	}
	path = canonicalPath
	lock, err := acquireDatabaseLock(path)
	if err != nil {
		return nil, err
	}
	dsn := path + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		_ = lock.release()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, lock: lock}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		_ = lock.release()
		return nil, err
	}
	if err := secureDatabaseFiles(path); err != nil {
		_ = db.Close()
		_ = lock.release()
		return nil, err
	}
	// Calendar analysis can read up to one retained year of five-minute
	// buckets. Keep it on an independent read-only WAL connection so a slow
	// management chart does not hold the only writer connection needed by
	// completed-token settlement and operational logs.
	analysisURI := (&url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "mode=ro&_busy_timeout=5000",
	}).String()
	store.analysisReaderDSN = analysisURI
	analysisDB, readerErr := openAnalysisReader(analysisURI, usageAnalysisQueryTimeout)
	if readerErr != nil {
		// Analysis is management-only. Do not turn a read-connection
		// optimization failure into a quota-guard startup failure; callers fall
		// back to the writer connection and the native entry point records it.
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
	analysisDB, err := openUsageAnalysisReader("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	if analysisDB == nil {
		return nil, fmt.Errorf("open sqlite analysis reader returned no database")
	}
	analysisDB.SetMaxOpenConns(1)
	analysisDB.SetMaxIdleConns(1)
	pingCtx, cancel := context.WithTimeout(context.Background(), timeout)
	err = analysisDB.PingContext(pingCtx)
	cancel()
	if err != nil {
		_ = analysisDB.Close()
		return nil, err
	}
	return analysisDB, nil
}

// RestoreAnalysisReader retries the optional WAL read connection at a bounded
// rate. A successful retry keeps annual charts independent from the settlement
// writer again; a failed retry leaves the quota guard untouched.
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
	analysisDB, err := openAnalysisReader(store.analysisReaderDSN, time.Second)
	if err != nil {
		return false
	}
	store.analysisDB = analysisDB
	store.analysisReaderDegraded = false
	return true
}

// canonicalDatabasePath resolves relative paths and parent/file symlinks before
// the lease is acquired. This prevents two spellings of the same SQLite file
// from accidentally using independent .lock files.
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
	if resolvedFile, err := filepath.EvalSymlinks(canonical); err == nil {
		if err := secureDatabaseDirectory(filepath.Dir(resolvedFile)); err != nil {
			return "", err
		}
		return resolvedFile, nil
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

// secureDatabaseFiles keeps the database contents private even when an
// existing bind-mounted directory was created with permissive file modes.
// The directory itself is also restricted in canonicalDatabasePath.
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
	_, err := store.db.Exec(`
CREATE TABLE IF NOT EXISTS key_policies (
  key_id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  key_sha256 TEXT NOT NULL UNIQUE,
  group_id TEXT NOT NULL DEFAULT '',
  five_hour_percent REAL NOT NULL DEFAULT 0,
  seven_day_percent REAL NOT NULL DEFAULT 0,
  max_concurrency INTEGER NOT NULL DEFAULT 1,
  five_hour_multiplier REAL NOT NULL DEFAULT 0,
  seven_day_multiplier REAL NOT NULL DEFAULT 0,
  allowed_models_json TEXT NOT NULL DEFAULT '[]',
	access_rules_json TEXT NOT NULL DEFAULT '[]',
	access_timezone TEXT NOT NULL DEFAULT '',
  fingerprint_scheme TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS plugin_metadata (
  name TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key_id TEXT NOT NULL,
  group_id TEXT NOT NULL,
  auth_id TEXT NOT NULL,
  model TEXT NOT NULL,
  requested_at INTEGER NOT NULL,
  recorded_at INTEGER NOT NULL,
  input_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  reasoning_tokens INTEGER NOT NULL,
  cached_tokens INTEGER NOT NULL,
  units INTEGER NOT NULL,
  metered_by TEXT NOT NULL DEFAULT 'admission_estimate',
  failed INTEGER NOT NULL,
  failure_status INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_buckets (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  group_id TEXT NOT NULL,
  auth_id TEXT NOT NULL,
  bucket_at INTEGER NOT NULL,
  units INTEGER NOT NULL,
  request_count INTEGER NOT NULL,
  metered_by TEXT NOT NULL,
  UNIQUE(scope, scope_id, bucket_at)
);
CREATE INDEX IF NOT EXISTS idx_usage_events_key_recorded ON usage_events(key_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_events_account_recorded ON usage_events(auth_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_events_recorded ON usage_events(recorded_at);
CREATE INDEX IF NOT EXISTS idx_usage_buckets_scope_bucket ON usage_buckets(scope, scope_id, bucket_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_buckets_bucket ON usage_buckets(bucket_at);
CREATE TABLE IF NOT EXISTS usage_analysis_buckets (
  key_id TEXT NOT NULL,
  bucket_at INTEGER NOT NULL,
  units INTEGER NOT NULL,
  request_count INTEGER NOT NULL,
  PRIMARY KEY(key_id, bucket_at)
);
CREATE INDEX IF NOT EXISTS idx_usage_analysis_buckets_bucket ON usage_analysis_buckets(bucket_at);
CREATE TABLE IF NOT EXISTS key_actual_token_totals (
  key_id TEXT PRIMARY KEY,
  units INTEGER NOT NULL,
  request_count INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS request_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key_id TEXT NOT NULL,
  auth_id TEXT NOT NULL,
  model TEXT NOT NULL,
  request_content TEXT NOT NULL DEFAULT '',
  requested_at INTEGER NOT NULL,
  decision TEXT NOT NULL,
  status_code INTEGER NOT NULL,
  reason TEXT NOT NULL,
  units INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_request_logs_key_requested ON request_logs(key_id, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_requested ON request_logs(requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_decision_requested ON request_logs(decision, requested_at DESC);
CREATE TABLE IF NOT EXISTS operational_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  occurred_at INTEGER NOT NULL,
  level TEXT NOT NULL,
  event TEXT NOT NULL,
  message TEXT NOT NULL,
  auth_id TEXT NOT NULL DEFAULT '',
  key_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_operational_logs_occurred ON operational_logs(occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_operational_logs_level_occurred ON operational_logs(level, occurred_at DESC);
CREATE TABLE IF NOT EXISTS model_catalog (
  model_id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  owner TEXT NOT NULL,
  available INTEGER NOT NULL,
  synced_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS account_pool_entries (
  auth_id TEXT PRIMARY KEY,
  auth_index TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
  capacity_x REAL NOT NULL,
  enabled INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS official_quota_snapshots (
  auth_id TEXT PRIMARY KEY,
  plan_type TEXT NOT NULL DEFAULT '',
  allowed INTEGER NOT NULL,
  limit_reached INTEGER NOT NULL,
  primary_used_percent REAL NOT NULL,
  primary_window_seconds INTEGER NOT NULL,
  primary_reset_at INTEGER NOT NULL DEFAULT 0,
	primary_reset_estimated INTEGER NOT NULL DEFAULT 0,
	primary_baseline_at INTEGER NOT NULL DEFAULT 0,
  secondary_used_percent REAL NOT NULL,
  secondary_window_seconds INTEGER NOT NULL,
  secondary_reset_at INTEGER NOT NULL DEFAULT 0,
	secondary_reset_estimated INTEGER NOT NULL DEFAULT 0,
	secondary_baseline_at INTEGER NOT NULL DEFAULT 0,
	secondary_estimated_reset_candidate_at INTEGER NOT NULL DEFAULT 0,
	secondary_estimated_reset_candidate_seen_at INTEGER NOT NULL DEFAULT 0,
  observed_at INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT ''
);
-- The estimate is derived only from plugin-owned completed account buckets
-- and matching official weekly percentage deltas. It is independent from
-- Key policy rows, so deleting a policy never loses an account calibration.
CREATE TABLE IF NOT EXISTS official_quota_calibrations (
  auth_id TEXT PRIMARY KEY,
  tokens_per_x INTEGER NOT NULL,
  samples INTEGER NOT NULL DEFAULT 0,
  account_capacity_x REAL NOT NULL DEFAULT 0,
  window_reset_at INTEGER NOT NULL DEFAULT 0,
  observed_at INTEGER NOT NULL DEFAULT 0
);
-- Official weekly percentage changes are the quota authority. This watermark
-- makes each account observation idempotent across refreshes and restarts.
CREATE TABLE IF NOT EXISTS official_x_reconciliation_state (
  auth_id TEXT PRIMARY KEY,
  window_reset_at INTEGER NOT NULL,
  used_percent REAL NOT NULL,
  account_capacity_x REAL NOT NULL,
  observed_at INTEGER NOT NULL
);
-- A one-unit fixed-point x correlation marker is written before CPA dispatches
-- a managed request. After trustworthy calibration, the terminal callback may
-- replace that marker with a bounded provisional x charge; a later measurable
-- official percentage change atomically replaces it with confirmed x.
-- Raw Token analytics remain separate in usage_buckets.
CREATE TABLE IF NOT EXISTS key_account_allocation_buckets (
  key_id TEXT NOT NULL,
  auth_id TEXT NOT NULL,
  window_reset_at INTEGER NOT NULL,
  bucket_at INTEGER NOT NULL,
  completed_units INTEGER NOT NULL DEFAULT 0,
  provisional_units INTEGER NOT NULL DEFAULT 0,
  reserved_units INTEGER NOT NULL DEFAULT 0,
	capacity_units INTEGER NOT NULL DEFAULT 0,
	global_capacity_units INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(key_id, auth_id, window_reset_at, bucket_at)
);
CREATE INDEX IF NOT EXISTS idx_key_account_allocation_window
  ON key_account_allocation_buckets(key_id, auth_id, window_reset_at);
`)
	if err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	if err := store.ensureUsageMeteringColumn(); err != nil {
		return err
	}
	if err := store.ensurePolicyColumns(); err != nil {
		return err
	}
	if err := store.ensureRequestLogColumns(); err != nil {
		return err
	}
	if err := store.ensureAccountPoolColumns(); err != nil {
		return err
	}
	if err := store.ensureOfficialQuotaSnapshotColumns(); err != nil {
		return err
	}
	if err := store.ensureAllocationBucketColumns(); err != nil {
		return err
	}
	if err := store.ensureOfficialXLedger(); err != nil {
		return err
	}
	if err := store.ensureAlignedQuotaCalibrations(); err != nil {
		return err
	}
	// Percentage rows copied from codex-quota-guard cannot be safely translated
	// to shared-pool allocations. Keep them visible but disabled, which preserves the
	// new invariant that missing configuration never restricts a CPA Key.
	_, err = store.db.Exec(`UPDATE key_policies SET enabled = 0
	WHERE allocation_x <= 0
	  AND (five_hour_multiplier <= 0 OR seven_day_multiplier <= 0)`)
	if err != nil {
		return fmt.Errorf("disable unmigrated legacy policies: %w", err)
	}
	if err := store.backfillUsageAnalysisBuckets(); err != nil {
		return err
	}
	if err := store.backfillKeyActualTokenTotals(); err != nil {
		return err
	}
	return nil
}

// ensureAlignedQuotaCalibrations removes estimates learned by releases that
// paired an official percentage movement only with the immediately preceding
// poll. When the rounded percentage stayed unchanged for one or more polls,
// that interval omitted completed Tokens and made one x appear much smaller
// than the matching official window. Existing provisional x was derived from
// the same invalid scale, so it is cleared with the reproducible calibration
// cache. Confirmed official x, pending reservations, and raw analytics remain.
func (store *Store) ensureAlignedQuotaCalibrations() error {
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin aligned quota calibration migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var marker string
	err = tx.QueryRow(`SELECT value FROM plugin_metadata WHERE name = ?`, alignedQuotaCalibrationMetadataName).Scan(&marker)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("read aligned quota calibration marker: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM official_quota_calibrations`); err != nil {
		return fmt.Errorf("clear misaligned quota calibrations: %w", err)
	}
	updatedAt := time.Now().UTC().UnixMilli()
	if _, err := tx.Exec(`UPDATE key_account_allocation_buckets
SET provisional_units = 0, updated_at = ?
WHERE provisional_units <> 0`, updatedAt); err != nil {
		return fmt.Errorf("clear provisional x from misaligned calibrations: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM key_account_allocation_buckets
WHERE completed_units = 0 AND provisional_units = 0 AND reserved_units = 0`); err != nil {
		return fmt.Errorf("cleanup empty misaligned provisional x buckets: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO plugin_metadata(name, value) VALUES (?, '1')`, alignedQuotaCalibrationMetadataName); err != nil {
		return fmt.Errorf("save aligned quota calibration marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit aligned quota calibration migration: %w", err)
	}
	return nil
}

// ensureOfficialXLedger performs the initial allocation-ledger migration. Old
// allocation rows contain completed Token counts and cannot be reinterpreted
// as fixed-point x units. Per-Key usage analysis, request logs, account
// snapshots and calibrations are deliberately preserved.
func (store *Store) ensureOfficialXLedger() error {
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin official x ledger migration: %w", err)
	}
	defer tx.Rollback()
	var marker string
	err = tx.QueryRow(`SELECT value FROM plugin_metadata WHERE name = ?`, officialXLedgerMetadataName).Scan(&marker)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("read official x ledger marker: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM key_account_allocation_buckets`); err != nil {
		return fmt.Errorf("clear incompatible Token allocation ledger: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM official_x_reconciliation_state`); err != nil {
		return fmt.Errorf("clear official x reconciliation state: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO plugin_metadata(name, value) VALUES (?, '1')`, officialXLedgerMetadataName); err != nil {
		return fmt.Errorf("save official x ledger marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit official x ledger migration: %w", err)
	}
	return nil
}

// backfillUsageAnalysisBuckets imports at most one retained year of durable
// pre-upgrade actual usage. A migration marker makes the potentially expensive
// GROUP BY a one-time operation rather than adding startup cost on each CPA
// plugin reload. New rows are mirrored transactionally at write time.
func (store *Store) backfillUsageAnalysisBuckets() error {
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin usage analysis backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var marker string
	err = tx.QueryRow(`SELECT value FROM plugin_metadata WHERE name = ?`, usageAnalysisBackfillMetadataName).Scan(&marker)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("read usage analysis backfill marker: %w", err)
	}
	bucketMillis := usageAnalysisBucketWindow.Milliseconds()
	cutoff := time.Now().UTC().Add(-usageAnalysisRetention).UnixMilli()
	if _, err := tx.Exec(`
INSERT OR REPLACE INTO usage_analysis_buckets(key_id, bucket_at, units, request_count)
SELECT scope_id, ((bucket_at - 1) / ?) * ?, SUM(units), SUM(request_count)
FROM usage_buckets
WHERE scope = 'key_actual' AND bucket_at >= ?
GROUP BY scope_id, ((bucket_at - 1) / ?) * ?`, bucketMillis, bucketMillis, cutoff, bucketMillis, bucketMillis); err != nil {
		return fmt.Errorf("backfill usage analysis buckets: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO plugin_metadata(name, value) VALUES (?, '1')`, usageAnalysisBackfillMetadataName); err != nil {
		return fmt.Errorf("save usage analysis backfill marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage analysis backfill: %w", err)
	}
	return nil
}

// backfillKeyActualTokenTotals seeds the lifetime counter once from the
// retained analytics ledger. Every later completed usage write increments the
// counter transactionally, so retention cleanup cannot make the total shrink.
func (store *Store) backfillKeyActualTokenTotals() error {
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin Key actual Token totals backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var marker string
	err = tx.QueryRow(`SELECT value FROM plugin_metadata WHERE name = ?`, keyActualTotalsBackfillMetadataName).Scan(&marker)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("read Key actual Token totals backfill marker: %w", err)
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO key_actual_token_totals(key_id, units, request_count)
SELECT key_id, SUM(units), SUM(request_count)
FROM usage_analysis_buckets
GROUP BY key_id`); err != nil {
		return fmt.Errorf("backfill Key actual Token totals: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO plugin_metadata(name, value) VALUES (?, '1')`, keyActualTotalsBackfillMetadataName); err != nil {
		return fmt.Errorf("save Key actual Token totals backfill marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Key actual Token totals backfill: %w", err)
	}
	return nil
}

func (store *Store) ensurePolicyColumns() error {
	for _, column := range []struct {
		name string
		kind string
	}{
		{"five_hour_multiplier", "REAL NOT NULL DEFAULT 0"},
		{"seven_day_multiplier", "REAL NOT NULL DEFAULT 0"},
		{"allowed_models_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"access_rules_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"access_timezone", "TEXT NOT NULL DEFAULT ''"},
		{"fingerprint_scheme", "TEXT NOT NULL DEFAULT ''"},
		{"allocation_x", "REAL NOT NULL DEFAULT 0"},
	} {
		exists, err := store.columnExists("key_policies", column.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := store.db.Exec(`ALTER TABLE key_policies ADD COLUMN ` + column.name + ` ` + column.kind); err != nil {
			return fmt.Errorf("add key policy column %s: %w", column.name, err)
		}
	}
	// Existing direct-multiplier policies remain visible after the upgrade. The
	// larger historic limit is the only non-destructive single-x equivalent;
	// operators can edit it explicitly from the new panel afterwards.
	if _, err := store.db.Exec(`UPDATE key_policies
SET allocation_x = CASE
  WHEN allocation_x > 0 THEN allocation_x
  WHEN five_hour_multiplier > seven_day_multiplier THEN five_hour_multiplier
  ELSE seven_day_multiplier
END`); err != nil {
		return fmt.Errorf("migrate key allocation_x: %w", err)
	}
	return nil
}

// ensureRequestLogColumns keeps existing plugin databases compatible while
// allowing new releases to persist a bounded user-text excerpt per decision.
func (store *Store) ensureRequestLogColumns() error {
	exists, err := store.columnExists("request_logs", "request_content")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := store.db.Exec(`ALTER TABLE request_logs ADD COLUMN request_content TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add request log content column: %w", err)
	}
	return nil
}

func (store *Store) ensureAccountPoolColumns() error {
	for _, column := range []struct {
		name string
		kind string
	}{
		{"auth_index", "TEXT NOT NULL DEFAULT ''"},
		{"name", "TEXT NOT NULL DEFAULT ''"},
		{"capacity_x", "REAL NOT NULL DEFAULT 0"},
		{"enabled", "INTEGER NOT NULL DEFAULT 1"},
		{"updated_at", "INTEGER NOT NULL DEFAULT 0"},
	} {
		exists, err := store.columnExists("account_pool_entries", column.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := store.db.Exec(`ALTER TABLE account_pool_entries ADD COLUMN ` + column.name + ` ` + column.kind); err != nil {
			return fmt.Errorf("add account pool column %s: %w", column.name, err)
		}
	}
	return nil
}

// ensureAllocationBucketColumns keeps allocation ledger upgrades additive.
// Existing databases gain a zeroed provisional ledger without losing their
// confirmed usage. The global capacity marker distinguishes new Key-wide rows
// from old account-proportional rows, which remain conservatively recoverable.
func (store *Store) ensureAllocationBucketColumns() error {
	for _, column := range []struct {
		name string
		kind string
	}{
		{"provisional_units", "INTEGER NOT NULL DEFAULT 0"},
		{"capacity_units", "INTEGER NOT NULL DEFAULT 0"},
		{"global_capacity_units", "INTEGER NOT NULL DEFAULT 0"},
	} {
		exists, err := store.columnExists("key_account_allocation_buckets", column.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := store.db.Exec(`ALTER TABLE key_account_allocation_buckets ADD COLUMN ` + column.name + ` ` + column.kind); err != nil {
			return fmt.Errorf("add allocation bucket %s column: %w", column.name, err)
		}
	}
	return nil
}

// ensureOfficialQuotaSnapshotColumns keeps older plugin databases compatible
// while preserving the stable per-window baseline needed for conservative
// local accounting across refreshes and process restarts.
func (store *Store) ensureOfficialQuotaSnapshotColumns() error {
	for _, column := range []struct {
		name string
		kind string
	}{
		{"primary_reset_estimated", "INTEGER NOT NULL DEFAULT 0"},
		{"primary_baseline_at", "INTEGER NOT NULL DEFAULT 0"},
		{"secondary_reset_estimated", "INTEGER NOT NULL DEFAULT 0"},
		{"secondary_baseline_at", "INTEGER NOT NULL DEFAULT 0"},
		{"secondary_estimated_reset_candidate_at", "INTEGER NOT NULL DEFAULT 0"},
		{"secondary_estimated_reset_candidate_seen_at", "INTEGER NOT NULL DEFAULT 0"},
	} {
		exists, err := store.columnExists("official_quota_snapshots", column.name)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := store.db.Exec(`ALTER TABLE official_quota_snapshots ADD COLUMN ` + column.name + ` ` + column.kind); err != nil {
				return fmt.Errorf("add official quota snapshot column %s: %w", column.name, err)
			}
		}
	}
	return nil
}

func (store *Store) columnExists(table, expected string) (bool, error) {
	rows, err := store.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, fmt.Errorf("scan %s column: %w", table, err)
		}
		if name == expected {
			return true, nil
		}
	}
	return false, rows.Err()
}

// EnsureFingerprintScheme preserves legacy rows without preventing the plugin
// from starting. An old SHA-256 fingerprint cannot be safely converted without
// the raw downstream Key, so copied policies are paused and explicitly marked
// for a CPA rebind instead of being guessed or silently activated.
func (store *Store) EnsureFingerprintScheme() error {
	const schemeName = "key_fingerprint_scheme"
	const legacyScheme = "legacy-sha256-v1"
	store.mu.Lock()
	defer store.mu.Unlock()
	var stored string
	err := store.db.QueryRow(`SELECT value FROM plugin_metadata WHERE name = ?`, schemeName).Scan(&stored)
	if err == sql.ErrNoRows {
		if _, err := store.db.Exec(`UPDATE key_policies
SET enabled = 0, fingerprint_scheme = ?
WHERE fingerprint_scheme = ''`, legacyScheme); err != nil {
			return fmt.Errorf("pause legacy key policies: %w", err)
		}
		if _, err := store.db.Exec(`INSERT INTO plugin_metadata(name, value) VALUES (?, ?)`, schemeName, hmacFingerprintScheme); err != nil {
			return fmt.Errorf("record key fingerprint scheme: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read key fingerprint scheme: %w", err)
	}
	if stored != hmacFingerprintScheme {
		return fmt.Errorf("unsupported key fingerprint scheme %q", stored)
	}
	// v1.0 already stored HMAC fingerprints globally but did not persist the
	// per-policy marker. Backfill those rows only when the installation marker
	// proves that they are HMAC values.
	if _, err := store.db.Exec(`UPDATE key_policies SET fingerprint_scheme = ? WHERE fingerprint_scheme = ''`, hmacFingerprintScheme); err != nil {
		return fmt.Errorf("backfill key fingerprint scheme: %w", err)
	}
	return nil
}

// LoadOrCreateInstallationSettings keeps all operator-controlled plugin
// settings in the plugin database. The generated HMAC secret never needs to
// be copied into CLIProxyAPI's config file or exposed to the browser.
func (store *Store) LoadOrCreateInstallationSettings(seed InstallationSettings, legacySecret string) (storedInstallationSettings, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var encoded string
	err := store.db.QueryRow(`SELECT value FROM plugin_metadata WHERE name = ?`, installationSettingsMetadataName).Scan(&encoded)
	if err == nil {
		var stored storedInstallationSettings
		if err := json.Unmarshal([]byte(encoded), &stored); err != nil {
			return storedInstallationSettings{}, fmt.Errorf("decode installation settings: %w", err)
		}
		normalized, err := normalizeInstallationSettings(stored.InstallationSettings, stored.KeyHMACSecret)
		if err != nil {
			return storedInstallationSettings{}, fmt.Errorf("validate installation settings: %w", err)
		}
		return normalized, nil
	}
	if err != sql.ErrNoRows {
		return storedInstallationSettings{}, fmt.Errorf("read installation settings: %w", err)
	}
	secret := strings.TrimSpace(legacySecret)
	if secret == "" {
		generated, err := generateInstallationSecret()
		if err != nil {
			return storedInstallationSettings{}, err
		}
		secret = generated
	}
	normalized, err := normalizeInstallationSettings(seed, secret)
	if err != nil {
		return storedInstallationSettings{}, fmt.Errorf("validate initial installation settings: %w", err)
	}
	if err := store.saveInstallationSettingsLocked(normalized); err != nil {
		return storedInstallationSettings{}, err
	}
	return normalized, nil
}

func generateInstallationSecret() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate installation HMAC secret: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

func (store *Store) saveInstallationSettingsLocked(settings storedInstallationSettings) error {
	raw, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode installation settings: %w", err)
	}
	if _, err := store.db.Exec(`INSERT INTO plugin_metadata(name, value) VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET value = excluded.value`, installationSettingsMetadataName, string(raw)); err != nil {
		return fmt.Errorf("save installation settings: %w", err)
	}
	return nil
}

// LoadAccountGroups returns only the quota metadata selected in the wizard;
// it never reads CLIProxyAPI auth files or OAuth secrets.
func (store *Store) LoadAccountGroups() ([]AccountGroup, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var encoded string
	err := store.db.QueryRow(`SELECT value FROM plugin_metadata WHERE name = ?`, accountGroupsMetadataName).Scan(&encoded)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read account groups: %w", err)
	}
	var groups []AccountGroup
	if err := json.Unmarshal([]byte(encoded), &groups); err != nil {
		return nil, false, fmt.Errorf("decode account groups: %w", err)
	}
	return groups, true, nil
}

func (store *Store) SaveAccountGroups(groups []AccountGroup) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	raw, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("encode account groups: %w", err)
	}
	if _, err := store.db.Exec(`INSERT INTO plugin_metadata(name, value) VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET value = excluded.value`, accountGroupsMetadataName, string(raw)); err != nil {
		return fmt.Errorf("save account groups: %w", err)
	}
	return nil
}

func (store *Store) SaveInstallation(settings InstallationSettings, secret string, groups []AccountGroup) (storedInstallationSettings, error) {
	normalized, err := normalizeInstallationSettings(settings, secret)
	if err != nil {
		return storedInstallationSettings{}, err
	}
	rawSettings, err := json.Marshal(normalized)
	if err != nil {
		return storedInstallationSettings{}, fmt.Errorf("encode installation settings: %w", err)
	}
	rawGroups, err := json.Marshal(groups)
	if err != nil {
		return storedInstallationSettings{}, fmt.Errorf("encode account groups: %w", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return storedInstallationSettings{}, fmt.Errorf("begin installation update: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO plugin_metadata(name, value) VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET value = excluded.value`, installationSettingsMetadataName, string(rawSettings)); err != nil {
		_ = tx.Rollback()
		return storedInstallationSettings{}, fmt.Errorf("save installation settings: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO plugin_metadata(name, value) VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET value = excluded.value`, accountGroupsMetadataName, string(rawGroups)); err != nil {
		_ = tx.Rollback()
		return storedInstallationSettings{}, fmt.Errorf("save account groups: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return storedInstallationSettings{}, fmt.Errorf("commit installation update: %w", err)
	}
	return normalized, nil
}

// CompactLegacyUsageEvents converts the previous per-request table into the
// bounded key/account bucket model once. Each transaction processes a limited
// row range so a large historical database cannot create one giant SQLite
// write transaction during startup. The marker makes later restarts load only
// bucket rows.
func (store *Store) CompactLegacyUsageEvents() error {
	const formatName = "usage_storage_format"
	const formatValue = "bucket-v1"
	store.mu.Lock()
	defer store.mu.Unlock()
	var stored string
	err := store.db.QueryRow(`SELECT value FROM plugin_metadata WHERE name = ?`, formatName).Scan(&stored)
	if err == nil {
		if stored != formatValue {
			return fmt.Errorf("unsupported usage storage format %q", stored)
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("read usage storage format: %w", err)
	}
	for {
		boundary, found, err := store.legacyCompactionBoundary()
		if err != nil {
			return err
		}
		if !found {
			break
		}
		if err := store.compactLegacyUsageBatch(boundary); err != nil {
			return err
		}
	}
	if _, err := store.db.Exec(`INSERT INTO plugin_metadata(name, value) VALUES (?, ?)`, formatName, formatValue); err != nil {
		return fmt.Errorf("record usage storage format: %w", err)
	}
	return nil
}

func (store *Store) legacyCompactionBoundary() (int64, bool, error) {
	var boundary int64
	err := store.db.QueryRow(`SELECT id FROM usage_events ORDER BY id LIMIT 1 OFFSET ?`, legacyCompactionBatchRows-1).Scan(&boundary)
	if err == nil {
		return boundary, true, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, fmt.Errorf("find legacy usage batch boundary: %w", err)
	}
	var last sql.NullInt64
	if err := store.db.QueryRow(`SELECT MAX(id) FROM usage_events`).Scan(&last); err != nil {
		return 0, false, fmt.Errorf("find final legacy usage batch boundary: %w", err)
	}
	if !last.Valid {
		return 0, false, nil
	}
	return last.Int64, true, nil
}

func (store *Store) compactLegacyUsageBatch(boundary int64) error {
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin legacy usage compaction: %w", err)
	}
	bucketMillis := usageBucketWindow.Milliseconds()
	if _, err := tx.Exec(`
INSERT INTO usage_buckets(scope, scope_id, group_id, auth_id, bucket_at, units, request_count, metered_by)
SELECT 'key', key_id, group_id, 'mixed', ((recorded_at + ? - 1) / ?) * ? AS bucket_at, SUM(units), COUNT(*), 'admission_estimate_legacy_bucket'
FROM usage_events WHERE id <= ?
GROUP BY key_id, group_id, bucket_at
ON CONFLICT(scope, scope_id, bucket_at) DO UPDATE SET
  group_id = excluded.group_id,
  auth_id = 'mixed',
  units = usage_buckets.units + excluded.units,
  request_count = usage_buckets.request_count + excluded.request_count,
		metered_by = excluded.metered_by`, bucketMillis, bucketMillis, bucketMillis, boundary); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("compact legacy Key usage: %w", err)
	}
	if _, err := tx.Exec(`
INSERT INTO usage_buckets(scope, scope_id, group_id, auth_id, bucket_at, units, request_count, metered_by)
SELECT 'account', auth_id, group_id, auth_id, ((recorded_at + ? - 1) / ?) * ? AS bucket_at, SUM(units), COUNT(*), 'admission_estimate_legacy_bucket'
FROM usage_events
WHERE id <= ? AND auth_id <> ''
GROUP BY auth_id, group_id, bucket_at
ON CONFLICT(scope, scope_id, bucket_at) DO UPDATE SET
  group_id = excluded.group_id,
  auth_id = usage_buckets.auth_id,
  units = usage_buckets.units + excluded.units,
  request_count = usage_buckets.request_count + excluded.request_count,
		metered_by = excluded.metered_by`, bucketMillis, bucketMillis, bucketMillis, boundary); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("compact legacy account usage: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM usage_events WHERE id <= ?`, boundary); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete compacted legacy usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy usage compaction: %w", err)
	}
	return nil
}

// ensureUsageMeteringColumn keeps the old table readable until the explicit
// transactional compaction step converts its rows into aggregate buckets.
func (store *Store) ensureUsageMeteringColumn() error {
	rows, err := store.db.Query(`PRAGMA table_info(usage_events)`)
	if err != nil {
		return fmt.Errorf("inspect usage event schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan usage event schema: %w", err)
		}
		if name == "metered_by" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate usage event schema: %w", err)
	}
	if _, err := store.db.Exec(`ALTER TABLE usage_events ADD COLUMN metered_by TEXT NOT NULL DEFAULT 'legacy'`); err != nil {
		return fmt.Errorf("add usage metering column: %w", err)
	}
	return nil
}

// InsertMissingPolicies seeds source-controlled bootstrap policies. Existing
// SQLite policies remain authoritative so panel edits are not overwritten.
func (store *Store) InsertMissingPolicies(policies []KeyPolicy) error {
	if len(policies) == 0 {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin bootstrap policy transaction: %w", err)
	}
	statement, err := tx.Prepare(`
INSERT INTO key_policies (
  key_id, name, key_sha256, group_id, five_hour_percent, seven_day_percent,
  max_concurrency, five_hour_multiplier, seven_day_multiplier, allowed_models_json,
	  access_rules_json, access_timezone, fingerprint_scheme, allocation_x, enabled, created_at, updated_at
) VALUES (?, ?, ?, '', 0, 0, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(key_id) DO NOTHING`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare bootstrap policy insert: %w", err)
	}
	defer statement.Close()
	now := time.Now().UTC().UnixMilli()
	for _, policy := range policies {
		models, err := json.Marshal(policy.AllowedModels)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("encode allowed models for %q: %w", policy.ID, err)
		}
		rules, err := json.Marshal(policy.AccessRules)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("encode access rules for %q: %w", policy.ID, err)
		}
		if _, err := statement.Exec(policy.ID, policy.Name, policy.KeySHA256, policy.FiveHourMultiplier, policy.SevenDayMultiplier, string(models), string(rules), policy.AccessTimezone, policy.FingerprintScheme, policy.AllocationX, boolToInt(policy.Enabled), now, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert bootstrap policy %q: %w", policy.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit bootstrap policies: %w", err)
	}
	return nil
}

// UpsertPolicy creates or replaces one policy through the authenticated panel.
func (store *Store) UpsertPolicy(policy KeyPolicy) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := time.Now().UTC().UnixMilli()
	models, err := json.Marshal(policy.AllowedModels)
	if err != nil {
		return fmt.Errorf("encode allowed models: %w", err)
	}
	rules, err := json.Marshal(policy.AccessRules)
	if err != nil {
		return fmt.Errorf("encode access rules: %w", err)
	}
	_, err = store.db.Exec(`
INSERT INTO key_policies (
  key_id, name, key_sha256, group_id, five_hour_percent, seven_day_percent,
  max_concurrency, five_hour_multiplier, seven_day_multiplier, allowed_models_json,
	  access_rules_json, access_timezone, fingerprint_scheme, allocation_x, enabled, created_at, updated_at
) VALUES (?, ?, ?, '', 0, 0, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(key_id) DO UPDATE SET
  name = excluded.name,
  key_sha256 = excluded.key_sha256,
  five_hour_multiplier = excluded.five_hour_multiplier,
  seven_day_multiplier = excluded.seven_day_multiplier,
  allowed_models_json = excluded.allowed_models_json,
	  access_rules_json = excluded.access_rules_json,
	  access_timezone = excluded.access_timezone,
  fingerprint_scheme = excluded.fingerprint_scheme,
  allocation_x = excluded.allocation_x,
  enabled = excluded.enabled,
  updated_at = excluded.updated_at`,
		policy.ID, policy.Name, policy.KeySHA256, policy.FiveHourMultiplier, policy.SevenDayMultiplier,
		string(models), string(rules), policy.AccessTimezone, policy.FingerprintScheme, policy.AllocationX, boolToInt(policy.Enabled), now, now,
	)
	if err != nil {
		return fmt.Errorf("upsert key policy: %w", err)
	}
	return nil
}

// LoadPolicies returns all durable API key policies without exposing raw keys.
func (store *Store) LoadPolicies() ([]KeyPolicy, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`
SELECT key_id, name, key_sha256, group_id, five_hour_percent, seven_day_percent, max_concurrency,
       five_hour_multiplier, seven_day_multiplier, allowed_models_json, access_rules_json, access_timezone, fingerprint_scheme, allocation_x, enabled
FROM key_policies ORDER BY name, key_id`)
	if err != nil {
		return nil, fmt.Errorf("query key policies: %w", err)
	}
	defer rows.Close()
	policies := make([]KeyPolicy, 0)
	for rows.Next() {
		var policy KeyPolicy
		var enabled int
		var models, accessRules string
		if err := rows.Scan(&policy.ID, &policy.Name, &policy.KeySHA256, &policy.GroupID, &policy.FiveHourPercent, &policy.SevenDayPercent, &policy.MaxConcurrency, &policy.FiveHourMultiplier, &policy.SevenDayMultiplier, &models, &accessRules, &policy.AccessTimezone, &policy.FingerprintScheme, &policy.AllocationX, &enabled); err != nil {
			return nil, fmt.Errorf("scan key policy: %w", err)
		}
		if err := json.Unmarshal([]byte(models), &policy.AllowedModels); err != nil {
			return nil, fmt.Errorf("decode allowed models for %q: %w", policy.ID, err)
		}
		if err := json.Unmarshal([]byte(accessRules), &policy.AccessRules); err != nil {
			return nil, fmt.Errorf("decode access rules for %q: %w", policy.ID, err)
		}
		policy.Enabled = enabled != 0
		policies = append(policies, policy)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate key policies: %w", err)
	}
	return policies, nil
}

func (store *Store) LoadAccountPool() ([]AccountPoolEntry, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT auth_id, auth_index, name, capacity_x, enabled, updated_at
FROM account_pool_entries ORDER BY name, auth_id`)
	if err != nil {
		return nil, fmt.Errorf("query account pool: %w", err)
	}
	defer rows.Close()
	entries := make([]AccountPoolEntry, 0)
	for rows.Next() {
		var entry AccountPoolEntry
		var enabled int
		var updatedAt int64
		if err := rows.Scan(&entry.AuthID, &entry.AuthIndex, &entry.Name, &entry.CapacityX, &enabled, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan account pool entry: %w", err)
		}
		entry.Enabled = enabled != 0
		entry.UpdatedAt = time.UnixMilli(updatedAt).UTC()
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (store *Store) UpsertAccountPoolEntry(entry AccountPoolEntry) error {
	return store.UpsertAccountPoolEntries([]AccountPoolEntry{entry})
}

// UpsertAccountPoolEntries writes a selection from the account-pool dialog as
// one transaction. A multi-account save must never leave only part of the
// operator's selection persisted when a later row is invalid or SQLite fails.
func (store *Store) UpsertAccountPoolEntries(entries []AccountPoolEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("account pool entries are required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin account pool upsert: %w", err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`INSERT INTO account_pool_entries(auth_id, auth_index, name, capacity_x, enabled, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(auth_id) DO UPDATE SET
  auth_index = excluded.auth_index,
  name = excluded.name,
  capacity_x = excluded.capacity_x,
  enabled = excluded.enabled,
	  updated_at = excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("prepare account pool upsert: %w", err)
	}
	defer statement.Close()
	for _, entry := range entries {
		updatedAt := entry.UpdatedAt.UTC()
		if updatedAt.IsZero() {
			updatedAt = time.Now().UTC()
		}
		if _, err := statement.Exec(entry.AuthID, entry.AuthIndex, entry.Name, entry.CapacityX, boolToInt(entry.Enabled), updatedAt.UnixMilli()); err != nil {
			return fmt.Errorf("upsert account pool entry %q: %w", entry.AuthID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit account pool upsert: %w", err)
	}
	return nil
}

func (store *Store) DeleteAccountPoolEntry(authID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin account pool delete: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM account_pool_entries WHERE auth_id = ?`, authID); err != nil {
		return fmt.Errorf("delete account pool entry: %w", err)
	}
	// Keep the last official snapshot and allocation buckets through their
	// official reset. Removing and later re-adding the same CPA identity must
	// reconstruct the same account-wide ledger instead of freeing the shared
	// official remainder before Codex has actually reset it.
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit account pool delete: %w", err)
	}
	return nil
}

func (store *Store) LoadOfficialQuotaSnapshots() ([]OfficialQuotaSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT auth_id, plan_type, allowed, limit_reached,
	 primary_used_percent, primary_window_seconds, primary_reset_at, primary_reset_estimated, primary_baseline_at,
	 secondary_used_percent, secondary_window_seconds, secondary_reset_at, secondary_reset_estimated, secondary_baseline_at,
	 secondary_estimated_reset_candidate_at, secondary_estimated_reset_candidate_seen_at,
	 observed_at, last_error
FROM official_quota_snapshots`)
	if err != nil {
		return nil, fmt.Errorf("query official quota snapshots: %w", err)
	}
	defer rows.Close()
	items := make([]OfficialQuotaSnapshot, 0)
	for rows.Next() {
		var item OfficialQuotaSnapshot
		var allowed, limitReached int
		var primaryResetAt, primaryBaselineAt, secondaryResetAt, secondaryBaselineAt, secondaryCandidateAt, secondaryCandidateSeenAt, observedAt int64
		var primaryResetEstimated, secondaryResetEstimated int
		if err := rows.Scan(&item.AuthID, &item.PlanType, &allowed, &limitReached,
			&item.Primary.UsedPercent, &item.Primary.LimitWindowSeconds, &primaryResetAt, &primaryResetEstimated, &primaryBaselineAt,
			&item.Secondary.UsedPercent, &item.Secondary.LimitWindowSeconds, &secondaryResetAt, &secondaryResetEstimated, &secondaryBaselineAt,
			&secondaryCandidateAt, &secondaryCandidateSeenAt,
			&observedAt, &item.LastError); err != nil {
			return nil, fmt.Errorf("scan official quota snapshot: %w", err)
		}
		item.Allowed = allowed != 0
		item.LimitReached = limitReached != 0
		if primaryResetAt > 0 {
			value := time.UnixMilli(primaryResetAt).UTC()
			item.Primary.ResetAt = &value
		}
		item.Primary.ResetEstimated = primaryResetEstimated != 0
		if primaryBaselineAt > 0 {
			item.Primary.BaselineAt = time.UnixMilli(primaryBaselineAt).UTC()
		}
		if secondaryResetAt > 0 {
			value := time.UnixMilli(secondaryResetAt).UTC()
			item.Secondary.ResetAt = &value
		}
		item.Secondary.ResetEstimated = secondaryResetEstimated != 0
		if secondaryBaselineAt > 0 {
			item.Secondary.BaselineAt = time.UnixMilli(secondaryBaselineAt).UTC()
		}
		if secondaryCandidateAt > 0 {
			value := time.UnixMilli(secondaryCandidateAt).UTC()
			item.SecondaryEstimatedResetCandidateAt = &value
		}
		if secondaryCandidateSeenAt > 0 {
			value := time.UnixMilli(secondaryCandidateSeenAt).UTC()
			item.SecondaryEstimatedResetCandidateSeenAt = &value
		}
		item.ObservedAt = time.UnixMilli(observedAt).UTC()
		items = append(items, normalizeOfficialQuotaSnapshot(item))
	}
	return items, rows.Err()
}

func (store *Store) UpsertOfficialQuotaSnapshot(snapshot OfficialQuotaSnapshot) error {
	snapshot = normalizeOfficialQuotaSnapshot(snapshot)
	store.mu.Lock()
	defer store.mu.Unlock()
	primaryResetAt, primaryBaselineAt, secondaryResetAt, secondaryBaselineAt := int64(0), int64(0), int64(0), int64(0)
	secondaryCandidateAt, secondaryCandidateSeenAt := int64(0), int64(0)
	if snapshot.Primary.ResetAt != nil {
		primaryResetAt = snapshot.Primary.ResetAt.UnixMilli()
	}
	if snapshot.Secondary.ResetAt != nil {
		secondaryResetAt = snapshot.Secondary.ResetAt.UnixMilli()
	}
	if !snapshot.Primary.BaselineAt.IsZero() {
		primaryBaselineAt = snapshot.Primary.BaselineAt.UnixMilli()
	}
	if !snapshot.Secondary.BaselineAt.IsZero() {
		secondaryBaselineAt = snapshot.Secondary.BaselineAt.UnixMilli()
	}
	if snapshot.SecondaryEstimatedResetCandidateAt != nil {
		secondaryCandidateAt = snapshot.SecondaryEstimatedResetCandidateAt.UnixMilli()
	}
	if snapshot.SecondaryEstimatedResetCandidateSeenAt != nil {
		secondaryCandidateSeenAt = snapshot.SecondaryEstimatedResetCandidateSeenAt.UnixMilli()
	}
	_, err := store.db.Exec(`INSERT INTO official_quota_snapshots(
 auth_id, plan_type, allowed, limit_reached,
 primary_used_percent, primary_window_seconds, primary_reset_at, primary_reset_estimated, primary_baseline_at,
	secondary_used_percent, secondary_window_seconds, secondary_reset_at, secondary_reset_estimated, secondary_baseline_at,
	secondary_estimated_reset_candidate_at, secondary_estimated_reset_candidate_seen_at,
 observed_at, last_error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(auth_id) DO UPDATE SET
 plan_type = excluded.plan_type,
 allowed = excluded.allowed,
 limit_reached = excluded.limit_reached,
 primary_used_percent = excluded.primary_used_percent,
 primary_window_seconds = excluded.primary_window_seconds,
 primary_reset_at = excluded.primary_reset_at,
	primary_reset_estimated = excluded.primary_reset_estimated,
	primary_baseline_at = excluded.primary_baseline_at,
 secondary_used_percent = excluded.secondary_used_percent,
 secondary_window_seconds = excluded.secondary_window_seconds,
	secondary_reset_at = excluded.secondary_reset_at,
	secondary_reset_estimated = excluded.secondary_reset_estimated,
	secondary_baseline_at = excluded.secondary_baseline_at,
	secondary_estimated_reset_candidate_at = excluded.secondary_estimated_reset_candidate_at,
	secondary_estimated_reset_candidate_seen_at = excluded.secondary_estimated_reset_candidate_seen_at,
 observed_at = excluded.observed_at,
 last_error = excluded.last_error`,
		snapshot.AuthID, snapshot.PlanType, boolToInt(snapshot.Allowed), boolToInt(snapshot.LimitReached),
		snapshot.Primary.UsedPercent, snapshot.Primary.LimitWindowSeconds, primaryResetAt, boolToInt(snapshot.Primary.ResetEstimated), primaryBaselineAt,
		snapshot.Secondary.UsedPercent, snapshot.Secondary.LimitWindowSeconds, secondaryResetAt, boolToInt(snapshot.Secondary.ResetEstimated), secondaryBaselineAt,
		secondaryCandidateAt, secondaryCandidateSeenAt,
		snapshot.ObservedAt.UnixMilli(), snapshot.LastError)
	if err != nil {
		return fmt.Errorf("upsert official quota snapshot: %w", err)
	}
	return nil
}

// LoadQuotaCalibrations returns only plugin-derived scale factors. It is read
// once at startup; normal admission reads the Engine's in-memory copy.
func (store *Store) LoadQuotaCalibrations() ([]quotaCalibration, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT auth_id, tokens_per_x, samples, account_capacity_x, window_reset_at, observed_at
FROM official_quota_calibrations`)
	if err != nil {
		return nil, fmt.Errorf("query official quota calibrations: %w", err)
	}
	defer rows.Close()
	items := make([]quotaCalibration, 0)
	for rows.Next() {
		var item quotaCalibration
		var observedAt int64
		if err := rows.Scan(&item.AuthID, &item.TokensPerX, &item.Samples, &item.AccountCapacityX, &item.WindowResetAt, &observedAt); err != nil {
			return nil, fmt.Errorf("scan official quota calibration: %w", err)
		}
		if observedAt > 0 {
			item.ObservedAt = time.UnixMilli(observedAt).UTC()
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate official quota calibrations: %w", err)
	}
	return items, nil
}

// UpsertQuotaCalibration persists the account-local estimate separately from
// Key usage. A failed write leaves the previous in-memory estimate intact and
// never weakens the official percentage/reset guard.
func (store *Store) UpsertQuotaCalibration(calibration quotaCalibration) error {
	if calibration.AuthID == "" || calibration.TokensPerX <= 0 {
		return fmt.Errorf("invalid official quota calibration")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	_, err := store.db.Exec(`INSERT INTO official_quota_calibrations(
  auth_id, tokens_per_x, samples, account_capacity_x, window_reset_at, observed_at
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(auth_id) DO UPDATE SET
  tokens_per_x = excluded.tokens_per_x,
  samples = excluded.samples,
  account_capacity_x = excluded.account_capacity_x,
  window_reset_at = excluded.window_reset_at,
  observed_at = excluded.observed_at`,
		calibration.AuthID, calibration.TokensPerX, calibration.Samples, calibration.AccountCapacityX,
		calibration.WindowResetAt, calibration.ObservedAt.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("upsert official quota calibration: %w", err)
	}
	return nil
}

// CompletedAccountUsageBetween sums the dedicated account aggregates instead
// of joining Key rows. This preserves correct calibration when one Key is
// routed across several Codex accounts inside the same minute.
func (store *Store) CompletedAccountUsageBetween(authID string, since, until time.Time) (int64, error) {
	if authID == "" || !until.After(since) {
		return 0, nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	var units sql.NullInt64
	err := store.db.QueryRow(`SELECT SUM(units)
FROM usage_buckets
WHERE scope = 'account' AND scope_id = ? AND bucket_at > ? AND bucket_at <= ?`,
		authID, since.UTC().UnixMilli(), until.UTC().UnixMilli()).Scan(&units)
	if err != nil {
		return 0, fmt.Errorf("sum completed account usage: %w", err)
	}
	if !units.Valid || units.Int64 <= 0 {
		return 0, nil
	}
	return units.Int64, nil
}

// CompletedKeyAccountUsageBetween returns each managed Key's completed Token
// usage on one CPA account. Official percentages calibrate the Token/x scale;
// this Key-owned aggregate is the x charge authority.
func (store *Store) CompletedKeyAccountUsageBetween(authID string, since, until time.Time) (map[string]int64, error) {
	result := make(map[string]int64)
	if authID == "" || !until.After(since) {
		return result, nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT group_id, SUM(units)
FROM usage_buckets
WHERE scope = 'key_account_actual' AND auth_id = ? AND bucket_at > ? AND bucket_at <= ?
GROUP BY group_id`,
		authID, since.UTC().UnixMilli(), until.UTC().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("sum completed Key/account usage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var keyID string
		var units int64
		if err := rows.Scan(&keyID, &units); err != nil {
			return nil, fmt.Errorf("scan completed Key/account usage: %w", err)
		}
		if keyID != "" && units > 0 {
			result[keyID] = units
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate completed Key/account usage: %w", err)
	}
	return result, nil
}

func (store *Store) LoadOfficialXReconciliationState(authID string) (officialXReconciliationState, bool, error) {
	if authID == "" {
		return officialXReconciliationState{}, false, nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	var state officialXReconciliationState
	var observedAt int64
	err := store.db.QueryRow(`SELECT auth_id, window_reset_at, used_percent, account_capacity_x, observed_at
FROM official_x_reconciliation_state WHERE auth_id = ?`, authID).Scan(
		&state.AuthID, &state.WindowResetAt, &state.UsedPercent, &state.AccountCapacityX, &observedAt,
	)
	if err == sql.ErrNoRows {
		return officialXReconciliationState{}, false, nil
	}
	if err != nil {
		return officialXReconciliationState{}, false, fmt.Errorf("load official x reconciliation state: %w", err)
	}
	state.ObservedAt = time.UnixMilli(observedAt).UTC()
	return state, true, nil
}

// replaceOfficialXChargesTx rewrites the current account window from durable
// per-Key Token aggregates. Official percentages calibrate the Token/x scale,
// but must not assign unrelated account usage to a managed Key. Replacing the
// window also repairs charges written by older plugin releases on the next
// successful quota poll without requiring an operator reset.
func replaceOfficialXChargesTx(tx *sql.Tx, authID string, windowResetAt, observedAt int64, charges []officialXCharge) error {
	if authID == "" || windowResetAt <= 0 || observedAt <= 0 {
		return fmt.Errorf("invalid official x replacement")
	}
	if _, err := tx.Exec(`UPDATE key_account_allocation_buckets
SET completed_units = 0,
    provisional_units = CASE WHEN bucket_at <= ? THEN 0 ELSE provisional_units END,
    updated_at = ?
WHERE auth_id = ? AND window_reset_at = ?`,
		observedAt, observedAt, authID, windowResetAt); err != nil {
		return fmt.Errorf("clear replaced official x charges: %w", err)
	}
	for _, charge := range charges {
		if charge.KeyID == "" || charge.AuthID != authID || charge.WindowResetAt != windowResetAt ||
			charge.BucketAt <= 0 || charge.Units <= 0 || charge.CapacityUnits <= 0 {
			return fmt.Errorf("invalid official x charge")
		}
		if _, err := tx.Exec(`INSERT INTO key_account_allocation_buckets(
  key_id, auth_id, window_reset_at, bucket_at, completed_units, reserved_units,
  capacity_units, global_capacity_units, updated_at
) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)
ON CONFLICT(key_id, auth_id, window_reset_at, bucket_at) DO UPDATE SET
  completed_units = excluded.completed_units,
  capacity_units = MAX(key_account_allocation_buckets.capacity_units, excluded.capacity_units),
  global_capacity_units = MAX(key_account_allocation_buckets.global_capacity_units, excluded.global_capacity_units),
  updated_at = excluded.updated_at`,
			charge.KeyID, charge.AuthID, charge.WindowResetAt, charge.BucketAt, charge.Units,
			charge.CapacityUnits, charge.CapacityUnits, observedAt); err != nil {
			return fmt.Errorf("replace official x charge for Key %q: %w", charge.KeyID, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM key_account_allocation_buckets
WHERE auth_id = ? AND window_reset_at = ?
  AND completed_units = 0 AND provisional_units = 0 AND reserved_units = 0`,
		authID, windowResetAt); err != nil {
		return fmt.Errorf("cleanup replaced official x charges: %w", err)
	}
	return nil
}

// ApplyOfficialXReconciliation commits the calibration watermark and a full
// Token-derived replacement of the current Key window in one transaction. A
// crash can therefore neither double-charge a poll nor publish a watermark
// without its matching ledger.
func (store *Store) ApplyOfficialXReconciliation(state officialXReconciliationState, charges []officialXCharge, provisionalAfter time.Time) error {
	if state.AuthID == "" || state.WindowResetAt <= 0 || state.ObservedAt.IsZero() {
		return fmt.Errorf("invalid official x reconciliation state")
	}
	if !provisionalAfter.IsZero() && !state.ObservedAt.After(provisionalAfter) {
		return fmt.Errorf("invalid provisional x reconciliation interval")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin official x reconciliation: %w", err)
	}
	defer tx.Rollback()
	// A nil slice is the initial baseline and intentionally leaves any ledger
	// untouched. Every later observation supplies a non-nil (possibly empty)
	// replacement so stale percentage-derived charges can be removed.
	if charges != nil {
		if err := replaceOfficialXChargesTx(tx, state.AuthID, state.WindowResetAt, state.ObservedAt.UTC().UnixMilli(), charges); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO official_x_reconciliation_state(
  auth_id, window_reset_at, used_percent, account_capacity_x, observed_at
) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(auth_id) DO UPDATE SET
  window_reset_at = excluded.window_reset_at,
  used_percent = excluded.used_percent,
  account_capacity_x = excluded.account_capacity_x,
  observed_at = excluded.observed_at`,
		state.AuthID, state.WindowResetAt, state.UsedPercent, state.AccountCapacityX, state.ObservedAt.UTC().UnixMilli()); err != nil {
		return fmt.Errorf("save official x reconciliation state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit official x reconciliation: %w", err)
	}
	return nil
}

// ReplaceOfficialXCharges refreshes the current Token-derived Key ledger when
// the rounded official percentage has not changed. The calibration watermark
// remains untouched so Token evidence still accumulates until the next
// measurable official percentage change.
func (store *Store) ReplaceOfficialXCharges(authID string, windowResetAt int64, observedAt time.Time, charges []officialXCharge) error {
	if authID == "" || windowResetAt <= 0 || observedAt.IsZero() || charges == nil {
		return fmt.Errorf("invalid official x replacement")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin official x replacement: %w", err)
	}
	defer tx.Rollback()
	if err := replaceOfficialXChargesTx(tx, authID, windowResetAt, observedAt.UTC().UnixMilli(), charges); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit official x replacement: %w", err)
	}
	return nil
}

// DeleteAllocationBucketsThrough durably retires every Key cycle belonging to
// an official account window that Codex has replaced, including an early reset
// whose former reset timestamp is still in the future.
func (store *Store) DeleteAllocationBucketsThrough(authID string, resetAt time.Time) error {
	if authID == "" || resetAt.IsZero() {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, err := store.db.Exec(`DELETE FROM key_account_allocation_buckets
WHERE auth_id = ? AND window_reset_at <= ?`, authID, resetAt.UTC().UnixMilli()); err != nil {
		return fmt.Errorf("delete retired official-window allocations: %w", err)
	}
	return nil
}

// DeletePolicy atomically removes enforcement configuration and every
// plugin-owned per-Key history row. Account snapshots and calibrations are
// independent, so re-adding the Key starts at zero without resetting Codex.
func (store *Store) DeletePolicy(keyID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin key policy delete: %w", err)
	}
	// Deleting management also deletes every plugin-owned per-Key ledger and
	// history row. Re-adding the same CPA Key is an explicit fresh allocation;
	// official account snapshots remain independent and are not touched here.
	deletes := []struct {
		name  string
		query string
	}{
		{name: "policy", query: `DELETE FROM key_policies WHERE key_id = ?`},
		{name: "usage events", query: `DELETE FROM usage_events WHERE key_id = ?`},
		{name: "usage buckets", query: `DELETE FROM usage_buckets WHERE scope_id = ? AND scope IN ('key', 'key_actual')`},
		{name: "usage analysis", query: `DELETE FROM usage_analysis_buckets WHERE key_id = ?`},
		{name: "actual Token total", query: `DELETE FROM key_actual_token_totals WHERE key_id = ?`},
		{name: "request logs", query: `DELETE FROM request_logs WHERE key_id = ?`},
		{name: "operational logs", query: `DELETE FROM operational_logs WHERE key_id = ?`},
		{name: "allocation buckets", query: `DELETE FROM key_account_allocation_buckets WHERE key_id = ?`},
	}
	for _, item := range deletes {
		if _, err := tx.Exec(item.query, keyID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete Key %s: %w", item.name, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM usage_buckets WHERE scope = 'key_account_actual' AND group_id = ?`, keyID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete Key account-attribution usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit key policy delete: %w", err)
	}
	return nil
}

// ResetPolicyUsage atomically clears plugin-owned quota and usage accounting
// for one Key while preserving its policy and both classes of audit logs.
// Official account snapshots and calibration rows are intentionally excluded.
func (store *Store) ResetPolicyUsage(keyID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin Key usage reset: %w", err)
	}
	deletes := []struct {
		name  string
		query string
	}{
		{name: "usage events", query: `DELETE FROM usage_events WHERE key_id = ?`},
		{name: "usage buckets", query: `DELETE FROM usage_buckets WHERE scope_id = ? AND scope IN ('key', 'key_actual')`},
		{name: "usage analysis", query: `DELETE FROM usage_analysis_buckets WHERE key_id = ?`},
		{name: "actual Token total", query: `DELETE FROM key_actual_token_totals WHERE key_id = ?`},
		{name: "allocation buckets", query: `DELETE FROM key_account_allocation_buckets WHERE key_id = ?`},
	}
	for _, item := range deletes {
		if _, err := tx.Exec(item.query, keyID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("reset Key %s: %w", item.name, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM usage_buckets WHERE scope = 'key_account_actual' AND group_id = ?`, keyID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("reset Key account-attribution usage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Key usage reset: %w", err)
	}
	return nil
}

// UpsertUsageBuckets persists an in-memory batch of minute aggregates.
// The primary key separates the downstream Key and account counters, avoiding
// a key/account Cartesian growth when accounts rotate within one time bucket.
func usageBucketScopeID(event UsageEvent) string {
	switch event.Scope {
	case "key", "key_actual":
		return event.KeyID
	case "account":
		return event.AuthID
	case "key_account_actual":
		if event.KeyID == "" || event.AuthID == "" {
			return ""
		}
		return event.KeyID + "\x1f" + event.AuthID
	default:
		return ""
	}
}

func (store *Store) UpsertUsageBuckets(events []UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin usage bucket transaction: %w", err)
	}
	statement, err := tx.Prepare(`
INSERT INTO usage_buckets (
  scope, scope_id, group_id, auth_id, bucket_at, units, request_count, metered_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(scope, scope_id, bucket_at) DO UPDATE SET
  group_id = excluded.group_id,
  auth_id = CASE WHEN usage_buckets.auth_id = excluded.auth_id THEN usage_buckets.auth_id ELSE 'mixed' END,
  units = usage_buckets.units + excluded.units,
  request_count = usage_buckets.request_count + excluded.request_count,
  metered_by = excluded.metered_by`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare usage bucket upsert: %w", err)
	}
	defer statement.Close()
	for _, event := range events {
		scopeID := usageBucketScopeID(event)
		if scopeID == "" || event.RecordedAt.IsZero() || event.Units <= 0 || event.RequestCount <= 0 {
			_ = tx.Rollback()
			return fmt.Errorf("invalid usage bucket")
		}
		if _, err := statement.Exec(
			event.Scope, scopeID, event.GroupID, event.AuthID, event.RecordedAt.UTC().UnixMilli(),
			event.Units, event.RequestCount, event.MeteredBy,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("upsert usage bucket: %w", err)
		}
	}
	if err := upsertUsageAnalysisBuckets(tx, events); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage buckets: %w", err)
	}
	return nil
}

// FlushUsageAndLogs keeps quota buckets and their compact audit decisions in
// the same SQLite transaction. The scheduler has already made the decision in
// memory; this atomic flush prevents a retry after a partial database failure
// from double-counting the Key window.
func (store *Store) FlushUsageAndLogs(events []UsageEvent, logs []DecisionLog) error {
	if len(events) == 0 && len(logs) == 0 {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin usage/log flush: %w", err)
	}
	if len(events) > 0 {
		statement, err := tx.Prepare(`
INSERT INTO usage_buckets (scope, scope_id, group_id, auth_id, bucket_at, units, request_count, metered_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(scope, scope_id, bucket_at) DO UPDATE SET
  group_id = excluded.group_id,
  auth_id = CASE WHEN usage_buckets.auth_id = excluded.auth_id THEN usage_buckets.auth_id ELSE 'mixed' END,
  units = usage_buckets.units + excluded.units,
  request_count = usage_buckets.request_count + excluded.request_count,
  metered_by = excluded.metered_by`)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("prepare usage bucket flush: %w", err)
		}
		defer statement.Close()
		for _, event := range events {
			scopeID := usageBucketScopeID(event)
			if scopeID == "" || event.RecordedAt.IsZero() || event.Units <= 0 || event.RequestCount <= 0 {
				_ = tx.Rollback()
				return fmt.Errorf("invalid usage bucket")
			}
			if _, err := statement.Exec(event.Scope, scopeID, event.GroupID, event.AuthID, event.RecordedAt.UTC().UnixMilli(), event.Units, event.RequestCount, event.MeteredBy); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("flush usage bucket: %w", err)
			}
		}
		if err := upsertUsageAnalysisBuckets(tx, events); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if len(logs) > 0 {
		statement, err := tx.Prepare(`INSERT INTO request_logs(key_id, auth_id, model, request_content, requested_at, decision, status_code, reason, units)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("prepare request log flush: %w", err)
		}
		defer statement.Close()
		for _, entry := range logs {
			if entry.KeyID == "" || entry.RequestedAt.IsZero() {
				continue
			}
			if _, err := statement.Exec(entry.KeyID, entry.AuthID, entry.Model, entry.RequestContent, entry.RequestedAt.UTC().UnixMilli(), entry.Decision, entry.StatusCode, entry.Reason, entry.Units); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("flush request log: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage/log flush: %w", err)
	}
	return nil
}

// upsertUsageAnalysisBuckets mirrors only completed per-Key usage into the
// compact analytics ledger. It shares the same transaction as the source
// bucket, so a chart can never be ahead of durable actual-token accounting.
func upsertUsageAnalysisBuckets(tx *sql.Tx, events []UsageEvent) error {
	bucketStatement, err := tx.Prepare(`
INSERT INTO usage_analysis_buckets(key_id, bucket_at, units, request_count)
VALUES (?, ?, ?, ?)
ON CONFLICT(key_id, bucket_at) DO UPDATE SET
  units = usage_analysis_buckets.units + excluded.units,
  request_count = usage_analysis_buckets.request_count + excluded.request_count`)
	if err != nil {
		return fmt.Errorf("prepare usage analysis bucket upsert: %w", err)
	}
	defer bucketStatement.Close()
	totalStatement, err := tx.Prepare(`
INSERT INTO key_actual_token_totals(key_id, units, request_count)
VALUES (?, ?, ?)
ON CONFLICT(key_id) DO UPDATE SET
  units = key_actual_token_totals.units + excluded.units,
  request_count = key_actual_token_totals.request_count + excluded.request_count`)
	if err != nil {
		return fmt.Errorf("prepare Key actual Token total upsert: %w", err)
	}
	defer totalStatement.Close()
	for _, event := range events {
		if event.Scope != "key_actual" {
			continue
		}
		if event.KeyID == "" || event.RecordedAt.IsZero() || event.Units <= 0 || event.RequestCount <= 0 {
			return fmt.Errorf("invalid usage analysis bucket")
		}
		bucketTime := event.RequestedAt
		if bucketTime.IsZero() {
			bucketTime = event.RecordedAt
		}
		bucketAt := bucketTime.UTC().Truncate(usageAnalysisBucketWindow).UnixMilli()
		if _, err := bucketStatement.Exec(event.KeyID, bucketAt, event.Units, event.RequestCount); err != nil {
			return fmt.Errorf("upsert usage analysis bucket: %w", err)
		}
		if _, err := totalStatement.Exec(event.KeyID, event.Units, event.RequestCount); err != nil {
			return fmt.Errorf("upsert Key actual Token total: %w", err)
		}
	}
	return nil
}

func (store *Store) ListDecisionLogs(keyID string, limit int) ([]DecisionLog, error) {
	items, _, err := store.ListDecisionLogsPage(keyID, "", "", limit, 0)
	return items, err
}

// ListDecisionLogsPage filters and pages compact Key decision logs inside
// SQLite, so the panel never has to load the complete retained request trail.
func (store *Store) ListDecisionLogsPage(keyID, decision, search string, limit, offset int) ([]DecisionLog, int, error) {
	keyID = strings.TrimSpace(keyID)
	decision = strings.ToLower(strings.TrimSpace(decision))
	search = strings.TrimSpace(search)
	switch decision {
	case "", "completed", "blocked", "failed", "ignored", "expired":
	default:
		return nil, 0, fmt.Errorf("invalid decision log filter")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	clauses := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if keyID != "" {
		clauses = append(clauses, `key_id = ?`)
		args = append(args, keyID)
	}
	if decision != "" {
		clauses = append(clauses, `decision = ?`)
		args = append(args, decision)
	}
	if search != "" {
		// instr preserves literal operator searches such as 503 or
		// quota_snapshot_unavailable without LIKE wildcard semantics.
		clauses = append(clauses, `instr(lower(model || ' ' || request_content || ' ' || reason || ' ' || auth_id || ' ' || key_id), lower(?)) > 0`)
		args = append(args, search)
	}
	where := ""
	if len(clauses) > 0 {
		where = ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	var total int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM request_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count request logs: %w", err)
	}
	query := `SELECT id, key_id, auth_id, model, request_content, requested_at, decision, status_code, reason, units
FROM request_logs` + where + ` ORDER BY requested_at DESC, id DESC LIMIT ? OFFSET ?`
	pageArgs := append(append([]any(nil), args...), limit, offset)
	return store.scanDecisionLogs(query, pageArgs, total)
}

// ClearDecisionLogs removes only the request and policy audit trail for one
// managed Key. Quota ledgers, usage analysis, policies, and runtime logs remain
// independent and are deliberately not touched.
func (store *Store) ClearDecisionLogs(keyID string) error {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return fmt.Errorf("key_id is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, err := store.db.Exec(`DELETE FROM request_logs WHERE key_id = ?`, keyID); err != nil {
		return fmt.Errorf("clear request logs: %w", err)
	}
	return nil
}

func (store *Store) scanDecisionLogs(query string, args []any, total int) ([]DecisionLog, int, error) {
	rows, err := store.db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query request logs: %w", err)
	}
	defer rows.Close()
	entries := make([]DecisionLog, 0)
	for rows.Next() {
		var entry DecisionLog
		var requestedAt int64
		if err := rows.Scan(&entry.ID, &entry.KeyID, &entry.AuthID, &entry.Model, &entry.RequestContent, &requestedAt, &entry.Decision, &entry.StatusCode, &entry.Reason, &entry.Units); err != nil {
			return nil, 0, fmt.Errorf("scan request log: %w", err)
		}
		entry.RequestedAt = time.UnixMilli(requestedAt).UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}

func (store *Store) AppendOperationalLog(entry OperationalLog) error {
	entry.Level = strings.ToLower(strings.TrimSpace(entry.Level))
	entry.Event = strings.TrimSpace(entry.Event)
	entry.Message = strings.TrimSpace(entry.Message)
	entry.AuthID = strings.TrimSpace(entry.AuthID)
	entry.KeyID = strings.TrimSpace(entry.KeyID)
	if entry.Level != "info" && entry.Level != "warn" && entry.Level != "error" {
		return fmt.Errorf("invalid operational log level")
	}
	if entry.Event == "" || entry.Message == "" {
		return fmt.Errorf("operational log event and message are required")
	}
	if len(entry.Event) > 96 || len(entry.Message) > 1024 {
		return fmt.Errorf("operational log exceeds the maximum length")
	}
	if entry.OccurredAt.IsZero() {
		entry.OccurredAt = time.Now().UTC()
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	_, err := store.db.Exec(`INSERT INTO operational_logs(occurred_at, level, event, message, auth_id, key_id)
VALUES (?, ?, ?, ?, ?, ?)`, entry.OccurredAt.UTC().UnixMilli(), entry.Level, entry.Event, entry.Message, entry.AuthID, entry.KeyID)
	if err != nil {
		return fmt.Errorf("append operational log: %w", err)
	}
	return nil
}

func (store *Store) ListOperationalLogs(level, search string, limit int) ([]OperationalLog, error) {
	items, _, err := store.ListOperationalLogsPage(level, search, limit, 0)
	return items, err
}

// ListOperationalLogsPage returns one bounded operation-log page and its
// matching total. Filtering happens in SQLite so the panel never loads the
// complete retained log history into browser memory.
func (store *Store) ListOperationalLogsPage(level, search string, limit, offset int) ([]OperationalLog, int, error) {
	level = strings.ToLower(strings.TrimSpace(level))
	search = strings.TrimSpace(search)
	if level != "" && level != "info" && level != "warn" && level != "error" {
		return nil, 0, fmt.Errorf("invalid operational log level")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	clauses := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if level != "" {
		clauses = append(clauses, `level = ?`)
		args = append(args, level)
	}
	if search != "" {
		// instr avoids LIKE wildcard semantics, so an operator can search literal
		// error codes, identifiers, or punctuation without escaping surprises.
		clauses = append(clauses, `instr(lower(event || ' ' || message || ' ' || auth_id || ' ' || key_id), lower(?)) > 0`)
		args = append(args, search)
	}
	if len(clauses) > 0 {
		where := ` WHERE ` + strings.Join(clauses, ` AND `)
		countQuery := `SELECT COUNT(*) FROM operational_logs` + where
		var total int
		if err := store.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count operational logs: %w", err)
		}
		query := `SELECT id, occurred_at, level, event, message, auth_id, key_id FROM operational_logs` + where + ` ORDER BY occurred_at DESC, id DESC LIMIT ? OFFSET ?`
		pageArgs := append(append([]any(nil), args...), limit, offset)
		return store.scanOperationalLogs(query, pageArgs, total)
	}
	var total int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM operational_logs`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count operational logs: %w", err)
	}
	query := `SELECT id, occurred_at, level, event, message, auth_id, key_id FROM operational_logs ORDER BY occurred_at DESC, id DESC LIMIT ? OFFSET ?`
	return store.scanOperationalLogs(query, []any{limit, offset}, total)
}

// ClearOperationalLogs removes only plugin lifecycle, configuration, quota
// synchronization, and error rows. Per-Key request logs and accounting remain.
func (store *Store) ClearOperationalLogs() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, err := store.db.Exec(`DELETE FROM operational_logs`); err != nil {
		return fmt.Errorf("clear operational logs: %w", err)
	}
	return nil
}

func (store *Store) scanOperationalLogs(query string, args []any, total int) ([]OperationalLog, int, error) {
	rows, err := store.db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query operational logs: %w", err)
	}
	defer rows.Close()
	items := make([]OperationalLog, 0)
	for rows.Next() {
		var item OperationalLog
		var occurredAt int64
		if err := rows.Scan(&item.ID, &occurredAt, &item.Level, &item.Event, &item.Message, &item.AuthID, &item.KeyID); err != nil {
			return nil, 0, fmt.Errorf("scan operational log: %w", err)
		}
		item.OccurredAt = time.UnixMilli(occurredAt).UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (store *Store) ReplaceModelCatalog(models []ModelCatalogEntry, syncedAt time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin model catalog sync: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM model_catalog`); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("clear model catalog: %w", err)
	}
	statement, err := tx.Prepare(`INSERT INTO model_catalog(model_id, display_name, owner, available, synced_at) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare model catalog sync: %w", err)
	}
	defer statement.Close()
	for _, model := range models {
		if strings.TrimSpace(model.ID) == "" {
			continue
		}
		if _, err := statement.Exec(model.ID, model.DisplayName, model.Owner, boolToInt(model.Available), syncedAt.UTC().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("save model catalog entry: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit model catalog sync: %w", err)
	}
	return nil
}

func (store *Store) ListModelCatalog() ([]ModelCatalogEntry, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT model_id, display_name, owner, available, synced_at FROM model_catalog ORDER BY model_id`)
	if err != nil {
		return nil, fmt.Errorf("query model catalog: %w", err)
	}
	defer rows.Close()
	models := make([]ModelCatalogEntry, 0)
	for rows.Next() {
		var item ModelCatalogEntry
		var available int
		var syncedAt int64
		if err := rows.Scan(&item.ID, &item.DisplayName, &item.Owner, &available, &syncedAt); err != nil {
			return nil, fmt.Errorf("scan model catalog: %w", err)
		}
		item.Available = available != 0
		item.SyncedAt = time.UnixMilli(syncedAt).UTC()
		models = append(models, item)
	}
	return models, rows.Err()
}

// LoadMeteringSince rebuilds counters from the bounded bucket table. Open
// calls CompactLegacyUsageEvents first, so this never scans old request rows.
func (store *Store) LoadMeteringSince(since time.Time) ([]UsageEvent, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	events := make([]UsageEvent, 0)
	rows, err := store.db.Query(`
SELECT scope, scope_id, group_id, auth_id, bucket_at, units, request_count, metered_by
FROM usage_buckets WHERE scope = 'key_actual' AND bucket_at >= ? ORDER BY bucket_at ASC, id ASC`, since.UTC().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("query usage buckets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var event UsageEvent
		var scopeID string
		var bucketAt int64
		if err := rows.Scan(&event.Scope, &scopeID, &event.GroupID, &event.AuthID, &bucketAt, &event.Units, &event.RequestCount, &event.MeteredBy); err != nil {
			return nil, fmt.Errorf("scan usage bucket: %w", err)
		}
		if event.Scope == "key_actual" {
			event.KeyID = scopeID
		} else {
			event.AuthID = scopeID
		}
		event.RecordedAt = time.UnixMilli(bucketAt).UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage buckets: %w", err)
	}
	return events, nil
}

// loadAllocationBucket is the durable counterpart of one managed Key's
// account-specific official weekly window.  The key includes the upstream
// reset instant, so a new official window never inherits a previous window's
// local accounting.
type allocationBucketRecord struct {
	KeyID               string
	AuthID              string
	WindowResetAt       int64
	BucketAt            int64
	CompletedUnits      int64
	ProvisionalUnits    int64
	ReservedUnits       int64
	CapacityUnits       int64
	GlobalCapacityUnits int64
}

// LoadAllocationBuckets returns only windows which may still be selected by a
// current official snapshot. Older rows are retained by normal data retention
// for auditability but never re-enter admission state after a restart.
func (store *Store) LoadAllocationBuckets(now time.Time) ([]allocationBucketRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`
SELECT key_id, auth_id, window_reset_at, bucket_at, completed_units, provisional_units, reserved_units, capacity_units, global_capacity_units
FROM key_account_allocation_buckets
WHERE window_reset_at > ?
ORDER BY window_reset_at ASC, bucket_at ASC`, now.UTC().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("query allocation buckets: %w", err)
	}
	defer rows.Close()
	items := make([]allocationBucketRecord, 0)
	for rows.Next() {
		var item allocationBucketRecord
		if err := rows.Scan(&item.KeyID, &item.AuthID, &item.WindowResetAt, &item.BucketAt, &item.CompletedUnits, &item.ProvisionalUnits, &item.ReservedUnits, &item.CapacityUnits, &item.GlobalCapacityUnits); err != nil {
			return nil, fmt.Errorf("scan allocation bucket: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate allocation buckets: %w", err)
	}
	return items, nil
}

// applyAllocationMutations commits reservation, provisional, and completion
// transitions as one SQLite transaction. The engine waits for a positive
// reservation to commit before handing CPA an allowed response; terminal
// transitions may be batched, but a clean shutdown drains them before close.
func (store *Store) applyAllocationMutations(mutations []allocationMutation) error {
	if len(mutations) == 0 {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin allocation mutation: %w", err)
	}
	statement, err := tx.Prepare(`
INSERT INTO key_account_allocation_buckets(
  key_id, auth_id, window_reset_at, bucket_at, completed_units, provisional_units,
  reserved_units, capacity_units, global_capacity_units, updated_at
) VALUES (?, ?, ?, ?, ?, MAX(?, 0), MAX(?, 0), MAX(?, 0), MAX(?, 0), ?)
ON CONFLICT(key_id, auth_id, window_reset_at, bucket_at) DO UPDATE SET
  completed_units = key_account_allocation_buckets.completed_units + excluded.completed_units,
  provisional_units = CASE
    WHEN key_account_allocation_buckets.provisional_units + ? < 0 THEN 0
    ELSE key_account_allocation_buckets.provisional_units + ?
  END,
  reserved_units = CASE
    WHEN key_account_allocation_buckets.reserved_units + ? < 0 THEN 0
    ELSE key_account_allocation_buckets.reserved_units + ?
  END,
	capacity_units = MAX(key_account_allocation_buckets.capacity_units, excluded.capacity_units),
	global_capacity_units = MAX(key_account_allocation_buckets.global_capacity_units, excluded.global_capacity_units),
  updated_at = excluded.updated_at`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare allocation mutation: %w", err)
	}
	defer statement.Close()
	// Only a reservation release with no actual usage can leave a zero row.
	// Normal successful admissions must not pay an extra DELETE round trip.
	emptyCandidates := make(map[allocationBucketKey]struct{})
	for _, mutation := range mutations {
		provisionalOnPrimary := mutation.ProvisionalDelta != 0 && mutation.ProvisionalKey == mutation.Key
		if mutation.CompletedDelta == 0 && mutation.ReservedDelta < 0 && !provisionalOnPrimary {
			emptyCandidates[mutation.Key] = struct{}{}
		}
	}
	var cleanup *sql.Stmt
	if len(emptyCandidates) > 0 {
		cleanup, err = tx.Prepare(`DELETE FROM key_account_allocation_buckets
WHERE key_id = ? AND auth_id = ? AND window_reset_at = ? AND bucket_at = ?
  AND completed_units = 0 AND provisional_units = 0 AND reserved_units = 0`)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("prepare empty allocation cleanup: %w", err)
		}
		defer cleanup.Close()
	}
	updatedAt := time.Now().UTC().UnixMilli()
	applyRow := func(key allocationBucketKey, completedDelta, provisionalDelta, reservedDelta, capacityUnits, globalCapacityUnits int64) error {
		if _, err := statement.Exec(
			key.KeyID,
			key.AuthID,
			key.WindowResetAt,
			key.BucketAt,
			completedDelta,
			provisionalDelta,
			reservedDelta,
			capacityUnits,
			globalCapacityUnits,
			updatedAt,
			provisionalDelta,
			provisionalDelta,
			reservedDelta,
			reservedDelta,
		); err != nil {
			return fmt.Errorf("save allocation mutation: %w", err)
		}
		return nil
	}
	for _, mutation := range mutations {
		if mutation.ProvisionalDelta != 0 && mutation.ProvisionalKey.KeyID == "" {
			_ = tx.Rollback()
			return fmt.Errorf("save allocation mutation: missing provisional bucket key")
		}
		provisionalOnPrimary := mutation.ProvisionalDelta != 0 && mutation.ProvisionalKey == mutation.Key
		primaryProvisionalDelta := int64(0)
		if provisionalOnPrimary {
			primaryProvisionalDelta = mutation.ProvisionalDelta
		}
		if mutation.ReservedDelta != 0 || mutation.CompletedDelta != 0 || primaryProvisionalDelta != 0 {
			if err := applyRow(
				mutation.Key, mutation.CompletedDelta, primaryProvisionalDelta,
				mutation.ReservedDelta, mutation.CapacityUnits, mutation.GlobalCapacityUnits,
			); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if mutation.ProvisionalDelta != 0 && !provisionalOnPrimary {
			if err := applyRow(
				mutation.ProvisionalKey, 0, mutation.ProvisionalDelta, 0,
				mutation.CapacityUnits, mutation.GlobalCapacityUnits,
			); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	// A failed request that consumed no tokens returns its pre-dispatch
	// reservation to zero. Keep no durable zero row: it is neither audit usage
	// nor enforcement state, and otherwise every such request is reloaded into
	// the active in-memory indexes until the next weekly reset.
	for key := range emptyCandidates {
		if _, err := cleanup.Exec(key.KeyID, key.AuthID, key.WindowResetAt, key.BucketAt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("cleanup empty allocation bucket: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit allocation mutation: %w", err)
	}
	return nil
}

// ListUsageEvents returns latest records for a specific Key. It is panel-only,
// never used in the scheduler request path.
func (store *Store) ListUsageEvents(keyID string, limit int) ([]UsageEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`
SELECT id, key_id, group_id, auth_id, model, requested_at, recorded_at,
       input_tokens, output_tokens, reasoning_tokens, cached_tokens, units, request_count, metered_by, failed, failure_status
FROM (
  SELECT id, key_id, group_id, auth_id, model, requested_at, recorded_at,
         input_tokens, output_tokens, reasoning_tokens, cached_tokens, units, 1 AS request_count, metered_by, failed, failure_status
  FROM usage_events WHERE key_id = ?
  UNION ALL
  SELECT id, scope_id AS key_id, group_id, auth_id, '' AS model, bucket_at AS requested_at, bucket_at AS recorded_at,
         0 AS input_tokens, 0 AS output_tokens, 0 AS reasoning_tokens, 0 AS cached_tokens, units, request_count, metered_by, 0 AS failed, 0 AS failure_status
  FROM usage_buckets WHERE scope = 'key_actual' AND scope_id = ?
)
ORDER BY recorded_at DESC, id DESC LIMIT ?`, keyID, keyID, limit)
	if err != nil {
		return nil, fmt.Errorf("query usage records: %w", err)
	}
	defer rows.Close()
	return scanUsageEvents(rows)
}

// ListUsageTrend aggregates completed Key token usage into caller-selected
// bins in SQLite. The panel therefore receives a bounded, real history rather
// than synthesizing a trend from one current percentage.
func (store *Store) ListUsageTrend(keyID string, since time.Time, bin time.Duration) ([]UsageTrendPoint, error) {
	if strings.TrimSpace(keyID) == "" {
		return nil, fmt.Errorf("key_id is required")
	}
	binMillis := bin.Milliseconds()
	if binMillis <= 0 {
		return nil, fmt.Errorf("trend bin must be positive")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`
SELECT (bucket_at / ?) * ? AS bucket_at, SUM(units), SUM(request_count)
FROM usage_buckets
WHERE scope = 'key_actual' AND scope_id = ? AND bucket_at >= ?
GROUP BY (bucket_at / ?) * ?
ORDER BY bucket_at ASC`, binMillis, binMillis, keyID, since.UTC().UnixMilli(), binMillis, binMillis)
	if err != nil {
		return nil, fmt.Errorf("query usage trend: %w", err)
	}
	defer rows.Close()
	points := make([]UsageTrendPoint, 0)
	for rows.Next() {
		var point UsageTrendPoint
		var bucketAt int64
		if err := rows.Scan(&bucketAt, &point.Units, &point.RequestCount); err != nil {
			return nil, fmt.Errorf("scan usage trend: %w", err)
		}
		point.At = time.UnixMilli(bucketAt).UTC()
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage trend: %w", err)
	}
	return points, nil
}

// ListUsageAnalysisBuckets returns the 5-minute actual Key analysis ledger
// for a bounded time interval. Calendar grouping remains in Go so the
// selected IANA timezone (including DST) is represented correctly.
func (store *Store) ListUsageAnalysisBuckets(keyID string, from, until time.Time) ([]UsageTrendPoint, error) {
	if strings.TrimSpace(keyID) == "" {
		return nil, fmt.Errorf("key_id is required")
	}
	store.analysisMu.RLock()
	defer store.analysisMu.RUnlock()
	database := store.analysisDB
	if database == nil {
		database = store.db
	}
	rows, err := database.Query(`
SELECT bucket_at, units, request_count
FROM usage_analysis_buckets
WHERE key_id = ? AND bucket_at >= ? AND bucket_at < ?
ORDER BY bucket_at ASC`, keyID, from.UTC().UnixMilli(), until.UTC().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("query usage analysis buckets: %w", err)
	}
	defer rows.Close()
	points := make([]UsageTrendPoint, 0)
	for rows.Next() {
		var point UsageTrendPoint
		var bucketAt int64
		if err := rows.Scan(&bucketAt, &point.Units, &point.RequestCount); err != nil {
			return nil, fmt.Errorf("scan usage analysis bucket: %w", err)
		}
		point.At = time.UnixMilli(bucketAt).UTC()
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage analysis buckets: %w", err)
	}
	return points, nil
}

// LoadUsageAnalysisSnapshot reads chart buckets and their retained-history
// boundary through one read transaction. The panel must not describe a newer
// coverage boundary than the Token points it is rendering.
func (store *Store) LoadUsageAnalysisSnapshot(ctx context.Context, keyID string, from, until time.Time) ([]UsageTrendPoint, *time.Time, error) {
	if strings.TrimSpace(keyID) == "" {
		return nil, nil, fmt.Errorf("key_id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	store.analysisMu.RLock()
	defer store.analysisMu.RUnlock()
	database := store.analysisDB
	usingWriterFallback := database == nil
	if database == nil {
		database = store.db
	}
	queryCtx := ctx
	cancel := func() {}
	if usingWriterFallback {
		queryCtx, cancel = context.WithTimeout(ctx, usageAnalysisWriterFallbackTimeout)
	}
	defer cancel()
	tx, err := database.BeginTx(queryCtx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin usage analysis snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(queryCtx, `
SELECT bucket_at, units, request_count
FROM usage_analysis_buckets
WHERE key_id = ? AND bucket_at >= ? AND bucket_at < ?
ORDER BY bucket_at ASC`, keyID, from.UTC().UnixMilli(), until.UTC().UnixMilli())
	if err != nil {
		return nil, nil, fmt.Errorf("query usage analysis snapshot: %w", err)
	}
	points := make([]UsageTrendPoint, 0)
	for rows.Next() {
		var point UsageTrendPoint
		var bucketAt int64
		if err := rows.Scan(&bucketAt, &point.Units, &point.RequestCount); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("scan usage analysis snapshot: %w", err)
		}
		point.At = time.UnixMilli(bucketAt).UTC()
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("iterate usage analysis snapshot: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("close usage analysis snapshot rows: %w", err)
	}
	var earliest sql.NullInt64
	if err := tx.QueryRowContext(queryCtx, `SELECT MIN(bucket_at) FROM usage_analysis_buckets WHERE key_id = ?`, keyID).Scan(&earliest); err != nil {
		return nil, nil, fmt.Errorf("query usage analysis snapshot coverage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit usage analysis snapshot: %w", err)
	}
	if !earliest.Valid {
		return points, nil, nil
	}
	availableFrom := time.UnixMilli(earliest.Int64).UTC()
	return points, &availableFrom, nil
}

type keyActualTokenTotals struct {
	Cycle int64
	Total int64
}

// LoadKeyActualTokenTotals aggregates every requested Key in one SQLite query.
// A nil cycle start deliberately leaves Cycle at zero; the caller separately
// reports that the official cycle is unknown instead of presenting a false 0.
func (store *Store) LoadKeyActualTokenTotals(ctx context.Context, cycleStarts map[string]*time.Time) (map[string]keyActualTokenTotals, error) {
	result := make(map[string]keyActualTokenTotals, len(cycleStarts))
	if len(cycleStarts) == 0 {
		return result, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var query strings.Builder
	query.WriteString(`WITH requested_keys(key_id, cycle_start) AS (VALUES `)
	arguments := make([]any, 0, len(cycleStarts)*2)
	index := 0
	for keyID, cycleStart := range cycleStarts {
		if index > 0 {
			query.WriteString(",")
		}
		query.WriteString("(?, ?)")
		arguments = append(arguments, keyID)
		if cycleStart == nil {
			arguments = append(arguments, nil)
		} else {
			arguments = append(arguments, cycleStart.UTC().UnixMilli())
		}
		index++
	}
	query.WriteString(`)
SELECT requested_keys.key_id,
	   COALESCE(key_actual_token_totals.units, 0),
	   COALESCE(SUM(CASE
	     WHEN requested_keys.cycle_start IS NOT NULL
	      AND usage_analysis_buckets.bucket_at >= requested_keys.cycle_start
	     THEN usage_analysis_buckets.units ELSE 0 END), 0)
FROM requested_keys
LEFT JOIN key_actual_token_totals ON key_actual_token_totals.key_id = requested_keys.key_id
LEFT JOIN usage_analysis_buckets
  ON usage_analysis_buckets.key_id = requested_keys.key_id
 AND requested_keys.cycle_start IS NOT NULL
 AND usage_analysis_buckets.bucket_at >= requested_keys.cycle_start
GROUP BY requested_keys.key_id, key_actual_token_totals.units`)

	store.analysisMu.RLock()
	defer store.analysisMu.RUnlock()
	database := store.analysisDB
	usingWriterFallback := database == nil
	if database == nil {
		database = store.db
	}
	queryCtx := ctx
	cancel := func() {}
	if usingWriterFallback {
		queryCtx, cancel = context.WithTimeout(ctx, usageAnalysisWriterFallbackTimeout)
	}
	defer cancel()
	rows, err := database.QueryContext(queryCtx, query.String(), arguments...)
	if err != nil {
		return nil, fmt.Errorf("query Key actual Token totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var keyID string
		var totals keyActualTokenTotals
		if err := rows.Scan(&keyID, &totals.Total, &totals.Cycle); err != nil {
			return nil, fmt.Errorf("scan Key actual Token totals: %w", err)
		}
		result[keyID] = totals
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Key actual Token totals: %w", err)
	}
	return result, nil
}

// UsageAnalysisAvailableFrom reports the first retained completed-usage
// bucket for one Key. It lets the panel distinguish genuinely idle periods
// from a date range that predates plugin accounting or the 366-day retention.
func (store *Store) UsageAnalysisAvailableFrom(keyID string) (*time.Time, error) {
	if strings.TrimSpace(keyID) == "" {
		return nil, fmt.Errorf("key_id is required")
	}
	store.analysisMu.RLock()
	defer store.analysisMu.RUnlock()
	database := store.analysisDB
	if database == nil {
		database = store.db
	}
	var earliest sql.NullInt64
	if err := database.QueryRow(`SELECT MIN(bucket_at) FROM usage_analysis_buckets WHERE key_id = ?`, keyID).Scan(&earliest); err != nil {
		return nil, fmt.Errorf("query usage analysis coverage: %w", err)
	}
	if !earliest.Valid {
		return nil, nil
	}
	value := time.UnixMilli(earliest.Int64).UTC()
	return &value, nil
}

// AnalysisReaderDegraded reports whether management analysis is sharing the
// writer connection because the optional read-only WAL connection was not
// available at startup. The value is immutable after Store construction.
func (store *Store) AnalysisReaderDegraded() bool {
	if store == nil {
		return false
	}
	store.analysisMu.RLock()
	defer store.analysisMu.RUnlock()
	return store.analysisReaderDegraded
}

// DeleteUsageEventsBefore enforces configured data retention. It never deletes
// the seven days needed for enforcement.
func (store *Store) DeleteUsageEventsBefore(before time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return fmt.Errorf("begin expired usage cleanup: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM usage_events WHERE recorded_at < ?`, before.UTC().UnixMilli()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete expired legacy usage records: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM usage_buckets WHERE bucket_at < ?`, before.UTC().UnixMilli()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete expired usage buckets: %w", err)
	}
	analysisBefore := time.Now().UTC().Add(-usageAnalysisRetention).UnixMilli()
	if _, err := tx.Exec(`DELETE FROM usage_analysis_buckets WHERE bucket_at < ?`, analysisBefore); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete expired usage analysis buckets: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM request_logs WHERE requested_at < ?`, before.UTC().UnixMilli()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete expired request logs: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM operational_logs WHERE occurred_at < ?`, before.UTC().UnixMilli()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete expired operational logs: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM key_account_allocation_buckets WHERE updated_at < ? AND window_reset_at < ?`, before.UTC().UnixMilli(), before.UTC().UnixMilli()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete expired allocation buckets: %w", err)
	}
	// A removed account keeps its snapshot only until the active official
	// window and normal audit-retention period have both elapsed. This retains
	// delete/re-add protection without accumulating abandoned identities.
	if _, err := tx.Exec(`DELETE FROM official_quota_snapshots
WHERE auth_id NOT IN (SELECT auth_id FROM account_pool_entries)
  AND observed_at < ?
  AND (secondary_reset_at = 0 OR secondary_reset_at < ?)`, before.UTC().UnixMilli(), before.UTC().UnixMilli()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete expired detached official snapshots: %w", err)
	}
	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("delete expired usage records: %w", err)
	}
	return nil
}

func scanUsageEvents(rows *sql.Rows) ([]UsageEvent, error) {
	events := make([]UsageEvent, 0)
	for rows.Next() {
		var event UsageEvent
		var requestedAt, recordedAt int64
		var failed int
		if err := rows.Scan(
			&event.ID, &event.KeyID, &event.GroupID, &event.AuthID, &event.Model, &requestedAt, &recordedAt,
			&event.InputTokens, &event.OutputTokens, &event.ReasoningTokens, &event.CachedTokens, &event.Units, &event.RequestCount, &event.MeteredBy, &failed, &event.FailureStatus,
		); err != nil {
			return nil, fmt.Errorf("scan usage event: %w", err)
		}
		event.RequestedAt = time.UnixMilli(requestedAt).UTC()
		event.RecordedAt = time.UnixMilli(recordedAt).UTC()
		event.Failed = failed != 0
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage events: %w", err)
	}
	return events, nil
}

func (store *Store) Close() error {
	if store == nil {
		return nil
	}
	store.analysisMu.Lock()
	defer store.analysisMu.Unlock()
	var analysisErr error
	if store.analysisDB != nil {
		analysisErr = store.analysisDB.Close()
	}
	var databaseErr error
	if store.db != nil {
		databaseErr = store.db.Close()
	}
	var lockErr error
	if store.lock != nil {
		lockErr = store.lock.release()
	}
	if databaseErr != nil {
		return databaseErr
	}
	if analysisErr != nil {
		return analysisErr
	}
	return lockErr
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
