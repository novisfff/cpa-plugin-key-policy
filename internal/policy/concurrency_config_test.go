package policy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConcurrencyConfigDefaults(t *testing.T) {
	cfg, err := DecodeConfig([]byte("enabled: true\n"))
	if err != nil {
		t.Fatalf("DecodeConfig() error = %v", err)
	}
	want := DefaultConcurrencyConfig()
	if cfg.Concurrency != want {
		t.Fatalf("Concurrency = %+v, want defaults %+v", cfg.Concurrency, want)
	}
	if cfg.Concurrency.Enabled {
		t.Fatal("concurrency must be disabled when old config omits the block")
	}
}

func TestConcurrencyConfigYAML(t *testing.T) {
	cfg, err := DecodeConfig([]byte(`
enabled: true
concurrency:
  enabled: true
  global_limit: 9
  queue_timeout: 45s
  max_queue_per_key: 7
`))
	if err != nil {
		t.Fatalf("DecodeConfig() error = %v", err)
	}
	if !cfg.Concurrency.Enabled || cfg.Concurrency.GlobalLimit != 9 ||
		time.Duration(cfg.Concurrency.QueueTimeout) != 45*time.Second ||
		cfg.Concurrency.MaxQueuePerKey != 7 {
		t.Fatalf("Concurrency = %+v", cfg.Concurrency)
	}
}

func TestConcurrencyConfigRejectsInvalidValues(t *testing.T) {
	for _, raw := range []string{
		"concurrency:\n  enabled: true\n  global_limit: 0\n  queue_timeout: 60s\n  max_queue_per_key: 32\n",
		"concurrency:\n  enabled: true\n  global_limit: 6\n  queue_timeout: 0s\n  max_queue_per_key: 32\n",
		"concurrency:\n  enabled: true\n  global_limit: 6\n  queue_timeout: 60s\n  max_queue_per_key: 0\n",
	} {
		if _, err := DecodeConfig([]byte(raw)); err == nil {
			t.Fatalf("DecodeConfig(%q) succeeded, want validation error", raw)
		}
	}
}

func TestConcurrencyOldStateDefaultsDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"keys":[],"updated_at":"2026-08-28T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if got := store.ConcurrencyConfig(); got != DefaultConcurrencyConfig() || got.Enabled {
		t.Fatalf("ConcurrencyConfig() = %+v, want disabled defaults", got)
	}
}

func TestConcurrencyConfigPersistsAndStateWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	seed := ConcurrencyConfig{
		Enabled:        true,
		GlobalLimit:    6,
		QueueTimeout:   Duration(60 * time.Second),
		MaxQueuePerKey: 32,
	}
	if err := store.Configure(Config{Enabled: true, StateFile: path, Concurrency: seed}); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	updated := ConcurrencyConfig{
		Enabled:        true,
		GlobalLimit:    8,
		QueueTimeout:   Duration(25 * time.Second),
		MaxQueuePerKey: 11,
	}
	if err := store.UpdateConcurrencyConfig(updated); err != nil {
		t.Fatalf("UpdateConcurrencyConfig() error = %v", err)
	}

	state, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.Concurrency == nil || *state.Concurrency != updated {
		t.Fatalf("persisted concurrency = %+v, want %+v", state.Concurrency, updated)
	}

	reloaded := NewStore()
	yamlAttempt := ConcurrencyConfig{
		Enabled:        true,
		GlobalLimit:    3,
		QueueTimeout:   Duration(5 * time.Second),
		MaxQueuePerKey: 2,
	}
	if err := reloaded.Configure(Config{Enabled: true, StateFile: path, Concurrency: yamlAttempt}); err != nil {
		t.Fatalf("reload Configure() error = %v", err)
	}
	if got := reloaded.ConcurrencyConfig(); got != updated {
		t.Fatalf("reloaded concurrency = %+v, want state value %+v", got, updated)
	}
}

func TestSaveUsageOnlyPreservesConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	want := ConcurrencyConfig{
		Enabled:        true,
		GlobalLimit:    4,
		QueueTimeout:   Duration(10 * time.Second),
		MaxQueuePerKey: 9,
	}
	if err := store.Configure(Config{Enabled: true, StateFile: path, Concurrency: want}); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if err := store.FlushUsage(); err != nil {
		t.Fatalf("FlushUsage() error = %v", err)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.Concurrency == nil || *state.Concurrency != want {
		t.Fatalf("concurrency after usage flush = %+v, want %+v", state.Concurrency, want)
	}
}

func TestDurationJSONRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := ConcurrencyConfig{
		Enabled:        true,
		GlobalLimit:    2,
		QueueTimeout:   Duration(1500 * time.Millisecond),
		MaxQueuePerKey: 4,
	}
	if err := SaveStateWithConcurrency(path, nil, nil, nil, nil, want); err != nil {
		t.Fatalf("SaveStateWithConcurrency() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if stringContains := string(raw); !containsAll(stringContains, `"queue_timeout": "1.5s"`) {
		t.Fatalf("state duration is not human-readable: %s", raw)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.Concurrency == nil || *state.Concurrency != want {
		t.Fatalf("round-trip concurrency = %+v, want %+v", state.Concurrency, want)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if len(part) > len(value) {
			return false
		}
		found := false
		for index := 0; index+len(part) <= len(value); index++ {
			if value[index:index+len(part)] == part {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
