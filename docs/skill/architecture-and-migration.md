# Skill 架构、迁移与同步流程

当前为单仓单模块 `github.com/tjbdwanghaibo/roost-core`。`skill/`、`skill/skillcompose`、`skill/skillsync`、`syncstream/` 同一个版本发布；不再按 core/skill/kit 三仓排序发布。本文以本轮 v1.23.1 修改为准，尚未发布。

## 1. 包边界

- `skill` 编译并运行权威技能，产生状态快照/增量和表现事件。
- `skill/combat` 提供战斗数学；`skill/combatcomponent` 把战斗状态接到 Entity/DAO，业务适配 Host。
- `skill/skillsync` 提供 Manifest、state、presentation 三流、Coordinator 与客户端 Applier。
- `syncstream` 拥有 Observer/Stream 的网络序号、历史、ACK、恢复；同包 Publisher 提供 JSON/gzip、分片、校验和及 SyncBus 适配。
- `sync/syncbus` 是总线契约；NATS/JetStream 是传输实现。向客户端发技能流时，宿主负责相应传输适配，不能把它与 entitysync 的对象复制协议混为一谈。

依赖不能反向指向具体游戏或渲染器。Runtime/Host 的回调与锁契约见 [skill.md](skill.md)；Runtime 不是 Nest 回滚事务的一部分，失败回滚只覆盖被捕获的 combat DAO。

## 2. 三条流

| 流 | 内容 | 恢复 |
| --- | --- | --- |
| manifest | Program 的视觉表与计划 | 重发 Full |
| state | 状态全量与业务增量 | 连续 replay，否则 Full |
| presentation | cast/effect 表现指令 | 可靠有序；缺口恢复，过期后 reset |

表现包也不能任意丢弃后继续发送增量，否则 Applier 报序号缺口。晚加入 observer 从当前表现游标开始，不重放入场前的旧表现。是否暂停、换线、重建观察者属于宿主策略。

## 3. 发布与 ACK

Coordinator 从 `StateDeltas` / `PollPresentation` 取源记录，先做 VisibilityPolicy，再构建网络流。Runtime 的 source sequence 与 History 的网络 sequence 独立；多个 observer 的进度也独立。

History 分配网络序号；新建 delta 流首包 BaseSequence=0，后续指向前一网络包，Full 的 BaseSequence=0。不能把 source sequence 直接当 ACK 序号。

持久 outbox 保存尚未确认的网络记录。`Coordinator.Acknowledge` 在 observer/key 生命周期锁内验证 epoch/sequence，先删除 outbox 再推进 History ACK，失败按其恢复契约重建；业务不要分别调用两个底层步骤。重连经 Recover/Resync：连续历史重放，缺口或 schema 不匹配时全量恢复。

## 4. 生命周期与可见性

`CloseObserver` / `SweepIdle` 负责协调器状态和对应 pending 清理；在飞的外部 Publish 不可撤回，CloseObserver 不是外部传输完成栅栏。宿主应隔离旧连接、代际与旧 ACK。

显式关闭最后一个 key 可以让观察者离线；业务不能把底层 History 的 SweepIdle 当作 Coordinator outbox 清理入口。文件时间戳是系统时间，进程单调时间不能跨重启比较。

可见性投影器必须覆盖实际业务字段与嵌套载荷；新字段不会因为“默认 projector”这个名字就自动获得隐私保护。具体支持范围和回归见 B1 的 RR-20261007-07～25。

## 5. 持久化和升级

当前 Runtime checkpoint 版本为 **10**。格式变化按整套升级，旧版本明确拒绝，先停旧进程、清理或离线重建旧 checkpoint/outbox，再启动新进程；不能用旧二进制灰度读取新格式。世界状态与 Runtime checkpoint 必须来自同一恢复点。

History 绑定 journal 后，正常恢复依靠 WAL/checkpoint；Import 是受校验的替换操作，每次建立新的 checkpoint generation，不能把它当后台周期刷新。纯内存 History 由宿主选择持久策略。

FileJournal 直接并发 Append 可合批；单 History.Record 被其写锁串行，不能据此宣称每个业务 Record 都合批 fsync。

## 6. 接入验证

```sh
GOWORK=off go test ./skill/... ./syncstream -count=1
GOWORK=off go test -race ./skill/... ./syncstream -count=1
cd skill/integration/sync-e2e
GOWORK=off go test -race ./... -count=1
```

验收包括源游标到网络序号、ACK 与 outbox 一致性、晚加入/重连、可见性、重启恢复及容量拒绝。Exporter/Health 是公开集成接口，宿主周期采样并注册，不随 App 自动接线。

详情：[生产同步指南](visual-sync-production-guide.md)、[生产边界](production-readiness.md)、[战斗接入](skill-casting-and-combat.md)。旧 `/skillv2` 改包历史见 [历史迁移](breaking-upgrade-skill-package.md)，其中旧“格式不变/可灰度”只描述当时那次源码改名，不适用于本轮格式升级。
