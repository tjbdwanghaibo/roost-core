# notice：服务端发全服公告与跑马灯（经 chat 的世界频道）

一个很薄的可信入口：把一段文字以系统身份发到 chat 的世界频道，存进频道历史并推给本进程在线的玩家。公告与跑马灯的区别只是文字前缀。没有玩家协议，没有定时播放。

## 能力

**客户端协议**：无。客户端收到的就是 chat 模块的世界频道消息（推送 `ChatMessage`，漏掉的用 `ChatHistory` 补读），文字以 `公告：` 或 `跑马灯：` 开头，按前缀选表现。

**服务端入口**（`game/notice/notice.go`，在 Nest 事务之外调用）

```go
publisher, err := notice.FromRegistry(registry)                     // 能力名 notice.publisher
err = publisher.Publish(ctx, "announce", "维护完成", "maintenance-20261112") // kind 是 "announce" 或 "marquee"
```

`Publish(ctx, kind, text, key)`：`key` 是这一次业务事件的稳定键，同一个键的内容不能变。实际调 chat controller 的 `AnnounceWorld(ctx, 前缀+text, "notice:"+kind+":"+key)`。

**运维命令**（模块启动时登记到框架的 admin 命令表，经 ops 的 `/admin` 入口执行）

- `gm.notice.announce`：发公告。
- `gm.notice.marquee`：发跑马灯。
- 两条的 payload 都是 `{"text": "维护完成", "key": "maintenance-20261112"}`，成功返回 `{"kind": …, "key": …}`。

**登记点**：无。

**状态**：无。消息存在 chat 服务的世界频道历史里。

**配置**：无。模块启动入口是 `notice.Start`（清单的 `runtime`），不带后台任务。

**错误码**：模块没有自己的业务码。输入不合法时返回框架的 `admin.ErrCommandInvalid`；chat 的错误原样返回。

## 能力边界

- **不做的事**：没有玩家发公告的协议；不周期播放、没有定时器与调度；不做定向公告（按服、按渠道、按玩家）；不做富文本与多语言；不单独存公告列表，也没有“登录时弹出最新公告”的入口。
- **大小**：前缀加正文总长至多 256 字节（chat 的 `MaxTextBytes`），必须是合法 UTF-8，正文不能全是空白；`key` 必填、至多 64 字节。
- **幂等的范围**：同一个 `kind` + `key` 在 chat 的保留期内不会存第二条历史；但重试时在线推送可能重复，客户端要按消息序号去重。
- **只推本进程的在线玩家**：沿用 chat 的在线名单，多个 game 进程之间不互相扇出。别的进程上的玩家靠 `ChatHistory` 补读，或者每个进程各调一次（用同一个 `key`，历史只存一条）。
- **运维命令不经 gm 模块**：两条命令直接登记在框架的 admin 命令表上，用的是 admin 的令牌与审计日志；它们不经 gm 模块的 `Register`，gm 模块对操作者与备注的必填检查不适用于它们。安装不会自动打开运维 HTTP 入口，也不会填令牌。
- **停止之后拒绝**：模块停止后 `Publish` 返回 `notice: publisher is stopped`。
- **前提**：依赖 `chat`（chat 模块要求业务服务 `uses: [chat]`）；进程里要有框架的 admin 命令表（game 模板的 ops 自带），否则启动失败。
- **已知限制**：[KNOWN-LIMITS“场景模块的边界”](../../docs/maintenance/KNOWN-LIMITS.md)（notice 不默认调度）与“chat 模块的边界”（在线名单每进程一份、不跨进程扇出）。

## 复用方式

- **需求正好是它**：`roost module add notice`，连带装上 `chat`。
- **需求多一点**：
  - 周期播放：用工程自己的调度器，每次播放用一个新的稳定键调 `Publish`（例如 `活动号-第几次`）。
  - 新的展示类别：在 `notice.go` 的 `Publish` 里加一个 `kind` 与前缀，并在 `start` 里加一条命令。
  - 定向或带结构的公告：不适合在这里改，另写协议。
- **别的代码要用它**：`notice.FromRegistry(registry)` 之后 `publisher.Publish(ctx, kind, text, key)`。在 Nest handler 里不能直接调（它会等 chat 服务），要放到事务之外，或者经 effect 在提交之后发。
- **只适合借鉴的情形**：需要“公告列表 + 已读状态”的公告系统。值得借鉴的是用稳定业务键把“发一次”做成幂等，以及复用已有的 chat 频道而不另建广播通道。

## 联动

无。本模块没有 `with/` 联动，别的模块也没有 `with/notice`。

## 验证范围

- `roost module verify notice`：8 条场景，都在 `game/notice`。
- 离线：多数场景用只记调用的 chat 替身；一条经正式的 chat controller 验证系统发布的形状；一条是可编译变异（去掉空白文本的拒绝后，行为场景必须失败）。
- 覆盖：公告与跑马灯各自的键空间、非法输入在发布前被拒、chat 的错误原样传出、两条运维命令已登记且停止后拒绝、缺依赖或 payload 不合法时失败。
- 没有验证：真实的 NATS / Redis / TCP，多进程广播。
