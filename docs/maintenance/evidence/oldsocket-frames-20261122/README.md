# 被接替的旧连接仍收帧：实网确认（2026-11-22）

**结论：已确认。** 同一账号的新连接接替进图后，仍连着的旧连接继续收到 self_sync 帧，它的视图跟着新连接的移动走。

## 怎么测的

- 跑的是 `scripts/test-cellspace.py`（cellworld 配方工程，真实 Base + 两个 Cell + account，私有 Redis / NATS / Mongo / etcd，真实 TCP），core 版本为 v1.60.0（`c82d6655`）。
- 只在临时副本里给 `scripts/testdata/cellspace/reconnect_test.go.txt` 加了观察，没有改仓库。插入位置在 `third.move(...)` 答 `ErrCellspaceStaleSession`、再进场被拒之后：先等 `latest` 再向前走 3 格，再从旧连接 `third` 读包，最多读 3s，看它的视图是否也到了这个位置。

## 输出（原文）

```
cellspace_live_reconnect_test.go:196: OLDSOCKET after takeover: old y=489 latest start=492 latest now=489
cellspace_live_reconnect_test.go:217: OLDSOCKET result: old socket followed new owner to y=486 after 3 self-sync packets
PASS unique=100001, Base=102401029, CellPlayer=102401037; owner SID 1002 -> recovered 1002 -> 1001; entry 1 -> 2 (restart) -> 2 (reconnect) -> 3 (offline past 8s)
```

## 后半句的状态

KNOWN-LIMITS 里的后半句是：旧连接拒收推送时，manager 可能把当前成员的新连接关掉。这一条**只读代码确认了路径，没有复现**，复现需要注入驻留预算耗尽。路径分三步：

1. game-demo 场景的 `sceneLane.Push` 经 `selfsync.PushPlayerFrame` 按玩家推送。
2. `TCPServer.pushPlayerKind`（`infra/network/gateway/tcp.go`）发给该玩家的所有连接。只要有一条拒收（不是连接已断），即使别的连接已送达，也返回错误。
3. 会话丢失后，`Scene.lostPartitionSession` 关的是 `m.socket`，也就是当前成员的连接。
