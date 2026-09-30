# Codegen 第三轮新增问题：其余生成器的输入退役

**2026-09-30 修复更新：**本页以下“未修复”是第三轮基线时点。RR-06～10 的原触发已在后续工作中修复并按声明场景验证，逐项见 [Attribute](../bugfix/RR-20260930-06.md)、[Event](../bugfix/RR-20260930-07.md)、[Webroute](../bugfix/RR-20260930-08.md)、[Tablegen](../bugfix/RR-20260930-09.md)、[Errcode](../bugfix/RR-20260930-10.md)；[验证边界](../review/REVIEW-2026-09-30-codegen-04.md)。未发版。

基线 Core `5fedc6526d49378246f76fa6ea9cf9e582435eee`。前四项在隔离目录中用当前 CLI 做了“生成 → 删除最后一个输入/标记 → 重跑”的实际复现，均 **P2、未修复**；第五项为 Errcode 注释误提取，**P3、未修复**。没有把 Entity/Nest 的修复外推到这些生成器。[运行与复现](../review/REVIEW-2026-09-30-codegen-03.md) · [证据](../review/evidence/codegen-review-20260930-03/README.md)。

## RR-20260930-06 · Attribute 删最后一个 profile 后旧实现仍留存

`codegen/internal/attribute/main.go:26-52` 在 `len(profiles)==0` 直接成功返回，只写当前 profile，从不枚举旧 `gen_<profile>_attribute.go`。隔离 CLI 生成 `gen_player_profile_attribute.go` 后，将唯一 `//roost:attribute` 改成普通注释再运行，退出 0，旧文件仍在（`ATTRIBUTE_OLD_EXISTS=True`）。旧字段访问器和注册可能继续暴露已删除配置。建议按默认输出名与生成头对账，覆盖改名、零输入、手写同名文件与 `-output` 自定义路径；删除前验证当前输入。

## RR-20260930-07 · Event 删除最后定义/handler 后旧类型与派发仍留存

`codegen/internal/eventgen/main.go:71-78` 在零事件定义时直接返回；`codegen/internal/eventgen/handler.go:57-145` 只写当前 receiver 的 `_event_gen.go`，不退役消失的 receiver。隔离 CLI 先生成一个 `EventPing`，再移除唯一定义，第二次退出 0，三份 `event_*_gen.go` 仍在（`EVENT_OLD_COUNT=3`）；handler 退役为同一缺失对账路径的源码推断，尚未单独动态复现。旧事件 ID/类型对客户端有协议兼容风险。建议按当前定义、receiver 和生成头建立期望集合，先验证所有 handler 再清理；业务方另定旧事件 ID 的版本窗口。

## RR-20260930-08 · Webroute 删除最后标记后旧路由仍留存

`codegen/internal/webroute/gen.go:15-92` 仅把当前有路由的包放进 `packages`，循环只写这些包；没有处理先前生成、现在零路由的 `webroute_gen.go`。隔离 CLI 删除唯一 `//roost:web` 后退出 0，`WEB_OLD=webroute_gen.go`。若仍在业务启动时注册该生成文件，旧 HTTP 路由继续暴露；该消费者结果需在正式项目动态确认。建议对扫描范围内有自身生成头的路由文件做当前包集合对账，同时保护手写文件及混合包失败路径。

## RR-20260930-09 · Tablegen 元数据清空后旧 JSON 留存

`codegen/internal/tablegen/main.go:98-105` 在零 meta 且没有 `-out` 时直接返回；`convertCSVToJSON` 只写当前 meta 的 JSON，未清理不再有 meta 的 JSON；`codegen/internal/roost/generate.go:116-128` 在表格输入为空时也跳过 config-data。隔离 CLI 用一张 `monster` 表生成 `monster.json`，删除唯一定义后重跑退出 0，`TABLE_JSON_OLD_EXISTS=True`。旧配置数据会与当前 schema 不一致，实际加载行为取决于消费者。建议在明确文件归属/manifest 后处理退役，避免删除手工 JSON；`roost` 暂存提交和 `--check` 须同样识别 JSON 的删除/漂移，覆盖部分表与全部表删除。

## RR-20260930-10 · Errcode 注释中的 Define 被当成真实错误码

`codegen/internal/errcode/main.go:24,49-106` 以正则直接查每个 Go 文件的原始字节，不解析注释/字符串与实际调用。隔离 CLI 的 `errors.go` 只有 `// retired: var ErrGhost = errcode.Define(500999, "ghost", "removed")`，没有有效定义；生成的 CSV 仍包含 500999（`ERRCODE_COMMENT_GHOST=True`）。这会让错误码清单出现不存在的代码，若与真实定义撞号还会错误拒绝生成。建议用 Go AST 只接受真正的调用表达式，保留现有常量数字与字符串限制，并针对注释、字符串和重复码加回归；不要仅用“行首不是注释”补丁，因为块注释/字符串仍会误命中。

前四项是输入删除边界，第五项是原始文本误提取；本轮只登记 review，不改这些生成器。实际业务工程编译/部署、历史客户端兼容未做。
