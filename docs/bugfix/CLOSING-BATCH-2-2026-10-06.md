# 收尾第 2 批：生成形状相关的小项（A8 / A9 / A11 / A15 / A17）

- 日期：2026-10-06；基线 `8a292a5a`（分支 `cb2`）。来源：协调者的全仓盘点（维护者第十一轮“收尾：盘点全部未完成问题，处理完后统一发一个版本”），维护者已授权。**未发版**，随收尾统一发版。
- 范围：`codegen/internal/roost`、`codegen/scripts`、`.github/workflows`、demo 模板、文档。不碰 skill 与 kit / core 小修（另两批）。
- 读代码：codebase-memory 的共享 generation 停在 09-30，本批涉及的文件（`catalog.go`、`render_player_tcp.go`、`source-head-check.sh`、demo 仪表盘）都以当前源码为准直接阅读。

## A8 生成的配置缺新的 `remote_entity` 键

**问题**：`codegen/internal/roost/catalog.go` 的 `remote_entity` 段没有 B2 `cached_max_staleness`、O4 `snapshot_interest_per_consumer`、O-M6-3 `snapshot_l2_tombstone_wait_replicas` / `_timeout`、Mirror 第 5 步 `mirror.shutdown_timeout`。运维只能去 USER_GUIDE 找键名才知道能调。

**修复**：五个键连同缺省值与中文注释写进模板。开发配置、生产示例、k8s Secret 示例三份都从这一段生成（生产化与 Secret 嵌入沿用 `productionizeConfig` / `appendModConfigSections`）。缺省值逐个对着 core `remoteentity.DefaultConfig()` 与 kit 核对：

| 键 | 写入值 | 依据 |
| --- | --- | --- |
| `cached_max_staleness` | `30s` | `CachedMaxStaleness` 零值取 `SnapshotCacheTTL`；模板的 `snapshot_cache_ttl` 是 30s。kit 对设置了的值要求为正，所以写与它相同的值，不能写 0 |
| `snapshot_interest_per_consumer` | `0` | `DefaultConfig` 零值 = `snapshot_interest_subs / 16` |
| `snapshot_l2_tombstone_wait_replicas` | `1` | `DefaultConfig` 为 1 |
| `snapshot_l2_tombstone_wait_timeout` | `50ms` | `DefaultConfig` 为 50ms，上限 `MaxSnapshotL2TombstoneWaitTimeout` = 1s |
| `mirror.shutdown_timeout` | `5s` | kit `defaultMirrorShutdownTimeout`（core `DefaultConfig` 没有这一项）；只有 `RemoteMirrorMod` 读，注释写明手工接入时要调大 `shutdown.total_timeout` 与宽限期 |

**连带修正**：`productionizeConfig` 原来把整串 `replicas: 1` 换成 `replicas: 3`（流副本数），加了 `snapshot_l2_tombstone_wait_replicas: 1` 之后会把墓碑 `WAIT` 的副本数也改成 3。改为只替换独占一行、键名恰为 `replicas` 的行（`render.go` 的 `streamReplicasLine`）。

**红**（基线上跑新用例）：

```
--- FAIL: TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys
    production=false: config lacks "cached_max_staleness: 30s"   （其余四个键同样，开发 / 生产各一遍）
--- FAIL: TestGeneratedConfigsPassStrictAndProductionValidation
    a4_config_test.go:92: config.game.yaml does not set remote_entity.cached_max_staleness
    a4_config_test.go:103: config.game.yaml: snapshot_l2_tombstone_wait_replicas = 0, want DefaultConfig 1
    （config.game.prod.example.yaml、secret.game.example.yaml 同样）
```

只改模板、不改生产化时：`production=true: config lacks "snapshot_l2_tombstone_wait_replicas: 1"`（被改成了 3）。

**绿**：两条用例通过。生成工程里的检查（`generated_config_validation_promises_test.go` 注入的 `a4_config_test.go`）对每份含 `remote_entity:` 的配置：五个键都已设置；取值与 `DefaultConfig` / kit 缺省一致；`app.ValidateServiceConfig` 通过（生产示例加 `env: production` 也通过）；`RemoteEntityMod.Init` 与 `RemoteMirrorMod.Init` 都接受（值域检查在 Init 里）。生成的 game 服务带这份配置在隔离环境起来并就绪（见 A17）。

**兼容**：只影响新生成的工程；已有工程的配置文件归应用所有，不回写（`appendModConfigSections` 只追加整段新 Mod）。v1.21.0 / v1.22.0 的 kit 不认识墓碑两键，viper 忽略未知键，生成器的 Core 下限不需要上调。

## A9 生成 TCP 的上限校验报错不点名字段

**问题**：生成的 `validateConfig`（`render_player_tcp.go`）对 addr、所有上限与超时下限只回一句 `player tcp: addr, limits and timeouts are outside safe bounds`，超时上限是另一句 `timeouts exceed safe bounds`，运维只能逐行猜。

**修复**：逐条检查、`errors.Join` 一次报全，每条点出 `player_access.tcp.<键>` 与当前值和范围；受另一个键约束的同时点出那个键与理由：`max_connections_per_ip`、`max_handshakes` 不能超过 `max_connections`（一个 IP 的连接计在服务器的连接里；握手在已接受的连接上进行），`login_timeout` 不能超过 `dispatch_timeout`（登录在一次 dispatch 里）。`max_connections` 本身不合法时不再报关系类错误，免得一条错因生出三条。例：

```
player tcp: player_access.tcp.max_handshakes = 2000 exceeds player_access.tcp.max_connections = 1000; every handshake runs on an accepted connection
player tcp: player_access.tcp.handshake_timeout = 2m0s is outside (0, 1m0s]
```

**红**（生成的 `TestAnOutOfBoundsSettingIsRefusedByName`，旧 `validateConfig`）：

```
map[max_connections:1000 max_handshakes:2000] refused with "player tcp: addr, limits and timeouts are outside safe bounds", want it to name "player_access.tcp.max_handshakes = 2000"
map[handshake_timeout:2m] refused with "player tcp: timeouts exceed safe bounds", want it to name "player_access.tcp.handshake_timeout = 2m0s"
map[addr:localhost] refused with "player tcp: invalid addr \"localhost\": ...", want it to name "player_access.tcp.addr"
（共 10 个子用例全部失败）
```

**绿**：10 个子用例通过；缺省配置仍被接受；`TestConfigValuesOfTheWrongTypeAreRefusedByName`、`TestServerRejectsInvalidConstruction` 不变。

**兼容**：只改报错文本（多条时换行分隔），接受 / 拒绝的边界不变。

## A11 `source-head-check.sh` 吞掉 add 失败、与 CI 序列不一致

**问题**：`codegen/scripts/source-head-check.sh` 的 full 场景用 `(...) || true` 包住 add，任何一步失败都被吞掉；它只 add 了 access / transport / skill / saga 四步，而 `framework-compat.yml` 的 source-head lane 还有 component、dao、handler、protocol、endpoint、rpc 与 `project sync`，本地“full OK”并不代表 CI 那条路能过。

**修复**：add 序列收拢到 `codegen/scripts/full-scenario-adds.sh`（`set -euo pipefail`，参数：roost 二进制 + 传给 `project sync` 的参数），workflow 与本地脚本各调用一次，去掉 `|| true`。`uses_rpcs` 的清单编辑由 GNU 专用的 `sed -i 's/…\n…/'` 改为 awk + mv（本地是 macOS），改完 grep 确认，找不到 `    gate:` 条目就失败。根包新门禁 `TestFullScenarioAddSequenceIsDefinedOnceAndNotSwallowed`（`ci_full_scenario_test.go`）：两个调用方都不自己 `roost add`、恰好调用共享脚本一次且不带 `||`、没有 `) || true`；共享脚本带 `set -euo pipefail` 且没有 `|| true`。

**红**（根包，修前）：

```
.github/workflows/framework-compat.yml runs an add itself ("\"${RUNNER_TEMP}/roost\" add access player --service gate") ...（10 行）
codegen/scripts/source-head-check.sh swallows a failing subshell: "&& GOWORK=off \"$work/roost\" add saga GuildTransfer -service game -steps debit,credit) || true"
open codegen/scripts/full-scenario-adds.sh: no such file or directory
```

**绿**：门禁通过；`shellcheck` 两个脚本无告警；本地 `codegen/scripts/source-head-check.sh full` 走完整序列（含 rpc 与 `project sync -skip-deps`）后 build / vet / test / glsvet 通过（`source-head-check: full OK`）。

## A15 生成 TCP 缺“handler 不配合 ctx”的用例（只加测试）

现有 `TestADispatchThatOutlivesItsBudgetAnswersAndFreesTheSlot` 的 handler 在 ctx 结束时返回，钉不住不配合 ctx 的情形。新增生成用例 `TestADispatchTimeoutBoundsTheWaitNotAnUncooperativeHandler`（测试辅助 `stuckDispatchServer` 抽出通用的 `dispatchServer`），钉住：

1. **DispatchTimeout 只界定等待、不界定工作**：100ms 预算到点时 handler 拿到的 ctx 以 `DeadlineExceeded` 结束，但 handler 不返回就一直在跑；期间连接上没有任何答复；放行后答复照常发出，内容是 handler 自己的结果。
2. **名额归还的时机**：连接槽与按 IP 计数在 handler 返回、连接结束之后才归还。第二个不配合的请求在跑时客户端先断开，300ms 内两项计数仍各为 1（读循环停在 handler 里，看不到断开）；放行后归零。

当前实现本来如此，用例首次即通过（承诺固定，不是缺陷修复）；生成工程 `-race -count=3` 通过。

## A17 `scene_session_reopen_failed_total` 没有 Grafana 面板

**修复**：`demo/deploy/dev/observability/grafana/dashboards/roost-demo.json.tmpl` 新增一行“场景复制会话”（id 25）与面板“复制会话重开放弃（按原因，5 分钟内次数）”（id 26），查询 `sum by (reason)(increase(scene_session_reopen_failed_total{job="{{GAME_SERVICE}}"}[5m]))`，说明写明非零的含义与 No data 是好消息；后续各行下移 8 格，原有面板 id 不变。`demo_test.go` 加断言：仪表盘有查询该指标的面板，且 `scene.go` 仍以这个名字计数。

**实抓**：

- 生成 game-demo（replace 到 worktree），在隔离环境（`~/.roost-it/roost-dataengine-it` 的 Mongo / NATS / Redis，etcd 用本机 2379；DAO 库名常量、库、流、键、etcd 前缀都换成本次唯一的 `cb2m<时间戳>`）先起 global（game 的 activity 在 Init 里经 RPC 绑定它），再起 game：就绪，`curl /metrics` 返回 200、25 行。此时 `scene_session_reopen_failed_total` 不在输出里——计数器在第一次递增前不存在（与 demo 可观测性 README “No data 是好消息”一致），起服务本身触发不了放弃重开。结束后删除本次的流、库、Redis 键（etcd 无残留）。
- 名字一致性：在生成工程的副本里临时加一个探针用例（不提交），先跑现有的 `TestASessionReopenRefusedWhileTheOldOneClosesIsRetried`（用尽与拒绝两种放弃各一次），再用 ops `/metrics` 的同一个函数 `metrics.PrometheusText(registry.Snapshot())` 渲染进程注册表：

```
scene_session_reopen_failed_total{reason="exhausted"} 1
scene_session_reopen_failed_total{reason="refused"} 1
panel "复制会话重开放弃（按原因，5 分钟内次数）" queries scene_session_reopen_failed_total: sum by (reason)(increase(scene_session_reopen_failed_total{job="game"}[5m]))
```

导出名就是计数名（已带 `_total`，不再追加），标签 `reason` 与面板的 `sum by (reason)` 一致。

**顺带观察**（未改，不在本批范围）：demo 可观测性 README 也列了 `configdata_rollback_total{trigger}`，仪表盘同样没有查询它。

## 验证（全部 `GOWORK=off`，2026-10-06 本机 macOS）

| 命令 | 结果 |
| --- | --- |
| `gofmt -l`（改动的 Go 文件） | 空 |
| `go test -count=1 ./codegen/...` | 全部 ok（15 个有测试的包） |
| `go generate ./...` 后 `git status --porcelain` | 只有本批改动，无生成漂移 |
| 根包 `go test -count=1 .` | ok（含新门禁） |
| `go build ./... && go vet ./...` | 通过 |
| 生成 game-demo（`project new -skip-deps -template game-demo` → `go mod edit -replace` 到 worktree → `go mod tidy`）`go build ./... && go vet ./... && go test -count=1 ./...` | 通过（18 个包 ok，无 FAIL） |
| 生成 game-demo `go test -race -count=3 ./internal/access/player/tcp/` | ok |
| 生成 game-demo `go run …/cmd/glsvet ./...` | 通过 |
| `shellcheck codegen/scripts/full-scenario-adds.sh codegen/scripts/source-head-check.sh` | 无告警 |
| `codegen/scripts/source-head-check.sh full` | `source-head-check: full OK` |
| 隔离环境起 global + game、`curl /metrics` | 就绪，200；资源已清理 |

生成器的 Core 下限（`manifest.go`）不变：生成代码没有用到 v1.21.0 没有的接口（新增的只是配置键与生成测试；墓碑两键在 v1.21.0 / v1.22.0 的 kit 里不认识，viper 忽略）。

未验证：GitHub 上 framework-compat 的 full 场景（按约定不等 CI；本地 `source-head-check.sh full` 跑的是同一份 add 序列，minimum / released lane 的 `project sync` 不带 `-skip-deps`，本地没跑）。
