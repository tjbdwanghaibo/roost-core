# activity_alliance：联盟活动

`roost module add activity_alliance` 同时安装 activity 和 alliance。它给已有 Alliance 的主 DAO 追加 ActivityAlliance，挂共用 `activity.Component`（本地别名 AllianceActivityComponent），并为已有 alliance 服务追加配置 Mod 和 activity_alliance RPC；只补缺少的配置段，不覆盖联盟服务文件、已设配置和所有权。

`configs/table/activity.csv` 使用 `Scope=alliance`。联盟建成、升级等条件在**联盟实体自己的 Nest 事务**里调用 `AllianceActivityComp().Reconcile(event, createdAt)`，Eligible 读取权威实体的真实条件；不由 game Mirror 执行。已有固定时间实例及 pending 由权威服务的恢复扫描继续推进。

服务器通过 `internal/rpc/activity_alliance` 的 Pulse / Complete 协作。它们只用于可信服务，必须自行验证成员和操作资格；不能将“客户端声称已解锁”直接转发。入盟、退盟、承载 game 变动不重开联盟时间线；贡献归属、退盟后领奖范围由业务规则决定。

共用状态机、日历循环、异步结算、可选排行/邮件与验收边界见 [activity](../activity/README.md)。Remote 本地定时器仍不支持，本模块通过持久期限和权威扫描恢复，不修改 Nest/DataEngine/Sync 契约。
