# cfggen 的名称、类型与依赖迁移事务

## 2026-09-30 第六轮修复后的补充

下方是第五轮修前快照；RR-22～24 已在源码 `2f68aa22` 及其父提交修复，[逐项行为/兼容](../bugfix/README.md)、[正式验收](REVIEW-2026-09-30-codegen-06.md)。现在 usesStrconv 复用 indexSpec 的 enabled，固定 wrapper/configdata 与条件 strconv 名在完整 meta 校验时登记；冲突在写入前拒绝，正常 schema 真实消费包编译通过。

依赖事务现在在外层 stage 做 ConsolidateProject，按明确 Files/Manifest 名单提前规划并冻结迁移字节；模块 get/tidy 在其后运行，最终模块变化与迁移一起用既有输入核对/commitSyncChanges 提交。提前冻结是关键：即使 resolver 随后任意覆盖名单内业务 Go/manifest，也不把覆盖内容提交回 root。现有无迁移隔离语义保持。正式 CLI get/tidy 加源 head local replace 的消费者也通过，不等同发布验证。

三项原触发不再是当前缺陷；以下根因教学保留历史，并不把未查的 JSON 运行时、所有升级组合或磁盘/强杀语义写成已通过。

当前源码基线 Core `30085d628bc70eb89f051329b90020d652d97c52`。[第五轮运行](REVIEW-2026-09-30-codegen-05.md)区分源码事实、执行结果和建议；[RR-12～14](../bug/REVIEW-2026-09-30-codegen-05.md)均未修。

## schema 到配置包

`cfggen.Run` 先解码严格 YAML，再由 `validateMeta` 校验完整元数据的名称、类型、table key/ref、索引和非切片 bean 循环。之后 `exportForGroups` 解析本次 target，删除不属于目标的表、global 和字段，并检查导出后的主键、引用、bean 字段等约束。随后 `generate` 写一个 `cfg_gen.go`，经 `format.Source` 后落盘。对应源码：`codegen/internal/cfggen/main.go:148-204,227-342,344-599,610-706`。

bean 的名字作为 Go type 名直接输出；表/global 经导出转换生成 `<Name>Cfg`；同表的索引生成 `<Table>By<Index>`。命名检查因此要覆盖整个输出文件，而不只是 YAML 原始名称：不同 snake/camel 名可能导出为同名，固定 wrapper 与 import 名也占用标识符。当前登记遗漏 RegisterConfigData/configdata；两个 schema 都通过语法生成后在类型检查阶段失败，见 RR-13。

`FieldMeta.Index` 是 any：nil、bool false、bool true、string 名称语义不同。writeStruct 与 accessor 使用 `indexSpec` 识别 enabled，但 import 决策只检查是否非 nil，导致 false 与输出使用的规则分叉。RR-12 是“选项已声明”和“能力实际启用”混为一谈的直接后果。建议以后让校验、标签、函数、import 共用规范化后的 schema；这里是建议，并非现有实现。

嵌套 bean 是值字段/切片字段；纯值递归没有有限内存布局，现有循环检查拒绝；切片递归允许，因为切片头不内嵌无限大小的元素。这轮 server 控制含 `Bundle` 的 `[]Bundle`、嵌套 Reward、int/bool/string 索引与 global，消费包编译通过，client-only 表/字段被筛去。真实 JSON 读取、反射 ref/required 与 skipempty 执行仍须另外跑；输出声明正确不等于运行时数据契约已验证。

`RegisterGeneratedConfigData(r)` 显式接收 registry，`MustRegisterGeneratedConfigData` 在错误时 panic，带 `//roost:register phase=config` 的 `RegisterConfigData()` 使用 DefaultRegistry，供 aggregate 调用。accessor 通过 snapshot 获取 table/object，缺失表时索引查询返回 nil。这些生成调用复用 Core configdata，生成器不另建加载、持久化或并发语义。

## 依赖解析的暂存与所有权

公开 `UpdateFrameworkDependencies → updateFrameworkDependenciesTransactional` 建同级临时目录并复制工程、保存应用输入快照，调用内层 update，最后验证 root 输入未改再提交文件。`updateFrameworkDependencies` 在旧模块/旧布局存在时先 ConsolidateProject：按映射改 Go import、移除旧 require、清空 legacy manifest 策略；再调用 get 和 tidy。外层当前只把 go.mod/go.sum 当允许提交集合。源码：`codegen/internal/roost/dependencies.go:30-65,98-145`、`consolidate.go:196-286,608-695`。

这个隔离原本保护业务源码不受依赖 runner 任意修改；现有测试刻意在 stage 改 business.go，然后验证 root 保持。但自动 consolidation 也产生必需的业务 import 改动，两者被同一过滤规则忽略。stage 与 root 的生命周期不同：stage 的日志与 resolver 能看到迁移，不代表 commit 接收了它。RR-14 以真实事务入口和注入的 resolver 实测了两个层级的最终文件；失败控制则证实依赖失败时 root 字节保持。

修复需要区分业务所有权与框架迁移授权：可以用 ConsolidateResult 明确列出必需迁移文件，再复用既有输入快照/差异提交/回滚；不应提交 stage 所有 Go 文件来消除错误，那会破坏原隔离保证。另一可接受行为是 deps 拒绝旧布局并指引显式 consolidation。root 的并发输入变化、manifest 最终政策与模块版本、日志成功时点，都应纳入同一契约。

CLI 的 `project deps` 直接走此入口。`project upgrade --consolidate` 是另外的路径：先在 root 调用 ConsolidateProject，然后升级 manifest/同步工程/解析依赖；无 consolidate 且存在旧布局时明确拒绝。不能从 RR-14 推定显式路径也有同一漏提交问题，也不能从 dry-run 源码分支推定迁移消费者已构建。

## 本轮可复用的检查方法

生成器回归至少分三个层次：元数据校验与输出文字、正式 CLI 的生成/对账、生成消费者的类型检查或运行。gofmt 只能在前两层提供语法证据。本轮正式包测试/race 通过而消费者反例仍红，正说明三个层次需分别记录。

文件事务则分别检查 stage 的中间结果、root 的成功结果、失败后的 root 前像、并发修改保护。成功日志不是提交收据；只快照/回滚了部分文件，也不能代表整次迁移原子。强杀、磁盘故障和 rollback 失败仍是本轮未验证范围。
