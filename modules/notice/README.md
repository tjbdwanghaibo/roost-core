# notice

`roost module add notice` 自动安装 chat，在服务启动时发布 `game/notice.Publisher` 并注册两条 GM 命令：

- `gm.notice.announce`：公告。
- `gm.notice.marquee`：跑马灯。

两者 payload 均为 `{"text":"维护完成","key":"maintenance-20261112"}`。`key` 是稳定业务事件键，必填且最多 64 字节，同键内容不可变。GM 走已有 admin 鉴权与审计；安装不会自动启用 HTTP 运维入口或填入令牌。

可信服务端在实体事务外调用 `notice.FromRegistry(registry)`，再调用 `publisher.Publish(ctx, "announce"或"marquee", text, key)`。无玩家发布协议，无周期播放、定时器或后台任务。需要周期播放的项目自行用每次播放的稳定事件号调用。

发布复用 chat controller 的 `AnnounceWorld`：以系统身份存入世界频道历史，并推给本进程在线名单。文本带 `公告：` 或 `跑马灯：` 前缀，客户端可按前缀选表现；包括前缀总长最多 256 字节。推送是捷径，漏掉的消息由 `ChatHistory` 补读。chat 保留期内同键不新增历史，但重试可能重复在线推送，客户端按消息序号去重。沿用 chat 的本进程在线名单边界，未增加跨 game 进程扇出能力。

8 个独立离线场景验证服务入口、输入拒绝、错误传播、GM 注册/停机和正式 chat controller 的系统发布形状；可编译变异去掉空白文本拒绝后，行为场景必须失败。未声称真实 NATS/Redis/TCP 或多进程广播验收。

本轮验证（macOS，2026-11-12）：`GOWORK=off go test -count=1 -run '^TestModuleCdkeyNoticeInstallAndVerify$' ./codegen/internal/roost` 通过，全新自定义 module 工程得到 notice 8 PASS / 0 SKIP，build、vet、ID、生成漂移检查全部通过；生成工程 `GOWORK=off go test -race -count=2 ./game/controllers/cdkey ./game/notice` 通过。
