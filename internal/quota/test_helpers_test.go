package quota

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestEngine(t *testing.T, policy KeyPolicy) *Engine {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("native plugin database lock is Linux-only")
	}
	secret := "test-only-hmac-secret-with-at-least-32-characters"
	policy.KeySHA256 = FingerprintAPIKey("managed-key", secret)
	cfg, err := NormalizeConfig(Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		KeyHMACSecret:   secret,
		RecordRetention: "168h",
		BootstrapKeys:   []KeyPolicy{policy},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	// Most admission tests exercise routing, schedule, or content rules. Keep
	// their common gpt-5 fixture rate configured at $0 so the mandatory
	// price-card gate does not mask the behavior under test.
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-5"}}); err != nil {
		_ = engine.Close()
		t.Fatalf("ReplaceModelRates(default test rate) error = %v", err)
	}
	return engine
}

func readyNativeCandidate(t *testing.T, engine *Engine, now time.Time) []SchedulerCandidate {
	t.Helper()
	_ = engine
	_ = now
	return []SchedulerCandidate{{AuthID: "account-a"}}
}
