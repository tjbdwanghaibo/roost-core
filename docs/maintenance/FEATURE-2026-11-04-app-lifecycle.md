# 功能设计：App 生命周期——停机先关外部输入（2026-11-04）

**状态：已按推荐实施（维护者 2026-11-04：“同一个问题：App 的生命周期管理”，要求整体设计、补齐欠缺；方案里需拍板的点按推荐先做，列在“待决定”）。** 实施与验证记录在第 8 节。

基线 `afd61beb`（origin/main）。codebase-memory 本轮连不上，第 1 节的现状全部按当前源码与 `rg` 核对，引用写到文件:行（行号按基线）。

## 0. 问题

上一轮（`4947c62a`）修“停机窗口里路由 RPC 答成结果未知”：总线新增 `(*bus.Bus).WithdrawRpc`，生成的 owner Mod 与 game-demo alliance Mod 在 `StopWithContext` 里先撤 handler。留下的边界（KNOWN-LIMITS“联盟服务冒烟里的两条观察”）维护者认为是同一个问题：

1. 启动中（`Service.Init` 还没返回）收到停机信号，App 不停 Mod 直接退出；handler 已在 Mod Start 时注册，在途请求得不到答复、调用方超时。
2. 独立进程的生成 `Server`（`Service.Init` 注册 handler，`Shutdown` 不撤）与框架内置服务的 Mod（`Stop` 不撤）停机不先撤 handler；App 先调 `Service.Shutdown` 再停 Mod，之后 handler 仍可能用到已停的依赖。
3. 等在途请求的时间算在 owner Mod 的停机份额里（生成器不为它声明预算，3s 保底），超过即 App 不再停后面的 Mod。
4. Bus 停止时还没撤的方法被 `refuseTask` 以 `errRPCNotAdmitted` 拒掉，`errcode.ClientError` 把它折成 `CodeInternal`，与“确定没执行”的注释意图不符。

根子是同一个：**“停止接收外部输入并排空”没有一个统一的位置**。现在它要么靠 Mod 依赖序碰巧成立（输入的拥有者恰好排在它用的依赖之上），要么靠每个 Mod / 生成模板自己记得撤，要么根本没有。按“单实例 / 存活 / fail-stop 这类保证在 core App 里做一次，模块继承”的原则，收进 App。

## 1. 盘点（当前源码）

### 1.1 App 生命周期

`framework/app/app.go` 的 `run`（224～348）：读配置与声明检查 → 日志 → **从这里开始接收 SIGINT / SIGTERM**（275～277，`exitSignals`）→ `initRegistry`（发布 `app.init`）→ 单实例锁（293～309，等锁期间收到信号直接 `return nil`）→ 业务时间高水位 → 共享 Mod 排序与 `Init → Provide → Start`（`startSharedMods` 405～446）→ 服务专属 Mod 同样三步（`startServiceMods` 470～517）→ `mods.started` → `Service.Init` 与 `service.started`（`initService` 545～582）→ `serveAndShutdown`（586～596）。

- **Mod 顺序**：`sortMods`（1069～1168）按 `DependsOn` / `OptionalDependsOn` 拓扑排序，同层保持登记顺序；共享 Mod 全部先于服务专属 Mod 启动。停机逆序：先服务专属，再共享。
- **启动阶段等待**：每一步经 `runStartupStep`（`startup.go` 13～37）在 `startup.timeout`（缺省 2m）内等待，`select` 同时等信号（35 行）。**超时或收到信号都返回 `finished=false`，调用方一律直接 `return err`**（406～411、419～422、435～438、473～476、484～487、504～507、525～534、547～550、557～566）：不停任何 Mod、不释放单实例锁，交给进程退出。阶段本身（Mod 的 Init / Start、`Service.Init` 都不收 ctx）仍在后台跑。
- **启动失败**：步骤返回错误时逆序停已 Provide 的 Mod（`stopModsReverse`，无总截止、每个 Mod 缺省 5s，795～804），`Service.Init` 失败或之后的检查点失败先调一次 `Service.Shutdown`（限时 5s，`shutdownAfterStartupFailure` 762～793），收不回来就保留 Mod 与锁。
- **运行期 fail-stop**：`RuntimeFailure`（`runtime_failure.go`）首次失败先同步执行 `OnFail` 回调（Nest Mod 登记的 `NestMgr.Fence`），再唤醒 `waitForStop`（599～615），走同一条优雅停机。启动期间只在步骤之间查 `Err()`（`startupFailure` 382～388），步骤进行中发生的失败要等这一步结束。
- **优雅停机**（`shutdown` 620～681）：`shutdown.total_timeout`（缺省 30s，`config_schema.go` 298～300）一个窗口 → 发布 `service.stopping`（ops 把 readiness 置假，`wiring/ops/ops_mod.go` 117）→ 取消 Serve 的 ctx 并调 `Service.Shutdown`，等两者返回（`awaitServeAndShutdown` 704～728）→ 逆序停服务专属 Mod（`stopModsReverseBefore` 带上之后共享 Mod 的声明预算）→ 逆序停共享 Mod → 发布 `service.stopped`。启用单实例锁时 Mod 的截止时间比总截止提前 `min(3s, total/2)` 留给 Release（`modStopContext` 688～699）。
- **停机预算**：每停一个 Mod 按剩余时间重新规划（`modStopBudget` 904～948）：未声明的 Mod 3s 保底，声明了 `StopBudget` 的拿声明值，不够时按比例缩放或均分；`stop_budget_test.go` 覆盖。生成器按同一公式写 `total_timeout`（`codegen/internal/roost/shutdown_budget.go` 17～36：声明值 + 3s × 未声明 Mod 数 + 5s 给 `Service.Shutdown` 的余量）。
- **任何一步在预算内没结束**（`service.stopping` hook、`Service.Shutdown`、Serve、某个 Mod 的 `StopWithContext` 返回 ctx 错误）：直接返回，后面的依赖与单实例锁保留到进程退出（637～649、653～660）。
- **停机期间再来信号**：没人读，进程在预算内自己退出。

### 1.2 接收外部输入的入口

| 入口 | 谁注册 | 谁撤下、在停机序列哪一步 | 撤之前它用的依赖是否还活着 | 缺口 |
| --- | --- | --- | --- | --- |
| 总线轻量 RPC handler（`HandleRpc`） | 生成的 owner Mod 在 `Start` 注册（`codegen/internal/roost/add_rpc.go`、`add_rpc_entity.go` 的 `renderEntityRPCMod`）；独立进程的生成 `Server` 在 `Service.Init` 注册（`codegen/internal/servicerpc/template.go` 536～555，例：`wiring/mail/mail_rpc_assembly_gen.go` 129）；`wiring/physicsroom/server.go` 同形 | owner Mod：`StopWithContext` 调 `WithdrawRpc`（`4947c62a`），位于逆序停 Mod 时它自己那一步。生成 `Server`：`Shutdown` 不撤（`mail_rpc_assembly_gen.go` 164～169），内置服务 Mod 的 `Stop` 是空的（`wiring/mail/mail_mod.go` 136 等十个）。都要等到 nats Mod 停止、`Bus.StopWithContext` 退订（`bus.go` `stopResources`，直接 `Unsubscribe`，不排空） | owner Mod：是（依赖序）。`Server` / 内置服务：取决于共享 Mod 的排列——nats Mod 只可选依赖 redis，所以 redis 晚于总线停；同为共享 Mod 但排在 nats 之后的（etcd、mongo 等）先于总线停，期间 handler 用的是已停的依赖。总线停止时直接退订：服务端已经发出、还没进回调的请求丢成超时 | **缺口 2、3**；以及“撤 handler”分散在每个 owner Mod 里 |
| 总线 JetStream RPC handler | 同上（`nats.rpc.transport=jetstream`） | `WithdrawRpc` 停服务级 consumer、实例 consumer 留到 Bus 停止并答可重试；Bus 停止时 `stopJetStreamRPCRequests` 先关准入 | 同上 | 同上 |
| 总线调度池拒绝（池满 / 没在跑） | — | `dispatchTask` → `refuseTask`（`bus.go` 891～906），答 `errRPCNotAdmitted`（1212） | — | **缺口 4**：`ClientError` 折成 `CodeInternal`，调用方只能按结果未知处理 |
| 总线模块消息（`Handle`） | 业务 Service 在 Init 里经 `ownerroute.RegisterBusHandler`（game-demo 的赠礼 / 联盟 Saga 转交、联盟通知，`demo/internal/service/game/*_saga.go.tmpl`、`alliance_notice.go.tmpl`） | Bus 停止时退订基础主题、排空调度池 | 否：handler 跑 Nest 事务，Nest 在服务专属 Mod 里先于 nats Mod 停止，中间到达的转交 / 通知进已停的 Nest（转交靠 Saga 重投兜底、通知尽力投递，不丢账，但违反不变量） | **缺口 5（本轮新列）** |
| JetStream effect 消费者（`kitdataengine.SubscribeEffects`） | 业务 Service 在 Init 里订阅（game-demo `level_up_mail`、联盟三个消费者；`roost add effect` 的说明要求 Init 订阅、Shutdown `Drain()`） | 业务的 `Service.Shutdown` 调 `Drain()`（`demo/internal/service/alliance/service.go.tmpl` 72～80） | **`Drain()` 只发起、不等**（`infra/network/nats/driver/jetstream.go` 470），`Shutdown` 返回时在途 handler 还在跑，App 接着停 Nest / DataEngine | **缺口 6（本轮新列）**：在途 effect 可能落在已停的 Nest 上（nak 后重投，不丢，但违反不变量）；而且“记得 Drain”靠每个业务服务 |
| JetStream Saga 步骤消费者（`kitsaga.Steps`） | 业务 Service 在 Init 里 `Steps.Start` | 业务 `Service.Shutdown` 调 `Steps.Drain(ctx)`（`wiring/saga/steps.go` 304～330，等在途） | 是，只要业务记得调 | 靠每个业务服务记得调（同 6 的后一半） |
| Saga 协调器、syncbus、冷副本、JetStream RPC 应答 consumer | 框架 Mod 内部 | 各自 Mod 停止 | 是（依赖序） | 不是外部输入（协调器读自己的日志；应答 consumer 是出向调用的回程，必须留到 Bus 停止） |
| 网关 / 玩家 TCP 连接 | 生成的 player TCP Mod（依赖 `accessplayer`，后者依赖 `nest`，`render_player_tcp.go` 296、`render_access.go` 441）；gate 服务的 `GateMod`（依赖 nats，`wiring/gate/mod.go` 117） | 自己 Mod 的 `StopWithContext`（停 listener、等连接处理完） | 是（依赖序：它排在 Nest 之上，先停） | 无 |
| ops HTTP：`/healthz` `/readyz` `/metrics` `/statsz` | 共享 ops Mod | ops Mod 停止（`http.Server.Shutdown`） | — | 无（停机期间留着观测是对的；`service.stopping` 已把 readiness 置假） |
| ops HTTP：`/admin/execute` | 同上；命令由各 Mod / Service 登记（如联盟的 `alliance.transfer`） | ops Mod 停止 | **否**：ops 是共享 Mod、停得晚，停机期间运维命令照常执行，可能落在已停的 Nest / Remote 上（`infra/observe/ops/server.go` 284～341 没有准入门） | **缺口 7（本轮新列）** |
| 实体定时器（`wiring/entitytimer`） | 依赖 nest 的 Mod | 自己 Mod 停止 | 是（依赖序，`entitytimer.go` 37） | 无（进程内部输入） |
| `Service.Serve` 里的周期任务 | Service | `waitForStop` 之后先取消 Serve 的 ctx，再 `Service.Shutdown` | 是 | 无 |

### 1.3 启动中的输入

Mod `Start` 就会注册 handler（owner Mod），`Service.Init` 也会（生成 `Server`、effect 消费者、Saga 步骤、`Handle`）。所以从第一个这样的 Mod `Start` 返回起，进程就在接收外部输入，而启动被信号打断时（**缺口 1**）App 直接返回、进程退出：在途请求没有答复（调用方超时，结果未知），已 Provide / Start 的 Mod 没有停（连接被进程退出硬断、单实例锁等 TTL 过期）。启动失败的收尾路径（步骤返回错误）会停 Mod，但同样不先撤输入：`Service.Init` 注册了 handler 之后失败，`Service.Shutdown` 不撤，Mod 依次停下时 handler 仍在接请求。

## 2. 目标不变量

> **任何外部输入，在它用到的依赖停止之前，已经关闭准入并排空。** 关闭之后到达的请求得到确定的“没执行、可重试”（`errcode.CodeRetryable`）或 no responders；关闭之前已经接受的，答完再往下走。

App 用一个统一的步骤保证它：**停机的第一步（`service.stopping` 之后、`Service.Shutdown` 与任何 Mod 停止之前），App 关闭全部外部输入的准入并在停机预算内等它们排空。** 之后才停业务（`Service.Shutdown`）与基础设施（Mod 逆序停止）。排空在预算内没完成，与 `Service.Shutdown` 没完成同样处理：不停 Mod、不释放单实例锁。

所有停机路径走同一套：

- 运行中收到信号 / fail-stop / Serve 自行退出：已有的优雅停机，前面加这一步。
- **启动中收到信号**：同一套有序停机，只停已经开始的部分——先关已启动部分的输入，再在停机预算内等正在跑的启动步骤结束（`Service.Init`、某个 Mod 的 Start），结束后它也算已启动；然后 `Service.Shutdown`（若 Init 已返回）、逆序停已 Provide 的 Mod、释放锁。步骤在预算内不结束，保留依赖与锁直接返回（与现在相同）。
- 启动失败（步骤返回错误、启动检查点发现 fail-stop）：停 Mod 之前同样先关输入。
- 启动超时（`startup.timeout`）：先关输入（在途答完），不再等那个已经超时的步骤，保留依赖与锁返回（见待决定 D2）。

依赖序仍然有效：玩家 TCP、网关、实体定时器这类“拥有者排在依赖之上”的输入照旧由各自 Mod 在逆序停止时关闭，不挪到第一步（D3）。

## 3. 设计

### 3.1 App：`IngressCloser` 与停机第一步

`framework/app` 新增（`ingress.go`）：

```go
// IngressCloser 由拥有外部输入的 Mod / Service 实现（或经 AddIngress 登记）。
// CloseIngress 关准入（幂等）→ 在 ctx 内等已接受的处理完；ctx 先结束返回 ctx 错误，可用新 ctx 再调。
// 只停“接收”，出向能力（发消息、调别的服务）与对象本身的资源留到它的 Stop。
type IngressCloser interface{ CloseIngress(ctx context.Context) error }

// AddIngress 登记运行中建立的输入（Service.Init 里订阅的 JetStream 消费者等），App 在停机第一步关闭它。
func AddIngress(r *Registry, name string, closer IngressCloser) error
```

停机第一步 `closeIngress`：对已 Start 的共享与服务专属 Mod 中实现了 `IngressCloser` 的、Init 已返回且实现了它的 Service、以及 Registry 里 `AddIngress` 登记的，**并发**调用 `CloseIngress`（各自 recover），在停机 ctx 内等全部返回。并发是为了让各入口几乎同时关准入，排空时间取最长的一个，不是相加。任一返回 ctx 错误或没在期限内返回 → 停机不完整，后面不再做。普通错误并入返回值、继续。

### 3.2 各入口在新序列里的位置

| 入口 | 实现 | 位置 |
| --- | --- | --- |
| 总线 RPC（轻量 / JetStream）与模块消息 | `(*bus.Bus).CloseIngress`：对**全部**已注册方法做 `WithdrawRpc`（关准入答 `CodeRetryable` → 排空并退订 / 停服务级 consumer → 等在途），排空并退订四个基础主题、等已接收的模块消息处理完；之后 `HandleRpc` 拒绝。出向（`Send`、`Call`、JetStream RPC 应答 consumer）照常，直到 Bus 停止。`NatsMod.CloseIngress` 转调 | 第一步 |
| ops `/admin/execute` | `(*ops.Server).CloseIngress`：准入门（`operation.Lifetime`），关闭后答 503 “service stopping; command not executed”，等在途命令；健康、就绪、指标照常。`OpsMod` 内嵌它，自动实现 | 第一步 |
| effect 消费者 | `SubscribeEffects` 订阅后 `app.AddIngress`：`Drain()` 并在 ctx 内等 `Closed()`。业务 `Shutdown` 里的 `Drain()` 变成空操作，可以留着 | 第一步 |
| Saga 步骤消费者 | `Steps.Start` 成功后 `app.AddIngress(Steps)`（`Steps.Drain(ctx)` 本来就是关准入 + 等在途） | 第一步 |
| 玩家 TCP、网关、实体定时器 | 不变（依赖序） | 各自 Mod 停止 |
| `Service` 自己的输入 | Service 实现 `IngressCloser` 即可（可选） | 第一步 |

### 3.3 预算

不加配置键。第一步用 `shutdown.total_timeout` 的同一个停机 ctx；排空完后 Mod 的停机预算按剩余时间照旧规划（`modStopBudget` 每停一个 Mod 重新按剩余时间算）。理由：

- 排空不完成本来就不能停依赖（在途 handler 还在用），所以给它单独一个更短的上限没有意义：超了也只能保留依赖返回。
- 正常情况下排空很快（毫秒级，等的是已进 handler 的请求，上限是 `nest.request_timeout` 3s / 生成的 `HandleBudget` / effect 的 AckWait / `ops.admin_timeout`）。生成器的 `total_timeout` 公式里给 `Service.Shutdown` 的 5s 余量与每个 Mod 的 3s 保底留了这段时间；owner Mod 不再在自己的份额里等在途，它那 3s 保底变成余量。
- 缺口 3 因此消失：等在途不再占 owner Mod 的份额。

生成器公式不变，只更新注释（排空也花在这 5s 余量里）。运维命令跑得久（`ops.admin_timeout` 缺省 10s）时，停机第一步可能等它到 10s，挤掉后面 Mod 的时间——这是有意的：命令还在用 Nest / Remote。

### 3.4 与 `WithdrawRpc` / owner Mod `StopWithContext` 的关系

- `WithdrawRpc` 保留，是 `Bus.CloseIngress` 的实现，也继续给按方法撤下的场景用（缩容时先撤某个方法）；对已撤下的方法重复调用返回 nil。
- 生成的两种 owner Mod 与 game-demo alliance Mod **删掉 `StopWithContext` 与 `bus` 字段**，`Stop` 回到空操作：App 在第一步已经撤下全部 handler。已生成工程里留着它也无害（第二次撤下是空操作）。
- 生成的 `Server`、框架内置服务的 Mod 不需要改：它们的 handler 由 nats Mod 的 `CloseIngress` 撤下。
- 缺口 4：`refuseTask` 的答复包进 `errcode.ErrRetryable`（池满、调度器没在跑都是确定没执行）。

### 3.5 启动中的有序停机（缺口 1）

`runStartupStep` 被信号打断时把“正在跑的那一步”交还给调用方（结果通道）。打断后的统一收尾：

1. 停机 ctx（`total_timeout`）开始计时；关已 Start 的 Mod（和 Registry 登记的）的输入。
2. 在停机 ctx 内等那一步返回；返回后按它是什么补登记：Mod 的 Provide / Start 返回了就算已 Provide / 已 Start（不管成败，Provide / Start 失败本来也要停它），`Service.Init` 返回了就算 Service 已初始化。
3. 再关一次输入（这一步期间新启动的 Mod、新登记的输入，`CloseIngress` 幂等）。之后 `HandleRpc` 被总线拒绝，所以正在跑的 `Service.Init` 注册不上新 handler（返回错误，也就结束了）。
4. Service 已初始化 → `Service.Shutdown`；然后逆序停已 Provide 的服务专属 Mod、共享 Mod（与正常停机同一套预算规划）；全部停完才释放单实例锁。
5. 第 2～4 步任何一步没在停机 ctx 内结束 → 保留依赖与锁返回（现规则）。

不发 `service.stopping` / `service.stopped`（`service.started` 没发过）。返回值仍是带 `context.Canceled` 的“startup interrupted”错误，并入收尾错误。

### 3.6 兼容性

线上未部署，不做旧进程兼容。行为变化：

- 停机多了第一步，Mod 拿到的时间是排空之后剩下的。
- 启动中收到信号不再立即退出：最长等到 `total_timeout`（部署宽限期已按 `total_timeout + 5s` 生成，覆盖得住）。
- 总线调度池拒绝的 RPC 由 `CodeInternal` 变为 `CodeRetryable`。
- ops 停机期间的 `/admin/execute` 答 503。
- `Bus.CloseIngress` 之后 `HandleRpc` 返回错误。

已生成工程：不需要手改即可得到新行为（nats / ops / effect / Steps 都在框架里）。可选清理写进 CHANGELOG：`internal/rpc/<name>/mod.go` 里 `4947c62a` 起生成的 `StopWithContext`（与 `bus` 字段）可以删掉；`Service.Shutdown` 里的 effect `Drain()` 可以删掉也可以留着。

## 4. 待决定（已按推荐实施）

| # | 问题 | 选项 | 推荐 |
| --- | --- | --- | --- |
| D1 | 排空的预算 | A 用 `total_timeout` 同一个窗口、不加键（3.3）；B 新键 `shutdown.drain_timeout`（缺省 5s），生成器把它计进 `total_timeout` | **A**：排空没完成本来就不能停依赖，单独上限只是提前放弃；少一个键、生成器与 doctor 不用改 |
| D2 | 启动**超时**（不是信号）时要不要也等正在跑的步骤 | A 只关输入、不等，保留依赖与锁返回（现状 + 关输入）；B 与信号相同，再等最多 `total_timeout` | **A**：步骤已经超过自己的预算，多半卡住，再等一个停机窗口只会推迟失败退出；关输入让在途请求先答完 |
| D3 | 玩家 TCP / 网关连接要不要也挪到第一步 | A 维持依赖序（Mod 自己停）；B 生成的 player TCP Mod 实现 `CloseIngress`，第一步就断开玩家 | **A**：它们排在依赖之上，在途答完才停依赖已经成立；挪到第一步只会让玩家在 `Service.Shutdown` 期间提前断线 |
| D4 | 生成的 owner Mod 的 `StopWithContext` | A 删掉，App 统一做；B 保留作第二道 | **A**（维护者要求“能删就删”）；已生成工程留着无害 |
| D5 | 停机后到达的模块消息（`Handle`） | A 排空基础主题订阅、等已接收的处理完，之后的消息不再接收（核心 NATS 不持久，等于进程退出后的消息）；B 不动消息、只撤 RPC | **A**：消息 handler 同样跑在 Nest 上；转交 / 通知本来就靠重投 / 登录补齐兜底 |

## 5. 未采用的方案

- **每个 Mod / 模板自己撤**（`4947c62a` 的路子推广到生成 `Server`、十个内置服务 Mod、effect 模板）：同一不变量分散在几十处，下一个新入口还会漏；违反“App 级统一保证”。
- **改 Mod 停止顺序**（把 nats Mod 排到最先停）：总线也是出向通道，`Service.Shutdown` 与 Mod 停止（DataEngine 刷投影、Remote 释放、Saga）都可能要发消息 / 调用；而且只解决总线一个入口。
- **在 Nest 停止前加一层通用“请求门”**：只覆盖经 Nest 的 handler，直接访问 Redis / Mongo 的内置服务 handler 不经 Nest。
- **启动中收到信号时直接取消步骤**：Mod 的 `Init` / `Start` 与 `Service.Init` 都不收 ctx，App 杀不掉它们；只能等。

## 6. 剩余边界（实施后）

- 集群里退订兴趣的传播窗口（KNOWN-LIMITS 已有）不变：撤下发生得更早，窗口挪到停机第一步。
- 启动步骤进行中发生的 fail-stop 仍要等这一步结束才被看到（`startupFailure` 只在步骤之间查）。
- 依赖序入口（D3）不在第一步关闭；新增的外部输入若既不在总线、ops、effect / Steps 上，也不在依赖序上方，需要实现 `IngressCloser` 或 `AddIngress`，否则不受这条保证。
- 业务在 `Service.Init` 里自己用 `IJetStream.Subscribe` 建的消费者（不经 `SubscribeEffects` / `Steps`，如 game-demo 活动阶段消费者）不自动登记：由业务的 `Shutdown` 负责，或自己 `AddIngress`。

## 7. 实施与验证计划

1. 方案单独提交。
2. 缺口 4（`refuseTask`）→ 缺口 2/3/5（App 第一步 + Bus / NatsMod）→ 缺口 7（ops）→ 缺口 6（effect / Steps 登记）→ 缺口 1（启动中信号）→ 删除生成模板里的 owner Mod 撤下代码。每个缺口先在基线代码上写出确实变红的回归。
3. 真实进程：`scripts/mirror-local.sh` 私有依赖 + 自起 etcd，重新生成 game-demo，alliance 两实例与 mail，各自在 Init 中 / 运行中发 SIGINT，持续打请求，按 ok / retryable / no responders / internal / 超时 统计修前修后。

## 8. 实施记录

每个缺口先在基线上写出变红的回归（红的文本原样记在对应提交信息里），再改到绿；每个提交单独可编译。

| 提交 | 缺口 | 改动 | 修前变红的回归 |
| --- | --- | --- | --- |
| `aa55e801` | 4 | `refuseTask` 答 `errcode.CodeRetryable` | `infra/network/bus` `TestRefusedRPCIsAnsweredAtOnceWhenTheDispatcherIsNotRunning` / `…WhenTheQueueIsFull`：答复是 `server error (code 1)`（`CodeInternal`） |
| `4f2a0708` | 2、3、5 | App 停机第一步 `closeIngress`（`ingress.go`）；`(*bus.Bus).CloseIngress`（撤全部 RPC 方法 + 排空模块消息）；`NatsMod.CloseIngress` | `framework/app` `TestRequestsAfterShutdownBeginsNeverReachTheHandler`（`Service.Shutdown` 期间的请求答 ok、存储 Mod 停止时答 `internal: store stopped`）、`TestWaitingForInFlightRequestsIsNotChargedToOneModsShare`（owner Mod 份额耗尽，`mod owner stop: context deadline exceeded`，后面的 Mod 没停）；`TestCloseIngressClosesEveryInputOfTheBus`、`TestNatsModClosesTheBusInputsButKeepsSending`（接口不存在） |
| `dde90b7e` | 7 | `ops.Server.CloseIngress`，之后 `/admin/execute` 答 503 | `infra/observe/ops` `TestCloseIngressRefusesAdminCommandsAndWaitsForTheRunningOne`（接口不存在） |
| `f97df246` | 6 | `SubscribeEffects` 与 `Steps.Start` 登记 `AddIngress`，关闭时 Drain 并等订阅关闭；`CloseRegisteredIngress` | `wiring/dataengine` `TestSubscribeEffectsIsClosedWithTheAppsInputs`、`wiring/saga` `TestStepsAreClosedWithTheAppsInputs`：App 关完输入时 `drains=0` |
| `9c7c8c5f` | 1 | `runStartupStep` 交还没等到的步骤；`stopInterruptedStartup` / `cleanupAfterStartupFailure` | `framework/app` `TestSignalDuringServiceInitStopsInOrderAndAnswersInFlightRequests`（信号后立即返回，在途请求没答、没 `Service.Shutdown`、没停 Mod）、`TestStartupTimeoutClosesTheInputsWithoutWaitingForTheStep`（超时后的请求答 ok）、`TestSignalDuringModStartStopsWhatStartedOnceTheStartReturns`（Mod Start 还在跑就返回） |
| `109d673c` | D4 | 删掉 `roost add rpc` 两种 owner Mod 与 game-demo alliance 模板里的 `StopWithContext` 撤下代码；生成工程回归改名 `TestGeneratedRPCOwnerHandlersAreWithdrawnWhenTheAppStops`（先 `CloseIngress` 再停 Mod） | （删除既有代码，回归改为按 App 的顺序驱动） |

**真实进程**（私有依赖 `scripts/mirror-local.sh`：三节点 NATS、Mongo 副本集、Redis；自起 etcd；偏移改用 33500，因为 33000 的 NATS / Redis 端口被本机别的环境占着）。用当前生成器重新生成 game-demo（修前取基线 `afd61beb`，它已带 `4947c62a` 的 owner Mod 撤下；修后取本分支），两个 alliance 实例（sid 3000、3001）或两个 mail 实例，持续打请求时对其中一个发 SIGINT，客户端按 `servicerpc.Classify` 与传输错误分类计数。alliance 的“Init 中”场景给 `Service.Init` 加了冒烟专用的 4s 睡眠（模拟 Init 里建消费者的几秒，补丁只在冒烟工程里），owner Mod Start 已登记 handler，睡眠期间 SIGINT。客户端请求期限 3s，NATS 地址是三个节点的列表（服务端与客户端各自随机连一个，会跨节点）。

| 场景（次数） | 修前 | 修后 |
| --- | --- | --- |
| alliance 运行中停（15 / 15） | internal 0、超时 0；可重试 5～10 / 次 | internal 0；可重试 7～10 / 次；**15 次里 2 次超时**（一次 5 个 Found、一次 1 个 Info） |
| alliance Init 中停（5 / 5） | **每次 12 个 Info 超时**（12 个并发请求全部挂到进程退出）；进程 9～95ms 就退出 | 超时 0、internal 0；每次 10～11 个可重试；进程等 Init 返回后按顺序停，0.5～4.5s 退出 |
| mail 运行中停（5 / 5） | **internal 共 4 个（1 次）、超时共 6 个（3 次）** | internal 0、超时 0；可重试 2～39 / 次 |

no responders（发往已停实例 sid 的路由读）两边都有，是停下之后的正常答复，不计入。修后 alliance 运行中的 2 次超时属于 KNOWN-LIMITS“App 停机关输入后的边界”①（集群里退订兴趣的传播窗口）：日志里关输入分别在 32ms / 236ms 内完成、之后没有错误，按现象推断是退订之后经别的节点转来的请求被丢（没有逐个请求核对服务端，属推断）；修前同一场景的撤下走的是同一个 `WithdrawRpc`，上一轮（`4947c62a`）测过 22 次里 1 次。15 对 15 次的样本区分不了两边的这个比例。证据（分类结果、每次的进程日志、冒烟源码）在本轮的临时目录，不进仓库。

**验收**：gofmt；`GOWORK=off go build ./...`、改动包 `go vet`；glsvet（`./framework/nest ./framework/entity ./framework/dataengine/engine ./framework/sync/entitysync ./framework/app ./infra/network/bus ./wiring/nats`）；改动包 `-race`；根包；`check-doc-anchors.py`；全量 `go test ./...`；`scripts/test-demo-generated.sh`、`scripts/test-remote-generated.sh`、`scripts/test-genres-generated.py`。
