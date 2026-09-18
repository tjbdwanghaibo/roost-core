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

## 3. 九批各做了什么（对应 codegen 提交）

| 批 | 内容 | 提交 |
| --- | --- | --- |
| 1 | 机制 + P0-1/2/3：demo 目录、embed、占位符、CI 一格；Profile / Bag 组件 + Player DAO（`map=fast` 的 Items）；AddItem handler；player access + tcp + demo auth；协议与端点 | codegen `e499765` |
| 2 | item 配置表（`//roost:table` + CSV 四行头 → item.json）；errcode 三个码 + 端点错误边界（生成的 TCP server 把端点返回的 error 当坏帧断连，业务失败必须 `errcode.ClientError` 换形状）；account collaborators 可用版（demo 渠道 verifier + Redis INCR 分配器） | codegen `feae1b3`；kit `527eecd`（`account.RegistryBound`） |
| 3 | 会话票据校验（`session:<id>:<token>` 经 account `ValidateSession`）；升级事件链（`nest.Emit` → JetStream durable + Mongo inbox → `mail.Send`，`RequestID = EffectID`）；服务名占位符；脚手架 run 步骤开 TCP | codegen `b4d55d3`（含 TCP 传输层模板新增 `RegistryBound` 钩子） |
| 4 | 机器人压测 = 回归测试（`cmd/loadtest`、`loadtest/playertcp` 帧适配器、EnterGame 协议与手写端点）；**首次实跑并发现 `add endpoint` 的实体 id 缺陷** | codegen `f13b0af` |
| 5 | 跨服务组队（JoinQueue / PollMatch、game 进程内 matchmaker）；World 的真实职责（Stats 组件与 DAO、RecordEnter / RecordMatch / WorldStats） | codegen `99ffcdd` |
| 6 | 可观测性：Prometheus + Grafana compose、抓取配置、预置仪表盘（按链路分组 18 个面板）、指标 ↔ 链路说明；`cmd/loadtest -metrics-addr` | codegen `22bafa9` |
| 7 | `make loadtest`（Makefile 模板 + `help-make`）；压测真实登录（`-account-nats`：Login → CreateRole → SelectRole，`session:` 握手）；`cmd/accountctl upsert-server`（`UpsertServer` 刻意不在 RPC 上，操作员工具直开 store）；成组推送 `MatchFound`（notify 协议 + `PushPlayer`，场景 `wait_push`） | codegen `959fcc9` |
| 8 | 实体锁档（Player → `EntityCategoryPlayer`、World → `EntityCategoryWorld`）+ 两实体事务 `AddExp`（`MultiSync_AddExp`，World 累计 `ExpGranted`）；删掉 `player:<id>` 调试凭据；`demo-publish.yml` 把生成工程推到 `demo-generated` 分支 | codegen `53bafdd` |
| 9 | 真 Grafana 验证（33 条查询 0 错误，No data 逐条可解释）；面板加 `player_tcp_connection_rejected_total{reason}`；600 机器人撞上 `max_connections_per_ip` 的解释 | codegen `678b2d9` |

每一批的细节都在 codegen `CHANGELOG.md` 的 game-demo 条目（"第 N 批"）与 `demo/README.md` 里；本文不重复。
第 7～9 批对应 §7 的 7.1 / 7.2 / 7.4、7.3 / 7.7、7.6，每小节写了实际做法与验证结果。

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

## 7. 剩余工作与实施方法（按价值排）

> 2026-09-16 第七批完成 7.1、7.2、7.4，第八批完成 7.3、7.7 并删掉了 `player:<id>` 调试凭据（codegen CHANGELOG"第七批 / 第八批"）。
> 7.2 的实际做法与原计划有一处重要不同，见该小节开头。7.6 已在本机 docker 里打开过真 Grafana（见该小节）。剩余：7.5（先登记 RR）。

每一项都写成"改哪里 → 先红什么测试 → 怎么验证 → 边界"，可以各自独立开工；顺序建议 1 → 2 → 4 → 3 → 5 → 6 → 7。
通用规则沿用前六批：demo 的文件全部是业务所有、写在 `roost-codegen/demo/**`（`.tmpl`）、步骤进 `demoScaffoldSteps`、
断言进 `TestDemoTemplateGeneratesABuildableWritePath`；改到生成器模板的，先在既有测试里加一个会红的片段断言；
每项结束跑 §4 的 CI 侧命令，涉及运行时行为的再跑实跑侧。

### 7.1 `make loadtest`（codegen Makefile 模板）——已完成

做法与下文一致：`render.go` 加 `loadtest:` 目标与 `LOADTEST_ENDPOINT / COUNT / METRICS_ADDR / ARGS` 变量，`test -d cmd/loadtest` 守卫；
`cli.go` 的 `help-make` 同步；`TestGeneratedMakefileHasLoadtestTarget` 修前红。

- **改哪里**：`roost-codegen/internal/roost/render.go` 的 Makefile 模板（`run:` 在 599 行附近、`dev-logs:` 在 683 行附近），
  在 `.PHONY` 列表与 `run` 之后加：
  ```make
  LOADTEST_ENDPOINT ?= 127.0.0.1:7000
  LOADTEST_COUNT ?= 10
  loadtest:
  	@test -d cmd/loadtest || { echo "cmd/loadtest not present; generate with -template game-demo or add your own"; exit 1; }
  	go run ./cmd/loadtest -endpoint $(LOADTEST_ENDPOINT) -count $(LOADTEST_COUNT) -metrics-addr 127.0.0.1:9300
  ```
  `test -d` 守卫让 `game` 模板（没有 cmd/loadtest）也能保留同一份 Makefile。同步 `render_docs.go` / `render_workflow_docs.go`
  里列 Makefile 目标的段落，和 `help.go` 若有目标清单。
- **先红**：`roost_test.go` 里已有对 Makefile 内容的断言（103、196、628 行附近），加 `"loadtest:"`、`"LOADTEST_ENDPOINT"`。
- **验证**：生成 game-demo 工程后 `make loadtest`（服务起着）与 `make -n loadtest`；生成 game 工程后 `make loadtest` 应给出那句提示并非零退出。
- **边界**：Makefile 是生成物，改模板后 `project sync` 会回写到所有工程——这是想要的；`deploy_hygiene_test.go` 会扫 Makefile，
  看它对新目标有没有意见。

### 7.2 压测走真实登录（account Login → CreateRole → SelectRole）——已完成，一处与计划不同

**`UpsertServer` 刻意不在 account 的 RPC 接口上**（`account_rpc.go` 头注释：登记 / 开关服务器是控制面写入，game 进程无权做），
所以"game 在 Init 里登记自己"是错的，已改为操作员工具 `cmd/accountctl upsert-server`：用 Redis 凭据直接打开 account 的 store
（`svcaccount.NewRedisStores` + `svcaccount.New`，collaborators 用工程自己的）写服务器记录，环境准备时跑一次。
`cmd/loadtest -account-nats` 在压测进程里 `natsdriver.Assemble` + `bus.New` + `svcaccount.NewBusClient`，每个机器人
Login → CreateRole → SelectRole，以 `session:<id>:<token>` 握手；每次运行用新 open id。实跑：10/10 成功，account Redis 里 10 个角色，
game 无 `session ticket rejected`。`player:<id>` 分支仍保留（不给 `-account-nats` 时用），删除留待下一批。

- **改哪里**：`demo/cmd/loadtest/main.go.tmpl`。加 flag `-account-nats nats://127.0.0.1:4222`（空则保持 `player:<id>` 捷径）、
  `-server-id 1000`。有值时：
  1. 起一条 NATS 连接与 bus（参照 kit `nats` mod 的装配：`natsdriver` 客户端 + `bus.New(client, rpc, nil, bus.Config{SvcType:"loadtest", Sid:…})`；
     或更省事——在 loadtest 进程里直接用 `app` 装配 `kitnats.NewNatsMod(nil)` + `svcaccount.NewClientMod()`，从 registry 取 `svcaccount.Accounts`），
     `svcaccount.NewBusClient(bus, "", 0)` 得到 `Accounts`。
  2. `runner.WithAuthProvider`：对每个机器人按 `rb.PlayerID` 派生 open id（如 `robot-<PlayerID>`），依次
     `Login(ctx, Identity{Channel:"demo", OpenID, Credential:"demo:"+OpenID})` → `CreateRole(ctx, account.ID, serverID, name)`
     （`ErrNameTaken` / 已有角色时改为读账号角色列表——看 `Accounts` 接口有没有 Roles 读法；没有就先 SelectRole 用已知 PlayerID）→
     `SelectRole(ctx, account.ID, role.PlayerID)` → `playertcp.AuthPacket("session:" + PlayerID + ":" + session.Token)`。
     注意此时机器人的业务 PlayerID 是 **account 分配的**（Redis INCR，从 100001 起），不再是 `-first-player-id + i`；
     把它写回 `rb.Blackboard`，并用 `runner.WithIdentityProvider` 只提供 open id 的序号。
  3. account 进程要起：`config.account.smoke.yaml`（redis 16379、nats、独立 prefix、`session_secret` 随便填非空）。
- **先红**：`demo_test.go` 断言 `cmd/loadtest/main.go` 含 `svcaccount.NewBusClient(` 与 `"session:"`；auth.go 的 `player:` 分支保留到实跑通过后再删，
  删时同步 `TestDemoTemplateGeneratesABuildableWritePath` 里对 `demoTokenPrefix` 的断言。
- **验证**：四进程（account / game / mail / match）实跑，`-account-nats` 给值，10 个机器人成功；Redis 里出现 `roost:planet:account:*` 角色与
  会话，game 日志无 `session ticket rejected`。
- **边界**：SelectRole 的票据有 `session_ttl`（默认 30m），压测时长超过它要重新 SelectRole；account 的 `NameRules` 拒绝空白名，角色名用
  `robot_<n>`。

### 7.3 实体锁档（Player / World 分档）——已完成

demo 覆盖两个 `entity.go`：Player → `EntityCategoryPlayer`、World → `EntityCategoryWorld`，注释写清"档位即锁序、档位编进 id、
改档等于迁移"。同时把 `AddExp` 改成两实体事务 `handlerAddExp(target player.IProfileEntity, stats world.IStatsEntity, amount)`：
World 累计 `ExpGranted`，两处变更同一条 WAL 记录，Nest 按档位先锁 World 再锁 Player；Sender 变为 `MultiSync_AddExp`，端点手写
（`add endpoint` 只接单实体）。实跑前清掉了旧的 `game.player` / `game.world`（旧 id 不再匹配），10/10 成功，`db.world.exp_granted` 随每轮增长。

- **事实**：`entity.EntityCategory` 有 `Remote(1) / World(2) / PlayerScoped(3) / Player(4) / Other(5)`，锁按档从低到高获取；
  **档位编进实体 id**（`idgen.go` 的 `EntityCategoryBits`），所以改一个 kind 的 category 会改它所有实体的 id——已落库的 Player / World
  文档 `_id` 全部失配，等于一次数据迁移。demo 里 Player 与 World 从不互相叠锁，所以现在的 Other 没有正确性问题。
- **改哪里**：`demo/game/entities/player/entity.go.tmpl` 与 `world/entity.go.tmpl`（目前这两个文件由 `add entity` 生成、demo 未覆盖；
  要覆盖就把 `//roost:entity … category=entity.EntityCategoryPlayer` / `EntityCategoryWorld` 写进去，并把生成器的通用注释换成 demo 的
  一段：为什么分档、什么时候必须分、改档等于迁移）。
- **先红**：`demo_test.go` 断言两个 entity.go 的 category 标记。
- **验证**：生成 → build；实跑前**清空** `game.player` / `game.world` 与 WAL 目录（旧 id 不再匹配），再跑 loadtest。
- **边界**：只有在加入"一个 handler 同时锁 Player 与 World"的示例时才值得做（比如 `RecordEnter` 改成在 EnterGame 的同一事务里锁两者）；
  否则保持 Other 并在注释里说明即可。若做，顺带演示 `nest.pipelined.allowlist` 与 `nest_handler_lock_hold` 面板的对比。

### 7.4 PollMatch 改推送——已完成

`MatchFound`（10100）是 notify 形式的 `//roost:msg`（无请求、一个返回类型），生成的 bind 注册编码器、bootstrap 自动调用；
matchmaker Commit 后经 `accessplayertcp.Runtime.PushPlayer` 推给每个成员；场景用 `selector{wait_push(msg 10100, 10s) → retry{poll_match}}`，
轮询退为兜底。实跑：`player_tcp_push_total` = 机器人数。

- **事实**：生成的 TCP 传输层有 `Runtime.PushPlayer(ctx, playerID, messageID, value)` / `PushSession(ctx, sessionID, …)`，
  推送帧 `flags=flagServerPush, sequence=0`；robot 侧 `playertcp.Conn` 已把 seq 0 的帧交给 `WaitPush`。协议侧需要一个
  `//roost:push` 消息（看 `internal/protocol` 的 push 标记语法，`protocol_manifest.json` 里有 pushes 段）。
- **改哪里**：`demo/protocol/def/match_found.go.tmpl`（push 消息 `MatchFoundPush{MatchID, Members}`，id 10100）；
  `internal/service/game/matchmaker.go.tmpl` 在 Commit 成功后取传输层 Runtime（`app.Lookup[*accessplayertcp.Runtime](registry, accessplayertcp.Name)`）
  对每个 member `PushPlayer`；推送失败（玩家已下线）只记日志——ticket 里仍有 match_id，PollMatch 保留作为兜底。
  loadtest 场景把 `retry{wait; poll_match}` 换成 `action: wait_push, param: {msg: 10100, timeout: 10s}`（内建动作 `runWaitPush`）。
- **先红**：`demo_test.go` 断言 matchmaker 含 `PushPlayer(` 且场景含 `wait_push`。
- **验证**：实跑，机器人在无轮询下收到推送；game 日志 `player_tcp_push_total` 增长。
- **边界**：PushPlayer 推给该玩家**所有**会话；同一玩家多连接是否都该收到，写进注释。

### 7.5 kit match `Grouping` 无调用路径——**未做，唯一剩余项**

**现状**：`roost-kit/service/match/match_mod.go` 的 `NewMod(grouping Grouping, …)` 接受这个 collaborator，注释说"这是整个匹配策略"；
`queue_store.go` 里 `cfg.Grouping` 只在 159 行被赋默认值 `FirstComeGrouping{}`，全包（含 `store.go`、`redis_store.go`）没有任何
`.Group(` 调用；成组完全由调用方 `Candidates → Commit` 驱动。demo 的 `internal/service/<game>/matchmaker.go` 就是这样用的，
并直接调 `svcmatch.FirstComeGrouping{}.Group`。生成的 `internal/service/match/collaborators.go` 里 `Grouping()` 返回 nil。

- **已登记为待审查候选** [`docs/bug/WANTED.md` W-2026-09-16-01](../bug/WANTED.md)，含会红的测试草稿；由 review agent 判定后
  分配 RR 或判非问题。实现侧在此之前不动码。
- **两种修法（RR 里定契约后再做）**：A. 服务端驱动——`Enqueue` 末尾在同一次 CAS 里 `Grouping.Group(queue, waiting)`，成组即 `Commit`，
  票据直接以 matched 返回；调用方不再需要 matchmaker 循环，但 `Grouping` 成为热路径、且跨 Redis 键的原子性要论证。
  B. 删掉 collaborator——`NewMod` 去掉 `grouping` 参数，`Grouping` 保留为调用方工具（demo 现在的用法），codegen 的
  `framework_services.go` 里 match collaborators 模板同步删 `Grouping()`。B 更小，也更诚实。
- **先红**：A 需要 `queue_store` 的 promise test "两张票入队后第二张返回 matched"；B 需要 codegen 对 `internal/service/match/collaborators.go`
  的断言不再含 `Grouping`。

### 7.6 在真 Grafana 里打开仪表盘——已完成

用生成工程里的 `deploy/dev/observability/docker-compose.yaml` 原样起 Prometheus + Grafana（本地加了一个匿名 Viewer 的 override
只为让浏览器免登录看图，不在发布物里），四进程 + 六轮 20 机器人 + 一轮 600 机器人。结果：数据源与仪表盘 "Roost game-demo" 自动
预置；33 条查询 **0 条 PromQL 错误**，14 条有数据、19 条 No data 且每一条都能解释（错误计数器为零不存在、`bus_rpc_*` 只在
JetStream RPC 路径、robot 的 `rate()` 需要两次抓取而一场压测不到 1 秒、Nest 队列 gauge 由 statslog 每分钟一次写入）；
`histogram_quantile` 的 `le` 已是秒、Duration 平均值面板算法正确。浏览器截图确认玩家接入 / Nest / WAL 三行有曲线。

两个顺带发现：
- **600 机器人只成功 128**：`player_access.tcp.max_connections_per_ip` 默认 128，机器人全来自 127.0.0.1，超出的连接握手前被关，
  机器人侧报 `connect: auth send: robot session: closed`，server 侧 `player_tcp_connection_rejected_total{reason="per_ip"}=472`。
  不是缺陷，是上限在工作；已把该计数加进仪表盘并写进两处 README。压更大要调高配置。
- 本机 9100 被另一个 roost game 进程占着（`___1game`，非本会话所起），Prometheus 在我改端口前抓了它一分钟，仪表盘的 handler
  图例里因此混进了别的工程的 handler 名——共享机器上抓取端口要先 `lsof` 看一眼。

- 有 docker 的机器：`docker compose -f deploy/dev/observability/docker-compose.yaml up -d`，三进程 ops 端口 9100–9102 与
  `-metrics-addr 127.0.0.1:9300` 起着，看六个 row 是否都有数据；重点核 `histogram_quantile` 面板的 `le` 单位（导出器给的是秒）与
  Duration 平均值面板（`_sum_nanos / _count / 1e6`）。改动只在 `demo/deploy/dev/observability/grafana/dashboards/roost-demo.json.tmpl`。
- 若想让 CI 也校验 PromQL：`promtool` 没有离线 PromQL 语法检查，但可以在 `demo_test.go` 里用 `github.com/prometheus/prometheus/promql/parser`
  ——这会给 codegen 加一个重依赖，**不建议**；退而求其次是现在的做法（JSON 合法 + 查询非空 + 指标名对照实跑输出）。

### 7.7 一个可以直接 clone 来读的完整工程——已完成（首次运行待 GitHub 侧确认）

`.github/workflows/demo-publish.yml`：main 上 `demo/**`、`internal/**`、`cmd/**` 有变更时，按 source-head 生成工程，
build / vet / test / `generate --check` / `id check` 通过后 force-push 到本仓 `demo-generated` 分支，带 `GENERATED.md` 写明
codegen / core / kit 三个 SHA。actions 按仓库规则钉到完整 commit SHA（`TestRepositoryWorkflowsAreValidAndPinned`）。
本地无法执行 GitHub Actions，第一次运行结果要到 Actions 页看；若 `contents: write` 被组织策略禁止，改为 PAT secret。

- 在 `roost-codegen/.github/workflows/` 加一个 `demo-publish.yml`：在 `framework-compat` 的 `demo × source-head` 绿之后，
  把生成产物（去掉 go.work）推到 `tjbdwanghaibo/roost-demo` 仓的 `main`（或本仓 `demo-generated` 分支），提交信息带 codegen SHA。
  只读、不接受 PR；README 顶部写"由 codegen 生成，改动请回到 roost-codegen/demo"。
- **不要**手工维护第二份代码——core `examples/` 就是这么烂掉的。

## 9. 第十批（2026-09-17）：让 game / game-demo 直接跑起来，五个服务都可用

目标是 `roost project new … -template game-demo` 之后不改任何东西就能起来、`doctor` 全绿、每个托管服务都被链路真实调用。

- **一条命令起全部进程**：codegen 受控的 `deploy/dev/run.sh` + `make dev-run / dev-stop / dev-status / dev-smoke`。顺序 account → chat → mail → match → game，
  每个进程等 `/readyz`；有 `cmd/accountctl` 的工程顺手 `upsert-server -sid`。前置条件是每个服务本机配置有自己的 ops 端口（`opsPort`：业务服务按名从 9100 起，
  托管服务接在后面），此前五份配置都监听 9100。生产配置不变（统一 9100，一容器一进程）。
- **chat 进链路**：`internal/service/chat/collaborators.go`（策略写成决定、`text` 类型、`GrantSystem`）、`game/chatroom/`（频道 / 文本校验 / 每进程 presence）、
  协议 SendChat 10006 / ChatHistory 10007 / 推送 ChatMessage 10101、EnterGame 经 PublishSystem 公告登录并扇出、机器人脚本 send_chat → wait_push → chat_history。
  控制器经接口 + capability 名懒查 TCP Runtime（直接 import `internal/access/player/tcp` 是环：它 import 协议绑定，绑定 import 控制器）。
- **doctor 全绿**：account 演示 Verifier 的报错文案含 "is not configured"，被 doctor 的桩标记误判；改文案。这是启发式标记的代价，测试里把四个 collaborators 都对着标记断言了一遍。
- **实跑发现的 bug**：机器人 transport 只按序号识别响应，服务端推送也带会话序号（同起点 1），世界频道推送恰好带着在途请求的号就被当成响应
  （`response msg mismatch: got 10101 want 10000`）。改成按帧头 server-push 标志分类。此前只有 MatchFound 一种推送且只在 wait_push 期间到，所以没暴露。
- **实跑环境**：本机 27017 / 4222 / 6379 被不带 replSet 的 mongod、没开 JetStream 的 nats-server 占着，Docker 默认端口路径没法在这台机器验证；用 kit 隔离环境
  （`sed` 五份配置指向 27117-27119 / 14222 / 16379，ops 改 920x，nats.prefix 独立）跑通：五个进程 `run.sh start` 全部 ready，6 个机器人全链路通过。
  连续两轮间隔 < 60s 会有一个机器人 `ticket still waiting`（上一轮失败者的票被这一轮配走），是 match 的正确行为，README 写明了。
- **mail 的客户端一半**（同批第二段）：升级奖励邮件带附件（`game/rewards/`，发件方与领取方共用编码），协议 ListMail 10008（含 `Claimable`）/ ClaimMail 10009
  （ReserveClaim → Sync_AddItem → CommitClaim，失败 CancelClaim）；机器人 add_exp 后 `retry × 20 { wait 250ms; list_mail }` 再 `claim_mail` 断言背包计数。
  边界写在 README：demo 没把 claim token 带进 Nest 事务，进程死在 grant 与 commit 之间会重发一叠。场景变长（两条异步链、三个跨服务调用）后机器人 p95 默认阈值 2s → 5s（回归界限不是 SLO；成本直方图是 2 的幂桶，20 机器人同机实跑 p95 落在 3s 桶边）。
- **session 托管进 game 模板**（同批第三段）：`frameworkCatalog` 加 `session`（`Release()` / `Metrics()` collaborators，默认拒绝），game / game-demo 现在托管五个服务；
  demo 的 `Release()` 是日志 + nil（副本不占外部资源），协议 EnterDungeon 10010 / FinishDungeon 10011（Finish 后经 AddExp 两实体事务发 100 exp）。
  实跑：六进程 `run.sh` 全 ready，6 / 20 机器人全链路通过（登录 → 聊天 → 道具 → 升级 → 领邮件 → 副本 → 匹配 → 世界计数）。
- **ranked 队列**（A4）：`game/matchmaking.Pools()`——duel 按到达、ranked 按等级（`ScoreWindowGrouping` 窗口 5 / +5 每秒 / 上限 50，U-0222 修过溢出后的第一个使用方）；
  `PlayerLevel` 读 handler 在 Player 锁内取等级作 Score；JoinQueue / PollMatch 带 mode；机器人 duel 之后再排 ranked。
- **dao golden 过真实编解码器的 CI 门**（`scripts/dao-golden-runtime.sh`）：U-0224 那类"文本对、编码错"的缺陷此后在 CI 红。
- **demo 场景在 CI 真跑**（C11）：framework-compat 的 demo cell 现在 compose 起基础设施 → 生成的 `run.sh start` 起六进程 → `loadtest -count 6` 走完整条链 → stop；此前只编译。
- **`roost add rpc`**（B5）：工程自己的跨进程服务一条命令到装配——接口 / 实现 / owner Mod / 两半 / 清单 `rpcs` 与 `uses_rpcs` / bootstrap；full 场景 CI 编译 `Guild`。
- **C14 / D16**：生成工程带 `.gitattributes`（生成文本钉 LF），`generate --check` 与 `servicerpc -check` 容忍 CRLF 检出；`project next` 完成必做链后列出未用的能力（rpc / saga / attribute / skill / webroute / cfggen）。
- **B7 技能目录**：`add skill Fireball` + 契约写明的 JSON，game `Init` 用 roost-core/skill 编译目录 fail-fast，`SkillCatalog` 10012 列 id 与 warning 数；技能执行刻意不进 demo。
- **B6 GM 运维面**：`internal/service/game/gm.go` 在 app 的 admin 命令表（`app.ModAdmin`）注册 `gm.player.add_item` / `gm.player.add_exp` / `gm.mail.send`（奖励附件、`trace_id` 幂等）/ `gm.world.stats`，
  ops 经 `/admin/commands` `/admin/execute` 端出、`X-Admin-Token` 鉴权；开发配置开 admin（dev token），生产示例关（`config check --production` 也拒绝 dev token）。
  命令用与端点相同的 Nest Sender / mail 客户端，所以 GM 加的经验升级同样发奖励邮件。实跑发现并修：`player_id` 只收唯一 id 时，运维从 Mongo 拿到的 `_id`（完整实体 id）
  被再包一层成了不存在的实体；现在 `MatchEntityID` 识别完整 id、`GetUniqueIDFromEntityID` 还原邮件收件人，两种形式都收。实跑：加道具落库、加 300 exp 升 2 → 5 级且 World `exp_granted` 500 → 800、
  邮件同 trace 两次同一 `mail_id`、无 token 401、坏载荷 `command invalid`、超叠加上限按 `bag_full` 拒绝。
- **B9 saga 送礼**（`roost add saga GiftItem -service game -steps debit,deliver` 的第一个真实使用方）：`SendGift`（10013）在发送方 Player 的 Nest 事务里
  检查背包并 `saga.EmitStart`（start 意图与事务同一条 WAL 记录，saga id = 发送方 + 会话 + 帧序号，重发同帧不重开）；`internal/service/<game>/gift_saga.go`
  四个步骤消费者（`SubscribeMongoStep`）：debit = `GiftDebit` Nest 事务（`Bag.RemoveItem`，新 errcode `item_short`），补偿 = 既有 `AddItem`；deliver = 查 Player 集合确认
  收件人进过游戏后 `mail.Send` 带附件（RequestID = 命令 IdempotencyKey）；`GiftStatus`（10014）经 `mods.ModSaga` 的 Engine 读记录（未到 / 非本人 = `unknown`）。
  机器人：送给自己 → 轮询 `gift_status` 到终态 → `expect_gift completed` → 领邮件；再送给玩家 1（从未进游戏）→ `expect_gift compensated`。GM 加 `gm.saga.get` / `gm.saga.list`。
  边界写在文件头：Nest 提交与 inbox 回执不原子（同 claim token 那条）；原生路径的完成效果没人消费（W-2026-09-17-04）。
  实跑发现两个框架问题：**core saga 补偿版本 +2**（U-0225，Mongo 存储上任何步骤拒绝都进不了补偿——修后 6 completed + 6 compensated）；
  **`add mod` / `add saga` 不给已生成的配置补段**（后加的 saga 用默认 8 GiB 建流，隔离环境起不来，配置里没有 saga 段可改）——codegen 现在按缺失的顶层键追加两份配置。
  机器人 p95 阈值默认 5s → 10s（四条异步链，成本落在 4s 桶边）。framework-compat 的 released × demo 暂时排除到 core v1.15.6 发版。
- **B10 实时战斗：lockstep 帧同步**（`roost-core/lockstep` 的第一个使用方）：匹配成功后，形成比赛的 game 进程开一个 `lockstep.Room`
  （`internal/service/<game>/battle.go`，一条 goroutine 独占房间——Room 内部没有锁；端点把命令投进 channel），两名玩家打满 45 帧 / 30 Hz。
  线是 demo 已有的 player TCP：广播 = 推送 `BattleFrame`（10102，包里带冗余帧），输入 = 请求 `BattleInput`（10015，一条消息同时带
  本帧输入、关键帧哈希、补发请求）；`game/battle` 是两端共享的契约（帧预算、输入编码、座位、确定性模拟与哈希）。机器人用
  `roost-core/robot` 的 `LockstepBot` 跑真客户端。实跑：6 机器人全过，每个应用满 45 帧，无 desync。
  **两处实现级结论值得记住**：(1) 房间切完最后一帧要留收尾窗口——客户端只有应用了关键帧才报得出那一帧的哈希，切完就关会正好拒掉
  desync 裁决需要的那批报告（第一版就是这样，`report hash for frame 45: battle not found`）；(2) 推送处理器不能就地应用帧——
  应用一帧要在同一条会话上发输入，在读循环里做会把读循环堵死在自己的响应上（第一版只应用了 1 帧就停了）。
  边界：房间是进程内状态，多 game 进程部署要把战斗做成自己的服务、按 match id 寻址。
- **D15《数据流：六条路径》**（codegen `docs/DATA_FLOW.zh-CN.md`）：按路径而不是按功能组织——同步写、事件链、跨服务 bus 调用、saga、帧同步、配置快照，
  每条给出谁保证原子性、幂等键、失败后谁重试，外加"一次请求经过的边界清单"与排错入口。内容都是本轮实跑得到的结论。
- **C13 cfggen 过真实配置运行时的 CI 门**（codegen `scripts/cfggen-golden-runtime.sh`）：cfggen 此前只有文本比对，与 U-0224 之前的 dao 同一个盲区。
  临时模块 + 当前 core pin 跑 roundtrip：主键 / bean 切片 / 二级索引 / 关键字字段 / 全局 / 快照身份 / ref 悬空拒绝且不动现役快照 / required 拒绝 / 热更发布。
  验证过它会红：把生成的 json 名去掉下划线（编译与注册都正常、数据读不出来）四条全红。**cfggen 刻意没有进 demo**——
  demo 已经用 `//roost:table` + CSV 那条配置管线，同一个工程里并存两套配置定义方式是教学负担；`project next` 的进阶提示里指出 cfggen 的存在与命令。
- **B8 attribute 已完成**（09-18，随 U-0230 / RR-20260917-06）：W-2026-09-17-03 被 review 判为 RR，修法是把属性系统的**框架半**放进
  roost-core 新增的 `attribute` 包（`Meta` / `Profile` / `Selector` / `Snapshot` / `Container`），生成器改出包级访问器
  （`<Name>Of` / `<Name>In` / `<Name>Live`，不再给 Snapshot / Container 挂方法——Go 不允许给外包类型定义方法，这正是"只加 alias"走不通的原因），
  脚手架给启用该 feature 的工程写 codegen 受控的 `game/gameplay/attribute/runtime.go`。demo 随之用上它：`game/gameplay/attribute/combat.go`
  三个属性、一个派生公式（`_Power`）、`LevelUp` / `ForLevel`，随工程生成 `combat_test.go`。`dirtyMask uint64` 约定在生成期报错。

**ABCD 计划至此全部完成（16 / 16）**：A1–A4、B5 rpc、B6 GM、B7 技能、B8 attribute、B9 saga、B10 lockstep、
C11 CI 实跑门、C12、C13 cfggen 运行时门、C14 CRLF、D15 数据流文档、D16 进阶引导。

### 9.1 计划外仍然开着的事（按价值排）

> 2026-09-18 晚：第 1、2 条已实施（codegen `697ae18` / `0731db9`），做法与边界见 §9.2，**待 review 审查**。

3. **给 demo Player 加一个嵌套 struct 字段**。W-2026-09-17-02 已判为 RR-20260917-05 并修复（U-0232），
   现在加嵌套字段能让压测覆盖嵌套持久化与两层脏传播，且有运行时门兜底。
4. **attribute 接进持久化与同步**。demo 的 `Combat` 目前只在内存里：既没有进 Player 的 DAO，也没有接 `sync=true` 的实体复制。
   接上之后才是"属性系统"的完整形状；`Container` 的**层间合成**（base + buff → final）也留在这里一起定。
5. **多 game 进程**：chat 世界频道扇出目前按每进程 presence（多进程时只覆盖本进程的玩家），应改订阅流；
   battle 的房间是进程内状态，多进程要把战斗做成自己的服务、按 match id 寻址。
6. **Wanted-05 的接入样例**：生成实体 → room → session → sink 的可执行样例仍待产品化（review 09-18 已确认手动路线可行，八场景通过）。
7. **platform 待发货索引的参考实现**：U-0234 给了接入点（`PendingOrders`），索引本身（持久段、重启续接、分页公平性、终态退休）还没有范例。
8. **小账**：`session` 进程的 sweep owner 列表（默认懒解决）；attribute 生成的构造函数名 `New<TypeName>Profile` 在类型叫
   `XxxProfile` 时会得到 `NewXxxProfileProfile`；dao 的同一 child 被多父级共享时 `SetNotify` 后接线的赢。

### 9.2 第十一批（2026-09-18 晚）：把两条写在注释里的边界真正关掉

两条都是"范例已经有了，只差照着做"的收尾，实施后 demo 里"恰好一次"的形状统一了。**交 review 审查**。

#### 9.2.1 邮件附件的领取（§9.1 第 1 条）

- **问题**：`ClaimMail` 是 `ReserveClaim → Sync_AddItem → CommitClaim` 三步。三步不可能合成一个事务（邮件在另一个进程），
  所以窗口是真的：发放成功、`CommitClaim` 丢失、预留到期、重试用同一个 token 再预留一次。邮件服务只能把它算作重复**尝试**
  ——它无从知道游戏发没发——于是玩家拿到第二叠。
- **做法**：与 dungeon 清关奖励同形。中间那一步换成 `ClaimMailReward` 事务：邮件 id 与道具进同一条 WAL 记录
  （`db/def/player.go` 的 `MailClaims` 账本、`Bag.ClaimMailReward`、errcode `mail_claim`(100011)）。
  第二次发放在账本上撞到自己、什么也不做，回的还是同一组数字。
- **账本的界是时间**：保留期必须长于游戏能发出的最长邮件（生成配置 `mail.send_ttl` 720h，demo 的邮件 7 天），取 31 天；
  清理在写入口做。论证写在 `game/rewards`。发邮件很多的游戏应改成"commit 成功后即忘"或把账本放到邮件那侧——写在注释里。
- **测试**：随工程生成 `game/handler/claim_mail_reward_test.go`——真实 handler 跑在真实 Nest 事务里
  （一个只发一个实体的 Getter + 记录型 committer + 工程自己的配置数据，不需要 Mongo）。这也是"怎么测一个 handler"的范例。
  红：在生成工程里去掉账本判断 → `the replayed claim reported a fresh grant` / `the bag holds 2 after a replay`。
  机器人加 `claim_mail_replay`（邮件服务先拒，所以它证明的是"任何路径都不会再发一次"）。
- **仍然开着的**：预留成功但发放前崩溃（邮件被占到租约到期，玩家等，什么都没丢）；`CommitClaim` 丢失
  （账本挡的是重复**发放**，不是重复尝试）。

#### 9.2.2 送礼 saga 的原生步骤（§9.1 第 2 条）

- **问题**：debit / refund 的业务是 Nest 事务，却跑在 `SubscribeMongoStep` 上——Nest 提交与 inbox 回执是两次提交，
  中间崩溃会让重投再扣一次。这条边界一直写在文件头。
- **做法**：这两步改走 `saga.SubscribeDataEngineStep`，handler 在自己的事务里 `inbox.Bind(command, reservation)` +
  `saga.EmitCompletion(...)`（carrier 是 `game/gift` 的 `NativeStep` 与它的 `Complete(success, reason)`）。
  背包变更、命令回执、完成结果因此是同一条 WAL 记录；消费者不发布任何东西，它等回执被投影出来再 ack。
  补偿从"借用 `AddItem`"改成自己的 `GiftRefund` handler——给 `AddItem` 加 saga 身份会让每个端点都背上它没有的步骤。
- **deliver 刻意留在 Mongo inbox**：它的业务是一次 bus 调用，没有事务可绑；第二层幂等是 mail 服务的 `RequestID` 去重。
  **这条分界是规则**：原生路径给"业务本来就经 Nest 提交"的步骤用；拿它包一次跨服务调用，等于把回执绑在一个并不包含
  那次副作用的事务上。
- **业务拒绝也提交**（不动数据，只写回执与失败的完成结果）：协调器听不到的拒绝会让 saga 空等到 deadline。
  只有基础设施错误返回 error，回滚并让投递退避重试。
- **生成器补齐**：`roost add saga` 现在生成 `Topic<Step>` / `Topic<Step>Compensation` 常量。此前只有绑定 Mongo inbox 的
  `Subscribe*` 助手，想走原生路径只能自己重复 topic 字符串——durable 与 filter 会漂。
- **前提与陷阱**：需要 core ≥ v1.15.7（协调器直到那一版才有原生完成效果的消费者，U-0231）；
  `DataEngineStepInboxOptions.LeaseDuration` 必须长于消费者的 `AckWait`（demo 取 2 分钟 vs 30 秒），
  否则租约会在消息还没 ack 时过期、让第二个进程开始同一条命令——消费者会当场拒绝这种配置；
  原生 inbox 的 database 是 **Data Engine 的**（`game`），不是 saga 的，因为它绑的回执是那个库上的租约栅栏。
- **实跑证据**：6 机器人全过、saga 6 completed + 6 compensated、0 条 ERROR；Mongo 里 `saga-step` 回执 18 条
  （6 自赠 debit + 6 失败 debit + 6 refund，都带 payload）、`_dataengine_inbox_claims` 18 条、
  Mongo step inbox 12 条且全是 deliver（命令 id 的步位是 `:1:`）。
- **没做**：载荷解不出来的命令没有事务可以承载拒绝，只能靠重投与 deadline 收场；deliver 那条仍是两次提交；
  没有做"进程在 Nest 提交与 ack 之间被杀"的故障注入测试——证据是接线与回执，不是崩溃演练。

## 8. 相关文件速查

- 源码：`roost-codegen/demo/**`（`.tmpl`）、`roost-codegen/demo/README.md`（对新人的解释，按链路写）
- 脚手架：`roost-codegen/internal/roost/demo.go`（步骤表、占位符、落盘映射、run 步骤）、`demo_test.go`
- 模板改动：`internal/roost/render_player_tcp.go`（`RegistryBound`）、`internal/roost/add_workflow.go`（端点实体 id）
- CI：`roost-codegen/.github/workflows/framework-compat.yml`（`demo` scenario）
- kit：`roost-kit/service/account/identity.go`（`RegistryBound`）、`account_mod.go`（`bindCollaborators`）
- 生成工程内：`demo/README.md` 复制进去的说明、`deploy/dev/observability/README.md`（指标 ↔ 链路）
