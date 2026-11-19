# game-demo：World 写入与 Saga 首投等待（本机诊断）

同一提交 `a4e5975e`，darwin/arm64，Go 1.27.3。`scripts/perf/demo-robot.sh` 各起独立依赖和工程，各 100 个角色，各一次。`DEMO_ROBOT_COUNT=100`、`DEMO_ROBOT_MODE=burst` 或 `ramp20`（每秒放 20 人），另指定不冲突的 `DEMO_ROBOT_MIRROR_OFFSET`、`DEMO_ROBOT_PORT_SHIFT`、`DEMO_ROBOT_ETCD_PORT` 和新的 `DEMO_ROBOT_OUT`。一轮 ramp20 端口预检未过，既不是失败场景也不是性能样本。两次有效运行峰值在线均为 100，无调度丢弃。

| 指标 | 同秒入场 | 分五批入场 |
|---|---:|---:|
| 整套场景成功 / 开始 | 92/100（门槛失败） | 100/100 |
| 场景耗时 P50 / P95，秒 | 23.65 / 29.63 | 20.76 / 25.05 |
| AddExp 步骤，成功数 / 平均毫秒 | 100 / 1148 | 100 / 353 |

8 个失败都在第 20 步等待 `gift_status` 用尽重复查询次数。只重查状态，不重发赠礼；这些结果不能说明礼物未提交。同秒与分批走到后续步骤的角色数不同，不能只凭总耗时判断单一根因。

### 分段测量

`game-metrics-after.txt` 减 `before`；均值 = Δsum_nanos / Δcount，单位毫秒，括号为样本数。仅 game 进程，不含所有服务；不同阶段不是同一组请求，不能加成端到端耗时。首投年龄从 broker 原始追加时间算到消费者开始处理，含积压和等待 worker，不是 broker 或网络单项延迟。跨机测量须校时；缺失或未来时间戳不造样本。

| 段 | 同秒 | 分批 |
|---|---:|---:|
| outbox 入队至尝试发布 | 71.5 (665) | 66.3 (679) |
| outbox 单次发布 | 11.1 (665) | 10.6 (679) |
| Nest start 首投年龄 | 2090.1 (476) | 2207.0 (500) |
| start 协调器调用 | 191.6 (476) | 182.2 (500) |
| native step 首投年龄 | 48.4 (573) | 48.4 (600) |
| Mongo step 首投年龄 | 708.0 (501) | 214.5 (500) |
| Mongo inbox 执行 | 130.4 (501) | 139.8 (500) |

两档 start 到消费者开始处理均约 2 秒，不是把协调器调用误算成 2 秒。分批时 Mongo step 首投年龄下降，inbox 执行均值没有同步下降。**不是生产容量、TPS 或调并发的依据**；定位 8 个操作的最终去向需要关联操作 ID、Saga 状态和持久账本。下一次在相同硬件、相同业务量下单独改变 start 消费者配置，记录等待、成功率、重投与尾延迟后再判断。

### World 写语义（本次只盘点，不调级别）

- `RecordEnter`：登录投递、不同步等待，World 在独立 strict 事务中累加。`RecordMatch` 在匹配结果之后独立 strict 记数。都是共享 World 的热点；投递成功不等于已提交，也不能用累计值对每个事件去重。
- `AddExp`：Player 经验和 World 经验统计、升级效果同一 strict WAL 记录。`ClaimDungeon`：奖励与领取账本、World 变动同笔，重放按领取键处理。不能为吞吐拆事务，`AddExp` 请求超时后不可直接重发。
- `SettleActivity` 是写结算标记的 strict 事务；`ActivitySettled` 是读标记，虽声明 strict，并不因此每次都产生 WAL 记录。`WorldStats` 也只读。
- 本轮没有调整持久策略、WAL 或 Nest/Saga 全局默认值。要放宽普通计数，先定义丢失窗口、是否有权威事件可重放和如何防止重算；不能把奖励写一并放宽。
