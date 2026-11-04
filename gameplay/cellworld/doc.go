// Package cellworld 是“分区 Scene”的原型（分支原型，不是现行能力）：沿 BigWorld 的 Cell + Ghost 流派，静态方格分区，
// 每个格由一个进程托管；实体在所属格是 Real（唯一写权威），靠近边界时在邻格留只读 Ghost；越过边界时 Real 交接给新格。
// Player 不迁移：Player 所在 game 进程（Base）只跟随玩家当前所在的格，转发输入与攻击、接收 witness 喂给客户端。
//
// v2 执行模型（docs/maintenance/FEATURE-2026-11-03-seamless-world.md §7）：状态全在 Nest 实体上——格的区域表（Region：
// Ghost、交接 seen、墓碑、跨格伤害待决表）由该格的 Scene 实体持有，角色 / NPC 的格内状态（ActorState：位置、HP、
// 去重表、交接进度、租约）在 SceneActor / Monster 的 DAO 上，玩家的路由状态（BaseState）在 Player 上；推进是各实体
// 自己的心跳（Node.ActorBeat / RegionBeat / BaseBeat），交接以 Nest 事务为边界（提交才外发，回滚不移交）。Node 是
// 每进程一个的端点：把传输上的消息投成 Nest 请求，把提交后的发送 / 报告 / 销毁排进出站队列。客户端经 Base 进程
// entitysync Manager 里的 Sync mirror（policy.Mirrors）看到格里的对象。
package cellworld
