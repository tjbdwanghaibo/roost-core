# 仓库维护入口

当前运行时稳定基线v1.24.0。修改前阅读 [维护手册](docs/maintenance/README.md)、[模块目录](docs/framework/README.md) 和 [已知限制](docs/maintenance/KNOWN-LIMITS.md)。设计与实现的现行说明在docs/framework；开发过程资料已迁入Git历史，不再要求读取已删除的docs/agent-skills。

业务代码修改应补必要中文契约注释、按影响范围测试、更新对应现行文档。文档补丁不得混入行为重构。不等待GitHub CI，不能把未运行检查记成通过。

## Codebase Memory

- 结构发现优先使用 codebase-memory-mcp：search_graph → trace_path → get_code_snippet；复杂关系用 query_graph，高层概览用 get_architecture。字符串、配置和非代码文档可直接搜索。
- 每次会话开始或上下文压缩后先通过 list_projects/index_status 确认最近项目与 generation，默认 Verify；处理相关分页。
- 候选路径明确后，用 check_index_coverage 检查全部证据路径；否定/穷尽结论加对应 scopes。过期、部分解析、跳过、排除或未知覆盖必须读取源码补证；无记录缺口不代表完整。
- 工具不可用时明确限制，按精确源码和 rg 继续。不要声称使用了不可用索引，或把旧代际当新代码。
- 若任务获准委派，先在父任务完成图谱/coverage 定位，传递项目、代际、范围、符号、分页、缺口和源码补证；子 agent 无图谱能力时使用这些证据和当前源码。
