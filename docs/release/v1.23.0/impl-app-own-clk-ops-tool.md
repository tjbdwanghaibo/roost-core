# v1.23.0 实现 · APP / OWN / CLK / OPS / TOOL 部分

> 范围与编号同[说明文档](guide-app-own-clk-ops-tool.md)（45 条：APP 15、OWN 7、CLK 6、OPS 8、TOOL 9）。
> `path:line` 一律按最终代码冻结提交 `5e72ca4d`（2026-10-07，之后只允许文档改动）的源码核对；`.tmpl` 行号指模板文件本身。初稿按 `02c8a10d` 写，2026-10-06 按 `e6828e4f` 逐条重核一次，2026-10-07 按 `5e72ca4d` 再逐条重核（见下面“重核记录”）。codebase-memory 共享索引停在 2026-09-30，本文全部位置以当前源码直接读取为准，没有引用索引结论。
> 历史记录与源码冲突时以源码为准，冲突处在条目里写“源码与记录不一致”。修前红文本照记录原样抄录；记录没保存的写“记录未保存修前红文本”。

<a id="recheck"></a>
## 重核记录

### 2026-10-07，最终冻结提交 `5e72ca4d`

- **怎么核的**：脚本抽出三份文件里全部 `path:line` 引用（含只写 `:行号`、沿用前文文件的写法）与相邻的符号名 / 测试名，按 `git diff -U0 e6828e4f 5e72ca4d` 的 hunk 把旧行号映射到新行号：落在未改动区域的按偏移平移，落在改动 hunk 里的、以及“只写 `:行号`”而归属可能不明的，逐条人工读 `5e72ca4d` 的源码。改完后在 `5e72ca4d` 上再验一遍：引用指向测试的必须落在 `func TestX` 那一行，其余的符号必须出现在引用行附近；全部测试名（193 个）在 `5e72ca4d` 里都存在，除明确写“已删除 / 改名”的历史用例与前缀写法（`TestSingletonStore`、`TestRealNatsMod` 之类）。
- **改了的引用**：上一版三份文件里（不计旧的重核记录）共 499 处引用，改了 198 处——166 处是 `e6828e4f..5e72ca4d` 的行号漂移（主要来自 A4① `d1226825` 对 `app/app.go`、`app/singleton.go`、kit 各 Mod 的改写，RR-25 对 `app/app.go`、RR-17 / RR-29 对 activity 模板与 kit activity、A2③ 对 `versionstore/redis_store.go`），32 处人工改：符号被删或挪走的（`readSingletonSettings` / `validate` → `singletonConfig.ValidateConfig`，`validateProductionLogicOffset` → `appConfig.ValidateConfig`，`app/config_validation.go` 的严格读取清单 → 各 Mod 的配置声明，`catalog.go` 的 etcd 段字面量 → `kit/etcd/etcd_mod.go:43` 声明，`ServiceMetrics` / `ServiceMetricsEnabledKey` → `ServiceMetricsConfig`），所在行内容变了的（`run` 的范围与命名返回值、`clock.SetOffset`、`openSingleton` 的登记、activity 模板读组文件的那一行、OPS-5 的计数点），以及上一版“只写 `:行号`”、按上下文会读成别的文件的 10 处（APP-1 检查点 2 的 `renewLoop` / `cas` 实属 `app/singleton.go`；APP-4 检查点 3、APP-12 检查点 1、CLK-1 表里的 `:141` 实属 `app/app.go`；OPS-3 检查点 1、OPS-4 检查点实属 `kit/ops/ops_mod.go`；APP-1 不变量表的 `:1394` 实属 `app/singleton_test.go`——现在都写全路径或重新对齐）。新写的内容另有约 150 处引用，按同样方法核过。本文现有 633 处引用。
- **新增 / 合并的内容**：新条目 APP-14（RR-20261006-28，`3b06c66c`）、APP-15（真实进程演练，`0db819b0`、`3b06c66c`）、OWN-7（RR-20261006-29，`3b06c66c`）、OPS-8（RR-20261006-27，`3b06c66c`）；并入 APP-8（RR-20261006-25，`0db819b0`）、OWN-5（RR-20261006-17 `055a15d6`、`groups_file` 必填 `2c01e06d`、`activity.New` 要组 `061cb538`）、OPS-2（RR-20261006-18 / -19 `055a15d6`，补测 `2c01e06d`）；APP-1 / APP-7 / APP-8 / APP-9 / APP-12、OWN-2 / OWN-3、TOOL-7 / TOOL-9 补演练结论；APP-6 改为 A3② 已完成（`ebf679e1`，REM 分册）。RR-20261006-24 / -26 与 nats/driver 自持关闭状态属 DRV-5，本文只引用。
- **源码与记录不一致（本次新发现，以源码为准）**：RR-20261006-28 的修复记录写检查在 `app`、解析在 `app.RedisClusterAddrs`，A4① 之后在 `kit/redis` 的配置声明（APP-14）；RR-20261006-17 修复记录“后续”的错误文本，A4① 之后由声明的 `required` 报出（OWN-5）。两处在条目里写明，历史记录不改。
- **未验证项的口径**：只列外部环境项，指向 [外部验证清单](../../review/EXTERNAL-VERIFICATION-2026-10-06.md) 的 E 编号；Windows 一律“暂存，不保证正确”（TOOL-8）。上一版单列的 6 项“本机未做”全部闭环（APP-15、OWN-5、OPS-2），**仍未闭环 = 0**。

### 2026-10-06，冻结提交 `e6828e4f`（上一版，留作历史）

- 脚本抽出全部 `path:line` 引用（483 处）与相邻的符号名，在 `e6828e4f` 的源码里逐条比对，改 13 处（5 处因 `b7471ae4` / `565f657b` 行号漂移，8 处是初稿写偏）；核对了 158 个测试名与全部提交号。
- 新增 TOOL-8（`7fec136e`）、TOOL-9（`87d8d91e`）；APP-7 并入 roost-coding 的 `RedisMod` 例外（`b7471ae4`）；APP-9 并入注释挪位（`b7471ae4`）；OWN-4 / OWN-5 并入 RR-20261005-01 回归在 C4 后的去向与 `TestAGroupFitsOneLiveQuery`（`d5682dc4`）；三处方案文档按源码更正（App 锁方案 §3.6 与 §13 obs34、D-L3 方案 §3.2）。

## 怎么用这份文档 review

**建议顺序**（依赖从下往上）：

1. APP-4（`RuntimeFailure.OnFail`）→ APP-1（单实例锁状态机与 `run` 挂点）→ APP-9 / APP-8 / APP-12（`run` 的启动失败、停机、退出三条收尾路径）→ APP-5 / APP-13（能力登记）→ APP-14（Cluster 配置）→ APP-15（真实进程演练，汇总 ①～⑥ 与各条目的实测）。`app/app.go` `run`（`:180-568`）是这几条的交汇点，先通读一遍再看条目。
2. APP-6 / APP-7（共用停机类型）→ OPS-3（Ops 停止与 bind）。
3. CLK-1 → CLK-2 → CLK-3 / CLK-4 / CLK-5（时钟边界与高水位，高水位挂在单实例锁之后）→ CLK-6（timer，独立）。
4. OWN-2 → OWN-3 / OWN-4 / OWN-5（game-demo 模板与 kit 协调器，依赖 APP-1 / APP-5）→ OWN-7（通关自开窗，依赖 OWN-5 的组核对）→ OWN-6（kit global，独立）。
5. OPS-1 / OPS-2 / OPS-5 / OPS-8（metrics 口径与基数、分位数估计）→ OPS-4 / OPS-6 / OPS-7。
6. TOOL-*（脚本与根包门禁，独立；TOOL-8 / TOOL-9 只改文档与规范）。

**先读的规范**（[roost-coding](../../agent-skills/roost-coding/SKILL.md)）：“生命周期与装配的复审要点”里的三步停机与“新的停机对象优先用共用类型”（APP-6～9、OPS-3）；“业务时钟与系统时钟”（CLK-*）；“反复出问题要上报方向判断”（OWN-1 → OWN-2 的先例）；“验证与性能纪律”的“示例要实跑”与共享隔离环境规则（TOOL-4、TOOL-7）。[roost-bugfix §7](../../agent-skills/roost-bugfix/SKILL.md)“交给 review 之前不留 WANTED”（TOOL-9）：本文各条的 review 检查点都是给 review 去查的问题，不含待判断的风险。改了错误分类、关闭所有权的条目按 [fix-contract-review](../../agent-skills/roost-coding/references/fix-contract-review.md) 复核。

**本地复跑**（全部 `GOWORK=off`）：

| 范围 | 命令 |
| --- | --- |
| APP | `go test -race -count=3 ./app/ ./lifecycle/ ./kit/nest/ ./kit/redis/ ./kit/ops/ ./health/ ./internal/operation/ ./internal/stopcontract/` |
| 停机骨架 | `go test -race -count=3 -run 'StopContract\|Lifetime\|Serial' ./internal/... ./bus/ ./sync/syncbus/... ./manager/ ./kit/nest/ ./etcd/driver/ ./remoteentity/ ./kit/ops/` |
| CLK | `go test -race -count=3 ./clock/ ./timer/ ./service/mail/ ./kit/service/mail/ ./kit/service/global/activity/ ./kit/service/chat/ ./kit/service/account/ ./service/match/ ./kit/service/match/ ./cmd/glsvet/` |
| OPS | `go test -race -count=3 ./metrics/ ./robot/loadtest/ ./robot/runner/ ./versionstore/ ./kit/service/rank/ ./kit/service/chat/ ./servicemetrics/ ./kit/mods/ ./nest/ ./bus/`；改了 `nest` 的加跑 `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` |
| OWN（kit） | `go test -race -count=3 ./kit/service/global/... ./kit/service/account/` |
| OWN（模板） | 生成 game-demo：`roost project new <名> -template game-demo`，`go mod edit -replace github.com/tjbdwanghaibo/roost-core=<worktree>`，`go build ./... && go vet ./... && go test -race -count=3 ./internal/service/game/ ./game/controllers/player/ ./internal/access/... && go test ./...` |
| codegen | `go test -count=1 ./codegen/...`；`go generate ./...` 后 `git status --porcelain` 为空 |
| 根包门禁 | `go test -count=1 .`（含 TOOL-2～5 与 `TestCoreDependencyBoundary`） |
| 真实依赖（可选） | Redis：`REDIS_ADDR=<隔离 Redis> go test -tags integration -run 'TestSingletonStore\|TestBusinessTimeHighWaterMarkOnRealRedis' ./kit/redis/`；Mirror 私有环境：`ROOST_MIRROR_LOCAL_HOME=<私有目录> scripts/mirror-local.sh test-core`；kit Mongo / NATS 真实 Close（私有环境在跑时）：`go test -tags integration -race -count=3 -run 'TestRealNatsMod\|TestRealMongoMod' ./kit/nats/ ./kit/mongo/`；真实进程演练脚本见 APP-15 |

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
| [APP-7](#app-7) | `operation.Serial` 与 kit Mod 停止收敛 | v1.23.0（本版）；NC-233 / 234 v1.21.0 | `d05a04a1` `02c8a10d` `b7471ae4` `2c1c7be7`（`0db819b0` 真实依赖用例） | `internal/operation`、`kit/redis`、`kit/mongo`、`kit/nats`、`kit/remoteentity` |
| [APP-8](#app-8) | 停机 hook 受预算；超时点名 hook | v1.21.0（点名 v1.23.0） | `2c1c7be7` `0db819b0` | `app`、`lifecycle` |
| [APP-9](#app-9) | 启动失败收尾 | v1.20.2 | `d6550a16`（`b7471ae4` 只挪注释） | `app` |
| [APP-10](#app-10) | Degraded 算就绪 | v1.21.0 | `f6828f17` | `health`、`kit/ops` |
| [APP-11](#app-11) | checker 期限 | v1.23.0（本版） | `7b73aabc` | `health`、`kit/ops` |
| [APP-12](#app-12) | 退出原因进文件日志 | v1.23.0（本版） | `611d5d72` | `app` |
| [APP-13](#app-13) | App 能力与 Mod 依赖 | v1.20.0 / v1.21.0 / v1.23.0（本版） | `d4ac9853` `b9fc5342` `d483238e` | `app`、`kit/mods`、`kit/remoteentity` |
| [APP-14](#app-14) | 生成工程整体切 Redis Cluster | v1.23.0（本版） | `3b06c66c`（`d1226825` 挪位） | `kit/redis`、`demo/cmd/accountctl`、`codegen/internal/roost` |
| [APP-15](#app-15) | 真实进程演练（单机 + 同机 Cluster） | v1.23.0（本版） | `0db819b0` `3b06c66c` | `docs/bugfix`、`kit/mongo`、`kit/nats`、`kit/scripts/integration` |
| [OWN-1](#own-1) | 租约状态机期三处修复 | v1.20.0 | `18bb86ae` `a28a3152` `890abdda` | `demo/internal/service/game`（已删除的代码） |
| [OWN-2](#own-2) | 玩家静态绑定 | v1.20.0 | `f051e24a` `021454d5` `c441fbdd` | `demo`、`kit/service/account`、`codegen/internal/roost` |
| [OWN-3](#own-3) | 赠礼按 `FromSID` 路由 | v1.20.0 | `5bdac773` `c493a791` `ac5acfbe`（`054fdd66` 预算入配置） | `demo`、`codegen/internal/roost` |
| [OWN-4](#own-4) | activity 用 `Live`；候选校验 | v1.20.0 / v1.20.1 | `f051e24a` `46c4dfba` `d5682dc4` | `demo` |
| [OWN-5](#own-5) | 活动组文件；协调器按组核对、`groups_file` 必填 | v1.20.2（核对与必填 v1.23.0） | `277e1252` `d5682dc4` `055a15d6` `2c01e06d` `061cb538` | `kit/service/global/activity`、`demo`、`codegen/internal/roost` |
| [OWN-6](#own-6) | global `Bind` 幂等 | v1.23.0（本版） | `611d5d72` | `kit/service/global` |
| [OWN-7](#own-7) | 通关自开窗 | v1.23.0（本版） | `3b06c66c` | `demo` |
| [CLK-1](#clk-1) | 双时钟 | v1.21.0 | `b9fc5342` | `clock`、`app`、`kit/service/*`、`timer`、`ai`、`actionflow`、`cmd/glsvet`、`demo` |
| [CLK-2](#clk-2) | match / chat / account 换钟，doctor | v1.21.0 | `fa472ee7` | `kit/service/{match,chat,account}`、`codegen/internal/roost` |
| [CLK-3](#clk-3) | 业务时间只许前进 | v1.22.0 | `3e77beb9` | `app` |
| [CLK-4](#clk-4) | activity / mail 回业务钟 | v1.22.0（拆分 v1.21.0） | `b9fc5342` `5a3c4a60` `3e77beb9` | `kit/service/global/activity`、`service/mail`、`kit/service/mail` |
| [CLK-5](#clk-5) | 高水位推进失败计数 | v1.23.0（本版） | `7b73aabc` | `app` |
| [CLK-6](#clk-6) | timer 排序与 priority | v1.21.0 | `5abae51e` | `timer`、`demo` |
| [OPS-1](#ops-1) | 服务指标默认 | v1.20.2 | `491aaf3b` | `servicemetrics`、`kit/mods`、`kit/service/*`、`codegen/internal/roost` |
| [OPS-2](#ops-2) | `DeleteSeries`；派发器序列；RPC method 标签有界 | v1.23.0（本版） | `7b73aabc` `055a15d6` `2c01e06d` | `metrics`、`robot/loadtest`、`nest`、`bus` |
| [OPS-3](#ops-3) | Ops 同步 bind、admin 期限 | v1.21.0 | `2c1c7be7` | `kit/ops`、`app` |
| [OPS-4](#ops-4) | Ops Bearer | v1.23.0（本版） | `7b73aabc` | `kit/ops` |
| [OPS-5](#ops-5) | CAS 统一计数 | v1.23.0（本版） | `7b73aabc` | `versionstore`、`kit/service/{rank,chat}` |
| [OPS-6](#ops-6) | 仪表盘面板 | v1.23.0（本版） | `fcc78ad0` `7b73aabc` | `demo/deploy`、`codegen/internal/roost` |
| [OPS-7](#ops-7) | robot 序号 | v1.23.0（本版） | `7b73aabc` | `robot/runner` |
| [OPS-8](#ops-8) | 分位数收在观测范围；阈值失败说明 | v1.23.0（本版） | `3b06c66c` | `metrics`、`robot/loadtest`、`demo/cmd/loadtest` |
| [TOOL-1](#tool-1) | pretag | v1.20.0 / v1.20.2 | `999dc672` `6c1538be` | `scripts` |
| [TOOL-2](#tool-2) | full 场景 add 序列 | v1.23.0（本版） | `fcc78ad0` | `codegen/scripts`、`.github/workflows`、根包 |
| [TOOL-3](#tool-3) | 冲突标记门禁 | v1.21.0 | `36220f34` | 根包 |
| [TOOL-4](#tool-4) | 示例实跑门禁 | v1.23.0（本版） | `ba13cb05` | 根包、`examples` |
| [TOOL-5](#tool-5) | 文档链接门禁 | v1.23.0（本版） | `d05a04a1` | 根包 |
| [TOOL-6](#tool-6) | mirror-local.sh | v1.22.0（扩展 v1.23.0） | `b15e70c8` `db67b8ee` `d483238e` `ba13cb05` | `scripts` |
| [TOOL-7](#tool-7) | 故障矩阵与验收锁 | v1.20.2（预跑 v1.23.0） | `6c1538be` `3e3350d5` `0a6155e5` | `scripts`、`kit/scripts/integration`、根包 |
| [TOOL-8](#tool-8) | Windows 不保证正确 | v1.23.0（本版） | `7fec136e` | `README.md`、`docs/DEPLOYMENT.md`、`CHANGELOG.md` |
| [TOOL-9](#tool-9) | 交给 review 前不留 WANTED | v1.23.0（本版） | `87d8d91e` | `docs/agent-skills/roost-bugfix` |

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
| `app/singleton.go:57` `SingletonOpener`、`:115` `App.Singleton` | bootstrap 安装 opener；未启用时不调用 |
| `app/singleton.go:109-111`、`:123-131` `singletonConfig`、`:133` `startupWait` | `singletonReleaseBudget = 3s`；`singleton.*` 的配置声明（A4① 起，CFG 主题）：tag 缺省 15s / 3s / 5s，`startup_wait` 不写取 2×ttl |
| `app/singleton.go:146` `singletonConfig.ValidateConfig` | 启用时 `key_prefix` 必填、无空白与三条时间关系；由 App 的配置声明检查调用（`app/config_schema.go:85` `checkConfig`，`app/app.go:150` 在 `loadServiceConfig` 里、任何 Mod Init 之前；`ValidateServiceConfig` 转调同一检查，`app/config_validation.go:13`） |
| `app/singleton.go:281` `cas` | 每次 CAS 记 `asked`；持有期间单次超时 = `min(renew_interval, validUntil − asked)` |
| `app/singleton.go:296` `timely` | Applied 只有在 `replied < asked + ttl − guard` 时作数 |
| `app/singleton.go:330` `acquire`、`:368` `confirmOwnValue` | 启动获取：重试、认领自己的值、到 `startup_wait` 报 `ErrSingletonHeld` / `ErrSingletonStoreUnavailable` |
| `app/singleton.go:409` `startRenewal`、`:430` `renewLoop` | 单 goroutine 固定节拍续期；四个分支：按时 Applied / 迟到 Applied / NotHeld / Unknown |
| `app/singleton.go:467` `lose` | 吸收态 Lost，`RuntimeFailure.Fail(ErrSingletonLost …)` |
| `app/singleton.go:487` `finish`、`:511` `release` | 统一收尾：停续期 → 按状态与 `mayRelease` 决定 Release → 只在没拿到锁或全部 Mod 停完时 Close store |
| `app/singleton.go:536` `checkHealth` | Held OK / Unknown Degraded / Lost 与未持有 Fail |
| `app/singleton.go:589` `openSingleton` | 打开 store、登记 `ModSingleton` 与 `ModSingletonIncarnation`、登记健康检查 |
| `app/app.go:269-292` | `run` 里的挂点：`openSingleton` → `defer finish` → `acquire` → `startRenewal`，都在 `sortMods`（`:304`）与第一个 Mod Init 之前 |
| `app/app.go:478-493` | 停机时 Mod 停止截止提前 `min(3s, total/2)`，已 Lost 为 0 |
| `kit/redis/singleton.go:44` `SingletonStore`、`:69` `newSingletonStore` | 两个独立客户端（`:24-25` 各 PoolSize 2，`:74` `MaxRetries = -1`）；`:145` `Close` |
| `kit/mods/name.go:14` | `mods.ModSingleton` 别名 |

**不变量与强制位置**：

| 不变量 | 强制位置 | 守卫测试 |
| --- | --- | --- |
| 锁拿到之前不 Init 任何 Mod | `app/app.go:269-292` 在 `:304` 之前 | `TestSingletonWaitsForTheHolderBeforeAnyModInit`（`app/singleton_test.go:576`） |
| 不抢别人持有的锁 | `acquire` 只在 `current` 为空或等于自己的值时成功 | `TestSingletonGivesUpAtStartupWaitWithoutTakingTheKey`（`:599`）、`TestSingletonKeepsWaitingWhenALostReplyHidesAnotherHolder`（`:765`） |
| 本地窗口不晚于键过期 | `cas` 记 `asked`，`hold` 设 `validUntil = asked + ttl` | `TestSingletonWindowStartsWhenTheRenewalWasAsked`（`:1003`） |
| Unknown → Lost 不晚于 `validUntil` | `cas` 截断单次超时 | `TestSingletonUnknownRenewalTimeoutLosesNoLaterThanTheWindowEnd`（`:926`）、`TestSingletonUnknownRenewalsLoseOnlyAtTheEndOfTheWindow`（`:864`） |
| 迟到的 Applied 不作数 | `timely` | `TestSingletonALateAppliedRenewalDoesNotCount`（`:1026`） |
| Lost 是吸收态，恰好一次 fail-stop、先围栏后唤醒 | `lose` 之后 `renewLoop` 返回；`RuntimeFailure` 先执行回调 | `TestSingletonNotHeldFailsOnceAndFencesBeforeShutdown`（`:802`） |
| 只在全部 Mod 停完且未失锁时 Release | `run` 各返回点置 `singletonReleasable`，`finish` 判 `state` | `TestSingletonReleasesOnlyAfterEveryModStopped`（`:1066`）、`TestSingletonIsNotReleasedWhenShutdownIsIncomplete`（`:1159`）、`TestSingletonIsReleasedWhenAModStopReturnsAnOrdinaryError`（`:1240`） |
| 已 Lost 不预留 Release 时间 | `app/app.go:487-490` | `TestSingletonLostLockLeavesTheReleaseBudgetToModStop`（`:1394`） |
| 停机不完整时 store 不关（Live 仍可用） | `finish` 末段 | `TestSingletonIsNotReleasedWhenShutdownIsIncomplete` 三个子例 |
| 续期不被 Live 拖住 | 两个独立客户端 | `TestSingletonStoreRenewalDoesNotWaitBehindStalledLiveQueries`（`kit/redis/singleton_integration_test.go:197`，integration） |
| 启用必须有 opener；配置关系 | `openSingleton`、`singletonConfig.ValidateConfig` | `TestSingletonOpenerIsRequiredOnlyWhenEnabled`（`:1440`）、`TestValidateServiceConfigPinsSingletonTimeRelations`（`:1471`） |

**控制流**（`run` 内，按执行顺序）：

1. `loadServiceConfig`：读配置并按 App 与本服务全部 Mod 的配置声明检查（A4①；含 singleton 三条关系），`a.settings` 保存读出的值；日志、`NewRegistry`、`PhaseAppInit`。
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
- 当前代码上的重跑（v1.23.0，`0db819b0` / `3b06c66c`，[真实进程演练](../../bugfix/REAL-PROCESS-DRILLS-2026-10-06.md) ① ⑥）：私有依赖（`mirror-local.sh up` + 自起 etcd、唯一 DAO 库名），单机 Redis 一遍、同机 3 主 3 从 Cluster 两轮；§8.2 六步与 1c、6、6a、6b、6c 全部符合：SIGSTOP / kill -9 后新进程等锁 15.001～15.010s，SIGTERM 旧进程 0.1s 后起新进程等 3.0s，SIGCONT 到失锁 5ms、到 `app run failed` 63ms（Cluster 69ms），P2 在 ops Start 处因端口占用退出码 1、`singleton: released`。见 [APP-15](#app-15)。

**性能**：锁只在启动、每 3s 续期、停机时各一次 Redis 往返，不在请求路径上；方案 §8.3 判断不需要性能对照，没有基准数据。

**未验证（外部环境）**：多机 Redis Cluster 切主下的两客户端 store（E08；同机 3 主 3 从上 `redis-cluster-suites.sh` 已跑过，含 `./kit/redis`，真实进程演练也在同机 Cluster 上做过）；异步复制丢写与切主（E10）；多主机强杀（E13）。跨主机 / 换卷不在范围（维护者决定）。

**review 检查点**：

1. `app/app.go` `run` 每个 `return` 前 `singletonReleasable` 是否只在“全部 Mod 都停完”的路径上为 true：逐个核对 `:299`、`:306`、`:318`、`:324`、`:331`、`:336`、`:345`、`:354`、`:362`、`:368`、`:377`、`:384`、`:392`、`:402`、`:436`、`:552-554`；漏置为 false 的方向是安全的（键过期），误置为 true 是缺陷。
2. `renewLoop` 的 Unknown 分支用 `reply.replied` 与 `validUntil − guard` 比较（`app/singleton.go:452-461`），而 `cas` 的截断用 `asked`（`:284-286`）：确认“截断 + 比较”合起来保证 `TestSingletonUnknownRenewalTimeoutLosesNoLaterThanTheWindowEnd` 的承诺，在 `ttl / renew / guard` 取边界值（如 14 / 3 / 3）时也成立。
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
| `codegen/internal/roost/render.go:762` `turnGeneratedSingletonOn` | `add mod dataengine` 之后把与生成文本逐字相同的 `enabled: false` 段翻成 true，改过的段保持并 WARN |
| `codegen/internal/roost/shutdown_budget.go:72` `generatedSingletonRelease`、`:93`、`:236-261` | 停机预算 +3s、摘要行 `+ 3s for the singleton release` 与回读正则 |
| `codegen/internal/roost/render_deploy.go:248` `startupAllowance`、`:1029` `kubernetesStartupFailureThreshold` | shell `HEALTH_ATTEMPTS` 与 k8s `startupProbe` 按 `startup_wait + 30s` |
| `kit/etcd/etcd_mod.go:43`（声明的 `example`）、`codegen/internal/roost/kitconfig_gen.go:152`（生成器快照） | etcd 段 `service_prefix: /roost/services/`。A4① 起生成的配置段由 kit Mod 的配置声明渲染（CFG 主题），原 `catalog.go` 里的字面量随之删除，值不变 |
| `etcd/driver/discovery.go:203` `Deregister`、`:250` `isLeaseNotFound` | 租约已不存在视为注销达成；注册循环退出后再取消一次 keepalive |

**不变量**：opener 安装条件必须覆盖“默认启用 singleton 的服务”（否则启动即 fail-closed）；停机预算的 Release 份额与 App 的 `singletonReleaseBudget` 一致（`shutdown_budget.go:72` 注释写明镜像关系）。守卫：`codegen/internal/roost/singleton_promises_test.go` 六条（bootstrap 安装条件、只给 dataengine 服务打开、add mod 翻转与手改保持 + WARN、停机摘要含 Release 且能读回、doctor 计入 Release、部署启动等待）；`TestGeneratedEtcdServicePrefixSeparatesTheServerType`；etcd `TestDiscoveryDeregisterTreatsLeaseNotFoundAsDeregistered`、`TestAssemblyCloseTreatsLeaseNotFoundAsDeregistered`、`TestDiscoveryDeregisterStopsRegistrationThatCompletedDuringShutdown`、app `TestSingletonIsReleasedWhenAModStopReturnsAnOrdinaryError`；真机 `TestRealEtcdCloseAfterLeaseVanishedIsClean`（`-tags integration`）。

**测试**：

- 第 2 笔修前红（方案 §13 第 2 笔原文摘录）：`redis without dataengine: bootstrap does not install a.Singleton(kitredis.SingletonStore)`、`configs/service/config.account.yaml: a service without dataengine has singleton.enabled = "" (present false), want an explicit false`、`deploy/shell/install.sh lacks "  game) DEFAULT_HEALTH_ATTEMPTS=60 ;;\n"`、`compose game: start_period is not 60s`、`singleton on, total 105s: ok total_timeout …`。
- etcd 两处：记录写“真机回归修前红文本与演练日志相同”，演练日志原文为 `mod etcd stop: etcd Discovery: revoke: etcdserver: requested lease not found`；`service_prefix` 的修前红记录未保存。
- 修后：`go test -count=1 ./codegen/...` 全绿；生成 game-demo `go build ./... && go vet ./...`、`go test ./internal/service/game/`；`shellcheck deploy/shell/*.sh deploy/docker/*.sh deploy/k8s/*.sh` 无输出。

**未验证**：真实 systemd 部署（E21）；k8s 部署物与滚动停机（E22，含 `startupProbe` 渲染能被集群接受；E22 记 compose 已在本机实跑）。

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

**兼容说明**（不是未验证项）：仓外调用方无法在仓内核对，按破坏性变更登记；旧 `<global.key_prefix>:lease:*` 键不自动清理，运维按 [DEPLOYMENT §7.1](../../DEPLOYMENT.md#71-升级后的手工清理) 手工删除。

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
| `kit/nest/nest_mod.go:220-222` | `Provide` 里 `failure.OnFail(m.engine.Fence)` |
| `app/app.go:256` `startupFailure` | 启动检查点：`:330`（共享 Mod 每个 Start 之前）、`:376`（服务专属 Mod）、`:391`（全部 Start 之后）、`:417`（`Service.Init` 之后） |
| `app/app.go:245-253` | `failureReturned` 与 defer：失败没进过返回值就包成 `app: runtime failure after shutdown began: …` 并入 |
| `app/app.go:461-464` | select 的 `Done` 分支置 `failureReturned` |

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
3. `failureReturned` 的 defer 登记在 `singleton.finish` 的 defer 之前（`app/app.go:246` 早于 `:276`），所以后执行：确认 `finish` 期间发生的失败也被并入（`TestRuntimeFailureDuringShutdownIsReturned`）。
4. 启动检查点之间（例如某个 Mod 的 `Start` 内部很长）发生的失败要等到下一个检查点才停：确认没有 Mod 依赖“失败后立刻不再被 Start”。

<a id="app-5"></a>
### APP-5 `Live` 与 C5 契约

**提交与首发**：v1.20.0 `d4ac9853`（`Live`）、`ad35bbcc`（独立 Live 客户端）；v1.21.0 `bd6df5e5`（C5：注释、USER_GUIDE、用例）。

**改动文件与关键符号**：`app/singleton.go:59-71`（`SingletonLiveness` 与 C5 契约注释）、`:74` `ModSingleton`、`:93` `SingletonLiveMaxSIDs = 200`、`:557` `singletonLiveness.Live`（空 `serverType` 报错、空 `sids` 直接返回、超过 200 报错）、`:628` 登记；`kit/redis/singleton.go:116` `Get`（逐键 GET，单机一次 pipeline，Cluster 由客户端按槽拆分）。

**不变量**：活性只有锁这一个事实来源；不引入“停机中”中间值。守卫：`TestSingletonLiveReportsSidsHoldingTheLock`（`app/singleton_test.go:1604`）、`TestSingletonLiveCountsAStoppingProcessUntilRelease`（`:1097`，`Service.Shutdown`、服务 Mod Stop、共享 Mod Stop 三处查 `Live` 都得到本 sid，Release 后键不在）、`TestSingletonStoreGetReadsEveryKeyInOrder` / `TestSingletonStoreGetAcrossClusterSlots`（`kit/redis/singleton_integration_test.go:146` / `:152`）。

**测试**：C5 用例钉住现有行为，本来就绿，没有修前红（B9 / C5 方案 §5）。

**源码与记录不一致**：App 锁方案 §3.6 写“一次调用最多 `MaxPageSize` 个 sid（与 `LiveGames` 的上限相同）”；`MaxPageSize` 已在第 3b 笔随 global 租约删除，源码是 `app.SingletonLiveMaxSIDs = 200`（`app/singleton.go:93`），CHANGELOG v1.20.0 写的也是 200。以源码为准；方案 §3.6 已加更正（2026-10-06，本次重核）。

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
| `bus/jetstream_rpc.go`、`sync/syncbus/driver/jetstream.go`、`sync/syncbus/mirror/envelope.go`（v1.20.2） | 三份手写门迁移到 `Lifetime`。v1.23.0 A3②（`ebf679e1`，REM 分册）之后 mirror 那一份删除，排空下沉到 `sync/syncbus/subscription.go` 的 `Subscription.Unsubscribe(ctx)`（同样用 `Lifetime`）；另有 `nestwal/wal.go`、`nestwal/committer.go`、`dataengine/engine/projector.go`、`remoteentity/snapshot_client.go` 在用 |
| `internal/stopcontract/stopcontract.go:30` `Hooks`、`:51` `Check`、`:110` `CallerReleases` | 骨架：首次超时返回 ctx 错误且资源保留 → 未放行时重试仍超时 → 放行后新 ctx 重试返回 nil 且资源确实释放 → 再调用返回 nil |
| `cmd/glsvet/stophints.go`、`cmd/glsvet/main.go:38-41` | `-stophints`（缺省开）：带 ctx 的停止类函数里不受 ctx 约束的通道接收（跟进一层同包 helper）打印 `hint:` |

**套用骨架的停止入口**：`manager/stop_contract_test.go`、`kit/nest/stop_contract_test.go`（`TestNestModStopContract`）、`sync/syncbus/driver/stop_contract_test.go`、`etcd/driver/stop_contract_test.go`、`sync/syncbus/mirror/stop_contract_test.go`、`remoteentity/stop_contract_test.go`、`bus/stop_contract_test.go`、`kit/ops/stop_contract_test.go`（`TestOpsStopContract`）、`codegen/internal/roost/player_tcp_stop_contract_promises_test.go`（生成 TCP，注入骨架源码运行）、`kit/remoteentity/remote_mirror_mod_promises_test.go`、`remoteentity/snapshot_client_promises_test.go`。

**不变量**：停止返回 nil ⇒ 回调已静止、依赖可释放；重试收敛；`Released` 观察真实后果而不是被测对象自己的字段。守卫：`TestLifetimeStopDrainsOnlyAdmittedCalls` / `TestLifetimeWaitIsBoundedRetryableAndReportsTheRealDrain` / `TestLifetimeZeroValueWaitWithoutCallsReturnsNil`（`internal/operation/lifetime_test.go:10` / `:38` / `:68`）；`TestCheckPassesAStopperBuiltOnTheSharedLifetime` / `TestCheckCatchesKnownWrongStoppers`（`internal/stopcontract/stopcontract_test.go:155` / `:162`）；`TestStopHints*`（`cmd/glsvet/stophints_test.go:21-117`）。

**测试**（[A3 证据](../../bugfix/evidence/a3-stop-contract-20261005/README.md)）：骨架对故意写错的对象红——`retry Stop while the work is still in flight = <nil>`、`Stop returned without releasing the resource`；套到 NC-170 / 171 / 173 / 174 / NC-90 的修前实现、生成 TCP 的 NC-83 修前形状上都红（各自红文本在证据目录的 `*-prefix-red.txt`）；骨架在当时的 main 上发现 etcd `Assembly.Close` 第 4 步红（停完再 Close 返回 `context canceled`），补修为 `driver.Client.Close` 只关一次。迁移后原有 NC-90 / NC-172 / NC-174 回归 `-race -count=3` 通过。glsvet：NC-173 修前 `discovery.go` 命中 `Deregister(ctx) calls waitLoopDone …`，修后全仓非测试文件 0 条。

**范围外**：A3 ②（排空下沉到 `ISyncBus` 带 ctx 的退订）原定下个大版本，第十三轮维护者要求本版完成，已实施（`ebf679e1`，RR-20261006-36，REM 分册）；`worker.Pool` 自带等价的准入与排空，没有改用 `Lifetime`（A3 方案只迁移三份手写门）。

**review 检查点**：

1. `Lifetime.Wait` 先查排空再看 ctx（“已排空且 ctx 已结束”返回 nil）：确认三处迁移后的调用方没有依赖旧的随机返回 ctx 错误。
2. 消息回调里只有 `Begin` / `End`（一次短临界区），等待只在停止入口：在 syncbus / mirror 的迁移里确认没有在快池或消息回调里调 `Wait`。
3. 新增的停止入口是否都套了骨架：`git grep -ln 'func .*StopWithContext\|func .*) Close(ctx' -- '*.go'` 与上面的列表对照。

<a id="app-7"></a>
### APP-7 `operation.Serial` 与 kit Mod 停止收敛

**提交与首发**：v1.23.0 `d05a04a1`（RR-20261006-10，`Serial`、kit Mod 串行与数据竞争）、`02c8a10d`（roost-coding 写入口径）、`b7471ae4`（roost-coding 补 `RedisMod` 的 `sync.Mutex` 例外，只改规范）；v1.21.0 `2c1c7be7`（NC-233 Redis Mod、NC-234 remote_entity Mod）；v1.23.0 `0db819b0`（真实进程演练 ④：kit Mongo / NATS 的真实依赖 Close 用例，只加测试；同提交的 nats/driver 修复 RR-20261006-24 / -26 属 DRV-5）。驱动层的 Close 口径（redis / mongo / etcd / nats driver）属于 DRV 主题，本条只覆盖 `internal/operation.Serial` 与 kit Mod 一侧。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `internal/operation/serial.go:14` `Serial`、`:20` `Lock(ctx)`、`:39` `Unlock` | 容量 1 的通道；先非阻塞尝试，再在调用方 ctx 内等；零值可用（`sync.Once` 建通道） |
| `kit/mongo/mongo_mod.go:25`、`:143-146` | `StopWithContext` 用 `stopSerial` 串行；`mu` 保护 `client` |
| `kit/nats/nats_mod.go:32`、`:206-209` | 同上；`mu` 保护健康检查读的 `asm` |
| `kit/redis/redis_mod.go:23-24`、`:163-168` | `sync.Mutex` `mu` 保护 `asm`；`StopWithContext` 持锁；第一次 Stop 先取出并置空 `asm` 再 Close（NC-233） |
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

- 真实依赖（v1.23.0 `0db819b0`，[真实进程演练](../../bugfix/REAL-PROCESS-DRILLS-2026-10-06.md) ④，[红绿摘录](../../bugfix/evidence/real-process-drills/2026-10-06/app7-close-contract-real.txt)）：integration 用例经正式的 Init → Provide → Start 接私有 Mongo / NATS——`TestRealMongoModCloseContract`（`kit/mongo/close_contract_real_promises_test.go:27`）4 个并发 Stop 全部 nil、再 Stop 两次 nil、之后的操作立即返回 `mongo.ErrClientDisconnected`，首跑即绿；`TestRealNatsModCloseContract`（`kit/nats/close_contract_real_promises_test.go:61`）首跑红在 Subscribe / QueueSubscribe / CallAsync / JetStream 的错误 `errors.Is` 不到 `fnats.ErrClosed`（RR-20261006-24，DRV-5），修后绿；`TestRealNatsModUndrainedCloseIsReportedOnce`（`:188`）终态错误只报一次，`-race -count=3` 循环里偶发“硬关之后 `Connected()` 仍为 true”（RR-20261006-26，DRV-5），修后 `-count=30` 绿。修后 `GOWORK=off go test -tags integration -race -count=3 -run 'TestRealNatsMod|TestRealMongoMod' ./kit/nats/ ./kit/mongo/` 通过。
- 修后：`go test -race -count=3 ./internal/operation ./redis/driver ./mongo/driver ./etcd/driver ./nats/driver ./kit/redis ./kit/mongo ./kit/nats` 通过；私有 redis-server 上 `-tags integration -race -count=3 -p 1 ./redis/driver ./kit/redis` 通过。NC-233 用例原来靠“再调一次驱动 Close 得到 ErrClosed”制造 Close 错误，驱动幂等后改为先关底层 `Raw()` 连接池。

**范围说明**：三个 toxiproxy 用例覆盖的“丢回复 → 未知”路径 RR-20261006-10 没有改，那一批没有重跑（修复记录）；`TestSingletonStoreGetAcrossClusterSlots` 需要 Cluster，由同机 3 主 3 从的 `redis-cluster-suites.sh` 覆盖，真实进程的跨槽 `Live` 在演练 ⑥ 实测。kit Mongo / NATS 的 Close 已接真实依赖（上面“测试”）。**未验证（外部环境）**：多机 Cluster 切主（E08）。

**review 检查点**：

1. roost-coding 的 Serial 一条写明了例外（[规范](../../agent-skills/roost-coding/SKILL.md)“生命周期与装配”，`b7471ae4`）：临界区很短、关闭不等在途工作的可以用 `sync.Mutex`，kit `RedisMod`（`kit/redis/redis_mod.go:24`）是举出的例子。确认 `StopWithContext`（`:163-179`）持 `mu` 期间只有取出 `asm` 与 `asm.Close()`（go-redis 的 Close），没有会等网络或在途工作的调用——否则后到者的等待不受自己的 ctx 约束，例外的前提不成立。
2. `Serial.Lock` 返回 nil 后调用方必须恰好 `Unlock` 一次：检查 mongo / nats Mod 与 driver 的每条返回路径（`defer` 是否紧跟在成功的 `Lock` 之后）。
3. Mongo / Nats Mod 在串行器上等到 ctx 结束时“保留 client / asm、下次 Stop 继续”：确认健康检查此时读到的状态与“停机未完成”一致。

<a id="app-8"></a>
### APP-8 停机阶段 lifecycle hook 受预算（NC-231）

**提交与首发**：v1.21.0 `2c1c7be7`（NC-231）；v1.23.0 `0db819b0`（RR-20261006-25：超时错误点名卡住的 hook；同提交是真实进程演练 ③）。

**改动文件与关键符号**：`app/app.go:857` `emitLifecycleWithin`（goroutine 里 `emitStopLifecycle`，在 ctx 内等；`atomic.Pointer[string]` 记下最后一个开始的 hook，到期返回 `finished=false` 与点名该 hook 的错误（`:872-875`，还没有 hook 开始时保持旧文本）；与到期同时返回的按已返回算）；`:891` `emitStopLifecycle` 调 `lifecycle.Registry.EmitAllWatched`（`lifecycle/lifecycle.go:119`，每个 hook 开始前同步回调它的名字；`:111` `EmitAll` 改为转调它）；调用点 `app/app.go:498-510`（`service.stopping`：`!finished` 时直接返回，不调 Shutdown、不停 Mod、`singletonReleasable` 保持 false）与 `:560-566`（`service.stopped`：只并入错误）。启动阶段的 hook 仍用同步的 `emitLifecycle`（`app/app.go:228`、`:397`、`:420`）。

**不变量**：hook 可能正用着 Service / Mod 的能力，超时后不在它底下拆依赖；单实例锁释放规则不变。守卫：`TestShutdownLifecycleHooksStayWithinTheShutdownBudget`（`app/shutdown_hooks_promises_test.go:44`，两个阶段子例；v1.23.0 起还断言错误含卡住的 hook 名）、`TestEmitAllWatchedReportsEachHookBeforeItRuns`（`lifecycle/lifecycle_test.go:83`，按 Order 报名、前一个失败不跳过后面）；`TestAppReturnsServiceStoppingLifecycleError`（hook 返回普通错误）不变。

**测试**：修前红（[问题记录](../../bug/RR-20261005-NC-231.md)原文）：

```
--- FAIL: TestShutdownLifecycleHooksStayWithinTheShutdownBudget (10.61s)
    --- FAIL: TestShutdownLifecycleHooksStayWithinTheShutdownBudget/service.stopping (5.30s)
        shutdown_hooks_promises_test.go:79: run did not return 5.3s after a service.stopping hook that ignores its ctx (shutdown.total_timeout 300ms)
    --- FAIL: TestShutdownLifecycleHooksStayWithinTheShutdownBudget/service.stopped (5.30s)
        shutdown_hooks_promises_test.go:79: run did not return 5.3s after a service.stopped hook that ignores its ctx (shutdown.total_timeout 300ms)
```

修后两个阶段都在预算附近返回 DeadlineExceeded；stopping 卡住时 Shutdown 0 次、Mod 停 0 次、不 Release；stopped 卡住时 Shutdown 1 次、Mod 停 1 次。`go test -race -count=3 ./app/` 通过。

RR-20261006-25 修前红（[问题记录](../../bug/RR-20261006-25.md)原文）：

```
--- FAIL: TestShutdownLifecycleHooksStayWithinTheShutdownBudget/service.stopping (0.30s)
    shutdown_hooks_promises_test.go:93: run error = app: lifecycle service.stopping hooks did not return within the shutdown budget: context deadline exceeded, want it to name the hook that did not return ("blocking-service.stopping")
--- FAIL: TestShutdownLifecycleHooksStayWithinTheShutdownBudget/service.stopped (0.31s)
    shutdown_hooks_promises_test.go:93: run error = app: lifecycle service.stopped hooks did not return within the shutdown budget: context deadline exceeded, want it to name the hook that did not return ("blocking-service.stopped")
```

修后两个阶段通过；`go test -race -count=3 ./app/ ./lifecycle/` 通过。真实进程（[演练 ③](../../bugfix/REAL-PROCESS-DRILLS-2026-10-06.md)，game-demo，`shutdown.total_timeout` 10s，`ROOST_DRILL_STUCK_HOOK` 登记不配合 ctx 的 hook）：stopping 卡住时没有 `mod stopped`、`singleton: shutdown incomplete; leaving the key to expire`、退出码 1，SIGTERM 到 `app run failed` 10.006s；stopped 卡住时 25 个 Mod 停完、`no shutdown time left to release`，10.005s；修后错误为 `app: lifecycle service.stopping hook "drill-stuck-service.stopping" did not return within the shutdown budget: context deadline exceeded`（stopped 同）。

**未验证（外部环境）**：部署平台的停机宽限期（E21 systemd、E22 k8s `terminationGracePeriodSeconds`）。真实进程里的 SIGTERM 时序已在演练 ③ 实测（上面“测试”）。

**review 检查点**：

1. `service.stopping` 的 hook 用的是 `shutdownCtx`（完整预算），而 Mod 停机用的是提前了 `releaseReserve` 的 `modStopCtx`：确认 hook 吃掉大部分预算时 Mod 停机仍按剩余时间如实超时（`stopModsReverseBefore`）。
2. ops 的就绪位 hook（`kit/ops/ops_mod.go:164-172`）在 `service.stopping` 阶段：确认它不阻塞，不会让 `/readyz` 在 hook 超时路径上一直报就绪。
3. 错误点名的 hook 是“最后一个开始、还没返回的”：这只在 `EmitAllWatched` 串行派发、`starting` 在派发 goroutine 上同步调用时成立（`lifecycle/lifecycle.go:119`）——确认派发顺序与 `Order` 一致、前一个返回错误时后面的照常开始（`TestEmitAllWatchedReportsEachHookBeforeItRuns`），且 `atomic.Pointer` 的写读之间没有别的共享状态。

<a id="app-9"></a>
### APP-9 启动失败先收回 Service 已启动的部分（NC-193）

**提交与首发**：v1.20.2 `d6550a16`。`b7471ae4`（v1.23.0）只把 `stopModsReverse` 的文档注释挪回函数上方。

**改动文件与关键符号**：`app/app.go:407-439`（Init 失败与 Init 之后的启动失败合成一条收尾）、`:594-595` `startupCleanupTimeout = 5s`、`:602` `shutdownAfterStartupFailure`（goroutine 里调 `Shutdown`，recover panic；返回 nil → 已停；ctx 错误 / 超时 / panic → 未停；其他错误 → 已停并并入）、`app/service.go`（`Service` 注释：启动失败时也会调用 `Shutdown`，必须容忍部分初始化）。

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

修后 `app` `-race -count=3` 通过；生成 game-demo build / vet / test 通过。真实进程（v1.23.0 `0db819b0`，[演练 ②](../../bugfix/REAL-PROCESS-DRILLS-2026-10-06.md)）：预建冲突的 JetStream consumer，让 game 的 `Service.Init` 最后一步 `startActivityPhaseConsumer` 失败；第一条 `mod stop` 之前 activity 循环在途的 bus 请求以 `nats: request cancelled` 结束（ctx 被 `Shutdown` 取消，NATS Mod 13ms 后才停），`mod init` / `mod start` / `mod stopped` 各 25、`mod stop failed` 0，`singleton: released`、键随即不存在，退出码 1，文件日志 `app run failed err=service game init: game: nats: API error: code=500 err_code=10012 …`。启动失败的收尾路径不打 `service shutdown`，“先收回、再停 Mod”的顺序由 `TestServiceInitFailureStopsWhatInitStartedBeforeTheMods` 钉住。

**源码注释位置（已修）**：初稿基准 `02c8a10d` 上，`stopModsReverse` 的文档注释错放在 `startupCleanupTimeout` 常量上方，`go doc` 把它算作常量注释。`b7471ae4` 已把它挪回函数上方（现在注释 `app/app.go:635-637`、函数 `:638`），只改注释，行为无关。

**review 检查点**：

1. `shutdownAfterStartupFailure` 超时返回后 `Shutdown` goroutine 仍在跑：确认 `run` 返回后进程退出是唯一的回收方式（生产路径），测试里再次 `Run` 的泄漏可接受。
2. 仓外 `app.Service` 实现若在 `Shutdown` 里解引用 Init 才设置的字段会 panic → “收尾不完整” → 不释放锁、键 TTL 过期：确认这是安全方向（不会形成两个写者）。
3. `stopIncomplete(out.err)` 的分类（`:625`）与正常停机路径对 `Shutdown` 返回 ctx 错误的处理一致。

<a id="app-10"></a>
### APP-10 `/readyz`：Degraded 算就绪（D1）

**提交与首发**：v1.21.0 `f6828f17`（`ce79ef18` 标注）。

**改动文件与关键符号**：`health/health.go:17` `StatusDegraded`、`:31-38` 聚合规则注释与 `Snapshot.Degraded`、`:43` `DegradedResults`、`:174-183` 聚合（OK 不影响、Degraded 只置位、Fail 与未知 Status 让 `OK=false`）；`kit/ops/ops_mod.go:284` `handleReady`（200 条件仍是“就绪位 ∧ `deps.OK`”，响应体加 `degraded` 与 `degraded_dependencies`，`:301`）；`app/singleton.go:533-548` `checkHealth` 注释。

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

**改动文件与关键符号**：`app/app.go:211-216`：最先登记的 defer，`runErr != nil` 时 `slog.Error("app run failed", "err", runErr)` 再 `flog.Close()`；`run` 改为命名返回值 `runErr`（`:180`，NC-232 引入）。

**不变量**：这个 defer 最后执行，`runErr` 已含停机期间并入的 RuntimeFailure（APP-4）与单实例锁收尾结果。守卫：`TestRunWritesTheExitReasonToTheFileLog`（`app/exit_reason_log_promises_test.go:29`）。

**测试**：修前红（[问题记录](../../bug/RR-20261006-07.md)原文）：

```
--- FAIL: TestRunWritesTheExitReasonToTheFileLog (0.00s)
    exit_reason_log_promises_test.go:48: the exit reason "mod init failed: exit reason 7c1e" is not in the file log:
        ... msg="starting server" ...
        ... msg="mod init" mod=exit_reason
```

修后 `go test -race -count=3 ./app` 通过。真实进程演练里 Init 失败（②）、失锁 fail-stop（①）、停机 hook 超时（③）三种退出都在文件日志里留下了 `app run failed`（[APP-15](#app-15)）。

**review 检查点**：

1. `run` 在 `flog.Init` 之前的返回（读配置失败、按配置声明检查失败——A4① 起两者都在 `loadServiceConfig`（`app/app.go:128-156`）里——与 `flog.Init` 失败，`:182-204`）不经过这个 defer：确认这些错误由生成 main 的 `server exit` 打到 stderr，文件日志本就不存在。
2. `runErr` 只经命名返回值与 defer 修改：确认没有 `return` 用遮蔽的局部变量绕过它。

<a id="app-13"></a>
### APP-13 App 能力与 Mod 依赖

**提交与首发**：`ModSingleton` v1.20.0 `d4ac9853`；`ModBusinessClock` v1.21.0 `b9fc5342`；`ModSingletonIncarnation` v1.23.0 `d483238e`（O-M6-6，Remote 锁接管的完整实现属于 REM 主题）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `app/singleton.go:628-640` | `openSingleton` 登记 `ModSingleton` 与 `ModSingletonIncarnation`（token 取锁值第一段） |
| `app/singleton.go:76-90` `SingletonIncarnation{Key, Sid, Token}` | 能拿到它即说明本进程已持有该锁；只在 `singleton.enabled=true` 时登记 |
| `app/registry.go:43` | `NewRegistry` 按 `time.logic_offset` 登记 `clock.NewBusiness(offset)`（偏移经 `readTimeConfig` 读 App 的配置声明，`app/config_schema.go:249`） |
| `app/business_clock.go:9` `ModBusinessClock`、`:21` `BusinessClock` | 取 Registry 里的业务钟，没有就退回 `clock.Process()` |
| `kit/mods/name.go:14-16` | `ModSingleton`、`ModSingletonIncarnation` 别名 |
| `kit/remoteentity/remote_entity_mod.go:122-123` | `incarnation.Sid == m.localSid` 时 `deps.Incarnation = &ProcessIncarnation{Holder: Key, Token}` |
| `remoteentity/assemble.go:26-28`、`:113-114` | `AssemblyDeps.Incarnation` 非 nil 时第一次取实体共享锁就接管同 sid 上一代 |
| `demo/internal/service/game/activity.go.tmpl:96-101` | `startActivity` 第一步查 `app.ModSingleton`，取不到报错 |

**不变量**：Mod 在锁拿到之后才 Init / Provide，读到 `SingletonIncarnation` 时锁已持有；未启用 singleton 时不登记，依赖方不得接管 / 不得退化为“只有自己”以外的语义。守卫：`TestSingletonIncarnationIsTheHeldLocksIdentity`（`app/singleton_incarnation_promises_test.go:12`）、`kit/remoteentity/lock_incarnation_promises_test.go`、`TestActivityRefusesToStartWithoutTheAppLockLiveness`（`demo/internal/service/game/activity_test.go.tmpl:261`）、`TestBusinessClockFollowsTheConfiguredOffset`（`app/business_clock_promises_test.go:22`）。

**测试**：`SingletonIncarnation` 是新 API，记录未保存 app 侧的修前红文本；O-M6-6 的行为红绿（修前重启后的写 2408ms 等旧租约，修后 33ms、无多写）见 [Mirror 第 6 步观察 §7](../../feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md)。

**review 检查点**：

1. `RemoteEntityMod` 只在 `incarnation.Sid == m.localSid` 时接管：确认 `localSid` 与 App 写进配置的 `sid` 是同一来源，不同 sid、旧格式 token、`singleton.enabled=false` 一律不接管。
2. `app.BusinessClock(nil)` 的退回路径只给没有 Registry 的框架库与测试用：`git grep -n 'BusinessClock(nil)'` 列出调用点（目前 game-demo 两处兜底），确认生产路径都传了 Registry。

<a id="app-14"></a>
### APP-14 生成工程能整体切到 Redis Cluster（RR-20261006-28）

**提交与首发**：v1.23.0 `3b06c66c`（真实进程演练 ⑥ 准备环境时发现）。之后 A4①（`d1226825`，CFG 主题）把生产检查从 `app` 挪到 kit Redis 配置声明，行为与错误文本不变。

**改动文件与关键符号**（当前源码）：

| 位置 | 职责 |
| --- | --- |
| `kit/redis/redis_mod.go:45-47` `ClusterConfig` | `redis.cluster_addrs` 的声明（逗号串或 YAML 列表、每项去空白），Redis Mod、单实例锁连接与各服务 Mod 的 hash tag 校验共用 |
| `kit/redis/redis_mod.go:51-58` `Config`、`:63-68` `Config.ValidateConfig` | `env: production` 下 `redis.addr` 与 `redis.cluster_addrs` 至少一个非空，否则 `config: production requires redis.addr or redis.cluster_addrs`；`:101` `redisConfig` 经 `app.LoadConfig` 读，单实例锁 store（`kit/redis/singleton.go:48`）同样受它约束 |
| `demo/cmd/accountctl/main.go.tmpl:38`、`:46-54` | `-redis-cluster addr,addr,...`：给了就填 `fredis.Config.ClusterAddrs`，`-redis` / `-redis-db` 忽略 |
| `codegen/internal/roost/render_dev_run.go:134-144` | 生成的 `deploy/dev/run.sh` `register_game_server` 从 `config.account.yaml` 的 `redis:` 块读 `cluster_addrs`（逗号串或 YAML 列表），有值就 `accountctl -redis-cluster`，否则照旧 `-redis` / `-redis-db` |

**不变量**：“是不是 Cluster”只有一个判据（`redis.cluster_addrs` 非空即 Cluster、优先于 `addr`），生产检查、Redis Mod、单实例锁、生成的运维脚本按同一个判据；生产检查只要求“有 Redis 可连”，不要求写一个被 Cluster 覆盖、不起作用的 `addr`。守卫：`TestProductionRedisNeedsAnAddrOrClusterSeeds`（`kit/redis/config_types_promises_test.go:72`：`addr` / 逗号串 / YAML 列表三种通过，什么都没有 / 空 `addr` / 只有逗号 / 空列表 / 空白项五种拒绝）、`TestClusterAddrsAcceptAYAMLListAndTrimEntries`（`:14`）、`TestProductionDoesNotRequireSwitchesNothingReads`（`kit/config_schema_promises_test.go:217`，经 `app.CheckConfig` 的整服务检查）、`TestDevRunRegistersTheGameServerOnTheAccountServicesRedisCluster`（`codegen/internal/roost/dev_run_account_promises_test.go:124`，逗号串与 YAML 列表两种）。

**测试**：修前红（[问题记录](../../bug/RR-20261006-28.md)原文；当时检查在 `app`，用例是 `app/production_redis_cluster_promises_test.go`）：

```
--- FAIL: TestProductionServicesOnARedisClusterPassWithoutRedisAddr (0.00s)
    production_redis_cluster_promises_test.go:23: production game on a Redis Cluster (comma string): config: production game requires redis.addr
    production_redis_cluster_promises_test.go:23: production game on a Redis Cluster (yaml list): config: production game requires redis.addr
    （instance / account / global / match_group 同样，共 10 条）
--- FAIL: TestDevRunRegistersTheGameServerOnTheAccountServicesRedisCluster/comma_string (0.47s)
    dev_run_account_promises_test.go:132: accountctl -redis-cluster = "" (present false), want the three seeds; args ["run" "./cmd/accountctl" "-redis" "127.0.0.1:6379" "-redis-password" "" "-redis-db" "0" "-prefix" "roost:planet:account" "upsert-server" "-sid" "1000"]
--- FAIL: TestDevRunRegistersTheGameServerOnTheAccountServicesRedisCluster/yaml_list (0.21s)
    （同上）
```

accountctl 的 `-redis-cluster` 是新参数，红是真实进程上的 MOVED（修前模板生成的 `bin/accountctl` 对 6 个节点逐个登记，5 个 `accountctl: upsert server: MOVED 14044 127.0.0.1:48402`）。修后 `go test -race -count=3 ./app/ ./kit/mods/ ./kit/redis/` 与 `go test ./codegen/...` 通过；真实 3 主 3 从 Cluster 上新 `bin/accountctl -redis-cluster <6 个种子>` 登记 1000 / 1001 成功，两个 sid 的机器人建角、登录正常（演练 ⑥）。

**源码与记录不一致（以源码为准）**：修复记录写生产校验在 `app/config_validation.go`、解析收到 `app.RedisClusterAddrs`、`kit/mods.RedisClusterAddrs` 转调、守卫 `TestProductionServicesOnARedisClusterPassWithoutRedisAddr` / `TestProductionServicesStillNeedSomeRedis` 在 `app`。A4①（`d1226825`）之后：检查是 `kit/redis.Config.ValidateConfig`，`cluster_addrs` 由声明读出，`app.RedisClusterAddrs` 与 `kit/mods.RedisClusterAddrs` 已删除，`app/production_redis_cluster_promises_test.go` 删除，承诺由上面的 `TestProductionRedisNeedsAnAddrOrClusterSeeds`（注释写明 RR-20261006-28 与 A4① 的挪动）与 `TestProductionDoesNotRequireSwitchesNothingReads` 承担；错误文本由 `config: production <type> requires redis.addr or redis.cluster_addrs` 变为不带服务类型的 `config: production requires redis.addr or redis.cluster_addrs`（声明不按服务类型分）。

**未验证（外部环境）**：多机 Redis Cluster 切主（E08）；Cluster 下的业务服务组合（E09）。

**review 检查点**：

1. 生产检查现在跟着“谁读 Redis 配置”走：确认 game / instance / account / match_group / global 这些原来按服务类型要求 Redis 的进程，都注册了 Redis Mod 或开了单实例锁（否则修前会报的缺失现在不报）；`TestProductionDoesNotRequireSwitchesNothingReads` 用 `kitredis.NewRedisMod()` 覆盖了其中哪几种。
2. `render_dev_run.go` 的 awk 对 `cluster_addrs:` 后面同一行有值（逗号串）与下一行起的 `    - ` 列表两种写法都取到，遇到 `redis:` 块之外的顶层键就停：对照 `TestDevRunRegistersTheGameServerOnTheAccountServicesRedisCluster` 的两种输入，确认带引号、带空格的种子被 `tr -d "\"' "` 正确去掉。
3. accountctl 在 `-redis-cluster` 与 `-redis` 同时给出时只用 Cluster：确认 `fredis.Config` 的 `ClusterAddrs` 非空时驱动忽略 `Addr` / `DB`（与 Redis Mod 的“Cluster 优先”同一判据）。

<a id="app-15"></a>
### APP-15 真实进程演练：单机与同机 Redis Cluster

**提交与首发**：v1.23.0 `0db819b0`（①～④，RR-20261006-24～26；恢复自 WIP `976086d6`）、`3b06c66c`（⑤ ⑥，RR-20261006-27～29）、`a8e3933d`（交接记录）。记录：[REAL-PROCESS-DRILLS-2026-10-06](../../bugfix/REAL-PROCESS-DRILLS-2026-10-06.md)；App 锁方案 §13 第 5 笔的“未验证”改为实测结论（`3b06c66c`）。

**范围与环境**：被测是 main `37338490` + 本批改动（①～④）与 `0db819b0` + RR-27 / 28（⑤ ⑥ 第一轮，第二轮再加 RR-29）。依赖全部私有：`scripts/mirror-local.sh up`（端口偏移 30000 / 31000：Mongo 副本集 3 节点、NATS JetStream 3 节点、Redis 单机或 3 主 3 从 Cluster）、自起 etcd；生成工程 `roost project new … -template game-demo` replace 到被测检出，DAO 库名编译期常量改成唯一名字；单实例锁用生成默认值 15s / 3s / 5s / 30s。不碰共享隔离环境 `~/.roost-it`、共享的 game / saga / remote_entity 库与 `ROOST_*` 流；结束 `teardown.sh` 停全部进程、`mirror-local.sh clean`、`ps` 复查无残留。演练只用的注入件（`cmd/drillinject` 预建冲突 consumer、`zz_drill_hooks.go` 登记卡住的 hook）不进生成器。

**结论**（逐项时长与日志见记录与 `docs/bugfix/evidence/real-process-drills/` 的时间线、日志摘录）：

| 项 | 步骤与观测 | 涉及条目 |
| --- | --- | --- |
| ① 单 sid（§8.2 六步 + 1c/2c） | SIGSTOP P1 后 P2 只有 `singleton: acquiring` / `waiting … holder=…`、没有 `mod init`，等锁 15.010s，拿锁后 ops 端口占用（NC-230）、逆序停完、`released`、退出码 1；SIGCONT P1 5ms 失锁、63ms `app run failed`，etcd 租约过期按 Info 记（`b67d5945`）；kill -9 后等锁 15.009s；SIGTERM 旧进程 0.1s 后起新进程等 3.002s；1c/2c 失锁时断开 4 个在服连接 | APP-1、APP-2、APP-4、OPS-3、OWN-2 |
| ① 两 sid（6、6a、6b、6c） | 两个键各自持有，开窗 `expected_game_sids=[1000, 1001]`（`Live`）；40 个赠礼 saga 全部终结；6c 回 `100015`；6b kill 时 5 个在途，接替进程上线后全部 completed、`manual_required` 0 | APP-5、OWN-2、OWN-3、OWN-4 |
| ② Init 最后一步失败 | 见 APP-9“测试” | APP-9、APP-12 |
| ③ 停机 hook 卡住 | 见 APP-8“测试”；发现 RR-20261006-25 | APP-8 |
| ④ kit Mongo / NATS 真实 Close | 见 APP-7“测试”；发现 RR-20261006-24、-26（DRV-5）；`dataengine-env.sh` 包列表加 `./kit/mongo` | APP-7、TOOL-7 |
| ⑤ 两 sid 时 loadtest `rc=1` | 查明是分位数口径，发现 RR-20261006-27 | OPS-8、OWN-3 |
| ⑥ 同机 Cluster 两轮 | 两个 sid 的锁键在不同主节点（槽 12937 / 8872），`Live` 无 `CROSSSLOT` / `MOVED`；①的全部步骤与单机一致（等锁 15.001～15.010s、SIGCONT 到失锁 5ms、到 `app run failed` 69ms）；准备环境发现 RR-20261006-28，第一轮发现 RR-20261006-29 | APP-1、APP-5、APP-14、OWN-2、OWN-3、OWN-7 |

**复跑**：脚本在 `docs/bugfix/evidence/real-process-drills/scripts/`（`setup.sh` → `frameworks.sh` → `drill1.sh` → `drill1b.sh` → `drill2.sh` → `drill3.sh` → `teardown.sh`；Cluster 模式多设 `DRILL_REDIS=cluster`，每轮唯一 DAO 库名与端口偏移）；④ 是 `GOWORK=off go test -tags integration -race -count=3 -run 'TestRealNatsMod|TestRealMongoMod' ./kit/nats/ ./kit/mongo/`（需要私有环境在跑、只 source env.sh 不打印）。`frameworks.sh` 只能跑一次，要重置数据就整套 teardown + setup（只删 Mongo 库会留下 remote_entity 库与 Redis 里的 L2 快照，记录写了这次操作失误的现象）。

**测试**：演练本身不改代码；各项发现的红绿在对应条目（APP-8、APP-14、OPS-8、OWN-7；RR-24 / 26 在 DRV-5）。验证命令见记录“验证”一节：`go test -race -count=3 ./nats/driver/ ./kit/nats/ ./kit/mongo/ ./app/ ./lifecycle/`、`./metrics/ ./robot/... ./app/ ./kit/mods/ ./kit/redis/`，根包、`go build ./... && go vet ./...`，`go test ./codegen/...`、`go generate ./...` 后 porcelain，重新生成 game-demo 的 build / vet / test。

**未验证（外部环境）**：多机 Redis Cluster 切主与 MOVED / ASK 期间（E08）；Cluster 下的业务服务组合（E09）；异步复制丢写（E10）；多主机强杀（E13）；真实 systemd / k8s（E21 / E22）。etcd 的真实 Close 不在 ④ 范围（`TestRealEtcdCloseAfterLeaseVanishedIsClean` 已在 `b67d5945` 用真实 etcd，APP-2）。

**review 检查点**：

1. 演练记录的时间线与日志摘录是否支持表里每一格的结论（尤其 6b：kill 的时刻确实有在途 saga——第一次沿用旧脚本 kill 时 saga 已全部终结，记录写明“不算反例”并改为轮询到出现在途 saga 再 kill）。
2. ⑥ 的 Cluster 配置是否就是 USER_GUIDE“生成工程切到 Redis Cluster”清单里的全部设置（`setup.sh` 的 `DRILL_REDIS=cluster` 分支），没有清单以外的手工改动。
3. 演练脚本的清理（`teardown.sh`）在任何一步失败时是否仍会执行，不在共享环境留下进程或数据。

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
| `demo/internal/service/game/playerowner.go.tmpl:96` `ErrNotServedHere`、`:101` `IdleUnload = 5m`、`:112` `evictBudget = 5s` | 拒绝错误、闲置阈值、调用方等一次卸载的上限 |
| `:180` `Serve` | 登录：sid 判定 → 等卸载（`evictWait` / ctx）→ `touchLocked` |
| `:221` `AdmitBound` | 后台准入：非本服 `local=false`；卸载中 `(true, err)`；否则建记录 |
| `:251` `Admit`、`:275` `AdmitMessage` | 写准入只看记录与卸载状态；登录消息豁免 |
| `:287` `Resident` | 只读，不算使用（matchmaker） |
| `:306` `CloseServedSessions` | 停机时断开有驻留记录的玩家（锁外关闭） |
| `:365` `unloadIdle`、`:390` `unloadOne`、`:427` `runEviction` | 闲置卸载：锁外问连接数、锁内复核闲置后标记；单航班；删记录先于 `close(done)` |
| `demo/internal/access/player/tcp/auth.go.tmpl:101` | 认证器写 `Claims[svcaccount.ServerIDClaim]` |
| `kit/service/account/types.go:294` `ServerIDClaim = "server_id"` | 键常量（传输包与控制器包共用） |
| `demo/game/controllers/player/enter_game.go.tmpl:175` `BoundServerID`、`:48`、`:70` | 读 claim；缺失时 `player_elsewhere`（`owner_sid=0`）；`Serve` 返回 `ErrPlayerElsewhere` 时透传 |
| `demo/internal/service/game/service.go.tmpl:243-245` | `Shutdown` 第一步 `CloseServedSessions` |

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

**已判定维持现状的观察**（App 锁方案 §13 obs34，理由在记录里）：冷加载超过 `IdleUnload` 后实体留在内存、没有驻留记录（只占内存，WriteGate 照常拒绝）；`Destroy` 永不返回时卸载 goroutine 与 `evictions` 条目泄漏（根因在实体上永不结束的事务）；优雅停机先断会话、listener 仍开（只多一次重登，不形成两个写者）。

**真实进程验证**：当前模板上重跑了演练（单机与同机 Cluster，[APP-15](#app-15) ① ⑥）：6c 绑定在 1001 的角色连 1000 回 `response code 100015: player is bound to another server; reconnect to that server`；1c / 2c 失锁 fail-stop 时 `game: disconnected the players this process served sessions_closed=4`、退出码 1，P7 立即接管；SIGSTOP 期间的 Mongo 玩家快照与接管后逐字段相同。**未验证（外部环境）**：多主机强杀与双实例（E13）。

**review 检查点**：

1. `Admit` 在 Nest 快池上调用（`playerowner.go.tmpl:41` 注释）：确认只有一次加锁、没有等待（roost-coding“快池内不得阻塞等待”）。
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
| `demo/internal/service/game/gift_saga.go.tmpl:234-237` `playerOwnership.AdmitBound`、`:246-248` `giftStepHandoff.FromSID`、`:308` 路由 `KeyOf = cmd.FromSID` | 准入接口、转交载荷、按 sid 路由 |
| `:325` `runHandoff` | 接收方：解码载荷 → 核对信封 `PlayerID` / `FromSID` 与载荷一致（`:336-337`）→ 核对 topic 与 phase → `AdmitBound`（`:358`） |
| `:457` `admitPhase` | 发送侧：`FromSID <= 0` 拒绝并记 Error（`:473`）→ `AdmitBound`（`:482`）→ 非本服则 `handoff.Route` 后 nak（`:493-514`） |
| `codegen/internal/roost/demo.go:200-248` `demoGiftRefundBudget` | 把 `saga.steps.gift_item.debit.max_attempts: 15` 写进 game 服务三份配置 |

**不变量**：debit / refund 只在发送方绑定的 sid 上执行；接收方先核对再认领；`FromSID == 0` 不兜底。守卫：`demo/internal/service/game/gift_handoff_test.go.tmpl` 的 `TestAnOfflineSendersStepRunsOnTheSidTheyAreBoundTo`（`:124`）、`TestAdmitRefusesAndHandsOverAForeignStep`（`:147`）、`TestAHandoffIsAddressedToTheSendersBoundSid`（`:163`）、`TestAStepDoesNotClaimASenderBoundElsewhere`（`:177`）、`TestAStepThatNamesNoSenderSidIsRefused`（`:191`）、`TestAStepForASenderBeingUnloadedIsRefusedWithoutAHandoff`（`:217`）、`TestAHandoffForAnOfflineSenderBoundHereIsRun`（`:235`）、`TestAHandoffForAPlayerWeDoNotOwnIsDropped`（`:249`）、`TestAHandoffWhoseEnvelopeDisagreesWithItsPayloadIsRefused`（`:270`）、`TestAHandoffWhosePhaseDisagreesWithItsCommandIsRefused`（`:289`）、`TestAHandoffWithAnUnknownPhaseIsRefusedBeforeTheClaim`（`:312`）；`send_gift_test.go` `TestSendGiftWritesThisProcessSidAsTheSendersSid`；生成工程 `gift_saga_budget_test.go`。

**测试**：第 4 笔修前红（方案 §13 第 4 笔原文）：

```
TestAnOfflineSendersStepRunsOnTheSidTheyAreBoundTo/debit: the debit of an offline sender bound to this sid was refused: gift saga: player 42 is not served by this process; leaving the step: player owners: this process is not serving the player right now: player 42 has not been taken into service here
TestAdmitRefusesAndHandsOverAForeignStep: handed over to [], want the sender's sid 1000
TestAStepThatNamesNoSenderSidIsRefused: gift.Encode wrote a gift with no sender sid
TestSendGiftWritesThisProcessSidAsTheSendersSid: StartGift got sender 4242 on sid 0, want 4242 on this process's sid 1300
```

（记录另列 `TestAHandoffForAPlayerWeDoNotOwnIsDropped`、`TestAHandoffWhoseEnvelopeDisagreesWithItsPayloadIsRefused` 等红文本；`TestAStepDoesNotClaimASenderBoundElsewhere`、`TestAStepForASenderBeingUnloadedIsRefusedWithoutAHandoff` 在骨架上即绿，作守护。）`ac5acfbe` 修前红：`a refund gets at least 25.75s of retries … = 1m30s; past that the refund is marked manual_required`。`c493a791` 记录未保存修前红文本。真实进程演练 6a / 6b：两个 sid 各 10 个机器人 120 个 saga 全部终结；kill -9 时 7 个在途 saga 全部 compensated、没有 `manual_required`。当前模板上的重跑（v1.23.0，[演练](../../bugfix/REAL-PROCESS-DRILLS-2026-10-06.md) ① ⑥）：6a 两个 sid 各 10 个机器人，40 个赠礼 saga 全部终结（20 completed / 20 compensated，`max_attempt=0`），双方互相转交与执行转交步骤各数十次；6b 单机一轮 kill 时 5 个在途（扣款步骤）、Cluster 两轮各 3 个，接替进程上线后由对端按 `FromSID` 转交、执行转交来的步骤，全部 completed、`manual_required` 0，最后一个在途 saga 在就绪后 0.5～0.6s 终结。与 10-05 的差别（这次全部 completed、10-05 是 compensated）来自 kill 落在不同步骤，两者都是“在途 saga 由新进程接手到终态、无人工介入”。两个 sid 时 loadtest `rc=1` 的旧观察查明是分位数口径（RR-20261006-27，[OPS-8](#ops-8)），修后 `rc=0`。

**源码与记录不一致**：v1.20.0 CHANGELOG 与 App 锁方案 §13 obs34 写“生成的 `saga/gift_item/definition.go` 里 debit `MaxAttempts` 5 → 15”；v1.20.1 起预算由配置提供，当前源码 `demoGiftRefundBudget`（`codegen/internal/roost/demo.go:217`）改写的是配置里的空 `steps: {}` 块。以源码为准；App 锁方案 §13 obs34 第 3 条已加更正注（2026-10-06，本次重核），CHANGELOG 历史段不改。另：v1.23.0 起 `saga.steps` 的类型 / 步骤名只差大小写时报歧义错误（RR-20261006-06，SAGA 主题），生成的 `gift_item` 为小写，不受影响。

**review 检查点**：

1. `runHandoff` 核对顺序：信封 / 载荷一致 → topic 与 phase 匹配 → `AdmitBound` → 认领；确认未知 phase 的拒绝发生在建驻留记录之前。
2. 重试窗口下界计算（15 × 5s + 14 次退避抖动下界 ≈ 98.15s）与 `singleton.startup_wait + ttl + 45s = 90s` 的关系由生成工程 `gift_saga_budget_test.go` 读配置钉住：确认它读的是 `saga.steps.gift_item.debit`，而不是 `definition.go`。
3. 转交失败只记 Warn、靠共享 durable 重投（`:514`）：确认 nak 不会让同一步骤在两个 sid 上同时执行（只有绑定 sid 的 `AdmitBound` 返回 local）。

<a id="own-4"></a>
### OWN-4 activity 改用 App 的 `Live`；候选校验

**提交与首发**：v1.20.0 `f051e24a`（activity 部分）；v1.20.1 `46c4dfba`（RR-20261005-01）。v1.20.2 起候选来源换成组文件（OWN-5）。v1.23.0 `d5682dc4`：核对 RR-20261005-01 的回归在 C4 后的去向，补守卫 `TestAGroupFitsOneLiveQuery`（只加测试与记录）。

**改动文件与关键符号**：`demo/internal/service/game/activity.go.tmpl:66-68`（`liveness app.SingletonLiveness`）、`:96` `startActivity`（`:99-101` 取 `ModSingleton`）、`:331` `expectedGameSIDs`（`Live` 结果为空只等自己；报错本拍不开窗）。已删除：`incarnation`、`lease` / `leaseStanding`、`bindAndLease` 里的 `AcquireLease`、`renewLease`、`leaseNotOurs`、`activity_lease_test.go.tmpl`。

**不变量**：expected 集合 = `Live` 返回的活 sid（用本进程的 `server_type` 查）。守卫：`TestTheExpectedServersAreTheOnesTheAppLockSeesAlive`（`activity_test.go.tmpl:218`）、`TestActivityRefusesToStartWithoutTheAppLockLiveness`（`:261`）。

**RR-20261005-01 的回归在 C4 后的去向**（[修复记录末节](../../bugfix/RR-20261005-01.md)，`d5682dc4` 逐项核对）：C4（`277e1252`）删 `activity.game_sids` 时把 `TestActivityRefusesACandidateListNoWindowCouldOpenWith` 一并删掉；承诺“注定开不出窗口的候选集在启动时、任何远端调用之前按键名拒绝”仍然需要，由组文件兑现：

| 旧子用例 | 现在的覆盖 |
| --- | --- |
| `a-repeated-sid` | 模板 `TestActivityRefusesAGroupNoWindowCouldOpenWith/a-repeated-sid`（`activity_test.go.tmpl:309`）；kit `TestAGroupsFileThatCannotBeUsedIsRefusedByName/repeated-sid`（`kit/service/global/activity/groups_promises_test.go:79`） |
| `a-sid-beyond-int32` | 模板 `…/a-sid-beyond-int32`；kit `…/beyond-int32`、`…/non-positive` |
| `more-candidates-than-one-live-query`（> 200） | 一组至多 `MaxExpectedGames`（64）个成员、加载时拒绝；64 ≤ `app.SingletonLiveMaxSIDs`（200），一组的 `Live` 查询不会因候选过多失败。模板 `…/more-than-the-coordinator-takes`；kit `TestAGroupLargerThanOneWindowIsRefusedWhenLoaded`（`groups_promises_test.go:44`）、`TestModSweepsTheGroupsInTheGroupsFile/an-unusable-file-stops-init`。新守卫 `TestAGroupFitsOneLiveQuery`（`kit/service/global/activity/groups_live_limit_promises_test.go:15`）：两个常量改一个不改另一个时先红 |
| `own-sid-and-non-positive-entries-are-skipped` | 不再适用：本服必须在某个组里（否则启动拒绝），非正数从“跳过”收紧为“拒绝” |
| `exactly-one-live-query-is-accepted` | 恰好 64 个成员的组接受且能开窗：模板 `…/a-full-group-is-accepted`、kit `TestAFullGroupOpensAWindowWithTheCoordinator` |

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

`d5682dc4` 的变异证明（临时改源码，记录原文）：把 `MaxExpectedGames` 改成 201 →

```
an activity group may hold 201 game servers (MaxExpectedGames) but one App.Live query takes at most 200 (app.SingletonLiveMaxSIDs); ...
```

模板侧把 `activityGroup` 挪到协调器能力查找之后，`TestActivityRefusesAGroupNoWindowCouldOpenWith` 的六个拒绝子用例全红（与修前同一形状）；恢复后 `go test -count=1 -race ./internal/service/game/` 通过。

**review 检查点**：

1. `expectedGameSIDs` 用本进程的 `server_type`（App 写进配置、经 game 服务的配置声明读出，`demo/internal/service/game/activity.go.tmpl:104-117`，A4① 起）而不是写死 `"game"`。
2. `Live` 报错时本拍不开窗（不是退化为只等自己）：对照 APP-5 检查点 1。
3. `TestAGroupFitsOneLiveQuery` 只比较两个常量：确认 game-demo 一次 `Live` 查的确实是“整组成员”，而不是组成员之外再加别的 sid（否则组上限不足以保证不超过 200）。

<a id="own-5"></a>
### OWN-5 活动组文件（C4）；协调器按组核对、`groups_file` 必填（RR-20261006-17）

**提交与首发**：v1.20.2 `277e1252`（C4，`cb3e2549` 标注）；v1.23.0 `d5682dc4`（组上限守卫，见 OWN-4）、`055a15d6`（RR-20261006-17：协调器开窗按组文件核对 expected，`543d4287` 标注）、`2c01e06d`（后续：`activity.groups_file` 对协调器必填，`363be382` 标注）、`061cb538`（再后续：`activity.New` 要求 `Groups`、删掉 nil 不核对分支，`068bf5d4` 标注）。A4①（`d1226825`，CFG 主题）之后“必填”由 activity Mod 配置声明的 `required` 报出。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `kit/service/global/activity/groups.go:59` `LoadGroupsFile`、`:81` `ParseGroups`、`:110-112` 64 上限、`:137` `Of`、`:174` `IDs` | 唯一的解析与校验 |
| `kit/service/global/activity/groups.go:153` `Groups.checkExpected` | 协调器一侧的核对：Key 的组必须在文件里，expected 每个 sid 必须是该组成员；不一致包 `ErrInvalid`，点名文件、组、sid 及它实际所属的组（或“不在任何组”），没有定义的组附上全部组 id |
| `kit/service/global/activity/types.go:223` `MaxExpectedGames = 64` | 组上限 = 协调器单窗口 expected 上限，同一个常量 |
| `kit/service/global/activity/service.go:115-122` `Config.Groups`、`:180` `New`（`:202-208` nil 拒绝构造）、`:283` `OpenActivity` 核对 | 核对在形状校验之后、`admitToWindow` 写入之前；sweep 恢复已持久化的开窗计划不再核对 |
| `kit/service/global/activity/activity_mod.go:61` `groups_file` 声明（`required:"true"`，示例 `configs/activity_groups.yaml`）、`:104` `Init`（`:118` 加载、`:124` 交给 Service、`:125-128` `sweep_groups` 为空时取文件里的组）、`:156` `newService` | 协调器读组文件；`Provide` 与测试共用同一份装配 |
| `demo/internal/service/game/activity.go.tmpl:120`、`:180-200` `activityGroup` | game 按本进程 sid 找组；未配文件、不合格都启动报错 |
| `codegen/internal/roost/activity_groups.go:24` | 生成 `configs/activity_groups.yaml`（只创建一次）；协调器配置段的 `groups_file` 由声明快照写出（`codegen/internal/roost/kitconfig_gen.go:29`） |

**不变量**：组文件的规则只有一份，同一文件被协调器与 game 读；协调器接受的每个窗口，expected 都是 Key 所在组的子集（没有绕过核对的构造方式：Mod 缺文件拒绝启动，`New` 缺组拒绝构造）。守卫：`kit/service/global/activity/groups_promises_test.go` 的 `TestAGroupLargerThanOneWindowIsRefusedWhenLoaded`（`:44`）、`TestAFullGroupOpensAWindowWithTheCoordinator`（`:56`）、`TestAGroupsFileThatCannotBeUsedIsRefusedByName`（`:79`）、`TestGroupsAnswerWhichGroupASIDIsIn`（`:111`）、`TestModSweepsTheGroupsInTheGroupsFile`（`:142`）；`TestAGroupFitsOneLiveQuery`（`groups_live_limit_promises_test.go:15`，组上限不超过一次 `Live` 的上限）；`kit/service/global/activity/open_expected_group_promises_test.go` 的 `TestTheCoordinatorRefusesAnExpectedSetTheGroupsFileDoesNotAllow`（`:48`，四种不一致都 `ErrInvalid`、点名文件 / 组 / sid、窗口与活动存储都没有写入）、`TestTheCoordinatorOpensAnyLiveSubsetOfTheGroup`（`:87`）、`TestTheCoordinatorRefusesToStartWithoutAGroupsFile`（`:101`，未设置 / 空白两种）、`TestNewRefusesACoordinatorWithoutGroups`（`:126`）；生成工程 `TestActivityRefusesAGroupNoWindowCouldOpenWith`、`TestAWindowOpensForTheGroupTheFilePutsThisServerIn`（`activity_test.go.tmpl:309` / `:361`，协调器读 game 读的同一个组文件）；codegen `activity_groups_promises_test.go`、`TestActivityConfigCarriesTheTTLItsModRequires`（`codegen/internal/roost/framework_mod_chain_promises_test.go:76`，配置块含 `groups_file: configs/activity_groups.yaml`）。

**控制流**（协调器开窗）：`OpenActivity(key, expected)` → `key.Validate` → `validateExpectedGames`（非空、≤ 64、正数、不重复）→ `Groups.checkExpected(key.GroupID, expected)` → `admitToWindow` 写入。拒绝时什么都没写，sweep 没有 opening 条目可“帮着推进”。

**失败与不确定结果**：核对失败是确定的调用方错误（`ErrInvalid`，RPC 报调用方错误码），game 侧按开窗失败处理（本拍不开，下一拍重试同样被拒，日志点名不一致处）。文件只在启动时读，改组要重启两边。

**测试**：C4 修前红（[C4 方案 §6](../../feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md)原文）：

```
--- FAIL: TestActivityRefusesACandidateListNoWindowCouldOpenWith/more-candidates-than-the-coordinator-takes (0.00s)
    activity_test.go:238: startActivity with activity.game_sids=[2000 2001 2002 2003]: error activity: game: capability "service.global.activity" not found; is the activity process running and reachable over the bus? does not refuse the list by name; every window it would try to open would fail
```

修后 kit `go test -race -count=3` 通过；生成工程 build / vet / test 全绿；`second-game.sh` 的 `SECOND_SID=1005` 启动前退出并点名文件。

RR-20261006-17 修前红（[问题记录](../../bug/RR-20261006-17.md)原文，组 `alliance-a: [1000, 1001, 1002]`、`alliance-b: [2000, 2001]`）：

```text
--- FAIL: TestTheCoordinatorRefusesAnExpectedSetTheGroupsFileDoesNotAllow/a-sid-of-another-group (0.00s)
    open_expected_group_promises_test.go:68: OpenActivity(alliance-a, [1000 2000]) opened [1000 2000]; the groups file does not allow that expected set
--- FAIL: TestTheCoordinatorRefusesAnExpectedSetTheGroupsFileDoesNotAllow/a-sid-in-no-group (0.00s)
    open_expected_group_promises_test.go:68: OpenActivity(alliance-a, [1000 1001 3000]) opened [1000 1001 3000]; the groups file does not allow that expected set
--- FAIL: TestTheCoordinatorRefusesAnExpectedSetTheGroupsFileDoesNotAllow/a-group-not-in-the-file (0.00s)
    open_expected_group_promises_test.go:68: OpenActivity(alliance-c, [1000]) opened [1000]; the groups file does not allow that expected set
--- FAIL: TestTheCoordinatorRefusesAnExpectedSetTheGroupsFileDoesNotAllow/the-whole-other-group-under-this-key (0.00s)
    open_expected_group_promises_test.go:68: OpenActivity(alliance-a, [2000 2001]) opened [2000 2001]; the groups file does not allow that expected set
```

“后续：必填”修前红（[修复记录](../../bugfix/RR-20261006-17.md#后续groups_file-改为必填)原文）：

```text
--- FAIL: TestTheCoordinatorRefusesToStartWithoutAGroupsFile/unset (0.00s)
    open_expected_group_promises_test.go:114: Init accepted a coordinator with no activity.groups_file; it would open windows nothing checks against the groups
--- FAIL: TestTheCoordinatorRefusesToStartWithoutAGroupsFile/blank (0.00s)
    open_expected_group_promises_test.go:114: Init accepted a coordinator with no activity.groups_file; it would open windows nothing checks against the groups
```

“再后续：`New` 要求组”修前红（同记录原文）：

```text
--- FAIL: TestNewRefusesACoordinatorWithoutGroups (0.00s)
    open_expected_group_promises_test.go:135: New accepted a coordinator with no Groups; OpenActivity(no-such-group, [4242]) then returned <nil> without checking the group
```

修后：`go test -race -count=3 ./kit/service/global/activity ./nest` ok；隔离 Redis 上 `-tags integration -run` 跑 `kit/service/integration` 里经 `modConfig` 起 Mod 的用例（必填后 14 条、`New` 要求组后 `TestPerPackageKeyNamespacesDoNotCollide` 与 3 条）通过；`go test -count=1 ./codegen/...` ok；`go generate ./...` 后 porcelain 只有本次改动；重新生成 game-demo（replace 到 worktree）build / vet / test 通过；在生成工程里用临时用例读生成的 `config.activity.yaml` 与 `config.activity.prod.example.yaml` 调 `NewMod(nil).Init`：两份都通过，清空 `groups_file` 后两份都按新错误拒绝（修复记录“后续”）。负对照：`TestTheCoordinatorOpensAnyLiveSubsetOfTheGroup` 修前修后都绿（子集照常开窗）。

**源码与记录不一致（以源码为准）**：RR-20261006-17 修复记录“后续”写缺文件时的错误是 `activity mod: activity.groups_file is required: set it to …`（`generatedGroupsFile` 常量）；A4① 之后必填由声明的 `required:"true"` 报出，文本是 `config: activity.groups_file is required (for example configs/activity_groups.yaml)`（`internal/configschema/check.go:227`），仍点名键名与默认路径，守卫断言的子串不变。

**兼容**：破坏性，见说明分册 OWN-5“兼容与迁移”；打 v1.23.0 tag 时生成器的 Core 下限要提到 v1.23.0（`codegen/internal/roost/manifest.go:83` 现为 v1.21.0，`.github/workflows/framework-compat.yml:50` 同步）：生成的 game 测试用了 `activity.Config.Groups`，对 v1.22.0 编译报 `unknown field Groups`（发版步骤）。

**review 检查点**：

1. `ParseGroups` 拒绝未知字段（`game_sid` 拼错不会被当成空）：看 YAML 解码是否开了严格模式。
2. 组 id 规则与 `Key.Validate` 同（非空、不含 `/`）：两处是否引用同一个检查。
3. 协调器显式 `sweep_groups` 仍优先：多副本分担时文件里的组与 `sweep_groups` 不一致不会报错，确认这是有意的。
4. `OpenActivity` 的核对在 `admitToWindow` 之前（`service.go:283`）：确认被拒的开窗不留 opening 条目、活动存储也没有写入；sweep 恢复已持久化的开窗计划时不再核对，确认它读的是准入时已核对过的计划。
5. “没有绕过核对的构造方式”：`git grep -n 'activity.New(\|activity\.Config{' -- '*.go' '*.tmpl'` 与包内 `New(Config{` 的调用点逐个看是否都带 `Groups`；`Config.Groups` 为 nil 的唯一结局是 `New` 报错。
6. 空白的 `groups_file`（`"  "`）按缺失拒绝（`TestTheCoordinatorRefusesToStartWithoutAGroupsFile/blank`）：确认声明层的 `required` 对只有空白的字符串也判缺失，而不是交给 `LoadGroupsFile` 去打开一个空白路径。

<a id="own-6"></a>
### OWN-6 global `Bind` 重试幂等

**提交与首发**：v1.23.0 `611d5d72`（收尾第 4 批 A7）；`ba13cb05` 在真实 Redis 用例里补同参数重试断言。

**改动文件与关键符号**：`kit/service/global/service.go:55-95` `Bind`：`Create` 返回 `!created` 时 `Get`；`found` 且 group 与 globalSID 一致 → `Replayed("bind")` 返回已存绑定；否则 `Conflict("bind")` 并返回带已存 group / sid 的 `ErrConflict`；`Get` 出错原样返回。

**不变量**：只有“已存的绑定指向别处”才是冲突。守卫：`TestBindRetriedAfterUnknownOutcomeReturnsTheSameBinding`（`kit/service/global/bind_retry_promises_test.go:33`，同时钉住换 group、换 globalSID 两种仍报冲突、`conflict:bind` 计 2、库里绑定不变）；`kit/service/integration` `TestGlobalRunsOnRedis`（`kit/service/integration/redis_test.go:502`，真实 Redis；`ba13cb05` 在 `:517-525` 补同参数重试返回同一绑定、换 group 仍 `ErrConflict` 两条断言）。

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

<a id="own-7"></a>
### OWN-7 通关遇到还没开的窗口时自己开窗（RR-20261006-29）

**提交与首发**：v1.23.0 `3b06c66c`（与 RR-20261006-27 / -28 同提交，真实进程演练 ⑥ 发现）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `demo/internal/service/game/activity.go.tmpl:390` `ActivityRunner.Contribute`（`:399-405`） | `ApplyProgress` 得到 `CodeMissing` 且 `flags.Activity` 开着时调 `openWindow`，成功后再 `ApplyProgress` 一次；开窗失败返回原错误并附上开窗的原因 |
| `demo/internal/service/game/activity.go.tmpl:306` `openWindow` | 与循环共用：按 `Live` 算 expected、`OpenActivity`，已开（`CodeExists`）视为成功；不调 `ArmActivity`（对 World 的 Nest 同步调用） |
| `demo/internal/service/game/activity.go.tmpl:42` `activityLoopInterval = 5s`、`:251` `runActivity`、`:272` `openCurrentWindow` | 循环照旧每 5s 开当前窗口并装上关窗截止时间；对已由通关开出的窗口得到 `CodeExists`、照常 arm |

**不变量**：窗口 id 只由业务时钟决定，“开窗”只是让协调方知道它存在；开窗入口只有 `openWindow` 一处（循环与通关共用），同一 Key 的开窗幂等；Activity 开关关着时任何入口都不开新窗口。守卫：`TestAClearInTheFirstSecondsOfAWindowStillCounts`（`demo/internal/service/game/activity_test.go.tmpl:148`，`activity-on`：协调方只接受已开窗口的进度、上一个窗口开着、时钟进入下一个窗口 1s，通关记进时钟的窗口且只开了这一个窗口；`activity-off-opens-nothing`：开关关着时仍答 not found、什么都不开）；`TestAContributionSaysWhichWindowTookIt`（`:63`）不变。

**控制流**：`Contribute` → 按时钟算 `activityID` → `ApplyProgress` → `CodeMissing` 且开关开 → `openWindow(activityID)`（`Live` → `OpenActivity`，`CodeExists` 视为成功；受协调器的组核对约束，OWN-5）→ 再 `ApplyProgress` → 写贡献榜。关窗截止由循环下一轮 `openCurrentWindow` 装上。

**失败与不确定结果**：开窗失败（`Live` 查询失败、协调器拒绝）时返回原错误并附上原因，调用方（`finish_dungeon`）与以前一样只丢这一点、记 Warn；第二次 `ApplyProgress` 的结果未知按它自己的错误返回（进度请求带 `ProgressRequestID`，重放幂等）。

**测试**：修前红（[问题记录](../../bug/RR-20261006-29.md)原文，生成工程里 `GOWORK=off go test -count=1 -run TestAClearInTheFirstSecondsOfAWindowStillCounts ./internal/service/game/`）：

```
--- FAIL: TestAClearInTheFirstSecondsOfAWindowStillCounts (0.00s)
    --- FAIL: TestAClearInTheFirstSecondsOfAWindowStillCounts/activity-on (0.00s)
        activity_test.go:168: a clear 1s into window race-1791299400 was refused: activity: not found
```

修后生成工程 `go test ./internal/service/game/` 通过（含开关关着不开窗的子用例）；`go test ./codegen/...`、重新生成 game-demo 的 build / vet / test 通过。真实进程（[演练 ⑥](../../bugfix/REAL-PROCESS-DRILLS-2026-10-06.md) 第一轮修前、第二轮修后）：修前起跑恰在 23:05:00 前，`clear not contributed to the activity … activity: not found` 5 条（23:05:03.2～04.0），循环 23:05:04.08 才开出窗口，loadtest A `success=4 failure=6`；修后起跑对齐到 23:15:00 前 1.4s，协调方窗口 `race-1791299700` 的 `opened_at_unix=1791299701`（由通关开出），game 的循环 23:15:03.6 才轮到它，A / A2 全部成功、没有 `clear not contributed`。

**review 检查点**：

1. `Contribute` 在玩家 handler 上调 `openWindow`：确认 `openWindow` 里只有对协调方与 App `Live` 的远端调用、没有对 World 的 Nest 同步调用（`ArmActivity` 留在循环里），不会形成嵌套派发。
2. 两个 game 同时在缺口里通关、各自开同一个窗口：确认 `OpenActivity` 对同一 Key 幂等返回 `CodeExists`，且两次 expected 不同（各自的 `Live` 结果不同）时协调方保留先写的那份（先写者赢，OWN-4 已知限制同理）。
3. 第二次 `ApplyProgress` 与第一次用同一个 `ProgressRequestID`：确认第一次答 `CodeMissing` 时协调方确实没有记下这次进度（不会被第二次当成重放而少记，也不会多记）。

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
| `app/app.go:188` | `clock.SetOffset(a.settings.Time.LogicOffset)`，`fctx.Now` 读它（`a.settings` 是 `loadServiceConfig` 按声明读出的配置） |
| `app/registry.go:43` | `NewRegistry` 登记 `ModBusinessClock` = `clock.NewBusiness(offset)`；与 `app/app.go:188` 读同一个键（同一份声明 `timeConfig`，`app/config_schema.go:224`） |
| `app/business_clock.go:9` `ModBusinessClock`、`:13` `logicOffsetKey`、`:21` `BusinessClock`；`app/config_schema.go:234` `appConfig.ValidateConfig`（`:242-244`） | 能力名、唯一配置键、取钟；生产非 0 拒绝。A4① 起生产检查在 App 的配置声明里（原 `validateProductionLogicOffset` 删除，错误文本不变） |
| `kit/service/global/activity/activity_mod.go:143`、`kit/service/mail/mail_mod.go:94`、`kit/service/rank/rank_mod.go:94`、`kit/service/session/session_mod.go:106` | Mod 给服务的 `Config.Now` 注入 `app.BusinessClock(r).Now` |
| `timer/scheduler.go:131`、`ai/controller.go:242`、`actionflow/mission_runner.go:436`、`actionflow/action_runner.go:698` | 未注入时缺省 `clock.Now()`（原 `time.Now()`） |
| `cmd/glsvet/clockhints.go:21-32`、`cmd/glsvet/main.go:15-18` | `-clockhints`（缺省开）、`-businessdirs`（缺省 `game`）、豁免指令 `glsvet:system-clock`；业务目录按模块根（向上找 `go.mod`）之下的路径判断 |
| `demo/internal/service/game/activity.go.tmpl:154`、`gm.go.tmpl:157`、`spawner.go.tmpl:64`、`purchase_drain.go.tmpl:72`、`demo/game/controllers/player/controller.go.tmpl:487-489` | game-demo 业务时间读业务钟（`tickWorld` 拆出 `tickWorldOnce`，玩家控制器加 `BusinessNow()`） |

**不变量**：偏移只有一个配置来源、只在启动时生效；Nest / DataEngine / Sync 一行不改（全是系统时钟）。守卫：`TestProductionRefusesANonZeroLogicOffset`（`app/logic_offset_production_promises_test.go:12`）、`TestBusinessClockFollowsTheConfiguredOffset` / `TestOffsetMovesBusinessTimeButNotTheSingletonLease`（`app/business_clock_promises_test.go:22` / `:42`）、`TestTheModWiresTheCoordinatorToTheBusinessClock`（`kit/service/global/activity/business_clock_promises_test.go:16`）、`TestTheKeyTTLComesFromTheInjectedClock`（`service/mail/redis_store_test.go:361`）、`TestClockHints*`（`cmd/glsvet/clockhints_test.go:31-133`）；生成工程 `TestTheWindowFollowsTheBusinessClock` / `TestTheWorldTickCarriesBusinessTime`（`activity_test.go.tmpl:467` / `:492`）。

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

1. `app/app.go:188` 与 `app/registry.go:43` 两处读同一个键、各自建钟：确认测试里 `registry.Register(app.ModBusinessClock, …)` 替换不会让 `fctx.Now` 与 Registry 的钟分叉（方案 §2 写明测试要在 cfg 里写偏移）。
2. `git grep -n 'time.Now()' -- 'kit/service/**/*.go' 'demo/**/*.tmpl'` 的非测试结果逐个归到系统钟用途（租约、TTL、审计）；`game` 目录外的业务包 glsvet 不会提示，靠 review。
3. 生产校验 `isProductionServiceConfig` 认 `env` / `app.env` / `environment` 三个键的 prod / production：确认生成的生产示例写的是其中之一。

<a id="clk-2"></a>
### CLK-2 match / chat 展示 / account 换钟；doctor 偏移一致（D-L3 第八轮）

**提交与首发**：v1.21.0 `fa472ee7`（`d73289b5` 记录决定、`5505db4b` 标注）。

**改动文件与关键符号**：`kit/service/match/match_mod.go:125`（注入业务钟）；`demo/internal/service/game/matchmaker.go.tmpl:82`（`matchmaking.Pools` 读业务钟，去掉 `//glsvet:system-clock`）；`kit/service/chat/chat.go:487-502`（`Message.SentAtUnix`，`json:"sent_at_unix,omitempty"`）、`kit/service/chat/chat_mod.go:169`（`Now` 业务钟、`SystemNow: time.Now`）；`kit/service/account/service.go:57-64`（`Config.SystemNow` 说明）、`:149-150`（nil 沿用 `Now`）、`:292`（建会话：业务钟时间 + 系统钟签发）、`:317`（`VerifySessionToken` 用系统钟）、`:404`；`kit/service/account/admin.go:146`（运维时间系统钟）；`kit/service/account/account_mod.go:151`；`codegen/internal/roost/logic_offset_doctor.go:37` `checkLogicOffsets`、`:97` `configLogicOffset`（按 `app.ConfigDuration` 的读法解析）；`codegen/internal/roost/render_docs.go:238`（生成工程说明的时间规则）。

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
3. glsvet 生成工程里剩下的 5 处 `//glsvet:system-clock` 豁免（当前源码：`demo/game/controllers/player/purchase.go.tmpl:63` 支付时间、`start_gift.go.tmpl:48` 与 `gift_saga.go.tmpl:369` saga 截止、`playerowner.go.tmpl:124` / `:130` 驻留与闲置卸载）理由是否都属于系统用途（后两处是闲置判定，豁免注释写 “residency and idle unload are leases”）。D-L3 §3.2 的表原写“玩家归属租约”，已按源码更正为“驻留与闲置卸载”（2026-10-06，本次重核）。

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
| `app/app.go:294-301` | 挂点：单实例锁之后、`sortMods` 与任何 Mod Init 之前；失败时 `singletonReleasable = true` |
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

**设计范围**：没开锁、偏移为 0 的进程不检查（方案写明）。专门的真实依赖用例只在单机 Redis 上（`TestBusinessTimeHighWaterMarkOnRealRedis`；高水位是单键，不涉及跨槽）。**未验证（外部环境）**：多机 Cluster 切主（E08）。

**review 检查点**：

1. `check` 失败时 `run` 置 `singletonReleasable = true`（`app/app.go:299`）：此时还没有任何 Mod 启动，释放是安全的；确认 `businessTime` 为 nil 时 `defer businessTime.stop()` 不会被登记（`:301` 在错误返回之后）。
2. `advance` 的 `inspect` 只在第一次读到已有值时调用（`inspect = nil`）：确认被抢先重来时不会因为别的进程刚写的更大值而误判回退（回退只与第一次读到的 H 比）。
3. 共用锁的 store 时 `closeStore` 不关（`ownStore=false`）：确认 store 的唯一关闭方是 `singleton.finish`。
4. `advanceLoop` 用 `time.NewTicker` 的系统时间间隔、写入的值是业务时间：两者混用是有意的（间隔是系统用途），确认 `g.now` 是 `BusinessClock(a.registry).Now`。

<a id="clk-4"></a>
### CLK-4 activity / mail 回到业务钟

**提交与首发**：拆分 v1.21.0（`b9fc5342` mail；`5a3c4a60` activity，`3d3b0c09` 记录提交号）；合并与删除 v1.22.0 `3e77beb9`。

**改动文件与关键符号**（当前源码）：`kit/service/global/activity/service.go:1489`（创建派发 `NextAttemptAtUnix: nowUnix`，业务钟）、`:1661`（退避 `now.Add(s.dispatchBackoff(...))`）、`kit/service/global/activity/admin.go:105` `ReopenDispatch`、`:144`（重开写业务钟当前时间）；`service/mail/service.go:863`（`ClaimDeadlineUnix = nowUnix + ClaimLease`，业务钟）；`service/mail/redis_store.go:59` `EnvelopeStorageGrace = 24h`；`kit/service/mail/mail_mod.go:94`（只注入业务钟）。已删除：`activity.Config.SystemNow`、`mail.Config.SystemNow`、`mail.RedisConfig.StorageGrace`、`kit/service/global/activity/system_clock_promises_test.go`。

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

**改动文件与关键符号**：`app/business_time.go:51` `businessTimeAdvanceFailedMetric = "app.business_time.advance_failed.total"`；`:108` 取本 App 的 `*metrics.Registry`（`ModMetrics`）；`:218-222` 推进失败且 ctx 未结束时 `IncCounter` 并 Warn。`metrics.Registry.IncCounter` 对 nil 接收者安全（`metrics/metrics.go:277-279`）。

**守卫**：`TestAFailedHighWaterMarkAdvanceIsCounted`（`app/business_time_promises_test.go:251`：写失败期间计数增长，恢复后高水位继续推进、计数不再增长）。

**测试**：修前红（第十二轮 kit 批 §8 原文）：`business_time_promises_test.go:278: advance failures counted 0 while every write of the high-water mark failed, want them counted`。修后 `go test -race -count=3 ./app` 通过。

**review 检查点**：停机时 `ctx` 被取消导致的推进失败不计数（`ctx.Err() == nil` 条件）：确认停机不会产生一次误报。

<a id="clk-6"></a>
### CLK-6 timer 同期限顺序与 priority、未注册类型（D-L1 / D-L2）

**提交与首发**：v1.21.0 `5abae51e`（`e320578c` 标注）。

**改动文件与关键符号**：`timer/scheduler.go:3-6`（包注释：触发顺序）、`:29-31` `UnhandledDroppedMetric`、`:43-45` `Node.Priority`、`:142` `NewTimer` = priority 0、`:148` `NewTimerWithPriority`、`:165` `ReportUnhandledTypes`、`:305` 删除无 handler 节点时计数、`:405-414` `timerHeap.Less`（End → Priority → ID）；`demo/db/def/world.go.tmpl:46-51`（`TimerNode.Priority`，`bson:"priority"`，旧文档按 0 读回）；`demo/game/entities/world/timer_component.go.tmpl:115`（`OnInitFinish` 调 `ReportUnhandledTypes`，从 DAO 建一次调度器）。

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

**改动文件与关键符号**：`servicemetrics/metrics_reporter.go:12-23`（六个指标名）、`:39` `NewMetricsReporter`（每次上报解析当时的默认注册表）；`servicemetrics/servicemetrics.go:47` `KeyedReporter`；`kit/mods/service_servicemods.go:66-68` `ServiceMetricsConfig`（`service_metrics.enabled` 的声明，缺省 true，严格布尔）、`:72` `ApplyServiceMetrics`，各服务 Mod 的配置声明嵌入它（A4① 起；原 `ServiceMetrics` / `ServiceMetricsEnabledKey` 与 `app/config_validation.go` 的严格布尔清单删除，CFG 主题）；`codegen/internal/roost/framework_services.go:358-360`（生成的 `Metrics()`）；`service/match/queue_store.go`、`kit/service/rank/redis_store.go`、`service/session/service.go` 三个调用点（`DepthOf` 与 `Dropped("run.swept", n)`）。

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
### OPS-2 `metrics.DeleteSeries`；loadtest 运行与 nest 派发器的序列；可靠 RPC 的 method 标签有界

**提交与首发**：v1.23.0 `7b73aabc`（第十二轮 kit 批 §2：`DeleteSeries` 与 loadtest）；`055a15d6`（RR-20261006-18 派发器序列、RR-20261006-19 method 标签有界，`543d4287` 标注）；`2c01e06d`（RR-18 补测：`slow_reroute.total` 随派发器删除，只加测试）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `metrics/metrics.go:511` `Registry.DeleteSeries`、`:544` `SeriesCount`、`:243` 包级 `DeleteSeries` | 空 `match` 不删；`name` 空表示任何名字；四种类型都删；归还 `r.series[name]` 名额 |
| `robot/loadtest/manager.go:621` `appendHistoryLocked`、`:635` `runIDInUseLocked` | 挤出历史时 `metrics.DeleteSeries("", {"run": RunID})`；同名 RunID 在跑或在历史里不删 |
| `nest/dispatcher_series.go:25` `dispatcherSeriesNames`、`:41` `holdSeries`、`:57` `releaseSeries`、`:75` `reportSeries` | 按派发器名字计数的登记表（包级锁）；最后一个撤销时对 5 个指标名 `DeleteSeries(name, {dispatcher: <名字>})`；上报前确认仍持有 |
| `nest/dispatcher.go:117` `OnInit`（`:140` 登记）、`:167` `OnDestroyWithContext`（`:218-223`：`queue.stop(ctx)` 返回 nil 后才撤销）、`:309` `observeStats`（`:317` 经 `reportSeries` 上报） | 派发器生命周期挂点 |
| `bus/jetstream_rpc.go:668-690`（有界证明注释与常量 `jetStreamRPCMethodLabelLimit = 256`、`_other`、`_unregistered`）、`:693` `jetStreamRPCCallerMethodLabel`、`:331` 调用方取标签、`:451-453` 被调方按注册与否定标签 | 调用方 `sync.Map` 快路径，首次出现时在锁下计数；被调方在读 envelope 后先查注册表（handler 查找挪到这里，同一次读） |

**不变量**：删除后序列不在 Snapshot / `/metrics` 里、名额归还；同名同标签再写入是新序列。派发器序列只在“以这个名字运行的最后一个派发器”排空之后删除，停机超时不删；与撤销并发的上报不会把序列建回来。可靠 RPC 的 method 标签基数有固定上界：被调方 = 注册方法数 + 1，调用方每个 Bus ≤ 257，与对端发什么、调用方传什么无关；调用本身（`MsgName` / subject）不变。守卫：`TestDeleteSeriesRemovesEveryKindByLabelAndReturnsTheQuota`（`metrics/delete_series_promises_test.go:11`）、`TestRunSeriesLeaveTheRegistryWithTheRunRecord`（`robot/loadtest/run_series_lifecycle_promises_test.go:21`，`HistoryLimit=2` 连跑 6 次）；`nest/dispatcher_series_lifecycle_promises_test.go` 的 `TestDestroyedDispatchersLeaveNoSeries`（`:43`，20 轮每轮剩余 0 条、`/metrics` 文本里没有已销毁的名字）、`TestADispatcherSharingItsNameKeepsTheSeries`（`:75`）、`TestADispatcherThatDidNotDrainKeepsItsSeriesUntilItDoes`（`:96`）、`TestTheSlowRerouteSeriesGoesWithItsDispatcher`（`:149`，真实 NestMgr 触发冷目标改道后销毁）；`bus/rpc_method_label_bound_promises_test.go` 的 `TestCallerRPCMethodLabelsAreBounded`（`:35`，300 个方法：两个指标与 `pendingByMethod` 都 ≤ 257，前面的方法保留自己的标签，`_other` 的 ok 计数 = 44）、`TestServedRPCMethodLabelsAreTheRegisteredMethods`（`:79`，300 个伪造方法：两个指标只有 `mail.List` 与 `_unregistered`，`no_handler` 计 300）。

**测试**：loadtest 修前红（第十二轮 kit 批 §2 原文）：

```
run_series_lifecycle_promises_test.go:66: series count kept growing across runs: map[1:7 2:11 3:15 4:19 5:23 6:27] (history limit 2)
```

`metrics` 包的用例是新 API，记录未保存修前红文本。修后 `go test -race -count=3 ./metrics ./robot/loadtest` 通过。

RR-20261006-18 修前红（[问题记录](../../bug/RR-20261006-18.md)原文）：

```text
--- FAIL: TestDestroyedDispatchersLeaveNoSeries (0.01s)
    dispatcher_series_lifecycle_promises_test.go:64: series of destroyed dispatchers after each create/destroy round: [6 12 18 24 30 36 42 48 54 60 66 72 78 84 90 96 102 108 114 120]; want 0 every round
--- FAIL: TestADispatcherSharingItsNameKeepsTheSeries (0.00s)
    dispatcher_series_lifecycle_promises_test.go:89: the last dispatcher named rr18-series-shared is destroyed and 6 series remain
--- FAIL: TestADispatcherThatDidNotDrainKeepsItsSeriesUntilItDoes (0.00s)
    dispatcher_series_lifecycle_promises_test.go:125: the drained dispatcher still has 6 series
```

`slow_reroute.total` 补测的变异证明（[修复记录“补测”](../../bugfix/RR-20261006-18.md)原文，临时从 `dispatcherSeriesNames` 删掉这个名字，不提交）：

```text
--- FAIL: TestTheSlowRerouteSeriesGoesWithItsDispatcher (0.00s)
    dispatcher_series_lifecycle_promises_test.go:214: the dispatcher rr18-series-reroute is shut down and nest.dispatch.slow_reroute.total still reports 1
```

RR-20261006-19 修前红（[问题记录](../../bug/RR-20261006-19.md)原文；修前用一个临时测试文件提供新常量，跑完删除）：

```text
--- FAIL: TestCallerRPCMethodLabelsAreBounded (0.01s)
    rpc_method_label_bound_promises_test.go:63: bus_rpc_pending has 300 method labels after 300 distinct methods; the bound is 257
--- FAIL: TestServedRPCMethodLabelsAreTheRegisteredMethods (0.01s)
    rpc_method_label_bound_promises_test.go:116: bus_rpc_request_total has 301 method labels (e.g. "forged.M119") after 300 requests naming unregistered methods; want only map[_unregistered:true mail.List:true]
```

修后：`go test -race -count=3 ./nest ./bus` ok；`go test -count=1 -shuffle=1791263156350214000 ./nest` ok；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 退出 0；原有 `TestDispatcherObserveStatsRecordsQueueGauge` 与 JetStream RPC 用例（按方法的 pending、超时清零、投递次数）照常。

**范围说明**：同进程多个 Bus 时各自 256（标签不带 Bus 身份，同名方法写同一序列；修前即如此，生产每进程一个 Bus）。不在空闲时删 `bus_rpc_pending{method}`：`DeleteSeries` 在注册表写锁下扫全表，method 本身没有生命周期终点（修复记录“决策”）。

**review 检查点**：

1. loadtest 用包级 `metrics.DeleteSeries`（默认注册表）：确认 `robot.runner.*` 也写在默认注册表，而不是某个注入的注册表。
2. `DeleteSeries("", {"run": id})` 删除任何带该 `run` 标签的序列（不限 `robot.runner.*`）：确认没有别的子系统用 `run` 作标签且不希望被删。
3. `DeleteSeries` 持写锁遍历全部序列（O(序列数)）：确认只在运行挤出历史、最后一个同名派发器排空时各调用一次，不在热路径。
4. `dispatcherSeriesNames` 是手写清单（`nest/dispatcher_series.go:23-31` 注释要求新增带 `dispatcher` 标签的指标加进来）：`git grep -n '"dispatcher"' -- nest` 列出全部写 `dispatcher` 标签的点，确认都在清单里。
5. `OnDestroyWithContext` 停机超时返回时不撤销（`nest/dispatcher.go:218-222`）：确认之后的重试排空成功会走到 `releaseSeries`，且 `holdSeries` 对同一派发器第二次调用是空操作（不重复计数）。
6. 被调方的标签取自 `b.rpcHandlers`（`bus/jetstream_rpc.go:446-453`）：确认读注册表与 `HandleRpc` 注册之间的同步，与原来 handler 查找的同步相同（查找只是挪了位置）。

<a id="ops-3"></a>
### OPS-3 Ops 同步 bind 与 admin 命令期限

**提交与首发**：v1.21.0 `2c1c7be7`（NC-230 与 N02 O1，`897a1dd9` 标注）。

**改动文件与关键符号**：`kit/ops/ops_mod.go:31-37`（`defaultAdminTimeout = 10s`、`adminWriteMargin = 5s`、`defaultWriteTimeout = 15s`）、`:75` `ops.admin_timeout` 的声明（缺省 10s、`min:"1ns"`，写了就必须为正）、`:120` `Init` 按声明读、`:177-216` `Start`（`:194` 写超时 = max(15s, admin_timeout + 5s)；`:201` `net.Listen`；`:205-213` 登记 server 与实际地址、goroutine 里 `server.Serve(listener)`）、`:229` `commandTimeout`、`:389-398`（命令因期限返回 `context.DeadlineExceeded` → 504 + `effects are unknown`）。A4① 起 `ops.admin_timeout` 的严格读取由 OpsMod 自己的声明完成（原 `app/config_validation.go` 的严格时长键清单删除，CFG 主题）。

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

**未验证**：真实 shell / systemd 部署下端口冲突的完整进程链（E21）。

**review 检查点**：

1. `Start` 持 `serverMu` 做 `net.Listen`（`kit/ops/ops_mod.go:181-182` 加锁并 defer 解锁，`:201` bind）：bind 是本地系统调用、不等待网络，确认不会长时间持锁挡住并发的 `StopWithContext`（`TestConcurrentOpsStartStopUsesCapturedServer` 覆盖）。
2. `Shutdown` 早于 `Serve` 登记 listener 的交错：注释写由 `Serve` 关闭 listener；确认 `Start` 返回后立即 `Stop` 不会泄漏 listener。
3. 504 只在 `errors.Is(err, context.DeadlineExceeded)` 时返回：命令自己包装了别的错误时回 400，确认这仍被视为“结果未知”在文档里写清。

<a id="ops-4"></a>
### OPS-4 Ops 的 `Authorization` 必须带 `Bearer `

**提交与首发**：v1.23.0 `7b73aabc`（第十二轮 kit 批 §4）。

**改动文件与关键符号**：`kit/ops/ops_mod.go:443` `bearerToken`（没有大小写不敏感的 `bearer ` 前缀返回空串）、`:425` `authorized`（`X-Admin-Token` 或 Bearer，`:455` `secretEqual` 常量时间比较；空串永远不等于 token）。

**守卫**：`TestOpsAdminAuthorizationAcceptsOnlyTheExactToken`（`kit/ops/ops_mod_test.go:115`，“bare authorization”移到拒绝组，加“padded bare token”）、`TestOpsAdminAuthorizationRejectsEverythingWithoutAConfiguredToken`（`:146`）。

**测试**：修前红（第十二轮 kit 批 §4 原文）：`ops_mod_test.go:139: bare authorization: authorized when it must not be`。修后通过。

**review 检查点**：`authorized` 先判 `adminToken == ""` 一律拒绝（`kit/ops/ops_mod.go:426-428`，已核对），所以 `bearerToken` 返回的空串不可能与空 token 相等；确认没有别的入口（如 admin gateway 转发）绕过 `authorized` 自己解析 `Authorization`。

<a id="ops-5"></a>
### OPS-5 CAS 冲突统一计数

**提交与首发**：v1.23.0 `7b73aabc`（第十二轮 kit 批 §5）。

**改动文件与关键符号**：`versionstore/versionstore.go:164-168`（`MetricCompareAndSet`、`MetricConflict`）、`:173` `CountCompareAndSet`、`:182` `CountConflict`；`versionstore/redis_store.go:391`（`Update` 每次 CAS 结算后计数；A2③ 起回复丢失先按写令牌核对再定输赢，属 DRV 主题）、`:408`（预算用尽计 conflict）；`kit/service/rank/redis_store.go:180` / `:192` / `:211`（rank 自己的 CAS 循环，store = `<prefix>:o:`）；`kit/service/chat/store.go:495`（注释：`ErrConflict` 不在这里计数）；`kit/service/README.md`（`servicemetrics.Conflict` 与存储竞争的分工）。

**不变量**：存储竞争只在 versionstore 一处计数；`servicemetrics.Conflict` 只留给业务冲突。守卫：`TestUpdateCountsEveryCompareAndSetAndTheExhaustedConflict`（`versionstore/cas_metrics_promises_test.go:20`：两次成功、一次 3 次全输 → applied=2、lost=3、conflict=1；不保存不计数）；rank `TestSubmitAndPageReportWhatTheyDid`（lost = 8、conflict = 1、`conflict:submit` = 0）；chat 两条改为断言不再自报。

**测试**：新用例修前不能编译（新 API），记录未保存修前红文本。修后 `go test -race -count=3 ./versionstore ./kit/service/rank ./kit/service/chat` 通过。

**review 检查点**：

1. `store` 标签取键前缀（`RedisConfig.Prefix`）：确认每个存储一个固定值、没有把带业务 ID 的前缀传进来（低基数）。
2. account / activity 等直接用 `versionstore.RedisStore` 的服务自动获得计数（计数在 versionstore 一处、有用例，第十二轮 kit 批没有逐服务补用例）：抽查一个服务的 `Update` 调用确实经过 `redis_store.go:391`。

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

**改动文件与关键符号**：`robot/runner/runner.go:273-341` `runPopulation`：`launched`（在线目标数，缩容减回去）与 `lastOrdinal`（已发出的最大序号，只增）分开；扩容时先 `lastOrdinal++` 再 `index := lastOrdinal`（`:289-290`）；`IdentityProvider` 注释写明序号可能超过 `Count`。

**守卫**：`TestStageRegrowDoesNotReuseOrdinals`（`robot/runner/stage_ordinal_promises_test.go:17`）；原有 `TestStagedRampUpAndDown`（6 → 1 的在线数）不变。

**测试**：修前红（[问题记录](../../bug/RR-20261006-09.md)原文）：

```
--- FAIL: TestStageRegrowDoesNotReuseOrdinals (0.06s)
    stage_ordinal_promises_test.go:48: robot ordinal 2 was handed out twice across the stages (launch order [1 2 3 2 3]); a regrown stage must not reuse the ordinal (and player id) of a robot it just stopped
```

修后 `go test -race -count=3 ./robot/runner` 通过（序号 `[1 2 3 4 5]`）。

**review 检查点**：`shrinkTo` 只减 `launched`、不动 `lastOrdinal`：确认缩容停掉的机器人的 stop 通道按“末尾”关闭，与新序号的分配互不影响。

<a id="ops-8"></a>
### OPS-8 直方图分位数收在观测范围内；loadtest 阈值失败写明原因（RR-20261006-27）

**提交与首发**：v1.23.0 `3b06c66c`（与 RR-20261006-28 / -29 同提交，真实进程演练 ⑤）。

**改动文件与关键符号**：

| 位置 | 职责 |
| --- | --- |
| `metrics/metrics.go:83-86` `histogram.minNanos` / `maxNanos`、`:92` `newHistogram`（最小值初始化为 `math.MaxInt64`）、`:98` `observe`（先 CAS 更新最小 / 最大、再记桶、最后加计数） | 读到计数的读者一定看得到它拓宽的范围 |
| `metrics/metrics.go:133` `histogram.quantile`（`:138` 读范围、`:164-165` 溢出侧在 `[最大桶上界, 最大值]` 里插值） | 插值区间收窄到 `[max(桶下界, 最小值), min(桶上界, 最大值)]`；`:228` 包级 / `:334` `Registry.HistogramQuantile` 经它 |
| `robot/loadtest/manager.go:158` `ThresholdResult`（`:165` `Samples`，JSON `samples`）、`:537` `evaluate`（`:551-573` 分位数的样本数取成功场景数与直方图样本数的较小值）、`:582` `describeViolations`、`:500` 阈值失败写 `rec.Error`、`:530` `run done` 日志的 `verdict` | 阈值判定按设计（`actual > max`），输入改为收窄后的估计；失败说明点名阈值、实际值、上限与样本数 |
| `demo/cmd/loadtest/main.go.tmpl:69`（`-max-p95` 说明改为实际依据，阈值 16 不变）、`:151-153`（退出错误带上 `snapshot.Error`） | 生成工程的 loadtest |

**不变量**：任何分位数估计都在观测到的 `[最小, 最大]` 里（真值一定在这个区间，收窄只减小误差）；桶计数、`HistogramCount`、Prometheus `_bucket` 导出不变；阈值失败的运行 `Error` 非空。守卫：`metrics/histogram_quantile_bounds_promises_test.go` 的 `TestHistogramQuantileNeverExceedsTheSlowestObservation`（`:18`）、`TestHistogramQuantileInTheOverflowIsNotUnderstated`（`:42`）、`TestHistogramQuantileOfIdenticalSamplesIsThatSample`（`:52`）；`robot/loadtest/threshold_verdict_promises_test.go` 的 `TestQuantileThresholdJudgesTheObservedCostsNotABucketBound`（`:68`）、`TestThresholdFailureNamesTheThresholdAndTheActualValue`（`:79`）。

**测试**：修前红（[问题记录](../../bug/RR-20261006-27.md)原文；loadtest 第二条用例暂时去掉新字段 `Samples` 的断言）：

```
--- FAIL: TestHistogramQuantileNeverExceedsTheSlowestObservation (0.00s)
    histogram_quantile_bounds_promises_test.go:33: q0.50 = 12.288s, outside the observed range [8.3s, 9.6s]
    histogram_quantile_bounds_promises_test.go:33: q0.90 = 15.5648s, outside the observed range [8.3s, 9.6s]
    histogram_quantile_bounds_promises_test.go:33: q0.95 = 16.384s, outside the observed range [8.3s, 9.6s]
    histogram_quantile_bounds_promises_test.go:33: q0.99 = 16.384s, outside the observed range [8.3s, 9.6s]
    histogram_quantile_bounds_promises_test.go:38: p95 of 10 samples = 16.384s, want the slowest sample 9.6s
--- FAIL: TestHistogramQuantileInTheOverflowIsNotUnderstated (0.00s)
    histogram_quantile_bounds_promises_test.go:48: p95 of ten 100s samples = 1m5.536s, want 100s (the largest bucket bound is 65.536s)
--- FAIL: TestHistogramQuantileOfIdenticalSamplesIsThatSample (0.00s)
    histogram_quantile_bounds_promises_test.go:59: q0.01 of fifty 3ms samples = 2.04ms, want 3ms
    histogram_quantile_bounds_promises_test.go:59: q0.99 of fifty 3ms samples = 4ms, want 3ms
--- FAIL: TestQuantileThresholdJudgesTheObservedCostsNotABucketBound (0.01s)
    threshold_verdict_promises_test.go:71: every cost ≤ 9.6s but p95 ≤ 10s judged failed/threshold thresholds=[{Threshold:{Metric:p95 Max:10} Actual:16.384 Violated:true Reason:}] quantiles_ms=map[p50:12288 p90:15564 p95:16384 p99:16384]
--- FAIL: TestThresholdFailureNamesTheThresholdAndTheActualValue (0.01s)
    threshold_verdict_promises_test.go:87: failure explanation "" does not mention "p95"
    threshold_verdict_promises_test.go:87: failure explanation "" does not mention "9.6"
    threshold_verdict_promises_test.go:87: failure explanation "" does not mention "max 9"
    threshold_verdict_promises_test.go:87: failure explanation "" does not mention "10 samples"
```

`ThresholdResult.Samples` 是新字段，带上它的断言修前编译失败，不作为红。修后 `go test -race -count=3 ./metrics/ ./robot/...` 通过；`go test ./codegen/...`、`go generate ./...` 后 porcelain、重新生成 game-demo 的 build / vet / test 通过（演练记录“验证”）。真实进程（演练 ⑥ 两轮，同机 Cluster）：两个 sid C / D `rc=0`，p95 10.049 / 9.842s、10.130 / 9.837s（等于各自运行时长，即最慢机器人），单 sid 7.39～7.82s；同一组机器人修前的估计是 16.384s；一次失败运行的新输出 `loadtest: run … ended failed (threshold): threshold violated: error_rate = 1 > max 0 (10 samples); p95: no samples to judge (max 16s)`。

**性能**：`observe` 多两次原子读与（偶尔的）CAS，只在最小 / 最大被刷新时写；记录没有基准数据。

**review 检查点**：

1. `observe` 的写入顺序（最小 / 最大 → 桶 → 计数）与 `quantile` 的读取顺序（计数 → 范围 → 桶）：确认并发观测时读到的计数所对应的样本一定已经拓宽了范围，夹取不会把一个已计数的样本截掉。
2. 空直方图（`count == 0`）与只有溢出样本时 `minNanos` 仍是 `math.MaxInt64` 的分支：确认 `quantile` 不会把 `MaxInt64` 当成下界参与插值。
3. `evaluate` 里分位数的 `Samples` 取 `stats.Success` 与直方图样本数的较小值（`robot/loadtest/manager.go:551-553`）：确认 error_rate 用完成的场景数、分位数用成功场景数，失败说明里的样本数与阈值实际依据的样本一致。

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

**改动文件与关键符号**：`scripts/test-remote-matrix.sh:6-10`（取锁、导出 `ROOST_REMOTE_ACCEPTANCE_LOCK_HELD`）、`:32-34`（`run_case`：退出 0 且日志含 `no tests to run` / `[no test files]` → `FAIL(no tests ran)`）；`kit/scripts/integration/lib/common.sh:71-82`（锁路径与“持锁者子进程沿用”）；`kit/scripts/integration/dataengine-env.sh`（`:164` 故障矩阵的 integration 包列表；v1.23.0 `0db819b0` 加 `./kit/mongo`——真实进程演练 ④ 给 kit Mongo 加了需要完整环境的用例，下面的根包门禁要求矩阵也跑它，APP-15）、`scripts/remote-fault.sh`、`scripts/test-remote-generated.sh`；`acceptance_lock_promises_test.go:22` `TestGlobalEnvironmentOperationsHoldTheAcceptanceLock`；`integration_coverage_promises_test.go:216` `TestFaultMatrixScriptNamesEveryFullEnvironmentSuite`（v1.19.2 之前已有）。

**测试**：NC-207 修前红：替身 go 输出 `no tests to run` 时修前全部 go test 格 PASS、矩阵 exit 0；修后 8 个 go test 格 `FAIL(no tests ran)`、矩阵 exit 1；控制：替身输出真实 `--- PASS:` 时全部 PASS（脚本与输出见 [NC-207 修复](../../bugfix/RR-20261005-NC-207.md)，问题记录没有抄录可复制的红文本）。v1.23.0 预跑：`ROOST_REMOTE_MATRIX_LABEL=matrix-relprep-20261006 bash scripts/test-remote-matrix.sh`，17:22:13 → 17:26:30，业务 12 格与 lease-process（3）、redis-cluster、redis-unreplicated-fence、durable-process、ownership-counters（13）、mongo-wal-recovery、broker-failover（3）、broker-network（3）、final-health 全部 PASS、无 SKIP；源码 `d6a677e0`。

**发版步骤**（不是未验证项）：预跑在 `d6a677e0` 上；按惯例在最终发版提交上再跑一次（发版前补充验证 §5“发版仍按惯例在最终 HEAD 再跑一次”，外部验证清单“每版 pretag 重跑”）。NC-207 修复记录写的“修后的整张矩阵没有在真实隔离环境上跑”已由这次预跑闭环：`d6a677e0` 含 NC-207 与 A5，21 格全 PASS、无 SKIP。

**review 检查点**：

1. 新增的矩阵格是否同时登记进 `TestFaultMatrixScriptNamesEveryFullEnvironmentSuite` 的检查范围。
2. `--- SKIP:` 仍记 FAIL：确认新增用例在共享环境缺依赖时不会以 SKIP 静默通过。

<a id="tool-8"></a>
### TOOL-8 平台支持：Windows 不保证正确

**提交与首发**：v1.23.0 `7fec136e`（只改文档；维护者 2026-10-06 第十三轮）。

**改动文件**：`README.md`（“平台支持”一段；CI 一句改为“Linux 上跑完整矩阵、Windows 只跑一个 `go test ./...` 兼容性 job”）、`docs/DEPLOYMENT.md` 开头、`CHANGELOG.md`（v1.23.0 一条）、`docs/bug/WANTED.md`（W-2026-10-04-05 标“暂存”）、[外部验证清单](../../review/EXTERNAL-VERIFICATION-2026-10-06.md) E25（状态改“暂存”）与 E27（通过标准里 Windows 部分暂存）、`docs/review/DECISIONS-PENDING-2026-10-05.md` 第十三轮表。

**不变量**：正确性只在 Linux（生产）与 macOS（开发）上保证和验证；Windows 的编译、CLI 制品与 CI 兼容性 job 保留，但任何 Windows 行为都不作为发版条件。代码与 CI 配置没有改。

**测试**：只改文档，没有红绿测试；`TestTrackedMarkdownRelativeLinksResolve`（TOOL-5）覆盖新加的链接。

**未验证**：Windows 一律“暂存，不保证正确”（E25；E27 的 Windows 部分）。

**review 检查点**：本分册与另两份分册里提到 Windows 的地方（未验证项、已知限制）是否都写成“暂存，不保证正确”，没有写成待修或待验证。

<a id="tool-9"></a>
### TOOL-9 交给 review 之前不留 WANTED

**提交与首发**：v1.23.0 `87d8d91e`（只改规范；维护者 2026-10-06 第十三轮）。

**改动文件**：[roost-bugfix §7](../../agent-skills/roost-bugfix/SKILL.md)：`docs/bug/WANTED.md` 不再是“实现侧写给 review 拍板的候选表”，实现侧看到的疑点（签名承诺了但实现没履行 / 跨包契约对不上 / 读源码推断出的风险）本轮自己闭环——能写出红测试的按 RR 修；写不出红的加结构性守卫或写清不可达的证明并关闭；真正需要产品决定的直接提给维护者。WANTED 只作历史分流记录，新条目同一轮内必须转为 RR / 守卫 / 关闭。

**不变量**：交给 review 时 WANTED 未决数为 0；review 检查点只放“给 review 去查的问题”，不放“已知风险待判断”。本分册的落实：W-2026-10-06-02 → RR-20261006-10（APP-7）；W-2026-10-06-01 → RR-20261006-12（`b7471ae4`，NONCORE 分册）；上一版分册报给汇总者的 6 项“本机可做、记录写明没做”按同一规则闭环——4 项真实进程演练（APP-15，途中 RR-20261006-24～29）、协调器按组核对（RR-20261006-17，OWN-5）、O3 两类序列（RR-20261006-18 / -19，OPS-2）；本分册各条的 review 检查点都是可核对的问题，未验证项只剩外部环境类（E 编号）。

**测试**：只改规范，没有红绿测试。

**review 检查点**：`docs/bug/WANTED.md` 里 2026-10-06 之后的条目是否都已标“已转 RR / 关闭 / 暂存”；三份分册的 review 检查点里有没有残留“待判断”“待 review 拍板”的措辞。

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
| `TestAGroupFitsOneLiveQuery` | `kit/service/global/activity/groups_live_limit_promises_test.go:15` | 活动组上限不超过一次 `App.Live` 的上限 | OWN-4、OWN-5 |
| `TestTheCoordinatorRefusesAnExpectedSetTheGroupsFileDoesNotAllow`、`TestTheCoordinatorRefusesToStartWithoutAGroupsFile`、`TestNewRefusesACoordinatorWithoutGroups` | `kit/service/global/activity/open_expected_group_promises_test.go:48` / `:101` / `:126` | 协调器开窗按组核对；没有绕过核对的构造方式 | OWN-5 |
| `TestDestroyedDispatchersLeaveNoSeries` 等四条 | `nest/dispatcher_series_lifecycle_promises_test.go:43` | 派发器序列随最后一个同名派发器排空删除 | OPS-2 |
| `TestCallerRPCMethodLabelsAreBounded`、`TestServedRPCMethodLabelsAreTheRegisteredMethods` | `bus/rpc_method_label_bound_promises_test.go:35` / `:79` | 可靠 RPC 的 method 标签基数有固定上界 | OPS-2 |
| `TestHistogramQuantileNeverExceedsTheSlowestObservation` 等三条 | `metrics/histogram_quantile_bounds_promises_test.go:18` | 分位数估计在观测范围内 | OPS-8 |
| `TestProductionRedisNeedsAnAddrOrClusterSeeds` | `kit/redis/config_types_promises_test.go:72` | 生产 Redis：`addr` 或 `cluster_addrs` 任一即可 | APP-14 |
| `TestRealMongoModCloseContract`、`TestRealNatsModCloseContract`、`TestRealNatsModUndrainedCloseIsReportedOnce`（`-tags integration`） | `kit/mongo/close_contract_real_promises_test.go:27`、`kit/nats/close_contract_real_promises_test.go:61` / `:188` | kit Mod 在真实依赖上的 Close 契约；故障矩阵脚本必须跑 `./kit/mongo`（`TestFaultMatrixScriptNamesEveryFullEnvironmentSuite`） | APP-7、APP-15 |
| `TestNetworkCodegenTestsRunInSomeWorkflow`（C9，不属于本部分） | 根包 `ci_generated_code_test.go:184` | codegen 联网用例在某个 workflow 里跑 | NONCORE-31（另一分册） |

## 按包的改动索引

| 包 / 目录 | 条目 |
| --- | --- |
| `app` | APP-1、APP-4、APP-5、APP-8、APP-9、APP-12、APP-13、CLK-1、CLK-3、CLK-5 |
| `lifecycle` | APP-8 |
| `nest` | OPS-2 |
| `bus` | OPS-2 |
| `clock` | CLK-1 |
| `health` | APP-10、APP-11 |
| `internal/operation` | APP-6、APP-7 |
| `internal/stopcontract` | APP-6 |
| `cmd/glsvet` | APP-6、CLK-1 |
| `metrics` | OPS-2、OPS-8 |
| `servicemetrics` | OPS-1 |
| `timer` | CLK-6 |
| `versionstore` | OPS-5 |
| `etcd/driver` | APP-2 |
| `robot/loadtest`、`robot/runner` | OPS-2、OPS-7、OPS-8 |
| `ai`、`actionflow` | CLK-1（缺省时钟） |
| `kit/redis` | APP-1、APP-3、APP-5、APP-7、APP-14 |
| `kit/mongo`、`kit/nats` | APP-7、APP-15 |
| `kit/nest` | APP-4、APP-6 |
| `kit/ops` | APP-10、APP-11、OPS-3、OPS-4 |
| `kit/mods` | APP-1、APP-3、APP-13、OPS-1 |
| `kit/remoteentity` | APP-7、APP-13 |
| `kit/etcd` | APP-3 |
| `kit/service/global` | APP-3、OWN-6 |
| `kit/service/global/activity` | OWN-4、OWN-5、CLK-1、CLK-4 |
| `kit/service/{mail,rank,session}`、`service/mail`、`service/session` | CLK-1、CLK-4、OPS-1 |
| `kit/service/{match,chat,account}`、`service/match` | CLK-2、OPS-1、OPS-5、OWN-2（`account.ServerIDClaim`） |
| `codegen/internal/roost` | APP-2、APP-3、APP-14、OWN-2～5、CLK-2、OPS-1、OPS-6 |
| `demo/`（game-demo 模板） | APP-14（`cmd/accountctl`）、OWN-1～5、OWN-7、CLK-1、CLK-6、OPS-6、OPS-8（`cmd/loadtest`） |
| `scripts`、`codegen/scripts`、`kit/scripts/integration` | TOOL-1、TOOL-2、TOOL-6、TOOL-7、APP-15（`dataengine-env.sh` 加 `./kit/mongo`） |
| 根包（`*_test.go`） | TOOL-2～5、TOOL-7 |
| 文档与规范（`README.md`、`docs/DEPLOYMENT.md`、`docs/agent-skills/*`、`docs/bugfix/REAL-PROCESS-DRILLS-2026-10-06.md` 与演练脚本） | APP-6、APP-7、APP-15、TOOL-4、TOOL-8、TOOL-9 |
