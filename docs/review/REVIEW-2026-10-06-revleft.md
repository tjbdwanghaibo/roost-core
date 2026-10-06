# N11 / N12 / N13 留项与两个小防护（revleft）

2026-10-06，基线 `dc877c04`（origin/main），独立 worktree 分支 `revleft`，NC 编号段 260～279（本轮用 260～270）。维护者 2026-10-06 授权：确认的缺陷先红后绿。[接力清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md) · [跨轮进度](PROGRESS.md) · [证据](../bugfix/evidence/noncore-bugfix-20261006-revleft/README.md) · 上一轮记录：[N11](REVIEW-2026-10-05-n11.md)、[N12](REVIEW-2026-10-05-n12-revn12.md)、[N13](REVIEW-2026-10-05-n13.md)。

**结论**：确认 11 条缺陷（全部 P3，NC-270 为潜伏），均先红后绿修复、未发版；N11 的 O6 / O7 / O9 是需求选择，写成选项交维护者（本文 §5，DECISIONS-PENDING“revleft”节）；N13 其余观察逐条给出不修的理由；新增根包冲突标记门禁。

**图谱限制**：codebase-memory 项目 `Users-whb-roost-roost-core` generation 停在 2026-09-30T11:28:30Z。本轮引用的 22 个 Go 文件 coverage 均为 `no_recorded_issue`，其中 robot.go、loadtest/manager.go、log/log.go、topologic_sort.go、task_pool.go 与 kit 各 Mod、entity_manager.go 为 `metadata_changed`；`.tmpl` 不在图里。全部结论以当前源码与 `rg`（含 `.tmpl`）补证。

## 1. 确认的缺陷

| 编号 | 等级 | 单元 / 来源 | 一句话 |
| --- | --- | --- | --- |
| [NC-260](../bug/RR-20261005-NC-260.md) | P3 | 小防护 B | Mongo 客户端已断开时 Mongo Mod 的 Stop 永远失败（mongo-driver Disconnect 不幂等） |
| [NC-261](../bug/RR-20261005-NC-261.md) | P3 | N12 O8 | robot `Session.Call` 的发送不受 ctx 约束 |
| [NC-262](../bug/RR-20261005-NC-262.md) | P3 | N12 O8 | robot 超时之后才到的应答被当成推送 |
| [NC-263](../bug/RR-20261005-NC-263.md) | P3 | N12 O6 | 日志轮转失败丢行、控制台出错连带文件丢行、无失败计数 |
| [NC-264](../bug/RR-20261005-NC-264.md) | P3 | N12 O4 | Prometheus 标签值转义不符合 exposition 格式 |
| [NC-265](../bug/RR-20261005-NC-265.md) | P3 | N12 O10 | `Coalescer.Close` 不等最后一次 flush |
| [NC-266](../bug/RR-20261005-NC-266.md) | P3 | N12 O11 | loadtest 被 Duration 截断记成 completed |
| [NC-267](../bug/RR-20261005-NC-267.md) | P3 | N13 O5 | ObjectPool 重复 Put 后两次 Get 拿到同一对象 |
| [NC-268](../bug/RR-20261005-NC-268.md) | P3 | N13 O4 | TopologicalSortCache 与调用方共享依赖切片 |
| [NC-269](../bug/RR-20261005-NC-269.md) | P3 | N13 O9 | TaskPool 先 Shutdown 再 Start 后停不下；Shutdown 后 Start 假 running |
| [NC-270](../bug/RR-20261005-NC-270.md) | P3（潜伏） | N11 O4 | game-demo `PathFindSystem.Stop` 无锁清空 terrain，与无锁调用方并发寻路竞争 |

## 2. N11 留项

| 项 | 核对（调用 / 数据 / 资源所有权链） | 结论 |
| --- | --- | --- |
| O4 场景系统入口 | `rg` 穷举 `PathFind()` / `Terrain()` / `Refresh()` / `.Place(` / `.Path(`（含 `.tmpl`）：enter_scene、move_player 两个 Nest handler（Scene 为声明目标，持 Scene 锁）；spawner 定时器经 `RefreshSystem.Due` → `place` → `pathFind.Place`（不持 Scene 锁）。停止链：`EntityManager.Destroy` 取实体互斥锁、`SetRemoved` 后解锁再 `OnDestroy`（`entity/entity_manager.go:214-286`）→ `Runtime.Stop` 逆序（refresh → pathFind → terrain）。refresh 先停且 `Due` 与 `Stop` 同锁，所以现有入口都与 PathFind.Stop 有先后 | **不是所有入口都在 Scene 锁内**（spawner 不在），但现有入口不触发；Scene 注释承诺系统访问器“可从任何地方调用”，PathFind 的无锁置 nil 违背它 → NC-270（潜伏），按契约修 |
| O6 同期限触发顺序 | `timer/scheduler.go` 堆按期限比较，同期限顺序由堆决定；World 同一 Tick 武装的节点期限相同（`pin`），目前只有一种定时器、彼此无依赖 | 需求选择，§5 D-L1 |
| O7 未注册类型静默删除 | 到期节点类型无 handler → 发 ChangeDelete、删除、无日志 | 需求选择，§5 D-L2 |
| O8 activity stop | NC-193（`d6550a16`）之后 App 在 Init 失败时也调 `Service.Shutdown`；game-demo `Init` 在 `startActivity` 返回后立刻 `s.stopActivity = stop`（`service.go.tmpl:198-202`），后续任何一步失败 Shutdown 都会停两条循环；Shutdown 调用后置 nil | **Init 后段失败不停循环：已由 NC-193 收口，无残余**。stop 闭包本身不幂等（第二次永久阻塞在 `<-done`），唯一调用方 Shutdown 保证只调一次，Service 不承诺并发 Shutdown；不配合 ctx（两条循环收到 cancel 后在一次 Nest 调用内返回）。未改，记观察 |
| O9 logic_offset | `app.go:137` `clock.SetOffset(time.logic_offset)`；game-demo `tickWorld` / `openCurrentWindow` / 结算用 `time.Now()`，贡献用 `runner.clock()`（生产为 `time.Now`）；协调器 `kit/service/global/activity` 的 `Config.Now` 生产为 `time.Now`（`service.go:214`） | game 与协调器两端都用墙钟，窗口 ID / 截止时间 / 宽限期自洽。只让 game 跟逻辑时钟会与协调器错开 → 需求选择，§5 D-L3 |

## 3. N12 留项

| 项 | 结论 |
| --- | --- |
| O8 `Call` 写阻塞 / 迟到应答 | NC-261、NC-262 |
| O6 日志轮转 / MultiWriter / 失败计数 | NC-263 |
| O4 Prometheus 标签转义 | NC-264；`# TYPE` 不输出不是缺陷（抓取器按 untyped） |
| O10 `Coalescer.Close` | NC-265 |
| O11 Duration 记 completed | NC-266；顺带 O9 的丢失 cancel（无行为变化）与 Stats 注释改正 |
| O9 Stage 先缩后扩复用序号 / PlayerID | 未改：需要“序号由谁回收”的语义决定（复用是 k6 VU 的常见做法；旧机器人收尾与新机器人同 PlayerID 重叠才有害），且无确定性红测试 |
| O1～O3 Reporter / 序列删除 | 维护者已决定 C6（`491aaf3b` 已实施默认 Reporter）；序列删除 API 仍待定，不在本轮 |

## 4. N13 留项（§4 O1～O11）

| 项 | 结论与理由 |
| --- | --- |
| O1 `Bucket.Get` 未命中拿写锁 | 不改：性能项，没有性能证据；未命中时 build 需要写锁，拆分读写路径要先有压测数据 |
| O2 `LockManager` 工厂在桶写锁内执行 | 不改：默认工厂只分配内存；“分布式锁工厂”无实现方。设计提示已在注释 |
| O3 `RangeCursor` 无同步 | 不改：README §16 已写明“只能单 goroutine 用”，字段是导出的，加同步要改公开 API；零调用方 |
| O4 拓扑排序 | 切片别名 → NC-268；同层顺序随 map 变化不改：没有承诺顺序，唯一可能的使用方 `manager/order.go` 已自己排序 |
| O5 ObjectPool 重复 Put | NC-267 |
| O6 `ShardedSafeMap.Read` 持锁回调 | 不改：C7 已定为非遍历回调、不适用遍历契约，`safemap/sharded.go:70-72` 与 README 已写明 |
| O7 三种 map 的 `RangeX` 语义 | C7 已实施（`cd43a5ac`），不重复 |
| O8 `SafeFunc` 不记栈 | 不改：可观测性改进，调用方 worker / nest 属核心线；没有缺陷可红 |
| O9 TaskPool | 生命周期 → NC-269；`totalTasks` 入队后才加（瞬间 completed > total）没有确定性红测试（需要在入队与计数之间暂停），不改 |
| O10 `Parallel*` 吞 panic 成零值 | 不改：区分 panic 需要改返回签名（公开 API），零调用方；见 §5 D-L4 |
| O11 README 包名 | 上一轮已改正 |

## 5. 待维护者选择（同步写入 DECISIONS-PENDING“revleft”节）

| # | 事项 | 选项 | 推荐 |
| --- | --- | --- | --- |
| D-L1 | timer 同期限触发顺序（N11 O6） | (a) 保持：同期限顺序未定义，写进 `timer.Scheduler` 注释 (b) `timerHeap.Less` 以 ID 作第二键 = 登记顺序（行为变化，代价一次比较） | (b)：World 同一 Tick 武装的节点期限必然相同，登记顺序最符合直觉，成本可忽略；没有需求时 (a) 也可接受 |
| D-L2 | 未注册类型的节点到期（N11 O7） | (a) 保持静默删除 (b) 删除 + Warn 日志 + 计数（`timer.unregistered_fired`），`TimerComponent.OnInitFinish` 加载时对无 handler 的存量类型告警一次 (c) 保留节点不删、不触发、告警（需要“跳过”状态，堆顶会一直是它，Tick 要能越过） | (b)：移除一种定时器类型时运维能看见丢了什么；(c) 增加状态与分支，违背 N11 方向判断“不再给 Tick 加分支” |
| D-L3 | World / 活动窗口跟不跟 `time.logic_offset`（N11 O9） | (a) 保持墙钟，文档写明 `time.logic_offset` 只影响 `fctx.Now` / `clock.Now` / ops `server_time_ms`，不影响活动窗口与 World 定时器 (b) 全链跟逻辑时钟：game `runner.now = clock.Now` 并让 tick / open / settle 都读 `runner.clock()`，协调器 `Config.Now = clock.Now`，部署约束“参与同一活动组的进程 offset 必须相同”（doctor 检查） | (a) 现在做（文档）；需要用偏移测活动时再做 (b)，因为只改 game 一端会让窗口 ID 与协调器的截止 / 宽限期错开 |
| D-L4 | `goroutine.Parallel*` 回调 panic（N13 O10） | (a) 保持（吞成日志 + 零值） (b) `ParallelCollect` 改返回 `[]error` / 带 panic 的结果 (c) 删除（零调用方，C3 / C8 的“保留”覆盖范围待确认） | (a)，等出现调用方再定 |

## 6. 小防护

**A：冲突标记门禁**（根包 `conflict_marker_gate_test.go`）。`git grep -nIE '^(<<<<<<<|>>>>>>>)( |$)|^=======$' -- . ':(exclude)artifacts'`，一个文件只要有 `<<<<<<<` / `>>>>>>>` 行就失败，单独的 `=======`（Markdown setext 下划线）只在同文件已有前两种时列出。自检：历史里有 `0aa2e1b9`（`80902948` rebase 后实际进 main 的那笔）时，扫描它必须报出接力清单——任务里写的 `80902948` 本身不含冲突，冲突是在它 rebase 成 `0aa2e1b9` 时产生的，`2c1c7be7` 顺手删掉。非 git 工作区（模块 zip）跳过。红（临时追加冲突）/ 绿见[证据](../bugfix/evidence/noncore-bugfix-20261006-revleft/gate-red.txt)。

**B：kit 各 Mod 的 Stop / Close 再调用**（NC-173 残余、NC-233 同类：第三方客户端 Close 不幂等）。

| Mod | 停止实现 | 再调用 / 出错后重试 | 依据 |
| --- | --- | --- | --- |
| mongo | `client.Close` 成功才置空 | **客户端已断开时永远失败 → NC-260** | 新用例 |
| nats | bus / asm 非“排空未完”即置空，终态错误只报一次 | 收敛；`nats/driver.Assembly.Close` 直接调第二次返回 `ErrClosedUndrained`（`Drain` 对已关连接报错），只有 NatsMod 一个调用方且已置空，记观察 | 既有 `nats_mod_stop_retry` / `drain_budget` 用例 + 源码 |
| etcd | 每次调 `Assembly.Close` | NC-173 残余已让 `Client.Close` 幂等，骨架第 4 步绿 | `etcd/driver/stop_contract_test.go` |
| redis | NC-233 已修 | 收敛 | `kit/redis/stop_retry_promises_test.go` |
| syncbus | 成功置空 bus；排空未完保留 | 收敛 | `kit/syncbus/mod_stop_test.go`、syncbus 骨架 |
| dataengine | `Assembly.Shutdown` 成功后置空 runtime | 收敛（核心 runtime 自身契约属核心线） | 源码 `dataengine/engine/assembly.go:191-217` |
| remoteentity | `Assembly.Stop` 可重复 | 收敛 | `remoteentity/stop_contract_test.go` |
| ops | `http.Server.Shutdown` 成功才置空 | 收敛 | `kit/ops/stop_contract_test.go` |
| statslog | `stopOnce` + `closedCh` | 收敛 | 源码 + 既有用例 |
| configdata | 撤销钩子后清空列表 | 收敛 | 源码 + 既有用例 |
| manager / nest | 骨架覆盖 | 收敛 | `manager/`、`kit/nest/stop_contract_test.go` |
| lock | 空操作 | — | 源码 |
| saga | 不在本轮范围（`wt-revn06s5`） | — | — |
| account / chat / directory / global / mail / match / platform / rank / session 服务 Mod 与 `*_rpc_assembly_gen.go` ClientMod | `Stop() {}`，不持有客户端 | 不适用 | 源码 |

## 7. 方向判断

- **停机“再调用返回 nil”第三次在第三方客户端的 Close 上被打破**：NC-173 残余（etcd `clientv3.Close`）→ NC-233（go-redis `ConnPool.Close`）→ NC-260（mongo-driver `Disconnect`）。根因相同：驱动的 Close 第二次调用报错，而 Mod 用“Close 返回 nil”判断是否已释放。三处修法都在“驱动适配层或 Mod 自己识别已关闭”，没有增加状态。这是 A2“驱动行为契约表”的一行（Close 幂等性），建议维护者把“各驱动 Close 的二次调用 / 出错后状态”补进 A2 的契约表，以后新增驱动适配按表在 `*/driver` 层做成幂等，而不是在 Mod 里各自判断。kit 下已无同类残余（§6 B）。
- **robot 与 log / metrics**：本轮缺陷分散在不同层（会话、loadtest、日志 sink、导出格式），没有同一不变量反复被打破的信号。
- **N13 零调用方 API**：本轮三条（NC-267～269）仍落在零调用方 API 上（C3 / C8 决定保留）。保留就意味着每次 review 都会在它们身上找到合法误用的后果；若维护者希望减少这类维护面，可重新考虑 `ObjectPool` / `TopologicalSortCache` / `TaskPool` / `Parallel*` 的去留。

## 8. 验证

改动包 `go vet` 与 `go test -race -count=3`：`mongo/driver`、`kit/mongo`、`robot/...`、`log`、`metrics`、`container`、`goroutine`；相邻 `app`、`kit/ops`、`worker`、`lock`、`entity` race；全仓 `go build ./... && go vet ./...`、根包 `go test -count=1 .`、`go test -count=1 ./codegen/...`；全新生成 game-demo（replace 到本分支）`go build ./... && go vet ./...`、`go test -race -count=3 ./game/scene/...`、`go test ./game/... ./internal/service/game/...`。gofmt 空。本轮没有改核心四块，未跑 glsvet；未跑 integration。

未验证：真实网关上的发送阻塞与迟到应答、真实磁盘写满 / 只读文件系统、真实 Prometheus 抓取、真实 Mongo 的断开路径。

## 9. 停点

N11：O4 收口（NC-270），O8 无残余，O6 / O7 / O9 待维护者（D-L1～D-L3）；组件内存回滚契约已由 A1 定案；index 去留已由 C3 / C8 定为保留。N12：O4 / O6 / O8 / O10 / O11 收口，O9 Stage 复用序号待语义决定，O2 / O3 序列删除 API 待定，外部项照旧。N13：§4 全部有结论，D-L4 待定。
