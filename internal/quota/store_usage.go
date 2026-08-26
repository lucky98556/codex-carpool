package quota

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func (store *Store) FlushUsageAndLogs(events []UsageEvent, logs []DecisionLog) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertUsageEvents(tx, events); err != nil {
		return err
	}
	if err := insertDecisionLogs(tx, logs); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertUsageBuckets is the direct persistence entry used by bounded tests and
// management repair code; request callbacks use FlushUsageAndLogs.
func (store *Store) UpsertUsageBuckets(events []UsageEvent) error {
	return store.FlushUsageAndLogs(events, nil)
}

func upsertUsageEvents(tx *sql.Tx, events []UsageEvent) error {
	bucket, err := tx.Prepare(`INSERT INTO usage_buckets(scope,scope_id,auth_id,model,bucket_at,units,request_count,input_tokens,cached_tokens,output_tokens,metered_by)
VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(scope,scope_id,model,bucket_at) DO UPDATE SET
auth_id=CASE WHEN usage_buckets.auth_id=excluded.auth_id THEN usage_buckets.auth_id ELSE 'mixed' END,
units=usage_buckets.units+excluded.units,request_count=usage_buckets.request_count+excluded.request_count,
input_tokens=usage_buckets.input_tokens+excluded.input_tokens,cached_tokens=usage_buckets.cached_tokens+excluded.cached_tokens,
output_tokens=usage_buckets.output_tokens+excluded.output_tokens,metered_by=excluded.metered_by`)
	if err != nil {
		return fmt.Errorf("prepare usage bucket: %w", err)
	}
	defer bucket.Close()
	analysis, err := tx.Prepare(`INSERT INTO usage_analysis_buckets(key_id,model,bucket_at,units,request_count,input_tokens,cached_tokens,output_tokens,
input_cost_micros,cached_cost_micros,output_cost_micros,cost_micros)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(key_id,model,bucket_at) DO UPDATE SET units=usage_analysis_buckets.units+excluded.units,
request_count=usage_analysis_buckets.request_count+excluded.request_count,input_tokens=usage_analysis_buckets.input_tokens+excluded.input_tokens,
cached_tokens=usage_analysis_buckets.cached_tokens+excluded.cached_tokens,output_tokens=usage_analysis_buckets.output_tokens+excluded.output_tokens,
input_cost_micros=usage_analysis_buckets.input_cost_micros+excluded.input_cost_micros,
cached_cost_micros=usage_analysis_buckets.cached_cost_micros+excluded.cached_cost_micros,
output_cost_micros=usage_analysis_buckets.output_cost_micros+excluded.output_cost_micros,cost_micros=usage_analysis_buckets.cost_micros+excluded.cost_micros`)
	if err != nil {
		return fmt.Errorf("prepare usage analysis bucket: %w", err)
	}
	defer analysis.Close()
	total, err := tx.Prepare(`INSERT INTO key_actual_token_totals(key_id,total_tokens,input_tokens,cached_tokens,output_tokens,updated_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(key_id) DO UPDATE SET total_tokens=key_actual_token_totals.total_tokens+excluded.total_tokens,
input_tokens=key_actual_token_totals.input_tokens+excluded.input_tokens,cached_tokens=key_actual_token_totals.cached_tokens+excluded.cached_tokens,
output_tokens=key_actual_token_totals.output_tokens+excluded.output_tokens,updated_at=excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("prepare Key Token total: %w", err)
	}
	defer total.Close()
	for _, event := range events {
		if event.Scope != "key_actual" && event.Scope != "key_cost" {
			return fmt.Errorf("unsupported usage scope %q", event.Scope)
		}
		if event.KeyID == "" || event.RecordedAt.IsZero() || event.Units < 0 || event.RequestCount < 0 {
			return fmt.Errorf("invalid usage bucket")
		}
		model := strings.TrimSpace(event.Model)
		if _, err := bucket.Exec(event.Scope, event.KeyID, event.AuthID, model, event.RecordedAt.UTC().UnixMilli(), event.Units,
			event.RequestCount, event.InputTokens, event.CachedTokens, event.OutputTokens, event.MeteredBy); err != nil {
			return fmt.Errorf("upsert usage bucket: %w", err)
		}
		if event.Scope != "key_actual" {
			continue
		}
		at := event.RequestedAt
		if at.IsZero() {
			at = event.RecordedAt
		}
		if _, err := analysis.Exec(event.KeyID, model, at.UTC().Truncate(usageAnalysisBucketWindow).UnixMilli(), event.Units,
			event.RequestCount, event.InputTokens, event.CachedTokens, event.OutputTokens, event.InputCostMicros,
			event.CachedCostMicros, event.OutputCostMicros, event.CostMicros); err != nil {
			return fmt.Errorf("upsert usage analysis bucket: %w", err)
		}
		if _, err := total.Exec(event.KeyID, event.Units, event.InputTokens, event.CachedTokens, event.OutputTokens, time.Now().UTC().UnixMilli()); err != nil {
			return fmt.Errorf("upsert Key actual Token total: %w", err)
		}
	}
	return nil
}

func insertDecisionLogs(tx *sql.Tx, logs []DecisionLog) error {
	if len(logs) == 0 {
		return nil
	}
	statement, err := tx.Prepare(`INSERT INTO request_logs(key_id,key_suffix,auth_id,model,request_content,matched_term,matched_category,requested_at,
decision,status_code,reason,units,input_tokens,cached_tokens,output_tokens,input_cost_micros,cached_cost_micros,output_cost_micros,cost_micros)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, log := range logs {
		if _, err := statement.Exec(log.KeyID, log.KeySuffix, log.AuthID, log.Model, log.RequestContent, log.MatchedTerm, log.MatchedCategory,
			log.RequestedAt.UTC().UnixMilli(), log.Decision, log.StatusCode, log.Reason, log.Units, log.InputTokens, log.CachedTokens,
			log.OutputTokens, log.InputCostMicros, log.CachedCostMicros, log.OutputCostMicros, log.CostMicros); err != nil {
			return fmt.Errorf("insert request log: %w", err)
		}
	}
	return nil
}

func (store *Store) LoadMeteringSince(since time.Time) ([]UsageEvent, error) {
	return store.loadUsageSince("key_actual", since)
}

func (store *Store) LoadDollarSpendSince(since time.Time) ([]UsageEvent, error) {
	return store.loadUsageSince("key_cost", since)
}

func (store *Store) loadUsageSince(scope string, since time.Time) ([]UsageEvent, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT scope,scope_id,auth_id,model,bucket_at,units,request_count,input_tokens,cached_tokens,output_tokens,metered_by
FROM usage_buckets WHERE scope=? AND bucket_at>? ORDER BY bucket_at`, scope, since.UTC().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsageEvents(rows)
}

func (store *Store) ListUsageEvents(keyID string, limit int) ([]UsageEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT scope,scope_id,auth_id,model,bucket_at,units,request_count,input_tokens,cached_tokens,output_tokens,metered_by
FROM usage_buckets WHERE scope='key_actual' AND scope_id=? ORDER BY bucket_at DESC LIMIT ?`, strings.TrimSpace(keyID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsageEvents(rows)
}

func scanUsageEvents(rows *sql.Rows) ([]UsageEvent, error) {
	items := make([]UsageEvent, 0)
	for rows.Next() {
		var item UsageEvent
		var at int64
		if err := rows.Scan(&item.Scope, &item.KeyID, &item.AuthID, &item.Model, &at, &item.Units, &item.RequestCount,
			&item.InputTokens, &item.CachedTokens, &item.OutputTokens, &item.MeteredBy); err != nil {
			return nil, err
		}
		item.RecordedAt = time.UnixMilli(at).UTC()
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *Store) ListUsageTrend(keyID string, since time.Time, bin time.Duration) ([]UsageTrendPoint, error) {
	if bin <= 0 {
		return nil, fmt.Errorf("trend bin must be positive")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	rows, err := store.db.Query(`SELECT bucket_at,units,request_count FROM usage_buckets WHERE scope='key_actual' AND scope_id=? AND bucket_at>? ORDER BY bucket_at`, keyID, since.UTC().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := make(map[int64]UsageTrendPoint)
	for rows.Next() {
		var at, units, count int64
		if err := rows.Scan(&at, &units, &count); err != nil {
			return nil, err
		}
		bucket := time.UnixMilli(at).UTC().Truncate(bin).UnixMilli()
		point := points[bucket]
		point.At = time.UnixMilli(bucket).UTC()
		point.Units += units
		point.RequestCount += count
		points[bucket] = point
	}
	result := make([]UsageTrendPoint, 0)
	for at := since.UTC().Truncate(bin); at.Before(time.Now().UTC().Add(bin)); at = at.Add(bin) {
		point := points[at.UnixMilli()]
		point.At = at
		result = append(result, point)
	}
	return result, rows.Err()
}

func (store *Store) LoadUsageAnalysisSnapshot(ctx context.Context, keyID string, from, until time.Time) ([]UsageTrendPoint, *time.Time, error) {
	db, queryContext, release := store.analysisReader(ctx)
	defer release()
	rows, err := db.QueryContext(queryContext, `SELECT bucket_at,SUM(units),SUM(request_count),
SUM(input_tokens),SUM(cached_tokens),SUM(output_tokens),SUM(input_cost_micros),SUM(cached_cost_micros),SUM(output_cost_micros),SUM(cost_micros) FROM usage_analysis_buckets
WHERE key_id=? AND bucket_at>=? AND bucket_at<? GROUP BY bucket_at ORDER BY bucket_at`, keyID, from.UTC().UnixMilli(), until.UTC().UnixMilli())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]UsageTrendPoint, 0)
	for rows.Next() {
		var point UsageTrendPoint
		var at int64
		if err := rows.Scan(&at, &point.Units, &point.RequestCount, &point.InputTokens, &point.CachedTokens, &point.OutputTokens,
			&point.InputCostMicros, &point.CachedCostMicros, &point.OutputCostMicros, &point.CostMicros); err != nil {
			return nil, nil, err
		}
		point.At = time.UnixMilli(at).UTC()
		items = append(items, point)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var minimum sql.NullInt64
	if err := db.QueryRowContext(queryContext, `SELECT MIN(bucket_at) FROM usage_analysis_buckets WHERE key_id=?`, keyID).Scan(&minimum); err != nil {
		return nil, nil, err
	}
	var available *time.Time
	if minimum.Valid {
		value := time.UnixMilli(minimum.Int64).UTC()
		available = &value
	}
	return items, available, nil
}

func (store *Store) analysisReader(ctx context.Context) (*sql.DB, context.Context, func()) {
	store.analysisMu.RLock()
	if store.analysisDB != nil {
		return store.analysisDB, ctx, store.analysisMu.RUnlock
	}
	store.analysisMu.RUnlock()
	bounded, cancel := context.WithTimeout(ctx, usageAnalysisWriterFallbackTime)
	return store.db, bounded, cancel
}

func (store *Store) LoadUsageAnalysisBreakdown(ctx context.Context, keyID string, from, until time.Time) (UsageAnalysisBreakdown, error) {
	db, queryContext, release := store.analysisReader(ctx)
	defer release()
	// Durable analysis buckets retain the exact request-time rate snapshot, so
	// clearing audit logs or editing the rate card never rewrites history.
	rows, err := db.QueryContext(queryContext, `SELECT model,SUM(input_tokens),SUM(cached_tokens),SUM(output_tokens),SUM(units),SUM(request_count),
SUM(input_cost_micros),SUM(cached_cost_micros),SUM(output_cost_micros),SUM(cost_micros)
FROM usage_analysis_buckets WHERE key_id=? AND bucket_at>=? AND bucket_at<?
GROUP BY model ORDER BY SUM(units) DESC,model`,
		keyID, from.UTC().UnixMilli(), until.UTC().UnixMilli())
	if err != nil {
		return UsageAnalysisBreakdown{}, err
	}
	defer rows.Close()
	result := UsageAnalysisBreakdown{Models: make([]UsageModelBreakdown, 0)}
	for rows.Next() {
		var model UsageModelBreakdown
		if err := rows.Scan(&model.Model, &model.InputTokens, &model.CachedTokens, &model.OutputTokens, &model.TotalTokens,
			&model.RequestCount, &model.InputCostMicros, &model.CachedCostMicros, &model.OutputCostMicros, &model.CostMicros); err != nil {
			return UsageAnalysisBreakdown{}, err
		}
		result.InputTokens += model.InputTokens
		result.CachedTokens += model.CachedTokens
		result.OutputTokens += model.OutputTokens
		result.InputCostMicros += model.InputCostMicros
		result.CachedCostMicros += model.CachedCostMicros
		result.OutputCostMicros += model.OutputCostMicros
		result.CostMicros += model.CostMicros
		result.Models = append(result.Models, model)
	}
	return result, rows.Err()
}

func (store *Store) LoadGlobalUsageAnalysisBreakdown(ctx context.Context, from, until time.Time) (UsageAnalysisBreakdown, error) {
	db, queryContext, release := store.analysisReader(ctx)
	defer release()
	// This rollup reads the same durable request-time Token and cost snapshots
	// as per-Key analysis; it does not depend on paginated or clearable logs.
	rows, err := db.QueryContext(queryContext, `SELECT model,SUM(input_tokens),SUM(cached_tokens),SUM(output_tokens),SUM(units),SUM(request_count),
SUM(input_cost_micros),SUM(cached_cost_micros),SUM(output_cost_micros),SUM(cost_micros)
FROM usage_analysis_buckets WHERE bucket_at>=? AND bucket_at<?
GROUP BY model ORDER BY SUM(cost_micros) DESC,SUM(units) DESC,model`,
		from.UTC().UnixMilli(), until.UTC().UnixMilli())
	if err != nil {
		return UsageAnalysisBreakdown{}, err
	}
	defer rows.Close()
	result := UsageAnalysisBreakdown{Models: make([]UsageModelBreakdown, 0)}
	for rows.Next() {
		var model UsageModelBreakdown
		if err := rows.Scan(&model.Model, &model.InputTokens, &model.CachedTokens, &model.OutputTokens, &model.TotalTokens,
			&model.RequestCount, &model.InputCostMicros, &model.CachedCostMicros, &model.OutputCostMicros, &model.CostMicros); err != nil {
			return UsageAnalysisBreakdown{}, err
		}
		result.InputTokens += model.InputTokens
		result.CachedTokens += model.CachedTokens
		result.OutputTokens += model.OutputTokens
		result.InputCostMicros += model.InputCostMicros
		result.CachedCostMicros += model.CachedCostMicros
		result.OutputCostMicros += model.OutputCostMicros
		result.CostMicros += model.CostMicros
		result.Models = append(result.Models, model)
	}
	return result, rows.Err()
}

type keyActualTokenTotals struct {
	Cycle  int64
	Total  int64
	Input  int64
	Cached int64
	Output int64
}

func (store *Store) LoadKeyActualTokenTotals(ctx context.Context, cycleStarts map[string]*time.Time) (map[string]keyActualTokenTotals, error) {
	db, queryContext, release := store.analysisReader(ctx)
	defer release()
	result := make(map[string]keyActualTokenTotals, len(cycleStarts))
	for keyID, start := range cycleStarts {
		var total keyActualTokenTotals
		err := db.QueryRowContext(queryContext, `SELECT total_tokens,input_tokens,cached_tokens,output_tokens FROM key_actual_token_totals WHERE key_id=?`, keyID).
			Scan(&total.Total, &total.Input, &total.Cached, &total.Output)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if start != nil {
			if err := db.QueryRowContext(queryContext, `SELECT COALESCE(SUM(units),0) FROM usage_buckets WHERE scope='key_actual' AND scope_id=? AND bucket_at>?`, keyID, start.UTC().UnixMilli()).Scan(&total.Cycle); err != nil {
				return nil, err
			}
		}
		result[keyID] = total
	}
	return result, nil
}

func (store *Store) LoadCompletedTokenTotals(ctx context.Context) (TokenTotalsSnapshot, error) {
	db, queryContext, release := store.analysisReader(ctx)
	defer release()
	var result TokenTotalsSnapshot
	err := db.QueryRowContext(queryContext, `SELECT COALESCE(SUM(input_tokens),0),COALESCE(SUM(cached_tokens),0),COALESCE(SUM(output_tokens),0) FROM key_actual_token_totals`).
		Scan(&result.Input, &result.Cached, &result.Output)
	result.Available = err == nil
	return result, err
}

func (store *Store) AnalysisReaderDegraded() bool {
	store.analysisMu.RLock()
	defer store.analysisMu.RUnlock()
	return store.analysisReaderDegraded
}

type RetentionCleanupResult struct {
	UsageLogs       int64
	ForbiddenLogs   int64
	OperationalLogs int64
}

func (result RetentionCleanupResult) LogRows() int64 {
	return result.UsageLogs + result.ForbiddenLogs + result.OperationalLogs
}

// PruneRetention keeps aggregate-retention configuration independent from the
// fixed 30-day request/runtime log policy.
func (store *Store) PruneRetention(usageBefore, logBefore, analysisBefore time.Time) (RetentionCleanupResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var result RetentionCleanupResult
	tx, err := store.db.Begin()
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM usage_buckets WHERE bucket_at < ?`, usageBefore.UTC().UnixMilli()); err != nil {
		return result, err
	}
	logCutoff := logBefore.UTC().UnixMilli()
	deleted, err := tx.Exec(`DELETE FROM request_logs WHERE requested_at < ? AND reason='content_forbidden'`, logCutoff)
	if err != nil {
		return result, err
	}
	result.ForbiddenLogs, _ = deleted.RowsAffected()
	deleted, err = tx.Exec(`DELETE FROM request_logs WHERE requested_at < ?`, logCutoff)
	if err != nil {
		return result, err
	}
	result.UsageLogs, _ = deleted.RowsAffected()
	deleted, err = tx.Exec(`DELETE FROM operational_logs WHERE occurred_at < ?`, logCutoff)
	if err != nil {
		return result, err
	}
	result.OperationalLogs, _ = deleted.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM usage_analysis_buckets WHERE bucket_at < ?`, analysisBefore.UTC().UnixMilli()); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return RetentionCleanupResult{}, err
	}
	return result, nil
}
