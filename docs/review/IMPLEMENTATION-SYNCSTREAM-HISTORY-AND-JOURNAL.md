# SyncStream：History、ACK 和 Journal 恢复

09-15 第八轮（Core 6cef240）补充：[29 场景证据](REVIEW-2026-09-15-08.md)。持久化 Import/Restore 没有替换 journal，重启旧内容回归或跨 epoch 不能恢复（RR-20260915-08）；Recover 锁外捕获期间若同流被追加或全局 epoch 改变，旧捕获仍以更新身份提交（RR-20260915-09）。这些是独立新触发，旧 RR-06/07 按用户声明跳过复核。

替换方案应先完成候选快照校验，再在互斥边界用现有 journal checkpoint 持久化，最后发布内存；区分构造恢复和在线替换。Recover 应在调用 provider 前后校验不复用的 epoch/流修改代数，失效时拒绝或有界重捕获，避免持 History 锁运行任意业务回调。

三组同一 History 下 Append/Checkpoint 并发重启对照通过，十种非法导入原状态不变，四种合法替换及 store 错误对照通过。没有验证 journal 同步错误、进程崩溃、多实例所有权；成功路径和互斥不能消除落盘结果不确定性。

Core 4622463；[第七轮证据](REVIEW-2026-09-15-07.md)。描述已读实现和建议，不表示修复已实施。

History 将 Observer/Stream 作为复合键。Append 给包设置全局 epoch 和流内递增 sequence；delta 的 BaseSequence 指向旧 latest，Full 的 base 为零。有效 schema 切换需要 Full。载荷克隆后保留，超 MaxPacketsPerStream 时丢最早包。

AcknowledgeEpoch 校验 epoch 和 sequence 上界，只推进 acked；可选 PruneAcknowledged 裁掉已确认包。Resync 比较请求身份、schema、序号和保留链，连续时返回克隆包，否则要求全量。Recover 在锁外调用业务 provider，再强制 Full 并 Append，业务快照与其他写入的交错需要宿主和框架共同约束，尚未专项验证。

删除流、删除 observer 和 SweepIdle 直接删除 streamState，不修改全局 epoch。RR-20260915-06 表明重建同键会再次从 sequence 1 开始，旧 ACK/Resync 无法分辨新的内容。保留数据和保留身份高水位是两件事；可以设计墓碑/逐流 generation，或明确定义宿主新身份。RotateEpoch 是全局清空工具，不能不加区分地用于每个局部回收。

接入 HistoryJournal 时，Append、ACK、删除、Sweep 和 Rotate 均先 Record，再提交内存。本轮拒绝注入验证六种操作原状态保持。Checkpoint 持 History 读锁导出，交给 journal 发布；Import 则验证完整快照后替换 map。Import 与既有 journal 的组合语义留待下一轮，不由内存原子性推出持久化一致。

FileHistoryJournal 的 Record 将 JSONL 拼进 pending，由 leader 写入并 fsync，然后唤醒等待者。checkpoint 新建下一 WAL 并同步，再发布带 generation 的快照；Load 只认最新 generation，损坏不自动回退。旧 generation 用于显式恢复。单个 History 在调用 Record 时持有自身互斥锁，底层 group commit 的收益须结合调用拓扑测量。

replayWAL 忽略未换行尾部，而 O_APPEND 续写仍接在原尾部后，产生 RR-20260915-07。恢复不仅要算出内存快照，也要建立安全的下一次写入位置。建议恢复到最后完整记录偏移并同步截断，或用现有 checkpoint 轮转；完整坏行仍应拒绝，不能静默吞损坏。

真实文件对照支持“恢复后先 checkpoint 可避免拼接”，但不是完整生产修复证明。未覆盖进程崩溃注入、Linux 断电、所有 Record/Checkpoint 并发、同步失败后的不确定提交结果或多实例共用目录，不能将本轮结果表述为 journal 完整通过。
