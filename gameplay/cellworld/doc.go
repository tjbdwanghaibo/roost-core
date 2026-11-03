// Package cellworld 是多进程无缝大世界的 v1 原型（分支原型，不是现行能力）：沿 BigWorld 的 Cell + Ghost 流派，
// 静态方格分区，每个格由一个 Cell 进程模拟；实体在所属格是 Real（唯一写权威），靠近边界时在邻格留只读
// Ghost；越过边界时 Real 交接给新格。Player 不迁移：Player 所在 game 进程用 Base 充当 BaseApp，只跟随
// 玩家当前所在的 Cell，转发输入与攻击、接收视野。空间代理全部非持久，Cell 崩溃即丢失，由 Base 重新进入补回。
//
// 本包不依赖 Nest、DataEngine、Sync：Cell 用自己的单 goroutine 事件循环（心跳周期取 heartbeat 档位），
// 块索引用 infra/base/spatial，通道经 infra/network/nats 的 IClient。设计与取舍见
// docs/maintenance/FEATURE-2026-11-03-seamless-world.md §5，实测与已知缺口见 §6。
package cellworld
