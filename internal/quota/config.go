package quota

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// The plugin owns one private SQLite database and never mutates CPA's config.
	pluginDataDirectory    = "/CLIProxyAPI/plugins/codex-carpool/data"
	defaultDatabasePath    = pluginDataDirectory + "/codex-carpool.db"
	defaultRecordRetention = "720h"
)

// Config contains only settings used by the current Key dollar-meter product.
// CPA supplies scheduler candidates independently for each request.
type Config struct {
	Provider        string      `yaml:"provider"`
	DatabasePath    string      `yaml:"database_path"`
	KeyHMACSecret   string      `yaml:"key_hmac_secret"`
	RecordRetention string      `yaml:"record_retention"`
	BootstrapKeys   []KeyPolicy `yaml:"bootstrap_keys"`
}

// InstallationSettings are the non-secret runtime settings persisted in the
// plugin database.
type InstallationSettings struct {
	RecordRetention string `json:"record_retention"`
}

type storedInstallationSettings struct {
	InstallationSettings
	KeyHMACSecret string `json:"key_hmac_secret"`
}

// KeyPolicy governs one downstream CPA API Key. Enabled controls only rolling
// dollar-budget rejection; every registered Key remains fully metered. Empty
// model selection means all CPA-synchronized models, and a zero budget means
// that window is unlimited.
type KeyPolicy struct {
	ID                string       `yaml:"id" json:"id"`
	Name              string       `yaml:"name" json:"name"`
	KeySHA256         string       `yaml:"key_sha256" json:"-"`
	KeySuffix         string       `yaml:"key_suffix" json:"key_suffix,omitempty"`
	FiveHourBudgetUSD float64      `yaml:"five_hour_budget_usd" json:"five_hour_budget_usd"`
	SevenDayBudgetUSD float64      `yaml:"seven_day_budget_usd" json:"seven_day_budget_usd"`
	AllowedModels     []string     `yaml:"allowed_models" json:"allowed_models"`
	AccessRules       []AccessRule `yaml:"access_rules" json:"access_rules"`
	AccessTimezone    string       `yaml:"access_timezone" json:"access_timezone"`
	Enabled           bool         `yaml:"enabled" json:"enabled"`
}

// RuntimeConfig is the validated immutable/startup view used by the engine.
type RuntimeConfig struct {
	Config
	RecordRetentionDuration time.Duration
}

func DecodeConfig(raw []byte) (RuntimeConfig, error) {
	var cfg Config
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return RuntimeConfig{}, fmt.Errorf("decode config: %w", err)
		}
	}
	return NormalizeConfig(cfg)
}

// NormalizeConfig validates only the current dollar-meter settings.
func NormalizeConfig(cfg Config) (RuntimeConfig, error) {
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.Provider == "" {
		cfg.Provider = "all"
	}
	cfg.DatabasePath = strings.TrimSpace(cfg.DatabasePath)
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = defaultDatabasePath
	}
	if cfg.DatabasePath == ":memory:" {
		return RuntimeConfig{}, fmt.Errorf("database_path must be a persistent filesystem path, not :memory:")
	}
	cfg.KeyHMACSecret = strings.TrimSpace(cfg.KeyHMACSecret)
	if cfg.KeyHMACSecret != "" && (len(cfg.KeyHMACSecret) < 32 || strings.Contains(strings.ToLower(cfg.KeyHMACSecret), "replace-with")) {
		return RuntimeConfig{}, fmt.Errorf("key_hmac_secret must be a private value with at least 32 characters")
	}
	if cfg.RecordRetention == "" {
		cfg.RecordRetention = defaultRecordRetention
	}
	retention, err := time.ParseDuration(cfg.RecordRetention)
	if err != nil || retention < 7*24*time.Hour {
		return RuntimeConfig{}, fmt.Errorf("record_retention must be at least 168h")
	}
	for index := range cfg.BootstrapKeys {
		policy, err := normalizePolicy(cfg.BootstrapKeys[index])
		if err != nil {
			return RuntimeConfig{}, fmt.Errorf("bootstrap_keys[%d]: %w", index, err)
		}
		cfg.BootstrapKeys[index] = policy
	}
	return RuntimeConfig{Config: cfg, RecordRetentionDuration: retention}, nil
}

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
		KeyHMACSecret:   secret,
		RecordRetention: input.RecordRetention,
	})
	if err != nil {
		return storedInstallationSettings{}, err
	}
	return storedInstallationSettings{
		InstallationSettings: InstallationSettings{RecordRetention: runtime.RecordRetention},
		KeyHMACSecret:        secret,
	}, nil
}

func runtimeConfigFromInstallation(base RuntimeConfig, settings storedInstallationSettings) (RuntimeConfig, error) {
	cfg := base.Config
	cfg.Provider = "all"
	cfg.RecordRetention = settings.RecordRetention
	cfg.KeyHMACSecret = settings.KeyHMACSecret
	return NormalizeConfig(cfg)
}

func normalizePolicy(policy KeyPolicy) (KeyPolicy, error) {
	policy.ID = strings.TrimSpace(policy.ID)
	policy.Name = strings.TrimSpace(policy.Name)
	policy.KeySHA256 = strings.ToLower(strings.TrimSpace(policy.KeySHA256))
	policy.KeySuffix = normalizeAPIKeySuffix(policy.KeySuffix)
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
	if _, err := dollarBudgetMicros(policy.FiveHourBudgetUSD); err != nil {
		return KeyPolicy{}, fmt.Errorf("five_hour_budget_usd: %w", err)
	}
	if _, err := dollarBudgetMicros(policy.SevenDayBudgetUSD); err != nil {
		return KeyPolicy{}, fmt.Errorf("seven_day_budget_usd: %w", err)
	}
	seen := make(map[string]struct{}, len(policy.AllowedModels))
	models := make([]string, 0, len(policy.AllowedModels))
	for _, model := range policy.AllowedModels {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	sort.Strings(models)
	policy.AllowedModels = models
	rules, timezone, err := normalizeAccessRules(policy.AccessRules, policy.AccessTimezone)
	if err != nil {
		return KeyPolicy{}, err
	}
	policy.AccessRules = rules
	policy.AccessTimezone = timezone
	return policy, nil
}

func APIKeySuffix(raw string) string { return normalizeAPIKeySuffix(raw) }

func normalizeAPIKeySuffix(raw string) string {
	runes := []rune(strings.TrimSpace(raw))
	if len(runes) > 4 {
		runes = runes[len(runes)-4:]
	}
	return string(runes)
}
