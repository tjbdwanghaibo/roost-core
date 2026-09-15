# Lockstep 与 Sync：覆盖状态及后续优先级

09-16：journal 故障注入 + syncbus Patch/Delivery 新 28 场景，26 通过、2 失败，新 RR-20260916-01。下一入口真实 JetStream/NATS 确认、重连和订阅关闭，保留 journal 真实故障/多实例及其他消费链缺口。旧问题按用户声明跳过。[运行](REVIEW-2026-09-16.md)。

09-15 第八轮：History Import/Restore/Recover/checkpoint 新 29 场景，23 通过、6 失败，新 RR-20260915-08/09。旧问题按用户声明跳过；下一入口 journal 同步/发布失败语义及 syncbus 业务恢复，保留其他未查缺口。[运行](REVIEW-2026-09-15-08.md)。

09-15 第七轮：syncstream History/ACK/journal 新 20 场景 16 通过、4 失败，新 RR-20260915-06/07。下一入口 Recover 并发快照、Import/Restore 与 journal、checkpoint 边界；旧修复跳过，保留 room/state 消费链缺口，不标整包完成。[运行](REVIEW-2026-09-15-07.md)。

09-15 第六轮：room 生命周期 + syncstream 分片/发布新 24 场景，22 通过、2 失败确认 RR-20260915-05。syncbus 错误不重试是明确契约。下一入口 History/ACK/epoch/journal 与应用恢复；保留 room 旧回调/跨 room 剔除、statesync 外部恢复缺口。旧修复跳过，未标整包完成。[运行](REVIEW-2026-09-15-06.md)。

09-15 第五轮：room → RoomTransportSink → AsyncTransport 新增 21 场景，19 通过、2 失败确认 RR-20260915-04；旧修复跳过。下一入口跨 room session/释放/回调关闭，再 syncstream/syncbus。保留 statesync 真实消费者恢复缺口，不标整包完成。[运行](REVIEW-2026-09-15-05.md)。

09-15 第四轮：entitysync 失败重试/profile/批次/关闭/分片取消新增 17 场景通过，无新增确定 RR，旧修复跳过验收。下一入口真实 sink 原子准入、room.flushStateBatch，再 syncstream/syncbus；保留 statesync 外部客户端和运输恢复缺口，不标任何整包完成。[运行](REVIEW-2026-09-15-04.md)。

09-15 第三轮：保留 statesync 外部客户端/真实运输恢复缺口，转入 entitysync 新内容；订阅与持久化水位五场景三失败两通过，新增 RR-20260915-03。旧修复未核验，下一入口 profile/失败重试与生命周期，再 syncstream/syncbus。[运行](REVIEW-2026-09-15-03.md)。

09-15 第二轮：五项旧 statesync 原触发已验收（24 场景通过），重叠准备的接收后收敛检查新增 RR-20260915-02。不能由 stale 拒绝推导客户端已恢复。继续运输/恢复交接，再 entitysync → syncstream/syncbus。[运行](REVIEW-2026-09-15-02.md)。

09-15：statesync 生命周期六场景两失败四通过，新增 RR-20260915-01；旧修复未验收。下一入口消费者/恢复与业务 schema，再 entitysync → syncstream/syncbus，不标整包完成。[运行](REVIEW-2026-09-15.md)。

第八轮（Core 215fffa）：LOD/历史/PreparedFrame 七场景六通过一失败，新增 RR-13；旧问题按用户要求跳过复核。下一入口为投影生命周期和客户端恢复交接，再 entitysync → syncstream/syncbus，statesync 不标完成。[运行](REVIEW-2026-09-14-08.md)。

第七轮（Core a123605）：statesync 恢复/重组九场景已验证部分，新增 RR-11/12；RR-10 用户声明未修复，跳过复核。下一入口 LOD/历史淘汰与并发恢复，然后 entitysync、syncstream/syncbus。未将 statesync 标完成。[运行](REVIEW-2026-09-14-07.md)。

第六轮更新（Core 2c5469e）：RR-09 已验收；用户发现 U-0199 已复核，校验顺序与平台边界漏审见[复盘](POSTMORTEM-LOCKSTEP-U0199.md)。有界审查已转入 statesync，投影/ACK 已验证部分场景并新增 RR-10，下一入口为恢复屏障与分片重组。Lockstep 不标完整收敛，回绕/业务恢复保留缺口。以下是此前时点记录。

更新：2026-09-14 第五轮，Core `a1245fd`。用户顺序：先补完 lockstep，再优先未完成的 sync，随后回到通用 Roost review。

第五轮增量：RR-08 固定身份环原触发通过；真实 TCP 写后报错的追帧重投与取消恢复通过。消费者 Simulate/SubmitInput/ReportHash 三种中断后跳帧已确认 RR-09，下一轮优先验收。仍未完成生产网络拥塞、真实模拟/快照恢复；sync 尚未新展开。[第五轮证据](REVIEW-2026-09-14-05.md)。

## Lockstep 是否完成

**六个生产文件的当前实现已逐项阅读，但整体风险验证未完成，不能标记完整收敛。** RR-04..08 原触发已独立验收，当前新增消费者 RR-09；超期限重放与真实恢复尚有边界。审查完成度与修复完成度分开，发现问题不表示此前未读代码，也不表示修复记录能代替验证。

| 范围 | 已有证据 | 待查/限制 |
| --- | --- | --- |
| sequencer.go | 输入/切帧、配置、原身份重传、未来乱序上游回归、4096 旧身份实验；RR-08 原上限验收通过 | 超 ReplayHorizon 的身份与回执协议 |
| room.go | 绑定、追帧收敛、重绑、失败清理、关闭、当前修复 | 可靠网络发送阻塞/拥塞与宿主恢复行为 |
| history.go | 共享视图契约、裁剪、序列约束、追帧源 | 大历史频繁裁剪成本；业务快照恢复 |
| assembler.go / wire.go | 顺序释放、补洞、重复、配置与解码界限；真实 UDP 回环 | 更系统的变异输入及实际客户端兼容 |
| desync.go | 多数/首次报告/裁剪实现阅读及相关回归 | 默认输入 hash 不能代替真实模拟一致性与反作弊证明 |
| robot/lockstep.go | 消费者源码、三处回调失败后继续跳帧的独立复现；包 race | RR-09 修复与恢复/终止契约验收 |

本机微基准与 UDP/TCP 回环仅补充局部证据，不代表 Linux 多进程生产验收。优先处理 RR-09 和上表剩余可验证入口；跨进程或业务模拟缺少装配时明确记录缺口，不无限重复相同测试，也不擅自补实现。

## Sync 是否完成

**没有完整审查完成证据。** 当前目录存在 statesync、entitysync、syncstream、syncbus；README 仍出现 kit/sync 等旧称，不能直接用旧路径代替当前实现。未确认这些名称是同一模块的简单重命名。

| 当前范围 | 历史证据 | 本轮处理/下一入口 |
| --- | --- | --- |
| statesync | ledger 有 U-0067 编解码守卫验证 | 图谱定位 Replicator、ApplyDelta、Reassembler；未新增源码审查，优先 ACK 基线、分片/重组、resync |
| entitysync | 历史架构/remote 轮及 U-0104 订阅守卫 | 部分覆盖；补订阅切换、LOD、删除与失败重试 |
| syncstream | U-0126/0127 导入/恢复守卫及旧轮记录 | 不等于整包完成；补持久化、重放和检查点交接 |
| syncbus | 09-13 第六轮生命周期阅读/测试 | 尚未真实 broker 验证；解绑与回调完成 |

用户本轮已确认：后续按 statesync → entitysync → syncstream/syncbus 排队。本轮仅盘点 sync 覆盖与入口，没有把盘点写成新一轮 sync 代码验证。lockstep 仍有本轮新问题，故继续保持其优先，不抢先宣布已经转完 sync。

证据：[本轮运行](REVIEW-2026-09-14-04.md)、[全局进度](PROGRESS.md)、[原协议待办](OPEN-QUESTIONS.md)。
