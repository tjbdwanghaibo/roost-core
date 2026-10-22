# 业务层如何处理玩家协议

玩家接入层已经由生成器提供，不需要再创建一个 Session 服务。本篇以 demo 中的“穿戴装备”为例：客户端只提交物品 ID，服务器从已认证连接取得玩家 ID，通过 Nest 修改背包和装备，最后返回响应。

## 1. 请求经过哪些层

```text
客户端 PB 请求
  → Gate / Game TCP（连接与认证）
  → playeraccess.ProtocolRegistry（协议校验、解码、请求上下文）
  → player.Controller.HandleEquip（业务入口）
  → syncsender.EquipItemSender（生成的 Nest 调用）
  → handlerEquipItem（Entity 保护下的业务和事务）
  → Controller 返回 PB 响应 → 接入层编码、回包
```

独立 Gate 部署在 Gate 与协议派发之间还有 NATS 和 GameIngress；嵌入式部署直接使用本地 TCP 接入。业务 Controller 的写法相同。这里的 `playeraccess.Context.Session` 是 `gateway.Session`，不是管理副本运行的 `service/session`。

## 2. 定义协议

下面是已有 demo 的定义；已有工程不要重复添加相同消息号。源码为 [equip.go.tmpl](../../demo/protocol/def/equip.go.tmpl)。

```go
//go:build protocoldef

package protocoldef

type EquipRequest struct {
    ItemID int64 `pb:"1"`
}

type EquipResponse struct {
    Code     int32  `pb:"1"`
    Reason   string `pb:"2"`
    Slot     int32  `pb:"3"`
    Replaced int64  `pb:"4"`
}

//roost:protocol group=game handler=player
type EquipProtocol interface {
    //roost:msg id=10018
    Equip(EquipRequest) EquipResponse
}
```

在生成工程执行 `go generate ./...`，生成 PB 类型、编解码、消息号和注册代码。启动接线通过 `RegisterPlayerProtocols` 注册 Controller，再封存注册表。框架负责解码和回包，Controller 不要再手动写 socket，否则可能重复响应。

启用 player access 后，生成器还会按 `handler=player` 在 `game/controllers/player` 自动补齐 `Controller`、`FromRegistry` 和缺少的 `HandleXxx`。请求参数是 `*playeraccess.Context` 与对应 PB 请求；有响应的协议返回 `(*pb.XxxResponse, error)`，单向请求只返回 `error`。新方法先返回“not implemented”错误，必须补业务才能提供服务；该错误会进入接入层的错误处理，不是成功响应。

这些骨架生成后属于业务代码：重复生成、`--force`、项目同步都不覆盖或删除已有实现。方法可以移动到同包其他 Go 文件；生成器按 Controller 的方法声明查重。已有类型与 `FromRegistry` 必须配套；同名文件占用但缺少对应方法时会明确报冲突。协议删除或签名变更后，业务方法由开发者调整，编译负责校验类型是否仍匹配。`roost generate --check` 会报告缺失的骨架。

此过程不要求先写 Nest handler，也不会自动替你调用 Nest。首次生成的 Controller 提供 `NestClient()`，你可按下一节调用 Sender。已有 `roost add endpoint` 仍适合显式生成连接到 Nest handler 的方法：它生成的方法就是下一节的写法（接住 handler 的返回值，业务错误经 `errcode.ClientError` 变成带 `Code` / `Reason` 的响应、只给 `CodeInternal` 记日志），要求 Response 带 `Code` / `Reason` 字段；已自动生成骨架后，直接编辑该方法即可。首次生成的 Controller 同时提供 `Registry()`；登录边界用 `roost add endpoint EnterGame --handler player --lifecycle Player` 生成，它对 `context.PlayerID` 调 lifecycle `GetOrCreate`，保证之后的 Nest 请求打到的 Entity 已存在——没有这一步时，Nest 冷加载不存在的玩家回复稳定码 `errcode.CodeEntityNotFound`（`entity_not_found`），不是 `CodeInternal`。删过档的玩家（D10：该 ID 不可再用）业务请求与 EnterGame 都回复 `errcode.CodeEntityDeleted`（`entity_deleted`），客户端不要重试或自动重新登录。

## 3. Controller：把协议转换成业务调用

以下方法与 demo 的真实接口一致，放在业务工程 `game/controllers/player` 包中。依赖已有 Controller 的 `NestClient()`；`player` 指业务工程的 `game/entities/player`，`syncsender` 指 `game/handler/syncsender`，`playeraccess` 指 `game/playeraccess`，`pb` 指 `protocol/pb`。完整 imports 见 [Controller 源码](../../demo/game/controllers/player/equip.go.tmpl)。

```go
func (controller *Controller) HandleEquip(
    ctx *playeraccess.Context, req *pb.EquipRequest,
) (*pb.EquipResponse, error) {
    if ctx == nil || req == nil {
        return nil, fmt.Errorf("equip: context and request are required")
    }
    // 使用认证身份，不能让客户端传入另一个玩家 ID。
    id, err := entity.BuildEntityID(ctx.PlayerID, player.EntityKindPlayer)
    if err != nil {
        return nil, err
    }

    // Controller 在 Nest 业务池外，允许等待 Sender 的处理结果。
    result, err := syncsender.NewEquipItemSender(controller.NestClient()).
        Sync_EquipItem(ctx.Context(), id, req.ItemID)
    if err != nil {
        code, reason := errcode.ClientError(err)
        if code == errcode.CodeInternal {
            slog.Error("equip failed", "player_id", ctx.PlayerID, "err", err)
        }
        return &pb.EquipResponse{Code: code, Reason: reason}, nil
    }
    return &pb.EquipResponse{Slot: result.Slot, Replaced: result.Replaced}, nil
}
```

`ctx.PlayerID` 是玩家业务 ID；Nest 需要编码 Entity Kind 后的完整 Entity ID，因此不能直接把 PlayerID 当 Entity ID 传给 Sender。Controller 只做接入编排：依赖可变实体状态的校验必须放进下面的 handler，避免“检查时有装备，真正写入时已被另一个请求拿走”。

## 4. Nest handler：在实体保护下执行

以下是现有 [装备 handler](../../demo/game/handler/equip_item.go.tmpl) 的业务过程。`equipment`、`apperrors` 都是 demo 的业务包，不是需要新增到 core 的公共抽象。

```go
//roost:nest rollback=undo durability=strict
func handlerEquipItem(
    target player.IPlayerFullEntity, itemID int64,
) (equipment.Result, error) {
    slot, wearable := equipment.SlotFor(itemID)
    if !wearable {
        return equipment.Result{}, errcode.Wrap(
            apperrors.ErrItemUnknown, nil, "item_id", itemID)
    }
    if _, err := target.BagComp().RemoveItem(itemID, 1); err != nil {
        return equipment.Result{}, err
    }
    _, replaced, err := target.EquipmentComp().Equip(itemID, 1)
    if err != nil {
        return equipment.Result{}, err
    }
    if replaced != 0 {
        if _, err := target.BagComp().AddItem(replaced, 1); err != nil {
            return equipment.Result{}, err
        }
    }
    target.AttributeComp().RefreshGear()
    return equipment.Result{Slot: slot, Replaced: replaced}, nil
}
```

业务在 handler 中操作组件，不手动获取 Entity 锁。移出背包、装备替换、旧装备放回和属性刷新共用事务；`strict` 等待 WAL 持久化，不等于 Mongo 投影已经完成。生成 Sender 的参数是实体 ID，handler 收到的是对应实体接口；多个实体的例子见 [AddExp Controller](../../demo/game/controllers/player/add_exp.go.tmpl) 和 [handler](../../demo/game/handler/add_exp.go.tmpl)。

## 5. Agent 能否发送响应

能。常规写法就是在 Controller 中返回 `&pb.EquipResponse{...}, nil`，生成的注册链负责编码，并带原请求序号回给当前连接。`playeraccess.Context` 本身没有接收任意 PB 对象的 `SendResponse` 方法。

需要手动回包时，现有 `ctx.Session.Reply` 接收 `playeraccess.Response`，不是原始 PB 对象。例如在 `HandleEquip` 内：

```go
payload, err := pb.MarshalEquipResponse(&pb.EquipResponse{Slot: 1})
if err != nil {
    return nil, err
}
err = ctx.Session.Reply(ctx.Context(), &playeraccess.Response{
    MessageID: msgid.MsgEquip,
    Sequence:  ctx.Seq,
    Payload:   payload,
})
return nil, err // 已手动回包，不能再返回一个响应对象。
```

`msgid` 是业务工程的 `protocol/msgid` 包。响应沿原连接发送，连接关闭后可能失败；它不是发给该玩家所有连接的 Push。异步任务还要自行管理上下文取消、连接生命周期和只响应一次。本轮保留现有回包接口，没有新增隐式协程或自动去重机制。

## 6. 并发与慢操作边界

Controller 可能并发执行，不是每个玩家一个 Actor，不要在 Controller 字段里无保护地存可变玩家状态。Nest handler 则按声明的目标实体保护业务执行；长短业务池都不能同步等待另一条业务消息，也不能等待 I/O。适用的无回滚 memory handler 可用 [Await](NEST-AWAIT.md) 拆分锁外查询与恢复；本篇的 undo 事务 handler 不能直接套用 Await。

接入层生成目录是 `game/playeraccess`（包名 `playeraccess`，2026-10-14 起；此前叫 `game/playeraccess`，只改名、行为不变，升级步骤见 CHANGELOG）。维护应以生成器和 demo 模板为源，避免直接修改随后会被覆盖的生成文件。

## 7. 玩家看自己的状态（self_sync）

DAO 字段标了 `sync`、想让客户端收到“自己的金币 / 道具变化”时，不需要自建 `entitysync.Manager`：在 `roost.yaml` 打开开关，生成器写出全部接线，生命周期在 core。只看自己；附近的人、队友等多人视图仍用 [Interest / AOI 政策](guide/04-sync.md)（game-demo 的场景）。

```yaml
access:
    player:
        service: game
        transports:
            - tcp
        self_sync: true   # 默认关
```

打开后只需要一步：**客户端要看的 DAO 字段写 `dao:"persist,sync"`**，只经生成的 setter 在 Nest handler 里改，然后 `roost project sync`（或 `roost generate`）。不需要手动标脏：handler 提交时 Nest 自动取走生成实体记下的客户端同步变化（`TakeEntitySyncChanges`），一次提交一帧增量。不要照 game-demo 场景的写法调 `MarkSyncDirty(dao.DirtyTracker().TakeSyncDirty())`：那是自建 Manager 才需要的，`TakeSyncDirty` 取走的是服务间同步的独立掩码，会偷走那一侧的读者；对客户端只会多标一次、帧数不变。

Player 的 `//roost:entity` 标记不用改，也不用写 packer（维护者 2026-10-17 决定）：

- 开关打开时生成器把 Player 当作 `sync=true`（没写 `syncNamespace=` 时取 `"player"`）；标记里显式写 `sync=false` 与开关矛盾，拒绝。
- 标记没写 `subjectPacker=` 时生成 `game/entities/player/player_sync_packer_gen.go`（生成器所有，`roost generate` / `roost project sync` 会重写）：`NewPlayerSyncPacker`，视图集只有自身视图 `self`（`policy.SelfProfile`），包含 Player DAO 的**全部** sync 字段；字段表运行期从 DAO 取，DAO 增删 sync 字段自动跟随；未知视图返回 `entity.ErrSyncViewUnknown`。wire 文件的 `PackerFactory` 指向它。
- **代价：标了 sync 的字段默认全部对玩家本人可见。** 字段要标 `sync`（例如别的同步链路要用）却不想给玩家本人看时，写自己的 packer 把它裁掉。

就绪检查（`project sync`、`generate`、`doctor` 的 `self-sync` 项）的拒绝都写明改法：Player 不存在（`roost add entity Player`）、Player 没有 DAO（`roost add dao Player --entity Player`）、Player 有多个 DAO 而没写 `subjectPacker=`（默认 packer 只发一个 DAO）、显式 `sync=false`、缺推送编号文件。Player DAO 没有任何 sync 字段时不拒绝，doctor 给 warn、`roost generate` 打印 `warning:`：工程照常编译，客户端收不到任何东西。

需要自定义视图（AOI 里别人看到的视图、按字段权限、多个 DAO）时自己写 packer，视图集必须认识 `self`，然后在标记上写 `subjectPacker=NewPlayerSyncPacker`（开关打开时不必再写 `sync=true`；写上也行）。生成器随之不再生成、并删掉以前生成的那份；去掉 `subjectPacker=` 后它回来。业务自己的文件即使叫 `player_sync_packer_gen.go`（没有生成头）也不会被删除或覆盖，需要生成时生成器拒绝并提示改名。示例：玩家本人只看金币和等级（其余 sync 字段不给本人）：

```go
var playerViews, playerViewsErr = dataengine.NewSyncViewSet(db.PlayerDaoSyncFields(),
	entity.NamedSyncView{Profile: policy.SelfProfile, Fields: []string{"gold", "level"}},
)

func NewPlayerSyncPacker(value entity.IThreadSafeEntity) entity.SubjectSyncPacker {
	owner, _ := value.(*Player)
	return playerSyncPacker{player: owner}
}

type playerSyncPacker struct{ player *Player }

func (p playerSyncPacker) PackSubjectSnapshot(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
	return p.pack(profile, dataengine.AllFields)
}
func (p playerSyncPacker) PackSubjectDelta(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
	return p.pack(profile, mask)
}
func (p playerSyncPacker) pack(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
	if p.player == nil || playerViewsErr != nil {
		return entity.FrozenSyncPayload{}, fmt.Errorf("player sync packer: %v", playerViewsErr)
	}
	view, ok := playerViews.Lookup(profile)
	if !ok {
		return entity.FrozenSyncPayload{}, fmt.Errorf("%w: %+v", entity.ErrSyncViewUnknown, profile)
	}
	if mask &= view.Fields; mask == 0 {
		return entity.FrozenSyncPayload{}, nil
	}
	raw := p.player.Dao().MarshalSync(mask)
	if len(raw) == 0 {
		return entity.FrozenSyncPayload{}, nil
	}
	return entity.TakeFrozenSyncPayload(1, raw), nil
}
```

`db.PlayerDaoSyncFields` 只在 DAO 有 sync 字段时生成；视图里的字段名取 DAO 的字段名或 bson 名，写了不存在的名字时 `NewSyncViewSet` 报错，packer 每次打包都返回这个错误。AOI 里别人看到的视图属于 Interest / AOI 政策的 Manager；同一个 Player 只能登记在一个 Manager，见下文“一个 Player 一个 Manager”。

生成物（除推送编号外都在 `roost project sync` 会重写的文件里，关掉开关即恢复原样）：

| 文件 | 内容 |
| --- | --- |
| `internal/bootstrap/generated.go` | `PlayerSelfSync = selfsync.MustNew(...)`；Nest 改为 `kitnest.NewModWithEntitySync(EntityAccess, PlayerSelfSync.EntitySyncSetup())`；`accessplayertcp.NewMod().WithSelfSync(PlayerSelfSync)` |
| `internal/access/player/tcp/server_gen.go` | `Mod.WithSelfSync`：Provide 时交出推送能力，Start 时装握手钩子；`IngressMod.WithSelfSync`：独立 Gate 部署下把 OnActive / OnClosed 接到 Game Ingress，并把 Game Ingress 的载荷上限交给自身视图（`LimitFrameBytes`） |
| `protocol/def/self_sync.go` | 推送编号 `SelfSync()`（业务文件，只在缺失时创建；取所在协议组的上限，冲突时改这里的 id） |
| `game/entities/player/player_sync_packer_gen.go` | 标记没写 `subjectPacker=` 时的默认 packer（entity 生成器写出；写了 `subjectPacker=` 或关掉开关时删除） |

运行时（实现见 [impl/04-sync](impl/04-sync.md) 的 `policy.SelfView` 与 `wiring/selfsync`）：

- 鉴权 ACK 写出之后登记连接，下一个同步 tick 开会话、把玩家绑定到自己的 Player（视图 `self`），第一帧是全量；之后每次提交的变化是增量。首个全量总在 ACK 之后。
- 实体不在内存（首登还没 `GetOrCreate`、还没被请求加载）时会话照开，实体一出现就送全量；登录 endpoint 里 `lifecycle.GetOrCreate` 是常见写法。
- 断线重连、同一玩家的新连接（顶号或多端）都从全量开始；旧连接关闭不带走新连接的视图。全部连接断开后关会话、注销 subject。
- 推送只入出站队列、不等写出。**跟不上就断开**：一帧被活着的连接拒收（嵌入 TCP 的出站队列满、独立 Gate 下 Game Ingress 的可靠队列满或绑定失效等）时，关闭这名玩家的全部连接，Manager 关掉同步会话；客户端照常重连，新连接从全量开始。不会出现“连接还在、却再也收不到自己的状态”。例外是整个进程的出站驻留预算（独立 Gate 下 Game Ingress 的 `outbound.resident_bytes`、嵌入 TCP 的 `player_access.tcp.outbound.resident_bytes`）用尽：那不是哪个玩家慢，这一轮整体重试，连接和会话都不动，预算恢复后照常收增量。规则在 `wiring/selfsync.PushPlayerFrame`，与 game-demo 场景共用。
- **一帧不超过接入层上限，不用手工对齐**（2026-10-20）：自身视图一帧的上限在 Nest Mod Init、Manager 创建之前定下，取接入层的上限——嵌入 TCP 是 `player_access.tcp.max_payload_bytes`（Nest Mod 经 `EntitySyncSetup.LoadConfig` 读同一个键，声明与生成的 TCP Mod 逐字相同，App 启动前与 `--check-config` 一起检查）；独立 Gate 是 Game Ingress 的 `Config.MaxPayloadBytes`（生成的 `IngressMod.WithSelfSync` 交给它）。`selfsync.Config.MaxFrameBytes` 只能在此之下再收紧。以前调小 `max_payload_bytes` 而帧上限仍是 1 MiB 时，超限帧被接入层拒收、连接被关，重连后的全量同样超限，反复断线重连。现在单个玩家实体的帧超过上限时在 Manager 内编码失败，不会推出被接入层拒收的帧；2026-10-22 起（维护者决定 A，取代 10-17 的“不关连接、靠滞留指标”）随即断开该玩家（`selfsync.DisconnectLostPlayer`，`player.sync.session_lost_disconnect.total{reason="too_large"}`），实体一直超限时会反复断线重连（KNOWN-LIMITS 自身视图第 3 条）。
- 客户端要把“连接被服务端关闭”当成普通断线处理：重连、重新登录，等第一份全量。监控看 `player.sync.push_refused.total{outcome}`（`closed` 是因拒收断开的次数，`retried` 是预算耗尽的整轮重试次数）、`player.sync.session_lost_disconnect.total{reason}`（同步会话因编码失败被丢而断开的次数，正常为 0）与 `entitysync.self_view.stranded_players`（在线却超过 3 个同步周期没有会话的玩家数，正常为 0）。

客户端：包头载荷类型是 Sync、消息编号是 `msgid.MsgSelfSync` 的推送交给每连接一个的 `frame.Receiver`，组件数据用 `entitysync.DecodeSubjectUpdate` 取出，载荷交给 DAO 的 `ApplySync`。连接上第一份全量之前的增量被拒（`frame.ErrBaselineMismatch`），跳过等全量即可。Go 写法见 `wiring/selfsync/example_client_test.go`（随测试运行）。C# 本轮没有自身视图的解码样例：`Roost.Client.SyncReceiver` 能收帧，subject 载荷的解码需业务自写。

一个 Player 一个 Manager：同一个 Player 的同步状态（`SubjectSyncState`）只能登记在一个 Manager——脏通知器、冻结内容与在途捕获在状态上各只有一份。2026-10-20 起第二个 Manager 登记它时被拒绝（`entitysync.ErrSubjectRegisteredElsewhere`，信息点名实体、命名空间与已登记它的 Manager，按 `ManagerConfig.Name`：自身视图是 `self_sync`、Nest Mod 缺省 `nest`、game-demo 场景 `scene`），以前是静默抢走对方的变化通知。那个 Manager 注销、退役完成（remove 已交付或订阅者会话已关闭）之后可以登记到别的 Manager；同一个 Manager 重复登记仍是空操作。所以工程里另有自建 Manager 也登记 Player（game-demo 的场景）时不支持打开 self_sync：game-demo 打开后启动在 `service game init` 失败（场景接卸载后重载时报 `unload resync is already configured for entity sync manager "self_sync"`）；不经那条接线的自建 Manager 在登记 Player 时被拒（自身视图被拒时记 Warn 并计入 `entitysync.self_view.stranded_players`）。
