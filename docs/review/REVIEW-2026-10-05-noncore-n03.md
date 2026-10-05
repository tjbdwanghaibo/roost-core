# N03 bus / nats / servicerpc / etcd：真实 JetStream、ServiceRPC 发现与期限、etcd 选主与恢复

2026-10-05，分支 `revn03`，基线 `be7bcc18`（N03 源码与 `7922e428` 相同；`b67d5945` 的 etcd Discovery 租约过期注销修复已包含并直接复用）。范围按[接力清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md) N03 行：真实 JetStream 发布 / ACK / Term / 重投 / 重连、满队列 fallback、在途关闭与再次 Stop；ServiceRPC 服务发现与请求剩余期限；etcd 选主正常 Resign 预算、服务端资源清理、watch / lease 恢复。[证据](evidence/noncore-review-20261005-n03/README.md)。

## 开工核对

- W-2026-10-04-02 已登记并修复为 [RR-20261004-08](../bug/RR-20261004-08.md)（`e0591f79`），其前序 [RR-20261004-07](../bug/RR-20261004-07.md)（`e5aea173`）、NC-08～10（`3560a19b`）、NC-11/12（`1502f973`）、[RR-20261004-06](../bug/RR-20261004-06.md)（`c57b247b`）都在基线里。本轮不重复登记它们；相关路径只作控制验证。
- 图谱：项目 `Users-whb-roost-roost-core` ready，但共享 generation 停在 2026-09-30，N03 全部文件之后都改过（10-04 / 10-05 修复）。`check_index_coverage` 对 bus / nats / servicerpc / etcd / kit/nats / kit/etcd 无记录缺口，仍以当前源码逐文件阅读为准，结论不依赖图谱边。
- 前提（委托方已确认）：`svc.<type>.<sid>` 普通主题与 JetStream 按实例 durable `rpc_<type>_<sid>_<method>`、响应 durable `rpc_resp_<type>_<sid>` 都假设一个 sid 只有一个进程，由 App 单实例锁保证；本轮不把“同 sid 双进程”当缺陷。

## 已核对项

| 项 | 源码 / 所有权链 | 实际结果 | 证据 |
| --- | --- | --- | --- |
| JS 发布→ACK | `callJetStreamRPC` 发布（MsgID=reqID）→ 服务 durable 消费 → handler → `publishJetStreamRPCResponse` 成功才返回 nil → `settleJetStreamDelivery` Ack（`bus/jetstream_rpc.go:264-325,372-462`；`nats/driver/jetstream.go:92-114`） | 业务错误回 envelope、单次 ACK（delivered=1 ackfloor=1 redelivered=0） | P2 |
| JS Term | 解码失败返回 error → NAK → `terminalReason` 到 MaxDeliver 后 Term | 5 次投递后 Term，ack_pending=0 | P3 |
| JS 回包失败重投 | 回包 publish 失败 → error → NAK → 重投 | handler 被执行 5 次后 Term（观察 O2） | P5 |
| AckWait < handler 耗时 | 调用方期限 > AckWait 时 broker 在 handler 未完成时重投 | 执行 2 次，调用方成功（观察 O1） | P6 |
| broker 重启 / 重连 | nats.go Consume 在 CONNECTED 后重新拉取；durable 在文件存储上保留 | 自起 nats-server 重启：中断期调用 701ms 按期限返回，恢复约 1.1s；中断期发布的请求被服务端按 DeadlineAt 丢弃（观察 O4） | P7 |
| **在途 handler 时停止** | Bus 停止只排空 pool；JS handler 在 consume 回调 goroutine，`sub.Stop()` 不等它；回包 ctx 随 Bus 停止取消 | **Stop 50ms 返回 nil，handler 仍在运行；回包 / settle 失败，调用方超时 → [NC-90](../bug/RR-20261005-NC-90.md)** | P1 |
| **满队列 fallback（Bus）** | `dispatchTask` 对 RPC 任务不回包，写死信 | **调用方等满 2s 得到 request timeout；RPC 请求进死信、重放成普通消息 → [NC-91](../bug/RR-20261005-NC-91.md)** | P9、bus 探针 |
| 满队列 fallback（RPC callback 池） | `rpcTask.OnRelease` 同步完成、停止任务持有责任（NC-09 / RR-07） | 复用既有回归 `TestRPCBudgetStopRetainsFullQueueFallbackAndAllowsRetry`，本轮未改 | 包测 |
| 再次 Stop | Bus `awaitStop` / RPCClient `stopDone` / NatsMod 保留规则（RR-07/08） | 复用既有回归与 `TestRealNatsModStopAfterConnectionDrainBudgetConverges`；JS 在途部分随 NC-90 补 | 包测 |
| **轻量调用 × JetStream 部署** | 请求流收 `<prefix>.rpc.>`，core request 得到 PubAck；JS 消费者执行无期限、无 reply 的请求 | **调用方 `unsupported rpc response version 0`，handler 实际执行 → [NC-92](../bug/RR-20261005-NC-92.md)** | P11、P10 |
| ServiceRPC 发现 + 剩余期限 | `CallDiscoveredChecked` 一个 child deadline 覆盖发现 / 选择 / 传输（NC-10）；JS 路径 `DeadlineAt` = 该期限 | 真实 etcd 注册 + 真实 NATS：handler 看到剩余 796ms / 800ms；轻量路径不传期限（观察 O8） | P10 |
| **etcd 正常 Resign 预算** | `election.Resign` → SDK `Session.Close` 用 session ctx + TTL 发 Revoke | **冻结 etcd 时 Resign(500ms) 阻塞 >20s → [NC-93](../bug/RR-20261005-NC-93.md)** | E1 |
| 正常 Resign 服务端清理 | `elect.Resign` 删领导键，`Session.Close` Revoke | lease TTL=-1、键 0、另一候选立即当选 | E1b |
| Deregister 预算 | `Discovery.Deregister` → `revoke(ctx)` | 冻结时 500ms 返回 ctx 错误，解冻后重试成功、键清零 | E2 |
| lease 恢复 | keepalive 结束 → `registrationLoop` 退避重注册（`etcd/driver/discovery.go:157-189`） | 带外撤销 lease 后 1.2s 以新 lease 重新注册 | E4 |
| watch 恢复（Mirror） | `localMirror.run` watch 失败 → 退避 → `reload` 快照 → 从 revision+1 重新 watch | 断网期间写入 + 压缩，恢复后快照正确（观察 O6：断网期间 Synced 仍为 true） | E5 |
| Assembly.Close 两次 | Deregister 已清登记 → 第二次只剩 clientv3 Close | 第二次返回 `context canceled`（观察 O5） | E3 |

## 观察（未登记 RR）

- **O1** JetStream 请求的 `DeadlineAt` 取调用方期限，可以长于 AckWait（默认 10s）；handler 超过 AckWait 时被重投并重复执行（P6：2 次）。默认 CallTimeout 5s < AckWait 时重投会被过期检查丢弃，所以只影响显式长期限的调用方。可选：发布时把期限截到 AckWait，或在 handler 运行期间发 `InProgress`。
- **O2** 回包流不可用时同一请求最多执行 MaxDeliver 次（P5）。这是已写明的至少一次语义，handler 需要按 MsgID 幂等。
- **O3** 解码失败走 NAK 重试到 MaxDeliver（core 路径直接回拒绝），nc-audit-2 S5 已记，本轮实测 5 次后 Term。
- **O4** broker 中断时调用方得到 publish 错误，但请求可能已进流；服务端靠 `DeadlineAt` 丢弃过期请求，所以“发布超时”实际等价于“不会执行”，前提是两端时钟偏差小于剩余期限。
- **O5** `Assembly.Close` 第二次调用返回 `context canceled`；第一次 Deregister 失败后 client 已关闭，重试永远失败（lease 由 TTL 回收）。App 不重试 Mod 停止，仓内没有触发路径，不登记；若以后加入停机重试，应仿 RR-20261004-08 让终态可识别。
- **O6** LocalMirror 的 `Synced` 表示 watch 已建立，不表示连接可达：断网期间仍为 true。watch 未用 `WithRequireLeader`，多节点分区时可能静默停滞（单节点无法验证）。
- **O7** `WatchService` 的 serviceWatcher 遇压缩 / 错误即结束并给出 `Err`，不自动重建；仓内无消费者，消费者须自行重新 Watch。
- **O8** 轻量 RPC 信封不带期限，handler 的 ctx 只有 Bus 生命周期；只有 JetStream 路径传递剩余期限。
- **O9** Bus 停止后的 `CallReliable` 在 pending 表关闭之后登记，等满 CallTimeout 才返回，没有快速失败。
- **O10** 按实例 durable `rpc_<type>_<sid>_<method>` 与 `rpc_resp_<type>_<sid>` 在进程退出后保留在 broker；sid 频繁变化时会累积（消息受 StreamMaxAge 约束，consumer 不会被删）。
- **O11** Consume 没有设 ErrHandler：服务端删除 consumer 后消费静默停止，健康检查仍报 connected（未实测）。

## 外部 / 未验证

- 多节点 NATS 集群的节点故障 / 分区下的重投与 durable 迁移；本轮重连只在自起单节点上做。
- 多节点 etcd 的 leader 切换、分区、`WithRequireLeader`；Resign 的 Delete 结果未知。
- 长时间容量（pending、durable 累积、DLQ 增长）。

## 方向判断

**NATS / Bus 停止与排空**：近两天同一不变量（“停止返回 = 回调已静止、资源可释放；重试收敛”）连续被打破——[RR-20261004-07](../bug/RR-20261004-07.md)（Bus pool 超预算后重试永远失败）、[NC-09](../bugfix/RR-20261004-NC-09.md)（RPC 回调停止预算）、[RR-20261004-08](../bug/RR-20261004-08.md)（连接 drain 后重试永不收敛），本轮 NC-90（JS handler 根本不在排空范围内）。修复一直在加状态（`stopDone` / `teardownDone` / `drainPool` / `ErrClosedUndrained`）。根因是实现方向：Bus 有四个执行入口（worker pool、core 订阅回调、JetStream consume 回调、RPC callback 池），各自的生命周期由不同层零散地停。建议方向：**所有 Bus 拥有的回调入口经同一个“准入 + 在途计数”门，停止只有一个状态机**（先关准入、再按序停订阅、再等在途归零、最后交还连接）；或者把 JetStream handler 也投进 pool，使 pool 成为唯一排空点（代价：JetStream 的顺序 / 背压语义要改成“池满即 NAK”）。NC-90 的修复按前者的最小形态实现（JS 请求入口的准入 / 在途门并入现有停止），没有做全面重构。

**etcd 选主**：NC-11 → RR-20261004-06（NC-11 的回退）→ NC-12 → `b67d5945` → 本轮 NC-93，五个问题都在“取消 / 撤销 lease 的所有权与预算”上。`71c6fb6b` 之后 kit 已不再发布选举工厂，进程级单例由 App 单实例锁承担，选举代码在仓内没有生产调用方。建议维护者考虑：**弃用或移出 core 的选举 API**（保留 Discovery / Mirror），而不是继续为无人使用的路径打补丁；若保留，正常 Resign 与失败放弃应只走同一条撤销路径（NC-93 的修复即合并到 RR-06 的 `abandon`，减少一个分支）。
