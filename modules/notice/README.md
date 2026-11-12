# notice：服务端发全服公告与跑马灯（经 chat 的世界频道）

一个很薄的可信入口：把一段文字以系统身份发到 chat 的世界频道，存进频道历史并推给本进程在线的玩家。公告与跑马灯的区别只是文字前缀。没有玩家协议，没有定时播放。运维命令在同时装着 gm 模块时才有。

## 能力

**客户端协议**：无。客户端收到的就是 chat 模块的世界频道消息（推送 `ChatMessage`，漏掉的用 `ChatHistory` 补读），文字以 `公告：` 或 `跑马灯：` 开头，按前缀选表现。

**服务端入口**（`game/notice/notice.go`，在 Nest 事务之外调用）

```go
publisher, err := notice.FromRegistry(registry)                     // 能力名 notice.publisher
err = publisher.Publish(ctx, "announce", "维护完成", "maintenance-20261112") // kind 是 "announce" 或 "marquee"
```

`Publish(ctx, kind, text, key)`：`key` 是这一次业务事件的稳定键，同一个键的内容不能变。实际调 chat controller 的 `AnnounceWorld(ctx, 前缀+text, "notice:"+kind+":"+key)`。

**运维命令**：本模块自己不登记。同时装着 `gm` 模块时，联动文件 `game/gm/notice.go` 经 gm 的登记点提供两条（经 ops 的 `/admin` 入口执行）：

- `gm.notice.announce`：发公告。
- `gm.notice.marquee`：发跑马灯。
- 两条的 payload 都是 `{"text": "维护完成", "key": "maintenance-20261112", "note": "工单 42"}`，请求还要带 `operator`；成功返回 `{"kind": …, "key": …}`。操作者与备注必填、执行前后各记一条审计日志（按 `trace_id` 对齐）、拒绝时 `result.data.code` 带码：输入不合法 `gm_invalid`，notice 没启动或已停止 `gm_unavailable`，chat 的错误原样返回（internal）。
- 没装 gm 模块时没有运维命令，只有上面的服务端入口。

**登记点**：无。

**状态**：无。消息存在 chat 服务的世界频道历史里。

**配置**：无。模块启动入口是 `notice.Start`（清单的 `runtime`），只发布 `notice.publisher` 这个能力，不带后台任务，不往运维命令表里登记东西。

**错误码**：模块没有自己的业务码（没有玩家协议）。`Publish` 返回两个可用 `errors.Is` 判断的错误：`notice.ErrInvalid`（类别、正文或键不合法，没有调 chat）、`notice.ErrStopped`（没启动或已停止）；chat 的错误原样返回。

**日志**：服务端公告发布确认记 Debug，记录 kind / key，不打印正文。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/notice/notice.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：没有玩家发公告的协议；不周期播放、没有定时器与调度；不做定向公告（按服、按渠道、按玩家）；不做富文本与多语言；不单独存公告列表，也没有“登录时弹出最新公告”的入口。
- **大小**：前缀加正文总长至多 256 字节（chat 的 `MaxTextBytes`），必须是合法 UTF-8，正文不能全是空白；`key` 必填、至多 64 字节。
- **幂等的范围**：同一个 `kind` + `key` 在 chat 的保留期内不会存第二条历史；但重试时在线推送可能重复，客户端要按消息序号去重。
- **只推本进程的在线玩家**：沿用 chat 的在线名单，多个 game 进程之间不互相扇出。别的进程上的玩家靠 `ChatHistory` 补读，或者每个进程各调一次（用同一个 `key`，历史只存一条）。
- **运维命令只在装了 gm 模块时才有**：两条命令经 gm 模块的 `Register` 登记，沿用它的必填检查、审计与带码拒绝；没装 gm 时不登记任何运维命令，要发公告只能由工程自己的服务端代码调 `Publish`。安装不会自动打开运维 HTTP 入口，也不会填令牌（见 gm 模块的 README）。
- **运维命令可以重试**：同一个 `key` 重发不会多存一条历史（见“幂等的范围”），但在线推送可能重复。
- **停止之后拒绝**：模块停止后 `Publish` 返回 `notice.ErrStopped`（文案 `notice: publisher is stopped`）。
- **前提**：依赖 `chat`（chat 模块要求业务服务 `uses: [chat]`），取不到 chat 的能力时启动失败。没有别的前提：不要求 ops / admin 命令表。
- **已知限制**：[KNOWN-LIMITS“场景模块的边界”](../../docs/maintenance/KNOWN-LIMITS.md)（notice 不默认调度）与“chat 模块的边界”（在线名单每进程一份、不跨进程扇出）。

## 复用方式

- **需求正好是它**：`roost module add notice`，连带装上 `chat`。
- **需求多一点**：
  - 周期播放：用工程自己的调度器，每次播放用一个新的稳定键调 `Publish`（例如 `活动号-第几次`）。
  - 新的展示类别：在 `notice.go` 的 `Publish` 里加一个 `kind` 与前缀；要运维命令时在 `game/gm/notice.go` 的 `init` 里再 `Register` 一条。
  - 定向或带结构的公告：不适合在这里改，另写协议。
- **别的代码要用它**：`notice.FromRegistry(registry)` 之后 `publisher.Publish(ctx, kind, text, key)`。在 Nest handler 里不能直接调（它会等 chat 服务），要放到事务之外，或者经 effect 在提交之后发。
- **只适合借鉴的情形**：需要“公告列表 + 已读状态”的公告系统。值得借鉴的是用稳定业务键把“发一次”做成幂等，以及复用已有的 chat 频道而不另建广播通道。

## 联动

- **`with/gm`**（`game/gm/notice.go`，两个模块都装着时写，不论先装哪个）：把 `gm.notice.announce` 与 `gm.notice.marquee` 经 gm 的登记点装进运维命令表。命令在实体锁之外调 `Publish`，不经 Nest（notice 不改实体）。4 条场景（`^TestWithNoticeGM`）。

别的模块没有 `with/notice`。

## 验证范围

- `roost module verify notice`：8 条场景，都在 `game/notice`。
- 离线：多数场景用只记调用的 chat 替身；一条经正式的 chat controller 验证系统发布的形状；一条是可编译变异（去掉空白文本的拒绝后，行为场景必须失败）。
- 覆盖：公告与跑马灯各自的键空间、非法输入在发布前被拒（`ErrInvalid`）、chat 的错误原样传出、启动不登记任何运维命令且停止后拒绝（`ErrStopped`）、缺依赖时启动失败。
- 与 gm 的联动 4 条（`game/gm`，chat 服务是替身，notice 的 `Publish`、chat controller、gm 的审计包装是真实代码）：两条命令带齐操作者与备注时发布，执行前后的审计日志按 `trace_id` 对齐、成功的结果码是 0；缺操作者、缺备注、备注为空白时答 `gm_invalid`、不执行也不记执行日志；正文或键不合法、带未知字段时答 `gm_invalid` 且执行后那条带这个码；chat 失败原样返回、notice 停止后答 `gm_unavailable`。
- 没有验证：真实的 NATS / Redis / TCP，多进程广播，真实的 ops HTTP。
