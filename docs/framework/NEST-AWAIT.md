# Nest 三池与异步业务续行

本篇说明当前工作树新增的三池和 Await 接口，尚未发版。基线为 `7fe5533c`；不改变网络协议和持久格式，不自动发版。Nest 仍在原包内实现，Entity Kind 增加执行策略，fctx 增加执行身份，wiring 与 codegen 传递配置。文末登记实际验证及边界。

## 三种执行资源

| 配置 | 用途 | 实体访问 |
| --- | --- | --- |
| `nest.fast` | 短业务 | Guard 保护 |
| `nest.long` | 长业务 | Guard 保护 |
| `nest.slow` | I/O、冷加载、Remote 准备与收尾 | 禁止直接取得 Guard/Cast |

每组分别配置 `workers`、`queue_capacity`。沿用 fast/slow 配置名称，避免无关的配置迁移。长池默认 worker 数等于短池，等待容量 4096；短池与 I/O 池沿用既有缺省值。CPU 密集长业务仍消耗 CPU，更多 worker 不代表更多算力。

一个接线配置例子（这些数值只用于说明，不是容量建议）：

```yaml
nest:
  fast: {workers: 8, queue_capacity: 65536}
  long: {workers: 4, queue_capacity: 4096}
  slow: {workers: 32, queue_capacity: 64}
```

`NestOptionWithBusinessPools(short, long, io)` 可在直接构造引擎时配置三池。`NestOptionWithWorkerPools(fast, slow)` 保留原来的短业务与 I/O 含义。

## 初始化注册 Kind

```go
entity.MustRegisterEntityKindDefs(
    entity.EntityKindDef{
        Kind: PlayerKind, Category: PlayerCategory,
        BusinessPool: entity.BusinessPoolLong,
    },
    entity.EntityKindDef{
        Kind: TroopKind, Category: TroopCategory,
        BusinessPool: entity.BusinessPoolShort,
    },
)
```

先注册 Kind，再构造 Nest。已有 Kind 的默认执行策略在引擎构造时固化；禁止把已确定的短业务改成长业务，反之亦然。默认策略为短业务。注册 Builder 也可携带 `BusinessPool`；生成实体支持 `//roost:entity ... businessPool=long`，省略时不覆盖已经注册的 Kind 策略。

一条消息只选一个业务池。全部目标都是短业务 Kind 时走短池；任意目标为长业务 Kind 时，整条消息走长池。各池共用一张 ID tails 表。Player+Troop 长消息执行期间，后来的同 Troop 短消息等在依赖队列，不占用短 worker；不相关实体仍可执行。

这不会消除实体本身的串行限制。持锁执行 200ms，依赖这些目标的后继就需要等待；也不保证绕过 Nest 的外部锁操作不会产生等待。

## 在一个 handler 中写完查询和应用结果

以下 Player 和排行榜类型是业务定义，`Await` 为框架入口：

```go
//roost:nest target=PlayerKind durability=memory
func ClaimRankReward(player *Player, seasonID int64) error {
    playerID := player.GUId()
    board := boardFor(seasonID)
    return nest.Await(
        func(ctx context.Context) (RankResult, error) {
            return queryRank(ctx, board, playerID)
        },
        func(player *Player, result RankResult, err error) error {
            if err != nil { return err }
            if player.HasClaimed(seasonID) { return ErrAlreadyClaimed }
            player.AddReward(rewardFor(result))
            player.MarkClaimed(seasonID)
            return nil
        },
    )
}
```

必须使用 `return nest.Await(...)`。当前执行段完成清理、释放实体锁及 tail 后，框架才启动 I/O。原请求的最终回复延迟到恢复段完成。慢闭包只访问复制出来的值，不能捕获并使用原 Player/DAO 指针。恢复回调得到重新加载并加锁的实体。

生成标记使用 `durability=memory` 并省略 rollback；手写注册使用 `HandlerMeta{Durability: DurabilityMemory}`。第一版用于明确注册为 memory 的无事务 handler，拒绝有回滚事务或 Remote 写批次的段；不会悄悄降低已有持久化策略。恢复段也不创建业务事务。本例的奖励是临时内存值；不能直接用此接口写要求事务的持久 DAO setter。持久发奖仍要用已有持久事务业务入口，不能把 memory 当作自动落库。状态改变、防重复、重试和任务持久恢复由业务负责。内存闭包不保证掉进程后恢复。

回调签名为 `func(实体参数..., work结果, error) error`。默认继承当前消息声明的目标顺序，多组目标展开；任意数量实体均可。由于 Go 不支持任意实体参数列表的静态泛型签名，框架在准入慢操作前检查回调形状，恢复时再核对实际实体类型。

查询错误进入默认目标的恢复回调。实体不存在、上下文取消、回投失败等无法执行恢复的错误直接结束原请求。查询 panic 被转换为错误；回调 panic 使用普通 Nest 异常路径。停机停止新的恢复准入，但仍等待已开始的 work 真正结束，不能把超时当作释放成功。

## 查询后才知道其他目标

```go
return nest.Await(work,
    func(player *Player, troop *Troop, result Lookup, err error) error {
        if err != nil { return err }
        return apply(player, troop, result)
    },
    nest.WithResumeTargets(func(result Lookup) []int64 {
        return []int64{playerID, result.TroopID}
    }),
)
```

目标解析器不能做 I/O 或访问实体。work 失败时没有可靠新目标，直接结束原请求，不调用此恢复回调。成功后一次性登记全部新目标的 tail，再加载、取得 Guard、调用回调；长短业务池也按新目标重新决定。

## 执行约束与迁移

- 长短业务池禁止阻塞等待池任务；同步 Request、RunLocal 等入口在投递前拒绝。框架既有 Guard 锁序及提交协议的内部受控等待不等于允许业务任意等待。
- I/O 池禁止创建 GuardScope、RequireEntity、TryRequireEntity 和 Cast。
- Cast 只能访问本执行段已声明的目标。旧代码动态 Cast 其他 ID 会收到 `ErrCastUndeclaredTarget`；应把目标加入原消息，或者用 WithResumeTargets 重新准入，不能持有原锁补 tail。
- glsvet 检查 handler/匿名恢复中的 Request、RunLocal、Wait、Sleep 等显式等待；跳过 Await 的 I/O work，检查动态目标解析器。它是语法辅助检查，不具备任意调用图证明能力。
- 普通 Go channel、第三方阻塞函数无法被运行时普遍拦截；这些仍需代码审查。不要把框架入口检查理解为任意业务代码都不会阻塞。
- 新增匿名恢复复用正式加载、锁、同步及错误收尾路径，不单独维护一套 Entity 生命周期。

## 实际调用与复跑

仓库内的 [Player 示例](../../codegen/internal/entity/testdata/awaitflow/player.go) 同时使用 entity 和 nest 两个正式生成器。[消费端测试](../../codegen/internal/entity/testdata/awaitflow/await_test.go) 通过生成的 Sync_ClaimReward 调用，使用真实 EntityManager/ManagerAccess，没有 mock Getter。测试还验证第二次领取能看到第一次恢复后的状态。

在仓库根目录运行（PowerShell 先设置 `$env:GOWORK='off'`）：

```sh
go run ./codegen/cmd/entity -dir ./codegen/internal/entity/testdata/awaitflow
go run ./codegen/cmd/nest -dir ./codegen/internal/entity/testdata/awaitflow
go test ./codegen/internal/entity/testdata/awaitflow/... -count=2
go run ./cmd/glsvet ./codegen/internal/entity/testdata/awaitflow
```

## 验证记录

2026-10-10～11，本机 Windows/amd64、Go 1.27.0；基线 7fe5533c。以下只记录实际执行范围：

- 全仓 `go build ./...` 通过；Nest、Entity、fctx、servicerpc、wiring/nest、glsvet、entity/marker 生成器的 go vet 通过。
- Nest、Entity、fctx、servicerpc、wiring/nest、glsvet 整包测试连续两轮通过；前五包整包 race 通过。
- entity、marker、nest 生成器及 awaitflow 消费工程连续两轮通过；消费工程 race 连续两轮通过，glsvet 通过。
- 新增 Await/三池定向 race 连续三轮通过，包含多次 Await 串联和取消后不恢复；新增配置相关的 roost 生成工程在沙箱外定向测试通过（113.469s）。
- 回归包含单 worker 的跨长短池多目标依赖、I/O 期间原目标可继续执行、回复晚于恢复、动态多目标、禁止嵌套同步等待、查询错误/panic、停机拒绝恢复、容量预留与取消。
- 原动态 Cast 锁序/回滚/销毁竞态测试通过测试专用目标声明适配器保留底层防御覆盖；它不是合法生产用法，也不证明 tail 顺序。正式目标拒绝和三池 tail 由新调度回归覆盖。
- Mirror 生成文件比较曾在未修改基线上失败；确认 Git autocrlf 的 CRLF 与 go/format 的 LF 差异后，仅在测试比较时规范换行，重新通过。
- 全仓 `go test ./... -count=1 -timeout=120s` **失败**。根目录契约/示例包通过，其余失败包括文件原子替换 Access is denied、缺少 sh、占用文件清理失败及网络/包测试超时。本轮未逐项归因这些全仓失败，不能宣称整仓无回归。完整输出保留在本机 D:/whb_s/await-full.txt，可按该命令复跑；无需依赖该文件理解验收范围。
- 图谱核对开始时，可用项目 D-whb_s 的 coverage generation 为 2026-09-08T14:39:26Z，本工作树路径均 not_tracked，另两路 MCP transport closed。本轮结论以当前精确源码、生成和实跑补证，不使用过期图谱行号。提交后已对 D:/whb_s/cube-core 刷新 roost-core 项目，工具返回 indexed（29722 节点、277054 边）；但后续 coverage 对核心路径仍报告 metadata_changed，因此不把刷新成功等同于覆盖验证通过。

性能收益未经测量；没有重跑真实排行榜网络、生产持久发奖、跨机或长期重档，不宣称提高容量或消除所有死锁。

## Msg 状态与目标存储（2026-10-13）

本次以 6c36e0df 为基线，只整理 Msg 字段职责和目标存储。所有字段及类型保留，按请求、Remote 批次、自身事务、执行资源、Await、观测分组；没有把“已接纳”和“确定提交”合并，也没有删除提交后失败、部分提交、结果未知或不可回滚禁止重排的状态。字段注释说明当前契约，历史缺陷仍可在方法注释和 Git 历史追溯。未新增分组指针对象。Windows/amd64 实测结构体大小由 536 降至 528 字节，这是字段对齐变化，不等于每条消息实际堆分配减少 8 字节。

### 三种队列分别负责什么

- ready：ID 依赖已经满足、可以交给 worker 的普通请求。
- waiting：所有尚未开始执行的普通请求，包含等待前驱和等待 worker；双向链表支持 O(1) 删除和读取 OldestWaiting，不负责 ID 顺序。
- continuations：Remote/I/O 借用业务 worker 执行本地逻辑及收尾。原请求仍占 tail，续行不能再次排在它后面，否则会自等；内部信封完成后通过 done 将原消息交回 I/O。与普通 ready 交替取用，运行时占业务 worker，并参与停机排空。RunLocal 也使用该入口，其实体访问仍需 Guard；不能将内部队列当业务绕过 ID 顺序的入口。

Await resume 与上述内部续行不同：原段先释放 tail，再由 I/O 将恢复段作为普通消息准入。此次保留 waiting、continuations 和调度实现，不代为提交主工作树中维护者的 dispatch_queue.go 整理。

### 目标切片的所有权

Tid 保留单目标标量快路径。Tids 与 GroupTIds 继续保存业务参数顺序、重复成员和组边界；调度 ID 集合另行排序去重，不与业务切片原地共用。

DispatchMulti/RequestMulti 直接接管 normalizeClientIDs 已创建的独立切片，去掉第二次复制。分组入口仍接受 [][]int64，内部标准化结果由外层切片加一块连续 ID 数组构成；每组使用三下标切片限定 cap，因此对一组 append 不会覆盖下一组。源切片修改不影响消息。单组没有分配次数收益，多组由每组分配改为共享消息所有权；保留任意一组会保留整块数组，因此不把内部目标切片作为长期业务缓存。

没有改成公开的“扁平 IDs＋组长度”API。执行时的展平、Guard 排序和加载标准化继续保留，减少后续复制需单独验证参数顺序、重复实体引用和调用方修改隔离。

### 本地测量与验证

Go 1.27.0，Windows/amd64，Ryzen 7 5800H，GOMAXPROCS=4；每组 4 个 ID，前后使用相同 benchmark。先运行 300ms×3，再串行运行 1s×3；测试、race 和编译未与测量并跑。下表耗时为长样本中位数：

| 组数 | ns/op 前 → 后 | B/op 前 → 后 | allocs/op 前 → 后 |
| --- | ---: | ---: | ---: |
| 1 | 141.5 → 149.1 | 56 → 56 | 2 → 2 |
| 8 | 1023 → 878.5 | 448 → 448 | 9 → 2 |
| 32 | 3840 → 3692 | 1920 → 1920 | 33 → 2 |

单组中位数约慢 5.4%，短样本约慢 7.9%；新增总长度遍历存在成本，样本也有明显波动，不能宣称所有场景变快。保留此实现的依据是多组分配次数稳定减少，分配总字节不变。首轮 32 组短样本也有耗时上升，全部保留，不以该微基准推断调度或整服吞吐。

原始样本：[短测前](../performance/benchmarks/nest-message-targets-20261013/before.txt)、[短测后](../performance/benchmarks/nest-message-targets-20261013/after.txt)、[长测前](../performance/benchmarks/nest-message-targets-20261013/before-repeat.txt)、[长测后](../performance/benchmarks/nest-message-targets-20261013/after-repeat.txt)。复跑：`go test ./framework/nest -run '^$' -bench '^BenchmarkClientGroupTargets$' -benchmem -benchtime=1s -count=3`，旧版对照只带回相同测试文件。

新增所有权回归覆盖顺序、重复 ID、空组、调用方修改隔离、组 append 隔离，以及错误中的组号/成员位置。图谱代际 2026-10-10T16:08:17Z 已过期，本次用当前源码核对写入、读取、回收及调用链，未把旧索引当完整证明。

本次实际通过：`go test -race ./framework/nest ./wiring/nest -count=2 -timeout=4m`、`go build ./...`、`go vet ./framework/nest ./wiring/nest` 和根包 Markdown 链接检查。AST 对比确认 Msg 的 43 个字段名称和类型完全保留。未运行新增真实资源、跨机或长期容量测试，不宣称生产吞吐提升。
