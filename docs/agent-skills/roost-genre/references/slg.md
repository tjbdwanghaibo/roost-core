# SLG / 大地图策略

手册：`docs/framework/SLG-INTEGRATION.md`（下称 SLG §N）。路径相对 roost-core 根目录，以检出或 module cache 中的 `docs/` 为准。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 城建、研究、行军队列（期限驱动）；抢地 / 驻守（多实体事务）；联盟、国战（Remote 托管权威）；联盟活动与原服奖励（activity 协调器 + 义务账本） | 行军逐拍移动（按“起点 + 速度 + 出发时刻”算到达，不用心跳逐拍推进，guide/10-time.md §4B）；玩家随联盟跨 sid 迁移（SLG §7.1）；无缝大地图多进程分区（SKILL.md §6） |

## 2. 用到的框架模块

| 业务 | 模块 / 生成入口 | 出处 |
| --- | --- | --- |
| 玩家资源、队列、期限 | Player DAO + `roost add timer --entity Player`（`entitytimer`） | SLG §3.1、§4 |
| 地块归属 | Tile 实体（World 锁档）+ 多实体 Sender | SLG §5 |
| 联盟 / 国战权威 | `roost add mod remote_entity`，`category=entity.EntityCategoryRemote remote=managed`，DAO `dbscope=global` | SLG §7.4 |
| 战争迷雾 / 侦察 / 联盟视野 | Interest 关系来源、Block AOI、Profile 白名单、Hold / Ready | SLG §7.1、§7.5；INTEREST-BLOCK-AOI.md |
| 联盟活动 | `service/activity` 协调器；demo `ActivityRunner` 是接线参考 | SLG §7.1 |
| 跨服奖励 / 交易 | Saga 原生步骤 / effect、邮件 | SLG §8；guide/06-saga.md |

## 3. 生成最小 demo

接在 SKILL.md §3.2 公共前置命令之后（已 `cd "$DIR"`）。对应 `scripts/test-slg-generated.sh` 的建造 / 行军 / 抢地部分。

```bash
"$ROOST" add dao Player --entity Player
"$ROOST" add timer --entity Player
"$ROOST" add entity Tile
"$ROOST" add dao Tile --entity Tile
dao_add Player db/def/player.go "Gold int64 gold" "BuildingLevel int64 building_level" \
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
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestSLG' ./game/handler 2>&1 | tee "$WORK/test.log"
```

期望：`--- PASS: TestSLG` 恰好 **6** 条、0 skip；然后按 SKILL.md §3.4 跑 build / vet / `id check` / `generate --check`。

### 3.1 进阶：加联盟活动（Remote 托管联盟权威）

用一个**新工程**从头执行（替换上面整段）；在上面基础上多三条 add、两段注解改动和联盟业务文件：

```bash
"$ROOST" add dao Player --entity Player
"$ROOST" add timer --entity Player
"$ROOST" add entity Tile
"$ROOST" add dao Tile --entity Tile
"$ROOST" add mod remote_entity -service game
"$ROOST" add entity AllianceAuthority
"$ROOST" add dao AllianceAuthority --entity AllianceAuthority
dao_add Player db/def/player.go "Gold int64 gold" "BuildingLevel int64 building_level" \
  "ResearchLevel int64 research_level" "PendingResearch int64 pending_research" \
  "PendingBuild int64 pending_build" "Marches int64 marches" "ArrivedX int64 arrived_x" \
  "HomeSID int32 home_sid" "Receipts string receipts"
dao_add Tile db/def/tile.go "OwnerID int64 owner_id"
dao_add AllianceAuthority db/def/alliance_authority.go "State string state"
perl -pi -e 's/db=main/db=main dbscope=global/' db/def/alliance_authority.go
world game/entities/tile/entity.go
perl -pi -e 's/category=entity\.EntityCategoryOther/category=entity.EntityCategoryRemote remote=managed/; s/\*entity\.EntityBase/*entity.RemoteEntityBase/' \
  game/entities/alliance_authority/entity.go
F="$CORE/scripts/testdata/slg"
cp "$F/timer_handlers.go.txt"     game/entities/player/timer_handlers.go
cp "$F/tasks.go.txt"              game/handler/slg_tasks.go
cp "$F/tasks_test.go.txt"         game/handler/slg_tasks_test.go
cp "$F/alliance_rewards.go.txt"   game/handler/alliance_rewards.go
cp "$F/alliance_authority.go.txt" game/handler/alliance_authority.go
mkdir -p game/alliance && cp "$F"/alliance/*.go game/alliance/
fix_mod slg-practice game/entities/player/timer_handlers.go game/handler/*.go game/alliance/*.go
gofmt -w db/def/*.go game/entities/player/timer_handlers.go game/handler/*.go game/alliance/*.go
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestSLG' ./game/handler 2>&1 | tee "$WORK/test.log"
go test -mod=mod -race -count=1 -timeout 2m -v \
  -run '^(TestAllianceReportWindowFollowsTheCoordinatorReservationTTL|TestAllianceReceiverBudgetRejectsLateSuccess)$' ./game/alliance 2>&1 | tee "$WORK/alliance.log"
```

期望：`test.log` 里 `--- PASS: TestSLG` 恰好 6 条、`alliance.log` 里两条顶层 PASS（`grep -c '^--- PASS: TestAlliance'` = 2），合计 8 条，即 `scripts/test-slg-generated.sh` 缺省模式（pretag 跑的就是这个形态）的全部断言；0 SKIP，再按 SKILL.md §3.4 跑 build / vet / `id check` / `generate --check`。其余联盟用例（`TestSLGAlliance*` 四条）要真实 Mongo / Redis / NATS，缺省只编译与 vet（SLG §11、§12.1）；`-run '^TestSLG'` 只跑 `./game/handler`，不会碰到它们。

## 4. demo 的最小玩法范围（6 个场景，SLG §3.4）

| 场景 | 学到的框架契约 |
| --- | --- |
| 建造 → 行军 → 抵达 | DAO 与期限同事务；回调登记的下一阶段进入驱动索引 |
| 重放、取消与退款 | 目标等级做幂等事实；取消的节点不再触发 |
| 独立研究队列 | 同一实体多个期限独立到期 |
| 建造事务失败 | 资源、待建节点、驱动索引一起回滚 |
| 离线资源产出 | 加载后只补触发一次，补算公式与上限（样例 10 期 / 1000）由业务给 |
| 两名玩家抢同一地块 | 多实体 Sender + 锁序；只有一人获胜，归属与扣款同提交 |

## 5. 业务限制清单

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 期限与资源同事务、离线补触发一次、多实体抢地、Remote owner / lease / fence 与旧 owner 写入被拒、关系来源归并视野、activity 阶段 / 贡献去重 / 投递重投 | SLG §4、§5、§7.1 |
| 框架不能做 | 离线实体准点唤醒（到点给离线玩家发奖要业务发 Nest 消息）；Remote 托管实体挂定时器；跨服撤权传播协议、已下发信息收回；贡献意图 / 补报的通用组件；Redis 义务账本与 Player DAO 之间的分布式原子事务；常驻原服补发调度；自动选主后的业务续跑 | KNOWN-LIMITS“按实体的持久定时器（D11）”；SLG §7.1 |
| 视野边界 | 一个玩家挂的视野（主 + 联盟关系 + 行军 / 侦察）合计 ≤ 9 用缺省 ApplySync，否则 ApplyAsync（尾延迟约多一个同步周期）；每次 Hold → Ready 全量重发仍在的对象，只在权限读失败与重连时 Hold | guide/04-sync.md §2 选型表与“剩余差距”；KNOWN-LIMITS“会话 Hold / Ready” |
| 容量边界 | Remote 80TPS × 15 分钟服务端 p99 约 263～749ms（共享隔离环境，非跨机上限）；activity 去重只在 `ReservationTTL` 内有效；Saga 正式实现约 1050 saga/s（macOS 单机）；协调器子进程重启演练只做正常排空 | KNOWN-LIMITS“能力与验证边界”Remote容量；SLG §7.1、§12.1；KNOWN-LIMITS“Saga” |
| 一致性边界 | strict = WAL fsync，不等 Mongo 投影；“义务先持久接收、再可重试到账”，投递 ACK ≠ 玩家已领取；demo 的活动尽力贡献失败不补报是已接受取舍，赛季要求不漏计要按 §7 做持久意图；跨服奖励 / 交易的 Saga 业务键只在运行期间 + 终态保留期内唯一，保留期后可复用、靠协调器生成的生命周期身份区分前后两轮（维护者 2026-10-31 定案 A），“同一键永远只发一次”（如赛季奖）要在业务数据里记已用过的键 | SLG §2 图下说明、§7.1 活动学习顺序；SAGA.md“定义”业务键与查询；guide/06-saga.md 幂等键 |
| 业务要自己补 | 行军公式、战争迷雾 / 侦察授权、联盟权限 / 成员上限、名称唯一（directory）、战斗算法、赛季规则、加速 / 取消的退款政策 | SLG §4、§8 |

## 6. 常见坑

- 抢地拆成两次请求会留下“地占了钱没扣”；必须一个多实体事务（SLG §5）。
- 持玩家锁的 handler 里再发同步 Nest 请求取地块锁：被 `ErrSyncInHandler` 拒绝（SLG §5）。
- 定时器回调里另建调度器或直接改节点 map：DAO 有节点、驱动没期限；只用组件 API（SLG §9 第一行）。
- 手改生成的 Sender（如冲突参数名生成为 `idArg`）：下次生成被覆盖（SLG §9 第二行）。
- 排行 / 结算榜写增量：乱序回复会覆盖新值；写累计快照、只增不减（SLG §9 第四行）。
- Mirror 快照陈旧时据快照越权写；读镜像不授予写权（SLG §7.1、§8）。

## 7. 验证

- 一档：§3 的 6 条 PASS + SKILL.md §3.4。2026-10-31 在 v1.33.0 检出、自定 module `example.com/myslg` 实测：6 PASS、build / vet / `id check` / `generate --check` 通过；进阶版另 2 条 alliance 用例 PASS。同日在 v1.33.1 复测：§3 自定 module 6 PASS、0 SKIP、§3.4 全过（doctor 仅 4 个 collaborators FAIL）；§3.1 进阶版 6 + 2 = 8 条 PASS，build / vet / `id check` / `generate --check` 通过。“pretag 报 8 条”即这 6 + 2，`scripts/test-slg-generated.sh` 的 `TestSLG` 断言仍是 6。2026-10-31 在 v1.33.2 检出（`e4ec6260`）复测（自定 module `example.com/myslg` / `myslgadv`）：§3 6 PASS；§3.1 6 + 2 PASS；均 0 SKIP，§3.4 全过，doctor 仅 4 项 collaborators FAIL。
- 二档：`ROOST_SLG_REAL=1 bash scripts/test-slg-generated.sh`（需隔离 Mongo 副本集、Redis、NATS，共享环境先持有 `$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock`）：协调器重启、原服 SIGKILL 后冷加载补完、Remote owner 交接（SLG §12.1）。
- 不证明的（缺省模式）：无 WAL / Mongo、离线恢复用 DAO 序列化 + 手动投递 Tick，不是真实重载（SLG §11）。

## 8. 手册指针

- `docs/framework/SLG-INTEGRATION.md`：§3 生成与阅读顺序、§7 跨服联盟活动、§8 更多路线表、§9 通用接入原则、§11～§12 验收
- `docs/framework/guide/10-time.md` §4（实体定时器）、`docs/framework/guide/05-remote-mirror.md`、`docs/framework/INTEREST-BLOCK-AOI.md`
- `docs/maintenance/KNOWN-LIMITS.md`“游戏接入验收”SLG 行
- 参考实现：`demo/`（game-demo 模板）的 Map 组件、Guild、`activity.go.tmpl`、`saga/gift_item`
