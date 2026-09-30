# Service 第十三轮：真实代理丢回复、多 owner HA、游标恢复与热点竞争

**结论：本轮列明的正确性场景全部通过，未确认新的生产逻辑 bug。**上轮因环境缺失而跳过的 3 个 Toxiproxy 用例，本轮经真实 TCP 代理在 `-race -count=2` 下 6 次执行通过；新增 Session `Attach`、Match `Enqueue` 写后丢回复各 1 次通过。隔离 Cluster 的无 `WAIT` 单 master 强杀后，独立进程能恢复 6 个 owner 的 run；测试侧持久 OwnerSource 在进程重启后处理完 250 owner。Match 单热点四客户端一分钟样本无状态错误，但 p99 与分配成本较高，延续已记录的历史归档/容量风险，尚无生产 SLO，不能登记为新的功能缺陷。

审查基线 Core `d91d8a30d97f2bef8f0b00d449187fb741c2f95b`、Kit `f0e5b67aa43473c2bcabd2ae180c69559fd6e20e`、Codegen `1e028fa4b2927ffb5d7772b90440e7447dc665cf`：三仓先 fetch，远端 `main` 与本地 HEAD 一致，主树干净。本轮 Core 只增加 review 文档、隔离脚本和 `.go.txt` overlay 探针，不改生产源码、正式测试或模块依赖。[复跑环境、源码、原始日志 hash](evidence/service-review-20260930-02/README.md)与[机制学习](IMPLEMENTATION-SERVICE-FAULT-AND-CONTENTION.md)为本结论的证据入口。先前[第十二轮](REVIEW-2026-09-30-services-12.md)的四类专项分母不被重新计为全框架覆盖率。

| 具名场景 | 实际结果 | 仍不能推断 |
| --- | --- | --- |
| Toxiproxy 锁故障 | 真实代理下原 3 test × `-count=2`、`-race`，6 pass/0 fail/0 skip；丢 Release/Acquire 回复、3 秒下行延迟与 caller deadline 实际执行。 | 只覆盖 Redis lock 的现有契约；不是完整双向分区或所有 Service 链路。 |
| Session 写后丢回复 | 测试侧包装器在 `Attach` 首次读取成功后、CAS `Eval` 前注入下行 timeout。API 返回 `context deadline exceeded`；直连 Redis 读到 1 条已提交资源，同资源重试仍 1 条，`Finish` 回调 1 次、pending=0。1 pass。 | 外部资源只由计数回调模拟；实际 scene/allocator 必须自身幂等并可对账。 |
| Match 写后丢回复 | 同样在 `Enqueue` CAS 前注入真实代理故障，调用报超时；Redis 有 1 ticket/1 RequestID，重放得到同 ticket，`Cancel` 后 Waiting=0。1 pass。 | 未覆盖跨机客户端并行与 Match 归档后的旧 request 重放。 |
| Redis HA 无 `WAIT` / 六 owner | 3 主 3 从，六个不同 hash tag 的 owner 各有 pending；负责 owner 1 slot 2596 的独占 master `:16631` 被强杀，`:16636` 约六次 500ms 轮询后升主，16384 槽恢复。另一个 Go 进程完成六个 run、claim 清理与 request 重放。seed/verify 各 1 pass。 | 脚本不调用 `WAIT`，但不能证明数据在强杀时未复制；一次本机故障不能保证无 `WAIT` 已确认写永不丢失，也未测跨机器/分区。 |
| Session 积压跨进程 | 测试侧 Redis OwnerSource 保存游标；三个进程先 seed 250 个 owner，再处理首 100 中的 99 个（owner 1 一次注入读错），重建后完成 100/50/1。最终 250 个 run expired/pending=0、claim 消失、持久回调计数各 1。3 pass。 | 持久游标是本轮 fixture 提供，不是 Core 自动发现 owner，也未接部署用真实 roster。 |
| Match 热点一分钟 | 4096 旧终态、四个独立 Redis 客户端，60.371 秒内完成 443 对 Enqueue/Cancel；209 次版本冲突由稳定 request/ticket ID 的调用侧重试收敛，Waiting=0；Go TotalAlloc 增量 59.46 GB，样本 pair p50/p95/p99 为 233.770/1966.356/3528.761 ms。1 pass。 | 单机、单队列、合成请求和一分钟样本；这些分位数不是业务生产 SLO、稳态吞吐承诺或真实 Redis 服务器内存。 |

执行环境为 Redis 8.8.0 独占 standalone `:16630`、3 主 3 从 Cluster `:16631..16636`、按[官方模块](https://github.com/Shopify/toxiproxy/releases/tag/v2.12.0)构建的 Toxiproxy `v2.12.0`，API `:18475`、代理 `:26380→:16630`，均为 Windows loopback。第一次执行因 Go 默认缓存目录被沙箱拒绝而 `build-fail`，未进入测试；设置工作区内 `GOCACHE/GOTMPDIR` 后上述所有测试正常运行。20 秒探索样本另有 161 对、78 次冲突重试、p99 约 3.53 秒；只以 60 秒最终样本作本轮主要容量观察。全部测试实例最终按进程/数据目录核对后关闭，九个相关端口均不监听。

Tier 2 图谱用 `search_graph` 定位 Session `SweepPending`/Kit `Serve`、Match `queueState.clone`、Toxiproxy 锁测试，`trace_path` 核对 Sweep 双向调用，`get_code_snippet` 读取关键方法，并对 `service/session/`、`service/match/`、`redis/driver/` 及 `versionstore/redis_store.go` 等路径做 `check_index_coverage`。Core 图谱与 HEAD 对齐；full generation `2026-09-29T14:50:03Z` 对这些源码路径无记录的解析缺口，但 Windows `metadata_changed`，因此用实际 checkout 源码与可复跑测试补证。图谱 `Match.Enqueue`/`RedisStore.Update` 个别方法枚举不全，直接读源码，不作穷尽性否定。Kit 的 `kit/scripts/integration/dataengine-env.sh` 与 `lib/toxiproxy.sh` 提供 Toxiproxy 接线；上轮只说 Core 根目录 `scripts/integration/dataengine-env.sh` 缺失，不能推断整个框架没有接线脚本。

下一入口仍是实际 allocator/scene/资产/支付回执与幂等、跨机器及双向网络分区、故意未复制时的写入丢失/对账、部署用持久 OwnerSource、Match 归档后的兼容迁移与目标 SLO 下多小时/多天长稳。当前六个 owner 样本和一分钟容量观察只是有界增量，不把这些外部/长期项目标为已验收。
