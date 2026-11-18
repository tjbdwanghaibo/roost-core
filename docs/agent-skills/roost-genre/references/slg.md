# SLG / 大地图策略

手册：`docs/framework/SLG-INTEGRATION.md`（下称 SLG §N）。命令对应当前版本的 `scripts/test-slg-generated.sh`；目标 roost-core 版本更早时读那个 tag 里的本文件。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 城建、研究、行军队列（期限驱动）；抢地 / 驻守（多实体事务）；联盟、国战（Remote 托管权威）；联盟活动与原服奖励 | 行军逐拍移动（按“起点 + 速度 + 出发时刻”算到达，不用心跳逐拍推进，guide/10-time.md §4B）；玩家随联盟跨 sid 迁移（SLG §7.1）；无缝大地图多进程分区（SKILL.md §6） |

## 2. 命令

接在 SKILL.md §3.2 公共前置命令之后（已 `cd "$DIR"`）。模块：`wallet`（玩家金币，都走 `Wallet().Add`，判重靠目标等级 / 队列号）；玩家队列与期限、地块、联盟权威都手写。

### 2.1 精简形态：建造、行军、抢地

```bash
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" wallet   # = roost module add + 核对已登记
"$ROOST" add timer --entity Player
"$ROOST" add entity Tile
"$ROOST" add dao Tile --entity Tile
dao_add Player db/def/player.go "BuildingLevel int64 building_level" \
  "ResearchLevel int64 research_level" "PendingResearch int64 pending_research" \
  "PendingBuild int64 pending_build" "Marches int64 marches" "ArrivedX int64 arrived_x"
dao_add Tile db/def/tile.go "OwnerID int64 owner_id"
world game/entities/tile/entity.go
F="$CORE/scripts/testdata/slg"
cp "$F/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$F/tasks.go.txt"          game/handler/slg_tasks.go
cp "$F/tasks_test.go.txt"     game/handler/slg_tasks_test.go
fix_mod slg-practice game/entities/player/timer_handlers.go game/handler/slg_tasks*.go
gofmt -w db/def/*.go game/entities/player/timer_handlers.go game/handler/slg_tasks*.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" slg
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestSLG' ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" wallet
```

### 2.2 脚本形态：加联盟活动（Remote 托管联盟权威）

用一个**新工程**从头执行（替换 2.1 整段）：

```bash
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" wallet
"$ROOST" add timer --entity Player
"$ROOST" add entity Tile
"$ROOST" add dao Tile --entity Tile
"$ROOST" add mod remote_entity -service game
"$ROOST" add entity AllianceAuthority
"$ROOST" add dao AllianceAuthority --entity AllianceAuthority
# 原服奖励的永久到账回执手写在 Player 上：SLGReceipts（SLG §7.3）
dao_add Player db/def/player.go "BuildingLevel int64 building_level" \
  "ResearchLevel int64 research_level" "PendingResearch int64 pending_research" \
  "PendingBuild int64 pending_build" "Marches int64 marches" "ArrivedX int64 arrived_x" \
  "HomeSID int32 home_sid" "SLGReceipts string slg_receipts"
dao_add Tile db/def/tile.go "OwnerID int64 owner_id"
dao_add AllianceAuthority db/def/alliance_authority.go "State string state"
perl -pi -e 's/dbscope=sid/dbscope=global/' db/def/alliance_authority.go   # Remote 托管要 global
world game/entities/tile/entity.go
perl -pi -e 's/category=entity\.EntityCategoryOther/category=entity.EntityCategoryRemote remote=managed/; s/\*entity\.EntityBase/*entity.RemoteEntityBase/' \
  game/entities/alliance_authority/entity.go
F="$CORE/scripts/testdata/slg"
cp "$F/timer_handlers.go.txt"     game/entities/player/timer_handlers.go
cp "$F/tasks.go.txt"              game/handler/slg_tasks.go
cp "$F/tasks_test.go.txt"         game/handler/slg_tasks_test.go
cp "$F/alliance_rewards.go.txt"   game/handler/alliance_rewards.go
cp "$F/alliance_rewards_test.go.txt" game/handler/alliance_rewards_test.go
cp "$F/alliance_authority.go.txt" game/handler/alliance_authority.go
mkdir -p game/modules/alliance && cp "$F"/alliance/*.go game/modules/alliance/
fix_mod slg-practice game/entities/player/timer_handlers.go game/handler/*.go game/modules/alliance/*.go
gofmt -w db/def/*.go game/entities/player/timer_handlers.go game/handler/*.go game/modules/alliance/*.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" slg
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestSLG' ./game/handler 2>&1 | tee "$WORK/test.log"
go test -mod=mod -race -count=1 -timeout 2m -v \
  -run '^(TestAllianceReportWindowFollowsTheCoordinatorReservationTTL|TestAllianceReceiverBudgetRejectsLateSuccess)$' ./game/modules/alliance 2>&1 | tee "$WORK/alliance.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" wallet
```

两种形态都按 SKILL.md §3.4 收尾。联盟教学包放在 `game/modules/alliance/`，与 alliance 模块同路径：以后要装 alliance 模块先改名。

## 3. 期望场景数

| 形态 | 期望 |
| --- | --- |
| 2.1 精简 | `--- PASS: TestSLG` 恰好 7 条（6 条业务 + `TestSLGRobotBusiness`）、0 SKIP；`modules.sh verify` 打印 `wallet 17/17` |
| 2.2 脚本形态 | `test.log` 里 `TestSLG` 恰好 8 条（2.1 的 7 条 + `TestSLGAllianceRewardOnceBeyondWalletWindow`）；`alliance.log` 里 `grep -c '^--- PASS: TestAlliance'` = 2；`wallet 17/17` |

6 条业务场景：建造 → 行军 → 抵达；重放、取消与退款；独立研究队列；建造事务失败整体回滚；离线资源产出（加载后只补触发一次）；两名玩家抢同一地块只有一人获胜。其余联盟用例（`TestSLGAlliance*` 四条）要真实 Mongo / Redis / NATS，缺省只编译与 vet。

## 4. 限制

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 期限与资源同事务、离线补触发一次、多实体抢地、Remote owner / lease / fence 与旧 owner 写入被拒、关系来源归并视野、activity 阶段 / 贡献去重 / 投递重投 | SLG §4、§5、§7.1 |
| 框架不能做 | 离线实体准点唤醒；Remote 托管实体挂定时器；跨服撤权传播、已下发信息收回；贡献意图 / 补报的通用组件；Redis 义务账本与 Player DAO 之间的分布式原子事务；常驻原服补发调度；自动选主后的业务续跑 | KNOWN-LIMITS“按实体的持久定时器”；SLG §7.1 |
| 视野 | 一个玩家挂的视野（主 + 联盟关系 + 行军 / 侦察）合计 ≤ 9 用缺省 ApplySync，否则 ApplyAsync（尾延迟约多一个同步周期）；每次 Hold → Ready 全量重发 | guide/04-sync.md §2；KNOWN-LIMITS“会话 Hold / Ready” |
| 容量 | Remote 80TPS × 15 分钟服务端 p99 约 263～749ms（共享隔离环境，不是跨机上限）；activity 去重只在 `ReservationTTL` 内有效；Saga 约 1050 saga/s（macOS 单机） | KNOWN-LIMITS“Remote”“Saga”；SLG §7.1、§12.1 |
| 一致性 | 提交确认不等于 Mongo 已可见；“义务先持久接收、再可重试到账”，投递 ACK 不等于玩家已领取；跨服奖励的 Saga 业务键只在运行期间 + 终态保留期内唯一，“同一键永远只发一次”（如赛季奖）要在业务数据里记已用过的键 | SLG §7.1；guide/06-saga.md |
| 业务要自己补 | 行军公式、战争迷雾 / 侦察授权、联盟权限 / 成员上限、名称唯一、战斗算法、赛季规则、加速 / 取消的退款政策 | SLG §4、§8 |
| 缺省模式不证明 | 无 WAL / Mongo，离线恢复用 DAO 序列化 + 手动投递 Tick，不是真实重载。真实依赖档 `ROOST_SLG_REAL=1`（隔离 Mongo 副本集、Redis、NATS）：协调器重启、原服 SIGKILL 后冷加载补完、Remote owner 交接 | SLG §11、§12.1 |

玩家读取联盟加成、资料这类允许快照语义的功能，用 [Remote 指南 §6](../../../framework/guide/05-remote-mirror.md#6-普通-entity-写入--remote-view-读取) 的“普通 Player + Remote View”链路：请求 DTO 放在共用包，字段声明 `remote:"view.XxxSnapshot,consistency=monotonic,required"`，`roost generate` 生成访问器；视图是预取时的快照，不能授权扣联盟资源、审批成员，handler 要校验当前归属与引用一致。

**常见坑**：

- 抢地拆成两次请求会留下“地占了钱没扣”；必须一个多实体事务（SLG §5）。
- 原服奖励改用 wallet 的 `Apply(业务键)`：回执窗口只有最近 128 次，期间玩家又有超过 128 笔带键入账时会重复发。“只发一次”看 Player 上的永久到账回执，入账用 `Add`（SLG §7.3）。
- 持玩家锁的 handler 里再发同步 Nest 请求取地块锁：被 `ErrSyncInHandler` 拒绝。
- 定时器回调里另建调度器或直接改节点 map；只用组件 API（SLG §9）。
- 排行 / 结算榜写增量：乱序回复会覆盖新值；写累计快照、只增不减。
- Mirror 快照陈旧时据快照越权写；读镜像不授予写权。
