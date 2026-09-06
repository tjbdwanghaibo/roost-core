# Changelog

本文件从 v1.4.0 起维护；更早版本见 git 历史。

## [Unreleased]

### Changed（测试质量）

- **skillcompose 的合同构建与校验规则逐条钉住**（U-0063，C2）。本地 gap map 显示 `skillcompose` 20 条采样守卫 19 条无覆盖。
  `BuildContract`：无来源、空权威 / 策略 id、负上限（策略侧与调用方侧）、来源缺 id / 摘要、重复来源、空特征、负生命期 /
  目标数；`ValidateContract`：版本、权威、策略、无来源、负预算、来源缺摘要 / 重复、授权指向未知来源 / 空特征 / 重复 /
  无变换 / 空变换 / 重复变换、义务指向未知来源 / 空键 / 重复、包与约束的空键 / 重复、摘要不符——每个变异之后**重算摘要**，
  让被测规则成为唯一能拒绝的规则（否则全部被"摘要不符"掩盖）。`contract_promises_test.go` 两条；回退十处守卫各红。

### Added

- **gap map 工具**（与 roost-core 同一份拷贝）：`scripts/gapmap/revertsample.py`、`scripts/gapmap.sh`、`nightly-gapmap` 工作流。

### Fixed

- **两条运行时承诺补上测试**（收敛单元 U-0028，C2）。对 skill 包的注释承诺做临时回退：`RestoreRuntime` 改走
  `NewRuntime` 的快进+压缩路径（会删掉检查点之后、崩溃之前的事件）后全绿；asset cache 里等待加载的
  `Acquire` 去掉预留引用（首个持有者 Release 会在等待者拿到租约前逐出并卸载）后也全绿。新增
  `promises_test.go`：压缩型 MemoryHost 上检查点后追加事件再恢复，事件必须还在、游标必须回到检查点；
  门控型 loader 制造"加载中 + 第二个 Acquire 等待 + 第一个 Release"，卸载只能发生在最后一个租约释放之后。
  "跨 owner 的能力枚举必须拒绝"一条已有测试变红，属已覆盖。第二、三批再回退四条：cast window 钳位、cooldown 写点记录器、
  `commit_tick ≤ windup_ticks_min` 三条有测试盯住；**"已完成但仍被引用（有 pending 任务或被进程指着）的 cast 不被保留上限逐出"**
  去掉守卫后全绿——补 `TestReferencedCompletedCastsSurviveTheRetentionBound`。第四批再回退四条：检查点版本号、presentation 游标过期、pending 任务先挂起再恢复三条有测试盯住；**检查点校验和不匹配必须拒绝恢复**去掉比对后全绿——补 `TestRestoreRefusesACheckpointWhosePayloadWasTampered`。十一条承诺四处洞。


### Changed

- **go 指令 1.25.0 → 1.27.0**（含 `integration/sync-e2e` 子模块），与 roost-core /
  roost-kit / roost-codegen / roost-service 和 `go.work` 统一。取 1.27.0 而不是最新的
  1.27.1：一个补丁级的 go 指令什么都买不到，还会让停在 1.27.0 的工具链去下载一个新的。

  留在 1.25.0 已经买不到任何兼容性：roost-core 是 go 1.27.0，而任何用到本仓的项目都
  会同时用到 core。

### Added
- `scripts/pretag.sh`：打 tag 之前的发布预检（tag major 与 module 路径后缀一致、
  tag 未存在、无 replace、工作区干净、`GOWORK=off` 下 build/vet/test 通过）。
  由 tag push 触发的 CI 运行在 tag 已存在之后，能报告但阻止不了。

### Changed（破坏性：依赖模块路径与 skillsync 主题名）

- 依赖改为 `github.com/tjbdwanghaibo/roost-core v1.10.0`（`integration/sync-e2e` 同时依赖
  `roost-kit v1.10.0`）；模块路径随 core/kit 改名。
- `skillsync` 主题常量 `cube.skill.manifest/state/presentation` → `roost.skill.*`。发布端与
  应用端在同一模块内始终一致；跨版本滚动升级期间两端会互相听不见，请同批升级。
- JSON schema 标识 `cube.skill/v2` → `roost.skill/v2`，随之所有摘要域字符串（`source-document`、
  `gameplay-program`、`presentation-program`、`visual-manifest`、`gameplay-authority`、`cast-random`、
  `random-site`）一并改名。**不做兼容**：它们是 hash 的输入，旧名下的已存摘要与定义文件不再被接受。
  改名时尚无旧数据，因此不需要迁移；此后若已有定义文件，把 `"schema"` 字段改为 `roost.skill/v2` 并重新编译。
- CI checkout 路径 `cube-skill` → `roost-skill`；文档全部改为 roost 命名。

### Changed
- 核心 Go API 从 `/skillv2` 收敛为唯一稳定包 `/skill`，不保留双包兼容层；JSON schema `cube.skill/v2`、compiler semantics `skillv2-compiler-2` 和现有 checkpoint/wire 格式保持不变。仓库内消费者、示例、CI、codegen 接线和文档全部迁移。
- 依赖升级：`cube-core` → v1.8.0，（e2e）`cube-kit` → v1.8.0（lockstep 两层 + configdata 管线与两轮复审修复）；docs 版本引用同步（CI 门禁"docs versions match go.mod"），全量测试与 sync-e2e 在新版本上通过。skill 运行时的确定性契约（定点/注入随机/无墙钟）正是 core lockstep 客户端模拟的前提，两侧现已同版本对齐。

### Added
- `combatcomponent.StatusBridge`：skill 的 status 域效果命令（Status/RemoveStatus/DispelStatus/AttributeModifier）按 status catalog 标准化落到 combat 容器，事件词表与 MemoryHost 一致；挂 `HostAdapter.Status` 后由 Apply 自动分发。有意差异：mul_bp 修饰加性叠加（非 MemoryHost 乘性链），见 docs/skill-casting-and-combat.md。
- `HostAdapter` 支持 `ResourceCommand`（set/add/spend 语义对齐 MemoryHost：spend 原子校验、no-op 不推进 revision）。
- `combat.ChanceRoll`/`RollValue`：HMAC 确定性掷点（暴击/闪避概率 → 事实），推荐以效果命令 Event 坐标为掷点坐标。
- `combat.BuffContainer` 新增 `BuffIndependent` 叠加策略（同 ID 独立实例独立计时）与 `BuffSpec.MaxDurationTicks`（韧性缩放后的时长上限）；`CombatComponent.RemoveBuff`。
- `examples/`：三个可运行工程（combat 电池、fireball 全链路、statusbridge + 掷点）。
- docs：skill-casting-and-combat.md 补 StatusBridge/掷点章节；AI 作者提示词补 concurrent/GCD/窗口表达式语法。
- docs：新增按角色组织的导航、稳定 API 接入说明和 `/skillv2` → `/skill` 迁移手册；修复失效链接、错误的 module major 发布说明和过期测试基线。
- `StatusBridge` 支持 `ModifyStatusInstanceCommand`（实例句柄级偷取/转移/复制/层数/时长操作，授权矩阵照搬 MemoryHost）；实例寻址契约：opaque id = `combat.BuffInstanceID`。`combat.BuffContainer` 新增 `SetStacks`/`SetDueTick`/`Adopt`；导出 `skill.NewStatusInstanceID`（外部宿主此前无法构造实例句柄）。

### Fixed
- Host event 的消费游标现在只在事件成功分发后推进并 compact；容量拒绝或宿主回调失败会保留失败事件及其后续事件，下一次 tick 可安全重试，不再出现“分发失败但游标已确认”的静默丢事件。
- `combat.AddShield` 返回饱和后的权威实际增量，不再把请求值误报为 `ShieldResult.Added`；Heal/Shield no-op 不推进 revision、不标 `Changed`、不发误导事件。Nest 集成测试同步清理全局 handler，使重复/race 运行稳定。
- `RestoreRuntime` 不再经由新建路径压缩宿主事件队列——旧行为在校验之前就把 checkpoint 之后的事件 compact 掉（即使 restore 被拒绝）；`HostEventCompactor` 文档明确保留契约（必须保留最后一次成功 checkpoint 以来的全部事件）。
- `combat.RestoreBuffContainer` 校验持久化实例（id 唯一、非零、不超过序列号），损坏数据当场报错而不是在远处制造修饰句柄冲突。

## [1.5.0] - 2026-08

破坏性变化：编译器语义修订升级为 `skillv2-compiler-2`（新字段进入 gameplay digest），v1.2.x 的 checkpoint / 回放 / skillcompose 契约需重建；迁移说明见 `docs/skill-casting-and-combat.md`。

- **模块路径改为 `github.com/tjbdwanghaibo/roost-skill`**（与仓库名一致、去掉 `/v2` major 后缀）——自本版起外部可直接 `go get`。wire schema 仍为 `cube.skill/v2`，技能 JSON 不受影响。
- 修复（复审核实的缺陷）：ammo 只读路径不再回写 ability 缓存（曾永久污染增量 baseline 致 Checkpoint 失效）；回充同步 ability 缓存并发出 AbilityUpsert；`Cancel`/`Interrupt` 释放 policy 槽位（toggle 不再被永久封死、cast 不再无界滞留）；combat 管线全部 BP 段夹取非负；`mutationSortKey` 补入 StateHandle（persistent_remove 顺序跨运行确定）；IR 遍历器覆盖 castWindow 表达式与 sustainCosts（属性读不再被 lowering 成句柄 0，并加 panic 防御）。
- 导出 `ValueKind*`/`Quantity*` 常量与 `AuthorityDigest`：外部宿主可以构造 basis_points 攻速属性并驱动 windup 表达式。
- Inspect 补齐：`ProgramView.GlobalCooldownTicks`、`CastWindowView` 的 Concurrent 与表达式边界字段。
- 新文档 `docs/skill-casting-and-combat.md`；CI 增加 `release-hygiene` 门禁。

## [1.4.0] - 2026-08

- 空间度量统一为欧氏（对角追踪弹 41% 超速修复）；免分配定点数学（128 位 mulDivRounded / 牛顿 isqrt，与 big.Int 参考位一致）；随机选择 HMAC 分数预计算。
- 状态突变由全量快照 diff 改为写点记录增量（提交 ~180µs → ~114ns），测试套件带影子校验等价门。
- 施法互斥（`concurrent`/`ErrCasterBusy`）、全局冷却（`global_cooldown_ticks`、`"$gcd"` 哨兵、commit 起算）、windup/recovery 表达式化（编译期 min/max 钳制）。
- 新增 `combat/` 零依赖战斗电池与 `combatcomponent/` cube-core 集成（DirtyTracker、nest 事务逆操作、HostAdapter）；MemoryHost 收敛到同一份战斗数学。cube-core 依赖升至 v1.6.2。
- README 定位声明：2D 权威战斗运行时，不做 Z 轴 / 寻路 / 客户端预测。
