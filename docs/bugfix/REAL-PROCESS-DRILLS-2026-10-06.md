# 真实进程演练（2026-10-06）：单实例锁 / fail-stop / 静态绑定重跑、Init 中途失败、停机 hook 卡住、kit Mongo / NATS 真实 Close、loadtest rc=1、Redis Cluster 重跑

维护者要求交给 review 前不留未闭环项（[roost-bugfix §7](../agent-skills/roost-bugfix/SKILL.md)）。本记录把四处“未验证”换成真实进程 / 真实依赖上的实测；drill3 追加两项上一轮留下的未闭环（⑤ loadtest 的 rc=1、⑥ Redis Cluster 下的演练）：

| 项 | 出处的“未验证” | 发版条目 | 结论 |
| --- | --- | --- | --- |
| ① 重跑 game-demo 演练（单实例锁、失锁 fail-stop、静态绑定、6b） | [App 锁方案 §13](../feature/APP-SINGLETON-LOCK-2026-10-05.md) 第 5 笔：“`c493a791` 与 obs34 之后的代码没有重跑演练” | APP-1、OWN-2、OWN-3 | 全部符合预期，等待时长与参数一致；无新缺陷 |
| ② Init 中途失败的收尾 | [NC-193](RR-20261005-NC-193.md)：“真实 game-demo 进程里制造 `startActivityPhaseConsumer` 失败” | APP-9、APP-12 | 符合：Service 先收回、25 个 Mod 逆序停完、锁释放、退出码 1、文件日志有 `app run failed` |
| ③ 停机 hook 卡住时 SIGTERM | [NC-231](RR-20261005-NC-231.md)：“真实进程里业务 hook 卡住时的 SIGTERM → 宽限期时序” | APP-8 | 时序符合（run 在 10s 预算处返回）；**日志不点名卡住的 hook——登记并修复 [RR-20261006-25](RR-20261006-25.md)** |
| ④ kit Mongo / NATS Mod 接真实依赖的 Close | [RR-20261006-10](RR-20261006-10.md)：“未用真实 Mongo / etcd / NATS” | APP-7、DRV-5 | Mongo 符合；NATS 发现两处：**[RR-20261006-24](RR-20261006-24.md)**（关闭后订阅 / JetStream / CallAsync 的错误 `errors.Is` 不到 `fnats.ErrClosed`）、**[RR-20261006-26](RR-20261006-26.md)**（排空被硬关后 `Connected()` 约 5s 内仍为 true）；均已修复 |
| ⑤ 两个 sid 时 loadtest 全部成功仍 `rc=1`（drill3） | 本记录 ① 的“观察”（10-05、10-06 都出现，当时没有处理） | OWN-3（已知限制）、NONCORE-43 | 统计口径错误：p95 是“最慢样本所在桶的上界”，不是真实延迟——**[RR-20261006-27](RR-20261006-27.md)**；修后两个 sid `rc=0`，p95 为最慢机器人的真实耗时，失败时点名阈值与实际值 |
| ⑥ Redis Cluster 下重跑 ① 的核心步骤（drill3） | [App 锁方案 §13](../feature/APP-SINGLETON-LOCK-2026-10-05.md) 第 5 笔：“Redis Cluster 下的真实进程演练”未验证 | APP-1、APP-5、OWN-2、OWN-3、CFG-5 | 等待 / 接管、失锁 fail-stop、静态绑定、在途 saga 接手全部符合，与单机一致；准备环境发现 **[RR-20261006-28](RR-20261006-28.md)**（生产校验只认 `redis.addr`、accountctl / dev run.sh 只会连单机），跑的过程中发现 **[RR-20261006-29](RR-20261006-29.md)**（每个活动窗口开头最多 5s 通关不计入，与 Cluster 无关）；均已修复 |

恢复自 WIP 分支 `wip/drill`（`976086d6`，额度用尽暂停时的草稿）：WIP 里 `nats/driver` 的代码改动经核对是演练第 4 项在真实 NATS 上发现的缺陷（订阅 / JetStream 的关闭错误未映射），按 RR 流程补红绿后保留并扩到 RPC CallAsync，编号 RR-20261006-24（WIP 注释里的 “RR-20261006-17” 是误写，-17 是 OpenActivity）；WIP 的演练脚本路径写死旧 scratchpad、`drill1b.sh` 的裸 `wait` 会一直等后台游戏进程（WIP 输出停在 6a 就是这个原因），已整理成下面可复跑的版本。

## 环境与被测代码

- 被测：main `37338490` + 本批改动（`nats/driver` 的 RR-24 修复已在；RR-25 / RR-26 的修复在演练中途做出，见各项说明）。时间线首行的 `a632a057+dirty` 是 rebase 后的 WIP 提交加工作区改动。
- 依赖全部私有：`scripts/mirror-local.sh up`（根目录在本 agent 的 scratchpad，端口偏移 30000：Mongo 副本集 3 节点、NATS JetStream 3 节点、Redis），自起 etcd（`127.0.0.1:52379`）。不碰共享隔离环境 `~/.roost-it`，没有写共享的 game / saga / remote_entity 库与 `ROOST_*` 流（私有实例里的同名库 / 流随 `mirror-local.sh clean` 删除）。
- 生成工程：`roost project new rpd -template game-demo` + `go mod edit -replace` 指向被测检出；DAO 库名编译期常量 `*DaoDBName` 改为 `rpd2drill_game`，`dataengine.database` 同改；九个框架服务各一个（sid 1000），`accountctl upsert-server` 登记 game 1000 / 1001。单实例锁参数用生成默认值 `ttl / renew_interval / guard / startup_wait = 15s / 3s / 5s / 30s`。
- 演练只用的注入件（不进生成器，只在私有工程里）：`cmd/drillinject`（预建冲突的 JetStream consumer）、`internal/service/game/zz_drill_hooks.go`（`ROOST_DRILL_STUCK_HOOK` 时登记一个不配合 ctx 的停机 hook）。
- 结束时已停掉全部 `bin/app`、etcd，`mirror-local.sh clean` 删除私有依赖与数据，`ps` 复查无残留。

### 复跑

脚本在 [evidence/real-process-drills/scripts/](evidence/real-process-drills/scripts/)，`DRILL_HOME` 指向任一私有目录，`CORE_DIR` 指向被测的 roost-core 检出：

```bash
export DRILL_HOME=<私有目录> CORE_DIR=<roost-core 检出>
S=$CORE_DIR/docs/bugfix/evidence/real-process-drills/scripts
bash $S/setup.sh && bash $S/frameworks.sh   # 私有依赖 + etcd + 生成 / 编译 + 九个框架服务
bash $S/drill1.sh                           # ① 单 sid：步骤 0～5、1c/2c，结束时 P7 在 sid 1000
bash $S/drill1b.sh                          # ① 两个 sid：6、6a、6c、6b（6b 调 drill1-6b.sh P7 P8）
bash $S/drill2.sh P8                        # ② Init 中途失败（参数：sid 1000 上正在运行的进程名）
bash $S/drill3.sh G2                        # ③ 停机 hook 卡住
bash $S/teardown.sh                         # 停进程、etcd，mirror-local clean
# ④ 真实依赖 Close 用例（需要私有环境在跑，teardown 之前）：
source $DRILL_HOME/env/roost-dataengine-it/env.sh   # 只 source、不打印
GOWORK=off go test -tags integration -race -count=3 -run 'TestRealNatsMod|TestRealMongoMod' ./kit/nats/ ./kit/mongo/
```

证据（只提交摘录，主机名与 token 已脱敏）：[时间线与日志摘录](evidence/real-process-drills/2026-10-06/)。原始日志留在 scratchpad，不进仓库。

Redis Cluster 模式（⑥）：同一组脚本，多设 `DRILL_REDIS=cluster`（`setup.sh` 把生成配置的 `redis.addr` 换成 mirror-local 3 主 3 从的 `redis.cluster_addrs`，activity / platform / rank 及 game 配置里同名的 `key_prefix` 加 hash tag，game 配置加 `remote_entity.lock_key: "{roost:rpd:remote}"`；`lib.sh` 的 redis-cli 用 `-c`、accountctl 用 `-redis-cluster`）。每轮用唯一的 DAO 库名与自己的端口偏移，etcd 端口可用 `DRILL_ETCD_PORT` 换：

```bash
export DRILL_HOME=<私有目录> CORE_DIR=<roost-core 检出> DRILL_REDIS=cluster DRILL_OFFSET=31000 DRILL_DAO_DB=rpd3cl_game
bash $S/setup.sh && bash $S/frameworks.sh && bash $S/drill1.sh && bash $S/drill1b.sh && bash $S/teardown.sh
```

`frameworks.sh` 只能跑一次（重跑会再起一套、覆盖 pid 文件，teardown 找不到先起的进程）；要重置数据就整套 teardown + setup，不要只删 Mongo 库（remote_entity 库与 Redis 里的 L2 快照还在，见 ⑥ 的环境说明）。

## ① 单实例锁、失锁 fail-stop、静态绑定（APP-1 / OWN-2 / OWN-3）

步骤同方案 §8.2（P* 都是 sid 1000、同一配置与 WAL 目录）。[时间线](evidence/real-process-drills/2026-10-06/timeline-drill1.txt) · [两 sid](evidence/real-process-drills/2026-10-06/timeline-drill1b.txt) · [日志摘录](evidence/real-process-drills/2026-10-06/log-excerpts-drill1.txt)

| 步骤 | 动作 | 观测 | 耗时 |
| --- | --- | --- | --- |
| 0 | 起 P1；机器人 A、A2 各 10 个 | 都 `success=10`，`rc=0`；Mongo 20 个玩家 | 起到 `/readyz` 1.4s |
| 1 | SIGSTOP P1（键剩 14.98s）→ 起 P2 | P2 只有 `singleton: acquiring` / `waiting … holder=<token>\|<host>\|28522\|…`，没有任何 `mod init`；`acquired` 后 `mod init` lock / ops / stats_log，`ops: listen on ops.addr 127.0.0.1:36100: … address already in use`（NC-230：ops 同步 bind，先于 DataEngine 失败），逆序停完、`singleton: released`，退出码 1；键随即不存在 | 等锁 15.010s，拿锁到退出 1ms |
| 2 | SIGCONT P1 | `singleton lock lost; fail-stop`（holder 为空）→ `runtime infrastructure failure` → `service shutdown` → `server stopped` → `singleton: lock was lost; not releasing` → `app run failed`，退出码 1；etcd 租约过期按 `lease already gone` 记 Info（`b67d5945` 生效，不再并入退出错误）；全部日志无 `fatal projection version conflict` | SIGCONT 到失锁 5ms，到 `app run failed` 63ms |
| 3 | 起 P2b | 立即 `acquired`、就绪；Mongo 20 个玩家与 SIGSTOP 时快照逐字段相同；机器人 B 10 个 `success=10`，30 个玩家 | 起到就绪 0.14s |
| 4 | kill -9 P2b（键剩 14.34s）→ 立刻起 P3 | 等待后 `acquired`，`service init` 正常（activity 无租约冲突） | 等锁 15.009s |
| 5 | SIGTERM P3 → 0.1s 后起 P4 | P3 停完 `released`、退出码 0；P4 在释放之前开始获取，下一拍拿到 | P4 等锁 3.002s |
| 5b | SIGTERM P4，等它退出再起 P5 | P4 退出码 0、键不存在；P5 立即 `acquired` | 等锁 0 |
| 1c/2c | 4 个只 `enter_game` 后空闲的机器人连在 P5（`player_tcp_connections 4`），SIGSTOP（键剩 14.76s）→ P6 → SIGCONT | P6 等锁后同步骤 1 退出码 1；P5 失锁后 `game: disconnected the players this process served sessions_closed=4`、退出码 1；P7 立即接管 | P6 等锁 15.006s |
| 6 | P7（1000）在跑，起 Q1（1001） | 两个键各自持有；activity 开窗 `expected_game_sids=[1000, 1001]`（`Live`） | Q1 0.15s 就绪 |
| 6a | 两个 sid 各 10 个机器人 | 都 `success=10`；40 个 saga 全部终结（20 completed / 20 compensated，`max_attempt=0`）；P7 27 次 `step left to the sender's sid` / 14 次 `ran a step handed over by another process`，Q1 24 / 14 | — |
| 6c（OWN-2） | 角色绑定在 1001 的 2 个机器人连 1000 的端点 | `enter_game` 回 `response code 100015: player is bound to another server; reconnect to that server` | — |
| 6b（OWN-3） | 1000 上 10 个机器人赠礼进行中 kill -9 当前进程 → 立刻起新进程 | 第一次（P7→P8）沿用旧脚本 4.5s 后 kill，20 个 saga 已全部终结，**不算反例**（当前代码赠礼比 10-05 快）；脚本改为轮询到出现在途 saga 再 kill。第二次（P8→P9）：kill 时 5 个在途（`status=2`，phase 1 step 0，即扣款），之后共 9 个；P9 上线后由 Q1 按 `FromSID` 转交、P9 执行 5 次 handed-over 步骤，9 个全部 completed，`manual_required` 为 0 | P9 等锁 15.007s，kill 到就绪 15.60s；最后一个在途 saga 在就绪后 0.51s 终结 |

结论：§8.2 六步与 1c、6、6a、6b、6c 在当前代码上全部符合预期，与 10-05（`64acd782`）的结论一致，`c493a791` 与 obs34 之后没有退化。等待时长 15.0s（kill -9 / SIGSTOP 后立刻起）与 3.0s（旧进程停机中起）同 10-05 的解释。

- 与 10-05 的差别：6b 的在途 saga 这次全部 completed（10-05 是 7 个 compensated）：kill 落在扣款步骤（phase 1），接管后扣款与投递照常完成；10-05 kill 落在第二笔、按设计会被拒的赠礼上。两者都是“在途 saga 由新进程接手到终态、无人工介入”。
- 观察（与 10-05 相同，未改）：两个 sid 同时跑时 loadtest 的 p95 落进 16.384s 桶，超过缺省 `-max-p95 16`，`rc=1`（机器人全部成功），单 sid 时 `rc=0`。**更正（drill3）**：查明是分位数估计的口径错误，不是延迟，见 ⑤ 与 [RR-20261006-27](RR-20261006-27.md)，已修复。

## ② Init 中途失败的收尾（NC-193 / APP-9）

[时间线](evidence/real-process-drills/2026-10-06/timeline-drill2.txt) · [日志摘录](evidence/real-process-drills/2026-10-06/log-excerpts-drill2.txt)

注入：先把 game 的 durable `game-activity-phase`（流 `ROOST_EFFECTS`，过滤 `roost.effect.activity.phase_due`）删掉、按 `DeliverLast` 重建（`bin/drillinject conflict …`）。game 的 `Service.Init` 最后一步 `startActivityPhaseConsumer` 用 `DeliverAll` 调 `CreateOrUpdateConsumer`，服务端拒绝。此时 Init 前面的场景、spawner、matchmaker、赠礼 saga、activity 循环都已经启动——正是 NC-193 要收回的“已启动的部分”。

```
22:25:51.232 INFO singleton: acquired key=roost:rpd:singleton:game:1000 …
22:25:51.262 INFO service init service=game
22:25:51.277 WARN activity: window not opened activity_id=race-1791296700 group=rpd err=nats: request cancelled
22:25:51.277 INFO mod stop (service-specific) mod=access.player.tcp …        （服务专属 Mod 共 22 个，逆序）
22:25:51.341 INFO mod stopped mod=config_data …
22:25:51.341 INFO mod stop mod=stats_log … / ops … / lock …                 （共享 Mod 逆序）
22:25:51.341 INFO singleton: released key=roost:rpd:singleton:game:1000
22:25:51.341 ERRO app run failed err=service game init: game: nats: API error: code=500 err_code=10012 description=deliver policy can not be updated
```

- 退出码 1；`mod init` 25 / `mod start` 25 / `mod stopped` 25，`mod stop failed` 0；键随即不存在；`app run failed` 在文件日志里（RR-20261006-07 / APP-12）。
- Service 先于 Mod 收回：第一条 `mod stop` 之前，activity 循环在途的 bus 请求以 `nats: request cancelled` 结束——它的 ctx 被 `Shutdown` 取消（NATS Mod 此时还没停，13ms 后才停）。启动失败的收尾路径不打 `service shutdown`，这一点只能从这条告警推断；“先收回、再停 Mod”的顺序本身由 App 层用例 `TestServiceInitFailureStopsWhatInitStartedBeforeTheMods` 钉住。
- 恢复：`bin/drillinject restore` 删掉冲突的 consumer，G2 0.14s 就绪、拿锁。

结论：NC-193 的收尾在真实进程上成立。

## ③ 停机 hook 卡住时 SIGTERM（NC-231 / APP-8）

[时间线（修前 / 修后）](evidence/real-process-drills/2026-10-06/timeline-drill3.txt) · [日志摘录](evidence/real-process-drills/2026-10-06/log-excerpts-drill3.txt)

`config.game.hook.yaml` 把 `shutdown.total_timeout` 缩到 10s；`ROOST_DRILL_STUCK_HOOK=service.stopping|service.stopped` 登记一个不配合 ctx 的 hook（名字 `drill-stuck-<阶段>`），就绪后 SIGTERM。

| 阶段 | 观测 | 耗时 |
| --- | --- | --- |
| service.stopping 卡住 | `service shutdown` 之后 hook 进入；没有 `mod stopped`（0 条），不调 Service.Shutdown；`singleton: shutdown incomplete; leaving the key to expire` / `keeping the store open`，键留着（退出后剩 13.5s 自然过期）；退出码 1 | SIGTERM 到 `app run failed` 10.006s |
| service.stopped 卡住 | 25 个 Mod 停完、`server stopped` 之后 hook 进入；`singleton: no shutdown time left to release; leaving the key to expire`；退出码 1 | SIGTERM 到 `app run failed` 10.005s |

时序符合 NC-231：run 在停机预算处返回，部署侧宽限期（total + 5s）内进程自己退出；stopping 卡住时依赖与锁按“停机不完整”保留。

**发现**：修前的退出错误只有阶段——`app: lifecycle service.stopping hooks did not return within the shutdown budget: context deadline exceeded`——不点名 hook。同一阶段常有多个 hook（Ops 的就绪位、configdata、业务登记的），T-264 的判别也要求“看是哪个 phase 与 hook 名字”。登记 [RR-20261006-25](RR-20261006-25.md) 并修复，修后重跑：

```
22:28:43.650 ERRO app run failed err=app: lifecycle service.stopping hook "drill-stuck-service.stopping" did not return within the shutdown budget: context deadline exceeded
22:29:08.169 ERRO app run failed err=app: lifecycle service.stopped hook "drill-stuck-service.stopped" did not return within the shutdown budget: context deadline exceeded
```

## ④ kit Mongo / NATS Mod 接真实依赖的 Close（RR-20261006-10 / APP-7 / DRV-5）

[红绿摘录](evidence/real-process-drills/2026-10-06/app7-close-contract-real.txt) · [RR-26 红](evidence/real-process-drills/2026-10-06/rr26-red.txt)

integration 用例（`-tags integration`，读 `ROOST_DATAENGINE_IT_MONGO_URI` / `ROOST_DATAENGINE_IT_NATS_URL`，没有就跳过）经正式的 Init → Provide → Start 接真实依赖：

- `kit/mongo` `TestRealMongoModCloseContract`：4 个并发 Stop 全部 nil（`-race` 无竞争）、`Client()` 释放；再 Stop 两次 nil；`IMongo.Close` 再调 nil；之后 Ping / InsertOne / FindOne / UpdateOne / CountDocuments / 会话事务全部立即（0s）返回 `client is disconnected`，`errors.Is(mongo.ErrClientDisconnected)`。库名 `rr1006_10_<ns>`，Stop 前删除。**首跑即绿。**
- `kit/nats` `TestRealNatsModCloseContract`：4 个并发 Stop 全部 nil、`asm` 释放；再 Stop 两次 nil；之后经 Mod 发布的 IClient（Publish / Request / Subscribe / QueueSubscribe）、IRpc（Call / CallAsync）、IJetStream（EnsureStream / Publish / Subscribe）、IBus（Send / Call）都立即返回、`errors.Is(fnats.ErrClosed)`。**首跑红**：Subscribe、QueueSubscribe、CallAsync、JetStream 三个方法的错误 `errors.Is` 不到 `fnats.ErrClosed` → [RR-20261006-24](RR-20261006-24.md)，修后绿。
- `kit/nats` `TestRealNatsModUndrainedCloseIsReportedOnce`：直连订阅的 handler 卡住，200ms 预算的 Stop 第一次返回 `ErrClosedUndrained`（包着 `DeadlineExceeded`）并释放引用；之后 3 个并发 Stop、对同一 Assembly 的 Close 都是 nil——终态错误只报一次；Publish 返回 `fnats.ErrClosed`。`-race -count=3` 循环里偶发（2/150）“硬关之后 `Connected()` 仍为 true”，查明是 nats.go 排空协程在硬关后把状态翻回 `DRAINING_PUBS` → [RR-20261006-26](RR-20261006-26.md)；断言改为硬关后 300ms 内一直为 false，修前 20/20 红，修后 `-count=30` 绿。
- 修后 `GOWORK=off go test -tags integration -race -count=3 -run 'TestRealNatsMod|TestRealMongoMod' ./kit/nats/ ./kit/mongo/` 通过。`kit/mongo` 因此有了需要完整环境的 integration 用例，根包门禁 `TestFaultMatrixScriptNamesEveryFullEnvironmentSuite` 要求故障矩阵脚本也跑它：`kit/scripts/integration/dataengine-env.sh` 的包列表加 `./kit/mongo`。
- 未覆盖：etcd 的真实 Close（本轮范围是 kit Mongo / NATS Mod；etcd 的 `TestRealEtcdCloseAfterLeaseVanishedIsClean` 已在 `b67d5945` 用真实 etcd）。

## ⑤ 两个 sid 时 loadtest 全部成功仍 rc=1（drill3，RR-20261006-27）

原始输出（10-06 步骤 6a，机器人 C）：`state=failed stop_reason=threshold success=10 elapsed=9.635844208s`，`quantiles_ms {p50 9557, p90 15018, p95 16384, p99 16384}`，`p95 actual 16.384 > max 16`，最后一行只有 `ended failed (threshold)`。

- **p95 为什么是 16.384s**：不是真实延迟（整次运行 9.6s 就结束了，每个场景耗时都不超过它），也不是把排队 / 开窗算进去（耗时从 `runner.runOnce` 起算，在运行窗口内）。是估计口径：`metrics` 直方图的桶翻倍，`quantile` 在命中桶的 `[下界, 上界]` 里插值，排名落在桶内最后一个样本时估计就是桶上界；10 个样本时 p95 的排名是第 10 个，于是 p95 = 最慢机器人所在桶 `(8.192s, 16.384s]` 的上界。溢出一侧相反：落进溢出的排名返回 65.536s，比样本小。
- **rc=1 怎么判的**：`actual > max` 的判定本身按设计，错在 actual。缺省 `-max-p95 16` 的说明“健康运行落在 8s 桶，上限取下一个桶”与数值矛盾（下一个桶上界 16.384 > 16）：最慢的机器人一过 8.192s 就必然失败。单 sid 时最慢约 7.4～8.3s，恰好在 8.192s 两边，所以单 sid 多数通过。
- **处理**：按缺陷修（RR-20261006-27）。直方图记最小 / 最大观测值，插值区间收在 `[最小, 最大]` 内（10 个样本的 p95 就是最慢样本）；阈值失败时 `RunSnapshot.Error` 写 `threshold violated: p95 = 17.2s > max 16s (10 samples)`，生成的 loadtest 把它带进退出前最后一行，`ThresholdResult` 多 `samples`。阈值 16 不改：修正后两个 sid 的 p95 是 9.8～10.1s，16 约两倍余量；`-max-p95` 的说明改成真实依据。
- 实测（⑥ 的两轮）：两个 sid C / D `rc=0`，p95 10.049 / 9.842s（第一轮）、10.130 / 9.837s（第二轮）；单 sid A / A2 / B 7.39～7.82s。[日志摘录](evidence/real-process-drills/2026-10-06-cluster/log-excerpts-cluster.txt) · [修前红](evidence/real-process-drills/2026-10-06-cluster/rr27-red.txt)

## ⑥ Redis Cluster 下重跑单实例锁、fail-stop、静态绑定与在途 saga 接手（drill3）

[时间线（第二轮，修后二进制）](evidence/real-process-drills/2026-10-06-cluster/timeline-cluster.txt) · [第一轮](evidence/real-process-drills/2026-10-06-cluster/timeline-cluster-run1.txt) · [日志摘录](evidence/real-process-drills/2026-10-06-cluster/log-excerpts-cluster.txt) · [切换过程中的失败](evidence/real-process-drills/2026-10-06-cluster/cluster-setup-attempts.txt)

- 被测：drill3 worktree（main `0db819b0` + RR-27 / 28 的修复；第二轮再加 RR-29）。依赖全部私有：`scripts/mirror-local.sh up`（偏移 31000：Mongo 副本集、NATS JetStream 3 节点、Redis 3 主 3 从 Cluster，node-timeout 1s）、自起 etcd（`127.0.0.1:52379`）。DAO 库名 `rpd3cl_game`。不碰共享隔离环境、共享的 game / saga / remote_entity 库与 `ROOST_*` 流。结束 `teardown.sh`：停全部 `bin/app` 与 etcd，`mirror-local.sh clean` 删除私有依赖与数据（DAO 库随之删除），`ps` 复查无残留。
- 全部九个框架服务和两个 game 的 `redis.*` 只有 `cluster_addrs`（6 个种子）。两个 sid 的锁键在不同主节点上：`…:game:1000` 槽 12937（主 48402）、`…:game:1001` 槽 8872（主 48401）；`Live` 逐键读，开窗 `expected_game_sids=[1000,1001]`，无 `CROSSSLOT` / `MOVED` 报错。
- 生成工程切到 Cluster 要改的设置（演练记录了每一步的失败，清单写进 [USER_GUIDE](../USER_GUIDE.md#生成工程切到-redis-cluster)）：`cluster_addrs`；activity / platform / rank 的 `key_prefix` 带 hash tag（缺了 Init 点名报错）；game 配置里同名的 `activity.key_prefix` / `platform.key_prefix` 一起改（只改服务一侧时购买发货落空，机器人报 `delivered 10 of item 1001 but the bag holds 0`）；`remote_entity.lock_key` 带 hash tag（缺省 `e` Init 报错）；accountctl `-redis-cluster`。前两类与 lock_key 是设计如此的启动校验；做不到的三处登记为 RR-20261006-28 并修复（生产校验只认 `redis.addr`；修前 accountctl 对 6 个节点逐个登记，5 个 `MOVED 14044 127.0.0.1:48402`；dev run.sh 只读 `addr`）。

| 步骤 | 观测（第二轮） | 耗时 | 与单机（①）比较 |
| --- | --- | --- | --- |
| 0 | P1 就绪；机器人 A、A2 各 10 个，起跑对齐到活动窗口边界 23:15:00 前 1.4s，都 `success=10`、`rc=0` | 起到就绪 1.0s | 同 |
| 1 | SIGSTOP P1（键剩 13.8s）→ P2 `waiting … holder=<token>\|<host>\|63664\|…`，没有 `mod init`；拿锁后 ops 端口占用、逆序停完、`released`，退出码 1 | 等锁 15.010s | 同 |
| 2 | SIGCONT P1 → `singleton lock lost; fail-stop`（holder 为空）→ `server stopped` → `lock was lost; not releasing` → `app run failed`，退出码 1；无 `fatal projection version conflict` | SIGCONT 到失锁 5ms，到 `app run failed` 69ms | 同（单机 5ms / 63ms） |
| 3 | P2b 立即拿锁、就绪；玩家快照与 SIGSTOP 时逐字段相同；机器人 B `success=10` | 0.13s | 同 |
| 4 | kill -9 P2b（键剩 14.4s）→ P3 等锁后就绪 | 等锁 15.001s | 同 |
| 5 / 5b | SIGTERM P3，P4 在释放前开始获取，下一拍拿到；P4 退出后 P5 立即拿锁 | P4 等锁 3.001s；P5 0 | 同 |
| 1c/2c | 4 个空闲连接在 P5，SIGSTOP → P6 等锁后退出码 1 → SIGCONT P5 失锁，`disconnected the players this process served sessions_closed=4`，退出码 1；P7 立即接管 | P6 等锁 15.006s | 同 |
| 6 / 6a | Q1（1001）就绪，两个键各自持有；两个 sid 各 10 个机器人 `rc=0`；40 个赠礼 saga 全部终结（20 completed / 20 compensated，`max_attempt=0`）；P7 32 次转交给发送方 sid、17 次执行转交来的步骤，Q1 31 / 17 | — | 同；单机时这里 `rc=1`（⑤） |
| 6c（OWN-2） | 绑定在 1001 的 2 个角色连 1000：`response code 100015: player is bound to another server` | — | 同 |
| 6b（OWN-3） | kill -9 时 3 个在途（2 个 phase 1 step 0、1 个 step 1），之后共 9 个；P8 等锁 15.007s 后上线，执行 4 次转交来的步骤，9 个全部 completed，`manual_required` 0 | kill 到就绪 15.6s；最后一个在途 saga 就绪后 0.6s 终结 | 同 |

第一轮（RR-29 修复前的二进制，[时间线](evidence/real-process-drills/2026-10-06-cluster/timeline-cluster-run1.txt)）锁、fail-stop、静态绑定、6b 的结果与第二轮相同（6b：kill 时 3 个在途，P8 接手 5 次，9 个全部 completed）；两轮之间唯一的差别是步骤 0：P1 23:04:57 起，机器人恰在 23:05:00 的窗口边界上通关，A 有 5 个 `finish_dungeon: the clear did not say which activity window took its point`（game 日志 `clear not contributed to the activity … activity: not found`，23:05:03.2～04.0；新窗口 23:05:04.08 才由循环开出），少了 5 个机器人排队，第 6 个 `poll_match` 落单；A2 的 2 个 `poll_match` 超时是同一件事的连带——A 落单机器人的票据在玩家卸载前仍可被撮合（matchmaker 的设计，`ownedCandidates` 注释），占走了一个 A2 机器人的对手。根因是开窗循环每 5s 才开一次窗口，与 Cluster 无关，登记 [RR-20261006-29](RR-20261006-29.md) 并修复：`Contribute` 遇到窗口不存在时自己开窗再记。第二轮把起跑对齐到 23:15:00 前：协调方的窗口记录 `race-1791299700` 的 `opened_at_unix=1791299701`（23:15:01，由通关开出），game 的循环 23:15:03.6 才轮到它；A、A2 全部成功，没有 `clear not contributed`。

结论：Cluster 下 §8.2 六步与 1c、6、6a、6b、6c 全部符合预期，等待时长、fail-stop 时延、断开连接、转交与接手都与单机一致；App 锁方案 §13 第 5 笔的“Redis Cluster 下的真实进程演练未验证”关闭（多机 Cluster 切主仍属外部验证 E08，不在本机范围）。

- 环境说明（本 agent 的操作失误，不是缺陷）：第一轮之前有两次尝试只用 `mongosh` 删了 DAO 库与 saga 库，没删 `remote_entity` 库与 Redis 里的 L2 快照，公会 ID 分配器随 DAO 库重置后重发了旧 ID，`found_guild` 报 `state version conflict`（loaded 0 / authority 1、`L2 same version has different content`）。之后整套 teardown + setup 重来，记录里的两轮都在全新环境上跑。

## 验证（`GOWORK=off`）

- `gofmt -l` 空；`go vet ./nats/driver/ ./kit/nats/ ./kit/mongo/ ./app/ ./lifecycle/` 与 `-tags integration` 同包 vet 通过。
- `go test -race -count=3 ./nats/driver/ ./kit/nats/ ./kit/mongo/ ./app/ ./lifecycle/` 通过。
- integration 用例在私有真实依赖上 `-race -count=3` 通过（上一节）。
- 相邻包 `go test -count=1 ./nats/... ./bus/... ./kit/nats/... ./kit/mongo/... ./lifecycle/... ./kit/ops/... ./kit/configdata/...` 通过。
- 根包 `go test -count=1 .` 通过（加 `./kit/mongo` 之前红在故障矩阵门禁，见上）；`go build ./... && go vet ./...` 通过。
- 生成工程（replace 到本检出）`go build` 通过，即演练用的 `bin/app`；生成形状未改，没有重跑 codegen 测试。

drill3（⑤ ⑥，RR-20261006-27～29）：

- `gofmt -l` 空；`go vet ./metrics/ ./robot/... ./app/ ./kit/mods/ ./codegen/internal/roost/` 通过。
- `go test -race -count=3 ./metrics/ ./robot/... ./app/ ./kit/mods/ ./kit/redis/` 通过。
- 生成模板改了（`cmd/loadtest`、`cmd/accountctl`、`deploy/dev/run.sh`、`internal/service/game/activity.go` 与测试）：`go test -count=1 ./codegen/...` 通过；`go generate ./...` 后 porcelain 只有本批改动；重新生成 game-demo（`roost project new d3demo -template game-demo`，replace 到本检出）`go build ./... && go vet ./... && go test -count=1 ./...` 通过，生成物里有 `-redis-cluster`、`snapshot.Error` 与 RR-29 的开窗。
- 根包 `go test -count=1 .` 通过；`go build ./... && go vet ./...` 通过。
- 真实进程：⑥ 两轮（Cluster），每轮 drill1 + drill1b 全部步骤。
