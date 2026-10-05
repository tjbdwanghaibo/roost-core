# 2026-10-05 接续 review：N05 mirror 与路由接入

基线 `b2232db528de9947a2cf1cccaed79e7a9e29d2dc`；fetch / ff-only 已最新，没有新增 Go/Wanted/bugfix 文档待验收。canonical coding/bugfix/optimize 与本机镜像一致。按上轮停点转 N05，发现两个 P2 并按本次授权修复：[NC-33](../bug/RR-20261005-NC-33.md)、[NC-34](../bug/RR-20261005-NC-34.md)，均声明场景已验证，未发版。

## 范围与结果

当前读取 11 个生产文件：ownerroute 两文件、通用 Replicator、cache mirror、LocalStore/Store、Remote interest/syncer/assembly/assemble、Kit Remote Mod。读取相关精确回归；snapshot L2 只作历史风险定位，未全量重新验收，不计当前文件覆盖。没有重开另一线 Nest/Sync/DataEngine 完整专项。

| 入口 / 场景 | 本轮结论 |
| --- | --- |
| 通用缓存第三层身份 | 2 正式副作用反例红→绿；错误消息后键7/8不变、合法更新可恢复 |
| Interest第三层身份 | renew/release × scope/SID/expiry/op，8正式副作用反例红→绿；原lease与容量保持 |
| 兼容与恢复 | 无VersionOf/普通Delete、null指针拒绝后恢复、Generation=0、旧release/空Delete/新release控制通过；合计13个新增正式叶子（10反例+3控制） |
| Generic Replicator | 复用既有内外层topic/key/version/op、旧字段补全、取消、独立MessageID回归；不是新增覆盖 |
| Snapshot接收 | 复用既有payload身份与其他Remote包回归；不重复登记09-13已修Snapshot根因 |
| Ownerroute | 当前源码与既有本地/远程/缺依赖/ownerless回归核对；本轮无新确认RR，不宣称权威权限/业务ACK已由路由器提供 |
| Kit与Assembly | 配置/能力/启动停止由Kit转发core Assembly；仅增量边界审查，未声称完整生命周期HA已独立验收 |

[学习文档](IMPLEMENTATION-MIRROR-PAYLOAD-IDENTITY-AND-ROUTING.md)解释三层身份、数字语义、路由与关闭；[修复/运行证据](../bugfix/evidence/noncore-bugfix-20261005-13/README.md)保留真实红日志与最终检查。测试数不是全仓覆盖率。

最终592相关race叶子/8skip、根包14、build/vet/glsvet通过；另按正式生成双模式Sync消费两个叶子race通过。原Bash脚本的Windows replace路径失败与PowerShell等价复跑分别留档，不将环境失败当产品反例。

## 图谱证据

Tier 2 Verify；roost-core root D:/whb_s/cube-core，ready 32285 nodes / 209828 edges，generation `2026-09-30T11:58:14Z`。search_graph→trace_path→get_code_snippet 后检查22候选路径及相关scope。旧generation/metadata_changed与新测试not_tracked均用当前源码补证。无记录缺口不能证明完整性。

ownerroute候选29、mirror35完整；remote snapshot/interest查询106分80+26完整分页，测试针对性查询完整。cache泛查询70/76仅候选，未穷尽；依赖实际已知文件定向补证。receiver同名ApplyReplica的trace合并了不同实现，并有跨包误边，未将其当业务身份或无调用证明。[coverage与摘要](evidence/noncore-review-20261005-23/README.md)。

## 进度与下一入口

N05 **场景部分完成**，本轮新增13正式叶子与两个闭合缺陷，不把11文件分摊到历史24文件分母，也不计completed/15。下一补 N05 的 BindSync/Assembly失败重订阅、停止/在途回调交错与snapshot gap/epoch/schema权威回填组合；优先新Wanted/变更。跨节点L2删除水位、真实broker ACK/retry/断线恢复、时钟回拨、HA与长期容量继续保留。N04本机具名迁移链沿用上轮，外部留项不关闭。

Mirror仍按已有独立DTO reader方案交接维护者；本次只修既有适配器，没有实现MirrorClient/只读生成物。静态PlayerOwner仍是另线方案；App首批实现的最后同步见下节，不将整合误计完整验收。本轮本地验收后提交推送，不查询/等待GitHub CI，不发版。

## 最后同步：App单实例锁第1阶段

提交前再次fetch发现d4ac9853/c9b934ae，已正常rebase整合。18文件增量包含App单实例锁与OnFail、Kit Redis store与Nest fence接入，以及文档/本地测试；无Wanted增量。该实现由另一线负责，本轮未修改它，也未逐项独立审完610行状态机及真实Redis/进程接管。

本轮核对App.run增量与完整RuntimeFailure当前源码，后者在锁外调用hook、捕获hook panic并最终通知Done；未将旧图谱的Fail片段当新源码。图谱覆盖补查为[upstream-coverage](evidence/noncore-review-20261005-23/upstream-coverage.json)，新Singleton为not_tracked，因此不依赖结构图缺失作否定结论。补充[源码身份](evidence/noncore-review-20261005-23/upstream-source-hashes.csv)与原22路径哈希核对一致，N05正式修复红绿仍有效。

首次整合扩大至原相关包+app/kit-nest/kit-redis的race **708叶子通过（769通过事件）/8skip**，根包14、全仓build、扩大vet与glsvet均通过，见[首次整合矩阵](../bugfix/evidence/noncore-bugfix-20261005-13/upstream-checks.json)。这些是既有单测与编译的独立运行，不能代替singleton从获得/失去锁到外部写停止的全过程证明。

方案当前仅第1笔已实施，第2～5（含2b/3b）未实施；codegen生成链、静态玩家绑定和真实进程演练不能据本轮合并算完成。作者所述真实Redis/Cluster结果是作者证据，本轮未重跑。本轮T编号改为214/215，保留上游212/213；未改CI门禁或等待其结果。下一N05停点不变，新App全过程另按具名独立验收任务接续。

末次又整合e3810ef1：作者修正持有窗口末段的单次续期超时，取min(renewInterval, validUntil−asked)，防止合法14/3/3参数下超时结论晚于键到期。当前cas源码与该增量已核对；作者修前红未在本轮重做，新增时间预算回归已随扩大矩阵独立通过。最终 **709 race叶子（770事件）/8skip、根包14、build/vet/glsvet通过**：[末次矩阵](../bugfix/evidence/noncore-bugfix-20261005-13/upstream-final-checks.json)、[末次源码身份](evidence/noncore-review-20261005-23/upstream-final-source-hashes.csv)。它仍不证明真实进程/不合作adapter/调度暂停的全链fail-stop验收。
