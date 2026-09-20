# 实现学习：增量传输、租约 fencing 与持久化类型边界

基线：Core `8c589a6`、Kit `19fb010`、Codegen `9bbac81`。本文记录 RR-20260920-01..05 的机制与实施顺序；建议尚未实现。

## 增量只有在基线可证明时才能丢

状态同步的三个时刻不能混为一谈：

```text
prepared：字段被捕获，但 dirty 仍可恢复
admitted：下游队列接受了字节
delivered/acked：接收端已经拥有构造下一增量所依赖的基线
```

当前 room 链在 admitted 后提交 dirty，AsyncTransport 却允许在 delivered 前覆盖同 stream 帧。latest-only 只适用于“后一个值完整替代前一个值”的消息；dirty delta 不满足这个代数。`SessionSequence` 能描述缺口，但没有消费者据此恢复，因此它目前只是诊断字段。

### 可实施的两阶段方案

第一阶段先保证正确性：room 的增量也进入 reliable queue，复用现有 backpressure 与 slow-consumer eviction。这样性能上限清晰，修复面小，适合先让 16/32 客户端门稳定。

第二阶段再恢复低延迟 datagram：

1. 为每个 session/room 保存 ACK baseline；
2. 后续 delta 始终相对已 ACK baseline 构造，而不是相对“已入队”构造；
3. 队列 coalesce 时合并语义状态，不能拼接或覆盖已编码碎片；
4. sequence gap、重组过期或历史不足时走可靠全量；
5. ACK 与断线 generation 绑定，旧 session 的 ACK 不能推进新 session 基线。

Core 已有 `statesync` 的 frame、fragment、reassembler 与 ACK baseline 思路，优先复用；room 不应再发明独立的半套确认协议。

## 租约证明的是 token，不是 SID

SID 标识路由位置，不能标识进程实例。同一 SID 的旧进程、新进程和重启进程必须用不同 incarnation token。Redis value 至少包含：

```text
owner = { sid, token }
```

以下操作必须是单条 Lua/事务语义：

- claim：key 不存在时写 `{sid,token}` + TTL；
- refresh：仅 value 完整等于 `{sid,token}` 时续期；
- release：仅 value 完整等于 `{sid,token}` 时删除；
- inspect：返回 owner 与剩余 TTL，便于本地 fail-closed 和运维诊断。

Core `redis.ScriptRunner` 已经足够，不需要增加新的 Redis 抽象。不能用 `GET` 后再 `EXPIRE/DEL`，也不能只比较 SID。

## 原子 Redis 租约仍不是写 fence

租约过期后，Redis 可以正确地把 owner 交给 B；真正危险的是 A 不知道自己已经失主。PlayerOwners 需要把刷新结果提升为本地写准入状态：

```text
healthy → suspect → fenced → drained/unloaded
```

- `healthy`：最近一次 refresh 成功，`validUntil` 充足；
- `suspect`：Redis 暂时不可用，但仍在安全窗口内，可配置是否继续接收短事务；
- `fenced`：无法证明租约有效，拒绝新 handler/consumer work；
- `drained`：在途事务结束，实体卸载或进程停服，允许新 owner 独占。

`validUntil` 应从 Redis 确认与本地单调时钟推导，不能在失败时自行延长。若持久层能携带 owner token/fence，则 CAS 再做最终防线；否则本地必须在 Lease 前留出足够 drain 时间。

## Go uint64 不是 BSON 的无符号整数

BSON integer 只有 int32/int64。字段编码要按用途分开：

| 字段 | 需要数值排序/比较 | 建议 |
| --- | --- | --- |
| snapshot checksum | 否 | 固定 8 字节 binary；兼容读取历史 int64 |
| state/base version | 是 | 明确限制到 MaxInt64 并在 durable admission 前拒绝，或采用可排序的扩展编码 |
| marker/route epoch、lock fence | 是 | 与 Redis 计数域统一；当前若来自有符号 INCR，应把上限写进契约 |

迁移时应双读单写：先支持旧 numeric 与新编码读取，再切新写，最后离线归一。不能只在 `applyCommit` 临时 cast，因为 filter/update 的比较语义和历史文档类型也会一起变化。

## Poison 记录需要隔离，不应静默删除

支付 pending 与 WAL 都可能遇到“每次重试都确定失败”的记录。通用处理结构是：

1. 正常队列保留有界重试；
2. 超过确定性阈值或确认 codec/schema 错误后，原子移入 quarantine；
3. quarantine 保存原 key、错误、版本、首次/最近时间和来源队列；
4. 告警和管理面允许修复后重新入队；
5. 权威资产/WAL 不能自动丢弃，只能停止影响健康项。

platform 的 ghost entry 没有权威记录，安全退休；corrupt order 仍有记录，语义不同。Remote WAL 也不能为了启动成功而跳过提交，必须提供明确的 operator decision。

## 实施顺序与验收

1. 先把 room delta 切 reliable，补 `pos_x → equipment` 被阻塞队列夹住的红测，实跑 16/32 客户端。
2. playerroute value 加 token，三个 compare 操作改 Lua；补同 SID 重启和固定 ABA 测试。
3. PlayerOwners 增加刷新结果、validUntil 与 fenced 状态，接入 handler/consumer 准入和卸载 drain。
4. checksum 改 binary，版本/fence 做前置域校验和兼容读；用真实 Mongo 跑事务与 WAL 重启。
5. platform 增加 poison index 与 repair API；用真实 Redis 构造 1、128、129 个坏项验证健康订单仍前进。
6. 第二阶段才做 ACK baseline 的 datagram coalescing和性能压测。

每一步都应保留故障注入：回复丢失、进程暂停、队列背压、重启和混合版本。只跑包内 happy-path 不能验证这些交接不变量。
