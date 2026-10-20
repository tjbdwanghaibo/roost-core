# 静态 Player 跨机迁移参考流程

本篇是把一个 game sid（连同绑定在它上面的全部静态 Player）从一台机器搬到另一台的参考流程，维护者 2026-10-15 决定按此执行（[DESIGN-TODO D5](../maintenance/DESIGN-TODO.md#d5-静态-player-跨机迁移的参考流程)）。每一步写明用到的现有能力与检查点；检查工具是 game-demo 模板里的 [cmd/sidmove](../../demo/cmd/sidmove/main.go.tmpl)。框架级的“迁移 Player”命令（ops 管理面的 drain-and-move，自动执行并处理部分完成）是上线后评估的候选，尚未实施。

## 1. 先说清“迁移”的对象

静态绑定下，玩家建角时由 account 绑定到一个 game sid（`Role.ServerID`，[service/account/types.go](../../service/account/types.go)），之后不变；同一 sid 同一时刻只有一个进程（App 单实例锁），game 进程只为绑定在自己 sid 上的玩家服务（demo 的 [playerowner.go](../../demo/internal/service/game/playerowner.go.tmpl)）。因此“同一玩家至多一个写者”就等于“同一 sid 至多一个进程”。

所以跨机迁移搬的是 **sid**：旧机器上的 sid 进程停下，新机器上同一 sid 的进程接管，玩家仍绑定在原 sid。框架**没有**把单个玩家改绑到另一个 sid 的能力（account 没有修改 `ServerID` 的接口，`PlayerOwners` 拒绝不属于本 sid 的玩家）；需要“合服 / 单个玩家换服”属于新的产品功能，不在本篇范围。

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

