---
name: roost-bugfix
description: Fix confirmed Roost bugs from review and bug records, preserve regression evidence, document compatibility and verification, then commit and push the authorized changes. Use when the user requests Roost bug fixes; ordinary roost-review remains a documentation-only workflow.
---

# Roost Bugfix

用户指定问题、模块或 review 轮次时，完成该范围的修复、回归、交接与已授权的提交推送。不要把“继续 review”自动变成改代码，也不自动发版、打 tag 或部署。

## 接手与范围

- 按 go.mod 和 Git remote 定位当前仓库；Roost 已合为 roost-core 单模块，框架、kit、codegen 均在其中，不依赖历史三仓路径。
- 读取仓库 AGENTS.md、docs/agent-skills/roost-coding/SKILL.md 和相关交接，核对 docs/bug、docs/bugfix 与当前源码。稳定 RR/W 编号沿用；同根因残留追加原项。
- 每次 fetch 最新代码；干净跟踪分支快进更新。保留未提交或其他 agent 的工作，复用空闲隔离工作树；不擅自 reset、stash、force push 或改全局 Git 配置。
- 记录最新源码基线、选定问题清单和完成状态。当前用户对明确范围的“修复/实施”授权足够执行；历史 review 的“未实施”不是重复审批理由。遗漏业务语义时先完成独立项，再提出具体选择。

## 定位与修复

- codebase-memory-mcp 优先：确认项目和 generation，search_graph → trace_path → get_code_snippet，检查相关分页。全部证据/修改路径调用 check_index_coverage；缺口、过期或工具不可用时读当前源码补证并披露限制。
- 先保留修前失败证据，再修根因与必要调用链。已有 .go.txt/复跑脚本可作为种子，转成维护在正式包内的行为回归；不是把所有临时断言原样永久化。
- 优先复用框架现有 versionstore、Directory、索引、服务生命周期、codegen 等能力。保持少分包、易读主流程；不以新抽象、无界缓存或重试掩盖协议错误。
- 明确已提交、未应用、结果未知和补偿失败。未知结果不得直接回滚已可能提交的数据；清理原子校验不可复用身份，版本跨删键重建的重复不能当身份。
- 状态、持久格式、公开 API 或 wire 改变时，同步装配/生成物/调用方与兼容说明。默认不执行生产数据迁移；提供安全恢复入口或可审查迁移方案，不静默改旧数据含义。

## 验证与记录

- 以确定性交错、可控时钟、故障注入验证不变量；修后同时覆盖成功、失败/未知、重试、并发和生命周期边界。接口重构后调整故障钩子，保留业务断言；钩子不触发的超时不是红测。
- 按变更运行定向测试，并发跑 race；存储协议补真实隔离 Redis 等依赖，生成接口执行自身 go:generate 与 check/消费者验证。通过后只因新增修改或未解风险扩大检查，不重复无关长稳。
- 隔离依赖只关闭自己创建的实例；临时文件仅清理已确认内容的自身文件。凭据、大日志、二进制与缓存不提交。
- docs/bugfix/<RR>.md 记录根因、决策、最终行为/文件、兼容和恢复、修前/后命令与结果、未验证边界；保留 bug/review 历史，追加状态和修复链接。
- 更新 bug/bugfix 导航、docs/review/PROGRESS.md、相关机制/接手文档。按问题逐项标记已修复、已验证或残留，不能以包测试绿代表全部完成。

## 提交与续跑

本用户的 Roost 流程已授权收尾提交并推送 GitHub；本轮明确限制优先。完成具体实现和检查后直接收尾，不再次索要常规提交确认。

- 显式暂存本轮源码、必要测试、生成物和文档，不夹带其他 agent 工作。
- fetch 后整合远端变化，正常 push，不改写他人提交。新相关源码需补验；仅文档前进不重跑源码测试。
- 确认远端包含本次提交。报告已修/残留项、实际验证、兼容限制、文档与 commit 链接；commit/push 不等于发布或部署。
- 用户继续未完成任务时按上次清单推进，不重做已经验证的独立项。故障或环境阻碍要留下准确原因与可复跑入口，不能把部分交付写成全部完成。
