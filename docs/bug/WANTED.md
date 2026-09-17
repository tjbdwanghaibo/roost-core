# Wanted：实现侧提交的待审查候选

这里放**实现 / bugfix 一侧在干活时看到、但自己不该拍板的疑点**。它们不是 RR：没有编号、没有状态、不进覆盖矩阵。
review agent 每轮看一眼，对每条做三选一——登记为 RR（分配编号、写 REVIEW-*.md、移出本表）、判为"不是问题"（写一行结论后移出）、
或"再观察"（留着，写明还缺什么证据）。实现侧**不要**据此直接改码；这张表存在的意义就是把"发现"和"定契约"分开。

格式：一条一个二级标题，写清位置（仓 / 文件 / 行 / SHA）、现象、为什么觉得可疑、能怎么复现、候选修法（可选）、来源。

## W-2026-09-17-01：其余六个 kit RPC 服务（account / chat / global / activity / platform / rank）是否按 M-06～M-11 的形状下沉 core

- **位置**：roost-kit `4830150`，`service/account`（`Accounts`，`account_rpc.go:41`）、`service/chat`（`Messaging`，`chat_rpc.go:66`）、
  `service/global`（`Routing`，`global_rpc.go:45`）、`service/global/activity`（`Coordinator`，`activity_rpc.go:39`）、
  `service/platform`（`Platform`，`platform_rpc.go:46`）、`service/rank`（`Rank`，`rank.go:23`）。六个包的领域文件
  （types / service / store / redis_store / identity / admin / member）目前只 import core 与 kit 的 `mods`、`service/servicemetrics`（后者已是 core 别名）；
  account 另有 `service/directory`。
- **现象**：ARCH-01 点名的 session / mail / match 已按"领域实现 + RPC 接口 + 传输半进 core，kit 留 Mod / 装配半 / 别名"的形状完成
  （M-06～M-11，core v1.15.5 / kit v1.14.6 / codegen v1.15.8）。剩下六个服务仍整体在 kit：领域规则（账号身份与目录、聊天频道与保留、全局路由、
  活动窗口协调、平台身份、排行榜）和 Mod 混在同一包里，与 core README"核心实现在 core、kit 只装配"的自述不一致。
- **为什么可疑 / 为什么不自己拍板**：这是职责归属（ARCH 类）而不是缺陷；review 的 ARCH-01 只点名三个，并写明"没有穷尽盘点 Kit 全目录"、
  "statslog 一类运行时统计与接入便利逻辑可以保留在 Kit"。六个里哪些算"通用服务领域"（大概率：rank、chat、activity）、哪些算"接入便利 / 平台胶水"
  （可能：platform、account 的目录部分、global 路由）需要 review 定，避免为了对称把不该下沉的也下沉。account 的 `RegistryBound` 钩子
  （U-0217 同批加的 collaborator 绑定）与 `service/directory` 依赖也要先定去向。
- **候选修法**：按 M-07/M-08 + M-11 的模板逐包做——core 得领域文件 + 接口 + `-emit transport`，kit 留 Mod / server run + `alias.go`
  + `-emit assembly -dir github.com/tjbdwanghaibo/roost-core/service/<x> -out .`；每包一个 M 编号；判为"留 kit"的只在 kit README 写明理由。
  各包 Redis key / errcode 段 / RPC 方法名不变；每包各需 core + kit 一次发版（可以攒一批）。
- **复现 / 验收草稿**：`go list -deps ./service/<x> | grep roost-kit` 在 core 为空；kit 全套；codegen 生成工程（framework services 引用的是 kit 别名）编译。
- **来源**：2026-09-16 ARCH-01～04 收尾时的遗留项（`docs/history/POST_RELEASE_PLAN_2026-09-08.md` §0.6）。

## W-2026-09-17-03：attribute 生成器的输出依赖"所在包需提供"的七个类型，而 attribute feature 的脚手架不提供它们

- **位置**：roost-codegen `internal/attribute/gen.go`（生成物引用 `AttrID` / `AttrValue` / `AttributeMeta` / `AttributeProfile`，容器访问器还引用
  `Snapshot` / `Container` / `Selector`，都不带包名）；`internal/roost/render.go:138-160`（feature `attribute` 的脚手架只写 `package attribute` 一行的 `doc.go`）；
  `docs/CODEGEN_REFERENCE.zh-CN.md` §11 只说"所在包需提供框架约定的 … 类型"。roost-core / roost-kit 里没有任何包定义这些类型。
- **现象**：`features` 加 `attribute` → 写一个 `//roost:attribute` profile → `make generate` 生成 `gen_*_attribute.go` → 编译失败（`undefined: AttrID` 等）。
  没有一个可以 import 的权威定义，也没有一份写在文档里的接口签名可以照抄；attribute 生成器在全部三仓里零消费者（codegen 自己的测试只看生成文本）。
- **为什么可疑 / 为什么不顺手改**：这是"生成器承诺了一份契约，但契约在哪里没人写"（C4 跨包契约不一致），修法要先定：这些类型是进 roost-core
  （新包 `attribute`，生成物 import 它）、还是由脚手架的 `doc.go` 生成一份默认定义（每工程一份，可改）、还是生成器自己在 `gen_*_attribute.go` 里带上。
  三种选择对 core 的 API 面和生成工程的自由度影响不同，需要 review 定；实施侧本轮做 demo 时因此**没有**接 attribute（原计划 B8）。
- **候选修法**：A. core 新包 `attribute` 放这七个类型 + `AttributeProfile` 接口，生成物 import；B. 脚手架在 `game/gameplay/attribute/doc.go`
  生成默认定义并标"应用拥有"；C. 生成器每个 profile 文件自带私有别名（多 profile 会重复定义，需去重）。我倾向 A（与 dataengine.DirtyHook 同一模式）。
- **会红的测试草稿**：生成工程 `-features …,attribute`，写 `//roost:attribute index=1 max=4 type P struct{HP int64}`，`make generate && go build ./...`——现在红。
- **来源**：2026-09-17 实施 game-demo B8（attribute 演示）时发现。

## W-2026-09-17-02：dao 嵌套里的嵌套（map / slice / struct 字段的元素）从存储解码后没有 dirty 传播接线

- **位置**：roost-codegen `internal/dao/template_nested.go`——`Set<Field>` 对 Kind 2（map）只 `s.<f>.Set(key, val)`、Kind 3（struct）只 `s.<f> = v`，
  以及 U-0224 新增的 `set<Field>RawMap`，都没有对元素调用 `SetNotify`；对照 `template_dao.go` 的 DAO 层：`setXRawMap` / `SetX` 对每个嵌套值
  `val.SetNotify(func() { d.markXKeyDirty(key, val) })`（:266、:323、:342）。
- **现象**：`hero.GetEquips(1).GetGems(2).SetLevel(3)`——改的是嵌套里的嵌套，`GemInfo.Mark()` 的 notify 为 nil，`EquipInfo` 与 `HeroDao` 都不知道，
  这次变更不进 dirty、不进事务 patch。用 golden 的 `EquipInfo.gems: map[int32]*GemInfo` 就能写出会红的测试。
- **为什么可疑 / 为什么不顺手改**：嵌套 struct 模板从一开始就只把自己的字段变更 `Mark()` 给父级，第二层往下从未接线——是"承诺无实现"（C2）
  还是"嵌套只支持一层"的未写明限制，需要 review 定；接线要决定 notify 闭包捕获什么（`s.Mark` 即可，父链自然递归），以及 slice 元素的处理。
- **候选修法**：嵌套模板对 Kind 3 字段与 Kind 2 / Kind 1 的元素在 Set 与 RawMap 恢复时 `SetNotify(s.Mark)`；DAO 层 `Init` 已对第一层做了同样的事。
- **来源**：U-0224 修 BSON 表示时发现（`docs/bugfix/U-0224-dao-nested-bson.md` 未做一节）。

## 已分流记录（W-2026-09-16-01 已分流）

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
