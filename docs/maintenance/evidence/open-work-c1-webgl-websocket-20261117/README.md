# 方案：C# SDK 的 WebSocket 传输，让 Unity WebGL 能连 relay（交接 C1，2026-11-17）

状态：**方案，未实施**。第一个要定的问题是“要不要出 WebGL 包”。源码行号按 main 的 `742ccafc`。

## 现状

- **C# SDK 只有 TCP，而且没有传输层抽象。** 会话对象直接持有 `TcpClient` 和 `NetworkStream`（`client/dotnet/Roost.Client.Relay/RelayClient.cs:123-132`）；拨号处直接 `new TcpClient`（`:814`）；读帧是 `RelayCodec.ReadAsync(stream, …)`（`:684`、`:823`），写帧是 `Stream.Write`（`:144`）。目标框架 `netstandard2.1`（`Roost.Client.Relay.csproj:4`），没有第三方依赖。
- **Go SDK 也只有 TCP。** `Options.Dial` 返回 `net.Conn`（`client/relay/client.go:123-124`）；地址里带 `://` 就直接报错，提示改用 TCP（`:509-511`）；重连时固定向 lobby 要 TCP（`:812`）。
- **TS SDK 只有 WebSocket。** 传输是一个很小的接口 `WebSocketLike`（`client/ts/src/client.ts:35-44`），收到的每条消息交给一个按字节流拼帧的读取器（`:999-1011`），所以一条消息里有几帧、一帧拆成几条消息它都能处理。
- **relay 在 WebSocket 上的约定**：relay 发出时一条消息恰好一帧（`service/relay/conn.go:325` 的注释）；收的一侧，`wsconn` 把连接包成字节流（`infra/network/wsconn/wsconn.go:295-299`），消息必须是二进制、单条不超过上限。帧本身的字节和 TCP 上完全一样，金样 `client/spec/relay-packets.json` 直接适用。
- lobby 已经支持按传输方式给地址：`ws` 时给完整的 `ws://` / `wss://` URL（`client/relay/lobby.go:19-25`，C# 的 `LobbyClient.cs:55` 同）。
- 根模块已经依赖 `github.com/coder/websocket`（`go.mod:8`），`client/relay` 没有自己的 `go.mod`，和它在同一个模块里。

## 选项（C#）

| | 做法 | 代价 |
| --- | --- | --- |
| A（推荐） | 在 SDK 里抽出一个传输接口（连接、发一帧、收字节、关闭），现有 TCP 改成它的一个实现；再加一个用 `System.Net.WebSockets.ClientWebSocket` 的实现，给编辑器和非 WebGL 的平台用。接口公开，WebGL 上由使用者提供一个用浏览器 WebSocket 的实现（经 `.jslib`），SDK 的文档里给一份参考写法 | 要重构会话对象；WebGL 那一小段桥接代码不在 SDK 里，不受 SDK 的测试覆盖 |
| B | A 再加：SDK 附带 Unity 用的 `.jslib` 桥接和驱动它的组件，做成 Unity 包 | 需要 Unity 工程结构；只能在 Unity 里构建和验证，本机和 CI 都跑不了，要维护者的设备 |
| C | 不做，文档写明 Unity WebGL 不支持 | 没有成本；WebGL 的游戏用不了开房间 |

### 推荐 A 的理由和要点

- 传输接口无论如何都要抽，B 也建立在它上面；先做 A，B 可以以后再加。
- 非 WebGL 平台用 `ClientWebSocket` 不加依赖，能用现有的测试对端在本机验证（TS 的用例已经经对端的 WebSocket 监听接入：`client/ts/test/relay.test.ts`）。
- 发送：一帧一条二进制消息（和 relay 发出的方式对称）。接收：按字节流拼帧，不假设一条消息一帧（和 TS 一样）。
- 向 lobby 要哪种传输由选项决定；重连时沿用同一种。

### 两个只能在 Unity 里确认的风险

这两条是 Unity WebGL 平台的一般限制，**我没有在这个仓库里验证，需要在 Unity 里实测**：

1. WebGL 上不能用 `ClientWebSocket`（浏览器沙箱里没有套接字），所以才需要 `.jslib` 桥接。
2. WebGL 是单线程的。SDK 现在大量使用 `ConfigureAwait(false)` 和后台读循环（例如 `RelayClient.cs:814-832`），这些在 WebGL 上能不能正常工作要实测；如果不能，除了换传输还要改 SDK 的调度方式，工作量会明显变大。**建议在决定做之前，先用一个最小的 WebGL 工程验证这一点。**

## Go SDK 要不要同步加

- 标准库没有 WebSocket 客户端。根模块已有 `github.com/coder/websocket`，从依赖管理上不用新增。
- 但 Go SDK 的注释写的是“SDK 不带第三方依赖”（`client/relay/host.go:102`），加了就不再成立。
- Go 客户端一般是原生程序或测试工具，能用 TCP，没有必须走 WebSocket 的场景。**建议这一轮不加**，除非有具体需求。

## 影响面

- `client/dotnet/Roost.Client.Relay`：`RelayClient.cs`（会话、拨号、读写循环）、`RelayCodec.cs`（读帧现在绑定 `Stream`）、选项里加传输方式、README。
- 测试：`client/dotnet/Roost.Client.Relay.Tests` 的对端用例对两种传输各跑一遍。
- 协议、relay、lobby、金样：都不用改。
- 文档：`client/README.md`、C# SDK 的 README、KNOWN-LIMITS 里“Unity WebGL 连不上 relay”一条。

## 测试（先红后绿）

1. 对端用例加一组“向 lobby 要 ws，再连上”：现在 C# 会在解析地址时失败，先红。
2. 建房、加入、游戏往返、房间通道、踢人、离开、断线重连、换 host，对 WebSocket 传输全跑一遍（复用现有用例，参数化传输）。
3. 接收拼帧：对端把两帧放进一条消息、把一帧拆成两条消息，SDK 都能正确收到（单元测试，用假的传输）。
4. 变异：发送时把两帧拼成一条消息仍应可用（relay 一侧是字节流）；接收改成“一条消息当一帧”→ 第 3 步红。
5. 只能在 Unity 里做的：WebGL 构建下的连接、切后台、刷新页面。列进 B1 的操作清单。

## 需要维护者定的点

1. **要不要出 WebGL 支持。** 不要的话选 C，这份方案到此为止。
2. 选 A 还是 B（桥接代码放不放进 SDK）。
3. 是否先做一个最小的 WebGL 工程验证单线程问题，再决定工作量。
4. Go SDK 加不加 WebSocket。

## 尚未确认

- 上面两条 Unity WebGL 的平台限制。
- `RelayCodec.ReadAsync` 的内部实现没有逐行读，改成按字节流拼帧的具体改法要实施时再定。
- C# 测试工程现在怎么起对端、能不能直接拿到对端的 WebSocket 地址，没有核对。
