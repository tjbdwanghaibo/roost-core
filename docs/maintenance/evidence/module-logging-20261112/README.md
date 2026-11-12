# 业务模块日志验收（2026-11-12）

基线为登录线 `feat/business-modules-portal` 的 `17d35554`，工作分支 `feat/business-modules-logging`。[任务书](../../HANDOFF-2026-11-12-module-logging.md)。只增补日志，不改协议、错误码、事务、模块场景数或生成器。

## 日志契约

- 玩家成功操作 Info；普通规则拒绝静默；异常与未知结果 Warn/Error；无玩家的低频管理操作 Debug。portal 尚未分配角色时用 account_id。
- 成功事实写在正式 Nest Sender / 服务请求确认之后；业务方法内不记录成功。attribute 溢出 Warn 是失败诊断，回滚不会使这条诊断失真。
- `confirmed` / `completed` 表示本次调用取得成功结论，不保证是新执行。重放按 request_key、order_id、mail_id、版本等关联；日志不是账本。purchase 与 mailbox 的已发货确认先于队列 / 邮件 Ack，后续 Ack 失败不抹掉已确认事实。
- 不记录令牌、票据、渠道凭据、兑换码、原始 claims；兑换请求键含原码，邮件 RequestID 也可能承载凭据，因此均不记录。
- 保留框架默认 slog handler；不另建 logger。每个模块的 README 能力段提供生成后路径。

## 逐模块范围

| 模块 | 本次日志、级别与字段 | 源码 |
| --- | --- | --- |
| wallet | 客户端消费与 GM 调整确认，记录请求键 / delta、确认余额。服务端直接用 WalletGrant Sender 的调用方负责记原因。 | [wallet_spend.go](../../../../modules/wallet/files/game/controllers/wallet/wallet_spend.go.tmpl)<br>[wallet.go](../../../../modules/wallet/with/gm/game/gm/wallet.go.tmpl) |
| bag | 丢弃与 GM 发放确认，记录道具、数量和剩余数量；事务内 Add/Remove 由发起业务记录。 | [bag_discard.go](../../../../modules/bag/files/game/controllers/bag/bag_discard.go.tmpl)<br>[bag.go](../../../../modules/bag/with/gm/game/gm/bag.go.tmpl) |
| equipment | 穿戴、卸下确认，记录槽位、道具和替换结果。 | [equipment_equip.go](../../../../modules/equipment/files/game/controllers/equipment/equipment_equip.go.tmpl)<br>[equipment_unequip.go](../../../../modules/equipment/files/game/controllers/equipment/equipment_unequip.go.tmpl) |
| checkin | 签到确认，记录连续天数、奖励和余额。 | [checkin_claim.go](../../../../modules/checkin/files/game/controllers/checkin/checkin_claim.go.tmpl) |
| task | 领奖确认，记录任务、奖励种类 / 道具 / 数量和余额；Report 不记录。 | [task_claim.go](../../../../modules/task/files/game/controllers/task/task_claim.go.tmpl) |
| stamina | 付费补满确认，记录花费、次数、体力和余额；Spend 不记录。服务端 Grant 的实际截断数量仍由调用方记录。 | [stamina_refill.go](../../../../modules/stamina/files/game/controllers/stamina/stamina_refill.go.tmpl) |
| shop | 购买确认，记录请求键、商品、份数、花费、奖励和余额。 | [shop_buy.go](../../../../modules/shop/files/game/controllers/shop/shop_buy.go.tmpl) |
| gacha | 抽卡确认，记录请求键、池、次数、花费、结果和余额。 | [gacha_draw.go](../../../../modules/gacha/files/game/controllers/gacha/gacha_draw.go.tmpl) |
| idle | 领取确认，记录序号、奖励、余额和封顶；时钟回拨拒绝记 Warn。 | [idle_claim.go](../../../../modules/idle/files/game/controllers/idle/idle_claim.go.tmpl) |
| cdkey | 受理、发件、完成确认；未知结果与基础设施异常记 Warn/Error。只记录兑换码长度，不记录兑换码及其派生请求键。 | [controller.go](../../../../modules/cdkey/files/game/controllers/cdkey/controller.go.tmpl) |
| purchase | 订单入队和每笔发货确认，记录玩家、订单、序号、奖励、结清状态；发货确认在队列 Ack 前，重试按订单号关联。身份 / 商品不符及入队失败记 Warn。 | [controller.go](../../../../modules/purchase/files/game/controllers/purchase/controller.go.tmpl)<br>[purchase_collect.go](../../../../modules/purchase/files/game/controllers/purchase/purchase_collect.go.tmpl)<br>[collaborators.go](../../../../modules/purchase/files/internal/service/platform/collaborators.go.tmpl) |
| mailbox | 附件领取与 SendTo 发件确认；profile 联动记录升级区间、邮件号和 effect_id。发件只记收件人和确认邮件号，不把重放请求里的附件当成首次发件事实，也不记录可能承载兑换凭据的 RequestID。 | [mailbox_claim.go](../../../../modules/mailbox/files/game/controllers/mailbox/mailbox_claim.go.tmpl)<br>[mailbox_send.go](../../../../modules/mailbox/files/game/mailbox/mailbox_send.go.tmpl)<br>[mailbox_profile.go](../../../../modules/mailbox/with/profile/game/mailbox/mailbox_profile.go.tmpl) |
| profile | 默认改名确认（新名）；GM 发经验只在升级时记录 levels_gained，升级奖励邮件另记 from/to。接管改名的 naming 记录旧名、新名和请求键。 | [profile_rename.go](../../../../modules/profile/files/game/controllers/profile/profile_rename.go.tmpl)<br>[profile.go](../../../../modules/profile/with/gm/game/gm/profile.go.tmpl) |
| naming | 改名确认，记录请求键、旧名键中的名称与新名；重放时旧名可能为空。结果未知与内部错误记 Warn/Error，不打印占用票据。 | [controller.go](../../../../modules/naming/files/game/controllers/naming/controller.go.tmpl)<br>[naming_rename.go](../../../../modules/naming/files/game/controllers/naming/naming_rename.go.tmpl) |
| friends | 关系操作确认，记录 action 与对方玩家号；内部错误补 player_id。 | [friends_change.go](../../../../modules/friends/files/game/controllers/friends/friends_change.go.tmpl)<br>[friends_list.go](../../../../modules/friends/files/game/controllers/friends/friends_list.go.tmpl) |
| chat | 服务端 AnnounceWorld 发布确认记 Debug，失败记 Error；不记录玩家发言正文和每次发言。 | [chat_push.go](../../../../modules/chat/files/game/controllers/chat/chat_push.go.tmpl) |
| notice | 服务端公告发布确认记 Debug，记录 kind / key，不打印正文。 | [notice.go](../../../../modules/notice/files/game/notice/notice.go.tmpl) |
| team | 建队、邀请、加入、离开 / 解散确认；返回快照时记录队长、版本和成员，错误补 player_id。离队 Sender 不返回新队长，因此不另查快照伪称本次交接结果。 | [team.go](../../../../modules/team/files/game/controllers/team/team.go.tmpl) |
| matchmaking | 持久意图受理及阶段变化确认，记录玩家、请求键、阶段、MatchID / RoomID；unknown 记 Warn，内部错误补 player_id。 | [controller.go](../../../../modules/matchmaking/files/game/controllers/matchmaking/controller.go.tmpl)<br>[runtime.go](../../../../modules/matchmaking/files/game/matchmaking/runtime.go.tmpl) |
| matchroom | 房间移交确认记 Debug，记录 MatchID / RoomID、人数与服务号；绑定冲突记 Warn。 | [matchroom.go](../../../../modules/matchroom/files/game/matchroom/matchroom.go.tmpl) |
| room | 玩家加入、离开、重连、关闭、断线确认记 Info；服务端开房记 Debug。加入返回房主；离开不返回新房主，不为日志追加查询。 | [room_close.go](../../../../modules/room/files/game/controllers/room/room_close.go.tmpl)<br>[room_join.go](../../../../modules/room/files/game/controllers/room/room_join.go.tmpl)<br>[room_leave.go](../../../../modules/room/files/game/controllers/room/room_leave.go.tmpl)<br>[room_reconnect.go](../../../../modules/room/files/game/controllers/room/room_reconnect.go.tmpl)<br>[room.go](../../../../modules/room/files/game/room/room.go.tmpl) |
| turn | 开局、对局结束确认；每回合推进 / timer 超时回调不记录。 | [turn_end.go](../../../../modules/turn/files/game/controllers/turn/turn_end.go.tmpl)<br>[turn_start.go](../../../../modules/turn/files/game/controllers/turn/turn_start.go.tmpl) |
| alliance | 建盟、申请 / 审批 / 入盟 / 退盟 / 踢人、转让、捐赠、领奖、宣战确认，以及实际成员变更；记录业务键、玩家、联盟、数量、版本或重放标记。 | [guard.go](../../../../modules/alliance/files/internal/rpc/alliance/guard.go.tmpl)<br>[membership.go](../../../../modules/alliance/files/internal/rpc/alliance/membership.go.tmpl)<br>[service.go](../../../../modules/alliance/files/internal/rpc/alliance/service.go.tmpl)<br>[war.go](../../../../modules/alliance/files/internal/rpc/alliance/war.go.tmpl) |
| activity | 个人活动异步 Complete 确认，记录活动号、代数、期次、阶段和流程版本；没有阶段变化输出的定时推进不记录。 | [resume.go](../../../../modules/activity/files/game/activity/resume.go.tmpl) |
| activity_world | 显式事件触发与 Complete 确认记 Debug，记录宿主、scope、活动号、代数、期次与阶段；空事件恢复扫描不记录。 | [service.go](../../../../modules/activity_world/files/internal/rpc/activity_world/service.go.tmpl) |
| activity_alliance | 显式事件触发与 Complete 确认记 Debug，记录宿主、scope、活动号、代数、期次与阶段；空事件恢复扫描不记录。 | [service.go](../../../../modules/activity_alliance/files/internal/rpc/activity_alliance/service.go.tmpl) |
| dungeon | 开局、领奖确认记 Info；可信战斗事件返回终局记 Debug（接口没有玩家号）；记录局、实例 / 代数、经验、赛季。端点内部错误补 Error。 | [controller.go](../../../../modules/dungeon/files/game/controllers/dungeon/controller.go.tmpl) |
| combat | 准备 / 离开确认记 Info；combat_boundary 与未知内部失败记 Error，后台推进同样记录异常；施法、伤害和每拍成功推进不记录。 | [runtime.go](../../../../modules/combat/files/game/combat/runtime.go.tmpl)<br>[combat_cast.go](../../../../modules/combat/files/game/controllers/combat/combat_cast.go.tmpl)<br>[combat_leave.go](../../../../modules/combat/files/game/controllers/combat/combat_leave.go.tmpl)<br>[combat_ready.go](../../../../modules/combat/files/game/controllers/combat/combat_ready.go.tmpl)<br>[combat_step.go](../../../../modules/combat/files/game/controllers/combat/combat_step.go.tmpl) |
| scene | 玩家切图、撤离确认记 Info；开图、关闭、排空准入记 Debug。时间倍率只提供 handler / Sender，日志由外部调用方记录。 | [controller.go](../../../../modules/scene/files/game/controllers/scene/controller.go.tmpl)<br>[runtime.go](../../../../modules/scene/files/game/scene/service/runtime.go.tmpl) |
| monster | 出生、死亡、销毁、关图清怪确认记 Debug；普通命中、AI 与重生扫描不记录。 | [runtime.go](../../../../modules/monster/files/game/monster/service/runtime.go.tmpl) |
| attribute | 溢出拒绝记 Warn 并带 player_id；它是异常诊断，允许在组件发现拒绝时记录，不是事务成功事实。 | [attribute_component.go](../../../../modules/attribute/files/game/entities/player/attribute_component.go.tmpl) |
| terrain | 建造 / 拆除确认记 Info，带序号、建筑号和 duplicate；开图、关闭、Refresh 记 Debug；端点异常记 Error，已知规则拒绝静默，笔刷成功不记录。 | [controller.go](../../../../modules/terrain/files/game/controllers/terrain/controller.go.tmpl)<br>[runtime.go](../../../../modules/terrain/files/game/terrain/service/runtime.go.tmpl) |
| enter | 新建提交、首次进入与每次进入确认，记录玩家、名字或 first；game-demo 的覆盖端点同步处理。 | [enter_create.go](../../../../modules/enter/files/game/controllers/enter/enter_create.go.tmpl)<br>[enter_game.go](../../../../modules/enter/files/game/controllers/enter/enter_game.go.tmpl) |
| login | 握手认证成功，记录玩家、承载服、握手方式与是否新建映射，不记录票据和 claims。 | [login.go](../../../../modules/login/files/game/login/login.go.tmpl)<br>[auth.go](../../../../modules/login/files/internal/access/player/tcp/auth.go.tmpl) |
| portal | 登录与签发进服票据确认，记录账号、区服；渠道拒绝记 Info。角色尚未分配，因此此处使用 account_id，不造 player_id；内部失败只记录错误码，防止渠道错误携带凭据。 | [portal.go](../../../../modules/portal/files/game/portal/portal.go.tmpl) |
| gm | 执行结果与执行前审计按 trace_id 对齐，记录命令、操作者和结果码；具体玩家变更由各模块的 GM 联动记录。 | [gm.go](../../../../modules/gm/files/game/gm/gm.go.tmpl) |
| rehome | 规则 / 冷却 / account 拒绝申请记 Info，记录玩家、目标服和码；原有受理 / 完成日志保留。 | [rehome_apply.go](../../../../modules/rehome/files/game/controllers/rehome/rehome_apply.go.tmpl) |

## 明确保留的边界

- Alliance 原生 Saga 步骤的 Sender 返回 nil 也可能是业务拒绝的回执；pipelined 模式的最终持久结论由外层消费者等待。`NativeStep` 没有向业务步骤体暴露最终结果，所以不能据 nil error 记录扣费 / 退费 / 发奖成功。本轮不新增这些成功日志，不改框架回执 API；联盟服务的捐赠 / 奖励资格确认与实际资产到账是不同事实。

- WalletGrant、BagGrant、StaminaGrant、ProfileAddExp、SceneTime 只有生成 Sender / handler，业务调用方仍须在成功返回后记录原因 / 截断数量；不修改生成器，也不在组件中虚报成功。GM、领奖、购买等已有调用方已覆盖。
- profile 的默认改名、GM 升级接口不返回旧名 / 起止等级；不为了日志读锁外 DAO 或追加竞态查询。naming 的旧名来自持久改名意图，已完成重放可能没有旧名。升级邮件 effect 有 from/to 时记录。
- team / room 离开接口不返回新队长 / 房主；本轮只记录离开确认，不声称已记录该次自动交接的新值。turn 超时、activity 的 Activate / Reopen / JoinShared 与隐式阶段切换都在事务内，尚无统一提交后变更结果，本轮不添加成功日志。个人 Complete 日志仅覆盖 ResumePending 的已确认回调；直接 Sender 由调用方记录。
- dungeon 查询可能惰性结算超时；查询不打印每次终局快照，尚无独立首次终局标记。战斗事件返回终局记录为 Debug 确认，不宣称唯一终局事件。combat 的会话绑定发生在 Cast 内（高频接口），本轮记录 Ready / Leave，不把 Ready 当成双方会话已绑定。
- monster 查询的正常越界拒绝是普通 Go error，与内部错误共用码；为遵守正常拒绝不记 Error，本轮不增加无法区分的全量 Error。scene / monster / actor 的错误分类等既有问题按任务书 §6 保留；没有用字符串匹配错误来凑日志覆盖。
- actor 移动、AOI、心跳、同步、施法 / 非致死伤害、task.Report、stamina.Spend、普通聊天、只读查询、周期扫描均不加成功日志。leaderboard.Submit 继续由调用方记录；rules / servertime / skillbook 不加。

## 验证

环境为 macOS arm64 / Go 1.27.3；关键命令输出见[验证摘要](validation.txt)。

| 检查 | 结果 |
| --- | --- |
| 第一批独立生成工程 | build / vet / module verify / go test 全通过；工程测试 475 条通过，生产 Compose 检查 1 条按环境开关跳过 |
| 全模块独立生成工程 | build / vet / module verify 通过：87 个模块 / 联动分组、960 条场景、零 SKIP；[分组数量](module-counts.json)。工程测试 1084 条通过、2 条显式环境检查跳过 |
| 日志提交边界（临时工程，race） | [通过](rollback.txt)：真实 Nest 提交失败后无成功日志且余额回滚；余额不足不记日志；成功日志包含 player_id、请求键和确认余额 |
| 最终定向复核 | 邮件只记录信封 ID、naming 对未知结果记 Warn 的调整后，`go test -race -count=1 ./game/mailbox ./game/controllers/naming` 分别通过 |
| game-demo 全部测试 | `GOWORK=off go test -count=1 -timeout 20m ./...` 通过；依赖门控场景未启用，不能据此声称全部真实集成通过 |
| 真实进程 | [探针结果](real-process.txt)、[11 条关键日志](runtime-logs.json)：curl 三步登录、Robot TCP 先新建 / 首次进入、购买 100 金币、消费 7 后余额 93、重连；逐份日志扫描未出现本轮凭据和票据 |
| `scripts/test-modules-generated.sh` | 通过：正向 / 反向各 42 个模块、46 个联动，模块验证各 960 条零 SKIP；重复安装、ID / 依赖顺序、全包测试与生成物漂移检查通过 |
| `TestDemo` / `TestModule` / `TestRecipe` | `GOWORK=off go test -count=1 -timeout 45m -run 'TestDemo\|TestModule\|TestRecipe' ./codegen/internal/roost` 通过 |
| 根包与文档 | `GOWORK=off go test -count=1 .` 通过；文档 342 文件、零失效链接；`git diff --check` 与复跑脚本语法检查通过 |

全量生成检查使用启动时构建的 CLI；最后两处日志调整（SendTo 只记录最终信封 ID、naming 未知结果告警）随后覆盖到独立生成工程并分别完成 race 复核，未重复整套生成矩阵。

两条显式跳过分别为生产 Compose（需 `ROOST_COMPOSE_CHECK=1`）与联盟多实例停建迁移（需 `ROOST_DATAENGINE_IT_NATS_URL`）。它们不在 module verify 的 960 条必跑场景内，本轮没有宣称完成部署 / 多实例故障验收。

最初生成 demo 使用 `--skip-deps`，首次测试 / 启动发现缺少 go.sum；补完本地 replace 下的 `go mod tidy` 后通过。首次根包检查与生成脚本并行，边界扫描读到了尚未写入 go.mod 的 `.roost-generate-*` 临时目录，报告 core 引用了 wiring；保留失败原因；生成结束后根包复跑通过，未修改门禁。

### 真实进程复跑

生成 `-template game-demo`，使用自己的 module / 项目名，`--skip-deps` 后设置本地 core replace 并 `go mod tidy`。按 `demo/WALKTHROUGH.md` 准备依赖，运行 `make dev-run`。本轮采用本机 etcd 2379 / Redis 6379 的项目专属前缀，以及独立 NATS JetStream 24222 / Mongo 副本集 `logging-rs` 27028；未改共享服务。完成后 `make dev-stop`，独立 Mongo / NATS 也已关闭。

将本目录 `runtime_probe.go.tmpl` 的 `{{MODULE}}` 换成生成工程 module，放到 `.acceptance/main.go`；在工程目录运行 `bash <本目录>/portal-smoke.sh -check-name`。脚本只使用 demo 的开发凭据，票据保存在临时工程，不提交。生产渠道回调未验证，购买使用 demo 的模拟签名提供方；其后的 platform、Redis 队列、Nest/WAL、Mongo 和 TCP 是真实链路。

`wallet_log_test.go.tmpl` 可替换 module 后临时放入全模块工程 `game/handler/`，运行 `go test -race -run '^TestLoggingWalletCommitBoundary$' ./game/handler`。它依赖现有 wallet 测试夹具；不装入模块模板，也不改 module.yaml 的场景数。

## CBM 与证据边界

使用本 worktree 的 CBM 图谱进行 Tier 2 定位并检查 coverage；模块 Go 模板多为 File 节点，无法从图谱可靠追踪模板内部调用，因此逐文件读取源码，包含依赖的模板占位符解析缺口。已执行 fast 刷新，coverage 的 recorded_at 为 `2026-11-12T12:30:10Z`；返回的 generation 仍为 `2026-11-12T11:04:07Z`，但 66 个变更代码路径均为 `metadata_match`。其中 attribute 注册占位符、notice 的列表占位符是两处部分解析文件，已读对应源码补证。docs / artifacts 按索引配置排除，文档、脚本、临时探针使用直接源码检查；不把图谱无缺口当成完整性证明。
