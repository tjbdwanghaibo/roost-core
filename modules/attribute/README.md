# 属性模块

`roost module add attribute` 自动安装 profile 并启用 attribute feature；Combat 的 ID、Setter、元数据与公式更新由正式 attribute 生成器产出。`module verify attribute` 固定16场景、零跳过；与 equipment 联动另8场景。

`AttributeComp().Final()` 返回最终 Combat 副本，`Container()` 返回基础/装备/最终三层副本，均先持 Player 执行权再读取。战斗可以在锁外使用这份副本。HP/Attack 是直接属性，Power=Attack*2+HP/10，最终值先合并直接属性再推导；不会把层的 Power 相加。默认0级100HP/10Attack，每级+10HP/+2Attack。

AttributeBase.Values 持久且同步；AttributeGear.Values 非持久且不下发；AttributeFinal.Values 非持久且同步，每层嵌套字段使用 store=1。加载只派生非持久层，不写基础记录。Refresh/RefreshGear 在 Nest 事务内刷新并首次保存基础值；热更不会扫描在线玩家，工程按需要触发刷新。持久基础中的工程加成随升级保留；上限为 MaxInt64/4，负数和溢出拒绝整次事务。

profile 的工程 onLevelUp 先执行，然后 OnProfileLevelUp(name, hook) 按名执行；attribute 登记一次批量成长。任意钩子失败都必须交还 Nest，经验、等级、基础和最终层一起回滚。钩子仅启动期登记、不能阻塞或做外部 I/O。同样通过 OnAttributeGear 注册可选装备来源；重复名、空名和 nil 均 panic。

与 equipment 同装后自动联动。没有 equipment 时装备层为零，不强制依赖装备；提供的是基础层业务，不包含 buff、随机词条、实例战斗状态或客户端网络验收。事务场景使用替身提交器，真实存储依赖和 TCP 不在这些测试口径内。
