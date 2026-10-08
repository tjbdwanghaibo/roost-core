# 历史留项收口方案

承接 [outbox方案](REFACTOR-2026-10-08-outbox-single-publisher.md) 与 [历史清单](../bug/CARRYOVER.md)。维护者已授权逐项收敛，平台仅macOS/Linux，不做部署。维护者最新授权继续全部非压测工作，负载仍暂停，见 [中断记录](../review/OUTBOX-PARTIAL-LOAD-2026-10-08.md)。

## A1：历史坏 WAL 的隔离与恢复

在现有 `nestwal` 内提供只读检查能力，命令入口放 `cmd/walinspect`，不另建运维框架。检查时取得目录现有writer锁，拒绝活跃writer；不能调用会修尾或创建文件的 `Open` 来检查原件。按现有frame/CRC/记录校验读取，报告segment、offset、最后合法位置和错误；遇坏帧停止，不猜测下一个魔数并跳过字节。

隔离首先是停机后复制完整WAL目录到全新目标，保留checkpoint、坏记录与后缀，生成文件哈希和诊断清单；不自动删除记录、推进checkpoint或伪造确认。业务语义坏记录需以原事务ID、权威状态、回执核对后修复产生原因，再按正常恢复路径重放。没有依据时不得把“能启动”当作数据恢复成功。

验收：活跃writer拒绝；完整/截断/CRC损坏/语义坏记录；原件字节不变；快照包含后缀；目标已存在拒绝；取消不破坏原件。当前状态：`nestwal.Inspect`、`cmd/walinspect`已实施，普通测试及race三轮通过；[工具与恢复边界](WAL-INSPECTION-2026-10-08.md)。

## A2：客户端缺口检测与全量恢复

历史 `SessionSequence` 对应的旧协议已变化，当前以 `Frame` 的room、epoch、tick、baseTick与Full/Delta为准，不重新引入第二套序号。正式客户端应在应用Delta之前核对其baseline，只在应用成功后推进；首帧需要Full，旧代/重复不覆盖新状态，缺口明确进入需要全量的状态。

恢复优先复用服务端已有的会话Hold/Ready或断线重连全量能力；客户端不得把不连续Delta当作成功消费，也不能新增无鉴权的任意全量请求。应用回调失败与协议缺口都保留明确错误，由连接层发起正式恢复。实施时进一步收敛为每接收器一个连接中的一个流，固定元数据，不建立room字典或历史帧缓存。

验收应覆盖首Delta、跳帧、旧代迟到、重复、回调失败、重连Full及回调中切连接；Go协议与正式C# SDK/连接消费都要覆盖。当前状态：Go Receiver、C# SyncReceiver、Unity失败关闭接线已实施；Go普通/race三轮、C#功能测试、直接链接Unity正式适配源码的真实TCP测试通过。真实Unity引擎/IL2CPP仍未验。只在业务应用成功后推进，应用失败保留需要Full状态；回调中Reset用代际隔离旧回调，不能恢复旧基线。[正式接入说明](../../client/README.md)。不以解码golden或Lockstep追帧代替EntitySync验收，不声称已提供全部业务packer应用模型。

## A6/A7：移交与在途工作

旧玩家按实体租约/`handBackIdle` 的设计已被维护者2026-10-05的 [静态绑定](PLAYEROWNER-STATIC-BINDING-2026-10-05.md) 和 [App单实例锁](APP-SINGLETON-LOCK-2026-10-05.md) 取代。旧租约路径的Admit/Done及近似空闲移交不再作为当前实现任务，不恢复已废弃状态机。

通用Remote的 `TransferRemoteOwnership` 是另一条正式契约：`beginOwnershipTransition` 等writeGate、刷新权威、Shared模式持分布式锁，再CAS推进owner/epoch；确定移交后旧方Fenced，未知结果经权威回读裁决。本轮Mirror S6已有本地多进程验证。收口时继续核对在途写从准入到终态持有gate的组合契约，不把一条转移成功用例当作所有故障均覆盖。

维护者2026-10-08进一步定案：静态Player跨机迁移全部以已落库数据为准；WAL只负责保证落库，不参与跨机迁移。跨机本地WAL热迁移没有需求，不列为未完成项或后续能力建设，不扩展相关实现与验收场景；既有WAL落库与恢复职责保持不变。沿用已定“不做模块级fencing”和本轮“不考虑部署”的边界。当前状态：旧方向已取代；Remote验证及明确边界在最终清单单列。

## A8：Entity销毁等待可取消

修前 `EntityManager.Destroy` 只在进入时检查ctx，之后 `mu.Lock()` 可无限等待。补测要在初次ctx检查之后设置屏障、让另一goroutine持Entity锁，再取消；要求取消返回时实体仍可服务、没有删除准入、锁与引用未泄漏。

已在 `lock` 包增加取消感知的取锁入口，两个内建channel锁直接select上下文，保留可重入契约。避免改 `Mutex` 必选方法导致外部实现断编译；旧实现适配使用其有界取锁契约，不为每个等待起goroutine。拿锁同时取消时复查并归还本次锁层级。

Destroy只在取得执行权之前响应取消，且在删除准入前复查。准入已成功或结果未知后仍须完成索引隔离与生命周期收尾，不能回滚可能持久的删除。`OnDestroy` 等用户回调不是可强杀任务，不承诺取消能中断任意回调。

当前状态：RR-20261008-46已先红后绿，memory与durable持锁取消反例确认；普通与race三轮通过。配套验证包括持锁取消、相同goroutine重入、已接受删除后取消、未知删除结果、Deferred和回调panic释放。

## A14与N03/N04

保留 `kit` 的装配边界，物理合仓不等于应把所有包平铺。当前无新的行为收益依据，不做仅改import路径的打散；`TestCoreDependencyBoundary` 继续约束core、kit、codegen依赖方向，不能用它代替业务测试。

Layered容量候选已由RR-20261008-42复现修复；N03/N04已具名完成的真实资源证据引用原记录。本机etcd三节点实际Raft leader强杀已通过Watch、发现跨TTL存活与选主fence验证。最终版本Remote矩阵与Linux定向验证已通过，见[收口验收](../review/OUTBOX-HISTORICAL-CLOSURE-2026-10-08.md)；Sync负载继续暂停。跨主机物理网络、真实掉电及部署验收明确排除于本轮，保留边界；不能通过改文档把尚缺的本机测试判成通过。

最终在CARRYOVER顶部建立当前唯一状态表，逐条标明已修、已验、旧方向被取代或本轮排除；原始日期段保留。本轮约定的非压测实现与具名验收已完成；负载仍暂停，部署/跨主机等边界不扩称已验。
