# 非三大核心第四批：N03 RPC 协议、预算和退出责任

2026-10-04。先完成[NC-05～07三项修复](REVIEW-2026-10-04-noncore-05.md)，提交`49796514`，再整理本轮N03审查结果。执行时为`c4aa1e7d`加同一请求链修复工作树；N03源文没有变化，blob/实际SHA256见[manifest](evidence/noncore-review-20261004-06/source-manifest.json)。新增[NC-08～10三项未修P2](../bug/REVIEW-2026-10-04-noncore-06.md)，本批不改通信产品源码。

## 范围与进度

N03清单39个生产Go文件，本批完整读取 **25/39**：Bus8、NATS13、ServiceRPC3及etcd/discovery接口1。另完整读取Kit NatsMod1，按必要主链读worker.Pool Start/Stop/Dispatch和etcd.driver.Discover，后二者只记录范围，不升级为整文件已读。剩余14个etcd文件（含本批只读Discover的driver/discovery）继续接续，不能将其图谱盘点等同源码完成。Nest/Sync/DataEngine由另一agent处理，本批不改这些域。

| 主链 | 实际读取/验证 | 状态/余项 |
| --- | --- | --- |
| Bus装配/生命周期/handler、RPC envelope | bus8全部源文；两种传输、start失败清理、pool资源/回包、DLQ/inbox及Admin接入 | 源码已读，场景部分；JS无handler协议NC-08未修，DLQ真实Redis与关闭竞态仍缺 |
| NATS client/RPC/JetStream/配置/subject | nats13全部源文；pending唯一terminal、callback队列退回、重试、错误映射、drain、settle/Stop | 源码已读，场景部分；NC-09未修，连接/重连/满pending容量未实测 |
| ServiceRPC routing/status/affinity | servicerpc3全部源文，轻量/可靠选项、有效候选、原deadline/业务status | 源码已读，场景部分；NC-10未修，真实发现与生成服务往返待补 |
| Kit NatsMod与shared boundary | 完整配置/Provide/Start/Stop转发、etcd接口与Discover、Pool停止/投递范围 | 装配证据；未connected Provide/停止、未声明整个etcd/worker阶段完成 |

历史Service/NATS/Codegen专项用于识别边界，并保留原SHA；本批不是重做Service十域或全部生成器验收。现行async ReliableStore语义、JetStream settle和已有RPC callback guard从当前源文核对，不能凭旧测试路径引用宣布已全验。

## 实跑证据

最终overlay **17个叶子/独立项=4失败/13控制通过、3根因**。初跑16项3失败，随后仅为ServiceRPC追加实际150ms等待反例，重跑该包；[初始日志](evidence/noncore-review-20261004-06/initial-review.jsonl)保留，[最终日志](evidence/noncore-review-20261004-06/review.jsonl)按最后每包执行合并。六包既有race/vet通过：bus、nats、nats/driver、servicerpc、kit/nats、worker，93 test pass事件、0fail/skip。[复跑](evidence/noncore-review-20261004-06/README.md)。

JS反例进入真实request处理并用实际publish字节解码，broker是capture；Assembly使用真实RPC pool、确认callback已入场后由门闩控制退出，未连接NATS；ServiceRPC discovery为协作替身并检查deadline/实际等待，未跑etcd。没有编译失败作为反例、无挂住未回收的测试进程。没有真实外部资源、跨机HA、生产长稳或性能benchmark。

## 图谱与补证

Tier2 Verify，roost-core root/ready确认，generation仍`2026-09-30T11:58:14Z`。全域结构搜索Bus281/NATS199/ServiceRPC67/etcd365节点，Bus/NATS/etcd续页完整至has_more=false；File节点分别15/25/5/26（含测试，非生产分母），KitNats22/KitEtcd10。Kitbus/servicerpc搜索零结果不作不存在证明；最初file_pattern误用regex零结果已改为glob，不据此做负结论。

onJetStreamRPCRequest、CallDiscoveredChecked、PickServer及driver.Close/Stop双向depth1与关键片段核对；同名方法/heuristic边含误配，关键Assembly.Close按具体文件源码确认，不从边数推定完整caller。覆盖[30证据路径及追加路径](evidence/noncore-review-20261004-06/graph-coverage.json)无记录gap但metadata_changed/new附件not_tracked，现行源文与实际测试补证；未停止或重建共享索引。

## 设计、性能与下一入口

[通信机制学习](IMPLEMENTATION-MESSAGING-RPC-BUDGET-AND-TERMINAL-OWNERSHIP.md)解释发现/传输/远端业务、等待取消/实际退出、结果不确定与唯一完成责任；修法优先复用encodeRPCFailure、child context、既有Pool.StopWithContext。Async pending容量、affinity O(n²)、重投幂等均为具名观察，不虚构性能结论。

下一批优先 **etcd/driver/discovery、watcher、election、local_mirror及订阅**，跟KitEtcd，接续snapshot/watch readiness、lease恢复、CAS与关闭；同时保留本批JS真实往返、关闭fallback/满队列和RPC预算余项。N01/N02仍场景部分完成，N03亦未收口；[计划](NONCORE-REVIEW-PLAN-2026-10-03.md)保持风险预算、不按25/39扣小时或计算全仓覆盖。新三项未修，与旧三项已修分开，未发版。
