# 方案：单实例锁改用 etcd，与服务发现共用一套协调（2026-11-16）

本轮三项方案的依赖、实施顺序、全量测试与时间预估见[总览](FEATURE-2026-11-16-round-overview.md)。

状态：**已实施（阶段 1a，分支 `feat/round-20261116`），待阶段 7 的全量、先失败后通过证据与真实进程演练**（维护者 2026-11-16 定做法 A，第 7 节三问均已答复）。实施时追加的决定：生成的 `singleton.startup_wait` 由 30s 改为 35s（第 6 节第 2 条的换 leader 最坏情况）。演练脚本 `scripts/testdata/singleton-etcd-drill/singleton-etcd-drill.sh`。

## 1. 现状与问题

- **锁的位置**：`framework/app/singleton.go` 的单实例锁和业务时间高水位，都存在 `SingletonStore` 里。
- **生成工程用 Redis**：生成的 bootstrap 全工程统一写 `a.Singleton(kitredis.SingletonStore)`（`codegen/internal/roost/render.go`）。只要工程里有 redis 或 dataengine，就会装上。
- **服务发现用 etcd**：`wiring/etcd`，租约 10s，键 `<service_prefix><server_type>/<sid>`。

所以一个部署里有两套协调机制：

| | Redis（单实例锁） | etcd（服务发现） |
|---|---|---|
| 存什么 | `<key_prefix>:<type>:<sid>` 持锁令牌；`<key_prefix>:business_time` | 地址和元数据 |
| 过期 | 键 TTL 15s，3s 续一次 | 租约 10s，KeepAlive |
| 谁读 | `SingletonLiveness.Live`（唯一活性权威）、`SingletonIdentityChecker`（Gate、cellworldhost 核对候选是否持锁） | 路由选候选 |

**当初选 Redis 的原因**（`APP-SINGLETON-LOCK-2026-10-05`）：
- 能直接复用 `redis.CompareAndSet` / `CompareAndDelete`；
- 当时 DataEngine 工程都有 Redis，etcd 不一定有。

这个前提已经变了：
- 生成工程的缺省服务都带 etcd；
- Gate、亲和路由、doctor `gate:etcd-singleton` 都要求 etcd；
- 锁是"谁活着"的权威，地址却放在另一个系统里，两边各有一套过期时钟（15s 和 10s），需要分别理解、监控和排障。

## 2. 目标与不做的事

**目标**：
- 生成工程的单实例锁和业务时间高水位改存 etcd；
- 单实例锁不再需要 Redis，锁和发现共用一个 etcd 集群；
- `framework/app` 的锁语义（取锁、续租、守护、fail-stop、启动等待、释放）和 `SingletonStore` 接口都不变。

**不做**：
- 不改 `Live` 的地位：活性仍以锁为准。发现条目仍只表示"地址候选"。
- 不做旧部署兼容：线上未部署，锁键旧数据不迁移。
- 不替换 DataEngine、remote_entity 等对 Redis 的其他使用。Redis 仍是数据缓存；本方案只把"协调"从 Redis 拿走。

## 3. 可选做法

### A. etcd 实现 `SingletonStore`，全工程统一用它（推荐）

在 `wiring/etcd` 新增 `SingletonStore(cfg)`，生成器改写成 `a.Singleton(kitetcd.SingletonStore)`。框架层不改。

**必须全工程统一，不能按服务选**：Live 和 IdentityChecker 要读别的进程的锁键。比如 Gate 读 game 的锁键。后端不一致，就读不到。

### B. 在 A 的基础上，发现注册挂在锁的租约上

单实例进程的发现条目直接挂在锁的租约上：锁丢了，条目同时消失；崩溃后，条目和锁在同一时刻过期。

- **好处**：发现里的候选就是持锁者，不再有"地址还在、锁已丢"的 10s/15s 时间差。
- **代价**：
  - `framework/app` 要把租约 ID 交给 etcd Mod，`Discovery` 的断租重注册逻辑也要区分两种注册：挂锁租约的，和自己有租约的；
  - 没开单实例锁的服务仍用自己的租约，所以要维护两条注册路径。
- **收益有限**：Live 已是权威。Gate 和 cellworldhost 的 IdentityChecker 已经会核对锁，地址早一点或晚一点消失都不影响正确性。

建议先只做 A。B 等真遇到"发现里残留候选"带来的实际问题再说。

### C. 维持 Redis，只补文档

- **不改的理由**：零改动。Redis 和 etcd 都是单点依赖，换后端并不减少依赖的数量。
- **不改的代价**：两套协调机制长期并存。

## 4. A 的实现要点（etcd 语义怎么对上接口契约）

接口不变：

```go
CompareAndSet(ctx, key, expected, next []byte, ttl) (applied bool, current []byte, err error)
CompareAndDelete(ctx, key, expected []byte) (applied bool, err error)
Get(ctx, keys []string) ([][]byte, error)
Close() error
```

| 操作 | etcd 做法 |
|---|---|
| **抢锁**（expected=nil，ttl>0） | 先 `Grant(ttl)`，再执行一个 Txn：`If(CreateRevision(key)==0) Then(Put(key,next,lease)) Else(Get(key))`。不成功就 `Revoke` 刚申请的租约。 |
| **续锁**（expected==next，ttl 等于持有租约的 TTL） | 先 `KeepAliveOnce(lease)`，再执行校验 Txn：`If(Value==expected && Lease==lease)`。租约不存在或校验失败，返回 applied=false 和当前值。<br>因为令牌唯一，KeepAlive 成功就说明键还挂在我们的租约上。校验 Txn 只是兜底。 |
| **换值或换 TTL**（expected≠next，或 ttl 变了） | 申请新租约，执行 `If(Value==expected) Then(Put(next,newLease))`，然后撤销旧租约。 |
| **ttl=0**（business_time 高水位） | 不挂租约的 Put。expected=nil 时比较 `CreateRevision==0`，否则比较 `Value==expected`。 |
| **释放** CompareAndDelete | `If(Value==expected) Then(Delete)`，然后尽力撤销租约。 |
| **Get**（Live、IdentityChecker） | 一个 Txn 里放 N 个 Get（一次往返、同一版本），线性一致读。缺的键返回 nil。 |

**契约逐条核对：**

- **ctx 截止时间**：每个调用都只用传进来的 ctx。clientv3 自带的重试在同一个 ctx 内进行，不另开超时。
- **可以重复执行**：
  - 抢锁成功但回复丢失：重试时 `CreateRevision==0` 不成立，返回 current=自己的令牌。App 已经按"current 等于自己的值就认领"处理（`singleton.go` 契约第 44 行），所以不用改。
  - 续锁重做无副作用。
- **续锁不被 Live 饿死**：和 Redis 版一样，建两个独立的 clientv3.Client，一个续锁，一个读（Live / Get）。
- **时间窗口只会更保守**：
  - etcd 租约由 leader 从收到 KeepAlive 时开始计时，持锁方从发出请求时开始计时。所以服务端的过期不会早于持锁方以为的过期，这和 Redis 的 PX 是同一方向。
  - etcd 换 leader 时会把所有租约续满一次。这只会让旧锁晚过期，不会早过期，所以互斥不受影响。代价是新进程可能等得更久，见第 6 节。
- **TTL 粒度**：etcd 租约以整秒计。`singleton.ttl` 不是整秒时，opener 打开时直接拒绝，不偷偷取整。
- **连接配置**：opener 在任何 Mod Init 之前运行，所以和 Redis 版一样，从同一份配置的 `etcd.*` 自建连接，不复用 etcd Mod 的客户端。服务开了锁，只需要 `etcd.*` 配置段，不要求装发现 Mod。
- **单个 Cmp 的限制**：`infra/network/etcd.IEtcd.Txn` 只接受一个 Cmp，续锁的校验需要两个。做法二选一：
  - 实现里直接用 `driver.Client.Raw()` 拿到的 clientv3；
  - 把 `Txn` 改成接受 `[]Cmp`。

  倾向用 Raw，改动面小。

**键格式**：`framework/app` 的拼键规则（`<key_prefix>:<type>:<sid>`）不变。生成的 `key_prefix` 改成 `/<project>/singleton`，和发现的 `/<project>/services/` 放在同一个目录下，etcdctl 按前缀查看时能放在一起看。

## 5. 生成器、doctor、文档的改动

- **`render.go`**：
  - bootstrap 写 `a.Singleton(kitetcd.SingletonStore)`；
  - `projectInstallsSingleton` 改为"工程里有 dataengine 或 etcd"；
  - `renderSingletonConfig` 的注释改成 etcd，"需要 redis.addr"改成"需要 etcd.endpoints"。
- **`config_schema_doctor.go`**：开了锁的服务，要求的配置组从 `redis` 改为 `etcd`。
- **开锁服务没有 etcd 的情况**：生成器给 `singleton.enabled` 的服务补 `etcd.*` 配置段，只补配置，不加 Mod。
- **`wiring/redis/singleton.go`**：看第 7 节问题 2 的决定，删除或保留。
- **全工程重新生成后对比**：演示工程、genre 夹具、`scripts/test-gate-generated.sh`。
- **gate drill**：配置里锁的后端会变。
- **`physicsroom-local` 手写的 Singleton 接线**：跟着改。
- **文档**：
  - `docs/framework` 里单实例锁的说明；
  - KNOWN-LIMITS：协调依赖改成"etcd 不可用 → 开锁进程 fail-stop"；
  - CHANGELOG、升级须知：不兼容，重新生成即可。

## 6. 风险

1. **etcd 故障的后果变重。**
   - 现在：etcd 挂了只影响路由发现，Redis 挂了开锁进程才 fail-stop。
   - 改后：etcd 挂了，所有开锁进程会在 ttl−guard（10s）后 fail-stop。
   - 总体看依赖数量没变，仍是"协调存储挂了就停"。不同的是，以后 etcd 的可用性同时决定路由和存活。生产要求 etcd 三节点或更多，这一条写进 KNOWN-LIMITS 和部署文档。
2. **换 leader 时租约续满。** 旧进程真死了的时候，新进程最多可能多等一个 TTL。现在的启动等待是 2×TTL（30s），通常够用。如果正好赶上换 leader，可能超时启动失败，由部署重启兜底。演练里要单独测这一条。
3. **etcd 写压力。**
   - 续锁的 KeepAlive 由 leader 处理，不进 raft 日志；
   - 校验 Txn 是读；
   - 抢锁、换值、释放才写，频率很低。
   - 每个进程每 3s 一次续锁加一次读。百个进程量级对 etcd 没有压力。

## 7. 决定与待确认

1. **做法：A**（维护者 2026-11-16 定）。B 不做，以后真遇到“发现里残留候选”的实际问题再提。C 不采用。
2. **Redis 版 `SingletonStore`：删除**（维护者 2026-11-16 定）。只留 etcd 一条路，测试和文档只讲 etcd，不需要两套契约测试。
3. **etcd 不可用时开锁进程 fail-stop（第 6 节第 1 条）：接受**（维护者 2026-11-16 定）。实施时写进 KNOWN-LIMITS 框架限制和部署文档，生产要求 etcd 三节点或更多。

**“全工程统一”的含义**（答维护者 11-16 问）：一个工程里所有开锁或读锁的服务，锁键必须放在同一个 etcd 集群（同一组 `etcd.endpoints`），并且用同一个 `singleton.key_prefix`。原因是 Gate 的 IdentityChecker、`Live`、cellworldhost 都要读别的进程的锁键，只会去自己配置的存储和前缀下找。所以：

- 生成器给全工程写同一个后端，不提供按服务切换；
- 锁与服务发现用同一组 `etcd.*`，也就是同一个集群；
- 实施时 doctor 加一项全工程检查：各服务的 `etcd.endpoints` 与 `singleton.key_prefix` 一致。现有 `gate:namespace` 只查 Gate 两侧。

## 8. 验证计划

**单测**（接真实 etcd，brew 二进制，`~/.roost-it` 环境）：
- 抢锁、续锁、换值、释放、ttl=0；
- 回复丢失后重做；
- 租约过期后续锁失败；
- Get 多键和缺键；
- ctx 截止时间；
- 非整秒 TTL 被拒绝。

**契约测试**：如果保留 Redis 版，抽一套共用的契约测试，两个实现都跑。

**先失败后通过**：去掉续锁里"KeepAlive 失败即判丢锁"的判断后，对应用例必须失败。

**真实进程演练**（沿用 10-05 的做法）：
- 两个同 sid 的 game 进程，第二个阻塞在启动等待；
- 对持锁进程 SIGSTOP，第二个在 TTL 后接管；
- 恢复第一个，它应当 fail-stop，日志里有丢锁；
- etcd 停 20s 再恢复，开锁进程应当 fail-stop，并且不会写出双写 WAL。

**全量**：最后跑一次 codegen 全量、模块门禁和 gate drill。

## 9. 工作量

**以[总览 §4](FEATURE-2026-11-16-round-overview.md#4-实施顺序与时间预估)的 agent 墙钟估算为准。**本节原来按人力天数估算，偏大太多，已作废。
