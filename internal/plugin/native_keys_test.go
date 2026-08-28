package plugin

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"cpa-key-policy/internal/policy"
)

func configureNativeTestApp(t *testing.T) (*App, string) {
	t.Helper()
	plain := "existing-cpa-system-key"
	hash, err := policy.HashKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp()
	yaml := []byte(`
enabled: true
auth_mode: cpa-native
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
keys:
  - id: native-a
    name: Native A
    enabled: true
    key_source: cpa-native
    key_hash: "` + hash + `"
    key_preview: "existing...m-key"
    caller_scope: "` + policy.CallerScopeForPrincipal(plain) + `"
    models:
      - alias: fast
        provider: codex
        target_model: gpt-5-codex
`)
	req, _ := json.Marshal(LifecycleRequest{ConfigYAML: yaml})
	if _, err := app.HandleMethod(MethodPluginReconfigure, req); err != nil {
		t.Fatalf("configure native app: %v", err)
	}
	return app, plain
}

func TestNativeRegistrationIsExclusiveAndAuthenticationReturnsOriginalCPAKey(t *testing.T) {
	app, plain := configureNativeTestApp(t)
	registration := app.registration()
	if !registration.Capabilities.FrontendAuthProviderExclusive {
		t.Fatalf("capabilities = %+v, want exclusive frontend auth", registration.Capabilities)
	}
	req, _ := json.Marshal(FrontendAuthRequest{
		Method: "POST", Path: "/v1/chat/completions",
		Headers: http.Header{"Authorization": {"Bearer " + plain}},
		Body:    []byte(`{"model":"fast"}`),
	})
	raw, err := app.HandleMethod(MethodFrontendAuthAuthenticate, req)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	var resp FrontendAuthResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Authenticated || resp.Principal != plain || resp.Metadata["key_id"] != "native-a" {
		t.Fatalf("auth response = %+v", resp)
	}
}

func TestNativeRegistrationRemainsExclusiveWithZeroBindings(t *testing.T) {
	app := NewApp()
	yaml := []byte("enabled: true\nauth_mode: cpa-native\nstate_file: \"" + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + "\"\n")
	req, _ := json.Marshal(LifecycleRequest{ConfigYAML: yaml})
	if _, err := app.HandleMethod(MethodPluginRegister, req); err != nil {
		t.Fatal(err)
	}
	if !app.registration().Capabilities.FrontendAuthProviderExclusive {
		t.Fatal("zero-binding cpa-native registration is not exclusive")
	}
	authReq, _ := json.Marshal(FrontendAuthRequest{
		Method: "POST", Path: "/v1/chat/completions",
		Headers: http.Header{"Authorization": {"Bearer unbound-native-key"}},
		Body:    []byte(`{"model":"fast"}`),
	})
	raw, err := app.HandleMethod(MethodFrontendAuthAuthenticate, authReq)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	var resp FrontendAuthResponse
	_ = json.Unmarshal(raw, &env)
	_ = json.Unmarshal(env.Result, &resp)
	if resp.Authenticated {
		t.Fatalf("zero-binding auth response = %+v", resp)
	}
}

func TestNativePolicyDenialAndUnboundKeyReturnUnauthenticated(t *testing.T) {
	app, plain := configureNativeTestApp(t)
	for name, tc := range map[string][2]string{
		"model denied": {plain, "slow"},
		"unbound":      {"another-cpa-system-key", "fast"},
	} {
		t.Run(name, func(t *testing.T) {
			key, model := tc[0], tc[1]
			req, _ := json.Marshal(FrontendAuthRequest{
				Method: "POST", Path: "/v1/chat/completions",
				Headers: http.Header{"Authorization": {"Bearer " + key}},
				Body:    []byte(`{"model":"` + model + `"}`),
			})
			raw, err := app.HandleMethod(MethodFrontendAuthAuthenticate, req)
			if err != nil {
				t.Fatal(err)
			}
			var env Envelope
			var resp FrontendAuthResponse
			_ = json.Unmarshal(raw, &env)
			_ = json.Unmarshal(env.Result, &resp)
			if resp.Authenticated {
				t.Fatalf("auth response = %+v, want denied", resp)
			}
		})
	}
}

func TestManagementBindsNativeKeyWithoutReturningSecretAndRefusesRotation(t *testing.T) {
	app, _ := configureTestApp(t)
	plain := "one-of-cpa-api-keys"
	body := []byte(`{"key":"` + plain + `","name":"CPA Primary","models":[{"alias":"fast","provider":"codex","target_model":"gpt-5-codex"}]}`)
	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/cpa-key-policy/native-keys/bind",
		Body:   body,
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bind status = %d body=%s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(string(resp.Body), plain) || strings.Contains(string(resp.Body), "key_hash") || strings.Contains(string(resp.Body), "caller_scope") {
		t.Fatalf("bind response leaked secret material: %s", resp.Body)
	}
	var payload struct {
		Key publicKey `json:"key"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Key.ID == "" || payload.Key.KeySource != policy.KeySourceCPANative {
		t.Fatalf("bound key = %+v", payload.Key)
	}

	keys := app.Store().Keys()
	var bound *policy.KeyConfig
	for i := range keys {
		if keys[i].ID == payload.Key.ID {
			bound = &keys[i]
		}
	}
	if bound == nil || bound.KeySource != policy.KeySourceCPANative || bound.CallerScope != policy.CallerScopeForPrincipal(plain) || policy.MatchHash(plain, bound.KeyHash) == false {
		t.Fatalf("persisted native binding = %+v", bound)
	}

	rotateReq, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/cpa-key-policy/keys/rotate",
		Body:   []byte(`{"id":"` + payload.Key.ID + `"}`),
	})
	raw, err = app.HandleMethod(MethodManagementHandle, rotateReq)
	if err != nil {
		t.Fatal(err)
	}
	resp = managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("native rotate status = %d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestNativeModeRefusesLegacyGeneratedKeyCreation(t *testing.T) {
	app, _ := configureNativeTestApp(t)
	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/cpa-key-policy/keys",
		Body:   []byte(`{"id":"must-not-generate"}`),
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("legacy create status = %d body=%s", resp.StatusCode, resp.Body)
	}
}
