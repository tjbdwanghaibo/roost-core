# 新增提交的分批审查

范围按维护者要求扩为全部新增提交，不限 Nest。固定清单使用 Git 范围 `cc635481..4370e6b3`，共 249 个非合并提交；它是提交图的差集，包含并入主线的平行开发提交，不是按日期统计。完整 SHA、标题、变更路径、候选批次及状态见 [提交清单 CSV](REVIEW-COMMITS-20261014.csv)。合并提交还需单独检查冲突解决，不能因清单不列 merge 而忽略。

清单按路径分批，一个提交可以归属多批。`pending` 表示尚未逐项独立审查；`bounded-review-done` 只表示下文列出的有界改动已复核，不等于整个模块验收。没有以文件数或提交数计算行为覆盖率。基线之后的新提交下一轮增补，已经记录的问题不自动视为修复。

## 批次与当前进度

| 批次 | 范围和重点 | 当前状态 |
| --- | --- | --- |
| B1 Nest | 六项热路径优化、派发收尾、错误归一化、completionPump；其余 API 删除、命名迁移与接线 | 核心优化及三项后续改动已复核，见 [独立记录](../framework/NEST-OPTIMIZATION.md#9-nest-优化提交的独立复核)；其余待查 |
| B2 Entity / Remote / DataEngine | 快照格式拒绝、TTL、beginWrite 资源归还、Remote+lease WAL、投影与回放、实体拓扑及 API 删除 | 本批完成三项 Remote 兼容移除核对；beginWrite 仅初读，其他待查 |
| B3 Service / Demo | Account/Activity/Rank/Mail 旧数据拒绝、业务流程、计数器、生成后的使用路径 | 计数器发现 RV-20261014-01，已修复（`23c7a997`，未发版）；其他待查 |
| B4 Codegen / Wiring | 导出 API 删除对生成器、模板、装配、现有工程升级的影响；声明快照一致性 | 待查，不能用仓内无引用证明对外 API 无使用方 |
| B5 Sync / App / Saga / Infra | Flush、网络、RPC、锁、选主、关闭重试及分步重构的等价性 | 待查 |
| B6 Gameplay | Skill 启动、执行、运动步骤，删除符号与状态恢复契约 | 待查 |
| B7 文档 / 测试 / 脚本 / 发布 | 被合并测试的独有断言、旧路径清理、部署脚本、v1.26.0 声明及 merge 解决 | 待查；本轮只核对相关升级说明，不重做发布 |

下一批优先 B2 的 `d688d38b`（Remote+lease WAL 拒绝）、`632c8b4d`（beginWrite 拆分），随后 B3 的 `66b73fab`、`067be870`、`c7bf6af3`、`95d22603` 等服务旧格式移除（以清单 SHA 为准）。每批同时核对相应 B4 接线与 B7 回归，不把功能与使用方拆开验收。

## 本批结论（2026-10-14）

同步前后均为 `4370e6b3`，主工作树干净；文档与临时探针放隔离 worktree。使用当前源码审查，未修改运行行为，未发版。

| 提交 | 结论与依据 |
| --- | --- |
| `1ffb644f` Demo 旧计数键拒绝 | 确认 P2 缺陷 RV-20261014-01，已修复（`23c7a997`：BindRegistry 拒绝保留前缀，退役键报错不再引导删除；未发版），见 [已知问题](KNOWN-LIMITS.md#rv-20261014-01demo-计数器前缀与退役键冲突)。不是要求恢复旧兼容，而是新配置与保留键冲突未被启动校验拦截 |
| `eeb5062f` 缺发布时刻更新拒绝 | 拒绝发生在缓存写入前；现行发布器带时刻；历史消息与有效新消息对照仍保留。本范围未发现新增缺陷 |
| `7afc4c63` 无版本删除拒绝 | 删除必须携带正版本，未清掉已缓存的新版本；乱序删除与更新回归通过。本范围未发现新增缺陷 |
| `d71aa7ac` TTL 索引变更 | 新建索引使用 expires_at，已确认记录与未发布 outbox 的过期边界未由该 diff 改变。旧库迁移被明确移交维护者，不把移除兼容本身报 bug；本机只核对 fake 索引形态，未在真实旧 Mongo 上验收迁移 |

Remote 发布 → 信封身份校验 → 有版本删除 / 过旧更新过滤 → 缓存 ApplyReplica 的步骤仍保留。删除不过时刻过滤，更新需要发布时刻用于判断是否超出 L2 记忆窗口。接收端拒绝旧格式不等于能自动升级旧发布器，仍须遵守发布说明的停旧起新边界。

## 实际验证

```text
GOWORK=off go test ./framework/remoteentity -count=1 -timeout=180s
PASS，10.758s

GOWORK=off go test -race ./framework/remoteentity -run 'TestApplyReplica|TestB2HistoricReplicaPastL2MemoryIsNotAdmitted|TestMongoCommitterCASIdempotencySnapshotAndOutbox' -count=3 -timeout=120s
PASS，2.303s

当前原样 Demo 模板的既有计数器测试（隔离探针）
PASS，0.643s

新增 fresh-prefix/restart 探针：当前模板 FAIL；1ffb644f 父提交模板 PASS（0.662s）
```

探针原样复制 `collaborators.go.tmpl` 与同版本 `collaborators_test.go.tmpl` 为 Go 文件（这两份没有待替换模板变量），使用项目已有内存 Redis 替身；没有运行 Redis 服务，也没有将此结果称为完整生成工程验收。首次旧版对照误用了新版测试夹具，因旧版没有 errRetiredPlayerIDCounter 编译失败；换为父提交对应夹具后，相同新增行为探针通过。编译失败不计为修前红例。

MCP 项目 roost-core 仍为 `2026-10-10T16:08:17Z`，相关路径 metadata_changed，合并后的测试文件部分 not_tracked，已读当前 diff、实现与相关回归补证。Windows 本地验证不扩大支持平台，未执行真实资源、跨机、长期压力、全仓测试或全部生成工程验证。

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

未来若采用“绑定时拒绝冲突前缀”的修复，正式回归应改为断言首次绑定就失败且未写计数器，并保留正常前缀的重启递增测试；不要求修复后继续允许这个保留前缀。
