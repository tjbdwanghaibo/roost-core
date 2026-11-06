# v1.40.1 修复复验与续审（2026-11-02）

基线：[main / v1.40.1](https://github.com/tjbdwanghaibo/roost-core/commit/b3e9ea69)，主要修复为 [生命周期验收夹具修改](https://github.com/tjbdwanghaibo/roost-core/commit/36db4475)。对比上一轮 v1.40.0，实际运行时没有新增行为修改；新增报告合入、四处测试夹具和发布记录。

**结论：NATS、SLG 原问题在 Linux 复验通过；etcd 停止屏障合理，相关重复验证通过；Lockstep 仍然失败，不能关闭。** 相邻驱动及同步确认／回滚路径续审未确认新的生产缺陷，不代表全仓无问题。

## 修复是否合理

| 项目 | 修改判断 | 本机结果 |
| --- | --- | --- |
| RV-20261102-04 / NATS | 在关闭前建立真实 JetStreamLogClient 并传给统一关闭断言，修正了 nil receiver 夹具，保留 ErrClosed 要求 | 完整驱动 race 包通过；原用例另 `-race -count=5` 五轮通过，无 SKIP，可关闭原问题 |
| RV-20261102-05 / SLG | 不直接把 2 改为宽松阈值，而是创建→初始化→奖励分别要求 Committed/WALUnacked 为 1→2→3，Projected 始终为 0，钱包查询后仍为 3 | 正式 REAL 脚本通过；Player 到账和 Remote owner 交接两条 write→SIGKILL→recover→verify 链路都完成，可关闭原问题 |
| etcd 冻结预算 | SIGSTOP 后用 Wait4/WUNTRACED 确认子进程确实 stopped，再发请求；不修改生产 Resign 行为，清理阶段仍 CONT/Kill/Wait | 完整 etcd driver race 通过；Resign 和 AssemblyClose 两用例各五轮通过，无 SKIP。历史 3ms 成功样本的精确因果未重现，不据此编造根因 |
| Lockstep 快照活性 | 用真实连接发送递增序号心跳，cleanup 关闭连接以解除写阻塞；缩短 IdleTimeout 至 2 秒加强活性验证，这部分方向合理 | **仍失败**：完整包一次失败；顶层用例独立五轮中四轮通过、一轮失败，均为 deep-buffered link 的 written lane |

## 尚未解决：Linux 深缓冲快照连接仍会断开

原样执行：

```bash
go test -race ./framework/sync/lockstep -count=1
go test -race ./framework/sync/lockstep -run '^TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive$' -count=5 -v
```

两次命令均退出 1，失败叶节点相同：

```text
TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive/
  deep-buffered_link/written_lane_follows_the_link
tcp_test.go:921: snapshot over a slow TCP link closed the connection
(closed=true) or failed (0 Tick errors, first: <nil>)
```

完整包中该子例耗时 67.25 秒；独立五轮中的失败子例耗时 66.75 秒。原日志见 [完整包](evidence/linux-followup-20261102/lockstep-package.log) 和 [独立五轮](evidence/linux-followup-20261102/lockstep-repeat.log)。三层 FAIL 是同一叶节点及其父节点，不是三个问题。

进一步定位到测试中仍有一个平台缓冲假设：[`deepLinkReadBuffer`](https://github.com/tjbdwanghaibo/roost-core/blob/b3e9ea69/framework/sync/lockstep/tcp_test.go#L135) 为 16KiB，经 SetReadBuffer 用于中转的 TCP 接收端。同文件的直连慢客户端已用 256KiB，并明确说明过小的接收窗口在 Linux 回环 MSS 下可能触发内核窗口规避／持续定时器，使吞吐远低于夹具声明速率。深缓冲分支仍可能受到同类影响。

本轮仅通过 Go overlay 将 **测试** `deepLinkReadBuffer` 从 16KiB 改为 256KiB，生产代码、心跳、超时、速率和断言都不变；快照大小按现有公式随缓冲上界增加，未缩小负载逃避背压。对照顶层用例五轮全部通过（总计 43.140 秒），包括五轮 admission-only 必须溢出的负对照，见 [对照日志](evidence/linux-followup-20261102/buffer-control.log)。

这支持“深缓冲夹具还受 Linux 接收窗口影响”的定位方向，**尚未证明唯一根因，也不是正式修复**：没有捕获断开原因和 TCP_INFO，不能判定生产 Lockstep 丢快照或承诺任意慢链路不断开。应优先修正／显式建模中转 TCP 缓冲，再在原 Linux 环境复跑完整包和重复场景；若仍失败，记录 Gate close reason、读写 deadline 与链路进度后继续定位。不要撤销心跳或单纯放宽超时。

可复跑的 [诊断脚本](evidence/linux-followup-20261102/buffer-control.py)：

```bash
python3 docs/maintenance/evidence/linux-followup-20261102/buffer-control.py /path/to/v1.40.1-core
```

## 续审及证据边界

- 对 NATS 真实关闭对象、驱动已关闭状态检查、etcd 放弃租约与清理、Lockstep 慢快照背压及心跳收发顺序作相邻核对。NATS/etcd 两个完整 driver 包 `-race -tags integration -count=1 -v` 通过，247 PASS 测试事件、12 SKIP；跳过均为要求独立 `ROOST_NATS_CLUSTER_URL/ROOT` 的日志原语集群套件，不能将两包通过说成所有集群路径通过。见 [驱动日志](evidence/linux-followup-20261102/drivers.log)。
- 原 NATS 用例五轮、etcd 两用例各五轮的独立日志：[NATS](evidence/linux-followup-20261102/nats-repeat.log)、[etcd](evidence/linux-followup-20261102/etcd-repeat.log)。
- `ROOST_SLG_REAL=1 bash scripts/test-slg-generated.sh` 使用本机隔离 Mongo 7.0.45 三节点、Redis、NATS；21 PASS 测试事件、零 SKIP，见 [SLG 完整日志](evidence/linux-followup-20261102/slg.log)。没有把同机原服/owner 逻辑隔离当作跨机容灾或容量验收。
- 新增对递归同步的独立边界探针：父节点完整替换捕获→后续子字段提交→下一事务完整替换但失败→事务中确认旧捕获。分别验证单指针、列表元素、map 元素的后续修改没有被旧确认或回滚清掉，应用结果与权威全量逐字节一致。三个场景各跑三轮 race，通过；[源码](evidence/linux-followup-20261102/review_late_ack_test.go.txt)、[日志](evidence/linux-followup-20261102/late-ack.log)。探针在上一轮独立 DAO runtime 消费工程运行，所用 golden/runtime 生成物相对 v1.40.0 未变化，replace 指向本次 main；不是只检查生成文本。
- Linux x86_64、Go 1.27.2、GOMAXPROCS=4。代码索引工具不可用，按 diff、精确源码和执行日志补证；未修改生产运行时或 main。依赖进程已停止，原失败证据保留。
- 本轮不重跑全仓、Unity、跨机/物理磁盘故障、压测或长稳。旧 [Linux 报告](LINUX-FUNCTIONAL-20261102.md) 与 [新功能报告](FEATURE-REVIEW-20261102.md) 的其他环境限制仍适用。

## 2026-11-03 收尾与上线判断

收尾时重新 fetch，origin/main 仍为上述 v1.40.1，没有后续修复覆盖本次失败。上一轮测试发生于 11 月 2 日，证据文件按原日期保留，本节记录 11 月 3 日交付判断。

**暂不建议以“框架无 bug／生产验证完成”为前提全面上线。** v1.40.1 已有发布 tag，但发布包与生产部署验收是不同结论。当前有实际未通过的 Linux Lockstep 测试；扩大测试缓冲的五轮对照不足以代替 main 修复和原样复验。

上线前最直接的下一步是让维护者处理深缓冲夹具的平台窗口问题，保留负对照并补齐关闭原因证据，然后在确定的候选提交上跑原样完整包、重复用例和全仓门禁。真实客户端/部署镜像的未完成验收按目标产品接入范围补齐。压测与长稳按用户先前决定继续保留为独立事项；本报告既没有执行它们，也没有给出容量承诺。

若项目不使用 Lockstep，应明确将它排除在本次上线能力范围，再以项目实际使用的接线和目标部署环境验收；不能将“不使用这一能力”改写为框架问题已经全部修复。任何有限测试都无法证明整个框架零 bug；可交付的是确定版本、确定功能范围的通过证据和仍然开放的问题。
