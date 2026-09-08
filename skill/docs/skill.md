# 稳定 Skill API 与最小接入

roost-skill 对业务暴露唯一稳定核心包：

```go
import "github.com/tjbdwanghaibo/roost-skill/skill"
```

不再提供 `/skillv2` Go 包或兼容别名。Go API 名称与持久协议版本解耦：

| 身份 | 当前值 | 用途 |
| --- | --- | --- |
| Go import | `roost-skill/skill` | 业务编译期依赖，保持稳定 |
| JSON schema | `roost.skill/v2` | 技能定义 wire 格式，必须写入定义 |
| compiler semantics | `skillv2-compiler-2` | gameplay digest、checkpoint、回放和契约校验 |
| module | `github.com/tjbdwanghaibo/roost-skill` | Go module，沿 v1.x tag 发布 |

不要根据 import path 推断 schema，也不要自行改写 compiler semantics。

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

完整可运行版本位于 `examples/fireball`。`MemoryHost` 只适合示例、编译验收和
确定性参考测试；生产游戏应实现 `Host`，并让世界查询、效果提交、revision 和权限判断
全部经过该边界。

## 必须理解的四个对象

- `Definition`：严格解析后的 wire 定义，不是运行时对象。
- `CompileEnvironment`：属性、资源、状态、伤害、Visual 等权威目录与容量上限。
- `Program`：经过静态证明和 lowering 的不可变执行计划，可缓存并按 digest 标识。
- `Runtime`：确定性调度器；持有 cast、cooldown、process、state、checkpoint 与事件游标。

## Host 生产契约

Runtime 持锁调用 Host，因此 Host 必须：

1. 不重入同一个 Runtime；
2. 不执行无界 I/O、channel 等待或可能长期阻塞的跨实体锁；
3. 给定相同 revision 和请求时返回相同结果；
4. 对查询和提交执行 authority/revision 校验；
5. 把可预期的玩法失败编码为类型化结果，把基础设施失败作为 error 返回。

完整接口约束见 `skill/host.go` 和
[架构、迁移与同步流程](architecture-and-migration.md)。

## 数据与升级边界

- 技能 JSON 在进入目录前必须 `Parse` + `Compile`，禁止运行时解释未经验证的 JSON。
- `Program` 不应跨版本自行序列化；持久化源定义、环境 identity 和 gameplay digest。
- checkpoint 恢复必须匹配 Program resolver、Host authority、world revision 和 checksum。
- 客户端同步使用 `skillsync` 的 manifest/state/presentation 三流，不直接发送 Runtime 私有结构。
- 从 `/skillv2` 升级使用[稳定包迁移手册](breaking-upgrade-skill-package.md)。

## 下一步

- 技能作者：[README 的完整火球示例](../README.md#b-完整链路火球术-json--compile--memoryhost--runtime)
- Host 开发：[施法语义与战斗接入](skill-casting-and-combat.md)
- 同步开发：[Visual 与数据同步生产指南](visual-sync-production-guide.md)
- 框架维护：[实现学习手册](skill-implementation-guide.md)
- 发布人员：[生产门槛](production-readiness.md)
