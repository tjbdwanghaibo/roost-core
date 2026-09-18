# 发版后四项遗留的执行方案（2026-09-08）

关联：[P6_release.md](P6_release.md) §6、[P5_acceptance.md](P5_acceptance.md) §4、[P3_kit.md](P3_kit.md) §1.1、[账本](ledger.md) §4（B-14 / B-18 / B-19）、[v2 收敛方案](../ARCHITECTURE_V2_CONSOLIDATION_PLAN.zh-CN.md) §6。

状态：**五项全部完成**（2026-09-09 发版收尾；2026-09-08 决定：B-14 不单发补丁，修复合 main 后随 P3b 的 core v1.15.0 发版）。本机核对（2026-09-08）：三仓 main 干净，core `212afb7` / kit `e6163c5` / codegen `d24a6d1`；正式 tag core v1.14.0 / kit v1.13.0 / codegen v1.15.0；go.work 只含三仓；本机有 docker CLI（brew），但执行时守护进程未运行、未找到运行时（Docker Desktop / colima / OrbStack 均无），需要先起 daemon 才能跑故障矩阵与 Mod 级集成；benchstat 已 `go install` 到 `$(go env GOPATH)/bin`；`/tmp/roost-kit-main` 工作树已不存在。

## 0. 顺序与分轨

四项分两条轨：**U 轨**（B-18、B-19 只加测试；B-14 改代码走补丁发版）和 **M 轨**（P3b 是结构重构，独立 minor 发版）。基准归档是测量任务，要机器安静，与其他任何编译 / 测试互斥。

| 序 | 项 | 轨 | 改代码 | 发版 | 估时 |
| --- | --- | --- | --- | --- | --- |
| 1 | ~~B-18 entitysync（U-0104）~~ 完成 `4d22881` | U / C2 | 否 | 否 | 半个会话；真机五条已由 U-0153 处理 |
| 2 | ~~B-19 redis（U-0105 / U-0106）~~ 完成 `d25c088` | U / C2 | 否 | 否 | 1 个会话 |
| 3 | ~~B-14 摘要丢错（U-0107）~~ 完成（合 main，待随 P3b 发版） | U / C5 | 是（core） | 不单发；随 P3b core v1.15.0 | 1 个会话 |
| 4 | ~~安静基准归档~~ 完成（`P5_acceptance.md` §4.3，实际四段 95 分钟） | 测量 | 否（补脚本） | 否 | 机器空闲 1～1.5 小时，人工 20 分钟 |
| 5 | ~~P3b Mod 瘦身~~ 完成并发版（core v1.15.0 / kit v1.14.0 / codegen v1.15.1，2026-09-09），见 [P3b_mods.md](P3b_mods.md) | M | 是（core + kit） | core v1.15.0 → kit v1.14.0 → codegen 清单 | 实际一个会话（三批合一） |

理由：1、2 只加测试，先把两个 B 项关掉、账本干净；3 是唯一的 bug 修复，单独走补丁线，不和 P3b 的 API 变动混在一个版本里；4 放在 3 之后、机器空闲时段（午休或过夜，`caffeinate -i`），因为 B-26 之后代码没再动过 nestwal / dataengine / saga，何时跑结果一样；5 最大，最后做，且它的 core 发版顺带把 3 的修复带进 minor。

## 0.1 2026-09-09 追加的六项（用户排序：2、3 立刻；6 必须；4、5 可做；1 稍后）

| 项 | 状态 |
| --- | --- |
| 1 本机 Mod 级真实集成 | **完成（2026-09-09）**：集成环境其实靠 brew 二进制而非 docker（此前判断有误）。`reset` 后 fresh 环境整套 `dataengine-env.sh test` 绿：kit dataengine（real / failover / toxic）与 nats（JetStream RPC toxic + U-0153 真机守卫）、core redis + redis/driver 故障套件 + etcd/driver + mongo/driver。五条真机守卫 U-0153 全部钉住（93 记不可达）。修了两个基建问题：toxic RPC 跨轮主题重叠、脚本 core 段漏跑 redis/driver |
| 2 118 个未审格 | **完成**：`classscan.py` 首轮扫过 117 格（service / skill / codegen），无真洞，四条观察 O-1～O-4 记账本 §9 |
| 3 nightly 高位包 | **完成**：U-0108～U-0116 九个包（core nestwal / dataengine/engine / nest / robot/action / mongotest；kit dataengine Mod / service/session；codegen protocol / nest）；第二批 **完成**：U-0126～U-0143（core syncstream ×2 / remoteentity / etcd/driver / skill/combatcomponent / nettransport / bus / skillcompose（复核） / skill / nats/driver / app / lockstep / cache / statesync；kit service/mail / match / account / platform），昨夜报告 ≥7/20 的包全部处理；第三批 **完成**：U-0147～U-0151 五个批次单元把三仓余下的 nil / 参数守卫全部过完（48 包，约 300 条，红 ≈ 280）。剩余未红者分类：真机 5 条（etcd client.go:46、mongo/driver ×3、kit nats Mod Provide）、不可测 1 条（hotcode 需真插件）、留待 5 条已由 U-0152 处理：4 条钉红，roost add.go:299 记防御性包装（补钉 rollbackSync 检查分支）；其余冗余 / 不可达 / 防御均已在各单元行注明 |
| 4 C5 / C8 深挖 | **进行中**：C8 → U-0117；C5 grep 扫 core / kit 后台循环 → U-0120 / U-0121 / U-0122（计数，T-47）、顺带发现并修了 U-0119（activity sweep 组是桩，T-46）。故障切片等 daemon |
| 5 顺手观察（含 C8 候选） | MigrationRunner 三次冲突 → U-0109 已钉；O-1 路径字面量 → U-0118 已改；codegen entity / nest sender 模板守卫 → U-0124 生成物自带 `*_gen_wire_test.go` / `*_nest_gen_test.go`，余两条 U-0144 复核为对生成 DAO 不可达；死守卫清理 → U-0123：entity 四处不可达守卫已删；actionflow 四处复核：一处可达未测 → U-0125 钉住，三处冗余 / 不可达保留；C8 候选：mail Get / GetMany → U-0145 修复（T-48，kit 待发 v1.14.2）、dataengine Commit / Enqueue → U-0146 修复（T-49，core 待发 v1.15.1） |
| 6 升级器符号改名表 | **完成**：codegen `23c5964` 删除 `renames`，只映射包路径；三处符号由编译器指出，T-45 给改法 |

**2026-09-09 第二次发版**：kit v1.14.1（tag CI integration 首跑遇 Mongo 选主抖动，重跑绿）→ codegen v1.15.2（framework-release 全绿，8 个资产）。core 未发版：v1.15.0 之后只有测试、文档与 U-0123 的死代码删除。

## 0.2 第二次发版（2026-09-09 下午）

core v1.15.1（U-0146 流水线提交落盘即唤醒投影，T-49；U-0126～U-0139 测试）→ kit v1.14.2（U-0145 mail Get / GetMany 同判，T-48；U-0140～U-0143 测试）→ codegen v1.15.3（清单 core v1.15.1 / kit v1.14.2，source-head 默认 pin 同步）。三条 tag 的 `ci` 全绿（core 34318141953、kit 34318197054、codegen 34318266369）。

**一处失误**：kit 的 `go get roost-core@v1.15.1` 因 goproxy.cn 的 sumdb 暂时 404 失败，管道里的 `tail` 掩盖了退出码，回退分支没跑，kit v1.14.2 的 go.mod 仍 pin core v1.15.0，tag 已推不重打；kit 的修复不依赖 core v1.15.1，消费方按清单同时取两者即可，CHANGELOG 已更正说明，下次 kit 发版再升 pin。教训：发版脚本里的每一步不要接管道，或开 `pipefail`。

## 0.3 第三次发版（2026-09-09 傍晚）：docs/bug 四项 RR 的修复

core v1.15.2（U-0155 cache 等待名额归还，T-51；U-0157 接入文档）→ kit v1.14.3（U-0154 session Enter 账本 CAS，T-50；go.mod pin 升到 core v1.15.2，补上 v1.14.2 漏掉的那次）→ codegen v1.15.4（U-0156 consolidate 单行 import，T-52；清单 core v1.15.2 / kit v1.14.3）。修复记录见 [../bugfix/](../bugfix/README.md)。三条 tag 的 `ci` 全绿（core 34329421732、kit 34329677953、codegen 34330142658）。

kit 的依赖升级这次用 `GOPROXY=direct GONOSUMDB=github.com/tjbdwanghaibo` 绕开 goproxy.cn 的 sumdb 延迟，并开了 `pipefail`；CHANGELOG 的依赖说明因脚本里一处断言失败没随 tag 提交，随后补在 main。

## 0.4 第四次发版（2026-09-16）：core v1.15.3 / kit v1.14.4 / codegen v1.15.5

- core：U-0209～U-0216（review 09-15 第七轮至 09-16 第四轮登记的九项，含 U-0211 的 Windows 截断补修）、M-06（`service/match` 与 `servicemetrics` 自 kit 下沉）。
- kit：U-0191 / U-0192（activity）、U-0217（**破坏性**：`match.NewMod(reporter)`，删掉无执行者的 Grouping 注入）、account `RegistryBound` 钩子、M-06 kit 半（`service/match`、`service/servicemetrics` 改为 core 的别名包）。
- codegen：`-template game-demo` 九批、`add endpoint` 实体 id 修复、TCP 传输层 `RegistryBound`、match 不再生成 `Grouping()`。
- 顺序 core → kit → codegen，各仓 `scripts/pretag.sh` 全绿后打 tag；kit 的 integration 包里一处两参 `NewMod` 是 pretag 的 vet 抓到的（普通 `go test ./...` 因 build tag 跳过它）。
- 发版验证：对三个 tag 不带 go.work 生成 game-demo 并 build，抓到 codegen v1.15.5 的 U-0218（collaborators 无用 import，match 工程编译不过），当天补 codegen v1.15.6；据此把 framework-compat 的 demo scenario 排入 released。
- 遗留：ARCH-01 的 session / mail 下沉、ARCH-02 manager、ARCH-04 的生成器拆分（`Matchmaker` RPC 生成文件的 core / kit 归属）；U-0210 的 Kit 默认 Prefix durable 迁移演练（审查 §6）；Windows 截断补修以 CI `windows-compatibility` 为准。

## 0.5 第五次发版（2026-09-16 晚）：core v1.15.4 / kit v1.14.5 / codegen v1.15.7

- core：M-07（`service/mail`）、M-08（`service/session`）、M-09（`manager` 引擎）自 kit 下沉，领域测试随迁；`app/manager.go` 注释指向引擎。
- kit：依赖 core v1.15.4；`service/mail` / `service/session` 改别名包（精简 harness 留给 Mod / 传输测试），`manager.ManagerMod` 改为 `coremanager.Engine` 的包装（公开方法集不变，sentinel 同指针）；全部 9 个 `//roost:rpc` 传输用拆分后的生成器重生成为两半（M-10）。
- codegen：`servicerpc` 生成传输拆成 `<iface>_rpc_gen.go`（只依赖 core）与 `<iface>_rpc_assembly_gen.go`（Server / OwnerCapabilities / ClientMod，依赖 kit mods）；发布组合钉到 v1.15.4 / v1.14.5 / v1.15.7。
- 顺序 core → kit → codegen，三仓 pretag 全绿；发版验证：对三个 tag 不带 go.work 生成 game-demo，`go get` 显式钉 core / kit（proxy 对 kit 新 tag 有延迟，`project new` 解析到 v1.14.4），build / vet / test 全绿。
- 遗留：RPC 接口（`Mail` / `Session` / `Matchmaker`）连同传输半进 core 领域包，kit 只留装配半（需 core + kit 各一次发版）；ARCH-04 的 kit README 第 3～4 节逐包核对；U-0210 的 Kit 默认 Prefix durable 迁移演练；Windows 截断补修以 CI `windows-compatibility` 为准。

## 0.6 第六次发版（2026-09-16 夜）：core v1.15.5 / kit v1.14.6 / codegen v1.15.8

- codegen（先于 core 提交、随 codegen tag 发出）：`servicerpc -emit transport|assembly|all`、`-out`，`-dir` 接受 import path（`go list` 在 `-out` 模块上下文解析），文件头记录实际命令。
- core：M-11——`Mail` / `Session` / `Matchmaker` 接口文件与 `-emit transport` 生成的 `*_rpc_gen.go` 进 `service/{mail,session,match}`；mail 传输测试与 session `Capability` 断言随迁；边界测试绿。
- kit：依赖 core v1.15.5；删接口文件与 `*_rpc_gen.go`，`alias.go` 追加传输半别名并承载 `go:generate … -dir github.com/tjbdwanghaibo/roost-core/service/<x> -emit assembly -out .`；装配半重生成。
- 顺序 core → kit → codegen，三仓 pretag 全绿。ARCH-01 / ARCH-02 / ARCH-04 至此全部完成（ARCH-03 无需改动）。
- 遗留：U-0210 的 Kit 默认 Prefix durable 迁移演练；Windows 截断补修以 CI `windows-compatibility` 为准；其余六个 kit RPC 服务（account / chat / global / activity / platform / rank）的领域仍整体在 kit，不在本轮 ARCH 范围。

## 0.7 第七次发版（2026-09-17）：core v1.15.6 / kit v1.14.7 / codegen v1.15.9

- core：U-0225——saga 步骤给出不可重试失败（或重试用尽）后，进补偿的记录版本被加了两次，`MongoStore.Apply` 只接受 expected+1，
  于是 Mongo 存储上任何补偿都落不下去，saga 卡在 waiting、步骤按超时反复重发同一个拒绝；内存 store 只比对当前版本，单测一直绿。
  拆成"加一次版本"入口（`beginCompensation` / `retryOrCompensate`）与纯状态函数（`compensationState` / `retryState`）。
  同版本一并发出的还有 U-0219～U-0223（manager 交接、match Queue 键碰撞 / ScoreWindow 溢出 / 内存 Store 切片共享）与 U-0224 的记录。
- kit：只升 core pin（`saga.Mod` 只做装配与转发，本仓无代码改动）。
- codegen：game-demo 第十批的剩余部分——B6 GM 运维面（admin 命令表四条 + `gm.saga.get` / `gm.saga.list`，`player_id` 收唯一 id 或完整实体 id）、
  B9 送礼 saga（`add saga` 的第一个真实使用方：`SendGift` 在 Nest 事务里 `EmitStart`，四个 `SubscribeMongoStep` 步骤，`GiftStatus` 读协调器记录，
  机器人走完成路与补偿路）；通用修复：`add mod` / `add saga` 现在给已生成的配置补上新 Mod 的段（此前后加的 saga 用默认 8 GiB 建流、
  隔离环境起不来且配置里无段可改）；`add saga` 骨架改用非废弃的 `SubscribeMongoStep`。
- 顺序 core → kit → codegen，三仓 pretag 全绿。发版验证：对三个 tag 不带 go.work 生成 game-demo（kit 需显式 `go get` v1.14.7，proxy 对新 tag 有延迟），
  六进程 `run.sh start` 全 ready，6 机器人全链路通过，saga 记录 6 completed + 6 compensated。framework-compat 的 released × demo 单元格随本次发版恢复。
- 遗留：W-2026-09-17-04（原生 Nest saga 步骤的完成效果落在 `ROOST_EFFECTS`，协调器只订 `ROOST_SAGA`，无人消费）待 review 判定；
  W-2026-09-17-01～03 同样待判；U-0210 的 Kit 默认 Prefix durable 迁移演练；Windows 截断补修以 CI `windows-compatibility` 为准。

## 0.8 第八次发版（2026-09-18）：core v1.15.7 / kit v1.14.8 / codegen v1.15.10

一轮 bugfix 把 review 登记的**全部八条**未修复项收敛掉（09-17 第三轮五项 + 09-18 两项 + 09-17 第二轮的 platform 一项），
外加实施中自己撞上的一条生成器缺陷。

- core：U-0230 新增 `attribute` 包（属性系统的框架半：Meta / Profile / Selector / Snapshot / Container）；
  U-0231 Assembly 补第三个消费者，原生 Nest 步骤的完成效果此前无人消费、saga 永远 waiting；
  U-0233 房间与管理器配置接入 pipelined 提交的持久化水位（coordinator 一直有能力，没有入口）。
- kit：U-0234 platform 后台重试的候选来源做成显式可选 collaborator（`PendingOrders`，有界 + 可报错，
  经 `Mod.WithPendingOrders`）；saga Mod 读原生完成消费者的配置键；依赖 core v1.15.7。
- codegen：U-0226（P1）FinishDungeon 按权威 `run.State` 判定 + run id 账本；U-0227 nest handler 半不再 import
  只被返回类型用到的包；U-0228 battle 宽限期改独立计时器；U-0229 `sync=true` 生成物只写 Core 真有的字段；
  U-0230 生成器改出包级访问器 + 脚手架 runtime.go + **B8**（demo 用上 attribute profile）；U-0232 嵌套 DAO 第二层脏传播。
- 新增三条运行时门（entity sync / attribute / dao 脏传播用例），都针对"文本对、语义错"这类盲区。
- 顺序 core → kit → codegen，三仓 pretag 全绿。发版验证：对三个 tag 不带 go.work 生成 game-demo（kit 显式 go get），
  build / vet / test 全绿（含随工程生成的 dungeon、attribute、battle 三组测试）；隔离环境实跑 6 机器人全过、
  saga 6 completed + 6 compensated、dungeon claim 账本落库、game 日志 0 条 ERROR。
  framework-compat 的 released × demo 单元格随本次发版恢复。
- 遗留：Wanted-05 的"生成实体 → room → session → sink 可执行接入样例"仍待产品化；attribute 的层间合成未做；
  platform 待发货索引本身（持久段、重启续接、分页公平性）是部署的。

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
