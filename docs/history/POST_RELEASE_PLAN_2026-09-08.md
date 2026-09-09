# 发版后四项遗留的执行方案（2026-09-08）

关联：[P6_release.md](P6_release.md) §6、[P5_acceptance.md](P5_acceptance.md) §4、[P3_kit.md](P3_kit.md) §1.1、[账本](ledger.md) §4（B-14 / B-18 / B-19）、[v2 收敛方案](../ARCHITECTURE_V2_CONSOLIDATION_PLAN.zh-CN.md) §6。

状态：**五项全部完成**（2026-09-09 发版收尾；2026-09-08 决定：B-14 不单发补丁，修复合 main 后随 P3b 的 core v1.15.0 发版）。本机核对（2026-09-08）：三仓 main 干净，core `212afb7` / kit `e6163c5` / codegen `d24a6d1`；正式 tag core v1.14.0 / kit v1.13.0 / codegen v1.15.0；go.work 只含三仓；本机有 docker CLI（brew），但执行时守护进程未运行、未找到运行时（Docker Desktop / colima / OrbStack 均无），需要先起 daemon 才能跑故障矩阵与 Mod 级集成；benchstat 已 `go install` 到 `$(go env GOPATH)/bin`；`/tmp/roost-kit-main` 工作树已不存在。

## 0. 顺序与分轨

四项分两条轨：**U 轨**（B-18、B-19 只加测试；B-14 改代码走补丁发版）和 **M 轨**（P3b 是结构重构，独立 minor 发版）。基准归档是测量任务，要机器安静，与其他任何编译 / 测试互斥。

| 序 | 项 | 轨 | 改代码 | 发版 | 估时 |
| --- | --- | --- | --- | --- | --- |
| 1 | ~~B-18 entitysync（U-0104）~~ 完成 `4d22881` | U / C2 | 否 | 否 | 半个会话；真机时顺带补 core `etcd/driver/client.go:46`（Get 无键 → ErrKeyNotFound，U-0129 无法替身） |
| 2 | ~~B-19 redis（U-0105 / U-0106）~~ 完成 `d25c088` | U / C2 | 否 | 否 | 1 个会话 |
| 3 | ~~B-14 摘要丢错（U-0107）~~ 完成（合 main，待随 P3b 发版） | U / C5 | 是（core） | 不单发；随 P3b core v1.15.0 | 1 个会话 |
| 4 | ~~安静基准归档~~ 完成（`P5_acceptance.md` §4.3，实际四段 95 分钟） | 测量 | 否（补脚本） | 否 | 机器空闲 1～1.5 小时，人工 20 分钟 |
| 5 | ~~P3b Mod 瘦身~~ 完成并发版（core v1.15.0 / kit v1.14.0 / codegen v1.15.1，2026-09-09），见 [P3b_mods.md](P3b_mods.md) | M | 是（core + kit） | core v1.15.0 → kit v1.14.0 → codegen 清单 | 实际一个会话（三批合一） |

理由：1、2 只加测试，先把两个 B 项关掉、账本干净；3 是唯一的 bug 修复，单独走补丁线，不和 P3b 的 API 变动混在一个版本里；4 放在 3 之后、机器空闲时段（午休或过夜，`caffeinate -i`），因为 B-26 之后代码没再动过 nestwal / dataengine / saga，何时跑结果一样；5 最大，最后做，且它的 core 发版顺带把 3 的修复带进 minor。

## 0.1 2026-09-09 追加的六项（用户排序：2、3 立刻；6 必须；4、5 可做；1 稍后）

| 项 | 状态 |
| --- | --- |
| 1 本机 Mod 级真实集成 | **未做**：brew 只装了 docker CLI，无 daemon；等运行时（Docker Desktop cask 或 colima）起来后跑 `dataengine-env.sh test` 五切片，补进 `P3b_mods.md` §4 |
| 2 118 个未审格 | **完成**：`classscan.py` 首轮扫过 117 格（service / skill / codegen），无真洞，四条观察 O-1～O-4 记账本 §9 |
| 3 nightly 高位包 | **完成**：U-0108～U-0116 九个包（core nestwal / dataengine/engine / nest / robot/action / mongotest；kit dataengine Mod / service/session；codegen protocol / nest）；第二批 **完成**：U-0126～U-0143（core syncstream ×2 / remoteentity / etcd/driver / skill/combatcomponent / nettransport / bus / skillcompose（复核） / skill / nats/driver / app / lockstep / cache / statesync；kit service/mail / match / account / platform），昨夜报告 ≥7/20 的包全部处理；4 条真机待补（etcd client.go:46）、其余未红者均已记冗余 / 不可达 |
| 4 C5 / C8 深挖 | **进行中**：C8 → U-0117；C5 grep 扫 core / kit 后台循环 → U-0120 / U-0121 / U-0122（计数，T-47）、顺带发现并修了 U-0119（activity sweep 组是桩，T-46）。故障切片等 daemon |
| 5 顺手观察 | MigrationRunner 三次冲突 → U-0109 已钉；O-1 路径字面量 → U-0118 已改；codegen entity / nest sender 模板守卫 → U-0124 生成物自带 `*_gen_wire_test.go` / `*_nest_gen_test.go`，余两条 U-0144 复核为对生成 DAO 不可达；死守卫清理 → U-0123：entity 四处不可达守卫已删；actionflow 四处复核：一处可达未测 → U-0125 钉住，三处冗余 / 不可达保留 |
| 6 升级器符号改名表 | **完成**：codegen `23c5964` 删除 `renames`，只映射包路径；三处符号由编译器指出，T-45 给改法 |

**2026-09-09 第二次发版**：kit v1.14.1（tag CI integration 首跑遇 Mongo 选主抖动，重跑绿）→ codegen v1.15.2（framework-release 全绿，8 个资产）。core 未发版：v1.15.0 之后只有测试、文档与 U-0123 的死代码删除。

## 1. B-18：core `entitysync`（U-0104，C2）

本机重跑采样（`revertsample.py --max 30 ./entitysync`）：**7 / 9 无覆盖**，与账本一致。七条全部是入口参数守卫，三种错误：

| 守卫 | 位置 | 错误 |
| --- | --- | --- |
| `f == nil` | `subscription.go:97`（`EnvelopeSinkFunc` 构造） | `ErrEnvelopeSinkRequired` |
| `sink == nil` | `subscription.go:641`（`admitEnvelopes`） | `ErrEnvelopeSinkRequired` |
| `c == nil \|\| subscriber.Empty()` | `:174`（Subscribe）、`:247`（Unsubscribe） | `ErrSubscriberInvalid` |
| `state == nil \|\| state.SubjectID() == 0` | `:177`（Subscribe） | `ErrSubscriptionSubject` |
| `subjectID == 0` | `:250`（Unsubscribe） | `ErrSubscriptionSubject` |
| `c == nil \|\| state == nil \|\| state.SubjectID() == 0` | `:486` | `ErrSubscriptionSubject` |

做法：一个文件 `entitysync/subscription_promises_test.go`，表驱动，每条守卫一行；夹具复用现有 `subscription_test.go` 的 coordinator / sink 替身，并保证被测守卫是**唯一**可能的拒绝者（合法 subscriber + 合法 state 时先跑绿，再单独把一个参数打坏）。nil coordinator 的分支要用 `var c *SubscriptionCoordinator` 直接调方法。`:641` 的 `sink == nil` 由 Subscribe 传 nil sink 触达（先确认 `:213` 不会提前拒绝；若提前拒绝则记"冗余"，不算红）。

回退验证 7 条各红；产出：测试、core CHANGELOG `### Changed（测试质量）` 一条、账本 §5 行 + §3 `entitysync` 的 C2 格、§4 关闭 B-18。不改运行时代码。

## 2. B-19：core `redis` + `redis/driver`（U-0105 / U-0106，C2）

本机重跑采样：契约包 `redis` **2 / 2**、驱动包 `redis/driver` **19 / 24** 无覆盖，合计 21 条。已有 `client_deadline_test.go`（U-0061）、`lock_state_test.go`、`lock_test.go`、`client_test.go`（WAITAOF 解析、集群拓扑拒绝），不重复。按影响分级：

**A. 实质行为（优先，必须钉）**

| 守卫 | 位置 | 为什么重要 |
| --- | --- | --- |
| `err == goredis.Nil → fredis.ErrNil` × 7 | `driver/client.go:85,173,207,215,256,264,272`（Get / HGet / LIndex / … / 计数类） | 调用方（cache read-through、dataengine、service）全靠 `errors.Is(err, redis.ErrNil)` 区分"不存在"与"故障"；映射丢了就把 miss 当错误 |
| `len(results) != 1`、`len(reply) != 2` | `driver/client.go:339,369`（`EvalDurable` 结果与 WAITAOF 回复形状） | 形状错误应报错而不是越界 panic 或静默用错值 |
| `typed > MaxInt64` | `driver/client.go:432`（`redisInteger`） | 溢出静默变负数 |
| `err != nil && err != goredis.Nil` | `driver/pipeline.go:126` | pipeline 里 Nil 必须被容忍、其他错误必须上抛，两边各一条 |
| `len(items) != 2` | `redis/cas.go:86` | CAS 脚本返回形状守卫 |
| `state == distLockIdle \|\| value == ""` × 2 | `driver/lock.go:128,153`（Release / Extend 未持有） | `ErrLockNotHeld` 是锁的核心承诺 |
| `err != nil \|\| !ok` | `driver/lock.go:236`（AutoExtend 首次 acquire 失败透传） | 失败时不得启动续期 goroutine（用副作用断言） |

**补充**：driver 的 `lock_toxic_integration_test.go` / `mget_integration_test.go`（`-tags integration`）在 docker daemon 起来后本机用 `dataengine-env.sh` 环境跑一遍，作为 B-19 收口的真实环境证据；不算回退验证。

**B. 防御性 nil / 配置守卫（一起钉，成本低）**：`driver/lock.go:120,145,222,306,326`、`redis/cas.go:53`。

夹具：`lock_state_test.go` 已有 `scriptedRedis` 脚本化替身，可扩展；ErrNil 映射与 pipeline 需要一个能返回 `goredis.Nil` 的 `goredis.Cmdable` 替身——先看 `client_export_test.go` / `client_test.go` 的 `EvalDurable` 测试怎么造 `rdb`，复用同一路径；仓里没有 miniredis，**不新增依赖**。拆分：契约包 2 条 + driver 19 条如果一个会话装不下，按"一次会话 · 一个包"拆成 U-0105（driver）与 U-0106（redis 契约，2 条，顺手）。

产出同 §1；账本 §3 `redis` 行的"新位置"已是 core，格子写 `09-xx U-0105（回退 N 条）`；关闭 B-19。

## 3. B-14：摘要 `json.Marshal` 丢错（U-0107，C5，改代码）

三处（收敛后都在 core）：

| 函数 | 位置 | 输入 | 当前能否失败 |
| --- | --- | --- | --- |
| `DeadLetterEntry.requeueMsgID` | `bus/reliable.go:276` | string / int32 / int64 / []byte | **不能**（纯值） |
| `commandDigest(Command)` | `saga/command_consumer.go:368` | 含 `DeadlineAt` / `CreatedAt time.Time` | **能**：`time.Time` 年份超出 [0,9999] 时 `MarshalJSON` 返回错误；`Command.Validate` 只要求非零，`time.Date(10000,…)` 合法通过 |
| `completionDigest(Completion)` | `saga/mongo_store.go:491` | 手写 stable 结构，string / bool / []byte | **不能** |

这解决了统一方案 §6 的疑虑（"不得为制造红测试强行放宽生产入参"）：`commandDigest` 的红测试不需要放宽任何东西，现有 `Validate` 就放行。失败后果：`raw == nil` → 所有此类命令共享同一个 sha256（空输入摘要）→ 收件箱 `readReceipt` 把不同命令误判为重复投递，返回别人的 completion。这是 C5（吞错）导致的 C8 类后果，够格发补丁。

做法：
1. 红测试：`saga/command_consumer_promises_test.go`——两条 `DeadlineAt` 年份 10000 的不同 Command，断言摘要**不相等**或返回错误；`bus/promises_test.go` 追加 `requeueMsgID` 的表驱动"不同条目不同 ID"（这一条现在就绿，是护栏不是红）。
2. 修复（最小、不混入 ID 改幂等语义）：三个摘要函数改为 `(…, error)`；调用方 `MongoCommandInbox.Receive` / `readReceipt` / `dataengine_step_inbox` 的三处、`MongoStore` 的三处、`bus.go:164` Requeue，把错误包装后返回（saga 用 `fmt.Errorf("%w: command digest: %v", ErrInvalidRecord, err)`，bus 返回 `fmt.Errorf("bus: dead letter requeue id: %w", err)`），**不发布、不写库**。`completionDigest` / `requeueMsgID` 一并改签名保持一致，虽然当前不可失败。
3. 不动 `Validate`（是否拒绝年份越界的时间是另一个 API 决定，记账本 §4 观察）。
4. 回退验证：临时恢复 `raw, _ :=` 让红测试重新变红。
5. 四件产出 + `TROUBLESHOOTING.md` T-44；账本 §5、关闭 B-14。
6. 发版链（交接 §1 规则：修了代码就走链）：`scripts/pretag.sh v1.14.1` → core tag；kit go.mod 升 core v1.14.1、`pretag.sh v1.13.1` → tag；codegen `ci/framework-release.yaml` 清单改 core v1.14.1 / kit v1.13.1、`pretag.sh v1.15.1` → tag → 看 `framework-release` 与 `framework-compat` 六条 lane。若决定与 P3b 合并发版则跳过本步，但修复本身先合 main。

## 4. 发版后安静基准归档（P5 §4 收尾）

目的：给 B-26 之后的正式版留一份"迁移前 vs 迁移后"的安静基准，替代 P5 §4.1 那份带 `RecordEncodingMatrix` 回归的记录；预期回归消失、geomean 三项在 ±2% 内。

| 项 | 前（基线） | 后 |
| --- | --- | --- |
| 代码 | kit tag **v1.12.6**（收敛前最后正式版，含 nestwal / dataengine / saga） | core main **v1.14.0**（`40154e7`，B-14 修复不触及这三个包，之后跑也等价） |
| 检出 | `git -C roost-kit worktree add /tmp/roost-kit-main v1.12.6` | 本地 main |
| 包 | `./nestwal ./dataengine ./saga` | `./nestwal ./dataengine/engine ./saga` |
| 命令 | `GOWORK=off go test <pkgs> -run '^$' -bench . -benchmem -count=10` | 同 |
| 归一 | `sed` 把 `pkg:` 行改成 `pkg/nestwal` / `pkg/dataengine` / `pkg/saga`（P5 §4.1 同法），`benchstat before.norm.txt after.norm.txt` | |

前置：benchstat 已安装（`$(go env GOPATH)/bin/benchstat`，PATH 里没有则用全路径）；Unity / IDE / CI 观察全关，`caffeinate -i` 包住整条命令；两侧**交错各跑一轮**以排除时序漂移（P5 §4.1 用过）。全程只有这条命令在跑。

产出：`docs/history/P5_benchstat_release.txt`（全文）+ `P5_acceptance.md` 新增 §4.3 "正式版安静基准"（三项 geomean、是否还有 p<0.01 且 |Δ|>5% 的单项、有则按 §4.1 的四步归因；无则写"B-26 关闭，无系统性退化"）。顺手修一处过期：kit `scripts/perf/dataengine.sh` 仍指向 kit 里已删除的 `./nestwal`，把脚本搬到 core `scripts/perf/dataengine.sh` 并改包路径，kit 那份删除（CHANGELOG 各一条）。

## 5. P3b：kit Mod 瘦身（`Assemble*` 下沉 core）

### 5.1 目标与边界

P3 §1.1 的结论：Mod 仍是编排者，nats / etcd / redis / dataengine 直接拿 `Raw()` 和内部构造器拼装。P3b 的完成定义：

- **Mod 只做四件事**：viper → 配置结构；`registry.Lookup` 取依赖、`Register` 提供能力；健康检查注册；把生命周期（Init / Provide / Start / Stop）转交给 core 的装配对象。
- **core 每个包提供 `Assemble(deps, cfg) (*Assembly, error)`**，`Assembly` 暴露已装好的组件访问器与 `Start(ctx) / Close(ctx)`；构造顺序、失败回滚（比如 dataengine `Start` 里 WAL → projector → outbox → runtime 的逐级 Close）、`Raw()` 级探活全部在 core 内部。
- **行为零变化**：Mod 级测试（含 kit CI `integration` / `service-redis` 作业）不改断言、全绿；配置键、注册的能力名、错误文案不变。
- **新护栏**：kit 增加 `mods_no_raw_test.go`——Mod 包非测试文件不得出现 `.Raw()`、不得 import `roost-core/<x>/driver` 以外的实现构造器（与 `dependency_boundary_test.go` 同一套 AST 扫描）。

不做：不改 app.Registry / 能力名；不合并 Mod；不动 room / mongo（已经薄）；不改 codegen 模板（生成工程只用 Mod，不感知内部）。

### 5.2 分三小批（每批一份 `P3b-N_*.md` 记录，core 预发布 tag 递增）

| 批 | 包 | 现状（kit 行数） | 下沉内容 |
| --- | --- | --- | --- |
| P3b-1 | nats、etcd、redis | 412 / 190 / 111 | `nats.Assemble(cfg, extra, codec)` 返回 Client / JetStream / RPC（`NewRPCClient(client, policy, 4)` 的并发数常量进 core）；`etcd.Assemble(cfg)` 返回 Client / Discovery / Election 并提供 `Ping(ctx)`（替代 Mod 里两处 `Raw().Status(...)`）；`redis.Assemble(cfg)` 返回 Client / DistLockFactory（替代 `NewDistLockFactory(client.Raw())`）。**先做完整导出清单再打一个 alpha**（P3 的教训：alpha 打了四次） |
| P3b-2 | dataengine | 484（目录 1911 含测试） | `engine.Assemble(deps{Mongo, JetStream, Access, RemoteManager, OnFatal}, cfg)` 吃掉 Provide 后半段（MongoStore、远端投影绑定）和整个 Start（EnsureInfrastructure、EnsureStream、WAL Open、Projector、OutboxStore、`jetStreamOutboxPublisher`、OutboxWorker、Runtime、失败链式 Close）；Mod 保留 `WithEntityAccess` / `WithRemoteProjection` 选项、能力查找、`Runtime()` / `Repository()` / `NestOptions()` 等转发。`jetStreamOutboxPublisher` 搬到 core。kit `integration` 作业是这一批的门禁 |
| P3b-3 | saga、remoteentity | 384 / 347 | `saga.Assemble(mongo, js, cfg)` 返回 Store / Transport / Engine，`Start` 内含订阅建立与 `drainSubscriptions` 关停；remoteentity 把 `bindSyncer` / `stopReplicators` 的剩余编排并入已有 `Manager.BindSync`（`assembly.go`），Mod 只剩选项与注册 |

### 5.3 门禁与发版

- 每批：core `GOWORK=off go build/vet/test`；kit 对 core alpha `go vet -tags integration ./...` + 全部测试 + `mods_no_raw_test`；kit CI integration / service-redis 绿；codegen `scripts/source-head-check.sh full` 本地过。
- 三批合完：core `v1.15.0`（新增导出 API，additive minor；若 §3 未单独发补丁则一并带上 B-14）→ kit `v1.14.0`（Mod 重写）→ codegen 清单更新 + `framework-compat` 六条 lane。
- 账本：P3b 不算 U 单元；`P3_kit.md` §4 的"P3b 待做"改为指向记录。

### 5.4 风险

| 风险 | 缓解 |
| --- | --- |
| 导出集靠编译器逐轮发现、alpha 反复 | 每批先用 `go test -gcflags=-e -run '^$'` 列全 undefined 再打 tag |
| dataengine 失败回滚顺序在搬动中走样 | 先给现有顺序写一条"构造第 k 步失败时前 k-1 个组件都被 Close"的表驱动测试（用 P3 引入的测试缝），搬完在 core 侧原样跑 |
| Mod 级集成测试依赖 docker daemon | daemon 起来后本机用 `roost-kit/scripts/integration/dataengine-env.sh up && … test` 跑五切片当本地门禁（含 core 侧 Redis toxic 套件）；daemon 不可用时退到 kit CI `integration` 作业 |
| 业务工程把 Mod 当扩展点子类化 | 生成工程只调用 `NewXxxMod`，不继承；`framework-compat` 六 lane 覆盖 |

## 6. 开始执行的第一步

U-0104（B-18）：`entitysync/subscription_promises_test.go`，7 条守卫，回退各红，`go vet ./entitysync && go test ./entitysync && git add -A && git commit && git push`，然后账本。
