# N10 第二批：ai 节点逐个核对、actionflow 池化存储与 B7 留项、hotcode 真实插件

2026-10-06，分支 `revn10b`，基线 `81d7cb16`（origin/main，含 B7 `a9b7075b`）。本机 macOS arm64 / Go 1.27.0，`GOWORK=off`，不依赖外部服务。接力清单 [N10 行](REMAINING-REVIEW-HANDOFF-2026-10-05.md) · [第一批](REVIEW-2026-10-05-noncore-n10.md) · [B7 方案](../feature/REFACTOR-2026-10-06-actionflow-deferred-mutations.md) · [证据](../bugfix/evidence/noncore-bugfix-20261006-n10b/README.md)。NC 段 240～249，本批用 240～247。维护者已决定 ai / actionflow 保留在 core（B7）。

结论：登记并修复 8 条（NC-241～245 P2，NC-240 / 246 / 247 P3），均有修前红与修后绿，未发版。第一批留下的 O-A4、O-H1、O-H2、O-H3 在本批确认为缺陷并修复；真实 `.so` 插件第一次在本仓跑通，顺带发现 NC-244（接口变量导出的 PatchBundle 永远加载失败）。MissionRunner 延后语义、O-A1“清场”语义没有确认缺陷，列出选项待维护者。N10 **第二批完成**，本单元剩余只有外部环境项（Linux / Windows 插件）与待维护者的语义选择。

## 读代码的限制

图谱项目 `Users-whb-roost-roost-core`，generation 2026-09-30，三个包 scope coverage 无记录缺口；但 actionflow / ai / hotcode 在 09-30 之后有 `556d156d`（NC-120～123）与 `a9b7075b`（B7）两次提交，图谱落后，本批全部以当前源码直接阅读为准（三个包共约 3000 行，逐文件通读）。行号指基线 `81d7cb16`。

## ai：逐节点核对（`ai/nodes.go`、`ai/tree.go`、`behavior_strategy.go`、`controller.go`）

执行模型：Controller → BehaviorStrategy.Tick → root.Tick；动作结束经 Controller.OnActionEnd 缓冲到下一拍的 `BehaviorContext.actionEnds`，每拍未被消费的结束随上下文丢弃；有状态叶子只有 `TaskflowAction`（`launched`）、`Cooldown`（跨评估保留）、`TimeLimit`、`RandomSelector`、`Repeat`、`Parallel`、`Sequence` / `Selector` 游标。

| # | 节点 / 情形 | 依据 | 结果 |
| --- | --- | --- | --- |
| N1 | Sequence / Selector：Ready 视为 Running、nil 子节点跳过、完成即自 Reset、失败 / 成功传播 | 源码 `tree.go:43-122` + 既有用例 | 控制 |
| N2 | Inverter / Succeeder：Running / Ready 透传 | 源码 | 控制 |
| N3 | Parallel 结果已定后继续 Tick 后面的子节点 | 新增用例 | **NC-240** |
| N4 | Parallel 完成时打断仍在运行的子节点；已完成子节点不重复 Tick | 新增控制用例 + 既有用例 | 控制 |
| N5 | Repeat：Count≤0 无限、每拍至多一轮（不在一拍内紧循环）、失败穿透、完成清计数 | 源码 `nodes.go:104-137` | 控制；无限模式 `done` 只增不用，2^63 轮才溢出，不改 |
| N6 | UntilSuccess：失败后 Reset、每拍只试一次 | 源码 | 控制（设计如此，避免一拍内紧循环） |
| N7 | Guard：谓词转假时 Reset 子节点（触发 OnInterrupt）并失败 | 既有 `TestTaskflowActionInterruptHook` | 控制 |
| N8 | Cooldown：完成（成功或失败）才计时、Reset 保留计时 | 既有用例 + 源码注释 | 控制 |
| N9 | TimeLimit：期限先于子节点判断；注释写“more than Ticks”，代码是 `>=` | 探针 | 观察 O-N1，改正注释 |
| N10 | RandomSelector：roll 越界 / 选中 nil 直接失败、不 panic，运行中固定选择 | 源码 `nodes.go:309-343` | 控制 |
| N11 | 节点里调用 SetStrategy / Shutdown（树中途切换） | 新增用例 | **NC-241** |
| N12 | 冻结：Controller 冻结不 Tick、结束通知照常缓冲（NC-120）；树内无独立冻结 | 第一批 T2 | 控制（复用） |
| N13 | 节点 panic：Controller 恢复并报告，但这一拍已取走的结束通知随上下文丢失，未被 Tick 到的叶子会一直 Running | 源码 `behavior_strategy.go:91-96` | 观察 O-N2 |

## actionflow：池化存储、Update 与 B7 留项

| # | 情形 | 依据 | 结果 |
| --- | --- | --- | --- |
| P1 | `acquireContext` 池化：回调返回后 `releaseContext` 清零放回；下一次回调可能拿到同一个指针 | 探针：Start 里留下的指针返回后读到零值，下一次 Start 拿到的正是同一指针（`old ptr == new start ctx: true`） | 观察 O-A5（契约已写“不得保留”） |
| P2 | 框架自身是否会让两个回调同时持有上下文 | B7 起 apply 不嵌套，同一时刻至多一个上下文在用；Update 的 fn 不拿上下文 | 控制：框架内不会别名 |
| P3 | Update 的 fn panic | 新增用例 | **NC-242**（B7 引入） |
| P4 | Update 在回调里拿到的是尚未执行延后命令的当前动作 | B7 文档 | 控制（已写明） |
| P5 | 替换动作时旧动作 Cancel panic（O-A4） | 新增用例 | **NC-243** |
| P6 | ActionSnapshot 按值交给钩子；`deferred` 切片复用前逐条清零 | 源码 `action_runner.go:383-401` | 控制 |
| P7 | MissionRunner 未改延后语义 | 源码 `mission_runner.go:51-199` | 无确认缺陷，见“待维护者” |
| P8 | O-A1 EndAll 之后回调推进的任务启动新动作 | B7 用例 `TestEndAllEndsEverythingBeforeCallbackMutationsRun` | 语义选择，见“待维护者” |
| P9 | 替换旧动作时，旧动作的 OnEnded 里冻结了组，新动作仍在冻结组里启动 | 源码 `applyStart` 只在开头查冻结 | 观察 O-A6 |

## hotcode：真实 .so、回滚与可见性

本机用 `go build -buildmode=plugin` 现场构建最小插件（`hotcode/plugintest/testdata/bundle`），独立测试包 `hotcode/plugintest` 加载它：hotcode 自己的测试二进制带内部 `_test.go` 重新编译，与插件链接的 hotcode 不是同一版本，`plugin.Open` 会拒绝，所以必须放在独立包；race 运行时插件也带 `-race`。

| # | 情形 | 依据 | 结果 |
| --- | --- | --- | --- |
| H9 | 真实 .so 加载、Apply 生效、Bundle.Revert | 新增用例 | 接口变量导出方式失败 → **NC-244**；修后控制 |
| H10 | Apply 中途失败 / panic（O-H1） | 真实 .so + 进程内 panic 用例 | **NC-245** |
| H11 | `Patched` 用代码指针判断（O-H2） | 新增用例 | **NC-246** |
| H12 | `Resolve[T]` 类型不符静默回落（O-H3） | 新增用例 | **NC-247** |
| H13 | 旧请求生命周期 | 第一批 H2 | 复用，不重做 |

## 新观察（未改行为）

- **O-N1 TimeLimit 期限先于子节点判断**：BehaviorStrategy 把结束缓冲到下一拍，最后一拍窗口里结束（或冻结期间结束、时钟照走）的动作会被判超时并收到一次多余的 OnInterrupt。这是“期限优先”的常见取舍（与 BT.CPP Timeout 相同）；已把注释从 “more than Ticks” 改成与代码一致的 “Ticks or more”，并写明宽限做法。
- **O-N2 节点 panic 丢失这一拍的结束通知**：Controller 恢复 panic 继续运行，但 `actionEnds` 已从 pending 取走；panic 之后没被 Tick 到的 TaskflowAction 收不到自己的结束，会一直 Running。panic 是业务节点的编程错误，不改；若要更稳，可在 BehaviorStrategy.Tick 里 panic 时把未消费的结束放回 pending。
- **O-A5 ActionContext 池化**：每个 runner 一个 `sync.Pool`；契约写明回调返回后不得保留上下文，违约者读到的是零值或下一个回调的数据。B7 之后同一时刻至多一个上下文在用，runner 自带一个上下文字段就能做到零分配，没有必要每个实体一个 `sync.Pool`（大量实体时 GC 的 poolCleanup 要遍历它们）；属于简化建议，不改。
- **O-A6 替换途中被冻结**：直接 Start 替换当前动作时，旧动作 OnEnded 里调用的 Freeze（立即生效）不阻止新动作启动。Start 在冻结前已被接纳，不判为缺陷；若要“冻结立刻挡住一切启动”，在 `start` 前再查一次 `unit.frozen` 并按延后 Start 的方式发失败 OnEnded。

## 待维护者（语义选择，无确认缺陷）

1. **MissionRunner 是否改延后语义**（B7 留项）。现状是形态 (a)：任务启动 / 结束途中的 StartMission 返回 `ErrReentrantMutation`，EndCurMission 在结束途中静默忽略（本来就在结束）。逐项核对：拒绝都是显式错误，不留半截状态（U-0125 用例覆盖）；Tick / OnActionEnd 在 `starting` 期间照常送达；ClearActions → ClearMission 在 ActionRunner 回调里延后，按任务 ID 解释，不误伤新任务的动作。选项：(a) 保持；(b) 与 ActionRunner 一样延后，代价是 StartMission 的同步错误（ErrMissionRunning、Init 失败）改经 OnError、任务状态在回调返回前不变。**推荐 (a)**：没有使用方要求在任务钩子里链式启动任务，延后只换来便利。
2. **O-A1 EndAll 是否“清场”**。现状（B7 定义）：EndAll 先结束全部，回调发起的变更随后执行，所以 `OnFail=NextStep` 的任务会在 EndAll 之后启动下一步。选项：(a) 保持，并在 ActionList 接线说明里写清“要清场就先 EndCurMission 再 EndAllAction”（任务结束后 OnActionEnd 被忽略，不再推进）；(b) EndAll 执行期间回调发起的 Start / Enqueue 照常分配 ID，但执行时一律以取消状态发 OnEnded；(c) EndAll 顺带结束当前任务。**推荐 (a)**：(b) 会让“任务决定失败后去哪”在 EndAll 时被静默改写，(c) 把任务与动作两层的职责混在 runner 里；Controller 的 EndActions 接线由业务决定顺序。
3. 第一批的方向判断项中 “ai / actionflow 移出 core” 已由维护者否决（保留），本批不再提。

## 验证

见[证据 README](../bugfix/evidence/noncore-bugfix-20261006-n10b/README.md)：修前红 5 份（ai 3 条用例 5 处、actionflow 2 条 3 处、hotcode 2 条、真实插件 2 轮）；修后 gofmt 空、`go vet ./ai ./actionflow ./hotcode/...`（含 `GOOS=windows` / `linux`）、`go test -race -count=3 ./ai ./actionflow ./hotcode/...`（插件用例在 race 下实跑、覆盖率模式下 Skip）、`./nest`、全仓 `go build ./... && go vet ./...`、根包 `go test -count=1 .` 通过。没改三大模块，不跑 glsvet；没改生成形状，不重生成 game-demo（hotcode 只增字段与方法，demo 两处用法类型一致）。未跑：Linux / Windows 真实插件加载。

## 方向判断

- **actionflow**：本批两条（NC-242、NC-243）都在 B7 的延后机制上——一条是 B7 新增的 `executing` 状态在 panic 路径没复位，一条是 B7 的 ID 承诺在 panic 路径没兑现。这是 B7 之后的第一次补修，同属“用户回调 panic 时状态是否收敛”这一类，B7 判定集中在 submit 一处的方向没有被推翻，修复也是减少分支（applyStart 少一个提前返回）。建议之后 actionflow 的新回调点一律经 `recover…Panic` 包装，并把“任一回调 panic 后 runner 仍可用、交出的 ID 有结论”作为一条组合用例维护。
- **ai**：NC-241 是 actionflow 重入问题在 Controller 层的同构（回调里改变自身状态），按维护者 B7 的同一方向（延后）处理，不另开机制。NC-240 是单点疏漏。没有“修了又坏”的迹象。
- **hotcode**：NC-244 说明插件路径此前从未真实执行（U-0149 记“不可测”）；现在有了独立测试包，建议之后插件相关改动都在这里补用例。

## 停点

- **本批完成**：N1～N13、P1～P9、H9～H13；NC-240～247 修复，未发版。
- **N10 剩余**：外部环境——Linux / Windows 上的真实插件加载（Windows 不支持 Go 插件，只验证 stub 报错）；待维护者——上面两项语义选择，以及第一批的 O-T3 / O-T4。
