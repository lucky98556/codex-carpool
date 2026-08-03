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
	// One-minute aggregates keep storage bounded while reducing the rolling
	// window's conservative rounding from five minutes to at most one minute.
	usageBucketWindow        = time.Minute
	persistenceFlushInterval = time.Second
	// Durable allocation transitions are collected for a few milliseconds so
	// concurrent managed requests share one SQLite transaction. An allowed
	// admission still waits for its reservation to commit; batching never turns
	// a restart into a quota bypass.
	allocationPersistenceFlushInterval = 5 * time.Millisecond
	allocationCloseDrainTimeout        = 10 * time.Second
	maxPendingAllocationMutations      = 4_096
	usageCallbackDedupeTTL             = 15 * time.Minute
	maxRecentUsageCallbacks            = 20_000
	maxPendingBuckets                  = 20_000
	maxPendingDecisionLogs             = 20_000
	maxPendingAge                      = 10 * time.Second
	pendingShardCount                  = 32
	// x values are stored as fixed-point micro-x units. This keeps admission
	// checks integer-only while preserving more precision than the official
	// percentage display can expose.
	officialXUnitsPerX int64 = 1_000_000
)

// Admission is the outcome of a scheduler decision. The host is instructed to
// reject a request whenever Allowed is false; this is the enforcement point.
type Admission struct {
	Allowed bool
	// Bypass returns the request to CLIProxyAPI's normal scheduler. It is used
	// for clients without a registered quota policy; only registered Keys are
	// governed by this plugin.
	Bypass  bool
	AuthID  string
	Code    string
	Message string
	KeyID   string
}

// UsageEvent is a durable actual-Token audit aggregate emitted from CPA's
// terminal usage callback. These rows power analysis and official attribution.
// After an account has a trustworthy official calibration, the same Token
// total may also create a bounded provisional x guard that the next measurable
// official percentage observation replaces rather than adds again.
type UsageEvent struct {
	ID int64 `json:"id"`
	// Scope is internal storage metadata. Legacy records apply to both the Key
	// and account counters; new records are stored separately as key/account
	// buckets so their row count stays bounded under high request rates.
	Scope           string    `json:"-"`
	KeyID           string    `json:"key_id"`
	GroupID         string    `json:"group_id"`
	AuthID          string    `json:"auth_id"`
	Model           string    `json:"model"`
	RequestedAt     time.Time `json:"requested_at"`
	RecordedAt      time.Time `json:"recorded_at"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	CachedTokens    int64     `json:"cached_tokens"`
	Units           int64     `json:"units"`
	RequestCount    int64     `json:"request_count"`
	MeteredBy       string    `json:"metered_by"`
	Failed          bool      `json:"failed"`
	FailureStatus   int       `json:"failure_status"`
}

// DecisionLog is a compact audit event. RequestContent is a bounded excerpt of
// user-authored text only; raw request bodies, system/tool content, response
// bodies, raw API Keys, and OAuth credentials are never stored.
type DecisionLog struct {
	ID             int64     `json:"id"`
	KeyID          string    `json:"key_id"`
	AuthID         string    `json:"auth_id"`
	Model          string    `json:"model"`
	RequestContent string    `json:"request_content,omitempty"`
	RequestedAt    time.Time `json:"requested_at"`
	Decision       string    `json:"decision"`
	StatusCode     int       `json:"status_code"`
	Reason         string    `json:"reason"`
	Units          int64     `json:"units"`
}

// OperationalLog records plugin lifecycle and background work separately from
// per-Key request decisions. It never contains a raw API Key, OAuth token,
// prompt, or upstream response body.
type OperationalLog struct {
	ID         int64     `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	Level      string    `json:"level"`
	Event      string    `json:"event"`
	Message    string    `json:"message"`
	AuthID     string    `json:"auth_id,omitempty"`
	KeyID      string    `json:"key_id,omitempty"`
}

// ModelCatalogEntry is synchronized from CPA by the panel. The native plugin
// never calls CPA during request admission; a cached catalog keeps the hot
// path local and lets the browser reuse the management session it already has.
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

// meterState keeps rolling sums in bounded five-minute buckets. The normal
// hot path merges into the latest bucket in O(1), rather than retaining one
// in-memory event for every request during the seven-day window.
type meterState struct {
	mu          sync.Mutex
	events      []meterEvent
	fiveStart   int
	fiveUnits   int64
	weeklyUnits int64
}

// keyMeterState keeps completed usage and short-lived admission reservations
// under one lock. Reservations prevent concurrent requests from overshooting a
// limit before CLIProxyAPI publishes their completion token usage.
type keyMeterState struct {
	mu           sync.Mutex
	completed    *meterState
	reservations *meterState
}

func newKeyMeterState(events []meterEvent, now time.Time) *keyMeterState {
	return &keyMeterState{completed: newMeterState(events, now), reservations: newMeterState(nil, now)}
}

func newMeterState(events []meterEvent, now time.Time) *meterState {
	state := &meterState{}
	sort.Slice(events, func(left, right int) bool { return events[left].At.Before(events[right].At) })
	for _, event := range events {
		if event.At.Before(now.Add(-sevenDayWindow)) {
			continue
		}
		if count := len(state.events); count > 0 && state.events[count-1].At.Equal(event.At) {
			state.events[count-1].Units += event.Units
		} else {
			state.events = append(state.events, event)
		}
		state.weeklyUnits += event.Units
	}
	for state.fiveStart < len(state.events) && state.events[state.fiveStart].At.Before(now.Add(-fiveHourWindow)) {
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
	for drop < len(state.events) && state.events[drop].At.Before(sevenCutoff) {
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
	for state.fiveStart < len(state.events) && state.events[state.fiveStart].At.Before(fiveCutoff) {
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
	index := sort.Search(len(state.events), func(index int) bool {
		return !state.events[index].At.Before(at)
	})
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

// removeEvent releases one provisional admission reservation. Completion
// events normally share the exact minute bucket with admission; the fallback
// keeps out-of-order callbacks from permanently consuming a reservation.
func (state *meterState) removeEvent(at time.Time, units int64) bool {
	if units <= 0 || len(state.events) == 0 {
		return false
	}
	index := sort.Search(len(state.events), func(index int) bool {
		return !state.events[index].At.Before(at)
	})
	if index >= len(state.events) || !state.events[index].At.Equal(at) || state.events[index].Units < units {
		index = -1
		for candidate := range state.events {
			if state.events[candidate].Units >= units {
				index = candidate
				break
			}
		}
		if index < 0 {
			return false
		}
	}
	state.events[index].Units -= units
	state.weeklyUnits -= units
	if index >= state.fiveStart {
		state.fiveUnits -= units
	}
	if state.events[index].Units == 0 {
		state.events = append(state.events[:index], state.events[index+1:]...)
		if index < state.fiveStart {
			state.fiveStart--
		}
	}
	return true
}

type engineStates struct {
	keys map[string]*keyMeterState
}

// allocationBucketKey identifies a Key's share of one specific account's
// official weekly window. The upstream reset instant is part of the key,
// which naturally supports pools whose accounts reset at different times.
type allocationBucketKey struct {
	KeyID         string
	AuthID        string
	WindowResetAt int64
	BucketAt      int64
}

type allocationCycleKey struct {
	KeyID         string
	AuthID        string
	WindowResetAt int64
}

// allocationPendingKey narrows terminal usage lookup to the one managed Key
// and CPA account that own an outstanding reservation. Completed buckets do
// not belong here, keeping the scheduler hot path independent of seven days
// of historical allocation data.
type allocationPendingKey struct {
	KeyID  string
	AuthID string
}

type allocationBucketState struct {
	Completed   int64
	Provisional int64
	Reserved    int64
	// Capacity is durable for the account's official-week cycle. An increase
	// takes effect immediately; a decrease is saved as the next-cycle policy,
	// so the current snapshot never shrinks before the official reset.
	Capacity int64
	// GlobalCapacity is the one Key-wide allowance shared by every account
	// selected during an official window. Capacity remains only to recover
	// pre-global-ledger rows that were written as account-specific shards.
	GlobalCapacity int64
}

// expiredAllocationReservation is an old weekly reservation whose official
// account window has conclusively reset before CPA delivered a terminal usage
// callback. It is released as a reservation only; it is never fabricated into
// customer Token usage.
type expiredAllocationReservation struct {
	Key   allocationBucketKey
	Units int64
}

type allocationCycleState struct {
	Completed      int64
	Provisional    int64
	Reserved       int64
	Capacity       int64
	GlobalCapacity int64
}

// officialAccountWindowKey identifies one account-owned Codex quota window.
// Unlike a Key allocation, this ledger is shared by every managed Key routed
// to the account. It is rebuilt from the durable Key allocation buckets after
// a restart and re-based whenever CPA obtains a newer official snapshot.
type officialAccountWindowKey struct {
	AuthID        string
	Kind          string
	WindowResetAt int64
}

type officialAccountWindowState struct {
	// Capacity is the smallest official remaining allowance observed in this
	// reset window, converted to fixed-point x units. Completed is retained for
	// backward-compatible diagnostics but confirmed work is already reflected
	// by the next official remainder, so new code keeps it at zero.
	Capacity      int64
	Completed     int64
	Reserved      int64
	BaselineAt    int64
	WindowSeconds int64
}

type officialAccountWindowTarget struct {
	Key           officialAccountWindowKey
	Capacity      int64
	BaselineAt    int64
	WindowSeconds int64
}

// allocationMutation is committed by the dedicated durable reservation
// worker. Positive reservations have a reply channel because CPA must not see
// an allow until SQLite has made its correlation marker crash-safe. Settlement
// mutations do not delay the response; ordered retry releases that marker and
// writes any calibrated, bounded provisional x into its attribution-time bucket.
// Confirmed x is committed separately with official percentage observations.
type allocationMutation struct {
	Key              allocationBucketKey
	CompletedDelta   int64
	ReservedDelta    int64
	ProvisionalKey   allocationBucketKey
	ProvisionalDelta int64
	// CapacityUnits initializes a durable cycle snapshot and may only raise it
	// during the same official week; a policy decrease waits for that reset.
	CapacityUnits int64
	// GlobalCapacityUnits is durable evidence that CapacityUnits belongs to a
	// Key-wide ledger instead of an old account-proportional shard.
	GlobalCapacityUnits int64
	done                chan error
}

type allocationSettlement struct {
	Matched   bool
	Durable   bool
	Ambiguous bool
	Key       allocationBucketKey
	BucketAt  time.Time
}

var (
	errAllocationExhausted      = fmt.Errorf("account allocation exhausted")
	errOfficialAccountExhausted = fmt.Errorf("official account quota exhausted")
	errOfficialWeeklyReset      = fmt.Errorf("official weekly reset is unavailable")
)

const (
	// admissionReservationUnits is only a durable correlation marker. CPA may
	// ask the scheduler to pick an account more than once for one downstream
	// request, while publishing only one terminal usage record. Reserving the
	// full fallback estimate on every pick therefore creates fictitious usage.
	// Official weekly percentage changes remain the quota authority.
	admissionReservationUnits int64 = 1
)

// pendingShard keeps unrelated registered Keys from contending on one global
// aggregation mutex. A batch can span several shards, but admission only locks
// the one or two shards represented by its Key/account bucket events.
type pendingShard struct {
	mu            sync.Mutex
	buckets       map[pendingBucketKey]UsageEvent
	logs          []DecisionLog
	pendingSince  time.Time
	inFlightSince time.Time
}

// Engine owns immediate in-memory scheduling state plus a short, batched
// persistence queue. Clean shutdowns flush the queue before releasing SQLite.
type Engine struct {
	// adminMu serializes policy writes with plugin reconfiguration. Without this,
	// two panel updates can commit different rows to SQLite and leave the
	// in-memory fingerprint map pointing at the earlier value.
	adminMu sync.RWMutex

	configMu sync.RWMutex
	config   RuntimeConfig

	policiesMu     sync.RWMutex
	policiesByID   map[string]KeyPolicy
	policiesByHash map[string]string

	statesMu sync.RWMutex
	states   engineStates

	poolMu         sync.RWMutex
	accountPool    map[string]AccountPoolEntry
	quotaSnapshots map[string]OfficialQuotaSnapshot
	// quotaCalibrations is a separate plugin-owned account ledger. It converts
	// a configured x into an observed Token equivalent without changing CPA's
	// official percentage/reset authority or the per-Key usage history.
	calibrationMu     sync.RWMutex
	quotaCalibrations map[string]quotaCalibration
	// officialQuotaMu serializes a snapshot's durable write and local
	// account-window reconciliation. Callers acquire it before adminMu so the
	// normal SQLite snapshot write does not block admissions; adminMu is taken
	// only for the brief in-memory handoff or rare reset reconciliation.
	officialQuotaMu sync.Mutex

	store                    *Store
	allocationMu             sync.Mutex
	allocationBuckets        map[allocationBucketKey]allocationBucketState
	allocationBucketsByAuth  map[string]map[allocationBucketKey]struct{}
	pendingAllocationBuckets map[allocationPendingKey]map[allocationBucketKey]struct{}
	allocationCycles         map[allocationCycleKey]allocationCycleState
	officialAccountWindows   map[officialAccountWindowKey]officialAccountWindowState
	allocationMutations      chan allocationMutation
	allocationStop           chan struct{}
	allocationDone           chan struct{}
	allocationRetryMu        sync.Mutex
	allocationRetry          []allocationMutation
	allocationDegraded       atomic.Bool
	// accountSourceConflict is set while the file-backed synchronizer verifies
	// that enabled pool entries have distinct physical sources and stable Codex
	// identities. Managed Keys must fail closed rather than treating one
	// official account as two.
	accountSourceConflict atomic.Bool
	// accountSourceRevision changes whenever an auth-dir or account-pool update
	// can change source identity. sourceGuardMu commits the revision check and
	// guard update together, so an older clean scan cannot reopen admissions
	// after a newer configuration has required a fail-closed recheck.
	accountSourceRevision atomic.Uint64
	sourceGuardMu         sync.Mutex
	// accountSourceVerificationRevision is non-zero while the current source
	// configuration must receive one complete, conflict-free scan before the
	// fail-closed guard may be cleared. sourceGuardMu protects it.
	accountSourceVerificationRevision uint64
	// accountSourceVerificationEnabled is turned on by the native file-backed
	// integration before its first scan. Keeping direct Engine users opt-in
	// avoids imposing filesystem verification on embedders that do not use CPA
	// auth files, while every subsequent pool mutation in the native plugin
	// remains fail-closed until it is scanned again.
	accountSourceVerificationEnabled bool
	// pendingSettlements counts managed requests that were admitted with a
	// durable reservation but have not yet delivered CPA's terminal usage
	// record. Closing must wait for these records before it can stop the
	// allocation worker; otherwise a completed request could be left at its
	// smaller pre-dispatch reservation.
	pendingSettlements       atomic.Int64
	settlementMu             sync.Mutex
	pendingSettlementsByKey  map[string]int64
	pendingSettlementsByAuth map[string]int64
	// pendingSettlementsByBucket distinguishes this process's reservations
	// from durable reservations recovered after a crash. A late callback for a
	// recovered bucket must never drain the counter for a newer request that
	// happens to use the same Key and account.
	pendingSettlementsByBucket map[allocationBucketKey]int64
	// requestContentMu protects short-lived request excerpts captured before
	// CPA authentication and their association with one admitted allocation
	// bucket. Only terminal or blocked audit rows are persisted.
	requestContentMu       sync.Mutex
	capturedRequestContent map[string]capturedRequestContent
	pendingRequestContent  map[allocationBucketKey][]pendingRequestContent
	pendingRequestCount    int
	pendingShards              [pendingShardCount]pendingShard
	pendingBucketCount         atomic.Int64
	pendingLogCount            atomic.Int64
	droppedDecisionLogs        atomic.Uint64
	flushMu                    sync.Mutex
	usageRecordsMu             sync.Mutex
	lastUsageRecordsFlush      time.Time
	flushStop                  chan struct{}
	flushDone                  chan struct{}
	// admissionsClosed stops new scheduler decisions while still allowing
	// already-admitted requests to publish their terminal usage. usageClosed is
	// set only after those records have drained, immediately before workers are
	// stopped. closed means the SQLite store has actually been released.
	admissionsClosed     atomic.Bool
	usageClosed          atomic.Bool
	closed               atomic.Bool
	closeMu              sync.Mutex
	closeErr             error
	persistenceFailures  atomic.Uint64
	persistenceDegraded  atomic.Bool
	lastRetentionSweep   atomic.Int64
	retentionFailures    atomic.Uint64
	usageDedupeMu        sync.Mutex
	recentUsageCallbacks map[string]time.Time
}

// Open starts the engine, loads seven days of accounting state, and seeds any
// bootstrap keys. Database creation, the single-instance lease, and migrations
// happen before the plugin advertises scheduling capability.
func Open(cfg RuntimeConfig) (*Engine, error) {
	if err := migrateLegacyPluginDatabase(cfg.DatabasePath); err != nil {
		return nil, err
	}
	store, err := OpenStore(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	if err := store.EnsureFingerprintScheme(); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.CompactLegacyUsageEvents(); err != nil {
		_ = store.Close()
		return nil, err
	}
	settings, err := store.LoadOrCreateInstallationSettings(InstallationSettings{
		RequestUnits:    cfg.RequestUnits,
		RecordRetention: cfg.RecordRetention,
		AuthDirectory:   cfg.AuthDirectory,
	}, cfg.KeyHMACSecret)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	// Legacy account-group metadata stays untouched in SQLite for rollback, but
	// is intentionally not loaded into the scheduler: a Key quota is global
	// across all CPA-selected Codex accounts.
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
	accounts, err := store.LoadAccountPool()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	snapshots, err := store.LoadOfficialQuotaSnapshots()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	calibrations, err := store.LoadQuotaCalibrations()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	events, err := store.LoadMeteringSince(time.Now().UTC().Add(-sevenDayWindow))
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	allocationBuckets, err := store.LoadAllocationBuckets(time.Now().UTC())
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	// Bootstrap policies are a compatibility path, but they must obey the same
	// shared-pool and active-official-window guards as policies saved from the
	// panel. Validate before SQLite receives any missing bootstrap rows.
	policies = mergeBootstrapPolicies(policies, cfg.BootstrapKeys)
	bootstrapPolicies, _, err := buildPolicyMaps(cfg, policies)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	pool := make(map[string]AccountPoolEntry, len(accounts))
	for _, account := range accounts {
		pool[account.AuthID] = account
	}
	if err := validatePolicySetAgainstPool(bootstrapPolicies, pool, activeAllocationLedgerKeysFromRecords(allocationBuckets, time.Now().UTC())); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.InsertMissingPolicies(cfg.BootstrapKeys); err != nil {
		_ = store.Close()
		return nil, err
	}
	engine := &Engine{
		config:                     cfg,
		policiesByID:               make(map[string]KeyPolicy, len(policies)),
		policiesByHash:             make(map[string]string, len(policies)),
		states:                     engineStates{keys: make(map[string]*keyMeterState)},
		accountPool:                make(map[string]AccountPoolEntry, len(accounts)),
		quotaSnapshots:             make(map[string]OfficialQuotaSnapshot, len(snapshots)),
		quotaCalibrations:          make(map[string]quotaCalibration, len(calibrations)),
		store:                      store,
		allocationBuckets:          make(map[allocationBucketKey]allocationBucketState, len(allocationBuckets)),
		allocationBucketsByAuth:    make(map[string]map[allocationBucketKey]struct{}, len(accounts)),
		pendingAllocationBuckets:   make(map[allocationPendingKey]map[allocationBucketKey]struct{}),
		allocationCycles:           make(map[allocationCycleKey]allocationCycleState),
		officialAccountWindows:     make(map[officialAccountWindowKey]officialAccountWindowState),
		pendingSettlementsByKey:    make(map[string]int64),
		pendingSettlementsByAuth:   make(map[string]int64),
		pendingSettlementsByBucket: make(map[allocationBucketKey]int64),
		capturedRequestContent:     make(map[string]capturedRequestContent),
		pendingRequestContent:      make(map[allocationBucketKey][]pendingRequestContent),
		recentUsageCallbacks:       make(map[string]time.Time),
		allocationMutations:        make(chan allocationMutation, maxPendingAllocationMutations),
		allocationStop:             make(chan struct{}),
		allocationDone:             make(chan struct{}),
		flushStop:                  make(chan struct{}),
		flushDone:                  make(chan struct{}),
	}
	for index := range engine.pendingShards {
		engine.pendingShards[index].buckets = make(map[pendingBucketKey]UsageEvent)
		engine.pendingShards[index].logs = make([]DecisionLog, 0, 64)
	}
	if err := engine.replacePolicies(policies); err != nil {
		_ = store.Close()
		return nil, err
	}
	for _, account := range accounts {
		engine.accountPool[account.AuthID] = account
	}
	for _, snapshot := range snapshots {
		engine.quotaSnapshots[snapshot.AuthID] = snapshot
	}
	for _, calibration := range calibrations {
		engine.quotaCalibrations[calibration.AuthID] = calibration
	}
	engine.replaceMeterStates(events, time.Now().UTC(), engine.policiesByID)
	engine.replaceAllocationStates(allocationBuckets)
	// Older releases reserved request_units for every CPA scheduler pick. A
	// single request can cause several picks but only one terminal callback, so
	// normalize those legacy estimates into lightweight admission markers before
	// they can be mistaken for customer Token usage.
	if err := engine.conservativelyChargeUnsettledAllocations(); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("normalize unresolved allocation reservations: %w", err)
	}
	engine.rebuildOfficialAccountWindows(time.Now().UTC())
	go engine.allocationPersistenceLoop()
	go engine.flushLoop()
	return engine, nil
}

// replaceMeterStates rebuilds each managed Key's completed-token counter from
// durable minute buckets. Account selection is owned by CPA, so no account
// state is retained by this plugin.
func (engine *Engine) replaceMeterStates(events []UsageEvent, now time.Time, policies map[string]KeyPolicy) {
	byKey := make(map[string][]meterEvent, len(policies))
	for _, event := range events {
		if event.Scope == "key_actual" && event.KeyID != "" {
			byKey[event.KeyID] = append(byKey[event.KeyID], meterEvent{At: event.RecordedAt, Units: event.Units})
		}
	}
	next := engineStates{
		keys: make(map[string]*keyMeterState, len(policies)),
	}
	for keyID := range policies {
		next.keys[keyID] = newKeyMeterState(byKey[keyID], now)
	}
	engine.statesMu.Lock()
	defer engine.statesMu.Unlock()
	engine.states = next
}

func (engine *Engine) replaceAllocationStates(records []allocationBucketRecord) {
	engine.allocationMu.Lock()
	defer engine.allocationMu.Unlock()
	for _, record := range records {
		if record.KeyID == "" || record.AuthID == "" || record.WindowResetAt <= 0 || record.BucketAt <= 0 {
			continue
		}
		bucketKey := allocationBucketKey{
			KeyID:         record.KeyID,
			AuthID:        record.AuthID,
			WindowResetAt: record.WindowResetAt,
			BucketAt:      record.BucketAt,
		}
		bucket := allocationBucketState{Completed: record.CompletedUnits, Provisional: record.ProvisionalUnits, Reserved: record.ReservedUnits, Capacity: record.CapacityUnits, GlobalCapacity: record.GlobalCapacityUnits}
		if bucket.Completed == 0 && bucket.Provisional == 0 && bucket.Reserved == 0 {
			// Older versions could persist a terminal zero row after a failed
			// request. It carries no quota state and must not repopulate the
			// in-memory indexes on every restart.
			continue
		}
		engine.setAllocationBucketLocked(bucketKey, bucket)
		cycleKey := allocationCycleKey{KeyID: record.KeyID, AuthID: record.AuthID, WindowResetAt: record.WindowResetAt}
		cycle := engine.allocationCycles[cycleKey]
		cycle.Completed += bucket.Completed
		cycle.Provisional += bucket.Provisional
		cycle.Reserved += bucket.Reserved
		if bucket.Capacity > cycle.Capacity {
			// A mid-week allocation increase is written by a later bucket. Taking
			// the largest durable capacity preserves that monotonic change after a
			// restart and still fails safely for a partially migrated database.
			cycle.Capacity = bucket.Capacity
		}
		if bucket.GlobalCapacity > cycle.GlobalCapacity {
			cycle.GlobalCapacity = bucket.GlobalCapacity
		}
		engine.allocationCycles[cycleKey] = cycle
	}
}

// setAllocationBucketLocked updates the primary durable-bucket mirror plus
// key-only auth and pending-reservation indexes. Snapshot reconciliation only
// needs one CPA account, while terminal settlement only needs unresolved
// reservations; neither hot path scans every historical Key/account bucket.
func (engine *Engine) setAllocationBucketLocked(key allocationBucketKey, state allocationBucketState) {
	engine.allocationBuckets[key] = state
	if engine.allocationBucketsByAuth == nil {
		engine.allocationBucketsByAuth = make(map[string]map[allocationBucketKey]struct{})
	}
	byAuth := engine.allocationBucketsByAuth[key.AuthID]
	if byAuth == nil {
		byAuth = make(map[allocationBucketKey]struct{})
		engine.allocationBucketsByAuth[key.AuthID] = byAuth
	}
	byAuth[key] = struct{}{}
	pendingKey := allocationPendingKey{KeyID: key.KeyID, AuthID: key.AuthID}
	if state.Reserved > 0 {
		if engine.pendingAllocationBuckets == nil {
			engine.pendingAllocationBuckets = make(map[allocationPendingKey]map[allocationBucketKey]struct{})
		}
		pending := engine.pendingAllocationBuckets[pendingKey]
		if pending == nil {
			pending = make(map[allocationBucketKey]struct{})
			engine.pendingAllocationBuckets[pendingKey] = pending
		}
		pending[key] = struct{}{}
		return
	}
	if pending := engine.pendingAllocationBuckets[pendingKey]; pending != nil {
		delete(pending, key)
		if len(pending) == 0 {
			delete(engine.pendingAllocationBuckets, pendingKey)
		}
	}
}

// deleteAllocationBucketLocked removes a finished bucket from every in-memory
// index. Callers must hold allocationMu. An outstanding reservation is removed
// only after its official weekly window has conclusively reset; before then a
// late CPA callback must still be able to settle it safely.
func (engine *Engine) deleteAllocationBucketLocked(key allocationBucketKey, state allocationBucketState) {
	delete(engine.allocationBuckets, key)
	if byAuth := engine.allocationBucketsByAuth[key.AuthID]; byAuth != nil {
		delete(byAuth, key)
		if len(byAuth) == 0 {
			delete(engine.allocationBucketsByAuth, key.AuthID)
		}
	}
	pendingKey := allocationPendingKey{KeyID: key.KeyID, AuthID: key.AuthID}
	if pending := engine.pendingAllocationBuckets[pendingKey]; pending != nil {
		delete(pending, key)
		if len(pending) == 0 {
			delete(engine.pendingAllocationBuckets, pendingKey)
		}
	}
	cycleKey := allocationCycleKey{KeyID: key.KeyID, AuthID: key.AuthID, WindowResetAt: key.WindowResetAt}
	cycle := engine.allocationCycles[cycleKey]
	cycle.Completed -= state.Completed
	cycle.Provisional -= state.Provisional
	cycle.Reserved -= state.Reserved
	if cycle.Completed <= 0 && cycle.Provisional <= 0 && cycle.Reserved <= 0 {
		delete(engine.allocationCycles, cycleKey)
		return
	}
	engine.allocationCycles[cycleKey] = cycle
}

// expireAllocationReservationsAtOfficialReset retires every Key allocation
// bucket from an account's completed official weekly window and returns its
// unresolved reservations for pending-settlement cleanup. CPA has no request
// ID in this plugin ABI, so an ambiguous or missing callback cannot be
// reconciled exactly. Once Codex confirms the next weekly window, retaining
// any part of that old sub-ledger would incorrectly carry Key usage forward.
//
// The SQLite release is committed before the in-memory indexes are changed.
// Callers hold adminMu, which excludes admissions and usage callbacks while
// this rare per-account reconciliation runs.
func (engine *Engine) expireAllocationReservationsAtOfficialReset(authID string, previousResetAt time.Time) ([]expiredAllocationReservation, error) {
	if engine == nil || authID == "" || previousResetAt.IsZero() {
		return nil, nil
	}
	cutoff := previousResetAt.UTC().UnixMilli()
	engine.allocationMu.Lock()
	defer engine.allocationMu.Unlock()
	byAuth := engine.allocationBucketsByAuth[authID]
	staleKeys := make([]allocationBucketKey, 0, len(byAuth))
	keys := make([]allocationBucketKey, 0)
	for key := range byAuth {
		if key.WindowResetAt <= cutoff {
			staleKeys = append(staleKeys, key)
		}
	}
	sort.Slice(staleKeys, func(left, right int) bool {
		if staleKeys[left].WindowResetAt != staleKeys[right].WindowResetAt {
			return staleKeys[left].WindowResetAt < staleKeys[right].WindowResetAt
		}
		if staleKeys[left].BucketAt != staleKeys[right].BucketAt {
			return staleKeys[left].BucketAt < staleKeys[right].BucketAt
		}
		return staleKeys[left].KeyID < staleKeys[right].KeyID
	})
	for _, key := range staleKeys {
		if engine.allocationBuckets[key].Reserved > 0 {
			keys = append(keys, key)
		}
	}
	expired := make([]expiredAllocationReservation, 0, len(keys))
	for _, key := range keys {
		units := engine.allocationBuckets[key].Reserved
		expired = append(expired, expiredAllocationReservation{Key: key, Units: units})
	}
	// Retire the complete durable cycle, not only its reservations. Otherwise
	// completed/provisional rows can be loaded again after restart and make a
	// Key continue accumulating against an official account that already reset.
	if err := engine.store.DeleteAllocationBucketsThrough(authID, previousResetAt); err != nil {
		return nil, fmt.Errorf("retire old official-window allocations: %w", err)
	}
	for _, key := range staleKeys {
		engine.deleteAllocationBucketLocked(key, engine.allocationBuckets[key])
	}
	// The same reservations were reflected in the account-level primary and
	// secondary ledgers. Every old identity at or before this completed weekly
	// boundary is now historical and must not retain memory indefinitely.
	for key := range engine.officialAccountWindows {
		if key.AuthID == authID && key.WindowResetAt <= cutoff {
			delete(engine.officialAccountWindows, key)
		}
	}
	return expired, nil
}

const (
	officialSecondaryWindow = "secondary"
)

// officialAccountWindowTargets converts the current official weekly remaining
// percentage into fixed-point x units. The official percentage is the account
// guard authority; Token totals are attribution evidence only.
func (engine *Engine) officialAccountWindowTargets(entry AccountPoolEntry, snapshot OfficialQuotaSnapshot, requestUnits int64, now time.Time) []officialAccountWindowTarget {
	weekly := snapshot.Secondary
	if weekly.LimitWindowSeconds <= 0 {
		return nil
	}
	resetAt, ok := officialWeeklyResetAt(weekly, snapshot.ObservedAt, now)
	if !ok {
		return nil
	}
	remainingPercent := 100 - weekly.UsedPercent
	if remainingPercent < 0 {
		remainingPercent = 0
	}
	remainingX := entry.CapacityX * remainingPercent / 100
	return []officialAccountWindowTarget{{
		Key:           officialAccountWindowKey{AuthID: entry.AuthID, Kind: officialSecondaryWindow, WindowResetAt: resetAt.UnixMilli()},
		Capacity:      capacityForX(remainingX, officialXUnitsPerX),
		BaselineAt:    weekly.BaselineAt.UTC().UnixMilli(),
		WindowSeconds: weekly.LimitWindowSeconds,
	}}
}

// rebuildOfficialAccountWindows reconstructs the post-snapshot account-wide
// counters from durable Key allocation buckets. Each window carries the
// first successful observation as a durable baseline, so later delayed
// upstream snapshots cannot release locally completed work.
func (engine *Engine) rebuildOfficialAccountWindows(now time.Time) {
	if engine == nil {
		return
	}
	engine.configMu.RLock()
	requestUnits := engine.config.RequestUnits
	engine.configMu.RUnlock()
	engine.poolMu.RLock()
	entries := make(map[string]AccountPoolEntry, len(engine.accountPool))
	snapshots := make(map[string]OfficialQuotaSnapshot, len(engine.quotaSnapshots))
	for authID, entry := range engine.accountPool {
		entries[authID] = entry
	}
	for authID, snapshot := range engine.quotaSnapshots {
		snapshots[authID] = snapshot
	}
	engine.poolMu.RUnlock()
	engine.allocationMu.Lock()
	defer engine.allocationMu.Unlock()
	engine.officialAccountWindows = make(map[officialAccountWindowKey]officialAccountWindowState)
	for authID, entry := range entries {
		snapshot, exists := snapshots[authID]
		if !exists || snapshot.LastError != "" || !snapshot.usableAt(now) {
			continue
		}
		engine.replaceOfficialAccountWindowsLocked(entry, snapshot, requestUnits, now)
	}
}

// discardKeyAccounting removes every in-memory ledger owned by a reset or
// deleted Key.
// Completed account usage is deliberately not subtracted from the official
// account guard because Codex has already consumed it; only unresolved local
// reservations are released. Re-adding the same Key therefore starts its own
// policy and history at zero without inventing quota on the underlying account.
func (engine *Engine) discardKeyAccounting(keyID string) {
	if engine == nil || keyID == "" {
		return
	}
	engine.allocationMu.Lock()
	for key, bucket := range engine.allocationBuckets {
		if key.KeyID != keyID {
			continue
		}
		if bucket.Reserved > 0 {
			engine.releaseOfficialAccountReservationLocked(key.AuthID, officialSecondaryWindow, key.BucketAt, bucket.Reserved, 0)
		}
		engine.deleteAllocationBucketLocked(key, bucket)
	}
	engine.allocationMu.Unlock()

	engine.statesMu.Lock()
	delete(engine.states.keys, keyID)
	engine.statesMu.Unlock()
	engine.discardPendingSettlementsForKey(keyID)
}

// replaceOfficialAccountWindowsLocked re-bases one account when a successful
// official response arrives. Within one official reset identity it only
// tightens the available allowance and keeps locally charged work; a delayed
// upstream percentage can therefore never reopen quota. Old identities with
// in-flight reservations remain until their terminal callback settles.
func (engine *Engine) replaceOfficialAccountWindowsLocked(entry AccountPoolEntry, snapshot OfficialQuotaSnapshot, requestUnits int64, now time.Time) {
	targets := engine.officialAccountWindowTargets(entry, snapshot, requestUnits, now)
	if len(targets) == 0 {
		return
	}
	desired := make(map[officialAccountWindowKey]struct{}, len(targets))
	for _, target := range targets {
		desired[target.Key] = struct{}{}
	}
	for key, state := range engine.officialAccountWindows {
		if key.AuthID != entry.AuthID {
			continue
		}
		if _, current := desired[key]; !current && state.Reserved == 0 {
			delete(engine.officialAccountWindows, key)
		}
	}
	for _, target := range targets {
		if state, exists := engine.officialAccountWindows[target.Key]; exists {
			// A successful official snapshot already includes completed work.
			// Preserve only the smaller observed remainder against upstream
			// display jitter; local completions are not counted a second time.
			if target.Capacity < state.Capacity {
				state.Capacity = target.Capacity
			}
			state.Completed = 0
			state.WindowSeconds = target.WindowSeconds
			engine.officialAccountWindows[target.Key] = state
			continue
		}
		state := officialAccountWindowState{
			Capacity: target.Capacity, BaselineAt: target.BaselineAt, WindowSeconds: target.WindowSeconds,
		}
		windowStart := target.Key.WindowResetAt - target.WindowSeconds*int64(time.Second/time.Millisecond)
		for bucketKey := range engine.allocationBucketsByAuth[target.Key.AuthID] {
			bucket := engine.allocationBuckets[bucketKey]
			if bucketKey.BucketAt > target.Key.WindowResetAt || bucketKey.BucketAt < windowStart {
				continue
			}
			state.Reserved += bucket.Reserved
		}
		engine.officialAccountWindows[target.Key] = state
	}
}

func (engine *Engine) reserveOfficialAccountWindowsLocked(targets []officialAccountWindowTarget, requestUnits int64) error {
	for _, target := range targets {
		state, exists := engine.officialAccountWindows[target.Key]
		if !exists {
			return errOfficialWeeklyReset
		}
		if exceedsLimit(state.Completed+state.Reserved, requestUnits, state.Capacity) {
			return errOfficialAccountExhausted
		}
	}
	for _, target := range targets {
		state := engine.officialAccountWindows[target.Key]
		state.Reserved += requestUnits
		engine.officialAccountWindows[target.Key] = state
	}
	return nil
}

func (engine *Engine) releaseOfficialAccountReservationLocked(authID string, kind string, bucketAt int64, requestUnits, completedUnits int64) {
	var selected officialAccountWindowKey
	found := false
	for key, state := range engine.officialAccountWindows {
		if key.AuthID != authID || key.Kind != kind || state.Reserved < requestUnits {
			continue
		}
		windowStart := key.WindowResetAt - state.WindowSeconds*int64(time.Second/time.Millisecond)
		if bucketAt >= windowStart && bucketAt <= key.WindowResetAt {
			selected = key
			found = true
			break
		}
		if !found || key.WindowResetAt < selected.WindowResetAt {
			selected = key
			found = true
		}
	}
	if !found {
		return
	}
	state := engine.officialAccountWindows[selected]
	state.Reserved -= requestUnits
	// Completed Token usage is already represented by the next official
	// percentage snapshot. Adding it here would mix units and double-charge.
	engine.officialAccountWindows[selected] = state
}

// allocationPersistenceLoop makes a positive admission reservation durable
// before Admit returns. It batches short bursts for SQLite efficiency without
// placing a timer or network operation on CPA's scheduler path.
func (engine *Engine) allocationPersistenceLoop() {
	ticker := time.NewTicker(persistenceFlushInterval)
	defer func() {
		ticker.Stop()
		close(engine.allocationDone)
	}()
	for {
		select {
		case <-engine.allocationStop:
			// Close sends this only after it has stopped both admissions and usage
			// callbacks and committed an ordered allocation barrier. No producer can
			// enqueue another mutation after that barrier, so a second best-effort
			// drain would only create an unrecoverable failure after this worker has
			// exited.
			return
		case <-ticker.C:
			engine.retryAllocationMutations()
		case first := <-engine.allocationMutations:
			engine.flushAllocationBatch(first, false)
		}
	}
}

func (engine *Engine) flushAllocationMutations(draining bool) {
	for {
		select {
		case mutation := <-engine.allocationMutations:
			engine.flushAllocationBatch(mutation, draining)
		default:
			return
		}
	}
}

func (engine *Engine) flushAllocationBatch(first allocationMutation, draining bool) {
	batch := []allocationMutation{first}
	if !draining {
		timer := time.NewTimer(allocationPersistenceFlushInterval)
		defer timer.Stop()
		for len(batch) < maxPendingAllocationMutations {
			select {
			case mutation := <-engine.allocationMutations:
				batch = append(batch, mutation)
			case <-timer.C:
				goto persist
			case <-engine.allocationStop:
				// The current batch is still committed before Close releases SQLite.
				goto persist
			}
		}
	} else {
		for len(batch) < maxPendingAllocationMutations {
			select {
			case mutation := <-engine.allocationMutations:
				batch = append(batch, mutation)
			default:
				goto persist
			}
		}
	}

persist:
	engine.persistAllocationBatch(batch)
}

// persistAllocationBatch only acknowledges an admission after its reservation
// transaction commits. Failed completion settlements are retained for ordered
// retry; they must never be discarded merely because ordinary usage/log flushes
// later succeed.
func (engine *Engine) persistAllocationBatch(batch []allocationMutation) {
	err := engine.store.applyAllocationMutations(batch)
	if err != nil {
		engine.persistenceFailures.Add(1)
		engine.allocationDegraded.Store(true)
		engine.allocationRetryMu.Lock()
		for _, mutation := range batch {
			if mutation.done == nil {
				engine.allocationRetry = append(engine.allocationRetry, mutation)
			}
		}
		engine.allocationRetryMu.Unlock()
	}
	for _, mutation := range batch {
		if mutation.done != nil {
			mutation.done <- err
			close(mutation.done)
		}
	}
	if err == nil {
		engine.clearAllocationDegradedIfRecovered()
	}
}

func (engine *Engine) retryAllocationMutations() {
	if !engine.allocationDegraded.Load() {
		return
	}
	engine.allocationRetryMu.Lock()
	retry := engine.allocationRetry
	engine.allocationRetry = nil
	engine.allocationRetryMu.Unlock()
	if len(retry) == 0 {
		// A failed pre-dispatch reservation has no permitted request to replay.
		// Commit a no-op transaction as a health probe before reopening managed
		// traffic; ordinary request-log persistence is not that proof.
		retry = []allocationMutation{{}}
	}
	if err := engine.store.applyAllocationMutations(retry); err != nil {
		engine.persistenceFailures.Add(1)
		engine.allocationDegraded.Store(true)
		engine.allocationRetryMu.Lock()
		for _, mutation := range retry {
			if mutation.done == nil && (mutation.ReservedDelta != 0 || mutation.CompletedDelta != 0 || mutation.ProvisionalDelta != 0) {
				engine.allocationRetry = append(engine.allocationRetry, mutation)
			}
		}
		engine.allocationRetryMu.Unlock()
		return
	}
	engine.clearAllocationDegradedIfRecovered()
}

func (engine *Engine) clearAllocationDegradedIfRecovered() {
	engine.allocationRetryMu.Lock()
	pending := len(engine.allocationRetry)
	engine.allocationRetryMu.Unlock()
	if pending == 0 {
		engine.allocationDegraded.Store(false)
	}
}

func (engine *Engine) submitAllocationMutation(mutation allocationMutation, wait bool) error {
	return engine.submitAllocationMutationUntil(mutation, wait, time.Time{})
}

// submitAllocationMutationUntil prevents Close from waiting beyond its
// deadline when SQLite is busy. A barrier that has already entered the channel
// is still safe for the worker to finish after the caller has returned.
func (engine *Engine) submitAllocationMutationUntil(mutation allocationMutation, wait bool, deadline time.Time) error {
	if wait {
		mutation.done = make(chan error, 1)
	}
	if deadline.IsZero() {
		select {
		case engine.allocationMutations <- mutation:
		case <-engine.allocationStop:
			return fmt.Errorf("allocation persistence is stopping")
		}
	} else {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("allocation persistence deadline reached before barrier submission")
		}
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case engine.allocationMutations <- mutation:
		case <-engine.allocationStop:
			return fmt.Errorf("allocation persistence is stopping")
		case <-timer.C:
			return fmt.Errorf("allocation persistence deadline reached before barrier submission")
		}
	}
	if !wait {
		return nil
	}
	if deadline.IsZero() {
		return <-mutation.done
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("allocation persistence deadline reached before barrier completion")
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case err := <-mutation.done:
		return err
	case <-timer.C:
		return fmt.Errorf("allocation persistence deadline reached before barrier completion")
	}
}

// flushAllocationPersistence is an ordered barrier for administrative deletes.
// It ensures a settlement queued just before a Key or account is removed
// cannot recreate that deleted ledger row after the SQLite delete commits.
func (engine *Engine) flushAllocationPersistence() error {
	return engine.flushAllocationPersistenceUntil(time.Time{})
}

func (engine *Engine) flushAllocationPersistenceUntil(deadline time.Time) error {
	if err := engine.submitAllocationMutationUntil(allocationMutation{}, true, deadline); err != nil {
		return err
	}
	if engine.allocationDegraded.Load() {
		return fmt.Errorf("allocation settlements are still pending durable retry")
	}
	return nil
}

// waitForAllocationPersistenceUntil keeps retrying ordered allocation
// settlements until they become durable or the supplied shutdown deadline is
// reached. A timeout deliberately retains the SQLite lease: restarting from a
// lower on-disk counter is less safe than remaining unavailable.
func (engine *Engine) waitForAllocationPersistenceUntil(deadline time.Time) error {
	var lastErr error
	for {
		err := engine.flushAllocationPersistenceUntil(deadline)
		if err == nil {
			return nil
		}
		lastErr = err
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("allocation settlements did not become durable before shutdown deadline: %w", lastErr)
		}
		if remaining > 100*time.Millisecond {
			remaining = 100 * time.Millisecond
		}
		timer := time.NewTimer(remaining)
		<-timer.C
	}
}

type allocationTarget struct {
	AuthID          string
	WindowResetAt   time.Time
	Capacity        int64
	OfficialWindows []officialAccountWindowTarget
}

// globalAllocationStateLocked aggregates every still-active account cycle for
// one Key into one balance. Callers must hold allocationMu. Rows written by
// earlier plugin versions have GlobalCapacity == 0 and are kept as one summed
// legacy allowance until their official account windows reset; this prevents a
// live upgrade from silently releasing already-admitted usage.
func (engine *Engine) globalAllocationStateLocked(keyID string, now time.Time, policyCapacity int64) (capacity int64, used int64, resetAt *time.Time) {
	capacity = policyCapacity
	var legacyCapacity int64
	cutoff := now.UTC().UnixMilli()
	for cycleKey, cycle := range engine.allocationCycles {
		if cycleKey.KeyID != keyID || cycleKey.WindowResetAt <= cutoff {
			continue
		}
		used += cycle.Completed + cycle.Provisional + cycle.Reserved
		if cycle.GlobalCapacity > capacity {
			capacity = cycle.GlobalCapacity
		}
		if cycle.GlobalCapacity == 0 {
			legacyCapacity += cycle.Capacity
		}
		candidate := time.UnixMilli(cycleKey.WindowResetAt).UTC()
		if resetAt == nil || candidate.Before(*resetAt) {
			resetAt = &candidate
		}
	}
	if legacyCapacity > capacity {
		capacity = legacyCapacity
	}
	return capacity, used, resetAt
}

// allocationTargets finds accounts that may serve a managed request. Every
// target carries the same Key-wide capacity: account capacity is a pool and
// official-window guard, never a proportional slice of this Key's x balance.
func (engine *Engine) allocationTargets(policy KeyPolicy, requestUnits int64, now time.Time) []allocationTarget {
	globalCapacity := engine.globalAllocationCapacity(policy, requestUnits)
	if globalCapacity <= 0 {
		return nil
	}
	engine.poolMu.RLock()
	defer engine.poolMu.RUnlock()
	if policy.AllocationX <= 0 {
		return nil
	}
	targets := make([]allocationTarget, 0, len(engine.accountPool))
	for authID, entry := range engine.accountPool {
		if !entry.Enabled || entry.CapacityX <= 0 {
			continue
		}
		snapshot, known := engine.quotaSnapshots[authID]
		if !known || snapshot.LastError != "" || !snapshot.usableAt(now) || snapshot.hasPendingEstimatedSecondaryReset() {
			continue
		}
		resetAt, ok := officialWeeklyResetAt(snapshot.Secondary, snapshot.ObservedAt, now)
		if !ok {
			continue
		}
		officialWindows := engine.officialAccountWindowTargets(entry, snapshot, requestUnits, now)
		if len(officialWindows) == 0 {
			continue
		}
		targets = append(targets, allocationTarget{AuthID: authID, WindowResetAt: resetAt, Capacity: globalCapacity, OfficialWindows: officialWindows})
	}
	return targets
}

func officialWeeklyResetAt(window OfficialQuotaWindow, observedAt, now time.Time) (time.Time, bool) {
	if window.ResetAt != nil {
		if window.ResetAt.After(now.UTC()) {
			return window.ResetAt.UTC(), true
		}
		// A reset_after_seconds fallback can be fractionally early because it is
		// converted using the local receive time. Do not derive a new weekly
		// ledger during that short uncertainty interval; the stale-candidate path
		// will refresh the official snapshot instead.
		if now.UTC().Sub(*window.ResetAt) <= quotaResetStabilityTolerance {
			return time.Time{}, false
		}
	}
	if window.LimitWindowSeconds > 0 && !observedAt.IsZero() {
		resetAt := observedAt.UTC().Add(time.Duration(window.LimitWindowSeconds) * time.Second)
		if resetAt.After(now.UTC()) {
			return resetAt, true
		}
	}
	return time.Time{}, false
}

func (engine *Engine) reserveAccountAllocation(keyID, authID string, policy KeyPolicy, capacityRequestUnits, reservationUnits int64, now time.Time) (allocationBucketKey, error) {
	if reservationUnits <= 0 {
		return allocationBucketKey{}, fmt.Errorf("reservation units must be positive")
	}
	var target *allocationTarget
	for _, candidate := range engine.allocationTargets(policy, capacityRequestUnits, now) {
		if candidate.AuthID == authID {
			copy := candidate
			target = &copy
			break
		}
	}
	if target == nil {
		return allocationBucketKey{}, errOfficialWeeklyReset
	}
	bucketKey := allocationBucketKey{
		KeyID:         keyID,
		AuthID:        authID,
		WindowResetAt: target.WindowResetAt.UnixMilli(),
		BucketAt:      usageBucketEnd(now).UnixMilli(),
	}
	cycleKey := allocationCycleKey{KeyID: keyID, AuthID: authID, WindowResetAt: bucketKey.WindowResetAt}
	engine.allocationMu.Lock()
	cycle := engine.allocationCycles[cycleKey]
	previousCycleCapacity := cycle.Capacity
	previousCycleGlobalCapacity := cycle.GlobalCapacity
	capacity, used, _ := engine.globalAllocationStateLocked(keyID, now, target.Capacity)
	if exceedsLimit(used, reservationUnits, capacity) {
		engine.allocationMu.Unlock()
		return allocationBucketKey{}, errAllocationExhausted
	}
	if target.Capacity > cycle.GlobalCapacity {
		cycle.GlobalCapacity = target.Capacity
	}
	// Keep the current-cycle capacity in the durable row so reporting and
	// configuration changes do not reinterpret confirmed x usage.
	if capacity > cycle.Capacity {
		cycle.Capacity = capacity
	}
	if err := engine.reserveOfficialAccountWindowsLocked(target.OfficialWindows, reservationUnits); err != nil {
		engine.allocationMu.Unlock()
		return allocationBucketKey{}, err
	}
	bucket := engine.allocationBuckets[bucketKey]
	previousBucketCapacity := bucket.Capacity
	previousBucketGlobalCapacity := bucket.GlobalCapacity
	if capacity > bucket.Capacity {
		bucket.Capacity = capacity
	}
	if target.Capacity > bucket.GlobalCapacity {
		bucket.GlobalCapacity = target.Capacity
	}
	bucket.Reserved += reservationUnits
	cycle.Reserved += reservationUnits
	engine.setAllocationBucketLocked(bucketKey, bucket)
	engine.allocationCycles[cycleKey] = cycle
	engine.allocationMu.Unlock()

	mutation := allocationMutation{Key: bucketKey, ReservedDelta: reservationUnits, CapacityUnits: capacity, GlobalCapacityUnits: target.Capacity}
	if err := engine.submitAllocationMutation(mutation, true); err != nil {
		// The worker commits the whole batch or rolls it back. Undo this request's
		// in-memory reservation before returning a 429; no upstream request has
		// been allowed yet, so that rollback cannot under-charge usage.
		engine.allocationMu.Lock()
		bucket = engine.allocationBuckets[bucketKey]
		bucket.Reserved -= reservationUnits
		if bucket.Reserved < 0 {
			bucket.Reserved = 0
		}
		bucket.Capacity = previousBucketCapacity
		bucket.GlobalCapacity = previousBucketGlobalCapacity
		cycle = engine.allocationCycles[cycleKey]
		cycle.Reserved -= reservationUnits
		if cycle.Reserved < 0 {
			cycle.Reserved = 0
		}
		cycle.Capacity = previousCycleCapacity
		cycle.GlobalCapacity = previousCycleGlobalCapacity
		engine.setAllocationBucketLocked(bucketKey, bucket)
		engine.allocationCycles[cycleKey] = cycle
		if bucket.Completed == 0 && bucket.Provisional == 0 && bucket.Reserved == 0 {
			engine.deleteAllocationBucketLocked(bucketKey, bucket)
		}
		for _, window := range target.OfficialWindows {
			state := engine.officialAccountWindows[window.Key]
			state.Reserved -= reservationUnits
			if state.Reserved < 0 {
				state.Reserved = 0
			}
			engine.officialAccountWindows[window.Key] = state
		}
		engine.allocationMu.Unlock()
		return allocationBucketKey{}, err
	}
	return bucketKey, nil
}

// conservativelyChargeUnsettledAllocations migrates full request estimates
// written by older releases into one-Token admission markers. The historical
// name is retained to keep the shutdown and startup call sites stable.
func (engine *Engine) conservativelyChargeUnsettledAllocations() error {
	if engine == nil {
		return nil
	}
	engine.configMu.RLock()
	legacyReservationUnits := engine.config.RequestUnits
	engine.configMu.RUnlock()
	if legacyReservationUnits <= admissionReservationUnits {
		return nil
	}

	// Versions before the marker-based ledger reserved request_units for every
	// scheduler pick. CPA can issue several picks for one real request, so those
	// rows must not be restored as customer usage or expanded to the remaining
	// Key capacity after restart. Convert each old estimate into one 1-Token
	// marker per inferred pick. The conversion is monotonic and idempotent.
	engine.allocationMu.Lock()
	defer engine.allocationMu.Unlock()
	mutations := make([]allocationMutation, 0)
	for key, bucket := range engine.allocationBuckets {
		if bucket.Reserved < legacyReservationUnits {
			continue
		}
		markers := bucket.Reserved / legacyReservationUnits
		if bucket.Reserved%legacyReservationUnits != 0 {
			markers++
		}
		if markers < admissionReservationUnits {
			markers = admissionReservationUnits
		}
		if markers >= bucket.Reserved {
			continue
		}
		mutations = append(mutations, allocationMutation{
			Key: key, ReservedDelta: markers - bucket.Reserved,
		})
	}
	if len(mutations) == 0 {
		return nil
	}
	if err := engine.store.applyAllocationMutations(mutations); err != nil {
		return fmt.Errorf("normalize recovered admission reservations: %w", err)
	}
	for _, mutation := range mutations {
		bucket := engine.allocationBuckets[mutation.Key]
		bucket.Reserved += mutation.ReservedDelta
		engine.setAllocationBucketLocked(mutation.Key, bucket)
		cycleKey := allocationCycleKey{KeyID: mutation.Key.KeyID, AuthID: mutation.Key.AuthID, WindowResetAt: mutation.Key.WindowResetAt}
		cycle := engine.allocationCycles[cycleKey]
		cycle.Reserved += mutation.ReservedDelta
		engine.allocationCycles[cycleKey] = cycle
	}
	return nil
}

func (engine *Engine) settleAccountAllocation(keyID, authID string, bucketAt, provisionalAt time.Time, requestUnits, provisionalUnits, provisionalLimit int64) allocationSettlement {
	if authID == "" || requestUnits <= 0 {
		return allocationSettlement{}
	}
	bucketMillis := bucketAt.UTC().UnixMilli()
	engine.allocationMu.Lock()
	var selected allocationBucketKey
	exactMatch := false
	fallbackCount := 0
	pending := engine.pendingAllocationBuckets[allocationPendingKey{KeyID: keyID, AuthID: authID}]
	for key := range pending {
		bucket := engine.allocationBuckets[key]
		if bucket.Reserved < requestUnits {
			continue
		}
		if key.BucketAt == bucketMillis {
			selected = key
			exactMatch = true
			break
		}
		fallbackCount++
		if fallbackCount == 1 {
			selected = key
		}
	}
	if !exactMatch && fallbackCount != 1 {
		engine.allocationMu.Unlock()
		return allocationSettlement{Ambiguous: fallbackCount > 1}
	}
	bucket := engine.allocationBuckets[selected]
	bucket.Reserved -= requestUnits
	cycleKey := allocationCycleKey{KeyID: selected.KeyID, AuthID: selected.AuthID, WindowResetAt: selected.WindowResetAt}
	cycle := engine.allocationCycles[cycleKey]
	cycle.Reserved -= requestUnits
	// Keep the temporary Token-derived guard inside one observable official
	// percentage step for this Key/account cycle. The next measurable official
	// poll replaces the entire interval, so growing past this bound only creates
	// false early 429s without adding reliable evidence.
	if provisionalUnits > 0 {
		remainingProvisional := provisionalLimit - cycle.Provisional
		if provisionalLimit <= 0 || remainingProvisional <= 0 {
			provisionalUnits = 0
		} else if provisionalUnits > remainingProvisional {
			provisionalUnits = remainingProvisional
		}
	}
	provisionalKey := selected
	if provisionalUnits > 0 {
		provisionalKey.BucketAt = usageBucketEnd(provisionalAt.UTC()).UnixMilli()
		cycle.Provisional += provisionalUnits
	}
	if provisionalUnits > 0 && provisionalKey == selected {
		bucket.Provisional += provisionalUnits
		engine.setAllocationBucketLocked(selected, bucket)
	} else {
		engine.setAllocationBucketLocked(selected, bucket)
		if bucket.Completed == 0 && bucket.Provisional == 0 && bucket.Reserved == 0 {
			engine.deleteAllocationBucketLocked(selected, bucket)
		}
		if provisionalUnits > 0 {
			provisionalBucket := engine.allocationBuckets[provisionalKey]
			provisionalBucket.Provisional += provisionalUnits
			if bucket.Capacity > provisionalBucket.Capacity {
				provisionalBucket.Capacity = bucket.Capacity
			}
			if bucket.GlobalCapacity > provisionalBucket.GlobalCapacity {
				provisionalBucket.GlobalCapacity = bucket.GlobalCapacity
			}
			engine.setAllocationBucketLocked(provisionalKey, provisionalBucket)
		}
	}
	if cycle.Completed <= 0 && cycle.Provisional <= 0 && cycle.Reserved <= 0 {
		delete(engine.allocationCycles, cycleKey)
	} else {
		engine.allocationCycles[cycleKey] = cycle
	}
	engine.releaseOfficialAccountReservationLocked(authID, officialSecondaryWindow, selected.BucketAt, requestUnits, 0)
	engine.allocationMu.Unlock()
	mutation := allocationMutation{
		Key: selected, ReservedDelta: -requestUnits,
		CapacityUnits: bucket.Capacity, GlobalCapacityUnits: bucket.GlobalCapacity,
	}
	if provisionalUnits > 0 {
		mutation.ProvisionalKey = provisionalKey
		mutation.ProvisionalDelta = provisionalUnits
	}
	if err := engine.submitAllocationMutation(mutation, false); err != nil {
		engine.persistenceFailures.Add(1)
		engine.allocationDegraded.Store(true)
		// The mutation has already been queued for ordered durable retry. It is a
		// real matched completion, so callers must not leave Close waiting for a
		// second terminal callback that CPA will never send.
		return allocationSettlement{Matched: true, Key: selected, BucketAt: time.UnixMilli(selected.BucketAt).UTC()}
	}
	return allocationSettlement{Matched: true, Durable: true, Key: selected, BucketAt: time.UnixMilli(selected.BucketAt).UTC()}
}

// activeAllocationLedgerKeys returns every Key whose confirmed, provisional,
// or reserved allocation still belongs to an official weekly window.
// Configuration must keep these allocations accounted for until Codex has
// reset that window; otherwise a pool or Key edit could create a second
// allowance for the same shared-pool period.
func (engine *Engine) activeAllocationLedgerKeys(now time.Time) map[string]struct{} {
	active := make(map[string]struct{})
	if engine == nil {
		return active
	}
	cutoff := now.UTC().UnixMilli()
	engine.allocationMu.Lock()
	defer engine.allocationMu.Unlock()
	for key, bucket := range engine.allocationBuckets {
		if key.WindowResetAt > cutoff && (bucket.Completed > 0 || bucket.Provisional > 0 || bucket.Reserved > 0) {
			active[key.KeyID] = struct{}{}
		}
	}
	return active
}

func activeAllocationLedgerKeysFromRecords(records []allocationBucketRecord, now time.Time) map[string]struct{} {
	active := make(map[string]struct{})
	cutoff := now.UTC().UnixMilli()
	for _, record := range records {
		if record.KeyID != "" && record.WindowResetAt > cutoff && (record.CompletedUnits > 0 || record.ProvisionalUnits > 0 || record.ReservedUnits > 0) {
			active[record.KeyID] = struct{}{}
		}
	}
	return active
}

// hasActiveAllocationLedger reports whether changing a shared-pool setting
// would reinterpret metered x usage in an account window that has not reached
// its official reset yet.
func (engine *Engine) hasActiveAllocationLedger(now time.Time) bool {
	return len(engine.activeAllocationLedgerKeys(now)) > 0
}

func (engine *Engine) hasActiveAllocationLedgerForKey(keyID string, now time.Time) bool {
	if keyID == "" {
		return false
	}
	_, active := engine.activeAllocationLedgerKeys(now)[keyID]
	return active
}

// beginPendingSettlement tracks only one process-local callback wait per
// minute bucket. CPA may ask the scheduler for several candidates for one real
// request, while it emits only one terminal usage callback; counting every pick
// here would make shutdown and management wait for callbacks that cannot exist.
func (engine *Engine) beginPendingSettlement(key allocationBucketKey) {
	if key.KeyID == "" || key.AuthID == "" {
		return
	}
	engine.settlementMu.Lock()
	if engine.pendingSettlementsByBucket[key] > 0 {
		engine.settlementMu.Unlock()
		return
	}
	engine.pendingSettlementsByKey[key.KeyID]++
	engine.pendingSettlementsByAuth[key.AuthID]++
	engine.pendingSettlementsByBucket[key] = 1
	engine.pendingSettlements.Add(1)
	engine.settlementMu.Unlock()
}

// discardPendingSettlementsForKey removes process-local callback waits after
// an administrator explicitly deletes a Key. The durable Key ledger is purged
// in the same management operation, so a late callback is intentionally
// treated as unmanaged usage rather than reviving deleted history.
func (engine *Engine) discardPendingSettlementsForKey(keyID string) {
	if engine == nil || keyID == "" {
		return
	}
	engine.settlementMu.Lock()
	for key, count := range engine.pendingSettlementsByBucket {
		if key.KeyID == keyID {
			engine.releasePendingSettlementLocked(key, count)
		}
	}
	engine.settlementMu.Unlock()
	engine.discardPendingRequestContentForKey(keyID)
}

func (engine *Engine) pendingSettlementsForKey(keyID string) int64 {
	engine.settlementMu.Lock()
	defer engine.settlementMu.Unlock()
	return engine.pendingSettlementsByKey[keyID]
}

func (engine *Engine) pendingSettlementsForAccount(authID string) int64 {
	engine.settlementMu.Lock()
	defer engine.settlementMu.Unlock()
	return engine.pendingSettlementsByAuth[authID]
}

// PendingSettlementCount is used by the native shutdown bridge to keep the
// quota synchronizer alive only while an admitted request still needs a
// terminal callback or an official weekly-reset reconciliation.
func (engine *Engine) PendingSettlementCount() int64 {
	if engine == nil {
		return 0
	}
	return engine.pendingSettlements.Load()
}

// PendingSettlementAuthIDs returns the distinct CPA accounts that still need
// a terminal usage callback or official-reset reconciliation. The shutdown
// synchronizer uses this bounded snapshot to avoid polling unrelated accounts
// while the plugin is draining safely.
func (engine *Engine) PendingSettlementAuthIDs() []string {
	if engine == nil {
		return nil
	}
	engine.settlementMu.Lock()
	ids := make([]string, 0, len(engine.pendingSettlementsByAuth))
	for authID, pending := range engine.pendingSettlementsByAuth {
		if pending > 0 {
			ids = append(ids, authID)
		}
	}
	engine.settlementMu.Unlock()
	sort.Strings(ids)
	return ids
}

// HasPendingSettlementForAuth reports whether this process still awaits a
// terminal CPA usage callback for the account. The native shutdown bridge uses
// it while holding its refresh gate, so an unrelated account cannot begin a
// local auth-file read after graceful draining has started.
func (engine *Engine) HasPendingSettlementForAuth(authID string) bool {
	if engine == nil || strings.TrimSpace(authID) == "" {
		return false
	}
	engine.settlementMu.Lock()
	pending := engine.pendingSettlementsByAuth[authID] > 0
	engine.settlementMu.Unlock()
	return pending
}

// CloseAdmissions starts a graceful shutdown without touching persistence.
// The native bridge calls it before deciding whether its quota synchronizer
// can stop: no new request can appear after this boundary, while an existing
// pending request may still need an official weekly-reset reconciliation.
func (engine *Engine) CloseAdmissions() {
	if engine == nil {
		return
	}
	engine.adminMu.Lock()
	engine.admissionsClosed.Store(true)
	engine.adminMu.Unlock()
}

// finishPendingSettlement is intentionally separate from the allocation
// mutation. A duplicated or recovered callback can find no process-local
// reservation; it must never drain another request's in-flight counter.
func (engine *Engine) finishPendingSettlement(key allocationBucketKey, settled bool) {
	if !settled || key.KeyID == "" || key.AuthID == "" {
		return
	}
	engine.settlementMu.Lock()
	defer engine.settlementMu.Unlock()
	if engine.pendingSettlementsByBucket[key] <= 0 {
		return
	}
	engine.releasePendingSettlementLocked(key, 1)
}

// releasePendingSettlementLocked releases process-local pending requests for
// one exact reservation bucket. Callers hold settlementMu.
func (engine *Engine) releasePendingSettlementLocked(key allocationBucketKey, count int64) int64 {
	if count <= 0 {
		return 0
	}
	available := engine.pendingSettlementsByBucket[key]
	if available <= 0 {
		return 0
	}
	if count > available {
		count = available
	}
	if available == count {
		delete(engine.pendingSettlementsByBucket, key)
	} else {
		engine.pendingSettlementsByBucket[key] = available - count
	}
	if engine.pendingSettlementsByKey[key.KeyID] <= count {
		delete(engine.pendingSettlementsByKey, key.KeyID)
	} else {
		engine.pendingSettlementsByKey[key.KeyID] -= count
	}
	if engine.pendingSettlementsByAuth[key.AuthID] <= count {
		delete(engine.pendingSettlementsByAuth, key.AuthID)
	} else {
		engine.pendingSettlementsByAuth[key.AuthID] -= count
	}
	engine.pendingSettlements.Add(-count)
	return count
}

// expirePendingSettlements releases only this process's callback waits for
// reservations that were durably ended at an official weekly reset. Durable
// reservations recovered after a crash have no process-local counter and are
// deliberately ignored here.
func (engine *Engine) expirePendingSettlements(expired []expiredAllocationReservation) int64 {
	if len(expired) == 0 {
		return 0
	}
	engine.settlementMu.Lock()
	defer engine.settlementMu.Unlock()
	var released int64
	for _, item := range expired {
		released += engine.releasePendingSettlementLocked(item.Key, engine.pendingSettlementsByBucket[item.Key])
	}
	return released
}

// waitForPendingSettlementsUntil waits only during shutdown. An admitted
// request has already placed a durable correlation marker, but its CPA
// completion can be larger; releasing SQLite before that completion arrives
// would lose the terminal actual-Token settlement.
func (engine *Engine) waitForPendingSettlementsUntil(deadline time.Time) error {
	for engine.pendingSettlements.Load() > 0 {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%d admitted request settlements are still pending", engine.pendingSettlements.Load())
		}
		if remaining > 50*time.Millisecond {
			remaining = 50 * time.Millisecond
		}
		timer := time.NewTimer(remaining)
		<-timer.C
	}
	return nil
}

// IsClosing reports the recoverable shutdown state used by the native bridge.
// It remains true after a timed-out close so reconfiguration cannot open a
// second SQLite instance before the first one has finished its safe drain.
func (engine *Engine) IsClosing() bool {
	return engine != nil && engine.admissionsClosed.Load() && !engine.closed.Load()
}

// Close releases SQLite resources and the lease that prevents a second local
// proxy instance from independently admitting the same pool. It is retryable:
// a transient SQLite failure or a late CPA completion leaves the existing
// engine alive and fail-closed, rather than consuming a one-shot close path.
func (engine *Engine) Close() error {
	if engine == nil {
		return nil
	}
	engine.closeMu.Lock()
	defer engine.closeMu.Unlock()
	if engine.closed.Load() {
		return engine.closeErr
	}

	// Block new scheduler picks first, but deliberately leave RecordUsage open
	// for requests that CPA has already accepted. The admin write lock also
	// waits for an in-progress admission before this state transition completes.
	engine.adminMu.Lock()
	engine.admissionsClosed.Store(true)
	engine.adminMu.Unlock()

	deadline := time.Now().Add(allocationCloseDrainTimeout)
	if err := engine.waitForPendingSettlementsUntil(deadline); err != nil {
		engine.closeErr = fmt.Errorf("codex-carpool shutdown deferred: %w", err)
		return engine.closeErr
	}

	// Once the counter has drained, exclude any new terminal callback before
	// the allocation worker stops. RecordUsage holds this same read lock for its
	// whole settlement, so no mutation can be enqueued after this point.
	engine.adminMu.Lock()
	if pending := engine.pendingSettlements.Load(); pending != 0 {
		engine.adminMu.Unlock()
		engine.closeErr = fmt.Errorf("codex-carpool shutdown deferred: %d admitted request settlements are still pending", pending)
		return engine.closeErr
	}
	engine.usageClosed.Store(true)
	engine.adminMu.Unlock()

	if err := engine.waitForAllocationPersistenceUntil(deadline); err != nil {
		engine.closeErr = err
		return engine.closeErr
	}
	close(engine.allocationStop)
	<-engine.allocationDone
	close(engine.flushStop)
	<-engine.flushDone
	flushErr := engine.flushPending()
	closeErr := engine.store.Close()
	if flushErr != nil {
		engine.closeErr = flushErr
	} else {
		engine.closeErr = closeErr
	}
	engine.closed.Store(true)
	return engine.closeErr
}

// CloseConservatively is the bounded native-plugin shutdown path. When CPA
// cannot deliver a terminal usage callback before its shutdown deadline, every
// admitted request already has a durable pre-dispatch reservation. Charge the
// remainder of its Key allocation instead of waiting indefinitely for a
// callback that may never arrive after CPA stops. A later plugin instance
// rebuilds that conservative charge until the corresponding official reset,
// so this path cannot create a restart quota bypass.
func (engine *Engine) CloseConservatively() error {
	if engine == nil {
		return nil
	}
	engine.closeMu.Lock()
	defer engine.closeMu.Unlock()
	if engine.closed.Load() {
		return engine.closeErr
	}

	// Exclude new admissions and terminal callbacks before workers stop. An
	// already queued completion that cannot become durable remains represented
	// by its earlier durable reservation rather than being released.
	engine.adminMu.Lock()
	engine.admissionsClosed.Store(true)
	engine.usageClosed.Store(true)
	engine.adminMu.Unlock()

	deadline := time.Now().Add(allocationCloseDrainTimeout)
	persistenceErr := engine.waitForAllocationPersistenceUntil(deadline)
	if persistenceErr == nil {
		// Normalize any legacy full-size scheduler estimates before releasing
		// SQLite. New admissions already use one-Token correlation markers.
		persistenceErr = engine.conservativelyChargeUnsettledAllocations()
	}
	close(engine.allocationStop)
	<-engine.allocationDone
	close(engine.flushStop)
	<-engine.flushDone
	flushErr := engine.flushPending()
	storeErr := engine.store.Close()
	engine.closed.Store(true)
	for _, err := range []error{persistenceErr, flushErr, storeErr} {
		if err != nil {
			engine.closeErr = err
			return err
		}
	}
	engine.closeErr = nil
	return nil
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
	byID := make(map[string]KeyPolicy, len(policies))
	byHash := make(map[string]string, len(policies))
	for _, rawPolicy := range policies {
		policy, err := normalizePolicy(rawPolicy, cfg.Groups, cfg.RequestUnits)
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
	merged := make([]KeyPolicy, 0, len(persisted)+len(bootstrap))
	knownIDs := make(map[string]struct{}, len(persisted))
	for _, policy := range persisted {
		merged = append(merged, policy)
		knownIDs[policy.ID] = struct{}{}
	}
	for _, policy := range bootstrap {
		if _, exists := knownIDs[policy.ID]; exists {
			continue
		}
		merged = append(merged, policy)
		knownIDs[policy.ID] = struct{}{}
	}
	return merged
}

// Reconfigure applies host-level settings without deleting policies or usage.
// The database path and fingerprint secret are immutable after startup because
// changing either would disconnect existing durable policy fingerprints.
func (engine *Engine) Reconfigure(cfg RuntimeConfig) error {
	if engine == nil {
		return fmt.Errorf("quota engine is not initialized")
	}
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	engine.configMu.RLock()
	oldConfig := engine.config
	engine.configMu.RUnlock()
	if cfg.DatabasePath != oldConfig.DatabasePath || cfg.KeyHMACSecret != oldConfig.KeyHMACSecret {
		return fmt.Errorf("database_path and key_hmac_secret require a plugin restart and key migration")
	}
	// request_units is only the fallback Token estimate for callbacks without
	// usage fields. Fixed-point x ledgers are independent of it, so completed
	// weekly usage must not lock this setting; only in-flight requests still
	// need the value they were admitted under to remain stable.
	if cfg.RequestUnits != oldConfig.RequestUnits && engine.pendingSettlements.Load() > 0 {
		return fmt.Errorf("request_units cannot change until admitted requests settle")
	}
	persisted, err := engine.store.LoadPolicies()
	if err != nil {
		return err
	}
	// Validate the complete post-reload policy set before writing bootstrap
	// rows. This prevents a rejected config reload from leaving new SQLite
	// policies behind that the previous in-memory configuration cannot use.
	policies := mergeBootstrapPolicies(persisted, cfg.BootstrapKeys)
	byID, byHash, err := buildPolicyMaps(cfg, policies)
	if err != nil {
		return err
	}
	engine.poolMu.RLock()
	pool := make(map[string]AccountPoolEntry, len(engine.accountPool))
	for authID, entry := range engine.accountPool {
		pool[authID] = entry
	}
	engine.poolMu.RUnlock()
	if err := validatePolicySetAgainstPool(byID, pool, engine.activeAllocationLedgerKeys(time.Now().UTC())); err != nil {
		return err
	}
	if err := engine.store.InsertMissingPolicies(cfg.BootstrapKeys); err != nil {
		return err
	}
	engine.configMu.Lock()
	engine.config = cfg
	engine.configMu.Unlock()
	engine.policiesMu.Lock()
	engine.policiesByID = byID
	engine.policiesByHash = byHash
	engine.policiesMu.Unlock()
	for keyID := range byID {
		engine.keyState(keyID)
	}
	return nil
}

// Admit persists a one-Token correlation marker before CPA's built-in
// scheduler runs. The marker is replaced by the completed UsageRecord's actual
// token total in RecordUsage. This prevents a scheduler retry from being
// mistaken for 200K customer Tokens while still keeping a durable callback
// correlation and a minimal concurrent-limit guard.
func (engine *Engine) Admit(rawAPIKey, model string, now time.Time, candidateSets ...[]SchedulerCandidate) Admission {
	return engine.admit(rawAPIKey, model, "", now, candidateSets...)
}

// AdmitCaptured binds the user-authored request excerpt captured by CPA's
// before-auth interceptor to the final admission outcome. The capture ID is
// process-local and is removed before the request is dispatched upstream.
func (engine *Engine) AdmitCaptured(rawAPIKey, model, captureID string, now time.Time, candidateSets ...[]SchedulerCandidate) Admission {
	return engine.admit(rawAPIKey, model, captureID, now, candidateSets...)
}

func (engine *Engine) admit(rawAPIKey, model, captureID string, now time.Time, candidateSets ...[]SchedulerCandidate) Admission {
	if engine == nil {
		return deny("quota_unavailable", "codex-carpool is not initialized")
	}
	// This read lock is the policy-change boundary: a completed disable or
	// allocation update cannot be followed by an admission using its old policy.
	engine.adminMu.RLock()
	defer engine.adminMu.RUnlock()
	if engine.admissionsClosed.Load() {
		return deny("quota_unavailable", "codex-carpool is shutting down")
	}
	engine.configMu.RLock()
	cfg := engine.config
	engine.configMu.RUnlock()
	keyHash := FingerprintAPIKey(rawAPIKey, cfg.KeyHMACSecret)
	if keyHash == "" {
		return bypass()
	}
	engine.policiesMu.RLock()
	keyID, found := engine.policiesByHash[keyHash]
	policy := engine.policiesByID[keyID]
	engine.policiesMu.RUnlock()
	if !found || !policy.Enabled {
		return bypass()
	}
	requestContent := engine.claimCapturedRequestContent(captureID, keyID, now)
	if !policy.AllowsAt(now) {
		result := deny("access_schedule_closed", "This API key is outside its configured access schedule")
		result.KeyID = keyID
		engine.enqueueDecision(DecisionLog{KeyID: keyID, Model: model, RequestContent: requestContent, RequestedAt: now.UTC(), Decision: "blocked", StatusCode: httpStatusForbidden, Reason: result.Code})
		return result
	}
	if !modelAllowed(policy, model) {
		result := deny("model_not_allowed", "This API key is not allowed to use the requested model")
		result.KeyID = keyID
		engine.enqueueDecision(DecisionLog{KeyID: keyID, Model: model, RequestContent: requestContent, RequestedAt: now.UTC(), Decision: "blocked", StatusCode: httpStatusForbidden, Reason: "model_not_allowed"})
		return result
	}
	if engine.accountSourceConflict.Load() {
		result := deny("quota_account_source_conflict", "codex-carpool is verifying shared-pool account sources or found duplicate/unprovable Codex identities")
		result.KeyID = keyID
		engine.enqueueDecision(DecisionLog{KeyID: keyID, Model: model, RequestContent: requestContent, RequestedAt: now.UTC(), Decision: "blocked", StatusCode: httpStatusServiceUnavailable, Reason: result.Code})
		return result
	}
	// Storage health applies only to registered Keys. Unmanaged Keys must keep
	// returning control to CLIProxyAPI even while this plugin is recovering.
	if engine.persistenceDegraded.Load() || engine.allocationDegraded.Load() {
		result := deny("quota_persistence_unavailable", "codex-carpool accounting storage is temporarily unavailable")
		result.KeyID = keyID
		// A persistence failure is not a Key or account exhaustion. Keep the
		// persisted audit decision aligned with the bridge's 503 response so
		// operators and clients do not mistake recovery work for quota depletion.
		engine.enqueueDecision(DecisionLog{KeyID: keyID, Model: model, RequestContent: requestContent, RequestedAt: now.UTC(), Decision: "blocked", StatusCode: httpStatusServiceUnavailable, Reason: result.Code})
		return result
	}

	var candidates []SchedulerCandidate
	if len(candidateSets) > 0 {
		candidates = candidateSets[0]
	}
	if candidates == nil {
		// A managed Key cannot be evaluated safely without CPA's candidate list:
		// selecting an arbitrary account would desynchronize official windows,
		// while the removed legacy rolling path could bypass durable reservations.
		result := deny("quota_scheduler_candidates_required", "codex-carpool requires CPA scheduler candidates for managed API keys")
		result.KeyID = keyID
		engine.enqueueDecision(DecisionLog{KeyID: keyID, Model: model, RequestContent: requestContent, RequestedAt: now.UTC(), Decision: "blocked", StatusCode: httpStatusServiceUnavailable, Reason: result.Code})
		return result
	}
	authIDs, reason := engine.selectPoolAccounts(candidates, cfg.RequestUnits, now)
	if len(authIDs) == 0 {
		message := "No configured Codex account has a current official quota snapshot"
		status := httpStatusServiceUnavailable
		switch reason {
		case "quota_pool_unconfigured":
			message = "No Codex account is configured in the shared pool"
		case "quota_snapshot_unavailable":
			message = "No configured Codex account has a current official quota snapshot"
		case "quota_candidate_mismatch":
			message = "CPA's current scheduler candidates do not match the configured Codex shared-pool accounts"
		case "quota_pool_exhausted":
			message = "All configured Codex accounts have exhausted their official quota"
			status = httpStatusTooManyRequests
		case "quota_account_unavailable":
			message = "No configured Codex account is available in CPA's scheduler"
		}
		result := deny(reason, message)
		result.KeyID = keyID
		engine.enqueueDecision(DecisionLog{KeyID: keyID, Model: model, RequestContent: requestContent, RequestedAt: now.UTC(), Decision: "blocked", StatusCode: status, Reason: reason})
		return result
	}

	// A Key allocation is split across the pool's official weekly windows.
	// Try every locally eligible CPA account: another request can fill the
	// first choice after selectPoolAccounts releases allocationMu, but it must
	// not turn an available second account into an unnecessary 429.
	var lastAuthID string
	allocationExhausted := false
	accountExhausted := false
	resetUnavailable := false
	for _, authID := range authIDs {
		lastAuthID = authID
		reservation, err := engine.reserveAccountAllocation(keyID, authID, policy, cfg.RequestUnits, admissionReservationUnits, now)
		if err == nil {
			engine.beginPendingSettlement(reservation)
			engine.rememberPendingRequestContent(reservation, model, requestContent, now)
			return Admission{Allowed: true, KeyID: keyID, AuthID: authID}
		}
		switch err {
		case errAllocationExhausted:
			allocationExhausted = true
			continue
		case errOfficialAccountExhausted:
			accountExhausted = true
			continue
		case errOfficialWeeklyReset:
			resetUnavailable = true
			continue
		default:
			result := deny("quota_persistence_unavailable", "codex-carpool accounting storage is temporarily unavailable")
			result.KeyID = keyID
			engine.enqueueDecision(DecisionLog{KeyID: keyID, AuthID: authID, Model: model, RequestContent: requestContent, RequestedAt: now.UTC(), Decision: "blocked", StatusCode: httpStatusServiceUnavailable, Reason: result.Code})
			return result
		}
	}
	code, message := "quota_pool_exhausted", "All selected Codex accounts have no remaining official shared-pool allowance"
	status := httpStatusTooManyRequests
	if allocationExhausted {
		code = "quota_allocation_exhausted"
		message = "This API key has reached its allocated shared-pool allowance across the available Codex accounts"
	} else if resetUnavailable && !accountExhausted && !allocationExhausted {
		code = "quota_snapshot_unavailable"
		message = "The selected Codex accounts do not expose current official quota reset times"
		status = httpStatusServiceUnavailable
	}
	result := deny(code, message)
	result.KeyID = keyID
	engine.enqueueDecision(DecisionLog{KeyID: keyID, AuthID: lastAuthID, Model: model, RequestContent: requestContent, RequestedAt: now.UTC(), Decision: "blocked", StatusCode: status, Reason: result.Code})
	return result
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
	for _, allowed := range policy.AllowedModels {
		if allowed == model {
			return true
		}
	}
	return false
}

func multiplierCapacity(multiplier float64, requestUnits, baseRequests int64) int64 {
	if multiplier <= 0 || requestUnits <= 0 || baseRequests <= 0 {
		return 0
	}
	value := multiplier * float64(requestUnits) * float64(baseRequests)
	if value >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(math.Floor(value))
}

// CompletedUsage is the safe, dependency-free subset of CLIProxyAPI's
// completed UsageRecord required by the quota engine.
type CompletedUsage struct {
	APIKey          string
	AuthID          string
	Model           string
	RequestedAt     time.Time
	Generate        bool
	Failed          bool
	FailureStatus   int
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	TotalTokens     int64
}

// RecordUsage finalizes a previously reserved request using the host's actual
// completed token counters. The host includes the downstream API Key in each
// UsageRecord, allowing one Key to aggregate usage across any CPA-selected
// Codex account without persisting that Key in plaintext.
func (engine *Engine) RecordUsage(record CompletedUsage) {
	if engine == nil || engine.usageClosed.Load() {
		return
	}
	engine.adminMu.RLock()
	defer engine.adminMu.RUnlock()
	// Close first closes admissions, then waits for this callback class to
	// settle. Recheck after the read lock is held so the final worker shutdown
	// cannot race a terminal usage record that began just before usageClosed.
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
	requestedAt := record.RequestedAt.UTC()
	if requestedAt.IsZero() {
		requestedAt = time.Now().UTC()
	}
	bucketAt := usageBucketEnd(requestedAt)
	units := completedUsageUnits(record, cfg.RequestUnits)
	authID := strings.TrimSpace(record.AuthID)
	if !found {
		// Unmanaged CPA traffic still consumes the selected official account.
		// Record only its redacted account aggregate so a later official
		// percentage delta is not incorrectly charged to managed Keys.
		if units <= 0 || authID == "" || !engine.claimUsageCallback("unmanaged:"+fingerprint, authID, requestedAt, record, cfg.KeyHMACSecret) {
			return
		}
		attributionAt := time.Now().UTC()
		if requestedAt.After(attributionAt) {
			attributionAt = requestedAt
		}
		if !engine.enqueueBuckets(UsageEvent{
			Scope: "account", AuthID: authID, Model: strings.TrimSpace(record.Model),
			RequestedAt: requestedAt, RecordedAt: usageBucketEnd(attributionAt),
			Units: units, RequestCount: 1, MeteredBy: "completion_token_usage_unmanaged",
		}) {
			engine.persistenceDegraded.Store(true)
		}
		return
	}
	// CPA v7.2.97 does not expose a request ID to native usage plugins. This
	// bounded HMAC fingerprint suppresses repeated identical callbacks without
	// using an API Key or request text as correlation material. It is
	// intentionally a mitigation, not a substitute for a future CPA-provided
	// request identifier.
	if !engine.claimUsageCallback(keyID, authID, requestedAt, record, cfg.KeyHMACSecret) {
		return
	}
	// Charge a calibrated provisional x value at callback time so a busy Key
	// cannot run unmetered between five-minute official percentage polls. The
	// next successful official poll replaces this estimate with the Key's
	// full-window Token-derived charge; it is never counted twice.
	attributionAt := time.Now().UTC()
	if requestedAt.After(attributionAt) {
		attributionAt = requestedAt
	}
	provisionalUnits := engine.provisionalXUnits(authID, units, cfg.RequestUnits)
	provisionalLimit := engine.provisionalXLimit(authID)
	settlement := engine.settleAccountAllocation(keyID, authID, bucketAt, attributionAt, admissionReservationUnits, provisionalUnits, provisionalLimit)
	engine.finishPendingSettlement(settlement.Key, settlement.Matched)
	if !settlement.Matched {
		// Never turn an unmatched late/duplicate callback into customer usage.
		// Its lightweight durable marker remains until the official reset.
		reason := "unmatched_usage_callback"
		if settlement.Ambiguous {
			reason = "ambiguous_usage_callback"
		}
		engine.enqueueDecision(DecisionLog{
			KeyID: keyID, AuthID: authID, Model: strings.TrimSpace(record.Model), RequestedAt: requestedAt,
			Decision: "ignored", Reason: reason,
		})
		return
	}
	requestContent := engine.takePendingRequestContent(settlement.Key, strings.TrimSpace(record.Model), requestedAt)
	state := engine.keyState(keyID)
	state.mu.Lock()
	state.completed.prune(requestedAt)
	state.reservations.prune(requestedAt)
	state.reservations.removeEvent(settlement.BucketAt, admissionReservationUnits)
	if units > 0 {
		state.completed.addEvent(settlement.BucketAt, units)
	}
	state.mu.Unlock()
	if units == 0 {
		statusCode := record.FailureStatus
		if statusCode < 0 {
			statusCode = 0
		}
		reason := "request_not_completed"
		if record.Failed {
			reason = "upstream_failed"
		}
		if !policy.Enabled || policy.NeedsRebind {
			reason += "_after_policy_disabled"
		}
		// A reservation is internal accounting state, not a useful audit event.
		// Keep only terminal outcomes (completion, failure, or an admission block)
		// so a busy Key writes one meaningful log line per request rather than two.
		engine.enqueueDecision(DecisionLog{
			KeyID:          keyID,
			AuthID:         authID,
			Model:          strings.TrimSpace(record.Model),
			RequestContent: requestContent,
			RequestedAt:    requestedAt,
			Decision:       "failed",
			StatusCode:     statusCode,
			Reason:         reason,
		})
		return
	}
	event := UsageEvent{
		// Completed-token aggregates are separate from legacy admission
		// estimates, so historical approximations never contaminate live caps.
		Scope:           "key_actual",
		KeyID:           keyID,
		AuthID:          authID,
		Model:           strings.TrimSpace(record.Model),
		RequestedAt:     requestedAt,
		RecordedAt:      settlement.BucketAt,
		InputTokens:     record.InputTokens,
		OutputTokens:    record.OutputTokens,
		ReasoningTokens: record.ReasoningTokens,
		Units:           units,
		RequestCount:    1,
		MeteredBy:       "completion_token_usage",
		Failed:          record.Failed,
		FailureStatus:   record.FailureStatus,
	}
	// Keep one compact terminal audit line alongside the minute-level usage
	// aggregate. Admission reservations are intentionally not logged so the
	// queue remains bounded at one meaningful line per completed request.
	decision, reason, statusCode := "completed", "actual_token_usage", httpStatusOK
	if record.Failed {
		decision, reason = "failed", "upstream_failed_with_actual_usage"
		statusCode = record.FailureStatus
		if statusCode < 0 {
			statusCode = 0
		}
	}
	if !policy.Enabled || policy.NeedsRebind {
		if decision == "completed" {
			decision, reason = "completed", "actual_usage_after_policy_disabled"
		} else {
			reason += "_after_policy_disabled"
		}
	}
	engine.enqueueDecision(DecisionLog{
		KeyID:          keyID,
		AuthID:         event.AuthID,
		Model:          event.Model,
		RequestContent: requestContent,
		RequestedAt:    requestedAt,
		Decision:       decision,
		StatusCode:     statusCode,
		Reason:         reason,
		Units:          units,
	})
	accountEvent := event
	accountEvent.Scope = "account"
	accountEvent.KeyID = ""
	keyAccountEvent := event
	keyAccountEvent.Scope = "key_account_actual"
	keyAccountEvent.GroupID = keyID
	// Attribute by completion time so a late terminal callback cannot fall
	// behind an already persisted official observation watermark.
	keyAccountEvent.RecordedAt = usageBucketEnd(attributionAt)
	accountEvent.RecordedAt = keyAccountEvent.RecordedAt
	// Account and Key/account buckets are separate from Key analytics. Account
	// totals calibrate Token/x against the official percentage; Key/account
	// totals ensure only this managed Key's own traffic consumes its allocation.
	if !engine.enqueueBuckets(event) || !engine.enqueueBuckets(keyAccountEvent) || !engine.enqueueBuckets(accountEvent) {
		engine.persistenceDegraded.Store(true)
	}
}

func (engine *Engine) claimUsageCallback(keyID, authID string, requestedAt time.Time, record CompletedUsage, secret string) bool {
	if engine == nil || secret == "" {
		return true
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%s\x00%s\x00%s\x00%d\x00%t\x00%t\x00%d\x00%d\x00%d\x00%d\x00%d", keyID, authID, strings.TrimSpace(record.Model), requestedAt.UTC().UnixNano(), record.Generate, record.Failed, record.FailureStatus, record.InputTokens, record.OutputTokens, record.ReasoningTokens, record.TotalTokens)
	callbackID := hex.EncodeToString(mac.Sum(nil))
	now := time.Now().UTC()
	cutoff := now.Add(-usageCallbackDedupeTTL)
	engine.usageDedupeMu.Lock()
	defer engine.usageDedupeMu.Unlock()
	if previous, exists := engine.recentUsageCallbacks[callbackID]; exists && previous.After(cutoff) {
		return false
	}
	if len(engine.recentUsageCallbacks) >= maxRecentUsageCallbacks {
		for id, seenAt := range engine.recentUsageCallbacks {
			if !seenAt.After(cutoff) {
				delete(engine.recentUsageCallbacks, id)
			}
		}
	}
	// Under an exceptional callback storm, prefer bounded memory over claiming
	// a new fingerprint that cannot be retained for duplicate protection.
	if len(engine.recentUsageCallbacks) < maxRecentUsageCallbacks {
		engine.recentUsageCallbacks[callbackID] = now
	}
	return true
}

func completedUsageUnits(record CompletedUsage, fallback int64) int64 {
	if !record.Generate {
		return 0
	}
	if record.TotalTokens > 0 {
		return record.TotalTokens
	}
	var units int64
	for _, value := range []int64{record.InputTokens, record.OutputTokens, record.ReasoningTokens} {
		if value <= 0 || units > math.MaxInt64-value {
			continue
		}
		units += value
	}
	if units > 0 {
		return units
	}
	if record.Failed {
		return 0
	}
	// CLIProxyAPI emits a record even when an upstream response omits usage.
	// Retaining the admission reservation is safer than creating a zero-cost
	// bypass for those otherwise successful requests.
	return fallback
}

type pendingBucketKey struct {
	Scope    string
	ScopeID  string
	BucketAt int64
}

func usageBucketEnd(now time.Time) time.Time {
	now = now.UTC()
	return now.Truncate(usageBucketWindow).Add(usageBucketWindow)
}

func bucketKey(event UsageEvent) pendingBucketKey {
	// Keep Key/account attribution isolated by both identities. A Key can be
	// switched between CPA accounts inside one minute; merging those rows would
	// make the next official percentage delta impossible to assign correctly.
	return pendingBucketKey{
		Scope: event.Scope, ScopeID: usageBucketScopeID(event),
		BucketAt: event.RecordedAt.UTC().UnixMilli(),
	}
}

func mergeBucketEvent(existing, incoming UsageEvent) UsageEvent {
	existing.Units += incoming.Units
	existing.RequestCount += incoming.RequestCount
	if existing.AuthID != incoming.AuthID {
		existing.AuthID = "mixed"
	}
	return existing
}

func pendingShardIndex(key pendingBucketKey) int {
	// FNV-1a is small and deterministic. Key and account scopes intentionally
	// hash independently, so a busy shared Key does not serialize every other
	// Key's admission accounting.
	hash := uint32(2_166_136_261)
	for _, value := range [2]string{key.Scope, key.ScopeID} {
		for index := 0; index < len(value); index++ {
			hash ^= uint32(value[index])
			hash *= 16_777_619
		}
	}
	return int(hash % pendingShardCount)
}

func (engine *Engine) enqueueBuckets(event UsageEvent) bool {
	key := bucketKey(event)
	index := pendingShardIndex(key)
	shard := &engine.pendingShards[index]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	now := time.Now().UTC()
	if (!shard.pendingSince.IsZero() && now.Sub(shard.pendingSince) > maxPendingAge) ||
		(!shard.inFlightSince.IsZero() && now.Sub(shard.inFlightSince) > maxPendingAge) {
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
	if engine.reserveDecisionLogSlots(1) != 1 {
		engine.droppedDecisionLogs.Add(1)
		return
	}
	index := pendingShardIndex(pendingBucketKey{Scope: "log", ScopeID: entry.KeyID})
	shard := &engine.pendingShards[index]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	shard.logs = append(shard.logs, entry)
	if shard.pendingSince.IsZero() {
		shard.pendingSince = time.Now().UTC()
	}
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

// flushPending batches high-frequency admissions into one SQLite transaction.
// A clean shutdown waits for this flush; an unexpected crash can lose at most
// one short in-memory batch, while live admission remains strictly in memory.
func (engine *Engine) flushPending() error {
	if engine == nil {
		return nil
	}
	engine.flushMu.Lock()
	defer engine.flushMu.Unlock()
	type pendingBatch struct {
		shardIndex   int
		events       map[pendingBucketKey]UsageEvent
		logs         []DecisionLog
		pendingSince time.Time
	}
	batches := make([]pendingBatch, 0, pendingShardCount)
	batch := make([]UsageEvent, 0)
	decisionLogs := make([]DecisionLog, 0)
	for index := range engine.pendingShards {
		shard := &engine.pendingShards[index]
		shard.mu.Lock()
		if len(shard.buckets) == 0 && len(shard.logs) == 0 {
			shard.mu.Unlock()
			continue
		}
		pending := shard.buckets
		logs := shard.logs
		pendingSince := shard.pendingSince
		shard.buckets = make(map[pendingBucketKey]UsageEvent)
		shard.logs = make([]DecisionLog, 0, 64)
		shard.pendingSince = time.Time{}
		shard.inFlightSince = pendingSince
		shard.mu.Unlock()
		engine.pendingBucketCount.Add(-int64(len(pending)))
		engine.pendingLogCount.Add(-int64(len(logs)))
		batches = append(batches, pendingBatch{shardIndex: index, events: pending, logs: logs, pendingSince: pendingSince})
		for _, event := range pending {
			batch = append(batch, event)
		}
		decisionLogs = append(decisionLogs, logs...)
	}
	if len(batch) == 0 && len(decisionLogs) == 0 {
		return nil
	}
	if err := engine.store.FlushUsageAndLogs(batch, decisionLogs); err != nil {
		engine.persistenceFailures.Add(1)
		engine.persistenceDegraded.Store(true)
		for _, pending := range batches {
			shard := &engine.pendingShards[pending.shardIndex]
			shard.mu.Lock()
			shard.inFlightSince = time.Time{}
			// Producers may have appended new logs while this batch was in
			// flight. Reinsert only the bounded portion of the failed batch so
			// repeated SQLite failures cannot grow memory without limit.
			keep := engine.reserveDecisionLogSlots(len(pending.logs))
			if dropped := len(pending.logs) - keep; dropped > 0 {
				engine.droppedDecisionLogs.Add(uint64(dropped))
			}
			if keep > 0 {
				shard.logs = append(pending.logs[:keep], shard.logs...)
			}
			additions := int64(0)
			for key, event := range pending.events {
				if existing, found := shard.buckets[key]; found {
					shard.buckets[key] = mergeBucketEvent(existing, event)
				} else {
					shard.buckets[key] = event
					additions++
				}
			}
			if shard.pendingSince.IsZero() || (!pending.pendingSince.IsZero() && pending.pendingSince.Before(shard.pendingSince)) {
				shard.pendingSince = pending.pendingSince
			}
			shard.mu.Unlock()
			if additions > 0 {
				engine.pendingBucketCount.Add(additions)
			}
		}
		return err
	}
	for _, pending := range batches {
		shard := &engine.pendingShards[pending.shardIndex]
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
	// SQLite retention is intentionally longer than an active Codex window for
	// auditability, but the scheduler only needs current windows plus unresolved
	// reservations. Trim finished in-memory windows now so official refreshes do
	// not scan a process lifetime of minute buckets.
	engine.pruneAllocationState(now)
	if err := engine.store.DeleteUsageEventsBefore(now.Add(-retention)); err != nil {
		engine.retentionFailures.Add(1)
	}
}

func (engine *Engine) pruneAllocationState(now time.Time) {
	if engine == nil {
		return
	}
	cutoff := now.UTC().UnixMilli()
	engine.allocationMu.Lock()
	defer engine.allocationMu.Unlock()
	for key, bucket := range engine.allocationBuckets {
		if key.WindowResetAt > cutoff || bucket.Reserved > 0 {
			continue
		}
		engine.deleteAllocationBucketLocked(key, bucket)
	}
	for key, state := range engine.officialAccountWindows {
		if key.WindowResetAt <= cutoff && state.Reserved == 0 {
			delete(engine.officialAccountWindows, key)
		}
	}
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

// exceedsLimit avoids an int64 overflow when a deliberately large but valid
// configured allowance receives its final request-unit charge.
func exceedsLimit(used, charge, capacity int64) bool {
	return charge > capacity || used > capacity-charge
}

func deny(code, message string) Admission {
	return Admission{Code: code, Message: message}
}

func bypass() Admission {
	return Admission{Bypass: true}
}

// FingerprintAPIKey returns a stable, non-reversible HMAC fingerprint. The
// installation secret prevents offline guessing from a copied SQLite database.
func FingerprintAPIKey(raw, secret string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}
