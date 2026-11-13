---
name: roost-genre
description: 按一段业务需求用 roost-core（Go 游戏服务器框架）生成或扩展服务端工程：先判断需求是否真的需要这套服务器框架（不需要时给 Steam Lobby 等替代方案），再拆需求、分服务器与客户端、复用业务模块、手写其余、离线验收，并在工程里生成 DEMO 说明文档；支持在已有工程上持续增改需求；也能按玩法（SLG、MOBA、MMORPG、卡牌、塔防、放置等）出 demo 并说明框架边界。触发词：用 roost 做…、按需求生成游戏服务器、给 roost 工程加功能 / 改需求、某类玩法能不能用 roost、roost-genre。
---

# roost-genre（从 GitHub 取用）

本文件只是入口。skill 的正文、模块说明与框架文档都随 roost-core 的版本发布在 GitHub（`github.com/tjbdwanghaibo/roost-core`，公开仓库）上，按下面的步骤取到**指定版本**后照正文做。不需要本机有 roost-core 的检出。

前置：Go 1.27 及以上，能访问 GitHub 与 Go 模块代理。全程 `GOWORK=off`。命令用 bash 执行。

## 1. 定版本

- 当前目录已经是 roost 工程（有 `roost.yaml`）：用它 `go.mod` 里 `github.com/tjbdwanghaibo/roost-core` 的版本，不要擅自升级。
- 用户指定了版本：用指定的。
- 否则用最新发布版：

```bash
export GOWORK=off
V=$(go list -m -json github.com/tjbdwanghaibo/roost-core@latest | python3 -c 'import json,sys;print(json.load(sys.stdin)["Version"])')
```

把用的版本告诉用户，并写进工程的 `docs/DEMO-HISTORY.zh-CN.md`。

## 2. 取 skill 正文（只读）

```bash
CORE=$(go mod download -json "github.com/tjbdwanghaibo/roost-core@$V" | python3 -c 'import json,sys;print(json.load(sys.stdin)["Dir"])')
```

`$CORE` 是 Go 模块缓存里这个版本的完整源码，文件本身是只读的。然后读：

- `$CORE/docs/agent-skills/roost-genre/SKILL.md`：总入口；
- 按需求定制、给工程加功能或改需求：`$CORE/docs/agent-skills/roost-genre/references/compose.md`；
- 文中的 Markdown 链接一律按 `$CORE` 里的文件解析（模块说明在 `$CORE/modules/<name>/README.md`，能力清单与已知限制在 `$CORE/docs/`）。

## 3. 装 CLI

```bash
WORK=<放 CLI 与工程的目录>
GOBIN="$WORK/bin" go install "github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@$V"
ROOST="$WORK/bin/roost"
```

## 4. 与正文的差异：用发布版，不用本地检出

正文的命令按“本地检出”写（`(cd "$CORE" && go build …)`、`project new … --skip-deps`、`go mod edit -replace …`）。从 GitHub 取用时这样换：

| 正文写的 | 换成 |
| --- | --- |
| `(cd "$CORE" && go build -o "$WORK/roost" ./codegen/cmd/roost)` | 第 3 步的 `go install …@$V` |
| `"$ROOST" project new … --skip-deps` | `"$ROOST" project new … -roost-core-version "$V"`（不加 `--skip-deps`，由它解析发布版依赖） |
| `go mod edit -replace github.com/tjbdwanghaibo/roost-core=$CORE` | **不做**；工程直接依赖发布版 `$V` |
| 按玩法出 demo 时拷 `$CORE/scripts/testdata/…` 夹具 | 照做，从 `$CORE` 拷（只读取，不改 `$CORE`） |

其余步骤（`module add`、`module verify`、`go test`、生成 `docs/DEMO.zh-CN.md` 与 `docs/DEMO-HISTORY.zh-CN.md`）照正文。

## 5. 规则

- **skill 只读**：不修改本文件，不修改 `$CORE` 下的任何文件（模块缓存本身也是只读的，不要改权限去写）。发现正文写错、写漏或照做走不通，告诉用户：哪份文档哪一节、原文、实际情况、你怎么绕过的，建议用户到 roost-core 仓库提 issue；绕过的办法只记进工程的 `docs/DEMO-HISTORY.zh-CN.md`。
- **记忆在工程里**：`docs/DEMO.zh-CN.md`（当前状态）与 `docs/DEMO-HISTORY.zh-CN.md`（逐次变更）。后续增改需求先读它们（正文 compose §2A）。
- **版本一致**：同一个工程的 CLI、正文与依赖必须是同一个 `$V`。要升级 roost-core 时先问用户，升级后重跑全部验收并记进历史。
