# B2 战斗包收尾方案

按交接 C1～C16 逐条处理。确认缺陷先留实际红测；疑点不能因结构守卫通过就宣布排除。

- 数值边界：回避阶段终止后续钩子；吸血复用治疗的存活/缺血约束；损坏的负血量拒绝。modifier 聚合保持精确和可撤销，在 Current 进入固定点公式时才饱和，不能以逐次饱和破坏顺序无关性。
- HostAdapter：缺资源映射返回错误；资源池读/写/支付统一为 base，属性读仍含 buff；校验声明的 DamageType/Element/公式策略。
- buff：永久期限显式保持 0，重新应用规格后钳制层数，整数加法饱和；无变化不标持久/同步脏位。
- C14：DAO 只有在 undo 策略且本字段尚无逆操作时才复制字段。为 Nest 加只读 HasUndo 查询，仍由 DAO 登记首次逆操作；state 策略仍依赖声明目标快照，不暗中跨实体上锁或另起事务。
- C11/C12：按单次伤害预掷和业务 tick 驱动的真实边界补接入说明，不把持久 vitals 的 ForceCritical 当作一次性钩子。
- C13：优先核实正式 Nest 锁定/快照范围，不能用隐藏 resolver 获取未纳入事务的对象后宣称获得事务保证。验证吸血来源与 buff 迁移目标均纳入正式派发。
- C15：GridTerrain 搜索一次持有一致读视图；预算计实际展开节点，跳过失效队列项；BlockIndex 复核现有 TestBlockIndexRejectsUnsafeAllocationAndDeduplicatesRange 后确认允许同一 ID 占多块、查询并集去重，保留契约；不强加单位置登记。最初单位置假设产生的红测已排除，不作为缺陷证据。
- C16：根包依赖守卫钉住 combat 仅标准库、skill 根包不依赖 spatial，说明这是结构约束而非行为正确性证明。
