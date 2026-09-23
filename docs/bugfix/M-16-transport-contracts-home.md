# M-16：ARCH-12 S2——会话与传输契约从 statesync 归位到 nettransport

- 单元：M-16（结构整理，不占 U 编号，不计缺陷）· 承接 [ARCH-12](ARCH-12-sync-package-layout.md) §4 S2，前置 [M-15](M-15-statesync-dead-code.md)
- 仓库：roost-core `statesync`、`nettransport`、`entitysync`、`lockstep`、`demo` 模板
- 分支：`feature/arch-10-sync-policy`

## 为什么

S1 之后 `nettransport` 的 7 个源文件仍全部 import `statesync`，拿的是 `SessionID / SessionInfo`、`Transport / SessionTransport / DatagramBatchTransport`、42 字节分片头——这些是**传输**的契约，却住在帧格式包里，于是"传输依赖帧格式"。`lockstep` 只为一个 `SessionID` 也拖进整个 `statesync`。

## 搬了什么

| 从 `statesync` | 到 `nettransport` | 变化 |
| --- | --- | --- |
| `SessionID`、`SessionInfo` | `session.go` | 原样 |
| `Transport`、`DatagramBatchTransport`、`SessionTransport`、`TransportFunc`、`ErrTransportMissing` | `transport.go` | 原样，注释改为传输视角 |
| `datagram.go`（分片头、`InspectDatagram`、`FragmentFrame`） | `datagram.go` | **与帧无关**：`FragmentFrame(frame DeltaFrame, sequence, encoded, maxDatagram, limits)` 改为 `FragmentDatagrams(meta DatagramMeta, encoded, maxDatagram, maxFragments)`；`InspectDatagram(packet, limits)` 改为 `InspectDatagram(packet, maxDatagram, maxFragments)`；`DefaultMaxDatagram` / 新增 `DefaultMaxFragments`；三个 datagram 错误随行 |

`statesync.Limits` 去掉传输侧的 `MaxDatagramBytes / MaxFragments`（`entitysync.ManagerConfig.Limits` 的归一化跟随）。分片头的线格式一个字节都没变（magic、42 字节布局、CRC 都在原地），只是它的 owner 变成了传输。

## 之后的依赖箭头

```
nettransport  → （core 内无依赖）
statesync     → （core 内无依赖）
lockstep      → nettransport
entitysync    → statesync, nettransport, entity
policy        → entitysync, entity, spatial
```

帧格式与传输互不认识：这是 S3 把它们分别搬成 `sync/frame` 与 `sync/nettransport` 的前提。

## 调用方改动

- `entitysync.SessionID` 现在是 `nettransport.SessionID` 的别名（此前是 `statesync.SessionID`）
- `lockstep.Room` 的会话类型改为 `nettransport.SessionID`
- demo 模板 `battle.go.tmpl` 的 `battleSender` 跟随（`corestate.SessionID` → `nettransport.SessionID`）；`loadtest` 与 `scene_test` 仍从 `statesync` 取 `DefaultLimits / ObjectRemove`

## 验证

- `go list` 确认 `nettransport`、`statesync` 在 core 内零依赖；`lockstep` 只依赖 `nettransport` 与 `metrics`
- `go test` statesync、nettransport、entitysync（含 policy）、lockstep、robot、codegen/internal/roost 全绿；帧编解码测试未动，说明 `EncodeFrame` 字节不变
