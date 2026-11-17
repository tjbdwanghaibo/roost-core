# 回合 / 经营 Robot

复用公共 Robot、player-tcp 和本工程生成 PB；宿主负责鉴权、建立房间（或好友关系）、提供实际端点。身份只取鉴权会话，连接标识取服务端 Session，不允许请求指定操作者。

```sh
go run ./cmd/robot -endpoint 127.0.0.1:7000 -steps steps.json -timeout 30s
```

票据通过 `ROOST_ROBOT_TICKET` 注入，不写进步骤文件。步骤是 `[]loadtest.Step`：Request 为 `pb.ScenarioCommandRequest`（RoomID、Seq、Revision、Kind、Choice、Epoch），WantCode 为预期业务码，Revision/Reward 为成功响应预期值，Reconnect 为动作前重建连接。

- 回合 PvP（房间玩法）：RoomID 是房间号（room 的唯一号，与 room 自带协议同一口径；服务端经 `room.EntityID` 换算，不收 Entity ID）。每条新 TCP 连接的第一步是接管席位 `{"Request":{"RoomID":123,"Kind":"reconnect","Epoch":<RoomJoin 拿到的代际>}}`（服务端转 room 的 RoomReconnect，应答带新代际）；之后的步骤 Epoch 填 0 即沿用新代际。例如新房间第一拍：`[{"Request":{"RoomID":123,"Kind":"reconnect","Epoch":1}},{"Request":{"RoomID":123,"Seq":1,"Revision":0,"Kind":"attack"},"Revision":1,"Reward":1}]`。Revision 是已消耗的回合数（turn 的回合号减 1）。
- 社交经营：RoomID 是农场主人的玩家号（唯一号，服务端换成 Player 的 Entity ID）；主人首动作为 plant，好友访客的动作为 help。没有席位，不发 reconnect。

房间身份、当前序号、代际和版本必须从真实业务视图获得，示例 ID 不可直接用于线上。双人轮流操作时使用两份票据和各自的步骤，遵守服务端回合 / 版本；社交收获必须等服务端 ReadyAt。Robot 不可推进服务器时间或自报奖励。`loadtest.Commands` 可供业务组合多个身份和动态步骤；重放同一回执不能用作写入 TPS。

`game/handler/robot_complete_test.go` 展示闭环、重试、换连接接管与越权断言。默认验证是嵌入 TCP/PB → 正式 Nest/DAO，鉴权/提交器为替身，可信时间由测试拨动；不代表完整 Gate、实际存储恢复或生产容量。对线上部署还需加入业务查询/Sync 的最终状态断言。
