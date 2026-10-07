## 共同要求（实施类 agent）
- **授权**：维护者 2026-10-05 已决定方向并授权实施，详见 `docs/review/DECISIONS-PENDING-2026-10-05.md` 末尾的“维护者决定（第二轮）”。按先红后绿做，留 bug / bugfix / feature 文档，提交并推送。**不发版**（等全部决定项完成后统一发 v1.20.2），**不等 GitHub CI**。
- **额度**：维护者要求节省额度。不做冗余的压测和审查，验证按影响面来。如果遇到额度或用量上限，先把未完成的部分写进对应文档（方案文档的“实施状态 / 未完成”一节，以及 DECISIONS-PENDING 的实施状态列），提交推送，然后停止。
- **先读**：`AGENTS.md`、`docs/agent-skills/roost-coding/SKILL.md`（执行契约、快池不阻塞、三步停机、反复出问题要上报方向判断）、`docs/agent-skills/roost-coding/references/fix-contract-review.md`、`docs/agent-skills/roost-bugfix/SKILL.md`。纯重构或框架改动，先在 `docs/feature/` 写简短方案，写清目标、改动面、兼容、验证，然后直接实施，不需要再请示。
- **工作目录隔离（必须）**：先 `git -C /Users/whb/roost/roost-core fetch origin`，再 `git -C /Users/whb/roost/roost-core worktree add -b <分支> /private/tmp/claude-502/-Users-whb-roost/4f139f43-13e1-4529-b481-b39566685060/scratchpad/<worktree> origin/main`，全部工作在 worktree 里做。完成后 `git worktree remove`，并删掉分支。不碰主 checkout 及其 `artifacts/`。
- **并行方**：同时还有别的实施 agent。不要改别人负责的文件（每个 agent 的 prompt 里写了范围）。每次 push 前 `git pull --rebase origin main`；共享索引（两个 README、交接 §7、CHANGELOG、TROUBLESHOOTING、DECISIONS-PENDING）冲突时手工合并，双方都保留；T 编号撞了就顺延。push 被权限规则拦下时，改推同名分支 `origin/<分支>` 并在报告里写明；收尾清理被拦下时停下来报告。两种情况都不要绕过。
- **读代码**：优先用 codebase-memory-mcp（项目 `Users-whb-roost-roost-core`，共享 generation 停在 09-30，以当前源码为准并写明）。
- **真实依赖**：用隔离环境 `~/.roost-it/roost-dataengine-it`。`source ~/.roost-it/roost-dataengine-it/env.sh` 含凭据，只 source、不打印。跑 integration 一律加 `-run` 只跑自己的用例；故障注入自建代理或进程，不 reset 共享 toxiproxy。资源用自己的前缀，用完删除。生成工程的 DAO 库名是编译期常量，要改成唯一名字。`remote-acceptance.lock` 存在时，不跑真实依赖用例。
- **验证**（`GOWORK=off`）：`gofmt -l` 为空；改动包 `go vet`，`go test -race -count=3`；改了 nest / entity / dataengine / sync 的，加跑 `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`；根包 `go test -count=1 .`；`go build ./... && go vet ./...`；改了生成形状的，跑 `go test -count=1 ./codegen/...`，`go generate ./...` 后检查 porcelain，再生成 game-demo（replace 到 worktree）跑 build / vet / test。
- **提交**：中文，`git add` 显式列出路径，末尾 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`。完成后在 DECISIONS-PENDING 末表把对应行的实施状态改为“已实施（提交号）”。
- **报告**：方案要点、实施内容、红绿文本、验证结果、提交号、兼容影响、未完成项。

## 验证底线（2026-10-07 补）

改了 nest 提交语义、驱动契约、配置 schema 等跨包行为时，push 前必须跑一次全量 `GOWORK=off go test ./...`（不只目标包）。RR-20261006-41 只跑了目标包，导致 main 上 `skill/combatcomponent` 红了一段时间。
