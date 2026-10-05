# A2：驱动的重放与超时语义对齐“结果未知交给调用方”（2026-10-05）

依据：[DECISIONS-PENDING-2026-10-05](../review/DECISIONS-PENDING-2026-10-05.md) A2 行。维护者在第二轮决定中选定 ① + ②，③（versionstore 一次性写令牌）暂不做。
背景链：[RR-20261004-NC-21](../bugfix/RR-20261004-NC-21.md)、[NC-52](../bugfix/RR-20261005-NC-52.md)、[NC-100](../bugfix/RR-20261005-NC-100.md)、[NC-101](../bugfix/RR-20261005-NC-101.md)、[NC-160](../bugfix/RR-20261005-NC-160.md)，方向判断见 [REVIEW-2026-10-05-n04-revn04](../review/REVIEW-2026-10-05-n04-revn04.md#方向判断)。

## 目标

1. 写出驱动行为契约表，回答四个问题：驱动会重放哪些命令；哪些错误算“确定没执行”、哪些算“结果未知”；驱动在哪里替换了 ctx；默认超时是多少。新增调用点都要对照这张表。
2. Redis 默认不重放写命令，只在确定没执行时重发；读命令照常交给驱动重试。在 `redis/driver` 里提供一个集中的分类函数。
3. 补上 NC-101 复审列为“应改”的两项：
   - 脚本遇到“确定没执行”的错误时，在 `Client.Eval` 这一层重发；
   - Mongo 提交发出之后失败时，错误包上专用哨兵。
4. 全仓核对 Redis 写命令的生产调用方。

## 方案

**Redis（`redis/driver/replay.go`）**

- 写命令一律套上 `noReplay`（`NoRetry() == true`，`Clone` 后仍保留）再交给 go-redis。这样 go-redis 的三条重试路径都会跳过它：单命令重试（`redis.go` 的 `processWithRetry`）、含 NoRetry 命令的 pipeline 的整条重试（`generalProcessPipeline`）、Cluster 的换节点重试（`osscluster.go` 的 `process`）。MOVED / ASK 重定向的判断在 NoRetry 检查之前，照常跟随。这种做法 NC-100 的脚本已经用过，这次推广到全部写命令。
- 命令仍由 go-redis 的类型化方法组装（`buildWrite`：先入队到一个用完即弃的 Pipeliner、不经它发送），参数展开和过期格式与直接调用完全相同，再经 `sendOnce` 发出。
- `sendOnce` 发一次；只有错误满足 `IsDefinitelyNotExecuted` 时才重发，次数为 `resendsFor(MaxRetries)`（-1 不重发，0 取 3），退避与 go-redis 缺省相同（10ms 起、封顶 1s）。
- `IsDefinitelyNotExecuted` 按 go-redis v9.22.0 源码逐项核对：
  - 取连接阶段，请求还没写出：拨号失败、`ErrPoolTimeout`、`ErrPoolExhausted`、`ErrClosed`。`ErrClosed` 归为没执行，但不重发。
  - 服务端在执行前拒绝：LOADING、MASTERDOWN、TRYAGAIN、CLUSTERDOWN、max clients、READONLY、NOREPLICAS。
  - 池里的坏连接在取出时由池的健康检查丢弃、换成新连接，不会以错误形式出现；换新连接时的拨号失败归入第一类。
  - 握手阶段失败、EOF、超时、ctx 错误一律归为结果未知。握手失败在错误值上无法和写出之后的失败区分，保守处理。
- pipeline：只要含一条写命令，整条就不经驱动重放；`Exec` 只在每条命令的错误都满足分类函数时整条重发。`EvalBatchDurable` 同理，重发时换一条新的独占连接。
- `DistLock` 的 SETNX、释放脚本、续期脚本同样走 `sendOnce`。修前这三条直接发给 `UniversalClient`，不受 NC-100 的约束。
- 选型：go-redis 的 hook 改不了命令的 NoRetry；如果把 `MaxRetries` 设成 -1，读命令也会失去重试，Cluster 的换节点重试也关不掉。core 命令在 go-redis 里没有默认路由策略（`defaultPolicies` 里没有 core 模块），所以 `noReplay` 包装不影响 Cluster 的聚合路径。

**Mongo（`mongo/driver/session.go`）**

提交发出之后的失败统一包成 `fmt.Errorf("%w: %w", fmongo.ErrCommitResultUnknown, err)`：原错误链、标签、`errors.Is(err, context.DeadlineExceeded)` 都保留。确定没提交的错误不包，包括回调失败、截止在提交前到期、Transient 之后窗口在退避里关闭。

**文档**：契约表写在 [redis/driver/README.md](../../redis/driver/README.md) 和 [mongo/driver/README.md](../../mongo/driver/README.md)，`docs/USER_GUIDE.md` 链到这两页。

## 改动面

| 文件 | 变化 |
| --- | --- |
| `redis/driver/replay.go`（新） | `IsDefinitelyNotExecuted`、`noReplay`、`sendOnce`、`buildWrite` / `write`、`allNotExecuted`、重发退避 |
| `redis/driver/client.go` | 19 个写方法改走 `write`；Eval / EvalSha 改走 `runScript`（带确定没执行时的重发）；`EvalBatchDurable` 加整批重发；`Client.resends`。删除 `scriptCmd`，统一用 `noReplay` |
| `redis/driver/pipeline.go` | 写命令带 `noReplay` 入队；`Exec` 在确定没执行时整条重发 |
| `redis/driver/lock.go`、`assembly.go` | DistLock 走 `sendOnce` / `runScript`；Assemble 把客户端的重发次数传给锁工厂 |
| `cache/redis_hash.go`、`cache/redis_raw.go` | 写操作结果未知时仍补发 EXPIRE（调用方核对的结果，见下） |
| `mongo/errors.go`、`mongo/session.go`、`mongo/driver/session.go` | `ErrCommitResultUnknown` 及其包装 |
| `redis/driver/README.md`、`mongo/driver/README.md`（新） | 契约表 |

## 兼容

- **行为收紧**：写命令回复丢失时，以前可能被驱动“救回”成成功，也可能被重放成错误的结果（计数翻倍、列表重复、SETNX 误报、整条 pipeline 重放）；现在返回传输错误，属于结果未知。调用方本来就要处理这些错误。
- 脚本和写命令遇到 LOADING 等确定没执行的错误时，以前脚本会直接失败；现在会重发，可用性比 NC-100 之后好。
- `Raw()`、`kit/redis.SingletonStore`（本来就是 `MaxRetries=-1`）不变。
- 没有新配置键，没有持久格式或 wire 变化。新增公开 API：`driver.IsDefinitelyNotExecuted`、`fmongo.ErrCommitResultUnknown`。
- Mongo：`WithTransaction` 的错误文本多了 `mongo: transaction commit result unknown:` 前缀；`errors.Is` / `errors.As` 的语义不变。没有调用方按 `==` 比较这些错误（已 `rg` 核对）。

## 先红后绿

```text
# 修前（d346b45c 的 client.go / pipeline.go / lock.go / assembly.go；新用例与分类函数临时放在一起）
source ~/.roost-it/roost-dataengine-it/env.sh
GOWORK=off go test -tags integration -count=1 -run TestRealRedisAWriteWhoseReplyIsLostRunsOnce -v ./redis/driver/
  incr: err=<nil>; Redis holds counter=2                 one incr call was applied 2 times
  rpush: err=<nil>; Redis holds list=[msg-1 msg-1]       one rpush call was applied 2 times
  pipeline: err=<nil>; Redis holds counter=2 list=[msg-1 msg-1]
  setnx: the replayed SET NX saw this call's own key and reported it as taken (value=mine)
  distlock: the replayed SET NX saw this call's own key and reported it as taken (held-after-reconcile=1)
  （5/5 FAIL；真实 Redis + 本用例自建、端口随机的 toxiproxy 代理，limit_data 1 字节）
GOWORK=off go test -count=1 -run 'TestAWriteWhoseReplyIsLost|TestNotExecuted|TestDurableBatch' ./redis/driver/
  one {incr,incrby,rpush,lpush,lpop,rpop,set,del,hset,hdel,expire,zadd,zrem,sadd,srem,ltrim,lrem,publish,pipeline,distlock} call executed … 2 times on the server
  eval rejected once with "{LOADING,MASTERDOWN,TRYAGAIN,CLUSTERDOWN,ERR max number of clients reached,READONLY,NOREPLICAS} …" (never executed) failed instead of being resent
  EvalBatchDurable = [], local=0, LOADING Redis is loading the dataset in memory; want one result after one resend
  （28 个用例 / 子用例 FAIL）
GOWORK=off go test -tags integration -count=1 -run TestRealMongoCommitIsBoundedByTransactionTimeout -v ./mongo/driver/
  upstream committed=0 / downstream committed=1: commit was sent and cut by the deadline but err=… errors.Is ErrCommitResultUnknown=false DeadlineExceeded=true
GOWORK=off go test -count=1 -run TestAWriteWhoseReplyIsLostStillGetsItsTTL ./cache/
  HSET reached Redis but the key carries TTL 0s (want 1m0s): it never expires（ZADD 同）

# 修后：上述全部 PASS
  incr/rpush/pipeline/setnx/distlock: err=EOF; Redis holds counter=1 / [msg-1] / value=mine / held-after-reconcile=0
  mongo upstream / downstream: err=mongo: transaction commit result unknown: … context deadline exceeded（errors.Is 两者均成立）
```

绿用例（修前修后都应通过，用来守住边界）：`TestReadsKeepTheDriverRetry`（读命令回复丢失后仍由驱动重试，服务端执行 2 次）、`TestNotExecutedErrorsAreStillResent` 的写命令与 pipeline 部分（7 种拒绝 × 21 个调用点）、`TestResendsStopAtTheConfiguredBudget`（1 次发送加 3 次重发后，交回 LOADING 错误）、`TestAPipelineWithAnExecutedCommandIsNotResent`、`TestIsDefinitelyNotExecuted`（分类表）、`TestWithTransactionFailuresBeforeTheCommitAreNotResultUnknown`。

## 验证

```text
GOWORK=off go test -race -count=3 ./redis/... ./cache/ ./mongo/...                         ok
source env.sh; GOWORK=off go test -tags integration -count=1 -p 1 ./redis/...              ok（含 NC-208 的自建代理 toxic 锁用例）
GOWORK=off go test -tags integration -count=1 -run 'TestRealMongo(Commit|EndSession|WriteError)' ./mongo/driver/   ok
REDIS_ADDR=<隔离 Redis> GOWORK=off go test -tags integration -count=1 -p 1 ./kit/service/... ./service/... ./versionstore/ ./cache/... ./failurelog/ ./kit/redis/ ./remoteentity/   20 个包 ok
GOWORK=off go build ./... && go vet ./...                                                  ok
GOWORK=off go test -count=1 .                                                              ok（根包门禁）
GOWORK=off go test -race -count=1 ./kit/redis/ ./versionstore/ ./failurelog/ ./service/mail/ ./remoteentity/ ./kit/service/rank/ ./app/   ok
```

没有改动 nest / entity / dataengine / sync，所以没跑 glsvet；生成形状也没变。

## 调用方核对（第 3 项）

当前源码全仓 `rg`，排除测试、codegen 模板与生成 DAO、attribute、bus / syncbus、saga 协调器：

| 调用方 | 写 | 传输错误的处理 | 结论 |
| --- | --- | --- | --- |
| versionstore `RedisStore`（kit/service 各账本、service/mail、session、match） | 全部经 Eval（CAS / 删除 / 索引） | 原样返回；`Update` 只在“比较输了”时重读，不在错误上重试 | 正确。NC-100 起脚本就不重放，本次只多了“确定没执行时重发” |
| `kit/redis.SingletonStore` → `app/singleton.go` | CompareAndSet / CompareAndDelete | 错误按 Unknown 处理，`current == 自己的值` 时认领；释放失败让键自然过期 | 正确（客户端是 MaxRetries=-1） |
| remoteentity versioned lock | Eval | `UnlockWithRetry` 在循环里重试同一笔写，但有 unlockID 守卫，回复 2 表示上一次已生效；TryLock 按 token 前缀与序号认领；续期幂等 | 正确 |
| remoteentity marker / L2 快照 Set | Eval（lease CAS / 版本 CAS） | 返回；ownership 在不确定时重读裁决 | 正确 |
| remoteentity L2 快照 `Delete`（普通 DEL） | DEL | `ReadThroughStore` 按 IgnoreRemoteError 吞掉；`DeleteAtVersion` 在 `entity/remote_snapshot.go` 里是 `_ =` | 设计如此，按“L2 不可用”降级：结果未知时旧快照最多留到 TTL。以前驱动重放能在单次 EOF 时补上，现在不补。DEL 重放会删掉期间新写入的快照，所以也不该重放。见“未完成 / 观察” |
| cache `RedisJSONHashStore.Set` / `RedisRawSortedSetStore.SetScore` | HSET 或 ZADD，然后 EXPIRE | 修前：写失败直接返回，不发 EXPIRE，结果未知时新键永不过期 | **已修**：写结果未知时仍补发 EXPIRE，返回写的错误（新回归 `TestAWriteWhoseReplyIsLostStillGetsItsTTL`） |
| cache 其余写（SET PX、DEL、HDEL、ZREM）、`RefHMap` 脚本 | — | 返回 | 正确 |
| kit/service/rank | 交换脚本（带请求 ID 环）、Remove 脚本、Reset DEL | 返回；循环只在“没生效”时重跑 | 正确 |
| service/mail `redisEnvelopes.Create` | SETNX | 返回；同一 RequestID 重试时先查 Sends 账本，再回读信封 | 正确。修前 SETNX 被重放后，会把一次全新的发送报成 `ErrConflict "mail id already in use"`；本次修好 |
| failurelog | append / delete / purge 脚本 | 返回，出错时不走回退（NC-160） | 正确；回退里的 RPUSH 等只在替身返回 `(nil, nil)` 时才走到 |
| DistLock / AutoExtendLock | SETNX、脚本 | Acquire 出错时进入 uncertain，靠值守卫的 Release 和解 | 已改走 `sendOnce`；没有生产调用方 |

没有发现“把传输错误当成失败、然后再写一遍非幂等写”的生产调用方。

## 未完成 / 观察

- **bus / syncbus（不在本次范围，归别的 agent）**：`bus/reliable.go` 的 `BeginConsume` 用 SETNX 去重。修前回复丢失后被重放，第一次投递会看到自己写的 "processing"，被当成重复消息跳过；现在返回错误、触发重投，但重投时仍会看到 "processing"（这把键是否已写入属于结果未知），处理方式与消费者崩溃后留下 "processing" 相同，要等 InboxTTL。是否改成带 token 的认领，由 bus 负责方决定。
- **kit/service/global `Bind`**：`Create` 结果未知后，外层再调一次 Bind 会得到 `ErrConflict "already bound"`，尽管存的就是自己那条绑定。这是只插入 API 在结果未知后重试时的误报，不会重复写。这个问题从 NC-100 起就存在，与本次无关。需要时可以改成回读后比较 group / sid。
- **L2 快照 DEL**：见上表。如果要在结果未知时也保证删除，应该给 Delete 一个带版本的脚本（`DeleteAtVersion` 已经有），并让 `entity/remote_snapshot.go` 不再吞掉它的错误。这两处属于 Remote 本体，本次没有改。（2026-10-06 追加：[B2](B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md) 已处理——带版本删除结果未知时 L1 留未确认的删除标记，下一次读取重发（幂等）；收到复制删除的每个节点也会重发。）
- **Redis Cluster**：本机没有 Cluster 环境。`noReplay` 在 `ClusterClient.process` 里的 MOVED / ASK 跟随与换节点行为只按源码核对过，没有实测；连同 NC-100 一起留在外部验证清单。
- **versionstore 一次性写令牌（③）**：维护者决定暂不做。
- Redis 连接握手阶段的失败（例如新建连接时在 HELLO 上遇到 EOF）在错误值上无法和写出之后的失败区分，保守归为结果未知，不重发。

## 实施状态

①②与 NC-101 复审的两项“应改”已实施（分支 `a2driver`，提交号见 DECISIONS-PENDING 末表）；③按维护者决定暂不做。没有完成的部分见上一节。
