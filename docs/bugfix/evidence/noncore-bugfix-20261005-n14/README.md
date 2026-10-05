# N14 修复验证（revn14）

修复前红：见审查证据 [../../../review/evidence/noncore-review-20261005-n14/](../../../review/evidence/noncore-review-20261005-n14/README.md)。以下全部 `GOWORK=off`，macOS，在 revn14 修复后的工作树上执行。

| 命令 | 结果 |
| --- | --- |
| `gofmt -l app kit saga` | 空 |
| `go build ./... && go vet ./...` | 通过 |
| `go test -count=1 ./app/... ./kit/... ./saga/...` | 全部 ok |
| `go test -race -count=3 ./app ./kit/mods ./kit/redis ./kit/remoteentity ./kit/saga ./kit/mongo ./saga` | 全部 ok（app 35s） |
| `go test -count=1 .`（根包门禁） | ok |
| `go test -count=1 ./codegen/...` | 全部 ok |
| `roost project new n14demo -template game-demo` → `go mod edit -replace` 到本 worktree → `go build ./... && go vet ./... && go test -count=1 ./...` | 通过（18 个有测试的包 ok） |
| 临时探针：生成工程 20 份服务配置逐份 `ValidateServiceConfig` | 全部通过（探针未提交） |

未运行：integration 与真实依赖（开工时 `remote-acceptance.lock` 存在；收尾时已释放，但本轮修复不涉及真实依赖语义，未补跑）。
