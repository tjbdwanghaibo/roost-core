host 组证据（2026-11-17，大厅 / relay 实施第二批：换 host；并入被踢记在 lobby、每条连接向 lobby 验权）
==========================================================================================

方案：docs/maintenance/FEATURE-2026-11-16-lobby-relay.md §4.6（4.6.1～4.6.6）、§4.7.4、§7 A / F / G / H；
被踢与验权：§4.3、§4.4、§6 第 11 条（bba60f19）。
现行契约：docs/framework/RELAY.md（§2.3 控制帧编码、§3 连接与验权、§4 限额、§4A 换 host、§5 与 lobby 的契约）、
docs/framework/LOBBY.md（§2 错误码 kicked、§2.1 被踢、§5.2 RoomEvent.reason 与验权）。

设计要点
--------
- 房间三种状态：有房主 / 宽限期 / 选人。房间的全部处理（连接进出、控制帧、宽限期与 host_ready 两个计时器）都在
  room.mu 内，计时器回调用代次核对，所以 §4.6.4 的步骤是单线程的。锁顺序仍是 Server.mu → room.mu → conn.mu，
  报告在放锁后发（room.do），放帧方从不等待；向 lobby 验权在不持任何锁时进行。
- 槽位改为属于账号（房间存续期间不变，被踢的删掉）：you_are_host 的成员名单能带离线成员；successors 按槽位声明不会
  因槽位复用指错人；“最小槽位 = 最早进房”成立。
- 报告里的 host / epoch 是最近一个已就绪的房主：选人期间仍报旧的，新房主回 host_ready 才报 host_changed（新 epoch），
  lobby 的 epoch CAS 不会被半途状态带偏。
- 房主断线的发现：房主连接单独一个空闲阈值 relay.host_idle_timeout（缺省 2s），成员仍是 idle_timeout（30s）。
- 快照：只认当前房主；seq 房间内递增（跨房主）；同一房主最小间隔；snapshot_max_bytes 以内收下，超过但不到两倍丢载荷
  回 Rejected{too_big}（连接保留），超过两倍按超大帧断开；快照帧不受 max_payload_bytes 限制（读帧先看帧头），
  WebSocket 单条消息上限相应放大（Config.MaxFrameBytes）。
- 验权：令牌本地验签 → 本地预检（房间不在且非 host 令牌、关过的房间号、本地被踢）→ NATS request <subject>.verify →
  进房。lobby 拒绝 Closed{lobby_rejected}(15)；出错 / 超时 / 无应答 Closed{lobby_unavailable}(16)，失败即关闭。
- 被踢：relay 的 member_left 报告原因 kicked → lobby 记进 Room.Kicked（随记录过期）、让出席位、之后 join 答 403 kicked，
  验权答 kicked；relay 在线名单里再出现也不补回席位。

测试（只跑本组包的快速检查）
----------------------------
    vet.txt：go vet（含 -tags integration）→ ok
    tests-race-count3.txt：ROOST_REDIS_TEST_ADDR=<共享隔离环境 Redis> go test -race -count=3 -v
        ./client/wire ./service/relay/... ./service/lobby/... ./wiring/relay ./wiring/lobby → 全部 ok，0 FAIL
    real-nats-etcd.txt：source ~/.roost-it/roost-dataengine-it/env.sh && go test -tags integration -race -count=1
        -run TestRealRelay -v ./wiring/relay/ → 2 条 PASS

方案 §5 换 host 条目 → 用例（service/relay/host_test.go，另有 WebSocket 与集成）：
- 主动转交、交接数据送达、旧房主降级 you_are_member、迁移期间成员帧丢弃并收到一次 host_changing、旧房主的房主帧
  被拒、host_ready 后全体 host_changed 与 lobby 报告、旧 host 令牌重连只是成员：TestTransferHandsTheRoomToTheTarget
- 转交被拒（目标不在线 / 自己 / 非房主）：TestTransferRejections
- 宽限期内重连仍是房主（host_resumed、成员名单补发、epoch 不变）、宽限期内经 lobby leave 立即换：TestHostReturnsWithinGrace
- 宽限期满按候选顺序、快照 seq 与年龄交给新房主、新房主从旧 seq 往上接：TestGraceExpiresAndSuccessorsAreHonoured
- 候选不在线取最小槽位、离线座位进成员名单、Leave 控制帧不等宽限期：TestCandidateFallsBackToTheSmallestOnlineSlot
- host_ready 超时换下一个、候选断开换下一个、都失败关房、迟到的 host_ready 无效、选人期间报告仍是旧 epoch：
  TestHostReadyTimeoutMovesOnAndAllFailingCloses
- 房主心跳超时进宽限期：TestHostHeartbeatTimeoutStartsTheGrace
- 快照的间隔、大小（拒收不断开 / 两倍以上断开）、seq、只留最新一份、成员快照被拒、转交数据超限：TestSnapshotLimits
- 候选刷新页面（顶号）重新收到 you_are_host：TestCandidateReconnectGetsTheOfferAgain
- 没有成员时房主断开就关房：TestHostDisconnectWithoutMembersClosesTheRoom
- WebSocket：TestWebSocketHostChange（浏览器房主推 100 KiB 快照、刷新后宽限期内回来、再断开由 TCP 成员拿快照接手、
  再主动转交回浏览器）
- lobby 的 epoch CAS（乱序只收更大的）：TestHostChangedAcceptsOnlyLargerEpochs（lobby 组已有，本组用变异 H20 证明）
- 真实 NATS + etcd（+ 真实 Redis）：TestRealRelayHostChangeKickAndVerification（转交后 lobby 改房主、old host 改状态被拒、
  踢人进 lobby 名单、join 被拒、旧令牌被拒、lobby 不应答时新连接被拒而已连上的不受影响）
- 协议：TestRelayHostChangePayloads、TestRelayProtocolGolden（relay-packets.json 只追加 13 条，旧条目未改）

被踢与验权 → 用例：
- 被踢账号 join 被拒（含迟到报告、不被在线名单补回）：TestKickedAccountsAreRecordedAndRefused、TestHTTPRoomLifecycle（403 kicked）
- lobby 验权各原因：TestRelayVerification；wiring/lobby 订阅并答复：TestModAndServerOnRealRedis
- relay 每条连接验权、lobby 拒绝 → lobby_rejected、lobby 不应答 → 到 admit_timeout 后 lobby_unavailable、建房也验：
  TestEveryConnectionIsCheckedWithTheLobby；NATS 适配（主题、字段、unavailable 与无应答）：TestServerReportsAdvertisesAndTakesLobbyCommands
- 旧令牌在 relay 本地记录已清掉时仍被拒（relay 进程“kill -9”后重启，房间与本地被踢记录都没了，拒绝只能来自 lobby）：
  TestKickIsRecordedInTheLobbyAndEnforcedOnEveryConnection（进程内 lobby.Service + relay，线上 JSON 编码）
- 契约：TestRelayMatchesTheLobbyContract（verify 主题、请求 / 答复字段、unavailable 原因、kicked 原因名、host_changed、reason 字段）

第一批用例随语义调整（不是放宽）：TestSlotsKickAndReplace（槽位属于账号：新账号取 4，回来的 a 拿回 2）、
TestRoomLifecycleAndReports / TestHostDisconnect…（房主断开不再立即关房，改测“没有成员时关房”）、TestConnectionLimits
（槽位号随之后移）、TestLobbyLeaveCommand（房主被 lobby 移出改为立即换 host）。

变异
----
mutations.txt：30 条（脚本 mutate.py.txt），每条撤 / 改一行关键代码，目标用例全部变红（没有编译失败的红），恢复后全部变绿。
覆盖：epoch 加 1（H1）、宽限期（H2、H3、H27）、候选顺序与兜底（H4、H5）、超时换人与都失败关房（H6、H7）、迁移期间丢帧
与 host_changing（H8～H10）、旧房主降级（H11、H11b、H12）、快照大小 / 间隔 / seq / 存下（H13～H16）、报告 epoch（H17）、
host_changed 报告（H18）、房主心跳超时（H19）、lobby CAS（H20）、lobby 被踢检查（H21 join、H22 验权、H23 记录）、
relay 验权调用（H24）、超时失败即关闭（H25）、unavailable 答复（H26）、WebSocket 消息上限（H28）、lobby 答复验权（H29）。
H11 第一次选的“去掉 r.host = nil”没有变红：选人期间的转发、踢人、快照都先看房间状态，那一行只是防御；改用 H11 / H11b
（按建房槽位认房主）证明降级由 room.host 与状态共同保证。

没有做的
--------
- codegen：kitconfig_gen.go 未重生成（TestKitConfigSchemasMatchKitDeclarations 现在红，由协调者重生成）。
- 根包 TestTrackedMarkdownRelativeLinksResolve 现在红在 docs/framework/GATEWAY.md:144（别组的证据链接），不是本组文件。
- 多进程演练（kill -9 房主客户端、两 relay 实例）、浏览器演练：按方案 §5 放到最后统一做。

给协调者：CHANGELOG [Unreleased] 建议文字
----------------------------------------
- relay 换 host（方案 §4.6）：房主主动转交（transfer，带交接数据）、离开（leave 控制帧或 lobby 的 leave）立即换；房主断线
  （房主连接单独的心跳超时 relay.host_idle_timeout，缺省 2s）等 relay.host_grace（缺省 5s），期间同一账号回来仍是房主
  （host_resumed）；期满按房主声明的候选顺序（successors）、兜底最小槽位选人，新房主在 relay.host_ready_timeout（缺省 10s）
  内回 host_ready 才生效，超时或断开换下一个，都失败关房。迁移期间成员发给房主的游戏帧丢弃并收到一次 host_changing。
  relay 存房主推来的最新快照（seq 递增、relay.snapshot_min_interval 1s、relay.snapshot_max_bytes 256 KiB，超限拒收不断开），
  换 host 时连同 seq 与年龄交给新房主。新控制帧 64～69、80～84（client/spec/relay-packets.json 追加）。
- 行为变化：房主断开不再立即关房（没有在线成员时仍立即关）；槽位改为房间存续期间属于账号（重连拿回原槽位，新账号取
  下一个没用过的槽位）；Welcome 里的房主槽位换 host 后不再是 1。
- 被踢记在 lobby、每条连接向 lobby 验权（方案 §6 第 11 条）：relay 本地验签后经 NATS request <subject>.verify 问 lobby
  （房间在、分在本实例、是成员、没被踢；建房时是房主），拒绝 Closed{lobby_rejected}，超时 / 不可用 Closed{lobby_unavailable}
  （relay.admit_timeout 缺省 2s，失败即关闭）。lobby 按 member_left{kicked} 记被踢名单（随房间记录过期），join 答 403 kicked。
  升级：relay 与 lobby 必须一起升级（旧 lobby 不答 verify，新 relay 会拒绝全部连接）。
- 新配置键：relay.host_idle_timeout、relay.host_grace、relay.host_ready_timeout、relay.snapshot_min_interval、
  relay.snapshot_max_bytes、relay.admit_timeout；relay.ws_allowed_origins 的 example 改为 https://play.example.com。

给协调者：KNOWN-LIMITS 建议（框架限制，不是遗留）
------------------------------------------------
- lobby 不可用时 relay 新连接与重连都进不来（方案 §4.3 的取舍），已连上的不受影响。
- relay 的座位表（账号 → 槽位）在房间存续期间只增不减（被踢的除外），由 lobby 的席位与令牌限住规模。
