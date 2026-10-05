# N11 revn11 证据（NC-140～147）

基线 `23f82dbc`，macOS / Go 1.27.0，`GOWORK=off`。修前红在基线源码（与基线模板生成的工程）上执行，修后绿在本分支执行。

| 文件 | 内容 |
| --- | --- |
| [red-nc140-nc142-world.txt](red-nc140-nc142-world.txt) | 基线模板生成的 game-demo（replace 到基线 core），World 回滚四叶、过期截止时间两叶失败 |
| [red-nc141-nc147-timer.txt](red-nc141-nc147-timer.txt) | `go test ./timer`：NC-141 三个用例、NC-147 一个用例失败；邻近分支通过 |
| [red-nc143-spatial.txt](red-nc143-spatial.txt) | `go test ./spatial`：上界两叶失败，下界对照通过 |
| [red-nc144-146-index.txt](red-nc144-146-index.txt) | `go test ./index -run` 逐个：NaN panic、混合类型键 panic、零值丢写 |
| [green-core.txt](green-core.txt) | `go test -race -count=3 -v ./timer ./spatial ./index ./clock` 结果行 |
| [green-world.txt](green-world.txt) | 本分支模板全新生成的 game-demo，`go test -race -count=3 -v ./game/entities/world/` 结果行 |

其余执行（未存原文）：生成工程 `go build ./... && go vet ./...`、`go test ./game/... ./internal/service/game/...` 通过；core `go build ./... && go vet ./...`、根包 `go test -count=1 .`、`go test -count=1 ./codegen/...`、`go test -race ./sync/entitysync/policy ./fctx ./app` 通过。

复跑：`GOWORK=off go run ./codegen/cmd/roost project new n11demo -module example.com/n11demo -out <scratch>/n11demo -template game-demo`，`go mod edit -replace github.com/tjbdwanghaibo/roost-core=<本仓库>` 后 `go mod tidy`，再跑上面的命令。
