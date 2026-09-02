# Roost 维护与收敛清单

Roost 当前进入稳定化阶段。后续主线只接受三类工作：已复现 bug 修复、可靠性/工程性验证、删除或迁出非核心能力。未经明确产品决策，不再向 core/kit 增加新玩法 feature、控制面或基础设施抽象。

## 当前状态

- **研发基线已闭环**：四仓用未提交的 `go.work` 做 source-head 联调。持久 Entity 删除已经进入唯一 Data Engine admission；Remote 删除使用显式 delete intent 并复用 ownership marker、lock fence、route epoch。Saga reservation token 已作为控制 receipt 进入同一 Mongo 事务，只有 owner、token、`pending` 状态和未过期租约全部匹配才应用业务 mutation。
- **正式发布仍有一个门禁**：按 core → kit → skill → codegen 发布正式 tag，并让生成工程在 `GOWORK=off` 下只依赖这些 tag。研发 workspace 通过不能代替 pure-tag 闭包。

## P0：发布前必须关闭

1. **跨仓版本闭包**：生成工程必须只依赖正式 tag；release gate 同时验证 standalone/pure-tag 与 source-head，不允许 pseudo-version、local replace 或不存在的 Mod。具体流程见 [多仓研发与发布](DEVELOPMENT_WORKSPACE.md)。

## P1：正确性收敛

以下项目已完成，继续作为不可回退的回归门禁：

1. 聚合加载把全缺失/全 tombstone 识别为 NotFound，把 live/missing/tombstone 混合态识别为 Corrupt，并禁止发布半聚合 Entity。
2. 新 Data Engine 默认 WAL writer v2；v1 仅作为显式 reader-first 兼容开关，不得承载 Patch/Receipt。
3. Mongo 绝对过期字段使用 `expireAfterSeconds=0`，相对 TTL 字段使用正数，索引变更通过显式迁移策略完成。
4. 持久删除失败、延迟 admission、indeterminate、Remote 明确 delete intent 和 rollback 都有测试。
5. Saga 旧 worker 的 owner/token/status/expiry 围栏与 stale transaction no-op marker 有测试。
6. 可靠命令消费者的 ACK/NAK、permanent error、drain 与 redelivery 语义由故障测试覆盖；`ISyncBus` 保持明确“不因 handler error 重试”的版本化状态同步语义。

## 工程性门禁

- 每个修复必须包含最小回归测试；涉及并发、锁、WAL、Remote、Saga 时补 `-race` 或真实依赖故障验证。
- 每次发布运行 `go test ./...`、`go vet ./...`、source-head 四仓组合测试、生成工程 smoke test 与依赖闭包检查。
- 文档、Grafana、CI 和 Codegen catalog 必须与当前唯一 Data Engine 架构一致；删除代码时同提交删除失效配置、指标、脚本与文档。
- 性能工作只允许建立或收紧现有关键路径基线，不以性能名义引入第二套机制。

## 精简原则

- `checkpoint` Mod 与 standalone `nestwal` Mod 已删除；`nestwal` 只保留为 Data Engine 内部 WAL 库。
- 没有仓内引用不等于可直接删除。公开包先标记迁出/废弃并验证下游引用，公共 API 的物理删除放在主版本边界。
- 游戏类型相关扩展优先迁出 core/kit；Entity、Nest、Data Engine、Remote Entity、Saga、同步契约和必要基础设施适配保留在框架主线。
- 不维护平行实现、兼容空壳或只为未来设想存在的目录。
