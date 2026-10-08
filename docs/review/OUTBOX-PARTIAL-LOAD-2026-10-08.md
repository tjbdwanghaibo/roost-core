# Remote 长稳中断记录与继续入口

## 当前状态

维护者在 2026-10-08 要求暂停本机长稳，把资源让给其他重任务，可以继续其余轻量工作。本次已终止测试进程，并用私有环境的 `mirror-local.sh down` 停止 Mongo/NATS/Redis/代理，保留数据目录。没有改动共享测试环境。后续维护者明确授权“除了压测其他的都继续实施”：可以进行编译、功能/并发/集成测试、私有依赖和CBM更新；Remote/Sync压测继续暂停。

代码分支 `codex/outbox-closure`，本轮负载编译基线 `55f2cc20`。main 尚未合入本分支，不能拿 main 的旧性能记录代表此版本。

## 已采集的样本

1000 会话、10000 实体、每事务 2 实体 × 2 DAO；输入 80 TPS，strict WAL；快池 8/队列4096，慢池1024/队列16，Remote写额度256，WAL额度512，投影worker8，GOMAXPROCS=4。计划1小时，实际最后一个采样为240.000981秒，共25个采样点（含起始点）。

| 最后采样 | 值 |
| --- | ---: |
| 完成事务 | 14423 |
| 完成数 / 已采样秒数 | 60.096 TPS |
| 请求错误 | 4520 |
| 调度丢弃 | 0 |
| Remote 写额度拒绝计数 | 4521 |
| 在途写 | 256 / 256 |
| WAL 未确认 | 8 |
| Go heap | 143898024 bytes（约137.2 MiB） |
| goroutine | 2629 |

请求计数和组件统计不是同一原子快照，拒绝数比请求错误多1不能据此认定数据丢失。第60/120/180/240秒的在途写分别255/256/255/256；同期 WAL 未确认3/4/4/8。该证据指向写额度持续占满、完成速度跟不上输入；尚不能仅凭采样把根因归给 Mongo、唯一发布扫描或释放阶段。

停止使用 SIGTERM，中断时仍有在途工作，测试退出码1，脚本报告 `Final data verification missing (test exit=1)`。没有最终一致性核验、完整延迟分布或10/30/60分钟heap快照。不能算1小时通过，不能把这段与后续重跑拼成连续长稳。原有零错误验收条件不变，不以扩大写额度或降低输入直接掩盖退化。

第一轮 label `outbox-1h-80-w256-36ce227c` 在初始化20000个兴趣scope时触及默认配额，尚未开始计时；RR-20261008-45已修复夹具配置。本次样本来自修复后的新label，两个实验分开保留。

## 证据与继续顺序

可携带结论在本文；本地完整副本：`roost-core/artifacts/perf/outbox-closure-20261008/partial-1h-55f2cc20/`，包括 `env.txt`、`preflight.log`、`run.log.gz`、`result.json.jsonl`、`partial-summary.json`。这些产物不提交。分支原`artifacts/perf/remote/outbox-1h-80-w256-55f2cc20/`已另复制到主仓同一证据根下的`worktree-remote/`并核对checksum，随后可以清理工作树。私有资源数据在 `/tmp/roost-outbox-it-20261008/roost-dataengine-it`，env文件含凭据，不复制进仓库。

1. 非压测工作继续：历史条目实施、普通/race/功能故障验收、文档及索引更新。
2. A8锁等待取消与独立事务发布阻塞已取得红证据并修复；完成最终版本的目标race、根包与跨包全量验证。
3. 单独定位本次80 TPS退化：记录 Applied→发布→Committed→资源释放各阶段，核对唯一发布入口串行范围、网络往返、回源次数与finalizer唤醒；profile与正式延迟测试分开。
4. 退化处理后用全新label独立重跑1小时，保留本次失败；本机HA与Linux功能验证、CBM不再等待压测恢复；Sync负载同样保持暂停。

已完成轻量源码与采样分析：[TPS定位记录](OUTBOX-TPS-ANALYSIS-2026-10-08.md)。已确认重构把独立事务发布收成全局串行，仍需阶段计时确定耗时比例；有界并发已实施，功能反例红转绿；没有重跑负载实验。

统一长稳入口仍是 `scripts/perf/remote.sh`；按上表设置 `ROOST_REMOTE_*`，设置 `ROOST_REMOTE_DURATION=1h`、`ROOST_REMOTE_TIMEOUT=2h`、`ROOST_REMOTE_HEAP_PROFILE_MINUTES=10,30,60`。私有依赖使用 `ROOST_MIRROR_LOCAL_HOME` 与 `ROOST_MIRROR_LOCAL_OFFSET=22000`，需要重新启动时先核对端口和所属进程，不复用未知实例。不部署，不自动打tag。

复跑前还要核对故障实验留下的Redis角色和代理上游，不能只看PING成功：本私有环境的负载基线为38379主、38380从；恢复该拓扑并清理toxic、确认复制链路正常后才开始新label。当前没有自动恢复压测。
