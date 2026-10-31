# 业务 Robot

这是应用自己的场景，复用 roost-core 的公共 Robot、Player TCP 和生成 PB，不复制一套网络库。

`protocol/def/robot.go` 定义业务请求，`game/robotaccess.Dispatch` 将已鉴权身份与请求交给正式 Sender。宿主必须自己装配 Dispatcher、账号鉴权和实体/房间初始化；生成骨架不会自动监听一个可玩的游戏。不要把测试中的固定票据鉴权器用于生产。

```sh
go run ./cmd/robot -endpoint 127.0.0.1:7000 -steps /path/to/steps.json -timeout 30s
```

票据从 `ROOST_ROBOT_TICKET` 环境变量读取。步骤文件是 `[]loadtest.Step`：每项有 `Request`（本工程生成的 `pb.PlayRequest` 字段）、`WantCode`（0 成功、1 业务拒绝）、`Reconnect`（该步骤前重建 TCP 会话）。例如通用外形为 `[{"Request":{"Seq":1,"Kind":"<本玩法命令>"},"WantCode":0}]`；不能原样执行占位符。具体合法字段和闭环示例见 `game/handler/robot_network_test.go`。RoomID、Epoch、Round、Base 等从当前服务端业务视图取得，不复制测试 ID 到线上。

配置文件使用 JSON，网络业务载荷为生成 PB。Seq 是业务请求号；线序列由 transport 单独维护。重试沿用原业务参数，各玩法幂等键不同，不能认为仅有 Seq 就实现了永久去重。连接成功也不等于业务重连完成：宿主若切换 epoch，必须先恢复当前业务视图，再构造下一步。

`cmd/robot` 是单账号功能 smoke。扩展容量时调用 `loadtest.Commands`，给每个机器人分配独立身份和当前业务步骤；不能把同一个幂等请求重复命中回执的速度称为写入 TPS。当前响应验证 Code/Seq，最终 DAO 断言在生成测试内；真实部署需要增加业务查询或同步视图断言。

`go test -race -run RobotBusiness ./game/handler` 实际走 loopback TCP/PB → Sender → Nest → DAO，覆盖该玩法的对局或经营闭环、结算重试、TCP 重连与拒绝路径。可信时间/敌方推进只由服务器测试驱动。提交器与鉴权是替身，不代表真实 Gate、账号服务、WAL/Mongo、EntitySync、业务重登和容量已经验收。
