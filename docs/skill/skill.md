# 稳定 Skill API 与最小接入

阅读准备：先了解 [Skill 的职责与例子](../framework/guide/08-skill.md)；不熟悉DAO、事务、Host等概念时，从 [入门手册](../GETTING-STARTED.md) 开始。本篇保留具体接口与示例，适合在接入或定位问题时查阅。

技能系统（roost-core/skill）对业务暴露唯一稳定核心包：

```go
import "github.com/tjbdwanghaibo/roost-core/skill"
```

不再提供 `/skillv2` Go 包或兼容别名。Go API 名称与持久协议版本解耦：

| 身份 | 当前值 | 用途 |
| --- | --- | --- |
| Go import | `github.com/tjbdwanghaibo/roost-core/skill` | 业务编译期依赖，保持稳定 |
| JSON schema | `roost.skill/v2` | 技能定义 wire 格式，必须写入定义 |
| compiler semantics | `skillv2-compiler-2` | gameplay digest、checkpoint、回放和契约校验 |
| module | `github.com/tjbdwanghaibo/roost-core` | Go module，沿 v1.x tag 发布 |

不要根据 import path 推断 schema，也不要自行改写 compiler semantics。

## 一个完整的技能定义

下面定义一个需要短暂准备的火球：提交后消耗法力，对目标造成伤害，随后结束施法。`activation`描述如何开始，`input_schema`说明输入是实体目标，`phases`描述执行步骤。`tick`是离散逻辑步，不直接等同于毫秒；实际频率由游戏推进方式决定。

这段JSON来自可运行火球示例，并由文档测试使用实际Parse/Compile校验。可以先照着运行，再逐项调整参数。

```json
{
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
}
```

## 最小工作流

```go
definition, err := skill.Parse(rawJSON)
if err != nil {
    return err
}

environment := skill.DefaultCompileEnvironment()
program, diagnostics := skill.Compile(definition, environment)
for _, diagnostic := range diagnostics {
    if diagnostic.Severity == skill.DiagnosticError {
        return fmt.Errorf("compile %s: %s", diagnostic.Path, diagnostic.Message)
    }
}
if program == nil {
    return errors.New("skill compile failed")
}

host := newGameHost(environment) // 生产项目实现 skill.Host
runtime := skill.NewRuntime(host, skill.RuntimeOptions{MatchSeed: matchSeed})
_, err = runtime.Activate(program, skill.CastInput{Caster: caster, Target: target})
```

完整可运行版本位于 [skill/examples/fireball](../../skill/examples/fireball/main.go)。`MemoryHost` 只适合示例、编译验收和
确定性参考测试；生产游戏应实现 `Host`，并让世界查询、效果提交、revision 和权限判断
全部经过该边界。

## 必须理解的四个对象

- `Definition`：严格解析后的 wire 定义，不是运行时对象。
- `CompileEnvironment`：属性、资源、状态、伤害、Visual 等权威目录与容量上限。
- `Program`：经过静态证明和 lowering 的不可变执行计划，可缓存并按 digest 标识。
- `Runtime`：确定性调度器；持有 cast、cooldown、spawn、state、checkpoint 与事件游标。

## Host 生产契约

Runtime 持锁调用 Host，因此 Host 必须：

1. 不重入同一个 Runtime；
2. 不执行无界 I/O、channel 等待或可能长期阻塞的跨实体锁；
3. 给定相同 revision 和请求时返回相同结果；
4. 对查询和提交执行 authority/revision 校验；
5. 把可预期的玩法失败编码为类型化结果，把基础设施失败作为 error 返回。

完整接口约束见 `skill/host.go` 和
[架构、迁移与同步流程](../framework/guide/08-skill.md)。

## Host 能力表

编译器、Runtime、Host 共用一张“Host 能读什么、支持什么”的表，随 `CompileEnvironment` 下发、算进 authority digest。
八列：可读属性（catalog 里 `Readable` 的属性）、资源（catalog 全部资源）、衍生物 kind、motion 步骤、只交给 Host 的
衍生物数值字段（`turn_rate_mdeg_per_tick` / `return_speed_bp` / `collision_force`）、资源 operation、属性修正
operation、召唤物（`OwnedEntityRuntimeHost`）。后六列写在 `environment.Host`（`skill.HostCapabilityCatalog`），
`skill.HostCapabilityTableOf(environment)` 拼出完整的表。

- **技能作者**：定义用到表外的取值时编译报 `HOST_CAPABILITY_MISSING`，消息点名缺的那一项（如
  `environment host capability table lacks motion_step "collision"`、`... lacks summon`）。这不是定义写错，而是
  这个项目的 Host 不支持它：换写法，或让 Host 实现后在环境里声明。默认环境声明全部能力。运动衍生物固定需要
  frame / steering / offsets / completion 四个步骤（Runtime 每步都发），collision / carry 写了才需要。
- **Host 实现**：`skill.Host` 接口包含 `HostCapabilities() skill.HostCapabilityTable`（`HostCapabilityProvider` 已并入，漏写编译不过），每个 Host 都声明自己的表，包装别的 Host 的类型转发被包装者的表；
  环境的 Host 段取自它（`environment.Host = host.HostCapabilities().HostCapabilityCatalog`，再
  `skill.AuthorityDigest` 重算摘要），或手写后在启动时用 `skill.HostSupportsEnvironment(host, environment)` 核对。
  Runtime 在 Program 第一次启动 / 注册 / 入队被动 / 从 checkpoint 恢复时核对它的需求都在 Host 的表里，缺了返回
  `skill.ErrHostCapabilityMissing`（也是 `ErrHostContractViolation`），不会到施法中途、扣费之后才失败。对表外的属性
  handle、资源名要返回错误，不能当成 0。测试里用 `skill.CheckHostCapabilities(host, environment.Gameplay, probe)`
  按声明逐项调用 Host 核对。细节与业务最少要写的代码见[方案](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md) §6。

## 数据与升级边界

- 技能 JSON 在进入目录前必须 `Parse` + `Compile`，禁止运行时解释未经验证的 JSON。
- `Program` 不应跨版本自行序列化；持久化源定义、环境 identity 和 gameplay digest。
- checkpoint 恢复必须匹配 Program resolver、Host authority、world revision 和 checksum。
- 客户端同步使用 `skillsync` 的 manifest/state/presentation 三流，不直接发送 Runtime 私有结构。
- 从 `/skillv2` 升级使用[稳定包迁移手册](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/skill/breaking-upgrade-skill-package.md)。

## 下一步

- 技能作者：[README 的完整火球示例](../../skill/README.md)
- Host 开发：[施法语义与战斗接入](skill-casting-and-combat.md)
- 同步开发：[Visual 与数据同步生产指南](visual-sync-production-guide.md)
- 框架维护：[实现学习手册](skill-implementation-guide.md)
- 发布人员：[生产门槛](../maintenance/KNOWN-LIMITS.md)
