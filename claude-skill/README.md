# 在自己的项目里使用 roost 的 Claude Code skill

`roost-genre/SKILL.md` 是一个入口 skill：放进任何项目，Claude Code 就会在“按需求做游戏服务器 / 给 roost 工程加功能”这类任务时用它。它不带正文，运行时从 GitHub 取 roost-core 的**指定发布版**（Go 模块缓存，只读），读其中的 `docs/agent-skills/roost-genre/`，并用同一版本的 CLI 生成工程。不需要本机有 roost-core 检出。

## 安装

在你的项目根目录执行：

```bash
mkdir -p .claude/skills/roost-genre
curl -fsSL https://raw.githubusercontent.com/tjbdwanghaibo/roost-core/main/claude-skill/roost-genre/SKILL.md -o .claude/skills/roost-genre/SKILL.md
```

想对所有项目生效，就放到 `~/.claude/skills/roost-genre/SKILL.md`。

**更新**：入口文件每次对话开始时检查两样——自己有没有新版（与 GitHub 上 main 的这份比较）、roost-core 有没有新的发布版（只比版本号，你要升级时才下载新版读发布说明）——有新版就告诉你并给出更新办法，不会自己改文件，也不会未经同意升级你的服务端工程。手动更新入口文件就是重新执行上面的下载命令。用哪个 roost-core 版本由入口在运行时决定（已有工程跟工程的 `go.mod`，新工程用最新发布版，用户可以指定），适用 v1.56.0 及以后。

前置：Go 1.27 及以上，git、curl、python3，能访问 GitHub 与 Go 模块代理；起服冒烟要 docker；客户端是 Unity / C# 时导出协议要 protoc 31.1（正文 compose 第 1 步写了怎么准备）。

## 可选：禁止 agent 修改入口文件

skill 规定使用它的 agent 只读；正文在 Go 模块缓存里，本来就是只读文件。入口文件也想挡住的话，在项目的 `.claude/settings.json` 里加：

```json
{
  "permissions": {
    "deny": ["Edit(.claude/skills/roost-genre/**)"]
  }
}
```

这条只拦 Claude Code 内置的编辑工具；skill 正文里的“只读”规则仍然要遵守。

## 用法

直接对 Claude Code 说需求，例如“用 roost 做一个休闲游戏的服务端：登录建角、金币、每日签到……”。skill 会：

1. 开场一次问完几个白话问题：客户端用什么做、工程在哪，大概多少人在线，局外有没有要保存的东西，有没有实时战斗，你想让 AI 先做完（业余，默认）、逐项确认（专业）还是只给决策清单；
2. 判断需求是否真的需要这套服务器框架；
3. 拆需求、分服务器与客户端、复用模块、手写其余、离线验收，再真起服务做一次登录冒烟；
4. 在生成的工程里写 `docs/DEMO.zh-CN.md`（当前状态）、`docs/CLIENT.zh-CN.md`（客户端对接的单一入口，同一份在导出目录 `shared/README.md`）与 `docs/DEMO-HISTORY.zh-CN.md`（逐次变更，也是之后增改需求时的记忆）；专业模式还有决策单 `docs/DECISIONS.zh-CN.md`。

之后在同一个工程里继续提需求或改需求即可。别的 agent（例如 Unity 客户端工程里的 agent）也可以调用它，结束时它回交一块固定格式的交付摘要，客户端 agent 照 `docs/CLIENT.zh-CN.md` 接入。完整流程见对应版本的 [compose.md](../docs/agent-skills/roost-genre/references/compose.md) 与 [SKILL.md](../docs/agent-skills/roost-genre/SKILL.md) 的“与其他 agent 对接”。
