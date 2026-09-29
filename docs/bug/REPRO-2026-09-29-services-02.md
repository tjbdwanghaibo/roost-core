# Service 第二轮反例复跑

基线 `e10dd3f18d2da985f969cbc54cb19a87f1f287af`。五个 `.go.txt`、复跑脚本和摘要在 [evidence/service-review-20260929b](../review/evidence/service-review-20260929b/)。测试依赖相应包已有 helper，只在独立 worktree 注入，生产目录不常驻测试。问题解释见[交接](REVIEW-2026-09-29-services-02.md)。

从包含本轮文档的提交使用一个独立 worktree；不要注入其他 agent 的工作目录。脚本要求 `.git` 为文件、五个目标不存在，清理仅限内容未变化的自身副本。

```powershell
$env:GOCACHE='D:/whb_s/.gocache'
& D:/whb_s/.tmp/service-repro/docs/review/evidence/service-review-20260929b/Run-Repro.ps1 `
  -RepoPath D:/whb_s/.tmp/service-repro -RedisAddress 127.0.0.1:16390
```

地址必须是自己控制的测试 Redis，脚本不负责启动/停止 Redis。本轮 Redis 无持久化，ABA 每次使用随机 prefix，只关闭自己创建的客户端；服务器临时数据随本轮自有实例关闭而丢弃。不能把地址指向生产或共享环境。要求 Go、CGO/race 工具链。

| 文件 | 包 | 顶层结果 / 对应问题 |
| --- | --- | --- |
| account.go.txt | kit/service/account | 2 FAIL：RR-11、12 |
| global.go.txt | kit/service/global | 2 FAIL：RR-13、17；后者含 3 个 FAIL 子场景 |
| session.go.txt | service/session | 3 FAIL：RR-14、18、旧 RR-20260909-02 新触发 |
| activity.go.txt | kit/service/global/activity | 1 FAIL：RR-15；1 PASS：未认定 bug 的重叠观察 |
| mail.go.txt | service/mail | 1 FAIL：RR-16 |

修前预期 **exit 1，9 个顶层 FAIL + 1 个观察 PASS**；不是工具运行失败。修后九个缺陷断言应转绿。观察测试用 `TestReviewObserve...` 区分：如正式设计收紧完成边界，应修改该观察断言与公开契约，不能把“保持旧行为”作为修复门禁。脚本故意拒绝空 Redis 地址，确保 ABA 不悄悄降成内存证据。

反例依赖当前存储接口的暂停钩子。若 ABA 修复引入原子条件删除接口，应把钩子移到对应操作之前并保留唯一 live run 断言；旧钩子不再触发导致的超时不是业务红测。若数值或身份输入契约合法收紧，也应同步更新边界断言与兼容说明。

```text
VerifiedIdentityEncodingIsInjective: different verified pairs -> vendor:a:b
LostSlotReplyCannotDeleteCommittedRole: slot v2 -> deleted role 1000001; retry ErrRoleLimit
LeaseCannotRenewAgainstChangedRoute: route=(sid=2 epoch=3) lease=(sid=1 epoch=1)
AttachCannotAssertAlreadyReleased: Releaser calls=0
PositiveProgressCannotWrapNegative: score=progress=-9223372036854775808
TransientEnvelopeFailureCanRetrySameRequest: ledger names missing mail mail-1
LeaseSnapshotsOwnLoadMaps/{renew,lookup,live}: load=forged version=2->2
SuccessfulFinishRetryFreesClaim: terminal owner's claim remains
TerminalFinishCannotDeleteReacquiredClaim [real Redis]: run-2/run-3 both live
ObserveProgressCompletionOverlap [PASS]: admitted write lands after complete; not a confirmed bug
```

完整可重建输出见 [results.txt](../review/evidence/service-review-20260929b/results.txt)。原有 7 包 race/integration 回归通过，425 个测试/子测试、无 skip，命令见[运行记录](../review/REVIEW-2026-09-29-services-02.md)。本轮测试没有真实网络丢包、Broker、业务发奖、Redis HA 或多进程强杀；人为控制交错与故障点只声明其所证明的协议缺口。
