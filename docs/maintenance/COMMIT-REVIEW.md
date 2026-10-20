# 新增提交审查

审查日期：2026-10-14。范围为上次本 agent 提交 `a9fa95d9` 之后到 `9a2d3a8f`：**266 个非合并提交、2 个合并提交**。同步时 main 与 origin/main 一致；审查与文档修改在独立 worktree 完成。完整 SHA 与路径见[提交清单](REVIEW-COMMITS-20261014.csv)。此范围替代此前 `cc635481..4370e6b3` 的阶段清单及最近 28 个提交的局部结论。

本轮按最终合并代码的净变化分组审查，未确认新增运行时缺陷。没有逐个检出、运行 266 个历史中间版本，也不声称全部测试断言和外部依赖组合穷尽覆盖。CSV 的 `net-state-reviewed` 表示纳入最终状态审查，不表示该提交单独通过验收。

## 结论与范围

| 范围 | 核对内容与结论 |
| --- | --- |
| Nest / Entity | ID tail 前驱、完成收尾、延时循环、组锁、资源归还、错误归一化与 short/long/io 接线；未发现拆函数改变完成顺序。Entity 批量加载、Destroy 锁和引用收尾、组件拓扑顺序保持契约 |
| DataEngine / WAL | schema 先校验后水合、投影分段、连续成功前缀、回放退出、WAL 预算/批写/fsync/关闭；Remote+lease 历史组合仍明确拒绝并保留 WAL，未跳过失败记录推进 checkpoint |
| Remote | beginWrite 的 ownership/gate/grant/共享锁及失败出口；WAL 级 Rejected 精简保留内存确定拒绝与回滚，等待失败仍结果不确定，自定义 Store 后续拒绝仍由 finalizer 收尾；发布时刻/删除版本/TTL 的旧形态拒绝与当前发布端一致 |
| Sync / Lockstep | Flush 准备、会话帧前缀结算、失败后重同步、生命周期检查与编码工作区；setter 标脏与提交边界未改变；Lockstep 命名迁移及消费者编译通过 |
| App / Gate / Bus / etcd | 启停依赖与 singleton 释放边界、TCP 握手和消息预算、Forward 的 admission 归还、广播结算、RPC 上下文和选主会话收尾；未发现提取 helper 改变所有权或退出顺序 |
| Service | Account/Activity/Rank/Mail 的旧数据拒绝；对照现行写入端与正常数据路径。移除已明确放弃的兼容本身不算缺陷。计数器前缀冲突 RV-20261014-01 已由 `23c7a997` 修复并随 v1.26.1 发布 |
| Gameplay | Skill 启动、operation 执行、运动阶段、checkpoint 恢复及 effect 解码；20 个新提取解码 helper 的函数体与原 switch 分支去空白后相同，其他阶段对照错误与状态更新顺序；attribute selector 包装保持相同返回值 |
| Codegen / Wiring / Demo | manifest 严格解码、旧 feature/mod/tag 拒绝、API 删除与使用方、playeraccess 改名、配置声明快照、部署回退脚本。实际生成 game-demo 并 build/vet/test；既有业务代码仍需遵循升级说明手工迁移 |
| 脚本 / CI / 文档 | 旧目录路径、生成器版本、私有环境入口、Redis 清理与跳过守卫、Windows 编译边界。修正维护文档中仍称 v1.26.0 为当前版本、已发布修复仍称未发版的状态；不改业务行为 |
| 合并 | `8da1e771`、`eade0fb3` 的 `git show --remerge-diff` 均无冲突解决差异 |

## 测试整理核对

使用 Go AST 的非 import 声明与去注释 token 摘要区分搬文件、改名和行为变化，再针对运行时变化及关键测试删改读源码。摘要只是定位手段，不作为语义等价证明；模板和被索引排除的脚本直接检查。未重新做全仓逐函数覆盖率前后对照，不将本轮描述为每个被删断言的穷尽审计。

已核对的易误判删除包括：

| 原测试 | 当前保留的行为覆盖 |
| --- | --- |
| `TestFirstLoadFailureLeavesNoTypedNilInRuntimeConfig` | `framework/configdata/reload_commit_test.go` 的 `TestFirstLoadFailureSkipsRollbackCallbacks` 保留 typed-nil、Current 与 rollback 断言 |
| `TestRuntimeFailureFirstReportWakesShutdown` | `framework/app/singleton_test.go` 保留首错、Done 唤醒、hook 顺序及后注册 hook；App 退出测试另外覆盖错误仅返回一次 |
| `TestEtcdLifetimeWatcherReadinessAndTerminalCauses` | `infra/network/etcd/driver/watcher_test.go` 分别验证 Ready、compacted 与 closed |
| `TestMalformedEnvelopeIsAnError` | `infra/storage/versionstore/write_token_test.go` 的 `TestAnUnreadableRecordIsReportedAsMalformed` 含分隔符缺失、零版本及旧无 token 记录，并区分传输故障 |
| `TestTemporalRestoreRejectedEmitsTemporalEvent` | `gameplay/skill/temporal_test.go` 的 `TestTemporalEventsNormalizeSnapshotTarget` 覆盖 capture/restore/rejected 三种事件及 target |
| `TestCancelRefusesBlankAndUnknownTickets` | `service/match/guards_test.go` 保留空 ticket、未知 ticket 与错误内容断言 |

## 实际验证

以下均在 macOS、`9a2d3a8f` 的隔离检出上运行，`GOWORK=off`，退出码 0；不使用缓存测试结果。

```sh
go test ./... -count=1 -timeout=8m

go test -race ./framework/nest ./framework/entity ./framework/nestwal ./framework/dataengine/... ./framework/sync/... ./framework/app ./infra/network/gateway ./infra/network/bus ./infra/network/etcd/driver ./gameplay/skill -count=1 -timeout=6m

go test -race ./framework/remoteentity -run 'Test.*(WAL|Rejected|Reject|Finaliz|Pipelined|WriteGate|Shared|Reload|Reply)' -count=2 -timeout=180s
```

生成工程验证：正式生成 game-demo，将 core replace 到审查检出后，执行 `go build ./...`、`go vet ./...`，另运行 `go test -count=1 -timeout=180s ./...`；全量生成工程测试探针通过（37.31s），临时探针已删除。此前单独跑过 Remote、DataEngine、Nest、protocol、redistest、根包与 etcd，以及生成工程 SceneWatcher / PlayerID 行为测试，均通过。

原始输出保留在本地 `artifacts/review/20261014-full/`，不当作性能报告。真实资源测试的 opt-in 环境未开启；包通过不表示 Redis/Mongo/NATS/etcd 集成用例执行。未重跑真实资源故障矩阵、跨机、Linux、长期压力或性能对照；发布文档中的 30 分钟数据是已有记录，不是本轮测量。

CBM 项目 `Users-whb-roost-roost-core`，代际 `2026-10-14T14:59:51Z`，24,550 节点、245,349 边。已对范围内 1,914 个变更 Go/模板路径及项目范围检查 coverage；图谱有 6 个模板局部解析缺口、无 skipped，相关缺口读源码并用实际生成工程补证。docs/scripts/testdata 等按配置排除，直接读文件。无记录缺口不等于图谱完整。

## 已修复问题的历史证据

RV-20261014-01：`1ffb644f` 引入的 allocator 保留键冲突，首次探针在该版本失败、父提交通过。修复 `23c7a997` 在任何 Redis 写入前拒绝冲突前缀，保留正常前缀重启和第二副本递增测试。本轮已复核；已修复、无剩余限制，KNOWN-LIMITS 不再登记，经过见 [CHANGELOG](../../CHANGELOG.md) v1.26.1。以下仅保留历史复现入口，不是当前版本应当通过的测试。

## 复现入口

在隔离检出的根目录新建 `_reviewprobe/account`，将 Demo 的两份 collaborators 模板原样复制为 `collaborators.go`、`collaborators_test.go`，加入下面的测试，运行 `GOWORK=off go test ./_reviewprobe/account -run '^TestReviewFreshCounterSurvivesRestart$' -count=1`。对照旧版时只更换这两份模板为 `1ffb644f^` 的内容，保留相同探针。

```go
package Account

import (
    "context"
    "testing"
)

func TestReviewFreshCounterSurvivesRestart(t *testing.T) {
    client := newCounterRedis(nil)
    first := boundAllocator(t, client, "roost:demo")
    if got := allocate(t, first); got != firstPlayerID+1 {
        t.Fatal(got)
    }
    restarted := boundAllocator(t, client, "roost:demo")
    got, err := restarted.Allocate(context.Background(), 1000)
    if err != nil {
        t.Fatalf("fresh deployment restart cannot allocate: %v", err)
    }
    if got != firstPlayerID+2 {
        t.Fatalf("got=%d", got)
    }
}
```

“绑定时拒绝冲突前缀”的修复已由 `23c7a997` 采用（随 v1.26.1 发布）：正式回归断言 `roost:demo` 在 BindRegistry 即失败且 Redis 零调用，并保留正常前缀的重启与第二副本递增测试；修复后该探针在 `roost:demo` 前缀下预期在绑定处失败，只用于对照旧版。
