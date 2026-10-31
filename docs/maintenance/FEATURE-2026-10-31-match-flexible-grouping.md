# 匹配：弹性人数成局与中途补位（方案）

来源：派对房间纵切（[PARTY-ROOM-INTEGRATION §8](../framework/PARTY-ROOM-INTEGRATION.md#8-框架没有什么)）发现 `service/match` 的 `Queue.GroupSize` 固定，没有“够最少 M 人、等待超过 T 就开局”的成局策略，也没有已开局房间的中途补位。派对、休闲、房间合作类都要用。维护者 2026-10-31“继续框架改进”。

本轮实施第一阶段（弹性成局）；第二阶段（补位）维护者 2026-10-31 选方案 B（业务层补位、零框架改动），已写进[派对手册 §6A](../framework/PARTY-ROOM-INTEGRATION.md#6a-补位业务层写法)与派对夹具；方案 A 留作以后。

## 1. 现状（2026-10-31 源码，v1.33.2）

- 一个分区（`Queue{Mode, GroupSize, Partition}`）的全部状态是一个版本化值，`Enqueue` / `Cancel` / `Commit` / `Sweep` 各是一次比较并设置；Redis 存储就是 `versionstore.RedisStore` 的通用 CAS 脚本，匹配规则全在 Go 的 mutate 里，内存与 Redis 两种存储跑的是同一段代码。
- 成组由调用方做：`Candidates` → `Grouping.Group` → `Commit`。存储只保证票集合合法、一次成局原子、同一票集合重放返回原局、部分重叠拒绝。
- `Commit` 要求票数恰好等于 `GroupSize`（`checkTicketSet`）。

## 2. 第一阶段：弹性人数成局（本轮实施）

**队列声明**。`Queue` 增加两个字段，与 `GroupSize` 一样是队列身份的一部分（进 `Queue.Key()` 与 `LogicalKey()`，不同弹性规则的队列是不同的池，票永不混局）：

| 字段 | 含义 |
| --- | --- |
| `GroupSize` | 一局人数**上限**（即 MaxSize）。不另加 `MaxSize` 字段：同一事实两种写法要多一条“两者必须相等”的校验 |
| `MinSize` | 0 = 固定人数队列（行为与现在完全相同，键名不变）；非 0 时 `2 ≤ MinSize < GroupSize` |
| `FillAfterSeconds` | 弹性队列里，一组中最早的票等待满这么多秒后，`MinSize..GroupSize-1` 人也可以成局；0 表示够 `MinSize` 就可以开。整数秒、单位写在名字里（票时间按 Unix 秒记；`Queue` 走总线，`time.Duration` 在线上是纳秒整数，servicerpc 生成器拒绝它），不能为负，固定队列必须为 0（保证一个队列只有一种写法）；存储要求它小于票期限 `match.ticket_ttl`，否则小局永远等不到 |

**唯一成局规则** `Queue.FormableSize(available, waitedSeconds)`：可用票 ≥ `GroupSize` 时取 `GroupSize`（立即成满局）；否则弹性队列在 `available ≥ MinSize` 且 `waited ≥ FillAfterSeconds` 时取全部 `available`；其余返回 0（还不能成局）。`Grouping` 用它决定取几张，`Commit` / `CommitAcross` 用它校验——规则声明一次，策略与存储不会各说各的。

**存储强制**。`Commit` / `CommitAcross` 接受 `MinSize..GroupSize` 张票（固定队列仍是恰好 `GroupSize`）；不满 `GroupSize` 时，在同一次比较并设置里按存储时钟（`Config.Now`，Mod 注入业务时钟）计算这组票里最早一张的等待时间，`FormableSize(n, waited) != n` 时报新错误 `ErrGroupNotReady`（550110），状态不变。重放判定在这一步之前：已成的局在时钟回拨后重放仍返回原局。并发：不同大小、互相重叠的票集合争同一批票时仍由 CAS 保证只有一个成功，其余报 `ErrConflict`；同一票集合的重放返回同一局；“分区约 4500 人”的容量建议不变（弹性队列不改变每名成局玩家的状态大小）。

**策略**。`FirstComeGrouping` 增加可选的 `NowUnix`（弹性队列必须给，固定队列不用，零值 `FirstComeGrouping{}` 照旧可用）；`ScoreWindowGrouping` 按同一规则在窗口内的票里取 `FormableSize` 张。两者都以最早一张候选票的等待时间判定（候选按队列顺序，最早的票一定在所选集合里，存储按集合里最早一张算，所以策略判定能成的局存储一定接受，除非两边时钟不一致——那时存储报 `ErrGroupNotReady`，匹配器下一轮再试）。

**时钟**。等待时间读业务时钟：存储是 `Config.Now`，策略是 `NowUnix`，都按 Unix 秒取整，与票期限同一精度（判定最多提前不到 1 秒）。时钟回拨时等待时间按 0 计（不会因回拨让小局提前开）。

**配置**。队列在代码里声明（与 `GroupSize` 一样），Mod 没有新键；唯一涉及配置的是 `match.sweep_queues` 条目的人数段：`mode:8[:partition]`（固定）或 `mode:2-8@20s[:partition]`（弹性：MinSize-GroupSize@FillAfterSeconds），写法与 `Queue.LogicalKey()` 相同，运维可以照 Redis 键抄。

**RPC**。`Queue` 是 JSON 线格式，两个新字段（`min_size`、`fill_after_seconds`）`omitempty`：固定队列的线格式与存储格式字节不变；弹性队列需要服务端是新版本（旧服务端会忽略这两个字段、按固定队列处理——尚未上线，不做兼容，CHANGELOG 写明）。

**验证**。先红后绿：未到阈值不成小局（状态不变）、到阈值按 ≥MinSize 成局、到 GroupSize 立即成局、并发不同子集只成一局、幂等重放（含回拨后）、业务时钟前拨与回拨、`CommitAcross` 同规则、校验与键名、`sweep_queues` 解析；真实 Redis 上同一组用例（`wiring/integration`，私有端口实例）。派对房间夹具改用 2～8 人、20 秒开局的弹性队列。

## 3. 第二阶段：中途补位（backfill，待维护者选择）

**决定（维护者 2026-10-31）**：选 B。落地：[派对手册 §6A](../framework/PARTY-ROOM-INTEGRATION.md#6a-补位业务层写法)（流程、不超员的理由、幂等身份、截止与撤票、局限）与 `scripts/testdata/party-room` 的 `backfill.go.txt` / `backfill_match.go.txt` 及四个补位场景。与下表 B 列的差别：房间票的排队身份是“房间 ID + 补位轮次”，下一轮只在本轮被消耗或撤回后才开；截止由房间状态判定（本例第 1 回合结束），匹配循环不给过了截止的房间票配人，宿主撤票，已被消耗的在途补位局进房时全部拒绝。A 列四件事留待真要做 A 时再定。

需求：已开局的房间有空座（有人离开、或弹性开局不满员），后来的玩家优先补进已开的局，而不是另开新局。

要回答的问题与两个方向：

| | A 存储原生补位（推荐，若补位是常用路径） | B 业务层补位（零框架改动，现在就能写） |
| --- | --- | --- |
| 做法 | 新增 `OpenSlots(queue, matchID, n, until)` / `Backfill(queue, matchID, ticketIDs)` / `CloseSlots(queue, matchID)`。比赛记录增加空位数、截止时间与补位批次；`Backfill` 在同一分区的一次 CAS 里校验空位 ≥ 张数、未过截止、票在等待，把票标成已匹配到这一局、追加成员、扣空位 | 房间宿主把“空座”当成一张票排进单独的补位队列（例如主体 Kind=`room`，Score/Payload 记空位数），匹配器从补位队列取一张房间票 + 若干玩家票用弹性 `Commit` 成“补位局”，房间按局里的玩家开门 |
| 不超员 | 存储保证（空位计数在同一 CAS 里扣） | 房间票一次只能被一个 Commit 消耗，所以同一时刻一个房间只有一批补位；人数由策略按房间票的空位数取，存储不校验，多出的人由房间的进房 handler 拒绝并回到排队 |
| 通知 | 补进的票照常变成 matched、`MatchID` 是原局，玩家轮询 `Ticket` 的写法不变；房间经匹配器拿到 `Backfill` 返回的新成员（与成局后开房同一条路），或轮询 `Match` | 与普通成局相同 |
| 生命周期 | 开着空位的比赛不能按 `Retention` 从成局时刻剪掉：保留到空位关闭 / 截止后再满 `Retention`；房间结束要 `CloseSlots`，否则留到截止。长时间开放补位的局一直占分区状态（计入约 4500 人的容量建议） | 房间票有票期限，过期即不再补位；房间结束时 `Cancel` 自己的房间票 |
| 要定的事 | ①原 Commit 的重放身份：成员增加后原票集合不再等于 `TicketIDs`，要另存首批票集合；②跨分区成局的比赛是否允许补位（每个分区一份副本都要改，建议只允许单分区）；③补位与新开局的优先级放在存储（`Candidates` 前先列开放空位）还是匹配器；④截止与房间结束谁负责关闭 | 是否把这种写法写进手册作为推荐，框架不提供 API |
| 工作量 | 存储与 RPC 各 3 个方法、比赛格式加字段、剪除规则改动、两种存储的并发 / 截止 / 清理测试，约一轮 | 夹具与手册各一节 |

建议：先按 B 在派对手册写清写法（它只依赖本轮的弹性成局）；若后续多个玩法都要补位、需要“不超员”由框架保证，再做 A，并在开工前由维护者定上表 A 列的四件事。
