# M-09：manager 生命周期引擎下沉 roost-core（ARCH-02）

**来源**：`docs/bug/REVIEW-2026-09-16-04.md` §7 ARCH-02。
**类型**：架构迁移，不占 U 编号；不改行为、不改错误文本里可被断言的片段（"aborted by shutdown" / "rollback" / "registry is nil" /
"Start before Provide" / "dependency cycle at" / "depends on missing manager" / "duplicate manager"）、不改指标名（`manager.start.duration`、`manager.started`）。

## 拆分

| 半 | 内容 | 位置 |
| --- | --- | --- |
| 引擎（本批已迁） | `Order(managers)`（按 `DependsOn` 的稳定拓扑序：无依赖关系者保持注册序、环与缺失依赖按名报错、拒绝 nil / 空名 / 重名）；`Engine`：`NewEngine` / `Register` / `MustRegister` / `Managers` / `Manager` / `Provide(registry)` / `Start` / `Stop(ctx)`。Start 失败只回滚已成功者（失败的那个不 Stop）、回滚失败与启动失败一起 `errors.Join`、Start 途中收到关停即中止余下管理器、Stop 优先 `IManagerStopperWithContext` 并逐个报错、幂等；`ErrRegisterAfterStart` | `roost-core/manager` |
| Mod（留 kit） | `ManagerMod`：`Name()` 返回 `mods.ModManager`、`Init`、`Provide` 在注册表登记 `mods.ModManager` 并把注册表交给引擎、`Stop()` 记日志、`StopWithContext` | `roost-kit/manager` |

**为什么不用 core 已有的东西**：`container.TopologicalSortCache` 从 map 出队（独立管理器每进程顺序不同）且遇环只记日志返回 nil；
`lifecycle.ManagerGroup` 是另一套 `lifecycle.Manager` 契约（Init / Start / Stop 三段、panic 兜底），与 `app.IManager` 的 Start(registry) 形状不同。
审查也明确要求保留现有语义原样迁移。

## 本批改动（roost-core）

- 新增 `manager/order.go`（原 kit `manager_order.go`，`sortManagers` 导出为 `Order`）、`manager/engine.go`（原 kit `manager_mod.go` 去掉 `Name` / `Init` /
  `mods.ModManager` 登记；`StopWithContext(ctx)` 改名 `Stop(ctx)`，`Provide` 只绑注册表不登记 capability；错误前缀 "manager mod:" → "manager:"）。
- 随迁测试：`engine_test.go`（原 `manager_mod_test.go`，13 条）、`abort_promises_test.go`（U-0152）、`guards_promises_test.go`（U-0150）、`order_test.go`（7 条），
  机械改名（`NewManagerMod` → `NewEngine`、`StopWithContext(ctx)` → `Stop(ctx)`、`sortManagers` → `Order`）。
- `app/manager.go` 的注释从 "managed by ManagerMod" 改为指向引擎。
- `dependency_boundary_test` 通过；`manager`、`app` `-race` 绿。

## kit 半（已于 2026-09-16 随 core v1.15.4 发版后完成，kit v1.14.5；原计划如下，均已按此做）

1. `roost-kit/manager/manager_mod.go` 改成包装：`type ManagerMod struct{ engine *coremanager.Engine }`；`var ErrManagerRegisterAfterStart = coremanager.ErrRegisterAfterStart`
   （同一指针，`errors.Is` 不变）；`Register` / `MustRegister` / `Managers` / `Manager` / `Start` 转发；`Provide(r)`：nil 检查 → `engine.Provide(r)` → `r.Register(mods.ModManager, m)`；
   `Stop()` → `engine.Stop(fctx.BaseContext())` 记日志；`StopWithContext(ctx)` → `engine.Stop(ctx)`。删掉 `manager_order.go`。
2. kit 测试：删 `manager_order_test.go`、`abort_promises_test.go`、`guards_promises_test.go` 里的引擎断言（已在 core）；`manager_mod_test.go` 缩成 Mod 形状：
   `Name()==mods.ModManager`、`Provide` 登记 capability、`Start` / `StopWithContext` 转发一条、`Register` 后置拒绝走同一个 sentinel。
3. codegen 不变：生成物仍是 `kitmanager.NewManagerMod(service<X>.Managers()...)`（`catalog.go:66`、`render.go:408-410`）。
4. 验收：kit 全套；codegen `-template game-demo` 生成工程编译。

## 不做

- 不改 `ManagerMod` 的公开方法集与 codegen 生成形状。
- 不把 `mods.ModManager` 之类的 Mod 名常量搬进 core。
