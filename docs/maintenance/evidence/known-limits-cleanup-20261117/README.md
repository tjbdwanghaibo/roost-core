# KNOWN-LIMITS 二的两条收口（2026-11-17，协调者）

## 锁改存 etcd（已实施，待最终全量）→ 删除

条目里写的“还没做”三项核对结果：

1. **阶段 7 全量**：该分支已合入 main（合并提交 `0b7779f0`），v1.52.0 发版时 `scripts/release.sh` 跑过全量 pretag（codegen 包、模块门禁、genre 脚本），见 [round-20261116](../round-20261116/README.md)。
2. **真实进程演练** `scripts/testdata/singleton-etcd-drill/singleton-etcd-drill.sh`：已跑，含 etcd 停 20s 后 B fail-stop、etcd 恢复 18s 后同 sid 重新拿到锁（重连延迟实测），同上证据“单实例锁 etcd 演练”。
3. **先失败后通过**：今天补做。去掉续锁里“KeepAliveOnce 失败即返回”后 `TestSingletonStoreRenewalWhoseKeepAliveFailedIsNotApplied` 变红（`renewal with a failed keep-alive = true, "", <nil>; want the keep-alive error and applied=false`），还原后通过，见 [etcd-keepalive-mutation.txt](etcd-keepalive-mutation.txt)。用例自起 etcd（`-tags integration`），跑完进程随测试退出。

etcd 不可用时 fail-stop 已在 KNOWN-LIMITS 一，不受影响。

## 登录潮里 account CreateRole 超时，尚未确认 → 关闭（维护者 2026-11-17 定“关掉”）

只在 chat 抢写风暴下复现过（2 轮中 1 轮），无风暴的 21 轮没有；能复现它的风暴条件已由 chat 写排队与公告合并去掉。维护者定：记为“触发条件已消除、未归因”，关闭，不再为它加指标。以后再出现时按新问题处理（account 的 Redis 连接池、NATS 排队、整机 CPU 三个方向当时没有排除）。
