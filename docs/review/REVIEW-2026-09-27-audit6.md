# 2026-09-27 第六轮独立审计（本地 main `001b03b`）

范围：[OPEN-ITEMS-2026-09-27](OPEN-ITEMS-2026-09-27.md) 批次 1～12 的修复 RR-20260927-01～28（08 未使用）、B40 测试修正、A06 gofmt、RR-20260926-73 复核补修、USER_GUIDE §4 判别表整体一致性。
两名非修复方审计员分两路在导出副本上核验（各修复提交的父提交为修前，必要时补无行为 shim）。图谱已过期，结论来自源码与实跑。

## 修复核验结论

全部 27 条修复“修前红、修后绿”成立。约束方面：RR-09（拒绝点放在 `FinalizeLocked`）、RR-17（新增显式前缀配置）偏离清单候选，论证成立；RR-25、RR-27 部分成立（见 RR-20260927-29 / 30）。
RR-01 的 Windows 分支按 Go 1.27.0 源码与交叉编译判定成立，未在 Windows 实跑（待 CI）。USER_GUIDE §4 判别表 1～12 行连续无重复，行序与 `dispatchNest` 包装顺序一致。

门禁（审计员）：审计范围内各包 `-race -count=20`、整仓非 race、glsvet、vet、`GOOS=windows` vet、sync-modes、HEAD 生成 game-demo（17 包、doctor 114s / 119s、diff 0）、`d4234c9` 旧工程经 HEAD sync 一次收敛，均通过。
压力探针：entitysync 注销 / 撤回并发 3 × 约 110 万次操作无死锁 / race / panic；B40 在 race 门禁并行负载下 `-count=2000` 通过。

## 新登记

- [RR-20260927-29](../bug/RR-20260927-29.md)（P4，RR-27 引入）：领头方正常完成时不关 `leaderAway`，迟到 `RunLocal` 永久阻塞。
- [RR-20260927-30](../bug/RR-20260927-30.md)（P4，RR-25 不完整）：含装着 func 的接口字段的 Mutex 仍 panic。
- [RR-20260927-31](../bug/RR-20260927-31.md)（P3，修前即有）：Cast 捕获失败被吞后事务照常提交。
- [RR-20260927-32](../bug/RR-20260927-32.md)（P4）：提交前拒绝不带 `ErrCommitRejected`、判别表无兜底行、RR-06 在 RollbackNone 下的“已回滚”表述。
- 清单 B42 / B43（推断）：多 ManagerAccess 下积压 gauge 互相覆盖；Guard 跨 Manager 同 ID 时 RR-21 判定。

## 已直接更正的文档

- RR-20260927-18 / 19 / 23 的兼容性：`scene.go` / `scene_test.go` 是脚手架只写一次的应用文件，sync 不更新，已生成工程须手工合并（原记录与 CHANGELOG 写成“sync 即得到”）。
- CHANGELOG 的 gofmt 文件数（21 → 32）；RR-20260927-03 的回滚步骤补进 demo/README（新版本不推进旧键，回滚会重发 id）。

## 疑点（未登记）

- RR-06：检查与 Commit 之间有检查到使用的时间窗（别的 goroutine 其间 fence 不拦，嵌套场景同 goroutine 确定）；fence 原因以 `%v` 写入，类型化错误不能再 `errors.As` 取到。
- RR-18：`Scene.Close` 由 `Service.Shutdown` 以 `context.Background()` 调用，停重载不受 App 停机时限约束（worker 在 ctx 取消后返回、Nest 仍在运行，正常有界；修前即有）。
- RR-05：`shutdown:` 段注释只举 dataengine 30s 一例却写“40s declared”（文案不完整，改文案会使 sync 认不出未手改段，按现机制保留）。
- B40：state_strict 子用例的 `decided` 计的是 Create 返回总次数，不是“每个 follower 至少一次”（只使占满快池的前提变弱，不误报）。
- RR-03：`-redis-password` 出现在开发脚本命令行（按清单推荐接受）。
- RR-11 / 21：fence 后再开的嵌套独立事务仍会提交（设计内）。
