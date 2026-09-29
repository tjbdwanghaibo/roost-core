# 第三轮 Service 复现材料

基线 `83c042438ea9ae235cfe3a41a1a7217949139670`。四个 `.go.txt` 是审查文档附件，通过 Go overlay 加入现有测试包；不会复制为源码文件或修改生产实现。需要支持新增虚拟文件 overlay 的 Go（本轮 Go 1.27）。

[正式问题](../../../bug/REVIEW-2026-09-29-services-03.md) · [运行记录](../../REVIEW-2026-09-29-services-03.md) · [结果摘要](RESULTS.json)。

先准备隔离的测试 Redis，本脚本会写独立前缀数据，不启动/停止 Redis、不清理整库。两个新环境变量只在脚本期间设置，结束恢复；独立前缀持久记录由该测试 Redis 生命周期清理。

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File D:/whb_s/cube-core/docs/review/evidence/service-review-20260929-03/Run-Repro.ps1 -RepoPath D:/whb_s/cube-core -RedisAddress 127.0.0.1:16392
```

可用 `-Mode memory` 或 `-Mode redis` 分组。memory 组的 Directory 用例内部同时运行 Memory 和 Redis，仍需 Redis。脚本对单仓/隔离工作树均可用；overlay 清理仅删除本脚本生成的临时 JSON，不删除源码。

本轮已经执行该脚本的 all 模式，输出 exit 1、没有 skip；对应当前缺陷断言失败，不能将任意编译/环境错误也当作复现。RESULTS.json 保存前一轮相同 Go 命令的最终逐测试事件及脚本检查摘要。

| 材料 | 断言 |
| --- | --- |
| account_review_test.go.txt | 三个写后未知失败；三种写前控制通过；Profile Memory 两入口失败、Redis 对照通过 |
| directory_review_test.go.txt | Cancel/Release × Memory/Redis 四交错失败；门控同时保留 ConditionalDeleter 能力，方便未来验证 |
| mail_review_test.go.txt | 旧取消释放新预约失败；过期 pending proof 容量及显式 Delete 后同 Send 恢复为观察通过 |
| session_review_test.go.txt | 并发 Finish 两次回调为观察通过，不假定外部适配器双重 free |

脚本合计 25 个叶子场景，14 个缺陷反例失败、11 个控制/对照/观察通过；父测试事件不重复计。真实断网、强杀和发奖方不在复现内。Directory 门控用 go test 的 2m 超时限制意外停滞。

修复实现调整 API/阶段协议后，要按新契约更新这些附件或转为正式行为回归，再重新执行；不能只改断言消掉失败。本轮容量最初的“必须自动回收”假设已纠正，最终观察明确保留未结算证明。
