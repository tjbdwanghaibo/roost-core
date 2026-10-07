---
name: roost-review
description: Roost（单仓 roost-core，kit/ 与 codegen/ 已并入）审查一轮：先读 core 的 AGENTS.md → docs/agent-skills/roost-coding/SKILL.md → docs/CORE-OPTIMIZATION-HANDOFF.md，只读不改码，按 Nest / Sync / DataEngine+Remote 的执行契约找出确定的缺陷，查到代码级根因，登记 RR-YYYYMMDD-NN 到 roost-core/docs/bug/，并分流 docs/bug/WANTED.md 里实现侧挂起的候选。触发词：roost review、审查一轮、分流 wanted、$roost-review、继续 Roost review。
---

# Roost review：只审查，不改码

`roost-bugfix` 是修的那一半，这是**找**的那一半。产出是 `roost-core/docs/bug/` 下的问题记录，以及必要时 `docs/review/` 下的
运行记录。**2026-09-26 起只有一个仓**：`/Users/whb/roost/roost-core`（一个 go.mod、一个 tag；`kit/`、`codegen/`、`demo/`
是顶层目录，旧 roost-kit / roost-codegen 目录已不存在）。`main` 直推，中文提交信息，末尾
`Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`（以当次会话 system reminder 给的署名为准）。

## 0. 规则源与硬规则

**仓库里的规则优先于本 skill。** 每轮先读：`AGENTS.md` → `docs/agent-skills/roost-coding/SKILL.md` →
`docs/CORE-OPTIMIZATION-HANDOFF.md`。roost-coding 对 review 的要求：给出触发条件、影响、证据和源码位置；
**区分确认缺陷、疑点和重构建议**；可补已核实的中文注释，但纯 review 不修行为；用户说"只读"时连注释也不改。
它的"必须保持的执行契约"就是本 skill 的审查清单（§2）。交接文档 §4 的"用户已接受的边界"（如 Sync 集中恢复
少量 50ms 超标）**不是缺陷，不重复登记**；但若超出已接受的具体数值 / 场景，照常登记并引用原接受记录。

- **不改业务行为，补必要中文注释**（维护者 2026-10-07 确认）。按已核实的源码补充职责、执行顺序、不变量和设计原因；允许提交审查文档及保持行为不变的注释，生成代码的注释在正式模板维护。用户明确“只读”时只记录注释建议。临时探针测试（文件名 `zz_probe_*_test.go`）跑完即删，不提交。
  **例外：用户明确说"修"**（"把这几条修了" / "继续实施修复" / "顺手修掉"）——不用再问，在同一会话切到 `roost-bugfix`。
  **先审查提交（`docs(review)`），再修复提交（`fix(<pkg>)`），两笔分开**：审查记录写的是修之前看到的东西，不能被修复结论回头改写。
  根因没定到行的，照 bugfix 规矩先定根因、先红后绿；定不出来就如实写"仍未定位"并列出已排除的机制，**绝不为交差编一个修复**。
- **一条 RR = 一个确定的缺陷**，有代码级根因、有复现或源码判定。"可能有问题"不登记，写进 `docs/bug/WANTED.md` 或丢掉。
  文档之间 / 文档与代码的不一致，若会误导使用者或下一位 agent（错误的默认值、已撤回 API 仍被描述为可用），可登记为 P2 文档缺陷；
  纯措辞问题直接在报告里列出，不占 RR。
- **等级**：P1 = 数据损坏 / 丢失 / 进程死亡 / 静默少发货 / 违反执行契约（慢池执行业务、未提交数据外发、伪造持久确认）；
  P2 = 可用性、延迟、潜伏（当前无触发路径）、可观测性。潜伏但后果重的定 P2，正文写明"定级低是因为没有触发路径，不是因为后果轻"。
- **编号连续**：`head -5 docs/bug/README.md` 看最新一条（索引是新在上的一行式列表），接着编；不跳号、不复用。
  优化 agent 也会登记 RR（如 RR-20260926-01/02），编号前务必先 `git pull`。
- **读代码走 codebase-memory-mcp**（项目 `Users-whb-roost`）：`list_projects` / `index_status` 确认 generation（默认 Verify）→
  `search_graph` → `trace_path` → `get_code_snippet`；每个引用路径 `check_index_coverage`，否定 / 穷尽结论加 scopes。
  generation 落后于 HEAD 或刷新被挡时（记忆 `codebase-memory-daemon-lock`），以当前源码补证并在记录里写明；
  不删未知锁、不停其他实例、不声称已刷新。字符串 / 配置 / 文档直接 `rg`。
- 引用代码写 `file:line` + 基线 SHA；bugfix 那边会拿当前 HEAD 核对。

## 1. 一轮的步骤

1. **拉代码**：`git pull --ff-only`，记下 HEAD SHA（记录开头写基线）。
2. **先看三张表**：
   - `docs/bug/WANTED.md` 里没有"已分流"前缀的条目——实现侧挂起、等你定性，**优先级最高**（已有现场证据，只需补根因）。
   - `docs/bug/CARRYOVER.md`：各 bugfix 记录"未做"里真正的缺口（要拍板的 / 没跑过的）。Wanted 空不等于没有遗留。
   - `docs/CORE-OPTIMIZATION-HANDOFF.md` §5（没有实施 / 没有证明 / 已撤回）：确认"已撤回"的东西没在代码里残留，
     "未验证"的东西没被别的文档写成已验证。
3. **选一条链路审**（没有 Wanted 时）：优先三大块（记忆 `roost-three-pillars-principle`）里**最近一批优化碰过的地方**——
   交接文档 §3 的"最新"行与 `git log --stat -5`；其次零覆盖 / 新接入的包（`docs/feature/GAME_DEMO_TEMPLATE.md` §9.1）。
   自己 / 别人上一轮的修复最容易被下一轮打回。
4. **查根因**（§2 的清单与 §3 的手法）。
5. **写文档**（每条 RR 一对文件，与现行 docs/bug 格式一致）：
   - `docs/bug/RR-YYYYMMDD-NN.md`：标题一句话；日期 / 基线 SHA / 等级 / 状态（"已复现，未修复" 或 "已确认（源码判定），未修复"）；
     触发条件 → 预期 vs 实际 → 根因指到 `file:line`（贴关键几行）→ 影响 → 复现命令与原始输出（或"源码判定"）→ 验收覆盖点。
     实施方向只写约束与候选，不写成命令式——那是修的那一半的判断。**明确写出本轮没做 / 没验的部分**。
   - 一轮多条且需要共享背景时，可另写 `docs/review/REVIEW-YYYY-MM-DD[-NN].md` 作运行记录（查了什么、排除了什么）。
   - `docs/bug/README.md` 头部插一行 `[RR-…](RR-….md)：一句话（未修复）`，新在上。
   - `docs/CORE-OPTIMIZATION-HANDOFF.md` §7 缺陷索引加一行（修复列写"未修复"）；若推翻了 §3～§5 的某条结论，在该处加一句指向新 RR，
     不改写原结论。
6. **分流 Wanted**：判为 RR 的，把标题改成 `## W-… 已分流：→ RR-…`（保留正文）；判非问题的删掉条目并在报告里说明理由。
7. **提交**：一笔 `docs(review)：RR-… …`，显式 `git add docs/...`，推送。
8. **只在用户说"修"时**：审查提交推完之后按 `roost-bugfix` 逐条修，每条修完两个 README 的状态改为"已修复"。

## 2. 按执行契约审（来自 roost-coding，每条都是一类可找的违例）

**Nest 与 Entity**
- 业务 handler、Guard / Entity local 锁、本地回滚只在快池；慢池只做准备 I/O、远端获取 / 确认 / 释放。找：慢阶段直接执行业务、取本地锁、
  初始化 / 发布 / 回滚未交回快池。
- 同 ID 顺序在统一准入处按显式声明的全部目标建立；内部续行沿用准入资格，不排到自己后面。找：续行重新排队、未声明 ID 被当作有 FIFO 保证。
- 快阶段 Getter 只读已加载实体（`LoadedEntitiesOnly`）；找：GetMany / preparedGetter 回退 / 动态 Cast / 自定义 Getter 绕过冷加载禁令，
  或把执行一半的 handler 搬到慢池 / 自动重试。
- 准入失败 vs 结果不确定要分清；已准入后不因释放 / 回复失败回滚。Cast、组锁、引用、完成 ticket、关闭排空各有**唯一**收尾责任——
  找重复释放、漏释放、panic 路径跳过收尾。
- worker 数、等待容量、Remote / 数据库写预算分开；锁内 WAL 准入不能被移出锁或降低持久级别。
- **快池内禁止阻塞等待，尤其禁止"投递到快池再同步等它"**（维护者 2026-09-26 明确）。"投递到快池并等待"只允许慢 worker 用
  （`entity.RunLocal` / `dispatchFastContinuation`）。找：慢阶段 ctx（Base 里的 local executor 等）随快照泄漏进快续行、
  快阶段代码经 RunLocal / 同步 Call / 冷加载间接等待快池——快池 worker 数个这样的请求就会全池饥饿死锁，并且 Guard 一直不释放。

**Sync**
- setter 只标脏；on_change 在成功准入后、Guard 释放 Entity 锁前冻结，全部锁释放且满足提交确认后才唤醒 Manager；网络发送在锁外。
  找：未提交 / 将回滚的数据被发出、同一业务多次 setter 逐次发帧。
- Interest/AOI 多来源订阅独立撤销；位置 / 关系事实受提交与回滚边界约束。
- Profile 白名单、优先级与 packer 一致；未知视图不能默默扩大字段权限。共享不可变编码，不共享可变在途字节，不拼接不透明 delta。
- 以完整 EntitySync 包为最小组帧单位；超硬上限明确失败，软预算保证合法大对象最终有进度。
- 只有交付成功的帧前缀才结算；版本 / 基线、epoch / lifetime、订阅 revision、remove-before-create、旧在途回调隔离；
  可靠增量不能 latest-only 丢弃；慢会话失败不拖垮其他会话。
- 冷创建 / 重连恢复预算与已有对象更新分开，即时 Flush 共用窗口额度；新入场 / 恢复之间和会话之间公平。
  **增量索引必须跟随同一生命周期维护**（订阅增删、会话 Hold/Ready/Close/同 ID 重开、subject 退役、预算耗尽重入）——找漏登记与残留；
  完整扫描只在显式 Audit 入口。

**DataEngine 与 Remote**
- Nest → 正式生成 DAO → 文件 WAL → 投影 → 持久确认 / 发布，区分内存修改、WAL 准入、durable、投影、远端发布 / 确认。
- async / strict / pipelined 完成条件明确；回放读取、投影批次、未确认 WAL、Remote 在途写各有预算，**用原子准入而非 Stats 瞬时读取**；
  所有出口（成功 / 错误 / 取消 / 停机 / 单条超预算）都归还预算。
- 并发投影只用于 Store 明确支持、实体与事务身份独立、无特殊屏障的记录；首次观察失败停止补位、等待已启动任务，
  checkpoint 只推进连续成功前缀。
- Remote 持久写权限由 Mongo ownership / 最新 grant / fence 与版本共同校验，Redis 只做竞争协调；多 Entity / 多 DAO 原子性、事务 digest、
  回执、outbox 重放。找测试专用弱校验入口。
- 超时 / 未知结果 ≠ 未提交或回滚；找跳过冲突 WAL、自动删记录、放宽版本比较、伪造持久确认。

**性能与证据**
- 性能结论要同机同配置同负载前后对照；区分输入速率与成功 TPS、吞吐与延迟、微基准与端到端、热点与独立实体。
  文档把短测写成长期保证、把进程内解码写成网络延迟、把拒绝下的成功 TPS 写成容量——是文档缺陷。

## 3. 查根因的手法（这些是真的省时间的）

- **先杀候选，别先找答案**。实现侧的 Wanted 通常给了两三个候选根因，挑**一行代码就能排除**的那个先做
  （例：packer 的 profile 参数是 `_`，"LOD 筛字段"候选当场出局）。
- **注释与代码对不上，就是缺陷本身**。搜 "retries"、"will be retried"、"兜底"、"保证"、"always" 这类承诺词，再看兑现它的那段。
- **乐观置位**：`x.done = true` 出现在真正的调用**之前**，后面失败只记日志。
- **超时 / 预算是否覆盖等待**：`select { case gate <- …: case <-parent.Done(): }` 之后才 `context.WithTimeout(…)`，配置值就不约束排队。
  "配置 N 秒却观测到 M≫N 秒"，先看 deadline 加在等待之前还是之后。
- **增量结构 vs 全量扫描**：新加的索引 / 计数（替代全量遍历的优化）要逐个列出原全量扫描覆盖的所有状态来源，再逐个找增量维护点；
  少一个就是漏登记或残留。Stats 与 AuditStats 口径不一致就是线索。
- **预算归还**：列出 acquire 之后的每个 return / panic / ctx 取消出口，逐个找 release；滑动窗口补位与"首次失败停止"是否同时成立。
- **接口没被实现**：Go 签名不匹配只会静默不生效（`bson.Type` vs `byte`、`error` vs 无返回值都踩过）。去依赖里看接口原型，
  `grep` 全仓同类实现；实施方向带一条"加 `var _ Iface = (*T)(nil)`"。
- **替身比实现更宽容**：跨语言（Lua / BSON / 驱动）逻辑，Go 写的 fake 只证明你的理解。要么跑真组件，要么在记录里写明这一层没验。
- **数一数触发条件**："4 个不复现、16 个每轮复现"比"偶发"值钱十倍。负载相关往往指向容量上限 / 耐久闸 / 队列这类拒绝路径。
- **并发复现控制顺序**：用屏障 / 钩子 / select 入口钉住 goroutine，不用 sleep 赌概率；跑 `-race`。

## 4. 环境（复现用，全部在 roost-core 内，`GOWORK=off` 从模块根跑）

- 定向回归入口见交接文档 §6（race / vet / glsvet / 生成工程脚本 `scripts/test-*-generated.sh`）。
- 集成环境：`kit/scripts/integration/dataengine-env.sh up`，brew 二进制不是 docker，env 文件 `/tmp/roost-dataengine-it/env.sh`
  （含凭据，不输出、不提交）。Mongo 副本集 `roost-it`（27117/27118/27119），Redis 6379，NATS 14222。
  故障矩阵 `scripts/test-remote-matrix.sh` 会注入故障，单独跑，不与其他验收共用正在运行的环境。
- 生成工程：`GOWORK=off go run ./codegen/cmd/roost project new X -module example.com/X -out <scratchpad>/X
  -mods configdata,mongo,nats,dataengine,nest,saga,redis,etcd -template game-demo`，把 configs 里的 prefix / 端口 / 连接串改成本机的
  （每轮换 prefix，否则和上一轮的流、键撞车）。起服务 `sh deploy/dev/run.sh start`；第二进程 `sh deploy/dev/second-game.sh start`；
  机器人 `go run ./cmd/loadtest -endpoint 127.0.0.1:7000 -count N -nats-prefix <p> -account-nats nats://127.0.0.1:14222`。
- 性能入口 `scripts/perf/*.sh`（每次新 label 目录）；性能期间不跑 race、故障注入或索引重建。审查通常不重跑长压测，引用交接文档的数据时写明是引用。
- **清场要清干净**：drop `game` 的同时 drop `remote_entity` 和 `saga`，删 `data/wal`，删该 prefix 的 redis 键；只清一半会造出假现象。
- JetStream 预留被历次实验占满（`no suitable peers for placement`）时按名字删自己造的流，别动 `itest:` 开头的。

## 5. 汇报格式

先给表：编号 / 等级 / 一句话结论（确认缺陷、疑点、文档不一致分开列）。然后每条**三句话**：现象是什么、根因在哪一行、
为什么它一定是缺陷。最后说明哪些是源码判定、哪些跑过（命令）、哪一部分本轮没做；提交了没有、推了没有。


**反复出问题要上报方向判断**（维护者 2026-10-05）：同一模块 / 同一机制在近期多轮里反复出缺陷，或某次 bugfix 之后又在同一处出 bug（修复被打回、补修再补修），不要只是继续打补丁——在汇报里单列一段给维护者：列出该模块近期的问题与修复链（编号、提交），判断根因是实现细节还是前提 / 设计 / 实现方向有问题，给出是否需要修正思路、简化或改变实现方向的建议（候选方向与代价）。信号：同一不变量第二次被打破；修复在增加状态 / 分支 / 重试而不是减少；状态机交错类问题反复；修复依赖越来越多的时间 / 预算假设。先例：game-demo PlayerOwners 的按玩家租约连续多轮出问题，维护者确认前提不成立后改为静态绑定 + App 单实例锁（`docs/feature/APP-SINGLETON-LOCK-2026-10-05.md`）。 审查时同样适用：同一模块本轮又登记新 RR、或新 RR 打回了上一轮修复时，汇报里单列"方向判断"。

## 6. 相邻 skill

- `roost-bugfix`：修的那一半。
- 仓库内 `docs/agent-skills/roost-coding`（共同规范）与 `roost-optimize`（优化入口）。
- `roost-consolidate`：合仓已完成，只剩历史参考。
