# 投影器按实体分片并行投影：本机与云 Mongo 对照（2026-10-27）

对应设计：[FEATURE-2026-10-27-projector-entity-parallel](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/FEATURE-2026-10-27-projector-entity-parallel.md)。分支 `feat/projector-entity-parallel`
（基线 `8c691215`）。样本在 [benchmarks/projector-entity-parallel-20261027](benchmarks/projector-entity-parallel-20261027/)；完整原始输出（含测试日志）在本机
`artifacts/perf/projector-entity-parallel-20261027/`（不随仓）。全部是单机、短时、每档 1～2 个样本，只给量级，不是容量保证。

## 0. 结论

1. **普通记录（单 mutation，实体分散）**：本机隔离副本集 6815 → 24717 条/s（w8，约 3.6 倍）；云 Mongo（本次 ping P50 64ms）431 → 1653 条/s（w8，约 3.8 倍）。
   w16 与 w8 持平，没有更多收益。
2. **带回执、不带 effect 的记录**（每条一个 Mongo 事务）：本机 65 → 493（w8）/ 793（w16）条/s，云上 2.4 → 19.5（w8）/ 37（w16）条/s，基本随通道数线性增长——
   这类记录的上限是单个事务的往返与提交等待，不是服务端。
3. **带 effect 的记录**（saga 原生步骤的形状）：**不变**（本机约 60 条/s、云上 1.8 条/s，任何档位都一样），因为缺省设计不放宽 effect 暂存顺序，带 effect 的记录之间仍串行。
   现行 saga 原生步骤正向 scan（本机）分支与 main 在噪声内一致；实验 B（去掉 effect 顺序类，未提交）c16 约 +30%、c64 约 +60%。是否采用 B 需要维护者决定。（2026-11-05 更正：维护者已把缺省改为 B“按实体有序”，原行为保留为 `dataengine.projection.effect_order: wal`，见 [impl/03](../framework/impl/03-dataengine.md#effect-顺序范围)。）
4. **原型 cloud-part（参与方投影是普通记录）在分支代码上**：同一时段同一网络（RTT 约 64ms），参与方投影速率从 main 的约 384～400 条/s 提到约 950～1365 条/s。
   c16 停止加压时积压 1844 条（main 1.49 万条），1.2s 追平（main 44s）；c64 / c128 积压仍随时间增长（需要约 3140 / 5450 条/s，投影约 1300 条/s），但都在 120s 内追平
   （c64 47.6s、c128 102s；main c64 120s 后仍剩 4.5 万条）。
5. 热点（一半记录落在同一实体）：本机 6682 → 15470 条/s（w8）。热点实体的记录全部在一条通道里，其余记录分到别的通道，所以仍有约 2.3 倍。
6. 正确性核对：每档排空后逐实体核对 `_version`、effect 条数与 WAL 已清空，全部通过；原型各档余额 / 复读 / Mongo 账户逐账户 0 差异；saga scan 各档 `business_verified=true`、残留 0。

## 1. 口径

- **投影器吞吐夹具**：`framework/dataengine/engine` 的 `TestRealMongoProjectorParallelThroughput`（`-tags integration`），入口 `scripts/perf/projector-parallel.sh`。
  先把 N 条记录以 async 写进一个新 WAL 并 `Sync`，再用 `ManualReplay` 的 Projector（正式 `MongoStore`，缺省选项，只改 `ProjectionWorkers`）`Flush` 排空，
  条/s = N ÷ 排空耗时，即“积压追平速率”。每档一个一次性库（`roost_sagaperf_projpar_<摘要>_…`），排空后删库；结束用 `mongo-ceiling -leftover` 核对残留。
- **形状**：`plain` 单 mutation（首写 Put、之后 Patch），实体在 4096 个里均匀随机；`hot` 同上但一半记录落在实体 1；`receipt` 每条另带一条回执（走 Mongo 事务 + 事务标记）；
  `special` 回执 + effect（原生步骤去掉 lease fence 后的形状）。
- **main 对照**：同机用基线 `8c691215` 的工作树跑同一夹具的 w1（去掉 `ProjectionWorkers` 字段）；分支 w1 走的就是原串行路径，两者应一致（§2 表里的 main 列）。
- **saga scan**：`scripts/perf/saga-scan.sh`，`ROOST_SCAN_TARGET=local`，原生步骤正向，c16 / c64，每档 60s，`pairs=256`、`HOPS=0`；main / 分支 / 实验 B 交替各两轮。
- **原型**：分支 `proto/saga-jetstream-log` 的 `scripts/perf/saga-jslog-proto`（只复制到临时工作树，不往原型分支提交）分别放到 main（`8c691215`）与本分支代码上，
  `run-perf.sh cloud-part`（P=4、4 条流、每档 30s、追平上限 120s，NATS 是本机私有三节点）；参与方投影写云库。
- **代码版本**：测量用的是提交 `67460265`（之后改写历史为 `2adbbdf8`，代码差异只有一个局部变量改名）；实验 B 在其上去掉 `effectOrderKey`，未提交。
- **机器**：macOS arm64，GOMAXPROCS=4，`go1.27.3`；另有一个原型 agent 同时在本机做短测，每组开跑前门控 1 分钟 loadavg < 8，实际 loadavg 记在各 `runs.log`
  （投影器组约 3～6，saga scan 组 4～13）。

## 2. 本机隔离副本集（三成员同盘；ping P50 0.06ms，单文档 update c1 P50 9.9ms、c16 1052 次/s；单文档事务 c1 P50 10.4ms）

条/s，两轮（2026-10-27 21:37～22:10）：

| 形状（记录数） | main w1 | w1 | w2 | w4 | w8 | w16 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| plain（20000） | 6852 / 6655 | 7052 / 6579 | 10319 / 10434 | 18813 / 17151 | 25937 / 23498 | 24604 / 24688 |
| hot（20000） | 6925 / 6684 | 6646 / 6718 | 10982 / 9990 | 14032 / 13943 | 15044 / 15896 | 15746 / 15475 |
| receipt（3000） | 61 | 65 / 65 | 136 / 143 | 285 / 271 | 502 / 485 | 790 / 797 |
| special（3000） | 59 | 62 / 65 | 62 / 59 | 60 / 58 | 63 / 64 | 64 / 64 |

本机副本集的单文档写受提交 / 刷盘等待限制（c1 约 10ms），并发提交能合并刷盘，所以带回执的记录几乎线性；普通记录本来就合批（每批 256 条一个事务），
w8 之后被服务端吞吐挡住。

冒烟里的一个反例（没有进入正式实现）：最初“一轮仍只读 256 条、再分给通道”时，2000 条 / 512 实体 w8 只有 3857 条/s，低于串行的 5418——一个批被拆成 8 个小事务，
本机是提交受限而不是 RTT 受限。改成一轮读 `batch_records × workers` 条（每条通道仍是满批）后才有上表的结果。

## 3. 现行 saga 原生步骤正向 scan（本机）

成功吞吐 TPS（P50 / P99 ms），两轮交替（22:10～22:26）：

| 版本 | c16 | c64 |
| --- | --- | --- |
| main 第 1 轮 | 7.08（2239 / 3078） | 7.31（8517 / 15173） |
| 分支 第 1 轮 | 8.35（1891 / 2714） | 8.12（7505 / 14911） |
| 实验 B 第 1 轮 | 9.71（1600 / 2575） | 12.63（4987 / 8665） |
| main 第 2 轮 | 7.98（1946 / 2785） | 9.12（6806 / 9500） |
| 分支 第 2 轮 | 6.64（2368 / 3370） | 6.74（8708 / 15658） |
| 实验 B 第 2 轮 | 10.21（1514 / 2451） | 13.46（4755 / 10375） |

- 分支与 main 的差在两轮之间方向相反（±15%），判为没有差别：原生步骤的两条记录都带 completion effect 或与带 effect 的记录共享实体，按缺省设计整窗串行。
- 实验 B（effect 只按实体等键有序）c16 +30%、c64 +60% 左右，远小于投影器微基准的倍数：投影器不再是唯一瓶颈，余下的是步骤租约、同实体屏障与 checkpoint 之外的环节，
  本轮没有拆分。这组只用于给维护者决定提供量级。
- 各档 `business_verified=true`、终态全部完成、残留库 / 流 0。

## 4. 云 Mongo

**网络**（本轮 `mongo-ceiling`，22:27 / 22:31 / 22:45 三次）：ping P50 64.0～64.3ms（P99 67～113ms）；单文档 update c1 P50 67.5ms、c16 212 次/s；单文档事务 c1 P50 132ms。
**本轮属于“高 RTT”档**（与 10-26 的约 60ms 相当，不是 10-27 下午原型第三轮的约 8ms），与原型第三轮的云结果不是同一网络条件。

### 4.1 投影器吞吐（条/s，22:27～23:08）

| 形状（记录数） | w1 | w2 | w4 | w8 | w16 |
| --- | ---: | ---: | ---: | ---: | ---: |
| plain（10000） | 431 / 428 | 816 / 804 | 1272 / 1300 | 1616 / 1691 | 1652 / 1607 |
| receipt（1000） | 2.45 / 2.45 | 5.05 / 5.03 | 9.87 / 10.0 | 20.0 / 19.0 | 38.1 / 36.0 |
| special（500） | 1.85 | — | — | 1.84 | — |

- 带回执的单条事务在 64ms RTT 下约 410ms（会话、查标记、查 / 写回执、写标记、提交，约 6 次往返），w16 仍线性。
- 普通记录 w8 之后不再增长：每条通道一个 256 条的事务约 1.2s，多于往返次数能解释的部分，余下是云库服务端的写入时间；本轮没有拆分服务端耗时。

### 4.2 原型 cloud-part：main 与分支同时段对照（23:08～23:17）

| 版本 | 并发 | TPS | 端到端 P50 / P99 ms | 停止加压时积压（条） | 积压峰值（条，÷ 在途） | 投影速率 负载期 / 追平期（条/s） | 追平 | 余额 / 复读 / 账户 |
| --- | ---: | ---: | --- | ---: | --- | --- | --- | --- |
| main | 16 | 419.7 | 37.4 / 62.8 | 14859 | 14847（464×） | 384 / 336 | 44.3s | ok / ok / 0 差异 |
| main | 64 | 1540.4 | 40.3 / 72.5 | 82436 | 82340（643×） | 400 / 309 | 未追平（120s 后剩 45316） | ok / ok / 未核对 |
| 分支 | 16 | 492.2 | 31.5 / 56.6 | 1844 | 1947（60.8×） | 948 / 1579 | 1.2s | ok / ok / 0 差异 |
| 分支 | 64 | 1568.3 | 39.4 / 76.1 | 55441 | 55391（433×） | 1307 / 1154 | 47.6s | ok / ok / 0 差异 |
| 分支 | 128 | 2723.4 | 45.6 / 81.9 | 122692 | 122573（479×） | 1364 / 1198 | 102.4s | ok / ok / 0 差异 |

- 两步正向每笔 2 条参与方记录，需要的投影速率 ≈ 2 × TPS：c16 约 984 条/s，分支负载期 948 条/s，差约 4%，30s 里积压只到约 1.9k 条（main 同档 1.5 万条）；
  是否在更长时间里持平没有测（只跑了 30s），**不能据此说 c16 已经不再增长**。c64 / c128 需要约 3140 / 5450 条/s，投影约 1300 条/s，积压仍持续增长。
- 主要限制见 §4.1：普通记录在本轮云库上 w8 约 1650 条/s 封顶；原型的负载期速率（约 1300）低于微基准，因为同一进程里还在跑关键路径。

## 5. 限制与未验证项

- 每档 1～2 个样本，短时；本机有另一个 agent 的负载，saga scan 两轮之间 ±15% 的波动比分支与 main 的差还大。
- 云上只测了 64ms RTT 一档；8ms RTT 下的倍数没有测（原型第三轮的 500～590 条/s 是 main 在 8ms 下的数）。
- 原型只测了正向；补偿方向、c1、长时（≥ 10 分钟）积压曲线没有测。
- 投影器夹具是“预先写好的积压一次排空”，不是边写边投影；`special` 形状不含 lease fence 的协调文档读写。
- 实验 B 只做了本机 saga scan 与规划器 / 用例层面的变异核对，没有做 effect 乱序对下游的影响评估。

## 6. 复跑

```sh
# 本机（先 source ~/.roost-it/roost-dataengine-it/env.sh；脚本自己持 remote-acceptance.lock）
ROOST_PROJPAR_SHAPES="plain hot" ROOST_PROJPAR_RECORDS=20000 scripts/perf/projector-parallel.sh
ROOST_PROJPAR_SHAPES="receipt special" ROOST_PROJPAR_RECORDS=3000 scripts/perf/projector-parallel.sh
# 云库（连接串只从 ~/.roost-it/cloud-mongo.env 读，输出脱敏）
ROOST_PROJPAR_TARGET=cloud ROOST_PROJPAR_SHAPES=plain ROOST_PROJPAR_RECORDS=10000 scripts/perf/projector-parallel.sh
# saga 原生步骤 scan
ROOST_SCAN_TARGET=local ROOST_SCAN_SCENARIOS=native-forward ROOST_SCAN_LEVELS="16 64" ROOST_SCAN_HOPS=0 scripts/perf/saga-scan.sh
```

（历史记录：原型目录 `scripts/perf/saga-jslog-proto/` 已在 saga 第 6 批删除（正式实现与压测入口 `scripts/perf/saga-js-scan.sh` 取代它）；下文的命令与路径是历史记录，取回用 `git checkout c565c81b -- scripts/perf/saga-jslog-proto`。）原型对照用分支 `proto/saga-jetstream-log` 的 `scripts/perf/saga-jslog-proto/run-perf.sh cloud-part`，把该目录复制到要测的代码版本的临时工作树里运行；
本轮另在 `tee` 前加了脱敏过滤（临时修改，未提交到任何分支）。
