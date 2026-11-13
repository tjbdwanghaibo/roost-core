# chat：聊天（世界频道、一对一私聊、历史与系统公告，消息存在托管的 chat 服务里）

## 能力

本模块是聊天在 game 进程这一侧的端点与规则。消息本身由框架的托管服务 `service/chat`（独立的 chat 进程，按频道给消息编序号并保存）存储；本模块决定有哪些频道、谁能发谁能读、一条消息长什么样，并把存好的消息推给在线玩家。

- **客户端协议**（`protocol/def/`）
  - `ChatSend`（`chat_send.go`）：发一行文本。请求带 `Kind`（`"world"` 或 `"private"`）、`Target`（私聊时是对方玩家号，世界频道不用填）、`Text`；应答带频道内序号 `Seq`。端点 `HandleChatSend`（`game/controllers/chat/chat_send.go`）。
  - `ChatHistory`（`chat_history.go`）：从游标往后翻一个频道。请求带 `Kind`、`Target`、`AfterSeq`（不含，0 表示从保留的最早一行开始）、`Limit`；应答带 `Messages`、`NextCursor`、`HasMore`。端点 `HandleChatHistory`（`chat_history.go`）。可读世界、私聊与系统三种频道。
  - `ChatMessage`（`chat_message.go`）：服务端推送 `ChatMessagePush`，字段 `Kind`、`Target`、`Seq`、`FromID`、`FromName`、`Text`、`System`。历史返回的每一行也是这个形状。系统消息的 `FromID` 为 0。
- **服务端入口**
  - `Controller.AnnounceWorld(ctx, text, requestID string) error`（`game/controllers/chat/chat_push.go`）：以游戏的名义在世界频道说一句话并推给在线玩家。`requestID` 是幂等键，同一个键重发不会存两次。不对客户端开放。
  - `chat.FromRegistry(registry)`（`game/controllers/chat/controller.go`）：取得 `Controller`。
  - `Controller.Presence()` 与包级 `DefaultPresence()`（`game/chat/chat.go`）：本进程的世界频道在线名单，方法 `Add`、`Remove`、`Holds`、`Snapshot`。
  - `game/chat` 包的规则函数：`ChannelOf(kind, target)`（协议字段换成频道）、`ValidateText(body)`（消息体校验）、`Allow(who, channel)`（频道政策）、`PlayerSender(playerID)`（发送者身份）、`PrivateChannel(peer)`、变量 `WorldChannel`。
- **登记点 / 钩子**
  - `OnLocalChannel(name string, send LocalChannel)`（`game/controllers/chat/local.go`）：登记一个不经 chat 服务存储的瞬时频道。`ChatSend` 的 `Kind` 等于登记名时，文本校验通过后交给登记的函数处理。只在 `init` 登记；名字不能是 `world`、`private`、`system`、`group`，重复登记 panic。登记的函数在端点的请求上下文里执行，不在任何实体事务里，必须自己检查权限与收件范围。
- **状态**
  - 不在任何实体上挂字段。消息保存在 chat 服务里；在线名单 `Presence` 是进程内存。
- **配置**
  - 没有配置表与配置键。规则是 `game/chat/chat.go` 里的常量：`MaxTextBytes = 256`、`WorldChannelTarget = 1`、`TypeText`（唯一的消息类型）、`SystemActor = "game"`。
  - `services/chat/collaborators.go`：chat 进程向工程要的协作者（`Policy`、`Bodies`、`System`、`Rules`、`Metrics`）。安装时它取代生成的拒绝桩，之后归工程所有。
- **错误码**
  - 模块自己不定义错误码。拒绝时应答的 `Code` / `Reason` 来自框架 `service/chat` 的错误（例如 `ErrMessageInvalid`、`ErrChannelInvalid`、`ErrNotPermitted`、`ErrRequestInvalid`）经 `errcode.ClientError` 的映射。

**日志**：服务端 AnnounceWorld 发布确认记 Debug，失败记 Error；不记录玩家发言正文和每次发言。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/chat/chat_push.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：没有群组频道（队伍、公会频道被 `Allow` 明确拒绝）；没有敏感词过滤、禁言、屏蔽、举报；没有发言频率限制；没有已读与离线消息提醒；玩家不能往系统频道发。发送者名字固定是 `player-<玩家号>`（`PlayerSender`），不读档案里的名字。
- **大小**：一条消息至多 256 字节的合法 UTF-8 文本，不能为空。历史每页条数上限由 chat 服务决定。
- **幂等与送达**
  - 发送：幂等键是“玩家号 + 会话号 + 帧序号”，重发的同一帧不会存两次。
  - 推送是尽力而为：chat 服务里存下的才是记录。推送失败、玩家不在名单里都不重试；漏掉的消息由客户端用 `ChatHistory` 按 `Seq` 补读。同一条公告重发时在线推送可能重复，客户端按 `Seq` 去重。
  - 世界与系统频道推给在线名单里的所有玩家，私聊推给双方。
- **在线名单按进程计**：玩家进入游戏（装了 `enter` 时）、发过言或翻过历史后加入；最后一条连接关闭、或推送时发现没有会话时移除。多个 game 进程不互相扇出：一个进程上的玩家发言，只推给同一进程名单里的人，别的进程的玩家要靠历史补读。
- **不经过 Nest**：世界与私聊的发送不取任何实体锁，也不改任何实体。
- **系统消息入口无条件授予**（`collaborators.go` 的 `System`）：前提是 chat 服务只经内部总线可达。总线能被不可信一方够着的部署，必须把它换成对调用方身份的检查。
- **前提**：业务服务在 `roost.yaml` 里要有 `uses: [chat]`，并且有玩家 TCP 接入（安装时代办）；运行时需要 chat 服务进程。工程已经改过 `services/chat/collaborators.go` 时装不了（安装器只替换没改过的生成桩）。不依赖别的模块。
- **已知限制**：见 `docs/maintenance/KNOWN-LIMITS.md` 的“chat 模块的边界”一条（连接关闭的订阅在第一次取推送面时才挂上、进程退出时不单独退订等）。

## 复用方式

- **需求正好是它**：`roost module add chat`。不连带安装别的模块。
- **需求比它多一点**
  - 改长度上限、分区世界（把区号放进 `WorldChannelTarget`）、发送者显示名：改 `game/chat/chat.go`。game 进程与 chat 进程都 import 这个包，两边规则保持一致。
  - 开放群组频道：改 `game/chat/chat.go` 的 `Allow` 与 `ChannelOf`，按队伍或公会的成员关系授权；`chat_push.go` 的 `deliver` 也要补上群组的收件人。
  - 加敏感词或频率限制：在 `HandleChatSend` 里、调用 `messaging.Publish` 之前加检查。
  - 加一个不存历史的瞬时频道（附近、房间内）：在自己的包里 `init` 调 `OnLocalChannel`，不要改 `chat_send.go`。
  - 替换后的 `collaborators.go` 里不要再出现 `is not configured` 这句话，`roost project doctor` 靠它判断文件是否还是拒绝桩。
- **别的模块 / 业务代码要用它**
  - 发全服公告：`controller, _ := chat.FromRegistry(registry)`，然后 `controller.AnnounceWorld(ctx, text, requestID)`，`requestID` 用稳定的业务键。
  - 让玩家立刻收到世界频道推送：`chat.DefaultPresence().Add(playerID)`（`game/chat` 包）。
  - 复用文本校验：`chat.ValidateText([]byte(text))`。
- **只适合借鉴的情形**：需要跨进程实时扇出的世界频道（多个 game 进程的玩家互相即时可见）时，本模块的进程内名单不够，要改为订阅 chat 服务的流；值得借鉴的是“存储为准、推送为捷径、每条推送带 `Seq` 供补读”的做法。

## 联动

- `chat` + `enter`（`with/enter/game/chat/chat_enter.go`）：经 `gamelogin.OnEntered("chat", …)` 在玩家进入游戏时把他加进本进程的世界频道在线名单。不发登录公告。
- `scene` + `chat`（在 `scene/with/chat`）：经 `OnLocalChannel("nearby", …)` 登记附近聊天，在 Nest 里取同场景收件人快照后在锁外推送，没有历史。
- 以 `requires` 依赖它的模块：`notice`（公告与跑马灯经 `AnnounceWorld` 发出）。

## 验证范围

- `roost module verify chat`：10 个场景（`./game/chat`、`./game/controllers/chat`、`./services/chat`，`^TestChat`）；与 `enter` 的联动另有 2 个场景（`./game/chat`，`^TestWithChatEnter`）。
- 场景是离线的，覆盖：文本校验、`ChannelOf` 的寻址、`Allow` 的放行与拒绝、在线名单的增删、最后一条连接关闭后移出名单（玩家传输用替身 `chatSessions`）、本地频道先校验再分发且没有历史、collaborators 执行同一套政策并授予系统入口。
- 没有验证：`HandleChatSend` 经 chat 服务发布世界或私聊消息、`HandleChatHistory` 读回、`AnnounceWorld`、`deliver` 的实际推送——这几条路径在模块场景里没有用例；真实 chat 进程、NATS 总线与 TCP 推送也没有在模块场景里跑过。
