# MOBA / 竞技对战（帧同步）

手册：`docs/framework/MOBA-INTEGRATION.md`（下称 MOBA §N）。路径相对 roost-core 根目录，以检出或 module cache 中的 `docs/` 为准。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 帧同步对局：1v1 单路入门到 5v5 三路；匹配成局、断线追帧 / 快照恢复、服务器参考模拟判胜负、幂等结算、持久对局续赛、宿主代际交接、原服逐人奖励 | 需要隐藏信息（战争迷雾）的 MOBA：要服务端状态同步，见 SKILL.md §6“MOBA 状态同步 / RTS”暂缓；格斗 / 射击类 rollback netcode（框架无）；对局权威自动 HA（MOBA §7 末行） |

核心区分：**局内状态**由服务器参考模拟与客户端按同一输入序列各自计算，不写 DAO；**局外事实**（结算身份、奖励、战绩）经 Nest 多实体事务写 DAO（MOBA §1）。

## 2. 用到的框架模块

| 业务 | 模块 | 出处 |
| --- | --- | --- |
| 排队、成局、跨分区成局 | `service/match`（Enqueue / Commit / CommitAcross） | MOBA §2、§5.1 |
| 席位、输入排序、切帧、追帧 | `framework/sync/lockstep`：`Match`、`Sequencer`；客户端 `FrameAssembler` | MOBA §2；guide/04-sync.md §6 |
| 窗口外恢复 | `MatchConfig.Recovery` + `SnapshotProvider`（服务器参考模拟的检查点） | MOBA §6；guide/04-sync.md §6 |
| 结算、战绩、奖励 | Player / MatchResult DAO + 多实体 Sender + Nest | MOBA §2、§5.4 |
| 持久对局、比赛目录 | BattleRound / BattleDirectory 实体（World 锁档），先提交再广播 | MOBA §5.5、§5.6 |
| 传输 | 嵌入 TCP `lockstep.TCPSender`；KCP 只生成装配（`-features nettransport-kcp`） | MOBA §6；`roost help replication` |

## 3. 生成最小 demo

接在 SKILL.md §3.2 公共前置命令之后。对应 `scripts/test-moba-generated.sh` 的缺省模式（双英雄入门 + 5v5 夹具一起拷，测试互相依赖，不拆）。

```bash
"$ROOST" add dao Player --entity Player
for e in MatchResult BattleRound BattleDirectory; do
  "$ROOST" add entity "$e"
  "$ROOST" add dao "$e" --entity "$e"
done
dao_add Player db/def/player.go "Gold int64 gold" "Wins int64 wins" "Losses int64 losses" "MobaRewards string mobarewards"
dao_add BattleDirectory db/def/battle_directory.go "Payload string payload"
dao_add BattleRound db/def/battle_round.go "Payload string payload"
dao_add MatchResult db/def/match_result.go "MatchKey string matchkey" "Digest string digest" \
  "WinnerID int64 winnerid" "LoserID int64 loserid" "Status int64 status"
world game/entities/match_result/entity.go game/entities/battle_round/entity.go game/entities/battle_directory/entity.go
F="$CORE/scripts/testdata/moba"
mkdir -p game/moba
for n in simulation simulation_test network_test matchmaking_test arena round room directory arena_test arena_network_test; do
  cp "$F/$n.go.txt" "game/moba/$n.go"
done
cp "$F/round_handlers.go.txt"      game/handler/moba_round.go
cp "$F/round_handlers_test.go.txt" game/handler/moba_round_test.go
cp "$F/settlement.go.txt"          game/handler/moba_settlement.go
cp "$F/settlement_test.go.txt"     game/handler/moba_settlement_test.go
fix_mod moba-practice game/moba/*.go game/handler/moba_*.go
gofmt -w db/def/*.go game/moba/*.go game/handler/moba_*.go
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMOBA' ./game/moba ./game/handler 2>&1 | tee "$WORK/test.log"
```

期望：`--- PASS: TestMOBA` 恰好 **28** 条、0 skip（含 4 条真实 loopback KCP）；然后按 SKILL.md §3.4 收尾。bson 键沿用脚本的“字段名小写”写法。

可选：需要 KCP / UDP 装配函数时，新建工程加 `-features protocol,config,entity,nest,dao,errcode,nettransport-kcp`（生成 `internal/transport/generated.go`；监听、握手鉴权、Session 绑定仍手写，`roost help replication`）。本 skill 未把它和上面的夹具组合实测。

## 4. demo 的最小玩法范围（MOBA §4.4）

| 层 | 场景（28 条） | 证明 |
| --- | --- | --- |
| 双英雄单路入门（12） | 结算并发重放 / 身份冲突 / 拒绝回滚 / 无胜者；会话身份、输入窗口与重放、冗余去重、断线追帧、旁观、历史过期、`NoMajority`；match 内存存储排队 / 取消 / 过期 / 并发 Commit 幂等；一条真实 KCP 丢包追帧 | MOBA §10 第一条 |
| 5v5 三路（16） | 十人成局与准备；日志重算拒绝被改快照 / 规则版本 / 种子；十个 KCP 客户端追帧与超窗快照恢复；先提交后广播、提交未知停房间、`resume` / `handoff` 代际隔离、目录并发只分配一局、奖励部分投递重放 | MOBA §10 第二条 |

最小可讲的玩法：两名英雄单路对推 → 服务器参考模拟判胜负 → 一次多实体结算发奖；5v5、ban/pick、持久续赛是进阶。

## 5. 业务限制清单

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 输入准入与按 seat 排序、冗余广播、窗口内追帧、窗口外经业务快照恢复、`ReportHash` 发现分歧、匹配幂等与跨分区原子成局、嵌入 TCP 追帧随链路放慢 | MOBA §2、§6 |
| 框架不能做 | 确定性模拟本身（整数、固定 seat 序、逻辑帧由业务保证）；活动对局持久化的默认实现（5v5 夹具自己提交输入帧日志）；KCP 监听 / 鉴权 / Session 绑定；权威 Round 的选主或自动接管；反作弊（hash 一致不证明没作弊） | MOBA §1、§6、§7 |
| 网络 / 恢复边界 | 历史窗口缺省 3600 帧（60Hz 下 60 秒），越界未配快照来源即 `ErrCatchupUnservable` 终止本场；输入去重只在 `ReplayHorizon` 内；独立 Gate 下追帧与快照不随链路放慢，慢链路大快照会被断开并从头重传（缺省 16 KiB/Tick 节流）；已提交门要求宿主提交落后切帧 ≤ `RedundancyDepth - 1` | MOBA §6、§10；KNOWN-LIMITS“Lockstep”“Lockstep 追帧链路背压（独立 Gate）”；docs/performance/README.md 的历史快照（LOCKSTEP-SNAPSHOT-DRILL-20261020） |
| 容量边界 | 无 5v5 目标负载、Tick 预算、分位延迟、移动网络、跨数据中心数据；本机 KCP / TCP 回归不代表这些；Tick 不能放进 Nest worker（`TCPSender` 发送前 `fctx.BlockingError` 守卫） | MOBA §6、§13；guide/04-sync.md §6“经嵌入 TCP 追帧” |
| 一致性边界 | 胜负与摘要只取服务器参考模拟；结算 ID 开局分配并保留，换 ID 就绕开幂等；匹配记录只保留 `match.retention`，开局后 Match→Round 路由由业务目录保存；跨服奖励各原服逐人提交，不是十人原子到账 | MOBA §7 |
| 业务要自己补 | 英雄 / 技能 / 兵线 / 装备、ban/pick 与准备政策、队伍与 MMR、输入 codec、快照 Provider、长局分段日志 / 归档、常驻奖励投递、权威 HA | MOBA §8、§13 |

## 6. 常见坑（MOBA §7）

- 每帧把局内状态写 DAO：持久的应是结算事实或输入日志。
- 客户端上报胜负 / hash 一致就发奖。
- 放宽匹配对两个分区各 `Commit` 一半：失败留半局，用一次 `CommitAcross`。
- 以为 `Tick` 报错就是没切帧：只有 `ErrMatchClosed` 不切帧，其他错误帧已在历史里。
- `Match.Tick` 切出帧就广播：需要续赛时先提交到权威、成功后再发，提交结果未知就停房间。
- 宿主 SID 相同就当同一宿主：重启要 `resume` 推进 Epoch。
- 以为用了 KCP 就有了玩家接入与鉴权：接入层只提供 TCP（GATEWAY.md §4）。

## 7. 验证

- 一档：§3 的 28 条 PASS + SKILL.md §3.4。2026-10-31 在 v1.33.0 检出、自定 module `example.com/mymoba` 实测通过（约 11s，构建缓存热）。2026-10-31 在 v1.33.2 检出（`e4ec6260`）复测：28 PASS、0 SKIP，§3.4 全过，doctor 仅 4 项 collaborators FAIL。
- 框架自带网络门禁（在 roost-core 根目录）：`GOWORK=off go test -race -count=1 ./framework/sync/lockstep ./framework/sync/nettransport`（MOBA §12）。
- 二档：`ROOST_MOBA_REAL=1`（隔离 Mongo 副本集）追加两进程结算恢复、活动对局 SIGKILL 续赛、SIGKILL 后窗口外快照恢复；再加 `ROOST_MOBA_ROUTING=1`（+ NATS）追加五进程宿主交接与奖励重投（MOBA §9、§11）。
- 不证明：对客户端广播与真实鉴权的端到端链路、权威 HA、tick 预算、5v5 目标负载与跨机（KNOWN-LIMITS“游戏接入验收”MOBA 行）。

## 8. 手册指针

- `docs/framework/MOBA-INTEGRATION.md`：§2 状态归谁、§4 纵切与阅读顺序、§5 一场对局怎样连接模块、§6 网络恢复反作弊边界、§7 易混边界、§9～§13 验收
- `docs/framework/guide/04-sync.md` §6 Lockstep（历史窗口、追帧 / 快照、嵌入 TCP）
- `docs/framework/guide/09-services.md` §4（match 保留期与分区容量）
- `docs/maintenance/KNOWN-LIMITS.md`“Lockstep”与“游戏接入验收”MOBA 行
- 参考实现：game-demo 的 battle 房间（嵌入 TCP lockstep，`demo/internal/service/game/battle.go.tmpl`）
