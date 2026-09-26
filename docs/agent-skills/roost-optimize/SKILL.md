---
name: roost-optimize
description: 优化 roost-core 的 Nest、DataEngine、Sync 和 Remote；用于“roost优化”、核心架构整理、重构、review 或 bugfix。复用 roost-coding 基本规范，纯重构先形成方案，已授权实施和确认 bug 直接推进并留证据。
---

# roost优化

先读取 [roost 写代码基本要求](../roost-coding/SKILL.md)，这是本入口的共同规范，保留原 skill 的可读性、图谱、重构、历史 bug 授权与双文档要求。仓库版本位于 `docs/agent-skills/roost-coding/SKILL.md`；若本机缺少相邻副本，直接读取当前 roost-core 仓库版本。两处均缺失时说明规范缺失，不声称已加载。

按 `docs/CORE-OPTIMIZATION-HANDOFF.md` 找到当前模块的最新记录，核对源码和本次目标。用已确认的正确性问题或 profile 证据选择改动；保留少包结构与正式链路，不重复实施已完成项，也不把撤回实验当成待办。

用户明确授权实施时完成代码、适用回归、问题/修复或重构文档与交接；纯 review/只读遵守限制。历史 bug 可按当前授权直接修复，不为旧待拍板状态重复请求批准。性能偏差是否接受以用户明确决定和对应证据为准，不通过改变门禁“优化”结果。
