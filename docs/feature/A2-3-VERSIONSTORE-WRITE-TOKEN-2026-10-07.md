# A2 ③：versionstore 写入带一次性令牌（2026-10-07）

依据：[DECISIONS-PENDING-2026-10-05](../review/DECISIONS-PENDING-2026-10-05.md) A2 行的 ③，第十三轮“原‘下个大版本’项”——维护者：“还有留到下个版本的几项在本机能完成吗？希望本次能完成了”。
A2 ① ② 已实施（`cf5721c9`，[驱动重放契约](A2-DRIVER-REPLAY-CONTRACT-2026-10-05.md)）：驱动不重放写命令，结果未知原样交给调用方。来由 NC-21、NC-52、NC-100、NC-101、NC-160；NC-100 / NC-101 复审报告建议 ③“信封里带一次性写令牌，回复丢失后像 versioned lock 那样认领自己的写”。

线上还没部署，**不做旧格式兼容**：持久格式改变，升级需清空 versionstore 的键（见 §7）。

## 1. 问题

`RedisStore` 的写是一条 Lua（compare-and-set，比较读到的整条信封字节）。回复丢失（读超时、发出后连接断开）时，脚本可能已经执行：

- `Update`：store 把错误原样返回；调用方再调一次 `Update`，mutate 叠在自己那次已生效的写上，**重复生效**（RR-20261005-NC-100 修前驱动重放就是这样写两次的；现在驱动不重放，换成调用方重放，后果一样）。
- `Create`：调用方重试时键已经被自己建好，返回“没建”（`created=false`），**把自己的写判成冲突**。
- `Delete` / `DeleteIf`：重试时键已经没了，返回 `ErrVersionMismatch`，**判成冲突**。

调用方手里只有一个传输错误，判断不了上一次写入其实已经生效。

## 2. 方案：令牌放在信封里，跟值同一次 SET

### 2.1 持久格式

```
旧：<version>\n<payload>
新：<version>|<token_v>|<token_v-1>|…|<token_v-n+1>\n<payload>
```

- 令牌：每次写（每一条 compare-and-set 命令）由 store 生成，8 字节随机数的 base64url，11 个字符，字母表不含 `|` 与换行。
- 令牌列表是**这个键最近 n 次写的令牌，新的在前**：第 i 个（从 0 数）是写出 `version - i` 的那一次。版本每次写加 1，所以位置就是版本，不必另存。
- 下一次写的信封由 Go 拼出：`[新令牌] + 读到的列表的前 n-1 个`。脚本比较的是读到的整条字节，比较通过就说明列表正是读到的那份，所以**不改 Lua**：令牌和值在同一个 key、同一条 `SET` / `PSETEX`、同一个脚本里原子写入；带索引的写，索引条目也在同一个脚本里（与原来相同）。
- 解码严格：头部没有令牌（旧格式 `"<version>\n"`）、令牌为空、令牌多于版本号，都是 `ErrMalformedRecord`。

### 2.2 保留多久、如何有界

- **按次数保留，不按时间**：每个键只留最近 `WriteTokenHistory` 个令牌（`RedisConfig.WriteTokenHistory`，0 取缺省 `DefaultWriteTokenHistory = 8`）。每键额外 ≤ 8 × 12 字节 ≈ 96 字节，与键一起删除、一起过期，不新增键、不需要 TTL、不需要清理任务。
- **缺省 8 的推导**：令牌要覆盖的是“我的写落地之后、我去核对之前，这个键上又落了几次写”。store 设计上承受的单键竞争就是 `DefaultMaxAttempts = 8`：一次 `Update` 输 8 次就报 `ErrConflict`，判为真竞争。取同一个数，竞争在设计范围内时核对都能认出自己。
- **超出保留范围不会判错**：令牌被挤出去之后，核对得不出结论，返回“结果未知”（§3），即今天的行为。保留个数只影响“能认出的比例”，不影响正确性；所以缺省值不是产品选择，热点键可以单独调大。

### 2.3 Redis Cluster

令牌就在值的 key 里，没有新键，槽位不变。带索引的 store 原来就要求值 key 与索引 key 同槽（`RedisConfig.Index` 注释、`CompareAndSetCommand.Index`），本方案不改变这条。

## 3. 判定：结果未知之后怎么认

一次写命令（下称 P）发出后得到传输错误，P 可能已经执行。store 先 **GET 一次**（读命令，驱动照常重试），按读到的当前值 C 判定：

| 当前值 C | 判定 | 动作 |
| --- | --- | --- |
| C 的令牌列表里有 P 的令牌（位置对应 P 要写的版本） | P 已生效 | 返回 P 写的值与版本，`applied=true` |
| P 有基准值 B（键原本存在），C 与 B 字节相同 | P 没执行（之后也不可能落在别的值上：比较的是整条字节） | 原样重发 P（同一令牌、同一期望值） |
| P 有基准值 B，C 的令牌列表在 B 的版本位置上正是 B 的最新令牌（C 由 B 演进而来），且没有 P 的令牌 | P 没生效：B 之后的那个版本是别人写的 | `Update` 当作比输了，重读、重跑 mutate；`Delete` 返回 `ErrVersionMismatch` |
| 其他：C 不存在；C 与 B 不在一条演进链上（中间被删过 / 过期）；B 或 P 的版本已被挤出令牌列表；P 没有基准值（键原本不存在）而 C 里没有 P 的令牌 | 无法证明 | 返回结果未知 |

要点：

- **只在能证明时下结论**。“键不存在”没有身份：P 以“键不存在”为基准（`Create`、对不存在键的 `Update`）而此刻键仍不存在时，分不清“P 没执行”与“P 执行了、随后被别人删掉”，这时**不重发**（重发会把别人删掉的东西再建出来），返回结果未知。`Delete` 不留下任何东西，删掉之后同样无从核对。见 §8。
- 重发只在 C 与 B 字节相同时发生。B 的字节含有随机令牌，删掉重建不可能得到同样的字节，所以重发不会落在别的值上；被延迟的原命令与重发至多一个生效。
- 结果未知时的核对与重发都在**同一次调用里、用调用方的 ctx**：GET 失败（后端不可达）立即停止，返回结果未知；重发次数与 compare-and-set 尝试共用 `MaxAttempts` 预算。
- 不是“回复丢失”的错误原样返回，与改前相同、不核对不重发：服务端的错误回复（带 `RedisError()`：脚本报错、`WRONGTYPE`、`LOADING` 等——回复到了，结果就是这个错误）、拨号失败（`*net.OpError` Op=="dial"，命令没发出）、`fredis.ErrCASInvalidCommand`（发出前就拒绝）。`ErrPoolTimeout` / `ErrClosed` 这类 go-redis 哨兵 versionstore 认不出（不能 import 驱动），保守按回复丢失处理：核对用的 GET 同样会失败，立即返回结果未知。

## 4. API

`Store` 接口**不变**，现有调用方不改一行就得到 §3 的核对。新增：

```go
var ErrOutcomeUnknown = errors.New("versionstore: write outcome unknown")
var ErrWriteTokenMismatch = errors.New("versionstore: write token reused for a different write")

// 核对之后仍无法证明时返回；errors.Is(err, ErrOutcomeUnknown) 与 errors.Is(err, <传输错误>) 都成立。
type UnknownOutcomeError struct {
    Key   string // 渲染后的 Redis key
    Token string // 这次写的令牌
    Err   error  // 最后一次传输错误
    // 未导出：这条写命令本身（基准字节、新信封、版本、索引），供 Resume 原样重发
}

// Resume 把上一次调用返回的结果未知交给下一次调用：同一个键、同一种写，store 先核对上一次
// 的令牌——已生效就直接返回上一次的结果，确定没生效才重新执行。err 不是结果未知时原样返回 ctx。
func Resume(ctx context.Context, err error) context.Context

type RedisConfig[K, T] struct {
    …
    WriteTokenHistory int // 每个键保留的令牌个数，0 取 DefaultWriteTokenHistory（8）
}
```

调用方要在 store 之外再重试时（例如 ctx 到期后由上层再调一次），只需一行：

```go
result, applied, err := store.Update(ctx, key, mutate)
if errors.Is(err, versionstore.ErrOutcomeUnknown) {
    result, applied, err = store.Update(versionstore.Resume(ctx, err), key, mutate)
}
```

令牌由 store 生成、随错误携带、重试时自动复用，调用方不接触令牌本身。Resume 的防误用：

- **同一个令牌、不同的值必须报错**：`Create` 重试时值的编码与上一次不同；`Update` 重试时把 mutate 作用在上一次的基准值上，得到的编码与上一次不同（mutate 本来就要求是纯函数）或不再保存；`Delete` 重试时持有的版本与上一次不同——都返回 `ErrWriteTokenMismatch`，不写。
- 同一个键上换了写的种类（上一次是 `Create`，这次是 `Update`）同样是 `ErrWriteTokenMismatch`。
- 别的键上的写忽略这份 Resume（上层一次操作可能先写别的键）。
- 只在进程内有效：`UnknownOutcomeError` 持有的是这条写命令本身，不能跨进程传递。跨进程 / 跨 RPC 的重试仍按原来的方式，由值里的请求 ID 去重（chat `RequestID`、mail Sends 账本、session 请求账本等）。

`MemoryStore` 不会产生结果未知，忽略 Resume。

指标：新增 `versionstore.unknown_outcome.total{store, result=applied|lost|unresolved}`（导出名 `versionstore_unknown_outcome_total`），一次结果未知被核对成什么各计一次。`versionstore.cas.total` 的口径不变（核对出“已生效 / 没生效”仍按 applied / lost 计）。

## 5. 各写入口的影响

| 入口 | 变化 |
| --- | --- |
| `Create` | 新信封（版本 1，令牌列表只有自己）。结果未知时 GET 核对：认出自己的令牌返回 `created=true`；否则结果未知（键原本不存在，见 §3）。 |
| `Update` | 每次 compare-and-set 一个新令牌，列表承接读到的那份。结果未知时按 §3：已生效直接返回、没执行原样重发、别人先写了当作比输。 |
| `Delete` / `DeleteIf` | 脚本不变（仍比较整条字节再 DEL）。结果未知时：键还是原值就原样重发；键已由原值演进（别人改过）返回 `ErrVersionMismatch`；键不存在返回结果未知。 |
| compare-and-set（`fredis.CompareAndSet` / `CompareAndDelete`） | 不改。它们比较的是调用方给的字节，versionstore 的令牌在字节里；其他用户（`app` 单实例锁与业务时间经 `kit/redis.SingletonStore`、rank 自己的交换脚本）不用 versionstore 信封，不受影响，它们各自的未知结果处理见 A2 驱动契约的调用方核对表。 |
| 二级索引（`RedisIndex`） | 索引条目仍与值在同一个脚本里、只在比较通过时移动，重发是同一条命令，所以索引与令牌同进退。`IndexDue*` 只读；`IndexRemove*`（ZREM）、`IndexRemoveIfAbsent`（EXISTS + ZREM）、`IndexDefer*`（只改已在的分数）都是幂等的，重复执行结果相同，不需要令牌。 |
| `Get` | 解码新信封；旧格式报 `ErrMalformedRecord`。 |

## 6. 调用方（`rg versionstore`，2026-10-07 当前源码）

全部经 `Store` 接口或 `*RedisStore`，没有直接读写信封字节的代码（`rg '"[0-9]+\\n'` 只命中 rank 自己的环格式，与 versionstore 无关），也没有在传输错误上自行重写一次的调用方（A2 驱动契约 §调用方核对）。所以**调用方不改代码**；它们拿到的变化只有：回复丢失的写多数在 store 内被核对成确定结果，剩下的错误多一层 `ErrOutcomeUnknown`（原来的传输错误仍可 `errors.Is`）。

| 包 | store（键空间，前缀见各 `redis_store.go`） | 写入口 |
| --- | --- | --- |
| `kit/service/account` | Accounts / Roles / Servers / Slots | Create、Update、DeleteIf |
| `kit/service/chat` | 频道状态 | Update（带 `RequestID` 去重环） |
| `kit/service/directory` | 目录条目（带 TTL） | Update、DeleteIf |
| `kit/service/global` | Routes | Create、Update、Delete |
| `kit/service/global/activity` | Activities / Participants / Ledger / Audits / Dispatches（按 owner 索引）/ Windows | Create、Update、Delete、`IndexDueIn` |
| `kit/service/platform` | Orders（固定索引） | Create、Update、Delete、`IndexDue` / `IndexRemoveIfAbsent` / `IndexDefer` |
| `kit/service/rank` | 不用信封，只调 `CountCompareAndSet` / `CountConflict` | — |
| `service/mail`（`kit/service/mail` 是测试 harness） | Mailboxes / Sends | Create、Update、Delete |
| `service/match`（`kit/service/match` 是别名） | 队列状态 | Update |
| `service/session`（`kit/service/session` 是别名） | Runs / Claims（`DeleteIf` 身份围栏）/ 请求账本 | Create、Update、Delete、DeleteIf |
| `demo` 模板 `activity_test.go.tmpl` | 测试里用 MemoryStore | — |

`kit/service/integration` 的 `everyNamespace` 不用改：没有新增键空间，令牌在原来的值 key 里。生成模板没有直接用信封，codegen 不受影响。

## 7. 不兼容与升级

- 持久格式改变：新版本读旧信封报 `ErrMalformedRecord`，旧版本读新信封同样报错（版本号行解析失败）。**升级需清空** versionstore 的全部键（上表各 store 的前缀；带索引的 store 连同索引有序集合），不支持新旧进程混跑。线上未部署，维护者决定不做兼容。
- 行为：回复丢失的写多数变成确定结果；同一次调用里可能多出一次 GET 和至多 `MaxAttempts` 次重发（受调用方 ctx 约束）。

## 8. 没有覆盖、需要维护者决定的部分

“键不存在”与“删除”不留下身份，§3 对两种情况只能返回结果未知：

1. `Create`（或对不存在键的 `Update`）回复丢失、核对时键不存在——P 没执行，还是执行后被别人删掉？
2. `Delete` / `DeleteIf` 回复丢失、核对时键不存在——是我删的，还是别人删的？

要让这两种也能认出来，删除就不能真的删：改成写一个**墓碑**（版本继续递增、带删除者令牌、读者当作不存在），墓碑保留一段时间后才真正消失。这会带来一个无法从现有约束推出的产品选择——**墓碑保留期**：store 内的核对只隔一个往返，但 `Resume` 的重试间隔由调用方决定，没有上界可依；保留期内同一键的空间不释放（session claim、directory 条目、activity 参与者等删除频繁的 store 会持续占内存），保留期之后照样无法核对；另外 `Get` / `IndexRemoveIfAbsent` 的 EXISTS / TTL store 的过期语义都要改成认墓碑，`Create` 的“键不存在”比较要改成比较墓碑字节。

| 选项 | 做法 | 代价 |
| --- | --- | --- |
| A（推荐） | 保持本次实现：这两种返回 `ErrOutcomeUnknown`，由调用方按请求 ID 去重或回读裁决（与 A2 ② 之后相同） | 无额外存储；这两种情况仍需调用方自己处理。仓内没有在传输错误上重试 Create / Delete 的调用方（A2 调用方核对表），跨 RPC 的重试已由值里的请求 ID 去重 |
| B | 墓碑：`Delete` 写墓碑（TTL = 保留期 R），读者与 `IndexRemoveIfAbsent` 当作不存在，`Create` 比较墓碑字节、版本跨删除继续递增 | 需要定 R 的缺省值（建议若选 B 取 1 分钟：长于框架内最长的 RPC 调用超时，`Resume` 在一次请求的重试窗口内可核对）；删除频繁的 store 内存放大约“每秒删除数 × R × 每键约 100 字节”；持久格式再加一种记录 |

**本次按 A 实施，B 等维护者决定**；选 B 时持久格式还会再变一次（仍可在发版前完成，线上未部署）。

## 9. 验证

- 替身先行（roost-bugfix lessons“比被测实现更正确的替身会把缺陷藏起来”）：`fakeRedis` 的 compare-and-set 是逐字节比较，与 Lua 一致；本次先给替身加“执行之后丢回复”“没执行就断开”“核对时读不到”三个注入点，在**修前实现**上跑新用例确认变红，再改实现。实施中又发现替身对 NaN 分数比 Redis 宽松（RR-20261006-35），同样先改忠实再修。
- 红绿用例（`versionstore/write_token_promises_test.go`）：`Update` / `Create` 回复丢失后调用方重试；回复丢失后别人又写一次；命令没执行时原样重发；结果未知之后 `Resume` 返回上一次的结果；上一次确定没生效时 `Resume` 照常执行；同一令牌不同的值报 `ErrWriteTokenMismatch`；令牌被挤出时返回结果未知不猜；键不存在 / Delete 的边界；信封令牌个数有界；旧格式报 `ErrMalformedRecord`（`malformed_promises_test.go`）。
- 真实 Redis（`write_token_integration_test.go`）：单机用本用例自建的 toxiproxy 代理丢回复（沿用 `lost_reply_integration_test.go`），Cluster 用 `scripts/mirror-local.sh` 私有 3 主 3 从、在 go-redis ProcessHook 里让脚本执行后丢回复（带固定索引、hash tag 同槽）。

## 10. 实施状态

已实施（`6b3a0eb9`，分支 `a2t`），未发版。

- 代码：`versionstore/write_token.go`（令牌、信封头、判定、`settle`、`Resume`、`UnknownOutcomeError`、指标）；`versionstore/redis_store.go`（`Create` / `Update` / `DeleteIf` 走 `settle`，`WriteTokenHistory`，严格解码）；`versionstore/versionstore.go`（契约注释）；`redis/cas.go`（RR-20261006-35）。Lua 未改。调用方未改。
- 红（修前 `66d72a33`，替身与真实 Redis 单机 / Cluster 都红）：

```text
--- FAIL: TestAnUpdateWhoseReplyIsLostIsNotAppliedTwiceWhenTheCallerRetries
    update returned {Value:{Name:a Total:2} Version:3} applied=true err=<nil>; store holds {Value:{Name:a Total:2} Version:3} (want total 1 at v2)
--- FAIL: TestACreateWhoseReplyIsLostIsNotReportedAsTakenWhenTheCallerRetries
    a create whose reply was lost was reported as created=false {Value:{Name: Total:0} Version:0} err=<nil>; it is this caller's own write
--- FAIL: TestAnUpdateWhoseReplyIsLostStaysAppliedOnceAfterSomeoneElseWritesOnTop
    update returned {Value:{Name:a Total:102} Version:4} applied=true err=<nil>; store holds {Value:{Name:a Total:102} Version:4} (want this write at v2 total 1, store total 101 at v3)
--- FAIL: TestRealRedisAWriteWhoseReplyIsLostIsRecognisedByItsToken/update
    lost reply: update returned [hello msg-1 msg-1] / v3 applied=true err=<nil>; Redis holds [hello msg-1 msg-1] / v3 (want msg-1 once at v2)
--- FAIL: TestRealRedisAWriteWhoseReplyIsLostIsRecognisedByItsToken/create
    lost reply: create returned created=false [] / v0 err=<nil>; it is this caller's own write
--- FAIL: TestRealRedisClusterAWriteWhoseReplyIsLostIsRecognisedByItsToken/update   （同上文本）
--- FAIL: TestRealRedisClusterAWriteWhoseReplyIsLostIsRecognisedByItsToken/create   （同上文本）
```

  `Resume` / `ErrWriteTokenMismatch` 是新 API：它们的红就是上面“朴素重试”的同一场景；防误用的检查用变异确认（去掉值比较后 `TestResumeRefusesTheSameTokenForADifferentWrite` 报 `create resumed with a different value returned <nil>`）。
- 绿：上面全部通过；`lost_reply_integration_test.go` 的 NC-100 用例现在打印 `lost reply surfaced as <nil>; Redis holds [hello msg-1] / v2`（store 内核对成已生效）。
- 验证命令（`GOWORK=off`）：

```text
gofmt -l（改动文件）                                                        空
go vet ./redis/ ./versionstore/；go vet -tags integration ./redis/ ./versionstore/   ok
go test -race -count=3 ./redis/ ./versionstore/                             ok
source <私有 mirror-local env.sh>; go test -tags integration -count=1 -p 1 ./redis/ ./versionstore/   ok（单机 toxiproxy + Cluster 3 主 3 从）
REDIS_ADDR / ROOST_REVIEW_REDIS / ROOST_REDIS_TEST_ADDR=<私有单机>，ROOST_REVIEW_CLUSTER=<私有 Cluster>
  go test -tags integration -count=1 -p 1 -v ./kit/service/... ./service/...   15 个包 ok，0 SKIP
go test -count=1 ./kit/service/... ./service/... ./demo/...                  ok
go build ./... && go vet ./...                                               ok
go test -count=1 .                                                           ok（根包门禁）
```

  没有改 nest / entity / dataengine / sync，没跑 glsvet；生成模板未改，codegen / game-demo 不受影响（模板与生成工程不直接读写信封，`activity_test.go.tmpl` 用 MemoryStore）。
- 未完成：§8 的墓碑（选项 B）等维护者决定。

## 2026-10-08 实施更正（B3）

- `Update` 的 CAS 竞争与传输重发现在共用 `MaxAttempts` 次写命令预算，原来的最坏平方次数已修正；读回次数不算写命令预算，仍受 context 期限约束。
- `Resume` 确认 Update applied/lost 也计 CAS 指标；unknown_outcome 每次核对调用计结果，首次 unresolved、稍后显式 Resume applied 是两次核对，并非一个全局唯一故障计数。
- §6 调用方表更正：directory 的 Redis 存储拒绝 TTL（预留期是值内逻辑期限）；global 和 mail 没有 Delete 调用；activity/platform 的 Store 接口有 Delete，但生产服务没有调用；chat RequestID 回执独立于消息 ring 存放。
- `ErrAborted` 保留为弃用的公开符号；mutate 不保存时返回当前值和 nil。
- 跨 RPC 不携带进程内 Resume。global 完成回执、match 相同票集合回执、session admission 恢复分别补齐业务重试/回收边界；不能把服务里未调用 Resume 等同于所有操作都重复生效。
