# 框架整体双文档：每个分区 agent 的共同要求

**维护者原话**（2026-10-06）：
- “还有一个就是这个框架整体的说明文档和实现，越详细越好”；
- 发版双文档那次的要求同样适用：“兼顾人类阅读和agent使用”，维护者自己读，也会交给另一个 agent 做 review。

## 规格与基准
- 先完整阅读规格 `docs/review/handoff-v1.23.0/framework-docs-spec.md`。分区划分、两份文档各自的章节结构、写作要求都以它为准。
- **源码基准是 tag `v1.23.0`（`28912cd6`）。**
  - 所有 `path:line` 以这个 tag 为准，用 `git show v1.23.0:<path>` 或者在 tag 上建一个 detached worktree 来读。
  - codebase-memory 图谱可能落后于 tag。图谱只用来定位，结论以源码为准，并在文档里注明这一点。
- 可以参考但不能照抄的素材：
  - `docs/release/v1.23.0/`：三份分册（本版改动的详细记录），加上 `v1.23.0-GUIDE.md` / `v1.23.0-IMPLEMENTATION.md`；
  - 各模块原有文档：`docs/USER_GUIDE.md`、`docs/INTERNALS.md`、`SAGA.md`、`docs/skill/*`、`kit/README.md`、`codegen/README.md`、`docs/feature/*` 方案、`docs/bugfix/*` 记录；
  - `docs/agent-skills/roost-coding/SKILL.md`：执行契约。

  框架文档写的是**整体现状**，不是改动记录。历史只放在实现文档的“历史与重要修复”一节，并且只列改变过设计的那些。

## 产出
- `docs/framework/guide/NN-<area>.md`（说明）和 `docs/framework/impl/NN-<area>.md`（实现）。NN 和 area 名用规格里的，例如 `01-app-lifecycle`。
- 两篇在开头和每一节末尾互相链接。
- 每篇开头写一个“速览”：三到五句话说明这一块是什么、最重要的保证、最容易踩的坑。
- 再给一张“本篇覆盖的包”表，列出包路径和职责。
- 跨分区的内容只引用对方文件，不展开重复写。分区编号对照：00 总览、01 app、02 nest、03 dataengine、04 sync、05 remote/mirror、06 saga、07 配置、08 skill、09 kit 服务、10 时间、11 可观测、12 codegen。对方文档可能还没写出来，链接照写，文件名按规格的命名推断，例如 `../guide/03-dataengine.md`，最后汇总时统一核对。
- 越详细越好，但要分层：说明文档面向业务作者和运维，实现文档面向 review agent。
  - 说明文档要给最小可运行示例，引用仓库里真实的示例或生成代码路径。
  - 实现文档的主流程要配 mermaid 时序图或状态机。
  - 不变量清单每条都写清三件事：内容、在哪里强制（path:line）、由哪个守卫测试守住。
  - review 检查点要写具体的问题，例如“确认 X 只经 Y 进入：看守卫 Z 是否覆盖 W”。
- 术语用 v1.23.0 的现行名字，例如 skill 里叫 Spawn（衍生物）和 Summon（召唤物）。

## 纪律
- **只新增 `docs/framework/**` 下属于自己分区的两个文件**，不改代码，也不改别的文档。发现别处文档和源码不一致时，在报告里列出来，不要改。
- 写作风格按 roost-coding 的“写给人阅读”：先结论后细节，用表格、短段落和图，不堆形容词；推断的地方写明“推断 / 未验证”。
- 用独立的 worktree，放在 scratchpad 下，分支名用 `fw-NN`。push 前先 `git pull --rebase origin main`，只用 `git add` 显式加自己的两个文件。
  - 提交信息用中文：`docs(framework)：NN <area> 说明与实现`，末尾加 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`。
  - 推 main 被拦时，推同名分支，然后在报告里说明。
- 验证：根包 `GOWORK=off go test -count=1 -run 'Markdown|Conflict' .` 必须通过。指向尚未存在的分区文件的链接，用 `<!-- pending: ../guide/NN-x.md -->` 注释占位，正文里写纯文本，这样不会让链接门禁报红；最后汇总时再改成真链接。
- 完成后删掉 worktree 和本地分支。
- 报告内容：两篇的规模（行数、主要章节）、引用了多少处 `path:line`、发现的文档与源码不一致之处、跨分区需要汇总时处理的事项。

## 资源约束（维护者 2026-10-07）

**不要派任何子 agent**（不做 fact sheet / 调研 fan-out），自己直接读源码；同时并行的分区 agent 不超过 2 个。
