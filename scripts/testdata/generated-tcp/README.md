# Generated TCP persistent writes

Run `scripts/perf/generated-tcp.sh` from any directory after setting an isolated `ROOST_DATAENGINE_IT_MONGO_URI`. The script generates a fresh project with the current CLI, compiles it against the current checkout, checks IDs and generation drift, builds/vets it, and runs `TestGeneratedTCPPersistentLoad`. All artifacts remain in the output directory; only the uniquely named Mongo fixture database is dropped after successful verification. A failure preserves its data.

```sh
# 10-second functional smoke, 50 connections with separate Player entities.
ROOST_GENERATED_TCP_OUTPUT=/tmp/tcp-strict DURABILITY=strict \
  bash scripts/perf/generated-tcp.sh

# Concurrent requests to the same Player, separate authenticated sessions.
ROOST_GENERATED_TCP_OUTPUT=/tmp/tcp-shared CONNECTIONS=2 ENTITY_MODE=shared \
  SELF_SYNC=1 DURABILITY=pipelined bash scripts/perf/generated-tcp.sh

# Global message arrivals per second, independent of replies; bounded queue.
ROOST_GENERATED_TCP_OUTPUT=/tmp/tcp-open OPEN_LOOP_RATE=100 \
  DURABILITY=pipelined SELF_SYNC=1 bash scripts/perf/generated-tcp.sh
```

Settings: `DURABILITY=strict|pipelined|async`, `SELF_SYNC=0|1`, `ENTITY_MODE=distinct|shared`, `CONNECTIONS` (default 50, range 1..5000), `ROOST_GENERATED_TCP_SECONDS` (default 10, range 1..300), `OPEN_LOOP_RATE` (0 for closed loop, otherwise 1..100000 messages/s), `GOMAXPROCS`, `GOFLAGS`. Each run requires a new output directory. `self_sync` is always generated but its runtime modules are enabled only in the selected mode. WAL group commit interval is explicitly 2ms for every mode.

The fixture uses the public Robot PlayerTCP transport (including the connection-generation handshake), generated PB registry and Robot Session/Recorder. The test process hosts the client and the generated TCP/access/Nest modules. DataEngine uses its real Runtime, file WAL, MongoStore and projector; no effects are emitted. This is not a generated service-main/config or multi-process benchmark, and its figures should not be compared directly with the historical 2026-10-20/22 service-process measurements.

Each accepted AddGold request increments the DAO exactly once through the generated Setter and Sender. The client never retries unknown requests. Player authentication uses the unique player number; the generated endpoint builds the full entity ID. Shared mode authenticates several independent sessions to the same player. After requests finish, the projector drains, a new EntityManager/Repository cold-loads every target and compares Gold with acknowledged calls. When self_sync is enabled, every connection must receive that same final Gold through the generated packer and real TCP. WAL unacked must be zero.

`report.json` distinguishes TCP request/reply latency, scheduled-arrival-to-reply latency, client queue delay, acknowledged request throughput, projection drain time, WAL append/sync counts, persisted Gold, self-sync packets and the Robot request report. Robot statistics include EnterGame; Gold counts do not. In open loop, missed arrival slots and full-queue arrivals are dropped and counted, never silently converted into a closed-loop stream or replayed as an unbounded burst. Outstanding queued requests drain after the offer window, so `elapsed_seconds` can exceed the configured duration. Zero samples and accounting mismatches fail.

Ten-second concurrent development runs are functional diagnostics, with `capacity_claim: false`. Formal M-17 measurements require an otherwise idle machine, recorded load/swap and repeated runs with identical options. These checks do not cover unknown-commit recovery, power loss, independent Gate, client-device behavior, cross-machine networking or target deployment capacity.
