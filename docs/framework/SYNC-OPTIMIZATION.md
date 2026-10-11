# Sync 局部优化与玩家 Controller 生成

基线：a0ecc74f，2026-10-11。原包目录保持不变，本次不更改线协议、持久化边界或 on_change/periodic 调度语义。

## 实施方案

1. Flush：修订号快照只在当前 subject 的捕获与结算准备间使用，同轮复用一张临时表，减少每个 subject 单独分配。不共享 Prepared 内容，不复用 Transport 已接纳的字节。
2. AOI：串行阶段直接汇集事件；并行计算仍写 observer 私有缓冲，等待整批完成后由协调者归集。本轮完成后 Flush 只取事件，不扫描全部观察者。保持同一 pair 的事件顺序和移除后重建的生命周期。
3. Relation：同一锁下维护 subject 到 observers 的反向索引，Forget 只遍历真正持有关系的观察者；Set/Clear 同步维护正反索引。代价是每条关系额外的索引内存。
4. Lockstep：成员变化时作废广播名单缓存，Tick 复用确定顺序；退出、重绑、旁观者与 Close 都要处理。保持单所有者模型和逐接收者准入错误，不改帧序列。

Controller 已按用户确认接入 player access 的协议生成流程：骨架参数为 player_agent.Context 和对应 PB 请求，业务代码自行维护，重复生成不覆盖业务。手动响应继续走当前连接的 Session.Reply，不能把发往 PlayerID 所有连接的 Push 当请求响应。实际用法见[玩家协议教程](PLAYER-ACCESS.md)。

## Controller 如何生成和维护

协议定义写 `handler=player` 后，`go generate ./...` 会创建缺少的 `game/controllers/player/controller.go` 与对应业务方法文件。Controller 持有 Nest Client，但不会自动执行任何 Entity 操作。每个新方法只有参数、返回类型、中文说明和明确的未实现错误，剩下的逻辑由业务编写。

生成器先解析同包所有非测试 Go 文件，按 Controller 方法名查重；方法移动到其他文件后也不会补出重复方法。类型和 FromRegistry 已存在时复用，只有一半存在则报错，避免猜测业务依赖。文件名占用但没有所需方法时报错。删除协议不删除业务，修改签名后由开发者调整方法并编译验证。

事务生成在暂存目录内工作，新增骨架作为首次创建的业务文件提交；已存在的骨架不覆盖、不删除，也仍参与业务输入并发修改检查。`--check` 单独把骨架纳入内容快照，能发现缺失，但不将其视作允许覆盖的生成文件。直接调用底层 `protocol` 工具时可显式传 `-controllers`；正式业务推荐使用 `go generate` 的事务入口。

## 顺带修复：手动响应后的 typed nil

原实现中，类型为 `*pb.Response` 的 Controller 返回 nil，经泛型注册器包装成 any 后仍有动态类型。Dispatch 仅检查 `response == nil`，结果误入自动编码。业务已经调用 Session.Reply 时，可能再回一条空响应，或被编码错误中断连接。

真实生成工程中的负对照记录为：`response=&{2 7 []} err=<nil> encoded=1 replies=1`，测试因额外的自动响应失败。修复在 Dispatch 编码前识别 nil 指针及其他可为 nil 的值，将其恢复为“不自动回复”；非 nil 响应与业务错误仍走原路径。回归要求返回响应为空、编码次数为 0、手动回复次数为 1。该修复并不提供通用重复请求防护，也不禁止业务主动调用两次 Reply。

## 验证与回退

同机、同工具链、同参数先后运行 Manager Flush、AOI 空闲/稀疏事件、Relation 更新/退出和 Match Tick 基准；保存全部样本和分配量。性能运行不并发执行 build/race。对新状态增加生命周期/并行与重生成保护回归，再执行受影响包、生成工程和 Go/C# 接入验证。

没有收益或引入明显代价的优化应撤回或明确限制，不改变测试断言和负载通过验收。不以微基准推断端到端 P99 或跨机容量；不增加默认延迟合并窗口，不把 Flush 改成并行交付，不新增无锁队列。

## 同机基准结果

2026-10-11，Windows/amd64、Go 1.27.0、AMD Ryzen 7 5800H、GOMAXPROCS=4。基线 a0ecc74f 加入相同 benchmark 后先跑 before，再运行本次实现；两次计时期间均未并发执行本任务的 build/test/race。每组 `200ms × 3`，下表为耗时中位数，分配取该样本值。原始三次样本完整保留：[before](../performance/benchmarks/sync-local-20261011/before.txt)、[after](../performance/benchmarks/sync-local-20261011/after.txt)、[汇总](../performance/benchmarks/sync-local-20261011/medians.csv)、[工具链](../performance/benchmarks/sync-local-20261011/environment.txt)。

| 场景 | 优化前 ns/op | 优化后 ns/op | B/op 前 → 后 | allocs/op 前 → 后 |
| --- | ---: | ---: | ---: | ---: |
| Flush 1 会话、32 实体、1 profile | 66,781 | 54,911 | 50,136 → 50,136 | 245 → 245 |
| Flush 64 会话、32 实体、1 profile | 1,883,592 | 1,573,727 | 1,087,762 → 1,017,203 | 850 → 763 |
| Flush 64 会话、32 实体、4 profiles | 1,977,582 | 1,638,951 | 1,185,375 → 1,114,768 | 948 → 861 |
| Flush 256 会话、32 实体、1 profile | 7,811,413 | 5,535,216 | 4,250,910 → 3,965,370 | 2,389 → 2,305 |
| Flush 256 会话、32 实体、4 profiles | 6,915,964 | 5,858,103 | 4,348,582 → 4,063,040 | 2,488 → 2,405 |
| AOI 1,000 观察者、空闲 | 29,981 | 11.17 | 0 → 0 | 0 → 0 |
| AOI 1,000 观察者、单实体来回移动 | 36,348 | 485.3 | 104 → 72 | 4 → 3 |
| AOI 10,000 观察者、空闲 | 435,472 | 10.5 | 0 → 0 | 0 → 0 |
| AOI 10,000 观察者、单实体来回移动 | 422,628 | 501.6 | 104 → 72 | 4 → 3 |
| Relation 10 观察者、重复 Set | 279.1 | 271.3 | 192 → 192 | 2 → 2 |
| Relation 10 观察者、Forget 后重加 | 669.7 | 648.5 | 288 → 480 | 4 → 6 |
| Relation 10,000 观察者、重复 Set | 297.6 | 270.1 | 192 → 192 | 2 → 2 |
| Relation 10,000 观察者、Forget 后重加 | 269,422 | 659.3 | 288 → 480 | 4 → 6 |
| Lockstep 10 玩家 Tick | 4,576 | 3,656 | 2,600 → 2,394 | 20 → 18 |

如何理解这些数字：

- Flush 的帧数、打包次数与传输字节在前后样本中一致。多会话档分配下降符合复用修订表的目标；单会话分配不变，短测耗时变化不单独归因为该改动。
- AOI 空闲值只包含两个事件来源的 Flush，不包含空间更新、网络发送或实际业务。原来每次遍历全部观察者，现在空事件直接返回。移动档是观察者相距很远、一个实体只影响附近一个观察者的稀疏场景，不代表全员集中移动、并行批量计算或大扇出容量。
- Relation 大表中的退出原本扫描所有观察者，现按反向索引只处理真正订阅者。小表几乎没有耗时收益，Forget/重加却额外增加 192 B 与 2 次分配；静态存量也多持有一份反向关系。这是为大表退出可扩展性保留的明确代价，不是所有关系操作都更省内存。极密集关系需要另做存量容量验证。
- Lockstep 本样本耗时下降约 20%，每 Tick 少 2 次分配；历史仍按原逻辑增长，宿主仍须管理 Trim。没有重测长局容量、网络 P99、15 分钟重档或跨机 HA，不能据此宣布已解决 v1.25 的重档失败。

复跑命令（PowerShell；先切换到需要对比的源码版本，并确保有相同的 benchmark 文件）：

```powershell
$env:GOWORK='off'
$env:GOMAXPROCS='4'
go test ./framework/sync/entitysync ./framework/sync/entitysync/policy ./framework/sync/lockstep -run '^$' -bench 'Benchmark(ManagerFlush|AOIEventCollection|RelationUpdates|MatchTickTenPlayers)$' -benchmem -benchtime=200ms -count=3 -timeout=5m
```

## 行为验证

本地已通过：Sync 全包与 protocol 包测试；policy/lockstep 全包 race；新增 AOI 串行与 4 worker 事件一致性、观察者移除重建顺序（race 下重复 3 次）；Relation 替换/清空/退出/重加；Lockstep 重绑、旧断线、旁观者退出与 Close；根模块 `go build ./...`；Sync 与 codegen 相关包 vet。

生成工程验证覆盖自动创建、已有业务逐字节保留、方法移到其他文件后不重复创建、文件冲突拒绝、事务生成落盘、缺失骨架 `--check`、生成工程实际编译，以及 typed nil 的修前失败/修后通过。既有显式 endpoint 工作流和完整 demo 模板也通过。Go/C# TCP 消费测试覆盖真实生成服务器的协议回包与生成工程 build/vet；不等待 GitHub CI，也没有发布新版本。

结构定位使用 codebase-memory 后以当前源码补证：图谱基线仍为 2026-10-10T16:08:17Z，相关路径报告 metadata_changed，新文件不在该索引中，不能把旧图谱当作本次源码的完整索引。
