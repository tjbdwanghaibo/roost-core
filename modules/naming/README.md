# naming：全服唯一的玩家名字（两阶段占名、可恢复的改名）

装上它之后，玩家的显示名在所有共用同一个 Directory（框架的共享占名目录，数据在 Redis）的 game 服之间唯一、大小写不敏感。改名是一个记在玩家档里的小状态机：先在目录里临时占住新名，提交之后写进档案，最后释放旧名。任何一步结果不明都保留现场，用同一个请求键重发就接着做完。

## 能力

**客户端协议**

- `NamingRename{RequestKey, Name}` → `NamingRenameResponse{Code, Reason, Name}`（`protocol/def/naming_rename.go`）：改名。同一次操作重发要用同一个 `RequestKey` 与同一个 `Name`。`Code` 为 0 才表示改名与旧名清理都完成。
- profile 模块的 `ProfileRename{Name}` 被自动接到同一个流程：模块在 `init` 里调 `profile.RegisterRenamer`，旧协议用目标名字的 SHA-256 作请求键。

**服务端入口**（`game/controllers/naming/controller.go`）

```go
c, err := naming.FromRegistry(registry)
name, err := c.Rename(ctx, playerID, requestKey, name) // 只能在 Nest 事务之外调用，会等待目录与 Nest
```

**Nest handler**（`game/handler/naming_begin.go`、`naming_step.go`，都是 `rollback=undo durability=strict`，由 `Rename` 经生成的 Sender 调用）

- `NamingBegin(operation, name)`：受理一次改名（校验名字与请求键、记下意图），返回 `naming.NamingTicket`。
- `NamingStep(seq, operation, action, key, token)`：推进一步。`action` 1 = 记下目录凭据，2 = 目录已提交、把名字写进档案，3 = 旧名已释放、完成，4 = 占名被明确拒绝、撤掉意图，5 = 凭据被明确判为失效、退回重新占名。

**登记点 / 守卫**

- 模块没有给别的功能的登记点。
- 它给 `Player` 加了 `CheckProfileRenameRequest()`（`game/components/naming/naming.go`）：profile 的本地改名 handler `ProfileRename` 看到它就拒绝并答 `naming_required`，所以旧的本地 Sender 绕不过目录。

**状态**：`PlayerDao.Naming`（`db/def/naming.go` 的 `NamingState`），只落库、不下发。字段：`Seq`（改名代际，只增）、`Operation`（在途或最近一次完成的请求键）、`Target`、`Phase`（0 无在途、1 已受理、2 已记凭据、3 已写档案待清旧名）、`ClaimKey` / `ClaimToken`（目录凭据）、`CurrentKey` / `CurrentOwner`（当前名字在目录里的键与属主）、`OldKey` / `OldOwner`（待释放的旧名）。至多一笔在途加一笔最近的回执。显示名本身仍在 profile 的 `PlayerDao.Profile.Name`。

**目录里的数据**：键是 `player-name/<名字>` 经目录规范化（小写）之后的结果；属主是 `naming:<Player 的完整 Entity ID>:<Seq>`；临时占名的有效期是 1 分钟（`naming_rename.go` 里 `Reserve` 的 `time.Minute`）。

**配置**：模块没有自己的配置键。它要求 Directory Mod 的配置（`directory.key_prefix`、`directory.reservation_ttl`），安装时代办 Directory Mod。

**错误码**（`internal/errors/naming.go`）

- `naming_busy`：这名玩家有另一笔改名在途（换请求键重发会得到它），或步骤的代际 / 请求键对不上。
- `naming_invalid`：请求键不合规，或同一个请求键换了目标名字。
- `naming_required`：改名必须走本模块（本地 `ProfileRename` handler 被守卫拒绝时）。
- 名字不合规时是 profile 的 `profile_name_empty` / `profile_name_long` / `profile_name_invalid`。
- 名字被占时是 Directory 的码原样带回（`directory: key is taken`，530101）；凭据失效是 530102 / 530103。

**日志**：改名确认，记录请求键、旧名键中的名称与新名；重放时旧名可能为空。结果未知与内部错误记 Warn/Error，不打印占用票据。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/naming/controller.go`, `game/controllers/naming/naming_rename.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：没有敏感词过滤、改名花费、次数或冷却限制（要加就在受理之前加）；没有“只改大小写”的专门路径（只改大小写也会重新占名）；不导入旧数据——装它之前直接写进档案的名字不会自动补登记到目录；没有按玩家查询在途改名的协议；没有全服扫描器与自动清扫；没有后台任务。
- **名字规则来自 profile**：合法 UTF-8、去掉首尾空白后 1 到 16 个字符。初始的空名字不占名。唯一性按目录的小写规范化判断。
- **请求键**：1 到 64 字节，不能带首尾空白。只保留最近一笔回执：完成之后再做别的改名，旧回执就被替换。需要长期幂等的调用方自己记账。与当前显示名完全相同的改名，用新请求键来会直接当作已完成。
- **一致性**：玩家档与目录是两个存储，不是一笔事务。保证的是：
  - 每一步阶段转换是 Player 上的一笔严格事务；同一名玩家同时只有一笔改名在途。
  - 超时、目录应答丢失、Nest 提交结果未知、旧名释放失败时，**不撤销也不释放新名字**，保留在途状态；同一个请求键重发从存下来的阶段继续，进程重启之后也一样。
  - 只有两种明确的回答会回退：占名时目录明确说“已被占”（撤掉还没拿到凭据的意图）；提交时目录明确说凭据不存在或已过期（清掉凭据退回已受理，本次仍返回错误，下一次重发重新占名）。
  - 释放旧名带着旧属主做 CAS；属主里有 `Seq`，迟到的旧清理删不掉同一名玩家后来重新取得的名字，也删不掉别人的。
  - 代价：出故障期间一名玩家可能暂时同时占着新旧两个名字。
- **客户端要保存待确认的请求键与目标**：失败可能是结果未知，换请求键重试会被 `naming_busy` 挡住。
- **可信写入原语仍然存在**：`ProfileComponent.Rename` 与 DAO 的 setter 是事务内的字段写入，不保证唯一。装了本模块的工程里，业务改名一律走 `Rename` 入口，不要直接调它们。
- **前提**：依赖 `profile`（连带 `enter`）；业务服务要有 `nest` 与 `directory` 两个 Mod（Directory 用 Redis）。所有 game sid 必须共用同一套目录前缀与同一个 Redis，不同游戏不能共用前缀。目录调用都在 controller 里、Nest 事务之外。
- **人工处理在途记录**时要同时核对 Player 与 Directory，不能只删一边。
- **已知限制**：[KNOWN-LIMITS“场景模块的边界”](../../docs/maintenance/KNOWN-LIMITS.md)里提到 naming 的共享占名依赖 Directory，真实 Redis 故障的联合验证没有做。

## 复用方式

- **需求正好是它**：`roost module add naming`，连带装上 `profile` 与 `enter`，代办 Directory Mod 与 Redis 依赖；新工程的配置里会写按工程隔离的目录前缀与 60 秒的预留期。
- **需求多一点**：
  - 敏感词、花费、次数：在 `game/controllers/naming/naming_rename.go` 的 `HandleNamingRename` 里、调 `Rename` 之前检查；或者在 `NamingComponent.Begin` 里加只读检查（它在受理的那笔事务里）。扣费要与“名字写进档案”同一笔事务时，加在 `Step` 的动作 2 里。
  - 不要改 `resume` 的步骤顺序，也不要在超时分支里加撤销或释放。
- **别的代码要用它**：
  - 服务端改名：`naming.FromRegistry(registry)` 之后 `Rename(ctx, playerID, requestKey, name)`。
  - 不想直接依赖本模块：调 profile 的 `profile.Rename(ctx, registry, playerID, name)`（`game/profile`），装了本模块时它转到这里。`enter` + `profile` 的联动就是这样在新建角色时占名的。
  - 读名字：仍读 profile（`ProfileComp().DisplayName()` 或 handler `ProfileCard`）。
- **只适合借鉴的情形**：公会名、队伍名等别的全服唯一名字——状态机挂在 Player 上，不能直接拿来给别的实体用。值得借鉴的是“意图先落库 → 临时占名 → 凭据落库 → 提交 → 写业务字段 → 释放旧值”的顺序，以及属主带代际防止迟到清理误删。

## 联动

- 本模块没有 `with/` 目录。
- `enter` + `profile` 的联动（`enter/with/profile`）会感知本模块：装了 naming 时，新建角色（`EnterCreate`）的名字在建档之后、新建事务之前经改名入口占名；名字被占则这一次新建不成，换名字再发。
- profile 的 `ProfileRename` 端点与本地 handler 的行为在装了本模块之后改变（见上面的协议与守卫）。

## 验证范围

- `roost module verify naming`：15 条场景，都在 `game/controllers/naming`。
- 离线：Directory 用真实的实现加内存 versionstore，Nest 用真实引擎加替身提交器。
- 覆盖：唯一性与重放、只释放旧名、目录提交应答丢失、档案提交失败（两个名字都保留待重试）、档案回包丢失、释放失败后恢复、同一玩家不同请求互斥、同一请求并发重试幂等、过期凭据重试、旧 Sender 被拒、旧端点走统一入口、非法输入与被拒的受理不写任何东西、迟到的清理不误删重新取得的名字、等待目录时不占 Nest worker、释放应答丢失且旧名已被别人取得时仍能完成。
- `scripts/test-demo-login.sh` 的 casual + naming 阶段使用真实 HTTP / Robot TCP、Redis Directory、Mongo / WAL：新建占名、两服同名拒绝、失败后仍需新建、换名释放旧名、完成回执重放、game `SIGKILL` 后重连恢复。见[登录线证据](../../docs/maintenance/evidence/v147-login-20261114.md)。
- 没有验证：真实 Redis 故障与命名各中间阶段崩溃的联合恢复、跨机、长期稳定性。

## 阶段表

| `Phase` | 已确认的事实 | 下一步 |
| --- | --- | --- |
| 0 | 没有在途；保留最近的请求键与目标作为回执 | 接收新的请求键 |
| 1 | 改名意图已提交，旧名仍有效 | 在目录里临时占新名；把凭据写进 Player |
| 2 | 凭据已写进 Player；档案里仍是旧名 | 提交目录，再在一笔事务里写档案并记下当前凭据的属主，进入 3 |
| 3 | 档案已是新名，目录里新名已永久占有 | 按旧属主释放旧名，再清在途阶段 |
