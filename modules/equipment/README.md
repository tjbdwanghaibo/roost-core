# 装备模块

`roost module add equipment` 自动安装 bag；默认 equipment_item.csv 引用 bag 的1001/1002/1003，可直接生成新 game。`module verify equipment` 固定16场景、零跳过。与 attribute 同装自动刷新装备层，联动另8场景。

EquipmentComp().Equip(itemID,level) 返回 slot,replaced,error，内部扣一件新物品并退一件旧物品。Unequip(slot) 返回 itemID,found,error，内部退回背包。不要在调用方重复扣回背包。调用须在已持 Player 锁的 Nest 事务内，任何错误必须交还 Nest；后续联动失败与提交拒绝会连同背包、穿戴、属性回滚。先扣新物品再退旧物品，种类满时可以复用腾出的空位。相同物品与等级已穿着、空槽脱下幂等；并非跨换装序列的请求账本，延迟旧请求可能再次改变穿戴意图。

协议 EquipmentEquip 仅收 itemID，等级固定1；可信服务端可传1..10000。Worn(slot)/RangeWorn 提供只读值。每个槽最多一件、槽号1..32，所有持久字段明确 store。物品是堆叠道具，没有独立实例、耐久或随机词条；升级后的等级只属于穿戴事实，脱下退回的是基础堆叠道具，不保留强化级，需实例装备的项目应扩展事实模型。

装备规则单独位于 equipment_item，ItemID 引用 item.ID，Slot 确定槽，Attack/HP 为每级非负加成、单项至多1e12。不会改写 bag 的道具 schema。移除规则使新穿戴被拒、已穿装备暂不提供属性；背包退回仍受 item 表和容量约束，失败保留整件装备。正式迁移由工程负责，不自动销毁物品。OnEquipmentChanged 在修改后按名字执行同事务钩子，仅启动期登记，不能做外部 I/O。

这些正式生成场景使用内存替身提交器；真实存储、TCP、跨机恢复未覆盖。
