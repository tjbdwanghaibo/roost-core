# 网络体验：时间、调度、预测与诊断

本轮范围：沿正式 Gate、EntitySync、C# / native 客户端补齐网络时间估计、有界插值、连接级状态调度及输入预测接口。保留 handler 提交后的 on_change 和周期兜底；不引入全世界固定帧循环，不改变 WAL、Remote、Saga 的持久化契约。PVS、命中回溯和全局确定性回滚不在本轮范围。

## 实施契约

1. **诊断**：复用服务端 SyncTrace 的 session/lifetime/epoch/tick，客户端记录应用与展示阶段。环形容量固定，明确覆盖计数；不采集业务 payload，不直接相减不同机器未经校准的时间。应用延迟与展示年龄分别报告。
2. **校时与插值**：Gate 在鉴权后的控制通道处理有界时钟探测，不经过 Nest 业务 handler；客户端以本地单调时钟测往返，接收服务端采样时间，拒绝异常/过旧样本，限速修正时钟。插值缓冲与权威 EntityViews 分离，只有表现层选择使用，不延迟资产/删除等业务回调。默认不开启自动探测。
3. **发送调度**：Manager 的可选连接预算在组帧前按完整 subject 单元选取，和 Profile 授权选择分开；等待时间防止低优先级永久饥饿。删除优先。没有交付的订阅不推进基线；若全局内容版本已提交，该订阅转为当前全量恢复，禁止保留失效 delta 或拼接跳版本增量。预算是允许超额以保证进度的软预算，硬帧/队列上限仍有效。默认关闭。
4. **预测**：共享客户端能力只管理有界输入历史、确认和重放；业务提供复制状态及纯模拟函数，权威状态、预测状态与显示状态分离。重放不做持久化/RPC/发奖，副作用由业务按输入序号去重。既有物理 viewer 的最新输入邮箱语义保留，不能改成每条输入都在服务器执行一次。

## 验证要求

- 校时覆盖对称延迟、异常 RTT、倒退输入时间、时钟微调及重连重置；明确非对称链路的偏移不确定性。
- 插值覆盖乱序/重复、容量、抖动、停顿、状态过期与重置，禁止无界外推。
- 调度覆盖慢/快连接隔离、连续改变、延后恢复、删除/重订阅、重试、基线与最终值一致，运行 race。
- 预测覆盖确认、重放、历史溢出、旧确认和清理；实际接入已有物理 viewer，不只增加离线工具类。
- 正式 Gate 控制协议和客户端通过真实回环网络核对；原有客户端与 Sync 回归必须通过。实际测量和环境限制在完成后记录，不能提前标记为通过。

## 正式接入

### 诊断

`ManagerConfig.Trace = entitysync.NewSyncTrace(...)` 记录提交就绪、捕获、选择、编码及传输准入；构造函数返回 `(trace, error)`。`TCPConfig.WriteTrace = gateway.NewTCPWriteTrace(...)` 记录真实 socket 写尝试。两个环都需定期 `Drain`，返回的覆盖计数是累计值。默认 nil 不启用。

TCP 写记录包含 PlayerID、连接 nonce、message ID、sequence、Sync epoch/tick、实际写入字节、失败标记及耗时。耗时包含 writeMu 等待，不含入队前/出站队列等待；只说明本机写调用完成，不说明远端 ACK、解码或显示完成。通过玩家/业务路由/连接代际/epoch/tick 关联 SyncTrace；不同 Manager 的流要由业务路由区分，不能只按 tick 合并。

C# `EntityViews.Trace` / C++ `entity_views.trace` 在成功应用帧后记录 Apply。显示层自行记录 `ClientStage.Present` / `client_stage::present`，用同一个 epoch/tick；插值显示要由业务明确选择所代表的快照。`ClientTrace.Drain()` 和 `client_trace.drain()` 消费有界环，`Overwritten` / `overwritten()` 表示丢失的诊断记录。C++ 指针由调用方持有，必须比 views 活得久。

这条链路用于定位阶段，不提供自动跨机精确延迟报表。各机的 UTC、客户端单调时钟以及 Game 场景时间不同；未校准时只比较各自阶段耗时，不能直接相减。当前没有单独拆出 native 工作线程到 Pump 的排队耗时。

### 校时与表现

已认证 RS ID=0、flags=0、payload=`RCN1`（4 字节）请求，回复沿用 sequence，载荷为 `RCN1` + 接收 UTC Unix 纳秒 int64 + 写入队列前 UTC Unix 纳秒 int64，总共 20 字节，均为大端。空 payload 仍是心跳；鉴权前仍把首包解释为票据。此控制功能沿用 `HeartbeatEnabled` 和每连接请求限速，不调用业务 handler。关闭心跳也关闭校时探针。

三类入口：

- C#：`GameClient.ProbeClock(completed, failed)` 在 Pump 回调，底层也可用 `Connection.ProbeClockAsync()`。
- Unity native：`NativeGameClient.ProbeClock(completed, failed, timeoutMilliseconds)`，回调在 Pump。
- C++：`game_client.probe_clock(...)` 返回解析后的 `clock_sample`；低层 `client.clock_probe` / C ABI `roost_clock_probe` 返回普通 RESPONSE/REQUEST_FAILED 事件。

由业务以低频发探针，不按每个 handler 或每个渲染帧发送。断线/换服重置估计器；旧连接完成不能更新新连接时钟。控制样本来自 **Gate UTC**，只有 Game 使用同一已校准时间域时才能交给 `SceneTimeDomain.Now`。Sync 的 tick 是每连接递增帧号，尤其 on_change 模式下，**不能用 tick × 50ms 推算服务器时间**。场景应另同步权威时间戳/时间锚点。

`NetworkClock` / `network_clock` 默认拒绝超过 2 秒的探针和明显高 RTT 异常样本，修正速率为每秒最多 0.05 秒，输出不倒退。`Uncertainty` 包含 RTT 一半和未完成修正量，只是估计，不是非对称网络/时钟漂移的精确误差保证；长时间断流要重新探测。

`SnapshotBuffer<T>` / `snapshot_buffer<T>` 默认最多 32 个表现快照，延迟在 50～200ms 内按到达间隔和抖动调整；参数由业务按体验选定，不是统一要求。拒绝倒序/重复快照，延迟增大时显示时间不倒放，断流保持最新权威值，不无界外推。删除、换 epoch、重生、传送时清空缓冲。它不延迟 EntityViews 的资产、全量/增量、删除回调；显示延迟需与“同步应用 P99≤50ms”分开统计。

### 连接发送预算

```go
cfg.SendBudget = entitysync.SendBudget{
    MaxBytes: 32 << 10, // 每个连接每 Interval 的状态软预算，示意值，需按业务测量
    MaxWait:  250 * time.Millisecond,
    Priority: map[entity.SyncProfile]int{
        {Key: "combat"}: -1,
    },
}
```

零字节值关闭。预算范围是本 Manager 的 session 状态流，不是跨多个 Manager、PB、Lockstep 的全 TCP 带宽上限。Interval 在 periodic 和 on_change 都是同一个连接窗口，即时 Flush 不增加额度；空闲不积攒额度，新 lifetime 重置。选择按完整 subject 估算大小，真正尝试 Push 时按实际帧字节计费；编码失败、取消前未尝试以及失效会话不扣。RetryLater 已尝试则计费，下一窗口恢复，避免重试绕过限流。

每窗口首个完整大对象可超过软值，remove 不受限制，帧头可能造成少量超额。MaxWait 后最早等待者优先，不保证过载下有固定交付期限。`Priority` 仅决定已授权视图的发送顺序，不替代 `ProfilePriorities` 的字段权限选择。冷创建先受原有 SnapshotBudget 的候选计划限制，再与本预算组合，不额外占一条发送通道。被挡更新改为欠当前全量；此策略可能增加全量字节，适合限制低优先级状态，不能期待降低任意负载的总带宽。诊断看 `Counters().SendDeferred` 和 `send_budget_wait`。

### 预测

`PredictionHistory<T>` / `prediction_history<Input>` 管理递增输入序号、ACK、有界历史和纯函数 Replay。容量默认 128，溢出后 Held，直到 ACK 覆盖丢失范围才恢复预测；不能默默对不完整历史继续重放。输入必须是值或不可变对象，引用状态的 clone 必须深复制。ACK 是业务服务器确认的输入序号，不是网络请求 seq 或 Sync tick。

物理 viewer 已改用正式 PredictionHistory，保留原来的 32 条历史、250ms 预测范围、单次 jump、最新输入邮箱及碰撞模拟接口。邮箱合并语义仍由 viewer 按本地时间段重放，不调用通用 Replay 把每条输入误作服务器执行了一帧。不宣称不同 PhysX 版本/平台确定性一致。

viewer 的 SparsePosePlayback 同时接入 SnapshotBuffer，32 个样本、20～200ms 自适应延迟；使用真实物理 tick × 20ms 的样本时间，从最新到达样本推算表现时间，不把它冒充经过 Gate UTC 校准的单向延迟。重复/旧 tick 不重置插值，完整重同步重建 playback；播放头不超过最后可显示样本，服务器长暂停恢复后仍可继续插值。固定 20Hz 到达轨迹加抖动、60Hz 展示的 600 帧验证检查不倒放、不超过最新权威位置和最终进度；这是逻辑验证，不是 Unity 渲染 FPS 压测。

## 本轮验证（2026-11-01，macOS ARM64）

- Go EntitySync/Gate 功能及 race：通过。包含连接隔离、跳过 delta 后最新 Full、重复 on_change、remove、RetryLater、大对象进度、新连接额度、优先级等待和冷预算组合。
- C# SDK 金样及真实回环：通过；新增校时异常样本、单调修正、插值/抖动/容量/停顿、预测 ACK/溢出和诊断环测试。
- `TestClientSDKAgainstGeneratedPlayerTCP`：通过，正式生成工程上的 Gate 校时、PB、Sync、Lockstep。
- Native CTest 7 组通过，包含独立应用线程的 500 次请求和 1ms 心跳顺序回归。此前 Unity 使用的托管/native 双连接回环通过，包含校时请求及重连后回调；本次插值跟进未重跑 Unity Player 构建。
- 物理 viewer 既有 packed body、playback、prediction 测试：通过。
- 全仓 `go build ./...`、受影响包 `go vet`、根包规范门禁：通过。

以上是功能证据，不是新的容量或带宽提升报告；本轮未做生产负载、跨机时钟误差、Linux/移动设备/UE 实跑。native 新导出函数要求库、头文件、托管绑定一起重新构建；不使用旧动态库搭配新客户端。
