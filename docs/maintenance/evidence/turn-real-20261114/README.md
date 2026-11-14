# P9 / B9：回合与房间的真实链路

基线 `55681f53`，工作分支 `tmp/v147-turn`，2026-11-14，macOS arm64，Go 1.27.3；所有 Go 命令使用 `GOWORK=off GOFLAGS=-p=2`。私有 mirror-local offset 37500，业务端口整体偏移 35000、独立 etcd 57779；先查空闲，只停本次创建的进程。没有重置共享依赖，没有容量测量。

本批不改 room / turn 业务语义，不处理待决定的 room §5 / R11。game-demo 追加安装正式 room / turn / matchroom / matchmaking；`module-duel` 按 sid 与队伍人数分池，既有 `duel / ranked` lockstep 路径仍在。没有新增可任意开房的测试生产接口。

## 两套业务分别验收

| 入口 | 实际链路与断言 | 结果 |
| --- | --- | --- |
| `ROOST_TURN_PVP_REAL=1 ROOST_GENRE_MODULE=example.com/custom-turn-check bash scripts/test-turn-pvp-generated.sh` 的默认段 | 10 条 `TestScenario`，含真实 loopback TCP Robot、自动房间定时器；替身提交器；build / vet / id check / generate --check；默认档也编译与 vet 新 timer 夹具 | 10 PASS、0 SKIP；[完整输出](generated-real-green.log) |
| 同入口的 `TestTurnPVPRealTimerRecovery` | 两个 OS 进程，真实 Mongo / 文件 WAL / Nest / 生成 DAO：seed 提交开局、一次攻击及持久节点并 Flush；正常退出后 recover 冷加载完整 Room/Player 持久字节，驱动重新登记原期限；业务钟跨两小时，正式驱动循环只补一回合，回执可重放、旧 revision 拒绝，再完成终局并自动删除节点 | seed / recover 各 1 PASS，race、0 SKIP；[seed](timer-seed-green.log)、[recover](timer-recover-green.log) |
| `scripts/test-turn-pvp-real.sh` 的 `TestTurnRecipeRealLifecycle` | 正式 `-recipe turn-pvp`，真实 portal/account 与 match，独立 Gate → NATS → Game Ingress → Controller → Nest/DAO → WAL/Mongo；两名单排经 MatchEnqueue/Status 得同一 Room，Join 后开局；第 1 回合结束的重复请求停在第 2 回合 | seed 1 PASS；[输出](recipe-seed-green.log) |
| 同配方的 SIGKILL / recover | 第 2 回合确认后 kill 自己的 game-ingress，等过保存期限加两个回合周期，再用同一配置、WAL、Mongo 启动；新物理连接带旧 Epoch 的 TurnView 被拒，RoomReconnect 增 Epoch；只补一次超时至第 3 回合，原行动可重放，之后结束第 3 / 4 回合，重复终局行动不变并由房主关闭 | recover 1 PASS、0 SKIP；[输出](recipe-recover-green.log) |

GameRoom 是带生命/攻击规则的纵切；正式配方 Room/Turn 只提供顺序回合。两套不能互相替代。配方进程 SIGKILL 保留真实投影循环，**没有声称只存在 WAL、尚未投影的崩溃窗口**；纵切两阶段是正常关闭后的真实存储恢复。P10 的 WAL-only 地形验收不是这里的回合证据。

## game-demo 验收

`scripts/test-demo-turn.sh` 实际生成 game-demo，运行 `make dev-run` 后调用 **Makefile 的 `make dev-smoke`**，三个场景 demo / alliance_offline_join / turn 各 2/2 PASS、无失败或取消；随后停止嵌入 TCP game，换成 Game Ingress + 独立 Gate，turn 再有 2/2 PASS。旧 battle 完整场景没有被新回合替代。见 [脚本输出](demo-real-green.log)、[dev-smoke 摘要](dev-smoke-summary.log)、[Gate Robot 报告](gate-turn-green.json)。报告里的耗时仅为功能诊断，不是容量测量。

Robot 的回合操作使用正式 module-duel 单排匹配创建 Room，读取服务端 ActorID 决定行动；每条 End 都立刻重试；两人各完成两次行动后读到 Round=4、Status=finished、Sequence=2、Timeouts=0、Deadline=0。重新连接后旧 Epoch 拒绝，CAS 重连得到新 Epoch，最后一条 End 重试仍是同一终态。首次席位到达不要求同伴已经 Join，开局前以两名 Connected 席位作为屏障。

## 复跑

```sh
export GOWORK=off GOFLAGS=-p=2
export ROOST_MIRROR_LOCAL_HOME=/private/tmp/turn-private ROOST_MIRROR_LOCAL_OFFSET=37500
bash scripts/mirror-local.sh up
source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
ROOST_TURN_PVP_REAL=1 ROOST_GENRE_MODULE=example.com/custom-turn-check bash scripts/test-turn-pvp-generated.sh
bash scripts/test-demo-turn.sh
bash scripts/mirror-local.sh down
```

端口需先确认空闲，必要时调整 offset 与 `ROOST_TURN_PVP_PORT_SHIFT` / `ROOST_TURN_PVP_ETCD_PORT`（demo 对应 `ROOST_DEMO_TURN_*`）。终止后保留报告、不保留运行进程。使用不同 module 的生成命令也已实际跑过。

## 反向验证与夹具修正

定时器变异只改一次性生成工程的 `game/entities/game_room/timer_handlers.go`：回调收到恢复节点直接返回 0，不推进业务状态；恢复测试实际变红，报 `recovered room timer did not fire`（[红灯](drop-timer-red.log)）。恢复原文件后用全新私有库与 WAL 再跑 seed/recover，均通过（[seed](timer-restored-seed-green.log)、[recover](timer-restored-recover-green.log)）。复跑步骤见 [本次脚本](timer-mutation.sh.txt)；脚本里的本机路径需替换为本次保留的生成工程与证据目录。

早期夹具问题均与框架行为分开：配方没有 activity 配置，Gate 改为只取 game 公共配置；MatchStatus.Members 是入队小队（单排 1 人），不是成局名单；首个 RoomJoin 发生时同伴未必已 Join，Robot 先确认自己的席位，再由房主等两名席位 Connected 后开局。保留的初轮输出在本工作树 `artifacts/turn-pvp-real-{first,second}` 与 `artifacts/demo-turn-first`。

## 验证边界与 KNOWN-LIMITS 回写文本

仅 macOS 单机功能验收；没有 Linux、跨机 HA、断电、房间跨 game 接管、自动断线广播、客户端引擎或容量结论。模块 verify 原 room 22 / turn 25 条计数不变，新真实 probe 单独登记 `integration_coverage_test.go`。默认纵切仍为 10 条。

供主线程最后统一回写，本文没有编辑 KNOWN-LIMITS：

> **room / turn 真实联合验收**（2026-11-14）：`ROOST_TURN_PVP_REAL=1 scripts/test-turn-pvp-generated.sh` 的正式 turn-pvp 配方已在本机私有 Mongo / Redis / NATS / etcd 上经真实 account / match、独立 Gate / Game Ingress 验证双人匹配入房、四回合、命令重试，以及第 2 回合确认后 SIGKILL → 同 WAL/Mongo 重启 → 旧连接 Epoch 拒绝、CAS 重连、离线超时只补一次、序号恢复与终态关闭。game-demo 追加正式模块和 turn Robot，make dev-smoke 跑 demo / alliance_offline_join / turn 三场景，另有 Gate 的 turn；原 room 22 / turn 25 条替身场景计数不变。配方无默认伤害/胜负/奖励、自动广播或跨机房间接管，Q13 的后续设计仍待决定。

> **回合 PvP 纵切真实持久恢复**（2026-11-14）：GameRoom 的持久 timer 已在两个 OS 进程以真实 Mongo/WAL 冷加载完整 Room/Player DAO 与期限索引，正式驱动循环在离线两小时后只补一次，原回执可重放、旧页面拒绝，继续终局后清除节点。默认 10 条场景仍为替身提交器；此纵切与正式 Room/Turn 配方分别计数。合作 Roguelike / 异步社交经营未由本批提供真实档证据。

## 门禁与索引

定向根包注册 / pretag / gofmt 守卫通过；现有 demo 模板注册、模块列表、组件职责和视图命名守卫通过。shell 语法检查和文档锚点检查通过。未运行整仓慢套件或正式性能档。

沿用父线程同基线的 Verify / Tier 2 索引 `Users-whb-roost-worktrees-v147-native-acceptance`，代际 `2026-11-14T02:19:08Z`。Room/Turn 模板只返回 Module，不能从无函数结果推断无实现；相关组件、端点与 fixture 已读当前源码。room/turn 的 ID 占位行、handler 测试 207 行以及配方 network fixture 的 partial ranges 已补读。新增文件不在该旧索引，依赖精确源码、生成及实际运行验证，不宣称图谱覆盖完毕。
