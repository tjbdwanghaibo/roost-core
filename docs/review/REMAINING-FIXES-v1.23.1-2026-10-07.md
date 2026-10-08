# v1.23.1 剩余修复：交接给其他 agent（2026-10-07）

> RR-25最新决定：必须保留MaxDeliver，旧无限重投方案不再等待批准；后续以[有界投递新方案](../feature/REFACTOR-2026-10-08-rr25-bounded-delivery.md)为准，尚未实施。

> 当前进度（2026-10-08）：B1～B5、B7已提交验收；B6除RR-25已提交，B8已提交a221d224并完成最终pretag验收。RR-25因取消消费者次数上限的动作被自动审批拒绝，等待明确确认；候选已保存补丁并移出待合并代码。不要因下文历史“进行中”重复实施，也不能把本轮记作全部完成。当前入口见 [v1.23.1实现](../release/v1.23.1-IMPLEMENTATION.md)。

维护者 2026-10-07：“以上需要我定的问题都按照推荐处理，需要把未完成的正在做的做完，剩余的整理成一个文档，我用其他的agent处理”。

本文是**接手入口**。问题的原始证据（条件、后果、`path:line`）都在登记表 [FRAMEWORK-DOCS-FINDINGS-2026-10-07.md](FRAMEWORK-DOCS-FINDINGS-2026-10-07.md) 和各分区实现文档 `docs/framework/impl/NN-*.md` 的“源码疑点”节；本文只做分批、给入口和规则，不重复证据。

## 1. 接手规则（必须遵守）

- 共同要求：[agent-impl-common.md](handoff-v1.23.0/agent-impl-common.md)（独立 worktree、先红后绿、显式 `git add`、push 前 rebase、推 main 被拦推同名分支并报告）。
- **资源约束（维护者明确要求）**：**不派任何子 agent**；同时并行的 agent **不超过 2 个**；自己直接读源码。
- **验证底线**：改了跨包行为（nest 提交语义、驱动契约、配置 schema、生成形状等），push 前必须跑一次**全量** `GOWORK=off go test ./...`；改生成模板还要重新生成 game-demo 跑 build / vet / test（结构性守卫批次合入后，codegen 测试会自动做这一步）。
- **编号**：缺陷按 RR 流程登记，编号从 `docs/bug/README.md` 头部当前最大号往后接（截至本文为 RR-20261006-66，进行中的两路可能再占若干号），push 前 fetch，撞号就顺延。
- **每修一项都要**：bug / bugfix 两份记录、两个 README 索引、CHANGELOG `[Unreleased]`、交接 `docs/CORE-OPTIMIZATION-HANDOFF.md` §7；登记表对应行改为“已修复（提交号，RR-…）”；对应分区 `docs/framework/impl/NN-*.md` 的疑点条目后追加“v1.23.1 已修复，见 RR-…”。框架文档以 v1.23.0 为基准，只追加说明，不改原描述。
- **疑点按证据处理**（维护者 2026-10-07 澄清，见 [roost-bugfix §7](../agent-skills/roost-bugfix/SKILL.md)）：确认的 bug 修复，排除的保留依据，证据不足的标记“尚未确认”并写清下一步，不为收尾强行关闭；守卫测试须说明实际保证与限制。真正需要产品决定的提给维护者，不自己拍板。
- 线上未部署：存储 / 协议改动不做旧版兼容（版本号按规则加 1，旧版本拒绝）。
- 反复出问题的模块（saga 完成判定、skill 衍生物生命周期、nats driver、skill 编译与执行侧）：如果修复又牵出连锁问题，先停下来向维护者汇报方向判断。

## 2. 已定的维护者决定（直接实施，不用再问）

| 编号 | 决定 |
| --- | --- |
| D1 | 能力覆盖条目加数量上限（如 `MaxAbilityOverlays`），超过时在设置入口拒绝，与 `MaxQueuedTasks` 同一做法（来源：RR-20261006-55 后续二） |
| D2 | 玩家 TCP 补应用层心跳（空闲超时断开）与每连接请求限流（令牌桶），作为生成 TCP 接入层的配置项，限流默认开启、阈值可配（F04-13） |
| D3 | 带 affinity 的 RPC 必须有 discovery，缺失时启动拒绝（RR-20261006-59 已按此实施，保持） |
| — | 其余“按推荐”的历史决定见登记表各行与 `DECISIONS-PENDING-2026-10-05.md` 第十三轮 |

## 3. 交接前已完成的两路（接手者不要重复做）

| 内容 | 登记行 | 状态 |
| --- | --- | --- |
| 结构性守卫：指标名与 `OBSERVABILITY.md` / 仪表盘一致性、三大块依赖方向、生成 game-demo 必须 build + vet、pretag 加 `go generate` 漂移检查 | F11（N1～N3）、F00（依赖方向）、F12（G7） | 已完成（`fe8ef362`，[REFACTOR-2026-10-07-structural-guards](../feature/REFACTOR-2026-10-07-structural-guards.md)、[RR-20261006-67](../bug/RR-20261006-67.md)） |
| remote / bus：F05-1～F05-7，外加 RPC 超时被截 5s、JetStream 传输不兼容 | F05-1～7、F09-R2、F09-R3 | 已完成（分支 `remfix`，`a9bb8923`，RR-20261006-68～74） |

两路均已完成并推送（结构性守卫 RR-20261006-67，`fe8ef362`；remote / bus RR-20261006-68～74，`a9bb8923`）。接手前先 `git pull` 看登记表的最新状态。截至交接，RR 已用到 **RR-20261006-74**，T 行到 **T-308**。

### 3.1 待维护者决定（接手者不要自己拍板）

- **Remote 发布路径方向判断**（来自 RR-20261006-69 的方向判断）：“提交后发布 / outbox”这一段反复出问题（RR-20260926-11、-37、-38、-61、-63、RR-20261006-69），根因是发布被绑在投影器和 finalizer 的重试上。v1.23.1 已在现有设计内解耦（补发循环）。候选方向：让 outbox 补发成为**唯一**发布者，投影器与 finalizer 只认持久结论、不再自己发布，删掉三路并发发布；代价是正常路径发布延迟略增。需要维护者选：A 实施（先写方案）/ B 保持现状。

## 4. 剩余批次（建议顺序；每批一个 agent，最多两批并行）

| 批 | 范围（登记行） | 主要包 | 提示 |
| --- | --- | --- | --- |
| B1 | F08-Y（skillsync / skillcompose，Y1～Y17）、F08-R 的 R5～R9、F08-H+ 的 H2～H8、D1 | `skill/skillsync`、`skill/skillcompose`、`skill/` | Y2（SweepIdle 后 ACK 永远失败、24h 后全局拒发）与 Y1（晚加入 observer 收到过期表现）优先；H2 包装 Host 不转发可选接口会“扣费后失败”；本模块属反复出问题模块，注意上报 |
| B2 | F08-C（战斗包 C1～C16） | `skill/combat`、`skill/combatcomponent`、`attribute`、`spatial` | C2 吸血、C4 修正求和回绕、C6 nil panic、C12 `TickBuffs` 无调用方优先 |
| B3 | F09-N（N1～N16）、F09-V、F09-K、F09-R4 其余、F09-D | `kit/service/*`、`service/*`、`codegen/internal/servicerpc` | N2 亚秒时长被截成秒优先；F09-K 的 `player_elsewhere` 丢 `owner_sid`；chat `retention_age=0` 按 72h 裁剪（`wip` 半成品已证实，需复核） |
| B4 | F07（F1～F10）、F10（T1～T7） | `configdata`、`codegen/internal/tablegen`、`internal/configschema`、`clock`、`timer` | F1 tablegen 漏 `json` 标签整列为 0、F2 demo 开关监听无 Rollback、F3 Nest 异步链读旧快照优先；T2 缺省时钟方向需按 D-L3 定一条统一规则（如无法从现有决定推出，问维护者） |
| B5 | F03-2～F03-12、F02-2～F02-5、F02-7、F01-1～F01-10 | `nest`、`nestwal`、`dataengine`、`app`、`cmd/glsvet` | F03-10（async 记录可能在 fsync 前投影）需先论证；F03-12 事务标记 TTL 启动校验；F01-8 `--check-config` 缺文件仍报 ok；F01-9 Mod 同名不拒；F02-3 glsvet 漏方法 handler |
| B6 | F04-3、F04-4、F04-10～F04-14、F04-16、D2 | `sync/entitysync`、`sync/syncbus/driver`、`codegen/internal/roost`（玩家 TCP）、`cache` | F04-14 syncbus JetStream 三个窗口；F04-10 panic 只打 Debug；F04-11 `shutdown_timeout: 0s` doctor 误报；D2 心跳与限流 |
| B7 | F11 其余（N4～N15）、F12 其余（G4～G6、G8） | `failurelog`、`kit/ops`、`kit/statslog`、`log`、`codegen/internal/roost` | N4 死信静默裁剪、N5 `/admin/execute` 无审计、N14 `ops.admin_token` 无 `secret` 标记 |
| B8 | 所有文档类行：F04-D、F08-D、F09-D、F05-7（若 remfix 未覆盖）、F12 G5、F00 ①～⑤（口径统一，尤其 pipelined “提交点”02 与 03 分歧） | `docs/`、各包注释 | F00 ① 以源码定义统一后改 02 / 03 两篇；不改代码 |

## 5. v1.23.1 发版步骤（全部批次完成后）

1. `codegen/ci/framework-release.yaml` 改 `release: v1.23.1`。
2. **必须**：`codegen/internal/roost/manifest.go` 的 `minimumVersions.Core` 与 `.github/workflows/framework-compat.yml` 的 minimum 行（含新加的 `cmd/roost@…` 检查）升到 **v1.23.1**——RR-20261006-57 之后 v1.23.0 的生成器读不懂新清单，发版前 minimum lane 会一直红（见 `docs/bugfix/RR-20261006-57.md`）。生成形状还依赖了 v1.23.1 的其他新 API（HandlerMeta Durability、affinity discovery 等）。
3. 干净 worktree 跑 `scripts/pretag.sh v1.23.1`（结构性守卫合入后含 `go generate` 漂移检查），再跑 `scripts/test-remote-matrix.sh`（21 格、独占、约 4 分钟；需要共享隔离环境 `~/.roost-it/roost-dataengine-it/env.sh`，含凭据只 source 不打印）。
4. 打 tag、推 tag；对着 tag 用 `GOPROXY=direct` 生成 game-demo 跑 build / vet / test（代理 sumdb 对新 tag 有延迟，`project new` 首次解析失败属正常，direct 重试）。
5. 回填：bug / bugfix README、交接 §7、登记表、各 `docs/framework/impl` 里的“v1.23.1 已修复”说法改为已发布。
6. 按维护者要求写 v1.23.1 发版双文档（说明 + 实现，见 `docs/release/v1.23.0-GUIDE.md` 的结构）；交 review 前按当前证据逐项列出已修复、已排除与尚未确认，尚未确认项不能计作已完成。

## 6. 接手进度：B5 第一批（2026-10-07）

基线 `85a13d4c`，隔离分支 `codex/maintainer-rules`。先统一维护者本轮确认的 review 中文注释与疑点证据规则（`1d6816d8`）；Codex 本地 coding / optimize / bugfix / review 已同步，Claude 专属文件保持原样。

- F01-8：[RR-20261007-01](../bugfix/RR-20261007-01.md)，检查缺失配置不能报成功。
- F01-9：[RR-20261007-02](../bugfix/RR-20261007-02.md)，服务专属 Mod 不能与共享 Mod 重名。
- F02-3：[RR-20261007-03](../bugfix/RR-20261007-03.md)，方法 handler 进入 glsvet 并发/捕获检查。

三项都有修前行为失败；修后 App/glsvet 三轮 race、全仓 build/vet、根包与三大模块 glsvet 已通过，全量测试通过（130 个包）。B5 其余项保持待处理，本批不代表 B5 全部完成。没有接手 remfix 的范围，没有发版。

本批实现提交 `aa12460b`，规范提交 `1d6816d8`；已对齐远端 `494fe096` 并再次通过目标 race、全仓 build/vet/test（130 个包）。未推送、未发布。原始日志在主检出 `artifacts/perf/core-b5-20261007/`。

## 7. 接手进度：B5 续批（2026-10-07）

基线 `6f0c1bd8`，工作树 `codex/remaining-fixes`。B5 剩余项已实施 / 核实：RR-20261007-04～06（pipelined 生成、TTL 校验、启动期限与信号）；F02-2 删除无效心跳配置与参数；F02-7 结构性加固；其余文档 / 观察项逐条更正。F03-10 的 async 先投影符合弱持久承诺，Ack 前仍 fsync；F03-8 按 C8 保留公开 API。

本批全仓 build / vet / test（130 个包）、三大模块 glsvet、正式生成 game-demo 的 build / vet / test 均通过；App 启动取消新增场景 3 轮 race 通过，7 个目标包 race 通过。全仓 go generate 已执行。 原始证据在主检出 `artifacts/perf/remaining-fixes-20261007/`，生成工程 `/tmp/roost-remaining-b5-demo` 可复查，命令见各 RR。本批未推送、未发布。

继续 B1～B4、B6～B8；Remote 唯一发布者方向仍等待维护者答复，不把 B5 完成当成全部完成。

## 8. B1 进行中（2026-10-07）

B5 续批提交 `56a2006c`，提交后 go generate 跟踪文件无漂移。B1 首批 RR-20261007-07～13 已修复，skillsync / syncstream race 通过，尚未全仓验收。Y5～9 文档/合同、Y12 资源上界、Y15 store 年龄、Y16/17 key 语义与 Host/R/D1 继续；B2～B4、B6～B8 未完成。原始证据已复制主检出 artifacts；不把本节当全部交接完成。

B1 续进：RR-20261007-14～18（H2/H3/H4/R6/Y8）已修复；Y5/Y7/Y9/R9 与 Y6 嵌套字段边界为文档更正。`go test -race ./skill/... ./syncstream` 6 包通过。仍需 Y12/Y15/Y16/17、R5/R7/R8、H5/H6/H7/H8、D1，再继续 B2～B4/B6～B8。各 RR 暂未提交，勿标全部完成。

## 2026-10-07 validation

`GOWORK=off go build ./...`, `go vet ./...`, `go test ./... -count=1`: PASS (130 tested packages). `go test -race ./skill/... ./syncstream -count=1`: PASS (6 packages). Logs: `artifacts/perf/remaining-fixes-20261007/roost-remaining-b1-{build,vet,all-test}.log`. Not pushed or released. CBM generation 2026-09-30 is stale; current worktree source and regressions are the evidence.

## 9. B1 最终验收（2026-10-07）

B1 已完成：RR-20261007-07～25、R7/R8/H8 与合同文档更正；Y14 按维护者 Windows 决定明确暂存。首批 376d848d，续批本提交。MaxAbilityOverlays 默认 10000，checkpoint v10，旧版本拒绝。

全仓 build / vet / test（130 包）通过，skill 全族与 syncstream 共 6 包 race 通过；独立 skill/integration/sync-e2e race 通过。原始日志 `artifacts/perf/remaining-fixes-20261007/roost-remaining-b1-final-*.log` 和 `roost-remaining-b1-complete-race.log`。未推送、未发布；继续 B2～B4/B6～B8。

B2 验收完成（2026-10-07）：全仓 build/vet/test（130 包）通过；skill 全族/spatial/nest/根包共 8 包 race 通过，独立 statusbridge 示例 go test 通过。RR-20261007-26～31 和 C11～C16 的边界说明、优化与守卫在本批提交。原始日志与快照专项基准已保存主检出 artifacts/perf/remaining-fixes-20261007；未推送/发布。继续 B3/B4/B6～B8。

## 10. B3 进行中（2026-10-08）

基线 f518f12d；RR-20261008-01～08 完成 N1～4/N7/N9/N10/N12/N14、F09-K 的 chat 保留和活动坏记录、S6 创建竞争。N5 新增 owner 内人工 ExhaustDispatch，不自动消耗离线 game 的投递预算；N6/N8/N11/N13/N16 为契约说明。首批8包 race 与后续 platform/activity race 已通过。F09-V 其余、R4、demo owner_sid、命名空间及 B4/B6～B8 继续，不能计作全部完成。

远端新增 ba0dd7c2 客户端 RS v2/C#；尚未合入本工作树，最终验收前须合并并重新验证生成 TCP。

B3 验收完成（2026-10-08）：RR-20261008-01～14；全仓 build/vet/test（130 包）通过；服务目标 race、私有 Redis 跨服务 race（28 命名空间和 admission 原子回收）通过。正式生成 game-demo 458 文件，最终 build/vet/test 通过；配置声明守卫初次发现 WriteGate 未走统一声明，已按正式声明和缓存配置值修复，完整复验通过。`go generate ./...` 已运行。原始日志在 artifacts/perf/remaining-fixes-20261007，未推送/发布。继续 B4/B6/B7/B8。

### B4 验收完成（2026-10-08）

B3 已提交 b01f7e66；远端 RS v2/C# 合并 0dff3676（CHANGELOG 冲突保留双方全部记录并逐行核对）。B4 RR-20261008-15～21 已实施，目标 13 包 race、tablegen 实际生成运行、开关回滚红绿通过；全仓 build/vet/test（131 包）通过，重新生成 game-demo 的 build/vet/test（19 个测试包）通过，含定时器清理提交/回滚和开关监听回滚。最终 timer race 通过；所有红绿日志已复制主检出 artifacts/perf/remaining-fixes-20261007/。B6/B7/B8 待处理，Remote 唯一 publisher 产品选择仍待回复，不据此阻塞已授权批次。

### B6 进行中（2026-10-08）

B4 提交 50526a2c。B6 RR-22/23/24/26/27/28 已实施，cache/policy/kit-nest 和生成 TCP/Game race 通过，生成完整工程 build/vet/test 通过。RR-25 本地候选修复通过 race；取消消费者投递次数上限的配置整理被自动审批拒绝，已请求维护者确认有延迟、受流保留期/容量限制的空窗重投方案，不改业务错误 ACK 与 RPC/Remote 策略。真实 broker 验证尚未完成。B7 观测缺陷已开始红绿，B8待处理。

### B7 实施进度（2026-10-08）

RR-20261008-29～38 已完成目标行为红绿；admin/ops/statslog/log/failurelog/configschema/app/dataengine-engine/bus race 已通过。N8/9/10/12/13/15 的指标、性能和公开 API 保留边界见 impl/11 末节。完整生成器回归进行中，B8 文档继续；RR-25 仍等待确认。尚未全部完成、未推送/发布。

B7 验收：全仓 GOWORK=off build/vet/test 通过（131 个测试包），完整 codegen 测试包含重新生成工程并 build/vet/test。首轮旧断言仍要求 versions.skill=latest 已按本批删除失效字段的规格纠正，第二轮全仓通过。原始日志已持久保存；未推送/发布。

### B8 文档与独立验收完成（2026-10-08）

F00/F04-D/F08-D/F09-D/F12 G5 按 [B8逐项记录](B8-DOCUMENTATION-CLOSURE-2026-10-08.md)收口；当前指南、模板注释和基建包地图已同步，历史framework正文保留并追加当前边界。补齐v1.23.1说明/实现双文档，准备release清单与minimum版本，不打tag。RR-39更正旧Remote验收契约并补真实Mongo后续事务验证，生产代码不变。最终独立game-demo build/vet/test（19包）与TCP/Game race（2包）通过；RR-25候选移出后，私有Remote矩阵21/21通过。日志已持久保存。仍待RR-25明确批准，不计全部完成。

最终收口：B8提交 `a221d224`，干净树pretag通过（生成漂移/build/vet/tidy/全仓test）；根包/kit独立-count=1与三大块glsvet通过。测试环境已清理，证据持久保存。RR-25仍待明确批准，不打tag；其余已授权批次进入合并推送。
