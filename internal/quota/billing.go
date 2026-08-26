package quota

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const usdMicrosPerDollar int64 = 1_000_000

// defaultModelRates is written only for a brand-new, unconfigured rate card.
// Operators retain full control once any rate exists in SQLite. Codex Spark has
// no separately published API rate, so its seed intentionally follows the
// published GPT-5.3-Codex text-token rate until an operator changes it.
// GPT Image 1.5's per-image charges cannot be represented by the current
// Token callback, so its seed covers reported text Tokens only.
// GPT Image 2 is an explicit operator-requested zero-rate exception.
var defaultModelRates = []ModelRate{
	{Model: "gpt-5.3-codex-spark", InputUSDPerMillion: 1.75, CacheReadUSDPerMillion: 0.175, ReasoningUsesOutput: true, OutputUSDPerMillion: 14},
	{Model: "gpt-5.4-mini", InputUSDPerMillion: 0.75, CacheReadUSDPerMillion: 0.075, ReasoningUsesOutput: true, OutputUSDPerMillion: 4.5},
	{Model: "gpt-5.6-sol", InputUSDPerMillion: 5, CacheReadUSDPerMillion: 0.5, CacheWriteUSDPerMillion: 6.25, ReasoningUsesOutput: true, OutputUSDPerMillion: 30},
	{Model: "gpt-5.6-luna", InputUSDPerMillion: 0.2, CacheReadUSDPerMillion: 0.02, CacheWriteUSDPerMillion: 0.25, ReasoningUsesOutput: true, OutputUSDPerMillion: 1.2},
	{Model: "gpt-image-1.5", InputUSDPerMillion: 5, CacheReadUSDPerMillion: 1.25, OutputUSDPerMillion: 10},
	{Model: "gpt-image-2"},
}

type ModelRateTier struct {
	ContextOverTokens          int64   `json:"context_over_tokens"`
	InputUSDPerMillion         float64 `json:"input_usd_per_million"`
	CacheReadUSDPerMillion     float64 `json:"cache_read_usd_per_million"`
	CacheWriteUSDPerMillion    float64 `json:"cache_write_usd_per_million"`
	ReasoningUSDPerMillion     float64 `json:"reasoning_usd_per_million"`
	ReasoningUsesOutput        bool    `json:"reasoning_uses_output,omitempty"`
	OutputUSDPerMillion        float64 `json:"output_usd_per_million"`
	inputMicrosPerMillion      int64
	cacheReadMicrosPerMillion  int64
	cacheWriteMicrosPerMillion int64
	reasoningMicrosPerMillion  int64
	outputMicrosPerMillion     int64
}

type ModelRateMode struct {
	Name                       string  `json:"name"`
	ServiceTier                string  `json:"service_tier"`
	InputUSDPerMillion         float64 `json:"input_usd_per_million"`
	CacheReadUSDPerMillion     float64 `json:"cache_read_usd_per_million"`
	CacheWriteUSDPerMillion    float64 `json:"cache_write_usd_per_million"`
	ReasoningUSDPerMillion     float64 `json:"reasoning_usd_per_million"`
	ReasoningUsesOutput        bool    `json:"reasoning_uses_output,omitempty"`
	OutputUSDPerMillion        float64 `json:"output_usd_per_million"`
	inputMicrosPerMillion      int64
	cacheReadMicrosPerMillion  int64
	cacheWriteMicrosPerMillion int64
	reasoningMicrosPerMillion  int64
	outputMicrosPerMillion     int64
}

// ModelRate is the operator-maintained rate card. Values are US dollars per
// one million Tokens; integer micro-dollars are used internally for all
// accounting so a fixed-cycle budget never depends on floating-point rounding.
type ModelRate struct {
	Model                      string          `json:"model"`
	Provider                   string          `json:"provider,omitempty"`
	Source                     string          `json:"source,omitempty"`
	InputUSDPerMillion         float64         `json:"input_usd_per_million"`
	CacheReadUSDPerMillion     float64         `json:"cache_read_usd_per_million"`
	CacheWriteUSDPerMillion    float64         `json:"cache_write_usd_per_million"`
	ReasoningUSDPerMillion     float64         `json:"reasoning_usd_per_million"`
	ReasoningUsesOutput        bool            `json:"reasoning_uses_output,omitempty"`
	OutputUSDPerMillion        float64         `json:"output_usd_per_million"`
	Tiers                      []ModelRateTier `json:"tiers,omitempty"`
	Modes                      []ModelRateMode `json:"modes,omitempty"`
	UpdatedAt                  time.Time       `json:"updated_at"`
	inputMicrosPerMillion      int64
	cacheReadMicrosPerMillion  int64
	cacheWriteMicrosPerMillion int64
	reasoningMicrosPerMillion  int64
	outputMicrosPerMillion     int64
}

// DollarWindowSnapshot is a Key-owned fixed spend cycle. It intentionally
// has no account or official percentage field: traffic outside CPA is never
// assigned to a managed Key.
type DollarWindowSnapshot struct {
	Limited      bool       `json:"limited"`
	BudgetUSD    float64    `json:"budget_usd"`
	SpentUSD     float64    `json:"spent_usd"`
	RemainingUSD float64    `json:"remaining_usd"`
	RefreshAt    *time.Time `json:"refresh_at,omitempty"`
	CoolingUntil *time.Time `json:"cooling_until,omitempty"`
}

type DollarSpendSnapshot struct {
	FiveHour DollarWindowSnapshot `json:"five_hour"`
	SevenDay DollarWindowSnapshot `json:"seven_day"`
}

type budgetCycleState struct {
	FiveHourStartedAt time.Time
	SevenDayStartedAt time.Time
}

func normalizeModelRate(rate ModelRate) (ModelRate, error) {
	rate.Model = strings.TrimSpace(rate.Model)
	if rate.Model == "" {
		return ModelRate{}, fmt.Errorf("model is required")
	}
	values := []struct {
		name  string
		value float64
		set   *int64
	}{
		{"input_usd_per_million", rate.InputUSDPerMillion, &rate.inputMicrosPerMillion},
		{"cache_read_usd_per_million", rate.CacheReadUSDPerMillion, &rate.cacheReadMicrosPerMillion},
		{"cache_write_usd_per_million", rate.CacheWriteUSDPerMillion, &rate.cacheWriteMicrosPerMillion},
		{"reasoning_usd_per_million", rate.ReasoningUSDPerMillion, &rate.reasoningMicrosPerMillion},
		{"output_usd_per_million", rate.OutputUSDPerMillion, &rate.outputMicrosPerMillion},
	}
	for _, item := range values {
		if math.IsNaN(item.value) || math.IsInf(item.value, 0) || item.value < 0 {
			return ModelRate{}, fmt.Errorf("%s must be a non-negative finite number", item.name)
		}
		micros := item.value * float64(usdMicrosPerDollar)
		if micros > float64(math.MaxInt64) {
			return ModelRate{}, fmt.Errorf("%s is too large", item.name)
		}
		*item.set = int64(math.Round(micros))
	}
	rate.InputUSDPerMillion = float64(rate.inputMicrosPerMillion) / float64(usdMicrosPerDollar)
	rate.CacheReadUSDPerMillion = float64(rate.cacheReadMicrosPerMillion) / float64(usdMicrosPerDollar)
	rate.CacheWriteUSDPerMillion = float64(rate.cacheWriteMicrosPerMillion) / float64(usdMicrosPerDollar)
	rate.ReasoningUSDPerMillion = float64(rate.reasoningMicrosPerMillion) / float64(usdMicrosPerDollar)
	rate.OutputUSDPerMillion = float64(rate.outputMicrosPerMillion) / float64(usdMicrosPerDollar)
	for index := range rate.Tiers {
		if rate.Tiers[index].ContextOverTokens < 0 {
			return ModelRate{}, fmt.Errorf("tier context_over_tokens must be non-negative")
		}
		if err := normalizeRatePrices("tier", rate.Tiers[index].InputUSDPerMillion, rate.Tiers[index].CacheReadUSDPerMillion, rate.Tiers[index].CacheWriteUSDPerMillion, rate.Tiers[index].ReasoningUSDPerMillion, rate.Tiers[index].OutputUSDPerMillion,
			&rate.Tiers[index].inputMicrosPerMillion, &rate.Tiers[index].cacheReadMicrosPerMillion, &rate.Tiers[index].cacheWriteMicrosPerMillion, &rate.Tiers[index].reasoningMicrosPerMillion, &rate.Tiers[index].outputMicrosPerMillion); err != nil {
			return ModelRate{}, err
		}
	}
	sort.Slice(rate.Tiers, func(left, right int) bool {
		return rate.Tiers[left].ContextOverTokens < rate.Tiers[right].ContextOverTokens
	})
	for index := range rate.Modes {
		rate.Modes[index].Name = strings.TrimSpace(rate.Modes[index].Name)
		rate.Modes[index].ServiceTier = strings.TrimSpace(rate.Modes[index].ServiceTier)
		if rate.Modes[index].Name == "" || rate.Modes[index].ServiceTier == "" {
			return ModelRate{}, fmt.Errorf("rate mode name and service_tier are required")
		}
		if err := normalizeRatePrices("mode", rate.Modes[index].InputUSDPerMillion, rate.Modes[index].CacheReadUSDPerMillion, rate.Modes[index].CacheWriteUSDPerMillion, rate.Modes[index].ReasoningUSDPerMillion, rate.Modes[index].OutputUSDPerMillion,
			&rate.Modes[index].inputMicrosPerMillion, &rate.Modes[index].cacheReadMicrosPerMillion, &rate.Modes[index].cacheWriteMicrosPerMillion, &rate.Modes[index].reasoningMicrosPerMillion, &rate.Modes[index].outputMicrosPerMillion); err != nil {
			return ModelRate{}, err
		}
	}
	return rate, nil
}

func normalizeRatePrices(prefix string, input, cacheRead, cacheWrite, reasoning, output float64, inputMicros, cacheReadMicros, cacheWriteMicros, reasoningMicros, outputMicros *int64) error {
	items := []struct {
		name  string
		value float64
		set   *int64
	}{
		{"input", input, inputMicros}, {"cache_read", cacheRead, cacheReadMicros}, {"cache_write", cacheWrite, cacheWriteMicros}, {"reasoning", reasoning, reasoningMicros}, {"output", output, outputMicros},
	}
	for _, item := range items {
		if math.IsNaN(item.value) || math.IsInf(item.value, 0) || item.value < 0 || item.value*float64(usdMicrosPerDollar) > float64(math.MaxInt64) {
			return fmt.Errorf("%s %s price must be a non-negative finite number", prefix, item.name)
		}
		*item.set = int64(math.Round(item.value * float64(usdMicrosPerDollar)))
	}
	return nil
}

func rateFromStored(model string, input, cached, output int64, updatedAt time.Time) ModelRate {
	return ModelRate{
		Model: model, InputUSDPerMillion: float64(input) / float64(usdMicrosPerDollar),
		CacheReadUSDPerMillion: float64(cached) / float64(usdMicrosPerDollar), CacheWriteUSDPerMillion: float64(cached) / float64(usdMicrosPerDollar),
		ReasoningUsesOutput: true, OutputUSDPerMillion: float64(output) / float64(usdMicrosPerDollar),
		UpdatedAt: updatedAt, inputMicrosPerMillion: input, cacheReadMicrosPerMillion: cached, cacheWriteMicrosPerMillion: cached, outputMicrosPerMillion: output,
	}
}

func dollarBudgetMicros(value float64) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, fmt.Errorf("dollar budgets must be non-negative finite numbers")
	}
	micros := value * float64(usdMicrosPerDollar)
	if micros > float64(math.MaxInt64) {
		return 0, fmt.Errorf("dollar budget is too large")
	}
	return int64(math.Round(micros)), nil
}

type costBreakdown struct {
	Input      int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	Cached     int64
	Output     int64
	Total      int64
}

func costBreakdownMicros(rate ModelRate, inputTokens, cachedTokens, outputTokens int64) costBreakdown {
	// CPA reports cached input as part of the input total. Bill only the
	// uncached remainder at the normal input rate, then price cache once at the
	// operator-maintained cache-read rate.
	uncachedInput := inputTokens - cachedTokens
	if uncachedInput < 0 {
		uncachedInput = 0
	}
	return costBreakdownFromBillableMicros(rate, uncachedInput, cachedTokens, 0, 0, outputTokens)
}

func costBreakdownFromBillableMicros(rate ModelRate, inputTokens, cacheReadTokens, cacheWriteTokens, reasoningTokens, outputTokens int64) costBreakdown {
	reasoningRate := rate.reasoningMicrosPerMillion
	if rate.ReasoningUsesOutput {
		reasoningRate = rate.outputMicrosPerMillion
	}
	parts := []struct {
		tokens int64
		rate   int64
		value  *int64
	}{
		{inputTokens, rate.inputMicrosPerMillion, nil},
		{cacheReadTokens, rate.cacheReadMicrosPerMillion, nil},
		{cacheWriteTokens, rate.cacheWriteMicrosPerMillion, nil},
		{reasoningTokens, reasoningRate, nil},
		{outputTokens, rate.outputMicrosPerMillion, nil},
	}
	result := costBreakdown{}
	parts[0].value = &result.Input
	parts[1].value = &result.CacheRead
	parts[2].value = &result.CacheWrite
	parts[3].value = &result.Reasoning
	parts[4].value = &result.Output
	for _, part := range parts {
		if part.tokens <= 0 || part.rate <= 0 {
			continue
		}
		value := (float64(part.tokens) * float64(part.rate)) / 1_000_000
		if value >= float64(math.MaxInt64-result.Total) {
			result.Total = math.MaxInt64
			*part.value = math.MaxInt64
			return result
		}
		*part.value = int64(math.Round(value))
		result.Total += *part.value
	}
	result.Cached = nonNegativeTokenSum(result.CacheRead, result.CacheWrite)
	return result
}

func costMicros(rate ModelRate, inputTokens, cachedTokens, outputTokens int64) int64 {
	return costBreakdownMicros(rate, inputTokens, cachedTokens, outputTokens).Total
}

func cachedUsageTokens(record CompletedUsage) int64 {
	if record.CacheReadTokens > 0 || record.CacheCreationTokens > 0 {
		return nonNegativeTokenSum(record.CacheReadTokens, record.CacheCreationTokens)
	}
	return max(record.CachedTokens, 0)
}

type normalizedUsageTokens struct {
	Input      int64
	CacheRead  int64
	CacheWrite int64
	Cached     int64
	Reasoning  int64
	Output     int64
}

func nonNegativeTokenSum(left, right int64) int64 {
	left, right = max(left, 0), max(right, 0)
	if left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func normalizedBillableUsage(record CompletedUsage) normalizedUsageTokens {
	provider := strings.ToLower(strings.TrimSpace(record.Provider + " " + record.ExecutorType + " " + record.Model))
	cacheRead, cacheWrite := max(record.CacheReadTokens, 0), max(record.CacheCreationTokens, 0)
	if cacheRead == 0 && cacheWrite == 0 {
		cacheRead = max(record.CachedTokens, 0)
	}
	cached := nonNegativeTokenSum(cacheRead, cacheWrite)
	input, output := max(record.InputTokens, 0), max(record.OutputTokens, 0)
	independentCache := strings.Contains(provider, "claude") || strings.Contains(provider, "anthropic")
	separateReasoning := independentCache
	reasoningIncludedInOutput := false
	for _, marker := range []string{"openai", "codex", "gpt-", "xai", "grok", "kimi", "qwen", "deepseek", "openrouter"} {
		if strings.Contains(provider, marker) {
			reasoningIncludedInOutput = true
			break
		}
	}
	for _, marker := range []string{"gemini", "aistudio", "antigravity", "vertex", "interaction"} {
		if strings.Contains(provider, marker) {
			separateReasoning = true
			break
		}
	}
	if !independentCache {
		input -= cached
		if input < 0 {
			input = 0
		}
	}
	reasoning := int64(0)
	if separateReasoning {
		reasoning = max(record.ReasoningTokens, 0)
	} else if reasoningIncludedInOutput {
		// OpenAI-compatible providers report reasoning as an output subset.
		// Split it out so a dedicated rate never charges the same tokens again.
		reasoning = min(max(record.ReasoningTokens, 0), output)
		output -= reasoning
	}
	return normalizedUsageTokens{Input: input, CacheRead: cacheRead, CacheWrite: cacheWrite, Cached: cached, Reasoning: reasoning, Output: output}
}

func costBreakdownForUsage(rate ModelRate, record CompletedUsage) (costBreakdown, normalizedUsageTokens) {
	tokens := normalizedBillableUsage(record)
	rate = rateForCompletedUsage(rate, record, tokens)
	return costBreakdownFromBillableMicros(rate, tokens.Input, tokens.CacheRead, tokens.CacheWrite, tokens.Reasoning, tokens.Output), tokens
}

func rateForCompletedUsage(rate ModelRate, record CompletedUsage, tokens normalizedUsageTokens) ModelRate {
	contextTokens := nonNegativeTokenSum(tokens.Input, tokens.Cached)
	for _, tier := range rate.Tiers {
		if contextTokens < tier.ContextOverTokens {
			continue
		}
		rate.inputMicrosPerMillion, rate.cacheReadMicrosPerMillion = tier.inputMicrosPerMillion, tier.cacheReadMicrosPerMillion
		rate.cacheWriteMicrosPerMillion, rate.reasoningMicrosPerMillion = tier.cacheWriteMicrosPerMillion, tier.reasoningMicrosPerMillion
		rate.ReasoningUsesOutput = tier.ReasoningUsesOutput
		rate.outputMicrosPerMillion = tier.outputMicrosPerMillion
	}
	serviceTier := strings.ToLower(strings.TrimSpace(record.ServiceTier))
	// CPA does not expose a separately verified response tier to plugins. An
	// automatic request therefore uses the base rate instead of a premium mode.
	if serviceTier == "auto" {
		serviceTier = ""
	}
	for _, mode := range rate.Modes {
		if strings.EqualFold(mode.ServiceTier, serviceTier) {
			rate.inputMicrosPerMillion, rate.cacheReadMicrosPerMillion = mode.inputMicrosPerMillion, mode.cacheReadMicrosPerMillion
			rate.cacheWriteMicrosPerMillion, rate.reasoningMicrosPerMillion = mode.cacheWriteMicrosPerMillion, mode.reasoningMicrosPerMillion
			rate.ReasoningUsesOutput = mode.ReasoningUsesOutput
			rate.outputMicrosPerMillion = mode.outputMicrosPerMillion
			break
		}
	}
	return rate
}

func modelRateHasCost(rate ModelRate) bool {
	if rate.inputMicrosPerMillion > 0 || rate.cacheReadMicrosPerMillion > 0 || rate.cacheWriteMicrosPerMillion > 0 ||
		rate.reasoningMicrosPerMillion > 0 || rate.outputMicrosPerMillion > 0 {
		return true
	}
	for _, tier := range rate.Tiers {
		if tier.inputMicrosPerMillion > 0 || tier.cacheReadMicrosPerMillion > 0 || tier.cacheWriteMicrosPerMillion > 0 || tier.reasoningMicrosPerMillion > 0 || tier.outputMicrosPerMillion > 0 {
			return true
		}
	}
	for _, mode := range rate.Modes {
		if mode.inputMicrosPerMillion > 0 || mode.cacheReadMicrosPerMillion > 0 || mode.cacheWriteMicrosPerMillion > 0 || mode.reasoningMicrosPerMillion > 0 || mode.outputMicrosPerMillion > 0 {
			return true
		}
	}
	return false
}

func fixedWindowSnapshot(state *keyMeterState, now time.Time, budget int64, startedAt time.Time, window time.Duration) DollarWindowSnapshot {
	result := DollarWindowSnapshot{Limited: budget > 0, BudgetUSD: float64(budget) / float64(usdMicrosPerDollar)}
	now, startedAt = now.UTC(), startedAt.UTC()
	if startedAt.IsZero() || !now.Before(startedAt.Add(window)) {
		return result
	}
	refreshAt := startedAt.Add(window).UTC()
	result.RefreshAt = &refreshAt
	if state == nil {
		if result.Limited {
			result.RemainingUSD = result.BudgetUSD
		}
		return result
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.completed.prune(now)
	spent := int64(0)
	for _, event := range state.completed.events {
		if event.At.Before(startedAt) {
			continue
		}
		if !event.At.Before(refreshAt) {
			break
		}
		if event.Units > 0 && spent <= math.MaxInt64-event.Units {
			spent += event.Units
		} else if event.Units > 0 {
			spent = math.MaxInt64
			break
		}
	}
	result.SpentUSD = float64(spent) / float64(usdMicrosPerDollar)
	// An empty or zero budget keeps the fixed cycle observable but never blocks.
	if !result.Limited {
		return result
	}
	remaining := budget - spent
	if remaining > 0 {
		result.RemainingUSD = float64(remaining) / float64(usdMicrosPerDollar)
		return result
	}
	result.RemainingUSD = 0
	result.CoolingUntil = &refreshAt
	return result
}

func cycleActive(startedAt, now time.Time, window time.Duration) bool {
	return !startedAt.IsZero() && now.UTC().Before(startedAt.UTC().Add(window))
}

func (engine *Engine) budgetCycles(keyID string) budgetCycleState {
	engine.cyclesMu.RLock()
	cycle := engine.cycles[keyID]
	engine.cyclesMu.RUnlock()
	return cycle
}

// ensureBudgetCycles starts each inactive window at the first admitted request.
// Later requests never move an active cycle's refresh boundary.
func (engine *Engine) ensureBudgetCycles(keyID string, now time.Time) error {
	now = now.UTC().Truncate(time.Millisecond)
	engine.cyclesMu.Lock()
	defer engine.cyclesMu.Unlock()
	cycle := engine.cycles[keyID]
	changed := false
	if !cycleActive(cycle.FiveHourStartedAt, now, fiveHourWindow) {
		cycle.FiveHourStartedAt = now
		changed = true
	}
	if !cycleActive(cycle.SevenDayStartedAt, now, sevenDayWindow) {
		cycle.SevenDayStartedAt = now
		changed = true
	}
	if !changed {
		return nil
	}
	if err := engine.store.UpsertBudgetCycles(keyID, cycle); err != nil {
		return err
	}
	engine.cycles[keyID] = cycle
	return nil
}

func laterCoolingUntil(left, right *time.Time) *time.Time {
	if left == nil {
		return right
	}
	if right == nil || left.After(*right) {
		return left
	}
	return right
}

func sortedModelRates(rates map[string]ModelRate) []ModelRate {
	items := make([]ModelRate, 0, len(rates))
	for _, rate := range rates {
		if strings.TrimSpace(rate.Source) == "" {
			rate.Source = "manual"
		}
		items = append(items, rate)
	}
	sort.Slice(items, func(left, right int) bool { return items[left].Model < items[right].Model })
	return items
}

func (engine *Engine) keySpendState(keyID string) *keyMeterState {
	engine.statesMu.RLock()
	state := engine.states.spend[keyID]
	engine.statesMu.RUnlock()
	if state != nil {
		return state
	}
	engine.statesMu.Lock()
	defer engine.statesMu.Unlock()
	if state = engine.states.spend[keyID]; state == nil {
		state = newKeyMeterState(nil, time.Now().UTC())
		engine.states.spend[keyID] = state
	}
	return state
}

func (engine *Engine) modelRate(model string) (ModelRate, bool) {
	engine.ratesMu.RLock()
	rate, exists := engine.modelRates[strings.TrimSpace(model)]
	engine.ratesMu.RUnlock()
	return rate, exists
}

func (engine *Engine) ModelRates() []ModelRate {
	if engine == nil {
		return nil
	}
	engine.ratesMu.RLock()
	copy := make(map[string]ModelRate, len(engine.modelRates))
	for model, rate := range engine.modelRates {
		copy[model] = rate
	}
	engine.ratesMu.RUnlock()
	return sortedModelRates(copy)
}

func (engine *Engine) ReplaceModelRates(rates []ModelRate) ([]ModelRate, error) {
	if engine == nil {
		return nil, fmt.Errorf("codex-carpool is not initialized")
	}
	engine.rateSyncRunMu.Lock()
	defer engine.rateSyncRunMu.Unlock()
	normalized := make([]ModelRate, 0, len(rates))
	next := make(map[string]ModelRate, len(rates))
	updatedAt := time.Now().UTC()
	for _, rate := range rates {
		if strings.TrimSpace(rate.Source) == "" {
			rate.Source = "manual"
		}
		rate, err := normalizeModelRate(rate)
		if err != nil {
			return nil, err
		}
		if _, duplicate := next[rate.Model]; duplicate {
			return nil, fmt.Errorf("model rate %q is duplicated", rate.Model)
		}
		rate.UpdatedAt = updatedAt
		next[rate.Model] = rate
		normalized = append(normalized, rate)
	}
	if err := engine.store.ReplaceModelRates(normalized); err != nil {
		return nil, err
	}
	engine.ratesMu.Lock()
	engine.modelRates = next
	engine.ratesMu.Unlock()
	return engine.ModelRates(), nil
}

func (engine *Engine) dollarSpendSnapshot(policy KeyPolicy, now time.Time) DollarSpendSnapshot {
	fiveBudget, _ := dollarBudgetMicros(policy.FiveHourBudgetUSD)
	sevenBudget, _ := dollarBudgetMicros(policy.SevenDayBudgetUSD)
	state := engine.keySpendState(policy.ID)
	cycle := engine.budgetCycles(policy.ID)
	return DollarSpendSnapshot{
		FiveHour: fixedWindowSnapshot(state, now, fiveBudget, cycle.FiveHourStartedAt, fiveHourWindow),
		SevenDay: fixedWindowSnapshot(state, now, sevenBudget, cycle.SevenDayStartedAt, sevenDayWindow),
	}
}

func (engine *Engine) dollarBudgetCoolingUntil(policy KeyPolicy, now time.Time) *time.Time {
	snapshot := engine.dollarSpendSnapshot(policy, now)
	return laterCoolingUntil(snapshot.FiveHour.CoolingUntil, snapshot.SevenDay.CoolingUntil)
}

// chargeDollarSpend records settled cost at the original customer request time.
// Token totals arrive only in CPA's terminal callback, but that callback must
// not shift either fixed budget cycle later than the request itself.
func (engine *Engine) chargeDollarSpend(keyID string, at time.Time, micros int64) bool {
	if micros <= 0 {
		return true
	}
	at = at.UTC().Truncate(time.Millisecond)
	state := engine.keySpendState(keyID)
	state.mu.Lock()
	state.completed.prune(at)
	state.completed.addEvent(at, micros)
	state.mu.Unlock()
	return engine.enqueueBuckets(UsageEvent{
		Scope: "key_cost", KeyID: keyID, Model: "aggregate", RecordedAt: at,
		Units: micros, RequestCount: 1, MeteredBy: "model_rate_card",
	})
}
