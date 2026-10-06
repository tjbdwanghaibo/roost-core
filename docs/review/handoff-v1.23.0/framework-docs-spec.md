# 框架整体双文档规格（以 v1.23.0 tag 源码为准）

维护者要求（2026-10-06）：“还有一个就是这个框架整体的说明文档和实现，越详细越好”（与发版双文档同样要兼顾人读与 agent review）。

## 位置
- `docs/framework/README.md`：总入口（怎么读、两套文档的关系、术语表、包 → 文档索引）。
- `docs/framework/guide/NN-<area>.md`：说明文档（是什么、为什么这样设计、怎么用、配置、运维、限制）。
- `docs/framework/impl/NN-<area>.md`：实现文档（代码结构、数据结构、控制流 / 状态机、不变量与强制点、并发与锁、失败处理、测试与门禁、review 检查点）。
- 同一 area 两份文档同编号、互链；现有 `docs/USER_GUIDE.md`、`docs/INTERNALS.md` 保留为快速参考，头部链到 framework 文档，内容冲突以新文档 + 源码为准。

## 分区（NN-area）
00 总览：三大块原则（nest / dataengine / sync）、包地图与依赖方向、一次请求 / 一次落盘 / 一次同步的端到端走读、术语表
01 app 与生命周期：app、lifecycle、manager、kit/mods 装配、Mod 依赖、单实例锁 / liveness / fail-stop、stop 契约、readyz、静态注册、配置读取（严格读）
02 nest 调度与实体：nest 快慢双池、entity / Guard 作用域、actionflow、MissionRunner、goroutine / fctx、glsvet 规则
03 dataengine：engine、nestwal、projector、outbox、DAO 与 A1 统一回滚、驱动契约（mongo / redis，A2）、cache
04 sync：entitysync、syncstream、syncbus（JetStream 驱动）、lockstep、frame、玩家接入 TCP / gateway
05 remote entity 与 Mirror：remoteentity、L2 水位权威、Mirror 1～6、interest / 配额、ownerroute、bus / nats
06 saga：协调器、步骤收件箱（原生 / Mongo）、stepTransition、步骤预算、kit/saga、延迟决定 A
07 配置：configdata（规则、大小写敏感、热更）、tablegen / cfggen、featureflag、hotcode
08 skill 与战斗：skill 编译器 / 运行时 / checkpoint、eval-context 表、combatcomponent 与 buff 投影、attribute、spatial
09 kit 服务：account、session、global / activity、mail、chat、match、rank、directory、platform、玩家静态绑定
10 时间：clock、业务 / 系统双时钟、单调高水位、timer 排序
11 可观测与运维：metrics、health / readyz、log / failurelog、admin / ops（Bearer）、security、servicemetrics、仪表盘
12 代码生成与工程：codegen（roost CLI、entity / dao / rpc 生成器、模板）、game-demo 模板、robot、脚本（pretag、矩阵、mirror-local、source-head-check）

## 说明文档每篇结构
1. 一句话定位与边界（负责什么、不负责什么）；2. 核心概念与术语；3. 设计原因（关键取舍、维护者决定，附出处）；
4. 怎么用（业务作者视角：API、生成代码、最小示例，引用仓库内真实示例路径）；5. 配置（键、默认值、范围、校验）；
6. 运行与运维（指标、日志、错误、常见故障 → TROUBLESHOOTING T 行）；7. 保证与不保证（契约、已知限制、需外部验证项）；8. 相关文档。

## 实现文档每篇结构
1. 包与文件地图（path，职责一句话）；2. 关键类型与数据结构（path:line）；3. 主流程（编号步骤 / mermaid 时序图或状态机）；
4. 不变量清单：内容、强制位置、守卫测试；5. 并发：goroutine 归属、锁与锁序、快池禁止阻塞等；6. 失败与不确定结果处理；
7. 持久化 / 协议格式（键空间、集合、消息、版本兼容）；8. 测试与门禁（单测、性质测试、integration、生成工程脚本、故障矩阵，带命令）；
9. 历史与重要修复（指向 RR / U / REFACTOR 记录，只列改变了设计的）；10. review 检查点（具体问题清单）。

## 写作要求
- 中文，roost-coding“写给人阅读”风格；先结论后细节；表格 + 短段落 + 图。
- 所有事实可追溯到 path:line（v1.23.0 tag）或文档链接；推断写明“推断 / 未验证”。
- codebase-memory 索引若落后于 tag，以源码为准并注明；不贴大段代码。
- 不改任何代码；只新增 docs/framework/** 并在 USER_GUIDE / INTERNALS / docs/README 头部加链接。
