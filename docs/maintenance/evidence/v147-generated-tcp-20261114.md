# B-16 generated TCP fixture evidence (2026-11-14)

Base: `f6ffac36` after the B-4 batch. The new versioned entry is `scripts/perf/generated-tcp.sh`; its exact options and scope are in `scripts/testdata/generated-tcp/README.md`. No framework behavior changed.

Private mirror dependency environment: offset 35000, isolated Mongo replica set. Every row below ran the actual script for 10 seconds with 50 TCP connections, GOWORK=off and GOFLAGS=-p=2. Each script performed CLI generation, current-checkout replacement, build, vet, ID validation, generated-file drift validation and one non-skipped real test. Runs were serial. Other development was active: these are functional smoke counts, not capacity measurements.

| Durability | Self sync | Entity distribution | Acknowledged = cold Gold | Sync packets |
| --- | --- | --- | ---: | ---: |
| strict | False | distinct | 10078 | 0 |
| strict | False | shared | 1189 | 0 |
| strict | True | distinct | 10935 | 10985 |
| strict | True | shared | 1266 | 29690 |
| pipelined | False | distinct | 8247 | 0 |
| pipelined | False | shared | 1111 | 0 |
| pipelined | True | distinct | 6660 | 6710 |
| pipelined | True | shared | 1209 | 3451 |
| async | False | distinct | 9380 | 0 |
| async | False | shared | 899 | 0 |
| async | True | distinct | 9246 | 9296 |
| async | True | shared | 1347 | 29342 |

Every row: errors=0, dropped=0, cold_verified=true, WAL unacked=0. Shared mode uses one Player with separate authenticated sessions; distinct mode uses 50 Players. Per-connection final self_sync Gold is checked where enabled.

The fixture uses Robot transport/AuthPacket, generated PB codecs, Session/Recorder, generated TCP/access/Nest modules, real DataEngine Runtime and file WAL/Mongo. Clients and server modules share a test process; service main/config, independent Gate, unknown-result recovery, cross-machine and device tests are outside this fixture. The historical 2026-10-20/22 service-process numbers are not directly comparable.

Initial fixture failures were corrected before final acceptance: full Entity IDs were passed as authentication player numbers (the generated endpoint encodes the number itself), and the pipelined allowlist required `handlerAddGold` rather than `AddGold`. Cold verification caught the first mismatch; the official admission gate rejected the second. All four pipelined rows were rerun after that correction.

Artifacts: `/private/tmp/v147-tcp-final-*` (strict/async), `/private/tmp/v147-tcp-fixed-pipelined-*`, and `scratchpad/v147-moba/tcp-*.log`; each output contains `env.txt`, generated consumer, test binary, `report.json` and WAL. Scratch summary: `scratchpad/v147-moba/tcp-smoke-summary.json`.

Graph: Tier 2 on parent project generation `2026-11-14T02:19:08Z`; generator self-sync test and gateway session registration located with graph/snippets, relevant Runtime/Repository/WAL and Robot APIs coverage checked and read. New files are missing in that parent generation; exact source plus actual generated compilation/race is the evidence, and the parent owns final refresh.

KNOWN-LIMITS closing text: B-16 / No.103,112 missing reusable fixture is closed by the versioned generated TCP entry and real-dependency smoke matrix. Keep M-17 formal isolated-host self_sync, durability and open-loop capacity comparisons outstanding. Do not promote these functional samples into a capacity baseline.

Additional gates: actual script with pipelined + self_sync + OPEN_LOOP_RATE=100 for 10 seconds: offered 1000, acknowledged 1000, dropped 0, errors 0, cold Gold 1000, WAL unacked 0. Healthy `-race` run passed; mutation removing the DAO `SetGold` call failed with `cold Gold id=10240005 got=0 acknowledged=238`; restoring the exact business file passed `-race` again. Root `go test -count=1 .` passed; document anchors reported 0 broken. Logs are `tcp-open.log`, `tcp-race.log`, `tcp-mutation.log`, `tcp-restored.log`, `tcp-root.log`, `tcp-anchors.log` under the scratch directory above.
