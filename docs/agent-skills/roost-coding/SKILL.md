---
name: roost-coding
description: roost-core 的代码开发、优化、重构、review 和 bugfix 基本要求。用于编写或审查 Nest、DataEngine、Sync、Remote 及必要调用链；强调少分包、易读 Go、正式业务链路、并发与持久化契约、证据和交接文档。这里是 agent 开发规范，不是游戏内技能系统。
---

# roost 写代码

## 开始工作与接手

roost-core 是采用 ECS 编程模式的通用游戏服务器框架。Entity 是数据与业务逻辑的组织中心，Nest 调度业务，DataEngine 落地数据，Sync 同步实体；Remote 协作维护跨进程权限与事务。不要把所有框架职责堆入 Entity。

1. 定位包含 `go.mod` 的 roost-core 根目录，读取适用 AGENTS.md、Git 分支/工作树、go.mod、go.work 与实际工具链；不要覆盖前序或用户未提交的工作。
2. 在仓库中阅读 `docs/CORE-OPTIMIZATION-HANDOFF.md`，再按任务阅读其中链接的 feature、review、bug、bugfix。用当前源码核对文档，不把旧“待实施”“已完成”或旧目录当成事实。交接文档中的性能是有日期和配置的证据，不是永久保证。
3. 代码发现优先 codebase-memory-mcp：`list_projects` / `index_status` 确认项目及 generation，默认 Verify；`search_graph` 定位，`trace_path` 查相关调用方向，`get_code_snippet` 读实现并处理分页。全部证据路径检查 `check_index_coverage`，否定/穷尽结论还需相应 scopes。
4. metadata_changed、partial、skipped、excluded、unknown 或工具不可用时，读取当前源码补证并说明限制。无记录缺口不等于完整性证明；字符串、配置与文档可直接用 `rg`。实际 import/编译才是依赖判断依据，删除 API 还要检查仓外调用、kit、codegen、模板与正式生成物。
5. 大改后在性能测试结束再刷新索引。刷新被工具阻止时记录原因和旧代际，不删除未知锁、不停止其他实例、不声称已更新。图谱缺失不能阻断已有源码可完成的任务。

仓库中的本文件是规则源；本机安装副本可能较旧，优先核对仓库版本。没有图谱或 skill 自动发现能力的 agent，也可以按本文件和仓库源码完成工作，不得声称调用了不可用工具。

## 当前兼容范围（维护者 2026-10-08）

当前尚未上线，不需要兼容旧逻辑、旧 API、旧配置或旧存储/协议格式。优化时收敛为唯一正式路径，删除已被替代的实现、双写/双读、旧字段转换与兼容开关；同步修改仓内调用方、生成器、模板和测试，不为已生成旧工程保留适配层。发现仓外消费者时列清影响，不擅自改其他项目。维护者宣布上线或明确要求兼容后，重新确定兼容边界。

格式不兼容时提升格式版本，明确拒绝旧版本，在变更说明记录停旧进程、使用新数据目录或经维护者处理旧数据的要求；不能自动清空数据库/WAL、跳过冲突记录或伪造确认。去兼容不等于削弱当前落库、幂等恢复、事务及并发契约。静态 Player 跨机迁移只以已落库数据为准，WAL 只负责保证落库，不实现或登记跨机 WAL 热迁移需求。

## 写给人阅读的代码

- 三大模块保持少量内聚包。优先在同包按职责分文件，不为一个接口、机械转发、单一实现或想象中的扩展新建 manager/service/helper 层。独立协议、依赖隔离、可复用基建可以单独成包，但说明职责和边界；不能为了减少目录硬合并大包。
- 主流程要能顺着读出：输入与前置条件、取得执行权、业务修改、准入/确认、失败处理、释放和回复。命名表达领域事实；用显式状态和错误分支，减少嵌套、隐藏副作用、重复状态及跨文件跳转。
- 函数拆分对应真实业务步骤，不按行数机械拆分。不要为每个步骤制造接口/泛型，不把连续事务过程拆成难追踪的回调链。
- 核心包、类型和关键步骤应有必要的中文注释，说明职责、调用顺序、锁/资源所有权、失败与不确定结果、设计原因。review 时按已核实的当前源码补缺失或过时的关键说明，保持行为不变；显式只读时只记录建议。遵循 Go 文档注释形式，不逐行翻译、不重复堆叠中英文，不把推测或未实施方案写成事实。修改行为时同步更新旧注释和错误信息；生成代码的注释优先在正式模板维护。
- 使用项目当前支持 Go 版本中语义合适的标准库 API 与惯用写法。依据 go.mod/toolchain/CI 和本地 `go doc`、编译确认，不为“现代化”擅自升级最低版本。不引入无证据的对象池、无界缓存、每请求 goroutine 或复杂调度层。
- 业务能力落实到正式 option/config、kit 装配、生成 DAO/Entity 和生命周期。demo 可作为既有调用方验证，不能以 demo 特判代替框架实现。错误保留 `errors.Is` 语义和可定位上下文，指标标签保持低基数。

## 必须保持的执行契约

### Nest 与 Entity

- 快池执行全部业务 handler、Guard/Entity local 锁和本地回滚；慢池仅承担准备 I/O、远端获取/确认/释放等等待。慢池不能直接执行业务或取得 Entity local 锁，初始化/发布/回滚若需锁必须交回快池。
- 同 ID 顺序在统一准入处、按显式声明的全部目标建立；内部续行使用既有准入资格，不能重新排到自身之后。动态 Cast 仍受锁保护，但未声明 ID 不承诺调度 FIFO。
- **快池内不得阻塞等待。** 快 worker 上不做 I/O 等待、不等待在途加载，更不能把任务投递到快池再同步等它（快池 worker 数个这样的请求即全池饥饿死锁，且 Guard 不释放）。“投递到快池并同步等待”（`entity.RunLocal`、快续行）只属于慢 worker；快阶段需要本地步骤时就地执行。慢阶段注入的执行器等上下文不能随快照泄漏进快阶段。框架自带的等待入口在快 worker 上被调用属于编程错误，目标是 fail-fast（RR-20260926-06），不能静默阻塞。**显式豁免**（设计内、不依赖快池、不会饥饿死锁）：锁内 WAL 准入及其持久策略（strict 锁内 fsync，以及回退到 strict 路径的 pipelined——带 Remote 批次、经 `WAL.Append` 提交的记录同样在锁内等 fsync，RR-20260928-11）；pipelined 阶段一在锁外等待 WAL group-commit（`<-ticket.Done()`，含完成泵满时的降级，见 NEST_PIPELINED_COMMIT.md）。豁免清单之外的新等待入口须经评审。
- 冷目标用正式 Slow option 并声明所需 ID，由慢阶段准备。快阶段 Getter 只读已加载实体；自定义 Getter 必须遵守 LoadedEntitiesOnly 契约。不能把执行一半的 handler 搬到慢池或自动重试来掩盖冷加载错误；任意 handler 内的阻塞 RPC 也不会自动隔离。
- 执行位置与请求数据分离：快 worker 标记由接收阶段建立，嵌套 fctx 继承，但 ContextSnapshot 不传递；慢 executor 在快阶段屏蔽、返回慢阶段后恢复。不能只检查传入 ctx 或 msg.getter；直接 ManagerAccess、Repository、生成 lifecycle、保存的慢 ctx 和 Background ctx 都要核对。RunLocal 在实际快阶段就地执行。
- 快池检查须早于等待和副作用。保留 Nest.Request 返回 ErrSyncInHandler 的契约；Guard/本地锁、回滚、Finalize 和既有锁内 WAL 准入是明确豁免，不因本条改变持久语义。列出实际保护的入口，不把定向保护称为全局 I/O 拦截。
- **回滚统一走 DAO**（维护者 2026-10-05 决定 A1，[方案](../../feature/REFACTOR-2026-10-05-dao-unified-rollback.md)）：事务内会改的状态一律放在 DAO（必要时用非持久字段：只同步 `nopersist,sync`、只参与事务 `nopersist,nosync`），组件不得自行维护需要回滚的内存状态，不在组件里调 `RecordUndo` / `RecordUndoToken` / `DeferRollback`。派生值同样是 DAO 字段，由组件里唯一的 derive 在加载与改源字段的事务里写；回滚不是重算触发点。组件需要的索引 / 堆在一次调用内从 DAO 构建，不常驻。`cmd/glsvet` 对组件方法里的 undo 登记打印 `hint:`（不计入失败；含跟进一层同包 helper，RR-20261006-13）；组件方法（`OnInitFinish` / `OnDestroy` 除外，同样跟进一层 helper）给组件自身字段赋值或改字段里的 map / slice 元素也打印 `hint:`——字段是 DAO 句柄（类型名以 `Dao` / `DAO` 结尾）或函数类型（装配的投影、回调）不提示；确属缓存、允许不随事务回滚的字段在声明上一行或行尾写 `//roost:cache`（第十三轮“A1 盲区”，[记录](../../feature/A1-COMPONENT-FIELD-WRITE-HINT-2026-10-06.md)）。手写 DAO 的方法自己登记逆操作是允许的（与生成 setter 同形，如 `skill/combatcomponent.CombatDao`）。**明确例外（维护者决定 B4，2026-10-06）**：`skill.Runtime` 的冷却、ammo、cast 状态、proc 账本、state mutation 流与 revision 不进事务，handler 失败或提交被拒回滚后都不回退；接入方按此设计（先校验、后扣费，或依赖 Runtime 自己的失败终态），不要为它补 undo 或 DAO 化，约束见 `docs/skill/skill-casting-and-combat.md`“Runtime 不在事务里（B4）”。glsvet 的 A1 提示不会命中 Runtime（它不是组件、不登记 undo），无需豁免。
- 准入失败与结果不确定要分清；成功准入后不能因释放/回复失败回滚已接受事务。取消等待不等于撤销业务。保持 Cast、组锁、引用、完成 ticket 和关闭排空的唯一收尾责任。
- worker 数、等待容量与 Remote/数据库写预算分开配置。不能推断慢 worker 越多、队列越短就必然越快。锁内 WAL 准入沿用当前契约，不以移出锁或降低持久级别换吞吐。

### Sync

- setter 只标脏。on_change 在成功准入后、Guard 释放 Entity 锁前冻结数据；全部锁释放且满足提交确认后唤醒 Manager。网络发送在锁外；同一业务多次 setter 不逐次发帧。periodic 是正式默认模式，on_change 以周期兜底，两种模式共享内容/版本/交付语义。
- Interest/AOI 决定可见集合，多来源订阅独立撤销；业务位置/关系事实须受提交与回滚边界约束。不要重新退回“1000 人同一个 room”替代真实目标负载。
- Profile 字段白名单、优先级与 packer 保持一致，未知视图不能默默扩大字段权限。共享不可变编码，不共享可变在途字节；不能拼接不透明 delta。
- 以完整 EntitySync 包为最小组帧单位。可以一帧装多个完整实体包，不能按任意字节切断实体数据。超硬上限明确失败，软预算保证合法大对象最终有进度。
- 只有交付成功的帧前缀才能结算。保持版本/基线、epoch/lifetime、订阅 revision、remove-before-create 及旧在途回调隔离。可靠增量不能 latest-only 丢弃；慢会话失败不能拖垮其他会话。
- 冷创建/重连恢复预算与已有对象更新分开，即时 Flush 共用窗口额度；保持新入场/恢复之间和会话之间的公平。统计的增量索引跟随同一生命周期维护，完整扫描放显式 Audit 入口。
- 预算区分计划预留、实际尝试和成功交付。当前冷预算按会话本批 Push 尝试计费，成功帧另行结算；取消、失效、编码失败和未尝试会话不得被预扣，游标跟随真实尝试且保留计划顺序。改变计费规则时明确退款与重试空转的取舍。
- 公平性不能只看有限积压：加入持续新增小对象、单调/随机 ID、跨会话及 arrival/recovery 的负载。字节预算挡住旧请求时后来的小包不能无限越过它；同会话保留意图入队顺序。声明最终进度须有有界回归，不能靠提高预算通过。当前 recovery 是同一 lifetime 的 Hold/Ready，Close/Open 属于新入场，不凭 SessionID 推断跨生命周期恢复。

### DataEngine 与 Remote

- 保持 Nest → 正式生成 DAO → 文件 WAL → 投影 → 持久确认/发布的完整链路。区分内存修改、WAL 准入、durable、投影、远端发布/确认；不把其中一个阶段的 TPS 作为整条业务吞吐。
- async/strict/pipelined 的完成条件必须明确。回放读取、投影批次、未确认 WAL、Remote 在途写各有预算；不能用 Stats 的瞬时读取替代原子准入。
- 并发投影只用于 Store 明确支持、实体与事务身份独立且无特殊屏障的记录。首次观察失败停止补位，等待已启动任务，checkpoint 只推进连续成功前缀；成功后缀靠持久身份幂等重放。
- 并行错误带每笔失败事务 ID 并保留 errors.Is；不能依赖 goroutine 返回顺序判断某条记录未开始。区分唯一业务提交、成功投影尝试、WAL consume 次数与 ack 水位；成功后缀重放会增加 Projected，不能以 committed == projected 证明一致性。用最终 DAO 值/版本、持久身份、回执和 WAL 排空共同验证。
- Remote 正式持久写权限由 Mongo 的 ownership/最新 grant/fence 与版本共同校验；Redis 是竞争协调。保持多 Entity、多 DAO 原子性、事务 digest、回执及 outbox 重放。不增加测试专用弱校验入口。
- 超时/未知结果不等于未提交或回滚；继续承担恢复和资源释放责任。不能跳过冲突 WAL、自动删坏记录/生产数据、放宽版本比较或伪造持久确认来恢复“绿灯”。

### 生命周期与装配的复审要点

修改提交、回放或关闭时，先区分“数据已提交”“回调失败”“释放失败”“结果未知”。已提交事务不能 Abort；每个完成回调独立处理异常，后续框架 Confirm/Release 仍须执行。未知结果保留恢复责任，不能当成明确拒绝。冷卸载重载必须等待该 Entity 的在途投影，不能只凭 Runtime 启动时曾 Ready 就读取旧库状态。

WAL checkpoint 不得超过持久日志；Close 必须等待外部 Flush/Replay/Commit，超时仍保留目录/资源所有权。创建者负责关闭自己创建的 WAL；测试须覆盖旧实例退出、新实例接管及旧 Ack 被拒绝。同 SessionID 不代表同 lifetime，旧发送未结束时新 Open 必须明确失败或建立独立资源，不能吞注册冲突。

停止入口按三步审（RR-20261004-NC-04 Ops、RR-20261005-NC-83 player TCP）：①发起关闭，幂等；②在调用方 ctx 内等待真实排空，超时返回错误并保留对象，用新 ctx 重试会再等；③排空后才释放对象与依赖。不要用“字段已置空 / once 已执行 / 列表已取走”表示已停——第一次超时后的重试会在工作仍运行时报告成功，并提前释放它的依赖。标准库已提供①②时（`http.Server.Shutdown`）直接用。不配合 ctx 的回调不能被杀，契约只要求停机如实超时并保留责任。

**新的停机对象优先用共用类型，并套用契约测试骨架**（维护者决定 A3，2026-10-05；此前“不为此抽公共框架类型”的说法作废——同一不变量已被打破 11 次）：
- 在途工作的准入 / 计数 / 等待用 `internal/operation.Lifetime`：`Begin`/`End` 包住每次回调或调用，`Stop` 幂等关准入，`Wait(ctx)` 在调用方 ctx 内等排空（超时保留计数、可重试，已排空时任何 ctx 都返回 nil），返回 nil 之后才释放依赖；需要“停后可再启动”的对象每次启动换一个新的 Lifetime（`syncbus.Subscription` 就是每次订阅一个）。不要再手写一份“mutex + running + idle chan”。
- 停止入口（Close / StopWithContext）的并发串行用 `internal/operation.Serial`（等待受后到者自己的 ctx 约束，不用 `sync.Mutex`）。例外：临界区很短、关闭不等在途工作的可以用 `sync.Mutex`，如 kit `RedisMod` 停止持 `mu`——go-redis 的 Close 不等在途命令，持锁时间很短，后到者不会被拖过自己的 ctx（理由见 [RR-20261006-10 修复记录](../../bugfix/RR-20261006-10.md)）；关闭要等排空的仍用 Serial。驱动与 Mod 的 Close 统一口径：重复 Close 返回 nil（首次错误只报给那一次调用），Close 之后的其他调用返回该驱动可 `errors.Is` 的“已关闭”错误（RR-20261006-10；唯一例外是 nats `Assembly.Close` 的 `ErrClosedUndrained` 报一次）。
- 停止入口的回归用 `internal/stopcontract.Check` 写：给出 Start / Block（投递一个卡住的工作）/ Stop(ctx) / Release / Released 钩子，骨架统一断言首次超时返回 ctx 错误且资源保留、未放行时重试仍超时、放行后新 ctx 重试返回 nil 且资源确实释放、再调用返回 nil。`Released` 要观察真实后果（依赖被关闭、登记被撤销、派发器退出），不能读被测对象自己的“已停”字段；自己不持有依赖、由调用方在 nil 之后释放的对象用 `stopcontract.CallerReleases`。生成代码（另一个模块）在 codegen 测试里注入骨架源码运行（`codegen/internal/roost/player_tcp_stop_contract_promises_test.go`）。
- `glsvet` 默认对带 ctx 的停止类函数里不受 ctx 约束的通道接收给出 `hint:`（含跟进一层同包 helper），只提示、不计入违例；看到提示时确认通道一定在预算内关闭，或改成 select ctx / `Lifetime.Wait`。Mutex.Lock 不提示（实测全是短临界区），锁被在途工作长期持有的风险靠契约骨架在行为上覆盖。
- **排空在传输层**（A3 ②，2026-10-07 实施，[方案](../../feature/A3-2-SYNCBUS-DRAINING-UNSUBSCRIBE-2026-10-07.md)）：`ISyncBus.Subscribe` / `SubscribeLive` 返回 `*syncbus.Subscription`，`Unsubscribe(ctx)` 本身就是三步停机——返回 nil 即这个订阅没有在途回调、也不会再有新回调。订阅者不要再为“退订后回调还在跑”自己包一层 Lifetime 准入门，直接等 `Unsubscribe(ctx)`；可重启的订阅者只记还没确认排空的旧订阅（见 `mirror.Replicator`）。handler 签名带投递 ctx：在 handler 里退订自己必须传它（或其派生），否则会等自己。新传输与测试替身一律用 `syncbus.NewSubscription` + `Deliver`，不手写排空。订阅者自己还有别的在途工作（如 `SnapshotClient` 读路径对 L2 / 权威的调用）时，那部分照旧用 Lifetime。

**遍历回调契约**（维护者决定 C7，2026-10-05，[方案](../../feature/C7-RANGE-CALLBACK-CONTRACT-2026-10-06.md)）：凡框架交给业务的 `Range`（container、safemap、`EntityManager` / `ManagerAccess`、生成 DAO 的 `RangeX`），回调里可以读写同一个容器，返回 false 立即停止。具体是：回调里 Get / Set / Delete / Clear / 嵌套 Range 不死锁不 panic；false 之后不再调用回调（跨桶 / 跨分片也停）；不交出不存在的条目（零值键、已被清理的实体），同一键至多一次；开始时就在、期间没被删除的键恰好一次；期间删掉的未到达键在快照实现里可能仍交出，回调里新增的键是否交出不承诺。实现上回调不在容器锁内执行（锁内复制、锁外回调），实体先持引用（`Touch`）再交出。新增或修改遍历入口时用共用辅助 `internal/rangecontract.Check` 套一遍（`container` / `safemap` / `entity` 的 `range_contract_promises_test.go`，生成 DAO 见 `codegen/internal/dao/testdata/runtime/range_contract_test.go`）。不是遍历的回调（`ShardedSafeMap.Read` / `Compute` 持分片锁调用）不适用，回调里不能访问同一个 map。

性能与功能 fixture 应经过正式 kit/Backend 适配链，检查能力声明是否逐层传递。重命名要覆盖旧 import、限定符号、标记及业务文件迁移边界。停机预算的生成值（`shutdown.total_timeout` 与部署宽限期）只计入生成器 Manifest 已知的 Mod：手写 Mod 即使实现 `app.ModStopBudgetProvider.StopBudget` 也不计入，doctor 也不检查它，新增这类 Mod 时须手工调大 total 与宽限期（RR-20260926-66，OPEN-ITEMS C31）。真实时钟可能连续两次读到相同值：用可控时间验证时间策略，不为统计测试增加生产 sleep 或改变门禁。

**配置只经声明读**（维护者决定 A4 ①，2026-10-07，[方案](../../feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md)）：Mod 读的每个配置键写在它的配置结构体上（`config` / `default` / `min` / `max` / `enum` / `required` / `secret` / `example` / `help` tag），实现 `ConfigSchema() app.ConfigSchema { return app.SchemaOf(cfg{}) }`，Init 里 `app.LoadConfig(cfg, &m.cfg)` 一次读完；跨键规则写成配置结构体的 `ValidateConfig(production bool) error`，不在 Init 里散写范围检查与“≤0 取默认”。两个 Mod 读同一组键时共用（嵌入）同一个结构体，声明不一致 App 启动即报错。框架代码（app、kit、生成模板）不直接调 viper 的读方法、不用 `app.ConfigBool` 一类单键读取（`TestFrameworkModsReadConfigOnlyThroughDeclarations`），声明了的字段必须被读（`TestEveryDeclaredConfigFieldIsRead`）；kit Mod 改了声明要 `go generate ./...` 刷新生成器的快照 `codegen/internal/roost/kitconfig_gen.go`，生成的配置段（标了 `example` 的键）与 doctor 的 `config-schema` 检查都从它来，不要再在 codegen 里手写配置字符串。

**业务时钟与系统时钟**（维护者决定 D-L3，2026-10-06，[方案](../../feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md)）：业务时间（活动窗口与协调器、World 定时器、日 / 周重置、冷却、邮件 / 道具业务过期、赛季、排行周期、游戏时间）读 `app.BusinessClock(registry)`（真实时间 + `time.logic_offset`，服务经 `Config.Now` 注入）；帧率、租约与锁、超时、重试退避、存储 TTL、Ack、日志 / 指标 / WAL 时间戳是系统时间，用 `time` 包。Nest / DataEngine / Sync 全部是系统时间，不要改成业务时钟。偏移只在启动时生效、生产强制为 0；**业务时间只许前进**（[方案](../../feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md)）：App 用协调存储里的高水位在启动时拒绝让业务时间回退的偏移，测试环境回到过去只能清库重建，所以不要再为“偏移往回调”在服务里拆系统钟——服务内部、只与业务时间比较的退避和租约读业务时钟，依赖存储服务端 TTL、与读系统钟的进程比较、安全有效期、空间回收、审计的才用系统钟。业务过期不靠存储 TTL 判断，TTL 只兜底且更长。同一服务里两种都有时分开注入（如 chat 的 `Now` / `SystemNow`：展示时间与保留期）。`glsvet` 对 `game` 目录下直接读 `time.Now` / `Since` / `Until` 打印 `hint:`，系统时间写 `//glsvet:system-clock <理由>` 豁免。

当前 Remote mutation 与 lease-fence receipt 混合事务在 WAL 前明确拒绝；不要误以为 generated RollbackRemoteCommit 保存了跨实体前像（它是 no-op）。投影时被跳过 / 持久拒绝后的在线恢复已有定案契约（RR-20260926-30 / 39，维护者批准），新路径沿用它，不另建机制：
- 内存无法证明等于权威时不解冻、不原地“回滚”：原生步骤记录投影结论前，同实体写在 WAL 准入处以可重试的 `dataengine.ErrFencedEntityPending` 屏障；跳过 / 拒绝后经 `NestMgr.RunLocal`（`LocalExecutorBinder` 接线）在快池对受影响实例做仅内存卸载（`ManagerAccess.Unload`，`DestroyReasonMemoryUnload`），下一次访问从权威重载；卸载失败保持隔离并重试。
- Sync：实体在权威里仍存在就换代——关闭旧状态、订阅保持、不发 remove，重载后 `Rebind` 强制全量；事务内新建而权威里不存在的实体才 `RetractSyncSubject`（remove-before-create）。
- Remote 持久拒绝只丢弃被拒绝 Remote 实体的 Sync 内容与事实，同一事务里已持久提交的本地实体照常生效（`SyncMutation.RejectEntities`，RR-20260926-58）；拒绝后卸载 / 重载窗口给写者可重试的 `entity.ErrRemoteEntityReloading`（包裹 `ErrRemoteFenced`，RR-20260926-62）。
- 结果未知时提交后回调随 Remote 收尾交给 finalizer，拿到持久结论后只经快池执行至多一次、带原请求上下文快照；快池拒绝投递（停机 / fence）时不离池执行，只计数告警（RR-20260926-37 / 61）。`IRemoteCommitParticipant.AcknowledgeRemoteCommit` 必须幂等且并发安全，投影器重试与 finalizer 回源发布可能并发确认同一提交（RR-20260926-63）。

历史问题与适用回归见 `docs/review/REVIEW-2026-09-26-release-fixes.md` 与各 RR 修复记录。

## 优化、重构、review 与 bugfix

| 用户任务 | 执行方式 |
| --- | --- |
| 只读分析/只出方案 | 按指定范围读取与交付，不改行为或其他文件 |
| review | 给出触发条件、影响、证据和源码位置；区分确认缺陷、疑点和重构建议。可补已核实的中文注释，但纯 review 不擅自修行为；显式只读时连注释也不修改 |
| 纯重构/泛指优化 | 先形成详细方案；已有“实施/继续”授权时写完方案后直接执行，不重复审批 |
| bugfix/已授权优化 | 复现确认后直接修复当前范围及必要调用链内的 bug，补问题和修复文档；无关重构另列 |

纯重构方案写到既有相关设计或 `docs/feature/REFACTOR-YYYY-MM-DD-<topic>.md`：当前问题和收益、当前/目标目录树、符号迁移及依赖方向、公开 API/协议/持久格式变化、分批步骤、生成物影响、验证与回退。没改结构也明确写原包保留；行为变化单列，不藏在“整理”里。

确认 bug 需要触发条件、预期与实际差异、根因证据和针对行为的回归。优先控制并发事件顺序，不用任意 sleep 制造概率性通过；能做时保留修前失败/修后通过证据，不编造负对照。修根因，避免症状特判。

**疑点按证据处理**（维护者 2026-10-07 确认）：确认的 bug 修复；已排除的疑点记录依据后关闭；证据不足的标记“尚未确认”，写清缺少的证据与下一步定位入口，继续调查，不强行关闭。没复现不等于没有 bug，新增守卫测试通过也不等于原疑点已排除：说明守卫实际保证的不变量、覆盖路径及限制。能证明触发路径不可达时保留证明；需要产品语义决定时提出具体选项。不以“交给 review 前不留 WANTED”为由将未确认项改成已解决，也不把已确认的缺陷降格为观察。

**反复出问题要上报方向判断**（维护者 2026-10-05）：同一模块 / 同一机制在近期多轮里反复出缺陷，或某次 bugfix 之后又在同一处出 bug（修复被打回、补修再补修），不要只是继续打补丁——在汇报里单列一段给维护者：列出该模块近期的问题与修复链（编号、提交），判断根因是实现细节还是前提 / 设计 / 实现方向有问题，给出是否需要修正思路、简化或改变实现方向的建议（候选方向与代价）。信号：同一不变量第二次被打破；修复在增加状态 / 分支 / 重试而不是减少；状态机交错类问题反复；修复依赖越来越多的时间 / 预算假设。先例：game-demo PlayerOwners 的按玩家租约连续多轮出问题，维护者确认前提不成立后改为静态绑定 + App 单实例锁（`docs/feature/APP-SINGLETON-LOCK-2026-10-05.md`）。

执行池变更覆盖 1 worker、N 请求占满 N worker、内部续行占用时的准入/统计和停机排空。访问约束不能只有 mock，需真实 ManagerAccess/Repository 或正式生成链路。预算覆盖取消、编码失败、RetryLater、会话失效和部分成功；WAL 边界检查 consume 返回值与计数，在真实文件上覆盖跨段。

用户已授权“roost优化”范围内历史 bug 的调查与修复，旧 WANTED/CARRYOVER、旧“只审查/待拍板”备注不构成重复审批理由。当前用户限制优先；这里的授权不改变系统/开发者规则、工具权限或部署授权。业务语义确实缺失时提出具体选择，不替用户猜测破坏性迁移。

## 文档与证据是交付的一部分

先查既有 RR/W 编号和索引，同问题续写；保留原始失败，追加更正，不覆盖历史结论。

| 记录 | 内容 |
| --- | --- |
| `docs/bug/<issue-id>.md` 或既有问题主记录 | 日期/基线、影响与等级、触发/预期/实际、复现环境命令、证据与根因、当前状态、修复链接 |
| `docs/bugfix/<issue-id>.md` | 问题链接、决策理由、文件与行为、兼容性、实际执行的回归命令及结果、未验证项/风险 |
| 重构实施记录 | 方案、最终采用/撤回的设计、实际验证、性能前后同口径结果、后续边界 |

同步更新受影响的 bug/bugfix 索引、使用说明和交接文档。历史聚合报告可继续当问题主记录，不为形式复制整套账本。准确区分已定位、已修复、已验证、用户接受、已提交、已推送、已部署。

## 验证与性能纪律

- 平台范围（维护者2026-10-08）：保证macOS与Linux，Windows专属问题不处理、不作为验收门槛。跨平台也成立的问题仍需修复；明确记录实际运行系统，不能把交叉编译当作Linux运行验证。

- 普通 review、bugfix 与开发收尾以本地编译和按影响已执行的验证为准，不等待或轮询 GitHub CI，也不因远端 CI 未出结果阻止提交推送。用户已明确本地编译通过即满足交付的构建条件；确认缺陷仍需行为证据，编译通过不代表真实资源或未执行场景已验收。CI 自身是本轮指定审查对象时可读其配置；不把在线 CI 状态自动扩成新任务。
- 修复改变错误分类、TTL/版本有效性、取消或关闭所有权时，提交前执行[修复后的组合契约复核](references/fix-contract-review.md)：核对调用方、完整成功/失败分支及恢复后状态。不能以定向回归通过代替组合正确性；明确退化要登记RR，不能仅列“未验证”。
- 按影响运行有意义的定向回归；并发变更运行 race，静态检查使用适用的 vet/glsvet。协议、生成器、存储变更补正式生成工程/真实依赖验证；单测不能代替这些验证。纯文档检查链接和 skill 结构，不为文案编写镜像测试。
- **示例要实跑，不能只编译**（2026-10-06 发版前验证）：A1 之后 `skill/examples/statusbridge` 一运行就 panic，`examples/` 模块的 go.sum 也早已缺条目编译不过，build、vet、测试全绿——两个示例目录都是独立模块，根模块的 `go build ./...` 不包含它们。根包 `TestExamplesRun`（`examples_run_test.go`）穷尽发现所有 `examples/` 下的 `main` 包，以 `GOWORK=off` 在各自模块里编译并运行，要求退出码 0、带超时。改了示例用到的 API（skill、robot、configdata、事务 / DAO 形状等）或新增示例时，根包测试必须通过；新示例登记进 `exampleRuns`，只有需要外部依赖的才允许写明理由跳过。示例模块依赖变化时在该模块 `GOWORK=off go mod tidy`。
- 性能使用同机、同配置、同负载、同编译模式的前后对照；profile/trace/race 与正式延迟测量分开，CPU 密集任务和索引重建不要与压测并跑。保存 Git/源码摘要、Go/CPU、持续时间、实体/会话/DAO、变化率、队列/预算、成功/失败/拒绝、p99/max、分配/积压及最终数据校验。
- 区分输入速率与成功完成 TPS、吞吐与延迟、微基准与端到端、同 ID 热点与独立实体。20Hz 状态变化窗口不是 Remote 持久事务频率；进程内解码不是生产网络延迟；短测不是长期容量保证。
- 保留失败和退化样本。不能删样本、换口径、放宽门禁、增加负载特判或无效代码强行过关；优化没有收益就撤回或如实记录代价。用户接受某项偏差时，记录范围、具体数值和决定，保留原始失败结果；不得扩张成以后任何退化都可接受。
- 目前负载背景与已接受指标从交接文档读取，不把 1000/10000/50、20Hz、worker1024 等写死进生产逻辑。新业务目标改变时重新验证。
- 测试写进程级注册表（entity kind / builder、nest 包级 handler、hotcode 等）时，同包 kind 号互不冲突（取新号前先查同包已用号，常量注释写明为什么不能撞号），注册用 `sync.Once`、按用例快照恢复或 `t.Cleanup` 撤销，保证单用例和整包 `-count>1` 都可重复运行；nest 包级 handler 注册后 `t.Cleanup(ResetHandlersForTest)` 或改用实例级 `mgr.RegisterHandlerWithMeta`；不用清空整张表的 `entity.ResetEntityRegistryForTest`（已弃用）。没有集中的 kind 占用清单，以同包源码为准（RR-20260926-82/83、RR-20260927-20，OPEN-ITEMS C31）。
- 本地测试依赖使用隔离库/实例；环境脚本可能含凭据，不输出或提交。共享隔离环境（`~/.roost-it/roost-dataengine-it`）允许多个会话并行：integration 一律加 `-run`，故障注入一律自建代理或进程，全局运维命令（up/down/heal/reset/fault、故障矩阵）必须持有 `remote-acceptance.lock`（维护者决定 A5，规则源 `kit/scripts/integration/README.md`）。将可携带的小型脱敏证据写入 docs，原始 profile、二进制和大日志保留在被忽略的 artifacts。不可用环境/未执行测试明确标记。

## 接力与提交

结束时留下：基线与实际改动、契约/配置影响、验证命令及结果、用户已接受的边界、未解决问题、下一步入口。下一位 agent 无需依赖聊天记录或某台机器的 `/tmp` 文件才能理解当前状态；本地原始产物缺失时给可复跑入口，不能编造重跑成功。

提交前检查 diff、必要测试、生成物和文档一致性，显式选择范围，排除凭据/本地环境/压测二进制。已有用户授权的提交直接完成；push、合并、部署按当前授权范围执行，不把 commit 等同于发布。修改本规范时同步仓库规则源与本机 skill 副本，并更新旧 roost-optimize 入口，避免两套规范漂移。
