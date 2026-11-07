# v1.42.1 修复复验与新增修改审查（2026-11-07）

基线：[v1.42.1 / main](https://github.com/tjbdwanghaibo/roost-core/commit/773453d3)，对比 [上轮 v1.42.0](https://github.com/tjbdwanghaibo/roost-core/commit/a6235227)。本轮检查新增 7 个提交，90 个变更文件中大量为合入历史审查证据；运行时变化集中于 Nest、best_effort、cellworld 与联盟 demo。代码索引不可用，使用 Git diff、源码及 Linux 实测。固定分支只提交报告与探针，不改 main 实现，不启动压测。

## 结论

四项原确认缺陷的修复方向与实现合理：失败创建 best_effort、单 worker 停止屏障、旧 Leave、BaseState 编码。联盟的周期扫描解决首次迁入的补发，但扫描缓存仍存在迁出/迁回竞态，RV-20261107-01 **部分修复，不能关闭**。没有把已修四项继续作为未修问题重复报告。

## RV-20261107-03 / P2：迁出前的扫描缓存失效可被重新覆盖，迁回后漏补宣战

位置：[TransferOwnership 的 picked.drop](https://github.com/tjbdwanghaibo/roost-core/blob/773453d3/demo/internal/rpc/alliance/transfer.go.tmpl#L114)、[扫描过滤](https://github.com/tjbdwanghaibo/roost-core/blob/773453d3/demo/internal/rpc/alliance/war.go.tmpl#L210)、[扫描标记](https://github.com/tjbdwanghaibo/roost-core/blob/773453d3/demo/internal/rpc/alliance/war.go.tmpl#L256)。这是对上一项修复的遗漏，仍是联盟 demo 的补偿接续问题，不是 Remote 所有权 CAS 失效。

当前 drop 放在实际 TransferRemoteOwnership 之前。drops 计数只能挡住在 drop **之前**拿到 mark 的扫描，挡不住以下合法时序：

1. A 拥有联盟 X，曾扫描并标为 picked。
2. A 将 X 迁至 B：先 `picked.drop(X)`，尚未调用实际所有权转移。
3. A 的周期扫描此时看到 X 仍属于 A，取得新的 mark，读出空 WarAcks，重新 `picked.add(X, mark)`；计数没变，所以成功。
4. A 完成迁出，但没有再次使 picked 失效，错误标记留下。
5. B 将 X 迁回 A；在待确认检查之后、fencing 之前，X 宣战成功并留下 WarAcks。
6. B 的 follow-up 看到不再拥有 X 而退出；A 的周期扫描由于 picked 中仍有 X，永远跳过它。对方没有战争记录，WarAcks 一直保留。

用户可见后果仍是扩缩容/联盟往返迁移后宣战双方不一致，需要重试原请求、重启或人工恢复；不是仅等待一个扫描周期即可恢复。

[探针](evidence/fix-review-20261107/review_scan_transfer_test.go.txt) 使用正式生成工程的 cluster/Nest/Sender 和已有 ownership hook。只在迁出 drop 之后、实际 transfer 入口前执行一次完整 scanWarsOnce，随后构造迁回时的宣战。三轮均观察到后续三次扫描仍遗漏；不插入该扫描的对照三轮全部通过，见 [红色输出](evidence/fix-review-20261107/scan-transfer.log)。这是确定性时序注入，ownership/transport 仍使用现有内存夹具，不宣称已在真实 Mongo 上复现并发。

以 Go overlay 在转移成功后再次 drop，使窗口内的已完成标记和仍在途扫描 mark 一并失效，原探针两组各三轮通过，见 [定位对照](evidence/fix-review-20261107/scan-control.log)。该 overlay 仅证明根因，不作为正式修复交付或完整并发正确性证明。建议围绕迁移前后失效或 ownership generation 设计缓存生命周期，同时覆盖转移失败、扫描已取 mark 但未完成、再次迁入等边界；不要只扩大扫描频率。

复跑：用目标版本正式 `project new planet -module example.com/planet -template game-demo --skip-deps` 生成工程，go.mod replace 指向目标 core，将探针放到 `internal/rpc/alliance/review_scan_transfer_test.go`：

```bash
go test -mod=mod -race ./internal/rpc/alliance -run '^TestReviewScanBetweenDropAndTransfer$' -count=3 -v
```

## 逐项修复判断

| 原问题 | 实现检查与状态 |
| --- | --- |
| RV-20261106-01 失败创建事件 | createdBuildMark 纳入 bestEffort 长度，失败构建有外部效果就使整个事务回滚。原探针及新回归通过，关闭原问题 |
| RV-20261107-02 Stop 提前完成 | acceptMu 下关闭准入，与 tasks.Add 排序；单 worker 也计入在途，Drain 排空后停止接纳；排队 worker 取得名额后复查取消。原探针和新 Stop/Drain 回归通过，关闭原问题 |
| RV-20261106-03 迟到 Leave | 每次入场 Entry、Enter/Leave/Offer 透传、目标 Real 比较 Entry；demo DAO 映射同步补齐。保留跨 Base 重启的系统时钟不回拨前提，wire v6 要求停止旧进程再升级 |
| RV-20261106-04 BaseState 编码 | x/y/vx/vy 拆开，直接 MsgPack 编码回归覆盖完整状态。旧格式不迁移是明确的未上线升级边界 |
| RV-20261106-02 边界抖动夹具 | 改为有期限地等待真实 owner/epoch/角色/witness 收敛，并额外禁止靠交接放弃收尾；保留视野连续性断言。设计合理，Linux 完整包及重复验证通过，可以关闭此次夹具问题；不扩大为所有网络故障组合均通过 |
| RV-20261107-01 宣战迁移补偿 | 新增周期扫描、空认领跳过、正常往返接续回归有价值；本轮发现 picked 失效窗口，不能完全关闭 |

最近修改的运行时路径均已按上述模块复核；新生成工程也覆盖 CellEntry 接口/访问器/分区映射与联盟服务接线。没有发现需要另外登记的 Nest 或 best_effort 新缺陷。

## 已登记但不能关闭的发布风险

Lockstep 深缓冲快照链路本区间没有修改，本轮不重复运行同一慢用例，也不将其关闭。

main 新登记 Saga 压测单写者偶发 `reason=fenced`，根因未确认。本轮复核 publishChunk/lastIsMine 与 NATS leader LastMsg 路径，确实在期望序号不符且最后 MsgID 不匹配时 fail-stop；这些源文件本区间未改。普通 Saga/NATS race 通过不能否定压测样本。原 NATS 数据目录和详细日志不在本 checkout，没有将“在途提议与已落盘状态差异”当成已证实根因，也没有把没有观察到余额错误扩大为所有故障组合安全。按用户要求保留压测，不新开压测任务。

## 验证

Linux、Go 1.27.2、GOMAXPROCS=4。下面记录本轮实际执行结果；未启动真实集群、Unity 或性能验收，不用包级成功代替这些验收。

- Nest、wiring/dataengine、cellworld 完整包 race：三个包均通过，见 [定向完整包](evidence/fix-review-20261107/targeted.log)。包含前轮保留的原复现探针。
- Nest / best_effort 原探针及新修复回归各三轮：通过，见 [重复验证](evidence/fix-review-20261107/fix-repeat.log)。
- cellworld 原探针、Late Leave/交接、编解码、普通与延迟收尾抖动各三轮全部通过，夹具等待不足修正获得本机 Linux 验证，见 [cellworld 重复验证](evidence/fix-review-20261107/cell-repeat.log)。
- 正式 game-demo 自定义 module 全量 race：32 个有测试包通过，退出 0，见 [生成工程](evidence/fix-review-20261107/demo-race.log)，运行时移出本轮预期失败探针，不修改原仓库测试。
- Saga / NATS driver 普通 race：通过，见 [日志](evidence/fix-review-20261107/saga.log)。未设置真实依赖门变量，不声明零 Skip。

新增缺陷已有确定复现；结合联盟未完全修复、Lockstep 未闭环和新登记的 Saga 偶发 fail-stop，仍不能给出生产上线验收通过的结论。
