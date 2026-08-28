# cpa-key-policy

Downstream **API key policy** plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).

In plain words: the plugin can bind policies directly to the keys already present in CPA's top-level `api-keys` list, without issuing another set of `cpa_...` secrets. In `cpa-native` mode it becomes the exclusive frontend auth provider, so an unbound or policy-denied key cannot fall through and bypass the policy. Legacy plugin-owned keys remain available for compatible upgrades.

| | |
|---|---|
| **Repo** | [origin652/cpa-plugin-key-policy](https://github.com/origin652/cpa-plugin-key-policy) |
| **License** | MIT |
| **Install** | [CLIProxyAPI Plugins Store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store) or build from source |
| **中文说明** | [README.zh-CN.md](./README.zh-CN.md) |

---

## What it does (human version)

1. **Reuse CPA keys** — select existing top-level `api-keys` and bind model/alias policies without generating new secrets.
2. **Route** — client calls with alias name `fast`; plugin rewrites to e.g. `codex` + `gpt-5.4-mini`.
3. **Limit** — per-key RPM, optional daily/weekly USD caps, token or per-call billing, plus opt-in fair global concurrency.
4. **Isolate credentials (tiers / groups)** — pin a request to Codex free/team/… or to a **custom classify group** so it never lands on the wrong auth file.
5. **Multi-target aliases** — one alias can point at several backends (priority or round-robin).
6. **Web UI** — manage keys, mappings, credential classification, concurrency configuration, and runtime status inside CPA.

---

## Concepts

### Downstream keys and auth modes

- `auth_mode: cpa-native` (recommended): bind CPA's existing top-level `api-keys`. The plugin stores only SHA-256, a masked preview, and policy data. It generates and persists no plaintext key. Unbound keys are denied by exclusive authentication.
- `auth_mode: plugin` (compatibility default): retain the legacy plugin-issued `cpa_...` flow. Stage every native binding in this mode before switching.

Each binding holds:

- allowed **models** and/or **aliases**
- RPM
- optional daily / weekly dollar limits
- optional `allow_models_endpoint` (see below)

### Alias (global mapping table)

A reusable name like `fast` that expands to one or more **targets**:

| Field | Meaning |
|--------|---------|
| `provider` | CPA provider id (`codex`, `claude`, or an openai-compatibility **name** such as `cerebras`) |
| `target_model` | Real upstream model id |
| `group` | Optional credential filter (see [Credential groups](#credential-groups-tiers--classify)) |
| `dispatch` | `priority` (always first usable target) or `round-robin` |
| billing | `tokens` (per-million prices) or `per_call` (fixed USD) |

Keys can **reference** aliases instead of duplicating targets. Multi-target aliases expand to several rules with the same alias name; auth and routing share one pick per request so the `group` filter matches the chosen target.

### Credential groups (tiers + classify)

Two sources of “which auth file may serve this request”:

| Kind | How it appears in the picker | Stored in mapping as |
|------|------------------------------|----------------------|
| **Built-in tier** (Codex `plan_type`, Antigravity `tier`) | e.g. Free tier / Team | bare name: `free`, `team`, `supported` |
| **Custom classify rule** | e.g. **Custom · vip** | prefixed: `classify:vip` |

**Runtime rule:** if a mapping sets a group, the plugin scheduler **only** picks auth files in that group. No match → hard failure (`auth_not_found`), never silently fall back to another tier.

**Custom classification** (Web UI → Mapping → Credential Classification):

- Match auth-file fields (`filename`, `provider`, `plan_type`, `tier`, …) with a regex.
- Assign a **group name** you choose (stored bare on the rule).
- Catalog and mappings use `classify:<name>` so it never collides with built-in `free` / `team`.
- One file can match **multiple** custom groups (shown under each).
- If no custom rule matches → built-in tier (for Codex/Antigravity) or flat (no group) for other auth-file providers.
- OpenAI-compat / API-key channels stay **flat** (no groups).

Configure classify rules in the UI, or via management API (`/classify-rules`, `/classify-preview`, `/catalog`). You do not need to hand-edit state JSON for normal use.

### OpenAI-compatibility providers

Channels under CPA `openai-compatibility` (e.g. a named proxy) use the **channel name** as `provider`. The plugin maps it to CPA’s internal key `openai-compatible-<name>` when routing. Models must be listed on that channel in CPA config, or the host reports no auth for that model.

---

## Capabilities (plugin hooks)

| Hook | Role |
|------|------|
| Frontend auth | Know plugin keys; enforce alias allow-list, RPM, budget; stamp route + group metadata |
| Model router | Alias → provider + target model |
| Scheduler | When `group` is set, filter auth candidates by tier / `classify:` group |
| Request interceptor + lifecycle | Fair concurrency acquire before inference; exactly-once release on `request.complete` |
| Response interceptor | Non-stream JSON: rewrite top-level `model` back to the alias |
| Usage | Token / per-call billing into the state file |
| Management API + embedded Web UI | Keys, aliases, classify rules, status |

---

## Build

Linux `.so` needs cgo and a matching toolchain:

```bash
make test
make build-linux          # builds web UI, then linux amd64/arm64 .so
# or
make web-build
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -buildvcs=false -tags cshared \
  -buildmode=c-shared -o dist/cpa-key-policy_linux_amd64.so ./cmd/cpa-key-policy
```

On Windows, build the `.so` via WSL/Linux. `go test ./...` uses a non-cgo stub so unit tests run without a shared-library toolchain.

Copy the `.so` into CPA `plugins.dir` and enable the plugin in config.

---

## Config

Minimal shape (see also [`config.example.yaml`](./config.example.yaml)):

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-key-policy:
      enabled: true
      priority: 10
      auth_mode: plugin # bind every CPA key first, then switch to cpa-native
      state_file: "cpa-key-policy-state.json"
      concurrency:
        enabled: false
        global_limit: 6
        queue_timeout: 60s
        max_queue_per_key: 32
```

Notes:

- If `state_file` contains a `concurrency` block, that persisted value wins over YAML. Web UI/API changes are written there. Old state files without the block remain disabled unless YAML explicitly enables it.
- Missing `auth_mode` defaults to `plugin` for safe upgrades. Stage every binding before switching when possible. With zero bindings, `cpa-native` still loads exclusively and denies all inference requests while management routes remain available; it never fails open by dropping the exclusive provider.
- Keep the original keys in CPA's top-level `api-keys` list. CPA Usage Keeper syncs that list as metadata; successful native auth returns the original CPA key as its principal, so Keeper continues grouping usage by the same key without changes.
- The current CPA plugin ABI does not publish top-level key changes to plugins. When deleting or rotating a CPA key, also remove its old policy binding and bind the replacement; the WebUI flags bindings no longer present in CPA's live list.
- Concurrency is **disabled by default**, so upgrading does not change existing request behavior.
- Prefer creating keys and aliases in the **Web UI** or Management API; seed YAML `keys` is mainly for first boot.
- Never commit real key hashes, management secrets, or live host URLs into public docs.

---

## Dynamic Fair Concurrency Limiter

This optional, process-local limiter caps running inference requests globally while lending all idle capacity to whoever is active. With `global_limit: 6`:

| Active downstream keys | Steady-state allocation while all keep waiting |
|------------------------|------------------------------------------------|
| A only | A = 6 |
| A + B | A ≈ 3, B ≈ 3 |
| A + B + C | A ≈ 2, B ≈ 2, C ≈ 2 |

Scheduling is non-preemptive. Existing requests are never interrupted when another user arrives or the limit is lowered. Each newly freed slot goes to a waiting principal with the lowest running count; ties use least-recently-granted principal and then oldest per-principal FIFO head. A user with a large queued backlog therefore cannot starve later users, and a lone user can borrow the full limit again as others become idle.

When all slots are busy, requests queue instead of immediately failing:

- `queue_timeout` expiry returns OpenAI-compatible HTTP 429 with code `concurrency_queue_timeout` and `Retry-After: 1`.
- Exceeding `max_queue_per_key` returns HTTP 429 with code `concurrency_queue_full`.
- Client cancellation removes a queued request immediately.
- Disabling the limiter lets queued requests continue without counting; already counted requests drain naturally.

The limiter runs at CPA's inference execution hook, so Management API, health/static resources, login/config and usage queries do not enter it. Identity uses CPA's irreversible authenticated `caller_scope` first, with a transient downstream-key lookup fallback. Runtime status exposes only key ID/name/masked preview—never a plaintext key or authorization header.

Lifecycle coverage in the inspected current CLIProxyAPI includes normal HTTP, streaming EOF/error, cancellation/disconnect and WebSocket-backed model executions through `request.complete`. A slot represents one host request execution (not an independently distributed quota or per-message WebSocket counter). This limiter is in-memory per CPA process; it does not coordinate multiple CPA instances. A future host transport that does not emit the advertised lifecycle completion cannot be observed by a plugin, so use a CLIProxyAPI version providing both `request_interceptor` and `request_lifecycle_plugin`.

---

## Web Management UI

Embedded in the plugin. After load, open:

```text
http://<your-cpa-host>:<api-port>/v0/resource/plugins/cpa-key-policy/index.html
```

Login with CPA **management** secret (`remote-management.secret-key` / management password). The secret stays in memory only (not `localStorage`); refresh → re-login.

UI areas:

| Tab / page | Use for |
|------------|---------|
| Keys | Bind existing CPA keys; edit/delete policies; bind models or aliases; RPM & budgets |
| Mapping → Aliases | Global multi-target aliases, dispatch, pricing |
| Mapping → Classification | Custom credential groups + match preview |
| Model picker | Catalog of providers; tier / **Custom · …** subgroups |
| Concurrency | Enable/configure the limiter; view running, waiting and safe per-key status (3-second polling) |

Dev UI without rebuilding the `.so`:

```bash
cd web
npm install
VITE_CPA_BASE=http://127.0.0.1:8317 npm run dev
```

---

## Management API (summary)

Exact paths (no path templates). Auth: CPA management bearer token.

**Keys**

- `GET/PATCH/DELETE …/keys` (`id` in query or body for mutate)
- `POST …/native-keys/bind` — bind one existing CPA key without persisting plaintext
- Legacy `plugin` mode only: `POST …/keys` and `POST …/keys/rotate?id=…`
- `POST …/keys/reset-rpm?id=…`
- `GET …/keys/usage?id=…`
- `GET …/status`

**Concurrency**

- `GET …/concurrency` — `{config, status}` with global and safe per-key runtime counters
- `PUT …/concurrency` — validate, persist and hot-reconfigure the complete config

**Aliases**

- `GET/POST/DELETE …/aliases`

**Classify**

- `GET/POST/DELETE …/classify-rules`
- `POST …/classify-rules/reorder`
- `POST …/classify-preview` — group → credential ids (UI preview; bare group names)
- `POST …/catalog` — body: auth-file credentials + models; response: picker `entries` with `classify:` groups

Bind an existing CPA key (plaintext exists only in this management request):

```bash
curl -X POST "$CPA/v0/management/plugins/cpa-key-policy/native-keys/bind" \
  -H "Authorization: Bearer $MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "key": "'$CPA_API_KEY'",
    "id": "team-a-policy",
    "name": "Team A",
    "rpm": 60,
    "models": [
      {"alias":"fast","provider":"codex","target_model":"gpt-5.4-mini","group":"free"}
    ]
  }'
```

Create a multi-target alias:

```bash
curl -X POST "$CPA/v0/management/plugins/cpa-key-policy/aliases" \
  -H "Authorization: Bearer $MANAGEMENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "alias": "cheap-chat",
    "dispatch": "priority",
    "billing_mode": "tokens",
    "targets": [
      {"provider":"cerebras","target_model":"gpt-oss-120b"},
      {"provider":"codex","target_model":"gpt-5.4-mini","group":"free"}
    ]
  }'
```

---

## Client request behavior

| Case | Result |
|------|--------|
| Known key + allowed alias | Auth OK → route → optional group filter → upstream |
| Known key + unknown model | Auth rejected |
| RPM / budget exceeded | Rejected |
| Concurrency full | Fair queue; 429 only on timeout or per-key queue overflow |
| Group set, no matching auth file | `auth_not_found` / unavailable (no cross-tier leak) |
| Unbound/disabled/policy-denied key in `cpa-native` | Authentication fails; exclusive auth prevents native-provider fallback |
| Unknown key in legacy `plugin` mode | Plugin declines and CPA may try another provider |
| Non-stream chat response | Top-level `model` rewritten to alias |
| Stream | Body not rewritten (v1) |

### `/v1/models` on CPA main port

Per-key `allow_models_endpoint`: **binary** — deny (401) or full global list. CPA cannot filter that list per plugin key on the main port.


---

## Setup checklist

1. Build / install the `.so` into CPA `plugins.dir`.
2. Enable `plugins` + `cpa-key-policy` in CPA config; set `state_file`.
3. Open the Web UI with the management secret.
4. (Optional) Define **classify rules** if you need custom credential buckets.
5. While still in `auth_mode: plugin`, bind every existing CPA key and assign models/policies.
6. Confirm all bindings are enabled, then switch to `auth_mode: cpa-native` and reload/restart CPA.
7. Clients keep their original CPA keys: OpenAI-compatible base URL = CPA; `Authorization: Bearer <existing CPA key>`; `model` = alias name.
8. Ensure openai-compat channels list the models you map; empty model lists → host “no auth” errors.

---

## Tests

```bash
go test ./...
cd web && npm test && npm run build
```
