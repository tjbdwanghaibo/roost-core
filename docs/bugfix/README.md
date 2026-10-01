# Bugfix 记录

**09-30 Codegen 第四批：**[RR-CG-12 false 索引](RR-20260930-CG-12.md)、[RR-CG-13 命名空间](RR-20260930-CG-13.md)、[RR-CG-14 依赖迁移](RR-20260930-CG-14.md) **3/3 已修复并在具名场景验证，未发版**。正式回归先红后绿；全 Codegen race/vet、glsvet、正式 project deps 消费与双模式 Sync 生成链通过，具名 shell 环境项跳过。[运行/进度](../review/REVIEW-2026-09-30-codegen-06.md) · [原始证据](evidence/codegen-bugfix-20260930-12-14/README.md)。

**v1.18.0 已发布（2026-09-30）**：RR-20260928-15、RR-20260930-03/11 与 B 线 RR-20260929-01～34、RR-20260930-01/02/04～10 随本版发布；下面各行的“未发版”均指本版之前的状态。

[RR-20261001-06](RR-20261001-06.md)：名字被别的账号 committed 后同名重试自动释放 pending slot（仍答 `ErrNameTaken`，孤儿未发布角色记录保留且不可玩）；新 owner-only `Admin.ResolvePendingCreation` 备注写 `Account.admin_note`、版本 + 身份围栏 DeleteIf 释放，名字仍被本计划 reserved / committed、已发布、legacy 空 slot 拒绝 `ErrNotResolvable`（560115）；Memory + 真实 Redis 真等租约过期先红后绿；T-182（未发版）。
[RR-20261001-09](RR-20261001-09.md)：activity sweep 对无 Intent 的 legacy Opening 过 `OpeningGrace` 回收名额（CAS 里只删仍无计划的那一段）；畸形 Intent 跳过、指标 `sweep.opening_intent_malformed`、日志只在出现 / 恢复时各一次、名额保留；单条 Create 失败在同组其余工作做完后再上报；Memory + 真实 Redis 先红后绿；T-181（未发版）。
[RR-20261001-08](RR-20261001-08.md)：chat `pageOf` 去掉“到达 ring 头部且头部序号 > 1”这条洞判定，普通容量淘汰后的无游标最新页 / 翻到保留边缘 `Gap=false`、不计 `history.gap`；Gap 只剩页内洞、游标点名消息已不在、尾部缺失三种；Memory + 真实 Redis 先红后绿（未发版）。
[RR-20261001-07](RR-20261001-07.md)：game-demo `PlayerOwners.Claim` 扔副本失败后 `abandon` 本地租约状态——刷新循环不再续、`confirmRenewal` 不再清 `interrupted`、`Admit` 持续拒绝，租约自然过期，恢复点是之后的 `Claim` 等清除完成；模板回归先红后绿；已生成工程手工合并 playerowner.go（未发版）。
[RR-20261001-05](RR-20261001-05.md)：activity `applyProgress` 把 ledger 条目已过期的 pending 证明回收（条目消失即越过 ReservationTTL 重试地平线）；满窗改报 `ErrProgressBacklog`（620119）不再是 `ErrConflict`；新 owner-only `Admin.ReconcileProgress` 补 ledger mark 后释放证明；Memory + 真实 Redis 真等 TTL 先红后绿；T-180（未发版）。
[RR-20261001-04](RR-20261001-04.md)：v1 manifest 遇未认领 JSON 的错误文本写明两条恢复路径（旧生成物删除；手写 JSON 移出 → 生成升 v2 → 移回）并指向 `CODEGEN_REFERENCE.zh-CN.md` §9.1（新增 manifest / 退役 / v1 升级一节）；失败设计不变（未发版）。
[RR-20261001-03](RR-20261001-03.md)：`roost generate` config-data 在 `configs/table` 没有 CSV 时只有 manifest `tables` 非空才跑 tablegen（新增 `tablegen.ManifestOwnsJSON`），schema 已写、CSV 未写的工程恢复 v1.17.2 的通过；RR-09 退役回归不变（未发版）。
[RR-20261001-02](RR-20261001-02.md)：`sameSendIntent` 去掉 `reflect.DeepEqual` 改逐字段比较（切片 `slices.Equal` / `bytes.Equal`，nil 与空等价），自定义 EnvelopeStore 还原空切片时 RR-20260929-16 的同 RequestID 恢复不再永久 `ErrConflict`；替换正文 / 期限 / 收件人仍拒绝（未发版）。
[RR-20261001-01](RR-20261001-01.md)：Redis job `env:` 补四个门变量、守卫正则扩展；根包 `TestCIRedisJobSetsEveryRedisGateVariable` 把测试文件里 `os.Getenv` 的 Redis 门变量钉到 ci.yml（未发版）。
[RR-20260930-24](RR-20260930-24.md)：game-demo 活动租约续租失败按 `errors.Is` 分类——过期 / 不持有立刻 `AcquireLease`，拿不到进 standby 每心跳重试，瞬时错误下周期再续，日志只在状态变化时打（未发版）。
[RR-20260930-23](RR-20260930-23.md)：game-demo `PlayerOwners.renew` 取回失效租约后像围栏一样 `CloseSessions`（日志加 `sessions_closed`），副本扔不掉的结局（`errStaleCopyKept`）同样关连接；两进程 + SIGSTOP 40s 演练实跑通过；T-179；已生成工程须手工合并 playerowner.go（未发版）。
[RR-20260930-22](RR-20260930-22.md)：game-demo gift deliver 收件人检查改读 Player DAO 的 `db.PlayerDaoDBName` / `PlayerDaoCollection`，隔离端到端 6 机器人通过；已有工程 `project sync` 或手改一处（未发版）。
[RR-20260930-21](RR-20260930-21.md)：释放 Redis 锁没有明确答复（错误用尽重试 / ctx 到期）后锁进入“持有状态未知”，下一次 `TryLock` 以 Redis 为准——租约仍是自己的就在同一条 Lua 里换新 token / 新 fence 重新取得，被别人持有走 NotAcquired；端到端用例去掉 Skip；T-178（未发版）。
[RR-20260930-20](RR-20260930-20.md)：`dispatchLoadedEntities` 兜底释放在自己的 recover 边界里跑，hook panic 以 `errors.Join` 并进在途业务错误；`runNestLogic` / `dispatchNest` 的 recover 改并入不覆盖；已提交路径 `ErrAfterCommitFailed` 语义与文本不变；两条生成链路端到端用例去 Skip（未发版）。
[RR-20260930-18](RR-20260930-18.md)：game-demo `Service.Shutdown(ctx)` 把 App 停机 ctx 传给 `Scene.Close`；回归模板 `service_shutdown_test.go`；已有工程需手改（demo 文件应用所有）（未发版）。
[RR-20260930-17](RR-20260930-17.md)：生成工程新增受控文件 `deploy/docker/compose_check_test.go`，对 `docker compose config --format json` 断言 tmpfs / read_only / user / cap_drop / security_opt / stop_grace_period / healthcheck / config bind / 命名卷；`ROOST_COMPOSE_CHECK` 开关，CI 与 `make compose-check` 设置它（未发版）。
[RR-20260930-16](RR-20260930-16.md)：`appendModConfigSections` 走 `lfText` / `restoreLineEndings`，CRLF 配置按 CRLF 追加（未发版）。
[RR-20260928-04 后续](RR-20260928-04.md)：stats_log 不做内建轮转，DEPLOYMENT §4 / §5 与生成 README 给 `copytruncate` 的 logrotate 示例（N30，维护者 09-30 拍板）。
[RR-20260930-15](RR-20260930-15.md)：实体实现必须是指针——契约 + `BuildEntity` / `TryAdd` 用 reflect 校验一次并点名类型，热路径比较不改；仓内值类型替身 `kit/nest` `reloadedEntity` 改指针（未发版）。
[RR-20260930-14](RR-20260930-14.md)：广播每个目标自己的 Guard 作用域，目标实例按实例释放、其余锁与 post-release 随作用域结束释放；补 Remote 目标 `ReleaseCast` 回归（未发版）。
[RR-20260930-13](RR-20260930-13.md)：`SetEntityVersion` 同一 fence 下拒绝 StateVersion 回退（`ErrRemoteVersionConflict`），接口改为 `SetEntityVersion(int64) error`（未发版）。
[RR-20260930-12](RR-20260930-12.md)：`durableCommit` 的 memory 早返回分支在消息带 Remote 批次时先做 `refuseCommitAfterFence`，fence 后 Durability 0 直写被拒并 Abort 批次（未发版）。
[RR-20260930-19](RR-20260930-19.md)：core 新增 `NewRedisMarkerWithKeyPrefix` / `ValidateMarkerKeyPrefix`（`<prefix>:remote_entity:marks`，空值键不变）；kit 不加配置项（Mod 不写 marks）；USER_GUIDE §6 三类 Redis 键清单，`lock_key` 隔离要求写进文档（未发版）。
[RR-20260930-11](RR-20260930-11.md)：`cache` 包 `TestReadThroughStoreCoalescesMisses` 偶发 `loads=2` 是测试假设过强（只等到 `loads > 0` 就放行），改为轮询 `Stats()` 把 8 个 goroutine 钉到合并点再放行，断言 `Loads=1 / Coalesced=7`；实现不动，不进 CHANGELOG（已修复，v1.18.0）。

[09-30 Codegen 第三批](RR-20260930-06.md)：[RR-06 Attribute](RR-20260930-06.md)、[RR-07 Event](RR-20260930-07.md)、[RR-08 Webroute](RR-20260930-08.md)、[RR-09 Tablegen](RR-20260930-09.md)、[RR-10 Errcode](RR-20260930-10.md) **5/5 原触发已修、声明场景已验**；表格退役额外通过隔离业务工程的暂存提交、`--check` 和消费者编译。[本轮验证](../review/REVIEW-2026-09-30-codegen-04.md)。旧 v1 表格 manifest 归属不明时需人工确认；未发版。

[09-30 Codegen 第二批](RR-20260930-04.md)：Entity/Nest 旧生成物退役 [RR-04](RR-20260930-04.md)/[RR-05](RR-20260930-05.md) **2/2 修复并按声明场景验证**；当时[五个新问题](../bug/REVIEW-2026-09-30-codegen-03.md)只审查未实施。该轮外部工程受模块缓存写锁限制，本轮已用隔离缓存完成消费者编译；未发布。

[09-30 Codegen 第一批](RR-20260930-01.md)：RR-20260930-01/02 **2/2 原触发修复并验证**，RPC 孤儿检查/清理、Protocol 空定义退役及上层暂存提交/漂移检查；[Protocol 明细](RR-20260930-02.md) · [继续 review 的新 RR-04/05](../bug/REVIEW-2026-09-30-codegen-02.md)未实施。未发布/部署，旧协议消费须按业务窗口迁移。

[09-29第九批Service修复](SERVICE-BUGFIX-2026-09-29-09.md)：[RR-34 Pipeline写错误被缺失掩盖](RR-20260929-34.md) **1/1已修/声明场景已验**，原3/3绿、正式race/count2 58叶子执行、整体19包951pass叶子，3Toxiproxy skip另列。签名/key/RPC不变，不回滚/重放。[阶段完成](../review/REVIEW-2026-09-29-services-11.md)：10域主链完成，RR-01～34沿各自验收关闭，本轮无新确认缺陷，外部/HA/容量设计另列。

[09-29第八批service修复](SERVICE-BUGFIX-2026-09-29-08.md)：[RR-33](RR-20260929-33.md) **1/1已修/声明场景已验**，现有Pipeline有界单key读取；原2/2绿、Mail race/count2 314pass事件/280叶子执行0skip。完整service+driver18包965pass事件、3Toxiproxy skip；全编译/生成consumer编译、vet、RPC12check通过。自定义窄客户端需补Pipeline，无key迁移；[新RR-34](../bug/REVIEW-2026-09-29-services-10.md)未修，未发布。


[09-29 第七批service修复](SERVICE-BUGFIX-2026-09-29-07.md)：[RR-31](RR-20260929-31.md)/[RR-32](RR-20260929-32.md) **2/2已修/已验**，Activity现有Cluster验证/多game恢复、购买首次durable grant保留；原8/8、新正式33叶子、integration+race17包909事件通过。已有生成业务需合并write-once补丁，未发布/迁移；[新RR-33](../bug/REVIEW-2026-09-29-services-09.md)仅交接。

[09-29 第六批 service 修复](SERVICE-BUGFIX-2026-09-29-06.md)：[RR-28](RR-20260929-28.md) / [RR-29](RR-20260929-29.md) / [RR-30](RR-20260929-30.md) **3/3 已修/已验**，原15叶子全过，新增正式43叶子、完整17包/826事件通过。原子缺失索引清理、Rank/Platform有效Cluster tag，不改键格式；旧数据对账/配置迁移、混合旧owner边界另列，未发布。[继续review新问题](../bug/REVIEW-2026-09-29-services-08.md)尚未实施。

[09-29 第五批 service 修复](SERVICE-BUGFIX-2026-09-29-05.md)：RR-25/26/27 与旧 Activity Opening 残余 4/4；Grouping 校验、Rank 溢出拒绝、Chat 年龄清理/Gap、Activity 持久计划与有界轮转。完整回归 16 包/830 事件通过，最终定向两模式各 27 叶子全过；Activity/Chat owner 升级与 legacy 处理有明确边界，未发版。

[09-29 第四批 service 修复](SERVICE-BUGFIX-2026-09-29-04.md)：RR-23/24 与旧 RR-10 删除残余已修复，正式 Memory/Redis、生成工程消费回归通过。


[09-29 Service 第四轮复核](../review/REVIEW-2026-09-29-services-04.md)：`bdbb61bc` 的 RR-19～22 当前实现与现有回归通过（16 包/794 事件；另补 Redis Account/Mail 15 事件），未外推全部交错。新增 RR-23/24 和旧 Mail 删除残余另见[问题](../bug/REVIEW-2026-09-29-services-04.md)，未实施。

[09-29 第三轮 Service 修复](SERVICE-BUGFIX-2026-09-29-03.md)：[RR-19](RR-20260929-19.md)、[RR-20](RR-20260929-20.md)、[RR-21](RR-20260929-21.md)、[RR-22](RR-20260929-22.md) 已实施；持久建角计划、预约代次/生成消费者、原子身份删除和返回所有权。Memory/Redis、16 包/794 测试事件通过，Go API/旧数据边界已记录，未发版。下方是此前时点的记录。

[09-29 修复后第三轮 review](../review/REVIEW-2026-09-29-services-03.md)：`83c04243` 现有 service/versionstore 回归 16 包、759 测试/子测试事件全过；邻近新增 RR-19..22 尚未实施，详见[问题](../bug/REVIEW-2026-09-29-services-03.md)。不将原触发通过外推到全故障窗口。

[09-29 Service 两轮修复总记录](SERVICE-BUGFIX-2026-09-29.md)：RR-20260929-01～18 + 旧正常 Finish ABA；含逐项 bugfix 链接、回归、API/存储升级限制与 roost-bugfix skill（未发版）。

[RR-20260930-03](RR-20260930-03.md)：`AtomicLocalStore` 时钟记录随存活键数有界（覆盖写 / Delete / 过期后压缩），修复 C01 24 小时堆单调增长（v1.18.0）。

[RR-20260928-15](RR-20260928-15.md)：生成的 player TCP / scene 测试先等会话登记再推送或计数（v1.18.0）。

[RR-20260928-14](RR-20260928-14.md)：生成工程按参数缓存为私有副本、慢用例并行（265s→73s）（v1.17.2）。

[RR-20260928-13](RR-20260928-13.md)：CRLF 与 LF 同样编辑，认不出结构统一 WARN（v1.17.2）。

[RR-20260928-12](RR-20260928-12.md)：先按当前 unit 停机再装入目标 unit 启动（v1.17.2）。

[RR-20260928-11](RR-20260928-11.md)：`Append` 对 pipelined 同样等 fsync（v1.17.2）。

[RR-20260928-10](RR-20260928-10.md)：unit 随 release 一起回退（v1.17.2）。

[RR-20260928-09](RR-20260928-09.md)：与 strict 一样交 finalizer 回滚、隔离、卸载（v1.17.2）。

[RR-20260928-08](RR-20260928-08.md)：等待失败除明确拒绝外带 `ErrRemotePersistenceIndeterminate`（v1.17.2）。

[RR-20260928-07](RR-20260928-07.md)：Secret 与 prod 示例同源（v1.17.2）。

[RR-20260928-06](RR-20260928-06.md)：prod / Secret 示例补齐（v1.17.2）。

[RR-20260928-05](RR-20260928-05.md)：数据装进 release，WorkingDirectory 改为 `$APP_ROOT/current`（v1.17.2）。

[RR-20260928-04](RR-20260928-04.md)：写失败计数告警，部署物给可写目录（v1.17.2）。

[RR-20260928-03](RR-20260928-03.md)：新增 `ErrRemotePartRejected`，判别表第 4 / 14 / 15 行（v1.17.2）。

[RR-20260928-02](RR-20260928-02.md)：撤销记录以（Manager, ID）为键（v1.17.2）。

[RR-20260928-01](RR-20260928-01.md)：按进程汇总（v1.17.2）。

[RR-20260927-35](RR-20260927-35.md)：kit 导出流名解析（v1.17.2）。

[RR-20260927-34](RR-20260927-34.md)：镜像自带数据，与相对 `config_data.dir` 一致（v1.17.2）。

[RR-20260927-33](RR-20260927-33.md)：单条带引号的挂载（v1.17.2）。

[RR-20260927-32](RR-20260927-32.md)：统一带 `ErrCommitRejected`，第 1 / 11 行与 RR-06 表述更正（v1.17.2）。

[RR-20260927-31](RR-20260927-31.md)：捕获失败留记号、整条回滚（v1.17.2）。

[RR-20260927-30](RR-20260927-30.md)：按值判定可比较性（v1.17.2）。

[RR-20260927-29](RR-20260927-29.md)：任何方式离开都登记离开（v1.17.2）。

[RR-20260927-28](RR-20260927-28.md)：锁内比对、按实例撤回（v1.17.2）。

[RR-20260927-27](RR-20260927-27.md)：发布交回领头方 goroutine（v1.17.2）。

[RR-20260927-26](RR-20260927-26.md)：按实例释放，新增 `ReleaseEntityInstance`（v1.17.2）。

[RR-20260927-25](RR-20260927-25.md)：`sameMutex`，框架锁按指针比较（v1.17.2）。

[RR-20260927-24](RR-20260927-24.md)：截止分支并入该哨兵（v1.17.2）。

[RR-20260927-23](RR-20260927-23.md)：接上并处理 Start / Close 顺序（v1.17.2）。

[RR-20260927-22](RR-20260927-22.md)：取锁后确认表项，注销当前登记（v1.17.2）。

[RR-20260927-21](RR-20260927-21.md)：确定失败、不重排（v1.17.2）。

[RR-20260927-20](RR-20260927-20.md)：25 个用例收尾清表（v1.17.2）。

[RR-20260927-19](RR-20260927-19.md)：`scene_session_reopen_failed_total`（v1.17.2）。

[RR-20260927-18](RR-20260927-18.md)：接 `ConfigureUnloadResync`，停机先停重载（v1.17.2）。

[RR-20260927-17](RR-20260927-17.md)：可选 `remote_entity.snapshot_l2_key_prefix`，缺省键不变（v1.17.2）。

[RR-20260927-16](RR-20260927-16.md)：Warn + 计数（v1.17.2）。

[RR-20260927-15](RR-20260927-15.md)：同 fence 更小 StateVersion 返回 `ErrRemoteVersionConflict`（v1.17.2）。

[RR-20260927-14](RR-20260927-14.md)：积压指标 + 真实上界文档（v1.17.2）。

[RR-20260927-13](RR-20260927-13.md)：新增 `nest.entity_load_timeout`、`nest.unload_resync.*`，缺省不变（v1.17.2）。

[RR-20260927-12](RR-20260927-12.md)：新增 `DestroyReasonCreateRevoked`（v1.17.2）。

[RR-20260927-11](RR-20260927-11.md)：强制整条事务失败（v1.17.2）。

[RR-20260927-10](RR-20260927-10.md)：整批校验后再写入（v1.17.2）。

[RR-20260927-09](RR-20260927-09.md)：`FinalizeLocked` 与 `ValidateEntityRegistry` fail-fast（v1.17.2）。

[RR-20260927-07](RR-20260927-07.md)：原始 mutation 按 EntityID 命中外层快照时同样拒绝（v1.17.2）。

[RR-20260927-06](RR-20260927-06.md)：提交前检查 fence，返回 `ErrNestFenced` 并回滚（v1.17.2）。

[RR-20260927-05](RR-20260927-05.md)：声明 `StopBudget = shutdown_timeout`，生成公式与 doctor 同步计入（v1.17.2）。

[RR-20260927-04](RR-20260927-04.md)：按运行时 30s 兜底判定，建议值按配置里的 dataengine 预算（v1.17.2）。

[RR-20260927-03](RR-20260927-03.md)：run.sh 传 db / password；计数器移到 key_prefix 下并兼容旧键（v1.17.2）。

[RR-20260927-02](RR-20260927-02.md)：按首次实际关闭计数（v1.17.2）。

[RR-20260927-01](RR-20260927-01.md)：写失败注入在 Windows 上改为持有文件句柄（v1.17.2）。

[RR-20260926-85](RR-20260926-85.md)：Direct 的释放通知带逻辑时钟戳，只删被释放那一次生命期内的绑定（v1.17.1）。

[RR-20260926-84](RR-20260926-84.md)：收尾阶段调用 RunIsolatedTransaction 不再认领消息：带 Remote 批次拒绝，纯本地按嵌套独立事务处理（v1.17.1）。

[RR-20260926-83](RR-20260926-83.md)：entity 包测试可重复运行——夹具按用例快照并恢复注册表，撞号改开（只改测试）（v1.17.1）。

[RR-20260926-82](RR-20260926-82.md)：dataengine/engine 包测试可重复运行——kind 撞号改开，驱逐队列断言先等 worker 取走第一项（只改测试，v1.17.1）。

[RR-20260926-81](RR-20260926-81.md)：handler 内新建撞上同 ID 撤销 / 销毁收尾中的实例，按新建锁冲突处理（v1.17.1）。

[RR-20260926-80](RR-20260926-80.md)（含复核补修：示例解析失败只指明文件与键）：doctor 逐份判定三份仓内配置、显示磁盘模板的宽限期；减少 Mod 后一次 sync 收敛（v1.17.1）。

[RR-20260926-79](RR-20260926-79.md)：会话关闭 / 业务注销时通知政策，Direct 的绑定表随之收缩（v1.17.1）。

[RR-20260926-78](RR-20260926-78.md)：重新提交交付前再次撤销，未交付的记录回到撤销表，下一次登记再交还（v1.17.1）。

[RR-20260926-77](RR-20260926-77.md)：“是否已提交 / 能否重试”判别表与停机文档如实改写（v1.17.1）。

[RR-20260926-76](RR-20260926-76.md)：嵌套独立事务提交结果未知时，返回业务之前 fence 引擎（v1.17.1）。

[RR-20260926-75](RR-20260926-75.md)：带 Remote 批次的消息里嵌套独立事务直接拒绝，不替外层 finalize、不提交外层批次（v1.17.1）。

[RR-20260926-74](RR-20260926-74.md)：嵌套独立事务不得持久写外层可回滚事务已快照的实体，在写持久记录之前拒绝并自身回滚（v1.17.1）。

[RR-20260926-73](RR-20260926-73.md)（含复核补修：摘除错误带目标 ID）：不能回滚的 handler 开始执行后不再因暂时性错误重排；Cast 目标等锁期间被摘除返回 ErrEntityNotFound（v1.17.1）。

[RR-20260926-72](RR-20260926-72.md)：卸载退役中的 RegisterAfterRetirement 返回 queued=true 并由 done 恰好报告一次（v1.17.1）。

[RR-20260926-71](RR-20260926-71.md)：冷加载与构建策略按 kind 的实际 Remote 策略判定，全仓同类点一并收口（v1.17.1）。

[RR-20260926-70](RR-20260926-70.md)：框架退回 remove 撤销的政策订阅，在实体重新登记后交还政策自动重新提交（v1.17.1）。

[RR-20260926-69](RR-20260926-69.md)：forget 在删表前、m.mu 内清除旧 subject 的通知器，且只忘掉调用方看到的那个 subject（v1.17.1）。

[RR-20260926-68](RR-20260926-68.md)：调用方截止在第一个字节写出前到期的推送按“写前拒绝”处理，不关闭连接（v1.17.1）。

[RR-20260926-67](RR-20260926-67.md)：Guard 以实例判断是否已持有，handler 内 Destroy 后重建的同 ID 实例取锁并纳入提交边界（v1.17.1）。

[RR-20260926-66](RR-20260926-66.md)：生成器按每个服务实际注册的 Mod 计算 `shutdown.total_timeout` 与部署宽限期（v1.17.1）。

[RR-20260926-65](RR-20260926-65.md)：handler 内嵌套独立事务已提交后，外层消息不再自动重排，回复带可判别错误（v1.17.1）。

[RR-20260926-64](RR-20260926-64.md)：不能回滚的 handler 内新建实体遇锁冲突时返回可判别错误，不再整条重排（v1.17.1）。

[RR-20260926-63](RR-20260926-63.md)：RR-43 回归改为事件同步；AcknowledgeRemoteCommit 幂等且并发安全写入契约，测试替身加锁；规范与 Saga 运维说明（v1.17.1）。

[RR-20260926-62](RR-20260926-62.md)：新增可重试哨兵 `entity.ErrRemoteEntityReloading`，区分“拒绝后卸载 / 重载中”与真正的 fence（v1.17.1）。

[RR-20260926-61](RR-20260926-61.md)：快池拒绝投递时不在 finalizer 线程执行提交后回调（计数告警）；延迟回调携带原请求上下文快照（v1.17.1）。

[RR-20260926-60](RR-20260926-60.md)：RR-45 装配期校验改按 kind 在注册表里的实际 Remote 策略判断（v1.17.1）。

[RR-20260926-59](RR-20260926-59.md)：仅内存卸载后仍有订阅者时，框架从权威重载并 Rebind 全量；重载不了退回 remove（v1.17.1）。

[RR-20260926-58](RR-20260926-58.md)：Remote 部分被拒绝时只丢弃 Remote 实体的 Sync 内容与事实，已提交的本地实体照常生效（v1.17.1）。

[RR-20260926-57](RR-20260926-57.md)：生成 GetOrCreate 对 ErrEntityRemoved 有界重试；删除 flush.go 被遮蔽的死分支并更正 RR-35 记录（v1.17.1）。

[RR-20260926-56](RR-20260926-56.md)：syncbus JetStream 流名缺省由 prefix 派生，默认 prefix 仍为 ROOST_SYNC，可显式配置，启动日志输出实际流名（v1.17.1）。

[RR-20260926-55](RR-20260926-55.md)：退役完成后登记的正式入口；快速重连不丢 Join，会话打开的“稍后重试”有界退避（v1.17.1）。

[RR-20260926-54](RR-20260926-54.md)：实体共享加载与调用方 ctx 解耦——每个等待方按自己的 ctx 离开，加载只受框架加载上限与 loader 注销约束（v1.17.1）。

[RR-20260926-53](RR-20260926-53.md)：纯本地事务提交后的释放 / 回调错误同样带 ErrAfterCommitFailed（v1.17.1）。

[RR-20260926-52](RR-20260926-52.md)：生成传输层关闭写失败的那一条连接，其余连接收到则推送成功（v1.17.1）。

[RR-20260926-51](RR-20260926-51.md)：停机预算先按声明值分配、未声明 Mod 固定保底不参与缩放；生成默认 `shutdown.total_timeout` 改为 60s，部署宽限期 65s（v1.17.1）。

[RR-20260926-50](RR-20260926-50.md)：被跳过的原生步骤按事务身份只登记一次驱逐；WAL terminal 唤醒全部投影等待方（v1.17.1）。

[RR-20260926-49](RR-20260926-49.md)：越过提交点的事务一律不重新准入；提交回调 panic 以 %w 保留原因链（v1.17.1）。

[RR-20260926-48](RR-20260926-48.md)：handler 内新建实体按 Cast 锁序取锁，冲突时整条回滚并重新准入；memory handler 内 Create 持锁到 handler 结束（v1.17.1）。

[RR-20260926-47](RR-20260926-47.md)：准入冷目标判定改用不加载的 IsLoaded 查询，不再在发送方调用 Getter（v1.17.1）。

[RR-20260926-46](RR-20260926-46.md)：已提交的 Remote 回复统一带 ErrAfterCommitFailed（v1.17.1）。

[RR-20260926-45](RR-20260926-45.md)：禁止 remote=managed 实体使用 dbscope=sid 的 DAO（生成期 + 装配期双重校验）（v1.17.1）。

[RR-20260926-44](RR-20260926-44.md)：未定稿的 Remote 批次 Abort 就地收尾，不再投递快池续行（v1.17.1）。

[RR-20260926-43](RR-20260926-43.md)：versionedLock 异步续期按锁代际（token）绑定（v1.17.1）。

[RR-20260926-42](RR-20260926-42.md)：Mod 可声明停机预算，App 在 `shutdown.total_timeout` 内优先分配；dataengine 声明 `shutdown_timeout`（v1.17.1）。

[RR-20260926-41](RR-20260926-41.md)：最后一段零填充尾部按四条件判据截断，其余仍 ErrCorrupt（v1.17.1）。

[RR-20260926-40](RR-20260926-40.md)：scene 的断线处理按 Join 代际复查，旧连接推送失败不再移出已重连玩家（v1.17.1）。

[RR-20260926-39](RR-20260926-39.md)：持久拒绝收尾后框架自动仅内存卸载被拒绝的实例，下一次访问从权威重载（v1.17.1）。

[RR-20260926-38](RR-20260926-38.md)：Durability 1/2 的 Remote 事务由投影器完成，finalizer 不再并发回源发布（v1.17.1）。

[RR-20260926-37](RR-20260926-37.md)：Remote 确认无结论时，Sync Confirm 与 AfterCommit 随收尾交给 finalizer，拿到持久结论后执行一次（v1.17.1）。

[RR-20260926-36](RR-20260926-36.md)：生成传输层每次 Dispatch 有截止、Session 关闭取消在途请求、EnterGame 另给登录预算（v1.17.1）。

[RR-20260926-35](RR-20260926-35.md)：事务内 CreateInScope 进入提交边界，回滚 / 拒绝时撤销发布；kit 自动接 pipelined 水位（v1.17.1）。

[RR-20260926-34](RR-20260926-34.md)：投影事务内身份裁决改为“先快照读、后插入”，伪 Mongo 忠实中止事务，驱动错误链保留（v1.17.1）。

[RR-20260926-33](RR-20260926-33.md)：fsync 失败粘滞——terminal 后 Ack / ticker / Sync 不再刷段、不推进 checkpoint 与 DurableLSN（v1.17.1）。

[RR-20260926-32](RR-20260926-32.md)：Remote 批次按显式持久提交状态决定 Commit / Abort。

[RR-20260926-31](RR-20260926-31.md)：demo 闲置交还驱逐后等待本实体投影再释放租约。

[RR-20260926-30](RR-20260926-30.md)：本地 lease fence 跳过——评估后按维护者决定修复：实体屏障 + 跳过后驱逐重载，Sync 强制全量（v1.17.1）。

[RR-20260926-29](RR-20260926-29.md)：外部夹具改用 ManualReplay 手动回放 Projector。

[RR-20260926-28](RR-20260926-28.md)：Durability 0 未知结果由 finalizer 持久拒绝收敛；拒绝后实体保持隔离直到重载。

[RR-20260926-27](RR-20260926-27.md)：快 worker 上的 Remote 删除在副作用前确定拒绝。

[RR-20260926-26](RR-20260926-26.md)：快阶段冷缺失返回 ErrColdLoadInLogic，不再 panic。

[RR-20260926-25](RR-20260926-25.md)：统一准入对冷的声明目标自动走慢阶段预加载（P1）。

复核补修（追加在原记录末尾）：RR-20260926-04、05、07、10、11、12、15、17、19、22、24。

[RR-20260926-24](RR-20260926-24.md)：补齐 room 迁移与模块符号改名。

[RR-20260926-23](RR-20260926-23.md)：去除真实时钟分辨率假设。

[RR-20260926-22](RR-20260926-22.md)：公会 ID 首次并发分配。

[RR-20260926-21](RR-20260926-21.md)：生产 Backend 转发并发投影能力。

[RR-20260926-20](RR-20260926-20.md)：内存级 Remote 未知提交交给恢复。

[RR-20260926-19](RR-20260926-19.md)：租约跳过必须有确定结论。

[RR-20260926-18](RR-20260926-18.md)：OpenRuntime 关闭自己打开的 WAL。

[RR-20260926-17](RR-20260926-17.md)：关闭排空外部在途调用。

[RR-20260926-16](RR-20260926-16.md)：checkpoint 不超过持久 WAL。

[RR-20260926-15](RR-20260926-15.md)：重连不复用正在退出的传输队列。

[RR-20260926-14](RR-20260926-14.md)：提交后 hook 失败不能 Abort。

[RR-20260926-13](RR-20260926-13.md)：释放失败仍完成已提交事务。

[RR-20260926-12](RR-20260926-12.md)：读取正式 syncbus 配置段。

[RR-20260926-11](RR-20260926-11.md)：旧 Remote 回执重放不回退活实体。

[RR-20260926-10](RR-20260926-10.md)：卸载重载等待实体投影。

[RR-20260926-09](RR-20260926-09.md)：Remote 并行投影缺少失败事务 ID。

[RR-20260926-08](RR-20260926-08.md)：续行占用时快队列容量口径分叉。

[RR-20260926-07](RR-20260926-07.md)：WAL 回放边界漏计。

[RR-20260926-06](RR-20260926-06.md)：快 worker 阻塞入口保护。

[RR-20260926-05](RR-20260926-05.md)：Sync 未尝试会话预算预扣。

[RR-20260926-04](RR-20260926-04.md)：Sync 字节预算大对象饥饿。

[RR-20260926-03](RR-20260926-03.md)：Slow 快阶段 RunLocal 自等。

[RR-20260926-02](RR-20260926-02.md)：Nest 快阶段禁止冷加载和等待在途加载，声明目标由慢阶段预加载。

[RR-20260926-01](RR-20260926-01.md)：Sync policy 失败保留 LastError，所有已执行 Flush 失败统一记账并标记阶段。

[RR-20260925-13](RR-20260925-13.md)：冷快照预算窗口不再随晚醒持续漂移，固定边界推进且不积攒空闲额度。

[RR-20260925-12](RR-20260925-12.md)：现有对象的全量视图替换不再等待冷恢复预算，修前对象/字节/会话额度三场景均可复现。

[RR-20260925-11](RR-20260925-11.md)：补齐通用故障矩阵的 Remote 集成包，按包串行使用隔离环境。

[RR-20260925-10](RR-20260925-10.md)：同 ID 重开会话不再接收旧生命周期订阅，反向索引清理与并发回归通过。

[RR-20260925-08](RR-20260925-08.md)：修复小队列错误占用 worker 执行预算，支持 1024 并发 / 16 等待位。
[RR-20260925-09](RR-20260925-09.md)：Remote 回滚钩子 panic 仍释放 Entity 本地锁，防止卡住后续快 worker。

[RR-20260925-07](RR-20260925-07.md)：Nest 收敛快慢双池，统一显式目标 ID 顺序，慢阶段本地回滚和加载初始化回到快池。

[RR-20260925-06](RR-20260925-06.md)：Remote 获取/确认/释放隔离到慢 worker，业务逻辑和 Guard 留在 cost worker；并发回归与 80 TPS × 10 分钟全量验收通过，90 TPS 长测不达标；21/21 故障矩阵通过。

[RR-20260925-05](RR-20260925-05.md)：Remote 串行投影放大确认与 Nest 排队超时；已修复，30 分钟实测 59.997 TPS、108000 笔零错误，全量一致性与故障矩阵通过。

[RR-20260925-04](RR-20260925-04.md)：Remote 发布阶段重复开启 Mongo 只读事务；按持久 digest 直接重放已提交回执。

[RR-20260925-02](RR-20260925-02.md)：统一 MongoCommitter 默认持久许可校验，删除未发布兼容路径。
[RR-20260925-03](RR-20260925-03.md)：限制全进程慢请求堆栈采样，避免 Remote 依赖阻塞时诊断放大。

[RR-20260925-01](RR-20260925-01.md)：故障测试夹具遗留 JetStream 流耗尽预留容量；补齐关闭时删除本次独有流，清理错误使测试失败。
[RR-20260924-26](RR-20260924-26.md)：Redis 未复制写丢失导致 fence 复用；已接入 Mongo 持久权限，真实故障下 Redis 1→1、权威 fence 1→2，旧写拒绝/新写成功；当前未部署，已删除未发布迁移/弱校验分支。

09-24 Remote 集群验收：[RR-24](RR-20260924-24.md) 断连后刷新拓扑且不重放不确定写入；[RR-25](RR-20260924-25.md) Kit 初始化校验锁 hash tag。真实六节点 Redis 与 race 回归通过。

09-24 Remote 第三轮：[RR-23](RR-20260924-23.md) 整批准入共享调用方与框架的较早 deadline，冷构造不执行 RPC；真实 Redis 延迟和三种持久化策略业务链路回归通过，未发布。

09-24 Remote 第二轮：[RR-19](RR-20260924-19.md) 生命周期；[RR-20](RR-20260924-20.md) 本地锁代际；[RR-21](RR-20260924-21.md) 精确计数；[RR-22](RR-20260924-22.md) Lua 失败不遗留 owner。真实 Redis、关联 race 和 Mongo/WAL 恢复通过，未发布。

09-24 Remote：[RR-16](RR-20260924-16.md) 批量等待保留 tracker；[RR-17](RR-20260924-17.md) 校验成功后才准入；[RR-18](RR-20260924-18.md) Redis 校验和转换为精确十进制参数。真实 Mongo/Redis/WAL 恢复及关联包 race 通过，未发布。

`docs/bug/` 保存问题证据（RR / 历史 W 编号）；本目录记录这些发现是怎样被修掉的：
改了哪几行、为什么这样改、用什么测试证明修前红修后绿、留下了什么没做。
每条一个文件，编号沿用 RR，账本单元编号（U-）指向 [history/ledger.md](../history/ledger.md)。

| 编号 | 仓库 | 问题 | 修复单元 | 记录 |
| --- | --- | --- | --- | --- |
| RR-20260924-14 | core/dataengine | 事务内 Put 重复键直接判定冲突，不查询已中止 session | — | [RR-20260924-14.md](RR-20260924-14.md) |
| RR-20260924-15 | core/dataengine | 批量构建 digest 时拒绝同 ID 不同内容 | — | [RR-20260924-15.md](RR-20260924-15.md) |
| RR-20260924-13 | core/dataengine | Outbox 从发布失败返回时计算退避，避免慢 Broker 下立即重试 | — | [RR-20260924-13.md](RR-20260924-13.md) |
| RR-20260924-11 | core/dataengine | 统一 Assembly/Runtime 启停与失败清理所有权 | — | [RR-20260924-11.md](RR-20260924-11.md) |
| RR-20260924-12 | core/remoteentity/dataengine | Remote 确定版本冲突触发熔断，拒绝继续积压新事务 | — | [RR-20260924-12.md](RR-20260924-12.md) |
| W-2026-09-22-02 | core/codegen/remoteentity | 持久化公会改用 Mongo 发号，避免同 SID 重启复用临时 ID | — | [证据与修复](W-2026-09-22-02.md) |
| RR-20260924-10 | core/dataengine | Outbox 统一启停状态，阻止关闭后再启动和重复关闭通道 | — | [RR-20260924-10.md](RR-20260924-10.md) |
| RR-20260924-09 | core/dataengine | Projector 与 Runtime 所有权等待响应 context，保留未投影 WAL 和停机重试责任 | — | [RR-20260924-09.md](RR-20260924-09.md) |
| RR-20260924-01 | core/nest | pipelined 准入回调移回解锁前，保持最终完成与错误报告 | — | [RR-20260924-01.md](RR-20260924-01.md) |
| RR-20260924-02 | core/nest | release hook panic 时清理组锁与 scope，覆盖部分加锁失败 | — | [RR-20260924-02.md](RR-20260924-02.md) |
| RR-20260924-03 | core/nest | 动态 Cast 的回滚、解锁与 pipelined 水位不再依赖 Sync 接线 | — | [RR-20260924-03.md](RR-20260924-03.md) |
| RR-20260924-04 | core/nest | 广播释放异常时仍配对 UnTouch，并继续后续实体 | — | [RR-20260924-04.md](RR-20260924-04.md) |
| RR-20260924-05 | core/nest | 异步完成等待 Entity 解锁屏障，保留完成排序与回复所有权 | — | [RR-20260924-05.md](RR-20260924-05.md) |
| RR-20260924-06 | core/nest | Ticker Start/Stop 在同一临界区转换状态，消除停止后启动竞争 | — | [RR-20260924-06.md](RR-20260924-06.md) |
| RR-20260924-07 | core/nest | 准入后的释放异常仍等待 WAL、完成回调并回复，保留错误链 | — | [RR-20260924-07.md](RR-20260924-07.md) |
| RR-20260924-08 | core/nest | 按实际解锁状态执行 pipelined 回调，统一内联/降级的错误报告 | — | [RR-20260924-08.md](RR-20260924-08.md) |
| M-19 | core/sync | 多来源订阅与政策所有权，保留默认 API | M-19 | [实施与验收](../feature/SYNC-COMPLETION-2026-09-23.md) |
| M-20 | core/sync | tick 级共享组件编码与性能基准 | M-20 | [基准与实测](../feature/SYNC-BENCHMARKS.md) |
| RR-20260923-04 | core/sync | 在途交付覆盖新的会话或订阅意图 | — | [RR-20260923-04.md](RR-20260923-04.md) |
| RR-20260923-05 | core/sync | Stop/Close 等待 Flush 忽略 deadline，并允许旧循环未退出就重启 | — | [RR-20260923-05.md](RR-20260923-05.md) |
| RR-20260923-06 | core/sync | Group.AddSubject 部分失败后残留成员状态且无法重试 | — | [RR-20260923-06.md](RR-20260923-06.md) |
| RR-20260923-07 | core/sync | 不同政策相互撤销订阅，Group 关闭误退役共享实体 | — | [RR-20260923-07.md](RR-20260923-07.md) |
| RR-20260923-01 | core/sync | 等待新快照的旧对象退订 / 退役漏 remove，改按已交付引用表判定 | — | [RR-20260923-01.md](RR-20260923-01.md) |
| RR-20260923-02 | core/sync | 部分准入后 RetryLater 导致内容基线 / 帧时钟分叉，逐帧采纳并全量恢复 | — | [RR-20260923-02.md](RR-20260923-02.md) |
| RR-20260923-03 | core/sync | 满容量对象替换因 ID 排序误拒，编码先释放后分配 | — | [RR-20260923-03.md](RR-20260923-03.md) |
| RR-20260908-01 | kit | Session `Enter` 丢掉幂等账本 Create 的落败结果，两个 owner 都成功 | U-0154（方案 B） | [RR-20260908-01.md](RR-20260908-01.md) |
| RR-20260908-02 | core | ReadThrough 跟随者取消后不归还等待名额 | U-0155 | [RR-20260908-02.md](RR-20260908-02.md) |
| RR-20260908-03 | codegen | `--consolidate` 对单行 import 的混合分流产出非法 Go | U-0156 | [RR-20260908-03.md](RR-20260908-03.md) |
| RR-20260909-01 | core/docs | Quickstart 固定过时 codegen 版本且缺 `-module` | U-0157 | [RR-20260909-01.md](RR-20260909-01.md) |
| RR-20260909-02 | kit | Session 撞 RequestID 的撤回误删同 owner 重新取得的 claim（ABA） | U-0158 | [RR-20260909-02.md](RR-20260909-02.md) |
| RR-20260909-03 | core | Assembly 停机未完成就忘掉 Runtime，重试虚报成功 | U-0159 | [RR-20260909-03.md](RR-20260909-03.md) |
| RR-20260909-04 | codegen | 同包多个 Entity 生成重名注册符号 | U-0160 | [RR-20260909-04.md](RR-20260909-04.md) |
| RR-20260909-05 | kit | Enqueue 幂等重放返回其他 Subject 的 Ticket | U-0164 | [RR-20260909-05.md](RR-20260909-05.md) |
| RR-20260909-06 | codegen | 多 Entity 配合显式 `-output` 静默覆盖生成结果 | U-0162 | [RR-20260909-06.md](RR-20260909-06.md) |
| RR-20260910-01 | core | 极短 IdleTTL 推导出零扫描周期 | U-0163 | [RR-20260910-01.md](RR-20260910-01.md) |
| RR-20260910-02 | kit | 已领取邮件淘汰后重投生成新发奖 token | U-0165 | [RR-20260910-02.md](RR-20260910-02.md) |
| RR-20260910-03 | core | 完成事务的等待者因缓存淘汰读到已删记录而 panic | U-0168 | [RR-20260910-03.md](RR-20260910-03.md) |
| RR-20260910-04 | core | Checkpoint 恢复按 ID 重排完成历史,淘汰顺序分叉 | U-0169 | [RR-20260910-04.md](RR-20260910-04.md) |
| RR-20260910-05 | codegen | category 指向业务包常量时生成物遗漏 import(M-05 引入) | U-0166 | [RR-20260910-05.md](RR-20260910-05.md) |
| RR-20260910-06 | codegen | 聚合注册的固定 entity 导入与业务包别名冲突(M-05 引入) | U-0167 | [RR-20260910-06.md](RR-20260910-06.md) |
| RR-20260911-01 | kit | 墓碑按计数淘汰早于信封过期,再铸领取 token(U-0165 残余) | U-0171 | [RR-20260911-01.md](RR-20260911-01.md) |
| RR-20260911-02 | kit | `Mailbox.clone` 未复制 SettledClaims,快照与存储共享(U-0165 引入) | U-0170 | [RR-20260911-02.md](RR-20260911-02.md) |
| RR-20260911-03 | core | finalizer 停止后晚到的 Close 仍被接受,遗留 entries 与 slot | U-0173 | [RR-20260911-03.md](RR-20260911-03.md) |
| RR-20260911-04 | core | Nest 停机回收延迟消息而不回复等待者 | U-0172 | [RR-20260911-04.md](RR-20260911-04.md) |
| RR-20260913-03 | core | Remote payload 身份未与信封绑定,可写到别的 scope | U-0179 | [RR-20260913-03.md](RR-20260913-03.md) |
| RR-20260913-04 | core | 快照合并加载的等待名额在取消后泄漏 | U-0174 | [RR-20260913-04.md](RR-20260913-04.md) |
| RR-20260913-07 | core | L2 同版本比较遗漏 schema / codec | U-0176 | [RR-20260913-07.md](RR-20260913-07.md) |
| RR-20260913-08 | core | 快照的绝对过期时间未参与读取准入 | U-0175 | [RR-20260913-08.md](RR-20260913-08.md) |
| RR-20260913-10 | core | 快照 Lua 把 uint64 版本转浮点,排序失真 | U-0177 | [RR-20260913-10.md](RR-20260913-10.md) |
| RR-20260913-11 | core | 大 epoch 拼成科学计数法并持久化不可读 marker | U-0178 | [RR-20260913-11.md](RR-20260913-11.md) |
| RR-20260913-05 | core | L2 版本冲突被 IgnoreRemoteError 吞掉,L1/L2 同版本分叉 | U-0180 | [RR-20260913-05.md](RR-20260913-05.md) |
| RR-20260913-06 | core | L2 回填绕过同版本内容检查,覆盖已发布值 | U-0181 | [RR-20260913-06.md](RR-20260913-06.md) |
| RR-20260913-02 | core | 迟到的旧 release 取消更新的 renewal | U-0184 | [RR-20260913-02.md](RR-20260913-02.md) |
| RR-20260911-05 | kit | 被拒绝的投递仍修改 MemoryStore 邮箱(回调未 clone) | U-0182 | [RR-20260911-05.md](RR-20260911-05.md) |
| RR-20260911-06 | core | AfterCommit panic 遗漏回复与释放,饱和回退可崩溃 | U-0183 | [RR-20260911-06.md](RR-20260911-06.md) |
| RR-20260912-02 | core | WAL.Sync 只 fsync 文件,不等队列里已准入的记录 | U-0185 | [RR-20260912-02.md](RR-20260912-02.md) |
| RR-20260912-01 | core | Committer Flush / Shutdown 等 replay 所有权时不可取消 | U-0186 | [RR-20260912-01.md](RR-20260912-01.md) |
| RR-20260913-01 | core | Remote 快照删除无版本屏障,迟到删除清新值 / 旧值复活 | U-0187 | [RR-20260913-01.md](RR-20260913-01.md) |
| RR-20260913-09 | core | Transfer 回复丢失被当成没执行,旧 owner 恢复可写 | U-0188 | [RR-20260913-09.md](RR-20260913-09.md) |
| RR-20260913-12 | core | EnterShared / LeaveShared 回复丢失被当成没执行,恢复旧模式放行独占写 | U-0189 | [RR-20260913-12.md](RR-20260913-12.md) |
| RR-20260913-13 | core | LeaveShared 回复丢失后普通写重试卡在 `shared -> local_owned` 非法迁移 | U-0189(同一修复覆盖) | [RR-20260913-12.md](RR-20260913-12.md#2026-09-13-第十轮rr-20260913-13-由同一修复覆盖仍归-u-0189) |
| RR-20260914-01 | core | Sync 把 Close 发起当成排空完成,关闭进行中提前报告成功(U-0185 引入) | U-0190 | [RR-20260914-01.md](RR-20260914-01.md) |
| RR-20260914-02 | kit | OpenActivity 与 sweep 交错丢失窗口索引(条目无 opening / 确认生命周期) | U-0191 | [RR-20260914-02.md](RR-20260914-02.md) |
| RR-20260914-03 | kit | 派发循环只看本轮新完成,退避重试 / notify 完成 / heal 都失去入口 | U-0192 | [RR-20260914-03.md](RR-20260914-03.md) |
| RR-20260914-04 | core | lockstep 已入帧输入的迟到重传再次入帧(身份只活到目标帧被切) | U-0193 | [RR-20260914-04.md](RR-20260914-04.md) |
| RR-20260914-05 | core | lockstep CatchupBatchFrames=1 净补帧速度为 0,永不切回 live | U-0194 | [RR-20260914-05.md](RR-20260914-05.md) |
| RR-20260914-06 | core | lockstep 座位 -1 与旁观者哨兵碰撞 | U-0195 | [RR-20260914-06.md](RR-20260914-06.md) |
| RR-20260914-07 | core | lockstep 座位数未对齐 wire 的 MaxFrameInputs | U-0196 | [RR-20260914-07.md](RR-20260914-07.md) |
| RR-20260914-08 | core | lockstep 去重身份表准入无上限,只靠 Advance 回收(U-0193 引入) | U-0197 | [RR-20260914-08.md](RR-20260914-08.md) |
| RR-20260914-09 | core | LockstepBot 把接收游标当应用游标,回调失败后丢失批次尾帧 | U-0198 | [RR-20260914-09.md](RR-20260914-09.md) |
| RR-20260914-10 | core | statesync 同 tick 重投影覆盖 sent[tick],迟到 ACK 绑错基线 | U-0200 | [RR-20260914-10.md](RR-20260914-10.md) |
| RR-20260914-11 | core | statesync ACK 路径清 forceFull,旧发送的 ACK 取消恢复意图 | U-0201 | [RR-20260914-11.md](RR-20260914-11.md) |
| RR-20260914-12 | core | statesync 单片重组绕过 MaxFrameBytes | U-0202 | [RR-20260914-12.md](RR-20260914-12.md) |
| RR-20260914-13 | core | statesync LOD 按绝对 tick 采样,错相发送的组件永远保留旧值 | U-0203 | [RR-20260914-13.md](RR-20260914-13.md) |
| RR-20260915-01 | core | statesync ApplyDelta 每步用最终存量上限检查暂存 map,满容量替换看 ID 排序决定成败 | U-0204 | [RR-20260915-01.md](RR-20260915-01.md) |
| RR-20260915-02 | core | statesync 重叠准备的不同视图已交付后 Commit 被拒,基线静默分叉(U-0200 残余窗口) | U-0205 | [RR-20260915-02.md](RR-20260915-02.md) |
| RR-20260915-03 | core | entitysync 持久化水位门槛只在 FlushSubject,订阅快照 / profile 切换 / 直接分发绕过 | U-0206 | [RR-20260915-03.md](RR-20260915-03.md) |
| RR-20260915-04 | core | room 慢连接剔除通知绑在批次结果上,剩余批次失败即丢失,room 保留失效订阅 | U-0207 | [RR-20260915-04.md](RR-20260915-04.md) |
| RR-20260915-05 | core | room SetDownstream 只换指针不迁移慢消费者回调,替换后剔除不清理订阅 | U-0208 | [RR-20260915-05.md](RR-20260915-05.md) |
| RR-20260915-06 | core | syncstream 清理后重建的流从序号 1 重来,旧 ACK / Resync 吞掉新内容 | U-0215 | [RR-20260915-06.md](RR-20260915-06.md) |
| RR-20260915-07 | core | syncstream 恢复时忽略的半条 WAL 尾部未截断,续写后下次启动不可读 | U-0211 | [RR-20260915-07.md](RR-20260915-07.md) |
| RR-20260915-08 | core | syncstream 绑定 journal 的 Import / Restore 只换内存不发布 | U-0213 | [RR-20260915-08.md](RR-20260915-08.md) |
| RR-20260915-09 | core | syncstream Recover 把过期捕获提交为更新的 Full | U-0214 | [RR-20260915-09.md](RR-20260915-09.md) |
| RR-20260916-01 | core | syncstream journal 写入 / 发布结果不确定后继续写,重复序号或丢追加 | U-0212 | [RR-20260916-01.md](RR-20260916-01.md) |
| RR-20260916-02 | core | room JetStream 持久消费者身份不含 Prefix,共用 Stream 时撞名 | U-0210 | [RR-20260916-02.md](RR-20260916-02.md) |
| RR-20260916-03 | core | room JetStream 同 topic 多个本地订阅竞争同一消费者,广播变分摊 | U-0209 | [RR-20260916-03.md](RR-20260916-03.md) |
| RR-20260916-04 | core | syncstream Recover 的位置核对识别不出删除 ABA / 同位置 Import,旧捕获仍被提交 | U-0216 | [RR-20260916-04.md](RR-20260916-04.md) |
| RR-20260916-05 | kit + codegen | match 的 Grouping 注入参数无执行者;**破坏性**:`NewMod(reporter)`,codegen 不再生成 `Grouping()` | U-0217 | [RR-20260916-05.md](RR-20260916-05.md) |
| RR-20260916-06 | core | manager：最后一个管理器启动期间 Stop，成功启动者漏清理（交接不在同一把锁下） | U-0219 | [RR-20260916-06.md](RR-20260916-06.md) |
| RR-20260916-07 | core | manager：首个管理器启动期间 Register 被接纳，新对象永不启动也不停止 | U-0220 | [RR-20260916-07.md](RR-20260916-07.md) |
| RR-20260917-01 | core | match：合法 Queue 键碰撞（Mode / Partition 含冒号），跨队列读票与成组 | U-0221 | [RR-20260917-01.md](RR-20260917-01.md) |
| RR-20260917-02 | core | match：ScoreWindow 距离 / 窗口 int64 溢出，远端成组、cap 失效 | U-0222 | [RR-20260917-02.md](RR-20260917-02.md) |
| RR-20260917-03 | core | match：内存 Store 输入 / 返回切片与存储共享 | U-0223 | [RR-20260917-03.md](RR-20260917-03.md) |

| RR-20260919-01 | codegen | 顶层 nested 指针字段替换 / 回滚后没有解绑离开的对象，游离对象仍能提交该字段 | U-0249 | [RR-20260919-01.md](RR-20260919-01.md) |
| RR-20260919-07 | core+kit | 没有订单的索引条目永远排在页首、占住每一页的槽位 | U-0256 | [RR-20260919-07.md](RR-20260919-07.md) |
| RR-20260919-10 | core+kit+codegen | activity 的 dispatch 没有交付者：sweep 空耗尝试次数、游戏端只能猜两个窗口 | U-0257 | [RR-20260919-10.md](RR-20260919-10.md) |
| RR-20260920-03 | core+codegen | 玩家租约的 `GET` 之后再 `EXPIRE`/`DEL`：旧 owner 会给新 owner 续期或删掉它 | U-0258 | [RR-20260920-03.md](RR-20260920-03.md) |
| RR-20260920-04 | codegen | 租约丢了不阻断本地写入：续租结果被丢弃，实体既不卸载也不停服务 | U-0259 | [RR-20260920-04.md](RR-20260920-04.md) |
| RR-20260920-02 | core | 普通 room delta 走 latest-only 通道，待发帧被下一帧替掉，独有字段永久丢失 | U-0260 | [RR-20260920-02.md](RR-20260920-02.md) |
| RR-20260920-01 | core | snapshot 的 checksum 是完整 64 位散列，BSON 装不下高位为 1 的那一半；失败记录还会成为启动毒丸 | U-0261 | [RR-20260920-01.md](RR-20260920-01.md) |
| RR-20260920-07 | core | `SmallSafeMap` 的 BSON 方法签名不符合驱动接口，从未生效，该类型被写成空文档 | U-0265 | [RR-20260920-07.md](RR-20260920-07.md) |
| RR-20260920-08 | core | `OpTimeout` 在拿到写闸之后才生效，排队没有上界（调用方无 deadline 时无限等） | U-0266 | [RR-20260920-08.md](RR-20260920-08.md) |
| RR-20260920-06 | codegen | 被房间拒绝的 subscribe 只记一条日志就丢掉，那个观察者永久收不到那个 subject | U-0267 | [RR-20260920-06.md](RR-20260920-06.md) |
| RR-20260920-09 | codegen | 租约失而复得后仍用失效期间没重新加载过的常驻 Player 实体；有间断就扔副本 | U-0268 | [RR-20260920-09.md](RR-20260920-09.md) |
| RR-20260920-10 | codegen | 后台为离线玩家取得的租约永不归还，玩家被钉在一个进程上；空闲即归还（先扔副本再还租约） | U-0269 | [RR-20260920-10.md](RR-20260920-10.md) |
| RR-20260920-11 | codegen | 为新工作重新取得的租约带着旧时间戳被当成空闲还掉；拆开续租与认领两种事件，并在归还前先停准入 | U-0271 | [RR-20260920-11.md](RR-20260920-11.md) |
| RR-20260920-12 | codegen | 撤离的 ctx 预算不覆盖它要等的实体锁，一个忙实体钉住整轮刷新；撤离改成跑到底，预算只限制调用方等多久 | U-0272 | [RR-20260920-12.md](RR-20260920-12.md) |
| RR-20260921-01 | core `demo` | 贡献落进哪个窗口没有回传通路，机器人只能对读时的窗口下断言；加时钟缝 + 回传窗口 id | U-0273 | [RR-20260921-01.md](RR-20260921-01.md) |
| RR-20260921-02 | core `.github` | demo-publish 从未发布过：发布的树自带 workflow 文件，token 推不上去；改名 generated-github/ | U-0274 | [RR-20260921-02.md](RR-20260921-02.md) |
| RR-20260922-01 | core `entitysync` + `room` | 撤订阅以"Leave 投递成功"为前提，断线观察者永远撤不掉，room 每次 flush 都给死会话生成帧、整批被拒，其后所有观察者停摆；改为撤订阅无条件完成、Leave 尽力投递并用 `ErrLeaveNotDelivered` 上报 | U-0277 | [RR-20260922-01.md](RR-20260922-01.md) |
| RR-20260922-03 | core `.github` | `service/mail` 五个 Redis 集成用例在 CI 的 glob 之外，守卫看不见从没跑过的包；两条命令加 `./service/...`，promise test 把列表钉到文件系统 | U-0275 | [RR-20260922-03.md](RR-20260922-03.md) |
| RR-20260922-02 | core `kit/scripts` + `.github` | 故障矩阵脚本两格无测试文件、core 侧四套件因 `../roost-core` 不存在被跳过且退出 0、无 workflow 调用；改从模块根跑真实列表，nightly 加回 `fault-matrix` job | U-0276 | [RR-20260922-02.md](RR-20260922-02.md) |
| RR-20260920-05 | core+kit | 读不回来的订单永久占住重试页，健康订单永远进不了 AttemptDelivery | U-0262 | [RR-20260920-05.md](RR-20260920-05.md) |
| RR-20260919-08 | core | 两层共享加载没有 defer 收尾，一次 loader panic 让该实体永久加载不了 | U-0254 | [RR-20260919-08.md](RR-20260919-08.md) |
| RR-20260919-09 | core | 缺失实体 `(nil,nil)` 在 single / broadcast 处被解引用；broadcast 还会中止后续实体 | U-0255 | [RR-20260919-09.md](RR-20260919-09.md) |
| RR-20260919-02 | codegen | 同一个 nested 指针占两个字段 / key 时通知是单槽的，只有最后一个会落库 | U-0252 | [RR-20260919-02.md](RR-20260919-02.md) |
| RR-20260919-03 | codegen | 会话关闭的订阅者 panic 逃出派发 goroutine，整个 game 进程崩溃 | U-0247 | [RR-20260919-03.md](RR-20260919-03.md) |
| RR-20260919-04 | core+kit+codegen | 订单先持久化、待办索引后写；中间崩一次就留下后台永远枚举不到的已付款订单 | U-0253 | [RR-20260919-04.md](RR-20260919-04.md) |
| RR-20260919-05 | codegen | 待发货索引里一条读不出来的订单让整页失败，后面的健康订单永远拿不到重试 | U-0248 | [RR-20260919-05.md](RR-20260919-05.md) |
| RR-20260919-06 | codegen | 未领取的付费 grant 固定 30 天后被拒绝并删除，而订单早已 delivered，形成永久少发货 | U-0251 | [RR-20260919-06.md](RR-20260919-06.md) |

用户复审 / 自查直接发现、没有 RR 编号的修复另记,编号沿用账本单元:

| 编号 | 仓库 | 问题 | 记录 |
| --- | --- | --- | --- |
| U-0270 | codegen | 发布清单里的 codegen 版本停在 v1.15.19，受保护的 framework-release 闸自 v1.15.22 起连红十次，`framework-lock.json` 十个版本没有产出（进度盘点对照 CI 发现） | [U-0270-framework-release-version-drift.md](U-0270-framework-release-version-drift.md) |
| U-0263 | kit | U-0257 加的 owed 索引是一个新键空间却没登记，`TestPerPackageKeyNamespacesDoNotCollide` 红（CI 发现） | [U-0263-activity-owed-namespace.md](U-0263-activity-owed-namespace.md) |
| U-0264 | codegen | 声明的框架版本下限是假的：生成物在 core v1.14.0 / kit v1.13.0 上编译不过（CI 发现） | [U-0264-generator-version-floor.md](U-0264-generator-version-floor.md) |
| U-0199 | core | lockstep `SubmitInput` 先按客户端帧号索引身份环再校验(32 位平台越界 panic + 垃圾帧号分配环) | [U-0199-submit-input-validation-order.md](U-0199-submit-input-validation-order.md) |
| U-0218 | codegen | 托管服务 collaborators 无条件 import 服务包,U-0217 后 match 工程 "imported and not used"(发版验证发现,v1.15.6 补丁) | [U-0218-collaborators-unused-import.md](U-0218-collaborators-unused-import.md) |
| U-0224 | codegen | dao 生成的嵌套 struct 无 BSON 表示，落库 / 回滚快照 / 同步只剩 `{"dirtyhook": {}}`；加 `bson:"-" json:"-"` 并生成 MarshalBSON / UnmarshalBSON（用户复审提出） | [U-0224-dao-nested-bson.md](U-0224-dao-nested-bson.md) |
| U-0278 | core `demo` 模板 + `codegen/internal/roost` | 场景 lane 逐个推、遇错整批放弃：一个推不到的会话让同批其后的观察者少收帧，接入层不可用时还把玩家踢出场景；按失败种类分流，`pushPlayer` 无会话计 `player_tcp_push_no_session_total`（W-2026-09-22-03，维护者拍板） | [U-0278-scene-lane-per-session-push.md](U-0278-scene-lane-per-session-push.md) |
| U-0250 | codegen | handler 参数名写成 `_` 时生成的 sender 声明并传递空白名，工程编译不过（修 RR-20260919-06 时撞上） | [U-0250-nest-blank-parameter-name.md](U-0250-nest-blank-parameter-name.md) |
| U-0246 | codegen | DAO 字段名小写之后是 Go 关键字（`Type` → `type`），生成物编译不过，错误指向临时文件（加 demo 计时器节点时自查） | [U-0246-dao-keyword-field-names.md](U-0246-dao-keyword-field-names.md) |
| U-0245 | codegen | 新建的 DAO 不接嵌套回调，第一次存盘前的嵌套写入悄悄丢掉（加 demo 嵌套字段时自查） | [U-0245-fresh-dao-nested-wiring.md](U-0245-fresh-dao-nested-wiring.md) |
| U-0244 | codegen | spawner 从 `fctx.RuntimeConfig()` 读 sid，configdata 覆盖该槽位后 sid 为 0，工程起不来（自查，已随 v1.15.11 发出） | [U-0244-spawner-sid-source.md](U-0244-spawner-sid-source.md) |
| U-0243 | codegen | 生成的接入层不通知会话关闭，空闲世界里断线成员永远留着（RR-20260918-06） | [RR-20260918-06.md](RR-20260918-06.md) |
| U-0242 | codegen | 运行期实体 id 由进程本地计数器发号，多实例碰撞（RR-20260918-09） | [RR-20260918-09.md](RR-20260918-09.md) |
| U-0241 | core+codegen | 邮件账本按固定 31 天清理，而 send_ttl 只要求为正数；账本先忘、信封还可领（RR-20260918-05） | [RR-20260918-05.md](RR-20260918-05.md) |
| U-0240 | core | `spatial.InterestConfig` 对单个观察者订阅的格数没有上界，合法配置可登记 40,401 格（RR-20260918-08） | [RR-20260918-08.md](RR-20260918-08.md) |
| U-0239 | codegen | `syncTopic` 的裸标识符被静默当成字面量，实体订阅到常量的名字（RR-20260918-07） | [RR-20260918-07.md](RR-20260918-07.md) |
| U-0238 | codegen | 顶层 DAO 容器换掉成员后不解绑旧值，游离对象能以原 key 写回持久化补丁（RR-20260918-10） | [RR-20260918-10.md](RR-20260918-10.md) |
| U-0237 | codegen | 清关奖励账本按"别的服务会忘掉 run"收界，而 session 的 run 没有存储 TTL；清理后重放旧 run 再发一次（RR-20260918-04） | [RR-20260918-04.md](RR-20260918-04.md) |
| U-0236 | codegen | 嵌套 child 的通知归属散写在各 setter 分支，undo 不搬 callback、`*Child` 不解绑旧值；回滚后恢复的 child 漏出持久化链（RR-20260918-03） | [RR-20260918-03.md](RR-20260918-03.md) |
| U-0235 | codegen | attribute 的 runtime.go 文件头自造标记，doctor 判成应用自有文件、project-templates 整项失败（自查发现，已随 v1.15.10 发出） | [U-0235-attribute-runtime-header.md](U-0235-attribute-runtime-header.md) |
| U-0234 | kit | platform 后台重试的候选来源固定为空，可恢复的发货失败长期挂起（RR-20260917-04） | [RR-20260917-04.md](RR-20260917-04.md) |
| U-0233 | core | RoomBroadcaster 私有持有 coordinator，房间层没有接入持久化水位的入口（RR-20260918-02） | [RR-20260918-02.md](RR-20260918-02.md) |
| U-0232 | codegen | 嵌套 DAO 的第二层 child 变更不通知父级，改动进不了 patch（RR-20260917-05，Wanted-02 转入） | [RR-20260917-05.md](RR-20260917-05.md) |
| U-0231 | core | 默认 Saga Assembly 不订阅原生 Nest 完成效果，saga 永远 waiting（RR-20260917-07，Wanted-04 转入） | [RR-20260917-07.md](RR-20260917-07.md) |
| U-0230 | core+codegen | attribute feature 只有生成器、没有运行时契约，生成物引用七个无人提供的类型（RR-20260917-06，Wanted-03 转入） | [RR-20260917-06.md](RR-20260917-06.md) |
| U-0229 | codegen | sync=true 实体生成物写了 Core 没有的 FlushPolicy / SubjectPackerFactory，整条 feature 编译不过（RR-20260918-01） | [RR-20260918-01.md](RR-20260918-01.md) |
| U-0228 | codegen | battle demo 的启动宽限期没有事件源，无人输入的房间一帧不切（RR-20260917-09） | [RR-20260917-09.md](RR-20260917-09.md) |
| U-0227 | codegen | handler 半的生成文件 import 了只被返回类型用到的包，`imported and not used`（实施 RR-20260917-08 时发现） | [U-0227-nest-return-type-imports.md](U-0227-nest-return-type-imports.md) |
| U-0225 | core | saga 步骤拒绝 / 重试用尽后进补偿的记录版本被加了两次，MongoStore.Apply 只收 expected+1，saga 永远卡在 waiting（game-demo 实跑发现） | [U-0225-saga-compensation-version.md](U-0225-saga-compensation-version.md) |

重构类改动(不对应任何 RR,不关闭任何 RR)另记,编号 M-:

| 编号 | 仓库 | 改了什么 | 记录 |
| --- | --- | --- | --- |
| M-01 | core | entity kind 注册表改为按 kind 的无锁定长表;含 category 重设计四步方案,仅第一步已实施 | [M-01-entity-kind-registry.md](M-01-entity-kind-registry.md) |
| M-02 | core | category 离开 EntityID,注册表成为唯一权威;ID 那两位降为历史填充,零数据迁移 | [M-02-category-leaves-the-id.md](M-02-category-leaves-the-id.md) |
| M-03 | core | 声明 category 后值即锁序,remote 档强制最先,锁序注册期派生;新增 `ValidateEntityRegistry` | [M-03-category-lock-order.md](M-03-category-lock-order.md) |
| M-04 | core + codegen | **破坏性**:删除 `RemotePolicyCapable`、`GetEntityGroupFunc`、`EntityGroup*`;推荐分类常量;codegen 拒绝 `remote=capable`、脚手架默认 Other | [M-04-drop-capable-and-the-group-hook.md](M-04-drop-capable-and-the-group-hook.md) |
| M-06 | core + kit（core v1.15.3 / kit v1.14.4 已发） | match 领域实现（类型、errcode 段、Store 状态机、Redis store、Grouping）与 `servicemetrics` 契约下沉 core：`roost-core/service/match`、`roost-core/servicemetrics`；kit 侧改别名包的步骤写在记录里 | [M-06-match-domain-into-core.md](M-06-match-domain-into-core.md) |
| M-07 | core + kit（core v1.15.4 / kit v1.14.5 已发） | mail 领域实现（信封 / 邮箱状态机 / 三段式领取 / Redis stores / errcode 段）下沉 `roost-core/service/mail`；`Mail` RPC 接口、生成传输与 Mod 留 kit | [M-07-mail-domain-into-core.md](M-07-mail-domain-into-core.md) |
| M-08 | core + kit（core v1.15.4 / kit v1.14.5 已发） | session 领域实现（幂等 Enter / 归属 / run 状态机 / Admin 操作面 / Redis stores）下沉 `roost-core/service/session`；`Session` RPC 接口、生成传输与 Mod 留 kit | [M-08-session-domain-into-core.md](M-08-session-domain-into-core.md) |
| M-09 | core + kit（core v1.15.4 / kit v1.14.5 已发） | manager 生命周期引擎（稳定拓扑序 `Order`、`Engine`：只回滚已成功者 / 关停中止启动 / Stop 幂等逐个报错）下沉 `roost-core/manager`；`ManagerMod`（Mod 名、capability 登记）留 kit 改为包装 | [M-09-manager-engine-into-core.md](M-09-manager-engine-into-core.md) |
| M-10 | codegen + kit（codegen v1.15.7 / kit v1.14.5 已发） | `servicerpc` 生成传输拆成 `<iface>_rpc_gen.go`（只依赖 core）与 `<iface>_rpc_assembly_gen.go`（Server / OwnerCapabilities / ClientMod，依赖 kit mods）；kit 全部 RPC 接口已重生成 | [M-10-servicerpc-split.md](M-10-servicerpc-split.md) |
| M-11 | codegen + core + kit（core v1.15.5 / kit v1.14.6 / codegen v1.15.8 已发） | `Mail` / `Session` / `Matchmaker` RPC 接口连同传输半（`*_rpc_gen.go`）进 `roost-core/service/*`；codegen `servicerpc` 加 `-emit` / `-out`，kit 从 core 的接口生成装配半 | [M-11-rpc-interfaces-into-core.md](M-11-rpc-interfaces-into-core.md) |
| M-05 | codegen | `category=` 进实体标记并直接进生成物,拆掉运行期查表的隐式前置;生成的聚合注册末尾调 `ValidateEntityRegistry` | [M-05-marker-owns-the-category.md](M-05-marker-owns-the-category.md) |
| M-12 | core+codegen | 生成的同步字段词汇表（ARCH-06，承接 W-2026-09-18-02） | [ARCH-06-sync-field-vocabulary.md](ARCH-06-sync-field-vocabulary.md) |
| M-13 | core + codegen 模板 | 实体同步统一为 `entitysync.Manager`（subject 私有订阅者表、每会话一帧、`ErrRetryLater`/关会话两种失败），删除 coordinator 与 room 广播器 / envelope sink / transport sink / RoomManager，线格式 v2；demo scene 直接持 Manager（ARCH-10，破坏性） | [M-13-entitysync-manager.md](M-13-entitysync-manager.md) |
| M-14 | core + codegen + demo 模板 | ARCH-10 收尾：held/ready 会话（demo `scene_ready`），`syncTopic` → `syncNamespace`，新包 `entitysync/policy`（Interest / Room / Direct）——demo 的兴趣聚合搬进 core；分层收成内容 / 机制 / 组织 / 应用四层 | [M-14-sync-policy-and-ready.md](M-14-sync-policy-and-ready.md) |
| M-15 | core | ARCH-12 S1：删掉 `statesync` 里 ARCH-10 后零引用的老 Replicator 一族（Replicator / SessionState / LOD / delta / ShadowStore / SnapshotRing / Schema / control / Reassembler）与 `nettransport.ControlPlane`，约 −3.9k 行；修 "replication" 过时注释 | [M-15-statesync-dead-code.md](M-15-statesync-dead-code.md) |
| M-16 | core + demo 模板 | ARCH-12 S2：`SessionID / SessionInfo`、`Transport` 一族、分片头从 `statesync` 归位到 `nettransport`；分片 API 与帧解耦（`FragmentDatagrams(DatagramMeta, …)`）；`nettransport` 与 `statesync` 互不依赖 | [M-16-transport-contracts-home.md](M-16-transport-contracts-home.md) |
| M-17 | core + codegen + demo 模板 + 文档 | ARCH-12 S3/S4：同步块收进 `sync/`（entitysync、frame、nettransport、lockstep、syncbus + driver + mirror）；`statesync` → `sync/frame` 并去前缀；`spatial.InterestManager / InterestCluster` → `policy.AOI / AOICluster`（spatial 只剩纯几何）；迁移表第三阶段 `layout:`；T-175 | [M-17-sync-layout.md](M-17-sync-layout.md) |
| M-18 | core | W-2026-09-23-01 拍板：`AsyncTransport` 删掉 datagram lane（latest-only 折叠、分片头、`AdmitBatch` 批准入），只留每会话有界 reliable 队列；`NewAsyncTransport` 下游收窄为 `ReliableSender` | [M-18-async-transport-reliable-only.md](M-18-async-transport-reliable-only.md) |

写法约定：**问题**（一句话）→ **根因**（指向具体行）→ **方案选择**（列出考虑过的方案与取舍）→
**改动**（文件与要点）→ **证明**（红测试名、修前失败文本、修后结果）→ **未做 / 边界**。
前四项已随 core v1.15.2 / kit v1.14.3 / codegen v1.15.4（2026-09-09）发版；RR-20260909-02/03/04 已修复待下次发版（core v1.15.3 / kit v1.14.4 / codegen v1.15.5）。

## 2026-09-13 第三轮:前两轮推后的四项已收敛

| 编号 | 上轮说"要先定的契约" | 这轮定成了什么 |
| --- | --- | --- |
| RR-20260913-01 | 删除水位保留多久、与可重放窗口怎么协调 | `DeleteAtVersion` 与 `Publish` 共用分片锁;墓碑按 `TombstoneTTL`(默认 = 缓存 TTL)收界,更新的快照清墓碑(U-0187) |
| RR-20260913-09 | 所有权"结果不确定"状态 | 失效 marker、独立有界重查权威:未变→恢复,已转移→按成功 fence,查不到→`Recovering` 冻结直到下一次权威读成功(U-0188) |
| RR-20260912-01 | 可取消的锁等待 | `flushMu` / `replayMu` 换成一格信号量,`select` 上 ctx(U-0186) |
| RR-20260912-02 | 有序屏障 / LSN 水位 | Sync 往 appendCh 放 `barrier` 请求,收批即答,答后再 fsync(U-0185) |

未修复清单为空。EnterShared / LeaveShared 的同类失败恢复第九轮登记为 RR-20260913-12,已由 U-0189 收敛(骨架参数化)。

### 第七轮复核后的补修(不新编号,记在原 RR 记录末尾)

| 原 RR | 归属 U | 残余 | 补修 |
| --- | --- | --- | --- |
| RR-20260913-01 | U-0187 | 在途 L2 回填越过墓碑;冷 L1 的旧删除清掉较新的 L2 | `StoreConfig.Superseded` 让三条 L1 写入口共用删除水位;L2 新增 `DeleteAtVersion` 脚本 |
| RR-20260913-05 | U-0180 | 预检查之后的 L2.Set 冲突仍被 IgnoreRemoteError 吞掉 | `ReadThroughOptions.FatalRemoteError` 分类,冲突不降级 |
| RR-20260913-02 | U-0184 | 同一时钟刻度创建的两个 Manager 代际不递增(Windows 实测) | 进程级代际高水位,播种取 `max(now, 已发出+1)` |
| RR-20260915-07 | U-0211 | Windows 上 `O_APPEND` 句柄的 `Truncate` 被拒(Access is denied),CI windows-compatibility 红 | 截断改为开追加句柄前按路径 `os.Truncate`;Linux 由 CI linux-quality 持续验收 |

## ARCH：Kit 职责边界收敛（来源 `docs/bug/REVIEW-2026-09-16-04.md` §7,不占 U / M 编号,不进覆盖矩阵）

目标:**Core 核心实现,Kit 装配与使用便利,Codegen 代码生成**。Core 不反向依赖 Kit;同一职责只保留一份实现;迁移不顺便改行为。

| 任务 | 内容 | 状态 |
| --- | --- | --- |
| ARCH-04 | 文档与生成器对齐:kit README 自述与组件表按当前目录;codegen 不再生成旧职责下的接口引用 | **第一步已做（2026-09-16）**:kit README 改为"装配层"自述,删掉 13 行已迁入 core 的包（nestwal / remote_entity / syncstream / replication / lockstep / gateway / spatial / ai / actionflow / versionstore / servicerpc / mongo/mongotest / robot）并加"已在 roost-core"说明;codegen 随 U-0217 删掉 `Grouping()` collaborator。**生成器部分已做（M-10，2026-09-16）**:servicerpc 生成传输拆成 transport（只依赖 core）/ assembly（依赖 kit mods）两半,kit 全部 RPC 接口已重生成,为 RPC 接口随领域包进 core 铺路。**RPC 接口 + 传输半进 core 已做（M-11，core v1.15.5 / kit v1.14.6 / codegen v1.15.8）**:kit 从 core 的接口生成装配半。kit README 第 3～4 节逐包核对见 kit 提交（2026-09-16）。**ARCH-04 完成** |
| ARCH-01 | 通用服务领域实现下沉 core:session（Enter 幂等 / 归属 / 会话状态机）、mail（CommitClaim）、match（票据状态、Grouping 与 ScoreWindow） | **match 的 core 半已做（M-06，2026-09-16）**:`roost-core/service/match` + `roost-core/servicemetrics`,领域测试随迁,边界测试绿。kit 半已随 kit v1.14.4 完成（别名包 + 删重复实现,kit 全套绿）。**mail / session 的 core 半已做（M-07 / M-08，2026-09-16）**：`roost-core/service/mail`、`roost-core/service/session`，领域测试随迁，边界测试绿；kit 半已随 kit v1.14.5 完成（别名包 + 删重复 + 精简 harness，kit 全套绿）。**ARCH-01 完成**：三个领域的实现、RPC 接口与传输半都在 core（M-06～M-08、M-11），kit 只剩 Mod / 装配半 / 别名。原计划:列出纯契约（`Queue` / `Subject` / `Ticket` / `Match` / `Store` 接口 / `Grouping`）、实现（`queue_store.go`）、Mod / 配置（`match_mod.go`、`redis_store.go`）、生成文件（`matchmaker_rpc_gen.go`）与外部依赖（`versionstore`、`servicemetrics`）；`servicemetrics.Reporter` 契约需先在 core 安放。Kit 侧以类型别名过渡,持久化 key / JSON 字段 / 错误码 / RPC 方法名不变。验收:kit 全套 + codegen 生成工程编译 + `go list -deps` 证明 core 不依赖 kit |
| ARCH-02 | manager 生命周期引擎（排序 / 状态 / 失败回滚 / 停止协调）迁 core | **core 半已做（M-09，2026-09-16）**：`roost-core/manager`（`Order` + `Engine`），24 条测试随迁，语义原样（不用 `TopologicalSortCache` / `lifecycle.ManagerGroup`）；kit `ManagerMod` 已随 kit v1.14.5 改为引擎包装（公开方法集不变，sentinel 同指针）。**ARCH-02 完成**。原计划：单独一批:保留启动失败仅回滚成功者、依赖错误诊断、关闭交接与稳定顺序;不换成语义不同的 `TopologicalSortCache` |
| ARCH-03 | 已正确的装配（dataengine / saga 的 Mod 调 core `Assemble` 并转发生命周期）作为迁移样板 | 无需改动,作为 ARCH-01 / 02 的形状参照 |
| ARCH-08 | room 同步的分层收敛（envelope sink 的 roomID 键；coordinator 的边界） | **并入 ARCH-10**（2026-09-22） |
| ARCH-09 | 非房间的实体复制路径与 `syncTopic` 的去向 | **并入 ARCH-10**（2026-09-22） |
| ARCH-10 | 实体同步统一为一个 SyncManager：subject 私有订阅者表 + 进程一个 manager + 每会话一个 SessionSink，room / AOI 降为 policy；帧头 Epoch/Tick 改为会话私有、RoomID 改为流常量 | **已完成**：M-13（机制，main）+ M-14（policy / ready / namespace，`feature/arch-10-sync-policy` 待 review） → [ARCH-10](ARCH-10-sync-manager.md) · [M-13](M-13-entitysync-manager.md) · [M-14](M-14-sync-policy-and-ready.md) |
| ARCH-11 | 从 wdsync 借两件事：多 profile 引用计数 + 优先级（一会话一 subject 只发最细视图，跨组织重叠有正确语义）；tick 级组件编码缓存 → 可选 `FramePerSubject` 帧模式 + `MulticastTransport` 网关扇出 | **部分实施**（2026-09-23）：多来源订阅（M-19）与 tick 内同 Profile 共享不可变编码（M-20）已落地（见 ARCH-11 §4 实施更正）；共享帧协议 + 网关多播（`FramePerSubject` / `MulticastTransport`）需网关 / 客户端配套，按用户确认定为独立后续需求（交接文档 §5），不再“待拍板”（2026-09-27 回写，OPEN-ITEMS A13④） → [ARCH-11](ARCH-11-view-priority-and-shared-encoding.md) |
| ARCH-12 | sync 块包结构整理：划清范围（两条轴七个包；spatial / syncstream / cache 是基建，remoteentity 归 dataengine 块）；删 statesync 约 1900 行老 Replicator 死码；传输契约从 statesync 归位 nettransport；八个目录收进 `sync/` 根 | **已实施**（M-15～M-18）：statesync 老 Replicator 已删除，传输契约归位 nettransport，同步块收进 `sync/`（entitysync / frame / lockstep / nettransport / syncbus），当前目录以 [sync/README](../../sync/README.md) 为准（2026-09-27 回写，OPEN-ITEMS A13④） → [ARCH-12](ARCH-12-sync-package-layout.md) · [M-15](M-15-statesync-dead-code.md) · [M-16](M-16-transport-contracts-home.md) · [M-17](M-17-sync-layout.md) · [M-18](M-18-async-transport-reliable-only.md) |

## 2026-09-19 那两条后来也修了

RR-20260919-02 与 RR-20260919-04 在同一天的第二轮里收敛：前者定了"唯一父所有权"并让越界的绑定
当场 panic（U-0252），后者把索引搬进存储、与值同一个 Lua 写（U-0253）。当时写下的"要先定什么"
就是这两个决定，记录在各自的方案一节里。

## 2026-09-19 第二轮那一条也修了

RR-20260919-10 在同一天收敛（U-0257）：owed 索引 + `OwedDispatches` RPC + `AttemptDispatch` 交给取走
payload 的一方 + sweep 收回本分。当时写下的"要先在 kit 加一个按 gameSID 的待交付 RPC"就是这次做的事。
