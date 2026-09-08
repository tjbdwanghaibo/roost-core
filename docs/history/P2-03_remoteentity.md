# P2-③：remoteentity 并入 core（M-02 试点）

批次：P2-③（收敛方案 §4；统一方案 M-02）
状态：完成（core 侧）；kit 侧旧路径保留到 P3
目标与非目标：Remote Entity 的 wrapper / 管理器 / 批次 / 所有权与标记 / 版本锁 / 事务与 Mongo 提交器 / L2 快照 / 兴趣注册 / 同步器带历史并入 `core/remoteentity`；`remote_entity_mod.go` 留 kit；不改行为、不改持久化与 Lua
负责人、工作树：本机，`roost-core` 分支 `consolidation`
前置批次：P2-①（mongotest）、P2-②（core/mongo、core/redis 客户端）

## 文件清单

- 搬入：kit/remoteentity 全部实现与测试（`merge_kit_pkg.py remoteentity --exclude remote_entity_mod.go`），core 原无同名包，无文件名冲突。
- 改动：`mongo_committer_test.go` 的 `roost-kit/mongo/mongotest` → `roost-core/mongo/mongotest`。
- **留在 kit 的测试**（依赖 Mod 胶水，随 Mod 归 kit）：
  - `promises_test.go` 里的 `TestRemoteEntityModRequiresANonZeroSid`（U-0025 的一部分，`NewRemoteEntityMod(0).Init` 拒绝 sid 0）——core 拷贝中删去该函数，其余两条承诺测试保留；
  - `mongo_committer_integration_test.go`（真实 Mongo，通过 `kitmongo.NewMongoMod` 建客户端）——整文件留 kit。P3 时若要在 core 侧跑真实 Mongo 提交器集成，改用 `core/mongo.NewClient` 直接建客户端重写一份。
- `backend.go` 注释提到 RemoteEntityMod，保留（它描述的边界仍成立：Backend 是 Mod 装配 Core 实现时唯一的业务特定输入）。

## 公共 API 变化

导入路径 `roost-kit/remoteentity` → `roost-core/remoteentity`；符号不变。`NewRemoteEntityMod` / `RemoteEntityMod` / `ModOption` / `WithBackend` / `WithMongoStorage` 仍在 kit（映射表 `kit_keeps`）。

## 不变量、持久化 / wire 格式

未触碰 Redis 标记 Lua、版本锁 Lua、Mongo 提交文档结构、L2 快照编码。

## 关联历史 U / B / T

U-0011、U-0023、U-0031、U-0049、U-0093 随文件搬入并通过；U-0025 的 Mod 部分留 kit。统一方案 M-02 列出的新增场景（同 ID 并发创建与取消、引用释放与驱逐、容量满、所有权转移、续租失败、结果不确定、停止时等待者退出、快照乱序、首次 / 重试判据一致）**未在本批做**——本批只移动；这些作为后续 U 单元在新路径上补（进账本第 4 节）。

## 命令与结果

`go build ./...` 0；`go vet ./remoteentity/` 0；`go vet -tags integration ./remoteentity/` 0；`go test -count=1 ./remoteentity/ .` ok（含边界测试）。go.mod 仅依赖整理（3 行）。

## 未验证项 / 风险

- 真实 Mongo 提交器集成在 core 侧暂无测试（见上）。
- Nest 访问 → 提交 → 快照同步 → 再读的端到端仍在 kit 的 Mod 级测试与 `framework-compat` 中，P3 / P4 门禁验证。

## 回退范围

`git revert` 本批两个提交。

## 下一步唯一动作

P2-④：dataengine。
