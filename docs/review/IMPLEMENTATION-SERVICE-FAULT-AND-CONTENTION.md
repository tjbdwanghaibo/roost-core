# Service 学习：回复未知、故障接管与热点队列的不同保证

本篇对应[第十三轮实测](REVIEW-2026-09-30-services-13.md)与[测试源/结果](evidence/service-review-20260930-02/README.md)，延续[第十二轮的回收机制说明](IMPLEMENTATION-SERVICE-CRASH-HA-AND-CAPACITY.md)。以下区分源码保证、隔离环境实测和部署方仍需完成的契约。

## 写入成功与收到成功回复是两件事

[`versionstore.RedisStore.Update`](../../versionstore/redis_store.go)先读存储值，再通过 `CompareAndSet` 脚本尝试提交；如果脚本执行后回复丢失，它返回传输错误，不能知道本次是否已写入。测试侧包装器仅在 CAS `Eval` 前向 Toxiproxy 安装下行 timeout，使此前的读请求正常完成；Session `Attach` 与 Match `Enqueue` 都出现“调用报 deadline、Redis 已提交”的实际反例。Session 同资源 `Attach` 的无操作分支和 Match 同 RequestID 的回放分支，在读回后给出稳定结果。业务调用者应保留稳定资源 ID / RequestID，遇到未知结果先读回或重试同一身份；生成新身份盲目重试会绕过幂等键。Core 对资源记录/匹配票据的幂等不能代替真实 allocator 或资产系统的持久幂等与回执。

已有 [`TestToxicRedis*`](../../redis/driver/lock_toxic_integration_test.go) 由 [Kit 的 Toxiproxy 接线脚本](../../kit/scripts/integration/lib/toxiproxy.sh)配合独占代理运行。此次三项 `-race -count=2` 都是 pass，覆盖锁获取/释放回复丢失与调用超时。Toxiproxy 对下行数据作真实 TCP 层干预，而测试包装器只控制**何时**装上 toxic；正常 Redis 读、CAS 写与业务判定仍使用生产代码。不能从这几种 toxic 推断双向分区、所有 Redis 命令和服务间 RPC 都已验证。

## HA 恢复不等于写入持久性证明

[`clusterRecoveryHook`](../../redis/driver/cluster_recovery.go)在连接错误后刷新拓扑；Session 的 RequestID、run 与 claim 仍由各自 Redis store 维护。本轮从六个不同 hash tag seed run，再强杀其中一个 master，在副本提升后用新 Go 进程完成六个 owner，证明该**已实际存活数据**可从新拓扑继续。脚本没有执行 `WAIT`，但强杀前的 CLI 查询与进程启动耗时允许副本自然复制；因此“无 WAIT 的这一次通过”不是“未复制的已确认写绝不会丢”。要检验真正未复制写的业务处理，应控制副本延迟/隔离、制造已确认但未复制的窗口，再分别验证缺失 run、claim、RequestID 时的恢复或明确拒绝；跨机器故障域与双向分区仍单列。

## OwnerSource 的持久性属于部署接线

[`SweepPending`](../../service/session/sweep_source.go)只调用注入的 `Owners.SweepOwners(limit)` 并将该页交给 `Sweep`。本轮测试侧 OwnerSource 将游标存入隔离 Redis，进程重建后从 100 继续，最终回到遗漏的 owner 1；框架没有替测试自动构建 roster 或证明生产接线公平。接入时要明确 owner 生命周期、游标原子推进与回退、重复页、长时间失败 owner 的重试位置、孤儿 owner 对账以及 backlog 年龄指标。业务资源释放仍需按 `(run, resource, action)` 身份做持久幂等。

## Match 热历史放大并发冲突

[`queueState.clone`](../../service/match/queue_store.go)每次复制等待列表和所有历史 map，Redis versionstore 每次再读写整份状态；这样单 key CAS 能原子提交 Queue/Ticket/Request 关系，但历史终态仍在热值里。四客户端在 4096 条旧终态上竞争一分钟，443 对操作全部完成且最终 Waiting=0，却发生 209 次由调用侧处理的版本冲突，进程内 pair p99 为 3.529 秒、Go 累计分配约 59.46 GB。这说明**当前测试环境中**热点历史对成本与冲突有实际影响；这些数值随机器、客户端、历史分布和负载而变，不能作为生产性能门槛或归档收益预测。[既有归档交接](IMPLEMENTATION-SERVICE-FINAL-SPECIALTIES.md)仍须先定义幂等回放窗口、旧 ticket/request 查询和迁移，再评估拆热值后的一致性与性能。
