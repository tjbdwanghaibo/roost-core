# servertime：把服务器的业务时间、当前业务日与下一次每日重置的时刻告诉客户端

“业务时间”是真实时间加上部署配置的偏移（`time.logic_offset`，生产必须为 0）；“业务日”是按业务时区（`time.business_timezone`）与每日重置时刻（`time.business_day_start`）切出来的一天，写成 YYYYMMDD 的十进制数。签到、每日任务、体力、商店都按业务日惰性清零，客户端显示前要知道“今天是哪个业务日”，这个模块用服务端判定业务日的同一份日历把答案算好下发。

## 能力

- **客户端协议**
  - `ServertimeSync`（`protocol/def/servertime_sync.go`）：请求 `ServertimeSyncRequest{ClientTick}`，应答 `ServertimeSyncResponse{Code, Reason, ClientTick, Time}`。客户端主动问一次。`ClientTick` 是客户端自己的单调时钟读数，服务器原样带回、不解释，客户端用它算往返时延。这条协议没有业务拒绝，`Code` 总是 0。
  - `ServertimeNow`（`protocol/def/servertime_now.go`）：推送 `ServertimeNowPush{NowUnixMilli, Day, DayStartUnixMilli, NextResetUnixMilli, NextDay, PrevDay, UTCOffsetSeconds, Zone}`。`ServertimeSync` 应答里的 `Time` 是同一个形状，客户端只处理一种。
- **服务端入口**
  - 包 `game/servertime`（`servertime.go`），纯计算、没有状态、不依赖任何 controller 与实体包：
    - `At(nowUnixMilli int64) Snapshot`：某一刻的时间事实。给 Nest handler 用，参数就是 Nest 填进来的那个 `nowUnixMilli`；得到的 `Day` 与 `clock.BusinessDay` 是同一个数。
    - `Now() Snapshot`：此刻的快照，现读进程业务时钟。给不在 Nest 消息里的端点用；Nest handler 不要调它。
    - `Snapshot` 的字段与推送一一对应。
  - `PushNow(ctx, registry, playerID) error`（`game/controllers/servertime/servertime_push.go`）：把此刻的 `ServertimeNow` 推给一名玩家的全部连接。每次调用现取传输、现读时钟，不缓存。
  - `Controller.HandleServertimeSync`（`servertime_sync.go`）：协议端点，不经过 Nest。
- **登记点 / 钩子**
  - `ZoneName(location *time.Location) string`（`game/servertime/servertime_zone.go`，留给工程改）：对外展示的业务时区名，缺省原样用 IANA 名。只影响显示，业务日与重置时刻不读它。
  - 模块自己不提供登记表。装了 enter 时，联动文件把 `PushNow` 登记成进入钩子（见“联动”）。
- **状态**：无。不属于任何实体，不落库。
- **配置**：没有自己的配置键与配置表；读的是框架的三个时间配置（见上），它们只在服务进程启动时读一次。
- **错误码**：无。

## 能力边界

- **不做的事**：不能改时间（没有“拨动业务时间”的入口，改偏移要改配置并重启进程）；没有“时间变了”的广播；不做每日重置的服务端定时触发或通知——只告诉客户端重置在什么时候；不做客户端与服务器的精确对时，客户端算出来的业务时间是估计值。
- **客户端该怎么用**（协议注释里的约定）：收到快照时记下自己的单调时钟读数，之后的业务时间 = `NowUnixMilli` + 单调时钟走过的毫秒数，不要每秒问一次、不要用设备的墙上时间；算到 `NextResetUnixMilli` 时当前业务日就是 `NextDay`，再问一次拿新一天的数据（在线客户端各自随机晚几秒，避免同一刻涌来）；从后台回来、休眠醒来、以及隔一段时间（例如半小时）再问一次。重置时刻前后几秒里，“今天做过没有”以服务器对那条业务请求的应答为准。
- **推送是尽力而为**：传输没装配、玩家没有连接、发送队列满都会让 `PushNow` 返回错误；调用方记日志即可，不要因此让自己的操作失败。客户端没收到就自己发 `ServertimeSync`。
- **频率**：服务端不限流。每日重置之后在线客户端会集中来问一次。
- **夏令时**：`NextResetUnixMilli` 不一定是 `DayStartUnixMilli` 之后 24 小时；`UTCOffsetSeconds` 只用来显示墙上时间，不要拿它推算重置时刻；`PrevDay` 是按“本业务日起点之前那一刻所在的业务日”算的，不是日期数字减一。
- **一致性的范围**：`At` 与各功能判定业务日用的是同一个进程日历，所以同一个 `nowUnixMilli` 两边的业务日相同；但 `Now()` 现读的时钟会比某条 Nest 消息冻结的时间晚，跨天那一刻两者可能不是同一个业务日。
- **前提**：没有模块依赖。安装时代办玩家 TCP 接入（`access.transport: tcp`）；推送按能力名 `access.player.tcp` 取传输，只对连接在本进程的玩家有效。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md`）：“servertime 的边界”一条（只有离线场景；推送经真实 TCP 到达、与 `EnterGame` 应答的先后、独立 Gate 部署都没有验收；独立 Gate 下 Game 重启后客户端连接是否保留尚未确认）；“惰性清零对客户端可见”一条说明了客户端为什么需要它。

## 复用方式

- **需求正好是它**：`roost module add servertime`，没有模块依赖。已经装了 enter 的工程会同时得到“进入游戏时推一次”的联动。
- **需求多一点**
  - 改显示的时区名（“服务器时间”、“UTC+8”、文案键）：只改 `game/servertime/servertime_zone.go` 的 `ZoneName`。
  - 工程用自己的登录流程（没装 enter）：在玩家进入之后自己调 `servertime.PushNow(ctx, registry, playerID)`，或让客户端进入后自己发 `ServertimeSync`。
  - 不要改 `servertime.go` 里 `At` 的算法：它必须与 `clock.BusinessDay` 保持同一份日历，否则客户端看到的业务日与各功能记进状态的 `Day` 对不上。
- **别的功能要用**
  - 在 Nest handler 里要“今天 / 昨天 / 下一次重置”：`snapshot := servertime.At(nowUnixMilli)`（import `game/servertime`），取 `snapshot.Day`、`snapshot.PrevDay`、`snapshot.NextResetUnixMilli`。
  - 在自己的应答里顺带返回服务器时间：非 Nest 端点用 `servertime.Now()`。
  - 状态变化后想让客户端马上对时：在事务之外调 `PushNow`，错误只记日志。
  - 注意两个包同名：`At` / `Now` 在 `game/servertime`，`PushNow` 在 `game/controllers/servertime`，包名都是 `servertime`；同一个文件里两个都要用时给其中一个起别名（模块自己的代码把前者写成 `gameservertime`）。
- **不适合直接用的情形**：需要服务端在重置时刻主动做事（发奖、清榜）——那要用定时器或活动模块；需要每个场景不同的时间流速——那是 scene 的时间域。值得借鉴的是“服务端把日历算好下发，客户端用单调时钟自己走”的分工，以及“Nest 里用冻结时间、端点里现读时钟”的区分。

## 联动

- `servertime/with/enter`：登记 `gameenter.OnEntered("servertime", …)`，玩家每次进入游戏（含重连）调一次 `PushNow`。推送在进入钩子里、端点答复之前交给连接，客户端一般先收到它、后收到 `EnterGame` 的应答，但不要依赖这个先后。推送不成只由 enter 记一条日志，不让登录失败。3 条场景。
- 别的模块没有 `with/servertime`。checkin、task、stamina、shop 的状态注释里都要求客户端用它下发的 `Day` 判过期，但它们不 import 这个模块。

## 验证范围

- `roost module verify servertime`：12 条场景（`^TestServertime`，包 `./game/servertime` 7 条、`./game/controllers/servertime` 5 条）。覆盖：普通时刻、重置瞬间与前后各 1 毫秒、凌晨 5 点重置、业务时区不是 UTC、夏令时切换日不是 24 小时、`Now()` 跟随业务时钟偏移、`Day` 与 `clock.BusinessDay` 一致；同步应答带快照并回显 `ClientTick`、快照取自应答那一刻、缺上下文或请求时拒绝、`PushNow` 把快照发给指定玩家、传输不可用时报告错误。
- enter 联动另有 3 条（`^TestWithServertimeEnter`）：进入时推送、没有传输时照常进入、推送失败照常进入。
- 全部是离线场景：传输是内存替身。推送经真实 TCP 到达、与 `EnterGame` 应答的先后、独立 Gate 部署都没有验收；与 checkin 等模块的一致性是按“同一个 `clock.BusinessDay`”核对的，没有调它们的 handler 对比。
