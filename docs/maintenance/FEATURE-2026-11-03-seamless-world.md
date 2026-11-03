# 功能设计：多进程无缝大世界（2026-11-03）

**状态：思路稿（维护者 2026-11-03：“可以先不做”“把这个方案思路先记到文档里”）；同日维护者要求“可以开一个分支先实现一版，并测试一下”，第一版原型只在分支 `feat/seamless-world-prototype` 上（§5 设计、§6 实测），未合入 main，不是现行能力。现行能力以 [MMORPG 接入手册](../framework/MMORPG-INTEGRATION.md) 为准。**

登记在 [DESIGN-TODO“按需求触发的功能”](DESIGN-TODO.md#按需求触发的功能)。

## 0. 问题

开放大世界无缝 MMO 的常见形态：多个 map server 各负责一片区域，玩家在区域间走动不加载、不断线。难点都在区域交界处：

| 难点 | 要回答的问题 |
| --- | --- |
| ① 跨界权威迁移 | 实体从 A 区走到 B 区，写权威怎样从 A 进程交给 B 进程：A 停写、状态交接、B 接管、连接不换或无感换，不双写、不丢最近修改 |
| ② 边界可见性 | A 区边上的玩家要看到 B 区边上的实体：边界带实体实时镜像到邻区（Ghost / 代理） |
| ③ 跨界交互 | 技能、子弹、AOE 打到另一区的目标，由谁结算、时序与失败语义 |
| ④ 分区与负载 | 区域怎样切，热点区域怎样拆分 / 合并 |

## 1. 当前 core 的位置（2026-11-03，v1.40.1）

| 能力 | 状态 |
| --- | --- |
| 同进程多地图，每图一个 Interest，共用一个 Manager 与连接，切图先提交归属再换订阅 | 已提供（[MMORPG §5](../framework/MMORPG-INTEGRATION.md#5-地图切换与断线重连怎样接)） |
| 单进程大地图 AOI / Block AOI | 已提供，容量见 KNOWN-LIMITS“广播与 AOI” |
| Player 按 sid 静态归属，跨服协作走 Saga / Remote | 已提供（[MMORPG §6](../framework/MMORPG-INTEGRATION.md#6-保持-player-归属的跨服协作)） |
| ① 动态跨进程迁移 | **没有**。底层零件：Remote 的 owner / lease / fence 与 `TransferRemoteOwnership`、DataEngine WAL 排空后冷加载、ownerroute；没有串成秒级无感交接 |
| ② 跨进程边界镜像 | **没有**。Interest 只在进程内；Remote Mirror 是给低频共享实体（公会、联盟）的快照复制，延迟撑不住 20Hz 移动 |
| ③ 跨进程实时交互 | **没有**。Nest 多实体事务只在单进程内；跨进程只有 Saga（最终一致，不适合战斗） |
| ④ 世界分区目录 / 动态负载 | **没有** |

另有与本题相关的现行边界：玩家接入层只有 TCP；服务端心跳最短 50ms（≤20Hz）。

## 2. 业界成熟方案

产品现状以记录时所知为准。

| 流派 | 代表 | 做法 | 评价 |
| --- | --- | --- | --- |
| A. Cell 分区 + Ghost | BigWorld（《坦克世界》等）；开源 KBEngine（C++）、GoWorld（Go，支持实体跨 game 进程迁移） | BaseApp 管玩家持久状态与连接，不随地图走；CellApp 管空间模拟，大地图切成 Cell。实体在所属 Cell 是 Real（唯一写权威），靠近边界在邻 Cell 留只读 Ghost 供邻区 AOI；跨界时 Real 迁到新 Cell、旧 Cell 变 Ghost；改别区实体是把消息转发给 Real 所在进程，不做跨进程事务；CellAppMgr 按负载动态挪 Cell 边界 | 最经典、长期商用验证；难点是迁移时的消息时序与 Ghost 一致性 |
| B. 空间数据库 + 多 Worker | Improbable SpatialOS（及 Unreal GDK）、Hadean | 世界状态放在中心化分布式实体组件库；每个组件同一时刻授权一个 Worker 写，Worker 按区域订阅；边界与权威交接由平台负责 | 思路干净，但运维与延迟成本高；Improbable 已基本退出游戏引擎业务，不宜作为选型 |
| C. Server Meshing + 复制层 | Star Citizen | 状态集中在独立复制层，游戏服务器是无状态模拟节点，只认领负责区域的实体权威；先静态网格再动态网格 | A 的现代化版本，状态与模拟彻底分开；工程量巨大、仍在演进 |
| D. 用产品设计绕开 | 多数商业 MMO | 区域间加载点 / 传送门；大陆连续 + 热点按人数分层（WoW 的 layering / sharding）；单分片 + 时间膨胀（EVE：一个星系一个节点，跳跃即加载，过载时减慢游戏时间） | 成熟、风险低；多数“看起来无缝”的 MMO 是“区域内无缝 + 区域间过渡 + 热点分层” |

选型参考：

| 需求 | 建议 |
| --- | --- |
| 同屏几百～一千人，地图可分区 | D：区域内单进程无缝，区域间过渡带，热点分层 / 分线。**roost 现在能做** |
| 大地图连续、跨区实时互动（国战、攻城跨边界） | A：Cell + Ghost；开源实现可参考 KBEngine、GoWorld |
| 超大规模且有专门平台团队 | C 方向，自研成本很高 |

## 3. 用现有 core 的做法（不需要框架改动）

1. **大区单进程 + 切图**：一个大区一个进程，区域间用传送门 / 加载切图。MMORPG 纵切已验证。
2. **伪无缝**：相邻区域放在同一进程，多图共用一个 Manager，切图只换订阅不换连接；进程边界放在天然阻隔处（海、山、城门），越过时切一次图。
3. **分线**：同一张地图开多条线，每条线一个进程，换线即换服。

## 4. 将来要做时的起点：沿 A 流派，Player 不迁移

与 roost 结构最贴近的拆法：

| 角色 | roost 对应 | 说明 |
| --- | --- | --- |
| BaseApp | 现有 Player 进程（静态 sid、DataEngine 持久、连接） | 不动。Player 的“静态 sid”正好是 BaseApp 角色，避开最难的持久权威迁移 |
| CellApp | **新增**空间模拟层 | Cell 内的 Real / Ghost 是**非持久**空间代理实体；跨界迁移的只是空间代理 |
| Ghost 同步 | **新增**低延迟跨进程通道 | 大概率经 NATS 点对点；不用 Remote Mirror（为低频共享实体设计，延迟不够） |
| 跨区交互 | **新增**路由 | 对齐 A 的“消息转发给 Real”，不做跨进程事务；明确时序与失败语义 |
| 分区目录 | 可在 ownerroute 上扩展 | 区域 → Cell 进程映射、扩缩容与迁移 |

实施前要先定的事：目标规模（同屏人数、区域数、跨界频率、跨界交互比例）；空间代理与 Player 持久状态的边界（哪些字段在 Cell、哪些在 Player，Cell 崩溃后从哪里重建）；Ghost 的生灭条件与允许的陈旧度；跨界交互的结算方（目标的 Real）与超时、结果未知的处理；Cell 进程故障时的接管方式。

这是牵涉 Nest、DataEngine、Sync 三块的大改，按 roost-coding 的重构流程先写完整方案再实施。

## 5. v1 原型设计（2026-11-03，分支 `feat/seamless-world-prototype`）

目的：用最小、可验证的一版把 §0 的 ①②③ 串起来跑通，量出交接与 Ghost 的延迟量级，暴露真正难的地方；不追求容量与上线质量。代码在新包 [`gameplay/cellworld`](../../gameplay/cellworld/README.md)，**不改 Nest、DataEngine、Sync 三块的现有代码与行为**。

### 5.1 包放在哪里

`gameplay/cellworld`。理由：它是建在基础设施之上的玩法层空间模拟（类似 `gameplay/movement`、`gameplay/skill`），v1 只依赖 `infra/base/spatial`（块索引与几何）、`infra/network/nats` 的契约接口 `IClient`、`gameplay/heartbeat` 的档位表；不在三大块的包表里，不新增跨块依赖边。没有放进 `framework/sync/`：Ghost 通道不是 Entity Sync（没有 Profile、基线、可靠增量这些语义），放进去会让 Sync 块出现一个与 entitysync 并列、语义不同的同步机制。将来接入 Interest / entitysync 时由本包 import 它们（gameplay → framework 方向本来就允许）。

### 5.2 “实施前要先定的事”的 v1 选择

| 事项 | v1 选择 | 性质 |
| --- | --- | --- |
| 目标规模 | 不定产品目标；测 1×2、2×2 网格，每 Cell 1k / 5k 个移动实体，玩家 5～10 m/s 跨界。数据只说明量级 | 原型简化；上线前按产品给的同屏人数、区域数、跨界频率、跨界交互比例重测 |
| 分区 | 静态二维等大方格：配置给出原点、格边长、行列数与“格 → Cell 进程 sid”映射，所有进程读同一份配置；格内坐标半开 `[min, max)`。不做动态负载、不挪边界 | 原型简化；动态分区（CellAppMgr）上线前另行设计，可在 ownerroute 上扩展目录 |
| 空间代理与 Player 持久状态的边界 | Cell 只持有非持久空间代理：ID、种类（玩家 / NPC）、所属 BaseApp sid、位置、速度、HP、版本。HP 在原型里由 Cell 结算（为了跨界交互有东西可结算），不回写 Player 持久数据 | **上线前必须解决**：哪些战斗字段在 Cell、何时回写 Player（DataEngine），Cell 崩溃从哪里重建 |
| Cell 进程内的执行模型 | 每个 Cell 一个独占 goroutine 的事件循环：消息、心跳、控制请求都串行在这个 goroutine 上，状态无锁；传输回调只解码后非阻塞投进有界收件箱，满了丢弃并计数（交接与伤害靠重传容忍丢失）。心跳周期必须是 `heartbeat.Periods` 的档位，缺省 50ms（20Hz）。**不跑在 Nest 上**，空间代理不是 Nest Entity，没有事务与回滚 | 原型简化；上线前决定 Cell 是否改成 Nest 实体 + 心跳驱动（复用事务、回滚、快池纪律） |
| Ghost 生灭与陈旧度 | Real 的位置落在某邻格“外扩 G 的矩形”内即向该格发 Ghost；每拍发完整状态（不是增量），按目标格合批、分块；离开边界带发显式删除；接收端 Ghost 超过 GhostTTL（缺省 1s）没收到更新就删，删除留墓碑防旧消息复活。要求 G ≥ 视野半径 R（邻区实体在视野内就一定有 Ghost）、G < 格边长（只镜像到相邻格）。允许的陈旧度：一拍 + 传输延迟（§6 实测） | Ghost 全量每拍发是原型简化；上线前改增量 / 分级频率并与 Sync 的 LOD 对齐 |
| 消息顺序 | 每个实体的版本是（epoch, seq）：epoch 是迁移代际，每次交接 +1；seq 每拍 +1、交接时随状态带走。接收端只接受更新的版本，旧消息丢弃。不依赖 NATS 的投递顺序 | 定案方向 |
| 跨界迁移（handoff） | 见 §5.3。迟滞：越出本格超过 H（缺省 2m，要求 H < G）才交接，边界附近抖动不来回交接 | 定案方向，参数待调 |
| 跨界交互 | 只做一种：伤害。攻击者所在 Cell 用本地视野（含 Ghost）检查距离，把伤害请求转发给目标 Real 所在 Cell 结算；结算方按 HitID（攻击者 ID + 攻击序号）去重，去重表随交接带走。目标在交接中时返回“重试 + 新 owner 提示”，攻击者 Cell 在期限（缺省 2s）内重试，过期向 BaseApp 报“结果未知”。见 §5.4 | 定案方向；结算方不复核攻击距离（信任攻击者 Cell）是原型简化，上线前要按反作弊要求复核 |
| BaseApp | Player 所在 game 进程（静态 sid）里的 `cellworld.Base`：记住每个玩家当前所在 Cell 与 epoch（只接受更大的 epoch），把移动意图（速度 + 单调输入序号）发给当前 owner；owner 在视野消息里带回已应用的输入序号，落后时 Base 重发最新意图（交接窗口里丢的输入自愈）。Cell 每拍把玩家视野内实体（含邻区 Ghost）整份推给 Base | 原型简化：视野是整份集合，没有接入 Entity Sync / Interest，也没有推到客户端；上线前把视野结果接成 Interest 订阅来源或由 Cell 直推 Gate |
| 通道 | NATS core pub/sub（至多一次，不用 JetStream：空间代理非持久，延迟优先）；每个进程一个收件主题 `roost.cellworld.<world>.<sid>`，消息带类型。经 `infra/network/nats.IClient` 接口，测试用真实 nats-server 与进程内替身 | 定案方向；鉴权（谁能往哪个 Cell 发）上线前补，参考 Gate 的 NATS ACL |
| Cell 故障 | Cell 崩溃后它的 Real 与 NPC 全部丢失（非持久）。Base 对玩家做静默看门狗：超过 SilenceTimeout（缺省 1s）没收到视野，就按最后位置向该位置的 Cell 重新进入，epoch 提示取已知 +1；邻格里旧 Ghost 靠 TTL 消失 | 原型简化；**上线前必须解决**：NPC / 怪物的重建来源、Cell 接管（热备或从 Player 侧重建）、重新进入的防重复 |

### 5.3 交接状态机

角色：实体在某 Cell 里是 **Real**（唯一写权威）、**Frozen**（已发出交接、等结论，只读）、**Ghost**（只读镜像）。每个 Cell 另有一张 `seen` 表：实体 → 本格见过的最高交接 epoch 及是否接受（带 TTL，覆盖重传窗口）。

1. 旧格 O：Real 越出本格超过 H，目标 N = 位置所在格。O 先把自己的记录改为 Frozen、epoch+1（=E），**之后不再写它**（输入丢弃、伤害回“重试”），再向 N 发 `Offer{完整状态, E, 已应用输入序号, 伤害去重表, O 的 Ghost 目标集}`。一个 epoch 只对应一个目标，重传只发给同一个 N。
2. N 收到 Offer：
   - 本地已是该实体的 Real / Frozen 且 epoch ≥ E，或 `seen` 记录的 epoch > E：说明 E 已被接受、甚至已经再交接过 → 回“已接受”（幂等，重复 Offer 不会重装）。
   - `seen` 恰好是 E：按当时的结论原样回复。
   - E 比见过的都新：位置确实在本格、容量未满就安装为 Real（有本地 Ghost 则就地转正），记 `seen`，回“已接受”，并通知玩家的 Base（owner 变更）；否则记“拒绝 E”并回拒绝。
3. O 收到“已接受 E”：位置仍在本格边界带内就降为 Ghost（owner = N），否则删除。也接受两种隐式结论：收到 N 发来的 epoch ≥ E 的 Ghost 更新（只有 E 的 Real 能发出）、收到 epoch > E 的 Offer（实体已经从 N 再交接回来）——都说明 N 接受过 E。
4. O 收到“拒绝 E”：N 承诺以后不会再接受 E，O 以 epoch E 恢复为 Real，退避一段时间后再试（下一次用 E+1）。
5. 超时：O 每 OfferRetry（缺省 100ms）重传；到 HandoffAbort（缺省 3s）仍无结论，O **删除**本地记录（不恢复为 Real，因为无法证明 N 没装），给 Base 发 `Lost{E, 最后位置}`。Base 若已从别的 Cell 收到 epoch ≥ E 的消息就忽略；否则等 LostGrace 后按最后位置重新进入（目标就是 N：N 若已有 Real 则幂等返回；若有存活 Ghost 说明 Real 在别处，拒绝并提示 owner）。

保证与限制：同一 (实体, epoch) 至多一个 Cell 取得 Real；O 先 Frozen 后发 Offer、N 收到后才安装，所以任何时刻至多一个 Real（Frozen 不是 Real）。消息丢失、重复、乱序只会推迟结论，不会双写。代价：N 不可达超过 HandoffAbort 时实体会丢失，由 Base 重新进入补回（非持久状态回到 Base 最后看到的值）。

### 5.4 跨界伤害

`Base.Attack(attacker, target, damage)` → 攻击者 Real 所在 Cell：攻击者必须是本格 Real；目标必须在本格视野里（Real / Frozen / Ghost）且距离 ≤ AttackRange。目标是本格 Real 就地结算；否则登记待决请求，把 `Damage{HitID, 目标, 伤害}` 发给目标的 owner（Ghost 的来源格或 Frozen 的交接目标）。结算方：是 Real 就查去重表（命中则回“已结算”带当前 HP，不再扣）、死亡则拒绝、否则扣血记表；Frozen / Ghost / 不认识则回“重试 + owner 提示”。攻击者 Cell 每 AttackRetry 重发直到收到结论或到期，到期回 Base“结果未知”（可能已结算）。去重表随交接带走；去重窗口必须长于重试期限，表满（窗口内同一目标挨打过多）时拒绝新伤害而不是淘汰旧记录，保证不重复结算。

### 5.5 验收设计

单元（状态机用手动投递的确定性夹具，不起 goroutine）、进程内多 Cell（进程内替身传输可注入丢失 / 重复 / 延迟）、真实 nats-server（私有端口，测试自起自停）上的多 Cell、多 OS 进程（测试二进制自 exec，每个 Cell 一个进程）。全部用例检查：同一 (实体, epoch) 只有一个 Cell 取得 Real、Real 区间不重叠、Base 看到的 epoch 单调、边界带内邻区实体在视野里不连续缺失超过约定帧数、每个 HitID 至多结算一次。测量用 `ROOST_CELLWORLD_PERF=1` 开启。
