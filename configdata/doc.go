// Package configdata 持有业务配置的不可变快照，并负责加载、校验与热更。
//
// # 规则在加载层强制（B10）
//
// 表与单例配置声明的列规则（TableDef.Rules / ObjectDef.Rules，类型 FieldRule）
// 在每次 Load / Reload 时检查：required / unique / min / enum 查原始 JSON 行（缺列
// 与零值分得清），ref 在全部表加载后查目标表的主键。生成器（tablegen 的 CSV 转换
// 与 -check）用同一份规则、同一个检查器（包 configdata/rules）提前反馈，但把关
// 只在这里：直接改 JSON 再 reload 也绕不过去。违反任何一条，整次加载被拒，旧快照
// 继续生效，错误是点名“表 / 行 / 字段 / 规则”的 *RuleError。
//
// 键大小写敏感（2026-10-06）：数据文件里的键必须与字段的 json 名逐字一致，嵌套
// 对象同样；只差大小写的键在解码之后被拒绝（Rule "case"），不交给 encoding/json
// 的大小写不敏感匹配。未声明的键维持原样（宽松忽略，严格模式由解码拒绝）。
//
// # 热更的可见性契约（C2）
//
// 一次 Reload 依次是：build（读文件、规则、表 / 对象 / custom 的校验）→ 监听者
// ValidateReload → BeforeApplyReload → **发布**（Current、DefaultStore、fctx 运行时
// 配置一起切到新的一代）→ lifecycle PhaseConfigReload → AfterApplyReload。
//
//   - 新的一代在 AfterApplyReload 之前就已发布：发布之后准入的请求读到新的一代。
//   - AfterApplyReload（或 lifecycle emit）失败时整次撤回，Current 回到旧的一代；
//     发布到撤回之间准入的请求在整个生命周期里读的仍是被撤回的那一代——请求钉住
//     准入时的快照，同一请求内两次读不会跨代（ActiveSnapshot）。不能接受这一点的
//     检查应放进 ValidateReload / BeforeApplyReload，它们在发布之前。
//   - Store.Rollback 把上一代重新发布，走同一套监听者协议；回滚之前准入的请求
//     读的仍是被回滚的那一代。
//   - 版本号来自单调计数器，失败、撤回与 DryRun 也占号：成功后版本变大但不一定
//     +1，Rollback 回到上一代的版本号。
//
// 每次 Load / Reload / Rollback 结束后，Store 恰好报告一次结果（ReloadOutcome，
// 经 OnReloadOutcome 订阅）并写一条日志：成功 Info，失败（未发布）与撤回（已发布后
// 收回）Warn，带阶段、原因与版本。build 阶段的失败到不了任何监听者，只在这里可见。
package configdata
