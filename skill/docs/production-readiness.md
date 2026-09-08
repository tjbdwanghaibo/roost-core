# roost-skill 生产基线

This document defines the supported production path for `skill`,
`skillcompose`, and `skillsync`. Legacy checkpoint and packet-only outbox files
are intentionally rejected; migrate by draining the old runtime/outbox before
deploying this version. Follow the
[stable package migration runbook](breaking-upgrade-skill-package.md). The Go
module is `github.com/tjbdwanghaibo/roost-skill` and therefore follows normal
v1.x semantic-version tags; the wire schema version is independent.

## Runtime limits

Every runtime is bounded by `RuntimeOptions`. Defaults are safe service
guardrails, not capacity targets: tune them from room-level load tests.

- `RuntimeEventLimit`, `StateEventLimit`, `StateMutationLimit`, and
  `PresentationLimit` bound delivery/diagnostic buffers. `CastEventLimit`
  independently bounds each cast's inspection history and reports
  `EventsDropped`. Monitor dropped
  counters and force a snapshot when a state cursor expires.
- `CompletedCastLimit` retains recent terminal casts for inspection. Active or
  still referenced casts are never evicted.
- `MaxActiveCasts`, `MaxAbilities`, `MaxOwnedProcesses*`, `RootEventLimit`, and
  `MaxProcLedgerEntries`
  provide deterministic backpressure through `ErrRuntimeCapacityExceeded`.
- Root event accounting is reclaimed only after no active cast, process, or
  scheduled task references the root, preserving once-per-root semantics.
- Checkpoints use version 2. `CheckpointMaxBytes` and
  `CheckpointMaxRecords` are checked before recovery publishes a runtime.
- A Host may implement `HostEventCompactor` only when Runtime is the exclusive
  event consumer. `MemoryHost` enables this explicitly through
  `NewMemoryHostWithOptions`; the default preserves event history.

`RuntimeEvents` is a bounded diagnostic view. Authoritative replication must
use `StateDeltas` and recover an expired cursor with `StateSnapshot`.

## Parsing generated skills

`Parse` and `ParseGenerated` apply `DefaultParseLimits`. Gateways with stricter
tenant limits should call `ParseWithLimits` or `ParseGeneratedWithLimits`.
Limits cover bytes, nesting, token count, string size, and entries per JSON
container before semantic decoding begins.

## Composition authority

Only contracts produced by `BuildContract` are accepted. `ValidateContract`
checks version, digest, authority, source identities, grants, obligations,
packages, constraints, and non-negative budgets. A composed candidate must
carry the exact authority, source `(skill_id, gameplay_digest)`, and per-feature
`(feature, source_id, transform)` provenance. `ValidateCandidate` rejects
missing, duplicate, extra, substituted, or unauthorized origins. Generators
should consume `DeriveContractPromptView`, not unsigned profile input.

## Durable sync outbox

Production coordinators must set `RequireDurableOutbox` and use a durable
`OutboxStore`. `FileOutboxStore` writes checksummed envelopes with atomic file
replacement and durability barriers. It rejects legacy unchecksummed formats
and bounds record count/record bytes through `FileOutboxOptions`.

Publish attempts and retry deadlines are persisted after every attempt.
`MaxPublishBatch` bounds both the selected candidate memory and one retry cycle.
Concurrent retry calls reserve selected packets, preventing duplicate
in-process publication. ACK lookup is indexed by observer/stream/epoch and is
bounded by `MaxPendingPerStream`, rather than scanning the global outbox.
`RetryPending` is incremental and
does not rescan history. Construction performs crash reconciliation once;
operators can call `ReconcilePending` for an explicit repair scan.

ACK remains the deletion boundary: a successful transport publish does not
remove a packet. Coordinator validates ACK under the observer/key lifecycle
lock, deletes durable outbox state before History, and immediately reconciles
the outbox if the History WAL commit fails. If deletion fails, History is
intentionally retained so reconciliation cannot lose the pending packet.

## Release gates

Before tagging a release, all of the following must pass:

```text
go mod verify
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
go test ./skill -run ^$ -fuzz FuzzParseGeneratedNeverPanics -fuzztime 30s
go test ./skill -run ^$ -fuzz FuzzRestoreRuntimeCheckpointNeverPanics -fuzztime 30s
go test ./... -run ^$ -bench . -benchmem -count=3
```

Also run `integration/sync-e2e` and the `examples` module with the exact
core/kit/skill tags selected for the release. The repository CI and nested
module `go.mod` files are the authoritative compatibility baseline; their
versions must be updated together and must not rely on an uncommitted go.work.
