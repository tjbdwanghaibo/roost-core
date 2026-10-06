# v1.23.0 实现 · APP / OWN / CLK / OPS / TOOL 部分

> 范围与编号同[说明文档](guide-app-own-clk-ops-tool.md)（39 条：APP 13、OWN 6、CLK 6、OPS 7、TOOL 7）。
> `path:line` 一律按提交 `02c8a10d`（本版发版前 main）的源码核对；`.tmpl` 行号指模板文件本身。codebase-memory 共享索引停在 2026-09-30，本文全部位置以当前源码直接读取为准，没有引用索引结论。
> 历史记录与源码冲突时以源码为准，冲突处在条目里写“源码与记录不一致”。修前红文本照记录原样抄录；记录没保存的写“记录未保存修前红文本”。

## 怎么用这份文档 review

**建议顺序**（依赖从下往上）：

1. APP-4（`RuntimeFailure.OnFail`）→ APP-1（单实例锁状态机与 `run` 挂点）→ APP-9 / APP-8 / APP-12（`run` 的启动失败、停机、退出三条收尾路径）→ APP-5 / APP-13（能力登记）。`app/app.go` `run`（`:104-524`）是这几条的交汇点，先通读一遍再看条目。
2. APP-6 / APP-7（共用停机类型）→ OPS-3（Ops 停止与 bind）。
3. CLK-1 → CLK-2 → CLK-3 / CLK-4 / CLK-5（时钟边界与高水位，高水位挂在单实例锁之后）→ CLK-6（timer，独立）。
4. OWN-2 → OWN-3 / OWN-4 / OWN-5（game-demo 模板，依赖 APP-1 / APP-5）→ OWN-6（kit global，独立）。
5. OPS-1 / OPS-2 / OPS-5（metrics 口径）→ OPS-4 / OPS-6 / OPS-7。
6. TOOL-*（脚本与根包门禁，独立）。

**先读的规范**（[roost-coding](../../agent-skills/roost-coding/SKILL.md)）：“生命周期与装配的复审要点”里的三步停机与“新的停机对象优先用共用类型”（APP-6～9、OPS-3）；“业务时钟与系统时钟”（CLK-*）；“反复出问题要上报方向判断”（OWN-1 → OWN-2 的先例）；“验证与性能纪律”的“示例要实跑”与共享隔离环境规则（TOOL-4、TOOL-7）。改了错误分类、关闭所有权的条目按 [fix-contract-review](../../agent-skills/roost-coding/references/fix-contract-review.md) 复核。

**本地复跑**（全部 `GOWORK=off`）：

| 范围 | 命令 |
| --- | --- |
| APP | `go test -race -count=3 ./app/ ./kit/nest/ ./kit/redis/ ./kit/ops/ ./health/ ./internal/operation/ ./internal/stopcontract/` |
| 停机骨架 | `go test -race -count=3 -run 'StopContract\|Lifetime\|Serial' ./internal/... ./bus/ ./sync/syncbus/... ./manager/ ./kit/nest/ ./etcd/driver/ ./remoteentity/ ./kit/ops/` |
| CLK | `go test -race -count=3 ./clock/ ./timer/ ./service/mail/ ./kit/service/mail/ ./kit/service/global/activity/ ./kit/service/chat/ ./kit/service/account/ ./service/match/ ./kit/service/match/ ./cmd/glsvet/` |
| OPS | `go test -race -count=3 ./metrics/ ./robot/loadtest/ ./robot/runner/ ./versionstore/ ./kit/service/rank/ ./kit/service/chat/ ./servicemetrics/ ./kit/mods/` |
| OWN（kit） | `go test -race -count=3 ./kit/service/global/... ./kit/service/account/` |
| OWN（模板） | 生成 game-demo：`roost project new <名> -template game-demo`，`go mod edit -replace github.com/tjbdwanghaibo/roost-core=<worktree>`，`go build ./... && go vet ./... && go test -race -count=3 ./internal/service/game/ ./game/controllers/player/ ./internal/access/... && go test ./...` |
| codegen | `go test -count=1 ./codegen/...`；`go generate ./...` 后 `git status --porcelain` 为空 |
| 根包门禁 | `go test -count=1 .`（含 TOOL-2～5 与 `TestCoreDependencyBoundary`） |
| 真实依赖（可选） | Redis：`REDIS_ADDR=<隔离 Redis> go test -tags integration -run 'TestSingletonStore\|TestBusinessTimeHighWaterMarkOnRealRedis' ./kit/redis/`；Mirror 私有环境：`ROOST_MIRROR_LOCAL_HOME=<私有目录> scripts/mirror-local.sh test-core` |

**不能碰的共享资源**：共享隔离环境 `~/.roost-it/roost-dataengine-it`（只 `source env.sh`、不打印；integration 一律加 `-run`；`remote-acceptance.lock` 存在时不跑真实依赖用例；故障注入一律自建代理 / 进程，不 `/reset` 共享 toxiproxy）；故障矩阵与 `dataengine-env.sh` 的 up / down / heal / reset / fault 会取全局验收锁，review 期间不要跑；生成工程的 DAO 库名是 `db/def` 编译期常量（生成为 `"game"`），在共享环境演练要连它一起改（App 锁方案 §13 第 5 笔“隔离的教训”）；主检出的 `artifacts/` 不动。

## 条目总表

| 编号 | 一句话 | 首发版本 | 主要提交 | 主要包 |
| --- | --- | --- | --- | --- |
| [APP-1](#app-1) | App 单实例锁 | v1.20.0 | `d4ac9853` `e3810ef1` `b291edb9` `4959a0dd` `86f687cf` `cd8c1ad3` `cbccacdb` `ad35bbcc` `3d7d228b` | `app`、`kit/redis` |
| [APP-2](#app-2) | 生成器装配单实例锁；etcd 两处 | v1.20.0 | `6863dbc3` `364b763c` `b67d5945` | `codegen/internal/roost`、`etcd/driver` |
| [APP-3](#app-3) | 删 global 租约 API，停发选主 / 锁 capability | v1.20.0 | `71c6fb6b` `40d89ac6` `4bea90d0` `10e2e0ea`（`88f33776` 文档） | `kit/service/global`、`kit/mods`、`kit/redis`、`kit/etcd` |
| [APP-4](#app-4) | `OnFail` 与统一 fail-stop | v1.20.0 / v1.21.0 | `d4ac9853` `cd8c1ad3` `2c1c7be7` | `app`、`kit/nest` |
| [APP-5](#app-5) | `Live` 与 C5 契约 | v1.20.0 / v1.21.0 | `d4ac9853` `bd6df5e5` | `app`、`kit/redis` |
| [APP-6](#app-6) | `Lifetime`、契约骨架、glsvet stophints | v1.20.2 | `50f2ac2a` | `internal/operation`、`internal/stopcontract`、`cmd/glsvet` |
| [APP-7](#app-7) | `operation.Serial` 与 kit Mod 停止收敛 | v1.23.0（本版）；NC-233 / 234 v1.21.0 | `d05a04a1` `2c1c7be7` | `internal/operation`、`kit/redis`、`kit/mongo`、`kit/nats`、`kit/remoteentity` |
| [APP-8](#app-8) | 停机 hook 受预算 | v1.21.0 | `2c1c7be7` | `app` |
| [APP-9](#app-9) | 启动失败收尾 | v1.20.2 | `d6550a16` | `app` |
| [APP-10](#app-10) | Degraded 算就绪 | v1.21.0 | `f6828f17` | `health`、`kit/ops` |
| [APP-11](#app-11) | checker 期限 | v1.23.0（本版） | `7b73aabc` | `health`、`kit/ops` |
| [APP-12](#app-12) | 退出原因进文件日志 | v1.23.0（本版） | `611d5d72` | `app` |
| [APP-13](#app-13) | App 能力与 Mod 依赖 | v1.20.0 / v1.21.0 / v1.23.0（本版） | `d4ac9853` `b9fc5342` `d483238e` | `app`、`kit/mods`、`kit/remoteentity` |
| [OWN-1](#own-1) | 租约状态机期三处修复 | v1.20.0 | `18bb86ae` `a28a3152` `890abdda` | `demo/internal/service/game`（已删除的代码） |
| [OWN-2](#own-2) | 玩家静态绑定 | v1.20.0 | `f051e24a` `021454d5` `c441fbdd` | `demo`、`kit/service/account`、`codegen/internal/roost` |
| [OWN-3](#own-3) | 赠礼按 `FromSID` 路由 | v1.20.0 | `5bdac773` `c493a791` `ac5acfbe`（`054fdd66` 预算入配置） | `demo`、`codegen/internal/roost` |
| [OWN-4](#own-4) | activity 用 `Live`；候选校验 | v1.20.0 / v1.20.1 | `f051e24a` `46c4dfba` | `demo` |
| [OWN-5](#own-5) | 活动组文件 | v1.20.2 | `277e1252` | `kit/service/global/activity`、`demo`、`codegen/internal/roost` |
| [OWN-6](#own-6) | global `Bind` 幂等 | v1.23.0（本版） | `611d5d72` | `kit/service/global` |
| [CLK-1](#clk-1) | 双时钟 | v1.21.0 | `b9fc5342` | `clock`、`app`、`kit/service/*`、`timer`、`ai`、`actionflow`、`cmd/glsvet`、`demo` |
| [CLK-2](#clk-2) | match / chat / account 换钟，doctor | v1.21.0 | `fa472ee7` | `kit/service/{match,chat,account}`、`codegen/internal/roost` |
| [CLK-3](#clk-3) | 业务时间只许前进 | v1.22.0 | `3e77beb9` | `app` |
| [CLK-4](#clk-4) | activity / mail 回业务钟 | v1.22.0（拆分 v1.21.0） | `b9fc5342` `5a3c4a60` `3e77beb9` | `kit/service/global/activity`、`service/mail`、`kit/service/mail` |
| [CLK-5](#clk-5) | 高水位推进失败计数 | v1.23.0（本版） | `7b73aabc` | `app` |
| [CLK-6](#clk-6) | timer 排序与 priority | v1.21.0 | `5abae51e` | `timer`、`demo` |
| [OPS-1](#ops-1) | 服务指标默认 | v1.20.2 | `491aaf3b` | `servicemetrics`、`kit/mods`、`kit/service/*`、`codegen/internal/roost` |
| [OPS-2](#ops-2) | `DeleteSeries` | v1.23.0（本版） | `7b73aabc` | `metrics`、`robot/loadtest` |
| [OPS-3](#ops-3) | Ops 同步 bind、admin 期限 | v1.21.0 | `2c1c7be7` | `kit/ops`、`app` |
| [OPS-4](#ops-4) | Ops Bearer | v1.23.0（本版） | `7b73aabc` | `kit/ops` |
| [OPS-5](#ops-5) | CAS 统一计数 | v1.23.0（本版） | `7b73aabc` | `versionstore`、`kit/service/{rank,chat}` |
| [OPS-6](#ops-6) | 仪表盘面板 | v1.23.0（本版） | `fcc78ad0` `7b73aabc` | `demo/deploy`、`codegen/internal/roost` |
| [OPS-7](#ops-7) | robot 序号 | v1.23.0（本版） | `7b73aabc` | `robot/runner` |
| [TOOL-1](#tool-1) | pretag | v1.20.0 / v1.20.2 | `999dc672` `6c1538be` | `scripts` |
| [TOOL-2](#tool-2) | full 场景 add 序列 | v1.23.0（本版） | `fcc78ad0` | `codegen/scripts`、`.github/workflows`、根包 |
| [TOOL-3](#tool-3) | 冲突标记门禁 | v1.21.0 | `36220f34` | 根包 |
| [TOOL-4](#tool-4) | 示例实跑门禁 | v1.23.0（本版） | `ba13cb05` | 根包、`examples` |
| [TOOL-5](#tool-5) | 文档链接门禁 | v1.23.0（本版） | `d05a04a1` | 根包 |
| [TOOL-6](#tool-6) | mirror-local.sh | v1.22.0（扩展 v1.23.0） | `b15e70c8` `db67b8ee` `d483238e` `ba13cb05` | `scripts` |
| [TOOL-7](#tool-7) | 故障矩阵与验收锁 | v1.20.2（预跑 v1.23.0） | `6c1538be` `3e3350d5` `0a6155e5` | `scripts`、`kit/scripts/integration`、根包 |

---

<a id="app"></a>
## APP

<a id="app-1"></a>
### APP-1 App 单实例锁

**提交与首发**：v1.20.0。方案 `ed127736` `0f554aed` `84512f8f`；第 1 笔 `d4ac9853`；审查修复 `e3810ef1`（单次超时截到 `validUntil`）、`b291edb9`（接手用例去抖，只改测试）；审查收尾 `4959a0dd`（已 Lost 不预留 Release 时间）、`86f687cf`（停机不完整时 store 留到进程退出）、`cd8c1ad3`（补两条启动期回归）、`cbccacdb`（`SingletonStore.Close` 幂等）、`ad35bbcc`（续期与 Live 不共用连接池）、`3d7d228b`（契约注释与使用说明）。后续：`f9367785`（v1.20.2，NC-190 严格布尔，CFG 主题）、`d05a04a1`（v1.23.0，store Close 错误只报一次，见 APP-7）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `app/singleton.go:46` `SingletonStore` | 后端窄接口：CAS / CompareAndDelete / Get / Close；注释写明必须遵守 ctx 截止、内部重试只在同一 ctx 内、续期不能被 Live 拖住（`:31-45`） |
| `app/singleton.go:57` `SingletonOpener`、`:121` `App.Singleton` | bootstrap 安装 opener；未启用时不调用 |
| `app/singleton.go:110-117` | 缺省 15s / 3s / 5s，`singletonReleaseBudget = 3s` |
| `app/singleton.go:139` `readSingletonSettings`、`:170` `validate` | 读配置与三条时间关系；`app/config_validation.go:69` 并入 `ValidateServiceConfig` |
| `app/singleton.go:325` `cas` | 每次 CAS 记 `asked`；持有期间单次超时 = `min(renew_interval, validUntil − asked)` |
| `app/singleton.go:340` `timely` | Applied 只有在 `replied < asked + ttl − guard` 时作数 |
| `app/singleton.go:374` `acquire`、`:412` `confirmOwnValue` | 启动获取：重试、认领自己的值、到 `startup_wait` 报 `ErrSingletonHeld` / `ErrSingletonStoreUnavailable` |
| `app/singleton.go:453` `startRenewal`、`:474` `renewLoop` | 单 goroutine 固定节拍续期；四个分支：按时 Applied / 迟到 Applied / NotHeld / Unknown |
| `app/singleton.go:511` `lose` | 吸收态 Lost，`RuntimeFailure.Fail(ErrSingletonLost …)` |
| `app/singleton.go:531` `finish`、`:555` `release` | 统一收尾：停续期 → 按状态与 `mayRelease` 决定 Release → 只在没拿到锁或全部 Mod 停完时 Close store |
| `app/singleton.go:580` `checkHealth` | Held OK / Unknown Degraded / Lost 与未持有 Fail |
| `app/singleton.go:633` `openSingleton` | 打开 store、登记 `ModSingleton` 与 `ModSingletonIncarnation`、登记健康检查 |
| `app/app.go:222-245` | `run` 里的挂点：`openSingleton` → `defer finish` → `acquire` → `startRenewal`，都在 `sortMods`（`:257`）与第一个 Mod Init 之前 |
| `app/app.go:434-449` | 停机时 Mod 停止截止提前 `min(3s, total/2)`，已 Lost 为 0 |
| `kit/redis/singleton.go:44` `SingletonStore`、`:69` `newSingletonStore` | 两个独立客户端（`:24-25` 各 PoolSize 2，`:74` `MaxRetries = -1`）；`:145` `Close` |
| `kit/mods/name.go:14` | `mods.ModSingleton` 别名 |

**不变量与强制位置**：

| 不变量 | 强制位置 | 守卫测试 |
| --- | --- | --- |
| 锁拿到之前不 Init 任何 Mod | `app/app.go:222-245` 在 `:257` 之前 | `TestSingletonWaitsForTheHolderBeforeAnyModInit`（`app/singleton_test.go:576`） |
| 不抢别人持有的锁 | `acquire` 只在 `current` 为空或等于自己的值时成功 | `TestSingletonGivesUpAtStartupWaitWithoutTakingTheKey`（`:599`）、`TestSingletonKeepsWaitingWhenALostReplyHidesAnotherHolder`（`:765`） |
| 本地窗口不晚于键过期 | `cas` 记 `asked`，`hold` 设 `validUntil = asked + ttl` | `TestSingletonWindowStartsWhenTheRenewalWasAsked`（`:1003`） |
| Unknown → Lost 不晚于 `validUntil` | `cas` 截断单次超时 | `TestSingletonUnknownRenewalTimeoutLosesNoLaterThanTheWindowEnd`（`:926`）、`TestSingletonUnknownRenewalsLoseOnlyAtTheEndOfTheWindow`（`:864`） |
| 迟到的 Applied 不作数 | `timely` | `TestSingletonALateAppliedRenewalDoesNotCount`（`:1026`） |
| Lost 是吸收态，恰好一次 fail-stop、先围栏后唤醒 | `lose` 之后 `renewLoop` 返回；`RuntimeFailure` 先执行回调 | `TestSingletonNotHeldFailsOnceAndFencesBeforeShutdown`（`:802`） |
| 只在全部 Mod 停完且未失锁时 Release | `run` 各返回点置 `singletonReleasable`，`finish` 判 `state` | `TestSingletonReleasesOnlyAfterEveryModStopped`（`:1066`）、`TestSingletonIsNotReleasedWhenShutdownIsIncomplete`（`:1159`）、`TestSingletonIsReleasedWhenAModStopReturnsAnOrdinaryError`（`:1240`） |
| 已 Lost 不预留 Release 时间 | `app/app.go:443-446` | `TestSingletonLostLockLeavesTheReleaseBudgetToModStop`（`:1394`） |
| 停机不完整时 store 不关（Live 仍可用） | `finish` 末段 | `TestSingletonIsNotReleasedWhenShutdownIsIncomplete` 三个子例 |
| 续期不被 Live 拖住 | 两个独立客户端 | `TestSingletonStoreRenewalDoesNotWaitBehindStalledLiveQueries`（`kit/redis/singleton_integration_test.go:197`，integration） |
| 启用必须有 opener；配置关系 | `openSingleton`、`validate` | `TestSingletonOpenerIsRequiredOnlyWhenEnabled`（`:1440`）、`TestValidateServiceConfigPinsSingletonTimeRelations`（`:1471`） |

**控制流**（`run` 内，按执行顺序）：

1. 读配置、`ValidateServiceConfig`（含 singleton 三条关系）、日志、`NewRegistry`、`PhaseAppInit`。
2. `openSingleton`：未启用返回 nil；启用而无 opener → `ErrSingletonOpenerMissing`；打开 store，登记 `ModSingleton`、`ModSingletonIncarnation`、健康项 `singleton`（此时 Fail “not acquired”，ops 还没起，不可见）。
3. `defer finish(singletonReleasable, singletonReleaseDeadline)`；`acquire`：`CAS(nil, v)` → Applied 则持有；`current == v` 则认领并立即 Renew 一次以那次 `asked` 起算；别人的值则每 `renew_interval` 重试到 `startup_wait`；测试注入的 `signalSource` 在等待期间被监听，收到信号 `run` 返回 nil。
4. `startRenewal` → `renewLoop`：按时 Applied → `hold`、下一拍 `asked + renew_interval`；迟到 Applied → `markUnknown`、立即再续；NotHeld → `lose`；报错 / 超时 → 若 `replied ≥ validUntil − guard` 则 `lose`，否则 `markUnknown`、下一拍。
5. 业务时间守卫（CLK-3）→ Mod Init / Provide / Start（各 Start 之前 `startupFailure` 检查，APP-4）→ `Service.Init` → Serve。
6. 停机：Mod 停止截止提前 `releaseReserve`；按 APP-8 / APP-9 / 正常路径置 `singletonReleasable`。
7. `finish`：停续期并等它退出；Waiting 不 Release；Lost 记 `lock was lost; not releasing`；`!mayRelease` 记 `shutdown incomplete; leaving the key to expire`；否则 `release`（剩余时间为零则跳过）；拿过锁且 `!mayRelease` 时不关 store。

**失败与不确定结果**：CAS 报错或超时一律是 Unknown（结果未知），在窗口内重试、到窗口末尾判 Lost；Release 失败只记 Warn、键在 TTL 内过期；启动获取以报错结束时（最后一次 Acquire 可能已落地、回复丢失）状态仍是 Waiting、不 Release，下一次启动要等它过期（观察 6，维持现状）。

**测试**：

- 第 1 笔修前红（方案 §13 第 1 笔只摘录了例子，原文）：`#1 service started serving while the singleton key belonged to another process`，`#13 hooks ran [], want [first second] once`，`#12 ValidateServiceConfig = <nil>`；`kit/nest` 的 `TestRuntimeFailureFencesNestDispatch` 在原代码上 `FenceError after RuntimeFailure = <nil>`。完整输出记录未保存。
- `e3810ef1` 修前红（提交说明原文）：

  ```
  singleton_test.go:939: lost at validUntil+999.999459ms: the key may already
  belong to a new process while this one still serves
  ```

- `b291edb9`（只改测试）旧循环 + 慢启动红：`singleton_test.go:639: never took over the expired key`。
- `4959a0dd` 修前红：`mod stop deadline is 3s before the shutdown deadline, want 0s`。
- `86f687cf` 修前红（三个子例）：`store closed while a component that may still call Live is running`。
- `cbccacdb` 修前红：`Close #2 = redis: client is closed, want nil`。
- `ad35bbcc` 修前红：`renew while every Live connection is stalled = false "" context deadline exceeded, want applied`。
- `cd8c1ad3`：现有代码已满足，首跑即绿，没有修前红。
- 修后：`go test -race -count=3 ./app/ ./kit/nest/ ./kit/redis/` 通过；singleton 用例 `-race -count=50` 稳定；`kit/redis` integration 在隔离 Redis 上 `-race -count=3` 通过。
- 真实进程演练（第 5 笔，`~/.roost-it` 端口偏移 1000，自起 etcd）：SIGSTOP 旧进程后新进程等锁 15.004s、在 `flock` 处退出并释放；SIGCONT 后 10ms 判定失锁、156ms 退出；kill -9 后新进程等锁 15.005s；SIGTERM 后 40ms 起新进程等 3.0s（旧进程还在停）；等旧进程退出后再起 0ms。时间线全文见方案 §13 第 5 笔。

**性能**：锁只在启动、每 3s 续期、停机时各一次 Redis 往返，不在请求路径上；方案 §8.3 判断不需要性能对照，没有基准数据。

**未验证与风险**：两客户端在真实 Redis Cluster 上的 integration（`ROOST_REVIEW_CLUSTER` 用例跳过）；Redis Cluster 下的真实进程演练；跨主机 / 换卷（不在范围）。`c493a791` 与 obs34 之后的代码没有重跑演练（6b 在 `64acd782` 上测）。

**review 检查点**：

1. `app/app.go` `run` 每个 `return` 前 `singletonReleasable` 是否只在“全部 Mod 都停完”的路径上为 true：逐个核对 `:252`、`:259`、`:271`、`:277`、`:284`、`:289`、`:298`、`:307`、`:315`、`:321`、`:330`、`:337`、`:345`、`:355`、`:389`、`:508-510`；漏置为 false 的方向是安全的（键过期），误置为 true 是缺陷。
2. `renewLoop` 的 Unknown 分支用 `reply.replied` 与 `validUntil − guard` 比较（`:496-505`），而 `cas` 的截断用 `asked`（`:328-330`）：确认“截断 + 比较”合起来保证 `TestSingletonUnknownRenewalTimeoutLosesNoLaterThanTheWindowEnd` 的承诺，在 `ttl / renew / guard` 取边界值（如 14 / 3 / 3）时也成立。
3. `acquire` 认领自己的值后立即 Renew 一次（方案差异 1）：确认迟到 Applied 在启动获取阶段同样走这条路。
4. `finish` 在 `state == singletonWaiting`（启动等待失败）时关闭 store：确认此时没有任何 Mod 启动、没有组件可能调 `Live`。
5. `kitredis.SingletonStore` 关闭了 go-redis 自动重试（`MaxRetries = -1`）：确认 Cluster 下 `MaxRedirects` 的 MOVED / ASK 重试仍在同一个 ctx 内（`app.SingletonStore` 契约）。
6. `ErrSingletonLost` 的 fail-stop 经 `OnFail` 围栏 Nest 再唤醒 `run`：与 APP-4 的检查点一起看。

<a id="app-2"></a>
### APP-2 生成器装配单实例锁；etcd 两处修复

**提交与首发**：v1.20.0。`6863dbc3`（第 2 笔，codegen）、`364b763c`（`etcd.service_prefix` 结尾斜杠）、`b67d5945`（Discovery 租约已过期视为注销达成）、`49335ab5` / `fdcd8605`（方案状态）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `codegen/internal/roost/render.go:399` | bootstrap 写 `a.Singleton(kitredis.SingletonStore)` |
| `codegen/internal/roost/render.go:570` `serviceSingletonEnabled`、`:578` `projectInstallsSingleton`、`:585` `renderSingletonConfig` | 哪些服务启用、bootstrap 是否安装（有 redis Mod 或有 dataengine 服务）、配置段文本 |
| `codegen/internal/roost/render.go:760` `turnGeneratedSingletonOn` | `add mod dataengine` 之后把与生成文本逐字相同的 `enabled: false` 段翻成 true，改过的段保持并 WARN |
| `codegen/internal/roost/shutdown_budget.go:72` `generatedSingletonRelease`、`:93`、`:236-261` | 停机预算 +3s、摘要行 `+ 3s for the singleton release` 与回读正则 |
| `codegen/internal/roost/render_deploy.go:248` `startupAllowance`、`:1029` `kubernetesStartupFailureThreshold` | shell `HEALTH_ATTEMPTS` 与 k8s `startupProbe` 按 `startup_wait + 30s` |
| `codegen/internal/roost/catalog.go:47` | etcd 段 `service_prefix: /roost/services/` |
| `etcd/driver/discovery.go:203` `Deregister`、`:248` `isLeaseNotFound` | 租约已不存在视为注销达成；注册循环退出后再取消一次 keepalive |

**不变量**：opener 安装条件必须覆盖“默认启用 singleton 的服务”（否则启动即 fail-closed）；停机预算的 Release 份额与 App 的 `singletonReleaseBudget` 一致（`shutdown_budget.go:72` 注释写明镜像关系）。守卫：`codegen/internal/roost/singleton_promises_test.go` 六条（bootstrap 安装条件、只给 dataengine 服务打开、add mod 翻转与手改保持 + WARN、停机摘要含 Release 且能读回、doctor 计入 Release、部署启动等待）；`TestGeneratedEtcdServicePrefixSeparatesTheServerType`；etcd `TestDiscoveryDeregisterTreatsLeaseNotFoundAsDeregistered`、`TestAssemblyCloseTreatsLeaseNotFoundAsDeregistered`、`TestDiscoveryDeregisterStopsRegistrationThatCompletedDuringShutdown`、app `TestSingletonIsReleasedWhenAModStopReturnsAnOrdinaryError`；真机 `TestRealEtcdCloseAfterLeaseVanishedIsClean`（`-tags integration`）。

**测试**：

- 第 2 笔修前红（方案 §13 第 2 笔原文摘录）：`redis without dataengine: bootstrap does not install a.Singleton(kitredis.SingletonStore)`、`configs/service/config.account.yaml: a service without dataengine has singleton.enabled = "" (present false), want an explicit false`、`deploy/shell/install.sh lacks "  game) DEFAULT_HEALTH_ATTEMPTS=60 ;;\n"`、`compose game: start_period is not 60s`、`singleton on, total 105s: ok total_timeout …`。
- etcd 两处：记录写“真机回归修前红文本与演练日志相同”，演练日志原文为 `mod etcd stop: etcd Discovery: revoke: etcdserver: requested lease not found`；`service_prefix` 的修前红记录未保存。
- 修后：`go test -count=1 ./codegen/...` 全绿；生成 game-demo `go build ./... && go vet ./...`、`go test ./internal/service/game/`；`shellcheck deploy/shell/*.sh deploy/docker/*.sh deploy/k8s/*.sh` 无输出。

**未验证与风险**：kubeconform / `docker compose config` 未跑；真实 systemd / k8s 部署（E21 / E22）。

**review 检查点**：

1. `projectInstallsSingleton` 与 `serviceSingletonEnabled` 的条件是否一致：存在“服务默认启用但 bootstrap 不装 opener”的 manifest 组合吗（`-mods configdata,nest` 这类无 redis 的 dataengine 服务）。
2. 停机摘要正则（`shutdown_budget.go:261`）对没有 Release 段的旧文本逐字兼容，`roost project sync` 刷新不会误改用户手改过的段。
3. `isLeaseNotFound` 对被包裹 / 未转换的 gRPC NotFound 的判定是否只匹配“lease not found”描述，不会把别的 NotFound 当成注销成功。

<a id="app-3"></a>
### APP-3 删除 global 租约 API，停发选主 / 锁 capability

**提交与首发**：v1.20.0。`71c6fb6b`（第 2b 笔：停发 `ModRedisLock` / `ModEtcdElection`）、`40d89ac6`（第 3b 笔：删 global 租约 API）、`4bea90d0`（global / activity 注释不再说 global 负责存活）、`10e2e0ea`（使用说明与演练记录）。`88f33776`（v1.23.0）在 DEPLOYMENT §7.1 写明手工删除 `:lease:*` 键。

**改动文件与关键符号**：`kit/service/global/service.go`（只剩路由五个方法）、`kit/service/global/types.go:43-44`（570105～570109 退役注释）、`global_rpc.go` / `routing_rpc_gen.go`（`go generate` 重新生成）、`kit/mods/name.go`（删两个常量）、`kit/redis/redis_mod.go` 与 `kit/etcd/etcd_mod.go`（不再登记 capability）、`redis/lock.go` / `etcd/election.go`（注释写明进程级单例用 `app.Singleton`）、`codegen/internal/roost/framework_services.go`（`global:` 段去掉 `lease_ttl`）、`etcd/driver/discovery.go` 与 `kit/etcd/etcd_mod.go` 注释（Discovery 只做发现）。

**不变量**：路由 `Bind` / `Resolve` / 迁移与错误码 570101～570104、570110、570125 不变；退役号不复用（`errcode_test.go` 的“退役号不得复用”检查覆盖 570105～570109、570111）。

**测试**：纯删除，没有先红后绿。用例去向：`global_test.go` 11 条、`promises_test.go` 两条、`guards_promises_test.go` 一条、`rr_20260929_round2_test.go` 整个文件随租约删除；混合用例改为覆盖迁移完成与二次 `Bind` 的 `conflict:bind`。修后 `go test -race -count=1 ./kit/service/global/...`、`go test -tags integration -count=1 -p 1 ./kit/service/global/... ./kit/service/integration/...`（隔离 Redis）、`go test -count=1 ./codegen/...`、`go generate ./...` porcelain 为空。

**未验证与风险**：仓外调用方；旧键不自动清理。

**review 检查点**：

1. `git grep -n 'AcquireLease\|RenewLease\|ReleaseLease\|LiveGames\|GameLease\|ModRedisLock\|ModEtcdElection'` 在非历史文档外应无结果。
2. `routing_rpc_gen.go` 是生成文件：`go generate ./...` 后 porcelain 为空即证明与 `global_rpc.go` 一致。
3. `redisdriver.Assemble(cfg).Locks`、`etcddriver.Assemble(cfg).Election` 仍可用（迁移路径）。

<a id="app-4"></a>
### APP-4 `RuntimeFailure.OnFail` 与统一 fail-stop

**提交与首发**：v1.20.0 `d4ac9853`（`OnFail`、kit/nest 登记、启动阶段检查点）、`cd8c1ad3`（补启动期两条回归）；v1.21.0 `2c1c7be7`（NC-232）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `app/runtime_failure.go:35` `Fail` | 锁下判断并置位 `failed`、取出回调列表；锁外逐个执行；`defer` 投递 `done` |
| `app/runtime_failure.go:61` `OnFail`、`:77` `runHook` | 失败前追加、失败后立即调用；每个回调 `recover`，panic 并入 `Err` |
| `kit/nest/nest_mod.go:205-207` | `Provide` 里 `failure.OnFail(m.engine.Fence)` |
| `app/app.go:209` `startupFailure` | 启动检查点：`:283`（共享 Mod 每个 Start 之前）、`:329`（服务专属 Mod）、`:344`（全部 Start 之后）、`:370`（`Service.Init` 之后） |
| `app/app.go:198-206` | `failureReturned` 与 defer：失败没进过返回值就包成 `app: runtime failure after shutdown began: …` 并入 |
| `app/app.go:414-417` | select 的 `Done` 分支置 `failureReturned` |

**不变量**：每个回调恰好调用一次、按登记顺序、先于 `Done` 投递（失败前登记的）；回调 panic 不阻止 `Done`；回调里调 `Fail` 不死锁；fail-stop 不能以 0 退出。守卫：`TestRuntimeFailureOnFailRunsHooksOnceInOrderBeforeDone`（`app/singleton_test.go:1517`）、`TestRuntimeFailureOnFailSurvivesPanicsAndReentry`（`:1549`）、`TestRuntimeFailureOnFailConcurrentRegistrationRunsEachHookOnce`（`:1579`）、`TestRunStopsStartingModsAfterARuntimeFailure`（`:1303`，单实例锁关 / 开各一遍）、`TestSingletonLostDuringStartupStopsTheModsWithoutReleasing`（`:1340`）、`TestRuntimeFailureFencesNestDispatch`（`kit/nest/nest_mod_test.go:106`）、`TestRuntimeFailureDuringShutdownIsReturned` / `TestRuntimeFailureThatStartsTheShutdownIsReturnedOnce`（`app/late_runtime_failure_promises_test.go:15` / `:36`）。

**控制流**：`Fail(err)` → 回调（Nest `Fence`：只拿 `lifecycleMu` 与 dispatcher `mu` 各一次、不做 I/O）→ `done <- err` → `run` 的 select 醒来 → 优雅停机（`Service.Shutdown` 断开连接、Mod 逆序停）→ defer 链：`businessTime.stop` → `singleton.finish` → `failureReturned` 补报 → 写退出日志（APP-12）→ 关文件日志。

**失败与不确定结果**：失败之后才登记的回调在登记者 goroutine 上立即执行，但不保证先于 `Done` 投递（`runtime_failure.go:57-60` 注释写明）；Nest `Fence` 不打日志。

**测试**：

- 第 1 笔修前红：`#13 hooks ran [], want [first second] once`；`FenceError after RuntimeFailure = <nil>`（APP-1 同段）。
- NC-232 修前红（[问题记录](../../bug/RR-20261005-NC-232.md)原文）：

  ```
  --- FAIL: TestRuntimeFailureDuringShutdownIsReturned (0.00s)
      late_runtime_failure_promises_test.go:27: run error = <nil>, want the runtime failure that happened during shutdown
  ```

- `cd8c1ad3`：首跑即绿，没有修前红。
- 修后：`go test -race -count=3 ./app/`（含 `singleton_test.go`、`startup_cleanup_promises_test.go`）通过。

**review 检查点**：

1. `Fail` 在第一次调用时把 `hooks` 置 nil 后才执行（`:46-48`）：并发的 `OnFail` 在 `failed=true` 之后进入立即调用分支，确认不存在“既不在列表里、也不立即调用”的窗口。
2. 所有 `OnFail` 回调都不阻塞：`git grep -n 'OnFail('` 列出全部登记点，逐个确认不做 I/O、不在快池 worker 上等。
3. `failureReturned` 的 defer 登记在 `singleton.finish` 的 defer 之前（`:199` 早于 `:229`），所以后执行：确认 `finish` 期间发生的失败也被并入（`TestRuntimeFailureDuringShutdownIsReturned`）。
4. 启动检查点之间（例如某个 Mod 的 `Start` 内部很长）发生的失败要等到下一个检查点才停：确认没有 Mod 依赖“失败后立刻不再被 Start”。

<a id="app-5"></a>
### APP-5 `Live` 与 C5 契约

**提交与首发**：v1.20.0 `d4ac9853`（`Live`）、`ad35bbcc`（独立 Live 客户端）；v1.21.0 `bd6df5e5`（C5：注释、USER_GUIDE、用例）。

**改动文件与关键符号**：`app/singleton.go:59-71`（`SingletonLiveness` 与 C5 契约注释）、`:74` `ModSingleton`、`:93` `SingletonLiveMaxSIDs = 200`、`:601` `singletonLiveness.Live`（空 `serverType` 报错、空 `sids` 直接返回、超过 200 报错）、`:672` 登记；`kit/redis/singleton.go:116` `Get`（逐键 GET，单机一次 pipeline，Cluster 由客户端按槽拆分）。

**不变量**：活性只有锁这一个事实来源；不引入“停机中”中间值。守卫：`TestSingletonLiveReportsSidsHoldingTheLock`（`app/singleton_test.go:1604`）、`TestSingletonLiveCountsAStoppingProcessUntilRelease`（`:1097`，`Service.Shutdown`、服务 Mod Stop、共享 Mod Stop 三处查 `Live` 都得到本 sid，Release 后键不在）、`TestSingletonStoreGetReadsEveryKeyInOrder` / `TestSingletonStoreGetAcrossClusterSlots`（`kit/redis/singleton_integration_test.go:146` / `:152`）。

**测试**：C5 用例钉住现有行为，本来就绿，没有修前红（B9 / C5 方案 §5）。

**源码与记录不一致**：App 锁方案 §3.6 写“一次调用最多 `MaxPageSize` 个 sid（与 `LiveGames` 的上限相同）”；`MaxPageSize` 已在第 3b 笔随 global 租约删除，源码是 `app.SingletonLiveMaxSIDs = 200`（`app/singleton.go:93`），CHANGELOG v1.20.0 写的也是 200。以源码为准。

**review 检查点**：

1. `Live` 对 store 报错整体返回错误（不返回部分结果）：确认 activity 据此“本拍不开窗”而不是把空结果当“只有自己”（OWN-4 检查点 2）。
2. `Get` 在 Cluster 下不依赖多键命令：`TestSingletonStoreGetAcrossClusterSlots` 需要 Cluster，本地默认跳过，review 时注意它是否被 CI 的 `redis-cluster-suites.sh` 覆盖（`ci_service_redis_test.go` / `integration_coverage_promises_test.go` 钉住 `./kit/redis` 在 Redis job 与 Cluster 脚本里）。

<a id="app-6"></a>
### APP-6 `operation.Lifetime`、停机契约骨架、glsvet stophints（A3）

**提交与首发**：v1.20.2 `50f2ac2a`（`afb93579` 标注 DECISIONS-PENDING）。前置修复 NC-170～174（`c99a687d`，v1.20.2，NONCORE 主题）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `internal/operation/lifetime.go:23` `Lifetime`；`:31` `Begin`、`:42` `End`、`:53` `Stop`、`:67` `Stopping`、`:78` `Wait(ctx)` | 准入 / 在途计数 / 幂等关准入 / 在 ctx 内等排空（已排空时任何 ctx 都返回 nil，超时保留计数可重试） |
| `bus/jetstream_rpc.go`、`sync/syncbus/driver/jetstream.go`、`sync/syncbus/mirror/envelope.go` | 三份手写门迁移到 `Lifetime`；另有 `nestwal/wal.go`、`nestwal/committer.go`、`dataengine/engine/projector.go`、`remoteentity/snapshot_client.go` 在用 |
| `internal/stopcontract/stopcontract.go:30` `Hooks`、`:51` `Check`、`:110` `CallerReleases` | 骨架：首次超时返回 ctx 错误且资源保留 → 未放行时重试仍超时 → 放行后新 ctx 重试返回 nil 且资源确实释放 → 再调用返回 nil |
| `cmd/glsvet/stophints.go`、`cmd/glsvet/main.go:37-39` | `-stophints`（缺省开）：带 ctx 的停止类函数里不受 ctx 约束的通道接收（跟进一层同包 helper）打印 `hint:` |

**套用骨架的停止入口**：`manager/stop_contract_test.go`、`kit/nest/stop_contract_test.go`（`TestNestModStopContract`）、`sync/syncbus/driver/stop_contract_test.go`、`etcd/driver/stop_contract_test.go`、`sync/syncbus/mirror/stop_contract_test.go`、`remoteentity/stop_contract_test.go`、`bus/stop_contract_test.go`、`kit/ops/stop_contract_test.go`（`TestOpsStopContract`）、`codegen/internal/roost/player_tcp_stop_contract_promises_test.go`（生成 TCP，注入骨架源码运行）、`kit/remoteentity/remote_mirror_mod_promises_test.go`、`remoteentity/snapshot_client_promises_test.go`。

**不变量**：停止返回 nil ⇒ 回调已静止、依赖可释放；重试收敛；`Released` 观察真实后果而不是被测对象自己的字段。守卫：`TestLifetimeStopDrainsOnlyAdmittedCalls` / `TestLifetimeWaitIsBoundedRetryableAndReportsTheRealDrain` / `TestLifetimeZeroValueWaitWithoutCallsReturnsNil`（`internal/operation/lifetime_test.go:10` / `:38` / `:68`）；`TestCheckPassesAStopperBuiltOnTheSharedLifetime` / `TestCheckCatchesKnownWrongStoppers`（`internal/stopcontract/stopcontract_test.go:155` / `:162`）；`TestStopHints*`（`cmd/glsvet/stophints_test.go:21-117`）。

**测试**（[A3 证据](../../bugfix/evidence/a3-stop-contract-20261005/README.md)）：骨架对故意写错的对象红——`retry Stop while the work is still in flight = <nil>`、`Stop returned without releasing the resource`；套到 NC-170 / 171 / 173 / 174 / NC-90 的修前实现、生成 TCP 的 NC-83 修前形状上都红（各自红文本在证据目录的 `*-prefix-red.txt`）；骨架在当时的 main 上发现 etcd `Assembly.Close` 第 4 步红（停完再 Close 返回 `context canceled`），补修为 `driver.Client.Close` 只关一次。迁移后原有 NC-90 / NC-172 / NC-174 回归 `-race -count=3` 通过。glsvet：NC-173 修前 `discovery.go` 命中 `Deregister(ctx) calls waitLoopDone …`，修后全仓非测试文件 0 条。

**未验证与风险**：A3 ②（排空下沉到 `ISyncBus`）未做；`worker.Pool` 未改用 `Lifetime`。

**review 检查点**：

1. `Lifetime.Wait` 先查排空再看 ctx（“已排空且 ctx 已结束”返回 nil）：确认三处迁移后的调用方没有依赖旧的随机返回 ctx 错误。
2. 消息回调里只有 `Begin` / `End`（一次短临界区），等待只在停止入口：在 syncbus / mirror 的迁移里确认没有在快池或消息回调里调 `Wait`。
3. 新增的停止入口是否都套了骨架：`git grep -ln 'func .*StopWithContext\|func .*) Close(ctx' -- '*.go'` 与上面的列表对照。

<a id="app-7"></a>
### APP-7 `operation.Serial` 与 kit Mod 停止收敛

**提交与首发**：v1.23.0 `d05a04a1`（RR-20261006-10，`Serial`、kit Mod 串行与数据竞争）、`02c8a10d`（roost-coding 写入口径）；v1.21.0 `2c1c7be7`（NC-233 Redis Mod、NC-234 remote_entity Mod）。驱动层的 Close 口径（redis / mongo / etcd / nats driver）属于 DRV 主题，本条只覆盖 `internal/operation.Serial` 与 kit Mod 一侧。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `internal/operation/serial.go:14` `Serial`、`:20` `Lock(ctx)`、`:39` `Unlock` | 容量 1 的通道；先非阻塞尝试，再在调用方 ctx 内等；零值可用（`sync.Once` 建通道） |
| `kit/mongo/mongo_mod.go:25`、`:135-138` | `StopWithContext` 用 `stopSerial` 串行；`mu` 保护 `client` |
| `kit/nats/nats_mod.go:32`、`:180-183` | 同上；`mu` 保护健康检查读的 `asm` |
| `kit/redis/redis_mod.go:22-23`、`:131-136` | `sync.Mutex` `mu` 保护 `asm`；`StopWithContext` 持锁；第一次 Stop 先取出并置空 `asm` 再 Close（NC-233） |
| `kit/redis/singleton.go:145` | 单实例锁 store 的 Close 错误只报给第一次（v1.20.0 是 `sync.Once` 粘滞返回同一结果） |
| `kit/remoteentity/remote_entity_mod.go` `StopWithContext` | 失败记 Warn `remote_entity mod: stop incomplete`、保留 Assembly；成功才记 `stopped`（NC-234） |

**不变量**：重复 Close / Stop 返回 nil，第一次的错误只报给第一次调用；并发的后到者等第一个做完，等待受它自己的 ctx 约束，ctx 先结束返回 ctx 错误、对象状态不变、可再调用。守卫：`TestSerialLaterCallerWaitsWithinItsOwnContext`（`internal/operation/serial_test.go:11`）、`TestSingletonStoreCloseErrorIsReportedOnce` / `TestRedisModConcurrentStopIsSafe`（`kit/redis/close_contract_promises_test.go:19` / `:36`）、`kit/mongo/close_contract_promises_test.go`、`kit/nats/close_contract_promises_test.go`、`TestRedisModStopConvergesAfterACloseError`（`kit/redis/stop_retry_promises_test.go:18`）、`TestRemoteEntityModDoesNotLogStoppedWhenStopFails`。

**测试**：

- RR-20261006-10 修前红（[问题记录](../../bug/RR-20261006-10.md)原文，kit 部分）：

  ```
  --- FAIL: TestSingletonStoreCloseErrorIsReportedOnce                      (kit/redis)
      close_contract_promises_test.go:31: Close #2 = redis: client is closed, want nil: the first error is reported once
  --- FAIL: TestRedisModConcurrentStopIsSafe / TestMongoModConcurrentStopIsSafe / TestNatsModConcurrentStopIsSafe
      WARNING: DATA RACE（redis_mod.go:116/120、mongo_mod.go:117/129、nats_mod.go:183/197）
      testing.go:1865: race detected during execution of test
  ```

  `Serial` 是新类型，`TestSerialLaterCallerWaitsWithinItsOwnContext` 记录未保存修前红文本（修前不能编译）。
- NC-233 修前红（[问题记录](../../bug/RR-20261005-NC-233.md)原文）：

  ```
  --- FAIL: TestRedisModStopConvergesAfterACloseError (0.00s)
      stop_retry_promises_test.go:30: Stop after the pool was closed = redis: client is closed, want nil: retrying can never succeed
  ```

- NC-234 修前红（[问题记录](../../bug/RR-20261005-NC-234.md)原文）：

  ```
  --- FAIL: TestRemoteEntityModDoesNotLogStoppedWhenStopFails (0.00s)
      stop_log_promises_test.go:29: a failed stop logged success:
          time=2026-10-06T07:32:14.551+08:00 level=INFO msg="remote_entity mod: stopped"
  ```

- 修后：`go test -race -count=3 ./internal/operation ./redis/driver ./mongo/driver ./etcd/driver ./nats/driver ./kit/redis ./kit/mongo ./kit/nats` 通过；私有 redis-server 上 `-tags integration -race -count=3 -p 1 ./redis/driver ./kit/redis` 通过。NC-233 用例原来靠“再调一次驱动 Close 得到 ErrClosed”制造 Close 错误，驱动幂等后改为先关底层 `Raw()` 连接池。

**未验证与风险**：三个 toxiproxy 用例与 `TestSingletonStoreGetAcrossClusterSlots` 本批跳过；Close 路径没有用真实 Mongo / etcd / NATS（用不可达地址验证）。

**review 检查点**：

1. roost-coding 写“停止入口的并发串行用 `operation.Serial`（不用 `sync.Mutex`）”，而 kit Redis Mod 用的是 `sync.Mutex`（`kit/redis/redis_mod.go:23`）：修复记录的理由是 go-redis Close 不等在途命令、持锁很短。确认 `StopWithContext` 持锁期间没有任何可能阻塞的调用，否则后到者的等待不受自己的 ctx 约束。
2. `Serial.Lock` 返回 nil 后调用方必须恰好 `Unlock` 一次：检查 mongo / nats Mod 与 driver 的每条返回路径（`defer` 是否紧跟在成功的 `Lock` 之后）。
3. Mongo / Nats Mod 在串行器上等到 ctx 结束时“保留 client / asm、下次 Stop 继续”：确认健康检查此时读到的状态与“停机未完成”一致。

<a id="app-8"></a>
### APP-8 停机阶段 lifecycle hook 受预算（NC-231）

**提交与首发**：v1.21.0 `2c1c7be7`。

**改动文件与关键符号**：`app/app.go:812` `emitLifecycleWithin`（goroutine 里 `emitLifecycle`，在 ctx 内等；到期返回 `finished=false`；与到期同时返回的按已返回算）；调用点 `:454-466`（`service.stopping`：`!finished` 时直接返回，不调 Shutdown、不停 Mod、`singletonReleasable` 保持 false）与 `:516-522`（`service.stopped`：只并入错误）。启动阶段的 hook 仍用同步的 `emitLifecycle`（`:181`、`:350`、`:373`）。

**不变量**：hook 可能正用着 Service / Mod 的能力，超时后不在它底下拆依赖；单实例锁释放规则不变。守卫：`TestShutdownLifecycleHooksStayWithinTheShutdownBudget`（`app/shutdown_hooks_promises_test.go:42`，两个阶段子例）；`TestAppReturnsServiceStoppingLifecycleError`（hook 返回普通错误）不变。

**测试**：修前红（[问题记录](../../bug/RR-20261005-NC-231.md)原文）：

```
--- FAIL: TestShutdownLifecycleHooksStayWithinTheShutdownBudget (10.61s)
    --- FAIL: TestShutdownLifecycleHooksStayWithinTheShutdownBudget/service.stopping (5.30s)
        shutdown_hooks_promises_test.go:79: run did not return 5.3s after a service.stopping hook that ignores its ctx (shutdown.total_timeout 300ms)
    --- FAIL: TestShutdownLifecycleHooksStayWithinTheShutdownBudget/service.stopped (5.30s)
        shutdown_hooks_promises_test.go:79: run did not return 5.3s after a service.stopped hook that ignores its ctx (shutdown.total_timeout 300ms)
```

修后两个阶段都在预算附近返回 DeadlineExceeded；stopping 卡住时 Shutdown 0 次、Mod 停 0 次、不 Release；stopped 卡住时 Shutdown 1 次、Mod 停 1 次。`go test -race -count=3 ./app/` 通过。

**未验证与风险**：真实进程里业务 hook 卡住时的 SIGTERM → 宽限期时序。

**review 检查点**：

1. `service.stopping` 的 hook 用的是 `shutdownCtx`（完整预算），而 Mod 停机用的是提前了 `releaseReserve` 的 `modStopCtx`：确认 hook 吃掉大部分预算时 Mod 停机仍按剩余时间如实超时（`stopModsReverseBefore`）。
2. ops 的就绪位 hook（`kit/ops/ops_mod.go:126-134`）在 `service.stopping` 阶段：确认它不阻塞，不会让 `/readyz` 在 hook 超时路径上一直报就绪。

<a id="app-9"></a>
### APP-9 启动失败先收回 Service 已启动的部分（NC-193）

**提交与首发**：v1.20.2 `d6550a16`。

**改动文件与关键符号**：`app/app.go:360-392`（Init 失败与 Init 之后的启动失败合成一条收尾）、`:553-554` `startupCleanupTimeout = 5s`、`:561` `shutdownAfterStartupFailure`（goroutine 里调 `Shutdown`，recover panic；返回 nil → 已停；ctx 错误 / 超时 / panic → 未停；其他错误 → 已停并并入）、`app/service.go`（`Service` 注释：启动失败时也会调用 `Shutdown`，必须容忍部分初始化）。

**不变量**：Service 收不回时不停 Mod、不释放锁（与正常停机“Shutdown 不完整就保留依赖”一致）。守卫：`TestServiceInitFailureStopsWhatInitStartedBeforeTheMods`、`TestStartupCleanupThatDoesNotFinishKeepsTheModsAndTheLock`（`app/startup_cleanup_promises_test.go:38` / `:82`，配合 / 不配合两种）；既有 `TestSingletonIsReleasedAfterAStartupFailureStopsTheMods`（`app/singleton_test.go:1257`）不变。

**测试**：修前红（[nc193-red.txt](../../review/evidence/noncore-review-20261005-n14/nc193-red.txt)，摘录其中的断言行，原文）：

```
    startup_cleanup_promises_test.go:71: Service.Shutdown called 0 times after a failed Init, want 1: what Init started keeps running
--- FAIL: TestServiceInitFailureStopsWhatInitStartedBeforeTheMods (0.00s)
    startup_cleanup_promises_test.go:126: mods stopped 1 times while Service.Shutdown had not finished; the service may still be using them
    startup_cleanup_promises_test.go:117: run did not return 15s after a startup failure whose Service.Shutdown never finished
--- FAIL: TestStartupCleanupThatDoesNotFinishKeepsTheModsAndTheLock (25.01s)
    --- FAIL: TestStartupCleanupThatDoesNotFinishKeepsTheModsAndTheLock/cooperative_timeout (5.00s)
    --- FAIL: TestStartupCleanupThatDoesNotFinishKeepsTheModsAndTheLock/uncooperative (20.00s)
```

修后 `app` `-race -count=3` 通过；生成 game-demo build / vet / test 通过。

**源码注释问题（不是文档与源码冲突）**：`app/app.go:550-552` 是 `stopModsReverse` 的文档注释，却紧挨着放在 `startupCleanupTimeout` 常量（`:553-554`）上方，`go doc` 会把它算作常量注释；函数本身在 `:594`，没有文档注释。只影响可读性，行为无关；建议下次动这段时把注释移回函数上方。

**review 检查点**：

1. `shutdownAfterStartupFailure` 超时返回后 `Shutdown` goroutine 仍在跑：确认 `run` 返回后进程退出是唯一的回收方式（生产路径），测试里再次 `Run` 的泄漏可接受。
2. 仓外 `app.Service` 实现若在 `Shutdown` 里解引用 Init 才设置的字段会 panic → “收尾不完整” → 不释放锁、键 TTL 过期：确认这是安全方向（不会形成两个写者）。
3. `stopIncomplete(out.err)` 的分类（`:584`）与正常停机路径对 `Shutdown` 返回 ctx 错误的处理一致。

<a id="app-10"></a>
### APP-10 `/readyz`：Degraded 算就绪（D1）

**提交与首发**：v1.21.0 `f6828f17`（`ce79ef18` 标注）。

**改动文件与关键符号**：`health/health.go:17` `StatusDegraded`、`:31-38` 聚合规则注释与 `Snapshot.Degraded`、`:43` `DegradedResults`、`:174-183` 聚合（OK 不影响、Degraded 只置位、Fail 与未知 Status 让 `OK=false`）；`kit/ops/ops_mod.go:246` `handleReady`（200 条件仍是“就绪位 ∧ `deps.OK`”，响应体加 `degraded` 与 `degraded_dependencies`，`:263`）；`app/singleton.go:577-592` `checkHealth` 注释。

**不变量**：聚合规则只在 `health.Registry.Snapshot` 一处。守卫：`TestReadyzTreatsDegradedAsReadyAndNamesTheDegradedChecker`、`TestReadyzStillFailsOnFailOrNotReady`（`kit/ops/readyz_degraded_promises_test.go:46` / `:67`，后者修前修后都绿，作控制）；`TestRegistrySnapshotCountsDegradedAsAvailable`（`health/health_test.go:54`，新字段，修前不能编译）。

**测试**：修前红（[D1 方案 §4](../../feature/D1-READYZ-DEGRADED-IS-READY-2026-10-06.md)原文）：

```
readyz_degraded_promises_test.go:52: /readyz with a degraded checker = 503 ok=false, want 200 ok=true: degraded still serves
```

修后 200、`ok=true`、`degraded=true`，`degraded_dependencies` 只有 `singleton`。

**review 检查点**：

1. `git grep -n 'Snapshot(\|\.OK\b' -- '*.go'` 找出 `health.Snapshot.OK` 的全部读取方，确认只有 Ops 一处聚合消费者，且没有别处把 `OK` 当“全部 OK”。
2. 四个 Degraded 来源（`app/singleton.go` `checkHealth`、`sync/entitysync/manager.go` `CheckHealth`、`kit/remoteentity/remote_entity_mod.go` `checkHealth`、`kit/dataengine/mod.go` 健康检查）确实都是“还能服务”的状态，没有应当是 Fail 的条件被报成 Degraded。

<a id="app-11"></a>
### APP-11 `/readyz` 每个 checker 的期限

**提交与首发**：v1.23.0 `7b73aabc`（第十二轮 kit 批 §3）。

**改动文件与关键符号**：`health/health.go:66-71` `DefaultCheckTimeout = 1500ms`、`:97` 新 Registry 缺省期限、`:124` `SetCheckTimeout`（≤0 恢复缺省）、`:148` `Snapshot`（为每个 checker `begin` 一次调用，`waitCtx` = 请求 ctx + 期限）、`:191` `begin`（已有在途调用就复用；调用 ctx = `context.WithoutCancel(请求 ctx)` + 期限）、`:211` `wait`（到期且未返回 → Fail，区分 `check timed out` 与 `check abandoned`；与期限同时返回的按已返回算）。

**不变量**：同一 checker 同一时刻只有一次调用；`/readyz` 总时长不超过一个期限（并发）。守卫：`TestReadyzReportsAStuckCheckerAsFailInsteadOfHanging`（`kit/ops/readyz_checker_deadline_promises_test.go:21`）、`TestSnapshotBoundsEveryCheckerByOneDeadline`（`health/checker_deadline_promises_test.go:12`）。

**测试**：修前红（第十二轮 kit 批 §3 原文）：

```
readyz_checker_deadline_promises_test.go:60: /readyz did not answer within 3.5s: one checker that never returns hangs the whole probe
```

修后两次探针都在约 1.5s 返回 503，卡住的报 Fail 并写明期限，正常的仍是 ok，卡住的 checker 只被调用 1 次。`health` 用例：两个卡住的只等一个期限，配合 ctx 的 checker 拿到带期限的 ctx。

**review 检查点**：

1. 复用的在途调用的 ctx 期限属于第一个发起它的探针（`begin` 里建），后来的探针按自己的 `waitCtx` 等：确认后到探针不会因为第一个探针的期限先到而误报，也不会多等。
2. `call.result` 由 checker goroutine 在 `close(call.done)` 之前写入、`wait` 在 `<-call.done` 之后读：确认没有数据竞争（`-race` 覆盖了两个用例）。
3. 卡住的 checker 永不返回时，`registeredChecker.call` 一直非 nil，后续每次快照都复用它并在期限内报 Fail：确认这正是“不会每次探针多一个 goroutine”的承诺。

<a id="app-12"></a>
### APP-12 退出原因写进文件日志

**提交与首发**：v1.23.0 `611d5d72`（收尾第 4 批 A13，RR-20261006-07）。相关：RR-20261005-NC-165（v1.20.2，`e798a759`，NONCORE 主题）。

**改动文件与关键符号**：`app/app.go:164-169`：最先登记的 defer，`runErr != nil` 时 `slog.Error("app run failed", "err", runErr)` 再 `flog.Close()`；`run` 改为命名返回值 `runErr`（`:104`，NC-232 引入）。

**不变量**：这个 defer 最后执行，`runErr` 已含停机期间并入的 RuntimeFailure（APP-4）与单实例锁收尾结果。守卫：`TestRunWritesTheExitReasonToTheFileLog`（`app/exit_reason_log_promises_test.go:29`）。

**测试**：修前红（[问题记录](../../bug/RR-20261006-07.md)原文）：

```
--- FAIL: TestRunWritesTheExitReasonToTheFileLog (0.00s)
    exit_reason_log_promises_test.go:48: the exit reason "mod init failed: exit reason 7c1e" is not in the file log:
        ... msg="starting server" ...
        ... msg="mod init" mod=exit_reason
```

修后 `go test -race -count=3 ./app` 通过。

**review 检查点**：

1. `run` 在 `flog.Init` 之前的返回（读配置失败、`ValidateServiceConfig` 失败、`flog.Init` 失败，`:124-158`）不经过这个 defer：确认这些错误由生成 main 的 `server exit` 打到 stderr，文件日志本就不存在。
2. `runErr` 只经命名返回值与 defer 修改：确认没有 `return` 用遮蔽的局部变量绕过它。

<a id="app-13"></a>
### APP-13 App 能力与 Mod 依赖

**提交与首发**：`ModSingleton` v1.20.0 `d4ac9853`；`ModBusinessClock` v1.21.0 `b9fc5342`；`ModSingletonIncarnation` v1.23.0 `d483238e`（O-M6-6，Remote 锁接管的完整实现属于 REM 主题）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `app/singleton.go:672-682` | `openSingleton` 登记 `ModSingleton` 与 `ModSingletonIncarnation`（token 取锁值第一段） |
| `app/singleton.go:76-90` `SingletonIncarnation{Key, Sid, Token}` | 能拿到它即说明本进程已持有该锁；只在 `singleton.enabled=true` 时登记 |
| `app/registry.go:43` | `NewRegistry` 按 `time.logic_offset` 登记 `clock.NewBusiness(offset)` |
| `app/business_clock.go:13` `ModBusinessClock`、`:25` `BusinessClock` | 取 Registry 里的业务钟，没有就退回 `clock.Process()` |
| `kit/mods/name.go:14-16` | `ModSingleton`、`ModSingletonIncarnation` 别名 |
| `kit/remoteentity/remote_entity_mod.go:182-183` | `incarnation.Sid == m.localSid` 时 `deps.Incarnation = &ProcessIncarnation{Holder: Key, Token}` |
| `remoteentity/assemble.go:26-28`、`:113-114` | `AssemblyDeps.Incarnation` 非 nil 时第一次取实体共享锁就接管同 sid 上一代 |
| `demo/internal/service/game/activity.go.tmpl:95-100` | `startActivity` 第一步查 `app.ModSingleton`，取不到报错 |

**不变量**：Mod 在锁拿到之后才 Init / Provide，读到 `SingletonIncarnation` 时锁已持有；未启用 singleton 时不登记，依赖方不得接管 / 不得退化为“只有自己”以外的语义。守卫：`TestSingletonIncarnationIsTheHeldLocksIdentity`（`app/singleton_incarnation_promises_test.go:12`）、`kit/remoteentity/lock_incarnation_promises_test.go`、`TestActivityRefusesToStartWithoutTheAppLockLiveness`（`demo/internal/service/game/activity_test.go.tmpl:187`）、`TestBusinessClockFollowsTheConfiguredOffset`（`app/business_clock_promises_test.go:22`）。

**测试**：`SingletonIncarnation` 是新 API，记录未保存 app 侧的修前红文本；O-M6-6 的行为红绿（修前重启后的写 2408ms 等旧租约，修后 33ms、无多写）见 [Mirror 第 6 步观察 §7](../../feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md)。

**review 检查点**：

1. `RemoteEntityMod` 只在 `incarnation.Sid == m.localSid` 时接管：确认 `localSid` 与 App 写进配置的 `sid` 是同一来源，不同 sid、旧格式 token、`singleton.enabled=false` 一律不接管。
2. `app.BusinessClock(nil)` 的退回路径只给没有 Registry 的框架库与测试用：`git grep -n 'BusinessClock(nil)'` 列出调用点（目前 game-demo 两处兜底），确认生产路径都传了 Registry。

---

<a id="own"></a>
## OWN

<a id="own-1"></a>
### OWN-1 租约状态机期的三处修复（已被 OWN-2 取代）

**提交与首发**：v1.20.0。`18bb86ae`（RR-20261004-10）、`a28a3152`（RR-20261004-11）、`890abdda`（RR-20261004-14）、`5d1cb974` / `1cf96347`（索引与 WANTED W-2026-10-04-08）；`3127d37c`（状态机方案）、`42059469`（静态绑定方案）；`f051e24a` 删除整套租约代码。

**改动文件**：`demo/internal/service/game/playerowner.go.tmpl`（三笔修改的那一版已被 `f051e24a` 整体重写）、`demo/game/playerroute/`（整包已删除）。

**回归去向**（App 锁方案 §7.3 与 §13 第 3 笔）：RR-20261004-14 的两条承诺转写为 app 包 `TestSingletonWindowStartsWhenTheRenewalWasAsked`、`TestSingletonClaimsItsOwnValueAfterALostAcquireReply` / `TestSingletonKeepsWaitingWhenALostReplyHidesAnotherHolder`；RR-20261004-11 的两个子测试转写为驻留表的 `TestAnUnloadWhoseDropOutlivesItsWaitAdmitsNothingUntilTheDropEnds`（`demo/internal/service/game/playerowner_test.go.tmpl:512`）等；RR-20261004-10 的“回合预算”随“续期在 App 的 goroutine 上、与闲置卸载分开”失去前提，删除。

**测试**：修前红（各问题记录原文节选）：

- RR-20261004-10：

  ```
  playerowner_test.go:1374: one refresh pass re-claiming lost leases held the refresh loop for 45s (9 evictions at evictBudget 5s each), not below Lease(30s) - RefreshInterval(10s) = 20s
  --- FAIL: TestARefreshPassRetakingLostLeasesHoldsTheLoopLessThanTheLeaseAllows (0.00s)
  playerowner_test.go:1447: re-claiming lost leases held the refresh loop for 809.791041ms with a budget of 150ms (without one it would be 800ms)
  --- FAIL: TestARefreshPassStopsWaitingForRetakesWhenItsBudgetRunsOut (0.81s)
  ```

- RR-20261004-11：

  ```
  --- FAIL: TestAHandBackWhoseDropOutlivesItsWaitAdmitsNothingUntilTheDropEnds/background-work (0.05s)
      playerowner_test.go:1385: Admit let work start on player 77 while the hand-back's drop is still destroying the copy in the background
  ```

- RR-20261004-14：

  ```
  playerowner_test.go:1652: a login across a gap: the local window ends 5s after the key Redis set can expire (asked at +0s, Lease 30s, local window until +35s)
  playerowner_test.go:1781: the next renewal put player 42 back in service on the copy from before the gap, which was never dropped: dropped=[]
  ```

  三条记录都写明修前红 `-race -count=20` 20/20。

**review 检查点**：这三笔的代码已不存在，review 只需确认 v1.20.0 之后的模板里 `git grep -n 'playerroute\|game_route\|RefreshInterval\|handBackPassBudget' demo codegen` 只剩历史注释或“不能再有”的断言。

<a id="own-2"></a>
### OWN-2 玩家静态绑定

**提交与首发**：v1.20.0。`f051e24a`（第 3 笔：驻留表、闲置卸载、登录、activity 改 `Live`、删 `playerroute`）、`021454d5`（钉住闲置卸载的锁内复核与“删记录先于关闭 done”，只加测试）、`c441fbdd`（认证写入与登录读取共用 `server_id` claim 键）、`68d0edb2` / `10e2e0ea`（方案状态与使用说明）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `demo/internal/service/game/playerowner.go.tmpl:95` `ErrNotServedHere`、`:100` `IdleUnload = 5m`、`:111` `evictBudget = 5s` | 拒绝错误、闲置阈值、调用方等一次卸载的上限 |
| `:175` `Serve` | 登录：sid 判定 → 等卸载（`evictWait` / ctx）→ `touchLocked` |
| `:216` `AdmitBound` | 后台准入：非本服 `local=false`；卸载中 `(true, err)`；否则建记录 |
| `:246` `Admit`、`:270` `AdmitMessage` | 写准入只看记录与卸载状态；登录消息豁免 |
| `:282` `Resident` | 只读，不算使用（matchmaker） |
| `:301` `CloseServedSessions` | 停机时断开有驻留记录的玩家（锁外关闭） |
| `:360` `unloadIdle`、`:385` `unloadOne`、`:422` `runEviction` | 闲置卸载：锁外问连接数、锁内复核闲置后标记；单航班；删记录先于 `close(done)` |
| `demo/internal/access/player/tcp/auth.go.tmpl:101` | 认证器写 `Claims[svcaccount.ServerIDClaim]` |
| `kit/service/account/types.go:294` `ServerIDClaim = "server_id"` | 键常量（传输包与控制器包共用） |
| `demo/game/controllers/player/enter_game.go.tmpl:175` `BoundServerID`、`:48`、`:70` | 读 claim；缺失时 `player_elsewhere`（`owner_sid=0`）；`Serve` 返回 `ErrPlayerElsewhere` 时透传 |
| `demo/internal/service/game/service.go.tmpl:235-237` | `Shutdown` 第一步 `CloseServedSessions` |

**不变量**：

| 不变量 | 强制位置 | 守卫 |
| --- | --- | --- |
| 驻留记录只在绑定校验通过后建立 | `Serve` / `AdmitBound` 的 sid 判定先于 `touchLocked` | `TestServeAnswersEachOfItsThreeOutcomes`（`playerowner_test.go.tmpl:187`）、`TestAdmitBoundServesOnlyItsOwnSid`（`:233`） |
| WriteGate 覆盖除登录外的全部消息 | `AdmitMessage` | `TestTheWriteGateExemptsLoginAndRefusesEverythingElse`（`:252`） |
| 卸载中不准入；登录等卸载结束 | `evictions` 检查 | `TestAnUnloadInFlightAdmitsNothing`（`:478`）、`TestAnUnloadWhoseDropOutlivesItsWaitAdmitsNothingUntilTheDropEnds`（`:512`）、`TestALoginDuringAnUnloadEndsServed`（`:578`） |
| 有连接或正在用的玩家不被卸载 | `unloadOne` 锁内复核 `lastUsed` | `TestAPlayerWithASessionIsNeverUnloaded`（`:318`）、`TestAUseBetweenTheSessionCheckAndTheMarkCancelsTheUnload`（`:410`） |
| fail-stop 时断开全部服务中的连接 | `Shutdown` 第一步 | `TestCloseServedSessionsClosesEveryServedPlayer`（`:671`）、`TestShutdownClosesTheConnectionsOfEveryServedPlayer`（`service_shutdown_test.go.tmpl:53`） |
| 认证写入与登录读取同一个键 | `account.ServerIDClaim` | 生成工程 `internal/access/player/tcp/auth_test.go` |

**失败与不确定结果**：`Serve` 等卸载超时返回包着 `context.DeadlineExceeded` 的错误，控制器沿用 `loginCutShort` 回 `login_timeout`（不新增错误码）；卸载在自己的 goroutine 上跑到结束，等待方只受 `evictBudget` 约束。

**测试**：第 3 笔修前红（骨架上跑新用例，方案 §13 第 3 笔原文摘录）：`Serve(bound=2000) = <nil>, want player_elsewhere`、`a served player is not resident`、`background work for a player bound elsewhere left a resident record here`、`a write was admitted for a player this process does not serve: <nil>`、`closed 0 connections, want 2`、`Shutdown left served players connected: closed [], want 42 and 43`、`a login with no server_id reached the ownership table with [0]`、`the ownership table was asked about [0], want the session's bound sid 2000`。`021454d5` 变异红：`a player used between the session check and the mark was unloaded: dropped=[77]`、`the unload deleted the record the login created after it woke`。`c441fbdd` 变异红：`read (0, false) … want the role's server 1300`、`code=1 reason="server error", want login_timeout`。修后生成工程 `go test -race -count=3 ./internal/service/game/ ./game/controllers/player/ ./internal/access/...`，新用例 `-race -count=30`（obs34 为 `-count=50`）稳定。

**未验证与风险**（方案 §13 观察，未改）：冷加载超过 `IdleUnload` 后实体留在内存、没有驻留记录；`Destroy` 永不返回时卸载 goroutine 与 `evictions` 条目泄漏；优雅停机先断会话、listener 仍开。

**review 检查点**：

1. `Admit` 在 Nest 快池上调用（`playerowner.go.tmpl:40` 注释）：确认只有一次加锁、没有等待（roost-coding“快池内不得阻塞等待”）。
2. `Serve` 在登录连接 goroutine 上等卸载，不在 Nest worker 上：顺着 `enter_game.go.tmpl` 的调用链确认。
3. `AdmitBound` 只在 `boundSID == owners.sid` 时进入临界区建记录：确认 `NewPlayerOwners` 要求 sid > 0，0 永远不是本服。
4. 删除驻留记录发生在 `close(done)` 之前（`runEviction`）：确认醒来的 `Serve` 不会看到旧记录。

<a id="own-3"></a>
### OWN-3 赠礼按发送方绑定的 sid 准入与转交

**提交与首发**：v1.20.0。`5bdac773`（第 4 笔）、`c493a791`（转交接收方核对 phase 与 topic）、`ac5acfbe`（退款预算覆盖一次崩溃重启）、`64acd782`（方案标注）。v1.20.1 `054fdd66`（U-0280，步骤预算改由配置提供，`demoGiftRefundBudget` 随之改写配置）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `demo/game/gift/gift.go.tmpl:92-96` `State.FromSID`、`:118` | `Encode` 拒绝 `FromSID <= 0`；`Decode` 不要求 |
| `demo/game/handler/start_gift.go.tmpl` | `handlerStartGift` 多参数 `fromSID int32`（`//roost:nest` 生成 sender） |
| `demo/internal/service/game/gift_saga.go.tmpl:225-228` `playerOwnership.AdmitBound`、`:237-239` `giftStepHandoff.FromSID`、`:299` 路由 `KeyOf = cmd.FromSID` | 准入接口、转交载荷、按 sid 路由 |
| `:316` `runHandoff` | 接收方：解码载荷 → 核对信封 `PlayerID` / `FromSID` 与载荷一致（`:327-328`）→ 核对 topic 与 phase → `AdmitBound`（`:349`） |
| `:448` `admitPhase` | 发送侧：`FromSID <= 0` 拒绝并记 Error（`:464`）→ `AdmitBound`（`:473`）→ 非本服则 `handoff.Route` 后 nak（`:484-505`） |
| `codegen/internal/roost/demo.go:192-240` `demoGiftRefundBudget` | 把 `saga.steps.gift_item.debit.max_attempts: 15` 写进 game 服务三份配置 |

**不变量**：debit / refund 只在发送方绑定的 sid 上执行；接收方先核对再认领；`FromSID == 0` 不兜底。守卫：`demo/internal/service/game/gift_handoff_test.go.tmpl` 的 `TestAnOfflineSendersStepRunsOnTheSidTheyAreBoundTo`（`:124`）、`TestAdmitRefusesAndHandsOverAForeignStep`（`:147`）、`TestAHandoffIsAddressedToTheSendersBoundSid`（`:163`）、`TestAStepDoesNotClaimASenderBoundElsewhere`（`:177`）、`TestAStepThatNamesNoSenderSidIsRefused`（`:191`）、`TestAStepForASenderBeingUnloadedIsRefusedWithoutAHandoff`（`:217`）、`TestAHandoffForAnOfflineSenderBoundHereIsRun`（`:235`）、`TestAHandoffForAPlayerWeDoNotOwnIsDropped`（`:249`）、`TestAHandoffWhoseEnvelopeDisagreesWithItsPayloadIsRefused`（`:270`）、`TestAHandoffWhosePhaseDisagreesWithItsCommandIsRefused`（`:289`）、`TestAHandoffWithAnUnknownPhaseIsRefusedBeforeTheClaim`（`:312`）；`send_gift_test.go` `TestSendGiftWritesThisProcessSidAsTheSendersSid`；生成工程 `gift_saga_budget_test.go`。

**测试**：第 4 笔修前红（方案 §13 第 4 笔原文）：

```
TestAnOfflineSendersStepRunsOnTheSidTheyAreBoundTo/debit: the debit of an offline sender bound to this sid was refused: gift saga: player 42 is not served by this process; leaving the step: player owners: this process is not serving the player right now: player 42 has not been taken into service here
TestAdmitRefusesAndHandsOverAForeignStep: handed over to [], want the sender's sid 1000
TestAStepThatNamesNoSenderSidIsRefused: gift.Encode wrote a gift with no sender sid
TestSendGiftWritesThisProcessSidAsTheSendersSid: StartGift got sender 4242 on sid 0, want 4242 on this process's sid 1300
```

（记录另列 `TestAHandoffForAPlayerWeDoNotOwnIsDropped`、`TestAHandoffWhoseEnvelopeDisagreesWithItsPayloadIsRefused` 等红文本；`TestAStepDoesNotClaimASenderBoundElsewhere`、`TestAStepForASenderBeingUnloadedIsRefusedWithoutAHandoff` 在骨架上即绿，作守护。）`ac5acfbe` 修前红：`a refund gets at least 25.75s of retries … = 1m30s; past that the refund is marked manual_required`。`c493a791` 记录未保存修前红文本。真实进程演练 6a / 6b：两个 sid 各 10 个机器人 120 个 saga 全部终结；kill -9 时 7 个在途 saga 全部 compensated、没有 `manual_required`。

**源码与记录不一致**：v1.20.0 CHANGELOG 与 App 锁方案 §13 obs34 写“生成的 `saga/gift_item/definition.go` 里 debit `MaxAttempts` 5 → 15”；v1.20.1 起预算由配置提供，当前源码 `demoGiftRefundBudget`（`codegen/internal/roost/demo.go:209`）改写的是配置里的空 `steps: {}` 块。以源码为准。另：v1.23.0 起 `saga.steps` 的类型 / 步骤名只差大小写时报歧义错误（RR-20261006-06，SAGA 主题），生成的 `gift_item` 为小写，不受影响。

**review 检查点**：

1. `runHandoff` 核对顺序：信封 / 载荷一致 → topic 与 phase 匹配 → `AdmitBound` → 认领；确认未知 phase 的拒绝发生在建驻留记录之前。
2. 重试窗口下界计算（15 × 5s + 14 次退避抖动下界 ≈ 98.15s）与 `singleton.startup_wait + ttl + 45s = 90s` 的关系由生成工程 `gift_saga_budget_test.go` 读配置钉住：确认它读的是 `saga.steps.gift_item.debit`，而不是 `definition.go`。
3. 转交失败只记 Warn、靠共享 durable 重投（`:505`）：确认 nak 不会让同一步骤在两个 sid 上同时执行（只有绑定 sid 的 `AdmitBound` 返回 local）。

<a id="own-4"></a>
### OWN-4 activity 改用 App 的 `Live`；候选校验

**提交与首发**：v1.20.0 `f051e24a`（activity 部分）；v1.20.1 `46c4dfba`（RR-20261005-01）。v1.20.2 起候选来源换成组文件（OWN-5）。

**改动文件与关键符号**：`demo/internal/service/game/activity.go.tmpl:65-67`（`liveness app.SingletonLiveness`）、`:95` `startActivity`（`:98-100` 取 `ModSingleton`）、`:329` `expectedGameSIDs`（`Live` 结果为空只等自己；报错本拍不开窗）。已删除：`incarnation`、`lease` / `leaseStanding`、`bindAndLease` 里的 `AcquireLease`、`renewLease`、`leaseNotOurs`、`activity_lease_test.go.tmpl`。

**不变量**：expected 集合 = `Live` 返回的活 sid（用本进程的 `server_type` 查）。守卫：`TestTheExpectedServersAreTheOnesTheAppLockSeesAlive`（`activity_test.go.tmpl:144`）、`TestActivityRefusesToStartWithoutTheAppLockLiveness`（`:187`）；RR-20261005-01 的 `TestActivityRefusesACandidateListNoWindowCouldOpenWith` 在 C4 后演变为 `TestActivityRefusesAGroupNoWindowCouldOpenWith`（`:235`）。

**测试**：第 3 笔修前红：`expected [1300], want exactly the live sids [1300 1302]`、`a Live query that failed produced an expected set`、`startActivity error "activity: game: capability \"service.global.activity\" not found; ..." does not name the missing capability`。RR-20261005-01 修前红（[修复记录](../../bugfix/RR-20261005-01.md)原文）：

```text
$ GOWORK=off go test -race -count=1 -run TestActivityRefusesACandidateListNoWindowCouldOpenWith -v ./internal/service/game/
    activity_test.go:236: startActivity with activity.game_sids=[1302 1301 1302]: error activity: game: capability "service.global.activity" not found; is the activity process running and reachable over the bus? does not refuse the list by name; every window it would try to open would fail
    activity_test.go:236: startActivity with activity.game_sids=[2000 2001 2002 2003]: error activity: game: capability "service.global.activity" not found; ... does not refuse the list by name; every window it would try to open would fail
    activity_test.go:236: startActivity with activity.game_sids=[4294968598]: error activity: game: capability "service.global.activity" not found; ... does not refuse the list by name; every window it would try to open would fail
    --- FAIL: .../a-repeated-sid (0.00s)
    --- FAIL: .../more-candidates-than-one-live-query (0.00s)
    --- FAIL: .../a-sid-beyond-int32 (0.00s)
    --- PASS: .../own-sid-and-non-positive-entries-are-skipped (0.00s)
FAIL
```

**review 检查点**：

1. `expectedGameSIDs` 用 `registry.Config().GetString("server_type")`（`run` 写入）而不是写死 `"game"`。
2. `Live` 报错时本拍不开窗（不是退化为只等自己）：对照 APP-5 检查点 1。

<a id="own-5"></a>
### OWN-5 活动组文件（C4）

**提交与首发**：v1.20.2 `277e1252`（`cb3e2549` 标注）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `kit/service/global/activity/groups.go:58` `LoadGroupsFile`、`:80` `ParseGroups`、`:110-112` 64 上限、`:137` `Of`、`:146` `IDs` | 唯一的解析与校验 |
| `kit/service/global/activity/types.go:223` `MaxExpectedGames = 64` | 组上限 = 协调器单窗口 expected 上限，同一个常量 |
| `kit/service/global/activity/activity_mod.go:117-125` | 读 `activity.groups_file`；`sweep_groups` 为空时取文件里的组 |
| `demo/internal/service/game/activity.go.tmpl:115`、`:171-184` `activityGroup` | game 按本进程 sid 找组；未配文件、不合格都启动报错 |
| `codegen/internal/roost/activity_groups.go:22` | 生成 `configs/activity_groups.yaml`（只创建一次） |

**不变量**：组文件的规则只有一份；同一文件被协调器与 game 读。守卫：`kit/service/global/activity/groups_promises_test.go` 的 `TestAGroupLargerThanOneWindowIsRefusedWhenLoaded`（`:44`）、`TestAFullGroupOpensAWindowWithTheCoordinator`（`:56`）、`TestAGroupsFileThatCannotBeUsedIsRefusedByName`（`:79`）、`TestGroupsAnswerWhichGroupASIDIsIn`（`:111`）、`TestModSweepsTheGroupsInTheGroupsFile`（`:142`）；生成工程 `TestActivityRefusesAGroupNoWindowCouldOpenWith`、`TestAWindowOpensForTheGroupTheFilePutsThisServerIn`（`activity_test.go.tmpl:235` / `:285`）；codegen `activity_groups_promises_test.go`。

**测试**：修前红（[C4 方案 §6](../../feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md)原文）：

```
--- FAIL: TestActivityRefusesACandidateListNoWindowCouldOpenWith/more-candidates-than-the-coordinator-takes (0.00s)
    activity_test.go:238: startActivity with activity.game_sids=[2000 2001 2002 2003]: error activity: game: capability "service.global.activity" not found; is the activity process running and reachable over the bus? does not refuse the list by name; every window it would try to open would fail
```

修后 kit `go test -race -count=3` 通过；生成工程 build / vet / test 全绿；`second-game.sh` 的 `SECOND_SID=1005` 启动前退出并点名文件。

**未验证与风险**：协调器 `OpenActivity` 不核对 expected 集合属于 Key 的组；没有真实依赖进程演练。

**review 检查点**：

1. `ParseGroups` 拒绝未知字段（`game_sid` 拼错不会被当成空）：看 YAML 解码是否开了严格模式。
2. 组 id 规则与 `Key.Validate` 同（非空、不含 `/`）：两处是否引用同一个检查。
3. 协调器显式 `sweep_groups` 仍优先：多副本分担时文件里的组与 `sweep_groups` 不一致不会报错，确认这是有意的。

<a id="own-6"></a>
### OWN-6 global `Bind` 重试幂等

**提交与首发**：v1.23.0 `611d5d72`（收尾第 4 批 A7）；`ba13cb05` 在真实 Redis 用例里补同参数重试断言。

**改动文件与关键符号**：`kit/service/global/service.go:55-95` `Bind`：`Create` 返回 `!created` 时 `Get`；`found` 且 group 与 globalSID 一致 → `Replayed("bind")` 返回已存绑定；否则 `Conflict("bind")` 并返回带已存 group / sid 的 `ErrConflict`；`Get` 出错原样返回。

**不变量**：只有“已存的绑定指向别处”才是冲突。守卫：`TestBindRetriedAfterUnknownOutcomeReturnsTheSameBinding`（`kit/service/global/bind_retry_promises_test.go:33`，同时钉住换 group、换 globalSID 两种仍报冲突、`conflict:bind` 计 2、库里绑定不变）；`kit/service/integration` `TestGlobalRunsOnRedis`（真实 Redis）。

**测试**：修前红（[问题记录](../../bug/RR-20261006-05.md)原文）：

```
--- FAIL: TestBindRetriedAfterUnknownOutcomeReturnsTheSameBinding (0.00s)
    bind_retry_promises_test.go:47: retrying the same bind after an unknown outcome: global: conflict: game 7 is already bound, want the stored binding
```

真实 Redis 负对照（发版前补充验证 §3，把回读比较临时改成 `if false && ...`）：

```
redis_test.go:520: retried Bind against real Redis = {GameSID:0 ...}, global: conflict: game 7 is already bound to group-a/100; want the same binding {GameSID:7 GlobalGroupID:group-a GlobalSID:100 ... Epoch:1 ...}
```

修后 `go test -race -count=3 ./kit/service/global` 与真实 Redis `TestGlobalRunsOnRedis` 通过。

**review 检查点**：

1. `!created && !found`（`Create` 没建成、随即又被删）返回 `ErrConflict "already bound"`（`service.go:87-89`）：确认这条“读不到已存值”的路径报冲突而不是结果未知是可接受的。
2. 迁移中（`RouteMigrating`）且 globalSID 仍是请求值时返回成功：确认调用方（game-demo activity 的 `routing.Bind`）不依赖返回的 `State` 为 Active。

---

<a id="clk"></a>
## CLK

<a id="clk-1"></a>
### CLK-1 业务时钟与系统时钟分开（D-L3）

**提交与首发**：v1.21.0 `b9fc5342`（`7cae9d78` 记录维护者确认的修订版、`f4fbdef1` 标注）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `clock/clock.go:26` `Business`、`:31` `BusinessFunc`、`:40` `NewBusiness`、`:57` `Process` | 业务时钟接口；固定偏移实现（偏移 0 时直接返回 `time.Now()`）；进程级业务时钟 |
| `clock/clock.go:81` `Now`、`:96` `SetOffset` | 进程级偏移；`SetOffset` 只由 App 启动时调一次 |
| `app/app.go:141` | `clock.SetOffset(read.Duration(logicOffsetKey))`，`fctx.Now` 读它 |
| `app/registry.go:43` | `NewRegistry` 登记 `ModBusinessClock` = `clock.NewBusiness(offset)`；与 `:141` 读同一个键 |
| `app/business_clock.go:13` `ModBusinessClock`、`:17` `logicOffsetKey`、`:25` `BusinessClock`、`:44` `validateProductionLogicOffset` | 能力名、唯一配置键、取钟、生产非 0 拒绝（`app/config_validation.go:101` 调用） |
| `kit/service/global/activity/activity_mod.go:152`、`kit/service/mail/mail_mod.go:91`、`kit/service/rank/rank_mod.go:79`、`kit/service/session/session_mod.go:103` | Mod 给服务的 `Config.Now` 注入 `app.BusinessClock(r).Now` |
| `timer/scheduler.go:131`、`ai/controller.go:242`、`actionflow/mission_runner.go:436`、`actionflow/action_runner.go:698` | 未注入时缺省 `clock.Now()`（原 `time.Now()`） |
| `cmd/glsvet/clockhints.go:21-32`、`cmd/glsvet/main.go:15-18` | `-clockhints`（缺省开）、`-businessdirs`（缺省 `game`）、豁免指令 `glsvet:system-clock`；业务目录按模块根（向上找 `go.mod`）之下的路径判断 |
| `demo/internal/service/game/activity.go.tmpl:149`、`gm.go.tmpl:152`、`spawner.go.tmpl:63`、`purchase_drain.go.tmpl:70`、`demo/game/controllers/player/controller.go.tmpl:478-480` | game-demo 业务时间读业务钟（`tickWorld` 拆出 `tickWorldOnce`，玩家控制器加 `BusinessNow()`） |

**不变量**：偏移只有一个配置来源、只在启动时生效；Nest / DataEngine / Sync 一行不改（全是系统时钟）。守卫：`TestProductionRefusesANonZeroLogicOffset`（`app/logic_offset_production_promises_test.go:12`）、`TestBusinessClockFollowsTheConfiguredOffset` / `TestOffsetMovesBusinessTimeButNotTheSingletonLease`（`app/business_clock_promises_test.go:22` / `:42`）、`TestTheModWiresTheCoordinatorToTheBusinessClock`（`kit/service/global/activity/business_clock_promises_test.go:16`）、`TestTheKeyTTLComesFromTheInjectedClock`（`service/mail/redis_store_test.go:361`）、`TestClockHints*`（`cmd/glsvet/clockhints_test.go:31-133`）；生成工程 `TestTheWindowFollowsTheBusinessClock` / `TestTheWorldTickCarriesBusinessTime`（`activity_test.go.tmpl:386` / `:411`）。

**测试**：修前红（[D-L3 方案 §7](../../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md)原文）：

```text
$ GOWORK=off go test -count=1 -run TestProductionRefusesANonZeroLogicOffset ./app
--- FAIL: TestProductionRefusesANonZeroLogicOffset (0.00s)
    logic_offset_production_promises_test.go:18: production with time.logic_offset=24h: ValidateServiceConfig error = <nil>, want a refusal naming time.logic_offset
$ GOWORK=off go test -count=1 -run TestTheModWiresTheCoordinatorToTheBusinessClock ./kit/service/global/activity
--- FAIL: TestTheModWiresTheCoordinatorToTheBusinessClock (0.00s)
    business_clock_promises_test.go:40: the coordinator's clock reads 2026-10-06T08:48:45+08:00, 0s from the wall clock; want the business clock, 24h ahead
# 生成的 game-demo（replace 到 worktree）
$ GOWORK=off go test -count=1 -run TestTheWindowFollowsTheBusinessClock ./internal/service/game/
--- FAIL: TestTheWindowFollowsTheBusinessClock (0.00s)
    activity_test.go:400: opened [west/race-1791247800/close], want the window the business clock is in (west/race-1791334200/close), not the wall clock's (west/race-1791247800/close)
```

v1.21.0 的 `TestMailExpiryIsBusinessTimeAndTheClaimLeaseIsSystemTime` 在 v1.22.0 改写为 `TestMailExpiryAndTheClaimLeaseRunOnTheMonotonicBusinessClock`（CLK-4）。修后 `go test -race -count=3` 覆盖 app、clock、service/mail、kit/service/mail、kit/service/global/activity、kit/service/rank、kit/service/session、service/session、timer、ai、actionflow、cmd/glsvet、fctx；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 无违例、无提示；生成 game-demo `glsvet ./...` 无违例、无提示。

**性能**：热路径（nest / dataengine / sync / entity）不改；`fctx` 仍是一次原子读 + Add；新增的接口调用只在 kit 服务的单次请求里（每次都有 Redis 往返），方案判断可以忽略，没有基准数据。

**review 检查点**：

1. `app/app.go:141` 与 `app/registry.go:43` 两处读同一个键、各自建钟：确认测试里 `registry.Register(app.ModBusinessClock, …)` 替换不会让 `fctx.Now` 与 Registry 的钟分叉（方案 §2 写明测试要在 cfg 里写偏移）。
2. `git grep -n 'time.Now()' -- 'kit/service/**/*.go' 'demo/**/*.tmpl'` 的非测试结果逐个归到系统钟用途（租约、TTL、审计）；`game` 目录外的业务包 glsvet 不会提示，靠 review。
3. 生产校验 `isProductionServiceConfig` 认 `env` / `app.env` / `environment` 三个键的 prod / production：确认生成的生产示例写的是其中之一。

<a id="clk-2"></a>
### CLK-2 match / chat 展示 / account 换钟；doctor 偏移一致（D-L3 第八轮）

**提交与首发**：v1.21.0 `fa472ee7`（`d73289b5` 记录决定、`5505db4b` 标注）。

**改动文件与关键符号**：`kit/service/match/match_mod.go:114`（注入业务钟）；`demo/internal/service/game/matchmaker.go.tmpl:82`（`matchmaking.Pools` 读业务钟，去掉 `//glsvet:system-clock`）；`kit/service/chat/chat.go:487-502`（`Message.SentAtUnix`，`json:"sent_at_unix,omitempty"`）、`kit/service/chat/chat_mod.go:158`（`Now` 业务钟、`SystemNow: time.Now`）；`kit/service/account/service.go:57-64`（`Config.SystemNow` 说明）、`:149-150`（nil 沿用 `Now`）、`:292`（建会话：业务钟时间 + 系统钟签发）、`:317`（`VerifySessionToken` 用系统钟）、`:404`；`kit/service/account/admin.go:146`（运维时间系统钟）；`kit/service/account/account_mod.go:127`；`codegen/internal/roost/logic_offset_doctor.go:37` `checkLogicOffsets`、`:97` `configLogicOffset`（按 `app.ConfigDuration` 的读法解析）；`codegen/internal/roost/render_docs.go:238`（生成工程说明的时间规则）。

**不变量**：持久格式只增不改（chat 旧消息读出时 `SentAtUnix` 用 `StoredAtUnix` 兜底，存量不改写）；`account` 判定表一格不动（换钟发生在 Mod 注入处）。守卫：`TestOffsetMovesMatchChatAndAccountBusinessTimesButNotRetentionOrSessions`（`kit/service/integration/business_clock_test.go:21`，真实 Redis）、`TestDisplayTimeIsBusinessTimeAndRetentionIsSystemTime` / `TestAMessageStoredBeforeSentAtUnixShowsItsStoredTime`（`kit/service/chat/business_clock_promises_test.go:13` / `:46`）、`TestAccountTimesAreBusinessTimeAndSessionsAreSystemTime`（`kit/service/account/business_clock_promises_test.go:13`）、`TestDoctorNamesTheServicesWhoseLogicOffsetDisagrees`（`codegen/internal/roost/logic_offset_doctor_promises_test.go:50`）。

**测试**：修前红（[D-L3 方案 §8.2](../../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md#8-第八轮留项实施2026-10-06)原文；chat 红跑时临时用 `StoredAtUnix` 代替 `SentAtUnix` 断言）：

```text
$ REDIS_ADDR=<隔离 Redis> GOWORK=off go test -tags integration -count=1 -run TestOffsetMovesMatchChatAndAccountBusinessTimesButNotRetentionOrSessions ./kit/service/integration/
--- FAIL: TestOffsetMovesMatchChatAndAccountBusinessTimesButNotRetentionOrSessions (0.01s)
    business_clock_test.go:61: ticket.CreatedAtUnix = 2026-10-06T09:38:12+08:00, 0s from the wall clock; want 24h0m0s
    business_clock_test.go:62: ticket.ExpiresAtUnix = 2026-10-06T09:43:12+08:00, 5m0s from the wall clock; want 24h5m0s
    business_clock_test.go:74: message.StoredAtUnix (shown to players, pre-fix) = 2026-10-06T09:38:12+08:00, 0s from the wall clock; want 24h0m0s
    business_clock_test.go:97: account.CreatedAtUnix = 2026-10-06T09:38:12+08:00, 0s from the wall clock; want 24h0m0s
    business_clock_test.go:98: account.LastLoginAtUnix = 2026-10-06T09:38:12+08:00, 0s from the wall clock; want 24h0m0s
    business_clock_test.go:106: role.CreatedAtUnix = 2026-10-06T09:38:12+08:00, 0s from the wall clock; want 24h0m0s
$ GOWORK=off go test -count=1 -run TestDoctorNamesTheServicesWhoseLogicOffsetDisagrees ./codegen/internal/roost/
--- FAIL: TestDoctorNamesTheServicesWhoseLogicOffsetDisagrees (8.66s)
    logic_offset_doctor_promises_test.go:59: only the dev game config sets time.logic_offset 24h: doctor says {Name: Status: Detail:} (present=false), want FAIL
```

负对照：把 `Prune` 截止临时改回业务钟，用例报 `Prune with a 1h retention dropped 1 message(s) stored a moment ago`。修后 `go test -race -count=3 ./service/match/ ./kit/service/match/ ./kit/service/chat/ ./kit/service/account/` 通过；生成 game-demo `glsvet ./...` 退出 0、无提示（豁免剩 5 处：支付时间、saga 截止 2 处、玩家归属 2 处）。

**review 检查点**：

1. `account` 的名字预约 TTL 由名字目录自己的钟管（Mod 不注入，`time.Now`）：确认它与 `Config.Now` 的业务时间没有被拿来比较。
2. doctor 只在三套配置内部各自比较、不跨套比较：确认 prod example 偏移 0 与 dev 偏移 24h 并存时为 OK。
3. glsvet 生成工程里剩下的 5 处 `//glsvet:system-clock` 豁免（当前源码：`purchase.go.tmpl:63` 支付时间、`start_gift.go.tmpl:48` 与 `gift_saga.go.tmpl:360` saga 截止、`playerowner.go.tmpl:119` / `:125` 驻留与闲置卸载）理由是否都属于系统用途。D-L3 §3.2 的表把后两处写成“玩家归属租约”，静态绑定后那里已没有按玩家的租约，豁免注释写的是 “residency and idle unload are leases”——用途是闲置判定，读系统钟合理，只是文档措辞过时。

<a id="clk-3"></a>
### CLK-3 业务时间只许前进

**提交与首发**：v1.22.0 `3e77beb9`（`a26c9454` 标注；`2a4c835d` 规划）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `app/business_time.go:26` `ErrBusinessTimeMovedBack`、`:30` `ErrBusinessTimeGuardMissing` | 两个哨兵 |
| `app/business_time.go:32-51` | 容差 1 分钟、推进间隔 10s、单次调用 3s、CAS 8 次、键后缀 `:business_time`、指标名 |
| `app/business_time.go:97` `startBusinessTimeGuard` | 谁检查：生产跳过；有锁用锁的 store；偏移 0 且无锁跳过；偏移非 0 无锁则自己开 store（没有 opener / key_prefix → `ErrBusinessTimeGuardMissing`） |
| `app/business_time.go:146` `check` | 启动检查：`now < H − 1m` 拒绝；读写失败拒绝 |
| `app/business_time.go:173` `advance` | 只用 `CompareAndSet`（`ttl = 0` 不过期）；`CAS(nil, B)` 首写；读到 `H ≥ B` 不写；被抢先用对方的值重来，最多 8 次 |
| `app/business_time.go:208` `advanceLoop`、`:228` `stop` | 每 10s 推进；`stop` 在锁的 `finish` 之前执行，只关自己开的 store |
| `app/app.go:247-254` | 挂点：单实例锁之后、`sortMods` 与任何 Mod Init 之前；失败时 `singletonReleasable = true` |
| `app/singleton.go:55-56` | `SingletonOpener` 注释：`singleton.enabled=false` 而偏移非 0 时 App 也用它只为高水位开一个连接 |

**不变量**：同一套部署的业务时间单调不减（容差 1 分钟内）；守卫读不到就不能证明没回退（fail-closed）；生产行为一字不变。守卫：`TestBusinessTimeMovingBackRefusesToStart`、`TestBusinessTimeMayMoveForwardOrStay`、`TestTheHighWaterMarkAdvancesWhileRunning`、`TestAnUnreadableHighWaterMarkRefusesToStart`、`TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime`、`TestProductionDoesNotRunTheBusinessTimeGuard`（`app/business_time_promises_test.go:48` / `:85` / `:126` / `:157` / `:178` / `:235`）；真实 Redis `TestBusinessTimeHighWaterMarkOnRealRedis`（`kit/redis/business_time_integration_test.go:77`）。

**控制流**：读配置 → 单实例锁获取 → `startBusinessTimeGuard`：`check`（`advance(now, inspect)`：`CAS(last=nil, B)`；不生效则解析当前值 H，交给 `inspect` 判回退；`H ≥ B` 则记 `last = H` 返回；否则 `CAS(H, B)` 重来）→ 起 `advanceLoop` → Mod 启动 …… 停机 defer 链：`businessTime.stop()`（登记晚于锁的 `finish`，所以先执行）→ `singleton.finish`。

**失败与不确定结果**：CAS 报错直接返回（启动时即拒绝启动，运行中只 Warn + 计数，下一拍重试）；8 次都被抢先返回 `high-water mark still changing after 8 attempts`。

**测试**：修前 = 去掉 `App.run` 里对 `startBusinessTimeGuard` 的调用（[方案 §8](../../feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md)原文）：

```text
$ GOWORK=off go test -count=1 -run 'TestBusinessTimeMovingBackRefusesToStart|TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime|TestAnUnreadableHighWaterMarkRefusesToStart' ./app/
--- FAIL: TestBusinessTimeMovingBackRefusesToStart (0.01s)
    business_time_promises_test.go:59: moving time.logic_offset from 24h back to 0 started the service; business time must only move forward
--- FAIL: TestAnUnreadableHighWaterMarkRefusesToStart (0.01s)
    business_time_promises_test.go:165: started without being able to read the business time high-water mark
--- FAIL: TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime (10.06s)
    --- FAIL: TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime/a_non-zero_offset_checks_and_keeps_the_mark (5.02s)
        business_time_promises_test.go:184: run did not return
    --- FAIL: TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime/forward_offset_serves_and_closes_the_store_at_stop (0.01s)
        business_time_promises_test.go:199: high-water mark = 0001-01-01 00:00:00 +0000 UTC, want real time + 1h
    --- FAIL: TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime/no_store_to_keep_the_mark (5.01s)
        business_time_promises_test.go:213: run did not return
```

（`TestTheHighWaterMarkAdvancesWhileRunning` 修前：`high-water mark stayed at 0001-01-01 00:00:00 +0000 UTC while the service ran`。）修后 `go test -race -count=3 ./app/ ./clock/ ./service/mail/ ./kit/service/mail/ ./kit/service/global/activity/` 通过；真实 Redis `-tags integration -run TestBusinessTimeHighWaterMarkOnRealRedis ./kit/redis/` 通过。

**未验证与风险**：没开锁、偏移为 0 的进程不检查；只在单机 Redis 上验证（单键，不涉及跨槽）。

**review 检查点**：

1. `check` 失败时 `run` 置 `singletonReleasable = true`（`app/app.go:252`）：此时还没有任何 Mod 启动，释放是安全的；确认 `businessTime` 为 nil 时 `defer businessTime.stop()` 不会被登记（`:254` 在错误返回之后）。
2. `advance` 的 `inspect` 只在第一次读到已有值时调用（`inspect = nil`）：确认被抢先重来时不会因为别的进程刚写的更大值而误判回退（回退只与第一次读到的 H 比）。
3. 共用锁的 store 时 `closeStore` 不关（`ownStore=false`）：确认 store 的唯一关闭方是 `singleton.finish`。
4. `advanceLoop` 用 `time.NewTicker` 的系统时间间隔、写入的值是业务时间：两者混用是有意的（间隔是系统用途），确认 `g.now` 是 `BusinessClock(a.registry).Now`。

<a id="clk-4"></a>
### CLK-4 activity / mail 回到业务钟

**提交与首发**：拆分 v1.21.0（`b9fc5342` mail；`5a3c4a60` activity，`3d3b0c09` 记录提交号）；合并与删除 v1.22.0 `3e77beb9`。

**改动文件与关键符号**（当前源码）：`kit/service/global/activity/service.go:1469`（创建派发 `NextAttemptAtUnix: nowUnix`，业务钟）、`:1641`（退避 `now.Add(s.dispatchBackoff(...))`）、`kit/service/global/activity/admin.go:105` `ReopenDispatch`、`:144`（重开写业务钟当前时间）；`service/mail/service.go:863`（`ClaimDeadlineUnix = nowUnix + ClaimLease`，业务钟）；`service/mail/redis_store.go:59` `EnvelopeStorageGrace = 24h`；`kit/service/mail/mail_mod.go:91`（只注入业务钟）。已删除：`activity.Config.SystemNow`、`mail.Config.SystemNow`、`mail.RedisConfig.StorageGrace`、`kit/service/global/activity/system_clock_promises_test.go`。

**不变量**：只与业务时间比较、没有服务端 TTL 的退避与租约读业务钟；依赖服务端 TTL、跨进程墙钟比较、空间回收、安全有效期、审计的读系统钟（chat `StoredAtUnix` / `Prune`、account token 与运维记录保留系统钟）。守卫：`TestDispatchBackoffAndProofExpiryRunOnTheMonotonicBusinessClock`、`TestAReopenedDispatchIsOwedNow`（`kit/service/global/activity/monotonic_business_time_promises_test.go:18` / `:112`）、`TestMailExpiryAndTheClaimLeaseRunOnTheMonotonicBusinessClock`（`service/mail/business_clock_promises_test.go:15`）。这些用例只注入一个钟，在拆分的代码上同样通过——它们证明的正是“合并前后行为相同”（方案 §8 原话）。

**测试**：

- v1.21.0 `5a3c4a60` 拆分时的修前红（[发版前审查收尾 §1](../../bugfix/PRERELEASE-AUDIT-FOLLOWUP-2026-10-06.md)原文；这个测试文件已在 v1.22.0 删除）：

  ```text
  $ GOWORK=off go test -count=1 -run 'TestDispatchBackoffAndProofExpiryAreSystemTime|TestAReopenedDispatchIsOwedNowOnTheSystemClock' ./kit/service/global/activity/
  --- FAIL: TestDispatchBackoffAndProofExpiryAreSystemTime (0.00s)
      system_clock_promises_test.go:49: proof ExpiresAtUnix = 1700087000, 24h0m0s from system time + ReservationTTL; want the system clock (the ledger TTL is relative system time)
      system_clock_promises_test.go:63: a dispatch created due immediately in the previous run: activity: dispatch is not due for another attempt: activity group-a/race-untouched/close game 1000 is due at 1700086400, now 1700000000; want it due now, not an offset later
  ```

- v1.22.0 合并：没有新的“修前红”——删除拆分是在 CLK-3 让回调场景不可能之后的简化；证据是上面三条用例在合并前后都绿，回调场景由 app 的 `TestBusinessTimeMovingBackRefusesToStart` 取代。
- 修后 `go test -race -count=3 ./kit/service/global/activity/ ./service/mail/ ./kit/service/mail/`；`go test -tags integration -run TestIntegration ./service/mail/` 通过（integration 用例在包内把宽限缩到 1ms）。

**review 检查点**：

1. `git grep -n 'SystemNow' -- kit/service/global/activity service/mail kit/service/mail` 应无结果；chat / account 的 `SystemNow` 保留。
2. 偏移非 0 的测试环境里，升级前按系统钟写下的 `NextAttemptAtUnix` / `ClaimDeadlineUnix` 比业务钟早一个偏移：确认只会提前到期（重试提前），mail 重试拿到的是同一个 claim token（`Entry.ClaimToken`），不会重复发放。
3. Redis owed 索引对 `NextAttemptAtUnix = 0` 用 `CreatedAtUnix` 打分的兜底：v1.22.0 起两者同为业务钟，确认这条兜底只剩历史记录。

<a id="clk-5"></a>
### CLK-5 高水位推进失败计数

**提交与首发**：v1.23.0 `7b73aabc`（第十二轮 kit 批 §8）。

**改动文件与关键符号**：`app/business_time.go:51` `businessTimeAdvanceFailedMetric = "app.business_time.advance_failed.total"`；`:108` 取本 App 的 `*metrics.Registry`（`ModMetrics`）；`:218-222` 推进失败且 ctx 未结束时 `IncCounter` 并 Warn。`metrics.Registry.IncCounter` 对 nil 接收者安全（`metrics/metrics.go:235-237`）。

**守卫**：`TestAFailedHighWaterMarkAdvanceIsCounted`（`app/business_time_promises_test.go:251`：写失败期间计数增长，恢复后高水位继续推进、计数不再增长）。

**测试**：修前红（第十二轮 kit 批 §8 原文）：`business_time_promises_test.go:278: advance failures counted 0 while every write of the high-water mark failed, want them counted`。修后 `go test -race -count=3 ./app` 通过。

**review 检查点**：停机时 `ctx` 被取消导致的推进失败不计数（`ctx.Err() == nil` 条件）：确认停机不会产生一次误报。

<a id="clk-6"></a>
### CLK-6 timer 同期限顺序与 priority、未注册类型（D-L1 / D-L2）

**提交与首发**：v1.21.0 `5abae51e`（`e320578c` 标注）。

**改动文件与关键符号**：`timer/scheduler.go:3-6`（包注释：触发顺序）、`:29-31` `UnhandledDroppedMetric`、`:43-45` `Node.Priority`、`:143` `NewTimer` = priority 0、`:148` `NewTimerWithPriority`、`:165` `ReportUnhandledTypes`、`:305` 删除无 handler 节点时计数、`:401-411` `timerHeap.Less`（End → Priority → ID）；`demo/db/def/world.go.tmpl:46-51`（`TimerNode.Priority`，`bson:"priority"`，旧文档按 0 读回）；`demo/game/entities/world/timer_component.go.tmpl:115`（`OnInitFinish` 调 `ReportUnhandledTypes`，从 DAO 建一次调度器）。

**不变量**：顺序是全序（ID 唯一），与入堆顺序、存储遍历顺序无关；`ChangeTimer` 与按返回值重排都保留 ID（“最初登记的顺序”）。守卫：`TestTimersWithTheSameDeadlineFireInRegistrationOrder`、`TestPriorityOrdersTimersWithTheSameDeadline`、`TestPriorityIsKeptThroughStorageAndRescheduling`、`TestADueTimerWithoutAHandlerIsDroppedWithAWarningAndACount`、`TestStoredTypesWithoutAHandlerAreReportedOncePerType`（`timer/order_and_unhandled_promises_test.go:35` / `:86` / `:105` / `:162` / `:202`）；game-demo `TestDeadlinesDueAtTheSameMomentFireInArmOrder`、`TestAStoredTimerOfATypeWithNoHandlerIsReportedAndCountedWhenDropped`、`TestTimerPriorityIsStoredAndOrdersAfterARestart`。NC-140 / 141 / 147 的回归照样通过。

**测试**：修前红（[D-L1 / D-L2 方案 §4](../../feature/D-L1-L2-TIMER-ORDER-AND-UNHANDLED-2026-10-06.md)原文）：

```
--- FAIL: TestTimersWithTheSameDeadlineFireInRegistrationOrder/armed_in_one_scheduler
    timers due at the same instant fired in order [1 6 5 4 3 2], want registration order [1 2 3 4 5 6]
--- FAIL: TestTimersWithTheSameDeadlineFireInRegistrationOrder/rebuilt_from_stored_nodes_in_any_order
    stored timers due at the same instant fired in order [4 3 5 1 6 2], want [1 2 3 4 5 6]
--- FAIL: TestTimersWithTheSameDeadlineFireInRegistrationOrder/rescheduled_timers_keep_their_place
    fired [2 1], want [1 2]: the postponed-then-advanced timer was registered first
--- FAIL: TestPriorityOrdersTimersWithTheSameDeadline
    fired [6 3 5 4 1 2], want [6 2 3 5 1 4] (deadline, then priority ascending, then registration order)
--- FAIL: TestADueTimerWithoutAHandlerIsDroppedWithAWarningAndACount
    timer.unhandled_dropped_total{kind="7"} = 0, want 2
    got 0 warnings, want one per dropped node:
```

game-demo：

```
--- FAIL: TestDeadlinesDueAtTheSameMomentFireInArmOrder
    deadlines due at the same moment fired in order [race-c race-f race-e race-b race-h race-g race-a race-d], want the order they were armed in [race-a race-b … race-h]
--- FAIL: TestAStoredTimerOfATypeWithNoHandlerIsReportedAndCountedWhenDropped
    loading stored timers of two unhandled types logged 0 warnings, want one per type:
    dropping three unhandled nodes logged 0 warnings, want 3:
    timer.unhandled_dropped_total{kind="99"} = 0, want 2
    timer.unhandled_dropped_total{kind="98"} = 0, want 1
```

修后 `go test -race -count=3 ./timer`；全新生成 game-demo `go test -race -count=3 ./game/entities/world/`。

**性能**：排序多一次整数比较（方案判断“代价一次比较”），没有基准数据。

**review 检查点**：

1. `kind` 标签是类型号（代码常量）：确认没有业务把动态值当类型号，标签基数有界。
2. 宿主事务回滚后重试同一次 Tick 会再计一次 `unhandled_dropped_total`：计数口径是“删除尝试”而不是“删除的节点”，告警阈值按此理解。

---

<a id="ops"></a>
## OPS

<a id="ops-1"></a>
### OPS-1 服务指标默认落到 metrics 注册表（C6）

**提交与首发**：v1.20.2 `491aaf3b`（`cb3e2549` 标注）。

**改动文件与关键符号**：`servicemetrics/metrics_reporter.go:12-23`（六个指标名）、`:39` `NewMetricsReporter`（每次上报解析当时的默认注册表）；`servicemetrics/servicemetrics.go:47` `KeyedReporter`；`kit/mods/service_servicemods.go:144-169` `ServiceMetrics` 与 `ServiceMetricsEnabledKey = "service_metrics.enabled"`；`app/config_validation.go:262`（登记为严格布尔）；`codegen/internal/roost/framework_services.go:402-404`（生成的 `Metrics()`）；`service/match/queue_store.go`、`kit/service/rank/redis_store.go`、`service/session/service.go` 三个调用点（`DepthOf` 与 `Dropped("run.swept", n)`）。

**不变量**：指标名固定六个，服务、操作、原因、对象都在标签里；不实现 `KeyedReporter` 的项目 Reporter 收到旧形状。守卫：`servicemetrics/metrics_reporter_promises_test.go`（真实注册表 → `PrometheusText`）、`TestQueueDepthDropsAndReplaysAreReported`（match）、`TestSubmitAndPageReportWhatTheyDid`（rank）、`TestSweepResolvesExpiredRunsAndFreesTheirClaims`（session）、`TestGeneratedServicesReportIntoTheMetricsRegistryByDefault`（codegen）。

**测试**：修前红（[C6 方案 §6](../../feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md)原文）：

```
--- FAIL: TestQueueDepthDropsAndReplaysAreReported
    match_test.go:779: queue depth reported 0 under the fixed name queue keyed by the queue, want 2; accepted:enqueue=2, depth:queue.ranked:2:eu=2
--- FAIL: TestSubmitAndPageReportWhatTheyDid
    store_test.go:536: board depth reported 0 under the fixed name board keyed by the board, want 1; accepted:submit=1, depth:board.arena=1, replayed:submit=1
--- FAIL: TestSweepResolvesExpiredRunsAndFreesTheirClaims
    session_test.go:508: the sweep counted 0 swept runs, want 3; accepted:attach=3, accepted:enter=3, accepted:release=3, depth:session.swept=3, dropped:run.expired=3
--- FAIL: TestGeneratedServicesReportIntoTheMetricsRegistryByDefault
    service_metrics_promises_test.go:43: internal/service/account/collaborators.go: Metrics() returns nil, want servicemetrics.NewMetricsReporter("account"): a nil reporter leaves the service's events with nowhere to go
```

真实进程（隔离 Redis / NATS）：修前生成工程 `/metrics` 0 行；修后出现 `service_accepted_total{op="submit",service="rank"} 3`、`service_depth{key="arena",name="board",service="rank"} 3`、`service_replayed_total{op="submit",service="rank"} 1`。

**review 检查点**：

1. `key` 标签基数由调用方决定（队列分区、看板 ID）：确认 kit 内没有把玩家 / 公会 ID 拼进 `Partition` 或 `Board.ID` 的调用；超过 2048 条时看 `obs.series.dropped{metric="service.depth"}`。
2. `NewMetricsReporter` 每次上报解析默认注册表：确认 App 启动时设置默认注册表发生在任何服务事件之前，或之前的事件落到旧注册表是可接受的。

<a id="ops-2"></a>
### OPS-2 `metrics.DeleteSeries` 与 loadtest 运行序列

**提交与首发**：v1.23.0 `7b73aabc`（第十二轮 kit 批 §2）。

**改动文件与关键符号**：`metrics/metrics.go:469` `Registry.DeleteSeries`（空 `match` 不删；`name` 空表示任何名字；四种类型都删；归还 `r.series[name]` 名额）、`:502` `SeriesCount`、`:201` 包级 `DeleteSeries`；`robot/loadtest/manager.go:585` `appendHistoryLocked`（挤出历史时 `metrics.DeleteSeries("", {"run": RunID})`）、`:598` `runIDInUseLocked`（同名 RunID 在跑或在历史里不删）。

**不变量**：删除后序列不在 Snapshot / `/metrics` 里、名额归还；同名同标签再写入是新序列。守卫：`TestDeleteSeriesRemovesEveryKindByLabelAndReturnsTheQuota`（`metrics/delete_series_promises_test.go:11`）、`TestRunSeriesLeaveTheRegistryWithTheRunRecord`（`robot/loadtest/run_series_lifecycle_promises_test.go:21`，`HistoryLimit=2` 连跑 6 次）。

**测试**：loadtest 修前红（第十二轮 kit 批 §2 原文）：

```
run_series_lifecycle_promises_test.go:66: series count kept growing across runs: map[1:7 2:11 3:15 4:19 5:23 6:27] (history limit 2)
```

`metrics` 包的用例是新 API，记录未保存修前红文本。修后 `go test -race -count=3 ./metrics ./robot/loadtest` 通过。

**review 检查点**：

1. loadtest 用包级 `metrics.DeleteSeries`（默认注册表）：确认 `robot.runner.*` 也写在默认注册表，而不是某个注入的注册表。
2. `DeleteSeries("", {"run": id})` 删除任何带该 `run` 标签的序列（不限 `robot.runner.*`）：确认没有别的子系统用 `run` 作标签且不希望被删。
3. `DeleteSeries` 持写锁遍历全部序列（O(序列数)）：只在运行挤出历史时调用一次，确认不在热路径。

<a id="ops-3"></a>
### OPS-3 Ops 同步 bind 与 admin 命令期限

**提交与首发**：v1.21.0 `2c1c7be7`（NC-230 与 N02 O1，`897a1dd9` 标注）。

**改动文件与关键符号**：`kit/ops/ops_mod.go:31-37`（`defaultAdminTimeout = 10s`、`adminWriteMargin = 5s`、`defaultWriteTimeout = 15s`）、`:76-80`（读 `ops.admin_timeout`，写了就必须为正）、`:139-177` `Start`（`:156` 写超时 = max(15s, admin_timeout + 5s)；`:163` `net.Listen`；`:167-175` 登记 server 与实际地址、goroutine 里 `server.Serve(listener)`）、`:191` `commandTimeout`、`:351-360`（命令因期限返回 `context.DeadlineExceeded` → 504 + `effects are unknown`）；`app/config_validation.go:266`（登记 `ops.admin_timeout` 为严格时长键）。

**不变量**：`Start` 返回 nil 就表示探针与运维端点已在监听；配合 ctx 的命令到期返回之后回复一定写得出去。守卫：`TestOpsStartFailsWhenTheAddressIsTaken`（`kit/ops/listen_promises_test.go:20`）、`TestOpsStartServesOnTheBoundAddress`（`kit/ops/stop_contract_test.go:19`，断言写超时 ≥ 命令期限 + 5s）、`TestOpsStopContract`（`:45`，A3 骨架套 OpsMod，卡住的工作是不配合 ctx 的 admin 命令，`CallerReleases`）、`TestOpsAdminCommandRunsUnderTheConfiguredDeadline`（`kit/ops/admin_deadline_promises_test.go:23`）。

**测试**：NC-230 修前红（[问题记录](../../bug/RR-20261005-NC-230.md)原文）：

```
--- FAIL: TestOpsStartFailsWhenTheAddressIsTaken (0.00s)
    listen_promises_test.go:31: Start on 127.0.0.1:59850 (already in use) = nil, want the bind error: the process would run without its probe endpoints
```

admin 期限修前红（[Ops admin 期限方案](../../feature/OPS-ADMIN-TIMEOUT-2026-10-06.md)原文）：

```
--- FAIL: TestOpsAdminCommandRunsUnderTheConfiguredDeadline (0.00s)
    admin_deadline_promises_test.go:57: admin command deadline remaining = -1ns, want ≤ ops.admin_timeout (100ms); -1 means the command ran without a deadline
```

修后 `go test -race -count=3 ./kit/ops/ ./app/` 通过。

**review 检查点**：

1. `Start` 持 `serverMu` 做 `net.Listen`（`:143-144` 加锁并 defer 解锁，`:163` bind）：bind 是本地系统调用、不等待网络，确认不会长时间持锁挡住并发的 `StopWithContext`（`TestConcurrentOpsStartStopUsesCapturedServer` 覆盖）。
2. `Shutdown` 早于 `Serve` 登记 listener 的交错：注释写由 `Serve` 关闭 listener；确认 `Start` 返回后立即 `Stop` 不会泄漏 listener。
3. 504 只在 `errors.Is(err, context.DeadlineExceeded)` 时返回：命令自己包装了别的错误时回 400，确认这仍被视为“结果未知”在文档里写清。

<a id="ops-4"></a>
### OPS-4 Ops 的 `Authorization` 必须带 `Bearer `

**提交与首发**：v1.23.0 `7b73aabc`（第十二轮 kit 批 §4）。

**改动文件与关键符号**：`kit/ops/ops_mod.go:405` `bearerToken`（没有大小写不敏感的 `bearer ` 前缀返回空串）、`:387` `authorized`（`X-Admin-Token` 或 Bearer，`:415` `secretEqual` 常量时间比较；空串永远不等于 token）。

**守卫**：`TestOpsAdminAuthorizationAcceptsOnlyTheExactToken`（`kit/ops/ops_mod_test.go:115`，“bare authorization”移到拒绝组，加“padded bare token”）、`TestOpsAdminAuthorizationRejectsEverythingWithoutAConfiguredToken`（`:146`）。

**测试**：修前红（第十二轮 kit 批 §4 原文）：`ops_mod_test.go:139: bare authorization: authorized when it must not be`。修后通过。

**review 检查点**：`authorized` 先判 `adminToken == ""` 一律拒绝（`:388-390`，已核对），所以 `bearerToken` 返回的空串不可能与空 token 相等；确认没有别的入口（如 admin gateway 转发）绕过 `authorized` 自己解析 `Authorization`。

<a id="ops-5"></a>
### OPS-5 CAS 冲突统一计数

**提交与首发**：v1.23.0 `7b73aabc`（第十二轮 kit 批 §5）。

**改动文件与关键符号**：`versionstore/versionstore.go:149-153`（`MetricCompareAndSet`、`MetricConflict`）、`:158` `CountCompareAndSet`、`:167` `CountConflict`；`versionstore/redis_store.go:369`（`Update` 每次 CAS 计数）、`:386`（预算用尽计 conflict）；`kit/service/rank/redis_store.go:180` / `:192` / `:211`（rank 自己的 CAS 循环，store = `<prefix>:o:`）；`kit/service/chat/store.go:495`（注释：`ErrConflict` 不在这里计数）；`kit/service/README.md`（`servicemetrics.Conflict` 与存储竞争的分工）。

**不变量**：存储竞争只在 versionstore 一处计数；`servicemetrics.Conflict` 只留给业务冲突。守卫：`TestUpdateCountsEveryCompareAndSetAndTheExhaustedConflict`（`versionstore/cas_metrics_promises_test.go:20`：两次成功、一次 3 次全输 → applied=2、lost=3、conflict=1；不保存不计数）；rank `TestSubmitAndPageReportWhatTheyDid`（lost = 8、conflict = 1、`conflict:submit` = 0）；chat 两条改为断言不再自报。

**测试**：新用例修前不能编译（新 API），记录未保存修前红文本。修后 `go test -race -count=3 ./versionstore ./kit/service/rank ./kit/service/chat` 通过。

**review 检查点**：

1. `store` 标签取键前缀（`RedisConfig.Prefix`）：确认每个存储一个固定值、没有把带业务 ID 的前缀传进来（低基数）。
2. account / activity 等直接用 `versionstore.RedisStore` 的服务自动获得计数，没有逐服务的用例（记录“未做”）：review 时抽查一个服务的 `Update` 调用确实经过 `redis_store.go:369`。

<a id="ops-6"></a>
### OPS-6 game-demo 仪表盘补两个面板

**提交与首发**：v1.23.0。`fcc78ad0`（收尾第 2 批 A17：场景复制会话）、`7b73aabc`（第十二轮 kit 批 §9：配置撤回）。

**改动文件与关键符号**：`demo/deploy/dev/observability/grafana/dashboards/roost-demo.json.tmpl:756`（面板 id 27，`sum by (trigger)(increase(configdata_rollback_total{job="{{GAME_SERVICE}}"}[5m]))`）、`:811`（面板 id 26，`sum by (reason)(increase(scene_session_reopen_failed_total{job="{{GAME_SERVICE}}"}[5m]))`）；`codegen/internal/roost/demo_test.go`（断言面板存在、README 仍列出、`kit/configdata/configdata.go` 与 `scene.go` 仍以这两个名字计数）。

**测试**：修前红：`demo_test.go:565: the dashboard has no panel querying configdata_rollback_total by trigger`（第十二轮 kit 批 §9 原文）；A17 的修前红记录未保存，记录给出了实抓（临时探针，不提交）：

```
scene_session_reopen_failed_total{reason="exhausted"} 1
scene_session_reopen_failed_total{reason="refused"} 1
panel "复制会话重开放弃（按原因，5 分钟内次数）" queries scene_session_reopen_failed_total: sum by (reason)(increase(scene_session_reopen_failed_total{job="game"}[5m]))
```

**review 检查点**：导出名转换（点换下划线、计数器加 `_total`）与面板查询一致：`configdata.rollback.total` → `configdata_rollback_total`（已带 total 不重复）。

<a id="ops-7"></a>
### OPS-7 robot Stage 序号只增不回收

**提交与首发**：v1.23.0 `7b73aabc`（RR-20261006-09）。

**改动文件与关键符号**：`robot/runner/runner.go:275-310` `runPopulation`：`launched`（在线目标数，缩容减回去）与 `lastOrdinal`（已发出的最大序号，只增）分开；扩容时先 `lastOrdinal++` 再 `index := lastOrdinal`（`:289-290`）；`IdentityProvider` 注释写明序号可能超过 `Count`。

**守卫**：`TestStageRegrowDoesNotReuseOrdinals`（`robot/runner/stage_ordinal_promises_test.go:17`）；原有 `TestStagedRampUpAndDown`（6 → 1 的在线数）不变。

**测试**：修前红（[问题记录](../../bug/RR-20261006-09.md)原文）：

```
--- FAIL: TestStageRegrowDoesNotReuseOrdinals (0.06s)
    stage_ordinal_promises_test.go:48: robot ordinal 2 was handed out twice across the stages (launch order [1 2 3 2 3]); a regrown stage must not reuse the ordinal (and player id) of a robot it just stopped
```

修后 `go test -race -count=3 ./robot/runner` 通过（序号 `[1 2 3 4 5]`）。

**review 检查点**：`shrinkTo` 只减 `launched`、不动 `lastOrdinal`：确认缩容停掉的机器人的 stop 通道按“末尾”关闭，与新序号的分配互不影响。

---

<a id="tool"></a>
## TOOL

<a id="tool-1"></a>
### TOOL-1 pretag

**提交与首发**：v1.20.0 `999dc672`（失败打印）；v1.20.2 `6c1538be`（NC-205）。

**改动文件与关键符号**：`scripts/pretag.sh:49-58`（`git ls-remote --exit-code --tags origin refs/tags/$version`：0 → 已存在失败；2 → 继续；其他 → 打印 git 错误并失败）、`:100-105`（`go test ./...` 输出写临时文件，失败时 `grep -E '^(--- FAIL|FAIL|panic:)'`，没有匹配就 `tail -50`）。

**测试**：NC-205 修前红（[问题记录](../../bug/RR-20261005-NC-205.md)原文，origin 指向不存在的仓库）：

```
pretag: framework release manifest names v1.99.0
…
pretag: example.com/pretagred@v1.99.0 is ready to tag
pretag exit=0
```

修后 `git ls-remote exit 128`、pretag exit 1；控制：本地 bare origin 无该 tag → ready、exit 0，已有该 tag → `already exists on origin`、exit 1（证据脚本见修复记录）。`999dc672` 是诊断输出改动，没有红绿测试。

**review 检查点**：`set -e` 下 `remote_err="$(…)" || remote_code=$?` 的写法确实捕获到退出码（`:53`），`remote_code` 未赋值时的缺省值为 0。

<a id="tool-2"></a>
### TOOL-2 full 场景 add 序列

**提交与首发**：v1.23.0 `fcc78ad0`（收尾第 2 批 A11）。

**改动文件与关键符号**：`codegen/scripts/full-scenario-adds.sh`（新，`set -euo pipefail`；参数：roost 二进制 + 传给 `project sync` 的参数）；`.github/workflows/framework-compat.yml:83` 与 `codegen/scripts/source-head-check.sh:42` 各调用一次；`ci_full_scenario_test.go:33` `TestFullScenarioAddSequenceIsDefinedOnceAndNotSwallowed`。

**测试**：修前红（[收尾第 2 批](../../bugfix/CLOSING-BATCH-2-2026-10-06.md)原文）：

```
.github/workflows/framework-compat.yml runs an add itself ("\"${RUNNER_TEMP}/roost\" add access player --service gate") ...（10 行）
codegen/scripts/source-head-check.sh swallows a failing subshell: "&& GOWORK=off \"$work/roost\" add saga GuildTransfer -service game -steps debit,credit) || true"
open codegen/scripts/full-scenario-adds.sh: no such file or directory
```

修后根包通过。

**review 检查点**：门禁是文本检查（不跑脚本）：确认它对 `||` 的识别不会被换行续行或子 shell 的其他写法绕过；`uses_rpcs` 的 awk 编辑在 GNU 与 BSD 上同样工作。

<a id="tool-3"></a>
### TOOL-3 冲突标记门禁

**提交与首发**：v1.21.0 `36220f34`（revleft 小防护 A）。

**改动文件与关键符号**：`conflict_marker_gate_test.go:30` `TestNoMergeConflictMarkersInTrackedFiles`（`git grep -nIE '^(<<<<<<<|>>>>>>>)( |$)|^=======$' -- . ':(exclude)artifacts'`）；`:26` `conflictIncidentCommit = "0aa2e1b9"`（自检：历史里有这个提交时扫描它必须报出）。

**测试**：红（[证据](../../bugfix/evidence/noncore-bugfix-20261006-revleft/gate-red.txt)原文，在跟踪文件末尾临时追加冲突）：

```
--- FAIL: TestNoMergeConflictMarkersInTrackedFiles (0.27s)
    conflict_marker_gate_test.go:59: docs/review/PROGRESS.md still carries merge conflict markers (resolve the merge before committing):
          1384: <<<<<<< HEAD
          1386: =======
          1388: >>>>>>> topic
FAIL
```

**review 检查点**：浅克隆里没有 `0aa2e1b9` 时只跳过自检、不跳过正式扫描（`conflict_marker_gate_test.go:38-47`，已核对）；确认 CI 的检出深度下正式扫描仍然运行，且 `git grep` 退出码 1（无匹配）与其他错误分得清（`conflictMarkerFiles`）。

<a id="tool-4"></a>
### TOOL-4 示例实跑门禁

**提交与首发**：v1.23.0 `ba13cb05`。

**改动文件与关键符号**：`examples_run_test.go:32` `exampleRuns`（6 个示例，空串 = 必须运行）、`:46` `TestExamplesRun`（穷尽发现：遍历仓库，跳过 `.git` / `artifacts` / `testdata` / `node_modules`，路径含 `examples` 段且有非测试 `package main` 的目录；与登记表逐字比对；每个示例在自己的模块里 `GOWORK=off go build`（5 分钟）并运行（1 分钟），子用例并行）；`examples/go.mod` / `examples/go.sum`（`GOWORK=off go mod tidy` 补条目）；roost-coding 与 roost-optimize 入口各加一条。

**测试**：先红（[发版前补充验证 §2](../../bugfix/PRERELEASE-VERIFICATION-2026-10-06.md)原文；`git show 229a5aa0^:skill/examples/statusbridge/main.go` 覆盖当前文件并 stash 掉 go.sum 修复）：

```
--- FAIL: TestExamplesRun/examples/robotdemo (0.07s)
    examples_run_test.go:67: 编译 examples/robotdemo（模块 examples）失败：exit status 1
        ../robot/dialers.go:19:2: missing go.sum entry for module providing package github.com/quic-go/quic-go (imported by github.com/tjbdwanghaibo/roost-core/robot); to add:
--- FAIL: TestExamplesRun/skill/examples/statusbridge (2.02s)
    examples_run_test.go:67: 运行 skill/examples/statusbridge 失败：exit status 2
        panic: combatcomponent: persistence mutation outside transaction: nest: transaction is already closed
```

后绿：恢复两处后 6 个子用例全部 PASS；`go test -race -count=3 .` 通过；`examples/`、`skill/examples/` 模块内 `go vet ./...` 通过。

**review 检查点**：

1. 发现规则以路径段 `examples` 判断：确认 `kit/service/examples/split`（库包）被排除的原因是“没有 `package main`”，而不是被硬编码跳过。
2. 门禁需要下载示例模块依赖：离线且缓存缺失时在编译阶段失败（有意）；CI 的根包测试步骤是否有网络。

<a id="tool-5"></a>
### TOOL-5 文档相对链接门禁

**提交与首发**：v1.23.0 `d05a04a1`（RR-20261006-10 同批）。

**改动文件与关键符号**：`doc_links_gate_test.go:28` `docLinkExemptSources`（源文件前缀豁免，当前只有 `docs/history/`）、`:37` `TestTrackedMarkdownRelativeLinksResolve`、`:140`（目标在 `artifacts/` 下豁免）；带自检（合仓遗留形状的链接必须报出、代码块不误报，`:72` 一带）。

**测试**：扩大范围后首跑（修文档前）红：`58 relative links in tracked Markdown point at nothing tracked`——`skill/README.md` 52 处，另 6 处（`docs/bug/REVIEW-2026-09-08.md` 3 处、`docs/review/IMPLEMENTATION-MIRROR-LIFECYCLE.md` 2 处、`docs/feature/SAGA-MONGO-STEP-LATENCY-2026-10-06.md` 1 处）；全部修正后绿。完整修前输出记录未保存（[修复记录](../../bugfix/RR-20261006-10.md)只给了首行与分布）。

**review 检查点**：门禁不检查锚点：Markdown 标题改名后指向它的 `#锚点` 不会被发现，跨文档锚点链接（如本分册互链）要靠显式 `<a id>` 维持。

<a id="tool-6"></a>
### TOOL-6 `scripts/mirror-local.sh`

**提交与首发**：v1.22.0 `b15e70c8`；v1.23.0 扩展 `db67b8ee`（`test-core`、`fault redis-cluster-stop-replica` / `redis-cluster-cont`、`ROOST_MIRROR_LOCAL_REDIS_REPLICA`）、`d483238e`（O-M6-6 场景）、`ba13cb05`（`cluster_replicas_online`）。

**改动文件与关键符号**：`scripts/mirror-local.sh:1-23`（用法）、`:100-122`（`cluster_replicas_online` 与 `cluster_up` 里 30s 等待）、`:279` `fault_action`（`:346` `redis-cluster-stop-replica`、`:355` `redis-cluster-cont`）、`:381` `run_core_tests`、`:427` `finish`（清理与残留检查）、`:446` `test-core` 分支；`codegen/internal/entity/testdata/remoteflow/mirror_local_test.go:145-156`（`ROOST_MIRROR_LOCAL_ONLY` 逗号列表）；依赖 `kit/scripts/integration/lib`（根目录校验、端口平移、按命令行认领 pid）。

**不变量**：不碰共享隔离环境（拒绝 `~/.roost-it` 与偏移 0 / 1000；清掉继承的 `ROOST_DATAENGINE_IT_*`；不读共享 env.sh）；结束时 `clean`（先 SIGCONT 被暂停的进程，再停全部进程、核对没有残留、删根目录）。

**测试**：`cluster_replicas_online` 修前（[发版前补充验证 §4b](../../bugfix/PRERELEASE-VERIFICATION-2026-10-06.md)原文，既有用例单独跑必红）：

```
snapshot_l2_tombstone_wait_integration_test.go:319: WAIT calls on 127.0.0.1:37401 went up by 0, want 1 (the key's primary is 127.0.0.1:37401)
```

修后该用例单独跑 PASS；默认 `test-core`（`^TestMirrorLocal`，5 条）全部 PASS。v1.22.0 的完整运行：7 类故障 `-race` 107.5s 全部 PASS、0 违例；性能 n=6 对照 v1.20.2 无显著差别（L1 命中 p50 +41ns，[Mirror 第 6 步本机替代 §4](../../feature/MIRROR-STEP-6-LOCAL-2026-10-06.md)）。

**review 检查点**：`finish` 在 `ROOST_MIRROR_LOCAL_KEEP=1` 时不清理：确认残留检查不会误杀同机另一个 `mirror-local` 实例（按根目录与偏移认领 pid）。

<a id="tool-7"></a>
### TOOL-7 故障矩阵与验收锁

**提交与首发**：v1.20.2 `6c1538be`（NC-200～208：含 NC-207 无用例记 FAIL、NC-203 入口尊重锁）、`3e3350d5`（A5：全局运维命令运行期间持锁，NC-203 复核补修）；v1.23.0 `0a6155e5`（预跑记录，只改文档）。

**改动文件与关键符号**：`scripts/test-remote-matrix.sh:6-10`（取锁、导出 `ROOST_REMOTE_ACCEPTANCE_LOCK_HELD`）、`:32-34`（`run_case`：退出 0 且日志含 `no tests to run` / `[no test files]` → `FAIL(no tests ran)`）；`kit/scripts/integration/lib/common.sh:71-82`（锁路径与“持锁者子进程沿用”）；`kit/scripts/integration/dataengine-env.sh`、`scripts/remote-fault.sh`、`scripts/test-remote-generated.sh`；`acceptance_lock_promises_test.go:22` `TestGlobalEnvironmentOperationsHoldTheAcceptanceLock`；`integration_coverage_promises_test.go:216` `TestFaultMatrixScriptNamesEveryFullEnvironmentSuite`（v1.19.2 之前已有）。

**测试**：NC-207 修前红：替身 go 输出 `no tests to run` 时修前全部 go test 格 PASS、矩阵 exit 0；修后 8 个 go test 格 `FAIL(no tests ran)`、矩阵 exit 1；控制：替身输出真实 `--- PASS:` 时全部 PASS（脚本与输出见 [NC-207 修复](../../bugfix/RR-20261005-NC-207.md)，问题记录没有抄录可复制的红文本）。v1.23.0 预跑：`ROOST_REMOTE_MATRIX_LABEL=matrix-relprep-20261006 bash scripts/test-remote-matrix.sh`，17:22:13 → 17:26:30，业务 12 格与 lease-process（3）、redis-cluster、redis-unreplicated-fence、durable-process、ownership-counters（13）、mongo-wal-recovery、broker-failover（3）、broker-network（3）、final-health 全部 PASS、无 SKIP；源码 `d6a677e0`。

**未验证与风险**：预跑不在最终发版提交上；NC-207 修后的整张矩阵在 v1.20.2 时没有在真实隔离环境上跑（修复记录“未验证”），之后由每次发版前矩阵覆盖。

**review 检查点**：

1. 新增的矩阵格是否同时登记进 `TestFaultMatrixScriptNamesEveryFullEnvironmentSuite` 的检查范围。
2. `--- SKIP:` 仍记 FAIL：确认新增用例在共享环境缺依赖时不会以 SKIP 静默通过。

---

## 全局守卫测试与门禁清单（本部分新增或改变的）

| 守卫 | 位置 | 钉住什么 | 条目 |
| --- | --- | --- | --- |
| `TestNoMergeConflictMarkersInTrackedFiles` | 根包 `conflict_marker_gate_test.go:30` | 跟踪文件无合并冲突标记 | TOOL-3 |
| `TestExamplesRun` | 根包 `examples_run_test.go:46` | 全部示例能编译并运行到退出码 0，新示例必须登记 | TOOL-4 |
| `TestTrackedMarkdownRelativeLinksResolve` | 根包 `doc_links_gate_test.go:37` | Markdown 相对链接指向跟踪的文件 | TOOL-5 |
| `TestFullScenarioAddSequenceIsDefinedOnceAndNotSwallowed` | 根包 `ci_full_scenario_test.go:33` | full 场景 add 序列只在一处、不吞失败 | TOOL-2 |
| `TestGlobalEnvironmentOperationsHoldTheAcceptanceLock` | 根包 `acceptance_lock_promises_test.go:22` | 全局运维命令持验收锁 | TOOL-7 |
| `internal/stopcontract.Check` | 各停止入口的 `stop_contract_test.go` | 三步停机：超时保留、未放行重试仍超时、放行后释放、再调用 nil | APP-6 |
| `glsvet -stophints` / `-clockhints` | `cmd/glsvet` | 停止入口的裸通道接收；`game` 目录直接读墙钟（只提示） | APP-6、CLK-1 |
| `TestValidateServiceConfigPinsSingletonTimeRelations` | `app/singleton_test.go:1471` | 单实例锁三条时间关系 | APP-1 |
| `TestProductionRefusesANonZeroLogicOffset` | `app/logic_offset_production_promises_test.go:12` | 生产偏移为 0 | CLK-1 |
| `TestBusinessTimeMovingBackRefusesToStart` | `app/business_time_promises_test.go:48` | 业务时间只许前进 | CLK-3 |
| `TestDoctorNamesTheServicesWhoseLogicOffsetDisagrees` | `codegen/internal/roost/logic_offset_doctor_promises_test.go:50` | 同一套配置偏移一致 | CLK-2 |
| `singleton_promises_test.go` 六条 | `codegen/internal/roost` | 生成的 opener / 配置 / 停机预算 / 启动等待 | APP-2 |
| `TestNetworkCodegenTestsRunInSomeWorkflow`（C9，不属于本部分） | 根包 `ci_generated_code_test.go:184` | codegen 联网用例在某个 workflow 里跑 | — |

## 按包的改动索引

| 包 / 目录 | 条目 |
| --- | --- |
| `app` | APP-1、APP-4、APP-5、APP-8、APP-9、APP-12、APP-13、CLK-1、CLK-3、CLK-5、OPS-3（登记键） |
| `clock` | CLK-1 |
| `health` | APP-10、APP-11 |
| `internal/operation` | APP-6、APP-7 |
| `internal/stopcontract` | APP-6 |
| `cmd/glsvet` | APP-6、CLK-1 |
| `metrics` | OPS-2 |
| `servicemetrics` | OPS-1 |
| `timer` | CLK-6 |
| `versionstore` | OPS-5 |
| `etcd/driver` | APP-2 |
| `robot/loadtest`、`robot/runner` | OPS-2、OPS-7 |
| `ai`、`actionflow` | CLK-1（缺省时钟） |
| `kit/redis` | APP-1、APP-3、APP-5、APP-7 |
| `kit/mongo`、`kit/nats` | APP-7 |
| `kit/nest` | APP-4、APP-6 |
| `kit/ops` | APP-10、APP-11、OPS-3、OPS-4 |
| `kit/mods` | APP-1、APP-3、APP-13、OPS-1 |
| `kit/remoteentity` | APP-7、APP-13 |
| `kit/etcd` | APP-3 |
| `kit/service/global` | APP-3、OWN-6 |
| `kit/service/global/activity` | OWN-5、CLK-1、CLK-4 |
| `kit/service/{mail,rank,session}`、`service/mail`、`service/session` | CLK-1、CLK-4、OPS-1 |
| `kit/service/{match,chat,account}`、`service/match` | CLK-2、OPS-1、OPS-5、OWN-2（`account.ServerIDClaim`） |
| `codegen/internal/roost` | APP-2、APP-3、OWN-2～5、CLK-2、OPS-1、OPS-6 |
| `demo/`（game-demo 模板） | OWN-1～5、CLK-1、CLK-6、OPS-6 |
| `scripts`、`codegen/scripts`、`kit/scripts/integration` | TOOL-1、TOOL-2、TOOL-6、TOOL-7 |
| 根包（`*_test.go`） | TOOL-2～5、TOOL-7 |
