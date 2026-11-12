# team

`roost module add team` 安装持久队伍、Player 队伍引用与六个玩家 PB 端点。自动加入已有 remote_entity Mod；运行需 Mongo / Redis / NATS。队伍可跨 game 进程，成员表是 Remote 共享权威，成员操作与本人的引用在一笔 `[Team, Player]` Nest 事务里提交。

先 TeamCreate，再由队长 TeamInvite，被邀请方 TeamAccept。队长离队交给玩家号最小者；TeamInfo 只读本人的队伍，解散后的引用在 TeamInfo / TeamLeave 时惰性清除。TeamCreate 重试返回已有队伍；并发首次创建可能留下未建成的空实体，号不复用。邀请缺省 60 秒、最多 64 份，队伍最多 8 人；规则在实体的 roster.go 中，归生成工程所有。

解散不在锁外逐个改成员档。其他成员操作前先查询 TeamInfo；残留引用会保守阻止换服，核对权威后即可解除。与 rehome 同装时，两边事务的准入检查确保不能同时入队与换服。

仅完成组队基础，不提供匹配、匹配房、队友视野或推送。相应模块和场景联动按交接 R7 后续实现。离线生成场景不等同跨进程 Remote 存储验收。

## 验收与使用边界

新工程先安装 `enter team`（鉴权可再装 login），通过 EnterGame 建好 Player 后请求上述端点。`TeamID` 是完整实体号；`Invitee` 与 `Members` 使用玩家号。客户端不能指定操作人或其 sid：它们来自认证会话和本服务配置。队伍数据通过 PB 查询返回，没有直接向其他成员推送名单。

生成工程通过 25 条基础场景、与 rehome 的 2 条联动，8 个变异自检和两轮定向 race；验证了同一提交记录同时含生成的 Remote 变化与 Player 变化。提交器、Remote 外部锁与权威回执为隔离内存替身，不能据此宣称跨进程持久验收完成。安装器还验证缺少 remote_entity 时自动补齐，重复安装不改文件；完整正反序组合和真实依赖按交接最后统一验证。


## 匹配与队伍视野

matchmaking 在 `[Team, []Player]` 中保存排队意图与 Team.MatchKey，排队期间冻结名单；确定终态后按同一 Key 解除。每个成员有自己的匹配结果，不依赖队长在线。

`OnTeamChanged(name, hook)` 为事务内的组关系投影入口，回调出错整笔回滚，不能在回调里做 I/O。demo 使用它更新 cellworld 组缓存，通知经 Guard 的提交后机制发送；刷新查询不全队广播。该 hook 不负责房间、匹配规则或客户端同步格式。
