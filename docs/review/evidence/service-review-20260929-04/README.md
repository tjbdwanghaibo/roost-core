# Service 第四轮可复跑证据

源码基线 `bdbb61bc4340c5c6261f746f882039e6cc4f3928`，Go 1.27。三个 `.go.txt` 是审查附件，使用 overlay 给包添加临时测试，不修改源码或 go.mod。复用基线现有测试 helper；未来 API/helper 变化需迁移，不能将编译失败算作 bug 反例。

```powershell
# 在 Core 根目录执行；Redis 必须是自己隔离的测试实例。
./docs/review/evidence/service-review-20260929-04/Run-Repro.ps1 -Backend memory
./docs/review/evidence/service-review-20260929-04/Run-Repro.ps1 -Backend redis -RedisAddress 127.0.0.1:16394
```

16394 是本次临时实例的历史地址，已在任务收尾关闭；脚本不会启动/停止 Redis。重跑自行选空闲端口/独立目录，不使用生产库。Run-Repro 恢复进程环境，overlay/log/summary 写临时输出目录；Go 退出 1 是当前源码预期的安全断言失败，脚本返回实际统计。

| 叶子场景 | Memory | Redis | 结论 |
| --- | --- | --- | --- |
| UnknownGrantMustBlockSettlement | FAIL | FAIL | RR-23，模拟外部已生效后超时，pending 清空、允许结算 |
| UnknownGrantMustNotGrantAgain | FAIL | FAIL | RR-23，非幂等受控发货器计数 2 |
| KnownNoGrantControl | PASS | PASS | 对照，仅确定未发货的 fixture；不是通用错误均安全的证明 |
| CallbackReceiptMustOwnPendingProof | FAIL | PASS | RR-24，仅 Memory 返回别名 |
| DeletedUnclaimedMailMustNotResurrect | FAIL | FAIL | 旧 RR-20260910-02 删除残余，无第一次发奖 |
| ObserveDeleteInFlightCommitRetention | PASS | PASS | 观察 Commit 在淘汰前 missing、后 claimed |
| MatchUnknownCommitRecovery/after_write_false | PASS | PASS | 写前拒绝、tickets 保持 waiting、重试成功 |
| MatchUnknownCommitRecovery/after_write_true | PASS | PASS | 写后未知，从 ticket 找回 match；重试 conflict 无第二 match |

16 个叶子执行，7 FAIL/9 PASS。JSON 含两个 Match 父事件，Memory 9 事件、Redis 9 事件；[RESULTS](RESULTS.json) 保存实际统计、关键输出和证据 SHA256。Redis 仅为真实订单/邮箱/队列存储，外部发奖是可控函数，不是实际游戏资产服务器。

[COVERAGE](COVERAGE.json) 记录图谱 generation、freshness 和源文件限制。完整现有回归 16 包/794 pass，定向补 Redis 2 包/15 pass；相关命令和未验证范围见[运行记录](../../REVIEW-2026-09-29-services-04.md)。不能用绿的基础回归掩盖红的新增不变量。
