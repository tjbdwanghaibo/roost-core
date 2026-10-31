# 回合 PvP、合作 Roguelike、异步社交经营

用户需要双人回合对战、合作路线闯关、离线经营加好友帮助时读本篇。玩法规则与边界唯一来源是 `docs/framework/SESSION-GENRES.md`；生成与执行命令也在该文档，不维护第二份手抄生成流程。

- 回合 PvP：`scripts/test-turn-pvp-generated.sh`，9 场景。
- 合作 Roguelike：`scripts/test-coop-roguelike-generated.sh`，10 场景。
- 异步社交经营：`scripts/test-social-sim-generated.sh`，9 场景。

用 `ROOST_GENRE_KEEP=1` 保留生成工程，用 `ROOST_GENRE_MODULE` 指定用户 module。必须核对回合/经营各 9 条、合作 10 条 PASS、零 SKIP，以及 build / vet / id check / generate --check。生成器只出骨架，玩法来自对应 `scripts/testdata/<genre>/`。自 v1.35.0 提供，不能声称 v1.34.2 已提供。

选择依据：即时动作 Roguelike 应组合现有战斗或物理能力并另验收；这里只验证队伍投票与奖励。跨服经营访问不能套用本进程双实体事务，需要 Remote / Saga。回合超时入口已有期限校验但未接自动驱动，需参考 card-rpg 持久 timer 正式接线，不能用不持久的 goroutine sleep 冒充离线恢复。

不把最后一条回执说成永久幂等账本，不将 Room 的内部 JSON 当作安全的全员公开视图。接入层必须从鉴权会话选择 Player；名单与房间建立只交给宿主。默认模式用替身提交器，真实 TCP/PB Robot 已扩到回合对战终态、合作三关领奖、种植/帮助/到期收获及重试。对应 `robot_complete_test.go.txt` 由共用脚本安装；无这三类的独立真实存储恢复、Gate 组网或容量证据。

合作工程还安装 `coop-roguelike` 的 view/robot_full 文件，以同检出的 `genre-common/run.sh` 为准。`cmd/robot` 默认走三关双人领奖整局，需两份环境变量票据；只读成员可见 PB 视图验证最终结果。详情见 [Robot](../../../framework/ROBOT.md#完整业务样板)。
