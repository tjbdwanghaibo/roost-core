# 实体同步一步移动的下行字节（2026-11-10）

起因：主视野尺度的实测（[terrain-main-view-scale 证据 §4](../terrain-main-view-scale-20261110/README.md)）里，玩家走一步单帧 140 字节、同一帧每多一个玩家 108 字节，64 个移动对象约 35～46 KB/s，超过每个观察者 50 KB/s 的预算的一半到接近全部。维护者：“增量同步，理论上数据不会很多，动的话只同步位置信息”，并要求按三类（字符串、信封、同步的信息）逐项拆字节、优先改这三类。

结论：位置本身只有 5 字节（`pos_x` 包装），其余 135 字节是帧头、信封与重复的身份。改动后一步 30 字节、每多一个对象 20 字节；64 个移动对象约 7～8 KB/s，128 个约 14～15 KB/s。脏位粒度本来就对（一步只有 `pos_x` 的脏位），没有整组重发，也没有时间戳一类的服务端内部字段。

## 1. 测量方法

生成的 game-demo（`roost project new … -template game-demo`，replace 指回当前源码），一个观察者、另一个玩家走一步（移动心跳 200ms 一步）、四个玩家同一拍各走一步、怪巡逻一步，以及玩家 / 怪进入视野的全量。用 `TestMainViewMeasureSyncBytes` 的流程，在每次 `take` 之后加一个临时的拆解函数：外层帧逐字段解码，subject 信封逐字段解码，DAO 同步 PB 按 `protowire` 逐字段列出 tag 与值的字节。拆解函数只在临时工程里，不提交。修后的字节由正式用例 `TestScenePositionStepsCarryLittleBesidesThePosition` 每次运行打出（帧头 + 对象 / 组件头、信封、DAO 补丁三项）。

数字是帧的 payload（含帧头）；线上每个推送另加 16 字节消息头，前后相同。

## 2. 修前拆解（帧协议 1 / 信封 v2）

### 2.1 玩家走一步：140 字节

| 部分 | 字节 | 内容 |
| --- | ---: | --- |
| 帧头 | 32 | 魔数 4、版本、类型、StreamID、Epoch 8、Tick 8、BaseTick 8、SchemaVersion、对象数，全是定长 |
| 对象头 | 9 | 操作 1、Ref.ID 2、Ref.Generation 2、Archetype 2、组件数 2 |
| 组件头 | 9 | 操作 1、TypeID 2、SchemaVersion 2、长度 4 |
| subject 信封 | 64 | 魔数 4、版本 2、标志 2、subject ID 8、Kind 4、Version 8、BaseVersion 8、Mask 8、Reason 4、Codec 2、LOD 1、保留 1、视图 schema 4、命名空间长度 2、视图键长度 2、载荷长度 4 |
| 命名空间 `player` | 6 | 字符串 |
| 视图键 `default` | 7 | 字符串 |
| DAO 补丁 | 13 | `entity_id` 5（tag 1 + 值 4）、`sync_format` 3、`pos_x` 5（tag 1 + 长度 1 + 内层 tag 1 + 值 2） |

四个玩家同一拍各走一步：一帧 464 字节 = 32 + 4 × 108。怪走一步 146 字节（怪的实体 ID 是 19 位数，补丁里的 `entity_id` 占 10 字节）。

### 2.2 按三类分

| 类别 | 字节 | 明细 |
| --- | ---: | --- |
| 字符串 | 17 | `player` 6、`default` 7、两个长度字段 4 |
| 信封（字符串以外） | 60 | 上表信封 64 减两个长度字段；每个增量都重复 subject ID、Kind、Codec、LOD、视图 schema，版本与基线各 8 字节定长，Mask 8 字节定长 |
| 帧、对象、组件头 | 50 | 32 + 9 + 9，全部定长；update 也带 Archetype |
| 同步的信息 | 13 | 位置 5；`entity_id` 5（增量里重复，接收端已有）；`sync_format` 3 |

### 2.3 进入视野（全量）

| 用例 | 修前 | 其中 DAO 全量 |
| --- | ---: | ---: |
| 玩家进入视野（一帧一个对象） | 205 | 78 |
| 三只怪进入视野（一帧三个对象） | 443 | 3 × 41 |

## 3. 可省的字节（按一步移动的收益排序）

| # | 项 | 一步省 | 契约影响 | 状态 |
| --- | --- | ---: | --- | --- |
| 1 | 增量信封不带身份（subject ID、Kind、命名空间、视图键、Codec、LOD、视图 schema），由接收端从同一 ObjectRef 的全量继承；定长整数改 uvarint，版本写成 BaseVersion + 差值 | 77 | 会话里 ObjectRef 持有期间只对应一个 subject（换 subject 必先 remove 再 create，create 一定是全量）；BaseVersion 仍逐个核对。客户端改为有状态解码 | 已实施 |
| 2 | 帧头、对象头、组件头改 uvarint；增量帧写 `Tick−BaseTick`；Archetype 只在 create 上 | 32 | 接收端照旧逐项核对 epoch / tick / 基线 / 代际；非最短编码当损坏拒绝 | 已实施 |
| 3 | DAO 增量补丁不带 `entity_id` | 3～10（玩家 4、怪 10） | 身份由信封给出；Apply 在增量上接受缺省的 0，带了不同的 ID 照旧拒绝 | 已实施 |
| 4 | 视图键 `default` 写成空串（全量里） | 只省全量 7 | 无 | 已实施 |
| 5 | 增量补丁不带 `sync_format` | 3 | 全量已经声明格式；需要生成器和 C# / C++ Apply 在增量上接受缺省值 | 候选 |
| 6 | 标量字段的包装消息（`pos_x` = tag + 长度 + 内层 tag + 值）改 proto3 `optional` | 每字段 2 | DAO 同步 proto 的形状变化，三端生成物都要换；零值与“没改”的区分改由 presence 表达 | 候选 |
| 7 | 增量不写 Mask（载荷里的字段已经说明改了什么） | 1 | Mask 是 `SubjectSyncUpdate` 的公开字段，客户端若用它判断要改哪些字段需改读载荷 | 候选 |
| 8 | 帧里“一个对象一个组件”的简写 | 约 3 / 对象 | 帧格式再变一次；Manager 现在每个对象恰好一个组件 | 候选 |
| 9 | BaseVersion 只发低位（或不发，靠帧的基线连续性） | 运行久了 1～3 | 削弱逐对象的版本核对（低 8 位碰撞 1/256），需要维护者决定是否接受 | 候选，需决定 |
| 10 | 会话内字符串表 / 编译期常量表（命名空间、视图键编号） | 只省全量 6～8 | 增量已经不带字符串；全量里每个对象省 7 字节左右，客户端要多一张表 | 候选 |
| — | 远处降频 / 距离 LOD | 按距离 | 本轮按要求不做 | 不做 |

改动 1～4 之后，增量里已经没有任何字符串；全量里只剩命名空间（`player` / `monster`），视图键缺省为空。

## 4. 修后拆解（帧协议 2 / 信封 v3）

### 4.1 玩家走一步：30 字节

| 部分 | 字节 | 内容 |
| --- | ---: | --- |
| 帧头 | 10 | 魔数 `RF` 2、协议版本 1、类型 1、StreamID 1、Epoch 1、Tick 1、`Tick−BaseTick` 1、SchemaVersion 1、对象数 1 |
| 对象头 | 4 | 操作 1、Ref.ID 1、Ref.Generation 1、组件数 1（update 不带 Archetype） |
| 组件头 | 4 | 操作 1、TypeID 1、SchemaVersion 1、长度 1 |
| subject 信封 | 4 | 标志 1、BaseVersion 1、`Version−BaseVersion` 1、Mask 1 |
| DAO 补丁 | 8 | `sync_format` 3、`pos_x` 5 |

同一帧每多一个对象 20 字节（对象头 4 + 组件头 4 + 信封 4 + 补丁 8）；怪走一步同样是 30 字节（实体 ID 不再出现在增量里）。

三类对照：

| 类别 | 修前 | 修后 |
| --- | ---: | ---: |
| 字符串 | 17 | 0 |
| 信封（字符串以外） | 60 | 4 |
| 帧、对象、组件头 | 50 | 18 |
| 同步的信息 | 13 | 8 |
| 合计 | 140 | 30 |

**随运行时间增长的字节**：Tick 每帧加 1（一个会话 20 帧/秒）：超过 127 约 6 秒后、超过 16383 约 14 分钟后各多 1 字节，帧头稳态 12 字节；subject 的 Version 每次变化加 1（一个对象 5 步/秒）：BaseVersion 超过 127 约 25 秒后、超过 16383 约 55 分钟后、超过 2097151 约 4.9 天后各多 1 字节；会话里对象引用超过 127 个时 Ref.ID 占 2 字节。下面的带宽估算按稳态：帧头 12、每对象 22 字节。

### 4.2 进入视野（全量）

| 用例 | 修前 | 修后 | 明细（修后） |
| --- | ---: | ---: | --- |
| 玩家进入视野 | 205 | 117 | 帧头 10 + 对象 / 组件头 9 + 全量信封 20（标志、ID 4、Kind、Version、BaseVersion、Reason、Codec、LOD、视图 schema、`player` 1 + 6、视图键空 1）+ DAO 全量 78 |
| 三只怪进入视野 | 443 | 238 | 帧头 10 + 3 × （头 9 + 信封 26 + DAO 全量 41） |

全量的 DAO 部分没变：全量仍带 `entity_id`。玩家的全量用的是缺省视图 `"*"`（见 §6），78 字节里有 exp、gold、items、attr_base、attr_final 等其他玩家不需要的字段。

## 5. 每个观察者的下行带宽

模型：N 个移动对象各 200ms 走一步（每对象 5 步/秒），场景每 50ms 出一帧（每会话至多 20 帧/秒），每帧另加 16 字节消息头。“批量”是同一窗口的步合进同一帧（64 个对象每帧约 16 步），“最坏”是每一步各自成帧。1 KB = 1000 字节。

| 对象数 | 修前批量 | 修前最坏 | 修后批量（单轴） | 修后批量（斜走，多 `pos_y` 5 字节） | 修后最坏（斜走） |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 64 | 35.5 KB/s | 49.9 KB/s | 7.6 KB/s | 9.2 KB/s | 17.6 KB/s |
| 128 | 70.1 KB/s | 99.8 KB/s | 14.6 KB/s | 17.8 KB/s | 35.2 KB/s |

**实测（同机同用例 `TestMainViewMeasureMovingCrowdBytes`，[地形方案 §29.6](../../FEATURE-2026-11-08-editable-terrain.md#296-每观察者下行增量字节压缩后-128-个同时动约-186kbs在-50kbs-以内)）**：开地形的普通场景，N 个玩家在观察者周围 10～29m 沿切线斜走（每步改 `pos_x` 与 `pos_y`），每拍（200ms）复制一次，帧 payload 不含消息头，这一行 KB = 1024 字节：

| 对象数 | 修前：每对象 / 下行 | 修后：每对象 / 下行 |
| ---: | ---: | ---: |
| 64 | 119.3 字节 / 37.3KB/s | 30.4 字节 / 9.5KB/s |
| 128 | 117.6 字节 / 73.5KB/s | 29.7 字节 / 18.6KB/s |

进入视野的全量合计：64 个 11447 → 7022 字节，128 个 22583 → 13893 字节。

算式：修前批量 = N × 5 × 108 + 20 × (32 + 16)；修前最坏 = N × 5 × (140 + 16)；修后批量 = N × 5 × 22（斜走 27）+ 20 × (12 + 16)；修后最坏 = N × 5 × (12 + 27 + 16)。128 个对象同时走在 50 KB/s 预算内，单轴移动约占 30%。进入视野的全量、建筑与地形块不在此估算里。

## 6. 每个同步字段是否需要发给其他客户端（demo Player / Monster）

一步移动的脏位只有 `pos_x`（玩家 mask `0x20`，怪 `0x2`），增量补丁只带它：脏位按字段、不整组重发，同步字段里没有时间戳或服务端内部计数。下表是主视野（其他玩家看到的）是否需要：

**Player**（DAO `dao_sync.proto`；主视野现在用缺省视图 `"*"`，全部字段）

| 字段 | 需要发给其他玩家？ | 说明 |
| --- | --- | --- |
| `entity_id` | 只在全量 | 增量已不带 |
| `sync_format` | 只在全量即可 | 增量里 3 字节，候选 5 |
| `name`、`level` | 是 | 头顶名字、等级 |
| `exp`、`gold`、`items` | **否** | 私有数据，只该在自身视图（`self`） |
| `pos_x`、`pos_y`、`facing` | 是 | 位置与朝向 |
| `scene_id` | 否（主视野） | 观察者与被观察者在同一场景；只在全量里，2～5 字节 |
| `equipment` | 是 | 外观 |
| `attr_base` | **否** | 私有属性 |
| `attr_final` | 视玩法 | 若只要血条，用 `cell_hp` / `cell_max_hp` 即可 |
| `guild_id` | 是 | 公会名牌 |
| `cell_hp`、`cell_max_hp` | 是 | 血条 |
| `cell_attack`、`cell_power` | 否 | 战斗内部数值 |

player 的 packer 已经定义了 `near` 视图（`Name, Level, PosX, PosY, SceneID, Facing, Equipment, GuildID`），但场景主视野（`policy.InterestConfig` 没配 Bands，`DefineRelationView(sceneView, SyncProfile{})`）用的是缺省视图 `"*"`。这既多发字节（全量 78 字节里约一半是私有字段），也让附近玩家收到别人的 exp / gold / items / attr_base。改主视野用哪个视图是产品选择，见 KNOWN-LIMITS。

**Monster**

| 字段 | 需要发给客户端？ | 说明 |
| --- | --- | --- |
| `entity_id` | 只在全量 | 增量已不带（怪的 ID 占 10 字节） |
| `template` | 是（只在全量） | 模型 / 外观 |
| `pos_x`、`pos_y`、`facing` | 是 | |
| `hp` | 是 | 血条 |
| `scene_id` | 否（主视野） | 只在全量里 |
| `rooted` | 视玩法 | 定身表现 |

## 7. 实施

1. **帧协议 2**（`framework/sync/frame`，`ProtocolVersion = 2`）：魔数 `RF`，整数一律最短 uvarint，增量帧写 `Tick−BaseTick`，Archetype 只在 create；`MaxHeaderBytes` / `MaxObjectHeaderBytes` / `MaxComponentHeaderBytes` 给出编码上限，Manager 分帧按上限预留。
2. **subject 信封 v3**（`framework/sync/entitysync`，`WireVersion = 3`）：增量只有标志、BaseVersion、差值、可选 Mask / Reason 与载荷；全量带全部身份，视图键 `default` 写成空串。新增有状态的 `entitysync.Decoder`（客户端、机器人、测试共用），删除可以单独解一个信封的 `DecodeSubjectUpdate`。快照预算对没测过大小的候选按本 lane 已测到的最小一份估算。
3. **DAO 增量不带 `entity_id`**（codegen）：生成 `MarshalSyncDelta`，`MarshalSyncChanges` 不带 ID；Go / C# 的 `ApplySync*` 在增量上接受 0。demo 的四个 packer 的 `PackSubjectDelta` 改用 `MarshalSyncDelta`。
4. **客户端**：C# `SubjectUpdate.Decode(data, baseline)`、`EntityViews` 自动传基线；C++ `sync_receiver` 同样按对象表继承；`client/spec/packets.json` 的 sync 金样重生成，三端逐字节比对。
5. 调用方迁移：框架、wiring、gameplay、codegen 模板与夹具、demo 模板、scripts 的测试客户端改用 `entitysync.Decoder`。

## 8. 先红后绿

修前（基线源码，帧协议 1 / 信封 v2）原样失败文本：

- 框架用例 `TestAPositionDeltaCarriesLittleBesidesThePosition`（单个对象一步、5 字节补丁）：`one position step costs 127 bytes besides the position patch, want at most 24`。
- 生成工程 `TestScenePositionStepsCarryLittleBesidesThePosition`（为在修前 API 上编译，解码部分换成旧的 `frame.Decode` + `DecodeSubjectUpdate`，字段检查放过 `_id`，三条字节上限不变，逐条放开前一条看下一条）：
  - `a player's step is a 140-byte frame, want at most 32`
  - `each extra moving player costs 108 bytes in the frame, want at most 22`
  - `a monster's patrol step is a 140-byte frame, want at most 32`

修后都通过：玩家一步 30 字节、每对象 20 字节、怪一步 30 字节；框架用例单帧开销 22 字节（≤24）、每多一个对象开销 12 字节（≤12）。

**变异**（在源码副本上每次只改一处，跑对应用例，确认变红后还原）：

| 变异 | 变红的用例与原文 |
| --- | --- |
| 解码不核对 BaseVersion | `TestSubjectEnvelopeRoundTripsAndADeltaInheritsItsIdentity`：`a delta on version 9 applied to a baseline at version 8`；`TestDecoderRefusesWhatDoesNotContinueTheStream`：`a delta on an old version: accepted` |
| 增量也按全量带全部身份 | `TestAPositionDeltaCarriesLittleBesidesThePosition`：`one position step costs 38 bytes besides the position patch, want at most 24` |
| 解码不查“对象引用换了 subject” | `TestDecoderRefusesWhatDoesNotContinueTheStream`：`another subject on the ref: accepted` |
| 没测过大小的快照候选退回按最小信封估算 | `TestByteBlockedSnapshotsAreNotRecapturedEveryWindow`：`blocked windows re-encoded snapshots: X packs=2 SnapshotsCaptured=46 delivered objects=31 windows=6` |
| demo 的玩家 / 怪 packer 增量改回 `MarshalSync`（带 `entity_id`） | `TestScenePositionStepsCarryLittleBesidesThePosition`：`a player's step: subject 10078212's delta carries "_id" …`；放过 `_id` 之后：`a player's step is a 35-byte frame, want at most 32`，再放开单帧上限：`each extra moving player costs 25 bytes in the frame, want at most 22`，再放开每对象上限：`a monster's patrol step is a 35-byte frame, want at most 32` |

**未超额逐字节不变的金样**：`over_limit_test.go` 的 `TestUnderTheLimitFramesAreByteIdentical` 钉的是整段帧字节的哈希，格式变了哈希必然变。为确认“未超额时内容与修前相同”这一承诺没被顺手破坏，在修前与修后源码上各跑同一用例，把每帧的对象（操作、ref、代际）、组件与解出的信封字段（subject、kind、版本、基线、mask、reason、命名空间、视图键、载荷）逐行打出，两份输出逐字相同，然后才换新哈希。

## 9. 验证

全部 `GOWORK=off`，在本机运行；需要真实依赖（Mongo / NATS / Redis）的用例按缺省跳过，由协调方另跑。

| 项 | 结果 |
| --- | --- |
| `gofmt` / `go vet ./...` / `go build ./...` | 通过 |
| `go test -race -count=1 ./framework/sync/... ./framework/entity/...` | 通过 |
| `go run ./cmd/glsvet ./framework/nest ./framework/entity ./framework/dataengine/engine ./framework/sync/entitysync` | 通过 |
| `go test -count=1 ./...`（全量，合并 main 之前）；合并后重跑 `.`、`./codegen/...`、`./demo/...` | 通过 |
| `bash scripts/test-sync-modes-generated.sh` | 通过 |
| `python3 scripts/check-doc-anchors.py` | 通过 |
| 生成的 game-demo：build / vet、`go test ./...`、`internal/service/game`、`cmd/loadtest`、`game/entities/...` 加 `-race`、`roost id check`、`roost generate --check` | 通过（合并 main 之后重新生成重跑；合并前的一轮暴露一处旧断言——玩家视图过滤用例按“增量带 `_id`”数字段，已改为只断言 `pos_x`）|
| C#：`Roost.Client.Tests -- client/spec/packets.json`、`Roost.Client.Terrain.Tests -- client/spec` | 通过（含 golden sync、Sync receiver、EntityViews、DAO PB Go/C# 用例） |
| C++：`scripts/test-client-cpp.sh`（不带 protobuf） | 5 项 ctest 通过；protobuf 用例本机无 protoc 未跑 |
