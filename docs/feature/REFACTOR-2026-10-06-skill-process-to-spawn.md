# 重构：skill 的“进程”（process）全量改名为衍生物（Spawn）

日期 2026-10-06（实施 2026-10-07），分支 `spawnrename`，基线 main `3285d354`。重构登记，不占 RR。未发版。

## 1. 维护者决定

[DECISIONS-PENDING 第十三轮](../review/DECISIONS-PENDING-2026-10-05.md)“skill 进程改名”：维护者原话“skill的一个流程叫做进程很奇怪，用更专业的词语”，选定 “Spawn吧”。线上没有部署，**不保留旧名、不做兼容别名**。

**衍生物（Spawn）**：技能施放时生成、之后每个 tick 由技能驱动的东西——飞行物（projectile）、法术场 / 光环（area）、召唤物（summon）、光束（beam）、位移（dash）、环绕（orbit）。DSL 里的 `spawn` 效果生成实体，挂在它上面的 `spawn` 定义（原 `process`）就是这些实体上运行的衍生物。

## 2. 目标与改动面

- 目标：名字。行为不变——测试除改名外不改断言；唯一的格式变化是 checkpoint 版本号 3 → 4。
- 改动面：`skill/`（含 skillsync、skillcompose、combatcomponent、testdata、integration/sync-e2e、examples）全部 Go / JSON / Markdown，`docs/skill/*`，CHANGELOG 一条，TROUBLESHOOTING 一行（T-288）与 T-271 / T-265 两行补注，交接 §7 一行，DECISIONS-PENDING 实施状态。
- 仓外与其他目录：codegen、demo 模板、kit、examples 没有引用这些名字（`rg` 核对：demo 的 `fireball.json.tmpl` 不带 spawn 定义；codegen 只在帮助文案里提 skill 包）。不需要改生成物。
- 结构：包与目录不变；16 个文件改名（§3.8），内容不拆不合。

## 3. 新旧名对照

改名规则（脚本机械替换后人工复核）：小写 `process` / `processes`（前一个字符不是字母、后一个字符不是小写字母，所以 `processed`、`processing`、`processor` 不动）→ `spawn` / `spawns`；`Process` / `Processes`（后一个字符不是小写字母）→ `Spawn` / `Spawns`；中文“进程”→“衍生物”。之后逐条复核通用英文与操作系统进程（§4）。

### 3.1 技能 DSL（JSON）

| 旧 | 新 | 位置 |
| --- | --- | --- |
| effect flow 的 `"process": {…}` | `"spawn": {…}` | `{"flow":"effect","effect":{"type":"spawn",…},"spawn":{"kind":"area",…},"on":{…}}`；只能写在 spawn 效果上 |
| `{"type":"modify_process","process":"$process",…}` | `{"type":"modify_spawn","spawn":"$spawn",…}` | 回调里改衍生物的数值属性 |
| 引用 `$process` | `$spawn` | 衍生物回调里的当前衍生物 |
| 快照点 `"snapshot":"process_start"` | `"snapshot":"spawn_start"` | `read_attribute` |
| 编译环境 JSON `process_properties`（含 `$.process_properties.revision` / `.properties`）、`process_kinds`、`motion.process_trajectory_pairs` | `spawn_properties`、`spawn_kinds`、`motion.spawn_trajectory_pairs` | `CompileEnvironment` |
| 引用 `$environment.process_properties` | `$environment.spawn_properties` | |
| skillcompose 特性键 `motion.process`（kind `process`）、预算 / 指标 JSON `processes` | `motion.spawn`（kind `spawn`）、`spawns` | `skillcompose` |
| 诊断路径 `….process.area.from`、`….process.motion.steering.target` 等 | `….spawn.area.from`、`….spawn.motion.steering.target` | 编译诊断 |
| 值类型名 `process` | `spawn` | 类型检查诊断里的类型名 |

### 3.2 求值上下文（编译诊断与 `ErrReferenceOutOfContext` 文案）

| 旧 | 新 |
| --- | --- |
| `process_start`（采样） | `spawn_start` |
| `process_step`（衍生物每一步） | `spawn_step` |
| `process_callback`（衍生物回调） | `spawn_callback` |

诊断文案里的 “process” 一并改为 “spawn”，中文说明里的“进程”改为“衍生物”。例：`only a spawn effect starts a spawn`、`only a spawn effect has spawn callbacks`、`summon spawns live for the spawn effect's duration_ticks; remove the spawn's own duration_ticks`。

### 3.3 同步与表现（skillsync / 客户端可见）

| 旧 | 新 | 类别 |
| --- | --- | --- |
| `process_upsert` / `process_remove` | `spawn_upsert` / `spawn_remove` | state mutation kind |
| mutation / reset / 状态快照字段 `process_id`、`process`、`processes` | `spawn_id`、`spawn`、`spawns` | JSON |
| `process_start` / `process_update` / `process_signal` / `process_stop` | `spawn_start` / `spawn_update` / `spawn_signal` / `spawn_stop` | presentation event kind |
| presentation 字段 `process_id`、`process_template`、`process_status` | `spawn_id`、`spawn_template`、`spawn_status` | JSON |
| Visual 挂载位置 `process.visual`（衍生物定义里的 `visual`）、presentation plan 的 `Processes` mount | `spawn.visual`、`Spawns` | Visual |
| skillsync 可见性字段 `processes`、`process_spatial` | `spawns`、`spawn_spatial` | `VisibilityField` |
| RuntimeValue JSON `process` | `spawn` | |
| Runtime 事件 `owned_process_callback_<event>` | `owned_spawn_callback_<event>` | `RuntimeEvent.Kind` |
| MemoryHost 事件 `process_stepped` / `process_stopped` | `spawn_stepped` / `spawn_stopped` | |
| 回放记录 `step_process` / `stop_process` | `step_spawn` / `stop_spawn` | `replay.go` |
| 停止重试日志字段 `process_id` | `spawn_id` | slog |

### 3.4 checkpoint（版本 3 → 4，旧版本拒绝恢复）

| 旧 JSON 字段 | 新 |
| --- | --- |
| `processes` | `spawns` |
| `owned_processes` | `owned_spawns` |
| `next_process_id` | `next_spawn_id` |
| `process_stop_retry_backoff` / `process_stop_retry_limit` | `spawn_stop_retry_backoff` / `spawn_stop_retry_limit` |
| `max_stop_pending_processes` | `max_stop_pending_spawns` |
| 记录里的 `process_id`、`process` | `spawn_id`、`spawn` |
| 排程任务 kind `process_step` | `spawn_step` |

`RuntimeCheckpointVersion` = 4；版本 3 及更早得到 `ErrCheckpointUnsupported`（排空后再升级）。既有用例 `unsupported.Version++` 继续覆盖“非当前版本拒绝”。

**gameplay / presentation digest 全部改变**：digest 的输入里有 `spawn_templates`、`spawn_properties`、`has_spawn` 等字段名（原 `process_*`），所以同一份定义改名后 digest 不同——36 个 testdata 定义的 gameplay 与 presentation digest 全部变化（本地对比基线与改名后逐个打印核对）。这是改名的直接后果，不是语义变化：skillcompose 契约要按新 digest 重签，旧回放记录与旧 checkpoint 一样不再适用。没有为保留 digest 把旧名留在 digest 输入里（维护者要求不保留旧名）。

### 3.5 指标

| 旧 | 新 |
| --- | --- |
| `skill.process.stop_retry_exhausted.total`（`MetricProcessStopRetryExhausted`） | `skill.spawn.stop_retry_exhausted.total`（`MetricSpawnStopRetryExhausted`） |
| `skill.process.stop_pending_dropped.total`（`MetricProcessStopPendingDropped`） | `skill.spawn.stop_pending_dropped.total`（`MetricSpawnStopPendingDropped`） |

### 3.6 Host 接口与公开 Go API

Host 接口：`StepProcess(ProcessStepCommand, ProcessHostState) (ProcessStepResult, error)` → `StepSpawn(SpawnStepCommand, SpawnHostState) (SpawnStepResult, error)`；`StopProcess(ProcessStopCommand, ProcessHostState)` → `StopSpawn(SpawnStopCommand, SpawnHostState)`（幂等契约不变）。MemoryHost 同名方法一起改。

`RuntimeOptions`：`ProcessStopRetryBackoff` / `ProcessStopRetryLimit` / `MaxStopPendingProcesses` / `MaxOwnedProcesses*` → `SpawnStopRetryBackoff` / `SpawnStopRetryLimit` / `MaxStopPendingSpawns` / `MaxOwnedSpawns*`。`RuntimeRetentionStats`：`StopPendingProcesses` / `StopRetryExhaustedProcesses` → `StopPendingSpawns` / `StopRetryExhaustedSpawns`。

全部导出标识符（含类型、字段、常量、方法；同名字段出现在多个结构里时只列一次）：

| 旧名 | 新名 |
| --- | --- |
| `ActivePresentationProcess` | `ActivePresentationSpawn` |
| `HasProcess` | `HasSpawn` |
| `MaxOwnedProcesses` | `MaxOwnedSpawns` |
| `MaxOwnedProcessesPerOwner` | `MaxOwnedSpawnsPerOwner` |
| `MaxOwnedProcessesPerProgram` | `MaxOwnedSpawnsPerProgram` |
| `MaxOwnedProcessesPerTemplate` | `MaxOwnedSpawnsPerTemplate` |
| `MaxProcesses` | `MaxSpawns` |
| `MaxStopPendingProcesses` | `MaxStopPendingSpawns` |
| `MetricProcessStopPendingDropped` | `MetricSpawnStopPendingDropped` |
| `MetricProcessStopRetryExhausted` | `MetricSpawnStopRetryExhausted` |
| `ModifyProcessEffectDefinition` | `ModifySpawnEffectDefinition` |
| `MotionProcessStage` | `MotionSpawnStage` |
| `MotionProcessTrajectoryPair` | `MotionSpawnTrajectoryPair` |
| `NextProcessID` | `NextSpawnID` |
| `OwnedProcessSnapshot` | `OwnedSpawnSnapshot` |
| `OwnedProcesses` | `OwnedSpawns` |
| `PresentationProcessSignal` | `PresentationSpawnSignal` |
| `PresentationProcessStart` | `PresentationSpawnStart` |
| `PresentationProcessStop` | `PresentationSpawnStop` |
| `PresentationProcessUpdate` | `PresentationSpawnUpdate` |
| `Process` | `Spawn` |
| `ProcessCallbacksDefinition` | `SpawnCallbacksDefinition` |
| `ProcessCancelled` | `SpawnCancelled` |
| `ProcessCommandMeta` | `SpawnCommandMeta` |
| `ProcessDefinition` | `SpawnDefinition` |
| `ProcessEnded` | `SpawnEnded` |
| `ProcessFailed` | `SpawnFailed` |
| `ProcessHostState` | `SpawnHostState` |
| `ProcessID` | `SpawnID` |
| `ProcessInstance` | `SpawnInstance` |
| `ProcessKinds` | `SpawnKinds` |
| `ProcessNumericSnapshot` | `SpawnNumericSnapshot` |
| `ProcessNumericState` | `SpawnNumericState` |
| `ProcessProperties` | `SpawnProperties` |
| `ProcessPropertyCatalog` | `SpawnPropertyCatalog` |
| `ProcessPropertyHandle` | `SpawnPropertyHandle` |
| `ProcessPropertyPolicy` | `SpawnPropertyPolicy` |
| `ProcessPropertySlotBinding` | `SpawnPropertySlotBinding` |
| `ProcessRunning` | `SpawnRunning` |
| `ProcessRuntimeValue` | `SpawnRuntimeValue` |
| `ProcessScope` | `SpawnScope` |
| `ProcessScopeCast` | `SpawnScopeCast` |
| `ProcessScopeEntity` | `SpawnScopeEntity` |
| `ProcessScopePhase` | `SpawnScopePhase` |
| `ProcessSignal` | `SpawnSignal` |
| `ProcessSignalCancel` | `SpawnSignalCancel` |
| `ProcessSignalCollision` | `SpawnSignalCollision` |
| `ProcessSignalEnd` | `SpawnSignalEnd` |
| `ProcessSignalEnter` | `SpawnSignalEnter` |
| `ProcessSignalHit` | `SpawnSignalHit` |
| `ProcessSignalKind` | `SpawnSignalKind` |
| `ProcessSignalLeave` | `SpawnSignalLeave` |
| `ProcessSignalTargetLost` | `SpawnSignalTargetLost` |
| `ProcessSignalTick` | `SpawnSignalTick` |
| `ProcessSignalTransition` | `SpawnSignalTransition` |
| `ProcessStateSnapshot` | `SpawnStateSnapshot` |
| `ProcessStatus` | `SpawnStatus` |
| `ProcessStepCommand` | `SpawnStepCommand` |
| `ProcessStepResult` | `SpawnStepResult` |
| `ProcessStopCommand` | `SpawnStopCommand` |
| `ProcessStopPending` | `SpawnStopPending` |
| `ProcessStopRetryBackoff` | `SpawnStopRetryBackoff` |
| `ProcessStopRetryLimit` | `SpawnStopRetryLimit` |
| `ProcessTemplate` | `SpawnTemplate` |
| `ProcessTemplateIndex` | `SpawnTemplateIndex` |
| `ProcessTemplateView` | `SpawnTemplateView` |
| `ProcessTrajectoryPairs` | `SpawnTrajectoryPairs` |
| `Processes` | `Spawns` |
| `StateMutationProcessRemove` | `StateMutationSpawnRemove` |
| `StateMutationProcessUpsert` | `StateMutationSpawnUpsert` |
| `StepProcess` | `StepSpawn` |
| `StopPendingProcesses` | `StopPendingSpawns` |
| `StopProcess` | `StopSpawn` |
| `StopRetryExhaustedProcesses` | `StopRetryExhaustedSpawns` |
| `VisibilityProcessSpatial` | `VisibilitySpawnSpatial` |
| `VisibilityProcesses` | `VisibilitySpawns` |

### 3.7 未导出标识符与测试名

规则同上，全部机械替换（编译器保证无遗漏引用）。完整列表（从基线 `skill/` 全部 Go 词元里取含 process 的，含注释里的词）：

<details><summary>未导出标识符与词（198 个）</summary>

`activeCallbackProcess`→`activeCallbackSpawn`、`activeHostProcesses`→`activeHostSpawns`、`advanceOwnedProcesses`→`advanceOwnedSpawns`、`advanceProcessNumeric`→`advanceSpawnNumeric`、`afterProcesses`→`afterSpawns`、`applyProcessMotionStep`→`applySpawnMotionStep`、`areaProcessSignals`→`areaSpawnSignals`、`areaProcessSkillJSON`→`areaSpawnSkillJSON`、`beforeProcess`→`beforeSpawn`、`beforeProcesses`→`beforeSpawns`、`bindProcessNumericState`→`bindSpawnNumericState`、`captureOwnedProcessSnapshots`→`captureOwnedSpawnSnapshots`、`castHasRunningProcessLocked`→`castHasRunningSpawnLocked`、`checkedProcessNumericAdd`→`checkedSpawnNumericAdd`、`checkpointProcess`→`checkpointSpawn`、`checkpointProcessMap`→`checkpointSpawnMap`、`checkpointProcessNumeric`→`checkpointSpawnNumeric`、`clampProcessNumeric`→`clampSpawnNumeric`、`cloneProcessSnapshots`→`cloneSpawnSnapshots`、`cloneProcessState`→`cloneSpawnState`、`countProcessSignals`→`countSpawnSignals`、`decodeProcess`→`decodeSpawn`、`decodeProcessCallbacks`→`decodeSpawnCallbacks`、`defaultProcessPropertyCatalog`→`defaultSpawnPropertyCatalog`、`deferUnstoppedProcessesLocked`→`deferUnstoppedSpawnsLocked`、`detachedProcess`→`detachedSpawn`、`detachedProcessCast`→`detachedSpawnCast`、`detachedProcessLocals`→`detachedSpawnLocals`、`diffProcessStates`→`diffSpawnStates`、`dispatchOwnedProcessSignals`→`dispatchOwnedSpawnSignals`、`emitProcessPresentation`→`emitSpawnPresentation`、`emitProcessSignals`→`emitSpawnSignals`、`environmentProcessProperties`→`environmentSpawnProperties`、`equalProcessPropertyPolicy`→`equalSpawnPropertyPolicy`、`evalNoCastInProcess`→`evalNoCastInSpawn`、`evalProcessCallback`→`evalSpawnCallback`、`evalProcessStartCapture`→`evalSpawnStartCapture`、`evalProcessStep`→`evalSpawnStep`、`evalRowProcess`→`evalRowSpawn`、`executeModifyProcess`→`executeModifySpawn`、`executeProcessStep`→`executeSpawnStep`、`executeWithProcessValue`→`executeWithSpawnValue`、`expiredProcess`→`expiredSpawn`、`failOwnedProcess`→`failOwnedSpawn`、`forgetCastProcessesLocked`→`forgetCastSpawnsLocked`、`handoffEntityProcesses`→`handoffEntitySpawns`、`hasOwnedProcessCapacityExcluding`→`hasOwnedSpawnCapacityExcluding`、`hasProcess`→`hasSpawn`、`hasProcessSignal`→`hasSpawnSignal`、`initializeProcessNumeric`→`initializeSpawnNumeric`、`isProcessReference`→`isSpawnReference`、`lookupProcessNumericProperty`→`lookupSpawnNumericProperty`、`lookupProcessNumericState`→`lookupSpawnNumericState`、`lookupProcessProperty`→`lookupSpawnProperty`、`lookupProcessPropertyPolicy`→`lookupSpawnPropertyPolicy`、`lookupProcessPropertyPolicyByArtifacts`→`lookupSpawnPropertyPolicyByArtifacts`、`lowerProcessNumericOperation`→`lowerSpawnNumericOperation`、`lowerProcessProperties`→`lowerSpawnProperties`、`memoryProcess`→`memorySpawn`、`modifyProcessEffectIR`→`modifySpawnEffectIR`、`modifyProcessOperation`→`modifySpawnOperation`、`newProcess`→`newSpawn`、`nextOwnedProcessTick`→`nextOwnedSpawnTick`、`nextProcessID`→`nextSpawnID`、`normalizeProcess`→`normalizeSpawn`、`normalizeProcessSignals`→`normalizeSpawnSignals`、`numericLinearProcess`→`numericLinearSpawn`、`numericLinearProcessWithTracks`→`numericLinearSpawnWithTracks`、`numericProcessSkillJSON`→`numericSpawnSkillJSON`、`onlyProcessOfCast`→`onlySpawnOfCast`、`ownedProcessTestHost`→`ownedSpawnTestHost`、`ownedProcesses`→`ownedSpawns`、`previewOwnedProcessCapacity`→`previewOwnedSpawnCapacity`、`process`→`spawn`、`processCallbackProgram`→`spawnCallbackProgram`、`processCallbacks`→`spawnCallbacks`、`processCallbacksIR`→`spawnCallbacksIR`、`processFlow`→`spawnFlow`、`processID`→`spawnID`、`processIDs`→`spawnIDs`、`processIR`→`spawnIR`、`processInvocationBound`→`spawnInvocationBound`、`processKind`→`spawnKind`、`processKinds`→`spawnKinds`、`processNumericAdd`→`spawnNumericAdd`、`processNumericBase`→`spawnNumericBase`、`processNumericBinding`→`spawnNumericBinding`、`processNumericBindingCount`→`spawnNumericBindingCount`、`processNumericBoundValue`→`spawnNumericBoundValue`、`processNumericFieldBound`→`spawnNumericFieldBound`、`processNumericInterpolation`→`spawnNumericInterpolation`、`processNumericLinearInteger`→`spawnNumericLinearInteger`、`processNumericMulBP`→`spawnNumericMulBP`、`processNumericOperation`→`spawnNumericOperation`、`processNumericOperations`→`spawnNumericOperations`、`processNumericRounding`→`spawnNumericRounding`、`processNumericSet`→`spawnNumericSet`、`processNumericTruncateTowardZero`→`spawnNumericTruncateTowardZero`、`processPresentationTargetLocked`→`spawnPresentationTargetLocked`、`processPrograms`→`spawnPrograms`、`processProperties`→`spawnProperties`、`processPropertyAngularSpeedMDegPerTick`→`spawnPropertyAngularSpeedMDegPerTick`、`processPropertyArcHeight`→`spawnPropertyArcHeight`、`processPropertyBindingCount`→`spawnPropertyBindingCount`、`processPropertyCollisionForce`→`spawnPropertyCollisionForce`、`processPropertyFieldAmplitude`→`spawnPropertyFieldAmplitude`、`processPropertyFieldAngularSpeed`→`spawnPropertyFieldAngularSpeed`、`processPropertyFieldForce`→`spawnPropertyFieldForce`、`processPropertyFieldHeight`→`spawnPropertyFieldHeight`、`processPropertyFieldRadius`→`spawnPropertyFieldRadius`、`processPropertyFieldReturnSpeedBP`→`spawnPropertyFieldReturnSpeedBP`、`processPropertyFieldSpeed`→`spawnPropertyFieldSpeed`、`processPropertyFieldTurnRateMDegPerTick`→`spawnPropertyFieldTurnRateMDegPerTick`、`processPropertyKey`→`spawnPropertyKey`、`processPropertyKeys`→`spawnPropertyKeys`、`processPropertyOffsetAmplitude`→`spawnPropertyOffsetAmplitude`、`processPropertyOffsetRadius`→`spawnPropertyOffsetRadius`、`processPropertyProcessArea`→`spawnPropertySpawnArea`、`processPropertyProcessDash`→`spawnPropertySpawnDash`、`processPropertyProcessKind`→`spawnPropertySpawnKind`、`processPropertyProcessKinds`→`spawnPropertySpawnKinds`、`processPropertyProcessOrbit`→`spawnPropertySpawnOrbit`、`processPropertyProcessProjectile`→`spawnPropertySpawnProjectile`、`processPropertyProgram`→`spawnPropertyProgram`、`processPropertyRadius`→`spawnPropertyRadius`、`processPropertyReturnSpeedBP`→`spawnPropertyReturnSpeedBP`、`processPropertySlotBindingProgram`→`spawnPropertySlotBindingProgram`、`processPropertySlotCollision`→`spawnPropertySlotCollision`、`processPropertySlotCompletion`→`spawnPropertySlotCompletion`、`processPropertySlotField`→`spawnPropertySlotField`、`processPropertySlotFields`→`spawnPropertySlotFields`、`processPropertySlotOffset`→`spawnPropertySlotOffset`、`processPropertySlotStage`→`spawnPropertySlotStage`、`processPropertySlotStages`→`spawnPropertySlotStages`、`processPropertySlotSteering`→`spawnPropertySlotSteering`、`processPropertySlotTrajectory`→`spawnPropertySlotTrajectory`、`processPropertySlotVariant`→`spawnPropertySlotVariant`、`processPropertySlotVariants`→`spawnPropertySlotVariants`、`processPropertySpeed`→`spawnPropertySpeed`、`processPropertyTurnRateMDegPerTick`→`spawnPropertyTurnRateMDegPerTick`、`processPropertyVariantBoomerang`→`spawnPropertyVariantBoomerang`、`processPropertyVariantCircular`→`spawnPropertyVariantCircular`、`processPropertyVariantLinear`→`spawnPropertyVariantLinear`、`processPropertyVariantOrbit`→`spawnPropertyVariantOrbit`、`processPropertyVariantParabola`→`spawnPropertyVariantParabola`、`processPropertyVariantPath`→`spawnPropertyVariantPath`、`processPropertyVariantPresent`→`spawnPropertyVariantPresent`、`processPropertyVariantTracking`→`spawnPropertyVariantTracking`、`processPropertyVariantZigzag`→`spawnPropertyVariantZigzag`、`processReference`→`spawnReference`、`processRemove`→`spawnRemove`、`processRow`→`spawnRow`、`processSignalRank`→`spawnSignalRank`、`processSignalRankWithinContact`→`spawnSignalRankWithinContact`、`processSpatial`→`spawnSpatial`、`processStatusForStop`→`spawnStatusForStop`、`processStepTask`→`spawnStepTask`、`processStopRetryDelay`→`spawnStopRetryDelay`、`processStopRetryMaxDoublings`→`spawnStopRetryMaxDoublings`、`processTemplate`→`spawnTemplate`、`processTemplateProgram`→`spawnTemplateProgram`、`processTemplates`→`spawnTemplates`、`processValue`→`spawnValue`、`processes`→`spawns`、`processesSnapshotLocked`→`spawnsSnapshotLocked`、`reapInvalidOwnedProcesses`→`reapInvalidOwnedSpawns`、`reapOwnedProcesses`→`reapOwnedSpawns`、`reapUnhandedEntityProcesses`→`reapUnhandedEntitySpawns`、`reapedProcess`→`reapedSpawn`、`replaceProcessNumericTrack`→`replaceSpawnNumericTrack`、`resolveProcessNumeric`→`resolveSpawnNumeric`、`resolveProcessProgram`→`resolveSpawnProgram`、`restoreCheckpointProcesses`→`restoreCheckpointSpawns`、`retryProcessStopsLocked`→`retrySpawnStopsLocked`、`runOwnedProcessCallback`→`runOwnedSpawnCallback`、`runtimeProcessesOfCast`→`runtimeSpawnsOfCast`、`sampleProcessNumericTrack`→`sampleSpawnNumericTrack`、`snapshotProcessNumeric`→`snapshotSpawnNumeric`、`snapshotProcessStart`→`snapshotSpawnStart`、`startEntityProcess`→`startEntitySpawn`、`stepProcessMotion`→`stepSpawnMotion`、`stopFinishingProcesses`→`stopFinishingSpawns`、`stopOwnedProcess`→`stopOwnedSpawn`、`stopProcess`→`stopSpawn`、`stopProcesses`→`stopSpawns`、`stopScopedProcesses`→`stopScopedSpawns`、`terminateOwnedProcess`→`terminateOwnedSpawn`、`terminateProcess`→`terminateSpawn`、`uniqueProcessNumericBinding`→`uniqueSpawnNumericBinding`、`validMotionProcessKind`→`validMotionSpawnKind`、`validateAreaProcess`→`validateAreaSpawn`、`validateProcessMotion`→`validateSpawnMotion`、`validateProcessPropertyCatalog`→`validateSpawnPropertyCatalog`、`valueKindProcess`→`valueKindSpawn`、`valueReferencesProcess`→`valueReferencesSpawn`、`visualAreaProcess`→`visualAreaSpawn`、`visualProcessSkill`→`visualSpawnSkill`、`walkProcesses`→`walkSpawns`

</details>

<details><summary>测试函数（34 个）</summary>

`TestCastsWhoseProcessesEndedStayWithinTheCompletedCastLimit`→`TestCastsWhoseSpawnsEndedStayWithinTheCompletedCastLimit`、`TestCompileRejectsCastValuesInOwnedProcessFields`→`TestCompileRejectsCastValuesInOwnedSpawnFields`、`TestCompileRejectsProcessesOnEffectsThatDoNotSpawn`→`TestCompileRejectsSpawnsOnEffectsThatDoNotSpawn`、`TestEffectResultBranchDiagnosticNamesProcessCallbacks`→`TestEffectResultBranchDiagnosticNamesSpawnCallbacks`、`TestEffectResultBranchMayStartAProcessWithoutCallbacks`→`TestEffectResultBranchMayStartASpawnWithoutCallbacks`、`TestFailedChargeStartWhoseProcessCannotStopKeepsItsCast`→`TestFailedChargeStartWhoseSpawnCannotStopKeepsItsCast`、`TestFailedChargeStartWithOwnedProcessLeavesNoResidue`→`TestFailedChargeStartWithOwnedSpawnLeavesNoResidue`、`TestHandedOffAreaFinishEndsOnlyTheAreaProcess`→`TestHandedOffAreaFinishEndsOnlyTheAreaSpawn`、`TestHandedOffProcessStopFailureDoesNotFreezeTheRuntime`→`TestHandedOffSpawnStopFailureDoesNotFreezeTheRuntime`、`TestHandedOffProcessesBeyondTheCompletedLimitStillRestore`→`TestHandedOffSpawnsBeyondTheCompletedLimitStillRestore`、`TestInterruptProcessStopFailureStillEndsTheCast`→`TestInterruptSpawnStopFailureStillEndsTheCast`、`TestMemoryHostStopProcessIsIdempotent`→`TestMemoryHostStopSpawnIsIdempotent`、`TestModifyProcessIsCallbackScopedAndDoesNotEmitWorldEffect`→`TestModifySpawnIsCallbackScopedAndDoesNotEmitWorldEffect`、`TestModifyProcessRejectsInvalidOwnershipOrValue`→`TestModifySpawnRejectsInvalidOwnershipOrValue`、`TestMotionCatalogRestrictsStageVariantsPerProcessTrajectory`→`TestMotionCatalogRestrictsStageVariantsPerSpawnTrajectory`、`TestMovingProcessUsesTemplateLifetimeAndCompletesOnTerminalTick`→`TestMovingSpawnUsesTemplateLifetimeAndCompletesOnTerminalTick`、`TestOwnedProcessCancellationDetachesBeforeCallback`→`TestOwnedSpawnCancellationDetachesBeforeCallback`、`TestOwnedProcessCapacityFailureDoesNotMutateHost`→`TestOwnedSpawnCapacityFailureDoesNotMutateHost`、`TestOwnedProcessCleanupBoundaries`→`TestOwnedSpawnCleanupBoundaries`、`TestOwnedProcessDetachedScopeRejectsCastDependencies`→`TestOwnedSpawnDetachedScopeRejectsCastDependencies`、`TestOwnedProcessFieldsReadingTheCasterStillRun`→`TestOwnedSpawnFieldsReadingTheCasterStillRun`、`TestOwnedProcessHandoffAndPreHandoffCancellation`→`TestOwnedSpawnHandoffAndPreHandoffCancellation`、`TestOwnedProcessReplacementReleasesCapacityAtomically`→`TestOwnedSpawnReplacementReleasesCapacityAtomically`、`TestOwnedProcessSignalsUseCanonicalOrderAndTargetContext`→`TestOwnedSpawnSignalsUseCanonicalOrderAndTargetContext`、`TestOwnedProcessStartFailureRollsBackReplacement`→`TestOwnedSpawnStartFailureRollsBackReplacement`、`TestOwnedProcessStopFailureRemainsTrackedForRetry`→`TestOwnedSpawnStopFailureRemainsTrackedForRetry`、`TestPresentationResetProcessEntryMatchesItsIncrementalEvent`→`TestPresentationResetSpawnEntryMatchesItsIncrementalEvent`、`TestProcessSignalsUseCanonicalOrder`→`TestSpawnSignalsUseCanonicalOrder`、`TestProcessStepPrimaryTargetIsRejectedAtCompileTime`→`TestSpawnStepPrimaryTargetIsRejectedAtCompileTime`、`TestProcessStopIsUnifiedAndIdempotent`→`TestSpawnStopIsUnifiedAndIdempotent`、`TestProcessVisualCompilesAndEmitsLifecycle`→`TestSpawnVisualCompilesAndEmitsLifecycle`、`TestRuntimeCheckpointRestoresActiveProcesses`→`TestRuntimeCheckpointRestoresActiveSpawns`、`TestSummonProcessRejectsFieldsItNeverReads`→`TestSummonSpawnRejectsFieldsItNeverReads`、`TestSummonProcessWithoutDurationCompilesAndLivesForTheSpawnDuration`→`TestSummonSpawnWithoutDurationCompilesAndLivesForTheSpawnDuration`

</details>

### 3.8 文件

| 旧 | 新 |
| --- | --- |
| `skill/process.go`、`process_area.go`、`process_motion.go`、`process_numeric.go`、`process_owned.go`、`process_test.go` | `skill/spawn.go`、`spawn_area.go`、`spawn_motion.go`、`spawn_numeric.go`、`spawn_owned.go`、`spawn_test.go` |
| `skill/host_process.go`、`memory_host_process.go`、`program_process.go`、`runtime_owned_process.go` | `skill/host_spawn.go`、`memory_host_spawn.go`、`program_spawn.go`、`runtime_owned_spawn.go` |
| `skill/runtime_process_stop_retry.go` 与 `_promises_test.go` | `skill/runtime_spawn_stop_retry.go` 与 `_promises_test.go` |
| `skill/runtime_process_retention_promises_test.go` | `skill/runtime_spawn_retention_promises_test.go` |
| `skill/compile_summon_process_promises_test.go`、`effect_result_branch_process_promises_test.go` | `skill/compile_summon_spawn_promises_test.go`、`effect_result_branch_spawn_promises_test.go` |
| `skill/skillsync/presentation_reset_process_promises_test.go` | `skill/skillsync/presentation_reset_spawn_promises_test.go` |

## 4. 没有改的，以及理由

### 4.1 `proc` 不是 process

`ProcLedger`、`procLedger`、`MaxProcLedgerEntries`、`proc_ledger`、`proc_policy`、`ProcPlan`、`ProcDepth`、`MaxProcDepth`、`runtime_proc.go`、`compile_proc.go`、`passive_proc_guard.json` 等里的 **proc 是游戏术语“触发”（被动技能按事件触发，once-per-root-event 账本）**，与衍生物无关，不是 process 的缩写。任务说明把 `ProcLedger` 列进改名范围，核对源码后不改：改成 `SpawnLedger` 会把被动触发账本错叫成衍生物账本。全仓 skill 包内没有把 process 缩写成 proc 的标识符（`rg -w 'procs?'` 只命中触发语义）。

### 4.2 改名后 `rg -n -i 'process' skill docs/skill` 剩下的每一处

| 位置 | 原文 | 理由 |
| --- | --- | --- |
| `skill/combatcomponent/adapter.go:69` | “the business host must process it” | 动词“处理” |
| `skill/combatcomponent/status_bridge.go:53` | “the caller must process it” | 动词“处理” |
| `skill/skillsync/schema.go:100` | “after process startup” | 操作系统进程 |
| `skill/integration/sync-e2e/e2e_test.go:118` | “as a new process would” | 操作系统进程（模拟重启） |
| `docs/skill/production-readiness.md:93` | “in-process publication” | 进程内（操作系统进程） |
| `docs/skill/visual-sync-production-guide.md:407` | “Trace/Process/Status 兼容 API” | v1 已删除的旧 API 名，历史描述 |
| `docs/skill/production-readiness.md:31-33`、`skill/runtime_checkpoint.go:18-22` | 版本说明与本文件链接 | 新旧名对照本身 |

中文“进程”留下的是操作系统进程：`docs/skill/visual-sync-production-guide.md:165`（进程退出前保存）与 `:291`（进程重启恢复）、`docs/skill/skill-implementation-guide.md:325`（进程可重启）、`:397`（进程内占用）、`:469`（进程 kill -9）、`docs/skill/breaking-upgrade-skill-package.md:9`（同一进程出现两套包）、`skill/skillsync/file_outbox_tmp_cleanup_promises_test.go:13`（进程崩溃）。

### 4.3 文档

- 历史记录不改：`docs/bug`、`docs/bugfix`、`docs/review`、`docs/history`，以及 `docs/feature` 里已有的方案 / 实施记录（如 SKILL-EVAL-CONTEXT-TABLE、ROUND12-SKILL-CFGGEN、B3-SKILL-LOWER-FAILFAST），它们描述当时的代码；CHANGELOG 已有条目与交接 §7 已有行同理。查旧名用本文 §3 与 CHANGELOG 的对照。
- `docs/release/v1.23.0/*` 不改，由汇总者按新名统一处理。
- TROUBLESHOOTING T-271 / T-265 的现象列保留当时的报错原文，处置列补一句新名。

### 4.4 命名上仍要留意的相近名字（本次不改）

owned entity 的 `spawn` 效果 API 本来就叫 spawn：`SpawnCommand`（spawn 效果的宿主命令）、`SpawnEffectResult`、`OwnedSpawnPreview` / `PreviewOwnedSpawn` / `CommitOwnedSpawn` / `RollbackOwnedSpawn`（生成实体的事务）、`OwnedEntitySnapshot.SpawnTick` / `SpawnSequence`（实体生成时刻）、选择排序 `spawn_tick` / `spawn_sequence`。改名后与衍生物的 `SpawnCommandMeta`（`SpawnStepCommand` / `SpawnStopCommand` 的元数据，不是 `SpawnCommand` 的）、`OwnedSpawns` / `OwnedSpawnSnapshot`（移交给实体的衍生物）读起来相近。编译器上没有冲突（改名前核对过：含 process 的标识符改名后只有局部变量 `process`→`spawn` 与既有局部变量同名，涉及的作用域逐个看过，都是不同作用域或选择器访问）。是否再给实体生成那一侧换名，留给维护者。

## 5. 验证（2026-10-07，`GOWORK=off`，本机 darwin，worktree 基线 `3285d354`）

| 命令 | 结果 |
| --- | --- |
| `gofmt -l <改动的 .go>` | 空 |
| `go vet ./skill/...` | 通过 |
| `go test -race -count=3 ./skill/...`（含 `compile_mutation_property_test.go` 等性质测试） | skill 59.7s、combat、combatcomponent、skillcompose、skillsync 全部 ok |
| `go test -run '^$' -fuzz '^FuzzParseGeneratedNeverPanics$' -fuzztime 20s ./skill/`、同样跑 `FuzzRestoreRuntimeCheckpointNeverPanics` | 两个都 PASS |
| `skill/examples`、`skill/integration/sync-e2e` 两个独立模块（replace 到本仓）`go vet ./... && go test ./...` | 通过（sync-e2e ok） |
| 根包 `go test -count=1 .` | ok；`-v` 核对 `TestExamplesRun`（含 skill/examples 三个示例实跑）与 `TestTrackedMarkdownRelativeLinksResolve` 均 PASS |
| `go build ./... && go vet ./...` | 通过 |
| `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`；`go test ./cmd/glsvet/...`（含读 skill 目录的 componentfields 守卫） | 无违例；ok |
| digest 对比：基线与改名后各编译 36 个 testdata 定义打印 identity | gameplay / presentation digest 36 个全部变化（§3.4）；source document digest 36 个里 35 个变化（规范化文档按新键名编码） |
| 旧定义解析：基线的 `area_heal.json` / `dynamic_numeric.json` 交给改名后的 `skill.Parse` | `phases: [0].on: [0]: json: unknown field "process"`（T-288 的现象） |

没跑的：codegen 测试、`go generate ./...` 与 game-demo 重新生成——codegen 与 demo 模板不引用这些名字（`rg` 核对：demo 唯一的技能 `fireball.json.tmpl` 不带 spawn 定义，Go 模板只用 `skills.CompileAll(skill.DefaultCompileEnvironment())`），skill 包也没有 `go:generate`。改名不涉及 nest / entity / dataengine / sync，glsvet 按要求跑了一次作确认。

## 6. 实施状态

已实施，未发版（分支 `spawnrename`）。行为不变：除改名外没有改测试断言；变化是名字、checkpoint 版本号 3 → 4，以及随字段名变化的 digest 值。

未做 / 留给维护者：
- §4.4 owned entity 生成 API（`SpawnCommand`、`OwnedSpawnPreview` 等）与衍生物名字相近，是否给实体生成那一侧另换名。
- 停止入口统一到一个状态机（维护者待定），本次只改名。
- `docs/release/v1.23.0/*` 由汇总者按本文 §3 统一处理。
