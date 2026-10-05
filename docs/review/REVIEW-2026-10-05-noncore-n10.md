# N10 第一批：ai / actionflow / featureflag / hotcode 的状态、取消、重入与运行期替换

2026-10-05，分支 `revn10`，基线 `197f7bb9`（origin/main）。本机 macOS / Go 1.27.0，`GOWORK=off`，不依赖外部服务。接力清单 [N10 行](REMAINING-REVIEW-HANDOFF-2026-10-05.md) · [跨轮进度](PROGRESS.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-n10/README.md)。NC 段 120～129，本批用 120～123。

结论：登记并修复 4 条（NC-120 / NC-121 P2，NC-122 / NC-123 P3），均有修前红与修后绿，未发版。feature flag 运行期变更复用 N07 第二批的真实进程热更结果（不重做），源码复核无新缺陷。观察 9 条不改行为，其中 2 处只补了已核实的中文注释。N10 **第一批完成、场景部分完成**，不计 completed/15。

## 读代码的限制

图谱项目 `Users-whb-roost-roost-core`（根是主检出，generation 2026-09-30），四个包自 09-30 起没有提交（`git log --since`），scope coverage 无记录缺口，所以图谱与当前源码一致；`getHandlerEntry` 的入边用 `trace_path` 定位（5 个调用方），调用点行号、模板（`.tmpl` 不索引）与 demo 用法用 `rg` 和直接阅读补证。行号指 `197f7bb9`。

## 使用面与所有权链

- **ai / actionflow 没有生产使用方**：`rg` 全仓 import，只有 ai → actionflow 自身；`docs/feature/GAME_DEMO_TEMPLATE.md:346` 也写明 game-demo 不用。actionflow 也没有 `ActionList` / `MissionManager` 的框架实现，接线（ActionRunner 的 OnEnded → MissionRunner.OnActionEnd → ai Controller.OnActionEnd，Mission 的 ClearActions → ActionRunner.ClearMission，Controller 的 EndActions → EndAllAction）全由业务按接口注释完成。本批按这些注释做了最小接线探针（[probes](../bugfix/evidence/noncore-bugfix-20261005-n10/probes.txt)），没有新增框架实现（没有真实使用方，不补抽象）。
- **执行模型**：三者都是“由持有实体的 Entity 锁串行调用、自身无锁”（`actionflow/action_types.go:1-3`、`action_runner.go:51-52`、`ai/controller.go:26-28`），因此本批不审跨 goroutine 并发，审回调内重入、取消 / 失败传播与生命周期事件是否成对。
- **featureflag**：写入方只有 game-demo `flags.go.tmpl:89`（config reload 的 AfterApply 整表 Replace）与 `gm.go.tmpl:474`（`gm.flag.set` 单条 Set）；读者只在入口读：`purchase.go.tmpl:51`、`activity.go.tmpl:268`、`spawner.go.tmpl:98`。
- **hotcode**：补丁点注册方是 Nest 全局 handler（`nest/handler.go:60`，生成代码 `codegen/internal/nest/gen.go:820/822` 走 `MustRegisterHandlerWithMeta`）与 game-demo `installPatchPoints`（`flags.go.tmpl:123-137`）；解析方是 Nest 派发（`nest/handler.go:98/118`）与 `rewards.LevelUpReward`；替换 / 回退入口是 admin 命令（`hotcode/admin.go`，game-demo 挂到 GM admin，`gm.go.tmpl:494`）与插件 Bundle.Apply。

## 场景矩阵

### actionflow（ActionRunner / MissionRunner / PlanMission）

| # | 情形 | 依据 | 结果 |
| --- | --- | --- | --- |
| A1 | 显式 Start 替换当前动作：旧的取消并 OnEnded，新的启动，队列保留 | 既有 `TestActionRunnerExplicitStartPrecedesQueue` | 控制 |
| A2 | 空闲组 Enqueue 立即启动；排队项启动失败继续队列且 OnEnded | 既有两条用例 | 控制 |
| A3 | Start / Tick / Cancel / transition 回调内重入 | 既有 U-0100 用例 | 控制 |
| A4 | 启动失败、Cancel 里重入启动别的动作（直接 / 排队两种） | 新增用例 | **NC-122** |
| A5 | ClearQueue / EndAll / ClearMission 丢弃排队项 | 新增用例 | **NC-121**；ClearMission 不动别的任务（控制） |
| A6 | 冻结组：不 Tick、不 Start，End 照常并发 OnEnded，Recover 续队列 | 源码 `:178-267` + 既有 `promises_test.go` | 控制 |
| M1 | 任务启动失败清状态；启动途中钩子结束任务报重入 | 既有用例 | 控制 |
| M2 | 两步计划推进、旧动作 ID 的结束不推进 | 探针 M3 | 控制 |
| M3 | 取消任务：ClearActions 取消当前动作，`ending` 下 OnActionEnd 被吞、不推进下一步 | 探针 M4 | 控制 |
| M4 | EndAll 时任务步骤 `OnFail=NextStep`：结束 Hook 里任务推进，EndAll 之后仍有动作在跑 | 探针 A8 | 观察 O-A1 |

### ai（Controller / BehaviorStrategy / 节点 / 解析）

| # | 情形 | 依据 | 结果 |
| --- | --- | --- | --- |
| T1 | 新策略 Init 失败保留旧策略、旧策略不 Stop、不 EndActions | 既有用例 | 控制 |
| T2 | 冻结期间动作 / 任务结束，Recover 后树能否完成 | 新增用例 | **NC-120** |
| T3 | 新策略 Init 里发起的动作被随后的 EndActions 一并结束、通知在切换中被丢 | 源码 `controller.go:57-74` | 观察 O-T3，补注释 |
| T4 | Shutdown 不调 EndActions；BehaviorStrategy 经 Stop → Reset → OnInterrupt 收尾 | 探针 T4 | 观察 O-T4 |
| T5 | Guard 关闭 / Parallel 失败 / TimeLimit 超时中断运行中的叶子 | 既有 Guard 用例 + 探针 T5 | 控制：都触发 OnInterrupt |
| T6 | TaskflowAction 注释称可用 SetMission 发起 | 源码：SetMission 不返回 ID，`OnMissionEnd`（`behavior_strategy.go:109`）为空，任务 ID 与动作 ID 两套计数 | 观察 O-T6，改正注释 |
| T7 | JSON 树严格解析、深度上限、guard 只收无状态谓词 | 既有用例 | 控制 |

### featureflag（运行期变更）

| # | 情形 | 依据 | 结果 |
| --- | --- | --- | --- |
| F1 | Set / Replace 在写锁内递增版本，读者不会看到新表配旧版本 | `featureflag/flags.go:39-69` + 既有 `TestReplaceVersionIsConsistentUnderConcurrentReads` | 控制 |
| F2 | config reload 成功 / 四种失败 / rollback / CSV+generate 后开关的真实进程行为 | **复用** [N07 第二批](REVIEW-2026-10-05-noncore-n07b.md) H0～H4、G1/G2 | 不重做；结论沿用 |
| F3 | 开关只在入口读（事务中间不读） | 三处读者源码 | 符合 `game/flags` 包注释 |
| F4 | `gm.flag.set` 与下一次 reload：后写者胜、reload 覆盖 GM 值 | `gm.go.tmpl:461-488`、`flags.go.tmpl:33-37` | 文档化取舍，控制 |
| F5 | `gm.flag.list` 先 Snapshot 再单独读 Version | `gm.go.tmpl:447-459` | 观察 O-F1 |

### hotcode（注册 / 替换 / 回退 / 旧请求）

| # | 情形 | 依据 | 结果 |
| --- | --- | --- | --- |
| H1 | Replace 之后的 Resolve 看到新函数（原子发布） | 既有 `TestRegistryReplaceResolveAndRevert` | 控制 |
| H2 | 旧请求生命周期：Nest 每次派发只解析一次 handler（`nest_dispatch.go:261/303/311/508`），已开始的请求跑完旧函数，下一次派发用新函数；广播整轮共用一次解析；冷目标转慢准备时 handler 尚未开始，重派发会解析到新版本 | 源码 | 符合“补丁在两次调用之间生效”；Go 插件不卸载，旧函数代码一直有效 |
| H3 | 并发 Replace / Revert 后的补丁点状态 | 新增有界对撞 | **NC-123** |
| H4 | `List` 与写者并发时读到半新半旧 | 同一根因 | 随 NC-123 修复（单次 Load） |
| H5 | 插件 Apply 中途失败：已 Replace 的点保留，LoadPlugin 报错且丢掉 Bundle | `plugin.go:49-51` | 观察 O-H1 |
| H6 | `Patched` 用代码指针比较 | `registry.go:175` | 观察 O-H2 |
| H7 | `Resolve[T]` 类型不符时静默回落到 fallback | `registry.go:79-86` | 观察 O-H3 |
| H8 | 实例级 handler（`mgr.RegisterHandlerWithMeta`）不是补丁点 | `nest/handler.go:122-151` | 观察 O-H4 |

## 新观察（未改行为）

- **O-A1 EndAll 不保证“全部结束”**：结束 Hook 里任务按 `OnFail=NextStep` 推进时会立刻启动下一步（探针 A8：EndAll 之后 `current=true`、仍在任务中）。这是“任务决定失败后去哪”的直接结果；若 EndAll 的语义应是“清场”，需要在 EndAll 期间屏蔽 Hook 内的重入启动，由维护者定。
- **O-A2 非排队 Start 失败**：发了进入 / 离开切换但不发 OnEnded（错误同步返回调用方），与排队路径不对称；依赖切换 Hook 计数的消费者会看到“进入又离开却没有结束”。
- **O-A3 Cancel 重入时同一动作被 Cancel 两次**：finish 与（NC-122 修复后的）启动失败分支都有：嵌套 Start 看到当前位仍是正在取消的动作，会再 finish 它一次。
- **O-A4 Start 被旧动作 Cancel 的 panic 打断**：`Start` 里替换旧动作时 finish 返回错误就整体返回，旧动作已结束、新动作却没启动，组空闲。
- **O-T3 新策略 Init 里发起的动作会被 EndActions 结束**（T3）：“Init 失败保留旧策略”的事务式顺序决定了 EndActions 只能在 Init 之后；已在 `SetStrategy` 上补注释说明应在第一次 Tick 发起。另一选择是先结束旧动作再 Init，代价是失去事务性。
- **O-T4 Shutdown 不调 EndActions**：BehaviorStrategy 配了 OnInterrupt 时仍会收尾（探针 T4）；自定义策略没有 Stop 收尾时，Shutdown 后动作继续跑、结束无人接收。
- **O-T6 TaskflowAction 不能等任务**：原注释说 Launch 可用 SetMission，实际等不到；已改正注释，建议用读 `MissionManager.InMission` 的 Condition。
- **O-F1 `gm.flag.list` 的 version 与 flags 可能不是同一代**：两次独立读取，期间有 reload 时会配错；只影响运维展示。要修可给 Store 加一次读出 `(flags, version)` 的方法。
- **O-H1 插件部分应用**：`Bundle.Apply` 中途失败时框架不回滚已替换的点，也丢掉 Bundle（admin 无法调用它的 Revert）；运维看到“加载失败”，`hotcode.list` 里却有点已被替换。`Revert` 总是回到原函数而不是上一代，所以框架层回滚应恢复 Apply 前的快照而不是调 `Bundle.Revert`。本仓不构建 `.so`，没有可跑的红，由维护者定是否做。
- **O-H2 `Patched` 判定**：比较的是函数代码指针，同一函数字面量 / 工厂产生、捕获值不同的闭包会被报成“未打补丁”。
- **O-H3 `Resolve[T]` 静默回落**：注册的是未命名函数类型、按命名类型解析（或反过来）时，补丁永远不生效且不报错；Nest 与 rewards 两处现有用法类型一致。
- **O-H4 实例级 handler 不能热补丁**：`hotcode.list` 只列全局注册的 `nest.handler.*`；GAME_DEMO_TEMPLATE 的“每个 Nest handler 都是补丁点”只对生成代码（全局注册）成立。

## 验证

见[证据 README](../bugfix/evidence/noncore-bugfix-20261005-n10/README.md)：修前红（7 条用例 / 子用例；hotcode 对撞 5 次普通 + 1 次 race 全红）；修后 gofmt / vet、`./ai ./actionflow ./hotcode ./featureflag` race×3、全仓 build / vet、根包、`./nest` race、全新生成 game-demo build / vet 与 `game/rewards`、`internal/service/game` 测试通过。未跑：Linux / Windows；真实 `.so` 插件加载；`glsvet`（没改三大模块）；ai / actionflow 没有生产调用方，只有包内组合与最小接线探针。

## 方向判断

- **actionflow**：本仓此前对它只做过钉测（U-0077 / U-0100 / U-0125，没有行为修复），本批是第一次行为修复，谈不上“修了又坏”。但 NC-122 与 U-0100 是同一机制的另一个分支：runner 允许任意回调重入，再在每一步之后用 `unit.cur != entry` 事后比对；每多一个回调点就要多一处判定，漏一处就出孤儿。NC-121 则是“入队 / 结束事件成对”这个不变量在三处丢弃路径都没守。建议维护者在出现真实使用方之前定方向：(a) 结构上禁止重入——像 MissionRunner 的 `starting/ending`、Controller 的 `switching` 一样，回调期间的变更直接返回 `ErrReentrantMutation`（代价：Hook 里推进任务这种合法用法要改成延后执行）；(b) 回调里的变更进一个延后命令队列，回调返回后按序执行（代价：多一层状态，但判定集中在一处）。在没有使用方的前提下，也可以考虑把 ai / actionflow 移出 core 主干，等真实游戏接入时按其需求重做。
- **ai / featureflag / hotcode**：没有反复出问题的迹象。NC-120 是两个判断合并成一个造成的单点问题，修复减少了耦合；NC-123 的修复把三份状态减成一份。

## 停点与下一步

- **本批完成**：A1～A6、M1～M4、T1～T7、F1～F5（F2 复用 N07b）、H1～H8；NC-120～123 修复。
- **未做（第二批入口）**：ai `nodes.go` 剩余组合（Repeat 无限计数与 UntilSuccess 每 Tick 只试一次、RandomSelector 越界 roll）的逐节点红线；actionflow `UpdateAction`、`Context` 钩子复用存储（`acquireContext` 池化）在回调保存指针时的行为；hotcode 插件真实 `.so` 加载（需要同工具链构建插件）与 O-H1 的回滚方案；featureflag 无剩余项。
- **待维护者**：方向判断 (a)/(b)/移出 core；O-A1、O-T3、O-T4、O-H1 的语义选择。
