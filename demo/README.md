# `-template game-demo` 的源码

`roost project new <name> -module <mod> -template game-demo` 在 `game` 模板之上再生成**一条可运行的写入链路**。
这个目录就是它写进项目的那些文件，**按生成后的真实路径原样存放**（多一个 `.tmpl` 后缀）。

## 为什么是文件而不是 Go 字符串

codegen 的其他模板是 `fmt.Sprintf` 出来的字符串字面量。demo 的体量大、要被人照着改，所以存成真文件：
能直接读、能 diff、能整段 review，不用在字符串里数 `%q`。

`.tmpl` 后缀让它们不参与 codegen 自身的编译——内容 import roost-core，而 **codegen 对运行时零依赖**
（见 `go.mod`，只有 `gopkg.in/yaml.v3`）。

正确性不靠在这里编译，而是靠 **CI 生成一个项目再编译它**（`framework-compat.yml` 的 `demo` scenario）。
roost-core 自己的 `examples/` 模块就是反例：不在任何 CI 里，`go.sum` 过期之后静静地编译不过了。

## 占位符

| 占位符 | 替换成 |
| --- | --- |
| `{{MODULE}}` | 项目的 Go module 路径 |

## 链路

本节是这条写入链的逐层说明；六条路径（同步写、事件链、跨服务、saga、帧同步、配置）横向的对比——
每层守什么、失败在哪一层处理、重放靠什么幂等——见 codegen 仓的 `docs/DATA_FLOW.zh-CN.md`。

```
TCP (player access)
  → game/controllers/player/add_item.go      端点（本目录）：错误边界，coded error → 响应里的 Code/Reason
  → game/handler/syncsender/…Sync_AddItem     Sender（从 handler 参数与返回值生成）
  → Nest 锁住 Player
  → game/handler/add_item.go                  handlerAddItem（本目录）
  → BagComponent.AddItem                      本目录：查 item 表 → 校验 → 只走生成的 mutator
      ↳ configs/generated.ItemByID             生成的表访问器，读的是钉在本次请求上的配置快照
      ↳ internal/errors.Err*                   errcode.Define，code 在清单的 errcode 号段里
  → PlayerDao.SetItems                        生成的 map mutator，带 dirty / undo / patch
  → dataengine 落库
```

几个刻意保留的教学点：

- **业务代码只碰生成的 mutator**（`dao.SetItems(...)`），不直接写私有字段——dirty 追踪、Nest undo 和持久化
  patch 全靠它。
- **handler 参数与协议字段必须对上**：`add endpoint` 会解析两边并拒绝对不上的组合，所以
  `game/handler/add_item.go` 的 `(itemID int64, count int32)` 与 `protocol/def/add_item.go` 的
  `ItemID` / `Count` 是同一份契约。handler 的第一个返回值（新数量）经 Sender 原样带回端点。
- **`rollback=undo durability=strict`**：handler 返回 error 时本次调用过的 mutator 全部回滚。
- **错误在端点换形状**：生成的 TCP server 把"端点返回 error"当作坏帧处理——断连。所以业务失败不能作为
  error 返回，而是 `errcode.ClientError(err)` 换成 `(code, reason)` 写进响应；没有 code 的错误（基础设施故障、
  bug）统一坍缩成 `CodeInternal` + 固定文案，内部信息不出网，原因在端点处打日志。
- **配置表走生成器**：`configs/schema/item.go` 的 `//roost:table` 是唯一手写的地方；`roost generate` 生成类型化
  loader、把 `configs/table/item.csv` 转成 `configs/data/item.json`（CSV 前四行是列名 / 标题 / 类型 / 规则），
  并注册进 `roost-core/configdata`。`required` / `unique` 在转换时校验，坏数据死在 `make generate`，
  不会死在玩家请求里。
- **错误码归号段**：`roost add errcode X -id N` 的 N 必须落在清单 `ids.errcode`（100000–199999）里，
  `roost id check` 负责查重；`docs/generated/errcode.csv` 是给客户端的对照表。

## 事件链：升级 → 奖励邮件（事务性 outbox）

```
AddExp 端点 → Nest 锁 Player → ProfileComponent.AddExp
  ├─ dao.SetExp / SetLevel                 状态变更
  └─ effects.EmitPlayerLevelUp             nest.Emit：effect 与状态变更进同一条 WAL 记录
        ↓ commit 后由 dataengine 发到 JetStream（<subject_prefix>.player.level_up）
internal/service/game/level_up_mail.go     durable consumer + Mongo inbox（每个 EffectID 只处理一次）
  └─ mail.Send(RequestID = EffectID)       mail 服务按 RequestID 去重：第二层幂等
```

- **升级"这件事"在组件里发出**，紧挨着让它成立的状态变更；谁对它做出反应住在别处。handler 回滚时 effect 一起消失，
  所以永远不会给一个没落库的升级发奖励。
- **两层幂等**：inbox 收据与业务写入在一个 Mongo 事务里提交，进程重启后重投的 effect 会被认出来；邮件是总线调用
  不是 Mongo 写入，所以还要靠 `RequestID = EffectID` 让 mail 服务自己去重。
- **生产者与消费者读同一组配置键**（`dataengine.database` / `dataengine.effects.*`）、用同一组默认值，两边不会对"effect 在哪"
  产生分歧。
- 消费者在 `Service.Init` 里订阅、`Shutdown` 里 `Drain`：进程宕机期间提交的升级，回来时会补投。

## 邮件：列表与领附件（客户端这一半）

升级奖励邮件现在带附件——`game/rewards/` 是附件的编码（一个道具一叠，JSON），发件方（`level_up_mail.go`）与领取方共用它：

```
ListMail 端点  ─ List(playerID, cursor, limit) ─▶ mail 服务（按认证玩家作用域，服务端不需要端点再查归属）
ClaimMail 端点 ─ ReserveClaim(mailID, "")      ─▶ mail 服务：交出附件 + 对 (玩家, 邮件) 恒定的 token
               ─ Sync_ClaimMailReward(...)    ─▶ Nest 锁 Player 的一笔事务：道具 + **邮件 id 记进账本**，同一条 WAL 记录
               ─ CommitClaim(token)           ─▶ mail 服务：标记已领；重试同 token 幂等
```

- **领取是恰好一次的**：中间那一步把邮件 id 和道具放进同一条 WAL 记录（`game/handler/claim_mail_reward.go`）。
  三次调用不可能合成一个事务——邮件在另一个进程——所以窗口是真的：发放成功、`CommitClaim` 丢了、预留到期、重试用同一个 token 再预留一次。
  邮件服务只能把它算作重复**尝试**（它无从知道游戏发没发），游戏这边知道：第二次发放在账本上撞到自己，什么也不做，回的还是同一组数字。
  没有账本时这里就是第二叠道具——`game/handler/claim_mail_reward_test.go` 把这条钉住了（去掉账本判断即红）。
- **账本的界是时间**：保留期必须长于游戏能发出的最长邮件（生成配置 `mail.send_ttl` 720h），论证写在 `game/rewards`；
  清理在写入口做，不需要清扫器。发的邮件很多的游戏应该改成"commit 成功后就忘掉"或把账本放到邮件那侧。
- **仍然开着的**：预留成功但发放前崩溃（邮件被占到租约到期，玩家等，什么都没丢）；`CommitClaim` 丢失（账本挡的是重复**发放**，不是重复尝试）。
- `Claimable` 是 mail 服务对"现在领会成功吗"的回答，客户端不用从 Status 猜（Status 表达不了"被在途投递占着"）。
- 附件解不出奖励、或 AddItem 被拒（背包满）时 `CancelClaim` 归还预留，邮件不会卡在"held"直到租约到期。
- **demo 明确没做的一步**：把 claim token 带进 Nest 事务当幂等键。进程在 AddItem 之后、CommitClaim 之前死掉，租约到期后重试会再发一次
  （mail 侧记成重复 *尝试*，背包却多一叠）。生产做法是在同一事务里把 token 记到 Player 上，第二次拒发。
- 机器人脚本：add_exp 之后 `retry × 20 { wait 250ms; list_mail }`（效果链是异步的：WAL → outbox → JetStream → consumer → mail.Send），
  拿到第一封可领邮件的 id 再 `claim_mail`，断言背包计数 ≥ 领到的数量。

## 跨服务：game → match 组队

match 服务是通用的：队列由 `Queue{Mode, GroupSize, Partition}` 定义、subject 的 kind 对它不透明，它负责的是队列本身与
`Commit` 的原子性——**谁和谁一组不是它决定的**。`Candidates` 交出等待中的票，`Commit` 一次 CAS 成组；决定权是游戏策略，
所以住在 game 进程：

```
JoinQueue 端点 ─ Enqueue(队列, subject, requestID=帧序号) ─▶ match 服务（typed servicerpc，经生成的 Match() 客户端）
internal/service/game/matchmaker.go  每 500ms：Candidates → Grouping.Group → Commit → Sync_RecordMatch(World)
PollMatch 端点 ─ Ticket / Match ─▶ match 服务（带 subject，服务端校验票的归属）
```

- `game/matchmaking/queue.go` 是游戏对 match 说的话：duel 队列两人一组按到达顺序（`FirstComeGrouping`），ranked 队列按等级配（`ScoreWindowGrouping`：
  等级差 5 以内立刻配，窗口随最老候选的等待每秒放宽 5，上限 50）；ticket 的 Score 在 JoinQueue 时经 `PlayerLevel` 读 handler 取（Player 锁内），
  PollMatch 带 `mode` 选队列。subject kind 是 `player`。matchmaker 每 500ms 扫两个 pool，各用自己的策略。
- 所有对 match 的调用都从端点或普通 goroutine 发出，**从不在实体锁里**——World 只在 Commit 成功之后经自己的 Nest handler 记一笔。
- `configs/service/config.match.yaml` 的 `sweep_queues` 列出 duel 队列：match 进程只负责扫过期票，不负责成组。
- `Grouping` 是调用方的工具，不是 match 服务的配置：kit 的 match Mod 曾接受一个 `Grouping` collaborator 却从不执行它
  （RR-20260916-05，已删掉该参数）。demo 的 matchmaker 直接调 `FirstComeGrouping{}.Group`，这是那个接口唯一的用法。

## 跨服务：game → chat 聊天

chat 服务也是通用的：它按频道存带序号的消息、执行这个工程交给它的策略；哪些频道存在、一句话长什么样、谁在线能收到，
是游戏的事，住在 `game/chatroom/`（game 进程与 chat 进程都 import 它——这就是两边对 `text` 这个消息类型名达成一致的方式）：

```
SendChat 端点 ─ Publish(sender=认证玩家, requestID=帧序号) ─▶ chat 服务（typed servicerpc，经生成的 Chat() 客户端）
              └─ deliver：world 频道推给本进程 presence 里的每个在线玩家（含发送者），private 推给双方 ── ChatMessage 推送 10101
EnterGame 端点 ─ PublishSystem(actor=game, "player N entered") ─▶ chat 服务的特权入口 ── 同样 deliver
ChatHistory 端点 ─ History(viewer, after_seq, limit) ─▶ chat 服务（服务端校验读权限）—— 重连补读路径
```

- `internal/service/chat/collaborators.go`：策略写成决定——world（只有 world 1）与 private 对所有认证玩家开放，group 拒绝（demo 没有队伍 /
  公会成员关系），system 频道只读；`Bodies()` 注册唯一的 `text` 类型，校验函数与端点共用（端点先校验，免一次总线往返）；
  `System()` 无条件 `GrantSystem()`——这是对部署的陈述：chat 只经内网 NATS 可达，没有端点转发 PublishSystem。总线对不可信一方可达的部署要换成查调用者身份。
- **推送是捷径，序号是依据**：每条投递都带 `Seq`，错过推送的客户端用 `ChatHistory(after_seq)` 补读——机器人脚本就是这么写的
  （它自己那句的推送可能先于响应到达而被丢，所以 `wait_push` 外面套了 selector，再用 history 断言）。
- `Presence` 是**每进程**的在线集合：EnterGame 加入，推送失败且没有活动会话时移除。多个 game 进程的部署应改为订阅 chat 的流或走 room 总线，而不是从内存扇出。
- 机器人 transport（`loadtest/playertcp/conn.go`）靠帧头的 server-push 标志位识别推送——服务端给推送编的是自己的会话序号，与客户端序号同起点，
  只看序号会把一条世界频道推送当成正在等的响应。

## 跨服务：game → session 副本

session 服务是通用的"有界 run 原语"：每个 owner 同时只有一个活 run、Enter 按 RequestID 幂等、run 有截止时间、附着的资源恰好释放一次。
什么算一局、清了给什么，是游戏的事：

```
EnterDungeon 端点  ─ Enter(playerID, {Kind, RequestID=帧序号}) ─▶ session 服务（typed servicerpc，经生成的 Session() 客户端）
FinishDungeon 端点 ─ Finish(playerID, runID, succeeded|failed, outcome) ─▶ session 服务：状态机 + 释放（调这个工程的 Release()）
                   └─ 清了：MultiSync_AddExp(Player, World, 100) —— 与 AddExp 端点同一笔两实体事务，够升一级，于是又走一遍升级 → 奖励邮件
```

- `internal/service/session/collaborators.go`：`Release()` 是 session 服务对每个附着资源恰好调一次的钩子；demo 的副本不占外部资源，所以是一行日志 + nil。
  真实游戏在这里释放实例 / 座位，返回 error 会让服务保留待释放并在下次 Enter / sweep 重试。
- 第二局要等第一局结束：一个 owner 一个活 run 是服务的契约，重复 Enter 得到 `ErrAlreadyRunning` 的 coded 响应，不是第二个 run。
- **发奖是恰好一次的**：`ClaimDungeon` 事务把 run id 与经验、World 计数放进同一条 WAL 记录（`game/handler/claim_dungeon.go`），
  重放找到账本里已有的 run id 就什么也不做。判发奖的依据是 `session.Finish` 返回的 `run.State`，不是请求里的 `Success`——
  Finish 对终态 run 幂等返回，"调用成功"不等于"这次调用结算了它"。
- **账本有期限，期限与"还能不能领"是同一条规则**：账本记的是 run 的**结算时刻**（`run.FinishedAtUnix`，session 服务盖的章，
  不是 `time.Now()`），清理与准入共用 `dungeon.ClaimWindowClosed`。于是"记录被清掉"恒等于"这个 run 已过窗口、会被拒"，
  不依赖 session 服务保留多久——**succeeded 的 run 在 session 服务里没有存储 TTL，可以被 Finish 到天荒地老**
  （`run_ttl` 管的是 run 能开多久）。早先按"反正重放不了"给账本定 4 小时保留期是错的：另一笔领取顺手清掉旧记录之后，
  重放旧 run 就又拿了一份（RR-20260918-04）。
- **过窗口的领取是一条有名字的拒绝**（`dungeon_claim_window`，端点打 Warn 日志），不是静默的 0 ——
  真的有玩家一直没领而窗口过了，这件事必须让运营看得见，补发是运营的口径。把重复发奖改成静默漏奖只是换了个 bug。
- **Finish 与发奖仍不是一个事务**：进程死在两步之间，run 已终态、exp 没给；这是安全的方向——重试 FinishDungeon 会拿到同一个
  succeeded run，账本还没记，下一次尝试就补上了。生产要补的是"重试不靠客户端的善意"。
- session 进程默认不扫过期 run（`sweepOwners` 返回空并在日志里说明）：过期 run 由同一 owner 的下一次 Enter 懒解决。要及时释放资源的部署自己接 owner 列表。

## 跨域事务：送礼 saga（debit → deliver，失败补偿）

送礼把一个道具从 A 的背包挪进 B 的邮箱：背包是 A 的 Player 上的 Nest 事务，邮件是对 mail 服务的调用——两个事务域，
所以是 saga（`roost add saga GiftItem -service game -steps debit,deliver` 的产物，`saga/gift_item/definition.go`）。

- **开始**：`SendGift`（10013）→ `StartGift` Nest 事务：在 A 的锁内检查背包够不够，然后 `saga.EmitStart`——start 意图和这次事务同一条 WAL 记录，
  Data Engine outbox 把它送到协调器（saga Mod 在 game 进程里，`saga.start` 效果）。saga id = 发送方 + 会话 + 帧序号，同一帧重发不会开第二个 saga。
  这步不动背包：检查是建议性的，道具在 debit 之前被花掉，debit 会以同一个 coded 错误拒绝，saga 以 failed 结束、无事可补。
- **步骤**（`internal/service/game/gift_saga.go`，四个消费者，**两种形状**）：
  - debit（`GiftDebit`，`Bag.RemoveItem`，不够是 `item_short`）与它的补偿（`GiftRefund`，`AddItem`）走**原生路径**
    `saga.SubscribeDataEngineStep`：handler 在自己的 Nest 事务里 `inbox.Bind(command, reservation)` + `saga.EmitCompletion(...)`，
    于是背包变更、命令回执、协调器等的完成结果是**同一条 WAL 记录**。重投会撞上回执、回放已存的完成结果而不是再扣一次；
    提交前崩溃则三样都没发生。消费者自己不发布任何东西——它等回执被投影出来再 ack。
  - deliver（查 Player 集合确认收件人进过游戏，再 `mail.Send` 带附件）的业务是一次 bus 调用，不是 Nest 事务，没有东西可以绑，
    所以留在 `saga.SubscribeMongoStep`：Mongo inbox 先占命令 id，第二层幂等是 mail 服务自己的（Send 按 RequestID 去重，
    而 RequestID 就是命令的 IdempotencyKey）。deliver 的补偿什么也不做（邮件不撤回）。
  - 这条分界是规则不是权宜：原生路径给"业务本来就经 Nest 提交"的步骤用；拿它去包一次跨服务调用，等于把回执绑在一个
    并不包含那次副作用的事务上。
  - 业务拒绝也**提交**：不动数据，只写回执和一个失败的完成结果——协调器听不到的拒绝会让 saga 空等到 deadline。
    只有基础设施错误才返回 error（回滚，让投递退避重试）。
- **状态**：`GiftStatus`（10014）经 saga Engine 读记录。刚发完轮询会得到 `unknown`——start 意图还在 outbox → 协调器的路上；非本人的 saga 也是 `unknown`。
  终态：`completed` / `compensated` / `failed` / `manual_required`（补偿自己也拒绝了，例如退回时叠加已满——运维在 `gm.saga.list` 里看到并决定）。
- **机器人**：送给自己（背包 −1）→ 轮询到终态 → `expect_gift completed` → 领邮件（背包 +1）；再送给玩家 1（从未进游戏）→ deliver 拒绝 → debit 补偿 →
  `expect_gift compensated`。两条路都在 `loadtest -count 6` 里跑。
- **边界**：原生路径要求 core ≥ v1.15.7——协调器直到那一版才有原生完成效果的消费者（U-0231）。
  `LeaseDuration` 必须长于消费者的 `AckWait`（demo 取 2 分钟 vs 30 秒），否则租约会在消息还没 ack 时过期、让第二个进程开始同一条命令；
  消费者会当场拒绝这种配置。载荷解不出来的命令没有事务可以承载拒绝，只能靠重投与 deadline 收场。
  deliver 那条仍然是"两次提交"：mail 服务的去重是第二层，不是同一条记录。
- **需要 core ≥ v1.15.6 / kit ≥ v1.14.7**：此前 Mongo 存储上任何步骤拒绝都进不了补偿（U-0225，`step result timeout` 反复出现），送给玩家 1 那一段会卡住。

## 地图：Scene 是一个实体，它的 system 各自带锁

`game/entities/scene` 是地图实体：有 id、有框架驱动的生命周期、GM 能寻址、将来能跨进程——但**没有 DAO**
（`noPersist=true lifetime=runtime_rebuild`），因为一张地图每次启动都是从配置重建出来的。

```
Scene 实体 ─ OnInitFinish ─▶ sceneruntime.Runtime{ terrain, path_find }   按序 Start，逆序 Stop
   │
   └─ Terrain() / PathFind()  →  game/scene 里声明的接口，不是实现
```

- **system 自带锁，不借实体锁**。地形查询来自端点、来自刷新计时器、来自 AOI tick；让它们都去拿 Scene 的实体锁，
  等于让地图成为这个场景里所有事情的瓶颈。实体锁排的是**实体状态**的事务顺序，"这里能不能站"不是那个问题。
- **位置的权威在 DAO，别处不缓存**。读位置 → `MapComponent.Pos()` → DAO；写位置 → `MapComponent.MoveTo()` → DAO。
  AOI 将来会持一份 id→坐标的索引，那是索引不是第二个真相：只由这一条写入路径更新，永远不被当作"X 在哪"的答案读出来。
  两个可写的真相就是重连之后玩家出现在别处的那种 bug。
- **移动是两实体事务**（Scene rank 2 → Player rank 4）：位置改动与地图交出去的地面必须一致，所以它们一起提交、一起回滚。
  `durability=async` 而不是 `strict`——移动要持久，但不该每一步都等 fsync。
- **玩家不占地**（`Walkable` 而不是 `Occupy`）：两个玩家可以站在同一点，于是断线也没有残留占位要回收。占位留给真正会挡路的东西
  （地图里的墙、将来判定为实心的怪）。
- **落点用 `Place` 向外一圈圈找**：地图每次重建，玩家记住的位置可能不再可用；返回最近的可站点，而不是"某个"可站点。
  登录不会因为地面变了而失败。
- 拒绝只有一个码（`scene_position`）：越界、被占、不在地图上共用它——能精确知道哪些点被占的客户端，等于拿到了所有人的位置。

## 服务端权威状态同步：Player 是复制主体，scene 是它的调度器

`sync=true` 的实体有一个 **sync 主体**（`Player.Sync()`）：版本、脏掩码、packer。scene 是它的另一半——谁订阅了谁、
什么时候成帧、往哪条线发。

```
Player 的 DAO setter ─ MarkSync(mask) ─▶ Player.PublishSyncDirty() ─▶ 主体标脏
                                                                      │  room 每 200ms
主体 ─ Prepare(packer) ─▶ entitysync coordinator ─▶ RoomTransportSink ─▶ AdmitBatch ─▶ TCP 推送 10103
                                                            （快照/离场走 reliable，delta 走分片）
客户端：分片 → statesync.Reassembler → room.DecodeRoomWireFrame → DecodeRoomSubjectUpdate → 合并进本地视图
```

- **写入侧不是自动的**。把 DAO 标脏和把**主体**标脏是两件事：`PublishSyncDirty()` 在一次变更的末尾调一次，
  于是一次事务产出**一条** delta 而不是每个 setter 一条。复制是"一次已提交的变更"的副作用，不是"有人调了 setter"的副作用。
- **payload 是 DAO 自己的同步文档**（`MarshalSync(mask)`，与 `ApplySync` 成对）。掩码从生成的 setter 来、原样回到生成的
  marshaller，两端都不需要知道哪个 bit 是哪个字段——生成的字段掩码常量是 DAO 包私有的，别的包里的 packer 没法按字段裁剪
  （已登记给 review）。要自己的客户端协议的项目在这里换成自己的消息。
- **持久化水位**：`kit/dataengine` 的 `DurableLSN` 装进 `RoomManagerConfig.DurableWatermark`。流水线提交的部署会在 WAL 落盘前
  就确认事务，把这种内容外发等于让客户端看到服务端还可能丢掉的状态；房间会压住它直到水位追上。
- **谁订阅谁不在这里决定**：Scene 实体的兴趣系统决定（距离 + 社会关系），交回一串订阅变更，这个文件只负责说给 room 听。
  加一种关系（好友、同盟）不会碰到这个文件。
- **没有断连回调**：生成的接入层不通知会话关闭，所以"谁还在线"靠两条——推送失败就把人摘掉，以及有人入场时按
  `ActiveSessions` 扫一遍陈旧成员。chat 的 presence 有同样的问题。
- 机器人 `scene_watch` / `scene_expect` 是真客户端：解码、合并、断言。**推送消息必须在 loadtest 注册解码器**，
  否则推送到了也解不出来、静默丢弃。

## 刷新：场景该有多少东西活着

`configs/table/spawn.csv` 一行是一组：模板、数量、血量、中心点、半径、重生秒数。

- **系统只说"该生成什么"，装配层去建**。`Refresh.Due(now)` 返回 `[]SpawnRequest`，`internal/service/<game>/spawner.go`
  建实体、放位置、注册进 room 与 AOI，然后才 `Spawned` 回报。与兴趣系统只产出订阅变更是同一个形状——
  也正好避开包环：建实体要 lifecycle，lifecycle 要实体包，实体包持有 runtime。
- **数的是"被告知存在的"而不是"被请求过的"**。一次失败的创建会在下一次 `Due` 里重新出现；若按请求扣减，
  这个进程余下的时间里都会少一只，而且没人会说。
- **怪的 DAO 每个字段都是 `nopersist,sync`**：不存，但复制。这是为了让"位置住在 DAO 里、经组件读写"对**所有**实体一致——
  否则 demo 里会出现两种位置权威，而第一段要同时处理玩家和怪的代码就会挑错一种。
- **计时器在装配层**，不在 system 里：system 仍是纯状态机（`Due(now)` 的时间是传进去的），所以它可测、可重放。
- GM：`gm.scene.population` 看当前几只，`gm.scene.kill` 杀一只——表里写 20 秒，20 秒后回到满员且是一个**新 id**。
- **没做**：怪不动（没有 AI），战斗只有 GM 的"杀掉"，id 由进程本地计数器生成（第二个进程会撞，见 WANTED）。

## 装备栏：demo 唯一的嵌套字段，以及为什么它在这里

`PlayerDao.Equipment` 是一个嵌套 struct，里面是 `Slots map[int32]*GearPiece`——**嵌套里再嵌套**。

- **它在这里的理由不只是玩法**：两个 P1 缺陷（U-0236 换下的件仍标脏、U-0238 游离值以原 key 写回）都住在这条路径上，
  而在此之前生成的工程里**一个使用方都没有**，只有生成器自己的运行时门看得见。加了这个字段，demo 的测试与实跑也覆盖它了。
  加完当天就又抓到一条（U-0245：新建的 DAO 根本没接嵌套回调）。
- **穿装备是一个事务**：从背包取出、穿上、把换下来的放回背包——三件事一起提交。崩在中间要么丢件要么复制件，两者都是资产错误。
- **属性的 Gear 层从穿戴集算**，不是从整个背包：背着一把剑不该让人变强。
- **写入只经 `EquipmentComponent`**，而且整张 `Slots` 重写而不是就地改一个 entry——走 `SetSlots` 才会把离开的件解绑、
  把新来的件绑上。伸手去改 `Range` 里拿到的指针，等于改 DAO 仍然认为自己拥有的东西。

## 数据版本迁移：老文档怎么办

`//roost:dao coll=player db=game schema=2` 声明代码期望的版本，`db/migrations` 说一份旧文档怎么变成它。

- **加字段不需要迁移**（BSON 解码给零值）。需要迁移的是**改形状**：v1 把武器存成 Player 上的扁平 `weapon_id`，
  v2 存成按槽位键的 `equipment` 子文档，新定义无论如何都找不到那个旧数字。
- **注册是显式的，不是 `init()`**：一个因为包恰好被链接进来而运行的迁移，是没人决定要运行的迁移。
  服务 `Init` 的第一件事就注册它——晚于它的注册会让最初几次装载漏掉。
- **步骤只能读老版本真的有的东西**，并且要接受老文档里数字的各种 Go 类型（驱动与写它的那个 build 决定）。
- 测试分两半：变换本身，以及**接线**——`RestorePersisted` 在 v1 文档上真的会跑。
  一个正确但从不被调用的步骤才是这里真正的失败模式。
- **遗留键不会被清掉**：补丁只写 DAO 认识的字段，所以老文档里的 `weapon_id` 会留着。要清得靠一次性离线脚本。

## 兴趣：距离是一种来源，关系是另一种

"谁该收到谁的状态"由 Scene 实体的兴趣系统回答。它把**每一种理由都做成同一种来源**：

```
spatial.InterestManager ─┐
self（永远看得见自己）   ─┼─▶ 汇总（按来源计数 + 档位合并）─▶ []SubscriptionChange ─▶ room.Subscribe/Unsubscribe
team（匹配成队的队友）   ─┘
```

- **自己不是特例，是一种关系**。`spatial` 明确拒绝自观察（`evaluatePair` 第一行就 `observer.id == subject` 返回），
  而"我永远看得见自己"也确实不是距离的事——它是最退化的那种社会关系。做成来源之后，桥接里一行特判都没有。
- **一对 (观察者, 主体) 可能被多个来源同时持有**（队友正好站在旁边）。所以汇总层按来源计数：**第一个**来源命中才 Subscribe，
  **最后一个**来源撤销才 Unsubscribe。少了这一步，队友走远时距离来源发 Leave，会把关系来源仍然需要的订阅退掉——
  而这种 bug 只在"两个来源重叠又分开"的时序里出现（`interest_test.go` 里钉住了这条）。
- **档位合并取最高保真**（band 最小者胜）：关系压过距离，这就是"队友在地图另一头我也看得见他的状态"的代价。
  分带本身暂时只有一档——档位要能裁字段才有意义，而生成的字段掩码常量是 DAO 包私有的（见 WANTED）。
- **全程用 entity id**。entity id 把 unique id、kind、category 打包进一个 int64，跨 kind 唯一；unique id 只在 kind 内唯一，
  拿它做索引会让 Player 42 和 Monster 42 相撞。转成传输会话只在一个地方发生：`RoomSessionResolver`。
- **滞回**：进圈 120、出圈 150。在边界上来回微动不产生 Enter/Leave 抖动——每一次抖动都会重发一次快照。
- **格边长选 150 ≈ 视野半径**：一个观察者订阅的格数是 `(⌈2·出圈半径/格边长⌉+1)²`，格子远小于视野不会让 AOI 更准
  （半径判定本来就是精确的），只会让观察者每动一步的簿记成倍增加。框架不强制这个比值，所以它写在 `interest.go` 的注释里。
- **没做**：多房间（`spatial.InterestCluster`）、非玩家主体（`Show`/`Hide` 的入口已经留好，第三批的怪会用）。

## 属性：层、合成，以及两种存储意图

`game/gameplay/attribute` 声明属性与派生公式，`game/entities/player/attribute_component.go` 是容器的归属地。

- **三层**：`Base`（玩家自己的值）、`Gear`（背包的投影，按 item 表的 `attack` / `hp` 求和）、`Final`（合成视图）。
- **合成规则**：`Final = Base + Gear` 逐属性相加，**然后**才 `Update()` 重算派生属性。派生属性只算一次、且算在合成后的输入上——
  把各层自己的战力相加，等于把两个各用一半输入算出来的评分加起来，那不是战力的意思。
- **两种存储意图**：`AttrBase` 是 `persist,sync`；`AttrFinal` 是 `nopersist,sync`——能从已存的东西推出来的值不存
  （存了就是第二个真相，会和第一个打架），但它是客户端要画的，所以照样复制。Mongo 里只有 `attr_base`，线上两个都有。
- 容器在 `OnInitFinish` 填充：生成的 builder 先挂 DAO、再初始化组件，所以那时存储值已经在手上了。
- **没做**：会过期的层。真正的 buff 层需要一个时钟和一次扫描，而它该待在被 tick 调用的组件方法里，不是某个 getter 里的惰性检查。

## 排行榜：rank 服务，与领奖账本同一个幂等键

`game/ranking/ranking.go` 是游戏这侧的决定：哪个榜、一分是什么、什么让提交可重试。

- 清关提交用 `UpdateAdd`（本身**不**可重放）+ requestID = `clear:<run id>`——和奖励账本键的是同一个 run 身份。
  一次清关只发一次奖、只记一分，两件事同源不是巧合。
- 提交**不**以"这次是否真的发了奖"为条件：重放发现奖励已发，恰恰是"分可能还没记上"的那种情况。
- 提交在事务**外**（榜在另一个服务），两者不原子。崩在中间会留下"已发奖但没记分"——可恢复的方向，重试这个端点因幂等键只记一次。
- `RankTop`（10016）读榜头 + 自己的名次，两次读：一页是榜头，说不了不在榜上的人的事。榜 id 由服务端定，
  从线上收一个 board id 等于让任何人读到任何榜。

## 技能目录：启动时编译，客户端可查

`game/skills/fireball.json` 是 demo 的一个技能定义（`roost add skill` 生成骨架，再写上契约说明），`game/skills/catalog.go` 把目录下的 JSON
嵌进二进制并用 roost-core/skill 严格 `Parse` + `Compile`。game 服务在 `Init` 里先编译整份目录——**编不过就不起来**（一个坏定义死在启动，
不死在第一个客户端请求里），warning 数进日志；`SkillCatalog` 端点（10012）把编译出的 id 列给客户端，机器人断言至少一个且 0 warning。
技能**执行**（Host 读已锁 Entity、确定性 tick、checkpoint / replay）刻意没进 demo，见 `roost help skill`。

## 实时战斗：lockstep 帧同步（匹配之后的那一段）

Nest 是"服务器说了算"的状态：客户端请求、服务器改实体、结果落库。lockstep 是另一半——**确定性模拟**：
服务器只把所有人的输入排成编号的帧广播出去，每个客户端用同一份输入跑同一套模拟，得到同一份状态，服务器一个字节的状态都不发。
demo 把两者都给出来：匹配成功后，形成这场比赛的 game 进程开一个 `lockstep.Room`，两名玩家打满 45 帧（30 Hz，1.5 秒）。

- **框架给的**（`roost-core/lockstep`）：把输入按提交窗口排进帧、每个座位的重放保护、每个广播包携带最近几帧（丢一个包由后续包自愈，不重传）、
  给掉线重连的会话分页补发历史、把关键帧的哈希报告按法定人数判成 desync 裁决。
- **工程写的**（`internal/service/<game>/battle.go`）：房间的生命周期、驱动它的那条串行 goroutine、以及线。
  `lockstep.Room` 是单所有者状态（内部没有锁），所以所有调用都在一条 goroutine 上；端点把消息投进 channel，不直接碰房间。
- **线是 demo 已有的 player TCP 连接**：广播就是一条普通服务器推送（`BattleFrame`，10102），输入回来是一条普通请求（`BattleInput`，10015）。
  lockstep 不要求数据报通道，它要求帧能到；包里的冗余是让有损通道能用的东西。换成 UDP / KCP 部署时替换 `battleSender` 这个适配器即可，房间一行不动。
- **一条消息三件事**：`BattleInput` 同时带本帧输入、关键帧的模拟哈希、以及"从第 N 帧开始补发"的请求——这三样都是每帧节奏的东西。
- **收尾窗口**：房间切完最后一帧不会立刻关。客户端只有应用了关键帧才能报出那一帧的哈希，所以最后一个关键帧的报告必然晚于模拟结束；
  切完就关的房间会正好拒掉 desync 裁决需要的那批报告。战绩结算、反作弊校验也在这个窗口里做。
- **两端共享帧预算**：`game/battle` 里的 `Frames` 双方都读，所以客户端到最后一帧就停，而不是给一个刚关闭的房间发输入然后被拒。
  按条件结束的战斗要显式下发结果——房间的最后一帧不自带"我是最后一帧"。
- **机器人是真客户端**：`cmd/loadtest` 的 `battle` 动作用 `roost-core/robot` 的 `LockstepBot` 跑完整条链——推送喂给 bot，bot 按序应用帧到
  `battle.State`，再把本座位下一帧的输入和关键帧哈希发回。推送处理器只负责把包转交：它跑在会话读循环上，而应用一帧要在同一条会话上发请求，
  在那里应用会把读循环堵死在自己的响应上。
- **座位**：`poll_match` 返回的 members 顺序就是座位顺序，服务端开房间和客户端算自己座位用的是同一份顺序。
- **desync 长什么样**：两个客户端的模拟不一致时，房间在关键帧判出 outlier，game 日志里是一条 `battle: desync verdict` 的 ERROR。
  真实游戏在这里踢人或强制重同步；demo 只记录——所有客户端都诚实时它不该出现。
- **边界**：房间是进程内状态，只有形成比赛的那个 game 进程持有它。多 game 进程部署要把战斗做成自己的服务、按 match id 寻址；
  进程重启会结束它托管的战斗（框架能给重连会话补发历史，但只在房间还活着时）。ranked 匹配也会开房间，但 demo 的机器人只打 duel 那一场，
  没人进的房间在 `battleStartGrace + battleIdleTimeout` 后自己退出。

## GM 运维面：admin 命令，不是另一个 HTTP 服务

`internal/service/game/gm.go` 把四条 GM 命令注册进 app 发布的 admin 命令表（`app.ModAdmin`），ops Mod 把它们经 HTTP 端出来：
`GET /admin/commands` 列命令、`POST /admin/execute` 执行，鉴权 `X-Admin-Token` 或 Bearer；`ops.admin_enabled=false` 时两个端点 404。
demo 的**开发配置**开着 admin、token 是 `dev-gm-token`（`allow_dev_token: true`）；生产示例配置关着，而且 `config check --production` 拒绝 `dev-` token。

```bash
curl -s -H 'X-Admin-Token: dev-gm-token' http://127.0.0.1:9100/admin/commands
curl -s -H 'X-Admin-Token: dev-gm-token' -X POST http://127.0.0.1:9100/admin/execute \
  -d '{"name":"gm.player.add_item","trace_id":"t1","payload":{"player_id":100866,"item_id":2001,"count":1}}'
curl -s -H 'X-Admin-Token: dev-gm-token' -X POST http://127.0.0.1:9100/admin/execute \
  -d '{"name":"gm.mail.send","trace_id":"t2","payload":{"player_id":100866,"subject":"补偿","body":"抱歉","item_id":1002,"count":5}}'
curl -s -H 'X-Admin-Token: dev-gm-token' -X POST http://127.0.0.1:9100/admin/execute -d '{"name":"gm.world.stats"}'
curl -s -H 'X-Admin-Token: dev-gm-token' -X POST http://127.0.0.1:9100/admin/execute -d '{"name":"gm.saga.list","payload":{"status":"manual_required","limit":20}}'
curl -s -H 'X-Admin-Token: dev-gm-token' -X POST http://127.0.0.1:9100/admin/execute -d '{"name":"gm.saga.get","payload":{"id":"gift-100866-demo-100866-1789-15"}}'
```

- `gm.saga.list` / `gm.saga.get` 读 saga 协调器的记录（状态、当前步、最后一次错误、状态数据）；`manual_required` 是需要人的那一类。

- `player_id` 两种形式都收：客户端看到的唯一 id（`EnterGameResponse.PlayerID`，也是邮件的收件人 id），或 Mongo 里 Player 文档的 `_id`（完整实体 id，多了 kind / category 位）。
  两者不同：把 `_id` 当唯一 id 再包一层会得到一个不存在的实体（`entity aggregate not found`）。响应里同时给出 `player_id`（唯一 id）与 `entity_id`。
  唯一 id 从 Redis 计数器 `roost:demo:player_id` 分配，同一份 Redis 上反复起 demo 不会从 100001 重来——别猜，从 `EnterGame` 的响应或 Mongo 取。
- 加道具受 Player 的背包规则约束（每种道具的叠加上限），超出时命令按业务错误码拒绝（`bag_full`），不是 GM 越过规则。
- 命令做的就是游戏做的事：加道具 / 加经验走与端点相同的 Nest Sender（GM 加的经验升级同样发奖励邮件），发邮件走同一个 mail 客户端、附件用同一份 `rewards` 编码，玩家经 `ClaimMail` 领。
- `trace_id` 是邮件的幂等键：同一 trace 重试不会发两封。
- 为什么不是 webroute：这套 admin 命令表就是框架给运维面的位置（kit 的 nats / bus 也把 DLQ 命令注册在这里），共用鉴权、开关和 `/admin/commands` 的自描述；`webroute` 留给面向玩家 / 第三方的 HTTP 接口。

## World 的职责

World 有了自己的 DAO（`PlayersEntered` / `MatchesFormed`）和 `Stats` 组件：`RecordEnter` 在 EnterGame 之后、
`RecordMatch` 在 Commit 之后各是一次独立的 Nest 调用（Player 与 World 是不同实体、不同锁档）；`WorldStats` 是一个带返回值的
读 handler，锁内读、值出锁，端点从不碰 World 本身。

## 机器人压测 = 回归测试

```bash
go run ./cmd/accountctl -redis 127.0.0.1:6379 upsert-server -sid 1000   # 环境准备时一次：account 得知道这台服
make loadtest LOADTEST_COUNT=20                                           # 等价于下面这行
go run ./cmd/loadtest -endpoint 127.0.0.1:7000 -count 20 -metrics-addr 127.0.0.1:9300 -account-nats nats://127.0.0.1:4222
```

机器人走真实凭据：在压测进程里起一条 bus，用 account 的 typed 客户端依次 `Login`（demo 渠道，凭据 `demo:<open_id>`）→
`CreateRole`（在 `-server-id` 上）→ `SelectRole`，再以 `session:<player_id>:<token>` 握手；每次运行用新的 open id
（账号在一个服务器上只能有一个角色，且没有角色列表可查）。

`CreateRole` 会拒绝未知或未开放的服务器，而 **`UpsertServer` 刻意不在 account 的 RPC 接口上**：登记 / 开关服务器改变的是
所有玩家能登录什么，game 进程无权做，放在 Login 同一条总线上等于任何能到达 account 的进程都能关服。所以 demo 给了一个
操作员工具 `cmd/accountctl`：用 Redis 凭据直接打开 account 服务自己的 store 写入服务器记录——
`go run ./cmd/accountctl -redis 127.0.0.1:6379 upsert-server -sid 1000`，环境准备时跑一次。

每个机器人：`connect`（握手凭据是 account 签发的票据）→ `enter_game`（GetOrCreate Player）→ `add_item` → `add_exp`（升级，触发奖励邮件）
→ `join_queue` → `wait_push` 等服务端推送的 `MatchFound`（msg 10100，10s 超时后退回每 250ms `poll_match`，`selector` 节点）
→ `world_stats`（两个计数都得大于零）。
任何一步返回非零 code、或 `error_rate` / `p95` 超阈值，进程以非零码退出并打印 JSON 报告。**`-count` 要给偶数**：duel 两人一组。
**`-count` 不要超过 `player_access.tcp.max_connections_per_ip`（默认 128）**：机器人全从一个 IP 来，超出的连接在握手前就被
server 关掉，机器人报 `connect: auth send: robot session: closed`，server 侧计入 `player_tcp_connection_rejected_total{reason}`。
600 个机器人的实跑正好 128 成功、472 这样失败——这是上限在工作，不是缺陷；要压更大就在 game 配置里调高它。

- **runner / 场景树 / 动作注册 / 阈值门**全是 `roost-core/robot`，`cmd/loadtest/main.go` 只做三件事：注册本工程的消息
  （`action.MustRegisterCall` + 一个把泛型 Marshal 路由到生成的 pb 函数的 codec）、加载 `loadtest/scenarios/*.yaml`、
  把 flag 变成一个 `loadtest.Profile`。
- **`loadtest/playertcp`** 是生成的 player TCP 帧的客户端半边：core 的 robot 传输层说的是 12 字节小端帧，生成的 server
  说的是 16 字节大端带 magic / 版本 / flags 的帧，且要求先握手、每帧序号严格递增非零。适配器自己计数 wire 序号，
  用一张表把响应映射回机器人等待的序号；这是唯一同时知道两边格式的地方，常量要与 `server_gen.go` 同步。
- **`enter_game` 为什么不是 Nest handler**：Nest 处理的是已存在的实体，第一次登录还没有；创建 Player 是生命周期操作，
  端点直接走 `PlayerLifecycle.GetOrCreate`，所以这条协议只有 `roost add protocol`，控制器方法手写。
- 生成的 pb 类型没有 `GetCode()`，`RegisterCall` 的自动 code 检查不会生效，每个 call 用 `OnResp` 自己查 `Code`。
- **成组后推送**：matchmaker 在 Commit 成功后经传输层 `Runtime.PushPlayer` 给每个成员推 `MatchFound`（协议里是一个没有请求的
  notify 方法，生成的 bind 注册它的编码器）。推送到达的是该玩家**所有**已认证会话；玩家已下线时推送失败只记 debug 日志——票据里
  仍有 match_id，`PollMatch` 是兜底，推送是捷径而不是事实来源。

## 实体锁档与跨实体事务

`entity.EntityCategory` 的值就是锁的获取顺序（低的先锁）：Remote(1) → World(2) → PlayerScoped(3) → Player(4) → Other(5)。
生成器默认把新实体放在 Other——"持有它之后什么都锁不了"，对还没决定顺序的实体是安全的。demo 把 Player 放到
`EntityCategoryPlayer`、World 放到 `EntityCategoryWorld`，因为 `AddExp` 是一个**两实体事务**：
`handlerAddExp(target player.IProfileEntity, stats world.IStatsEntity, amount int64)`——Player 加经验升级、World 累计
`ExpGranted`，两处变更进同一条 WAL 记录，要么都落库要么都不。Nest 按档位锁：World 先、Player 后。生成的 Sender 变成
`MultiSync_AddExp(ctx, target, stats, amount)`，一个实体一个 id；`roost add endpoint` 只接单实体 handler，所以这个端点手写。

**档位编进实体 id**：改一个 kind 的 category 会改它所有实体的 id，已落库的文档全部失配。所以这是在第一条文档落库之前
决定一次的事；之后再改等于一次数据迁移。

## 可观测性

`deploy/dev/observability/` 是一套 Prometheus + Grafana：compose、抓取配置（五个进程的 ops 端口——game 9100、account 9101、chat 9102、mail 9103、match 9104，生成的配置已按此分配——+ 压测的 `-metrics-addr`）、
预置数据源与仪表盘 "Roost game-demo"。仪表盘按链路分组：玩家接入 → Nest 分发与锁 → WAL 落库 → 事件链与配置 →
跨服务 RPC → 机器人。每个指标对应链路上的哪一步、该看什么，写在同目录 `README.md`。它与生成的 `deploy/dev/docker-compose.yaml`
分开：那个文件会被重生成，观测是可选的。

## 本地实跑

三条命令（需要 Docker）：

```bash
make dev-up      # deploy/dev/docker-compose.yaml：Mongo 副本集 rs0、NATS JetStream、Redis
make dev-run     # deploy/dev/run.sh start：编译 bin/app，按 account → chat → mail → match → game 起五个进程，等每个 /readyz，
                 # 再用 cmd/accountctl 把 sid 1000 注册进 account（否则 CreateRole 拒绝）；日志与 pid 在 .dev/
make dev-smoke   # 两个机器人走完整条链（登录 → 聊天 → 加道具 → 升级 → 匹配 → 世界计数）
make loadtest LOADTEST_COUNT=20   # 更多机器人；偶数，duel 两人一组
make dev-stop
```

每个服务的 `configs/service/config.<服务>.yaml` 都有自己的 ops 端口（见上），所以五个进程可以同机共存；`make dev-status` 看谁在跑。

没有 Docker、或本机 27017 / 4222 / 6379 已被别的东西占着（一个不带 `--replSet` 的 mongod、一个没开 JetStream 的 nats-server 都不行）时，
用 roost-kit 的隔离环境（`scripts/integration/dataengine-env.sh up`：Mongo 副本集 `roost-it` 27117–27119、NATS JetStream 14222–14224、Redis 16379），
然后把五份配置改指向它——`sed` 一遍即可：

```bash
for f in configs/service/config.*.yaml; do sed -i '' \
  -e 's#mongodb://127.0.0.1:27017/?replicaSet=rs0#mongodb://127.0.0.1:27117,127.0.0.1:27118,127.0.0.1:27119/?replicaSet=roost-it#' \
  -e 's#nats://127.0.0.1:4222#nats://127.0.0.1:14222#' -e 's#^  addr: 127.0.0.1:6379#  addr: 127.0.0.1:16379#' \
  -e 's#^  prefix: roost$#  prefix: mysmoke#' -e 's#max_bytes: 8589934592#max_bytes: 67108864#' "$f"; done
make dev-run
go run ./cmd/loadtest -count 6 -account-nats nats://127.0.0.1:14222 -nats-prefix mysmoke
```

`dataengine.effects.max_bytes` 要调小是因为隔离集群只预留了 1GB 存储，默认 8GB 会报 `insufficient storage`。

看四处：`db.player` 在 DAO 标记写的 `db=game` 库里（不是 `dataengine.database`）；重启 game 再跑一轮，items 与 level 在原值上累加；
mail 的 Redis 里每个升级的玩家一封 `box:<id>`，`send:<EffectID>` 是幂等键；`db.world` 的 `players_entered` / `matches_formed` 随每轮增长；
chat 的 Redis 里 world 频道的流每次登录多一条系统公告、每个机器人多一句 hello。

**给每次实跑一个独立的 `nats.prefix`**（五个进程一致）。共享的 JetStream 集群里若残留了别的测试建的流、
且它的 subject 过滤覆盖 `roost.rpc.>`，JetStream 会用 PubAck 回应每一个 RPC 请求，与真正的服务端抢先——先到的赢，于是
读调用大面积得到 `bus: unsupported rpc response version 0`（PubAck 被当作响应信封解码），写调用偶尔成功。kit
集成测试留下的 `ROOST_IT_RPC_REQ_*` 流就是一例；换前缀即可，无需删流。

**同一个 Mongo 上换工程名 / 换 `nats.prefix` 重跑，game 进程会在就绪后几秒 fail-stop**：`dataengine outbox: hard backlog limit exceeded: oldest_age=… max=30m`。
所有生成工程的 `dataengine.database` 默认都是 `game`，上一轮工程留下的 effect outbox 行没有消费者会 ack（前缀不同、流不同），超过 30 分钟就触发
Data Engine 的硬积压熔断——这是它该有的行为（积压不该静默增长），不是 demo 的 bug。处置：`mongosh --eval 'db.getSiblingDB("game").dropDatabase()'`，
或给每个工程改 `dataengine.database`。

**换工程重跑后 game 日志里有 `saga: consumer processing failed … kind=completion … err="saga: not found"`**：
`saga.subject_prefix`（`roost.saga`）与 `saga.stream`（`ROOST_SAGA`）不跟着 `nats.prefix` 走，`dataengine.effects` 的
`roost.effect` / `ROOST_EFFECTS` 也一样——同一个 NATS 上两个工程共用这两条流。上一轮的完成消息还在流里，
而它的 saga 记录随 `saga` 库一起被删了，于是协调器对每条都答 not found，按 `result_max_deliver` 重投几次后丢弃。
不影响本轮（本轮自己的 saga 照常走完），但要干净就把这两组也改掉：
`-e 's#subject_prefix: roost.saga#subject_prefix: roost-mysmoke.saga#' -e 's#stream: ROOST_SAGA#stream: ROOST_SAGA_MYSMOKE#'`，
effects 同形；或者 `mongosh --eval 'db.getSiblingDB("saga").dropDatabase()'` 之外再删流。

**连续两次压测间隔不到 60s 时可能有一个机器人 `poll_match: ticket still waiting`**：上一轮失败机器人的 duel 票还在队列里（TicketTTL 60s），
被这一轮的第一个机器人配走了，剩下奇数个。这是 match 服务的正确行为，不是 bug；等 sweep 把过期票清掉再跑，或起偶数个再加一个。

## account 的 collaborators

`game` 模板给的 `Verifier` / `Allocator` 默认全拒绝（这是对的：没有校验的身份和会重复的 id 都不该有默认值）。
demo 换成能跑的版本：

- `Verifier` 只认 `demo` 渠道，凭据必须是 `demo:<open_id>`，其它渠道一律 fail-closed，返回的是它确认过的身份而不是提交
  上来的那份。**这不是身份校验**，只是把"该做的两件事"做了个样子，上线前换成对平台的真实调用。
- `Allocator` 用 account 服务自己 Redis 里的一个 `INCR` 计数器——持久、跨副本共享，满足 allocator 契约。它通过
  `account.RegistryBound`（roost-kit）在 `Provide` 里拿到 registry 再查 Redis 客户端：collaborator 是在 app 存在之前
  构造的，没有这个钩子就拿不到任何持久的东西。

## auth.go：两种凭据

`internal/access/player/tcp/auth.go` 认两种字符串：

- `session:<player_id>:<token>` —— **真实路径**。token 由 account 服务签发（Login → CreateRole → SelectRole），
  这里经 account 客户端 `ValidateSession` 校验，principal 用的是 account 返回的角色而不是 socket 声称的 id。
  客户端拿到 account 客户端靠生成的 TCP 传输层新增的 `RegistryBound` 钩子：authenticator 在 `Init` 里只有 viper 配置，
  Mod 在 `Provide` 里把 registry 交给它。
- `player:<id>` —— **不是认证**，直接信任 socket 给的 id，只为了能用一个裸 TCP 客户端把 demo 跑通。上线前删掉
  `demoTokenPrefix` 和读它的分支。

生成器默认给的是 fail-closed 骨架，demo 故意替换掉它——这也是 `roost config` 启用 TCP 时会检查的那个文件。
脚手架同时把 `player_access.tcp.enabled` 置为 true（等价于 `roost config enable player-tcp`），
所以 `roost project doctor -workflow player-tcp` 在刚生成的工程上全绿。

## 改这里的东西之后

1. `go test ./internal/roost/ -run TestDemo` —— 嵌入清单、步骤与文件一致性、生成后可解析。
2. 真正的验收是 CI 的 `demo` scenario：生成 + `go build` + `go vet` + `go test`。
   本地等价做法：

   ```bash
   go build -o /tmp/roost ./cmd/roost
   /tmp/roost project new planet -module example.com/planet -out /tmp/planet \
     -mods configdata,mongo,nats,dataengine,nest -template game-demo
   cd /tmp/planet && go build ./...
   ```

   codegen HEAD 生成的实体接线依赖 core HEAD（entity category），account collaborators 依赖 kit HEAD
   （`account.RegistryBound`），所以本地要用 `go.work` 指到工作树的 core / kit，不能只用已发布 tag。
   生成后再跑 `roost id check` 与 `roost generate --check`，确认 demo 留下的是一份干净的生成态。
