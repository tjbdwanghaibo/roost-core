# 联盟服务短测（2026-11-04，方案阶段 5）

基线 main `afd61beb` 加阶段 5 的脚本与用例（本目录同一提交），macOS（Apple M5，10 核，32GB），`scripts/mirror-local.sh` 私有依赖（偏移 32000：Mongo 三节点副本集、NATS 三节点 JetStream、Redis 单机）+ 脚本自起 etcd；生成的 game-demo 二进制起真实进程。单轮、单样本；本机同时有别的负载（load average 15～30，IDE、Unity、别的测试进程），延迟数字只作量级，两轮之间相差可到 2 倍（下表给两轮）。

复跑：

```sh
ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=32000 scripts/mirror-local.sh up
source <私有目录>/roost-dataengine-it/env.sh
ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_RUN='^TestAllianceE2E(ShortPerf|MirrorRevalidation)$' scripts/test-alliance-e2e.sh
scripts/mirror-local.sh clean
```

`ALLIANCE_E2E_PERF_COUNTS`（缺省 `1000,10000`）选预加载的联盟数。整轮约 25 分钟，其中 1 万个联盟的建盟约 9 分钟、转移约 6 分钟。

## 路由 RPC 延迟（`TestAllianceE2EShortPerf`）

四个 alliance 实例，64 个联盟分在各实例，调用方是生成的路由 `BusClient`（带路由缓存）。

| 调用 | 并发 | 第一轮 次/秒 · p50 / p99 | 第二轮 次/秒 · p50 / p99 |
| --- | --- | --- | --- |
| `Info`（读权威） | 1 | 90 · 9.3 / 35.7 ms | 51 · 17.5 / 48.3 ms |
| `Info`，联盟分在 4 个实例 | 16 | 214 · 36 / 228 ms | 149 · 69 / 306 ms |
| `Info`，同一个联盟（1 个实例） | 16 | 91 · 166 / 265 ms | 67 · 227 / 366 ms |
| `Join`（strict 写，入会） | 1 | 11 · 86 / 131 ms | 8 · 115 / 235 ms |
| `Join`，分在 4 个实例 | 16 | 24 · 317 ms / 2.2 s | 20 · 761 ms / 2.2 s |
| `Found`（建盟，队列组） | 16（4 实例） | 16～18 个/秒 | — |

同一实例上同一方法的请求是串行处理的（bus 按方法名哈希选 worker，见 KNOWN-LIMITS“实体服务的 RPC 在一个实例上按方法串行”）：一个实例上 16 并发读 16 个不同联盟也只有约 100 次/秒，`Join` 约 13 次/秒、`Found` 约 9 个/秒；并发只增加排队时间。

## 预加载与续期（`TestAllianceE2EShortPerf`）

四个实例并发建盟后用 `alliance.transfer {count}` 全部转给 3000（`ops.admin_timeout` 放宽到 600s，缺省 10s 在上万实体时超时），3000 正常停机后同 sid 拉起。续期量取拉起后 30s 内 `remote_entity.route_renew_total{result="renewed"}` 的增量，折成每 10s。

| owner 是 3000 的联盟数 | 预加载 | 起服到 ready | 常驻内存（预加载完 / 30s 后） | 续期 / 10s |
| --- | --- | --- | --- | --- |
| 1 000 | 1.7 s | 2.2 s | 65 / 45 MiB | 1 000 |
| 2 000（`TestAllianceE2EPreloadOrder`） | 2.0 s | 2.6 s | 71 MiB | — |
| 10 000 | 13.7 s | 14.0 s | 123 / 40 MiB | 10 000 |

预加载时间约随联盟数线性增长（约 1.4ms / 个，并发上限 16）；30s 后的常驻内存比预加载刚完成时低（`ps` 的 RSS，GC 归还之后），每个演示联盟只有几个成员，文档很小，不代表大联盟的内存。拉起后只有预加载的联盟登记路由，守卫按需加载后每个同样每 10s 续一次。转移本身约 25ms / 个（1882 个 46s，15447 个 5 分 57 秒）。

## Mirror 回源（`TestAllianceE2EMirrorRevalidation`）

全栈（九个托管服务 + alliance 两实例 + game 两进程），20 个玩家在 game 1000 上每 250ms 读一次“我的公会”（守卫 + 联盟两个 Mirror，Monotonic），60s。Mirror 没有按来源的计数，回源量取 Redis `INFO commandstats` 的 `HGET` 增量（L2 读取用 `HGET`），减去同长度无读取窗口的底噪（两轮底噪都是 0）。

| `cached_max_staleness` | 读取次数 | 不同 key | HGET（回源） | 回源 / 秒 | guild_info p50 / p99 |
| --- | --- | --- | --- | --- | --- |
| 30s | 4780 | 21 | 42 | 0.7 | 0.30 / 1.7 ms |
| 5s | 4780 | 21 | 252 | 4.2 | 0.29 / 3.6 ms |

回源量正好是“在读的不同 key 数 × 窗口 / 陈旧上限”（21 × 60 / 30 = 42，21 × 60 / 5 = 252），与读取频率无关；推送正常时读一直命中 L1。结论：5s 的代价是每个在读的 key 每 5 秒一次 Redis `HGET`——10 万在线、每人读自己的守卫与公会时约 2 万次/秒（30s 时约 3 千多次/秒），单个 Redis 承受得住但占可观的份额。推荐 game 读者用 5s（推送中断时陈旧上限从 30s 降到 5s）；Redis 与别的业务共用、余量紧时留 30s。配置没有改，待维护者确认。
