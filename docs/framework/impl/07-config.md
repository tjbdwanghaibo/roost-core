# 配置、数据表与热更：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/07-config.md)

## 如何阅读

服务器地址、连接超时是进程配置；怪物血量、掉落表是游戏数据。两者的加载和更新规则不同。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`framework/configdata`、`infra/base/featureflag`、`framework/hotcode`、`internal/configschema`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 配置声明、读取、生成与 doctor 同源。
2. 异步准入捕获快照；同步链保持代际。
3. 热更失败恢复此前真实视图；敏感值不泄露。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### configdata

6 个实现文件、9 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [auto.go](../../../framework/configdata/auto.go) | `AutoOption` |
| [configdata.go](../../../framework/configdata/configdata.go) | `Name`、`Snapshot`、`Table`、`BuildContext`、`IndexDef`、`TableDef`、`ObjectDef`、`CustomDef`、`Registry`、`ReloadEvent`、`ReloadListener`、`ReloadHook`、`Store`、`ReloadStage`、`ReloadOutcome` |
| [doc.go](../../../framework/configdata/doc.go) | 函数/方法或内部实现；见源码 |
| [external.go](../../../framework/configdata/external.go) | `ExternalOption` |
| [fieldrules.go](../../../framework/configdata/fieldrules.go) | `FieldRule`、`RuleError` |
| [keyspelling.go](../../../framework/configdata/keyspelling.go) | 函数/方法或内部实现；见源码 |

### configdata/rules

1 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [rules.go](../../../framework/configdata/rules/rules.go) | `Rule`、`Error` |

### featureflag

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [flags.go](../../../infra/base/featureflag/flags.go) | `Flag`、`Store` |

### hotcode

4 个实现文件、8 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [admin.go](../../../framework/hotcode/admin.go) | `RevertCommand`、`LoadPluginCommand` |
| [plugin.go](../../../framework/hotcode/plugin.go) | `Bundle` |
| [plugin_stub.go](../../../framework/hotcode/plugin_stub.go) | `Bundle` |
| [registry.go](../../../framework/hotcode/registry.go) | `Meta`、`PointInfo`、`Registry` |

### internal/configschema

5 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [check.go](../../../internal/configschema/check.go) | `Source` |
| [guard.go](../../../internal/configschema/guard.go) | 函数/方法或内部实现；见源码 |
| [parse.go](../../../internal/configschema/parse.go) | 函数/方法或内部实现；见源码 |
| [schema.go](../../../internal/configschema/schema.go) | `Kind`、`Key`、`Schema`、`Validator` |
| [yaml.go](../../../internal/configschema/yaml.go) | 函数/方法或内部实现；见源码 |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Config`、`Test.*Reload`、`Test.*Rollback`、`Test.*Secret`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### configdata

- [auto_hardening_test.go](../../../framework/configdata/auto_hardening_test.go)：`TestAutoTablePromotesEmbeddedFields`、`TestAutoTableRefTargetCheckedEvenWhenColumnIsZero`、`TestAutoTableRefTypeMismatchIsSchemaError`、`TestAutoTableNamedAliasRefIsCompatible`、`TestAutoTableRegistrationRejectsBadRefAndIndexShapes`；其余 6 项见文件
- [auto_tag_guards_promises_test.go](../../../framework/configdata/auto_tag_guards_promises_test.go)：`TestAutoTableRefusesTaggedTiesAndSkipEmptyWithoutIndex`
- [auto_test.go](../../../framework/configdata/auto_test.go)：`TestRegisterAutoTableDerivesMappingFromTags`、`TestRegisterAutoTableRefValidation`、`TestRegisterAutoTableUserValidateRunsAfterRefs`、`TestRegisterAutoTableTagMistakesFailAtRegistration`、`TestInferTableName`；其余 2 项见文件
- [configdata_test.go](../../../framework/configdata/configdata_test.go)：`TestStoreLoadReloadAndActiveSnapshot`、`TestReloadFailureKeepsOldSnapshot`、`TestDryRunBuildsSnapshotWithoutPublishing`、`TestRollbackRestoresPreviousPublishedSnapshot`、`TestReloadListenerRollbackKeepsOldSnapshot`；其余 2 项见文件
- [field_rules_promises_test.go](../../../framework/configdata/field_rules_promises_test.go)：`TestDeclaredRulesRejectReloadAndNameTheViolation`、`TestRuleDeclarationsAreCheckedAtRegistration`、`TestAutoTableTagRulesUseTheSharedCheck`、`TestEveryReloadReportsOneOutcome`
- [key_case_promises_test.go](../../../framework/configdata/key_case_promises_test.go)：`TestMisspelledKeysRejectLoad`、`TestMisspelledKeysRejectReloadAndDryRun`、`TestUndeclaredKeysKeepTheirBehaviour`、`TestRuleFieldMustMatchTheJSONNameExactly`
- [promises_test.go](../../../framework/configdata/promises_test.go)：`TestTableLoadRefusesDuplicateKeys`、`TestDefinitionsRefuseIncompleteShapes`、`TestRegisterAutoTableRefusesEachTagMistakeByMessage`
- [reload_commit_test.go](../../../framework/configdata/reload_commit_test.go)：`TestFailedCommitDoesNotRollBackAnotherStorePublication`、`TestListenerPanicFailsReloadAndKeepsStateConsistent`、`TestBeforeApplyFailureRollsBackOnlyPreparedListeners`、`TestAfterApplyFailureRollsBackAllPreparedInReverse`、`TestFirstLoadFailureSkipsRollbackCallbacks`；其余 5 项见文件
- [review_round2_test.go](../../../framework/configdata/review_round2_test.go)：`TestBeforeApplyPanicRollsBackOnlyPrepared`、`TestRevertRestoresAllGlobalSlots`、`TestFirstLoadFailureLeavesNoTypedNilInRuntimeConfig`、`TestRollbackTwiceIsRejected`、`TestWrapperDocumentsHappyPathAndGuards`；其余 9 项见文件

### configdata/rules

- [key_spelling_promises_test.go](../../../framework/configdata/rules/key_spelling_promises_test.go)：`TestCheckKeysRejectsCaseVariants`、`TestCheckObjectKeysNamesTheSpelling`、`TestLookupIsExact`
- [rules_test.go](../../../framework/configdata/rules/rules_test.go)：`TestCheckNamesTableRowKeyFieldAndRule`、`TestCheckObjectHasNoRowNumber`、`TestDocumentRefusesVanishingAndAmbiguousData`、`TestRuleValidateRejectsBadDeclarations`

### featureflag

- [flags_test.go](../../../infra/base/featureflag/flags_test.go)：`TestStore`、`TestReplaceBumpsVersionWithData`、`TestReplaceVersionIsConsistentUnderConcurrentReads`、`TestSetAdvancesVersionAndSnapshotIsSorted`、`TestNilStoreAndDefaultStoreAreSafe`

### hotcode

- [admin_test.go](../../../framework/hotcode/admin_test.go)：`TestRegisterAdminCommandsTargetsInstanceRegistry`
- [concurrent_patch_promises_test.go](../../../framework/hotcode/concurrent_patch_promises_test.go)：`TestConcurrentReplaceAndRevertLeaveAConsistentPoint`
- [guards_promises_test.go](../../../framework/hotcode/guards_promises_test.go)：`TestHotcodeEntryPointsRefuseMissingRegistryPathsAndPoints`
- [patch_visibility_promises_test.go](../../../framework/hotcode/patch_visibility_promises_test.go)：`TestListReportsAClosurePatchFromTheSameFactoryAsPatched`、`TestResolveConvertsAnIdenticalSignatureAndCountsRealMismatches`、`TestApplyBundleRollsBackWhenApplyPanics`
- [plugin_guards_promises_test.go](../../../framework/hotcode/plugin_guards_promises_test.go)：`TestLoadPluginRefusesEmptyAndNonSharedObjectPaths`
- [plugin_stub_guards_promises_test.go](../../../framework/hotcode/plugin_stub_guards_promises_test.go)：`TestLoadPluginReportsUnsupportedPlatforms`
- [promises_test.go](../../../framework/hotcode/promises_test.go)：`TestRegistryRefusesNamelessNonFunctionAndDuplicatePoints`
- [registry_test.go](../../../framework/hotcode/registry_test.go)：`TestRegistryReplaceResolveAndRevert`、`TestRegistryRejectsSignatureMismatch`

### hotcode/plugintest

- [plugin_load_test.go](../../../framework/hotcode/plugintest/plugin_load_test.go)：`TestMain`、`TestLoadPluginAcceptsABundleExportedAsAnInterfaceVariable`、`TestLoadPluginRollsBackAPartiallyAppliedBundle`
- [race_off_test.go](../../../framework/hotcode/plugintest/race_off_test.go)
- [race_on_test.go](../../../framework/hotcode/plugintest/race_on_test.go)

### internal/configschema

- [remaining_promises_test.go](../../../internal/configschema/remaining_promises_test.go)：`TestNonFiniteAndOverflowingConfigNumbersAreRejected`、`TestUnderscoreClosedChecksBothSchemaForms`、`TestMapSourcePreservesUnderscoreYAMLHierarchy`
- [schema_test.go](../../../internal/configschema/schema_test.go)：`TestDecodeAppliesDefaultsAndReadsValues`、`TestDecodeReportsEveryBadKeyByName`、`TestDecodeRunsValidateConfigOnlyAfterDeclarationsPass`、`TestProductionSecrets`、`TestDataSchemaChecksLikeTheStruct`；其余 4 项见文件

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../performance/STABLE-v1.24.0.md)，没有新测量则不能改容量承诺。
