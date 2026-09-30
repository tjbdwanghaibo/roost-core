# Codegen 第五轮：cfggen 消费者与依赖自动迁移

**修复更新（第六轮）：**以下 RR-CG-12～14 已修复并在具名场景验证，未发版：[RR-CG-12](../bugfix/RR-20260930-CG-12.md)、[RR-23](../bugfix/RR-20260930-CG-13.md)、[RR-24](../bugfix/RR-20260930-CG-14.md)。[全轮验证](../review/REVIEW-2026-09-30-codegen-06.md)。下方“未修复”及修前失败保留第五轮历史时点；不代表当前状态。

审查基线：Core `30085d628bc70eb89f051329b90020d652d97c52`。以下三项均为 **P2、已复现、未修复**；本轮只提交文档与复现材料。实际生成器在 Core `codegen/`。运行记录见[第五轮](../review/REVIEW-2026-09-30-codegen-05.md)，[复现脚本与原始结果](../review/evidence/codegen-review-20260930-05/README.md)。

编号更正：本报告原 Codegen RR-12/13/14 后被另一工作线重复使用，当前分别为 [RR-CG-12](RR-20260930-CG-12.md)/[23](RR-20260930-CG-13.md)/[24](RR-20260930-CG-14.md)。下方标题、原始日志与提交历史不改写；另一工作线 Nest/Entity 的独立 RR-12～14 不属于本报告。

## RR-20260930-12

**数字字段显式 `index: false` 仍引入 strconv，CLI 成功但消费者编译失败。**

位置：`codegen/internal/cfggen/main.go:617-627,680-699`；现有 `main_test.go:114-122` 只断言索引标签和 accessor 消失。

触发：表含 `camp: int32, index: false`，且没有其他启用的非字符串索引。`generate` 按 `field.Index != nil` 判断是否导入 strconv；YAML 的 false 是非 nil，但 accessor 循环通过 `indexSpec` 解析 false 后不输出任何 strconv 调用。

期望：false 与未声明索引都生成可编译绑定。实际：正式 `cfggen -meta schema.yaml -out cfg` exit 0；独立消费者 `go test ./...` exit 1，报 `"strconv" imported and not used`。相同 schema 改 `index: true` 或删掉 index 选项都通过，排除了环境/依赖问题。

影响：显式关闭已有索引或元数据统一输出 false 时阻断整个配置包构建；生成器和原有字符串断言测试会假绿。

实施建议：用已解析的 enabled 状态决定是否导入 strconv，并与 accessor 的类型转换条件共用规则。至少覆盖 int/bool false、无索引、只有字符串索引、混合启用/禁用索引，编译生成消费者；不能只删除 import 而破坏真索引。已有生成包重新生成即可，没有运行期数据迁移要求。

## RR-20260930-13

**生成名称登记不完整，合法 bean 名与默认注册函数或 import 名冲突。**

位置：`codegen/internal/cfggen/main.go:344-376,626-633,662-668`。

触发一：bean 名为 `RegisterConfigData`。validateMeta 预占了 `RegisterGeneratedConfigData` 和 `MustRegisterGeneratedConfigData`，漏掉新增的默认注册 wrapper；后续既生成 `type RegisterConfigData struct`，又生成同名函数。触发二：bean 名为 `configdata`，与同文件固定 import 名冲突；bean 名直接使用，不经过首字母导出转换。

期望：生成前明确拒绝名称冲突，或采用一致的消歧规则。实际：两种 schema CLI 均 exit 0，`go/format` 接受语法；真正编译分别报 `RegisterConfigData redeclared in this block` 和 `configdata already declared through import`。正常 bean、嵌套 bean、切片递归和分组筛选的消费者编译对照通过。

影响：schema 已被校验、文件也落盘，但绑定无法编译。`format.Source` 只提供语法格式化，不检查声明/导入解析。

实施建议：将固定函数、固定 import，以及条件导入的名称纳入同一生成命名空间检查；至少补 `RegisterConfigData`、`configdata` 和启用数字索引时的 `strconv` 测试。优先拒绝冲突并点名 bean 与保留名，避免静默改已有公开 API。保留现有的表/对象/accessor 相互冲突检测；用消费者编译验证正常名称，不能把“gofmt 成功”当作类型检查。存量冲突 schema 需改 bean 名及其引用并重新生成。

## RR-20260930-14

**依赖事务在暂存树完成合仓迁移，却仅回写模块文件，成功后丢弃源码与 manifest 迁移。**

位置：`codegen/internal/roost/dependencies.go:34-65,114-123`；`consolidate.go:196-286,628-648`；CLI `cli.go:205-210` 的 `roost project deps` 调用公开入口。

触发：manifest 已满足当前版本策略，但 go.mod/业务 import 仍引用合仓前模块，例如 `roost-kit/mods`。`UpdateFrameworkDependencies` 复制项目；stage 内的 `needsConsolidation → ConsolidateProject` 改写 Go import、清除旧 require 和 legacy manifest 策略，然后完成依赖解析；外层 `planExplicitStagedFiles` 只规划 `go.mod/go.sum`。

期望：自动迁移成功须把必要且明确归属的迁移结果与模块依赖一起提交，或在改依赖前显式拒绝并要求先运行 consolidation。实际：runner 在 stage 读到了 `roost-core/kit/mods`，日志报告 `rewrote 1 Go file(s), go.mod, roost.yaml`，函数返回 nil；root 的 go.mod 已只有新 Core 依赖，business.go 却仍是 `roost-kit/mods`，roost.yaml 的 legacy policies 也保留。

证据：临时 Go overlay 的期望行为断言失败两处。依赖 command runner 使用现有注入点模拟 get/tidy 成功，**没有验证真实代理、发布版本解析或旧模块消费者的具体编译错误**；源码与输出文件对账已直接证明迁移丢失。解析失败对照通过，root 三个输入逐字节不变。现有 `TestFrameworkDependencyUpdateStagesAndCommitsOnlyModuleFiles` 只覆盖“隔离业务文件任意改写”，没有区分框架自身必需的 migration 变化。

影响：用户得到迁移成功信息和新的依赖文件，原项目仍引用退役模块；下一次 tidy 可能重新引入旧模块，或无法解析/构建。该报告限于依赖入口的自动 consolidation 分支：`project upgrade --consolidate` 在外层直接迁移 root，不据此断言该显式路径也丢弃迁移。

实施建议：复用现有暂存、输入快照、逐文件差异提交和回滚能力，明确迁移输出的允许集合（ConsolidateResult.Files、必要 manifest 和模块文件），检查原项目并发修改后一起提交；不能把依赖 runner 的所有业务文件改动无条件回写。若暂不支持此事务，deps 应拒绝旧布局并说明显式迁移步骤。补成功迁移、失败保护、dry-run/显式 upgrade、并发业务输入变化、无 consolidation 时任意业务改动隔离等场景。迁移是文件/API 布局问题，不授权自动修改业务符号或生产数据。

## 证据边界

Tier 2 图谱定位和调用追踪，加最新源码逐项核对、正式 cfggen CLI/独立消费者和临时 overlay。初始覆盖没有记录缺口，但 metadata_changed；刷新请求超时，不能宣称图谱全域同代或 Codegen 全审完成。四个反例执行对应三个根因，四个正常/失败保护控制通过。包回归通过不关闭这三项新缺陷。
