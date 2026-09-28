# v1.17.2 之后仍未完成 / 未验证 / 保留的条目（2026-09-28）

- **基线**：代码与行号基于 main `51fc7ee`（v1.17.2 发版前最后一笔代码提交）。v1.17.2 tag 指向 `924cc5d`；`51fc7ee` 之后只有文档与版本号提交：`00194bb`（版本清单、生成器下限、CHANGELOG 标题、索引）、`924cc5d`（交接文档 §5 插入一行 `:110`，所以交接文档 `:110` 以后的行号在 HEAD 上要 +1）、`18467f8`（OPEN-ITEMS §H 记录发版）。
- **与 OPEN-ITEMS 的关系**：[OPEN-ITEMS-2026-09-27](OPEN-ITEMS-2026-09-27.md) 是过程清单，记录 v1.17.1 之后逐条处理的经过，进度在 §H。本文是发版后的结论清单，只列到 v1.17.2 为止仍然成立的条目。OPEN-ITEMS 里的条目沿用原编号（A / B / C）。其余条目来自修复记录 `RR-20260927-*` / `RR-20260928-*`、审计记录 audit5～8 和交接文档 §5，OPEN-ITEMS 没有编号，本文给 `N` 编号。
- **读取范围**：OPEN-ITEMS 全文（§0～§H）；`docs/bugfix/RR-20260927-01～35`（08 号未使用）与 `RR-20260928-01～14` 共 48 份，每份的“未验证项 / 风险 / 边界 / 另外发现 / 范围外发现”以及后加的“后续验证 / 后续更正 / 更正 / 合并时补修 / 维护者决定”节；[audit5](REVIEW-2026-09-27-audit5.md)、[audit6](REVIEW-2026-09-27-audit6.md)、[audit7](REVIEW-2026-09-28-audit7.md)、[audit8](REVIEW-2026-09-28-audit8.md) 全文（只有 audit8 有“处理结果”一节）；[交接文档](../CORE-OPTIMIZATION-HANDOFF.md) §5。B27 引用的 RR-20260926 修复记录另读了各自的“未验证项”节。
- **本次只读核对**：`git log ffcf902..51fc7ee`（122 笔）；`gh run list` / `gh run view`（见 §6）；`GOWORK=off go run ./cmd/glsvet -tests ./nest` 在 `51fc7ee` 上仍报 3 条（N03）；`git worktree list` 只剩主检出，OPEN-ITEMS §F 列出的残留 worktree 已全部移除。codebase-memory 的 `index_status` 显示 ready，但不给出 generation 时间，无法证明图谱已刷新（C33）。本文结论全部来自源码、`git` 和上述实跑。

### 统计（OPEN-ITEMS 的 A / B / C 共 93 条）

| 类 | 总数 | 已完成 | 已接受 / 保留（已记录） | 剩余 |
| --- | --- | --- | --- | --- |
| A | 13 | 13 | 0 | 0 |
| B（B01～B46） | 46 | 43 | 0 | 3：B27、B29、B30 |
| C（C01～C34） | 34 | 17 | 14：接受 6 条（C09、C16、C19、C21、C28、C30），保留 8 条（C02、C04 的 k8s / systemd 部分、C12、C20、C22、C23、C24、C34） | 3：C01、C03、C33 |
| 合计 | 93 | **73** | **14** | **6** |

计数口径：

- **已完成**：有修复提交，或已收为回归、写成文档。B 类中证实为缺陷的（B03、B06、B07、B20 的相邻形状、B23、B25、B39、B41、B42、B43）已转为 RR-20260927 / RR-20260928 并修复，按已完成计。B22 的后一半转成了 C34，B22 本身按已完成计。
- **已接受 / 保留**：维护者已有决定并写进了记录。其中“保留”是还没做完的，本文仍列出：C02、C04 放 §2；C12、C20、C22、C24 放 §3；C23 放 §5；C34 放 §1。C21 已接受，但按任务要求在 §3 注明需要业务方确认的那一半。
- **剩余**：C01、C03、C33、B27、B29、B30，都在 §1。
- **OPEN-ITEMS 之外的新条目（N）**：§1.2 共 7 条，§2 共 6 条，§3 共 13 条，§4 共 18 条，§5 共 3 条，§6 共 1 条。§6 的 N70 是发版后 CI 上新出现的失败。
- **OPEN-ITEMS §D 的 39 条已接受边界不在本文重复**，见 [OPEN-ITEMS §D](OPEN-ITEMS-2026-09-27.md)。

各节条数：§1 待执行 14 条（OPEN-ITEMS 编号 7 条 + N 7 条）；§2 本地做不了 9 条（C02、C03 的断电部分、C04 + N 6 条）；§3 待设计 / 决定 18 条（C12、C20、C21、C22、C24 + N 13 条）；§4 已知残留风险 18 条；§5 观察项 4 条（C23 + N 3 条）；§6 CI 4 条。

---

## 1. 待执行（本地能做）

### 1.1 已排期（OPEN-ITEMS §H 批次 8、批次 9、C33、C34）

OPEN-ITEMS §H 把批次 8（长跑与性能：B29、B30、C01、C03）定为“最后跑、独占机器”，批次 9（B27）为“待开始”。维护者 2026-09-27 定下的规则是：长稳和性能对照在本机跑，结果注明“单机 macOS、有背景负载”（[OPEN-ITEMS §H C 类推荐处理](OPEN-ITEMS-2026-09-27.md)）。环境先 `source /tmp/roost-dataengine-it/env.sh`（隔离环境，brew 二进制）。性能条目不要与 race、故障注入、索引重建同时跑（[交接文档 §6](../CORE-OPTIMIZATION-HANDOFF.md)）。

| 编号 | 内容 | 需要 | 入口 | 完成判据 |
| --- | --- | --- | --- | --- |
| **B30** | 正式 kit Backend 装配下的 Remote 30 分钟持续压测和容量阶梯没有复测。交接文档 §4 的 80 / 120 / 160 TPS 来自直接注入 MongoCommitter 的装配，交接文档 §5 `:105` 写明“在正式装配复测前不作为上线依据”。来源：[OPEN-ITEMS B30](OPEN-ITEMS-2026-09-27.md)、[交接文档 §5](../CORE-OPTIMIZATION-HANDOFF.md) `:105` | 独占本机约 1 小时：30 分钟持续压测 + 阶梯 5 档 × 120s + 准备时间。需要隔离环境。与 C01 共用 `remote-acceptance.lock`，只能串行 | [`scripts/perf/remote.sh`](../../scripts/perf/remote.sh)：`ROOST_REMOTE_LABEL=<新标签> ROOST_REMOTE_DURATION=30m ROOST_REMOTE_RATE=80` 加上[九项报告“复跑入口”](../feature/REFACTOR-2026-09-26-core-nine-items.md) 24 小时命令里的其余参数；[`scripts/perf/remote-capacity.sh`](../../scripts/perf/remote-capacity.sh)：`ROOST_REMOTE_CAPACITY_LABEL=<新标签> ROOST_REMOTE_RATES='80 120 160 200 240'`。fixture 已走 Backend（`codegen/internal/entity/testdata/remoteflow/flow_test.go`） | 30 分钟 80TPS：0 错误 / 丢弃，WAL / 投影失败为 0，Mongo / NATS / outbox 全量核验通过。阶梯给出“最后通过档 / 首个过载档”。结果写进交接文档 §4 的 Remote 两行，并把 §5 `:105` 的“保留为 B30”改成实测结论 |
| **C01** | 24 小时长稳从未跑过。30 分钟压测中堆在 86.94～307.58MB 之间、后半程基线偏高，事务保留接近 65536 上限，不能宣称排除了泄漏。来源：[OPEN-ITEMS C01](OPEN-ITEMS-2026-09-27.md)、[交接文档 §5](../CORE-OPTIMIZATION-HANDOFF.md) `:101`、[fix-verification §未做](REVIEW-2026-09-26-fix-verification.md) | 独占本机约 25 小时（`ROOST_REMOTE_TIMEOUT=25h`），期间不能跑其他 Remote 验收。建议排在 B30 之后，先确认 30 分钟基线 | [九项报告“复跑入口”](../feature/REFACTOR-2026-09-26-core-nine-items.md)的 24 小时命令：`ROOST_REMOTE_DURATION=24h ROOST_REMOTE_TIMEOUT=25h ROOST_REMOTE_RATE=80 ROOST_REMOTE_SESSIONS=1000 ROOST_REMOTE_ENTITIES=10000 … bash scripts/perf/remote.sh`，用新 label | 24 小时 0 错误；堆与事务保留量没有单调增长（按小时采样给出趋势）；保留量低于 65536；结果按“单机 macOS、有背景负载”写进交接文档 §5 `:101`，替换“24小时未运行” |
| **B29** | 性能对照缺口。多数修复只做了微基准，其中一部分跑在高负载下（load 5～11），有的没有跑 benchstat，有的没有端到端数字。来源：[OPEN-ITEMS B29](OPEN-ITEMS-2026-09-27.md) 及下表各行 | 空闲机器，每组每侧 25～35 分钟，后台跑。修前树用 `git worktree add` / `git archive` 导出，两侧 `go test -c` 后交替运行，用 benchstat 比较 | 见下表 | 每组得到 benchstat 结果（n ≥ 10）并写进对应修复记录的“性能”节：没有显著退化，或者退化已如实记录并由维护者接受 |
| **C03（可做部分）** | Linux fsync 对照。RR-16（async + Ack 多出的 fsync）只有 macOS `F_FULLFSYNC` 数据（v1.17.0 triage：group-commit 2ms 时慢 4～9%，fsync 次数 +12～25%）。RR-20260928-11（pipelined 回退到 strict 路径时等 fsync）同样只有 macOS 数据，且“未在 Linux 上实测”。来源：[OPEN-ITEMS C03](OPEN-ITEMS-2026-09-27.md)、[triage §性能](REVIEW-2026-09-26-v1170-triage.md)、[RR-20260928-11 §未验证项](../bugfix/RR-20260928-11.md) | 本机 Docker Desktop 的 Linux 容器（虚拟化环境，要注明口径）。本机没有 golang 镜像，按规则不拉取：改为在宿主机 `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c` 交叉编译测试二进制，放进 `FROM scratch` 形态的容器运行（批次 7 的 compose 演练用过这种做法）。WAL 目录放在容器内 volume，不用 bind mount | 基准：`nestwal` 的 `BenchmarkWALAppendStrict` / `BenchmarkWALAppendAsyncParallel` / `BenchmarkBroadcastPipelinedCommit`；`dataengine/engine` 的 `BenchmarkProjectorAdmissionMatrix`（async）/ `BenchmarkProjectorWALReplayAckMatrix`。修前 / 修后按 B29 的 D 组和 E 组选。triage 当时用的是哪个基准：**待核实** | 得到 Linux 容器内的 fsync 次数（`Stats().Syncs`）和吞吐对照，写进 [RR-20260926-16 修复记录](../bugfix/RR-20260926-16.md)和 [RR-20260928-11 修复记录](../bugfix/RR-20260928-11.md)，标注“Docker Desktop VM、非物理机”。断电和 dm-flakey 的部分仍在 §2 |
| **B27** | 真实环境 / 生成工程端到端补测：多条修复只用替身、受控 committer、回环连接或 recorder 验证过。按 OPEN-ITEMS 建议分四批，本文把 RR-20260927 / 28 记录里新报的同类缺口也并入。来源：[OPEN-ITEMS B27](OPEN-ITEMS-2026-09-27.md) 及下表各行 | 隔离集成环境（Mongo 副本集 / NATS / Redis），串行执行；每批约半天到一天（写 fixture + 实跑） | 写成 remoteflow / dataengine fixture 用例或 `-tags integration` 用例，用 [`scripts/test-remote-generated.sh`](../../scripts/test-remote-generated.sh)、[`scripts/test-dataengine-generated.sh`](../../scripts/test-dataengine-generated.sh) 跑；场景类的用生成 game-demo + `deploy/dev/run.sh` | 每份记录的“未验证项”都追加“后续验证”节，写明已补的用例名，或者说明为什么做不了（例如转入 C34 / §2） |
| **C33** | codebase-memory 图谱从 09-26 起没有刷新（交接文档写的 generation 是 `2026-09-26 08:36`）。刷新被“pre-coordination or unverified CBM generation is active”阻止。48 份新记录全部写的是“图谱过期、以源码为准”。来源：[OPEN-ITEMS C33](OPEN-ITEMS-2026-09-27.md)、[交接文档 §5](../CORE-OPTIMIZATION-HANDOFF.md) `:115` | 需要没有其他会话在用 CBM 的时间窗：kill 守护进程会断开所有连着它的 MCP 会话。约 10 分钟 | 先 `ps` 看 `codebase-memory-mcp` 有没有停止态（`STAT=T`）的进程，再 `lsof /private/tmp/cbm-daemon-502/*` 查锁的持有者；没有持有者的陈旧 `.sock` / `.anc` 移走（不删）；然后 `codebase-memory-mcp daemon start`，再 `codebase-memory-mcp cli index_repository --repo-path /Users/whb/roost/roost-core`。不要清理来历不明的锁，不要中断其他实例 | `index_status` 能给出新的 generation 时间，或者某个明确晚于 `51fc7ee` 的索引证据；抽查 `nest/rollback.go`、`remoteentity/batch.go` 的 coverage；交接文档 §5 `:115` 更新 |
| **C34** | B22 的后一半：remoteflow 上“真实持久拒绝 + 订阅者”。现在正式链路上新写入能到达的持久拒绝只有 Durability 0 结果未知一种（finalizer `RejectUnresolvedRemoteCommits`，B12 已验证）。要让提交“没到达 Mongo”，需要适配器替身或 Mongo 故障注入；remoteflow 的 `vaultLoader` 不支持卸载，要换成正式 `ManagerAccess` + entitysync。维护者标为“保留，需要 fixture 设计”，按任务要求放在本节。来源：[OPEN-ITEMS C34](OPEN-ITEMS-2026-09-27.md)、[RR-20260926-59 更正节](../bugfix/RR-20260926-59.md) | fixture 设计，规模 M（1～2 天）；需要隔离集成环境 | `codegen/internal/entity/testdata/remoteflow/` 加一个 loader 可卸载的装配和一个 Mongo 写失败注入点，用 `scripts/test-remote-generated.sh` 跑 | 生成链路上构造出 Remote 持久拒绝：实例被卸载，订阅者先收到 remove（或重载后的全量），重载后可写。这条同时能关闭 B27 第 1 批里“生成工程没构造 Remote 持久拒绝”的几条（RR-28 / 39 / 63、RR-20260928-09） |

**B29 分组**（“修前”指修复提交的父提交；括号里是原记录做对照时实际用的基线）：

| 组 | 修复 | 修复提交 → 父提交 | 缺口 | 基准 / 入口 |
| --- | --- | --- | --- | --- |
| A Nest 准入 | [RR-20260926-25](../bugfix/RR-20260926-25.md)、[RR-20260926-47](../bugfix/RR-20260926-47.md) | `a31d0ff` → `28ff15a`；`3577b6f` → `52efb71`（原基线 `49f20ee`） | 微基准已做（无显著差异 / 冷探测 −85%）；“1000 玩家负载下的 Nest 吞吐与尾延迟”没测 | `BenchmarkClientRequestSingle` / `Multi` / `ManagerAccess`、`BenchmarkAdmissionColdProbe`；端到端用 [`scripts/perf/nest-msg.sh`](../../scripts/perf/nest-msg.sh)（10000 实体、1000 producer） |
| B Nest 取锁与事务 | [RR-20260926-48](../bugfix/RR-20260926-48.md)、[RR-20260926-73](../bugfix/RR-20260926-73.md)、[RR-20260926-74](../bugfix/RR-20260926-74.md) | `adc34ae` → `390144b`（原基线 `3e26f91`，load 5～11）；`0b9edac` → `520855b`（未跑基准）；`2074bd1` → `0b9edac`（`-count=3`、没跑 benchstat） | 在空闲机器上重跑并做 benchstat | `BenchmarkHandlerCreateEntity`（memory / state_strict）、`BenchmarkMixedFastSlow`、`BenchmarkCommitLockHold`；[`scripts/perf/nest.sh`](../../scripts/perf/nest.sh) |
| C DataEngine 投影 | [RR-20260926-10](../bug/RR-20260926-10.md) 复核残留 (3)、[RR-20260926-30](../bugfix/RR-20260926-30.md) §5 | `2b2a2f7` → `40804f2`（原基线 `87d9499`；(3) 在 Entity 锁内为每笔事务新增分配、`completeProjection` 每条取 `heldMu` 写锁，从未测过）；`570c407` → `f0d1a1e`（原基线 `cdcb541`，load 4～6，方差 ±16～33%） | RR-10 (3) 没有任何数字；RR-30 只测了 writers_32，而且噪声大 | `BenchmarkProjectorAdmissionMatrix`（async / pipelined 各档 writers）、`BenchmarkProjectorReserveDiscard`、`BenchmarkProjectorWALReplayAckMatrix`；端到端用 [`scripts/perf/dataengine.sh`](../../scripts/perf/dataengine.sh) |
| D DataEngine async + Ack | [RR-20260926-16](../bugfix/RR-20260926-16.md) | 修复提交**待核实**（RR-10～24 登记于 `381efc9`，修复在其后） | macOS 已由 triage 测过；Linux 部分并入上表 C03 | 同 C03 |
| E WAL pipelined 回退 strict | [RR-20260928-11](../bugfix/RR-20260928-11.md) | `b5c655e` → `20a339b`（原基线 `ead6a9d`） | broadcast pipelined 已测（吞吐 −21%，与 strict 持平）；**带 Remote 批次的 pipelined 没有数字**（记录写明“没有该路径的性能数字”） | 端到端：`ROOST_REMOTE_POLICY=pipelined bash scripts/perf/remote.sh`（30m 或更短，新 label），修前 / 修后各一次；微基准 `BenchmarkBroadcastPipelinedCommit` |
| F Sync 字节预算与 RetryLater | [RR-20260926-04](../bugfix/RR-20260926-04.md)、[RR-20260926-05](../bugfix/RR-20260926-05.md) | `73b9e84` → `0882ce0`（原基线 `87d9499`）；`89dde63` → `73b9e84`（原基线 `2b770f6`，load 约 5，方差 ±21～161%） | 微基准已做；**端到端字节预算没压测**（bf-04 写的“sync-aoi 没有字节预算参数”不成立，`scripts/perf/sync-aoi/main.go:61` 有 `-snapshot-bytes`）；RR-05 没有跑 `sync-recovery.sh` | `BenchmarkByteBudgetRecoveryDrain`、`BenchmarkSessionRecovery`、`BenchmarkExhaustedSnapshotWindow`、`BenchmarkManagerFlush`；端到端 [`scripts/perf/sync-aoi.sh`](../../scripts/perf/sync-aoi.sh)` -snapshot-bytes=<N>`、[`scripts/perf/sync-recovery.sh`](../../scripts/perf/sync-recovery.sh) |

**B27 分批**（从 OPEN-ITEMS 的 24 份 RR-20260926 记录出发，加上 RR-20260927 / 28 新报的同类缺口）：

| 批 | 主题 | 覆盖的记录（各自“未验证项”里的那一条） |
| --- | --- | --- |
| 1 | Remote 结果未知 / 持久拒绝（remoteentity 正式 Manager + 真实 Mongo；生成工程里要构造拒绝的部分依赖 C34） | [RR-20260926-27](../bugfix/RR-20260926-27.md)（快 worker 上删除 Remote 实体）、[28](../bugfix/RR-20260926-28.md) §复核更正（生成工程里的 Remote 持久拒绝）、[37](../bugfix/RR-20260926-37.md)（strict 确认超时后的延迟回调、RR-38 吞吐）、[39](../bugfix/RR-20260926-39.md)（持久拒绝后卸载重载）、[63](../bugfix/RR-20260926-63.md)（结果未知后停机 / fence、混合事务的 Remote 部分被拒）、[75](../bugfix/RR-20260926-75.md)、[84](../bugfix/RR-20260926-84.md)（真实 remoteentity + Mongo）；新增：[RR-20260927-09](../bugfix/RR-20260927-09.md)（sid 作用域拒绝没连真实 Mongo）、[RR-20260927-15](../bugfix/RR-20260927-15.md)、[RR-20260927-24](../bugfix/RR-20260927-24.md)（没连真实 Mongo / NATS）、[RR-20260928-09](../bugfix/RR-20260928-09.md)（生成工程里没有 pipelined + Remote 持久拒绝）、[RR-20260928-11](../bugfix/RR-20260928-11.md)（带 Remote 批次的 pipelined 没有真实投影器上的端到端） |
| 2 | 提交后 hook / Close 失败与本地持久链路（dataengine fixture + 真实 WAL group-commit + Mongo） | [RR-20260926-32](../bugfix/RR-20260926-32.md)（提交后 hook panic）、[46](../bugfix/RR-20260926-46.md)（提交后 Close 失败）、[53](../bugfix/RR-20260926-53.md)（提交后 release hook 失败）、[44](../bugfix/RR-20260926-44.md)（真实慢池 + Remote 争用下的快池续行数）、[35](../bugfix/RR-20260926-35.md)（pipelined 新建实体、断电恢复）、[48](../bugfix/RR-20260926-48.md)（交叉创建）、[71](../bugfix/RR-20260926-71.md)、[74](../bugfix/RR-20260926-74.md)、[30](../bugfix/RR-20260926-30.md) §6（game-demo 投影积压超过租约）；新增：[RR-20260927-06](../bugfix/RR-20260927-06.md)（没在 Mongo 投影 / 生成链路上跑）、[RR-20260927-07](../bugfix/RR-20260927-07.md)、[RR-20260927-16](../bugfix/RR-20260927-16.md)（真实 Mongo 上确定性的写失败，例如权限或文档校验） |
| 3 | 冷登录、重连与场景（生成 game-demo + 真实 TCP 客户端 / 机器人） | [RR-20260926-25](../bugfix/RR-20260926-25.md)（进程重启后 gift 补偿、GM AddItem 打到离线玩家）、[36](../bugfix/RR-20260926-36.md)（Mongo 不可用时冷登录挂起 → 超时答复）、[40](../bugfix/RR-20260926-40.md)（真实双连接）、[52](../bugfix/RR-20260926-52.md)、[68](../bugfix/RR-20260926-68.md)（旧 socket 写超时、临界截止）、[55](../bugfix/RR-20260926-55.md)（同一 tick 内断线 + 重连）、[70](../bugfix/RR-20260926-70.md)（卸载 → 重载失败 → 退回 remove → 重新登记）；新增：[RR-20260927-02](../bugfix/RR-20260927-02.md)（真实所有权围栏场景）、[RR-20260927-18](../bugfix/RR-20260927-18.md) / [23](../bugfix/RR-20260927-23.md)（真实 Mongo + lease fence 下场景的重载与 Rebind）、[RR-20260927-03](../bugfix/RR-20260927-03.md)（`run.sh start` 与机器人在非 0 `redis.db` 下跑：批次 7 的 B34 / B38 已实跑过 `run.sh start` 和机器人，但用的是不是非 0 db：**待核实**） |
| 4 | 真实 Redis 续期与键 | [RR-20260926-43](../bugfix/RR-20260926-43.md)（续期 goroutine 观察到失效 → 下一写者立即加锁的时序）；新增：[RR-20260927-17](../bugfix/RR-20260927-17.md)（L2 快照键前缀在真实 Redis 单机 / Cluster 上的表现） |

### 1.2 本地可做、尚未排期（新增）

| 编号 | 内容 | 需要 | 入口 | 完成判据 |
| --- | --- | --- | --- | --- |
| N01 | 发版 HEAD 上没有重跑生成链路与 integration 三项门禁。B28 是在 `001b03b` 上跑的；之后 RR-20260928-08 / 09 / 11 改了 Remote 与 WAL 路径。`51fc7ee` 上只重跑了故障矩阵（21/21）；RR-20260928-11 自报“未跑 `test-remote-generated.sh` 与故障矩阵”。来源：[RR-20260928-11 §未验证项](../bugfix/RR-20260928-11.md)、[RR-20260926-51 §后续验证（B28 门禁）](../bugfix/RR-20260926-51.md)、[交接文档 §5](../CORE-OPTIMIZATION-HANDOFF.md)（`924cc5d` 新增行）。故障矩阵是否已包含这三项：**待核实** | 隔离环境，约 1 小时。`dataengine-env.sh test` 含故障注入，要串行 | `bash scripts/test-dataengine-generated.sh`；`bash scripts/test-remote-generated.sh`；`bash kit/scripts/integration/dataengine-env.sh test` | 三项在 `924cc5d`（或更新的 HEAD）上 exit 0，结果补进交接文档 §5 |
| N02 | 卸载后重载在非默认配置、持续故障下的风暴压测没做。最坏延迟上界只是解析推导。来源：[RR-20260927-13 §未验证项](../bugfix/RR-20260927-13.md)、[RR-20260927-14 §未验证项](../bugfix/RR-20260927-14.md)（都自报“批次 7 / 8 范围”，但 §H 批次 8 的清单里没有它） | 隔离环境，数小时 | 调小 `unload_resync` 的 workers / attempts，让 loader 持续失败，观察积压 gauge `entity.unload_resync.backlog` 与退回 remove 的时间 | 实测最坏延迟不超过 USER_GUIDE 写的精确上界；结果写进 RR-20260927-14 |
| N03 | `glsvet -tests` 在 `nest/group_lock_test.go:134`、`:185`、`:227` 报 3 条“GetEntityGuard called inside a go statement”，修前就有（`ae671a0`）。CI 跑的是 `glsvet ./...`（不带 `-tests`），抓不到。来源：[audit7 §疑点](REVIEW-2026-09-28-audit7.md)；本次在 `51fc7ee` 上实跑复现 | S | `GOWORK=off go run ./cmd/glsvet -tests ./nest` | 0 条；或者在用例里注明这是有意的，并加例外 |
| N04 | nest 包在 `-shuffle=on` 下的跨用例顺序依赖没有专门扫描（RR-20260927-20 只扫了 `-count=N`）。来源：[RR-20260927-20 §未验证项](../bugfix/RR-20260927-20.md) | S | `GOWORK=off go test -count=3 -shuffle=on -v ./nest`，记下 seed | 多个 seed 下都通过，seed 记进 RR-20260927-20 |
| N05 | core-optimization 复审末尾四条旧疑点“OPEN-ITEMS E 节未逐条核对、本次也未核对，不在此关闭”：D1 取消 / 跨段回归、D2 停止补位的严格断言、窗口字节上限命名、九项 Cast / GetMany 只测 mock 且 RR-02 错误语义收紧未列兼容项。来源：[REVIEW-2026-09-26-core-optimization 末尾](REVIEW-2026-09-26-core-optimization.md)。现状：**待核实** | S～M，只读核对 | 逐条对照当前源码与测试 | 每条标注关闭或登记 |
| N06 | `scene_session_reopen_failed_total` 没有加 Grafana 面板，也没有在运行中的进程上从 ops 端点实际抓取过。来源：[RR-20260927-19 §未验证项](../bugfix/RR-20260927-19.md) | S，生成 game-demo + `run.sh start` | 编辑 `deploy/dev/observability/grafana/dashboards/roost-demo.json` 模板，curl ops 的 metrics 端点 | 面板存在，端点里能看到该指标 |
| N07 | compose 停机只验证了“宽限期内正常退出”，没有构造超过 `stop_grace_period`（game 119s）的停机来观察 SIGKILL。来源：[RR-20260926-66 §后续验证（2026-09-27）](../bugfix/RR-20260926-66.md) | S，本机 Docker | 让某个 Mod 的 Stop 阻塞超过 119s，然后 `docker compose stop game` | 记录 SIGKILL 的时间点，以及 WAL / 投影在下次启动时的恢复情况 |

---

## 2. 本地做不了，需要外部环境

| 编号 | 缺什么 | 谁能提供 | 验证方法 | 来源 |
| --- | --- | --- | --- | --- |
| C02 | 真实跨机 / 弱网环境、生产客户端和业务 schema | 部署方（网关 / 客户端团队） | 跨机部署 game-demo 或业务工程，接真实网关，注入延迟与丢包；覆盖 RR-15 / 40 / 52 / 55 各自记录的场景 | [OPEN-ITEMS C02](OPEN-ITEMS-2026-09-27.md)；[RR-20260926-15 维护者决定](../bugfix/RR-20260926-15.md)（C02 保留） |
| C03（断电部分） | 物理断电 / dm-flakey 类故障注入；ext4 `data=writeback`、网络盘、VM 快照上的断电。RR-41 零尾截断、RR-33 fsync 失败（只注入了返回值）、RR-20260928-11 的 fsync 由测试缝与计数判定，都不是断电实验 | 有 root 权限的 Linux 物理机或 VM（运维） | dm-flakey / `echo b > /proc/sysrq-trigger`，在 strict、pipelined 和 group-commit 窗口内断电，重开后核对已确认的记录不丢、checkpoint 不越过 fsync 位置 | [OPEN-ITEMS C03](OPEN-ITEMS-2026-09-27.md)；[RR-20260928-11 §未验证项](../bugfix/RR-20260928-11.md) |
| C04（k8s / systemd 部分） | k8s 集群实际滚动停机、systemd 实际停机。compose 部分已在批次 7 实跑，见 [RR-20260926-66 §后续验证](../bugfix/RR-20260926-66.md) | 集群环境（运维） | 按生成的 k8s README（overlay + `deploy.sh`）部署，滚动发布时核对 `terminationGracePeriodSeconds` 内正常退出、无 SIGKILL | [OPEN-ITEMS C04](OPEN-ITEMS-2026-09-27.md) |
| N10 | 真 systemd 语义。现在都用替身近似：`WorkingDirectory` 是否跟随 `current` 链接、`ProtectSystem=strict` / `ReadWritePaths` 的挂载语义、`daemon-reload` 之后 `stop` 用哪份 unit 的 `TimeoutStopSec`、`Restart=on-failure` 的崩溃重启循环、`useradd` / `install -o/-g` 属主、GNU `mv -T`。另外 install.sh 从没在 Linux 上跑过 | 一台带 systemd 的 Linux 机器（可以是 VM） | 用生成的 `deploy/shell/install.sh` / `rollback.sh` 做真实安装 → 升级 → 自动回滚 → 手工回滚；核对 stats_log 落盘、`configs/data` 可读、停机时限 | [RR-20260928-04](../bugfix/RR-20260928-04.md)、[05](../bugfix/RR-20260928-05.md)、[10](../bugfix/RR-20260928-10.md)、[12](../bugfix/RR-20260928-12.md) §未验证项 |
| N11 | 真实 distroless 基础镜像和多阶段 `golang` 构建没跑过（本机没有这些镜像，按规则不拉取；运行阶段的 `COPY` 行执行过） | 允许拉取镜像的环境（CI 或有 registry 访问的机器），或者维护者允许本机拉取 | 用生成的 Dockerfile `docker build`，compose 起 10 个服务到 healthy，核对 `configs/data` 与 `log` 目录 | [RR-20260927-34 §未验证项](../bugfix/RR-20260927-34.md)、[RR-20260928-04 §未验证项](../bugfix/RR-20260928-04.md) |
| N12 | k8s 上的部署物行为：emptyDir 可写性与 sizeLimit 驱逐、ConfigMap 覆盖 + Reload、Secret 示例（含 `saga` / `player_access` / `game_route` 等段）能否让服务起来 | 集群环境 | 按 `deploy/k8s/base` + overlay 部署 game-demo，逐服务核对 Init 成功、stats_log 落盘、Secret 内嵌 config 生效 | [RR-20260927-34](../bugfix/RR-20260927-34.md)、[RR-20260928-04](../bugfix/RR-20260928-04.md)、[06](../bugfix/RR-20260928-06.md)、[07](../bugfix/RR-20260928-07.md) §未验证项 |
| N13 | doctor 读不到仓库外的生产配置。已生成工程的 prod 示例、Secret 示例和仓库外真正在用的配置，都要手工补段（`project sync` 不更新脚手架） | 部署方提供生产配置的位置，并自行合并 | 对生产配置跑 `roost project doctor`（或把配置拷回仓内），核对 `shutdown:*` 与各段完整 | [RR-20260927-04 §未验证项](../bugfix/RR-20260927-04.md)、[RR-20260928-06 §兼容性](../bugfix/RR-20260928-06.md)、[RR-20260928-07 §兼容性](../bugfix/RR-20260928-07.md) |
| N14 | 真正的 Windows `core.autocrlf=true` 检出（RR-20260928-13 是用 `\n → \r\n` 转换模拟的） | Windows 机器（CI 的 windows job 可以加一步） | 在 autocrlf 检出上跑 `add transport tcp` / `add mod` / `project sync`，核对 Secret 示例被编辑、行尾保持 CRLF | [RR-20260928-13 §未验证](../bugfix/RR-20260928-13.md) |
| N15 | 生产负载下各类竞态窗口的实际发生频率（RR-20260927-22 / 28 的窗口只用测试缝确定性进入过） | 生产或准生产流量 | 在对应分支加计数或日志后观察 | [RR-20260927-22](../bugfix/RR-20260927-22.md)、[RR-20260927-28](../bugfix/RR-20260927-28.md) §未验证项 |

---

## 3. 需要设计或维护者进一步决定

| 编号 | 问题 | 现状（`51fc7ee`） | 要决定什么 | 来源 |
| --- | --- | --- | --- | --- |
| C12 | 生成 DAO 的库名写死 `"game"`（DAO 与 demo 发号器集合） | `codegen/internal/dao/gen.go:333` 生成 `<Dao>DBName`；golden 在 `codegen/internal/dao/testdata/golden/gen_hero_dao.go:37` | 维护者已定“保留为 feature 需求”：库名可配置会改变生成形状并涉及数据迁移，需要设计 | [OPEN-ITEMS C12](OPEN-ITEMS-2026-09-27.md)；[RR-20260926-56 维护者决定](../bugfix/RR-20260926-56.md) |
| C20 | Direct：RR-70 交还时不带会话 lifetime，同 ID 关闭重开的微秒窗口里重新提交可能落到新连接；重新提交已取出、Subscribe 落下之前同 ID 关闭又重开，重开的连接没有 Bind 却被订阅（修前修后相同） | `policy.NewDirect`（`sync/entitysync/policy/direct.go:40`）没有非测试调用方 | 维护者已定“保留到 Direct 有真实使用方时再定” | [OPEN-ITEMS C20](OPEN-ITEMS-2026-09-27.md)；[RR-20260926-79](../bugfix/RR-20260926-79.md)、[85](../bugfix/RR-20260926-85.md) 维护者决定；[audit5 §疑点](REVIEW-2026-09-27-audit5.md) |
| C21 | “重连恢复”的定义：旧会话先 Close 再 Open 的订阅全部算新入场，只有同一 lifetime 的 Hold→Ready 算恢复 | 维护者已接受当前语义（`sync/README.md` 已写明） | 业务方确认是否需要跨会话恢复；需要的话要设计新协议（原记录写的是“需业务确认”） | [OPEN-ITEMS C21](OPEN-ITEMS-2026-09-27.md)；[core-optimization 维护者决定](REVIEW-2026-09-26-core-optimization.md) |
| C22 | periodic 只配字节预算时仍会捕获全部候选（编码前不知道精确大小）；跨 tick 内容缓存需要版本与内存预算设计 | 维护者已定“保留为设计题” | 是否做跨 tick 缓存、按什么内存预算 | [OPEN-ITEMS C22](OPEN-ITEMS-2026-09-27.md)；[RR-20260926-04 维护者决定](../bugfix/RR-20260926-04.md) |
| C24 | 跨进程所有权移交：RR-30 契约点 4（WAL 没排空就迁走所有权）没有纳入框架，仍靠 demo 的 `Admit` 与 RR-31 约定；两进程端到端没做 | 维护者已定“保留在 feature 线” | 移交协议设计（CARRYOVER A6 / A7） | [OPEN-ITEMS C24](OPEN-ITEMS-2026-09-27.md)；[RR-20260926-30](../bugfix/RR-20260926-30.md)、[31](../bugfix/RR-20260926-31.md) 维护者决定 |
| N20 | RR-20260927-06 从检查到使用的时间窗：`refuseCommitAfterFence` 只在交给 committer 之前读一次 `FenceError`（`nest/rollback.go:602`、`:753`，函数在 `:690-699`）。如果别的 goroutine 在检查之后、Commit 之前 fence 了引擎，这笔提交不会被拦；嵌套场景是同一 goroutine，时序是确定的。另外 fence 原因用 `%v` 写进错误，不能再 `errors.As` 取到——这一点是有意的：`:688` 的注释说明，原因链上的 `ErrCommitIndeterminate` 会让调用方误走结果未知分支 | 按一次读取实现，audit6 记为“疑点，未登记” | 是否收紧（例如把检查放进 committer 的临界区，或者依赖 WAL terminal 兜底），还是接受为一次性检查并写进契约 | [audit6 §疑点](REVIEW-2026-09-27-audit6.md) |
| N21 | RR-20260927-32 疑点：带 Remote 批次、**没有 effect** 的 memory handler 不经 `durableCommit`，fence 检查不适用；别的消息 fence 引擎之后，它仍会写 Remote（`nest/msg.go:72` `finishRemoteWriteBatch`，Durability 0 直接写权威） | 修复方声明为范围外，audit7 复述了这一点 | 引擎 fence 之后，Durability 0 的 Remote 写是否也要拒绝（返回 `ErrNestFenced` 并 Abort 批次） | [RR-20260927-32 §未验证项 / 风险](../bugfix/RR-20260927-32.md)；[audit7 §疑点](REVIEW-2026-09-28-audit7.md) |
| N22 | RR-20260927-32 的另一半：外层没有持久记录时，业务吞掉嵌套事务的结果未知，得到成功回复（判别表第 1 行已按实际行为写明） | 未改 | 这种回复要不要也带哨兵（新决定） | [RR-20260927-32 §未验证项 / 风险](../bugfix/RR-20260927-32.md) |
| N23 | RR-20260928-13 残留：`appendModConfigSections`（`codegen/internal/roost/render.go:589`，调用方在 `add.go:78`、`:241`）对 CRLF 的开发配置和生产示例仍用 LF 追加新段，造成行尾混用（修前就有）。YAML 解析不受影响；下一次停机块刷新或 `ensurePlayerTCPConfig` 会整份写回 CRLF | 未改 | 是否让追加时沿用文件原有的行尾 | [RR-20260928-13 §未验证 / 风险](../bugfix/RR-20260928-13.md) |
| N24 | 生成工程 CI 只跑 `docker compose config --quiet`，“语法合法、语义错”的部署物问题（例如 RR-20260927-33 的 tmpfs）CI 抓不到 | 靠生成器回归按 YAML 结构断言 | 是否在生成 CI 里加一步结构检查，或者实际 `compose up`（修复记录写明“需要维护者决定”） | [RR-20260927-33 §未验证项 / 风险](../bugfix/RR-20260927-33.md) |
| N25 | 其余没带部署前缀的 Redis 键：非 authority 兼容装配的 `remote_entity:marks`（`remoteentity/marker.go:96`，`NewRedisMarker` 已有 key 参数）、锁键 `remote_entity.lock_key`（`kit/remoteentity/remote_entity_mod.go:74`） | RR-20260927-17 只给 L2 快照键加了前缀 | 是否同样给这两个键加可选前缀（与 C11 同类） | [RR-20260927-17 §未验证项 / 风险](../bugfix/RR-20260927-17.md) |
| N26 | `SetEntityVersion`（`entity/entity_remote.go:46`）仍能不做检查就改写 StateVersion（生成实体的 `AdvanceVersion` 等） | RR-20260927-15 只在同一 fence 下拒绝回退，不管这个入口 | 是否给这个入口也加同 fence 不回退的检查 | [RR-20260927-15 §未验证项 / 风险](../bugfix/RR-20260927-15.md) |
| N27 | Guard 的 `eMap` 按 ID 记账（`entity/entity_guard.go:84`）：同一 Guard 先持有 Manager A 上的 X、再给 Manager B 上同 ID 的 X 取锁时，按“同 ID 换了实例”处理；锁序判断同样按 `eMap` 条目 | RR-20260928-02 只改了撤销记录的键，这一处“不扩大也不收窄”，没补回归 | 跨 Manager 同 ID 在一个 handler 里共存时，锁账本语义是否要收紧（要收紧就另行登记） | [RR-20260928-02 §Guard eMap 一节、§未验证项](../bugfix/RR-20260928-02.md) |
| N28 | 广播 handler 里 Destroy 后同 ID 重建：`broadcastDispatch` 按 ID 逐个释放实体（`nest/nest_dispatch.go:473`、`:537`），要么放掉新实例，要么新实例的锁一直留到整轮广播结束——两种都有一把锁跨后续广播目标持有（修前就是这个形状）。Remote 实体的 `ReleaseCast` 没有单独回归 | 未改，未构造回归 | 广播路径是否改为按实例释放，并且在单个目标结束时释放 | [RR-20260927-26 §未验证项 / 风险](../bugfix/RR-20260927-26.md) |
| N29 | `holding` 里对实体接口值直接比较 `current == ent`（`entity/entity_guard.go:325`）：值类型且不可比较的实体实现会 panic（RR-25 / 30 只处理了 Mutex）。仓内和生成物中的实体都是指针 | 没构造回归 | 改为可比较性安全的比较，或者在契约里写明“实体必须是指针” | [RR-20260927-25 §未验证项 / 风险](../bugfix/RR-20260927-25.md) |
| N30 | 统计文件（stats_log）没有轮转（原来就没有）；长期运行的 compose / systemd 需要运维配 logrotate（`copytruncate`，文件以 `O_APPEND` 打开）。DEPLOYMENT 与生成的 README 里都没写（`rg logrotate` 无命中） | 未改、未写文档 | 做内建轮转，还是在部署文档里写明 logrotate 要求 | [RR-20260928-04 §未验证项 / 风险](../bugfix/RR-20260928-04.md) |
| N31 | 审计疑点：`Scene.Close` 由 `Service.Shutdown` 用 `context.Background()` 调用（`demo/internal/service/game/service.go.tmpl:293`），停止重载不受 App 停机时限约束。worker 在 ctx 取消后返回、Nest 仍在运行，正常情况下有界；修前就有 | audit6 记为疑点、未登记 | 是否改用 App 给的停机 ctx | [audit6 §疑点](REVIEW-2026-09-27-audit6.md) |
| N32 | 审计疑点：B40 回归 `state_strict` 子用例的 `decided` 计的是 Create 返回的总次数，不是“每个 follower 至少一次”，占满快池这个前提因此变弱（不会误报） | 未改 | 是否加强断言 | [audit6 §疑点](REVIEW-2026-09-27-audit6.md) |

---

## 4. 已知残留风险（已接受或在契约内）

只列 RR-20260927-* / RR-20260928-* 记录里新接受的边界。更早的 39 条见 [OPEN-ITEMS §D](OPEN-ITEMS-2026-09-27.md)，本节不重复。

| 编号 | 边界 | 依据 | 来源 |
| --- | --- | --- | --- |
| N40 | demo 玩家 id 计数器：新旧版本混跑的升级窗口可能发出重复 id（只在 README 写“先全停再升级”，代码不防）；回滚会重发 id；`-redis-password` 以命令行参数传给 accountctl，本机其他用户能在 `ps` 里看到；密码只按“整行、去一层引号”解析 | demo/README 写了升级与回滚步骤；audit6 的并发升级探针 300 轮无重复；密码可见只影响开发脚本，按清单推荐接受（audit6 复述） | [RR-20260927-03 §未验证项、§后续验证](../bugfix/RR-20260927-03.md)；[audit6 §疑点](REVIEW-2026-09-27-audit6.md) |
| N41 | `shutdown.total_timeout` 写成不带单位的整数（例如 `60`）时，viper 读成 60ns，App 不兜底 | doctor 的 `time.ParseDuration` 会报非法时长（dev 配置 FAIL、示例配置 WARN），方向正确 | [RR-20260927-04 §未验证项 / 风险](../bugfix/RR-20260927-04.md) |
| N42 | 生成的 `shutdown:` 段注释只举 dataengine 30s 一例，却写着“40s declared”；`project sync` 的公式按生成值计入 player TCP 10s，调大 `player_access.tcp.shutdown_timeout` 时要手动调大 total | 改注释文字会让 sync 认不出没手改过的旧段（按现有机制保留，audit6 同意）；公式不读配置，与 D11 同理，doctor 会提示 | [RR-20260927-05 §决策、§未验证项](../bugfix/RR-20260927-05.md)；[audit6 §疑点](REVIEW-2026-09-27-audit6.md) |
| N43 | 引擎 fence 之后再开的嵌套独立事务照常提交；RollbackNone 事务（带 Remote 批次、emit 了 effect 的 memory handler）在 fence 后被拒时，内存修改不撤销 | C07 的约束只拦外层自己的提交；audit6 判定“设计内”；内存不撤销沿用 RR-64 / 73 的 memory 语义，USER_GUIDE 已按实际行为更正 | [RR-20260927-06 §未验证项、§后续更正](../bugfix/RR-20260927-06.md)；[audit6 §疑点](REVIEW-2026-09-27-audit6.md) |
| N44 | 外层 undo 事务里业务用 `RecordUndo` 撤销非 DAO 数据，而嵌套事务持久写的正是这份数据：框架识别不了 | 生成代码的持久写都经过 DAO；RR-74 已列为同一边界 | [RR-20260927-07 §未验证项 / 风险](../bugfix/RR-20260927-07.md) |
| N45 | `ValidateEntityRegistry` 在启动期实例化托管 kind 的 DAO 工厂（与 Assemble 相同，只一次）；如果业务的 DAO 工厂在启动期有副作用，需要业务自查 | 修复记录写明；拒绝点放在 `FinalizeLocked` 的论证经 audit6 认可 | [RR-20260927-09 §未验证项 / 风险](../bugfix/RR-20260927-09.md) |
| N46 | 生成配置里 `unload_resync` 的注释给的是近似式（略去退避和“+workers”一轮），精确式只在 USER_GUIDE | 修复记录写明精确式所在位置 | [RR-20260927-14 §未验证项 / 风险](../bugfix/RR-20260927-14.md) |
| N47 | demo 场景的新行为（RR-18 卸载后重载、RR-19 计数、RR-23 `OnEntityLoaded → Rebind`）在已生成工程上**不会**随 `project sync` 更新：`scene.go` / `scene_test.go` 是只写一次的脚手架，要按新模板手工合并 | audit6 更正了原记录，CHANGELOG 已同步 | [RR-20260927-18](../bugfix/RR-20260927-18.md)、[19](../bugfix/RR-20260927-19.md)、[23](../bugfix/RR-20260927-23.md) §更正 |
| N48 | 已生成工程的 prod 示例和 k8s Secret 示例缺的段（`game_route` / `activity` / `platform`、`saga` / `player_access`）不会被 `project sync` 补上，要手工合并；`saga.stream_max_bytes` 按目标 JetStream 容量调小（默认 8 GiB） | 两份都是脚手架（非受控文件）；修复记录给了手工合并清单，audit7 核对旧工程 sync 后只差这几份脚手架 | [RR-20260928-06](../bugfix/RR-20260928-06.md)、[07](../bugfix/RR-20260928-07.md) §兼容性；[audit7 §修复核验结论](REVIEW-2026-09-28-audit7.md) |
| N49 | RR-21 的“本 Guard 自己撤销”只覆盖嵌套 `RunIsolatedTransaction` 这一种来源；`revokeCreated` 的其他调用发生在 handler 返回之后，那时 handler 内不会再有 `Create` | 修复记录的时序论证 | [RR-20260927-21 §未验证项 / 风险](../bugfix/RR-20260927-21.md) |
| N50 | 自定义 `RemoteWriteBatch` 或自定义 Remote 实现如果在“没等到结论 / 结果未知”时不带 `entity.ErrRemotePersistenceIndeterminate`，回复会被归到第 4 行“Remote 部分被拒绝”；框架替它区分不了 | 契约要求必须带，已写进 USER_GUIDE 判别表；godoc 两个哨兵都列 | [RR-20260927-24 §未验证项](../bugfix/RR-20260927-24.md)；[RR-20260928-03](../bugfix/RR-20260928-03.md)、[08](../bugfix/RR-20260928-08.md) §未验证项 / 风险 |
| N51 | `nest.ErrNestCanceled` 同时用于“未执行”（`validateClientDispatch`、派发入口时 ctx 已取消）和“等待截止”，判别表第 14 行统一按“结果未知、不得据此重试”处理 | 主会话合并时的补修按表后既有语义定为第 14 行；audit7 判定“偏保守，可接受” | [RR-20260928-03 §合并时补修](../bugfix/RR-20260928-03.md)；[audit7 §疑点](REVIEW-2026-09-28-audit7.md) |
| N52 | 自定义 Mutex 如果多个实例内部共用同一把不可重入锁，同一 Guard 给第二个实例加锁会自锁；共享加载时，交回领头方执行的发布看到的是领头方 goroutine 的 fctx（`BaseContext` 受领头方截止约束）；领头方离开后的发布若要锁领头方持有的锁，要等领头方放锁 | 框架无法判定两个 Mutex 是否同一把锁，`sameMutex` 注释已写明；后两条与 RR-54 之前的行为相同 | [RR-20260927-25](../bugfix/RR-20260927-25.md)、[27](../bugfix/RR-20260927-27.md)、[30](../bugfix/RR-20260927-30.md) §未验证项 / 风险 |
| N53 | 卸载重载积压 gauge 按接线汇总：如果将来某个接线没调 `stop` 就被丢弃，它的份额会一直留在总和里 | 现在唯一的接线来源（kit Nest Mod）停止时会调 `stop`；`ConfigureUnloadResync` 只替换已停止的接线 | [RR-20260928-01 §未验证项 / 风险](../bugfix/RR-20260928-01.md) |
| N54 | shell / systemd 回滚：更早安装、当时不是 `current` 的 release 没有 unit 记录，回退到它仍用当时的 unit（`WorkingDirectory=$APP_ROOT`，没有 `configs/data`） | RR-20260928-10 修复后只剩这一种限制，记录写明；audit8 的演练覆盖了“无记录 release” | [RR-20260928-05 §后续](../bugfix/RR-20260928-05.md)；[audit8 §修复核验结论](REVIEW-2026-09-28-audit8.md) |
| N55 | broadcast pipelined 在修复后吞吐 −21%、p50 +28%、p99 +13%（与 broadcast strict 没有显著差异） | 文档承诺 broadcast / 带 Remote 批次的 pipelined “等同 Strict”；修前更快是因为没等 fsync，不是可以保留的收益；audit8 同机 A/B 复跑方向一致 | [RR-20260928-11 §性能](../bugfix/RR-20260928-11.md)；[audit8](REVIEW-2026-09-28-audit8.md) |
| N56 | RR-20260928-13 对认不出结构的 k8s Secret 示例 统一处理为“stderr 打 WARN、不改 Secret、命令照常完成”，不让命令失败 | 修复记录“为什么统一成 WARN”一节的论证；audit8 处理结果认可 | [RR-20260928-13 §修复](../bugfix/RR-20260928-13.md)；[audit8 §处理结果](REVIEW-2026-09-28-audit8.md) |
| N57 | Remote L2 快照键的部署前缀要显式配置 `remote_entity.snapshot_l2_key_prefix`，缺省为空、键不变；不配置时，同一 Redis db 上的多份部署仍共用 `remote_entity:snapshot:*` | kit 没有一个在缺省下既能保持键不变、又能区分部署的前缀，论证见修复记录；audit6 判定“偏离清单候选，论证成立” | [RR-20260927-17 §决策](../bugfix/RR-20260927-17.md)；[audit6 §修复核验结论](REVIEW-2026-09-27-audit6.md) |

---

## 5. 观察项（只能等再次出现）

| 编号 | 内容 | 出现时怎么做 | 来源 |
| --- | --- | --- | --- |
| C23 | 历史 FlushFailures=1 的根因没有证明；RR-20260926-01 只修了“失败原因要留下”（LastError 与阶段）。原 main 4 次复跑都是 0 | 按 LastError 与阶段定位，然后登记 RR | [OPEN-ITEMS C23](OPEN-ITEMS-2026-09-27.md)；[RR-20260926-01 维护者决定](../bug/RR-20260926-01.md)；[交接文档 §5](../CORE-OPTIMIZATION-HANDOFF.md) `:103` |
| N60 | B46：`TestRejectedWriteUnloadsOnNestFastPoolAndRebindsSync` 在 CI linux-test shard 1 偶发一次（run `36364494353`），已按契约改成有界重试 `ErrRemoteEntityReloading`（`51fc7ee`）；本地 300 次 + race 200 次没复现 | 如果改后还出现，看拿到的是不是别的错误（辅助函数对其他错误仍判失败） | [OPEN-ITEMS B46](OPEN-ITEMS-2026-09-27.md) |
| N61 | B40 / B44 两条用例的时序问题已修测试；B08 回归修前约 1/100 超时 | 高负载整包 `-race` 下如果再次出现，说明钉住的同步点还不够 | [OPEN-ITEMS §H 批次 12、14](OPEN-ITEMS-2026-09-27.md) |
| N62 | 生成 TCP 测试的“认证 ack 先于 `registerSession`”时序：RR-20260927-02 给 `TestCloseSessionsCountsTheSessionsItClosed` 加了等待，同一文件里其他断言 `ActiveSessions` 的用例没有加（见 §6 N70） | 同形失败再出现时，逐个补上等待 | [RR-20260927-02 §后续更正](../bugfix/RR-20260927-02.md) |

---

## 6. 发版后需要确认的 CI

2026-09-28 本次核对（`gh run list` / `gh run view`）。主会话提交前复核：`924cc5d` 上 main 与 tag `v1.17.2` 的 ci 均 success，security / demo-publish / upgrade-compat success；framework-compat 仍 failure（released / minimum 两条 demo lane 已转绿，失败为 N70）。

| 编号 | 内容 | 现状 | 判据 |
| --- | --- | --- | --- |
| CI-1 | framework-compat 的 released / minimum demo 两条 lane：生成 demo 的测试用到了 v1.17.1 没有的 `kitsyncbus.JetStreamStreamFromConfig`（RR-20260927-35），要发布 v1.17.2 并把生成器下限升到 v1.17.2 才能转绿 | `51fc7ee` 的 run `36365082217`：`released, demo` 与 `minimum, demo` 失败。v1.17.2 发布后，`924cc5d` 的 run `36367611936`：这两条已**转绿**，但 **`source-head, full` 失败**（见 N70），所以整个 workflow 仍是 failure | framework-compat 9 个 job 全部 success |
| N70 | **新失败（发版后才出现）**：framework-compat `generated-consumer (source-head, full)` 里生成工程的 `TestAConnectionThatCannotTakeAPushIsClosedAndTheOthersKeepIt` 报 `server_gen_test.go:556: active sessions = 1, want 2`。生成器源码 `codegen/internal/roost/render_player_tcp.go:1563-1569` 在 `waitReleased` 之后立即断言 `ActiveSessions(7) == 2`，没有像 `TestCloseSessionsCountsTheSessionsItClosed`（`:1657` 起）那样等两个会话都登记。**推断**：与 [RR-20260927-02 §后续更正](../bugfix/RR-20260927-02.md) 同一原因——服务器先写认证 ack、后 `registerSession`。只改测试，不涉及产品行为 | 只在 run `36367611936` 出现一次（`51fc7ee` 的同一 lane 通过），**待复现** | 生成测试加上同样的有界等待；生成工程里 `-race -count=200` 跑该用例通过；framework-compat 转绿 |
| CI-2 | ci 的 linux-test 分片偶发（B46）是否再现 | `51fc7ee` 的 ci run `36365082237` 全部 success（linux-test 0～3、windows-compatibility、linux-quality、release-hygiene、service-redis）。本次核对时，`924cc5d` 上 main 的 run `36367611929` 与 tag v1.17.2 的 run `36367614024` 都还在运行 | 连续若干次 main / tag 的 ci 中 linux-test 全绿；再现则按 §5 N60 处理 |
| CI-3 | windows-compatibility 超时（RR-20260928-14）是否真的解决 | `7b5e5a4` 起转绿，`codegen/internal/roost` 在 Windows 上 224s | 包耗时持续明显低于 10 分钟默认超时；如果回到接近 10 分钟，按 [RR-20260928-14 §未验证项](../bugfix/RR-20260928-14.md)给 windows job 加 `-timeout` 并调大 `timeout-minutes` |

## 7. 进度（2026-09-28 起按维护者安排执行）

维护者 2026-09-28 决定：优先 5 条中 **C03 不做**，**C01 最后做**，其余（N70、B30、B27 第 1 批 + C34）先做。

| 条目 | 状态 |
| --- | --- |
| N70（及 N62） | **已修复**：[RR-20260928-15](../bugfix/RR-20260928-15.md)（`0516670`）——生成的 player TCP / scene 测试先等会话登记；延迟探针确定根因，并行负载下修前 1/500 失败、修后 500 次通过 |
| C34 | **已完成**（`21a3171`）：remoteflow 生成链路改用正式 `ManagerAccess` + entitysync，加写失败注入点，构造出 Durability 0～3 的 Remote 持久拒绝：实例仅内存卸载、订阅者收到权威全量（无权威时 remove）、重载后可写、被拒修改未写出；负对照成立 |
| B27 第 1 批 | **已完成**（`911dbec`）：13 份记录中 10 份在生成链路补了端到端用例、均无缺陷；RR-20260927-09（需独立 fixture 包）、RR-20260927-15（正式装配不可达）、RR-20260926-63 的混合事务 Remote 部分拒绝（正式链路构造不出）写明原因；各记录已追加“后续验证” |
| B30 | **未通过，调查中**：`6b73289` 正式装配 80 TPS × 30 分钟 completed 143991、CompletionTPS 79.99、p50 / p95 / p99 133 / 207 / 314ms、max 1860ms、Dropped 0、投影失败 0，但 **Errors 9**（全部 `remote entity: capacity exceeded`，`ElapsedMS 0`，集中在约 1680s），最终一致性未核验（`artifacts/perf/remote/b30-sustained-80-6b73289`）；阶梯首档 80 TPS × 120s 同样 18 个（p99 1330ms），阶梯停止。疑为延迟尖峰时准入额度（写许可 128 / tracker）被占满，是否为本轮回归待与 v1.17.1 同参数对照 |
| C01 | 待 B30 之后 |
| C03 | 维护者决定不做 |

B27 第 1 批的观察：生成链路上 `Nest.Request` 的调用方与 Remote 确认等待共用同一请求 ctx，调用方总是先拿到判别表第 14 行（`nest: sync canceled` + `DeadlineExceeded`），RR-20260927-24 新加的第 2 行哨兵送不到调用方；两行结论同为“结果未知”，已写入 RR-37 / RR-20260927-24 记录。
