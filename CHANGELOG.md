# Changelog

本文件从 v1.6.2 起维护；更早版本见 git 历史。格式遵循 Keep a Changelog，版本号遵循语义化版本。

## [Unreleased]

### Fixed
- `worker` 为队列任务和 `Pool.Go/TryGo` 的每次执行建立并释放全新 `fctx.Context`，不再隐式继承父请求或跨任务泄漏；生命周期外的 `Pool.Go` 不再启动无人追踪的 goroutine。Nest 异步 Dispatch 只保留配置代际、Trace 与 Player/Msg/Seq 框架信封，KV/Base/SyncWait/Frame/事务不再跨异步边界传播，业务数据必须显式放入 Params。
- `cmd/glsvet` 将 Handler 并发约束升级为机器门禁：拒绝裸 `go`、同文件命名 wrapper、非 core worker 的 `.Go`、worker callback 外层捕获和裸忽略 Dispatch/Publish/Submit admission；保留框架既有 `AfterCommit` 合法语义。`app` 对显式 `--config` 以及默认配置的解析/权限错误 fail-closed，仅允许缺失的默认开发配置使用 defaults。
- App 对每个 Mod 分配 shutdown 子预算并隔离 Stop panic；普通错误或 panic 后继续反序停止其他模块，但任一 Mod 超时/取消后立即停止拆除其下游依赖，避免上层仍在运行时关闭底层连接。Provide 失败会回收失败 Mod 自身及此前已装配模块；Registry 批量注册先全量预检再一次提交，避免 capability 半注册。
- 轻量与 JetStream RPC 统一使用带版本、路由身份和业务成败字段的信封；服务端业务错误不再伪装成成功字节。轻量 RPC 默认仅发送一次，调用方取消会中断正在等待的 NATS 请求；RPC handler 的空值和重复注册在启动期显式报错。
- Bus 停止流程不再持有生命周期锁等待订阅/worker 退出，消除正在执行的 handler 回调注册路径与 Stop 互锁；实体引用计数增加溢出/下溢 fail-fast；DLQ 重放使用稳定消息 ID，并在后端支持时逐条删除，发布成功而删除失败后的重试不再制造新业务消息。
- 内存限流器增加默认 10 万 key 硬上限、空闲 TTL 与机会式回收；未知 key 在容量耗尽且无空闲项可回收时 fail-closed，并通过 `Stats` 暴露当前基数、容量拒绝和回收计数，阻断随机身份造成的无界内存增长。
- Nest ticker 测试清理全局 callback 注册，使 `go test -count=N` 可重复执行。

### Added — Data Engine
- `dataengine` 统一持久化契约：事务内 `PersistChange` 生成 Put/Patch/Delete，WAL record 原子携带 receipt/effect，并定义聚合 load、schema migration、tombstone 与 system transaction 接口；迁移和运维边界见 `docs/DATA_ENGINE_MIGRATION.md`。
- Native Saga step 可把 Entity mutation、Command receipt 与 completion effect 绑定到同一 CommitRecord；Remote Entity commit 也改为消费同一事务变更源，并保留 lease/fence/version 语义。

### Changed — Data Engine
- Data Engine tracker 只保留已接受持久化版本和 sync dirty；持久化 dirty 不再写回 DAO。`DurabilityPipelined` 在 WAL Enqueue 接受后、Entity 解锁前推进普通 DAO version。
- Legacy Checkpoint write runtime、dirty contract 与独立包已删除，运行时只保留 Data Engine 单一持久化路径，避免 WAL 与 snapshot 重复落地。

### Added
- `worker.Pool.TryGo`：返回异步任务 admission error；已接纳任务全部纳入 `StopWithContext`。
- `app.ModOptionalDependencyProvider`：声明“安装时需要排在当前 Mod 前、未安装时忽略”的可选依赖；与硬依赖共同拓扑排序并检测依赖环，消除可选集成对业务 Mod 书写顺序的隐式依赖。
- 新增三级文档中心：新手快速开始、熟练开发者完整说明、框架实现原理、生产部署手册和分级路线图。
- `syncstream.FileHistoryJournal` 新增幂等 `Close`，等待已接纳 group commit 后关闭常驻 WAL handle；关闭后持久操作 fail-closed。`History.Close` 将资源释放纳入运行时生命周期，修复重复启停的文件句柄泄漏。
- **`robot` 机器人框架**（从 cube 的 robot 服务拣入并重构，cube 仓库零改动）：模拟客户端逻辑 + 压测双用途。分层：`robot/transport`（统一包协议 `[4B body_len][4B msg_id][4B seq]` 小端、TCP/WebSocket 内置、`RegisterDialer` 扩展点——KCP/QUIC 客户端拨号在 cube-kit `robot` 包）；`robot/protocol`（编解码注册表，`Codec` 注入 + `EnsureEncoder`/`EnsureDecoder` 按方向幂等安装——请求响应共用 msgID 不冲突）；`robot/session`（seq 匹配请求响应、push 分发、幂等关闭，每次 Call 埋 `robot.session.call{msg,result}` 直方图）；`robot`（Context 黑板 + `TypedKey[T]` 类型化访问器、LIFO 关闭钩子、`EnsurePushCapture`、`Coalescer`（去重合帧确认）、`BoundedQueue`（drop-oldest））；`robot/action`（动作注册表 + 内置 connect/wait/wait_push；**`RegisterCall[Req,Resp]` 泛型一行注册调用动作**——请求字段按 json tag/snake_case 从参数与黑板自动填充、`GetCode()` 约定判错、业务只写 `OnResp` 闭包）；`robot/scenario`（行为树组合子 Sequence/Selector/Parallel/Retry/Timeout/加权 Random（按 Seed 确定性），可选 YAML spec 解释器，解析期全量校验）；`robot/runner`（k6 式三执行器 pool/looping（含 `Stages` 分段升降）/arrival-rate，账目不变量 `Started == Success+Failure+Canceled`，10k bot 基准 ~2s/60MB）；`robot/loadtest`（单活跃 run 状态机、`Threshold` SLO 裁决（error_rate/p50–p99，违约即 `StateFailed`+`StopReasonThreshold`）、环形历史、6 条 admin 命令、Markdown 报告；默认指标带 `run` label——同 profile 连跑分布互不污染）。端到端示例 `examples/robotdemo`。
- `obs`：新增 **Histogram** 指标类型——17 个固定指数桶（1ms 起逐桶翻倍），`ObserveHistogram`/`HistogramQuantile`（桶内线性插值）/`HistogramBounds`；Prometheus 导出累积 `_bucket{le}` + `_sum_nanos` + `_count`，可直接喂 `histogram_quantile()`。
- `lockstep`：新增 **`FrameAssembler`**——客户端半场的帧装配器：冗余广播/追帧页去重、严格顺序释放、等待帧到达即时释放并排空连续段（追帧补洞永不被缓冲上限误拒）、缓冲越界返回"该追帧了"错误（`ErrHistoryUnknown` 链）。回归：3 客户端 × 600 帧 × 30% 丢包经冗余愈合零丢帧。

## [1.8.0] - 2026-08

### Fixed（v1.7.1 发布后复审：lockstep 首审 + configdata 修复波自查，共 46 项全部实施，均带回归测试）
- `lockstep`（核心层）：`SubmitWindow` 硬上限 64 + 溢出配置注册期拒绝；`SequencerConfig.MaxInputBytes` 每局可收紧的输入上限（kit Room 据此做传输预算校验）；显式当前帧输入覆盖迟到折入的过期 payload（原先真实操作被静默丢弃）；帧号耗尽显式 panic（一局一 Sequencer）；`RedundantEncoder` 定长环 + `NormalizeRedundancyDepth`（按解码端上限收口）；解码补 `MaxFrameInputs` 与包内帧号严格递增（关闭 ~16× 内存放大）；`History.TrimBefore/FirstID`（长局内存收口）+ 单所有者/最坏内存文档；`DesyncDetector` quorum 改为"同意组大小"语义（少数抢先上报无法定罪）+ Trim 墓碑化（迟到补报不能重建报告集）。房间层修复见 cube-kit CHANGELOG。
- `configdata`（上一轮修复的自查，31 项）：`required`/`ref` 改逐指令解析（原先子串匹配会误拒 `index=required_level` 这类合法 tag、误报合法零值）；嵌入字段实现 encoding/json 的深度遮蔽规则（同名外层胜出、平局丢弃，被遮蔽字段带 cfg tag 报错——原先 key 可能静默取到恒零的内层字段；带 json 名的匿名字段不再被错误提升）；readJSON 包装文档改显式探测（对象目标的包装文档在宽松模式不再静默归零、strict/宽松语义一致；rows 为 null 拒绝；strict 模式补尾部垃圾检测）；listener 的 `Name()` 在 recover 保护内求值（typed-nil 监听器不再逃逸 panic）；版本号改单调分配永不回退（失败/回滚烧号，向监听者暴露过的号永不复用于不同内容）；Rollback 消费 previous 槽（连按两次不再回到坏配置）+ 事件带 `from_version`；全局槽位（defaultStore/fctx）改保存/恢复并加包级锁（跨 Store 并发不再互相抹除、首载失败不再清掉别人的运行时配置）；`ValidateReload` 在 Reload 与 DryRun 间保持单线程（`valMu`，只锁校验阶段——DryRun 的长 build 仍不阻塞紧急 Rollback）；panic 错误保留 `errors.Is` 链并附堆栈，`Must*` panic 改为包装 error；外部聚合指纹改顺序无关（按文件名排序折叠 per-file 摘要，并发读安全）+ 零读取报错（绕过注入 reader 即失败）+ 符号链接解析校验 + `WithExternalValidate` 选项；`finalize` 对全非导出字段的 custom/object 拒绝静默空对象哈希、未消费的指纹名报错、载荷流式写入；`SetFingerprint` 在快照定稿后封印；auto 表 ref 校验整体移入表级 `ValidateTable`（目标解析每 build 一次）+ `WithAutoValidateTable` 选项；回调契约文档化（不得 Goexit）。**行为说明：hash 值再次变化；重复注册与部分宽松解析路径现在报错。**

## [1.7.1] - 2026-08

### Fixed（configdata 三路对抗性复审，39 项发现全部实施，均带回归测试）
- **发布路径重构为单一提交点**（`configdata.Store.commit`）：current/version/defaultStore/fctx 四个状态在锁内一次性推进与回退（此前分五步推进，中途失败留下混合世代——Version 重号、Rollback 跳代、defaultStore 被劫持、typed-nil 进 fctx 槽）。所有 listener 回调、lifecycle emit、def 的 load/build/validate **全部 panic 容器化**（对齐全仓惯例；此前 listener panic 会让 Store 永久失去一致性）。
- **回滚配对语义修正**：`RollbackReload` 与 `BeforeApplyReload` 配对——只对 prepare 成功的监听者按逆序回调（此前 BeforeApply 失败不触发任何回滚、AfterApply 失败却回调从未执行过的监听者）；**`Old == nil`（首次加载失败）跳过全部回滚回调**——监听者永远不会把"没有上一代"误读成"回退到默认配置"。
- **Hash 修复三连**：`finalize` 的 `json.Marshal` 错误不再被吞（含循环引用等不可序列化值时 build 失败并提示 `SetFingerprint`，此前 hash 静默退化为常量）；新增 `Snapshot.SetFingerprint` 显式内容摘要；**`RegisterExternalTables` 对回调实际读到的字节做增量 sha256 作为该聚合的 hash 贡献**（Luban Tables 内部状态非导出，`json.Marshal` 恒为 `{}`，外部表内容漂移此前对 hash 完全不可见）。hash 算法同时改为长度前缀增量计算（消除大字符串物化与定界符歧义）。**升级后 Hash 值会变化。**
- **`RegisterExternalTables` 加固**：build 回调 panic 转为构建错误（Luban 生成的 loader 遇畸形数据习惯 panic，此前 bootstrap 期直接崩进程）；read 闭包在 build 返回后失效（惰性加载会绕过指纹与 fail-fast）；路径检查改用 `filepath.IsLocal`；目录在 build 开始时一次性捕获。
- **auto 表注册期校验补齐到文档承诺**：嵌入（非指针）字段按 encoding/json 语义提升（此前 base 里的 ref/index tag 被静默忽略）；ref/key 字段类型白名单（整数/字符串，bool/float/切片/指针注册期拒绝）；ref 类型兼容性收紧为同 Kind（int64→int32 截断、int→string rune 转换此前会放过悬空引用）；**ref 目标表存在性与类型兼容改为表级前置校验**（`TableDef.ValidateTable` 新 API，空表/全零列不再隐藏拼写错误）；index 重名注册期拒绝（此前两列静默并入同一索引）；新增 `required`（零值即错，抓字段改名导致的整列归零）与 `skipempty`（零值不进索引）指令；错误信息用完整类型名。
- **`Registry` 同名同 kind 注册从静默幂等改为报错**——此前第二个同名定义被静默丢弃、整张表不加载（自动名字推导下极易撞名）。**行为变更：依赖重复注册幂等的装配代码需要清理。**
- **`readJSON` 收紧**：`null`/空文档拒绝（此前静默变成空表）；`rows`/`records`/`data` 多包装键并存拒绝（此前静默取优先者）；新增 `Store.SetStrictJSON`（opt-in 拒绝未知字段——字段改名此前静默整列归零，且被 ref 零值跳过掩护）。
- 其它：`SetDir` 加锁（与 build 的数据竞争）、build 目录单次捕获（三次读取不一致绕过目录校验）、custom 构建移到表/对象校验**之后**（悬空引用报精确校验错误而非 builder 崩溃）、validate 循环响应 ctx 取消、`DryRun` 不再持store 锁（大配置 dry-run 不阻塞紧急 Rollback）、`MustGet` nil 接收者不再裸解引用、`Rollback` 的 lifecycle 事件带 `from_version`。
- kit `configdata` Mod：Rollback 回调把 `configdata.version` gauge 复位到 `Old.Version`（此前失败的 reload 让 gauge 永久停在已回滚的版本上，监控误报成功）。

### Fixed（全量能力审计发现，均带回归测试）
- `hotcode`：`RegisterAdminCommands` 改为注册到调用方传入的 admin **实例**注册表（对齐 bus 的模式，返回聚合错误）——原先注册到包级 default，而装配路径（kit ops HTTP）只读实例注册表，`hotcode.list/revert/load_plugin` 三条命令在生产运维端点不可达。
- `configdata`：`Table` 增加 `MarshalJSON`（表名 + 行数据，文件序）——原先 Table 字段全不可导出，`json.Marshal` 恒为 `{}`，快照 `Hash` 只反映表名集合、改任意行数据 hash 不变，无法用于配置一致性校验。**注意：升级后同一份配置的 Hash 值会变化**（首次真实覆盖内容）。
- `query`：`OrderedIndex` 默认比较器改为类型感知（整数/无符号/浮点/字符串按自然序，其余回退格式化串）——原先统一 `fmt.Sprint` 字典序，整数 key `[9,10]` 排成 `[10,9]`。显式传入 `less` 的行为不变。
- `obs`：指标基数打满不再全静默——首次打满每 metric 记一条 Warn，丢弃数以 `obs.series.dropped{metric}` counter 随 Snapshot/Prometheus 导出（序列数以打满的 metric 名数为界）；`Reset` 同步清零。
- `failurelog`：① 原子 Lua 失败降级为非原子回退时记 `failurelog_degraded_total{namespace,op}` + Warn（对齐 `cache.refhmap` 的"降级必须可见"规范）；② trim/delete 回退优先走新的 `fredis.ListTrimmer`/`ListRemover`（LTRIM/LREM 就地操作）——原先 DEL+RPUSH 两步间崩溃会丢整个列表，无该能力的客户端保留旧回退；③ `failurelog_trim_total` 补上 namespace label。
- `timer`：`Scheduler` 新增 `SetClock` 注入时间源（默认 `time.Now` 行为不变）——原先 `NewTimer` 的 End 用裸墙钟而 Tick 用调用方时钟，`time.logic_offset` 非 0 时所有新建 timer 整体偏移一个 offset；宿主用偏移时钟驱动 Tick 时必须同源注入。
- `misc`：`SafeFuncWithTryCount` 补上与同族 `Safe*` 一致的 panic 恢复、返回包装后的真实末次错误（原先吞掉所有 error 只返回 "try count exceeded"）、`tryCount<=0` 至少执行一次（原先一次都不调用就报错）；`TaskPool` 对非 nil 配置做归一化（`WorkerCount==0` 原先构造空 worker 切片致 `Submit` 除零 panic），哈希取模改在 uint32 空间（32 位平台负索引 panic）。
- `featureflag`：`Replace` 的版本号递增移入临界区（原先先换 map 再递增，读者可能观察到"新数据 + 旧版本号"，基于版本的缓存失效判定会漏）；`Snapshot` 按 Name 排序输出（对齐全仓 List 类方法）。

### Changed
- README 按全量能力审计扩充：能力总览修正分类（taskflow/ai 独立成"实体行为契约"行并指明 runner 在 kit、ownerroute 的"路由 epoch"归位到 entity、`cache`/`httpclient`/`httpserver` 从"接口抽象"改为完整实现三档分类）；实现细节新增第 12–16 条（平台注册表实例优先、缓存分层选型、bus 四条易踩契约、`CaptureSnapshot` 跨 goroutine 正向出路、单所有者组件清单）；"补充契约"新增分布式 fence 三化身、configdata 热更一致性、`errcode.ClientError` 信息隐藏边界；修正第 7 条"时长分布"措辞（obs timer 无分位数）；学习路径补第 11–12 条测试即规格清单。

### Added（v1.7.1）
- `configdata` 配置管线打磨（映射零手写）：① `RegisterAutoTable`——`cfg` struct tag（`key`/`index[=名]`/`ref=表名`）推导全部映射，`ref` 为 Luban 式悬空引用校验（每次 load/reload 进程内执行，零值=无引用），tag 错误一律注册期 fail-fast，读取路径零反射；② `RegisterExternalTables`/`ExternalTablesFrom`——外部生成的表聚合（如 Luban code_go_json 的 Tables）作为快照成员接入，原子热更/回滚/hash/请求一致性全部继承，文件读取限制在数据目录内（拒绝路径逃逸）；③ 新增 `examples/` 模块：`configgen`（cfggen meta→生成→热更/ref 拦截端到端）与 `lubanreal`（**真实 Luban 接入**：gen/ 与导出数据由官方 luban CLI v4.11.0 生成，XML schema + JSON 数据源，`RegisterExternalTables` 接线后热更/回滚全继承，重生成脚本 gen.sh）。配套的 schema 生成器 `cfggen` 在 roost-codegen。

## [1.7.0] - 2026-08

### Added
- `lockstep` 包：帧同步（输入帧）核心，与状态同步（entitysync / kit sync）并列的第三条同步通道。`Sequencer` 乐观帧锁定（到点切帧永不等待、缺席即空输入、迟到折入下一未切帧、提交窗口防未来帧滥用、重复提交幂等）；`RedundantEncoder`/`EncodeBroadcast`/`DecodeBroadcast` 帧冗余广播编码（每报文携带最近 N 帧，丢包靠冗余修复而非重传；解码严格 fail-fast，坏包永不变成静默错帧）；`History` 全量帧历史（追帧分页 + 回放产物）；`DesyncDetector` 关键帧哈希多数派裁决（首报不可改口、法定人数后出裁决）。单帧封包基准 ~0.84µs（验收线 50µs）；30% 丢包仿真零帧缺失。房间与传输接线在 cube-kit 的 `lockstep` 包。
- 可观测性统一：`OBSERVABILITY.md`（命名规范、全仓指标清单、告警基线、Prometheus 导出接线）与 `observability/grafana-roost-overview.json` 总览面板（调度/durability/缓存总线/跨服实体四组）。

## [1.6.3 – 1.6.5] - 2026-08

### Fixed
- `cache/ref_hmap`：Redis Lua 写失败降级为非原子回退时不再静默——记录 `slog.Warn` 并递增 `cache.refhmap.write_degraded_total` 指标（降级行为本身保留，可用性优先）。
- `entity/ManagerAccess`：冷缓存加载增加 single-flight 合并——并发请求同一实体只发出一次 `LoadEntity`，消除热实体冷启动对数据库的惊群；失败航班共享错误且立即移除（重试触发全新加载），等待者可被自身 context 取消。

### Added
- CI 增加 `release-hygiene` 门禁：module 路径必须能被 `git ls-remote` 解析、版本 tag 必须与 module major 后缀匹配。
- `nest`：每 handler 锁内耗时指标 `nest.handler.lock_hold`（pipelined 提前放锁按提前点计），超阈值（`NestOptionWithSlowLockThreshold`，默认 100ms）计 `nest.handler.lock_hold.slow.total` 并告警——`DurabilityPipelined` 灰度对象的选择依据。
- `cmd/glsvet`：静态检查器，扫描 `go` 语句内对 goroutine 绑定 API（RecordUndo/CurrentRollbackTx/fctx.CurrentContext 等）的调用；已接入本仓库 CI。
- `NEST_PIPELINED_COMMIT.md` §12：pipelined 灰度扩大到默认提交档的四步路线（含量化门槛与回退开关）。
- `nest`：实例作用域 handler 注册 `(*NestMgr).RegisterHandlerWithMeta`/`MustRegisterHandlerWithMeta`（Start 前有效；实例优先、全局注册表兜底）——测试与多引擎进程不再共享包级 handler 表。
- `saga`：`NewEngine` 的配置拒绝逐条给出具体字段、实际值与被违反的预算计算式（原先 8 类错误共用一句 "unsafe engine limits"）。

### Fixed
- `nest/ticker`：tick 回调改为每 tick 读取实时注册表（按注册顺序执行）——原先 `NewTicker` 做构造期快照，引擎启动后注册的回调**静默不执行**，且执行顺序来自 map 迭代（跨进程不定）。
- `syncstream/file_journal`：`Record` 从"每条 open+fsync+close"改为常驻句柄 + leader 合批组提交——"返回即持久"语义不变，fsync 次数从每条降为每批；generation 轮转时释放句柄。

## [1.6.2] - 2026-08

- 修复 pipelined 完成泵：降级路径改为按实体完成链（链序 = LSN 序），三条路径统一等待前驱——降级只牺牲延迟不牺牲同实体完成顺序；submit/stop TOCTOU 加固；stop 超时不再泄漏池。
- v1.5.0–v1.6.x 主线：实体锁由自旋改停车信号量、`DurabilityPipelined` 两阶段提交（锁内仅 append、fsync 锁外、外化闸门）、checkpoint FlushAll 唤醒修复、saga Incarnation 折入 CommandID、bus/worker/sync 竞态修复等，详见 `NEST_PIPELINED_COMMIT.md` 与 git 历史。
