# 游戏技能、战斗与空间：实现与维护

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）；源码目录最初按 v1.24.0 提取、之后随改动维护，链接由根包文档门禁保证存在，缺漏以源码为准。[设计与使用](../guide/08-skill.md)

## 如何阅读

这里的Skill指游戏中的火球、治疗、召唤等技能。框架把技能定义编译成执行计划，游戏世界通过Host接口提供实际能力。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`gameplay/skill`、`gameplay/attribute`、`infra/base/spatial`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. Runtime 与 DAO 回滚域分离。
2. Spawn 与 Summon 生命周期不同，能力声明与包装转发一致。
3. 表现流有序、隐私由 projector 保证；checkpoint 旧版拒绝。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### attribute

2 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [attribute.go](../../../gameplay/attribute/attribute.go) | `AttrID`、`AttrValue`、`Meta`、`Profile` |
| [container.go](../../../gameplay/attribute/container.go) | `Selector`、`Snapshot`、`Container` |

### skill

155 个实现文件、54 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [canonical_definition.go](../../../gameplay/skill/canonical_definition.go) | 函数/方法或内部实现；见源码 |
| [compile.go](../../../gameplay/skill/compile.go) | 函数/方法或内部实现；见源码 |
| [compile_ability.go](../../../gameplay/skill/compile_ability.go) | `AbilityPropertyHandle`、`AbilityReadPlan` |
| [compile_authority.go](../../../gameplay/skill/compile_authority.go) | `AuthorityIdentity`、`AuthorityProvider` |
| [compile_budget.go](../../../gameplay/skill/compile_budget.go) | 函数/方法或内部实现；见源码 |
| [compile_capability.go](../../../gameplay/skill/compile_capability.go) | 函数/方法或内部实现；见源码 |
| [compile_context.go](../../../gameplay/skill/compile_context.go) | `InputLayout`、`PhaseGraph`、`RandomSite`、`ComputedLimits`、`AttributeReadPlan`、`FilterPlan`、`ProcPlan` |
| [compile_effect_result.go](../../../gameplay/skill/compile_effect_result.go) | 函数/方法或内部实现；见源码 |
| [compile_environment.go](../../../gameplay/skill/compile_environment.go) | `AttributeHandle`、`ResourceHandle`、`StatusHandle`、`UnitTemplateHandle`、`CollisionLayerHandle`、`DamageTypeHandle`、`ElementHandle`、`GameplayTagHandle`、`SharedStateHandle`、`TemporalProfileHandle`、`AttributeCatalog`、`AttributeCatalogEntry`、`ResourceCatalog`、`ResourceCatalogEntry`、`StatusCatalog`、`StatusCatalogEntry`、`StatusAttributeModifier`、`UnitTemplateCatalog`、`UnitTemplateCatalogEntry`、`UnitTemplateAttributeOverridePolicy`、`UnitTemplateParameterPolicy`、`CollisionLayerCatalog`、`CollisionLayerCatalogEntry`、`DamageTypeCatalog`、`DamageTypeCatalogEntry`、`ElementCatalog`、`ElementCatalogEntry`、`GameplayTagClass`、`GameplayTagCatalog`、`GameplayTagCatalogEntry`、`CombatPolicyCatalog`、`SharedStateCatalog`、`SharedStateCatalogEntry`、`AbilityControlCatalog`、`AbilityPropertyPolicy`、`TemporalSnapshotCatalog`、`TemporalSnapshotProfile`、`GameplayCatalog`、`MotionCapabilityCatalog`、`MotionSpawnTrajectoryPair`、`MotionVariantCapability`、`SpawnPropertyCatalog`、`SpawnPropertyHandle`、`SpawnPropertySlotBinding`、`SpawnPropertyPolicy`、`VisualCatalog`、`VisualLimits`、`VisualElementDescriptor`、`VisualCategoryDescriptor`、`VisualThemeDescriptor`、`NumericAuthority`、`CompileLimits`、`CompileEnvironment` |
| [compile_graph.go](../../../gameplay/skill/compile_graph.go) | 函数/方法或内部实现；见源码 |
| [compile_host_capability.go](../../../gameplay/skill/compile_host_capability.go) | 函数/方法或内部实现；见源码 |
| [compile_identity.go](../../../gameplay/skill/compile_identity.go) | 函数/方法或内部实现；见源码 |
| [compile_input.go](../../../gameplay/skill/compile_input.go) | 函数/方法或内部实现；见源码 |
| [compile_lifetime.go](../../../gameplay/skill/compile_lifetime.go) | 函数/方法或内部实现；见源码 |
| [compile_memory.go](../../../gameplay/skill/compile_memory.go) | 函数/方法或内部实现；见源码 |
| [compile_motion.go](../../../gameplay/skill/compile_motion.go) | 函数/方法或内部实现；见源码 |
| [compile_normalize.go](../../../gameplay/skill/compile_normalize.go) | 函数/方法或内部实现；见源码 |
| [compile_optional.go](../../../gameplay/skill/compile_optional.go) | 函数/方法或内部实现；见源码 |
| [compile_owned_entity.go](../../../gameplay/skill/compile_owned_entity.go) | 函数/方法或内部实现；见源码 |
| [compile_proc.go](../../../gameplay/skill/compile_proc.go) | 函数/方法或内部实现；见源码 |
| [compile_quantity.go](../../../gameplay/skill/compile_quantity.go) | 函数/方法或内部实现；见源码 |
| [compile_random.go](../../../gameplay/skill/compile_random.go) | 函数/方法或内部实现；见源码 |
| [compile_shape.go](../../../gameplay/skill/compile_shape.go) | 函数/方法或内部实现；见源码 |
| [compile_snapshot.go](../../../gameplay/skill/compile_snapshot.go) | 函数/方法或内部实现；见源码 |
| [compile_state.go](../../../gameplay/skill/compile_state.go) | 函数/方法或内部实现；见源码 |
| [compile_status.go](../../../gameplay/skill/compile_status.go) | 函数/方法或内部实现；见源码 |
| [compile_tags.go](../../../gameplay/skill/compile_tags.go) | 函数/方法或内部实现；见源码 |
| [compile_temporal.go](../../../gameplay/skill/compile_temporal.go) | 函数/方法或内部实现；见源码 |
| [compile_typecheck.go](../../../gameplay/skill/compile_typecheck.go) | 函数/方法或内部实现；见源码 |
| [compile_visual.go](../../../gameplay/skill/compile_visual.go) | 函数/方法或内部实现；见源码 |
| [diagnostic.go](../../../gameplay/skill/diagnostic.go) | `DiagnosticSeverity`、`Severity`、`DiagnosticCode`、`Diagnostic` |
| [doc.go](../../../gameplay/skill/doc.go) | 函数/方法或内部实现；见源码 |
| [environment_kinds.go](../../../gameplay/skill/environment_kinds.go) | 函数/方法或内部实现；见源码 |
| [eval_contexts.go](../../../gameplay/skill/eval_contexts.go) | 函数/方法或内部实现；见源码 |
| [executor.go](../../../gameplay/skill/executor.go) | 函数/方法或内部实现；见源码 |
| [fixed_math.go](../../../gameplay/skill/fixed_math.go) | 函数/方法或内部实现；见源码 |
| [generated.go](../../../gameplay/skill/generated.go) | `GeneratedResult`、`Rejection`、`RejectionError` |
| [host.go](../../../gameplay/skill/host.go) | `Host`、`HostEventCompactor`、`InputPositionResolver`、`InputPositionRequest`、`AbilityRelationProvider`、`ReadRequest`、`ReadPayload`、`ResourceRead`、`PositionRead`、`AttributeRead`、`ReadResult`、`SelectRequest`、`SelectResult`、`SelectShape`、`SingleSelectShape`、`CircleSelectShape`、`RingSelectShape`、`ConeSelectShape`、`LineSelectShape`、`RectangleSelectShape`、`RaycastSelectShape`、`ChainSelectShape`、`PathSelectShape`、`NearestValidSelectShape`、`SelectFilter`、`AliveSelectFilter`、`NotCasterSelectFilter`、`RelationSelectFilter`、`StatusSelectFilter`、`AttributeSelectFilter`、`VisibleSelectFilter`、`TargetableSelectFilter`、`LineOfSightSelectFilter`、`GameplayTagSelectFilter`、`SelectOrderBy`、`SelectDirection`、`SelectOrder` |
| [host_capability.go](../../../gameplay/skill/host_capability.go) | `HostCapabilityCatalog`、`HostCapabilityTable`、`HostCapabilityProvider`、`HostCapabilityKind`、`HostCapability` |
| [host_capability_check.go](../../../gameplay/skill/host_capability_check.go) | `HostCapabilityProbe` |
| [host_combat.go](../../../gameplay/skill/host_combat.go) | `DamageCommand`、`DamageResult`、`DamageEffectResult`、`HealCommand`、`HealResult`、`HealEffectResult`、`ShieldCommand`、`ShieldResult`、`ShieldEffectResult`、`StatusCommand`、`RemoveStatusCommand`、`DispelStatusCommand`、`StatusResult`、`StatusEffectResult`、`AttributeModifierCommand`、`AttributeModifierResult`、`AttributeModifierEffectResult` |
| [host_command.go](../../../gameplay/skill/host_command.go) | `EffectCommand`、`EffectCommandPayload`、`TeleportCommand`、`KnockbackCommand`、`PullCommand`、`ResourceCommand`、`SummonCommand`、`SummonAttributeOverride`、`SummonParameterBinding`、`EffectResult`、`ExpectedFailureReason`、`ResultOutcome`、`TeleportEffectResult`、`SummonEffectResult`、`StateChangeEffectResult`、`AbilityChangeEffectResult`、`EntityCommandEffectResult`、`SnapshotCaptureEffectResult`、`SnapshotRestoreEffectResult`、`EffectResultPayload`、`CostEntry`、`CostPayment` |
| [host_event.go](../../../gameplay/skill/host_event.go) | 函数/方法或内部实现；见源码 |
| [host_owned_entity.go](../../../gameplay/skill/host_owned_entity.go) | `OwnedEntityMetadata`、`OwnedEntityCommand`、`OwnedEntitiesSelectShape`、`OwnedSourceSkillFilter`、`OwnedSourceCastFilter`、`OwnedUnitTemplateFilter`、`OwnedEntityTagFilter`、`OwnedSummonTickFilter`、`OwnedSummonPreview`、`OwnedSummonTransactionID`、`OwnedEntityRuntimeHost` |
| [host_revision.go](../../../gameplay/skill/host_revision.go) | `WorldRevision`、`EventCursor`、`SpawnID`、`QueryMeta`、`QueryResultMeta`、`CommandMeta`、`CommitReceipt`、`RuntimeEvent` |
| [host_spawn.go](../../../gameplay/skill/host_spawn.go) | `SpawnCommandMeta`、`MotionStep`、`StaticMotionStep`、`FrameMotionStep`、`SteeringMotionStep`、`TrajectoryMotionStep`、`OffsetsMotionStep`、`CollisionMotionStep`、`CarryMotionStep`、`CompletionMotionStep`、`SignalsMotionStep`、`SpawnNumericSnapshot`、`SpawnStepCommand`、`SpawnStopCommand`、`SpawnHostState`、`SpawnStepResult` |
| [host_state.go](../../../gameplay/skill/host_state.go) | `StateSlot`、`StateHandle`、`StateScope`、`StateScopeBinding`、`StateReadRequest`、`StateReadResult`、`StateMutationCommand`、`StateMutationResult`、`StateChangeEvent`、`StateStore` |
| [host_status.go](../../../gameplay/skill/host_status.go) | `StatusInstanceID`、`StatusInstanceRef`、`StatusSetSelectShape`、`StatusIDSelectFilter`、`StatusTextSelectFilter`、`StatusFlagSelectFilter`、`StatusEntitySelectFilter`、`StatusSourceSkillSelectFilter`、`StatusCompareSelectFilter`、`ModifyStatusInstanceCommand` |
| [host_temporal.go](../../../gameplay/skill/host_temporal.go) | `TemporalCaptureCommand`、`TemporalRestoreCommand` |
| [host_value.go](../../../gameplay/skill/host_value.go) | 函数/方法或内部实现；见源码 |
| [inspect.go](../../../gameplay/skill/inspect.go) | 函数/方法或内部实现；见源码 |
| [ir.go](../../../gameplay/skill/ir.go) | 函数/方法或内部实现；见源码 |
| [ir_ability.go](../../../gameplay/skill/ir_ability.go) | 函数/方法或内部实现；见源码 |
| [ir_combat.go](../../../gameplay/skill/ir_combat.go) | 函数/方法或内部实现；见源码 |
| [ir_effect.go](../../../gameplay/skill/ir_effect.go) | 函数/方法或内部实现；见源码 |
| [ir_effect_result.go](../../../gameplay/skill/ir_effect_result.go) | 函数/方法或内部实现；见源码 |
| [ir_event.go](../../../gameplay/skill/ir_event.go) | `EventID`、`CastID`、`EventContext` |
| [ir_flow.go](../../../gameplay/skill/ir_flow.go) | 函数/方法或内部实现；见源码 |
| [ir_input.go](../../../gameplay/skill/ir_input.go) | 函数/方法或内部实现；见源码 |
| [ir_motion.go](../../../gameplay/skill/ir_motion.go) | 函数/方法或内部实现；见源码 |
| [ir_numeric.go](../../../gameplay/skill/ir_numeric.go) | 函数/方法或内部实现；见源码 |
| [ir_owned_entity.go](../../../gameplay/skill/ir_owned_entity.go) | 函数/方法或内部实现；见源码 |
| [ir_select.go](../../../gameplay/skill/ir_select.go) | 函数/方法或内部实现；见源码 |
| [ir_state.go](../../../gameplay/skill/ir_state.go) | 函数/方法或内部实现；见源码 |
| [ir_status.go](../../../gameplay/skill/ir_status.go) | 函数/方法或内部实现；见源码 |
| [ir_temporal.go](../../../gameplay/skill/ir_temporal.go) | 函数/方法或内部实现；见源码 |
| [ir_visit.go](../../../gameplay/skill/ir_visit.go) | 函数/方法或内部实现；见源码 |
| [lower.go](../../../gameplay/skill/lower.go) | 函数/方法或内部实现；见源码 |
| [memory_host.go](../../../gameplay/skill/memory_host.go) | `MemoryEntity`、`MemoryHost`、`MemoryHostOptions` |
| [memory_host_combat.go](../../../gameplay/skill/memory_host_combat.go) | 函数/方法或内部实现；见源码 |
| [memory_host_effect.go](../../../gameplay/skill/memory_host_effect.go) | 函数/方法或内部实现；见源码 |
| [memory_host_motion.go](../../../gameplay/skill/memory_host_motion.go) | 函数/方法或内部实现；见源码 |
| [memory_host_owned_entity.go](../../../gameplay/skill/memory_host_owned_entity.go) | 函数/方法或内部实现；见源码 |
| [memory_host_select.go](../../../gameplay/skill/memory_host_select.go) | 函数/方法或内部实现；见源码 |
| [memory_host_spawn.go](../../../gameplay/skill/memory_host_spawn.go) | 函数/方法或内部实现；见源码 |
| [memory_host_state.go](../../../gameplay/skill/memory_host_state.go) | 函数/方法或内部实现；见源码 |
| [memory_host_status.go](../../../gameplay/skill/memory_host_status.go) | 函数/方法或内部实现；见源码 |
| [memory_host_temporal.go](../../../gameplay/skill/memory_host_temporal.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../gameplay/skill/parse.go) | `ParseLimits` |
| [parse_duplicate.go](../../../gameplay/skill/parse_duplicate.go) | 函数/方法或内部实现；见源码 |
| [phase_events.go](../../../gameplay/skill/phase_events.go) | 函数/方法或内部实现；见源码 |
| [presentation.go](../../../gameplay/skill/presentation.go) | `PresentationEventKind`、`PresentationAnchor`、`PresentationMount`、`PresentationPlan`、`PresentationEvent`、`PresentationBatch` |
| [presentation_asset_cache.go](../../../gameplay/skill/presentation_asset_cache.go) | `VisualCatalogTrust`、`TrustedVisualCatalogs`、`VisualAssetLoader`、`VisualPlanCacheOptions`、`VisualPlanCache`、`VisualPlanLease` |
| [presentation_assets.go](../../../gameplay/skill/presentation_assets.go) | `VisualAsset`、`VisualAssetResolver`、`ResolvedVisualAsset`、`ResolvedPresentationPlan` |
| [presentation_recovery.go](../../../gameplay/skill/presentation_recovery.go) | `ActivePresentationKind`、`ActivePresentation`、`PresentationRecoverySnapshot` |
| [program.go](../../../gameplay/skill/program.go) | `Program`、`ProgramView`、`CastWindowView`、`CostView`、`MemorySlotView`、`LocalSlotView`、`PhaseView`、`RootView`、`OperationView`、`MetricSnapshot`、`StateLayoutView` |
| [program_ability.go](../../../gameplay/skill/program_ability.go) | 函数/方法或内部实现；见源码 |
| [program_combat.go](../../../gameplay/skill/program_combat.go) | 函数/方法或内部实现；见源码 |
| [program_digest.go](../../../gameplay/skill/program_digest.go) | 函数/方法或内部实现；见源码 |
| [program_effect_result.go](../../../gameplay/skill/program_effect_result.go) | `ResultFieldHandle` |
| [program_identity.go](../../../gameplay/skill/program_identity.go) | `MemoryIndex`、`LocalIndex`、`PhaseIndex`、`RootIndex`、`SelectorIndex`、`OperationIndex`、`EffectIndex`、`SpawnTemplateIndex`、`VisualIndex`、`RandomSiteIndex`、`ProgramIdentityView` |
| [program_input.go](../../../gameplay/skill/program_input.go) | `InputPort`、`InputSlotView`、`InputProgramView`、`InputLayoutView` |
| [program_operation.go](../../../gameplay/skill/program_operation.go) | 函数/方法或内部实现；见源码 |
| [program_owned_entity.go](../../../gameplay/skill/program_owned_entity.go) | 函数/方法或内部实现；见源码 |
| [program_quantity.go](../../../gameplay/skill/program_quantity.go) | `QuantityView` |
| [program_random.go](../../../gameplay/skill/program_random.go) | `RandomSiteView` |
| [program_select.go](../../../gameplay/skill/program_select.go) | `SelectionView`、`EffectResultView` |
| [program_state.go](../../../gameplay/skill/program_state.go) | `PersistentStateView` |
| [program_status.go](../../../gameplay/skill/program_status.go) | 函数/方法或内部实现；见源码 |
| [program_temporal.go](../../../gameplay/skill/program_temporal.go) | 函数/方法或内部实现；见源码 |
| [program_visual.go](../../../gameplay/skill/program_visual.go) | `VisualView`、`SkillVisualManifest` |
| [quantity.go](../../../gameplay/skill/quantity.go) | 函数/方法或内部实现；见源码 |
| [replay.go](../../../gameplay/skill/replay.go) | `HostRecord`、`RecordingHost`、`ReplayHost` |
| [replay_optional.go](../../../gameplay/skill/replay_optional.go) | 函数/方法或内部实现；见源码 |
| [runtime.go](../../../gameplay/skill/runtime.go) | `RuntimeOptions`、`CastInput`、`InputPayload`、`CastStatus`、`CastWindowStage`、`CastSnapshot`、`Runtime` |
| [runtime_ability.go](../../../gameplay/skill/runtime_ability.go) | `AbilityRegistration`、`AbilityChangeResult`、`AbilityChangeEvent` |
| [runtime_cast.go](../../../gameplay/skill/runtime_cast.go) | 函数/方法或内部实现；见源码 |
| [runtime_cast_policy.go](../../../gameplay/skill/runtime_cast_policy.go) | 函数/方法或内部实现；见源码 |
| [runtime_cast_window.go](../../../gameplay/skill/runtime_cast_window.go) | 函数/方法或内部实现；见源码 |
| [runtime_checkpoint.go](../../../gameplay/skill/runtime_checkpoint.go) | `RuntimeCheckpoint`、`ProgramResolver`、`ProgramResolverFunc` |
| [runtime_dispatch.go](../../../gameplay/skill/runtime_dispatch.go) | 函数/方法或内部实现；见源码 |
| [runtime_effect_result.go](../../../gameplay/skill/runtime_effect_result.go) | `EffectResultEvent` |
| [runtime_eval.go](../../../gameplay/skill/runtime_eval.go) | 函数/方法或内部实现；见源码 |
| [runtime_event.go](../../../gameplay/skill/runtime_event.go) | 函数/方法或内部实现；见源码 |
| [runtime_host_capability.go](../../../gameplay/skill/runtime_host_capability.go) | 函数/方法或内部实现；见源码 |
| [runtime_input.go](../../../gameplay/skill/runtime_input.go) | 函数/方法或内部实现；见源码 |
| [runtime_mutation.go](../../../gameplay/skill/runtime_mutation.go) | `StateMutationKind`、`StateMutation`、`StateMutationBatch` |
| [runtime_owned_entity.go](../../../gameplay/skill/runtime_owned_entity.go) | 函数/方法或内部实现；见源码 |
| [runtime_proc.go](../../../gameplay/skill/runtime_proc.go) | `PassiveActivationID`、`AbilityHandle`、`PassiveCandidate`、`PassiveRouter` |
| [runtime_random.go](../../../gameplay/skill/runtime_random.go) | 函数/方法或内部实现；见源码 |
| [runtime_retention.go](../../../gameplay/skill/runtime_retention.go) | `RuntimeRetentionStats` |
| [runtime_select.go](../../../gameplay/skill/runtime_select.go) | 函数/方法或内部实现；见源码 |
| [runtime_spawn_stop.go](../../../gameplay/skill/runtime_spawn_stop.go) | 函数/方法或内部实现；见源码 |
| [runtime_state.go](../../../gameplay/skill/runtime_state.go) | 函数/方法或内部实现；见源码 |
| [runtime_sync.go](../../../gameplay/skill/runtime_sync.go) | `StateEvent`、`StateEventBatch`、`CastStateSnapshot`、`CooldownStateSnapshot`、`SkillResourceSnapshot`、`AbilityOverlaySnapshot`、`AbilityStateSnapshot`、`NumericPropertySnapshot`、`SpawnStateSnapshot`、`ActivePolicySnapshot`、`PersistentStateSnapshot`、`RuntimeStateExtensionProvider`、`RuntimeStateSnapshot` |
| [runtime_temporal.go](../../../gameplay/skill/runtime_temporal.go) | 函数/方法或内部实现；见源码 |
| [runtime_turn.go](../../../gameplay/skill/runtime_turn.go) | 函数/方法或内部实现；见源码 |
| [runtime_value.go](../../../gameplay/skill/runtime_value.go) | `EntityID`、`SnapshotToken`、`Position`、`Direction`、`Hit`、`AbilityRef`、`RuntimeValue`、`Selection`、`SelectionQueryMeta` |
| [runtime_value_json.go](../../../gameplay/skill/runtime_value_json.go) | 函数/方法或内部实现；见源码 |
| [runtime_value_redaction.go](../../../gameplay/skill/runtime_value_redaction.go) | `RuntimeValueRedactionOptions` |
| [scheduler.go](../../../gameplay/skill/scheduler.go) | `FrameID` |
| [spawn.go](../../../gameplay/skill/spawn.go) | `SpawnStatus`、`SpawnScope`、`StopCause`、`MotionSpawnStage`、`MotionState`、`SpawnNumericState`、`SpawnInstance`、`SpawnSignalKind`、`SpawnSignal` |
| [spawn_area.go](../../../gameplay/skill/spawn_area.go) | `AreaMemberState` |
| [spawn_motion.go](../../../gameplay/skill/spawn_motion.go) | 函数/方法或内部实现；见源码 |
| [spawn_numeric.go](../../../gameplay/skill/spawn_numeric.go) | 函数/方法或内部实现；见源码 |
| [spawn_owned.go](../../../gameplay/skill/spawn_owned.go) | `OwnedSpawnSnapshot` |
| [spawn_table.go](../../../gameplay/skill/spawn_table.go) | 函数/方法或内部实现；见源码 |
| [trace.go](../../../gameplay/skill/trace.go) | `TraceSink`、`TraceLimits`、`TraceEvent`、`TraceEventKind` |
| [value.go](../../../gameplay/skill/value.go) | `Value`、`ValueDefinition`、`NullValueDefinition`、`IntValueDefinition`、`BoolValueDefinition`、`StringValueDefinition`、`ReferenceValueDefinition`、`ExpressionValueDefinition`、`AttributeReadValueDefinition`、`StateReadValueDefinition`、`AbilityStateReadValueDefinition` |
| [value_optional.go](../../../gameplay/skill/value_optional.go) | 函数/方法或内部实现；见源码 |
| [wire_activation.go](../../../gameplay/skill/wire_activation.go) | `ActivationDefinition`、`ActiveActivationDefinition`、`PassiveActivationDefinition`、`EventFilterDefinition`、`ProcPolicyDefinition`、`CastPolicyDefinition`、`TapPolicyDefinition`、`TogglePolicyDefinition`、`ChargePolicyDefinition`、`AmmoPolicyDefinition`、`HoldPolicyDefinition` |
| [wire_cast_window.go](../../../gameplay/skill/wire_cast_window.go) | `CastWindowDefinition` |
| [wire_combat.go](../../../gameplay/skill/wire_combat.go) | 函数/方法或内部实现；见源码 |
| [wire_definition.go](../../../gameplay/skill/wire_definition.go) | `Tick`、`Definition`、`Cost`、`MemoryDeclaration`、`PhaseDefinition`、`PhaseEventsDefinition` |
| [wire_effect.go](../../../gameplay/skill/wire_effect.go) | `EffectDefinition`、`DamageEffectDefinition`、`HealEffectDefinition`、`ShieldEffectDefinition`、`AddStatusEffectDefinition`、`RemoveStatusEffectDefinition`、`ModifyStatusInstanceEffectDefinition`、`AttributeModifierEffectDefinition`、`ResourceEffectDefinition`、`SetMemoryEffectDefinition`、`AddMemoryEffectDefinition`、`ClearMemoryEffectDefinition`、`TeleportEffectDefinition`、`KnockbackEffectDefinition`、`PullEffectDefinition`、`StopMovementEffectDefinition`、`ModifyStateEffectDefinition`、`ModifyAbilityStateEffectDefinition`、`NumericTrackDefinition`、`ModifySpawnEffectDefinition`、`SummonEffectDefinition`、`DismissEffectDefinition`、`IssueEntityCommandEffectDefinition` |
| [wire_effect_result.go](../../../gameplay/skill/wire_effect_result.go) | `EffectResultDefinition` |
| [wire_flow.go](../../../gameplay/skill/wire_flow.go) | `FlowDefinition`、`SequenceFlowDefinition`、`ParallelFlowDefinition`、`IfFlowDefinition`、`RepeatFlowDefinition`、`WaitFlowDefinition`、`SelectFlowDefinition`、`EffectFlowDefinition`、`GotoFlowDefinition`、`FinishFlowDefinition`、`SpawnCallbacksDefinition` |
| [wire_input.go](../../../gameplay/skill/wire_input.go) | `InputSchemaDefinition`、`NoneInputSchemaDefinition`、`DirectionInputSchemaDefinition`、`PositionInputSchemaDefinition`、`EntityInputSchemaDefinition`、`DirectionPositionInputSchemaDefinition`、`EntityPositionInputSchemaDefinition`、`TwoPointInputSchemaDefinition`、`DragInputSchemaDefinition`、`PathInputSchemaDefinition` |
| [wire_motion.go](../../../gameplay/skill/wire_motion.go) | `SpawnDefinition`、`MotionDefinition`、`FrameDefinition`、`WorldFrameDefinition`、`FollowFrameDefinition`、`SteeringDefinition`、`TrackingSteeringDefinition`、`TrajectoryDefinition`、`StationaryTrajectoryDefinition`、`LinearTrajectoryDefinition`、`PathTrajectoryDefinition`、`OrbitTrajectoryDefinition`、`ParabolaTrajectoryDefinition`、`OffsetDefinition`、`ZigzagOffsetDefinition`、`CircularOffsetDefinition`、`MotionCollisionDefinition`、`CarryDefinition`、`CompletionDefinition`、`EndCompletionDefinition`、`PauseThenEndCompletionDefinition`、`BoomerangCompletionDefinition` |
| [wire_select.go](../../../gameplay/skill/wire_select.go) | `SelectDefinition`、`SelectOrderDefinition`、`SelectConsumeDefinition`、`SelectOneConsumeDefinition`、`SelectEachConsumeDefinition`、`ShapeDefinition`、`SingleShapeDefinition`、`CircleShapeDefinition`、`RingShapeDefinition`、`ConeShapeDefinition`、`LineShapeDefinition`、`RectangleShapeDefinition`、`RaycastShapeDefinition`、`ChainShapeDefinition`、`PathShapeDefinition`、`NearestValidShapeDefinition`、`AbilitySetShapeDefinition`、`StatusSetShapeDefinition`、`OwnedEntitiesShapeDefinition`、`FilterDefinition`、`FlagFilterDefinition`、`RelationFilterDefinition`、`StatusFilterDefinition`、`AttributeCompareFilterDefinition`、`GameplayTagFilterDefinition`、`LineOfSightFilterDefinition`、`AbilityTagFilterDefinition`、`AbilitySlotFilterDefinition`、`OwnedSourceSkillFilterDefinition`、`OwnedSourceCastFilterDefinition`、`OwnedSummonTickFilterDefinition`、`OwnedUnitTemplateFilterDefinition`、`OwnedEntityTagFilterDefinition`、`StatusInstanceFilterDefinition` |
| [wire_state.go](../../../gameplay/skill/wire_state.go) | `PersistentStateDefinition`、`StateLifetimeDefinition` |
| [wire_temporal.go](../../../gameplay/skill/wire_temporal.go) | `CaptureSnapshotEffectDefinition`、`RestoreSnapshotEffectDefinition` |
| [wire_visual.go](../../../gameplay/skill/wire_visual.go) | `SkillPresentation`、`VisualRef` |

### skill/combat

5 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [attributes.go](../../../gameplay/skill/combat/attributes.go) | `AttributeID`、`ModifierHandle`、`Modifier`、`AttributeBounds`、`AttributeSet`、`AttributeValue`、`AttributeBaseState` |
| [buffs.go](../../../gameplay/skill/combat/buffs.go) | `BuffID`、`BuffInstanceID`、`Tag`、`BuffStackPolicy`、`BuffSpec`、`BuffApplyOutcome`、`BuffInstance`、`BuffContainer`、`BuffContainerState` |
| [combat.go](../../../gameplay/skill/combat/combat.go) | 函数/方法或内部实现；见源码 |
| [damage.go](../../../gameplay/skill/combat/damage.go) | `DamageType`、`Element`、`Combatant`、`DamageInput`、`DamageOutcome`、`Hooks`、`HealOutcome` |
| [roll.go](../../../gameplay/skill/combat/roll.go) | 函数/方法或内部实现；见源码 |

### skill/combatcomponent

3 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [adapter.go](../../../gameplay/skill/combatcomponent/adapter.go) | `Resolver`、`RevisionSource`、`EffectEvent`、`HostAdapter` |
| [component.go](../../../gameplay/skill/combatcomponent/component.go) | `CombatDao`、`CombatComponent`、`AttributeProjection` |
| [status_bridge.go](../../../gameplay/skill/combatcomponent/status_bridge.go) | `StatusBridge` |
| [state_bridge.go](../../../gameplay/skill/combatcomponent/state_bridge.go) | `StateBinding`（`BindStates`、`HasBuffTag`：buff 标签 → `gameplay/state` 派生状态，D21 第 3 期，契约见 [impl/10-time §2D](10-time.md#2d-行为控制agent取代冲突冻结与状态) 第 9 条） |

### skill/examples/combat

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../gameplay/skill/examples/combat/main.go) | 函数/方法或内部实现；见源码 |

### skill/examples/fireball

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../gameplay/skill/examples/fireball/main.go) | 函数/方法或内部实现；见源码 |

### skill/examples/statusbridge

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../gameplay/skill/examples/statusbridge/main.go) | 函数/方法或内部实现；见源码 |

### skill/skillcompose

15 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [canonical.go](../../../gameplay/skill/skillcompose/canonical.go) | 函数/方法或内部实现；见源码 |
| [catalog.go](../../../gameplay/skill/skillcompose/catalog.go) | `FeatureKey`、`FeatureKind`、`TransformKind`、`FeatureDescriptor`、`Catalog` |
| [contract.go](../../../gameplay/skill/skillcompose/contract.go) | `SkillCompositionContract`、`SourceGrant`、`SourceObligation`、`GenericPackageGrant`、`Constraint` |
| [contract_builder.go](../../../gameplay/skill/skillcompose/contract_builder.go) | 函数/方法或内部实现；见源码 |
| [contract_validate.go](../../../gameplay/skill/skillcompose/contract_validate.go) | 函数/方法或内部实现；见源码 |
| [diagnostic.go](../../../gameplay/skill/skillcompose/diagnostic.go) | `Diagnostic` |
| [doc.go](../../../gameplay/skill/skillcompose/doc.go) | 函数/方法或内部实现；见源码 |
| [graph.go](../../../gameplay/skill/skillcompose/graph.go) | `CausalGraph` |
| [matcher.go](../../../gameplay/skill/skillcompose/matcher.go) | `Match` |
| [metric.go](../../../gameplay/skill/skillcompose/metric.go) | 函数/方法或内部实现；见源码 |
| [policy.go](../../../gameplay/skill/skillcompose/policy.go) | `CompositionPolicy`、`CallerPolicy`、`PolicyIdentity`、`SourceIdentity`、`CompositionBudgets` |
| [profile.go](../../../gameplay/skill/skillcompose/profile.go) | `SkillProfile`、`FeatureOrigin`、`SelectionFact`、`Metrics` |
| [profile_extract.go](../../../gameplay/skill/skillcompose/profile_extract.go) | 函数/方法或内部实现；见源码 |
| [prompt_view.go](../../../gameplay/skill/skillcompose/prompt_view.go) | `PromptContractView` |
| [validator.go](../../../gameplay/skill/skillcompose/validator.go) | `ValidationReport` |

### skill/skillsync

10 个实现文件、8 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [applier.go](../../../gameplay/skill/skillsync/applier.go) | `ApplyTransaction`、`ManifestConsumer`、`TransactionalManifestConsumer`、`StateConsumer`、`TransactionalStateConsumer`、`PresentationConsumer`、`TransactionalPresentationConsumer`、`ApplierOptions`、`ApplyResult`、`Applier` |
| [coordinator.go](../../../gameplay/skill/skillsync/coordinator.go) | `PacketPublisher`、`VisibilityPolicy`、`AllowAllVisibility`、`CoordinatorOptions`、`CoordinatorMetrics`、`Coordinator` |
| [file_outbox.go](../../../gameplay/skill/skillsync/file_outbox.go) | `FileOutboxStore`、`FileOutboxOptions` |
| [file_replace_unix.go](../../../gameplay/skill/skillsync/file_replace_unix.go) | 函数/方法或内部实现；见源码 |
| [file_replace_windows.go](../../../gameplay/skill/skillsync/file_replace_windows.go) | 函数/方法或内部实现；见源码 |
| [observability.go](../../../gameplay/skill/skillsync/observability.go) | `MetricSink`、`HealthOptions`、`HealthStatus` |
| [outbox.go](../../../gameplay/skill/skillsync/outbox.go) | `OutboxStore`、`OutboxDelete`、`BatchOutboxStore`、`OutboxRecord`、`RecordOutboxStore`、`BatchRecordOutboxStore`、`OutboxOptions`、`OutboxMetrics`、`Outbox` |
| [schema.go](../../../gameplay/skill/skillsync/schema.go) | `SchemaRange`、`SchemaMigrator`、`SchemaMigration`、`SchemaRegistry` |
| [skillsync.go](../../../gameplay/skill/skillsync/skillsync.go) | `RecordKind`、`Header`、`ManifestRecord`、`StateSnapshot`、`StateRecord`、`PresentationReset`、`PresentationRecord`、`Projector` |
| [visibility.go](../../../gameplay/skill/skillsync/visibility.go) | `EntityVisibilityEvaluator`、`VisibilityField`、`FieldVisibilityEvaluator`、`EntityVisibilityPolicy` |

### spatial

4 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [block_index.go](../../../infra/base/spatial/block_index.go) | `BlockIndex` |
| [geometry.go](../../../infra/base/spatial/geometry.go) | `Point`、`Rect` |
| [pathfind.go](../../../infra/base/spatial/pathfind.go) | `Terrain`、`PathOptions` |
| [terrain.go](../../../infra/base/spatial/terrain.go) | `GridTerrain` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Host`、`Test.*Spawn`、`Test.*Checkpoint`、`Test.*Combat`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### attribute

- [container_test.go](../../../gameplay/attribute/container_test.go)：`TestSnapshotHandsOutACopy`、`TestEmptyLayerAnswersInsteadOfPanicking`、`TestZeroSelectorIsBase`、`TestLayersAreIsolated`、`TestSnapshotCopiesUnderTheContainerLock`

### skill

- [ability_test.go](../../../gameplay/skill/ability_test.go)：`TestAbilityStateCooldownMutationClampsAndPreservesCast`、`TestAbilityStateReadAndMutationExecuteFromDSL`、`TestAbilityStateRejectsReadOnlyMutation`、`TestAbilityDisableOverlaysExpireIndependentlyAndDoNotCancelCast`、`TestAbilityAmmoMutationPreservesRechargeTimeline`；其余 9 项见文件
- [acceptance_test.go](../../../gameplay/skill/acceptance_test.go)：`TestAllFixturesParseCompileInspectAndRun`
- [area_test.go](../../../gameplay/skill/area_test.go)：`TestTargetValidity`、`TestAreaMembership`、`TestAreaMembershipRotatingMembersRemainBounded`、`TestAreaCallbackFinishStopsRemainingSignals`、`TestAreaFinalLeaveFinishSuppressesTerminalCallback`；其余 2 项见文件
- [cast_retention_test.go](../../../gameplay/skill/cast_retention_test.go)：`TestUncommittedFailedCastsStayWithinTheCompletedCastLimit`、`TestCheckpointCarriesTheCompletionOrderSeparately`、`TestRestoreCompletedCastOrderPrefersTheRecordedOrder`、`TestRestoredCompletionOrderDecidesWhichCastRetentionEvicts`、`TestReferencedCompletedCastsSurviveTheRetentionBound`
- [cast_terminal_test.go](../../../gameplay/skill/cast_terminal_test.go)：`TestFailedStartLeavesNoScheduledWorkForTheReusedCastID`、`TestCheckpointAfterFailedStartRestores`、`TestCancelCallbackFailureStillEndsTheCast`、`TestChargeReleaseFailureStillEndsTheCast`、`TestFailedToggleReleasesItsPolicySlot`；其余 4 项见文件
- [cast_window_test.go](../../../gameplay/skill/cast_window_test.go)：`TestCastWindowUsesExactPrepareCommitExecuteRecoveryTimeline`、`TestCastWindowCancelBeforeCommitUsesRefundPolicy`、`TestCastWindowExpressionsAreClampedToDeclaredBounds`、`TestCastWindowExpressionShapeRules`、`TestExternallyAuthoredHasteAttributeDrivesWindup`；其余 2 项见文件
- [checkpoint_test.go](../../../gameplay/skill/checkpoint_test.go)：`TestRuntimeCheckpointBytesAreDeterministic`、`TestRuntimeRestoresUnsortedLegacyCheckpointLists`、`TestCheckpointRestoreRejectsPhaseTimeoutTask`、`TestRuntimeCheckpointRestoresActiveTimelineDeterministically`、`TestRuntimeCheckpointRestoresActiveSpawns`；其余 6 项见文件
- [combat_test.go](../../../gameplay/skill/combat_test.go)：`TestCombatPipelineResolvesMitigationElementCriticalShieldAndDeath`、`TestDamageImmuneIsSuccessfulAndTagsDoNotAlias`
- [compile_authority_test.go](../../../gameplay/skill/compile_authority_test.go)：`TestCompileEnvironmentRejectsEmptyAuthority`、`TestAuthorityDigestChangesWithElementPolicy`、`TestVisualCatalogDoesNotChangeGameplayAuthorityDigest`、`TestAuthorityIdentityRequiresExactNonEmptyMatch`、`TestGameplayCatalogValidation`；其余 1 项见文件
- [compile_contract_test.go](../../../gameplay/skill/compile_contract_test.go)：`TestCompileRejectsSnapshotsThatCannotBeCapturedWhereTheyAreRead`、`TestSnapshotsCapturedAtTheirPointStillRun`、`TestCompileRejectsPassivesThatCanNeverActivate`、`TestPassivesWithEventInputsStillActivate`、`TestCompileRejectsSpawnsOnEffectsThatDoNotSummon`；其余 14 项见文件
- [compile_mutation_property_test.go](../../../gameplay/skill/compile_mutation_property_test.go)：`TestCompiledMutationsNeverHitProgramInvariant`、`TestCompiledMutationsChangeDigestWhenProgramChanges`
- [compile_mutation_race_test.go](../../../gameplay/skill/compile_mutation_race_test.go)
- [compile_pass_branches_test.go](../../../gameplay/skill/compile_pass_branches_test.go)：`TestRandomPassBranches`、`TestTemporalPassBranches`、`TestGraphPassBranchesRunEveryReachablePhase`、`TestEffectResultPassBranches`、`TestProcPassResultFilterBranches`；其余 1 项见文件
- [compile_rules_test.go](../../../gameplay/skill/compile_rules_test.go)：`TestBudgetRejectsRepeatAboveLimit`、`TestCompileRejectsInvalidPhaseGraphs`、`TestCompileRejectsFallthroughEnterWithoutTimeout`、`TestCompileRejectsParallelFinish`、`TestCompileRejectsMaybeInitializedMemoryRead`；其余 26 项见文件
- [documentation_test.go](../../../gameplay/skill/documentation_test.go)：`TestUserGuideExamplesCompile`
- [effect_result_test.go](../../../gameplay/skill/effect_result_test.go)：`TestEffectResultDamageSuccessExecutesTypedBranch`、`TestEffectResultStateChangeCarriesDynamicBeforeAndAfter`、`TestEffectResultBlockedCombatStillUsesSuccessOutcome`、`TestEffectExpectedFailureFromMemoryHostUsesFailureBranch`、`TestEffectResultScopeRejectsInvalidFieldOutcomeAndEscape`；其余 10 项见文件
- [eval_context_test.go](../../../gameplay/skill/eval_context_test.go)：`TestCompileRejectsMemoryDefaultsReadingMemoryDeterministically`、`TestMemoryDefaultsReadingTheCastStillRun`、`TestCompileRejectsStateDefaultsThatCannotBeEvaluatedEverywhere`、`TestStateDefaultOfTheCasterRunsInCallbacks`、`TestCompileRejectsOptionalEntitiesInCachedReads`；其余 10 项见文件
- [executor_test.go](../../../gameplay/skill/executor_test.go)：`TestStartRefusesEachCorruptedProgramShape`、`TestExecutorRefusesNonBoolConditionsAndStopsRepeatOnBodyOutcome`、`TestExecutorRefusesSelectorsAndIterationTasksOutsideTheProgram`、`TestMemoryHostReadsAndDamageRefuseUnknownEntitiesAndForeignCatalogs`
- [fixed_math_test.go](../../../gameplay/skill/fixed_math_test.go)：`TestMulDivRoundedMatchesReference`、`TestIntegerDistanceMatchesReference`、`TestMotionMetricIsEuclidean`、`TestIsqrt64Floor`
- [fuzz_test.go](../../../gameplay/skill/fuzz_test.go)
- [generated_definitions_compile_test.go](../../../gameplay/skill/generated_definitions_compile_test.go)：`TestGeneratedSkillDefinitionsCompileWithoutDiagnostics`
- [host_contract_test.go](../../../gameplay/skill/host_contract_test.go)：`TestNilHostAdvanceAndPassiveReturnError`、`TestCheckpointCapabilityFailureKeepsSentinel`、`TestHostAdmissionCacheRemainsBounded`、`TestHostCapabilityMissingIsRejectedAtCompileTime`、`TestRuntimeRefusesProgramsOutsideTheHostTable`；其余 15 项见文件
- [identity_test.go](../../../gameplay/skill/identity_test.go)：`TestProgramIdentityAndIndicesAreDeterministic`、`TestProgramIdentitySeparatesGameplayAndPresentation`、`TestVisualCatalogIdentityDoesNotChangeGameplayDigest`、`TestProgramIdentityIgnoresMemoryMapInsertionOrder`、`TestInspectReturnsStableCopies`
- [input_test.go](../../../gameplay/skill/input_test.go)：`TestInputSchemaVisibilityAndValidation`、`TestInputClampRangeDragAndBlockedPosition`、`TestInputPathBoundsSimplificationAndSnapshot`、`TestInputUpdateReplacesOnlyPortFieldAndPreservesCommitState`
- [lower_test.go](../../../gameplay/skill/lower_test.go)：`TestProgramDoesNotAliasDefinition`、`TestLowerProducesTypedDamageOperation`、`TestLowerPreservesStableInputSelectionAndRandomTables`、`TestLowerRefusesEveryUnresolvedLookup`
- [memory_host_test.go](../../../gameplay/skill/memory_host_test.go)：`TestHostCommandPayloadsAreNarrowAndTyped`、`TestMemoryHostPayCostsIsAtomic`、`TestMemoryHostSelectIsDeterministic`、`TestMemoryHostRaycastUsesHitDistanceThenStableCollider`、`TestMemoryHostStopSpawnIsIdempotent`；其余 3 项见文件
- [motion_test.go](../../../gameplay/skill/motion_test.go)：`TestMotionPipelineHasFixedStageOrder`、`TestMotionLinearTrajectoryRoundsHalfAwayFromZero`、`TestMotionPipelineAggregatesStageSignalsAndEmitsTargetLostOnce`、`TestMotionPayloadsControlTrajectoryAndSteering`、`TestMotionFixedSteeringAndOffsetsUseStableTrajectoryBase`；其余 12 项见文件
- [normalize_test.go](../../../gameplay/skill/normalize_test.go)：`TestNormalizeCreatesTypedDamageIR`、`TestNormalizeDefaultsTapPolicy`、`TestNormalizeWalkValuesVisitsEachSourceOnce`、`TestNormalizeDoesNotRetainMutableWireData`、`TestNormalizeRejectsInvalidIdentifiersWithSourcePath`；其余 1 项见文件
- [numeric_test.go](../../../gameplay/skill/numeric_test.go)：`TestNumericPropertyCatalogIsClosed`、`TestModifySpawnRejectsInvalidOwnershipOrValue`、`TestModifySpawnIsCallbackScopedAndDoesNotEmitWorldEffect`、`TestNumericCatalogAuthorityValidation`、`TestNumericCatalogRequiresExactCanonicalPolicyFacts`；其余 2 项见文件
- [owned_entity_test.go](../../../gameplay/skill/owned_entity_test.go)：`TestOwnedEntitySummonRegistersAuthoritativeIdentityAndTypedResult`、`TestOwnedEntitySummonBindingsAreTypedClampedAndCapabilityChecked`、`TestOwnedReplacementPoliciesAreStableAndAtomic`、`TestOwnedReplacementDistanceTieUsesSummonSequenceThenEntityID`、`TestOwnedReplacementLimitsPerSourceSkillAndTeam`；其余 14 项见文件
- [parse_test.go](../../../gameplay/skill/parse_test.go)：`TestParseGeneratedAcceptsDirectSkillRoot`、`TestParseGeneratedRejectsSkillJSONWrapper`、`TestParseGeneratedAcceptsRejection`、`TestParseRejectsDuplicateNestedKey`、`TestParseRejectsNonCanonicalJSON`；其余 8 项见文件
- [presentation_asset_cache_test.go](../../../gameplay/skill/presentation_asset_cache_test.go)：`TestVisualPlanCacheTrustFallbackAndRelease`、`TestVisualPlanCacheRejectsUntrustedCatalog`、`TestVisualPlanCacheRejectsDigestCollision`、`TestVisualPlanCacheSharesAssetUntilLastPlanEviction`、`TestVisualPlanCacheSharesResolvedFallbackAcrossDifferentPrimaryKeys`；其余 5 项见文件
- [presentation_test.go](../../../gameplay/skill/presentation_test.go)：`TestPresentationPlanExposesCastAndEffectMounts`、`TestRuntimePresentationEventsAreCommittedOrderedAndPollable`、`TestPresentationPollingReportsRetentionLoss`、`TestSpawnVisualCompilesAndEmitsLifecycle`、`TestRuntimeDoesNotEmitEffectPresentationForExpectedFailure`；其余 4 项见文件
- [production_limits_test.go](../../../gameplay/skill/production_limits_test.go)：`TestParseLimitsRejectWorkBeforeSemanticDecode`、`TestRuntimeRetentionIsBounded`、`TestClockMutationCarriesDerivedTimeWithoutEntityFanout`
- [random_test.go](../../../gameplay/skill/random_test.go)：`TestRuntimeRandomSelectionIgnoresCandidateInsertionOrder`、`TestBoundedRandomIsDeterministicAndBounded`、`TestCompileRandomSitesAreDeterministic`
- [replay_test.go](../../../gameplay/skill/replay_test.go)：`TestRecordingAndReplayHostMatchTypedInteractionOrder`、`TestRecordingReplayPreserveOwnedSummonLifecycle`、`TestRecordingReplayForwardOptionalWorldViews`
- [runtime_bounds_test.go](../../../gameplay/skill/runtime_bounds_test.go)：`TestQueueExternalEventRejectsAtTheQueueBoundInsteadOfFillingTheRootTable`、`TestPassiveCandidatesPastTheQueueBoundAreRejectedNotDropped`、`TestAbilityOverlayExpiriesDoNotPinRoots`、`TestTasksOfEndedCastsDoNotPinRoots`、`TestLegalConfigurationNeverReachesTheRootTableFallback`；其余 3 项见文件
- [runtime_cast_policy_test.go](../../../gameplay/skill/runtime_cast_policy_test.go)：`TestCastPolicyTogglePulsesAndSecondActivateReleases`、`TestHoldReleaseStopsPulsesAndStartsCooldown`、`TestChargeBelowMinimumCancelsAndSuccessfulReleasePaysOnce`、`TestAmmoPaymentStockAndSingleRechargeTimeline`、`TestToggleSustainFailureReleasesAndCancelStartsCooldown`；其余 3 项见文件
- [runtime_dispatch_test.go](../../../gameplay/skill/runtime_dispatch_test.go)：`TestQueueExternalEventRoutesWithoutRecursiveActivation`、`TestRouterCandidatesAreNormalizedBeforeEnqueue`、`TestUncoverablePassiveCandidateDoesNotStallTheEventStream`、`TestCostPaymentDispatchFailureDoesNotDropAPaidCast`、`TestDerivedEventPreservesRootAndSetsParent`；其余 5 项见文件
- [runtime_mutation_benchmark_test.go](../../../gameplay/skill/runtime_mutation_benchmark_test.go)
- [runtime_mutation_verify_test.go](../../../gameplay/skill/runtime_mutation_verify_test.go)
- [runtime_sync_test.go](../../../gameplay/skill/runtime_sync_test.go)：`TestRuntimeValueJSONRoundTrip`、`TestRuntimeStateSnapshotAndEventCursorAreComplete`、`TestStateMutationsFoldExactlyIntoLaterSnapshot`、`TestStateMutationsAreCommittedBeforeWriteReturns`、`TestRedactRuntimeValueRecursivelyFiltersSensitiveReferences`
- [runtime_test.go](../../../gameplay/skill/runtime_test.go)：`TestRuntimeActivateExecutesTypedDamage`、`TestRuntimeExecutesSequenceRepeatIfAndMemory`、`TestRuntimeRejectsAuthorityMismatchWithoutSideEffects`、`TestRuntimeDoesNotRetainOrInterpretDefinition`、`TestRuntimeRejectsCompilerSemanticsAndInputBeforeSideEffects`；其余 5 项见文件
- [scheduler_test.go](../../../gameplay/skill/scheduler_test.go)：`TestSchedulerOrdersByDueTickThenSequence`、`TestRuntimeWaitExecutesExactlyAtDueTick`、`TestRuntimeRepeatIntervalExecutesExactCount`、`TestRuntimeSelectEachSuspensionRetainsIndependentLocalFrames`、`TestRuntimeStalePhaseTaskReleasesFrameOnce`
- [spawn_lifecycle_test.go](../../../gameplay/skill/spawn_lifecycle_test.go)：`TestSpawnLifecycleSettlesEveryTickFromCastToHandoffToStop`、`TestRefusedMotionStartStepLeavesNoHostSpawn`、`TestSpawnCallbacksShareTheCastRootForOncePerRoot`、`TestProcCastSpawnCallbacksKeepDepthAndRoot`、`TestCastingAreaFinishAtALaterTickFinishesTheCast`；其余 7 项见文件
- [spawn_partition_test.go](../../../gameplay/skill/spawn_partition_test.go)：`TestSpawnPartitionsFollowRecordFields`、`TestSpawnPartitionWritesStayInSpawnTable`
- [spawn_retention_test.go](../../../gameplay/skill/spawn_retention_test.go)：`TestStopSweepAbandonsAtTheStopPendingLimit`、`TestAbandonedSpawnsArePrunedOnlyAtTheEndOfAdvance`、`TestAbandonedSpawnIsVisibleToSyncConsistently`、`TestCastsWhoseSpawnsEndedStayWithinTheCompletedCastLimit`、`TestHandedOffSpawnsBeyondTheCompletedLimitStillRestore`
- [spawn_stop_test.go](../../../gameplay/skill/spawn_stop_test.go)：`TestEveryStopEntryDefersARefusedStopTheSameWay`、`TestSpawnStopEntriesAreRegistered`、`TestFailedStartRetriesTheStopItCouldNotFinish`、`TestStopPendingSurvivesCheckpointAndKeepsRetrying`、`TestPinnedCastsBeyondTheCompletedLimitStillRestore`；其余 4 项见文件
- [state_test.go](../../../gameplay/skill/state_test.go)：`TestPersistentStateSchemaLowersStablePrivateLayout`、`TestPersistentStateRejectsSharedDeclarationDuplicateEnumAndInvalidSnapshotScope`、`TestSharedStateUnknownHandleIsRejected`、`TestPersistentStateTypedReadAndMutationExecuteAcrossCasts`、`TestPersistentStatePrivateIdentityAndOwnerTargetIsolation`；其余 7 项见文件
- [status_instance_test.go](../../../gameplay/skill/status_instance_test.go)：`TestStatusSelectFiltersAndStableOrders`、`TestStatusOperationClosedSetClampCopyTransferAndExpiry`、`TestStatusDurationClampAppliesAtCreationAndCopy`、`TestStatusBatchSnapshotAndConsumerCannotSuspend`、`TestShieldStatusIsQueryableAndEmitsSingleAbsorbBreak`；其余 3 项见文件
- [status_test.go](../../../gameplay/skill/status_test.go)：`TestStatusRefreshStacksAndExpiresAtLogicalTick`、`TestAttributeModifierUsesAddThenMultiplyOrdering`、`TestAttributeModifierUsesCatalogRounding`、`TestStatusHonorsImmunityTenacitySourceRemovalAndDispel`
- [temporal_test.go](../../../gameplay/skill/temporal_test.go)：`TestTemporalProfileCompileRules`、`TestTemporalCatalogRejectsDuplicateProfileKey`、`TestTemporalCaptureFreezesProfileAuthorization`、`TestTemporalResultLayoutsAcceptHostExpectedFailures`、`TestTemporalRestoreResultProjectsFieldLists`；其余 8 项见文件
- [test_helpers_test.go](../../../gameplay/skill/test_helpers_test.go)
- [trace_test.go](../../../gameplay/skill/trace_test.go)：`TestTraceIsBoundedAndDoesNotChangeGameplay`、`TestTraceSinkIsDrainedOutsideGameplayAndFailuresAreIgnored`

### skill/combat

- [combat_test.go](../../../gameplay/skill/combat/combat_test.go)：`TestAttributeSetGrantRevokeIsExactlyReversible`、`TestAttributeSetObserverAndSnapshotOrder`、`TestBuffContainerStackRefreshDispelAndImmunity`、`TestBuffContainerTenacityAndExpiry`、`TestBuffExtendPolicyAccumulatesDuration`；其余 14 项见文件

### skill/combatcomponent

- [adapter_test.go](../../../gameplay/skill/combatcomponent/adapter_test.go)：`TestHostAdapterAppliesDamageWithEvents`、`TestHostAdapterHealShieldAndReads`、`TestHostAdapterShieldReportsEffectiveDeltaAndSkipsNoOpCommit`、`TestHostAdapterPayCostsIsAtomic`、`TestResourceCommandRefusesEachIllegalOperation`；其余 10 项见文件
- [benchmark_test.go](../../../gameplay/skill/combatcomponent/benchmark_test.go)
- [component_test.go](../../../gameplay/skill/combatcomponent/component_test.go)：`TestNestUndoRollbackRestoresCombatStateExactly`、`TestCombatDaoPersistenceRoundTrip`、`TestNestUndoWorksThroughRealHandlers`、`TestCombatDaoProducesTransactionLocalMutationAndSyncMask`、`TestCombatMutationOutsideTransactionPanicsBeforeStateChange`；其余 6 项见文件
- [refusals_test.go](../../../gameplay/skill/combatcomponent/refusals_test.go)：`TestDetachedTransactionFailuresAreReturnedNotDereferenced`、`TestReadsAndPaymentsRefuseUnknownEntitiesAndUnmappedResources`、`TestCombatDaoRefusesForeignPersistedStateAndEmptyPatches`、`TestStatusBridgeRefusesToModifyABuffWithoutACatalogPolicy`
- [state_bridge_test.go](../../../gameplay/skill/combatcomponent/state_bridge_test.go)：`TestBuffTagsDriveDerivedStates`、`TestBindStatesRefusesBadBindings`、`TestLoadedBuffsReachBoundStatesOnInitFinish`
- [status_bridge_test.go](../../../gameplay/skill/combatcomponent/status_bridge_test.go)：`TestStatusBridgeAppliesStacksAndModifiers`、`TestStatusBridgeImmunityTenacityAndReplace`、`TestStatusBridgeRemoveAndDispel`、`TestStatusBridgeAttributeModifierIsIndependent`、`TestHostAdapterResourceCommand`；其余 2 项见文件

### skill/integration/sync-e2e

- [e2e_test.go](../../../gameplay/skill/integration/sync-e2e/e2e_test.go)：`TestCrashRecoveryThroughConfirmedFragmentedTransport`
- [soak_test.go](../../../gameplay/skill/integration/sync-e2e/soak_test.go)：`TestProtocolSoak`

### skill/skillcompose

- [contract_test.go](../../../gameplay/skill/skillcompose/contract_test.go)：`TestContractCallerPolicyOnlyTightens`、`TestContractCanonicalOmitsDerivedPromptFields`、`TestCanonicalContractDoesNotMutateCaller`、`TestBuildContractRejectsUnboundSourceAuthority`、`TestBuildContractRefusesEachInvalidInput`；其余 1 项见文件
- [matcher_test.go](../../../gameplay/skill/skillcompose/matcher_test.go)：`TestMatcherIsDeterministic`
- [profile_test.go](../../../gameplay/skill/skillcompose/profile_test.go)：`TestProfileExtractionIsStableAndUsesInspectorFacts`
- [validator_test.go](../../../gameplay/skill/skillcompose/validator_test.go)：`TestValidateCandidateRejectsUngrantableGrowthAndDisconnectedFlow`、`TestValidateCandidateRejectsUngroundedFeatureOrigin`、`TestValidateCandidateExplainsBlankOrDuplicateSources`、`TestGrantedNonDamageEffectsAreValidCandidates`

### skill/skillsync

- [applier_test.go](../../../gameplay/skill/skillsync/applier_test.go)：`TestRejectedFullPacketDoesNotWedgeTheApplier`、`TestApplierRefusesConcurrentApplyStatesAndMisshapenRecords`、`TestNewApplierRefusesUnservableSchemas`、`TestApplierAdmissionRefusesEachMalformedPacket`、`TestApplierRecordRefusesHeaderAndDigestDisagreement`
- [coordinator_test.go](../../../gameplay/skill/skillsync/coordinator_test.go)：`TestCoordinatorRepairsPartialOutboxDeleteFailure`、`TestCoordinatorAckKeepsHistoryWhenOutboxDeleteFails`、`TestCoordinatorRejectsAckAheadBeforeDeletingOutbox`、`TestCoordinatorRepairsOutboxWhenHistoryAckFails`、`TestCoordinatorCloseKeepsHistoryWhenOutboxDeleteFails`；其余 6 项见文件
- [outbox_benchmark_test.go](../../../gameplay/skill/skillsync/outbox_benchmark_test.go)
- [outbox_test.go](../../../gameplay/skill/skillsync/outbox_test.go)：`TestPutAndPutBatchReachTheSameAdmissionVerdict`、`TestOutboxFailureBlocksLaterPacketsInSameStream`、`TestOverageOutboxCanRestartPublishAndAcknowledge`、`TestIdleSweepPreservesUnacknowledgedDelivery`、`TestDiscardObserverCannotSucceedWithPublishInFlight`；其余 8 项见文件
- [presentation_source_test.go](../../../gameplay/skill/skillsync/presentation_source_test.go)：`TestPresentationResetSpawnEntryMatchesItsIncrementalEvent`、`TestSnapshotStartsPresentationAtCurrentRuntime`、`TestOutboxFailureDoesNotAppendTheSameSourceAgain`、`TestFullRecoveryAdvancesSourceCursor`
- [schema_test.go](../../../gameplay/skill/skillsync/schema_test.go)：`TestSchemaRegistryMigratesDeterministicShortestChain`、`TestSchemaRegistryRejectsInvalidAndMissingMigrations`、`TestNegotiateSchemaRequiresBothRangesToContainTheResult`
- [skillsync_test.go](../../../gameplay/skill/skillsync/skillsync_test.go)：`TestProjectorBuildsStronglyTypedPackets`、`TestApplierValidatesChainObserverAndManifestDependency`、`TestCoordinatorRetainsPublishFailureForRecovery`、`TestCoordinatorRequiresExplicitVisibilityPolicy`、`TestCoordinatorReclaimsViewLocksAndRequiresExplicitReopen`；其余 9 项见文件
- [visibility_test.go](../../../gameplay/skill/skillsync/visibility_test.go)：`TestVisibilityClockAndUnknownMutationFailClosed`、`TestPresentationResetFromRecoverHonoursVisibility`、`TestPresentationResetFromExpiredCursorHonoursVisibility`、`TestStateSnapshotAndDeltasHideTheSameAbility`、`TestRemoveMutationsOfInvisibleEntitiesAreFiltered`

### spatial

- [spatial_test.go](../../../infra/base/spatial/spatial_test.go)：`TestGridTerrainAtomicMovesAndBounds`、`TestDistanceOperationsDoNotOverflow`、`TestGridTerrainConcurrentAccessKeepsWritesConsistent`、`TestFindPath`、`TestFindPathDistinguishesBudgetFromNoPath`；其余 6 项见文件

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。没有新测量不能改容量承诺，性能结论以 [性能报告](../../performance/README.md) 为准。
