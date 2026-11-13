# rules：把各功能给客户端看的规则数据打成一份快照，进入游戏时推送、随时可补取

“规则”指客户端显示时要用、但不属于某个玩家状态的数：体力上限与恢复周期、商店的商品与价格等。rules 自己不带任何规则，它只提供一个收集口和一条下发通道。

## 能力

- **客户端协议**（`protocol/def/rules.go`）
  - `RulesNow`（推送 `RulesNowPush{Parts}`）：一份完整的规则快照。每个 `RulePart{MessageID, Payload}` 是一个功能的规则，`Payload` 用 `MessageID` 对应的生成 PB 解码器解开。
  - `RulesGet`（请求 `RulesGetRequest{}`，应答 `RulesGetResponse{Code, Reason, Rules}`）：补取同一份快照，`Rules` 与推送是同一个结构。
- **服务端入口**
  - `PushNow(ctx, registry, playerID) error`（`game/controllers/rules/controller.go`）：给一名玩家推一次 `RulesNow`。先经 Nest 读快照，回到调用方之后再发网络消息。
  - `Controller.HandleRulesGet`：`RulesGet` 的端点。读取失败时应答里带 `Code` / `Reason`，不返回半份规则。
  - Nest handler `handlerRulesRead`（`game/handler/rules_read.go`）：只读，没有写入与落库；生成的 Sender 是 `sender.NewRulesReadSender(client).Sync_RulesRead(ctx, playerEntityID)`。
  - `Player.RulesSnapshot() (*pb.RulesNowPush, error)`（`game/entities/player/rules.go`）：在 Player 锁内调用，按登记名排序依次读各提供方。
- **登记点**（`game/entities/player/rules.go`）
  - `RegisterPlayerRules(name string, read RuleProvider)`，`RuleProvider` 是 `func(view.PlayerEntity) (*pb.RulePart, error)`。各功能在自己的 `with/rules` 文件的 `init` 里登记；运行期不增删。空名、nil、重名在启动时 panic。
  - 提供方在 Player 已被 Nest 锁住时被调用（`handlerRulesRead` 这一条消息里），只读、不做 I/O，用自己的生成 PB 编码器产出 `Payload`。它可以读同一个 Player 上的别的组件（体力规则随等级变化时就是这样读的）。
  - 进入钩子：模块在 `init` 里登记了 `gamelogin.OnEntered("rules", …)`，玩家每次成功进入游戏（含重连）调一次 `PushNow`。
- **状态**：无。不新增持久字段，也不启动独立服务。
- **配置**：无。
- **错误码**：无自己的业务码。

## 能力边界

- **不做的事**：不带任何具体规则；不做配置热更的广播（服务端规则变了不会主动再推）；不做大目录的分页下载；不重复下发业务日与重置时刻（那是 servertime 的事）；不下发玩家状态（体力当前值、已购计数继续走 Player 的自身同步）。
- **一致性**：所有提供方在同一条 Nest 消息、同一把 Player 锁、同一份配置快照里读取，所以一份快照内部是一致的。快照里的字节是复制出来的，不随提供方后续调用变化。任何一个提供方出错、返回空、`MessageID` 为 0 或与别人重复，整份读取失败，不下发半套。
- **推送是尽力而为**：进入时的推送失败只由 enter 的钩子执行器记一条日志，不撤销进入；客户端没收到、从后台恢复，或等级 / 配置可能变了，自己发 `RulesGet`。
- **规则只管显示**：购买、消耗仍以服务端当时的权威规则为准。展示用的快照与随后的业务请求是两次请求，中间配置可能已经热更；商店协议没有“期望价格”校验，不能把旧界面上的价格当成成交承诺。
- **大小**：没有条数与字节上限的检查，也没有分页；过大的目录应由工程拆成单独的查询，不会被静默截断。
- **未加载的玩家**：场景 `TestRulesQueryForAnUnloadedPlayerDoesNotCreateIt` 断言 `RulesGet` 对没有加载的 Player 应答带错误码、不产生提交。未确认：这条场景里的实体获取器是只认已有实体的替身；真实运行时 Nest 的获取器遇到未加载的 Player 会不会去加载它，没有在本模块里验证。
- **前提**：依赖 enter 模块与 nest Mod；推送要求玩家 TCP 传输已装配（按能力名 `access.player.tcp` 取）。只对本进程持有的 Player 有效。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md`）：“模块的规则大多还是 Go 写的业务文件”一条写明 rules 尚无热更广播与大目录分页；“业务模块目录”一条给出场景数的权威来源。

## 复用方式

- **需求正好是它**：`roost module add rules`，连带装上 enter。单独装时没有提供方，快照是空的。shop、stamina 依赖它，装它们时自动装上并登记各自的规则。
- **需求多一点——让自己的功能也下发规则**：
  1. 在功能自己的协议定义里声明一条只有推送形状的消息（照 `StaminaRules` / `ShopRules` 的写法，`handler=rules`），拿到一个消息号与 PB 编码器。
  2. 在功能自己的组件包（`game/components/<功能>/`）里加一个文件，`init` 里调 `gamerules.RegisterPlayerRules("<功能名>", func(owner view.PlayerEntity) (*pb.RulePart, error) { … })`，返回 `&pb.RulePart{MessageID: msgid.Msg<你的消息>, Payload: payload}`。
  3. 不要改 `protocol/def/rules.go`、`controller.go` 与 `rules.go`：新增功能不需要改公共快照类型。
- **服务端别处要主动推一次**（例如升级后规则变了）：`rules.PushNow(ctx, registry, playerID)`，在 Nest 事务之外调用；返回的错误记日志即可。
- **客户端处理**：`RulesNowPush.Parts` 收到后整份替换；按 `MessageID` 分派到对应的解码器，不认识的 `MessageID` 忽略；内层 `Payload` 是本地解码用的数据，不要当成一条业务请求再发给服务器。示例：

```go
for _, part := range snapshot.Parts {
    switch part.MessageID {
    case msgid.MsgStaminaRules:
        rule, err := pb.UnmarshalStaminaRulesPush(part.Payload) // Max / RegenMillis 用来推算体力
        if err != nil { return err }
        updateStaminaDisplay(rule)
    case msgid.MsgShopRules:
        rule, err := pb.UnmarshalShopRulesPush(part.Payload) // Goods 已按商品号排序
        if err != nil { return err }
        updateShopDisplay(rule)
    }
}
```

  两个 update 函数由客户端界面实现；C# / C++ 使用同一份生成的 PB 定义。
- **不适合直接用的情形**：需要按版本增量下发、热更后广播给所有在线玩家、或单份规则很大。值得借鉴的是“提供方按名字登记、在一把实体锁里读完、解锁后再做网络 I/O”的分层。

## 联动

- `shop/with/rules`：登记名 `shop`，导出未下架的商品（`ShopRulesPush`，按商品号升序）。2 条场景。
- `stamina/with/rules`：登记名 `stamina`，导出上限、硬顶、恢复周期与每日可购次数（`StaminaRulesPush`）。2 条场景。
- rules 自己没有 `with/` 目录。它对 enter 的使用（`gamelogin.OnEntered`）写在主体文件里，因为 enter 是它的依赖。

## 验证范围

- `roost module verify rules`：10 条场景（`^TestRules`，包 `./game/entities/player`、`./game/handler`、`./game/controllers/rules`）。覆盖：查询经 Nest 且不产生提交、未加载的玩家不被创建、缺依赖时查询与推送都拒绝、推送与查询是同一份快照且发给正确的玩家、进入与重连各推一次、推送失败后可用查询补回、提供方有序且快照自有字节、错误登记在启动时失败、重复 `MessageID` 整份拒绝、提供方错误保留原因。
- 场景使用真实的 Nest 引擎、生成的 DAO / PB，提交器与玩家传输是内存替身。真实 TCP、独立 Gate、WAL 的联合路径没有验证。
- 仓库侧的组合验收入口是 `codegen/internal/roost` 里的 `TestModuleClientRulesAndShopTable`（rules 与 shop 配置表一起装）。
