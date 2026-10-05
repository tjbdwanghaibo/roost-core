# N09 skill 第一批：子包清单、执行/状态与事务/结算

2026-10-05，基线 `be7bcc1853020d9f744360d78831abd0ddf990a3`（origin/main），独立 worktree 分支 `revn09`，NC 编号段 110～119（本批用 110～113）。[接力清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md) · [跨轮进度](PROGRESS.md)。

图谱：项目 `Users-whb-roost-roost-core`，generation 2026-09-30T11:28:30Z。`skill/` 自 09-27 起没有代码提交（最后一笔 `001b03b1` 只是 gofmt），本批引用的 skill 文件 coverage 均为 `no_recorded_issue / metadata_match`；`nest/rollback.go`、`nest/execution.go` 为 `metadata_changed`，按当前源码读取。调用方向用 `trace_path`（`releaseCast`、`markAbilityCastFinished` 的全部调用方）核对，再用 `rg` 补 `CastFailed` 的全部写点。

## 1. 子包清单（179 个 Go 候选，按四个方向）

10-03 计划写的“skill（含 cmd），182”：当前候选 CSV 里 N09 只有 `skill/` 下 179 个文件，仓库没有 skill 专属命令；`cmd/glsvet` 已归 N15。正式调用方指仓库内生产代码 / 生成工程，测试、examples、`skill/integration` 不算。

| 方向 | 范围（文件数） | 职责 | 正式调用方 | 风险 |
| --- | --- | --- | --- | --- |
| 执行/状态 | 根包 `runtime*.go`（24）、`host*.go`（11）、`memory_host*.go`（10）、`process*.go`（5）、`scheduler.go`、`executor.go`、`replay.go`、`trace.go`、`inspect.go`（共 55） | 确定性 Runtime：施法生命周期（准备/提交/执行/恢复/取消/打断/释放）、排程、proc/被动、进程与 owned 实体、checkpoint/回放、Host 世界边界与参考宿主 | 仓内无：game-demo 明确不接执行（`skill_catalog.go.tmpl` 注释）；只有 examples、测试与 `skill/integration/sync-e2e` | **高**（公开库 API，状态机分支多；本批发现 3 条） |
| 数据/属性 | 根包 `compile*.go`（28）、`program*.go`（18）、`ir*.go`（16）、`wire*.go`（13）、`parse*.go`（2）、`value*.go`（2）、`lower.go`、`quantity.go`、`fixed_math.go`、`canonical_definition.go`、`environment_kinds.go`、`diagnostic.go`、`generated.go`、`doc.go`；`combat/`（5）；`skillcompose/`（15）（共 107） | 严格 JSON 解析 → 编译器静态证明 → 不可变 Program 与摘要；定点数学；`combat` 属性/buff/伤害/掷点电池；`skillcompose` 组合契约 | game-demo `Init` 与 `HandleSkillCatalog` 调 `skills.CompileAll`（只用 Parse + Compile）；`roost add skill` 生成定义与 catalog；`combat` 仅经 combatcomponent / MemoryHost | 中（编译期有 fuzz 与大量 promises；生成工程启动链依赖它） |
| 事务/结算 | `combatcomponent/`（3） | `CombatDao`（DataEngine Tracker、BSON 持久化、state 回滚快照）、`CombatComponent`（Nest 逆操作 + 字段脏位）、`HostAdapter`（伤害/治疗/护盾/资源/PayCosts、事务外经 `RunDetachedTransaction`）、`StatusBridge` | 仓内无（文档写“由生成的实体工厂装配”，codegen 没有这条装配） | **高**（唯一接 Nest / DataEngine 的 skill 包；NC-61 同形风险点） |
| 同步/接入 | `skillsync/`（10）、根包 `presentation*.go`（4）；另含根包 `runtime_sync.go` / `runtime_mutation.go`（计在执行/状态） | manifest/state/presentation 三类记录、Coordinator 可见性过滤与 durable outbox（文件 outbox）、客户端 Applier；表现计划 | 仓内无；`skill/integration/sync-e2e` 是独立模块的端到端 | 中高（持久 outbox 与可见性；下一批） |

## 2. 本批审过的场景（执行/状态 + 事务/结算）

| 编号 | 场景 | 读过的源码 / 跑过的 | 结论 |
| --- | --- | --- | --- |
| E1 | 技能开始：容量、语义修订、权限、toggle 二次激活、busy / GCD / 冷却门、ID 与随机键、prepare 失败收尾 | `runtime.go:336-446`、`runtime_cast_window.go:10-125`、`runtime_ability.go:198-230`、`runtime_retention.go:20-60` | **NC-110**：失败启动删 cast 并复用 ID，残留排程任务 |
| E2 | 打断 / 取消 / 释放 | `runtime_cast_window.go:233-360`、`runtime_cast_policy.go:5-87` | **NC-111**：中途出错停在半终止；**NC-112**：失败 cast 仍被当作活的 |
| E3 | 结束：恢复期、完成、排程任务失败、回收 | `runtime_cast_window.go:127-224`、`scheduler.go:229-485`、`executor.go` 全文 | 正常完成路径无问题；失败路径见 NC-112 |
| E4 | 重复触发：toggle 二次激活、ammo 库存与回充代际、charge 再次激活、proc 递归深度 / 每根事件上限 | `runtime_cast_window.go:390-438`、`runtime_proc.go:42-118`、`runtime_dispatch.go:22-61` | ammo 回充按 generation 去重、proc 经系统任务入队（不在 drain 中同步开 cast，排除了 drain 期间 ID 冲突）；toggle 见 NC-112 |
| S1 | 组合效果：StatusBridge 叠层 / replace / 免疫 / 实例转移与复制 / 属性修饰 | `combatcomponent/status_bridge.go` 全文、`combat/buffs.go` 全文 | 无确认缺陷；mul_bp 加性叠加是文档声明的差异 |
| S2 | 伤害：十二段管线、钩子消费、护盾、吸血 | `combat/damage.go` 全文、`adapter.go:169-222` | 无确认缺陷 |
| S3 | 资源：PayCosts 原子校验、ResourceCommand set/add/spend | `adapter.go:118-157, 293-345`、`runtime_turn.go` | 无确认缺陷（既有 `TestHostAdapterPayCostsIsAtomic`、resource promises） |
| T1 | Nest 快池提交与回滚（NC-61 同形核对） | `component.go` 全文、`nest/rollback.go:160-215, 370-405, 903-1015`；探针 4 组合 | combat 状态**会**随回滚恢复：RollbackUndo handler 失败 / 提交被拒、RollbackState handler 失败 / 提交被拒，DAO 字节、含 buff 授予的属性当前值、tracker 版本都恢复（undo 逆操作与 state 快照两条路径分别覆盖）。另发现 **NC-113**：副本共享 map |
| T2 | DataEngine 持久化：Put / Patch / Delete、版本、schema 迁移、id 校验 | `component.go:76-171`；探针（含 `ElementMultipliersBP` 的 BSON 往返与 Patch） | 无确认缺陷 |
| T3 | Sync 与生成代码接线 | `demo/internal/service/game/service.go.tmpl`、`demo/game/controllers/player/skill_catalog.go.tmpl`、`codegen/internal/roost/add_skill.go` | 生成工程只编译 catalog，执行、combatcomponent、skillsync 都没有正式接线（设计如此，见观察 O3）；skillsync 本身留下一批 |

探针 `zz_probe_n09_test.go`（skill、combatcomponent 两包）跑完已删除，结论落为正式回归。

## 3. 确认缺陷

| 编号 | 等级 | 一句话 |
| --- | --- | --- |
| [NC-110](../bug/RR-20261005-NC-110.md) | P2 | 启动失败的 cast 被删除、ID 复用，却留下排程任务：旧任务在下一个 cast 上执行，checkpoint 恢复判 corrupt |
| [NC-111](../bug/RR-20261005-NC-111.md) | P2 | Cancel / Interrupt / Release 中途出错，cast 停在半终止状态，施法者永久 `ErrCasterBusy` |
| [NC-112](../bug/RR-20261005-NC-112.md) | P2 | 失败的 policy cast 不释放槽位，下次激活变成对失败 cast 的 toggle-off |
| [NC-113](../bug/RR-20261005-NC-113.md) | P3 | `Combatant()` / `InitCombatant` 共享 `ElementMultipliersBP`，事务外改权威状态 |

NC-110～112 是同一类：施法的终止路径各自手写收尾步骤（撤任务、停进程、释放 policy 槽位、记 finished、拒绝再操作），每条路径漏的步骤不同。

## 4. 观察与设计建议（不登记 RR）

- **O1 Runtime 状态不在 Nest 事务里（NC-61 同形，但没有被违反的承诺）**。`HostAdapter` 的注释写 Host 方法“在 nest handler 内、Runtime 锁下运行”。在 handler 里驱动 `Runtime.Start / Advance` 时，combat DAO（法力、血量、buff）随 handler 失败或提交被拒回滚，但 Runtime 自己的冷却、ammo 库存、cast 状态、proc 账本、state mutation 流，以及业务提供的 `RevisionSource.CommitEffect` 推进的 revision 和事件都不回滚——回滚后玩家法力还在，技能却已进入冷却。仓内没有正式调用方，文档也没承诺 Runtime 参与事务，所以记为契约缺口：要么在 `docs/skill` 写明“Runtime 只能在提交确认后推进 / 失败时用 checkpoint 恢复”，要么提供 Runtime 的回滚参与（每笔事务 checkpoint 成本高，需要先定接入形态）。
- **O2 属性修饰到不了伤害管线**。`ResolveDamage` 读 `Combatant` 的平铺字段（Armor 等），`combat` 文档要求宿主用 `AttributeSet.Observe` 把属性投影进 Combatant；但 `CombatDao.attributes` 不导出，`CombatComponent` 也没有安装投影的入口，且 `applyState`（持久恢复 / state 回滚）会换掉 AttributeSet 实例。经 StatusBridge 施加的 Armor 等修饰只对 `HostAdapter.Read` 可见，不影响伤害。需要维护者决定投影映射放在组件还是业务。
- **O3 生成工程不接执行**。game-demo 只在 `Init` 与 `HandleSkillCatalog` 编译 catalog，执行、combatcomponent、skillsync 的 Nest / DataEngine / Sync 正式链路都不存在；本批的事务 / 持久化结论来自包内真实 `nest.Engine` 与 DAO，不是生成工程三进程链路。
- **O4 事务外的 HostAdapter 每条命令一笔 strict 事务**。`Runtime.Advance` 在 handler 外被调用时，PayCosts 与每个效果各自经 `RunDetachedTransaction(context.Background(), …)` 提交，彼此不原子、不可取消；注释写明是“lower-isolation”，记录供接入者参考。

## 5. 未审 / 未验证

- 执行/状态：`runtime_checkpoint.go`（只用作探针的恢复判据）、`runtime_mutation.go`、`runtime_sync.go`、`runtime_effect_result.go`、`runtime_select.go`、`runtime_input.go`、`runtime_eval.go`、`runtime_owned_process.go`、`process*.go`、`memory_host*.go`、`replay.go`、`trace.go`、`inspect.go` 未逐行审。
- 数据/属性：`compile*` / `program*` / `ir*` / `wire*` / `parse*` / `lower` 与 `skillcompose/` 全部未审；`combat/roll.go`、`combat/combat.go` 未读。
- 同步/接入：`skillsync/` 与 `presentation*.go` 全部未审。
- 没有在 game-demo 生成工程或真实 WAL / Mongo 上跑 combatcomponent（O3：没有正式接线）。

## 6. 停点与下一批入口

已审：执行/状态的施法生命周期主干（E1～E4）、事务/结算全部（`combatcomponent/` 3 文件 + `combat/` 的 attributes / buffs / damage）。下一批（N09 第二批）建议：

1. 同步/接入：`skillsync` Coordinator → durable outbox（`file_outbox.go` 的原子替换、Windows / Unix 两份）→ Applier 事务（`applier.go` 的 Commit / Rollback 幂等）→ 可见性过滤；`runtime_sync.go` / `runtime_mutation.go` 的增量 state mutation 与 baseline。
2. 执行/状态余项：checkpoint / 回放与 owned 进程（`runtime_checkpoint.go`、`runtime_owned_process.go`、`process_owned.go`），重点核对 NC-110 修复后终止路径与 checkpoint 的一致性。
3. 数据/属性：编译器与 skillcompose 放第三批，先看 game-demo 启动链实际用到的 Parse + Compile 拒绝路径。

## 7. 方向判断

施法终止路径反复出缺陷：v1.5.0 修过“Cancel / Interrupt 不释放 policy 槽位”（`releasePolicySlot`），本批又在失败路径上打破同一个不变量（NC-112），并在启动失败（NC-110）与 Cancel / Release 中途出错（NC-111）上各漏了不同的收尾步骤。根因不是某一行，而是 8 个终止点（`startLocked`、`Cancel`、`Interrupt`、`releaseCast` 的 charge 取消、`executeAutoRelease`、`failScheduledCast`、`executeScheduledTask` 通用错误分支、`completeCastRecovery`）各自手写“撤任务 / 停进程 / 释放槽位 / 记 finished”。建议收敛为唯一的失败终态入口，并让对外 API 先判断终态再动手；修复按这个方向做（见修复记录），不在各分支继续补步骤。成本：失败路径统一撤销本 cast 的全部排程任务（按 cast ID 而不是当前 phase token），之前留到触发时才丢弃的失效任务现在立即清理，checkpoint 内容随之变少，不改 wire 格式。
