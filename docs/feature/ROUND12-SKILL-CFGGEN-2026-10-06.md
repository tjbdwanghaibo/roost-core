# 第十二轮决定：skill 剩余观察、buff 投影、cfggen globals 规则

2026-10-06，分支 `bsk`，基线 `78e26853`。来由：[DECISIONS-PENDING-2026-10-05](../review/DECISIONS-PENDING-2026-10-05.md) 末尾“第十二轮”表的三行（均按推荐）：

| 决定 | 内容 |
| --- | --- |
| skill 剩余观察 | O22 编译期拒绝；O7 排序；O29 改文案；O15 / O16 / O17 / O27 / O28 保持并写作者文档；其余保持 |
| buff 投影（N09 O2） | 组件给投影入口，投影交业务 |
| cfggen globals 规则 | 支持 required / min / enum，与 tablegen 统一 |

观察原文：[n09 第一批](../review/REVIEW-2026-10-05-n09-batch1.md) O2、[第二批](../review/REVIEW-2026-10-05-n09-batch2.md) O7、[第三批](../review/REVIEW-2026-10-05-n09-batch3.md) O15～O17、[第四批](../review/REVIEW-2026-10-05-n09-batch4.md) O22、[第五批](../review/REVIEW-2026-10-06-n09-batch5.md) O27～O29。

代码定位：codebase-memory 共享 generation 停在 09-30、根路径是主检出，本记录的结论以 worktree 当前源码（`rg` + 直接读）为准。

## 1. 方案

### 1.1 O22：summon 进程的 `duration_ticks`

`validateProcessMotion`（`skill/compile_motion.go`）对 summon 提前返回，进程自己的 `duration_ticks`、`area`、`interval_ticks`、`emit_leave_on_stop` 都不检查。运行期 `startEntityProcess`（`process_owned.go`）只在带 motion / area 的进程上读模板时长，summon 的寿命是 spawn 效果的 `duration_ticks`（编译期已要求为正）。核对结果：`duration_ticks` 确实从不被读，负数也照样编译；另外 summon 上写 `area` 时运行期反而读模板时长 0，启动即 `ErrProgramInvariant`（同一处提前返回造成）。

做法：summon 上写了非 0 的 `duration_ticks` 报 `MOTION_INVALID`（“remove the process duration_ticks”）；`area` / `interval_ticks` / `emit_leave_on_stop` 按非 area 进程同样的规则拒绝。不写时行为不变。

### 1.2 O7：checkpoint 字节确定

`checkpointPayloadLocked`（`skill/runtime_checkpoint.go`）的 `ActivePolicies`、`ProcLedger`、`RootEventCounts`、`AbilityByProgram` 四个列表按 map 迭代顺序写出，其余列表早已排序。做法：四个列表按键排序（`(caster, skill)`；proc 账本 `(root, caster, digest)`；root 事件按 ID）。

兼容：**改变 checkpoint 字节、不改变格式**。版本号仍是 2，字段与类型不变；恢复本来就与列表顺序无关（`rootEventOrder` 恢复时按 ID 排序，其余三个按键装 map），所以旧版本写出的乱序 checkpoint 照常可读，恢复后再 Checkpoint 得到排序后的字节。新版本写出的 checkpoint 旧版本同样可读。依赖“同一状态两次 checkpoint 字节相同”的比对从本版起成立，不能拿新旧版本各自写出的字节互相比对。

### 1.3 O29：result 分支的诊断文案（只改文案）

规则 `effectResultBranchMaySuspend`：不能有 `wait`、带间隔的 `repeat`、带 `on` 的效果；spawn 加不带回调的进程照常编译、执行时照常启动进程。文案 “effect result branches cannot suspend or start a process” 改为 “… cannot suspend (wait, repeat with interval_ticks) or start a process with on callbacks”。同一谓词的 status 实例消费流程文案（“cannot suspend or create a process”）有同样的不符，一并改成同一说法。规则不变。

### 1.4 O15 / O16 / O17 / O27 / O28：保持，写作者文档

写进 [`docs/skill/skill-casting-and-combat.md`](../skill/skill-casting-and-combat.md)“进程、运动与 temporal 的既定语义”：area 的 `$event.enter_count` 恒为 1；`max_reflects: N` 是碰撞预算，实际反弹 N−1 次（pierce 同理）；Host 拿到的 numeric 快照里未绑定字段是启动时的值、运动每步重新求值；restore 的 `on_blocked` 与 profile 策略不同时必然 `policy_rejected`；process_start 读取的实体按启动事件求值。O22 / O29 写进同文“编译期收紧与诊断文案”。其余 N09 观察保持现状。

### 1.5 buff 投影（N09 O2）

问题：伤害管线 `ResolveDamage` 读 `Combatant` 的平铺字段（Armor 等），buff 与属性修饰只改 `CombatDao.attributes`；组件没有把属性写到 Combatant 的入口（`attributes` 不导出，`applyState` 还会换掉 AttributeSet 实例，`Observe` 挂不住），经 `StatusBridge` / `ApplyBuff` 加的护甲对伤害没有任何影响。

入口（`skill/combatcomponent`）：

```go
type AttributeProjection func(attribute func(combat.AttributeID) int64, combatant *combat.Combatant)
func (component *CombatComponent) ProjectAttributes(projection AttributeProjection)
```

业务只写一个纯函数（哪个属性写到哪个字段），构造组件后 `ProjectAttributes` 一次。组件里唯一的 derive `deriveProjection`：

- 触发点（A1 §3）：装上时；`OnInitFinish`（新建与从存储加载共用，存储里的 vitals 是上次提交时的投影，投影函数可能已变）；每个改属性来源的 mutator 末尾（`InitCombatant`、`SetAttributeBase` / `SetAttributeBounds`、`ApplyBuff`、`RemoveBuff`、`SetBuffStacks`、`AdoptBuff`、`DispelBuffs`、`TickBuffs`；`StatusBridge` 与资源命令经这些 mutator 落地）。`SetBuffDueTick` 不改属性，不投影。
- 回滚：投影写在 DAO 的 vitals（`FieldVitals`）上，事务里经 `beginChange` / `markChanged`，与源字段同一笔逆操作 / 快照；回滚不是触发点，DAO 恢复到事务开始时的值。加载与构造时不在事务里，直接写内存、不登记逆操作、不标脏。结果与现值相同（`reflect.DeepEqual`）时什么都不做，不产生持久写。
- 投影函数是代码不是状态，组件持有它不违反“组件不持有需要回滚的内存状态”。

未采用：

- **只在伤害时算一份投影视图、不存**：伤害管线会改 target / source 的 Health、Shield、SpellShield，要逐字段写回，`HostAdapter` 的 shield 事件等其他读 `Combatant()` 的地方看到的仍是未投影值；A1 已规定派生值是 DAO 字段。
- **组件内置属性 → 字段映射表**：哪个属性是护甲是游戏设计，决定是“投影交业务”。
- **构造参数 `NewCombatComponent(dao, opts...)`**：同样一行，但实体工厂之外（测试、示例）也要能后装；方法更直接。

示例：`skill/examples/statusbridge` 改为 `ProjectAttributes(projectCombat)`（删掉手写的 `syncArmor`），并把整段演示包进 `nest.RunDetachedTransaction`——A1 之后战斗组件的改动必须在事务里，这个示例在 main 上运行即 panic（`persistence mutation outside transaction`），`go build` / `go vet` 看不出来。

### 1.6 cfggen globals 的 required / min / enum

B10 时 cfggen 对 globals 上的规则直接报错（“singleton configs do not support … yet”，[B10 方案](B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md) §2.4 列为后续）。运行时早已支持：`configdata.ObjectDef.Rules` 在每次 Load / Reload 用 `rules.CheckObject` 检查（tablegen 的 object 已在用）。

做法（`codegen/internal/cfggen/main.go`）：

- 允许 globals 上的 `required` / `min` / `enum`；`unique`（单个对象没有可比的行）、`ref`、`index` 仍拒绝。
- `fieldRule(field)` 是字段选项到 `configdata/rules.Rule` 的唯一翻译：生成期用同一个 `Rule.Validate` 检查规则声明（tables 与 globals 都过），globals 的 `ObjectDef.Rules` 字面量从它写出。生成期与运行时是同一份规则、同一段检查代码。
- 生成形状：有规则的 global 注册写成 `configdata.ObjectDef[WorldCfg]{Name: "world", File: "world.json", Rules: []configdata.FieldRule{{Field: "width", Required: true, Min: "1"}, …}}`；没有规则的 global 与 tables 的输出逐字不变。global 的 struct 不再带规则 `cfg` 标签（对象注册路径不读标签，写了是不生效的第二份）。
- 业务不写任何代码：meta 里写规则，重新生成即可。

## 2. 实施与红绿

全部 `GOWORK=off`。

| 项 | 回归 | 修前（红） | 修后 |
| --- | --- | --- | --- |
| O22 | `skill/compile_summon_process_promises_test.go` | `TestSummonProcessRejectsFieldsItNeverReads` 五个子用例（正 / 负 duration、area、interval、emit_leave_on_stop）全部 `missing diagnostic MOTION_INVALID in []skill.Diagnostic(nil)` | 通过；`…WithoutDurationCompilesAndLivesForTheSpawnDuration` 钉住不写时寿命 = spawn 的 10 tick |
| O7 | `skill/checkpoint_deterministic_bytes_promises_test.go` | `TestRuntimeCheckpointBytesAreDeterministic`：`checkpoint 2 of the same state differs`；`TestRuntimeRestoresUnsortedLegacyCheckpointLists`：恢复乱序 checkpoint 后再 Checkpoint 的字节与排序版不同 | 通过：20 次 Checkpoint 字节与 Checksum 相同、四个列表有序；倒序的旧样子照常恢复，再 Checkpoint 与排序版逐字节相同 |
| O29 | `skill/effect_result_branch_process_promises_test.go` | `result branch diagnostic "effect result branches cannot suspend or start a process" must name process on callbacks` | 通过；`TestEffectResultBranchMayStartAProcessWithoutCallbacks` 钉住分支里 spawn + 无回调进程照常启动（修前修后都通过，是行为钉子） |
| O2 | `skill/combatcomponent/attribute_projection_promises_test.go` | 旧 API 下同一场景（+100 护甲 buff 前后各打 100 物理伤害）：`health damage without / with the +100 armor buff = 100 / 100, want 100 / 50` | 装投影后 100 / 50，移除 buff 后回到 100；两种回滚策略 × handler 失败 / 提交被拒，投影的护甲随 DAO 回到 20、字节一致、无残留脏位；加载后 `OnInitFinish` 投影出存储属性 50、不标脏 |
| cfggen | `codegen/internal/cfggen/global_rules_promises_test.go`；`testdata/runtime`（cfg.yaml 的 world 加 `width` required+min、`height` min、`mode` enum，roundtrip 新增 `TestGlobalRulesAreEnforcedOnLoadAndReload`） | 单测：`global world field width: singleton configs do not support required / unique / min / enum yet`；`cfggen-golden-runtime.sh`：生成步骤同样报错退出；去掉规则后（修前唯一能生成的写法）`load of world {"width":1024,"height":768,"mode":"pvz"}: err = <nil>, want "field mode: enum"` | 单测通过（Rules 字面量、无规则 global 输出不变、unique / min 非数值 / enum 重复 / bean 上 enum 仍拒绝）；`ROOST_CORE_DIR=<worktree> cfggen-golden-runtime.sh -race` 通过：缺 width、width 0、height 0、mode 拼错四种在启动 Load 与 Reload 都被拒，reload 不动现行快照 |

O2 的回滚用例做过变异检查：把 `deriveProjection` 里的 `beginChange(FieldVitals)` 去掉，undo 策略的两个子用例失败，state 策略通过（vitals 本来就在 DAO 快照里）。

## 3. 验证

- `gofmt -l` 空；`go build ./... && go vet ./...` 通过。
- `go test -race -count=3 ./skill/... ./codegen/internal/cfggen/ ./configdata/...` 通过（含 skill 的性质测试 `compile_mutation_property_test` 与 race 构建下的 `compile_mutation_race_test`）。
- `go test ./skill -run '^$' -fuzz FuzzRestoreRuntimeCheckpointNeverPanics -fuzztime 20s` 通过。
- `skill/examples`：`go build` / `go vet` / `go test` 通过，`combat`、`fireball`、`statusbridge` 都运行退出 0（statusbridge 修前 panic）。`skill/integration/sync-e2e`：`go vet` / `go test` 通过。
- `go test -count=1 ./codegen/...` 通过；`go generate ./...` 前后 porcelain 相同；根包 `go test -count=1 .` 通过（codegen 引用 `configdata/rules` 属于根包边界测试允许的例外）。
- 生成形状只在 global 带规则时变化；game-demo 模板不用 cfggen，无需重新生成。没改 nest / entity / dataengine / sync，未跑 glsvet。

## 4. 兼容影响

- **O22 收紧**：已有定义在 summon 进程上写了 `duration_ticks`（或 area 成员字段）的，升级后编译失败（`MOTION_INVALID`）；删掉这几个字段，行为不变（area 那种本来运行即失败）。
- **O7**：checkpoint 字节变化、格式不变，双向可读。
- **O29**：只改诊断文案；按文案字符串匹配的工具需要更新。
- **O2**：只新增 API；不装投影的业务行为不变。装了投影后，被投影的字段以投影为准，`InitCombatant` 里给的这些字段值会被覆盖。
- **cfggen**：只放开以前报错的写法；已有 meta 的输出逐字不变。

## 5. 实施状态

已实施，未发版。无未完成项。
