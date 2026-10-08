# DAO 自动迁移与 Remote 旧发布端清理：实现

基线 0166a45b；行为与升级约束见 [说明](RETIRE-DAO-MIGRATION-2026-10-08-NOTES.md)。这是维护者撤销能力后的重构，不把旧契约下的实现冒充新 bug，不编造红绿证据。

| 条目 | 强制点 | 回归 |
| --- | --- | --- |
| R1 | dataengine/engine/entity_repository.go：一致性读取一次，完整 schema 校验后才水合；codegen/internal/dao/template_dao.go：解码之前严格相等 | TestEntityRepositoryRejectsSchemaMismatchBeforePublishing：2 DAO × 旧/未来 schema，单次读、无发布、无文档变化；正式 game-demo 的 TestRestorePersistedRequiresCurrentSchema |
| R1 | 删除 MigrationRunner、DAORegistry、生成 Migrate；mongo_projection.go 不再按 handler 忽略版本冲突 | TestMongoStoreHandlerCannotSuppressProjectionConflict；原批量/Remote 窗口/投影失败回归保留 |
| R1 | 系统事务 ID helper 移至 entity_delete.go；ProjectionTicket/SystemCommitter/WaitProjection 保留于 projection.go | 原删除与系统投影测试；根示例实跑 |
| R2 | remoteentity/syncer.go 与 interest.go 双端拒绝旧发布端；删除 generation=0 撤销分支 | TestInterestWireRejectsLegacyAndPreservesOrdering：正式发布拒绝、绕过发布直传旧格式仍拒绝、空 Delete 拒绝、乱序撤销和当前撤销 |
| R3 | 维护者确认 Cube 仓库已废弃，不处理三处旧 API | 未改 Cube；不列验收或迁移待办 |

## 验证与复跑

实际命令均在干净 worktree 执行，macOS、Go1.27，GOWORK=off、GOMAXPROCS=2、-p 1；无 benchmark 或压测。

- `go build -p 1 ./...`、`go vet -p 1 ./...` 已通过。
- 定向 DataEngine 加载/拒绝/同步恢复与 Remote 兴趣契约已通过。
- 正式 `go run ./codegen/cmd/roost project new compatverify -module example.com/compatverify -out <temp>/game -template game-demo`，替换 roost-core 到本轮 worktree，再 build/vet/test：19 个有测试包通过；go generate 后所有 Go 文件摘要一致。
- 正式 dataengine/remoteflow fixture DAO/Entity 生成及编译通过，仅编译，没有真实资源运行；正式 syncruntime 生成后带 entitysyncruntime tag 实跑通过。
- Linux arm64 交叉构建通过；未做 Linux 运行验证。
- `go test -p 1 ./...`：131 个有测试包通过；`go test -p 1 -race ./dataengine/... ./remoteentity -count=1` 三包通过（1.748s / 6.471s / 30.349s）；根包 `go test -p 1 -count=1 .` 通过（4.245s，包含示例实跑）。根模块 `go generate ./...` 全部 up to date，无生成漂移。

原始日志保留 `artifacts/core-retire-20261008/`。第一次编译发现测试夹具曾删除过宽，修复后重跑；严格 schema 暴露 fenced resync 夹具缺声明/_schema，已补正式字段；生成清单曾引用已移除迁移模板，已同时更新。Remote 容量旧用例依赖 generation=0 撤销直接删除表槽，现按当前非零代际验证配额释放、水位保留与到期回收；未提高容量或削弱乱序保护。保留初次失败日志，不把它们当成基线产品缺陷。

CBM 基线 generation 2026-10-08T06:53:22Z；变更路径与范围已校验。金样、demo/db/migrations 与 docs 被索引排除，使用当前源码/差异及正式生成结果补证；新增文件在工作树直接读取。图谱名称冲突的发布方法调用关系以源文件核对，不作图谱穷尽声明。收尾后刷新索引。

生产 Go 与 Go 模板净减少 401 行（排除测试与金样，含 projection.go 移动后的内容）；没有为了旧接口留下转发适配器。

## 提交与索引

实现提交 `605ac914` 已快进合并 main。CBM 已对 main 重建，generation `2026-10-08T07:31:58Z`，25669 nodes / 246054 edges。54 个存续变更路径中 41 个覆盖无已知缺口且 metadata_match；13 个为按规则排除的文档/金样，已源码核对。索引全局仍有 5 个既有模板局部解析缺口（本轮未改），不宣称全图完整。原始覆盖报告见 `artifacts/core-retire-20261008/cbm-final.json`。

Core 本轮无未完成验收；Cube 按维护者最终决定不处理（仓库已废弃），本轮无剩余迁移待办。无 tag、部署或压测。
