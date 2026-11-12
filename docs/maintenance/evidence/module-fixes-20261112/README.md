# 业务模块修复验收（2026-11-12）

承接 [module-fixes 任务书](../../HANDOFF-2026-11-12-module-fixes.md)，工作分支 `feat/business-modules-fixes`，基线 `5b99e8ae`，验收跨日完成于 2026-11-13；它包含尚未验收的草稿 `99f91afd`。只提交到本分支，不合并、不发版。本轮修复模块模板、清单和验收，未修改 gateway / Nest / DataEngine / Sync 或生成器行为。`compose.md` 的结构与规则未改。

## 1. review 分支复核

检查独立分支 `fix/core-game-server-review` 的 BUSINESS-MODULES-REVIEW-20261112 与 BUSINESS-MODULES-FOLLOWUP-20261111 文档；对应远端版本记在 checks.txt。报告分析的是旧业务模块基线，不能把其结论全部当作当前未修问题。

| 编号 | 当前结论与处理 |
| --- | --- |
| RV-20261112-BM-01 | **确认并修复**。Terrain 的 failure 计数清零后复用旧代际，迟到 Refresh 可以清掉后来的故障。改为单调故障序号 + 独立恢复水位，旧完成不能覆盖新失败。`TestTerrainRecoveryOldCompletionKeepsNewFailure`、`TestTerrainRecoveryKeepsFailuresAddedDuringRefresh`；修前红、修后 race 通过，恢复旧实现的行为变异被拦住。 |
| RV-20261111-BM-04 | 历史问题成立，当前已含 `2bfde374` / `8c988308` 的修复：按当前角色绑定与代数判定，不从 Stale 推断旧请求终态。本轮未重复改运行逻辑，重验 rehome 场景。 |
| RV-20261111-BM-05 | 历史单次关系检查不足；当前已含 `2960af8e` 与 R10 `fc0fa6e9`。联盟允许成员换服，以入盟批次 + 旧 SID 修正 HomeSID，进入时对账。更新 KNOWN-LIMITS 中仍写“公会成员禁止换服、HomeSID 不修正”的过期段落。HomeSID 仍是尽力通知地址，不能当 account 的线性一致复制。 |
| 较早 account / wallet 问题 | 当前基线已有对应修复，不移植 review 分支旧运行时代码。 |

## 2. 任务逐项结论

路径以仓库根为准；模块文件均为 `modules/<name>/files/` 模板，组合文件在 `with/`。场景名可从表中的前缀和 [变异结果](mutation-results.json) 定位。

| 任务 | 结论、主要文件与验证 |
| --- | --- |
| 2.1.1 Scene 正常拒绝 | 已修。`scene/controller.go`、`scene_interact.go`、实体 / handler：正常拒绝填 PB Code/Reason、返回 nil error；内部错误仍返回 error。`TestScene*Refusal*`。真实启动额外发现接线问题，见 §7，未宣称 TCP 不断连接已实测。 |
| 2.1.2 Terrain 错误码 | 已修。`terrain/rules.go`、controller：invalid / denied / sequence / occupied / site / full 六类业务码；畸形预检不用硬编码 1。`ErrBase` / `ErrState` 代表底图版本或持久事实异常，保留内部错误。`TestTerrainEndpointsAnswerEachRefusalWithItsOwnCode`。 |
| 2.1.3 Monster 错误码与清单 | 已修。Inspect 的 invalid / gone / not_here 独立业务码，verify 包改为真正的 `game/monster/service`；`TestMonsterInspectRefusals*`。 |
| 2.1.4 Actor 正常拒绝 | 已修。`MoveRefusal` 区分未入空间、被定身、路径太长；已知拒绝不记 Error，未知原始错误保留。`TestActorMoveRefusals*`。 |
| 2.2.1 Activity 进入实体号 | 已复现并修。`with/enter/.../activity_enter.go` 把 raw PlayerID 转 EntityID；真实进入回调驱动活动与待完成步骤，`TestWithActivityEnterHookReconcilesAndResumesPendingForTheEnteredPlayer`，传回原始号的变异失败。 |
| 2.2.2 Alliance 遗漏验收包 | 已修。纳入 `internal/service/alliance`。StopFounding 在默认离线队列组替身和显式真实 NATS 两种环境运行，默认不 skip。 |
| 2.2.3 Alliance 旧 demo 注释 | 已修。协议、规则、扣费 handler 与设置说明按模块自己的 CoinItemID / Saga 链路表述，不再引用 gift demo。 |
| 2.2.4 Alliance 客户端缺口 | 已补申请、批准、拒绝、踢人、转让、名单；末位盟主 Leave 即解散。权限仍由联盟服务判定，申请名单只向盟主展示。服务侧无结束战争，按任务书保留“不开客户端宣战”。端点场景 + 服务场景 + §5 真实网络链路。 |
| 2.2.5 联盟活动 created | 已修。`ActivityCreatedAt()` 从创建秒换算毫秒；未建成或已解散联盟不激活。`TestAllianceActivityCreatedTriggerStartsFromTheFoundingTime`。 |
| 2.3.1 Friends 离线操作 | 部分能力完成，安全边界保留。Reject / Unblock 只锁本人；Request / Accept / Remove / Block 仍需双方本进程认证会话。删除 / 拉黑保持双边一致；没有持久归属证明时不能冷加载未知离线档案写入。`TestFriendsEndpointOwnSide*`、`TestFriendsEndpointRemoveAndBlockRequirePeerOwnership`。详见 §4。 |
| 2.3.2 同装备换等级 | 已修。同槽同道具换级不调 Bag Remove/Add，不需要背包第二件；不同道具换装维持原事务。`TestEquipmentRelevel*`。 |
| 2.3.3 Team 邀请可见 | 已补 `TeamInvites` 查询、`TeamInvited` 推送、Player 有界收件记录。查询重新读 Team 权威，过滤解散 / 满员 / 冻结 / 过期等无效记录；迟到旧通知不能缩短续期。正式 Nest 场景 + controller 的查询 / 推送失败场景。仅本进程在线收件人可自动收到，详见 §4。 |
| 2.3.4 Combat 死亡后路径 | 已修。显式离开或会话结束在原事务中恢复满血、清盾、alive；死亡本身仍可观察，之后需重新 Ready。`TestCombatLeaveRestores*`、`TestCombatRejectedLeave*`。补 `TestWithCombatSkillbook*`，目录、摘要解析、正式 Nest 定身与到期。 |
| 2.3.5 Dungeon 敌人定义 | 已修。允许敌人从规则 Program 推导；场景用 Go overlay 仅给规则加 elite，再走正式处理链，`TestDungeonNewEnemyOnlyNeeds*`。 |
| 2.3.6 只读 handler | 已确认并统一本轮误声明的查询：AttributeGet、DungeonCounts、MatchState、MatchRoomClosed、RoomView、TurnView、AllianceInfo/Receipt、GuardInfo/Receipt；新 TeamInvitation 同样用 `durability=memory`。裸 tag 默认 state/async，不能当 memory。带惰性推进的 ActivityView、DungeonView、引用清理的 TeamInfo 仍用写事务。生成注册元数据和原有行为场景共同验收。 |
| 2.3.7 单装访问声明 | 已补 equipment / gacha 的 TCP + self_sync，保留草稿 idle 声明；dungeon 经 profile → enter 获得。`TestModuleClientAccessWithoutEnter` 分别新建三工程，不靠 enter，检查 manifest、生成 self_sync、build、ID 与漂移。监听 / 鉴权仍是运行配置责任。 |
| 2.4.1 Notice GM 审计 | 已迁到 `notice/with/gm`，独立 notice 只提供可信服务端入口；四条联动场景验证必填、执行前后审计、成功 CodeOK。同时修复草稿暴露的 nil error 审计记为 INTERNAL。 |
| 2.4.2 Wallet 文案 | 已扩为涵盖非法键与非正金额，错误码不变；既有 `TestWalletApplyRefusesKeysThatCannotBeStored` 加断言并做变异。 |
| 2.4.3 Task 名字与注释 | 已改 `TaskDailyDungeon` 与每日 / 每周 / 永久、道具奖励拒绝的表述；依赖引用和生成工程验收通过。 |
| 2.4.4 Servertime 同包名 | 核实是模块通用写法，game/domain 与 controllers 同名、调用处按需别名，不为此改包名。 |
| 2.4.5 Leaderboard 真实规则 | 已用真实 rank Service + MemoryStore 验证只升不降与同分先到；Redis 行为用内存夹具供 Service 依赖，失败与参数转交场景保留替身。`TestLeaderboardRepeatedAndStaleSubmitsNeverMoveTheBoard`、`TestLeaderboardEqualTotalsRankWhoeverGotThereFirst`，UpdateMax → UpdateSet 变异被拦住。 |
| 2.4.6 Naming / Matchmaking 日志 | 当前基线的内部错误均带 player_id 记 Error，前一轮日志提交已完成，本轮保留并验收，不重复增加日志。 |

## 3. 场景、协议和错误码

场景数量来自实际 PASS 数，不把缺包、零测试或 skip 当通过。所有模块清单、安装测试、`test-modules-generated.sh` 与 README 同步。

| 模块 / 联动 | 原任务前 → 当前 |
| --- | --- |
| actor / scene / monster / terrain | 12→14 / 26→28 / 25→26 / 23→28 |
| attribute / equipment / dungeon | 16→17 / 16→18 / 19→21 |
| friends / team / combat | 12→18 / 27→35 / 36→38 |
| alliance / activity_alliance / activity+enter | 49→55 / 4→5 / 1→2 |
| leaderboard / matchmaking | 20→21 / 16→17 |
| combat+skillbook / notice+gm | 新增 1 / 新增 4 |

原数量以 `ec9cb5a3` 的清单为准（Team 原 27）；不把未纳入清单的测试当作原已验收数量。完整当前清单见 [scenario-counts.json](scenario-counts.json)。42 个模块、47 条有验收的联动，共 1006 条模块及联动场景。

新增 PB 请求：`ApplyAlliance`、`ApproveAllianceApply`、`RejectAllianceApply`、`KickAllianceMember`、`TransferAllianceLeader`、`AllianceRoster`、`TeamInvites`；新增推送 `TeamInvited`。由安装器分配 ID，不写死到消费工程。

新增场景 / Actor / Monster / Terrain 业务错误码按 module.yaml 分配。现有联盟管理沿用联盟业务码，不靠客户端身份字段判断操作者。具体请求字段与所有拒绝码以各模块 README 为准。

## 4. 本轮定下的业务边界

- Friends 不把“曾在本服”当作当前写归属；离线双边写需要独立关系权威或可验证的路由 / 归属机制。本轮只放开单边动作，不绕过框架。
- Team 收件记录只是索引，Team 中邀请才是权威；查询不是从全体队伍反向扫描。邀请提交之后通知失败不撤销邀请；离线 / 跨 game 无自动投递，客户端也不能从查询发现从未送达的记录。要扩大覆盖须补正式投递机制。
- Combat 在会话结束时恢复，不在每次检测死亡时自动复活；提交拒绝时恢复动作随事务回滚。
- 宣战服务缺结束能力，不给客户端只进不出的入口。联盟解散不返还资金，旧联盟号不复用。
- 只读声明显式 memory，持久 setter 在此类入口中会被框架拒绝。没有把会推进状态的“查询”误改成 memory。

## 5. 真实进程验收

环境是 macOS 单机。新工程 `live-alliance` 安装 alliance / login / portal / enter / gm；MongoDB 副本集端口 27030、NATS JetStream 24224 为本轮单独启动，Redis 6379、etcd 2379 沿用本机。配置使用独立工程名前缀，不修改既有业务服务。

流程：11 个生成服务启动 → 两个新账号经 `/portal/login`、`/portal/enter`，TCP auth、EnterStatus、EnterCreate、EnterGame → 官方 GM 给创建者 100 个 1003 → 建盟 Saga 完成 → 申请 / 盟主看到待审批名单 → 拒绝 / 再申请 → 批准 / 成员名单 → 转让 → 新盟主踢掉原盟主 → 末位盟主退出解散。原样输出见 [live-alliance.txt](live-alliance.txt)。多个新增客户端行为在同一真实链路上已验收，不仅调用了 RPC 替身。

复跑探针是 [alliance-probe.go.tmpl](alliance-probe.go.tmpl)：放到同样生成工程的 `.acceptance/main.go`，替换 `{{MODULE}}`，配置本机开发认证令牌与 admin token，`GOWORK=off go run ./.acceptance`。探针使用新的账号和联盟，会创建验收数据；不用于已有生产数据库。服务就绪后运行；最终 `make dev-stop`。开发令牌是此隔离验收的示例值，不含真实票据，探针输出不打印票据。

## 6. 验证证据

修前 / 变异失败摘要见 [red-green.txt](red-green.txt)，27 个可编译变异均被对应断言拦住；每项测试名见 [mutation-results.json](mutation-results.json)。变异只写 scratch 生成工程，finally 恢复；没有改生成器让测试通过。通过数据与各检查原样末行记录在 [checks.txt](checks.txt)。

本轮不做性能结论；没有 Linux、跨机、独立 Gate、长时间稳定性、强杀故障矩阵验收。四组生成工程各跳过 1 条未启用的生产 Docker compose 测试；game-demo 有 11 条需要显式环境的真实 Mongo / Redis、Docker 或性能场景跳过，具体名称记在 checks.txt。模块 verify 本身均为零 skip。

## 7. 新发现且按交接未扩大范围修复的问题

**Scene 启动依赖时序**：全新 game 工程 `module add scene alliance login portal enter gm`，配置同样真实依赖后 `make dev-run`，其余服务可启动，game 失败：

```text
mod access.player provide: scene: runtime is unavailable
```

`modules/scene/files/game/controllers/scene/controller.go.tmpl` 的 FromRegistry 在构造 controller 时要求 Manager；`game/scene/service/runtime.go.tmpl` 的 Start 到启动阶段才登记。生成的 access.player 在 Provide 阶段构造 controller，先于 Start。建议后续统一能力构造 / 发布与启动激活的时序，或者在正式依赖准备完成后构造 controller；不能靠测试夹具提前塞能力掩盖。没有单独声称“最小 scene-only 工程”已复现，实际失败组合如上。

任务书要求新发现的问题先登记、不扩大本轮修复范围；因此这里只登记到 scene README、KNOWN-LIMITS 和能力表。Scene 的 PB 拒绝离线验收通过，但真实连接上的“不被断开”尚未跑通。后续合并 / 发版应明确评估此启动限制。

## 8. 图谱与源码核对

使用 Codebase Memory Tier 2 验证，项目 `Users-whb-roost-worktrees-module-fixes`。执行 index_repository 刷新后，对本轮修改涉及的 134 个代码 / 清单路径分批做 coverage：无陈旧路径，35 个模板存在局部解析缺口，已直接读对应范围；主要是 `{{ID:…}}` 和 Go 复合字面量被模板解析器误判。生成工程的编译与行为验收补足模板层的证据，不能把“没有 recorded gap”当作全仓无缺陷证明。docs / scripts / artifacts 按索引配置排除，直接读文件。

收尾格式化曾因临时替代标识前缀相互包含，损坏五份测试模板；独立 Turn 生成检查捕获，已从草稿恢复原内容、保留本轮断言，改为仅识别已知占位符并一次性还原。检查前后占位符多重集一致，再重跑受影响的生成场景；不靠放松门禁通过。

Scene 的旧安装变异检查仍匹配原 Go error 文本，首次完整套件因此报 1 项失败；新接口把正常拒绝写在 PB 中，已强化应答 Code / Text 的成功断言并更新变异诊断，定向复测通过。该失败保留在 checks.txt，不隐去首轮结果。

全仓首轮的 root 与 codegen 包未通过：报告当时未纳入 tracked 文件，且该轮已编译的测试程序嵌入了中间损坏模板。其他 163 个包返回 OK。根包、九项受影响的生成检查与 Scene 诊断已重验，余下四项 Combat / 配方 / Timer 也已重验通过，最终结果记录在 checks.txt；未将首轮全仓命令写成一次全绿。
