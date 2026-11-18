# main 增量审查 · 2026-11-18

从 `8940ca73c` 到 `59e2d352a` 共 43 笔提交，包含 v1.55.0 和其后的分游戏账号、portal CORS。收尾追加审查 `8a2cf3a9e` 的登录并进服。两次检出之间，本报告五个问题点的代码相同。最需要先处理的是合服保护漏检，以及房主交接时旧状态再次生效。

| 编号 | 级别 | 影响范围 |
| --- | --- | --- |
| RV-20261118-05 | P2 | 合服预检翻页，可能带着始终在途的换服成功写入合服关系 |
| RV-20261118-06 | P2 | 大厅中新预留席位被旧在线报告标记为已连接，过期后继续留座 |
| RV-20261118-07 | P2 | Go SDK 正在压缩的主动交接可跨越房主任期生效 |
| RV-20261118-08 | P3 | portal 接受显式默认端口的 Origin 配置，合法网页预检却被拒 |
| RV-20261118-09 | P3 | Go 自动快照测试在取状态时就计为已发，产生偶发误报 |

**05 · 合服漏检。** 在途索引按期限排序，每页 200 条。构造前 200 笔与被并服无关、第 201 笔目标正是被并服；读完第一页后，通过正式 Abort 接口结束第一笔。原第 201 笔移动到第 200 位，下一页仍从 offset 200 开始，正好跳过它。实际 MergeServer 返回成功，服 2 已指向服 1，目标为服 2 的换服仍在途。三轮复现，索引稳定时三轮均正确拒绝。这笔换服在扫描开始前就已存在，有别于已记载的检查后新发起窗口。使用实际内存索引，Redis 的 IndexPage 也用 offset 分页，本轮未在 Redis 集群复跑。建议为合服预检提供稳定读取，例如索引变更计数前后不一致就重扫或拒绝，再保留提交时防线。[scanTransfers](https://github.com/tjbdwanghaibo/roost-core/blob/59e2d352a/service/account/transfer_index.go#L106)

**06 · 席位重建丢失水位。** 原席位收到 seq 3 主动离开并被删除。玩家在开局前重新 join，只取得预留席位，没有再次连接 relay。此时上一席位的 seq 2 在线报告晚到，因为新席位的 PresenceSeq 是零，notePresence 把它记为 Connected。开局后即使预留过期，再来只有房主在线的心跳，该席位仍被保留，玩家还能按重连回座。三轮乱序均出现，按序对照三轮正确释放。建议创建新预留时至少保留创建时已知的房间水位作为下界，并测试同一账号跨两次预留的乱序。[创建预留](https://github.com/tjbdwanghaibo/roost-core/blob/59e2d352a/service/lobby/rooms.go#L198) · [在线事实合并](https://github.com/tjbdwanghaibo/roost-core/blob/59e2d352a/service/lobby/events.go#L179)

**07 · 主动交接仍会带出旧任期状态。** Go SDK 的 Transfer 在压缩前检查房主身份，压缩完成后直接发送。A 在任期 0 发起交给 C 的请求，在压缩处暂停；期间 A 正常交给 B，B 携新状态交还 A。释放旧请求后，它返回成功，真实 relay 开启任期 3，C 收到旧交接数据。三轮结果一致。旧快照修复在 Snapshot 入口有效；Transfer 这条路径从 v1.54.0 就存在，本轮扩大入口覆盖后确认。建议将交接命令绑定到发起时的连接和房主任期，并在 relay 应用命令时拒绝过期任期。实际探针使用 Go 客户端。[Transfer](https://github.com/tjbdwanghaibo/roost-core/blob/59e2d352a/client/relay/host.go#L332)

**08 · 默认端口导致白名单失配。** 配置检查接受 `https://play.example.com:443`，但该网页的序列化 Origin 是 `https://play.example.com`。实际 Preflight 返回 403，去掉配置中的默认端口立即得到 204。建议统一规范化配置项或在启动检查中拒绝并提示应填的值。当前部署可直接省略 HTTPS 的 443 和 HTTP 的 80。[CheckCORSOrigins](https://github.com/tjbdwanghaibo/roost-core/blob/59e2d352a/infra/network/httpserver/cors.go#L80)

**09 · 测试使用了错误的同步点。** 原始 Go relay 整包及定向复跑均出现 `offer seq 2 state "auto-2" (pushed 3)`。pushed 在 SnapshotState 回调内递增，压缩和发送尚在其后。另一个 goroutine 的 Ping/Pong 只能确认已排在 Ping 前面的帧。让第三次取状态停在计数之后、发帧之前，三轮都观察到同样结果。建议测试等待可观测的第三份快照发送完成或 relay 已收，再触发离开。这是既有夹具问题。[夹具等待条件](https://github.com/tjbdwanghaibo/roost-core/blob/59e2d352a/client/relay/host_test.go#L266)

## 历史问题与验收

- v1.53.0 的满邮箱换服与 room 重试原场景在生成工程内 race 五轮通过，C# 错误码冲突回归和真实编译通过。
- v1.54.0 的 TS 跨任期旧快照按原双交接探针复验，接班人恢复 epoch-two-state；原大厅乱序 left 探针三轮通过，Linux wsconn 整包 race 通过。这三个原问题可按原复现场景关闭。
- 在 `59e2d352a` 生成并安装 room、mailbox、rehome、team、portal、login 及依赖，250 个模块场景零跳过，build、vet、ID、生成漂移检查通过。TS 完整套件 71 项通过，C# SDK 866 项检查通过。account、mail、lobby、wsconn、errcode、robot/session、robot/scenario、remoteentity、httpserver、webroute、wiring/account 和 wiring/dataengine 包 race 通过。Go relay 整包的首次失败单独保留。
- 收尾提交的 portal/login 复用 servers 与 enter 路径。指定服、推荐服、无推荐、非法参数及普通登录的行为见该提交的生成模块测试。针对该版另装 portal 校验，结果见 verify8.json。
- 环境为 Linux，联机用例使用本机传输与内存房间目录。生产存储集群、压测、Unity Player 和真实账号渠道不在本次执行结果内。代码图谱不可用，定位依据为 Git diff 和源码。

[审核探针与执行结果](evidence/main-review-20261118/README.md)
