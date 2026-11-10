# 开地形的分区场景真实多进程演练（T05 / T06 未覆盖项）证据（2026-11-10）

方案见 [§26](../../FEATURE-2026-11-08-editable-terrain.md#26-开地形的分区场景真实多进程演练t05t062026-11-10)，进度见 [TERRAIN-STATUS](../../TERRAIN-STATUS.md) 的 T05 / T06。本地分支 `feat/terrain-partition-drill`，基于 main `960e485c`，只在本地提交、未推送、未合入，不是已发布状态。

## 环境

Apple M5，macOS，Go 1.27.3，`GOWORK=off`；机器有后台负载。真实依赖全部是 `scripts/mirror-local.sh up` 起的私有实例（NATS JetStream 三节点、Mongo 三节点副本集、Redis；私有根目录与端口偏移），etcd 由演练脚本在私有端口自起。演练结束脚本删掉本次的库，私有依赖用完 `scripts/mirror-local.sh clean`。

## 怎么跑

```bash
export ROOST_MIRROR_LOCAL_HOME=<scratch>/ml ROOST_MIRROR_LOCAL_OFFSET=<偏移>
scripts/mirror-local.sh up
set -a; source <scratch>/ml/roost-dataengine-it/env.sh; set +a     # 只 source，不打印
ROOST_SEAMLESS_TERRAIN=1 ROOST_SEAMLESS_OUT=<scratch>/ev scripts/test-seamless-demo.sh   # 开地形的演练（约 2 分钟）
ROOST_SEAMLESS_OUT=<scratch>/ev-plain scripts/test-seamless-demo.sh                       # 原演练（不开地形）
# 变异：生成工程之后、编译之前在工程目录里执行一条命令
ROOST_SEAMLESS_TERRAIN=1 ROOST_SEAMLESS_PROJECT_HOOK="sed -i '' 's/cfg.Surface = partitionSurface(m, grid)/_ = partitionSurface/' internal/service/game/partition.go" ...
scripts/mirror-local.sh clean
```

脚本与客户端：`scripts/test-seamless-demo.sh`（`ROOST_SEAMLESS_TERRAIN=1` 换配置与步骤，`ROOST_SEAMLESS_PROJECT_HOOK` 供变异）、`scripts/testdata/seamless/terrain.go.txt`（与 `drill.go.txt` 编成同一个程序，`-terrain` 选步骤，`-write-flat-pack` 写地形包）。读各进程地形视图用生成 demo 新增的只读 GM `gm.terrain.probe`（每块：是否本进程写、是否装着、版本、副本订阅报的版本、数据摘要 FNV-1a 64；每点：世界高度）；客户端按同一摘要规则算自己收到的块，两边比较“是否同一份数据”。

## 配置与几何

| 项 | 值 | 为什么 |
| --- | --- | --- |
| 地形包 | 4 × 2 块平地（原始高度 1000 = 世界高度 0、没有水），演练程序写出 | demo `cmd/terrainpack` 的缺省程序化包（种子 1）上向东一步只有 45.5% 走得通（8128 条边 3700 条），角色会被天然坡随机挡住，判定不出“被邻格的坎挡住”（见下“待维护者确认”2） |
| 分区 | `cell_size` 12800（两块）、2 × 1 格，格 A = sid 1001、格 B = sid 1002，分界线 x = 12800；世界正好是地图 | 块不跨格；每格 2 × 2 块，Ghost 带只含邻格靠边的一列块，能分出“Ghost 带的块”和“只给客户端看的块” |
| `ghost_band` / `view_radius` / `hysteresis` | 600 = 400 + 200 | 满足 G ≥ R + H；越过分界线 200（x ≥ 13000）才交接，演练把邻格的坎放在这 200 里 |
| 其余 | `idle_period` 0、`max_speed` 600、进场点 (11800, 3000)、格 A 一只 y = 9000 往返的怪（600/s） | 怪一直 50ms 一拍，GM 能看清折回点 |
| 进程 | 1000、1004 只做 Base（不装块），1001 / 1002 是格；不起普通场景 1003 | 至少 2 个格进程 + 1 个只做 Base 的进程 |
| 玩家 | ta 连 A、tb 与 tb2 连 B（各自只能改本进程写的块）、tw 连 1004（跨格走的玩家，也是“别的进程上的客户端”） | |

各进程装着的块（第一步核对）：A 装列 0～2（写 0、1），B 装列 1～3（写 2、3），1000 / 1004 不装；全部是原始块、摘要相同。

## 步骤、通过条件与结果

两次完整运行（[terrain-run1.log](terrain-run1.log)、[terrain-run2.log](terrain-run2.log)，第一次的完整 [evidence.json](terrain-run1-evidence.json)）都 6 步通过。延迟：GM 轮询每 5ms 一次（含一次本机 HTTP），所以“视图对齐”的数字精度约 5ms；客户端的到达时刻是读到那一帧时记下的。

| 步骤 | 通过条件 | 第 1 次 | 第 2 次 |
| --- | --- | --- | --- |
| (a) `terrain_ghost_edit`：A 在边界块 (1,0)、远处块 (0,0) 各抬一次（r=2m、+1m） | B 的视图里 (1,0) 与 A 版本、摘要相同；B 的视图不装 (0,0)；B 上的 tb、1004 上的 tw、A 上的 ta 三个客户端都拿到两块且摘要与 A 相同；B 改 A 的块答 `terrain_not_owned` | 编辑往返 22.6ms；B 的 Ghost 带在答复后 3.3ms（一次轮询）已对齐；三个客户端从发请求算 22.7～27.3ms（答复之后 1ms 内） | 往返 26.2ms；3.4～4.2ms；26.0～26.8ms |
| (b1) `terrain_player_step`：B 在 x 13000～13600、y 3000 抬 5m 的坎；tw（Base 1004，SceneActor 在格 A）朝东 600/s 走 | 坎到 A 的 Ghost 带；tw 停在 x ∈ (12800, 12949]、y 3000，速度 0；之后 2s 内一直是 A 的 Real、位置不变、B 没有它、Base 记的拥有者是 1001；客户端看到的自己就在停下的位置；A 的 `SurfaceBlocks` 增加；坎挖平（摘要回到原始块）后再走，交给 B | 坎到 A 9.98ms；**停在 12949**（坎的第一个节点从 12950 起），发移动意图后 1945ms；挖平到 A 9.3ms；再走 114ms 交给 B，在 B 上 x 13008 | 16.1ms；**12949**，2004ms；13.4ms；109ms，x 13007 |
| (b2) `terrain_monster_step`：怪朝东过 x 11300～12300 时，B 在 (13300, 9000) 抬坎（块 (2,1)，A 的 Ghost 带），A 在 (10700, 9000) 立墙（块 (1,1)，自己的） | 连续 4 次折回：在坎前折回时越过了分界线且不超过 12949，在墙前折回时不早于 11050；期间一直是 A 的 Real、B 没有它；坎挖平后交给 B | 等怪 3.1s；折回点 东 12934 / 西 11059 / 东 12939 / 西 11057（GM 约 10ms 采样，真实折回点在采样之间）；挖平后 3365ms 交给 B | 2.9s；12933 / 11063 / 12937 / 11077；3336ms |
| (c) `terrain_cell_restart`：B 改边界块 (2,0)（答复到了就读 B 的视图：v3），tb2 在 B 的块 (3,1) 上连续小编辑，第 5 个答复后 kill -9 B；B 停机时 A 改 (1,1)；重启 B | 重启后 B 的 (2,0) 与 kill 前读到的版本、摘要相同（WAL 持久）；A 的 Ghost 带与 B 对齐；B 的 Ghost 带补上 A 停机期间的编辑；tb2 的 LastSeq ≥ 最后答复成功的序号，且 (3,1) 的版本 = LastSeq（块与账本同一笔事务）；用最后答复的序号重发答 Duplicate、块不变；B 上重新登录的 tb、tw、ta 都拿到三块且摘要与拥有者相同 | 停机 5.8s；B 重启后 (2,0) = v3 同摘要；A 对齐、B 补齐在 readyz 后第一次读就已成立（1.6 / 1.9ms，即对齐在就绪之前完成）；最后答复 seq 5，在 kill 之后 17ms 到达（答复与 kill 几乎同时），LastSeq 5、(3,1) v5，在途的 seq 6 没有生效；重发 seq 5 → Duplicate；tb 重新登录后 66～88ms 拿到三块；tw 的 SceneActor（在 B 上）重启后 95ms 回到 B | 6.4s；同上；3.5 / 3.6ms；seq 5、14ms、LastSeq 5、v5；Duplicate；52～73ms；74ms |
| `terrain_consistent` | 8 块在每个装着它的格进程里与拥有者版本、摘要相同；三个客户端持有的改过的块也一样 | 通过（版本：(0,0) 1、(1,0) 1、(1,1) 2、(2,0) 3、(2,1) 2（摘要回到原始块）、(3,1) 5） | 通过（同） |

副本计数（第 1 次收尾）：A 发出 9 条块状态（推送 8），共 729 字节（平地块压缩后很小，不代表真实地形的块大小）；1004 收 14 条、应用 14；三边 `BaseRevisionDrops`、`WrongOwner`、`DecodeErrors`、`SendDrops` 都是 0。

## 能变红：不装地面钩子

`startPartition` 里装钩子的那一行（`cfg.Surface = partitionSurface(m, grid)`）在生成工程里改掉、其余不变（[mutation-no-surface-hook.log](mutation-no-surface-hook.log)）：(a) 照常通过，(b1) 变红——

```
seamlessdrill: FAIL: (b) pW stops at the step: not reached within 15s (not a real on A (on B: true map[... role:real vx:600 vy:0 x:20673 y:3000]))
```

即 tw 走过邻格的坎、交给了 B、一直走到 x 20673。这就是 §24.4 说“只有真实进程演练能覆盖”的那几行。

## 原演练（不开地形）不退化

本分支上不开地形跑原演练（[no-terrain-run.log](no-terrain-run.log)）：14 步全部通过，最后的普通场景 sid 1003 机器人压测这次也通过（p95 13.54s，阈值 16s）。同一天开工前在未改动的检出上跑的一次（[no-terrain-baseline.log](no-terrain-baseline.log)）：14 步通过、压测 p95 19.48s 超阈值失败——与 T05 记录的“main 上同样失败”一致。这一步的阈值在同机两次之间一过一不过，与地形无关，本轮不处理，登记为 [KNOWN-LIMITS](../../KNOWN-LIMITS.md) 的“尚未确认”。

## 过程中发现的问题

没有发现框架或 demo 的缺陷。演练程序自己的两处错误（修正后才得到上面的结果，记在这里以免后来者重犯）：

1. 第一次运行 (a) 红：“tb holds chunk (1,0): not reached within 5s (held false v0 ...)”，而 tb 实际已持有 `(1,0):v1/h4096/m4096`。原因：演练程序没有登记实体种类，`entity.BuildEntityID`（按种类解析类别）返回错误，按自己拼的 ID 查块永远查不到。改为按唯一号（`terrain_chunk.KeyOf`）认块、玩家的 SceneActor 用客户端从同步里认出的完整 ID。
2. 业务码按 `Reason`（文本）比较，实际 `Reason` 是说明文字；改为按 `Code`（100054 / 100056）比较。

另：demo 缺省程序化地形包上大部分 1m 的步走不通（上表），开地形的分区场景若用缺省包，刷出的怪多半原地被挡。这是包内容的性质，不是判定的错误；是否给 `cmd/terrainpack` 加平缓 / 平地选项列在待确认里。

## 未覆盖

- 跨格寻路：分区场景没有寻路入口（§24.4）。
- “提交了但答复没到”的在途编辑：两次运行里在途的那一笔都没有生效，崩溃恰好落在 WAL 落盘与答复之间的情形没有稳定构造出来；保证仍由 strict 契约推出（§21 第 4 条）。
- 只 kill 了格 B；格 A、只做 Base 的进程在开地形下的崩溃，与换版（D13）的真实进程演练没有做。
- 消息丢失 / 延迟（toxiproxy 注入）、跨机、T09 规模（单格数千玩家、1w 地图）都没有做。
- 演练用平地包：真实起伏地形上的移动只在单元 / 端到端用例里覆盖。
- 演练不在 pretag 里；需要真实依赖，手动运行。

## 待维护者确认（实施方先按推荐做了）

| 项 | 现状 | 选项（推荐标★） |
| --- | --- | --- |
| 1. 读视图的手段 | 生成 demo 新增只读 GM `gm.terrain.probe`（每次至多 64 块、256 点，admin 令牌，不取锁、不等 Nest） | ★ 保留为 demo 的运维 / 诊断命令（演练与排障共用）；或只在演练里用（需另找读各进程视图的办法，例如测试专用构建标签） |
| 2. 演练的地形包 | 演练程序写 4 × 2 平地包 | ★ 演练用平地包（判定清楚、与 demo 内容无关）；或给 `cmd/terrainpack` 加 `-flat` / 平缓选项，演练与 demo 共用；或改缓 `terrain.Generated`（包字节变化，须换 BaseRevision） |
| 3. 演练的入口 | 原脚本加 `ROOST_SEAMLESS_TERRAIN=1` 模式，复用依赖准备与清理；不进 pretag | ★ 保持手动；或进 pretag（每次发版多约 2 分钟，且要求真实依赖） |
