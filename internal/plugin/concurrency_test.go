package plugin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"cpa-key-policy/internal/policy"
)

func configureConcurrencyApp(t *testing.T, enabled bool, limit int, timeout time.Duration, maxQueue int) (*App, map[string]string) {
	t.Helper()
	app := NewApp()
	plains := map[string]string{
		"team-a": "cpa_concurrency_a",
		"team-b": "cpa_concurrency_b",
	}
	cfg := policy.Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Concurrency: policy.ConcurrencyConfig{
			Enabled:        enabled,
			GlobalLimit:    limit,
			QueueTimeout:   policy.Duration(timeout),
			MaxQueuePerKey: maxQueue,
		},
	}
	for _, id := range []string{"team-a", "team-b"} {
		hash, err := policy.HashKey(plains[id])
		if err != nil {
			t.Fatal(err)
		}
		cfg.Keys = append(cfg.Keys, policy.KeyConfig{
			ID:         id,
			Name:       "Name " + id,
			Enabled:    true,
			KeyHash:    hash,
			KeyPreview: policy.PreviewKey(plains[id]),
		})
	}
	rawYAML, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// JSON is valid YAML and preserves the custom Duration string encoding.
	req, _ := json.Marshal(LifecycleRequest{ConfigYAML: rawYAML})
	if _, err := app.HandleMethod(MethodPluginReconfigure, req); err != nil {
		t.Fatalf("configure: %v", err)
	}
	t.Cleanup(app.Shutdown)
	return app, plains
}

func TestNewAppDoesNotPersistDefaultConcurrencyBeforeRegistration(t *testing.T) {
	workDir := t.TempDir()
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	statePath := filepath.Join(workDir, policy.DefaultConfig().StateFile)

	app := NewApp()
	t.Cleanup(app.Shutdown)
	if _, err := os.Stat(policy.DefaultConfig().StateFile); !os.IsNotExist(err) {
		t.Fatalf("state file exists before plugin.register: %v", err)
	}

	rawYAML := []byte(`
enabled: true
concurrency:
  enabled: true
  global_limit: 6
  queue_timeout: 60s
  max_queue_per_key: 32
`)
	request, err := json.Marshal(LifecycleRequest{ConfigYAML: rawYAML})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.HandleMethod(MethodPluginRegister, request); err != nil {
		t.Fatalf("plugin.register: %v", err)
	}

	want := policy.ConcurrencyConfig{
		Enabled:        true,
		GlobalLimit:    6,
		QueueTimeout:   policy.Duration(60 * time.Second),
		MaxQueuePerKey: 32,
	}
	if got := app.store.ConcurrencyConfig(); got != want {
		t.Fatalf("store concurrency = %+v, want %+v", got, want)
	}
	state, err := policy.LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState(%q): %v", statePath, err)
	}
	if state.Concurrency == nil || *state.Concurrency != want {
		t.Fatalf("persisted concurrency = %+v, want %+v", state.Concurrency, want)
	}
}

func requestInterceptResponse(t *testing.T, raw []byte) RequestInterceptResponse {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("intercept envelope error = %+v", env.Error)
	}
	var response RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func callIntercept(t *testing.T, app *App, requestID, plain string, metadata map[string]any) RequestInterceptResponse {
	t.Helper()
	headers := http.Header{}
	if plain != "" {
		headers.Set("Authorization", "Bearer "+plain)
	}
	req, _ := json.Marshal(RequestInterceptRequest{
		RequestID:    requestID,
		SourceFormat: "openai-response",
		Model:        "gpt-5-codex",
		Stream:       true,
		Headers:      headers,
		Metadata:     metadata,
	})
	raw, err := app.HandleMethod(MethodRequestInterceptBefore, req)
	if err != nil {
		t.Fatalf("request.intercept_before: %v", err)
	}
	return requestInterceptResponse(t, raw)
}

func callComplete(t *testing.T, app *App, requestID string) {
	t.Helper()
	req, _ := json.Marshal(RequestCompletion{RequestID: requestID, Outcome: "succeeded"})
	raw, err := app.HandleMethod(MethodRequestComplete, req)
	if err != nil {
		t.Fatalf("request.complete: %v", err)
	}
	if !okEnvelope(t, raw) {
		t.Fatalf("request.complete envelope = %s", raw)
	}
}

func TestRegistrationAdvertisesConcurrencyLifecycleCapabilities(t *testing.T) {
	registration := NewApp().registration()
	if !registration.Capabilities.RequestInterceptor || !registration.Capabilities.RequestLifecyclePlugin {
		t.Fatalf("capabilities = %+v, want request interceptor + lifecycle", registration.Capabilities)
	}
}

func TestRequestLifecycleAcquireAndRelease(t *testing.T) {
	app, plains := configureConcurrencyApp(t, true, 1, time.Second, 32)
	if response := callIntercept(t, app, "request-a", plains["team-a"], nil); response.Terminate {
		t.Fatalf("A unexpectedly terminated: %+v", response)
	}

	type interceptResult struct {
		response RequestInterceptResponse
	}
	bResult := make(chan interceptResult, 1)
	go func() {
		bResult <- interceptResult{response: callIntercept(t, app, "request-b", plains["team-b"], nil)}
	}()
	waitForAppConcurrencyStats(t, app, func(waiting int, running map[string]int) bool {
		return waiting == 1 && running["team-a"] == 1
	})

	callComplete(t, app, "request-a")
	select {
	case result := <-bResult:
		if result.response.Terminate {
			t.Fatalf("B unexpectedly terminated: %+v", result.response)
		}
	case <-time.After(time.Second):
		t.Fatal("B was not admitted after A completed")
	}
	waitForAppConcurrencyStats(t, app, func(waiting int, running map[string]int) bool {
		return waiting == 0 && running["team-b"] == 1
	})
	callComplete(t, app, "request-b")
	waitForAppConcurrencyStats(t, app, func(waiting int, running map[string]int) bool {
		return waiting == 0 && len(running) == 0
	})
}

func TestRequestQueueTimeoutReturnsOpenAI429(t *testing.T) {
	app, plains := configureConcurrencyApp(t, true, 1, 20*time.Millisecond, 32)
	_ = callIntercept(t, app, "request-a", plains["team-a"], nil)
	response := callIntercept(t, app, "request-b", plains["team-b"], nil)
	if !response.Terminate || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("timeout response = %+v, want terminating 429", response)
	}
	if response.ResponseHeaders.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", response.ResponseHeaders.Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.ResponseBody, &body); err != nil {
		t.Fatalf("timeout body = %s: %v", response.ResponseBody, err)
	}
	if body.Error.Type != "rate_limit_error" || body.Error.Code != "concurrency_queue_timeout" || body.Error.Message == "" {
		t.Fatalf("timeout body = %+v", body)
	}
	callComplete(t, app, "request-a")
}

func TestRequestQueueFullReturnsOpenAI429(t *testing.T) {
	app, plains := configureConcurrencyApp(t, true, 1, time.Second, 1)
	_ = callIntercept(t, app, "request-a", plains["team-a"], nil)
	firstWaiting := make(chan RequestInterceptResponse, 1)
	go func() {
		firstWaiting <- callIntercept(t, app, "request-b1", plains["team-b"], nil)
	}()
	waitForAppConcurrencyStats(t, app, func(waiting int, running map[string]int) bool {
		return waiting == 1 && running["team-a"] == 1
	})

	response := callIntercept(t, app, "request-b2", plains["team-b"], nil)
	if !response.Terminate || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("queue-full response = %+v, want terminating 429", response)
	}
	if response.ResponseHeaders.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", response.ResponseHeaders.Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.ResponseBody, &body); err != nil {
		t.Fatalf("queue-full body = %s: %v", response.ResponseBody, err)
	}
	if body.Error.Type != "rate_limit_error" || body.Error.Code != "concurrency_queue_full" {
		t.Fatalf("queue-full body = %+v", body)
	}

	callComplete(t, app, "request-b1")
	select {
	case queued := <-firstWaiting:
		if queued.Terminate {
			t.Fatalf("canceled first waiter returned a rejection: %+v", queued)
		}
	case <-time.After(time.Second):
		t.Fatal("first waiter was not canceled")
	}
	callComplete(t, app, "request-a")
}

func TestRequestCompletionCancelsQueuedAcquire(t *testing.T) {
	app, plains := configureConcurrencyApp(t, true, 1, time.Second, 32)
	_ = callIntercept(t, app, "request-a", plains["team-a"], nil)
	bResult := make(chan RequestInterceptResponse, 1)
	go func() { bResult <- callIntercept(t, app, "request-b", plains["team-b"], nil) }()
	waitForAppConcurrencyStats(t, app, func(waiting int, _ map[string]int) bool { return waiting == 1 })

	callComplete(t, app, "request-b")
	select {
	case response := <-bResult:
		if response.Terminate {
			t.Fatalf("client-canceled waiter returned a queue rejection: %+v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("request.complete did not cancel the queued ABI call")
	}
	waitForAppConcurrencyStats(t, app, func(waiting int, running map[string]int) bool {
		return waiting == 0 && running["team-a"] == 1
	})
	callComplete(t, app, "request-a")
}

func TestRequestCompletionBeforeInterceptDoesNotLeakSlot(t *testing.T) {
	app, plains := configureConcurrencyApp(t, true, 1, time.Second, 32)
	// guardedPluginClient may return on an already-canceled host context before
	// its goroutine enters the C ABI. The asynchronous completion can therefore
	// win the race with intercept registration.
	callComplete(t, app, "request-completed-first")
	response := callIntercept(t, app, "request-completed-first", plains["team-a"], nil)
	if response.Terminate {
		t.Fatalf("already-completed request returned a fabricated rejection: %+v", response)
	}
	waitForAppConcurrencyStats(t, app, func(waiting int, running map[string]int) bool {
		return waiting == 0 && len(running) == 0
	})
}

func TestDuplicateRequestCompletionReleasesExactlyOnce(t *testing.T) {
	app, plains := configureConcurrencyApp(t, true, 1, time.Second, 32)
	_ = callIntercept(t, app, "request-a", plains["team-a"], nil)
	callComplete(t, app, "request-a")
	callComplete(t, app, "request-a")
	waitForAppConcurrencyStats(t, app, func(waiting int, running map[string]int) bool {
		return waiting == 0 && len(running) == 0
	})
}

func TestUnknownKeyBypassesConcurrency(t *testing.T) {
	app, _ := configureConcurrencyApp(t, true, 1, time.Second, 32)
	response := callIntercept(t, app, "native-request", "native-cpa-key", nil)
	if response.Terminate {
		t.Fatalf("native key was terminated: %+v", response)
	}
	waitForAppConcurrencyStats(t, app, func(waiting int, running map[string]int) bool {
		return waiting == 0 && len(running) == 0
	})
}

func TestCallerScopeResolvesPrincipalWithoutPlainKey(t *testing.T) {
	app, _ := configureConcurrencyApp(t, true, 1, time.Second, 32)
	// SHA-256("cli-proxy-api:caller-scope:v1\\x00team-a"), derived
	// independently from CLIProxyAPI's documented CallerScope contract.
	const teamAScope = "07fd5bb88ccc0e9b46658eeadd4c0931be7daf2c5e02bc08e2c397558b01d0e7"
	response := callIntercept(t, app, "scoped-request", "", map[string]any{"caller_scope": teamAScope})
	if response.Terminate {
		t.Fatalf("caller_scope request terminated: %+v", response)
	}
	waitForAppConcurrencyStats(t, app, func(waiting int, running map[string]int) bool {
		return waiting == 0 && running["team-a"] == 1
	})
	callComplete(t, app, "scoped-request")
}

func TestMissingRequestIDFailsClosedWithoutLeakingSlot(t *testing.T) {
	app, plains := configureConcurrencyApp(t, true, 1, time.Second, 32)
	response := callIntercept(t, app, "", plains["team-a"], nil)
	if !response.Terminate || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("missing request id response = %+v, want terminating 503", response)
	}
	waitForAppConcurrencyStats(t, app, func(waiting int, running map[string]int) bool {
		return waiting == 0 && len(running) == 0
	})
}

func TestRequestInterceptAfterDoesNotDoubleAcquire(t *testing.T) {
	app, plains := configureConcurrencyApp(t, true, 1, time.Second, 32)
	_ = callIntercept(t, app, "request-a", plains["team-a"], nil)
	req, _ := json.Marshal(RequestInterceptRequest{RequestID: "request-a"})
	raw, err := app.HandleMethod(MethodRequestInterceptAfter, req)
	if err != nil {
		t.Fatalf("request.intercept_after: %v", err)
	}
	if response := requestInterceptResponse(t, raw); response.Terminate {
		t.Fatalf("after-auth interceptor terminated: %+v", response)
	}
	waitForAppConcurrencyStats(t, app, func(waiting int, running map[string]int) bool {
		return waiting == 0 && running["team-a"] == 1
	})
	callComplete(t, app, "request-a")
}

func TestConcurrencyManagementRoutesAreRegistered(t *testing.T) {
	routes := NewApp().managementRegistration().Routes
	want := map[string]bool{
		http.MethodGet + " /plugins/" + PluginID + "/concurrency": false,
		http.MethodPut + " /plugins/" + PluginID + "/concurrency": false,
	}
	for _, route := range routes {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Fatalf("management route %q was not registered", route)
		}
	}
}

func TestConcurrencyManagementGetReturnsConfigAndSafeRuntimeStatus(t *testing.T) {
	app, plains := configureConcurrencyApp(t, true, 1, time.Second, 32)
	_ = callIntercept(t, app, "request-a", plains["team-a"], nil)

	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/cpa-key-policy/concurrency",
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, resp.Body)
	}
	var payload struct {
		Config policy.ConcurrencyConfig `json:"config"`
		Status struct {
			Enabled          bool `json:"enabled"`
			GlobalLimit      int  `json:"global_limit"`
			GlobalRunning    int  `json:"global_running"`
			TotalWaiting     int  `json:"total_waiting"`
			ActivePrincipals int  `json:"active_principals"`
			Principals       []struct {
				Principal  string `json:"principal"`
				KeyID      string `json:"key_id"`
				KeyName    string `json:"key_name"`
				KeyPreview string `json:"key_preview"`
				Running    int    `json:"running"`
				Waiting    int    `json:"waiting"`
			} `json:"principals"`
		} `json:"status"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, resp.Body)
	}
	if !payload.Config.Enabled || payload.Config.GlobalLimit != 1 {
		t.Fatalf("config = %+v", payload.Config)
	}
	if !payload.Status.Enabled || payload.Status.GlobalRunning != 1 || payload.Status.ActivePrincipals != 1 {
		t.Fatalf("status = %+v", payload.Status)
	}
	if len(payload.Status.Principals) != 1 {
		t.Fatalf("principals = %+v", payload.Status.Principals)
	}
	principal := payload.Status.Principals[0]
	if principal.Principal != "team-a" || principal.KeyID != "team-a" || principal.KeyName != "Name team-a" ||
		principal.KeyPreview != policy.PreviewKey(plains["team-a"]) || principal.Running != 1 {
		t.Fatalf("principal status = %+v", principal)
	}
	for _, plain := range plains {
		if bytesContain(resp.Body, []byte(plain)) {
			t.Fatalf("management status leaked plaintext key %q: %s", plain, resp.Body)
		}
	}
	callComplete(t, app, "request-a")
}

func TestConcurrencyManagementPutPersistsAndHotReconfigures(t *testing.T) {
	app, _ := configureConcurrencyApp(t, false, 6, time.Minute, 32)
	want := policy.ConcurrencyConfig{
		Enabled:        true,
		GlobalLimit:    2,
		QueueTimeout:   policy.Duration(1500 * time.Millisecond),
		MaxQueuePerKey: 7,
	}
	body, _ := json.Marshal(want)
	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/cpa-key-policy/concurrency",
		Body:   body,
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, resp.Body)
	}
	if got := app.store.ConcurrencyConfig(); got != want {
		t.Fatalf("store config = %+v, want %+v", got, want)
	}
	stats := app.concurrencyLimiter.Stats()
	if !stats.Enabled || stats.GlobalLimit != want.GlobalLimit {
		t.Fatalf("runtime stats = %+v", stats)
	}
	state, err := policy.LoadState(app.store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if state.Concurrency == nil || *state.Concurrency != want {
		t.Fatalf("persisted config = %+v, want %+v", state.Concurrency, want)
	}
}

func TestConcurrencyManagementPutRejectsInvalidConfig(t *testing.T) {
	app, _ := configureConcurrencyApp(t, true, 3, time.Second, 8)
	before := app.store.ConcurrencyConfig()
	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/cpa-key-policy/concurrency",
		Body:   []byte(`{"enabled":true,"global_limit":0,"queue_timeout":"1s","max_queue_per_key":8}`),
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", resp.StatusCode, resp.Body)
	}
	if got := app.store.ConcurrencyConfig(); got != before {
		t.Fatalf("invalid update changed store config: got %+v want %+v", got, before)
	}
	stats := app.concurrencyLimiter.Stats()
	if stats.GlobalLimit != before.GlobalLimit {
		t.Fatalf("invalid update changed runtime stats: %+v", stats)
	}
}

func TestConcurrencyManagementPutReportsPersistenceFailure(t *testing.T) {
	app, _ := configureConcurrencyApp(t, false, 6, time.Minute, 32)
	before := app.store.ConcurrencyConfig()
	statePath := app.store.StatePath()
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/cpa-key-policy/concurrency",
		Body:   []byte(`{"enabled":true,"global_limit":2,"queue_timeout":"1s","max_queue_per_key":8}`),
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", resp.StatusCode, resp.Body)
	}
	if app.concurrencyLimiter.Stats().Enabled {
		t.Fatal("runtime limiter changed despite persistence failure")
	}
	if got := app.store.ConcurrencyConfig(); got != before {
		t.Fatalf("store config changed despite persistence failure: got %+v want %+v", got, before)
	}
}

func TestEarlyCompletionTombstonesAreBounded(t *testing.T) {
	app, _ := configureConcurrencyApp(t, true, 1, time.Second, 32)
	for index := 0; index < maxEarlyConcurrencyCompletions+128; index++ {
		app.finishConcurrencyRequest("early-" + strconv.Itoa(index))
	}
	app.concurrencyRequestMu.Lock()
	defer app.concurrencyRequestMu.Unlock()
	if len(app.concurrencyCompleted) > maxEarlyConcurrencyCompletions || len(app.concurrencyOrder) > maxEarlyConcurrencyCompletions {
		t.Fatalf("early-completion memory is unbounded: completed=%d order=%d", len(app.concurrencyCompleted), len(app.concurrencyOrder))
	}
}

func bytesContain(value, part []byte) bool {
	if len(part) == 0 || len(part) > len(value) {
		return false
	}
	for index := 0; index+len(part) <= len(value); index++ {
		matched := true
		for offset := range part {
			if value[index+offset] != part[offset] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func waitForAppConcurrencyStats(t *testing.T, app *App, predicate func(waiting int, running map[string]int) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := app.concurrencyLimiter.Stats()
		running := make(map[string]int)
		for _, principal := range stats.Principals {
			if principal.Running > 0 {
				running[principal.Principal] = principal.Running
			}
		}
		if predicate(stats.TotalWaiting, running) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	stats := app.concurrencyLimiter.Stats()
	t.Fatalf("concurrency stats predicate timed out: %+v", stats)
}
