---
name: roost-genre
description: 按一段业务需求用 roost-core（Go 游戏服务器框架）生成或扩展服务端工程：开场问清客户端、人数、局外状态、战斗与参与方式，判断需求是否真的需要这套服务器框架（不需要时给 Steam Lobby 等替代方案），拆需求、分服务器与客户端、复用业务模块、手写其余、离线验收、起服冒烟，并在工程里生成说明文档、客户端对接文档与导出目录；支持在已有工程上持续增改需求，也能被别的 agent（例如客户端工程里的 agent）调用并回交固定格式的摘要。其次也能按玩法（SLG、MOBA、MMORPG、卡牌、塔防、放置等）出最小 demo 并说明框架边界。触发词：用 roost 做…、按需求生成游戏服务器、给 roost 工程加功能 / 改需求、客户端要对接 roost 服务端、某类玩法能不能用 roost、roost-genre、$roost-genre。
---

# roost-genre：按需求生成游戏服务端，按玩法出 demo

本文只写当前版本的事实。目标 roost-core 版本早于本 skill 所在版本时，读那个 tag 里的 skill，不把这里的命令套上去。下文 Markdown 链接相对本文件所在目录，反引号里的路径相对 roost-core 根目录（取发布版时就是模块缓存里的 `$CORE`）。

## 0. 路由

| 任务 | 读哪里 |
| --- | --- |
| 先判断这个需求要不要 roost | [compose 第 2 步](references/compose.md#2-判断要不要-roost)；不需要时的替代方案见 [steam-lobby-ngo.md](references/steam-lobby-ngo.md) |
| 按一段需求生成服务端，或在已有工程上加功能、改需求 | [references/compose.md](references/compose.md)，从开头的步骤总览照做 |
| 被别的 agent 调用（例如客户端工程里的 agent） | 本文 [§1](#1-与其他-agent-对接)，再按 compose 做 |
| 照某类玩法出一个最小 demo，或问某类玩法在 roost 上的限制 | 本文 §2～§7 与对应的 reference |
| 问 roost 有没有某项能力 | [能力清单](../../framework/CAPABILITIES.md)，不凭本文摘要回答 |
| 问某种客户端（Unity、Unity WebGL、手机、网页、Go、C++ / Unreal）能不能连、用哪个 SDK | [compose 客户端表](references/compose.md#客户端能连什么按客户端类型) |
| 改框架、修 bug、发版 | 不用本 skill |

两条总规则（细节在 compose）：

- **skill 对使用它的 agent 只读**：不改本目录与装到工程里的入口副本，不改 roost-core 的模块模板与框架。照做走不通时告诉用户哪份文档哪一节、原文、实际情况、怎么绕过的；绕过的办法只记进工程的 `docs/DEMO-HISTORY.zh-CN.md`。
- **记忆放在工程里**：`docs/DEMO.zh-CN.md`（当前状态）与 `docs/DEMO-HISTORY.zh-CN.md`（逐次变更），后续增改需求先读它们。

## 1. 与其他 agent 对接

本 skill 常被别的 skill 或 agent 包含调用（例如 Unity 客户端工程里的 agent 需要一个服务端）。**不要假设调用方能交互**：需要用户决定的事放进交付摘要的“待决定”栏，按推荐默认继续，不卡住。

**调用方应给的输入**（缺的项按右列处理，并写进 HISTORY）：

| 输入 | 缺了怎么办 |
| --- | --- |
| 需求原文 | 必须有；没有就只回交一句“缺需求原文”的摘要 |
| 客户端引擎与语言、发行平台（是否网页或 Unity WebGL、是否手机）、客户端工程路径 | 按需求推断；推断不出按“Unity 桌面 / C#、路径未知”处理，平台写进“待决定” |
| 服务端工程目录（新建还是已有） | 新建，放在调用方工作目录下的 `server/` |
| 模式：业余 / 专业 / 只给决策 | 业余 |
| 是否允许提问 | 不允许：开场问题与冲突一律按推荐默认，记成“待决定” |
| roost-core 版本 | 已有工程跟它的 `go.mod`；新工程用最新发布版 |

**产出**：服务端工程；`docs/DEMO.zh-CN.md`；`docs/DEMO-HISTORY.zh-CN.md`；`docs/CLIENT.zh-CN.md`（客户端 agent 的单一入口，同一份复制到 `shared/README.md`）；导出目录 `shared/`（协议与配置表的 C# 代码与数据，要提交）；专业模式与“只给决策”还有 `docs/DECISIONS.zh-CN.md`。格式见 [compose 第 12 步](references/compose.md#12-文档democlienthistory-与决策单)。

**结束时回给调用方的交付摘要**（固定格式，原样填写，上层 agent 可以直接转交给客户端 agent；compose 第 13 步的报告也带这一块）：

```markdown
## roost-genre 交付摘要

- 服务端工程：<绝对路径>
- roost-core 版本：<vX.Y.Z>；模式：<业余 / 专业 / 只给决策>
- 能做什么：<一两句白话>
- 客户端：<引擎 / 平台>；能用：<连 game 走 TCP / 开房间走 relay 的 TCP 或 WebSocket / …>；不支持：<…与替代做法；没有写“无”>（依据 compose 客户端表）
- 怎么起服：`cd <工程> && make dev-up && make dev-run`；开发地址 <工程里实际有的：登录入口 http://127.0.0.1:8081，玩家 TCP 127.0.0.1:7000，玩家 WebSocket、lobby、relay>
- 起服冒烟：<通过：登录与握手 / 未做：原因>
- 客户端入口文档：<工程>/docs/CLIENT.zh-CN.md（同一份在 <工程>/shared/README.md）
- 导出目录：<工程>/shared/protocol（manifest：protocol_def=<哈希>，dao_sync=<哈希>）；<工程>/shared/config（data_version=<短值>）
- 验收：module verify <全部 ok / 失败项>；手写场景 <通过数>/<DEMO 记的数>，0 SKIP
- 未做 / 待确认：<逐条>
- 待决定（需要调用方或用户决定）：<逐条，每条带推荐做法；没有写“无”>
```

**客户端一侧的 agent 拿到摘要后的第一步**：

1. 读 `docs/CLIENT.zh-CN.md`（或 `shared/README.md`），第 1 节是它的清单，开头一句写着本客户端能用与不支持的路；写着不支持的（例如网页客户端连 game）不要自己想办法硬连，回给用户；
2. 同步导出目录：有 roost CLI 时 `roost client sync`，没有时按 CLIENT 文档第 10 节直接拷 `shared/` 下的 C# 与数据；
3. 在客户端做一处能改服务器接入地址的地方（CLIENT 文档第 2 节），缺省值填开发地址；
4. 跑最小联调：服务端 `make dev-up && make dev-run`，客户端登录 → 握手 → `EnterGame`（开房间是登录 → 建房 → 连 relay），看到 CLIENT 文档第 1 节写的期望现象。

调用方是服务端一侧的上层 agent 时，把摘要与 CLIENT 文档路径原样转给客户端 agent，不要改写其中的地址与哈希。

## 2. 玩法选型

| 玩法 | reference | 支撑度 | 场景数（全部 PASS 才算过） |
| --- | --- | --- | --- |
| SLG / 大地图策略（建造、行军、抢地、联盟） | [references/slg.md](references/slg.md) | 高 | `TestSLG` 9 + 联盟 2（脚本形态）；只做建造、行军、抢地的精简形态 `TestSLG` 7 |
| MOBA / 竞技对战（帧同步） | [references/moba-lockstep.md](references/moba-lockstep.md) | 高 | `TestMOBA` 29 |
| MMORPG（组队、交易、战斗、Buff、切图） | [references/mmorpg.md](references/mmorpg.md) | 高 | `TestMMORPG` 14；game-demo 多地图 TCP 1 |
| 卡牌 / 回合制 RPG（抽卡、养成、回合 PvE） | [references/card-turn-based.md](references/card-turn-based.md) | 高 | `TestCardRPG` 17 |
| 房间合作 PvE：塔防 / 生存撤离 / 自走棋 | [references/room-coop-pve.md](references/room-coop-pve.md) | 高 | 11 / 13 / 13 |
| 放置模拟 / 合成休闲 | [references/idle-merge-casual.md](references/idle-merge-casual.md) | 高 | `TestIdleSim` 13 / `TestMergeCasual` 16 |
| 实时派对房间（2～8 人短局：抢答、计时、回合、轻反应） | [references/party-room.md](references/party-room.md) | 中高（心跳 ≤ 20 次 / 秒、房间无跨机接管、补位不超员只靠进房检查、无容量数据） | `TestPartyGame` 19 |
| 回合 PvP / 合作 Roguelike / 异步社交经营 | [references/session-genres.md](references/session-genres.md) | 高 | 10 / 10 / 9 |

- 场景数以各 `scripts/test-<玩法>-generated.sh` 的计数断言为准（MMORPG 多地图那 1 条在 `scripts/test-mmorpg-scenes.sh`）；本表与脚本不一致时以脚本为准。
- 13 类玩法本身不是可安装的模块。每类的夹具 = 用 `roost module add` 装几个现成模块（脚本的 `genre_modules` 列出）+ 手写模块没覆盖的部分；`project new` 也没有 `-template slg` 这类模板。完整参考应用是 `-template game-demo`。
- 用户的玩法跨两类时（MMO 里的竞技副本），分别按两篇 reference 取，不为“用全所有模块”硬拼。
- 夹具里的数值（金币、时长、上限）都是样例；玩法规则缺少默认实现不算框架缺陷。

## 3. 按玩法出 demo 的公共骨架

### 3.1 前置

- Go 版本按 roost-core 的 `go.mod`，全程 `GOWORK=off`；另要 python3、perl。
- CLI、`$CORE`（只读源码，含 `docs/`、`modules/`、`scripts/testdata/`）与工程依赖必须是同一个版本 `$V`。命令按发布版写；仓库里的玩法脚本按本地检出跑，维护者用检出时的写法见 [compose 附录 C](references/compose.md#附录-c维护者用本地检出时)。

### 3.2 公共前置命令（每篇 reference 的命令都接在它后面，用 bash）

```bash
set -euo pipefail
export GOWORK=off
V=<roost-core 版本>
WORK=/path/to/work                        # 放 CLI 与工程的目录
NAME=mygame; MOD=example.com/mygame       # 工程名与自定 Go module；同一台机器上的工程不能重名（Mongo 库名前缀等由它派生，见 compose 第 0 节）
mkdir -p "$WORK"
CORE=$(go mod download -json "github.com/tjbdwanghaibo/roost-core@$V" | python3 -c 'import json,sys;print(json.load(sys.stdin)["Dir"])')
GOBIN="$WORK/bin" go install "github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@$V"
ROOST="$WORK/bin/roost"; DIR="$WORK/$NAME"
# dao_add <实体类型名> <文件> "字段 类型 store键 [dao标签]" ...：在 `type <X>Dao struct {` 后插入字段，标签缺省 nosync（只落库）
dao_add() {
  local t=$1 f=$2 ins="" n ty k tag s; shift 2
  for s in "$@"; do read -r n ty k tag <<<"$s"; ins+=$'\t'"$n $ty \`store:\"$k\" dao:\"${tag:-nosync}\"\`"$'\n'; done
  T="$t" INS="$ins" perl -0pi -e 's/(type $ENV{T}Dao struct \{\n)/$1$ENV{INS}/ or die "no $ENV{T}Dao in $ARGV\n"' "$f"
}
# world <entity.go...>：多人争用的对象放进 World 锁档（锁序低于 Player）
world() { perl -pi -e 's/category=entity\.EntityCategoryOther/category=entity.EntityCategoryWorld/' "$@"; }
# fix_mod <夹具工程名> <文件...>：夹具 import 写死 example.com/<genre>-practice，换成自己的 module
fix_mod() { local from=$1; shift; FROM="example.com/$from" TO="$MOD" perl -pi -e 's#\Q$ENV{FROM}\E(?=[/"])#$ENV{TO}#g' "$@"; }

"$ROOST" project new "$NAME" -module "$MOD" -out "$DIR" -template game -roost-core-version "$V"
cd "$DIR"
```

- `-template game` 只有 game 一个业务服务，带 Player 与 World 两个实体，缺省不托管框架服务；模块用到哪个托管服务就在安装时声明哪个。各纵切夹具不经生成的访问器调托管服务，所以不需要 `-framework`。
- 三个 helper 只改业务文件：`dao_add` 每项第四个词是 dao 标签，缺省 `nosync`（只落库，账本、回执、种子用它）；要给客户端看的字段显式写 `persist,sync` 或 `nopersist,sync`。组件、访问器、Sender 由 `roost generate` 生成，不手改。
- 用 `-practice` 原名（如 `MOD=example.com/slg-practice`）时 `fix_mod` 是空操作；自定 module 名必须做。`$CORE` 只读，夹具只从它拷出。

### 3.3 公共顺序

`modules.sh add` 装模块 → `add entity <X>` + `add dao <X> --entity <X>` → `add timer --entity <X>`（需要持久期限时）→ `dao_add` 填字段 → `world` 改锁档 → 拷 `$CORE/scripts/testdata/<genre>/*.go.txt` → `fix_mod` → `gofmt -w` → 装公共 Robot（`python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" <genre>`）→ `roost generate` → 跑场景 → `modules.sh verify`。

### 3.4 验证（每篇都以此收尾）

```bash
go test -mod=mod -race -count=1 -timeout 3m -v -run '^Test<前缀>' <包...> 2>&1 | tee "$WORK/test.log"
grep -c '^--- PASS: Test<前缀>' "$WORK/test.log" || true   # 必须等于 reference 给的场景数
grep -c '^--- SKIP' "$WORK/test.log" || true              # 必须为 0
go build -mod=mod ./...
go vet -mod=mod ./...
"$ROOST" id check            # 期望输出 IDs are valid
"$ROOST" generate --check    # 期望输出 generated files are up to date
"$ROOST" project doctor || true   # 不应有 FAIL 行
```

- 计数和 SKIP 都要核对：`-run` 写错或用例被跳过时 `go test` 照样 `ok`。
- `modules.sh verify` 打印每个模块 `<名> n/n`，n 等于 `$CORE/modules/<名>/module.yaml` 的 `verify.pass`。
- `project doctor`：`-template game` 工程不应有 FAIL；`-framework` 带上 account / chat / platform / session 而没有模块接管时各报一项 `collaborators:<服务>` FAIL（生成的拒绝桩，上线前由工程实现）。

### 3.5 一键方式（要 roost-core 检出）

```bash
cd "$CORE" && ROOST_<GENRE>_KEEP=1 bash scripts/test-<genre>-generated.sh   # 例：ROOST_SLG_KEEP=1 bash scripts/test-slg-generated.sh
```

脚本打印临时目录，工程在其中的 `project/`，module 固定为 `example.com/<genre>-practice`；成功即删，`KEEP=1` 保留。批量验收用 `python3 scripts/test-genres-generated.py --out <新目录>`，口径见 `docs/framework/ROBOT.md`。

### 3.6 Robot 与网络验收

每篇的公共 Robot 安装器给工程加业务 PB、TCP 到 Sender 的接线、`cmd/robot` 与网络用例，场景数已含 Robot 那一条。用户要压测时沿用 `robot` 的 transport / session / scenario / runner / loadtest；生成新玩法时，自己的 PB、端点、Robot 步骤、数据断言与说明要一起交付，只编译或直接调 Nest 不算网络验收。真实 Gate / 账号 / 存储的通用链路另有独立验证，不自动覆盖每种玩法；功能、容量、长稳、存储和客户端平台分别出结论。

## 4. “可运行”的两档

| 档 | 含义 | 需要什么 | 证明到哪 |
| --- | --- | --- | --- |
| 一档：`go test` 可运行（缺省） | 纵切测试用替身提交器 + `nest.NewEngine` 驱动生成的 Sender / Nest / DAO / 定时器 | 只要 Go | 事务、回滚、锁序、幂等、期限；**不含**文件 WAL、Mongo、真实重载与投递循环 |
| 二档：真起进程 / 真实依赖 | 进程连 Mongo 副本集、NATS（JetStream）、Redis | 生成工程 `make dev-up`（docker compose）+ `make dev-run`；或 roost-core 检出里的 `wiring/scripts/integration/dataengine-env.sh up`（本机二进制，用 `ROOST_IT_HOME` / `ROOST_IT_PORT_OFFSET` 隔离） | 各纵切脚本的 `ROOST_<GENRE>_REAL=1` 追加崩溃恢复等用例，地址经 `ROOST_DATAENGINE_IT_MONGO_URI` / `_REDIS_ADDR` / `_NATS_URL`（`wiring/scripts/integration/README.md`） |

- `make dev-smoke` 只在 `-template game-demo` 工程里有 `cmd/loadtest`。
- 没有被要求时不起外部服务；不要指向生产库或共享业务库。

## 5. 公共限制（所有玩法都适用）

| # | 限制 | 出处 |
| --- | --- | --- |
| 1 | Nest 业务 worker 内禁止同步等网络、磁盘、数据库、另一个 Nest 请求，禁止自开 goroutine 访问实体；handler 内同步 Nest 请求直接被拒（`ErrSyncInHandler`），已知参与者一次传给多实体 Sender | guide/02-nest-entity.md §1 |
| 2 | `//roost:nest` 缺省是 `rollback=state durability=async`：写进 WAL 文件就回复、不等 fsync：进程崩溃（kill -9、panic、OOM）不丢已回复的修改，只有整机掉电或内核崩溃可能丢最后一个组提交间隔（缺省 10ms）里的修改，由业务幂等兜底。skill 的样例都用缺省。要“落盘之后才做下一步”的（saga 步骤与补偿、回复后马上对外部确认：通知别的服务已领、删外部队列记录、标账本完成）用 `durability=pipelined`：等 fsync 才回复，但实体锁先放。只读 handler 用 `durability=memory`，不得写持久字段。`durability=strict` 是框架能力，业务代码不用 | `codegen/docs/CODEGEN_REFERENCE.zh-CN.md` §6；guide/02-nest-entity.md §4 |
| 3 | 回复错误按表判别：`ErrCommitIndeterminate` 等结果未知不得盲目重试，按业务幂等键核对 | guide/02-nest-entity.md §6 |
| 4 | DataEngine 对 MongoDB：≥ 5.0、副本集（单节点即可）、`transactionLifetimeLimitSeconds=20`；实测过 7.0 / 8.0 | guide/03-dataengine.md §6 |
| 5 | 实体持久定时器：离线实体不准点，加载后只触发一次，补算由业务按 `Fire` 做；Remote 托管与 noPersist 实体不支持；回调只改本实体 | KNOWN-LIMITS“按实体的持久定时器”；guide/10-time.md §4 |
| 6 | 实体心跳只有 10 档（50ms～60s），回调不阻塞、不做 I/O | guide/10-time.md §4A |
| 7 | 长期幂等靠实体上的持久业务事实（序号账本、领奖事实），服务去重环只防短期重投；夹具账本满了拒绝新写、不淘汰旧回执 | 各玩法手册“幂等身份”节 |
| 8 | Player 跨服协作走 Remote / Saga / 邮件；单玩家换服用 rehome（断线后重连，不是无缝迁移；两个服的玩家库要映射到同一个 Mongo 与前缀；中止或复制失败时目标库可能留一份旧副本，下次换去时覆盖；源档与目标服已有的别的版本冲突时要运维对账）；按 sid 搬机与 CellWorld 跨格是另外两条路径 | MMORPG-INTEGRATION §6；`modules/rehome/README.md`；KNOWN-LIMITS“单个玩家换服” |
| 9 | 服务端的玩家接入层（嵌入 TCP、独立 Gate）能接 TCP 与 WebSocket（同一套 RS v2 帧，可靠有序）；KCP / UDP 只生成装配函数，监听、握手鉴权、Session 绑定宿主自写。HTTP 不是玩家协议。独立 Gate 没有跨机验证。**客户端一侧另看**：经 WebSocket 连 game 的只有 C# `Roost.Client`（含 Unity WebGL）；C++ / Unreal 只连 TCP；网页（TS）没有连 game 的 SDK，只能开房间（lobby + relay） | docs/framework/GATEWAY.md；[compose 客户端表](references/compose.md#客户端能连什么按客户端类型) |
| 10 | SyncBus 是默认可丢广播；要最终一致须业务自建对账 | guide/04-sync.md §5 |
| 11 | 视野：缺省 ApplySync 每玩家 ≤ 9 个视野（超出用 ApplyAsync）；一个会话持有的对象受 `MaxObjects`（缺省 100）约束，超出时不断开、超额降级（暂缓多出的对象，从最远处收紧视野） | guide/04-sync.md §2；KNOWN-LIMITS“广播与 AOI” |
| 12 | Saga 只在 macOS 单机测过（原生正向约 1050 saga/s），Linux、跨机未测；不是跨服瞬时原子交易。业务键只在运行期间 + 终态保留期（`saga.terminal_retention`，缺省 1h）内唯一，之后可复用；要“同一业务键永远只有一笔”在自己数据里记已用过的键 | KNOWN-LIMITS“Saga”；guide/06-saga.md |
| 13 | match 单分区“每秒成局人数 × `match.retention`”持续上限约 4500 人（本机单样本）；有弹性人数队列（`Queue.MinSize` / `FillAfterSeconds`），没有存储原生补位 | guide/09-services.md §4；KNOWN-LIMITS“匹配的弹性人数成局与补位” |
| 14 | 客户端引擎与平台只验到客户端表“实跑到哪”一列为止（Godot、Unreal、手机设备没有实跑）；跨机高可用、断电未验收 | [compose 客户端表](references/compose.md#客户端能连什么按客户端类型)；KNOWN-LIMITS“客户端：遗留”“平台与外部验证” |
| 15 | 业务时间一律取自上下文：handler 用约定参数 `nowUnixMilli`（Sender 不带时间），心跳 `Beat.Now`、定时器 `Fire.Now`；测试用 `clock.SetSource` 拨 | guide/10-time.md §1A |
| 16 | 一个 Nest 只有一个 `EntitySync` 观察者：打开 `access.player.self_sync` 后它归自身视图。房间 / 队伍推给成员用 `access.player.sync_groups`（与自身视图同一条连接，要求 self_sync，成员关系不持久）；不要自建 Manager 再交给 Nest（报 `ErrEntitySyncObserverConflict`） | ENTITY_SYNC.md“按组同步与 self_sync 共用连接”；references/party-room.md |
| 17 | 提交后动作用 `if !nest.AfterCommit(fn) { fn() }`（`SyncGroup.Join / Leave / Disband` 内部就是这样） | docs/framework/impl/02-nest-entity.md §2 |

## 6. 不适合 / 暂缓的类型

| 类型 | 结论 | 原因（出处） |
| --- | --- | --- |
| FPS / 动作射击 | 不建议 | 心跳最短 50ms（guide/10-time.md §4A），没有更高频的档位；这不是 Sync 的发送上限（on_change 在提交后汇总唤醒，guide/04-sync.md §3）。没有延迟补偿 / 命中回溯、没有通用客户端预测；接入层是可靠有序流 |
| 无缝开放世界（多进程分区） | 固定分区可做，容量未达标 | `cellworld` 模块（`modules/cellworld/README.md`，契约见 `gameplay/cellworld/README.md`）：固定网格、一个进程一个 Cell，Player 留在 Base 进程，跨格移动、跨 Cell 攻击、死亡奖励有正式路径。不支持动态拆格、一个进程多个 Cell、跨多个 Cell 的原子战斗结算；千人 / 50ms 状态同步目标在本机没有通过（`docs/performance/CELLWORLD.md`），Linux / 跨机 / 生产 Redis 高可用没有实跑。用户要做时如实说明，装模块，Player / 场景 / 怪物的接线照 game-demo 的 `game/entities/player/cell_base.go` 与 `internal/service/game/partition.go` 写，容量验收写进文档的限制 |
| MOBA 状态同步 / RTS | 暂缓 | 阵营视野挂接几十个，必须 ApplyAsync 并按自身密度压测（guide/04-sync.md §2）；无纵切、无容量数据。帧同步 MOBA 走 moba-lockstep |

用户坚持做这些类型时：如实说明缺口与出处，提议先做框架设计讨论，不要用现有零件拼一个“能跑”的假 demo。

## 7. 已知卡点

| 卡点 | 处理 |
| --- | --- |
| 派对房间场景用 `^TestPartyRoom` 过滤 | 会多跑生成的 `TestPartyRoomTimerTickHandlerMatchesTheDriver`，计数对不上；用 `^TestPartyGame` |
| 派对房间 `project sync` 报 `framework resolution failed ... go mod tidy: exit status 1`（`cannot find module providing package <module>/game/modules/partymatch`） | `game/modules/partymatch/backfill.go` 与 handler 测试要在 `project sync` 之前就位并 `fix_mod`（references/party-room.md §2 的顺序） |
| `roost add handler <name> --entity X` 报 `handler requires --component <name>` | 纯 DAO 的多实体 handler 手写：文件放 `game/handler/`，加 `//roost:nest`，再 `roost generate` |
| 锁接口类型名不含 `Entity` 又没写 `target=` / `targets=` | `generate` 报 `//roost:nest found no entity parameter`；接口名改成 `IXxxEntity` |
| 多实体事务锁序 | 争用对象（Tile、Room、Match…）改 `EntityCategoryWorld`，Player 保持 Other（取值即锁序，`framework/entity/category_order.go`） |
| 夹具 import 写死 `example.com/<genre>-practice` | §3.2 的 `fix_mod`，或直接用 `-practice` 原名 |
| `project doctor` 退出 1 | §3.4：只有 `-framework` 带上、没有模块接管的托管服务的 collaborators 拒绝桩是预期 |
| 去掉 `timers` 条目后生成部分不自动删除 | 手工删，见 KNOWN-LIMITS“按实体的持久定时器” |
