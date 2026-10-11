# 业务层如何处理玩家协议

玩家接入层已经由生成器提供，不需要再创建一个 Session 服务。本篇以 demo 中的“穿戴装备”为例：客户端只提交物品 ID，服务器从已认证连接取得玩家 ID，通过 Nest 修改背包和装备，最后返回响应。

## 1. 请求经过哪些层

```text
客户端 PB 请求
  → Gate / Game TCP（连接与认证）
  → player_agent.ProtocolRegistry（协议校验、解码、请求上下文）
  → player.Controller.HandleEquip（业务入口）
  → syncsender.EquipItemSender（生成的 Nest 调用）
  → handlerEquipItem（Entity 保护下的业务和事务）
  → Controller 返回 PB 响应 → 接入层编码、回包
```

独立 Gate 部署在 Gate 与协议派发之间还有 NATS 和 GameIngress；嵌入式部署直接使用本地 TCP 接入。业务 Controller 的写法相同。这里的 `player_agent.Context.Session` 是 `gateway.Session`，不是管理副本运行的 `service/session`。

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

启用 player access 后，生成器还会按 `handler=player` 在 `game/controllers/player` 自动补齐 `Controller`、`FromRegistry` 和缺少的 `HandleXxx`。请求参数是 `*player_agent.Context` 与对应 PB 请求；有响应的协议返回 `(*pb.XxxResponse, error)`，单向请求只返回 `error`。新方法先返回“not implemented”错误，必须补业务才能提供服务；该错误会进入接入层的错误处理，不是成功响应。

这些骨架生成后属于业务代码：重复生成、`--force`、项目同步都不覆盖或删除已有实现。方法可以移动到同包其他 Go 文件；生成器按 Controller 的方法声明查重。已有类型与 `FromRegistry` 必须配套；同名文件占用但缺少对应方法时会明确报冲突。协议删除或签名变更后，业务方法由开发者调整，编译负责校验类型是否仍匹配。`roost generate --check` 会报告缺失的骨架。

此过程不要求先写 Nest handler，也不会自动替你调用 Nest。首次生成的 Controller 提供 `NestClient()`，你可按下一节调用 Sender。已有 `roost add endpoint` 仍适合显式生成连接到 Nest handler 的方法；已自动生成骨架后，直接编辑该方法即可。

## 3. Controller：把协议转换成业务调用

以下方法与 demo 的真实接口一致，放在业务工程 `game/controllers/player` 包中。依赖已有 Controller 的 `NestClient()`；`player` 指业务工程的 `game/entities/player`，`syncsender` 指 `game/handler/syncsender`，`player_agent` 指 `game/player_agent`，`pb` 指 `protocol/pb`。完整 imports 见 [Controller 源码](../../demo/game/controllers/player/equip.go.tmpl)。

```go
func (controller *Controller) HandleEquip(
    ctx *player_agent.Context, req *pb.EquipRequest,
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

能。常规写法就是在 Controller 中返回 `&pb.EquipResponse{...}, nil`，生成的注册链负责编码，并带原请求序号回给当前连接。`player_agent.Context` 本身没有接收任意 PB 对象的 `SendResponse` 方法。

需要手动回包时，现有 `ctx.Session.Reply` 接收 `player_agent.Response`，不是原始 PB 对象。例如在 `HandleEquip` 内：

```go
payload, err := pb.MarshalEquipResponse(&pb.EquipResponse{Slot: 1})
if err != nil {
    return nil, err
}
err = ctx.Session.Reply(ctx.Context(), &player_agent.Response{
    MessageID: msgid.MsgEquip,
    Sequence:  ctx.Seq,
    Payload:   payload,
})
return nil, err // 已手动回包，不能再返回一个响应对象。
```

`msgid` 是业务工程的 `protocol/msgid` 包。响应沿原连接发送，连接关闭后可能失败；它不是发给该玩家所有连接的 Push。异步任务还要自行管理上下文取消、连接生命周期和只响应一次。本轮保留现有回包接口，没有新增隐式协程或自动去重机制。

## 6. 并发与慢操作边界

Controller 可能并发执行，不是每个玩家一个 Actor，不要在 Controller 字段里无保护地存可变玩家状态。Nest handler 则按声明的目标实体保护业务执行；长短业务池都不能同步等待另一条业务消息，也不能等待 I/O。适用的无回滚 memory handler 可用 [Await](NEST-AWAIT.md) 拆分锁外查询与恢复；本篇的 undo 事务 handler 不能直接套用 Await。

接入层名称目前仍为 `player_agent`，本次没有实施 `playeraccess` 改名。后续维护应以生成器和 demo 模板为源，避免直接修改随后会被覆盖的生成文件。
