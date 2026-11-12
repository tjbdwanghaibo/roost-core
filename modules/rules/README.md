# 客户端规则快照

`roost module add rules` 安装规则入口与 enter。shop / stamina 会自动安装它并登记自己的规则；仅装 rules 时可以返回空快照。没有新增持久状态，也不启动独立服务。

每次成功进入游戏（含重连）推送 `RulesNow`。所有提供方在同一条 `RulesRead` Nest 消息、同一个 Player 锁和配置快照中读取；回到调用方后再发网络消息。依赖等级的体力规则也在这条路径读取，网络 controller 不直接操作 Entity。

## 客户端处理

`RulesNowPush.Parts` 是完整快照，收到后整份替换。每项包含 `MessageID` 和 `Payload`，载荷仍是正式生成的业务 PB：

```go
for _, part := range snapshot.Parts {
    switch part.MessageID {
    case msgid.MsgStaminaRules:
        rule, err := pb.UnmarshalStaminaRulesPush(part.Payload)
        // 使用 Max / RegenMillis 与同步状态推算体力。
        if err != nil { return err }
        updateStaminaDisplay(rule)
    case msgid.MsgShopRules:
        rule, err := pb.UnmarshalShopRulesPush(part.Payload)
        // Goods 已按商品号排序，包含每份数量、金币价格、限购类型和份数上限。
        if err != nil { return err }
        updateShopDisplay(rule)
    }
}
```

示例中的两个 update 函数由客户端界面实现。C# / C++ 使用同一份生成 PB 定义。内层 Payload 是供本地解码的数据，不要当作收到了一条业务请求再向服务器发送。客户端不认识的 MessageID 可忽略。

进入推送失败只记日志，不撤销建档；客户端进入后没有收到规则、从后台恢复，或等级/配置可能改变时，用 `RulesGet` 补取。应答的 Rules 与推送是同一个结构。服务端不为每次体力变化重复发送规则，也没有配置热更广播；更新界面前可补取一次。规则只帮助展示，购买、消耗仍以服务端当时的权威规则为准。当前商店协议没有期望价格校验，不能把旧 UI 价格视为成交承诺。

规则包不重复包含业务日和重置时间；这些由 servertime 提供。体力当前值、恢复起点及商店已购计数继续走 Player 自身同步；体力补满的逐次价格仍归业务规则和交易应答，不包含在本次规则快照里。

## 扩展

各功能在 `with/rules/game/entities/player` 调 `RegisterPlayerRules(name, provider)`。提供方只读、无 I/O，使用自己的 PB 编码器返回 `RulePart`；协议号由功能模块主体声明。空名、重名、nil 在启动时拒绝；缺失/重复消息号或提供方错误让整份读取失败，不下发半套配置。返回的数据会复制为独立快照。

协议与 controller 统一放在 rules，功能只写提供方与 PB 数据定义。这里没有 JSON 反射协议，也不需要为新增玩法修改公共快照类型。默认是进入时的一次快照，不是大配置文件分页下载；过大目录应由工程拆分查询，不能静默截断。

验收入口 `TestModuleClientRulesAndShopTable`：实际 Nest、生成 DAO / PB、替身提交器与传输。没有跑真实 TCP/Gate/WAL 的联合路径，按交接文档 §6 在最终批次验证。
