# attribute：玩家属性（基础 / 装备 / 最终三层，随档案等级成长）

## 能力

给 `Player` 提供一组战斗属性：生命 `HP`、攻击 `Attack` 与由公式推导的战力 `Power`。属性分三层：基础层（随等级成长，落库）、装备层（由别的模块提供来源，加载时重算）、最终层（基础加装备，再推导 `Power`）。

- **客户端协议**
  - `AttributeGet`（`protocol/def/attribute_get.go`）：读自己的最终属性。请求没有字段；应答带 `HP`、`Attack`、`Power`。端点是 `game/controllers/attribute/attribute_get.go` 的 `HandleAttributeGet`，目标玩家取自认证会话。
  - 属性变化也随同步下发（见“状态”）。
- **服务端入口**（`game/entities/player/attribute_component.go`，都要求调用方已持有这名 `Player` 的锁）
  - `AttributeComp().Final() (*gameattr.Combat, bool)`：最终属性的副本；最终层为空时第二个返回值为 `false`。副本可以在释放锁之后继续用于计算。
  - `AttributeComp().Container() *coreattr.Container`：基础、装备（选择器 `Gear`）、最终三层的副本；对副本的修改不影响实体。
  - `AttributeComp().Refresh() error`：在 Nest 事务里重算全部三层；玩家还没有持久基础记录时，这一次把缺省基础值写进持久层。`RefreshGear()` 是它的别名，供装备变化时调用。
  - `AttributeComp().LevelUp(levels int32) error`：基础层一次性成长 `levels` 级并重算。由档案升级钩子调用，一般不直接调。
  - `IAttributeEntity`：窄实体接口。
  - Nest handler `handlerAttributeGet`（`game/handler/attribute_get.go`）：协议背后的只读 handler。
- **属性定义**（`game/gameplay/attribute/combat.go`，归工程所有）
  - `Combat` 结构带 `//roost:attribute` 标注，属性号、Setter 与公式更新代码由框架的 attribute 生成器产出。
  - `_Power` 是推导公式：`Power = Attack*2 + HP/10`。`Power` 不能直接写。
  - `ForLevel(level)` 是缺省基础值：`HP = 100 + level*10`，`Attack = 10 + level*2`。
  - `(*Combat).LevelUp(levels)`：每级 `HP +10`、`Attack +2`。
- **登记点 / 钩子**
  - `OnAttributeGear(name string, hook AttributeGearHook)`（`game/entities/player/attribute_gear.go`）：登记一个装备层来源。钩子签名 `func(*Player) (*gameattr.Combat, error)`，在 `Refresh` 里、持有 `Player` 锁时调用，只能读同一个 `Player`。多个来源按名字排序后把 `HP`、`Attack` 相加。返回错误会让这次刷新以及所在事务失败。只在 `init` 登记；重名、空名、`nil` 直接 panic。
  - 本模块自己接在 `profile` 模块的登记点上：`OnProfileLevelUp("attribute", …)`，升级时调 `LevelUp(to - from)`，与经验、等级在同一笔事务里。
- **状态**（都在 `PlayerDao` 上，类型 `AttributeValues`，`db/def/attribute.go`，内容是“属性号 → 值”的 map，值为 0 的属性不存）
  - `AttributeBase`：`persist,sync`，基础属性，落库并下发给本人。没有记录时按档案等级用 `ForLevel` 派生。
  - `AttributeGear`：`nopersist,nosync`，装备层，不落库也不下发，加载时重算。
  - `AttributeFinal`：`nopersist,sync`，最终属性，不落库，加载时重算并下发。
- **配置**：没有配置表。成长数值与公式写在 `game/gameplay/attribute/combat.go`。
- **错误码**（`internal/errors/attribute.go`）
  - `attribute_overflow`：属性为负，或超过支持的范围（单项上限是 `math.MaxInt64/4`）。

## 能力边界

- **不做的事**：没有 buff 与临时加成层、没有百分比加成、没有随机词条、没有战斗中的当前血量（那在 `combat` 模块的战斗 DAO 里）。只有 `HP`、`Attack`、`Power` 三个属性，要更多属性需要改属性定义文件。
- **`Power` 的算法**：先把各层的直接属性（`HP`、`Attack`）相加，再对合计值推导一次 `Power`；不是把各层的 `Power` 相加。
- **上限**：`HP`、`Attack` 每项不超过 `math.MaxInt64/4` 且不为负；越界时返回 `attribute_overflow`，整笔事务回滚。
- **一致性**：升级成长、装备变化引起的刷新都在调用方的同一笔 Nest 事务里，任一钩子失败时经验、等级、基础层、最终层一起回滚。加载实体时只派生两层非持久数据，不写基础记录。
- **不会自动刷新**：改了成长数值或装备规则（包括配置热更）后，在线玩家的属性不变，要由工程在事务里调 `Refresh()`；玩家重新加载时会按新规则重算非持久层。
- **基础层保留加成**：持久基础层里工程自己加进去的数值在升级时保留，升级只在现有值上加成长量。
- **`AttributeGet` 只读已派生的最终层**，不触发重算。
- **前提**：依赖 `profile` 模块（等级来自 `ProfileComp().Level()`）；需要 `nest` Mod 与 `attribute` 特性（安装时启用）。实体必须叫 `Player`。只在持有这名 `Player` 的进程内有效。
- **已知限制**：`docs/maintenance/KNOWN-LIMITS.md` 没有专门针对本模块的条目；通用的“模块绑定具体的实体名与工程形状”“不装 enter 时安装不打开客户端同步”两条适用。

## 复用方式

- **需求正好是它**：`roost module add attribute`，连带安装 `profile`。
- **需求比它多一点**
  - 改成长数值、缺省值或战力公式：改 `game/gameplay/attribute/combat.go`（`ForLevel`、`LevelUp`、`_Power`）。
  - 加属性（防御、暴击等）：在 `Combat` 结构里加带 `attr` 标签的字段，`roost generate`；`attribute_component.go` 的 `refresh` 与 `attribute_gear.go` 的 `attributeGear` 里目前只合并 `HP` 与 `Attack`，新属性要在这两处补上合并与溢出检查，协议 `AttributeGet` 的应答也要加字段。
  - 加一个属性来源（坐骑、称号、公会科技）：在自己的包的 `init` 里调 `OnAttributeGear("<来源名>", 钩子)`，并在来源变化的事务里调 `AttributeComp().RefreshGear()`。不要直接写 `AttributeGear` 或 `AttributeFinal` 字段。
- **别的模块 / 业务代码要用它**
  - 取玩家当前属性做计算：在持有 `Player` 锁的 handler 里 `p, ok := target.AttributeComp().Final()`，拿到的是副本。
  - 没有持锁时：用协议 handler 的生成 Sender `syncsender.NewAttributeGetSender(client).Sync_AttributeGet(ctx, 完整实体号, &pb.AttributeGetRequest{})`。
- **只适合借鉴的情形**：怪物、NPC 等非 `Player` 实体的属性不能直接用本模块（组件绑定在 `Player` 上）；值得借鉴的是“持久层只存基础值、派生层加载时重建、来源经登记点汇总、公式只在合计后推导一次”的分层方式。

## 联动

- `attribute` + `equipment`（`with/equipment/game/entities/player/attribute_equipment.go`）：登记 `OnAttributeGear("equipment", …)`，按已穿戴装备与等级算装备层；并登记 `OnEquipmentChanged("attribute", …)`，穿卸装备的同一笔事务里调 `RefreshGear()`。装备规则已被移除的穿戴物不再提供属性。
- `attribute` + `enter`（`with/enter/game/entities/player/attribute_enter.go`）：登记 `OnFirstEnter("profile.attribute", …)`，首次建档时调 `Refresh()`，按初始等级建立基础层与最终层，与建档一起提交或回滚。
- `scene` + `attribute`（在 `scene/with/attribute`）：把 `AttributeFinal` 登记为对附近玩家公开的字段，基础层与装备层仍只有本人（或无人）可见。
- 以 `requires` 依赖它的模块：`combat`（开战时从 `Final()` 取最大生命与攻击）。

## 验证范围

- `roost module verify attribute`：16 个场景（`./game/handler` 与 `./game/gameplay/attribute`，`^TestAttribute`）；与 `equipment` 的联动另有 8 个场景，与 `enter` 的联动另有 2 个场景（都在 `./game/handler`）。
- 场景是离线的：真实的 Nest 引擎、生成的 DAO、属性生成代码与 `profile` 组件；提交器是替身（`attributeCommitter`，可注入拒绝）。
- 覆盖：新玩家只派生不落库、升级成长、一次升多级只成长一次、只加经验不升级时不写基础层、`Final` 与 `Container` 返回副本、`Refresh` 落库且重复调用稳定、提交被拒与钩子失败时等级和各层一起回滚、非法基础值拒绝、并发加经验串行成长、协议使用认证身份、基础层加成在升级后保留、重新加载后重建派生层且不写基础层；生成的属性定义拒绝直接写推导属性。
- 没有验证：真实数据库、经 TCP 的客户端链路与同步载荷到达客户端。
