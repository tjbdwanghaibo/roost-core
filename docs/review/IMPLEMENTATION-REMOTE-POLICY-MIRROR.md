# RemotePolicy Mirror：现有能力与实现方案

审查日期：2026-09-12。Core `ef44d770fc231896187b6e6b104d10ffa965bb01`，Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`，Codegen `cacd627b70991c5d0e38545866610db78e695b53`。本文件前半解释现有实现，后半是**尚未实施的方案**，没有修改源码。

## 结论与适用业务

`remote=mirror` 已有声明、ID 标识与默认生命周期，但尚无完整的只读实体运行时。`mirror.Replicator` 已实现通用副本消息传输，不能据此认定 `RemotePolicyMirror` 已落地。M-04 的[未实现边界](../bugfix/M-04-drop-capable-and-the-group-hook.md)与当前源码一致；之前的[类别审查](IMPLEMENTATION-CATEGORY-REGISTRY-AND-GENERATION.md)只指出声明缺口，本轮补查实际复用路径。

建议 Mirror 用于公会摘要、世界状态、跨服榜单等允许显式陈旧度的只读投影。发奖、扣款、竞争资格等权威判断仍由 owner 执行；客户端缓存命中不代表权威读。读完 Mirror 后向 owner 发命令，应携带观察到的版本，由 owner 校验。

## 当前代码实际做了什么

| 层 | 已有行为及源码 | 尚不能承担的职责 |
| --- | --- | --- |
| Entity policy | [policy.go](../../entity/policy.go):17–84：Mirror 为 remote-capable、非 managed；默认 `MirrorCache`；校验 lifetime 配对 | 不订阅、不加载、不强制只读；lifetime 注释本身声明其为元数据 |
| 构造/生成 | [entity_factory.go](../../entity/entity_factory.go):341、[gen.go](../../../cube-codegen/internal/entity/gen.go):34、50、400：特殊接口/RemoteBase 分支只针对 Managed | Mirror 仍走普通 EntityBase 分支；`noPersist` 独立设置，不能假定 Mirror 自动禁用 DAO/持久化 |
| Nest | [nest.go](../../nest/nest.go):477：只为 Managed prepare；[rollback.go](../../nest/rollback.go):822：非 Managed 仍可注册普通提交参与者 | 跳过 Remote 写批次不是本地写拒绝，也不是安全的 Mirror 读 API |
| 通用传输 | [mirror/envelope.go](../../mirror/envelope.go):48、129：校验内外层 topic/key/version/op、发布独立 MessageID、可传 context | 没有全量首次加载、重连补洞、权限校验、版本墓碑；Apply 错误怎样重试由实际 bus 决定 |
| 通用缓存适配 | [cache/mirror.go](../../cache/mirror.go):94：Upsert 解码后 Set，Delete 直接 Delete | 不通用地比较 Envelope.Version；Delete 不携带版本给 Store。`VersionStale` 是策略，不是复制协议 |
| 实体快照缓存 | [remote_snapshot.go](../../entity/remote_snapshot.go):160、244、308、350：完整 scope key、冻结字节、版本/epoch、delta gap、同版本内容冲突、加载限额 | 删除/淘汰后没有永久版本底线；不是端到端一致性与授权证明 |
| Remote 同步链 | [assembly.go](../../remoteentity/assembly.go):54 → [syncer.go](../../remoteentity/syncer.go):31、92：兴趣过滤发布、接收更新、gap/epoch/schema 异常后权威回填；[transaction_manager.go](../../remoteentity/transaction_manager.go):483：读时续租兴趣 | reader 与写 Manager 耦合，内部 wire/interest 实现未导出；不能直接把完整 Assembly 当轻量 Mirror 客户端 |
| 现有 Assembly | [assemble.go](../../remoteentity/assemble.go):49：要求 Redis、SID、原子事务 backend | 只读服务不应为启动镜像被迫接入写锁、事务存储、finalizer |
| 订阅协调 | [entitysync/subscription.go](../../entitysync/subscription.go):89、112、173：成员、profile、首快照准入、subject 串行、可靠 sink 契约 | 不等同 RemoteSnapshot wire，也不自行提供网络/历史重放；接入需明确协议适配 |

源码中的 `RemoteSnapshotKey.Policy` 是快照视图维度的 `uint32`，不能与实体 `RemotePolicy` 枚举混为一谈。Tenant/Kind/Scope/Policy 全部进入身份域；只按 EntityID 建缓存会串视图。

## 本轮实际验证

五包 `mirror/cache/entity/entitysync/remoteentity` 完整 race 测试及 codegen `internal/entity` 测试通过。临时 overlay `TestReviewMirrorSnapshotPrimitives` 另外证明：

1. 注册 Mirror kind 后，可以直接使用 `RemoteSnapshotCache`，不必伪装成 Managed。
2. 已缓存 v2 时迟到 v1 不覆盖；`BytesCopy()` 返回值的修改不污染缓存。
3. `Delete(key)` 后再送 v1，v1 会进入缓存。此测试明确断言**现有原语行为**，通过不代表“删除防复活”验收通过。

第三项说明 Mirror 协议必须新增版本墓碑/重同步屏障。当前这里只测试缓存原语，没有伪称已重现真实 broker 重投递，亦未把缓存失效 API 本身误报为新功能 bug。运行与复现代码见[本轮记录](REVIEW-2026-09-12-02.md)。

## 建议方案：复用快照基础，补一层只读客户端

下面出现的 `MirrorReader`、`MirrorClient`、`MirrorSpec`、`MirrorReadOnly` 均是**拟议名称，不是已有 API**。

### 分工与接口边界

- **core/entity**：保留现有 key、envelope、冻结 payload、cache；新增只暴露 `Read(ctx,key,consistency,minVersion)` 的 reader 契约。返回值带版本、epoch、是否命中与错误，解码产生独立 DTO，不能泄露 live DAO、可写实体指针或缓存 backing slice。
- **core/remoteentity**：从现有 Manager 抽出快照读、兴趣续租、接收更新的组件，组成 MirrorClient。输入已有 `RemoteSnapshotLoader`、`ISyncBus` 与可选 L2；复用 `mirror.Replicator`，不复制一套 bus。保留现有 Manager 外观，由它组合新组件，避免同时维护两份读协议。当前私有 wire/interest 需要内部提取与适配，不能仅凭公开类型名声称现在就能接线。
- **kit**：增加只读装配配置，注入 reader/loader/bus/生命周期/健康检查；不自动启用写能力。不为 Mirror 接入要求 Mongo 原子 backend 或分布式写锁。现有远程写服务继续原装配。
- **codegen**：生成只读视图及明确的 Mirror 注册/装配声明；不生成 DAO save/remove、CommitParticipant 或 Remote write participant。对旧 `remote=mirror` 加严格诊断与迁移提示；不能只设置 `noPersist=true` 后仍把可写 Entity 当只读完成。

第一版优先独立 DTO reader，不让 Mirror 对象进入普通 Nest 实体获取路径。对已有 Mirror Entity 接入，必须在构造、Nest 写目标准入和持久化参与者注册处明确拒绝，并给出迁移错误。框架无法阻止业务直接修改自己持有的公开 Go 字段，因此只读承诺应建立在不暴露权威可变对象的 API 上。

### Owner 与 kind 注册

保持跨服务相同的 EntityID/kind/category 与 wire schema，明确哪个服务是唯一写权威。当前 kind 注册表按进程维护且拒绝冲突政策；**不能在同一进程给同 kind 同时注册 Managed 和 Mirror**。方案需让公共身份/schema 与进程能力装配分离：owner 注册其写角色，consumer 注册只读角色；同进程混合部署只注册一次身份，使用独立 reader 视图，不重复登记相反 policy。不要为了绕过注册冲突发明另一个 kind 改写同一实体身份。

Owner 可以是已有 Managed 路径，也可以是普通本地权威 Entity。前者复用 commit/outbox 后的快照发布；后者提供权威 loader 与提交后发布适配。不能直接把 `SubjectSyncUpdate` 强转成 `RemoteSnapshotRecord`；版本、profile、schema 与 epoch 的映射需要显式定义和测试。普通 owner 第一版只提供完整快照，暂不要求接上所有 entitysync/syncstream 功能。

### 首次加载、推送与恢复

建议每个完整 key 的状态为 `Cold → Loading → Ready → Stale/Resyncing → Closed`，删除为有版本的 Tombstone。状态转换由按 key 串行的 apply/fetch 协调器拥有。

1. 启动时建立订阅并确认可接收，随后取得权威全快照。仅“先 Subscribe 再 GET”不足以证明无缺口：需与 owner 建立可验证的订阅水位/快照版本边界，或缓冲期间更新并在加载后按版本补齐。
2. 第一版仅发送 full snapshot，按 key 合并过时待发送快照，队列有界。队列溢出、连接重建或发现缺口时标记 Stale 并重新全量加载，不把丢包当更新成功。没有新消息的断线也要能被健康状态识别。
3. 复用快照 cache 加载合并、超时和并发额度；首次加载期间到达的新值不能被较旧 fetch 结果覆盖。缓存被淘汰/墓碑到期后，先恢复权威水位，再接纳历史流；仅保留 TTL 无法证明安全。
4. 兴趣按完整 key 维护，活跃订阅定时续租并抖动，最后一个本地使用者释放；当前 Manager 是“读时续租”，持续订阅不能在业务暂时不读时无声过期。设置总 key 数、每 key 等待者与回填并发上限。
5. 第二阶段才接 delta：复用 `ApplyUpdate` 的 BaseVersion/epoch/schema 检查及权威回填。可进一步接 `SubscriptionCoordinator` 做 profile 与成员管理，使用已有 syncstream 能力前另审协议适配、重放和背压，不把它列成 MVP 必选项。

### 一致性与删除协议

默认契约建议为有最大陈旧时间的缓存读；调用者可要求已观察到的最低版本。既有 `RemoteReadMonotonic` 加 minVersion 只能构建调用者传递版本下限的保证，不自动提供跨重启、迁移、淘汰的永久单调性。若 owner 换 epoch，调用者的观察 token 也需包含权威代际并定义比较规则。

线性化读只有在权威 loader 真正提供对应保证时才允许声明；普通缓存或随意 Mongo 查询不因枚举命名而变成线性化。无法满足时返回明确不支持/不可用，不能静默退化。断线时是否容忍陈旧必须由读选项决定，并返回可观测状态。

删除消息应包含完整 key、权威代际、版本、删除标志。接收侧在同一串行 apply 边界下原子比较水位、记录墓碑并移除值：旧 delete 不能删新值，旧 upsert 不能复活已删值，相同版本不同操作也必须有确定顺序或拒绝冲突。建议 owner 为删除分配新的内容版本。`MessageID` 只管投递身份，不能代替内容版本；当前 `Delete(ctx,key)` 和通用 `PublishDelete` 不足以实现此承诺。

墓碑保存期与 broker 可重放窗口必须一致；如果无法给出有限重放窗口，就需要持久高水位或重连后更换流代际并全量重建。L2 也要比较同一水位，不能只保护 L1。重新创建同 ID 应使用更高版本或新的不可混淆代际，不能归零后接受旧消息。

### 可靠性、权限与生命周期

权威状态先持久化，再经可重试 outbox 发布，复用已有 Managed commit 发布流程。普通 owner 也需明确等价的提交后机制；AfterCommit 回调本身不提供崩溃后补发保证。既有 [RR-20260911-06](../bug/REVIEW-2026-09-11-04.md) 与 [WAL 问题](../bug/REVIEW-2026-09-12.md)未在本轮关闭，不能把未修复的完成路径当作可靠交付证据。

对跨服务消息校验来源权限、key 内外身份、schema、payload 大小和租户/profile 可见性。Envelope 一致性校验与 checksum 不是身份认证。完整业务 key 是最终判断依据，传输 hash 不能作为唯一授权或身份依据。

停机顺序：停止新读/订阅准入，取消续租与回填，解绑消息入口，等待已准入 apply/fetch 退出，再释放缓存与依赖。将自身 context 传入 loader；Replicator.Stop 仅解绑订阅，不能假设它已等待所有 handler。启动失败要逆序回收已启动资源，重试不得留下重复订阅。

### 性能评价与分阶段验收

已有 cache 提供分片、容量/字节上限、冻结数据和合并加载，适合作为第一版基础。纯 L1 读无需 Nest 写锁或 Redis 所有权操作；但 DTO 解码仍可能分配，不能宣传整条业务读取零分配。热 key 上的 publish 分片锁与 L2 RTT、full snapshot 大小、订阅扇出是需要测量的实际成本。

| 阶段 | 可交付范围 | 必须通过的验收 |
| --- | --- | --- |
| P0 契约与封口 | 只读 reader、角色装配、生成诊断 | Mirror 不注册写参与者；不能写本地 DAO；同进程不冲突注册；owner 与 consumer 身份一致 |
| P1 完整快照 MVP | owner loader + 现有 cache/Replicator + 接收水位 + 墓碑 + 生命周期 | 首载与推送交错、旧 upsert/delete、delete 后重建、断线无新消息、缓存淘汰、重连补全、权限隔离、重复启动/停机取消 |
| P2 增量与扇出 | 现有 delta、兴趣续租、可选 coordinator/stream 适配 | delta gap/epoch/schema 切换、续租丢失、慢消费者溢出、同 key 合并、owner 切换 fencing |
| P3 实际部署验证 | Linux 两服务 + 真实 broker/存储 | 重投、强杀恢复、outbox 补发、重启水位；报告 payload/扇出/热点参数下的吞吐、p95/p99、RSS 与回填放大 |

本轮没有生产吞吐数据，不给出未经测量的性能倍数。可先以公会摘要这一种完整快照接入做纵向样例，证明 P0/P1 后再推广为所有实体的通用能力。
