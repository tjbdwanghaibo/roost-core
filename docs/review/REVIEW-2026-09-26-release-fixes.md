# 2026-09-26 新增复审问题核实与修复

**提交更新（2026-09-26）**：用户已授权提交，RR-03～24 的修复、回归与文档随本次 main 提交保存；未执行 push 或发布。下文“未提交”和索引刷新失败是修复验收时的记录。

基线 main `381efc9`（只增加审查文档），保留上一轮 RR-03～09 未提交代码。本轮处理 RR-20260926-10～24，15 项均确认有依据；RR-23 的 FatalSuffix 部分已由上一轮修复。本轮没有发布 tag、提交或 push。

## 最终行为

| RR | 修复 | 兼容边界 |
| --- | --- | --- |
| [10](../bugfix/RR-20260926-10.md) | 卸载重载等待实体投影 | 加载可能等待相关投影，须走慢阶段。独立构造的 Repository 若使用自定义 RecoveryGate，需提供 WaitEntityProjection；正式 Runtime 已接入。 |
| [11](../bugfix/RR-20260926-11.md) | 旧 Remote 回执重放不回退活实体 | 只作用于已验证持久回执的重放，不授权旧 writer。 |
| [12](../bugfix/RR-20260926-12.md) | 读取正式 syncbus 配置段 | 保留两种旧配置兼容；默认 topic prefix 不改变。 |
| [13](../bugfix/RR-20260926-13.md) | 释放失败仍完成已提交事务 | 收到释放错误不等于事务未提交，不能据此重复执行业务。 |
| [14](../bugfix/RR-20260926-14.md) | 提交后 hook 失败不能 Abort | 不把 hook 错误包装成提交失败。最终回复保留 callback/release 错误。 |
| [15](../bugfix/RR-20260926-15.md) | 重连不复用正在退出的传输队列 | 复用 SessionID 的调用者应在旧发送退出后重试，或为新连接分配新 ID；不在快池等旧发送。 |
| [16](../bugfix/RR-20260926-16.md) | checkpoint 不超过持久 WAL | 异步投影确认可能增加一次文件同步。测试验证调用顺序/持久边界，不冒称物理断电实验。 |
| [17](../bugfix/RR-20260926-17.md) | 关闭排空外部在途调用 | 关闭超时不表示资源已释放；调用方应等待并再次 Close。新增包只承担三个模块共用的生命周期基建。 |
| [18](../bugfix/RR-20260926-18.md) | OpenRuntime 关闭自己打开的 WAL | 零值 options 不再泄漏目录锁。 |
| [19](../bugfix/RR-20260926-19.md) | 租约跳过必须有确定结论 | 采用原问题允许的“准入时禁止组合”。生成 Entity 的 RollbackRemoteCommit 不保存完整跨实体前像，不能宣称支持投影阶段事后撤销内存事务。自定义 remoteStore 回放历史混合记录必须实现 RejectRemoteCommitsInTransaction，否则明确报错且不 ack。 |
| [20](../bugfix/RR-20260926-20.md) | 内存级 Remote 未知提交交给恢复 | 已明确的 fenced/version-conflict/rejected 仍走确定拒绝。 |
| [21](../bugfix/RR-20260926-21.md) | 生产 Backend 转发并发投影能力 | 历史直接注入 MongoCommitter 的性能数据保留，但不能再称为 kit Backend 装配容量证明；本轮短测见汇总。 |
| [22](../bugfix/RR-20260926-22.md) | 公会 ID 首次并发分配 | 创建失败仍允许空号，不能回收。未放宽持久权威校验；旧测试伪造 fence=0 的问题被修正。 |
| [23](../bugfix/RR-20260926-23.md) | 去除真实时钟分辨率假设 | 没有改变生产统计阈值或放松延迟 SLO；本机 darwin/arm64，未冒称 Windows 实机已通过。 |
| [24](../bugfix/RR-20260926-24.md) | 补齐 room 迁移与模块符号改名 | 旧 syncTopic 必须手动改为 syncNamespace；旧 demo 业务文件仍遵守再生成/人工合并契约。没有发布新 tag，未验证尚不存在的“无 replace 新发布组合”。 |

## 实际验证

Go 1.27.0，darwin/arm64；使用 `GOCACHE=/tmp/roost-nest-go-cache GOWORK=off`。集成环境为本机既有隔离 Mongo replica set、Redis、NATS，每次唯一数据库，结束清理。环境凭据不入库。

- `go test -race ./nest ./nestwal ./dataengine/engine ./remoteentity ./kit/syncbus ./sync/entitysync ./sync/nettransport ./internal/operation ./scripts/perf/nest-msg`：10 包通过。后续 Nest 回调与 Remote/lease admission 调整后，nest/dataengine/engine/remoteentity 三包 race 再通过。
- `go test -race ./nest ./nestwal -run 'Test(RemoteCloseFailure|CommitterCloseWaits|PostRemoteCommit)' -count=5`：通过，包含 periodic/on_change。
- `go test ./codegen/internal/roost`：通过；本轮迁移定向负对照也已执行。
- 受影响包 `go vet` 与 `go run ./cmd/glsvet`：通过。
- `bash scripts/test-dataengine-generated.sh`：三次独立进程通过；新增重载回归在 phase 1 的 async/strict/pipelined 全部通过。
- `bash scripts/test-remote-generated.sh`：三策略真实依赖 + 生产 Backend 装配断言通过。
- `bash scripts/test-sync-modes-generated.sh`：通过。
- `bash codegen/scripts/source-head-check.sh full`：完整 game 工程 build/vet/test/glsvet 通过。它不是 game-demo，不能替代下面的模板验证。
- 当前 CLI `project new ... -template game-demo -skip-deps`，临时 go.work 指向当前源码；完整 build/vet 与 `go test -race -count=1 -run '^TestGuildIDs' -v ./game/controllers/player` 通过。真实 Mongo 128 个首次并发分配无重复、无错误；WAL 三次重启通过。测试暴露旧 fixture 无权威 grant，已按正式许可流程补齐，未改生产 fence 条件。

负对照使用本轮改动前的 `381efc9 + RR-03～09` 副本，复制可兼容的新测试，仅测试文件变化。RR-11、12、13、14、15、16、17（WAL 外部回放）、18、20、24 共 10 个定向用例按预期失败；修后对应通过。RR-10 采用原复审的真实 Mongo 复现与本轮正式三策略回归，未冒称所有 15 项都有新负对照。

原始本机日志：`/tmp/roost-release-negative.log`、`roost-release-race.log`、`roost-release-final-race.log`、`roost-release-dataengine.log`、`roost-release-remote.log`、`roost-release-sync.log`、`roost-release-guild.log`。这些临时日志不是交接依赖，回归代码和运行入口均在仓库。

## 性能和未验证边界

RR-21 更正：历史 Remote 性能 fixture 直接注入 MongoCommitter，不能作为生产 kit Backend 装配容量证明。历史数据不删除。新 fixture 必须走 Backend，并明确断言并行能力。本轮短测结果在此追加；不把短测视为 30 分钟/24 小时长稳。

DataEngine engine、Nest、nettransport 测试已交叉编译为 windows/amd64 可执行文件；未在 Windows 实机重跑，不宣称 Windows CI 已绿。未做物理断电。没有创建发布版本，未验证未来无 replace 的发布组合；RR-24 包路径与限定符号修复已验证，不等于所有 v1.16.1 业务文件能无人工迁移升级。原报告“疑点（未登记 RR）”仍为独立待核实清单，其中 CommitSystem 关闭竞态随 RR-17 得到保护，其他未覆盖项不能自动标为已修复。

图谱 project `Users-whb-roost-roost-core`、generation `2026-09-25T11:41:37Z`，使用 Verify；覆盖检查显示 metadata_changed、生成 fixture excluded、guild_ids_test 模板 partial（157/163 行）。已读当前源码补证，没有做全仓完整性承诺。索引刷新需在压测结束后尝试；旧代际阻止刷新时不能删未知锁。


### 正式 Backend 装配短测

运行：加载本地隔离 env 后，`ROOST_REMOTE_LABEL=review-20260926-backend-100tps ROOST_REMOTE_DURATION=60s ROOST_REMOTE_RATE=100 bash scripts/perf/remote.sh`。GOMAXPROCS=4，strict，1000 会话/10000 Entity，每事务 2 Entity × 2 DAO；Fast 64/4096、Slow 64/64，Remote 写预算128，默认投影并发8。无 race/profile，测量期没有并行编译/索引。

| 项目 | 结果 |
| --- | --- |
| 测量/完成耗时 | 60s / 60.233845s |
| 成功完成 | 6000 |
| 错误 / 丢弃 | 0 / 0 |
| 成功完成 TPS | 99.6118 |
| p50 / p95 / p99 / max | 265 / 347 / 386 / 444 ms |
| 最终 WALUnacked / ProjectionFailures | 0 / 0 |
| 最终 ActiveTransactions / WritesInFlight | 0 / 0 |
| 最终数据/版本/回执验证 | 通过（result.json.verified） |

产物 `artifacts/perf/remote/review-20260926-backend-100tps/` 保存配置、源码摘要、JSON 和压缩日志。它验证本次装配在该负载下的短时行为，不是能力上限或长稳保证，也不能拿 Remote 持久事务延迟与 Sync 的 50ms 客户端可见指标混算。前序性能对照并非相同装配，因此不提供虚假的提升百分比。

最终索引刷新实际失败：`CBM index worker could not start: a pre-coordination or unverified CBM generation is active`。未改锁/未停止其他实例，旧图谱代际保持不变。两份本机 skill 与仓库规则源已同步，quick_validate 均通过。
