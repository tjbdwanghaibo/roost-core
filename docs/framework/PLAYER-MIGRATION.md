# 静态 Player 跨机迁移参考流程

本篇是把一个 game sid（连同绑定在它上面的全部静态 Player）从一台机器搬到另一台的参考流程，维护者 2026-10-15 决定按此执行（[DESIGN-TODO D5](../maintenance/DESIGN-TODO.md#d5-静态-player-跨机迁移的参考流程)）。每一步写明用到的现有能力与检查点；检查工具是 game-demo 模板里的 [cmd/sidmove](../../demo/cmd/sidmove/main.go.tmpl)。框架级的“迁移 Player”命令（ops 管理面的 drain-and-move，自动执行并处理部分完成）是上线后评估的候选，尚未实施。

## 1. 先说清“迁移”的对象

静态绑定下，玩家建角时由 account 绑定到一个 game sid（`Role.ServerID`，[service/account/types.go](../../service/account/types.go)），之后不变；同一 sid 同一时刻只有一个进程（App 单实例锁），game 进程只为绑定在自己 sid 上的玩家服务（demo 的 [playerowner.go](../../demo/internal/service/game/playerowner.go.tmpl)）。因此“同一玩家至多一个写者”就等于“同一 sid 至多一个进程”。

所以跨机迁移搬的是 **sid**：旧机器上的 sid 进程停下，新机器上同一 sid 的进程接管，玩家仍绑定在原 sid。框架**没有**把单个玩家改绑到另一个 sid 的能力（account 没有修改 `ServerID` 的接口，`PlayerOwners` 拒绝不属于本 sid 的玩家）；把整个服并入另一个服用别名合服（[§6](#6-合服把服-a-并入服-b)），角色仍绑定原服，由承载服的进程服务。

数据只以已落库（Mongo）为准：WAL 只保证本机落库，新机器用空的 WAL 目录启动，不搬运旧机器的 WAL 热状态（[guide/03-dataengine §6](guide/03-dataengine.md#6-维护与升级)）。整个流程的正确性取决于一点：**旧进程的 WAL 在新进程启动前已全部投影并确认**。

## 2. 步骤

| 步骤 | 做什么 | 用到的现有能力（源码） | 检查点 |
| --- | --- | --- | --- |
| 0 准备 | 新机器部署同版本程序与同一份配置（同 sid），只改本机地址：`ops.addr`、`player_access.tcp.addr`、`etcd.advertise_addr`，WAL 目录是本机一个**空**目录（`dataengine.wal.dir` 不写时是工作目录下的 `data/wal/dataengine/<sid>`，新机器上本来就空；同一台机器上演练时显式写一个空目录）；网络能到同一套 Mongo / NATS / Redis / etcd | 配置 | `--check-config` 通过 |
| 1 拒绝新准入 | ① account 把该 sid 标为 maintenance，阻止在该 sid 上新建角色；② 负载均衡 / DNS 停止把新连接导向旧机器 | `Service.UpsertServer`（[service/account/service.go](../../service/account/service.go)；demo `cmd/accountctl upsert-server -status maintenance`）。注意 maintenance 只拒绝 `CreateRole`，**不拒绝已有角色登录**；已有玩家的准入由第 2 步的停机关闭 | accountctl 输出 `"status": "maintenance"` |
| 2 停写 | 向旧进程发 SIGTERM，走正常停机：Service.Shutdown 先断开本进程服务中的全部玩家（`PlayerOwners.CloseServedSessions`），之后新请求被 Nest 以 `nest: stopped` 拒绝；各 Mod 逆序停止 | App 停机顺序；demo [service.go](../../demo/internal/service/game/service.go.tmpl) 的 `Shutdown` | 进程退出（以 PID 为准，不以停机命令返回为准）；停机未完成不得进入下一步 |
| 3 等投影落库 | DataEngine 停机先 `Projector.Flush`（WAL fsync，再回放到一轮没有可处理记录），再关闭 Projector、WAL、Outbox | `Runtime.Shutdown` → `Projector.Flush`（[runtime.go](../../framework/dataengine/engine/runtime.go)、[projector.go](../../framework/dataengine/engine/projector.go)） | `sidmove wal-drained -dir <旧 WAL 目录>`：`drained: true`（checkpoint 等于最后一条记录，或没有记录）。进程仍持有 `writer.lock` 时它拒绝检查 |
| 4 释放单实例锁 | 全部 Mod 停完后 App 删除 `<singleton.key_prefix>:<服务名>:<sid>`；新进程启动时先等这把锁（`singleton: waiting for the current holder to release or expire`），等不到 `singleton.startup_wait` 就以 `ErrSingletonHeld` 退出、不 Init 任何 Mod | App 单实例锁（[singleton.go](../../framework/app/singleton.go)） | `sidmove singleton -key ...`：`held: false`，或已由新进程持有（`pid` 为新进程） |
| 5 路由切换 | 玩家侧：负载均衡 / DNS / 独立 Gate 把该 sid 的接入地址改到新机器，客户端重连、重新认证（account 会话里的 `server_id` 不变）。服务侧不需要手工切：NATS 总线按 sid 寻址，新进程订阅后即接收；etcd 注册由旧进程停机时 `Deregister`、新进程启动时 `Register`（键 `/roost/services/<服务>/<sid>`，值为新的 `advertise_addr`）；JetStream durable 消费者与 Mongo outbox 由新进程从持久游标继续 | etcd `Discovery.Register` / `Deregister`（[discovery.go](../../infra/network/etcd/driver/discovery.go)）；SyncBus / Saga / effects 的 durable 消费者 | 新进程 `/readyz` 200；etcd 注册键指向新地址 |
| 6 新节点加载与校验 | 新进程冷加载玩家时读 Mongo（schema 校验在水合前），`WaitEntityProjection` 只等本进程自己的投影，不知道旧机器的 WAL——这就是第 3 步必须通过的原因 | `EntityRepository` 冷加载；`Runtime.WaitEntityProjection` | 停旧进程后 `sidmove players -ids ... -out before.json`，新进程就绪、业务写入前 `-compare before.json`：未变（空窗期没有第三个写者）；抽样写入成功且 `_version` 从旧值继续 |
| 7 失败回退 | 新进程起不来或校验失败：正常停掉新进程（它的 WAL 同样会排空），确认 `wal-drained`，再在旧机器启动（旧 WAL 目录已全部确认，可复用）。两边任何时候都不并存：单实例锁保证 | 同上 | 回退后同样做第 3、4、6 步的检查 |

**进程异常退出（kill -9、掉电）时**：WAL 里可能有已确认给业务、但还没投影进 Mongo 的记录。此时不能在新机器用空 WAL 目录接管，否则这些写入丢失。正确做法是在**原 WAL 目录**上重启同一 sid（原机器，或把该持久卷原样挂到新机器，仍是单写者），让它恢复并投影，再正常停机、`wal-drained` 通过后继续第 4 步。崩溃进程的单实例锁要等 `singleton.ttl` 过期，重启会先等待这段时间。

**空窗期长度**：从旧进程停机到新进程就绪，这个 sid 不处理请求。跨服 saga 的步骤重试预算要覆盖它：demo 的赠礼 saga 把 debit 的重试次数设成覆盖“一次崩溃重启”（`singleton.startup_wait + ttl + 45s`，见生成的 `config.game.yaml` 与 `gift_saga_budget_test.go`）；空窗超过预算的步骤会进入补偿或人工处理。

**与 Directory / global 路由的关系**：`service/directory` 是全局名字的预留 / 提交 / 释放，与玩家所在机器无关，迁移不需要动它。`service/global` 的 `RouteBinding`（game sid → global 实例，`BeginMigration` / `CompleteMigration` / `AbortMigration` 按 epoch CAS）描述的是“哪个 global 实例服务这个 game sid”，game sid 换机器时绑定不变；只有搬 global 实例本身时才走那套迁移。

## 3. 检查工具

game-demo 的 `cmd/sidmove`（只读，不搬数据、不切路由、不起停进程）：

```sh
# 第 3 步：旧进程停完之后，WAL 是否全部确认（进程还在跑、尾部未确认、WAL 损坏都以非零码退出）
go run ./cmd/sidmove wal-drained -dir data/wal/dataengine/1000
# 第 4 步：单实例锁现在被谁持有（不打印锁值里的令牌）
go run ./cmd/sidmove singleton -redis 127.0.0.1:6379 -key roost:<project>:singleton:game:1000
# 第 6 步：停旧进程后记一份，新进程写入前比较（Player 所在物理库按 -config 的 dataengine.databases 换算，-db 可直接指定）
go run ./cmd/sidmove players -mongo "$URI" -config configs/service/config.game.yaml -ids 2001,2002 -out before.json
go run ./cmd/sidmove players -mongo "$URI" -config configs/service/config.game.yaml -ids 2001,2002 -compare before.json
```

`wal-drained` 的报告带 `databases`：旧进程记在 WAL 目录 `databases.json` 里的逻辑库 → 物理库映射；新机器的 `dataengine.databases` 应与它一致，否则新进程会去别的库里冷加载同一批玩家。

`wal-drained` 基于 `nestwal.Inspect`（[inspect.go](../../framework/nestwal/inspect.go)，与 [cmd/walinspect](../../cmd/walinspect/main.go) 相同的只读检查，先取 `writer.lock`），比较最新有效 checkpoint 与最后一条记录的位置。测试（生成工程 `cmd/sidmove/main_test.go`）覆盖：进程持有时拒绝、有未确认尾部时不通过、全部确认后通过；锁持有者解析不含令牌；两次快照之间的写入被发现。

## 4. 2026-10-15 本机演练

环境：本机私有隔离环境（`dataengine-env.sh`，端口偏移 12000：Mongo 三副本、NATS JetStream 三节点、Redis；另起私有 etcd），当前分支生成的 game-demo，game 与 account / activity / chat / global / mail / match / platform / rank / session 九个支撑服务都是真实进程。“机器 A / B”是同一台机器上的两个 game 进程：同 sid 1000、不同 ops / TCP / advertise 端口、各自的 WAL 目录（B 的为空）。写入负载是一个循环，经 ops 的 `gm.player.add_item` 对玩家 2001、2002 的物品 1001、1002 轮流加 1（每次请求返回加完后的数量），记录每次确认。“路由切换”是把这个循环的目标端口从 A 改成 B。

**正常迁移**：

1. A 就绪，写入循环指向 A。4 秒后启动 B：B 日志 `singleton: waiting for the current holder to release or expire`（持有者为 A 的 PID），`/readyz` 不通，没有创建 WAL 目录。
2. account 把 sid 1000 标为 maintenance。
3. SIGTERM A（写入循环不停）：A 在 09:00:58.795 记 `singleton: released`；之后的请求得到 1 次 `nest: stopped`、47 次连接拒绝。
4. `wal-drained` A：`drained: true`，checkpoint 与最后一条记录同为 `segment 1 offset 45915`。记录玩家快照（2001 `_version` 172，2002 `_version` 75）。
5. B 在 09:01:01.657 记 `singleton: acquired`（A 释放后约 2.9 秒，B 的重试间隔），随后就绪；`players -compare` 未变。
6. 写入切到 B 6 秒，停 B，`wal-drained` B 通过。

结果：A 上确认 146 次、B 上确认 90 次。四个（玩家, 物品）键的确认数量都严格逐次加 1，A 上最后一次分别是 37 / 37 / 36 / 36，B 上第一次正好是 38 / 38 / 37 / 37；最终 Mongo 的数量都是 59，等于各键最后一次确认。`_version` 的增量等于确认次数（2001：98 → 172 → 216，即 A 上 74 次、B 上 44 次；2002：3 → 75 → 121）。没有丢写，也没有重复应用；A 与 B 从未同时持有锁。

**异常退出与回退**：

1. B 服务、写入中直接 `kill -9`（投影跟得上时 `wal-drained` 仍通过，这一次正是如此）。在原 WAL 目录重启 B：等旧锁过期约 12 秒后就绪；正常停机后再迁回 A（A 的旧 WAL 目录已全部确认），写入继续：确认数量从 B 的最后值接着加 1，最终 Mongo 等于最后确认（86 / 85 / 85 / 85），`_version` 增量等于确认次数。
2. 为了得到“未排空”的状态：写入中对三个私有 mongod 发 SIGSTOP 让投影停住 2 秒，期间 `kill -9` B，再 SIGCONT。`wal-drained` B：`drained: false`，checkpoint `segment 1 offset 32760`、最后一条记录 `offset 37310`，退出码非零。此时 Mongo 中的数量（94 / 93 / 93 / 92）落后于已确认给业务的数量（各 99）——**如果此刻在新机器用空 WAL 接管，这 5～7 次已确认的写入会丢失**。在原 WAL 目录重启 B 后正常停机，`wal-drained` 通过，Mongo 等于 99 / 99 / 99 / 99，`_version` 增量等于确认次数。

**边界**：两台“机器”是同机两个进程，没有验证真实跨机网络、磁盘卷迁移与 DNS / 负载均衡切换；写入负载经 ops GM 命令而不是玩家 TCP 连接，客户端重连路径没有演练；只演练了 sid 1000 一个 game 进程。演练脚本与日志在本次会话的临时目录，未入库；本节记录的是其中的数字。

## 5. 改 DAO 库名映射

DAO 标记的 `db=` 是逻辑库名，物理库名由 `dataengine.databases`（或 `kitdataengine.WithDatabaseResolver`）决定（[DESIGN-TODO D9](../maintenance/DESIGN-TODO.md#d9-dao-库名不随配置_id-是编码后的-entityid) 修订版 A，[设计与使用](guide/03-dataengine.md#3-投影与幂等)）。WAL 只记逻辑名、投影时才换算，所以**改映射等于把数据搬到另一个库**，与搬机同一套停写纪律：

| 步骤 | 做什么 | 检查点 |
| --- | --- | --- |
| 1 停写 | 用**原映射**正常停机所有写这些逻辑库的进程（含跨进程写 Remote 实体的） | 进程已退出、单实例锁已释放（`sidmove singleton`） |
| 2 排空 WAL | 停机时 DataEngine 把 WAL 全部投影确认；异常退出的进程先用原映射在原 WAL 目录重启恢复、再正常停机 | 每个 sid `sidmove wal-drained`：`drained: true`，报告的 `databases` 是原映射 |
| 3 迁移数据 | 离线把旧物理库的集合复制到新物理库（`mongodump` / `mongorestore --nsFrom/--nsTo`，或 `$out`），核对文档数与 `_version` | 新库文档数、抽样 `_version` / `_last_tx` 与旧库一致；`sidmove players -db <新库>` 与 `-db <旧库>` 的快照相同 |
| 4 改映射启动 | 改所有进程的 `dataengine.databases`，再启动 | 启动日志 `dataengine mod: database mapping logical=… physical=<新库>`；WAL 目录的 `databases.json` 更新为新映射 |
| 5 回退 | 新进程写入之前：改回原映射启动即可（旧库未动）。写入之后：只能向前修复或停写后再搬一次 | — |

**框架守住的部分**：还有未投影记录而映射变了的进程拒绝启动（`dataengine: database mapping changed while the WAL still holds unprojected records`，点名哪个逻辑库从哪个物理库变到哪个），记下的映射不被覆盖；用原映射重启即可恢复。没有 `databases.json` 的 WAL（引入映射之前写下的）按“物理名就是逻辑名”比较。**框架不守的部分**：第 3 步的数据复制与核对、多进程映射是否一致、已经排空后改映射却忘了迁移数据（新库是空的：冷加载得到“从未创建”，GetOrCreate 会新建玩家）——这些由运维流程保证。

## 6. 合服：把服 A 并入服 B

别名合服（[DESIGN-TODO D15](../maintenance/DESIGN-TODO.md#d15-合服把一个服并入另一个服)，设计与实施记录见 [FEATURE-2026-10-20-server-merge-alias](../maintenance/FEATURE-2026-10-20-server-merge-alias.md)）：A 的角色身份（PlayerID、`Role.ServerID`、slot、名字）一律不变，account 在 A 的服务器记录上写 `MergedInto = B`，此后会话里的承载服（`Role.ServingServerID`、`ServerIDClaim`）是 B，Gate 与嵌入 TCP 都把 A 的角色连到 B 的 game；B 的 game 进程、配置与代码不变，进程仍只认自己的 sid。原服在 `OriginServerIDClaim`，显示规则由业务决定。

合服是停服窗口内的**离线**操作，A、B 都停机。工具：game-demo 的 `cmd/accountctl`（account 记录）与 `cmd/sidmerge`（跟着 sid 走的数据，逻辑在 [wiring/sidmerge](../../wiring/sidmerge/sidmerge.go)）；`roost project doctor` 的 `merge-readiness` 只读打印同一份清单。

### 6.1 步骤

| 步骤 | 做什么 | 检查点 |
| --- | --- | --- |
| 0 准备 | 读清单：`go run ./cmd/sidmerge checklist`（按 sid 分库的逻辑库要搬，共享逻辑库两侧必须同一位置，持久总线消息，需要业务语义的合并）。业务按 §6.3 准备自己的合并脚本 | 清单里每一项有负责人 |
| 1 公告、停准入 | A、B 的 account 状态改 maintenance（`accountctl upsert-server -status maintenance`），接入层停止接受新连接 | — |
| 2 停服 | A、B 的 game 进程正常停机（SIGTERM，等进程退出）；之前先按清单的 `bus` 项处理发往 A 的持久消息（可靠总线的死信、JetStream RPC 的实例 durable） | 进程退出；`sidmove singleton` 两把锁都已释放 |
| 3 前置检查 | `go run ./cmd/sidmerge migrate -from A -into B [-from-config …] [-into-config …] -dry-run`：两侧 WAL 已排空（且配置的库名映射与该进程最后一次运行记下的相同）、每个非按 sid 的逻辑库两侧在同一 Mongo 的同一物理库、A 的 outbox 为空或两侧共用控制库、saga 全部终态、按 sid 分库的 `_id` 冲突扫描；持久总线清单要人工确认（`confirm`） | 报告 `ready: true`，`confirm` 项已逐条确认。WAL 不在本机时在那台机器跑 `sidmove wal-drained`，通过后加 `-from-wal-checked` / `-into-wal-checked` |
| 4 抽样快照 | `sidmove players -ids <A 的抽样玩家> -out before.json` | — |
| 5 迁移按 sid 独占的数据 | `go run ./cmd/sidmerge migrate -from A -into B -manifest merge-A.json`：A 的 `<物理名A>_<A>` 逐集合原样拷进 B 的 `<物理名B>_<B>`（两侧各按自己的 `dataengine.databases` 换算），带 `_version` / `_schema` / `_last_tx` 与墓碑字段；冲突整体停止、不覆盖不跳过；重跑幂等。没有 `dbscope=sid` DAO 的工程（game-demo）这一步什么也不写 | 报告 `copied` / `already_present`；清单文件保存好（回退要用）。源库保留为只读归档，确认无误后由运维手动删除 |
| 6 业务语义合并 | 业务脚本（§6.3） | 业务自己的核对 |
| 7 account 记录 | `accountctl merge-server -from A -into B -note "<窗口、工单>"`（先 `-dry-run` 看会改哪些记录）：A 写 `MergedInto = B`、`closed`；原来并入 A 的服一并改指 B（拍平） | `accountctl show-server -sid A`：`merged_into: B`；`show-server -sid B` 的 `merged_from` 含 A |
| 8 部署层 | 服务器列表显示“A（已并入 B）”并给出 B 的接入地址；嵌入 TCP 部署把 A 的接入域名 / 端口指到 B；独立 Gate 不需要配置 | — |
| 9 启动 B | 启动 game(B)，开放登录前 `sidmove players -compare before.json`（Player 等共享库文档未变），按清单核对迁入文档 | `/readyz` 200；比较通过 |
| 10 开放登录 | B 的状态改回 open。之后不再部署 game(A) | A 的角色登录到 B；`CreateRole` 在 A 上被拒（`server … is merged into B`） |

**误启动 A**：装配了 account 的 game 进程启动时由 `ServingGuardMod`（[wiring/account](../../wiring/account/serving_guard.go)）向 account 查本服记录，`MergedInto != 0` 拒绝启动并点名承载服；它排在 DataEngine Mod 之前（DataEngine 把它列为可选依赖），所以 WAL 与 outbox 都还没打开。account 一直查不到时在 30 秒内重试，仍查不到也拒绝启动（无法证明没被并入就不写数据）。本服还没在 account 登记时放行。

### 6.2 回退

1. 停服窗口内停 game(B)，`sidmove wal-drained` 确认排空。
2. `go run ./cmd/sidmerge revert -from A -into B -manifest merge-A.json`：先核对（B 已排空、清单记的库名与按配置重新换算的相同、清单上每份文档都在 B 里），再把每份文档按 B 里的**当前版本**写回 A 的源库、按版本从 B 删掉。合服期间 B 里新产生、不在清单上的按 sid 独占文档不动，报告 `new_in_target` 计数：框架无法判断归属，由业务按自己的键规则处理，否则留在 B。
3. `accountctl unmerge-server -from A -note …`：清空别名，A 保持 closed；拍平时一并改指 B 的其他服不受影响。
4. 用**新的** WAL 目录部署 game(A)（旧目录已排空，可清掉），`accountctl upsert-server -sid A -status open`，接入层改回；启动 B。

没有按 sid 独占数据的工程只有第 1、3、4 步。中途失败：revert 是逐份“写回 A、按版本删 B”，已搬回的文档在 A、不在 B，原样重跑会把它们报成 missing 并整体拒绝——对照报告去掉清单里已搬回的项再跑。

### 6.3 持久总线消息与业务合并清单

`sidmerge` 按 A 的配置列出下列总线用法（逐项核对过源码，`checklist` / `migrate -dry-run` 的 `bus` 段）。发往 A 的消息只有标为“按 sid 寻址”的会因 A 退役而无人消费：

| 用法 | 持久？ | 按 sid 寻址？ | A 退役后 | 处理 |
| --- | --- | --- | --- | --- |
| DataEngine 效果流 `dataengine.effects.stream` | 是 | 否：共享 durable（game-demo `game-level-up-mail`、`game-activity-phase`，saga 的 `roost-saga-start` / `-result`） | 已发布的由 B 消费；A 暂存未发布的留在 outbox | outbox 检查 |
| saga 流 `saga.stream` | 是 | 否：共享 durable（`game-gift-<step>`），但命令载荷可能带发起方 sid（赠礼 `FromSID`），只有那个 sid 的进程接纳 | 指向 A 的非终态 saga 永远推进不了 | saga 全部终态（检查） |
| SyncBus（`syncbus.transport: jetstream`） | 是 | 是：每个主题每个 sid 一个 durable `sync_<topic>_<sid>_<hash>`（及 `.live`） | 广播副本无人消费，到流的 max age 丢弃；B 有自己的 durable，不丢只发给 A 的东西 | 合服后删 A 的 durable（`nats consumer ls <流> \| grep _A_`） |
| 服务 RPC（`nats.rpc.transport: jetstream`） | 是 | 是：请求流里的实例 durable `rpc_<服务>_<sid>_<方法>`（game-demo 的赠礼步骤转交发往 `svc.game.<sid>`） | 发往 A 的请求等到 request TTL，调用方超时；不会重投给 B | 停服前确认 A 的实例 durable 没有积压，合服后删除。缺省 `core` 传输不落盘 |
| 可靠总线（`nats.reliable.enabled`） | 是（Redis） | 是：去重收件箱与死信按消费者 `<服务>:<sid>` | A 的死信无人重放 | 停 A 前用 admin `bus.dlq.*` 列出，重放到 B 或丢弃（业务决定）。缺省关闭 |
| Remote 快照兴趣 | 是（Redis） | 是：消费者 sid | 按 `remote_entity.snapshot_interest_ttl` 过期 | 无 |

需要业务语义的合并（框架只列出，不实现）：World 单例（UniqueID = sid，合服后只加载 World(B)，World(A) 的计数 / 进度并入或丢弃）、按服排行榜（并存或重算）、活动组 `activity.groups_file`（移除 A）、跨服 global `Routing`（解除 A 的绑定）、按服唯一的名字（改名规则）、按服的邮件 / 聊天频道、业务数据里记着的 sid。

### 6.4 边界

- `sidmerge` 不连 NATS：持久总线一项只能列出、由运维确认，不自动检查积压。
- “同一 Mongo” 按 URI 的主机列表 + 副本集名比较：同一集群换主机名写会被判为不同而拒绝（偏保守）。
- saga 记录不带“属于哪个 sid”，检查的是整个 saga 存储：同一存储里还在运行的其他服的 saga 也会让检查不通过。
- 剩余边界与尚未确认项见 [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#合服d15)。
