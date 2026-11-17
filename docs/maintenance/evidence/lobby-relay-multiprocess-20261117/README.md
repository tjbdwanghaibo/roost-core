# lobby + relay 多进程演练（2026-11-17）

目标：方案 [FEATURE-2026-11-16-lobby-relay](../../FEATURE-2026-11-16-lobby-relay.md) §5 的真实进程演练里还没做的四项：两个 relay 实例的房间分布、kill -9 房主客户端、kill -9 relay 进程、吞吐基线；顺带观察交接 A1（经 lobby 离开后迟到的在线名单不把人加回）。对错按现行契约 [LOBBY.md](../../../framework/LOBBY.md)、[RELAY.md](../../../framework/RELAY.md) 判断。

结论先列：

| 项 | 结论 |
| --- | --- |
| 1 两个 relay 分布 | 符合契约 §5.1：间隔 1.5s 建 6 个房间 3 / 3 交替；间隔 0 连建 6 个全部进 relay-1000（房间数每秒至多更新一次，只算房主已连上的房间）。契约 §7 只写了“几个 lobby 实例同时建房”，单个实例连续建房也一样，已补进 LOBBY.md §7 |
| 2 kill -9 房主客户端 | 符合契约：成员立即收到 `HostChanging`，宽限期 5s 后槽位最小的成员收到 `YouAreHost`（来源快照、seq 4、年龄 6.3s），回 `HostReady` 后全员 `HostChanged`，新房主从快照的 tick 接着广播，另一个成员继续收帧；lobby 的房主变成新房主、epoch 1，被杀的旧房主让出席位 |
| 3 kill -9 relay | **查出一个缺陷并已修复**：relay 崩溃后，客户端按 SDK 重连策略每次先向 lobby join，join 写记录、重置 Redis 键的 TTL，房间在目录里多活了 87 秒、一直列在大厅里（契约：最后一条报告之后 room_ttl 内消失）。修复后同一演练 33 秒内房间消失、客户端拿到 404 停止重连。另一个 relay 上的房间两次都不受影响。etcd 键约 8～9s 消失（registry_ttl 10s） |
| 3b relay 重启（附带） | relay 在 room_ttl 内重启时，房主能重建同一个房间号（RELAY.md §7 已列的限制），但比房主早几毫秒到的成员收到 `Closed{room_not_found}`、SDK 不再重连；新进程的报告 seq 从 1 重新数，lobby 在追上旧 seq 之前不改在线名单（演练里错了 52 秒）。**要维护者定**（见末尾） |
| 4 吞吐基线 | 一个 relay、每房 8 人：20 房 30Hz 交付 8.4k 帧/s，relay 约 0.11 核；50 房 60Hz 交付 42k 帧/s，约 0.7～0.8 核；100 房 60Hz 交付 84k 帧/s，约 1.3 核，p99 5.9ms，0 断线。只是本机基线，驱动与 relay 同机，不是容量结论 |
| A1 观察 | 成员 `Leave` 后目录立即去掉他，之后的心跳没有把他加回；房主 `Leave` 立即换 host（不等宽限期）。迟到报告的竞态窗口是毫秒级，真实进程里没有撞上，由 A1 的单元用例覆盖 |

## 环境与进程

- 工程：`roost project new lrmp -module example.com/lrmp -out lrmp -template lobby --skip-deps`，`go.mod` replace 指向本 worktree，`roost module add portal`，`config.account.yaml` 末尾加 `login.dev_token: dev-lrmp-drill`，`go build -o bin/app .`、`go vet ./...` 通过。照 [lobby-relay-portal-20261117](../lobby-relay-portal-20261117/README.md) 的做法。
- 依赖：隔离环境的 Redis、NATS（`~/.roost-it/roost-dataengine-it/env.sh`，只 source、不打印）；私有 etcd `127.0.0.1:23891`（scratchpad 里起，用完停掉）。键前缀 `roost:lrmp:*`、NATS 前缀 `lrmp`、主题 `roost.lrmp.lobby`、etcd 前缀 `/lrmp/`。
- 进程（[run.sh.txt](run.sh.txt)，配置由 [mkconf.py.txt](mkconf.py.txt) 生成）：

| 进程 | 命令 | 端口 |
| --- | --- | --- |
| etcd | `etcd --listen-client-urls http://127.0.0.1:23891 …` | 23891 / 23892 |
| account（带 portal） | `bin/app account --sid 1000` | ops 19400，web 19480 |
| lobby | `bin/app lobby --sid 1000` | ops 19401，HTTP 19481 |
| relay-1000 | `bin/app relay --sid 1000` | ops 19402，TCP 19410，WS 19420 |
| relay-1001 | `bin/app relay --sid 1001` | ops 19403，TCP 19411，WS 19421 |

- 客户端驱动：[drill-main.go.txt](drill-main.go.txt)（放在生成工程的 `cmd/drill`，只用 Go SDK `client/relay` 与 portal 的 HTTP 登录；没进仓库的源码树）。子命令：`player`（一个玩家一个进程：房主按 20Hz 广播 `tick=N`，用 `SnapshotState` 每 3s 推快照；成员按 20Hz 给房主发输入；收到 `YouAreHost` 从状态里取 tick、回 `HostReady`、当上房主后接着广播；重连策略 `MaxAttempts: 20`、退避 0.5s～5s 用 SDK 缺省）、`rooms`（列大厅）、`distribute`（连续建房看分布）、`load`（吞吐）。[players.sh.txt](players.sh.txt) 起一个房间的三个玩家进程（`<前缀>-a` 建房，`-b`、`-c` 加入），[watch.sh.txt](watch.sh.txt) 每 2 秒记一次房间记录的 PTTL、etcd 的 relay 键、大厅列表。
- 服务日志：account、lobby、relay-1000、relay-1001 四个进程的日志 WARN 0 条、ERROR 0 条（etcd 自己的启动 WARN 不计）。

## 1. 两个 relay 实例的房间分布

```
drill distribute -prefix dA -rooms 6 -gap 1500ms   # 每次建房后房主连上 relay，再等 1.5s
drill distribute -prefix dB -rooms 6 -gap 0
```

- 间隔 1.5s：relay-1000、relay-1001、relay-1000…交替，最后 3 / 3（[s1-distribute-gap.txt](s1-distribute-gap.txt)）；建完 etcd 两个键都是 `"rooms":3`。
- 间隔 0：6 个房间全部进 relay-1000（[s1-distribute-burst.txt](s1-distribute-burst.txt)）。原因按契约：选法读 etcd 的 `rooms`（relay 每秒至多更新一次，且房主连上之后才算），房间数相同取实例名小的。
- 关房后 1～2 秒内两个键回到 `"rooms":0`。

## 2. kill -9 房主客户端

```
players.sh h1      # 房间 ef5be5bdea41e1d1 在 relay-1000；h1-a 房主（pid 74201），h1-b slot 2，h1-c slot 3
sleep 9; drill rooms   # host=dev:h1-a host_epoch=0 members=3
kill -9 74201      # 21:38:41
sleep 20; drill rooms  # host=dev:h1-b host_epoch=1 members=2
```

时间线（[h1-b.log.txt](h1-b.log.txt)、[h1-c.log.txt](h1-c.log.txt)）：

| 时刻 | 事件 |
| --- | --- |
| 21:38:41.5 | kill -9 房主进程；它最后一条 STAT 是 tick 200 |
| 41.612 / 41.639 | B、C 各收到一次 `HostChanging{epoch 0}`（宽限期内发给房主的游戏帧被丢）：进程被杀时内核关掉连接，relay 立即发现，不用等 `host_idle_timeout` |
| 46.602 | B（在线成员里槽位最小）收到 `YouAreHost{epoch 1, 来源快照, seq 4, 年龄 6.285s, 成员 3 人在线 2 人, state "tick=180"}`；B 最后收到的是 tick 205，快照之后的 25 个 tick（1.25s）丢失，在“最多一个推送间隔 3s”之内 |
| 46.602 / 46.603 | B 回 `HostReady`，B、C 收到 `HostChanged{epoch 1, slot 2, dev:h1-b}` |
| 46.613 | C 收到 B 广播的 tick 181（从快照接着走，比 A 最后发的 205 小，这是玩法自己要处理的回退） |
| 47～48 | C 每秒继续收到 20 帧；B 收到 C 的输入；之后 B 推快照没有被拒（没有 `Rejected`） |
| 21:39:01 | 大厅：`host=dev:h1-b host_epoch=1 members=2`；Redis 记录里 B、C `online/connected`，A 的席位已让出（房间没开局） |

停顿约 5s（宽限期），与 RELAY.md §4A 的估算一致。

## 3. kill -9 relay 进程

### 3.1 修复前

```
players.sh h2            # 房间 f5973c3cce0620a5 分到 relay-1001（relay-1000 已有 h1 一个房间）
run.sh kill9 relay1      # relay-1000 pid 69210，21:39:44
watch.sh 20 <h1 房间> <h2 房间>   # 之后又看了 25 轮
```

- h1 的 B、C（[h1-b.log.txt](h1-b.log.txt)）：21:39:44.760 断开（`reason=disconnected reconnecting=true err=EOF`）。之后每次重连先向 lobby join（lobby 照常签令牌、给 relay-1000 的地址），再连 relay 被拒（connection refused），按退避重试；21:41:12.33 用完 20 次，最后一条 `reconnect gave up after 20 attempts: … connect: connection refused`。
- 目录（[s3-watch.txt](s3-watch.txt)、[s3-watch2.txt](s3-watch2.txt)）：h1 房间记录的 PTTL 一直在 25～29s 之间跳回去，直到 21:41:12 客户端放弃才开始一路减到 0；大厅列表里一直有这个房间（`members=2`，可加入），21:41:43 才消失，**从 kill 起约 118 秒**；契约（LOBBY.md §4“relay 不再报告时记录随 TTL 消失”、RELAY.md §1、方案 §5“房间记录在 TTL 内消失”）是 room_ttl（30s）。新玩家在这段时间里从列表看到它、join 成功、拿到连不上的地址，他的 join 也在给房间续命；重连次数设得更大，房间就一直活着。
- etcd：`/lrmp/relays/relay-1000` 在 21:39:50～21:39:53 之间消失（kill 之后约 8～9s，`relay.registry_ttl` 10s）。这之前新房间仍可能分给它（契约：租约到期前键还在），已补进 LOBBY.md §7。
- relay-1001 上的 h2（[h2-unaffected.txt](h2-unaffected.txt)）：三个进程全程没有断开，`send_err=0`，tick 与输入计数一直在涨。

根因：`service/lobby/rooms.go` 的 `JoinRoom` 对已在房间里的人（断线重连）把预留期重新算并写回记录；`RoomStore.Update` 每次写入都把 Redis 键的 PX 重置为 room_ttl（`versionstore.RedisStore` 的 `TTL`）。所以“只有 relay 的报告续期”这个前提在有人反复 join 时不成立。离开、改状态也一样会续。

### 3.2 修复（service/lobby）

- 房间记录新增 `alive_until_unix_ms`（`Room.AliveUntilUnixMilli`）：建房与每一条被接受的 relay 报告把它设为此刻加 room_ttl（`RoomStore.TTL()`）；join、leave、改状态照样写记录，但不改它。
- `Room.gone(now)`（已关闭或过了寿命）替换原来各入口的 `room.Closed`：join、leave、改状态答 `room_not_found`，列表不列，验权答 `room_not_found`，relay 的报告（含迟到的）不再让它复活。Redis 键仍按写入重置 PX，过了寿命的记录等键自己过期。
- 存储格式变化：没有这个字段的旧记录按已过期处理（线上未部署，不做兼容，升级时在途房间作废）。

先红后绿：

- 新用例 `service/lobby` 的 `TestRejoiningDoesNotKeepARoomAliveWithoutRelayReports`（内存与真实 Redis 两种目录）：建房、成员加入、一条 relay 报告；之后半个 TTL 内房主重连 join、成员重连 join、新人 join、房主改属性、新人离开都成功；再过半个 TTL 多一点，房主 / 成员 join、改状态答 `ErrRoomMissing`，列表为空，验权 `room_not_found`，迟到的报告不复活。
- 修复前两种目录都红在同一条承诺上（[red-before.txt](red-before.txt)）：`lobby_test.go:996: host rejoin after the ttl since the last relay report: err = <nil>, want ErrRoomMissing`。
- 修复后全绿（[green-after.txt](green-after.txt)）：`service/lobby`、`wiring/lobby`、`wiring/relay`、`client/relay` 加 `-race` 通过（Redis 用例用隔离环境的 Redis）；`wiring/relay` 带 `-tags integration` 的 `TestRealRelay*` 三条（真实 NATS + 用例自起的 etcd）与两条 lobby 契约用例通过；`go vet` 通过，`gofmt -l` 无输出。
- 变异 6 处，全部变红（[mutations.txt](mutations.txt)，[mutate.py.txt](mutate.py.txt)）：join 只看 Closed、relay 报告不续命、建房不设寿命、验权只看 Closed、列表不看寿命、报告入口只看 Closed（迟到报告复活）。

### 3.3 修复后再演一次

lobby 换成修复后的二进制（relay 与 account 不变），`players.sh h5`（relay-1000）、`players.sh h6`（relay-1001），21:56:06 `kill -9` relay-1000（pid 85879）：

- [s3-fixed-watch.txt](s3-fixed-watch.txt)：h5 房间 21:56:35 从大厅列表消失（最后一条 relay 报告之后约 30s；键的 PTTL 仍被 join 重置，记录等它自己过期）。
- h5 的三个客户端（[h5-a.log.txt](h5-a.log.txt) 等）：21:56:06.94 断开、开始重连；21:56:39.46 那次 join 拿到 `lobby: room_not_found (HTTP 404)`，`reconnecting=false`，停止。从 kill 起 33 秒，此前 87 秒（且取决于客户端重连次数）。
- h6（[h6-unaffected.txt](h6-unaffected.txt)）全程不受影响。

## 3b. relay 在 room_ttl 内重启（附带观察，没有修）

```
players.sh h3            # 房间 323dc69ba0d98a8a 在 relay-1000，跑了 30s（relay_seq 到 10）
run.sh kill9 relay1; run.sh start1 relay1   # 21:45:00.2 kill，新进程 21:45:00.73 已在接连接（/readyz 21:45:01.29）
```

- 房主 h3-a 21:45:00.732 重连成功，`Welcome{host}`：新进程上重建了同一个房间号（RELAY.md §7 已列：重启后关房记录没了，没过期的 host 令牌能重建）。
- 成员 h3-b、h3-c 分别在 00.731、00.729 到达，比房主早 1～3ms：relay 上还没有这个房间、令牌不是 host，收到 `Closed{room_not_found}`；SDK 把它当终止原因，不再重连（[h3-b.log.txt](h3-b.log.txt)）。结果房主一个人留在重建的房间里。谁先到取决于重连退避的时刻，三个客户端的退避节奏相同，先后是随机的。
- 目录（[s3b-record.txt](s3b-record.txt)）：新进程的报告 seq 从 1 重新数，lobby 只认比 10 大的 seq，所以 21:45:00～21:45:52 记录里 B、C 一直是 `online:true`（他们早已放弃）；21:45:52 seq 到 11 才改成只有房主。错的时长约“旧 seq × 心跳间隔”，房间活得越久越长（活了一小时的房间 seq 七百多，要再等一小时）。
- 同样的原因，新进程的 host_epoch 从 0 重来，lobby 只接受更大的 epoch：重建前换过房主的房间，重建后的换房主 lobby 要等 epoch 追上才认（按代码推断，演练里没有触发，**尚未确认**）。

这不是修复引入的，修复前后行为相同；它和 RELAY.md §1“relay 进程崩溃时房间丢失”与 §5“seq 同一房间内严格递增”两条前提冲突，改法有取舍，见末尾“要维护者定”。

## 4. 吞吐基线

relay-1001 先正常停掉，房间全部进 relay-1000（pid 85879）。驱动一个进程（[load.sh.txt](load.sh.txt)）：每房 1 个房主 + 7 个成员，房主按 Hz 向全体广播一帧游戏帧（64 字节，末尾带发出时刻），每个成员按 Hz 给房主发一帧输入；预热 5s，统计 60s。时延是房主发出到成员收到（同一台机器同一时钟），包含驱动自己的调度。relay CPU 用 `ps -o time` 前后差除以整段时间（含建连与预热，约 66～72s）；运行中每 5 秒的 `%CPU` 采样另列。

| 房间 × 人 | Hz | 连接 | relay 收 帧/s | relay 发出（交付）帧/s | 时延 p50 / p90 / p99 / max | relay CPU | relay RSS | 断线 / 发送失败 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 20 × 8 | 30 | 160 | 4 800 | 8 405（期望 8 400） | 1.7 / 2.9 / 4.8 / 14.8 ms | 7.5s / 66s ≈ 0.11 核（采样 6～15%） | 41 MB | 0 / 0 |
| 50 × 8 | 60 | 400 | 24 000 | 41 999（期望 42 000） | 2.4 / 3.2 / 4.0 / 22.7 ms | 47.4s / 67s ≈ 0.7 核（采样 63～81%） | 49 MB | 0 / 0 |
| 100 × 8 | 60 | 800 | 48 000 | 84 004（期望 84 000） | 3.7 / 4.9 / 5.9 / 11.9 ms | 87.0s / 72s ≈ 1.2 核（采样 103～140%） | 60 MB | 0 / 0 |

原始输出：[load-r20m8hz30.txt](load-r20m8hz30.txt)、[load-r50m8hz60.txt](load-r50m8hz60.txt)、[load-r100m8hz60.txt](load-r100m8hz60.txt)。

条件与限制：MacBook 10 核，驱动与 relay、lobby、account、NATS、Redis 同机，驱动自己的 CPU 与 relay 相当；机器有常驻后台负载（load average 跑前 4.3、跑时 6～8），swap 已用 8 GB / 9 GB；每档只跑一次 60s。只能当本机数量级参考，不是容量结论；没测到上限，没测房间通道与快照、换 host 期间的吞吐。

## 5. 顺带观察：经 lobby 离开（交接 A1）

```
ARGS_c="-leave-after 8s" ARGS_A="-leave-after 16s" players.sh h4   # Client.Leave = relay 的 Leave 控制帧 + lobby 的 leave
```

[s5-leave-record.txt](s5-leave-record.txt)（每 2 秒一条 Redis 记录）、[h4-a.log.txt](h4-a.log.txt) 等：

- 21:51:20.86 成员 C 离开：房主收到 `MemberLeft{left}`；21:51:21 起记录里只剩 A、B，之后 4 次心跳都没有把 C 加回；`leaving` 名单在 2 秒一次的采样里没有出现（relay 的 member_left 很快就把它清掉了）。
- 21:51:28.21 房主 A 离开：不等宽限期，B 同一毫秒收到 `YouAreHost{epoch 1, 来源快照, seq 6, 年龄 995ms}`、回 `HostReady`；21:51:29 记录里 `host=dev:h4-b host_epoch=1`，A 的席位让出。
- “在途报告晚于离开到达”的竞态窗口是毫秒级，这次真实进程里没有撞上；它由 A1 的单元用例（`TestAStaleOnlineReportAfterALeaveDoesNotBringTheLeaverBack`）覆盖。

## 清理

- 进程按 PID 停：玩家进程（`kill`）、relay-1001、lobby、account、etcd（`run.sh stop`），relay-1000 两次是 kill -9（演练本身）。收尾 `ps` 里没有本演练的 `bin/app`、`drill`、etcd 23891。
- Redis：`roost:lrmp:*` 演练前 0、演练后 1396 个键（账号与房间），已删到 0（[redisclean.sh.txt](redisclean.sh.txt)）。NATS 没有用可靠总线，不留流。
- 工程、日志、etcd 数据目录都在 scratchpad。

## 尚未确认

- relay 重启重建房间后，lobby 是否会因为 epoch 从 0 重来而一直不认新的换房主（3b 末条，按代码推断，没有在真实进程里触发）。
- 吞吐上限、跨机、两个 lobby 实例、同一套 account 配两个游戏 / 两个 lobby 互拒（方案 §5 的其余几条）这次都没做。

## 要维护者定

**relay 在 room_ttl 内重启时房间被重建（3b）**：

- **A（推荐）**：lobby 验权只在房间还没被任何 relay 报告过（`relay_seq == 0`）时放行 `create`。relay 重启后房主也进不来（`Closed{lobby_rejected}`），所有客户端放弃重连，目录在 room_ttl 内消失，和 RELAY.md §1“relay 进程崩溃时房间丢失”一致；RELAY.md §7 最后一条改成“不能重建”。代价：只剩房主、还没人进的房间在 relay 重启后也丢；改动是验权的一个条件加用例。
- **B**：允许重建，并补齐它：relay 在报告里带进程代号（启动时随机），lobby 见到新代号时把 `relay_seq` 归零、接受从 0 开始的 epoch；relay 对“房间还没建好时到达的成员”不立即拒绝（或 SDK 把这种 `room_not_found` 当可重试）。代价：协议加字段、lobby 与 relay 都多一种状态，C# / TS SDK 也要跟。
- C：保持现状，只在 RELAY.md §7 写清楚成员可能先到被拒、目录在线名单要等 seq 追上。

**（可选）更快发现 relay 没了**：现在客户端要等 room_ttl（30s）才从 lobby 拿到 404。lobby 在 join / 验权时可以再看一眼 etcd 里房间所在的 relay 键还在不在（键随租约在 registry_ttl 10s 内消失），没了就答 `room_not_found`。代价：每次 join 多读一次 etcd，etcd 抖动时会误判。没有实施。

## 建议写进 CHANGELOG 的文字（`[Unreleased]`）

> **修复**：lobby：relay 崩溃后，断线重连的 join（以及新人 join、离开、改状态）不再给房间续命，房间在最后一条 relay 报告之后 `room_ttl` 内从大厅列表、join、relay 验权里消失；此前客户端按 SDK 重连策略每次 join 都会重置房间记录的 TTL，2026-11-17 多进程演练里 relay 被 kill -9 后房间多活了 87 秒、一直可加入。房间记录新增 `alive_until_unix_ms`，没有这个字段的旧记录按已过期处理：升级时在途房间作废（线上未部署，不做兼容）。证据 `docs/maintenance/evidence/lobby-relay-multiprocess-20261117/`。

## 建议写进 KNOWN-LIMITS 的文字（二“大厅 lobby 与转发 relay：遗留”）

把“多进程演练没有做”一条换成：

> - **多进程演练（2026-11-17 已做）**：`-template lobby` + portal 工程上 account、lobby、两个 relay 真实进程，Go SDK 驱动每个玩家一个进程：两个 relay 的房间分布、kill -9 房主客户端（宽限期后按槽位换 host、新房主拿到快照、lobby 房主跟随）、kill -9 relay（另一个 relay 不受影响；查出并修复“重连 join 给崩溃 relay 的房间续命”）、吞吐基线（一个 relay，100 房 × 8 人 60Hz 交付 84k 帧/s 约 1.3 核，p99 5.9ms，本机同机，不作容量结论），证据 `evidence/lobby-relay-multiprocess-20261117/README.md`。没做：跨机、两个 lobby 实例、同一套 account 两个游戏 / 两个 lobby 互拒、容量上限。

新增一条遗留：

> - **relay 在 room_ttl 内重启会重建房间，待维护者定**（2026-11-17 演练）：房主重连在新进程上重建同一房间号；比房主早到的成员收到 `Closed{room_not_found}`、SDK 不再重连；新进程的报告 seq 从 1 重数，lobby 在追上旧 seq 之前不改在线名单（演练里 52 秒），epoch 从 0 重来时 lobby 可能不认之后的换房主（尚未确认）。选项 A（推荐，验权只在 relay 从没报告过时放行建房，重启即丢房）/ B（报告带进程代号，lobby 重置 seq / epoch）/ C（只写文档），见证据 `evidence/lobby-relay-multiprocess-20261117/README.md`“要维护者定”。

LOBBY.md §7 已补两条限制：单个 lobby 实例一秒内连续建房也会都分到同一个 relay；relay 没了之后 etcd 键要等租约到期（约 10s）才消失，这段时间新房间仍可能分给它。
