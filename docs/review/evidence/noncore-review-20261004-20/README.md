# N04 正式生成迁移写回与重载证据

2026-10-04；source `341379254a5f74bcf445c473fde161b1b5e4cf19`。正式DAO CLI由当前仓库编译，生成[定义](consumer-definition.go.txt)的MigratingDao；[消费者](consumer-test.go.txt)通过公开Repository、Runner、实际文件WAL/Projector及正式MongoStore验证。Mongo客户端是框架mongotest，不是真实服务；没有真实Mongo/HA/网络强杀或持久跨进程重启验收。

复跑：[Run-Consumer.ps1](Run-Consumer.ps1)传入当前GoExecutable、可选RepoRoot与全新OutputRoot（必须不存在）。使用`GOWORK=off`，生成模块replace到该RepoRoot，再`go test -mod=mod -race -count=1 -timeout=60s -json ./db`及vet。生成物、exe、module/cache在scratch，不写入产品源码。预期当前实现test_exit=1、vet_exit=0；编译/panic/race/整包timeout不是产品红。

最终[consumer.jsonl](consumer.jsonl)、[exits.json](exits.json)为11叶子：3fail/8pass/0skip。非法输出3项断言CommitSystem未被调用，实际均1次；语法/ID错误是已准入WAL、未投影且不断重试，类型错误则真实MongoStore替身后端已存schema3/version8，新Manager也无法加载。正常/current/newer/step-failure、直接hydrate3对照与取消后晚投影重载共8项通过。所有Projector/WAL均有3秒有界Cleanup，无cleanup错误；没有留下外部实例。

既有定向：`go test -race -count=1 -timeout=90s -json -run '^(TestEntityRepository.*|TestRepository.*|TestMigrationRunner.*|TestWaitProjection.*|TestDAORegistry.*|TestRegistry.*|TestRegistries.*)$' ./dataengine/engine ./dataengine ./migration`，34叶子pass/0fail/0skip，[事件](existing.jsonl)。相关vet及全仓`go build ./...`通过；根包`go test -count=1 -timeout=90s -json .`14叶子通过，[事件](root.jsonl)、[退出](checks.json)、[计数](summary.json)。不把这两个绿集合与新反例相抵消，不查询GitHub CI。

图谱[coverage](coverage.json)为Tier2/roost-core，generation09-30，材料metadata_changed，用[源码摘要和具名阅读范围](source-hashes.csv)补证。初猜不存在路径保留在coverage快照但不引用为源码证据；golden只是定位结果。本轮的中文注释不改变执行代码；Go文件diff只含这些注释。

初版消费者的不存在Stats字段导致build-fail，后续门闩嵌入MongoStore暴露ProjectFenced导致错误超时，均修正后执行此归档版；初版scratch保留，不计入3个行为反例。其他公开编译通过不代表后端替身的所有真实Mongo语义成立。

[问题/实施方向](../../../bug/RR-20261004-NC-31.md) · [本轮记录](../../REVIEW-2026-10-04-noncore-20.md) · [学习](../../IMPLEMENTATION-DAO-MIGRATION-HYDRATION-AND-REFHMAP-SCHEMA.md) · [进度](../../PROGRESS.md)。

最终整合到64529e39，DAO模板新增nocoll分支后重新生成本轮有集合消费者，仍11叶子3fail/8pass、vet0：synced-consumer.jsonl / synced-exits.json。synced-checks.json与synced-root.jsonl保存最终全仓build0/根包0。原source-hashes/34定向证据仍是341基线，不覆盖新nocoll完整实现或playerowner全部修复。
