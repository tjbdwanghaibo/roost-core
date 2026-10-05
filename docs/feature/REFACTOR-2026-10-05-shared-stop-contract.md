# 共用停机类型与停机契约测试骨架（A3，2026-10-05）

来由：[DECISIONS-PENDING A3](../review/DECISIONS-PENDING-2026-10-05.md)，维护者第二轮“按推荐”（① 现在做；② 下个大版本；③ 只提示不做门禁）。
背景与修复链：[stopshape 方向判断](../bugfix/evidence/noncore-bugfix-20261005-stopshape/README.md)——RR-20261004-07/08、NC-04、NC-09、NC-83、NC-90、NC-170～174，同一不变量（“停止返回 = 回调已静止、资源可释放；重试收敛”）被打破 11 次。

## 当前问题

1. “准入 + 在途计数 + idle 通道”在三处各写一份（`bus/jetstream_rpc.go`、`sync/syncbus/driver/jetstream.go`、`sync/syncbus/mirror/envelope.go`），另有一份更早的 `internal/operation.Lifetime`（nestwal、dataengine Projector 在用，但没有 `Wait(ctx)`）。
2. 每个停止入口的回归各写一遍“首次超时 → 放行 → 重试 → 已释放”，断言口径不一，有的漏掉“未放行时重试”或“再调用返回 nil”。
3. 等待不看 ctx 的写法（裸 `<-done`）没有任何静态提示。

## 目标与改动面

| 项 | 内容 |
| --- | --- |
| 共用类型 | 不新建包：给已有的 `internal/operation.Lifetime`（零依赖、core 层、无环）补 `Wait(ctx)`（= 幂等 Stop + 在 ctx 内等排空，已排空时任何 ctx 都返回 nil）与 `Stopping()`。`Begin`/`End`/`Stop` 语义不变，nestwal / dataengine 不改。 |
| 迁移 | 三份副本改用 `operation.Lifetime`：bus `jetStreamRPC.handlers`、syncbus `jetStreamSyncBus.deliveries`、mirror 每次 Start 一个 `*operation.Lifetime`。错误值（`errJetStreamRPCStopping`、`errJetStreamSyncStopping`）、停止顺序、指标都不变。 |
| 契约骨架 | 新 `internal/stopcontract`（只给测试用，导入 `testing`，在 internal 下不成为公开 API）：`Check(t, Hooks{Start, Block, Stop, Release, Released})` 与 `CallerReleases`。 |
| 套用 | manager Engine（NC-170）、kit/nest Mod（NC-171）、syncbus JetStream（NC-172）、etcd Discovery 与 Assembly.Close（NC-173）、mirror Replicator 与 remoteentity Assembly（NC-174）、bus JetStream RPC（NC-90）、生成 TCP 的 Server / Mod（NC-83，codegen 测试里注入骨架源码运行）。 |
| glsvet | `-stophints`（默认开）：带 ctx 的停止类函数里不受 ctx 约束的通道接收（含跟进一层同包 helper）打印 `hint:`，不计入违例、不改退出码。Mutex.Lock 不提示（理由见下）。 |
| 规范 | roost-coding 生命周期一节：新的停机对象优先用共用类型并套契约骨架；删去“不为此抽公共框架类型”。 |

目录树：新增 `internal/stopcontract/`、`cmd/glsvet/stophints.go`；其余原包保留，无移动。依赖方向：`bus`、`sync/syncbus/driver`、`sync/syncbus/mirror` 新增 import `internal/operation`（core → core internal，`TestCoreDependencyBoundary` 通过）。

## 兼容

- 公开 API、wire、持久格式、生成形状均不变（生成 TCP 只在 codegen 测试的夹具副本里加测试文件）。
- 行为差异只有一处、方向更严格：三处等待在“已排空且 ctx 也已结束”时，旧写法 `select` 在两个就绪分支间随机，可能返回 ctx 错误；`Lifetime.Wait` 先查排空，如实返回 nil。
- 契约骨架在 etcd `Assembly.Close` 上发现第 4 步失败（已停完再调用返回 `context canceled`：clientv3 `Close` 不幂等）。作为 NC-173 的复核补修：`driver.Client.Close` 只关一次、之后返回第一次的结果。

## 快池不阻塞与三大块

- Nest：只加了 `kit/nest` 的契约测试，没有改代码；契约里的等待都在测试 goroutine / 停机路径，不进入快 worker。
- Sync：`sync/syncbus/driver`、`mirror` 的改动是同语义替换（准入 / 计数 / 等待换成 Lifetime），等待只在停止入口（App 停机 goroutine）里发生；消息回调里只有 `Begin`/`End`（一次短临界区，和原来的 `deliveryMu` 相同），不新增等待，快池契约不受影响。
- DataEngine：未改（nestwal / Projector 继续用原有 `Begin/End/Stop`）。

## glsvet 为什么不提示 Mutex.Lock

同一规则加上无参 `Lock()` / `RLock()` 后，全仓非测试文件 33 处提示，逐一看过全部是读写几个字段的短临界区（bus、manager、entitysync、saga、worker…），没有一处会被在途工作长期持有；没有类型与持有时长信息的语法检查分不出两者，误报约 100%。锁被长期持有导致停机越过预算的风险由契约骨架第 1 步（`Stop ignored its context`）在行为上覆盖。通道接收的提示在当前全仓非测试文件上 0 处，在 NC-173 修前的 `discovery.go` 上命中（`Deregister(ctx) calls waitLoopDone`）。

## 验证

见 [证据目录](../bugfix/evidence/a3-stop-contract-20261005/README.md)。要点：原有 NC-90 / NC-172 / NC-174 回归在迁移后 `-race -count=3` 通过；骨架对故意写错的对象红；骨架套到 NC-170 / 171 / 173 / 174 / NC-90 的修前实现、生成 TCP 的 NC-83 修前形状上都红，在当前实现上绿。

## 实施状态

已实施（提交见 DECISIONS-PENDING A3 行）。未做：A3 ② 排空下沉到 `ISyncBus` 带 ctx 的退订（下个大版本，需兼容期）；`worker.Pool` 自带等价机制（`StopWithContext`），未改用 Lifetime。
