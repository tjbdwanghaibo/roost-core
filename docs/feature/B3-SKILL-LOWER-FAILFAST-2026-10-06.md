# B3：skill lower 查找失败一律报编译错误；phase 事件派发表单一来源

2026-10-06，分支 `b7c7b3`。来由：[DECISIONS-PENDING B3](../review/DECISIONS-PENDING-2026-10-05.md)（第二轮决定：①② 做），NC-110～117、NC-150～154、NC-210～216。

## 1. 问题

skill 编译器的类型检查（NC-210 / NC-214 补过）负责拒绝未声明 / 不在 catalog 里的名字，`lower.go` 再把名字查成槽位 / handle。lower 里的查找各写各的：多数直接读 map 零值（status / attribute / resource / tag / collision / unit template / damage 语义 / ability property / state slot / memory / 初始 phase，打断标签查不到时干脆跳过），少数 panic（goto、state plan、temporal profile、snapshot plan、process property），`$memory.` / `$local.` / `$input.` 引用查不到时退成同名 builtin、运行期才 `ErrProgramInvariant`。前面的 pass 每漏查一种名字，lower 就静默产出指向槽位 / handle 0 的 Program——NC-210、NC-214 都是这个形状。

## 2. ① lower 查找 fail-fast

- 所有“名字 → 槽位 / handle”的查找走唯一入口 `resolveName`（`skill/lower.go`）：查不到时记一条 `LOWER_UNRESOLVED` 编译错误（新诊断码 `DiagnosticLowerUnresolved`），带正在 lower 的源路径，返回零值继续，以便一次报出全部；`lowerProgram` 收尾时有任何失败就返回 `nil` 与诊断，`Compile` 不交出 Program。
- 覆盖：authority 六张表（attribute / resource / status / collision / gameplay tag / unit template，含打断标签、spawn 覆盖、各类 filter）、damage 语义、snapshot plan、state slot 与 state plan、ability property、temporal profile、memory、goto 与初始 phase、process property 与它的枚举（numeric operation / key / process kind / slot stage / variant / field，原来的 switch + panic 改成表查）；`$memory.` / `$local.` / `$input.` 前缀引用查不到时报错，不再退成 builtin。
- 不变：产物不完整、IR 形状未知、operation 与 identity 计数不符等编译器自身不变量仍 panic（不是名字查找）；visual 按源路径的查找本来就是可选的。
- 签名：`lowerProgram` 改为 `(*Program, []Diagnostic)`（包内函数）。`Compile` 对外签名不变；正常定义的输出不变（gameplay / presentation digest 不变，全部既有用例通过）。

### 2.1 回归（先红后绿）

`skill/lower_lookup_promises_test.go` `TestLowerRefusesEveryUnresolvedLookup`：以 N09 变异性质测试的全部种子（testdata fixture + 6 个补充种子）为起点，对每个种子从刚编译好的产物里删掉一张查找表的一个条目（模拟前面的 pass 漏查），再 lower。承诺：要么报 `LOWER_UNRESOLVED`（带源路径、不交出 Program），要么产出与原来逐字段相同的 Program（这个名字本来没被 lower 用到）；静默产出不同的 Program 或 panic 都算违反。输入槽、memory、ability property 同时决定 Program 布局，只在定义确实引用它们时要求报错。共检查约 590 次删除。

修前红（旧 lower，临时把旧函数包成新签名）：35 处静默兜底、23 处未解析引用照样出 Program、4 处 panic，按表：

```
status_cleanse.json: drop authority.statuses["slow"]: lower silently produced a different Program instead of a compile error (zero-value fallback)
area_heal.json: drop authority.unitTemplates["deployable.trap"]: lower silently produced a different Program ...
ammo_burst.json: drop gameplay.damage["$.phases[0].on.enter.steps[0].effect"]: lower silently produced a different Program ...
tracking_boomerang.json: drop authority.collision["terrain"]: lower silently produced a different Program ...
ammo_burst.json: drop input slot "$input.target": lower produced a Program for an unresolved reference instead of a compile error
ability_disable.json: drop ability property "enabled": lower produced a Program for an unresolved reference instead of a compile error
charge_projectile.json: drop memory "charged": panic=<nil> errors=false, want a LOWER_UNRESOLVED compile error for a referenced memory
persistent_mark.json: drop state.plans["marks"]: lower panicked (skill: unresolved state reference), want a LOWER_UNRESOLVED compile error
（另有 attributes ×1、resources ×2、statuses ×3、unitTemplates ×13、damage ×15、input slot ×21、memory ×3、snapshots.reads ×1、state.plans ×2、temporal.profiles ×1）
```

修后：该用例通过；N09 第四批的 `compile_mutation_property_test.go`（编译 ⇒ 可执行、变异改 digest）照样通过。

## 3. ② 事件派发表单一来源（做了）

评估：代价小（一个新文件、四处替换），做。

- 新 `skill/phase_events.go`：`phaseEventTable` 列出定义能写的全部 phase 事件（顺序即 lower 导出 root 的顺序）与 Runtime 是否有派发点；`dispatchedPhaseEventFlows` / `undispatchedPhaseEventFlows`。
- lower 只导出有派发点的事件（原 `phaseEventFlows` 删除）；编译期 `requireDispatchedPhaseEvents` 对表里没有派发点的事件逐个报错（原来点名 recast / timeout，消息不变）；Runtime 的派发点（executeCast 的 enter、Cancel、Release / 自动释放、policy 脉冲）改用同名常量；`InputPortDirectionChanged` / `InputPortTargetChanged` 直接定义为对应事件名。
- 守卫 `skill/phase_events_promises_test.go`：表覆盖 `phaseEventsIR` 与 `PhaseEventsDefinition` 的全部字段、每项取的是同名字段、派发与拒绝两组不重不漏、输入端口名都是表里的事件。
- 行为不变：recast / timeout 本来就在编译期被拒，lower 输出的 root 顺序与内容不变（digest 不变）。线格式解析（`parse.go` 的 `decodePhaseEvents`）仍独立列键，它是 JSON 契约，由守卫用例对齐字段数。
- 没做：Host 取值约束随环境下发（B3 ③，下个大版本）；process callback 事件（enter / hit / … / leave）是另一张表，不在本项。

## 4. 验证

`GOWORK=off`：`gofmt -l` 空；`go vet ./skill`；`go test -race -count=3 ./skill/...`（skill、combat、combatcomponent、skillcompose、skillsync）通过；根包与全仓 build / vet。

## 5. 实施状态

已实施（见 DECISIONS-PENDING B3 行的提交号）。B3 ③ 下个大版本；④ 保持方向 B。
