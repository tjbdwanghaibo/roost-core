# Lockstep 客户端接线验收与剩余问题

2026-10-08，基线main `36fb32ab`，独立工作树实施。结论只针对本轮RS v2/生成TCP/C#接线；不是全框架或所有外部资源验收。接入和格式见[客户端说明](../../client/README.md)，决策及升级见[实施文档](../feature/IMPLEMENTATION-2026-10-08-CLIENT-LOCKSTEP.md)。

## 已核验链路

| 场景 | 预期与实际 | 证据 |
| --- | --- | --- |
| Go/C#跨语言 | Go正式wire、C7与Command生成7个金样；C#解包/重封包、uint64最大值与带符号seat、输入链hash一致 | `client/spec/packets.json`；C#测试程序 |
| 命令权限 | 无客户端player字段；已鉴权会话映射至当前Room绑定，重绑后旧会话拒绝，旁观者不能Input/Hash但可Catchup | `TestRoomCommandUsesCurrentAuthenticatedBinding` |
| 注册表 | PB零值不变；同一个路由表，类型不匹配在decoder前拒绝；Notify不伪造响应；注册扩展在Seal前执行，失败不发布Runtime | 正式生成TCP负面网络场景、独立空业务生成工程注册测试 |
| 输入到消费 | C#Input→鉴权/Registry→比赛单所有者→Room.Tick→TCPSender→Lockstep push→C#顺序模拟 | `TestClientSDKRealGeneratedTCP` |
| 缺口恢复 | 夹具主动隐藏帧2..9，frame10触发Next=2的Catchup；使用真实Room History可靠页，1..12每帧只应用一次；Hash和PB穿插沿用同一客户端序列 | 同一真实TCP场景 |
| 客户端失败 | 乱序/重复/补洞、坏包/容量事务式拒绝；模拟在第2帧抛错后终止，不重放第1/2帧；缺口请求去重、停滞重试、游标前进与补齐后停止 | C# `LockstepTests` |
| Notify与Request组合 | 共用发送锁/序列/在途容量；Notify写前取消不关连接，后续PB正常；无应答发送不登记假Pending | C#真实loopback会话回归 |
| 快worker网络边界 | TCP两条发送入口在resolver/socket前返回`fctx.ErrBlockingInFastWorker`，普通调用成功；测试先观察4次副作用的红，再实施拒绝获得绿 | [修前](../feature/evidence/client-lockstep-20261008/fast-red.txt)、[修后](../feature/evidence/client-lockstep-20261008/command-green.txt) |

真实TCP夹具使用独立串行owner channel驱动Room，证明生成传输与现有Room消费；没有用这个夹具冒认业务Nest比赛实体、持久化或真实游戏模拟已装配。Room含TCP发送的Tick须在Entity锁和Nest快worker外由单所有者驱动；不能启动并发goroutine绕过拒绝。

## 全仓失败：保持原始结果

最终`GOWORK=off go test -count=1 ./...`共153个包状态：126通过、22无测试、5失败，**不能称全仓绿**。根包通过。5个失败包如下，均不包含本轮新增逻辑的行为失败；codegen/roost内本轮SDK/注册测试通过，失败来自原有kit配置守卫。

| 失败包/场景 | 本轮与基线观察 | 后续处理 |
| --- | --- | --- |
| `codegen/internal/entity`：Mirror DTO生成夹具差异 | 未修改main同一测试亦失败；提交夹具与生成文本不一致，尚未按平台独立审计根因 | 保留生成守卫，核验夹具/换行后处理，不能自动更新期望冒认修复 |
| `codegen/internal/roost`：KitConfigSchemasMatchKitDeclarations | 未修改main同样报kitconfig_gen.go不是当前声明；本轮SDK生成build/vet、幂等生成及真实消费者通过 | 核对当前kit声明、生成文件及换行，再决定重生成；不是Lockstep注册失败 |
| `kit/statslog`：EntityGaugesReturnToZeroWhenAKindEmpties | 基线与本轮均在TempDir清理时报告stats日志文件被占用 | 按仓库Windows暂存规则保留，不修改无关模块 |
| `log`：RotationFailureKeepsWritingTheCurrentSlice、AFailingConsoleDoesNotStopTheFileSink | 基线与本轮均出现轮转行/计数断言及占用文件清理失败；既有问题回链NC-263 | 不将已修历史当本机再验绿；遵循Windows问题暂存约定 |
| `remoteentity`：SnapshotClientWithoutConfirmedSubscriptionsReadsOnDemand | 基线及最终复跑均读到v1，期望commit后的v2 | 独立Remote复审/修复任务，未改缓存一致性契约 |

第一次扩大运行还观察到Remote并发读者收敛v59/期望v60；基线复跑与最终运行未再次触发，**保留偶发观察，不归因为Lockstep，也不算已修复**。

既有KCP `TestLockstepEndToEndGate`在一次race复跑收尾时记录robot frame302 `context canceled`；首次race、最终全仓以及后续本轮完整三次race均通过，未改main三次定向KCP也通过。源码中的测试先取消共享ctx，再读取bot错误，有关停竞态嫌疑，但没有确定修复，因此保留这次失败与需要确定性复现的边界。一次受限沙箱运行还在KCP E2E等待上达到10分钟包超时；随后允许loopback socket的执行通过，包超时不作为正确性证明。

初次全量还暴露本轮模板新增constructor却未落地字段/类型的生成编译失败，已修正；再次全量没有这些编译失败。教训是Go raw模板所在包编译不能证明生成产物可编译，新增入口必须跑完整正式生成消费者。本轮扩展测试首次混入game-demo未提供的业务依赖，已改为空业务工程独立验收注册，TCP/Room仍在完整demo里执行，没有删失败断言或弱化网络消费。

完整失败、基线对照和最终通过日志（仅空白规范化，前后SHA见manifest）保存在[证据目录](../feature/evidence/client-lockstep-20261008)。没有修改旧RR的修复状态，没有降低断言，没有等待GitHub CI，没有发版。

## 后续边界

- Unity编辑器/IL2CPP、Godot、Unreal/C++实机没有运行；当前运行库为netstandard2.1，测试消费者用.NET 10。
- TCP有队头阻塞及逐会话同步写预算；无公网UDP、HA、长期容量或大规模比赛性能结论。
- 不提供游戏快照、跨比赛epoch或自动跳帧；应用负责比赛生命周期、订阅与恢复，C7本身不含matchID。
- 真实游戏确定性、作弊裁决、主线程帧预算和业务输入生产/状态Hash仍由游戏实现；输入链Hasher仅为诊断。
- 自动追帧重试按收到的包计数；无新包时业务计时器需显式调用Catchup或终止比赛，不能无限假定恢复。
- 图谱仍为2026-09-30旧代际；Verify查询/coverage后逐个读当前源码补证，不宣称图谱覆盖本轮新增实现。

收尾索引更新（2026-10-08）：main源码提交e9626354已全量刷新成功，generation=2026-10-08T00:23:20Z，ready，48620 nodes/294215 edges、0 skipped、65 parse_partial；部分解析仍需按范围补读，不能冒认全覆盖。相关18路径coverage无记录缺口但freshness仍报metadata_changed，已读取main源码并与验收工作树逐个比较LF规范化内容一致，SHA见证据目录main-source-sha256.json；状态与coverage见index-snapshot.json。此前9-30旧代际描述保留为工作开始时点，后续使用此新代际及当前源码补证。
