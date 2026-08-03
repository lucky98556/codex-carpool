package quota

import (
	"fmt"
	"math"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// defaultDatabasePath lives beneath the plugin's own directory, so removing
	// codex-carpool also removes its independently owned SQLite data.
	// The native plugin never discovers or writes CLIProxyAPI's config path.
	pluginDataDirectory         = "/CLIProxyAPI/plugins/codex-carpool/data"
	defaultDatabasePath         = pluginDataDirectory + "/codex-carpool.db"
	defaultAuthDirectory        = "~/.cli-proxy-api"
	previousPluginDatabasePath  = "/CLIProxyAPI/plugins/codex-quota-guard/data/codex-quota-guard.db"
	legacyDefaultDatabasePath   = "/var/lib/cliproxyapi/codex-quota-guard.db"
	defaultRequestUnits         = int64(200000)
	defaultRecordRetention      = "720h"
	defaultSevenDayBaseRequests = int64(30)
	hmacFingerprintScheme       = "hmac-sha256-v1"
	// maxConfiguredUnits stays below the largest exactly-representable integer
	// in float64. This keeps percentage calculations safe while still allowing
	// an allowance far beyond any practical request-unit deployment.
	maxConfiguredUnits = int64(9_000_000_000_000_000)
)

// Config remains for direct Engine users and tests. The native entry point
// starts from an empty standalone config; settings and Key policies are
// persisted in the plugin's SQLite database. Legacy group fields remain only
// so old plugin databases can be opened and migrated without mutating CPA.
//
// request_units defines the fallback Token count when CPA reports a generated
// completion without usable token fields, and the uncalibrated Token/x scale.
// Admission itself persists only a one-Token correlation marker; normal
// completions always replace it with CPA's actual token usage.
type Config struct {
	Provider        string         `yaml:"provider"`
	DatabasePath    string         `yaml:"database_path"`
	AuthDirectory   string         `yaml:"auth_directory"`
	RequestUnits    int64          `yaml:"request_units"`
	KeyHMACSecret   string         `yaml:"key_hmac_secret"`
	RecordRetention string         `yaml:"record_retention"`
	AccountGroups   []AccountGroup `yaml:"account_groups"`
	BootstrapKeys   []KeyPolicy    `yaml:"bootstrap_keys"`
}

// InstallationSettings are operator-controlled settings persisted in the
// plugin's own SQLite database. The HMAC secret is intentionally excluded: it
// is generated once by the plugin and never returned to the browser.
type InstallationSettings struct {
	RequestUnits    int64  `json:"request_units"`
	RecordRetention string `json:"record_retention"`
	// AuthDirectory is CPA's file-backed auth-dir. codex-carpool reads only
	// Codex JSON files from this directory; it never writes CPA credentials.
	AuthDirectory   string `json:"auth_directory"`
}

type storedInstallationSettings struct {
	InstallationSettings
	KeyHMACSecret string `json:"key_hmac_secret"`
}

// AccountGroup is retained only to read legacy codex-quota-guard metadata.
// codex-carpool never creates, selects, or enforces account groups.
type AccountGroup struct {
	ID             string          `yaml:"id" json:"id"`
	Name           string          `yaml:"name" json:"name"`
	Accounts       []AccountConfig `yaml:"accounts" json:"accounts"`
	MaxConcurrency int             `yaml:"max_concurrency" json:"max_concurrency"`
}

// AccountConfig represents one CLIProxyAPI Codex OAuth auth record. Capacities
// are intentionally per account because subscriptions in the same group can
// have different entitlements, such as a 20x account next to a normal account.
// MaxConcurrency remains accepted for configuration compatibility, but cannot
// be enforced safely in admission metering because this host version has no
// reliable completion callback.
type AccountConfig struct {
	AuthID           string `yaml:"auth_id" json:"auth_id"`
	Name             string `yaml:"name" json:"name"`
	FiveHourCapacity int64  `yaml:"five_hour_capacity" json:"five_hour_capacity"`
	SevenDayCapacity int64  `yaml:"seven_day_capacity" json:"seven_day_capacity"`
	MaxConcurrency   int    `yaml:"max_concurrency" json:"max_concurrency"`
}

// KeyPolicy defines a downstream API key allocation. KeySHA256 is retained as
// a database-compatible field name, but stores an HMAC-SHA-256 fingerprint
// when key_hmac_secret is configured. Raw client keys are accepted only by the
// management API and are never saved or returned in plaintext.
type KeyPolicy struct {
	ID                 string   `yaml:"id" json:"id"`
	Name               string   `yaml:"name" json:"name"`
	KeySHA256          string   `yaml:"key_sha256" json:"-"`
	// AllocationX is the Key's single share of the plugin-wide Codex pool. The
	// official account windows remain independent and synchronized from Codex;
	// AllocationX is charged against the selected account's official weekly
	// reset window so a Key cannot consume more than its allocated share.
	AllocationX        float64  `yaml:"allocation_x" json:"allocation_x"`
	// These fields exist only to read legacy database rows. The management API
	// deliberately does not accept or return them: codex-carpool has one
	// allocation_x, while Codex remains the source of both official windows.
	FiveHourMultiplier float64  `yaml:"five_hour_multiplier" json:"-"`
	SevenDayMultiplier float64  `yaml:"seven_day_multiplier" json:"-"`
	AllowedModels      []string `yaml:"allowed_models" json:"allowed_models"`
	// AccessRules is optional. An empty list preserves the original unrestricted
	// behavior; otherwise the Key can be routed only during one of its weekly
	// recurring intervals in AccessTimezone.
	AccessRules       []AccessRule `yaml:"access_rules" json:"access_rules"`
	AccessTimezone    string       `yaml:"access_timezone" json:"access_timezone"`
	// FingerprintScheme distinguishes HMAC fingerprints created by this plugin
	// from copied legacy SHA-256 rows. Legacy rows stay paused until the operator
	// rebinds the Key from CPA; they must never silently become active.
	FingerprintScheme string `yaml:"-" json:"-"`
	NeedsRebind       bool   `yaml:"-" json:"needs_rebind"`
	// Legacy fields remain only so copied codex-quota-guard databases can be
	// opened safely. New policies are saved with AllocationX only.
	GroupID         string  `yaml:"group_id" json:"-"`
	FiveHourPercent float64 `yaml:"five_hour_percent" json:"-"`
	SevenDayPercent float64 `yaml:"seven_day_percent" json:"-"`
	// MaxConcurrency is retained for backwards-compatible policy rows. It is
	// not part of codex-carpool's multiplier policy and is never exposed by the
	// current management API.
	MaxConcurrency int  `yaml:"max_concurrency" json:"-"`
	Enabled        bool `yaml:"enabled" json:"enabled"`
}

// AccountPoolEntry is one Codex account admitted to the shared pool. AuthID is
// the file-backed account identity used for local quota reads and accounting.
// AuthIndex optionally preserves CPA's scheduler-facing credential ID when it
// differs from the relative auth-file path.
type AccountPoolEntry struct {
	AuthID     string  `json:"auth_id"`
	AuthIndex  string  `json:"auth_index"`
	Name       string  `json:"name"`
	CapacityX  float64 `json:"capacity_x"`
	Enabled    bool    `json:"enabled"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RuntimeConfig is the validated shape used by the scheduler hot path.
type RuntimeConfig struct {
	Config
	RecordRetentionDuration time.Duration
	Groups                  map[string]AccountGroup
	AuthToGroup             map[string]string
	Accounts                map[string]AccountConfig
}

// DecodeConfig parses and validates the YAML supplied by CLIProxyAPI.
func DecodeConfig(raw []byte) (RuntimeConfig, error) {
	var cfg Config
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return RuntimeConfig{}, fmt.Errorf("decode config: %w", err)
		}
	}
	return NormalizeConfig(cfg)
}

// NormalizeConfig fills safe defaults and validates all policy invariants.
func NormalizeConfig(cfg Config) (RuntimeConfig, error) {
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.Provider == "" {
		cfg.Provider = "codex"
	}
	if cfg.Provider != "codex" {
		return RuntimeConfig{}, fmt.Errorf("provider must be codex")
	}
	cfg.DatabasePath = strings.TrimSpace(cfg.DatabasePath)
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = defaultDatabasePath
	}
	if cfg.DatabasePath == ":memory:" {
		return RuntimeConfig{}, fmt.Errorf("database_path must be a persistent filesystem path, not :memory:")
	}
	cfg.AuthDirectory = strings.TrimSpace(cfg.AuthDirectory)
	if cfg.AuthDirectory == "" {
		cfg.AuthDirectory = defaultAuthDirectory
	}
	if cfg.RequestUnits <= 0 {
		cfg.RequestUnits = defaultRequestUnits
	}
	if cfg.RequestUnits > maxConfiguredUnits {
		return RuntimeConfig{}, fmt.Errorf("request_units must not exceed %d", maxConfiguredUnits)
	}
	cfg.KeyHMACSecret = strings.TrimSpace(cfg.KeyHMACSecret)
	if cfg.KeyHMACSecret != "" && (len(cfg.KeyHMACSecret) < 32 || strings.Contains(strings.ToLower(cfg.KeyHMACSecret), "replace-with")) {
		return RuntimeConfig{}, fmt.Errorf("key_hmac_secret must be a private value with at least 32 characters")
	}
	if cfg.RecordRetention == "" {
		cfg.RecordRetention = defaultRecordRetention
	}
	recordRetention, err := time.ParseDuration(cfg.RecordRetention)
	if err != nil || recordRetention < 7*24*time.Hour {
		return RuntimeConfig{}, fmt.Errorf("record_retention must be at least 168h")
	}

	groups := make(map[string]AccountGroup, len(cfg.AccountGroups))
	authToGroup := make(map[string]string)
	accounts := make(map[string]AccountConfig)
	for index := range cfg.AccountGroups {
		group := cfg.AccountGroups[index]
		group.ID = strings.TrimSpace(group.ID)
		group.Name = strings.TrimSpace(group.Name)
		if group.ID == "" {
			return RuntimeConfig{}, fmt.Errorf("account_groups[%d].id is required", index)
		}
		if _, exists := groups[group.ID]; exists {
			return RuntimeConfig{}, fmt.Errorf("duplicate account group %q", group.ID)
		}
		if group.MaxConcurrency < 0 {
			return RuntimeConfig{}, fmt.Errorf("account group %q max_concurrency cannot be negative", group.ID)
		}
		if len(group.Accounts) == 0 {
			return RuntimeConfig{}, fmt.Errorf("account group %q must contain at least one account", group.ID)
		}
		seenAuth := make(map[string]struct{}, len(group.Accounts))
		var fiveHourTotal, sevenDayTotal int64
		for accountIndex := range group.Accounts {
			account := group.Accounts[accountIndex]
			account.AuthID = strings.TrimSpace(account.AuthID)
			account.Name = strings.TrimSpace(account.Name)
			if account.AuthID == "" {
				return RuntimeConfig{}, fmt.Errorf("account group %q contains an empty auth_id", group.ID)
			}
			if account.FiveHourCapacity <= 0 && account.SevenDayCapacity <= 0 {
				return RuntimeConfig{}, fmt.Errorf("account %q must set positive 5-hour and 7-day capacities", account.AuthID)
			}
			if account.FiveHourCapacity <= 0 {
				return RuntimeConfig{}, fmt.Errorf("account %q must set a positive 5-hour capacity", account.AuthID)
			}
			if account.SevenDayCapacity <= 0 {
				return RuntimeConfig{}, fmt.Errorf("account %q must set a positive 7-day capacity", account.AuthID)
			}
			if account.FiveHourCapacity > maxConfiguredUnits || account.SevenDayCapacity > maxConfiguredUnits {
				return RuntimeConfig{}, fmt.Errorf("account %q capacities must not exceed %d", account.AuthID, maxConfiguredUnits)
			}
			if fiveHourTotal > maxConfiguredUnits-account.FiveHourCapacity || sevenDayTotal > maxConfiguredUnits-account.SevenDayCapacity {
				return RuntimeConfig{}, fmt.Errorf("account group %q combined capacities must not exceed %d", group.ID, maxConfiguredUnits)
			}
			fiveHourTotal += account.FiveHourCapacity
			sevenDayTotal += account.SevenDayCapacity
			if account.MaxConcurrency < 0 {
				return RuntimeConfig{}, fmt.Errorf("account %q max_concurrency cannot be negative", account.AuthID)
			}
			if _, exists := seenAuth[account.AuthID]; exists {
				return RuntimeConfig{}, fmt.Errorf("account group %q repeats auth_id %q", group.ID, account.AuthID)
			}
			if other, exists := authToGroup[account.AuthID]; exists {
				return RuntimeConfig{}, fmt.Errorf("auth_id %q belongs to both %q and %q", account.AuthID, other, group.ID)
			}
			seenAuth[account.AuthID] = struct{}{}
			authToGroup[account.AuthID] = group.ID
			accounts[account.AuthID] = account
			group.Accounts[accountIndex] = account
		}
		groups[group.ID] = group
		cfg.AccountGroups[index] = group
	}
	for index := range cfg.BootstrapKeys {
		policy, err := normalizePolicy(cfg.BootstrapKeys[index], groups, cfg.RequestUnits)
		if err != nil {
			return RuntimeConfig{}, fmt.Errorf("bootstrap_keys[%d]: %w", index, err)
		}
		cfg.BootstrapKeys[index] = policy
	}
	return RuntimeConfig{
		Config:                  cfg,
		RecordRetentionDuration: recordRetention,
		Groups:                  groups,
		AuthToGroup:             authToGroup,
		Accounts:                accounts,
	}, nil
}

// StandaloneRuntimeConfig is used by the native entry point. It intentionally
// contains no account groups, secret, or key policies: those are managed by
// the plugin's SQLite-backed setup wizard rather than CLIProxyAPI's YAML.
func StandaloneRuntimeConfig() (RuntimeConfig, error) {
	return NormalizeConfig(Config{DatabasePath: defaultDatabasePath})
}

func normalizeInstallationSettings(input InstallationSettings, secret string) (storedInstallationSettings, error) {
	secret = strings.TrimSpace(secret)
	if len(secret) < 32 {
		return storedInstallationSettings{}, fmt.Errorf("installation HMAC secret must be at least 32 characters")
	}
	runtime, err := NormalizeConfig(Config{
		DatabasePath:    defaultDatabasePath,
		AuthDirectory:   input.AuthDirectory,
		RequestUnits:    input.RequestUnits,
		KeyHMACSecret:   secret,
		RecordRetention: input.RecordRetention,
	})
	if err != nil {
		return storedInstallationSettings{}, err
	}
	return storedInstallationSettings{
		InstallationSettings: InstallationSettings{
			RequestUnits:    runtime.RequestUnits,
			RecordRetention: runtime.RecordRetention,
			AuthDirectory:   runtime.AuthDirectory,
		},
		KeyHMACSecret: secret,
	}, nil
}

func runtimeConfigFromInstallation(base RuntimeConfig, settings storedInstallationSettings) (RuntimeConfig, error) {
	cfg := base.Config
	cfg.Provider = "codex"
	cfg.RequestUnits = settings.RequestUnits
	cfg.RecordRetention = settings.RecordRetention
	cfg.AuthDirectory = settings.AuthDirectory
	cfg.KeyHMACSecret = settings.KeyHMACSecret
	// Account groups are intentionally ignored by codex-carpool. A policy is
	// keyed only by its downstream Key, so routing across any Codex account can
	// never reset that Key's 5-hour or 7-day usage.
	cfg.AccountGroups = nil
	return NormalizeConfig(cfg)
}

// FiveHourCapacity returns the combined group pool. A key percentage always
// applies to this aggregate, while the scheduler still protects every account
// independently using its own capacity.
func (group AccountGroup) FiveHourCapacity() int64 {
	var total int64
	for _, account := range group.Accounts {
		total += account.FiveHourCapacity
	}
	return total
}

// SevenDayCapacity returns the combined group pool.
func (group AccountGroup) SevenDayCapacity() int64 {
	var total int64
	for _, account := range group.Accounts {
		total += account.SevenDayCapacity
	}
	return total
}

func normalizePolicy(policy KeyPolicy, groups map[string]AccountGroup, requestUnits int64) (KeyPolicy, error) {
	_ = groups
	_ = requestUnits
	policy.ID = strings.TrimSpace(policy.ID)
	policy.Name = strings.TrimSpace(policy.Name)
	policy.KeySHA256 = strings.ToLower(strings.TrimSpace(policy.KeySHA256))
	policy.FingerprintScheme = strings.TrimSpace(policy.FingerprintScheme)
	if policy.ID == "" || policy.Name == "" || policy.KeySHA256 == "" {
		return KeyPolicy{}, fmt.Errorf("id, name and key_sha256 are required")
	}
	if len(policy.KeySHA256) != 64 {
		return KeyPolicy{}, fmt.Errorf("key_sha256 must be a 64-character HMAC-SHA-256 fingerprint")
	}
	for _, char := range policy.KeySHA256 {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return KeyPolicy{}, fmt.Errorf("key_sha256 must be lowercase hexadecimal")
		}
	}
	// Old databases only have the two rolling-window fields. Preserve their
	// intent during migration by using the larger historical allocation, while
	// all newly saved policies use AllocationX exclusively.
	if policy.AllocationX <= 0 {
		policy.AllocationX = math.Max(policy.FiveHourMultiplier, policy.SevenDayMultiplier)
	}
	if math.IsNaN(policy.AllocationX) || math.IsInf(policy.AllocationX, 0) {
		return KeyPolicy{}, fmt.Errorf("allocation_x must be a finite number")
	}
	if policy.Enabled && policy.AllocationX <= 0 {
		return KeyPolicy{}, fmt.Errorf("enabled Key policies require a positive allocation_x")
	}
	if policy.FingerprintScheme == "" {
		policy.FingerprintScheme = hmacFingerprintScheme
	}
	if policy.Enabled && policy.FingerprintScheme != hmacFingerprintScheme {
		return KeyPolicy{}, fmt.Errorf("this legacy Key policy must be rebound to a CPA API Key before it can be enabled")
	}
	policy.NeedsRebind = policy.FingerprintScheme != hmacFingerprintScheme
	modelSet := make(map[string]struct{}, len(policy.AllowedModels))
	models := make([]string, 0, len(policy.AllowedModels))
	for _, model := range policy.AllowedModels {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := modelSet[model]; exists {
			continue
		}
		modelSet[model] = struct{}{}
		models = append(models, model)
	}
	policy.AllowedModels = models
	accessRules, accessTimezone, err := normalizeAccessRules(policy.AccessRules, policy.AccessTimezone)
	if err != nil {
		return KeyPolicy{}, err
	}
	policy.AccessRules = accessRules
	policy.AccessTimezone = accessTimezone
	// Keep copied legacy rows distinguishable in SQLite but never let them
	// affect scheduling: their enabled flag is disabled during migration.
	policy.GroupID = ""
	policy.FiveHourPercent = 0
	policy.SevenDayPercent = 0
	if policy.MaxConcurrency < 1 {
		policy.MaxConcurrency = 1
	}
	return policy, nil
}

func normalizeAccountPoolEntry(entry AccountPoolEntry) (AccountPoolEntry, error) {
	entry.AuthID = strings.TrimSpace(entry.AuthID)
	entry.AuthIndex = strings.TrimSpace(entry.AuthIndex)
	entry.Name = strings.TrimSpace(entry.Name)
	if entry.AuthID == "" {
		return AccountPoolEntry{}, fmt.Errorf("auth_id is required")
	}
	if entry.Name == "" {
		entry.Name = entry.AuthID
	}
	if math.IsNaN(entry.CapacityX) || math.IsInf(entry.CapacityX, 0) || entry.CapacityX <= 0 {
		return AccountPoolEntry{}, fmt.Errorf("capacity_x must be a positive finite number")
	}
	if entry.CapacityX > float64(maxConfiguredUnits) {
		return AccountPoolEntry{}, fmt.Errorf("capacity_x must not exceed %d", maxConfiguredUnits)
	}
	return entry, nil
}
