# equipment：按配置表的槽位穿卸装备，与背包在同一笔事务里扣回

装备在这里是背包里的堆叠道具：穿上时从背包扣一件，卸下时退回一件。哪些道具能穿、穿在哪个槽、每级加多少属性写在配置表里。它不是带独立实例、耐久、随机词条的装备系统。

## 能力

### 客户端协议（`protocol/def/equipment_equip.go`、`equipment_unequip.go`）

- `EquipmentEquip`（请求 / 应答）：`EquipmentEquipRequest{ItemID}`，返回 `Slot`（穿进了哪个槽）与 `Replaced`（被换下的道具号，没有为 0）。客户端穿上的装备等级固定为 1。
- `EquipmentUnequip`（请求 / 应答）：`EquipmentUnequipRequest{Slot}`，返回 `ItemID` 与 `Found`（空槽为 false，不是错误）。

玩家身份取自已认证的会话。

### 服务端入口

- 组件 `EquipmentComp()`（`game/entities/player/equipment_component.go`），只能在持有 Player 锁的 Nest 事务里调，错误必须交还 Nest：
  - `Equip(itemID int64, level int32) (slot int32, replaced int64, err error)`：内部从背包扣一件新的、把旧的退回背包。可信的服务端代码可以传 1～10000 的等级。槽里已经是同一道具、只是等级不同时不动背包，只改等级并执行变化钩子，`replaced` 为 0。
  - `Unequip(slot int32) (itemID int64, found bool, err error)`：内部把道具退回背包后清槽。
  - `Worn(slot) (itemID, level, ok)` 与 `RangeWorn(fn)`：只读。
- Nest handler（`game/handler/equipment_equip.go`、`equipment_unequip.go`）：`handlerEquipmentEquip`、`handlerEquipmentUnequip`，都是 `rollback=undo durability=strict`。端点 `HandleEquipmentEquip` / `HandleEquipmentUnequip` 直接分发到它们。
- 规则函数（`game/equipment/equipment.go`）：`Lookup(itemID) (slot, attack, hp, ok)`、`SlotFor(itemID)`、`ValidSlot(slot)`、`Slots()`，常量 `SlotWeapon`（1）、`SlotArmor`（2）、`MaxSlots`（32）、`MaxLevel`（10000）、`MaxStat`（1e12）。

### 登记点

`player.OnEquipmentChanged(name, EquipmentChangedHook)`（`game/entities/player/equipment_hooks.go`）：每次穿上或卸下成功修改之后执行，签名 `func(component *EquipmentComponent) error`。它在同一笔 Nest 事务、同一把 Player 锁内，只能改同一个 Player、不能做外部 I/O；返回错误让穿戴、背包与全部钩子的写入一起回滚。只在 `init` 登记，按名字顺序执行，空名、nil、重名 panic。

### 状态

`PlayerDao.Equipment`（类型 `EquipmentState`，`db/def/equipment.go`）：`Slots` 是槽号 → `GearPiece{ItemID, Level}`。落库并同步给玩家自己。

### 配置

配置表 `equipment_item`（`configs/schema/equipment_item.go`、`configs/table/equipment_item.csv`）：`ItemID`（引用 bag 的道具表 `item`，唯一）、`Slot`、`Attack`、`HP`（每级的非负加成）。缺省三行引用 bag 自带的 1001 / 1002 / 1003。它是独立的表，不改 bag 的道具 schema。

### 错误码（`internal/errors/equipment.go`）

`equipment_item_invalid`（这个道具没有合法的装备规则）、`equipment_level_invalid`（等级不在 1～10000）、`equipment_slot_invalid`（槽号不在 1～32）。背包的拒绝（没有这件道具、退回时放不下）原样透出 bag 的码。

**日志**：穿戴、卸下确认，记录槽位、道具和替换结果。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/equipment/equipment_equip.go`, `game/controllers/equipment/equipment_unequip.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做物品实例。** 没有独立实例、耐久、随机词条、镶嵌、套装。强化等级只属于“穿着”这个事实：卸下退回的是基础堆叠道具，等级不保留。需要实例装备的工程要扩展 `GearPiece` 的事实模型。
- **不算属性。** `Attack` / `HP` 只是表里的数；把它们加到角色身上是 attribute 模块联动做的。单装 equipment 时这两列没有人读。
- **客户端不能指定等级**，协议固定传 1。没有强化、升级的协议。
- **上限：** 槽号 1～32，每槽一件；等级 1～10000；单项属性每级至多 1e12。
- **一致性与幂等：**
  - 穿、卸、背包扣回、钩子的写入在一笔事务里，任何一步失败或提交被拒整体回滚。
  - 换装先扣新道具、再退旧道具，所以背包种类满时可以复用刚腾出的位置。
  - 同一道具同一等级已经穿着时 `Equip` 幂等成功、不扣背包；空槽 `Unequip` 幂等返回 `found=false`。这只是状态幂等，不是请求账本：延迟到达的旧请求可能再次改变穿戴。
  - 同一槽位上同一道具换一个等级再 `Equip`：穿着的还是这一件，背包不动（不扣、不退，空包也能换），只改等级；变化钩子照常执行，钩子失败时等级保持原值。
- **配置变更：** 从 `equipment_item` 删掉一行之后，这件道具不能再穿，已经穿着的不再提供属性；卸下仍受 bag 的道具表与容量约束，退不回去时整件装备保留。数据迁移由工程负责，模块不自动销毁物品。
- **前提：** 依赖 bag 模块、nest Mod 与 configdata。只操作一个 Player，没有跨实体、跨进程的部分。模块自己不打开玩家接入与自身视图同步（不装 enter 时要自己做，见 KNOWN-LIMITS 的“不装 enter 时安装不打开客户端同步”）。
- **已知限制：** `docs/maintenance/KNOWN-LIMITS.md` 的“业务模块目录”一条：game-demo 的属性 / 装备已换成模块。

## 复用方式

- **需求正好是它：** `roost module add equipment`，连带装上 bag。要让装备加属性再装 attribute。
- **需求比它多一点：**
  - 加装备、改槽位与加成：改 `configs/table/equipment_item.csv`，道具本身要先在 bag 的 `item` 表里。
  - 加槽位名字：在 `game/equipment/equipment.go` 加常量（上限 `MaxSlots`）。
  - 穿卸之后要重算别的东西（战力、外观）：登记 `player.OnEquipmentChanged`，不要改 `Equip` / `Unequip` 的顺序。
  - 做强化：自己写一个 Nest handler 调 `EquipmentComp().Equip(itemID, 新等级)`；对已经穿着的这件只改等级、不动背包。强化的消耗（材料、金币）由这个 handler 在同一笔事务里自己扣。
- **别的业务代码要读 / 改穿戴：** 在自己的 Nest handler 里（目标实体实现 `player.IEquipmentEntity`）调 `target.EquipmentComp().Worn(slot)`、`RangeWorn(fn)`、`Equip(itemID, level)`、`Unequip(slot)`。调用方不要再自己扣、退背包。
- **只适合借鉴的情形：** 每件装备各不相同（随机属性、可交易的唯一实例）时不适合直接用。值得借鉴的是“穿戴事实与背包变化在一笔事务里、先扣后退”的写法。

## 联动

本模块没有 `with/`。别的模块的 `with/equipment`：

- `attribute`：穿卸的同一笔事务里重算属性；加载时按已穿的装备重建非持久的属性层。
- `combat`：真实换装只在下一拍纳入技能世界，换装被拒时保持旧的权威状态。
- `scene`：把 `Equipment` 公开给场景里的其他人，其余私有状态仅本人可见。

## 验证范围

`roost module verify` 跑 18 条场景（`^TestEquipment`），零跳过；与 attribute 的联动另 8 条（记在 attribute 模块下）。场景是离线的：用正式生成的 Nest、Sender、协议端点加内存替身提交器，覆盖穿上扣一件、替换退回旧件、卸下退回、重复穿不扣、空槽幂等、缺道具与非法道具 / 槽 / 等级拒绝、背包满时卸下回滚、提交失败与钩子失败回滚、并发重复只扣一件、协议返回业务码、同一道具换等级不动背包（空包能换、包里另有一件时不被动用）且照常执行变化钩子。真实存储、真实 TCP、跨机恢复没有覆盖。
