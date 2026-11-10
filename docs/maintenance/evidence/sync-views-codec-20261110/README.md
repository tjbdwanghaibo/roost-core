# 同步视图由业务层声明、增量 codec 由服务端守住（2026-11-10）

维护者 2026-11-10 对 [KNOWN-LIMITS“实体同步增量字节之后的剩余项”](../../KNOWN-LIMITS.md) 前两条的决定：

1. “同步数据是同步视图的问题，业务层处理，处理 dao 的视图的问题”：game-demo 的场景主视野用缺省视图 `"*"`，附近玩家收到别人的 exp / gold / items / attr_base / attr_final；在业务层（DAO 视图、packer、场景同步配置）解决，不做框架特例。
2. “同一视图的 packer 始终用同一个 Codec”只是前提（增量信封 v3 不带 Codec），在服务端加校验。

结论：第 1 项只改了 game-demo（三个 packer 与场景配置），框架已有的 `BandLayer` / `ViewSets` / `ProfilePriorities` 够用，没有补框架能力；第 2 项在 `framework/sync/entitysync` 按会话记基线 Codec，不一致的增量不发、改发全量。线格式没变。

## 1. 同步视图（game-demo）

### 1.1 改动

- `game/entities/player/sync_packer.go`：新增 `team` 视图；`near` 加 `cell_hp`、`cell_max_hp`（血条）；导出 `SyncViews()`。
- `game/entities/monster/sync_packer.go`：新增 `team`（与 `near` 相同）；导出 `SyncViews()`。
- `game/entities/building/sync_packer.go`：新增 `near`、`team`（与缺省视图相同，建筑字段对谁都公开）；缺省视图优先级 −1（与 Player / Monster 的同名视图一致）；导出 `SyncViews()`。
- `internal/service/game/scene.go`：主视野 `BandLayer` 一律给 `near`；队伍关系视野 `DefineRelationView(team, sceneTeamLayer)`；`ViewSets` = player / monster / building 三份视图集（Interest 构造时核对每类实体都声明了每个用到的视图）；Manager 的 `ProfilePriorities` 由 `entity.MergeSyncViewPriorities` 从同一批视图集得到。本人经 self 仍是缺省视图（`SelfLayer` 零值）；场景主体视图、分区场景的 `self` 绑定与 mirror 打包不变。

### 1.2 字段 × 视图

Player（✓ = 视图里有；default 是本人经 self 看自己）：

| 字段 | default | near（主视野里的别人） | team（队友，不论距离） | far（示例，未用） | self（分区场景本人） |
| --- | :-: | :-: | :-: | :-: | :-: |
| `name`、`level` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `equipment`、`guild_id` | ✓ | ✓ | ✓ | | ✓ |
| `pos_x`、`pos_y`、`scene_id` | ✓ | ✓ | ✓ | ✓ | |
| `facing` | ✓ | ✓ | ✓ | | |
| `cell_hp`、`cell_max_hp` | ✓ | ✓ | ✓ | | |
| `exp`、`gold`、`items` | ✓ | | | | ✓ |
| `attr_base`、`attr_final`、`cell_attack`、`cell_power` | ✓ | | | | |

Monster：default 全部；`near` = `team` = `template`、`pos_x`、`pos_y`、`facing`、`hp`、`rooted`（没有 `scene_id`）；`far` = `template`、`pos_x`、`pos_y`。Building：default = `near` = `team` = 全部同步字段（`state`、`owner`、`config_id`、`params`、`facing`、`gx`、`gy`、`foundation`）。Scene、TerrainChunk 各一个视图，不变。

与 [sync-delta-bytes 证据 §6](../sync-delta-bytes-20261110/README.md#6-每个同步字段是否需要发给其他客户端demo-player--monster) 的逐字段判断对照，差别两处：Player 的 `near` 保留 `scene_id`（只在全量里，2～5 字节；MMORPG 玩法夹具 `scripts/testdata/mmorpg` 多场景共用一个 Manager，用 `near` 并断言 `scene_id` 在），`attr_final`（“视玩法”）不给别人。

### 1.3 合并规则怎么落在 demo 上

- 本人在队伍里：self（default）与队伍视野（team）同时持有本人，default 包含 team 的全部字段 → 订阅保持 default，不是并集。
- 队友站在身边：主视野（near）与队伍视野（team）同时持有，两者字段相同，team 优先级 −1 < near 0 → 订阅是 team；走远只剩 team，不换视图、不重发全量。
- 不配 `ViewSets` 时 Interest 不知道字段，上面两种都会变成并集身份（`default+team`、`near+team`），队友每次进出主视野都换视图、重发全量（见 §1.5 变异）。

### 1.4 字节前后（同机同用例，帧 payload，不含线上 16 字节消息头）

| 用例 | 修前（缺省视图） | 修后（near） |
| --- | ---: | ---: |
| 一名玩家进入别人视野（`TestMainViewMeasureSyncBytes`，一帧一个对象） | 117 | 85 |
| 三只怪进入视野（同上，一帧三个对象） | 238 | 235 |
| 玩家 / 怪走一步（增量） | 30 | 30 |
| 新建玩家的全量 DAO 载荷，本人 / 别人（`TestEachObserverGetsOnlyItsViewOfAPlayer`） | 69 / 69 | 69 / 33 |
| 64 个玩家进入视野合计（`TestMainViewMeasureMovingCrowdBytes`，分在两帧） | 3899 + 3113 = 7012 | 2554 + 2410 = 4964 |
| 128 个玩家进入视野合计（分在四帧） | 13883 | 9787 |
| 128 个同时走，每拍中位数（每对象） | 3797（29.7） | 3807（29.7） |

全量信封多了视图键 `near`（4 字节：缺省视图的键在线上是空串），玩家全量的 DAO 部分少 36 字节；怪少了 `scene_id`（约 5 字节）、多了视图键，净少 1 字节。走一步的增量只有 `pos_x` / `pos_y`，与视图无关，不变。

### 1.5 先红后绿与变异

新用例 `TestEachObserverGetsOnlyItsViewOfAPlayer`（`internal/service/game/scene_test.go`）：本人、附近的人、远处的队友、身边的队友各自收到的 owner 全量的字段与视图键；改等级经验（AddExp）之后，本人的增量带 `level` 与 `exp`，别人的只带 `level`；别人的全量比本人的小。

修前（生成的 game-demo，demo 模板未改，用例同一份）原样失败文本（摘录；`received the player's private` 一类共 21 条 = 7 个字段 × 3 个观察者）：

```
observer 9302 holds player 9524228 in view "default", want "near"
observer 9303 holds player 9524228 in view "default", want "team"
observer 9304 holds player 9524228 in view "default", want "team"
a player nearby received the player's private "exp": map[_id:9524228 attr_base:map[] attr_final:map[1:100 2:10 3:30] cell_attack:0 … exp:0 … gold:0 … items:map[] …]
others' full is not smaller than the player's own: own 69 bytes, nearby 69, teammate 69
observer 9302: the delta carries exp = true, want false: map[attr_base:map[1:120 2:14 3:40] attr_final:map[1:120 2:14 3:40] exp:50 level:2]
```

修后通过（own 69 / nearby 33 / teammate 33 字节）。变异（生成工程副本上每次只改一处，跑这条用例，确认变红后还原）：

| 变异 | 变红的原文 |
| --- | --- |
| 主视野 `BandLayer` 改回缺省视图 | `observer 9302 holds player 9524228 in view "default", want "near"`；`a player nearby received the player's private "exp"` |
| 队伍视野改回缺省视图 | `observer 9303 holds player 9524228 in view "default", want "team"`；`a teammate received the player's private "exp"` |
| Interest 不配 `ViewSets`（优先级照旧） | `observer 9301 holds player 9524228 in view "default+team", want "default"`；`observer 9304 holds player 9524228 in view "near+team", want "team"` |
| Player 的 `near` / `team` 加上 `Exp` | `a player nearby received the player's private "exp"`（三个观察者各一条） |

既有用例的断言随契约改：`TestSceneReplicatesOnePlayersChangeToAnother` 原来断言“另一个玩家收到的增量带 exp”，这正是被改掉的泄露，改为断言带 `level`、**不带** `exp`；`TestSceneRetriesASubscribeTheRoomRefused` 用 `exp` 判断队友收到的是全量，改用同样只在全量里出现的 `name`（判据不变：增量不会带没改的字段）。

## 2. 增量的 codec 与基线一致（framework/sync/entitysync）

### 2.1 选择与语义

增量信封不带 Codec，接收端（Go `Decoder`、C# `SubjectUpdate.Decode`、C++ `sync_receiver`）按同一对象上一次全量的 Codec 解增量载荷；空载荷的增量不改基线的 Codec。

可选两种：A. 打包 / 注册时拒绝换 Codec；B. 发送时按会话核对，不符就改发全量。**选 B**：Codec 只有打包之后才知道；换 Codec（载荷格式升级）本身合法，A 会让这个 subject 的同步卡在“打包失败”上；B 只让真正持有旧 Codec 基线的会话多收一次全量，基线已经是新 Codec 的会话照常收增量。

实现（`flush.go` `codecMismatchLocked`）：`subscription.codec` 是会话持有的基线 Codec，只在结算一份全量时更新（`settleSubscriptions`）。把增量分给 `kindLive` 订阅之前比较：非全量、载荷非空、Codec 不同 → 这份增量不发给这个会话；订阅转回 `kindSnapshot`（`baseVersion = 0`），subject `markPending` 并 `WakeSync`；下一次 Flush 捕获当前状态的全量（`Reason = Resync`，带新 Codec）。同一份内容版本照常提交，其他会话不受影响。

失败语义：不是错误，不关会话、不让 Flush 失败；这个会话晚一个 Flush 看到这次变化，多收一份全量。计数：`entitysync_codec_resyncs_total{namespace}`（`namespace` 是 subject 的同步命名空间，随实体类型有限）、`ManagerStats.CodecResyncs` / `ManagerCounters.CodecResyncs`；限频 Warn（每个 Manager 10 秒至多一条，带被省略的次数）。正常为 0。

结构体大小不变：`codec uint16` 放在 `subscription` 里 `inFlight` / `overLimit` 之后的填充字节里。未超额逐字节不变的金样（`TestUnderTheLimitFramesAreByteIdentical`）照旧通过：同一 Codec 的 packer 帧不变。

### 2.2 先红后绿与变异

用例 `TestADeltaInAnotherCodecThanTheBaselineGoesOutAsAFull`（`wire_test.go`）：自定义 packer 的载荷文本写着产生它的 Codec（`full@1`、`delta@2`）。会话 1 在 Codec 1 时订阅，之后 packer 换成 Codec 2、会话 2 订阅（基线 Codec 2），再改一次：会话 1 必须收到 Codec 2 的全量（Reason Resync），会话 2 必须照常收 Codec 2 的增量，计数加 1；之后两边都收增量，中间一份空载荷（Codec 0）的增量不引起重同步。

修前原样失败文本（为在修前代码上编译，`Stats().CodecResyncs` 一行临时换成常量）：

```
session 1 read "delta@2" as codec 1
```

即接收端把 Codec 2 的字节当 Codec 1 解——静默错用，不报错（KNOWN-LIMITS 原记“尚未确认会不会静默错用”，此为确认：会）。

变异（每次只改一处）：

| 变异 | 变红的原文 |
| --- | --- |
| 不做比较（等于修前） | `session 1 read "delta@2" as codec 1` |
| 基线 Codec 从不记录（恒 0） | `session 2 (codec-2 baseline) got [... Full:true Reason:2 ...], want one codec-2 delta` |
| 拒绝增量但不转回待快照 | `session 1 (codec-1 baseline) got [] after the switch, want one codec-2 full with reason resync` |
| 增量结算也改基线 Codec | `session 1 got [] for mask 0x1 once both hold codec 2, want one delta`（空载荷增量把基线改成 0，下一份增量被误判） |
| 空载荷增量也比较 Codec | `session 1 got [] for mask 0x2 once both hold codec 2, want one delta` |

## 3. 验证

全部 `GOWORK=off`，本机；真实依赖（Mongo / NATS / Redis）的用例按缺省跳过，由协调方另跑。

| 项 | 结果 |
| --- | --- |
| `gofmt` / `go vet` / `go build ./...` | 通过 |
| `go test -race -count=1 ./framework/sync/... ./framework/entity/...` | 通过 |
| `go run ./cmd/glsvet ./framework/nest ./framework/entity ./framework/dataengine/engine ./framework/sync/entitysync` | 通过 |
| 根包 `go test -count=1 .`、全量 `go test -count=1 ./...` | 通过 |
| `go test -count=1 ./codegen/...`（含生成 game-demo 的 `-race` 场景用例） | 通过 |
| `bash scripts/test-sync-modes-generated.sh` | 通过 |
| `python3 scripts/check-doc-anchors.py` | 通过 |
| 生成的 game-demo（replace 指回本检出）：build / vet、`go test -count=1 ./...`；`internal/service/game`、`game/entities/...`、`cmd/loadtest` 加 `-race`；`roost id check`、`roost generate --check` | 通过 |
| C#：`Roost.Client.Tests -- client/spec/packets.json`、`Roost.Client.Terrain.Tests -- client/spec` | 通过 |

## 4. 线格式与兼容

线格式没有变化（帧协议 2 / 信封 v3）。客户端看到的变化：别的玩家（以及怪、建筑）的全量里视图键是 `near` / `team`（原来是空串，即 `default`），Player 少了私有字段。按（命名空间, 视图键, 编码, schema）注册解码器的客户端（C# `EntityViews.Register` / `RegisterDao`、C++ `register_dao`）要为 `player`、`monster`、`building` 各注册 `near` 与 `team`（同一个 DAO 同步 PB 解码器）；只看自己的客户端（本人仍是 `default`）不用改。仓库里的 C# / C++ 夹具只注册本人的 `player` / `default`（`Roost.Client.Demo`、`client/cpp/tests/gate.cpp`），不受影响。线上尚未部署，不做旧客户端兼容。
