# Dynamic Fair Concurrency Limiter Implementation Plan

> Executed directly in the current session with red/green tests and final verification evidence.

**Goal:** Add an opt-in process-local global concurrency limiter that dynamically and fairly shares slots between authenticated downstream key principals.

**Architecture:** A standalone mutex-protected limiter performs least-running, per-principal FIFO scheduling and returns exactly-once releases. The plugin acquires in the verified `request.intercept_before` ABI hook and releases or cancels from the correlated `request.complete` lifecycle event; existing state persistence and WebUI management patterns carry configuration and status.

**Tech Stack:** Go 1.23, CLIProxyAPI c-shared JSON ABI, React 18, TypeScript, Axios, Vitest, Vite.

**Spec:** `docs/superpowers/specs/2026-08-28-dynamic-fair-concurrency-design.md`

## Global Constraints

- Concurrency is disabled by default and old YAML/state must preserve existing behavior.
- Fairness is non-preemptive and applies only to later slot admissions.
- Per-principal requests remain FIFO; one principal's backlog cannot starve another.
- No plaintext API key, Authorization header, or OAuth token may enter logs, state, or status.
- Only real model execution hooks are limited; management, WebUI resources, health, login, config, and usage queries remain unbounded.
- Do not add RPM/TPM/token scheduling, weights, Redis/database coordination, or CLIProxyAPI source changes.

---

### Task 1: Fair limiter core

**Files:**
- Create: `internal/concurrency/limiter.go`
- Create: `internal/concurrency/limiter_test.go`

**Interfaces:**
- Produces: `Config`, `Limiter`, `New(Config)`, `(*Limiter).Acquire(context.Context, string) (func(), error)`, `(*Limiter).Reconfigure(Config) error`, `(*Limiter).Stats() Stats`, `ErrQueueFull`, and validation errors.

- [x] **Step 1: Write failing behavior tests** for single-user borrowing, second-user released-slot preference, two/three-user convergence, per-user FIFO, no starvation, timeout/cancel removal, queue bound, once-only release, disabled mode, and concurrent race pressure.
- [x] **Step 2: Run the package tests** with `go test ./internal/concurrency`; expected failure is missing limiter symbols.
- [x] **Step 3: Implement the minimal scheduler** with one mutex, per-principal FIFO slices, least-running selection, least-recently-granted/oldest-head tie-breaking, direct admission, and `sync.Once` release.
- [x] **Step 4: Run and refactor** until `go test ./internal/concurrency` passes; keep all state mutation under the limiter mutex.

### Task 2: Configuration and state persistence

**Files:**
- Modify: `internal/policy/config.go`
- Modify: `internal/policy/store.go`
- Modify: `internal/policy/store_test.go`
- Modify: `internal/policy/mapping_test.go`

**Interfaces:**
- Produces: `policy.ConcurrencyConfig`, `DefaultConcurrencyConfig()`, `Store.ConcurrencyConfig()`, and `Store.UpdateConcurrencyConfig(ConcurrencyConfig) error`.
- Persists: optional `State.Concurrency *ConcurrencyConfig` while old state decodes to disabled defaults.

- [x] **Step 1: Add failing tests** for YAML defaults/validation, old-state compatibility, state round-trip, state authority on reload, and update persistence.
- [x] **Step 2: Run focused policy tests** and confirm failures are due to absent configuration behavior.
- [x] **Step 3: Add schema, normalization, store snapshot/update, and SaveState plumbing** without changing existing key/alias/rule/usage semantics.
- [x] **Step 4: Run `go test ./internal/policy`** and fix regressions before continuing.

### Task 3: Request admission and terminal release

**Files:**
- Modify: `internal/plugin/types.go`
- Modify: `internal/plugin/app.go`
- Create: `internal/plugin/concurrency.go`
- Create: `internal/plugin/concurrency_test.go`

**Interfaces:**
- Advertises: `request_interceptor` and `request_lifecycle_plugin`.
- Handles: ABI methods `request.intercept_before`, `request.intercept_after`, and `request.complete`.
- Uses: `RequestInterceptRequest.RequestID`, `Metadata["caller_scope"]`, and `RequestCompletion.RequestID`.

- [x] **Step 1: Add failing integration tests** proving capability registration, managed-key admission/queueing/release, queue timeout/full 429 shape/header, completion cancellation, duplicate/early completion safety, unknown/native key bypass, caller-scope lookup, and missing RequestID fail-closed behavior.
- [x] **Step 2: Run focused plugin tests** and confirm failures are missing ABI handling.
- [x] **Step 3: Implement request tracking** with race-safe completion/admission coordination, bounded early-completion tombstones, and no plaintext-key retention.
- [x] **Step 4: Run `go test ./internal/plugin ./internal/concurrency`** until all lifecycle and limiter tests pass.

### Task 4: Management API and WebUI

**Files:**
- Modify: `internal/plugin/app.go`
- Modify: `internal/plugin/app_test.go`
- Create: `web/src/api/concurrency.ts`
- Create: `web/src/api/concurrency.test.ts`
- Create: `web/src/pages/Concurrency.tsx`
- Modify: `web/src/App.tsx`
- Modify: `web/src/types.ts`
- Modify: `web/src/styles.css`
- Modify: `web/src/i18n/locales/en.json`
- Modify: `web/src/i18n/locales/zh-CN.json`
- Modify: `web/src/i18n/locales/zh-TW.json`
- Modify: `web/src/i18n/locales/ru.json`

**Interfaces:**
- Produces: `GET/PUT .../concurrency` returning `{config, status}` with masked user rows.
- Web API: `fetchConcurrency()` and `updateConcurrency(config)`.

- [x] **Step 1: Add failing Go/API and TypeScript tests** for configuration/status response shape and update payload.
- [x] **Step 2: Implement exact management routes** and merge limiter stats with key ID/name/preview display metadata.
- [x] **Step 3: Implement the React page** with existing controls/styles, safe numeric validation, save, and mounted polling.
- [x] **Step 4: Run Web tests and build**; confirm TypeScript and the single-file bundle succeed.

### Task 5: Documentation and full verification

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `config.example.yaml`
- Modify: `internal/plugin/web/dist/index.html` via existing `make web-build`

- [x] **Step 1: Document** configuration, dynamic borrowing, 6→3/3→2/2/2 examples, non-preemption, queue errors, WebUI/status, state authority, WebSocket execution granularity, and required host lifecycle capability.
- [x] **Step 2: Build the embedded WebUI** with `make web-build` (or the equivalent commands when Make cannot find the bundled runtime).
- [x] **Step 3: Run fresh verification**: `go test ./...`, `go test -race ./...`, Web `npm test`, Web `npm run build`, and the repository's c-shared build command supported by the host toolchain.
- [x] **Step 4: Re-read the spec and diff** to verify every requested behavior, compatibility constraint, security constraint, and final reporting item has evidence.
