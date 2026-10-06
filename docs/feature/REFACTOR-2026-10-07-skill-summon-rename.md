# 重构：skill 生成宿主实体改名为召唤物（Summon），衍生物 `kind: "summon"` 改为 `kind: "minion"`

日期 2026-10-07，分支 `summon`，基线 main `8bde60ac`。重构登记，不占 RR。未发版。

## 1. 维护者决定

[DECISIONS-PENDING 第十三轮](../review/DECISIONS-PENDING-2026-10-05.md)“skill 生成宿主实体改名”：维护者“第 1 类改 Summon”，衍生物 `kind: "summon"` 改为 `kind: "minion"`（“按照推荐处理”）。线上没有部署，**不保留旧名、不做兼容别名**。

上一次改名（`4451a0a5`，[记录](REFACTOR-2026-10-06-skill-process-to-spawn.md)）把“进程”（process）改成衍生物（Spawn）之后，skill 包里有两套 spawn：

1. **召唤物（Summon，第 1 类，本次改名）**：效果 `{"type":"spawn","template":…}` 用单位模板在场景里生成真实单位（陷阱、宠物、图腾、墙等，归施法者所有），宿主侧走 `PreviewOwnedSpawn` / `CommitOwnedSpawn` / `RollbackOwnedSpawn` 事务，单位本身由宿主管理，技能之后用 `issue_entity_command` / `despawn` 指挥或移除它。
2. **衍生物（Spawn，第 2 类，保持）**：技能施放时生成、之后每个 tick 由技能驱动的东西（飞行物、法术场 / 光环、光束、位移、环绕，以及驱动召唤物移动的那种），写在 effect flow 的 `"spawn": {…}` 上，Host 接口 `StepSpawn` / `StopSpawn`，`$spawn`、`spawn_step`、`modify_spawn`、`spawn_upsert` / `spawn_remove` 等。

两者的关系：召唤效果可以带一个衍生物定义（`{"flow":"effect","effect":{"type":"summon",…},"spawn":{"kind":"area",…},"on":{…}}`），这个衍生物以召唤出的单位为 lifecycle 实体运行；衍生物里有一种 kind 本来叫 `summon`（只跟着召唤物活、不做运动和成员检测），本次改为 `minion`，避免与召唤效果同名。

## 2. 目标与改动面

- 目标：名字。行为不变——测试除改名外不改断言；格式变化是 checkpoint 版本号 4 → 5（§3.4），以及随名字变化的 digest（§3.5）。
- 改动面：`skill/`（含 skillcompose、testdata）的 Go / JSON / Markdown，`docs/skill/*`，CHANGELOG 一条，TROUBLESHOOTING 一行，交接 §7 一行，DECISIONS-PENDING 实施状态。
- 不需要改的目录（`rg` 核对）：codegen、demo 模板（唯一的技能 `fireball.json.tmpl` 只有 `finish`，不带召唤效果；`demo/` 里的 `spawn` 是场景刷怪，与 skill 无关）、kit、examples、`skill/examples`、`skill/integration/sync-e2e`、`skill/combatcomponent`（只有两处注释，见 §4.2）都不引用这些名字。skillsync 只有衍生物的名字，不改。
- 结构：包与目录不变；2 个文件改名（§3.7）。

## 3. 新旧名对照

### 3.1 判定方法

逐个名字判定，依据按优先级：

1. **来源**：`4451a0a5` 之前（基线 `4451a0a5^`）skill 包里已经叫 spawn 的名字，全部是第 1 类的候选（那时衍生物还叫 process）；`4451a0a5` 由 process 改名而来的名字（上一份记录 §3.6 / §3.7 逐个列出）与 `3fad5b6e` 为衍生物停止新增的名字，属于第 2 类。
2. **语义**：候选再看它实际管的是什么——单位模板、宿主事务、单位的生成时刻 / 序号、召唤效果的结果与求值，是第 1 类；以衍生物（`SpawnInstance`、`SpawnID`、`ownedSpawns` 表）为对象的，是第 2 类。来源与语义冲突时以语义为准（§3.3 列出全部这类个案）。
3. 基线 `4451a0a5^` 的 `skill/` Go 文件里含 spawn 的词元 53 个，全部在 §3.2 / §3.3 判定；改名前（基线 `8bde60ac`）比它多出的 351 个里，只有 `despawnLifecycle`（`3fad5b6e` 新增的测试辅助，移除召唤物）属于第 1 类，另有 3 个测试名一半属于第 1 类（§3.2 末），其余 347 个属于第 2 类（§3.6）。

### 3.2 第 1 类：召唤物（改）

**DSL（JSON）**

| 旧 | 新 | 说明 |
| --- | --- | --- |
| 效果 `{"type":"spawn","template":…,"position":…,"count":…,"duration_ticks":…,"attribute_overrides":…,"parameter_bindings":…}` | `{"type":"summon",…}`（字段不变） | 生成宿主单位 |
| 效果 `{"type":"despawn","target":…}` | `{"type":"dismiss","target":…}` | 移除召唤物（见 §3.3 “despawn”） |
| `issue_entity_command` 的 `"command":"despawn"`；单位模板 `commands` 里的 `despawn` | `"dismiss"` | 召唤物指令 |
| 单位模板（`UnitTemplateCatalogEntry`）生命周期策略 `OwnerDeathPolicy` / `SkillRemovedPolicy` / `MatchEndPolicy` 的值 `"despawn"` | `"dismiss"` | 编译环境 Go 字段，值改名；另一个值 `persist_until_duration` 不变 |
| owned 选择过滤器 `{"type":"spawned_before","tick":…}` / `spawned_after` | `summoned_before` / `summoned_after` | |
| owned 选择排序 `"by":"spawn_tick"` / `"spawn_sequence"` | `summon_tick` / `summon_sequence` | |
| effect result 类型 `spawn_result` | `summon_result` | 进 checkpoint（§3.4） |
| 操作名（`Inspect` 的 `OperationView.Kind`、gameplay digest 的 `kind`、skillcompose `Operations`）`spawn` | `summon` | skillcompose 因果图 sink 判断同步 |
| Visual 类别 `summon` 允许的效果 `["spawn"]` | `["summon"]` | 类别名本来就叫 summon，不变 |

**诊断文案**

| 旧 | 新 |
| --- | --- |
| `spawn count exceeds the unit template or environment maximum` | `summon count exceeds …` |
| `spawn lifetime exceeds the unit template or environment maximum` | `summon lifetime exceeds …` |
| `spawn tick filter requires a non-negative tick` | `summon tick filter requires a non-negative tick` |
| `only a spawn effect starts a spawn` | `only a summon effect starts a spawn` |
| `only a spawn effect has spawn callbacks` | `only a summon effect has spawn callbacks` |
| `summon spawns do not support motion` | `minion spawns do not support motion`（§3.2 末“kind”） |
| `summon spawns live for the spawn effect's duration_ticks; remove the spawn's own duration_ticks` | `minion spawns live for the summon effect's duration_ticks; remove the spawn's own duration_ticks` |
| Runtime 错误 `spawn success requires a stable non-empty entity result` | `summon success requires …` |
| 求值上下文说明（`ErrReferenceOutOfContext` 文案）“spawn 的位置 / 属性覆盖 / 参数”“spawn 出的 lifecycle 实体”“施法流程里求一次的 spawn position” | “召唤效果的位置 / 属性覆盖 / 参数”“召唤出的 lifecycle 实体”“施法流程里求一次的召唤 position” |

**MemoryHost 事件**（参考宿主的事件词表，`MemoryHost.Events`）

| 旧 | 新 |
| --- | --- |
| `owned_entity_spawned` | `owned_entity_summoned` |
| `owned_entity_spawn_rolled_back` | `owned_entity_summon_rolled_back` |
| `owned_entity_despawned` | `owned_entity_dismissed` |

**Host 契约与导出 Go 标识符**

| 旧 | 新 | 所在 |
| --- | --- | --- |
| `SpawnCommand` | `SummonCommand` | `host_command.go`，召唤效果的宿主命令 |
| `SpawnAttributeOverride` / `SpawnParameterBinding` | `SummonAttributeOverride` / `SummonParameterBinding` | `SummonCommand` 的字段类型 |
| `SpawnEffectResult` | `SummonEffectResult` | 召唤效果的宿主结果 |
| `SpawnEffectDefinition` | `SummonEffectDefinition` | wire |
| `DespawnEffectDefinition` | `DismissEffectDefinition` | wire |
| `OwnedEntityRuntimeHost.PreviewOwnedSpawn(SpawnCommand) (OwnedSpawnPreview, error)` | `PreviewOwnedSummon(SummonCommand) (OwnedSummonPreview, error)` | `host_owned_entity.go`，Host 契约 |
| `CommitOwnedSpawn` / `RollbackOwnedSpawn(OwnedSpawnTransactionID)` | `CommitOwnedSummon` / `RollbackOwnedSummon(OwnedSummonTransactionID)` | 同上 |
| `OwnedSpawnPreview` / `OwnedSpawnTransactionID` | `OwnedSummonPreview` / `OwnedSummonTransactionID` | 同上 |
| `OwnedSpawnTickFilter` | `OwnedSummonTickFilter` | owned 选择过滤器（宿主 Select） |
| `OwnedSpawnTickFilterDefinition` | `OwnedSummonTickFilterDefinition` | wire |
| `OwnedEntityMetadata.SpawnTick` / `SpawnSequence` | `SummonTick` / `SummonSequence` | 单位生成时刻 / 序号 |
| `SelectOrderSpawnTick` / `SelectOrderSpawnSequence` | `SelectOrderSummonTick` / `SelectOrderSummonSequence` | 值 `summon_tick` / `summon_sequence` |
| `UnitTemplateCatalogEntry.MaximumSpawnCount` | `MaximumSummonCount` | 单次召唤数量上限 |

**未导出标识符与测试名**

| 旧 | 新 |
| --- | --- |
| `spawnEffectIR`、`spawnAttributeOverrideIR`、`spawnParameterBindingIR` | `summonEffectIR`、`summonAttributeOverrideIR`、`summonParameterBindingIR` |
| `spawnOperation`、`spawnAttributeOverrideProgram`、`spawnParameterBindingProgram` | `summonOperation`、`summonAttributeOverrideProgram`、`summonParameterBindingProgram` |
| `ownedSpawnTickFilterIR` | `ownedSummonTickFilterIR` |
| `ownedSpawnTransaction`（MemoryHost） | `ownedSummonTransaction` |
| `executeOwnedSpawn` | `executeOwnedSummon` |
| `resultTypeSpawn` | `resultTypeSummon` |
| `spawnOwnedLocked`、`resolveSpawnBindings`（MemoryHost） | `summonOwnedLocked`、`resolveSummonBindings` |
| `validateSpawnBindings`（编译器） | `validateSummonBindings` |
| 局部变量 `spawn`（`typed.effect.(*spawnEffectIR)` 的断言结果，`compile_budget.go` / `compile_lifetime.go` / `compile_shape.go`）、`spawned`（`compile_budget.go`） | `summon`、`summoned` |
| 测试辅助 `ownedSpawnCommand`、`spawnOwnedForTest`、`spawnWithCallbacks`、`despawnLifecycle` | `ownedSummonCommand`、`summonOwnedForTest`、`summonWithCallbacks`、`dismissLifecycle` |
| `TestOwnedEntitySpawnBindingsAreTypedClampedAndCapabilityChecked` | `TestOwnedEntitySummonBindingsAreTypedClampedAndCapabilityChecked` |
| `TestOwnedEntitySpawnRegistersAuthoritativeIdentityAndTypedResult` | `TestOwnedEntitySummonRegistersAuthoritativeIdentityAndTypedResult` |
| `TestOwnedReplacementDistanceTieUsesSpawnSequenceThenEntityID` | `TestOwnedReplacementDistanceTieUsesSummonSequenceThenEntityID` |
| `TestCompileRejectsSpawnsOnEffectsThatDoNotSpawn` | `TestCompileRejectsSpawnsOnEffectsThatDoNotSummon`（前一个 Spawns 是衍生物） |
| `TestSummonSpawnWithoutDurationCompilesAndLivesForTheSpawnDuration` | `TestMinionSpawnWithoutDurationCompilesAndLivesForTheSummonDuration`（kind 改 minion；末尾是召唤效果的时长） |
| `TestSummonSpawnRejectsFieldsItNeverReads` | `TestMinionSpawnRejectsFieldsItNeverReads` |

**衍生物 kind（第 2 类里唯一改名的值）**

| 旧 | 新 |
| --- | --- |
| `"spawn":{"kind":"summon"}` | `"spawn":{"kind":"minion"}` |
| 编译器 `spawn.kind == "summon"`、`validMotionSpawnKind` 的 `summon`、运动能力表校验 `pair.Spawn == "summon"`（minion 不能出现在 `motion.spawn_trajectory_pairs`） | `minion` |
| 中文“summon 衍生物” | “minion（随从）衍生物” |

### 3.3 逐个判定的个案（来源与语义可能读混的名字）

| 名字 | 判定 | 依据 |
| --- | --- | --- |
| `SpawnCommandMeta` | 衍生物，不改 | 由 `ProcessCommandMeta` 改名而来；是 `SpawnStepCommand` / `SpawnStopCommand` 的元数据（`SpawnID`、`Tick`），不是 `SpawnCommand` 的 |
| `OwnedSpawns`（`Runtime.OwnedSpawns(owner)`、`CompileLimits.MaxOwnedSpawns`、`computed.OwnedSpawns`）、`ownedSpawns`（Runtime 字段）、`owned_spawns`（checkpoint） | 衍生物，不改 | 由 `OwnedProcesses` / `ownedProcesses` 改名而来；是移交给实体（`handedOff`）的衍生物表，元素是 `*SpawnInstance`。宿主单位的数量另有 `OwnedEntities` / `MaxOwnedEntities` |
| `OwnedSpawnSnapshot` | 衍生物，不改 | 任务说明把它列在改名清单里，核对源码后不改：由 `OwnedProcessSnapshot` 改名而来，字段是 `ID SpawnID`、`LifecycleEntity`、`StartTick` / `EndTick`、`Status SpawnStatus`、`HandedOff`，是 `Runtime.OwnedSpawns` 返回的衍生物快照，不是单位快照（单位元数据是 `OwnedEntityMetadata`） |
| `MaxOwnedSpawns*`（`RuntimeOptions`）、`hasOwnedSpawnCapacityExcluding`、`previewOwnedSpawnCapacity` | 衍生物，不改 | 由 `*Process*` 改名而来；数的是 `SpawnScopeEntity` 的衍生物。`previewOwnedSpawnCapacity` 在召唤前调用宿主 `PreviewOwnedSummon` 得到会被替换的单位，再算衍生物容量够不够，名字里的 Spawn 指衍生物容量 |
| `hasSpawn` / `HasSpawn`（`summonOperation.hasSpawn`、表现事件 `HasSpawn`） | 衍生物，不改 | 由 `hasProcess` 改名而来：召唤效果上是否挂了衍生物定义 |
| `SpawnTemplate` / `SpawnTemplateIndex` / `spawnTemplate(s)` | 衍生物，不改 | 由 `ProcessTemplate*` 改名而来：衍生物定义的编译产物。召唤用的单位模板是 `UnitTemplate*` |
| `startEntitySpawn`、`handoffEntitySpawns`、`reapUnhandedEntitySpawns` | 衍生物，不改 | 由 `*EntityProcess*` 改名而来：以召唤出的单位为 lifecycle 实体启动 / 移交 / 回收衍生物 |
| `captureSpawn`（测试辅助） | 衍生物，不改 | 基线前已存在，但它返回的是“召唤效果 + area 衍生物（tick 回调是参数）”，名字用于构造衍生物回调的读取点；改名后按衍生物读正确 |
| `evalContextAreaSpawn`（测试辅助） | 衍生物，不改 | 基线前已存在；注释写明“是一个 area 衍生物” |
| `TestCompileRejectsSpawnsOnEffectsThatDoNotSpawn` | 两者都有，只改后半 | “Spawns” 是衍生物定义，“DoNotSpawn” 是“不是召唤效果” |
| `despawn`（效果、指令、生命周期策略、MemoryHost 事件） | 召唤物，改为 `dismiss` | 维护者清单里没有点名，但它是召唤物的移除，与 `spawn` 效果成对；保留 `despawn` 会读成“移除衍生物”，正是这次要消除的混淆，且违反“skill 包里剩下的 spawn 只属于衍生物或通用英文”。选 `dismiss`（游戏里“解散召唤物 / 宠物”的通用词）而不是生造的 `unsummon`；维护者若更想要 `unsummon`，是一次纯字符串替换 |
| `compile_visual.go` icon keyword 保留字 `"spawn"` | 通用英文，不改 | 图标关键词禁止太泛的词（skill / effect / entity / projectile / spawn / trigger / aoe），与哪一类无关；不加 `summon` 以免行为变化 |
| `program_quantity.go` 值类型名 `"spawn"`、`valueKindSpawn` | 衍生物，不改 | `$spawn` 的值类型 |
| `skill/combatcomponent/component.go` “spawn/config load” | 通用英文，不改 | 业务实体出生 / 加载时初始化战斗属性，与技能无关 |
| `skill/combatcomponent/adapter.go` “motion, spawns, and spawning stay with the business host” | 改为 “motion, spawns, and summoning” | “spawns” 指衍生物（`StepSpawn`），“spawning” 指召唤单位 |
| Visual 类别 `summon` | 召唤物，名字不变 | 本来就叫 summon；允许的效果从 `spawn` 改为 `summon` |

### 3.4 checkpoint（版本 4 → 5，旧版本拒绝恢复）

checkpoint 的 JSON 字段名都属于衍生物，不改；变的是**值**：cast 的 locals / memory 里如果存着召唤效果的结果（`result.as`），`checkpointEffectResultValue.Type` 是 `"spawn_result"`，改名后是 `"summon_result"`。衍生物 kind 不进 checkpoint（在 Program 里）。按 `RuntimeCheckpointVersion` 的约定（格式每次变化都递增），版本 4 → 5，版本 4 及更早得到 `ErrCheckpointUnsupported`（排空后再升级）。既有用例 `unsupported.Version++` 继续覆盖“非当前版本拒绝”。

### 3.5 digest

- **编译环境 digest（`CompileEnvironment.Digest`）**：authority digest 序列化了 `GameplayCatalog`（含单位模板：字段名 `MaximumSummonCount`、策略与指令值 `dismiss`），默认环境的 digest 从 `68cd09d4…` 变为 `a855e2dc…`。
- **gameplay / presentation digest**：gameplay digest 的输入含 `program.authority`（即上面的环境 digest），召唤操作的 `kind` 也从 `spawn` 变为 `summon`，所以按默认环境编译的 36 个 testdata 定义 gameplay digest **全部**改变，presentation digest 随之全部改变（例：`owned_trap.json` gameplay `35e40543…` → `c2376552…`）。
- **source document digest**：规范化文档是 `json.Marshal(Definition)`，效果按 Go 结构体编码、不含 `type` 字符串，所以 `"type":"spawn"` → `"type":"summon"` 本身不改变它；36 个里只有改了描述 / 别名的 3 个变化（`owned_pet_command.json` 描述与 `result.as` 别名 `spawned` → `summoned`，`owned_trap.json`、`two_point_wall.json` 描述）。

与上一次改名同理：这是改名的直接后果，不是语义变化；没有为保留 digest 把旧名留在 digest 输入里。实测见 §5。

### 3.6 第 2 类：衍生物（不改）的完整名单

以下 Go 词元全部由 `4451a0a5` 从 process 改名而来，或由 `3fad5b6e`（停止入口统一）为衍生物新增，按 §3.1 判定为衍生物，保持：

<details><summary>347 个词元（改名后从 `skill/` 全部 Go 文件取含 spawn 的词元，去掉 §3.3 的个案与 §3.2 改过的测试名）</summary>

`ActivePresentationSpawn`、`HasSpawn`、`MaxOwnedSpawns`、`MaxOwnedSpawnsPerOwner`、`MaxOwnedSpawnsPerProgram`、`MaxOwnedSpawnsPerTemplate`、`MaxSpawns`、`MaxStopPendingSpawns`、`MetricSpawnStopPendingDropped`、`MetricSpawnStopRetryExhausted`、`ModifySpawnEffectDefinition`、`MotionSpawnStage`、`MotionSpawnTrajectoryPair`、`NextSpawnID`、`OwnedSpawnSnapshot`、`OwnedSpawns`、`PresentationSpawnSignal`、`PresentationSpawnStart`、`PresentationSpawnStop`、`PresentationSpawnUpdate`、`Spawn`、`SpawnCallbacksDefinition`、`SpawnCancelled`、`SpawnCommandMeta`、`SpawnDefinition`、`SpawnEnded`、`SpawnFailed`、`SpawnHostState`、`SpawnID`、`SpawnInstance`、`SpawnKinds`、`SpawnNumericSnapshot`、`SpawnNumericState`、`SpawnProperties`、`SpawnPropertyCatalog`、`SpawnPropertyHandle`、`SpawnPropertyPolicy`、`SpawnPropertySlotBinding`、`SpawnRunning`、`SpawnRuntimeValue`、`SpawnScope`、`SpawnScopeCast`、`SpawnScopeEntity`、`SpawnScopePhase`、`SpawnSignal`、`SpawnSignalCancel`、`SpawnSignalCollision`、`SpawnSignalEnd`、`SpawnSignalEnter`、`SpawnSignalHit`、`SpawnSignalKind`、`SpawnSignalLeave`、`SpawnSignalTargetLost`、`SpawnSignalTick`、`SpawnSignalTransition`、`SpawnStateSnapshot`、`SpawnStatus`、`SpawnStepCommand`、`SpawnStepResult`、`SpawnStopCommand`、`SpawnStopPending`、`SpawnStopRetryBackoff`、`SpawnStopRetryLimit`、`SpawnTemplate`、`SpawnTemplateIndex`、`SpawnTemplateView`、`SpawnTrajectoryPairs`、`Spawns`、`StateMutationSpawnRemove`、`StateMutationSpawnUpsert`、`StepSpawn`、`StopPendingSpawns`、`StopRetryExhaustedSpawns`、`StopSpawn`、`TestCastsWhoseSpawnsEndedStayWithinTheCompletedCastLimit`、`TestCompileRejectsCastValuesInOwnedSpawnFields`、`TestEffectResultBranchDiagnosticNamesSpawnCallbacks`、`TestEffectResultBranchMayStartASpawnWithoutCallbacks`、`TestFailedChargeStartWhoseSpawnCannotStopKeepsItsCast`、`TestFailedChargeStartWithOwnedSpawnLeavesNoResidue`、`TestHandedOffAreaFinishEndsOnlyTheAreaSpawn`、`TestHandedOffSpawnStopFailureDoesNotFreezeTheRuntime`、`TestHandedOffSpawnsBeyondTheCompletedLimitStillRestore`、`TestInterruptSpawnStopFailureStillEndsTheCast`、`TestMemoryHostStopSpawnIsIdempotent`、`TestModifySpawnIsCallbackScopedAndDoesNotEmitWorldEffect`、`TestModifySpawnRejectsInvalidOwnershipOrValue`、`TestMotionCatalogRestrictsStageVariantsPerSpawnTrajectory`、`TestMovingSpawnUsesTemplateLifetimeAndCompletesOnTerminalTick`、`TestOwnedSpawnCancellationDetachesBeforeCallback`、`TestOwnedSpawnCapacityFailureDoesNotMutateHost`、`TestOwnedSpawnCleanupBoundaries`、`TestOwnedSpawnDetachedScopeRejectsCastDependencies`、`TestOwnedSpawnFieldsReadingTheCasterStillRun`、`TestOwnedSpawnHandoffAndPreHandoffCancellation`、`TestOwnedSpawnReplacementReleasesCapacityAtomically`、`TestOwnedSpawnSignalsUseCanonicalOrderAndTargetContext`、`TestOwnedSpawnStartFailureRollsBackReplacement`、`TestOwnedSpawnStopFailureRemainsTrackedForRetry`、`TestPresentationResetSpawnEntryMatchesItsIncrementalEvent`、`TestRuntimeCheckpointRestoresActiveSpawns`、`TestSpawnSignalsUseCanonicalOrder`、`TestSpawnStepPrimaryTargetIsRejectedAtCompileTime`、`TestSpawnStopEntriesAreRegistered`、`TestSpawnStopIsUnifiedAndIdempotent`、`TestSpawnVisualCompilesAndEmitsLifecycle`、`VisibilitySpawnSpatial`、`VisibilitySpawns`、`activeCallbackSpawn`、`activeHostSpawns`、`advanceOwnedSpawns`、`advanceSpawnNumeric`、`afterSpawns`、`applySpawnMotionStep`、`areaSpawnSignals`、`areaSpawnSkillJSON`、`beforeSpawn`、`beforeSpawns`、`bindSpawnNumericState`、`captureOwnedSpawnSnapshots`、`castHasRunningSpawnLocked`、`checkedSpawnNumericAdd`、`checkpointSpawn`、`checkpointSpawnMap`、`checkpointSpawnNumeric`、`clampSpawnNumeric`、`cloneSpawnSnapshots`、`cloneSpawnState`、`countSpawnSignals`、`decodeSpawn`、`decodeSpawnCallbacks`、`defaultSpawnPropertyCatalog`、`detachedSpawn`、`detachedSpawnCast`、`detachedSpawnLocals`、`diffSpawnStates`、`dispatchOwnedSpawnSignals`、`emitSpawnPresentation`、`emitSpawnSignals`、`environmentSpawnProperties`、`equalSpawnPropertyPolicy`、`evalNoCastInSpawn`、`evalRowSpawn`、`evalSpawnCallback`、`evalSpawnStartCapture`、`evalSpawnStep`、`executeModifySpawn`、`executeSpawnStep`、`executeWithSpawnValue`、`expiredSpawn`、`failOwnedSpawn`、`forgetCastSpawnsLocked`、`handoffEntitySpawns`、`hasOwnedSpawnCapacityExcluding`、`hasSpawn`、`hasSpawnSignal`、`has_spawn`、`initializeSpawnNumeric`、`isSpawnReference`、`lookupSpawnNumericProperty`、`lookupSpawnNumericState`、`lookupSpawnProperty`、`lookupSpawnPropertyPolicy`、`lookupSpawnPropertyPolicyByArtifacts`、`lowerSpawnNumericOperation`、`lowerSpawnProperties`、`max_stop_pending_spawns`、`memorySpawn`、`modifySpawnEffectIR`、`modifySpawnOperation`、`modify_spawn`、`newSpawn`、`nextOwnedSpawnTick`、`nextSpawnID`、`next_spawn_id`、`normalizeSpawn`、`normalizeSpawnSignals`、`numericLinearSpawn`、`numericLinearSpawnWithTracks`、`numericSpawnSkillJSON`、`onlySpawnOfCast`、`ownedSpawnTestHost`、`ownedSpawns`、`owned_spawn_callback_`、`owned_spawn_callback_cancel`、`owned_spawn_callback_collision`、`owned_spawn_callback_end`、`owned_spawn_callback_enter`、`owned_spawn_callback_hit`、`owned_spawn_callback_leave`、`owned_spawn_callback_tick`、`owned_spawns`、`previewOwnedSpawnCapacity`、`reapInvalidOwnedSpawns`、`reapOwnedSpawns`、`reapUnhandedEntitySpawns`、`reapedSpawn`、`replaceSpawnNumericTrack`、`requestSpawnStop`、`resolveSpawnNumeric`、`resolveSpawnProgram`、`restoreCheckpointSpawns`、`retrySpawnStopsLocked`、`runOwnedSpawnCallback`、`runtimeSpawnsOfCast`、`runtime_spawn_stop`、`sampleSpawnNumericTrack`、`snapshotSpawnNumeric`、`snapshotSpawnStart`、`spawnCallbackProgram`、`spawnCallbacks`、`spawnCallbacksIR`、`spawnFlow`、`spawnID`、`spawnIDs`、`spawnIR`、`spawnInvocationBound`、`spawnKind`、`spawnKinds`、`spawnNumericAdd`、`spawnNumericBase`、`spawnNumericBinding`、`spawnNumericBindingCount`、`spawnNumericBoundValue`、`spawnNumericFieldBound`、`spawnNumericInterpolation`、`spawnNumericLinearInteger`、`spawnNumericMulBP`、`spawnNumericOperation`、`spawnNumericOperations`、`spawnNumericRounding`、`spawnNumericSet`、`spawnNumericTruncateTowardZero`、`spawnPresentationTargetLocked`、`spawnPrograms`、`spawnProperties`、`spawnPropertyAngularSpeedMDegPerTick`、`spawnPropertyArcHeight`、`spawnPropertyBindingCount`、`spawnPropertyCollisionForce`、`spawnPropertyFieldAmplitude`、`spawnPropertyFieldAngularSpeed`、`spawnPropertyFieldForce`、`spawnPropertyFieldHeight`、`spawnPropertyFieldRadius`、`spawnPropertyFieldReturnSpeedBP`、`spawnPropertyFieldSpeed`、`spawnPropertyFieldTurnRateMDegPerTick`、`spawnPropertyKey`、`spawnPropertyKeys`、`spawnPropertyOffsetAmplitude`、`spawnPropertyOffsetRadius`、`spawnPropertyProgram`、`spawnPropertyRadius`、`spawnPropertyReturnSpeedBP`、`spawnPropertySlotBindingProgram`、`spawnPropertySlotCollision`、`spawnPropertySlotCompletion`、`spawnPropertySlotField`、`spawnPropertySlotFields`、`spawnPropertySlotOffset`、`spawnPropertySlotStage`、`spawnPropertySlotStages`、`spawnPropertySlotSteering`、`spawnPropertySlotTrajectory`、`spawnPropertySlotVariant`、`spawnPropertySlotVariants`、`spawnPropertySpawnArea`、`spawnPropertySpawnDash`、`spawnPropertySpawnKind`、`spawnPropertySpawnKinds`、`spawnPropertySpawnOrbit`、`spawnPropertySpawnProjectile`、`spawnPropertySpeed`、`spawnPropertyTurnRateMDegPerTick`、`spawnPropertyVariantBoomerang`、`spawnPropertyVariantCircular`、`spawnPropertyVariantLinear`、`spawnPropertyVariantOrbit`、`spawnPropertyVariantParabola`、`spawnPropertyVariantPath`、`spawnPropertyVariantPresent`、`spawnPropertyVariantTracking`、`spawnPropertyVariantZigzag`、`spawnReference`、`spawnRemove`、`spawnRow`、`spawnSignalRank`、`spawnSignalRankWithinContact`、`spawnSpatial`、`spawnStatusForStop`、`spawnStepTask`、`spawnStopCallGraph`、`spawnStopEntries`、`spawnStopEntry`、`spawnStopRetryDelay`、`spawnStopRetryMaxDoublings`、`spawnTemplate`、`spawnTemplateProgram`、`spawnTemplates`、`spawnValue`、`spawn_callback`、`spawn_id`、`spawn_kinds`、`spawn_owned`、`spawn_properties`、`spawn_property`、`spawn_remove`、`spawn_signal`、`spawn_spatial`、`spawn_start`、`spawn_start_read`、`spawn_status`、`spawn_step`、`spawn_stepped`、`spawn_stop`、`spawn_stop_entries_promises_test`、`spawn_stop_retry_backoff`、`spawn_stop_retry_limit`、`spawn_stopped`、`spawn_template`、`spawn_templates`、`spawn_trajectory_pairs`、`spawn_update`、`spawn_upsert`、`spawns`、`spawnsSnapshotLocked`、`startEntitySpawn`、`stepSpawnMotion`、`step_spawn`、`stopFinishingSpawns`、`stopScopedSpawns`、`stopSpawn`、`stopSpawns`、`stop_spawn`、`terminateOwnedSpawn`、`terminateSpawn`、`uniqueSpawnNumericBinding`、`validMotionSpawnKind`、`validateAreaSpawn`、`validateSpawnMotion`、`validateSpawnPropertyCatalog`、`valueKindSpawn`、`valueReferencesSpawn`、`visualAreaSpawn`、`visualSpawnSkill`、`walkSpawns`

</details>

另有 §3.3 判定为衍生物的 `captureSpawn`、`evalContextAreaSpawn`，以及 §3.2 改名后仍含 Spawn（衍生物）的三个测试名。

### 3.7 文件

| 旧 | 新 | 理由 |
| --- | --- | --- |
| `skill/runtime_owned_spawn.go` | `skill/runtime_owned_entity.go` | 内容是 `executeOwnedSummon` 与 `executeOwnedCommand`，都是召唤物效果；与 `compile_owned_entity.go` / `ir_owned_entity.go` / `program_owned_entity.go` / `memory_host_owned_entity.go` 同名排列 |
| `skill/compile_summon_spawn_promises_test.go` | `skill/compile_minion_spawn_promises_test.go` | 测的是 minion 衍生物的字段 |
| 不改：`spawn_owned.go`、`effect_result_branch_spawn_promises_test.go`、`host_owned_entity.go`、`owned_entity_test.go` | | 前两个是衍生物（owned 衍生物表、result 分支里启动衍生物）；后两个本来就叫 owned entity |

## 4. 没有改的，以及理由

### 4.1 历史记录

`docs/bug`、`docs/bugfix`、`docs/review`、`docs/history`、`docs/feature` 里已有的方案 / 实施记录（含上一份改名记录、ROUND12-SKILL-CFGGEN 的 O22）、CHANGELOG 已有条目与交接 §7 已有行描述当时的代码，不改；查旧名用本文 §3 与 CHANGELOG 的对照。`docs/release/v1.23.0/*` 不改，由汇总者按新名统一处理。

### 4.2 改名后 `rg -n -i 'spawn' skill docs/skill` 剩下的每一处

逐类核对（Go 词元按 §3.6 名单比对，Markdown / JSON 词元与带 spawn 的英文句子逐条看过）：

1. **衍生物（Spawn）**：Go 词元 347 个（§3.6）加 `captureSpawn`、`evalContextAreaSpawn` 与三个测试名中的 Spawn；JSON 里的 `"spawn":{…}`、`$spawn`、`modify_spawn`、`spawn_start`；testdata 描述 “area spawn”“beam spawn”“projectile spawn”；文档与注释里的 “spawn / spawns / Spawn” 全部指衍生物（Host `StepSpawn` / `StopSpawn`、`spawn_upsert` 等）。动词形式只剩 “a spawn the Host fails to stop”“host spawns = …” 这类，主语都是衍生物。
2. **新旧名对照本身（只出现旧名，作为说明）**：`docs/skill/README.md` 术语节的对照表、`skill/README.md`“迁移与版本”的第一条、`docs/skill/production-readiness.md` 的 checkpoint 版本说明、`skill/runtime_checkpoint.go` `RuntimeCheckpointVersion` 注释（版本 5 说明里的 `spawn_result`）。
3. **通用英文**：`skill/compile_visual.go` 图标关键词保留字 `"spawn"`（§3.3）；`skill/combatcomponent/component.go` “InitCombatant replaces the vitals block (spawn/config load)”（业务实体出生时初始化）。

第 1 类的旧名（`SpawnCommand`、`PreviewOwnedSpawn`、`despawn`、`spawn_tick`、`spawned_before`、`spawn_result`、`owned_entity_spawned` 等）在 `skill/` 的代码、testdata 与 `docs/skill/` 的正文里不再出现，只出现在第 2 条的对照里。`rg -n 'summon|minion' skill` 剩下的 `summon` 全部是召唤效果 / 召唤物（含 Visual 类别 `summon`、测试辅助 `summonWithCountingCancel` 等“召唤一个带衍生物的陷阱”），`minion` 全部是衍生物 kind。

## 5. 验证（2026-10-07，`GOWORK=off`，本机 darwin，worktree 基线 `8bde60ac`）

| 命令 | 结果 |
| --- | --- |
| `gofmt -l`（改动的 .go） | 空 |
| `go vet ./skill/...` | 通过 |
| `go test -race -count=3 ./skill/...`（含 `compile_mutation_property_test.go` 等性质测试） | skill 56.8s、combat、combatcomponent、skillcompose、skillsync 全部 ok |
| `go test -run '^$' -fuzz '^FuzzParseGeneratedNeverPanics$' -fuzztime 20s ./skill/`、`FuzzRestoreRuntimeCheckpointNeverPanics` 同样 20s | 两个都 PASS，未产生新语料 |
| `skill/examples`、`skill/integration/sync-e2e` 两个独立模块（replace 到本仓）`go vet ./... && go test ./...` | 通过（sync-e2e ok，examples 无测试文件） |
| 根包 `go test -count=1 .` | ok；`-v` 核对 `TestExamplesRun`（含 skill/examples 三个示例实跑）与 `TestTrackedMarkdownRelativeLinksResolve` 均 PASS |
| `go build ./... && go vet ./...` | 通过 |
| `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`；`go test ./cmd/glsvet/...` | 无违例（exit 0）；ok |
| digest 对比：基线与改名后各编译 36 个 testdata 定义打印 `InspectIdentity` 与环境 digest | 见 §3.5：环境 digest 变化；gameplay / presentation 36 个全部变化；source document 3 个变化 |
| 旧定义：基线 `owned_trap.json` 交给改名后的 `skill.Parse` | `phases: [0].on: [0]: effect: unsupported effect "spawn"`（T-289 的现象） |
| 旧 kind：召唤效果上挂 `"spawn":{"kind":"summon"}` 编译 | `MOTION_INVALID at $.phases[0].on.enter.steps[0].spawn.kind: spawn kind is not a closed motion kind` |

没跑的：codegen 测试、`go generate ./...` 与 game-demo 重新生成——codegen、demo 模板、kit、examples 不引用任何改名的名字（`rg -w` 核对 `SpawnCommand` / `PreviewOwnedSpawn` / `despawn` / `spawned_before` / `spawn_result` 等全部旧名，以及 `"type":"spawn"`、`"kind":"summon"`，零命中；demo 唯一的技能 `fireball.json.tmpl` 只有 `finish`），skill 包也没有 `go:generate`。改名不涉及 nest / entity / dataengine / sync，glsvet 按要求跑了一次作确认。

## 6. 实施状态

已实施，未发版（分支 `summon`）。行为不变：除改名外没有改测试断言（测试里改的是名字、JSON 里的效果类型 / kind / 别名，以及失败文案里的称呼）；格式变化是 checkpoint 版本号 4 → 5，以及随名字变化的 digest。

机械替换（`perl` 按词边界替换 §3.2 的 Go 标识符、JSON 里的 `"type":"spawn"` / `"despawn"` / `spawned_*` / `spawn_tick` / `spawn_sequence` / `spawn_result` / MemoryHost 事件名）之后人工复核：按基线 `4451a0a5^` 里所有含 spawn 的旧注释 / 文案逐条找到现在的位置，改写指召唤效果的句子（例：“只有 spawn 会启动实体衍生物” → “只有召唤效果会启动实体衍生物”，局部变量 `spawn, ok := typed.effect.(*summonEffectIR)` → `summon`）；衍生物 kind 的 `summon` 先于效果改名单独替换为 `minion`，避免两者混在一起。

未做 / 留给维护者：
- `despawn` 选了 `dismiss`（§3.3）；若要 `unsummon` 是一次纯字符串替换。
- 发现（不是本次改名引入，本次不改，交给维护者 / review 定是否登记）：source document digest 是 `json.Marshal(Definition)`（`skill/canonical_definition.go`），效果按 Go 结构体字段编码、不含效果类型，所以字段完全相同的两种效果得到同一个 source document digest。实测：同一份定义里把 `{"type":"set_memory","name":"x","value":1}` 换成 `add_memory`，`InspectIdentity` 的 `SourceDocumentDigest` 相同、`GameplayDigest` 不同（`SetMemoryEffectDefinition` 与 `AddMemoryEffectDefinition` 都是 `{Name, Value}`）。框架内没有用 source document digest 做身份判断（只经 `InspectIdentity` 暴露），影响限于拿它判断“源文件变没变”的调用方。召唤改名本身不受影响（`SummonEffectDefinition` 的字段集合独有，`DismissEffectDefinition{Target}` 也没有同形的效果）。**已闭环**：登记为 [RR-20261006-33](../bug/RR-20261006-33.md)（排查时另发现 `json:"-"` 的消耗数量、cast window 表达式也不在摘要里，以及策略 / 输入 / 形状 / 过滤器的同形类型），已修复，[修复记录](../bugfix/RR-20261006-33.md)；修复后全部源文档 digest 改变，gameplay / presentation digest 不变。
