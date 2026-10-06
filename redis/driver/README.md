# redis/driver 驱动行为契约

`redis/driver` 是 `fredis.IRedis`（`redis/client.go`）在 go-redis v9.22.0 上的实现，kit 的 RedisMod（`kit/redis/redis_mod.go`）、`SingletonStore` 和 `NewClient` 都用它。框架的约定是：**结果未知时交给调用方处理**。下面几张表写明驱动在哪些情况下会自己重放命令、哪些错误可以安全重试、在哪里替换了 ctx，以及各项默认值。新增 Redis 调用点时先对照这几张表（A2 决定，2026-10-05；[方案与实施记录](../../docs/feature/A2-DRIVER-REPLAY-CONTRACT-2026-10-05.md)）。

升级 go-redis 时要按源码重新核对本页：`error.go`（`shouldRetry`、`isBadConn`）、`redis.go`（`processWithRetry`、`generalProcessPipeline`）、`osscluster.go`（`process`）、`internal/pool`。

## 1. 哪些命令会被重放

| 命令 | 驱动行为 |
| --- | --- |
| 读命令：GET、MGET、EXISTS、TTL、HGET、HGETALL、HEXISTS、LLEN、LRANGE、ZSCORE、ZRANK、ZREVRANK、ZRANGE/ZREVRANGE WITHSCORES、ZCARD、SMEMBERS、SISMEMBER、PING；只含读命令的 pipeline | 由 go-redis 自动重试，最多 `MaxRetries` 次。会重试的错误：EOF、连接重置、读写超时（命令没有显式读超时时）、等池超时、拨号失败，以及 LOADING、READONLY、MASTERDOWN、CLUSTERDOWN、TRYAGAIN、NOREPLICAS、max clients。Cluster 模式下还会按 `MaxRedirects` 换节点重试 |
| 写命令：SET、SETNX、DEL、EXPIRE、INCR、INCRBY、HSET、HDEL、LPUSH、RPUSH、LPOP、RPOP、LTRIM、LREM、ZADD、ZREM、SADD、SREM、PUBLISH | 每条都加了 `NoRetry` 标记再交给 go-redis，驱动在任何错误上都不重发。只有当错误满足 `IsDefinitelyNotExecuted`（见 §2）时，才由本包重发（`replay.go`） |
| 脚本：EVAL、EVALSHA（`Client.Eval` / `EvalSha`） | 与写命令相同。脚本只要回复丢了就不重发（RR-20261005-NC-100）；确定没执行时照常重发，NC-100 之后丢掉的这部分可用性由此补回 |
| `EvalDurable` / `EvalBatchDurable`（脚本加 WAITAOF，走同一条物理连接） | 整批不重放。只有每条命令的错误都证明没执行时，才换一条新连接整批重发 |
| `EvalReplicated`（脚本加 ROLE，必要时同连接 WAIT；O-M6-3，L2 墓碑用） | 脚本与 ROLE 一条流水线，不经驱动重放；每条命令的错误都证明没执行时才换一条新连接整条重发。**WAIT 从不重放**：换了连接的 WAIT 只统计新连接上的写（没有），会立即给出假的确认。Cluster 下 WAIT 只发往脚本第一个键所在槽位的主节点（`MasterForKey` 取节点、在它的独占连接上发）；脚本回 MOVED / ASK 时改经集群客户端普通发送一次、不 WAIT。主节点没有连着的副本时不发 WAIT |
| 含任一写命令的 pipeline（`IPipeline` 的 Set、Del、HSet、Incr、Expire、ZAdd、RPush、LPop） | 整条不重放，因为回复丢失时前面的写可能已经执行。只有每条命令的错误都证明没执行时，才整条重发 |
| `DistLock` 的 SETNX、释放脚本、续期脚本 | 与写命令相同。被重放的 SETNX 会把自己刚拿到的锁报成“已被占用”，锁一直挂到 TTL；被重放的释放脚本会把成功释放报成 `ErrLockNotHeld` |
| Subscribe | go-redis PubSub 断线后自动重连并重新订阅，推送不保证送达 |
| 经 `Client.Raw()` 直接发出的命令 | 不受本表约束，按 go-redis 缺省规则重放。业务代码不要用 Raw 发写命令 |

确定没执行时的重发次数由 `Config.MaxRetries` 换算，规则与 go-redis 相同：-1 不重发，0 取 3，其他值照用。重发前退避，与 go-redis 缺省相同：10ms 起按次数翻倍取随机值、封顶 1s。退避期间调用方的 ctx 结束时，返回“上一次错误 + ctx 错误”（`errors.Join`），这个错误仍属于“确定没执行”。

所有写命令一律不重放，包括 SET、EXPIRE 这类结果看似幂等的命令。原因是重放会和别的写者交错：A 的 SET 回复丢了，B 随后写入，A 被重放的 SET 会覆盖 B。

## 2. 错误分类：`IsDefinitelyNotExecuted(err)`

| 类别 | 错误 | 调用方怎么做 |
| --- | --- | --- |
| **确定没执行** | 取连接阶段失败，请求还没写出：拨号失败（`*net.OpError` 且 `Op=="dial"`，包括拨号超时）、`ErrPoolTimeout`、`ErrPoolExhausted`、`ErrClosed` | 可以安全重试。`ErrClosed` 表示客户端已关闭，重试没有意义，驱动也不重发 |
| **确定没执行** | 服务端在执行前拒绝：LOADING、MASTERDOWN、TRYAGAIN、CLUSTERDOWN、`ERR max number of clients reached`、READONLY、NOREPLICAS。判定沿用 go-redis 的 `IsXxxError`：认类型化错误，也认以该前缀开头的错误文本 | 可以安全重试。脚本里遇到 READONLY 或 NOREPLICAS 时，被拒的是脚本里第一条写，之前没有写入。框架自己的脚本不会返回带这些前缀的错误回复，新写的脚本也不要这样返回 |
| **结果未知** | EOF、`io.ErrUnexpectedEOF`、连接重置、读写超时（i/o timeout）、调用方 ctx 取消或到期（可能落在写出之后）、建连握手阶段的失败（从错误值上无法和写出之后的失败区分，保守归为未知），以及不属于上下两类的其他错误 | 不能当成“没写”。要重试，就必须先让写可以安全重复执行，比如带请求 ID、按版本做 CAS、带值守卫令牌；或者先回读再裁决 |
| **已执行并报错** | 服务端回了针对命令本身的错误，例如 WRONGTYPE、脚本里的 `ERR ... user_script` | 命令已经被处理。脚本中途报错时，报错之前的写不会回滚 |

池里的坏连接（例如服务端已经关掉的空闲连接）在取出时由 go-redis 的健康检查丢弃并换成新连接，不会以错误形式出现。换新连接时如果拨号失败，归入第一行。连接池自己会先重拨 `DialerRetries` 次（缺省 5 次）；连续拨号失败达到 `PoolSize` 次后，在后台探测恢复之前会直接返回上一次的拨号错误，所以 PoolSize 小的客户端在 Redis 宕机期间，驱动层的重发基本都会很快失败。

pipeline 只有在每条命令的错误都满足本函数时，才算整条没执行。

**WAIT（`EvalReplicated`）的分类**：WAIT 不是写，它只回答“本连接之前的写有几个副本确认了”。脚本的回复已经收到时，脚本属于“已执行”（按脚本回复裁决），之后 ROLE / WAIT 的任何错误（EOF、超时、连接重置）都只说明**复制结果未知**——副本可能已经收到，也可能没有——放在 `ReplicatedEvalResult.WaitErr`，不改变脚本的分类，也不重放 WAIT 或脚本。WAIT 正常返回但确认数少于要求（超时）不是错误：写已在主节点上、尚未确认复制，切主可能丢。调用方据此计数或告警，不要回滚，也不要把已执行的写报成失败。

## 3. 驱动在哪里替换了 ctx

| 位置 | 用的 ctx |
| --- | --- |
| 命令的读写 | 调用方的 ctx。客户端打开了 `ContextTimeoutEnabled`，ctx 的截止时间会下发为 socket deadline。没有截止时间时，由 ReadTimeout / WriteTimeout 兜底（TROUBLESHOOTING T-43） |
| 拨号 | 调用方的 ctx 外面再套一层 DialTimeout，取两者中更早的，每次拨号单独计时 |
| 等待池连接 | PoolTimeout 与调用方 ctx，取先到期的 |
| 确定没执行时的重发退避 | 调用方的 ctx |
| Cluster 断连后的拓扑刷新 | `clusterRecoveryHook` 用 `context.Background()` 异步调用 `ReloadState`，不阻塞调用方，原错误原样返回 |
| `kit/redis.SingletonStore` | 自建的两个客户端都设 `MaxRetries=-1`，读写都不重发；启动时用 `singletonPingTimeout` 做 Ping |

## 4. 默认值

| 项 | 默认值 | 来源 |
| --- | --- | --- |
| DialTimeout / ReadTimeout / WriteTimeout | 5s / 3s / 3s | `fredis.DefaultConfig`；kit 的 `redis.*` 不暴露这三项 |
| MaxRetries | 3：读命令最多重试 3 次；写命令只在确定没执行时最多重发 3 次 | `fredis.DefaultConfig` |
| PoolSize / MinIdleConns | 10 / 5 | `fredis.DefaultConfig`；kit 用 `redis.pool_size`、`redis.min_idle_conns` 覆盖 |
| PoolTimeout | ReadTimeout + 1s = 4s | go-redis 缺省 |
| 重试与重发退避 | 10ms 起，封顶 1s | go-redis 缺省（MinRetryBackoff / MaxRetryBackoff），本包重发沿用同一公式 |
| 连接池内部重拨 | 5 次 | go-redis `DialerRetries` 缺省 |
| Cluster MaxRedirects | 3 | go-redis 缺省。core 命令没有默认路由策略，跨槽的多键 DEL 由服务端回 CROSSSLOT，驱动不拆分 |

## 5. Close：重复调用与出错后再调用

维护者第十二轮决定把 Close 的行为写进契约表。下表是 2026-10-06 按当前源码（go-redis v9.22.0）用临时探针实测的结果（不可达地址，`-race`），不是理想契约；不一致的地方登记在 [WANTED W-2026-10-06-02](../../docs/bug/WANTED.md)，代码没有改。

| 对象 | 第二次 Close | 第一次 Close 出错后再调用 | Close 之后的操作 | 并发 Close |
| --- | --- | --- | --- | --- |
| 单机 `Client.Close` / `Assembly.Close` | 返回 `redis: client is closed`（`goredis.ErrClosed`，可 `errors.Is`；`IsDefinitelyNotExecuted` 为真），不阻塞 | 第一次不论成败都已关完全部连接池（go-redis 遇错继续关），再调返回 `ErrClosed`，不泄漏 | 一律 `ErrClosed`；写命令不重发 | 一个拿到第一次的结果，其余立刻返回 `ErrClosed`，**不等**第一个释放完 |
| Cluster `Client.Close` / `Assembly.Close` | 返回 nil | 同上，再调返回 nil | 一律 `ErrClosed` | 后到者等第一个做完，都返回 nil |
| `IPubSub.Close` | nil（`sync.Once`） | 只在第一次释放；之后都是 nil，拿不到第一次的错误 | 消息 channel 已关闭 | 后到者等第一个做完，返回 nil |
| `DistLock` / `AutoExtendLock` | 没有 Close，只有 Release：第二次 Release 返回 `ErrLockNotHeld` | — | 客户端关闭后 Acquire / Release 返回 `ErrClosed`，锁记为结果未知，之后 Acquire 一直返回 `ErrDistLockStateUncertain` | — |
| kit `RedisMod.StopWithContext` | nil（第一次就交出连接，NC-233） | 第一次报错（错误里写明连接池照样已关），之后 nil | — | 不支持并发调用（App 每次停机对每个 Mod 只串行调一次） |
| kit 单实例锁 `singletonStore.Close` | 返回第一次的结果（`sync.Once`，粘滞） | 两个客户端都已关；再调返回同一个错误 | `ErrClosed` | 安全 |

调用方据此：判断“已经关了”用 `errors.Is(err, goredis.ErrClosed)`，不要假设重复 Close 一定返回 nil；需要“再调用返回 nil”的停机路径（Mod Stop）在第一次 Close 之后就交出连接，像 `RedisMod` 那样。

## 6. 新增调用点核对清单

1. 写命令返回错误时，结果未知，不能当成失败后再写一遍。需要重试的写，先让它可以安全重复执行（请求 ID、版本 CAS、值守卫令牌），或者先回读再裁决。确实只想在“确定没执行”时重试的，用 `driver.IsDefinitelyNotExecuted`，不要自己按错误文本判断。
2. 写命令的返回值（SETNX 的 bool、DEL / HSET / SADD 的计数、INCR 的新值、LPOP 弹出的元素）只在 `err == nil` 时可信。
3. 多步写，例如先写再 EXPIRE：第一步结果未知时，仍要补上后面那步保护，例如照样补发 EXPIRE，不要留下永不过期的键。`cache.RedisJSONHashStore.Set` 和 `cache.RedisRawSortedSetStore.SetScore` 都是这样做的。
4. 吞掉错误的路径（best effort）要写明：结果未知时会留下什么状态，留多久。
