# Agent skills（仓库正本）

本目录是维护 roost-core 时 agent 使用的规范与 skill 的**正本**，随仓库交接，不依赖某台机器的安装。本机 `~/.claude/skills/<name>/`、`~/.claude/agents/roost-maintainer.md` 只是镜像：改动先改这里，再 `cp` 同步过去。

| 名称 | 文件 | 用途 |
| --- | --- | --- |
| roost-maintainer | [roost-maintainer/AGENT.md](roost-maintainer/AGENT.md) | 维护 agent：汇总维护者长期规则、常用 skill、本机环境与发版流程，接手实现类任务的入口 |
| roost-coding | [roost-coding/SKILL.md](roost-coding/SKILL.md) | 写代码 / review / bugfix 的共同规范与执行契约（规则源） |
| roost-bugfix | [roost-bugfix/SKILL.md](roost-bugfix/SKILL.md) | 一轮 RR 修复流程；踩坑事实在 `reference/lessons.md` |
| roost-review | [roost-review/SKILL.md](roost-review/SKILL.md) | 审查行为、补必要中文注释，登记 RR；不改业务行为 |
| roost-optimize | [roost-optimize/](roost-optimize/) | 性能优化入口 |
| codebase-memory | [codebase-memory/SKILL.md](codebase-memory/SKILL.md) | 用 codebase-memory-mcp 图工具读代码 |
| roost-consolidate | [roost-consolidate/SKILL.md](roost-consolidate/SKILL.md) | 历史：三仓合一（已完成），只作参考 |

同步镜像（在仓库根执行）：

```bash
for d in roost-bugfix roost-review codebase-memory roost-consolidate; do mkdir -p ~/.claude/skills/$d && cp -R docs/agent-skills/$d/. ~/.claude/skills/$d/; done
```

```bash
mkdir -p ~/.claude/agents && cp docs/agent-skills/roost-maintainer/AGENT.md ~/.claude/agents/roost-maintainer.md
```
