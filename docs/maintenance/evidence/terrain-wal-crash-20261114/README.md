# 地形仅 WAL 确认后的 SIGKILL 恢复（B-11）

2026-11-14，macOS arm64，基线 `feat/v147` 的 `086524b3`，实施分支 `tmp/v147-terrain`。不改生产持久语义；没有协议、DAO schema 或 WAL 格式变化。新生成的 game-demo 带本用例。

## 覆盖的事实

`demo/internal/service/game/terrain_wal_crash_test.go.tmpl` 的 `TestTerrainWALCrashRealMongo` 使用正式生成的 TerrainEdit / BuildingPlace handler、Sender、DAO、Lifecycle、Nest strict 提交与真实文件 WAL / MongoStore / Runtime。

子进程用已有的 `ProjectorOptions.ManualReplay` 禁止后台投影（新建实体也只在 WAL）；完成一笔业务后通过专用管道交出确认快照。父进程收到确认、检查 Mongo 后才发 `SIGKILL`，核实 `WaitStatus.Signal()==SIGKILL`，不会调用子进程的 Shutdown。恢复在父进程里创建全新的 DataEngine / EntityManager / Map，同一 WAL 目录的启动恢复屏障投影后才冷加载。

| 场景 | 崩溃前 | 恢复断言 |
| --- | --- | --- |
| `brush` | 四块交点的一笔抬高；6 条追加、6 次 fsync、0 ack | 全图 6 块版本与全部高度/材质摘要一致，4 块版本为 1；账本 LastSeq=1；同序号重试 Duplicate 且块不变 |
| `building` | 跨两块的 hall 放置并整平；5 条追加、5 次 fsync、0 ack | 全图版本/摘要一致；站立建筑的完整 State、配置、参数、属主、地基、Lot 一致；48 个占位点及四边界外逐点核对；账本 LastSeq=1、Buildings=1；重试 Duplicate |

两种场景都在确认后、SIGKILL 后分别检查 `terrain_chunks` / `terrain_editors` / `buildings` **全部零文档**，避免用先前 Mongo 投影冒充 WAL 恢复。

这是一条服务端持久恢复验收。没有接真实 TCP / 客户端重连，没有测混合同步网络交付、Linux、跨机或性能；不把它扩写为这些范围的验收。

## 运行与反例

新用例登记在 `codegen/internal/roost/demo.go`，实际入口是 `scripts/test-demo-generated.sh`，根包 `integration_coverage_test.go` 新增登记。

私有环境 `/private/tmp/v147-terrain-deps-37500`，offset 37500；没有 reset 共享环境。offset 37000 首次启动遇到外部 NATS 占端口后，只 down 本任务已起的 Mongo，改用检查后的空闲端口。命令（不要打印 env 内容）：

```sh
ROOST_MIRROR_LOCAL_HOME=/private/tmp/v147-terrain-deps-37500 \
ROOST_MIRROR_LOCAL_OFFSET=37500 bash scripts/mirror-local.sh up
source /private/tmp/v147-terrain-deps-37500/roost-dataengine-it/env.sh
GOWORK=off GOFLAGS=-p=2 bash scripts/test-demo-generated.sh
```

定向入口在生成的 game-demo 内：

```sh
ROOST_TERRAIN_IT_MONGO_URI="$ROOST_DATAENGINE_IT_MONGO_URI" \
GOWORK=off GOFLAGS=-p=2 go test -mod=mod -race -count=1 -timeout 5m -v \
  -run '^TestTerrainWALCrashRealMongo$' ./internal/service/game
```

- 首次正确夹具运行：两场景通过，测试 1.49s（[green.log](green.log)）。
- 丢 WAL 变异：只在一次性生成工程把父进程的 `walDir := filepath.Join(dir, "wal")` 改成 `filepath.Join(dir, "lost-wal")`，子进程仍写原 WAL。两场景都在 `WAL recovery lost acknowledged terrain` 失败，已确认的版本 1 变为 0（[mutation-lost-wal-red.log](mutation-lost-wal-red.log)）。不删原 WAL，不改变断言；变异后恢复文件。
- 夹具开发阶段一次错误断言保留在 [fixture-durable-lsn-red.log](fixture-durable-lsn-red.log)：把仅供 pipelined 票据使用的 `DurableLSN` 当成 strict 的水位，实际为 0；依据 `framework/nestwal/wal.go` 契约改为检查 strict 成功返回、所有追加完成、fsync 非零、ack=0。这不是框架缺陷，也不作为修前行为证据。
- 完整真实脚本与最终检查结果见本报告末尾；没有用仅编译或“没有测试可运行”替代实际执行。

## 同批删除重复分配器

`demo/game/controllers/player/controller.go.tmpl` 删除没有调用方的 `ids`、`runtimeid.New`、`RuntimeIDs()` 与 import。同 sid 的 spawner 是当前唯一正式运行期分配器；不新建共享抽象。仓内 `demo` / `modules` 与全仓 Go/template 的 `RuntimeIDs(` 搜索没有调用，生成工程编译覆盖删除后的调用方。消费者需重新生成 game-demo；外部手改工程若调用过这个未使用接口，应使用业务所属的唯一分配器，不能另建同 sid 分配器。

## 图谱证据边界

Tier 2，沿用同基线父索引 `Users-whb-roost-worktrees-v147-native-acceptance`，generation `2026-11-14T02:19:08Z`。模板函数不可见于 graph，相关 partial ranges 已由本 worktree 精确源码补读；新模板不在父索引，按当前源码和真实生成运行核实。`NewProjector` 的 exact snippet / outbound trace 确认 ManualReplay 不启动后台循环；覆盖检查没有在 projector、runtime、entity_create、wal、controller 与生成清单记录缺口。这些结果不声称父索引已含本次修改。

## 提供给最终 KNOWN-LIMITS 文档批

仅关闭 B-11 的“地形编辑/放建筑仅 WAL 确认后崩溃恢复未验收”：

> 2026-11-14 已在 macOS 私有 Mongo 副本集上验收：正式生成 game-demo 的 `TestTerrainWALCrashRealMongo`，跨四块笔刷与跨两块整平建筑仅 WAL strict 确认、Mongo 业务集合仍空时 SIGKILL；同 WAL 恢复后块版本/全部数据摘要、建筑、占位与编辑账本一致，重放不重复执行。恢复丢 WAL 的变异两场景均红；入口 `scripts/test-demo-generated.sh`。未覆盖真实客户端重连、Linux、跨机与长稳，不据此关闭这些外部验收项。

删除小件尾项“controllers/player 与 spawner 各有同 sid runtimeid allocator”：未使用的 controller 分配器与 `RuntimeIDs()` 已移除，spawner 保持原唯一分配器。

## 最终验证与清理

- **实际完整脚本退出 0**：`GOWORK=off GOFLAGS=-p=2 bash scripts/test-demo-generated.sh`，联盟、purchase、mailbox（core / JetStream）、activity、player 排行、迁移、旧地形重启三条、新 SIGKILL 两场景、Gate、原生 Saga/effect 全通过，零 SKIP；[脚本结果摘录](demo-script-summary.log)。新 SIGKILL 用例 1.47s，Gate 74.25s，Saga/effect 19.66s。
- 第一次完整脚本在联盟入口因 NATS 不存活失败（[原始错误](environment-red.log)）。`up` 所在工具执行会话结束后，NATS / Redis 子进程不再存在，Mongo 的 daemon 仍在；重新在**同一 shell 会话**执行 `up → source env.sh → test-demo-generated.sh → down` 后通过。没有改断言或跳过任何用例。
- `GOWORK=off GOFLAGS=-p=2 go test -count=1 -run '^(TestGeneratedProjectRealDependencyTestsHaveAnEntry|TestTrackedGoFilesAreGofmtClean)$' .` 通过（2.047s）。`bash -n scripts/test-demo-generated.sh` 与 `git diff --check` 通过。
- `python3 scripts/check-doc-anchors.py`：359 文件、0 broken。没有运行全仓慢套件或性能测试。
- 完整原始日志保留在本 worktree 的被忽略目录 `artifacts/terrain-wal-crash-20261114/`；本目录保留可携带的红绿日志与脚本结果摘录。
- 完整脚本完成后对 offset 37500 执行 `mirror-local.sh down`，只停止本批依赖；offset 37000 的本批 Mongo 已在换端口时 down。没有删除 worktree、分支或共享环境数据。
