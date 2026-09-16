# game-demo：一个能跑起来的参考实现（`roost project new … -template game-demo`）

> 状态：2026-09-16，六批实施完成并全部推送；除 Grafana 一批外每一批都在本地真实基础设施上实跑过。
> 本文是交接文档：给要继续做、要改、要审查这套 demo 的人（或 agent）。代码在 roost-codegen，
> 运行时行为在 roost-core / roost-kit；本文不重复代码里的注释，只写代码里看不出来的东西——为什么这样、
> 怎么验证、踩过什么坑、还剩什么。

## 1. 它是什么，不是什么

`-template game` 给的是**接线**：四个托管服务（account / chat / mail / match）、Player 与 World 两个空实体、
World 单例、装配代码。`Serve` 只是 `<-ctx.Done()`，两个实体是空壳，account 的 collaborators 默认全拒绝。
新人拿到后仍要自己想"实体、组件、DAO、Nest 事务、协议、端点、跨服务、效果"怎么串。

`-template game-demo` 在 `game` 之上按真实顺序跑一遍 `roost add`，并把业务文件的内容一并写进工程，生成出来就有
一条完整的、可以用机器人打通的链路：

```
TCP 接入（player:<id> 调试凭据 或 session:<id>:<token> 经 account ValidateSession）
 → EnterGame        PlayerLifecycle.GetOrCreate（第一次登录还没有实体，这不是 Nest handler）→ World.RecordEnter
 → AddItem          Nest 锁 Player → 查 item 配置表 → coded error 或 生成的 map mutator → WAL → Mongo
 → AddExp           升级 → nest.Emit(level_up effect) 与状态变更同一条 WAL 记录 → JetStream → 消费者 → mail.Send
 → JoinQueue/Poll   game 经 typed servicerpc 调 match：Enqueue / Ticket / Match；game 进程里的 matchmaker
                    每 500ms Candidates → Grouping → Commit → World.RecordMatch
 → WorldStats       World 的读 handler，锁内读、值出锁
```

它**不是**：不是安全的（两处显式标注"不是认证"）、不是性能基准（阈值只是回归门）、不是 core `examples/`
的替代品——那个目录烂掉的原因（不在 CI、go.sum 过期）正是这套机制要避免的。

## 2. 机制：文件放哪、怎么进工程、怎么保证不烂

| 事实 | 决定 |
| --- | --- |
| codegen 的 `go.mod` 只依赖 `gopkg.in/yaml.v3`，对 roost-core 零依赖；模板全是 `fmt.Sprintf` 字符串 | demo 源码放 `roost-codegen/demo/` 下，**按生成后的真实路径存放，多一个 `.tmpl` 后缀**，不参与 codegen 自身编译 |
| `go:embed` 不能跨越父目录、嵌套 module 会被排除 | `demo/embed.go` 是独立包 `demo`，`//go:embed cmd configs db deploy game internal loadtest protocol`；`internal/roost/demo.go` 从 `demo.Files` 读 |
| 业务文件被 `project sync` / `upgrade` 回写会冲掉玩家的改动 | demo 写进工程的全部是**业务所有**文件（无 `Code generated` 头、只写一次）；有几处刻意**覆盖**刚由 `roost add` 生成的骨架（组件、端点、控制器、auth.go、collaborators.go、service.go）：骨架给接线，demo 给正文 |
| 游戏服务名可能不是 `game` | 占位符 `{{MODULE}}`、`{{GAME_SERVICE}}`（snake）、`{{GAME_SERVICE_PKG}}`（`safeIdent`）；`demo/internal/service/game/` 下的文件落到 `internal/service/<name>/` |
| 正确性不能靠在 codegen 里编译 | CI `framework-compat.yml` 新增 `demo` scenario：生成工程 → build / vet / test。只排在 `source-head`（codegen HEAD 生成的实体接线用 M-04 之后的 entity category，kit 侧用 `account.RegistryBound`，已发布 tag 都没有），`demo × minimum` / `demo × released` 被 exclude |

脚手架是一张**有序步骤表** `demoScaffoldSteps`（`internal/roost/demo.go`），三类步骤：`add`（调 `roost add`）、
`write`（写模板）、`run`（改配置：开 TCP、给 match 加 `sweep_queues`）。顺序本身是契约——`add endpoint` 会比对
handler 形参与协议字段并拒绝对不上的组合，所以两边的文件必须先落盘。最后跑一次 `Generate`。

四条测试钉住机制（`internal/roost/demo_test.go`）：嵌入清单与磁盘一致；每个随包文件恰有一步写它、每步写的文件都存在；
模板保留 `game` 的服务与所需 features；**真正生成一个工程**后逐文件断言契约（含 doctor 的 player-tcp 工作流全 OK、
非默认服务名落盘正确、仪表盘 JSON 合法）。

## 3. 六批各做了什么（对应 codegen 提交）

| 批 | 内容 | 提交 |
| --- | --- | --- |
| 1 | 机制 + P0-1/2/3：demo 目录、embed、占位符、CI 一格；Profile / Bag 组件 + Player DAO（`map=fast` 的 Items）；AddItem handler；player access + tcp + demo auth；协议与端点 | codegen `e499765` |
| 2 | item 配置表（`//roost:table` + CSV 四行头 → item.json）；errcode 三个码 + 端点错误边界（生成的 TCP server 把端点返回的 error 当坏帧断连，业务失败必须 `errcode.ClientError` 换形状）；account collaborators 可用版（demo 渠道 verifier + Redis INCR 分配器） | codegen `feae1b3`；kit `527eecd`（`account.RegistryBound`） |
| 3 | 会话票据校验（`session:<id>:<token>` 经 account `ValidateSession`）；升级事件链（`nest.Emit` → JetStream durable + Mongo inbox → `mail.Send`，`RequestID = EffectID`）；服务名占位符；脚手架 run 步骤开 TCP | codegen `b4d55d3`（含 TCP 传输层模板新增 `RegistryBound` 钩子） |
| 4 | 机器人压测 = 回归测试（`cmd/loadtest`、`loadtest/playertcp` 帧适配器、EnterGame 协议与手写端点）；**首次实跑并发现 `add endpoint` 的实体 id 缺陷** | codegen `f13b0af` |
| 5 | 跨服务组队（JoinQueue / PollMatch、game 进程内 matchmaker）；World 的真实职责（Stats 组件与 DAO、RecordEnter / RecordMatch / WorldStats） | codegen `99ffcdd` |
| 6 | 可观测性：Prometheus + Grafana compose、抓取配置、预置仪表盘（按链路分组 18 个面板）、指标 ↔ 链路说明；`cmd/loadtest -metrics-addr` | 见 codegen CHANGELOG |

每一批的细节都在 codegen `CHANGELOG.md` 的 game-demo 条目与 `demo/README.md` 里；本文不重复。

## 4. 验证：CI 侧与实跑侧

**CI 侧（每批都跑）**

```bash
cd roost-codegen && GOWORK=off go test ./... -count=1              # 含 TestDemo*
go build -o /tmp/roost ./cmd/roost
/tmp/roost project new planet -module example.com/planet -out /tmp/planet \
  -mods configdata,mongo,nats,dataengine,nest -template game-demo
cd /tmp/planet && cat > go.work <<'W'
go 1.27.0
use ( . /Users/whb/roost/roost-core /Users/whb/roost/roost-kit )
W
go build ./... && go vet ./... && go test ./... && /tmp/roost id check && \
/tmp/roost generate --check && /tmp/roost config check --all && \
/tmp/roost project doctor -workflow player-tcp
```

本地必须用 `go.work` 指向 core / kit 工作树（原因见 §2 表末行）。
注意 `roost` 会尊重 `GOWORK`，在 `/Users/whb/roost` 下跑 codegen 的测试要 `GOWORK=off`，否则会解析到根 go.work。

**实跑侧（第 4、5 批做过，第 6 批用它验证仪表盘的指标名）**

基础设施用 roost-kit 的隔离环境：`scripts/integration/dataengine-env.sh up`——Mongo 副本集 `roost-it` 27117–27119、
NATS JetStream 集群 14222–14224（监控 18222–18224）、Redis 16379。在生成工程里复制 `config.game.yaml` 等三份为
`*.smoke.yaml`，改：`mongo.uri` 指副本集、`nats.url`、**`nats.prefix` 给一个独立值（三进程一致）**、
`dataengine.effects.max_bytes` 调小（隔离集群只预留 1GB）、ops 与 TCP 端口、日志与 WAL 目录。然后：

```bash
go build -o /tmp/planet-bin . && \
/tmp/planet-bin game  --sid 1000 --config configs/service/config.game.smoke.yaml  &
/tmp/planet-bin mail  --sid 1000 --config configs/service/config.mail.smoke.yaml  &
/tmp/planet-bin match --sid 1000 --config configs/service/config.match.smoke.yaml &
go run ./cmd/loadtest -endpoint 127.0.0.1:17000 -count 10 -metrics-addr 127.0.0.1:9300
```

看：`game.player`（DAO 标记写的 `db=game`，不是 `dataengine.database`）、`game.world` 计数、
mail 的 Redis `box:<id>` 与 `send:<EffectID>`、match 的 `queue:duel:2:default`、JetStream `ROOST_EFFECTS` 消费者
`pending=0 redelivered=0`。重启 game 再跑一轮，items 与 level 在原值上累加。

最后一次完整实跑结果：10 个机器人 10/10 成功，error_rate 0、p95 1.0s（含刻意等待匹配），成组 5 对，每个升级玩家一封邮件。

## 5. 实跑发现的框架问题（已修 / 已记）

| 发现 | 处置 |
| --- | --- |
| **`roost add endpoint` 生成的端点把 `context.PlayerID` 直接当实体 id 交给 Sender。** Nest 寻址的是完整实体 id（unique id + kind + 锁档），第一条真实请求死于 `entity id: invalid: kind N category is not registered`。脚手架能编译，此前没有一步真的跑过它 | 已修（codegen `f13b0af`）：解析 handler 第一个形参 `<pkg>.I<Component>Entity`，生成 `entity.BuildEntityID(context.PlayerID, <pkg>.EntityKind<Name>)`；两条测试修前红。**已有工程的端点文件是业务所有、不会回写，需手改一行**，症状即上面那条错误 |
| account 的 collaborators 由 bootstrap 在 app 存在之前构造，拿不到任何进程内能力，而 `PlayerIDAllocator` 契约要求"持久且共享的计数器" | kit 新增 `account.RegistryBound`（`527eecd`）：Mod 在 `Provide` 里把 registry 交给实现了它的 collaborator |
| 生成的 TCP authenticator 只在 `Init` 拿到 viper 配置，无法用 account 客户端校验票据 | codegen TCP 传输层模板新增同名钩子 `RegistryBound`（`b4d55d3`），`server_gen.go` 是生成物，`project sync` 即得 |
| kit match Mod 接受 `Grouping` collaborator 并注明"这是整个匹配策略"，但 store 里**没有任何调用路径**；成组完全由调用方 `Candidates → Commit` 驱动 | **未改，待 review agent 登记为 RR**。demo 的 matchmaker 直接调 `FirstComeGrouping{}.Group`，这才是那个接口的用法 |
| Nest 对不存在的实体返回 `ErrEntityNotFound`；第一次登录没有 Player | 不是缺陷，是被 `game` 模板遗漏的一环：demo 加 `EnterGame` 走 `PlayerLifecycle.GetOrCreate` |

## 6. 环境陷阱（写给下一个实跑的人）

- **共享 JetStream 里残留的测试流会抢答 RPC。** kit 集成测试留下的 `ROOST_IT_RPC_REQ_*` 流覆盖 `roost.rpc.>`，JetStream 对匹配
  subject 的每个请求回 PubAck，与真正的服务端抢先——先到的赢。症状：读调用大面积 `bus: unsupported rpc response version 0`
  （PubAck 被当作响应信封解码出 Version 0），写调用偶尔成功。**处置：每次实跑给三进程一个独立 `nats.prefix`**；无需删流。
  排查手法：用 `nats.go` 裸发一条 `roost.rpc.match.match.Candidates` 请求看原始回包（回的是 `{"stream":"…","seq":N}` 就是它）。
- `dataengine.effects.max_bytes` 默认 8GB，隔离集群 `no suitable peers for placement, insufficient storage`，调到 64MB。
- Duration 类指标没有分位数（只有 `_count/_sum_nanos/_max_nanos/_last_nanos`），要分位数只有机器人那组 Histogram；
  直方图桶是进程生命期累积的，看一场压测请限定时间窗。计数器第一次递增前不存在，仪表盘 "No data" 的错误面板是好消息。
- 在 `/Users/whb/roost` 根下有 go.work，`go test ./...` 若 cwd 被重置到根会报 "directory prefix . does not contain main module"，用子 shell `(cd roost-codegen && GOWORK=off go test ./...)`。

## 7. 还没做的（按价值排）

1. **`make loadtest` 目标。** Makefile 是生成物（`Code generated`），demo 不能改它；要在 codegen 的 Makefile 模板里加一个
   `loadtest:` 目标（当 `cmd/loadtest` 存在时，或无条件 `go run ./cmd/loadtest -endpoint $(LOADTEST_ENDPOINT) -count $(LOADTEST_COUNT)`），
   并同步 `render_docs.go` 里的 Makefile 说明。现在用 `go run ./cmd/loadtest`。
2. **压测走真实登录。** 机器人现在用 `player:<id>` 调试凭据。真实路径是 account 的 `Login → CreateRole → SelectRole` 拿票据，
   再以 `session:<id>:<token>` 握手。account 的 RPC 是 bus 上的 typed 客户端，机器人进程可以起一个 `svcaccount.NewBusClient` 直接调；
   demo verifier 认 `demo` 渠道的 `demo:<open_id>` 凭据。做完后 `player:<id>` 分支可以删。
3. **实体锁档。** 两个实体都在 `entity.EntityCategoryOther`（生成的安全默认："持有它之后什么都锁不了"）。demo 里 Player 与 World
   从不互相叠锁，所以没问题；一旦有 handler 同时锁两者，需要把 World 挪到 `EntityCategoryWorld`、Player 到 `EntityCategoryPlayer`。
   这是教学点，应该在 `game/entities/*/entity.go` 的注释里说清（现在只有生成器的通用注释）。
4. **PollMatch 改推送。** 现在客户端轮询；生成的 TCP 传输层有 `Runtime.PushPlayer / PushSession`，matchmaker 成组后可以主动推。
5. **kit match `Grouping` 无调用路径**——交 review agent 登记；修法二选一：store 的 Enqueue 尾部试成组（服务端驱动），
   或删掉这个 collaborator、把 `Grouping` 挪成调用方工具（demo 现在的用法）。
6. Grafana 仪表盘只验证了 JSON 合法与指标名存在，没在真 Grafana 里打开过（本机无 docker）。第一次打开时检查
   `histogram_quantile` 面板的 `le` 标签为秒（导出器已是秒）。
7. 一个可以直接 clone 来读的完整工程：让 CI 把 `-template game-demo` 的产物推到独立仓 / 分支，不要手工维护第二份代码。

## 8. 相关文件速查

- 源码：`roost-codegen/demo/**`（`.tmpl`）、`roost-codegen/demo/README.md`（对新人的解释，按链路写）
- 脚手架：`roost-codegen/internal/roost/demo.go`（步骤表、占位符、落盘映射、run 步骤）、`demo_test.go`
- 模板改动：`internal/roost/render_player_tcp.go`（`RegistryBound`）、`internal/roost/add_workflow.go`（端点实体 id）
- CI：`roost-codegen/.github/workflows/framework-compat.yml`（`demo` scenario）
- kit：`roost-kit/service/account/identity.go`（`RegistryBound`）、`account_mod.go`（`bindCollaborators`）
- 生成工程内：`demo/README.md` 复制进去的说明、`deploy/dev/observability/README.md`（指标 ↔ 链路）
