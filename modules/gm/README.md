# gm：运维命令的登记点与审计，复用框架 ops 的 `/admin` 端点

GM 命令是给运维、客服用的可信操作（加币、发补偿邮件、发起换服）。这个模块只提供“登记命令、强制填写操作者与备注、记审计日志、把业务码带回给调用方”这一层；它自己不带任何玩法命令，命令由各功能模块的 `with/gm` 联动提供。HTTP 监听、token 认证、请求期限都沿用框架的 ops Mod，模块不另开端口。

## 能力

### 客户端协议

无。命令不暴露给玩家协议。

### 对运维的接口（框架 ops 提供，模块复用）

- `GET /admin/commands` 列出命令，`POST /admin/execute` 执行，认证用 ops 的 `X-Admin-Token` 或 Bearer。
- 请求体：

  ```json
  {
    "name": "gm.mail.send",
    "operator": "support-17",
    "trace_id": "incident-42-send-1",
    "payload": {
      "player_id": 1001,
      "subject": "故障补偿",
      "body": "维护期间奖励补发",
      "attachment": {"type": "coins", "amount": 100},
      "request_id": "incident-42-player-1001",
      "note": "工单 42 已核对"
    }
  }
  ```

- 每条命令都必须带 `operator`（1～64 字节）与 `payload.note`（1～256 字节），不能是空白，必须是合法 UTF-8；否则答 `gm_invalid`，命令不执行。
- 命令被拒绝时，应答的 `result.data.code` 是业务码；基础设施异常用框架的 internal 码。

### 服务端入口（`game/gm/gm.go`）

- `Register(def Definition)`：登记一条命令。`Definition{Name, Description, Handler}`，`Handler` 的签名是 `func(context.Context, *app.Registry, admin.Command) (admin.Result, error)`。只在 `init` 里调；空名、nil、重名 panic。
- `Replace(registry, def) error`：服务初始化时替换一条已登记命令的业务执行部分，审计与停止后的拒绝保持不变。
- `Start(registry)`：模块的运行时入口（清单 `runtime.start`），启动时把登记的命令按名字顺序装进框架的 admin 命令表（`app.ModAdmin`）；返回的 stop 函数让之后的执行一律答 `gm_unavailable`。没有联动命令时零条命令也能启动。
- 给联动文件用的辅助：`Decode[T](command)`（把 payload 解成结构体，未知字段、多余内容都拒绝）、`Audit`（内嵌进 payload 结构体，带 `note` 字段）、`PlayerIDs(raw, kind)`（接受玩家唯一号或完整 EntityID，返回两种写法）、`Invalid(reason)`（带 `gm_invalid` 码的参数错误）。

### 登记点

就是 `Register`。命令的 `Handler` 在实体锁之外执行；要改实体只能调生成的 Nest Sender，不能直接改 DAO。

### 现有命令（都由别的模块的 `with/gm` 提供，装了那个模块才有）

| 同装模块 | 命令 | payload（都还要带 `note`） | 成功结果 |
| --- | --- | --- | --- |
| wallet | `gm.player.coins` | `player_id, delta`，正数增加、负数扣除，不能为 0 | `balance` |
| bag | `gm.player.add_item` | `player_id, item_id, count` | 该道具的持有量 |
| profile | `gm.player.add_exp` | `player_id, amount`（正数） | `levels_gained` |
| mailbox | `gm.mail.send` | `player_id, subject, body, attachment?, request_id` | `mail_id`、`recipients` |
| rehome | `gm.player.rehome` | `player_id, to` | `accepted`、`transfer_id`、`from`、`to` |
| notice | `gm.notice.announce`、`gm.notice.marquee` | `text, key`（`key` 是稳定业务键，同键内容不可变） | `kind`、`key` |

### 状态与配置

- **状态：** 无。模块不往任何实体上加字段。
- **配置：** 模块没有自己的配置键。要用它得打开 ops 的 admin（按部署环境配置，模块不自动开端口、不生成 token）：

  ```yaml
  ops:
    enabled: true
    addr: 127.0.0.1:9100
    admin_enabled: true
    admin_token: "${OPS_ADMIN_TOKEN}"
  ```

  这里的环境变量写法表示部署时注入，不要把示例字符串当成真实 token。开发用的 `dev-` 前缀 token 需要 `ops.allow_dev_token: true`，生产配置即使写了这个开关也拒绝。

### 错误码（`internal/errors/gm.go`）

`gm_invalid`（命令参数或审计信息不合法）、`gm_unavailable`（admin 命令表不可用、模块已停止、或命令依赖的能力不在）。

**日志**：执行前一条 `gm: executing`（命令、`trace_id`、操作者、备注），执行后一条 `gm: completed`（命令、`trace_id`、操作者、结果码 `code`：成功是 0，被拒是业务码，基础设施异常是 internal），两条按 `trace_id` 对齐；必填检查不过或模块已停止时命令没有开始执行，这两条都不记（框架 admin 层的开始 / 结束日志仍在）。具体玩家变更由各模块的 GM 联动记录。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/gm/gm.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **`operator` 不是认证出来的身份。** 它是持有 admin token 的调用方自报的审计名字；模块不做个人账号、权限分级、双人审批。谁能执行由 ops 的 token 决定，拿到 token 的人能执行全部命令。
- **不做重试与去重。** 加减币、加道具、加经验每调用一次就执行一次；网络超时不等于没生效，不能盲目重发。需要可以重试的补偿用邮件命令：`gm.mail.send` 要求显式、稳定的 `request_id`（1～64 字节，与 `trace_id` 无关），同一件事不管重试几次、换哪个进程都用同一个键。公告两条命令同理靠 `key`：同键重发不多存历史，在线推送可能重复。
- **不创建玩家。** 命令的目标玩家由 Nest 冷加载，没建档的玩家不会被自动创建。
- **审计只是日志。** 每次执行记两条 Info 日志：执行前（命令名、`trace_id`、操作者、备注）与执行后（命令名、`trace_id`、操作者、结果码）；框架的 admin 层另记开始与结果分类。不记 token、不记整份 payload，也不落库——留存多久取决于部署的日志系统。
- **没有批量命令。** 现有命令都是单个玩家；全服发放、按条件筛选没有。
- **换服命令的 `accepted` 只是受理**，最终绑定要查 account 的角色记录。运维入口免玩家冷却与目标白名单，仍受各模块登记的换服检查限制。
- **邮件命令先用一条只读的 Nest 请求确认收件人存在，再在实体锁之外调 mail 服务**；它不改玩家余额，奖励在玩家领取附件时到账。附件里的道具在不在表里、背包有没有空间，要到领取时才核对。
- **前提：** 需要 ops Mod（game 模板自带）；admin 命令表取不到时 `Start` 返回 `gm_unavailable`。命令只作用于本进程能加载到的实体。
- **已知限制：** `docs/maintenance/KNOWN-LIMITS.md` 的“gm 联动边界”：离线场景不替代真实的 ops HTTP、跨进程 mail、真实 DataEngine 交出实体的验收。

## 复用方式

- **需求正好是它：** `roost module add gm`。单装时没有任何玩法命令；同时装着 wallet、bag、profile、mailbox、rehome、notice 中的哪个，就多出上表里对应的命令，两种安装顺序都可以。然后按上面的 YAML 打开 ops 的 admin。
- **给自己的功能加一条命令：** 在工程的 `game/gm/` 下新写一个文件：

  ```go
  func init() {
      Register(Definition{Name: "gm.player.reset_daily", Description: "…", Handler: resetDaily})
  }
  func resetDaily(ctx context.Context, registry *app.Registry, command admin.Command) (admin.Result, error) {
      payload, err := Decode[struct {
          Audit
          PlayerID int64 `json:"player_id"`
      }](command)
      // PlayerIDs(payload.PlayerID, player.EntityKindPlayer) 取 EntityID，再调自己的 Nest Sender
  }
  ```

  payload 结构体内嵌 `Audit`，否则 `note` 会被当成未知字段拒绝。实体写入只经 Nest Sender。
- **想换掉某条已有命令的事务**（例如加经验时还要同时改另一个实体）：服务初始化时调 `gm.Replace(registry, def)`。profile 的联动提供了 `ProfileAddExpCommand(grant)`（在 profile 模块的 `with/gm` 里）来复用解析与应答，只换发放的那一步。
- **不要改的：** `gm.go` 里 `install` 包的那一层（停止后的拒绝、`operator` / `note` 的检查、审计日志、把业务码写进 `result.data.code`）。
- **只适合借鉴的情形：** 需要带登录、角色权限、审批流、操作留痕入库的运营后台时，这个模块只能当作后台背后的执行层。值得借鉴的是“命令在锁外解析与校验、实体写入只走正式的 Nest 事务”。

## 联动

本模块没有 `with/`。别的模块的 `with/gm`（各 4 条场景）：

- `wallet`：`gm.player.coins`，经生成的 Sender 进正式的 Nest 事务加减金币。
- `bag`：`gm.player.add_item`，加道具。
- `profile`：`gm.player.add_exp`，加经验。
- `mailbox`：`gm.mail.send`，单人补偿邮件，可带附件。
- `rehome`：`gm.player.rehome`，发起单个玩家换服。
- `notice`：`gm.notice.announce`、`gm.notice.marquee`，发公告与跑马灯；在实体锁之外调 notice 的服务端入口，不经 Nest。

## 验证范围

`roost module verify` 跑 9 条场景（`^TestGM`）：没有联动命令也能启动、缺操作者拒绝、缺备注拒绝、畸形 payload 拒绝、带审计的命令执行（执行前后两条日志按 `trace_id` 对齐，成功的结果码是 0）、停止后拒绝新调用、保留带码的原因（执行后那条日志带同一个码）、生产配置拒绝开发 token、`Replace` 之后审计仍在。六组联动各 4 条，记在各自模块下。

场景是离线的：直接对 admin 命令表执行，不经真实的 ops HTTP；联动场景里的提交器、mail RPC、chat 服务、account 与实体交出都是替身。仓库一侧另有专项测试 `TestModuleGMScenariosAndMutations`（先单装再增量装 wallet、bag、profile、mailbox、rehome 五组联动；notice 的联动只由 `roost module verify` 覆盖），它不属于 `roost module verify`。
