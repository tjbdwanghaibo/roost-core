# Wanted：实现侧提交的待审查候选

这里放**实现 / bugfix 一侧在干活时看到、但自己不该拍板的疑点**。它们不是 RR：没有编号、没有状态、不进覆盖矩阵。
review agent 每轮看一眼，对每条做三选一——登记为 RR（分配编号、写 REVIEW-*.md、移出本表）、判为"不是问题"（写一行结论后移出）、
或"再观察"（留着，写明还缺什么证据）。实现侧**不要**据此直接改码；这张表存在的意义就是把"发现"和"定契约"分开。

格式：一条一个二级标题，写清位置（仓 / 文件 / 行 / SHA）、现象、为什么觉得可疑、能怎么复现、候选修法（可选）、来源。

## 已分流记录（当前无待审查候选）

W-2026-09-16-01 已于 2026-09-16 登记为 [RR-20260916-05](REVIEW-2026-09-16-04.md)，不再属于待审表。确认的是策略注入承诺无效；原草稿预设 Enqueue/Sweep 自动成组，与当前 Store 契约不符，不直接作为修复测试。建议保留调用方驱动，移除无效 Mod/Config/codegen 注入入口。具体实施与验收以链接文档为准。

### W-2026-09-16-01 原始候选（仅保留来源，不代表最终方案）

- **位置**：roost-kit `527eecd`，`service/match/match_mod.go`（`NewMod(grouping Grouping, …)`，头注释"Grouping is a constructor
  argument because 'which candidates form a match' is the whole of a game's matchmaking policy"）；`service/match/queue_store.go:118`
  （`Config.Grouping`）与 `:159-161`（`if cfg.Grouping == nil { cfg.Grouping = FirstComeGrouping{} }`）。
- **现象**：`grep -rn '\.Group(' service/match/*.go | grep -v _test | grep -v grouping.go` 为空——`cfg.Grouping` 被赋值后再没有被读。
  `Enqueue` 只入队，成组完全由调用方 `Candidates → Commit` 驱动；`Sweep` 只处理过期票。生成工程的
  `internal/service/match/collaborators.go` 因此有一个 `Grouping()` 返回 nil 的函数，注释却说"replace it with the project's own rules"。
- **为什么可疑**：Mod 的公开签名与注释承诺了一个策略注入点，运行时不履行。一个项目实现了自己的 `Grouping` 交给 `NewMod`，
  会以为匹配按它的规则进行，实际仍是调用方（若有）拿 `Candidates` 自己配对——静默失效，属于 C2（承诺无实现）/ C4（跨包契约不一致）一类。
- **复现（会红的测试草稿）**：
  ```go
  // service/match/grouping_wired_promises_test.go
  type recordingGrouping struct{ calls atomic.Int32 }
  func (g *recordingGrouping) Group(q Queue, c []Ticket) ([]Ticket, bool, error) { g.calls.Add(1); return FirstComeGrouping{}.Group(q, c) }
  // 用现有 harness 建一个 Store（Config.Grouping = &recordingGrouping{}），Enqueue 两张不同 subject 的票到 GroupSize=2 的队列，
  // 再 Sweep 一次；断言 g.calls.Load() > 0，或第二张票的 State 为 matched。当前实现两者都不成立。
  ```
- **候选修法（定契约后再选）**：
  A. 服务端驱动——`Enqueue` 末尾在同一次 CAS 里 `Grouping.Group(queue, waiting)`，成组即 `Commit`，票据直接以 matched 返回；
     调用方不再需要 matchmaker 循环，但 `Grouping` 进入热路径，且要论证与 `Commit` 的原子性一致。
  B. 删掉 collaborator——`NewMod` 去掉 `grouping` 参数，`Grouping` 保留为调用方工具；codegen `internal/roost/framework_services.go`
     的 match collaborators 模板同步删 `Grouping()`。改动更小，也让签名与行为一致。
- **来源**：game-demo 第五批实施时发现（roost-core `docs/feature/GAME_DEMO_TEMPLATE.md` §7.5）；demo 的
  `internal/service/<game>/matchmaker.go` 直接调 `svcmatch.FirstComeGrouping{}.Group`，是这个接口目前唯一的使用方式。
