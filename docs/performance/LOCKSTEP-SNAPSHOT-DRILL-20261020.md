# Lockstep 快照恢复限速链路演练（2026-10-20）

快照恢复第 4 期（[设计 §9](../maintenance/FEATURE-2026-10-20-lockstep-snapshot-recovery.md#9-测试与验收计划)“限速链路”一行）的一次性**功能演练**，不是性能基线：只核对“快照在慢链路上能不能传完、连接断不断、恢复是否成功”，数字只对本机、本次参数成立。两组：

1. **嵌入 TCP**（`TCPSender`，可靠通道等写出，`ReliablePacer` = true）：256 KB/s、64 KB/s 两档，1 MiB 与 4 MiB 两种快照。
2. **独立 Gate**（`gate.Transport`，只准入、写出无回执，快照按 `UnpacedBytesPerTick` 固定节流）：把设计 §6.3 风险 1“慢链路被 Gate 断开、重连后反复重来”落成实测。

框架、Gate 与 Match 的参数全部取缺省，没有为通过调参。代码与原始日志在 [benchmarks/lockstep-snapshot-p4-toxiproxy-20261020](benchmarks/lockstep-snapshot-p4-toxiproxy-20261020/)。

## 1. 环境与做法

- 机器：Apple M5，macOS 26.5.2，Go 1.27.2；`net.inet.tcp.autosndbufmax` 4 MiB、`sendspace` / `recvspace` 128 KiB（[env.txt](benchmarks/lockstep-snapshot-p4-toxiproxy-20261020/env.txt)）。基线 `69fa9e79`（P1～P3 已合入，本期未改框架代码）。
- 限速：共享隔离环境的 toxiproxy 2.12.0，持 `remote-acceptance.lock` 期间新建带 `lsp4-` 前缀的临时代理，只在下行（服务端→客户端）加 `bandwidth` toxic（`rate` 单位 KB/s，1 KB = 1000 B）；用例结束删除代理，演练后核对代理列表与演练前相同（只剩环境原有的 `nats-1～3`、`redis`）。
- 夹具（演练后从源码树移走，原样存为 `.go.txt`）：
  - 嵌入 TCP：[embedded_drill_test.go.txt](benchmarks/lockstep-snapshot-p4-toxiproxy-20261020/embedded_drill_test.go.txt)，放进 `framework/sync/lockstep` 以 `-tags lsdrill` 运行，复用 `tcp_test.go` 的嵌入 TCP 接入（`gateway.TCPServer` + `TCPRuntime`，`write_timeout` 取缺省 5s）。
  - 独立 Gate：[gate_drill_test.go.txt](benchmarks/lockstep-snapshot-p4-toxiproxy-20261020/gate_drill_test.go.txt)，放进 `wiring/gate` 以 `-tags 'integration lsdrill'` 运行，复用 `gate_real_test.go` 的真实 Gate + Game Ingress + 共享隔离环境 NATS（本次唯一 namespace，不用 JetStream）；Gate / Game 配置取 `gateway.DefaultConfig()`（出站每绑定 32 条、2 MiB，`max_age` 250ms）。
- 场景：Match 只配 `Snapshots.Provider`（其余缺省：窗口 3600、钉住 3600、`BytesPerTick` 64 KiB、`UnpacedBytesPerTick` 16 KiB、分块 16 KiB、`CatchupSendTimeout` 2s），先无连接地切 3700 帧，再以 30Hz Tick。客户端经代理登录，从第 1 帧请求追帧（本地状态为空，例如进程重启），起点早已裁掉，auto 走快照。Provider 返回请求上界帧的定长快照（内容可校验）。
- 计时：客户端“传输”= 收到第一块到快照到齐；服务端“恢复结束”= `CatchingUp` 变为 false；“回到头部”= 客户端组装器追到当前帧。

## 2. 嵌入 TCP：两档限速都恢复、连接不断

| 快照 | 限速 | 客户端传输 | 大小 / 速率 | 比值 | 服务端恢复结束 | 客户端回到头部 | 连接 | Tick 错误 | 分块（含重发） |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 MiB | 不限速（对照） | 0.501s | — | — | 0.602s | 0.634s | 不断 | 0 | 64 |
| 1 MiB | 256 KB/s | 4.076s | 4.096s | 1.00 | 0.968s | 4.234s | 不断 | 0 | 64 |
| 1 MiB | 64 KB/s | 16.568s | 16.384s | 1.01 | 3.367s | 17.068s | 不断 | 1 | 65 |
| 4 MiB | 256 KB/s | 16.464s | 16.384s | 1.00 | 13.567s | 16.6s | 不断 | 0 | 256 |
| 4 MiB | 64 KB/s | 68.281s | 65.536s | 1.04 | 39.801s | 69.034s | 不断 | 12 | 265 |

- **传输耗时与链路速度对应**：限速档的比值 1.00～1.04；不限速时 1 MiB 用 0.5s，正好是 64 KiB/Tick × 30Hz 的上限（约 2 MB/s）。
- **连接不断、恢复成功**：五档都由同一次取快照完成（Provider 各调用 1 次），`lockstep.snapshot.failed.total` 不变。
- **Tick 错误是批次超时后重发**：64 KB/s 下一批 64 KiB 要约 1s，碰上 socket 缓冲已满时个别批次超过 `CatchupSendTimeout`（2s），报 `catchup session 1: snapshot chunk at byte 786432: context deadline exceeded`，计一次失败、下一 Tick 从已确认偏移重发（1 MiB 档重发 1 块，4 MiB 档重发 9 块、约 4% 字节）；连续失败没有达到 `CatchupMaxFailures`（8），恢复照常完成。
- **服务端认为恢复结束远早于客户端收完**：`PushLockstepSessionWritten` 的“写出”是写进本机 socket 发送缓冲；本机发送缓冲自动增长上限 4 MiB，加上代理里的缓冲，服务端“写出”与客户端“收到”之间最多隔了约 1.7 MB（4 MiB / 64 KB/s 档：服务端 39.8s 结束时客户端约收到 2.5 MB）。之后服务端把这名玩家切回实时广播，实时帧排在缓冲里的快照字节后面，客户端在链路排空后（69.0s）才回到头部。这是推断（由计时与缓冲上限对得上），没有单独测量缓冲占用。影响：慢链路玩家恢复后的头几秒实时帧延迟等于缓冲排空时间；没有断连、没有丢帧。

## 3. 独立 Gate：§6.3 风险 1 实测成立

缺省 `UnpacedBytesPerTick` 16 KiB × 30Hz 约 492 KB/s。每档最多重连 3 次，每次都是新连接上的空客户端从第 1 帧请求。

| 快照 | 限速 | 结果 | 每次连接 | Provider 调用 | 下发快照字节 |
| --- | --- | --- | --- | --- | --- |
| 1 MiB | 不限速（对照） | 恢复 | 传输 2.102s，2.266s 回到头部 | 1 | 1 MiB |
| 1 MiB | 256 KB/s | 恢复（没有断开） | 传输 4.068s，4.201s 回到头部 | 1 | 1 MiB |
| 1 MiB | 64 KB/s | **三次都被 Gate 断开，未恢复** | 每次约 12.7s 后断开，收到 48/64 块（786,758 B） | 3 | 2,752,512 B |
| 4 MiB | 不限速（对照） | 恢复 | 传输 8.504s，8.866s 回到头部 | 1 | 4 MiB |
| 4 MiB | 256 KB/s | **三次都被 Gate 断开，未恢复** | 每次约 6.4s 后断开，收到 98/256 块（1,606,259 B） | 3 | 5,210,112 B |

- **断开方式**：Gate 日志 `gate closed binding after channel failure ... err="player tcp: connection cannot take the frame: writev ...: i/o timeout"`，计数 `gate.channel.failure_total{phase="socket",role="gate"}` 每次 +1。也就是 Gate 往 socket 写一帧时写到一半、写期限到期，按连接故障关闭（`tcp_session.go` `writeFrame`：期限取 `write_timeout` 与这一帧剩余发送预算中较早者）。设计 §6.3 原写“Gate 侧积压超过 `max_age` 250ms 即关闭”，实测关闭来自写 socket 的期限；具体是 5s `write_timeout` 还是 250ms 的出站预算先到期，这次没有区分（尚未确认）。两种都是“慢链路跟不上就断开”的同一类规则，结论不变。
- **反复重来**：每次重连服务端重新取快照、从头再传（快照不续传：新连接是新会话、新传输编号），在同一位置被断开；三次的分块数逐字节相同。客户端按现行 SDK 规则会一直重连重试，服务端每次多发约 0.8～1.6 MB。
- **低于预算的限速不一定断开**：1 MiB / 256 KB/s 虽然低于 492 KB/s，也传完了。按速率差估计，超出链路的部分约 1 MiB ×（1 − 256/492）≈ 0.5 MB，被 Gate 侧 socket 缓冲与代理缓冲吸收；4 MiB 时超出约 2 MB，写满缓冲后被断开。所以“是否断开”取决于快照大小、预算与链路速度三者，而不只是“链路速度低于预算”（推断，依据上面两档的对比，没有单独测量缓冲）。
- **框架这边看不出失败**：`lockstep.snapshot.failed.total` 三次都不变，`lockstep.snapshot.total{reason="out_of_window"}` 每次重连 +1；Gate 断开后 Game 的 `OnClosed` 调 `DetachSession`，进行中的恢复随会话一起清掉，不计 `failed{reason="abandoned"}`。设计 §6.3 说“框架能做的是计数（`failed{abandoned}` 与同一会话的重复快照次数）”，现状只有后一项能从 `snapshot.total` 间接看出（且不分会话）。是否要在“恢复进行中被 Detach”时计一次失败，交维护者决定（见设计 §14 P4 第 5 条）。

## 4. 复跑

```sh
source ~/.roost-it/roost-dataengine-it/env.sh
mkdir "$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock"   # 取不到就说明别人在用，不要跑
cp docs/performance/benchmarks/lockstep-snapshot-p4-toxiproxy-20261020/embedded_drill_test.go.txt framework/sync/lockstep/snapshot_drill_test.go
cp docs/performance/benchmarks/lockstep-snapshot-p4-toxiproxy-20261020/gate_drill_test.go.txt wiring/gate/snapshot_drill_test.go
LS_DRILL_RATES=0,256,64 GOWORK=off go test -tags lsdrill -count=1 -v -run TestSnapshotDrillEmbeddedTCP ./framework/sync/lockstep
LS_DRILL_SNAPSHOT_BYTES=4194304 LS_DRILL_RATES=256,64 GOWORK=off go test -tags lsdrill -count=1 -v -timeout 10m -run TestSnapshotDrillEmbeddedTCP ./framework/sync/lockstep
LS_DRILL_RATES=0,256,64 GOWORK=off go test -tags 'integration lsdrill' -count=1 -v -run TestSnapshotDrillStandaloneGate ./wiring/gate
LS_DRILL_SNAPSHOT_BYTES=4194304 LS_DRILL_RATES=0,256 GOWORK=off go test -tags 'integration lsdrill' -count=1 -v -timeout 15m -run TestSnapshotDrillStandaloneGate ./wiring/gate
rm framework/sync/lockstep/snapshot_drill_test.go wiring/gate/snapshot_drill_test.go
rmdir "$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock"
```

`LS_DRILL_RATES` 里的 0 表示只建代理、不加限速（对照）。代理名带进程号，用例结束（含失败）由 `t.Cleanup` 删除；中途被杀时用 `curl -X DELETE $ROOST_DATAENGINE_IT_TOXIPROXY_URL/proxies/<名字>` 手动删。
