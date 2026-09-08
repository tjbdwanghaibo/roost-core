# roost-skill

**roost-skill 是一个 2D 权威（server-authoritative）ARPG 技能框架：JSON 技能定义经过编译器的静态证明，成为不可变 Program，由确定性 Runtime 在单一世界边界接口（Host）之上执行——全程 int64 定点数学、位一致回放。**

- Go 模块：`github.com/tjbdwanghaibo/roost-skill`（自 `v1.5.0` 起可直接 `go get`；不使用 `/v2` major 路径，版本沿 v1.x tag 线演进）
- 稳定 Go API：`github.com/tjbdwanghaibo/roost-skill/skill`；不再把 wire 版本写进目录名，也不保留 `/skillv2` 兼容包
- 当前 wire/语义线：JSON schema **`roost.skill/v2`**；编译器语义修订 **`skillv2-compiler-2`**，两者与 Go import 独立演进
- 依赖基线：`roost-core v1.12.0`；对具体游戏服务器、渲染器、传输层零依赖

第一次接入请先读[稳定 Skill API](docs/skill.md)；完整文档按角色整理在
[文档导航](docs/README.md)，从旧包升级见
[稳定包迁移手册](docs/breaking-upgrade-skill-package.md)。

## Scope：明确的非目标（deliberate non-goals）

> **以下三条是设计决定，不是待补的缺口。** 依赖它们做选型判断。

- **没有第三轴。** 位置是 2D 定点世界坐标。高度、重力、体积碰撞属于宿主世界；建模了高度的宿主在回答 Runtime 查询前，把它投影进 2D 平面（或在 `Select`/`Apply` 内自行消化）。
- **没有 navmesh、没有寻路。** 运动沿作者编写的路径与开阔空间转向进行。绕障路由是宿主的职责——Runtime 通过 `InputPositionResolver` 询问"该位置是否被阻挡"这类事实、通过 `Select` 询问世界真相，无论宿主怎么答，Runtime 都保持确定性。
- **没有客户端预测、没有 rollback netcode。** Runtime 是服务器权威的：客户端渲染 `PresentationEvent` 流、应用 `StateMutation` 记录，从不提前模拟。感知延迟由传输层和渲染层负责掩盖（施法窗口与 windup 表现的存在部分正是为此），而不是由 Runtime 去预测。

确定性是让其余一切成立的契约：只有定点整数数学、HMAC 派生随机、位一致回放、checkpoint 恢复。任何"用确定性换便利"的方案都被设计性地排除。

## 包总览

| 包 | 一句话 |
| --- | --- |
| [`skill`](skill) | 核心：严格 wire 解析 → 编译器（静态证明）→ 不可变 Program → 确定性 Runtime → `Host` 世界边界；含参考宿主 `MemoryHost`、checkpoint/replay、表现计划与表现事件 |
| [`combat`](combat) | 零依赖战斗内容电池：`AttributeSet`（属性聚合）、`BuffContainer`（叠层/驱散/免疫/韧性）、`ResolveDamage`（twelve_stage_v1 十二段定点伤害管线）、`ChanceRoll`（HMAC 确定性掷点：暴击/闪避概率 → 事实）。`MemoryHost` 直接运行这份代码 |
| [`combatcomponent`](combatcomponent) | `combat` 接入 roost-core 实体模型：`CombatDao`（脏跟踪 + 持久化）、`CombatComponent`（`nest.RecordUndo` 可逆 mutator）、`HostAdapter`（实现 `skill.Host` 的战斗面：damage/heal/shield/resource 命令与读取/PayCosts）、`StatusBridge`（status/attribute-modifier 命令落到 buff 容器） |
| [`skillcompose`](skillcompose) | 技能组合契约与策略：只经由 Program Inspector 消费编译产物，验证候选技能不超出授予的能力、预算与因果连通性 |
| [`skillsync`](skillsync) | 客户端同步协议：manifest/state/presentation 三类强类型记录、服务端 Coordinator（可见性过滤、durable outbox）、客户端 Applier，构建于 `roost-core/syncstream` 之上 |

宿主应用拥有玩法目录（catalog）并实现 `skill.Host`；客户端按 Program 身份消费一次 `PresentationPlan`，对局内消费增量 `PresentationEvent`。

---

## 快速启动

下面两条腿各自独立可跑；[examples/](examples) 里有它们的可运行工程（外加 `statusbridge`：StatusBridge + 确定性掷点演示），`cd examples && go run ./fireball` 即可。新建一个空目录：

```bash
mkdir skill-demo && cd skill-demo
go mod init skill-demo
go get github.com/tjbdwanghaibo/roost-skill@latest
```

### A. 只用 combat 电池：属性 + buff + 一次伤害解析

不需要 Runtime，也不需要 JSON——`combat` 是可以单独拿走的确定性战斗数学。

```go
package main

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-skill/combat"
)

func main() {
	const attrArmor = combat.AttributeID(1)

	// 属性集：base + 修饰器，结果与授予顺序无关。
	attributes := combat.NewAttributeSet()
	attributes.SetBase(attrArmor, 30)

	// Combatant 是伤害管线读写的平铺定点数值块；用 Observe 把属性投影进去。
	target := &combat.Combatant{Alive: true, Health: 500, MaxHealth: 500}
	attributes.Observe(func(id combat.AttributeID) {
		if id == attrArmor {
			target.Armor = attributes.Current(attrArmor)
		}
	})

	// Buff 容器：叠层/驱散/免疫/韧性，LinkAttributes 让 buff 自动物化为属性修饰。
	buffs := combat.NewBuffContainer()
	buffs.LinkAttributes(attributes)
	buffs.Apply(combat.BuffSpec{
		ID: 7, Tags: []combat.Tag{"defense"}, DurationTicks: 100,
		Modifiers: []combat.Modifier{{Attribute: attrArmor, Flat: 20, RateBP: 1000}}, // +20 且 +10%
	}, 0, 99)

	// 一次伤害解析：随机性外置，闪避/暴击作为预掷事实传入（这里都为 false）。
	source := &combat.Combatant{Alive: true, Health: 400, MaxHealth: 400}
	outcome, ok := combat.ResolveDamage(source, target,
		combat.DamageInput{Amount: 200, Type: combat.DamageTypePhysical, CanCritical: true}, nil)

	fmt.Printf("ok=%v armor=%d result=%s dealt=%d targetHP=%d\n",
		ok, target.Armor, outcome.Result, outcome.HealthDamage, target.Health)
}
```

输出：

```text
ok=true armor=55 result=hit dealt=129 targetHP=371
```

armor = (30+20)×1.10 = 55；物理减伤 10000/(10000+100×55)，200 点打出 129 点——每一步都是可复算的整数运算。

### B. 完整链路：火球术 JSON → Compile → MemoryHost + Runtime

一个带施法窗口、冷却、全局冷却与法力消耗的火球，从 JSON 一路跑到伤害落地：

```go
package main

import (
	"fmt"

	skill "github.com/tjbdwanghaibo/roost-skill/skill"
)

const fireballJSON = `{
  "schema": "roost.skill/v2",
  "id": "skill.demo.fireball",
  "name": "Fireball",
  "description": "Windup, commit, then burn the target.",
  "activation": {
    "type": "active",
    "policy": {"mode": "tap"},
    "cast_window": {
      "windup_ticks": 3,
      "commit_tick": 2,
      "recovery_ticks": 2,
      "movement": "locked",
      "turning": "allowed",
      "interrupt_tags": [],
      "refund_before_commit": true
    }
  },
  "input_schema": {"type": "entity"},
  "cooldown_ticks": 20,
  "global_cooldown_ticks": 8,
  "costs": [{"resource": "mana", "amount": 5}],
  "memory": {},
  "initial_phase": "cast",
  "phases": [{
    "id": "cast",
    "timeout_ticks": 0,
    "on": {"enter": {"flow": "sequence", "steps": [
      {"flow": "effect", "effect": {
        "type": "damage", "target": "$input.target",
        "amount": 25, "damage_type": "magic", "element": "fire"
      }},
      {"flow": "finish", "reason": "done"}
    ]}}
  }]
}`

func main() {
	// 1. Parse：严格 wire 解析（未知字段、重复键、尾随数据都会被拒绝）。
	definition, err := skill.Parse([]byte(fireballJSON))
	if err != nil {
		panic(err)
	}

	// 2. Compile：编译环境提供属性/资源/状态等权威目录，产出不可变 Program。
	environment := skill.DefaultCompileEnvironment()
	program, diagnostics := skill.Compile(definition, environment)
	for _, diagnostic := range diagnostics {
		fmt.Println("diagnostic:", diagnostic)
	}
	if program == nil {
		panic("compile failed")
	}

	// 3. Host + Runtime：MemoryHost 是内置参考世界；生产环境换成自己的 Host 实现。
	host := skill.NewMemoryHost(skill.AuthorityIdentity{
		Revision: environment.Revision, Digest: environment.Digest,
	})
	host.ConfigureGameplayCatalog(environment.Gameplay)
	host.UpsertEntity(skill.MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100,
		Resources: map[string]int64{"mana": 30}})
	host.UpsertEntity(skill.MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100})

	var seed [32]byte // 生产环境使用对局级随机种子
	runtime := skill.NewRuntime(host, skill.RuntimeOptions{MatchSeed: seed})

	// 4. Activate 进入 windup；Advance 推进确定性时间线（tick 为绝对时刻）。
	castID, err := runtime.Activate(program, skill.CastInput{Caster: 1, Target: 2})
	if err != nil {
		panic(err)
	}

	// windup 3 tick（commit 在 tick 2）+ recovery 2 tick => tick 5 施法完整结束。
	if err := runtime.Advance(5); err != nil {
		panic(err)
	}

	cast, _ := runtime.InspectCast(castID)
	fmt.Println("cast status:", cast.Status)                     // finished
	fmt.Println("target hp:", host.HealthForTest(2))             // 75
	fmt.Println("caster mana:", host.ResourceForTest(1, "mana")) // 25

	// 冷却与全局冷却（"$gcd"，从 commit tick 起算）都是普通冷却条目。
	for _, cooldown := range runtime.StateSnapshot().Cooldowns {
		fmt.Printf("cooldown %s: remaining %d ticks\n", cooldown.ProgramID, cooldown.Remaining)
	}

	// Host 事件流：伤害解析等世界事实。
	for _, event := range host.Events(0) {
		fmt.Printf("event tick=%d kind=%s entity=%d\n", event.Tick, event.Kind, event.Entity)
	}
}
```

输出：

```text
cast status: finished
target hp: 75
caster mana: 25
cooldown $gcd: remaining 5 ticks
cooldown skill.demo.fireball: remaining 17 ticks
event tick=2 kind=tick_advanced entity=0
event tick=2 kind=costs_paid entity=1
event tick=3 kind=tick_advanced entity=0
event tick=3 kind=damage_resolved entity=2
event tick=5 kind=tick_advanced entity=0
```

读这份输出就能看到施法窗口语义：法力在 **commit（tick 2）** 支付，伤害在 **windup 结束（tick 3）** 执行，全局冷却从 commit 起算（tick 5 时剩 8−3=5）。在 commit 前 `Cancel` 则退款（`refund_before_commit`）；windup/commit/recovery 期间再次 `Activate` 其他技能会得到 `ErrCasterBusy`（除非该技能声明 `"concurrent": true`），全局冷却期间则得到 `ErrGlobalCooldownActive`。

---

## 技能定义 JSON：权威参考的入口

完整语法以代码为准：wire 层是封闭的（未知字段即解析错误），所以 [`skill/wire_*.go`](skill) 就是语法的穷举定义；[`skill/testdata/`](skill/testdata) 的 37 个 fixture 每个都是可独立编译运行的样例（弹道、光环、引导、蓄力、召唤物、被动 proc、时间回溯……），并被 `acceptance_test.go` 全量执行。施法语义详见 [docs/skill-casting-and-combat.md](docs/skill-casting-and-combat.md)。

### 顶层字段（[wire_definition.go](skill/wire_definition.go) / [parse.go](skill/parse.go)）

| 字段 | 说明 |
| --- | --- |
| `schema` | 必须是 `"roost.skill/v2"` |
| `id` / `name` / `description` | 技能身份与元信息 |
| `presentation` | 可选，视觉表现声明（图标、visual ref，见 `wire_visual.go`） |
| `gameplay_tags` | 玩法标签，须在编译环境的标签目录内 |
| `activation` | `"active"`（`policy` 为 `tap`/`toggle`/`hold`/`charge`/`ammo`，可带 `cast_window`、`concurrent`）或 `passive_on_hit`/`passive_on_damaged`/`passive_on_kill`/`passive_on_status`/`passive_on_resource`（带 `event_filter`、`proc_policy`） |
| `input_schema` | `none` / `direction` / `position` / `entity` / `direction_position` / `entity_position` / `two_point` / `drag` / `path` |
| `cooldown_ticks` | 技能自身冷却 |
| `global_cooldown_ticks` | 全局冷却，**从 commit tick 起算**，同步为保留程序 id `"$gcd"` 下的普通冷却条目 |
| `costs` | `[{"resource": ..., "amount": ...}]`，amount 可为表达式；在 commit 时原子支付 |
| `memory` | 施法内可变变量声明（类型 + 默认值） |
| `persistent_state` | 跨施法持久状态声明 |
| `initial_phase` / `phases` | 相位机；每个 phase 的 `on` 支持 `enter`/`recast`/`cancel`/`direction_changed`/`target_changed`/`timeout`/`release`/`pulse` 事件挂 flow |

### cast_window 字段（[wire_cast_window.go](skill/wire_cast_window.go)）

| 字段 | 说明 |
| --- | --- |
| `windup_ticks` | 前摇长度（与表达式互斥） |
| `windup_ticks_expression` + `windup_ticks_min`/`_max` | 运行时求值的前摇（如按攻速缩放），结果**钳制**进 `[min, max]`；使用表达式时必须声明边界 |
| `commit_tick` | 提交时刻：支付消耗、进入冷却/GCD 的时间点；必须 ≤ `windup_ticks`（表达式时 ≤ `windup_ticks_min`） |
| `recovery_ticks`（或 `recovery_ticks_expression` + `_min`/`_max`） | 后摇；表达式在恢复阶段开始时采样，能看到执行后的状态 |
| `movement` / `turning` | 窗口期移动/转向策略（如 `locked` / `allowed`） |
| `interrupt_tags` | 携带这些标签的事件会打断施法 |
| `refund_before_commit` | commit 前被取消/打断时退还消耗 |

### 表达式与量纲要点

- 表达式是封闭算子集：`add`/`sub`/`mul`/`div`/`min`/`max`/`clamp`/`scale_bp`、比较 `eq`/`ne`/`lt`/`lte`/`gt`/`gte`、布尔 `and`/`or`/`not`、可选值守卫 `exists`（见 [compile_typecheck.go](skill/compile_typecheck.go)）。
- **每个数值都有量纲**（ticks、world distance、basis points、combat amount、资源量……见 [quantity.go](skill/quantity.go)）。量纲不匹配是编译错误：windup 表达式必须产出 ticks，`scale_bp` 的第二个参数必须是 basis_points。
- 引用词表：`$caster`、`$input.target`、`$memory.<name>`、`$local.<result>.<field>` 等；`read_attribute` 读宿主属性目录中的条目（可指定 `snapshot` 采样点）。
- 扩展属性/资源目录（例如加一个 `attack_haste_bp` 供攻速缩放前摇用）的做法见 [docs/skill-casting-and-combat.md](docs/skill-casting-and-combat.md) —— 记得 `environment.Digest = skill.AuthorityDigest(environment)` 重封摘要。

---

## 核心概念

### 四层边界，一个方向

```text
Skill JSON → Parse（严格 wire）→ Compile（IR + 静态证明）→ Program（不可变）
           → Runtime（确定性调度）→ Host（唯一允许读写世界的接口）
```

Runtime 从不回头解析 JSON，Host 之外没有任何世界写入路径，UI/组合系统只经由 `Inspect*` 只读视图消费 Program。

### 编译管线：每个 pass 证明一件事

[compile.go](skill/compile.go) 按固定顺序运行 18 个静态 pass，随后公开的 `Compile` 完成 lowering。后面的 pass 假定前面的 pass 已收窄名称、类型与作用域——它们不是可乱序的 lint 集合：

| Pass | 静态证明 |
| --- | --- |
| `normalize` | Wire → 封闭 IR，记录源路径（诊断可定位到 `$.phases[0]...`） |
| `shape` | Flow/Effect/Select/Process 结构合法 |
| `authority_capability` | 属性、资源、状态、伤害类型、元素等字符串解析到 `CompileEnvironment` 的权威 Handle；不在目录内即拒绝 |
| `gameplay_tags` / `input_state` / `temporal` | 标签类别、施放输入、Persistent/Shared State 与时间快照合法 |
| `type_snapshot` / `optional_quantity` / `effect_result_scope` | 值类型、量纲、快照采样点正确；可选值必须有 `exists` 守卫；effect result 只在其作用域内可读 |
| `graph` | phase/flow 图可达、无非法跳转 |
| `memory` | 每个 memory 读之前必然已初始化 |
| `lifetime_ownership` | 生命周期与拥有关系有界（不会有泄漏的召唤物/过程） |
| `motion` | 过程运动参数在能力目录允许的范围内 |
| `event_proc` | 被动触发的递归深度、同根事件保护有界 |
| `identity_random` | 每个随机点（random site）静态编号，身份稳定 |
| `budget` | **最坏情形**执行预算（操作数、任务数）有上界 |
| `visual` | 表现资产只引用目录允许的类别/主题/元素 |
| `lower` | 确认全部证明完成，进入 Program lowering |

产出的 `Program` 是只按索引执行的不可变计划：名称已降为 Handle、MemoryIndex、OperationIndex。同一定义 + 同一环境 ⇒ 同一 gameplay digest（进入 checkpoint、同步协议与组合契约的身份校验）。

### Runtime 与 Host 的职责边界

Runtime 拥有：cast 生命周期（windup/commit/recovery、打断、退款）、互斥与 GCD、冷却/弹药/蓄力/引导/光环策略、调度器、memory/persistent state、随机、proc 账本、checkpoint。Host 拥有：实体、属性、位置、空间查询、伤害/治疗/状态落地、过程步进的世界事实。

**Host 并发契约四条**（[host.go](skill/host.go) 的接口文档是全文）：

1. 所有 Host 方法都在 Runtime 持锁时调用——严格串行，Host 无需自己做并发防护；
2. **不得重入 Runtime**（`Activate`/`Advance`/`Cancel`/`StateDeltas`……锁不可重入，重入即死锁）；世界对技能效果的反应走 `Events`，由 Runtime 在确定性时点轮询；
3. **不得阻塞**（channel、他人持有的锁、无界 I/O）——一次阻塞停摆整个 Runtime 上所有施法者；
4. 结果必须是"给定 revision 下世界状态"的**确定性函数**——墙钟、map 遍历序、goroutine 时序都不许影响结果，因为回放与 checkpoint 恢复会重发同样的调用并要求同样的答案。

World revision 是防线：查询/命令携带期望 revision，Host 拒绝失效读写（`ErrRevisionUnavailable`）。

### 确定性为什么能位一致回放

- **全 int64 定点数学**：无 float。热路径用 128-bit 中间积的无分配定点算术（[fixed_math.go](skill/fixed_math.go)），与 big.Int 参考实现 fuzz 验证位一致；三角函数是定点 CORDIC（[process_motion.go](skill/process_motion.go) 的 `motionSinCos`，毫度角 → 百万分度向量）。
- **随机是 HMAC 派生的纯函数**：`HMAC(matchSeed, digest, caster, castSequence)` 派生施法密钥，再按编译期编号的 random site + 调用序号求值（[runtime_random.go](skill/runtime_random.go)）——没有全局 RNG 状态可漂移。
- **调度稳定排序**：`Advance` 按 (dueTick, 稳定序) 消费任务；并行 flow 分支按声明顺序提交。
- **checkpoint**：`Runtime.Checkpoint()` 产出带版本 + SHA-256 的权威镜像；`RestoreRuntime` 严格校验 Host revision/authority、Program digest，全部匹配才恢复（[runtime_checkpoint.go](skill/runtime_checkpoint.go)）。仓库每个验收 fixture 都做 checkpoint 往返并比对快照字节。

### 状态同步三流（[skillsync](skillsync)）

| Topic | 内容 | 可靠性 | 恢复 |
| --- | --- | --- | --- |
| `roost.skill.manifest` | Program 的 `PresentationPlan`（视觉表与挂载计划），按 PresentationDigest 每身份一次 | 必须可靠 | 重发 Full |
| `roost.skill.state` | `RuntimeStateSnapshot` 全量 / `StateMutation` 增量 | 必须可靠 | 历史连续则 replay，否则 Full |
| `roost.skill.presentation` | cast/effect 播放指令 | 可丢弃、需有序 | 短历史 replay；过期后以 state 为准 |

玩法修改先由 Host 成功提交，才产生 effect 表现事件；cast 表现只在 commit 后产生——**客户端永远不会看到一个被权威层拒绝的效果**。服务端 `Coordinator` 做可见性过滤（封闭字段集、default-deny 可选）与 durable outbox，客户端 `Applier` 做 epoch/schema/sequence/manifest 校验后事务应用。完整发布/恢复流程见 [docs/architecture-and-migration.md](docs/architecture-and-migration.md) 与 [docs/visual-sync-production-guide.md](docs/visual-sync-production-guide.md)。

---

## 关键实现细节

给想读源码的人五个切口，每条都标了主文件：

1. **写点增量突变 + 影子校验**（[runtime_mutation.go](skill/runtime_mutation.go)）。每个公开写入口都包在 `beginStateMutationLocked` / `commitStateMutationsLocked` 临界区里：写路径直接在"写点"登记脏对象，提交时只对脏集合生成 canonical `StateMutation`——不再全量 diff 快照，单次提交约百余纳秒（[runtime_mutation_benchmark_test.go](skill/runtime_mutation_benchmark_test.go)）。快路径的正确性由影子校验守住：测试开启 `stateMutationVerifyIncremental` 后，每次增量提交都与参考实现（全量快照 diff）比对，发散即 panic。把 mutation 序列折叠到旧快照上必须精确复原新快照——这是同步协议增量流的根基。
2. **HMAC 位点随机与 Host 返回顺序无关**（[runtime_random.go](skill/runtime_random.go)、[runtime_select.go](skill/runtime_select.go)）。随机选择不是"从 Host 给的列表里 roll 一个下标"，而是给**每个候选**算 `HMAC(castKey, randomSite, invocation, stableID)` 分数后按分数排序取样。stableID 来自候选自身（实体 ID 等），所以 Host 用什么顺序返回候选、宿主内部用不用 map，都不影响选中谁——确定性不依赖宿主实现细节。
3. **`"$gcd"` 哨兵**（[runtime_cast_window.go](skill/runtime_cast_window.go)，`globalCooldownProgramID`）。全局冷却不是一套并行机制，而是以保留程序 id `"$gcd"` 存在的一条**普通冷却条目**：`StateSnapshot().Cooldowns`、增量 mutation、checkpoint、同步协议全部免费复用现有冷却通道，客户端按普通冷却渲染即可。从 commit tick 起算、多次提交取最晚到期。
4. **表达式钳制保住最坏情形预算**（[wire_cast_window.go](skill/wire_cast_window.go)、[runtime_cast_window.go](skill/runtime_cast_window.go)）。窗口表达式必须声明 `min`/`max`，运行时结果无条件钳入边界。于是编译期以边界做的所有证明在运行时恒成立：budget pass 用 `max` 算最坏时长，`commit_tick <= windup_ticks_min` 保证"先提交后执行"的不变量——表达式再怎么算也破坏不了静态结论。这是"动态数值"与"静态证明"共存的通用手法。
5. **combat 是单一数学源，MemoryHost 收敛于它**（[combat/damage.go](combat/damage.go)、[skill/memory_host_combat.go](skill/memory_host_combat.go)、[combatcomponent/adapter.go](combatcomponent/adapter.go)）。twelve_stage_v1 十二段管线（`target_validity → immunity → avoidance → damage_type → penetration → element → modifiers → critical → caps → shield → health → aftermath`）只实现一次：参考宿主 `MemoryHost` 与生产集成 `combatcomponent.HostAdapter` 跑的是同一份代码、发同一套事件词表（`damage_resolved`、`combat_hook_*`、`shield_absorbed`……），proc 过滤器在两种宿主上行为一致。status 域命令（Status/Remove/Dispel/AttributeModifier）由 `combatcomponent.StatusBridge` 按 status catalog 落到 buff 容器（唯一的有意差异：mul_bp 修饰加性叠加而非 MemoryHost 的乘性链，见 [docs/skill-casting-and-combat.md](docs/skill-casting-and-combat.md)）。用 MemoryHost 验证过的数值结论直接适用于生产。

---

## 学习路径

**第一轮（30 分钟）——一次最小施放**：[testdata/simple_damage.json](skill/testdata/simple_damage.json) → [parse.go](skill/parse.go) 的 `Parse` → [lower.go](skill/lower.go) 的 `Compile` → [runtime.go](skill/runtime.go) 的 `Activate` → [executor.go](skill/executor.go) → [memory_host_effect.go](skill/memory_host_effect.go) → [acceptance_test.go](skill/acceptance_test.go)。

**第二轮（1 小时）——编译器如何拒绝不安全定义**：[compile.go](skill/compile.go) 的 pass 目录 + 各 `compile_*_test.go` 的拒绝用例 + [compile_environment.go](skill/compile_environment.go) 的默认目录。练习：把 fixture 的 `damage_type` 改成不存在的键，观察诊断落在 authority 边界而非运行时。

**第三轮（40 分钟）——IR 到 Program**：`wire_*.go` → `ir_*.go` / [compile_normalize.go](skill/compile_normalize.go) → [lower.go](skill/lower.go) → `program_*.go` → [inspect.go](skill/inspect.go)。

**之后按能力选切口**（fixture 即可运行示例）：施法窗口/策略看 `toggle_aura`/`hold_beam`/`charge_projectile`/`ammo_burst`/`cast_window_interrupt` 配 [runtime_cast_policy.go](skill/runtime_cast_policy.go)、[runtime_cast_window.go](skill/runtime_cast_window.go)；弹道/运动看 `tracking_boomerang`/`path_projectile`/`carry_dash` 配 [process_motion.go](skill/process_motion.go)；召唤物看 `owned_trap`/`owned_pet_command`；被动 proc 看 `passive_counter`/`passive_proc_guard` 配 [runtime_proc.go](skill/runtime_proc.go)。完整对照表在 [docs/skill-implementation-guide.md](docs/skill-implementation-guide.md)（含 Visual/Sync 深入路线与实验清单），日常测试清单在 [docs/skill-testing-guide.md](docs/skill-testing-guide.md)。

验证一切正常的最短命令：

```bash
go test ./skill -run TestAllFixturesParseCompileInspectAndRun -count=1
go test ./... -count=1
```

### 迁移与版本

- **compiler-2 语义修订（v1.4 → v1.5）**：`concurrent`、`global_cooldown_ticks`、窗口表达式进入 gameplay digest，旧 checkpoint/回放记录/skillcompose 契约在新版本下会得到明确解析错误。迁移动作（全量重编译、排空旧 checkpoint、重签契约）见 [docs/skill-casting-and-combat.md](docs/skill-casting-and-combat.md) 的迁移说明。
- **旧 `/skillv2` → 稳定 `/skill` 的源码升级**：[docs/breaking-upgrade-skill-package.md](docs/breaking-upgrade-skill-package.md)。wire v2 与 compiler semantics 保持不变。
- **生产部署与发布门槛**：[docs/production-readiness.md](docs/production-readiness.md)。

### 与 roost-core / roost-kit 的关系

依赖方向固定，任何反向依赖（指向具体游戏、渲染器、网络实现）都是边界违规：

```text
roost-core/syncstream            （通用可靠流：Observer/Packet/序号/ACK/恢复）
        ^
        |
roost-skill/skillsync           roost-kit/syncstream（Packet ↔ NATS/JetStream 编码）
        ^
        |
roost-skill/skill  ←  roost-skill/combat（零依赖，可单独使用）
        ^
        |
     game host（实现 skill.Host；combatcomponent 提供 roost-core 实体侧的现成接法）
```

`combatcomponent` 依赖 `roost-core`（`entity`/`checkpoint`/`nest`），把 combat 状态做成带脏跟踪、可持久化、handler 回滚后字节一致的实体组件；`skill` 与 `combat` 本身不依赖 roost-core 的运行时设施。

## License

见 [LICENSE](LICENSE)。
