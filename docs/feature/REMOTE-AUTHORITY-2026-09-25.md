# Remote 权限修复与批量投影验收

2026-09-25，基线 967bc69 + 当前工作树，未发布。沿用 [实施方案](REFACTOR-2026-09-25-remote-authority.md)；历史长稳数据见 [2026-09-24 报告](REMOTE-ACCEPTANCE-2026-09-24.md)，不把旧协议结果冒充本轮新协议验收。

## 已完成

1. [RR-20260924-26](../bugfix/RR-20260924-26.md)：正式 Remote 所有权与写许可迁入 Mongo majority+journal；Redis 仅协调竞争，提交原子校验最新许可。两种写模式均覆盖，用户后续确认未部署，当前默认统一严格许可校验，移除迁移入口（见下述后续报告）。
2. 同一 Remote 事务按数据库/集合合并 DAO 和快照操作。双 Entity × 双 DAO、每 DAO 一个快照时，payload 从 8 次单操作调用降到 3 次集合 BulkWrite；保留原子事务和集合内顺序，不跨独立 WAL 事务合并。后续进一步将已迁移实体的 metadata CAS 合并为一次 BulkWrite，双实体从 2 次降到 1 次；MatchedCount 必须等于实体数，否则整个事务回滚。
3. 授予许可使用单次 FindAndModify 返回当前版本并递增 fence，减少先读后写的往返。已确认授予但 Redis 租约失效时，在业务执行前安全重新争锁；未知结果不盲目重新授予。
4. [RR-20260925-01](../bugfix/RR-20260925-01.md)：修复真实测试夹具遗漏删除 JetStream 流，网络故障补跑无新增残留。历史 56 个流首次清理后剩余 33 个回复停滞，之后按每项 2 秒有界清理并独立查询列表，最终残留为 0；保留中间日志，没有重置测试集群或强删数据目录。

仍在同一个 `remoteentity` 包，只增加职责清楚的文件。增加中文契约注释和包文档，不新增 Entity/WAL 字段，不改变 digest 与持久确认边界。

## 正确性与故障

- `go test -race ./remoteentity ./dataengine/engine ./kit/remoteentity ./kit/dataengine` 通过。
- 相同包 `go vet` 及 `go vet -tags=integration ./remoteentity ./kit/dataengine` 通过；脚本语法和变更空白检查通过。
- 真实生成业务链：三种持久策略的 2 Entity × 2 DAO、拒绝/panic 回滚、Mongo 与 NATS 快照核对通过。
- 真实六节点 Redis 丢复制写：保留 Redis fence **1→1** 的失败机制，正式 Mongo fence **1→2**，旧提交拒绝，新提交成功。
- 两个独立业务子进程：旧进程 Redis 分区后仍能访问 Mongo；新进程接管，旧提交在重连前后均被拒绝，新进程提交成功。
- 21 组矩阵覆盖 Mongo 主节点/多数派、NATS 单节点/全集群、进程退出/分区、Redis 选主/未复制写、ownership 边界、WAL 恢复、JetStream 与代理网络故障。

矩阵首轮 **18/21**：两项短租期在 Mongo 恢复时过期，一项测试流容量耗尽。修复后分别复跑三项，全部通过，因此 **21 组均有通过证据**，不是声称同一最终二进制完整连续跑了一遍 21/21。首轮失败日志保留。证据：

- `artifacts/perf/remote/authority-matrix-20260925/`：原始完整矩阵。
- `artifacts/perf/remote/authority-verification-20260925/consolidated-results.tsv`：每项通过来源和修后补跑日志。

## 容量测试

单机 Apple M5，Go 1.27.0，GOMAXPROCS=4，真实 Mongo/NATS 三节点与 Redis；1000 个业务会话、10000 个实体，每事务 2 Entity × 2 DAO，先完整预热 5000 个事务。默认 24 小时锁 TTL、5 分钟快照缓存、64 Nest worker，Interest 持续续订。Remote TPS 独立于客户端 Sync 的 20 Hz。

初版持久权威 + 批量投影的 100 TPS/30 秒探测仍失败：98 成功、2902 错误、0 丢弃，首错 `nest: sync timeout`。该样本之后还优化了 grant 的单次 FindAndModify 和短租期重试；不能据此估计最终版本的精确容量，更不能宣布 100 TPS 已通过。失败样本未通过全量数据核对，原始日志保留于 `artifacts/perf/remote/authority-strict-100-20260925/`。

第一轮 20 TPS/60 秒（尚未合并 metadata CAS）全量校验通过：1200 成功、0 错误/丢弃，完成吞吐 19.733 TPS，p50/p95/p99/max 为 186/1038/1757/2706ms，采样最大 WAL 未确认 20、最终 0。它证明正确性，但延迟明显高于旧协议基线，因此继续合并 metadata CAS；原始数据保留于 `artifacts/perf/remote/authority-final-strict-20-20260925/`，该目录名是创建时的标签，不代表最终代码版本。合并后的同参数复测在下节记录。吞吐优化不能仅从命令数推导固定百分比提升；新增持久许可本身也有成本。

## 合并 metadata CAS 后的最终实测

目录 `artifacts/perf/remote/authority-bulkmeta-strict-20-20260925/`；Go 测试、脚本退出以及 `.verified` 全部通过。计时 60.149 秒，含输入尾部等待；不含初始化与 5000 笔预热。

| 指标 | 合并前 | 合并后 |
| --- | ---: | ---: |
| 成功 / 错误 / 丢弃 | 1200 / 0 / 0 | 1200 / 0 / 0 |
| 完成 TPS | 19.733 | 19.950 |
| Request p50 | 186ms | 183ms |
| Request p95 | 1038ms | 342ms |
| Request p99 | 1757ms | 386ms |
| 最大延迟 | 2706ms | 721ms |
| 采样最大 WAL 未确认 | 20 | 4 |
| 最终 WAL 未确认 | 0 | 0 |
| Mongo 文档 / NATS 快照核对 | 20000 / 20000 通过 | 20000 / 20000 通过 |

相同夹具的单次前后样本，p99 下降约 78%，p50 基本不变。事务吞吐受输入 20 TPS 限制，不能用这两个值推导系统最大吞吐。新协议的许可持久化仍有额外开销，本轮未达到 50ms Remote strict 返回延迟；它也不代表客户端 Entity Sync 的 50ms 同步指标。负载结束后 6200 个事务（含预热）全部投影，Remote 活跃事务归零，末尾拒绝/panic 回滚与 outbox 排空校验通过。

最终 bulk metadata 代码另外重跑了三种策略的正式生成业务链、真实 Redis 未复制写和双进程旧写拒绝，全部通过；日志已保存到 `authority-verification-20260925/`。没有重新将整个 21 组矩阵串行跑一遍，未以补跑覆盖此前失败记录。

## 使用与未完成验收

正式 `Assemble` 自动要求 durable authority；自定义 backend 须实现 `WriteAuthorityProvider` 并在事务中验证许可。当前 getter 为无副作用的 WriteAuthority()；默认协议与接口收敛见 RR-20260925-02。

复跑入口（先加载隔离环境的 env.sh）：

```sh
ROOST_REMOTE_DURATION=24h ROOST_REMOTE_RATE=20 ROOST_REMOTE_POLICY=strict \
  ROOST_REMOTE_LABEL=authority-soak-24h bash scripts/perf/remote.sh
ROOST_REMOTE_MATRIX_LABEL=authority-matrix-next bash scripts/test-remote-matrix.sh
```

新协议的 30 分钟/24 小时长稳、多主机部署、真实业务流量回放和 100 TPS 稳定容量尚未验收；本轮不宣称 Remote 生产验收全部结束。NATS 故障读取采用已有的权威回填和已知最小版本，不等于 Core NATS 自动持久重放。

代码索引更新到 `2026-09-25T00:11:34Z`（北京时间 2026-09-25），15 个相关源码路径 coverage metadata_match、无记录缺口；bulk metadata 改动后的两个路径另行确认 metadata_match；3 个历史 demo 模板解析缺口不涉及本次，docs/scripts/testdata 按排除规则直接读取。

## 后续修复与复测

[未部署前提下的 Remote 收敛与复测](REMOTE-UNIFIED-2026-09-25.md) 记录最新代码和验收，本页保留上一轮原始性能数据。
