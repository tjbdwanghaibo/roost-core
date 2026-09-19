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

> 2026-09-18 晚重排。判据变了：先看**整块框架能力在 demo 里有没有任何可执行使用方**——RR-20260918-01（`sync=true` 的生成物
> 根本编译不过）是 review 用三份 fixture 才发现的，而 demo 里只要有一个 `sync=true` 的实体，`make generate && go build`
> 当场就红。零覆盖的地方就是缺陷能长期活着的地方。第 1、2、3 条已于第十二批实施（§9.3）。

覆盖现状（2026-09-18 晚，实施第十二批之后）：kit 的 9 个真实服务 demo 用了 6 个（account / chat / mail / match / rank / session），
kit 的真实服务至此全部有使用方（platform 第十五批、global 与 activity 第十六批），directory 仍只被 account 间接用；core 这边 game-facing 的包里
**remoteentity、ownerroute、mirror、ai、actionflow 仍没有任何直接使用**（spatial 第十三批、migration 第十四批、timer 第十六批、featureflag 与 hotcode 第十七批已接入）。

1. ~~实体同步（`sync=true` / entitysync）~~ — 第十二批完成，见 §9.3.1。
2. ~~attribute 接进持久化与同步 + `Container` 层间合成~~ — 第十二批完成，见 §9.3.2。
3. ~~rank 服务~~ — 第十二批完成，见 §9.3.3。
4. ~~spatial（AOI / 兴趣管理）+ 一张地图~~ — 第十三批三批全部完成（§9.4）。
5. **多 game 进程**：chat 世界频道按每进程 presence、battle 房间是进程内状态、scene 也只覆盖本进程在线的玩家。
   真做要引入 `remoteentity` / `ownerroute` / `mirror`——三个包都零覆盖，合起来是"跨进程实体所有权"的完整故事。工作量最大。
   **2026-09-19 第一批做到一半，见 §9.10**：Guild（`remote=managed`）写完并通过生成 / 编译 / 单测，
   但实跑提交阶段撞上 core 的一条缺陷（负的 state version 转 uint64 后 BSON 写不进去，而且会让进程之后再也起不来），
   已记入 `docs/bug/WANTED.md` 的 W-2026-09-19-01 交审查定契约；代码暂存未合入。
   同一轮还确认了第二件事：**当前 demo 跑两个 game 进程本来就不安全**——玩家寻址的 Nest 调用没有按所有者路由
   （两个进程都订阅 `svc.game.all`），saga / 效果消费者共用 durable，于是两个进程会同时改同一个 Player，
   dataengine 以 `fatal projection version conflict` 退出。这正是这一条要引入 `ownerroute` 的原因。
6. ~~platform 待发货索引的参考实现~~ — 第十五批完成，见 §9.7。
7. ~~`core/migration`（数据版本迁移）~~ — 第十四批完成，见 §9.5.2。
8. ~~给 demo Player 加一个嵌套 struct 字段~~ — 第十四批完成，见 §9.5.1。
9. ~~featureflag / hotcode~~ — 第十七批完成，见 §9.9。
10. **ai / actionflow**：NPC 行为，最偏"游戏内容"的一档。
11. **小账**：`session` 进程的 sweep owner 列表（默认懒解决）；attribute 生成的构造函数名 `New<TypeName>Profile`
    在类型叫 `XxxProfile` 时会得到 `NewXxxProfileProfile`；dao 的同一 child 被多父级共享时 `SetNotify` 后接线的赢。

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
  **这条论证成立但属于"押在别的服务的存储策略上"那一类**（信封确实按 Redis key TTL 过期）；同批写的 dungeon 版本
  押的那条 TTL 压根不存在，被审查打回成 RR-20260918-04，修法见 §9.2.3。邮件这条要不要改成同形，写进了 `docs/bug/WANTED.md`。
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


### 9.7 第十五批（2026-09-19）：支付订单走通两个进程，以及 U-0234 留下的那个索引

platform 是 kit 仅剩的两个零使用服务之一，而 U-0234 在修"后台重试没有候选来源"时明确把**索引本身**留给了部署：
持久段、重启续接、分页公平性、终态退休。这一批把它实现出来，并且不是凭空实现——它必须支撑一条真的支付链路。

#### 9.7.1 一条付费订单的全程

`Purchase` 端点 → 游戏进程签一份回调（**demo 在扮演支付渠道**，真实工程里这把密钥游戏进程不该有，端点注释里写清了）
→ `platform.HandleCallback` 在 platform 进程里验签、按 order id **只插不改**地登记、然后调用部署的 deliverer
→ deliverer 把**已解析好的**发货内容（item id 与数量，不是 product id）写成一条 Redis 里的 grant
→ 游戏进程把 grant 抽干：一个 Nest 事务把道具加进背包、**把 order id 写进同一条 WAL 记录**，提交之后才删 grant。

几处是刻意的：

- **platform 进程碰不到 Entity**，它只有 Redis 和总线。所以 deliverer 能做的"已发货"只能是**把欠什么写成持久记录**；
  它返回 nil 的那一刻，这笔账已经不依赖任何一个进程还活着。如果它改成推一条消息就返回，推完到发放之间崩一次，
  就会留下一笔服务认为已完成、玩家没收到的订单。
- **先提交事务、后删 grant**。反过来（先删）任何一次崩溃都会吞掉一笔已付款的发货；这个顺序下崩溃只会让 grant 被再读一次，
  而第二次读会在账本里撞上同一个 order id，什么都不发。这是 dungeon 清关、邮件附件之后**同一形状的第三例**。
- **准入判据与清理判据是同一个数**：`paid_at + purchase.ClaimRetentionSeconds`。过期的 grant 是**带错误码的拒绝**
  （`purchase_expired`），不是悄悄发放——账本已经忘了它，悄悄发放就是第二次发放（RR-20260918-05 的教训）。
- **加字段不需要迁移**：`PurchaseClaims` 是新 map，BSON 解码给零值，所以 `schema=` 仍是 2。需要迁移的是**形状**变化。
- 掉线时买的东西在下次登录时结算（`EnterGame` 里抽一次），platform 进程不需要知道玩家在不在线、在哪个进程。

#### 9.7.2 索引：四件事，外加"退休由谁决定"

`pendingIndex` 是 platform 进程里的一个 Redis sorted set，与订单记录同前缀：

- **重启续接**：内存里的索引意味着进程死时正在等的订单永远在等。
- **分页**：`PendingOrders(ctx, limit)` 只取 `ZRangeWithScores(0, limit-1)`，其余的下一 tick 再说。
- **公平**：score 是"这笔订单下次值得一试的时刻"。deliverer 拿到的是**已认领**的订单，服务此前已经把
  `NextAttemptAtUnix` 推到本次退避之后，所以反复失败的那笔是在往后挪自己，挡不住排在它后面的。
  score 未到的条目直接停止扫描——集合按 score 排序，第一个没到期的后面不会有到期的。
- **退休**：索引**不自己判断**订单是否结束，而是在读的时候问服务（`Order(ctx, id)`），终态或查无此单才 `ZREM`。
  发货侧做不了这个判断——订单还能以 exhausted 或被运维 settled 结束，两者都不经过 deliverer，
  盯着自己的写入来退休的索引恰好会留住那些人已经处理过的单。读不出来的订单**保留**：
  对已发货的订单多试一次是空操作，把没发货的退休掉是一笔再也没人看的付款。

为此 kit 补了一个接缝：`platform.RegistryBound`（与 `account.RegistryBound` 同形，kit v1.14.10）。collaborator 由
bootstrap 在 app 之前构造，构造函数里拿不到 registry；索引要的 Redis 客户端和 key 前缀都只有 registry 给得了。
服务自身的 capability 在 `Provide` 返回之后才发布，所以索引**留住 registry、首次使用时再查**。

#### 9.7.3 测试与实跑

- 索引的四条承诺对着 map 替身测（`internal/service/platform/pending_index_test.go` 六条）：分页且最旧优先、
  反复失败不饿死后面的、退避未到不给出、三种终态与"查无此单"都退休、读不出来的保留、未绑定时报错而不是"没有待办"。
  把 Redis 客户端窄化成三个方法的接口，就是为了这个替身写得出来。
- 账本的重放方向客户端造不出来（客户端没法重发同一个帧序号），所以对着真实生成的 handler 测
  （`game/handler/grant_purchase_test.go` 三条）：同一 order 抽两次只发一次、过期拒绝且边界属于可领取的一侧、
  缺 order id 或缺付款时刻直接拒。
- 机器人：`purchase` 断言背包**真的涨了**（发货失败但回包成功是这里最容易漏的失败），`purchase_again`
  断言第二次是**另一笔订单**、又涨了一次——这条会抓到按玩家或按商品去重的账本。
- 实跑（8 进程 + 2 机器人，全绿）：Mongo 里 `purchase_claims` 两条、`items.1001 = 21`；
  Redis 里四笔订单 `state: delivered`、grants 哈希已清空；**下一次 30 秒 tick 之后 pending zset 归零**——
  这一条只有实跑能证明，它走的是"进程内延迟查 capability"那条路。

#### 9.7.4 顺带修掉的生成缺口

给 catalog 加 platform 时发现两处，都属于"生成出来的工程起不来"：

- platform 的配置块没有 `session_secret` / `payment_secret`，而 Mod 在 `Init` 里对空值直接拒绝——
  account 的块早就按 `CHANGE_ME` 发了，platform 漏了。
- `NewMod(...)` 之外的可选协作者没有生成入口。加 `frameworkServiceSpec.ModChain`，
  于是 `svcplatform.NewMod(...).WithPendingOrders(servicePlatform.Pending())` 是生成的，
  默认 collaborators 文件里也有一个返回 nil 的 `Pending()`（"没有索引"是合法选择，但不能是隐形的）。


### 9.8 第十六批（2026-09-19）：限时活动——World 的持久计时器与跨服聚合

一条链子把两个零覆盖的东西串起来：core 的 `timer`（实体自己的定时器，没有任何生成工程用过）与
kit 的 `global` + `global/activity`（路由 / 租约与跨服阶段聚合，kit 最后两个零使用服务）。
串起来才有意义——活动需要一个"到点关闭"的截止时刻，而截止时刻需要活过重启。

#### 9.8.1 World 的计时器堆（core `timer`）

- **形状**：`WorldDao.Timers map[int64]*TimerNode` + `TimerSeed`，`TimerComponent` 在 `OnInitFinish`
  里用它重建 `timer.Scheduler`，scheduler 的 change hook 反过来写 DAO。没有"保存定时器"这一步——
  加一个、烧掉一个、删一个，都落在**当时那个事务**里。
- **三件事和 `time.AfterFunc` 不同**：活过重启（19:58 部署，20:00 的截止依然在）；
  在锁内触发（handler 改的东西和节点的删除是同一条 WAL 记录）；**handler 不做 I/O**——
  它记一条 effect 就返回，跨进程调用由 outbox 消费者在锁外做。
- **handler 的返回值就是重试**：0 表示"烧掉我"，非 0 表示"这么久之后再来"。Emit 失败时返回 30s，
  于是窗口晚关而不是永远不关——一个只会 log 的 handler 会把唯一能关窗口的东西丢掉。
- **时钟被钉住**：`Tick(now)` 与 `ScheduleActivityPhase(..., now)` 都带着自己的时刻，进来先
  `SetClock`。否则一个截止时刻的 `End` 取决于**锁是什么时候拿到的**，而不是取决于安排它的那一 tick。
  这条是写测试时发现的：第一版测试用假时钟安排、真时钟计算 End，于是"还没到期"的 tick 把它烧了。
- 测试三条，核心那条是**重启**：第一个 World 安排截止时刻 → 取它会存下的文档 → 新 DAO `RestorePersisted`
  → 用 `EntityCreateParam.Dao` 建第二个 World → 未到期的 tick 什么都不写、到期的 tick 烧掉它并带出 effect。
  安排它的那个进程恰恰是不在了的那个，所以这一半只有测试能演。

#### 9.8.2 活动：谁说"结束了"，谁说"值多少"（kit `global` + `activity`）

- **职责切得很干净**：coordinator 只聚合 —— 收集"服务器 N 到达 close 阶段"，在**每个预期服务器都报到**
  或宽限期过后说"收齐了"，然后给每个服务器发一份 result dispatch。它不知道道具、不知道玩家。
  什么叫一分、结算发什么，全在 `game/activity` 和游戏进程里。
- **窗口 id 由时钟算出来**（`race-<窗口起点>`），不是谁分配的。于是组里每台服务器对同一个窗口算出同一个 id，
  中途重启的服务器**重新加入它本来就在贡献的那个活动**，不需要任何人告诉它。
- **租约来自 `global`**：进程启动 `Bind`（只插不改，第二次起冲突就是围栏）+ `AcquireLease`，循环里续约，
  停机时归还。活动的预期集合取自 `LiveGames(候选集)`——**没起来的服务器不能被等**，否则每个窗口都要等到宽限期。
- **贡献的幂等锚是 dungeon run id**，和清关奖励用的是同一个：重投的贡献被 coordinator 的 reservation 挡掉，
  而不是记两次。机器人断言的是精确的 1（清关一次 + 重放一次），不是"至少 1"。
- **结算的顺序是 邮件 → 记录 → ack**，每一步都能重复：邮件按 (活动, 玩家) 幂等；World 上的记录让整张榜
  只读一次；ack 放最后，所以中间任何一次崩溃都让 dispatch 保持 pending、整段重来。
  先 ack 的那个顺序，崩一次就是所有人的奖励没了而且没有任何记录说欠过。
- **榜是游戏自己的**：coordinator 没有"列出参与者"的接口（对聚合做枚举是无界的），所以贡献时顺手
  `ZADD` 一条到 Redis，结算只读前 `PaidRanks` 名。结算的成本因此不随参与人数增长。
- **远端错误要按 code 认**：`Bind` 的"已绑定"在总线上回来是 `remote.570110`，不是 `global.ErrConflict`，
  `errors.Is` 永远不匹配——写成 `errors.Is` 就是"第一次能起、第二次起不来"。实跑第二次启动时当场看到。

#### 9.8.3 实跑（10 进程）

`race-1789781400` 开窗 → 两个机器人各清一次关（含一次重放）→ `activity_standing` 读到精确的 1/1 →
09:35:01（截止 1789781700）计时器触发 → effect → `NotifyPhase` → `status=complete` →
下一个窗口自动开 → `settled paid=2 reason=collected`；Mongo 里 `settled_activities` 一条、
计时器堆里换成了下一个窗口的节点、seed 递增；两封结算邮件在邮箱里；"settled" 只出现一次。
`gm.activity.close` 也走同一条路把当前窗口提前关掉（提前关等于提前结束，之后到下个窗口开始之前的清关不计分，
命令描述里写了）。

#### 9.8.4 顺带抓到的缺陷：U-0246

`TimerNode.Type` 这个字段让 dao 生成器整个崩了：私有名是 `type`，不是合法 Go，错误
（`expected '}', found 'type'`）指向一个临时生成文件，跟"你有个字段叫 Type"之间毫无提示。
只有**关键字**会坏（`String`、`Len` 这种预声明标识符是合法的）。修法只改私有名（`typeValue`），
访问器、BSON 键、脏位常量都保持原拼写。记录见 `docs/bugfix/U-0246-dao-keyword-field-names.md`。

又一次同一个判据：**零覆盖的地方就是缺陷能长期活着的地方**——`Type` 是个再自然不过的字段名，
而在此之前没有任何生成工程用过它。


### 9.9 第十七批（2026-09-19）：运维面——开关与热补丁

两个零覆盖的 core 包，合起来是"不发版也能改一点东西"的那一面：`featureflag`（开关）与 `hotcode`（补丁点）。

#### 9.9.1 开关的源是配置表，不是常量

- **形状**：`configs/table/feature_flag.csv` 是源，`featureflag.DefaultStore()` 是游戏读的内存态，
  `internal/service/<game>/flags.go` 是连接两者的唯一一处：启动时发布一次，
  之后挂在**配置存储自己的 reload 钩子**（`AddReloadListener` 的 `AfterApply`）上，每次 reload 重新发布。
- **启动就要发布**，而不是等第一次 reload：incident 期间重启的进程必须带着运维留下的开关起来，
  而不是全开。
- **Replace 而不是 merge**：表是源，所以表里删掉的开关要消失，而不是停在最后一次的值上。
  代价是 GM 的临时覆盖会被下一次 reload 冲掉——这条写进了命令描述里，并在日志里 WARN 一行：
  两个源静默打架比一个源糟糕得多。
- **缺表时拒绝发布**：把"快照里没有这张表"读成"所有开关都关"，会在一次配置失误里把商店关掉。
  拒绝发布保留上一批开关并报错。
- **开关只能在入口读**。三个开关各自说清了边界：`purchase` 关掉只拒绝新购买——平台已经记下的订单
  照常结算；`monster_spawn` 关掉只停止补刷——活着的怪不动；`activity` 关掉只停止开新窗口——
  已经开的窗口照样结算。**在事务中间读开关会留下两条路径都不会产生的状态**，这句话写在 `game/flags` 的包注释里。
- 新增 GM：`gm.flag.list`（带 note——运维凌晨三点看到一个关着的开关，要能判断打开它安不安全）、
  `gm.flag.set`（本进程、临时）、`gm.config.reload`（编辑表之后让它生效的那一步，此前 demo 没有任何入口）。

#### 9.9.2 补丁点：能换的函数，和不能换的函数

- **形状**：`rewards.LevelUpReward` 走 `hotcode.Resolve(点名, 原函数)`；点在服务 `Init` 里**显式注册**
  （`installPatchPoints`），不是在各自文件的 `init()` 里——"这个函数可以在运行时被替换、revert 会精确回到它"
  是一句运维承诺，它的全集应该能在一个地方读完。
- **判据写下来了**：函数必须**每次调用自成一体**。补丁是在两次调用之间换的，所以任何"两个版本要对
  同一份在途状态达成一致"的函数都不能做成补丁点。奖励表这种"读一个等级、返回一个值"的正合适。
- **fallback 不是摆设**：没有注册补丁点的进程（测试、工具、没装的服务）必须照常工作。
  测试的第一条就是它——如果 Resolve 在没注册时返回零值，那"能热补丁"的代价就是"平时也可能坏"。
- 框架侧本来就把每个 Nest handler 注册成了补丁点（`hotcode.list` 里能看到 `nest.handler.*`），
  demo 补的是**领域函数**这一类；`hotcode.RegisterAdminCommands` 把 list / revert / load_plugin
  挂到同一个 admin 注册表上，与 GM 命令共用 token 与审计。

#### 9.9.3 实跑

`gm.flag.set purchase=false` → 机器人的 purchase 步骤当场拿到编码拒绝（`600101`），run 变红；
改回 true → 全绿。编辑 `configs/data/feature_flag.json` 把 `monster_spawn` 关掉 → `gm.config.reload`
→ 日志 `feature flags published count=3 off=[monster_spawn] version=2`；随后 `gm.scene.kill` 杀掉一只，
六秒后 `alive` 停在 2 不再回补——**关掉的是补刷，不是活着的怪**，与注释里写的边界一致。


### 9.10 第十八批（2026-09-19，未合入）：远端实体的第一个使用方，以及它撞到的两堵墙

§9.1 第 5 条的第一批。目标不是一次做完多进程，而是先给 `remoteentity` 一个**真实使用方**——
按既定判据，零覆盖的地方就是缺陷能长期活着的地方。

#### 9.10.1 选 Guild 而不是改 Player

把 Player 改成远端托管会牵动 demo 的一切（同步主体、场景、生命周期、刷怪）。Guild 是新的：
一个玩家名单，天然被不同进程上的玩家共同修改，正是远端托管要解决的形状；生成器的 testdata 里本来
就有一个 Guild fixture，说明这也是框架作者设想的例子。

写完的东西：`GuildDao`（名字 + 成员 map）、`Guild` 实体（`remote=managed`，`EntityCategoryRemote`——
远端托管的 kind **必须**排在第一位，因为分布式锁在 dispatch 顶部获取，在任何本地 mutex 之后再取它
就等于把本地锁拿着跨网络）、`RosterComponent`（found / join / snapshot）、三个 handler
（其中 JoinGuild 同时锁远端 guild 与本地 player）、三个端点、四个错误码、机器人动作，以及
`deploy/dev/second-game.sh`（第二个 game 进程：自己的 sid / ops 端口 / 客户端端口 / **自己的 WAL 目录**——
共用 WAL 目录会被 `nestwal: directory is already locked` 正确拒绝）。

生成、编译、生成工程全量单测都通过。

#### 9.10.2 第一堵墙：远端提交写不进 Mongo（→ W-2026-09-19-01）

实跑时 `FoundGuild` 的提交阶段报

```
dataengine mongo: remote projection: cannot marshal type bson.M to a BSON Document:
12630717602282170696 overflows int64
```

那个数正好是 `uint64(负的 int64)`。`remoteentity/batch.go` 的 `BaseVersion: uint64(stateVersion)`
对负数没有设防，而 `applyCommit` 把这些 uint64 直接放进 `bson.M`。更要命的是**之后进程再也起不来**：
启动时的投影恢复会重放这条 WAL 记录，每次都在同一处失败退出，只能删 WAL。

三处都需要先定契约（新建远端聚合的版本语义、uint64 在 BSON 里的编码、投影恢复遇到必然失败的记录怎么办），
所以没有自己动手改，写进了 Wanted 交审查。demo 侧的实现暂存未合入。

#### 9.10.3 第二堵墙：两个 game 进程现在本来就不安全

顺带把"两个进程"真跑了一次，结果是第一个进程**直接退出**：

```
dataengine fatal storage outcome: ... fatal projection version conflict: game/player/102437892 expected=6 next=7 stored=7
```

原因不在远端实体，而在 demo 自己：玩家寻址的 Nest 调用没有按所有者路由——两个进程都订阅
`svc.game.all`，saga 步骤与效果消费者共用 JetStream durable——于是同一个 Player 会被两个进程同时加载和写入，
而 Player 不是远端托管的，没有任何围栏。

这正是 §9.1 第 5 条说的"要引入 `ownerroute`"：**远端托管解决的是被共享的对象，按所有者路由解决的是
被独占的对象**，多进程需要两者。第一批的收获是把这句话变成了一次可复现的失败，而不是一句设计判断。

#### 9.10.4 下一批的顺序

1. W-2026-09-19-01 有结论 → 合入 Guild（单进程即可验收：分布式锁、fenced 提交、恢复）。
2. `ownerroute`：玩家寻址的调用路由到持有者；saga / 效果消费者按所有者分区。
3. 两个 game 进程的实跑：一个 guild 被两个进程写，机器人只连其中一个。

### 9.11 第十九批（2026-09-19）：按所有者分区，两个 game 进程的实跑变绿

§9.10.4 的第 2、3 条。目标只有一个可验收的句子：**两个 game 进程对着同一份存储跑完整机器人场景，
两个进程都活着。** §9.10.3 的那次实跑里第一个进程在几秒内就带着
`fatal projection version conflict` 退出了。

#### 9.11.1 三个各自独立的写冲突

一条一条查出来的，形状完全不同：

| 冲突文档 | 谁在写 | 根因 |
| --- | --- | --- |
| `game/player/<id>` | 两个进程的 saga 步骤消费者 | 步骤 durable 是共享的，命令投给"闲着的"进程而不是"持有这个玩家的"进程 |
| `game/world/1034` | 两个进程各自的 World 单例 | `WorldUniqueID` 是常量 `1`，注释却写着"每个 game server 各有一个" |
| （不是写冲突）`battle not found` | 两个进程各自的 matchmaker | 谁提交了匹配谁就在**自己内存里**建战斗，玩家却连在另一个进程上 |

#### 9.11.2 所有权表：`game/playerroute`

一个 Redis key 一个玩家，值是持有者 sid，带 30 秒租约、10 秒续约。`Claim` 是 insert-only（SetNX），
所以"没人持有"时两个进程抢同一份工作只会有一个赢；`GetRoute` 把无人持有报成 **NOT FOUND** 而不是"我"——
报"我"就等于每个进程都是每个空闲玩家的持有者，正是这张表要防的事。

一个反直觉的结论写在这里，因为它是第一版做错的地方：**所有权不跟着连接走，跟着实体走。**
第一版在最后一个 session 关闭时释放，实跑里立刻看见六个刚下线玩家的礼物步骤在几秒内漂到另一个进程——
Player 实体在连接断开后仍然驻留在原进程（demo 从不卸载它），于是"释放"等于把文档交给另一个进程去加载第二份。
现在只有两种情况结束所有权：实体被销毁（demo 里不会发生，因此长驻进程会持续持有）或进程没了、租约自然到期。
登录因此变成 fail-closed：落在非持有者进程上的登录被 `player_elsewhere`（100015）明确拒绝，
客户端改连持有者的网关，或者等对方的租约到期——demo 没有把驻留实体在进程间搬家的手段，
"明确拒绝"和"悄悄写坏"之间只能选前者。

#### 9.11.3 准入必须排在认领之前（core 的新缝 `StepConsumerConfig.Admit`）

第一版把"这个玩家不归我"写在 handler 的第一行，实跑直接证明这个位置是错的：
`SubscribeDataEngineStep` 在调用 handler **之前**就 `inbox.Reserve` 了，于是错误的进程先拿走一个
两分钟的 Mongo 租约，真正的持有者反而执行不了，消息在两个消费者之间空转——16 个礼物产生了
**248 次投递、24 条卡住**。

所以 core 加了 `saga.StepConsumerConfig.Admit`：在两个步骤消费者里都排在任何认领之前被调用，
返回错误就原样 nak，不留任何痕迹。demo 的 `admitOwned` 挂在写 Player 的两个步骤上
（debit / debit-undo），不写 Player 的 deliver 保持 nil——邮件谁都能发，钉在持有者身上只会让它等。

这仍然是**背压而不是路由**：消息只是再次投给 durable 的任意消费者，持有者往往要弹一两次才拿到。
真正去掉弹跳要按所有者分区消费者，是下一批。拒绝现在会打一条 info 日志，否则这件事在进程里完全不可见。

#### 9.11.4 World 的 id 带上 sid，matchmaker 只匹配自己持有的玩家

World 从 `const WorldUniqueID int64 = 1` 变成 `WorldUniqueID(registry) = sid`：注释一直说"每个 game server
各有一个"，只是 id 没有兑现这句话。Scene 是 `noPersist`，两个进程各有一份内存对象不写同一个文档，不动。

matchmaker 在分组前按所有权过滤候选票（`OwnedHere`，**不**认领——只是看一眼就把空闲玩家拉进自己进程是错的）。
代价写在这里：**跨进程的匹配做不了**，它需要一个战斗宿主和把另一方输入路由过去的能力，demo 没有；
现在的失败形态是"队列多等一会儿"，而不是"建出一场谁都打不了的战斗"。

#### 9.11.5 验收

两个进程（sid 1000 / 1001，各自的 WAL 目录、ops 与客户端端口）对着同一套 Mongo / Redis / NATS：

| 跑法 | 结果 |
| --- | --- |
| 16 机器人全连进程 1 | 两个进程都活着；进程 2 拒绝了 89 个不归它的步骤 |
| 2 机器人（场景的设计点）连进程 1 | `success=2 failure=0`，两个进程都活着 |
| 2 机器人连进程 2 | `success=2 failure=0`；这次换进程 1 拒绝了 11 个 |

16 个机器人时场景本身会红在 `scene_expect`（它断言视野里正好 2 个主体），那是场景的设定，不是进程的问题。

`game/playerroute` 带表驱动单测：一个玩家一个持有者、持有者续约、非持有者续约不生效、租约到期后换人、
越权释放无效、无人持有报 NOT FOUND。用变异验证过（把 Refresh 的所有者校验去掉，测试立刻红）。

#### 9.11.6 没做的

- **按所有者分区的消费者**：现在是背压，不是路由。
- **驻留实体的进程间搬家**：所以跨进程登录只能拒绝。
- **跨进程匹配 / 战斗宿主**。
- Guild（第十八批）仍卡在 W-2026-09-19-01。

## 8. 相关文件速查

- 源码：`roost-codegen/demo/**`（`.tmpl`）、`roost-codegen/demo/README.md`（对新人的解释，按链路写）
- 脚手架：`roost-codegen/internal/roost/demo.go`（步骤表、占位符、落盘映射、run 步骤）、`demo_test.go`
- 模板改动：`internal/roost/render_player_tcp.go`（`RegistryBound`）、`internal/roost/add_workflow.go`（端点实体 id）
- CI：`roost-codegen/.github/workflows/framework-compat.yml`（`demo` scenario）
- kit：`roost-kit/service/account/identity.go`（`RegistryBound`）、`account_mod.go`（`bindCollaborators`）
- 生成工程内：`demo/README.md` 复制进去的说明、`deploy/dev/observability/README.md`（指标 ↔ 链路）

#### 9.2.3 清关奖励的领取窗口（RR-20260918-04 修正 §9.2.1 同批的 dungeon 版本）

- **被推翻的是什么**：U-0226 的账本保留期（4h）论证为"`session.run_ttl` 30m 一到 run 就没了，重放到不了奖励路径"。
  session 的 Runs / Claims **没有存储 TTL**，`run_ttl` 管的是 run 能开多久；succeeded 的 run 永远可以再 Finish 一次。
  于是另一笔领取清掉旧记录之后，重放旧 run 再发一次奖励——两次合法清关付出 300。
- **改成什么**：账本存 run 的**结算时刻**（`run.FinishedAtUnix`，session 服务盖的章，不是 `time.Now()`、不是请求里的东西），
  准入与清理共用 `dungeon.ClaimWindowClosed(resolvedAt, now)`。于是"记录被清掉 ⟺ 该 run 的领取被拒"，
  不再引用任何别的服务的存储行为。准入检查放在**付款的那个事务里**，是所有入口共同的咽喉。
- **不把重复发奖换成静默漏奖**：过窗口回 `dungeon_claim_window`(100012) 并在端点打 Warn（玩家、run、结算时刻），
  补发是运营口径。一条"有名字的拒绝"是这个方案能成立的前提。
- **旧数据**：老记录存的是领取时刻 ≥ 结算时刻，按新读法只会更晚过期，不会更早——不需要迁移。
- **教训（写给下一次给账本收界的人）**：给"记住一批东西"的结构定界，判据必须是这个结构自己能证明的事实；
  引用别的服务"会忘掉"，要先确认那条遗忘**真实存在**、且不会被一行配置改掉。清理谓词与准入谓词必须是同一个函数。

### 9.3 第十二批（2026-09-18 晚）：把三块零覆盖的框架能力接进 demo

选这三条的判据见 §9.1 的按语：它们是"框架里有、demo 里一行使用方都没有"的能力，而今天的 RR-20260918-01 正说明这种
地方缺陷能活多久。三条都只加 demo，不改框架。

#### 9.3.1 实体同步：Player 成为复制主体，scene 是它的调度器（§9.1 第 1 条）

- **缺什么**：`sync=true`、`entitysync`、`room` 的订阅/水位这一整条"服务端权威状态推给客户端"的主路径，demo 一次都没走过；
  此前只有战斗内的 lockstep 帧同步和手写推送。
- **做法**：四块，每块都是框架现成的——
  `Player.Sync()`（主体：版本、脏掩码、packer）→ `room.RoomBroadcaster`（调度：谁订阅了谁、什么时候 flush）→
  `room.RoomTransportSink`（编码：信封 → 每会话的线帧）→ `AtomicBatchTransport`（通道：demo 的 TCP 推送）。
  `internal/service/<game>/scene.go` 是这四块的装配；`game/entities/player/sync_packer.go` 是 packer。
- **写入侧不是自动的**：把 DAO 标脏和把**主体**标脏是两件事，`Player.PublishSyncDirty()` 是游戏决定第二件何时发生的地方
  （一次变更一次调用 = 一条 delta，而不是每个 setter 一条）。这也是为什么复制不能做成"有人调了 setter"的副作用——
  它是"一次已提交的变更"的副作用。
- **payload 用 DAO 自己的同步文档**（`MarshalSync(mask)`，与 `ApplySync` 成对）。掩码从生成的 setter 来、原样回到生成的
  marshaller，两端都不需要知道哪个 bit 是哪个字段——因为**生成的字段掩码常量是 DAO 包私有的**，别的包里的 packer 根本
  没法按字段裁剪。这条已写进 WANTED 交 review。
- **水位**：`kit/dataengine.Mod.DurableLSN()` 装进 `RoomManagerConfig.DurableWatermark`——U-0233（RR-20260918-02）
  加的那个入口的第一个真实使用方。
- **边界（写在文件头）**：没有兴趣管理，所有人订阅所有人，O(n²)，只因为 demo 世界只有几个机器人；AOI 应该插在
  Subscribe/Unsubscribe 前面而 scene 其余部分不变。生成的接入层**没有会话关闭回调**，所以"谁还在线"只能靠推送失败懒清理
  + 入场时按 `ActiveSessions` 扫一遍（chat presence 有同样的问题）——这条也进了 WANTED。
- **测试**：随工程生成 `internal/service/game/scene_test.go`——一个玩家经真实 Nest 事务改了 DAO，另一个玩家的线上收到可解码的
  delta（reliable 快照整帧、delta 走分片重组，与 UDP 客户端同一套代码）。去掉 `PublishSyncDirty` 即红。
  机器人加 `scene_watch` / `scene_expect`：真客户端解码、合并、断言。**注册推送解码器不是可选的记账**——漏了它推送到了也解不出来，
  第一次实跑就是这么红的。

#### 9.3.2 attribute 接进持久化与同步，层间合成定死（§9.1 第 2 条）

- **缺什么**：`game/gameplay/attribute` 此前只在内存里、谁也没用它；`Container` 的层怎么合成没定。
- **层**：`Base`（玩家自己的值，持久化 + 复制）、`Gear`（背包的投影，进程内）、`Final`（合成视图）。
  `game/entities/player/attribute_component.go` 是容器的归属地，`OnInitFinish` 是填充它的时机（生成的 builder 先挂 DAO 再初始化组件）。
- **合成规则**：`Final = Base + Gear`（逐属性相加），**然后**才 `Update()` 重算派生属性。两件事随之而来：派生属性只算一次、
  且是在合成后的输入上算的（把各层自己的 Power 相加 = 把两个各用一半输入算出来的评分加起来，那不是 power 的意思）；
  合成是层的纯函数，所以 Final 永远不落库。
- **两种存储意图各用一次**：`AttrBase` 是 `persist,sync`，`AttrFinal` 是 `nopersist,sync`——能从已存的东西推出来的值不存，
  存了就是第二个真相、会和第一个打架；但它是客户端要画的东西，所以照样复制。
- **配置数据进属性**：item 表加 `attack` / `hp` 两列，Gear 层是背包按配置求和——改剑的攻击是一次表编辑 + 热更，不是一次构建。
- **测试**：`game/entities/player/attribute_component_test.go` 断言合成规则与两个意图。
  一条诚实的记录：这个公式下"各层评分相加"与"合成后再算"数值上恰好相等（基础 HP 恒为除数的倍数、余数不会进位），
  所以断言写的是规则本身而不是"≠ 相加"，并在测试里写明为什么——造数据去凑出差异是本末倒置。
- **实跑证据**：Mongo 里 `attr_base: {1:120, 2:14, 3:40}`（等级 2 的 HP/攻击/战力），**没有 `attr_final`**；
  而机器人的 `scene_expect` 要求 `attr_final` 必须到达——只存在于线上、从不落库的那个视图确实被复制了。

#### 9.3.3 rank 服务进链路（§9.1 第 3 条）

- **缺什么**：kit 的 rank 服务零使用方。
- **做法**：`frameworkCatalog` 加 `rank`，game / game-demo 模板自动托管它（ops 端口 9105，session 顺延 9106）。
  `game/ranking/ranking.go` 是游戏这侧的决定：哪个榜、一分是什么、什么让提交可重试。
- **幂等键与领奖账本同源**：清关提交用 `UpdateAdd`（本身**不**可重放）+ requestID = `clear:<run id>`——和奖励账本键的是
  同一个 run 身份。一次清关只发一次奖、只记一分，两件事同源不是巧合。
- **提交故意不以 `result.Claimed` 为条件**：重放发现奖励已发，恰恰是"分可能还没记上"的那种情况。
- **边界**：提交在事务**外**（榜在另一个服务），两者不原子；崩在中间会留下"已发奖但没记分"，这是可恢复的方向，
  重试这个端点会因幂等键只记一次。
- **实跑证据**：12 个玩家（两轮机器人）每人恰好 1 分，而每个玩家都调了 `finish_dungeon` 与 `finish_dungeon_replay`
  两次提交——幂等键生效。机器人 `rank_top` 断言自己在榜上且自己的值为 1。

### 9.4 第十三批（2026-09-18 晚）：地图，按 cube 的 scene 组合形状

参考 cube 的 `game/entities/scene`：**组合形状照搬，同步逻辑不参考**（roost 用第十二批那条 subject → room 的规则）。
cube 的 terrain / pathfind / block AOI 对应的原语 roost-core `spatial` 里全都有，而且多出滞回、距离分带与 `MaxVisible`，
所以框架侧一行不用加——要写的是组合、`FindPlace`（spatial 没有）、以及 AOI 事件到订阅的桥接。

两处由用户拍板的设计：Scene **是**一个实体，但 runtime 的各个 system 线程安全（内部锁），实体按接口导出它们；
位置权威在 DAO，别处不缓存，读写都落到组件。

#### 9.4.1 第一批：地图与移动（已完成）

- `game/scene`（契约）/ `game/scene/runtime`（system 实现）/ `game/entities/scene`（实体）三层分开，
  互不成环——system 需要的那点实体能力由契约包里的 `scene.Entity` 接口给出（当前只有 `ID()`）。
- `System` 生命周期是 `Name/Init(ctx)/Start/Stop`，按序启动、逆序停止，`New` 里先把所有 `Init` 跑完再 `Start`，
  于是一个 system 可以在 Init 时持有列表里靠后者的指针。启动失败逆序回卷已启动的部分。
- **锁的归属**：system 自带锁，不借实体锁。理由写在 terrain 的文件头——地形查询来自端点 / 计时器 / 将来的 AOI tick，
  让它们排队等实体锁等于把地图变成整个场景的瓶颈；实体锁排的是实体**状态**的事务顺序，那是另一个问题。
- **位置**：`PosX` / `PosY` / `SceneID` 在 Player DAO 上（`persist,sync`），`MapComponent` 是唯一的门。
  于是移动没有单独的广播——它走第十二批那条复制链（机器人的 `scene_expect` 现在要求 `pos_x` 到达）。
  AOI 将来那份 id→坐标是**索引不是缓存**：只由这一条写入路径更新，永远不被当作"X 在哪"的答案读。
- **移动是两实体事务**（Scene rank 2 → Player rank 4，`durability=async`），因为"玩家记录的位置"和"地图交出去的地面"
  必须一致。地形判断留在 `MapComponent.MoveTo` 里（唯一写入路径），handler 不重复判一遍。
- **玩家不占地**：用 `Walkable` 而不是 `Occupy`（cube 的 `AddToMap` 有 `checkObstacle` 开关，是同一个选择）。
  好处是断线没有残留占位要回收——而 demo 恰恰没有断连回调（W-2026-09-18-03）。占位留给墙与将来的怪。
- **`Place` 向外一圈圈找**最近可站点：地图每次重建，玩家记住的位置可能不再可用，登录不该因此失败。这是 spatial 里没有、
  cube 有（`FindPlace`）、写在 demo 侧的那块。
- 测试：`game/scene/runtime/runtime_test.go`（边界、占位、最近可站点、穿不过墙、停止后回错而不是 panic）；
  机器人 `move` + `move_out_of_bounds`（越界必须被拒，且答复带玩家仍然所在的位置）。
- 实跑：6 机器人全过、0 ERROR，Mongo 里 `pos_x: 501`（从出生点 500 走了一步）、`scene_id` 是场景的完整实体 id。

#### 9.4.2 第二批：AOI 接管订阅（已完成）

把"谁该收到谁的状态"从"所有人订阅所有人"换成兴趣系统回答。两处用户拍板改变了做法：**全程用 entity id**、
**"订阅自己"做成一类关系而不是特例**。

- **每一种理由都是同一种来源**：`spatial.InterestManager`（距离）、`self`（永远看得见自己）、`team`（匹配成队）
  都实现 `scene.Source`，产出同一个 `spatial.InterestEvent`。关系来源是 `RelationSource`——集合驱动，
  谁拥有这段关系谁推进来，它把差分变成 Enter/Leave。好友 / 同盟是同一个类型换一个 feed，这就是它不叫 TeamSource 的原因。
- **汇总层按来源计数**：第一个来源命中才 Subscribe，**最后一个**来源撤销才 Unsubscribe。这是有了第二个来源之后被强制的东西——
  一对 (观察者, 主体) 可能被多个来源同时持有（队友正好站在旁边），少了引用计数，队友走出视野会把关系来源仍然需要的订阅退掉，
  而这个 bug 只在"两个来源重叠又分开"的时序里出现。档位合并取最高保真（band 最小者胜），即关系压过距离。
- **"订阅自己"因此不再是特判**：`spatial` 的 `evaluatePair` 第一行就拒绝自观察，而"我永远看得见自己"本来也不是距离的事。
  做成最退化的那种关系之后，桥接里一行特判都没有。
- **id 空间**：全程 entity id（跨 kind 唯一），转成传输会话只在 `RoomSessionResolver` 一处。demo 跨这条边界只有两个地方：
  resolver（entity id → 会话）与 `SetTeam`（匹配给的 player id → entity id）。
- **`internal/service/<game>/scene.go` 退化成复制桥接**：不再决定谁订阅谁，只把兴趣系统交回的 `[]SubscriptionChange`
  说给 room 听。加一种关系不会碰到这个文件。
- **参数**：进圈 120 / 出圈 150 / 格边长 150。格数是 `(⌈2·出圈/格边⌉+1)²`，格子远小于视野不会让 AOI 更准
  （半径判定本来就精确），只会让观察者每动一步的簿记成倍增加；框架不强制这个比值（W-2026-09-18-05），所以写在注释里。
- **分带暂时只一档**：档位要能裁字段才有意义，而生成的字段掩码常量是 DAO 包私有的（W-2026-09-18-02）。
  汇总层已经带 band，那天到了是一次配置改动。
- 测试：`game/scene/runtime/interest_test.go` 五条——自己经关系订阅、距离进出、**边界抖动不产生任何事件**（滞回，
  roost 比 cube 多出来的那部分）、关系在距离撤销后仍保住订阅、离场双向释放。实跑 6 机器人全过、0 ERROR、无 scene 告警。
- **未做**：多房间（`spatial.InterestCluster`）；非玩家主体的入口（`Show`/`MoveShown`/`Hide`）已留好，第三批的怪用。

#### 9.4.3 第三批：object refresh（刷怪）（已完成）

照 cube `RefreshManager` 的骨架，但只做必须的三件事：按组维持存活数、死亡后按延迟排队重生、落点经 `Place` 选。

- **`Monster` 实体**（kind 4，`noPersist=true lifetime=ephemeral`，`sync=true`）：证明 AOI 与复制对非玩家主体一视同仁——
  它作为"只被看、不看"的 subject 进兴趣系统（`Show`/`Hide`），其余一整条链路（room、packer、线上格式）与 Player 同一份代码。
- **`MonsterDao` 每个字段都是 `nopersist,sync`**：不存，但复制。这是为了让"位置住在 DAO 里、经组件读写"这条规则
  对所有实体一致——否则 demo 里会有两种位置权威。代价与取舍记在 W-2026-09-18-09。
- **刷新策略是配置**（`configs/schema/spawn.go` + `spawn.csv`：组、模板、数量、血量、中心点、半径、重生秒数）。
  表在每次 `Due` 时读，不缓存——热更下一 tick 生效。
- **系统只说"该生成什么"，装配层去建**：`Refresh.Due(now)` 返回 `[]SpawnRequest`，
  `internal/service/<game>/spawner.go` 建实体、放位置、注册进 room 与 AOI，然后才 `Spawned` 回报。
  与兴趣系统"只产出订阅变更、不直接调 room"是同一个形状，也正好避开包环（建实体要 lifecycle，lifecycle 要实体包，
  实体包持有 runtime）。
- **数的是"被告知存在的"而不是"被请求过的"**：一次失败的创建会在下一次 `Due` 里重新出现；
  若按请求扣减，进程余下的时间里都会少一只而且没人会说。
- 测试：`game/scene/runtime/refresh_test.go` 四条——新场景一次要满、未回报的请求会再来一次、死亡要等表里的延迟、
  落点可站且分散。实跑：GM `gm.scene.population` 报 3 只，`gm.scene.kill` 后 3→2，20 秒（表里的值）后回到 3 且是**新 id**；
  6 机器人全过，`scene_expect` 要求至少 2 个 subject（自己 + 一只怪）。
- **未做**：怪不动（没有 AI / actionflow，位置写好就不再变）；战斗只有 GM 的"杀掉"，没有玩家可发起的伤害；
  id 由进程本地计数器生成，第二个进程会撞（W-2026-09-18-10）。

### 9.5 第十三批的遗留：拿不准的、与既有约束冲突的、新加的

**与既有约束冲突（已在 WANTED 登记，等 review）**

| 冲突 | 现在怎么处理的 | 条目 |
| --- | --- | --- |
| "位置权威在 DAO" vs 无持久化的怪 | 给它一个全 `nopersist,sync` 的 DAO，规则对所有实体一致 | W-09 |
| "全程 entity id" vs 运行期实体没有 id 分配器 | 进程本地计数器 + 基数，单进程正确 | W-10 |
| "system 是纯状态机、不自带 goroutine"（第一批定的） vs 刷新需要计时器 | 系统仍是状态机（`Due(now)` 要传时间进去），**计时器在装配层**（`spawner.go` 的 ticker） | 无（本批内自洽，但值得 review 确认这条分界） |
| AOI 分带 vs packer 不能按字段裁剪 | 只用一档，汇总层已带 band | W-02 |

**拿不准的**

- **档位合并取"最高保真"**（关系压过距离）是实现侧默认，不是产品决定——同盟成员在地图另一头也收全量状态，带宽照付（W-08）。
- **`InterestConfig` 没有订阅规模上界**，demo 靠注释里的"格边长 ≈ 视野半径"约束自己（W-05）。
- **`spatial` 的 subject / observer 与 entitysync 的 subject 同名不同物**，AOI 的点也不是实体 pos——两处都只写在注释里（W-07）。
- **怪的 `Hide` 与 room 的 `RetireSubject` 之间没有事务**：先告诉 AOI 再退 room，中间崩溃会留下一个 room 还持有、
  但没人订阅的 subject。单进程 + 进程退出即清空的前提下无害，多进程要重新看。

**新加的能力（供 review 一并看）**

- `game/scene`：契约包（Terrain / PathFind / Interest / Relations / Refresh / SubscriptionChange / SpawnRequest）。
- `game/scene/runtime`：五个 system（terrain、path_find、interest、refresh，加组合本身），各自带锁。
- `game/entities/scene`：Scene 实体（无 DAO）；`game/entities/monster`：Monster 实体（全 nopersist 的 DAO）。
- 新端点 `Move`(10017)，新 GM 命令 `gm.scene.population` / `gm.scene.kill`，新错误码 `scene_position`(100013)。
- 新配置表 `spawn`；item 表加 `attack` / `hp` 两列（第十二批）。
- 机器人：`move`、`move_out_of_bounds`，`scene_expect` 加 `subjects` 参数。

### 9.6 第十四批（2026-09-19）：装备栏（嵌套字段）与数据版本迁移

两条接在一起做：加一个嵌套字段本身就是一次真实的 schema 变更，于是迁移有了不是编造出来的主题。

#### 9.6.1 装备栏：demo 的第一个嵌套 DAO 字段（§9.1 第 8 条）

- **为什么值得做**：U-0236、U-0238 两个 P1 都住在"嵌套里再嵌套"那条路径上，而**生成的工程里一个使用方都没有**——
  只有生成器自己的运行时门看得见。加一个字段，demo 的测试与实跑就都覆盖它。
- **形状**：`PlayerDao.Equipment`（嵌套 struct）→ `Slots map[int32]*GearPiece`（嵌套里的指针 map），
  正是那两条缺陷的形状。`game/equipment` 定槽位与"什么能穿"，`EquipmentComponent` 是唯一写入口。
- **穿装备是一个事务**：从背包取出、穿上、把换下来的放回背包——三件事一起提交，否则崩在中间要么丢件要么复制件。
  属性的 Gear 层随之改成**从穿戴集算**而不是从整个背包算（背着一把剑不该让人变强）。
- **它立刻抓到一条新缺陷**：见 §9.6.3。
- 测试：`equipment_component_test.go` 四条（两级下的改动落库、换下的件不再落库、回滚后所有权双向归位）；
  机器人加 `equip`，`scene_expect` 要求 `equipment` 出现在复制载荷里。
  实跑：Mongo 里 `equipment.slots.1 = {item_id: 2001, level: 1}`、`_schema: 2`、背包里那把剑已扣除。

#### 9.6.2 数据版本迁移（§9.1 第 7 条）

- **先补一个生成器缺口**：`<Dao>SchemaVersion` 此前在模板里**写死为 1**，于是生成的 `Migrate` 里
  `from` 与 `target` 恒等——框架整套迁移机制（`migration.RegisterDAO` + `RestorePersisted` 的版本分支）
  **没有任何生成的工程能触发**。`//roost:dao` 加 `schema=N`（省略为 1，0 与非数字拒绝）。
- **主题是真实的结构变更**：v1 把武器存成 Player 上的扁平 `weapon_id`，v2 存成 `equipment` 子文档按槽位键。
  这正是零值覆盖不了的那种——**加字段不需要迁移**（BSON 解码给零值），改形状才需要。
- **注册是显式的**，不是 `init()`：一个因为包恰好被链接进来而运行的迁移，是没人决定要运行的迁移。
  服务 `Init` 的第一件事就是注册，晚于它的注册会让最初几次装载漏掉。
- **步骤只能读老版本真的有的东西**，且要接受老文档里数字的各种 Go 类型（驱动与写它的那个 build 决定）。
- 测试六条：三条测变换本身（带武器、没武器、三种数字类型），三条测**接线**——
  `RestorePersisted` 在 v1 文档上真的会跑、在当前版本上不跑、比自己新的版本拒绝装载。
  "一个正确但从不被调用的步骤"才是这里真正的失败模式。
- **边界**：补丁只写 DAO 认识的字段，所以老文档里的 `weapon_id` 不会被 unset，会作为遗留键留在文档里；
  要清掉得靠一次性的离线脚本，demo 没做。

#### 9.6.3 顺带抓到的缺陷：U-0245

加完嵌套字段，`equipment_component_test.go` 立刻红了两条。根因是 `Init()`（接嵌套回调）**只在装载路径上被调用**，
`New<Dao>()` 不调——于是**新建**的实体在第一次存盘前，所有穿过嵌套值的写入都不标脏、不落库、不报错。

三个条件凑齐才藏住它：运行时门的每个 harness 都自己调了 `Init()`；顶层 Kind 3 的 setter 在赋值后自己会绑
（所以"先 Set 再改"是好的）；而 demo 此前没有嵌套字段。掉进坑里的恰恰是组件的正常写法——**只穿过嵌套值改**。

这条正好印证第 8 条的判据：**零覆盖的地方就是缺陷能长期活着的地方**。记录见 `docs/bugfix/U-0245-fresh-dao-nested-wiring.md`。
