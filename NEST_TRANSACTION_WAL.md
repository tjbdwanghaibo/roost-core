# Nest Transaction、WAL 与 Outbox 生产方案

## 1. 目标

Nest handler 在进入业务代码前已经持有所有目标 entity 的 mutex。本方案在不把锁暴露给业务层的前提下，提供：

- handler 失败或 panic 时的内存状态回滚；
- 多 entity 修改组成一个原子提交记录；
- 返回成功前可选择 WAL 严格刷盘；
- 进程崩溃后的顺序恢复；
- 外部消息的 transactional outbox；
- 主动 `Flush(ctx)`；
- 不在 entity 锁内执行数据库和消息系统 I/O；
- 生成代码的低开销 undo 路径，并拒绝不完整的旧事务标记。

## 2. 职责边界

### 核心运行时

`nest/` 只定义框架语义：事务状态机、rollback/durability 策略、commit record、participant、committer 和 entity 解锁通知。core 不依赖文件系统、Mongo、Redis 或消息中间件。

### 持久化与 Kit 装配

`nestwal/` 提供物理日志原语：分段 WAL、CRC、group commit、fsync、ack checkpoint 与 replay。`dataengine/engine` 在其上提供 Mongo projection、aggregate load/migration、Saga/Remote mutation、effect outbox、健康状态和主动 flush；应用只装配 Data Engine Mod。

### 代码生成

生成的 DAO 提供：

- 无副作用的 `CaptureRollbackState` / `RestoreRollbackState`；
- setter 级 inverse undo；
- map 按 key 去重的 undo；
- transaction-local `PersistChange` 与字段级 patch；
- `MutationParticipant.PrepareMutation` / `AcceptMutation`。

业务层仍然只调用生成的 component/entity API，不处理锁、WAL 和 rollback。

## 3. handler 策略

```go
//nest:rollback undo
//nest:durability strict
func Transfer(...) error
```

Rollback：

- `none`：无事务开销，只适合不修改状态的 handler；
- `state`：handler 前抓取完整、无副作用 snapshot；
- `undo`：生成 setter 第一次修改字段时记录 inverse operation，推荐热路径。

Durability：

- `memory`：不写 commit WAL，因此禁止修改 persistent 字段（运行期强制，RR-20261006-41：`rollback=none` 时持久 setter panic；`rollback=state|undo` 或带 Remote 批次时，提交点发现本地持久改动 / `AddMutation` / receipt 即以 `ErrMemoryTransactionPersistentWrite` + `ErrCommitRejected` 整笔拒绝并按回滚策略撤销内存修改）；
- `async`：等待 WAL write，不等待本批 fsync；后台按间隔刷盘；
- `strict`：等待所在 group commit 完成 fsync 后才返回成功。

durability 非 `memory` 时必须配置 rollback。调用 `nest.Emit` 会自动把 memory 事务提升为 strict，防止出现名义上的 outbox 实际可能丢失。

## 4. 提交时序

1. Nest 按既有顺序持有涉及的 entity mutex。
2. 建立 `RollbackTx`，抓取 tracker snapshot 或注册 undo。
3. 执行 handler。setter 修改状态并向当前 transaction 登记 `PersistChange`。
4. handler 失败：逆序执行 undo/snapshot restore，恢复 tracker/version。
5. handler 成功：DAO participant 生成 Put/Patch/Delete mutation。
6. committer 将整个多 entity record 追加到 WAL。
7. strict 在 group fsync 成功后形成 commit point。
8. Nest 提交内存事务，entity release hook 只处理 sync/lifecycle 并释放锁。
9. 解锁后通知 Data Engine projector，replay 才允许落库；effect 先 staging 到 Mongo outbox。
10. mutations 与 effects 全部成功后，持久化 ack checkpoint。

同一进程内的“暂缓到解锁后 replay”避免 projector 在事务仍持有 Entity 锁时抢占存储工作。若进程在第 7 步后、第 9 步前崩溃，新进程没有暂缓表，会直接从 WAL 恢复，符合 commit point 语义。

### 动态 Cast 的事务边界

handler 通过 `CastOne` / `CastMulti` 新取得的本地实体也参与当前事务的回滚与持久化准备，无需启用客户端 Sync。事务内调用 `ReleaseCast` 不会立即解锁；框架必须持锁完成回滚或成功准入，再统一释放。无事务、无正式 Sync 作用域时仍可提前释放；提前释放按实例，只放传入实例自己的锁——handler 内 Destroy 后又新建了同 ID 的实体时，`ReleaseCast(旧实例)` 不会放掉新实例的锁（[RR-20260927-26](docs/bugfix/RR-20260927-26.md)）。

pipelined 在准入后为动态实体写入 CommitLSN，并在等待 WAL 前释放其锁。若业务必须先放锁再执行另一项操作，应拆成独立业务调用，不能依赖 `ReleaseCast` 在事务中途放锁。[修复与验证](docs/bugfix/RR-20260924-03.md)。

## 5. WAL 格式与恢复

- 单 writer goroutine，调用方可并发 append；
- segment 默认 256 MiB；
- record 默认上限 16 MiB；
- frame 包含 magic、格式版本、payload 长度、header CRC、payload CRC；
- payload 为确定性二进制编码，header map 按 key 排序；
- strict 请求在 batch 中合并为一次 fsync；经阻塞式 `Append` 提交的 pipelined 记录（broadcast、带 Remote 批次时回退到 strict 路径）同样等这次 fsync（[RR-20260928-11](docs/bugfix/RR-20260928-11.md)）；
- async 默认每 10 ms 刷盘；
- 启动时只截断最后 segment 的不完整尾 frame 或零填充尾部，判据见下文“尾部截断判据”；
- 完整 frame CRC 错误、segment 缺口、ack 越界均拒绝启动，不静默跳过；
- writer 目录使用 OS 文件锁，禁止双进程同时写；
- ack 使用双槽 checkpoint，更新一个槽时另一个槽保持可恢复；
- segment 创建、轮转、删除和 checkpoint 原子替换同时持久化目录元数据；Windows checkpoint 使用 write-through replace；
- replay 严格按 append 顺序；ack 后清理过老且已确认的 segment。
- `max_disk_bytes` 与 `max_unacked_age` 进入健康门禁，超过恢复窗口立即摘除实例并告警。

### 尾部截断判据（RR-20260926-41）

断电、内核崩溃或 VM 快照后，最后一段尾部可能是“文件长度已持久、数据块未写回”的零填充（ext4 `data=writeback`、部分网络盘 / 云盘）。
“坏点”指扫描遇到的第一个无法解析的帧起点（此前各帧均完整且 CRC 正确）。以下**同时满足**才截断坏点之后的全部内容，否则 `ErrCorrupt` 拒绝启动，且拒绝前不改动文件：

1. 仅最后一段。更早的段在轮转时已 fsync，其中任何损坏（含零尾）在回放时报 `ErrCorrupt`。
2. 坏点不早于 checkpoint fence。fence 是最后一个已确认帧的结束偏移，坏点 ≥ fence 表示被截掉的内容从未被确认；坏点落在已确认前缀内即拒绝。
3. 坏点形态二选一：
   - 不完整尾帧：剩余不足 20 字节帧头，或帧头声明的帧体越过文件末尾（既有规则）；
   - 零填充：自坏点至文件末尾全为 0，即坏帧头所在 4 KiB 对齐块的剩余部分及其后所有块全零。
     帧头有效但 CRC 不符、非零垃圾、零区之后又出现非零字节（包括中间零页之后仍有完整帧）都不属于此类。
4. 截断后记 `slog.Warn`（段、坏点偏移、截断字节数、所在块偏移、checkpoint），并累加
   `nestwal.recovery.tail_truncated.total` / `nestwal.recovery.tail_truncated.bytes`，标签只有 `reason=torn_frame|zero_fill`。

残余风险：已 fsync、strict 已向调用方确认但尚未 ack 投影的记录，若介质把它所在的块连同其后全部内容整块清零，形态与未写回的零尾无法区分，会被当作撕裂截断，
该记录丢失且没有 `ErrCorrupt`（checkpoint 之前的已确认记录不受影响）。截断告警与指标是发现这种情况的唯一线索，生产应对 `reason=zero_fill` 告警并核对投影结果。

判据内仍拒绝启动的形态（跨页半写回，OPEN-ITEMS B02 / C08，维护者 2026-09-27 决定不放宽）：最后一帧跨 4 KiB 页边界，前页（含帧头）已写回、后页未写回读出为 0，文件长度不变。
坏点是这一帧的起点，帧头有效、帧体长度在文件之内，只有 payload CRC 不符（`invalid frame payload checksum`）；坏点之后并非全 0，所以不属于零填充尾，
即使这一帧在 checkpoint fence 之后、从未被确认，也按“帧头有效但 CRC 不符”拒绝启动且不改动文件。WAL 无法区分“后页丢写回”与真实的 payload 损坏，
放宽会吞掉真实的 CRC 错误。这种拒绝与“零页之后仍有非零字节”一样需要人工处置；错误文本只有 `invalid frame payload checksum`，不带段号与偏移，坏帧在最后一段，起点是其中最后一个完整帧的结束偏移。
回归：`nestwal/torn_page_tail_promises_test.go`。

## 6. 一致性和幂等

WAL 是 at-least-once replay。以下情况会重复执行：mutation 已落库但 ack 尚未刷盘；effect 已发布但 ack 尚未刷盘；ack 文件更新失败。

因此：

- mutation applier 必须按 `(entity, version)` 做 CAS；仅已存在的版本恰好等于 Next 且 `_last_tx` 等于本事务时视为成功；更高版本不是幂等证明，按冲突失败；
- `MutationApplier.ApplyMutations` 一次接收整个多 entity transaction；需要对外可见的跨实体原子性时，实现必须使用数据库原生 transaction；
- publisher 使用 `Effect.ID` 作为 JetStream MsgID；这只是去重优化。Data Engine 先在业务 mutation 的 Mongo transaction 中 staging outbox；消费端仍需持久 receipt，TTL 必须大于 broker 最大保留/重投窗口；
- 不允许把不可幂等的外部调用放入 `AfterCommit`；应改为 `nest.Emit`；
- Mongo Store 使用 transaction receipt 与 expected/next version CAS 吸收重复 replay；版本冲突只有在已投影完全相同事务时才是幂等成功。

同一 `CommitRecord` 的普通 mutation、Remote commit、Saga receipt 与 effect staging 按需进入一个 Mongo transaction，读侧不会看到跨文档中间态。生成 DAO 的 WAL version 使用 `dataengine.Tracker.Version()+1`，投影确认后由 `AcceptMutation` 推进。

## 7. fsync 不确定结果

设备在 write/fsync 报错时可能无法证明数据究竟是否持久化。WAL 返回 `nest.ErrCommitIndeterminate` 并进入 terminal 状态：

- core 不执行内存 rollback，避免 WAL 实际已提交时产生第二条相反历史；
- 不运行 outbox release 通知；
- WAL 拒绝后续 append；
- fsync 失败是粘滞的（RR-20260926-33）：Linux 同一 fd 的写回错误只报告一次，之后的 fsync 返回 0 不证明失败那批页已落盘。
  因此 terminal 之后 `Ack` 直接返回 terminal 错误、不写 checkpoint，group-commit ticker 与 `Sync` 不再 fsync，`DurableLSN` 不再前进；
  排空关闭最后一次 sync 失败时在途 `Ack` 同样返回错误。这些错误以及交给在途 pipelined 票据的错误都满足 `errors.Is(err, nest.ErrCommitIndeterminate)`；
- `OnFatal` 必须让实例停止接流并退出，由新进程 replay 判定最终历史。

这不是可在线重试的普通错误。生产环境必须将 `OnFatal` 接到进程 fencing/shutdown。

## 8. 接入示例

```go
dataMod := dataengine.NewMod(dataengine.WithEntityAccess(entityAccess))
application.Mods(
    mongo.NewMongoMod(),
    nats.NewNatsMod(codec),
    dataMod,
    nestkit.NewMod(entityAccess), // 从 ModDataEngine 取得 lazy committer
)
```

主动排空：

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
if err := dataMod.Flush(ctx); err != nil {
    return err
}
```

停服顺序：停止网关接流 -> 等待 Nest 请求和 entity guard 排空 -> `dataMod.StopWithContext(ctx)` -> 关闭 Mongo/NATS client。

## 9. 监控建议

至少采集：WAL queue、segment/offset、append bytes、fsync 次数和耗时、unacked 年龄、replay failures、mutation/effect 重试次数、最后错误、segment 磁盘占用。以下情况告警：

- terminal/indeterminate error：立即最高级别告警并 fencing；
- unacked oldest age 持续超过业务目标；
- WAL 目录空间低于可容纳最大恢复窗口；
- queue 长时间高水位；
- replay 连续失败或 outbox 重试持续增长。

## 10. 发布与升级

当前为单仓单模块，一个 roost-core tag 包含 core、kit 与 codegen。应用升级同一版本并重新生成 DAO/Nest handler，再运行生成工程验证。

不存在 V1/V2 双写或旧 `rollback=dirty` 兼容链路。升级时必须重新生成 DAO/Nest 代码；生产 durable handler 必须使用无副作用的生成快照，或显式实现 `RollbackSnapshotter`。

### v1.23.1 复核：async 与重放身份

async 接受写入后允许锁外投影先于 WAL fsync；这符合“不保证断电前这批 WAL 已持久”的契约，不保证失败事务未生效。Mongo 已提交时仍保留结果，禁止回滚猜测。`WAL.Ack` 在写 checkpoint 前强制刷覆盖段，刷盘失败则进入 terminal 并停止水位推进。strict / pipelined 的成功持久承诺不变。

当前 CommitRecord digest 来自 JSON。改结构字段可能改变同一记录的 digest；版本升级前须停止旧进程并排空 WAL / outbox，不能用新二进制盲目重放旧格式未确认记录。当前尚未部署，不提供历史 digest 兼容。2026-10-08 维护者授权 WAL 单路径收敛，取代此前对本组 API 的 C8 保留决定：删除 `nestwal.Committer` / `OpenRuntime`，正式提交与投影统一使用 `engine.Projector` / `Assembly`，不扩展删除其他 C8 API。

WAL 只支持 codec 7，旧 codec 5/6 明确拒绝；移除 `writer_version` 配置与 Mutation 旧字段。升级先停旧进程、完成需保留数据的落库，再使用新 WAL 目录；不自动删除旧文件或推进 checkpoint。
