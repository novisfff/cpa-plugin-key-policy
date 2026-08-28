package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	fairconcurrency "cpa-key-policy/internal/concurrency"
	"cpa-key-policy/internal/policy"
)

const callerScopeMetadataKey = "caller_scope"

// Completion callbacks are asynchronous in CLIProxyAPI. A canceled host call
// can notify request.complete just before the guarded C-ABI goroutine enters
// request.intercept_before. Keep a bounded set so that ordering cannot create
// an admitted request with no later completion. Request IDs are random host
// correlation IDs and contain no credentials.
const maxEarlyConcurrencyCompletions = 4096

type trackedConcurrencyRequest struct {
	cancel    context.CancelFunc
	release   func()
	completed bool
}

type concurrencyPrincipalStatus struct {
	Principal  string `json:"principal"`
	KeyID      string `json:"key_id"`
	KeyName    string `json:"key_name,omitempty"`
	KeyPreview string `json:"key_preview,omitempty"`
	Running    int    `json:"running"`
	Waiting    int    `json:"waiting"`
}

type concurrencyRuntimeStatus struct {
	Enabled          bool                         `json:"enabled"`
	GlobalLimit      int                          `json:"global_limit"`
	GlobalRunning    int                          `json:"global_running"`
	TotalWaiting     int                          `json:"total_waiting"`
	ActivePrincipals int                          `json:"active_principals"`
	Principals       []concurrencyPrincipalStatus `json:"principals"`
}

type concurrencyManagementPayload struct {
	Config policy.ConcurrencyConfig `json:"config"`
	Status concurrencyRuntimeStatus `json:"status"`
}

func newConcurrencyLimiter(config policy.ConcurrencyConfig) *fairconcurrency.Limiter {
	limiter, err := fairconcurrency.New(limiterConfig(config))
	if err == nil {
		return limiter
	}
	// DefaultConcurrencyConfig is always valid. Keep construction total even if
	// a future caller passes a malformed config before policy normalization.
	limiter, _ = fairconcurrency.New(fairconcurrency.Config{Enabled: false})
	return limiter
}

func limiterConfig(config policy.ConcurrencyConfig) fairconcurrency.Config {
	return fairconcurrency.Config{
		Enabled:              config.Enabled,
		GlobalLimit:          config.GlobalLimit,
		MaxQueuePerPrincipal: config.MaxQueuePerKey,
	}
}

func (a *App) reconfigureConcurrency(config policy.ConcurrencyConfig) error {
	if a.concurrencyLimiter == nil {
		a.concurrencyLimiter = newConcurrencyLimiter(config)
		logConcurrencyConfig(config)
		return nil
	}
	if err := a.concurrencyLimiter.Reconfigure(limiterConfig(config)); err != nil {
		return err
	}
	logConcurrencyConfig(config)
	return nil
}

func logConcurrencyConfig(config policy.ConcurrencyConfig) {
	if !config.Enabled {
		return
	}
	// Configuration changes are low-frequency. Request queue/admission events
	// intentionally are not logged at INFO to avoid noisy hot-path output.
	log.Printf(
		"cpa-key-policy: fair limiter enabled global_limit=%d queue_timeout=%s max_queue_per_key=%d",
		config.GlobalLimit,
		config.QueueTimeout.String(),
		config.MaxQueuePerKey,
	)
}

func (a *App) concurrencyManagementPayload() concurrencyManagementPayload {
	return concurrencyManagementPayload{
		Config: a.store.ConcurrencyConfig(),
		Status: a.concurrencyRuntimeStatus(),
	}
}

func (a *App) concurrencyRuntimeStatus() concurrencyRuntimeStatus {
	stats := a.concurrencyLimiter.Stats()
	keys := a.store.Keys()
	byID := make(map[string]policy.KeyConfig, len(keys))
	for _, key := range keys {
		byID[key.ID] = key
	}
	status := concurrencyRuntimeStatus{
		Enabled:          stats.Enabled,
		GlobalLimit:      stats.GlobalLimit,
		GlobalRunning:    stats.GlobalRunning,
		TotalWaiting:     stats.TotalWaiting,
		ActivePrincipals: stats.ActivePrincipals,
		Principals:       make([]concurrencyPrincipalStatus, 0, len(stats.Principals)),
	}
	for _, principal := range stats.Principals {
		entry := concurrencyPrincipalStatus{
			Principal: principal.Principal,
			KeyID:     principal.Principal,
			Running:   principal.Running,
			Waiting:   principal.Waiting,
		}
		if key, ok := byID[principal.Principal]; ok {
			entry.KeyName = key.Name
			entry.KeyPreview = key.KeyPreview
		}
		status.Principals = append(status.Principals, entry)
	}
	return status
}

func (a *App) updateConcurrency(body []byte) ManagementResponse {
	var config policy.ConcurrencyConfig
	if err := json.Unmarshal(body, &config); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	if err := policy.ValidateConcurrencyConfig(config); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_concurrency_config", err.Error())
	}
	if err := a.store.UpdateConcurrencyConfig(config); err != nil {
		return jsonError(http.StatusInternalServerError, "concurrency_persistence_failed", err.Error())
	}
	if err := a.reconfigureConcurrency(a.store.ConcurrencyConfig()); err != nil {
		return jsonError(http.StatusInternalServerError, "concurrency_reconfigure_failed", err.Error())
	}
	return jsonResponse(http.StatusOK, a.concurrencyManagementPayload())
}

func (a *App) interceptConcurrency(raw []byte) ([]byte, error) {
	var req RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	config := a.store.ConcurrencyConfig()
	if !config.Enabled {
		return OKEnvelope(RequestInterceptResponse{})
	}
	principal := a.concurrencyPrincipal(req)
	if principal == "" {
		// Request interceptors run for every inference request in the host. Native
		// CPA keys and keys owned by other auth providers must remain untouched.
		return OKEnvelope(RequestInterceptResponse{})
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		return OKEnvelope(concurrencyErrorResponse(
			http.StatusServiceUnavailable,
			"Concurrency limiter cannot safely track this request lifecycle",
			"concurrency_lifecycle_unavailable",
			false,
		))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.QueueTimeout))
	tracked := &trackedConcurrencyRequest{cancel: cancel}
	a.concurrencyRequestMu.Lock()
	if _, completed := a.concurrencyCompleted[requestID]; completed {
		a.consumeEarlyConcurrencyCompletionLocked(requestID)
		a.concurrencyRequestMu.Unlock()
		cancel()
		return OKEnvelope(RequestInterceptResponse{})
	}
	if _, exists := a.concurrencyRequests[requestID]; exists {
		a.concurrencyRequestMu.Unlock()
		cancel()
		return OKEnvelope(concurrencyErrorResponse(
			http.StatusServiceUnavailable,
			"Concurrency limiter received a duplicate request identifier",
			"concurrency_lifecycle_conflict",
			false,
		))
	}
	a.concurrencyRequests[requestID] = tracked
	a.concurrencyRequestMu.Unlock()

	release, errAcquire := a.concurrencyLimiter.Acquire(ctx, principal)
	cancel() // release the deadline timer; completion cancellation is idempotent.

	a.concurrencyRequestMu.Lock()
	if tracked.completed {
		delete(a.concurrencyRequests, requestID)
		a.concurrencyRequestMu.Unlock()
		if errAcquire == nil && release != nil {
			release()
		}
		// The host has already classified this execution as canceled/completed.
		// Its guarded ABI call ignores this response, so do not fabricate a 429.
		return OKEnvelope(RequestInterceptResponse{})
	}
	if errAcquire != nil {
		delete(a.concurrencyRequests, requestID)
		a.concurrencyRequestMu.Unlock()
		switch {
		case errors.Is(errAcquire, context.DeadlineExceeded):
			return OKEnvelope(concurrencyErrorResponse(
				http.StatusTooManyRequests,
				"Concurrency limit reached, request queue timeout",
				"concurrency_queue_timeout",
				true,
			))
		case errors.Is(errAcquire, fairconcurrency.ErrQueueFull):
			return OKEnvelope(concurrencyErrorResponse(
				http.StatusTooManyRequests,
				"Concurrency limit reached, request queue is full for this key",
				"concurrency_queue_full",
				true,
			))
		case errors.Is(errAcquire, context.Canceled):
			return OKEnvelope(RequestInterceptResponse{})
		default:
			return OKEnvelope(concurrencyErrorResponse(
				http.StatusServiceUnavailable,
				"Concurrency limiter could not admit this request",
				"concurrency_admission_failed",
				false,
			))
		}
	}
	tracked.cancel = nil
	tracked.release = release
	a.concurrencyRequestMu.Unlock()
	return OKEnvelope(RequestInterceptResponse{})
}

func (a *App) completeConcurrency(raw []byte) ([]byte, error) {
	var completion RequestCompletion
	if err := json.Unmarshal(raw, &completion); err != nil {
		return nil, err
	}
	a.finishConcurrencyRequest(strings.TrimSpace(completion.RequestID))
	return OKEnvelope(struct{}{})
}

func (a *App) finishConcurrencyRequest(requestID string) {
	if requestID == "" {
		return
	}
	a.concurrencyRequestMu.Lock()
	tracked := a.concurrencyRequests[requestID]
	if tracked == nil {
		if a.store.ConcurrencyConfig().Enabled {
			a.rememberEarlyConcurrencyCompletionLocked(requestID)
		}
		a.concurrencyRequestMu.Unlock()
		return
	}
	if tracked.completed {
		a.concurrencyRequestMu.Unlock()
		return
	}
	tracked.completed = true
	cancel := tracked.cancel
	release := tracked.release
	if release != nil {
		delete(a.concurrencyRequests, requestID)
	}
	a.concurrencyRequestMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if release != nil {
		release()
	}
}

func (a *App) rememberEarlyConcurrencyCompletionLocked(requestID string) {
	if requestID == "" {
		return
	}
	if _, exists := a.concurrencyCompleted[requestID]; exists {
		return
	}
	a.concurrencyCompleted[requestID] = struct{}{}
	a.concurrencyOrder = append(a.concurrencyOrder, requestID)
	if len(a.concurrencyOrder) <= maxEarlyConcurrencyCompletions {
		return
	}
	oldest := a.concurrencyOrder[0]
	copy(a.concurrencyOrder, a.concurrencyOrder[1:])
	a.concurrencyOrder[len(a.concurrencyOrder)-1] = ""
	a.concurrencyOrder = a.concurrencyOrder[:len(a.concurrencyOrder)-1]
	delete(a.concurrencyCompleted, oldest)
}

func (a *App) consumeEarlyConcurrencyCompletionLocked(requestID string) {
	delete(a.concurrencyCompleted, requestID)
	for index, candidate := range a.concurrencyOrder {
		if candidate != requestID {
			continue
		}
		copy(a.concurrencyOrder[index:], a.concurrencyOrder[index+1:])
		a.concurrencyOrder[len(a.concurrencyOrder)-1] = ""
		a.concurrencyOrder = a.concurrencyOrder[:len(a.concurrencyOrder)-1]
		return
	}
}

func (a *App) cancelAllConcurrencyRequests() {
	a.concurrencyRequestMu.Lock()
	requests := make([]*trackedConcurrencyRequest, 0, len(a.concurrencyRequests))
	for requestID, tracked := range a.concurrencyRequests {
		if tracked == nil || tracked.completed {
			delete(a.concurrencyRequests, requestID)
			continue
		}
		tracked.completed = true
		requests = append(requests, tracked)
		delete(a.concurrencyRequests, requestID)
	}
	clear(a.concurrencyCompleted)
	a.concurrencyOrder = nil
	a.concurrencyRequestMu.Unlock()
	for _, tracked := range requests {
		if tracked.cancel != nil {
			tracked.cancel()
		}
		if tracked.release != nil {
			tracked.release()
		}
	}
}

func (a *App) concurrencyPrincipal(req RequestInterceptRequest) string {
	if !a.store.Enabled() {
		return ""
	}
	if scope := metadataString(req.Metadata, callerScopeMetadataKey); scope != "" {
		if principal, ok := a.store.PrincipalForCallerScope(scope); ok {
			return principal
		}
	}
	rawKey := policy.ExtractAPIKey(req.Headers, nil)
	if rawKey == "" {
		return ""
	}
	key := a.store.FindByAPIKey(rawKey)
	if key == nil || !key.Enabled {
		return ""
	}
	return key.ID
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func concurrencyErrorResponse(status int, message, code string, retry bool) RequestInterceptResponse {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    "rate_limit_error",
			"code":    code,
		},
	})
	headers := http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}
	if retry {
		headers.Set("Retry-After", "1")
	}
	return RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      status,
		ResponseHeaders: headers,
		ResponseBody:    body,
	}
}
