# NC-31 修前红 / 修后绿

基线 3127d37ce113b887038366f9289218c32729dbbc；[修复记录](../../RR-20261004-NC-31.md)。开始 10-04 / 完成 10-05；source_head 字段是基线，绿测包含未提交修复，实际修改身份由[source-hashes.csv](source-hashes.csv)和包含本目录的提交确定。

## 修前

[red.jsonl](red.jsonl) / [red-exit](red-exit.json)：用 Git 基线 migration_runner.go overlay 到当前源码，跑正式新回归中的验证/缺 loader 两组，5 fail / 3 pass。四个坏输出和缺契约均已调用 CommitSystem；红不是编译失败、race或包timeout。当前其余正式回归测试文件保留，overlay 只替换实现。

[consumer-red](consumer-red.jsonl) / [退出](consumer-red-exits.json)：上轮正式 DAO CLI 原消费者在当前基线重新生成，3 fail / 8 pass，vet0。使用真实文件 WAL/Projector/正式 MongoStore，后端 mongotest。两个1秒预算只是终止坏投影等待；失败断言是准入已发生 / 原记录已变化。

## 修后与最终验证

[consumer-green](consumer-green.jsonl) / [退出](consumer-green-exits.json)：原11项全部通过。坏输出不调用 CommitSystem，原 schema/version 保留；正常、较新 schema、步骤失败、取消后晚投影/新Manager重载均保持。

[final-packages](final-packages.jsonl)：`GOWORK=off go test -race -count=1 -timeout=180s -json ./dataengine/engine ./dataengine ./migration ./nestwal ./codegen/internal/dao`，421 叶子pass、0fail、1 helper skip。NC-31新增正式12叶子全绿。skip为 `TestNestWALCrashChildProcess`，正常父用例执行其子进程分支；不把 skip 计作独立通过。

[final-root](final-root.jsonl)：`GOWORK=off go test -count=1 -timeout=90s -json .`，14叶子通过。[退出检查](final-checks.json)、[场景汇总](final-summary.json)：全仓build、相关vet、glsvet、包/根测试全部0。

```powershell
$env:GOWORK='off'
go build ./...
go vet ./dataengine/engine ./dataengine ./migration ./nestwal ./kit/dataengine ./codegen/internal/dao
go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync
go test -race -count=1 -timeout=180s ./dataengine/engine ./dataengine ./migration ./nestwal ./codegen/internal/dao
go test -count=1 .
```

[Kit integration编译](kit-integration-compile-exit.json)：`go test -tags integration -run '^$' ./kit/dataengine`，仅编译适配后的手写候选，不执行真实资源验收。

新增六项 review 的正式生成/17叶子事件、脚本和子进程测试见[第21轮证据](../../../review/evidence/noncore-review-20261004-21/README.md)。本轮没有改生成形状/WAL格式，未运行 Bash sync-mode/外部三资源/故障矩阵脚本，正式 CLI 生成与消费、现有 DAO golden 测试已运行；它们不替代上述外部矩阵。没有性能benchmark，不等待GitHub CI。

[初始coverage](coverage.json)与[完整最终coverage](../../../review/evidence/noncore-review-20261004-21/coverage.json)均为陈旧图谱+当前源文补证，不称索引已更新。原始失败历史保留，夹具更正见第21轮README；不使用初次int32负断言/crash水位错误作为缺陷证据。
