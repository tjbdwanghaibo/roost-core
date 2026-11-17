# 回合 PvP、合作 Roguelike、异步社交经营

用户需要双人回合对战、合作路线闯关、离线经营加好友帮助时读本篇。玩法规则与边界唯一来源是 `docs/framework/SESSION-GENRES.md`；生成与执行命令也在该文档，不维护第二份手抄生成流程。

R9（2026-11-17）起三类都是“装模块 + 只写模块没覆盖的部分”，模块清单写在各脚本的 `genre_modules` 数组里，由 `scripts/testdata/genre-common/run.sh` 经 `genre-common/modules.sh` 装上并在门禁之后跑 `roost module verify`：

- 回合 PvP：`scripts/test-turn-pvp-generated.sh`，`turn wallet`（连带 room），10 场景。手写生命、胜负、带内容的攻击回执、名单准入，并按 turn README 扩展 turn 组件（超时扣行动方生命、分出胜负即结束）。回合期限是 turn 的 `TurnDeadline`（Room 上的持久定时器），不再 `add timer --entity GameRoom`。
- 合作 Roguelike：`scripts/test-coop-roguelike-generated.sh`，`room wallet`，10 场景。同时投票、种子、三关、领奖事实手写（借鉴 turn 的“每人连续命令号 + 最后一条可重放”）。
- 异步社交经营：`scripts/test-social-sim-generated.sh`，`friends wallet`，9 场景。不再有房间：农场是主人 Player 上的字段，访客须是好友，帮助是两 Player 事务（借鉴 friends）。

用 `ROOST_GENRE_KEEP=1` 保留生成工程，用 `ROOST_GENRE_MODULE` 指定用户 module。必须核对回合 / 合作各 10 条、经营 9 条 PASS、零 SKIP，build / vet / id check / generate --check，以及末尾 `module verify: <模块> n/n skipped=0`（回合 PvP：room 34、turn 25、wallet 17；合作：room 34、wallet 17；经营：friends 18、wallet 17）。生成器只出骨架，玩法来自对应 `scripts/testdata/<genre>/` 与 `genre-common/`。场景名与条数不变、断言改读模块状态的地方，SESSION-GENRES“模块化之后场景语义的变化”逐条写了旧断言 → 新断言。

选择依据：即时动作 Roguelike 应组合现有战斗或物理能力并另验收；这里只验证队伍投票与奖励。跨服经营访问不能套用本进程双 Player 事务，需要 Remote / Saga；friends 的双边动作要求两人本进程在线。房间玩法的身份围栏全部交给 room：新命令经 `Room().Authorize`，合作的最后一条回执重放只核对代际与连接、不要求在线；重启后全员断线，客户端先 `RoomReconnect`。

不把最后一条回执说成永久幂等账本，不将 Room 上的玩法 JSON 当作安全的全员公开视图。接入层必须从鉴权会话选择 Player、从服务端 Session 取连接标识；名单、开房与好友关系只交给宿主。默认模式用替身提交器，真实 TCP/PB Robot 覆盖回合对战终态、合作三关领奖、种植 / 帮助 / 到期收获及重试；房间玩法的 Robot 每条新连接先接管席位（`reconnect` → `RoomReconnect`）。回合 PvP 真实档 `ROOST_TURN_PVP_REAL=1` 先跑纵切的 `TestTurnPVPRealTimerRecovery`（Room 上 turn 的期限跨进程冷加载、离线只补一次、换连接后重放），再生成 `-recipe turn-pvp` 经真实 Gate / NATS / Mongo / WAL 验证 SIGKILL 重启——两者从 R9 起是同一套 Room / Turn 模型，证据分别计数；合作与经营另有 `ROOST_COOP_ROGUELIKE_REAL=1` / `ROOST_SOCIAL_SIM_REAL=1` 的 Sender→真实 Mongo/WAL 崩溃恢复档（各两个窗口）；三类容量仍无本条新增证据。

合作工程还安装 `coop-roguelike` 的 view / robot_full 文件，以同检出的 `genre-common/run.sh` 为准。`cmd/robot` 默认走三关双人领奖整局，需两份环境变量票据和两人的席位代际（`-epoch` / `-peer-epoch`）；只读成员可见 PB 视图验证最终结果。详情见 [Robot](../../../framework/ROBOT.md#完整业务样板)。

合作 / 经营 REAL：默认 10 / 9 条保持不变，各追加 1 条真实测试的 2 个 WAL-only SIGKILL 子场景，随后第三进程核对领奖重试。默认也编译 / vet 共享 Runtime 和专属恢复夹具；实际命令、窗口与限制见 SESSION-GENRES“合作与经营的 WAL-only 真实档”。该档不含 TCP / 真实登录 / Gate，不把既有替身 TCP 与真实存储分开通过说成联合链。
