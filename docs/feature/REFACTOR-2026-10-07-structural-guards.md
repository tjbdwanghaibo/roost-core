# REFACTOR-2026-10-07：四项结构守卫（指标名、三大块方向、生成工程编译、pretag 漂移）

分支 `guards`，基线 `43a06e44`（rebase 到 `ca173fcb`），提交 `fe8ef362`。来源：[框架文档发现登记表](../review/FRAMEWORK-DOCS-FINDINGS-2026-10-07.md) F11 N3、F00（三大块方向无守卫）、F12 G7，以及 RR-20261006-60 之后生成工程 `go vet` 失败没人发现这一新情况。顺带修掉守卫抓到的真实问题 [RR-20261006-67](../bug/RR-20261006-67.md)（F11 N1、N2）。

## 目标

四类问题过去都是“漏进来没人发现”：名字或依赖在一处改了，另一处靠人记得。每类各补一条在常规 `go test` 或 pretag 里跑的守卫，并用真实缺陷或变异证明它会红。

| 守卫 | 位置 | 抓什么 | 何时跑 |
| --- | --- | --- | --- |
| 指标名一致性 | `metric_names_gate_test.go`（根包） | 文档、两个仪表盘、demo README 写的指标名与标签不存在；仪表盘 / 告警用的指标没进 `OBSERVABILITY.md`；lockstep / saga / skill 指标没收录 | 常规 `go test .` |
| 三大块依赖方向 | `dependency_boundary_test.go` 的 `TestCorePillarDependencyDirection` / `TestCrossPillarViolation` | nest / dataengine / sync 之间新增的跨块 import | 常规 `go test .` |
| 生成工程能编译 | `codegen/internal/roost/generated_project_compiles_promises_test.go` | 新生成的 game-demo 对着当前检出 `go build ./...`、`go vet ./...` 失败（含测试文件） | 常规 `go test ./codegen/...`（`-short` 与 Windows 跳过） |
| pretag 漂移 | `scripts/pretag.sh` 第 3b 步；`ci_generated_code_test.go` 的 `TestPretagRegeneratesAndChecksTheTree` | 提交的生成代码与生成器当前输出不一致 | `scripts/pretag.sh`；守卫自身在常规 `go test .` |

## 1. 指标名一致性（F11 N3）

### 收集

测试从源码收集进程会写进 `metrics` 注册表的全部指标（名字、类型、标签键），口径与 [guide 11 §6.3](../framework/guide/11-observability.md#63-全仓指标总表) 的全仓指标总表相同：

- 非测试 Go 文件里对 `IncCounter` / `SetGauge` / `AddGauge` / `ObserveDuration` / `ObserveHistogram` 的调用。名字参数可以是字面量、本包或别的包的字符串常量、`a + b`、函数内只赋过字面量的局部变量，或者一层包装函数的形参。`kit/statslog` 的 `publishCounts(name, label, …)` 就是这种包装，调用方传进来的名字和标签键照样收。名字解析不出来的写入点直接报错，保证总表完整。
- `metrics` 包在 `Snapshot` 里自己合成的序列（`obs.series.dropped{metric}`），按 `Metric{Name: …}` 字面量收。
- 各包导出的 `Metric*` 字符串常量。写入点已经收到的以写入点为准；没收到的，类型按未知、标签按开放算存在。
- 生成器产物：`demo/**/*.tmpl` 和 codegen 渲染进字符串的 Go 代码。这两处按文本扫描同样的调用和 `*Metric* = "…"` 常量，例如 `player_tcp_*`、`scene_session_reopen_failed_total`。

标签键从标签参数里的 `metrics.Labels{…}` / `map[string]string{…}` 字面量收。参数是局部变量时，收函数内对它的字面量赋值和 `x["k"] = v`。参数是包装调用（`r.metricLabels(metrics.Labels{…})`）、字段或形参时，该指标的标签记为“开放”，不做标签核对。这里宁可漏报，也不误报。

导出序列名按 `metrics/prometheus.go` 的规则推出：点号变 `_`；Counter 补 `_total`；Duration 有 `_count/_sum_nanos/_max_nanos/_last_nanos` 四个序列；Histogram 有 `_bucket/_sum_nanos/_count` 三个序列。

当前收集到 175 个指标名，其中 15 个标签开放。guide §6.3 列的 167 个不含生成工程的指标。

### 核对

| 对象 | 规则 |
| --- | --- |
| `OBSERVABILITY.md` “指标清单”各表第一列 | 每个反引号名字都要能解析到真实指标（源码名或导出序列名，支持 `*`、`{a,b}` 展开、同格内相对写法 `queue_len` / `worker_num`、`_requeue_total`），`{…}` 里的标签要存在；删除线内的已移除指标跳过 |
| `OBSERVABILITY.md` “告警基线建议” | 每条告警的指标与标签存在，且该指标在指标清单里 |
| `observability/grafana-roost-overview.json` 与 demo 仪表盘模板 | 每条 `expr` 里的指标都是真实导出序列，`by (…)` 和选择器里的标签存在（`job` / `instance` 总是允许，`_bucket` 另允许 `le`）；仪表盘上的框架指标要在 `OBSERVABILITY.md` 里，只在生成代码里写的指标要在 demo README 里 |
| `demo/deploy/dev/observability/README.md.tmpl` “指标 ↔ 链路”表 | 每个名字与标签存在 |
| 收录范围 | 源码里所有 `lockstep.*`、`saga.*`、`skill.*` 指标都在 `OBSERVABILITY.md` 指标清单里 |

`TestMetricGateParsers` 给每种解析写法各留一例（PromQL 名字与标签、label 列表与名字展开、相对写法），防止解析不出东西时门禁静默空转。

### 怎么红（修前，基线 `c78d0c34`）

在未修的仪表盘与文档上直接跑，红的就是真实问题，不用变异：

```text
observability/grafana-roost-overview.json: "rate(entitysync_flush_gate_deferred_total[1m])" queries entitysync_flush_gate_deferred_total, which no source writes (exported series of the metrics registry)
demo/deploy/dev/observability/grafana/dashboards/roost-demo.json.tmpl: "sum by (subject)(rate(nats_jetstream_terminal_total{job=\"{{GAME_SERVICE}}\"}[1m]))" uses labels [subject] that [nats_jetstream_terminal_total] are never written with (written at [nats/driver/jetstream.go:IncCounter])
demo/deploy/dev/observability/README.md.tmpl 指标表第 9 行: `nats_jetstream_terminal_total{subject}` has labels [subject] that the metric is never written with (written at nats/driver/jetstream.go:IncCounter)
observability/grafana-roost-overview.json: runtime_goroutines (runtime.goroutines) is on a dashboard but OBSERVABILITY.md does not document it
…（runtime_* 4 个、entity_count* 3 个、configdata_* 3 个、nats_jetstream_terminal_total 同上）
OBSERVABILITY.md does not document saga.completion.late_after_abandon_total (saga/engine.go:IncCounter); every lockstep.* / saga.* / skill.* metric belongs in its metric list
…（saga.* 11 个、skill.* 5 个同上）
```

前三行就是 F11 N1、N2（[RR-20261006-67](../bug/RR-20261006-67.md)）。

### 修好的问题

- 总览仪表盘 `entitysync_flush_gate_deferred_total` 改为 `entitysync_durability_gate_deferred_total`（N1）；`docs/TROUBLESHOOTING.md` T-100 的同一旧名一并改掉。
- demo 仪表盘按 `reason` 聚合 `nats_jetstream_terminal_total`，图例同步改为 `{{reason}}`，面板说明补上 `permanent`。demo README 的标签改为 `{reason}`，并写清两种原因（N2 ①）。README 里 `bus_rpc_*` 的开关改为 `nats.rpc.transport: jetstream`（N2 ②，源码 `kit/nats/nats_mod.go` `jetStreamRPC`）。N2 ② 是开关键写错，门禁抓不到，这次顺手修掉。
- `OBSERVABILITY.md` 新增四节：Saga（11 个）、技能运行时（5 个）、进程与配置表（`runtime.*` 5 个、`entity.count*` 3 个、`configdata.*` 3 个）、NATS（`nats.jetstream.terminal.total`、`nats.jetstream.settle_failures.total`）。

### 范围取舍

guide §6.3 统计 `OBSERVABILITY.md` 漏收 88 个指标（N8～N9）。这次没有要求全量收录，理由如下：

- 全量收录要给 80 多个指标逐一写说明，工作量大。其中不少是内部诊断计数，例如 nest 的 `trace.*`、remoteentity 的各类 `_total`。它们该不该进面向运维的清单，需要逐个判断，不是机械搬运。
- 门禁先保证两件事。第一，文档、仪表盘、告警写到的名字和标签都真实存在，这是 F04-5、N1、N2 的共同根因。第二，仪表盘和告警用到的指标都有文档，运维看到一条曲线就能查到含义。
- 在此之上，`lockstep.*`、`saga.*`、`skill.*` 全部收录。这三块的指标是近几轮新加的，靠 review 维护，最容易漏；它们大多是“非零即查”类告警信号。今后这三个前缀下新加指标而不写文档，测试会红。
- 以后要扩大收录范围，只需在 `TestObservabilityDocNamesExistingMetrics` 的前缀表里加前缀。

没有覆盖的部分：

- `OBSERVABILITY.md` 正文段落里的反引号内容不做存在性核对，因为正文里混有配置键、包路径、函数名，只有表格第一列和告警列表是名字契约。正文里点名、且能解析到的指标仍算“已收录”。
- `syncstream_*`、`skillsync_*` 走自己的 `MetricSink`，不进注册表，不在总表里（N10）。

## 2. 三大块之间的依赖方向（F00）

`dependency_boundary_test.go` 新增 `TestCorePillarDependencyDirection`，以 [00-overview §1.1](../framework/impl/00-overview.md#11-包级依赖图) 的依赖图为现状：

- 块的划分：nest 调度 = `nest`、`entity`、`actionflow`、`lock`；dataengine = `dataengine`（含 `engine`）、`nestwal`、`versionstore`、`cache`；sync = `sync/...`、`syncstream`、`gateway`。
- 块内随意。块与块之间只允许 `allowedCrossPillarImports` 登记的 9 条边：nest → dataengine 契约根包、entity → cache、dataengine 根包 / engine / nestwal → entity、engine / nestwal → nest、cache → `sync/syncbus/...`、`sync/entitysync/...` → entity。nest → sync、sync → dataengine 都没有边，sync 也不碰调度（nest、lock、actionflow）。
- 只看非测试文件，口径与 §1.1 相同。测试本来就跨块装配，例如 nest 的测试用 nestwal，nestwal 的测试用 `dataengine/engine`。
- 表里某行不再被任何文件用到时也红，保证表只记录现状。
- 建在三块之上的包（`skill`、`saga`、`remoteentity` 等）不在这条规则的范围里，由现有的层次规则管。

`TestCrossPillarViolation` 用固定的例子钉住判定：拒绝 9 例，放行 6 例，另加 `pillarOf` 的边界例子，比如 `synctest` 不属于 sync。

### 怎么红（变异）

在 `sync/lockstep/room.go` 加 `_ ".../nest"`，在 `nest/dispatcher.go` 加 `_ ".../sync/syncbus"`。两处都能编译，不成环：

```text
--- FAIL: TestCorePillarDependencyDirection (0.04s)
    dependency_boundary_test.go:459: nest/dispatcher.go: nest 调度 块的 nest 不得 import sync 块的 sync/syncbus（三大块之间只允许 allowedCrossPillarImports 登记的边，见 docs/framework/impl/00-overview.md §1）
    dependency_boundary_test.go:459: sync/lockstep/room.go: sync 块的 sync/lockstep 不得 import nest 调度 块的 nest（三大块之间只允许 allowedCrossPillarImports 登记的边，见 docs/framework/impl/00-overview.md §1）
```

还原后绿。00-overview §1.2 的守卫表同步加了这一行，“没有守卫的方向”一段改为指向这条守卫。

## 3. 生成工程必须能编译（新发现）

RR-20261006-60（`2a8b2e64`）把手写 HandlerMeta 的 Durability 换成 dataengine 的类型之后，demo 的两个测试模板 `enter_game_test.go.tmpl`、`guild_ids_test.go.tmpl` 仍写 `nest.DurabilityStrict`，新生成的 game-demo `go vet` 报类型不符。codegen 的测试只做文本断言和 `parser.ParseFile`，没有任何用例把生成工程交给类型检查器，直到 saga 那一轮顺手修掉（`133a4e88`）才被发现。

`TestGeneratedGameDemoBuildsAndVetsAgainstThisCheckout` 的做法：取包内 game-demo 夹具的私有副本（mods `configdata,mongo,nats,dataengine,nest`，与 `project_fixture_test.go` 登记的形状相同），`go mod edit -replace` 到当前检出，然后在 `GOWORK=off GOFLAGS=-mod=mod` 下跑 `go build ./...` 和 `go vet ./...`。`go vet` 会把 `_test.go` 一起类型检查，补上的正是 `go build` 看不到的那一半。

- 耗时：本机热缓存 13～14s（生成夹具与其他用例共享）。决定直接放进常规测试，不放 pretag；pretag 的 `go test ./...` 和 CI 的 `go test ./...` 都会跑到它。
- `-short` 跳过。Windows 也跳过：生成形状与平台无关，而 windows-compatibility 作业的整包时长已经贴着上限（RR-20260928-14）。
- 不跑生成工程的 `go test`：其中的集成用例要真实依赖。类型错误 vet 已经能抓到。

### 怎么红（变异）

把 `guild_ids_test.go.tmpl` 的 `coredata.DurabilityStrict` 改回 RR-60 之前的 `nest.DurabilityStrict`：

```text
--- FAIL: TestGeneratedGameDemoBuildsAndVetsAgainstThisCheckout (13.12s)
    generated_project_compiles_promises_test.go:57: generated game-demo: go vet ./... failed: exit status 1
        # example.com/planet/game/controllers/player
        # [example.com/planet/game/controllers/player]
        vet: game/controllers/player/guild_ids_test.go:179:24: cannot use nest.DurabilityStrict (constant 3 of uint8 type nest.DurabilityPolicy) as dataengine.Durability value in struct literal
```

还原后绿（13.6s）。

## 4. pretag 漂移检查（F12 G7）

`scripts/pretag.sh` 在“工作树干净”之后新增第 3b 步：先跑 `GOWORK=off go generate ./...`，然后要求 `git status --porcelain` 为空。不为空时列出变化的文件后失败，变化留在工作树里，方便看 diff。

放在第 3 步之后、manifest 与构建之前，原因有两个：它属于“工作树与提交一致”这一类检查；而且只需 7 秒左右，失败要尽早报。ci.yml 已有同样的检查，但 CI 在 tag 推上去之后才跑，而且没人等它（见脚本开头），所以发版前必须由 pretag 自己做。

根包新增 `TestPretagRegeneratesAndChecksTheTree`，钉住这一步：pretag 里有 `go generate ./...` 命令（`echo` 与报错文本里的同名字样不算），之后有 `git status --porcelain`，并且两者都在 “ready to tag” 之前。

### 怎么红

- 守卫自身：删掉脚本里的 `GOWORK=off go generate ./... >/dev/null` 这一行，测试报 `scripts/pretag.sh never runs \`go generate ./...\`…`；还原后绿。第一版用子串匹配，被 `echo` 行骗过、变异下仍绿，改成匹配命令行首后才红。这一点记在这里，供以后写同类守卫参考。
- 脚本本身：在一次性 worktree 里给 `service/session/session_rpc_gen.go` 追加一行注释并提交，然后跑 `bash scripts/pretag.sh v1.99.99-test`：

```text
pretag: checking go generate ./... leaves the tree unchanged
 M service/session/session_rpc_gen.go
pretag: go generate ./... changed the tree; commit the regenerated files (git diff shows what drifted)
```

### 实跑 pretag（干净 worktree，`80e3b331` = 本分支代码）

- `bash scripts/pretag.sh v9.9.9-test`：停在**第 1 步**（主版本号与模块路径）：`v9.9.9-test needs module path 'github.com/tjbdwanghaibo/roost-core/v9'`。这是版本号相关的检查，所以到不了新加的第 3b 步。
- 为了走到第 3b 步，又跑了 `bash scripts/pretag.sh v1.99.99-test`。第 2 步（tag 不存在、origin 可达）、第 3 步（无 replace、树干净）、第 3b 步（`go generate` 后树仍干净，约 7s）都通过，停在**第 4 步**：`codegen/ci/framework-release.yaml says release: v1.23.0 but this release is v1.99.99-test`。这也是版本号相关的检查，没有打 tag。第 5 步（build / vet / tidy / 全量 test）另行全量跑过，见下节。

## 验证（`GOWORK=off`）

见提交说明与 [RR-20261006-67 修复记录](../bugfix/RR-20261006-67.md)的“回归”一节：`gofmt -l` 为空；根包 `go test -count=1 .`；`go test -count=1 ./codegen/...`；`go build ./... && go vet ./...`；推送前全量 `go test ./...`。

## 兼容

- 只加测试与文档。行为上的变化只有一处：pretag 多了一步，提交的生成代码过期时 pretag 会失败。
- 仪表盘 JSON 与 demo 模板改了查询。已导入旧总览仪表盘的，重新导入即可。已生成的工程，`roost project sync` 会更新 `deploy/dev/observability/` 下由生成器管理的文件。
- 今后新增指标时，名字解析不出来（运行期拼名字）会让门禁报错：要么改成常量，要么在 `knownDynamic` 里登记理由。新增跨块 import 时，在 `allowedCrossPillarImports` 加一行并同步 00-overview §1。

## 实施状态

已实施（`fe8ef362`）。没有未完成项。全量收录 `OBSERVABILITY.md` 留给以后：在前缀表里加前缀即可逐步收紧（见 §1 范围取舍）。
