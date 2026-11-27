# 订阅预处理：稠密重试中到底发生了什么

**结论：存在大量重复检查；本轮没有把它们当成可跳过的授权。** 原始产品包在 `fix/main-review-fixes@24b704bf` 和 `main@58f47cbe` 内容相同。未启用历史的 A/B 优化。观测器和一处开局就绪检查仅在私有测试 overlay，未给产品 Flush 留探针。下面是单轮归因，不是生产性能验收。

## 原判与分母

1000 玩家/10000 实体，集中 c4、独立私有 Gate、30 秒预热+30 秒计时、60 秒收尾。原程序 FAIL/1：正式提交的 49791 条增量全部在截止后到达；All p99 10ms 宽桶上界 **10390ms**（门槛 100ms）。业务、心跳、Gate、600000 个业务往返及全员含 self 终验通过，收尾 25.278s。探针会影响重负载的时序，不把该数值当作未探针的 main 数值。64/512 smoke 原判 PASS/0，137 条正式增量（108 窗内、29 窗后），p99 桶上界 10ms。最初 8 人开场遇 `auth ACK` 与服务端 `OnActive` 的先后竞速；就绪条件在发压前逐一确认全部预期 ID、唯一 SID 及 held 状态，不延长正式窗。8 人 1% 改值还出现过原程序零正式增量的 FAIL，未改门槛当作通过。

在正式窗完整、未失败的普通 Flush 有 137 轮。以下严格交集只看相邻两轮都在正式窗、同一个 `*subject` 实例两次真实返回 commit_pending、这次预处理的输入订阅至少 65 个（不是两轮 SID 集合相等的证明）。

| 观察项 | 本轮实测 |
|---|---:|
| 满足交集的整对象访问 / 扫描订阅条目 | 127102 / 123518851 |
| 整次都命中本轮同一 `*session` 工作项 | 126966 次（99.89%） |
| 无会话异常但必须做首次工作绑定 | 136 次访问，136000 次首次绑定 |
| 会话缺失、lifetime 不符、held、同 SID 工作项冲突 | 四类均为 0（在本交集，非全局不可能） |
| 进入时有 leaving 订阅的整次访问 | 78（28280 个 leaving 条目，不等于成功 Remove 帧数） |
| 进入时 snapshot / live 订阅条目 | 113669098 / 9821473 |

所有正式普通对象访问的**已持对象锁后的预处理**计时合计约 7.100s，其中 commit_pending 类约 7.075s。包含观察开销，非 API 内耗时；没有逐次给严格交集计时，不按边数分摊为可节省秒数。同样，之前其他版本上包含锁等待的历史数值不能拼来计算改善。

## 为什么没有改生产预处理

每轮的 work 从空表开始；第一次绑定还会影响后续同 SID 的会话冲突判定。上一轮正常，不能证明本轮仍是同一会话（Hold/Ready 可在同一 lifetime 替换指针），也不能证明在途的 Close 没留下旧订阅。即使同一轮内探针最终记录全是 same_work，也没有一个原决定点的凭据能证明整次遍历里不会有 registry writer。137 轮的首绑次数都达到 1000，只是工作表最终覆盖量，不是 visit **入口**就齐备的证据。

另做了一次实际 Go `-race -count=1` 的固定交错对照（退出 0）：两个 SID 在原循环入口的 work/registry 都逐项相符。无 writer 时，原函数返回 `held=nil`；在 SID 1、2 的实际查询之间让 SID 2 经 Hold 发布后完成 Ready，第二次读到同 lifetime 的新 `*session`，而本轮 work 仍指旧会话，原函数返回 `held={2}`。所以连“入口逐项相符”也不保证后续每个原判定点相符。两组在屏障期间都实际重排两个对象、零帧，放行后都收到两个 Full / 四个 Create；这只证实内部判定不同，不是测到了丢帧。原日志、覆盖差异与哈希在 `artifacts/perf/watcher-three/session-interleave.zip`。

因此不提交“上轮没变就跳过”的方案，不扩大锁区间以换取看似一致的快照。原发布检查、修订复核、Remove、空 Full 及断线重开仍走原路径。本轮未跑新产品 B 的无探针验收，也未修复 M-16。若要再试，应先用影子观察对一个**当前可验证而非历史推断**的失效凭据核对命中/兜底，单独证明有并发 writer 时的行为，再比较正常成功路径的分配、完整 Flush 和无探针原 c4。

## 离线复算

维护者本机主检出的 `artifacts/perf/watcher-three/{c4-watchers-01,small64-freeze,session-interleave}.zip` 保存原报告、trace、原退出码、指纹、生成与私有 Gate/NATS 日志、观察差异和重放材料；2026-11-27 已从 `main-review-fixes` 工作树复制过来，删工作树不会丢。artifacts 由 Git 忽略，不进公开仓库；`session-interleave.zip` 的 SHA-256 为 `b2bd7cdc395acbee665ab9147d50d8ff628915dbec100aef350245d9e2bb7e0d`。原始压缩包 SHA-256：

- 千人：`10d7f9f0af2f7b03582d4d819a8c111070f6c845276d4ae940b6eb326da575d7`
- 64人：`8056d09f851edf01616a7f55aa5eef205cbc0913cd18e970dc973e06a27c7638`

从仓库根执行（macOS / Linux，Python 3），无需 Go 或服务，也不执行证据 ZIP 中的代码：

```sh
out=$(mktemp -d)
unzip -q artifacts/perf/watcher-three/c4-watchers-01.zip -d "$out"
bash scripts/perf/sync-subscriber-review/run.sh check "$out"
```

预期报告 `observer reconciliation OK; original FAIL exit 1`。前者表示逐轮原始统计、直方图和成功帧对齐，后者是产品夹具原判，两者不可互换。观测版与无观测版对同一组固定输入输出一致且六类分支都覆盖，不证明所有并发交错都可安全合并。
