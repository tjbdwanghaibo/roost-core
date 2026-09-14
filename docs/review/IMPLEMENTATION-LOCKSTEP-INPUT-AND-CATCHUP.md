# Lockstep：输入身份、权威帧与追帧收敛

第四轮 Core `30a6b5b`：原四项已验收，当前实现先按 accepted[player][original] 去重，拒绝 batch=1、负座位、超 MaxFrameInputs。accepted 仅在 Advance 修剪，两次 Tick 间可被大量不同旧身份撑大，新增 RR-08；超过 ReplayHorizon 的重传仍允许再次折入，不能当作无限幂等。真实 UDP 回环 24 帧（主动丢弃两个已收到包）恢复通过。两个 100x 微基准约 8.4—8.8 μs/op，但包含提交/切帧，且发送器为空实现，不能推算生产吞吐。[本轮证据与限制](REVIEW-2026-09-14-04.md) · [覆盖清单](LOCKSTEP-AND-SYNC-COVERAGE.md)。以下保留第三轮基线。

2026-09-14，Core `a1455fb63903e5f1e4d1e7741ed13d52533bebf9`。已读 sequencer、room、history、assembler、wire、desync，十个测试叶子及包 race；部分场景验证。

## 数据流与责任

Room 是单比赛的单线程所有者；Attach、SubmitInput、Tick、ReportHash、Trim 应由同一串行处理器调用。无内部锁是明确契约，不将并发直接调用造成的竞争当成本轮缺陷。

SubmitInput 校验玩家、大小与未来窗口，存入 pending；Advance 按 PlayerID 排序产生权威 Frame、删除该帧 pending。Tick 先推进并写 History，再编码最近 N 帧、发送；发送失败不撤销权威帧。客户端 Assembler 缓冲乱序帧、连续释放并去重已释放帧。

下行 FrameID 去重无法阻止同一上行输入被服务器放进不同 FrameID。late-fold 丢失原始帧身份，是 RR-20260914-04 的根因。持续方向状态可能掩盖重复，开火/释放技能等边沿事件则需要明确命令身份与执行次数。

## 历史与追赶

History 是内存输入日志，ReadRange/Replay 返回共享视图，不可修改。TrimBefore 复制保留部分到新 backing array；裁剪意味着不能从比赛起点重放，此路径没有自动游戏状态快照。

StartCatchup 记录可靠通道 cursor，Tick 跳过该 session 的 live 发送，再分页补历史。成功前移、失败计数，达到阈值删除 cursor 并报错；非零 cursor 早于历史起点时报 ErrCatchupUnservable。重绑同 session 保留 cursor，新 session 清理旧状态。宿主负责处理补洞请求与失败后的重连决策。

每 tick 产生一帧、补 C 帧，积压变化为 1-C；C=1 没有净追赶能力，C=2 对照收敛。此为源码速率推导，不是实测带宽。真实链路过载仍需暂停、快照或明确终止策略。

## 配置与身份

NewRoom 核算冗余深度、玩家数和输入字节的最坏包大小，但还必须遵守 wire.MaxFrameInputs。RR-07 说明字节界限不能代替条数界限。sessionOwners 用 -1 表示旁观者，配置又允许 -1 座位；RR-06 说明身份种类应显式表达，或严守保留值。

DesyncDetector 每玩家首个报告胜出，同值达到 quorum 后裁决；Room 默认座位多数，Trim 保存水位阻止旧集合复活。本轮只验证裁剪屏障，未完成反作弊审计；多数一致不等于模拟正确。

## 性能与实施顺序

环形冗余编码避免窗口滑动复制；Tick 仍整理/排序玩家与接收者，分配编码包并逐接收者发送。History 随比赛时长增长，频繁 Trim 长窗口会重复复制 Frame 描述；追帧预算按 session 配置，总工作量还受旁观者数量影响。以上是源码复杂度观察，没有吞吐/延迟测量。

优先修复输入身份与追帧收敛，再统一座位与协议校验；复用已有 Sequencer、History、Assembler、可靠发送接口。之后做真实传输的突发丢包、阻塞、过载与重连实验，并验证客户端重放一致性。[问题](../bug/REVIEW-2026-09-14-03.md) · [运行](REVIEW-2026-09-14-03.md)。
