# v1.23.0 实现文档（分册）：配置、skill 与非核心 review（CFG / SKILL / NONCORE）

本分册写给 review agent，也让人能读懂：每条给出提交、改动文件与关键符号（`path:line`，以代码冻结提交 `e6828e4f` 为准）、不变量与守卫测试、控制流、失败处理、修前红文本（原样抄自 `docs/bug` / `docs/bugfix` / 方案记录）、复跑命令、未验证项与 review 检查点。配套的说明文档是 [guide-cfg-skill-noncore.md](guide-cfg-skill-noncore.md)，同一编号互链。APP、OWN、CLK、OPS、TOOL、SAGA、DRV、DAO、REM 主题在同目录的其他分册里。

**重核说明（2026-10-06，按 `e6828e4f`）**：起草时以 `02c8a10d` 为准，冻结后逐条重核全部 `path:line`、函数名与测试名。方法：机器比对每个引用行在两个提交上的内容与相邻的符号名，再人工看没有符号可比的引用。结果：正文（不含修前红文本）约 490 处 `path:line`、约 165 个测试名逐个核对；`02c8a10d..e6828e4f` 期间本分册引用到的源文件只有 nest 与 `entity/remote_snapshot.go` 变了。共改 8 处：行号 3 处（NONCORE-14 两处、NONCORE-55 一处）；NONCORE-54 原有 3 处引用随 RR-20261006-12 重写换成现名现行号（`releaseDispatchLocks` 已删除）；已删除的符号 1 处（SKILL-2 `undoVitals` → `beginChange`）；已改名的测试 1 处（NONCORE-20 原回归 → `TestActivityRefusesAGroupNoWindowCouldOpenWith`）。其余引用在 `e6828e4f` 上与所述符号一致。修前红文本里的 `文件:行` 是当时的原文，不改。各条“未验证”只留外部环境项并指向 [外部验证清单](../../review/EXTERNAL-VERIFICATION-2026-10-06.md) 的 E 编号（Windows 一律“暂存，不保证正确”）；本机能做的已在 `e6828e4f` 上补跑并写进对应条目。与其他分册重复的条目（NONCORE-1、23、24、40、45、50、56）只保留索引，以对方分册为准。

## 怎么用这份文档 review

**建议顺序**：

1. 先读说明文档的“本部分总览”，知道哪些是破坏性变化。
2. CFG 的数据规则主线：CFG-7 → CFG-8 → CFG-10 → CFG-11（`configdata` 加载流水线是一处共享代码，先建立整体图）。再看严格读取 CFG-1 → CFG-2（守卫是源码扫描，要看扫描模式会不会漏）。
3. SKILL 的结构主线：SKILL-13（B3）→ SKILL-14 → SKILL-15（求值上下文表）→ SKILL-16（O33）。这四条决定了“编译接受 ⇒ Runtime 能执行”是否成立；其余 SKILL 条目是单点修复。
4. NONCORE 里改了契约的：NONCORE-35 / 37（actionflow 延后队列）、NONCORE-47（遍历回调契约）、NONCORE-21（activity 窗口条目入口、account 判定表）、NONCORE-52（停机三步）。
5. 其余条目按包查（文末“按包的改动索引”）。

**先读的 roost-coding 契约**（[SKILL.md](../../agent-skills/roost-coding/SKILL.md)）：

| 行 | 内容 | 用在 |
| --- | --- | --- |
| 20～27 | 写给人阅读、错误保留 `errors.Is`、指标标签低基数 | 全部 |
| 39 | A1 回滚统一走 DAO；B4 skill Runtime 不进事务的例外 | SKILL、NONCORE-25 / 26 / 39 |
| 63～80 | 生命周期复审要点：三步停机、A3 共用类型与契约骨架、C7 遍历回调契约、新增配置核对生成配置与读取方 | NONCORE-1 / 4 / 6 / 46 / 47 / 52、CFG-12 |
| 105 | 反复出问题要上报方向判断 | SKILL 主线、NONCORE-21 / 31 / 35 / 56 |
| 123～129 | 验证纪律：按影响面验证、示例要实跑、性能对照 | 全部 |

**本地复跑环境与命令**（全部 `GOWORK=off`，在仓库根执行）：

```sh
gofmt -l <改动的 .go 文件>                       # 应为空
go build ./... && go vet ./...
go test -count=1 .                                # 根包：边界、文档链接、冲突标记、示例实跑、CI 门禁
go test -race -count=3 ./app ./kit/... ./configdata/... ./codegen/internal/tablegen/ ./codegen/internal/cfggen/   # CFG
go test -race -count=3 ./skill/...                # SKILL（见 SKILL 节开头的完整清单）
go test -count=1 ./codegen/...                    # 生成形状（较慢）
ROOST_CORE_DIR=$PWD sh codegen/scripts/tablegen-runtime.sh -race      # tablegen 运行期门（用本地 core）
ROOST_CORE_DIR=$PWD sh codegen/scripts/cfggen-golden-runtime.sh -race # cfggen 运行期门
go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync   # 改了 nest / entity / dataengine / sync 时
go run ./cmd/glsvet -tests ./nest                 # NONCORE-54 之后应无输出
```

生成工程验证：用当前 CLI 生成 game-demo（`roost project new … -template game-demo`），`go mod edit -replace` 到本仓、DAO 库名常量改成唯一名字，再 `go build ./... && go vet ./... && go test ./...`。

**真实依赖与不能碰的共享资源**（A5 规则，NONCORE-51）：

- 隔离环境 `~/.roost-it/roost-dataengine-it`；`env.sh` 含凭据，只 `source`、不打印。
- 真实依赖用例一律加 `-run` 只跑自己的；故障注入自建代理或进程，**不 `/reset` 共享 toxiproxy**，不跑 `dataengine-env.sh` 的 up / down / reset / heal / fault（全局命令运行期间持有 `remote-acceptance.lock`）。
- `remote-acceptance.lock` 存在时不跑真实依赖用例；资源（Mongo 库、JetStream 流、Redis 键、etcd 前缀）用自己的前缀，用完删除。
- 不碰主 checkout 及其 `artifacts/`。
- 本分册的条目大多不需要真实依赖；需要的在各条“测试”里写了 `-tags integration` 与用例名。

## 条目总表

| 编号 | 一句话 | 首发版本 | 行为变化 / 兼容破坏 | 需业务改动 |
| --- | --- | --- | --- | --- |
| [CFG-1](#cfg-1) | 布尔开关与时长严格读取（NC-190） | v1.20.2 | 收紧：`on` / 无单位时长启动即报错 | 写错类型的配置要改 |
| [CFG-2](#cfg-2) | 框架配置一律严格读取，启动校验覆盖全部类型化键（A4） | v1.20.2 | 收紧：类型错误启动即点名报错 | 同上；升级前用新版本启动一次 |
| [CFG-3](#cfg-3) | kit/redis 三个整数键严格读取 | v1.20.2 | 收紧（只影响绕过 App 直接装配 Mod） | 否 |
| [CFG-4](#cfg-4) | 生成的 player TCP / RPC 客户端严格读取 | v1.20.2 | 收紧；生成器 Core 下限升到 v1.20.2 | 已生成工程不迁移 |
| [CFG-5](#cfg-5) | `env: production` 只校验有读取方的设置（C1 / NC-192） | v1.20.2 | 放宽 | 否（上线前自己接入限流 / 鉴权） |
| [CFG-6](#cfg-6) | tablegen `ref` 加载时检查、`-check` 按 schema（NC-75） | v1.20.1 | 收紧：悬空 ref 加载失败 | 修正悬空数据 |
| [CFG-7](#cfg-7) | 配置规则统一由运行时加载层强制（B10） | v1.21.0 | **破坏**：违规数据启动 / reload 失败；生成器 Core 下限 v1.21.0 | 修正数据；重新 generate 才有运行时规则 |
| [CFG-8](#cfg-8) | 热更失败与回滚可见（C2） | v1.21.0 | **破坏**：`configdata.reload.total` 标签变化 | 调整依赖 `reason` / `result="rollback"` 的看板 |
| [CFG-9](#cfg-9) | 规则层大小写变体取最后一个（发版前审查） | v1.21.0（v1.22.0 删除） | 被 CFG-10 取代 | 否 |
| [CFG-10](#cfg-10) | configdata 键大小写敏感 | v1.22.0 | **破坏**：只差大小写的键整次拒绝 | 改成声明的拼写 |
| [CFG-11](#cfg-11) | cfggen globals 支持 required / min / enum | v1.23.0（本版） | 新能力；已有输出不变 | 否（可选） |
| [CFG-12](#cfg-12) | 生成配置写出 `remote_entity` 新键（A8） | v1.23.0（本版） | 只影响新生成工程 | 否 |
| [CFG-13](#cfg-13) | 生成 TCP 越界报错逐条点名（A9） | v1.23.0（本版） | 只改报错文本 | 按旧文本匹配的脚本要更新 |
| [SKILL-1](#skill-1) | 施法失败只走一个终态入口（NC-110～112） | v1.20.1 | 收紧：终态 cast 的输入被拒；手动 Release 失败不可重试 | 否 |
| [SKILL-2](#skill-2) | Combatant 副本不共享 map（NC-113） | v1.20.1 | 改副本不再改实体 | 误用者改用 `InitCombatant` |
| [SKILL-3](#skill-3) | skillsync 三条下发路径同一可见性（NC-114 / 115） | v1.20.1 | wire 追加字段；可见性变化时 remove 不下发 | 可见性变化时重发快照 |
| [SKILL-4](#skill-4) | Applier 被拒包不改 epoch（NC-116） | v1.20.1 | 无 | 否 |
| [SKILL-5](#skill-5) | 提交前失败的 cast 有界回收（NC-117） | v1.20.1 | 无 | 否 |
| [SKILL-6](#skill-6) | wire 字段名逐字匹配（NC-150） | v1.20.2 | 收紧：非规范大小写 Parse 失败 | 修正技能 JSON |
| [SKILL-7](#skill-7) | 拒绝 Runtime 不派发的 phase 事件（NC-151） | v1.20.2 | 收紧；非零 `timeout_ticks` 给 warning | 修正定义；loadtest 断言 `Warnings == 0` |
| [SKILL-8](#skill-8) | 作者写的 tick 非负（NC-152） | v1.20.2 | 收紧 | 修正负值 |
| [SKILL-9](#skill-9) | 表现缓存等待者、skillcompose 诊断（NC-153 / 154） | v1.20.2 | 无 | 否 |
| [SKILL-10](#skill-10) | 编译器只接受 Runtime / Host 执行的范围（NC-210、212～215） | v1.20.2 | 收紧 | 修正定义与 catalog |
| [SKILL-11](#skill-11) | 移交后 area finish 只结束本进程（NC-211） | v1.20.2 | 放宽（不再报错） | 否 |
| [SKILL-12](#skill-12) | `NegotiateSchema` 拒绝空区间（NC-216） | v1.20.2 | 收紧 | 否（无生产调用方） |
| [SKILL-13](#skill-13) | lower fail-fast 与 phase 事件表单一来源（B3） | v1.20.2 | 正常定义不变；引入一处回归（v1.21.0 修） | 否 |
| [SKILL-14](#skill-14) | 按 Runtime 求值上下文收紧，修 B3 回归（NC-220～224） | v1.21.0 | 收紧；NC-223 恢复 | 修正定义；v1.20.1 的相关 checkpoint 先排空 |
| [SKILL-15](#skill-15) | 求值上下文表（第五轮，NC-280～283） | v1.21.0 | 收紧 + 放宽；诊断码变化 | 修正定义；按诊断码匹配的工具更新 |
| [SKILL-16](#skill-16) | O33 漂移格子编译期拒绝；O34～O36 写文档 | v1.21.0 | **破坏**：以前能编译的定义启动失败 | 按作者文档改写 |
| [SKILL-17](#skill-17) | summon 进程拒绝 `duration_ticks` 与 area 成员字段（O22） | v1.23.0（本版） | 收紧 | 删掉这些字段 |
| [SKILL-18](#skill-18) | checkpoint 字节确定（O7） | v1.23.0（本版） | 字节变、格式不变 | 不要跨版本比较字节 |
| [SKILL-19](#skill-19) | result 分支诊断文案（O29）；O15～O17 / O27 / O28 写文档 | v1.23.0（本版） | 只改文案 | 按文案匹配的工具更新 |
| [SKILL-20](#skill-20) | null 默认值实体状态可以 set（RR-20261006-02） | v1.23.0（本版） | 放宽；事件 Before 的缺省值类型变化 | 否 |
| [SKILL-21](#skill-21) | checkpoint 恢复拒绝 `phase_timeout`（RR-20261006-03） | v1.23.0（本版） | 只拒绝不会出现的任务 | 否 |
| [SKILL-22](#skill-22) | 示例 `statusbridge` 能运行 | v1.23.0（本版） | 只改示例 | 否 |
| [NONCORE-1](#noncore-1) | N01 留项：Ops 同步 bind、停机 hook 预算、停机期 fail-stop 退出码、Mod 停止收尾、`ops.admin_timeout`（NC-230～234） | v1.21.0 | 收紧：Ops 端口被占启动失败；admin 命令 10s 期限 | 同机多实例各配 `ops.addr`；长命令调大 `ops.admin_timeout` |
| [NONCORE-2](#noncore-2) | HTTP JSON 先编码后写；recover 尊重已开始的响应（NC-80 / 81） | v1.20.1 | 编码失败回 500 | 否 |
| [NONCORE-3](#noncore-3) | RateLimiter 每主体 key 上限（NC-82） | v1.20.1 | 行为变化：每主体默认 256 key | 否 |
| [NONCORE-4](#noncore-4) | 生成 TCP 接入停机可重试（NC-83） | v1.20.1 | 生成形状 | 重新生成 `server_gen*.go` |
| [NONCORE-5](#noncore-5) | 生成 TCP“handler 不配合 ctx”用例（A15） | v1.23.0（本版） | 只加测试 | 否（handler 须配合 ctx） |
| [NONCORE-6](#noncore-6) | Bus JetStream RPC 停止排空 / 拒绝回包 / 截获（NC-90～92） | v1.20.1 | 收紧：截获的轻量 RPC 不执行 | 两端 `nats.rpc.transport` 一致 |
| [NONCORE-7](#noncore-7) | etcd `Resign` 按调用方期限（NC-93） | v1.20.1 | `Resign` 可能返回 ctx 错误 | 否 |
| [NONCORE-8](#noncore-8) | bus SETNX 去重保持、写进契约 | v1.23.0（本版） | 只改注释 | 否 |
| [NONCORE-9](#noncore-9) | 迁移输出 WAL 准入前验证（NC-31） | v1.20.0 | 收紧 | 手写迁移候选须提供 loader / Id |
| [NONCORE-10](#noncore-10) | 生成 DAO 深层嵌套修改进入提交（NC-32） | v1.20.0 | 生成形状 | 重新生成 DAO / nested |
| [NONCORE-11](#noncore-11) | versionstore 退避后重读（NC-52） | v1.20.1 | 伪冲突减少 | 否 |
| [NONCORE-12](#noncore-12) | mongotest 数组唯一键、`$in` 具名切片（NC-102、RR-20261006-08） | v1.20.1 / v1.23.0（本版） | 只影响测试替身 | 否 |
| [NONCORE-13](#noncore-13) | 缓存副本与 interest 身份绑定（NC-33 / 34） | v1.20.0 | 收紧 | 否 |
| [NONCORE-14](#noncore-14) | 权威快照身份与最终最低版本（NC-35 / 36） | v1.20.0 | 收紧 | 否 |
| [NONCORE-15](#noncore-15) | L2 CAS 落败、删除墓碑、Stats 清理（NC-130 / 131） | v1.20.2 | 收紧；L2 键格式增字段 | 否 |
| [NONCORE-16](#noncore-16) | saga 三消费者健康与 Resume 持久代际（NC-37 / 38） | v1.20.1 | 持久格式增量 | 协调器统一升级 |
| [NONCORE-17](#noncore-17) | saga 启动身份与完成路由（NC-39 / 40） | v1.20.1 | 收紧：缺摘要的旧记录重投冲突 | 自定义 Store 保存新字段 |
| [NONCORE-18](#noncore-18) | saga outbox 领取与 activity 恢复校验（NC-41 / 42） | v1.20.1 | 收紧 | 否 |
| [NONCORE-19](#noncore-19) | account 换名释放死计划、activity 确认键校验（NC-50 / 51） | v1.20.1 | 行为变化 | 否 |
| [NONCORE-20](#noncore-20) | `activity.game_sids` 启动校验（RR-20261005-01；C4 后由组文件兑现、回归改名） | v1.20.1（v1.20.2 被 C4 取代） | 已取代 | 否 |
| [NONCORE-21](#noncore-21) | activity 窗口条目统一入口与修复入口；account 判定表（B9） | v1.21.0 | 坏条目不再交出；新 Admin 入口 | 否（运维可用新入口） |
| [NONCORE-22](#noncore-22) | account 换名释放未 admitted 计划（第五轮、O37） | v1.21.0 | 行为变化 | 否 |
| [NONCORE-23](#noncore-23) | saga 定义缺失 fence 时退避中步骤记为放弃（NC-250） | v1.21.0 | 迟到成功告警 | 否 |
| [NONCORE-24](#noncore-24) | global `Bind` 同参重试幂等（RR-20261006-05） | v1.23.0（本版） | 放宽 | 否 |
| [NONCORE-25](#noncore-25) | N07 第一批：快照锁、属性层回滚、attribute 生成器、errcode 扫描（NC-60～63） | v1.20.1 | 收紧：非字面量 errcode 报错 | 常量编号改字面量 |
| [NONCORE-26](#noncore-26) | demo 开关热更说明、加载时重建 Gear（NC-64 / 65） | v1.20.1 | 模板 | `roost project sync` |
| [NONCORE-27](#noncore-27) | 生成器不改进程工作目录（RR-20261004-12） | v1.20.0 | 两行提示变绝对路径 | 否 |
| [NONCORE-28](#noncore-28) | go 命令按进程树取消（RR-20261004-13 与补修） | v1.20.0 / v1.20.1 | 无 | 否 |
| [NONCORE-29](#noncore-29) | N08：中断清理、预览、cfggen 帮助、id 工具（NC-70～73） | v1.20.1 | 无 | 否 |
| [NONCORE-30](#noncore-30) | 运行期目录不算生成输入（NC-74） | v1.20.1 | 无 | 否 |
| [NONCORE-31](#noncore-31) | CLI 入口统一接管信号（B6）；联网用例门（C9） | v1.21.0 | 中断先回滚再以同一信号退出 | 否 |
| [NONCORE-32](#noncore-32) | DAO `//roost:dao nocoll` | v1.20.0 | 新能力 | 可选迁移 |
| [NONCORE-33](#noncore-33) | 文件 outbox 清理崩溃遗留的临时文件（RR-20261006-04） | v1.23.0（本版） | 打开时删除精确匹配的临时文件 | 同一目录不能有两个在写的 store |
| [NONCORE-34](#noncore-34) | N10 第一批（NC-120～123） | v1.20.1 | 行为变化：丢弃排队项发 OnEnded | 否 |
| [NONCORE-35](#noncore-35) | actionflow 回调变更进延后队列（B7） | v1.20.2 | **破坏**：`ActionRunner` 回调语义变化 | 按延后语义改写回调 |
| [NONCORE-36](#noncore-36) | N10 第二批（NC-240～247） | v1.21.0 | 行为变化：ai 回调里切换延后 | 否 |
| [NONCORE-37](#noncore-37) | MissionRunner 延后队列；EndAll 清场文档 | v1.21.0 | **破坏**：`MissionRunner` 回调语义变化 | 按延后语义改写；清场先 `EndCurMission` |
| [NONCORE-38](#noncore-38) | ai O-T3 / O-T4 写文档 | v1.23.0（本版） | 只改注释 | 否 |
| [NONCORE-39](#noncore-39) | N11 第一批：timer / spatial / index（NC-140～147） | v1.20.1 | 行为变化：Tick 期间取消 / 改期立即生效 | 模板 `roost project sync` |
| [NONCORE-40](#noncore-40) | timer 同期限按 priority / 登记顺序；未注册类型可见（D-L1 / D-L2） | v1.21.0 | 行为变化 | 模板 `roost project sync` |
| [NONCORE-41](#noncore-41) | `PathFindSystem.Stop` 数据竞争（NC-270） | v1.21.0 | 生成形状 | 模板 `roost project sync` |
| [NONCORE-42](#noncore-42) | failurelog 结果未知不降级（NC-160） | v1.20.2 | 收紧 | 否 |
| [NONCORE-43](#noncore-43) | robot / statslog / log（NC-161～165） | v1.20.2 | 收紧：loadtest 无样本判失败 | 否 |
| [NONCORE-44](#noncore-44) | robot 会话 / 日志 sink / Prometheus 转义等（NC-261～266） | v1.21.0 | 行为变化 | 否 |
| [NONCORE-45](#noncore-45) | robot Stage 序号只增不回收（RR-20261006-09） | v1.23.0（本版） | 行为变化 | 自定义 `IdentityProvider` 覆盖超出 `Count` 的序号 |
| [NONCORE-46](#noncore-46) | N13 遍历与 TaskPool 等（NC-180～185） | v1.20.2 | 行为变化 | 否 |
| [NONCORE-47](#noncore-47) | 遍历回调仓库级契约（C7） | v1.20.2 | 契约成文 | 否 |
| [NONCORE-48](#noncore-48) | container / goroutine 零调用方 API（NC-267～269） | v1.21.0 | 无 | 否 |
| [NONCORE-49](#noncore-49) | Mongo URI 口令脱敏（NC-191） | v1.20.2 | 无 | 否 |
| [NONCORE-50](#noncore-50) | 启动失败先收回 Service（NC-193） | v1.20.2 | 契约补充 | `Shutdown` 须容忍部分初始化 |
| [NONCORE-51](#noncore-51) | N15 脚本与门禁（NC-200～208）、A5 | v1.20.2 | 收紧：glsvet 对没检查到的输入退出 2 | 已有工程 `.gitignore` 补 `/data/wal/` |
| [NONCORE-52](#noncore-52) | 停机三步（NC-170～174） | v1.20.2 | 收紧 | 否 |
| [NONCORE-53](#noncore-53) | Mongo Mod 停止收敛（NC-260） | v1.21.0 | 重复 Close 返回 nil | 否 |
| [NONCORE-54](#noncore-54) | nest 用例隔离（A2 / A3）；派发取锁要求 Guard 作用域（RR-20261006-12，原 W-2026-10-06-01） | v1.23.0（本版） | 生产行为不变；nest 内部派发函数在没有 Guard 作用域时返回错误 | 否 |
| [NONCORE-55](#noncore-55) | Nest 重排抖动（U-0279） | v1.20.1 | 平均重排延迟 5ms → 7.5ms | 否 |
| [NONCORE-56](#noncore-56) | 租约修复 RR-20261004-10 / 11 / 14（同版被静态绑定取代） | v1.20.0 | 代码已删 | 否 |

共 91 条：CFG 13 条、SKILL 22 条、NONCORE 56 条。

### 提交速查

| 编号 | 主要提交（实施 / 补修；记录与标注提交见各条） |
| --- | --- |
| CFG-1 | `f9367785` |
| CFG-2 | `3e3350d5`（后续登记新键：`7d49e54d`、`491aaf3b`、`2c1c7be7`、`23e17d81`、`a6985cf3`、`db67b8ee`） |
| CFG-3 | `5df60765` |
| CFG-4 | `914a725f` |
| CFG-5 | `3e3350d5` |
| CFG-6 | `45d4bc1c` |
| CFG-7 / CFG-8 | `b12216ed` |
| CFG-9 | `5a3c4a60`（v1.22.0 由 `c474a6ef` 删除） |
| CFG-10 | `c474a6ef` |
| CFG-11 | `229a5aa0` |
| CFG-12 / CFG-13 | `fcc78ad0` |
| SKILL-1 / 2 | `855c2a38` |
| SKILL-3 / 4 / 5 | `f37a94e3` |
| SKILL-6～9 | `bfd353c0` |
| SKILL-10～12 | `7cf86f98` |
| SKILL-13 | `023eb276` |
| SKILL-14 | `5c04726f` |
| SKILL-15 | `4ed038d9` |
| SKILL-16 | `f6043e44` |
| SKILL-17～19、22 | `229a5aa0` |
| SKILL-20 / 21 | `b8fbcee0` |
| NONCORE-1 | `2c1c7be7` |
| NONCORE-2 | `c8d72122`、`eef7822e` |
| NONCORE-3 | `eef7822e` |
| NONCORE-4 | `c8d72122` |
| NONCORE-5 | `fcc78ad0` |
| NONCORE-6 | `ae742984`、`64179ad5`、`25646001` |
| NONCORE-7 | `89a102db` |
| NONCORE-8 | `88f33776` |
| NONCORE-9 | `c3aa0edd` |
| NONCORE-10 | `b2232db5` |
| NONCORE-11 | `be7bcc18` |
| NONCORE-12 | `81659082`、`611d5d72` |
| NONCORE-13 | `be4eb0fa` |
| NONCORE-14 | `7949da08` |
| NONCORE-15 | `6f06f0da`、`c3475150`、`366058a7` |
| NONCORE-16 | `47fca740` |
| NONCORE-17 | `12726715` |
| NONCORE-18 | `10c73e0c` |
| NONCORE-19 | `be7bcc18` |
| NONCORE-20 | `46c4dfba`（v1.20.2 被 `277e1252` 取代；回归去向复核 `d5682dc4`） |
| NONCORE-21 | `bd6df5e5` |
| NONCORE-22 | `b18d5613`、`f6043e44` |
| NONCORE-23 | `31b48bc0` |
| NONCORE-24 | `611d5d72` |
| NONCORE-25 | `3d4fe9f3` |
| NONCORE-26 | `197f7bb9` |
| NONCORE-27 | `a646193c` |
| NONCORE-28 | `7ffa7199`、`cb11be90` |
| NONCORE-29 | `80802200` |
| NONCORE-30 | `45d4bc1c` |
| NONCORE-31 | `f556049c` |
| NONCORE-32 | `2a82d826`、`b680ea26` |
| NONCORE-33 | `b8fbcee0` |
| NONCORE-34 | `556d156d` |
| NONCORE-35 | `a9b7075b` |
| NONCORE-36 | `44964553` |
| NONCORE-37 | `f6828f17` |
| NONCORE-38 | `88f33776` |
| NONCORE-39 | `23a10f42` |
| NONCORE-40 | `5abae51e` |
| NONCORE-41 | `36220f34` |
| NONCORE-42 | `f750ce43` |
| NONCORE-43 | `5fea59ce`、`efeede0f`、`92547035`、`2a9e0c2c`、`e798a759` |
| NONCORE-44 | `36220f34` |
| NONCORE-45 | `7b73aabc` |
| NONCORE-46 | `7e4ed438`、`20400337`、`1d600b9b`、`4c26b4b5`、`815c3661` |
| NONCORE-47 | `cd43a5ac` |
| NONCORE-48 | `36220f34` |
| NONCORE-49 | `e1a6b01d` |
| NONCORE-50 | `d6550a16` |
| NONCORE-51 | `6c1538be`、`d43aa3ba`、`3e3350d5` |
| NONCORE-52 | `c99a687d`（NC-173 补修 `50f2ac2a`） |
| NONCORE-53 | `36220f34` |
| NONCORE-54 | `611d5d72`（A2 / A3）、`b7471ae4`（RR-20261006-12） |
| NONCORE-55 | `47a9132c` |
| NONCORE-56 | `18bb86ae`、`a28a3152`、`890abdda`（同版 `f051e24a` 删除） |

## CFG：配置读取、配置数据规则与生成配置

先读：roost-coding“写给人阅读的代码”第 27 行（指标标签低基数、错误保留 `errors.Is` 语义）与第 80 行（新增配置要核对生成配置和运行时实际读取）。本主题的两条主线各有一组守卫：

- 严格读取：`app/config_strict_reads_promises_test.go`、`app/config_types_promises_test.go` 扫描 app、kit 与两份生成模板的源码。新增宽松读取或没登记的键会让它们点名失败。
- 规则单一来源：`configdata/rules` 是唯一实现；根包 `TestSharedConfigRulesStayALeaf`（`dependency_boundary_test.go:209`）保证它只依赖标准库，因为 codegen 也 import 它。

<a id="cfg-1"></a>
### CFG-1 布尔开关与时长严格读取（NC-190）

> 其他分册对应：APP-1 提到 `singleton.enabled` 的严格布尔（NC-190），本条为主。

[说明](guide-cfg-skill-noncore.md#cfg-1)

1. **提交与版本**：`f9367785`（修复）、`0aa2e1b9`（N14 收口文档）；审查 `efe219c1`。首发 v1.20.2。
2. **改动与符号**（以 `e6828e4f` 为准）：

   | 位置 | 职责 |
   | --- | --- |
   | `app/config_values.go:30` `ConfigBool` | 严格布尔：YAML 布尔、`ParseBool` 字符串、整数 0 / 1；其余点名报错 |
   | `app/config_values.go:55` `ConfigDuration` | 严格时长：`ParseDuration` 字符串、`time.Duration`、0；不带单位的非零数字报错 |
   | `app/singleton.go:134-172` `singletonSettings.readErrs` / `readSingletonSettings` | 读取错误先记下，`validate` 先报它们，不受 `enabled` 读成什么影响 |
   | `app/config_validation.go:17` `ValidateServiceConfig` | 对 `frameworkBoolKeys` 严格检查（NC-190 时 15 个，A4 后扩为三份登记，见 CFG-2） |
   | `kit/mods/service_servicemods.go:112` `Duration`、`:130` `RequiredDuration` | 服务 Mod 的时长改走 `app.ConfigDuration` |
   | `kit/saga/step_budgets.go:136` | 步骤预算时长改走 `app.ConfigDuration` |
   | `kit/mods/service_servicemods.go:68` `RedisClusterAddrs` | 逗号串或 YAML 列表、去空白；Redis Mod（`kit/redis/redis_mod.go:73`）、`ValidateClusterKeyPrefix`、remoteentity hash tag 检查都经它 |

3. **不变量**：已设置的框架布尔 / 时长键要么严格解析成功，要么启动失败并点名键；没有“读成零值继续跑”的分支。守卫：`TestEveryFrameworkBoolSwitchIsCheckedStrictly`（`app/config_types_promises_test.go:93`）扫描 `GetBool("…")`，新开关忘了登记即点名失败。
4. **控制流**：`App.Run` → `ValidateServiceConfig`（`app/app.go:134`，任何 Mod Init 之前）→ 单实例锁读配置 → Mod Init（kit 的 `mods.Duration` 等再严格读一遍）。
5. **失败处理**：读取错误汇总后返回，进程不启动；未设置的键返回零值，调用方照常“≤0 取默认”。
6. **测试**：
   - 修前红（基线 `f6245613`，[nc190-red.txt](../../review/evidence/noncore-review-20261005-n14/nc190-red.txt) 原文节选）：

     ```text
     --- FAIL: TestValidateServiceConfigRejectsABoolSwitchThatIsNotABool/singleton_on (0.00s)
         config_types_promises_test.go:40: ValidateServiceConfig = <nil>; want an error naming singleton.enabled (viper reads the value as false and the switch is silently off)
     --- FAIL: TestValidateServiceConfigRejectsADurationWithoutAUnit/singleton_all_unitless (0.00s)
         config_types_promises_test.go:68: ValidateServiceConfig = <nil>; want an error naming singleton.ttl
     --- FAIL: TestServiceDurationsRefuseValuesWithoutAUnit (0.00s)
         config_types_promises_test.go:21: Duration(claim_ttl: 30) = 30ns, <nil>; want an error naming mail.claim_ttl
     --- FAIL: TestStepBudgetDurationsRefuseValuesWithoutAUnit (0.00s)
         config_types_promises_test.go:14: timeout: 5 -> {Timeout:5ns MaxAttempts:5 BackoffMin:100ms BackoffMax:5s}, <nil>; want an error naming saga.step_defaults.timeout
     --- FAIL: TestClusterAddrsAcceptAYAMLListAndTrimEntries (0.00s)
         config_types_promises_test.go:23: cluster_addrs: a:1, b:2 -> cluster=true addrs=["a:1" " b:2"], want Cluster [a:1 b:2]
     ```

   - 修后：上述用例与 `…AcceptsTheBoolSpellingsItAlwaysAccepted`（`app/config_types_promises_test.go:46`，负对照：合法写法照常接受）、`…AcceptsDurationsWithUnits`（`:74`）通过。
   - 复跑：`GOWORK=off go test -count=1 -run 'BoolSwitch|DurationWithoutAUnit|DurationsWithUnits|BoolSpellings|FrameworkBoolSwitch' ./app` 与 `./kit/mods ./kit/saga ./kit/redis` 同名用例。
7. **性能**：无（只在启动时读）。
8. **未验证**：真实 Redis Cluster 下 YAML 列表 `cluster_addrs` 起服（外部 E08）。
9. **review 检查点**：
   - `ConfigBool` 是否仍接受 `1` / `0` / `"true"` / `True`（兼容承诺），并拒绝 `on` / `yes`？看 `app/config_values.go:30` 与 `config_types_promises_test.go:46`。
   - `readSingletonSettings` 的错误是否在 `enabled` 判断之前返回（`app/singleton.go:171`）？若有人把 `if !enabled { return nil }` 移到前面，`singleton_on` 用例应变红。
   - `RedisClusterAddrs` 的所有调用点是否都改了：`rg 'cluster_addrs' kit` 中不应再有 `GetString("redis.cluster_addrs")`。

<a id="cfg-2"></a>
### CFG-2 框架配置一律严格读取（A4）

[说明](guide-cfg-skill-noncore.md#cfg-2)

1. **提交与版本**：`3e3350d5`（app + kit 严格读取，与 C1 同批）、`61fb0894`（DECISIONS-PENDING 标注）；后续登记新键：`7d49e54d`（B2 键）、`491aaf3b`（C6 `service_metrics.enabled`）、`2c1c7be7`（`ops.admin_timeout`）、`23e17d81`（O4）、`a6985cf3`（Mirror 第 5 步）、`db67b8ee`（O-M6-3）。首发 v1.20.2。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `app/config_values.go:91` `ConfigInt`、`:103` `ConfigInt64` | 严格整数：YAML 整数、无小数部分的浮点、十进制字符串 |
   | `app/config_values.go:135-172` `ConfigReader`（`NewConfigReader`、`Bool` / `Duration` / `Int` / `Int64`、`Err`） | 一次读多键，错误汇总；读失败的键返回零值 |
   | `app/config_validation.go:255` `frameworkBoolKeys`、`:264` `frameworkDurationKeys`、`:304` `frameworkIntKeys` | 三份登记。发版提交上 17 / 99 / 79 个键；A4 方案写的是实施当时的 16 / 95 / 77（**文档与源码差异**：之后各批新增 `service_metrics.enabled`、`ops.admin_timeout`、`remote_entity.cached_max_staleness`、`remote_entity.mirror.shutdown_timeout`、`remote_entity.snapshot_l2_tombstone_wait_timeout`、`remote_entity.snapshot_interest_per_consumer`、`remote_entity.snapshot_l2_tombstone_wait_replicas`） |
   | `app/config_validation.go:347` `checkFrameworkConfigTypes` | 按三份登记加 syncbus 三段与 `<service>.call_timeout` 后缀逐个严格读；与语义检查重复的同一条错误只报一次（`uniqueErrors`，`:76`） |
   | kit 各 Mod | `dataengine`、`remoteentity`、`saga`、`nats`、`nest`、`syncbus`、`mongo`、`ops`、`etcd`、`statslog`、`mods.ResolvePersistenceEngine`、`service/platform`、`service/global/activity` 改用 `app.ConfigReader`，变量按约定叫 `read` |

3. **不变量与守卫**（`app/config_strict_reads_promises_test.go`）：
   - `TestFrameworkCodeDoesNotReadConfigLeniently`（`:140`）：app、kit 非测试源码与两份生成模板出现 `GetBool` / `GetDuration` / `GetInt*` / `GetUint*` / `GetFloat*` / `GetSizeInBytes` 即失败；例外只有 cobra 的 `Flags().` 与键 `sid`。
   - `TestEveryFrameworkDurationAndIntKeyIsCheckedStrictly`（`:125`）：按读取形式（`GetX`、`ConfigX`、`read.X`、`mods.Duration`、syncbus `cfgX(cfg, read, …)`、player TCP `read.X(key + "…")`）扫出键，没登记就点名失败；扫到的读取少于 150 处时认为扫描模式失效、直接失败。
   - 守卫做过变异：往 kit 加一处 `cfg.GetBool("stats_log.new_switch")`，两条守卫都点名失败（见 NC-192 记录）。
4. **控制流**：同 CFG-1；Mod Init 里 `read := app.NewConfigReader(cfg)` → 读全部键 → 语义检查前 `if err := read.Err(); err != nil { return … }`。
5. **失败处理**：启动校验一次报全；Mod Init 用各自的错误前缀。
6. **测试**：
   - 修前红（[a4-red.txt](../../bugfix/evidence/a4-config-20261005/a4-red.txt)、[kit-red.txt](../../bugfix/evidence/a4-config-20261005/kit-red.txt) 节选）：

     ```text
     --- FAIL: TestValidateServiceConfigRejectsFrameworkValuesOfTheWrongType/nest_worker_num_suffix (0.00s)
         a4_red_test.go:32: ValidateServiceConfig = <nil>; want an error naming nest.worker_num
     --- FAIL: TestValidateServiceConfigRejectsFrameworkValuesOfTheWrongType/wal_queue_fraction (0.00s)
         a4_red_test.go:32: ValidateServiceConfig = <nil>; want an error naming dataengine.wal.queue_capacity
     --- FAIL: TestValidateServiceConfigRejectsFrameworkValuesOfTheWrongType/etcd_lease_bool (0.00s)
         a4_red_test.go:32: ValidateServiceConfig = <nil>; want an error naming etcd.lease_ttl
     （共 14 个子用例同形）
     --- FAIL: TestKitModsRefuseConfigValuesOfTheWrongType/remote_cache_entries_suffix (0.00s)
         strict_config_promises_test.go:44: Init = <nil>; want an error naming remote_entity.snapshot_cache_entries
     --- FAIL: TestKitModsRefuseConfigValuesOfTheWrongType/dataengine_pipelined_async_yes (0.00s)
         strict_config_promises_test.go:44: Init = <nil>; want an error naming nest.pipelined.async
     （共 6 个子用例同形）
     ```

     说明：修前红用例文件名是 `a4_red_test.go`（临时文件），入库后同一断言在 `app/config_strict_reads_promises_test.go:20` `TestValidateServiceConfigRejectsFrameworkValuesOfTheWrongType`；kit 侧是 `kit/strict_config_promises_test.go:21`。
   - 修后：上述用例、`TestConfigIntAcceptsWholeNumbersOnly`（`:82`）、`TestConfigReaderReportsEveryBadKeyAtOnce`（`:105`）、负对照 `TestValidateServiceConfigAcceptsFrameworkValuesWrittenCorrectly`（`:48`）通过；生成工程三份配置（game-demo 与 CI full 场景）在严格校验下全部通过。
   - 复跑：`GOWORK=off go test -count=1 ./app ./kit`；变更影响面 `go test -race -count=3 ./app ./kit/...`。
7. **性能**：无。
8. **未验证**：无。真实依赖上的 Init 路径已由之后每版的发版矩阵覆盖（v1.20.2 / v1.21.0 / v1.22.0 各 21/21，`kit/dataengine`、`remoteentity` 的 `TestReal*` 用例经严格读取装配 Mod，见 [交接 §7](../../CORE-OPTIMIZATION-HANDOFF.md) 各版条目）。
9. **review 检查点**：
   - 发版提交上 `TestFrameworkCodeDoesNotReadConfigLeniently` 的例外是否只剩 `sid` 与 cobra flags（`:149-151`）？
   - 新增的配置键（例如 v1.23.0 的 `snapshot_l2_tombstone_wait_*`）是否既在 kit 用 `read.X` 读、又出现在登记表？可以临时删掉登记表里的一项，`TestEveryFrameworkDurationAndIntKeyIsCheckedStrictly` 应点名它。
   - `ConfigInt` 是否拒绝 `1.5`、`8k`、`10s`、`true`，接受 `1e3` 与 `"42"`？看 `TestConfigIntAcceptsWholeNumbersOnly`。

<a id="cfg-3"></a>
### CFG-3 A4 留项：kit/redis 三个整数键

[说明](guide-cfg-skill-noncore.md#cfg-3)

1. **提交与版本**：`5df60765`（分支 `c4c6`）、`cb3e2549`（DECISIONS-PENDING 标注）。首发 v1.20.2。
2. **改动**：`kit/redis/redis_mod.go:58` `redisConfig` 返回 `(*fredis.Config, error)`，`:59` 起 `read := app.NewConfigReader(cfg)` 读 `redis.db` / `pool_size` / `min_idle_conns`；Redis Mod 与 `kitredis.SingletonStore` 共用。守卫 `TestFrameworkCodeDoesNotReadConfigLeniently` 删掉“文件 + 键”的三项放行。
3. **不变量**：同 CFG-2。
4. **控制流**：`RedisMod.Init` / `SingletonStore` → `redisConfig` → 错误 `redis mod: config: redis.db …`。
5. **失败处理**：Init 返回错误。
6. **测试**：
   - 修前红：`config_types_promises_test.go:45: redis.db: 8k: Init returned <nil>, want a refusal naming redis.db`
   - 修后：`TestRedisIntegerKeysAreReadStrictly`（`kit/redis/config_types_promises_test.go:38`，三键 × 三种坏值，Mod 与 SingletonStore 都拒绝；负对照：合法的 2 / 16 / 3 读对）通过；`go test -race -count=3 ./kit/redis ./app` 与根包通过。
7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：`rg 'GetInt\(' kit/redis` 应无结果；`SingletonStore` 是否也走 `redisConfig`（两个独立小客户端都从同一份 `redis.*` 建）。

<a id="cfg-4"></a>
### CFG-4 A4 留项：生成的 player TCP 与 RPC 客户端严格读取

[说明](guide-cfg-skill-noncore.md#cfg-4)

1. **提交与版本**：`914a725f`（worktree `rel122`，基线 `cb3e2549`）；生成器 Core 下限同批随 v1.20.2 上调（`c85d4565` 发版提交）。首发 v1.20.2。
2. **改动**：

   | 位置 | 职责 |
   | --- | --- |
   | `codegen/internal/roost/render_player_tcp.go:239` `configFromViper`（模板内） | `read := app.NewConfigReader(cfg)`（`:243`）读 `player_access.tcp.*` 与 `nest.request_timeout`，最后 `read.Err()` |
   | `codegen/internal/servicerpc/template.go:519` | `app.ConfigDuration(cfg, "{{.ServiceType}}.call_timeout")` |
   | `kit/service/*_rpc_assembly_gen.go`（九份） | `go generate ./...` 重生成 |
   | `app/config_strict_reads_promises_test.go` `generatedConfigTemplates` | 两份模板纳入扫描；`playerTCPReadPattern` 改认 `read.X(key + "…")` |

3. **不变量**：生成进工程的配置读取与框架同一规则；守卫扫描模板源码，所以生成物不再靠登记表兜底。
4. **控制流**：生成工程 `Mod.Init` → `configFromViper` → `validateConfig`（CFG-13）。
5. **失败处理**：`max_handshake_bytes` / `max_payload_bytes` 先按 int 读，负数或超过 16 MiB 点名拒绝，再转 uint32。
6. **测试**：
   - 修前红（提交说明原文）：“enabled: on、max_payload_bytes: 8k、max_handshake_bytes: -1、idle_timeout: 90、handshake_timeout: soon、nest.request_timeout: 7 六项 Init returned <nil>（读成 false / 默认 / 90ns / 7ns），max_connections: 1.5 只报不点名的 safe bounds；mail.call_timeout: 5 / soon 两项 error = <nil>。”
   - 修后：生成用例 `TestConfigValuesOfTheWrongTypeAreRefusedByName`（模板内，`render_player_tcp.go:1404`）7 项全部点名拒绝；负对照：合法字符串写法（`enabled: "true"`、`max_payload_bytes: "65536"`）照常读取；kit/service/mail `call_timeout` 5 / soon 拒绝、2s 读对。
   - 复跑：`GOWORK=off go test -count=1 ./codegen/... ./app ./kit/service/...`；生成 game-demo（`go mod edit -replace` 到本仓）后 `go test ./internal/access/player/tcp/`。
7. **性能**：无。
8. **未验证**：无额外项；已生成工程不迁移。
9. **review 检查点**：`manifest.go:83` 的 `minimumVersions.Core` 是否 ≥ v1.20.2（发版提交上是 v1.21.0，因 CFG-7 再次上调）；`.github/workflows/framework-compat.yml` minimum 格与 `codegen/ci/framework-release.yaml` 是否同步。

<a id="cfg-5"></a>
### CFG-5 生产校验只查有读取方的设置（C1 / NC-192）

[说明](guide-cfg-skill-noncore.md#cfg-5)

1. **提交与版本**：`3e3350d5`（与 A4 同批）、`61fb0894`。首发 v1.20.2。
2. **改动**：`app/config_validation.go:96` `validateProductionServiceConfig` 只保留：`validateProductionOpsExposure`（`:173`）、`validateProductionLogicOffset`（CLK 主题，后加）、五类服务的 `redis.addr`、`validateProductionSecret`（`:162`，account / platform 密钥）、`validateProductionAdminGateway`（`:120`）。删掉 `app.go` 对 `player_protocol.rate_limit.enabled` 的 `SetDefault`。USER_GUIDE §10 新增“`env: production` 校验什么”。
3. **不变量**：生产校验的每一项都有运行时读取方。守卫：`TestValidateServiceConfigDoesNotRequireSwitchesNothingReads`（`app/config_validation_test.go:120`）；生成工程侧 `TestGeneratedConfigsPassStrictAndProductionValidation`（`codegen/internal/roost/generated_config_validation_promises_test.go:155`）在生成的 game-demo 里用真实 `app.ValidateServiceConfig` 读开发配置、生产示例、Secret 示例（后两者加 `env: production`）。
4. **控制流**：`ValidateServiceConfig` → `isProductionServiceConfig`（`:204`）→ 上述检查。
5. **失败处理**：缺项汇总报错，进程不启动。
6. **测试**：
   - 修前红（[c1-red.txt](../../bugfix/evidence/a4-config-20261005/c1-red.txt) 节选）：

     ```text
     a4_config_test.go:69: config.game.prod.example.yaml with env: production is refused:
         config: production game requires player.login_auth_required=true
         config: production requires non-dev player.login_secret
         config: production game requires player_protocol.rate_limit.enabled=true
         config: production game requires save_load.wal.enabled=true
         config: production game requires save_load.wal.required=true
         config: production game requires save_load.wal.mode=durable
     a4_config_test.go:69: config.global.prod.example.yaml with env: production is refused:
         config: production global requires global.redis_required=true
     ```

   - 修后：两条通过；负对照 `TestValidateServiceConfigRejectsProductionServicesWithoutRedis`（`:141`）与 ops 暴露、密钥拒绝用例仍通过。原先断言无效键的用例按新契约改写。
   - 复跑：`GOWORK=off go test -count=1 -run 'Production' ./app`、`go test -count=1 -run TestGeneratedConfigsPassStrictAndProductionValidation ./codegen/internal/roost`（会生成工程，较慢）。
7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：`validateProductionServiceConfig` 每个检查的键都能在非测试源码里找到读取点（`rg '"redis.addr"' kit app`）；新增生产要求时应同时有读取方。

<a id="cfg-6"></a>
### CFG-6 tablegen `ref` 与 `-check`（NC-75）

[说明](guide-cfg-skill-noncore.md#cfg-6)

1. **提交与版本**：`45d4bc1c`（NC-74 / NC-75 同批）、登记 `6c278081`。首发 v1.20.1。**v1.21.0 被 `b12216ed`（CFG-7）改写**：生成 loader 不再带 ref 循环，`-check` 改用 `configdata/rules`。
2. **改动（发版提交上仍存在的部分）**：`codegen/internal/tablegen/main.go:1031` `resolveRefs`（目标必须是本次 schema 的表、字段类型去掉指针后等于目标主键类型，否则生成失败）；`:492` `checkJSONFiles`（现由 `rules.Document` / `Rows` / `Check` 实现）。新增 `codegen/scripts/tablegen-runtime.sh` 与夹具 `codegen/internal/tablegen/testdata/runtime/`，并进 `.github/workflows/ci.yml:81`。
3. **不变量**：悬空 ref 在加载 / reload 时被拒、旧快照保持；ref 声明在生成期解析。
4. **控制流**：见 CFG-7（v1.21.0 起 ref 由 `configdata.checkRefs` 在全部表加载后检查）。
5. **失败处理**：生成期报错；运行时整次拒绝。
6. **测试**：
   - 修前红（[nc75-gate-red.txt](../../review/evidence/noncore-review-20261005-n08/nc75-gate-red.txt)、[nc75-check-red.txt](../../review/evidence/noncore-review-20261005-n08/nc75-check-red.txt)）：

     ```text
     roundtrip_test.go:73: reload with a dangling scene_id = <nil>, want a reference error
     --- FAIL: TestDanglingRefIsRejectedOnLoadAndReload/monster_points_at_no_scene (0.00s)
     --- FAIL: TestDanglingRefIsRejectedOnLoadAndReload/the_scene_it_points_at_is_removed (0.00s)
     check_json_promises_test.go:41: -check accepted JSON that breaks "required key missing"
     check_json_promises_test.go:41: -check accepted JSON that breaks "below min"
     （"required key null"、"key repeated"、"unique column repeated"、"not a list of rows" 同形）
     ```

   - 修后：`TestCheckJSONEnforcesTheDeclaredRules`（`codegen/internal/tablegen/check_json_promises_test.go:26`）、`TestRefDeclarationsAreResolvedAtGeneration`（`:57`，合法 / 指针 / 未知目标 / 类型不符）、运行期门 `TestDanglingRefIsRejectedOnLoadAndReload`（`testdata/runtime/roundtrip_test.go:60`）通过。
   - 复跑：`GOWORK=off go test -count=1 ./codegen/internal/tablegen/`；`ROOST_CORE_DIR=$PWD sh codegen/scripts/tablegen-runtime.sh -race -count=1`。
7. **性能**：无。
8. **未验证**：无。object 文件不支持 `Ref`（B10 起注册期拒绝，`configdata/fieldrules.go:33` `resolveRules` 的 `object` 分支），不存在待验证的路径。
9. **review 检查点**：`resolveRefs` 是否仍在 `generateGo`（`:716`）之前调用；运行期门是否用 `ROOST_CORE_DIR` 能指向本地 core（否则按 pin 跑会缺新 API）。

<a id="cfg-7"></a>
### CFG-7 配置规则统一由运行时加载层强制（B10）

[说明](guide-cfg-skill-noncore.md#cfg-7)

1. **提交与版本**：`5c0d6fc8`（记录决定与 config 易用原则）、`b12216ed`（实施，分支 `b10cfg`，基线 `4f0bab75`）、`81d7cb16`（标注）。生成器 Core 下限 v1.21.0 随发版提交 `4881f2b7`。首发 v1.21.0。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `configdata/rules/rules.go:30` `Rule`、`:48` `Rule.Validate`、`:72` `Error` | 规则的唯一表示、声明自检、结构化错误（表 / 行 / 键 / 字段 / 规则 / 细节） |
   | `configdata/rules/rules.go:102` `Document`、`:141` `Rows` | 文件 → 载荷（`rows` / `records` / `data` 单包装键）→ 原始行 |
   | `configdata/rules/rules.go:212` `Check`、`:217` `CheckObject`、`:221` `check` | required / unique / min / enum 在原始 JSON 上检查；`Ref` 跳过（由加载层查） |
   | `configdata/rules/rules.go:277` `Lookup`、`:284` `Canonical` | 逐字查键（CFG-10 后）；`1` / `1.0` / `1e0` 同形 |
   | `configdata/fieldrules.go:16` `FieldRule = rules.Rule`、`:20` `RuleError`、`:33` `resolveRules`、`:142` `checkRefs` | 注册期声明校验；全部表加载后查 ref |
   | `configdata/configdata.go:400` `TableDef`（`:422` `Rules`、`:429` 注册校验、`:459` `rules.Check`、`:474` `checkRefs`）；`:494` `ObjectDef`（`:501`、`:508`、`:534`） | 每次加载同一份字节解一次类型化行、一次原始行 |
   | `configdata/auto.go:385-399` | `cfg` 标签 `required` / `unique` / `min=` / `enum=` / `ref=` 解析成同一组 `Rule`；auto 自己的 `validateRefs` 删除 |
   | `codegen/internal/tablegen/main.go:559` `metaRules`、`:725` `rule.Validate()`、`:796` / `:810` 生成 `Rules: []configdata.FieldRule{…}`、`:845` `<Type>Table()` | 一个 schema 标签同时驱动 CSV 转换、`-check` 与生成 loader |
   | `codegen/internal/cfggen/main.go` | `required` 独立，新增 `unique` / `min` / `enum` |
   | `dependency_boundary_test.go:204-227` `sharedConfigRules` / `TestSharedConfigRulesStayALeaf`；`:257` / `:273` 边界例外 | codegen 只允许 import `configdata/rules`，该包只能依赖标准库 |

3. **不变量**：
   - 一份规则声明、一个检查器：生成期（tablegen CSV 转换、`-check`）与运行时（configdata Load / Reload）调用同一个 `rules.Check`。守卫：`TestOneTagDrivesTheGenerationCheckAndTheGeneratedLoader`（`codegen/internal/tablegen/rules_single_source_promises_test.go:17`）改一个标签，生成期与 loader 同时变。
   - 违反任何规则整次拒绝、旧快照保持（沿用 build 失败语义）。守卫：`TestDeclaredRulesRejectReloadAndNameTheViolation`（`configdata/field_rules_promises_test.go:55`）、`TestRuleDeclarationsAreCheckedAtRegistration`（`:98`）、`TestAutoTableTagRulesUseTheSharedCheck`（`:123`）。
   - `configdata/rules` 只依赖标准库：`TestSharedConfigRulesStayALeaf`。
4. **控制流**（一次 Reload）：

   ```text
   读文件 → rules.Document → 解类型化行（encoding/json）
          → checkKeySpelling（CFG-10）
          → rules.Rows + rules.Check（required / unique / min / enum）
          → newTable（主键唯一）
   全部表 build 完 → checkRefs（ref）→ 监听者 Validate → BeforeApply → 发布 → emit / AfterApply（CFG-8）
   任何一步失败 → 整次失败，Current 不变（发布后失败则撤回）
   ```

5. **失败处理**：`*RuleError` 可 `errors.As` 取结构化字段；失败计入 CFG-8 的 outcome（stage=build）。
6. **测试**：
   - 修前红（[b10-c2 red.txt](../../feature/evidence/b10-c2-20261006/red.txt) 原文节选）：

     ```text
     --- FAIL: TestDeclaredRulesAreEnforcedOnReload (0.00s)
         --- FAIL: TestDeclaredRulesAreEnforcedOnReload/a_required_value_is_null (0.00s)
             roundtrip_test.go:111: reload accepted a required value is null
         --- FAIL: TestDeclaredRulesAreEnforcedOnReload/a_value_is_below_min (0.00s)
             roundtrip_test.go:111: reload accepted a value is below min
         --- FAIL: TestDeclaredRulesAreEnforcedOnReload/a_required_column_is_deleted (0.00s)
             roundtrip_test.go:111: reload accepted a required column is deleted
     --- FAIL: TestOneTagDrivesTheGenerationCheckAndTheGeneratedLoader (0.00s)
         rules_single_source_promises_test.go:40: min=1: generated loader does not carry {Field: "level", Min: "1"}:
     ```

   - 端到端（[e2e-reload.txt](../../feature/evidence/b10-c2-20261006/e2e-reload.txt)）：生成 game-demo、隔离环境起 global + game，`gm.config.reload` 删掉 spawn 的 template → `configdata: table spawn row 1 (key 1) field template: required: missing or null`；hp 0 → `min`；template null → `required`；三次失败后版本仍 2、存活仍 4，`configdata_reload_total{result="failed"} 3`。修前同一场景见 N07 第二批 H2e（reload 被接受、按 template 0 刷怪，日志 0 行）。
   - 复跑：`GOWORK=off go test -race -count=3 ./configdata/... ./kit/configdata/ ./codegen/internal/tablegen/ ./codegen/internal/cfggen/`；`ROOST_CORE_DIR=$PWD sh codegen/scripts/tablegen-runtime.sh -race`；`ROOST_CORE_DIR=$PWD sh codegen/scripts/cfggen-golden-runtime.sh -race`；根包 `go test -count=1 .`。
7. **性能**：每次加载多解析一次原始行（只在加载 / 热更）；未做基准。
8. **未验证**：无。ref 只在加载时查是 B10 决定的分层（“规则统一由运行时加载层强制，生成期检查只作提前反馈”，DECISIONS-PENDING 第四轮 B10 行）；两种标签方言合一是 B10 §2.4 评估后列的后续工作，不是待验证项。
9. **review 检查点**：
   - `rules.Check` 是否对“键缺失”与“值为 null”给出同一 `required` 错误、对“值为 0”不报 required（看 `rules.go:221` 起的 `check` 与 `isNull`）。
   - `resolveRules` 是否在注册时拒绝 `Min` 用在字符串字段、`Unique` 用在对象上（`fieldrules.go:33`）。
   - tablegen 生成的 loader 里是否还有手写 ref 循环（`rg ValidateTable codegen/internal/tablegen/main.go` 只应剩用户自定义钩子相关）。
   - `codegen` 下除 `configdata/rules` 外是否没有 import 任何 core 包：跑根包边界测试。

<a id="cfg-8"></a>
### CFG-8 热更失败与回滚可见（C2）

[说明](guide-cfg-skill-noncore.md#cfg-8)

1. **提交与版本**：`b12216ed`（与 B10 同批）。首发 v1.21.0。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `configdata/doc.go:20-33` | C2 契约：发布先于 AfterApply、撤回期间准入的请求读被撤回的一代、版本号单调占号 |
   | `configdata/configdata.go:914` `ReloadOutcome`、`:936` `Reverted`、`:943` `OnReloadOutcome` | 每次 Load / Reload / Rollback 恰好一个结果 |
   | `configdata/configdata.go:967` `report` | 写日志（`:983` rolled back、`:985` applied、`:987` reverted、`:990` failed）并通知订阅者 |
   | `configdata/configdata.go:860` `ReloadWithReason`、`:1106` `DryRun`、`:1126` `Rollback` | 各入口都经 `report` |
   | `kit/configdata/configdata.go:63-81` | 订阅 outcome 记 `configdata.reload.total{result}`、`configdata.rollback.total{trigger}`、`configdata.version`；不再挂 ReloadHook |

3. **不变量**：一次尝试恰好一个 outcome、一条日志、至多一次 `reload.total` 计数；标签只有 `result` / `trigger` 两种低基数值。守卫：`TestEveryReloadReportsOneOutcome`（`configdata/field_rules_promises_test.go:151`）、`TestFailedReloadAndRollbackAreCountedAndLogged`（`kit/configdata/reload_visibility_promises_test.go:56`）。
4. **控制流**：见 CFG-7 的流程图；`report` 在持锁状态下复制订阅者列表后调用。
5. **失败处理**：撤回（stage=apply）记一次 `failed` + 一次 `rollback{trigger=apply_failed}`，不再先记 `ok`。
6. **测试**：
   - 修前红（同一 red.txt）：

     ```text
     --- FAIL: TestFailedReloadAndRollbackAreCountedAndLogged (0.00s)
         reload_visibility_promises_test.go:95: configdata.reload.total{result=ok} = 3, want 2 (Start's load and the first reload)
         reload_visibility_promises_test.go:98: configdata.reload.total{result=failed} = 0, want 2 (missing dir, reverted publish)
         reload_visibility_promises_test.go:101: configdata.rollback.total{trigger=apply_failed} = 0, want 1
         reload_visibility_promises_test.go:109: configdata.reload.total carries label "reason"="late"; only low-cardinality result / trigger are allowed
         reload_visibility_promises_test.go:116: log does not contain "config reload failed":
         reload_visibility_promises_test.go:116: log does not contain "stage=build":
         reload_visibility_promises_test.go:116: log does not contain "config reload reverted":
         reload_visibility_promises_test.go:116: log does not contain "stage=apply":
     ```

   - 修后通过；端到端见 CFG-7（三条 Warn `config reload failed … stage=build`）。
   - 复跑：`GOWORK=off go test -count=1 -run 'TestFailedReloadAndRollback|TestEveryReloadReportsOneOutcome' ./kit/configdata/ ./configdata/`。
7. **性能**：无。
8. **未验证**：无外部项。`stage=apply` 撤回与运维 Rollback 由单测在同一个 `Store` 上覆盖（`TestEveryReloadReportsOneOutcome`、`TestFailedReloadAndRollbackAreCountedAndLogged`）；生成的 game-demo 没有运维 Rollback 入口、也没有会失败的 AfterApply 监听者，没有可演练这两条路径的真实进程（B10 方案“未完成 / 后续”第 3 条）。
9. **review 检查点**：
   - `OnReloadOutcome` 的取消函数是否在 kit Mod 停止时调用（`kit/configdata/configdata.go` 的 `unregisters`）。
   - DryRun（`configdata.go:1106`）占版本号（`:1112` `s.version.Add(1)`）但不经 `report`，不产生 outcome、日志与计数；契约只承诺 Load / Reload / Rollback 各报告一次。确认运维用 DryRun 预检时看不到指标属于预期。
   - observability README 是否已改正“热更后版本 +1”的说法（C-O7）。

<a id="cfg-9"></a>
### CFG-9 规则层大小写变体取最后一个（已被 CFG-10 取代）

[说明](guide-cfg-skill-noncore.md#cfg-9)

1. **提交与版本**：`5a3c4a60`（发版前审查收尾第 5 条，分支 `auditfu`，基线 `c99b59f6`）、记录 `3d3b0c09`。首发 v1.21.0；**v1.22.0 由 `c474a6ef` 删除**（`hasCaseVariants` / `keepLastCaseVariant` / `foldName` 与对应用例）。
2. **改动**：发版提交上已不存在。v1.21.0 上是 `configdata/rules/rules.go` 的 `Rows` 预处理与 `Lookup` 的确定选择。
3. **不变量（v1.21.0）**：规则检查的值等于 encoding/json 解出的值。
4. **控制流**：无（已删除）。
5. **失败处理**：无。
6. **测试**（记录原文，修前红）：

   ```text
   --- FAIL: TestRulesCheckTheValueEncodingJSONDecodes (0.00s)
       lookup_order_promises_test.go:44: [{"id":1,"level":5,"Level":0}]: encoding/json decodes level=0, but the rule check refused=false (<nil>) on run 0
   --- FAIL: TestObjectRulesCheckTheValueEncodingJSONDecodes (0.00s)
       lookup_order_promises_test.go:64: {"width":3,"Width":0} decodes width=0, but the min=1 rule accepted it
   ```

   这些用例在 v1.22.0 随逻辑删除，换成 `configdata/rules/key_spelling_promises_test.go`。
7. **性能**：只在出现变体行时多解析一次。
8. **未验证**：无（已取代）。
9. **review 检查点**：确认发版提交上 `rg 'keepLastCaseVariant|hasCaseVariants|foldName' configdata` 无结果，`Lookup`（`rules.go:277`）是逐字匹配。

<a id="cfg-10"></a>
### CFG-10 configdata 键大小写敏感

[说明](guide-cfg-skill-noncore.md#cfg-10)

1. **提交与版本**：`2a4c835d`（下一轮规划）、`c474a6ef`（实施，分支 `cfgcase`，基线 `2a4c835d`）、`da8c133a`（标注）。首发 v1.22.0。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `configdata/rules/rules.go:165` `MisspelledKey` | 唯一的拼写规则：键与某个声明名只差大小写即为拼错 |
   | `configdata/rules/rules.go:182` `CaseError`、`:189` `CheckKeys`、`:203` `CheckObjectKeys` | 行形式的检查，供生成器用 |
   | `configdata/keyspelling.go:22` `checkKeySpelling`、`:59` `spellingWalker.walk`、`:125` `structFields` | 解码成功后按行类型逐层核对原始键（结构体、切片 / 数组元素、map 的值；跳过 map 键、`interface{}`、`json.Unmarshaler`） |
   | `configdata/configdata.go:451`（表）、`:526`（对象） | 每次加载都跑（不只在声明了 Rules 时） |
   | `configdata/fieldrules.go:91` `fieldForJSONKey` | 规则字段逐字匹配 json 名 |
   | `codegen/internal/tablegen/main.go:522` / `:527` | `-check` 先跑 `CheckObjectKeys` / `CheckKeys` |
   | `codegen/internal/tablegen/main.go:605-606` | CSV 转换核对表头 |
   | `codegen/internal/tablegen/main.go:888`、`:933` `tablegenCheckHeader` | 生成的 `Convert<Type>CSV` 里同一规则的标准库副本（不引入新 core API） |

3. **不变量**：声明字段的键逐字匹配；未声明的键行为不变。守卫与负对照：`TestUndeclaredKeysKeepTheirBehaviour`（`configdata/key_case_promises_test.go:140`，修前修后都通过）。
4. **控制流**：解码（宽松 / 严格）成功 → `checkKeySpelling` → 规则检查。生成期只看顶层字段（tablegen 的 meta 不知道嵌套类型），嵌套由加载层核对；两处调用同一个 `MisspelledKey`。
5. **失败处理**：`*RuleError{Rule: "case"}`，整次拒绝。
6. **测试**：
   - 修前红（方案 §5 原文）：

     ```text
     --- FAIL: TestMisspelledKeysRejectLoad/variant_only (0.00s)            # {"id":1,"Level":1}
             key_case_promises_test.go:84: err = <nil>, want a *RuleError
     --- FAIL: TestMisspelledKeysRejectLoad/exact_then_variant (0.00s)      # {"id":1,"level":1,"Level":2}
             key_case_promises_test.go:84: err = <nil>, want a *RuleError
     --- FAIL: TestRuleFieldMustMatchTheJSONNameExactly (0.00s)
         key_case_promises_test.go:151: register err = <nil>, want a refusal naming level
     --- FAIL: TestCheckJSONRejectsMisspelledKeys (0.00s)
         key_case_promises_test.go:21: [{"id":1,"name":"slime","Level":3,"code":"a"}]: err = <nil>, want "table monster row 1 (key 1) field level: case: key \"Level\" must be spelled \"level\""
     --- FAIL: TestCSVHeaderIsCaseSensitive (0.00s)
         key_case_promises_test.go:33: err = <nil>, want the misspelled header named
     --- FAIL: TestGeneratedCSVConverterRejectsMisspelledHeaders (0.00s)
         roundtrip_test.go:131: ConvertMonsterCSV err = <nil>, want the misspelled header named
     ```

   - 修后全部通过：`configdata/key_case_promises_test.go:84` / `:104` / `:156`、`configdata/rules/key_spelling_promises_test.go:14` / `:43` / `:55`、`codegen/internal/tablegen/key_case_promises_test.go:12` / `:30`、`testdata/runtime/roundtrip_test.go:130`。生成 game-demo 临时用例：把 `spawn.json` 的 `template` 改成 `Template` 等，Start 报 `table spawn row 1 (key 1) field template: case: key "Template" must be spelled "template"`（临时用例不入库）。
   - 复跑：`GOWORK=off go test -race -count=3 ./configdata/... ./kit/configdata/ ./codegen/internal/tablegen/ ./codegen/internal/cfggen/`；`ROOST_CORE_DIR=$PWD sh codegen/scripts/tablegen-runtime.sh -race -count=1`。
7. **性能**：每次加载多解析一次载荷（原始行 + 逐层 map），未做基准。
8. **未验证**：无。说明两点（不是待验证项）：`tablegen-runtime.sh` 按 pin（v1.21.0）跑时新用例只依赖生成代码，要验运行时行为用 `ROOST_CORE_DIR` 指向本地 core；同名 json 字段在不同嵌入深度时按“浅层优先”取类型（行为限制，见说明文档 CFG-10）。
9. **review 检查点**：
   - `checkKeySpelling` 是否对 `json.RawMessage` / `time.Time` 字段不递归（`keyspelling.go:56` `jsonUnmarshalerType`）。
   - 生成的 `tablegenCheckHeader`（`main.go:933`）与 `rules.MisspelledKey` 是否同一判定（两份代码，需人工对照）。
   - kit/configdata 不改代码：Start / Reload 走同一个 Store，确认没有绕过 `TableDef` 的加载入口。

<a id="cfg-11"></a>
### CFG-11 cfggen globals 的 required / min / enum

[说明](guide-cfg-skill-noncore.md#cfg-11)

1. **提交与版本**：`229a5aa0`（第十二轮 skill / buff 投影 / cfggen 同批，分支 `bsk`，基线 `78e26853`）、`6bf15516`（标注）。首发 v1.23.0（本版）。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `codegen/internal/cfggen/main.go:455-463` | globals 上 `index` / `ref` / `unique` 仍拒绝（带理由） |
   | `codegen/internal/cfggen/main.go:631` `fieldRule`、`:637` `validateFieldRules`、`:639` `.Validate()` | 字段选项 → `rules.Rule` 的唯一翻译；生成期用同一个 `Rule.Validate` |
   | `codegen/internal/cfggen/main.go:792-822` `globalRulesLiteral` | 写出 `, Rules: []configdata.FieldRule{…}`；无规则时为空串，输出逐字不变 |

3. **不变量**：生成期与运行时同一份规则、同一段检查代码；无规则 global 的输出不变。守卫：`TestCfggenGlobalRulesBecomeObjectDefRules`（`codegen/internal/cfggen/global_rules_promises_test.go:23`）、`TestCfggenGlobalWithoutRulesKeepsItsRegistration`（`:43`，负对照）、`TestCfggenGlobalRulesStillRejectWhatAnObjectCannotMean`（`:53`）；运行期门 `TestGlobalRulesAreEnforcedOnLoadAndReload`（`codegen/internal/cfggen/testdata/runtime/roundtrip_test.go:163`）。
4. **控制流**：meta → `fieldRule` → `Validate` → 生成 `ObjectDef.Rules` → configdata `CheckObject`（CFG-7）。
5. **失败处理**：违反时启动失败；reload 被拒，旧快照不动。
6. **测试**：
   - 修前红（记录原文）：单测 `global world field width: singleton configs do not support required / unique / min / enum yet`；`cfggen-golden-runtime.sh` 生成步骤同样报错退出；去掉规则后 `load of world {"width":1024,"height":768,"mode":"pvz"}: err = <nil>, want "field mode: enum"`。
   - 修后：单测通过（Rules 字面量、无规则 global 输出不变、unique / min 非数值 / enum 重复 / bean 上 enum 仍拒绝）；`ROOST_CORE_DIR=<worktree> cfggen-golden-runtime.sh -race` 通过：缺 width、width 0、height 0、mode 拼错四种在启动 Load 与 Reload 都被拒。
   - 复跑：`GOWORK=off go test -count=1 ./codegen/internal/cfggen/`；`ROOST_CORE_DIR=$PWD sh codegen/scripts/cfggen-golden-runtime.sh -race`。
7. **性能**：无。
8. **未验证**：无。game-demo 模板不用 cfggen，生成物由运行期门 `cfggen-golden-runtime.sh` 覆盖。
9. **review 检查点**：`fieldRule` 是 tables 与 globals 共用的唯一翻译吗（`rg 'rules.Rule{' codegen/internal/cfggen` 应只在 `fieldRule` 里）；global struct 上是否确实不再写规则 `cfg` 标签（`:799` 附近）。

<a id="cfg-12"></a>
### CFG-12 生成配置写出 `remote_entity` 新键（A8）

> 其他分册对应：REM-13 是同一项。汇总去重：本条保留，REM-13 改为引用本条。

[说明](guide-cfg-skill-noncore.md#cfg-12)

1. **提交与版本**：`fcc78ad0`（收尾第 2 批，分支 `cb2`，基线 `8a292a5a`）、`94548913`（标注）。首发 v1.23.0（本版）。
2. **改动**：`codegen/internal/roost/catalog.go:71`（`remote_entity` 段模板，五个键与中文注释）；`codegen/internal/roost/render.go:646-656` `streamReplicasLine`（只替换独占一行、键名恰为 `replicas: 1` 的行）。
3. **不变量**：生成值等于 core `remoteentity.DefaultConfig()` / kit 缺省；生产化不改墓碑 `WAIT` 副本数。守卫：`TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys`（`codegen/internal/roost/remote_entity_config_keys_promises_test.go:17`）；`TestGeneratedConfigsPassStrictAndProductionValidation`（`generated_config_validation_promises_test.go:155`）在生成工程里注入 `a4_config_test.go`，核对五键已设置、取值一致、`ValidateServiceConfig` 通过、`RemoteEntityMod.Init` 与 `RemoteMirrorMod.Init` 接受。
4. **控制流**：生成器拼接 Mod 配置段 → `productionizeConfig`（生产示例 / Secret 示例）。
5. **失败处理**：无运行时变化。
6. **测试**：
   - 修前红（记录原文）：

     ```text
     --- FAIL: TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys
         production=false: config lacks "cached_max_staleness: 30s"   （其余四个键同样，开发 / 生产各一遍）
     --- FAIL: TestGeneratedConfigsPassStrictAndProductionValidation
         a4_config_test.go:92: config.game.yaml does not set remote_entity.cached_max_staleness
         a4_config_test.go:103: config.game.yaml: snapshot_l2_tombstone_wait_replicas = 0, want DefaultConfig 1
     ```

     只改模板不改生产化时：`production=true: config lacks "snapshot_l2_tombstone_wait_replicas: 1"`（被改成了 3）。
   - 修后通过；生成的 game 服务带这份配置在隔离环境起来并就绪。
   - 复跑：`GOWORK=off go test -count=1 -run 'RemoteEntitySection|GeneratedConfigsPass' ./codegen/internal/roost`。
7. **性能**：无。
8. **未验证**：无。GitHub framework-compat 在 `e6828e4f` 上全部通过（run `37461843085`：minimum / released / source-head × minimal / demo / full 九格与 `codegen-network`）。
9. **review 检查点**：`streamReplicasLine` 的正则 `(?m)^([ \t]*)replicas: 1$` 是否不会命中 `snapshot_l2_tombstone_wait_replicas: 1`（行首锚定 + 键名恰为 `replicas`）；五个键的缺省值是否仍与 `remoteentity.DefaultConfig()` 一致（若以后改缺省，生成工程测试会报差异）。

<a id="cfg-13"></a>
### CFG-13 生成 TCP 越界报错逐条点名（A9）

[说明](guide-cfg-skill-noncore.md#cfg-13)

1. **提交与版本**：`fcc78ad0`、`94548913`。首发 v1.23.0（本版）。
2. **改动**：`codegen/internal/roost/render_player_tcp.go:288-349` `validateConfig`（模板内）：逐条检查，`refuse(...)` 收集，`errors.Join` 一次报全；`:312` 一带是“受约束键”表（`max_handshakes`、`max_connections_per_ip` 不超过 `max_connections`），`:345-347` 是 `login_timeout` 与 `dispatch_timeout` 的关系。
3. **不变量**：接受 / 拒绝的边界不变，只改报错。守卫：生成用例 `TestAnOutOfBoundsSettingIsRefusedByName`（`render_player_tcp.go:1709`，10 个子用例，如 `:1720` handshakes_above_connections、`:1725` login_above_dispatch）；`TestConfigValuesOfTheWrongTypeAreRefusedByName`、`TestServerRejectsInvalidConstruction` 不变（负对照：缺省配置仍被接受）。
4. **控制流**：`configFromViper`（CFG-4）→ `validateConfig`。
5. **失败处理**：`max_connections` 本身不合法时不再报关系类错误，免得一条错因生出三条。
6. **测试**：
   - 修前红（记录原文）：

     ```text
     map[max_connections:1000 max_handshakes:2000] refused with "player tcp: addr, limits and timeouts are outside safe bounds", want it to name "player_access.tcp.max_handshakes = 2000"
     map[handshake_timeout:2m] refused with "player tcp: timeouts exceed safe bounds", want it to name "player_access.tcp.handshake_timeout = 2m0s"
     map[addr:localhost] refused with "player tcp: invalid addr \"localhost\": ...", want it to name "player_access.tcp.addr"
     （共 10 个子用例全部失败）
     ```

   - 修后 10 个子用例通过；生成 game-demo `go test -race -count=3 ./internal/access/player/tcp/` 通过。
   - 复跑：生成 game-demo 后 `GOWORK=off go test -count=1 -run TestAnOutOfBoundsSettingIsRefusedByName ./internal/access/player/tcp/`。
7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：错误里每条是否都带 `player_access.tcp.` 前缀与当前值；关系类错误是否只在被约束键本身合法时报出。

## SKILL：skill 编译器、Runtime 与同步

先读：`skill/README.md` 的 pass 表（各 pass 负责拒绝什么；该文件合仓后 52 个按旧仓布局写的相对链接已在 `d05a04a1` 改对，并加了根包文档链接门禁，见 TOOL-5，本分册不重复）、[`docs/skill/skill-implementation-guide.md`](../../skill/skill-implementation-guide.md)、[作者文档](../../skill/skill-casting-and-combat.md)“引用在哪里能读”与“Runtime 不在事务里（B4）”，以及 roost-coding A1 条里 skill Runtime 的例外（第 39 行）。

本主题的全局守卫（改 skill 编译器或 Runtime 时都要跑）：

| 守卫 | 位置 | 守什么 |
| --- | --- | --- |
| 变异性质测试“编译 ⇒ 可执行” | `skill/compile_mutation_property_test.go:369` `TestCompiledMutationsNeverHitProgramInvariant` | 以全部 fixture 与补充种子为起点做变异，凡编译无 error 的变体施法推进不得出现 `ErrProgramInvariant` / `ErrReferenceOutOfContext` |
| 变异性质测试“改 Program 必改 digest” | 同文件 `:413` `TestCompiledMutationsChangeDigestWhenProgramChanges` | Program 变了 gameplay digest 必须变 |
| lower 逐表删条目 | `skill/lower_lookup_promises_test.go:105` `TestLowerRefusesEveryUnresolvedLookup` | lower 查不到名字要么 `LOWER_UNRESOLVED`、要么 Program 逐字段不变 |
| phase 事件单一来源 | `skill/phase_events_promises_test.go:11` `TestPhaseEventTableIsTheSingleSource` | 表覆盖 IR 与定义的全部字段、派发与拒绝两组不重不漏 |
| 求值上下文表逐格 | `skill/eval_contexts_table_test.go:259` / `:290` / `:333` / `:504` | 每格有用例或理由；编译期与 Runtime 对每格的判断一致 |
| 增量 state mutation 影子校验 | `skill/runtime_mutation.go:94` `stateMutationVerifyIncremental`（测试打开） | skill 包全部用例里增量 mutation 与全量快照逐笔比对 |

本地复跑（全部 `GOWORK=off`，不需要外部依赖）：

```sh
go test -race -count=3 ./skill/...                   # skill / combat / combatcomponent / skillcompose / skillsync
SKILL_MUTATION_FULL=1 go test -count=1 -run 'TestCompiledMutations' ./skill   # 完整变异集（较慢）
go test ./skill -run '^$' -fuzz FuzzRestoreRuntimeCheckpointNeverPanics -fuzztime 20s
(cd skill/examples && go build ./... && go vet ./... && go run ./fireball && go run ./combat && go run ./statusbridge)
(cd skill/integration/sync-e2e && go test -count=1 ./...)
go test -count=1 -run TestExamplesRun .               # 根包示例实跑门禁
```

<a id="skill-1"></a>
### SKILL-1 施法失败单一终态入口（NC-110～112）

[说明](guide-cfg-skill-noncore.md#skill-1)

1. **提交与版本**：`8c2d21a6`（登记）、`855c2a38`（NC-110～113 修复）。首发 v1.20.1。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `skill/scheduler.go:476` `failCastLocked` | 记 failed（保留第一次原因）→ 撤本 cast 全部排程与帧 → 停进程 → 释放 policy 槽位 → 标记 ability cast 结束；可重复调用 |
   | `skill/scheduler.go:489` `cancelCastTasks` | 按 cast ID 撤任务与帧（不分 phase token） |
   | `skill/runtime_cast_window.go:323` `castEnded` | failed / finished / 逻辑结束 / 恢复期 / 完成 / 已取消都拒绝 Cancel / Interrupt / Release，替代三处重复的窗口阶段判断 |
   | `skill/runtime.go` `startLocked`、`Cancel`、`Interrupt`、`releaseCast` | 出错时先 `failCastLocked` 再返回原错误；`startLocked` 在它之后才删 cast、还 ID |

3. **不变量**：任何把 cast 改到一半后出错的路径都进入同一个终态入口；ID 复用前旧 cast 名下没有排程任务。守卫：`skill/runtime_cast_terminal_promises_test.go` 五个用例（`:37`、`:71`、`:89`、`:118`、`:146`）。
4. **控制流**：`Advance` / API 调用 → 业务回调失败 → `failCastLocked`（幂等）→ 返回原错误。
5. **失败处理**：保留第一次失败原因；之后对该 cast 的输入返回 `ErrCastInputRejected`。
6. **测试**：
   - 修前红（基线 `be7bcc18`，问题记录原文）：

     ```text
     --- FAIL: TestFailedStartLeavesNoScheduledWorkForTheReusedCastID
         runtime_cast_terminal_promises_test.go:46: failed start left 1 scheduled tasks and 1 frames for cast 1
         runtime_cast_terminal_promises_test.go:60: health at tick 3 = 60: the rejected cast's wait landed on the reused id (second cast is due at tick 4)
     --- FAIL: TestCheckpointAfterFailedStartRestores
         runtime_cast_terminal_promises_test.go:84: restore after a failed start: skill: runtime checkpoint is corrupt
     --- FAIL: TestCancelCallbackFailureStillEndsTheCast
         runtime_cast_terminal_promises_test.go:104: cast after failed cancel: status=suspended stage=preparing, want failed
     --- FAIL: TestChargeReleaseFailureStillEndsTheCast
         runtime_cast_terminal_promises_test.go:136: cast after failed release: status=running stage=preparing, want failed
     --- FAIL: TestFailedToggleReleasesItsPolicySlot
         runtime_cast_terminal_promises_test.go:165: activation after a failed toggle: id=1 (failed=1) failedStatus=finished health=98 cooldown=12; want a new cast, the failed one untouched
     ```

   - 修后：`go test -race -count=3 ./skill/...` 5 包通过（影子校验打开）；`skill/examples` 的 fireball、sync-e2e 通过。
7. **性能**：无。
8. **未验证**：无外部项。
9. **review 检查点**：
   - `rg 'cast.status = CastFailed' skill` 除 `failCastLocked` 外是否还有直接置 failed 的地方；`startLocked` 删除 cast 是否在 `failCastLocked` 之后（顺序反了会让任务残留在被复用的 ID 上）。
   - 下面三条路径没有单独用例，核对它们与已测的五条一样经 `failCastLocked` 收尾：`Interrupt` 的 `stopProcesses` 出错分支（`skill/runtime_cast_window.go:297-299`）、toggle release 回调出错分支、charge enter 失败后 owned 进程在宿主侧是否留下实体。

<a id="skill-2"></a>
### SKILL-2 Combatant 副本不共享 map（NC-113）

[说明](guide-cfg-skill-noncore.md#skill-2)

1. **提交与版本**：`855c2a38`。首发 v1.20.1。
2. **改动**：`skill/combatcomponent/component.go:292` `cloneCombatant`；`Combatant()` 返回它、`InitCombatant` 存它。
3. **不变量**：存储的 `ElementMultipliersBP` 不可变（DAO 自身从不原地改它），所以 DAO `beginChange`（`skill/combatcomponent/component.go:334`）对 vitals 的浅拷贝 `before` 仍精确。（NC-113 当时这份浅拷贝在组件方法 `undoVitals` 里；v1.20.2 的 A1 `5407f127` 把逆操作登记移进 DAO，`undoVitals` 已删除。）守卫：`TestCombatantCopiesDoNotShareElementMultipliers`（`skill/combatcomponent/combatant_copy_promises_test.go:16`）。
4. **控制流**：无。
5. **失败处理**：无。
6. **测试**：修前红：

   ```text
   combatant_copy_promises_test.go:28: mutating the caller's template changed the stored multiplier to 1, want 12000
   combatant_copy_promises_test.go:34: mutating the returned copy changed the stored multiplier to 0, want 12000
   ```

   修后 `go test -race -count=3 ./skill/combatcomponent` 通过；四种 Nest 回滚组合由 `TestNestUndoRollbackRestoresCombatStateExactly` 等覆盖。
7. **性能**：每次 `Combatant()` 多一次小 map 分配（nil 时没有）。
8. **未验证**：无。
9. **review 检查点**：`combat.Combatant` 以后若新增引用类型字段，`cloneCombatant` 要同步（目前只有这一张 map）。

<a id="skill-3"></a>
### SKILL-3 skillsync 可见性三条路径同一规则（NC-114 / NC-115）

[说明](guide-cfg-skill-noncore.md#skill-3)

1. **提交与版本**：`1b2a51c4`（登记）、`f37a94e3`（NC-114～117 修复）。首发 v1.20.1。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `skill/skillsync/coordinator.go:472` `presentationReset` | reset 的唯一构造点；游标过期的 Flush 与 Recover 都调用它 |
   | `skill/skillsync/coordinator.go:495` `activePresentationEvent` | 把 `ActivePresentation` 还原成 Runtime 为它发出的增量事件形状，交给 `FilterPresentation` |
   | `skill/skillsync/visibility.go:383` `abilityHandleReference` | ability 快照与增量共用按 handle 的 `FieldVisible` 键 |
   | `skill/runtime_mutation.go` `diffCastStates` / `diffProcessStates` | cast remove 带 `Caster`、process remove 带 `Owner`（增量写点与全量 diff 共用） |

3. **不变量**：同一 observer 在快照、增量、reset 三条路径上看到的可见集合一致；不可见实体的 remove 不下发。守卫：`skill/skillsync/visibility_recovery_promises_test.go:77` / `:95` / `:119` / `:144`、`skill/runtime_mutation_remove_identity_promises_test.go:9`。
4. **控制流**：Flush / Recover → `presentationReset` → 每条持续表现 → 策略过滤（不可见整条去掉、可见的取回过滤后的 Anchor）。
5. **失败处理**：策略报错 fail-closed，计 `VisibilityFailures`；去掉的计 `Filtered`。
6. **测试**：修前红（问题记录原文）：

   ```text
   --- FAIL: TestPresentationResetFromRecoverHonoursVisibility (0.00s)
       visibility_recovery_promises_test.go:92: Recover reset leaked to an observer that cannot see entity 1: {Kind:cast ... CastID:1 ... CastStatus:suspended ... Anchor:{Source:1 Target:2 Position:<nil> Direction:<nil> Path:[]}}
   --- FAIL: TestStateSnapshotAndDeltasHideTheSameAbility (0.00s)
       visibility_recovery_promises_test.go:140: snapshot abilities = [{Owner:1 Handle:5 ...}]: deltas hide ability 5 but the snapshot sends it
   --- FAIL: TestRemoveMutationsOfInvisibleEntitiesAreFiltered (0.00s)
       visibility_recovery_promises_test.go:166: cast_remove of entity 1's cast 1 allowed=true err=<nil>; its upserts were filtered
       visibility_recovery_promises_test.go:174: persistent_remove bound to invisible entity 1 allowed=true err=<nil> binding={Owner:1 Subject:1 Team:0}
   --- FAIL: TestRemoveMutationsCarryTheEntityTheirUpsertCarried (0.00s)
       runtime_mutation_remove_identity_promises_test.go:28: cast remove = {... Kind:cast_remove CastID:3 Caster:0 ...}, want Caster 7 (the upsert's caster)
       runtime_mutation_remove_identity_promises_test.go:31: process remove = {... Kind:process_remove ... Owner:0 ... ProcessID:4 ...}, want Owner 7 (the upsert's owner)
   ```

   修后：`go test ./skill/skillsync -run 'TestPresentationResetFrom|TestStateSnapshotAndDeltasHideTheSameAbility|TestRemoveMutationsOfInvisibleEntitiesAreFiltered' -count=1` 与 `go test ./skill -run TestRemoveMutationsCarryTheEntityTheirUpsertCarried -count=1` 通过。
7. **性能**：无。
8. **未验证**：经 kit syncstream / NATS 的端到端（外部 E04）。
9. **review 检查点**：
   - reset 里的 process 条目没有专门用例（只经源码推导与 `presentationReset` 的通用断言覆盖）：核对 `activePresentationEvent` 对 process 条目还原出的事件形状与 Runtime 增量里的 process 事件一致。
   - `rg 'PresentationSnapshot\(\)' skill/skillsync` 是否只在 `presentationReset` 里被投影给 observer；Applier 的 `decodeStrict` 是否认识新增的 `caster` / `owner`（同一结构体，新旧互通）。

<a id="skill-4"></a>
### SKILL-4 Applier 被拒包不改 epoch（NC-116）

[说明](guide-cfg-skill-noncore.md#skill-4)

1. **提交与版本**：`f37a94e3`。首发 v1.20.1。
2. **改动**：`skill/skillsync/applier.go:186-194` `admit` 先记局部 `switchesEpoch`，全部准入检查通过、占用 inflight 时才置 `pendingEpoch`。
3. **不变量**：状态只在准入成功处改一次。守卫：`TestRejectedFullPacketDoesNotWedgeTheApplier`（`skill/skillsync/applier_epoch_admission_promises_test.go:15`，第一包与 epoch 切换两种）。
4. **控制流**：`admit` → 各项检查 → 成功才写状态。
5. **失败处理**：被拒的包不留状态。
6. **测试**：修前红：

   ```text
   --- FAIL: TestRejectedFullPacketDoesNotWedgeTheApplier (0.00s)
       applier_epoch_admission_promises_test.go:36: first epoch: valid full after a rejected one = {Applied:false Duplicate:false Sequence:0 Epoch:0}, skillsync: another packet is being applied to the stream (epoch 0); the rejected packet wedged the applier
   ```

   修后通过。
7. **性能**：无。
8. **未验证**：经真实传输的端到端（外部 E04）。
9. **review 检查点**：`admit` 里是否还有在拒绝分支之前改 `applier.*` 字段的写法。

<a id="skill-5"></a>
### SKILL-5 提交前失败的 cast 有界回收（NC-117）

[说明](guide-cfg-skill-noncore.md#skill-5)

1. **提交与版本**：`f37a94e3`。首发 v1.20.1。
2. **改动**：`skill/runtime_retention.go:20` `trackCompletedCastLocked`（收所有终态）、`:41` `forgetCompletedCastLocked`（`startLocked` 删除失败启动、复用 ID 时撤掉刚登记的条目）。
3. **不变量**：完成队列与 checkpoint 恢复对“完成”的口径一致，按 `CompletedCastLimit` 回收。守卫：`TestUncommittedFailedCastsStayWithinTheCompletedCastLimit`（`skill/runtime_failed_cast_retention_promises_test.go:35`）、`TestFailedStartLeavesNoCompletedQueueEntry`（`:71`，负对照：修前也通过）。
4. **控制流**：cast 进终态 → 入队 → 超限淘汰最老可回收者（产生 `cast_remove`）。
5. **失败处理**：无。
6. **测试**：修前红：

   ```text
   --- FAIL: TestUncommittedFailedCastsStayWithinTheCompletedCastLimit (0.00s)
       runtime_failed_cast_retention_promises_test.go:46: retained casts = 5 (completed queue 0) after 5 pre-commit failures; CompletedCastLimit is 2
       runtime_failed_cast_retention_promises_test.go:54: restore after pre-commit failures: skill: runtime checkpoint is corrupt
   ```

7. **性能**：无。
8. **未验证**：无外部项。生产 Host 的 checkpoint 与世界成对恢复没有正式接线（N09 O3，第十二轮“skill 剩余观察：其余保持”），不存在可验证的生产路径；本条只在 MemoryHost 上成立。
9. **review 检查点**：失败启动登记时若队列已满，会先淘汰一条更老的终态 cast 再被撤掉——确认这只损失可检查的历史、不影响正确性。

<a id="skill-6"></a>
### SKILL-6 wire 字段名逐字匹配（NC-150）

[说明](guide-cfg-skill-noncore.md#skill-6)

1. **提交与版本**：`7a874663`（登记 NC-150～154）、`bfd353c0`（修复）。首发 v1.20.2。
2. **改动**：`skill/parse.go:196` `decodeStrictSingle` 末尾调用 `requireExactFieldNames`；`skill/parse_duplicate.go:144` `requireExactFieldNames`、`:214` `structFieldNames`（按类型缓存）；`skill/wire_select.go` 两个过滤器解码结构体补 `json` tag。
3. **不变量**：所有 wire 对象经同一个严格解码入口；以名字为键的 map 只递归值；自带 `UnmarshalJSON` 的类型跳过（内部再走严格解码）。守卫：`TestParseRejectsCaseVariantKeys`（`skill/parse_exact_keys_promises_test.go:12`，10 个子用例）、负对照 `TestParseKeepsNameKeyedMapsAndCanonicalKeys`（`:47`）。
4. **控制流**：Parse / checkpoint 恢复 → `decodeStrictSingle` → encoding/json（`DisallowUnknownFields`）→ `requireExactFieldNames`。
5. **失败处理**：`json: unknown field "X" (field names are case-sensitive)`。
6. **测试**：修前红（10 个子用例全部 FAIL）：

   ```text
   --- FAIL: TestParseRejectsCaseVariantKeys/top-level_duplicate_by_case
       parse_exact_keys_promises_test.go:40: Parse accepted a case-variant or unknown key; id="skill.test.other" cooldown=0
   ```

   修后通过；36 个 fixture、fuzz 种子、skillsync / combatcomponent（RuntimeValue JSON 往返）race×3 通过。
7. **性能**：每个对象多一次 map 解码，只发生在 Parse / checkpoint 恢复。
8. **未验证**：无。仓外的非 Go JSON 生产者若写非规范大小写会被拒，属兼容说明（见说明文档 SKILL-6“兼容”）。
9. **review 检查点**：用 AST 扫描确认 wire / value / parse 里没有别的无 tag 解码目标（记录写已扫过，新增解码结构体时要补 tag）。

<a id="skill-7"></a>
### SKILL-7 不派发的 phase 事件与 `timeout_ticks`（NC-151）

[说明](guide-cfg-skill-noncore.md#skill-7)

1. **提交与版本**：`bfd353c0`。首发 v1.20.2。之后 `023eb276`（B3 ②）把事件集合收进 `phaseEventTable`（SKILL-13）。
2. **改动**：`skill/compile_lifetime.go:192` `requireDispatchedPhaseEvents`（`:12` 调用）；fallthrough 条件去掉 `timeoutTicks == 0`；删除 `skill/testdata/recast_combo.json`；测试辅助改为 `sequence[进程效果, wait N → finish]`。
3. **不变量**：编译通过的定义里不含 Runtime 没有派发点的事件。守卫：`skill/phase_event_dispatch_promises_test.go:14` / `:31` / `:53`；控制 `skill/generated_definitions_compile_test.go`（`roost add skill` 骨架与 game-demo `fireball.json.tmpl` 零诊断，O19 的补位）。
4. **控制流**：生命期 pass → 每个 phase → `requireDispatchedPhaseEvents`。
5. **失败处理**：recast / timeout 为 error；`timeout_ticks > 0` 为 warning。
6. **测试**：修前红：

   ```text
   --- FAIL: TestCompileRejectsPhaseEventsTheRuntimeNeverDispatches/recast
       phase_event_dispatch_promises_test.go:24: on.recast compiled into a Program the Runtime never runs it from; diagnostics=[]skill.Diagnostic(nil)
   --- FAIL: TestCompileRejectsPhaseEventsTheRuntimeNeverDispatches/timeout
       phase_event_dispatch_promises_test.go:24: on.timeout compiled into a Program the Runtime never runs it from; diagnostics=[]skill.Diagnostic(nil)
   --- FAIL: TestPhaseTimeoutTicksDoNotExcuseEnterFallthrough
       phase_event_dispatch_promises_test.go:40: fallthrough enter with timeout_ticks compiled; Activate = skill: immutable program invariant failed (invariant=true)
   --- FAIL: TestNonzeroPhaseTimeoutTicksWarnsThatItIsNotEnforced
       phase_event_dispatch_promises_test.go:64: missing timeout_ticks warning: []skill.Diagnostic(nil)
   ```

7. **性能**：无。
8. **未验证**：无。game 服 Init 用 `skills.CompileAll` 编译全部定义（`demo/internal/service/game/service.go.tmpl:61`），之后两次在隔离环境起生成的 game 服并就绪都经过这条编译（v1.21.0 CFG-7 端到端、v1.23.0 CFG-12）。checkpoint 里的 `phase_timeout` 任务当时仍可恢复（O20），v1.23.0 由 SKILL-21 收口。
9. **review 检查点**：game-demo 机器人 `cmd/loadtest` 断言 `Warnings == 0`——生成模板里的技能定义不应带 `timeout_ticks`。

<a id="skill-8"></a>
### SKILL-8 tick 非负集中检查（NC-152）

[说明](guide-cfg-skill-noncore.md#skill-8)

1. **提交与版本**：`bfd353c0`。首发 v1.20.2。
2. **改动**：`skill/compile_shape.go:128` `requireNonNegativeAuthoredTicks`（`:13` 调用）。
3. **不变量**：作者写的 tick 字段在 shape pass 以字段路径报负值。守卫：`TestCompileRejectsNegativeTicksAtTheirField`（`skill/compile_tick_sign_promises_test.go:13`）、负对照 `TestCompileAcceptsZeroTicks`（`:43`）。
4. **控制流**：shape pass 最早执行，负 wait 不再走到预算 pass。
5. **失败处理**：`SHAPE_INVALID`。
6. **测试**：修前红（6 个子用例）：

   ```text
   --- FAIL: …/cooldown_ticks: negative cooldown_ticks compiled; diagnostics=[]skill.Diagnostic(nil)
   --- FAIL: …/wait_ticks: missing error SHAPE_INVALID at $.phases[0].on.enter.ticks in []skill.Diagnostic{… Code:"BUDGET_EXCEEDED", Path:"$", Message:"lifetime_ticks budget 9223372036854775807 exceeds limit 36000"}
   --- FAIL: …/chain_hop_interval_ticks: negative chain hop_interval_ticks compiled; diagnostics=[]skill.Diagnostic(nil)
   （phase_timeout_ticks、repeat_interval_ticks、add_status_duration_ticks 同形）
   ```

7. **性能**：无。
8. **未验证**：无。编译器其余 pass 的 Tick 字段由各自既有检查负责，审查时用 37 fixture × 每个 tick 字段改成 -1 扫描确认全部被拒。
9. **review 检查点**：未采用“`Tick` 类型自带拒绝负数的 `UnmarshalJSON`”（`Tick` 也用于 Runtime / checkpoint / sync 的 JSON），确认没有人以后这样改。

<a id="skill-9"></a>
### SKILL-9 表现缓存与 skillcompose（NC-153 / NC-154）

[说明](guide-cfg-skill-noncore.md#skill-9)

1. **提交与版本**：`bfd353c0`。首发 v1.20.2。
2. **改动**：`skill/presentation_asset_cache.go:60-73` 条目新增 `abandoned`（加载失败且创建者 ctx 已结束时置位），`Acquire` 改循环、`acquireAsset` 拆出 `acquireAssetOnce`；`skill/skillcompose/validator.go:30` 补 `PROVENANCE_MISMATCH`（“candidate source is blank or duplicated”）。
3. **不变量**：等待者的结果只由真实失败或自己的取消决定。重试次数以“依次取消的创建者个数”为界。守卫：`TestVisualPlanCacheWaiterSurvivesCreatorCancellation`（`skill/presentation_asset_cache_cancel_promises_test.go:90`）、`TestVisualAssetWaiterSurvivesCreatorCancellation`（`:149`）、控制 `TestVisualPlanCacheWaiterSeesRealLoadFailureAndOwnCancellation`；`TestValidateCandidateExplainsBlankOrDuplicateSources`（`skill/skillcompose/validator_provenance_promises_test.go:12`）。
4. **控制流**：等待者醒来 → 看到 `abandoned` 且自己 ctx 有效 → 重新进入获取（失败条目已删，它成为新创建者）。
5. **失败处理**：真实失败照旧传给所有等待者。
6. **测试**：修前红：

   ```text
   presentation_asset_cache_cancel_promises_test.go:128: waiter with a live context returned before reloading: lease=false err=preload "asset-impact": context canceled
   presentation_asset_cache_cancel_promises_test.go:178: asset waiter returned before reloading: context canceled
   validator_provenance_promises_test.go:39: invalid report without PROVENANCE_MISMATCH: []skillcompose.Diagnostic(nil)
   ```

   资产层用例给等待者 20ms 有界窗口进入等待，只影响修前能否复现；修后两种顺序都通过，`-race -count=5` 通过。
7. **性能**：只在别人取消时多一次加载。
8. **未验证**：无外部项。加载器由业务客户端提供，仓内没有真实加载器，用例用可控的假加载器覆盖“创建者取消”与“真实失败”两种时序；O18（取消不触发空闲淘汰）按第十二轮“skill 剩余观察：其余保持”不改。
9. **review 检查点**：plan 层用例是否断言释放后 asset 引用归零（防止重试路径漏还引用）。

<a id="skill-10"></a>
### SKILL-10 编译器只接受 Runtime / Host 执行的范围（NC-210、212～215）

[说明](guide-cfg-skill-noncore.md#skill-10)

1. **提交与版本**：`caf9837e`（登记 NC-210～216）、`7cf86f98`（修复 + 变异性质测试）。首发 v1.20.2。
2. **改动与符号**：

   | 编号 | 位置 |
   | --- | --- |
   | NC-210 | `skill/compile_typecheck.go:544` `declaredMemory`（set / add / clear_memory 共用） |
   | NC-212 | `skill/compile_authority.go:113` `validateCatalogHandles` 末尾、`:141` `checkKeys` |
   | NC-213 / NC-215 | `skill/compile_shape.go:173` `rejectValuesTheRuntimeDoesNotExecute`（`:14` 调用） |
   | NC-214 / NC-215 | `skill/compile_capability.go:109` `validateCatalogReferences`、`:61` `validateTargetFilterList` |
   | 性质测试 | `skill/compile_mutation_property_test.go` |

3. **不变量**：编译通过 ⇒ Runtime 与参考 Host 能执行。守卫：`skill/compile_runtime_agreement_promises_test.go:54` / `:84` / `:132` / `:161` / `:182`；控制 `TestDeclaredMemoryEffectsWriteTheirOwnSlot`、`TestCatalogConsistentEffectsStillCompileAndRun`；变异性质测试（见本主题开头）。
4. **控制流**：shape → typecheck → authority / capability → lower。
5. **失败处理**：各自诊断码，指向字段路径。
6. **测试**（修前红，均为记录原文）：
   - NC-210：`TestCompileRejectsMemoryEffectsOnUndeclaredOrNonIntMemory` 5 个子用例 `set undeclared compiled; diagnostics=[]skill.Diagnostic(nil)` 等；性质测试 `charge_projectile.json drop $.memory.charged: … activate[0]: skill: immutable program invariant failed`。
   - NC-212：`missing error CATALOG_DUPLICATE_HANDLE at $.gameplay.unit_templates[1].key in []skill.Diagnostic{}`。
   - NC-213：`TestCompileRejectsFieldsTheRuntimeDoesNotExecute` 四个子用例 `compiled; diagnostics=[]skill.Diagnostic(nil)`。
   - NC-214：`TestCompileRejectsUnknownCatalogNames` 八个子用例 `compiled; diagnostics=[]skill.Diagnostic(nil)`。
   - NC-215：`TestCompileRejectsValuesEveryHostRejects` 七个子用例 `compiled; diagnostics=[]skill.Diagnostic(nil)`；探针在 MemoryHost 上施法分别得到 `unsupported modifier operation "set"`、`modifier duration must be positive`、`status duration must be positive`、`unsupported resource operation "mul_bp"`、`runtime value type mismatch`。
7. **性能**：无。
8. **未验证**：无。Host 之间口径不同的取值（shield 时长、relation 取值，O24 / O25）按第十二轮“skill 剩余观察：其余保持”不动；cost 表达式的符号由运行期 `payCostList` 拒绝。
9. **review 检查点**：`compile_shape.go:173` 里每个“只接受默认值”的字段是否在 `ChainSelectShape` / `AttributeModifierCommand` 里确实没有对应字段（若以后实现方向 A，这里要同步放宽）。

<a id="skill-11"></a>
### SKILL-11 移交后 area finish（NC-211）

[说明](guide-cfg-skill-noncore.md#skill-11)

1. **提交与版本**：`7cf86f98`。首发 v1.20.2。
2. **改动**：`skill/process_owned.go`：`runOwnedProcessCallback`（`:441`）对已移交或拥有者已终态的 finish 只置 `areaCallbackFinishedCast`；`advanceOwnedProcesses` 派发信号后看到标记即按 `StopCauseCancel` 停进程（`:92`、`:332`、`:394`）。
3. **不变量**：施法存活时 finish 结束施法；已移交时只结束本 area 进程，不跑 end / cancel 回调。守卫：`TestHandedOffAreaFinishEndsOnlyTheAreaProcess`（`skill/area_handoff_finish_promises_test.go:21`）、控制 `TestLiveAreaFinishStillFinishesTheCast`（`:72`）、既有 `TestAreaCallbackFinishStopsRemainingSignals`、`TestAreaFinalLeaveFinishSuppressesTerminalCallback`；性质测试种子 `seed.area_handoff`。
4. **控制流**：信号派发 → 回调 finish → 标记 → 同一 tick 停进程并回收。
5. **失败处理**：标记只在同一次 Advance 内生效，不进 checkpoint。
6. **测试**：修前红：

   ```text
   leave_after_handoff: Advance(2) = skill: immutable program invariant failed; a compiled area finish must not fail the runtime
   enter_after_handoff: Advance(1) = skill: immutable program invariant failed; a compiled area finish must not fail the runtime
   ```

7. **性能**：无。
8. **未验证**：无（不适用：combatcomponent 宿主不实现 `OwnedEntityRuntimeHost`，不跑 owned 进程）。
9. **review 检查点**：停止过程中（`terminateProcess` 的 leave）触发的 finish 是否不会重复停止。

<a id="skill-12"></a>
### SKILL-12 NegotiateSchema 拒绝空区间（NC-216）

[说明](guide-cfg-skill-noncore.md#skill-12)

1. **提交与版本**：`7cf86f98`。首发 v1.20.2。
2. **改动**：`skill/skillsync/schema.go:34` `NegotiateSchema` 合并前检查两边。
3. **不变量**：协商结果被两边 `Contains`。守卫：`TestNegotiateSchemaRequiresBothRangesToContainTheResult`（`skill/skillsync/schema_negotiation_promises_test.go:12`）；控制 `skillsync_test.go:308`。
4. **控制流**：无。
5. **失败处理**：`ErrSchemaNegotiationFailed`。
6. **测试**：修前红：`NegotiateSchema({0 5}, {2 3}) = 3; server contains=false client contains=true, want ErrSchemaNegotiationFailed`。
7. **性能**：无。
8. **未验证**：无（仓内无生产调用方）。
9. **review 检查点**：无。

<a id="skill-13"></a>
### SKILL-13 lower fail-fast 与 phase 事件单一来源（B3）

[说明](guide-cfg-skill-noncore.md#skill-13)

1. **提交与版本**：`023eb276`（分支 `b7c7b3`）、`3a71321c`（标注）。首发 v1.20.2。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `skill/lower.go:33` `resolveName[T]` | 所有“名字 → 槽位 / handle”查找的唯一入口；查不到记 `LOWER_UNRESOLVED`（带源路径），返回零值继续以一次报全 |
   | `skill/diagnostic.go:58` `DiagnosticLowerUnresolved` | 新诊断码 |
   | `lowerProgram` | 签名改为 `(*Program, []Diagnostic)`（包内）；有失败即不交出 Program |
   | `skill/phase_events.go:25` `phaseEventTable`、`:46` `dispatchedPhaseEventFlows`、`:57` `undispatchedPhaseEventFlows` | 事件名与是否派发只写一次；lower 导出、编译期拒绝、Runtime 派发点共用同名常量 |

3. **不变量**：lower 不产出指向槽位 / handle 0 的兜底 Program。守卫：`TestLowerRefusesEveryUnresolvedLookup`（约 590 次删除）、`TestPhaseEventTableIsTheSingleSource`。
4. **控制流**：`Compile` → 各 pass → `lowerProgram`（查找经 `resolveName`）→ 有 `LOWER_UNRESOLVED` 即返回诊断。
5. **失败处理**：编译器自身不变量（产物不完整、IR 形状未知、operation 与 identity 计数不符）仍 panic。
6. **测试**：修前红（旧 lower 包成新签名，方案 §2.1 原文节选）：

   ```text
   status_cleanse.json: drop authority.statuses["slow"]: lower silently produced a different Program instead of a compile error (zero-value fallback)
   ammo_burst.json: drop input slot "$input.target": lower produced a Program for an unresolved reference instead of a compile error
   charge_projectile.json: drop memory "charged": panic=<nil> errors=false, want a LOWER_UNRESOLVED compile error for a referenced memory
   persistent_mark.json: drop state.plans["marks"]: lower panicked (skill: unresolved state reference), want a LOWER_UNRESOLVED compile error
   （共 35 处静默兜底、23 处未解析引用照样出 Program、4 处 panic）
   ```

   修后通过；全部既有用例与 digest 不变。
7. **性能**：无。
8. **未验证**：无。B3 ③（Host 取值能力表）维护者定为下个大版本；process callback 事件是另一张表，不在本项。
9. **review 检查点**：
   - `rg 'artifacts.authority.statuses\[' skill/lower.go` 应无直接 map 读（都经 `resolveName`）。
   - B3 后的回归 NC-223（SKILL-14）说明：`resolveName` 在空作用域下会把合法的局部变量引用判为未解析——lower 快照计划时要用读取处的作用域。

<a id="skill-14"></a>
### SKILL-14 按 Runtime 求值上下文收紧（NC-220～224）

[说明](guide-cfg-skill-noncore.md#skill-14)

1. **提交与版本**：`5c04726f`（分支 `revn09e`）、`86882561`（标注）。首发 v1.21.0。v1.21.0 内随后 `4ed038d9`（SKILL-15）把 NC-220 / NC-224 的按 pass 检查收拢到求值上下文表：发版提交上 `compile_snapshot.go` 的 `snapshotCapturableWhereRead` / `readsInsideProcessCallbacks` 与 `compile_owned_entity.go` 的 `validateDetachedProcessFields` 已不存在（`rg` 无结果），判断改由表完成（NC-220 / NC-224 修复记录已加后注）。
2. **改动（发版提交上的位置）**：

   | 编号 | 位置 |
   | --- | --- |
   | NC-220 | `skill/compile_typecheck.go:882` `checkCachedRead` + `skill/eval_contexts.go:397` `evalSnapshotTable` |
   | NC-221 | `skill/compile_shape.go:74-81`（被动 `max_depth` 与 `input_schema`） |
   | NC-222 | `skill/compile_shape.go:100`、`:103`（`only a spawn effect starts a process` / `has process callbacks`） |
   | NC-223 | `skill/lower.go:22-24` `loweringContext.readEntities`（读取处 lower 出的实体按源路径记下，`lowerSnapshots` 用它） |
   | NC-224 | 表的 process_step 列不含 `$input` / `$memory` / `$local`（`skill/eval_contexts.go:194` 起） |

3. **不变量**：编译接受的读取在它实际求值的上下文里都能求出。守卫：`skill/compile_capture_context_promises_test.go:37` / `:119` / `:159` / `:176` / `:209`，控制 `:68` / `:136` / `:230`；性质测试新种子 `seed.process_start_read`、`seed.passive_entity_input`、`seed.local_attribute_read`。
4. **控制流**：见 SKILL-15。
5. **失败处理**：`ATTRIBUTE_SNAPSHOT_INVALID` / `SHAPE_INVALID` / `INPUT_UNAVAILABLE`。
6. **测试**（修前红，记录原文）：
   - NC-220：六个子用例，三个 `compiled; diagnostics=[]skill.Diagnostic(nil)`，三个 `missing error ATTRIBUTE_SNAPSHOT_INVALID at ….read_attribute.entity in [… Code:"LOWER_UNRESOLVED", Path:"$" …]`；探针 Activate 返回 `skill: immutable program invariant failed`。
   - NC-221：`TestCompileRejectsPassivesThatCanNeverActivate` 五个子用例 `compiled; diagnostics=[]skill.Diagnostic(nil)`。
   - NC-222：`TestCompileRejectsProcessesOnEffectsThatDoNotSpawn` 三个子用例同上。
   - NC-223：`TestReadsOfALocalEntityAtNonCachedPointsCompileAndRun` 四个子用例 `unexpected diagnostic: … Code:"LOWER_UNRESOLVED", Path:"$" …`；同一用例在 `023eb276^` 上通过（回归证据）。
   - NC-224：五个子用例（临时还原实现）`compiled; diagnostics=[]skill.Diagnostic(nil)`；性质测试 `seed.process_start_read $.phases[0].on.enter.steps[1].process.area.from=$input.target: compiled without errors but advance[0] 1: skill: immutable program invariant failed`。
   - 修后通过；42 个既有种子 gameplay / presentation digest 逐一相同。
7. **性能**：无。
8. **未验证**：无。说明：编译期按字段所在的求值上下文（process_step 列）判断，不看进程实际活几步，所以“进程只活一步、从不走到移交后求值”的定义同样被拒绝——这是表的设计（O33 之后同一列的漂移格子也一律拒绝），不是漏验。
9. **review 检查点**：NC-223 的修法让非缓存型采样点的计划实体取读取处的值——确认 `lowerSnapshots` 对没有记录的计划仍按空作用域 lower 并在查不到时报错（不再定位在 `$`，而是计划的源路径）。

<a id="skill-15"></a>
### SKILL-15 求值上下文表（第五轮，NC-280～283）

[说明](guide-cfg-skill-noncore.md#skill-15)

1. **提交与版本**：`0aeb5ab6`（记录决定）、`4ed038d9`（实施，分支 `revn09f`）、`bc98f05c`（标注）。首发 v1.21.0。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `skill/eval_contexts.go:37` `evalContext`（8 个值） | 求值上下文；零值是施法流程 |
   | `skill/eval_contexts.go:194` `evalReferenceTable` | 每行一个引用（或一类），每格可用 / 不可用 + 一句中文语义 |
   | `skill/eval_contexts.go:371` `ErrReferenceOutOfContext` | Runtime 查表拒绝时的错误，点名上下文与表项；不包裹 `ErrProgramInvariant` |
   | `skill/eval_contexts.go:397` `evalSnapshotTable` | 三个缓存型快照点能写在哪个上下文 |
   | `skill/compile_typecheck.go:121` `scopeFor`、`:707` `referenceType`、`:882` `checkCachedRead` | 作用域由表生成；不可用报 `INPUT_UNAVAILABLE`；快照按采样上下文检查 |
   | `skill/lower.go:814` `lowerReference`；`program_operation.go` `referenceProgramValue.row` | 引用带表行号（不进 digest）；builtin / 输入槽位的投射拆成根 + field（NC-283） |
   | `skill/runtime_eval.go:107` `evalReference` | 先查 `cast.evalContext` 的格子 |
   | `skill/runtime.go:161-163` `castInstance.evalContext`；`skill/process_owned.go:109` `detachedProcessCast(process, evalContext)`；`skill/runtime_state.go:95` `evalStateDefault` | 各求值点切到对应上下文 |

3. **不变量**：编译期与 Runtime 对每个（上下文，引用）格子的判断一致。守卫：

   | 守卫 | 位置 | 内容 |
   | --- | --- | --- |
   | `TestEvalContextTableEveryCellHasACase` | `eval_contexts_table_test.go:259` | 每行有 fixture，每格有语义与用例或“没有该类型位点”的理由；加行 / 加上下文不补用例即失败 |
   | `TestEvalContextTableCellsAgreeWithCompilerAndRuntime` | `:290` | 192 格逐格：可用格编译无 error、施法 10 tick、见证效果执行；不可用格编译被拒且诊断点名上下文与表项（143 格有用例，49 格无该类型位点） |
   | `TestRuntimeEvaluatesReferencesOnlyWhereTheTableAllows` | `:333` | 192 格的 Runtime 一半：不可用格 `ErrReferenceOutOfContext` |
   | 性质测试 | `compile_mutation_property_test.go` | Runtime 返回 `ErrReferenceOutOfContext` 同样算失败；补 3 个种子 |

   变异检查：把 state_default 列的 `$input.*` 临时改成可用，新种子当场报 `seed.state_default_context $.persistent_state.who.default=$input.target: … immutable program invariant failed`。
4. **控制流**：

   ```text
   Compile: typeChecker.scopeFor(上下文) 按表生成作用域 → referenceType 查格子 → checkCachedRead 查快照点表
   Runtime: 进入求值点（Activate memory 默认值 / 采样 / 进程启动 / 进程每步 / 进程回调 / 状态默认值）设 cast.evalContext
            → evalReference 查同一格子 → 不可用返回 ErrReferenceOutOfContext（只有编译器漏了位点才会出现）
   ```

5. **失败处理**：编译期诊断原样带出格子的 semantics（含替代写法）。
6. **测试**（修前红，记录原文）：
   - NC-280：`later slot reads an earlier one: attempt 0 compiled (cast error <nil>); the compile result must not depend on map order`；另一顺序被拒时码为 `REFERENCE_UNKNOWN`。
   - NC-281：`cast input: state default "$input.target" compiled; casting it: skill: immutable program invariant failed`；memory 子用例修前是 `LOWER_UNRESOLVED`。
   - NC-282：`TestCompileRejectsOptionalEntitiesInCachedReads` 四个子用例 `… compiled; casting without it: skill: runtime value type mismatch`。
   - NC-283：`TestProjectedReferencesCompileAndRun` 六个子用例 `LOWER_UNRESOLVED` / `immutable program invariant failed: builtin "…"`。
   - 用例位置：`skill/eval_context_promises_test.go:46` / `:90` / `:133` / `:177`；控制 `:72` / `:116` / `:157`。修后通过；45 个既有种子 gameplay / presentation digest 修前修后相同。
   - 复跑：`go test -race -count=3 ./skill/...`、`SKILL_MUTATION_FULL=1` 性质测试。
7. **性能**：无基准。
8. **未验证**：无。`$caster` 在进程回调里 Runtime 其实求得出（= owner），表维持编译期不可用：第七轮 O34～O36“保持现状并写进作者文档”。
9. **review 检查点**：
   - 新增一个求值点时，是否在 Runtime 那一侧设置了 `evalContext`（否则零值是施法流程，会放过表外引用）。看 `rg 'evalContext:' skill/*.go` 的赋值点是否覆盖 8 个上下文。
   - `referenceProgramValue.row` 确实不进 digest：`programValueDigest` 不读 `row`。
   - 按 pass 各写一份的检查已删除（`rg 'validateDetachedProcessFields|detachedReferenceAllowed|snapshotCapturableWhereRead' skill` 无结果）；回调不能 finish / goto / wait / 递归建进程 / 改 memory 的控制流规则仍在 `compile_owned_entity.go`。

<a id="skill-16"></a>
### SKILL-16 O33 漂移格子编译期拒绝；O34～O36

[说明](guide-cfg-skill-noncore.md#skill-16)

1. **提交与版本**：`4da5e7ea`（记录第七轮决定）、`f6043e44`（实施，分支 `o33`，基线 `4da5e7ea`；同提交含 O37 account，见 NONCORE）、`2a7d2a65`（标注；提交说明里的 `a6e75488` 是 rebase 前的号，不在 main 上，正确的是 `f6043e44`；DECISIONS-PENDING 第七轮表下已加更正注）。首发 v1.21.0。
2. **改动**：`skill/eval_contexts.go`：21 格改为不可用（引用表 process_step / state_default 两列 8 行 = 16 格；快照点表 process_step / state_default 两列的 cast_start、phase_start = 4 格；memory_default 列的 phase_start = 1 格）；`evalDrifting` 删除，`usable()` 改为 `== evalAvailable`；每格 semantics 写“为什么没有、此前实际得到的值、改用 …”。Runtime 不用改（本来查同一张表）。作者文档、`ai-skill-system-prompt.md` 同步。
3. **不变量**：表里只有两种格子。守卫：`TestProcessStepPrimaryTargetIsRejectedAtCompileTime`（`eval_contexts_table_test.go:366`）、`TestEvalContextTableRejectsTheO33DriftCellsWithAnAlternative`（`:400`，21 格逐格：不可用、semantics 有“改用”、有位点的格子诊断点名上下文 / 表项 / 替代写法）、`TestEvalSnapshotTableCellsAgreeWithCompilerAndRuntime`（`:504`，3 个快照点 × 5 个非采样上下文 = 15 格，此前快照点表没有逐格守卫）、`TestO33AlternativesCompileAndRun`（`:555`，替代写法逐条能编译能跑）。
4. **控制流**：同 SKILL-15。
5. **失败处理**：`INPUT_UNAVAILABLE`（引用）/ `ATTRIBUTE_SNAPSHOT_INVALID`（快照）。
6. **测试**：修前红（基线 `4da5e7ea`，只加测试，方案 §8.4 原文）：

   ```text
   --- FAIL: TestProcessStepPrimaryTargetIsRejectedAtCompileTime
       $primary_target in a process field compiled; it drifts to the lifecycle entity after handoff (O33)
   --- FAIL: TestEvalContextTableRejectsTheO33DriftCellsWithAnAlternative
       row $primary_target context process_step: usable=true semantics "启动那一步是施法的主目标；移交后是进程的 lifecycle 实体（O33）"; O33 cells are rejected and name an alternative
       …（21 格各一行）
       snapshot phase_start context memory_default: usable=true semantics "Activate 时的值：memory 初始化早于第一个 phase 开始"; …
   --- FAIL: TestEvalSnapshotTableCellsAgreeWithCompilerAndRuntime/process_start/cast_flow
       no ATTRIBUTE_SNAPSHOT_INVALID diagnostic naming cast_flow and row process_start: … (evaluation context snapshot table)
   ```

   修后通过。性质测试 48 个种子全部编译、施法通过；`compiled_and_run` 8758 → 8725（33 个变异落在新拒绝的格子上），digest 性质 `compared=147` 不变。
7. **性能**：无。
8. **未验证**：无（仓内 36 个 fixture、examples、`roost add skill` 骨架、game-demo `fireball.json.tmpl` 都不用漂移格子）。
9. **review 检查点**：
   - 21 格的 semantics 都含“改用”（守卫已查）；替代写法里排除了两条看似可行的写法（绑定到进程数值属性的 motion 字段只收字面量；`set_memory` 不能存属性读取），确认作者文档没有写回它们。
   - 作者文档 `docs/skill/skill-casting-and-combat.md` 第 80 行标题原写“（O33，未发版）”，已改为“（O33，v1.21.0）”（本次重核）；确认同文其余“未发版”标注（O22 / O29 / O2）只指 v1.23.0 本版的内容。

<a id="skill-17"></a>
### SKILL-17 O22 summon 字段编译期拒绝

[说明](guide-cfg-skill-noncore.md#skill-17)

1. **提交与版本**：`78e26853`（记录第十二轮决定）、`229a5aa0`（实施，分支 `bsk`）、`6bf15516`（标注）。首发 v1.23.0（本版）。
2. **改动**：`skill/compile_motion.go:112` `validateProcessMotion`：`:118` summon 分支不再提前返回，`:125` motion、`:128` `duration_ticks`（“summon processes live for the spawn effect's duration_ticks; remove the process duration_ticks”），`area` / `interval_ticks` / `emit_leave_on_stop` 按非 area 进程同样规则拒绝。
3. **不变量**：编译接受的 summon 进程字段都会被 Runtime 读取。守卫：`TestSummonProcessRejectsFieldsItNeverReads`（`skill/compile_summon_process_promises_test.go:16`，五个子用例）、`TestSummonProcessWithoutDurationCompilesAndLivesForTheSpawnDuration`（`:32`，钉住不写时寿命 = spawn 的 10 tick）。
4. **控制流**：motion pass。
5. **失败处理**：`MOTION_INVALID`。
6. **测试**：修前红：五个子用例（正 / 负 duration、area、interval、emit_leave_on_stop）全部 `missing diagnostic MOTION_INVALID in []skill.Diagnostic(nil)`。修后通过。
7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：Runtime `startEntityProcess`（`process_owned.go`）是否确实只在带 motion / area 的进程上读模板时长——这是“summon 的 duration_ticks 从不被读”的依据。

<a id="skill-18"></a>
### SKILL-18 O7 checkpoint 字节确定

[说明](guide-cfg-skill-noncore.md#skill-18)

1. **提交与版本**：`229a5aa0`。首发 v1.23.0（本版）。
2. **改动**：`skill/runtime_checkpoint.go:555` `checkpointPayloadLocked`：`:635` `ActivePolicies` 按 `(caster, skill)`、`:641` `ProcLedger` 按 `(root, caster, digest)`、`:650` `RootEventCounts` 按 ID、`:668` `AbilityByProgram` 排序。其余列表此前已排序（`:565` 起）。
3. **不变量**：同一状态两次 Checkpoint 字节与 Checksum 相同；恢复与列表顺序无关。守卫：`TestRuntimeCheckpointBytesAreDeterministic`（`skill/checkpoint_deterministic_bytes_promises_test.go:40`，20 次 Checkpoint）、`TestRuntimeRestoresUnsortedLegacyCheckpointLists`（`:78`，倒序的旧样子照常恢复，再 Checkpoint 与排序版逐字节相同）。
4. **控制流**：无。
5. **失败处理**：无。
6. **测试**：修前红：`checkpoint 2 of the same state differs`；恢复乱序 checkpoint 后再 Checkpoint 的字节与排序版不同。修后通过；`FuzzRestoreRuntimeCheckpointNeverPanics` 20s 通过。
7. **性能**：四次排序，只在 Checkpoint 时。
8. **未验证**：无。
9. **review 检查点**：`RuntimeCheckpointVersion` 未变（格式不变是兼容承诺的前提）。

<a id="skill-19"></a>
### SKILL-19 O29 文案；O15 / O16 / O17 / O27 / O28 文档

[说明](guide-cfg-skill-noncore.md#skill-19)

1. **提交与版本**：`229a5aa0`。首发 v1.23.0（本版）。
2. **改动**：`skill/compile_effect_result.go:37`、`skill/compile_status.go:78` 文案；`docs/skill/skill-casting-and-combat.md`“进程、运动与 temporal 的既定语义”“编译期收紧与诊断文案”。
3. **不变量**：规则 `effectResultBranchMaySuspend` 不变。守卫：`TestEffectResultBranchDiagnosticNamesProcessCallbacks`（`skill/effect_result_branch_process_promises_test.go:29`）、行为钉子 `TestEffectResultBranchMayStartAProcessWithoutCallbacks`（`:16`，修前修后都通过）。
4. **控制流**：无。
5. **失败处理**：无。
6. **测试**：修前红：`result branch diagnostic "effect result branches cannot suspend or start a process" must name process on callbacks`。
7. **性能**：无。
8. **未验证**：无。O27 按第十二轮“保持并写作者文档”：口径钉在 MemoryHost（`TestTemporalPassBranches`），自定义 Host 按作者文档自行保持。
9. **review 检查点**：status 实例消费流程的文案与 result 分支是否同一说法（两处代码各写一遍）。

<a id="skill-20"></a>
### SKILL-20 null 默认值实体状态可以 set（RR-20261006-02）

[说明](guide-cfg-skill-noncore.md#skill-20)

1. **提交与版本**：`b8fbcee0`（收尾第 3 批 A4～A6，分支 `cb3`，基线 `8a292a5a`）、`efecb24a`（标注）。首发 v1.23.0（本版）。
2. **改动**：`skill/runtime_state.go:95` `evalStateDefault(cast, state)`：求得的默认值缺省时返回 `MissingRuntimeValue(state.typ)`（`:103`）；`skill/host_state.go` `StateMutationCommand` 注释写明 Default 的类型契约。
3. **不变量**：交给 Host 的状态默认值类型等于 state 声明类型。守卫：`TestNullDefaultEntityStateCanBeSet`（`skill/state_null_default_promises_test.go:15`）。同形分支（`runtime_ability.go:397`、`runtime_cast.go:113`、`combatcomponent/adapter.go:134`）逐一核对不受影响（理由见问题记录）。
4. **控制流**：`modify_state` → `evalStateDefault` → Host `applyStateOperation`。
5. **失败处理**：无。
6. **测试**：修前红：

   ```text
   --- FAIL: TestNullDefaultEntityStateCanBeSet (0.00s)
       state_null_default_promises_test.go:28: modify_state set on a null-default entity state reported a type mismatch: skill: runtime value type mismatch
   ```

   修后：第一次施法写入目标实体、Before 是 entity 类型缺省值，第二次施法 `exists(read_state)` 读到并打伤目标。
7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：lower 仍把 null 默认值按 null 类型写进 Program（digest 不变是兼容前提）——修法只在求值时换类型。

<a id="skill-21"></a>
### SKILL-21 checkpoint 拒绝 `phase_timeout`（RR-20261006-03）

[说明](guide-cfg-skill-noncore.md#skill-21)

1. **提交与版本**：`b8fbcee0`。首发 v1.23.0（本版）。
2. **改动**：`skill/scheduler.go` 删除 `phaseTimeoutTask` 与 `scheduledTaskIdentity` 分支；`skill/runtime_checkpoint.go` 删除编码分支，恢复时 `case "phase_timeout"`（`:1150`）→ `ErrCheckpointCorrupt`（显式分支只为说明，落到 default 也是 corrupt）。
3. **不变量**：恢复不接受 Runtime 不会产生的任务。守卫：`TestCheckpointRestoreRejectsPhaseTimeoutTask`（`skill/checkpoint_phase_timeout_promises_test.go:16`，对照：未改动的 checkpoint 能恢复）。
4. **控制流**：无。
5. **失败处理**：`ErrCheckpointCorrupt`。
6. **测试**：修前红：

   ```text
   --- FAIL: TestCheckpointRestoreRejectsPhaseTimeoutTask (0.00s)
       checkpoint_phase_timeout_promises_test.go:68: checkpoint with a phase_timeout task restored with err=<nil>; phase timeouts are rejected at compile time, so the task must be refused as corrupt
   ```

7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：`rg phaseTimeoutTask skill` 无结果。

<a id="skill-22"></a>
### SKILL-22 statusbridge 示例能运行

[说明](guide-cfg-skill-noncore.md#skill-22)

1. **提交与版本**：`229a5aa0`（示例改写）；门禁 `TestExamplesRun` 在 `ba13cb05`（TOOL 部分）。首发 v1.23.0（本版）。
2. **改动**：`skill/examples/statusbridge/main.go:44` `projectCombat`（业务写的全部投影代码）、`:56` 整段包进 `nest.RunDetachedTransaction`、`:70` `defender.ProjectAttributes(projectCombat)`。投影入口本身（`skill/combatcomponent/component.go:232` `AttributeProjection`、`:251` `ProjectAttributes`、`:262` `deriveProjection`）属于 DAO 部分。
3. **不变量**：示例在 A1 契约下运行（战斗组件改动在事务里）。守卫：根包 `TestExamplesRun`（TOOL）。
4. **控制流**：无。
5. **失败处理**：无。
6. **测试**：修前：`go run ./statusbridge` panic `persistence mutation outside transaction`；修后 `combat`、`fireball`、`statusbridge` 运行退出 0。
7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：示例模块依赖变化时是否在 `skill/examples` 下 `GOWORK=off go mod tidy`。

## NONCORE：非核心 review（N01～N15）的修复与收尾

先读：roost-coding“生命周期与装配的复审要点”（三步停机、A3 共用类型与契约骨架、C7 遍历回调契约，第 63～80 行）与“反复出问题要上报方向判断”（第 105 行），以及 [fix-contract-review](../../agent-skills/roost-coding/references/fix-contract-review.md)。各单元的场景矩阵与“保持现状的观察”在 [单元状态](../../review/REMAINING-REVIEW-HANDOFF-2026-10-05.md)。

本主题的条目大多是“单点缺陷 + 一条先红后绿用例”。review 时建议先看每条的“不变量”与守卫用例是否真的钉住承诺（而不是钉住实现细节），再看第 9 点的检查点。红文本均取自 `docs/bug/` 或 `docs/bugfix/` 记录原文；记录只给证据链接的，这里抄证据文件的首段。

共用复跑方式（`GOWORK=off`）：改哪个包就 `go test -race -count=3 ./<包>`；涉及生成形状的跑 `go test -count=1 ./codegen/...`，再用当前 CLI 生成 game-demo（`roost project new … -template game-demo`，`go mod edit -replace` 到本仓，DAO 库名改成唯一名字）后 build / vet / test；真实依赖用例一律 `-run` 只跑自己的用例，故障注入自建代理或进程，不 reset 共享 toxiproxy，`remote-acceptance.lock` 存在时不跑（A5 规则，见 NONCORE-51）。

<a id="noncore-1"></a>
### NONCORE-1 N01 留项（NC-230～234）与 `ops.admin_timeout`

> 与其他分册重复：以对方条目为准——NC-230 与 `ops.admin_timeout` 见 [OPS-3](impl-app-own-clk-ops-tool.md#ops-3)，NC-231 见 [APP-8](impl-app-own-clk-ops-tool.md#app-8)，NC-232 见 [APP-4](impl-app-own-clk-ops-tool.md#app-4)，NC-233 / 234 见 [APP-7](impl-app-own-clk-ops-tool.md#app-7)；本条只保留编号与索引（汇总去重）。

[说明](guide-cfg-skill-noncore.md#noncore-1)

**提交与版本**：`2c1c7be7`（分支 revn01b）、`897a1dd9`（标注）。首发 v1.21.0。`ops.admin_timeout` 登记进 `frameworkDurationKeys` 见本分册 CFG-2。外部验证项（E08 / E13 / E21 / E22）也在那几条里。

<a id="noncore-2"></a>
### NONCORE-2 HTTP JSON 先编码后写；recover 尊重已开始的响应（NC-80 / 81）

[说明](guide-cfg-skill-noncore.md#noncore-2)

1. **提交与版本**：`c8d72122`（NC-80 / 81 / 83）、`eef7822e`（合并前复核：不复制响应体、Flush 错误透传）、`25bb0d66`（记录）。首发 v1.20.1。
2. **改动**：`httpserver/server.go:222` `JSON` 用 `:235` `statusOnFirstWrite`（状态码推迟到 Encoder 唯一一次 Write）；`:316` `recoverMiddleware`（响应已开始则 `http.ErrAbortHandler`，`:313-315`）。
3. **不变量**：编码失败不发出 2xx；成功输出就是 `json.Encoder` 的原字节；已开始的响应不追加内容。守卫：`httpserver/response_integrity_promises_test.go`（含 `:194` `TestResponseControllerFlushErrorsPassThroughTheTracker`）、`httpserver/response_buffering_promises_test.go:21` `TestJSONDoesNotCopyTheEncodedBodyPerResponse`（`//go:build !race`）、`webroute/result_encoding_promises_test.go`、`kit/ops/response_encoding_promises_test.go`。
4. **控制流**：`JSON` → Encoder 编码进池化缓冲 → 第一次 Write 时 `WriteHeader(status)` → 写出；编码失败 → 500 固定错误体 + 日志。
5. **失败处理**：已写出后才返回的错误只能是连接写失败，与修前一样忽略、不 panic。
6. **测试**：修前（旧产品 `50e9a4e8`）httpserver / webroute / ops 5+1+1 红、控制绿；生成 webroute 消费：修前 `err=<nil> out={WinRate:0}`，修后 `http client: status 500`。复核红：`JSON allocated 289625 bytes per 245760-byte response, want it independent of the body size (< 30720)`；`Flush on a writer without flush support = <nil>, want http.ErrNotSupported`。
7. **性能**：采用方案 0.5KB/op（与修前基线相同）、+1 alloc（32B 适配器）。
8. **未验证**：HTTP/2 路径未单跑（外部 E05）。
9. **review 检查点**：包装 writer 是否只在原 writer 支持时暴露 Hijacker（否则 `http.ResponseController` 会误判能力）。

<a id="noncore-3"></a>
### NONCORE-3 RateLimiter 每主体 key 上限（NC-82）

[说明](guide-cfg-skill-noncore.md#noncore-3)

1. **提交与版本**：`eef7822e`。首发 v1.20.1。
2. **改动**：`security/ratelimit.go:24` `DefaultMaxKeysPerOwner = 256`、`:32` `MaxKeysPerOwner`；`gateway/middleware.go`。
3. **不变量**：一个主体只耗尽自己的名额；满表陌生 key O(1) 拒绝。守卫：`security/owner_capacity_promises_test.go`（`:35` `TestOwnerKeyLimitIsCountedAndReleasedByIdleSweep`）、`gateway/ratelimit_owner_promises_test.go`；原有 `ratelimit_test.go`、`admission_promises_test.go`（NC-05）不改、全绿。
4. **控制流**：`Allow`（`:135`）→ 主体计数 → 超额拒绝计 `OwnerCapacityRejected`；闲置 key 每 SweepInterval 至多清扫一次。
5. **失败处理**：拒绝即限流。
6. **测试**：修前 3 红（N02 提交上），修后 3 绿。
7. **性能**：`BenchmarkRateLimiterFullTableUnseenKey`（`-benchtime 200x -count 3`）满表陌生 key 修前 1k 表约 5.7µs、100k 表约 529µs；修后 89～102ns、56～89ns。
8. **未验证**：无（仓内无装配方，N02 O6）。
9. **review 检查点**：`MaxKeysPerOwner` 被 Clamp 到不超过 `MaxKeys`。

<a id="noncore-4"></a>
### NONCORE-4 生成 TCP 接入停机可重试（NC-83）

[说明](guide-cfg-skill-noncore.md#noncore-4)

1. **提交与版本**：`c8d72122`。首发 v1.20.1。之后 `50f2ac2a`（A3，APP）用 `internal/stopcontract` 骨架套了生成 TCP 的停止入口。
2. **改动**：`codegen/internal/roost/render_player_tcp.go`（模板内 `Server.Stop` / `Mod.StopWithContext`）。
3. **不变量**：三步停机——超时返回错误并保留 server，重试继续等真实排空。守卫：生成用例 `TestAStopRetryWaitsForAConnectionTheFirstStopCouldNotDrain`（`render_player_tcp.go:2203`）、`TestAModStopRetryKeepsTheServerUntilItDrains`（`:2226`）、`TestABlockingCloseSubscriberDoesNotHoldStopPastItsContext`（`:2254`）。
4. **控制流**：关准入 → ctx 内等连接 goroutine → 排空后释放。
5. **失败处理**：超时 → ctx 错误、保留。
6. **测试**：与旧生成器的 `server_gen.go` 组合时 3 红（`retry Stop reported success...`、`server kept = false`、`ignored its context`），新生成物 3 绿。
7. **性能**：无。
8. **未验证**：外部 E05。
9. **review 检查点**：NC-83 记录里列了同形停机候选的处理表（NC-170～174、NC-90、NC-171），确认每个都有结论。

<a id="noncore-5"></a>
### NONCORE-5 生成 TCP“handler 不配合 ctx”用例（A15）

[说明](guide-cfg-skill-noncore.md#noncore-5)

1. **提交与版本**：`fcc78ad0`（收尾第 2 批）。首发 v1.23.0（本版）。
2. **改动**：只加测试：生成用例 `TestADispatchTimeoutBoundsTheWaitNotAnUncooperativeHandler`（`codegen/internal/roost/render_player_tcp.go:1622`），测试辅助 `stuckDispatchServer` 抽出通用的 `dispatchServer`。
3. **不变量**：DispatchTimeout 只界定等待；名额在 handler 返回且连接结束后才归还。
4. **控制流**：无变化。
5. **失败处理**：无。
6. **测试**：当前实现本来如此，用例首次即通过（承诺固定，不是缺陷修复）；生成工程 `-race -count=3` 通过。
7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：用例在客户端断开后 300ms 内断言两项计数仍各为 1——确认它用的是有界等待而不是 sleep 赌时序。

<a id="noncore-6"></a>
### NONCORE-6 Bus JetStream RPC（NC-90 / 91 / 92）

[说明](guide-cfg-skill-noncore.md#noncore-6)

1. **提交与版本**：`e81d81bc`（N03 登记）、`ae742984`（NC-90）、`64179ad5`（NC-91）、`25646001`（NC-92）、`a80be80c`（收口）。首发 v1.20.1。之后 `50f2ac2a`（A3）把 bus JetStream RPC 的手写准入门改用 `internal/operation.Lifetime`。
2. **改动**：`bus/bus.go:317` `Stop`、`:334` `StopWithContext`、`:425` `stopResources`；`bus/jetstream_rpc.go:255` `stopJetStreamRPCRequests`、`:285` `stopJetStreamRPCResponses`（响应消费者推迟到排空之后再停）；`bus/rpc_error.go:19` `ErrRPCCapturedByJetStream`、`:68` 识别 PubAck。
3. **不变量**：停止时先关 JetStream 请求准入、再在 ctx 内等在途 handler；回包不随 Bus 停止取消；没有 reply subject 的 JetStream RPC 请求不执行。守卫：`bus/jetstream_stop_promises_test.go`（`:120` `TestJetStreamStopHandsInterruptedRequestBackToBroker` 为行为保持的控制）；真实 NATS `kit/nats/jetstream_stop_real_promises_test.go:81` `TestRealJetStreamStopWaitsForInFlightHandler`（`-tags integration`）。
4. **控制流**：Stop → 关请求准入 → 等在途 handler（超预算返回 ctx 错误、保留，重试继续等）→ 停响应消费者 → 关连接。
5. **失败处理**：因停止被取消而中断的 handler 不回“已取消”失败包，交还 broker 重投；投递次数已到 MaxDeliver 时被 Term（与修前相同）。
6. **测试**（问题记录原文）：

   ```text
   stop returned err=<nil> after 50.301084ms; handler still running=1
   StopWithContext returned nil while the JetStream handler was still running
   call refused by full queue: err=rpc: revn03q....rpc.rvq.Block failed after 1 attempts: nats: request timeout elapsed=2.00182475s
   dead letters for the RPC request: 1 [{MsgID:m1 ... ToModule: MsgName:mail.Send ... Reason:dispatcher not running ...}]
   lightweight Call: err=bus: unsupported rpc response version 0 resp=map[]; handler runs afterwards=1 handler_had_deadline=false
   ```

   真实 NATS 修前：`budgeted stop with the handler in flight: err=<nil> bus_retained=false asm_retained=false`；NC-92：`captured lightweight requests: handler runs=2 ack_pending=0, want 0 / 0`。
7. **性能**：无。
8. **未验证**：外部 E06 / E07。
9. **review 检查点**：Start 失败的清理（`detachLocked`）同样关掉 JetStream 请求准入——确认失败后重试 Start 的行为没有被本修复改变（记录写“没有改变这一点”）。

<a id="noncore-7"></a>
### NONCORE-7 etcd `Resign` 期限（NC-93）

[说明](guide-cfg-skill-noncore.md#noncore-7)

1. **提交与版本**：`89a102db`。首发 v1.20.1。
2. **改动**：`etcd/driver/election.go:315` `Resign`：lease 撤销由 election 持有、自带 5s 截止。
3. **不变量**：`Resign` 按调用方期限返回。守卫：`etcd/driver/election_resign_budget_promises_test.go:29` / `:85`；真实 etcd `etcd/driver/real_etcd_resign_budget_promises_test.go:55` / `:90`（`-tags integration`）。
4. **控制流**：Resign → 本地领导权结束（`IsLeader=false`、`LeaderChan` 关闭）→ `elect.Resign` → 有界 Revoke。
5. **失败处理**：期限先到返回 ctx 错误；Revoke 失败或超 5s 时 lease 在 TTL 后过期。
6. **测试**：修前 `Resign(500ms) with frozen etcd: err=still blocked after 20s elapsed=20.000663958s is_leader=false`；包内 `Resign with a 50ms budget still blocked after 2s: the lease Revoke ignores the caller's ctx`。
7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：etcd 选举无生产调用方（B5 保留），改动面只在 driver。

<a id="noncore-8"></a>
### NONCORE-8 bus SETNX 去重契约写进注释

[说明](guide-cfg-skill-noncore.md#noncore-8)

1. **提交与版本**：`88f33776`（收尾第 1 批）、`8059b877`（标注）。首发 v1.23.0（本版）。
2. **改动**：`bus/reliable.go` 的 `ReliableStore` 注释。
3～8. 只改注释，无测试。
9. **review 检查点**：对照当前 `BeginConsume` 出错分支确认注释描述的“进死信、不重投；死信重投用新 MsgID”与代码一致。

<a id="noncore-9"></a>
### NONCORE-9 迁移输出 WAL 准入前验证（NC-31）

[说明](guide-cfg-skill-noncore.md#noncore-9)

1. **提交与版本**：`c3aa0edd`。首发 v1.20.0。
2. **改动**：`dataengine/engine/migration_runner.go`（预提交复用 Mongo BSON / ID 与目标 `RestorePersisted`，不修改在线 DAO）。
3. **不变量**：坏的迁移产物不进入 CommitSystem。守卫：正式 runner overlay 与 DAO CLI 消费（证据见 [noncore-bugfix-20261004-11](../../bugfix/evidence/noncore-bugfix-20261004-11/README.md)）。
4. **控制流**：迁移候选 → 预校验（BSON / ID / 目标 schema、旧 version）→ WAL 准入。
5. **失败处理**：预校验失败不提交；缺 loader / Id 的手写候选拒绝。
6. **测试**：修前 5 失败 / 3 对照通过，失败文本 `invalid migration reached CommitSystem: changed=true error=<nil> commits=1`；正式 DAO CLI 消费 3 失败 / 8 对照通过。修后 12 新增正式、原 11 消费和含 6 新场景的 17 消费通过。
7. **性能**：无。
8. **未验证**：真实副本集 / HA / 持续竞争（外部 E11）。生产上已有的坏 WAL 按兼容说明处理（不自动跳过或删除，见说明文档 NONCORE-9），不是待验证项。
9. **review 检查点**：这是 DataEngine（核心线）文件，改动由非核心线发起；确认 glsvet `./dataengine/engine` 通过。

<a id="noncore-10"></a>
### NONCORE-10 生成 DAO 深层嵌套修改进入提交（NC-32）

[说明](guide-cfg-skill-noncore.md#noncore-10)

1. **提交与版本**：`b2232db5`。首发 v1.20.0。
2. **改动**：`codegen/internal/dao/template_nested.go`（wire 转换先恢复未绑定数据，父对象到最终位置后再递归接线）。
3. **不变量**：加载后深层修改经脏通知进入持久提交。守卫：正式金样运行九项（DAO map / slice / pointer 三种父入口 × 子对象三种入口）、独立正式 DAO CLI 消费六项。
4. **控制流**：wire → 恢复 → 父对象定位 → 递归绑定 DirtyHook。
5. **失败处理**：无。
6. **测试**：修前九项 `loaded child level=99 but commit records=0, want 1`；CLI 六项 `persisted descendant changed to 99 but commit records=0, want 1`。修后完整金样 53 个叶子、最终消费者 28 个叶子通过。
7. **性能**：无。
8. **未验证**：无。范围说明：验收的是上面九项入口组合与六项 CLI 消费，不据此宣称所有组合；修复前漏写的历史数据不补回（兼容说明）。
9. **review 检查点**：DirtyHook 是每个结构体自己的函数槽——确认没有其他按值返回后再绑定的路径（迁移后重载、回滚恢复、同步入口共用同一 wire）。

<a id="noncore-11"></a>
### NONCORE-11 versionstore 退避后重读（NC-52）

[说明](guide-cfg-skill-noncore.md#noncore-11)

1. **提交与版本**：`be7bcc18`（NC-50～52 同批）。首发 v1.20.1。之后 `7b73aabc`（OPS）在同一 `Update` 里加了 CAS 计数。
2. **改动**：`versionstore/redis_store.go` `RedisStore.Update`：输掉 CAS 后退避，再 GET 一次取新值重试。
3. **不变量**：重试应用到最新状态；尝试次数与退避策略不变。守卫：`versionstore/retry_freshness_promises_test.go`（fake Redis，确定性，无 sleep）。
4. **控制流**：CAS 失败 → backoff → GET → mutate → CAS。
5. **失败处理**：预算用尽仍 `ErrConflict`。
6. **测试**：修前红：

   ```text
   retry_freshness_promises_test.go:57: Update after 8 attempts (8 backoffs, 9 competitor writes) failed: versionstore: version conflict: nc52:k after 8 attempts
   retry_freshness_promises_test.go:109: mutate saw found=[true true false]; want present then absent (deleted during the backoff)
   ```

   真实隔离 Redis 探针：修前三轮 6 写者 0～2/120、8 写者 3～5/160 次 ErrConflict，修后 0/120、0/160（小样本，只证明方向）。chat 两副本并发 Append + Prune 组合修前持续失败、修后 9/9。
7. **性能**：只在输掉 CAS 后多一次 GET。
8. **未验证**：Redis Cluster 与多进程（外部 E09）。
9. **review 检查点**：退避期间键被删时以版本 1 重建（用例覆盖）。

<a id="noncore-12"></a>
### NONCORE-12 mongotest 数组唯一键与 `$in`（NC-102、RR-20261006-08）

[说明](guide-cfg-skill-noncore.md#noncore-12)

1. **提交与版本**：`81659082`（NC-100～102 同批，NC-100 / 101 属 DRV），首发 v1.20.1；`611d5d72`（收尾第 4 批 A14），首发 v1.23.0（本版）。
2. **改动**：`mongo/mongotest/mongotest.go`（唯一索引路径遇数组 `ErrUnsupported`；`$in` 按 reflect 展开切片 / 数组、元素按底层类型比较，`byte` 元素仍按二进制拒绝）；saga 用例去掉绕行（`saga/coordinator_takeover_review_test.go`、`saga/step_operation_promises_test.go`、`saga/cross_process_real_integration_test.go`）。
3. **不变量**：替身对真实 Mongo 行为要么一致、要么明确拒绝，不猜。守卫：`mongo/mongotest/unique_array_promises_test.go:18`、`mongo/mongotest/in_named_slice_promises_test.go:22`。
4. **控制流**：无。
5. **失败处理**：`ErrUnsupported`。
6. **测试**：NC-102 修前 6 个子用例 FAIL（三组分歧 ended with `<nil>` / `mongo: duplicate key`；建索引、更新、compound 返回 `<nil>`）。RR-20261006-08 修前（子项按 map 顺序，两次运行各报一项）：

   ```text
   in_named_slice_promises_test.go:46: named_string_slice: Find: mongofake: unsupported construct: $in operand []mongotest.namedLabel
   in_named_slice_promises_test.go:47: plain_int64_slice: Find: mongofake: unsupported construct: $in operand []int64
   ```

7. **性能**：无。
8. **未验证**：无。`TestRealMongoCoordinatorLeaseTakeover` 已在 `e6828e4f` 上用隔离环境的真实副本集重跑，三个子用例通过（`GOWORK=off go test -tags integration -count=1 -run '^TestRealMongoCoordinatorLeaseTakeover$' ./saga/`，1.85s）。
9. **review 检查点**：元素按“底层类型”比较与驱动编码一致——具名 int 类型与 int64 是否同值相等。

<a id="noncore-13"></a>
### NONCORE-13 缓存副本与 interest 身份绑定（NC-33 / 34）

[说明](guide-cfg-skill-noncore.md#noncore-13)

1. **提交与版本**：`be4eb0fa`。首发 v1.20.0。
2. **改动**：`cache/mirror.go:85` `replicaStore.ApplyReplica`（反序列化后先核对 key / version 与信封）；`remoteentity/interest.go`（写注册表前核对完整消息身份）。
3. **不变量**：信封与载荷身份不一致的消息不改 Store / 注册表。守卫：`cache/replica_payload_identity_promises_test.go`、`remoteentity/interest_payload_identity_promises_test.go`（八项）。
4. **控制流**：消息 → 解码 → 身份核对 → Set / Upsert。
5. **失败处理**：明确拒绝、Store 不变；含身份的 null 更新在回调前拒绝。
6. **测试**：修前 `refused message changed key 8: {ID:8 Version:3 Data:bad}`、`payload identity mismatch returned success`；interest 修前真实副作用 `generation:11` 和 `exists=false total=0`。
7. **性能**：无。
8. **未验证**：跨主机 broker 与弱网下的丢更新（外部 E03 / E06）。
9. **review 检查点**：无 VersionOf 的发布 / 删除兼容保持（控制用例）。

<a id="noncore-14"></a>
### NONCORE-14 权威快照身份与最终读取承诺（NC-35 / 36、RR-20260913-08 残余）

[说明](guide-cfg-skill-noncore.md#noncore-14)

1. **提交与版本**：`7949da08`。首发 v1.20.0。之后 B2（`f376bba0`）、Mirror 第 1～3 步（`8495c5c4`）重写了 `entity/remote_snapshot.go` 的大部分结构（REM 部分）。
2. **改动**：`entity/remote_snapshot.go`（`e6828e4f` 上相关入口：`:396` `loadAuthoritative`、`:634` `Get`；`02c8a10d` 上为 `:392` / `:629`；`155b9f91`（RR-20261006-11 同批）按源码改写了该文件的注释，行号后移）。
3. **不变量**：权威结果写缓存前绑定完整请求键；返回前按最终 L1 重新检查最低版本。守卫：`entity/snapshot_authoritative_admission_promises_test.go`、`remoteentity/snapshot_repair_admission_promises_test.go`、`entity/snapshot_expiry_authoritative_promises_test.go`。
4. **控制流**：Get → 权威加载 → 键核对 → 缓存准入 → 最终最低版本检查。
5. **失败处理**：异键明确拒绝；版本不足 `ErrRemoteSnapshotStale`。
6. **测试**：修前 `foreign authority accepted: found=true version=2 err=<nil> foreignL2=true/4`、`failed authoritative repair returned success to publisher`、`minimum version violated after admission: found=true version=2 err=<nil>`。
7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：B2 之后这些守卫用例是否仍在跑、仍有意义（它们在发版提交上存在）。

<a id="noncore-15"></a>
### NONCORE-15 L2 CAS 落败、版本化删除墓碑、Stats 清理过期兴趣（NC-130 / 131、RR-20260913-01 残余）

> 其他分册对应：REM-1 的背景提到 NC-130 / 131，未单列；本条为主。

[说明](guide-cfg-skill-noncore.md#noncore-15)

1. **提交与版本**：`be2713ad`（revn05 登记）、`6f06f0da`（NC-130）、`c3475150`（NC-131）、`366058a7`（墓碑）、`f6245613`（收口）。首发 v1.20.2。
2. **改动**：`remoteentity/snapshot_l2.go:263` `Set`（CAS 落败返回 `cache.ErrStaleWrite`）、`:324` `DeleteAtVersion`（写墓碑）；`entity/remote_snapshot.go`（`Publish` 不把旧快照装进 L1）；`remoteentity/assembly.go:27` `Manager.Stats`（表满先清理过期）。
3. **不变量**：L1 不低于 L2；已删除实体不被写回 L2。守卫：`remoteentity/snapshot_l2_cas_promises_test.go`、`remoteentity/snapshot_l2_tombstone_promises_test.go`、`entity/snapshot_l2_conflict_promises_test.go`、真实 Redis 用例。
4. **控制流**：见 REM 部分 B2（同版本把 L2 定为水位权威）。
5. **失败处理**：旧写返回 `ErrStaleWrite`。
6. **测试**：修前：

   ```text
   consistency 2 read version=1 route=1 payload="v1" after a late replica; L2 holds version=2 route=1 — L1 was pinned below the shared layer
   monotonic read returned version=1 payload="v1"; L2 already held version 2
   older write = <nil>, want cache.ErrStaleWrite (cache.Store contract)
   Stats().LocalInterests = 4 after every interest expired, want 0
   health after every interest expired = fail (capacity exhausted wrappers=0 local_interests=4 transactions=0 active_transactions=0), want ok
   ```

   真实 Redis：`older write on real Redis = <nil>, want cache.ErrStaleWrite`。
7. **性能**：无（B2 的性能数据见 REM）。
8. **未验证**：复制丢写与切主、多主机强杀（外部 E10 / E13）。滚动升级期间旧节点（< v1.20.2）不认墓碑、仍可能写回旧数据，是 [RR-20260913-01 记录](../../bugfix/RR-20260913-01.md)“兼容”里写明的升级说明，不是待验证项。
9. **review 检查点**：墓碑与 v1.23.0 的 O-M6-3 `WAIT` 副本确认（REM）是同一次写：看 `remoteentity/snapshot_l2.go` 的 `tombstoneWait`（`:113`）只挂在 `DeleteAtVersion` 写墓碑之后。

<a id="noncore-16"></a>
### NONCORE-16 saga 三消费者健康与 Resume 持久代际（NC-37 / 38）

[说明](guide-cfg-skill-noncore.md#noncore-16)

1. **提交与版本**：`47fca740`。首发 v1.20.1。
2. **改动**：`saga/assembly.go:249` `ConsumersClosed`（纳入原生 Nest 完成消费者）；`saga/mongo_store.go:431` `Incarnation uint32 bson:"incarnation,omitempty"`（`saga/record.go:226`）。
3. **不变量**：任一必需订阅缺失 / 退出健康项 fail；重载后派发不复用旧命令 / 回执 ID。守卫：`saga/assembly_health_promises_test.go`、`kit/saga/health_promises_test.go`、`saga/mongo_resume_incarnation_promises_test.go`。
4. **控制流**：Resume → 代际 +1 → 持久 → 新 Store / Engine 重载 → 派发用新代际的 ID。
5. **失败处理**：无。
6. **测试**：NC-37 修前 10 叶子 4 红 / 6 控制；NC-38 修前 `persisted generation=0, want 1`、`redispatch reused old command ID "resume:1:0:1" at generation 1`（补偿为 `resume:2:0:1`）。
7. **性能**：无。
8. **未验证**：跨主机副本集与切主（外部 E11）。单机真实副本集已验：本次重核在 `e6828e4f` 上把 `saga/mongo_resume_incarnation_promises_test.go` 的两个用例临时换成隔离环境的真实副本集（库名唯一、用后删除，探针未入库）跑过：Resume 三代持久与重派发（forward / compensate）、`incarnation` 在 0 / 1 / 17 / 2³²−1 下经 Get / GetByBusinessKey / List / Apply 不丢，全部通过。
9. **review 检查点**：uint32 最大值只验 BSON 往返，不验 Resume 溢出。

<a id="noncore-17"></a>
### NONCORE-17 saga 启动身份与完成路由（NC-39 / 40）

[说明](guide-cfg-skill-noncore.md#noncore-17)

1. **提交与版本**：`12726715`。首发 v1.20.1。
2. **改动**：`saga/record.go:216` `StartDigest`、`saga/mongo_store.go:424` `start_digest,omitempty`；`saga/engine.go`、`saga/nest_completion_consumer.go`（解码后精确匹配 Topic 与 payload SagaID）、`saga/store.go`；`servicemetrics/recorder.go`、`servicemetrics/servicemetrics.go`。
3. **不变量**：启动身份只由原始意图决定；异键完成在副作用前 Permanent 拒绝。守卫：两后端 × 推进 / 完成 / Resume 换期限 / 清期限 8 叶子 + 8 兼容控制（证据 [noncore-bugfix-20261005-16](../../bugfix/evidence/noncore-bugfix-20261005-16/README.md)）；完成路由三反例经公开 `SubscribeNestCompletions`。
4. **控制流**：Start 重投 → 比较 StartDigest → 同意图返回当前进度、异意图冲突。
5. **失败处理**：异键 Permanent 拒绝（复用既有 NATS settle）。
6. **测试**：修前 `original start redelivery rejected after progress: saga: idempotency identity conflict`、`runtime state accepted as a new start intent after progress: <nil>`；`foreign route="saga.result.other" was not refused permanently: <nil>`、`foreign route mutated saga: version=2 status=pending`、`foreign route recorded completion: recorded=true err=<nil>`。
7. **性能**：无。
8. **未验证**：无（发布鉴权不在本条范围）。
9. **review 检查点**：旧的已推进、缺摘要记录重投收紧为冲突——确认运维知道这类重投会被拒（不自动迁移）。

<a id="noncore-18"></a>
### NONCORE-18 saga outbox 领取与 activity 恢复校验（NC-41 / 42）

[说明](guide-cfg-skill-noncore.md#noncore-18)

1. **提交与版本**：`10c73e0c`。首发 v1.20.1。
2. **改动**：`saga/mongo_store.go:352` `ClaimOutbox`（原子领取复查 `next_attempt_at` 与 lease）、`saga/store.go`；`kit/service/global/activity/service.go`（公开 Open 与 sweep 的计划校验）。
3. **不变量**：陈旧候选不绕过 Nack 退避；非法计划不进 Create。守卫：`saga/outbox_backoff_promises_test.go`（两个独立 Store 共用 mongotest，钩子在候选读取后执行另一发布者的领取 / Nack，无 sleep）；activity 用例（证据 [noncore-bugfix-20261005-17](../../bugfix/evidence/noncore-bugfix-20261005-17/README.md)）。
4. **控制流**：Find 候选 → 原子 Claim（条件含退避与 lease）→ 新 token 隔离旧 Ack / Nack。
5. **失败处理**：非法计划 `ErrConflict`（可 `errors.Is`）。
6. **测试**：修前 `stale scan claimed unavailable retry (future_nack): next=2026-10-05 07:55:06.781 +0000 UTC now=2026-10-05 06:55:06.781 +0000 UTC`；`malformed foreign_key plan was accepted: <nil>`。
7. **性能**：无。
8. **未验证**：Mongo 网络丢回复与提交结果未知（外部 E11）。多进程：本次重核在 `e6828e4f` 上跑了 `TestRealSagaCrossProcessKillRecovers`（真实 JetStream + Mongo 副本集，两个协调器 + Mongo 步骤进程，中途 SIGKILL 一个）：60 个 saga 全部完成、120 个操作各恰好提交一次、outbox 排空、无残留租约（76s）。
9. **review 检查点**：NC-250（NONCORE-23）补修时用“去掉 `applyFilter` 的租约条件”做过负对照，确认该条件仍在。

<a id="noncore-19"></a>
### NONCORE-19 account 换名释放死计划、activity 确认键校验（NC-50 / 51）

[说明](guide-cfg-skill-noncore.md#noncore-19)

1. **提交与版本**：`be7bcc18`。首发 v1.20.1。B9（`bd6df5e5`）复核补修见 NONCORE-21。
2. **改动**：`kit/service/account/create_role.go`（换名也释放；补偿失败计 `rollback.failed`）；`kit/service/global/activity/service.go:988` `windowKeyProblem`（Opening 与 sweep 共用）。
3. **不变量**：建角补偿的计数如实反映发生了什么；sweep 只结算本组的合法键。守卫：`kit/service/account/pending_creation_other_name_promises_test.go:237` 等；`kit/service/global/activity/confirmed_key_ownership_promises_test.go`。
4. **控制流**：换名请求 → 名字已被他人提交 → 释放死计划 → 按新名建角。
5. **失败处理**：坏条目跳过、保留、计 `sweep.window_key_malformed`。
6. **测试**：修前：

   ```text
   pending_creation_other_name_promises_test.go:287: a failed compensation counted 0 times, want 1; accepted:create_role=1, accepted:login=2, refused:create_role:name_taken=1
   confirmed_key_ownership_promises_test.go:59: group-a's sweep completed [{Key:group-a/local/close ...} {Key:group-b/foreign/close ... Status:complete ... CompletionReason:grace_expired ...}], want only its own group-a/local/close
   ```

7. **性能**：无。
8. **未验证**：Redis Cluster 与多进程（外部 E09）。
9. **review 检查点**：见 NONCORE-21。

<a id="noncore-20"></a>
### NONCORE-20 activity.game_sids 启动校验（RR-20261005-01，已被 C4 取代；回归在组文件形态下兑现）

[说明](guide-cfg-skill-noncore.md#noncore-20)

1. **提交与版本**：`54e8bea3`（登记）、`46c4dfba`（修复）。首发 v1.20.1；v1.20.2 `277e1252`（C4）删除 `activity.game_sids`。回归去向复核 `d5682dc4`（分支 `fixr2`，新增守卫，首发 v1.23.0 本版）。
2. **改动**：当时 `demo/internal/service/game/activity.go.tmpl`、`codegen/internal/roost/demo.go`、`kit/service/global/service.go`（注释更正不存在的 `Rebind`）。`e6828e4f` 上这条承诺由活动组文件兑现：`kit/service/global/activity/groups.go:80` `ParseGroups`（`:58` `LoadGroupsFile` 调它）加载时拒绝组内重复、非正数 / 超出 int32、一组超过 `MaxExpectedGames`（`kit/service/global/activity/types.go:223`，64）；game-demo `startActivity` 在查协调器能力之前按 `activity.groups_file` 点名拒绝（OWN-5）。
3. **不变量**：注定开不出窗口的候选集在启动时、任何远端调用之前按配置键名失败。C4 之后候选集 = 本服在 `configs/activity_groups.yaml` 里所在组的成员。
4. **控制流**：game Init → `startActivity` → `activityGroup`（`ParseGroups` 校验）→ 才查协调器能力。
5. **失败处理**：`Service.Init` 失败，错误点名 `activity.groups_file`。
6. **测试**：
   - 修前（v1.20.1，模板只加测试，原用例 `TestActivityRefusesACandidateListNoWindowCouldOpenWith`）：

     ```text
     activity_test.go:236: startActivity with activity.game_sids=[1302 1301 1302]: error activity: game: capability "service.global.activity" not found; is the activity process running and reachable over the bus? does not refuse the list by name; every window it would try to open would fail
     --- FAIL: .../a-repeated-sid (0.00s)
     --- FAIL: .../more-candidates-than-one-live-query (0.00s)
     --- FAIL: .../a-sid-beyond-int32 (0.00s)
     ```

   - **回归改名对照**（C4 `277e1252` 删除 `candidateSIDs` 时原用例一并删除；`d5682dc4` 复核结论：承诺仍需要，回归没有丢、改名改形，见 [修复记录](../../bugfix/RR-20261005-01.md)末节）：

     | 原子用例（`277e1252^` 的 `activity_test.go.tmpl`） | `e6828e4f` 上的覆盖 |
     | --- | --- |
     | `a-repeated-sid` | 模板 `TestActivityRefusesAGroupNoWindowCouldOpenWith/a-repeated-sid`（`demo/internal/service/game/activity_test.go.tmpl:235`）；kit `TestAGroupsFileThatCannotBeUsedIsRefusedByName/repeated-sid`（`kit/service/global/activity/groups_promises_test.go:79`），另有 `a-sid-in-two-groups` / `sid-in-two-groups` |
     | `a-sid-beyond-int32` | 模板 `…/a-sid-beyond-int32`；kit `…/beyond-int32`、`…/non-positive` |
     | `more-candidates-than-one-live-query`（> `app.SingletonLiveMaxSIDs` = 200） | 一组至多 64 个成员、加载时拒绝：模板 `…/more-than-the-coordinator-takes`；kit `TestAGroupLargerThanOneWindowIsRefusedWhenLoaded`（`groups_promises_test.go:44`）；**新增**守卫 `TestAGroupFitsOneLiveQuery`（`kit/service/global/activity/groups_live_limit_promises_test.go:15`，`MaxExpectedGames` ≤ `app.SingletonLiveMaxSIDs`，两个常量改一个不改另一个时先红） |
     | `own-sid-and-non-positive-entries-are-skipped` | 不再适用：组文件里本服必须是成员、非正数从“跳过”收紧为“拒绝”——模板 `…/this-server-in-no-group`、`…/no-groups-file`；kit `…/non-positive` |
     | `exactly-one-live-query-is-accepted` | 恰好 64 个成员的组接受：模板 `…/a-full-group-is-accepted`、`TestAWindowOpensForTheGroupTheFilePutsThisServerIn/a-full-group-all-live`；kit `TestAFullGroupOpensAWindowWithTheCoordinator` |

   - 能红（`d5682dc4` 记录，临时改源码未提交）：去掉 `ParseGroups` 的重复 / int32 / 组大小检查各自变红；把 `MaxExpectedGames` 改成 201，`TestAGroupFitsOneLiveQuery` 红在 `an activity group may hold 201 game servers (MaxExpectedGames) but one App.Live query takes at most 200 …`；生成 game-demo 里把 `activityGroup` 挪到能力查找之后，模板用例红在与修前同一形状（`capability "service.global.activity" not found … does not refuse the group by name`）。
   - 复跑：`GOWORK=off go test -count=1 -run 'TestAGroup|TestAFullGroup' ./kit/service/global/activity/`；生成 game-demo 后 `go test -count=1 -run TestActivityRefusesAGroupNoWindowCouldOpenWith ./internal/service/game/`。
7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：`ParseGroups` 是否仍是组文件的唯一校验入口（协调器与 game 读同一文件、同一规则）；`TestAGroupFitsOneLiveQuery` 比较的是 `MaxExpectedGames` 与 `app.SingletonLiveMaxSIDs` 两个常量本身，而不是各写一份字面量。

<a id="noncore-21"></a>
### NONCORE-21 B9：activity 窗口条目统一入口与修复入口；account 建角判定表

[说明](guide-cfg-skill-noncore.md#noncore-21)

1. **提交与版本**：`2954c583`（第四轮决定）、`bd6df5e5`（实施，分支 `b9svc`；同提交含 C5 Live 契约，属 APP）、`57c0b3b6`（标注）。首发 v1.21.0。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `kit/service/global/activity/window_entries.go:59` `readWindowEntries` | 窗口条目的唯一读入口；坏条目进 malformed，按列表计数（`:119` `sweep.window_key_malformed`、`:129` `sweep.delivering_key_malformed`） |
   | `kit/service/global/activity/admin.go:55` `MalformedWindowEntries`、`RemoveMalformedWindowEntry` | 持有方专用修复入口，note 必填，只删确实坏的条目 |
   | `kit/service/global/activity/types.go:828` 起 | 窗口记录 `admin_note` / `admin_action_at_unix`（omitempty） |
   | `kit/service/account/creation_table.go:116` `decideCreation` | 名额 6 × 入口 4 × 名字 6 的判定表 |
   | `kit/service/account/create_role.go:269-293` | `releaseCreationSlot` 返回 `(released, err)`；`plan_released` 只在本次真的删掉死计划时计 |

3. **不变量**：任何列表都不交出坏键；一个死计划只计一次释放。守卫：`kit/service/global/activity/window_entries_promises_test.go:53` `TestDeliveringSkipsMalformedEntriesAndKeepsThem`、`TestPendingActivitiesSkipsMalformedEntries`、`TestRetireDeliveredReportsOnlyWhatItRemoved`、`:157` `TestOperatorRemovesOnlyMalformedWindowEntries`；`kit/service/account/plan_release_count_promises_test.go:38` `TestConcurrentReleasesOfOneDeadPlanCountOnce`；144 格判定表的表格测试。
4. **控制流**：`PendingActivities` / `DeliveringActivities` / `RetireDelivered` / sweep → `readWindowEntries` → 合法条目处理、坏条目计数保留。
5. **失败处理**：版本不符的 `DeleteIf` 不是错误、不计 `rollback.failed`，也不算“本次释放”。
6. **测试**：修前（Memory）：

   ```text
   window_entries_promises_test.go:76: group-a's delivering list = [group-a//close group-a/local/close group-b/foreign/close], want only its own group-a/local/close
   window_entries_promises_test.go:93: group-a's sweep wrote group-b's window: before {... Delivering:[group-b/foreign/close] ...} Version:3} after {... Delivering:[] ...} Version:4}
   window_entries_promises_test.go:145: retiring a key that is no longer listed = true, <nil>; want false
   plan_release_count_promises_test.go:80: one dead plan released once was counted 2 times; accepted:create_role=2, accepted:login=2, dropped:create_role.plan_released=2, refused:create_role:role_limit=1
   ```

   修复入口是新 API，没有修前红。`go test -race -count=3 ./kit/service/global/activity/ ./kit/service/account/`。
7. **性能**：无。
8. **未验证**：Redis Cluster 与多进程（外部 E09）。
9. **review 检查点**：activity 包里读已存窗口条目（Keys / Delivering 列表）的地方除 `window_entries.go` 外不应再直接遍历原始列表；`RemoveMalformedWindowEntry` 对健康条目拒绝（用例覆盖四种拒绝）。

<a id="noncore-22"></a>
### NONCORE-22 account 换名释放未 admitted 的计划（第五轮、O37）

[说明](guide-cfg-skill-noncore.md#noncore-22)

1. **提交与版本**：`0aeb5ab6`（第五轮决定）、`b18d5613`（res-else 格，分支 `revn09f`）、`4da5e7ea`（第七轮决定）、`f6043e44`（O37 free 格，与 SKILL-16 同提交）。首发 v1.21.0。
2. **改动**：`kit/service/account/creation_table.go`（`unadmitted other` 行的 res-else 格与 free 格 limit → retry）、`kit/service/account/create_role.go`。
3. **不变量**：已 admitted 的计划仍 `ErrRoleLimit`。守卫：判定表表格测试与 B9 方案 §6 / §6.1 的用例。
4. **控制流**：`decideCreation(slot=unadmitted, entry=other-name, name=reserved-by-other|free)` → 释放旧 slot → 按新名建角。
5. **失败处理**：释放失败计 `rollback.failed`。
6. **测试**：见 [B9 方案 §6](../../feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md)；`go test -race -count=3 ./kit/service/account`。
7～8. 无。
9. **review 检查点**：表里只改了两格；`rg 'limit' kit/service/account/creation_table.go` 对照方案的格子表逐格核对。

<a id="noncore-23"></a>
### NONCORE-23 saga 定义缺失 fence 时退避中的步骤记为放弃（NC-250）

> 与其他分册重复：以 [SAGA-7](impl-saga-drv-dao-rem.md#saga-7) 为准，本条只保留编号与索引（汇总去重）。

[说明](guide-cfg-skill-noncore.md#noncore-23)

**提交与版本**：`31b48bc0`。首发 v1.21.0。之后 `a95cf4dc`（saga 方向 ①）把各出口收成 `stepTransition`，同样见 SAGA 部分。

<a id="noncore-24"></a>
### NONCORE-24 global `Bind` 同参重试幂等（RR-20261006-05）

> 与其他分册重复：以 [OWN-6](impl-app-own-clk-ops-tool.md#own-6) 为准，本条只保留编号与索引（汇总去重）。

[说明](guide-cfg-skill-noncore.md#noncore-24)

**提交与版本**：`611d5d72`（收尾第 4 批 A7）、`53fd9e9c`（标注）；真实 Redis 用例在 `ba13cb05`。首发 v1.23.0（本版）。

<a id="noncore-25"></a>
### NONCORE-25 N07 第一批（NC-60～63）

[说明](guide-cfg-skill-noncore.md#noncore-25)

1. **提交与版本**：`3d4fe9f3`。首发 v1.20.1。NC-61 的组件 undo 在 v1.20.2 被 `5407f127`（A1）删除。
2. **改动**：`attribute/container.go:108` `Snapshot`（持读锁复制）；`codegen/internal/attribute/parse.go`（拒绝不可表示的声明）；`codegen/internal/errcode/main.go`（按导入名识别 `Define`、非字面量报 `file:line`、查重复 name）。
3. **不变量**：快照不撕裂；生成期拒绝 wire 不能承载的属性；errcode 不静默跳过。守卫：`attribute/container_snapshot_promises_test.go:32`、`codegen/internal/attribute/representable_promises_test.go:15`、`codegen/internal/errcode/scan_promises_test.go:14`。
4. **控制流**：无。
5. **失败处理**：生成期报错。
6. **测试**：修前：

   ```text
   container_snapshot_promises_test.go:59: Apply entered LoadValues while Snapshot was still copying the same profile: the copy is not taken under the container lock
   attribute_component_test.go:238: rolled back to level 0 but the base layer kept the level-up: HP 110 attack 12, want HP 100 attack 10
   rate=0.15 GetAttr=0 export=map[1:0] reload=0
   scan accepted it and exported 0 definitions        (常量编号 / 常量名字 / 别名导入+常量编号)
   ```

7. **性能**：无。
8. **未验证**：无。
9. **review 检查点**：`CloneProfile` 不得回调同一容器（持读锁调用，回调会死锁）——确认文档 / 注释写明。

<a id="noncore-26"></a>
### NONCORE-26 N07 第二批（NC-64 / 65）

[说明](guide-cfg-skill-noncore.md#noncore-26)

1. **提交与版本**：`197f7bb9`。首发 v1.20.1。
2. **改动**：`demo/game/flags/flags.go.tmpl`、`demo/internal/service/game/flags.go.tmpl`、`demo/internal/service/game/gm.go.tmpl`（说明）；`codegen/internal/roost/demo.go`；玩家属性组件模板（加载时按 `EquipmentComp` 求 Gear、写 `attr_final`）。
3. **不变量**：加载后的 Final 与穿戴一致；`attr_final` 是 nopersist、只标同步脏。守卫：生成工程 `attribute_component_test.go`（模板），e2e reload 证据。
4～5. 无。
6. **测试**：修前：

   ```text
   {"name":"gm.config.reload","ok":true,"data":{"flags":2,"reason":"s0-csv-only","version":2},...}
   flags version 2 {'activity': True, 'monster_spawn': True, 'purchase': True}
   attribute_component_test.go:276: a new player's attr_final is empty: the composed view was never handed to replication
   attribute_component_test.go:320: after load final = attack 10 HP 100 power 30, want what the worn set gives: attack 25 HP 105 power 60
   ```

7～8. 无。
9. **review 检查点**：A1 之后 Gear 层在 DAO（`attr_gear`），确认加载重建与 A1 的 derive 是同一入口（DAO 部分）。

<a id="noncore-27"></a>
### NONCORE-27 生成器不改进程工作目录（RR-20261004-12）

[说明](guide-cfg-skill-noncore.md#noncore-27)

1. **提交与版本**：`a646193c`。首发 v1.20.0。
2. **改动**：`codegen/internal/roost/generate.go`（生成器拿工程根下的绝对路径）、`codegen/internal/servicerpc/run.go`。
3. **不变量**：生成期间进程工作目录不变。守卫：`codegen/internal/roost/generator_cwd_promises_test.go:25` / `:65`。
4. **控制流**：无。
5. **失败处理**：无。
6. **测试**：修前：

   ```text
   generator_cwd_promises_test.go:53: the process working directory was the generated tree while generators ran: /private/var/folders/xs/…/TestGeneratorsDoNotMoveTheProcessIntoTheTreeTheyGenerate3562826371/001
   generator_cwd_promises_test.go:105: 9 children started without Dir ran inside a staging tree, e.g. /private/var/folders/xs/…/TestSyncProjectStagesNeverBecomeAChildsWorkingDirectory1926229470/001/.roost-sync-4089255759/go.mod
   ```

   生成物不变：修前 / 修后两个 roost 二进制生成三个工程 `diff -r` 完全相同。
7. **性能**：无。
8. **未验证**：Windows 上的清理失败是否全由此引起——Windows 暂存，不保证正确（外部 E25）。
9. **review 检查点**：`rg 'os.Chdir' codegen` 应无结果。

<a id="noncore-28"></a>
### NONCORE-28 go 命令按进程树取消（RR-20261004-13 与补修）

[说明](guide-cfg-skill-noncore.md#noncore-28)

1. **提交与版本**：`7ffa7199`（首发 v1.20.0）、`cb11be90`（补修，首发 v1.20.1）。v1.21.0 起 `f556049c`（B6）删掉了 `runCommandTree` 里的信号接管与重发。
2. **改动**：`codegen/internal/roost/command_tree.go:32` `runCommandTree`（发版提交上只剩 `exec.CommandContext` + 进程组 + `Cancel` 杀树 + `WaitDelay`，157 行 → 42 行）、`command_tree_unix.go` / `command_tree_windows.go`（`taskkill /T`）/ `command_tree_other.go`；`dependencies.go`、`doctor.go`。
3. **不变量**：超时 / 取消后不留孙进程，Wait 有界。守卫：`codegen/internal/roost/go_command_tree_promises_test.go:220` 等、`:345` `TestDependencyCommandInterruptKillsTheGoTreeAndStillKillsRoost`。
4. **控制流**：ctx 结束 → 杀进程组 → 最多再等 5s。
5. **失败处理**：返回超时 / 取消错误。
6. **测试**：修前：

   ```text
   go_command_tree_promises_test.go:153: runDoctorGoCommand was still blocked 10s after its context ended: Wait is held by grandchild 1252's copy of the output pipe
   go_command_tree_promises_test.go:176: runDependencyCommand (buffered output) was still blocked 10s after its context ended: Wait is held by grandchild 1370's copy of the output pipe
   ```

   负对照：去掉信号接管只留进程组，`TestDependencyCommandInterruptKillsTheGoTreeAndStillKillsRoost` 红——`interrupted roost returned but grandchild 4006 is still alive 2s later`。另做过真 go 探针：修前确认 `kill -9` go 会留下 compile 孙进程，修后不留（修前修后的承诺由上面两条单测的红绿给出）。
7. **性能**：无。
8. **未验证**：Windows（`taskkill /T` 分支只 `GOOS=windows go vet`）——Windows 暂存，不保证正确（外部 E25）。
9. **review 检查点**：`GOOS=windows go vet ./codegen/internal/roost/` 通过（taskkill 分支）。

<a id="noncore-29"></a>
### NONCORE-29 N08：NC-70～73

[说明](guide-cfg-skill-noncore.md#noncore-29)

1. **提交与版本**：`a7aa62fc` / `75ab37e7`（登记）、`80802200`（修复）。首发 v1.20.1。NC-70 的登记表在 v1.21.0 被 B6 删除（行为由“正常回滚后再重抛”保证）。
2. **改动**：`codegen/internal/roost/command_tree.go`、`dependencies.go`、`generate.go`（NC-70）；`render_docs.go` / `help.go`（NC-71 / 72 文档）、`diffManifest`（NC-71）；`codegen/internal/roost/id.go`、`codegen/internal/errcode/main.go`（NC-73 `ScanDefinitions`）。
3. **不变量与守卫**：`codegen/internal/roost/interrupt_stage_promises_test.go:39`（NC-70，`//go:build unix`）、`diff_preview_promises_test.go`（NC-71）、`cfggen_help_promises_test.go:30`（NC-72）、`id_errcode_scan_promises_test.go:20`（NC-73）。
4～5. 见说明。
6. **测试**：修前：

   ```text
   interrupt_stage_promises_test.go:134: interrupted roost deps left its staging tree .roost-deps-2619724946 beside the project
   diff_preview_promises_test.go:93: project diff previewed 11 files, the sync rewrote 14
        missing from the preview: [configs/service/config.game.prod.example.yaml configs/service/config.game.yaml deploy/k8s/base/secret.game.example.yaml]
   cfggen_help_promises_test.go:54: roost generate after following roost help cfggen (-out ./configs/generated): generator registry: registry: example.com/planet/configs/generated.RegisterConfigData is marked twice (…:40 and …:34)
   roost id next errcode handed out 100000 again although ec.Define(100000, …) (aliased import) already uses it
   ```

7～8. 无。
9. **review 检查点**：NC-73 之后 `_test.go` 里的定义不再计入 ID 工具（与生成器一致）。

<a id="noncore-30"></a>
### NONCORE-30 运行期目录不算生成输入（NC-74）

[说明](guide-cfg-skill-noncore.md#noncore-30)

1. **提交与版本**：`6c278081`（登记）、`45d4bc1c`（NC-74 / 75）。首发 v1.20.1。
2. **改动**：`codegen/internal/roost/generate.go:530` `skippedProjectDirectory`（`.dev`、`data/wal`）、`project.go`。
3. **不变量**：复制、输入快照与提交计划共用同一条工程边界。守卫：`codegen/internal/roost/runtime_dirs_promises_test.go:75` `TestGenerateAndSyncIgnoreTheProjectsRuntimeOutput`（控制：生成期间改应用自有配置仍被拒绝）。
4～5. 无。
6. **测试**：修前 generate / sync 两条报 `project inputs changed …: .dev/game.log, data/wal/dataengine/000001.wal`；修后三条通过；负对照（只还原 `project.go` 的遍历）红在 `.dev/quoted.log did not survive`。
7～8. 无。
9. **review 检查点**：生成的 `.gitignore` 也要忽略同样的目录（NC-206，NONCORE-51）。

<a id="noncore-31"></a>
### NONCORE-31 B6 CLI 信号统一接管；C9 联网用例门

[说明](guide-cfg-skill-noncore.md#noncore-31)

1. **提交与版本**：`2954c583`（第四轮决定）、`f556049c`（实施，基线 `4f0bab75`）、`c8ecbabb`（标注）。首发 v1.21.0。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `codegen/internal/roost/interrupt.go:38` `Main`、`:48` `runInterruptible`、`:118` `dieOf` | 入口接管信号：第一次取消 ctx，第二次直接 `dieOf`；命令返回后打印 `roost: interrupted by <sig>: <err>` 并以同一信号结束（`signal.Reset` → 向自己发信号 → 等 `reraisedSignalGrace` → 兜底 `os.Exit(128+n)`） |
   | `codegen/internal/roost/command_tree.go:32` | 只剩进程组 + 杀树 + `WaitDelay`；NC-70 登记表删除 |
   | `copyProject` / `runGenerators` / `runCommandTree` / 提交点前 | 取消检查点；提交与提交之后的模板脚手架不查 ctx |
   | `codegen/cmd/roost/main.go`、`codegen/cmd/project/main.go` | 改调 `roost.Main`；`Run` 保留为无取消包装 |
   | `.github/workflows/framework-compat.yml` job `codegen-network` | `ROOST_NETWORK_TESTS=1`，`-run` 点名两条，出现 `--- SKIP` 即失败 |
   | 根包 `ci_generated_code_test.go:184` `TestNetworkCodegenTestsRunInSomeWorkflow` | 钉住门变量名、两条用例存在、某个无条件步骤设变量并点名跑它们 |

3. **不变量**：中断不留半份提交、不留暂存树、退出码与以前相同。守卫：`TestInterruptedCommandRemovesItsStagingTree`、`TestDependencyCommandInterruptKillsTheGoTreeAndStillKillsRoost`（经新入口，断言不变）、O6 三个阶段用例。
4. **控制流**：信号 → cancel → 命令在检查点返回 → 回滚 / 删暂存树 → 打印 → 重抛信号。
5. **失败处理**：go 命令被取消时错误为 `go <args> interrupted: context canceled`（`errors.Is(err, context.Canceled)`）；doctor 返回 `doctor interrupted, no report`。
6. **测试**：先红：O6 三个阶段用例在旧机制上红在“暂存树残留”；C9 负对照：把变量改成 `"0"` 即红。Unix 信号用例 macOS 上 `-race -count=3`；`GOOS=windows go vet`。
7. **性能**：无。
8. **未验证**：Windows 不接管信号（只 vet）——Windows 暂存，不保证正确（外部 E25）；Linux 上的信号与进程树（外部 E26）。GitHub `codegen-network` job 在 `e6828e4f` 上通过（framework-compat run `37461843085`）。
9. **review 检查点**：`reraisedSignalGrace` 只剩入口一处（`rg reraisedSignalGrace codegen`）；提交点之后的步骤确实不查 ctx（否则会留半份提交）。

<a id="noncore-32"></a>
### NONCORE-32 DAO `//roost:dao nocoll`

[说明](guide-cfg-skill-noncore.md#noncore-32)

1. **提交与版本**：`822576b9`（方案）、`2a82d826`（生成器）、`b680ea26`（game-demo MonsterDao）、`d3f28337` / `c776c814`（记录）。首发 v1.20.0。
2. **改动**：`codegen/internal/dao/gen.go`、`parse.go`、`template_dao.go`；`codegen/internal/entity/nocoll_dao.go`、`gen.go`、`main.go`、`parse.go`、`remote_dao_scope.go`；`codegen/internal/roost/help.go`。
3. **不变量**：nocoll DAO 没有任何 Mongo 路径；以 `<Dao>RegistryKey` 在 DaoManager 登记。守卫：dao / entity 生成器的拒绝用例与金样（见方案 §7）。
4～5. 生成期点名报错（见说明的五种组合）。
6. **测试**：见 [方案 §7 实施结果](../../feature/DAO-NO-COLLECTION-2026-10-04.md)。
7～8. 无。
9. **review 检查点**：nocoll DAO 的生成物里没有集合名与 Mongo 读写路径：看金样 `codegen/internal/dao/testdata/golden/gen_wraith_dao.go` 与运行期用例 `codegen/internal/dao/testdata/runtime/nocoll_test.go`。

<a id="noncore-33"></a>
### NONCORE-33 文件 outbox 清理遗留临时文件（RR-20261006-04）

[说明](guide-cfg-skill-noncore.md#noncore-33)

1. **提交与版本**：`b8fbcee0`（收尾第 3 批 A6，与 SKILL-20 / 21 同提交）。首发 v1.23.0（本版）。
2. **改动**：`skill/skillsync/file_outbox.go:28` `fileOutboxTemporaryPattern`（`PutRecord` 的 `os.CreateTemp` 与判别共用，`:219`）、`:60` `NewFileOutboxStoreWithOptions` 扫描目录时 `:97` 删除、`:289` `isFileOutboxTemporary`（`DirEntry.Type().IsRegular()`，名字恰为 `outbox-` + 非空十进制数 + `.tmp`）；有删除时目录 fsync。
3. **不变量**：只删本 outbox 生成的临时文件。守卫：`skill/skillsync/file_outbox_tmp_cleanup_promises_test.go:17`（两个 `os.CreateTemp` 半截文件被删；`outbox-abc.tmp`、`outbox-.tmp`、`outbox-12.tmp.keep`、`xoutbox-12.tmp`、`outbox-12.TMP`、`notes.tmp`、目录 `outbox-7.tmp`、指向目录外的符号链接 `outbox-8.tmp` 保留）。
4. **控制流**：打开 → 扫描（数 `.packet` 与清理）→ 有删除则 fsync 目录。
5. **失败处理**：删除失败（ErrNotExist 除外）让打开失败（fail-closed）。
6. **测试**：修前 `crash leftover outbox-2862453243.tmp survived reopening the outbox (stat err=<nil>)`；修后通过；`GOOS=windows go vet ./skill/skillsync` 通过。
7. **性能**：只在打开时扫描一次。
8. **未验证**：Windows 上未实际运行（只 `GOOS=windows go vet`）——Windows 暂存，不保证正确（外部 E25）。
9. **review 检查点**：判别依赖 Go 1.27 `os.CreateTemp` 的随机部分是 `uint32` 十进制（记录写明）；升级 Go 版本时复核。

<a id="noncore-34"></a>
### NONCORE-34 N10 第一批（NC-120～123）

[说明](guide-cfg-skill-noncore.md#noncore-34)

1. **提交与版本**：`556d156d`。首发 v1.20.1。NC-122 的“外层返回 `ErrReentrantMutation`”在 v1.20.2 被 B7（`a9b7075b`）整体取代。
2. **改动**：`ai/controller.go:117` `Freeze`（只暂停 Tick）、`ai/behavior_strategy.go`；`actionflow/action_runner.go`（`ClearQueue` / `EndAll` / `ClearMission` 为丢弃项发 OnEnded；启动失败 Cancel 重入保留当前位）；`hotcode/registry.go`（当前函数、Meta、代数合成一个不可变状态原子替换，`:190` `Replace`、`:212` `Revert` 按点串行）。
3. **不变量与守卫**：`ai/frozen_completion_promises_test.go`、`actionflow/queue_discard_promises_test.go`、`actionflow/start_failure_reentrancy_promises_test.go`、`hotcode/concurrent_patch_promises_test.go`（有界 100000 轮对撞，无调度注入点）。
4～5. 见说明。
6. **测试**：修前（[n10 red](../../bugfix/evidence/noncore-bugfix-20261005-n10/red.txt)）：

   ```text
   frozen_completion_promises_test.go:60: after Recover the tree never saw the completion that arrived while frozen: launched=1 results=[]
   ClearQueue dropped queued action 2 without OnEnded (ended=map[])
   start_failure_reentrancy_promises_test.go:67: current = <nil>, want the action the reentrant Start installed (started 1 time(s)); failing left 2 time(s)
   round 3708: torn patch point: resolve=1 patched=false meta={Version:v2 ...} generation=2
   ```

7. **性能**：无。
8. **未验证**：Linux / Windows 上的真实插件加载（外部 E27，Windows 部分暂存，不保证正确）。说明：NC-123 的红是有界对撞而不是受控调度，单核 / `GOMAXPROCS=1` 环境里修前也可能不红，修后的正确性不依赖概率；插件真实 `.so` 路径由 NC-244 补的 `hotcode/plugintest` 覆盖（NONCORE-36）。
9. **review 检查点**：hotcode 状态是否只经一次原子 Store 发布（读者不会看到 Patched 与 Meta 不一致）。

<a id="noncore-35"></a>
### NONCORE-35 B7 actionflow 延后队列

[说明](guide-cfg-skill-noncore.md#noncore-35)

1. **提交与版本**：`b694f0ba`（记录第三轮决定）、`a9b7075b`（实施）、`3a71321c`（标注）。首发 v1.20.2。
2. **改动与符号**：

   | 位置 | 职责 |
   | --- | --- |
   | `actionflow/action_runner.go:362` `submit` | 回调期间（`executing`）的变更入队，否则立即执行；判定只在这一处 |
   | `actionflow/action_runner.go:379` `drain` | 最外层调用返回前按发起顺序执行队列；回调 panic 恢复成错误 |
   | `actionflow/action_runner.go:196` `Deferring` | 回调里可查是否处于延后态 |
   | `actionflow/action_runner.go:19-31` | `ErrReentrantMutation`（保留导出、不再返回）、`ErrDeferredQueueFull`、`ErrDeferredRunaway`、默认 64 / 1024 |
   | `actionflow/runtime.go`、`ai/behavior_strategy.go` | 配合与注释 |

3. **不变量**：拿到 ID 的动作一定收到 OnEnded；同一动作至多被取消一次；队列与步数有界。守卫：`actionflow/deferred_mutation_promises_test.go`、`actionflow/reentrancy_promises_test.go`。删掉了 U-0100 / NC-122 那组事后比对当前位的六处分支。
4. **控制流**：

   ```text
   外层调用 → executing=true → 回调里 Start / Enqueue / … → submit 入队（Start / Enqueue 返回已分配 ID 与 nil）
   回调返回 → drain：按序执行；执行中再触发的命令继续入队（MaxDeferredSteps 截停）→ executing=false → 返回
   ```

5. **失败处理**：超出 `MaxDeferredCommands` 返回 `ErrDeferredQueueFull`（被拒的 Start / Enqueue 不分配 ID）；超出 `MaxDeferredSteps` 丢弃剩余命令、已交出 ID 的动作各收到一次结束，返回 `ErrDeferredRunaway`。
6. **测试**：修前红（旧代码，临时桩 `Deferring()` 与两个哨兵，方案 §4 原文）：

   ```text
   --- FAIL: TestCallbackMutationsAreDeferredUntilTheOuterCallReturns/start
       outer call from start = taskflow: reentrant mutation changed current action, want nil: a callback mutation is deferred, not a conflict
   --- FAIL: TestCancelReentryCancelsTheReplacedActionOnce/start_failure
       failing action canceled 2 time(s), want 1
   --- FAIL: TestDirectStartFailureEndsTheActionOnce
       failed action left 1 / ended 0 time(s), want 1 / 1
   --- FAIL: TestDeferredStartThatCanNoLongerRunStillEndsItsID
   ```

7. **性能**：未做基准（回调内多一次入队）。
8. **未验证**：无（仓内无生产使用方，B7 选项 c 的理由）。
9. **review 检查点**：`rg 'ErrReentrantMutation' actionflow` 只剩定义与注释；`drain` 在 panic 后是否复位 `executing`（NC-242 补的就是这里，见 NONCORE-36）。

<a id="noncore-36"></a>
### NONCORE-36 N10 第二批（NC-240～247）

[说明](guide-cfg-skill-noncore.md#noncore-36)

1. **提交与版本**：`44964553`（分支 `revn10b`）、`dc877c04`（标注）。首发 v1.21.0。
2. **改动**：`ai/nodes.go`（Parallel 结果已定即停）、`ai/controller.go:67` `SetStrategy` / `:177` `Shutdown`（回调中延后）；`actionflow/action_runner.go`（`Update` panic 恢复、替换时旧 Cancel panic 只报 OnError）；`hotcode/plugin.go:27` `LoadPlugin`（接口变量导出的 Bundle）、`hotcode/registry.go:261` `ApplyBundle`（失败恢复到加载前那一代）、`hotcode/admin.go`（revert 与加载串行）、`hotcode/registry.go:134` `Resolve[T]` 与 `:39` `ResolveMismatches`；新测试包 `hotcode/plugintest`（真实 .so）。
3. **不变量与守卫**：`ai/parallel_decision_promises_test.go`、`ai/switch_in_callback_promises_test.go`、`actionflow/update_and_replace_promises_test.go`、`hotcode/plugintest/plugin_load_test.go`、`hotcode/patch_visibility_promises_test.go`。
4～5. 见说明。
6. **测试**：修前（[n10b red](../../bugfix/evidence/noncore-bugfix-20261006-n10b/red-ai.txt) 等）：

   ```text
   parallel_decision_promises_test.go:43: a child after the deciding one was ticked: launched=1 interrupted=1, want 0/0
   switch_in_callback_promises_test.go:77: events = [end_actions stop launch], want [launch end_actions stop]: the replaced tree kept launching after it was stopped
   update_and_replace_promises_test.go:26: runner still reports Deferring() after Update's fn panicked: every later mutation is parked forever
   update_and_replace_promises_test.go:103: Start replacing an action whose Cancel panics = (0, taskflow: action cancel panic: cancel exploded), want an ID and nil
   plugin_load_test.go:117: LoadPlugin = hotcode: PatchBundle in <tmp>/bundle.so does not implement hotcode.Bundle
   plugin_load_test.go:164: after the failed load first(1) = 101, want 11: the partially applied patch was left in place (101) or rolled back past the previous generation (2)
   patch_visibility_promises_test.go:24: List = {Name:closure ... Generation:1 Patched:false Meta:{Version:v2 ...}}, want Patched=true with the patch's Meta: the patch is in effect
   patch_visibility_promises_test.go:52: Resolve[namedAdder] returned the fallback (2), want the patch (101): a registered patch never takes effect for this caller
   ```

   修后 `go test -race -count=3 ./ai ./actionflow ./hotcode/...` 通过。
7. **性能**：无。
8. **未验证**：Linux 真实插件加载（外部 E27）；Windows 部分暂存，不保证正确。
9. **review 检查点**：`ApplyBundle` 回滚目标是“加载前那一代”而不是原函数（用例 `first(1) = 11` 钉住）。

<a id="noncore-37"></a>
### NONCORE-37 MissionRunner 延后队列；EndAll 清场文档

[说明](guide-cfg-skill-noncore.md#noncore-37)

1. **提交与版本**：`87de077f`（记录第五轮决定）、`f6828f17`（实施，分支 `d1mr`；同提交含 D1 `/readyz`，属 APP）、`ce79ef18`（标注）。首发 v1.21.0。
2. **改动**：`actionflow/mission_runner.go:231` `submit`、`:248` `submitWithoutResult`、`:138` `Deferring`、`:31-34` `MaxDeferredCommands` / `MaxDeferredSteps`；SetRuntime 补 recover、`executing` 在 defer 里复位；starting / ending 与 `ErrReentrantMutation` 分支删除。`actionflow/action_types.go`、`actionflow/runtime.go` 包注释；`ai/controller.go` `SetStrategy` 注释；kit/README。
3. **不变量与守卫**：`actionflow/mission_deferred_promises_test.go`（含 `TestMissionDeferredQueueIsBounded`、`TestMissionRunnerStaysUsableAfterCallbacksPanic`、`TestDeferredMissionStartErrorsAreReported`）；EndAll 控制 `TestEndCurMissionBeforeEndAllLeavesNothingRunning`（修前即绿）。两条既有用例按延后语义改写（理由逐条写在方案 §7）。
4. **控制流**：同 B7。
5. **失败处理**：执行错误经 OnError（`taskflow: deferred start mission: …`）。
6. **测试**：修前（旧 `mission_runner.go`，临时桩 `Deferring()`）：

   ```text
   mission_deferred_promises_test.go:141: StartMission from state = taskflow: reentrant mutation changed current action, want nil: a callback mutation is deferred, not a conflict
   mission_deferred_promises_test.go:144: mission issued from mission tick started inside the callback (1 start(s)), want after the outer call
   --- FAIL: TestDeferredMissionStartErrorsAreReported
       deferred StartMission returned [taskflow: reentrant mutation changed current action ×3], want nil, nil, nil
   --- FAIL: TestMissionRunnerStaysUsableAfterCallbacksPanic
       deferred start after panics: current=<nil> next starts=0
   ```

7～8. 无。
9. **review 检查点**：ai `OnMissionEnd` 里 SetMission 的语义变化（`ErrReentrantMutation` → nil + 结束后启动）是否在 ai 的注释里写明。

<a id="noncore-38"></a>
### NONCORE-38 ai O-T3 / O-T4 文档

[说明](guide-cfg-skill-noncore.md#noncore-38)

1. **提交与版本**：`88f33776`（收尾第 1 批）。首发 v1.23.0（本版）。
2. **改动**：`ai/strategy.go`（`Strategy`、`StoppableStrategy` 注释）、`ai/controller.go`（`Controller.Shutdown` 注释）、kit/README ai 段。
3～8. 只改注释。
9. **review 检查点**：注释描述与 `Controller` 实际调用顺序一致（Init 之后 EndActions、Shutdown 不调 EndActions）。

<a id="noncore-39"></a>
### NONCORE-39 N11 第一批（NC-140～147）

[说明](guide-cfg-skill-noncore.md#noncore-39)

1. **提交与版本**：`d0d98a25`（登记）、`23a10f42`（修复）。首发 v1.20.1。World 定时器的组件逆操作在 v1.20.2 被 A1（`5407f127`）取代。
2. **改动**：`timer/scheduler.go:259` `Tick`（Tick 期间的取消 / 改期立即生效，重入 Tick 只由最外层收尾）；`spatial/block_index.go:71` `BlockRect`（int64 上界）；`index/index.go`（NaN、混合动态类型键、零值 `OrderedIndex`）；`demo/game/entities/world/timer_component.go.tmpl`。
3. **不变量与守卫**：`timer/scheduler_promises_test.go`、`spatial/block_index_promises_test.go`、`index/index_promises_test.go`；生成工程 `timer_rollback_test.go`（模板）。
4～5. 见说明。
6. **测试**：修前：

   ```text
   timer_rollback_test.go:49: rolled back, but the heap still holds node 1: every later arm is refused as a repeat and the stored World has no deadline
   scheduler_promises_test.go:36: a timer cancelled before it fired still fired 1 time(s) in the same tick
   block_index_promises_test.go:45: block 1 rect {Min:{X:4611686018427387904 Y:0} Max:{X:-9223372036854775808 Y:1}} is empty or inverted
   panic: assignment to entry in nil map [recovered, repanicked]
   index_promises_test.go:50: rotation 0: Query panicked: reflect: call of reflect.Value.Uint on string Value
   index_promises_test.go:86: Get after Upsert on a zero OrderedIndex = "", false
   scheduler_promises_test.go:192: cancelling the firing timer after a re-entrant tick returned false
   ```

7. **性能**：无。
8. **未验证**：真实 WAL / Mongo 三进程链路里 `ArmActivity` / `TickWorldTimers` 的提交被拒（随 E19）。
9. **review 检查点**：邻近分支 `TestDeferredOperationsDuringTickKeepTheirMeaning` 修前修后都通过（行为保持）。

<a id="noncore-40"></a>
### NONCORE-40 timer D-L1 / D-L2

> 与其他分册重复：以 [CLK-6](impl-app-own-clk-ops-tool.md#clk-6) 为准，本条只保留编号与索引（汇总去重）。

[说明](guide-cfg-skill-noncore.md#noncore-40)

**提交与版本**：`b3538251`（第六轮决定）、`5abae51e`（实施，分支 `dl12`）、`e320578c`（标注）。首发 v1.21.0。

<a id="noncore-41"></a>
### NONCORE-41 PathFindSystem.Stop（NC-270）

[说明](guide-cfg-skill-noncore.md#noncore-41)

1. **提交与版本**：`36220f34`（revleft 批）。首发 v1.21.0。
2. **改动**：`demo/game/scene/runtime/pathfind.go.tmpl`（`Stop` 只置原子标志）。
3. **不变量**：停止与并发寻路无数据竞争。守卫：生成工程 `TestPathFindingWhileTheSceneStopsIsSafe`（`demo/game/scene/runtime/runtime_test.go.tmpl`，`-race`）。
4～5. 无。
6. **测试**：修前 `-race` 连续 3 次红：

   ```text
   WARNING: DATA RACE
   Write at 0x00c0000020a8 by goroutine 8:
     example.com/revleftgd/game/scene/runtime.(*PathFindSystem).Stop()
   --- FAIL: TestPathFindingWhileTheSceneStopsIsSafe (0.00s)
       testing.go:1865: race detected during execution of test
   ```

7～8. 无。
9. **review 检查点**：已生成工程需 `roost project sync`。

<a id="noncore-42"></a>
### NONCORE-42 failurelog 结果未知不降级（NC-160）

[说明](guide-cfg-skill-noncore.md#noncore-42)

1. **提交与版本**：`b248a199`（登记）、`f750ce43`（修复）。首发 v1.20.2。
2. **改动**：`failurelog/failurelog.go:98` `AppendRaw`、`:145` `Purge`、`:178` `DeleteRaw`：Lua 报错原样返回；降级只在 Eval 返回 `(nil, nil)` 时。
3. **不变量**：结果未知不补做非原子写。守卫：`failurelog/unknown_result_promises_test.go`；真实 Redis `unknown_result_integration_test.go`（代理吞掉第一条 EVAL 的回复并断开）。
4～5. 见说明。
6. **测试**：修前：

   ```text
   unknown_result_integration_test.go:153: one AppendRaw stored 2 copies in real Redis [{"msg_id":"m-1",...} {"msg_id":"m-1",...}] (err=<nil>); want 1
   append: one AppendRaw whose script ran but whose reply was lost stored 2 entries [dead-letter dead-letter] (err=<nil>)
   purge:  Purge whose script ran but whose reply was lost left [] (err=<nil>); the dead letter written after the purge must survive
   ```

7. **性能**：无。
8. **未验证**：Redis Cluster（外部 E08）。`DeadLetter` 写失败之后不存在重投链：按 bus 去重契约（NONCORE-8，`bus/reliable.go` `ReliableStore` 注释，第十二轮“保持并写进契约”），消息只留 `bus: write dead letter failed` 错误日志（`bus/bus.go:1036`），InboxTTL 内同 MsgID 的投递按重复跳过；本修复只让结果未知也走这条既有分支。
9. **review 检查点**：与 A2（DRV）口径一致——写结果未知交给调用方。

<a id="noncore-43"></a>
### NONCORE-43 robot / statslog / log（NC-161～165）

> 其他分册对应：NC-165 另见 APP-12 背景。

[说明](guide-cfg-skill-noncore.md#noncore-43)

1. **提交与版本**：`5fea59ce`（NC-161）、`efeede0f`（NC-162）、`92547035`（NC-163）、`2a9e0c2c`（NC-164）、`e798a759`（NC-165）、`c35fe606`（收口）。首发 v1.20.2。
2. **改动**：`robot/loadtest/manager.go` / `admin.go`、`metrics/metrics.go`（`HistogramCount`）；`robot/robot.go`；`robot/transport/transport.go`；`kit/statslog/statslog.go`；`log/log.go`。
3. **守卫**：`robot/loadtest/threshold_no_samples_promises_test.go`、`robot/capture_reconnect_promises_test.go`、`robot/transport/dial_bounds_promises_test.go`、`kit/statslog/gauge_lifecycle_promises_test.go`、`log/close_fallback_promises_test.go`。
4～5. 见说明。
6. **测试**：修前：

   ```text
   threshold_no_samples_promises_test.go:35: a run with no completed scenario ended finished/completed
     thresholds=[{error_rate Max:0 Actual:0 Violated:false} {p95 Max:1 Actual:0 Violated:false}]; want failed/threshold
   capture_reconnect_promises_test.go:74: a push on the reconnected session reached the capture 0 times; want 1
   dial_bounds_promises_test.go:78: websocket dial still blocked after 3s (ctx 1m0s, DialTimeout 200ms): the handshake ignores both
   gauge_lifecycle_promises_test.go:95: entity.count_by_kind{kind=2} = 2 after every kind-2 entity unloaded; the record says 0
   close_fallback_promises_test.go:65: after Close of a file-only sink "server exit" went nowhere (stderr="")
   ```

7～8. 无。
9. **review 检查点**：NC-161 的 Markdown 报告 verdict 写出原因（`**FAIL** (no_samples)`）。

<a id="noncore-44"></a>
### NONCORE-44 N12 留项（NC-261～266）

[说明](guide-cfg-skill-noncore.md#noncore-44)

1. **提交与版本**：`36220f34`（revleft）、`7f964564`（T 编号顺延）。首发 v1.21.0。
2. **改动**：`robot/session/session.go`（NC-261 / 262）、`log/log.go` / `log/rotation.go`（NC-263）、`metrics/prometheus.go`（NC-264）、`robot/robot.go`（NC-265）、`robot/loadtest/manager.go` / `robot/runner/runner.go`（NC-266）；`OBSERVABILITY.md`。
3. **守卫**：`robot/session/session_promises_test.go`、`log/write_failure_promises_test.go`、`metrics/prometheus_escape_promises_test.go`、`robot/coalescer_close_promises_test.go`、`robot/loadtest/duration_stop_reason_promises_test.go`。
4～5. 见说明。
6. **测试**：修前（[revleft 证据](../../bugfix/evidence/noncore-bugfix-20261006-revleft/green.txt) 同目录 red 文件）：

   ```text
   session_promises_test.go:121: Call ignored its context while the write was blocked (still waiting 2s after a 50ms deadline)
   session_promises_test.go:196: WaitPush delivered "late response", want the push: a response that arrived after its Call timed out was delivered as a push
   write_failure_promises_test.go:84: Write while the next slice cannot be opened = open <tmp>/app.2026100611.log: permission denied, want nil: the current slice is still writable
   prometheus_escape_promises_test.go:29: exposition = "m{v=\"a\\tb\"} 1\n", want "m{v=\"a\tb\"} 1\n"
   coalescer_close_promises_test.go:39: Close returned while the final flush had not finished (the batch is still being sent)
   duration_stop_reason_promises_test.go:37: a run cut by its 100ms Duration ended finished/completed; want stop_reason=duration
   ```

7. **性能**：无。
8. **未验证**：真实网关上的发送缓冲满、`-duration` 重跑（外部 E05）；真实磁盘写满（外部 E19）。说明：目录权限用例在 root 用户下自动跳过（root 不受权限位限制），本机非 root 已跑。
9. **review 检查点**：新指标 `robot.session.late_response{msg}`、`log.rotate_failures`、`log.write_errors{sink}` 标签低基数（msg 是消息类型名，sink 是固定枚举）。

<a id="noncore-45"></a>
### NONCORE-45 robot Stage 序号只增不回收（RR-20261006-09）

> 与其他分册重复：以 [OPS-7](impl-app-own-clk-ops-tool.md#ops-7) 为准，本条只保留编号与索引（汇总去重）。

[说明](guide-cfg-skill-noncore.md#noncore-45)

**提交与版本**：`7b73aabc`（第十二轮 kit 批，分支 `bkit`）、`d6a677e0`（标注）。首发 v1.23.0（本版）。

<a id="noncore-46"></a>
### NONCORE-46 N13（NC-180～185 与复审补修）

[说明](guide-cfg-skill-noncore.md#noncore-46)

1. **提交与版本**：`e7bbac3d`（登记）、`7e4ed438`（NC-180 / 181 container）、`20400337`（NC-184 / 185）、`1d600b9b`（NC-182）、`4c26b4b5`（NC-183）、`815c3661`（NC-180 复审：entity 持引用）、`c10cc9ac` / `8d6f4652`（记录与复审）。首发 v1.20.2。
2. **改动**：`container/bucket.go:157` `Bucket.Range`（读锁内复制、锁外回调）、`:68` `RangeAll` / `:49` `RangeWithCursorCnt`（false 跨桶停止）；`entity/entity_manager.go:337` `Range`、`:343` `RangeByCategory`、`:369` `rangeHeld`（Touch / UnTouch）；`safemap/fast.go:54` `Set`、`:133` `Clear`、`:149` `Range`；`goroutine/task_pool.go`（受理与关闭互斥）；`container/topologic_sort.go`、`container/keymap.go:97` `Range`。
3. **不变量与守卫**：`container/bucket_range_promises_test.go`、`entity/entity_manager_range_promises_test.go`（含 `TestManagerRangeNeverHandsOutAClearedEntity`）、`safemap/fastmap_range_promises_test.go`、`goroutine/task_pool_shutdown_promises_test.go`、`container/keymap_topo_promises_test.go`；生成 `VarietyDao` 组合（真实文件 WAL 重放，`-race -count=50`）。
4. **控制流**：快照 → 锁外逐个回调 → 实体先 `Touch`，失败跳过 → 回调后 `UnTouch`（最后一个 UnTouch 才清理被销毁的实体）。
5. **失败处理**：无。
6. **测试**：修前：

   ```text
   bucket_range_promises_test.go:41: RangeAll did not return: the callback is waiting for the bucket lock its own Range holds
   entity_manager_range_promises_test.go:37: Range did not return: Destroy inside the callback waits for the bucket lock Range holds
   bucket_range_promises_test.go:63: callback ran 8 times after returning false on the first entry, want 1
   update_every_existing_key: Range delivered map[0:3 1:1 3:1 4:1], want each of [1 2 3 4 5 6] exactly once
   task_pool_shutdown_promises_test.go:56: Submit panicked 60 times while Shutdown ran; first: send on closed channel
   keymap_topo_promises_test.go:18: A depends on unregistered B: got [], want [B A]
   keymap_topo_promises_test.go:57: Range delivered map[0:1 1:1 2:1 3:1], want each of 1..4 exactly once
   ```

   复审：`TestManagerRangeNeverHandsOutAClearedEntity` 修前两叶红（`id=0 category=0`）。
7. **性能**：`EntityManager.Range` 10 万实体约 0.86ms → 4.1ms（Apple M5，两次 CAS / 实体）；`BenchmarkFastMapSetGet` 修前 38.09ns ± 22%、修后 38.71ns ± 26%（p=0.937），0 allocs。
8. **未验证**：无。随机模型探针（2 万个种子）未发现违反。`TaskPool.totalTasks` 入队后才加、统计瞬间可能 completed > total，按 [revleft 记录](../../review/REVIEW-2026-10-06-revleft.md) §4 O9 的处置不改（零调用方 API，C8 决定“保留”；没有确定性红测试）。
9. **review 检查点**：`EntityManager.Range` 持引用的协议与 nest 分发持有实体引用是同一协议——确认没有引入快池等待（`Touch` / `UnTouch` 是原子计数、不阻塞）。

<a id="noncore-47"></a>
### NONCORE-47 C7 遍历回调仓库级契约

[说明](guide-cfg-skill-noncore.md#noncore-47)

1. **提交与版本**：`d346b45c`（第二轮决定）、`cd43a5ac`（实施）、`3a71321c`（标注）。首发 v1.20.2。
2. **改动**：`internal/rangecontract/rangecontract.go:66` `Check`（共用回归辅助）；`container/bucket.go`（`RangeWithCursorCnt` 进入时按游标定下要走的桶 `start+i`）；`entity/entity_manager.go:487` `RangeGroupEntities` 与 `rangeHeld` 共用 `visitHeld`；`safemap/map.go`、`sharded.go`、`small.go` 注释；`codegen/scripts/dao-golden-runtime.sh`；roost-coding 第 78 行、根 README §16。
3. **不变量**：见说明的契约细节。守卫：`container/range_contract_promises_test.go`、`safemap/range_contract_promises_test.go`、`entity/range_contract_promises_test.go`、生成 DAO `codegen/internal/dao/testdata/runtime/range_contract_test.go`。
4. **控制流**：同 NONCORE-46。
5. **失败处理**：无。
6. **测试**：修前（方案 §4 原文）：

   ```text
   --- FAIL: TestContainerRangeContract/BucketHolder.RangeWithCursorCnt/read_inside_the_callback
       rangecontract.go:160: Range handed out key 8 twice
   --- FAIL: TestEntityRangeContract/EntityManager.RangeGroupEntities/delete_inside_the_callback
       rangecontract.go:212: Range handed out key 0 (value 0) that never existed
   ```

7. **性能**：无新增（`RangeGroupEntities` 仓内零调用方）。
8. **未验证**：无（不适用：index / lock 无遍历回调；`ShardedSafeMap.Read` / `Compute` 不是遍历回调，C7 已定）。
9. **review 检查点**：新增或修改遍历入口时是否套了 `rangecontract.Check`（roost-coding 要求）；nest 的 `EntityLockGroupScope.Range` 走 `GetGroupEntities`，未改。

<a id="noncore-48"></a>
### NONCORE-48 container / goroutine 零调用方 API（NC-267～269）

[说明](guide-cfg-skill-noncore.md#noncore-48)

1. **提交与版本**：`36220f34`。首发 v1.21.0。
2. **改动**：`container/object_pool.go:60` `Put`（重复 Put 忽略）、`container/topologic_sort.go:24` `RegisterCompDependency`（复制切片）、`goroutine/task_pool.go`（一次性）。
3. **守卫**：`container/pool_topo_aliasing_promises_test.go`、`goroutine/task_pool_restart_promises_test.go`。
4～5. 无。
6. **测试**：修前：

   ```text
   pool_topo_aliasing_promises_test.go:21: two Gets after a double Put returned the same object 0x…: both callers now share it
   pool_topo_aliasing_promises_test.go:45: dependencies of A after the caller reused its slice = [X C], want [B C]
   task_pool_restart_promises_test.go:24: IsRunning = true after Start on a shut-down pool, while every Submit is refused
   task_pool_restart_promises_test.go:38: Shutdown after Shutdown-then-Start = task pool shutdown timeout after 200ms, want nil (the workers started after the first Shutdown must still stop)
   ```

7～8. 无。
9. **review 检查点**：无。

<a id="noncore-49"></a>
### NONCORE-49 Mongo URI 口令脱敏（NC-191）

[说明](guide-cfg-skill-noncore.md#noncore-49)

1. **提交与版本**：`efe219c1`（N14 登记）、`e1a6b01d`（修复）。首发 v1.20.2。
2. **改动**：`kit/mongo/mongo_mod.go:159` `redactedURI`（userinfo 取到 `?` 之前的最后一个 `@`，口令里没转义的 `/` 也盖得住）；只有日志用它，连接仍用原 URI。
3. **守卫**：`kit/mongo/uri_log_promises_test.go`（`TestStartDoesNotLogTheMongoPassword`：多主机、srv、无选项；`TestRedactedURIKeepsEverythingButThePassword`：7 种形状）。
4～5. 无。
6. **测试**：修前 `uri_log_promises_test.go:63: Start logged the Mongo password: "... msg=\"mongo mod: connected\" uri=\"mongodb://roost:s3cret-pw@db1:27017,db2:27017/?replicaSet=rs0\"\n"`。
7. **性能**：无。
8. **未验证**：无。mongo-driver 自身的错误信息：本次重核在 `e6828e4f` 依赖的 mongo-driver v2.6.0 上用临时探针（未入库）试了 12 种带口令的 URI——端口非数字、口令里非法转义 / 未转义的 `:` 与 `@`、非法选项值、非法 authMechanism、srv 带端口、srv 解析失败、错误 scheme、连不上的主机（server selection 超时）、以及对隔离环境 Mongo 的认证失败（`auth error: sasl conversation error …`）——错误文本都不含口令。
9. **review 检查点**：未采用 `net/url`（多主机与 `mongodb+srv` 解析边界不稳定）。

<a id="noncore-50"></a>
### NONCORE-50 启动失败先收回 Service（NC-193）

> 与其他分册重复：以 [APP-9](impl-app-own-clk-ops-tool.md#app-9) 为准，本条只保留编号与索引（汇总去重）。

[说明](guide-cfg-skill-noncore.md#noncore-50)

**提交与版本**：`d6550a16`。首发 v1.20.2。

<a id="noncore-51"></a>
### NONCORE-51 N15 脚本与门禁（NC-200～208）、A5

> 其他分册对应：TOOL-1（NC-205 pretag）、TOOL-7（NC-207 与全局命令持锁），其余 N15 项本条为主。

[说明](guide-cfg-skill-noncore.md#noncore-51)

1. **提交与版本**：`76ece142`（登记）、`6c1538be`（修复）、`d43aa3ba`（NC-208 kit/dataengine 补修）、`3e3350d5`（A5 / NC-203 复核补修）。首发 v1.20.2。
2. **改动**：`scripts/gapmap.sh`（NC-200）、`kit/scripts/integration/redis-cluster-suites.sh`（NC-201）、`kit/scripts/integration/lib/toxiproxy.sh` / `lib/common.sh` / `dataengine-env.sh`（NC-202 / 203）、`cmd/glsvet/main.go`（NC-204）、`scripts/pretag.sh`（NC-205）、`codegen/internal/roost/render.go`（NC-206 `.gitignore`）、`scripts/test-remote-matrix.sh`（NC-207）、`scripts/remote-fault.sh`、`scripts/perf/remote.sh`、`redis/driver` / `kit/nats` / `kit/dataengine` toxic 用例（NC-208）；A5：`scripts/test-remote-generated.sh`、`kit/scripts/integration/dataengine_env_test.sh`。
3. **守卫**：`cmd/glsvet/inputs_promises_test.go`（测试二进制经 TestMain 充当命令，断言退出码）、`codegen/internal/roost/gitignore_runtime_promises_test.go`（真实 `git check-ignore --no-index`）；其余为脚本红绿（证据目录 [n15 review](../../review/evidence/noncore-review-20261005-n15/nc200-red.txt) 同目录）；A5 根包守卫点名 failover 用例与 `scripts/test-remote-generated.sh`。
4～5. 见说明。
6. **测试**：修前（问题记录原文）：

   ```text
   RESULT: uncommitted work LOST
   ROOST_DATAENGINE_IT=1
   REDIS_ADDR=127.0.0.1:1
   toxiproxy_running=true (pid 57408 is: sleep 300)
   RESULT: bystander pid 57408 KILLED
   RESULT: lock dir (and root) REMOVED by reset while held
   glsvet …/no-such-package: exit 0, output ""; want exit 2 naming the missing directory (nothing was vetted)
   pretag: example.com/pretagred@v1.99.0 is ready to tag
   generated .gitignore does not ignore data/wal/dataengine/000001.wal, which generation itself treats as runtime output
   after redis/driver: bystander toxics=[]
   ```

   A5 红（基线 `bb3aa647`）：垫片一节四条全部 `lock=free`。NC-207 绿：8 个 go test 格 `FAIL(no tests ran)`，矩阵 exit 1。
7. **性能**：无。
8. **未验证**：Linux 上 NC-202 的 pid 认领（外部 E26）。修后 heal / 矩阵已在真实共享隔离环境上实跑：`scripts/test-remote-matrix.sh` 先 `dataengine-env.sh heal` 再逐格跑、每格前再 heal，v1.20.2 / v1.21.0 / v1.22.0 发版矩阵各 21/21（`matrix-v1202-c85d4565` / `matrix-v1210-4881f2b7` / `matrix-v1220-9bf690fb`，见 [交接 §7](../../CORE-OPTIMIZATION-HANDOFF.md)）。
9. **review 检查点**：`kit/scripts/integration/README.md` 的共享使用规则与 roost-bugfix lessons 是否一致（NC-205 pretag 以 TOOL-1 为准）。

<a id="noncore-52"></a>
### NONCORE-52 停机三步（NC-170～174）

> 其他分册对应：APP-6（A3 停机契约骨架，背景提到 NC-170～174），本条为主。

[说明](guide-cfg-skill-noncore.md#noncore-52)

1. **提交与版本**：`c99a687d`、`ae66d593`（记录）；NC-173 的 A3 复核补修 `50f2ac2a`。首发 v1.20.2。
2. **改动**：`manager/engine.go:209` `Stop`；`kit/nest/nest_mod.go:291` `Stop` / `:295` `StopWithContext`；`sync/syncbus/driver/jetstream.go:309` `Stop` / `:319` `StopWithContext`；`etcd/driver/discovery.go:203` `Deregister`、`etcd/driver/assembly.go:76` `Close`；`sync/syncbus/mirror/envelope.go:148` / `:169`；`remoteentity/assemble.go:213` `Stop`。
3. **不变量**：①发起关闭幂等 ②ctx 内等真实排空，超时返回错误并保留对象 ③排空后才释放。守卫：`manager/stop_retry_promises_test.go`、`kit/nest/unload_resync_stop_retry_promises_test.go`、`sync/syncbus/driver/jetstream_stop_drain_promises_test.go` / `jetstream_stop_retry_promises_test.go`、`etcd/driver/deregister_budget_promises_test.go`、`remoteentity/assembly_replica_drain_promises_test.go`；真实 nats-server / etcd（测试自起私有进程）。之后 A3 的 `internal/stopcontract.Check` 套了这些入口（APP）。
4. **控制流**：见不变量。
5. **失败处理**：不配合 ctx 的 `StopWithContext` 不会被终止，只是如实超时并保留；只实现 `Stop()` 的 manager 没有期限（与修前相同）。
6. **测试**：修前：

   ```text
   retry Stop reported success while manager "dependent" was still running (calls=1)
   retry Stop reported success while the unload resync worker was still running (stop handle calls=1)
   Stop returned while a handler was still running on the subscription
   stop_contract_test.go:99: stopcontract: Stop after a completed stop = context canceled, want nil
   Stop reported success while a replica handler was still applying
   ```

   真实 nats-server：修前 `err=<nil> bus_retained=false nats_closed=true`、`ack_pending=1 ack_floor=0`；修后 `err=context deadline exceeded bus_retained=true nats_closed=false`、重试 `err=<nil>`、`ack_pending=0 ack_floor=1`。真实 etcd：修前 `client_open=false`、重试 `grpc: the client connection is closing`；修后 `elapsed=500ms client_open=true`、解冻后重试 `err=<nil> keys=0`。
7. **性能**：无。
8. **未验证**：JetStream / etcd 多节点 HA（外部 E06 / E07）。
9. **review 检查点**：A3 的 `internal/stopcontract.Check` 是否套在本条列出的全部停止入口上（骨架本身见 APP-6）；记录里 `-race -count=10` 通过。

<a id="noncore-53"></a>
### NONCORE-53 Mongo Mod 停止收敛（NC-260）

> 其他分册对应：DRV-5 的 Close 口径表列了 NC-260，本条为主。

[说明](guide-cfg-skill-noncore.md#noncore-53)

1. **提交与版本**：`36220f34`。首发 v1.21.0。v1.23.0 的 RR-20261006-10（DRV）把 Close 口径统一到全部驱动与 Mod。
2. **改动**：`mongo/driver/client.go:119` `Close`（已断开返回 nil）。
3. **守卫**：`kit/mongo/stop_retry_promises_test.go`。
4～5. 无。
6. **测试**：修前：

   ```text
   stop_retry_promises_test.go:31: Stop #1 on an already disconnected client = client is disconnected, want nil: the connection is released and a retry can never succeed
   stop_retry_promises_test.go:53: Close on the released client = client is disconnected, want nil
   ```

7. **性能**：无。
8. **未验证**：无。真实 Mongo：本次重核在 `e6828e4f` 上把 `kit/mongo/stop_retry_promises_test.go` 的两个用例临时换成隔离环境的真实副本集（先 Ping 确认已连上，探针未入库）：已断开客户端上连续两次 Stop 返回 nil、正常停止后再 Stop 与再 Close 返回 nil，通过。
9. **review 检查点**：`mongo/driver/client.go:119` `Close` 在 RR-20261006-10 统一口径（DRV-5，串行化）之后仍对已断开的客户端返回 nil。

<a id="noncore-54"></a>
### NONCORE-54 nest 用例隔离（A2 / A3）与派发取锁要求 Guard 作用域（RR-20261006-12，原 W-2026-10-06-01）

[说明](guide-cfg-skill-noncore.md#noncore-54)

1. **提交与版本**：`611d5d72`（收尾第 4 批 A2 / A3，只改测试）、`53fd9e9c`（标注）；`b7471ae4`（RR-20261006-12 修复，分支 `fixn`，基线 `87d8d91e`；同提交的 RR-20261006-13 glsvet 跟进属 DAO 部分）、`71c8f394`（记录 fixn 合并时 -11 → -12 顺延）。均首发 v1.23.0（本版）。WANTED W-2026-10-06-01 已转 RR 并关闭（`docs/bug/WANTED.md` 标为已转）。
2. **改动与符号**（以 `e6828e4f` 为准）：

   | 位置 | 职责 |
   | --- | --- |
   | `nest/group_lock.go:237` `errDispatchWithoutGuardScope`、`:247` `dispatchScopeGuard` | 派发取锁只用当前 goroutine Guard 作用域里的 Guard；没有作用域返回未导出错误 |
   | `nest/group_lock.go:258` `lockDispatchEntitiesForHandlerWithStore` | 开头检查传入的 Guard 就是作用域里那一个（`:259-261`），否则取锁前返回错误、不取任何锁；组迁移重试（`:273-279`）只放本次取得的实体锁，Guard 不归还 |
   | `nest/nest_dispatch.go:370` `dispatchLoadedEntities`（`:375`）、`nest/group_transition.go:124` `groupTransitionDispatch`（`:134`，取组锁之前） | 用 `dispatchScopeGuard` 取 Guard，没有作用域时不留副作用 |
   | `nest/nest_dispatch.go:485` `releaseDispatchEntities` | 逆序只放 `acquired`；Guard 本身与其上的其他持有（handler 新建实体、被取代实例、解锁后回调）由作用域结束时统一释放，Guard 只在那时归还池一次。原 `releaseDispatchLocks`（`02c8a10d` 上 `nest/nest_dispatch.go:480`，无作用域时 `EntityGuardRelease` 整个归还 Guard）**已删除** |
   | `nest/group_lock_test.go:147` `withDispatchGuardScope`；`nest/cross_create_requeue_budget_promises_test.go:72-92` | A2 / A3：用例先建作用域再取锁；放行闸门是 `sync.Once` 包住的 `releaseGates`，后于停机 defer 登记、先执行，`Shutdown` 用 10s 上限的 ctx |
   | `nest/dispatch_guard_scope_promises_test.go`（新） | RR-20261006-12 三条回归，见第 6 点 |

3. **不变量**：Guard 的所有权只有一种——作用域；取锁、组迁移重试与派发释放从不归还 Guard，Guard 每次进池恰好一次。生产入口全在作用域里：派发函数的非测试调用方只有 `runNestLogic` 的 `switch`，它在 `switch` 之前 `entity.NewGuardScope`（`nest/nest_dispatch.go:204`）；`runNestLogic` 的调用方是快池派发与 `remote_dispatch.go` 的快续行。守卫：第 6 点三条用例；`go run ./cmd/glsvet -tests ./nest` 无输出（测试不在 go 语句里取无作用域 Guard）。
4. **控制流（修前缺陷机理）**：无作用域 → `GetEntityGuard()` 从池取 Guard → 第一次取锁后锁组变了 → `releaseLocks()` → 旧 `releaseDispatchLocks` 无作用域时把 Guard 整个归还池 → 重试仍用同一 Guard，成功后调用方再归还一次 → `sync.Pool` 里同一指针两份 → 之后两个 `NewGuardScope` 取到同一 Guard、两个快 worker 互相解对方的实体锁（`unlock of unowned mutex`，A2 的 `-shuffle` 失败）。修后：同样的调用在取锁前返回 `errDispatchWithoutGuardScope`。
5. **失败处理**：无作用域的调用拿到可 `errors.Is` 的未导出错误（只有直接调用内部函数的测试会走到）；没有改成 panic——入口本来返回 error，`runNestLogic` 的 recover 也会把 panic 变成错误，panic 没有额外收益。未采用“调用方持有、重试不归还”：Guard 上挂的不只本次取得的实体锁，只有完整的作用域释放能收尾，而且 handler 里 `CurrentGuardScope()` 为 nil 时 `Cast` 返回 `ErrCastNoContext`，与快池派发语义不一致（见 [修复记录](../../bugfix/RR-20261006-12.md)“两种修法的取舍”）。
6. **测试**：
   - A2 修前（`611d5d72` 之前，seed `1791263156350214000`，6 次里 4 次挂到 60s 包超时，另两次）：

     ```text
     cross_create_requeue_budget_promises_test.go:106: pair 2: winners=0 losers=2, want one each
     cross_create_requeue_budget_promises_test.go:97: pair 1 slot 2 exhausted the requeue budget after 401 attempts: nest: lock timeout: nest: created entity is locked by another holder: entity 10192837 cannot be waited for in lock order
     ```

   - RR-20261006-12 修前红（基线 `87d8d91e`，[问题记录](../../bug/RR-20261006-12.md)原文；锁组在第一次 `Lock` 成功后由测试 Mutex 钩子改掉，确定触发重试，不靠 sleep）：

     ```text
     dispatch_guard_scope_promises_test.go:69: dispatch locking returned the caller's guard to the pool 2 time(s) during a lock-group retry (err=<nil>); the guard belongs to its scope / caller and must be returned exactly once, by its owner
     --- FAIL: TestDispatchLockingWithoutGuardScopeNeverReturnsCallerGuardToPool (0.00s)
     dispatch_guard_scope_promises_test.go:147: singleDispatch without guard scope err = <nil>, want errDispatchWithoutGuardScope
     --- FAIL: TestDispatchEntriesRequireGuardScope (0.00s)
     ```

   - 修后（`nest/dispatch_guard_scope_promises_test.go`）：`TestDispatchLockingWithoutGuardScopeNeverReturnsCallerGuardToPool`（`:50`，无作用域 + 重试形状返回错误、归还 0 次、不留锁）、`TestDispatchLockingGroupRetryInScopeKeepsGuardUntilScopeEnds`（`:89`，作用域里重试只放实体锁、作用域结束归还恰好 1 次；修前也通过，钉住作用域路径）、`TestDispatchEntriesRequireGuardScope`（`:129`，`singleDispatch` / `groupTransitionDispatch` 无作用域返回错误、handler 不执行；放进作用域照常执行）。记录里 `go test -race -count=3 ./nest/... ./entity/...`、A2 seed 3 次、glsvet 三项均通过。
   - 本次重核（`e6828e4f`，有界 seed 扫描）：`go test -c ./nest` 后 `-test.shuffle` 取 1～20、101、202 … 909、`1791263156350214000`、1000～1199 共 230 个 seed 各跑 1 次，另用 `-race` 测试二进制跑 5000～5029 共 30 个 seed，**260 次全部通过**。
   - 复跑：`GOWORK=off go test -race -count=3 ./nest/... ./entity/...`；`go test -count=1 -shuffle=on ./nest`；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 与 `go run ./cmd/glsvet -tests ./nest`。
7. **性能**：无（生产路径不经过被删除的分支）。
8. **未验证**：无。
9. **review 检查点**：
   - nest 非测试代码里是否已没有 `entity.EntityGuardRelease` 调用（`rg 'EntityGuardRelease|GetEntityGuard\(' nest --glob '!*_test.go'` 在 `e6828e4f` 上只剩一行注释与 `nest/cast.go:115`、`:211`）；核对 `cast.go` 这两处取到的都是当前作用域的 Guard（`:211` 前有 `CurrentGuardScope() == nil` 检查，`:115` 前是 `currentNestDispatchMsg()` 检查）。
   - 派发函数的非测试调用方是否仍只有 `runNestLogic` 的 `switch`（`nest/nest_dispatch.go:219-230` 一带）；`broadcastDispatch` 每个目标自建作用域、不经这几个函数。
   - `lockDispatchEntities`（`nest/nest_dispatch.go:442`）的 `useTryLock` 注释已随本修复改写为 `dispatchScopeGuard`，确认注释描述与代码一致。

<a id="noncore-55"></a>
### NONCORE-55 Nest 重排抖动（U-0279）

[说明](guide-cfg-skill-noncore.md#noncore-55)

1. **提交与版本**：`47a9132c`。首发 v1.20.1。
2. **改动**：`nest/group_transition.go:411` `transientRequeueDelay`（下限 `entityGroupDispatchRequeueDelay` 加 `[0, 下限)` 均匀抖动；`:399` 是重排调用点。`02c8a10d` 上定义在 `:407`，原文写的 `:399` 指的是调用点）。
3. **不变量**：400 次上限与最短约 2s 的重排窗口不变。守卫：`nest/requeue_jitter_promises_test.go:19` `TestSymmetricTransientRequeuesAreNotReadmittedInLockstep`。
4～5. 无。
6. **测试**：见 [U-0279 记录](../../bugfix/U-0279-nest-requeue-jitter.md)；生成工程 `TestGeneratedDataEngineCrossCreateResolvesOnRealWAL` 修前正常负载下失败率 25%～55%。
7. **性能**：平均重排延迟 5ms → 7.5ms。
8. **未验证**：无。
9. **review 检查点**：这是核心线 Nest 代码，按 roost-coding 改 nest 要跑 `go run ./cmd/glsvet ./nest …`。

<a id="noncore-56"></a>
### NONCORE-56 被静态绑定取代的租约修复（RR-20261004-10 / 11 / 14）

> 与其他分册重复：以 [OWN-1](impl-app-own-clk-ops-tool.md#own-1) 为准，本条只保留编号与索引（汇总去重）。

[说明](guide-cfg-skill-noncore.md#noncore-56)

**提交与版本**：`18bb86ae`（-10）、`a28a3152`（-11）、`890abdda`（-14）、`5d1cb974` / `1cf96347`（记录）；同在 v1.20.0 被 `f051e24a`（静态绑定）删除，发布物里没有这段代码。

## 全局守卫测试与门禁（本部分）

| 守卫 | 位置 | 守什么 | 条目 |
| --- | --- | --- | --- |
| `TestFrameworkCodeDoesNotReadConfigLeniently` | `app/config_strict_reads_promises_test.go:140` | app、kit 与两份生成模板不出现宽松 getter（例外只有 `sid` 与 cobra flags） | CFG-2～4 |
| `TestEveryFrameworkDurationAndIntKeyIsCheckedStrictly` | `app/config_strict_reads_promises_test.go:125` | 扫出的时长 / 整数键都在登记表里；扫到少于 150 处即认为模式失效 | CFG-2 |
| `TestEveryFrameworkBoolSwitchIsCheckedStrictly` | `app/config_types_promises_test.go:93` | 布尔开关都在登记表里 | CFG-1 |
| `TestGeneratedConfigsPassStrictAndProductionValidation` | `codegen/internal/roost/generated_config_validation_promises_test.go:155` | 生成工程三份配置过严格校验与生产校验；`remote_entity` 新键取值与缺省一致 | CFG-5、CFG-12 |
| `TestSharedConfigRulesStayALeaf` 与边界例外 | `dependency_boundary_test.go:209`、`:257`、`:273` | `configdata/rules` 只依赖标准库；codegen 只允许 import 它 | CFG-7 |
| `TestOneTagDrivesTheGenerationCheckAndTheGeneratedLoader` | `codegen/internal/tablegen/rules_single_source_promises_test.go:17` | 一个标签同时驱动生成期检查与生成 loader | CFG-7 |
| tablegen / cfggen 运行期门 | `codegen/scripts/tablegen-runtime.sh`、`cfggen-golden-runtime.sh`（`.github/workflows/ci.yml:79`、`:81`） | 生成 loader 在真实 configdata 上执行规则 | CFG-6、7、10、11 |
| `TestFailedReloadAndRollbackAreCountedAndLogged`、`TestEveryReloadReportsOneOutcome` | `kit/configdata/reload_visibility_promises_test.go:56`、`configdata/field_rules_promises_test.go:151` | 每次一个 outcome、低基数标签 | CFG-8 |
| skill 变异性质测试 | `skill/compile_mutation_property_test.go:369`、`:413` | 编译 ⇒ 可执行；Program 变则 digest 变 | SKILL 全部 |
| `TestLowerRefusesEveryUnresolvedLookup` | `skill/lower_lookup_promises_test.go:105` | lower 不静默兜底 | SKILL-13 |
| `TestPhaseEventTableIsTheSingleSource` | `skill/phase_events_promises_test.go:11` | phase 事件单一来源 | SKILL-7、13 |
| 求值上下文表逐格守卫 | `skill/eval_contexts_table_test.go:259`、`:290`、`:333`、`:400`、`:504`、`:555` | 编译期与 Runtime 对每格一致；O33 格子有替代写法 | SKILL-15、16 |
| `stateMutationVerifyIncremental` | `skill/runtime_mutation.go:94`（测试打开） | 增量 mutation 与全量快照逐笔一致 | SKILL-1、3、5 |
| `internal/rangecontract.Check` | `internal/rangecontract/rangecontract.go:66`；套在 container / safemap / entity / 生成 DAO | 遍历回调契约 | NONCORE-46、47 |
| `TestNetworkCodegenTestsRunInSomeWorkflow` | `ci_generated_code_test.go:184` | `ROOST_NETWORK_TESTS=1` 的 job 存在且点名两条用例 | NONCORE-31 |
| glsvet 输入守卫 | `cmd/glsvet/inputs_promises_test.go` | 没检查到的输入退出 2 | NONCORE-51 |
| `go run ./cmd/glsvet -tests ./nest` | — | nest 测试不在 go 语句里取无作用域 Guard | NONCORE-54 |
| `TestDispatchLockingWithoutGuardScopeNeverReturnsCallerGuardToPool`、`TestDispatchLockingGroupRetryInScopeKeepsGuardUntilScopeEnds`、`TestDispatchEntriesRequireGuardScope` | `nest/dispatch_guard_scope_promises_test.go:50`、`:89`、`:129` | 派发取锁只用作用域的 Guard，Guard 每次进池恰好一次 | NONCORE-54 |
| `TestAGroupFitsOneLiveQuery` | `kit/service/global/activity/groups_live_limit_promises_test.go:15` | 活动组上限不超过一次 `App.Live` 查询的上限 | NONCORE-20 |
| 根包 `TestExamplesRun`、`TestTrackedMarkdownRelativeLinksResolve`、`TestNoMergeConflictMarkersInTrackedFiles` | 根包（TOOL 部分） | 示例实跑、文档链接、冲突标记 | SKILL-22；本分册自身 |

A3 的 `internal/stopcontract.Check` 骨架（套在 manager、kit/nest、syncbus、etcd、mirror、remoteentity、bus、生成 TCP 上）守着 NONCORE-4 / 6 / 52 的停止入口，属于 APP 部分。

## 按包的改动索引

| 包 / 目录 | 条目 |
| --- | --- |
| `app` | CFG-1、CFG-2、CFG-5、NONCORE-1、NONCORE-50 |
| `kit`（根）、`kit/mods` | CFG-1、CFG-2 |
| `kit/redis` | CFG-1、CFG-3、NONCORE-1 |
| `kit/saga` | CFG-1、CFG-2 |
| `kit/ops`、`kit/remoteentity`、`lifecycle` | NONCORE-1 |
| `kit/configdata` | CFG-8 |
| `kit/mongo` | NONCORE-49、NONCORE-53 |
| `kit/nest` | NONCORE-52 |
| `kit/statslog` | NONCORE-43 |
| `kit/service/account` | NONCORE-19、NONCORE-21、NONCORE-22 |
| `kit/service/global` | NONCORE-20（注释）、NONCORE-24 |
| `kit/service/global/activity` | NONCORE-18、NONCORE-19、NONCORE-20（C4 后的守卫）、NONCORE-21 |
| `kit/scripts/integration`、`scripts`、`cmd/glsvet` | NONCORE-51 |
| `configdata` | CFG-7、CFG-8、CFG-10 |
| `configdata/rules` | CFG-7、CFG-9、CFG-10 |
| `codegen/internal/tablegen` | CFG-6、CFG-7、CFG-10 |
| `codegen/internal/cfggen` | CFG-7、CFG-11 |
| `codegen/internal/roost` | CFG-4、CFG-12、CFG-13、NONCORE-4、NONCORE-5、NONCORE-26、NONCORE-27～31、NONCORE-51（NC-206） |
| `codegen/internal/servicerpc` | CFG-4、NONCORE-27 |
| `codegen/internal/dao`、`codegen/internal/entity` | NONCORE-10、NONCORE-32 |
| `codegen/internal/attribute`、`codegen/internal/errcode` | NONCORE-25、NONCORE-29 |
| `codegen/cmd/*` | NONCORE-31 |
| `skill` | SKILL-1、SKILL-5～8、SKILL-10、SKILL-11、SKILL-13～21 |
| `skill/combatcomponent` | SKILL-2（投影入口属 DAO） |
| `skill/skillsync` | SKILL-3、SKILL-4、SKILL-12、NONCORE-33 |
| `skill/skillcompose` | SKILL-9 |
| `skill/examples` | SKILL-22 |
| `httpserver`、`webroute`、`security`、`gateway` | NONCORE-2、NONCORE-3 |
| `bus` | NONCORE-6、NONCORE-8 |
| `etcd/driver` | NONCORE-7、NONCORE-52 |
| `dataengine/engine` | NONCORE-9 |
| `versionstore` | NONCORE-11 |
| `mongo/mongotest` | NONCORE-12 |
| `mongo/driver` | NONCORE-53 |
| `cache` | NONCORE-13 |
| `entity` | NONCORE-14、NONCORE-15、NONCORE-46、NONCORE-47 |
| `remoteentity` | NONCORE-13、NONCORE-15、NONCORE-52 |
| `saga`、`servicemetrics` | NONCORE-16～18、NONCORE-23（`saga` 用例另见 NONCORE-12） |
| `attribute` | NONCORE-25 |
| `demo/` 模板 | NONCORE-20、NONCORE-25、NONCORE-26、NONCORE-39～41、NONCORE-56 |
| `ai` | NONCORE-34、NONCORE-36、NONCORE-38 |
| `actionflow` | NONCORE-34～37 |
| `hotcode` | NONCORE-34、NONCORE-36 |
| `timer` | NONCORE-39、NONCORE-40 |
| `spatial`、`index` | NONCORE-39 |
| `failurelog` | NONCORE-42 |
| `robot/...`、`log`、`metrics` | NONCORE-43、NONCORE-44、NONCORE-45 |
| `container`、`safemap`、`goroutine`、`internal/rangecontract` | NONCORE-46～48 |
| `manager`、`sync/syncbus/driver`、`sync/syncbus/mirror` | NONCORE-52 |
| `nest` | NONCORE-54（A2 / A3 只改测试；RR-20261006-12 改派发取锁）、NONCORE-55 |
| `.github/workflows` | CFG-6（`ci.yml`）、NONCORE-31（`framework-compat.yml`） |
| 根包测试 | CFG-7（边界）、NONCORE-31（C9） |
