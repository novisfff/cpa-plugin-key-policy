package policy

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nativeKeyConfig(t *testing.T, id, plain string, enabled bool) KeyConfig {
	t.Helper()
	hash, err := HashKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	return KeyConfig{
		ID:          id,
		Name:        id,
		Enabled:     enabled,
		KeyHash:     hash,
		KeyPreview:  PreviewKey(plain),
		KeySource:   KeySourceCPANative,
		CallerScope: CallerScopeForPrincipal(plain),
		Models: []ModelRule{
			{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex"},
		},
	}
}

func TestDecodeConfigDefaultsToPluginAuthModeAndMigratesLegacyKeySource(t *testing.T) {
	cfg, err := DecodeConfig([]byte(`
enabled: true
keys:
  - id: legacy
    enabled: true
    key_hash: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthMode != AuthModePlugin {
		t.Fatalf("auth mode = %q, want %q", cfg.AuthMode, AuthModePlugin)
	}
	if len(cfg.Keys) != 1 || cfg.Keys[0].KeySource != KeySourcePlugin {
		t.Fatalf("keys = %+v, want migrated plugin source", cfg.Keys)
	}
}

func TestDecodeConfigRejectsUnknownAuthModeAndInvalidNativeScope(t *testing.T) {
	if _, err := DecodeConfig([]byte("auth_mode: mystery\n")); err == nil || !strings.Contains(err.Error(), "auth_mode") {
		t.Fatalf("unknown auth mode error = %v", err)
	}
	_, err := DecodeConfig([]byte(`
auth_mode: cpa-native
keys:
  - id: native
    enabled: true
    key_source: cpa-native
    key_hash: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    caller_scope: not-a-scope
`))
	if err == nil || !strings.Contains(err.Error(), "caller_scope") {
		t.Fatalf("invalid native caller scope error = %v", err)
	}
}

func TestStoreNativeModeWithoutBindingsLoadsFailClosed(t *testing.T) {
	store := NewStore()
	err := store.Configure(Config{
		Enabled:   true,
		AuthMode:  AuthModeCPANative,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
	})
	if err != nil {
		t.Fatalf("Configure error = %v", err)
	}
	decision := store.Authenticate("POST", "/v1/chat/completions", http.Header{"Authorization": {"Bearer any-cpa-key"}}, nil, []byte(`{"model":"fast"}`))
	if decision.Known || decision.Allowed || store.AuthMode() != AuthModeCPANative {
		t.Fatalf("zero-binding native decision = %+v mode=%q", decision, store.AuthMode())
	}
}

func TestNativeModeUsesRawCPAKeyPrincipalAndIgnoresLegacySource(t *testing.T) {
	nativePlain := "existing-cpa-native-key"
	legacyPlain := "cpa_old_generated_key"
	legacyHash, err := HashKey(legacyPlain)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	err = store.Configure(Config{
		Enabled:   true,
		AuthMode:  AuthModeCPANative,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{
			nativeKeyConfig(t, "native-a", nativePlain, true),
			{
				ID: "legacy", Enabled: true, KeyHash: legacyHash,
				KeySource: KeySourcePlugin,
				Models:    []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	allowed := store.Authenticate("POST", "/v1/chat/completions", http.Header{"Authorization": {"Bearer " + nativePlain}}, nil, []byte(`{"model":"fast"}`))
	if !allowed.Known || !allowed.Allowed || allowed.Principal != nativePlain || allowed.KeyID != "native-a" {
		t.Fatalf("native decision = %+v", allowed)
	}
	legacy := store.Authenticate("POST", "/v1/chat/completions", http.Header{"Authorization": {"Bearer " + legacyPlain}}, nil, []byte(`{"model":"fast"}`))
	if legacy.Known || legacy.Allowed {
		t.Fatalf("legacy decision in native mode = %+v, want unknown", legacy)
	}
	if got := store.AuthMode(); got != AuthModeCPANative {
		t.Fatalf("AuthMode() = %q", got)
	}
}

func TestNativeCallerScopeAndRawUsageIdentityResolveWithoutPlaintextState(t *testing.T) {
	plain := "keeper-visible-native-key"
	key := nativeKeyConfig(t, "native-usage", plain, true)
	key.Models[0].InputPricePerMillion = 1
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	if err := store.Configure(Config{
		Enabled: true, AuthMode: AuthModeCPANative, StateFile: statePath,
		Keys: []KeyConfig{key},
	}); err != nil {
		t.Fatal(err)
	}
	if principal, ok := store.PrincipalForCallerScope(CallerScopeForPrincipal(plain)); !ok || principal != "native-usage" {
		t.Fatalf("caller scope resolved %q, %v", principal, ok)
	}
	cost := store.RecordUsage(plain, "fast", "gpt-5-codex", false, UsageDetail{InputTokens: 1_000_000})
	if cost != 1 {
		t.Fatalf("native raw-principal cost = %v, want 1", cost)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), plain) {
		t.Fatalf("state contains plaintext native key: %s", raw)
	}
}

func TestNativeBindingCanBeStagedInPluginModeThenActivatedFromState(t *testing.T) {
	plain := "staged-cpa-native-key"
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, AuthMode: AuthModePlugin, StateFile: statePath}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertKey(nativeKeyConfig(t, "staged", plain, true), true); err != nil {
		t.Fatal(err)
	}
	before := store.Authenticate("POST", "/v1/chat/completions", http.Header{"Authorization": {"Bearer " + plain}}, nil, []byte(`{"model":"fast"}`))
	if before.Known || before.Allowed {
		t.Fatalf("staged native key active in plugin mode: %+v", before)
	}
	if err := store.Configure(Config{Enabled: true, AuthMode: AuthModeCPANative, StateFile: statePath}); err != nil {
		t.Fatal(err)
	}
	after := store.Authenticate("POST", "/v1/chat/completions", http.Header{"Authorization": {"Bearer " + plain}}, nil, []byte(`{"model":"fast"}`))
	if !after.Known || !after.Allowed || after.Principal != plain {
		t.Fatalf("staged native key after mode switch: %+v", after)
	}
}
