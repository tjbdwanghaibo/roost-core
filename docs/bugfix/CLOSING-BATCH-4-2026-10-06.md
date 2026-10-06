# 收尾第 4 批：kit / core 小修与测试设施（A2 / A3 / A7 / A12 / A13 / A14）

- 日期：2026-10-06；基线 `8a292a5a`（分支 `cb4`，提交前 rebase 到 `94548913`）。来源：协调者的全仓盘点（维护者第十一轮“收尾：盘点全部未完成问题，处理完后统一发一个版本”），维护者已授权。**未发版**，随收尾统一发版。
- 范围：nest 测试、`app`、`kit/service/global`、`kit/saga`、`mongo/mongotest`、saga 用例、文档。不碰 codegen 的 roost 部分（cb2）与 skill（cb3）。
- 读代码：codebase-memory 的共享 generation 停在 09-30，本批涉及的文件（`nest/group_lock.go`、`nest/nest_dispatch.go`、`entity/entity_guard.go`、`lock/reentrant_mutex.go` 等）都以当前源码为准直接阅读。

| 项 | 结论 | 记录 |
| --- | --- | --- |
| A2 nest 用例在 `-shuffle` 下失败 | **用例隔离问题**，不是 U-0279 的重排抖动失效；修用例，不登记 RR | 本文 §A2 |
| A3 `glsvet -tests ./nest` 3 条 | 不是有意的写法（正是 A2 的污染源），改用例 | 本文 §A3 |
| A7 global `Bind` 结果未知后重试误报冲突 | 缺陷，P3 | [RR-20261006-05](RR-20261006-05.md) |
| A12 saga 步骤预算大小写冲突静默覆盖 | 缺陷，P3 | [RR-20261006-06](RR-20261006-06.md) |
| A13 `app.run` 的最终错误不进文件日志 | 缺陷，P3 | [RR-20261006-07](RR-20261006-07.md) |
| A14 mongotest `$in` 不支持具名切片（O-S5-6） | 缺陷，P3；saga 用例去掉绕行 | [RR-20261006-08](RR-20261006-08.md) |

## A2 `TestSymmetricCrossCreatePairsResolveWithinRequeueBudget` 在 `-shuffle` 下失败

**复现**（基线，非 race 测试二进制，seed `1791263156350214000`，6 次）：4 次在 60s 包超时挂死（栈停在 `cross_create_requeue_budget_promises_test.go:62` 的 deferred `Shutdown(context.Background())`，一个快 worker 在 `RequireEntity` 等一把永远不会放的实体锁）；另外两次：

```
cross_create_requeue_budget_promises_test.go:106: pair 2: winners=0 losers=2, want one each
cross_create_requeue_budget_promises_test.go:97: pair 1 slot 2 exhausted the requeue budget after 401 attempts: nest: lock timeout: nest: created entity is locked by another holder: entity 10192837 cannot be waited for in lock order
```

每次失败前日志都有 `ERROR entity release hook panic id=<pilot id> err="unlock of unowned mutex"`。

**定位**：

1. 给 `lock.ReentrantMutex.Unlock` 临时加打印（不提交）：`DEBUGUNOWNED id 10142689 owner 911 gid 912` 与 `id 10194885 owner 912 gid 911`——本用例的两个快 worker 互相释放了对方持有的实体锁，栈都在 `runNestLogic → releaseGuardScope → releaseEntities`。两个 goroutine 的 Guard 作用域里是**同一个** `EntityGuard` 对象。
2. 逐个前序用例与目标成对跑（各取一个让前序排在前面的 seed，各 5 次）：只有 `TestLockDispatchEntitiesForHandlerRetriesEpochChangeWhileWaiting` 在前时失败（4/5），其余 16 条 0/5。
3. 该用例在新 goroutine 里直接 `entity.GetEntityGuard()`：没有 Guard 作用域时它从 `guardPool` 取一个独立 Guard。`releaseDispatchLocks`（`nest/nest_dispatch.go:480`）在没有作用域时走 `entity.EntityGuardRelease(guard)`，把 Guard 放回池。用例里实体的锁组在等待期间变了（0 → 9103），`lockDispatchEntitiesForHandlerWithStore`（`nest/group_lock.go:235`）第一次取锁后校验失败、`releaseLocks()`——Guard 第一次进池；重试仍拿同一个 Guard 取锁、成功后用例 `releaseLocks()`——第二次进池。`sync.Pool` 里有同一个指针两份，之后同一进程里两个 `NewGuardScope` 会拿到同一个 Guard。
4. 前一条用例本身通过；被污染的是之后第一个同时用两个以上快 worker 的用例。正常顺序下它排在目标之后，所以只在 `-shuffle` 的某些 seed 下出现；单独跑、和直接前驱成对跑都通过（与协调者的观察一致）。

**结论**：用例隔离问题，不是产品问题。生产里 `lockDispatchEntitiesForHandlerWithStore` / `releaseDispatchLocks` 的调用方只有 `dispatchLoadedEntities` 与 `groupTransitionDispatch`，都在 `runNestLogic` 建的 Guard 作用域里（`nest_dispatch.go:443-448` 的注释也这样核对过），不走无作用域分支；U-0279 的重排抖动与本问题无关。无作用域分支“重试复用已归还的 Guard”这一隐患只在测试里可达，按 roost-bugfix §7 写进 [WANTED W-2026-10-06-01](../bug/WANTED.md)，本批不改产品代码。

**修复**（`nest/group_lock_test.go`）：三条在 goroutine 里取锁的用例改用 `withDispatchGuardScope`——`entity.WithGuardScope` 建作用域、`scope.Guard()` 取锁，与快池派发一致，Guard 由作用域结束时归还一次。

**失败快速报出**（`nest/cross_create_requeue_budget_promises_test.go`）：放行闸门改成 `sync.Once` 包住的 `releaseGates`，后于停机 defer 登记、先执行，断言中途 `t.Fatal` 时停在 `<-gate` 的 handler 也会被放行；`Shutdown` 用 10s 上限的 ctx，超时 `t.Errorf` 后返回，不再挂到包超时。验证：把 `group_lock_test.go` 换回基线、新目标用例照旧被污染，4 次运行分别在 2s / 16s / 17s / 17s 报出（`pair 0 slot 1: ... err=nest: sync timeout` + `shutdown: context deadline exceeded`），不再挂 10 分钟。

**验证**：

- 成对（seed 1，前序在前）：修后 0/10 失败。
- seed `1791263156350214000` 全包：修后 8 次全部通过；换 seed `11`、`22`、`333`、`4444`、`98765` 全包各 1 次通过。
- `GOWORK=off go test -race -count=3 ./nest` 通过；race 构建下 seed `1791263156350214000` 的 `-shuffle` 全包另跑 2 次通过。

## A3 `glsvet -tests ./nest` 报 3 条

`nest/group_lock_test.go:134 / 185 / 227` 的 “GetEntityGuard called inside a go statement” 不是有意的写法：用例要的是“另一个 goroutine 上属于它自己的 Guard”，正确做法是在那个 goroutine 上建作用域，而无作用域的 `GetEntityGuard()` 正是 A2 的污染源。随 A2 改为 `withDispatchGuardScope`，`go run ./cmd/glsvet -tests ./nest` 无输出，不需要豁免标注（glsvet 对 go 语句里的 goroutine 绑定调用也没有豁免机制）。

## 文件

- `nest/group_lock_test.go`、`nest/cross_create_requeue_budget_promises_test.go`（A2 / A3）
- `kit/service/global/service.go`、`kit/service/global/bind_retry_promises_test.go`（A7）
- `kit/saga/step_budgets.go`、`kit/saga/step_budgets_test.go`（A12）
- `app/app.go`、`app/exit_reason_log_promises_test.go`（A13）
- `mongo/mongotest/mongotest.go`、`mongo/mongotest/in_named_slice_promises_test.go`、`saga/coordinator_takeover_review_test.go`、`saga/step_operation_promises_test.go`、`saga/cross_process_real_integration_test.go`（A14）

## 整批验证（`GOWORK=off`）

- `gofmt -l` 为空。
- `go vet` 与 `go test -race -count=3`：`./nest ./app ./kit/service/global ./kit/saga ./mongo/mongotest ./saga` 通过；`go vet -tags integration ./saga` 通过（integration 用例本批未跑真实 Mongo，改动只是去掉不再需要的领取函数参数）。
- `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 与 `go run ./cmd/glsvet -tests ./nest` 无输出。
- 根包 `go test -count=1 .`、`go build ./... && go vet ./...` 通过。

## 未验证项

- A14 的 saga integration 用例（`TestRealMongoCoordinatorLeaseTakeover`）本批未在真实 Mongo 上重跑；它本来就走完整的 `ClaimDue`，改动只是删掉调用参数。
- A2 只验证了上面列出的 seed；`-shuffle` 下 nest 包里是否还有别的跨用例污染，本批没有做全 seed 扫描。
