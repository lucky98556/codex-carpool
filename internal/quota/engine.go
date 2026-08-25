package quota

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	fiveHourWindow = 5 * time.Hour
	sevenDayWindow = 7 * 24 * time.Hour

	usageBucketWindow        = time.Minute
	persistenceFlushInterval = time.Second
	usageCallbackDedupeTTL   = 15 * time.Minute
	pendingRequestTTL        = 24 * time.Hour
	maxRecentUsageCallbacks  = 20_000
	maxPendingBuckets        = 20_000
	maxPendingDecisionLogs   = 20_000
	maxPendingAge            = 10 * time.Second
	pendingShardCount        = 32
)

// Admission is the scheduler decision for one downstream Key request.
type Admission struct {
	Allowed bool
	Bypass  bool
	AuthID  string
	Code    string
	Message string
	KeyID   string
	RetryAt *time.Time
}

// UsageEvent is one bounded, durable aggregate derived from CPA's terminal
// callback. Only registered-Key Token and dollar scopes are accepted.
type UsageEvent struct {
	ID               int64     `json:"id"`
	Scope            string    `json:"-"`
	KeyID            string    `json:"key_id"`
	AuthID           string    `json:"auth_id"`
	Model            string    `json:"model"`
	RequestedAt      time.Time `json:"requested_at"`
	RecordedAt       time.Time `json:"recorded_at"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	ReasoningTokens  int64     `json:"reasoning_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	InputCostMicros  int64     `json:"input_cost_micros"`
	CachedCostMicros int64     `json:"cached_cost_micros"`
	OutputCostMicros int64     `json:"output_cost_micros"`
	CostMicros       int64     `json:"cost_micros"`
	Units            int64     `json:"units"`
	RequestCount     int64     `json:"request_count"`
	MeteredBy        string    `json:"metered_by"`
	Failed           bool      `json:"failed"`
	FailureStatus    int       `json:"failure_status"`
}

// DecisionLog is a bounded audit event. RequestContent contains only the last
// user-authored excerpt captured by the request interceptor.
type DecisionLog struct {
	ID               int64     `json:"id"`
	KeyID            string    `json:"key_id"`
	KeySuffix        string    `json:"key_suffix,omitempty"`
	AuthID           string    `json:"auth_id"`
	Model            string    `json:"model"`
	RequestContent   string    `json:"request_content,omitempty"`
	MatchedTerm      string    `json:"matched_term,omitempty"`
	MatchedCategory  string    `json:"matched_category,omitempty"`
	RequestedAt      time.Time `json:"requested_at"`
	Decision         string    `json:"decision"`
	StatusCode       int       `json:"status_code"`
	Reason           string    `json:"reason"`
	Units            int64     `json:"units"`
	InputTokens      int64     `json:"input_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	InputCostMicros  int64     `json:"input_cost_micros"`
	CachedCostMicros int64     `json:"cached_cost_micros"`
	OutputCostMicros int64     `json:"output_cost_micros"`
	CostMicros       int64     `json:"cost_micros"`
}

type OperationalLog struct {
	ID         int64     `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	Level      string    `json:"level"`
	Event      string    `json:"event"`
	Message    string    `json:"message"`
	AuthID     string    `json:"auth_id,omitempty"`
	KeyID      string    `json:"key_id,omitempty"`
}

// ModelCatalogEntry is synchronized from CPA by the management panel.
type ModelCatalogEntry struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Owner       string    `json:"owner"`
	Available   bool      `json:"available"`
	SyncedAt    time.Time `json:"synced_at"`
}

type meterEvent struct {
	At    time.Time
	Units int64
}

type meterState struct {
	mu          sync.Mutex
	events      []meterEvent
	fiveStart   int
	fiveUnits   int64
	weeklyUnits int64
}

type keyMeterState struct {
	mu        sync.Mutex
	completed *meterState
}

func newKeyMeterState(events []meterEvent, now time.Time) *keyMeterState {
	return &keyMeterState{completed: newMeterState(events, now)}
}

func newMeterState(events []meterEvent, now time.Time) *meterState {
	state := &meterState{}
	sort.Slice(events, func(left, right int) bool { return events[left].At.Before(events[right].At) })
	for _, event := range events {
		if !event.At.After(now.Add(-sevenDayWindow)) {
			continue
		}
		if count := len(state.events); count > 0 && state.events[count-1].At.Equal(event.At) {
			state.events[count-1].Units += event.Units
		} else {
			state.events = append(state.events, event)
		}
		state.weeklyUnits += event.Units
	}
	for state.fiveStart < len(state.events) && !state.events[state.fiveStart].At.After(now.Add(-fiveHourWindow)) {
		state.fiveStart++
	}
	for index := state.fiveStart; index < len(state.events); index++ {
		state.fiveUnits += state.events[index].Units
	}
	return state
}

func (state *meterState) prune(now time.Time) {
	sevenCutoff := now.Add(-sevenDayWindow)
	drop := 0
	for drop < len(state.events) && !state.events[drop].At.After(sevenCutoff) {
		state.weeklyUnits -= state.events[drop].Units
		drop++
	}
	if drop > 0 {
		state.events = append([]meterEvent(nil), state.events[drop:]...)
		state.fiveStart -= drop
		if state.fiveStart < 0 {
			state.fiveStart = 0
		}
	}
	fiveCutoff := now.Add(-fiveHourWindow)
	for state.fiveStart < len(state.events) && !state.events[state.fiveStart].At.After(fiveCutoff) {
		state.fiveUnits -= state.events[state.fiveStart].Units
		state.fiveStart++
	}
}

func (state *meterState) addEvent(at time.Time, units int64) {
	if count := len(state.events); count > 0 {
		last := &state.events[count-1]
		if last.At.Equal(at) {
			last.Units += units
			state.weeklyUnits += units
			state.fiveUnits += units
			return
		}
		if last.At.Before(at) {
			state.events = append(state.events, meterEvent{At: at, Units: units})
			state.weeklyUnits += units
			state.fiveUnits += units
			return
		}
	}
	index := sort.Search(len(state.events), func(index int) bool { return !state.events[index].At.Before(at) })
	if index < len(state.events) && state.events[index].At.Equal(at) {
		state.events[index].Units += units
	} else {
		state.events = append(state.events, meterEvent{})
		copy(state.events[index+1:], state.events[index:])
		state.events[index] = meterEvent{At: at, Units: units}
		if index < state.fiveStart {
			state.fiveStart++
		}
	}
	state.weeklyUnits += units
	state.fiveUnits += units
}

type engineStates struct {
	keys  map[string]*keyMeterState
	spend map[string]*keyMeterState
}

// pendingRequest is the short-lived request/callback association retained by
// the plugin so terminal Token usage is assigned to the originating Key.
type pendingRequest struct {
	KeyID        string
	AuthID       string
	Model        string
	Content      string
	RequestedAt  time.Time
	Managed      bool // true only when the plugin enforced budget and selected the CPA auth
	Rate         ModelRate
	Checkpointed bool
}

type pendingBucketKey struct {
	Scope    string
	ScopeID  string
	Model    string
	BucketAt int64
}

type pendingShard struct {
	mu            sync.Mutex
	buckets       map[pendingBucketKey]UsageEvent
	logs          []DecisionLog
	pendingSince  time.Time
	inFlightSince time.Time
}

// Engine owns Key policies, rolling dollar spend, terminal Token aggregation,
// and short-lived callback markers. It stores no account entitlement state.
type Engine struct {
	adminMu sync.RWMutex

	configMu sync.RWMutex
	config   RuntimeConfig

	policiesMu     sync.RWMutex
	policiesByID   map[string]KeyPolicy
	policiesByHash map[string]string

	ratesMu    sync.RWMutex
	modelRates map[string]ModelRate

	statesMu sync.RWMutex
	states   engineStates

	store *Store

	pendingMu       sync.Mutex
	pendingRequests []pendingRequest
	pendingCount    atomic.Int64

	requestContentMu       sync.Mutex
	capturedRequestContent map[string]capturedRequestContent

	contentFilterMu       sync.RWMutex
	contentFilterSettings ContentFilterSettings
	contentFilterMatcher  *contentFilterMatcher

	pendingShards       [pendingShardCount]pendingShard
	pendingBucketCount  atomic.Int64
	pendingLogCount     atomic.Int64
	droppedDecisionLogs atomic.Uint64
	flushMu             sync.Mutex
	flushStop           chan struct{}
	flushDone           chan struct{}

	admissionsClosed    atomic.Bool
	usageClosed         atomic.Bool
	closed              atomic.Bool
	closeMu             sync.Mutex
	closeErr            error
	persistenceFailures atomic.Uint64
	persistenceDegraded atomic.Bool
	lastRetentionSweep  atomic.Int64
	retentionFailures   atomic.Uint64

	usageDedupeMu        sync.Mutex
	recentUsageCallbacks map[string]time.Time
}

func Open(cfg RuntimeConfig) (*Engine, error) {
	store, err := OpenStore(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	settings, err := store.LoadOrCreateInstallationSettings(InstallationSettings{RecordRetention: cfg.RecordRetention}, cfg.KeyHMACSecret)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	cfg, err = runtimeConfigFromInstallation(cfg, settings)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	policies, err := store.LoadPolicies()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	policies = mergeBootstrapPolicies(policies, cfg.BootstrapKeys)
	if _, _, err := buildPolicyMaps(cfg, policies); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.InsertMissingPolicies(cfg.BootstrapKeys); err != nil {
		_ = store.Close()
		return nil, err
	}
	filter, err := store.LoadContentFilterSettings()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	contentMatcher, err := compileContentFilterMatcher(filter.Terms)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("compile content-filter expressions: %w", err)
	}
	now := time.Now().UTC()
	tokenEvents, err := store.LoadMeteringSince(now.Add(-sevenDayWindow))
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	spendEvents, err := store.LoadDollarSpendSince(now.Add(-sevenDayWindow))
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.SeedDefaultModelRates(defaultModelRates); err != nil {
		_ = store.Close()
		return nil, err
	}
	rates, err := store.ListModelRates()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	pendingRequests, err := store.LoadPendingRequests(now)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("load pending request checkpoints: %w", err)
	}
	engine := &Engine{
		config:                 cfg,
		policiesByID:           make(map[string]KeyPolicy, len(policies)),
		policiesByHash:         make(map[string]string, len(policies)),
		modelRates:             make(map[string]ModelRate, len(rates)),
		states:                 engineStates{keys: make(map[string]*keyMeterState), spend: make(map[string]*keyMeterState)},
		store:                  store,
		pendingRequests:        append(make([]pendingRequest, 0, len(pendingRequests)+128), pendingRequests...),
		capturedRequestContent: make(map[string]capturedRequestContent),
		contentFilterSettings:  filter,
		contentFilterMatcher:   contentMatcher,
		recentUsageCallbacks:   make(map[string]time.Time),
		flushStop:              make(chan struct{}),
		flushDone:              make(chan struct{}),
	}
	for index := range engine.pendingShards {
		engine.pendingShards[index].buckets = make(map[pendingBucketKey]UsageEvent)
		engine.pendingShards[index].logs = make([]DecisionLog, 0, 64)
	}
	if err := engine.replacePolicies(policies); err != nil {
		_ = store.Close()
		return nil, err
	}
	for _, rate := range rates {
		engine.modelRates[rate.Model] = rate
	}
	engine.pendingCount.Store(int64(len(pendingRequests)))
	engine.replaceMeterStates(tokenEvents, spendEvents, now, engine.policiesByID)
	go engine.flushLoop()
	return engine, nil
}

func (engine *Engine) replaceMeterStates(tokenEvents, spendEvents []UsageEvent, now time.Time, policies map[string]KeyPolicy) {
	byKey := make(map[string][]meterEvent, len(policies))
	for _, event := range tokenEvents {
		if event.Scope == "key_actual" && event.KeyID != "" {
			byKey[event.KeyID] = append(byKey[event.KeyID], meterEvent{At: event.RecordedAt, Units: event.Units})
		}
	}
	spendByKey := make(map[string][]meterEvent, len(policies))
	for _, event := range spendEvents {
		if event.KeyID != "" {
			spendByKey[event.KeyID] = append(spendByKey[event.KeyID], meterEvent{At: event.RecordedAt, Units: event.Units})
		}
	}
	next := engineStates{keys: make(map[string]*keyMeterState, len(policies)), spend: make(map[string]*keyMeterState, len(policies))}
	for keyID := range policies {
		next.keys[keyID] = newKeyMeterState(byKey[keyID], now)
		next.spend[keyID] = newKeyMeterState(spendByKey[keyID], now)
	}
	engine.statesMu.Lock()
	engine.states = next
	engine.statesMu.Unlock()
}

func (engine *Engine) replacePolicies(policies []KeyPolicy) error {
	engine.configMu.RLock()
	cfg := engine.config
	engine.configMu.RUnlock()
	byID, byHash, err := buildPolicyMaps(cfg, policies)
	if err != nil {
		return err
	}
	engine.policiesMu.Lock()
	engine.policiesByID = byID
	engine.policiesByHash = byHash
	engine.policiesMu.Unlock()
	return nil
}

func buildPolicyMaps(cfg RuntimeConfig, policies []KeyPolicy) (map[string]KeyPolicy, map[string]string, error) {
	_ = cfg
	byID := make(map[string]KeyPolicy, len(policies))
	byHash := make(map[string]string, len(policies))
	for _, raw := range policies {
		policy, err := normalizePolicy(raw)
		if err != nil {
			return nil, nil, err
		}
		if _, exists := byID[policy.ID]; exists {
			return nil, nil, fmt.Errorf("duplicate key policy id %q", policy.ID)
		}
		if _, exists := byHash[policy.KeySHA256]; exists {
			return nil, nil, fmt.Errorf("duplicate key fingerprint")
		}
		byID[policy.ID] = policy
		byHash[policy.KeySHA256] = policy.ID
	}
	return byID, byHash, nil
}

func mergeBootstrapPolicies(persisted, bootstrap []KeyPolicy) []KeyPolicy {
	merged := append([]KeyPolicy(nil), persisted...)
	known := make(map[string]struct{}, len(persisted))
	for _, policy := range persisted {
		known[policy.ID] = struct{}{}
	}
	for _, policy := range bootstrap {
		if _, exists := known[policy.ID]; exists {
			continue
		}
		merged = append(merged, policy)
		known[policy.ID] = struct{}{}
	}
	return merged
}

func (engine *Engine) Reconfigure(cfg RuntimeConfig) error {
	if engine == nil {
		return fmt.Errorf("quota engine is not initialized")
	}
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	engine.configMu.RLock()
	old := engine.config
	engine.configMu.RUnlock()
	if cfg.DatabasePath != old.DatabasePath || cfg.KeyHMACSecret != old.KeyHMACSecret {
		return fmt.Errorf("database_path and key_hmac_secret require a plugin restart")
	}
	persisted, err := engine.store.LoadPolicies()
	if err != nil {
		return err
	}
	policies := mergeBootstrapPolicies(persisted, cfg.BootstrapKeys)
	byID, byHash, err := buildPolicyMaps(cfg, policies)
	if err != nil {
		return err
	}
	if err := engine.store.InsertMissingPolicies(cfg.BootstrapKeys); err != nil {
		return err
	}
	engine.configMu.Lock()
	engine.config = cfg
	engine.configMu.Unlock()
	engine.policiesMu.Lock()
	engine.policiesByID, engine.policiesByHash = byID, byHash
	engine.policiesMu.Unlock()
	for keyID := range byID {
		engine.keyState(keyID)
		engine.keySpendState(keyID)
	}
	return nil
}

func (engine *Engine) Admit(rawAPIKey, model string, now time.Time, candidateSets ...[]SchedulerCandidate) Admission {
	return engine.admit(rawAPIKey, model, "", now, candidateSets...)
}

func (engine *Engine) AdmitCaptured(rawAPIKey, model, captureID string, now time.Time, candidateSets ...[]SchedulerCandidate) Admission {
	return engine.admit(rawAPIKey, model, captureID, now, candidateSets...)
}

func (engine *Engine) admit(rawAPIKey, model, captureID string, now time.Time, candidateSets ...[]SchedulerCandidate) Admission {
	if engine == nil {
		return deny("quota_unavailable", "quota management is not initialized")
	}
	engine.adminMu.RLock()
	defer engine.adminMu.RUnlock()
	if engine.admissionsClosed.Load() {
		return deny("quota_unavailable", "quota management is shutting down")
	}
	engine.configMu.RLock()
	cfg := engine.config
	engine.configMu.RUnlock()
	fingerprint := FingerprintAPIKey(rawAPIKey, cfg.KeyHMACSecret)
	if fingerprint == "" {
		return bypass()
	}
	engine.policiesMu.RLock()
	keyID, found := engine.policiesByHash[fingerprint]
	policy := engine.policiesByID[keyID]
	engine.policiesMu.RUnlock()
	if !found {
		return bypass()
	}
	captured := engine.claimCapturedRequestContent(captureID, keyID, now)
	model = strings.TrimSpace(model)
	if captured.Match.Matched {
		return engine.blockAdmission(keyID, "", model, captured.Content, now, "content_forbidden", "The request matched a content-blocking expression", httpStatusForbidden, captured.Match)
	}
	if !policy.AllowsAt(now) {
		return engine.blockAdmission(keyID, "", model, captured.Content, now, "access_schedule_closed", "This API key is outside its configured access schedule", httpStatusForbidden, ContentFilterMatch{})
	}
	if !modelAllowed(policy, model) {
		return engine.blockAdmission(keyID, "", model, captured.Content, now, "model_not_allowed", "This API key is not allowed to use the requested model", httpStatusForbidden, ContentFilterMatch{})
	}
	rate, configured := engine.modelRate(model)
	if !configured {
		return engine.blockAdmission(keyID, "", model, captured.Content, now, "model_rate_not_configured", "This model has no configured dollar rate", httpStatusServiceUnavailable, ContentFilterMatch{})
	}
	if policy.Enabled && (rate.inputMicrosPerMillion > 0 || rate.cachedMicrosPerMillion > 0 || rate.outputMicrosPerMillion > 0) {
		if coolingUntil := engine.dollarBudgetCoolingUntil(policy, now); coolingUntil != nil {
			result := engine.blockAdmission(keyID, "", model, captured.Content, now, "key_dollar_budget_exhausted", "This API key has reached a configured rolling dollar budget", httpStatusTooManyRequests, ContentFilterMatch{})
			result.RetryAt = coolingUntil
			return result
		}
	}
	if engine.persistenceDegraded.Load() {
		return engine.blockAdmission(keyID, "", model, captured.Content, now, "quota_persistence_unavailable", "quota accounting storage is temporarily unavailable", httpStatusServiceUnavailable, ContentFilterMatch{})
	}
	if !policy.Enabled {
		// A registered Key always stays in the metering pipeline. Disabled only
		// skips dollar-budget rejection and leaves CPA's normal routing untouched.
		marker := pendingRequest{KeyID: keyID, Model: model, Content: captured.Content, RequestedAt: now.UTC(), Rate: rate}
		if !engine.addPendingRequest(marker) {
			return engine.blockAdmission(keyID, "", model, captured.Content, now, "quota_persistence_unavailable", "too many requests are awaiting terminal usage", httpStatusServiceUnavailable, ContentFilterMatch{})
		}
		return Admission{Bypass: true, KeyID: keyID}
	}
	var candidates []SchedulerCandidate
	if len(candidateSets) > 0 {
		candidates = candidateSets[0]
	}
	if candidates == nil {
		return engine.blockAdmission(keyID, "", model, captured.Content, now, "quota_scheduler_candidates_required", "CPA did not provide scheduler candidates", httpStatusServiceUnavailable, ContentFilterMatch{})
	}
	authIDs, reason := engine.selectCPACandidates(candidates)
	if len(authIDs) == 0 {
		return engine.blockAdmission(keyID, "", model, captured.Content, now, reason, "CPA did not provide an eligible scheduler account candidate", httpStatusServiceUnavailable, ContentFilterMatch{})
	}
	marker := pendingRequest{
		KeyID: keyID, AuthID: authIDs[0], Model: model, Content: captured.Content,
		RequestedAt: now.UTC(), Managed: true, Rate: rate,
	}
	if !engine.addPendingRequest(marker) {
		return engine.blockAdmission(keyID, marker.AuthID, model, captured.Content, now, "quota_persistence_unavailable", "too many requests are awaiting terminal usage", httpStatusServiceUnavailable, ContentFilterMatch{})
	}
	return Admission{Allowed: true, KeyID: keyID, AuthID: marker.AuthID}
}

func (engine *Engine) blockAdmission(keyID, authID, model, content string, now time.Time, code, message string, status int, match ContentFilterMatch) Admission {
	result := deny(code, message)
	result.KeyID = keyID
	engine.enqueueDecision(DecisionLog{
		KeyID: keyID, AuthID: authID, Model: model, RequestContent: content,
		MatchedTerm: match.Term, MatchedCategory: match.Category, RequestedAt: now.UTC(),
		Decision: "blocked", StatusCode: status, Reason: code,
	})
	return result
}

func (engine *Engine) addPendingRequest(marker pendingRequest) bool {
	if engine == nil || marker.KeyID == "" {
		return false
	}
	if marker.RequestedAt.IsZero() {
		marker.RequestedAt = time.Now().UTC()
	}
	engine.pendingMu.Lock()
	defer engine.pendingMu.Unlock()
	cutoff := marker.RequestedAt.Add(-pendingRequestTTL)
	kept := engine.pendingRequests[:0]
	removed := int64(0)
	for _, existing := range engine.pendingRequests {
		if existing.RequestedAt.Before(cutoff) {
			removed++
			continue
		}
		kept = append(kept, existing)
	}
	engine.pendingRequests = kept
	engine.pendingCount.Add(-removed)
	if len(engine.pendingRequests) >= maxPendingDecisionLogs {
		return false
	}
	engine.pendingRequests = append(engine.pendingRequests, marker)
	engine.pendingCount.Add(1)
	return true
}

func (engine *Engine) takePendingRequest(keyID, authID, model string, requestedAt time.Time) (pendingRequest, bool) {
	engine.pendingMu.Lock()
	defer engine.pendingMu.Unlock()
	model, authID = strings.TrimSpace(model), strings.TrimSpace(authID)
	best := -1
	bestDistance := time.Duration(math.MaxInt64)
	for index, marker := range engine.pendingRequests {
		if marker.KeyID != keyID {
			continue
		}
		if marker.Model != "" && model != "" && marker.Model != model {
			continue
		}
		if marker.Managed && marker.AuthID != "" && authID != "" && marker.AuthID != authID {
			continue
		}
		distance := requestedAt.Sub(marker.RequestedAt)
		if distance < 0 {
			distance = -distance
		}
		if best < 0 || distance < bestDistance {
			best, bestDistance = index, distance
		}
	}
	if best < 0 {
		return pendingRequest{}, false
	}
	marker := engine.pendingRequests[best]
	engine.pendingRequests = append(engine.pendingRequests[:best], engine.pendingRequests[best+1:]...)
	engine.pendingCount.Add(-1)
	if marker.Checkpointed {
		if err := engine.store.DeletePendingRequest(marker); err != nil {
			engine.persistenceFailures.Add(1)
			engine.persistenceDegraded.Store(true)
		}
	}
	return marker, true
}

func (engine *Engine) discardPendingRequestsForKey(keyID string) {
	engine.pendingMu.Lock()
	defer engine.pendingMu.Unlock()
	kept := engine.pendingRequests[:0]
	removed := int64(0)
	for _, marker := range engine.pendingRequests {
		if marker.KeyID == keyID {
			removed++
			continue
		}
		kept = append(kept, marker)
	}
	engine.pendingRequests = kept
	engine.pendingCount.Add(-removed)
	engine.discardCapturedRequestContentForKey(keyID)
}

const (
	httpStatusOK                 = 200
	httpStatusForbidden          = 403
	httpStatusTooManyRequests    = 429
	httpStatusServiceUnavailable = 503
)

func modelAllowed(policy KeyPolicy, model string) bool {
	if len(policy.AllowedModels) == 0 {
		return true
	}
	model = strings.TrimSpace(model)
	for _, allowed := range policy.AllowedModels {
		if allowed == model {
			return true
		}
	}
	return false
}

// CompletedUsage is the safe subset of CPA's terminal UsageRecord.
type CompletedUsage struct {
	APIKey              string
	AuthID              string
	Model               string
	RequestedAt         time.Time
	Generate            bool
	Failed              bool
	FailureStatus       int
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

func (engine *Engine) RecordUsage(record CompletedUsage) {
	if engine == nil || engine.usageClosed.Load() {
		return
	}
	engine.adminMu.RLock()
	defer engine.adminMu.RUnlock()
	if engine.usageClosed.Load() {
		return
	}
	engine.configMu.RLock()
	cfg := engine.config
	engine.configMu.RUnlock()
	fingerprint := FingerprintAPIKey(record.APIKey, cfg.KeyHMACSecret)
	if fingerprint == "" {
		return
	}
	engine.policiesMu.RLock()
	keyID, found := engine.policiesByHash[fingerprint]
	policy := engine.policiesByID[keyID]
	engine.policiesMu.RUnlock()
	if !found {
		return
	}
	requestedAt := record.RequestedAt.UTC()
	if requestedAt.IsZero() {
		requestedAt = time.Now().UTC()
	}
	authID, model := strings.TrimSpace(record.AuthID), strings.TrimSpace(record.Model)
	if !engine.claimUsageCallback(keyID, authID, requestedAt, record, cfg.KeyHMACSecret) {
		return
	}
	marker, matched := engine.takePendingRequest(keyID, authID, model, requestedAt)
	if !matched {
		engine.enqueueDecision(DecisionLog{KeyID: keyID, AuthID: authID, Model: model, RequestedAt: requestedAt, Decision: "ignored", Reason: "unmatched_usage_callback"})
		return
	}
	requestedAt = marker.RequestedAt
	units := completedUsageUnits(record)
	if !marker.Managed {
		engine.recordUnenforcedUsage(marker, authID, units, record)
		return
	}
	cachedTokens := cachedUsageTokens(record)
	cost := costBreakdown{}
	if units > 0 {
		cost = costBreakdownMicros(marker.Rate, record.InputTokens, cachedTokens, record.OutputTokens)
		if !engine.chargeDollarSpend(keyID, requestedAt.Truncate(time.Millisecond), cost.Total) {
			engine.persistenceDegraded.Store(true)
		}
	}
	if units > 0 {
		state := engine.keyState(keyID)
		state.mu.Lock()
		state.completed.prune(requestedAt)
		state.completed.addEvent(usageBucketEnd(requestedAt), units)
		state.mu.Unlock()
	}
	decision, reason, status := "completed", "actual_token_usage", httpStatusOK
	if units == 0 {
		decision, reason, status = "failed", "request_not_completed", record.FailureStatus
		if record.Failed {
			reason = "upstream_failed"
		}
	} else if record.Failed {
		decision, reason, status = "failed", "upstream_failed_with_actual_usage", record.FailureStatus
	}
	if !policy.Enabled {
		reason += "_after_policy_disabled"
	}
	if status < 0 {
		status = 0
	}
	engine.enqueueDecision(DecisionLog{
		KeyID: keyID, AuthID: authID, Model: model, RequestContent: marker.Content,
		RequestedAt: requestedAt, Decision: decision, StatusCode: status, Reason: reason, Units: units,
		InputTokens: record.InputTokens, CachedTokens: cachedTokens, OutputTokens: record.OutputTokens,
		InputCostMicros: cost.Input, CachedCostMicros: cost.Cached, OutputCostMicros: cost.Output, CostMicros: cost.Total,
	})
	if units > 0 {
		event := UsageEvent{
			Scope: "key_actual", KeyID: keyID, AuthID: authID, Model: model,
			RequestedAt: requestedAt, RecordedAt: usageBucketEnd(requestedAt),
			InputTokens: record.InputTokens, CachedTokens: cachedTokens, OutputTokens: record.OutputTokens,
			InputCostMicros: cost.Input, CachedCostMicros: cost.Cached, OutputCostMicros: cost.Output, CostMicros: cost.Total,
			ReasoningTokens: record.ReasoningTokens, Units: units, RequestCount: 1,
			MeteredBy: "completion_token_usage", Failed: record.Failed, FailureStatus: record.FailureStatus,
		}
		if !engine.enqueueBuckets(event) {
			engine.persistenceDegraded.Store(true)
		}
	}
}

func (engine *Engine) recordUnenforcedUsage(marker pendingRequest, authID string, units int64, record CompletedUsage) {
	keyID, content, requestedAt := marker.KeyID, marker.Content, marker.RequestedAt
	cached := cachedUsageTokens(record)
	cost := costBreakdown{}
	if units > 0 {
		cost = costBreakdownMicros(marker.Rate, record.InputTokens, cached, record.OutputTokens)
		if !engine.chargeDollarSpend(keyID, requestedAt.Truncate(time.Millisecond), cost.Total) {
			engine.persistenceDegraded.Store(true)
		}
		state := engine.keyState(keyID)
		state.mu.Lock()
		state.completed.prune(requestedAt)
		state.completed.addEvent(usageBucketEnd(requestedAt), units)
		state.mu.Unlock()
	}
	decision, reason, status := "completed", "actual_token_usage_without_budget_enforcement", httpStatusOK
	if units == 0 {
		decision, reason, status = "failed", "request_not_completed_without_budget_enforcement", record.FailureStatus
		if record.Failed {
			reason = "upstream_failed_without_budget_enforcement"
		}
	} else if record.Failed {
		decision, reason, status = "failed", "upstream_failed_with_actual_usage_without_budget_enforcement", record.FailureStatus
	}
	if status < 0 {
		status = 0
	}
	engine.enqueueDecision(DecisionLog{
		KeyID: keyID, AuthID: authID, Model: strings.TrimSpace(record.Model), RequestContent: content,
		RequestedAt: requestedAt, Decision: decision, StatusCode: status, Reason: reason, Units: units,
		InputTokens: record.InputTokens, CachedTokens: cached, OutputTokens: record.OutputTokens,
		InputCostMicros: cost.Input, CachedCostMicros: cost.Cached, OutputCostMicros: cost.Output, CostMicros: cost.Total,
	})
	// Budget enforcement never owns observability. Persist the same Token,
	// rate-card cost and rolling-window accounting as an enforced Key.
	if units > 0 {
		if !engine.enqueueBuckets(UsageEvent{
			Scope: "key_actual", KeyID: keyID, AuthID: authID, Model: strings.TrimSpace(record.Model),
			RequestedAt: requestedAt, RecordedAt: usageBucketEnd(requestedAt),
			InputTokens: record.InputTokens, CachedTokens: cached, OutputTokens: record.OutputTokens,
			InputCostMicros: cost.Input, CachedCostMicros: cost.Cached, OutputCostMicros: cost.Output, CostMicros: cost.Total,
			ReasoningTokens: record.ReasoningTokens, Units: units, RequestCount: 1,
			MeteredBy: "completion_token_usage", Failed: record.Failed, FailureStatus: record.FailureStatus,
		}) {
			engine.persistenceDegraded.Store(true)
		}
	}
}

func (engine *Engine) claimUsageCallback(keyID, authID string, requestedAt time.Time, record CompletedUsage, secret string) bool {
	if engine == nil || secret == "" {
		return true
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%s\x00%s\x00%s\x00%d\x00%t\x00%t\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d", keyID, authID, strings.TrimSpace(record.Model), requestedAt.UTC().UnixNano(), record.Generate, record.Failed, record.FailureStatus, record.InputTokens, record.OutputTokens, record.ReasoningTokens, record.CachedTokens, record.CacheReadTokens, record.CacheCreationTokens, record.TotalTokens)
	id := hex.EncodeToString(mac.Sum(nil))
	now, cutoff := time.Now().UTC(), time.Now().UTC().Add(-usageCallbackDedupeTTL)
	engine.usageDedupeMu.Lock()
	defer engine.usageDedupeMu.Unlock()
	if previous, exists := engine.recentUsageCallbacks[id]; exists && previous.After(cutoff) {
		return false
	}
	if len(engine.recentUsageCallbacks) >= maxRecentUsageCallbacks {
		for existing, seenAt := range engine.recentUsageCallbacks {
			if !seenAt.After(cutoff) {
				delete(engine.recentUsageCallbacks, existing)
			}
		}
	}
	if len(engine.recentUsageCallbacks) < maxRecentUsageCallbacks {
		engine.recentUsageCallbacks[id] = now
	}
	return true
}

func completedUsageUnits(record CompletedUsage) int64 {
	if !record.Generate {
		return 0
	}
	if record.TotalTokens > 0 {
		return record.TotalTokens
	}
	var units int64
	for _, value := range []int64{record.InputTokens, record.OutputTokens, record.ReasoningTokens} {
		if value > 0 && units <= math.MaxInt64-value {
			units += value
		}
	}
	if units > 0 {
		return units
	}
	return 0
}

func usageBucketEnd(now time.Time) time.Time {
	return now.UTC().Truncate(usageBucketWindow).Add(usageBucketWindow)
}

func bucketKey(event UsageEvent) pendingBucketKey {
	return pendingBucketKey{Scope: event.Scope, ScopeID: event.KeyID, Model: event.Model, BucketAt: event.RecordedAt.UTC().UnixMilli()}
}

func mergeBucketEvent(existing, incoming UsageEvent) UsageEvent {
	existing.Units += incoming.Units
	existing.RequestCount += incoming.RequestCount
	existing.InputTokens += incoming.InputTokens
	existing.CachedTokens += incoming.CachedTokens
	existing.OutputTokens += incoming.OutputTokens
	existing.InputCostMicros += incoming.InputCostMicros
	existing.CachedCostMicros += incoming.CachedCostMicros
	existing.OutputCostMicros += incoming.OutputCostMicros
	existing.CostMicros += incoming.CostMicros
	if existing.AuthID != incoming.AuthID {
		existing.AuthID = "mixed"
	}
	return existing
}

func pendingShardIndex(key pendingBucketKey) int {
	hash := uint32(2_166_136_261)
	for _, value := range [3]string{key.Scope, key.ScopeID, key.Model} {
		for index := 0; index < len(value); index++ {
			hash ^= uint32(value[index])
			hash *= 16_777_619
		}
	}
	return int(hash % pendingShardCount)
}

func (engine *Engine) enqueueBuckets(event UsageEvent) bool {
	key := bucketKey(event)
	shard := &engine.pendingShards[pendingShardIndex(key)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	now := time.Now().UTC()
	if (!shard.pendingSince.IsZero() && now.Sub(shard.pendingSince) > maxPendingAge) || (!shard.inFlightSince.IsZero() && now.Sub(shard.inFlightSince) > maxPendingAge) {
		return false
	}
	if _, exists := shard.buckets[key]; !exists {
		for {
			current := engine.pendingBucketCount.Load()
			if current+1 > maxPendingBuckets {
				return false
			}
			if engine.pendingBucketCount.CompareAndSwap(current, current+1) {
				break
			}
		}
	}
	if existing, found := shard.buckets[key]; found {
		shard.buckets[key] = mergeBucketEvent(existing, event)
	} else {
		shard.buckets[key] = event
	}
	if shard.pendingSince.IsZero() {
		shard.pendingSince = now
	}
	return true
}

func (engine *Engine) enqueueDecision(entry DecisionLog) {
	if entry.KeyID == "" || engine == nil || engine.usageClosed.Load() {
		return
	}
	if entry.KeySuffix == "" {
		engine.policiesMu.RLock()
		entry.KeySuffix = engine.policiesByID[entry.KeyID].KeySuffix
		engine.policiesMu.RUnlock()
	}
	if engine.reserveDecisionLogSlots(1) != 1 {
		engine.droppedDecisionLogs.Add(1)
		return
	}
	shard := &engine.pendingShards[pendingShardIndex(pendingBucketKey{Scope: "log", ScopeID: entry.KeyID})]
	shard.mu.Lock()
	shard.logs = append(shard.logs, entry)
	if shard.pendingSince.IsZero() {
		shard.pendingSince = time.Now().UTC()
	}
	shard.mu.Unlock()
}

func (engine *Engine) reserveDecisionLogSlots(wanted int) int {
	for wanted > 0 {
		current := engine.pendingLogCount.Load()
		if current >= maxPendingDecisionLogs {
			return 0
		}
		available := int(maxPendingDecisionLogs - current)
		if available > wanted {
			available = wanted
		}
		if engine.pendingLogCount.CompareAndSwap(current, current+int64(available)) {
			return available
		}
	}
	return 0
}

func (engine *Engine) flushLoop() {
	ticker := time.NewTicker(persistenceFlushInterval)
	defer func() {
		ticker.Stop()
		close(engine.flushDone)
	}()
	for {
		select {
		case <-ticker.C:
			_ = engine.flushPending()
			engine.pruneRetention(time.Now().UTC())
		case <-engine.flushStop:
			_ = engine.flushPending()
			return
		}
	}
}

func (engine *Engine) flushPending() error {
	if engine == nil {
		return nil
	}
	engine.flushMu.Lock()
	defer engine.flushMu.Unlock()
	type batchState struct {
		index        int
		events       map[pendingBucketKey]UsageEvent
		logs         []DecisionLog
		pendingSince time.Time
	}
	states := make([]batchState, 0, pendingShardCount)
	events := make([]UsageEvent, 0)
	logs := make([]DecisionLog, 0)
	for index := range engine.pendingShards {
		shard := &engine.pendingShards[index]
		shard.mu.Lock()
		if len(shard.buckets) == 0 && len(shard.logs) == 0 {
			shard.mu.Unlock()
			continue
		}
		state := batchState{index: index, events: shard.buckets, logs: shard.logs, pendingSince: shard.pendingSince}
		shard.buckets = make(map[pendingBucketKey]UsageEvent)
		shard.logs = make([]DecisionLog, 0, 64)
		shard.pendingSince = time.Time{}
		shard.inFlightSince = state.pendingSince
		shard.mu.Unlock()
		engine.pendingBucketCount.Add(-int64(len(state.events)))
		engine.pendingLogCount.Add(-int64(len(state.logs)))
		states = append(states, state)
		for _, event := range state.events {
			events = append(events, event)
		}
		logs = append(logs, state.logs...)
	}
	if len(events) == 0 && len(logs) == 0 {
		return nil
	}
	if err := engine.store.FlushUsageAndLogs(events, logs); err != nil {
		engine.persistenceFailures.Add(1)
		engine.persistenceDegraded.Store(true)
		for _, state := range states {
			shard := &engine.pendingShards[state.index]
			shard.mu.Lock()
			shard.inFlightSince = time.Time{}
			keep := engine.reserveDecisionLogSlots(len(state.logs))
			if keep > 0 {
				shard.logs = append(state.logs[:keep], shard.logs...)
			}
			for key, event := range state.events {
				if existing, found := shard.buckets[key]; found {
					shard.buckets[key] = mergeBucketEvent(existing, event)
				} else {
					shard.buckets[key] = event
					engine.pendingBucketCount.Add(1)
				}
			}
			if shard.pendingSince.IsZero() || state.pendingSince.Before(shard.pendingSince) {
				shard.pendingSince = state.pendingSince
			}
			shard.mu.Unlock()
		}
		return err
	}
	for _, state := range states {
		shard := &engine.pendingShards[state.index]
		shard.mu.Lock()
		shard.inFlightSince = time.Time{}
		shard.mu.Unlock()
	}
	engine.persistenceDegraded.Store(false)
	return nil
}

func (engine *Engine) pruneRetention(now time.Time) {
	previous := engine.lastRetentionSweep.Load()
	if previous != 0 && now.Unix()-previous < int64(time.Hour/time.Second) {
		return
	}
	if !engine.lastRetentionSweep.CompareAndSwap(previous, now.Unix()) {
		return
	}
	engine.configMu.RLock()
	retention := engine.config.RecordRetentionDuration
	engine.configMu.RUnlock()
	if err := engine.store.DeleteUsageEventsBefore(now.Add(-retention)); err != nil {
		engine.retentionFailures.Add(1)
	}
}

func (engine *Engine) PendingSettlementCount() int64 {
	if engine == nil {
		return 0
	}
	return engine.pendingCount.Load()
}

func (engine *Engine) PendingSettlementAuthIDs() []string {
	if engine == nil {
		return nil
	}
	engine.pendingMu.Lock()
	defer engine.pendingMu.Unlock()
	seen := make(map[string]struct{})
	for _, marker := range engine.pendingRequests {
		if marker.Managed && marker.AuthID != "" {
			seen[marker.AuthID] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for authID := range seen {
		result = append(result, authID)
	}
	sort.Strings(result)
	return result
}

func (engine *Engine) HasPendingSettlementForAuth(authID string) bool {
	authID = strings.TrimSpace(authID)
	for _, item := range engine.PendingSettlementAuthIDs() {
		if item == authID {
			return true
		}
	}
	return false
}

func (engine *Engine) CloseAdmissions() {
	if engine != nil {
		engine.admissionsClosed.Store(true)
	}
}

func (engine *Engine) IsClosing() bool {
	return engine != nil && engine.admissionsClosed.Load() && !engine.closed.Load()
}

func (engine *Engine) Close() error {
	return engine.close(false)
}

func (engine *Engine) CloseConservatively() error {
	return engine.close(true)
}

func (engine *Engine) close(force bool) error {
	if engine == nil {
		return nil
	}
	engine.closeMu.Lock()
	defer engine.closeMu.Unlock()
	if engine.closed.Load() {
		return engine.closeErr
	}
	engine.admissionsClosed.Store(true)
	if !force && engine.PendingSettlementCount() > 0 {
		return fmt.Errorf("%d requests are still awaiting terminal usage", engine.PendingSettlementCount())
	}
	engine.pendingMu.Lock()
	checkpoint := append([]pendingRequest(nil), engine.pendingRequests...)
	engine.pendingMu.Unlock()
	if !force {
		checkpoint = nil
	}
	if err := engine.store.ReplacePendingRequests(checkpoint); err != nil {
		engine.closeErr = fmt.Errorf("checkpoint pending requests: %w", err)
	}
	engine.usageClosed.Store(true)
	close(engine.flushStop)
	<-engine.flushDone
	if err := engine.flushPending(); err != nil {
		if engine.closeErr == nil {
			engine.closeErr = err
		}
	}
	if err := engine.store.Close(); err != nil {
		if engine.closeErr == nil {
			engine.closeErr = err
		}
	}
	engine.closed.Store(true)
	return engine.closeErr
}

func (engine *Engine) keyState(keyID string) *keyMeterState {
	engine.statesMu.RLock()
	state := engine.states.keys[keyID]
	engine.statesMu.RUnlock()
	if state != nil {
		return state
	}
	engine.statesMu.Lock()
	defer engine.statesMu.Unlock()
	if state = engine.states.keys[keyID]; state == nil {
		state = newKeyMeterState(nil, time.Now().UTC())
		engine.states.keys[keyID] = state
	}
	return state
}

func deny(code, message string) Admission { return Admission{Code: code, Message: message} }
func bypass() Admission                   { return Admission{Bypass: true} }

func FingerprintAPIKey(raw, secret string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}
