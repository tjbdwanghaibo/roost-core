# 自走棋／多人回合竞技：共享卡池、阶段与恢复

这是一份八人自走棋教学消费者：三种棋子、三格战场、六格备用席、三格商店，服务器按准备／战斗阶段推进。数值与战斗只是可验证的业务规则，不代表商业自走棋的完整内容。先读[入门手册](../GETTING-STARTED.md)，再运行[正式生成消费者](../../scripts/test-auto-chess-generated.sh)。

## 学习：谁拥有状态，怎样使用 core

一局由 World 类 `ChessMatch` 独占管理，八席共享卡池、商店、经济、编队、指令账本、战斗快照、排名都在它的 DAO 内。Player 保持静态 sid，只承接永久金币。玩家用 `Sync_ChessPlayerInit` 正式建档；只调用 Entity Create 创建内存实例，不代表记录已持久化。不同玩家同时买卡、准备，最终都由同一个 Match 的 Nest 锁串行裁决；不需要给每张卡建立 Remote 实体。

| 业务场景 | 框架能力与调用 | 事务结果 |
| --- | --- | --- |
| 成局、配置固定 | `configdata.Store` → `CaptureRules` → `Sync_ChessInit` | 首次固定八个完整 PlayerID、服务端种子和 Rules；重复成局不重掷，改名单拒绝 |
| 商店与共享池 | `Sync_ChessCommand` → Match DAO `State` | 抽进商店即占池；刷新归还后再抽，买入／出售／三合一／淘汰保持真实卡张数守恒 |
| 经济与阵容 | 同一 Match 锁内校验 Gold、Level、XP、位置和余额 | 购买先入备用席；满席拒绝；一／三／九张对应一／二／三星；位置交换不得超过上阵等级 |
| 多人准备 | 每席 Ready，全部准备后冻结四场编队与服务器结果 | Ready 后该席拒绝编辑；重复 Seq 返回原回执，不重复开战 |
| 超时与离线 | 生成 Match TimerComponent，`TimerDriver.Bind/Start` | 准备超时直接用现有编队；恢复只推进一个阶段，下个期限从当前服务端时间起算 |
| 战斗与配对 | 业务纯函数 `Resolve`，持久 `Fights` | 前排优先、同时攻击、最多 32 tick；余血判胜；奇数席一人轮空；配对随回合轮换 |
| 回合经济、淘汰与终局 | 同一 handler 的规则状态机 | 基础收入加封顶利息；锁店保留占池；淘汰还卡；最多配置回合数后按血量与固定席序收口 |
| 断线与接管 | `Sync_ChessConnect` 与持久 Epoch | 旧断线事件不能踢新连接；掉线不阻塞阶段；新连接可用 `Sync_ChessReceipt` 查询旧回执 |
| 私有状态 | `Sync_ChessView` 白名单视图 | 仅本人看经济、商店、备用席；公开棋盘可侦察；不暴露种子、卡池余量或对手账本 |
| 永久奖励 | `MultiSync_ChessClaim(PlayerID, MatchID)` | Match Claimed 与 Player Gold 同一 strict 事务；排名由服务器决定，多次领奖返回原奖励额度 |
| 崩溃恢复 | DataEngine、文件 WAL、Mongo、生成 DAO 水合 | 恢复共享池、随机进度、冻结编队、回执及 timer 后继续阶段，不能重新初始化整局 |

源码阅读顺序：[规则与配置](../../scripts/testdata/auto-chess/rules.go.txt) → [状态、卡池与视图](../../scripts/testdata/auto-chess/state.go.txt) → [命令、战斗与阶段](../../scripts/testdata/auto-chess/match.go.txt) → [Nest handlers](../../scripts/testdata/auto-chess/handlers.go.txt) → [计时回调](../../scripts/testdata/auto-chess/timer_handlers.go.txt)。这些 `.go.txt` 会复制到独立工程，实际编译运行。

### 接入示例

工程由 CLI 创建 Player DAO、ChessMatch 实体／DAO／timer，再生成 Sender。只修改业务定义，不修改生成产物。将 ChessMatch 放在 World 锁档，Player 位于 Other 锁档，多实体领奖由 Nest 按分类顺序加锁。

```go
// ctx、engine、matchID 已由服务装配；playerID 必须取鉴权会话。
api := syncsender.NewAutoChessSender(engine)
v, err := api.Sync_ChessView(ctx, matchID, playerID)
if err != nil { return err }
_, err = api.Sync_ChessCommand(ctx, matchID, playerID, autochess.Command{
    Seq: v.Self.NextSeq, Epoch: v.Self.Epoch, Round: v.Round,
    Kind: "buy", A: 0,
}, serverNow.UnixMilli())
return err
```

正式业务入口还需将客户端命令身份与鉴权会话绑定。客户端不提供 `playerID`、配置、随机种子、接管票、服务器时间、胜者或奖励额度。种子应由服务端安全随机源创建；测试固定种子用于复现。初始化视图仅返回名单第一人，用作内部创建确认，不能无鉴权转发给任意调用者。

所有写 handlers 标记 `rollback=undo durability=strict`，先解码事务局部副本，再验证，最后写 DAO。任何失败都不留下卡池／随机序号／费用／回执／timer 的局部变动。strict 成功保证 WAL 持久，不保证 Mongo 已投影。回复未知时保留原 MatchID、PlayerID、Epoch、Seq 查询，不能换 Seq 再买一次。

同一 Epoch 重复命令必须逐字段相同；接管后旧 Epoch 不能写，但当前 Epoch 可以查询旧 Seq 的原回执。每席账本最多 128 项，满后拒绝新玩家操作，不淘汰旧身份；timer 仍可推进，终局仍可领奖。对局记录归档必须与永久奖励防重一起设计，不能删 Match 后用同 ID 重建。

计时器与状态同事务。回调检查持久 timer ID、参数和期限后推进一个阶段；提交后驱动才报告新期限。服务生命周期应先注册实体／handlers，加载前设置时钟，Nest 就绪后 Bind／Start；退出先 Stop 驱动，再排空 Nest／Runtime。真实恢复用例启动驱动循环；基础用例直接调用同一驱动的 `DeliverDue` 控制顺序。

## 验收：怎样复跑与理解证据

```bash
bash scripts/test-auto-chess-generated.sh
ROOST_AUTO_CHESS_REAL=1 \
ROOST_DATAENGINE_IT_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0' \
bash scripts/test-auto-chess-generated.sh
```

Mongo 地址必须指向隔离测试副本集。脚本分配本次唯一库和 WAL 目录，成功删除本次库；失败保留证据。`ROOST_AUTO_CHESS_KEEP=1` 可保留成功消费者。默认入口的提交器是替身，不是持久化验收。

| 可执行入口 | 验收范围 |
| --- | --- |
| `TestAutoChessSharedPoolConcurrentPurchaseAndRetry` | 八席并发购买、相同命令四次提交、出售刷新后的守恒 |
| `TestAutoChessReadyRaceFreezesOneBattle` | 八人重复并发准备只冻结一场，下一阶段只结算一次 |
| `TestAutoChessDeadlineRejectsLateInputAndDisconnectCannotStall` | 期限到达拒绝迟到输入，掉线玩家不阻塞回合 |
| `TestAutoChessReconnectFencesAndPrivateView` | 旧连接隔离、原回执查询、局外人拒绝、视图拷贝 |
| `TestAutoChessFormationLevelShopLockAndConfigFrozen` | 上阵数量、升级、锁店、编队冻结与配置热更 |
| `TestAutoChessUndoRestoresPoolRandomReceiptAndTimer` | 真实 Nest undo 恢复卡池、费用、种子序号、回执和 timer |
| `TestAutoChessTerminalRanksAndConcurrentRewards` | 有界终局、同回合并列按席序、双实体领奖 undo、八人并发重复领奖 |
| `TestAutoChessLedgerFullKeepsOriginalReceipt` | 账本满拒绝但旧命令可查 |
| 四项规则测试 | 连锁合星、返池、战斗重放、奇数轮空、淘汰、非法状态／篡改结果拒绝、离线不跳完整局、配置 Snapshot |
| `TestAutoChessRealCrashRecovery` | 33 条 strict 未投影记录后 SIGKILL；八人冻结战斗完整恢复；驱动补结算并持续按期限推进；八人领奖后第三进程核对冷数据、重放全部玩家命令与领奖 |

脚本要求全部基础场景和显式启用的真实场景实际执行、无跳过，并检查 race、消费者 build/vet、ID 与 `generate --check`。恢复代码见[三进程用例](../../scripts/testdata/auto-chess/persistence/recovery_test.go)。

## 问题归属与未覆盖边界

此消费者使用现有框架能力完成关键后端闭环，未据此提出新的框架缺陷。共享池、同回合淘汰排序、轮空策略、托管保持原编队、利息、锁店、固定账本容量均是业务选择。战斗结果持久化后会从冻结棋盘重算核验，但这不是正式技能系统或完整战斗回放客户端。

八个逻辑玩家在同进程通过 Sender 并发操作，不是八客户端网络或跨服压力验收。尚未接入：真实匹配／取消／候补、入场资产扣费、登录接管票签发、公网客户端、装备／羁绊／选秀／观战协议、自动奖励投递、赛季 MMR、长期归档、跨 sid 奖励、权威宿主 HA 和容量长稳。跨 sid 保持 Player 静态归属，应通过 Remote／Saga 原服发奖；本消费者的本地 MultiSync 不提供分布式原子性。SIGKILL 证明已确认 WAL 的进程恢复，不证明磁盘故障或未知回复的故障注入。
