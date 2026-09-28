# Core 全部 service 域审查（2026-09-29）

本轮完成 **10/10 服务域的主调用链审查**，确认 9 个动态反例问题、1 个源码接线缺口（2 P1、7 P2、1 P3），只提交文档。问题及实施验收条件在 [问题交接](../bug/REVIEW-2026-09-29-services.md)；原始反例保存在 [复现材料](../bug/REPRO-2026-09-29-services.md)。没有验收或关闭此前的修复，也没有修改另一个 agent 负责的模块。

## 基线与证据边界

当前单仓模块 `github.com/tjbdwanghaibo/roost-core`。范围包含 Core 仓库的 `service/` **及** `kit/service/`，不是只看三个已移到 service 的域。初始基线 `b8a401ab75206bc8a807cb135499296c08a3e982`；9 月 29 日 fetch 成功并同步至 `6b73289cefd4e300a75f2550d4d68646172294ad`，两者的 service 范围无 diff。

前阶段 MCP `Transport closed`，使用源码补证；本次恢复连接后重查 `list_projects` / `index_status`。`roost-core` 图谱 generation=`2026-09-20T01:02:19Z`，状态 ready、显示当前 Git SHA，但不能用这个 SHA 推断节点已经刷新。`D-whb_s` 的 activity 查询仍命中旧 cube-kit/roost-service；未把旧实现当作当前证据。

当前 96 个生产 `.go` 路径逐一调用 coverage：20 个 `metadata_changed`、76 个 `not_tracked`；两个 scope 无记录 gap，不能推导审查完整。邮件图谱发现 Deliver 后，双向 depth=1 trace（13 callees、2 callers，无分页）及 snippet 仅用于定位，关键结论回读当前源码。其他域因现有图谱不足采用源码与行为测试；不宣称完成 Tier 3 图谱审计，也不重启共享 watcher。详见 [逐路径清单](evidence/service-review-20260929/inventory.csv)。

## 模块结论与进度

| 服务域 | 本轮主链范围 | 当前结论 | 后续验证 |
| --- | --- | --- | --- |
| account | 登录/身份、server、slot/name/role 创建补偿、签名 session、Redis 装配 | RR-03；同账号身份粒度与补偿所有权不一致 | 多写结果不确定、进程崩溃与恢复 |
| directory | Normalize、Reserve/Commit/Cancel/Release、token/TTL、Redis/Mod | 本轮未新增独立缺陷；同 owner Reserve 是幂等复用，不是再获得一次资源 | 作为 account 修复的契约对照 |
| global routing/lease | route CRUD、租约 Acquire/Renew/Release、版本与过期、RPC/Mod | 本轮未新增可确认缺陷；过期在访问时判定，lease 不设 key TTL 的解释成立 | 真正双进程迟到续租/接管，与业务 fence 接入 |
| global/activity | Open/window、Notify/aggregate、Participant/ledger、dispatch/ACK/reopen、owed/分页、定时器 | RR-02、08；跨对象恢复与最后 ACK 窗口有缺陷 | game 真接收、断线/重启、多窗口及故障交错 |
| mail | Send ledger/envelope、direct/broadcast、邮箱分页、三段 claim/墓碑、Redis/Mod | RR-04；可见性到期与存储容量脱节 | 存储缺失、claim 在途到期及旧数据迁移 |
| match | queue key、Enqueue/Cancel/Candidates/Commit、Grouping、请求历史、Redis/runner | 本轮未新增可确认缺陷；历史问题和历史状态容量不据绿测宣布关闭 | 长生命周期队列内终态增长、队列公平性与争用压测 |
| platform | 订单校验、pending 索引、attempt/backoff/失败回写、Admin、Redis/runner | RR-01；预算耗尽不能证明没有外部在途动作 | 接收端回执、重启恢复、人工退款与迟到结果 |
| rank | member 编码、排序/范围、Submit/Lua CAS、request ring、Remove/Reset、RPC | RR-05、06、10；真实 Redis 复现 | 编码迁移、revision/Remove/Reset 交错、热点榜容量 |
| session | Enter/claim/request ledger、Attach/Finish/Leave/Get、Releaser、Admin/Sweep、Mod/run | RR-09 源码接线缺口；懒清理不能替代宣称的自动清理 | 可配置 roster、释放回执/重启、公平分页 |
| chat | rule/ref、身体注册/策略、Append/History/去重、Retention/Redis、RPC/Mod | RR-07；自定义配置破坏存储隔离 | 旧 key 迁移、规则组合唯一性、客户端限流/授权接入 |

辅助阅读覆盖 metrics 别名、split 组装/消费示例与 integration 契约。清单为 **96 个生产 Go 文件**，其中生成文件通过一致性门禁并抽样阅读，其他路径按当前任务定位主逻辑、构造与接线；不是每一行、每一种配置或每一种失败交错全部验证。10/10 是本次选定服务域的主链覆盖进度，不能等同 100% 文件/分支/架构正确性。没有把历史三仓 18.74% 引用率转换成当前单仓覆盖率。

## 执行结果

Windows amd64、Go 1.27.0、CGO/race 可用。独立工作树与独立本机 Redis 8.8.0（loopback 16389、无持久化）避免干扰其他 agent。三个核心服务及 kit/service 原有测试通过；最终基线：

```powershell
$env:REDIS_ADDR='127.0.0.1:16389'
$env:ROOST_REDIS_TEST_ADDR='127.0.0.1:16389'
go test -race -tags integration ./service/... ./kit/service/... -skip '^TestReview' -count=1 -json
```

15 个有测试的包通过，servicemetrics 别名包无测试。JSON 的 pass 事件 684（包含父测试/子测试，不是 684 个独立场景），无测试级 skip/fail。前一次未设置 `ROOST_REDIS_TEST_ADDR` 时四个 platform 索引测试 skip；补齐环境后四项全部通过，未把 skip 隐去。

9 个新增 `TestReview` 反例断言“应有的行为”，在基线全部失败；所有反例带 `-race`，未出现 race detector 警报。rank 三项明确接真实 Redis，其余为内存状态机/可控时钟/单点故障或通道交错，不能外推为真实分布式故障。生成检查按原 go:generate 的 dir/emit/out 执行 `-check`：9 个 RPC 服务、12 次检查（mail/match/session 分离两半），全部 exit 0，写入零文件。

不需要为了 docs 修改重跑全部框架大测试。本轮没有真实 NATS/etcd/Mongo、多节点、故障强杀、生产业务网关/真实资金和性能压测。

## 设计与性能评价

接口、domain 与 Mod/Server 拆分，加上 versionstore 的 CAS/索引和 servicemetrics，适合通用游戏服务。当前 `kit/service` 仍包含 account/chat/global/activity/platform/rank/directory 的实质 domain 逻辑；mail/match/session 已主要为别名与装配。同处 Core 仓库后这是包职责的现状，不是另一仓“仍偷偷实现”的问题。若继续收敛职责，先固定契约，再逐域迁移，避免在改包路径时顺便重写状态机。

主要设计风险是：多个独立 CAS 不构成跨对象事务；退避时钟不是外部操作的完成证据；有界去重只在明确窗口内成立；自动清理要有可达且有界的候选来源。优先修 RR-01/02，再处理所有权、容量与隔离，最后收口范围边界。

性能为源码级评价，无数值承诺。mail 页读取已经批量 GetMany，但分页前需要遍历/排序邮箱；queue 和 activity/window 的整对象 CAS 会随记录增长增加序列化与冲突成本，须区分准入数量和历史/Delivering 数量上界；rank 成员编码 + Redis sorted set 避免整榜重排，但热点 owner 仍有读/Lua/Rank 往返及重试。优先利用既有原子索引、批量读取、RetryBackoff 和有界候选分页，先测对象尺寸/重试次数/p95/p99，再决定是否拆键；本轮不凭直觉承诺优化倍数。

## 停点

本轮请求的 10 个服务域主链与文档交接完成。下一轮优先验收 RR-01/02 的反例及不确定结果恢复，再做 match/activity 的历史积压容量、session roster 与正式 game 交付的补测；用户说“没有修复”时直接转新范围，不重复验旧 bug。上述专项仍未完成，不应写成全框架或全部服务已经无问题。
