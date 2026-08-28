# CPA Native API Keys Design

## Goal

Add a `cpa-native` authentication mode in which `cpa-key-policy` applies its
model, RPM, budget, routing, scheduling, and concurrency policies directly to
the keys already configured in CLIProxyAPI's top-level `api-keys` list. The
plugin must not generate a second `cpa_...` secret in this mode, policy denial
must not fall through to CPA's built-in key provider, and CPA Usage Keeper must
continue to identify usage by the original CPA key value.

The existing plugin-owned-key behavior remains available as `plugin` mode for
backward compatibility. Existing installations default to `plugin`; this
server will explicitly migrate to `cpa-native` after all three native keys have
policies staged.

## Verified host and Keeper contracts

The current CLIProxyAPI host evaluates frontend authentication providers in
order. A plugin response with `Authenticated: false` becomes `NotHandled`, so
the built-in `api-keys` provider can otherwise accept the same key. The plugin
ABI exposes `frontend_auth_provider_exclusive`; when advertised, CPA restricts
the active provider registry to the selected plugin provider. This is the
mechanism that prevents policy denial from falling through.

On successful authentication CPA stores the plugin response's `Principal` as
`userApiKey`. It subsequently derives `caller_scope` from that principal and
uses the same principal as the usage record's `api_key` identity.

CPA Usage Keeper independently reads the original top-level keys from
`GET /v0/management/api-keys`, stores those values as its key identities, and
groups usage events by their `api_key` value. Returning an internal policy ID
as the principal therefore breaks Keeper's key-level association; returning
the original CPA key preserves it without modifying Keeper.

The lifecycle config passed to a plugin contains only that plugin instance's
configuration, not the top-level CPA `api-keys` list. The plugin backend cannot
continuously validate membership in that list without a CPA ABI change or a
management credential. The WebUI can read the list directly because it already
runs as an authenticated CPA management client.

## Chosen approach

Use an opt-in native mode with exclusive frontend authentication and hashed
native-key bindings. This keeps the change inside the plugin and works with the
current CPA host and Keeper.

Two alternatives were rejected:

- A non-exclusive plugin provider is unsafe because every plugin policy denial
  can fall through to CPA's built-in key provider.
- A post-authentication policy hook in CPA would make the top-level key list the
  single live source of truth, but the current ABI has no such hook. Adding one
  requires carrying and maintaining a CPA fork, which is outside this plugin
  change.

## Configuration and compatibility

Add a validated configuration field:

```yaml
auth_mode: plugin # plugin | cpa-native
```

Missing or empty `auth_mode` normalizes to `plugin`. This preserves existing
state files, deployments, and tests. `cpa-native` is an exact, case-insensitive
accepted value; other values fail configuration.

Registration is configuration-dependent:

- `plugin`: `frontend_auth_provider_exclusive: false`, preserving legacy
  provider fallthrough.
- `cpa-native`: `frontend_auth_provider_exclusive: true`, making this plugin
  the only request authentication provider.

The mode is returned by the management status API and described in registration
metadata. A host reconfigure or restart re-registers the capability.

Each persisted key policy gains:

```yaml
key_source: plugin       # plugin | cpa-native
caller_scope: "..."     # irreversible scope, populated for native bindings
```

Old entries without `key_source` normalize to `plugin`. Plugin mode authenticates
only `plugin` entries; native mode authenticates only `cpa-native` entries. This
prevents old generated secrets from remaining valid after a native-mode switch.

Both modes continue to persist only SHA-256 key hashes, masked previews, and
policy data. Native binding additionally persists the domain-separated
`caller_scope` computed while the plaintext key is transiently available. It
does not persist the key itself. The extra scope is required because it cannot
be derived from a plain SHA-256 key hash later.

## Authentication and policy data flow

For a request in `cpa-native` mode:

1. Extract the supplied key from the existing supported headers/query fields.
2. SHA-256 hash it and locate an enabled `cpa-native` policy binding.
3. Treat a missing binding, disabled binding, disallowed model, disabled models
   endpoint, exceeded RPM, or exceeded budget as a failed plugin authentication.
4. Because the provider is exclusive, CPA returns authentication failure and
   cannot try the built-in `api-keys` provider. Unbound keys are therefore
   denied, as explicitly approved.
5. On success, return the original supplied key as `Principal`, while retaining
   the internal policy ID only in plugin metadata.
6. CPA derives `caller_scope` from that original principal and publishes the
   same original value in usage events. The plugin resolves billing by hash;
   Keeper resolves it against the unchanged top-level `api-keys` list.

No logs, management response, runtime status, error, or concurrency row may
contain the plaintext principal. Existing previews remain the only displayed
key material.

`keysByCallerScope` is rebuilt by source and mode. Native entries use their
persisted scope; legacy entries continue to derive it from the internal ID.
This preserves O(1) concurrency identity lookup without retaining plaintext.

## Management API

Keep existing policy routes for compatibility and add an explicit native
binding route:

- `POST /v0/management/plugins/cpa-key-policy/native-keys/bind`

The request includes the selected CPA key transiently plus its policy fields.
The backend hashes the key, computes its masked preview and caller scope, marks
the entry `key_source: cpa-native`, rejects duplicate hashes, and persists no
plaintext. If `id` is omitted, it derives a stable non-secret identifier from
the hash; `name` remains editable.

Native bindings can be staged while the server is still in `plugin` mode, but
they do not authenticate until the mode switch. This enables a no-gap migration.

Existing route behavior becomes source-aware:

- `PATCH /keys` edits policy fields for both sources. It cannot replace a
  native binding's secret.
- `DELETE /keys` removes either source.
- `POST /keys/rotate` remains available only for plugin-owned entries and
  returns a conflict for native bindings.
- Legacy `POST /keys` keeps its current generate/import behavior only in
  `plugin` mode. In `cpa-native` mode it refuses generation and directs callers
  to the native binding route.
- Public key responses add `key_source` but never expose hashes, caller scopes,
  or plaintext keys.

The plugin backend cannot prove that an arbitrary management API caller supplied
a value currently present in CPA's top-level list. This does not expand the
trust boundary: that caller already holds the CPA management credential and can
add keys to the top-level list. The normal WebUI only submits values fetched
from CPA's own management endpoint.

## WebUI

In `cpa-native` mode the New Key flow becomes Bind CPA Key:

1. Fetch `GET /v0/management/api-keys` with the existing in-memory management
   session.
2. Keep returned plaintext values only in component memory and render masked
   previews.
3. Let the administrator select one existing key, configure its policy, and
   submit it to `/native-keys/bind`.
4. Do not show the generated-key modal and do not offer rotation for native
   bindings.

The list and edit pages label native bindings and retain all existing model,
RPM, budget, usage, and concurrency controls. Plugin mode retains the existing
UI for installations that have not migrated.

If a key is later removed from CPA's top-level list, the plugin cannot observe
that change server-side through the current ABI. The native-key picker/list
will compare the live CPA list with bindings when loaded, flag stale bindings,
and allow their removal. Operational documentation requires deleting the
plugin binding when deleting or rotating a CPA key. Until reconciliation, a
previously bound removed key remains accepted by the exclusive plugin; this is
an explicit current-ABI limitation, not silent behavior.

## State migration and server rollout

The rollout is fail-closed at the mode switch and preserves Keeper identity:

1. Back up the current CPA config, plugin binary, plugin config, and policy
   state file without printing any secret.
2. Deploy the new plugin while retaining `auth_mode: plugin`; verify legacy
   behavior and management access.
3. Fetch the three existing CPA keys through the local management interface and
   stage one `cpa-native` policy for each. Do not remove the keys from CPA's
   top-level `api-keys` list because Keeper uses that list as metadata.
4. Verify all three native bindings exist, are enabled as intended, and have
   model policies before switching.
5. Set `auth_mode: cpa-native` and reconfigure/restart CPA. Immediately verify
   an allowed request, a policy-denied request, a disabled or temporarily
   unbound native key returning 401 (proving no fallback), and `/v1/models`
   behavior.
6. Re-enable/bind the verification key, verify all three production keys, then
   verify CPA usage events and Keeper views retain the original key identities.
7. Remove obsolete plugin-owned bindings after the native path is proven.

Rollback restores the prior plugin binary/config/state together and restarts
CPA. Restoring only the mode while leaving mixed state is not considered a
complete rollback.

## Failure behavior

- Unknown, unbound, disabled, model-denied, RPM-limited, and budget-limited
  native keys fail authentication; exclusive registration prevents fallback.
- Missing credentials fail authentication.
- Duplicate native bindings are rejected.
- Missing/invalid caller scope on a native state entry fails validation or
  leaves concurrency admission fail-closed when concurrency is enabled.
- A configuration or mode switch to `cpa-native` with zero enabled native
  bindings is rejected to prevent accidental total lockout.
- Management routes remain protected by CPA's management credential and are
  not authenticated by this frontend provider.

An administrator can always disable/unload the plugin or edit CPA configuration;
that authority is outside the policy threat model. If the host removes the
exclusive plugin registration entirely, CPA can restore its built-in provider,
so deployment monitoring must treat plugin unload/fusion as a security-relevant
event.

## Testing and acceptance

Go tests will cover:

- config defaulting/validation and source migration;
- conditional exclusive capability registration;
- native success returning the exact original principal;
- unbound, disabled, model-denied, RPM-limited, and budget-limited native keys
  returning unauthenticated with no legacy-source match;
- legacy behavior remaining unchanged in `plugin` mode;
- native hash, preview, source, and caller-scope persistence with no plaintext;
- native bind duplicate handling, patch restrictions, generation refusal, and
  rotation refusal;
- usage billing resolving the raw native principal;
- concurrency resolving the native caller scope without plaintext headers;
- status/public responses never exposing hashes, scopes, or secrets.

Web tests will cover CPA `api-keys` response parsing, masked selection, binding
payloads, mode-dependent New/List/Edit behavior, removal of generated-key and
rotation controls in native mode, stale-binding indication, and translations.

Final local verification runs:

```text
go test ./...
go test -race ./...
go vet ./...
npm test
npm run build
```

The Web build is embedded into the Go plugin before packaging. Server acceptance
requires the live allowed/denied/fallback checks above plus a new Keeper usage
event associated with each original CPA key. No plaintext key may appear in
command output, commits, test fixtures copied from production, logs, or the PR.
