# M-15：ARCH-12 S1——删掉 statesync 里没有调用者的老 Replicator 一族，修过时注释

- 单元：M-15（结构整理，不占 U 编号，不计缺陷）· 承接 [ARCH-12](ARCH-12-sync-package-layout.md) §4 S1
- 仓库：roost-core `statesync`、`nettransport`、`entitysync`、`lockstep`、`syncstream`、`codegen/internal/roost`
- 分支：`feature/arch-10-sync-policy`
- 路径不变；这是 S2（契约归位）和 S3（搬进 `sync/`）之前的减重

## 删了什么（全部在 core 内零外部引用，含 kit / codegen / skill / robot / demo 模板）

| 文件 | 内容 | 行 |
| --- | --- | --- |
| `statesync/replicator.go` + `replicator_test.go` | 老 `Replicator`（每房间 Replicator、PrepareLatest / Commit / Acknowledge、SetProjector），ARCH-10 由 `entitysync.Manager` 取代 | 617 + 768 |
| `statesync/session.go` | `SessionState`（ACK 基线、pinned view、QualityTier） | 342 |
| `statesync/lod.go` + `lod_test.go` + `lod_phase_promises_test.go` | `LODProjector` / `LODSelector` / `LODMask` | 285 + 289 |
| `statesync/delta.go` | `BuildDelta / ApplyDelta`（快照 diff） | 236 |
| `statesync/shadow.go`、`ring.go`、`schema.go`、`snapshot.go` | `ShadowStore`、`SnapshotRing`、`SchemaRegistry`、`Snapshot.Clone` | 407 |
| `statesync/control.go` + `control_test.go` | 客户端→服务端控制消息（Ack / Resync），唯一处理者是 Replicator | 146 + 71 |
| `statesync/datagram.go` 的 `Reassembler`、`DecodeDatagram`；`reassembly_limit_promises_test.go` | 分片重组。robot 在 M-14 已不再重组（entitysync 走 reliable lane） | ~150 + 63 |
| `statesync/transport.go` 的 `Projector / ContextProjector / ProjectionContext / ProjectorFunc` | Replicator 的投影钩子 | 30 |
| `statesync/frame_limits.go` 的 `Lane / Reliability / Priority / Visibility / CodecType / ReplicationPolicy`、`ComponentState / ObjectState / Snapshot`、`Limits.MaxInflightFrames{,PerSession}`、10 个只被死代码用的 Err | 老策略枚举与全量快照类型 | ~110 |
| `nettransport/control_plane.go` + 对应测试 | 把 UDP 控制包路由到 Replicator 的 `ControlPlane` | 56 + 60 |
| 老 Replicator 的 promise tests：`baseline_identity`、`pinned_view`、`recovery_intent`、`replacement_capacity` | 断言的对象已不存在 | 374 |

合计约 −3.9k 行。`statesync` 剩下：`codec.go`（帧编解码）、`frame_limits.go`（帧类型 + Limits）、`datagram.go`（分片头 + `FragmentFrame` + `InspectDatagram`）、`transport.go`（`Transport / SessionTransport / DatagramBatchTransport / TransportFunc`）。

## 留下但标记的

- **datagram lane 没有消费者。** `AsyncTransport` 的 datagram lane 只被 `FragmentFrame` 产出的分片喂过，接收端的 `Reassembler` 已删；lockstep 明确不走这条 lane。是否连 lane 一起删是传输层的设计题，登记为 [W-2026-09-23-01](../bug/WANTED.md)，不在 S1 动。
- 错误串前缀仍是 `replication:`，S3 改包名为 `frame` 时一并改，只改一次。

## 顺手修的过时注释

- `nettransport` 的包注释原为 "Package replication … for roost-core/replication"，改为说明它是实体同步的网络侧
- `lockstep/room.go` 的 "replication package's senders" → nettransport
- codegen `render_deploy.go` 生成的部署文档里 "replication 生成 snapshot/delta/LOD" 改为四层的说法
- `syncstream/publisher.go` 的第二个包注释（提到 roost-kit sync transports）降为文件注释

## 验证

- `go build ./...`、`go vet` statesync / nettransport / entitysync / lockstep / robot 通过
- `go test` statesync、nettransport、entitysync（含 policy）、lockstep、robot、codegen/internal/roost 全绿
- 脚本核对：`statesync` 剩余导出符号里，除类型本身（`FrameKind` 等只用其常量）与 `FragmentFrame`（见上）外都有外部调用者
