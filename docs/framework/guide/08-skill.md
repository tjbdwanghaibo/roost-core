# 游戏技能、战斗与空间：设计与使用

适用运行时：v1.24.0；文档维护版：v1.24.1。[实现与维护入口](../impl/08-skill.md) · [模块总目录](../README.md)


## 1. 包的职责

skill 编译 DSL 并在离散 tick 推进 Runtime；Host 提供实体、资源、目标、运动和效果的业务能力。combat 是战斗计算，combatcomponent 把战斗状态接到实体/DAO；attribute 管属性计算，spatial 管空间查询。skill 不直接依赖某种地图实现，空间查询经 Host。

这里的 skill 是游戏技能，保留对应 DSL 与使用手册；清理的 agent-skills 和 AI prompt 是开发辅助文档，不能把游戏技能资料一起删除。

## 2. 编译与执行

DSL 解析/验证 → lowering → 编译计划 → Runtime 注册/施放 → tick 推进 → Host 执行效果与产生状态/表现增量。编译期尽早拒绝缺失能力和非法上下文，Host 必须如实声明能力，包装 Host 也须转发可选接口。

Host 回调应确定、非阻塞、不可违规重入，StopSpawn 幂等。业务为随机性提供可复放输入；框架无法从同一 DSL 自动证明跨平台浮点、物理与宿主逻辑确定性。

## 3. Spawn 与 Summon

Spawn 是由 Runtime 驱动的飞行物、区域、光束、位移、环绕等衍生物；Summon 是 Host 创建并管理的真实单位。minion 是跟随召唤单位生命周期的衍生物类别。召唤单位自身的实体同步由业务负责，不能把 Spawn 状态流当作单位完整权威。

衍生物从召唤后下一 tick 起推进，包括施法尚未结束期间。停止入口统一处理，失败停止有有限重试及保留状态；放弃状态不表示 Host 侧资源一定已消失，宿主仍须按场景生命周期清理。

## 4. 事务与战斗边界

Skill Runtime 不在 Nest DAO 回滚域内；handler 失败不会自动回退 cooldown/ammo/cast/proc/Spawn 账本。战斗 DAO 可以随实体事务恢复，两者不能混淆。HostAdapter 是需业务包装的适配器，不等于 MemoryHost 的全部能力。Buff 推进由业务 Tick 驱动，不应等待 Runtime 自动调所有战斗组件。

## 5. 同步与持久格式

skillsync/skillcompose 组织状态与表现流。表现也遵守可靠有序与基线恢复契约；隐私字段由业务 projector 裁剪。关闭 observer 不撤回已经在飞的 Publish。journal 恢复优先，Import 产生新 checkpoint 代际；空闲清理由 Coordinator 统一控制。

当前 Runtime checkpoint 格式为10，旧格式拒绝；JSON wire schema、编译器语义版本和 Go 模块版本是三种概念。现行接入按同一个 roost-core tag，不使用旧三仓发布图。详细 DSL、效果与 Host 参数见 docs/skill 的专项手册。


## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
