# alliance

`roost module add alliance` 安装独立联盟服务、game 侧 PB 端点、只读 Mirror、Player 展示组件与三个 Saga；依赖 bag。新工程无需手写接线即可生成、build 和 verify，运行需 Mongo / Redis / NATS / etcd。鉴权、连接监听与实际部署仍由工程配置。

## 权威与调用链

- `Alliance` 与 `AllianceMember` 是 `service=alliance` 的 Remote managed 实体，分别保存名单与成员守卫，只有联盟服务实例能写。game 即使因 team 安装了 remote_entity，也不能认领这两种实体。
- 只改联盟的 Join / Leave / 申请批准 / 踢人 / 转让 / 宣战走正式路由 RPC；同实例的多实体写仍在同一 Nest 事务里。跨实例入盟保留预约、名单裁决与守卫惰性修复，不用 Saga 包住每次调用。
- 建盟扣费、捐赠、领取奖励涉及独立的 Player 与联盟提交域，才使用 `alliance_found` / `alliance_donate` / `alliance_reward`。Player 步骤按 sid 路由，原生回执与背包同事务；联盟步骤保留 RPC 请求号去重与 Mongo 收件箱。
- `game/controllers/alliance` 提供 FoundAlliance、JoinAlliance、AllianceInfo、LeaveAlliance、DonateAlliance、ClaimAllianceReward、AllianceOpStatus 及通知；协议号在安装时分配，客户端以生成的 msgid / PB 为准。
- `PlayerDao.Alliance.ID` 是持久展示副本，不是权限依据。AllianceInfo 用 Monotonic Mirror，写后可带 MinVersion。成员守卫不生成 Remote 快照。

工程可修改 `game/alliance/rules.go` 中的货币道具（默认 bag 表的 1003），以及 Controller 的建盟费（默认 100）。demo 只覆盖规则为它已有的道具 9001；模块没有复制第二套经济系统。服务成员上限、预约期限、奖励和回执保留由 `internal/rpc/alliance/settings.go` 声明。

## 跨服与惰性校正

与 enter 同装时，每次进入/重连会调用 `Controller.RefreshMembership`。全部查询共用 1 秒预算，在 IO 上下文询问当前绑定及权威 Membership，再用正式 Nest 更新 Player 展示；只有确定的实体不存在才清理，超时/服务不可用保留展示并记日志，不阻塞登录成功。自定义进入链可直接调用这个公开方法。

与 rehome 同装时，Saga 在进入 Player Nest 之前调用 `rehome.Binding`：换服中退回等待，已到别服改投当前 sid，未知结果重投。未装 rehome 的工程采用可信会话/持久载荷中的静态 sid。自有驻留表或闲置卸载策略可实现 `game/alliance.PlayerBinding` 并在**消费者启动前**发布 `PlayerBindingCapability`。demo 在 `internal/service/game/alliance.go` 中先发布 PlayerOwners 适配，再手动启动模块 runtime；manifest 关闭该模块的自动 runtime，避免启动两份消费者。

联盟成员允许换服。对账拿到相同入盟批次的 Mirror 后，通过 RefreshHome RPC 比较 `JoinRequestID` 和旧 `HomeSID` 再更新通知地址。退盟重入、旧 SID 已被替换都不会被迟到请求改写。HomeSID 只是尽力通知提示，不参与权威写权限；account 查询与该 RPC 不是跨服务事务，快速往返换服仍可能暂时留下旧提示，后续进入对账继续修正。成员通知至多一次，只更新在线玩家；离线/漏通知靠进入时对账。

## 安装与文件所有权

安装器在一个模块写入事务中新增 `services.alliance`、uses_rpcs、实体生命周期、Saga 定义和 Player 原生回执；生成失败恢复这些文件，重复安装不改工程。已存在同名业务服务或 Saga 时拒绝接管。只读 remote_mirror 升级为 remote_entity 时去除重复 Mod，其他服务的读能力保留。

新服务开发配置使用 `data/wal/alliance`，与 game 分开；同机第二个 alliance 实例仍须独占 SID、端口与 WAL 目录。生产每实例独占持久卷。运维沿用 alliance.info / guard / transfer / stop_founding。

## 验证范围

49 条基础场景、enter/rehome 各 2 条联动，覆盖权威名单/守卫、重复请求、跨实例替身路由、补偿、展示、HomeSID 条件修正和提交失败回滚。详细结果见[本轮证据](../../docs/maintenance/evidence/module-alliance-20261112/README.md)。生成工程的内存总线、提交器与 Remote 替身不等于真实双进程持久验收；完整模块正反序、真实存储、TCP/Robot 与换服 kill -9 仍归交接 §5/§6 最终批次。没有为旧 guild API、字段或旧生成工程保留兼容层。
