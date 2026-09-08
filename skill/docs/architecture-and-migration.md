# roost-skill 架构、迁移与同步流程

## 1. 模块边界

依赖方向固定为：

```text
roost-core/syncstream
        ^
        |
roost-skill/skillsync       roost-kit/syncstream
        ^                         ^
        |                         |
roost-skill/skill        roost-core/syncbus.ISyncBus
        ^                         ^
        |                         |
     game host                 NATS / JetStream
```

- `roost-core/syncstream`：只有 Observer、Stream、Packet、序号、ACK、有限历史、
  自动全量恢复、持久化表示和指标，不知道技能、实体渲染或 NATS。
- `roost-kit/syncstream`：把 Packet 编码为现有 `sync.SyncMsg`，复用 NATS 或
  JetStream；不解释技能 Payload。
- `roost-skill/skill`：编译和执行权威技能逻辑，产生不可变 PresentationPlan、
  有序 PresentationEvent、RuntimeStateSnapshot 和强类型 StateEvent。
- `roost-skill/skillsync`：定义技能 Manifest、状态全量/增量、表现事件的 JSON
  记录，提供服务端 Coordinator 和客户端 Applier。
- 具体游戏：实现 `skill.Host`、决定 Observer 可见范围、生成状态快照、选择
  NATS 或 JetStream、处理断线重连。

任何依赖反向指向具体游戏仓库、渲染器或网络实现，都视为边界违规。

## 2. 表现数据与权威状态为什么分流

`RuntimeEvent` 是玩法内部事件，`TraceEvent` 用于诊断和确定性回放，二者都不应
当作客户端同步协议。新的三条流用途不同：

| Topic | 数据 | 可靠性 | 恢复策略 |
|---|---|---:|---|
| `roost.skill.manifest` | Program 的视觉表和挂载计划 | 必须可靠 | 重新发送 Full |
| `roost.skill.state` | Cast 状态全量或业务增量 | 必须可靠 | 历史连续则 replay，否则 Full |
| `roost.skill.presentation` | cast/effect 播放指令 | 可丢弃、需有序 | 短历史 replay；过期后以状态为准 |

玩法修改必须先由 Host 成功提交，再产生 effect PresentationEvent。Cast 表现只在
cast commit 后产生。因此客户端永远不会先看到一个被权威层拒绝的效果。

## 3. 服务端发布流程

1. 编译技能并保存 `InspectIdentity(program)`。
2. 调用 `InspectPresentationPlan(program)`；按 PresentationDigest 缓存并发布一次
   Manifest Full Packet。
3. Runtime 激活或推进后调用 `Coordinator.Flush(observer, key)`；它分别轮询
   `StateEvents` 和 `PollPresentation`。
4. Coordinator 先执行 VisibilityPolicy，再用 Projector 生成强类型 Packet。
5. `syncstream.History.Append` 分配 Observer + Stream 独立序号。
6. Append 成功后推进 source cursor，再交给 `roost-kit/syncstream.Publisher` 发布；
   publish 失败的数据仍留在 History。
7. 新 observer、游标过期或 schema/gap 恢复时发送 state full 或 presentation reset。

不要直接把 `PresentationEvent.Sequence` 当作网络流序号。它是单 Runtime 事件序号；
网络序号必须由 `History.Append` 按 Observer + Stream 分配。

## 4. ACK 与重同步

客户端 ACK 的对象是网络 Packet.Sequence：

1. 服务端调用 `Coordinator.Acknowledge(observer, stream, epoch, sequence)`。Coordinator
   在 observer/key 生命周期锁内先验证 epoch/sequence，再持久删除对应 Outbox，最后提交
   History ACK；Outbox 删除失败时 History 保持不变，History WAL 失败时立即从 History
   重建 Outbox。业务层不要分别调用这两个底层操作。
2. 客户端重连时提交 AfterSequence 与 SchemaVersion。
3. `History.Recover` 返回连续 Packets 时，按顺序重放。
4. 无法 replay 时，Recover 自动调用 SnapshotProvider 产生并 Append Full，Reason 表示原因：
   - `history_missing`：首次连接或服务重启，生成 Full；
   - `history_gap`：历史窗口已经覆盖，生成 Full；
   - `schema_mismatch`：客户端协议不同，发送兼容 Full 或拒绝升级；
   - `client_ahead`：客户端串服/回滚，清空客户端游标并发送 Full。
5. Full Packet 会创建新的恢复锚点；后续 delta 的 BaseSequence 指向它。
6. `History.Export/Import` 用于重启恢复；Import 先完整验证再原子替换。

## 5. 发布与迁移顺序

这是三个仓库的原子设计变更，但版本发布必须按依赖方向进行：

1. 发布 `roost-core`（v1.10.0；包含 `syncstream`，模块路径自此为 `roost-core`）。
2. 发布 Go 模块 `github.com/tjbdwanghaibo/roost-skill`（核心 API 固定在 `/skill`；wire schema 仍独立使用 `roost.skill/v2`）。
3. 发布/确认 `roost-kit`（v1.10.0）transport adapter。
4. 具体游戏升级 roost-skill/roost-kit，替换旧导入路径：

```text
github.com/tjbdwanghaibo/cube/game/gameplay/skill
=> github.com/tjbdwanghaibo/roost-skill/skill

github.com/tjbdwanghaibo/cube/game/gameplay/skillcompose
=> github.com/tjbdwanghaibo/roost-skill/skillcompose
```

从 `/skillv2` 到 `/skill` 是源码 API 的破坏性升级，但不改变当前 wire、checkpoint 或
outbox 格式。导入替换、验证和回滚流程见
[稳定包迁移手册](breaking-upgrade-skill-package.md)。

本地多仓开发使用 `go work`，不要把临时绝对路径写进代码。

## 6. 测试流程

### 6.1 单模块

```powershell
go test ./syncstream -count=1                         # roost-core
go test ./syncstream -count=1                         # roost-kit
go test ./skill ./skillcompose ./skillsync -count=1 # roost-skill
```

### 6.2 跨模块工作区

在四个仓库的共同父目录建立不提交的 `go.work`：

```text
go 1.25.0
use (
    ./roost-core
    ./roost-kit
    ./roost-skill
)
```

然后依次执行：

```powershell
go test ./...           # roost-core
go test ./syncstream    # roost-kit adapter
go test ./...           # roost-skill
go vet ./...            # 三个模块分别运行
go test -race ./...     # 三个模块分别运行
```

### 6.3 必测故障矩阵

- Manifest：同一 Program 输出稳定摘要，返回值不别名 Program 内存。
- 权威失败：invalid target、资源不足等 expected failure 不产生 effect 表现。
- 顺序：cast commit 事件先于同 tick 的 effect 事件。
- 游标：`PresentationEvents(after)` 只返回更大序号。
- 限制：PresentationLimit 和 History MaxPackets 生效。
- ACK：重复/倒序 ACK 无副作用，超前 ACK 返回错误。
- Gap：历史被覆盖后 FullRequired + history_gap。
- Schema：版本不一致 FullRequired + schema_mismatch。
- Observer：相同 Topic/Key 的不同 Observer 序号互不污染。
- Wire：内层 Packet 的 topic/key/sequence 与外层 SyncMsg 不一致时拒绝。
- Visibility：nil policy 拒绝启动；跨 observer packet 在发送端和客户端均拒绝。
- Persistence：非法快照 Import 不改变现有 History；合法快照 payload 不别名。
- Backpressure：有界队列满时显式返回错误，已接受包在 Close 时排空。
- Transport：NATS 自消息过滤；JetStream 重试、去重 ID、handler 错误传播。

### 6.4 验收门槛

发布前必须同时满足：所有 fixture、普通测试、vet、race 通过；仓库中不存在旧
导入路径；roost-core 不导入 roost-skill/roost-kit；roost-skill 不导入具体游戏；
roost-kit adapter 不导入 skill。
