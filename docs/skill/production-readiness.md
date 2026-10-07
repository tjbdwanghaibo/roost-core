# 技能系统（roost-core/skill）生产基线

This document defines the supported production path for `skill`,
`skillcompose`, and `skillsync`. Legacy checkpoint and packet-only outbox files
are intentionally rejected; migrate by draining the old runtime/outbox before
deploying this version. Follow the
[stable package migration runbook](breaking-upgrade-skill-package.md). The Go
module is `github.com/tjbdwanghaibo/roost-core/skill` and therefore follows normal
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
  still referenced casts (pending tasks, an active policy, or a spawn that is
  still running, including one handed off to its owner) are never evicted; when
  a cast is evicted, the records of its stopped spawns go with it
  (RR-20261006-23).
- `MaxActiveCasts`, `MaxAbilities`, `MaxOwnedSpawns*`, and
  `MaxProcLedgerEntries` provide deterministic backpressure through
  `ErrRuntimeCapacityExceeded`. `MaxQueuedTasks` (default 2048) bounds
  queued tasks that pin a root — passive activations not yet run and events
  queued by `QueueExternalEvent`: at the bound `QueueExternalEvent` and
  `ActivatePassive` return `ErrQueuedTasksFull` (which also matches
  `ErrRuntimeCapacityExceeded` under `errors.Is`), and a routed passive
  candidate is rejected — a `passive_suppressed` runtime event with Result
  `queue_full`, `skill.passive.dispatch_rejected.total{reason="queue_full"}`
  and a warning — while the event itself moves on (RR-20261006-55
  follow-up 2).
- Root event accounting is reclaimed only after no active cast, spawn, or
  scheduled task references the root, preserving once-per-root semantics.
- **`RootEventLimit > MaxActiveCasts + MaxOwnedSpawns +
  MaxStopPendingSpawns + MaxQueuedTasks`** (RR-20261006-55 follow-ups 1
  and 2). That sum bounds the distinct roots that can be referenced at once:
  - every unfinished cast pins one root (at most `MaxActiveCasts`); its
    scheduled tasks share that root;
  - entity spawns — casting, handed off or stop_pending — are at most
    `MaxOwnedSpawns`;
  - a phase/cast scoped spawn shares its cast's root while casting and can
    outlive the cast only as stop_pending (at most `MaxStopPendingSpawns`);
  - queued tasks — passive activations not yet run and `QueueExternalEvent`
    events — are at most `MaxQueuedTasks`, rejected at the entry when full;
  - nothing else pins a root: stopped and abandoned spawn records (so
    `MaxAbandonedSpawns` is not part of the sum), tasks left over by a cast
    that already ended (they are no-ops when they run), ability overlay
    expiries (they emit one `ability_enabled_changed` runtime event and never
    touch passive routing or root accounting) and ammo recharges.

  With the defaults the bound is 4096 + 128 + 256 + 2048 = 6528 < 8192.
  `NewRuntime` panics and `RestoreRuntime` fails (`ErrRuntimeLimitsInvalid`,
  plus `ErrCheckpointCorrupt` on restore) when the relation does not hold;
  the message names all five options with their effective values. Raise
  `RootEventLimit` together with any of the four. Under a legal
  configuration the root table therefore always has an unreferenced root to
  evict. The fallback stays as a defence: an event whose root cannot be
  tracked skips passive routing and moves on, counting
  `skill.root_event.capacity_dropped.total` and logging an error (before
  2026-10-07 the Runtime stopped on that event and `Advance` kept returning
  `ErrRuntimeCapacityExceeded`). The counter should stay at zero; a non-zero
  value is a bug — alert on it.
- Checkpoints use version 9 (2026-10-07: the payload carries
  `max_queued_tasks`, and a restore whose queued tasks exceed it is corrupt;
  versions 6–8, also 2026-10-07: one spawn record table, the `abandoned`
  spawn status with `max_abandoned_spawns`, and `spawn_event_sequence`;
  version 5, 2026-10-07: the unit-creating effect became
  `summon`, so a stored effect result's type `spawn_result` is now
  `summon_result` — see the
  [summon rename table](../feature/REFACTOR-2026-10-07-skill-summon-rename.md);
  version 4, 2026-10-06: every `process` name in the
  payload became `spawn` — `spawns`, `next_spawn_id`, `owned_spawns`, … —
  see the [rename table](../feature/REFACTOR-2026-10-06-skill-process-to-spawn.md);
  version 3 added the stop_pending retry state and the three stop-retry
  options). Older checkpoints are rejected with `ErrCheckpointUnsupported` —
  nothing was deployed, drain before upgrading. Every format change, extension
  or rename, bumps the version. `CheckpointMaxBytes` and
  `CheckpointMaxRecords` are checked before recovery publishes a runtime.
- A spawn the Host fails to stop stays owned by the Runtime, whichever stop
  entry asked for it (cast failure or any in-cast stop, failed spawn start,
  tick-driven reaping, `RemoveProgram`, `Shutdown` — one stop function since
  2026-10-07): it is marked `stop_pending`, leaves `OwnedSpawns`, and is
  retried with backoff (`SpawnStopRetryBackoff`, default
  4 ticks, doubling up to 64x) up to `SpawnStopRetryLimit` (default 10)
  failed retries, then `skill.spawn.stop_retry_exhausted.total` is counted,
  a warning is logged and the record is kept. At most
  `MaxStopPendingSpawns` (default 256) such records are kept; past it the
  oldest exhausted one (else the oldest still retrying) is abandoned: status
  `abandoned`, no more retries, it no longer pins its cast or counts against
  the stop-pending or owned limits, and `Shutdown` / `RemoveProgram` no longer
  stop it (`skill.spawn.abandoned.total` plus an error log naming spawn, cast
  and owner; the metric replaces `skill.spawn.stop_pending_dropped.total`).
  The record is kept — clients see one `spawn_update` / `spawn_upsert` with
  status `abandoned`, no `spawn_remove`, because the host may still run it.
  At most `MaxAbandonedSpawns` (default 1024) abandoned records are kept; the
  oldest past it are removed only at the end of `Advance`
  (`skill.spawn.abandoned_pruned.total` plus a warning; only then does a
  `spawn_remove` appear). No record leaves the Runtime in the middle of
  another loop. Alert on `skill.spawn.abandoned.total`: each one is a spawn
  the host may still run with nobody stopping it until match end / program
  removal. `Shutdown` / `RemoveProgram` return the first refusal
  and leave the spawn `stop_pending` in the checkpoint; keep advancing, or
  restore and advance, and the Runtime keeps retrying (calling them again
  re-requests immediately). New stop entries must be registered in
  `skill/spawn_stop_entries_promises_test.go`. Host `StopSpawn` must be
  idempotent. A restored
  checkpoint may hold more terminal casts than `CompletedCastLimit` when the
  excess are still referenced (RR-20261006-30).
- The same Runtime state always checkpoints to the same bytes and checksum:
  every list built from a map is written sorted by key (O7, maintainer round
  12; before that four lists followed map iteration order). The format did not
  change and restore ignores list order, so checkpoints written by earlier
  versions still restore; checkpointing again after the restore yields the
  sorted bytes.
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
and bounds record count/record bytes through `FileOutboxOptions`. The
directory belongs to one store: opening it removes `outbox-<digits>.tmp`
files left by a crash during a write (regular files with exactly that name
only; anything else is kept), so two live stores must not share a directory
(RR-20261006-04).

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
