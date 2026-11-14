# 回合 PvP、合作 Roguelike、异步社交经营

用户需要双人回合对战、合作路线闯关、离线经营加好友帮助时读本篇。玩法规则与边界唯一来源是 `docs/framework/SESSION-GENRES.md`；生成与执行命令也在该文档，不维护第二份手抄生成流程。

- 回合 PvP：`scripts/test-turn-pvp-generated.sh`，10 场景（含房间持久定时器自动超时：`roost add timer --entity GameRoom`）。
- 合作 Roguelike：`scripts/test-coop-roguelike-generated.sh`，10 场景。
- 异步社交经营：`scripts/test-social-sim-generated.sh`，9 场景。

用 `ROOST_GENRE_KEEP=1` 保留生成工程，用 `ROOST_GENRE_MODULE` 指定用户 module。必须核对回合/合作各 10 条、经营 9 条 PASS、零 SKIP，以及 build / vet / id check / generate --check。生成器只出骨架，玩法来自对应 `scripts/testdata/<genre>/`。自 v1.35.0 提供，不能声称 v1.34.2 已提供。

选择依据：即时动作 Roguelike 应组合现有战斗或物理能力并另验收；这里只验证队伍投票与奖励。跨服经营访问不能套用本进程双实体事务，需要 Remote / Saga。回合超时已接 GameRoom 的持久 timer 正式驱动；`ROOST_TURN_PVP_REAL=1` 另验跨进程冷加载、离线只补一次及重试。

不把最后一条回执说成永久幂等账本，不将 Room 的内部 JSON 当作安全的全员公开视图。接入层必须从鉴权会话选择 Player；名单与房间建立只交给宿主。默认模式用替身提交器，真实 TCP/PB Robot 已扩到回合对战终态、合作三关领奖、种植/帮助/到期收获及重试。对应 `robot_complete_test.go.txt` 由共用脚本安装；回合 PvP 真实档还独立生成 `-recipe turn-pvp`，经真实 Gate / NATS / Mongo / WAL 验证 Room/Turn 与 SIGKILL 重启（与 GameRoom 纵切分别计数）；合作与经营另有 `ROOST_COOP_ROGUELIKE_REAL=1` / `ROOST_SOCIAL_SIM_REAL=1` 的 Sender→真实 Mongo/WAL 崩溃恢复档（各两个窗口）；三类容量仍无本条新增证据。

合作工程还安装 `coop-roguelike` 的 view/robot_full 文件，以同检出的 `genre-common/run.sh` 为准。`cmd/robot` 默认走三关双人领奖整局，需两份环境变量票据；只读成员可见 PB 视图验证最终结果。详情见 [Robot](../../../framework/ROBOT.md#完整业务样板)。

回合完整命令与边界见 SESSION-GENRES“回合 PvP 真实档”；默认档也编译 / vet 真实 timer 夹具。不要把正式配方的顺序回合当作纵切的伤害/胜负规则。

合作/经营 REAL：默认10/9条保持不变，各追加1条真实测试的2个WAL-only SIGKILL子场景，随后第三进程核对领奖重试。默认也编译/vet共享Runtime和专属恢复夹具；实际命令、窗口与限制见 SESSION-GENRES“合作与经营的 WAL-only 真实档”。该档不含TCP/真实登录/Gate，不把既有替身TCP与真实存储分开通过说成联合链。
