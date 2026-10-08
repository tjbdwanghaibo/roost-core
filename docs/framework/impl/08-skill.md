# 游戏技能、战斗与空间：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/08-skill.md)

## 1. 实现边界

`skill`、`attribute`、`spatial`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. Runtime 与 DAO 回滚域分离。
2. Spawn 与 Summon 生命周期不同，能力声明与包装转发一致。
3. 表现流有序、隐私由 projector 保证；checkpoint 旧版拒绝。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

### attribute

2 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [attribute.go](../../../attribute/attribute.go) | `AttrID`、`AttrValue`、`Meta`、`Profile` |
| [container.go](../../../attribute/container.go) | `Selector`、`Snapshot`、`Container` |

### skill

155 个实现文件、110 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [canonical_definition.go](../../../skill/canonical_definition.go) | 函数/方法或内部实现；见源码 |
| [compile.go](../../../skill/compile.go) | 函数/方法或内部实现；见源码 |
| [compile_ability.go](../../../skill/compile_ability.go) | `AbilityPropertyHandle`、`AbilityReadPlan` |
| [compile_authority.go](../../../skill/compile_authority.go) | `AuthorityIdentity`、`AuthorityProvider` |
| [compile_budget.go](../../../skill/compile_budget.go) | 函数/方法或内部实现；见源码 |
| [compile_capability.go](../../../skill/compile_capability.go) | 函数/方法或内部实现；见源码 |
| [compile_context.go](../../../skill/compile_context.go) | `InputLayout`、`PhaseGraph`、`RandomSite`、`ComputedLimits`、`AttributeReadPlan`、`FilterPlan`、`ProcPlan` |
| [compile_effect_result.go](../../../skill/compile_effect_result.go) | 函数/方法或内部实现；见源码 |
| [compile_environment.go](../../../skill/compile_environment.go) | `AttributeHandle`、`ResourceHandle`、`StatusHandle`、`UnitTemplateHandle`、`CollisionLayerHandle`、`DamageTypeHandle`、`ElementHandle`、`GameplayTagHandle`、`SharedStateHandle`、`TemporalProfileHandle`、`AttributeCatalog`、`AttributeCatalogEntry`、`ResourceCatalog`、`ResourceCatalogEntry`、`StatusCatalog`、`StatusCatalogEntry`、`StatusAttributeModifier`、`UnitTemplateCatalog`、`UnitTemplateCatalogEntry`、`UnitTemplateAttributeOverridePolicy`、`UnitTemplateParameterPolicy`、`CollisionLayerCatalog`、`CollisionLayerCatalogEntry`、`DamageTypeCatalog`、`DamageTypeCatalogEntry`、`ElementCatalog`、`ElementCatalogEntry`、`GameplayTagClass`、`GameplayTagCatalog`、`GameplayTagCatalogEntry`、`CombatPolicyCatalog`、`SharedStateCatalog`、`SharedStateCatalogEntry`、`AbilityControlCatalog`、`AbilityPropertyPolicy`、`TemporalSnapshotCatalog`、`TemporalSnapshotProfile`、`GameplayCatalog`、`MotionCapabilityCatalog`、`MotionSpawnTrajectoryPair`、`MotionVariantCapability`、`SpawnPropertyCatalog`、`SpawnPropertyHandle`、`SpawnPropertySlotBinding`、`SpawnPropertyPolicy`、`VisualCatalog`、`VisualLimits`、`VisualElementDescriptor`、`VisualCategoryDescriptor`、`VisualThemeDescriptor`、`NumericAuthority`、`CompileLimits`、`CompileEnvironment` |
| [compile_graph.go](../../../skill/compile_graph.go) | 函数/方法或内部实现；见源码 |
| [compile_host_capability.go](../../../skill/compile_host_capability.go) | 函数/方法或内部实现；见源码 |
| [compile_identity.go](../../../skill/compile_identity.go) | 函数/方法或内部实现；见源码 |
| [compile_input.go](../../../skill/compile_input.go) | 函数/方法或内部实现；见源码 |
| [compile_lifetime.go](../../../skill/compile_lifetime.go) | 函数/方法或内部实现；见源码 |
| [compile_memory.go](../../../skill/compile_memory.go) | 函数/方法或内部实现；见源码 |
| [compile_motion.go](../../../skill/compile_motion.go) | 函数/方法或内部实现；见源码 |
| [compile_normalize.go](../../../skill/compile_normalize.go) | 函数/方法或内部实现；见源码 |
| [compile_optional.go](../../../skill/compile_optional.go) | 函数/方法或内部实现；见源码 |
| [compile_owned_entity.go](../../../skill/compile_owned_entity.go) | 函数/方法或内部实现；见源码 |
| [compile_proc.go](../../../skill/compile_proc.go) | 函数/方法或内部实现；见源码 |
| [compile_quantity.go](../../../skill/compile_quantity.go) | 函数/方法或内部实现；见源码 |
| [compile_random.go](../../../skill/compile_random.go) | 函数/方法或内部实现；见源码 |
| [compile_shape.go](../../../skill/compile_shape.go) | 函数/方法或内部实现；见源码 |
| [compile_snapshot.go](../../../skill/compile_snapshot.go) | 函数/方法或内部实现；见源码 |
| [compile_state.go](../../../skill/compile_state.go) | 函数/方法或内部实现；见源码 |
| [compile_status.go](../../../skill/compile_status.go) | 函数/方法或内部实现；见源码 |
| [compile_tags.go](../../../skill/compile_tags.go) | 函数/方法或内部实现；见源码 |
| [compile_temporal.go](../../../skill/compile_temporal.go) | 函数/方法或内部实现；见源码 |
| [compile_typecheck.go](../../../skill/compile_typecheck.go) | 函数/方法或内部实现；见源码 |
| [compile_visual.go](../../../skill/compile_visual.go) | 函数/方法或内部实现；见源码 |
| [diagnostic.go](../../../skill/diagnostic.go) | `DiagnosticSeverity`、`Severity`、`DiagnosticCode`、`Diagnostic` |
| [doc.go](../../../skill/doc.go) | 函数/方法或内部实现；见源码 |
| [environment_kinds.go](../../../skill/environment_kinds.go) | 函数/方法或内部实现；见源码 |
| [eval_contexts.go](../../../skill/eval_contexts.go) | 函数/方法或内部实现；见源码 |
| [executor.go](../../../skill/executor.go) | 函数/方法或内部实现；见源码 |
| [fixed_math.go](../../../skill/fixed_math.go) | 函数/方法或内部实现；见源码 |
| [generated.go](../../../skill/generated.go) | `GeneratedResult`、`Rejection`、`RejectionError` |
| [host.go](../../../skill/host.go) | `Host`、`HostEventCompactor`、`InputPositionResolver`、`InputPositionRequest`、`AbilityRelationProvider`、`ReadRequest`、`ReadPayload`、`ResourceRead`、`PositionRead`、`AttributeRead`、`ReadResult`、`SelectRequest`、`SelectResult`、`SelectShape`、`SingleSelectShape`、`CircleSelectShape`、`RingSelectShape`、`ConeSelectShape`、`LineSelectShape`、`RectangleSelectShape`、`RaycastSelectShape`、`ChainSelectShape`、`PathSelectShape`、`NearestValidSelectShape`、`SelectFilter`、`AliveSelectFilter`、`NotCasterSelectFilter`、`RelationSelectFilter`、`StatusSelectFilter`、`AttributeSelectFilter`、`VisibleSelectFilter`、`TargetableSelectFilter`、`LineOfSightSelectFilter`、`GameplayTagSelectFilter`、`SelectOrderBy`、`SelectDirection`、`SelectOrder` |
| [host_capability.go](../../../skill/host_capability.go) | `HostCapabilityCatalog`、`HostCapabilityTable`、`HostCapabilityProvider`、`HostCapabilityKind`、`HostCapability` |
| [host_capability_check.go](../../../skill/host_capability_check.go) | `HostCapabilityProbe` |
| [host_combat.go](../../../skill/host_combat.go) | `DamageCommand`、`DamageResult`、`DamageEffectResult`、`HealCommand`、`HealResult`、`HealEffectResult`、`ShieldCommand`、`ShieldResult`、`ShieldEffectResult`、`StatusCommand`、`RemoveStatusCommand`、`DispelStatusCommand`、`StatusResult`、`StatusEffectResult`、`AttributeModifierCommand`、`AttributeModifierResult`、`AttributeModifierEffectResult` |
| [host_command.go](../../../skill/host_command.go) | `EffectCommand`、`EffectCommandPayload`、`TeleportCommand`、`KnockbackCommand`、`PullCommand`、`ResourceCommand`、`SummonCommand`、`SummonAttributeOverride`、`SummonParameterBinding`、`EffectResult`、`ExpectedFailureReason`、`ResultOutcome`、`TeleportEffectResult`、`SummonEffectResult`、`StateChangeEffectResult`、`AbilityChangeEffectResult`、`EntityCommandEffectResult`、`SnapshotCaptureEffectResult`、`SnapshotRestoreEffectResult`、`EffectResultPayload`、`CostEntry`、`CostPayment` |
| [host_event.go](../../../skill/host_event.go) | 函数/方法或内部实现；见源码 |
| [host_owned_entity.go](../../../skill/host_owned_entity.go) | `OwnedEntityMetadata`、`OwnedEntityCommand`、`OwnedEntitiesSelectShape`、`OwnedSourceSkillFilter`、`OwnedSourceCastFilter`、`OwnedUnitTemplateFilter`、`OwnedEntityTagFilter`、`OwnedSummonTickFilter`、`OwnedSummonPreview`、`OwnedSummonTransactionID`、`OwnedEntityRuntimeHost` |
| [host_revision.go](../../../skill/host_revision.go) | `WorldRevision`、`EventCursor`、`SpawnID`、`QueryMeta`、`QueryResultMeta`、`CommandMeta`、`CommitReceipt`、`RuntimeEvent` |
| [host_spawn.go](../../../skill/host_spawn.go) | `SpawnCommandMeta`、`MotionStep`、`StaticMotionStep`、`FrameMotionStep`、`SteeringMotionStep`、`TrajectoryMotionStep`、`OffsetsMotionStep`、`CollisionMotionStep`、`CarryMotionStep`、`CompletionMotionStep`、`SignalsMotionStep`、`SpawnNumericSnapshot`、`SpawnStepCommand`、`SpawnStopCommand`、`SpawnHostState`、`SpawnStepResult` |
| [host_state.go](../../../skill/host_state.go) | `StateSlot`、`StateHandle`、`StateScope`、`StateScopeBinding`、`StateReadRequest`、`StateReadResult`、`StateMutationCommand`、`StateMutationResult`、`StateChangeEvent`、`StateStore` |
| [host_status.go](../../../skill/host_status.go) | `StatusInstanceID`、`StatusInstanceRef`、`StatusSetSelectShape`、`StatusIDSelectFilter`、`StatusTextSelectFilter`、`StatusFlagSelectFilter`、`StatusEntitySelectFilter`、`StatusSourceSkillSelectFilter`、`StatusCompareSelectFilter`、`ModifyStatusInstanceCommand` |
| [host_temporal.go](../../../skill/host_temporal.go) | `TemporalCaptureCommand`、`TemporalRestoreCommand` |
| [host_value.go](../../../skill/host_value.go) | 函数/方法或内部实现；见源码 |
| [inspect.go](../../../skill/inspect.go) | 函数/方法或内部实现；见源码 |
| [ir.go](../../../skill/ir.go) | 函数/方法或内部实现；见源码 |
| [ir_ability.go](../../../skill/ir_ability.go) | 函数/方法或内部实现；见源码 |
| [ir_combat.go](../../../skill/ir_combat.go) | 函数/方法或内部实现；见源码 |
| [ir_effect.go](../../../skill/ir_effect.go) | 函数/方法或内部实现；见源码 |
| [ir_effect_result.go](../../../skill/ir_effect_result.go) | 函数/方法或内部实现；见源码 |
| [ir_event.go](../../../skill/ir_event.go) | `EventID`、`CastID`、`EventContext` |
| [ir_flow.go](../../../skill/ir_flow.go) | 函数/方法或内部实现；见源码 |
| [ir_input.go](../../../skill/ir_input.go) | 函数/方法或内部实现；见源码 |
| [ir_motion.go](../../../skill/ir_motion.go) | 函数/方法或内部实现；见源码 |
| [ir_numeric.go](../../../skill/ir_numeric.go) | 函数/方法或内部实现；见源码 |
| [ir_owned_entity.go](../../../skill/ir_owned_entity.go) | 函数/方法或内部实现；见源码 |
| [ir_select.go](../../../skill/ir_select.go) | 函数/方法或内部实现；见源码 |
| [ir_state.go](../../../skill/ir_state.go) | 函数/方法或内部实现；见源码 |
| [ir_status.go](../../../skill/ir_status.go) | 函数/方法或内部实现；见源码 |
| [ir_temporal.go](../../../skill/ir_temporal.go) | 函数/方法或内部实现；见源码 |
| [ir_visit.go](../../../skill/ir_visit.go) | 函数/方法或内部实现；见源码 |
| [lower.go](../../../skill/lower.go) | 函数/方法或内部实现；见源码 |
| [memory_host.go](../../../skill/memory_host.go) | `MemoryEntity`、`MemoryHost`、`MemoryHostOptions` |
| [memory_host_combat.go](../../../skill/memory_host_combat.go) | 函数/方法或内部实现；见源码 |
| [memory_host_effect.go](../../../skill/memory_host_effect.go) | 函数/方法或内部实现；见源码 |
| [memory_host_motion.go](../../../skill/memory_host_motion.go) | 函数/方法或内部实现；见源码 |
| [memory_host_owned_entity.go](../../../skill/memory_host_owned_entity.go) | 函数/方法或内部实现；见源码 |
| [memory_host_select.go](../../../skill/memory_host_select.go) | 函数/方法或内部实现；见源码 |
| [memory_host_spawn.go](../../../skill/memory_host_spawn.go) | 函数/方法或内部实现；见源码 |
| [memory_host_state.go](../../../skill/memory_host_state.go) | 函数/方法或内部实现；见源码 |
| [memory_host_status.go](../../../skill/memory_host_status.go) | 函数/方法或内部实现；见源码 |
| [memory_host_temporal.go](../../../skill/memory_host_temporal.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../skill/parse.go) | `ParseLimits` |
| [parse_duplicate.go](../../../skill/parse_duplicate.go) | 函数/方法或内部实现；见源码 |
| [phase_events.go](../../../skill/phase_events.go) | 函数/方法或内部实现；见源码 |
| [presentation.go](../../../skill/presentation.go) | `PresentationEventKind`、`PresentationAnchor`、`PresentationMount`、`PresentationPlan`、`PresentationEvent`、`PresentationBatch` |
| [presentation_asset_cache.go](../../../skill/presentation_asset_cache.go) | `VisualCatalogTrust`、`TrustedVisualCatalogs`、`VisualAssetLoader`、`VisualPlanCacheOptions`、`VisualPlanCache`、`VisualPlanLease` |
| [presentation_assets.go](../../../skill/presentation_assets.go) | `VisualAsset`、`VisualAssetResolver`、`ResolvedVisualAsset`、`ResolvedPresentationPlan` |
| [presentation_recovery.go](../../../skill/presentation_recovery.go) | `ActivePresentationKind`、`ActivePresentation`、`PresentationRecoverySnapshot` |
| [program.go](../../../skill/program.go) | `Program`、`ProgramView`、`CastWindowView`、`CostView`、`MemorySlotView`、`LocalSlotView`、`PhaseView`、`RootView`、`OperationView`、`MetricSnapshot`、`CombatSemanticView`、`DamageSemanticView`、`StateLayoutView`、`AbilityControlView`、`OwnedEntityView`、`StatusOperationView`、`TemporalProfileView` |
| [program_ability.go](../../../skill/program_ability.go) | 函数/方法或内部实现；见源码 |
| [program_combat.go](../../../skill/program_combat.go) | 函数/方法或内部实现；见源码 |
| [program_digest.go](../../../skill/program_digest.go) | 函数/方法或内部实现；见源码 |
| [program_effect_result.go](../../../skill/program_effect_result.go) | `ResultFieldHandle` |
| [program_event.go](../../../skill/program_event.go) | `EventPlanView` |
| [program_identity.go](../../../skill/program_identity.go) | `MemoryIndex`、`LocalIndex`、`PhaseIndex`、`RootIndex`、`SelectorIndex`、`OperationIndex`、`EffectIndex`、`SpawnTemplateIndex`、`VisualIndex`、`RandomSiteIndex`、`ProgramIdentityView` |
| [program_input.go](../../../skill/program_input.go) | `InputPort`、`InputSlotView`、`InputProgramView`、`InputLayoutView` |
| [program_operation.go](../../../skill/program_operation.go) | 函数/方法或内部实现；见源码 |
| [program_owned_entity.go](../../../skill/program_owned_entity.go) | 函数/方法或内部实现；见源码 |
| [program_quantity.go](../../../skill/program_quantity.go) | `QuantityView` |
| [program_random.go](../../../skill/program_random.go) | `RandomSiteView` |
| [program_select.go](../../../skill/program_select.go) | `SelectionView`、`EffectResultView` |
| [program_spawn.go](../../../skill/program_spawn.go) | `SpawnTemplateView` |
| [program_state.go](../../../skill/program_state.go) | `PersistentStateView` |
| [program_status.go](../../../skill/program_status.go) | 函数/方法或内部实现；见源码 |
| [program_temporal.go](../../../skill/program_temporal.go) | 函数/方法或内部实现；见源码 |
| [program_visual.go](../../../skill/program_visual.go) | `VisualView`、`SkillVisualManifest` |
| [quantity.go](../../../skill/quantity.go) | 函数/方法或内部实现；见源码 |
| [replay.go](../../../skill/replay.go) | `HostRecord`、`RecordingHost`、`ReplayHost` |
| [replay_optional.go](../../../skill/replay_optional.go) | 函数/方法或内部实现；见源码 |
| [runtime.go](../../../skill/runtime.go) | `RuntimeOptions`、`CastInput`、`InputPayload`、`CastStatus`、`CastWindowStage`、`CastSnapshot`、`Runtime` |
| [runtime_ability.go](../../../skill/runtime_ability.go) | `AbilityRegistration`、`AbilityChangeResult`、`AbilityChangeEvent` |
| [runtime_cast.go](../../../skill/runtime_cast.go) | 函数/方法或内部实现；见源码 |
| [runtime_cast_policy.go](../../../skill/runtime_cast_policy.go) | 函数/方法或内部实现；见源码 |
| [runtime_cast_window.go](../../../skill/runtime_cast_window.go) | 函数/方法或内部实现；见源码 |
| [runtime_checkpoint.go](../../../skill/runtime_checkpoint.go) | `RuntimeCheckpoint`、`ProgramResolver`、`ProgramResolverFunc` |
| [runtime_dispatch.go](../../../skill/runtime_dispatch.go) | 函数/方法或内部实现；见源码 |
| [runtime_effect_result.go](../../../skill/runtime_effect_result.go) | `EffectResultEvent` |
| [runtime_eval.go](../../../skill/runtime_eval.go) | 函数/方法或内部实现；见源码 |
| [runtime_event.go](../../../skill/runtime_event.go) | 函数/方法或内部实现；见源码 |
| [runtime_host_capability.go](../../../skill/runtime_host_capability.go) | 函数/方法或内部实现；见源码 |
| [runtime_input.go](../../../skill/runtime_input.go) | 函数/方法或内部实现；见源码 |
| [runtime_mutation.go](../../../skill/runtime_mutation.go) | `StateMutationKind`、`StateMutation`、`StateMutationBatch` |
| [runtime_owned_entity.go](../../../skill/runtime_owned_entity.go) | 函数/方法或内部实现；见源码 |
| [runtime_proc.go](../../../skill/runtime_proc.go) | `PassiveActivationID`、`AbilityHandle`、`PassiveCandidate`、`PassiveRouter` |
| [runtime_random.go](../../../skill/runtime_random.go) | 函数/方法或内部实现；见源码 |
| [runtime_retention.go](../../../skill/runtime_retention.go) | `RuntimeRetentionStats` |
| [runtime_select.go](../../../skill/runtime_select.go) | 函数/方法或内部实现；见源码 |
| [runtime_spawn_stop.go](../../../skill/runtime_spawn_stop.go) | 函数/方法或内部实现；见源码 |
| [runtime_state.go](../../../skill/runtime_state.go) | 函数/方法或内部实现；见源码 |
| [runtime_sync.go](../../../skill/runtime_sync.go) | `StateEvent`、`StateEventBatch`、`CastStateSnapshot`、`CooldownStateSnapshot`、`SkillResourceSnapshot`、`AbilityOverlaySnapshot`、`AbilityStateSnapshot`、`NumericPropertySnapshot`、`SpawnStateSnapshot`、`ActivePolicySnapshot`、`PersistentStateSnapshot`、`RuntimeStateExtensionProvider`、`RuntimeStateSnapshot` |
| [runtime_temporal.go](../../../skill/runtime_temporal.go) | 函数/方法或内部实现；见源码 |
| [runtime_turn.go](../../../skill/runtime_turn.go) | 函数/方法或内部实现；见源码 |
| [runtime_value.go](../../../skill/runtime_value.go) | `EntityID`、`SnapshotToken`、`Position`、`Direction`、`Hit`、`AbilityRef`、`RuntimeValue`、`Selection`、`SelectionQueryMeta` |
| [runtime_value_json.go](../../../skill/runtime_value_json.go) | 函数/方法或内部实现；见源码 |
| [runtime_value_redaction.go](../../../skill/runtime_value_redaction.go) | `RuntimeValueRedactionOptions` |
| [scheduler.go](../../../skill/scheduler.go) | `FrameID` |
| [spawn.go](../../../skill/spawn.go) | `SpawnStatus`、`SpawnScope`、`StopCause`、`MotionSpawnStage`、`MotionState`、`SpawnNumericState`、`SpawnInstance`、`SpawnSignalKind`、`SpawnSignal` |
| [spawn_area.go](../../../skill/spawn_area.go) | `AreaMemberState` |
| [spawn_motion.go](../../../skill/spawn_motion.go) | 函数/方法或内部实现；见源码 |
| [spawn_numeric.go](../../../skill/spawn_numeric.go) | 函数/方法或内部实现；见源码 |
| [spawn_owned.go](../../../skill/spawn_owned.go) | `OwnedSpawnSnapshot` |
| [spawn_table.go](../../../skill/spawn_table.go) | 函数/方法或内部实现；见源码 |
| [trace.go](../../../skill/trace.go) | `TraceSink`、`TraceLimits`、`TraceEvent`、`TraceEventKind` |
| [value.go](../../../skill/value.go) | `Value`、`ValueDefinition`、`NullValueDefinition`、`IntValueDefinition`、`BoolValueDefinition`、`StringValueDefinition`、`ReferenceValueDefinition`、`ExpressionValueDefinition`、`AttributeReadValueDefinition`、`StateReadValueDefinition`、`AbilityStateReadValueDefinition` |
| [value_optional.go](../../../skill/value_optional.go) | 函数/方法或内部实现；见源码 |
| [wire_activation.go](../../../skill/wire_activation.go) | `ActivationDefinition`、`ActiveActivationDefinition`、`PassiveActivationDefinition`、`EventFilterDefinition`、`ProcPolicyDefinition`、`CastPolicyDefinition`、`TapPolicyDefinition`、`TogglePolicyDefinition`、`ChargePolicyDefinition`、`AmmoPolicyDefinition`、`HoldPolicyDefinition` |
| [wire_cast_window.go](../../../skill/wire_cast_window.go) | `CastWindowDefinition` |
| [wire_combat.go](../../../skill/wire_combat.go) | 函数/方法或内部实现；见源码 |
| [wire_definition.go](../../../skill/wire_definition.go) | `Tick`、`Definition`、`Cost`、`MemoryDeclaration`、`PhaseDefinition`、`PhaseEventsDefinition` |
| [wire_effect.go](../../../skill/wire_effect.go) | `EffectDefinition`、`DamageEffectDefinition`、`HealEffectDefinition`、`ShieldEffectDefinition`、`AddStatusEffectDefinition`、`RemoveStatusEffectDefinition`、`ModifyStatusInstanceEffectDefinition`、`AttributeModifierEffectDefinition`、`ResourceEffectDefinition`、`SetMemoryEffectDefinition`、`AddMemoryEffectDefinition`、`ClearMemoryEffectDefinition`、`TeleportEffectDefinition`、`KnockbackEffectDefinition`、`PullEffectDefinition`、`StopMovementEffectDefinition`、`ModifyStateEffectDefinition`、`ModifyAbilityStateEffectDefinition`、`NumericTrackDefinition`、`ModifySpawnEffectDefinition`、`SummonEffectDefinition`、`DismissEffectDefinition`、`IssueEntityCommandEffectDefinition` |
| [wire_effect_result.go](../../../skill/wire_effect_result.go) | `EffectResultDefinition` |
| [wire_flow.go](../../../skill/wire_flow.go) | `FlowDefinition`、`SequenceFlowDefinition`、`ParallelFlowDefinition`、`IfFlowDefinition`、`RepeatFlowDefinition`、`WaitFlowDefinition`、`SelectFlowDefinition`、`EffectFlowDefinition`、`GotoFlowDefinition`、`FinishFlowDefinition`、`SpawnCallbacksDefinition` |
| [wire_input.go](../../../skill/wire_input.go) | `InputSchemaDefinition`、`NoneInputSchemaDefinition`、`DirectionInputSchemaDefinition`、`PositionInputSchemaDefinition`、`EntityInputSchemaDefinition`、`DirectionPositionInputSchemaDefinition`、`EntityPositionInputSchemaDefinition`、`TwoPointInputSchemaDefinition`、`DragInputSchemaDefinition`、`PathInputSchemaDefinition` |
| [wire_motion.go](../../../skill/wire_motion.go) | `SpawnDefinition`、`MotionDefinition`、`FrameDefinition`、`WorldFrameDefinition`、`FollowFrameDefinition`、`SteeringDefinition`、`TrackingSteeringDefinition`、`TrajectoryDefinition`、`StationaryTrajectoryDefinition`、`LinearTrajectoryDefinition`、`PathTrajectoryDefinition`、`OrbitTrajectoryDefinition`、`ParabolaTrajectoryDefinition`、`OffsetDefinition`、`ZigzagOffsetDefinition`、`CircularOffsetDefinition`、`MotionCollisionDefinition`、`CarryDefinition`、`CompletionDefinition`、`EndCompletionDefinition`、`PauseThenEndCompletionDefinition`、`BoomerangCompletionDefinition` |
| [wire_select.go](../../../skill/wire_select.go) | `SelectDefinition`、`SelectOrderDefinition`、`SelectConsumeDefinition`、`SelectOneConsumeDefinition`、`SelectEachConsumeDefinition`、`ShapeDefinition`、`SingleShapeDefinition`、`CircleShapeDefinition`、`RingShapeDefinition`、`ConeShapeDefinition`、`LineShapeDefinition`、`RectangleShapeDefinition`、`RaycastShapeDefinition`、`ChainShapeDefinition`、`PathShapeDefinition`、`NearestValidShapeDefinition`、`AbilitySetShapeDefinition`、`StatusSetShapeDefinition`、`OwnedEntitiesShapeDefinition`、`FilterDefinition`、`FlagFilterDefinition`、`RelationFilterDefinition`、`StatusFilterDefinition`、`AttributeCompareFilterDefinition`、`GameplayTagFilterDefinition`、`LineOfSightFilterDefinition`、`AbilityTagFilterDefinition`、`AbilitySlotFilterDefinition`、`OwnedSourceSkillFilterDefinition`、`OwnedSourceCastFilterDefinition`、`OwnedSummonTickFilterDefinition`、`OwnedUnitTemplateFilterDefinition`、`OwnedEntityTagFilterDefinition`、`StatusInstanceFilterDefinition` |
| [wire_state.go](../../../skill/wire_state.go) | `PersistentStateDefinition`、`StateLifetimeDefinition` |
| [wire_temporal.go](../../../skill/wire_temporal.go) | `CaptureSnapshotEffectDefinition`、`RestoreSnapshotEffectDefinition` |
| [wire_visual.go](../../../skill/wire_visual.go) | `SkillPresentation`、`VisualRef` |

### skill/combat

5 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [attributes.go](../../../skill/combat/attributes.go) | `AttributeID`、`ModifierHandle`、`Modifier`、`AttributeBounds`、`AttributeSet`、`AttributeValue`、`AttributeBaseState` |
| [buffs.go](../../../skill/combat/buffs.go) | `BuffID`、`BuffInstanceID`、`Tag`、`BuffStackPolicy`、`BuffSpec`、`BuffApplyOutcome`、`BuffInstance`、`BuffContainer`、`BuffContainerState` |
| [combat.go](../../../skill/combat/combat.go) | 函数/方法或内部实现；见源码 |
| [damage.go](../../../skill/combat/damage.go) | `DamageType`、`Element`、`Combatant`、`DamageInput`、`DamageOutcome`、`Hooks`、`HealOutcome` |
| [roll.go](../../../skill/combat/roll.go) | 函数/方法或内部实现；见源码 |

### skill/combatcomponent

3 个实现文件、12 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [adapter.go](../../../skill/combatcomponent/adapter.go) | `Resolver`、`RevisionSource`、`EffectEvent`、`HostAdapter` |
| [component.go](../../../skill/combatcomponent/component.go) | `CombatDao`、`CombatComponent`、`AttributeProjection` |
| [status_bridge.go](../../../skill/combatcomponent/status_bridge.go) | `StatusBridge` |

### skill/examples/combat

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../skill/examples/combat/main.go) | 函数/方法或内部实现；见源码 |

### skill/examples/fireball

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../skill/examples/fireball/main.go) | 函数/方法或内部实现；见源码 |

### skill/examples/statusbridge

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../skill/examples/statusbridge/main.go) | 函数/方法或内部实现；见源码 |

### skill/skillcompose

15 个实现文件、7 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [canonical.go](../../../skill/skillcompose/canonical.go) | 函数/方法或内部实现；见源码 |
| [catalog.go](../../../skill/skillcompose/catalog.go) | `FeatureKey`、`FeatureKind`、`TransformKind`、`FeatureDescriptor`、`Catalog` |
| [contract.go](../../../skill/skillcompose/contract.go) | `SkillCompositionContract`、`SourceGrant`、`SourceObligation`、`GenericPackageGrant`、`Constraint` |
| [contract_builder.go](../../../skill/skillcompose/contract_builder.go) | 函数/方法或内部实现；见源码 |
| [contract_validate.go](../../../skill/skillcompose/contract_validate.go) | 函数/方法或内部实现；见源码 |
| [diagnostic.go](../../../skill/skillcompose/diagnostic.go) | `Diagnostic` |
| [doc.go](../../../skill/skillcompose/doc.go) | 函数/方法或内部实现；见源码 |
| [graph.go](../../../skill/skillcompose/graph.go) | `CausalGraph` |
| [matcher.go](../../../skill/skillcompose/matcher.go) | `Match` |
| [metric.go](../../../skill/skillcompose/metric.go) | 函数/方法或内部实现；见源码 |
| [policy.go](../../../skill/skillcompose/policy.go) | `CompositionPolicy`、`CallerPolicy`、`PolicyIdentity`、`SourceIdentity`、`CompositionBudgets` |
| [profile.go](../../../skill/skillcompose/profile.go) | `SkillProfile`、`FeatureOrigin`、`SelectionFact`、`Metrics` |
| [profile_extract.go](../../../skill/skillcompose/profile_extract.go) | 函数/方法或内部实现；见源码 |
| [prompt_view.go](../../../skill/skillcompose/prompt_view.go) | `PromptContractView` |
| [validator.go](../../../skill/skillcompose/validator.go) | `ValidationReport` |

### skill/skillsync

10 个实现文件、19 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [applier.go](../../../skill/skillsync/applier.go) | `ApplyTransaction`、`ManifestConsumer`、`TransactionalManifestConsumer`、`StateConsumer`、`TransactionalStateConsumer`、`PresentationConsumer`、`TransactionalPresentationConsumer`、`ApplierOptions`、`ApplyResult`、`Applier` |
| [coordinator.go](../../../skill/skillsync/coordinator.go) | `PacketPublisher`、`VisibilityPolicy`、`AllowAllVisibility`、`CoordinatorOptions`、`CoordinatorMetrics`、`Coordinator` |
| [file_outbox.go](../../../skill/skillsync/file_outbox.go) | `FileOutboxStore`、`FileOutboxOptions` |
| [file_replace_unix.go](../../../skill/skillsync/file_replace_unix.go) | 函数/方法或内部实现；见源码 |
| [file_replace_windows.go](../../../skill/skillsync/file_replace_windows.go) | 函数/方法或内部实现；见源码 |
| [observability.go](../../../skill/skillsync/observability.go) | `MetricSink`、`HealthOptions`、`HealthStatus` |
| [outbox.go](../../../skill/skillsync/outbox.go) | `OutboxStore`、`OutboxDelete`、`BatchOutboxStore`、`OutboxRecord`、`RecordOutboxStore`、`BatchRecordOutboxStore`、`OutboxOptions`、`OutboxMetrics`、`Outbox` |
| [schema.go](../../../skill/skillsync/schema.go) | `SchemaRange`、`SchemaMigrator`、`SchemaMigration`、`SchemaRegistry` |
| [skillsync.go](../../../skill/skillsync/skillsync.go) | `RecordKind`、`Header`、`ManifestRecord`、`StateSnapshot`、`StateRecord`、`PresentationReset`、`PresentationRecord`、`Projector` |
| [visibility.go](../../../skill/skillsync/visibility.go) | `EntityVisibilityEvaluator`、`VisibilityField`、`FieldVisibilityEvaluator`、`EntityVisibilityPolicy` |

### spatial

4 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [block_index.go](../../../spatial/block_index.go) | `BlockIndex` |
| [geometry.go](../../../spatial/geometry.go) | `Point`、`Rect` |
| [pathfind.go](../../../spatial/pathfind.go) | `Terrain`、`PathOptions` |
| [terrain.go](../../../spatial/terrain.go) | `GridTerrain` |

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Host`、`Test.*Spawn`、`Test.*Checkpoint`、`Test.*Combat`。

### attribute

- [container_promises_test.go](../../../attribute/container_promises_test.go)：`TestSnapshotHandsOutACopy`、`TestEmptyLayerAnswersInsteadOfPanicking`、`TestZeroSelectorIsBase`、`TestLayersAreIsolated`
- [container_snapshot_promises_test.go](../../../attribute/container_snapshot_promises_test.go)：`TestSnapshotCopiesUnderTheContainerLock`

### skill

- [ability_overlay_bound_promises_test.go](../../../skill/ability_overlay_bound_promises_test.go)：`TestAbilityOverlayBoundSurvivesRestoreAndReleasesOnExpiry`
- [ability_test.go](../../../skill/ability_test.go)：`TestAbilityStateCooldownMutationClampsAndPreservesCast`、`TestAbilityStateReadAndMutationExecuteFromDSL`、`TestAbilityStateRejectsReadOnlyMutation`、`TestAbilityDisableOverlaysExpireIndependentlyAndDoNotCancelCast`、`TestAbilityAmmoMutationPreservesRechargeTimeline`；其余 7 项见文件
- [acceptance_test.go](../../../skill/acceptance_test.go)：`TestAllFixturesParseCompileInspectAndRun`
- [area_handoff_finish_promises_test.go](../../../skill/area_handoff_finish_promises_test.go)：`TestHandedOffAreaFinishEndsOnlyTheAreaSpawn`、`TestLiveAreaFinishStillFinishesTheCast`
- [area_test.go](../../../skill/area_test.go)：`TestTargetValidity`、`TestAreaMembership`、`TestAreaMembershipRotatingMembersRemainBounded`、`TestAreaCallbackFinishStopsRemainingSignals`、`TestAreaFinalLeaveFinishSuppressesTerminalCallback`；其余 1 项见文件
- [canonical_definition_promises_test.go](../../../skill/canonical_definition_promises_test.go)：`TestSourceDocumentDigestSeesEveryDifference`、`TestSourceDocumentDigestSeesTheEffectType`
- [carry_retry_promises_test.go](../../../skill/carry_retry_promises_test.go)：`TestFailedCarryDetachRetriesBeforeSpawnRetirement`
- [checkpoint_deterministic_bytes_promises_test.go](../../../skill/checkpoint_deterministic_bytes_promises_test.go)：`TestRuntimeCheckpointBytesAreDeterministic`、`TestRuntimeRestoresUnsortedLegacyCheckpointLists`
- [checkpoint_phase_timeout_promises_test.go](../../../skill/checkpoint_phase_timeout_promises_test.go)：`TestCheckpointRestoreRejectsPhaseTimeoutTask`
- [combat_test.go](../../../skill/combat_test.go)：`TestCombatPipelineResolvesMitigationElementCriticalShieldAndDeath`、`TestDamageImmuneIsSuccessfulAndTagsDoNotAlias`
- [compile_authority_test.go](../../../skill/compile_authority_test.go)：`TestCompileEnvironmentRejectsEmptyAuthority`、`TestAuthorityDigestChangesWithElementPolicy`、`TestVisualCatalogDoesNotChangeGameplayAuthorityDigest`、`TestAuthorityIdentityRequiresExactNonEmptyMatch`、`TestGameplayCatalogValidation`；其余 1 项见文件
- [compile_budget_test.go](../../../skill/compile_budget_test.go)：`TestBudgetRejectsRepeatAboveLimit`
- [compile_capture_context_promises_test.go](../../../skill/compile_capture_context_promises_test.go)：`TestCompileRejectsSnapshotsThatCannotBeCapturedWhereTheyAreRead`、`TestSnapshotsCapturedAtTheirPointStillRun`、`TestCompileRejectsPassivesThatCanNeverActivate`、`TestPassivesWithEventInputsStillActivate`、`TestCompileRejectsSpawnsOnEffectsThatDoNotSummon`；其余 3 项见文件
- [compile_graph_test.go](../../../skill/compile_graph_test.go)：`TestCompileRejectsInvalidPhaseGraphs`
- [compile_lifetime_test.go](../../../skill/compile_lifetime_test.go)：`TestCompileRejectsFallthroughEnterWithoutTimeout`、`TestCompileRejectsParallelFinish`
- [compile_memory_test.go](../../../skill/compile_memory_test.go)：`TestCompileRejectsMaybeInitializedMemoryRead`、`TestCompileAcceptsGuardedMemoryRead`、`TestCompileAcceptsInitializedMemoryRead`
- [compile_minion_spawn_promises_test.go](../../../skill/compile_minion_spawn_promises_test.go)：`TestMinionSpawnRejectsFieldsItNeverReads`、`TestMinionSpawnWithoutDurationCompilesAndLivesForTheSummonDuration`
- [compile_mutation_property_test.go](../../../skill/compile_mutation_property_test.go)：`TestCompiledMutationsNeverHitProgramInvariant`、`TestCompiledMutationsChangeDigestWhenProgramChanges`
- [compile_mutation_race_test.go](../../../skill/compile_mutation_race_test.go)
- [compile_pass_branches_test.go](../../../skill/compile_pass_branches_test.go)：`TestRandomPassBranches`、`TestTemporalPassBranches`、`TestGraphPassBranchesRunEveryReachablePhase`、`TestEffectResultPassBranches`、`TestProcPassResultFilterBranches`；其余 1 项见文件
- [compile_proc_test.go](../../../skill/compile_proc_test.go)：`TestProcRejectsRequiredExcludedTagConflict`、`TestProcRejectsDepthAboveEnvironmentLimit`、`TestProcResolvesElementAndDamageTypeHandles`
- [compile_random_test.go](../../../skill/compile_random_test.go)：`TestCompileRandomSitesAreDeterministic`
- [compile_runtime_agreement_promises_test.go](../../../skill/compile_runtime_agreement_promises_test.go)：`TestCompileRejectsMemoryEffectsOnUndeclaredOrNonIntMemory`、`TestDeclaredMemoryEffectsWriteTheirOwnSlot`、`TestCompileEnvironmentRejectsDuplicateCatalogKeys`、`TestCompileRejectsFieldsTheRuntimeDoesNotExecute`、`TestCompileRejectsUnknownCatalogNames`；其余 2 项见文件
- [compile_shape_test.go](../../../skill/compile_shape_test.go)：`TestCompilePassOrder`、`TestCompilePassOrderStopsAfterUpstreamError`、`TestShapeRejectsSelectOneWithMultipleResults`、`TestShapeAcceptsVisualAfterVisualPassIsDeployed`
- [compile_snapshot_test.go](../../../skill/compile_snapshot_test.go)：`TestAttributeSnapshotResolvesHandleAndPoint`、`TestAttributeSnapshotRejectsUnsupportedPoint`
- [compile_tags_test.go](../../../skill/compile_tags_test.go)：`TestGameplayTagsResolveDeclarableHandles`、`TestGameplayTagsRejectCompilerAndRuntimeOnlyDeclarations`、`TestGameplayTagsDamageDefaultsToNeutralElement`、`TestGameplayTagsRejectTrueDamageWithElement`
- [compile_tick_sign_promises_test.go](../../../skill/compile_tick_sign_promises_test.go)：`TestCompileRejectsNegativeTicksAtTheirField`、`TestCompileAcceptsZeroTicks`
- [compile_typecheck_test.go](../../../skill/compile_typecheck_test.go)：`TestCompileRejectsDamageToPosition`、`TestCompileRejectsUnknownLocal`、`TestCompileRejectsDamageAmountWithWrongType`、`TestCompileRejectsInputReferenceOutsideSchema`
- [completed_order_checkpoint_promises_test.go](../../../skill/completed_order_checkpoint_promises_test.go)：`TestCheckpointCarriesTheCompletionOrderSeparately`、`TestRestoreCompletedCastOrderPrefersTheRecordedOrder`、`TestRestoredCompletionOrderDecidesWhichCastRetentionEvicts`
- [effect_result_branch_spawn_promises_test.go](../../../skill/effect_result_branch_spawn_promises_test.go)：`TestEffectResultBranchMayStartASpawnWithoutCallbacks`、`TestEffectResultBranchDiagnosticNamesSpawnCallbacks`
- [effect_result_test.go](../../../skill/effect_result_test.go)：`TestEffectResultDamageSuccessExecutesTypedBranch`、`TestEffectResultStateChangeCarriesDynamicBeforeAndAfter`、`TestEffectResultBlockedCombatStillUsesSuccessOutcome`、`TestEffectExpectedFailureFromMemoryHostUsesFailureBranch`、`TestEffectResultScopeRejectsInvalidFieldOutcomeAndEscape`；其余 9 项见文件
- [eval_context_promises_test.go](../../../skill/eval_context_promises_test.go)：`TestCompileRejectsMemoryDefaultsReadingMemoryDeterministically`、`TestMemoryDefaultsReadingTheCastStillRun`、`TestCompileRejectsStateDefaultsThatCannotBeEvaluatedEverywhere`、`TestStateDefaultOfTheCasterRunsInCallbacks`、`TestCompileRejectsOptionalEntitiesInCachedReads`；其余 3 项见文件
- [eval_contexts_table_test.go](../../../skill/eval_contexts_table_test.go)：`TestEvalContextTableEveryCellHasACase`、`TestEvalContextTableCellsAgreeWithCompilerAndRuntime`、`TestRuntimeEvaluatesReferencesOnlyWhereTheTableAllows`、`TestSpawnStepPrimaryTargetIsRejectedAtCompileTime`、`TestEvalContextTableRejectsTheO33DriftCellsWithAnAlternative`；其余 2 项见文件
- [executor_flow_promises_test.go](../../../skill/executor_flow_promises_test.go)：`TestExecutorRefusesNonBoolConditionsAndStopsRepeatOnBodyOutcome`、`TestExecutorRefusesSelectorsAndIterationTasksOutsideTheProgram`、`TestMemoryHostReadsAndDamageRefuseUnknownEntitiesAndForeignCatalogs`
- [executor_promises_test.go](../../../skill/executor_promises_test.go)：`TestStartRefusesEachCorruptedProgramShape`
- [fixed_math_test.go](../../../skill/fixed_math_test.go)：`TestMulDivRoundedMatchesReference`、`TestIntegerDistanceMatchesReference`、`TestMotionMetricIsEuclidean`、`TestIsqrt64Floor`
- [fuzz_test.go](../../../skill/fuzz_test.go)
- [generated_definitions_compile_test.go](../../../skill/generated_definitions_compile_test.go)：`TestGeneratedSkillDefinitionsCompileWithoutDiagnostics`
- [host_boundary_remaining_promises_test.go](../../../skill/host_boundary_remaining_promises_test.go)：`TestNilHostAdvanceAndPassiveReturnError`、`TestCheckpointCapabilityFailureKeepsSentinel`、`TestHostAdmissionCacheRemainsBounded`
- [host_capability_promises_test.go](../../../skill/host_capability_promises_test.go)：`TestHostCapabilityMissingIsRejectedAtCompileTime`、`TestRuntimeRefusesProgramsOutsideTheHostTable`、`TestRestoreRefusesProgramsOutsideTheHostTable`、`TestMemoryHostCapabilitiesMatchItsBehavior`、`TestCheckHostCapabilitiesCatchesMisdeclaredHosts`；其余 5 项见文件
- [host_capability_required_promises_test.go](../../../skill/host_capability_required_promises_test.go)：`TestHostInterfaceRequiresTheCapabilityTable`、`TestWrappingHostsForwardTheWrappedCapabilities`、`TestEmptyTableRefusesEveryRequirement`、`TestNoHostCapabilitySkipBranch`
- [host_probe_remaining_promises_test.go](../../../skill/host_probe_remaining_promises_test.go)：`TestCapabilityProbeIsNeutralAndStopsItsSpawn`、`TestCapabilityProbeUsesRuntimeHandleOnlyPayment`、`TestDefaultMemoryHostSupportsItsDeclaredResourceHandles`
- [identity_test.go](../../../skill/identity_test.go)：`TestProgramIdentityAndIndicesAreDeterministic`、`TestProgramIdentitySeparatesGameplayAndPresentation`、`TestVisualCatalogIdentityDoesNotChangeGameplayDigest`、`TestProgramIdentityIgnoresMemoryMapInsertionOrder`
- [input_test.go](../../../skill/input_test.go)：`TestInputSchemaVisibilityAndValidation`、`TestInputClampRangeDragAndBlockedPosition`、`TestInputPathBoundsSimplificationAndSnapshot`、`TestInputUpdateReplacesOnlyPortFieldAndPreservesCommitState`
- [inspect_test.go](../../../skill/inspect_test.go)：`TestInspectReturnsStableCopies`
- [lower_lookup_promises_test.go](../../../skill/lower_lookup_promises_test.go)：`TestLowerRefusesEveryUnresolvedLookup`
- [lower_test.go](../../../skill/lower_test.go)：`TestProgramDoesNotAliasDefinition`、`TestLowerProducesTypedDamageOperation`、`TestLowerPreservesStableInputSelectionAndRandomTables`
- [memory_host_promises_test.go](../../../skill/memory_host_promises_test.go)：`TestMemoryHostRefusesBackwardTicksAndIllegalPayments`
- [memory_host_test.go](../../../skill/memory_host_test.go)：`TestHostCommandPayloadsAreNarrowAndTyped`、`TestMemoryHostPayCostsIsAtomic`、`TestMemoryHostSelectIsDeterministic`、`TestMemoryHostRaycastUsesHitDistanceThenStableCollider`、`TestMemoryHostStopSpawnIsIdempotent`；其余 1 项见文件
- [motion_test.go](../../../skill/motion_test.go)：`TestMotionPipelineHasFixedStageOrder`、`TestMotionLinearTrajectoryRoundsHalfAwayFromZero`、`TestMotionPipelineAggregatesStageSignalsAndEmitsTargetLostOnce`、`TestMotionPayloadsControlTrajectoryAndSteering`、`TestMotionFixedSteeringAndOffsetsUseStableTrajectoryBase`；其余 12 项见文件
- [normalize_test.go](../../../skill/normalize_test.go)：`TestNormalizeCreatesTypedDamageIR`、`TestNormalizeDefaultsTapPolicy`、`TestNormalizeWalkValuesVisitsEachSourceOnce`、`TestNormalizeDoesNotRetainMutableWireData`、`TestNormalizeRejectsInvalidIdentifiersWithSourcePath`；其余 1 项见文件
- [numeric_test.go](../../../skill/numeric_test.go)：`TestNumericPropertyCatalogIsClosed`、`TestModifySpawnRejectsInvalidOwnershipOrValue`、`TestModifySpawnIsCallbackScopedAndDoesNotEmitWorldEffect`、`TestNumericCatalogAuthorityValidation`、`TestNumericCatalogRequiresExactCanonicalPolicyFacts`；其余 2 项见文件
- [optional_test.go](../../../skill/optional_test.go)：`TestCompileRejectsExistsOnNonOptionalValue`
- [owned_entity_test.go](../../../skill/owned_entity_test.go)：`TestOwnedEntitySummonRegistersAuthoritativeIdentityAndTypedResult`、`TestOwnedEntitySummonBindingsAreTypedClampedAndCapabilityChecked`、`TestOwnedReplacementPoliciesAreStableAndAtomic`、`TestOwnedReplacementDistanceTieUsesSummonSequenceThenEntityID`、`TestOwnedReplacementLimitsPerSourceSkillAndTeam`；其余 14 项见文件
- [parse_exact_keys_promises_test.go](../../../skill/parse_exact_keys_promises_test.go)：`TestParseRejectsCaseVariantKeys`、`TestParseKeepsNameKeyedMapsAndCanonicalKeys`
- [parse_test.go](../../../skill/parse_test.go)：`TestParseGeneratedAcceptsDirectSkillRoot`、`TestParseGeneratedRejectsSkillJSONWrapper`、`TestParseGeneratedAcceptsRejection`、`TestParseRejectsDuplicateNestedKey`、`TestParseRejectsNonCanonicalJSON`；其余 4 项见文件
- [phase_event_dispatch_promises_test.go](../../../skill/phase_event_dispatch_promises_test.go)：`TestCompileRejectsPhaseEventsTheRuntimeNeverDispatches`、`TestPhaseTimeoutTicksDoNotExcuseEnterFallthrough`、`TestNonzeroPhaseTimeoutTicksWarnsThatItIsNotEnforced`
- [phase_events_promises_test.go](../../../skill/phase_events_promises_test.go)：`TestPhaseEventTableIsTheSingleSource`
- [presentation_asset_cache_cancel_promises_test.go](../../../skill/presentation_asset_cache_cancel_promises_test.go)：`TestVisualPlanCacheWaiterSurvivesCreatorCancellation`、`TestVisualAssetWaiterSurvivesCreatorCancellation`、`TestVisualPlanCacheWaiterSeesRealLoadFailureAndOwnCancellation`
- [presentation_asset_cache_test.go](../../../skill/presentation_asset_cache_test.go)：`TestVisualPlanCacheTrustFallbackAndRelease`、`TestVisualPlanCacheRejectsUntrustedCatalog`、`TestVisualPlanCacheRejectsDigestCollision`、`TestVisualPlanCacheSharesAssetUntilLastPlanEviction`、`TestVisualPlanCacheSharesResolvedFallbackAcrossDifferentPrimaryKeys`；其余 1 项见文件
- [presentation_assets_test.go](../../../skill/presentation_assets_test.go)：`TestResolvePresentationPlanValidatesCatalogAndDetachesAssets`
- [presentation_test.go](../../../skill/presentation_test.go)：`TestPresentationPlanExposesCastAndEffectMounts`、`TestRuntimePresentationEventsAreCommittedOrderedAndPollable`、`TestPresentationPollingReportsRetentionLoss`、`TestSpawnVisualCompilesAndEmitsLifecycle`、`TestRuntimeDoesNotEmitEffectPresentationForExpectedFailure`
- [production_limits_test.go](../../../skill/production_limits_test.go)：`TestParseLimitsRejectWorkBeforeSemanticDecode`、`TestRuntimeRetentionIsBounded`、`TestClockMutationCarriesDerivedTimeWithoutEntityFanout`
- [promises_test.go](../../../skill/promises_test.go)：`TestRestoreRuntimeKeepsTheEventsEmittedAfterTheCheckpoint`、`TestAWaitingAcquireIsNotEvictedByTheFirstHoldersRelease`、`TestReferencedCompletedCastsSurviveTheRetentionBound`、`TestRestoreRefusesACheckpointWhosePayloadWasTampered`
- [prompt_test.go](../../../skill/prompt_test.go)：`TestPromptContractsDescribeCanonicalSkillV2`
- [quantity_test.go](../../../skill/quantity_test.go)：`TestCompileRejectsQuantityMismatchInDamageAmount`、`TestCompileAcceptsCombatAmountAttributeForDamage`
- [random_test.go](../../../skill/random_test.go)：`TestRuntimeRandomSelectionIgnoresCandidateInsertionOrder`、`TestBoundedRandomIsDeterministicAndBounded`
- [replay_optional_promises_test.go](../../../skill/replay_optional_promises_test.go)：`TestRecordingReplayPreserveOwnedSummonLifecycle`、`TestRecordingReplayForwardOptionalWorldViews`
- [replay_test.go](../../../skill/replay_test.go)：`TestRecordingAndReplayHostMatchTypedInteractionOrder`
- [runtime_cast_exclusivity_test.go](../../../skill/runtime_cast_exclusivity_test.go)：`TestCasterWindowExclusivityRejectsSecondRootCast`、`TestGlobalCooldownGatesRootCastsAcrossSkills`
- [runtime_cast_policy_test.go](../../../skill/runtime_cast_policy_test.go)：`TestCastPolicyTogglePulsesAndSecondActivateReleases`、`TestHoldReleaseStopsPulsesAndStartsCooldown`、`TestChargeBelowMinimumCancelsAndSuccessfulReleasePaysOnce`、`TestAmmoPaymentStockAndSingleRechargeTimeline`、`TestToggleSustainFailureReleasesAndCancelStartsCooldown`；其余 2 项见文件
- [runtime_cast_terminal_branches_promises_test.go](../../../skill/runtime_cast_terminal_branches_promises_test.go)：`TestInterruptSpawnStopFailureStillEndsTheCast`、`TestToggleReleaseCallbackFailureStillEndsTheCast`、`TestFailedChargeStartWithOwnedSpawnLeavesNoResidue`、`TestFailedChargeStartWhoseSpawnCannotStopKeepsItsCast`
- [runtime_cast_terminal_promises_test.go](../../../skill/runtime_cast_terminal_promises_test.go)：`TestFailedStartLeavesNoScheduledWorkForTheReusedCastID`、`TestCheckpointAfterFailedStartRestores`、`TestCancelCallbackFailureStillEndsTheCast`、`TestChargeReleaseFailureStillEndsTheCast`、`TestFailedToggleReleasesItsPolicySlot`
- [runtime_cast_window_expression_test.go](../../../skill/runtime_cast_window_expression_test.go)：`TestCastWindowExpressionsAreClampedToDeclaredBounds`、`TestCastWindowExpressionShapeRules`、`TestExternallyAuthoredHasteAttributeDrivesWindup`
- [runtime_cast_window_test.go](../../../skill/runtime_cast_window_test.go)：`TestCastWindowUsesExactPrepareCommitExecuteRecoveryTimeline`、`TestCastWindowCancelBeforeCommitUsesRefundPolicy`
- [runtime_checkpoint_test.go](../../../skill/runtime_checkpoint_test.go)：`TestRuntimeCheckpointRestoresActiveTimelineDeterministically`、`TestRuntimeCheckpointRestoresActiveSpawns`、`TestRuntimeCheckpointFailsClosed`、`TestRuntimeRestoreRequiresFullRecoveryForOmittedDeliveryBuffers`、`TestRuntimeCheckpointRestoresPendingPassiveActivation`
- [runtime_dispatch_failure_promises_test.go](../../../skill/runtime_dispatch_failure_promises_test.go)：`TestUncoverablePassiveCandidateDoesNotStallTheEventStream`、`TestCostPaymentDispatchFailureDoesNotDropAPaidCast`
- [runtime_dispatch_test.go](../../../skill/runtime_dispatch_test.go)：`TestQueueExternalEventRoutesWithoutRecursiveActivation`、`TestRouterCandidatesAreNormalizedBeforeEnqueue`
- [runtime_effect_result_test.go](../../../skill/runtime_effect_result_test.go)：`TestHostPayloadCarriesSuccessDataUsesTypedZeroChecks`
- [runtime_event_test.go](../../../skill/runtime_event_test.go)：`TestDerivedEventPreservesRootAndSetsParent`、`TestEventContextCopiesSortsAndDeduplicatesTags`、`TestCollectHostEventsSkipsEventWhenEveryRootIsPinned`
- [runtime_failed_cast_retention_promises_test.go](../../../skill/runtime_failed_cast_retention_promises_test.go)：`TestUncommittedFailedCastsStayWithinTheCompletedCastLimit`、`TestFailedStartLeavesNoCompletedQueueEntry`
- [runtime_mutation_benchmark_test.go](../../../skill/runtime_mutation_benchmark_test.go)
- [runtime_mutation_remove_identity_promises_test.go](../../../skill/runtime_mutation_remove_identity_promises_test.go)：`TestRemoveMutationsCarryTheEntityTheirUpsertCarried`
- [runtime_mutation_verify_test.go](../../../skill/runtime_mutation_verify_test.go)
- [runtime_proc_test.go](../../../skill/runtime_proc_test.go)：`TestPassiveActivationQueuesThenAppliesFilterAndOncePerRootLedger`、`TestPassiveSelfTriggerAndDepthAreBounded`、`TestPassiveActivationPerTickLimitSuppressesWithoutCreatingCast`
- [runtime_queued_task_bound_promises_test.go](../../../skill/runtime_queued_task_bound_promises_test.go)：`TestQueueExternalEventRejectsAtTheQueueBoundInsteadOfFillingTheRootTable`、`TestPassiveCandidatesPastTheQueueBoundAreRejectedNotDropped`、`TestAbilityOverlayExpiriesDoNotPinRoots`、`TestTasksOfEndedCastsDoNotPinRoots`、`TestLegalConfigurationNeverReachesTheRootTableFallback`；其余 1 项见文件
- [runtime_regression_v141_test.go](../../../skill/runtime_regression_v141_test.go)：`TestAmmoReadAfterRechargeKeepsCheckpointHealthy`、`TestCancelledToggleCanBeActivatedAgain`、`TestPersistentRemoveOrderingIsDeterministic`、`TestRestoreRuntimePreservesPostCheckpointEvents`
- [runtime_root_event_bound_promises_test.go](../../../skill/runtime_root_event_bound_promises_test.go)：`TestNewRuntimeRejectsRootEventLimitNotAboveReferencedRootBound`、`TestRestoreRuntimeRejectsCheckpointWhoseRootEventLimitIsNotAboveTheBound`
- [runtime_spawn_abandon_promises_test.go](../../../skill/runtime_spawn_abandon_promises_test.go)：`TestStopSweepAbandonsAtTheStopPendingLimit`、`TestAbandonedSpawnsArePrunedOnlyAtTheEndOfAdvance`、`TestAbandonedSpawnIsVisibleToSyncConsistently`
- [runtime_spawn_retention_promises_test.go](../../../skill/runtime_spawn_retention_promises_test.go)：`TestCastsWhoseSpawnsEndedStayWithinTheCompletedCastLimit`、`TestHandedOffSpawnsBeyondTheCompletedLimitStillRestore`
- [runtime_spawn_stop_retry_promises_test.go](../../../skill/runtime_spawn_stop_retry_promises_test.go)：`TestFailedStartRetriesTheStopItCouldNotFinish`、`TestStopPendingSurvivesCheckpointAndKeepsRetrying`、`TestPinnedCastsBeyondTheCompletedLimitStillRestore`、`TestStopRetriesAreBoundedAndAlertWhenExhausted`、`TestStopPendingIsVisibleToSyncConsistently`；其余 1 项见文件
- [runtime_spawn_stop_sweep_promises_test.go](../../../skill/runtime_spawn_stop_sweep_promises_test.go)：`TestStopSweepSkipsSpawnsDroppedAtTheStopPendingLimit`
- [runtime_sync_test.go](../../../skill/runtime_sync_test.go)：`TestRuntimeValueJSONRoundTrip`、`TestRuntimeStateSnapshotAndEventCursorAreComplete`、`TestStateMutationsFoldExactlyIntoLaterSnapshot`、`TestStateMutationsAreCommittedBeforeWriteReturns`
- [runtime_test.go](../../../skill/runtime_test.go)：`TestRuntimeActivateExecutesTypedDamage`、`TestRuntimeExecutesSequenceRepeatIfAndMemory`、`TestRuntimeRejectsAuthorityMismatchWithoutSideEffects`、`TestRuntimeDoesNotRetainOrInterpretDefinition`、`TestRuntimeRejectsCompilerSemanticsAndInputBeforeSideEffects`；其余 5 项见文件
- [runtime_value_redaction_test.go](../../../skill/runtime_value_redaction_test.go)：`TestRedactRuntimeValueRecursivelyFiltersSensitiveReferences`
- [scheduler_test.go](../../../skill/scheduler_test.go)：`TestSchedulerOrdersByDueTickThenSequence`、`TestRuntimeWaitExecutesExactlyAtDueTick`、`TestRuntimeRepeatIntervalExecutesExactCount`、`TestRuntimeSelectEachSuspensionRetainsIndependentLocalFrames`、`TestRuntimeStalePhaseTaskReleasesFrameOnce`
- [spawn_lifecycle_e2e_promises_test.go](../../../skill/spawn_lifecycle_e2e_promises_test.go)：`TestSpawnLifecycleSettlesEveryTickFromCastToHandoffToStop`、`TestRefusedMotionStartStepLeavesNoHostSpawn`、`TestSpawnCallbacksShareTheCastRootForOncePerRoot`、`TestProcCastSpawnCallbacksKeepDepthAndRoot`、`TestCastingAreaFinishAtALaterTickFinishesTheCast`；其余 1 项见文件
- [spawn_partition_promises_test.go](../../../skill/spawn_partition_promises_test.go)：`TestSpawnPartitionsFollowRecordFields`、`TestSpawnPartitionWritesStayInSpawnTable`
- [spawn_scope_contract_test.go](../../../skill/spawn_scope_contract_test.go)：`TestEntityCarrySurvivesGotoAndNormalFinish`
- [spawn_stop_entries_promises_test.go](../../../skill/spawn_stop_entries_promises_test.go)：`TestEveryStopEntryDefersARefusedStopTheSameWay`、`TestSpawnStopEntriesAreRegistered`
- [spawn_test.go](../../../skill/spawn_test.go)：`TestSpawnStopIsUnifiedAndIdempotent`、`TestSpawnSignalsUseCanonicalOrder`
- [state_null_default_promises_test.go](../../../skill/state_null_default_promises_test.go)：`TestNullDefaultEntityStateCanBeSet`
- [state_test.go](../../../skill/state_test.go)：`TestPersistentStateSchemaLowersStablePrivateLayout`、`TestPersistentStateRejectsSharedDeclarationDuplicateEnumAndInvalidSnapshotScope`、`TestSharedStateUnknownHandleIsRejected`、`TestPersistentStateTypedReadAndMutationExecuteAcrossCasts`、`TestPersistentStatePrivateIdentityAndOwnerTargetIsolation`；其余 4 项见文件
- [status_instance_test.go](../../../skill/status_instance_test.go)：`TestStatusSelectFiltersAndStableOrders`、`TestStatusOperationClosedSetClampCopyTransferAndExpiry`、`TestStatusDurationClampAppliesAtCreationAndCopy`、`TestStatusBatchSnapshotAndConsumerCannotSuspend`、`TestShieldStatusIsQueryableAndEmitsSingleAbsorbBreak`；其余 3 项见文件
- [status_test.go](../../../skill/status_test.go)：`TestStatusRefreshStacksAndExpiresAtLogicalTick`、`TestAttributeModifierUsesAddThenMultiplyOrdering`、`TestAttributeModifierUsesCatalogRounding`、`TestStatusHonorsImmunityTenacitySourceRemovalAndDispel`
- [temporal_test.go](../../../skill/temporal_test.go)：`TestTemporalProfileCompileRules`、`TestTemporalCatalogRejectsDuplicateProfileKey`、`TestTemporalCaptureFreezesProfileAuthorization`、`TestTemporalResultLayoutsAcceptHostExpectedFailures`、`TestTemporalRestoreResultProjectsFieldLists`；其余 9 项见文件
- [test_helpers_test.go](../../../skill/test_helpers_test.go)
- [trace_test.go](../../../skill/trace_test.go)：`TestTraceIsBoundedAndDoesNotChangeGameplay`、`TestTraceSinkIsDrainedOutsideGameplayAndFailuresAreIgnored`
- [visual_test.go](../../../skill/visual_test.go)：`TestVisualManifestCompilesCanonicalVisual`、`TestVisualRejectsInvalidSchemaAndCatalogBindings`、`TestVisualTableDeduplicatesAndDoesNotChangeGameplayIdentity`
- [world_revision_test.go](../../../skill/world_revision_test.go)：`TestMemoryHostRevisionAndEventCursorContract`

### skill/combat

- [combat_test.go](../../../skill/combat/combat_test.go)：`TestAttributeSetGrantRevokeIsExactlyReversible`、`TestAttributeSetObserverAndSnapshotOrder`、`TestBuffContainerStackRefreshDispelAndImmunity`、`TestBuffContainerTenacityAndExpiry`、`TestBuffExtendPolicyAccumulatesDuration`；其余 9 项见文件
- [remaining_promises_test.go](../../../skill/combat/remaining_promises_test.go)：`TestAvoidancePreservesCriticalHook`、`TestVampOnlyHealsMissingHealthOfLivingSource`、`TestDamageRejectsInvalidNegativeHealth`、`TestModifierOverflowRemainsReversibleAndOrderIndependent`、`TestReapplyBuffPermanentAndLowerStackLimit`

### skill/combatcomponent

- [adapter_test.go](../../../skill/combatcomponent/adapter_test.go)：`TestHostAdapterAppliesDamageWithEvents`、`TestHostAdapterHealShieldAndReads`、`TestHostAdapterShieldReportsEffectiveDeltaAndSkipsNoOpCommit`、`TestHostAdapterPayCostsIsAtomic`
- [attribute_projection_promises_test.go](../../../skill/combatcomponent/attribute_projection_promises_test.go)：`TestBuffAttributeModifierReachesDamage`、`TestAttributeProjectionRollsBackWithTheDao`、`TestAttributeProjectionReprojectsOnLoad`
- [combatant_copy_promises_test.go](../../../skill/combatcomponent/combatant_copy_promises_test.go)：`TestCombatantCopiesDoNotShareElementMultipliers`
- [component_test.go](../../../skill/combatcomponent/component_test.go)：`TestNestUndoRollbackRestoresCombatStateExactly`、`TestCombatDaoPersistenceRoundTrip`、`TestNestUndoWorksThroughRealHandlers`、`TestCombatDaoProducesTransactionLocalMutationAndSyncMask`、`TestCombatMutationOutsideTransactionPanicsBeforeStateChange`
- [dao_rollback_promises_test.go](../../../skill/combatcomponent/dao_rollback_promises_test.go)：`TestCombatRollbackIsTheDaoRollback`
- [detached_indeterminate_fence_promises_test.go](../../../skill/combatcomponent/detached_indeterminate_fence_promises_test.go)：`TestHostAdapterApplyInMemoryHandlerFencesOnIndeterminateOutcome`
- [guards_promises_test.go](../../../skill/combatcomponent/guards_promises_test.go)：`TestDetachedTransactionFailuresAreReturnedNotDereferenced`、`TestReadsAndPaymentsRefuseUnknownEntitiesAndUnmappedResources`、`TestCombatDaoRefusesForeignPersistedStateAndEmptyPatches`、`TestStatusBridgeRefusesToModifyABuffWithoutACatalogPolicy`
- [host_capability_promises_test.go](../../../skill/combatcomponent/host_capability_promises_test.go)：`TestBusinessHostOverHostAdapterMatchesItsCapabilities`、`TestHostAdapterWithoutStatusBridgeDeclaresNoModifiers`、`TestHostAdapterUnmappedResourceIsNamedAtStartup`、`TestHostAdapterRefusesAttributesOutsideTheCatalog`
- [remaining_benchmark_test.go](../../../skill/combatcomponent/remaining_benchmark_test.go)
- [remaining_promises_test.go](../../../skill/combatcomponent/remaining_promises_test.go)：`TestNoChangeCombatDoesNotMarkDirty`、`TestAbsentResourceMappingReturnsErrors`、`TestResourceReadMatchesSpendableBase`、`TestHostAdapterRejectsUnsupportedCombatCatalog`、`TestStatusStackOverflowAndPermanentRefresh`
- [resource_promises_test.go](../../../skill/combatcomponent/resource_promises_test.go)：`TestResourceCommandRefusesEachIllegalOperation`、`TestPayCostsRefusesNegativeOverflowingAndComponentlessPayments`
- [status_bridge_test.go](../../../skill/combatcomponent/status_bridge_test.go)：`TestStatusBridgeAppliesStacksAndModifiers`、`TestStatusBridgeImmunityTenacityAndReplace`、`TestStatusBridgeRemoveAndDispel`、`TestStatusBridgeAttributeModifierIsIndependent`、`TestHostAdapterResourceCommand`；其余 2 项见文件

### skill/integration/sync-e2e

- [e2e_test.go](../../../skill/integration/sync-e2e/e2e_test.go)：`TestCrashRecoveryThroughConfirmedFragmentedTransport`
- [soak_test.go](../../../skill/integration/sync-e2e/soak_test.go)：`TestProtocolSoak`

### skill/skillcompose

- [contract_promises_test.go](../../../skill/skillcompose/contract_promises_test.go)：`TestBuildContractRefusesEachInvalidInput`、`TestValidateContractRefusesEachStructuralDefect`
- [contract_test.go](../../../skill/skillcompose/contract_test.go)：`TestContractCallerPolicyOnlyTightens`、`TestContractCanonicalOmitsDerivedPromptFields`、`TestCanonicalContractDoesNotMutateCaller`、`TestBuildContractRejectsUnboundSourceAuthority`
- [effect_sink_promises_test.go](../../../skill/skillcompose/effect_sink_promises_test.go)：`TestGrantedNonDamageEffectsAreValidCandidates`
- [matcher_test.go](../../../skill/skillcompose/matcher_test.go)：`TestMatcherIsDeterministic`
- [profile_test.go](../../../skill/skillcompose/profile_test.go)：`TestProfileExtractionIsStableAndUsesInspectorFacts`
- [validator_provenance_promises_test.go](../../../skill/skillcompose/validator_provenance_promises_test.go)：`TestValidateCandidateExplainsBlankOrDuplicateSources`
- [validator_test.go](../../../skill/skillcompose/validator_test.go)：`TestValidateCandidateRejectsUngrantableGrowthAndDisconnectedFlow`、`TestValidateCandidateRejectsUngroundedFeatureOrigin`

### skill/skillsync

- [applier_epoch_admission_promises_test.go](../../../skill/skillsync/applier_epoch_admission_promises_test.go)：`TestRejectedFullPacketDoesNotWedgeTheApplier`
- [applier_promises_test.go](../../../skill/skillsync/applier_promises_test.go)：`TestNewApplierRefusesUnservableSchemas`、`TestApplierAdmissionRefusesEachMalformedPacket`、`TestApplierRecordRefusesHeaderAndDigestDisagreement`
- [applier_state_promises_test.go](../../../skill/skillsync/applier_state_promises_test.go)：`TestApplierRefusesConcurrentApplyStatesAndMisshapenRecords`
- [coordinator_durability_test.go](../../../skill/skillsync/coordinator_durability_test.go)：`TestCoordinatorRepairsPartialOutboxDeleteFailure`、`TestCoordinatorAckKeepsHistoryWhenOutboxDeleteFails`、`TestCoordinatorRejectsAckAheadBeforeDeletingOutbox`、`TestCoordinatorRepairsOutboxWhenHistoryAckFails`、`TestCoordinatorCloseKeepsHistoryWhenOutboxDeleteFails`
- [coordinator_resources_promises_test.go](../../../skill/skillsync/coordinator_resources_promises_test.go)：`TestAcquireRechecksObserverAfterWaiting`、`TestCoordinatorExplicitLifecycleCapacityAndManifestIdentity`、`TestCoordinatorCloseMustFinishBeforeReopen`、`TestClosedObserverIdentitiesDoNotAccumulate`、`TestRegisteredManifestPlansAreBounded`
- [file_outbox_tmp_cleanup_promises_test.go](../../../skill/skillsync/file_outbox_tmp_cleanup_promises_test.go)：`TestFileOutboxOpenRemovesCrashLeftoverTemporaryFiles`
- [outbox_age_contract_test.go](../../../skill/skillsync/outbox_age_contract_test.go)：`TestOutboxRefusesStoreWithoutPersistentAge`
- [outbox_batch_consistency_promises_test.go](../../../skill/skillsync/outbox_batch_consistency_promises_test.go)：`TestPutAndPutBatchReachTheSameAdmissionVerdict`
- [outbox_benchmark_test.go](../../../skill/skillsync/outbox_benchmark_test.go)
- [outbox_lifecycle_promises_test.go](../../../skill/skillsync/outbox_lifecycle_promises_test.go)：`TestOutboxFailureBlocksLaterPacketsInSameStream`、`TestOverageOutboxCanRestartPublishAndAcknowledge`、`TestIdleSweepPreservesUnacknowledgedDelivery`、`TestDiscardObserverCannotSucceedWithPublishInFlight`、`TestEpochRotationRetiresOldOutboxBeforeRetry`
- [outbox_production_test.go](../../../skill/skillsync/outbox_production_test.go)：`TestPublishPersistsRetryMetadata`、`TestPublishDueHonorsBatchLimit`、`TestConcurrentPublishDueDoesNotPublishSamePacket`、`TestPublishDueJoinsPublisherAndMetadataErrors`、`TestFileOutboxEnforcesRecordLimitOnPut`
- [presentation_reset_spawn_promises_test.go](../../../skill/skillsync/presentation_reset_spawn_promises_test.go)：`TestPresentationResetSpawnEntryMatchesItsIncrementalEvent`
- [publish_metrics_promises_test.go](../../../skill/skillsync/publish_metrics_promises_test.go)：`TestConcurrentCoordinatorMetricsCountEachPublishOnce`
- [schema_negotiation_promises_test.go](../../../skill/skillsync/schema_negotiation_promises_test.go)：`TestNegotiateSchemaRequiresBothRangesToContainTheResult`
- [schema_test.go](../../../skill/skillsync/schema_test.go)：`TestSchemaRegistryMigratesDeterministicShortestChain`、`TestSchemaRegistryRejectsInvalidAndMissingMigrations`
- [skillsync_test.go](../../../skill/skillsync/skillsync_test.go)：`TestProjectorBuildsStronglyTypedPackets`、`TestApplierValidatesChainObserverAndManifestDependency`、`TestCoordinatorRetainsPublishFailureForRecovery`、`TestCoordinatorRequiresExplicitVisibilityPolicy`、`TestCoordinatorReclaimsViewLocksAndRequiresExplicitReopen`；其余 9 项见文件
- [source_cursor_promises_test.go](../../../skill/skillsync/source_cursor_promises_test.go)：`TestSnapshotStartsPresentationAtCurrentRuntime`、`TestOutboxFailureDoesNotAppendTheSameSourceAgain`、`TestFullRecoveryAdvancesSourceCursor`
- [visibility_clock_promises_test.go](../../../skill/skillsync/visibility_clock_promises_test.go)：`TestVisibilityClockAndUnknownMutationFailClosed`
- [visibility_recovery_promises_test.go](../../../skill/skillsync/visibility_recovery_promises_test.go)：`TestPresentationResetFromRecoverHonoursVisibility`、`TestPresentationResetFromExpiredCursorHonoursVisibility`、`TestStateSnapshotAndDeltasHideTheSameAbility`、`TestRemoveMutationsOfInvisibleEntitiesAreFiltered`

### spatial

- [block_index_promises_test.go](../../../spatial/block_index_promises_test.go)：`TestBlockRectsStayInsideBoundsAtTheInt64Edge`
- [remaining_promises_test.go](../../../spatial/remaining_promises_test.go)：`TestPathBudgetCountsExpandedCells`、`TestGridSearchSeesOneObstacleView`
- [spatial_test.go](../../../spatial/spatial_test.go)：`TestGridTerrainAtomicMovesAndBounds`、`TestDistanceOperationsDoNotOverflow`、`TestGridTerrainConcurrentAccessKeepsWritesConsistent`、`TestFindPath`、`TestFindPathDistinguishesBudgetFromNoPath`；其余 3 项见文件

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../maintenance/PERFORMANCE.md)，没有新测量则不能改容量承诺。
