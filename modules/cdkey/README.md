# cdkey：兑换码（配置表里的码，一码一次或每人一次，奖励以邮件发金币）

## 能力

玩家输入一个兑换码，服务端核对后给他发一封带金币附件的邮件。码分两种：`player`（每名玩家可兑一次）与 `once`（全服只有第一个人能兑）。

- **客户端协议**
  - `CdkeyRedeem`（`protocol/def/cdkey_redeem.go`）：请求带 `Cdkey`（码），成功应答带 `MailID`（发出的邮件号）。玩家再用 `mailbox` 模块的列表与领取入口领附件；奖励不直接进钱包。端点是 `game/controllers/cdkey/controller.go` 的 `HandleCdkeyRedeem`。
- **服务端入口**
  - `Controller.Redeem(ctx, playerID int64, code string) (gamecdkey.Intent, error)`（`game/controllers/cdkey/controller.go`）：客户端与可信服务端共用的协调入口，`playerID` 是原始玩家号。只能在实体事务之外调用（它要做跨进程调用）。经 `cdkey.FromRegistry(registry)` 取得 `Controller`。
  - Nest handler `handlerCdkeyPrepare`、`handlerCdkeyComplete`（`game/handler/cdkey.go`，都是 `durability=strict`）：只操作玩家账本，不发 RPC。一般不直接调用，由 `Redeem` 驱动。
  - 组件方法 `CdkeyComp().Prepare(raw string, nowUnixMilli int64)`、`CdkeyComp().Complete(code, mailID string)`（`game/entities/player/cdkey_component.go`），窄接口 `ICdkeyEntity`。
  - `game/cdkey/cdkey.go`：`Normalize(code)`（去两侧空白、转大写、校验字符）、`Intent`（一次兑换的冻结内容）及其方法 `ClaimKey(playerID)`（directory 占用键）、`RequestID(playerID)`（发件幂等键）。
- **登记点 / 钩子**：无。
- **状态**
  - `PlayerDao.Cdkey`（类型 `CdkeyLedger`，`db/def/cdkey.go`）：`Receipts` 是“码 → `CdkeyReceipt`”的 map。每条记录冻结首次受理时的 `Mode`、`Coins`、`Subject`、`Body`、`ExpiresInSeconds`，以及 `MailID`（非空表示发件已确认）。标记为 `persist`：只落库，不下发给客户端。
  - 全服占用记录在框架的 directory 服务里（共享 Redis），键是 `cdkey:once:<码>` 或 `cdkey:player:<玩家号>:<码>`。
- **配置**
  - 配置表：列定义 `configs/schema/cdkey.go`（`Cdkey{Code, Mode, Coins, Subject, Body, ExpiresInSeconds, StartsAtUnix, EndsAtUnix}`），数据 `configs/table/cdkey.csv`。自带两行示例：`WELCOME`（`player`，100 金币）与 `ONE-GIFT`（`once`，500 金币），邮件有效期都是 604800 秒。
  - 常量 `MaxReceipts = 128`（`game/cdkey/cdkey.go`）。
- **错误码**（`internal/errors/cdkey.go`）
  - `cdkey_invalid`：码格式不对、表里没有、配置行不合规，或不在有效期内。
  - `cdkey_full`：玩家的兑换记录已满 128 条。
  - `cdkey_taken`：全服码已被别人占用。

**日志**：受理、发件、完成确认；未知结果与基础设施异常记 Warn/Error。只记录兑换码长度，不记录兑换码及其派生请求键。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/cdkey/controller.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：不生成码、不支持批量码（一批随机码各用一次）、不限渠道或区服、没有“每码限 N 人”的次数上限（只有 1 人或不限人数两种）。奖励只有金币一种，以单封邮件发出，不发道具、不直接入账。没有兑换失败次数限制与防撞库。
- **码的格式**：去掉两侧空白并统一大写后，只允许 ASCII 大写字母、数字、`-`、`_`，长度 1～64 字节。
- **配置行的运行期检查**（`Prepare`）：`Mode` 只能是 `player` 或 `once`；`Coins` 为正；`Subject` 非空且不超过 128 字节；`Body` 不超过 4096 字节；`ExpiresInSeconds` 在 1 秒到 365 天之间；`EndsAtUnix` 非 0 时必须大于 `StartsAtUnix`。兑换时刻要落在 `[StartsAtUnix, EndsAtUnix)` 内，0 表示这一侧没有边界。不合规一律答 `cdkey_invalid`。
- **数量上限**：每名玩家的账本至多 128 条，已成功的回执与在途的意图都占位，不淘汰。满了拒绝新码，旧码仍可继续恢复。全服码被别人占走时，本玩家那条没有奖励的意图也留着并占一个位置。
- **一致性与幂等**：一次兑换分四步——① `Player` 严格持久事务记下意图并冻结奖励；② directory 占用（先 `Reserve` 再 `Commit`，不释放）；③ `mailbox.SendTo` 以稳定键发件；④ `Player` 严格持久事务记下邮件号。四步不是一笔事务。任一步结果未知都保留意图；玩家用同一个码再兑一次，从已有事实接着做，不换键、不重复发件。已确认的码再兑直接返回原 `MailID`，不调外部服务、不写任何字段，也不受配置下架影响。
- **恢复靠玩家重试**：没有启动扫描与后台重试。意图已记下但后续步骤没走完时，要等玩家用原码再发一次请求才继续。
- **有效期只管首次受理**：已受理的意图按冻结的奖励继续，不再看开始、结束时刻。邮件过期后不会换一封新邮件补发，实际领取期限是邮件自己的期限。
- **前提**
  - 依赖 `mailbox` 模块（连带 `wallet`）；需要 `nest`、`directory` 两个 Mod，业务服务 `uses: [mail]`。
  - 所有 game 进程必须用同一套共享 Redis 与相同的项目 namespace，否则“全服一次”不成立。
  - 不能给 directory 的持久键配 TTL、不能删除 `cdkey:*` 占用、不能复用发行过的码；同一个码的 `Mode` 与奖励不能改，新活动用新码。Redis 的持久化与备份是部署责任。
- **已知限制**：`docs/maintenance/KNOWN-LIMITS.md` 的“场景模块的边界”一条里提到 cdkey 的共享占用依赖 Directory、真实 Redis / NATS / Mongo 故障联合验证另列。

## 复用方式

- **需求正好是它**：`roost module add cdkey`。连带安装 `mailbox`（及其依赖 `wallet`），并代办接入 `directory` Mod。上线前把 `configs/table/cdkey.csv` 的两行示例码换掉，改表后 `roost generate`。
- **需求比它多一点**
  - 加码、改有效期：改 `configs/table/cdkey.csv`。
  - 奖励改成道具或多种附件：改 `controller.go` 里 `Redeem` 组装 `mailbox.Letter` 的那一行（现在是 `mailbox.Coins(intent.Coins)`），并相应扩展 `configs/schema/cdkey.go`、`db/def/cdkey.go` 的 `CdkeyReceipt` 与 `game/cdkey/cdkey.go` 的 `Intent`（奖励必须在 `Prepare` 时冻结进账本）。
  - 加渠道、等级等兑换条件：在 `cdkey_component.go` 的 `Prepare` 里、写入账本之前加检查。
  - 不要改 `ClaimKey` 与 `RequestID` 的格式：它们是已发行码的永久身份，改了会让旧码被当成新请求。
- **别的模块 / 业务代码要用它**
  - 代玩家兑换（GM、活动）：`controller, _ := cdkey.FromRegistry(registry)`，然后 `controller.Redeem(ctx, playerID, code)`；必须在实体事务之外调用。
  - 查玩家兑过哪些码：在持有 `Player` 锁的 handler 里读 `owner.Dao().GetCdkey()`。
- **只适合借鉴的情形**：需要海量一次性随机码（每个码一条记录）时，不适合放进配置表与 128 条的玩家账本，要另做码库；值得借鉴的是“先在玩家档里持久记下意图并冻结奖励，再做外部占用与发件，最后记回执；所有外部步骤用永不变化的键”这套顺序。

## 联动

无。本模块没有 `with/` 目录，别的模块也没有 `with/cdkey`。它直接调用 `mailbox` 的 `SendTo`。

## 验证范围

- `roost module verify cdkey`：12 个场景（`./game/controllers/cdkey`，`^TestCdkey`）。
- 场景是离线的：正式生成的 PB、controller、Sender 与 Nest；directory 与 mail 用框架的真实业务实现加内存存储；提交器是替身（可注入拒绝）。
- 覆盖：每人一次的码给每名玩家各发一封、全服一次的码跨 controller 只有一人成功、已完成的重放零写入且不发 RPC、受理被拒时不留占用与邮件、directory 应答丢失后仍是同一个占用者、发件应答丢失后按冻结奖励恢复、确认被拒后重放同一封邮件、非法码与恰好到期零写入、账本满额不淘汰回执、并发重放只产生一封邮件，以及一条变异检查（把全服键错误地加上玩家号时跨玩家场景会失败）。
- 没有验证：真实的 NATS、Redis、Mongo、WAL、TCP 链路与跨进程重启。用例内通过重建 `Player` 并恢复持久数据来模拟重启，不等同真实数据库重启。
