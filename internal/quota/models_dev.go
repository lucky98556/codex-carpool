package quota

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	modelsDevAPIURL          = "https://models.dev/api.json"
	modelRateSyncInterval    = 24 * time.Hour
	modelRateSyncRetry       = time.Hour
	modelRateSyncHTTPTimeout = 12 * time.Second
	modelsDevMaxBodyBytes    = 32 << 20
	modelRateSyncMetadata    = "model_rate_sync_v1"
)

type modelRateHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// ModelRateSyncStatus is persisted independently from the rate card. A failed
// refresh never clears the last successful prices.
type ModelRateSyncStatus struct {
	Enabled            bool       `json:"enabled"`
	LastAttempt        *time.Time `json:"last_attempt,omitempty"`
	LastSuccess        *time.Time `json:"last_success,omitempty"`
	NextSyncAt         *time.Time `json:"next_sync_at,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
	MatchedModels      int        `json:"matched_models"`
	UnmatchedModels    int        `json:"unmatched_models"`
	RetiredModels      int        `json:"retired_models"`
	ETag               string     `json:"etag,omitempty"`
	CatalogFingerprint string     `json:"catalog_fingerprint,omitempty"`
}

type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID           string                `json:"id"`
	Cost         modelsDevCost         `json:"cost"`
	Experimental modelsDevExperimental `json:"experimental"`
}

type modelsDevCost struct {
	Input           *float64        `json:"input"`
	Output          *float64        `json:"output"`
	Reasoning       *float64        `json:"reasoning"`
	CacheRead       *float64        `json:"cache_read"`
	CacheWrite      *float64        `json:"cache_write"`
	ContextOver200K *modelsDevCost  `json:"context_over_200k"`
	Tiers           []modelsDevTier `json:"tiers"`
}

type modelsDevTier struct {
	Tier struct {
		Size int64 `json:"size"`
	} `json:"tier"`
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	Reasoning  *float64 `json:"reasoning"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

type modelsDevExperimental struct {
	Modes map[string]modelsDevMode `json:"modes"`
}

type modelsDevMode struct {
	Cost     modelsDevCost `json:"cost"`
	Provider struct {
		Body map[string]any `json:"body"`
	} `json:"provider"`
}

func (store *Store) LoadModelRateSyncStatus() (ModelRateSyncStatus, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var raw string
	err := store.db.QueryRow(`SELECT value FROM plugin_metadata WHERE name=?`, modelRateSyncMetadata).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ModelRateSyncStatus{}, nil
		}
		return ModelRateSyncStatus{}, err
	}
	var status ModelRateSyncStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		return ModelRateSyncStatus{}, fmt.Errorf("decode model rate sync settings: %w", err)
	}
	return status, nil
}

func (store *Store) SaveModelRateSyncStatus(status ModelRateSyncStatus) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	raw, err := json.Marshal(status)
	if err != nil {
		return err
	}
	_, err = store.db.Exec(`INSERT INTO plugin_metadata(name,value,updated_at) VALUES(?,?,?)
ON CONFLICT(name) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, modelRateSyncMetadata, string(raw), time.Now().UTC().UnixMilli())
	return err
}

func (engine *Engine) ModelRateSyncStatus() ModelRateSyncStatus {
	if engine == nil {
		return ModelRateSyncStatus{}
	}
	engine.rateSyncMu.RLock()
	status := engine.rateSyncStatus
	engine.rateSyncMu.RUnlock()
	status.NextSyncAt = nextModelRateSync(status, engine.rateSyncNow())
	return status
}

func (engine *Engine) SetModelRateSyncEnabled(enabled bool) (ModelRateSyncStatus, error) {
	if engine == nil {
		return ModelRateSyncStatus{}, fmt.Errorf("codex-carpool is not initialized")
	}
	engine.rateSyncRunMu.Lock()
	engine.rateSyncMu.RLock()
	status := engine.rateSyncStatus
	wasEnabled := status.Enabled
	engine.rateSyncMu.RUnlock()
	status.Enabled = enabled
	if enabled && !wasEnabled {
		// Force a complete response when synchronization is re-enabled. A 304
		// cannot rebuild rates that may have been edited while sync was off.
		status.ETag = ""
		status.CatalogFingerprint = ""
	}
	if err := engine.store.SaveModelRateSyncStatus(status); err != nil {
		engine.rateSyncRunMu.Unlock()
		return engine.ModelRateSyncStatus(), err
	}
	engine.rateSyncMu.Lock()
	engine.rateSyncStatus = status
	engine.rateSyncMu.Unlock()
	engine.rateSyncRunMu.Unlock()
	if enabled && engine.ModelRateSyncStatus().Enabled {
		_ = engine.SyncModelRates(context.Background())
	}
	engine.wakeModelRateSync()
	return engine.ModelRateSyncStatus(), nil
}

func (engine *Engine) SyncModelRates(ctx context.Context) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	engine.rateSyncRunMu.Lock()
	defer engine.rateSyncRunMu.Unlock()
	now := engine.rateSyncNow().UTC()
	engine.rateSyncMu.RLock()
	status := engine.rateSyncStatus
	engine.rateSyncMu.RUnlock()
	status.LastAttempt = timePointer(now)
	catalog, err := engine.Models()
	if err != nil {
		return engine.finishModelRateSync(status, fmt.Errorf("load CPA model catalog: %w", err))
	}
	catalogFingerprint := modelCatalogFingerprint(catalog)

	requestCtx, cancel := context.WithTimeout(ctx, modelRateSyncHTTPTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, engine.rateSyncURL, nil)
	if err == nil && status.ETag != "" && status.CatalogFingerprint == catalogFingerprint {
		request.Header.Set("If-None-Match", status.ETag)
	}
	if err != nil {
		return engine.finishModelRateSync(status, err)
	}
	response, err := engine.rateSyncClient.Do(request)
	if err != nil {
		return engine.finishModelRateSync(status, fmt.Errorf("request models.dev: %w", err))
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		status.RetiredModels = 0
		status.LastSuccess, status.LastError, status.CatalogFingerprint = timePointer(now), "", catalogFingerprint
		return engine.finishModelRateSync(status, nil)
	}
	if response.StatusCode != http.StatusOK {
		return engine.finishModelRateSync(status, fmt.Errorf("models.dev returned HTTP %d", response.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, modelsDevMaxBodyBytes+1))
	if err != nil {
		return engine.finishModelRateSync(status, fmt.Errorf("read models.dev response: %w", err))
	}
	if len(raw) > modelsDevMaxBodyBytes {
		return engine.finishModelRateSync(status, fmt.Errorf("models.dev response exceeds %d bytes", modelsDevMaxBodyBytes))
	}
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(raw, &providers); err != nil {
		return engine.finishModelRateSync(status, fmt.Errorf("decode models.dev response: %w", err))
	}
	if !modelsDevCatalogHasModels(providers) {
		return engine.finishModelRateSync(status, fmt.Errorf("models.dev returned an empty model catalog"))
	}
	rates, unmatched := modelRatesFromModelsDev(catalog, providers, now)
	successStatus := status
	successStatus.ETag = strings.TrimSpace(response.Header.Get("ETag"))
	successStatus.CatalogFingerprint = catalogFingerprint
	successStatus.LastSuccess, successStatus.LastError = timePointer(now), ""
	successStatus.MatchedModels, successStatus.UnmatchedModels = len(rates), unmatched
	engine.rateSyncMu.RLock()
	successStatus.Enabled = engine.rateSyncStatus.Enabled
	engine.rateSyncMu.RUnlock()
	retired, successStatus, err := engine.store.ReconcileSynchronizedModelRates("models.dev", rates, successStatus)
	if err != nil {
		return engine.finishModelRateSync(status, fmt.Errorf("save synchronized model rates: %w", err))
	}
	engine.ratesMu.Lock()
	for _, model := range retired {
		delete(engine.modelRates, model)
	}
	for _, rate := range rates {
		engine.modelRates[rate.Model] = rate
	}
	engine.ratesMu.Unlock()
	engine.rateSyncMu.Lock()
	engine.rateSyncStatus = successStatus
	engine.rateSyncMu.Unlock()
	engine.LogOperational("info", "model_rate_sync_completed", fmt.Sprintf("models.dev 价格同步完成：匹配 %d 个，未匹配 %d 个，移除过期费率 %d 个", successStatus.MatchedModels, successStatus.UnmatchedModels, successStatus.RetiredModels), "", "")
	return nil
}

func modelsDevCatalogHasModels(providers map[string]modelsDevProvider) bool {
	for _, provider := range providers {
		if len(provider.Models) > 0 {
			return true
		}
	}
	return false
}

func modelCatalogFingerprint(catalog []ModelCatalogEntry) string {
	entries := make([]string, 0, len(catalog))
	for _, item := range catalog {
		if item.Available {
			entries = append(entries, strings.TrimSpace(item.ID)+"\x00"+normalizedProviderID(item.Owner))
		}
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (engine *Engine) finishModelRateSync(status ModelRateSyncStatus, syncErr error) error {
	if syncErr != nil {
		status.LastError = syncErr.Error()
	}
	engine.rateSyncMu.RLock()
	status.Enabled = engine.rateSyncStatus.Enabled
	engine.rateSyncMu.RUnlock()
	if err := engine.store.SaveModelRateSyncStatus(status); err != nil {
		return err
	}
	engine.rateSyncMu.Lock()
	engine.rateSyncStatus = status
	engine.rateSyncMu.Unlock()
	if syncErr != nil {
		engine.LogOperational("warn", "model_rate_sync_failed", "models.dev 价格同步失败，已保留原费率："+syncErr.Error(), "", "")
	} else {
		engine.LogOperational("info", "model_rate_sync_completed", fmt.Sprintf("models.dev 价格同步完成：匹配 %d 个，未匹配 %d 个，移除过期费率 %d 个", status.MatchedModels, status.UnmatchedModels, status.RetiredModels), "", "")
	}
	return syncErr
}

func modelRatesFromModelsDev(catalog []ModelCatalogEntry, providers map[string]modelsDevProvider, now time.Time) ([]ModelRate, int) {
	rates := make([]ModelRate, 0, len(catalog))
	unmatched := 0
	for _, item := range catalog {
		if !item.Available {
			continue
		}
		providerID, model, found := matchModelsDevModel(item, providers)
		if !found || model.Cost.Input == nil || model.Cost.Output == nil {
			unmatched++
			continue
		}
		rate := modelRateFromModelsDev(item.ID, providerID, model, now)
		normalized, err := normalizeModelRate(rate)
		if err != nil {
			unmatched++
			continue
		}
		rates = append(rates, normalized)
	}
	sort.Slice(rates, func(left, right int) bool { return rates[left].Model < rates[right].Model })
	return rates, unmatched
}

func matchModelsDevModel(item ModelCatalogEntry, providers map[string]modelsDevProvider) (string, modelsDevModel, bool) {
	modelID := strings.TrimSpace(item.ID)
	owner := normalizedProviderID(item.Owner)
	for providerID, provider := range providers {
		if !providerMatchesOwner(owner, providerID, provider.ID, provider.Name) {
			continue
		}
		if model, found := providerModel(provider, modelID); found {
			return providerID, model, true
		}
	}
	var matchedProvider string
	var matched modelsDevModel
	for providerID, provider := range providers {
		if model, found := providerModel(provider, modelID); found {
			if matchedProvider != "" {
				return "", modelsDevModel{}, false
			}
			matchedProvider, matched = providerID, model
		}
	}
	return matchedProvider, matched, matchedProvider != ""
}

func providerModel(provider modelsDevProvider, modelID string) (modelsDevModel, bool) {
	if model, found := provider.Models[modelID]; found {
		return model, true
	}
	for _, model := range provider.Models {
		if strings.TrimSpace(model.ID) == modelID {
			return model, true
		}
	}
	return modelsDevModel{}, false
}

func providerMatchesOwner(owner string, candidates ...string) bool {
	if owner == "" {
		return false
	}
	for _, candidate := range candidates {
		candidate = normalizedProviderID(candidate)
		if candidate != "" && owner == candidate {
			return true
		}
	}
	return false
}

func normalizedProviderID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer(".", "", "-", "", "_", "", " ", "")
	value = replacer.Replace(value)
	switch value {
	case "claude":
		return "anthropic"
	case "gemini", "googleai", "aistudio":
		return "google"
	case "xai", "xaiapi", "grok":
		return "xai"
	}
	return value
}

func modelRateFromModelsDev(modelID, providerID string, model modelsDevModel, now time.Time) ModelRate {
	rate := ModelRate{
		Model: modelID, Provider: providerID, Source: "models.dev", UpdatedAt: now,
		InputUSDPerMillion: priceValue(model.Cost.Input), CacheReadUSDPerMillion: priceValue(model.Cost.CacheRead),
		CacheWriteUSDPerMillion: priceValue(model.Cost.CacheWrite), ReasoningUSDPerMillion: priceValue(model.Cost.Reasoning), ReasoningUsesOutput: model.Cost.Reasoning == nil, OutputUSDPerMillion: priceValue(model.Cost.Output),
	}
	for _, tier := range model.Cost.Tiers {
		if tier.Tier.Size <= 0 {
			continue
		}
		rate.Tiers = append(rate.Tiers, ModelRateTier{
			ContextOverTokens:  tier.Tier.Size,
			InputUSDPerMillion: inheritedPrice(tier.Input, model.Cost.Input), CacheReadUSDPerMillion: inheritedPrice(tier.CacheRead, model.Cost.CacheRead),
			CacheWriteUSDPerMillion: inheritedPrice(tier.CacheWrite, model.Cost.CacheWrite), ReasoningUSDPerMillion: inheritedPrice(tier.Reasoning, model.Cost.Reasoning), ReasoningUsesOutput: tier.Reasoning == nil && model.Cost.Reasoning == nil, OutputUSDPerMillion: inheritedPrice(tier.Output, model.Cost.Output),
		})
	}
	if legacy := model.Cost.ContextOver200K; legacy != nil && legacy.Input != nil && legacy.Output != nil {
		rate.Tiers = append(rate.Tiers, ModelRateTier{
			ContextOverTokens:  200_000,
			InputUSDPerMillion: inheritedPrice(legacy.Input, model.Cost.Input), CacheReadUSDPerMillion: inheritedPrice(legacy.CacheRead, model.Cost.CacheRead),
			CacheWriteUSDPerMillion: inheritedPrice(legacy.CacheWrite, model.Cost.CacheWrite), ReasoningUSDPerMillion: inheritedPrice(legacy.Reasoning, model.Cost.Reasoning), ReasoningUsesOutput: legacy.Reasoning == nil && model.Cost.Reasoning == nil, OutputUSDPerMillion: inheritedPrice(legacy.Output, model.Cost.Output),
		})
	}
	for name, mode := range model.Experimental.Modes {
		serviceTier, _ := mode.Provider.Body["service_tier"].(string)
		if strings.TrimSpace(serviceTier) == "" || mode.Cost.Input == nil || mode.Cost.Output == nil {
			continue
		}
		rate.Modes = append(rate.Modes, ModelRateMode{
			Name: name, ServiceTier: serviceTier,
			InputUSDPerMillion: inheritedPrice(mode.Cost.Input, model.Cost.Input), CacheReadUSDPerMillion: inheritedPrice(mode.Cost.CacheRead, model.Cost.CacheRead),
			CacheWriteUSDPerMillion: inheritedPrice(mode.Cost.CacheWrite, model.Cost.CacheWrite), ReasoningUSDPerMillion: inheritedPrice(mode.Cost.Reasoning, model.Cost.Reasoning), ReasoningUsesOutput: mode.Cost.Reasoning == nil && model.Cost.Reasoning == nil, OutputUSDPerMillion: inheritedPrice(mode.Cost.Output, model.Cost.Output),
		})
	}
	sort.Slice(rate.Modes, func(left, right int) bool { return rate.Modes[left].Name < rate.Modes[right].Name })
	return rate
}

func priceValue(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func inheritedPrice(value, fallback *float64) float64 {
	if value != nil {
		return *value
	}
	return priceValue(fallback)
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

func nextModelRateSync(status ModelRateSyncStatus, now time.Time) *time.Time {
	if !status.Enabled {
		return nil
	}
	if status.LastSuccess != nil && status.LastError == "" {
		return timePointer(status.LastSuccess.Add(modelRateSyncInterval))
	}
	if status.LastAttempt != nil {
		return timePointer(status.LastAttempt.Add(modelRateSyncRetry))
	}
	return timePointer(now)
}

func (engine *Engine) wakeModelRateSync() {
	select {
	case engine.rateSyncWake <- struct{}{}:
	default:
	}
}

// RequestModelRateSync queues a refresh on the managed synchronization loop so
// callers that update the CPA model catalog do not wait on external HTTP.
func (engine *Engine) RequestModelRateSync() {
	if engine == nil {
		return
	}
	engine.rateSyncForced.Store(true)
	engine.wakeModelRateSync()
}

func (engine *Engine) modelRateSyncLoop() {
	defer close(engine.rateSyncDone)
	for {
		status := engine.ModelRateSyncStatus()
		forced := engine.rateSyncForced.Swap(false)
		wait := modelRateSyncInterval
		if !status.Enabled {
			wait = modelRateSyncInterval
		} else if forced || status.NextSyncAt == nil || !status.NextSyncAt.After(engine.rateSyncNow()) {
			_ = engine.SyncModelRates(context.Background())
			continue
		} else {
			wait = time.Until(*status.NextSyncAt)
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-engine.rateSyncWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-engine.rateSyncStop:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}
