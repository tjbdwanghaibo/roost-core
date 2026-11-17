seat 组证据（2026-11-17，大厅 / relay 实施：开局后掉线成员的席位）
==================================================================

依据：docs/maintenance/FEATURE-2026-11-16-lobby-relay.md §6 第 12 条（20d58b3a）。
现行契约：docs/framework/LOBBY.md §2.1（席位、离开）、§5.2（member_left 的 kicked / left、验权 not_member）。

设计
----
- 一条席位规则 Room.holdsSeat（service/lobby/lobby.go），join（pruneReservations）、relay 报告（applyOnline）、
  验权（VerifyRelayConnection → pruneReservations）都按它算：
  在线 || 房主 || 预留期内 || (房间 started && Connected)。
- Member 新增 Connected（JSON connected）：relay 报告过他在线就置上，掉线不清。只有它让已开始房间里的掉线席位一直留着。
- 主动离开清掉 Connected，席位回到“没开局”的规则：
  - 成员调 lobby leave：原来就直接移出席位（不变）；
  - 成员在 relay 上发 RelayOpLeave：relay 报 member_left{reason:left}，lobby 清 Connected（只认 seq 更大的报告，
    迟到的旧 left 不清回座后又连上的人）；
  - 房主调 lobby leave：清 Connected（席位仍按房主规则留到 relay 报 host_changed），换完 host 后让出。
- 被踢（member_left{kicked}，任何账号、与 seq 无关）与关房（room.closed）沿用原逻辑，已覆盖。
- 回座：JoinRoom 的“已在房间里”分支原本就在 started / 密码 / 人数检查之前，席位留着就自然回到原座、签新令牌，不算新人。

改了哪些文件
------------
- service/lobby/lobby.go：StateStarted 注释、Member.Connected、Room.holdsSeat、pruneReservations 改用它。
- service/lobby/events.go：ReasonLeft、member_left{left} 清 Connected、applyOnline 记 Connected 并改用 holdsSeat。
- service/lobby/rooms.go：JoinRoom 注释；LeaveRoom 房主分支清 Connected。
- service/lobby/lobby_test.go：4 条新用例（见下）。
- wiring/relay/real_integration_test.go：新用例 TestRealRelayStartedRoomKeepsADroppedMembersSeat（只加用例）。
- docs/framework/LOBBY.md：§2.1 席位与离开、§5.2 报告处理与验权表。

测试
----
新用例（service/lobby，内存 + 真实 Redis 两种目录）：
- TestStartedRoomsKeepDroppedMembersSeats：开局后掉线、过了预留期席位仍在、验权 OK；新人 room_started；
  掉线者在 2/2 满员的已开始房间回座成功、新令牌、人数不变；回座后令牌再过期仍占座；重新连上。
- TestOpenRoomsReleaseDroppedMembersSeats：open 房间掉线并过预留期让出、验权 not_member、别人能进；started 改回 open
  后掉线的席位让出。
- TestStartingARoomDoesNotKeepSeatsNeverConnected：开局时预留期内、从没连上的人，预留过期让出，之后 join 答 room_started。
- TestStartedRoomsReleaseSeatsOnKickLeaveAndClose：掉线成员被踢（join 答 kicked、席位空出）、lobby leave、relay left
  （含迟到的旧 left 不生效）、房主 leave 后换 host 让出、关房后 join 404 / 验权 room_not_found。
真实依赖（共享隔离环境的 NATS + Redis，用例自起 etcd）：
- TestRealRelayStartedRoomKeepsADroppedMembersSeat：开局后 alice 断开 → relay 报 member_left → lobby 时钟拨过预留期 →
  新人 room_started → alice 回座拿新令牌 → relay 向 lobby 验权通过、Welcome、房主收到 member_joined。

文件：
- red-before.txt：新用例在改动前代码上（只加了 ReasonLeft 常量垫片）的失败原文：3 条红；
  TestStartingARoomDoesNotKeepSeatsNeverConnected 在旧代码上也绿（它守的是新规则的边界，旧规则本来就让出）。
- mutations.txt：8 处变异，全部变红后恢复：
  M1 开局保留席位（holdsSeat 去掉 started && Connected）、M2 回座不受 started 限制（started 检查挪到成员分支前）、
  M3 回座不算新人（人数检查挪到成员分支前）、M4 applyOnline 不记 Connected、M5 relay left 不清 Connected、
  M6 房主 lobby leave 不清 Connected、M7 left 不看序号、M8 开局后一律保留（不看 Connected）。
- mutations-integration.txt：真实依赖用例上的 IT-M1（同 M1，红在“alice 掉线后席位还在”）、IT-M3（同 M3）与恢复后绿。
- tests-race-count3.txt：service/lobby、wiring/lobby、wiring/relay 的 -race -count=3（含 Redis 子用例计数）、
  wiring/relay 集成用例 -race -count=3。
- vet.txt：go vet（含 -tags integration）。
- probe-offline-kick.txt：临时探针（未提交）：relay 踢不动离线槽位，见“拿不准”第 1 条。

拿不准
------
1. 【已定：协调者回复按 A 做，见文末“补充：踢离线座位”】relay 的踢人只作用于在线连接（service/relay/room.go 的 kick 用
   r.members[slot]，只含在线连接），房主踢掉线成员的离线槽位是空操作，relay 不报 member_left{kicked}（probe-offline-kick.txt
   实测）。所以“掉线不回来的人要房主踢掉才空出来”目前端到端做不到：这个席位要等房间关闭。lobby 侧已经就绪
   （member_left{kicked} 对任何非房主账号都记名单、让出席位，TestStartedRoomsReleaseSeatsOnKickLeaveAndClose 覆盖）。
   - A（推荐）：relay 的 kick 对 r.seats 里有座位、但不在线的账号也生效：记 kicked、删座位、报 member_left{kicked}
     （service/relay/room.go 几行，归 relay/host 组）。房主从 you_are_host 的成员名单与 member_left 拿得到离线槽位。
   - B：lobby 加 POST /rooms/{id}/kick（房主），直接记名单并发 relay 命令。多一个对外接口和一条命令，与“踢人是 relay
     控制帧”的方案不一致。
2. open → started 那一刻还在预留期、但从没连上 relay 的人：按“掉线成员”只指连上过的人处理——不永久保留，预留期内连上
   就转为永久保留，过期没连上就让出，之后再 join 是新人（room_started）。理由：relay 上没有他的座位，即使按 1-A 改了
   也踢不到他，永久保留就永远空不出来。代价：开局后才姗姗来迟、令牌又过期的人进不来了（他在预留期内重新 join 仍可续）。
3. started 改回 open：席位规则跟着回到“没开局”，掉线的人下一次报告 / join 时让出。方案没写，按“没开局沿用原规则”字面处理。
4. relay 上的 RelayOpLeave（不经 lobby）算主动离开：lobby 认 member_left{left}。只认 seq 更大的报告，这条报告丢了时退化成
   “掉线”（席位留着）；Go SDK 的 Leave 走 lobby leave，不受影响。
5. 房主主动离开（lobby leave）在已开始的房间里：换完 host 后他的席位让出。如果他之后想回来，是新人（room_started）。
   房主断线（非主动）换 host 后，他仍按掉线成员保留席位、能回座（拿 member 令牌）。
6. 任务说明里提到的 host 组 README“拿不准”第 8 条，在 docs/maintenance/evidence/lobby-relay-host-20261117/README.md 里
   没有找到（该文件没有“拿不准”一节），可能只在 host 组的报告里；本组按方案 §6 第 12 条原文做。

给协调者：CHANGELOG [Unreleased] 建议文字
----------------------------------------
- lobby 席位（方案 §6 第 12 条）：房间已开始时，连上过 relay 的成员掉线后席位一直保留，直到他主动离开（lobby leave、
  relay 的 leave 控制帧）、被踢或房间关闭；他再 join 回到原座（不受“已开始”与人数限制，签新令牌，relay 验权通过）。
  没开局的房间沿用原规则。开局时从没连上的人仍按预留期让出。房间记录的成员多一个字段 connected（线上尚未部署，不做旧记录兼容）。
- lobby 认 relay 报告 member_left 的原因 left（主动离开）。

给协调者：KNOWN-LIMITS 建议
--------------------------
- 无（“拿不准”第 1 条的离线踢人已按 A 修掉，见下）。

补充：踢离线座位（协调者按“拿不准”第 1 条 A 授权，2026-11-17）
--------------------------------------------------------------
- service/relay/room.go kick：槽位上没有在线连接时，在 r.seats 里找这个槽位的账号：记本地被踢、删座位、
  报 member_left{kicked}（Slot、Account），房主收到 MemberLeft{kicked} 作为确认。在线的情况不变。槽位上没有座位
  （没人用过、已经踢过）时什么也不做。
- 换 host 不会选到被踢的人：现有逻辑已经保证——pickCandidateLocked 只从 r.members（在线连接）里选，座位删掉后
  you_are_host 的成员名单（seatsLocked 读 r.seats）不含它；槽位不复用、被踢账号进不来，successors 里留着这个槽位
  也永远选不到。没有改换 host 的代码。
- 测试：
  - service/relay TestKickingAnOfflineSeat：successors [2,4]，2 掉线后被踢 → 房主收到 MemberLeft{kicked}、报告
    member_left{kicked}、它再来被拒、再踢一次无事发生；房主离开后 you_are_host 给 4（不是 2），成员名单
    "host@1:off acc-b@3:on acc-c@4:on" 不含被踢的。
  - wiring/relay TestRealRelayStartedRoomKeepsADroppedMembersSeat 续了一段：alice 回座后又掉线，房主踢她的离线槽位 2
    → lobby 记进 Kicked、席位让出（1 人）→ join 答 kicked → 改回 open 后 bob 进得来（上限 2）。
- 变异（mutations-kick.txt）：K1 撤掉“离线座位也能踢”（单测与真实依赖用例都红）、K2 不删座位、K3 不记本地被踢、
  K4 不报 lobby，全部变红后恢复。
- tests-kick-race-count3.txt：service/relay/...、wiring/relay -race -count=3，集成用例 -race -count=3，vet，根包文档链接。
- 文档：docs/framework/RELAY.md 控制帧表的 Kick、§3 第 7 步、§5 报告 reason（lobby 现在也读 left）。
- CHANGELOG 建议追加：relay 的踢人控制帧对有座位但不在线的槽位也生效（记被踢、删座位、报 member_left{kicked}，
  房主收到 MemberLeft{kicked} 确认）。
- 拿不准：离线踢人时给房主发一条 MemberLeft{kicked} 确认（在线踢人本来就发）。协调者的要求只列了三件事，这一条是
  为了让房主一侧（SDK）知道座位已删；如果不要，删掉 kick 里那一行 enqueueControl，并把单测与集成用例里等这条确认
  的断言改成等报告。
