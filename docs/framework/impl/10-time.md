# 时间与定时器：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/10-time.md)

## 如何阅读

测试时你可能想让活动时间快进，但不希望数据库连接也立刻超时。框架因此把业务时间和系统时间分开。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`infra/base/clock`、`infra/base/timer`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 业务时间与系统期限分开注入和使用。
2. 运行业务时间不倒退；恢复受高水位约束。
3. 宿主持久精度决定入队对齐，不隐式改变 Scheduler 纳秒契约。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### clock

1 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [clock.go](../../../infra/base/clock/clock.go) | `Business`、`BusinessFunc`、`Clock` |

### timer

1 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [scheduler.go](../../../infra/base/timer/scheduler.go) | `ChangeType`、`Node`、`Context`、`Handler`、`ChangeFunc`、`Scheduler` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Clock`、`Test.*Time`、`Test.*Scheduler`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### clock

- [clock_test.go](../../../infra/base/clock/clock_test.go)：`TestLogicClockOffsetRoundTripsAtDurationResolution`、`TestLogicClockSetLandsWithinOneMillisecond`、`TestInjectedClocksAreIndependentOfTheGlobalOne`、`TestLogicClockIsSafeForConcurrentUse`
- [remaining_promises_test.go](../../../infra/base/clock/remaining_promises_test.go)：`TestProcessClockKeepsConfiguredSubmillisecondOffset`

### timer

- [order_and_unhandled_promises_test.go](../../../infra/base/timer/order_and_unhandled_promises_test.go)：`TestTimersWithTheSameDeadlineFireInRegistrationOrder`、`TestPriorityOrdersTimersWithTheSameDeadline`、`TestPriorityIsKeptThroughStorageAndRescheduling`、`TestADueTimerWithoutAHandlerIsDroppedWithAWarningAndACount`、`TestStoredTypesWithoutAHandlerAreReportedOncePerType`
- [remaining_promises_test.go](../../../infra/base/timer/remaining_promises_test.go)：`TestInvalidSavedNodesEmitPersistenceDeletion`
- [scheduler_promises_test.go](../../../infra/base/timer/scheduler_promises_test.go)：`TestRemoveFromAHandlerStopsATimerDueInTheSameTick`、`TestChangeFromAHandlerPostponesATimerDueInTheSameTick`、`TestDeferredOperationsDuringTickKeepTheirMeaning`、`TestChangesSeenByTheHookDuringATickMatchTheFinalState`、`TestReentrantTickKeepsTheOuterTickSemantics`；其余 1 项见文件
- [scheduler_test.go](../../../infra/base/timer/scheduler_test.go)：`TestSchedulerFiresAndPersistsChanges`、`TestSchedulerReschedulesFromHandler`、`TestSchedulerSetClockStampsNewTimers`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../performance/STABLE-v1.24.0.md)，没有新测量则不能改容量承诺。
