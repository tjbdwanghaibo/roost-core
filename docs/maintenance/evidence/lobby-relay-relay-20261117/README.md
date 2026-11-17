relay 组证据（2026-11-17，大厅 / relay 实施第一批：转发核心，不含换 host）
================================================================================

方案：docs/maintenance/FEATURE-2026-11-16-lobby-relay.md §4.4（以及 §4.3、§4.6.5、§4.7.1）。
现行契约：docs/framework/RELAY.md。

提交
----
- 7b145258 feat(relay)：relay 转发核心与 client/wire 的通道帧头
- f14b1a43 feat(relay)：wiring/relay 接线与 lobby 的报告 / 命令 / etcd 广播契约
- （本目录、RELAY.md、lobby 契约测试所在的提交见 git log -- docs/maintenance/evidence/lobby-relay-relay-20261117）

设计要点
--------
- 帧头：flags bits3..4 = relay 通道（01 游戏 / 10 房间 / 11 控制），relay 帧上 seq = 槽位（上行目标、下行发送者）。
  Gate 的 wire.Validate/Read/Write 不变、仍拒绝通道位；relay 用 wire.*Relay。packets.json 没改，新增 relay-packets.json。
- 每条连接：读 goroutine（令牌、限额、按通道交给房间）+ 写 goroutine（有界队列，writev 整批写；满了关这一条）。
- 房间状态 room.mu 保护；锁序 Server.mu → room.mu → conn.mu；报告在放锁后发。
- 房主断开即关房；room.hostLostLocked 是下一批接换 host 的位置。

测试（只跑本组包的快速检查）
----------------------------
    go vet ./client/wire ./service/relay/... ./wiring/relay                → vet.txt：ok
    go test -race -count=3 -v ./client/wire ./service/relay/... ./wiring/relay → tests-race-count3.txt：96 PASS，0 FAIL

覆盖方案 §5 relay 条目（换 host 除外）：
- 转发方向：TestGameChannelDirection（成员游戏帧只到房主、房主到单个 / 全体、不在线目标丢弃）
- 房间通道直达：TestRoomChannelDirect；单独限额：TestRoomChannelHasItsOwnLimit（帧数与字节数，超出丢帧不掉线，游戏帧照常）
- 禁言：TestMuteBlocksOnlyTheRoomChannel（只挡房间通道；顶号后仍禁言；成员发 Mute 无效；解除）
- 槽位、踢人、同账号顶号：TestSlotsKickAndReplace（含房主顶号、被踢账号不能再进）
- 角色以房间状态为准：TestRoleFollowsTheRoomNotTheToken
- 超大帧、超速、空闲超时、心跳保活、超长 Ping：TestConnectionLimits
- 发送队列满断开慢连接、不阻塞别人：TestSlowConsumerIsDisconnectedWithoutBlockingOthers（内存 net.Pipe 的 Listener，
  同时证明 Server.Serve 接受任意 net.Listener）
- 房主断开后关房：TestHostDisconnectClosesTheRoom；生命周期与报告：TestRoomLifecycleAndReports（成员先到、满员、
  关过的房间号不能用旧 host 令牌重建、别的实例的令牌、坏首帧）；空房超时与心跳：TestHeartbeatAndEmptyRoomTimeout
- lobby leave 命令：TestLobbyLeaveCommand；停机：TestShutdownClosesEveryConnection
- 帧头：TestRelayChannelBitsAreRejectedByTheGateCodec、TestRelayHeaderValidation、TestRelayControlPayloads、
  TestRelayProtocolGolden；原有 TestClientProtocolGolden（packets.json）不变、仍通过
- 接线：TestModInitReadsConfigAndKey、TestModLoadsWhatItDeclares（每个声明的键写错都被点名；与 App 声明合并不冲突）、
  TestServerReportsAdvertisesAndTakesLobbyCommands（Mod→Server 正式链路：TCP 进房、报告主题与内容、leave 命令、
  etcd 广播随房间数更新、租约丢了重新登记、停机撤销与命令订阅排空）、TestRelayMatchesTheLobbyContract（主题、事件名、
  命令、JSON 字段与 service/lobby 钉在一起）
- 另跑过：go test ./wiring -run 'TestKitModsDoNotReachForRawDriverHandles|TestKitDependencyBoundary|TestForbiddenKitImport|
  TestBuiltInModsImplementContextStop|TestEveryModDependencyNamesAKitMod' → ok；go test . -run TestTrackedMarkdownRelativeLinksResolve → ok

变异
----
mutations.txt：15 条关键行各撤一次，目标用例全部变红，恢复后全部变绿（脚本 mutate.py.txt）。

真实依赖
--------
    source ~/.roost-it/roost-dataengine-it/env.sh && go test -tags integration -race -count=1 -run TestRealRelay -v ./wiring/relay/
    → real-nats-etcd.txt：PASS
私有环境的 NATS 集群 + 用例自己起的单节点 etcd（结束即杀），正式的 NATS Mod、etcd Mod、relay Mod 与 Server：
TCP 转发、报告经真实 NATS 到 <subject>.events、leave 命令经真实 NATS 送达、etcd 广播随房间数更新、停机后键消失。
etcd Mod 自己的服务登记（/service/relay/9）与 relay 的广播键互不影响。

没有做的
--------
- 多进程演练（lobby + relay 真实进程、kill -9、两实例分布、吞吐基线）：按方案 §5 放到最后统一做。
- WebSocket 监听：wsconn（ws 组）在本组收尾时尚未提交，relay 侧没有接 relay.ws_listen（见 RELAY.md §7）。
- 换 host（下一批）。

给协调者：CHANGELOG [Unreleased] 建议文字
----------------------------------------
- 新增 relay 转发服（service/relay、wiring/relay，docs/framework/RELAY.md）：房主做 host 的开房间场景，relay 只读帧头、
  不解析载荷。游戏通道成员帧只到房主、房主按目标转出；房间通道（聊天 / 表情 / 语音）按目标直达、单独限额、可禁言；
  控制帧通知房主成员进出、房主踢人禁言；单帧、每秒帧数、发送队列、空闲与空房超时限额；房主断开即关房（换 host 下一批）。
  经 NATS 向 lobby 报告房间（<subject>.events），收 lobby 的 leave 命令，经 etcd 广播实例地址与房间数。
- client/wire：flags bits3..4 定为 relay 通道、relay 帧上 seq 是槽位；Gate 帧格式与 packets.json 不变，新增
  client/spec/relay-packets.json。

给协调者：KNOWN-LIMITS 建议文字（框架限制）
------------------------------------------
- relay 房间只在一个实例的内存里，进程崩溃房间丢失；本期房主断开即关房（换 host 未做）。
- relay 槽位只在在线期间属于账号，成员掉线重连可能换槽位（房主按 MemberJoined 里的账号对应）。
- 语音走 TCP / WebSocket，丢包会卡住后面的包。
- 尚未确认：真实 NATS / etcd 下的 relay 演练、WebSocket 接入（等 wsconn 提交后接线）。

给协调者：接线待办
------------------
- frameworkCatalog / 生成器：relay 是 app.Service（relay.NewServer()）+ relay.NewMod()，依赖 NATS、etcd；没有 ClientMod。
- wiring/config_schema_test.go 的 everyKitMod 建议加 new(relay.Mod)（本组没改这份共用测试，同样的检查在
  wiring/relay 的 TestModLoadsWhatItDeclares 里）。
