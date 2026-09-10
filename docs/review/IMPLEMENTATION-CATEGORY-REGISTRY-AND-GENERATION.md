# 实现学习：category 注册权威与生成接线

基线：Core `f3eaad9b38f87b2873e6f35b517b4ad993aed680`，Codegen `8c38eeb2a183c1519f8a28e102133282e29c9670`。证据与限制见[第三轮运行记录](REVIEW-2026-09-10-03.md)。本页解释当前实现，不代表建议均已实现。

## kind 表与身份解析

entity_factory.go 用按 kind 编址的原子指针槽保存注册信息，写者由 registryMu 串行化，复制并发布记录；读者读取已发布槽。它减少热路径查表锁，并不意味着整个注册过程的所有派生状态是一个整体原子事务，也不让 builder 指针指向的可变对象自动不可变。

idgen.go 的 BuildEntityID 先 ResolveEntityKindCategory，再构造 ID；resolver.go 的 ResolveEntityID 对已注册 kind 从注册表读取 Category。历史低两位不再是本地已知 kind 的类别权威，但仍被写入，并在未知 kind 时作为兼容提示。FullID 保留原始低位，远程提示位可按本地注册补齐。NormalizeFullID 验 kind、unique、注册存在等，不再把 ID 低两位与注册类别作比较。

因此“类别语义离开 ID”和“ID 原始整数完全不受类别配置变化影响”是两件不同的事：category 保持不变时兼容旧布局，改 taxonomy 数值后重新 Build 同一 unique/kind 是否仍对应原存储键，需要业务迁移验证。源码中的阶段性注释不能取代这一验证。

## 锁序的来源与校验

category_order.go 中类别数值就是锁序：Remote=1，World=2，PlayerScoped=3，Player=4，Other=5；Unknown=255 保留。RegisterEntityCategories 提供名称与可选类别集合约束；不声明名称也会按注册 category 派生锁序。managed kind 强制派生到 Remote 档，ValidateEntityRegistry 同时报告其声明类别不为 Remote 的错误。

kind 注册完成后刷新 lockRankByKind；EntityGuard/GetEntityGroup 使用派生结果，避免业务可变 hook 决定锁序。未知 kind 的远程位仍提供远程档提示，否则落 Unknown。生产应在处理请求前完成所有注册；当前 Validate 不冻结表，晚注册仍可能改变进程对同一 ID 的认识。本轮没有把并发晚注册当成安全支持场景。

Validate 可检查保留类别、managed 类别错误、显式声明集合中的缺项。没有声明 taxonomy 时集合检查跳过。它不保证业务分类语义正确，也不提供所有升级前后锁序兼容性的证明。

## category 标记到消费者

链路是 extractEntities 读取 `category=` → EntityDef.Category → collectEntityImports → generateInPackage → 注册 EntityBuilder；有 category 时直接输出表达式，无 category 时保留 MustEntityCategoryOfKind 的运行期查表前置。

聚合生成器 render 先分配 importAliases，再生成各阶段注册调用，最后 ValidateEntityRegistry。RegisterAll 使用 sync.Once，首次失败被记住，后续调用返回同一结果；这符合启动诊断设计，但不能当作修改注册配置后可无限重试的接口。

M-05 把明确类别带到接线中，减少手写 kind category 必须先注册的隐含依赖；尚未把身份层与实体构造包拆分，也未实现封表。M-04 删除 Capable 与旧分组 hook，升级需要 core/codegen 配套及重新生成。Mirror 的声明不等价于已经实现快照订阅或本地写拒绝。

## 本轮证据带来的接入经验

内置 category 的最小消费者编译成功；业务 view 常量与业务 entity 包布局分别触发 RR-20260910-05/06。import 是文件局部作用域，动态业务别名还必须避开生成模板固定名称。这两项不能靠“输出字符串包含 Category/Validate”验证，需要真正编译消费者。

建议后续消费者矩阵覆盖：内置/本地/外包 category、显式 import 别名、固定包名碰撞、旧 marker、managed 基类、多实体默认/显式输出、真实 bootstrap 注册顺序。此矩阵是后续建议，本轮只验证运行记录列明的三个消费者。

## 2026-09-11 生成修复验收

Codegen `cacd627b70991c5d0e38545866610db78e695b53` 已将 Category 加入 import 收集，并把 entity 加入 aggregateReservedNames；两处原消费者重新生成后真实编译通过。多实体显式 -output 在任何写入前拒绝，已有产物 hash 保持不变，去掉该选项后同包默认生成可以编译。见[验收记录](REVIEW-2026-09-11.md)。上文 RR-05/06 的失败描述是历史状态，当前原触发已修复。

具名保留清单仍需要与模板固定 import 同步，不能因为换成变量就认为将来新增名称会自动纳入。真实消费者编译为本轮独立验收，仓内的 AST/字符串测试是不同层次证据；两个层次可以互补。