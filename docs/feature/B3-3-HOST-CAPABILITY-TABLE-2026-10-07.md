# B3 ③：Host 取值能力表随编译环境下发

2026-10-07，分支 `b3cap`，基线 `dac4f38c`。来由：[DECISIONS-PENDING B3 ③](../review/DECISIONS-PENDING-2026-10-05.md)（第二轮定为“下个大版本”，第十三轮维护者要求本版完成）；①② 已由 `023eb276` 实施（[方案](B3-SKILL-LOWER-FAILFAST-2026-10-06.md)），④ 保持方向 B。相关 RR：NC-110～117、NC-150～154、NC-210～216（NC-151 / 213 / 214 / 215 是“编译器放行、执行侧不支持”的直接先例）。

## 1. 问题

编译器、Runtime、Host 各自维护一份“能读什么、支持什么”的集合。编译器按 catalog 和自己写死的集合放行，Runtime 按 Program 执行，Host 按自己的实现作答，三者之间没有共同来源：编译时认为能读的属性、资源、运动步骤，到了运行期 Host 可能不支持，结果是施法到一半才报错（往往已经扣了费），或者 Host 静默返回 0。

## 2. 现状清单（基线 `dac4f38c`，rg + 源码逐项核对）

codebase-memory 的共享 generation 停在 09-30，下表以当前源码为准（`rg -n` 结果逐条读过）。

### 2.1 编译器侧

| 集合 | 位置 | 说明 |
| --- | --- | --- |
| motion 槽位 `EnabledSlots`（frame / steering / offsets / collision / carry / completion） | `compile_environment.go` `MotionCapabilityCatalog.EnabledSlots`；`compile_motion.go` `motionSlotEnabled` 6 处 | **steering / offsets 只在定义写了时才查**，与 Runtime 每步都发这两种步骤不一致（§7，RR-20261006-37） |
| Host 特性 `HostFeatures`（只有 `carry`） | `compile_environment.go`；`compile_motion.go` `motionHostFeatureEnabled`；`compile_authority.go` 封闭集合 `feature != "carry"` | 与 `EnabledSlots` 的 carry 重复 |
| 衍生物 kind（dash / orbit / projectile / area / beam / minion） | `compile_motion.go` `validMotionSpawnKind` | 语言封闭集合；没有“Host 能不能承载 minion”的表达 |
| 资源 operation（set / add / spend / sub） | `compile_shape.go:206` | 写死 |
| 属性修正 operation（add / mul_bp） | `compile_capability.go:144` 写死 + `AttributeCatalogEntry.ModifierOperations` | 写死部分与 Host 各自一份 |
| 可读属性 | `AttributeCatalogEntry.Readable` | **没有读取方**（`rg '\.Readable'` 只有声明），任何 catalog 属性都能 `read_attribute` |
| 资源 | `GameplayCatalog.Resources` | 编译期按 catalog 查（NC-214） |
| 衍生物数值属性 `spawn_properties`（含 `spawn_kinds`） | `compile_environment.go` `defaultSpawnPropertyCatalog`、`compile_authority.go` 要求与 canonical 逐字相同、`lower.go:161` | 九个属性里 `turn_rate_mdeg_per_tick` / `return_speed_bp` / `collision_force` Runtime 不用来算运动（`spawnNumericBinding` 对它们返回 nil 基础值），只经 `SpawnNumericSnapshot` 交给 Host |

### 2.2 Runtime 侧

| 集合 | 位置 | 说明 |
| --- | --- | --- |
| 召唤物需要 `OwnedEntityRuntimeHost` | `runtime_owned_entity.go:6`、`spawn_owned.go:213/265/342/402/521/557` 类型断言 | 断言失败返回 `ErrHostContractViolation`，发生在施法中途、扣费之后 |
| motion 步骤 | `spawn_motion.go` `stepSpawnMotion`：每步发 Frame / Steering / Trajectory / Offsets / Completion / Signals，collision / carry 写了才发；`spawn_motion.go:23/32` Static / Signals | Steering、Offsets 不论定义写没写都发 |
| 只交给 Host 的数值字段 | `spawn_numeric.go` `snapshotSpawnNumeric` | Host 不读就静默不生效 |
| 属性 / 资源读取 | `runtime_eval.go:41/93`（AttributeRead）、`PayCosts` | Runtime 不发 `ResourceRead`（只有 replay） |

### 2.3 Host 侧

| 集合 | 位置 | 说明 |
| --- | --- | --- |
| MemoryHost 属性读取 | `memory_host.go` `Read` AttributeRead | 任意 handle 按实体 map 作答，catalog 外返回 0 |
| MemoryHost 资源读取 / 付费 | `memory_host.go` `Read` ResourceRead、`PayCosts` | 任意名字，catalog 外读出 0、付零费成功 |
| MemoryHost 资源 / 修正 operation | `memory_host_effect.go:99-110`、`memory_host_status.go:185` | 与编译器各写一份 |
| MemoryHost motion 步骤 | `memory_host_motion.go` | 全部接受 |
| HostAdapter 属性读取 | `combatcomponent/adapter.go` `Read` | `AttributeCurrent` 对没有的通道返回 0 |
| HostAdapter 资源 | `ResourceAttribute` 映射 | 没映射的 catalog 资源到付费时才报 “has no attribute mapping” |
| HostAdapter / StatusBridge 修正 operation | `combatcomponent/status_bridge.go:460` | 没接 StatusBridge 时修正交给业务 Host |

## 3. 能力表的形状

一张表，八列（`skill/host_capability.go` 的 `hostCapabilityColumns` 是唯一登记处，Has / Items / 校验 / 合并 / 守卫都遍历它）：

| 列 | 取值 | 来源 | 编译期需求 | Host 侧含义 |
| --- | --- | --- | --- | --- |
| `attribute` | 属性 key | Gameplay catalog 里 `Readable` 的属性 | `read_attribute`（含快照、cast window 表达式）、`attribute_compare` 过滤 | `Read(AttributeRead)` 作答，表外拒绝 |
| `resource` | 资源 key | Gameplay catalog 全部资源 | cost、sustain cost、resource 效果 | `Read(ResourceRead)` / `PayCosts` / `ResourceCommand`，表外拒绝 |
| `spawn_kind` | dash / orbit / projectile / area / beam / minion | `environment.Host.SpawnKinds` | 每个衍生物的 kind | minion 需要召唤物 |
| `motion_step` | frame / steering / offsets / collision / carry / completion | `environment.Host.MotionSteps` | 运动衍生物：frame、steering、offsets、completion 每个都要（Runtime 每步都发）；collision / carry 写了才要 | `StepSpawn` 接受该步骤 |
| `spawn_numeric_field` | turn_rate_mdeg_per_tick / return_speed_bp / collision_force | `environment.Host.SpawnNumericFields` | `numeric_tracks` / `modify_spawn` 改这三个属性 | Host 读 `SpawnNumericSnapshot` 的该字段 |
| `resource_operation` | set / add / spend / sub | `environment.Host.ResourceOperations` | resource 效果的 operation | `Apply(ResourceCommand)` |
| `modifier_operation` | add / mul_bp | `environment.Host.ModifierOperations` | attribute_modifier 的 operation | `Apply(AttributeModifierCommand)` |
| `summon` | 布尔 | `environment.Host.Summon` | summon 效果、召唤物命令、`owned_entities` 选择 | 实现 `OwnedEntityRuntimeHost` |

属性与资源两列不另抄一份：catalog 本来就是“世界里有哪些属性 / 资源”的权威，`Readable` 原来没有读取方，现在就是“Host 能答这个属性的读取”。这样业务给 catalog 加一个可读属性，不需要再去能力表里登记第二遍（既有用例 `TestExternallyAuthoredHasteAttributeDrivesWindup` 就是这样做的）。

类型：

- `skill.HostCapabilityCatalog`：环境里业务声明的部分（`Revision`、`SpawnKinds`、`MotionSteps`、`SpawnNumericFields`、`ResourceOperations`、`ModifierOperations`、`Summon`）。
- `skill.HostCapabilityTable`：完整的表（`Attributes`、`Resources` + 内嵌 `HostCapabilityCatalog`）。`HostCapabilityTableOf(environment)` 拼出环境的表；Host 用 `HostCapabilityProvider.HostCapabilities()` 声明自己的表。
- `skill.HostCapability{Kind, Key}`：表的一项，`String()` 形如 `motion_step "collision"`、`summon`，诊断与错误都用它点名。

## 4. 环境格式与 authority digest

- `CompileEnvironment` 新增 `Host HostCapabilityCatalog`；`DefaultCompileEnvironment()` 用 `FullHostCapabilityCatalog()`（每列封闭集合的全部取值 + Summon，revision `host-1`）。
- `MotionCapabilityCatalog` 删除 `EnabledSlots`、`HostFeatures`，由 `Host.MotionSteps` 取代（同一事实只留一处）。
- authority digest 的输入新增 `Host` 字段，并去掉了两个被删的 motion 字段：**所有环境的 authority digest 都变了**（默认环境也是），revision 字符串（`gameplay-default-1`）不变。线上没有部署，不做旧格式兼容：用旧 digest 的 Program / checkpoint / skillcompose 契约与新 Host 的 `AuthorityIdentity` 不匹配，需重新编译、重签。仓内没有存放固定的 digest 值（`rg '[0-9a-f]{64}'` 在 skill、codegen、demo 里只有镜像 sha 占位），testdata 是技能定义 JSON、不含 digest，skillcompose 契约都在运行时由 `BuildContract` 计算，因此不需要改任何 golden 文件。gameplay / presentation digest（Program 自身的）不变：`hostRequirements` 由定义与环境推出，不进 digest。
- 新的环境校验（`CATALOG_HOST_POLICY_INVALID`）：每列取值唯一、在封闭集合里；声明了 minion 就必须声明召唤物；revision 必填。

## 5. 三处怎么共用

```text
CompileEnvironment.Host + Gameplay catalog
        │  HostCapabilityTableOf（编译器唯一入口）
        ▼
编译器：collectHostRequirements(IR) ──逐项 Has──▶ 表外报 HOST_CAPABILITY_MISSING（点名）
        │  排序去重
        ▼
Program.hostRequirements
        │  第一次 Start / RegisterAbility / ActivatePassive / RestoreRuntime
        ▼
Runtime：host.(HostCapabilityProvider).HostCapabilities().Missing(requirements) → ErrHostCapabilityMissing
        ▲
Host：MemoryHost / HostAdapter 声明表；表外的属性 / 资源报 ErrHostCapabilityMissing；
      CheckHostCapabilities 按声明逐项调用 Host 核对
```

- **编译器**（`skill/compile_host_capability.go`，在 `authority_capability` pass 末尾运行，不新增 pass 名）：遍历 IR 收集需求（带第一次用到的源路径），对照 `HostCapabilityTableOf(environment)`；原来写在 `compile_motion.go` 的槽位 / Host 特性检查删掉。资源 / 修正 operation 的语言封闭集合检查（`SHAPE_INVALID`）保留——那是 DSL 本身的取值范围，Host 表是它的子集。
- **Runtime**（`skill/runtime_host_capability.go`）：Program 第一次在这个 Runtime 上用时核对，通过的记在 `hostAdmitted`，之后不重复核对；`ErrHostCapabilityMissing` 同时 `errors.Is(ErrHostContractViolation)`。checkpoint 恢复经 `hostCheckedResolver` 同样核对（失败为 `ErrCheckpointProgram`，消息点名缺的项）。Host 没实现 `HostCapabilityProvider` 时不做核对（`RecordingHost` / `ReplayHost` 等调试替身；既有用例 `TestOwnedEntityRuntimeFailsClosedWithoutOwnedHostContract` 依赖这一点原样通过）。
- **Host**：
  - `MemoryHost.HostCapabilities()`：配置的 catalog（未配置时按默认 catalog）推出属性 / 资源两列，其余列全部实现。配置了 catalog 之后，`Read(AttributeRead)` / `Select` 的属性过滤对不可读或 catalog 外的 handle、`Read(ResourceRead)` / `PayCosts` 对 catalog 外的资源名返回 `ErrHostCapabilityMissing`（原来静默当 0）。未配置 catalog 的 MemoryHost 维持按实体数据作答（测试便利，`CheckHostCapabilities` 会指出它答了表外的属性）。
  - `combatcomponent.HostAdapter.HostCapabilities()`：Catalog 里可读的属性、有 `ResourceAttribute` 映射的资源、四种资源 operation；接了 `StatusBridge` 时再加 add / mul_bp。运动、衍生物、召唤物归业务 Host。`Read(AttributeRead)` 对 Catalog 里不可读或不存在的 handle 返回 `ErrHostCapabilityMissing`（原来读出 0）。
  - `skill.CheckHostCapabilities(host, catalog, probe)`：对 Host 声明的每一项调用 Host（读属性并核对量纲、读资源与付零费、施加零值资源变化与 1 tick 的零值修正、按步骤调 `StepSpawn`、断言 `OwnedEntityRuntimeHost`），属性 / 资源两列再核对表外的 key 被拒绝。检查有副作用（探针衍生物、零值修正），在测试世界上跑。
  - `skill.HostSupportsEnvironment(host, environment)`：启动时核对 Host 的表覆盖环境的表，返回缺的每一项。

## 6. 业务方要写的代码

最少两处：

```go
// 1) Host 声明能力：战斗部分交给 HostAdapter，自己负责的部分（运动、衍生物、召唤物）列出来合并。
func (h *GameHost) HostCapabilities() skill.HostCapabilityTable {
	own := skill.HostCapabilityTable{HostCapabilityCatalog: skill.HostCapabilityCatalog{
		Revision: "game-host-1", SpawnKinds: []string{"projectile", "area"},
		MotionSteps: []string{"frame", "steering", "offsets", "completion", "collision"},
	}}
	return skill.MergeHostCapabilities(h.combat.HostCapabilities(), own)
}

// 2) 装配时：环境的 Host 段取自 Host 的表（或手写后用 HostSupportsEnvironment 核对），再编译。
environment.Host = host.HostCapabilities().HostCapabilityCatalog
environment.Digest = skill.AuthorityDigest(environment)
```

测试里加一行 `skill.CheckHostCapabilities(host, environment.Gameplay, skill.HostCapabilityProbe{Entity: probe})` 核对声明与行为一致。用 `MemoryHost` 或默认环境的项目不需要改代码（默认环境声明全部能力，MemoryHost 全部实现）。

## 7. 先红后绿

### 7.1 修前（基线 `dac4f38c`，旧 API 的临时用例，不留仓库）

```text
summon 写在不实现 OwnedEntityRuntimeHost 的 Host 上：
  compile errors=false; Activate err=skill: host contract violation; caster mana after=90 (was 100)
带 collision 的衍生物跑在不接受碰撞步骤的 Host 上：
  compile errors=false; Activate err=host: collision motion is not supported; cast status=failed; caster mana after=90 (was 100)
enabled_slots 去掉 steering、定义不写 steering（RR-20261006-37）：
  compile errors=false; Activate err=host: steering motion is not supported; cast status=failed; caster mana after=90 (was 100)
MemoryHost（配置了默认 catalog）：
  attribute handle 99 (not in catalog): value=0 err=<nil>; resource "rage" (not in catalog): value=0 err=<nil>; PayCosts rage 0 err=<nil>
HostAdapter：
  HostAdapter AttributeRead handle 99 (not in catalog): handled=true value=0 err=<nil>
```

### 7.2 修后（`skill/host_capability_promises_test.go`、`skill/combatcomponent/host_capability_promises_test.go`）

- `TestHostCapabilityMissingIsRejectedAtCompileTime`：summon / collision / 不写 steering 的运动 / attribute_modifier 四种，环境取自 Host 声明的表时编译报 `HOST_CAPABILITY_MISSING`，路径与消息逐字核对（如 `environment host capability table lacks motion_step "collision"` at `$.phases[0].on.enter.steps[0].spawn.motion.collision`）；同一定义在默认环境照常编译。
- `TestRuntimeRefusesProgramsOutsideTheHostTable`：默认环境编译的 Program 交给能力不够的 Host，`Activate` / `RegisterAbility` 返回 `ErrHostCapabilityMissing` 并点名，mana 仍是 100（没扣费）。`TestRestoreRefusesProgramsOutsideTheHostTable`：checkpoint 恢复到没有召唤物的 Host 上被拒。
- `TestMemoryHostCapabilitiesMatchItsBehavior`、`TestBusinessHostOverHostAdapterMatchesItsCapabilities`：两个参考实现声明与行为一致、覆盖默认环境。
- `TestHostAdapterRefusesAttributesOutsideTheCatalog`、`TestHostAdapterUnmappedResourceIsNamedAtStartup`、`TestHostAdapterWithoutStatusBridgeDeclaresNoModifiers`：HostAdapter 的表外读取报错、未映射资源与缺修正在启动核对时点名。

### 7.3 守卫

| 守卫 | 承诺 |
| --- | --- |
| `TestCheckHostCapabilitiesCatchesMisdeclaredHosts` | 声明了却不支持的 Host（碰撞、修正、召唤物、多声明资源、对表外属性答 0）都被 `CheckHostCapabilities` 点名 |
| `TestCompilerConsultsTheTableForEveryRequirement` | 全部变异种子 × 默认表每一项（资源列除外，它就是 catalog）：去掉这一项后，项在需求里 ⇒ 只报点名它的 `HOST_CAPABILITY_MISSING`；不在 ⇒ 照常编译、需求不变（1200 次） |
| `TestRuntimeAsksHostOnlyForCompiledRequirements` | 全部种子在记录型 Host 上跑：Runtime 发给 Host 的每次取值（属性读 / 过滤、资源、operation、motion 步骤、非零的 Host 数值字段、召唤物调用）都在 Program 的需求里 |
| `TestCompilerReadsHostCapabilitiesOnlyThroughTheTable` | 编译器源文件（`compile_*`、`lower*`）不直接读 `environment.Host`、`Readable`、`HostFeatures` / `EnabledSlots`（环境自身的 digest 与校验两行除外） |
| `TestHostOnlySpawnNumericFieldsMatchRuntime` | `hostOnlySpawnNumericFields` 与 Runtime 实际用法一致（`spawnNumericBinding` 无基础值 ⇔ 只给 Host） |
| `TestEnvironmentHostCapabilityCatalogIsValidatedAndDigested` | Host 段的封闭集合、minion ⇒ summon、revision 必填；改 Host 段改变 digest |

### 7.4 变异证明守卫能红（临时改源码，跑后还原）

| 变异 | 结果 |
| --- | --- |
| steering 需求改回“写了才要” | `TestHostCapabilityMissingIsRejectedAtCompileTime` 红（`diagnostics = []`）；`TestRuntimeAsksHostOnlyForCompiledRequirements` 红：`runtime asked the host for motion_step "steering" via StepSpawn, but the compiled requirements [spawn_kind "beam" motion_step "completion" motion_step "frame" motion_step "offsets" summon] do not list it` |
| 删掉 collision 需求 | `runtime asked the host for motion_step "collision" via StepSpawn, but the compiled requirements [...] do not list it` |
| `compile_motion.go` 直接读 `context.environment.Host.MotionSteps` | `compile_motion.go:174 reads host capability data outside HostCapabilityTableOf` |
| MemoryHost 对表外属性照常作答 | `host capability attribute "<handle 4>": outside the catalog but Read(AttributeRead) answered ... instead of refusing` |
| 去掉 Runtime 准入核对 | `Activate err = skill: host contract violation, want ErrHostCapabilityMissing naming summon`（另三例同样红，错误来自施法中途） |
| `hostOnlySpawnNumericFields` 漏掉 collision_force | `spawn property "collision_force": runtime treats it as host-only=true, hostOnlySpawnNumericFields says false` |
| HostAdapter 不查 Catalog | `attribute "<handle 4>": outside the catalog but ... answered`；`AttributeRead handle 99: handled=true value=0 err=<nil>, want ErrHostCapabilityMissing` |
| 一致性检查不探表外 handle | 仍红（未配置 catalog 的 MemoryHost 在“声明的属性量纲不对”上被点名），说明该变异例另有一道检查兜住 |

## 8. 实施中发现的缺陷

- [RR-20261006-37](../bug/RR-20261006-37.md)（P3）：motion catalog 的 `enabled_slots` 只在定义写了 steering / offsets 时才查，Runtime 却对每个运动衍生物每步都发这两种步骤——关掉槽位的环境照样编译出运动衍生物，施法时扣费之后才失败。由 `TestRuntimeAsksHostOnlyForCompiledRequirements` 第一次运行发现；修法见 §3 的 motion_step 行（随本项的格式变化一起修，槽位并入 Host 表）。

## 9. 兼容与影响

- **破坏性**：环境格式变化（新增 `Host`、删 `Motion.EnabledSlots` / `HostFeatures`）、所有 authority digest 变化；配置了 catalog 的 MemoryHost 与 HostAdapter 对表外属性 / 资源从“返回 0”改为报错；环境收窄后原来能编译的定义报 `HOST_CAPABILITY_MISSING`；Host 声明了能力表时，Program 需要表外能力会在启动 / 注册 / 恢复时被拒（原来是施法中途）。
- 不变：默认环境下全部既有定义的编译结果、gameplay / presentation digest、Runtime 行为；既有 skill 测试全部原样通过（含 pass 顺序守卫、lower 逐表删条目、变异性质测试）。
- 生成工程：codegen 模板只用 `skill.DefaultCompileEnvironment()` 编译技能，形状不变；重新生成 game-demo（replace 到本 worktree）build / vet / test 通过（§10）。
- 发版文档：不改 `docs/release/v1.23.0/*`。受影响条目见交接报告（SKILL 相关条目里描述环境字段、Host 契约与 digest 的几行）。

## 10. 验证（`GOWORK=off`）

- `gofmt -l` 空；`go vet ./skill/...`；`go test -race -count=3 ./skill/...`（skill、combat、combatcomponent、skillcompose、skillsync）通过。
- fuzz：`FuzzParseGeneratedNeverPanics`、`FuzzRestoreRuntimeCheckpointNeverPanics` 各 25s 通过。
- `skill/examples` 三个示例 `go run` 退出 0；`skill/integration/sync-e2e` `go test` 通过。
- 根包 `go test -count=1 .`（含 `TestExamplesRun`）通过；`go build ./... && go vet ./...` 通过；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` rc=0。
- `go test -count=1 ./codegen/...` 通过（15 个包）；`roost project new ... -template game-demo` 生成工程（replace 到本 worktree，`go mod tidy`）`go build ./... && go vet ./... && go test ./...` 通过。生成形状未变，未跑 `go generate` porcelain 检查。

## 11. 实施状态

已实施（`cd8ed341`，未发版；登记 `13f032a3`）。未做：Host 数值字段（`spawn_numeric_field`）只能核对 `StepSpawn` 接受该字段，Host 是否真的使用它无法从接口上观察，声明即承诺；非 minion 的衍生物 kind 同理（`StepSpawn` 不带 kind），只核对基本步骤。
