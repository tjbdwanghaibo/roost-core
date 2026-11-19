# 四项本机恢复边界：实验与证据

基线 `66d6eab2`，在独立工作树验证。前三项使用本机 macOS 和独占临时依赖；第四项直接复算 11-18 的原始 200 人档，不是用本次机器状态推算上次结果。断尾模型是磁盘上保留有效的 WAL 整帧前缀，不声称进行了物理断电、介质损坏或 Mongo 副本集全体掉电。

## 1. Mongo 已经投影、WAL 尾部消失

[集成回归](../../../../framework/dataengine/engine/power_tail_integration_test.go) 使用文件 WAL 与真实单节点 Mongo 副本集：一次批量投影同一文档 `1→2→3` 的两条 async 记录，观察到两枚事务标记，在第一笔 Ack 前拦截，随后模拟三种有效文件前缀，再重开 WAL 与投影器。保留零条和两条时原实现都能回放；只剩第一条时原实现报 `expected=1 next=2 stored=3`。原因是恢复时 `projectSegment` 把一条记录交给 `Project` 的单文档快路，没看到批量写时的标记，文档 `_last_tx` 已是第二笔。

修后同三种前缀均保留 `v=3`，分别回放 0 / 1 / 2 条。陌生事务的陈旧版本仍是 `ErrProjectionConflict`；ID 相同但摘要不同时是 `ErrTransactionIdentity`。没有把所有超前版本认作成功，没有为正常更新增加一次 Mongo 读取。WAL 自己在成功 Ack 之前会先刷段；这次不是 checkpoint 文件越过了稳定 WAL，也不代表可直接从 WAL 恢复所有已投影或已对外确认的 async 修改。

复跑：在独占 Mongo 副本集配置 `ROOST_DATAENGINE_IT_MONGO_URI` 后运行 `go test -tags integration -count=3 -v -run 'TestRealMongoShorterWALAfterProjectedBatch|TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint' ./framework/dataengine/engine`。

## 2. MOBA：旧客户端领先恢复后的房主

[真实依赖回归](../../../../scripts/testdata/moba/active/power_tail_test.go) 经生成 handler、Room.Tick 和捕获的实际广播包推进客户端到第 2 帧。显式同步到第 1 帧；第 2 帧之后既没有新段 fsync，也未投影。终止子进程后**另行**裁掉文件的第 2 帧；不裁文件的原 SIGKILL 测试仍恢复到第 20 帧再继续到第 40 帧。

断尾后由真实运行时启动并 `resume`，epoch 从 1 变为 2。旧第 2 帧有移动输入；新权威从帧 1 开始，以不同输入再生成第 2 帧。旧组帧器不交付这个重复帧号，客户端的 Arena 摘要不同；另一客户端用新权威头部快照恢复 Arena，用新组帧器 `ResumeAfterSnapshot` 到对应帧号，收到同一新包后摘要与权威一致。**隔离旧会话、重置组帧器、替换业务状态缺一不可**；测试的是重建路径，不是任意业务客户端都已实现该握手。若业务不接受已显示帧回滚，应在持久确认后再对外发布，而非依赖 epoch。本次不改变现有 async 选型。

入口：`ROOST_MOBA_REAL=1` 配合私有 Mongo 副本集运行 `scripts/test-moba-generated.sh`。

## 3. 换服：检查交出返回时的注入事实

原演练仅凭 `reached.json` 的到达顺序报 BYPASS。修后同步与后台 Ack 探针记录实际被扣住的 `LeaveScene` 事务 ID；等待方记录快照中的未确认 ID；交出停点记录其**当时**所见的 held ID，而非由测试驱动事后猜测。正常路径要求持有和等待同一笔事务，释放后检查绑定、钱包、背包与源服拒绝。Mongo 快照改读带工程前缀的实际 game 分库。

- 正常：独立跑第七场景通过；按原顺序跑完整七场景 7/7 通过。第七场景持有 / 等待 ID 一致，目标服钱包 93、背包 11，源服拒绝。
- 对照：去掉生产等待后，观测到一次返回时 held 仍为空、而轮询时 held 文件已经存在。这是 `CHECKPOINT_PREMISE_MISSING`，不能拿来宣称已经证明跨过了一个命中的闸门。为了让反向验收确定，变异版先让注入器获得调度、扣住目标 Ack，再直接跳过确认（不等 Ack 释放）；此时报 `CHECKPOINT_BYPASS` 且返回点/held ID 完全一致。原生产交出逻辑未改。
- 11-18 那次告警缺少当时的目标事务和返回点快照，仍不能回溯定性。

入口：私有 Mongo / NATS / Redis 环境下运行 `DEMO_REHOME_OUT=<新目录> bash scripts/test-demo-rehome.sh`；对照用另一个新目录、`DEMO_REHOME_STAGES=checkpoint DEMO_REHOME_MUTATION=skip-checkpoint`，预期非零且原因必须是 BYPASS。

## 4. 原 200 人同秒档：失败不是未到账凭证

复算：`python3 docs/maintenance/evidence/local-recovery-four/addexp.py`。输入是[原轮次目录](../category3-batch2-20261118/item1-200burst-baseline/)里的 `robot.json`、`game-dispatch.csv.gz` 和 `game-metrics-after.txt`。200 个玩家中 114 人仅有一条派发，86 人各两条。首步 `add_exp(150)` 成功 86；之后 `scene_alive` 为这些玩家各成功调用 `add_exp(1)` 一次，两步无重试。派发合计 286 笔，`result=ok` 177、`error` 109；持实体锁执行计数也恰为 177。基线 `4652985a` 的 AddExp 是 Player + World 的 strict 事务，服务端结果在 `finishDispatchResources` 之后计数，不是收到请求时就记成功。故客户端首步失败的 114 笔中，**177 − 86 − 86 = 5 笔仍由服务端成功提交**，余下 109 笔未进入写入。不能按客户端超时无条件再给一次经验。

这是该次 A 基线的**总量**，不是逐玩家 WAL / Mongo 收据。不能据此指出哪五个玩家，也不能把这五笔只归给 105 个 timeout 而排除另 9 个取消/期限错误。另一次 [perf-compressed 的 200 人档](../perf-compressed-20261118/robot-200-burst/)没有归档同口径的按 handler 服务端结果，不能移用这个数字。未改 AddExp 接口、幂等策略、持久级别或 game-demo 模板。
