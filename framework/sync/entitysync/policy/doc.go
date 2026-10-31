// Package policy holds the ways subscriptions get organized on top of an
// entitysync.Manager: by views (Interest: spatial views over an AOI grid and
// relation views over explicit entity sets), by membership in a group (Group),
// or one binding at a time (Direct). None of them touch the wire or the
// frames — a policy only decides WHO receives WHOM and says so to the
// manager. The manager, in turn, knows nothing about any of them.
//
// SelfView（玩家看自己）与 GroupView（成员看组实体，如房间）是建在这些机制上的生命周期实现：会话由 SelfView
// 按玩家连接打开与关闭，GroupView 与它装在同一个 Manager 上，把成员关系当作事实、在会话打开的政策阶段订阅。
// 调用方自己开关会话时用 Group；会话归 SelfView（access.player.self_sync）时用 GroupView（wiring/selfsync.Groups）。
package policy
