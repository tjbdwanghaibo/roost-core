# B8 文档收口（2026-10-08，v1.23.1 未发布）

以当前源码和 B1～B7 的实际红绿证据为准。`docs/framework/impl` 保留 v1.23.0 原始分析，在末尾追加现行说明；历史 feature/bugfix 不抹掉当时结论，在开头标明被取代的部分。CBM 的 09-30 代际落后，本轮已按当前文件补证；不宣称图谱已更新。

## F00：跨分区口径

1. **不可回滚边界与持久确认分开命名。** Nest 的 `transactionPastCommitPoint` 表示“不再回滚/重排”：pipelined 是 Enqueue 已接纳，之后结果未知只能 fence，不能撤销猜测。fsync 覆盖 LSN 后票据完成，才到“持久确认边界”；成功回复、AfterCommit、Sync 放行仍等持久确认及解锁。async 允许先投影，但 WAL Ack 前仍保证 fsync。memory 没有磁盘持久承诺。02/03 两篇都采用这套补充定义。
2. **数量按范围。** 生成器 catalog 有14个 Mod 名，manager 确实实现 StopWithContext；生命周期列表补入 manager 后覆盖14种基础设施类型。RemoteMirror 是另外装配的能力，不能据 catalog 数字推断全仓 Mod 类型数。
3. **JetStream 结算归属。** nats/driver 负责执行 ACK/NAK/Term 与最大投递次数的终止判据；SyncBus 处理合法消息的 fanout/订阅生命周期，业务 handler 错误仍 ACK；Saga/RPC 各自把业务错误归类交给驱动，不共享一套重试语义。RR-25 的空窗策略等待确认，不能将候选方案写成默认已完成。
4. **跨包归属不等于搬包。** 保留三大块与基建布局，cache/Entity/nats 按主职责和使用侧互链；补充入口见 [FOUNDATION-PACKAGES](../framework/FOUNDATION-PACKAGES.md)。index/httpclient 等公开接口按 C8 保留，不用虚假生产调用消除“零调用”字样。
5. **服务数量。** kit/service 有10个服务包（包含嵌入使用的 directory）；game 模板托管9个框架服务，加 game 共10个进程。activity 属 global 路径但独立托管；directory 不另起进程。

## F04-D：同步文档逐组更正

| 原始问题 | 当前结论与落点 |
| --- | --- |
| 反向索引、profile 排序、显式 Publish、PrepareTick | ENTITY_SYNC / INTERNALS 已改：session lifetime 保存 subjects；先比 ProfilePriorities，再 LOD/Key/SchemaVersion；EntityBase 无 Publish；Flush 调 PrepareViews |
| lockstep 历史“按容量” | 必须调用方 TrimBefore/TrimHistory，INTERNALS 已改；Room 不自设无限保留承诺 |
| syncstream 只用于服务间 | 内置 Publisher 经 ISyncBus；skillsync 可复用流语义到客户端，宿主提供相应适配。kit/README 与 Skill 架构说明统一 |
| EntitySync datagram / 单房间100实体 | 状态基线、增量、remove 全走可靠有序通道；100是每会话默认MaxObjects，1024是每subject默认订阅者上限；USER_GUIDE 已改 |
| 限流和 auth 骨架 | 普通工程默认拒绝，game-demo 用 account 验 session；TCP内建每连接限流及MsgID0心跳，生成接入指南已补完整契约 |
| room/sync 配置兼容、configuredPrefix | A4后只读syncbus；旧段无fallback。现行指南改正，历史RR加后续标注；RR-25 max_deliver删除尚未批准，不写成已删除 |
| AssemblyTTL、碎片测试覆盖 | 新多片输入到达才清扫，没定时器；旧文关于确定时间TTL和耗尽场景的覆盖撤回，不用结构测试代替行为证明 |
| replication路径、Sync消息身份 | 传输位于sync/nettransport，DeliveryIDs/Replicator位于sync/syncbus；ROADMAP更正路径。MessageID反射删除属于RR-25候选，待确认 |
| lockstep/playerTCP指标、health | OBSERVABILITY收全源码指标并用守卫校验；entitysync健康已正式注册（RR-22），lockstep前缀与包路径用当前值 |
| SetDurableWatermark | 现行API是ManagerConfig.DurableWatermark；T-100与nestwal注释已改 |
| 事件发送与Drain | ModeOnChange已正式接入；setter只标脏。Drain也等retry，永久拒绝只能修复关系或超时；历史方案已标明后续变化 |
| 卸载重载上界、SelfVisible、AOI名称 | 统一 ceil((QueueCapacity+Workers)/Workers) × T_entity（含所有尝试与退避）；SelfVisible只有显式false关闭；改为per-region AOI |
| syncruntime脚本路径 | 单仓入口codegen/scripts/entity-sync-runtime.sh；正式发布pin测试与本地源码测试须区分 |
| live订阅“绝不丢” | 受流保留期限/容量、MaxDeliver次数上限、坏信封ACK、业务错误ACK约束；sync.go中文注释明确例外，RR-25未解决部分不隐藏 |
| FileJournal合批 | 直接并发Append可合批；单History.Record被写锁串行，README撤回逐业务记录减少fsync的宣称 |
| BaseSequence、Export/Import、SweepIdle、客户端观测 | 新流首delta base0，其后指向前包；journal恢复优先，Import换checkpoint代；清理走Coordinator；订阅端没有独立checksum/assembly指标，宿主观察错误；Skill指南已更正 |
| battle desync、调试凭据、推送序号 | 当前两人无多数也裁NoMajority；auth无player捷径；RS v2推送非0递增，PB/Sync类型不同；历史demo方案保留当时实跑但标明当前规则 |
| WriteGate、关停预算、middleware | WriteGate error断连；shutdown_timeout是真实StopBudget；Service.Shutdown先于Mod停止且listener仍开；没有项目Protocol middleware注入点，业务观测放Authenticator/endpoint |
| Scene SessionID、重连测试 | Scene选择playerID作entitysync会话；接入层每连接独立SessionID。同ID替换测试证明运行时替换契约，不能据此称demo重连会关闭旧连接 |
| dispatch错误含义 | 包括WriteGate、未知ID、解码、panic、取消与endpoint错误；模板观测README更正 |

## F08-D / F09-D / F12 G5

- Skill 当前 checkpoint=10；文档中的5/6/8明确为历史演进。单仓同tag升级，旧格式拒绝；旧三仓依赖/发布图由当前架构指南取代。
- 包装Host不仅转能力表，还转可选接口；StopSpawn幂等、能力如实声明补入Host六条契约。mul_bp探针非零中性值；HostAdapter是需业务包装的战斗适配器，不能与MemoryHost全部行为等同。Buff推进由业务Tick驱动，Runtime不享有Nest DAO回滚。
- Skill表现流可靠有序，新字段隐私由业务projector负责；CloseObserver不撤回在飞Publish。StateDeltas名称、ROOST_SYNC_SOAK环境变量按当前源码；历史v1.23.0分册不改当时版本证据。
- B3已改服务注释：名字目录全局；SelectRole请求accountID属于可信服务边界；一账号一区服一角色由slot insert-only保证；activity观察重试不自动消耗失联投递预算，人工ExhaustDispatch；ServiceMetricsConfig.ApplyServiceMetrics与groups_file必填用当前API。
- game模板说明统一9个托管服务；新增add rpc列入用法。删除无读者的codegen/framework-lock.json schema1历史产物，release按schema3动态生成；framework verify用expected-release，release清单和minimum已准备v1.23.1。CLI的`roost-codegen`版本输出前缀保留为工具名称，不代表独立Go模块。
- 生成game gift的debit/refund用DataEngine native inbox，外部步骤用Mongo step inbox，注释已区分。

## 保留与未完成边界

B1～B5、B7已验收；B6除RR-25已验收。RR-25候选修改尚待批准和真实broker验证，不计完成；Remote唯一outbox发布者是独立产品方向，尚无选择，不在本轮暗中切换。Windows按维护者既定范围只保证编译/CLI，Y14保留。这里的文档清单不是对所有故障的穷尽证明。
