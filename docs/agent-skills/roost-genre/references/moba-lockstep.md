# MOBA / 竞技对战（帧同步）

手册：`docs/framework/MOBA-INTEGRATION.md`（下称 MOBA §N）。命令对应当前版本的 `scripts/test-moba-generated.sh`；目标 roost-core 版本更早时读那个 tag 里的本文件。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 帧同步对局：1v1 单路到 5v5 三路；匹配成局、断线追帧 / 快照恢复、服务器参考模拟判胜负、幂等结算、持久对局续赛、宿主代际交接、原服逐人奖励 | 需要隐藏信息（战争迷雾）的 MOBA：要服务端状态同步，见 SKILL.md §6；格斗 / 射击类 rollback netcode（框架没有）；对局权威自动高可用 |

核心区分：**局内状态**由服务器参考模拟与客户端按同一输入序列各自计算，不写 DAO；**局外事实**（结算身份、奖励、战绩）经 Nest 多实体事务写 DAO（MOBA §1）。

## 2. 命令

接在 SKILL.md §3.2 公共前置命令之后。模块：`wallet`（胜者奖励 `Wallet().Add`，判重靠 MatchResult 终态与 Player 上的 `MobaRewards`）。排队成局用 `service/match`，席位与切帧用 `framework/sync/lockstep`，结算与战绩、持久对局、比赛目录都手写；双英雄入门与 5v5 夹具一起拷，测试互相依赖，不拆。

```bash
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" wallet
for e in MatchResult BattleRound BattleDirectory; do
  "$ROOST" add entity "$e"
  "$ROOST" add dao "$e" --entity "$e"
done
dao_add Player db/def/player.go "Wins int64 wins" "Losses int64 losses" "MobaRewards string mobarewards"
dao_add BattleDirectory db/def/battle_directory.go "Payload string payload"
dao_add BattleRound db/def/battle_round.go "Payload string payload"
dao_add MatchResult db/def/match_result.go "MatchKey string matchkey" "Digest string digest" \
  "WinnerID int64 winnerid" "LoserID int64 loserid" "Status int64 status"
world game/entities/match_result/entity.go game/entities/battle_round/entity.go game/entities/battle_directory/entity.go
F="$CORE/scripts/testdata/moba"
mkdir -p game/modules/moba loadtest protocol/def
# 公共 Robot：结果协议、LockstepBot 场景与 KCP 网络用例
cp "$F/result_protocol.go.txt"     protocol/def/result.go
cp "$F/robot.go.txt"               loadtest/battle.go
cp "$F/network_test.go.txt"        game/handler/moba_network_test.go
for n in simulation simulation_test matchmaking_test arena round room directory arena_test arena_network_test; do
  cp "$F/$n.go.txt" "game/modules/moba/$n.go"
done
cp "$F/round_handlers.go.txt"      game/handler/moba_round.go
cp "$F/round_handlers_test.go.txt" game/handler/moba_round_test.go
cp "$F/settlement.go.txt"          game/handler/moba_settlement.go
cp "$F/settlement_test.go.txt"     game/handler/moba_settlement_test.go
fix_mod moba-practice game/modules/moba/*.go game/handler/moba_*.go loadtest/battle.go protocol/def/result.go
gofmt -w loadtest/*.go db/def/*.go game/modules/moba/*.go game/handler/moba_*.go
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMOBA' ./game/modules/moba ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" wallet
```

然后按 SKILL.md §3.4 收尾。服务端初始化 / 结算 / 奖励接口不开放为玩家 PB；固定会话凭据只供验收。要 KCP / UDP 装配函数时新建工程加 `-features protocol,config,entity,nest,dao,errcode,nettransport-kcp`（监听、握手鉴权、Session 绑定仍手写，`roost help replication`）；它与上面夹具的组合没有实跑过。客户端一侧只有 Go 机器人有 KCP 拨号器，C# `LockstepClient` 与 C++ 的帧同步只走 TCP（[compose 客户端表](compose.md#客户端能连什么按客户端类型)）。

## 3. 期望场景数

`--- PASS: TestMOBA` 恰好 **29** 条、0 SKIP；`modules.sh verify` 打印 `wallet 17/17`。

| 层 | 场景 |
| --- | --- |
| 双英雄单路入门（12） | 结算并发重放 / 身份冲突 / 拒绝回滚 / 无胜者；会话身份、输入窗口与重放、冗余去重、断线追帧、旁观、历史过期、`NoMajority`；match 排队 / 取消 / 过期 / 并发 Commit 幂等；一条真实 KCP 丢包追帧 |
| 5v5 三路（17） | 十人成局与准备；日志重算拒绝被改快照 / 规则版本 / 种子；十个 KCP 客户端追帧与超窗快照恢复；先提交后广播、提交未知停房间、`resume` / `handoff` 代际隔离、目录并发只分配一局、奖励部分投递重放、到账回复在记录持久之后（`MOBAAward` 是 pipelined） |

29 条里有 4 条真实 loopback KCP，其中 `TestMOBAKCPRobotMatchSettlementAndClientResult` 用公共 LockstepBot 打完整局、经可信 Nest 幂等结算并向客户端发 PB 结果。

## 4. 限制

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 输入准入与按 seat 排序、冗余广播、窗口内追帧、窗口外经业务快照恢复、`ReportHash` 发现分歧、匹配幂等与跨分区原子成局 | MOBA §2、§6 |
| 框架不能做 | 确定性模拟本身（整数、固定 seat 序、逻辑帧由业务保证）；活动对局持久化的默认实现；KCP 监听 / 鉴权 / Session 绑定；权威 Round 的选主或自动接管；反作弊（hash 一致不证明没作弊） | MOBA §1、§6、§7 |
| 网络 / 恢复 | 历史窗口缺省 3600 帧（60Hz 下 60 秒），越界未配快照来源即 `ErrCatchupUnservable` 终止本场；输入去重只在 `ReplayHorizon` 内；独立 Gate 下追帧与快照不随链路放慢，慢链路大快照会被断开重传 | MOBA §6、§10；KNOWN-LIMITS“Lockstep” |
| 容量 | 没有 5v5 目标负载、Tick 预算、分位延迟、移动网络、跨数据中心数据；Tick 不能放进 Nest worker | MOBA §6、§13 |
| 一致性 | 胜负与摘要只取服务器参考模拟；结算 ID 开局分配并保留；匹配记录只保留 `match.retention`，开局后 Match → Round 路由由业务目录保存；跨服奖励各原服逐人提交，不是十人原子到账 | MOBA §7 |
| 业务要自己补 | 英雄 / 技能 / 兵线 / 装备、ban/pick 与准备政策、队伍与 MMR、输入编码、快照 Provider、长局分段日志、常驻奖励投递、权威高可用 | MOBA §8、§13 |
| 缺省模式不证明 | 对客户端广播与真实鉴权的端到端链路、权威高可用、tick 预算、5v5 目标负载与跨机。真实依赖档 `ROOST_MOBA_REAL=1`（隔离 Mongo）追加两进程结算恢复与 SIGKILL 续赛；再加 `ROOST_MOBA_ROUTING=1`（+ NATS）追加多进程宿主交接与奖励重投 | MOBA §9、§11 |

**常见坑**（MOBA §7）：

- 每帧把局内状态写 DAO：持久的应是结算事实或输入日志。
- 局外奖励改用 wallet 的 `Apply(业务键)`：回执窗口只有最近 128 次，原服重投可能晚于它；有永久事实时用 `Add`。
- 客户端上报胜负 / hash 一致就发奖。
- 放宽匹配对两个分区各 `Commit` 一半：失败留半局，用一次 `CommitAcross`。
- 以为 `Tick` 报错就是没切帧：只有 `ErrMatchClosed` 不切帧。
- `Match.Tick` 切出帧就广播：要续赛时先提交到权威、成功后再发，提交结果未知就停房间。
- 宿主 SID 相同就当同一宿主：重启要 `resume` 推进 Epoch。
- 以为用了 KCP 就有了玩家接入与鉴权。
