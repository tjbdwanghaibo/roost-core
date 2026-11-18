# 回合 PvP、合作 Roguelike、异步社交经营

用户需要双人回合对战、合作路线闯关、离线经营加好友帮助时读本篇。玩法规则、边界与生成命令的唯一来源是 `docs/framework/SESSION-GENRES.md`，本篇不维护第二份手抄流程；目标 roost-core 版本更早时读那个 tag 里的本文件。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 双人回合对战至终态（生命、胜负、超时扣行动方生命）；合作三关路线（同时投票、种子、领奖）；离线经营（农场在主人 Player 上，访客须是好友，帮助是两 Player 事务） | 即时动作 Roguelike（要组合战斗或物理能力另行验收）；跨服经营访问（本进程双 Player 事务套不上，要 Remote / Saga）；friends 的双边动作要求两人本进程在线 |

## 2. 命令

三类都是“装模块 + 只写模块没覆盖的部分”，由 `scripts/testdata/genre-common/run.sh` 生成并验收：

| 玩法 | 脚本 | 模块 | 手写的部分 |
| --- | --- | --- | --- |
| 回合 PvP | `scripts/test-turn-pvp-generated.sh` | `turn wallet`（连带 room） | 生命、胜负、带内容的攻击回执、名单准入；按 turn README 扩展 turn 组件。回合期限是 turn 的 `TurnDeadline`（Room 上的持久定时器） |
| 合作 Roguelike | `scripts/test-coop-roguelike-generated.sh` | `room wallet` | 同时投票、种子、三关、领奖事实（借鉴 turn 的“每人连续命令号 + 最后一条可重放”）；另装 coop-roguelike 的 view / robot 文件 |
| 异步社交经营 | `scripts/test-social-sim-generated.sh` | `friends wallet` | 种植 / 帮助 / 收获（借鉴 friends 的两 Player 同事务） |

用 `ROOST_GENRE_KEEP=1` 保留生成工程，`ROOST_GENRE_MODULE` 指定自己的 module 名。手工照做时按 `SESSION-GENRES.md` 的命令，接在 SKILL.md §3.2 之后。

## 3. 期望场景数

| 玩法 | 场景（0 SKIP） | `module verify` |
| --- | --- | --- |
| 回合 PvP | 10 | room 34、turn 25、wallet 17 |
| 合作 Roguelike | 10 | room 34、wallet 17 |
| 异步社交经营 | 9 | friends 18、wallet 17 |

另核对 build / vet / `id check` / `generate --check`。场景名与条数不变而断言改读模块状态的地方，`SESSION-GENRES.md`“模块化之后场景语义的变化”逐条写了。

## 4. 限制

- 房间玩法的身份围栏全部交给 room：新命令经 `Room().Authorize`，合作的最后一条回执重放只核对代际与连接、不要求在线；重启后全员断线，客户端先 `RoomReconnect`。
- 不把最后一条回执说成永久幂等账本；不把 Room 上的玩法 JSON 当作安全的全员公开视图。接入层必须从鉴权会话选择 Player、从服务端 Session 取连接标识。
- 网络验收：真实 TCP / PB Robot 覆盖回合对战终态、合作三关领奖、种植 / 帮助 / 到期收获及重试；房间玩法的 Robot 每条新连接先 `RoomReconnect` 接管席位。合作 `cmd/robot` 要两份环境变量票据和两人的席位代际（`-epoch` / `-peer-epoch`），见 [Robot](../../../framework/ROBOT.md#完整业务样板)。
- 真实依赖档：回合 PvP `ROOST_TURN_PVP_REAL=1`（turn 期限跨进程冷加载、离线只补一次，再用 `-recipe turn-pvp` 经真实 Gate / NATS / Mongo / WAL 验证 SIGKILL 重启）；合作 / 经营 `ROOST_COOP_ROGUELIKE_REAL=1` / `ROOST_SOCIAL_SIM_REAL=1`（Sender → 真实 Mongo / WAL 崩溃恢复，不含 TCP / 登录 / Gate）。不把分开通过的网络档与存储档说成联合链。
- 三类都没有容量数据。
