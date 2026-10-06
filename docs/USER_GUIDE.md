# Roost 开发者完整使用说明

权威快照加载现要求结果完整 Key 与请求一致，异键在写入前返回错误（NC-35，main未发版）。Monotonic/Linearizable 的成功结果还必须在返回前未过 ExpiresAt、实际 L1 达到 minVersion；epoch准入保留的版本不足返回已有 ErrRemoteSnapshotStale（NC-36），L2发布期间过期返回miss（旧RR-08补修）。接入方修正loader身份/权威版本，不忽略错误或放宽epoch保护；缓存miss与复制返回nil不代表业务持久ACK。[机制/边界](review/IMPLEMENTATION-AUTHORITATIVE-SNAPSHOT-POSTCONDITIONS.md)。

Mirror现有适配器新增payload身份校验（NC-33/34，main未发版）：cache接收端配置的KeyOf/VersionOf必须与发送端规则一致，不一致更新在Store写入前返回错误；未配置提取器不承诺该维度校验。带身份的null更新拒绝。Remote interest正式格式仍为Upsert，信封key包含完整snapshot key与SID，version仍是ExpiresAt，Generation独立负责代际。不要把错误消息盲目重发、校验成功当成发布认证，或普通Delete当成版本墓碑。完整只读DTO Mirror仍按既有方案待实施。[接入机制](review/IMPLEMENTATION-MIRROR-PAYLOAD-IDENTITY-AND-ROUTING.md)。

嵌套DAO的wire恢复已修复“加载值正确但下一次深层修改未提交”（RR-20261005-NC-32，未发版）。升级生成工具后重新生成关联DAO/nested代码，再编译并执行加载后setter的业务提交回归；仅升级runtime不会修改已有生成代码。唯一父归属仍受保护，含已绑定指针子对象的nested结构不能浅复制为独立树；需要独立状态应从数据重新构造。历史漏写数据不自动恢复。[机制与验证](bugfix/RR-20261005-NC-32.md)。

RefHMap Set/Delete 返回 `cache.ErrRefHMapRegistryChanged` 表示读取键登记之后、它又登记了本次清理清单之外的 hash（另一布局发布了新键）、此次Lua明确未写；同布局的并发首次创建、并发删除、记录到期不会返回它（RR-20261004-09，未发版）。先读回当前schema/业务意图再决定重试，不自动以旧全量值覆盖新布局。网络/Eval错误仍可能已应用，不能按明确拒绝处理。Delete也要求adapter支持现有Eval；存储格式保持，历史孤儿不自动清理。[用法和限制](bugfix/RR-20261004-NC-30.md)。

## 2026-10-06 业务时间只许前进（main，未发版）

同一套部署的业务时间（真实时间 + `time.logic_offset`）不能往回走：App 在单实例锁之后、任何 Mod Init 之前读部署级高水位（协调存储 `<singleton.key_prefix>:business_time`），按新偏移算出的业务时间低于“高水位 − 1 分钟”就拒绝启动（`app.ErrBusinessTimeMovedBack`，点名偏移与高水位），运行中每 10s 推进高水位。只在非生产检查（生产偏移强制为 0，行为不变）；配了非 0 偏移的进程必须能打开协调存储（bootstrap 装 `App.Singleton`、写 `singleton.key_prefix`，`singleton.enabled` 可以是 false）。测试环境要回到过去只能清库重建。随之删掉了只为“偏移往回调”存在的 API：`activity.Config.SystemNow`、`mail.Config.SystemNow`、`mail.RedisConfig.StorageGrace`（宽限固定为 `mail.EnvelopeStorageGrace` = 24h，原 `DefaultEnvelopeStorageGrace` 改名）——activity 派发退避、进度凭证与 mail 领取租约回到业务时钟；手工装配设置过它们的地方删掉那一行即可。详见 [§10 业务时钟与系统时钟](#业务时钟与系统时钟)。[方案](feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md)

## 2026-10-06 业务时钟与系统时钟（D-L3，main，未发版）

时间分两个钟：**业务时钟** = 真实时间 + `time.logic_offset`，活动窗口与协调器、World 定时器、日 / 周重置、冷却、邮件 / 道具业务过期、赛季、排行周期、游戏时间都读它，从 `app.BusinessClock(registry)` 拿（kit 的 activity、mail、rank、session Mod 已注入）；**系统时钟** = 真实时间，帧率、租约与锁、超时、重试、存储 TTL、Ack、日志与 WAL 时间戳，直接用 `time` 包。偏移只在启动时读一次，所有进程写同一个值；`env: production` 时非 0 拒绝启动。mail 信封的 Redis TTL 现在是业务剩余时长 + 24h 宽限（固定 `EnvelopeStorageGrace`；v1.21.0 的 `RedisConfig.StorageGrace` 已删）。`glsvet` 对 `game` 目录下的包直接读 `time.Now` / `Since` / `Until` 打印 `hint:`，系统时间写 `//glsvet:system-clock <理由>` 豁免。第八轮起 match 票据、chat 展示给玩家的时间（新字段 `SentAtUnix`；保留期仍按系统时钟）、account 的创建与登录时间也走业务时钟，`roost project doctor` 检查同一套部署各服务的 `time.logic_offset` 是否一致。详见 [§10 业务时钟与系统时钟](#业务时钟与系统时钟)。[方案](feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md)

## 2026-10-06 配置数据规则在加载层强制、热更失败可见（B10 / C2，main，未发版）

tablegen 标签（`required` / `unique` / `min` / `enum` / `ref`）与 cfggen 的同名选项变成一组 `configdata.FieldRule`，由 configdata 在每次加载与 reload 时对原始 JSON 检查——直接改 `configs/data` 再 `gm.config.reload` 也绕不过，删掉一个 required 列会被点名拒绝（表 / 行 / 字段 / 规则），旧快照保持；`roost generate` 与 `tablegen -check` 用同一个检查器提前反馈。热更失败（含 build 阶段）与撤回都留 Warn 日志并计入 `configdata.reload.total{result=failed}` / `configdata.rollback.total{trigger}`，`reason` 不再作指标标签。新生成的代码需要下一个版本的 core。详见 [§10 配置数据](#配置数据规则热更与可见性)。[方案](feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md)

## 2026-10-05 配置严格读取与生产校验范围（A4 / C1，main，未发版）

框架读取的布尔、时长、整数配置一律严格：`on` / `yes`、不带单位的时长（`ttl: 15`）、`8k` / `1.5` / `10s` 这样的整数，App 启动时（`ValidateServiceConfig`，任何 Mod Init 之前）点名报错，不再静默读成 false / 纳秒 / 0（取默认）。自己写 Mod 读配置时用 `app.ConfigBool` / `ConfigDuration` / `ConfigInt`，一次读多个键用 `app.NewConfigReader(cfg)` 读完再看 `Err()`。`env: production` 只校验有读取方的设置，原先要求的 `player.login_auth_required`、`player_protocol.rate_limit.enabled`、`save_load.wal.*` 等开关已删除——它们从来不控制任何行为，生成的游戏服接入层既没有按请求限流也只有演示凭据，上线前要自己接入。详见 [§10 配置写法与启动校验](#配置写法与启动校验)。[A4 方案](feature/REFACTOR-2026-10-05-strict-config-reads.md) · [NC-192](bugfix/RR-20261005-NC-192.md)

## 2026-10-05 驱动重放与超时契约（A2，main，未发版）

Redis 写命令（含脚本、含写的 pipeline、DistLock）不再由驱动在回复丢失后重放：错误就是“结果未知”，命令可能已经执行。要重试，先让这次写可以安全重复执行（请求 ID、版本 CAS、值守卫令牌），或者先回读再裁决。只有错误证明命令没执行时（拨号失败、池超时、LOADING 等），驱动才会自己重发；需要在业务里做同样判断时，用 `redis/driver.IsDefinitelyNotExecuted`。读命令照常重试。写命令的返回值（SETNX 的 bool、各种计数）只在没有错误时可信。Mongo `WithTransaction` 返回的错误满足 `errors.Is(err, mongo.ErrCommitResultUnknown)` 时，表示提交已经发出、可能已经生效，要按持久回执裁决；不带这个哨兵的错误都是确定没提交。完整的驱动行为契约（会重放的命令、错误分类、ctx 替换、默认超时）见 [redis/driver/README.md](../redis/driver/README.md) 和 [mongo/driver/README.md](../mongo/driver/README.md)，新增调用点时先对照它们。[方案](feature/A2-DRIVER-REPLAY-CONTRACT-2026-10-05.md)

## 2026-10-05 HTTP 响应完整性与 TCP 接入停机（main，未发版）

`httpserver.JSON`（Webroute `WriteResult`、Ops 管理面同一出口）先完整编码再写状态：值里有 NaN/±Inf、channel/func 或 MarshalJSON 报错时回 500 `{"error":"encode response","ok":false}` 并记日志，而不是 2xx 空体（NC-80）。业务此时可能已经执行，客户端按幂等键核对再重试；从源头避免不可编码值（如 0/0 胜率）。Engine 的 recover 只在响应开始之前回 500；写过头/体、Flush 或 Hijack 之后 panic 会中止连接，`panic(http.ErrAbortHandler)` 保持标准库语义（NC-81）。handler 拿到的 `w` 是包装 writer，`http.Flusher`、`http.ResponseController`（Flush 的错误照常返回）和（原 writer 支持时）`http.Hijacker` 照常可用，`http.Pusher` 不透传。

生成的 player TCP 接入：Stop/StopWithContext 超时后保留 server，用新 context 再调会等到所有连接 goroutine 真实返回；会话关闭订阅者的等待也受 ctx 约束（NC-83）。升级生成器后重新生成 `internal/access/player/tcp/server_gen*.go`。不配合 ctx 的认证回调/handler 不会被强行终止，只会让停机如实超时。

Ops `/admin/execute` 给命令的 ctx 带 `ops.admin_timeout`（缺省 10s）的期限，写超时 = max(15s, admin_timeout + 5s)：配合 ctx 的命令到期回 **504**（`its effects are unknown`），按 trace_id 核对后再决定是否重试；不配合 ctx 的命令仍可能跑过写超时、客户端只看到传输错误——同样是“结果未知”，不等于没执行（2026-10-06，[方案](feature/OPS-ADMIN-TIMEOUT-2026-10-06.md)；之前没有期限、写超时固定 15s）。`ops.enabled` 时 Ops 在 Start 里 bind `ops.addr`，端口被占用即启动失败，同机多实例须各配各的端口（[NC-230](bug/RR-20261005-NC-230.md)）。Mod 的停机之外，`service.stopping` / `service.stopped` 的 lifecycle hook 也在 `shutdown.total_timeout` 内等，hook 不配合 ctx 时 App 按停机不完整保留依赖、不释放单实例锁（[NC-231](bug/RR-20261005-NC-231.md)），退出错误点名卡住的 hook（`lifecycle service.stopping hook "<名字>" did not return …`，[RR-20261006-25](bug/RR-20261006-25.md)）；停机期间发生的 fail-stop 让进程非零退出（[NC-232](bug/RR-20261005-NC-232.md)）。`gateway.RateLimit`/`security.RateLimiter` 每个主体最多 `MaxKeysPerOwner`（默认 256）个 key，单个主体变化 MessageID 只会用完自己的名额；共享表（`MaxKeys`）满时陌生 key 立即拒绝，闲置名额最多晚一个 `SweepInterval` 回收；有协议注册表时把“拒绝未注册 MessageID”放在 RateLimit 之前（[NC-82](bug/RR-20261005-NC-82.md)）。[本轮](review/REVIEW-2026-10-05-noncore-n02.md)。

## 2026-10-05 App 单实例锁（main，未发版）

`singleton.enabled=true` 的服务在任何 Mod Init 之前先拿 `<key_prefix>:<server_type>:<sid>` 的锁，别人持有就等，失锁即 fail-stop，全部 Mod 停完才释放；`RuntimeFailure.OnFail` 让任何 fail-stop 先围栏 Nest（Remote Entity fatal 从此也会围栏，行为变化）；`app.ModSingleton` 提供只读的 `Live` 活性查询。配置、行为与约束见 [§2 单实例锁](#单实例锁singleton)。codegen 生成装配与配置（方案第 2 笔）：项目有 redis Mod 或带 dataengine 的服务时 bootstrap 安装 `kitredis.SingletonStore`，带 dataengine 的服务默认 `singleton.enabled: true`。

## 2026-10-04 Mongo替身事务与Redis锁接入（main，未发版）

NC-26～29现已修：BSON.D dotted路径可读写且保留兄弟字段，unique建立拒绝已有重复，非法bulk Type在写前拒绝。事务ctx使用私有快照，事务外Lookup/Documents/Seed只见已提交数据；集合粒度冲突可重跑callback，callback须幂等并等待自身操作结束。finished ctx不能留给后台继续写，事务内EnsureIndexes明确ErrUnsupported。完整Mongo索引/数组路径、Drop/namespace并发、未知commit/HA不由替身证明。[限制与消费者](bugfix/RR-20261004-NC-29.md)。

Redis普通锁无fencing；uncertain要按token校验清理再复用。AutoExtend生命周期不绑定Acquire请求ctx，长任务保留wrapper并检查Err、最终Release；Err不是下游写权限证明。Subscribe返回不代表就绪或可靠投递，保存并独立Close每个subscription。[14新场景与学习](review/IMPLEMENTATION-REDIS-LOCK-RENEWAL-AND-PUBSUB-LIFETIME.md)；真实弱网/重连/Cluster/长期容量仍未验。

## 2026-10-04 未知Lua写与Mongo替身（main，尚未发版）

RefHMap全量Set和Patch遇Eval错误均不自动重放，原始原因保留errors.Is；错误不证明数据未应用。不要无条件DEL补偿或忽略超时，应按业务的权威读回/现有版本能力确认再决定恢复。普通Stale检查仍是建议性，不新增CAS/原子Get。Lua不支持的adapter需实现已有Eval接口；旧write_degraded_total和降级告警已移除，查看Set错误与T-206。[NC-21/正式生成消费](bugfix/RR-20261004-NC-21.md)。

公开mongotest现在隔离嵌套BSON读结果/快照/写工作副本，_id $in每物理文档一次，整数与有限float精确比较，ReturnAfter返回同一更新/插入身份；非有限float明确unsupported。并非服务端Mongo认证：此前[D路径、唯一索引建立、非法bulk Type、并发全库rollback](bug/REVIEW-2026-10-04-noncore-16.md)四个P3已在第九批修复，使用这些场景验证业务时不能把替身成功作为上线证明。[修复矩阵](review/REVIEW-2026-10-04-noncore-15.md)。

## 2026-10-04 RefHMap类型、Patch与名称（main，尚未发版）

RefHMap支持struct/单层指针根（含命名指针），nil根在KeyOf前返ErrRefHMapUnsupported；指针TextMarshaler用于值根和Patch，编码错误不写数据，历史错误编码不会自动迁移。仅scalar本身地址副本，不承诺任意引用对象深拷贝。[16](bugfix/RR-20261004-NC-16.md) · [17](bugfix/RR-20261004-NC-17.md)。

Patch要求已有root，nil嵌套父可原子创建引用；单一同槽Lua先检查路径键类型，再维护叶、registry及路径祖先TTL，不续旁支TTL。缺root明确报错，Eval错误不自动重放；不提供版本CAS或原子Get快照，依赖敏感版本裁决时使用现有权威版本能力。[18/生成消费](bugfix/RR-20261004-NC-18.md)。

内部root物理键碰撞、根__keys、同hash字段重复和冒号/换行存储名称在I/O前拒绝；合法格式保持。旧非法布局读/删也被拒，需导出现有确定键和布局后显式迁移，不自动删/改业务数据。[19](bugfix/RR-20261004-NC-19.md)。公开mongotest分页已统一排序→Skip→Limit，但并非真实Mongo/事务隔离认证，当前[新替身问题](bug/REVIEW-2026-10-04-noncore-16.md)仍需处理。

## 2026-10-04 缓存准入与旧写结果（main，尚未发版）

ReadThrough配置FatalRemoteError后，Get遇fatal不会调用loader/发布L1，Delete遇fatal保留L1并返回原错；普通故障仍按IgnoreRemoteError策略处理，strict删除仍清L1并返回错误。Layered对L1明确stale/conflict拒绝不返回捕获旧值，而读取已准入当前值；stale且miss保持miss，conflict且miss/读回失败明确报错，拒绝不续TTL。正常TTL缓存与普通回填可用性故障保持既有行为。[NC-13](bugfix/RR-20261004-NC-13.md) · [NC-14](bugfix/RR-20261004-NC-14.md)。

Local、Grouped、RedisRawJSON、RedisJSONHash的Stale拒写现在返回ErrStaleWrite，和Atomic/RedisJSON一致；仅当业务有意容忍旧写时用errors.Is显式处理，不忽略所有错误、不无条件重试。正式生成Redis DAO透传该错误。公开接口/存储格式保持；Get→Stale→Set不是Redis原子CAS，正确性依赖版本裁决时使用正式CompareAndSet能力。[NC-15/实测与兼容](bugfix/RR-20261004-NC-15.md)。

## 2026-10-04 etcd setup与关闭责任（main，尚未发版）

Campaign的caller取消/期限覆盖session创建与竞选等待；成功取得领导权后，原caller取消不结束长期session。真正的session loss/Resign仍结束领导权，敏感写必须校验fence。取消停止等待/keepalive不代表服务端租约已即时撤销，未知结果不要自动重试副作用；Resign按caller期限返回：期限先到时返回ctx错误、本地领导权已结束，lease撤销由election持有（自带5s截止，失败则等TTL），下一次Campaign等它结束（[NC-93](bugfix/RR-20261005-NC-93.md)；修复前SDK Revoke可能TTL级等待）。[NC-11](bugfix/RR-20261004-NC-11.md)。

WatchCallback.CloseWithContext取消/超时只结束本次等待，同一subscription继续承担handler和底层watcher收尾；Done关闭才代表它们实际退出，可用新预算再次等待。底层watcher.Close错误现在由完成后的Close/CloseWithContext返回并通过Err保留，handler失败仍通过Err提取；干净显式关闭保持nil。第三方watcher/handler必须最终退出，callback内不无期限等自己的Done，应使用可取消等待，由外部最终排空。[NC-12](bugfix/RR-20261004-NC-12.md)。

## 2026-10-04 RPC 协议与停止预算（main，尚未发版）

JetStream无handler拒绝也使用版本1 response envelope，保持远端错误status，不按本地ErrNoHandler做errors.Is。ServiceRPC.CallDiscoveredChecked的配置timeout覆盖发现、picker与传输，较短父期限保留；独立PickServer仍使用调用方ctx，timeout/未知结果不代表业务未执行，不能自动重试副作用。

RPCClient提供可选StopWithContext；取消仅结束caller等待，同一实例的唯一停止任务继续负责pending、终态callback和pool排空。Assembly.Close与KitNats保留未完成资源，使用新预算再次等待同一对象；例外是连接drain失败或超预算：连接此时已被硬关闭，Assembly.Close返回包裹原错误的终态`natsdriver.ErrClosedUndrained`（仍可`errors.Is`原ctx错误），KitNats报告该错误并释放引用，再次Stop返回nil（[RR-20261004-08](bugfix/RR-20261004-08.md)）。Close之后经nats驱动的调用（以及过了入口检查、Close先完成的调用）一律返回可`errors.Is`到`fnats.ErrClosed`的错误，`Connected()`为false；判据是驱动自己的已关闭状态，不看nats.go的连接状态（[nats/driver README §5](../nats/driver/README.md)）。业务callback必须最终退出；callback内不要用Stop/Background等待自身，采用可取消等待并由外部生命周期最终排空。旧Stop依然无期限，重复Stop现在等待同一收尾任务，不再直接返回。[三项修复与验证限制](review/REVIEW-2026-10-04-noncore-07.md)。

## 2026-10-04 请求边界（main，尚未发版）

AllowN 超过实际 burst 的需求直接拒绝，不占 key、刷新 idle 或计作 key 容量拒绝；需要有效需求或显式 GC 维护活性。Gateway Recover 返回固定 ErrEndpointPanic，即使报告回调或失败日志 panic；同步报告仍需业务保证不永久阻塞。

Codegen 与运行期 Registrar 用同一套 chi 模式语法校验（运行期 `webroute.ValidatePath`；生成器不能 import core 运行时包，内部 `validateChiPath` 直接调用 chi，两处是同一解析器的两次调用，见 RR-20261004-NC-07 复核后的补修），坏路径在写生成物前拒绝，旧生成物注册也返回 error。正常参数/正则/通配符及生成形状保留，基础路径错误文本兼容；自定义 installer 半安装后的 error 不代表外部 router 已回滚，应重建。注册用于启动阶段，不与请求并发热改 router。[三项修复与证据](review/REVIEW-2026-10-04-noncore-05.md)。

## 2026-10-04 Manager / Admin / Ops 边界（main，尚未发版）

同一 Manager Engine 在确认 Provide 后只允许一次启动尝试；再次调用、Order/Start 失败后的重试或 Stop 后调用返回 ErrStartState（Kit 的 ErrManagerStartState 同值）。需要新生命周期时新建 Engine，并按业务契约准备 manager；没有 Provide 的前置失败可以补装配后启动。Stop panic 会转换为 error，清理其他对象并保留原因，但出错对象须自行保证资源退出。

MetadataRegistry 在 Register/Get/List 复制 JSON schema 容器；非 JSON 自定义对象须不可变，注册过程中不要并发改输入。Ops Shutdown 取消/超时后保留 server 供再次排空，active/draining 时 Start 返回错误；成功排空后才可启新 server。[四项修复/兼容记录](review/REVIEW-2026-10-04-noncore-03.md)。

## 2026-10-04 App / HTTP 使用边界（main，尚未发版）

只有一个 Mod 时也会执行名称、依赖和环校验；不要依赖单 Mod 跳过缺失依赖检查。HTTP `Clone(WithTimeout(...))` 对库创建的 client 生效，并保留父实例配置；显式 `WithHTTPClient` 的 Timeout 优先，由调用者配置，Clone 不会重配外部 client。Transport 仍可共享连接池。

`DoJSON` 的非 2xx 响应会保留 `StatusError`；读取或解码也失败时两类错误通过 Join 返回，调用者应使用 `errors.As` / `errors.Is` 分类。`BindJSON` 必须接收完整、单个合法 JSON，尾随垃圾或第二个 JSON 返回 400，不调用业务；合法尾随空白和现有空 body 零值行为保持。详见 [四项修复记录](bugfix/README.md) 与 [实际机制](review/IMPLEMENTATION-APP-AND-HTTP-BOUNDARIES.md)。

本文面向熟悉 Go、网络服务和数据库的开发者。示例版本基线：core/kit v1.8.0、skill v1.7.0、codegen v1.7.0。

## 1. 先理解边界

框架是一个 module（`github.com/tjbdwanghaibo/roost-core`），分四层：**仓库根部**定义稳定契约、调度和一致性语义，并承载 MongoDB、Redis、NATS、etcd 客户端、WAL、传输、常用玩法执行器与确定性技能程序（`skill/`）；**`kit/`** 负责配置解析与 Mod 装配，并托管通用游戏服务（`kit/service/`）；**`codegen/`** 生成工程、DAO、Entity、Nest、协议、配置和部署文件；**`demo/`** 是可运行的模板。业务仓库负责协议认证、玩家会话、具体组件、handler、玩法规则和容量参数。

推荐依赖方向：

```text
接入层 -> 生成 Sender -> Nest handler -> Entity/Component/DAO
                                      -> Remote Entity/Saga（需要跨域时）
DAO mutation -> Nest transaction -> Data Engine WAL -> Mongo projection/outbox
Entity mutation -> entitysync.Manager（按会话组帧、逐帧准入）或 lockstep -> 客户端
```

## 2. Service 与 Mod

一个二进制可注册多个 Service，通过 `server_type --sid` 选择启动实例。SID 是进程身份，也是路由和 fencing 的一部分；同一运行环境不能出现两个活跃 writer 共用 SID。

Mod 生命周期为 `Init → Provide → Start → StopWithContext`。硬依赖使用 `DependsOn`，存在时才需要排序的集成使用 `OptionalDependsOn`；框架拓扑排序，不要求业务记忆书写顺序。业务从 `app.Registry` 通过 capability 名称和目标类型获取依赖，不保存 kit 私有对象。

常用组合：

| 场景 | 建议 Mod |
| --- | --- |
| 无状态网关 | configdata、etcd、nats、ops、gateway |
| 普通实体服 | mongo、nats、dataengine、nest、ops |
| 跨服实体 | 普通实体服 + sync、remote_entity |
| 长事务协调器 | mongo、nats、dataengine、saga、ops |
| 状态同步 | player 接入层或 nettransport 作为 `entitysync.Transport`；业务装 `entitysync.Manager`，`policy.Interest / Group / Direct` 作为订阅政策（Manager 因传输失败丢掉会话而观察者仍在时，重开会话后 `Interest.Resubscribe` 恢复其订阅，见 RR-20260926-40；实体卸载后重载不了被框架退回 remove、之后重新登记时，三种政策自动重新提交仍持有的订阅，见 RR-20260926-70） |
| 确定性帧同步 | lockstep + KCP/QUIC/UDP transport |

Saga恢复与健康：三个持久消费者（普通完成、Nest启动意图、原生Nest完成）都是必需资源，任意一个关闭会使Saga健康项fail（NC-37）；不要仅凭协调循环运行判断可用。Resume代际由Mongo `incarnation` 保存，恢复后重载派发使用r1/r2命令身份（NC-38）；历史缺字段为0，参与写入的协调器应统一升级，旧writer完整Replace可能丢新字段。没有自动修复历史waiting/回执，不删除回执或修改版本来绕过冲突；按业务结果与既有超时/Resume流程恢复。[健康](bugfix/RR-20261005-NC-37.md) · [持久兼容与处置](bugfix/RR-20261005-NC-38.md)。

### 单实例锁（singleton.*）

同一服务类型 + sid 同一时刻只让一个进程跑 Mod。它只针对**同一 sid 崩溃重启时短暂出现两个进程**：旧进程卡住（SIGSTOP、长 GC、调试器）或没完全退出，新进程已被拉起。跨主机 / 换卷并存、网络分区、Redis failover 丢键不在保证范围内（[方案](feature/APP-SINGLETON-LOCK-2026-10-05.md) §1）。模块不感知锁、也不应各自检查：fail-stop 由 App 统一触发。

接入（codegen 生成的 bootstrap 已包含，手写装配时照此添加）：

```go
a := app.New(name, version)
a.Singleton(kitredis.SingletonStore) // kitredis = github.com/tjbdwanghaibo/roost-core/kit/redis
```

codegen 生成规则（方案 §6.3）：
- bootstrap：项目里有 `redis` Mod，或有带 `dataengine` 的服务，就生成 `a.Singleton(kitredis.SingletonStore)`。是否启用由各服务配置决定，`enabled=false` 时 opener 不会被调用；既没有 redis 也没有 dataengine 的项目要打开 singleton，先给某个服务 `roost add mod redis`，bootstrap 随之安装（生成的 bootstrap 不要手改）。
- 服务配置：resolved mods 含 `dataengine` 的服务写 `singleton.enabled: true`（`key_prefix: roost:<project>:singleton`，15s / 3s / 5s / 30s），同一文件没有 `redis:` 段时只补配置段、不强加 Redis Mod；其他服务写同一段的 `enabled: false`，按 sid 多副本部署与否生成器不知道，由应用自己打开。之后 `add mod` 让服务带上 dataengine 时，未改过的 `enabled: false` 段翻成 `true`，改过的段保持并 WARN。
- 停机预算：启用的服务 `shutdown.total_timeout` 计入 Release 的 3s（game-demo game 服务 111s / 宽限 116s）；`roost project doctor` 对 `singleton.enabled: true` 的配置同样计入。
- 启动等待：启用的服务 k8s `startupProbe` 覆盖 `startup_wait + 30s`（默认 60s，阈值不变），shell 部署 `HEALTH_ATTEMPTS` 默认 `startup_wait + dataengine.startup_timeout` = 60 次（其他服务 30），compose `start_period` 60s。调大 `startup_wait` 时同步调大这几处。

```yaml
singleton:
  enabled: true                       # 默认 false：不建连接，行为与不装完全相同
  key_prefix: roost:<project>:singleton  # 启用时必填，不能含空白
  ttl: 15s
  renew_interval: 3s                  # 续期节拍；单次续期的超时取这个值，持有期间不越过 validUntil
  guard: 5s                           # 本地窗口提前于键过期结束的量
  startup_wait: 30s                   # 缺省 2 × ttl
redis:
  addr: 127.0.0.1:6379                # 或 cluster_addrs；启用 singleton 时必须显式配置
```

- **后端**：`kitredis.SingletonStore` 从同一份 `redis.*` 建两个独立的小客户端（不依赖 Redis Mod）：一个只做 CAS（获取 / 续期 / 释放），一个只做 `Live` 的读，Redis 变慢时并发的 `Live` 占满自己的连接也不会让续期等连接、误判失锁。CAS 复用 `redis.CompareAndSet` / `CompareAndDelete`；缺 `redis.addr` 与 `redis.cluster_addrs` 时启动失败（不沿用 Redis Mod 的 `localhost:6379` 兜底）。`enabled=true` 而 bootstrap 没调 `Singleton` 时启动失败，错误是 `app.ErrSingletonOpenerMissing`。
- **写法**（`ValidateServiceConfig` 校验，RR-20261005-NC-190）：`enabled` 只接受 `true` / `false`（以及 `1` / `0`、`"true"`），`on` / `yes` / `off` / `no` 启动失败——宽松读取曾把它们读成 false、静默关掉锁；四个时长必须带单位（`15s`、`500ms`），不带单位的数字启动失败，不再当作纳秒。框架读取的其他布尔、时长与整数键同样严格（A4，见 [§10](#配置写法与启动校验)）；`redis.cluster_addrs` 可以写逗号分隔串或 YAML 列表。
- **时间关系**（`ValidateServiceConfig` 校验，违反即启动失败）：`renew_interval ≤ guard`、`2 × renew_interval ≤ ttl − guard`、`startup_wait ≥ ttl + 2 × renew_interval`。默认 15 / 3 / 5 / 30s 全部满足。
- **启动**：键被别人持有时等待，每 `renew_interval` 重试，持有者变化时打一条 `singleton: waiting for the current holder to release or expire`（带对方的值，含 hostname / pid）；等待期间什么 Mod 都不 Init。对方已经停完（键已释放）时新进程立即拿到；对方还在优雅停机时，新进程在对方释放后的下一个节拍拿到（最多再等一个 `renew_interval`，重试不监听删除）；对方崩溃或卡住时最多等 `ttl + renew_interval`（2026-10-05 真实进程演练：kill -9 / SIGSTOP 后实测都是 15.0s，见[方案](feature/APP-SINGLETON-LOCK-2026-10-05.md) §13 第 5 笔）。到 `startup_wait` 仍被持有返回 `app.ErrSingletonHeld`——对方一直在续期，说明两个健康进程配了同一个服务类型 + sid，是部署错误，App 不会抢锁；最后一次是报错 / 超时则返回 `app.ErrSingletonStoreUnavailable`。等待期间 SIGTERM 按默认处置直接终止进程（还没持有锁）。
- **持有**：一个 goroutine 按固定节拍续期（上一次成功的请求发出时刻 + k × `renew_interval`），窗口 `validUntil` 从请求发出时刻起算；回复迟到（晚于 `asked + ttl − guard`）不作数、立即再续一次。续期答“键已不是我的”，或续期失败且已到 `validUntil − guard`，即 `RuntimeFailure.Fail(app.ErrSingletonLost …)`：Nest 立即围栏、`Service.Shutdown`、Mod 逆序停、非零退出。代价是 Redis 连续不可用约 `ttl − guard`（默认 10s）以上时进程会退出重启；需要更宽容时调大 `ttl`。
- **释放**：只在全部 Mod 停完之后 `CompareAndDelete` 自己的值（停机路径用 `min(shutdown 剩余, 3s)`；启用时 App 把 Mod 停机的截止时间提前至多 3s 留给它，部署的 `shutdown.total_timeout` 应相应加 3s；进入停机时已失锁则不会释放，这 3s 留给 Mod 停机）。`Service.Shutdown` 超时、服务专属或共享 Mod 停机不完整、失锁三种情况不释放，键在 `ttl` 内自然过期；启动失败（Mod Init / Provide / Start、`Service.Init` 失败）在已启动的 Mod 停完后释放，启动期间失锁则同样停 Mod、不释放；`Service.Init` 失败及之后的启动失败先调用 `Service.Shutdown`（限时 5s，收回 Init 已经启动的部分，所以 Shutdown 要容忍部分初始化），没在时限内结束就不停 Mod、不释放，键在 `ttl` 内过期（RR-20261005-NC-193）。后端连接在全部 Mod 停完（或还没启动任何 Mod）时关闭；停机不完整时 `run` 不等仍在跑的组件、保留它们的依赖，后端连接也一样留到进程退出，这些组件调用 `Live` 不会读到 client closed。
- **约定**：拿锁之后、DataEngine 打开 WAL（`flock`）之前启动的 Mod 不应有按 sid 的外部写——旧进程卡住超过 `ttl` 时，新进程会拿到锁、启动这些 Mod，然后在 `nestwal: directory is already locked` 处退出（T-213）。
- **fail-stop 统一围栏**：`RuntimeFailure.OnFail(hook)` 登记首次失败时的回调（恰好一次、按登记顺序、在调用 `Fail` 的 goroutine 上同步执行，执行完才唤醒 `run`）。kit Nest Mod 登记了 `NestMgr.Fence`，所以失锁、DataEngine fatal、Remote Entity fatal 都会立即拒绝新的和排队中的派发（`nest.ErrNestFenced`）。回调必须快速、不阻塞、不做 I/O。启动期间发生的 fail-stop 让 `run` 停在下一个阶段边界，不再启动后面的 Mod。
- **本次启动的身份**：`app.Lookup[app.SingletonIncarnation](registry, app.ModSingletonIncarnation)` 给出 `Key`（`<key_prefix>:<server_type>:<sid>`）、`Sid` 与 `Token`（锁值第一段，每次启动不同），只在启用时登记，Mod Init 时锁已持有。框架模块据此接管上一代同 sid 进程留下的协调状态：目前是 Remote 实体共享锁（O-M6-6，见 Remote 一节）。
- **活性查询**：`app.Lookup[app.SingletonLiveness](registry, app.ModSingleton)`，`Live(ctx, serverType, sids)` 返回其中持有锁的 sid（按入参顺序，一次最多 200 个）。“活”= 进程持有锁：从任何 Mod Init 之前到全部 Mod 停完，崩溃的进程最多再算 `ttl`。**停机中的进程仍算活**（维护者决定 C5）：收到停机信号、`Service.Shutdown`、各 Mod Stop 期间键都在，到 Release 删键为止都算；停机不完整（不 Release）时算到键过期。用它决定“该等谁”的调用方要接受这一点——activity 开窗时可能把正在停机的服算进 expected，它不会再通知，那个窗口要等到宽限期（`activity.grace_window`）才完成，只影响停机那几秒里开的窗口。只能看见开了 `singleton.enabled` 的服务类型，且要求查询方与被查方共用同一个 Redis 与 `key_prefix`；`serverType` 传自己的 `server_type`（`registry.Config().GetString("server_type")`），不要写死。`enabled=false` 时不登记，依赖它的模块应在 Init 报错。kit `service/global` 原有的游戏服租约 API（`AcquireLease` / `RenewLease` / `ReleaseLease` / `Lease` / `LiveGames`）已删除，进程存活统一用这里的 `Live`。
- **game-demo 的用法**：activity 开窗时的预期集合是 `Live(本进程 server_type, 本服所在活动组的成员)`（活动组见下一条），取不到 `app.ModSingleton` 时 activity 启动失败（要求 game 的 `singleton.enabled=true`）；查询为空时只等自己，报错时这一拍不开窗。同一部署的 game 进程要共用一个 `redis.*` 与 `singleton.key_prefix`；给 game 关掉 singleton 会让 `Live` 恒空、activity 退化为只等自己。玩家所有权（静态绑定）不读锁，见 [GAME_DEMO_TEMPLATE §9.15](feature/GAME_DEMO_TEMPLATE.md)。
- **服务指标默认落地**（维护者决定 C6，[方案](feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md)）：生成的 `internal/service/<svc>/collaborators.go` 里 `Metrics()` 默认返回 `servicemetrics.NewMetricsReporter("<svc>")`，服务的接受 / 拒绝 / 重放 / 丢弃 / 冲突 / 深度事件以 `service_*` 出现在 ops `/metrics` 上（指标与标签见 OBSERVABILITY.md“服务事件”）。关闭：`service_metrics.enabled: false`（严格布尔，默认 true），或让 `Metrics()` 返回 nil。match 队列的 Partition 与 rank 的看板 ID 会成为 `key` 标签，必须是有限枚举。已生成的工程不改，想用默认 Reporter 时把 `Metrics()` 改成上面那一行。
- **活动组文件**（维护者决定 C4，[方案](feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md)）：参与同一个全服活动的 game sid 集合由一个文件定义，托管 activity 协调器的工程生成 `configs/activity_groups.yaml`（只创建一次，之后归项目）：
  ```yaml
  groups:
    - id: mygame              # 协调器 Key.GroupID；非空、不含 /、不重复
      game_sids: [1000, 1001] # 每个 game 的 --sid；每组至多 64 个，一个 sid 只属于一个组，正的 int32
  ```
  协调器读 `activity.groups_file`（**必填**，2026-10-06 起：缺失或为空时 activity Mod 的 `Init` 拒绝启动，错误点名键名与生成器的默认路径 `configs/activity_groups.yaml`；生成的协调器配置已写好这个键；启动时校验文件，`activity.sweep_groups` 为空时后台 sweep 扫文件里的全部组，显式写了 `sweep_groups` 仍以它为准；每次 `OpenActivity` 都按文件核对：Key 的组必须在文件里、expected 里每个 sid 都必须是该组成员，否则 `ErrInvalid` 点名文件 / 组 / sid，RR-20261006-17）；game-demo 的 game 读同名键，按本服 sid 找组，组 id 就是开窗 / 贡献 / 结算用的组，组成员就是 `Live` 的候选。校验只有一份（`kit/service/global/activity.LoadGroupsFile`）：每组成员数上限就是协调器单个窗口 expected 集合的上限 `activity.MaxExpectedGames`（64）；组内重复、一个 sid 在两个组、非正数或超出 int32、未知字段、game 的 sid 不在任何组里，都在启动时点名文件报错。列出没在跑的 sid 无害（`Live` 只算活着的）。文件按相对路径解析：开发时在工程根目录，镜像里由 Dockerfile 拷到 `/app/configs/activity_groups.yaml`，shell 部署由 `install.sh` 装进 release；某个环境要不同分组时把文件挂到这个路径，或把两边的 `groups_file` 指到挂载位置。旧的 game 配置键 `activity.game_sids` 已删除，已生成的工程不迁移（见 CHANGELOG）。
- **与 etcd Discovery 的分工**：kit etcd Mod 的 Discovery（`<etcd.service_prefix><server_type>/<sid>`，`etcd.lease_ttl` 秒的 etcd 租约）只做**地址 / 元数据发现**，不承担存活语义；“这个 sid 有没有进程在跑”以 `App.Live` 为准。Discovery 的 Mod Start 在拿锁之后，所以同一 sid 同一时刻只有一个进程注册；崩溃的进程的注册最多残留 `lease_ttl` 秒，卡住的进程恢复后会看到 `etcd Discovery: lease lost`（并在停机时报 `revoke: requested lease not found`），这只是旧租约过期的伴随现象。`service_prefix` 要以 `/` 结尾（Discovery 不补分隔符；codegen 2026-10-05 起生成 `/roost/services/`）。
- **观测**：`/readyz` 多一项 `singleton`：持有为 ok，窗口内续期结果未知为 degraded，失锁或未持有为 fail（`/healthz` 不受影响）。degraded 算就绪（维护者决定 D1）：续期结果未知的窗口里 `/readyz` 仍返回 200，响应体 `degraded: true`、`degraded_dependencies` 里列出 `singleton` 与原因；只有 fail 让它 503。
- **不要自建进程级单例**：kit 不再以 capability 发布 `redis.lock`（`mods.ModRedisLock`）与 `etcd.election`（`mods.ModEtcdElection`），两个常量已删除（破坏性变更，方案 §12 / 第 2b 笔）。core 的 `redis.IDistLock`、`etcd.IFencedElection` 仍在，只用于 cron 去重、按键选主这类键级用途，需要时用 `redis/driver.Assemble(cfg).Locks`、`etcd/driver.Assemble(cfg).Election` 自行装配。

## 3. Entity、Component 与 DAO

Entity 是锁、生命周期和路由的边界；Component 是领域能力；DAO 是持久化与同步状态。聚合加载必须完成所有 DAO、schema migration、版本向量恢复后再一次性发布 Entity，不能暴露半初始化对象。

DAO 字段由 codegen 改为私有存储，读取和修改都走生成方法。写方法在当前 Nest 事务中登记 undo、标记 persist/sync dirty、生成字段级 patch。Map 使用框架生成的受控容器，避免业务获得内部引用后绕过 dirty tracking。

不落库的实体（`noPersist=true`，例如刷出来的怪）也用 DAO 承载位置等状态，保证“位置在 DAO、经组件读写”对所有实体一致；这类 DAO 写 `//roost:dao nocoll`：全部字段必须 `nopersist`，生成物只有读写方法、回滚快照与 `MarshalSync` 复制，没有集合名常量与任何 Mongo 读写 / 迁移 / 加载路径，DaoManager 以 `<Dao>RegistryKey` 登记。持久实体或 `remote=managed` 实体使用它会在生成期报错。详见 codegen 参考 §4.1 与 [方案](feature/DAO-NO-COLLECTION-2026-10-04.md)。

必须遵守：

- handler 进入前 Nest 已按全局顺序获取 Entity mutex；业务不再加同一把锁。
- 不把 Entity、DAO、可变 map/slice 指针带出锁作用域。
- 异步 goroutine 只接收不可变值或 snapshot，不能闭包捕获 Entity。
- ID 默认不复用；删除使用高版本 tombstone，旧 save/ACK 不能复活对象。
- **实体定时器（core `timer`）的触发顺序与类型**（维护者决定 D-L1 / D-L2）：同一期限的定时器先比 `Node.Priority`（**数值小的先触发**，缺省 0，用 `NewTimerWithPriority` 指定），再按登记顺序（节点 ID；改期、按返回值重排保留 ID）。宿主从存储重建调度器时顺序同样由这三个键决定，与存储遍历顺序无关；持久化节点时要把 priority 一起存下（game-demo 的 `TimerNode.priority`，旧节点缺字段按 0）。到期节点的类型没有注册 handler 时节点照旧删除，打 Warn 并计 `timer.unhandled_dropped_total{kind=<类型号>}`；宿主在加载、注册完 handler 后调 `ReportUnhandledTypes()`，对没有 handler 的存量类型每种告警一次。下线一种定时器类型前先确认存量节点已触发完，或接受它们到期即被丢弃（[方案](feature/D-L1-L2-TIMER-ORDER-AND-UNHANDLED-2026-10-06.md)）。
- **实体实现必须是指针**（RR-20260930-15）：Guard 按实例比较接口值，值类型实现（尤其含 func / map / slice 字段、不可比较的）会在同 ID 两个实例相遇时 panic。注册的 builder 构建出值类型实体时 `BuildEntity` 拒绝，手工构造的值类型实体在 `EntityManager.Add` / `TryAdd` 处拒绝（`errors.Is(err, entity.ErrEntityNotPointer)`，错误里点名类型）；生成实体都是指针，不受影响。

## 4. Nest 请求与事务

客户端只持有 codegen 生成的 Sender。同步请求等待返回值，异步请求只表示已进入受控队列；两者都必须经过 Nest，不能直接调用 handler。

回滚模式：

- `rollback=undo`：生成的 mutator 登记逆操作，适合改动字段少的高频请求。
- `rollback=state`：事务前保存状态，适合修改范围复杂或第三方组件无法生成 undo 的请求。

持久化模式：

- `memory`：只承诺内存提交，用于可重建临时状态。
- `async`：WAL durable admission 后可返回，后台批量落权威存储。
- `strict`：等待事务 WAL 和存储提交点，适合支付、跨服资产、唯一奖励。
- `pipelined`：高吞吐 group commit；外部可见结果仍受 durable watermark 约束。Kit 装配的 EntitySync
  （`kitnest.NewModWithEntitySync`）在业务未显式设置 `ManagerConfig.DurableWatermark` 时自动接 committer 的
  `DurableLSN`：committer 实现 `PipelinedTransactionCommitter` 时按水位暂缓未持久内容，否则不设门槛
  （RR-20260926-35）。显式配置优先；自建 `entitysync.Manager` 仍需自己接线，入口 `NestMgr.DurableWatermark()`。
  broadcast 与带 Remote 批次的消息不走 pipelined 的提前放锁，按 strict 在锁内等本地 WAL fsync，之后才释放锁、确认 Sync、
  执行 AfterCommit 与回复；这两条路径的锁持有时间因此包含一次组提交 fsync（此前 WAL 在这里不等 fsync，实际是 async 语义，
  [RR-20260928-11](bugfix/RR-20260928-11.md)）。

handler 内新建的实体属于当前事务：用当前 Guard 作用域调用 `CreateInScope`（`entity.CurrentGuardScope()`），
以及在 handler 内调用 `EntityManager.Create` / `ManagerAccess.Create`（`IsCreate`）——生成 Lifecycle 的 `Create`、`GetOrCreate`
的创建分支都经过它——都进入同一边界，
与动态 Cast 相同：纳入回滚 / 持久化参与者与 Sync 提交屏障，提交确认（pipelined 为 ticket 持久）之前不外发；
handler 报错、panic 或提交被明确拒绝时撤销发布——从 EntityManager 摘除并标记 removed、不写入持久化记录、
Nest 自己的 EntitySync 注销该 subject（已持有对象的会话收到 ObjectRemove），Guard 释放后调用
`OnDestroy(entity.DestroyReasonCreateRevoked)` 并回收 ID（与业务 `Destroy` 传入的原因、仅内存卸载的 `DestroyReasonMemoryUnload` 都不同；
之前是 `DestroyReasonCommon`，[RR-20260927-12](bugfix/RR-20260927-12.md)）。登记在业务自建 Sync Manager 上的 subject 需在 `OnDestroy` 里自行注销。
在 Nest 之外（登录 / 创角端点、spawner、Service.Init、独立 `WithGuardScope`）调用这些入口仍是立即发布的原语义；
Repository 聚合加载（`IsCreate=false`）不受影响。
RollbackState 下新实体的 DAO 需要可快照（生成 DAO 已满足），remote-managed 实体在 durable 事务内创建会被拒绝，
这两条与 Cast 的约束一致。这类事务捕获失败时 `Create` 撤销发布并返回该错误；业务即使吞掉它、handler 返回成功，
事务也整条回滚，调用方收到该错误（可 `errors.Is`，不按锁超时重排），committer 不被调用（[RR-20260927-11](bugfix/RR-20260927-11.md)）。
handler 内动态 Cast 取得实体后的事务捕获失败同样处理：`CastOne` / `CastMulti` 等返回该错误，业务吞掉它时事务照常整条回滚、调用方收到该错误、不重排，
Cast 已取得的锁在事务结束时归还（[RR-20260927-31](bugfix/RR-20260927-31.md)；之前业务吞错后事务照常提交）。
handler 内新建实体的锁持有到 handler 结束（memory handler 也一样，并进入本次 Sync 提交屏障），取锁遵循与 Cast 相同的锁序：
新实体的锁组高于 handler 已持有的全部锁组时等待；否则（与声明目标同组或更低组，最常见的写法）只尝试加锁，被其他 handler 占用时
`Create` 返回满足 `errors.Is(err, nest.ErrCreatedEntityLockConflict)` 的错误。可回滚（state / undo）的事务里它同时满足
`errors.Is(err, nest.ErrLockTimeout)`，事务整条回滚后自动重新准入——即使业务吞掉了这个错误；重排后排到同 ID 后继之后，
多次仍冲突时调用方收到锁超时（[RR-20260926-48](bugfix/RR-20260926-48.md)）。例外：同一消息里嵌套独立事务已经提交时消息不再重排，
回复同样两者并存但另带 `ErrNestedTransactionCommitted`，表示“已回滚外层、未重排、嵌套部分已提交”，按下文判别表处理。
不能回滚的 handler（rollback=none，即 memory 快路径）冲突前的内存修改不会撤销，所以框架**不**自动重排这条消息：
`Create` 的错误不带 `ErrLockTimeout`，请直接返回它，调用方收到 `ErrCreatedEntityLockConflict`；业务改返回别的锁超时类错误时，
回复同样补上该哨兵、不重排。是否重试由业务按 handler 的幂等性决定（[RR-20260926-64](bugfix/RR-20260926-64.md)）。
同一规则推广到不能回滚的 handler（memory 快路径，或带 Remote 批次的 memory handler）开始执行后的**任何**锁超时 / 组迁移类错误
（`ErrLockTimeout`、`ErrEntityLockGroupChanged`、`ErrEntityGroupTransitionPending`）：框架不重排，回复满足
`errors.Is(err, nest.ErrNonRollbackNotRequeued)`，原因仍可 `errors.Is`——此时 `ErrLockTimeout` 不再表示“未执行”，失败前的修改已生效。
handler 开始执行之前的准入失败（声明目标取锁超时、组锁被占、组迁移待定）照常自动重新准入。
动态 Cast 在等锁期间目标被 `Destroy` 或仅内存卸载时返回满足 `errors.Is(err, nest.ErrEntityNotFound)` 的错误（此前是 `ErrLockTimeout`），
可回滚的事务照常整条回滚、不重排（[RR-20260926-73](bugfix/RR-20260926-73.md)）。
handler 内先 `Destroy` 某个实体、再新建同 ID 的实体时，新实例拿到新锁，按上面的锁序规则取锁（同组即尝试加锁），持锁到 handler 结束并进入本次提交边界；
事务回滚时新实例按上文撤销发布，旧实例的销毁不回滚（`Destroy` 本身不是事务操作）。同一 handler 里若 `Destroy` 之后别处重建了同 ID、再 `Cast` 它，
返回 `nest.ErrCastDeadlockRisk`，不会拿到未加锁的实例（[RR-20260926-67](bugfix/RR-20260926-67.md)）。
生成 Lifecycle 的 `GetOrCreate` 在同 ID 的上一个实例正在撤销 / 销毁收尾（`entity.ErrEntityRemoved`）时最多再试两次，已生成的工程重新运行生成器即可获得（[RR-20260926-57](bugfix/RR-20260926-57.md)）；在 Nest handler 内这个窗口按新建锁冲突返回 `nest.ErrCreatedEntityLockConflict`（[RR-20260926-81](bugfix/RR-20260926-81.md)）。
handler 内新建时同 ID 的上一个实例正在撤销 / 销毁收尾（锁已释放、收尾回调未结束），与上面的锁冲突同样处理：`Create` 返回 `ErrCreatedEntityLockConflict`
（可回滚事务同时带 `ErrLockTimeout`、整条回滚后重新准入；不能回滚的 handler 不带、不重排），不再返回 `entity.ErrEntityRemoved`；
Nest 之外仍返回 `entity.ErrEntityRemoved`（[RR-20260926-81](bugfix/RR-20260926-81.md)）。
例外：同 ID 的上一个实例是**本 handler 自己**撤销的（例如 handler 内 `RunIsolatedTransaction` 新建后回滚，撤销收尾挂在同一个 Guard 上，
要等整个 handler 释放才执行），在同一 handler 里再建它是确定失败：`Create` 返回满足 `errors.Is(err, entity.ErrEntityRemoved)` 的错误、
文案说明是本 handler 内已撤销的同 ID，不带 `ErrCreatedEntityLockConflict` / `ErrLockTimeout`，消息不重排（之前可回滚事务会每次重排都重现、
空转到重排上限）。handler 结束后同一 ID 可以正常新建（[RR-20260927-21](bugfix/RR-20260927-21.md)）。

结果不确定时框架 fence 实例，不进行猜测性回滚。业务必须把“服务暂不可用”和“业务失败”分成不同错误码。

### 回复错误判别：是否可能已提交、能否重试

`Nest.Request` 等返回的错误可能同时满足多个哨兵（原因链保留），**只看有没有 `ErrAfterCommitFailed` 不够**：嵌套独立事务已提交时
回复是 `ErrNestedTransactionCommitted` 包着外层原因（可能正是 `ErrLockTimeout`），并不带 `ErrAfterCommitFailed`。
按下表**从上到下取第一个命中的行**（全部用 `errors.Is`，不要匹配文本）。规则：**带任一“可能已提交”哨兵（前 5 行）即不得重试**，
即使链上同时有 `ErrLockTimeout`；第 6、7 行“框架没有重排、修改未回滚”也不得盲目重试（[RR-20260926-77](bugfix/RR-20260926-77.md)）。

| # | 回复满足 `errors.Is(err, …)` | 含义 | 是否可能已提交 | 能否重试 |
| --- | --- | --- | --- | --- |
| 1 | `nest.ErrCommitIndeterminate` | 消息自己的事务或 handler 内嵌套独立事务的提交结果未知；引擎已 fence（RR-76）。消息自己的事务结束后、收尾阶段（Guard post-release、解锁后回调）调用的独立事务结果未知时引擎同样 fence（RR-84），但业务在收尾阶段已无法把错误带进回复：**回复里看不到本哨兵**，只能从之后的请求得到 `ErrNestFenced` 得知。handler 内嵌套独立事务结果未知、业务**吞掉**这个错误时，回复同样不带本哨兵：外层自己的事务有要持久的记录时，它在交给 committer 之前被拒绝，回复是第 5 行的 `ErrNestedTransactionCommitted` 加 `ErrNestFenced`（与 `ErrCommitRejected`），按第 5 行处理（RR-20260927-06、表后 `ErrNestFenced` 说明）；外层没有要持久的记录（含 memory handler）时回复是成功，同样只能从之后的请求得到 `ErrNestFenced` 得知 | **可能** | 不得重试；等实例从 WAL 恢复后按业务幂等键核对 |
| 2 | `entity.ErrRemotePersistenceIndeterminate`，或 `entity.ErrRemoteCommitTimeout` | 本地已持久提交，Remote 结果未知（RR-37）：Durability 0 远端回复丢失 / 未到达、strict 等待期间投影器报告未知、strict 等待 Remote 确认到截止，四种形态都带前者；strict 截止另带后者与 `context.DeadlineExceeded`（RR-20260927-24 起；之前只带后者）。strict 等待时结论被容量淘汰、重新登记报 `entity.ErrRemoteOverloaded` 的同样只带前者，落在本行（RR-20260928-08；之前不带、被误判为第 4 行）。判断时只看前者即可，列出后者是为了兼容旧版本与只返回后者的自定义 Remote 实现 | **可能** | 不得重试；结论由 Remote 后台收尾给出 |
| 3 | `nest.ErrAfterCommitFailed` | 消息自己的事务已提交，收尾（release hook、AfterCommit、Close、引用释放）失败（RR-46 / 53）。收尾阶段调用的 `RunIsolatedTransaction` 不算消息自己的事务，它的提交不会让回复带本哨兵（RR-84） | **已提交** | 不得重试 |
| 4 | `nest.ErrRemotePartRejected` | 带 Remote 批次的消息自己的本地事务已提交（strict 与随 strict 路径提交的 pipelined 已持久——后者自 RR-20260928-11 起等 fsync；memory 为内存提交），Remote 部分被明确拒绝（RR-20260928-03）。async 不走本行：自带 `remoteentity` 在 async 下 `Commit` 返回推测回执、不等 Remote 结论，回复时本地 WAL 也未 fsync，Remote 结论在回复之后才由 WAL 投影给出：只丢弃被拒绝 Remote 实体的修改（回滚、隔离并从权威重载，RR-58 / 62），本地部分照常生效，`AfterCommit` 不执行。原因（如 `entity.ErrRemoteRejected`、`ErrRemoteVersionConflict`）仍可 `errors.Is`。与第 2 行（Remote 结果未知）、第 3 行（Remote 也已确认）互斥 | **部分已提交**（本地部分） | 不得整笔重试（会重复本地部分）；按业务只补做 Remote 部分，或读回状态后再决定 |
| 5 | `nest.ErrNestedTransactionCommitted` | 消息自己的事务没提交，但 handler 内嵌套独立事务（或收尾阶段——Guard post-release、解锁后回调——调用的独立事务，RR-84）已提交或结果未知；消息未重排（RR-65） | **部分已提交** | 不得整笔重试 |
| 6 | `nest.ErrNonRollbackNotRequeued` | 不能回滚的 handler（memory）开始执行后遇锁超时 / 组迁移类错误；未准入，但失败前的内存修改已生效且不撤销，未重排（RR-73） | 未持久提交，修改未回滚 | 框架不重试；确认 handler 幂等（或读回状态）后业务自行重试 |
| 7 | `nest.ErrCreatedEntityLockConflict`，且不带 `ErrLockTimeout` | 不能回滚的 handler 内新建实体锁冲突，未重排，冲突前的修改未回滚（RR-64） | 同上 | 同上 |
| 8 | `nest.ErrCreatedEntityLockConflict` 与 `ErrLockTimeout` 并存 | 可回滚事务新建实体冲突：已整条回滚、自动重排仍冲突到上限（RR-48）。只在不命中第 1～5 行时成立——外层已有嵌套提交时同样两者并存，但那是“已回滚、未重排”，按第 5 行。同 ID 由本 handler 自己撤销的不在此列，见第 13 行 | 否 | 可重试 |
| 9 | `nest.ErrLockTimeout` / `ErrEntityLockGroupChanged` / `ErrEntityGroupTransitionPending` | 未提交：准入阶段失败，或可回滚事务已回滚；框架已自动重排到上限 | 否 | 可重试 |
| 10 | `nest.ErrNestedTransactionRollbackConflict` | `RunIsolatedTransaction` 要写外层可回滚事务已快照的实体，写持久记录前被拒绝并自身回滚（RR-74） | 否（嵌套事务） | 原样重试仍被拒；把写入并入外层事务 |
| 11 | `nest.ErrNestedTransactionInRemoteMessage` | 带 Remote 批次的消息里调用 `RunIsolatedTransaction`（批次挂到消息上之后：慢阶段准备、handler 内，或消息自己的事务结束后、批次收尾前的收尾阶段），函数体未执行（RR-75 / 84）。`PrepareRemoteWriteBatch` 执行期间批次还没挂上，不在此列（见表后说明） | 否（嵌套事务） | 原样重试仍被拒；把写入并入消息自己的事务 |
| 12 | `nest.ErrCommitRejected` | 提交在写任何持久记录之前被明确拒绝：committer / Enqueue 拒绝，以及 Remote 批次定稿（`FinalizeLocked`）拒绝（如 sid 作用域，RR-20260927-09）、准备提交记录失败、fence 之后不再交给 committer（同时带 `ErrNestFenced`，RR-20260927-06）——后几种从 RR-20260927-32 起同样带本哨兵，原因仍可 `errors.Is`。可回滚事务（state / undo）已回滚；不能回滚的 handler（带 Remote 批次的 memory handler，RollbackNone）失败前的内存修改**不撤销**（与第 6 行同一语义） | 否 | 看原因：`dataengine.ErrFencedEntityPending` 可重试，其余按业务错误处理；不能回滚的 handler 同第 6 行，确认幂等（或读回状态）后再重试 |
| 13 | `entity.ErrEntityRemoved`（handler 内新建） | 同一 handler 里再建本 handler 自己较早撤销的同 ID（撤销收尾要等 handler 释放），确定失败、未重排（RR-20260927-21）；可回滚事务已回滚 | 否（消息自己的事务；嵌套事务已提交时按第 5 行） | 原样重试会重复同一流程、再次失败；改业务流程，不在同一 handler 内重建刚撤销的 ID |
| 14 | `nest.ErrNestCanceled` / `nest.ErrNestTimeout`（`Nest.Request*` 返回：`Request`、`RequestMulti`、`RequestMultiGroup` 及经它们的生成调用，同一等待出口） | 调用方自己的等待先结束（ctx 取消 / 截止，或同步等待超时）：只说明没等到回复，**不说明结果**——请求可能未执行，也可能已准入并在回复之后提交（strict 事务实测会在回复之后提交，RR-20260928-03） | **结果未知** | 持久 handler 按“可能已提交”处理、不得据此重试；等结论后按业务幂等键核对 |
| 15 | 不命中以上任何一行 | 未提交：准入 / 停机 / 慢阶段准备失败、handler 返回的业务错误（可回滚事务已回滚）等。例外：不能回滚的 handler（memory / RollbackNone）返回业务错误时，失败前的内存修改不撤销，按第 6 行语义处理 | 否 | 按业务错误处理 |

第 2 行的四种形态由 `remoteentity/reply_sentinel_table_test.go` 在真实 Nest + 正式 Remote Manager 上钉住（OPEN-ITEMS B23、RR-20260927-24），第 4 行的 strict（投影器写权威被拒）与 Durability 0（直接写权威被拒）两种形态、第 12 行的定稿拒绝、第 14 行的调用方截止 / 同步等待超时、strict 等待时 tracker 已被淘汰（见下段）同样在那里钉住（RR-20260928-03、RR-20260927-32、RR-20260928-08）。
第 4 行的判据：本地事务已提交后 Remote `Commit` 返回的错误，带 `entity.ErrRemotePersistenceIndeterminate` 的是结果未知（第 2 行），不带的按 entity 契约就是 Remote 没有写入，回复加 `ErrRemotePartRejected`。框架自带的 `remoteentity` 按 Durability 分两种：
Durability 0（memory）由 `Commit` 直接写权威，写前校验失败与权威返回的 fenced / 版本冲突 / 拒绝不带前者（第 4 行），写出后回复丢失、发布失败等带前者；
strict（以及带 Remote 批次、随 strict 路径提交的 pipelined）的 Remote 部分由 WAL 投影器写权威，`Commit` 只等结论——只有投影器写权威被拒（结论 Rejected，满足 `errors.Is(err, entity.ErrRemoteRejected)`，fenced / 版本冲突等原因只在文本里）不带前者，
其余没等到结论的失败（等待截止、投影器报告未知、结论被容量淘汰后重新登记报 `entity.ErrRemoteOverloaded` 等）一律带前者、落在第 2 行（RR-20260928-08；此前最后一种不带，被误标为第 4 行，Remote 其实已写入）。
自定义 Remote 实现在结果未知或没等到结论时必须带前者，否则会被当成明确拒绝。
第 14 行展开：调用方自己的等待先到截止时，`Nest.Request*` 返回 `nest.ErrNestCanceled`（与 ctx 错误并存）或 `nest.ErrNestTimeout`，这只说明没等到回复、不说明结果：
截止只停止等待、不撤销已准入的业务，持久 handler（尤其 strict Remote，请求截止与 Remote 确认截止常是同一个时刻）按“可能已提交”处理，不得据此重试。

第 10、11 行是 `RunIsolatedTransaction` 返回给业务的错误；业务原样回复时，消息自己的事务是否提交仍按其余行判断（这两种情况下嵌套事务
什么都没提交，不会触发第 5 行）。`ErrNestFenced` 表示实例已 fence、请求未执行，或 handler 已执行但消息自己的事务在交给 committer 之前被拒绝（例如 handler 内嵌套独立事务结果未知、fence 之后，RR-20260927-06；回复同时带 `ErrCommitRejected`，按第 12 行），等实例恢复后可重试；链上同时有前 5 行的哨兵时按那一行。
被拒绝时可回滚事务已回滚；不能回滚的 handler（带 Remote 批次的 memory handler——emit 了 effect 的把记录交给 committer 前被拒，没有 effect 的在 Durability 0 直写 Remote 前被拒，RR-20260930-12）内存修改不撤销，按第 6 行的语义确认幂等后再重试（RR-20260927-32 更正：此前这里写“已回滚”，对 RollbackNone 不成立）。
fence 之后的 Remote 直写（RR-20260930-12，维护者 2026-09-30 收紧）：引擎 fence 之后，带 Remote 批次、没有 effect 的 memory handler 的 Durability 0 直写同样被拒绝——回复 `ErrNestFenced` + `ErrCommitRejected`（第 12 行）、Remote 批次 Abort，权威不再被写；此前这类消息在 fence 之后仍成功并写了权威。
fence 检查的时间窗（REMAINING §3 N20，维护者 2026-09-30 接受并写入契约）：`refuseCommitAfterFence` 是消息自己的事务交给 committer（或 Remote 直写）之前的一次性检查，不是临界区。别的 goroutine 在检查之后、committer 接受之前 fence 引擎的那个窗口不由它拦：那种 fence 来自另一笔事务的结果未知，本笔记录进入 WAL 后由 WAL terminal 兜底；嵌套场景（同一 goroutine 里独立事务结果未知后外层再提交）时序确定，检查必然命中。fence 原因以文本写进错误（`%v`），不能再 `errors.As` 取到——这是有意的，原因链上的 `ErrCommitIndeterminate` 会让调用方误走结果未知分支。
吞掉嵌套事务结果未知时的成功回复（REMAINING §3 N22，维护者 2026-09-30 决定不加哨兵）：第 1 行已写明——handler 内嵌套独立事务结果未知、业务吞掉这个错误、外层自己没有要持久的记录（含 memory handler）时，回复是成功，结果未知只能从此后请求得到的 `ErrNestFenced` 得知。框架不会给这种回复加哨兵：**业务不要吞掉 `RunIsolatedTransaction` / `RunDetachedTransaction` 的错误**，原样带进回复才能命中第 1 / 5 行；吞掉后调用方看到的“成功”只对外层的内存修改成立，嵌套事务的持久结果由 WAL 恢复决定。
Guard 锁账本按 ID（REMAINING §3 N27，维护者 2026-09-30 定为契约）：**一个 handler 不跨 EntityManager 持有同 ID 的实体**。Guard 的 eMap 以 ID 记当前持有的实例，不区分 EntityManager；同一 handler 先持有 Manager A 上的 X、再对 Manager B 上同 ID 的 X 取锁时按“同 ID 换了实例”处理（B 上的进 eMap、A 上的转入 superseded，Guard 释放时一并解锁），锁序判断同样只看 eMap 条目。这不是支持的用法，框架不为它收紧也不为它扩大记账；撤销记录（RR-20260928-02）已按（Manager, ID）区分，与此无关。

`RunIsolatedTransaction`（以及新建事务的 `RunDetachedTransaction`）在消息里从不认领消息：消息自己的事务结束之后（提交、回滚或失败），
在它的收尾阶段（Guard post-release 回调、解锁后回调）调用时，同样按嵌套独立事务处理——带 Remote 批次的消息返回第 11 行的错误；
纯本地消息照常执行，已提交时回复按第 5 行、结果未知时按第 1 行 fence，不会被当成消息自己的提交（[RR-20260926-84](bugfix/RR-20260926-84.md)）。
“带 Remote 批次”从批次挂到消息上开始算：`IRemoteEntityManager.PrepareRemoteWriteBatch` **执行期间**批次还没挂上，这时调用 `RunIsolatedTransaction`
不返回第 11 行的错误，按不认领消息的嵌套独立事务处理——与纯本地消息的收尾阶段相同：照常执行并提交，不碰随后才挂上的批次，已提交时回复按
第 5 行、结果未知时按第 1 行 fence。框架自带的 `remoteentity.(*Manager).PrepareRemoteWriteBatch` 只做所有权准入与冷加载，本身不调用
`RunIsolatedTransaction`；只有在这一步里执行的代码（自定义 Remote 管理器，或准备过程中回调到的业务代码）开独立事务才会进入这个窗口
（RR-20260926-84 复核残留，OPEN-ITEMS B19）。这个窗口里独立事务已提交、消息自己的本地事务随后也提交而 Remote 结果未知或被明确拒绝时，
回复同时带第 2 / 4 行的哨兵与 `ErrNestedTransactionCommitted`，按先命中的第 2 / 4 行处理；外层文本是 `nest: a nested isolated transaction also committed`
（不说“消息失败”，此前沿用第 5 行哨兵的 `…committed before the message failed`），`errors.Is(err, nest.ErrNestedTransactionCommitted)` 照常成立（RR-20260928-08）。

## 5. Commit、Load 与主动 Flush

Data Engine 是唯一保存入口。字段变化先进入当前 Nest transaction，再作为版本化 Put/Patch/Delete 写入 WAL；Mongo version CAS 防止旧写覆盖新状态。WAL/Projector backlog 有硬容量和年龄上限，超过门禁触发 runtime failure，而不是无限堆内存。

主动刷盘使用 Registry 中的 Data Engine 能力调用 `Flush(ctx)`。典型时机：停机、迁服、运维检查和版本升级。不要为每个普通请求 Flush，否则会破坏 group commit/批 projection 吞吐；需要强确认的业务选择 Nest strict durability。

Load 只接受完整聚合快照。迁移函数必须幂等、可测试并携带 schema version；加载失败不允许生成“空玩家”覆盖旧数据。

普通 DAO 的 `MigrationRunner.Migrate` 在写 WAL 前验证 BSON / `_id`、目标 `PersistedDaoLoader.RestorePersisted` 解码及 `Id()`；缺装载/身份能力的手写 DAO 返回 `ErrMigrationUnsupported`（[NC-31](bugfix/RR-20261004-NC-31.md)）。直接调用时必须传未发布候选，不能传在线 DAO；预校验用目标 schema 和旧持久 version，可能改变候选字段，失败后应丢弃。BSON int32/int64 ID 兼容保留，`_schema` / `_version` 仍由正式 Put 归一化，不要求迁移步骤自行更新版本。

多 DAO schema 迁移逐 DAO 持久提交，不承诺全有或全无；后序失败时前序有效升级保留，但完整聚合验证前不发布实体。修正错误迁移/源数据后重新冷加载会接续未完成 DAO。等待取消不等于未提交；以新读取确认权威状态，不回滚已提交的有效迁移。竞争写可淘汰旧迁移记录，Repository 重读后最多再迁移一轮；持续竞争超出预算返回 `dataengine.ErrMigrationConflict`，不能放宽 CAS 或直接发布旧候选。

## 6. Remote Entity

Read 模式返回不可变 snapshot：L1 是进程内有界原子缓存，L2 是共享 snapshot store。`Cached` 不回源，`Monotonic` 在版本不足时 singleflight 回源，`Linearizable` 每次读权威存储。高频展示、排行榜引用和 AOI 属性优先 Cached/Monotonic；结算前校验使用 Linearizable 或转成 owner 命令。

共享 L2 是快照水位（已知最新版本 / 是否已删除）的唯一权威，L1 只是它的有界副本（main 未发版，[B2](feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md)）：每次 L1 写入都先在 L2 上以版本 CAS（删除走带版本删除，留与 `snapshot_l2_ttl` 同 TTL 的墓碑）落地，L1 只记 L2 接受或已持有的值并带确认时刻；L2 拒绝时 L1 改取 L2 的较新值。L2 断网或结果未知时写入不失败，L1 照记但标为未确认。

`Cached` / `Monotonic` 的陈旧上限是 `remote_entity.cached_max_staleness`（core `Config.CachedMaxStaleness`，缺省等于 `snapshot_cache_ttl`；必须是带单位的正时长）：交出的快照在交出前这段时间之内被 L2 或权威确认过“没有更新的版本或删除”。超过上限或未确认的 L1 条目先重新确认——读 L2（一次 HGET），L2 落后时把本机的新版本 / 删除补进 L2，L2 回答不了或已没有值时回源权威；都失败时读取返回错误，不交出旧值（行为收紧：之前 L2 断网时写入的条目会一直交出到 L1 TTL）。`Cached` 在 L1 与 L2 都没有该 key 时仍是“未找到”，不回源。不覆盖：owner 提交后写 L2 失败或结果未知时，L2 本身最长落后 `snapshot_l2_ttl`（从旧值最后一次写进 L2 算起），读者在那之后最多再交出 `cached_max_staleness`，即上界 = `snapshot_l2_ttl + cached_max_staleness`：core `DefaultConfig`（L2 TTL 5m、陈旧上限 30s）约 5m30s，生成工程配置模板（`snapshot_l2_ttl: 10m`、`cached_max_staleness: 30s`）约 10m30s；这是维护者定下的上界，不另设后台补写（通常 owner 的下一次读、推送收到的复制更新或下一笔提交会更早把 L2 补齐，见 [B2 §7](feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md)）。复制消息带发布时刻，早于 `snapshot_l2_ttl / 2` 的快照更新（JetStream 同步总线给新 sid 重放的历史）不再被接受（[审查 O5](review/REVIEW-2026-10-05-n05-revn05.md)）。

只读服务（另一个服务读 owner 发布的摘要）用 `remoteentity.NewSnapshotClient(cfg, remoteentity.SnapshotClientDeps{L2, Loader, ConsumerSID})` + `Start(bus)` / `Stop(ctx)`，不需要写 backend、锁或 finalizer；同进程已有 owner 的 Manager 时直接用 `Manager.SnapshotClient()`。业务读 DTO 用 `entity.NewRemoteMirrorReader(source, entity.RemoteMirrorSpec{Kind, Scope, Schema, Codec}, decode)`：解码拿到的是字节副本（改了不影响缓存），读侧校验 key 与 schema / codec，不注册 kind 或全局解码器，所以与同进程的 owner 不冲突。读结果带观察 token `entity.RemoteObservation`（route / marker epoch 与版本），可作为下一次读的最低要求或带给 owner 的命令；更新 epoch 的快照满足旧 token（不论版本），epoch 一新一旧返回 `ErrRemoteObservationIncomparable`。`Linearizable` 只在 loader 声明线性化能力（`LinearizableLoader`）时开放，否则返回 `ErrRemoteReadUnsupported`；Manager 沿用 backend 的线性化读。行为收紧（main 未发版，[Mirror 第 1～3 步](feature/MIRROR-STEPS-1-3-2026-10-06.md)）：`Cached` 带最低版本时，缓存里只有更低版本返回 `ErrRemoteSnapshotStale`（之前交出低于要求的值）；`Monotonic` 未命中只回源一次；`Assembly.Stop` 返回 nil 之后快照读返回 `ErrSnapshotClientStopped`。kit 只读装配与 codegen 只读产物见下面“只读服务（Mirror DTO）”。

快照推送依赖能确认订阅的同步总线（v1.21.0 起，[Mirror 第 4 步与 O4](feature/MIRROR-STEP-4-AND-O4-2026-10-06.md)）：JetStream（`kit/syncbus` 的 `transport: jetstream`）上快照主题用 DeliverNew 的 durable 消费者，订阅确认之后发布的快照不会被静默丢掉，确认之前的历史不重放；某个 key 的权威加载在途时到达的复制消息先进有界缓冲（`Config.SnapshotReplicaBuffer`，缺省 64），加载装入后按到达顺序重放，溢出时丢弃缓冲并再回源一次。普通 NATS 是最多一次，`Assembly.Start` / `SnapshotClient.Start` 不订阅快照推送并记一条 Warn（`snapshot push disabled`），读取按 `cached_max_staleness` 经 L2 / 权威回源（按需读取），健康信息 `snapshot_push=false`。JetStream 部署升级后快照主题换用新的 durable（服务端名字 `sync_remote_entity_snapshot_live_<sid>_<16 位十六进制>`：主题里的 `.` 被换成 `_`，2026-10-06 在隔离 NATS 上列消费者核对，用例 `TestRealJetStreamLiveDurableNameShape`），旧的 DeliverAll durable（`sync_remote_entity_snapshot_<sid>_<16 位十六进制>`）不再消费，可删除。

### 只读服务（Mirror DTO）

另一个服务只读 owner 发布的摘要（公会摘要、排行快照），接入是**一个 DTO + 一行装配**（main 未发版，[Mirror 第 5 步](feature/MIRROR-STEP-5-2026-10-06.md)）。DTO 放在 `roost generate` 会扫描的 `game/` 下：

```go
//roost:mirror entityKind=guild.EntityKindGuild coll=guild
type GuildSummary struct {
	Name      string `bson:"name"` // 按 owner DAO 的 bson 键写，只写要读的字段
	FounderID int64  `bson:"founder_id"`
}
```

生成 `guild_summary_gen_wire.go`：`GuildSummaryMirrorSpec`（`Kind`、`Scope = RemoteSnapshotScope(coll)`、`Schema = RemoteSnapshotSchema(kind, scope)`、`Codec 1`，与 `remote=managed` 生成的提交快照同一规则）、`DecodeGuildSummary`（owner DAO 的持久化 BSON → DTO，未声明的字段忽略）、`NewGuildSummaryReader(source)`。不生成 kind / builder / 解码器注册、DAO 或提交能力；DTO 带嵌入字段或 `comp:` / `dao:` 标签直接拒绝。旧的 `//roost:entity remote=mirror`（及 `lifetime=mirror_cache`）改为报迁移错误（T-274）。

装配：只读服务加 `kitremote.NewRemoteMirrorMod(sid)`（`kit/remoteentity`），读取时：

```go
source, err := kitremote.MirrorSource(registry) // Provide 之后（服务 Init / Mod Start）
reader, err := guildview.NewGuildSummaryReader(source)
summary, found, err := reader.Read(ctx, guildID, entity.RemoteReadCached, entity.RemoteObservation{})
```

`RemoteMirrorMod` 只建 `remoteentity.SnapshotClient`：共享 L2 来自 `redis` Mod（`remote_entity.snapshot_l2_key_prefix` 与 owner 一致），总线来自 `syncbus` Mod（JetStream 才有推送，普通 NATS 按需读取），权威回源缺省是只读 Mongo loader（`remoteentity.NewMongoSnapshotLoader`，读 `remote_entity.mongo.database` 的 `_remote_entity_snapshots`，依赖 `mongo` Mod；不声明线性化，`Linearizable` 读返回 `ErrRemoteReadUnsupported`），`WithMirrorLoader(loader, linearizable)` 可换。不要求 Mongo 原子 backend、锁或 finalizer，注册表里唯一的能力是 `entity.RemoteSnapshotReadOnly`（`mods.ModRemoteMirror`）。配置只读 `remote_entity.*` 的快照段（与 `RemoteEntityMod` 同一个严格读取）与 `remote_entity.mirror.shutdown_timeout`（停机预算，缺省 5s，带单位的正时长）；`sid` 是兴趣的 consumer 身份，必须与 owner 及其他只读服务不同。健康项 `remote_mirror` 带 `snapshot_push` 与 `interest_refused`；本机兴趣表满时 Degraded，停止后 Fail。停机：`StopWithContext` 取消在途的权威加载、在 ctx 内等已准入的工作，超时如实返回、可重试，重复调用返回 nil。手写 Mod 的 `StopBudget` 不计入生成器算出的 `shutdown.total_timeout`，装它的服务要手工留出这段预算。

owner 进程里 `RemoteEntityMod` 把 `Manager.SnapshotClient()` 登记为同一能力，同样的读取代码在 owner 进程里也能用；同一进程不要再装 `RemoteMirrorMod`（启动即报能力冲突）。生成的只读产物用到了 v1.20.2 之后新增的 `entity.RemoteMirrorReader` / `RemoteSnapshotReadOnly`，需要随下一版一起升级 core。

兴趣容量按 consumer 计（O4）：每个 consumer 节点在每个节点的兴趣表里最多 `remote_entity.snapshot_interest_per_consumer` 份租约（缺省 0 = `snapshot_interest_subs / 16`，不能大于 `snapshot_interest_subs`；core `DefaultConfig` 的 `snapshot_interest_subs` 是 262144，配额即 16384，生成工程配置模板写 `snapshot_interest_subs: 100000`，配额即 6250）；`snapshot_interest_subs` 是每个节点兴趣表的条目上限（兴趣是广播，每个节点存所有 consumer 的租约），按“consumer 节点数 × 配额”选。超出配额的 key 没有推送、按需读取：consumer 本地就得到 `remoteentity.ErrInterestQuotaExceeded`（表满为 `ErrInterestRegistryFull`，都是 `ErrRemoteOverloaded`），计入 `remote_entity.remote.interest_renew_refused_total` 与健康信息 `interest_refused`；owner 侧按原因计入 `remote_entity.remote.interest_rejected_total{reason}` 并限频记日志。release 之后迟到的更旧续租不再复活租约；表满时放不下撤销水位的 release 改记每个 consumer 一个的溢出水位，同样挡住迟到的旧续租（[RR-20261006-11](bug/RR-20261006-11.md)）。

owner 同 sid 重启（O-M6-1）：重启的 owner 兴趣表是空的。`Assembly.Start`（`RemoteEntityMod`）订阅确认后在 `remote_entity_interest_refresh` 主题广播一条“请重新续租”，推送开着的只读方收到后把本机仍有效的兴趣立即续租一次，推送在一次往返内恢复，不用等各自的续租周期（剩余不足一半，缺省最长约 15s）。续租走与读时续租同一入口（代际、配额、撤销水位不变），请求合并、两次遍历至少隔 1s。旧版本只读方不订阅这个主题，混跑期间它们仍按原周期收敛；不需要任何配置（T-278）。

owner 同 sid 重启后的第一笔写（O-M6-6）：开了 App 单实例锁（`singleton.enabled=true`）的进程，`RemoteEntityMod` 把锁的身份（`app.ModSingletonIncarnation`：锁键、sid、本次启动的 token）交给 Remote，共享锁的取锁 token 带上 sid 与进程代际（`~<sid>~<锁键摘要>~<token>~…`）。同 sid 的进程被强杀后重启，第一次取某个实体的锁时，若 owner 是同一单实例锁持有者的上一代留下的，就在取锁脚本里当场接管（与租约过期后取锁同一分支：fence 递增、Mongo 许可换代，上一代的许可 / 续期 / 释放全部失效），不再等 `remote_entity.lock_ttl`，也就不会因为锁的重试预算用完报 `versioned lock not acquired`、或 Request 先到截止变成结果未知。别的 sid / 服务类型 / 项目前缀持有的锁、旧格式（升级前进程）的锁一律不碰。**`singleton.enabled=false` 不接管**：没有“旧进程已死”的保证，照旧按 TTL 等待；手写装配用 `NewRemoteEntityMod(sid)` 给了与单实例锁不同的 sid 时同样不接管。接管计数 `remote_entity.lock_takeover_total`，每个进程第一次接管记一条 Info。直接调 `remoteentity.Assemble` 的装配可以经 `AssemblyDeps.Incarnation` 传入，但只有能证明同 sid 旧进程已死的进程才能传。[方案](feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md)

墓碑写入等副本（O-M6-3）：L2 带版本删除写墓碑之后，在同一连接上对该键所在的 Redis 主节点发 `WAIT`，等 `remote_entity.snapshot_l2_tombstone_wait_replicas`（缺省 1，0 关闭）个副本确认，最多 `remote_entity.snapshot_l2_tombstone_wait_timeout`（缺省 50ms，必须为正且不超过 1s：调用方最多多等这么久，也要低于 Redis 客户端读超时）。主节点没有副本（单机开发环境）时自动不等。确认不足或 WAIT 出错时删除照常成功（墓碑已在主节点上），只计 `remote_entity.snapshot_l2_tombstone_wait_total{result}` 并限频 Warn（T-277）。它缩小“墓碑未复制就切主、新只读方在陈旧上限内读回已删除实体”的窗口，但副本断开时仍可能丢，要彻底避免需要部署约束（`min-replicas-to-write`）。普通快照写入不等。两项都是 A4 严格读取，`RemoteEntityMod` 与 `RemoteMirrorMod` 共用。

L2 快照键默认是 `remote_entity:snapshot:<tenant>:<kind>:<id>:<scope>:<policy>`，不带部署前缀。多个部署共用一个 Redis db 时，给每个部署配置不同的
`remote_entity.snapshot_l2_key_prefix`（例如 `roost:<工程名>`，core 为 `Config.SnapshotL2KeyPrefix`），键变为 `<prefix>:remote_entity:snapshot:…`，
否则彼此读写同一份快照。缺省为空时键与旧版本逐字相同，不需要迁移。同一部署的所有节点必须配置同一个值：新设或修改前缀相当于换一套空的 L2（快照会从权威重新发布），
滚动修改期间新旧节点互相看不到对方写入的 L2，`Cached` 读可能在 `snapshot_l2_ttl` 内读到旧前缀下的旧快照，所以应整体重启。前缀不能含空白，
也不能含 Redis Cluster hash tag（`{…}`，L2 脚本只操作单键，带 tag 会把全部快照键钉在同一个槽），Init / Assemble 拒绝这类值（[RR-20260927-17](bugfix/RR-20260927-17.md)）。
这个前缀只作用于 L2 快照键。Remote 写到 Redis 的键一共三类，共用一个 Redis db 的部署要**逐类**隔离（[RR-20260930-19](bugfix/RR-20260930-19.md)）：

| 键 | 谁写 | 缺省 | 隔离方式 |
| --- | --- | --- | --- |
| `remote_entity:snapshot:<tenant>:<kind>:<id>:<scope>:<policy>`（逐实体 hash） | 正式装配的 L2 快照 | 不带前缀 | `remote_entity.snapshot_l2_key_prefix`（不能带 hash tag） |
| `lock:<lock_key>:<id>`、`lock:<lock_key>:<id>:fence`（逐实体 hash + 计数器） | 正式装配的版本锁 | `lock_key` 缺省 `e`，不带前缀 | `remote_entity.lock_key` 各部署配不同值（如 `game-a`；Cluster 下必须带 hash tag，如 `{roost:game-a}`）。缺省值不会自动改：改锁身份要整体重启，不能滚动（RR-20260924-25） |
| `remote_entity:marks`（一把 hash，field 是实体 ID） | **只有非 authority 兼容装配**（自行 `SetOwnershipStore(NewRedisMarker…)`）；正式 kit 装配的所有权存储是 Mongo 权威，不写这把键 | 不带前缀 | core API：`NewRedisMarkerWithKeyPrefix(redis, prefix)` → `<prefix>:remote_entity:marks`，或 `NewRedisMarker(redis, key)` 显式键名。kit 没有对应配置项，因为 kit 不写它 |

`NewRedisMarkerWithKeyPrefix` 的前缀与 L2 前缀同形（同一个部署前缀值可同时用于两者），空值键不变；不能含空白；标记只有一把 hash 键，允许 hash tag，但写了花括号就必须是首个非空闭合 tag（与 `lock_key` 的 Cluster 判断同一规则）。

Write 模式使用 Mongo 持久所有权；共享写先竞争 Redis 协调锁，再取得 Mongo majority 写许可，owner-routed 写也取得持久许可，然后权威加载 → Nest 事务修改 → 条件提交。存储条件同时包含 StateVersion、MarkerEpoch、LockFence、RouteEpoch。分布式锁只避免同时进入临界区，四维 fence 才能拒绝暂停后恢复的旧 owner 或旧路由写入。

正式 Assembly 要求持久权威能力；MongoCommitter 创建即强制校验许可，WriteAuthority() 只返回能力，没有弱校验开关。当前未部署，不提供旧协议迁移入口；不支持的元数据拒绝启动，见 [提交契约](bugfix/RR-20260925-02.md)。

`remote=managed` 实体的 DAO 必须是 `dbscope=global`（缺省）：托管实体由任一进程提交、所有权可迁移，按服选库会让提交与加载落在不同的库。这条规则在四处以同一个可 `errors.Is(err, entity.ErrRemoteManagedServerScopedDAO)` 的错误拒绝（`remoteentity.ErrRemoteManagedServerScopedDAO` 是同一个值）：`roost generate` 生成期、生成 registry 末尾的 `entity.ValidateEntityRegistry`、Remote 装配（`remoteentity.Assemble` / `Start`），以及 Remote 事务进入 WAL 之前（手写实体注册的 DAO 工厂漏报 sid DAO 时由这里兜底，事务回滚、不产生提交）。已写入 WAL 的旧记录重放不受影响。见 [RR-20260926-45](bugfix/RR-20260926-45.md)、[RR-20260927-09](bugfix/RR-20260927-09.md)。

不要把 Remote Entity 当透明 RPC ORM。调用方必须选择读一致性，命令必须有 request/transaction ID，重试必须幂等。

## 7. 跨服务 Saga

outbox候选扫描不授予发布权：自定义Store的ClaimOutbox也必须原子检查next_attempt_at与lease，避免另一发布者Nack延后后仍被陈旧候选领取（NC-41）。到期可重试、旧token不能Ack/Nack新持有者，不通过缩短退避解决该竞争。[说明](bugfix/RR-20261005-NC-41.md)。

Activity的持久opening若畸形，Open重投返回ErrConflict且不写活动；sweep保留坏名额/指标并跳过非法键或跨组条目。先核对原始计划并修正，再用合法Open或sweep恢复；不盲删生产记录、不把坏计划成功当正常进度。[NC-42与升级边界](bugfix/RR-20261005-NC-42.md)。

启动重投比较原始意图，不比较当前业务状态（NC-39）：新记录的 `StartDigest` / Mongo `start_digest` 与可变 `Data`、`DeadlineAt` 分离。相同 type/business_key/definition_version/data/deadline 返回已有进度；不同意图或显式ID不符返回 `ErrIdentityConflict`，不会重置 Saga。自定义 Store 必须保留摘要，协调 writer 统一升级；旧已推进缺摘要记录无法证明原意图，重投明确冲突，应先 Get/List 查进度，不换业务键盲重做、不自动从当前 Data 回填摘要。[兼容/迁移边界](bugfix/RR-20261005-NC-39.md)。

原生完成 effect 的 Topic 必须是 `saga.result.<payload SagaID>`（NC-40），不一致在状态/回执写入前永久拒绝；正式 `NewCompletionEffect/EmitCompletion` 本来满足该格式。自定义发布源修正路由后再按幂等协议发送；这不是内部发布鉴权。[说明](bugfix/RR-20261005-NC-40.md)。取消消费等待不代表业务已撤销，原生收件箱保留可能在途的lease，按权威receipt与既有屏障恢复，不删WAL/回执。[取消学习](review/IMPLEMENTATION-SAGA-START-IDENTITY-AND-CANCELLATION.md)。

Saga 用于无法放进同一 Mongo transaction 的多阶段流程，例如跨区交易、联盟转服、邮件发奖与外部支付。每一步由持久状态机、lease fencing、outbox 和幂等 receipt 驱动；失败执行显式补偿。Nest 事务可写入 start effect，使“本地提交”和“启动 Saga”共享一个 commit point。

Saga 不是分布式 ACID：补偿可能延迟，外部系统可能需要人工处理。步骤 handler 要区分可重试错误、永久错误和结果未知；补偿同样必须幂等。

### Saga 步骤预算

步骤的超时与重试次数是配置，一次操作（同一步骤、同一方向）可以有多次尝试（U-0280）。kit 的 saga Mod 读取：

```yaml
saga:
  step_defaults:          # 定义里没写的字段取这里；不写取框架默认 5s / 5 次 / 100ms..5s
    timeout: 5s
    max_attempts: 5
    backoff_min: 100ms
    backoff_max: 5s
  steps:                  # 按步骤覆盖，优先于定义与 step_defaults
    gift_item:            # Definition.Type
      debit:              # Step.Name
        max_attempts: 15
```

取值顺序：`saga.steps.<type>.<step>` > 定义里写的值 > `saga.step_defaults` > 框架默认。`saga.steps` 下写了不存在的 saga 类型、
步骤或字段，`Init` 失败（不会静默不生效）。`roost add saga` 生成的定义不再写预算，生成配置带 `step_defaults` 与空的 `steps: {}`；
game-demo 在 game 服务的三份配置里把 `gift_item.debit.max_attempts` 覆盖为 15，让退款（debit 的补偿）的重试窗口覆盖发送方 sid 的
一次崩溃重启，`internal/service/game/gift_saga_budget_test.go` 用 `kitsaga.StepBudgetsFromConfig` 读同一份配置核对。不用 kit Mod 时
设置 `saga.Options.StepBudgets`，`Engine.Register` 按它补齐。

预算正反两个方向共用；重试次数只决定协调器等多久，不会让同一次操作生效多次：原生步骤（`SubscribeDataEngineStep`）保证同一
操作实例的所有尝试里最多一次生效，尝试只在命令截止前生效，协调器放弃后才到的成功记 `saga.completion.late_after_abandon_total` 告警，
不重开终态（[SAGA.md「原生步骤执行契约」](../SAGA.md#原生步骤执行契约u-0280维护者-2026-10-05-决定)）。代价：Mongo 投影积压超过步骤
`timeout` 时步骤停住（每次尝试都在截止后才投影、被跳过），积压消退后才成功，而不是像以前那样重复执行。Mongo 步骤
（`SubscribeMongoStep`）自 2026-10-06 纳入同一契约（saga 方向 ②）：生效点是 handler 的 Mongo 事务，业务写经 handler 拿到的事务 ctx
写进这笔事务时，按 `IdempotencyKey` 的业务幂等只是可选的纵深防御；调用另一个服务（如发邮件）不在事务里，仍要按 `IdempotencyKey` 幂等。
两种步骤的收件箱对每个操作只写一份状态文档（原生 `_dataengine_step_operations`，Mongo 步骤 `<收件箱集合>_operations`，2026-10-06）。
**升级要求**：存储形状与 v1.22.0 及以前不兼容，步骤服务先停旧再起新，清空或丢弃旧的 `_dataengine_inbox_claims` / `<收件箱集合>_claims`
（[SAGA.md「操作状态文档」](../SAGA.md#操作状态文档2026-10-06维护者决定直接改成一份状态文档)）。

**部署：Mongo 的 `transactionLifetimeLimitSeconds`**。进程被 kill -9 时它手里的 Mongo 事务在服务端保持打开、持锁到这个参数（默认 60s），
期间同一文档上的步骤尝试一直 WriteConflict；默认步骤预算约 27s，短于它，受影响的操作会被拖进补偿。生产建议把它调到 20
（`--setParameter transactionLifetimeLimitSeconds=20`）：框架的事务由 `mongo.transaction_timeout`（默认 30s）约束整个重试过程，单次事务远短于 20s。
不能改服务端参数时，按步骤调大 Mongo 步骤的 `max_attempts`，让预算长于它（SAGA.md「进程被强杀时遗留的 Mongo 事务」）。

## 8. 实时同步怎么选

状态同步适合 ARPG/MMO/大多数房间服：服务器权威模拟，按 20 Hz 产生全局 snapshot 或 delta，按 AOI/LOD 给不同客户端裁剪字段；可靠通道发基线和关键事件，datagram 发可丢弃最新状态。技能的权威结果进入状态 mutation，施法表现、音效和轨迹进入 presentation event，因此能覆盖技能游戏而不要求把所有表现塞进 Entity snapshot。

Lockstep 适合客户端确定性模拟的 MOBA/RTS：服务器排序输入帧、保存历史、冗余广播和校验 hash，不运行完整战斗模拟。追帧走可靠通道，实时输入走 datagram。客户端算法、定点数、随机种子、配置 hash 必须一致。

单房间默认上限 100 Entity/订阅者。20 Hz 是调度目标，不代表所有字段每帧发送；用 interest、LOD、dirty delta、量化和 baseline ACK 控制带宽。慢客户端只能影响自己的 session。

## 9. 技能系统

技能配置先编译为不可变 Program，再由定点数 runtime 执行。HostAdapter 只能在已持锁 Entity 作用域内提交权威 mutation。技能逻辑要明确区分：可恢复的 runtime state、需要同步的权威状态、只发给客户端的表现事件。

版本发布时固定技能 schema/程序 hash；热更只能在声明的安全边界切换。正在执行的旧技能是继续旧 Program 还是迁移到新 Program，必须由业务策略明确，不能隐式替换。

## 10. 配置、协议与错误码

开发与生产配置分离。生产配置必须经过 `roost project doctor`，不得含 `CHANGE_ME`、localhost、开发 token 或明文仓库 Secret。环境差异使用部署系统挂载完整配置；不要靠构建不同镜像改变配置。

### 配置写法与启动校验

App 在任何 Mod Init 之前调用 `ValidateServiceConfig`，按严格规则检查框架读取的全部类型化的键（`app/config_validation.go` 的 `frameworkBoolKeys` / `frameworkDurationKeys` / `frameworkIntKeys`，另有 syncbus 的 `syncbus` / `room` / `sync` 三段与所有 `<service>.call_timeout`）：

| 类型 | 接受 | 拒绝（启动失败并点名键） |
| --- | --- | --- |
| 布尔 | `true` / `false`（大小写不限）、`"true"`、`1` / `0` | `on` / `off` / `yes` / `no`、拼写错误 |
| 时长 | `15s`、`500ms`、`1m30s`、`0` | 不带单位的非零数字（`15`、`"15"`，以前读成 15ns）、解析不了的值 |
| 整数 | 整数、`1e3` 这样没有小数部分的数、十进制字符串（环境变量覆盖） | `8k`、`1.5`、`10s`、布尔值（以前读成 0 后取默认或被截断） |

kit 的 Mod 在 Init 里也严格读取，直接装配 Mod、不经 App 启动的调用方同样拿到错误。业务 Mod 读配置用 `app.ConfigBool` / `ConfigDuration` / `ConfigInt` / `ConfigInt64`，或 `app.NewConfigReader(cfg)` 连续读、最后检查 `Err()`（一次报出全部写错的键）。框架内新增读取必须走这些函数并登记到上面的清单，app 的守卫测试会扫描源码。

### `env: production` 校验什么

配置里 `env` / `app.env` / `environment` 为 `prod` / `production` 时，`ValidateServiceConfig` 追加以下检查，其余照常：

- `ops.enabled` 时 `ops.addr` 只能绑回环地址；要绑 `0.0.0.0`（k8s 探针、Prometheus 抓取）须同时写 `ops.allow_public_addr: true`，表示端点已放在鉴权代理之后。生成的生产示例绑 `0.0.0.0:9100`，打开生产模式时要加这一行。
- game / instance / account / match_group / global 必须写 `redis.addr` 或 `redis.cluster_addrs`（逗号串或 YAML 列表；只配 Cluster 的生产配置可以启动，RR-20261006-28）。
- `account.session_secret`、`platform.session_secret`、`platform.payment_secret` 不能为空或以 `dev-` 开头。
- admin_gateway：`admin_gateway.tokens` 非空且不是 dev 令牌；`local_ops` 目标没有自己的 `ops_token` 时 `default_ops_admin_token` 必须是非 dev 值。
- `time.logic_offset` 必须为 0（或不写）：偏移只给测试环境前拨业务时间用（D-L3）。

它**不**检查、也不代表已经开启：按请求限流、登录鉴权、WAL 持久级别（持久化由 `dataengine.*` 决定）、实例状态存储。RR-20261005-NC-192 之前这里要求的 `player.login_auth_required`、`player.login_secret`、`player_protocol.rate_limit.enabled`、`save_load.wal.*`、`instance.client_mode` / `state_store_required`、`account.ops_token`、`account.redis_required`、`global` / `match_group.redis_required` 没有任何代码读取，已删除；配置里留着也没有影响。生成的游戏服接入层只有演示凭据（`auth.go`），上线前换成真实校验；需要按请求限流时自己装配 `gateway.RateLimit` 或在接入层限流。

### 生成工程切到 Redis Cluster

生成的配置是单机 Redis。整体切到 Cluster（[真实进程演练](bugfix/REAL-PROCESS-DRILLS-2026-10-06.md)第 ⑥ 项在 3 主 3 从上走通的清单）：

1. 每个服务配置的 `redis.addr` 换成 `redis.cluster_addrs`（逗号串或 YAML 列表；两者都写时 Cluster 优先）。
2. 有跨键原子写的服务，`key_prefix` 必须带非空的首个 hash tag：`activity`、`platform`、`rank`（例如 `"{roost:<project>:activity}"`，YAML 里加引号）。game 配置里的 `activity.key_prefix`、`platform.key_prefix` 与这两个服务**是同一个键空间**，要一起改——只改服务一侧时，购买的发货写进带 tag 的键、game 去旧键里取（机器人报 `delivered 10 of item … but the bag holds 0`），活动的贡献榜同理。漏改的服务在 Init 时点名报错（`requires a non-empty closed first hash tag for Redis Cluster`）。
3. game 配置加 `remote_entity.lock_key`，带 hash tag（例如 `"{roost:<project>:remote}"`）；缺省 `e` 在 Cluster 下 Init 报错。改锁身份要整体重启（见上文 Remote 键表）。
4. 单实例锁（`singleton.key_prefix`）、account、session、mail、chat、global、match 是单键操作，前缀不用改。
5. 登记游戏服：`go run ./cmd/accountctl -redis-cluster <种子,种子,…> -prefix <account.key_prefix> upsert-server -sid N`（`deploy/dev/run.sh` 读到 `cluster_addrs` 时自动这样调用）。用 `-redis <单个节点>` 只有键槽恰好在这个节点上才写得进去，其余报 `MOVED`（RR-20261006-28）。

改前缀等于换键空间：已有数据不会自动搬过去，生产上按各服务的迁移说明处理（activity 见 REVIEW-2026-09-29-services-08 的建议），不要只改配置。

### 业务时钟与系统时钟

维护者决定 D-L3，[方案与盘点](feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md)。

| 钟 | 读法 | 用在 |
| --- | --- | --- |
| 业务时钟：真实时间 + `time.logic_offset` | `app.BusinessClock(registry).Now()`；服务的 `Config.Now` 由 Mod 注入它；请求上下文里 `fctx.Now()` | 活动窗口与协调器、World 定时器、日 / 周重置、冷却、邮件 / 道具业务过期、赛季、排行周期、skill / 战斗游戏时间、匹配票据与等待放宽、展示给玩家的聊天时间、账号 / 角色创建与登录登出时间、业务计时规则 |
| 系统时钟：真实时间 | `time.Now()` | server 帧率、租约与锁、超时与 ctx 截止、重试退避、存储 TTL、消息 Ack、日志、指标、WAL 与审计时间戳 |

- `time.logic_offset`（时长，如 `24h`）只有这一个来源，所有进程写同一个值——game 与活动协调器、match 偏移不同，窗口 id、截止与票据时间会错开。只在启动时生效，改了要重启全部进程；没有运行期修改入口。生产环境必须为 0。
- **业务时间只许前进**（[方案](feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md)）：偏移可以前拨，不能让业务时间回到这套部署已经到过的时刻。App 把高水位存在协调存储（单实例锁的存储，共享 Redis，键 `<singleton.key_prefix>:business_time`，不过期）里，单实例锁之后、任何 Mod Init 之前检查：`真实时间 + 新偏移 < 高水位 − 1 分钟` 就拒绝启动，错误 `app.ErrBusinessTimeMovedBack` 写明偏移、两边的业务时间、写入者和至少要用多大的偏移；运行中每 10s 推进一次高水位。也就是说偏移最多能改小“距上次运行的真实时间 + 1 分钟”。1 分钟容差吸收主机之间的时钟偏差。检查的进程：非生产里开了单实例锁的，以及配了非 0 偏移的（没开锁的只为高水位打开一个连接；bootstrap 没装 `App.Singleton` 或没写 `singleton.key_prefix` 时拒绝启动，`app.ErrBusinessTimeGuardMissing`）；读写高水位失败也拒绝启动。生产不检查，行为不变。**测试环境要回到过去只能清库重建**（Redis、Mongo、WAL 等全部业务数据连同这个键）；只删键保留数据等于跳过守卫，已打戳的业务截止会晚到一个回退量。`roost project doctor` 的 `time:logic_offset` 一行检查同一套部署里每个服务的配置是否一致：dev 配置（`configs/service/config.<service>.yaml`）、prod example、k8s secret example 三套各自比较（不写等于 0s），不一致时 FAIL 并列出每个服务的值；格式错（如 `1d`、不带单位的数字）也 FAIL 并点名文件。三套之间可以不同（测试环境前拨、生产为 0）；仓库外的生产配置看不到。
- 已接入：kit 的 activity 协调器（窗口、宽限、开关窗、派发的重试排期 `NextAttemptAtUnix` 与 owed 清单的到期判断、进度凭证 `ExpiresAtUnix` 全部是业务时间）、mail（过期与领取租约 `ClaimDeadlineUnix` 都是业务时间——租约只在 mail 服务内部比较，业务时间不回退，前拨只让它提前结束，重试拿到同一个 token）、rank（同分 tiebreak）、session（run 截止）、match（票据的创建 / 过期 / 结束时间与超时判断）、chat（`Message.SentAtUnix` 是展示时间，业务时钟；`StoredAtUnix` 与 `Prune` 的保留期截止是系统时钟，`Config.SystemNow`）、account（账号创建与最近登录、角色创建、角色登录 / 登出时间是业务时钟；会话 token 签发与有效期、`UpsertServer` 与运维处置的记录时间是系统时钟，`Config.SystemNow`；建角时的名字预约 TTL 是目录自己的系统时钟）；game-demo 的活动窗口、World 定时器的每一拍、GM 关窗、怪物重生、匹配的等待放宽（`matchmaking.Pools`）、邮件领取 / 副本领奖 / 公会的 `nowUnix`。框架库（`timer.Scheduler`、`ai`、`actionflow`）没注入时间源时缺省读进程级业务时钟。platform、directory、saga 与 Nest / DataEngine / Sync 全部是系统时钟。
- chat 的持久格式只加了 `sent_at_unix`，`stored_at_unix` 含义与用途（保留期）不变。加字段之前存下的消息没有 `sent_at_unix`，读出时（Publish 的重放应答、History / Conversation / Scrollback）用 `StoredAtUnix` 兜底，存着的数据不改写；那时偏移为 0，两个时间一致。客户端展示消息时间请读 `SentAtUnix`。
- 业务过期不靠存储 TTL：服务按业务时钟判断过期，存储 TTL 只兜底回收空间且更长（mail 信封 = 业务剩余时长 + `EnvelopeStorageGrace`，固定 24h：过期后一天内领取报 `ErrExpired` 而不是 `ErrMailMissing`，也吸收主机之间的时钟偏差）。业务时间不能回退，所以不再有“偏移往回调超过宽限、存储早于业务过期被回收”的情形；v1.21.0 的 `RedisConfig.StorageGrace` 已删。
- 偏移在两次启动之间改变（只会在非生产）：前拨后到期的定时器、窗口、邮件在下一次检查时成批处理，业务时钟上的退避与租约提前结束（只是早一点重试）；让业务时间回退的后拨在启动时被拒绝（见上）。
- 测试：在 cfg 里写 `time.logic_offset` 再 `app.NewRegistry(cfg)`，或给服务的 `Config.Now` 注入 `clock.BusinessFunc` / 函数。
- `glsvet` 对业务包（模块根之下路径里有 `game` 目录，`-businessdirs` 可改）里直接读 `time.Now` / `time.Since` / `time.Until`（含把 `time.Now` 当函数值传）打印 `hint:`，不计入失败；确实是系统时间的在同一行或上一行写 `//glsvet:system-clock <理由>`，或写进函数文档注释豁免整个函数；`-clockhints=false` 关闭。

### 配置数据：规则、热更与可见性

业务配置表（`configs/data/*.json`）由 `configdata.Store` 加载成不可变快照；请求钉住准入时的那一代（`configdata.ActiveSnapshot` / 生成的 `XxxByID`、`XxxTable()`），同一请求内两次读不会跨代。

**两条生成管线，该用哪条**（维护者 2026-10-06 选 A：保持两条，不合成一套，理由见 [B10 §2.4](feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md#24-tablegen-与-cfggen-能否合成一套)）：

| | tablegen | cfggen |
| --- | --- | --- |
| 手写什么 | `configs/schema` 下的 Go 结构体，`//roost:table` / `//roost:object` 标记，字段带 `csv` / `json` / `title` 与规则标签 | 一个 YAML 描述 `configs/schema/cfg.yaml`（tables / globals / beans，写法类似简化版 Luban），不写 Go 结构体 |
| 数据 | 策划编辑的 CSV（`configs/table`，可在 Excel 里编辑后存 CSV），转换成 `configs/data/*.json`；生成器同时给出 CSV 模板 | 直接维护 JSON |
| 单例配置 | `//roost:object` | `globals` |
| 怎么跑 | `roost generate` 默认执行（config 步骤：Go 访问代码写到 `configs/generated`，CSV 转 JSON） | 可选，不在 `roost generate` 里，单独执行 `go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/cfggen -meta configs/schema/cfg.yaml -out configs/cfg`；输出不能和 tablegen 共用 `configs/generated`（RR-20261005-NC-72） |
| 适合 | 策划用表格填数、以 CSV 为交付物的流程 | 程序维护的配置、需要 bean（嵌套结构）或二级索引、不想手写 Go 结构体 |
| 规则 | 同一套：`configdata/rules`，configdata 在每次加载与 reload 时用同一个检查器执行（见下条） | 同左 |


- **规则只写一次，在加载层强制**（B10）：tablegen 的字段标签 `required:"true"`（JSON 键必须出现且不为 null）、`unique:"true"`、`min:"<n>"`、`enum:"a|b"`、`ref:"<table>"`，或 cfggen 的同名选项，生成为 `TableDef.Rules` / `cfg` 标签，configdata 在每次 Load / Reload 检查：required / unique / min / enum 查原始 JSON（缺列与零值分得清），ref 在全部表加载后查目标主键。**键大小写敏感**（2026-10-06，[方案](feature/CONFIGDATA-CASE-SENSITIVE-KEYS-2026-10-06.md)）：数据文件里的键必须与字段的 json 名逐字一致，嵌套对象同样；只差大小写的键（`Level` 对 `level`）或一行里同一字段的几种拼写，在 Load / Reload / DryRun 都被拒绝，错误形如 `configdata: table monster row 1 (key 1) field level: case: key "Level" must be spelled "level" (keys are case-sensitive)`。未声明的键维持原样（宽松忽略、严格模式拒绝）；`FieldRule.Field` 也必须逐字是 json 名。违反任何一条整次拒绝、旧快照保持，错误是 `*configdata.RuleError`：`configdata: table spawn row 1 (key 1) field template: required: missing or null`。规则声明写错（字段不存在、min 用在字符串上）在注册时就失败。业务不需要再为“缺列 / 零值 / 悬空引用”写校验或防御代码；跨行、跨表的业务约束仍写 `TableDef.ValidateTable` / `Validate`（在规则之后运行，可假定引用完整）。
- **热更顺序与可见性**（C2）：build（文件、规则、表校验）→ 监听者 `ValidateReload` → `BeforeApplyReload` → **发布** → lifecycle 事件 → `AfterApplyReload`。新的一代在 AfterApply 之前已发布；AfterApply 失败时整次撤回，发布到撤回之间准入的请求整个生命周期读被撤回的那一代。`Store.Rollback` 回到上一代，回滚前准入的请求读被回滚的那一代。不能接受的检查放进 Validate / BeforeApply。
- **失败看得见**：每次 Load / Reload / Rollback 恰好报告一次 `ReloadOutcome`（`Store.OnReloadOutcome` 订阅），Store 写日志：成功 Info `config reload applied`，失败 Warn `config reload failed`（带 `stage` = build / validate / before_apply）、撤回 Warn `config reload reverted`（`stage=apply`）。kit 的 configdata Mod 据此计 `configdata.reload.total{result=ok|failed}`、`configdata.rollback.total{trigger=apply_failed|operator}` 与 gauge `configdata.version`；运维填的 reason 只进日志。版本号来自单调计数器，失败、撤回、DryRun 也占号，成功后版本变大但不一定 +1。

协议定义保持单一来源，由 codegen 生成 pb、msgid、绑定和 manifest。变更遵循向后兼容：字段只新增、不复用编号；先发布兼容 reader，再发布 writer，最后清理旧字段。错误码 ID 空间由 `roost id` 检查，不在多个服务手工分配。

## 11. 测试策略

每次提交至少：

```bash
make generate
make ci
go test -race ./...
```

生产门禁额外包含：真实 Mongo replica set、Redis AOF/WAITAOF、NATS JetStream、etcd compaction；kill -9、磁盘满、网络分区、主从切换；WAL replay 与 tombstone 防复活；重复消息和乱序；20 Hz/100 Entity 的 p95/p99、CPU、分配、队列和带宽。

## 12. 常见错误

- `readyz` 失败：先看依赖 health 和 runtime failure，不要只重启掩盖 fence 原因。只有 fail（或进程还没就绪 / 正在停机）让 `readyz` 503；degraded 仍返回 200，从响应体 `degraded_dependencies` 看是谁降级、为什么（见 OBSERVABILITY“健康与就绪”）。
- WAL 无法启动：检查目录是否被另一个 SID 使用、权限、磁盘空间和旧版本格式。
- NATS reliable 找不到 Redis：必须安装 Redis Mod；顺序由可选依赖图自动处理。
- Remote Entity 旧写被拒绝：这是 fence 生效，重新解析 owner/route 并从新 snapshot 发起命令。
- K8s 滚动更新卡住：单副本 PDB 会阻止自愿驱逐；按部署手册执行有状态维护窗口，不要强行双 writer。

## Remote 投影并发

正式 DataEngine 的 `dataengine.projection.remote_workers` 默认 8，范围 1～64；设置 1 可保持串行投影。只并行相邻、Entity 不重叠的纯 Remote 事务，相同 Entity 和带普通 DAO/effect/receipt 的混合事务保留顺序。独立使用 core 时设置 `engine.ProjectorOptions.RemoteProjectionWorkers`，0 取默认值。自定义 Remote 适配器未声明并发安全时保持串行。

每笔业务仍执行原有 Mongo 原子提交和完整快照发布，strict 请求确认条件不变；WAL checkpoint 只跨越连续成功前缀。该参数调整 I/O 并发，不改变 Nest 请求超时和客户端 Sync 频率。性能与超时定位见 [实施报告](feature/REFACTOR-2026-09-25-remote-throughput.md)。

### Nest Remote 慢操作并发

Nest 统一使用快慢两个业务执行池。所有 handler、Entity Guard、本地事务和释放锁前的 Sync 在快池；Remote 获取/确认/释放、声明目标中未加载实体的预加载（统一准入自动判定，RR-20260926-25）与显式 `SendOptionSlow()` 的目标加载在慢池。慢池为共享队列，不按 ID 分槽；所有显式目标在统一准入时登记顺序，等待前驱不占快 worker。加载初始化、提交失败本地回滚等需要本地执行的步骤通过 `entity.RunLocal(ctx, fn)` 回到快池，自定义 Loader 也必须遵守。

```go
nest.NestOptionWithWorkerPools(
    nest.WorkerPoolConfig{Workers: 8, QueueCap: 10000}, // 快池
    nest.WorkerPoolConfig{Workers: 64, QueueCap: 64},   // 慢池
)
```

Kit 对应 `nest.fast.workers`、`nest.fast.queue_capacity`、`nest.slow.workers`、`nest.slow.queue_capacity`。容量是整池等待总数，包括等待 ID 前驱的消息，排除执行中的请求和已预留执行额度的就绪请求；内部快延续单独计数。默认快并发 GOMAXPROCS、容量 10000；慢并发 max(32, 快并发×4)、容量 64。旧 `worker_num/queue_capacity/remote_workers` 仍作为未设置新值时的来源，`heartbeat_worker_num` 不再创建单独池，旧 `SendOptionIsCost()` 等同 `SendOptionSlow()`。冷目标不再需要手工加 Slow：Nest 在统一准入时对 Single/Multi/MultiGroup 的声明目标做一次只读内存判定（Getter 可选实现的 `entity.LoadedChecker.IsLoaded`，`ManagerAccess` 已实现，不调用 `Get`，RR-20260926-47），有未加载且可由 loader 加载的目标就走慢阶段预加载，业务代码和生成 sender 不变；Broadcast 仍按已加载目标尽力扇出，冷目标逐个报告。显式 Slow 仍有效，用于强制慢准备。handler 内部阻塞 RPC 必须显式拆为前置 I/O，无法自动迁移。

`NestMgr.Stats().Fast/Slow/FastContinuations` 和 statslog 的 `nest.fast/nest.slow` 输出两池及内部延续；旧 `Stats().Remote` 只是 Slow 的源码兼容别名。可选 stage metrics 的 `remote_prepare`、`logic_queue`、`remote_confirm` 分别定位获取、逻辑排队和后置确认。停机关闭外部准入，两池保持运行直到已接受工作和内部延续全部排空；strict 请求仍在确认完成后返回，WAL 持久准入保留锁内契约。

慢池可以设为 1024 worker / 16 等待位，独立 ID 使用执行额度、同 ID 仍按前驱顺序等待；它最多准入 1040 个外部慢请求，不能把 16 当作总在途上限。应按后端容量选择并发，当前默认未上调；[配置解释与验证边界](review/NEST-FAST-SLOW-2026-09-25.md)。

### 回放、快阶段访问与观测

`dataengine.projection.read_bytes` 对应 `ProjectorOptions.ReplayReadBytes`，默认 4MiB，
限制一次回放保留的逻辑记录字节；与 `projection.batch_bytes` 的投影分段预算独立。
首条超过软限额的记录独占一次读取以保持进度；WAL 解码可能临时读取下一条，故不是精确堆内存上限。
Remote 在已读取记录中，再按 ReplayBatchRecords / ReplayBatchBytes（projection.batch_bytes）限制并行窗口；
worker 数只限制同时在途量，遇到共享 Entity、相同事务或特殊事务即截止。
观察到错误后停止补位，等待已经开始的工作结束，仅确认连续成功前缀。

Nest 快阶段对 Getter 传入 `entity.WithLoadedEntitiesOnly(ctx)`。
快阶段经 ManagerAccess `Get/GetMany`（含动态 Cast）访问未加载目标时，不做 I/O、不等 singleflight，
返回可 `errors.Is` 判别的 `entity.ErrColdLoadInLogic`（快 worker 上同时包裹 `fctx.ErrBlockingInFastWorker`），
业务可以据此降级，例如“好友离线”（RR-20260926-26）；仅带 LoadedEntitiesOnly context 的池外访问同样返回该错误。
真正会等待的入口——EntityRepository 冷加载、投影等待、Remote 准备/确认——在快 worker 上于等待和副作用之前 panic，
由 Nest 转为请求错误。无 loader 时保留 nil/缺失语义。
需要冷目标的业务把目标列入消息的 ID 集合即可：统一准入发现声明目标未加载时自动走慢阶段预加载，
同 ID 顺序仍在准入处建立（RR-20260926-25）。准入判定之后、handler 取得 Guard 之前目标被驱逐（读取、引用、
取锁三个窗口）时，同一条已准入请求原位转到慢池准备，保留同 ID 顺序位置，不重新排队、不在快 worker 上冷加载；
Stats 的慢池 Started 会多计一次，指标 `nest.dispatch.slow_reroute.total` 记录次数。显式 `nest.SendOptionSlow()` 仍然有效。
同一实体的并发冷加载由 ManagerAccess 合并成一次（singleflight）。加载运行在与调用方解耦的 ctx 上（RR-20260926-54）：
保留第一个调用方 ctx 的值，不随任何调用方取消或截止；每个等待方按自己的 ctx 离开并得到自己的 `ctx.Err()`，最后一个
等待方离开也不取消在途加载，完成后实体照常进入 EntityManager，之后的访问直接命中。加载只受两个框架约束：
`entity.DefaultEntityLoadTimeout`（30s，`ManagerAccess.ConfigureLoadTimeout` 可改，kit 配置键 `nest.entity_load_timeout`，缺省或 0 不改、负值拒绝启动，
[RR-20260927-13](bugfix/RR-20260927-13.md)；超出时等待方得到满足
`errors.Is(err, context.DeadlineExceeded)` 与 `entity.ErrEntityLoadTimeout` 的错误）和 loader 注销（DataEngine Runtime
停机时取消在途加载，错误满足 `context.Canceled` 与 `entity.ErrEntityLoaderStopped`）。因此短预算的调用方（如 2s 登录预算）
先超时离开，不再连带同一 flight 里预算更长的等待方；它随后重试会加入仍在进行的那次加载，而不是重新发起。
领头调用方仍在等待时，加载里需要 Entity 锁的发布沿用它自己的本地执行器（Nest 慢阶段即本条消息的快续行）；Nest 之外、
没有指定执行器的领头方若持有实体锁（例如在 `WithGuardScope` 里锁着 X 时冷加载 Y），发布交回领头方自己的 goroutine 执行，
loader 的发布即使还要锁 X 也不会死锁，领头方按自己的 ctx 离开最多晚一个本地步骤；不持锁的领头方，发布在加载 goroutine 上就地执行
（[RR-20260927-27](bugfix/RR-20260927-27.md)）。领头方离开后改走 Nest 绑定给 ManagerAccess 的 `NestMgr.RunLocal`（Nest 构造时经 `LocalExecutorBinder` 自动绑定），仍在快池执行，未绑定时就地执行。
“离开”包括领头方按自己的 ctx 返回，也包括加载完成（或 panic）后返回：loader 在 `LoadEntity` 返回之后仍用加载 ctx 调 `RunLocal`（契约外用法）时同样走这条路径，不会阻塞（[RR-20260927-29](bugfix/RR-20260927-29.md)）。
Nest 只对传入的 Getter 本身做 `LocalExecutorBinder` 类型断言：**包装了 `ManagerAccess` 的自定义 Getter 必须实现并转发
`BindLocalExecutor(run)` 给内层 ManagerAccess**，否则领头方离开后的发布会在加载 goroutine 上就地执行（与没有 Nest 时相同，
不经快池）；正式 kit 装配直接传 ManagerAccess，不受影响（RR-20260926-54）。
未声明的动态 Cast 目标保持只读已加载实体（返回 `ErrColdLoadInLogic`）。自定义 Getter 仍需遵守 `entity.LoadedEntitiesOnly(ctx)`
（快阶段读取用它）。准入判定只调用可选的 `entity.LoadedChecker.IsLoaded(id)`：它在发送方 goroutine（含快 worker 与唯一的延迟派发
goroutine）上执行，不得 I/O、等待或 Touch；未实现它的自定义 Getter 保持 RR-25 之前的行为——准入不判冷、不自动转慢、准入后也不原位转慢，
冷目标返回 `ErrColdLoadInLogic`，需要预加载时用显式 `SendOptionSlow()`（RR-20260926-47）。
不能在持有 Guard 的动态 Cast 中临时加载，也不能在失败后自动重放已执行的 handler。

`NestMgr.Stats().Queue` 分别报告快/慢池 Running、Ready、BlockedOnPredecessor、WaitingForWorker，
以及拒绝、峰值、最老等待与前驱/worker 累计等待；内部续行有独立运行计数。
Ready 包含已预留执行额度但 goroutine 尚未取走的请求；准入与等待统计都计入正在执行的续行。
PeakWaiting / OldestWaiting 是前驱等待和 worker 等待的合并值，累计等待时长分阶段。
Projected 是成功投影尝试数，成功但未 ack 的后缀重放后会再次增加；不能据此推断唯一事务数。
`Sync.Manager.Stats()` 使用增量计数和等待链表，不再获取所有 subject 锁；低频核对用 `AuditStats()`。
两者都允许各阶段间并发推进，需要在静止状态比较精确计数。

九项实现、回归、容量梯度、混合业务与长稳结果及复跑参数见
[三大模块九项实施记录](feature/REFACTOR-2026-09-26-core-nine-items.md)。


## 2026-09-26 提交与生命周期兼容说明

- 正式配置段为 `syncbus:`，旧 `room:` / `sync:` 仍兼容，优先级依次降低。旧段被读取时启动日志告警弃用；被 `syncbus:` 遮住的旧键与不认识的键也会告警；`syncbus.transport` 写错（非 nats / jetstream）启动失败（RR-20260926-12）。
- `roost project upgrade --consolidate` 遇到 v1.16.x 起已删除或换包的框架符号（如 `spatial.InterestManager`、`statesync.Reassembler`、`room.DecodeRoomWireFrame`）时，先完成 import 改写，再逐条列出 `文件:行` 与迁移指引并以非零退出；按指引修改后重跑（RR-20260926-24）。
- `EntityRepository` 在正式 Runtime 中冷加载时等待该实体在途投影，冷目标须声明 Slow；自定义 RecoveryGate 需要转发 `WaitEntityProjection`。
- 多进程按租约交接实体所有权时，交出前（驱逐本地副本之后、释放租约之前）须在慢路径调用 `kit/dataengine` Mod 的 `WaitEntityProjection(ctx, 完整EntityID)`，等待失败则保留租约重试；接手方只读 Mongo，看不到本进程未投影的 WAL。game-demo 的闲置交还已按此实现（RR-20260926-31）。
- Remote 数据提交成功后 Close/hook 错误仍可能返回给请求，不能按“未提交”自动重复业务。未知结果由恢复流程处理。
- 同 SessionID 旧传输仍在退出时，新 Open 返回 `ErrSessionAlreadyExists`；应在退出后重试或使用新连接 ID。
  （2026-09-26 复核补修）现在返回可 `errors.Is` 的 `entitysync.ErrSessionClosing`（仍包装 `nettransport.ErrSessionAlreadyExists`）；同 ID 另一次 Open 正在等待传输确认时返回 `entitysync.ErrSessionOpening`。两者都表示本次没有创建会话、可稍后重试；OpenSession 不阻塞等待，重试应放在慢阶段或下一次调度。传输确认前会话对 Subscribe/Flush 不可见，不再出现假的 SessionLost。
- Remote 写与 lease-fence receipt 同事务暂不支持，DataEngine 在写 WAL 前返回 `ErrRemoteLeaseFenceUnsupported`，由 Nest 锁内回滚。历史 skipped WAL 的 Remote 拒绝结论会被持久记录。
- **原生 saga 步骤的实体屏障（RR-20260926-30，行为收紧）**：本地 mutation + lease fence 的记录准入后、投影结果确定前，写同一实体的其他事务（含系统删档、同一命令的重投）在 WAL 准入处被拒绝，错误可 `errors.Is(err, dataengine.ErrFencedEntityPending)`（同时满足 `nest.ErrCommitRejected`），Nest 已整体回滚。它是**可重试**错误：业务入口应回复“稍后重试”或延迟重投，不能当成业务拒绝；正常只持续一次投影（毫秒级），Mongo 变慢或中断时与投影积压同量级。只读 handler 不受影响；屏障只看写集合。
- 投影时租约已失效的原生步骤被跳过后，DataEngine 在 Nest 快池内驱逐受影响的常驻实体（`EntityManager.Destroy`，`DestroyReasonCommon`，不删库），实体的 `OnDestroy` 会被调用；下次访问从 Mongo 重载。装配了 `NewModWithEntitySync` 的 kit 工程会在重载后调用 `entitysync.Manager.Rebind`，原订阅者收到整份全量；自建 entitysync 的装配需要自己在 `EntityRepository.OnEntityLoaded`（或 kit/dataengine Mod 的同名方法）里调用 `Rebind`，否则驱逐后的 subject 一直停在关闭状态（不发送、不计失败），直到 `Unregister`。`Register` 遇到旧状态已关闭的同 ID subject 也会走同一重新绑定路径。
- **驱逐 / 卸载后订阅者的修正（RR-20260926-59，行为变化）**：订阅者在驱逐前可能已收到被跳过的效果（Remote Durability 1 也可能已收到被拒绝的内容）。实体被仅内存卸载（`ManagerAccess.Unload`，本条与下文 Remote 持久拒绝共用）时同步状态立即关闭；若该 subject 仍有订阅者，框架在快池之外经正式仓储从权威重载实体（等该实体投影/驱逐结束，发布回快池）并 `Rebind`，订阅者收到同一对象的权威全量；权威中没有该实体、重载有界重试（默认 5 次）后仍失败或重载队列（默认 4096）已满时，订阅者收到 `ObjectRemove`（remove 发完前实体又被加载时，`Rebind` / `Register` 按 RR-55 的机制排到退役完成后登记、返回 nil，不再遇到 `ErrSubjectRetiring`）；无订阅者不主动重载。重载就是一次普通的共享冷加载（RR-54：受框架加载上限与 loader 注销约束）。`NewModWithEntitySync` 的 kit 装配自动接线，停机时先停重载（取消在途、不发 remove）；自建装配调用 `entity.ManagerAccess.ConfigureUnloadResync(entitySyncManager, entity.UnloadResyncConfig{})`（发布经 Nest 绑定给 getter 的 `RunLocal`）并在停 Nest 前调用返回的 stop。自定义 loader 报“权威没有”时包 `entity.ErrAuthorityEntityNotFound`（DataEngine 的 `ErrEntityAggregateNotFound` 已满足）或返回 `(nil, nil)`。计数见 `ManagerAccess.UnloadResyncStats()`（[RR-20260926-59](bugfix/RR-20260926-59.md)）。
  kit 配置键（[RR-20260927-13](bugfix/RR-20260927-13.md)，缺省或 0 取框架默认，负值拒绝启动）：`nest.unload_resync.workers`（并发重载上限，默认 4）、
  `nest.unload_resync.attempts`（每实体尝试次数，默认 5）、`nest.unload_resync.queue_capacity`（等待重载的实体数上限，默认 4096，超出立即 remove）；
  单次重载的上限即 `nest.entity_load_timeout`。退避（100ms 起翻倍、上限 2s）不开放配置。
  **最坏延迟（[RR-20260927-14](bugfix/RR-20260927-14.md)）**：一个实体从登记到得出结论（重载成功或退回 remove）最多
  `T_entity = attempts × T_load + Σ_{k=1..attempts-1} min(100ms·2^(k-1), 2s)`，`T_load` 为 `nest.entity_load_timeout`；队列 FIFO，风暴中最后一个被接纳的实体
  最迟在 `ceil((queue_capacity + workers) / workers) × T_entity` 后得出结论，其间订阅者停在旧内容上。默认值：`T_entity = 5 × 30s + 1.5s = 151.5s`，
  上界 `1025 × 151.5s ≈ 43.1 小时`（权威一直不作答、每次加载都等满 30s 时）；加载快速失败时约 `1025 × 1.5s ≈ 26 分钟`。上界假设风暴之后没有新的卸载
  （处理中又被卸载的实体会再排一轮），发布回快池的排队等待另计。需要更短的上界时减小 `queue_capacity`（放不下的立即 remove）、`attempts`、
  `nest.entity_load_timeout` 或增大 `workers`，例如 `workers: 8, attempts: 3, queue_capacity: 256, entity_load_timeout: 5s` 时约
  `33 × 15.3s ≈ 8.4 分钟`。积压看 gauge `entity.unload_resync.backlog`（无标签，进程内全部 ManagerAccess 等待或正在重载的实体数之和，每个接线停止时撤回自己的部分，RR-20260928-01）或 `UnloadResyncStats().Backlog`（单个 ManagerAccess）。
- 运维：`Projector.Stats()` 新增 `FencedEntities`（当前被屏障挡住写入的实体数）、`FencedAdmissionRejected`、`StaleEvictions`；指标 `dataengine.fence.skipped.total` / `dataengine.fence.evictions.started.total` / `dataengine.fence.evictions.failed.total`。`StaleEvictions` 持续增长说明原生步骤的 `LeaseDuration` 短于投影延迟；`FencedEntities` 长时间不为 0 同时 `evictions.failed` 增长说明驱逐失败（如 Nest 已停止或被 fence），实体写入会一直被可重试地拒绝，按 Nest/DataEngine 健康检查处理。
- `nest.NestMgr.RunLocal(ctx, fn)` 是框架后台 goroutine 把需要 Entity 锁的步骤交给快池的正式入口（快 worker 上调用返回 `fctx.ErrBlockingInFastWorker`）；实现 `nest.LocalExecutorBinder` 的 committer 在 `NewEngine` 时自动拿到它，DataEngine 的 Projector 与 kit Mod 已实现。
- 投影事务现在对 lease fence 指向的 claim 文档做条件写（`updated_at`），与 `DataEngineStepInbox` 的过期接管串行化；claim 文档的 `updated_at` 会随投影更新。
- 关闭超时不代表 WAL 目录已释放；等待在途调用退出后再次 Close。`OpenRuntime` 始终关闭自己创建的 WAL。
- `Projector.Close` 之后 `Flush` / `ReplayPass` 返回 `ErrRuntimeStopped`，不能再用“先 Close 停后台循环再手动回放”的写法；需要逐步驱动回放的外部夹具用 `ProjectorOptions.ManualReplay`（不启动后台循环，生产装配不设置），见 [RR-20260926-29](bugfix/RR-20260926-29.md)。
- `ProjectorOptions.OnFatal` 在首次确定性投影冲突后**异步**调用一次；调用前 fatal 已对准入、Flush 和实体等待方可见，回调里可以同步 Close/Shutdown。依赖“Flush 返回前回调已执行完”的代码需改为等待回调，见 [RR-20260926-17 补修](bugfix/RR-20260926-17.md)。

细节与验证边界见 [RR-10～24 修复汇总](review/REVIEW-2026-09-26-release-fixes.md)。

## 2026-09-26 Remote 收尾链路（RR-37/38/39/46）

- 已提交的 Remote 请求若收尾失败（Close 释放不完整、release hook 或 AfterCommit 回调异常），回复错误满足 `errors.Is(err, nest.ErrAfterCommitFailed)`，原因仍可 `errors.Is`；Abort、确认结果未知与拒绝的回复不带该哨兵。判断“是否已提交”请用 `errors.Is`，不要匹配错误文本（[RR-20260926-46](bugfix/RR-20260926-46.md)）；只看 `ErrAfterCommitFailed` 不够，完整顺序见 §4“回复错误判别”表（[RR-20260926-77](bugfix/RR-20260926-77.md)）。纯本地事务同样如此：strict / memory 提交成功、pipelined ticket 持久之后的 release hook、释放或回调失败都带该哨兵，结果未知不带（[RR-20260926-53](bugfix/RR-20260926-53.md)）。已越过提交点的事务，即使错误链里有 `nest.ErrLockTimeout` 也不会被框架自动重新执行（[RR-20260926-49](bugfix/RR-20260926-49.md)）；调用方也不要对带 `ErrAfterCommitFailed` 的回复重试业务。handler 内嵌套的独立事务（`nest.RunIsolatedTransaction`）一旦持久提交（或结果未知），这条消息同样按已越过提交点处理：外层随后失败不再自动重排，回复满足 `errors.Is(err, nest.ErrNestedTransactionCommitted)`（外层原因仍可 `errors.Is`），表示“外层未提交、独立事务已提交”，不要当作什么都没发生重试整笔业务（[RR-20260926-65](bugfix/RR-20260926-65.md)）。外层是可回滚事务（state / undo）时，独立事务不能持久写外层已登记回滚快照的实体（声明目标、外层 Cast / 新建的实体、外层 `MarkPersist` 过的 DAO）：外层失败回滚会把快照恢复到内存、覆盖已持久的结果。这种写在写任何持久记录之前被拒绝，`RunIsolatedTransaction` 返回满足 `errors.Is(err, nest.ErrNestedTransactionRollbackConflict)` 的错误，独立事务自身回滚、什么都没提交，原样重试仍会被拒绝——把写入并入外层事务，或只让独立事务写外层没有捕获的实体（例如在独立事务里 Cast 取得的实体）。外层是 memory handler 时不受此限（[RR-20260926-74](bugfix/RR-20260926-74.md)）。不经 DAO、用 `RollbackTx.AddMutation` 直接加入的原始 mutation 同样适用：按 `EntityID`（DocumentKey 形式按 `Key.ID`）命中外层已快照的实体时返回同一哨兵（[RR-20260927-07](bugfix/RR-20260927-07.md)）。带 Remote 批次（声明了 remote-managed 目标）的消息里不支持嵌套独立事务：`RunIsolatedTransaction` 直接返回满足 `errors.Is(err, nest.ErrNestedTransactionInRemoteMessage)` 的错误，函数体不执行、什么都没提交，Remote 批次只随消息自己的事务提交或撤销；把写入并入消息自己的事务（[RR-20260926-75](bugfix/RR-20260926-75.md)）；`PrepareRemoteWriteBatch` 执行期间批次尚未挂上，是唯一的例外（见 §4 判别表后说明）。消息自己的事务结束后、批次收尾前的收尾阶段（Guard post-release、解锁后回调）里调用同样被拒绝；纯本地消息的收尾阶段里调用照常执行，但按嵌套独立事务处理，不认领消息（[RR-20260926-84](bugfix/RR-20260926-84.md)）。handler 内的独立事务（`RunIsolatedTransaction`，以及 memory handler 内新建事务的 `RunDetachedTransaction`）提交结果未知（`ErrCommitIndeterminate`）时，框架在它返回之前就 fence 所在的 Nest 引擎，与消息自己的事务结果未知相同：`NestMgr.FenceError()` 满足 `nest.ErrNestFenced` 与 `nest.ErrCommitIndeterminate`，此后新请求一律被拒，业务吞掉这个错误也一样（[RR-20260926-76](bugfix/RR-20260926-76.md)）。外层 handler 仍会执行到结束，但它自己的事务不再交给 committer：提交点返回满足 `errors.Is(err, nest.ErrNestFenced)`（与 `nest.ErrCommitRejected`，RR-20260927-32）的错误并回滚外层修改，回复同时带 `ErrNestedTransactionCommitted`（按判别表第 5 行）。旧实现在结果未知来自 DAO `AcceptMutation` 失败（WAL 已接受嵌套记录、并未进入 terminal）时，会把外层记录照常写进 WAL（[RR-20260927-06](bugfix/RR-20260927-06.md)）。
- Durability 1/2（async/strict，以及带 Remote 批次、随 strict 路径提交的 pipelined）的 Remote 写由 WAL 投影器完成确认：投影期间后台收尾不再回源 Mongo、不再隔离实体，投影完成即释放写权限；投影器报告结果未知或超过 `remote_entity.finalize_projection_timeout`（默认 30s）后才回源。同一事务的快照只发布一次（[RR-20260926-38](bugfix/RR-20260926-38.md)）。
- Remote 写被持久拒绝后，框架在释放写权限后把持有被拒绝修改的实例从本进程内存卸载（不删持久数据，业务收到 `OnDestroy(entity.DestroyReasonMemoryUnload)`；DataEngine 驱逐被跳过的原生步骤留下的实体也改用同一原因），下一次访问从权威重新加载，无需重启。释放写权限到卸载完成之间、以及卸载过程中发起的重载，下一写者得到可 `errors.Is(err, entity.ErrRemoteEntityReloading)` 的**可重试**错误（它包裹 `ErrRemoteFenced`，既有 `errors.Is(err, entity.ErrRemoteFenced)` 判断不变；需要区分时先判断 `ErrRemoteEntityReloading`），稍后重试即得到从权威重载的新实例；仍是 `ErrRemoteFenced` 而不是该哨兵的，是真正的 fence / 隔离（例如结果未知、自定义 loader 不支持卸载），不要按短窗口重试处理（[RR-20260926-62](bugfix/RR-20260926-62.md)）。Sync 与 DataEngine 驱逐同一规则：订阅不注销、不发 remove，重载后原订阅者收到全量（kit 自动 Rebind）；不要在 `DestroyReasonMemoryUnload` 的 `OnDestroy` 里 Unregister。自定义 Remote loader 需实现 `entity.IRemoteEntityUnloader` 才有此行为（`ManagerAccess` 已实现）（[RR-20260926-39](bugfix/RR-20260926-39.md)）。带 Remote 批次的 pipelined handler 与 strict 相同：投影器结论为拒绝时同样由后台收尾回滚、隔离、卸载并放行 Sync 门（此前它被拒后实例一直隔离、从不卸载，Sync 门冻结，[RR-20260928-09](bugfix/RR-20260928-09.md)）。〔更正：“不发 remove”只指卸载本身；仍有订阅者时框架随即重载并全量，重载不了才发 remove，见上条 RR-20260926-59。〕
- 本地已提交、而 strict 远端确认超时或 Durability 0 结果未知时，请求返回“结果未知”的错误：Durability 0、strict 等待中投影器报告未知、strict 等待到截止都可 `errors.Is(entity.ErrRemotePersistenceIndeterminate)`，strict 等待到截止时另带 `entity.ErrRemoteCommitTimeout`（RR-20260927-24 起两者并存，之前只带后者，见 §4 判别表第 2 行）；该事务的 Sync 放行与 `AfterCommit` 回调转交 Remote 后台收尾，拿到持久结论后执行一次：已提交则在 Nest 快池执行（时机不早于这次错误回复，可能与回复并发或在其后），被拒绝则不执行且不再冻结同实体后续提交的 Sync——只丢弃被拒绝 Remote 实体本提交的 Sync 内容与兴趣事实，同一事务里已持久提交的本地实体照常同步、其 AOI/关系事实照常生效（[RR-20260926-58](bugfix/RR-20260926-58.md)）；Remote 部分在回复之前已被明确拒绝（Durability 0 直接写被拒、strict 等待中投影器写被拒）时，回复满足 `errors.Is(err, nest.ErrRemotePartRejected)`（本地部分已提交、不得整笔重试，§4 判别表第 4 行，[RR-20260928-03](bugfix/RR-20260928-03.md)）；停机前仍无结论则不执行。延迟执行的 AfterCommit 看到原请求的上下文快照（trace、handler 元数据、请求内写入的 fctx KV），其 Base 不继承已结束请求的取消。**停机 / fence 期间提交后回调可能不执行**：持久结论到达时 Nest 已停机或已 fence，框架不会在后台 goroutine 上执行业务回调，该事务的 AfterCommit 不执行、Sync 门保持冻结（重启后实体从权威重载），计数 `remote_entity.deferred_outcome_not_run_total{outcome}` 并记告警日志；需要可靠副作用的业务应使用持久记录 / outbox，而不是依赖“结果未知”的 AfterCommit 最终执行。不要因“结果未知”在别处重复 AfterCommit 的副作用（[RR-20260926-37](bugfix/RR-20260926-37.md)、[RR-20260926-61](bugfix/RR-20260926-61.md)）。

## 2026-09-26 连接、复制会话与同步总线（RR-52/55/56）

- 生成的玩家 TCP 传输：一次推送里某条连接写失败，这条连接立即被关闭注销（客户端断线重连），其余连接收到则 `PushPlayer` 返回 nil；全部连接都写不进时返回错误且这些连接都已关闭。写之前就被拒绝（ctx 结束、payload 超限）的推送不关闭连接。依赖“任一连接失败即报错”的调用方改看 `ActiveSessions` 或会话关闭事件。已生成工程重新生成 `server_gen.go` 即可（[RR-20260926-52](bugfix/RR-20260926-52.md)）。
- entitysync 新增 `Manager.RegisterAfterRetirement(state, done)`：subject 仍在退役（Leave 之后观察者还欠 ObjectRemove）时把登记排到退役完成，不再返回 `ErrSubjectRetiring`；每个 subject 至多一个排队，再次 `Unregister`、被替换、状态关闭或 Manager 关闭时 done 收到取消。`entitysync.SessionOpenRetryable(err)` 判断 OpenSession 的“稍后重试”错误。game-demo 的 scene 用它们让同 tick 内的快速重连进入复制场景，会话打开遇“旧会话仍在关闭”按 25ms 起翻倍、上限 1s、至多 8 次重试（[RR-20260926-55](bugfix/RR-20260926-55.md)）。
- （2026-09-27）卸载后重载不了、框架退回 remove 的实体（RR-59）重新登记后，`policy.Interest` / `Group` / `Direct` 自动重新提交仍持有的订阅：观察者仍在 AOI / 组内 / 仍绑定时恢复可见（先 remove、再 create），缺席期间已离开或解绑的不恢复；自建政策用 `Manager.NewSubscriptionSourceWithResubmit` 接入，直接 `Manager.Subscribe` 的订阅不恢复（[RR-20260926-70](bugfix/RR-20260926-70.md)）。重新登记后、政策重新提交之前实体又被退回 remove 时，这些订阅在下一次重新登记后照样恢复（[RR-20260926-78](bugfix/RR-20260926-78.md)）。`policy.Direct` 的绑定在会话关闭（含传输失败）或业务 `Unregister` 该实体后随下一次 Flush 删除，不再随历史绑定数增长；会话关闭时实体恰好缺席的绑定不会在实体重新登记后落到同 ID 重开的新连接上（需要继续观看就重新 `Bind`）。自建政策可用 `Manager.NewSubscriptionSourceWithHooks` 的 `Released` 回调得到同样的通知（[RR-20260926-79](bugfix/RR-20260926-79.md)），并用 `SubscribeStamped` 返回的戳与 `ReleasedSubscription.Stamp` 比较、只删除更早的簿记：通知途中在同 ID 重开的会话或重新登记的实体上重新 `Bind`、随后又被退回 remove 的 `Direct` 绑定不再被旧通知删掉，实体重新登记后照常恢复（[RR-20260926-85](bugfix/RR-20260926-85.md)）。卸载退役中调用 `RegisterAfterRetirement` 现在返回 `queued=true` 并由 done 报告结果（[RR-20260926-72](bugfix/RR-20260926-72.md)）。同一状态在退役收尾窗口里重新登记不再丢失脏通知器（[RR-20260926-69](bugfix/RR-20260926-69.md)）。
- syncbus 的 JetStream 流名缺省由 `syncbus.prefix` 派生：`roost.sync`（生成配置）与未配置 prefix 时仍为 `ROOST_SYNC`，其他 prefix 各得其流（`zz3640.sync` → `ZZ3640_SYNC`），`syncbus.stream` 显式配置优先，启动日志 `syncbus mod: started` 输出 `prefix` 与 `stream`。**升级注意**：写了非默认 prefix 却没写 stream 的 JetStream 部署会换到派生的新流；旧 `ROOST_SYNC` 仍占着该 prefix 的 subjects 时启动报 subjects overlap——要沿用旧流与 durable 游标就写 `syncbus.stream: ROOST_SYNC`，要迁移就先确认旧流已消费完（[RR-20260926-56](bugfix/RR-20260926-56.md)）。
