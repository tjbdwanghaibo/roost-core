# 施法语义与战斗内容电池（v1.4+）

本文档覆盖 v1.4 引入、v1.5 定稿的三组能力：施法互斥与全局冷却、施法窗口表达式化、`combat`/`combatcomponent` 战斗内容电池，以及随之而来的编译器语义修订迁移说明。

## 迁移说明（必读）

- **编译器语义修订升级为 `skillv2-compiler-2`。** 新增的 `concurrent`、`global_cooldown_ticks`、窗口表达式字段进入 gameplay digest，同一定义在新旧版本编译出的 digest 不同。v1.2.x 产生的 checkpoint、回放记录与 skillcompose 契约在新版本下**无法解析**（会得到明确错误而非静默失败）：升级时需要全量重编译技能定义、丢弃旧 checkpoint（或先在旧版本完成排空）并重签契约。
- **Go 模块路径已改为 `github.com/tjbdwanghaibo/roost-skill`**（与仓库名一致，不再使用 `/v2` major 路径）。自 `v1.5.0` tag 起可直接 `go get`；wire schema 仍是 `roost.skill/v2`，技能定义 JSON 不受影响。

## 施法互斥与全局冷却

- **施法互斥**：默认情况下，同一 caster 在已有施法窗口（windup / commit / recovery 阶段）内发起新的主动施法会得到 `ErrCasterBusy`。技能可在激活声明上用 `"concurrent": true` 退出互斥。proc / 被动触发的施法不受互斥与 GCD 限制。
- **全局冷却**：定义顶层的 `"global_cooldown_ticks": N`。**从 commit tick 起算**（不是 Activate 时刻）：施法提交时把 caster 置入 N tick 的全局冷却，期间任何技能的主动施法返回 `ErrGlobalCooldownActive`。多次提交取最晚到期时间。
- 全局冷却以保留程序 id `"$gcd"` 作为一条普通冷却条目存在：`StateSnapshot().Cooldowns`、增量 mutation 与 checkpoint 都能直接看到它，客户端按普通冷却渲染即可。

```json
{
  "cooldown_ticks": 40,
  "global_cooldown_ticks": 8,
  "activation": {"type": "active", "policy": {"mode": "tap"}, "concurrent": false}
}
```

## 施法窗口表达式（攻速缩放施法时间）

`cast_window` 的 windup 与 recovery 可以由表达式在运行时求值：

```json
{
  "cast_window": {
    "windup_ticks_expression": {"op": "scale_bp", "args": [10,
      {"read_attribute": {"entity": "$caster", "attribute": "attack_haste_bp", "snapshot": "current"}}]},
    "windup_ticks_min": 2,
    "windup_ticks_max": 10,
    "commit_tick": 2,
    "recovery_ticks": 3
  }
}
```

规则：

- 表达式与字面量互斥（同时给出 `windup_ticks` 与表达式是编译错误）；表达式必须声明 `*_min`/`*_max` 边界，结果在运行时**钳制**进边界——编译期的最坏情形（预算证明、commit 先于 execute 的不变量 `commit_tick <= windup_ticks_min`）因此始终成立。
- windup 表达式在窗口准备时采样一次；recovery 表达式在恢复阶段开始时采样一次（能看到执行后的状态）。
- 表达式量纲必须是 ticks；`scale_bp` 的第二个参数必须是 basis_points 量纲。

### 宿主如何提供攻速属性

属性目录条目的类型与量纲通过导出常量书写，扩展环境后需要重封 digest：

```go
environment := skill.DefaultCompileEnvironment()
environment.Gameplay.Attributes.Entries = append(environment.Gameplay.Attributes.Entries,
    skill.AttributeCatalogEntry{
        Handle: 40, Key: "attack_haste_bp",
        ValueType: skill.ValueKindInt, Quantity: skill.QuantityBasisPoints,
        Readable: true, Snapshots: []string{"current"}, ModifierOperations: []string{"add"},
        Minimum: 0, Maximum: 20000, Rounding: "toward_zero",
    })
environment.Digest = skill.AuthorityDigest(environment) // 重封，否则 ENVIRONMENT_INVALID
```

Host 的 `Read` 返回值用 `skill.AttributeRuntimeValue(catalog, handle, value)` / `skill.ResourceRuntimeValue(value)` 构造，量纲自动取自目录。

## combat：零依赖战斗内容电池

`combat` 包是可复用的确定性战斗数学，零外部依赖：

- `AttributeSet`：base + 修饰器（flat 求和 + rateBP 加性求和），`Grant`/`Revoke` 完全可逆、结果与授予顺序无关。
- `BuffContainer`：叠层（refresh / extend / ignore / independent 策略，independent 每次应用都是独立计时的新实例）、驱散标签、免疫标签、韧性减时（`SetTenacityBP`）、`MaxDurationTicks` 时长上限（在韧性缩放之后钳制），可 `LinkAttributes` 让 buff 修饰器自动物化为属性授予。
- `ResolveDamage`：twelve_stage_v1 十二段伤害管线（命中回避 → 抗性穿透 → 元素/增伤/减伤 BP → 暴击 → 上下限 → 护盾吸收 → 死亡防护钩子 → 吸血）。随机性外置：闪避/暴击等以预掷事实传入。所有 BP 段夹取非负。
- skill 的 `MemoryHost` 直接运行在这份代码上：参考实现与生产实现共享同一份数学。

**buff 与 skill status 的分工**：skill 的 status 是技能程序可见的世界目录语义（选择器过滤、combat hook 的载体），由 Host 拥有；`combat.BuffContainer` 是宿主实体侧的属性/时效容器。典型宿主用 status 承载技能系统语义，用 BuffContainer 承载数值聚合，两者在 Host 的 `Apply(StatusCommand)` 实现里桥接。

**AttributeSet → Combatant 投影**：伤害管线读取的是 `Combatant` 平铺字段。宿主用 `Observe` 回调把属性变化投影到 Combatant：

```go
attributes.Observe(func(id combat.AttributeID) {
    switch id {
    case attrArmor:
        combatant.Armor = attributes.Current(attrArmor)
    case attrDamageTakenBP:
        combatant.DamageTakenBP = attributes.Current(attrDamageTakenBP)
    }
})
```

## StatusBridge：status 命令落到 combat 容器

`combatcomponent.StatusBridge` 把 skill 的 status 域效果命令（`StatusCommand` / `RemoveStatusCommand` / `DispelStatusCommand` / `AttributeModifierCommand`）标准化地落到 combat 容器上，消灭"status 与 buff 双体系各自实现"的第三套状态系统：

- catalog 的 `StatusCatalogEntry` 驱动映射：`MaxStacks`/`RefreshPolicy`（refresh/extend/ignore/replace）/`DispelCategory`（作为驱散 Tag）/`TenacityPolicy: scale_duration`/`MaximumDurationTicks`/`AttributeModifiers` 全部翻译为 `combat.BuffSpec`；
- 免疫标签是世界事实，经 `HasGameplayTag` 回调判定（nil 则关闭该检查）；
- `AttributeModifierCommand` 以保留 BuffID（`AttributeModifierBuffID`）+ `BuffIndependent` 策略应用——每条命令独立计时，与 MemoryHost 的逐条 modifier 实例语义一致；
- 事件词表与 MemoryHost 相同（`status_applied` / `status_immune` / `status_removed` / `status_dispelled` / `attribute_modifier_applied`），proc 过滤器两边行为一致；
- 挂进 `HostAdapter.Status` 字段后，`HostAdapter.Apply` 自动把 status 域命令转给桥（同时 `ResourceCommand` 也由 adapter 落到映射属性上，语义与 MemoryHost 相同：spend 原子校验、no-op 不推进 revision）。
- **实例句柄操作**（`ModifyStatusInstanceCommand`：偷取/转移/复制、层数与时长编辑）同样由桥处理，授权矩阵与操作门控照搬 MemoryHost（SourceOwnership、Dispellable、Copyable/Transferable/Stealable、DurationOperations 白名单、MaximumDurationTicks 钳制）。**实例寻址契约：`StatusInstanceRef` 的 opaque id 就是 `combat.BuffInstanceID`**——宿主在 Select 返回 status 实例时用 `skill.NewStatusInstanceID(uint64(instance.Instance))` 发放句柄。护盾类 status 的盾值搬移不在桥内（护盾池在 `Combatant.Shield`，由宿主的 damage/shield 面管理）。

**一处有意的语义差异**：`mul_bp` 修饰在桥/AttributeSet 中按 **基点增量加性叠加**（两个 ×1.2 = +40%，顺序无关、事务回滚可精确逆转），而 MemoryHost 是乘性链（= +44%）。一个游戏只选一种宿主语义并保持一致。

## 确定性掷点（暴击/闪避概率 → 事实）

伤害管线只接受预掷事实（`Dodge`/`ForceCritical`…）。`combat.ChanceRoll` / `combat.RollValue` 是产生这些事实的标准方式：

```go
// 同一 key + purpose + 坐标永远得到同一结果——副本与回放位一致。
crit := combat.ChanceRoll(matchSeed, "crit", critChanceBP,
    uint64(event.RootEventID), uint64(event.EventID), uint64(event.EffectIndex), uint64(target))
```

推荐坐标取效果命令 Event 上的 `RootEventID`/`EventID`/`EffectIndex` 加目标实体——每个伤害实例独立掷点且可复现；不同 purpose（"crit"/"dodge"）在同一坐标下相互独立。

## combatcomponent：roost-core 集成

`combatcomponent` 把 combat 电池接入 roost-core 实体模型：

- `CombatDao`：持有战斗状态，实现 `entity.DaoInterface` + `dataengine.Tracker` 契约 + `entity.PersistedDaoLoader`（BSON + schema 版本）与 nest 状态回滚接口。
- `CombatComponent`：全部 mutator 在 nest 事务内记录 `nest.RecordUndo` 逆操作并按字段掩码（vitals / attributes / buffs）标脏——handler 失败回滚后实体字节一致。
- `HostAdapter`：实现 `skill.Host` 的战斗面（damage/heal/shield 命令、attribute/resource 读取、原子 PayCosts），事件词表与 MemoryHost 一致（`damage_resolved`、`combat_hook_*`、`shield_absorbed`…），proc 过滤器在两种宿主上行为相同。`Select`/`StepProcess`/空间查询/生成物仍由业务 Host 实现。
