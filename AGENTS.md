# Roost agent 工作入口

编写、优化或审查本仓库代码前，读取 [roost 写代码基本要求](docs/agent-skills/roost-coding/SKILL.md)。这是可随仓库交接的规则源，不依赖某台机器的 skill 安装；这里的 skill 是 agent 开发规范，和 `skill/` 游戏技能模块无关。

当前优化状态、业务目标、已接受的性能边界、验收与复跑入口见 [核心优化交接](docs/CORE-OPTIMIZATION-HANDOFF.md)。按任务读取相关原始记录，不因历史“待实施”文字重复改造已完成链路。

## Codebase Memory

- 结构发现优先使用 codebase-memory-mcp：search_graph → trace_path → get_code_snippet；复杂关系用 query_graph，高层概览用 get_architecture。字符串、配置和非代码文档可直接搜索。
- 每次会话开始或上下文压缩后先通过 list_projects/index_status 确认最近项目与 generation，默认 Verify；处理相关分页。
- 候选路径明确后，用 check_index_coverage 检查全部证据路径；否定/穷尽结论加对应 scopes。过期、部分解析、跳过、排除或未知覆盖必须读取源码补证；无记录缺口不代表完整。
- 工具不可用时明确限制，按精确源码和 rg 继续。不要声称使用了不可用索引，或把旧代际当新代码。
- 若任务获准委派，先在父任务完成图谱/coverage 定位，传递项目、代际、范围、符号、分页、缺口和源码补证；子 agent 无图谱能力时使用这些证据和当前源码。
