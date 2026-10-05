# N07 第二批证据（2026-10-05，revn07b）

基线 `3d4fe9f3`。路径里的 scratch 目录替换成 `<scratch>`，日志去掉了 `server_time_ms`。[本轮](../../../review/REVIEW-2026-10-05-noncore-n07b.md)。

| 文件 | 内容 |
| --- | --- |
| `e2e-reload.txt` | 真实进程（隔离 Mongo / NATS / Redis + 私有 etcd，global + game）经 ops admin 的 H0～H4：CSV-only（NC-64 红）、成功、四种失败、缺 required、探针 rollback、CSV + generate |
| `e2e-green.txt` | 本分支全新生成工程：修正后的说明与照做结果（NC-64 绿） |
| `nc65-red.txt` / `nc65-green.txt` | 生成工程 `TestFinalIsRecomputedFromWornGearWhenThePlayerLoads` 修前 / 修后 |
| `snapshot-probe.txt` | scratch 探针：handler 两次读之间 reload / rollback / 调用方自带上下文 |
| `snapshot-mutation-control.txt` | 把生成的 `ItemByID` 改读 `Current()` 后，新增控制用例变红（判别力） |
| `gen2-race.txt` | 全新生成工程 player / handler / gameplay / internal/service/game `-race -count=3` |
| `n08-generate-devdir.txt` | 移交 N08：后台写 `.dev/game.log` 时 `roost generate` 拒绝执行 |

复跑入口：`GOWORK=off go run ./codegen/cmd/roost project new X -module example.com/X -out <scratch>/X -template game-demo`，`go mod edit -replace github.com/tjbdwanghaibo/roost-core=<worktree>`，`db/def` 的 `db=game` 改成唯一名字后 `roost generate`；配置按 `demo/README.md`“本地实跑”的 sed 改到隔离环境，另把 `nats.prefix`、`syncbus.prefix`、`dataengine.effects`、`saga` 的 subject / stream / durable、`remote_entity.snapshot_l2_key_prefix`、ops / TCP / advertise 端口、etcd 端点换成本 agent 独有的值；先起 `global` 再起 `game`（`bin/app <svc> --sid 1000 --config ...`）。日志不要写在工程目录里（`roost generate` 会把它当输入）。

本地矩阵（本分支）：见下方“验证结果”。

## 验证结果（本分支，提交前）

- `gofmt -l`（改动的 Go 文件）空；`go vet ./codegen/internal/roost/` 通过；`go build ./... && go vet ./...` 通过；根包 `go test -count=1 .` 通过。
- `go test -race -count=3 ./codegen/internal/roost/`（355s）、`./configdata/ ./kit/configdata/ ./attribute/` 通过。
- `go test -count=1 ./codegen/...`：第一次跑 `codegen/internal/roost` 在 7.8s 报 `signal: terminated`（同一轮里其他包通过；该包单独跑与 race×3 都通过）；整组重跑 15 个包全部 ok。第一次的终止原因没有定位到，不计为绿，只记录。
- 全新生成工程 `n07bgd2`（replace 到本分支）：tidy、build、vet、`go test ./...` 通过；4 包 `-race -count=3` 见 `gen2-race.txt`。
- 未跑：`glsvet`（没改三大模块）、Linux / Windows、五进程全链路、真实客户端复制帧。

## 合并后（rebase 到 origin/main `855c2a38` 之后）

共享索引手工合并、保留双方（T 编号顺延为 T-231，接力清单 N07 / N08 取本批、N09 取上游）。`go build ./... && go vet ./...`、根包、`go test -count=1 ./codegen/internal/roost/` 通过；再全新生成一次 game-demo（replace 到合并后的分支）build / vet 通过，`game/entities/player`、`game/handler` `-race -count=3` 通过。
