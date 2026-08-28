# Dynamic Fair Concurrency Limiter Design

## Scope

Add an opt-in, process-local global concurrency limit shared fairly by downstream keys managed by `cpa-key-policy`. The first version covers inference execution only: dynamic borrowing of idle capacity, per-principal FIFO queues, queue timeout/cancellation, per-principal queue bounds, configuration, runtime status, WebUI, tests, and documentation.

It does not add RPM/TPM/token scheduling, weights, preemption, distributed coordination, or changes to CLIProxyAPI itself.

## Verified host contract

The current CLIProxyAPI plugin ABI exposes:

- `request.intercept_before`: synchronous request admission before upstream credential selection. It is reached from real model execution paths, not management/static/health routes.
- `request.complete`: one asynchronous terminal notification for every execution that reached request interception. Outcomes cover success, failure, interceptor rejection, request cancellation, and downstream disconnect.
- A stable `RequestID` shared by both calls.
- Execution metadata `caller_scope`, an irreversible SHA-256 namespace derived from the authenticated frontend `Principal`.

The host's lifecycle tracker completes after a regular response, stream EOF/error, canceled stream, or WebSocket-backed model execution. The C ABI does not pass a cancellable Go context into plugin code. When a client cancels while a plugin call is blocked, the host abandons that call and emits `request.complete` concurrently; the plugin therefore uses `RequestID` to cancel its waiting acquire.

## Configuration and persistence

```yaml
concurrency:
  enabled: false
  global_limit: 6
  queue_timeout: 60s
  max_queue_per_key: 32
```

Defaults preserve existing behavior: disabled, limit 6, timeout 60 seconds, queue bound 32. Enabling requires all numeric/duration values to be positive.

The configuration is part of both plugin YAML and the existing state JSON. On first boot YAML seeds state. Once state exists, its concurrency block is authoritative, matching the repository's existing WebUI-managed state model. Old state without the field gets disabled defaults.

## Fair limiter

`internal/concurrency` owns all scheduling state behind one mutex:

- `globalRunning` and a configured global limit.
- Per-principal `running` and FIFO waiter slices.
- Monotonic waiter and grant sequence numbers.

Acquire behavior:

1. Disabled mode returns an uncounted no-op release.
2. With an immediately free slot and no existing waiters, admit directly so one active principal can borrow all capacity.
3. Otherwise append to that principal's FIFO queue, subject to `max_queue_per_key`.
4. Each scheduling pass chooses a non-empty principal with the least `running` count. Ties prefer the least recently granted principal, then the oldest head waiter. It admits that principal's FIFO head. This prevents a principal with an older backlog from monopolizing a one-slot limiter.
5. Cancellation or deadline removes a still-waiting request immediately.

Release is a `sync.Once` closure. It decrements global and principal running counts exactly once, then schedules newly free slots. Lowering the limit never preempts running work; admissions resume only after running falls below the new limit. Disabling releases queued requests as uncounted bypasses while existing counted requests drain normally.

## Plugin lifecycle integration

`App` owns one limiter, a `RequestID -> requestLease` map, and a bounded recent-completion set for the rare case where an asynchronous completion reaches the plugin before the guarded C-ABI goroutine registers its request.

At `request.intercept_before`:

1. Ignore the call if the plugin/limiter is disabled or the request was not authenticated by one of this plugin's keys.
2. Resolve key ID from `caller_scope` by comparing it with the scope derived from configured key IDs; fall back to the downstream key in request headers for compatible hosts.
3. Consume an early-completion marker if completion already won the C-ABI scheduling race; otherwise register the request before blocking so later completion can cancel it.
4. Acquire with a context bounded by `queue_timeout`.
5. Return an OpenAI-compatible 429 termination with `Retry-After: 1` on timeout or queue overflow.
6. On admission, retain the exactly-once release until completion.

At `request.complete`, cancel a waiter or invoke its release. A completion racing with admission marks the request completed; the acquire path releases immediately if it wins after that mark. A completion arriving before registration is kept in a fixed-size RequestID-only tombstone set, preventing an admitted slot with no future release while keeping memory bounded. Duplicate completion is harmless.

If `RequestID` is missing while the limiter is enabled, reject before acquiring because no reliable terminal correlation exists; this avoids a silent permanent slot leak.

## Management API and WebUI

Add exact management routes:

- `GET /v0/management/plugins/cpa-key-policy/concurrency`
- `PUT /v0/management/plugins/cpa-key-policy/concurrency`

The response contains persisted config and a live snapshot. User rows contain key ID, key name, and existing masked preview only; never a plaintext key.

The WebUI adds a Concurrency Limiter page with enable, global limit, human-readable queue timeout, max queue per key, save controls, global running/waiting/active-user tiles, and a per-key table. It polls only while mounted and uses the existing Axios/session/i18n/style patterns.

## Compatibility and limitations

- Existing frontend auth, RPM, budget, model policy, routing, scheduler, response interception, usage billing, management routes, and state migration remain unchanged.
- Only plugin-key requests are tracked. Native CPA keys and unknown keys are ignored.
- The lifecycle is per model execution. For Responses WebSocket traffic, idle socket time is not counted; each model execution is counted until it finishes or disconnect cancellation completes it.
- This feature requires a CLIProxyAPI build that exposes RequestInterceptor, RequestLifecyclePlugin, `RequestID`, and `caller_scope`. Registration advertises those capabilities explicitly.

## Verification

Unit tests cover capacity borrowing, second-user preference, two/three-user convergence, FIFO, no starvation, timeout, cancellation, queue bounds, exactly-once release, disabled mode, concurrent acquire/release/cancel, persistence, management responses, lifecycle integration, and non-plugin-key bypass. Final verification runs Go tests, Go race tests, Web tests/build, and the repository's shared-library build path when the local C toolchain permits it.
