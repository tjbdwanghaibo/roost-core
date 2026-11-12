# activity_world：全服/服务器组活动

`roost module add activity_world` 同时安装 activity，引入 `ActivityWorld` 权威实体与独立 `activity_world` 服务。逻辑组 ID 是实体唯一号，不是执行 SID；服务以 Remote ownership 单写。挂载的是同一个 `activity.Component`，仅提供自己的 DAO、归属与持久创建时间；初始化、推进、异步完成、循环都复用共用实现。

`configs/table/activity.csv` 配置 `Scope=world` 的定义；`activity_scope.csv` 指定逻辑 ID、首次承载的 BootstrapSID 和真实 CreatedAt。两个表默认都为空。服务启动先经 lifecycle 建档/加载和正式 Nest 初始化，再开启 RPC 和有界恢复扫描；迁移后的旧属主会被拒绝。

服务器使用生成的 `internal/rpc/activity_world` 客户端：`Pulse(target, event, createdAt)` 返回视图并对账，`Complete(target, token)` 确认可靠后台完成。target 是完整 ActivityWorld 实体 ID。接口只供可信服务器使用；面向玩家的端点应先校验区域/资格，不能把客户端自报事件直接转发。

完整生命周期、日历、可选奖励接口与限制见 [activity](../activity/README.md)。扫描不是毫秒级定时器；真实存储与跨进程迁移留待最终验收。
