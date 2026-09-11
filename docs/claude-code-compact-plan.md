# Claude Code Opaque Compaction Implementation Plan

## 1. Purpose

This plan adds production-grade Codex-backed fast compaction to the Claude-compatible `POST /v1/messages` endpoint. Claude Code must continue using the Anthropic Messages API without calling `/v1/responses/compact` directly.

The intended flow is:

```text
Claude Code
    |
    | POST /v1/messages
    v
CLIProxyAPI
    |- detect the real Claude Code compact request
    |- translate Anthropic history into Codex Responses input
    |- invoke Codex remote compaction v2
    |- persist the compact output behind an authenticated opaque marker
    |- return that marker as a valid Anthropic SSE response
    `- resolve the marker and restore compacted history on later /v1/messages calls
```

The implementation is incomplete until it supports root compaction, repeated compaction, account affinity, process restarts, idempotent crash recovery, bounded storage, encryption at rest, fail-closed recovery, a kill switch, diagnostics, and real Claude Code end-to-end verification.

## 2. Design Decision

The production implementation belongs in the CLIProxyAPI core Claude compatibility path, not solely in a dynamic plugin.

A plugin may later observe outcomes, expose management resources, or supply optional policies. It must not be the only implementation mechanism because:

- `request_interceptor.Terminate` is recorded as a rejected request even when it returns HTTP 200.
- Stream interceptors can rewrite existing chunks but cannot create and own a new response stream.
- Selected credential identity becomes authoritative during execution and must participate in persisted state authorization.
- State readiness, shutdown, config reload, storage locking, migrations, and corruption handling are process lifecycle concerns.
- The embedded SDK and the main server binary must have identical behavior.

The stable integration point is `ClaudeCodeAPIHandler.ClaudeMessages` before normal `Execute*WithAuthManager` dispatch. A restored normal request then re-enters the existing auth, translation, retry, streaming, and usage pipeline.

## 3. Required Invariants

1. Claude Code continues to call `POST /v1/messages`.
2. A marker is never returned before its state transaction commits.
3. The same logical compact retry returns the same marker and does not call upstream twice.
4. A recompact operation stays pinned to the credential that owns its predecessor state.
5. Marker text, session identifiers, auth identifiers, compact output, and encryption keys never appear in logs.
6. Disabling the feature performs no state-store I/O for new requests.
7. A disabled or irrelevant marker is never forwarded upstream as ordinary prompt text.
8. Corrupt or incompatible persisted state fails closed and is retained for diagnosis.
9. Semantic digests describe the final request actually sent upstream after restoration, trimming, and normalization.
10. Normal Claude requests have no behavioral change when the feature is disabled or no compact marker/prompt is present.
11. Protocol selection is declarative. Upstream error text must never trigger an automatic v2-to-v1 fallback.
12. Tests must distinguish “the implementation ran and succeeded” from “the implementation did not run.”

## 4. Target Architecture

```text
+----------------------- ClaudeCodeAPIHandler ------------------------+
|                                                                     |
|  parse body -> resolve session -> scan marker -> restore state      |
|                                  |                                  |
|                                  +-> detect compact prompt           |
|                                           |                         |
|                        normal request      | compact request         |
|                              |             v                         |
|                              |     ClaudeCompactService             |
|                              |             |                         |
|                              |     Claude history -> Codex input     |
|                              |             |                         |
|                              |     AuthManager execution             |
|                              |     provider=codex                    |
|                              |     selected/pinned auth              |
|                              |             |                         |
|                              |       +-----+----------------+        |
|                              |       | v2: /responses        |        |
|                              |       | + compaction_trigger  |        |
|                              |       | v1: /responses/compact|        |
|                              |       +-----+----------------+        |
|                              |             |                         |
|                              |     authenticated state store         |
|                              |             |                         |
|                              |     Anthropic SSE marker response     |
|                              v                                       |
|                    existing execution pipeline                       |
+----------------------------------------------------------------------+
```

## 5. Proposed Packages and Files

Create a focused internal package instead of putting the state machine in the HTTP handler:

```text
internal/claudecompact/
|- bridge.go             request orchestration
|- prompt.go             strict Claude Code compact-prompt detection
|- request.go            Anthropic history to compact request
|- response.go           synthetic Anthropic SSE marker response
|- marker.go             marker parsing, encoding, and authentication
|- state.go              state payload and restoration
|- store.go              storage interface and result types
|- bbolt_store.go        transactional persistent implementation
|- crypto.go             AEAD, bindings, and key derivation
|- keyring.go            keyring initialization and rotation
|- runtime.go            startup, shutdown, reload, and readiness
|- sentinel.go           persistent store identity
|- quarantine.go         corruption isolation
|- budget.go             size estimation and trimming
|- outcome.go            privacy-safe structured observations
`- errors.go             stable machine-readable failure taxonomy
```

Expected integration files:

```text
internal/config/sdk_config.go
internal/config/config.go
internal/config/config_defaults.go
internal/config/config_load.go
config.example.yaml
sdk/api/handlers/handlers.go
sdk/api/handlers/handlers_execution.go
sdk/api/handlers/claude/code_handlers.go
internal/api/server.go
internal/api/server_reload.go
sdk/cliproxy/builder.go
sdk/cliproxy/service_config.go
sdk/cliproxy/service_lifecycle.go
```

Changes under `internal/translator/` must be part of this broader feature and limited to reusable translation behavior or compact-specific helpers. The normal Claude-to-Codex translation contract must remain unchanged.

## 6. Phase 0: Evidence and Red Baseline

### Tasks

1. Capture a real Claude Code compact request fixture.
2. Capture the next real request after Claude Code consumes the compact response.
3. Capture a real Codex remote-compaction-v2 stream containing a `compaction` output item.
4. Add a handler-level test that currently fails because `/v1/messages` has no compact bridge.
5. Add a negative control proving that removing/bypassing the detector makes the test fail.

### Fixture requirements

The compact fixture should contain realistic system content, text, tool calls, tool results, and the complete compact instruction. A manually abbreviated prompt must not be the sole contract fixture.

### Acceptance

- The red test reaches the Claude handler.
- It proves an upstream compact execution was expected.
- It fails when no compact execution occurs, even if the HTTP request otherwise returns 200.

## 7. Phase 1: Configuration and Kill Switch

Extend `ClaudeCodeConfig` with a nested compact section:

```go
type ClaudeCompactConfig struct {
    Enabled              bool
    Protocol             string
    StorePath            string
    KeyringFile          string
    TTL                  time.Duration
    Capacity             int
    MaxBytes             int64
    TokenBudgetOverrides map[string]int
}
```

Example YAML:

```yaml
claude-code:
  disable-cloaking-model-list: false
  compact:
    enabled: false
    protocol: v2
    store-path: ""
    keyring-file: ""
    ttl: 168h
    capacity: 4096
    max-bytes: 268435456
    token-budget-overrides: {}
```

### Rules

- Default to disabled.
- Accept `v2` and `v1`. `auto`, if retained, must currently mean v2 without inferred fallback.
- Reject an enabled configuration without a stable store and keyring location.
- Reject a keyring inside the state-store directory.
- Validate positive capacity and byte limits.
- Validate model-budget overrides and reject values at or above a known measured failure point.
- Keep config cloning, JSON/YAML tags, management serialization, and hot reload consistent.

### Acceptance

- Missing config preserves existing behavior.
- Disabled mode does not create a directory, key, lock, or database.
- Invalid enabled config produces a precise startup/readiness failure.

## 8. Phase 2: Strict Compact-Prompt Detection

Port the structural detector from the source implementation rather than using a keyword match.

A match requires:

1. The final message is a user message.
2. The candidate is the final non-empty text block.
3. The expected prefix and suffix are both present.
4. All expected sections are present once and in order.
5. The normalized prompt reaches the minimum length.
6. Multiple complete candidates in one message are rejected as ambiguous.
7. Non-text blocks may coexist with the one matching text block.
8. CRLF and Unicode normalization do not change the decision.

Suggested API:

```go
type PromptMatch struct {
    Matched          bool
    MessageIndex     int
    ContentIndex     int
    NormalizedLength int
}

func DetectClaudeCodeCompactPrompt(body []byte) (PromptMatch, error)
```

### Tests

- Full real prompt matches.
- Prefix only, suffix only, missing section, duplicate section, reordered section, short prompt, and text following the prompt do not match.
- Two complete prompts are rejected.
- The detector removes only the matched block when other blocks coexist.
- A partial fingerprint emits only privacy-safe structural diagnostics.

## 9. Phase 3: Compact Request Construction

Reuse the existing Claude-to-Codex translator where possible:

```text
internal/translator/codex/claude/ConvertClaudeRequestToCodex
```

Add compact-specific processing outside the normal translation path.

Required order:

```text
remove compact instruction from Anthropic request
-> translate Claude history to Codex input
-> remove historical thinking/reasoning payloads for compact input
-> split a complete trailing tool call/output chain into preservedTail
-> apply tool-output trimming when required
-> perform budget planning
-> construct final v1 or v2 request
-> calculate semantic digest over the final upstream request
```

### Content handling

- Preserve text, images, documents, tool calls, tool results, and unknown content in a deterministic representation.
- Historical Claude `thinking`, `redacted_thinking`, and their translated reasoning carriers must not inflate compact input.
- The normal request translator must continue preserving compatible reasoning where currently required.
- Tool IDs and tool-name shortening must remain consistent with normal Claude-to-Codex translation.

### Preserved tail

A complete trailing tool chain is stored outside the compact output and restored verbatim later. Duplicate IDs, missing outputs, mismatched outputs, or conflicting replay must not be guessed away.

### Semantic digest

Include only semantic fields such as model, final input, instructions, tools, parallel tool behavior, reasoning options, text format, and service tier. Exclude volatile routing and transport identifiers such as request IDs, timestamps, cache keys, client metadata, and window transport headers.

## 10. Phase 4: Internal Codex Compact Execution

Add a dedicated core API; the Claude handler must not reach directly into provider executors.

Suggested contract:

```go
type CompactExecutionOptions struct {
    Protocol       Protocol
    PinnedAuthID   string
    SelectedAuthID func(string)
    Headers        http.Header
}

type CompactExecutionResult struct {
    Output         []json.RawMessage
    AuthID         string
    Usage          usage.Detail
    UpstreamMillis int64
}
```

### v2 behavior

- Force the Codex provider.
- Use OpenAI Responses input and output formats.
- Call normal `/responses` with one final `{"type":"compaction_trigger"}` item.
- Advertise `x-codex-beta-features: remote_compaction_v2` explicitly.
- Parse the full upstream SSE sequence.
- Require exactly one `type:"compaction"` output item and a terminal `response.completed` event.
- Treat zero/multiple compaction items and premature stream close as protocol errors.
- Mark deterministic protocol errors as non-retryable so they are not multiplied into paid retries.

### v1 behavior

Reuse the existing `Alt = "responses/compact"` path as an explicit operational escape hatch.

### Credential behavior

The existing auth manager already publishes selected auth metadata and accepts pinned auth metadata:

- root compact: capture the successful selected auth ID;
- recompact: set the predecessor auth ID as `PinnedAuthMetadataKey`;
- never advance a predecessor state under a different credential;
- report original-auth failure precisely instead of silently switching accounts.

### Cancellation

Downstream cancellation must stop the compact request and any retry backoff. No additional attempt may begin after the Claude Code client disconnects.

## 11. Phase 5: Marker and State Model

Keep a compact, authenticated marker compatible with the established format:

```text
<analysis>Opaque compact state retained locally.</analysis>
<summary>codex-opaque-state:v1:<state-id>:<content-hash>:<signature></summary>
```

Suggested state payload:

```go
type State struct {
    Version       int
    Output        []json.RawMessage
    PreservedTail []json.RawMessage
    SessionID     string
    Model         string
    AuthID        string
    VariantHash   string
    ContentHash   string
    Generation    uint64
    CreatedAt     time.Time
    ExpiresAt     time.Time
}
```

The marker must not reveal model, session, credential, key ID, output, or database key.

### Restore validation order

1. Parse marker syntax.
2. Verify marker signature.
3. Resolve state lookup.
4. Verify AEAD.
5. Validate payload schema.
6. Validate content hash.
7. Validate session binding.
8. Validate model binding.
9. Validate variant binding.
10. Validate credential binding.
11. Validate authenticated expiry.
12. Validate generation and predecessor relationship.

Keep distinct failure codes for each meaningful class.

## 12. Phase 6: Session and Variant Identity

Reuse `sdk/cliproxy/session` rather than inventing an unrelated session namespace.

Session precedence:

```text
X-Claude-Code-Session-Id
-> metadata.user_id session identity
-> legacy _session_<id>
-> canonical/derived session identity
```

An explicit Claude Code identity is preferred because a derived root may change across compaction.

Suggested variant binding:

```text
HMAC(sessionID, model, stableWindowID, canonicalToolDefinitions)
```

Do not bind the first user message, full instructions, request ID, timestamp, context length, or volatile transport metadata. These values may change across the compaction boundary.

If no stable window/subagent dimension is observable, use model plus canonical tools and document that this does not prove main/subagent isolation. Expand the binding only after wire-level evidence identifies a stable discriminator.

## 13. Phase 7: Transactional Persistence

Prefer `go.etcd.io/bbolt` for the first production implementation:

- pure Go and no CGO;
- transactional single-file storage;
- one writer with crash consistency;
- process-level file locking;
- natural key-value access for state and edge records;
- simpler cross-platform and multi-architecture builds than introducing a SQLite driver.

Suggested buckets:

```text
meta     schema version and store identity
states   lookup digest -> encrypted state record
edges    content-addressed edge -> encrypted successor marker
expiry   expiry + lookup -> record reference
lru      last-used + lookup -> state reference
```

### Content-addressed edge

```text
edge = HMAC(
    session,
    model,
    predecessorStateID or root,
    compactSemanticDigest,
    authBinding,
    variantBinding
)
```

One write transaction must:

1. Look for an existing edge winner.
2. Replay the winner marker without writing if present.
3. Authenticate the predecessor when present.
4. compare-and-swap the expected generation.
5. Write the new encrypted state.
6. Write the encrypted edge-to-marker mapping.
7. Prune within capacity and byte limits.
8. Commit before the response is exposed.

This transaction is required for root requests as well as recompact requests.

### Pruning

Prune in this order:

1. Authenticated expired records.
2. Superseded generations with no live, unconsumed outgoing edge.
3. Global LRU only when no safe superseded candidate exists.

Always protect the newly written state and its predecessor during the transaction.

## 14. Phase 8: Encryption, Keyring, Sentinel, and Quarantine

### Record encryption

Use record-level AES-256-GCM or ChaCha20-Poly1305. Derive account-scoped data keys from a master keyring. AAD must cover all security- and lifecycle-relevant metadata, including schema version, store identity, lookup digest, generation, authorization bindings, timestamps, byte size, and predecessor lookup.

Persist only HMAC-derived lookup and binding values. Do not persist plaintext session or auth identifiers in index fields.

### Keyring

- Store with mode `0600`.
- Permit automatic creation only during a proven first initialization.
- If persisted state exists and the keyring is absent, fail closed.
- Retain prior keys long enough to cover the latest authenticated expiry on disk.
- Never log key material, stable key IDs, or secret-bearing paths.

### Sentinel

Store an external identity record with `phase=init|ready` and a random store ID. It distinguishes first initialization from a deleted, cleared, or replaced database and makes initialization restartable after interruption.

### Quarantine

During cold recovery, authenticate and semantically validate every retained state and edge. If any record is unreadable or inconsistent:

1. make the runtime not ready;
2. close the store;
3. retain the original bytes;
4. move the store files into a timestamped quarantine directory;
5. write an active quarantine marker tied to the store ID;
6. refuse to create a replacement empty store automatically.

## 15. Phase 9: Claude Handler Integration

Update `ClaudeCodeAPIHandler.ClaudeMessages` in this order:

```text
read body
-> rewrite cloaked model ID
-> resolve stable session
-> scan for opaque marker
-> resolve and restore state when present
-> detect compact prompt
-> execute compact or continue through the existing normal handler
```

### Restore result

The effective history must be:

```text
current system/developer instructions
+ compact output
+ preserved tool tail
+ messages after the marker boundary
```

It must remove the marker wrapper and any already-replayed preserved tail without dropping the current user text.

### Disabled behavior

When the kill switch is off and an old marker is present:

- do not touch the store;
- do not forward marker text upstream;
- replace it with an explicit “earlier opaque history is unavailable” placeholder;
- continue through the normal path;
- record a privacy-safe disabled-marker event.

## 16. Phase 10: Native Anthropic SSE Response

After the state transaction commits, emit a local Anthropic SSE sequence:

```text
message_start
content_block_start
content_block_delta(marker)
content_block_stop
message_delta(stop_reason=end_turn)
message_stop
```

Requirements:

- HTTP 200;
- `Content-Type: text/event-stream`;
- normal successful request lifecycle, not plugin rejection;
- downstream cancellation propagation;
- no marker response before commit;
- no full marker in logs;
- no dependency on a normal model translator to synthesize this local protocol response.

## 17. Phase 11: Failure Taxonomy and Recovery Policy

Define stable machine-readable failures, including:

```text
invalid_marker
tampered
not_found
expired
session_mismatch
model_mismatch
variant_mismatch
account_mismatch
content_hash_mismatch
stale_generation
preserved_tail_conflict
store_unavailable
store_locked
schema_unsupported
key_unavailable
key_mismatch
state_corrupt
state_too_large
migration_failed
budget_exceeded
prompt_too_long
auth_unavailable
protocol_error
transport_failure
```

Policy matrix:

| Failure class | Normal request | Compact request | Expected behavior |
|---|---|---|---|
| irrelevant/unparseable marker | continue with placeholder | new root compact | no opaque text forwarded |
| missing/expired state | 400 with `/compact` guidance | self-heal as root compact | no client retry storm |
| account mismatch | reject | reject | never cross account boundary |
| store/key/schema corruption | reject | reject | runtime becomes not ready |
| budget/context overflow | n/a | controlled fallback or precise error | old state remains valid |
| root upstream failure | n/a | configured fallback/error | structured outcome |
| recompact original-auth failure | n/a | 409 | do not switch auth |
| stale generation | n/a | retry guidance/winner replay | preserve CAS semantics |

Deterministic client errors and protocol errors must not be retried as transient upstream failures.

## 18. Phase 12: Input Budgeting

Implement two-stage planning:

```text
cheap byte estimate
    |- clearly within budget -> execute
    `- possibly over budget
          -> precise tokenizer estimate
          -> trim oversized tool outputs
          -> estimate again
          -> execute or controlled fallback
```

Initial delivery may use a conservative byte estimate plus per-model budgets and tool-output trimming. A later step may add a lazily loaded tokenizer.

Record estimate source, cheap estimate, final estimate, budget, trim count, image presence, and content byte categories. Never describe a theoretical model limit as measured unless an actual success/failure boundary was observed.

## 19. Phase 13: Runtime Lifecycle

Cover both the main server and embedded SDK.

### Main server

Integrate initialization, injection, reload, and shutdown through:

```text
internal/api/server.go
internal/api/server_reload.go
server shutdown path
```

### Embedded SDK

Mirror lifecycle handling through:

```text
sdk/cliproxy/builder.go
sdk/cliproxy/service_config.go
sdk/cliproxy/service_lifecycle.go
```

### Reload semantics

- false -> true: initialize fully, then atomically publish the ready runtime;
- true -> false: stop new store access, then close and release the lock;
- path/key/schema change: build and validate a replacement before swapping when safe;
- stale handles must never close a newer runtime instance;
- a runtime storage fault must detach the store, release resources, and publish one stable readiness reason.

## 20. Phase 14: Readiness, Management, and Observability

Expose an authenticated management status endpoint containing only aggregate information:

```json
{
  "enabled": true,
  "ready": true,
  "reason": null,
  "protocol": "v2",
  "state_count": 128,
  "state_bytes": 1234567,
  "edge_count": 31,
  "oldest_expiry": "...",
  "newest_expiry": "..."
}
```

Do not expose marker, state ID, session ID, auth ID, key ID, ciphertext, or local absolute paths.

Record outcomes with request ID, irreversible session/account tags, model, generation, compact path, result, cause, replay flag, duration, upstream duration, retry count, estimates, budget, and trimming statistics.

Suggested counters:

- compact attempts and successes;
- successor replays;
- budget exceeded;
- upstream/protocol failures;
- restore success and rejection by reason;
- readiness transitions;
- compact input/output usage;
- compact latency.

## 21. Phase 15: Test Matrix

### Unit tests

| Module | Required coverage |
|---|---|
| prompt | complete, partial, duplicate, order, CRLF, NFC |
| marker | parse, sign, truncate, tamper, wrapper |
| state | session, model, auth, variant, content hash |
| crypto | every AAD field mutation fails authentication |
| keyring | create, rotate, retain, missing, permissions |
| request | text, image, document, tool, unknown block |
| preserved tail | pair, duplicate, conflict, replay removal |
| budget | cheap, precise, image, trim, fallback |
| errors | every reason maps to stable HTTP/user behavior |
| config | disabled default, invalid paths, reload |

### Store integration tests

- save, close, reopen, resolve;
- second process receives `store_locked`;
- bit flip causes quarantine;
- missing database with retained sentinel causes reset detection;
- missing keyring with retained store fails closed;
- old schema migrates atomically or reports unsupported;
- identical concurrent edges commit once and return one marker;
- different semantic digests from one predecessor create valid forks;
- response loss after commit replays without upstream execution;
- capacity pressure prefers superseded generations;
- restore extends sliding TTL correctly.

### Handler integration tests

- real Claude compact prompt -> Codex compact -> marker SSE;
- marker next turn -> restored output -> normal Codex execution;
- root compact -> recompact -> generation two;
- process restart -> same marker resumes;
- recompact pins original auth;
- unavailable original auth does not switch credentials;
- disabled mode never forwards marker text;
- ordinary Claude requests remain unchanged;
- non-Codex routes are not accidentally intercepted;
- client abort prevents further compact attempts.

### Real Claude Code E2E

1. Start a clean CLIProxyAPI instance.
2. Enable compact state.
3. Start a real Claude Code conversation and establish unique facts.
4. Trigger the real `/compact` command.
5. Verify that Claude Code receives a marker rather than a rendered summary.
6. Ask for facts from before compaction.
7. Trigger a second compact.
8. Verify earlier facts again.
9. Restart CLIProxyAPI.
10. Continue the same conversation and verify recovery.

### Negative controls

- delete/bypass detector: E2E must fail;
- delete state: normal turn gives a deterministic failure and `/compact` self-heals;
- tamper marker: request is rejected;
- change session/model/auth/variant: corresponding binding failure appears;
- disable switch: store is not touched and marker is replaced visibly.

## 22. Phase 16: Performance and Fault Testing

Targets:

| Operation | Target |
|---|---:|
| request with no marker/prompt | near-zero additional cost |
| marker resolve | local p95 below 10 ms |
| successor replay | local p95 below 10 ms |
| state commit | local p95 below 20 ms under normal load |

Prevent:

- full request cloning for every SSE chunk;
- full database scans per request;
- tokenizer loading for normal requests;
- complete compact bodies in logs;
- repeated base64 decoding of large images;
- long bbolt read transactions blocking writes;
- stale runtime handles closing replacement stores.

## 23. Phase 17: Release, Migration, and Rollback

### Initial rollout

1. Merge and deploy with the feature disabled.
2. Verify ordinary Claude Code traffic.
3. Configure store and keyring.
4. Enable compact.
5. Verify readiness.
6. Run root compact and resume.
7. Restart the process and resume again.
8. Run recompact and verify generation/account behavior.

### Existing state migration

Do not let the TypeScript and Go implementations write the same store. The source implementation uses SQLite and its own key derivation/AAD rules, while this plan recommends bbolt.

Safe options:

- preferred first release: new compactions use the new store; old markers receive clear `/compact` recovery guidance;
- optional later migration: stop old writes, read and authenticate old SQLite records offline, write bbolt records, read every migrated record back, and retain the original database read-only.

### Rollback

1. Disable compact first.
2. Preserve the store and keyring.
3. Roll back the binary.
4. Do not delete marker state.
5. Redeploy a compatible fixed version.
6. Re-enable only after readiness and restart recovery pass.

## 24. Proposed Pull Request Sequence

| PR | Scope | Dependency |
|---|---|---|
| 1 | fixtures, strict detector, marker parser, red tests | none |
| 2 | compact request builder, preserved tail, semantic digest | PR 1 |
| 3 | Codex v2/v1 compact execution and selected/pinned auth | PR 2 |
| 4 | in-memory state, root compact, resume, recompact | PR 3 |
| 5 | bbolt, AEAD, keyring, sentinel, CAS, successor edges | PR 4 |
| 6 | runtime lifecycle, hot reload, SDK, kill switch | PR 5 |
| 7 | budgets, trimming, failure policy, fallback | PR 3-6 |
| 8 | management status, metrics, privacy-safe outcomes | PR 5-7 |
| 9 | restart/concurrency/fault E2E and real Claude Code proof | all |

The in-memory phase proves the vertical protocol path. It is not sufficient for production release; release requires persistent state and restart recovery.

## 25. Definition of Done

- [ ] Claude Code triggers compaction through `/v1/messages`.
- [ ] Claude Code never needs to call `/v1/responses/compact`.
- [ ] Remote compaction v2 returns and preserves exactly one `compaction` item.
- [ ] Claude Code receives a valid marker SSE response.
- [ ] The next turn restores complete compact output.
- [ ] Tool tails are neither lost nor duplicated.
- [ ] Current system/developer instructions survive restoration.
- [ ] Recompact advances generation correctly.
- [ ] Recompact remains pinned to the original Codex auth.
- [ ] Identical concurrent edges commit only once.
- [ ] Post-commit response loss replays the same marker.
- [ ] Markers survive a process restart.
- [ ] Key, store, schema, and tamper failures fail closed.
- [ ] The kill switch performs no state access.
- [ ] Ordinary Claude requests have no regression.
- [ ] Usage is attributed to the actual Codex auth and model.
- [ ] Logs contain no full marker, session, auth ID, or plaintext history.
- [ ] Real Claude Code passes root compact -> resume -> recompact -> restart -> resume.
- [ ] Deliberately breaking the implementation makes the relevant tests fail.
- [ ] `gofmt -w .` has been run for Go changes.
- [ ] Targeted tests pass.
- [ ] `go test ./...` passes.
- [ ] `go build -o test-output ./cmd/server` succeeds and the temporary artifact is removed.

## 26. Recommended Execution Order

Build the smallest real vertical path first:

```text
real Claude compact fixture
-> strict detector
-> Claude-to-Codex compact request
-> remote_compaction_v2
-> in-memory compact output
-> marker SSE
-> next-turn restoration
```

Then add, in order:

```text
credential binding
-> recompact
-> CAS and successor-edge idempotency
-> bbolt
-> AEAD and keyring
-> restart recovery
-> quarantine
-> budgets
-> management and metrics
```

Every stage must have an observable failure mode and a negative control. Do not complete a storage subsystem first and postpone proving that the real Claude Code client consumes and replays the marker correctly.
