# Sync 视图配置与发送语义

`SyncProfile` 是有限视图的身份：Key、LOD、SchemaVersion。它不包含玩家 ID、不存字段集合、不承诺独立发送频率。相同实体、相同 profile 的接收者共享内容捕获与组件编码。

本次把三个职责分开：业务用字段名定义视图，Manager 用显式优先级选视图，实体 packer 按视图和 dirty mask 编码。沿用现有包，不引入 profile 管理服务。

## 配置字段、优先级和来源

以下展示生成 DAO 的接入方式；`db.PlayerDaoSyncFields()` 由现有 DAO 生成器提供，字段位不需要手工维护：

```go
owner := entity.SyncProfile{Key: "owner", SchemaVersion: 1}
near := entity.SyncProfile{Key: "near", SchemaVersion: 1}
far := entity.SyncProfile{Key: "far", LOD: 1, SchemaVersion: 1}

views, err := dataengine.NewSyncViewSet(db.PlayerDaoSyncFields(),
    entity.NamedSyncView{Profile: owner, Priority: -10, Fields: []string{"*"}},
    entity.NamedSyncView{Profile: near, Priority: 0,
        Fields: []string{"Name", "Level", "PosX", "PosY", "Equipment"}},
    entity.NamedSyncView{Profile: far, Priority: 10,
        Fields: []string{"Name", "PosX", "PosY"}},
)
if err != nil { return err }

packer, err := entity.NewMaskedSyncPacker(views, PlayerSyncCodec,
    func(mask uint64) ([]byte, error) { return player.Dao().MarshalSync(mask), nil })
if err != nil { return err }
// 将 packer 交给此实体的 SubjectSyncState / subjectPacker 工厂。

manager, err := entitysync.NewManager(entitysync.ManagerConfig{
    Transport: transport,
    Interval: 50 * time.Millisecond,
    ProfilePriorities: views.Priorities(),
})
if err != nil { return err }

interest, err := policy.NewInterest(policy.InterestConfig{
    Manager: manager,
    AOI: policy.AOIConfig{
        Bounds: bounds, BlockSize: 150, EnterRadius: 150, LeaveRadius: 180,
        Bands: []int64{80}, MaxVisible: 49,
    },
    SourceProfiles: map[string]map[int]entity.SyncProfile{
        policy.SourceSelf: {0: owner},
        policy.SourceSpatial: {0: near, 1: far},
    },
    ViewSets: map[string]*entity.SyncViewSet{"player": views},
})
if err != nil { return err }
```

这段是装配片段，业务提供 DAO 实例、Transport 和 Bounds。不同实体类型可以给相同 profile 定义不同字段，但一个 Manager 内相同 profile 的优先级应一致。团队等关系加入 `Relations` 后可在 `SourceProfiles` 中独立配置，不再先把来源折叠为一个最小距离档位。

- 字段名接受 Go Name 或 BSON WireName。未知、歧义名称、重复 profile 在构造时拒绝；定义在当前生成 schema 上解析一次。
- `"*"` 显式包含当前 schema 的所有同步字段，适合业务确认过的 owner 视图；空列表表示不包含字段。新增字段会进入使用 `"*"` 的视图。
- 无生成 DAO 时，使用 `entity.NewSyncViewSet` + `SyncView.Fields` 直接提供业务掩码；0 是无字段，不隐式解释成全部字段。
- `SyncViewSet` 构造后不可变；Manager 和 Interest 都复制各自配置，调用方后续改 map 不改变运行状态。
- 未配置优先级时保留旧 LOD、Key、SchemaVersion 升序规则；配置后先比较 Priority（小者优先），相同再走旧规则。Key 是稳定平局规则，不应用来表达业务权限。
- `SourceProfiles` 中没有指定的来源/档位继续使用原有 `Profile` 回调；没有回调则使用 `DefaultBandProfile`。使用严格白名单 packer 时，应覆盖所有可能产生的 profile；未知视图会拒绝捕获，不回退到 default。
- `ViewSets` 非空时，Interest 构造会检查空间所有 band、启用的 self、关系 band 0 和显式映射在每个实体类型的集合中均存在，且优先级与 Manager 一致。SchemaVersion 属于视图身份，不能混用。运行时再次检查自定义 Profile 回调的选择，失败在 Subscribe 之前返回 Refusal。集合应与对应实体 packer 共用同一实例；该配置不自动识别任意自定义 packer 的内部定义。
- 多实体类型装配 Manager 时，先用 `entity.MergeSyncViewPriorities(playerViews, monsterViews)` 合并，冲突直接返回错误。ViewSets map 在构造时复制；集合本身不可变。可到达某来源的所有类型必须声明相应视图；只对某类型授权 owner 的业务仍需由其政策控制订阅。

Interest 现在按来源解析后的实际 profile 选优。若原自定义 `Profile(band)` 把更大的 band 映射为更小的 LOD，其选择结果可能与旧的“先取最小 band”不同；请按业务意图配置显式 Priority。默认 band→LOD 映射的结果保持不变。

## 变化与恢复

snapshot 编码该视图的完整白名单；delta 编码 `dirty mask & view.Fields`。profile 切换时仍发全量，即使对象已经存在，也会带 `Full=true` 的内容替换当前视图。客户端应把 Full 当作该视图的完整内容重新建立基线；不能把缩小视图的全量当作普通补丁，继续展示旧视图中不再包含的字段。

只有视图外的字段变化时，packer 不调用业务 marshaller，返回空 payload；框架仍发送版本推进信息。这样下次有可见字段变化时，BaseVersion 与客户端一致。这里没有引入每 profile 独立版本链，也不通过丢弃增量破坏原有链路。

同一次捕获中，full-dirty 与新订阅需要同一 profile 的 snapshot 时，只打包一次不可变 payload，分别保留不同的 BaseVersion/Reason 头。不会把 full 和 delta 的编码混成同一份字节，也不跨 tick 缓存状态内容。

Manager 使用新的 `PrepareViews` 明确传入需要的视图；无接收者时不打包无用的 default payload，但仍处理版本提交和持久化水位。公开 `Prepare` / `PrepareTick` 的空列表默认视图行为继续兼容。

20Hz 仍是合并检查频率；无内容或订阅变化时不发帧。本次没有实现事件触发发送，也没有给 LOD 添加隐含的降频语义。

## Demo 与兼容性

Player/Monster 模板现在通过生成 DAO 的字段词汇表提供 default、near、far 三个示例视图。default 保持原全字段行为，demo 场景默认装配不变；near/far 要由业务显式选用。Player near 排除 Gold、Exp、Items、AttrBase/AttrFinal，far 进一步排除装备；Monster far 排除 HP。

此前 demo packer 忽略任意 profile，现在仅接受声明过的完整 profile 身份；自定义 Key/LOD/SchemaVersion 必须加入配置。这是 demo packer 的有意行为收紧，不改变已有用户自定义 packer 的接口或默认 Manager 行为。未更新的已生成项目需要手动迁移其业务 packer，不应覆盖用户编辑的代码。

字段白名单不授予订阅权限。业务仍需决定谁可以订阅 owner/team 等视图；配置 Priority 只在已有的授权来源中选一个视图，不会合并成字段并集。

## 运行观测

Manager.Stats 新增 FlushCalls、EmptyFlushes、累计/最近 FlushDuration、DirtyCaptured、SnapshotsCaptured、Creates/Updates/RemovesAdmitted。捕获按实体/视图计数，准入按已成功 Push 的线协议对象计数；重试可能再次捕获。`entitysync_flush_duration` 提供固定桶分布，`entitysync_full_captures_total{reason=dirty|resync|schema|other}` 记录全量原因，不使用实体或会话标签。

AsyncTransport.Stats 新增 PendingReliableBytes、ReliableBytesInFlight、OldestReliableAge；年龄从准入时刻计，包含正在发送的消息。`MaxQueuedReliableBytes` 是每会话待发送字节预算，不含正在发送的一条，0 保持旧行为。消息数和字节预算任一超限都返回原有背压错误，entitysync 只关闭对应慢会话。保留 reliable 有序队列，不丢弃已经准入的旧增量。

Stats 会遍历状态，建议秒级采样；不要每个 tick 扫描全部 10000 实体来采集订阅计数。性能工具新增 `-async`，将固定时刻的测试信封经过真实 AsyncTransport，再由独立客户端解码校验。

09-24 新增 `Manager.Counters()`、`AsyncTransport.Counters()`，周期监控可直接读取原子计数，避免扫描实体和会话。Manager 增加快照延后数和捕获/编码/准入累计耗时；传输增加当前驻留字节、全局预算拒绝和消息过期次数。详细数量与最老消息年龄继续通过 Stats 获取。[六项实施与验证](REFACTOR-2026-09-24-sync-six-items.md)。
