# Roost Codegen 小白逐步操作手册

目标：不使用 preset，不要求先理解框架全貌。工具每次只告诉你一个安全动作，你完成后再询问下一步。
新项目还会生成一份带真实项目名和 Service 名的 `docs/BEGINNER_WORKBOOK.zh-CN.md`。

## 1. 安装与自检

```bash
go install github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest
roost version
roost env doctor
roost help
roost help beginner
```

如果找不到 `roost`，执行 `go env GOBIN GOPATH`。`GOBIN` 非空时把它加入 PATH；否则加入
`GOPATH/bin`。重新打开终端后再运行 `roost help`。Linux 生产开发机还应安装 Git、GNU Make 和 Docker。

## 2. 创建最轻量的学习项目

```bash
roost project new planet \
  -module github.com/your-name/planet \
  -mods configdata
cd planet
roost project next
```

`project new` 需要访问 Go Proxy/GitHub，以正式发布模块解析 roost-core 最新版本（wiring、skill 与生成器都是其中的包）。网络错误先解决
GOPROXY/DNS，不要提交本地绝对路径 `replace`。

`project next` 输出六行左右：当前 workflow、进度、唯一的 next 命令、原因、复查命令、详细文档。next 命令里的名字按工程现状填（已有的 Entity、Component、handler、controller 域）；工程里还没有时给示例名，命令后带 `# example name, use your own`，换成自己业务的名字再执行。
执行 next 后再次运行它。它在阶段未完成时也返回成功，适合新手反复调用。

## 3. 第一条业务链

完整顺序如下，但不要一次盲目执行；字段编辑点必须完成：

```bash
roost add access player --service game
roost add entity Player
roost add component Profile --entity Player
roost add dao Player --entity Player
roost add handler RenamePlayer --entity Player --component Profile
roost add protocol RenamePlayer --group game --handler player
# 编辑 DAO、Component、handler 与 Request 后
roost add endpoint RenamePlayer --handler player
roost add lifecycle Player --service game
# 登录边界：客户端第一条请求先 EnterGame，保证玩家 Entity 已存在
roost add protocol EnterGame --group game --handler player
roost add endpoint EnterGame --handler player --lifecycle Player
roost generate
roost project doctor --workflow first-business
go test ./...
```

`add access player` 会给 game 的 `configs/service/config.game.yaml` 与 prod 示例补上 Nest 带进来的 `nest`、`dataengine`、`mongo`、`nats` 配置段，并打开单实例锁（`singleton.enabled: true`，补 `redis:` 段）：启动 game 之前把这些地址改成自己的 Mongo 副本集、NATS 与 Redis（`deploy/dev/docker-compose.yaml` 带齐三者）。

为什么要 EnterGame：Nest handler 只处理已存在的 Entity。从未登录过的玩家直接发 RenamePlayer，Nest 冷加载在
Data Engine 里找不到他，客户端收到稳定码 `entity_not_found`（`errcode.CodeEntityNotFound = 2`），服务端不记
ERROR。`--lifecycle Player` 生成的 endpoint 对 `context.PlayerID` 调 `lifecycle.PlayerFromRegistry(...).GetOrCreate`：
权威里确认不存在才创建，数据库 / 解码 / 超时错误照常返回，不当成“不存在”。已删档（DAO 文档只剩墓碑）的玩家
这个 ID 不可再用：玩法请求与 EnterGame 都收到 `entity_deleted`（`errcode.CodeEntityDeleted = 3`），GetOrCreate 不会重建
（维护者 2026-10-17 决定，DESIGN-TODO D10）。客户端收到 `entity_not_found` 时重新走 EnterGame 再重试即可；收到
`entity_deleted` 时不要重试，提示角色已删除。`roost project doctor --workflow first-business` 的
`workflow:login` 检查这一步。

业务职责：

| 层 | 写什么 | 禁止什么 |
| --- | --- | --- |
| Protocol | 客户端 Request/Response | 玩法和数据库访问 |
| Endpoint | Request 到生成 Sender 的参数映射 | 直接拿 EntityManager |
| Nest handler | 一次业务命令和 durability | 自己给 Entity 再加 mutex |
| Component | 校验与玩法方法 | socket/PB/数据库驱动细节 |
| DAO | 持久化/同步字段声明 | 绕过生成 mutator 直接改字段 |
| Lifecycle | 登录加载、首次创建、销毁 | 每个普通请求都 GetOrCreate |

业务进入 handler 前 Nest 已经持有 Entity mutex。DAO 的 `SetXxx/UpdateXxx/RemoveXxx` 同时维护
dirty、undo、patch 和 sync；字段已私有化，使用 `GetXxx` 读取。

生成项目内的 workbook 会给出可复制的 RenamePlayer 字段、Component 方法、handler 和 Request 示例。

## 4. 接入 TCP

第一条业务完成后，如果不使用独立网关：

```bash
roost add transport tcp
roost project next --workflow player-tcp
```

配置会自动以 `enabled: false` 增量加入 Service 配置和生产示例，不需要复制 YAML。生成的
`internal/access/player/tcp/auth.go` 归业务所有，默认拒绝所有连接。必须验证服务端签名、有效期、登录代次、
设备和重放信息，返回非零 PlayerID 与非空 SessionID；固定 PlayerID、accept-all 都不能用于生产。

鉴权完成后：

```bash
roost config enable player-tcp
roost project doctor --workflow player-tcp
go test -race ./...
```

`enable` 会先拒绝默认鉴权骨架，只修改 `enabled` 标量。停流使用：

```bash
roost config disable player-tcp
```

帧格式、超时/连接硬上限、主动 `PushPlayer/PushSession`、监控和压测项见
[Player TCP 接入](PLAYER_ACCESS_TCP.zh-CN.md)。

## 5. 日常操作不用背

生成 Makefile 提供：

```bash
make next                                      # 自动选择当前工作流
make next NEXT_WORKFLOW=player-tcp             # 指定 TCP 工作流
make new-entity NAME=Player
make new-component NAME=Profile ENTITY=Player
make new-dao NAME=Player ENTITY=Player
make player-tcp-enable SERVICE=game
make generate
make doctor
make test
make ci
```

记不住命令时：`roost help` 看能力目录，`roost help <能力>` 看参数和例子，`make help` 看项目快捷入口。

## 6. 报错排查

按固定顺序：

```bash
roost project next
roost project doctor --workflow first-business
roost generate --check
go test ./...
go test -race ./...
go vet ./...
```

- generated stale：运行 `roost generate`，不要手改生成物；
- Nest 缺失：按错误给出的 `roost add mod nest --service ...`；
- endpoint 参数找不到：让 Request 字段名与 handler 除 Entity 外的参数名一致；
- 客户端收到 `entity_not_found`（code 2）：玩家还没有 Entity，先调 EnterGame（`roost add endpoint EnterGame --handler player --lifecycle Player`）；
- 客户端收到 `entity_deleted`（code 3）：这个玩家删过档，ID 不可再用，EnterGame 也不会重建；
- DAO 字段不可读：使用生成的 getter；
- player TCP 无法 enable：先完成 auth.go，不要绕过检查；
- 生产配置检查失败：替换 CHANGE_ME/localhost/dev 默认值，Secret 不进 Git。

## 7. 发布前

```bash
make deps-update
make ci
roost config check --production --file <实际生产配置>
```

然后完成真实基础设施、数据库恢复、WAL/PVC、TLS/LB、FD/连接限制、SIGTERM、灰度和回滚演练。
“能够启动”不是生产验收完成；以生成项目的 `docs/DEPLOYMENT.zh-CN.md` 为准。
