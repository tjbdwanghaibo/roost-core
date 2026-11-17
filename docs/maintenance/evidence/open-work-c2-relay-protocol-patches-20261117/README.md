# 方案：relay 的两个小协议补丁（交接 C2，2026-11-17）

状态：**方案，未实施**。等维护者定了再做。源码行号按 main 的 `742ccafc`。

两项互不依赖，可以分开做、分开提交。

## 一、Welcome 帧带上 epoch

### 现状

- Welcome 的载荷是固定 5 个字节：`[角色 1B：1 房主、2 成员][房主槽位 4B 大端]`，不带 epoch（`client/wire/relay.go:137-158`，解码要求长度恰好为 5）。
- relay 在两处发它：建房时给房主（`service/relay/room.go:96`），其余连接进房时（`service/relay/room.go:214`，选人阶段房主槽位写 0）。
- relay 自己有 epoch：`service/relay/room.go:49-50`，建房为 0，每次进入选人阶段加 1。
- Go SDK 的 `Role().Epoch` 取 `c.host.epoch`（`client/relay/host.go:202`），它只在收到 YouAreHost、YouAreMember、HostChanged、HostResumed 时更新（`client/relay/host.go:337、365、391、408`）。所以**在第 N 次换 host 之后才连进来（或重连回来）的客户端，查到的 epoch 是 0，而房间实际是 N**。
- C# SDK 的注释自己写明了这一点：“Welcome 不带 epoch，连上之后还没见过换 host 时为 0”（`client/dotnet/Roost.Client.Relay/RelayClient.cs:81`）。
- TS 的编解码在 `client/ts/src/codec.ts:242-255`。
- 金样条目 `relay_welcome` 在 `client/spec/relay-packets.json:23-24`。

这是不是 bug：协议文档如实写了“Welcome 不带 epoch”，行为和文档一致，所以是**协议缺口**，不是实现错误。它的实际影响是：游戏如果拿 `Role().Epoch` 去区分“这是第几任房主的数据”，后进房的人会拿到错的值。目前仓内没有代码依赖这一点（尚未确认下游游戏有没有）。

### 选项

| | 做法 | 代价 |
| --- | --- | --- |
| A（推荐） | 载荷改成固定 13 字节：`[角色 1B][房主槽位 4B][epoch 8B]`，三份 SDK 的解码都要求恰好 13 字节 | 金样 `relay_welcome` 的字节变了；三份 SDK、relay、RELAY.md 同时改。线上未部署，按交接文档的规则不需要兼容旧进程 |
| B | 解码同时接受 5 字节和 13 字节 | 多一条永远用不到的分支，而且旧客户端连新 relay 会因为长度不对直接断开，兼容只有单向，没有意义 |
| C | Welcome 不动，relay 在 Welcome 之后补发一帧只带 epoch 的控制帧 | 多一个操作码，客户端要处理“Welcome 到了但 epoch 还没到”的中间状态 |

### 推荐 A 的细节

- 选人阶段进房的连接：Welcome 里房主槽位仍是 0，epoch 写 relay 当前的值（此时已经加过 1）。这和 HostChanging 的约定一致（“选人阶段是新的 epoch”，`client/wire/relay.go:187`）。
- 客户端：收到 Welcome 时把 `Role().Epoch` 设成帧里的值；重连回来时也按 Welcome 更新。

### 影响面

- `client/wire/relay.go`（`RelayWelcome` 加字段、编解码）
- `service/relay/room.go:96、214`
- `client/relay/client.go`（处理 Welcome 的两处：`:548`、`:706`）、`host.go`
- `client/ts/src/codec.ts`、`client.ts`
- `client/dotnet/Roost.Client.Relay`（编解码所在文件尚未逐个核对，实施时先 grep `Welcome`）
- `client/spec/relay-packets.json` 的 `relay_welcome`
- 文档：`docs/framework/RELAY.md`、三份 SDK 的 README、KNOWN-LIMITS 一“大厅 lobby 与转发 relay”里对应的一句

### 测试（先红后绿）

1. 先改金样的 `relay_welcome`，三份 SDK 的金样用例变红。
2. `service/relay`：新用例——换过两次 host 之后新进房的成员，Welcome 里 epoch 为 2；选人阶段进房的，房主槽位 0、epoch 是新值。
3. 三份 SDK 的对端用例：换 host 之后才 join 的客户端，`Role().Epoch`（各端对应的查询）等于房间的 epoch。
4. 变异：relay 发 Welcome 时把 epoch 写成 0 → 第 2、3 步变红；SDK 收到 Welcome 不更新 epoch → 第 3 步变红。

## 二、前台成员上报

### 现状

- 方案 §4.7.3 已经写了做法：“前台成员靠 SDK 上报的‘是否前台’标记来认，是一个控制帧，relay 只转给房主，不解析；房主据此维护 `successors` 候选顺序”（`docs/maintenance/FEATURE-2026-11-16-lobby-relay.md:275`）。**这个控制帧没有实现。**
- 现有操作码：1～5 relay 发出，16～17 房主发出，32～33 心跳，64～69 relay 发出的换 host 帧，80～84 客户端发出的换 host 帧（`client/wire/relay.go:69-96、177-210`）。85 起是空的。
- relay 对不认识的操作码直接丢弃、不断开（`service/relay/conn.go:173-213`，switch 没有 default 分支处理）。
- TS 端让出房主时选人：取声明过的 `successors` 里第一个在线的，没声明就取在线成员里槽位最小的（`client/ts/src/web.ts:556-563`）。它不知道谁在前台，所以可能把房主交给另一个同样在后台的页面。

### 选项

| | 做法 | 代价与风险 |
| --- | --- | --- |
| 1（推荐，即方案原文） | 成员发一个新控制帧上报前台 / 后台；relay 只做长度检查，改写成“来自哪个槽位”后转给当前房主；房主的 SDK 据此排 `successors` | 新增两个操作码（成员发的、relay 转给房主的各一个，保持“64～79 relay 发、80～95 客户端发”的号段约定）。成员可以谎报，但后果只是影响房主排的候选顺序，房主的游戏逻辑仍可以自己覆盖 |
| 2 | 不加控制帧，用房间通道或游戏通道发一条约定的消息给房主 | 协议不用改；但房间通道会被禁言丢掉（被禁言的成员就报不上去），游戏通道的消息号归游戏所有、会撞号。不推荐 |
| 3 | relay 解析并记住每个成员的前台状态，房主掉线又没声明候选时，relay 的兜底顺序优先选前台成员 | 覆盖了“房主来不及声明就掉线”的情况；但 relay 要多存状态、多一条规则，和方案里“relay 不解析”的原则相反。可以作为选项 1 之后的增强，不建议这一轮做 |

### 推荐 1 的细节

- 操作码（建议值，待定）：`RelayOpPresence = 85`（任何成员 → relay，Seq=0，载荷 1 字节：1 前台、0 后台）；`RelayOpMemberPresence = 70`（relay → 房主，Seq=成员槽位，载荷同上）。
- relay：载荷不是 1 字节或值大于 1 → 按协议错误断开（和 Mute 一致，`service/relay/conn.go:184-188`）；选人阶段没有房主时丢弃。这一帧计入现有的每连接帧率限额，不另设限流。
- 新房主不知道之前的上报：SDK 在收到 HostChanged（以及自己重连成功）后自动重发一次当前状态。
- 房主一侧：SDK 维护“槽位 → 是否前台”，TS 的 `WebHost` 在没有被游戏显式声明候选顺序时，自动把前台成员按槽位升序排在前面并调用 `setSuccessors`。游戏显式声明过的，以游戏为准，不覆盖。
- Go / C#：提供 `SetForeground(bool)` 与 `OnMemberPresence` 回调，不自动排顺序（原生端通常自己管）。Unity 可以在 `OnApplicationPause` 里调。

### 影响面

- `client/wire/relay.go`（两个操作码、载荷编解码）、`service/relay/conn.go` 与 `room.go`
- 三份 SDK；TS 的 `web.ts`（成员侧在 `visibilitychange` 上报，房主侧排顺序）
- `client/spec/relay-packets.json` 追加两条（追加不影响现有条目）
- 文档：RELAY.md §2.3、`client/ts/README.md`、KNOWN-LIMITS 里“前台成员上报没有控制帧”一句

### 测试（先红后绿）

1. 金样追加两条，三份 SDK 的金样用例先红。
2. `service/relay`：成员上报 → 只有房主收到，Seq 是该成员的槽位；别的成员收不到；选人阶段上报被丢弃；载荷非法断开。
3. TS：两个成员一个前台一个后台，房主切后台超时后转交给前台的那个（现在的规则会交给槽位小的那个，这条用例在改之前应当是红的）。
4. 换 host 之后新房主收到各成员重发的状态。
5. 变异：relay 把上报转给全体 → 第 2 步红；房主侧不排前台优先 → 第 3 步红；HostChanged 后不重发 → 第 4 步红。

## 需要维护者定的点

1. Welcome 是否按选项 A 直接改成 13 字节（金样条目的字节会变）。
2. 前台上报的两个操作码取 85 和 70 是否合适。
3. TS 的 `WebHost` 自动排候选顺序时，和游戏显式声明的顺序谁优先（建议游戏优先）。
4. Go / C# 是否只给回调、不自动排顺序。
5. 选项 3（relay 兜底时优先前台）这一轮做不做。

## 尚未确认

- C# 里 Welcome 编解码的具体文件和行号没有逐个读，只确认了 `RelayClient.cs:81` 的注释。
- RELAY.md 里要改的具体小节没有逐句核对。
- 下游游戏有没有已经在用 `Role().Epoch` 的，没有信息。
