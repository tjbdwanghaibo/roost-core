# 在自己的项目里使用 roost 的 Claude Code skill

`roost-genre/SKILL.md` 是一个入口 skill：放进任何项目，Claude Code 就会在“按需求做游戏服务器 / 给 roost 工程加功能”这类任务时用它。它不带正文，运行时从 GitHub 取 roost-core 的**指定发布版**（Go 模块缓存，只读），读其中的 `docs/agent-skills/roost-genre/`，并用同一版本的 CLI 生成工程。不需要本机有 roost-core 检出。

## 安装

在你的项目根目录执行：

```bash
mkdir -p .claude/skills/roost-genre
curl -fsSL https://raw.githubusercontent.com/tjbdwanghaibo/roost-core/main/claude-skill/roost-genre/SKILL.md -o .claude/skills/roost-genre/SKILL.md
```

想对所有项目生效，就放到 `~/.claude/skills/roost-genre/SKILL.md`。

**更新**：入口文件每次使用时会先检查两样——自己有没有新版（与 GitHub 上 main 的这份比较）、roost-core 有没有新的发布版——有新版就告诉你并给出更新办法，不会自己改文件，也不会未经同意升级你的服务端工程。手动更新入口文件就是重新执行上面的下载命令。入口文件本身与版本无关：用哪个 roost-core 版本由它在运行时决定（已有工程跟工程的 `go.mod`，新工程用最新发布版，用户可以指定）。

前置：Go 1.27 及以上，能访问 GitHub 与 Go 模块代理。

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

1. 问一次要不要专业模式（不回答就是业余模式）；
2. 先判断需求是否真的需要这套服务器框架；
3. 拆需求、分服务器与客户端、复用模块、手写其余、离线验收；
4. 在生成的工程里写 `docs/DEMO.zh-CN.md`（模块、实体、服务、协议、客户端对接等）与 `docs/DEMO-HISTORY.zh-CN.md`（逐次变更，也是之后增改需求时的记忆）。

之后在同一个工程里继续提需求或改需求即可。完整流程见对应版本的 [compose.md](../docs/agent-skills/roost-genre/references/compose.md)。
