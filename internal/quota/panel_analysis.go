package quota

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (engine *Engine) UsageRecords(keyID string, limit int) ([]UsageEvent, error) {
	if engine == nil {
		return nil, fmt.Errorf("codex-carpool is not initialized")
	}
	if err := engine.flushPending(); err != nil {
		return nil, err
	}
	return engine.store.ListUsageEvents(keyID, limit)
}

func (engine *Engine) UsageTrend(keyID string, now time.Time, window time.Duration, bins int) (UsageTrendSnapshot, error) {
	if engine == nil {
		return UsageTrendSnapshot{}, fmt.Errorf("codex-carpool is not initialized")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return UsageTrendSnapshot{}, fmt.Errorf("key_id is required")
	}
	if window != fiveHourWindow && window != sevenDayWindow {
		return UsageTrendSnapshot{}, fmt.Errorf("unsupported trend window")
	}
	if bins < 2 || bins > 48 {
		return UsageTrendSnapshot{}, fmt.Errorf("trend bins must be between 2 and 48")
	}
	if err := engine.flushPending(); err != nil {
		return UsageTrendSnapshot{}, err
	}
	now = now.UTC()
	from := now.Add(-window)
	points, err := engine.store.ListUsageTrend(keyID, from, window/time.Duration(bins))
	if err != nil {
		return UsageTrendSnapshot{}, err
	}
	return UsageTrendSnapshot{From: from, To: now, Points: points}, nil
}

// UsageAnalysis aggregates completed Key token usage by local calendar time.
func (engine *Engine) UsageAnalysis(keyID string, from, until time.Time, location *time.Location, granularity string) (UsageAnalysisSnapshot, error) {
	if engine == nil {
		return UsageAnalysisSnapshot{}, fmt.Errorf("codex-carpool is not initialized")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" || location == nil || !until.After(from) {
		return UsageAnalysisSnapshot{}, fmt.Errorf("valid key_id, timezone and date range are required")
	}
	granularity = strings.ToLower(strings.TrimSpace(granularity))
	if granularity == "" {
		granularity = "day"
	}
	if granularity != "hour" && granularity != "day" && granularity != "month" && granularity != "year" {
		return UsageAnalysisSnapshot{}, fmt.Errorf("analysis granularity must be hour, day, month, or year")
	}
	firstDay := analysisPeriodStart(from.In(location), "day", location)
	lastDay := analysisPeriodStart(until.Add(-time.Nanosecond).In(location), "day", location)
	if lastDay.After(firstDay.AddDate(0, 0, 365)) {
		return UsageAnalysisSnapshot{}, fmt.Errorf("analysis range must not exceed 366 days")
	}
	if granularity == "hour" && lastDay.After(firstDay.AddDate(0, 0, 30)) {
		return UsageAnalysisSnapshot{}, fmt.Errorf("hour granularity range must not exceed 31 days")
	}
	if engine.store.RestoreAnalysisReader() {
		engine.LogOperational("info", "usage_analysis_reader_restored", "年度用量分析已恢复为独立 SQLite 只读连接", "", keyID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), usageAnalysisQueryTimeout)
	defer cancel()
	buckets, availableFrom, err := engine.store.LoadUsageAnalysisSnapshot(ctx, keyID, from, until)
	if err != nil {
		return UsageAnalysisSnapshot{}, err
	}
	breakdown, err := engine.store.LoadUsageAnalysisBreakdown(ctx, keyID, from, until)
	if err != nil {
		return UsageAnalysisSnapshot{}, err
	}
	from, until = from.UTC(), until.UTC()
	points := make(map[time.Time]UsageAnalysisPoint)
	for _, bucket := range buckets {
		start := analysisPeriodStart(bucket.At.In(location), granularity, location)
		point := points[start]
		point.Start, point.Label = start.UTC(), analysisPeriodLabel(start, granularity)
		point.Units += bucket.Units
		point.RequestCount += bucket.RequestCount
		point.InputTokens += bucket.InputTokens
		point.CachedTokens += bucket.CachedTokens
		point.OutputTokens += bucket.OutputTokens
		point.InputCostMicros += bucket.InputCostMicros
		point.CachedCostMicros += bucket.CachedCostMicros
		point.OutputCostMicros += bucket.OutputCostMicros
		point.CostMicros += bucket.CostMicros
		points[start] = point
	}
	first := analysisPeriodStart(from.In(location), granularity, location)
	last := analysisPeriodStart(until.Add(-time.Nanosecond).In(location), granularity, location)
	result := UsageAnalysisSnapshot{From: from, To: until, Timezone: location.String(), Granularity: granularity,
		AvailableFrom: availableFrom, RetentionDays: int(usageAnalysisRetention / (24 * time.Hour)),
		Points: make([]UsageAnalysisPoint, 0), UsageAnalysisBreakdown: breakdown}
	for start := first; !start.After(last); start = nextAnalysisPeriod(start, granularity, location) {
		point, ok := points[start]
		if !ok {
			point = UsageAnalysisPoint{Start: start.UTC(), Label: analysisPeriodLabel(start, granularity)}
		}
		result.Points = append(result.Points, point)
		result.TotalTokens += point.Units
		result.RequestCount += point.RequestCount
	}
	return result, nil
}

// ModelUsageRanking aggregates every added Key by model for one local day.
func (engine *Engine) ModelUsageRanking(from, until time.Time, location *time.Location) (ModelUsageRankingSnapshot, error) {
	if engine == nil {
		return ModelUsageRankingSnapshot{}, fmt.Errorf("codex-carpool is not initialized")
	}
	if location == nil || !until.After(from) {
		return ModelUsageRankingSnapshot{}, fmt.Errorf("valid timezone and date range are required")
	}
	firstDay := analysisPeriodStart(from.In(location), "day", location)
	lastDay := analysisPeriodStart(until.Add(-time.Nanosecond).In(location), "day", location)
	if !firstDay.Equal(lastDay) {
		return ModelUsageRankingSnapshot{}, fmt.Errorf("model usage ranking requires one calendar day")
	}
	if err := engine.flushPending(); err != nil {
		return ModelUsageRankingSnapshot{}, err
	}
	if engine.store.RestoreAnalysisReader() {
		engine.LogOperational("info", "usage_analysis_reader_restored", "模型用量排行已恢复为独立 SQLite 只读连接", "", "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), usageAnalysisQueryTimeout)
	defer cancel()
	breakdown, err := engine.store.LoadGlobalUsageAnalysisBreakdown(ctx, from, until)
	if err != nil {
		return ModelUsageRankingSnapshot{}, err
	}
	result := ModelUsageRankingSnapshot{
		From:       from.UTC(),
		To:         until.UTC(),
		Timezone:   location.String(),
		CostMicros: breakdown.CostMicros,
		Models:     breakdown.Models,
	}
	for _, model := range result.Models {
		result.TotalTokens += model.TotalTokens
		result.RequestCount += model.RequestCount
	}
	return result, nil
}

func analysisPeriodStart(value time.Time, granularity string, location *time.Location) time.Time {
	value = value.In(location)
	switch granularity {
	case "hour":
		return value.Add(-time.Duration(value.Minute())*time.Minute - time.Duration(value.Second())*time.Second - time.Duration(value.Nanosecond()))
	case "year":
		return time.Date(value.Year(), time.January, 1, 0, 0, 0, 0, location)
	case "month":
		return time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, location)
	default:
		return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, location)
	}
}

func nextAnalysisPeriod(value time.Time, granularity string, location *time.Location) time.Time {
	switch granularity {
	case "hour":
		return value.Add(time.Hour).In(location)
	case "year":
		return value.AddDate(1, 0, 0).In(location)
	case "month":
		return value.AddDate(0, 1, 0).In(location)
	default:
		return value.AddDate(0, 0, 1).In(location)
	}
}

func analysisPeriodLabel(value time.Time, granularity string) string {
	switch granularity {
	case "hour":
		return value.Format("2006-01-02 15:00")
	case "year":
		return value.Format("2006")
	case "month":
		return value.Format("2006-01")
	default:
		return value.Format("2006-01-02")
	}
}

func (engine *Engine) DecisionLogs(keyID string, limit int) ([]DecisionLog, error) {
	if engine == nil {
		return nil, fmt.Errorf("codex-carpool is not initialized")
	}
	if err := engine.flushPending(); err != nil {
		return nil, err
	}
	return engine.store.ListDecisionLogs(keyID, limit)
}

func (engine *Engine) DecisionLogPage(keyID, decision, search string, page, pageSize int) (DecisionLogPage, error) {
	return engine.DecisionLogPageInRange(keyID, decision, search, time.Time{}, time.Time{}, page, pageSize)
}

// DecisionLogPageInRange applies optional inclusive UTC boundaries without
// changing the unfiltered log-page contract used by existing callers.
func (engine *Engine) DecisionLogPageInRange(keyID, decision, search string, from, to time.Time, page, pageSize int) (DecisionLogPage, error) {
	if engine == nil {
		return DecisionLogPage{}, fmt.Errorf("codex-carpool is not initialized")
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return DecisionLogPage{}, fmt.Errorf("log end time must not be before start time")
	}
	page, pageSize = normalizePage(page, pageSize)
	if err := engine.flushPending(); err != nil {
		return DecisionLogPage{}, err
	}
	items, total, err := engine.store.ListDecisionLogsPageInRange(keyID, decision, search, from, to, pageSize, (page-1)*pageSize)
	if err != nil {
		return DecisionLogPage{}, err
	}
	totalPages := pageCount(total, pageSize)
	if totalPages > 0 && page > totalPages {
		page = totalPages
		items, total, err = engine.store.ListDecisionLogsPageInRange(keyID, decision, search, from, to, pageSize, (page-1)*pageSize)
		if err != nil {
			return DecisionLogPage{}, err
		}
	}
	return DecisionLogPage{Logs: items, Page: page, PageSize: pageSize, Total: total, TotalPages: pageCount(total, pageSize)}, nil
}

func normalizePage(page, pageSize int) (int, int) {
	if pageSize <= 0 {
		pageSize = 20
	} else if pageSize > 100 {
		pageSize = 100
	}
	if page <= 0 {
		page = 1
	} else if page > 1_000_000 {
		page = 1_000_000
	}
	return page, pageSize
}

func pageCount(total, pageSize int) int {
	if total <= 0 {
		return 0
	}
	return (total + pageSize - 1) / pageSize
}

func (engine *Engine) ClearDecisionLogs(keyID string) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	if err := engine.flushPending(); err != nil {
		return fmt.Errorf("flush decision logs before clear: %w", err)
	}
	return engine.store.ClearDecisionLogs(strings.TrimSpace(keyID))
}

func (engine *Engine) ClearForbiddenDecisionLogs(keyID string) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	if err := engine.flushPending(); err != nil {
		return fmt.Errorf("flush forbidden-content logs before clear: %w", err)
	}
	return engine.store.ClearForbiddenDecisionLogs(strings.TrimSpace(keyID))
}

func (engine *Engine) OperationalLogs(level, query string, limit int) ([]OperationalLog, error) {
	if engine == nil {
		return nil, fmt.Errorf("codex-carpool is not initialized")
	}
	return engine.store.ListOperationalLogs(level, query, limit)
}

func (engine *Engine) OperationalLogPage(level, query string, page, pageSize int) (OperationalLogPage, error) {
	if engine == nil {
		return OperationalLogPage{}, fmt.Errorf("codex-carpool is not initialized")
	}
	page, pageSize = normalizePage(page, pageSize)
	items, total, err := engine.store.ListOperationalLogsPage(level, query, pageSize, (page-1)*pageSize)
	if err != nil {
		return OperationalLogPage{}, err
	}
	totalPages := pageCount(total, pageSize)
	if totalPages > 0 && page > totalPages {
		page = totalPages
		items, total, err = engine.store.ListOperationalLogsPage(level, query, pageSize, (page-1)*pageSize)
		if err != nil {
			return OperationalLogPage{}, err
		}
	}
	return OperationalLogPage{Logs: items, Page: page, PageSize: pageSize, Total: total, TotalPages: pageCount(total, pageSize)}, nil
}

func (engine *Engine) ClearOperationalLogs() error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	return engine.store.ClearOperationalLogs()
}

func (engine *Engine) LogOperational(level, event, message, authID, keyID string) {
	if engine == nil || engine.usageClosed.Load() {
		return
	}
	_ = engine.store.AppendOperationalLog(OperationalLog{OccurredAt: time.Now().UTC(), Level: level,
		Event: event, Message: message, AuthID: authID, KeyID: keyID})
}

func (engine *Engine) AnalysisReaderDegraded() bool {
	return engine != nil && engine.store != nil && engine.store.AnalysisReaderDegraded()
}

func (engine *Engine) Models() ([]ModelCatalogEntry, error) { return engine.store.ListModelCatalog() }

func (engine *Engine) ReplaceModels(models []ModelCatalogEntry) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	seen := make(map[string]struct{}, len(models))
	normalized := make([]ModelCatalogEntry, 0, len(models))
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			continue
		}
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		model.DisplayName = strings.TrimSpace(model.DisplayName)
		if model.DisplayName == "" {
			model.DisplayName = model.ID
		}
		model.Owner = strings.TrimSpace(model.Owner)
		model.Available = true
		normalized = append(normalized, model)
	}
	if len(normalized) == 0 {
		return fmt.Errorf("refuse to replace the CPA model catalog with an empty result")
	}
	return engine.store.ReplaceModelCatalog(normalized, time.Now().UTC())
}
