# 联盟重试与数据成本探针（2026-11-14）

对应 v1.47.0 交接 B-2。正式联盟仍限制最多 500 人，本批没有扩大业务准入。新增探针已经用当前 CLI 生成工程跑过小规模功能验证；并行编译期间的数字只证明入口有效，不作为容量结论。effect worker 的正式交替测量归 M-11。

## 入口

| 用例 | 路径与边界 |
| --- | --- |
| `TestAllianceE2ERPCConcurrency` | `ALLIANCE_E2E_RPC_CONCURRENCY=16`（1～256），写额度固定 512。Info / Join 分散到 16 个联盟与单热点对照；单热点新增入会总数至多 256，保持 500 人准入上限。Found 初始化固定顺序和 16 并发。报告耗时至少 3 秒样本的开始时间、错误与数量；没有自动重试。 |
| `TestAllianceE2EJoinRetryableSources` | 真实联盟进程、NATS RPC、Mongo 与 Redis。可配置 `dataengine.effect_consumer.workers`，对明确的 retryable 沿用玩家号与请求号退避重试，最多 8 次；unknown 不自动重试。RPC 答复次数与逻辑请求终态分开计数。没有 unknown 时，最终人数必须等于创始人加成功入会数。 |
| `TestAllianceE2ELargeAllianceFootprint` | 正式 RPC 建盟和入会，1～500 人。读取 Mongo Remote 外层文档及其 `data` 中 DAO BSON，核对名单；再经正式 SnapshotClient、Redis L2、Mongo 回源、生成 Mirror 解码器冷读。 |
| `TestAllianceE2ESyntheticRosterFootprint` | 默认 1000 / 5000 / 10000 人。用持久文档形状经 `RestorePersisted` 加载生成的 AllianceDao，再用 DAO 编码及生成 Mirror 解码器读取 BSON；每名成员保留一条入会回执。不开服务、不经过存储和 L1、不表示业务支持超额联盟。更长回执历史、捐赠和战争记录需要另给数据形状。 |

堆成本先预热共享连接及 BSON 类型元数据，在 GC 后取两次 `HeapAlloc` 差值。正式入口保留各客户端的 L1 与独立 DTO；合成入口只保留 DTO。结果包含进程背景分配噪声，可能为负，不是 RSS，也不包含独立联盟服务的整进程占用。冷读是空 L1 的按需读取，Redis L2 已可能预热；不等于冷 Mongo 磁盘、不含推送订阅。

## 复跑

用自己的 `mirror-local.sh` 环境，确认端口未占用，保留环境数据；不要对共享环境做故障注入或清库。依赖启动、测试与停止放在同一个 shell 生命周期中。生成脚本会清理自己生成的临时工程与本次命名前缀的数据。

```sh
export GOWORK=off GOFLAGS=-p=2
export ROOST_MIRROR_LOCAL_HOME=/private/tmp/v147-alliance-probe-22000
export ROOST_MIRROR_LOCAL_OFFSET=22000
bash scripts/mirror-local.sh up
trap 'bash scripts/mirror-local.sh down' EXIT
source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
export ALLIANCE_E2E_ETCD_PORT=43379 ALLIANCE_E2E_PORT_BASE=43500
export ALLIANCE_E2E_PERF=1

# Functional smoke: force admission pressure, stable-ID bounded retry.
ALLIANCE_E2E_EFFECT_WORKERS=2 ALLIANCE_E2E_RETRY=1 \
ALLIANCE_E2E_RETRY_WRITES=2 ALLIANCE_E2E_RETRY_CONCURRENCY=8 \
ALLIANCE_E2E_RETRY_SECONDS=1 ALLIANCE_E2E_MEMBERS=8 ALLIANCE_E2E_MIRROR_COPIES=4 \
ALLIANCE_E2E_RUN='^TestAllianceE2E(JoinRetryableSources|LargeAllianceFootprint)$' \
  bash scripts/test-alliance-e2e.sh

ALLIANCE_E2E_SHAPE_COUNTS=8 ALLIANCE_E2E_MIRROR_COPIES=4 \
ALLIANCE_E2E_RUN='^TestAllianceE2ESyntheticRosterFootprint$' \
  bash scripts/test-alliance-e2e.sh

# Idle-machine measurement; keep these separate from the smoke results.
ALLIANCE_E2E_MEMBERS=500 ALLIANCE_E2E_MIRROR_COPIES=16 \
ALLIANCE_E2E_RUN='^TestAllianceE2ELargeAllianceFootprint$' \
  bash scripts/test-alliance-e2e.sh
ALLIANCE_E2E_SHAPE_COUNTS=1000,5000,10000 ALLIANCE_E2E_MIRROR_COPIES=16 \
ALLIANCE_E2E_RUN='^TestAllianceE2ESyntheticRosterFootprint$' \
  bash scripts/test-alliance-e2e.sh
```

M-11 保持缺省 256 并发、写额度 512，`ALLIANCE_E2E_RETRY=1`，effect workers 在 16 / 1 间交替三轮；每次脚本内部有三个 15 秒样本。开始前记录 load、swap，并确认没有其他编译、race 或索引任务。

## 证据与需回写的边界

- 初轮暴露生成配置省略 `effect_consumer`：夹具现在按精确层级补默认缺失节，不会误改 `projection.workers`。变异把目标改成 projection 后真实脚本失败于 `effect_consumer.workers=0, want 2`；恢复后通过。
- 正式 Remote 文档是元数据加 `data` 的封装，不能按外层 `leader_id` 查 DAO。探针现在分别报告外层字节和 DAO 字节，并解码验证成员。
- 8 人 / 4 份正式 Mirror 冒烟：外层 BSON 1967 B、DAO 1858 B；一次预热后的堆差 88352 B（22088 B/份），冷读 P50 100 μs / P99 160 μs / max 220 μs。样本少且机器并行编译，不外推吞吐和延迟。
- 低写额度重试三轮通过，既出现 retryable 又完成入会；最终名单核对通过。去掉正确配置目标的变异失败，恢复后两个正式探针通过（总计 8.054 s，不含生成与编译）。
- 合成探针 8 人 / 4 份视图实际脚本通过，DAO BSON 1864 B。初轮直接使用 setter 被 Nest 的事务约束拒绝；改走正式加载入口，没有放松守卫。这个小样本的堆差被进程噪声主导，不作为每成员大小。
- 原始日志：`/tmp/roost-v147-alliance-mutation.log`、`/tmp/roost-v147-alliance-final-green.log`、`/tmp/roost-v147-alliance-shape-green.log`；初轮失败保留在 `*-shape-smoke.log`、`*-missing-config.log` 与 `*-wrapper-red.log`。

最终文档批可把“尚无正式探针”改为：**重试 / effect worker 对照与正式上限内的 BSON、Mirror 成本入口已实跑；超额人数仅有生成 DAO / DTO 合成探针。正式容量与 M-11 结果仍需空闲测量，500 人上限没有改变。**

## RPC 并发入口补齐

原正式用例忽略 `ALLIANCE_E2E_RPC_CONCURRENCY`，始终执行 16 并发；128/256 的开关此前只存在 checkpoint 实验。修前设为 4 的真实脚本仍打印 `info-spread16 conc=16 n=1600`，要求并发为 4 的参数核对退出 1。现在正式探针读取 1～256，分散目标始终是 16 个联盟，单热点请求规模不会突破 500 人上限；Found 初始化仍用原来的顺序/16 并发，不把它报成可变档。

同一私有 offset 22000 环境，设置 `ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_RPC_CONCURRENCY=4 ALLIANCE_E2E_RUN='^TestAllianceE2ERPCConcurrency$'` 实际运行脚本后，Info 分散档为 4 并发/400 次，Join 热点为 4 并发/32 次；各档错误与 ≥3 秒样本均 0，1 PASS、零 SKIP。只是功能验证，不能用该次耗时推断正式容量。修前源码也等同于去掉新开关的反向对照，确实不能通过参数核对。

原始日志 `/tmp/roost-v147-rpc-options-before.log`、`/tmp/roost-v147-rpc-options-after.log`，过程日志分别在 `/private/tmp/v147-rpc-options-before`、`/private/tmp/v147-rpc-options-after`；依赖已停止。本批只移出测量参数、人数边界和慢样本统计，没有合入后台 checkpoint 配置或实现。后续 C-Q1/Q2 可使用正式开关在目标宿主测 128/256，M-19 仍在实验分支保持同夹具关/开交替。
