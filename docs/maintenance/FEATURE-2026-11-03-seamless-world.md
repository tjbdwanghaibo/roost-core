# 功能设计：多进程无缝大世界（2026-11-03）

**状态：思路稿（维护者 2026-11-03：“可以先不做”“把这个方案思路先记到文档里”）；同日维护者要求“可以开一个分支先实现一版，并测试一下”，第一版原型只在分支 `feat/seamless-world-prototype` 上（§5 设计、§6 实测）；同日维护者定下 v2 方向（Cell 是 Scene 的模式、Cell 部分是 Nest 实体），v2 也只在该分支（§7 设计、§8 实测与剩余缺口）；同日维护者定下 D1～D4，v2.1 的 NPC 降档与 demo 接入的方向问题见 §9；同日维护者定下 D5～D8，v2.2 的 game-demo 接入与多进程端到端见 §10。未合入 main，不是现行能力。现行能力以 [MMORPG 接入手册](../framework/MMORPG-INTEGRATION.md) 为准。**

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
| 跨界迁移（handoff） | 见 §5.3。迟滞：越出本格超过 H（测量配置取 2m；要求 H < G，且视野半径 R + H ≤ G）才交接，边界附近抖动不来回交接 | 定案方向，参数待调 |
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

## 6. v1 实测结果与已知缺口（2026-11-03）

### 6.1 用例与结果

全部在 macOS（Apple M5）本机运行，`GOWORK=off go test -race -count=1 ./gameplay/cellworld/` 通过；真实 NATS 与多进程用例要求 PATH 里有 nats-server（实测 2.14.5），测试自起私有端口、用完杀掉。

| 层 | 用例 | 断言 |
| --- | --- | --- |
| 单元 | 分区映射、边界带、迟滞、版本序；交接正常 / 丢 3 次 Offer 与 2 次回复且其余全重复 / 回复全丢靠 Ghost 隐式确认 / 超时只删不恢复（扣住的 Offer 迟到后对方安装，仍只有一个 Real）/ 超时后重进取下一 epoch / 被拒恢复并用下一 epoch 重试 / 回复扣住时来回越界 / 斜穿四格交接链 A→B→D；Enter 幂等与“有存活 Ghost 就拒绝”；Ghost 旧版本与墓碑 | 每次身份变化时同步检查：同一实体同一时刻至多一个 Real、同一 (实体, epoch) 只取得一次、epoch 严格递增。这批用例先对空桩运行，14 条红（记录在提交 7c71f762 之前的本地运行），实现后转绿 |
| 单元 | 跨格伤害：全部消息重复 + 同一攻击重发；距离 / 可见性拒绝；目标冻结期间“重试”改投新 owner；结算后回复丢失、目标迁走，去重随交接带走；回复全丢报“结果未知”；去重表满拒绝 | 每个 HitID 至多扣一次血；Applied 都真的结算过；Unknown 可能已结算 |
| 进程内多 Cell | 直线穿越、边界抖动（12 次交接）、斜穿四格交点、跨界攻击、Offer 延迟 300ms 的攻击、Cell 间 20% 丢包 + 10% 重复 + 0～30ms 乱序下的来回穿越与攻击、Cell 崩溃后重进 | 视野连续：静止 NPC 在视野半径内时连续缺失 ≤ 1 帧（实测 0），视野间隔 ≤ 3 拍 + 50ms（实测最坏约 100ms，即交接时 2 帧）；至多一个 Real；伤害只结算一次。两处变异（确认后不降级 / 不发 Ghost）分别让断言变红，说明断言能抓到双写与边界不可见 |
| 真实 NATS | 同一组场景（直线、抖动、斜穿、攻击），每个 Cell 与 Base 各一条连接 | 同上，约定放宽一拍（缺失 ≤ 2 帧、间隔 ≤ 4 拍 + 50ms）；实测缺失 0、最坏间隔约 101ms |
| 多 OS 进程 | 两个 Cell 各一个子进程（测试二进制自 exec），父进程跑 Base：来回穿越后跨界攻击 10 次 | 子进程 pid 互不相同且不是父进程；按各进程事件的系统时间排序后跨进程至多一个 Real、epoch 单调（玩家最终 epoch 3）；10 次伤害各只在一个进程结算一次，最终 HP 70 |

### 6.2 测量（原型量级）

2×2 网格、每格一个 OS 进程、单机 nats-server；40 个玩家经 Base 随机游走，NPC 3 m/s 随机游走、跨格交接。20s 窗口、两轮，原始输出与环境见 [benchmarks/cellworld-v1-20261103](../performance/benchmarks/cellworld-v1-20261103/README.md)。下表是两轮、四格的范围：

| 每格 NPC | 每格 CPU（一个核） | 每拍耗时 p50 / p99 | 每格出 / 入 | Ghost 延迟 p50 / p99 | 交接延迟 p50 / p99 | 视野延迟 p50 / p99 | 窗口内交接 / 格 |
| ---: | ---: | --- | --- | --- | --- | --- | ---: |
| 1k | 4.3～6.3% | 0.9～1.6ms / 2.5～4.6ms | 230～370 msg/s、310～400 KB/s / 70～75 msg/s、210～260 KB/s | 1.5～1.9ms / 3.8～5.9ms | 1.6～2.3ms / 2.8～14.8ms | 0.22～0.28ms / 1.1～1.2ms | 60～79 |
| 5k | 13.4～17.1% | 3.7～5.1ms / 5.9～9.8ms | 295～438 msg/s、1.4～1.7 MB/s / 137～141 msg/s、1.1～1.2 MB/s | 2.6～3.5ms / 6.0～6.7ms | 4.5～6.0ms / 6.3～11.1ms | 0.15ms / 0.74～0.77ms | 330～356 |

两档都没有被拒、放弃、重传、收件箱丢弃或 Real 冲突。观察：

- **交接延迟 ≈ 目标格一拍的处理时间**：Offer 在目标格的事件循环里排在正在执行的 tick 后面，5k 时 p50 从 1k 的约 2ms 涨到约 5ms，跟每拍耗时同步增长。格子更大时需要把 tick 切片或让交接消息优先。
- **带宽主要是全量 Ghost**：5k 时每格约 600 个实体在边界带里，每拍每个邻格发一份完整状态，出流量约 1.5 MB/s/格；增量与按距离降频是第一个要做的容量优化。
- **玩家视野在交接时最多断 2 帧**（旧格最后一份视野到新格第一份视野约 100ms），没有实体从视野里缺失；这依赖 R + H ≤ G（构造时校验）。
- 这是同机数据：Cell 与 nats-server 在同一台机器、进程间走本机回环，跨机的网络延迟要另测。

### 6.3 已知缺口

| # | 缺口 | 性质 |
| --- | --- | --- |
| 1 | Cell 不跑在 Nest 上，空间代理不是 Nest Entity：没有事务与回滚、没有快池纪律，战斗逻辑一复杂就需要 | **上线前必须解决**：定 Cell 的执行模型（Nest 实体 + 心跳驱动，或保持事件循环但定义自己的事务边界） |
| 2 | 视野只到 Base，是整份集合；没有接 Entity Sync / Interest，也没有推到客户端 | **上线前必须解决**：视野接成 Interest 的订阅来源，或 Cell 直推 Gate |
| 3 | HP 等战斗状态只在 Cell、非持久，不回写 Player；交接放弃（3s）时回到 Base 最后看到的值 | **上线前必须解决**：定字段归属与回写时机 |
| 4 | NPC / 怪物在 Cell 崩溃时全部丢失，没有重建来源 | **上线前必须解决** |
| 5 | Cell 进程没有单实例保证：同一 sid 起两个 Cell 进程会出现两个 Real；故障检测只靠 Base 的视野静默看门狗（1s） | **上线前必须解决**：按“App 级统一保证”，Cell 进程继承 core app 的单实例锁与 fail-stop，不在本包另做租约 |
| 6 | 玩家 Real 没有租约：Base 进程消失后，Cell 里的玩家 Real 一直留着 | **上线前必须解决** |
| 7 | 重进防重复只有一道：目标格有存活 Ghost 就拒绝。视野与 Ghost 同时丢失超过宽限期时理论上能造出第二个 Real | **上线前必须解决**（例如重进前向邻格 / 目录确认） |
| 8 | 结算方不复核攻击距离（信任攻击者所在格）；没有鉴权，任何连上 NATS 的进程都能往 Cell 主题发 Enter / Damage | **上线前必须解决**：复核与 NATS ACL（参考 Gate 的 ACL 用例） |
| 9 | 伤害去重窗口按各 Cell 的系统时间过期，跨机时依赖时钟一致 | **上线前必须解决**（跨机前改成相对时间或拍号） |
| 10 | 线上格式没有版本号 | **上线前必须解决** |
| 11 | Ghost 每拍全量、没有增量 / LOD | 原型简化（容量阶段第一项） |
| 12 | 交接延迟随目标格每拍耗时增长 | 原型简化（容量阶段：tick 切片或交接优先） |
| 13 | 静态分区，一个进程一个格，没有动态负载 / 热点拆分 | 原型简化 |
| 14 | 攻击只能打本格视野里的目标（R ≤ G）；远程、大范围 AOE 没有设计 | 原型简化（需要单独设计跨多格的范围交互） |
| 15 | Leave 尽力而为；配置不经 `app.LoadConfig` 声明 | 原型简化 |
| 16 | 测量是单机、20s 窗口、两轮；没有跨机、长稳、10k 以上 | 原型简化 |

### 6.4 对后续方向的判断

- 交接协议本身（先冻结后 Offer、epoch 唯一目标、幂等回复、隐式确认、只删不恢复）在丢失 / 重复 / 乱序 / 来回 / 链式交接下守住了“至多一个 Real”，协议规模小（`handoff.go` 约 160 行），可以作为正式实现的起点。
- 真正的工作量不在交接，而在 6.3 的 1～6：Cell 的执行模型与事务、战斗状态的持久归属、NPC 重建、Cell 单实例与故障接管。它们都牵涉 Nest / DataEngine / App，建议下一步先由维护者定 1（Cell 是否上 Nest）与 3（战斗字段归属），再决定是否进入正式方案。
- 如果产品需求是 §2 的“区域内无缝 + 区域间过渡 + 热点分层”，现有 core 已能做，不需要这条线；只有大地图连续且跨区实时互动是硬需求时才值得继续。

## 7. v2 设计：分区 Scene（2026-11-03，分支 `feat/seamless-world-prototype`）

维护者 2026-11-03 的决定：BigWorld 的 Cell 是 **Scene 的一种模式**（不另造“世界”）；角色在分区 Scene 里的部分是不持久的 Nest 实体 **SceneActor**，心跳驱动；战斗字段（位置、HP、Buff、冷却）放在 SceneActor 上随交接走，Player 留在原 sid、固定时机回写；Cell 崩溃按 Player 最近快照重建，Monster 按刷怪配置重建；不用 Remote 托管 + `TransferRemoteOwnership`（交接延迟太高）。补充（同日）：分区模式新增的状态尽量放进 Nest 实体（Scene、SceneActor、Monster），用心跳与事务驱动，**不新增带锁的进程级管理器**；现有管理器保持原样，普通 Scene 行为不变。

本节先写形状与取舍，代码按 §7.9 的阶段落地，实测与剩余缺口见 §8。

### 7.1 两种 Scene，一套成员接口

| | 普通 Scene（缺省，行为不变） | 分区 Scene（声明了分区配置） |
| --- | --- | --- |
| 进程 | 一个进程 | 每格一个进程（Cell 进程），格 → sid 静态映射（v1 的 `Grid`） |
| 角色在场景里的代表 | Player 实体本身是成员（Interest subject，AOI 跟随它） | Player 所在格的 **SceneActor**（不持久 Nest 实体，ID = `BuildEntityID(玩家 uniqueID, KindSceneActor)`，跨格交接 ID 不变）；Player 只留在 Base 进程 |
| NPC | Monster（ephemeral） | Monster 是所在格的 Real，跨格交接 |
| 客户端看到什么 | 本进程 Manager 里的 Player / Monster subject | Base 进程 Manager 里的 **mirror subject**（§7.3），内容来自玩家当前所在格的 witness |
| 成员接口（业务调） | Join / Ready / Leave / Move / SetView / 攻击 | 同名接口；分区模式下由服务层桥把调用转成发往当前 owner 格的消息（§7.6） |

“Base 进程”就是 Player 绑定的 game 进程（静态 sid，DataEngine、连接都在这里，v1 的 BaseApp）。Cell 进程也是普通 game 进程（同一个二进制，配置里声明它托管哪个格）：它可以同时是一些玩家的 Base。

### 7.2 Sync 补的能力：远端只读对象作为 subject（mirror）

**问题**：分区模式下客户端连接在 Base 进程，视野内容的权威在 Cell 进程（本格 Real + 邻格 Ghost）。Base 要把这些“不是本进程 Nest 实体”的对象，和本进程的 Player 实体一起，经同一个 Manager、同一条连接推给客户端：全量 / 增量 / 删除，按 (epoch, seq) 丢旧（玩家交接时旧格和新格的 witness 会交错到达），收不到更新就过期删除。

**现有抽象**（读源码确认，codebase-memory 本次连不上）：Manager 的 subject 是 `*entity.SubjectSyncState`——“不含观察者的内容状态机”，`entity.NewSubjectSyncState` 是公开构造，不绑实体时 `withEntityLock` 直接执行 packer、`commitGate` 为空即就绪、`MarkDirty` 直接标脏并唤醒；`Manager.Register / Unregister / RegisterAfterRetirement` 只看状态和 ID；Interest 的 `Show / Move / Hide` 只认 subject ID 与坐标。也就是说 **Manager 与 Interest 本来就不要求 subject 是 Nest 实体**，缺的只是“谁来维护这份远端内容”的那一层：版本序、墓碑、过期、按版本决定全量还是增量、登记 / 显示 / 移动 / 退役的固定顺序。

**最小改动点**：在 `framework/sync/entitysync/policy` 新增一个文件 `mirror.go`（类型 `Mirrors[T]`）组合现有公开 API，**不改 Manager、Interest、SubjectSyncState 的任何现有代码**：

```go
type MirrorVersion struct{ Epoch, Seq uint64 } // 字典序；epoch 是迁移代际，seq 跨 epoch 单调

type MirrorConfig[T any] struct {
	Manager  *entitysync.Manager
	Interest *Interest                    // 空间可见性：mirror 是 Show 进 AOI 的目标
	Position func(T) spatial.Point
	Snapshot func(T, entity.SyncProfile) (entity.FrozenSyncPayload, error)
	Delta    func(T, entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) // 可空：只发全量
	TTL      time.Duration                // 超过它没有 Apply 就删除（发 remove）
}

type MirrorItem[T any] struct {
	ID        int64; Namespace string; Kind uint32
	Version   MirrorVersion
	Content   T
	Mask      uint64 // 相对上一个已接受版本改了哪些字段；0 = 全量
}

func NewMirrors[T any](MirrorConfig[T]) (*Mirrors[T], error)
func (m *Mirrors[T]) Apply(item MirrorItem[T], now time.Time) MirrorOutcome // Created / Updated / Stale / Tombstoned
func (m *Mirrors[T]) Remove(id int64, ver MirrorVersion, now time.Time) bool  // 显式删除，留墓碑
func (m *Mirrors[T]) Expire(now time.Time) int                                 // 过期清理，返回删除数
func (m *Mirrors[T]) Get(id int64) (T, MirrorVersion, bool)
func (m *Mirrors[T]) Close()
```

语义：

- 新对象：建一个不绑实体的 `SubjectSyncState`（packer 读 mirror 当前内容），`Register`（同 ID 还在退役就 `RegisterAfterRetirement`，remove-before-create 不变）→ `Interest.Show`。顺序与 demo `ShowSubject` 相同（先登记再显示）。
- 更新：版本不新于已接受的丢弃（计数），新的替换内容；`Mask == 0` 或配置没给 `Delta` → `MarkFullDirty`，否则 `MarkDirty(mask)`；位置变了 `Interest.Move`（直接移动，不走提交门：mirror 没有事务）。增量的正确性前提是 mask 相对 **上一个被接受的版本**；跳过了中间版本（丢消息）时调用方要给全量——cellworld 的 witness 每拍是全量，不靠增量链（§7.5）。
- 删除 / 过期：`Interest.Hide` → `Manager.Unregister`（订阅者下一帧收到 ObjectRemove）；墓碑保留 2×TTL，不新于墓碑版本的迟到更新不会让对象复活。
- 锁：一把短锁保护 mirror 表（与 Interest 登记侧同类），调用 Manager / Interest 都在锁外按 ID 串行（同 ID 的 Apply / Remove 不交错）；可以在 Nest 快池上调用（不等待、不做 I/O）。

**为什么不另开第二个 Manager**：(1) 客户端一条连接一个 Sync 流（`frame.Receiver` 一份基线），两个 Manager 就是两条流、两套基线，客户端要合并；(2) 同一个对象可能先是 mirror、后来又是本进程实体（或反过来），同一 Manager 里是同一个 subject ID，跨 Manager 只能 remove + create，视野不连续；(3) 每会话每拍一帧、冷创建预算、慢会话规则都按 Manager 计，拆成两个就各算各的；(4) `ClaimSyncOwner` 已经规定一个内容状态只喂一个 Manager，mirror 与 Player 放在同一个 Manager 里不需要例外。

**不做的**：不在 Cell 进程上给 Ghost 建 subject（Cell 进程没有这些客户端）；不改 entitysync 的帧格式、Profile、预算。

### 7.3 Cell 侧：状态全在 Nest 实体上

| 状态 | 放在哪 | 驱动 |
| --- | --- | --- |
| 本格区域表：邻格 Ghost（只读镜像，不是实体）、交接 `seen` 记录、墓碑、跨格伤害待决表、本格 Real 的位置索引 | **Scene 实体**持有的 `cellworld.Region`（手写 DAO：事务内的修改登记逆操作，回滚即还原；与 `skill/combatcomponent.CombatDao` 同形） | Scene 的 `scene.cell` 心跳（50ms）+ Scene 上的 Nest handler |
| 角色 / NPC 的格内状态：位置、速度、HP、Buff、冷却、角色（Real / Frozen）、epoch / seq、已应用输入序号、伤害去重表、交接中的目标与 Offer | **SceneActor / Monster 实体**的 DAO（nopersist） | 实体自己的 `cell.actor` 心跳 + 实体上的 Nest handler |

框架包 `gameplay/cellworld` 不定义实体（实体由生成器与 game 定义），只提供：

```go
// 实体实现它：读 / 写自己的格内状态（写经 DAO setter，登记 undo 与同步脏标）。
type Actor interface {
	entity.IThreadSafeEntity
	CellState() ActorState
	SetCellState(ActorState)
}
// Region 由 Scene 实体持有；只在 Scene 的锁内调用。
func NewRegion(RegionConfig) (*Region, error)
// 心跳回调与 handler 体（game 在自己的 Nest handler 里调用这些函数）：
func (r *Region) Beat(now time.Time)                       // 刷新索引、发 Ghost 批、过期、伤害重试、算 witness
func (r *Region) Receive(m *Message, now time.Time) Effects // Ghost / Offer / 回复 / 伤害 / 进入
func ActorBeat(a Actor, g *Grid, now time.Time) ActorEffects // 积分、判交接、冻结、重传、到期放弃、租约到期
```

**执行模型**：

1. 传输回调只解码，按目标投 Nest：发给角色的（Input、OfferReply、Damage、Lease）→ `DispatchMulti(handler, [sceneID, actorID])`；发给区域的（Ghost 批、Offer、Enter、DamageReply）→ `Dispatch(handler, sceneID)`。全部 handler 都先锁 Scene（World 档 2）再锁角色（Other 档 5），锁序固定。
2. 角色心跳只锁角色本身：积分位置、判交接、冻结（epoch+1）——它不能反过来锁 Scene（违反锁序），所以“我在哪、我冻结了”这类给区域的报告在 **AfterCommit** 里投进区域的有界收件队列（无锁的 channel，满了丢弃计数；下一拍重报），由 Scene 心跳 / 下一次 Scene handler 在 Scene 锁内取出应用。Ghost 批在 Scene 心跳里组好、AfterCommit 发出。
3. 外发（Publish）一律在 AfterCommit：回滚的事务不外发。

**代价（§8 实测）**：每个角色每拍一笔 Nest 事务（v1 是一个 goroutine 里的循环），Ghost 与 witness 晚到至多一个 Scene 心跳（≤ 50ms）；交接 Offer 在目标格是一次 Scene handler，不再排在 tick 后面（v1 缺口 12 反而变好）。

### 7.4 交接以 Nest 事务为边界

- 旧格 O：角色心跳发现越界（迟滞 H）→ 同一笔事务里 `role = Frozen, epoch+1`，Offer 编码进 DAO；**提交后**才发 Offer（AfterCommit）。事务回滚 = 没冻结 = 没交接。冻结期间心跳按 OfferRetry 重传、到 HandoffAbort 放弃（销毁角色，给 Base 发 Lost）。
- 新格 N：Offer → Scene handler：查 `seen`、本格是否已有该 ID 的实体、容量；接受则在**同一笔事务里**经 `EntityManager.Create`（handler 内新建走 `CreateInScope`，由事务捕获：回滚 / 拒绝即撤销发布）建出 SceneActor / Monster 并写入状态（Real，epoch = E），`seen[E] = 接受` 也在这笔事务里（Region 手写 DAO，回滚还原）。**提交后**才回“已接受”、通知 Base。事务失败 → 记 `seen[E] = 拒绝`（另一笔事务）并回拒绝，旧格恢复 Real。所以“交接提交成功才算移交；回滚不移交”。
- O 收到“已接受 E”（或隐式确认：N 发来 epoch ≥ E 的 Ghost）→ 角色 handler 事务里确认冻结的 epoch 仍是 E，标记已移交；提交后销毁实体（`EntityManager.Destroy`，不在业务 worker 上等：交给 Region 的销毁队列，在 Scene 心跳之外的一个 goroutine 上执行，与 demo spawner 同样的“决定在事务里、建 / 毁在 worker 外”），区域在 Ghost 表里留一份最后状态（仍在边界带内时）。
- v1 的协议不变量（先冻结后 Offer、一个 epoch 一个目标、幂等回复、隐式确认、超时只删不恢复）原样保留；变化只是每一步落在哪笔事务里。

### 7.5 视野：witness → Base 的 mirror

Cell 的 Scene 心跳给本格每个玩家 Real 算 witness（半径 R_feed 内的本格 Real + Ghost，含自己），发给该玩家的 Base。Base 收到后在 Player 的 Nest handler 里：只接受 epoch 不回退的 witness，把每个对象 `Mirrors.Apply`（版本 = 对象在 Cell 上的 (epoch, seq)，内容 = 位置 / HP / 种类等客户端可见字段，全量）。客户端的可见集合由 Base 的 Interest AOI 按 mirror 位置决定（玩家主视野跟随自己的 SceneActor mirror），要求 R_feed ≥ Base 的离开半径，对象离开视野之前位置一直在更新；不在任何 witness 里的 mirror 按 TTL 过期删除。交接时旧格、新格的 witness 交错到达，按对象版本取新的，视野里的对象不会 remove 再 create。

### 7.6 Base 侧：租约、回写、重建

- 玩家的格内路由状态（当前 owner sid、epoch、输入序号、攻击序号、最近快照版本）放在 **Player 实体的 nopersist 字段**，Base 的看门狗是 Player 的心跳（不再是 v1 `Base` 里带锁的玩家表）。
- **租约**：Player 心跳每 LeasePeriod 给 owner 格发 Lease（顺带重发落后的输入）；SceneActor 心跳发现 `now > leaseUntil`（LeaseTimeout，缺省 3s）就自行离场销毁。Base 进程消失后 SceneActor 在 LeaseTimeout 内被清理（v1 缺口 6）。
- **回写**：SceneActor 在脱战、离开场景、交接放弃、战斗中每 N 秒（缺省 5s）把战斗字段快照（HP 等）+ 版本 (epoch, seq) 发给 Base；Base 在 Player 的 Nest handler 里只接受比已存版本新的快照，写 Player 的持久字段（正常 DataEngine 落库）。幂等：同一版本重复到达不再写。
- **重建**：Enter 带 Player 的最近快照；Cell 崩溃后 Base 静默看门狗重进时新格按快照建 SceneActor（最多丢一个回写周期的战斗状态）。Monster 由新起的 Cell 进程按刷怪配置重建。

### 7.7 App 级保证

Cell 进程是普通 roost App：`App.Singleton(kitredis 的 SingletonStore)`，`singleton.enabled=true`。同 sid 起第二个 Cell 进程在 startup_wait 内拿不到锁即 `ErrSingletonHeld` 启动失败；失锁 fail-stop。cellworld 不再自己做租约 / 单实例判断（v1 缺口 5）。

### 7.8 game-demo 进程内管理器在分区模式下的处理

| 管理器 | 文件 | 管什么 | 锁 | 与 Nest 事务的关系 | 分区模式 |
| --- | --- | --- | --- | --- | --- |
| Scene 桥 `Scene` | `internal/service/game/scene.go.tmpl` | entitysync Manager + Interest；成员表 `members`（Join 代际、ready、opening）；队伍视图 `teams` | `transitions`（Join / Ready / Leave / settle 串行）、`teamMu`（队伍视图）、`mu`（成员表） | 不在 Nest 锁内持有；`MoveSpace` 把移动作为提交门事实排进 Interest | 一分为二：**(b)** 客户端可见性、成员表、队伍视图属于玩家——留在 **Base** 进程，照旧是这一个桥（Manager 里加 mirror，§7.2）；**(a)** Cell 进程不需要这个桥（Cell 上没有客户端会话），格内可见性由 Region 的索引负责 |
| 队伍视图 | 同上 `SetTeam` / `teams` | 匹配成功的两人互相可见 | `teamMu` | 无 | **(b)** 留在 Base；队友若在别的 Base 进程、或不在本玩家 witness 里，跨格 / 跨进程的队伍可见用 v1.34 的 `GroupView` / `selfsync.Groups` 从 Base 推（本轮原型不接，§8 缺口） |
| `PlayerOwners` | `playerowner.go.tmpl` | 本进程为哪些玩家服务（驻留表）、写准入、闲置卸载 | `mu` | Admit 在 Nest 业务池上做一次加锁读写 | **(b)** 留在 Base 不动；Cell 进程不托管 Player，不需要它 |
| spawner | `spawner.go.tmpl` | 把 Scene 刷怪节拍的决定变成 Monster 实体 | `mu`（monsters 表）、`busy` | 决定在 Scene 心跳事务里，AfterCommit 交给它；建实体在 worker 外 | **(a)** 每个 Cell 进程一份，只刷本格（刷怪表按格过滤：刷怪点在本格矩形内才刷）。跨格游走的 Monster **(c)** 走交接，交接后在新格的 Monster 由新格的 Region 管，spawner 只记本格生出来的；被交接走的从 spawner 表里移除（经 Region 的通知，不加跨进程锁），在别的格死亡时由那个格的 Refresh 记账——**原型简化**：跨格死亡不回原格补刷，§8 缺口 |
| Refresh（刷怪表计数） | `game/scene/runtime/refresh.go.tmpl` | 每组该有几只、已有几只 | `mu` | Scene 心跳读 Due，spawner 报 Spawned / Died | **(a)** 每格一份 |
| Terrain / PathFind | `game/scene/runtime/terrain.go.tmpl`、`pathfind.go.tmpl` | 地图格子与占位、寻路 | `RWMutex` | 心跳内读 | **(a)** 每格一份（同一张地图，每个进程整张加载；占位只对本格 Real 有意义）。跨格寻路只算到本格边界，原型不处理跨格占位 |
| `actor` 包级变量 | `game/actor/actor.go.tmpl` | 心跳驱动、Lookup、WorldTime、Ground | 原子指针 | — | **(a)** 每进程一份，不变 |
| 战斗（lockstep 对战房间） | `battle.go.tmpl`（及 matchmaker） | 匹配成功后的帧同步对战房间 | `mu`、房间 goroutine | 不在 Nest 内 | **不属于三类**：它不是场景内战斗，是独立房间，和分区无关，保持原样（在匹配它的进程里开房） |
| 场景内战斗（攻击 / 伤害） | v1 `cellworld` combat | 攻击者格做前置检查，目标 Real 结算，HitID 去重 | — | — | **(c)** 不加跨进程锁，统一“转发给目标 Real 结算”，去重表与 HP 落在 SceneActor / Monster 的 DAO 上 |

分类后没有发现必须在分区模式下重写的管理器：Scene 桥在 Base 侧原样使用（只多一类 subject），Cell 侧不实例化它；成员进出串行化（`transitions`）只管 Base 上的会话与订阅，交接事务在 Cell 上，两者不交叉。**新增的分区状态全部在 Scene / SceneActor / Monster / Player 实体上**，进程级只增加：Region 的收件 channel 与销毁队列（无锁的单消费者队列，不是带锁的管理器）、Sync 的 `Mirrors`（Sync 框架组件，锁的性质同 Interest 登记侧）。

### 7.9 实施阶段与验收

1. **A. Sync mirror**（`policy/mirror.go`）：单测先对空桩跑红、实现后转绿；全部 `framework/sync/...` 测试 + `-race`。
2. **B. cellworld v2**（框架）：`Region` / `Actor` / `ActorBeat`；v1 的确定性用例按新执行模型改写（断言含义不变）；新增真实 Nest 引擎 + EntityManager + 心跳驱动的进程内多格用例与两 OS 进程用例（NATS 私有端口、Redis 私有端口给单实例锁）；Base 侧 mirror 推给测试客户端（`frame.Receiver` 解帧）检查视野连续。
3. **C. game-demo 分区配置**：Scene 支持分区、Monster 交接、Player 经 SceneActor；缺省不分区，`scripts/test-demo-generated.sh` 等全部通过。
4. 每阶段测试通过即提交并推分支；某一步发现方向问题先停下来写进汇报。

## 8. v2 实测结果与剩余缺口（2026-11-03）

### 8.1 做了什么

| 阶段 | 内容 | 位置 |
| --- | --- | --- |
| A. Sync | `policy.Mirrors[T]`：远端只读对象作为 subject，与本地实体同一 Manager、同一会话流；全量 / 增量 / 删除、按 (epoch, seq) 丢旧、墓碑、TTL 过期、删除后同 ID 重新出现走 remove-before-create。**只组合现有公开 API，不改 Manager、Interest、SubjectSyncState 的任何现有代码** | `framework/sync/entitysync/policy/mirror.go` |
| B. 框架 | `Region`（Scene 持有的手写 DAO，写时登记逆操作）、`ActorState` / `Actor`（角色状态在实体 DAO）、`Node`（每进程端点：消息 → Nest 请求，提交后的发送 / 报告 / 销毁 → 有界出站队列）、`BaseState` / `BasePlayer`（Player 上的路由状态，看门狗 + 续租是 Player 心跳）、witness → Mirrors、SceneActor 租约、回写按版本幂等、Cell 崩溃按 Player 快照重建、线上格式版本号 | `gameplay/cellworld/` |
| C. demo | **未做**，见 §8.5 | — |

### 8.2 用例与结果

macOS（Apple M5）本机。`GOWORK=off go test -race -count=1 ./gameplay/cellworld/ ./framework/sync/...` 全部通过；真实 NATS / Redis 用例要求 PATH 里有 nats-server（2.14.5）/ redis-server（8.10.1），测试自起私有端口、用完杀掉。

| 层 | 用例 | 断言 |
| --- | --- | --- |
| Sync 单测 | mirror 与本地实体同一会话流；丢旧版本（同 epoch 小 seq、相同版本、更大 epoch 小 seq 是新的）；有 Delta 时按 mask 发增量、mask 0 发全量；删除 → remove、墓碑挡迟到版本、同一拍删除又出现；TTL 过期；位置决定可见（走出半径 remove、回来 create）；并发乱序 Apply 收敛到最大版本；Close 全部 remove | 解帧后的客户端操作序列与持有集合。**先对空桩跑：8 条全红**（[修前输出](../performance/benchmarks/cellworld-v2-20261103/mirror-red-before.txt)），实现后转绿；`framework/sync/...` 全部 `-race` 通过 |
| 进程内多“进程”（各自 EntityManager、Nest 引擎、心跳驱动、Node） | v1 的直线、抖动（12 次交接）、斜穿四格、跨界攻击、慢交接（Offer 延迟 300ms）、20% 丢包 + 10% 重复 + 0～30ms 乱序 | v1 的全部断言（witness 连续：缺失 ≤ 1 份、间隔 ≤ 3 拍 + 50ms；至多一个 Real；伤害只结算一次），**新增客户端连续**：Base 进程 Manager 推出的帧解出来，玩家离开半径以内的静止 NPC 从未被 remove，结束时近处的 NPC 与自己的 SceneActor 都在客户端手里。实测 witness 缺失 0、最坏间隔约 82ms |
| 细粒度（v1 handoff / combat 按新模型改写） | 交接留 Ghost、迟滞不交接、丢 3 个 Offer 与 2 个回复且其余全重复、回复全丢靠 Ghost 隐式确认、超时只删不恢复后 Base 重进、被拒恢复并用下一 epoch 重试、回复扣住 400ms 时来回越界、A→B→D 交接链、Enter 幂等与“有存活 Ghost 就拒绝”、Ghost 墓碑；伤害全部消息重复、距离 / 可见性拒绝、去重随交接带走、结果未知、去重表满拒绝 | 同 v1（realChecker 在每次**提交后**的身份变化上检查） |
| 生命周期（新增） | Cell 崩溃后按 Player 快照重建（挨打回写 HP 90 → 崩溃 → 重进后 HP 90、epoch 更大）；Base 消失后 SceneActor 在 LeaseTimeout 内离场、邻格 Ghost 消失；回写全部重复 + 乱序时只接受更新的版本；**交接事务回滚不移交**（目标格安装到最后一步失败 → 新建的实体撤销、seen 还原 → 旧格重传同一 epoch → 第二次安装成功） | 三处变异各让断言变红（确认后不交出、不发 Ghost、区域表不随事务回滚），[记录](../performance/benchmarks/cellworld-v2-20261103/mutation-checks.txt) |
| 真实 NATS | 直线、抖动、斜穿、跨界攻击 | 约定比替身放宽一拍；实测缺失 0、最坏间隔约 82ms |
| 多 OS 进程（每个 Cell 是带单实例锁的 App 子进程，锁在私有 redis-server） | 来回穿越 + 跨界攻击 10 次；**kill -9** Cell 进程后新进程等旧锁过期（TTL 3s）接手，玩家按快照恢复（新 pid、HP 90）；**同 sid 起第二个 Cell 进程**在 startup_wait 内以 `ErrSingletonHeld` 启动失败，第一个照常服务；**Base 进程被 kill -9** 后 SceneActor 约 0.75s 离场（租约 1s） | 按各进程事件时间跨进程至多一个 Real（被 kill 的进程之后不算持有）、epoch 单调；伤害各只在一个进程结算一次 |

其余验证：`GOWORK=off go build ./... && go vet ./...`、glsvet（nest、entity、dataengine/engine、sync/entitysync 含 policy、gameplay/cellworld）无输出、全量 `GOWORK=off go test ./...`、根包 `go test -count=1 .`（含 `TestCoreDependencyBoundary`：core 层测试不 import wiring）、`scripts/test-demo-generated.sh`（私有依赖：`scripts/mirror-local.sh`，偏移 26000，用后 clean）、`test-sync-modes-generated.sh`、`test-mmorpg-generated.sh`、`check-doc-anchors.py` 全部通过。demo / codegen 本轮没有改动，生成工程的通过只说明“普通模式不回归”，不说明分区模式在 demo 里可用。

### 8.3 测量：与 v1 同负载对照

同一负载（2×2 网格、每格一个 OS 进程、单机 nats-server、每格 N 个 3 m/s 随机游走 NPC、40 个随机游走玩家、20s 窗口、两轮），[原始输出与环境](../performance/benchmarks/cellworld-v2-20261103/README.md)。表里是两轮、四格的范围（v1 数字取自 §6.2）：

| 每格 NPC | 版本 | 每格 CPU（一个核） | 每拍耗时 p50 / p99 | 每格出 | Ghost 延迟 p50 / p99 | 交接延迟 p50 / p99 | 视野（witness）延迟 p50 / p99 |
| ---: | --- | ---: | --- | --- | --- | --- | --- |
| 1k | v1 | 4.3～6.3% | 0.9～1.6 / 2.5～4.6ms（整拍） | 230～370 msg/s、310～400 KB/s | 1.5～1.9 / 3.8～5.9ms | 1.6～2.3 / 2.8～14.8ms | 0.22～0.28 / 1.1～1.2ms |
| 1k | v2 | **31.1～37.0%** | 0.34～0.48 / 1.1～2.2ms（只是区域心跳；角色心跳另计） | 1000～1500 msg/s、425～580 KB/s | 0.25～0.30 / 1.1～1.5ms | 0.53～0.70 / 1.5～3.5ms | 0.34～0.43 / 1.5ms |
| 5k | v1 | 13.4～17.1% | 3.7～5.1 / 5.9～9.8ms（整拍） | 295～438 msg/s、1.4～1.7 MB/s | 2.6～3.5 / 6.0～6.7ms | 4.5～6.0 / 6.3～11.1ms | 0.15 / 0.74～0.77ms |
| 5k | v2 | **76.3～82.0%** | 0.63～0.80 / 2.0～3.4ms（同上） | 1800～2200 msg/s、1.7～2.4 MB/s | 0.27～0.37 / 1.0～2.0ms | 0.61～1.01 / 1.7～3.0ms | 0.26～0.28 / 0.73～0.97ms |

两档、两轮都没有被拒、放弃、重传、出站队列丢弃、Nest 准入失败或 Real 冲突。Base 进程（父进程，含 Sync mirror 与客户端解帧）CPU 11～15%。

**新执行模型的代价**：

- **CPU 约 5 倍**（1k：5% → 33%；5k：15% → 80%）。主要是“每个实体每拍一笔 Nest 事务”本身：探针（[记录](../performance/benchmarks/cellworld-v2-20261103/probe-heartbeat-cost.txt)）里回调什么都不做的 50ms 心跳，1k 个实体就是 15%、5k 个 61% 的一个核；其余是报告批次与 Ghost 编码。按这个量级，5k 个 20Hz 的 NPC 一个格要接近一个核，v1 的单 goroutine 循环只要 1/6。
- **延迟反而更好**：交接 Offer 在目标格是一次立即执行的 Scene handler，不再排在 tick 后面（v1 缺口 12），5k 时 p50 从约 5ms 降到约 0.7ms；Ghost 由出站 goroutine 在队列排空时攒批发出，不等整拍结束，p50 从 2.6～3.5ms 降到约 0.3ms。
- **消息数约 4～5 倍**：Ghost 按报告批次（而不是按拍）发出，批次小、条数多；字节数接近（5k 时 1.7～2.4 MB/s 对 1.4～1.7 MB/s）。Ghost 仍是每拍全量，没有增量 / LOD（v1 缺口 11 未变）。
- 同机数据，跨机网络延迟、长稳、10k 以上没有测（v1 缺口 16 未变）。

### 8.4 v1 缺口的现状与 v2 新缺口

| # | 缺口 | v2 状态 | 性质 |
| --- | --- | --- | --- |
| v1-1 | Cell 不在 Nest 上 | **已解决**：区域表在 Scene、角色状态在实体 DAO、心跳驱动、交接以事务为边界 | — |
| v1-2 | 视野只到 Base、没到客户端 | **框架层已解决**：witness → `policy.Mirrors` → Base 的 Manager → 客户端（测试客户端解帧验证）；demo 未接 | 上线前必须解决（demo / 正式客户端格式） |
| v1-3 | 战斗状态不回写 | **部分解决**：HP 与位置按版本回写（定时、离场、租约到期），重建用快照；Buff / 冷却原型里没有建模 | 上线前必须解决（字段清单与回写时机由产品定） |
| v1-4 | NPC 崩溃丢失、无重建来源 | **未解决**：框架有 `Node.Spawn`，“新 Cell 进程按刷怪配置重建”要 demo 的每格 spawner（§7.8），没做 | 上线前必须解决 |
| v1-5 | Cell 没有单实例保证 | **已解决**：Cell 是带单实例锁的 App（多进程用例验证同 sid 第二进程被拒、kill -9 后新进程等锁接手） | — |
| v1-6 | 玩家 Real 没有租约 | **已解决**：SceneActor 租约（Base 每 LeasePeriod 续租，LeaseTimeout 离场） | — |
| v1-7 | 重进防重复只有“存活 Ghost 就拒绝”一道 | 未变 | 上线前必须解决 |
| v1-8 | 结算方不复核距离；没有鉴权 | 未变 | 上线前必须解决 |
| v1-9 | 去重窗口按系统时间 | 未变 | 上线前必须解决（跨机前） |
| v1-10 | 线上格式没有版本号 | **已解决**：`WireVersion = 2`，不是当前版本的消息按坏消息丢弃 | — |
| v1-11 | Ghost 每拍全量 | 未变；v2 消息条数更多（按报告批次发） | 原型简化（容量阶段） |
| v1-12 | 交接延迟随目标格每拍耗时增长 | **已改善**（见 8.3） | — |
| v1-13 | 静态分区 | 未变 | 原型简化 |
| v1-14 | 攻击只能打本格视野里的目标 | 未变 | 原型简化 |
| v1-15 | Leave 尽力而为；配置不经 `app.LoadConfig` | Leave 有租约兜底；配置仍是 `NodeConfig` 结构体，没有 Mod 配置声明 | 原型简化（接 demo 时按 A4① 声明） |
| v1-16 | 单机、短窗口测量 | 未变 | 原型简化 |
| v2-1 | **每实体每拍一笔 Nest 事务的 CPU 代价**（8.3：约 5 倍） | 新 | **上线前必须解决或接受**：见 8.6 D1 |
| v2-2 | witness 的空间索引每拍从区域表现建（A1：索引不常驻），O(本格 + Ghost) | 新 | 原型简化（5k 时区域心跳 p99 ≤ 3.4ms） |
| v2-3 | Base 喂 mirror 在 Player handler 里直接调 Sync（不随事务回滚，靠按版本幂等）；handler 回滚时 mirror 可能比 BaseState 新一份 witness | 新 | 原型简化（无害：下一份 witness 覆盖） |
| v2-4 | 客户端视野依赖 R_feed（witness 半径）≥ Base Interest 的离开半径 + 余量；超出 witness 半径的 mirror 停在最后位置直到 TTL 过期 | 新 | 定案前提，接 demo 时在配置校验里强制 |
| v2-5 | 实体销毁在 Node 出站 goroutine 上执行（等实体锁时出站队列暂停） | 新 | 原型简化 |
| v2-6 | 跨格 / 跨进程的队伍可见（GroupView / selfsync.Groups 从 Base 推）没接 | 新 | 上线前必须解决（§7.8 (b)） |
| v2-7 | 一个进程既是 Cell 又是 Base（自己格里的玩家走本机回环）只在进程内用例覆盖，多进程用例里 Base 都是单独进程 | 新 | 原型简化 |
| v2-8 | 区域表是手写 DAO（undoMap），没有生成 DAO 的 schema / 同步能力 | 新 | 需要维护者确认（8.6 D3） |

### 8.5 没做的：demo 分区配置（阶段 C）

没有改 `demo/`：Scene 的分区配置、Monster 交接、Player 经 SceneActor 参与都还只在框架层（`gameplay/cellworld` 的测试实体）验证过。原因：

1. 接 demo 需要新的生成实体 SceneActor（DAO、lifecycle）、Monster / Player / Scene 各加一组 nopersist 字段与心跳、分区配置按 A4① 声明、Scene 桥在分区模式下改走 `Node.Enter / Move`、mirror 的打包要产出 demo 客户端认识的 `player` / `monster` 载荷，再重新生成 game-demo 跑全部生成工程门禁——量级与本轮框架部分相当。
2. 其中 Monster 是否每只一个 20Hz 的格内心跳，取决于 8.6 D1 的决定（5k 只 NPC 约一个核）；先接 demo 再改执行方式会返工。
3. SceneActor 在客户端是什么（8.6 D2）也要先定。

接 demo 的入口已经备好：`NodeConfig.CreateActor` 接生成 lifecycle 的 `Create`（handler 内即 `CreateInScope`）；`Actor` / `RegionHost` / `BasePlayer` 三个接口就是 SceneActor / Monster、Scene、Player 要实现的全部方法；§7.8 的管理器分类不需要重写任何现有管理器。

### 8.6 需要维护者决定

- **D1 每实体心跳的 CPU 代价**（v2-1）。A：接受（每格 5k 个 20Hz NPC 约一个核）。B（推荐）：NPC 格内心跳按视野分档——区域表知道哪些 NPC 附近有玩家，没人看的 NPC 降到 200ms / 1s 档（heartbeat 已有档位与 SetPeriod），估计降到 1/3～1/5，代价是远处 NPC 的移动更粗、交接判断晚几拍。C：NPC 移动由 Scene 心跳批量积分，Monster 只保留战斗字段与事务（偏离“角色由自己的心跳驱动”，但 CPU 回到 v1 量级）。
- **D2 SceneActor 在客户端的样子**。A（推荐）：mirror 打成 demo 现有 `player` 命名空间与 Player 的同步编码（位置、HP 字段），客户端不变；B：新的 `actor` 命名空间与编码，客户端区分 Player 与 SceneActor。
- **D3 区域表的形式**（v2-8）。A（本轮）：框架内手写 DAO（undoMap 登记逆操作，与 `skill/combatcomponent.CombatDao` 同形）；B：由 Scene 的生成 DAO 承载（map 字段 + 生成 setter），可同步、可诊断，但 Ghost 表每拍大量写入会进 DAO 的脏标与 undo 记录。
- **D4** 合入 main 时 `policy.Mirrors` 是否作为正式 API 写进 impl/04（本轮只在分支文档里）。

### 8.7 方向判断

- 维护者的两个决定（Cell 是 Scene 的模式、Cell 部分是 Nest 实体）在框架层落地后，**协议本身没有变复杂**：交接、伤害、重进的规则与 v1 相同，只是每一步落在哪笔事务里；“交接提交成功才算移交”由 Nest 的 handler 内新建 + 回滚撤销直接给出，只多了区域表的逆操作登记（变异 3 证明它是必要的）。Sync 侧不需要改 Manager / Interest：subject 本来就是不含观察者的内容状态。
- 真正的新问题是 **v2-1 的 CPU 代价**：它不是实现细节，而是“每个移动实体一个 20Hz Nest 事务”这个前提的成本，NPC 数量越大越明显。是否进入 demo 与正式方案，建议先定 D1。
- 其余上线前缺口（v1-4/7/8/9、v2-6）与 v1 的判断相同，都不在交接协议里，而在 NPC 重建、重进防重复、鉴权、跨机时钟与队伍可见这些外围。

## 9. v2.1：D1 降档与 demo 接入（2026-11-03）

维护者 2026-11-03 对 §8.6 的决定：**D1 = B**（附近没有玩家的 NPC 心跳降到 200ms 或 1s，有玩家靠近回到 50ms）；**D2 = A**（SceneActor 在客户端沿用 `player` 命名空间与编码）；**D3 = A**（区域表保持框架内手写 DAO + 逆操作登记）；**D4** 等决定把无缝大世界并入 main 时再定，本轮 `policy.Mirrors` 只在分支文档里描述。本节是 D1 的设计、用例与测量，以及 demo 接入为什么停在设计层（§9.5）。

### 9.1 D1 设计

代码在 `gameplay/cellworld/tier.go`。只降格内移动心跳（`cell.actor`）的档位，**不暂停 AI**，与[无观察者暂停 AI 设计稿](FEATURE-2026-10-22-ai-idle-without-observers.md)是两件事：那份稿子停的是 AI 决策（扫描者读 AOI 观察者计数、Nest 广播暂停、实体心跳自己恢复），本项不停任何逻辑，只让没人看的 NPC 少跳几拍；两者将来可以叠加（AI 停了、移动心跳也在慢档），本轮不实现那份设计稿。

| 项 | 规则 | 为什么 |
| --- | --- | --- |
| 配置 | `NodeConfig.IdlePeriod`（heartbeat 档位、长于且整除 `Period`；零值不降档）、`WakeRadius` W（零值 = ViewRadius；校验 `ViewRadius ≤ W` 且 `W + H ≤ G`）、`IdleAfter`（零值 2s） | 零值保持 v2 行为；W ≥ R 让“看得见”一定先“被唤醒” |
| 观察者 | 本格玩家 Real / Frozen + 邻格玩家的 Ghost | NPC 至多越出本格 H，W 内的邻格玩家离本格至多 W + H ≤ G，一定在本格有 Ghost：只看区域表就够，不问邻格 |
| “附近有人” | 观察者离 NPC **报告位置** ≤ W + drift，drift = (\|VX\| + \|VY\|) × NPC 当前档位周期 | 慢档 NPC 一个慢拍才报告一次，报告位置最多陈旧一个慢拍的位移 |
| 升档 | 有人在附近且 NPC 在慢档 → 立即请求升档 | 延迟 = 一个区域心跳 + 出站队列 + 一次 NPC handler，之后下一拍按快档触发 |
| 降档 | 快档 NPC 连续 `IdleAfter` 附近没人 → 请求降档 | 时间滞后：玩家在 W 边上来回时只升一次、不降，离开后只降一次 |
| 谁执行 | 区域心跳（Scene 锁内）决定，请求经出站队列投给 NPC 自己的 `cellworld_tier` handler（只锁 NPC，锁序不变）；handler 幂等（档位已是目标值不动）、版本前进、提交后报告区域；请求丢了 `5 × Period` 后重发 | 区域不能锁没声明的 NPC；区域表只在状态变化时写（“无人起点”“上次请求”两个时间），没人在附近、已慢档的 NPC 每拍不产生写 |
| 档位存哪 | NPC 的 DAO：`EntityState.Idle`（随事务回滚），换档 `Actor.SetCellPeriod`（`heartbeat.Reg.SetPeriod`）在同一事务里、提交后生效 | A1：事务内会改的状态在 DAO |
| 慢档越界 | 一拍把慢档周期按 `Period` 分步积分，**每步判交接**，越界就在那一步冻结（`advance`） | 冻结点离本格至多 H + 一步，与快档相同：不会一步跨过边界带，旧格留 Ghost、新格的接受判断与快档一样 |
| 冻结中 | 一律快档（`beatPeriod`：只有慢档的 NPC **Real** 用 IdlePeriod），`Idle` 标记保留 | 重传（OfferRetry 100ms）与放弃（HandoffAbort 3s）的计时不因降档变粗 |
| 交接 | Offer 带着 `Idle`，新格以慢档装入（新建实体还没启动时 `SetCellPeriod` 只记档位，`StartCell` 按它启动）；被拒恢复为 Real 时回到慢档 | 不因交接回到快档再等一次 IdleAfter；新格的区域下一拍按自己的观察者决定要不要升档 |
| Ghost | 慢档 NPC 的 Ghost 过期放宽为 `GhostTTL + IdlePeriod`（按接收方配置，各进程读同一份分区配置） | 慢档长于 GhostTTL 时不放宽，Ghost 必然在两次更新之间被删掉（变异 M1） |
| 不变的 | 伤害结算（去重表在 NPC DAO、随交接带走）、交接协议、epoch / seq、玩家 SceneActor 恒为快档 | 慢档只改心跳频率，不改任何协议步骤 |

线上格式 v3（`EntityState.Idle`）；v2 进程的消息按坏消息丢弃，升级需先停旧进程（未上线，不做兼容）。

### 9.2 用例与先红后绿

`gameplay/cellworld/tier_test.go`，用例的慢档取 1s、降档滞后 300ms。**先对空桩跑**（配置字段、`Actor.SetCellPeriod`、计数已加，降档逻辑未实现）：6 条全红（[修前输出](../performance/benchmarks/cellworld-v21-20261103/d1-red-before.txt)），实现后转绿；再逐一放回 5 个错误，各让对应用例变红（[变异记录](../performance/benchmarks/cellworld-v21-20261103/d1-mutation-checks.txt)）。

| 用例 | 断言 | 变异 |
| --- | --- | --- |
| 无人时降档、邻格玩家靠近升档、走远再降 | 无人时 2s 内心跳 ≤ 4 拍；玩家仍是 B 的 Real（A 只有他的 Ghost）走到离 NPC 700 时 NPC 已升档、1s ≥ 15 拍；走远后滞后之后降档 | M5 观察者不算 Ghost → 永不升档 |
| W 边上来回不抖动 | 玩家在 W−30 与 W+50 之间每 200ms 换向 4s：升档 1 次、降档 0 次；离开后共 1 升 1 降 | M4 去掉滞后 → 6 升 6 降 |
| 慢档 Ghost 不过期 | GhostTTL 400ms < 慢档 1s：3s 内每 25ms 采样，邻格的 Ghost 一次都不缺 | M1 不放宽 → 54 次缺失 |
| 慢档冲过边界 | 慢档 NPC 一拍 800：冻结点（第一份 Offer 的位置）≤ 边界 + H + 一步；旧格留 owner = 新格的 Ghost；新格以慢档装入；全程至多一个 Real | M2 整拍积分后再判 → 冻结在 10520；M3 冻结时丢掉 Idle → 新格快档装入 |
| 慢档 NPC 被跨界攻击 | 发给 NPC 所在格的 Ghost 扣住 600ms（NPC 在头几次伤害到达时仍在慢档，用例断言第一次结算早于升档），同时它慢档走过边界、20% 丢失 / 10% 重复 / 乱序：25 次攻击每次只结算一次，最终 HP = 100 − 2 × 结算数 | — |
| `advance` 单元 | 慢档一拍走满 1s 的位移；一拍 1500（> G）冲过边界时冻结点在 [10100, 10175] | M2 → 冻结在 11000 |

其余：`GOWORK=off go test -race -count=1 ./gameplay/cellworld/` 全部通过（含 v2 的全部用例、真实 NATS、多 OS 进程），`-count=2 -run TestIdle` 通过。用例第一版的“慢档冲过边界”在旧格 Inspect 里找 Ghost，与新格第一个慢拍的删除 / 更新赛跑，8 次误报 2～3 次，改为截第一份 Offer 的位置（变异 M2 / M3 在改后仍各让它变红）。

### 9.3 测量：v1 / v2 / v2+D1

同一负载（2×2 网格、每格一个 OS 进程、单机 nats-server、每格 N 个 3 m/s 随机游走 NPC、40 个 5 m/s 随机游走玩家、20s 窗口）；v2 与 v2+D1 同机交替各两轮（D1：IdlePeriod 1s、W = R = 2500、IdleAfter 2s），[原始输出与环境](../performance/benchmarks/cellworld-v21-20261103/README.md)。v1 数字取自 §6.2，v2 列是本轮重测（与 §8.3 同量级：1k 时本轮 26～31%、§8.3 31～37%，机器后台负载不同）。表里是两轮、四格的范围：

| 每格 NPC | 版本 | 每格 CPU（一个核） | 角色心跳 / s | 慢档 NPC 占比 | 区域心跳 p50 / p99 | 每格出 | Ghost 延迟 p50 / p99 | 交接延迟 p50 / p99 | witness 延迟 p50 / p99 |
| ---: | --- | ---: | ---: | ---: | --- | --- | --- | --- | --- |
| 1k | v1 | 4.3～6.3% | —（单 goroutine 循环） | — | 0.9～1.6 / 2.5～4.6ms（整拍） | 230～370 msg/s | 1.5～1.9 / 3.8～5.9ms | 1.6～2.3 / 2.8～14.8ms | 0.22～0.28 / 1.1～1.2ms |
| 1k | v2 | 25.8～31.4% | 19.8k～20.4k | 0 | 0.40～0.56 / 1.9～3.3ms | 780～1400 msg/s、390～560 KB/s | 0.31～0.52 / 1.4～2.4ms | 0.59～1.30 / 1.8～15.1ms | 0.51～0.53 / 2.0～2.2ms |
| 1k | **v2+D1** | **9.6～15.2%** | 2.8k～4.5k | 84～91% | 0.59～0.74 / 1.7～2.8ms | 300～740 msg/s、180～380 KB/s | 0.21～0.24 / 0.9～1.6ms | 0.43～0.58 / 1.0～6.7ms | 0.30 / 1.2～1.6ms |
| 5k | v1 | 13.4～17.1% | — | — | 3.7～5.1 / 5.9～9.8ms（整拍） | 295～438 msg/s | 2.6～3.5 / 6.0～6.7ms | 4.5～6.0 / 6.3～11.1ms | 0.15 / 0.74～0.77ms |
| 5k | v2 | 76.1～82.1% | 99.7k～100.8k | 0 | 0.75～0.99 / 2.6～4.1ms | 1970～2370 msg/s、1.7～2.4 MB/s | 0.28～0.40 / 1.3～2.4ms | 0.70～1.14 / 2.0～18.0ms | 0.27～0.34 / 1.1～1.3ms |
| 5k | **v2+D1** | **20.9～33.3%** | 12.9k～21.4k | 85～92% | 1.12～2.06 / 2.9～7.8ms | 730～1460 msg/s、0.7～1.5 MB/s | 0.21～0.32 / 1.4～2.5ms | 0.46～0.74 / 2.8～14.6ms | 0.31～0.32 / 1.1～4.4ms |

两种版本、两档、两轮都没有被拒、放弃、重传、出站队列丢弃、Nest 准入失败或 Real 冲突。Base 进程 CPU：v2 10.6～15.1%，v2+D1 11.2～17.7%（mirror 与客户端解帧不受 D1 影响）。

- **CPU 降到 v2 的约 1/3**（1k：约 28% → 约 12%；5k：约 79% → 约 26%），仍是 v1 的约 2 倍。角色心跳从每 NPC 20 次 / s 降到约 3～4 次 / s：玩家随机散布时每格 84～92% 的 NPC 在慢档；剩下的 CPU 里，被看着的 8～16% 的 NPC 仍是 20Hz 事务，其余是慢档心跳、区域心跳（多了档位判断）、换档事务与 Ghost / 报告。
- **换档事务**：玩家走动时 NPC 不断进出 W（每个玩家每秒扫过约 2 × W × v 的面积），每格每 20s 约 0.4k～0.75k（1k）/ 1.8k～3.7k（5k）次换档，每次一笔只锁 NPC 的事务加一份报告；同一个 NPC 在 W 边上不来回换档（IdleAfter 滞后，§9.2 用例）。
- **区域心跳变长**：5k 时 p50 0.75～0.99ms → 1.1～2.1ms（第一版每个 NPC 查观察者桶时是 2.1～2.4ms，改为与 witness 共用一拍现建的索引后降下来，[第一版输出](../performance/benchmarks/cellworld-v21-20261103/measure-v21-first-attempt.txt)）。
- 延迟没有变差：Ghost、交接、witness 的 p50 与 v2 同档或略低（消息量少了一半以上）；交接 p99 的长尾两种版本都有（单机 nats-server 与 CPU 争用）。

### 9.4 玩家分布对降档的影响

同一负载，40 个玩家全部在格 (0,0) 内出生并游走（`ROOST_CELLWORLD_PERF_PLAYERS=cell`），D1 两轮、v2 一轮：

| 每格 NPC | 版本 | 玩家所在格 (0,0) | 没有玩家的格（如 (1,1)） |
| ---: | --- | --- | --- |
| 1k | v2 | CPU 37.5%，心跳 2.0 万 / s | CPU 33.3～34.1%（v2 不看玩家分布） |
| 1k | v2+D1 | CPU 22.1～22.8%，慢档 63%，心跳 8.1k / s，区域心跳 p50 1.1～1.3ms | CPU 6.6～9.3%，慢档 100%，心跳约 1.0k / s |
| 5k | v2 | CPU 82.4%，心跳 10.1 万 / s | CPU 73.9～75.4% |
| 5k | v2+D1 | CPU 48.1～58.6%，慢档 67%，心跳 3.7 万 / s，区域心跳 p50 3.4～3.8ms、p99 5.5～19.9ms | CPU 13.3～18.7%，慢档 100%，心跳约 5.2k / s |

- 降多少取决于**被看着的 NPC 占比**，不是玩家数：同样 40 个玩家，散开时每格只有约 10% 的 NPC 在某个玩家的 W 内；集中在一格时那一格约 1/3 的 NPC 被看着，那一格的 CPU 只降到 v2 的 60～70%，而另外三格几乎全在慢档（空格 5k NPC 约 15% 的一个核：慢档心跳 + 区域心跳的遍历）。
- 玩家集中的格，区域心跳变成最贵的部分（40 个观察者各一份 witness + 档位判断，5k 时 p50 约 3.6ms、一轮 p99 19.9ms）：Scene 锁持有变长，同一格的 Offer / Ghost handler 排在它后面。玩家越集中，越接近 §8.6 D1 的 C（Scene 心跳批量积分）要解决的场景。
- 同一个格从“没人”到“热点”的切换代价是成批的升档事务（与玩家走进来的速度成正比）；本轮测量里没有观察到准入失败或出站队列丢弃。

### 9.5 demo 接入：停在设计层（方向问题）

本轮要求的第 2、3 项（demo 接入分区 Scene、两进程 game-demo 端到端）**没有实施**。逐项读 demo 源码后发现，接入的难点不在 §7.8 分类的那些进程级管理器（它们确实不需要重写），而在 **Monster / Player 在场景里的状态与协议**：cellworld 的交接只带 `ActorState`（位置、速度、HP、去重表、输入序号），demo 的场景玩法建在本进程实体的组件上，有几项产品选择要先定，先接 demo 再定会返工。按“发现方向问题先停下来”的要求，列出问题、证据与选项：

| # | 问题 | 证据（demo 源码） | 影响 |
| --- | --- | --- | --- |
| P1 | Monster 的状态不只是位置与 HP：AI（`ai.Controller`，500ms 心跳）、动作（`actionflow.Behavior` 的 MoveTo，200ms）、移动（`movement.Move` / `MapBasic`，写 MonsterDao + 排 AOI）、状态位（`state.Set`，Rooted）、Buff（`combatcomponent.CombatComponent`，自己的 DAO 与 1Hz 心跳） | `demo/game/entities/monster/actor_component.go.tmpl` 类型注释与字段；巡逻范围 `PatrolHalf = 12`（围着出生点） | 跨格交接要么带走这些组件状态（cellworld 的 Offer 现在带不了），要么分区场景里的 Monster 不用这些组件 |
| P2 | 玩家移动协议是“站到一个点”（`Move`，同步返回服务器落定的位置）与服务器寻路（`MoveTarget`，Player 心跳推进）；cellworld 是速度意图（`Node.Move(vx, vy)`），权威在别的进程 | `demo/protocol/def/move.go.tmpl`、`move_target.go.tmpl` 的注释：客户端渲染响应里的位置 | 分区模式下 Base 不能同步答出“落定的位置”（要么在 handler 里跨进程等待——快池阻塞，违反契约；要么改响应语义） |
| P3 | demo 没有玩家打怪的协议；场景内战斗是技能系统（`gameplay/skill`，Runtime 不在事务里，B4），只作用于本进程实体 | `demo/protocol/def/` 下没有攻击消息；cellworld 的跨格交互只有“伤害数值” | 分区模式的“跨界打怪”需要新协议，技能 / Buff 跨格不在 cellworld 的能力里 |
| P4 | SceneActor 的 DAO 要装 `cellworld.ActorState`（含 `[]AppliedHit`、`*HandoffState`）；生成 DAO 的嵌套类型必须在 `db/def` 声明、每个字段带稳定 `store` 键 | AGENTS.md DAO 规则；`ActorState` 是框架包里的类型 | 要么在 `db/def` 镜像一份带 store 键的结构（生成 setter、逆操作），要么存成一个 msgpack 字节字段（每拍编解码一次） |
| P5 | demo 地图 1000×1000、Terrain 占位与寻路按整张图；Monster ID 由 `runtimeid` 按 sid 编码，Refresh 按 Scene 记账 | `demo/game/scene/scene.go.tmpl` `DefaultConfig`；`spawner.go.tmpl` | 分区要更大的地图与按格的刷怪表；跨格走掉 / 死在别的格的 Monster 不回原格补刷（§7.8 已记为原型简化） |

需要维护者决定的（D5～D8，推荐项标明）：

- **D5 分区场景里 Monster 的形态**（P1）。**A（推荐，原型）**：分区场景的 Monster 是 cellworld 的格内行者——只有 `cell.actor` 心跳（速度巡逻、碰边界反弹 / 回头），不启动 AI / actionflow / Buff；GM 的 root 命令在分区场景不可用。改动在 demo 的 Monster（按配置二选一启动组件）与 spawner（`Node.Spawn`）。B：保留 demo 的 AI / 移动 / Buff，cellworld 增加交接负载（`Actor` 增加 `HandoffPayload() / RestoreHandoff`，Offer 带游戏字节；Monster 把 MonsterDao + CombatDao + AI / 动作进度序列化进去）——框架与生成器都要动（DAO 需要“交接快照”编解码），AI / actionflow 的运行时状态是否全在 DAO 里还要逐个确认，量级接近再做一轮框架。C：Monster 不跨格（出生点离边界 ≥ 巡逻半径 + H，永远不交接），“Monster 跨界交接”只在框架用例里验证。
- **D6 分区场景的玩家移动**（P2）。**A（推荐）**：新增速度意图协议 `MoveIntent(vx, vy)` → `Node.Move`；分区场景里 `Move` / `MoveTarget` 返回“分区场景不支持”的错误码；位置只从同步来（SceneActor 的 `player` 编码，D2）。B：cellworld 增加“走向一个点”的输入（Cell 积分到目标点停下），`Move` 立即回 Base 所知的最后位置（响应语义从“落定位置”变成“当前所知位置”），`MoveTarget` 的寻路只在本格内有效。
- **D7 分区场景的攻击**（P3）。**A（推荐）**：新增 `AttackTarget(target)` 协议 → `Node.Attack`（固定伤害），响应只说“已发出 / 被拒”，结论（含结果未知）作为推送或日志，HP 变化从同步看；技能 / Buff 跨格不做。B：先不做攻击协议，demo 端到端只验证走动与视野，跨界伤害只在框架用例验证。
- **D8 SceneActor 的 DAO 形状**（P4）。**A（推荐）**：`db/def` 里声明 `SceneActorDao`（`nocoll`、全部 `nopersist,nosync`，位置 / 速度 / HP / 角色 / 版本 / 输入序号 / 租约 / 回写时间是标量字段，去重表与交接进度是带 store 键的嵌套类型），`CellState / SetCellState` 在实体里做字段映射——生成 setter 自带逆操作，事务回滚不另写代码。B：一个 `[]byte` 字段存 msgpack 的 `ActorState`（实现最少，但每拍一次编解码、DAO 诊断里看不到字段）。同一选择也适用于 Monster 的格内状态与 Player 的 `BaseState`。

定了 D5～D8 之后的接入顺序（估算与本轮框架部分相当）：分区配置按 A4① 声明（`scene.partition`：网格、owner 映射、视野、慢档；缺省关闭，`ValidateConfig` 里强制 `R + H ≤ G`、`W + H ≤ G`、Base 离开半径 ≤ R）→ SceneActor 生成实体（`roost add entity` / `//roost:entity noPersist=true lifetime=ephemeral`，DAO 按 D8）→ Scene 在分区模式持有 `Region` 与 `scene.cell` 心跳、spawner 只刷本格并走 `Node.Spawn` → Player 加 `BaseState` 与 `cell.base` 心跳 → Scene 桥分区分支（Join → `Node.Enter`、新协议 → `Node.Move` / `Node.Attack`，Manager 加 `policy.Mirrors`，SceneActor 打 `player` 编码、Monster 打 `monster` 编码）→ 重新生成 game-demo 跑 build / vet / test 与 `go generate` 后 porcelain 干净 → 两进程端到端（私有依赖，robot 加分区场景脚本：直线、抖动、斜穿、跨界打怪、正常停与 kill -9 后重进按快照恢复）。

### 9.6 缺口更新

§8.4 的表照旧有效，变化与新增如下：

| # | 缺口 | v2.1 状态 | 性质 |
| --- | --- | --- | --- |
| v2-1 | 每实体每拍一笔 Nest 事务的 CPU 代价 | **部分解决**（D1）：玩家散布时每格 CPU 约降到 v2 的 1/3（§9.3）；降多少取决于玩家分布，被玩家看着的 NPC 照旧是 20Hz 事务（集中在一格时那一格仍贵） | 上线前要按真实玩家分布复测 |
| v1-4 | NPC 崩溃丢失、无重建来源 | 未变（要 demo 的每格 spawner，§9.5 未接） | 上线前必须解决 |
| v1-15 | 配置不经 `app.LoadConfig` | 未变；D1 又多了三个 `NodeConfig` 字段（IdlePeriod / WakeRadius / IdleAfter） | 接 demo 时按 A4① 声明 |
| v2.1-1 | 换档那一拍的位移不准：积分步长取档位周期，换档后的第一拍可能早于或晚于一个周期到来，NPC 少走或多走至多一个慢档周期的路 | 新 | 原型简化（只发生在附近没人、或刚有人进入 W 而还没进入客户端视野时；可改为按心跳的 `Beat.Elapsed` 分步） |
| v2.1-2 | 升档期间客户端可能看到慢档的陈旧位置：升档延迟约一个区域心跳 + 一次 NPC handler + 下一个快拍（≤ 约 2 × Period），在这之前 witness 里是 NPC 最后一次慢拍的位置 | 新 | 定案前提：W − 客户端可见半径 ≥ 接近速度 × 升档延迟（缺省 W = R，R 大于 Base 的进入半径；配置校验不检查速度） |
| v2.1-3 | 邻格玩家的唤醒保证要 `W + H + 最大 NPC 速度 × IdlePeriod ≤ G`，配置只校验 `W + H ≤ G`（不知道 NPC 速度）；超出时 W 边缘的邻格玩家可能不在本格的 Ghost 里，NPC 晚一点升档（不影响正确性） | 新 | 原型简化（接 demo 时在分区配置里声明 NPC 最大速度并校验） |
| v2.1-4 | 换档请求成批：启动或玩家离开后大量 NPC 同一拍到点降档，一拍投出同样多的 Nest 请求（没有每拍上限）；玩家随机走动时每格每 20s 约 0.4k～3.7k 次换档（§9.3） | 新 | 原型简化（必要时给降档加每拍上限；升档不限） |
| v2.1-5 | 区域心跳多了一次档位判断：有观察者时与 witness 共用一份现建索引，玩家散布、5k NPC 时区域心跳 p50 从 0.75～0.99ms 到 1.1～2.1ms；玩家集中的格 3.4～3.8ms（§9.4） | 新 | 原型简化 |

### 9.7 需要维护者决定

- **D5～D8**（§9.5）：分区场景里 Monster 的形态、玩家移动协议、攻击协议、SceneActor 的 DAO 形状。推荐 D5 = A、D6 = A、D7 = A、D8 = A；定了之后按 §9.5 的顺序接 demo 与两进程端到端。
- **D1 的缺省档位**：本轮实现与测量用 1s（`IdlePeriod` 零值是“不降档”，接入方显式打开）。200ms 档的 CPU 介于两者之间、远处 NPC 的移动更细；要不要在 demo 的分区配置里缺省 1s，随 D5 一起定。
- D4 照旧等并入 main 时定。

### 9.8 方向判断

- D1 没有改交接协议：慢档只改心跳频率，“分步积分 + 冻结中快档 + 档位随交接走 + Ghost 过期放宽”四条把慢档对交接、Ghost、伤害的影响收在 `tier.go` 一个文件里，变异检查逐条证明它们各自是必要的。按测量，D1 把 v2 的主要代价（每实体 20Hz 事务）降到与“被看着的 NPC 数”成正比，这是 BigWorld 一类方案通常的做法；剩下的成本在被看着的 NPC 上，若真实负载里大部分 NPC 都在玩家附近（例如攻城），D1 帮不上，届时要回到 §8.6 的 C（Scene 心跳批量积分）。
- demo 接入停下来的原因不是 cellworld 的协议，而是“交接带走什么”：v2 的交接只带 `ActorState`，demo 的 Monster 把 AI、动作、Buff 放在各自的组件与 DAO 里，玩家移动与技能也是本进程同步语义。这是把无缝大世界做成正式能力之前必须回答的设计问题（D5 的 B 就是它的完整答案，量级接近再做一轮框架）；原型阶段推荐 D5～D7 的 A，用格内行者 + 速度意图 + 固定伤害先把 demo 端到端跑通。

## 10. v2.2：demo 接入分区 Scene（2026-11-03）

维护者 2026-11-03 对 §9.7 的决定（均为推荐项）：**D5 = A**（分区场景里的 Monster 只做格内心跳的行者：位置、速度、HP、Idle 档位，不启动 AI / actionflow / movement.Move / state / Buff；依赖这些组件的 GM 命令返回“不支持”）；**D6 = A**（新增 `MoveIntent(vx, vy)`，只传速度、`(0,0)` 停下，不加目标点模式；分区场景里 `Move` / `MoveTarget` 返回“不支持”）；**D7 = A**（新增 `AttackTarget(target)`，固定伤害，结论异步，HP 变化从同步看）；**D8 = A**（SceneActor、Monster 的格内状态与 Player 的 BaseState 都用生成 DAO 承载）；D1 在 demo 分区配置里缺省 1s；D2 = A、D3 = A 维持，D4 推迟。普通场景（缺省）行为不变。

codebase-memory-mcp 本轮连不上，结构结论都按当前源码与 `rg` 得出。

### 10.1 改动

| 层 | 内容 | 位置（生成后的路径；模板在 `demo/` 下，同名加 `.tmpl`） |
| --- | --- | --- |
| 生成器 | SceneActor 实体：`roost add entity SceneActor` + `add dao`（demo 步骤），`noPersist=true lifetime=ephemeral`、Other 档（在 Scene 之后加锁），只由分区场景的 Node 在 handler 里新建（`EntityCreateParam.Param = partition.CellMode{}`），自己的 `cell.actor` 心跳 | `game/entities/scene_actor/entity.go`、`db/def/scene_actor.go` |
| 生成器 | D8：`SceneActorDao`（nocoll，全部 `nopersist,nosync`）与 `MonsterDao` 用同名字段承载 `cellworld.ActorState`——`PosX / PosY / HP` + `Cell*` 标量，去重表 `[]CellHit`、交接进度 `*CellHandoff` 是带 store 键的嵌套类型（定义在 monster.go：demo 步骤里 Monster 的定义先写进工程）；`game/partition.ReadActor / WriteActor` 一份映射，生成 setter 自带逆操作。Player 的 `BaseState` 在 `PlayerDao` 的 `Cell*` 字段：路由状态不持久，快照 HP 与快照版本（`CellHP` / `CellSnapEpoch` / `CellSnapSeq`）持久，快照位置随**被接受的回写**写进 `SavedPosX / SavedPosY / SavedSceneID`（witness 只写不持久的 `PosX / PosY`） | `db/def/{monster,player}.go`、`game/partition/partition.go` |
| 生成器 | D6 / D7 协议：`MoveIntent`（msg 10027：`VX / VY` → 已受理）、`AttackTarget`（10028：`Target` → 攻击序号 `Seq`）；错误码 `scene_partitioned`（100024）、`scene_not_partitioned`（100025）、`partition_not_entered`（100026） | `protocol/def/{move_intent,attack_target}.go`、`internal/errors/scene_partition.go`、`game/controllers/player/{move_intent,attack_target,partition}.go` |
| 配置 | `scene.partition` 按 A4① 声明一次（`game/settings`，game 服务的 `ConfigSchema`），加载时校验一次：网格与 owner（行优先 sid 列表）、`R + H ≤ G`、`R ≥ Base 的最大离开半径 300`（与场景桥常量有编译期断言）、`W ∈ [R, G − H]`、慢档必须是长于 50ms 的心跳档位（缺省 1s）、刷怪点在世界内且速度不超过 `max_speed`。`enabled` 缺省 false：其余键不读、不校验 | `game/settings/partition.go` |
| 运行时 | 每个 game 进程读同一份分区配置：sid 是 owner 的进程是那个格的 **Cell**（Scene 实体持有 Region 与 `scene.cell` 心跳、启动时刷满本格的刷怪点），每个进程都是自己玩家的 **Base**。`startPartition` 建 `cellworld.Node`（NATS core pub/sub），witness 经 `policy.Mirrors` 喂场景桥自己的 Manager / Interest（SceneActor 打成 `player` 命名空间、Monster 打成 `monster`，载荷是生成的同步补丁 `db.SyncPlayerDao` / `db.SyncMonsterDao`，客户端解码器不变） | `internal/service/game/partition.go` |
| 运行时 | 场景桥：分区模式的 Join 照旧开会话（held → `scene_ready`），不登记 Player subject，改由 `Node.Enter` 进入（位置取 Player 的快照，没有就取 spawn）；Leave 走 `Node.Leave`；Interest 用分区世界的边界。EnterGame 在分区模式不做本地落点（`EnterScene`） | `internal/service/game/scene.go`、`game/controllers/player/enter_game.go` |
| 运行时 | 端点：分区场景里 `Move` / `MoveTarget` / `gm.scene.root` 返回 `scene_partitioned`；普通场景里 `MoveIntent` / `AttackTarget` 返回 `scene_not_partitioned`。新 GM `gm.scene.partition`：Node 计数、Base 侧攻击结论、mirror 计数、Cell 的区域表；带 `player_id` 时是该玩家的路由状态 | `game/controllers/player/{move,move_target}.go`、`internal/service/game/gm.go` |
| 运行时 | Monster ID：分区模式用 `runtimeid.NewStartingAt`，序号从启动时刻（2026-01-01 起每秒 64 个）起算——走到邻格的怪比铸它的进程活得长，重启后的进程不能再发出它的 ID | `game/runtimeid/runtimeid.go` |

**§7.8 的管理器分类落地情况**：没有重写、也没有新增带锁的进程级管理器。场景桥在 Base 侧原样使用（多一类 subject：mirror）；spawner 在分区模式不启动（刷怪由 `Node.Spawn` 在 Scene + Monster 事务里做）；`PlayerOwners` 不动；`partitionRuntime` 只是进程接线（Node、Mirrors 的句柄与诊断用的原子计数），不持有格内状态。

**接入中发现、在 cellworld 里修掉的两处**（各有先红后绿，[修前输出](../performance/benchmarks/cellworld-v22-20261103/)）：

1. **不在内存的角色不能进 Nest 的加锁集合**。v2 的框架用例里 getter 是不带加载器的 `ManagerAccess`，不存在的实体返回 nil；demo 的 getter 装了 DataEngine 加载器，对不持久的种类（SceneActor / Monster）返回“聚合不存在”的**错误**，整个请求失败——第一轮端到端里 Cell 进程启动刷怪即失败（`entity kind 4 has no persistent DAO`）。修法：`NodeConfig.Loaded`（`EntityManager.Get(id) != nil`），Node 只把在内存里的角色放进加锁集合，handler 按实体 ID 取角色；新建一律锁 Scene，“检查时不在、handler 里已被另一个 Scene handler 建出”时新建失败回滚、由对方重传。用例 `TestWorldWithAGetterThatRefusesAbsentEntities`（strict getter 替身）。
2. **自己的 mirror 删过又回来时，客户端看不到自己**。owner 格崩溃后 witness 停了超过 TTL，Base 的 mirror 过期（Hide + Unregister）；重进后再出现时 Mirrors 只把它 Show 回空间索引，Interest 却还当“玩家看自己”的订阅持有（self 不是空间订阅，Hide 不撤），Manager 随 Unregister 已经丢了它。修法：`policy.Mirrors.Generation`（每次新建一个代号）、`BaseState.ShownGen`（进入时的代号），代号变了就 `Interest.Resubscribe`（Manager 对已有订阅幂等，只补上丢了的）。`TestCellCrashPlayerReentersFromSnapshot` 加客户端断言（修前 6 次全红，修后 6 次全绿）。

另有一处 demo 侧问题当场改掉：mirror 打包最初用一份临时 `PlayerDao` 的 setter 填值，`CellHP` 是持久字段，setter 要求当前上下文有打开的事务，Manager 的打包路径上没有 → panic（第二轮端到端）。改为直接填生成的同步补丁再编码，格式号从一份空 DAO 的编码里读出。

### 10.2 多进程端到端

脚本 `scripts/test-seamless-demo.sh`（分支），客户端 `scripts/testdata/seamless/drill.go.txt`（经真实 account 登录、玩家 TCP、demo 的同步帧解码 `game/clientsync`）。依赖是 `scripts/mirror-local.sh` 的私有实例（偏移 30000：Mongo 副本集 57117–57119、NATS JetStream 44222–44224、Redis 46379），etcd 由脚本在 58379 自起；没有碰共享环境与 4222 / 6379 / 17379 / 27017 / 2379。

| 进程 | 角色 |
| --- | --- |
| 九个托管服务 + sid 1000 的 game（`deploy/dev/run.sh start`） | sid 1000：分区场景的 **Base**（不托管格）；p1、p2 的连接在这里 |
| sid 1001 的 game | 格 (0,0)（x ∈ [0, 3000)）的 **Cell**，同时是 p3 的 Base |
| sid 1002 的 game | 格 (1,0)（x ∈ [3000, 6000)）的 **Cell**；演练中被 kill -9 再重启 |
| sid 1003 的 game | 没有 `scene.partition`：**普通场景**（p4，以及 demo 自己的机器人） |

分区配置：2 × 1 个 3000 的格，G 500、H 50、R = W 400、慢档 1s、回写 1s、租约 3s、伤害 10；刷怪点：分界线两侧各三只静止的怪（2960 / 2975 / 2990 与 3010 / 3025 / 3040）、一只沿 X 以 150 单位 / 秒往返的 walker。单实例锁缩到 ttl 6s（kill -9 后的重启只等 ttl）。Base 的 Interest 进入 / 离开半径 120 / 150。

结果（macOS，Apple M5，同机；最终一轮的客户端输出与计数在 [证据目录](../performance/benchmarks/cellworld-v22-20261103/README.md)，各进程日志在 `artifacts/perf/cellworld-v22/e2e/`，未提交）。同一套断言前后完整跑通两轮，数字取最终一轮：

| 步骤 | 做法 | 断言 | 结果 |
| --- | --- | --- | --- |
| 进场 | p1、p2（Base 1000）、p3（Base 1001）EnterGame → scene_ready | 各自在客户端看到自己的 SceneActor（`player` 命名空间、`cell_hp` 100）与附近的怪；p1 发 `Move` 得 `scene_partitioned` | 通过 |
| 直线跨界 | p1 以 100 单位 / 秒从 2900 走到 3150 再回 2850 | 两次 owner 变化；离自己 ≤ 130 的对象一次都没被 remove；在格 (1,0) 时手里有 p2、回到格 (0,0) 时手里有 p3；自己的同步最大间隔 | 通过：owner 变化 2，近处 remove 0，自己最大间隔 50ms |
| 抖动 | 300 单位 / 秒、每 0.67s 换向 8 段（每段越过对面的迟滞线） | owner 变化 ≥ 4；离自己 ≤ 100 的对象不被 remove（快速移动时客户端的自己比服务端晚约 100ms，半径相应收小） | 通过：owner 变化 8，自己最大间隔 50ms |
| 斜穿 | 沿对角线 (2880,1380) ↔ (3120,1620)，120 单位 / 秒 | 同直线 | 通过：最大间隔 50～61ms。2 × 1 网格没有四格交点，四格交点只在框架的进程内用例里（`TestWorldDiagonalThroughFourCellCorner`） |
| 跨界打怪 ×2 | p1 在格 (0,0)（从未越过 3050）打格 (1,0) 的怪 10 次；走进格 (1,0) 后打格 (0,0) 的怪 10 次 | Base 收到 10 个结论且都是 applied、无 unknown；客户端看到的目标 HP = 100 − 10 × 10 | 通过：两次都是 applied 10、duplicate 0、HP 100 → 0；两个 Cell 的 `DamageForwarded` 合计各 +10（确实跨格转发）、重试 0 |
| Monster 跨界 | p2 站到 walker 过线的位置旁边，等它从一个格交给另一个格 | 新格 Real、旧格不再 Real；p2 在它离自己 ≤ 115 时没 remove 它 | 通过：walker 从 sid 1002 交给 1001；期间格 (0,0) 接受交接 8 次、格 (1,0) 7 次（含玩家） |
| Cell 崩溃 | p1 打 p2 两下（100 → 80），等回写进 Base 的 Player；kill -9 sid 1002，重启（等锁 ttl） | Base 静默看门狗重进，新进程按快照建 SceneActor：epoch 前进、客户端里 p2 自己的 HP 是 80 | 通过：kill 到重启就绪 5.2s（上一轮 6.3s）；epoch 2 → 3、owner 1002、HP 80；新进程按配置重刷本格的 3 只怪 |
| 普通场景 | p4 登进 sid 1003 | `Move` 照旧返回落定的位置并在同步里看到；`MoveIntent` 得 `scene_not_partitioned`；demo 自己的机器人（`cmd/loadtest -count 2`，完整链路）在 sid 1003 上跑一轮 | 通过 |

### 10.3 不回归

全部在同一工作树、同机跑完：`GOWORK=off go build ./... && go vet ./...`、glsvet（nest、entity、dataengine/engine、sync/entitysync 含 policy、gameplay/cellworld）无输出、`python3 scripts/check-doc-anchors.py`、根包 `go test -count=1 .`、`go test -race -count=1 ./gameplay/cellworld/ ./framework/sync/...`、全量 `GOWORK=off go test ./...`（含 codegen：doctor 用例钉住的业务键数随 `scene.partition` 的声明从 4 变 32）、`scripts/test-demo-generated.sh`（私有依赖）、`test-mmorpg-generated.sh`、`test-sync-modes-generated.sh`；生成工程 `go build / vet / test`、`roost id check`、`roost generate --check`、`go generate ./...` 后 porcelain 干净。逐项结果与耗时见 [regression-summary.txt](../performance/benchmarks/cellworld-v22-20261103/regression-summary.txt)：全部通过（`test-demo-generated.sh` 的 8 个真实依赖用例逐个 PASS、`test-mmorpg-generated.sh` 14 个场景 PASS、全量 `go test ./...` 无失败）。genre 全套（`test-genres-generated.py`）没有跑：本轮只动 `-template game-demo` 的模板、cellworld 与 `policy.Mirrors`。

### 10.4 缺口更新

§8.4 / §9.6 的表照旧有效，变化与新增如下：

| # | 缺口 | v2.2 状态 | 性质 |
| --- | --- | --- | --- |
| v1-2 | 视野只到 Base、没到客户端 | **已解决（demo）**：真实客户端经 demo 的同步帧看到 mirror | — |
| v1-4 | NPC 崩溃丢失、无重建来源 | **部分解决**：重启的 Cell 按 `scene.partition.spawns` 重刷本格的点（端到端验证 3 只）；死亡（HP 0）的怪留在原地不补刷，走到别的格的怪不回原格补 | 上线前必须解决（刷怪账本跨格的规则由产品定） |
| v1-15 | 配置不经 `app.LoadConfig` | **已解决（demo）**：`scene.partition` 按 A4① 声明；框架层仍是 `NodeConfig` 结构体 | — |
| v2-4 | R_feed ≥ Base 离开半径 | **已在配置校验里强制**（R ≥ 300，与场景桥常量有编译期断言） | — |
| v2-6 | 跨格 / 跨进程的队伍可见 | 未变：分区模式下 `SetTeam` 的成员是 Player 实体，而 Player 不是本 Manager 的 subject | 上线前必须解决 |
| v2-7 | 一个进程既是 Cell 又是 Base | **已覆盖**：sid 1001 同时是格 (0,0) 的 Cell 与 p3 的 Base，p1（Base 1000）与 p3 互相可见 | — |
| v2.2-1 | 分区场景里客户端看到的“自己”是 SceneActor 的 mirror（位置、`cell_hp`），不是自己的 Player subject：exp、背包等自己的字段不经场景同步（demo 机器人的 `scene_alive` 靠 exp 判断，在分区场景里不适用） | 新 | 上线前必须解决（自己的字段走 `access.player.self_sync` 或给 SceneActor 编码加字段，见 10.5 E4） |
| v2.2-2 | Base 重启后 Player 重新装载，`PosX / PosY` 是 0，进入用 spawn 点；持久的快照位置（`SavedPos*`）写了但没读回 | 新 | 原型简化（Enter 前读 `SavedPos*` 即可） |
| v2.2-3 | 攻击结论只进 Base 的日志与计数（`gm.scene.partition`），没有推给客户端 | 新 | 原型简化（D7 = A 允许“推送或日志”） |
| v2.2-4 | Node 的 handler 在 Nest 引擎启动之后装进**包级**注册表（game 服务的 Init 晚于 Nest Mod 的 Start；派发时按包级表兜底解析） | 新 | 需要维护者确认（10.5 E3） |
| v2.2-5 | Monster ID 的“重启不重号”靠启动时刻起算的序号（每秒 64 个；两次启动至少隔一个单实例锁 ttl） | 新 | 原型简化（正式做法是持久发号或 ID 带启动代际） |
| v2.2-6 | kill -9 后整格停摆直到新进程拿到单实例锁（缺省 ttl 15s；演练缩到 6s）：期间该格的玩家看不到这个格的东西，Base 每秒重发一次 Enter | 新 | 定案前提（与 App 单实例锁的 ttl 是同一个数） |
| v2.2-7 | 分区进程上仍有本地的普通 Scene 实体（地形、刷怪心跳都在，spawner 不装），只是没人用 | 新 | 原型简化 |

### 10.5 需要维护者决定

（维护者 2026-11-03 已决定：E1 = B、E2 = B、E3 = B、E4 = A，E5 待产品规则；落实见 [§11](#11-v23框架契约-e1e3自己的状态e4与追加四项2026-11-04)。）

- **E1 不持久实体“不存在”的答复**（10.1 第 1 处）。A（本轮，推荐原型阶段）：cellworld 自己按 `Loaded` 过滤加锁集合，框架不动。B：框架统一——`ManagerAccess.Get` 对不持久的种类（没有持久 DAO）在内存未命中时返回 (nil, nil)、不问加载器；所有 handler 都会在目标不存在时收到 nil 而不是错误（行为变化，生成的 sender 与现有 handler 要逐个确认）。并入 main 时推荐 B。
- **E2 Interest 的“自己”在 Hide / Show 之后不恢复**（10.1 第 2 处）。A（本轮）：cellworld 侧按 mirror 代号 `Resubscribe`；B：Interest 修正——观察者自己的目标被 Hide 时撤掉 self 订阅、重新 Show 时恢复（改 Interest 现有代码）。并入 main 时推荐 B（这是 Interest 的契约问题，不只 mirror 会碰到）。
- **E3 服务在 Nest 启动后注册 handler**（v2.2-4）。A（本轮）：包级注册表 + 派发时兜底解析；B：App / Nest Mod 给服务一个“引擎启动前注册实例 handler”的钩子。推荐 B（并入 main 时）。
- **E4 分区场景里自己的 Player 字段**（v2.2-1）。A（推荐）：分区模式打开 `access.player.self_sync`，自己的 Player 走自同步，场景里只有 SceneActor；B：SceneActor 的 mirror 编码带上需要的 Player 字段（每份 witness 都要带，体积大）。
- **E5 分区场景的刷怪账本**（v1-4）：死亡后在哪个格补刷、走到别格的怪算谁的；原型现在不补刷。需要产品规则。
- D4（`policy.Mirrors` 是否写进 impl/04）照旧等并入 main；本轮 Mirrors 多了 `Generation`。

### 10.6 方向判断

- D5～D8 的 A 让 demo 接入没有碰到协议层面的方向问题：交接、伤害、重进全部沿用 v2 / v2.1 的 cellworld，demo 只补了实体、DAO 映射、配置与桥。接入暴露的两处缺陷（10.1）都不在交接协议里，而在 **cellworld 与正式运行时的接缝**：v2 的框架用例用的是测试替身 getter 与只测 BaseState 的崩溃用例，替身对“不存在”的答复和真实 DataEngine 不同、用例没检查客户端的自己——这是测试替身与正式链路的差距，不是设计前提的问题；两处都已用更接近正式链路的替身补了用例（strict getter、客户端断言）。
- 这一系列是 cellworld 原型第三轮改动（v2 → v2.1 → v2.2），新出现的缺陷集中在“框架原型 ↔ 正式 Nest / DataEngine / Interest”的接缝上。若要并入 main，建议先定 E1～E3：它们本质上是三条框架契约（不持久实体不存在时的答复、Interest 自己的订阅、服务注册 handler 的时机），在框架里定一次比让每个接入方各自绕开更稳。

## 11. v2.3：框架契约 E1～E3、自己的状态（E4）与追加四项（2026-11-04）

维护者 2026-11-03 对 §10.5 的决定：**E1 = B**、**E2 = B**、**E3 = B**、**E4 = A**；E5（分区场景的刷怪账本）是产品规则，本轮不做，登记为“待产品规则”（§11.8）。同日追加四项（排在 E1～E4 之后）：① Base 重启后回到保存的位置；② 攻击结论推给攻击方客户端；③ 战斗字段全部放 Cell；④ 视野外 / 别的 Base 上的队友的队伍面板。E1～E3 是 Nest / Sync / 服务生命周期的框架契约变化；分支不发版，CHANGELOG 不改，合入 main 前的事项见 §11.9。

codebase-memory-mcp 本轮连不上，结构结论（调用方清单、影响面）都按当前源码与 `rg` 得出。修前输出在 [cellworld-v23-20261104](../performance/benchmarks/cellworld-v23-20261104/)。

### 11.1 E1：不持久种类内存未命中答 nil

| 项 | 内容 |
| --- | --- |
| 契约变化 | `entity.ManagerAccess` 对**不持久的种类**（登记了 builder，且 `NoPersist` 或一个 DAO 都没有：`kindHasNoAuthority`，判定与 DataEngine Repository 拒绝加载的条件逐字相同）在内存未命中时：`Get` 答 (nil, nil)、`GetMany` 留 nil 占位、`IsLoaded` 答 true（Nest 准入不再为它转 I/O 阶段），业务阶段（`LoadedEntitiesOnly`）同样答 nil 而不是 `ErrColdLoadInLogic`；不问加载器。持久种类、未登记 builder 的 kind 不变（照旧交给加载器）。原先 Repository 对它答 `ErrEntityAggregateNotFound`（“has no persistent DAO”），整个请求失败、handler 不运行 |
| Nest 侧 | 派发对 nil 目标的处理不变，只换了回复的错误值：单目标与多目标的首个目标回复 `nest.EntityNotFoundError()`——满足 `ErrEntityNotFound`，并带 `errcode.ErrEntityNotFound`（客户端码 `entity_not_found` = 2），与加载器报告“权威里不存在”时客户端拿到的码相同，所以对不持久目标客户端看到的码没有变；多目标其余位置把 nil 交给 handler |
| 生成器 | 生成的分发包装原先把 nil 目标当成类型不符（`ErrEntityTypeMismatch`，客户端看成内部错误）；改为回复同一个 `EntityNotFoundError()`。重新生成 `codegen/internal/entity/testdata/awaitflow`（`codegen/scripts/awaitflow-runtime.sh` 一致性 + race 通过） |
| 删掉的绕法 | cellworld `NodeConfig.Loaded` / `dispatchLocked` 的加锁集合过滤；demo `partition.go` 的 `Loaded:`；harness 的 strict getter 替身改为“装了 DataEngine 形状加载器的真实 ManagerAccess”，断言加载器一次都没被问到 |
| 红绿 | `TestManagerAccessAnswersAbsentNonPersistentKindsWithoutTheLoader`（entity，修前 6 条断言全红：IsLoaded false、Get/GetMany 带加载器错误、业务阶段冷缺失、加载器被问 2 次）；`TestAbsentNonPersistentTargetIsAnsweredWithoutTheLoader`（nest：修前单目标拿到加载器的“不存在”、多目标 handler 不运行、加载器被问 2 次）；`TestWorldOnALoaderBackedGetter`（cellworld：去掉 Loaded 后在旧 ManagerAccess 上 straight / attack 两个子用例都红，`kind 213 has no persistent DAO`、玩家等不到第一份视野）；awaitflow `TestGeneratedDispatchAnswersANilTargetAsNotFound`（修前 `nest: entity type mismatch`） |

**受影响调用方清单与结论**（按源码与 `rg` 逐个核对；“nil 目标”只对不持久种类、且只在内存里没有时出现）：

| 调用方 | 原答复 | 现答复 | 结论 |
| --- | --- | --- | --- |
| Nest `singleDispatch` / `dispatchMany` 首目标 | 加载器错误 → `targetLoadError` → `entity_not_found` 码 | `EntityNotFoundError()`（同码，满足 `ErrEntityNotFound`） | 客户端码不变；错误链上不再有 `ErrAuthorityEntityNotFound`（仓内无调用方对不持久目标判它） |
| Nest `dispatchMany` 非首目标 | 整个请求失败 | handler 收到 nil | 手写 handler：cellworld（按 ID 取、nil 安全，正是 E1 的目的）、`wiring/physicsroom`（自带 getter，不受影响）、genre 夹具与 demo 测试里的手写 handler（都只取首目标或自建 getter）；生成的 handler 由包装回 `EntityNotFoundError()`，业务函数见不到 nil |
| demo 生成 handler 的不持久目标 | `EnterScene` / `MovePlayer` / `MoveTarget`（[Player, Scene]）、`RootMonster`（Monster）、`SpawnMonster`（[Monster, Scene]）、`SetSceneTimeRate`（Scene） | 目标不在时同样回 `entity_not_found` | 行为一致（Scene 启动即建；Monster 死亡 / 刷掉后 GM 拿到同一个码） |
| Nest 广播 | 每个缺失目标记一条 ERROR 后继续 | 静默跳过 | 只少了噪声 |
| `TryBatch`（心跳投递） | `ErrEntityNotFound` 包着冷缺失 | `ErrEntityNotFound` | 同一哨兵；心跳驱动按 `dropped{not_loaded}` 计（原先对不持久目标按“rejected”跳拍并告警） |
| `Cast`（handler 内取锁） | `ErrColdLoadInLogic` | `ErrEntityNotFound`（index / id） | 仓内无调用方对 Cast 判 `ErrColdLoadInLogic` |
| 组迁移（`group_transition`） | 错误 → `ErrEntityNotFound` | nil → `ErrEntityNotFound` | 不变 |
| 卸载后重载（`unload_resync`） | `ErrAuthorityEntityNotFound` → 退回 remove | nil → 退回 remove | 不变（不持久实体本来就不会被仅内存卸载） |
| `LoadRemoteEntity` | 加载器错误 | nil | Remote 托管的种类都有持久 DAO（Repository 只加载这类），不受影响 |
| 生成 lifecycle（`GetOrCreate`） | `ErrEntityAggregateNotFound` → 新建 | nil → `Err…NotFound` → 新建 | 同一结果 |
| kit / wiring（heartbeat、entitytimer、selfsync 的 Lookup） | 只读 `Manager().Get` | 不变 | 不经 `ManagerAccess.Get` |
| genre 夹具 | 不持久种类：level 的 `Room`、awaitflow 的 `Player`、remoteflow 的 `Clerk` 等 | — | 全套 `test-genres-generated.py` 通过（§11.7） |

测试侧唯一的波及：`TestUnloadResyncStormIsBounded` 用的原始 ID（6000+i）的 kind 位恰好解出同包登记成不持久种类的 kind 222，改用同包未登记的 kind 110～119（测试数据问题，不是契约问题）。

兼容影响：项目未上线，不做旧进程 / 旧数据兼容；没有存储或协议格式变化。

### 11.2 E2：Interest 的 self 跟着自身实体的 Hide / Show 走

| 项 | 内容 |
| --- | --- |
| 契约变化 | 以某实体为 self 的玩家（`Enter` 时的自身实体），在该实体被 `Hide`（退出空间索引）期间看不见自己；它再次 `Show` / `Move` 进空间索引时 self 自动恢复（客户端收到一次全量）。同一次结算里 Hide → Show 照旧经 `departedSubjects` 先退订再订阅。原先 self 不是空间订阅、Hide 不撤它：自身实体被 Hide 并注销、重新登记再 Show 之后，Interest 以为还持有、Manager 已经忘了，玩家从此看不到自己 |
| 实现 | `Interest.selfBy`（`selfOf` 的反查）；`removeSubjectLocked` 收回、`placeSubjectLocked` 在目标重新进入空间索引时恢复 self 贡献（`contributeSelfLocked`），`setSelfLocked` 统一维护两个方向 |
| 删掉的绕法 | `policy.Mirrors.Generation`、cellworld `BaseState.ShownGen` 与 `feedMirrors` 里的 `Interest.Resubscribe`、demo 的 `CellShownGen`（nopersist,nosync 字段，不涉及存储格式） |
| 红绿 | entitysync 层 `TestSelfFollowsItsEntityThroughHideAndShow`（不依赖 cellworld；修前 `shown again, the player does not see itself`）；cellworld `TestCellCrashPlayerReentersFromSnapshot` 的客户端断言保留，去掉绕法后在旧 Interest 上 3/3 红（`the client does not hold its own scene actor again`），新 Interest 上 -count=6 与 -count=20 全绿 |
| 波及 | `Hide` 的调用方：`policy.Mirrors`（本项的目标）、`movement.InterestSpace.Leave`（只对非玩家实体 Hide，玩家走 Leave）、demo `HideSubject`（怪、掉落）。玩家自己的实体只在 mirror 这条路径上被 Hide |

### 11.3 E3：服务在 Nest 引擎启动前注册实例 handler

| 项 | 内容 |
| --- | --- |
| 契约变化 | App 在任何 Mod Init 之前把本次运行的 Service 登记为 `app.ModService`（阶段顺序不变：Mod Init → Provide → Start，然后 Service.Init）。wiring/nest 的可选钩子 `NestHandlerInstaller.InstallNestHandlers(nest.HandlerRegistrar)`：Nest Mod 在引擎 Start 之前调用一次，服务把只属于本引擎实例的 handler 注册上去；失败则 Nest Mod 启动失败。Nest 在 shared_mods 还是服务专属 Mod 里都一样生效（服务在 Mod 之前就登记了） |
| cellworld | `cellworld.Handlers`：handler 名单先于 Node 注册（Cell 侧与 Base 侧全部注册），`NewNode` 经 `NodeConfig.Handlers` 接上、只接一个 Node（原子指针，跟着服务实例走，不是进程级注册表）；`Node.RegisterHandlers` 改为返回错误、同一份名单。harness 改为正式进程的顺序（先注册、引擎 Start、再 NewNode），全部 cellworld 用例都走这条路径 |
| demo | game Service 持有 `cellworld.NewHandlers()`，`InstallNestHandlers` 注册，`startPartition` 接上；删 `packageHandlers`（包级注册表 + 派发时兜底解析） |
| 红绿 | `TestModsSeeTheRunningServiceBeforeTheyStart`（app，修前 `ModService at Provide = <nil>`）、`TestServiceInstallsInstanceHandlersBeforeTheEngineStarts`（wiring/nest，修前 `InstallNestHandlers called 0 time(s)`；同用例钉住 handler 不进包级表、钩子失败时引擎不启动）；`TestHandlersBindToExactlyOneNode` |
| 没动的 | Nest 自身的“派发时按包级表兜底解析”（`getHandlerEntry`）仍在：框架测试与引擎构造后注册的包级 handler 依赖它，删它是另一件事。demo `game/partition` 的 `Install` / `Node()`（实体心跳找本进程 Node 的原子指针）不是 handler 注册表，本轮没动：Player 由 DataEngine 装载、没有创建参数可以带 Node，换掉它要给实体加注入入口 |

### 11.4 E4：分区场景里自己的 Player 字段

E4 = A 的本意是“自己的 exp、背包等经自身视图走同一条连接，SceneActor 的 mirror 不带这些字段”。实现上**没有翻 `access.player.self_sync` 这个生成器开关**，原因：这个开关把 `policy.SelfView` 装在 Nest Mod 自己的 Manager 上、对所有场景生效，而一个 Player 的同步状态只能接一个 Manager——普通场景里 demo 的场景桥已经把 Player 登记在自己的 Manager 上，开关一开普通场景就会被拒（`Name: "scene"` 的注释写的就是这个冲突）。所以分区模式下在**场景桥自己的 Manager** 上做同一件事（不改普通场景）：

| 项 | 内容 |
| --- | --- |
| 绑定 | `joinPartition` 在会话打开后把玩家的 Player 登记为场景 Manager 的 subject，`policy.Direct` 把玩家自己的会话以 `self` 视图绑上（重试打开的会话在 `openLocked` 成功时绑；排在上一次离场的退役之后的登记在被接受时绑）；离场时解绑、注销，再关会话 |
| 视图 | Player packer 新增 `self`：Name、Level、Exp、Gold、Items、Equipment、GuildID。不含位置与 HP（SceneActor 的 mirror 带，分区场景的位置与 HP 在 Cell）、不含属性表（战斗字段归属待定，§11.6） |
| mirror | SceneActor 的 mirror 编码本来就只带 PosX / PosY / CellHP（`partitionPlayerMirrorMask`），不带 exp 等；端到端断言钉住 |
| 端到端 | `own_state`：p1 `AddExp 7` 后客户端手里自己的 Player 的 exp 加 7，SceneActor 的 mirror 里没有 exp |
| 副作用 | 同一 Base 上的队友经 `SetTeam` 的关系视野现在能看到彼此的 Player（缺省视图）——与普通场景队伍的行为一致；别的 Base 上的队友仍看不到（§11.6 ④） |

需要维护者确认：若坚持用生成器开关，代价是普通场景改为“Player 只由 SelfView 登记、场景桥不再登记 Player、别人看他改走另一类 subject”，等于重写普通场景的同步；推荐保持本节做法。

### 11.5 追加第 1、2 项

| 项 | 改动 | 红绿 |
| --- | --- | --- |
| ① Base 重启后回到保存的位置 | `partition.ReadBase(dao, sceneID)`：Player 不在世界里、实时位置（`PosX / PosY`，不持久）为零时，快照位置取持久的 `SavedPosX / SavedPosY`，只认 `SavedSceneID` 等于本进程分区场景 ID 的（否则留零，Enter 取 spawn）。`SavedPos*` 随被接受的回写写入（v2.2），所以保存的是最近一次回写的位置 | 单测 `TestReloadedPlayerEntersAtItsSavedPosition`（修前 `X:0 Y:0`）；端到端 `base_restart`：只做 Base 的 sid 1004 上的 p5 走到 (1200, 1500)、等回写，kill -9 sid 1004、重启、重新登录，进场在 (1175, 1500)（最近一次回写的位置），修前在 spawn (2900, 1500) |
| ② 攻击结论推给攻击方 | 新协议 `AttackResultPush`（10104，game-demo 的 `protocol/def/attack_result.go`）：`Seq`、`Target`、`Outcome`（applied / duplicate / rejected / unknown）、`Reason`（not_owner / not_visible / out_of_range / dead / busy）、`Damage`（只有 applied 非零）、`TargetHP`（unknown 时为 0：不声称）。Base 的 `Hooks.AttackResult`（提交后、Node 出站 goroutine）经 player TCP `PushPlayer` 推给攻击方，推送只准入出站队列；玩家离线就丢（计数照旧在 `gm.scene.partition`） | 单测 `TestAttackResultPushNamesTheVerdict`；端到端两轮跨界打怪各 10 次，客户端各收到 10 条 applied、伤害 10、推送里最低的目标 HP 等于同步里看到的 0；修前 `0 of 10 pushes` |

端到端（`scripts/test-seamless-demo.sh`，私有依赖：`scripts/mirror-local.sh` 偏移 30000，etcd 58379）新增了 sid 1004 与三类断言（`own_state`、每次攻击的结论推送、`base_restart`）。追加的断言失败时不中断后续步骤，演练结束一并报告，所以修前一次就能看到每一项的红：修前（E1～E3 之后、E4 之前的代码，加上新协议的定义）**4 项全红、原有 9 步全绿**；修后全部通过（[修前](../performance/benchmarks/cellworld-v23-20261104/e2e-red-before.txt)、[修后](../performance/benchmarks/cellworld-v23-20261104/e2e-green.txt)、[evidence.json](../performance/benchmarks/cellworld-v23-20261104/e2e-green-evidence.json)）。

| 步骤 | 修后结果 |
| --- | --- |
| own_state（E4） | AddExp 7 后客户端自己的 Player：exp 0 → 7；SceneActor 的 mirror 里没有 exp |
| 直线 / 抖动 / 斜穿 | owner 变化 2 / 8 / —，近处 remove 0，自己最大间隔 50 / 52 / 50ms |
| 跨界打怪 ×2 | applied 10、duplicate 0、HP 100 → 0、跨格转发 10、重试 0；推送 10 / 10 |
| Monster 跨界 | walker 从 sid 1002 交给 1001 |
| Cell 崩溃 | kill 到重启就绪 6.2s；epoch 2 → 3、HP 80；重刷 3 只怪 |
| base_restart（①） | 在 (1175, 1500) 进场（spawn 是 (2900, 1500)） |
| 普通场景 | Move 照旧、MoveIntent 得 scene_not_partitioned；demo 机器人在 sid 1003 跑一轮 |

### 11.6 追加第 3、4 项：停下来，需要维护者决定

**第 3 项（战斗字段全部放 Cell）**：先按要求列清 demo 里的战斗字段与写入点。

| 字段 | 存在哪 | 谁写 | 性质 |
| --- | --- | --- | --- |
| 属性 `hp`、`attack`（`game/gameplay/attribute/combat.go` 的 `Combat`）的 **Base 层** | PlayerDao `AttrBase`（persist,sync） | `AttributeComponent.LevelUp`（`ProfileComponent` 升级时调：`AddExp`、副本结算、邮件 / 购买发经验等经 `handlerAddExp` / `handlerClaimDungeon` 的升级路径）；装载时按等级重建 | **由等级推出**：等级、经验是养成数据 |
| 属性的 **Gear 层** | PlayerDao `AttrGear`（nopersist,nosync） | `AttributeComponent.RefreshGear`（`handlerEquipItem` 换装、配置重载） | **由装备推出**：装备在背包里，是养成数据 |
| 属性的 **Final 层**、派生的 `power` | PlayerDao `AttrFinal`（nopersist,sync） | `derive`（Base + Gear 之后 `Update`） | 派生值 |
| 分区场景的**当前 HP** | SceneActor DAO `HP`（Real，Cell 进程）；Base 的快照 `CellHP`（persist,sync）与 `CellSnap*` | Cell：伤害结算（`onDamage`）、回写；Base：接受回写；进场 / 重建时取快照或 `scene.partition.player_hp` | 已经只有 Cell 权威 + Base 快照（v2） |
| 分区场景的**伤害** | 配置 `scene.partition.attack_damage`（D7 = A 的固定伤害） | — | 与属性 `attack` 无关 |
| Monster 的 HP、buff | 普通场景：MonsterDao + CombatDao（root buff、1Hz 心跳）；分区场景：MonsterDao 的格内 `HP`，不启动 buff（D5 = A） | — | 分区场景里已经只在 Cell |
| 玩家 buff | demo 没有 | — | — |

结论：demo 里“Player 一份 HP、SceneActor 一份 HP”的两份，其实是两个不同的量——属性 `hp` 是**由等级和装备推出的上限 / 面板值**（养成的结果），SceneActor 的 `HP` 是**分区场景里的当前血量**（战斗状态）；属性 `attack` 同样由等级和装备推出，分区场景的伤害现在不读它。把“一切战斗字段的权威放 Cell”落到这组字段上，正好碰到维护者事先点名的情形“某字段既影响战斗又是养成数据”：`hp` / `attack` / `power` 的输入（等级、经验、装备）是持久的养成数据、住在 Base 的 Player 上，输出是战斗面板。需要产品决定的问题与选项：

- **A（推荐）**：养成输入（等级、经验、装备、`AttrBase`）留在 Base（持久、照旧）；战斗面板（最大 HP、攻击、派生的 power、将来的 buff 层）与当前 HP 的权威在 Cell 的 SceneActor。Base 上改了养成输入（升级、换装）就发一条“面板变化”命令给目标 Real（与攻击同一套转发、去重、结果未知语义），Cell 在 SceneActor 上重算 Final、按新上限夹当前 HP，随回写进快照；分区模式下 Player 的 `AttrFinal` 不再同步给客户端（面板从 SceneActor 的 mirror 来）。要定：升级是否回血、上限变小时当前 HP 怎么夹、命令丢了 / 结果未知时面板以谁为准。代价：cellworld `ActorState` 加面板字段与一种命令消息，SceneActor / Monster DAO 加字段，demo 的属性组件分两种模式。
- **B**：只把**当前 HP**（以及将来的 buff）算战斗状态、放 Cell（v2 已经如此）；最大 HP / 攻击仍由 Base 从养成数据推出，作为只读参数随 Enter / 交接 / 一条参数更新消息带给 Cell（Base 权威）。改动最小，但“攻击力在 Base”不满足“一切战斗字段在 Cell”的字面要求。
- **C**：分区模式下属性完全不用（伤害、HP 都按 `scene.partition` 的配置），demo 保持现状，只在文档里写明。

本轮没有改这部分代码（普通场景行为不变）。

**第 4 项（视野外 / 别的 Base 上的队友的队伍面板）**：视野内的队友经 Ghost / mirror 本来可见；E4 之后同一 Base 上的队友也经 `SetTeam` 的关系视野看到彼此的 Player（含 Base 所知的位置 `PosX / PosY` 与快照 `cell_hp`）。缺的是**别的 Base 进程上**、彼此不在视野里的队友：p1 的 Base（sid 1000）手里没有 p5（Base sid 1004）的任何状态，现有的消息只有 Cell → Base 的 witness（维护者要求不给 Cell 加订阅），没有 Base 与 Base 之间的通道，`selfsync.Groups` / `policy.GroupView` 只能把**本进程 Manager 上登记的**实体推给成员。所以这一项要先定一条 Base 之间的数据路径，这是架构选择，不是实现细节：

- **A（推荐，原型）**：Base 之间经 cellworld Node 已有的 NATS 传输发“队员快照”：每个 Base 按回写周期（缺省 1s）把本进程队员的快照（位置、HP、在线）发给其他队员所在 Base 的 sid；收到的一方把它们放进一份 mirror（`policy.Mirrors`，命名空间 `team_member`，TTL 过期即删），用 `policy.GroupView` 推给本进程的队员（队伍面板视图），不进空间索引。要定：队伍成员与“成员在哪个 Base”从哪来（demo 的队伍由 match 服务组成、`MatchFound` 推给各成员所在进程；“成员 → Base sid”可用账号绑定的 sid / `PlayerOwners`）、刷新频率、面板字段。
- **B**：队伍做成 Remote 托管实体，DAO 里存各成员快照，各 Base 定期写自己成员的那一项，读侧 GroupView。持久且一致，但每个成员每秒一次 Remote 事务，代价大。
- **C**：队伍面板只显示同 Base 与视野内的队友（现状 + E4），跨 Base 的面板等真正需要时再做。

v2-6（分区模式下 `SetTeam` 的成员是 Player 实体）：E4 之后 Player 在本 Base 的场景 Manager 上有 subject，同一 Base 内的关系视野因此恢复；跨 Base 的部分随上面的选择一起做。本轮没有为第 4 项写代码，端到端里“两个不同 Base 上、彼此不在视野内的队友互相看到位置 / HP”的断言也没有加（没有可验证的实现）。

### 11.7 不回归

全部在同一工作树、同机（macOS，Apple M5）跑完，`GOWORK=off`、从模块根：`go build ./... && go vet ./...`；glsvet（`./framework/nest/... ./framework/entity/... ./framework/dataengine/engine/... ./framework/sync/entitysync/... ./gameplay/cellworld/...`）无违例；`python3 scripts/check-doc-anchors.py` 0 broken；根包 `go test -count=1 .`；`go test -race -count=1 ./gameplay/cellworld/ ./framework/nest/... ./framework/entity/... ./framework/sync/... ./framework/app/... ./wiring/nest/...`；`TestCellCrashPlayerReentersFromSnapshot -count=20`；全量 `go test -count=1 ./...`（含 codegen）；`codegen/scripts/awaitflow-runtime.sh`（生成器一致 + race）；`scripts/test-demo-generated.sh`（私有依赖，8 个真实依赖用例 PASS）、`test-mmorpg-generated.sh`、`test-sync-modes-generated.sh`；因为 E1 改了全部 handler 对不持久目标的答复，**genre 全套** `scripts/test-genres-generated.py` 13 类全部 passed；新生成的 game-demo `go build / vet / test ./...`、`roost id check`、`roost generate --check`、`go generate ./...` 后 porcelain 干净；本仓 `go generate ./...` 后 porcelain 干净；端到端 `scripts/test-seamless-demo.sh` 全部步骤通过（§11.5）。

### 11.8 E5：分区场景的刷怪账本（待产品规则）

不做。现状照 §10.4 v1-4：重启的 Cell 按 `scene.partition.spawns` 重刷本格的点；死亡（HP 0）的怪留在原地不补刷，走到别的格的怪不回原格补。要产品定的规则：死亡后在哪个格、隔多久补刷；走到别格的怪算出生格的名额还是所在格的；Cell 崩溃时在途交接中的怪算谁的。定了之后账本放在出生格的 Scene 实体（区域表旁）还是单独的持久实体，再按规则做。

### 11.9 合入 main 前还要做的

- E1～E3 是框架契约：合入时 CHANGELOG `[Unreleased]` 写清三条（不持久种类的 nil 答复与 `EntityNotFoundError`、Interest self 跟着 Hide / Show、`app.ModService` + `NestHandlerInstaller`），框架文档已改（impl/02 第 6a 条、guide/04 Interest 一节、impl/01 第 4 条）；codegen 的分发包装形状变了，按发版流程决定是否需要 `--min-core`（生成物用了新 API `nest.EntityNotFoundError`，需要）。
- E3：Nest 的“派发时按包级表兜底解析”是否收敛（框架测试与引擎构造后注册的包级 handler 依赖它），以及 demo `game/partition.Install` 这一类“实体找本进程 Node”的原子指针是否要换成实体注入，单独定。
- E4：确认 §11.4 的做法（场景桥 Manager 上的 self 绑定）替代翻 `access.player.self_sync` 开关。
- 第 3、4 项按 §11.6 的决定实施（第 3 项已实施，§12.2；合入时 CHANGELOG 写 cellworld 线上格式 v4 与 game-demo Player 新增的持久字段，升级须先停旧进程）；第 4 项实施时补端到端“两个 Base 上彼此不在视野内的队友互相看到位置 / HP”。
- D4（`policy.Mirrors` 是否写进 impl/04）照旧；本轮 `Mirrors` 删掉了 `Generation`。

### 11.10 缺口更新

§8.4 / §9.6 / §10.4 的表照旧有效，变化与新增如下：

| # | 缺口 | v2.3 状态 | 性质 |
| --- | --- | --- | --- |
| v2.2-1 | 分区场景里自己的 exp、背包等字段不经场景同步 | **已解决（demo）**：self 视图（E4，§11.4） | — |
| v2.2-2 | Base 重启后进场用 spawn | **已解决（demo）**：读回 `SavedPos*`（§11.5） | — |
| v2.2-3 | 攻击结论只进日志与计数 | **已解决（demo）**：`AttackResultPush`（§11.5） | — |
| v2.2-4 | Node handler 装进包级注册表、派发时兜底 | **已解决**：`NestHandlerInstaller` + `cellworld.Handlers`（E3） | — |
| v2-6 | 跨格 / 跨进程的队伍可见 | **部分**：同一 Base 上的队友经 `SetTeam` 的关系视野恢复（E4 之后 Player 在场景 Manager 上）；别的 Base 上的队友见 §11.6 ④ | 需要维护者决定 |
| v2.3-1 | 只锁角色的单实体请求（输入、换档）发给已不在本进程的角色时，由 Nest 以 not found 丢弃并记一条 WARN（`async handler failed … entity not found`），不再计入 `InputsDropped`（原先 Loaded 过滤先数一次再丢）。cellworld 全套用例里共 2 条 | 新 | 原型简化（交接后 Base 的输入发往旧 owner 的短窗口） |
| v2.3-2 | 战斗字段的归属（属性 hp / attack / power 由养成数据推出） | 新 | 需要维护者决定（§11.6 ③） |

### 11.11 方向判断

- §10.6 判断“接入暴露的缺陷集中在框架原型 ↔ 正式 Nest / DataEngine / Interest 的接缝上”，本轮把这三处接缝在框架里定成契约，cellworld 侧对应的三处绕法（Loaded 过滤、mirror 代号 + Resubscribe、包级注册表 + 兜底）全部删掉，cellworld 的测试改成走正式形状（装了加载器的 ManagerAccess、先注册后建 Node）。这是 cellworld 第四轮改动；本轮没有新的协议缺陷，新增的问题（§11.6）是产品 / 架构选择而不是实现错误：战斗字段在“养成数据推出的面板”与“场景内的战斗状态”之间怎么切、Base 之间要不要有数据通道。二者都决定无缝大世界能不能从“原型”走向“正式能力”，建议在合入 main 之前先定。

## 12. v2.4：第 3 项 = A、第 4 项 = A、E4 确认（2026-11-04）

> 第 3 项已实施（§12.2）；第 4 项当时停下（§12.1），维护者定 A 之后在 §13 实施。

维护者 2026-11-04 的决定（都按 §11.6 的推荐）：**第 3 项 = A**（养成输入留 Base；面板——最大 HP、攻击、power、将来的 buff——与当前 HP 的权威在 Cell 的 Real，Base 升级 / 换装时向目标 Real 发“面板变化”命令，与攻击同一套转发、去重、结果未知语义，结果随快照回写）；**第 4 项 = A（原型）**（Base 之间经 cellworld 的 NATS 传输每 1s 互发队员快照，收到的放进 mirror、用 GroupView 推给本进程队员；成员沿用 demo 的 `SetTeam`，成员所在 Base 按 Player 的静态 sid 归属推出，快照超过 3 个周期未更新从面板移除）；**E4 的做法确认**为场景桥 Manager 上的 `self` 绑定（`policy.Direct`），不打开 `access.player.self_sync`。

### 12.1 第 4 项：停下——`SetTeam` 的成员关系拿不到另一个 Base 上的另一半

> **已由 §13 实施**：维护者 2026-11-04 定 A（分区场景另设不开战斗的组队关系，成员表在 Remote 托管的 Team 实体上）。下面是当时停下的依据，保留作追溯。

维护者给的停止条件（“若发现成员关系拿不到对端 Base 上的另一半，先停下汇报”）成立，第 4 项没有写代码。依据（当前源码，`rg` 核对）：

- demo 的队伍只有一个来源：`matchmaker.go` 的 `formMatches` 在**组成队伍的那个进程**上调 `scene.SetTeam(members)`；
- 而 `formMatches` 先经 `ownedCandidates(owners, candidates)` 只留**本进程驻留**（`PlayerOwners.Resident`）的票：别的 sid 的票“stay in the queue for that sid's own pass”。注释写明这是有意的取舍——“What this gives up is a match across two processes. That needs a battle host the losing side's inputs are routed to, which the demo does not have”（锁步战斗房间开在组队的进程里）。

所以 demo 里**根本不会形成跨 Base 的队伍**：队员一定都在同一个 Base 上，而同一 Base 上的队员 E4 之后已经经 `SetTeam` 的关系视野互相可见（§11.4）。“Base 之间互发队员快照”在现有成员来源下没有可发的对象，端到端里“两个不同 Base 上的队友”也构造不出来。要做第 4 项，先要定“跨 Base 的队伍从哪来”：

- **A**：分区场景另设一种不开战斗房间的组队关系（例如“组队”协议：邀请 / 接受，成员表放在一个 Remote 托管的 Team 实体或队长的 Player 上），各 Base 从它得到成员与对端 sid；队伍面板走 §11.6 ④ A 的快照通道。改动：新协议、成员表的权威、离队 / 解散。
- **B**：放开 matchmaker 的“只配本进程的票”：匹配可以跨 Base，各成员的 Base 都要登记同一支队伍（`MatchFound` 推到各自进程时由收到的一方 `SetTeam`）；锁步战斗在跨进程时不开（或另找战斗宿主）。改动：matchmaker 与战斗的跨进程语义，影响普通场景。
- **C**：用公会代替队伍做“视野外成员面板”的原型（公会是 Remote 托管实体，成员天然跨进程）。改动最小，但语义不是“队伍”。

推荐 A（不动普通场景与匹配 / 战斗）；需要维护者决定。

### 12.2 第 3 项：实施结果

维护者 2026-11-04 定方案 A，按本节原先定稿的设计实施（[定稿原文](#1221-定稿的设计)保留在下面）。codebase-memory-mcp 本轮连不上，结构结论（调用方、实现方清单）按当前源码与 `rg` 得出。修前输出在 [cellworld-v24-20261104](../performance/benchmarks/cellworld-v24-20261104/)。

**改动**

| 层 | 内容 | 位置 |
| --- | --- | --- |
| cellworld 状态 | `Panel{MaxHP, Attack, Power, Ver}` 进 `EntityState`（`ActorState` 随之带上）：Ghost、Offer（交接带走）、witness、回写、Enter 都带；零值字段 = 没有给出（NPC 目前没有面板）。`WireVersion` 3 → 4（未上线，不兼容：升级须先停旧进程） | `wire.go` |
| 伤害 | 攻击者所在格（攻击者是那里的 Real）按攻击者面板的 `Attack` 定伤害值，没有面板时取 `Node.Attack(…, amount)` 的缺省值（demo 是 `scene.partition.attack_damage`）；转发给目标格的是定好的数值；`damageReply` / `attackResultMsg` / `AttackResult` 带实际伤害 `Damage`。扣血后按目标面板的 `MaxHP` 夹 | `combat.go` `onAttack` / `applyDamage` |
| 上限 | 当前 HP 不超过 `MaxHP`（0 = 无上限）：`install` 时（Enter 的快照 HP 可能高于上限）、应用面板时、扣血后夹；**面板变化只改上限、不回血**（暂定，可改） | `EntityState.capHP` |
| 面板命令 | `kindPanel`（Base → 当前 owner，锁 Scene + 角色，与 `kindDamage` 同一条 `hActor` 路径）：Real 只接受 `Ver` 更大的（同序号、更旧的答重复，`PanelsDuplicate`）；收到的格不是 Real 答“重试”与所知 owner（`PanelRetries`），Base 不按提示改投（没有 epoch 证据）。结论 `kindPanelResult` 带 Real 上现在的序号、HP、上限。幂等重进（Base 重启、看门狗误判，Real 还在）时 Enter 带来的更新面板同样按序号应用 | `combat.go` `onPanel` / `applyPanel`、`node.go` `onEnter` |
| Base 侧 | `BasePlayer.CellPanel()`：玩家养成输入现在推出的面板数值。`Node.UpdatePanel(p)` 在调用方的 Player 事务里（升级、换装的 handler）读它，数值变了才编下一个序号，写进 `BaseState.Panel`（经 `SetBaseState`：回滚即撤销），提交后发给当前 owner；`Node.RefreshPanel(ctx, player)` 是不在 Player 事务里时的同一件事（一次 Player 请求）。Enter 带当前面板建 SceneActor。确认：面板结论、owner 通知（新增 `PanelVer`）、witness 里的自己、回写都带回 Real 上的序号（`PanelAcked`，只前进）。未确认时 owner 变化立即补发，否则 Base 心跳每 `LeasePeriod` 重发；`PanelDeadline`（新配置，缺省 2s，须长于 `LeasePeriod`）内没确认就经 `Hooks.PanelResult` 报一次 Unknown（`PanelUnknown`：可能已应用，Base 照旧以同一序号重发，直到确认）。handler 里没有同步请求 | `base.go` |
| demo DAO | SceneActor / Monster：`CellMaxHP / CellAttack / CellPower / CellPanelVer`（nopersist,nosync，同名，一份映射）。Player：`CellMaxHP / CellAttack / CellPower`（persist,sync：SceneActor 的 mirror 用 PlayerDao 的同步编码，带 Real 上的面板）、`CellPanelVer / CellPanelAcked`（persist：序号跨 Base 重启单调；Real 已确认的序号随回写回来）、`CellPanelSentAt / CellPanelSince / CellPanelUnknown`（nopersist,nosync）。存储形状变化，未上线，不做旧数据兼容 | `db/def/{player,scene_actor,monster}.go`、`game/partition` |
| demo 运行时 | `Player.CellPanel()` = 属性 Final 层的 hp / attack / power；`AttributeComponent.LevelUp` / `RefreshGear` 之后在同一笔事务里调 `partition.Node().UpdatePanel`（不是分区进程时什么都不做）。mirror 编码加 `cell_max_hp / cell_attack / cell_power`；`AttackResultPush.Damage` 改为目标格真正结算的值；`gm.scene.partition` 的实体表与玩家状态带面板，计数加 `panels`（applied / duplicate / unknown）。Player 的 `self` 视图本来就不含属性表与 `cell_hp`（§11.4），本轮新增的面板字段也不进它：客户端的面板与当前 HP 只从自己 SceneActor 的 mirror 来 | `game/entities/player/{cell_base,attribute_component}.go`、`internal/service/game/partition.go` |

普通（非分区）场景：`partition.Node()` 为 nil，`cellPanelChanged` 不做任何事，新增的 Player 字段在普通场景里从不写（零值、不产生同步）；行为不变（§12.2 验证里的普通场景步与 demo 机器人）。没有新增带锁的进程级管理器。

**先红后绿**（cellworld 用例在 `combat_test.go`，崩溃重建在 `lifecycle_test.go`）。为了让每条承诺各自红一次，修前分两段跑：只有类型与 API 骨架时（[unit-red-before.txt](../performance/benchmarks/cellworld-v24-20261104/unit-red-before.txt)），以及只做了“Enter 带面板”之后（[unit-red-stage2.txt](../performance/benchmarks/cellworld-v24-20261104/unit-red-stage2.txt)）：

| 用例 | 钉住的承诺 | 修前（原文摘录） |
| --- | --- | --- |
| `TestDamageUsesTheAttackersPanel` | 本格与跨格转发的伤害都取攻击者面板的攻击（17），没有面板的攻击者取缺省值（3）；结论带实际伤害 | `attacker 1873 on 73557: damage 0, want 17`，HP 按缺省值 2 扣（98） |
| `TestPanelCapsCurrentHPWithoutHealing` | 进入时快照 HP 100 夹到上限 60；上限 60 → 40 夹到 40；40 → 200 不回血；挨打照常扣；结果随回写进快照（HP 31、已确认序号 3），客户端里自己的 mirror 带新面板 | 骨架：`entered with … panel {MaxHP:0 …} hp 100, want panel ver 1`；只有 Enter 时：`lower max hp: … panel {MaxHP:60 … Ver:1} hp 60, want panel ver 2` |
| `TestPanelCommandLostIsResent` | 面板命令全丢时 Real 停在旧版本、Base 未确认；恢复后重发、恰好应用一次 | 只有 Enter 时：`base panel {… Ver:1} acked 0, want ver 2 pending with ver 1 acked` |
| `TestPanelCommandDuplicatedOrReorderedAppliesOnce` | 命令全部重复、ver 2 晚到 300ms：每版至多应用一次、晚到的旧版不覆盖 ver 3、答重复 | 只有 Enter 时：`latest panel: … {MaxHP:100 Attack:3 … Ver:1}, want panel ver 3` |
| `TestPanelChangeDuringHandoff` | Offer 延迟 300ms、在旧格冻结时改面板：旧格答重试，Base 看到新 owner 后补发，新格应用；之后一次攻击的伤害是新攻击 13；交接后唯一 Real | `panel after the handoff: … Ver:1}, want panel ver 2` |
| `TestPanelUnknownWhenUnconfirmed` | 只丢结论：witness 里的自己带回序号，确认、不报未知；命令与结论都丢：超过 `PanelDeadline` 报 Unknown 恰好一次，Real 仍是旧版本；恢复后应用、确认，Unknown 不再报 | 只有 Enter 时：`timed out waiting for base acknowledges ver 2 from the witness` |
| `TestCellCrashPlayerReentersFromSnapshot`（加断言） | Cell 崩溃后按快照重建的 SceneActor 带 Base 的面板 | 骨架：`after restart: panel {MaxHP:0 …}, want {MaxHP:150 Attack:4 Power:23 Ver:1}` |

demo 单测（[demo-unit-red-before.txt](../performance/benchmarks/cellworld-v24-20261104/demo-unit-red-before.txt)：DAO 字段已加、映射与打包未改时）：`TestCellStateRoundTripsThroughBothDAOs` / `TestCellStateRollsBackWithTheTransaction` / `TestBaseStateWritesTheSnapshotPositionOnlyOnAWriteBack`（映射带面板与确认序号）、新增 `TestPendingPanelRollsBackWithTheTransaction`（升级那笔事务失败时待发面板随之回滚，修前 `pending panel not rolled back: {MaxHP:0 …} … want {MaxHP:110 Attack:12 Power:35 Ver:1}`）、`TestPartitionMirrorsDecodeAsPlayersAndMonsters`（修前 `cell_max_hp = <nil>, want 120`）、`TestAttackResultPushNamesTheVerdict`（修前推送伤害是配置的 10、want 14）、新增 `TestCellPanelIsTheFinalLayer`（修前 `built: panel {MaxHP:0 …}, final hp 100 attack 10 power 30`）。修后全部通过；cellworld 全包 `-race` 通过。

**端到端**（`scripts/test-seamless-demo.sh`，私有依赖：`scripts/mirror-local.sh` 偏移 34000，etcd 58379；同机 macOS，Apple M5）。新增一步 `panel_level_up`（追加检查，失败不中断后续步骤）：p1 在格 (1,0) 里 `AddExp 100`（升一级）→ 客户端从自己 SceneActor 的 mirror 看到 `cell_max_hp` / `cell_attack` 变大、`cell_hp` 不变（不回血）；自己 Player 的 self 视图里没有 `cell_hp / cell_max_hp / cell_attack / cell_power / attr_final / attr_base`；Base 的 `gm.scene.partition` 里 `panel_acked == panel_ver`；之后打一只附近的怪一次，推送的伤害与客户端看到的 HP 下降都是新攻击值。原有的攻击步改为按 p1 自己 mirror 上的 `cell_attack` 计算期望伤害（没有这个字段时取 `attack_damage`）。

| 轮 | 代码 | 结果 |
| --- | --- | --- |
| 修前（[e2e-red-before.txt](../performance/benchmarks/cellworld-v24-20261104/e2e-red-before.txt)） | 分支 HEAD `dfbd77a6` + 新的演练客户端与脚本 | 只有 `panel_level_up` 红：`before the level-up: cell_max_hp 0 cell_attack 10, want a panel with the level-0 attack 10`（mirror 里没有面板）；其余 11 步全绿（伤害是固定的 10，p2 崩溃前 HP 100 → 80） |
| 修后（[e2e-green.txt](../performance/benchmarks/cellworld-v24-20261104/e2e-green.txt)、[evidence.json](../performance/benchmarks/cellworld-v24-20261104/e2e-green-evidence.json)） | 本轮 | 12 步全部通过。`panel_level_up`：`cell_max_hp` 100 → 110、`cell_attack` 10 → 12、`cell_hp` 保持 100；Base 侧面板 ver 2 已确认（attack 12、max_hp 110、power 35）；升级后一击伤害 12、目标 HP 100 → 88。跨界打怪两轮仍是 applied 10 / duplicate 0、伤害 10（0 级的攻击恰好等于 `attack_damage`）；`cell_crash` 里 p1（已升级）打 p2 两下 100 → 76，按快照重建后 HP 76；`base_restart`、普通场景（`Move` 照旧、`MoveIntent` 得 `scene_not_partitioned`、demo 机器人在 sid 1003 跑一轮）通过 |

**不回归**（同一工作树、同机，`GOWORK=off`、从模块根）：gofmt 无输出；`go build ./... && go vet ./...`；glsvet（`./framework/nest/... ./framework/entity/... ./framework/dataengine/engine/... ./framework/sync/entitysync/... ./gameplay/cellworld/...`）无违例；`python3 scripts/check-doc-anchors.py` 0 broken；根包 `go test -count=1 .`；`go test -race -count=1 ./gameplay/cellworld/`（含 nats / process 子进程用例）；全量 `go test -count=1 ./...`（含 codegen）；`scripts/test-demo-generated.sh`（私有依赖，真实依赖用例全部 PASS、`roost id check` / `roost generate --check` 通过）、`test-mmorpg-generated.sh`、`test-sync-modes-generated.sh`、`test-genres-generated.py` 13 类全部 passed；新生成的 game-demo `go generate ./...` 后 porcelain 干净；本仓 `go generate ./...` 后 porcelain 干净；端到端见上。本轮没有改 nest / entity / sync 的代码（只动 cellworld 与 game-demo 模板），所以这几个包的 `-race` 没有重跑，它们的普通测试在全量里通过。

**剩余缺口**

| # | 缺口 | 性质 |
| --- | --- | --- |
| v2.4-1 | 面板变化不回血（上限变大时当前 HP 不变）、上限变小直接夹：暂定，产品可改（例如升级回满、按比例缩放） | 需要产品确认 |
| v2.4-2 | NPC 没有面板（`attack_damage` 缺省伤害对怪物之间不适用：怪不攻击）；Monster DAO 字段已备好，怪的面板来源（刷怪表 / 模板）没接 | 原型简化 |
| v2.4-3 | buff 层：面板里只有 Base 推出的 Final 层；Cell 上自己的加成（buff、场景效果）还没有，将来在 Real 上把 Base 的面板当输入、在 Cell 上重算 Final | 后续 |
| v2.4-4 | “结果未知”之后 Base 无限期以同一序号重发（面板是状态，最新一份覆盖即可，不像攻击那样放弃）：owner 长期不可达时每 `LeasePeriod` 一条消息，直到看门狗重进（Enter 带最新面板）或玩家离开 | 原型简化 |
| v2.4-5 | 普通场景的 Player 也多了 8 个 `Cell*` 面板字段（5 个持久，普通场景里从不写） | 原型简化（与 v2.2 的 `CellHP` 同理） |

#### 12.2.1 定稿的设计

（2026-11-04 写定、本轮照此实施；与实施的差别：上限的夹法写成“只降不升”的一个函数，Base 侧的确认多了 owner 通知与 witness 两条来源，结果未知后继续重发。）

1. **cellworld 状态**：`EntityState` / `ActorState` 增加面板 `Panel{MaxHP, Attack, Power, Ver}`（Ver 是 Base 侧面板序号，接收端只接受更新的版本，与攻击序号同一种去重）；Offer / Ghost / witness / 回写都带它；`onDamage` 的伤害改为攻击者面板的 `Attack`（`scene.partition.attack_damage` 留作面板缺省），扣血后按 `MaxHP` 夹当前 HP；交接带走面板。
2. **命令消息**：`kindPanel`（Base → 当前 owner 的 Real），走 `Node.Attack` 同一条路径：Player 上的一次 Nest 事务里把“想要的面板 + 序号”写进 `BaseState`（`PanelSeq` / `Panel`），提交后发给 owner；owner 变化、超时未确认由 Base 心跳重发；Real 上按序号幂等应用（同序号再到算 duplicate），结论（applied / duplicate / unknown）回给 Base，随下一次回写进快照（`CellPanel*` 持久字段）。上限变小时当前 HP 夹到新上限；升级是否回血按维护者 A 的字面“面板变化”只改上限，不回血（实施时在 §12 记下，若需回血再定）。
3. **demo**：分区模式下 `AttributeComponent.LevelUp` / `RefreshGear` 之后，在同一笔 Player 事务里经 `partition` 把新面板（Final 层的 hp / attack / power）写进 BaseState 的待发面板；不能在 handler 里同步请求 Node（快池不阻塞），由提交后回调 / Base 心跳发送。分区模式下 Player 的 `AttrFinal` 不再进自身视图（面板从 SceneActor 的 mirror 来：mirror 编码加 `cell_max_hp` / `cell_attack`）；普通场景不变。
4. **测试**：cellworld 用例（面板命令丢失 / 重复 / 交接中 / 结果未知、伤害按攻击者面板、上限夹血）先红后绿；demo 单测（映射、待发面板随事务回滚）；端到端：p1 `AddExp` 升级后打怪一次的伤害从 10 变成新攻击值、自己 SceneActor 的 `cell_max_hp` 变大且 Player 自身视图里不再有另一份 HP。

### 12.3 E4

做法确认，§11.4 不变。
