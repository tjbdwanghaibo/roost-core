# Service 反例复跑（2026-09-29）

基线 `6b73289cefd4e300a75f2550d4d68646172294ad`，问题见 [交接文档](REVIEW-2026-09-29-services.md)。本目录下的 [evidence](../review/evidence/service-review-20260929/) 保存六个 `.go.txt` 文件、一个复跑脚本、逐路径清单及结果摘要，生产包中不常驻本轮临时测试。

## 使用独立工作树

先从包含本次文档的提交创建 **独立** Git worktree，基线 service 实现未修改。不要在其他 agent 的共享主工作目录注入临时测试。脚本要求目标 `.git` 是 worktree 文件，目标六个测试路径不存在；无论测试成功还是失败都清理自己复制的文件，内容被其他人改动时保留并报错。

```powershell
git worktree add --detach D:/whb_s/.tmp/service-repro HEAD
$env:GOCACHE='D:/whb_s/.gocache'
& D:/whb_s/.tmp/service-repro/docs/review/evidence/service-review-20260929/Run-Repro.ps1 `
  -RepoPath D:/whb_s/.tmp/service-repro -RedisAddress 127.0.0.1:16389
```

需要可用 Go/race 环境和一个自己控制的测试 Redis。本轮启动的 Redis 仅监听 loopback 16389，`save ""`、`appendonly no`，没有连接生产 Redis。地址为空时脚本拒绝执行，避免把真实 Lua 证据降成 mock；rank 使用独立随机 prefix，完成后 Reset 自己的榜。

反例依赖各包当前已有测试 helper，不能作为独立 Go 文件运行。六个文本文件分别注入以下包的 `zz_review_20260928_test.go`：

| 材料 | 目标包 | 反例数 |
| --- | --- | --- |
| mail.go.txt | service/mail | 1 |
| account.go.txt | kit/service/account | 1 |
| rank.go.txt | kit/service/rank | 3 |
| platform.go.txt | kit/service/platform | 1 |
| activity.go.txt | kit/service/global/activity | 2 |
| chat.go.txt | kit/service/chat | 1 |

## 结果与如何判断

这些测试断言期望的正确行为，**修前预期 exit 1、9 项 FAIL；修后应为 PASS**。失败退出不是测试工具坏了。修复如合法地收紧 radius/Kind/requestID 的契约，应同步调整反例到新的公开拒绝契约，仍保留红绿对照和旧问题触发证据。

```text
ExpiredMailboxMakesRoom: visible=0 unread=200; fresh Send: mailbox is full
CrossServerRoleRollbackKeepsExistingName: first role exists, name found=false
CommaRequestIsIdempotent [real Redis]: 10 -> 20
AroundAcceptedMaximumRadius [real Redis]: limit 201 exceeds 200
UnchangedMemberCASProtectsRequestRing [real Redis]: replay value=80 want=50
SettledOrderSurvivesLateDelivery: state=delivered note=refunded
LastDispatchRetryKeepsAckWindow: retry=exhausted; original ACK=exhausted
UnconfirmedProgressSurvivesRingEviction: score 33 -> 34, no clock advance
CustomSharedChannelCannotReadPrivatePair: viewer 3 sees from=1 body=secret
```

所有上述反例带 race，未检出 Go 内存数据竞争。受控顺序错误不会因此消失：rank 的 lost update 发生在 Redis 状态协议，platform 的错误是合法并发回写覆盖状态，不一定产生 Go race。

RR-20260929-09 为源码接线缺口，没有对应第十个动态测试。session 现有 Sweep 的功能测试通过不能证明默认 Server 有 owner 来源。

## 基线与生成门禁

原有测试命令见 [审查报告](../review/REVIEW-2026-09-29-services.md)。最终 15 个包通过、684 个 pass 事件，无测试级 skip。最初四个 platform 索引测试因为缺少 `ROOST_REDIS_TEST_ADDR` 跳过；设为同一隔离 Redis 后已补跑通过。

servicerpc 先本地构建，再在各包目录按自身 `go:generate` 参数运行 `-check`：account/chat/global/activity/platform/rank 为 `-dir .`；mail/match/session 的核心域为 `-dir . -emit transport`，kit 对应装配为 `-dir github.com/tjbdwanghaibo/roost-core/service/<name> -emit assembly -out .`。共 12 次，全为 exit 0，无生成文件变更。不要把不同 `-dir` 字面参数导致的生成头差异误当实际传输错误。

没有运行真实业务退款/发奖、网络 Broker、多进程故障和容量压测。平台用阻塞 Deliverer、activity 用单次 mark 失败、其他存储均为现有内存 helper；rank 三项才使用真实 Redis。复跑日志可由脚本输出重建，不提交巨大的原始 JSON 测试日志。
