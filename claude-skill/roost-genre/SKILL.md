---
name: roost-genre
description: 按一段业务需求用 roost-core（Go 游戏服务器框架）生成或扩展服务端工程：先问清客户端、人数、局外状态、战斗与参与方式，判断需求是否真的需要这套服务器框架（不需要时给 Steam Lobby 等替代方案），再拆需求、分服务器与客户端、复用业务模块、手写其余、离线验收、起服冒烟，并在工程里生成说明文档、客户端对接文档与给客户端的导出目录；支持在已有工程上持续增改需求，也能被别的 agent（例如客户端工程里的 agent）调用并回交固定格式的摘要。其次也能按玩法（SLG、MOBA、MMORPG、卡牌、塔防、放置等）出 demo 并说明框架边界。触发词：用 roost 做…、按需求生成游戏服务器、给 roost 工程加功能 / 改需求、客户端要对接 roost 服务端、某类玩法能不能用 roost、roost-genre。
---

# roost-genre（从 GitHub 取用）

本文件只是入口。skill 的正文、模块说明与框架文档都随 roost-core 的版本发布在 GitHub（`github.com/tjbdwanghaibo/roost-core`，公开仓库）上，按下面的步骤取到**指定版本**后照正文做。不需要本机有 roost-core 的检出。

入口版本：2026-11-18.1

适用的 roost-core：v1.56.0 及以后。更早的版本缺少正文用到的登录捷径与模块，告诉用户需要升级。正文的内容以取到的那个版本为准；正文的结构与本入口描述的不一致时（例如没有 compose 开头的步骤总览），照正文做，本入口只负责定版本、取正文、装 CLI。

前置：Go 1.27 及以上，git、curl、python3，能访问 GitHub 与 Go 模块代理；起服冒烟要 docker。全程 `GOWORK=off`，命令用 bash 执行。

## 0. 开始前（一次对话做一次）

先做这三件事，结果用一两句话告诉用户，并写进这一轮的 `docs/DEMO-HISTORY.zh-CN.md`。查不了（没有网络、GitHub 不通）就说“未能检查更新”，用现有版本继续，不要因此停下。

**① 定版本 `$V`**：

- 当前目录已经是 roost 工程（有 `roost.yaml`）：用它 `go.mod` 里 `github.com/tjbdwanghaibo/roost-core` 的版本，不擅自升级。
- 用户指定了版本：用指定的。
- 否则用最新发布版：

```bash
export GOWORK=off
LATEST=$(go list -m -json github.com/tjbdwanghaibo/roost-core@latest | python3 -c 'import json,sys;print(json.load(sys.stdin)["Version"])')
V="$LATEST"      # 新工程；已有工程 V 取 go.mod 里的版本
```

**② 入口文件有没有新版**：不落盘，用管道直接比较（`<本文件>` 换成本文件在工程里的路径，通常是 `.claude/skills/roost-genre/SKILL.md` 或 `.agents/skills/roost-genre/SKILL.md`）：

```bash
new="$(curl -fsSL https://raw.githubusercontent.com/tjbdwanghaibo/roost-core/main/claude-skill/roost-genre/SKILL.md)" || echo "未能检查更新"
[ -n "$new" ] && { printf '%s\n' "$new" | diff --strip-trailing-cr <本文件> - && echo "入口文件已是最新"; }
```

不同时告诉用户：入口有新版、两边的“入口版本”各是多少、主要改了什么（只读新版，概括差异）。**不要自己改本文件**：项目文档写了更新命令的照它给；没写的，更新办法是 `curl -fsSL https://raw.githubusercontent.com/tjbdwanghaibo/roost-core/main/claude-skill/roost-genre/SKILL.md -o <本文件>`。用户明确同意后可以执行这条命令，但不手改内容；用户没表态之前按本文件继续。不要把新版下载到工作目录之外，也不要用固定的临时文件名。

**③ roost-core 有没有新的发布版**：只比版本号，不下载。已有工程的 `$V` 比 `$LATEST` 旧时，告诉用户两个版本号，问要不要升级；**用户同意之前不升级**，继续用工程现在的版本。新工程直接用最新版，不用问。

用户要升级时再做：`go mod download -json "github.com/tjbdwanghaibo/roost-core@$LATEST"` 取到新版，读它的 `docs/release/<新版本>-NOTES.md`（没有就看 `CHANGELOG.md` 对应的一节），用两三句话说明与本工程有关的变化；然后 `roost project upgrade --root . --dry-run -core <新版本>` 看会改哪些文件，告诉用户后再执行不带 `--dry-run` 的那条，CLI 也换成新版本（第 2 步）。已装的业务模块不随升级自动更新，发布说明里提到的模块改动按模块的 README 手工对照。升级后重跑全部验收（正文 compose 第 10、11 步与以前每轮的场景），记进 HISTORY。

## 1. 取 skill 正文（只读）

```bash
CORE=$(go mod download -json "github.com/tjbdwanghaibo/roost-core@$V" | python3 -c 'import json,sys;print(json.load(sys.stdin)["Dir"])')
```

`$CORE` 是 Go 模块缓存里这个版本的完整源码，文件是只读的。然后读：

- `$CORE/docs/agent-skills/roost-genre/SKILL.md`：正文入口，§0 是路由；
- 按需求生成、加功能或改需求：`$CORE/docs/agent-skills/roost-genre/references/compose.md`，从开头的步骤总览照做；
- 文中的 Markdown 链接一律按 `$CORE` 里的文件解析（模块说明在 `$CORE/modules/<name>/README.md`，能力清单与已知限制在 `$CORE/docs/`）。

正文的命令默认按发布版写，与本入口一致；正文里“维护者用本地检出时”那一节与你无关。

## 2. 装 CLI

```bash
WORK=<放 CLI 与工程的目录>
GOBIN="$WORK/bin" go install "github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@$V"
ROOST="$WORK/bin/roost"
```

## 3. 被别的 agent 调用时

调用方（例如客户端工程里的 agent）应给：需求原文、客户端引擎与工程路径、服务端工程目录、模式（业余 / 专业 / 只给决策）、是否允许提问、roost-core 版本。缺的按默认补，**不假设能交互**：要用户决定的事放进交付摘要的“待决定”栏，不卡住。输入默认值、产出清单、交付摘要的固定格式与客户端 agent 的第一步，见正文 `SKILL.md` 的“§1 与其他 agent 对接”。

## 4. 规则

- **skill 只读**：不修改本文件，不修改 `$CORE` 下的任何文件（模块缓存本身也是只读的，不要改权限去写）。发现正文写错、写漏或照做走不通，告诉用户：哪份文档哪一节、原文、实际情况、你怎么绕过的，建议用户到 roost-core 仓库提 issue；绕过的办法只记进工程的 `docs/DEMO-HISTORY.zh-CN.md`。
- **记忆在工程里**：`docs/DEMO.zh-CN.md`（当前状态）、`docs/DEMO-HISTORY.zh-CN.md`（逐次变更）、`docs/CLIENT.zh-CN.md`（客户端对接）。后续增改需求先读它们（正文 compose 第 14 节）。
- **版本一致**：同一个工程的 CLI、正文与依赖必须是同一个 `$V`。要升级 roost-core 时按第 0 步③先问用户，升级后重跑全部验收并记进历史。
