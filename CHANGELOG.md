# Changelog

本文件从 v1.6.2 起维护；更早版本见 git 历史。格式遵循 Keep a Changelog，版本号遵循语义化版本。

## [Unreleased]

- **ai Controller：策略回调里的 SetStrategy / Shutdown 延后到回调返回后执行**（RR-20261005-NC-241，与 B7 同向）：之前立即切换，旧行为树在 Stop 之后接着跑、发起的动作没被切换结束，成为孤儿。回调里调用返回 nil，切换出错经 OnError 报告，回调返回前 `Strategy()` 仍是旧策略。[记录](docs/bugfix/RR-20261005-NC-241.md)
- **ai Parallel 结果确定即停止本次 Tick**（RR-20261005-NC-240）：RequireAll 出现失败 / RequireOne 出现成功后不再 Tick 后面的子节点，此前会先发起没人要的动作再打断。[记录](docs/bugfix/RR-20261005-NC-240.md)
- **actionflow 两处 B7 panic 路径收敛**（RR-20261005-NC-242 / NC-243）：`Update` 的 fn panic 恢复成错误，不再让 runner 永远处在“回调中”；替换动作时旧动作 Cancel panic 只经 OnError 报告，新动作照常启动——回调里交出的 ID 一定有结论，直接 Start 返回 (ID, nil)。[NC-242](docs/bugfix/RR-20261005-NC-242.md) · [NC-243](docs/bugfix/RR-20261005-NC-243.md)
- **hotcode 插件加载修正并可回滚**（RR-20261005-NC-244 / NC-245）：以 `hotcode.Bundle` 接口变量导出的 PatchBundle 之前因变量遮蔽一律被拒；新增 `Registry.ApplyBundle`，Apply 失败或 panic 时把已替换的点恢复成加载前那一代（不是原函数），LoadPlugin 改走它，admin revert 与插件加载串行。新增真实 .so 测试包 `hotcode/plugintest`。[NC-244](docs/bugfix/RR-20261005-NC-244.md) · [NC-245](docs/bugfix/RR-20261005-NC-245.md)
- **hotcode.list 如实报告补丁**（RR-20261005-NC-246 / NC-247）：`Patched` 由 Replace / Revert 记录（同一工厂产生的闭包补丁不再误报未打补丁）；`Resolve[T]` 在签名相同只差命名时转换后返回补丁，签名不符时仍回落 fallback 并计入新字段 `PointInfo.ResolveMismatches`。[NC-246](docs/bugfix/RR-20261005-NC-246.md) · [NC-247](docs/bugfix/RR-20261005-NC-247.md)
- **ops 端口 bind 失败时进程启动失败**（RR-20261005-NC-230）：`OpsMod.Start` 在 Start 里同步 `net.Listen`，端口被占用返回 `ops: listen on ops.addr …`，按启动失败收尾；之前 bind 在后台 goroutine 里失败只记日志，进程在没有 `/healthz`、`/readyz` 的情况下继续跑，同机部署的健康检查可能探到占着端口的另一个进程。同机多实例须各配各的 `ops.addr`。[修复记录](docs/bugfix/RR-20261005-NC-230.md)
- **停机阶段的 lifecycle hook 受 `shutdown.total_timeout` 约束**（RR-20261005-NC-231）：service.stopping / service.stopped 的 hook 在停机预算内等，不配合 ctx 时 `run` 按预算返回错误；service.stopping 卡住时与 Service.Shutdown 不完整相同，不停 Service / Mod、不释放单实例锁。之前一个忽略 ctx 的 hook 让停机永远不返回。[修复记录](docs/bugfix/RR-20261005-NC-231.md)
- **停机期间发生的 fail-stop 让进程非零退出**（RR-20261005-NC-232）：信号之后才发生的 RuntimeFailure（DataEngine / Remote fatal、失锁）在 `run` 返回时并入错误；之前只写 Error 日志、以 0 退出。安全动作（围栏、失锁不 Release）不变。[修复记录](docs/bugfix/RR-20261005-NC-232.md)
- **`ops.admin_timeout`：admin 命令有期限，到期回 504**（N02 观察 O1）：`/admin/execute` 交给命令的 ctx 带 `ops.admin_timeout`（缺省 10s）的期限，Ops 写超时 = max(15s, admin_timeout + 5s)；配合 ctx 的命令到期回 504 并写明结果未知。不配合 ctx 的命令仍可能以传输错误结束，同样是结果未知。原来超过 10s 的配合 ctx 的命令需要调大该值。[方案](docs/feature/OPS-ADMIN-TIMEOUT-2026-10-06.md)
- **Redis Mod / remote_entity Mod 的停止收尾**（RR-20261005-NC-233、NC-234）：Redis Mod 第一次 Close 之后不论结果都交出连接池，错误只报告一次、之后的 Stop 返回 nil（之前每次重试都得到 `client is closed`）；remote_entity Mod 停止失败记 Warn `stop incomplete`，停完才记 `stopped`。[NC-233](docs/bugfix/RR-20261005-NC-233.md) · [NC-234](docs/bugfix/RR-20261005-NC-234.md)
- **activity 读已存窗口条目只走一个入口，运维可清除坏条目**（维护者决定 B9，RR-20261005-NC-51 复核补修）：`PendingActivities`、`DeliveringActivities`、`RetireDelivered` 与后台 sweep 都经 `readWindowEntries` 读窗口，别的组的键或不合法的键在任何列表里都跳过、保留、计数（新增 `sweep.delivering_key_malformed`）；此前 Delivering 里的这种键会让本组 sweep 去写别的组的窗口、自己永远留着，`PendingActivities` 会把它交给 game。`RetireDelivered` 只在本次确实移走时返回 true。新增持有方专用的 `Admin.MalformedWindowEntries` / `RemoveMalformedWindowEntry`（note 必填，只删确实坏的条目）。窗口记录多两个可选字段 `admin_note` / `admin_action_at_unix`。[记录](docs/bugfix/RR-20261005-NC-51.md#复核后的补修2026-10-06维护者决定-b9)
- **account 建角判定写成一张表**（维护者决定 B9）：create_role 的同名 / 换名、同名被拒后的释放与 `Admin.ResolvePendingCreation` 都查“名额状态 × 入口 × 名字状态 → 动作”的 `decideCreation`，共用同一份归类；对外行为不变。两个换名请求并发释放同一个死计划时 `create_role.plan_released` 不再计两次（RR-20261005-NC-50 复核补修）。[方案](docs/feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md)
- **`app.SingletonLiveness.Live` 契约写明：停机中的进程仍算活**（维护者决定 C5）：直到 Release 删键为止都算；activity 开窗时可能把正在停机的服算进 expected，那个窗口要等到宽限期。行为不变，用例钉住。
- **roost CLI 由入口统一接管 SIGINT / SIGTERM / SIGHUP，中断走正常回滚后再死于该信号**（维护者决定 B6，含 N08 留项 O2 / O3 / O6）：第一次信号只取消命令的 ctx；`project deps / sync / upgrade / new`、`generate`、`doctor` 在复制、生成器、go 命令或提交点前停下，杀掉 go 进程树、回滚 go.mod / manifest、删除工程旁的 `.roost-*` 暂存树，已开始的提交做完（不再留半份提交），然后 stderr 打印一行 `roost: interrupted by <sig>: …` 并以同一信号结束（退出码不变）；回滚期间再按一次 Ctrl-C 立即退出。`runCommandTree` 里的信号接管、重发等待与 NC-70 的暂存树登记表删除。另外：upgrade / sync 依赖解析失败的错误提示 `roost project deps --root <dir>`（O2）；生成器在暂存树里报的错误改指工程路径（O3）。Windows 不接管信号，行为不变。[方案与实施](docs/feature/B6-CLI-SIGNAL-OWNERSHIP-2026-10-06.md)
- **codegen 联网用例改由 `ROOST_NETWORK_TESTS=1` 打开，并在 framework-compat 的 `codegen-network` job 里跑**（维护者决定 C9，N08 O1）：过时常量 `publishedDataEngineGeneratorDependencies = false` 让事务 generate + strict doctor 的首个业务流程、player TCP doctor 两条用例永远不跑；默认 `go test` 仍不联网，根包门禁钉住该 job。[记录](docs/feature/B6-CLI-SIGNAL-OWNERSHIP-2026-10-06.md#61-最终形状)
- **配置规则由运行时加载层强制**（B10，NC-75 留项）：新增 `configdata.FieldRule`（`TableDef.Rules` / `ObjectDef.Rules`）与 `*configdata.RuleError`，required / unique / min / enum 在每次 Load / Reload 时对原始 JSON 检查（缺列与零值分得清），ref 在全部表加载后检查；违反即整次拒绝、旧快照保持，错误点名表 / 行 / 字段 / 规则。规则的表示与检查只有一份（新叶子包 `configdata/rules`，codegen 唯一允许 import 的 core 包）：tablegen 把 schema 标签翻译成同一组规则，写进生成的 loader（取代逐表手写的 ref 循环），CSV 转换与 `-json -check` 用同一个检查器；auto 表的 `cfg` 标签（cfggen 输出）解析成同一组规则。tablegen 新增 `enum:"a|b"` 标签与每表一个 `<Type>Table()`；cfggen 新增 `unique` / `min` / `enum`，`required` 不再要求 `ref`（含义统一为“键出现且不为 null”，配 ref 时零值也要是目标主键）。以前直接改 `configs/data` 再 reload 能绕过的数据，现在启动 / reload 被拒（T-261）。
- **热更失败与回滚可见**（C2，N07 C-O5 / C-O6 / C-O7）：契约不变——新的一代在 AfterApply 之前已发布，撤回 / 回滚前准入的请求读被撤回的那一代（写进 configdata 包注释与 USER_GUIDE §10）。Store 新增 `OnReloadOutcome`，每次 Load / Reload / Rollback 恰好报告一次（含 build 阶段的失败）并写日志：失败 Warn `config reload failed`、撤回 Warn `config reload reverted`、成功 Info。kit configdata Mod 改为据此计 `configdata.reload.total{result=ok|failed}` 与新的 `configdata.rollback.total{trigger=apply_failed|operator}`：去掉运维自由填写的 `reason` 标签，`result` 不再有 `rollback` 取值，被撤回的 reload 不再同时记一次 ok。observability README 改正“热更后版本 +1”。
- **生成器 Core 下限需上调**：新生成的 loader 使用 `configdata.FieldRule`（v1.20.2 没有）。下次发版时把 `minimumVersions.Core` 与 framework-compat 的 minimum 行同步改到新版本；在此之前按 pin 跑的 `tablegen-runtime.sh` / `cfggen-golden-runtime.sh`（新增 `ROOST_CORE_DIR` 可指向本地 core）与 framework-compat 最低版本格会编译失败。已生成的工程不迁移，旧 loader 在新 core 上照常工作（不查 required），重新 `roost generate` 后获得运行时规则。[方案](docs/feature/B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md)

## [v1.20.2] - 2026-10-06

> 补丁版本：维护者 10-05 第二、三轮决定的实施与 v1.20.1 之后非核心 review 的收敛。决定项：A1 回滚统一走 DAO、A2 Redis / Mongo 驱动不重放写、A3 停机对象共用 `operation.Lifetime` 与契约骨架、A4 框架配置一律严格读取（含 kit/redis 与生成代码两项留项）、A5 隔离环境共享使用、C1 生产校验只查有读取方的设置、B1 saga completion 核对代际、B2 Remote 快照以 L2 为水位权威、B3 skill lower 查找失败 fail-fast、B7 actionflow 回调变更进延后队列、C4 活动组文件、C6 服务指标默认开启、C7 遍历回调契约。非核心 review：N05（NC-130 / 131）、N09 第三批（NC-150～154）与第四批（NC-210～216）、N12（NC-160～165）、停机三步（NC-170～174）、N13（NC-180～185，含 NC-180 / 181 复审补修）、N14（NC-190～194）、N15（NC-200～208，含 NC-208 补修），以及 NC-173 复核补修。**行为变化**：框架与生成代码的布尔 / 时长 / 整数配置严格读取，写错类型启动即点名报错；`env: production` 只校验有读取方的设置；Redis 写命令回复丢失不再重放、返回结果未知；服务指标默认写进 metrics 注册表；actionflow 回调里的变更延后到回调返回后执行；skill 编译按 Runtime 实际执行范围收紧。**破坏性变更**：game 配置键 `activity.game_sids` 删除，改用活动组文件；`ActionRunner` 不再返回 `ErrReentrantMutation`；服务指标的名字与 `Recorder` 事件名变化；以前被接受的配置写法与 skill 定义现在启动失败；生成器 Core 下限升到 v1.20.2。已生成工程不提供迁移（维护者决定）。

### 行为变化与破坏性变更（升级前必读）

- **严格配置读取**（A4）：`on` / `yes`、不带单位的时长（`15` 不再是 15ns）、`8k` / `1.5` / `10s` 这样的整数，在 `ValidateServiceConfig`（任何 Mod Init 之前）与各 Mod Init 点名报错；以前静默读成 false / 纳秒 / 0（取默认）。覆盖 app、kit 全部 Mod、kit/redis 三个整数键、生成的 player TCP 接入层与 RPC 客户端 Mod。升级前请用新版本启动一次检查配置。
- **生产校验变化**（C1，NC-192）：`env: production` 不再要求 `player.login_auth_required`、`player_protocol.rate_limit.enabled`、`save_load.wal.*`、`instance.*`、`account.ops_token`、`*.redis_required` 等没有读取方的键；保留 ops 端点暴露、各服务真正读取的密钥、依赖 Redis 的服务的 `redis.addr`、admin_gateway 令牌。生成的生产示例与 k8s Secret 示例打开 `env: production` 可以启动。
- **活动组文件替代 `activity.game_sids`**（C4，破坏性）：协调器与 game-demo 的 game 经 `activity.groups_file` 读 `configs/activity_groups.yaml`，每组至多 64 个 game，违例启动即点名文件报错；game 配置键 `activity.game_sids` 删除。旧工程的 game 代码仍读旧键、不受影响，不迁移。
- **Redis 写不重放**（A2）：写命令、含写的 pipeline、`EvalBatchDurable` 与 DistLock 命令回复丢失时以传输错误返回（结果未知），不再由 go-redis 重放；只有能证明未执行的错误（`driver.IsDefinitelyNotExecuted`）才重发。Mongo 提交发出后失败包上 `mongo.ErrCommitResultUnknown`。依赖驱动“救回”写的调用方现在会看到错误。
- **metrics 默认开启**（C6）：生成工程的 `Metrics()` 默认返回 `servicemetrics.NewMetricsReporter`，六个固定名指标经 ops `/metrics` 导出；`service_metrics.enabled: false` 或返回 nil 关闭。match 队列深度 / rank 看板大小改为固定名加 `key` 标签，`Recorder` 事件名变成 `depth:queue{<key>}` / `depth:board{<id>}`，session 清扫数从 gauge `session.swept` 改为计数——依赖旧名字的看板与告警需要调整。
- **actionflow 回调语义**（B7，破坏性）：Start / Tick / Cancel 与 OnQueued / OnTransition / OnEnded 里对 `ActionRunner` 的变更延后到回调返回后按发起顺序执行，回调里的 Start / Enqueue 返回已分配 ID 与 nil；`ActionRunner` 不再返回 `ErrReentrantMutation`（`MissionRunner` 照旧）；新增延后队列上限 `MaxDeferredCommands` / `MaxDeferredSteps`。
- **skill 编译收紧**（NC-150～152、NC-210～215、B3）：字段名逐字匹配；phase `on.recast` / `on.timeout`、未声明的 memory、catalog 外的 status / attribute / resource 名、Host 不执行的 chain / modifier 参数等在启动期 CompileAll 报错；lower 查找失败返回 `LOWER_UNRESOLVED`。以前能编译的这类定义现在启动失败。
- **生成器 Core 下限 v1.20.2**：新生成的工程调用 `activity.LoadGroupsFile`、`servicemetrics.NewMetricsReporter`、`app.ConfigReader` 等 v1.20.1 没有的接口；framework-compat 的 minimum 格随之改为 v1.20.2。
- **已生成工程不迁移**：模板侧变化（A1 DAO 回滚、C4 组文件、C6 默认 metrics、A4 生成代码严格读取、NC-206 `.gitignore`）只对新生成或 `roost project sync` 后的工程生效；旧工程在新 core 上仍由启动校验兜住配置类型。
- 其他收紧：saga completion 核对代际（B1）、Remote Cached 读有最大陈旧时间 `remote_entity.cached_max_staleness`（B2）、停机入口超时返回 ctx 错误并保留对象（NC-170～174、A3）、failurelog 脚本结果未知不再降级（NC-160）、loadtest 无样本判失败（NC-161）、glsvet 对没检查到的输入退出 2（NC-204）、隔离环境全局命令运行期间持有验收锁（A5、NC-203）。

### Changed

- **生成器 Core 下限升到 v1.20.2**：`codegen/internal/roost/manifest.go` 的 `minimumVersions.Core`、`.github/workflows/framework-compat.yml` 的 minimum 格与 `codegen/ci/framework-release.yaml` 的 `release:` 同步改为 v1.20.2。原因：生成工程调用了 `activity.LoadGroupsFile`（C4）、`servicemetrics.NewMetricsReporter`（C6）、`app.ConfigReader` / `app.ConfigDuration`（A4 生成代码留项）等 v1.20.1 没有的接口。
- **生成的 player TCP 接入层与 RPC 客户端 Mod 严格读取配置**（维护者决定 A4 的留项，行为收紧）：生成的 `internal/access/player/tcp/server_gen.go` 用 `app.ConfigReader` 读 `player_access.tcp.*` 与 `nest.request_timeout`，RPC 客户端 Mod 模板（kit/service 的 `*_rpc_assembly_gen.go` 与工程 `add rpc` 的产物）用 `app.ConfigDuration` 读 `<service>.call_timeout`。`enabled: on`、`max_payload_bytes: 8k`、`max_handshake_bytes: -1`、`idle_timeout: 90`、`call_timeout: 5` 这类写法在 Mod Init 点名报错，不再读成 false / 默认值 / 纳秒；守卫测试把两份模板纳入扫描，生成文件不再放行。经 App 启动的进程此前已由启动校验拒绝，行为不变；变化在直接调用 Mod Init 的路径。新生成的代码需要 core ≥ v1.20.2，已生成工程不迁移。[方案](docs/feature/REFACTOR-2026-10-05-strict-config-reads.md#a4-留项生成进工程的代码2026-10-06)
- **kit/redis 的三个整数键严格读取**（维护者决定 A4 的留项）：`redis.db` / `redis.pool_size` / `redis.min_idle_conns` 由 Redis Mod 与单实例锁的 `SingletonStore` 用 `app.ConfigReader` 读，`8k`、`1.5`、`10s` 在 Init 时点名报错，不再被读成 0；守卫测试删掉这三项放行。经 App 启动的进程此前已由启动校验拒绝，行为不变。[方案](docs/feature/REFACTOR-2026-10-05-strict-config-reads.md#a4-留项kitredis2026-10-06)
- **服务指标默认落到 metrics 注册表，名字里不再带 ID**（维护者决定 C6，接 N06 观察 1 / N12 O1）：新增生产 Reporter `servicemetrics.NewMetricsReporter(service)`（kit 别名同名），把 Accepted / Refused / Replayed / Dropped / Conflict / Depth 写成 `service.accepted.total` 等六个固定名的指标，服务、操作、原因、对象都在标签里，经 ops `/metrics` 导出；每次上报解析当时的默认注册表，所以先建 Reporter、后起 App 也落在 App 导出的那个。生成工程的 `Metrics()` 默认返回它（以前返回 nil，事件无处落地）；`service_metrics.enabled: false`（kit 各服务 Mod 经 `mods.ServiceMetrics` 处理）或返回 nil 可以关闭。match 队列深度与 rank 看板大小改为固定名加 `key` 标签（新增可选接口 `servicemetrics.KeyedReporter` 与 `Sink.DepthOf`；不实现它的项目 Reporter 仍收到旧形状 `queue.<key>`），`Recorder` 的事件名随之变成 `depth:queue{<key>}` / `depth:board{<id>}`；session 清扫数从 gauge `session.swept` 改为计数 `Dropped("run.swept", n)`。已生成工程不迁移。新生成的工程用到新 API，需要 core ≥ v1.20.2。[方案](docs/feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md)
- **活动组由一个配置文件定义，每组至多 64 个 game**（维护者决定 C4）：托管 activity 协调器的工程生成 `configs/activity_groups.yaml`（`groups: [{id, game_sids}]`，只创建一次，之后归项目），协调器与 game-demo 的 game 都经 `activity.groups_file` 读它，校验只有一份 `kit/service/global/activity.LoadGroupsFile`：每组成员数上限就是协调器单窗口 expected 集合的上限 `MaxExpectedGames`（64），组内重复、一个 sid 属于两个组、非正数或超出 int32、未知字段、game 的 sid 不在任何组里，都在启动时点名文件报错——之前 65～200 个候选能启动，同时活着超过 64 个时每个窗口被协调器拒绝。协调器新增可选键 `activity.groups_file`：设置后启动时校验，`sweep_groups` 为空时后台 sweep 扫文件里的全部组（之前生成的 `sweep_groups: []` 让 demo 的宽限窗口没有进程兜底）。game-demo：组 id 来自文件（不再是常量 `GroupID`，`gameactivity.Key` 多一个组参数），贡献榜键加组（`<prefix>:board:<组>:<窗口>`），`second-game.sh` 启动前检查 sid 在组文件里；Dockerfile 把文件拷进镜像，shell `install.sh` 装进 release。**game 配置键 `activity.game_sids` 已删除；维护者决定不迁移已生成的工程**——旧工程的 game 代码仍读旧键、不受影响，想用组文件请按新模板重新生成或手工移植。[方案](docs/feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md)
- **saga 协调器接收 completion 时核对代际**（维护者决定 B1，接 U-0280，行为收紧）：completion 的代际从 `CommandID`（`<key>:rN:<attempt>`）解析，与记录的 `Incarnation` 比较。Resume 之后旧一生晚到的拒绝 / 失败不再被新一生接收（以前会让新一生失败或消耗重试，而新一生的尝试照常执行生效），只计 `Stats().StaleIncarnation` 与 `saga.completion.stale_incarnation_total{saga_type,phase}`；旧一生的成功在记录停在该操作上时（含 Resume 之后、派发之前）直接接收为结果，不再先告警再计入。放弃后迟到的成功按（操作，代际）只告警一次（tombstone 新增 `late_alarms` 子文档，可选接口 `LateSuccessAlarmStore`），重复送达计 `Duplicates`。补偿方向 `ManualRequired` 上的人工 `Compensate` 与 `Resume` 一样进入新一生，补偿真正重新执行（以前复用上一轮的 `CommandID`，原生收件箱报身份冲突一直 nak）。持久格式只增；Mongo 步骤的跨尝试幂等仍不在框架契约内。[方案与实施](docs/feature/B1-SAGA-COMPLETION-INCARNATION-2026-10-06.md)
- **skill lower 查找失败一律报编译错误；phase 事件派发表单一来源**（维护者决定 B3 ①②）：lower 里每个名字到槽位 / handle 的查找（status / attribute / resource / tag / collision / unit template、damage 语义、snapshot / state / temporal / process property、memory、goto 与初始 phase、`$memory.` / `$local.` / `$input.` 引用）查不到时返回新诊断码 `LOWER_UNRESOLVED`、不交出 Program，不再用零值兜底或 panic——类型检查之外的第二道防线，正常定义的输出与 digest 不变。phase 事件名与“Runtime 是否派发”只在 `phaseEventTable` 写一次，lower 导出、编译期拒绝与 Runtime 派发点共用。[方案与实施](docs/feature/B3-SKILL-LOWER-FAILFAST-2026-10-06.md)
- **Remote 快照缓存以共享 L2 为水位权威，Cached 读有成文的最大陈旧时间**（维护者决定 B2，行为收紧）：一个 key 的全部 L1 写入（发布、复制、权威加载、L2 回填、删除）先在 L2 上以版本 CAS / 带版本删除落地，L1 只记 L2 确认过的值并带确认时刻；L2 断网或结果未知时 L1 照记但未确认。新增 `remote_entity.cached_max_staleness`（`Config.CachedMaxStaleness`，缺省 = `snapshot_cache_ttl`）：Cached / Monotonic 只交出上限内已确认的条目，其余先重新确认（读 L2、补写丢失的 L2 写 / 删除、回源权威），确认不了返回错误。复制 wire 增加 `published_at`，早于 `snapshot_l2_ttl / 2` 的快照更新不再被接受（N05 O5：DeliverAll 重放在 L2 过期后复活旧版本）。L2 `DeleteAtVersion` 被更新快照拒绝时返回 `cache.ErrStaleWrite`。删掉本机墓碑侧表、`Superseded` 钩子用法、`ReadThroughStore` / `FatalRemoteError` 分类与 L1 冷时的 L2 预查；L2 键格式与脚本不变。冷节点复制写入少一次 HGET（−38%），读命中无退化。[方案与实施](docs/feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md)
- **actionflow 回调里的变更进延后队列，判定集中一处**（维护者决定 B7，方向 b）：动作的 Start / Tick / Cancel 与 OnQueued / OnTransition / OnEnded 钩子里对 `ActionRunner` 的 Start、Enqueue、Tick、End、EndAll、ClearQueue、ClearMission、Recover 不再立即执行，回调返回后由最外层调用按发起顺序执行；删掉 U-0100 / NC-122 那组事后比对当前位的分支，`ActionRunner` 不再返回 `ErrReentrantMutation`（`MissionRunner` 照旧）。回调里的 Start / Enqueue 返回已分配的 ID 与 nil（新增 `Deferring()`），拿到 ID 的动作一定收到 OnEnded；直接 Start 失败也发一次 OnEnded（O-A2）；Cancel 重入不再让同一动作被取消两次（O-A3）。新增 `ActionRunnerConfig.MaxDeferredCommands`（默认 64，超出 `ErrDeferredQueueFull`）与 `MaxDeferredSteps`（默认 1024，回调互相触发时截停并返回 `ErrDeferredRunaway`）。`ai` 只补注释。[方案与实施](docs/feature/REFACTOR-2026-10-06-actionflow-deferred-mutations.md)
- **框架配置一律严格读取，启动校验覆盖全部类型化的键**（维护者决定 A4，接 RR-20261005-NC-190，行为收紧）：新增 `app.ConfigInt` / `ConfigInt64` 与汇总错误的 `app.ConfigReader`；kit 的 dataengine、remoteentity、saga、nats、nest、syncbus、mongo、ops、etcd、statslog、platform / activity 服务与 app 自身不再用 viper 的宽松 getter，`ValidateServiceConfig` 在任何 Mod Init 之前按 `frameworkBoolKeys` / `frameworkDurationKeys` / `frameworkIntKeys`（含 syncbus 三段与 `<service>.call_timeout`）检查。`on` / `yes`、不带单位的时长、`8k` / `1.5` / `10s` 这样的整数启动即报错并点名键，而不是读成 false / 纳秒 / 0（取默认）。守卫测试扫描源码，新增宽松读取或漏登记的键变红。kit/redis 三个整数键（A2 之后）与生成的 player TCP / RPC 客户端代码（生成器 Core 下限升到 v1.20.2 时）随后也改为严格读取，见上两条。[方案](docs/feature/REFACTOR-2026-10-05-strict-config-reads.md)
- **`env: production` 只校验有读取方的设置**（RR-20261005-NC-192，维护者决定 C1 方案 1）：删去 `player.login_auth_required`、`player.login_secret`、`player_protocol.rate_limit.enabled`、`save_load.wal.*`、`instance.*`、`account.ops_token`、`account.redis_required`、`global` / `match_group.redis_required` 这组没有任何代码读取的要求；保留 ops 端点暴露、各服务真正读取的密钥、依赖 Redis 的服务的 `redis.addr`、admin_gateway 令牌。生成的生产示例与 k8s Secret 示例打开 `env: production` 现在可以启动。USER_GUIDE §10 写明校验范围。[记录](docs/bugfix/RR-20261005-NC-192.md)
- **隔离测试环境按共享模式使用，全局运维命令运行期间持有验收锁**（维护者决定 A5，RR-20261005-NC-203 复核补修）：`dataengine-env.sh` 的 up / down / heal / reset / fault、`scripts/remote-fault.sh`、带 `ROOST_REMOTE_FAULT` 的 `scripts/test-remote-generated.sh`、`kit/dataengine` 的三个 failover 用例从“检查锁空闲”改为运行期间持有 `remote-acceptance.lock`（持锁者的子进程沿用）。规则写进 `kit/scripts/integration/README.md` 与 roost-bugfix lessons：并行会话各跑 `-run` 选定的用例，故障一律自建代理 / 进程。[记录](docs/bugfix/RR-20261005-NC-203.md#复核后的补修2026-10-05维护者决定-a5)
- **停机对象共用“准入 + 在途计数 + 等待”与停机契约测试骨架**（维护者决定 A3）：bus JetStream RPC、syncbus JetStream、mirror Replicator 三份手写准入门改用 `internal/operation.Lifetime`（新增 `Wait(ctx)`：幂等关准入、在 ctx 内等排空、已排空时任何 ctx 都返回 nil），错误值与停止顺序不变；唯一可观察差异是“已排空且 ctx 已结束”时不再随机返回 ctx 错误。新增测试骨架 `internal/stopcontract.Check`，已套 manager、kit/nest、syncbus、etcd、mirror、remoteentity、bus 与生成 TCP 的停止入口。`glsvet` 默认对带 ctx 的停止类函数里不受 ctx 约束的通道接收打印 `hint:`（`-stophints=false` 关闭），提示不计入违例、不改退出码。[方案与实施](docs/feature/REFACTOR-2026-10-05-shared-stop-contract.md)
- **回滚统一走 DAO**（维护者决定 A1）：事务内会改的状态一律放进 DAO，Nest 的 DAO 回滚（undo 逆操作 / state 快照）是唯一的回滚，组件不再自己登记 undo。生成 DAO 的 `dao:"nopersist,nosync"` 字段现在有 mutator（以前只有 getter，map 连 getter 都没有）：参与回滚、不落库、不进 WAL、不同步；只新增方法。game-demo 模板的属性三层（新增 `attr_gear`）与 World 定时器（新增 `timer_next_due`）改为全在 DAO、组件无状态，删掉 NC-61 / NC-140 的 `captureRollback`；`skill/combatcomponent` 的逆操作登记移进 `CombatDao`。`cmd/glsvet` 对组件方法里的 `RecordUndo` / `DeferRollback` 打印 `hint:`（不计入失败）。持久格式不变；已生成工程不迁移（`roost project sync` 可取新模板）。skill Runtime 状态未纳入，列为后续。[方案与实施](docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md)

### Fixed

- **遍历回调定为仓库级契约，补两处残余**（维护者决定 C7；RR-20261005-NC-180 / NC-181 复核后的补修）：凡框架交给业务的 `Range` 回调里可以读写同一个容器、返回 false 立即停止，写进 roost-coding、根 README §16 与 `safemap` 包注释；新增共用回归辅助 `internal/rangecontract`，已套 container、safemap、entity 与生成 DAO 三种 map 的 `RangeX`。`BucketHolder.RangeWithCursorCnt` 回调里再做游标遍历不再让外层重走同一个桶（同一条目交出两次）；`EntityManager.RangeGroupEntities` 先持引用再交出，回调里销毁同组实体后不再以 ID 0 交出已清零的实体。[方案与实施](docs/feature/C7-RANGE-CALLBACK-CONTRACT-2026-10-06.md)
- **etcd `Assembly.Close` / `EtcdMod.Stop*` 停完后再调用返回 nil**（RR-20261005-NC-173 复核补修，A3 契约骨架发现）：`clientv3.Client.Close` 不幂等，第二次关闭返回 `context canceled`；`driver.Client.Close` 现在只关一次、之后返回第一次的结果。[记录](docs/bugfix/RR-20261005-NC-173.md#复核后的补修2026-10-05a3-停机契约骨架发现)
- **Redis 驱动默认不再重放写命令；Mongo 提交结果未知时带专用哨兵**（A2，NC-21 / NC-52 / NC-100 / NC-101 / NC-160 同一类问题，行为收紧）：写命令（SET、SETNX、DEL、INCR*、HSET / HDEL、L/RPUSH、L/RPOP、LTRIM / LREM、ZADD / ZREM、SADD / SREM、PUBLISH、EVAL / EVALSHA）、含写的 pipeline、`EvalBatchDurable` 以及 DistLock 的三条命令，全部不经 go-redis 重放。回复丢失时以传输错误返回，属于结果未知；修前会被重放成 INCR 加两次、RPUSH 追加两次、SETNX 把自己的键报成“已被占用”。只有错误证明命令没执行时（拨号失败、池超时、LOADING / MASTERDOWN / TRYAGAIN / CLUSTERDOWN / max clients / READONLY / NOREPLICAS）才重发，脚本也一样（补回 NC-100 之后丢掉的可用性）。读命令照常重试。新增 `driver.IsDefinitelyNotExecuted(err)`；`ISession.WithTransaction` 在提交发出之后失败时包上 `mongo.ErrCommitResultUnknown`，`errors.Is(err, context.DeadlineExceeded)` 仍然成立。cache 的 Redis hash / 有序集合在写结果未知时仍补发 EXPIRE。契约表见 [redis/driver](redis/driver/README.md) 和 [mongo/driver](mongo/driver/README.md)，[方案与红绿](docs/feature/A2-DRIVER-REPLAY-CONTRACT-2026-10-05.md)
- **skill 编译器按 Runtime / Host 实际能执行的范围收紧**（RR-20261005-NC-210 / NC-212～NC-215，行为收紧）：set / add / clear_memory 的名字必须已声明、add_memory 只用于 int memory（此前未声明的名字落到槽位 0，静默改写别的 memory 或运行期 ErrProgramInvariant）；effect、filter、cost 里的 status / attribute / resource 名字必须在 catalog 里（此前兜底成 handle 0）；CompileEnvironment 的九类 catalog key 必须非空唯一；chain 的 `allow_repeat` / `hop_interval_ticks` 与 attribute_modifier 的 `stack_policy` / `max_stacks` 只接受默认值（它们从不传给 Host）；attribute_modifier 的 operation 须为 add / mul_bp 且在属性 catalog 的 `ModifierOperations` 里、时长为正，add_status 时长不能为 0，resource operation 限 set / add / spend / sub，cost 字面量非负，attribute_compare 的 op 须为比较运算。受影响的定义在启动期 CompileAll 失败并指向字段。[记录](docs/bugfix/RR-20261005-NC-214.md)
- **移交后的 area 回调 `finish` 不再让 Runtime.Advance 返回 ErrProgramInvariant**（RR-20261005-NC-211）：施法先结束、spawn 出的 area 进程已移交时，回调里的 finish 只停止本 area 进程；施法存活时仍结束施法。[记录](docs/bugfix/RR-20261005-NC-211.md)
- **skillsync.NegotiateSchema 拒绝空区间**（RR-20261005-NC-216）：任一边 `Min` 为 0 或 `Min > Max` 时返回 `ErrSchemaNegotiationFailed`，协商结果必须同时被两边 `Contains`。[记录](docs/bugfix/RR-20261005-NC-216.md)
- **布尔开关与时长严格读取**（RR-20261005-NC-190，行为收紧）：`singleton.enabled: on` / `yes`（以及 `nats.reliable.enabled`、`mongo.require_replica_set`、`ops.*`、`log.*` 等框架开关的非布尔写法）启动校验报错，不再被读成 false——单实例锁曾因此静默关闭；`singleton.*` 时长、`ValidateServiceConfig` 列出的时长键、`saga.step_defaults` / `saga.steps` 与服务 Mod 的 `mods.Duration` / `RequiredDuration` 拒绝不带单位的数字（以前读成纳秒并通过校验）。新增 `app.ConfigBool` / `app.ConfigDuration`、`mods.RedisClusterAddrs`；`redis.cluster_addrs` 接受逗号串或 YAML 列表（以前列表读成空串、Redis Mod 退回 localhost）。[记录](docs/bugfix/RR-20261005-NC-190.md)
- **Mongo Mod 连接日志不再含口令**（RR-20261005-NC-191）：`mongo mod: connected` 的 `uri` 把 userinfo 口令换成 `***`。[记录](docs/bugfix/RR-20261005-NC-191.md)
- **启动失败先收回 Service 已启动的部分**（RR-20261005-NC-193，契约补充）：`Service.Init` 返回错误时 App 同样调用 `Shutdown`（限时 5s），再停 Mod、释放单实例锁；Shutdown 没在时限内结束（含不配合 ctx、panic）时不停 Mod、不释放锁，与正常停机一致。以前 Init 失败直接拆依赖，Init 之后的收尾不配合 ctx 会让进程挂住。`Shutdown` 须容忍部分初始化。[记录](docs/bugfix/RR-20261005-NC-193.md)
- **saga 步骤覆盖按小写回退**（RR-20261005-NC-194）：`kitsaga.NewMod()` 不带定义时，`saga.steps.GiftItem.Debit` 这类大小写混写的覆盖不再静默失效。[记录](docs/bugfix/RR-20261005-NC-194.md)
- **隔离环境入口尊重 Remote 验收锁**（RR-20261005-NC-203，行为收紧）：`<根>/remote-acceptance.lock` 被别的运行持有时，`dataengine-env.sh` 的 up / down / reset / fault / heal / test 与 `scripts/remote-fault.sh` 以 2 拒绝（status 照常）；`test` 运行期间自己持锁；矩阵与 `scripts/perf/remote.sh` 取得锁后导出 `ROOST_REMOTE_ACCEPTANCE_LOCK_HELD`，自己调起的 heal / remote-fault 照常执行。之前 reset 能在矩阵运行中删掉整个根（含锁）。[记录](docs/bugfix/RR-20261005-NC-203.md)
- **toxiproxy pid 按所有权认领、toxic 用例不再 /reset 共享 toxiproxy**（RR-20261005-NC-202 / NC-208）：pid 文件指向的进程须是 `toxiproxy-server -port <本环境 API 端口>`，否则 down / reset 拒绝而不是杀掉复用该 pid 的进程；`redis/driver`、`kit/nats` 的 toxic 用例改为自建随机端口代理、只删自己的毒；补修：`kit/dataengine` 的三条 `TestToxicNATS*` 同样改为自建代理、只删自己的毒（DataEngine fixture 凡经代理即忽略 gossip 发现的节点），不再需要独占环境。[记录](docs/bugfix/RR-20261005-NC-208.md)
- **redis-cluster-suites.sh 不再把全环境故障套件跑到共享环境**（RR-20261005-NC-201）：脚本清掉 `ROOST_DATAENGINE_IT` / `REDIS_ADDR`，只跑 Cluster 准入的用例。[记录](docs/bugfix/RR-20261005-NC-201.md)
- **glsvet 对没检查到的输入以 2 退出**（RR-20261005-NC-204，行为收紧）：参数目录不存在、`<dir>/...` 的根不存在或文件解析失败时报错退出 2；之前按 0 个违例放行，路径拼错的 CI 门禁永远是绿的。[记录](docs/bugfix/RR-20261005-NC-204.md)
- **pretag / 故障矩阵 / gapmap 不再对没核对的东西报通过**（RR-20261005-NC-205 / 207 / 200）：pretag 在 origin 不可达时失败（之前跳过远端同名 tag 检查）；矩阵格日志含 `no tests to run` / `[no test files]` 记 FAIL；`scripts/gapmap.sh` 在跟踪文件有未提交修改时拒绝启动（之前收尾 `git checkout -- .` 会把它们一起丢掉）。[记录](docs/bugfix/RR-20261005-NC-200.md)
- **生成的 .gitignore 忽略 `/data/wal/`**（RR-20261005-NC-206）：DataEngine 默认 WAL 目录不再被 `git add -A` 提交。.gitignore 归工程所有、sync 不覆盖，**已有工程请手工补 `/data/wal/` 一行**，已提交的 WAL 段自行 `git rm --cached`。[记录](docs/bugfix/RR-20261005-NC-206.md)
- **BucketHolder / EntityManager 的遍历回调可以改同一容器，返回 false 立即停止**（RR-20261005-NC-180 / NC-181）：`Bucket.Range` 改为读锁内复制、锁外调用回调——之前回调里 `Del` / `Add` / 未命中 `Get` 当场自锁，`EntityManager.Range` 回调里 `Destroy` 会卡死全进程的 Add / Destroy；`RangeAll` / `RangeWithCursorCnt` 见 false 跨桶停止，`EntityManager.Range` / `RangeByCategory` 写明的提前停止此前不成立。回调看到的是快照：`BucketHolder` 期间删除的条目仍可能交出；`EntityManager.Range` / `RangeByCategory` / `CountByCategory` 对每个实体先持引用（`Touch`）再回调，到达前已摘除的实体不交出，回调期间被销毁的实体推迟到引用归还才清理（复审补修，之前会交出 ID / 分类已清零的实体，`-race` 下与 `doClear` 数据竞争）；要改实体照例拿锁后复核 `IsRemoved`。[记录](docs/bugfix/RR-20261005-NC-180.md) · [NC-181](docs/bugfix/RR-20261005-NC-181.md)
- **FastMap（DAO `map=fast`）遍历回调里改 map 不再交出不存在的键**（RR-20261005-NC-182）：`Set` 改已有键不再触发扩容重排；`Range` 识别表被换掉后到当前表里查剩余键；`Clear` 不清零旧数组。之前 `RangeItems(func(k, v) { SetItems(k, v*10) })` 在装载率到阈值时交出零值键、漏改原有键，并把 `items.0` 写进提交。[记录](docs/bugfix/RR-20261005-NC-182.md)
- **TaskPool / TopologicalSortCache / KeyMap 三处公开 API 的缺陷**（RR-20261005-NC-183～185，仓内零调用方）：`TaskPool.Submit` 与 `Shutdown` 并发不再 panic “send on closed channel”；拓扑排序遇到未单独注册的依赖不再误报环，也不再让未注册节点掩盖真正的环；`KeyMap.Range` 回调删除当前键不再漏键、交出零值键。[NC-183](docs/bugfix/RR-20261005-NC-183.md) · [NC-184](docs/bugfix/RR-20261005-NC-184.md) · [NC-185](docs/bugfix/RR-20261005-NC-185.md)
- **停机入口超预算时如实返回 ctx 错误并保留对象，重试继续等到真实排空**（RR-20261005-NC-170～174，行为收紧）：`manager.Engine` / `kit/manager` 不再在第一次超时后清空列表（重试曾报成功），也不在过期 ctx 下继续停依赖；kit Nest Mod 的卸载后重载停止句柄保留到 worker 退出、entitysync 排空后才关闭；JetStream 同步总线新增 `StopWithContext`（`Stop` 现在等在途 handler 返回），停止后的投递 NAK 交还 broker、停止后拒绝 `Subscribe`；etcd `Discovery.Deregister` 在 ctx 内等注册循环，`Assembly.Close` 只在注销成功后关闭 client；`mirror.Replicator` 新增 `StopWithContext`，remoteentity `Assembly.Stop` 等已进入 ApplyReplica 的 handler。[证据](docs/bugfix/evidence/noncore-bugfix-20261005-stopshape/README.md)
- **failurelog 在脚本结果未知时不再走非原子降级**（RR-20261005-NC-160，行为收紧）：`AppendRaw` / `DeleteRaw` / `Purge` 的 Lua 脚本报错（连接断开、读超时、ctx 到期）时原样返回错误，不再补做 RPUSH / LREM / LLEN+DEL——那会让同一条死信写两份、多删一条同值记录、把清空之后新到的死信删掉。降级只留给 Eval 返回 `(nil, nil)` 的无 Lua 适配器。bus 侧表现为 `bus: write dead letter failed`。[记录](docs/bugfix/RR-20261005-NC-160.md)
- **robot loadtest 阈值没有样本时判失败**（RR-20261005-NC-161，行为收紧）：没有完成的场景时 `error_rate`、没有成功场景或耗时直方图序列缺失时分位数阈值判违反，`ThresholdResult.reason` 写 `no_samples`；此前按 0 通过（`-duration` 短于场景耗时的运行退出码 0）。新增 `metrics.HistogramCount` 与可选 `loadtest.Config.SampleCount`。[记录](docs/bugfix/RR-20261005-NC-161.md)
- **robot 重连后再次 `EnsurePushCapture` 在新会话上注册**（RR-20261005-NC-162）：之前标记残留让安装变成空操作，重连后的推送被丢弃。[记录](docs/bugfix/RR-20261005-NC-162.md)
- **robot websocket 拨号服从 ctx 与 `DialTimeout`**（RR-20261005-NC-163）：超时覆盖建连与升级握手，之前握手不回时永久阻塞。[记录](docs/bugfix/RR-20261005-NC-163.md)
- **statslog 的 `entity.count_by_kind` / `entity.count_by_category` 在实体清空后归零**（RR-20261005-NC-164）。[记录](docs/bugfix/RR-20261005-NC-164.md)
- **`log.Close` 之后的日志不再丢失**（RR-20261005-NC-165）：关闭文件 sink 后默认 logger 改写控制台输出，只配文件时写 stderr；进程退出原因（main 的 `server exit`）在 `log.stdout: false` 时也有去处。[记录](docs/bugfix/RR-20261005-NC-165.md)
- **skill 的编译器只接受 Runtime 真正会执行的形状**（RR-20261005-NC-150 / NC-151 / NC-152，行为收紧）：`Parse` 要求字段名逐字匹配（此前 `encoding/json` 的大小写不敏感匹配让 `"id"` 与 `"ID"` 同时被接受、后者生效，`"Cooldown_Ticks"` 被当作 `cooldown_ticks`）；phase 的 `on.recast` / `on.timeout` 编译期拒绝（Runtime 从不派发它们），enter 落空不再以 `timeout_ticks` 豁免（Runtime 没有 phase 计时，此前这类 tap 技能每次施法 `ErrProgramInvariant`），非零 `timeout_ticks` 给 warning；`cooldown_ticks`、phase `timeout_ticks`、wait、repeat `interval_ticks`、chain `hop_interval_ticks`、add_status `duration_ticks` 为负时按字段报错。带这些形状的定义在游戏服启动期 CompileAll 失败（错误带 JSON 路径）；删除了从未执行 recast 分支的 `recast_combo.json` fixture。[记录](docs/bugfix/RR-20261005-NC-150.md) · [NC-151](docs/bugfix/RR-20261005-NC-151.md) · [NC-152](docs/bugfix/RR-20261005-NC-152.md)
- **VisualPlanCache 的共享加载不再把第一个调用者的取消传给其他等待者**（RR-20261005-NC-153）：创建者取消后，ctx 仍有效的等待者重新加载（plan 层与资产层），之前它们也拿到 `context.Canceled`。[记录](docs/bugfix/RR-20261005-NC-153.md)
- **skillcompose.ValidateCandidate 对空 / 重复 source 给出 `PROVENANCE_MISMATCH` 诊断**（RR-20261005-NC-154）：之前报告 `Valid=false` 却没有诊断。[记录](docs/bugfix/RR-20261005-NC-154.md)
- **Remote 快照共享 L2 的旧写与删除水位对所有节点生效**（RR-20261005-NC-130、RR-20260913-01 残余补修，行为收紧，L2 键格式增加字段）：`remoteSnapshotL2Store.Set` 在 CAS 落败（L2 已有更新版本 / 更新 epoch）时返回 `cache.ErrStaleWrite`（之前是 nil），`RemoteSnapshotCache.Publish` 不再把这份旧快照装进 L1，改取 L2 的较新值——L1 冷的节点收到迟到复制消息或权威加载输给新提交时不再读到比 L2 旧的版本。版本化删除在 L2 留下只含 `deleted_version`、与 `snapshot_l2_ttl` 同 TTL 的墓碑，任何节点写入不新于它的快照都被拒绝，更新的写（重建）清墓碑；之前另一节点的在途加载或迟到消息会把已删除实体写回 L2，冷节点读到它直到 L2 TTL。滚动升级期间旧节点仍可能写回（修复前行为），新节点的下一次删除会收敛。[NC-130](docs/bugfix/RR-20261005-NC-130.md) · [残余](docs/bugfix/RR-20260913-01.md)
- **Remote Mod 健康检查不再被过期兴趣钉在 Fail**（RR-20261005-NC-131）：本机兴趣表满时 `Manager.Stats` 先清理过期条目再计数；之前一阵读取把 `snapshot_interest_keys` 读满后，空闲进程的健康一直报 `capacity exhausted`。[记录](docs/bugfix/RR-20261005-NC-131.md)

## [v1.20.1] - 2026-10-05

> 补丁版本：v1.20.0 整体验证与后续 review 收敛。saga 原生步骤按操作实例最多生效一次（U-0280，含复审两处补修）、过期无回执命令不再无限 nak（U-0281）、Nest 暂时性冲突重排加抖动（U-0279）；驱动层 Redis 脚本不再被驱动重放（NC-100，P1）、Mongo 事务窗口覆盖提交与 EndSession（NC-101）；非核心 review NC-50～52、NC-60～65、NC-70～75、NC-80～83、NC-90～93、NC-100～102、NC-110～117、NC-120～123、NC-140～147 与 RR-20261005-01。**行为变化**：saga 步骤预算改由配置提供（`saga.step_defaults` / `saga.steps`）、原生步骤租约封顶到命令截止；Redis 脚本回复丢失返回结果未知；限流器每 owner 默认 256 key；生成器 Core 下限升到 v1.20.1。已生成工程不提供迁移（维护者决定）。

### Fixed

- **World 定时器堆随 Nest 事务回滚；timer Tick 期间的取消 / 改期 / 重入按承诺生效**（RR-20261005-NC-140～147）：game-demo `TimerComponent` 在武装和到期 Tick 前向当前可回滚事务登记逆操作，事务失败或提交被拒时堆与 DAO 一起恢复（此前撤回的武装留在内存、撤回的触发从堆里消失，截止时间丢到重启）；已过期的截止时间武装为下一次 Tick 触发，不再报告已武装却没有节点。`timer.Scheduler` 在 Tick 期间对仍在堆里的定时器取消 / 改期立即生效（此前它仍按旧期限触发一次），重入 Tick 只由最外层收尾。另修 `spatial.BlockIndex.BlockRect` 在 int64 上界的溢出，`index` 在 NaN 值、混合动态类型接口键、零值 `OrderedIndex` 上的 panic / 丢写。模板改动需 `roost project sync`。见 [N11 记录](docs/review/REVIEW-2026-10-05-n11.md)。
- **tablegen 的 `ref:"<table>"` 由生成的 loader 在每次加载 / reload 时检查**（RR-20261005-NC-75，行为收紧）：此前 ref 只被写进 CSV 规则行、在哪里都不检查，悬空引用的配置能加载上线。现在非零值必须是目标表主键，目标须是同一 schema 的表、字段类型等于其主键类型，否则 `roost generate` 失败；`tablegen -json <dir> -check` 也按 schema 的 required / unique / min 校验 JSON（此前只验语法），直接改 `configs/data` 后 reload 前可用它把关。运行时仍不检查 required 列是否出现（待决定）。[记录](docs/bugfix/RR-20261005-NC-75.md)
- **`make dev-run` 运行期间 `roost generate` / `project sync` 不再报 inputs changed**（RR-20261005-NC-74）：`.dev/`（dev-run 日志）与默认 WAL 目录 `data/wal` 不再算应用输入、不再复制进暂存树；复制、输入快照与提交计划共用同一条工程边界。[记录](docs/bugfix/RR-20261005-NC-74.md)
- **ai Controller 冻结期间不再丢弃结束通知**（RR-20261005-NC-120）：`Freeze` 只暂停 Tick；动作 / 任务结束照常交给策略。此前通知被丢，BehaviorStrategy 里等该动作的 TaskflowAction 叶子在 Recover 后永远 Running。[记录](docs/bugfix/RR-20261005-NC-120.md)
- **actionflow 丢弃排队动作时发 OnEnded，启动失败的 Cancel 重入不再留下孤儿**（RR-20261005-NC-121 / NC-122）：`ClearQueue` / `EndAll` / `ClearMission` 为每个被丢弃的排队项发一次取消状态的 OnEnded（不调 Cancel、不发切换）；启动失败后 Cancel 里重入装上的动作留在当前位，外层返回 `ErrReentrantMutation`。[记录](docs/bugfix/RR-20261005-NC-121.md) · [NC-122](docs/bugfix/RR-20261005-NC-122.md)
- **hotcode 补丁点状态整体发布**（RR-20261005-NC-123）：当前函数、Meta、代数合成一个不可变状态原子替换，写者按点串行。此前并发 Replace / Revert 可永久留下 Patched 与 Meta 互相矛盾的点，`hotcode.list` 误报。[记录](docs/bugfix/RR-20261005-NC-123.md)
- **skillsync presentation reset 按 observer 可见性过滤**（RR-20261005-NC-114）：presentation 游标过期的 Flush 与 Recover 生成的 reset 现在把每条持续表现交给 observer 的 `VisibilityPolicy.FilterPresentation`（复用现有方法，自定义策略无需改动）；此前 reset 原样投影 `Runtime.PresentationSnapshot()`，不可见施法者的持续表现、目标与坐标发给所有 observer。[记录](docs/bugfix/RR-20261005-NC-114.md)
- **skillsync state 快照与增量的可见性一致**（RR-20261005-NC-115）：ability 快照与增量一样按具体 handle 问 `FieldVisible`；cast / process remove mutation 带上归属实体（追加 `caster` / `owner` 字段），persistent remove 按 `Binding` 判断，不可见实体的 remove 不再下发。行为收紧：observer 可见性在 upsert 与 remove 之间变化时，以 remove 时的可见性为准，业务应在可见性变化时重发快照。[记录](docs/bugfix/RR-20261005-NC-114.md)
- **skillsync Applier 不再被一个畸形 full 包卡死**（RR-20261005-NC-116）：开新 epoch 的状态只在准入成功时置位；此前 BaseSequence 非零的 full 包被拒绝后，Applier 对之后每个包（包括合法的恢复 full）都返回 `ErrApplyInProgress`。[记录](docs/bugfix/RR-20261005-NC-116.md)
- **skill 提交前失败的 cast 按 CompletedCastLimit 回收**（RR-20261005-NC-117）：commit 时付费不足、Cancel / Release 回调失败等提交前失败的 cast 与其他终态 cast 一样进入完成队列；此前它们永不回收，累计超过上限（默认 2048）后 checkpoint 恢复判 corrupt。[记录](docs/bugfix/RR-20261005-NC-117.md)
- **Redis 驱动不再自动重放回复丢失的 Lua 脚本**（RR-20261005-NC-100）：`MaxRetries`（缺省 3）此前在 EOF / 读超时后把 EVAL 换连接重发，脚本可能执行两次；versionstore 的 compare-and-set 第二次执行看到自己的写，`Update` 于是把 mutate 再叠一次并返回成功（真实 Redis 上一次 Update 写入两条相同消息）。`redis/driver` 的 `Eval` / `EvalSha` / `EvalBatchDurable` 改为不可重放命令，回复丢失时返回传输错误；普通读写命令的重试不变。依赖驱动“救回”脚本调用的代码现在会看到该错误。[记录](docs/bugfix/RR-20261005-NC-100.md)
- **`mongo.transaction_timeout` 现在也约束事务提交**（RR-20261005-NC-101）：此前驱动便捷 API 用 background ctx 提交，网络在回调之后黑洞时事务阻塞到网络恢复（3s 窗口实测 40s / 150s）。`mongo/driver` 的 session 以相同重试规则自实现循环，提交使用带截止的 ctx；到时返回的错误可能已提交，按结果未知处理。复审补修：提交因截止失败后，调用方 `EndSession` 里驱动补发的 abort 也有 5s 上限，此前它用调用方的 ctx（投影器无截止）在网络黑洞时同样阻塞到恢复；窗口在 TransientTransactionError 重跑的退避里到期时，返回的错误保留最后一次回调 / 提交错误的链与标签（与驱动便捷 API 相同），此前只有 `context.DeadlineExceeded`。[记录](docs/bugfix/RR-20261005-NC-101.md)
- **mongotest 唯一索引路径上遇到数组时返回 `ErrUnsupported`**（RR-20261005-NC-102）：此前把数组当一个值比较，放过真实 Mongo 会拒绝的重复、又误报真实 Mongo 接受的写入。[记录](docs/bugfix/RR-20261005-NC-102.md)
- **roost 被 Ctrl-C / SIGTERM / SIGHUP 打断 go 命令时不再把暂存树留在工程旁**（RR-20261005-NC-70）：`project deps / sync / upgrade / new` 与 `generate` 在 `.roost-deps-*` / `.roost-generate-*` 里跑 `go get` / `go mod tidy` 时被中断，roost 照旧杀掉 go 进程树并死于该信号，但此前进程死于信号不跑 defer，整份工程副本留在父目录。现在 `runCommandTree` 在重发信号前删掉本次命令所在、已登记的那一棵暂存树。[记录](docs/bugfix/RR-20261005-NC-70.md)
- **`roost project diff` 与 `project upgrade --dry-run` 列出 sync 将刷新的应用自有配置**（RR-20261005-NC-71）：预览先对 `config.<svc>.yaml`、`config.<svc>.prod.example.yaml`、`secret.<svc>.example.yaml` 的副本做与 sync 相同的 shutdown 块刷新，再按刷新后的值渲染模板；此前 Mod 变化或旧工程升级时这三份文件不出现在预览里。生成工程文档与 `roost help project` 同步写明 sync / upgrade 会刷新未手改的生成 shutdown 块。[记录](docs/bugfix/RR-20261005-NC-71.md)
- **`roost id next / check` 与 `roost add errcode` 按生成器的口径识别错误码**（RR-20261005-NC-73，RR-20261005-NC-63 残余）：此前用正则，别名导入的 `ec.Define(…)` 不算占用（分配到已占用的码，直到 `roost generate` 才报 duplicate），注释 / 字符串里的文字却算占用（`roost id check` 误报重复）。现在复用 errcode 生成器导出的 AST 扫描 `ScanDefinitions`；与生成器一致，`_test.go` 里的定义不再计入，非字面量 `Define` 会让 ID 工具报同一条错误。[记录](docs/bugfix/RR-20261005-NC-73.md)
- **`roost help cfggen` 改用 `-out ./configs/cfg -pkg cfg`**（RR-20261005-NC-72）：旧帮助指向 tablegen 的输出目录 `configs/generated`，默认工程里照做会重复声明 `RegisterConfigData`，`roost generate` 报 marked twice。[记录](docs/bugfix/RR-20261005-NC-72.md)
- **Bus 停止排空在途的 JetStream RPC handler，回包不随停止取消**（RR-20261005-NC-90）：JetStream handler 在 nats.go consume 回调里执行，之前停止只排空 pool、Stop 立即返回 nil、NatsMod 随即关连接，回包用随停止取消的 ctx 发布——每次停机在途的可靠 RPC 都丢回包并在 AckWait 后重投再执行。现在停止先关 JetStream 请求准入、再等在途 handler（超预算返回 ctx 错误并保留，重试继续等），回包预算为调用方期限；因停止被取消而中断的 handler 交还 broker 重投。[记录](docs/bugfix/RR-20261005-NC-90.md)
- **派发池拒绝的轻量 RPC 立即收到失败回包**（RR-20261005-NC-91）：队列满 / 派发器未运行时之前不回包、调用方等满超时（结果未知），开可靠总线时还把 RPC 请求写进死信。现在回失败 envelope（`remote.1`），死信只收异步消息。[记录](docs/bugfix/RR-20261005-NC-91.md)
- **被 JetStream 请求流截获的轻量 RPC 不再执行，调用方得到 `bus.ErrRPCCapturedByJetStream`**（RR-20261005-NC-92）：目标服务在 JetStream 模式时，轻量 `Call` / `CallTo` 落进 `<prefix>.rpc.>` 请求流，调用方报 `unsupported rpc response version 0`，请求却被无期限执行。现在服务端拒绝没有 reply subject 的 JetStream RPC 请求，调用端识别 PubAck。两端 `nats.rpc.transport` 须一致。[记录](docs/bugfix/RR-20261005-NC-92.md)
- **选主 `Resign(ctx)` 按调用方期限返回**（RR-20261005-NC-93）：之前 SDK `Session.Close` 用 session TTL（默认 60s）发 Revoke，etcd 无响应时 Resign 阻塞到 TTL。现在 lease 撤销由 election 持有、自带 5s 截止，期限先到返回 ctx 错误。[记录](docs/bugfix/RR-20261005-NC-93.md)
- **game-demo 玩家加载时按穿戴重建 Gear 层并写 `attr_final`**（RR-20261005-NC-65）：之前 `OnInitFinish` 装一个空 Gear 层且不写 `attr_final`，穿着装备的玩家重启或冷卸载后重新加载，Final 掉回 Base，复制给客户端的 `attr_final` 为空，直到下一次换装 / 升级。现在加载与新建都按 `EquipmentComp` 的穿戴求 Gear、重组后写 `attr_final`（nopersist，只标同步脏）。不改存储格式；已有工程 `roost project sync` 取模板。[记录](docs/bugfix/RR-20261005-NC-65.md)
- **game-demo 开关热更说明改成 reload 实际读取的文件**（RR-20261005-NC-64）：`gm.config.reload` / `gm.flag.set` 的说明与 flags 注释原来让运维改 `configs/table/featureflag.csv`（文件名也不对）再 reload；reload 只读 `configs/data` 下的 JSON，照做会得到 `ok` 而开关不变。现在写明改 `configs/data/feature_flag.json`，或改 CSV 后先 `roost generate`。[记录](docs/bugfix/RR-20261005-NC-64.md)
- **原生 saga 步骤同一操作最多生效一次，kill -9 / 投影积压后不再以新尝试再执行一次，放弃后迟到的成功不再静默**（U-0280，维护者 10-05 决定按推荐 A + B + C'）：协调器按超时发出的新尝试（新 `CommandID`、同一 `IdempotencyKey`）以前看不到旧尝试的 claim / 回执，已写进 WAL 的旧尝试又能在 2 分钟 claim 租约内重放生效，于是重复扣款 / 重复退款，或已扣款却 Failed（drill6）。现在 `DataEngineStepInbox.Reserve` 以操作实例为单位：同一操作已有成功（任何一生）或本生的拒绝就不执行、把那次的 completion 经 saga 结果流重发给协调器；旧尝试仍持有效租约时返回可重试错误；租约过期时接替它（`status=superseded`、`lease_token+1`），它未投影的记录随后被 fence 跳过；同一操作的并发 Reserve 由守卫文档串行化。claim 租约封顶到命令截止（见 Changed），截止后重放的记录被跳过、不留回执。协调器在重试用尽 / saga 截止（含退避中）/ 人工 `Compensate` 时把操作记为“放弃关闭”，之后才到的成功记 ERROR、`Stats().LateAfterAbandon` 与 `saga.completion.late_after_abandon_total{saga_type,phase}`，不重开终态、不自动补偿；带结果关闭的旧结果仍按重复处理，U-0280 之前写的 tombstone 不告警。另修：原生 handler 返回零值 `Completion`（生成模板的写法）不再让每次成功执行多一次 nak 与重投。契约见 [SAGA.md「原生步骤执行契约」](SAGA.md)，[记录](docs/bugfix/U-0280-saga-step-reexecuted-after-crash.md)、T-226。
- **skill 施法失败只走一个终态入口**（RR-20261005-NC-110 / NC-111 / NC-112）：启动失败的 cast 被删除、ID 复用前先撤掉它的全部排程任务（此前旧任务会落到下一个拿到同一 ID 的 cast 上，失败启动后的 checkpoint 也恢复不了）；Cancel / Interrupt / Release 在改动 cast 后出错时 cast 进入 failed 终态（此前停在半终止，施法者永久 `ErrCasterBusy`）；排程失败的 toggle / hold / charge 释放 policy 槽位，对 failed cast 的 Cancel / Interrupt / Release 返回 `ErrCastInputRejected`（此前下一次激活会对失败 cast 执行 toggle-off）。行为收紧：手动 Release 付费失败后不能再重试，与 auto release 一致。[记录](docs/bugfix/RR-20261005-NC-110.md)
- **combatcomponent Combatant 副本不再共享 map**（RR-20261005-NC-113）：`Combatant()` 返回、`InitCombatant` 存入的 `ElementMultipliersBP` 都是拷贝；此前改副本或改共用的配置模板会在事务、逆操作与脏标记之外改掉权威战斗状态。[记录](docs/bugfix/RR-20261005-NC-113.md)
- **versionstore RedisStore 输掉 compare-and-set 后退避再重读**（RR-20261005-NC-52）：此前退避后仍用退避前 CompareAndSet 带回的值重试，竞争写落在退避窗口里时每次重试必输，同键并发（如 chat 世界频道）出现伪 `ErrConflict`。现在只在输掉 CAS 后多一次 GET，尝试次数与退避策略不变。[记录](docs/bugfix/RR-20261005-NC-52.md)
- **activity sweep 对已确认窗口键先验证合法与归属**（RR-20261005-NC-51）：与 Opening 共用 `windowKeyProblem`；坏条目跳过、保留给运维、计 `sweep.window_key_malformed`，不再结算别的组的活动或把跨组键并入本组 Delivering。[记录](docs/bugfix/RR-20261005-NC-51.md)
- **account 换名建角也释放名字已被他人提交的死计划**（RR-20261001-06 残余）：此前只有同名重试会释放，玩家直接换名永远得到 `ErrRoleLimit`；补偿失败重新计 `rollback.failed`（RR-20261005-NC-50）。[记录](docs/bugfix/RR-20261001-06.md#复核后的补修2026-10-05)
- **game-demo activity 启动时拒绝注定开不出窗口的 `activity.game_sids`**（RR-20261005-01）：列表里重复的 sid（两个都活时协调器拒绝带重复项的 expected 集合）、超出 int32 的值（会被截成另一个服的 sid）、本服加配置超过 `app.SingletonLiveMaxSIDs`（每次 `Live` 都报错）三种情形，修复前进程照常启动、此后每个窗口都开不出来、只有 Warn；现在 `startActivity` 在任何远端调用之前按键名报错，`Service.Init` 失败。本服 sid 与非正数照旧跳过；生成值 `[1000]` 不受影响。顺带更正 kit `global.Service.Bind` 注释里不存在的 `Rebind`。[记录](docs/bugfix/RR-20261005-01.md)
- **HTTP JSON 响应先编码后写状态**（RR-20261005-NC-80）：`httpserver.JSON` 编码失败（NaN/Inf、不可编码类型、MarshalJSON 错误）时回 500 固定错误体并记日志，不再发出 2xx 空体；生成 Webroute 与 Ops `/admin/execute` 的调用方不再把已执行但无结果的请求当成功。成功输出就是 `json.Encoder` 的原字节，且不为每个响应另复制一份响应体（状态码推迟到 Encoder 唯一一次 Write）。[记录](docs/bugfix/RR-20261005-NC-80.md)
- **Engine recover 尊重已开始的响应**（RR-20261005-NC-81）：响应开始（写头/写体/Flush/Hijack）后 panic 以 `http.ErrAbortHandler` 中止连接，handler 自己的 `ErrAbortHandler` 原样传播；响应前 panic 仍回 500 JSON。包装 writer 保留 Flusher（含 FlushError，`http.ResponseController.Flush` 的 `ErrNotSupported` / 写失败照常返回）/ReaderFrom/Unwrap，原 writer 支持时才暴露 Hijacker。[记录](docs/bugfix/RR-20261005-NC-81.md)
- **RateLimiter 每主体 key 上限，满表不再全表扫描**（RR-20261005-NC-82，行为变化）：`security.RateLimitConfig.MaxKeysPerOwner`（默认 256，不超过 MaxKeys）——一个主体变化 Action（`gateway.RateLimit` 下即 MessageID）只耗尽自己的名额，不再让其他玩家被限流；满表时陌生 key O(1) 拒绝，闲置 key 由每 SweepInterval 至多一次的周期清扫回收（满表时最多晚一个间隔腾出名额）。`RateLimitStats` 新增 `MaxKeysPerOwner`、`OwnerCapacityRejected`。[记录](docs/bugfix/RR-20261005-NC-82.md)
- **生成的 player TCP 接入停机可重试**（RR-20261005-NC-83，生成器）：`Server.Stop`/`Mod.StopWithContext` 超时后保留 server，用新 context 重试会等到连接 goroutine 真实返回；会话关闭订阅者的等待受 ctx 约束。已生成工程需重新生成 `internal/access/player/tcp/server_gen*.go`。[记录](docs/bugfix/RR-20261005-NC-83.md)
- **attribute.Container.Snapshot 在读锁内复制**（RR-20261005-NC-60）：之前先释放读锁再 CloneProfile，并发 Apply / ClearDirty 能改写正在复制的 profile，快照撕裂、-race 报竞争。现在复制期间持读锁；CloneProfile 不得回调同一容器。[记录](docs/bugfix/RR-20261005-NC-60.md)
- **game-demo 属性层随事务回滚**（RR-20261005-NC-61）：升级 / 换装改的是组件内存里的属性层，Nest 只撤回 DAO；失败或提交被拒绝后容器保留新值，下一次成功提交把虚高的 Base 持久化。模板 AttributeComponent 改层前用 `RecordUndo` 登记层的副本。已生成工程需 `roost project sync`；已污染的存量不自动修正。[记录](docs/bugfix/RR-20261005-NC-61.md)
- **attribute 生成器拒绝无法表示的声明**（RR-20261005-NC-62）：float / bool / string 字段、`max` 超过 64、`index+max-1` 超过 AttrID 在生成期报错；此前 float 被静默截断（0.15 导出为 0），超限声明到编译期才失败。[记录](docs/bugfix/RR-20261005-NC-62.md)
- **errcode 扫描不再静默跳过**（RR-20261005-NC-63）：按导入名识别 `Define`（含别名 / 点导入），编号、名字、消息不是字面量时报 `file:line`，并检查重复 name；此前这些定义不进导出表、逃过重复检查。用常量编号的业务工程需改为字面量。[记录](docs/bugfix/RR-20261005-NC-63.md)
- **Saga outbox领取遵守并发更新的重试期限**（RR-20261005-NC-41）：原子领取复查next_attempt_at与lease，陈旧候选不能绕过另一发布者Nack退避；到期恢复及旧token围栏保持。[记录](docs/bugfix/RR-20261005-NC-41.md)
- **Activity恢复前验证持久计划**（RR-20261005-NC-42、RR-20261001-09残余）：公开Open在Create前拒绝异键/非pending/非法expected计划；sweep在访问Activities前跳过非法键/跨组opening，保留名额且正常组继续。诊断按所属窗口清理，不自动修坏存量，API/格式不变。[记录](docs/bugfix/RR-20261005-NC-42.md)
- **原生 saga 步骤的过期命令不再无限重投**（U-0281）：`SubscribeDataEngineStep` 收到已过 `DeadlineAt` 且没有回执的命令时 ack（计数 `saga.step.expired_unexecuted_total`，Info 日志），不再返回 `context.DeadlineExceeded` 按退避 nak 到 MaxDeliver（默认约 8.7 天）、长期占住共享 durable 的 MaxAckPending。协调器本来就按超时自行重试或补偿；尝试的结果经 WAL 投影与 completion effect 送达，不依赖这条消息，所以 ack 不会丢掉已提交未投影的尝试。读回执出错仍重投；不执行业务、不调用 Admit。[记录](docs/bugfix/U-0281-saga-expired-command-nak-forever.md)

- **Saga启动幂等身份独立持久化**（RR-20261005-NC-39）：`StartDigest` / `start_digest,omitempty` 保存规范化原始意图，步骤Data与Resume截止时间变化不再改变启动身份；原请求返回当前进度，异意图明确冲突。旧已推进缺摘要记录不能证明原始身份，重投收紧为冲突，不自动迁移；自定义Store/协调writer须保存新增字段。[兼容与证据](docs/bugfix/RR-20261005-NC-39.md)
- **原生Saga完成路由绑定**（RR-20261005-NC-40）：解码后精确匹配Topic与payload SagaID，在Complete和回执副作用前Permanent拒绝异键。合法发送与wire不变，复用既有NATS settle；不提供发布鉴权或自动改路由。[记录](docs/bugfix/RR-20261005-NC-40.md)

- **Nest 暂时性冲突的重新准入加抖动，对称的交叉创建不再靠调度噪声解开**（U-0279，OPEN-ITEMS C09 预案）：锁超时 / 组变化 / 组迁移待定的消息重排延迟从固定 5ms 改为 5ms 下限加 `[0, 5ms)` 均匀抖动。固定延迟让同一轮因同一冲突回滚的两条消息（handler 内交叉新建 X / Y，RR-20260926-48）总在同一时刻重新准入，单定时器的延迟队列晚醒时还会把两侧重新对齐，只能等噪声偶然错开；v1.20.0 / v1.19.2 生成工程 `TestGeneratedDataEngineCrossCreateResolvesOnRealWAL` 频繁耗尽 400 次上限（正常负载下失败率 25%～55%）、调用方收到 `ErrLockTimeout`。400 次上限与最短约 2s 的重排窗口不变，平均延迟 5ms → 7.5ms。[记录](docs/bugfix/U-0279-nest-requeue-jitter.md)
- **Saga三消费者健康检查**（RR-20261005-NC-37）：`ConsumersClosed` 纳入原生Nest完成消费者，任意必需订阅缺失/退出均使Kit健康项fail；修正过时的两消费者注释。正式装配与停止后重启恢复已验证。[记录](docs/bugfix/RR-20261005-NC-37.md)
- **Saga Resume持久代际**（RR-20261005-NC-38）：Mongo记录增补兼容字段`incarnation,omitempty`并双向保存，重载后派发不复用旧命令/回执ID；旧缺字段为0。参与写入的协调器需统一升级，旧writer完整Replace会丢新字段；不自动修复历史waiting/回执。[兼容与证据](docs/bugfix/RR-20261005-NC-38.md)
- **Ctrl-C 打断 roost 跑的 go 命令后，roost 照旧死于该信号**（RR-20261004-13 补修）：杀掉 go 的进程树后重发给自己的信号是异步生效的，满载时调用方会先跑下去（回滚、打印错误、以自己的退出码结束）；现在重发后等信号生效再说。同时修掉 doctor / 依赖命令进程树用例在满载下的偶发红（计时器与替身 exec 赛跑），改为等替身报告孙进程后再触发超时或取消。[记录](docs/bugfix/RR-20261004-13.md#补修2026-10-05满载偶发红与重发信号的竞态)

### Changed

- **步骤超时与重试次数改由配置提供**（U-0280，维护者“重试次数可以是一个配置，一次操作可以有多次尝试”）：`saga.Step` 的 `Timeout` / `MaxAttempts` / `BackoffMin` / `BackoffMax` 留空时由 `Engine.Register` 按 `Options.StepBudgets` 补齐（按步骤覆盖 > 定义 > 配置默认 > 框架默认 5s / 5 次 / 100ms..5s）；kit saga Mod 读 `saga.step_defaults.*` 与 `saga.steps.<type>.<step>.*`，写错类型 / 步骤 / 字段时 `Init` 失败；`kitsaga.StepBudgetsFromConfig` 供生成工程测试使用。codegen `add saga` 生成的定义不再写预算，saga Mod 的生成配置带 `step_defaults` 与 `steps: {}`；game-demo 的 debit 预算从“生成后改 definition.go 为 MaxAttempts 15”（`ac5acfbe`）改为 game 服务三份配置里的 `saga.steps.gift_item.debit.max_attempts: 15`，`gift_saga_budget_test.go` 读配置核对。已生成工程不迁移：旧 definition.go 里写死的值照旧生效，配置的按步骤覆盖优先于它。
- **原生步骤 claim 租约封顶到命令截止时间**（U-0280 B）：`lease_until = min(now + LeaseDuration, Command.DeadlineAt)`，新建与接管都一样，`LeaseDuration` 只是上限；已过截止的命令 `Reserve` 返回 `saga.ErrCommandExpired`。代价：Mongo 投影积压超过步骤 `Timeout` 时步骤停住（每次尝试都在截止后才投影、被跳过），积压消退后才成功，而不是重复执行；依赖进程间时钟偏差远小于 `Timeout`。
- **持久格式增量**（U-0280）：`_dataengine_inbox_claims` 的 claim 多 `operation_key` / `incarnation` / `superseded_by` 字段、`superseded` 状态与新索引 `by_operation`，并存放操作守卫文档（`namespace=saga-step-op`）；`_saga_operations` tombstone 多 `closure`（`result` / `abandoned`）。新增可选接口 `saga.CompletionHistoryStore`（`MongoStore` 实现），`Store` 接口不变。契约只在全部步骤进程与协调器升级后成立：旧进程的 claim 不参与跨尝试判断、租约不封顶，旧 tombstone 不告警。

## [v1.20.0] - 2026-10-05

> 功能版本：App 层同一服务类型 + sid 单实例锁（`app.Singleton`、`RuntimeFailure.OnFail`、`SingletonLiveness.Live`），只覆盖崩溃重启短暂并存，失锁由 App 统一 fail-stop（[方案](docs/feature/APP-SINGLETON-LOCK-2026-10-05.md)）；game-demo 玩家所有权改为按角色 `server_id` 静态绑定（删除按玩家 Redis 租约 `playerroute`），activity 改用 App 的 `Live`，赠礼按发送方 sid 路由。**破坏性变更**：删除 kit `service/global` 的租约 API（错误码 570105～570109 退役）、停止发布 `ModEtcdElection` / `ModRedisLock` capability；RemoteEntity fatal 现在也围栏 Nest；生成器 Core 下限升到 v1.20.0。已生成工程不提供迁移（维护者决定）。另含 RR-20261004-10～14、DAO `//roost:dao nocoll` 等自 v1.19.2 以来的修复。

### Added

- **App 单实例锁：同一服务类型 + sid 只跑一个进程**（[方案](docs/feature/APP-SINGLETON-LOCK-2026-10-05.md) 第 1 笔，维护者 10-05 决定 D-A / D-B）：`singleton.enabled=true` 时 App 在 `NewRegistry` 之后、第一个 Mod `Init` 之前用后端单键 CAS 获取 `<singleton.key_prefix>:<server_type>:<sid>`（值 `token|hostname|pid|started_unix_ms`）；键被别人持有就等待（每 `renew_interval` 重试，最多 `startup_wait`，到上限返回 `app.ErrSingletonHeld`，最后一次是报错则 `app.ErrSingletonStoreUnavailable`，不抢锁），丢回复后键是自己的值则认领。持有期间一个 goroutine 按固定节拍续期，窗口从请求发出时刻起算，迟到的 Applied 不作数；续期答“不是我的”或续期失败已到 `validUntil − guard` 即 `RuntimeFailure.Fail(app.ErrSingletonLost …)` fail-stop（Redis 连续不可用约 10s 以上进程会退出重启）。全部 Mod 停完才释放（Mod 停机截止时间为此提前至多 3s）；`Service.Shutdown` 超时、Mod 停机不完整、失锁三条路径不释放、键在 TTL 内过期；进入停机时已失锁则不为 Release 预留时间，停机不完整时后端连接留到进程退出（仍在跑的组件调 `Live` 不报 client closed）。配置 `singleton.{enabled,key_prefix,ttl,renew_interval,guard,startup_wait}`（默认 15s / 3s / 5s / 2×ttl），`ValidateServiceConfig` 钉住 `renew_interval ≤ guard`、`2×renew_interval ≤ ttl − guard`、`startup_wait ≥ ttl + 2×renew_interval`；启用而 bootstrap 没调 `App.Singleton` 时启动失败（`app.ErrSingletonOpenerMissing`）。后端 `app.SingletonStore` / `app.SingletonOpener`，Redis 实现 `kitredis.SingletonStore`（从同一份 `redis.*` 建两个独立小客户端，CAS 与 `Live` 各一，Redis 变慢时并发的 `Live` 占满连接也不拖住续期；`Close` 幂等；缺 `redis.addr` / `redis.cluster_addrs` 报错，不用 localhost 兜底）。`/readyz` 多一项 `singleton` 健康检查。codegen 生成装配与配置在第 2 笔，本版需要手工在 bootstrap 调用 `Singleton(kitredis.SingletonStore)` 并写配置。[使用说明](docs/USER_GUIDE.md#单实例锁singleton)、T-212 / T-213
- **`RuntimeFailure.OnFail(hook)`**：登记首次失败时调用的回调（恰好一次、按登记顺序、在调用 `Fail` 的 goroutine 上同步执行，全部执行完才投递 `Done`；失败后登记立即调用；回调 panic 被 recover 并并入 `Err`，回调里再调 `Fail` 不死锁）。`run` 现在也在启动各阶段之间检查 `RuntimeFailure.Err()`：启动期间发生的 fail-stop 不再继续启动后面的 Mod，而是按启动失败路径停掉已启动的 Mod。
- **单实例锁的只读活性查询 `app.SingletonLiveness.Live`**：能力名 `app.ModSingleton`（kit 别名 `mods.ModSingleton`），`Live(ctx, serverType, sids)` 按 `<key_prefix>:<serverType>:<sid>` 逐键读、值非空即活，按入参顺序返回，一次最多 `app.SingletonLiveMaxSIDs`（200）个；Redis 实现逐键 GET（pipeline），Redis Cluster 下跨槽不报 `CROSSSLOT`。`singleton.enabled=false` 时不登记。
- **codegen：生成 App 单实例锁的装配与配置**（[方案](docs/feature/APP-SINGLETON-LOCK-2026-10-05.md) 第 2 笔，§6.3）：项目里有 `redis` Mod 或带 `dataengine` 的服务时，bootstrap 生成 `a.Singleton(kitredis.SingletonStore)`；带 `dataengine` 的服务配置默认 `singleton.enabled: true`（`key_prefix: roost:<project>:singleton`、15s / 3s / 5s / 30s，缺 `redis:` 段时只补配置段、开发 compose 带上 redis），其他服务写同一段的 `enabled: false`（D-B）；`add mod` 之后才带上 dataengine 的服务把未改过的 `enabled: false` 段翻成 `true`。启用的服务 `shutdown.total_timeout` 计入 Release 的 3s（game-demo game 服务 `-mods configdata,mongo,nats,dataengine,nest` 时 108s / 113s → 111s / 116s，缺省 Mod 集 114s / 119s → 117s / 122s），`roost project doctor` 对 `singleton.enabled: true` 的配置同样计入；k8s `startupProbe` 注明覆盖 `startup_wait + 30s`（默认仍 60s），shell 部署 `HEALTH_ATTEMPTS` 改为按 Service 默认（启用的服务 60 次、其他 30 次），compose `start_period` 启用的服务 60s。已生成工程：配置归应用所有，不自动加 `singleton` 段；未改过的 `shutdown:` 段由 `roost project sync` 随新公式刷新。
- **codegen：`//roost:dao nocoll` 声明无集合的内存 DAO**（W-2026-09-18-09，维护者 10-04 选 A，[方案](docs/feature/DAO-NO-COLLECTION-2026-10-04.md)）：全部字段须 `nopersist`；生成物保留读写、undo、回滚快照与 `MarshalSync` / `ApplySync`，不生成集合 / 库名常量和任何 Mongo 读写、迁移、加载路径，以 `<Dao>RegistryKey` 在 DaoManager 登记。与 `coll=` / `db=` 同时出现、`nocoll=<值>`、含持久字段，以及持久实体或 `remote=managed` 实体使用它时，都在生成期报错并点名。game-demo 的 `MonsterDao` 已改用该声明，不再编造 `monsters` 集合；已生成工程把 marker 改成 `nocoll` 后 `roost generate` 即可原地迁移。

### Changed

- **Remote Entity fatal 现在也会围栏 Nest**（行为变化，App 单实例锁方案 §5）：kit Nest Mod 在 `Provide` 里把 `NestMgr.Fence` 登记进 `RuntimeFailure.OnFail`，任何 fail-stop（单实例锁丢失、DataEngine fatal、Remote Entity 释放失败 fatal）都在唤醒停机之前立即拒绝新的和排队中的 Nest 派发（`nest.ErrNestFenced`）。此前 Remote Entity fatal 只调 `Fail`、Nest 在优雅停机开始前仍接受派发。DataEngine `onFatal` 里显式的 `Fence` 保持不变（幂等）。
- **game-demo 玩家所有权改为静态绑定**（行为变化，[静态绑定方案](docs/feature/PLAYEROWNER-STATIC-BINDING-2026-10-05.md) / [App 单实例锁方案](docs/feature/APP-SINGLETON-LOCK-2026-10-05.md) 第 3 笔）：玩家建角时由 account 绑定到一个 game sid（`Role.ServerID`）、之后不迁移，同一 sid 只有一个进程由 App 单实例锁保证，所以 `PlayerOwners` 不再维护按玩家的 Redis 租约，只剩一张本地驻留表：登录 `Serve(ctx, playerID, boundSID)` 用认证器写进 `Principal.Claims["server_id"]` 的角色 sid 判定（不是本服 → `player_elsewhere`；Claims 里没有 `server_id` → fail-closed，同样拒绝、`owner_sid=0`，记 Error 日志）；后台准入 `AdmitBound`、只读 `Resident`、WriteGate `Admit` 只看驻留记录与卸载状态（拒绝错误改名 `ErrNotServedHere`）；闲置卸载（无连接且 `IdleUnload` = 5 分钟未被使用，每 30s 扫一次）只是本地内存管理，卸载进行中拒绝准入、登录等它结束（单航班，等待上限 `evictBudget`）。`Service.Shutdown` 第一步断开本进程服务中的全部玩家连接（fail-stop 时客户端重连到接替的进程）。删除：`game/playerroute` 整包、按玩家的租约 / 续租 / 重新认领 / 跨间断撤离 / 归还与投影等待、公开 `Claim` / `Owns` / `OwnedHere` / `OwnerSID` / `Routes` / `Release`，以及 game 配置里的 `game_route` 段（`singleton.key_prefix` 由第 2 笔生成）。过渡：赠礼步骤暂时只准入在本进程驻留的发送方、不转交（命令携带发送方 sid 并按它路由是第 4 笔）；matchmaker 改用 `Resident`。已生成工程不迁移（维护者决定）。
- **game-demo activity 不再持有自己的全局租约，改用 App 单实例锁的 `Live` 查询**（App 单实例锁方案 §7.2）：协调器的 expected 集合是 `app.SingletonLiveness.Live(ctx, <本进程 server_type>, activity.game_sids 加上自己)` 返回的活 sid，为空时只等自己，查询失败本拍不开窗；取不到 `app.ModSingleton`（`singleton.enabled=false`）时 activity 启动失败。删除 `incarnation`、`AcquireLease` / `RenewLease` / `ReleaseLease` 的调用与 standby / retake 逻辑（`routing.Bind` 的组绑定保留），崩溃重启时“30s activity 租约 vs 15s App 锁”的启动冲突随之消失。kit `service/global` 的租约 API 本身在第 3b 笔删除。
- **`player_elsewhere`（100015）含义收窄**：从“玩家驻留在另一个进程（等租约过期或改连持有者）”改为“玩家绑定在另一个服，不要在本服重试”：改连 `owner_sid` 对应服的网关，`owner_sid=0` 时重新 `SelectRole`、按返回的 `Session.ServerID` 连接。message 改为 `player is bound to another server; reconnect to that server`。
- **game-demo 赠礼按发送方绑定的 `server_id` 路由**（行为变化，[静态绑定方案](docs/feature/PLAYEROWNER-STATIC-BINDING-2026-10-05.md) §3.3 / [App 单实例锁方案](docs/feature/APP-SINGLETON-LOCK-2026-10-05.md) 第 4 笔，取代第 3 笔的赠礼过渡形态）：`gift.State` 增加 `FromSID`（json `from_sid`），`start_gift`（`handlerStartGift`，`//roost:nest`）多一个参数 `fromSID int32`，`send_gift` 传本进程 sid（控制器的 `playerOwners` 接口加 `SID()`），`gift.Encode` 拒绝没有 sid 的赠礼。debit / refund 的准入与转交接收方改为 `PlayerOwners.AdmitBound(From, FromSID)`：是本服就接入并执行——离线、没有副本的发送方也在其绑定 sid 上 debit / refund，不再等他重新登录或到 saga 截止；不是本服就经 bus 转交给 `FromSID` 后拒绝（nak），本进程不建驻留记录；本服但副本正在卸载则拒绝、不转交；`FromSID == 0` 视为非法载荷，拒绝并记 Error，不兜底。生产装配装上转交的发送半边（`ownerroute.Router` 按 sid 键、静态解析器 `GetRoute(sid) = (sid, sid > 0)`；core `ownerroute` 不变），转交接收方另核对信封与载荷的玩家 / sid 一致。已生成工程不迁移（维护者决定），进行中的旧赠礼（载荷无 `from_sid`）不会被执行。
- **kit/redis 的 Redis 集成套件进入 CI Redis job**：`kit/redis` 新增以 `REDIS_ADDR` 准入的 integration 用例（单实例锁 store），ci.yml 的 `service-redis` 与 `kit/scripts/integration/redis-cluster-suites.sh` 都列入 `./kit/redis`。
- **game-demo 赠礼退款的重试预算覆盖发送方 sid 的一次崩溃重启**（行为变化，App 单实例锁方案第 3 / 3b / 4 笔审查观察）：生成的 `saga/gift_item/definition.go` 里 debit 步骤（它的补偿就是退款）`MaxAttempts` 5 → 15，Timeout 5s、退避 100ms..5s 不变；判为用尽之前的重试窗口从至少约 26s 变为至少约 98s（至多约 121s），不小于 `singleton.startup_wait`（30s）+ `singleton.ttl`（15s）+ 拉起与 Init 余量 45s = 90s。此前发送方绑定的 sid 崩溃重启期间，每次退款尝试都只能超时，约 26s 后补偿用尽、直接转为 `manual_required`。`saga.Step` 的预算正反两个方向共用，debit 正向也是 15 次（扣款失败时什么都没扣，只是更晚判赠礼失败，仍受 `gift.Deadline` 约束）；deliver 不变。新增生成工程测试 `gift_saga_budget_test.go` 按生成的 singleton 配置钉住这个关系。已生成工程不迁移，需要时手工改 `definition.go`。

### Removed

- **破坏性变更：kit 不再发布 `redis.lock` 与 `etcd.election` capability**（[App 单实例锁方案](docs/feature/APP-SINGLETON-LOCK-2026-10-05.md) 第 2b 笔，§12）：`kitredis.RedisMod` 不再登记 `mods.ModRedisLock`（`redis.IDistLockFactory`），`kitetcd.EtcdMod` 不再登记 `mods.ModEtcdElection`（`etcd.IElectionFactory`），两个常量从 `kit/mods` 删除。仓库内（core、kit、codegen、demo 模板）没有使用者；“同一服务类型 + sid 只跑一个进程”由 App 单实例锁（`app.Singleton`）统一提供。core 原语保留：`redis.IDistLock` / `IDistLockFactory`、`etcd.IElection` / `IFencedElection` / `IElectionFactory` 及 `redis/driver`、`etcd/driver` 的实现不变，注释写明进程 / sid 级单例改用 App 单实例锁。迁移：仍需要键级锁 / 选主的应用用 `redisdriver.Assemble(cfg).Locks`、`etcddriver.Assemble(cfg).Election` 自行装配，进程级单例改为配置 `singleton.enabled: true` 并在 bootstrap 调用 `App.Singleton(kitredis.SingletonStore)`。

- **破坏性变更：kit `service/global` 删除游戏服租约 API**（[App 单实例锁方案](docs/feature/APP-SINGLETON-LOCK-2026-10-05.md) 第 3b 笔，§12）：`Routing` 接口与 `Service` 删除 `AcquireLease` / `RenewLease` / `ReleaseLease` / `Lease` / `LiveGames`（RPC 方法 `global.AcquireLease` 等五个随之从生成的传输层删除，`Methods` 只剩路由五个），删除类型 `GameLease` / `LeaseState`（`LeaseActive` / `LeaseReleased` / `LeaseLapsed`）、`Config.Leases` / `Config.LeaseTTL` / `Config.NewIncarnation`、`DefaultLeaseTTL`、`MaxPageSize` / `MaxLoadEntries`、`RedisStores.Leases`；错误码 570105～570108（`ErrLeaseInvalid` / `ErrLeaseMissing` / `ErrLeaseNotHolder` / `ErrLeaseExpired`）与只由 `LiveGames` 产生的 570109（`ErrRangeInvalid`）退役、不复用。`global.lease_ttl` 不再读取（旧配置里留着也不报错），codegen 生成的 `global:` 段不再写它。路由 `Bind` / `Resolve` / `BeginMigration` / `CompleteMigration` / `AbortMigration` 与错误码 570101～570104、570110、570125 不变。仓库内最后一个调用方（game-demo activity）已在第 3 笔改用 App 的 `Live`。迁移：进程存活改用 App 单实例锁——被查的服务配置 `singleton.enabled: true`（bootstrap 调 `App.Singleton(kitredis.SingletonStore)`），查询方 `app.Lookup[app.SingletonLiveness](registry, app.ModSingleton)` 后调 `Live(ctx, serverType, sids)`，返回持有锁的 sid；原 `AcquireLease` + 心跳 + `ReleaseLease` 的生命周期由 App 锁的获取 / 续期 / 释放代替，不需要应用代码。`Live` 不按 global 组与路由世代过滤、不带负载快照，需要时由应用自己维护。旧部署 Redis 里 `<global.key_prefix>:lease:*` 的键不再被读写，可以删除。

### Fixed


- **codegen：生成的 `etcd.service_prefix` 带结尾 `/`**（2026-10-05 App 单实例锁第 5 笔真实进程演练发现）：etcd Discovery 的键是 `service_prefix + server_type + "/" + sid`，自己不补分隔符（core 缺省 `/service/`），生成值 `/roost/services` 让 game 1300 注册成 `/roost/servicesgame/1300`。新工程改为 `/roost/services/`；已生成工程的配置归应用所有，不自动改写——需要时手工补上 `/`（同一部署的所有进程一起改，注册与查询用同一个前缀，混跑期间互相看不见）。回归 `TestGeneratedEtcdServicePrefixSeparatesTheServerType`。
- **权威快照加载在写缓存前绑定完整请求键**（RR-20261005-NC-35，P2）：异键结果明确拒绝，不写其他视图，合法加载可恢复；API/wire不变。[记录](docs/bugfix/RR-20261005-NC-35.md)
- **权威读取重新检查最终L1最低版本**（RR-20261005-NC-36，P2）：旧epoch结果被缓存准入拒绝后，实际L1版本不足返回ErrRemoteSnapshotStale；保留epoch防护、不新增重试。[记录](docs/bugfix/RR-20261005-NC-36.md)
- **快照有效期残余补修**（RR-20260913-08）：L2发布期间跨过ExpiresAt时，以返回前当前时间判过期并返回miss，避免用发布前时间服务过期值；不新编号。[记录](docs/bugfix/RR-20260913-08.md)

- **etcd Discovery：租约已过期时停机注销不再算失败**（2026-10-05 App 单实例锁第 5 笔真实进程演练偏差 3）：进程暂停超过 `etcd.lease_ttl` 后恢复、重注册尚未成功就停机时，`Deregister` 对已过期的租约 Revoke 得到 `etcdserver: requested lease not found`，此前记为 `mod etcd stop` 失败并入 `run` 的返回值，正常停机（SIGTERM）的退出码因此非零。现在“租约已不存在”（`rpctypes.ErrLeaseNotFound`，或未转换 / 被包裹的 gRPC NotFound 同描述）视为注销已达成（键已随租约删除），记 Info；其他 Revoke 错误照旧报告并保留登记以便重试。同时 `Deregister` 在注册循环退出后再取消一次 keepalive：停机期间才完成的重注册不再留下一个续期中的 keepalive。核实：这个错误不影响单实例锁——Mod 停机返回普通错误算已停完，锁照常 Release，下一个进程不用多等一个 TTL（只有超时 / 取消算停机不完整）。kit `EtcdMod` 停机的错误只来自 `Assembly.Close`，无需另改。回归 `TestDiscoveryDeregisterTreatsLeaseNotFoundAsDeregistered`、`TestAssemblyCloseTreatsLeaseNotFoundAsDeregistered`、`TestDiscoveryDeregisterStopsRegistrationThatCompletedDuringShutdown`、`TestSingletonIsReleasedWhenAModStopReturnsAnOrdinaryError`，真机 `TestRealEtcdCloseAfterLeaseVanishedIsClean`（`-tags integration`）。

- **game-demo 赠礼转交接收方核对信封的 phase 与命令的 topic**（第 4 笔审查，`c493a791`）：接收方此前原样信任信封的 Phase，而它决定执行哪笔事务：debit 信封包着 refund 的命令时，会用 refund 命令的预留执行一次 debit（再扣一次物品，并在 refund 的回执下写入，协调器把它当作“已退还”）。现在 `runHandoff` 在信封玩家 / sid 核对之后、`AdmitBound` 与认领之前要求命令的 Topic 等于该 phase 的消费者订阅的 topic（debit → `TopicDebit`，refund → `TopicDebitCompensation`），未知 phase 的拒绝也前移到这里（不再先建驻留记录）。正常转交不受影响。已生成工程须手工合并 `gift_saga.go`。

- **缓存副本写入前绑定业务key/version**（RR-20261005-NC-33，P2）：配置的提取器与信封不一致时明确拒绝、Store不变；含身份null更新在回调前拒绝。无VersionOf与普通Delete兼容保持，不增加版本墓碑。[记录](docs/bugfix/RR-20261005-NC-33.md)

- **Remote interest写注册表前绑定完整消息身份**（RR-20261005-NC-34，P2）：校验完整snapshot key/SID哈希、ExpiresAt及Upsert操作，避免信封A操作订阅B。合法Generation=0和旧release代际保护保持；不增加发布权限认证。[记录](docs/bugfix/RR-20261005-NC-34.md)

- **生成 DAO 恢复后深层嵌套修改进入持久提交**（RR-20261005-NC-32，P2）：wire 转换先恢复未绑定数据，父对象到最终位置后再递归接线，避免子通知指向按值返回前的临时副本。唯一父归属保护和BSON/版本格式保持；应用须重生成关联DAO/nested代码，历史漏写不自动补回。[记录](docs/bugfix/RR-20261005-NC-32.md)

- **迁移输出在WAL准入前验证目标装载与身份**（RR-20261004-NC-31，P2）：复用 Mongo BSON/ID 和目标 RestorePersisted；坏 BSON、字段类型或身份不再先持久提交。手写候选须提供 loader / Id，预校验使用目标 schema、旧 version；正常 CAS/投影等待/整聚合重读与 int32 ID 兼容保持。已有坏 WAL 不自动跳过或删除。[记录](docs/bugfix/RR-20261004-NC-31.md)

- **game-demo 玩家租约的本地准入窗口不再越过 Redis 键**（RR-20261004-14，P3，W-2026-10-04-07）：确认改为从 SetNX / Refresh 发出的时刻起算 Lease，跨间断认领的撤离等待和慢 Refresh 不再吃掉 AdmissionGuard；跨间断认领 Redis 没答复（SetNX 可能已落地）时玩家记为 interrupted，下一轮续租答 Held 也会先重新认领、扔掉间断前的副本再放行。已生成工程须手工合并 `playerowner.go`。[记录](docs/bugfix/RR-20261004-14.md)
- **roost 替用户跑的 go 命令超时 / 取消时连同子进程一起结束**（RR-20261004-13，P3，来源 W-2026-10-04-06）：doctor 的 `go mod verify` / `go list` / `go test` 与 project deps / generate 的 `go get` / `go mod tidy` 原先超时只杀 go，它起的 compile / link / git 进程继续以 `.roost-deps-*` / `.roost-generate-*` 暂存树为工作目录（Windows 上删不掉），输出被缓冲时调用还要等它们结束，超时不起作用。现在按进程树取消（Unix 进程组，Windows `taskkill /T`），Wait 最多再等 5 秒；Unix 上 go 在独立进程组运行，roost 接住 Ctrl-C 后先杀树再按原样退出。[记录](docs/bugfix/RR-20261004-13.md)
- **game-demo 玩家副本撤离进行中拒绝准入**（RR-20261004-11，P3，W-2026-10-04-04）：归还撤离超时后租约回到服务、撤离在后台继续时，`Admit` 现在拒绝该玩家直到撤离结束，登录的 Claim 因此加入撤离而不是在将被销毁的实体上进场。已生成工程须手工合并 `playerowner.go`。[记录](docs/bugfix/RR-20261004-11.md)
- **game-demo 刷新回合的重新认领有了时间预算，续租确认不再排在等待之后**（RR-20261004-10，P2，W-2026-10-04-03）：`renew` 先处理全部续租结果，再在 `handBackPassBudget`（15s）内先重新认领丢失租约、后归还闲置租约；超出的丢失租约保持被拒、下一轮再取。此前一批租约同时丢失可把刷新循环占住 N×5s，且排在后面的租约本地窗口越过 Redis 键 TTL（别的进程可合法接手，形成双写窗口）。已生成工程须手工合并 `playerowner.go`。[记录](docs/bugfix/RR-20261004-10.md)
- **`roost generate` / `project sync` 不再改变进程工作目录**（RR-20261004-12，P3，来源 W-2026-10-04-05）：生成器原先在运行期间 `os.Chdir` 进被生成的工程（常是 `.roost-sync-*` 暂存树），同进程其他 goroutine 此时不设 `Dir` 启动的子进程会继承它；Windows 上这会让暂存目录删不掉而残留。生成器现在拿工程根下的绝对路径，生成物逐字节不变；两行生成器提示从相对路径变为绝对路径。Windows CI 上的两次清理失败是否全由此引起未在 Windows 上证实。[记录](docs/bugfix/RR-20261004-12.md)

## [v1.19.2] - 2026-10-04

> 补丁版本：v1.19.1 回归 RR-20261004-09（RefHMap 注册表 guard 误报）、NC-30 复审发现的 RR-20261004-08、历史遗留核实（[open-triage](docs/review/REVIEW-2026-10-04-open-triage.md)）仍存在的 RR-20260921-03（P1）/ 04 / 05。无源码不兼容的 API 变化；`natsdriver.Assembly.Close` 在 drain 失败时返回包裹原错误的 `ErrClosedUndrained`（`errors.Is` 原错误仍成立）；game-demo 已生成工程须手工合并 `playerowner.go`（RR-20260921-03 / 04）。

### Fixed

- **game-demo 一次闲置归还回合不再把刷新循环占住超过租约**（RR-20260921-04，P2）：新增回合时间预算 `handBackPassBudget`（Lease − RefreshInterval − AdmissionGuard = 15s），撤离与投影等待都在预算内，没轮到的玩家留在服务、下一轮再归还；此前最坏 8×5s + 5s = 45s > Lease 30s，本进程其余租约会全部过期。已生成工程须手工合并 `playerowner.go`。[记录](docs/bugfix/RR-20260921-04.md)
- **game-demo 归还租约途中到来的登录不再拿到一个共享表里无主的“是你的”**（RR-20260921-03，P1，09-21 登记、10-04 核实仍存在）：Claim 发现该玩家正在归还就等这次归还结束（受 `login_timeout` 约束，超时回 `login_timeout`），归还不挑有在途认领的玩家；归还的标记只由它自己清除，Release 前复核、释放完才解除。此前确认先于 / 后于 Release 两种交错都让 Claim 回答 `mine=true` 而 Redis 无主，另一进程可装载第二份副本。已生成工程须手工合并 `playerowner.go`。[记录](docs/bugfix/RR-20260921-03.md)
- **RefHMap 同布局并发不再误报 `ErrRefHMapRegistryChanged`；Cached DAO 的 Delete 失败不再留 L1**（RR-20261004-09，P2，修 v1.19.1 回归，NC-30 复审发现）：Set / Delete 的 Lua guard 由逐字节比较改为“当前注册表的每个键都在本次清理清单里就放行”，并发首次创建、并发删除、Set 与 Delete 交错、记录到期恢复成功；另一布局登记了新键的 schema 竞争仍拒绝。`LayeredStore.Delete` 在远端删除报错时也删除 L1，再返回错误。v1.19.1 旧进程与新进程混跑时旧进程仍会误报。[记录](docs/bugfix/RR-20261004-09.md)
- **NatsMod 连接 drain 超预算或连接已关闭后，停止能够收敛**（RR-20261004-08，P3，来源 W-2026-10-04-02）：之前 `Assembly.Close` 硬关闭连接后只返回 ctx 错误，NatsMod 保留引用，重试永远拿到 `ErrConnectionClosed`。现在 `Assembly.Close` 返回包裹原错误的终态 `natsdriver.ErrClosedUndrained`（`errors.Is` 原错误仍成立，错误文本多一个前缀），NatsMod 报告该错误并释放引用，之后的 Stop 返回 nil；RPC 回调等待超预算时仍保留并可重试。[记录](docs/bugfix/RR-20261004-08.md)
- **CI 现在校验本仓的生成物，并运行 codegen 运行期守卫**（RR-20260921-05，P2，09-21 登记、10-04 核实仍存在）：ci.yml 新 job `generated-code`——`go generate ./...` 后工作树有变化（含未提交的新产物）即失败；随后跑 `codegen/scripts` 的 dao-golden / attribute / entity-sync / cfggen-golden 四个运行期守卫。守卫的 roost-core pin 改由 `codegen/scripts/core-pin.sh` 从 `minimumVersions.Core` 读取（此前读的那一行在合仓时被删，四个脚本默认 exit 2）；`attribute-runtime.sh` 的本地 core 目录缺省改为模块根。[记录](docs/bugfix/RR-20260921-05.md)

## [v1.19.1] - 2026-10-04

> 补丁版本：对 v1.19.0 中 B 线 NC 修复的独立复审（[NC-01～07](docs/review/REVIEW-2026-10-04-nc-audit-1.md)、[NC-08～12](docs/review/REVIEW-2026-10-04-nc-audit-2.md)、[NC-13～29](docs/review/REVIEW-2026-10-04-nc-audit-3.md)）确认的 6 个缺陷——其中 RR-20261004-02 / 03 / 06 是 v1.19.0 带出的回归——以及 B 线 RR-20261004-NC-30。无源码不兼容的 API 变化；行为变化见 RR-20261004-07（`Bus.Stop()` / `RPCClient.Stop()` 在停止已发起后立即返回）与 NC-30（RefHMap Set / Delete 在注册表变化时返回新错误 `ErrRefHMapRegistryChanged`，Delete 现在要求 adapter 支持 Eval；**已知回归**：无 schema 变化的并发首次创建 / 删除也会误报，见 RR-20261004-09，下一版修复）。

### Fixed

- **Bus 停止超预算后可再次排空，NatsMod 重试最终关闭 Assembly**（RR-20261004-07，P3，NC 复审发现，NC-09 引入）：`Bus.StopWithContext` 超预算只返回 ctx 错误并保留 worker pool，之后的调用继续等同一次排空；NatsMod 只在 ctx 错误时保留 Bus / Assembly，退订失败等终态错误照常关闭连接。**行为变化**：`Bus.Stop()` / `RPCClient.Stop()` 在停止已发起后立即返回、不再等待（回调内再调 `Stop()` 不再自锁），要等待同一次排空请用 `StopWithContext`；RPC 停止排空不再漏掉 reply / timeout 刚领取的终态 callback。[记录](docs/bugfix/RR-20261004-07.md)
- **etcd 竞选失败或取消后即时撤销 lease**（RR-20261004-06，P2，NC 复审发现，NC-11 回退）：v1.19.0 的失败清理先取消 session context 再 Close，SDK 的 Revoke 因此立即失败，候选 / 领导键留到 TTL（缺省 60s），其他候选选不上。现在由 election 用 client context 派生、5s 截止的独立 Revoke 撤销（caller 在等时等它，已取消时立即返回，下一次 Campaign 等它结束）；正常 Resign 与“setup 取消与长期 session 分离”不变。[记录](docs/bugfix/RR-20261004-06.md)、T-208
- **Layered / ReadThrough 准入拒绝不再否决权威或让读取失败**（RR-20261004-02 P2、RR-20261004-04 P3，NC 复审发现）：`LayeredStore` 的 L1 副本只在 TTL 窗口内能以 stale 拒绝回填，窗口外（含 ttl≤0）删掉旧副本、交付并回填权威值；`Set` 在远端已生效时不再因 L1 拒绝返回 `ErrStaleWrite`（生成的带版本 Cached Redis DAO 受益）。`ReadThroughStore` 的 loader 回填被 L1 拒绝时交付 L1 已准入值或 miss（conflict 无值仍拒绝），loader 结果回写 L2 的 stale 不再让 `Get` 失败。[02](docs/bugfix/RR-20261004-02.md)、[04](docs/bugfix/RR-20261004-04.md)
- **RefHMap Patch 不再让同一条记录的 hash 分开过期**（RR-20261004-03，P2，NC 复审发现）：Patch 原先只续期根到叶路径上的 hash，兄弟 hash 先过期后 `Get` 报 `ok=true` 返回部分记录（写入的值读回零值），NC-18 把它扩大到所有嵌套路径。现在 Patch 续期整条记录的全部布局 hash，并把它们并入 `__keys`（顺带修复旧数据 Patch 后 Delete 漏删）；`Get` 遇到被引用、按布局必然非空却已缺失的子 hash 时整条报 miss，修复前写入的这类数据会按缺失重载。存储格式不变。[记录](docs/bugfix/RR-20261004-03.md)
- **mongotest 唯一索引的 null / sparse 语义**（RR-20261004-05，P3，NC 复审发现）：替身以前缺任一唯一字段就跳过检查、也不看 `Sparse`，比真实 Mongo 宽松。现在非 sparse 唯一索引把缺字段（含穿过标量父字段的点路径）当 BSON null、与显式 null 相等，建索引与写入报 `ErrDuplicateKey`；sparse 只在全部索引字段都缺时跳过。仅测试替身行为收紧，需要允许缺字段的测试应设 `Sparse: true`（真实部署同样需要）。[记录](docs/bugfix/RR-20261004-05.md)
- **RefHMap schema清理竞争（RR-20261004-NC-30）**：Set/Delete的Lua在副作用前比对root registry快照，变化返回新增ErrRefHMapRegistryChanged，防止遗漏并发发布的子hash。Delete复用Eval、未知结果不自动重放，存储格式保持；不是值CAS，不清理历史孤儿。[12正式回归/消费与限制](docs/bugfix/RR-20261004-NC-30.md)。

## [v1.19.0] - 2026-10-04

> v1.18.0 之后两条工作线的修复：A 线——B27 端到端补测暴露的 RR-20260930-20～24、维护者拍板的 N20～N32（RR-20260930-12～19）、对 B 线修复的独立复审发现的 RR-20261001-01～09、RR-20261004-01（取锁结果未知），以及 C01 长稳通过（1 小时 0 错误、全量核验通过、内存平台）与 B29 性能对照（无新退化）；B 线——非核心模块审查 RR-20261003/04-NC-01～29（runtime / request / RPC / etcd / cache / Mongo 测试替身）与 codegen CG-12～14。**次版本而非补丁**：`entity.IThreadSafeRemoteEntity.SetEntityVersion` 改为返回 `error`（RR-20260930-13），`activity.Admin` 增加 `ReconcileProgress`（RR-20261001-05），新增 `account.Admin`；另外 B 线下列行为收紧升级前要核对：`manager.Engine` 只能 Start 一次（新错误 `ErrStartState`，RR-20261004-NC-02）、运行中的 Ops 再次 Start 报错（NC-04）、`BindJSON` 拒绝合法首值后的尾随内容（RR-20261003-NC-04）、单 Mod 应用缺依赖 / 自循环即启动失败（RR-20261003-NC-01）、`httpclient` 部分错误改为 `errors.Join`（NC-03，`errors.Is` 仍成立）、RefHMap Eval 失败不再降级为非原子写且 `cache.refhmap.write_degraded_total` 指标移除（NC-21）；新增 `webroute.ValidatePath`。生成的 game-demo 仍能对 v1.18.0 编译，生成器 Core 下限不变。

- **Mongo测试替身边界（RR-20261004-NC-26～29）**：D嵌套路径保留同级字段，unique索引验证存量后逐个发布，BulkWrite先验证全部模型Type；事务使用私有快照，abort不擦除并发提交，集合revision冲突按既有限制重试。事务内索引明确unsupported，新增ErrTransactionConflict；集合粒度比真实Mongo保守。28正式回归及受影响消费者通过，生产Mongo/DataEngine未改。[修复与限制](docs/review/REVIEW-2026-10-04-noncore-17.md)。

- **RefHMap未知写与Mongo替身契约（RR-20261004-NC-21～25）**：Eval失败保留原始原因，停止无条件DEL重放；已应用写不自动回滚，旧降级告警/计数移除，依赖fallback的adapter需支持现有Lua。替身深复制BSON容器、稳定去重_id候选、精确比较整数/有限float并按目标身份返回post-image；非有限float明确unsupported。40新增正式叶子、真实Redis及生成DAO验证通过；[记录](docs/review/REVIEW-2026-10-04-noncore-15.md)。

- **RefHMap与替身分页（RR-20261004-NC-16～20）**：保留根指针类型、nil根写前拒绝、指针文本codec用地址副本；Patch同槽维护nil父引用/登记键/路径TTL，缺root或Eval失败明确返错。布局名称碰撞/重复/分隔符在I/O前拒绝；mongotest分页统一skip→limit。合法存储格式保持，旧非法布局/错误字节不自动迁移。[类型/编码](docs/bugfix/RR-20261004-NC-17.md)、[Patch/生成消费者](docs/bugfix/RR-20261004-NC-18.md)、[名称兼容](docs/bugfix/RR-20261004-NC-19.md)、[分页](docs/bugfix/RR-20261004-NC-20.md)

- **缓存准入与拒写结果（RR-20261004-NC-13～15）**：ReadThrough的Get/Delete保留fatal裁决，Layered不交付L1拒绝的回填、不续拒绝TTL；Local/Grouped/RawJSON/JSONHash旧写现在返回ErrStaleWrite，容忍晚到写的调用方需显式errors.Is。普通故障策略、公开签名与存储格式保持；Redis读前比较仍非CAS。[fatal](docs/bugfix/RR-20261004-NC-13.md)、[Layered](docs/bugfix/RR-20261004-NC-14.md)、[兼容与消费者](docs/bugfix/RR-20261004-NC-15.md)

- **etcd setup与关闭预算（RR-20261004-NC-11/12）**：Campaign取消连接覆盖session创建，成功前解除以保留长期领导权，deadline保留caller与SDK原因；WatchCallback使用唯一底层关闭任务，Done等handler/watcher真实退出，取消只结束等待。完成关闭现在返回过去被吞掉的底层Close错误，公开签名/存储格式不变；正常Resign的TTL级Revoke及真实集群恢复尚未验证。[选主](docs/bugfix/RR-20261004-NC-11.md)、[关闭兼容](docs/bugfix/RR-20261004-NC-12.md)

- **RPC协议与预算（RR-20261004-NC-08/09/10）**：JetStream无handler拒绝复用版本envelope；RPCClient.StopWithContext以每实例唯一扫尾保留回调责任，Assembly/Kit取消后保留资源供再次排空；ServiceRPC组合调用从发现开始共用deadline，不向过期候选发业务。[协议](docs/bugfix/RR-20261004-NC-08.md)、[停止兼容](docs/bugfix/RR-20261004-NC-09.md)、[发现预算](docs/bugfix/RR-20261004-NC-10.md)
- **请求准入与异常边界（RR-20261004-NC-05/06）**：超 burst 需求不占限流 key、不续 idle 活性；Recover 保持固定错误，上报与失败日志 panic 独立隔离，不引入异步重试。[限流](docs/bugfix/RR-20261004-NC-05.md)、[Recover](docs/bugfix/RR-20261004-NC-06.md)
- **生成路由模式校验（RR-20261004-NC-07）**：新增 webroute.ValidatePath 复用 chi 解析，生成扫描和运行期共同拒绝非法模式；安装成功才写 seen，installer panic 转 error。正常生成形状与原基础路径错误文本不变，自定义 router 半安装仍须重建。[兼容与验证](docs/bugfix/RR-20261004-NC-07.md)
- **Manager 生命周期错误与启动权（RR-20261004-NC-01/02）**：逐对象 Stop panic 转 error，清理/rollback 继续并保留原因；Engine 在 Provide 后只允许一次启动尝试，再次/停止后的 Start 返回 ErrStartState（Kit ErrManagerStartState 同值），缺 Provide 拒绝不消耗启动权。[清理](docs/bugfix/RR-20261004-NC-01.md)、[启动](docs/bugfix/RR-20261004-NC-02.md)
- **Admin JSON schema 隔离（RR-20261004-NC-03）**：非 nil 空 map 与嵌套数组内 JSON 容器递归复制，Register/Get/List 的副本互不改写；nil/空保持，非 JSON 扩展值需不可变。[记录](docs/bugfix/RR-20261004-NC-03.md)
- **Ops 关闭后再释放 server（RR-20261004-NC-04）**：取消/超时保留同一 server，成功排空才释放；Start 拒绝未关闭实例、监听 goroutine 捕获实例，状态短锁与各 caller 的 Shutdown context 分离。[记录](docs/bugfix/RR-20261004-NC-04.md)

- **App 单 Mod 也做完整依赖校验（RR-20261003-NC-01）**：仅空集合快速返回，单 Mod 拒绝 nil/空名、缺失硬依赖和自循环，错误图不进入 Init。合法外部共享/optional 行为不变。[记录](docs/bugfix/RR-20261003-NC-01.md)
- **HTTP Clone 超时和错误分类（RR-20261003-NC-02/03）**：库拥有 client 的 Clone timeout 修改作用于副本，父实例和连接池保持；自定义 client 自己的 Timeout 优先。非 2xx 坏/异型或读取失败的 body 仍可 errors.As 提取 StatusError，并保留解码/读取原因。[超时](docs/bugfix/RR-20261003-NC-02.md)、[分类](docs/bugfix/RR-20261003-NC-03.md)
- **JSON 请求必须为完整单值（RR-20261003-NC-04）**：BindJSON 拒绝合法首值后的垃圾/第二值，返回 400 且不调用业务；合法尾随空白、现有空 body 与限长行为保持。[记录](docs/bugfix/RR-20261003-NC-04.md)

> 维护者 2026-09-30 对 [REMAINING §3](docs/review/REMAINING-2026-09-28.md) 的 13 条待决定项拍板：N21 / N23 / N24 / N25 / N26 / N28 / N31 / N32 做，N20 / N22 / N27 / N29 写进契约（N29 另加入口校验），N30 写部署文档。

### Changed（行为收紧 / API 变化）

- **chat 最新页不再因普通容量淘汰报 `Gap`**（RR-20261001-08，P3，复审 B 线发现）：RR-20260929-27 把“本页到达 ring 头部且头部序号 > 1”也当作洞，任何溢出过的频道无游标取最新页、或 `BeforeSeq` 翻到保留边缘都 `Gap=true` 并每次计 `history.gap.<kind>`。现在 `Gap` 只在页内序号不连续、游标点名的消息已不在（`AfterSeq+1` / `BeforeSeq-1` 已淘汰）、尾部缺失时为真，指标只计这些情形；`HasMore` / 游标 / `OldestSeq` 不变，靠 `Gap` 判断“曾淘汰过”的客户端改看 `OldestSeq > 1` 或 `Stats.Evicted`。[记录](docs/bugfix/RR-20261001-08.md)
- **account 建角 slot 不再因名字被他人拿走而永久 pending**（RR-20261001-06，P2，复审 B 线发现）：同名重试发现名字已被别的账号 committed 时释放 slot（该计划已不可能 Commit 名字 / 发布角色），仍回 `ErrNameTaken`，之后换名建角成功，指标 `create_role.plan_released`；名字只被别人 reserved 时仍保留计划。新增 owner-only `account.Admin.ResolvePendingCreation(accountID, serverID, note)`（不上 bus）放弃一个 pending 计划：备注必填写到 `Account.admin_note` / `admin_action_at_unix`，返回被放弃的 `RoleCreation`（其 `PlayerID` 指向要留档的未发布角色记录），名字仍被本计划 reserved / committed、已发布、legacy 空 slot 拒绝 `ErrNotResolvable`（新码 560115；560116 `ErrAdminNoteRequired`）。RR-20260929-19 “名字被新 owner 占用后保留证明”收窄为“被 reserved 时保留”。[记录](docs/bugfix/RR-20261001-06.md)、T-182
- **activity 参与者不再因过期的 pending 证明永久被拒**（RR-20261001-05，P2，复审 B 线发现）：`applyProgress` 把 ledger 条目已被 `reservation_ttl` 删除的 pending 证明回收（条目消失即越过客户端重试地平线，ledger 本就把之后的重放当新请求），指标 `apply_progress.proof_expired`；pending 满 32 改报 `activity.ErrProgressBacklog`（新码 620119）而不是 `versionstore.ErrConflict`，调用方可区分背压与 CAS 争用。新增 owner-only `activity.Admin.ReconcileProgress(key, participantID, note)`，把每个 pending 证明缺的 ledger mark 补上再释放（补后同 requestID 重放是 no-op），`Participant` 新增 `admin_note` / `admin_action_at_unix`；`activity.Admin` 接口多一方法（仓外自实现需补）。[记录](docs/bugfix/RR-20261001-05.md)、T-180
- **Go API 签名变化（源码不兼容）**：`entity.IThreadSafeRemoteEntity.SetEntityVersion(int64)` → `SetEntityVersion(int64) error`（RR-20260930-13，N26）。同一 fence 下写更小的 StateVersion 现在返回 `ErrRemoteVersionConflict` 且不写入；生成实体经嵌入 `RemoteEntityBase` 获得该方法、不受影响，仓外自行实现该接口的类型需改签名。[记录](docs/bugfix/RR-20260930-13.md)
- **fence 之后拒绝 Durability 0 的 Remote 直写（RR-20260930-12，P2，N21）**：带 Remote 批次、没有 effect 的 memory handler 在引擎 fence 后回复 `nest.ErrNestFenced` + `ErrCommitRejected`（判别表第 12 行）、Remote 批次 Abort，权威不再被写；此前成功并写权威。[记录](docs/bugfix/RR-20260930-12.md)
- **广播每个目标自己的锁作用域（RR-20260930-14，N28）**：`broadcastDispatch` 在目标结束时释放它取得的全部锁（含 Destroy 后同 ID 重建的实例、Cast 取得的实体）与 Sync post-release 回调，不再跨后续目标持有。每目标多一次 Guard 池取还，未做基准。[记录](docs/bugfix/RR-20260930-14.md)
- **实体实现必须是指针（RR-20260930-15，N29）**：`BuildEntity` 与 `EntityManager.Add` / `TryAdd` 拒绝值类型实现，新增 `entity.ErrEntityNotPointer`（错误点名类型）；生成实体都是指针。[记录](docs/bugfix/RR-20260930-15.md)
- 契约文字（无行为变化）：USER_GUIDE §4 判别表外补 N20（`refuseCommitAfterFence` 是交给 committer 前的一次性检查，跨 goroutine 的窗口由 WAL terminal 兜底）、N22（业务吞掉嵌套事务结果未知得到的成功回复不加哨兵）、N27（Guard 锁账本按 ID，一个 handler 不跨 Manager 持有同 ID 实体）；B40 回归 `state_strict` 断言加强为每个 follower 各至少一次（N32）。

### Added

- **生成工程自带 compose 结构检查 `deploy/docker/compose_check_test.go`，CI 与 `make compose-check` 跑它**（RR-20260930-17，N24）：读 `docker compose config --format json` 的解析结果，断言每个 Service 的 tmpfs 恰好一条绝对路径挂载、read_only / user / cap_drop / security_opt、stop_grace_period、healthcheck、config bind 与命名卷；只在 `ROOST_COMPOSE_CHECK` 设置时执行（CI generated-and-deployment 作业与 Makefile 设置），没设置时跳过。已有工程 `project sync` 新建该文件并更新 Makefile / ci.yml。[记录](docs/bugfix/RR-20260930-17.md)
- **部署文档：stats_log 统计文件不轮转，给出 `copytruncate` 的 logrotate 示例**（REMAINING N30）：`docs/DEPLOYMENT.md` §4 / §5 与生成的 `deploy/shell|docker/README.md`；文件以 `O_APPEND` 打开且进程不重开，`create` / 改名式轮转无效。[记录](docs/bugfix/RR-20260928-04.md)
- **Remote 非 authority 兼容装配的所有权标记键可加部署前缀**（RR-20260930-19，REMAINING N25，维护者 09-30 拍板）：新增 `remoteentity.NewRedisMarkerWithKeyPrefix(redis, prefix)` 与 `ValidateMarkerKeyPrefix`，非空时键为 `<prefix>:remote_entity:marks`，与 L2 快照前缀同形；空值键逐字不变，与 `NewRedisMarker(redis, "")` 互读。正式 kit 装配不写这把键（所有权存储是 Mongo 权威），kit 配置面不变；USER_GUIDE §6 新增 Remote 三类 Redis 键清单，写明 `remote_entity.lock_key` 缺省 `e` 不隔离、共用 Redis 的部署须各配不同值。记录：[bug](docs/bug/RR-20260930-19.md) / [bugfix](docs/bugfix/RR-20260930-19.md)。

### Fixed

- **CI 恢复绿色**：main 的 ci / nightly 自 10-01 起红——RR-20261001-01 改宽守卫正则后既有测试按旧字面查找（已改为按形状匹配），10-04 起 `codegen/internal/webroute` 为复用 `ValidatePath` import 了 core 运行时包、违反生成器边界（改为直接调用 chi），以及 grpc 由间接依赖变为直接依赖未 tidy。见 [RR-20261001-01](docs/bugfix/RR-20261001-01.md)、[RR-20261004-NC-07](docs/bugfix/RR-20261004-NC-07.md) 的“复核后的补修”。
- **Remote 锁取锁没有拿到 Redis 答复后不再卡到 LockTTL**（RR-20261004-01，P2）：取锁脚本 Eval 因 ctx 截止 / 网络错误返回错误而脚本已在 Redis 执行时，旧版本之后每次写都回 `versioned lock not acquired`，直到 `lock_ttl`（缺省 24h）。现在 token 按锁对象分代（随机前缀 + 递增序号），下一次 TryLock 由 Lua 判定：owner 是本锁对象更早一代就换成新 token、新 fence 取回，别人持有照旧 NotAcquired，迟到的旧代脚本挤不掉新代际；RR-20260930-21 的释放未知路径并入同一判定。token 格式变为 `<base32>.<seq>`，TryLock 脚本多一个 ARGV（KEYS 不变），新旧二进制混跑时互相视为“别人持有”。[记录](docs/bugfix/RR-20261004-01.md)、T-207
- **Remote 负载 harness 有错误时仍做区间一致性核验**（测试 harness，不是 RR）：失败请求按 USER_GUIDE §4 判别表用 `errors.Is` 分成“确定未生效”和“结果未知”两类，每个实体按 `[成功回复数, 成功回复数 + 结果未知数]` 区间核验 Mongo / NATS / outbox，无错误时与原来的精确核验逐字相同。`result.json` 新增 `ErrorsNotApplied` / `ErrorsUncertain` / `ErrorClasses` / `ErrorClassFirst`；`.verified` 先写出，之后再以负载错误让测试失败；`scripts/perf/remote.sh` 在核验通过但有错误时输出 `consistency verified; load errors=…` 并退出 3。新增 `ROOST_REMOTE_REQUEST_TIMEOUT`（缺省 30s，仅用于构造结果未知）。见 [REMOTE-ACCEPTANCE §负载有错误时的区间核验](docs/feature/REMOTE-ACCEPTANCE-2026-09-24.md)、C01-RUNBOOK §5。
- **隔离集成环境的 mongod 有了 WiredTiger 缓存上限**（`ROOST_IT_MONGO_CACHE_GB`，缺省 1）：三个副本同机，缺省缓存（物理内存的一半）合计超过内存，24 小时长跑中宿主机换页、被测进程停顿数秒，C01 第 3、4 次的写许可拒绝都来自这里。已在跑的环境要 `down` 再 `up` 才生效。见 `kit/scripts/integration/README.md`。
- **cfggen 显式 `index: false` 生成可编译绑定（RR-20260930-CG-12）**：strconv 导入复用索引 enabled 判断；数字/bool 禁用不再留下无用导入，真索引转换保持。重生成绑定即可，无持久格式变化。[修复与消费回归](docs/bugfix/RR-20260930-CG-12.md)。
- **cfggen 写入前拒绝生成名称冲突（RR-20260930-CG-13）**：保留默认注册 wrapper、configdata import 与实际需要的 strconv 名；已冲突 schema 必须改 bean 名和引用，拒绝时保留旧输出，不静默改公开 API。[兼容与回归](docs/bugfix/RR-20260930-CG-13.md)。
- **依赖事务同时提交明确的合仓迁移（RR-20260930-CG-14）**：在 resolver 前冻结框架迁移的 Go/manifest 变化，与最终模块文件共同验证/提交；依赖命令的任意业务改写仍隔离。普通 deps 仍只更新模块文件，映射外业务 API 手工处理。[事务边界与正式消费者](docs/bugfix/RR-20260930-CG-14.md)。
- **game-demo 赠礼 deliver 的收件人检查改读 Player DAO 自己的库与集合**（RR-20260930-22，P2，B27 第 3 批真实环境暴露）：此前读 `dataengine.database`，与 DAO 标记的 `db=game` 只在缺省时同库，改 `dataengine.database` 后所有赠礼以“收件人从未进过游戏”补偿；现在用 `db.PlayerDaoDBName` / `db.PlayerDaoCollection`。已有工程 `project sync` 或手改 `gift_saga.go` 一处。[记录](docs/bugfix/RR-20260930-22.md)
- **续租中断后重取租约时不再把在线玩家留在“连接活着却脱离场景”的状态**（RR-20260930-23，P2，B27 第 3 批真实环境暴露）：game-demo `PlayerOwners.renew` 取回失效租约（没人接手）后对仍连着的玩家像围栏一样关闭连接，日志 `lease had lapsed and was retaken; the stale copy was dropped` 新增 `sessions_closed`；副本在预算内扔不掉时同样关连接。客户端重连走正常登录从 Mongo 冷加载并进场；之前连接保留但收不到帧、别人也看不到他。只改 demo 模板，已生成工程须手工合并 `internal/service/game/playerowner.go`。[记录](docs/bugfix/RR-20260930-23.md)、T-179
- **game-demo 活动租约丢失后重新 acquire 而不是每 5s 告警到停机**（RR-20260930-24，P3）：续租错误按 `errors.Is` 分类——过期 / 不持有 / 无记录立刻 `AcquireLease`，拿不到进 standby 每心跳重试，瞬时错误下周期再续；日志只在状态变化时打。`kill -9` 后 30s 内启动的 acquire conflict 维持 Init 失败（理由见记录）。[记录](docs/bugfix/RR-20260930-24.md)
- **activity 后台 sweep 不再被一条坏 Opening 条目卡死，legacy Opening 不再永久占名额**（RR-20261001-09，P3，复审 B 线发现）：无 Intent 的旧 Opening 条目过 `OpeningGrace` 且活动不存在时回收名额（同 key Open 补上计划的不动）；Intent 畸形的条目跳过并计 `sweep.opening_intent_malformed`、日志只在出现 / 恢复时各打一次、名额保留待运维修正；单条 Create 失败在同组其他到期活动完成之后再上报。此前 v1.18.0 的 `AdvanceExpired` 在该条目上直接返回，整组每 tick 都失败。[记录](docs/bugfix/RR-20261001-09.md)
- **game-demo 取回租约但 stale 副本扔不掉的玩家不再被下一次心跳续回服务**（RR-20261001-07，P3）：`Claim` 在 `dropResident` 失败后忘掉该玩家的本地租约状态，刷新循环不再续它、`Admit` 持续拒绝，Redis 租约按“进程死掉”的结局自然过期；之后的 `Claim` 等清除完成再确认。此前下一轮 `Refresh` 用自己的 token 续成功会清掉 `interrupted` 并延长窗口。已生成工程须手工合并 `playerowner.go`。[记录](docs/bugfix/RR-20261001-07.md)
- **schema 已写但 `configs/table` 还没有 CSV 的工程 `roost generate` 不再失败**（RR-20261001-03，P2，v1.18.0 回归，复审 B 线发现）：config-data 步骤在 CSV 目录为空时只有 `_manifest.json` 仍登记着生成文件才运行 tablegen（有东西可退役），否则跳过——恢复 v1.17.2 语义，RR-20260930-09 的退役能力不变。保留 schema 但删掉其 CSV 且上一轮 JSON 仍登记时仍报 `open configs/table/<file>.csv`，见参考文档 §9.1。[记录](docs/bugfix/RR-20261001-03.md)
- **v1 manifest 遇未认领 JSON 的错误文本给出恢复步骤**（RR-20261001-04，P3）：`untracked table JSON … from legacy manifest (v1 recorded no ownership): delete it if an earlier generation produced it; if it is hand-maintained, move it out of …, run generate once to upgrade _manifest.json to v2, then move it back`；`codegen/docs/CODEGEN_REFERENCE.zh-CN.md` 新增 §9.1（manifest v2、退役规则、v1 升级三步）。失败设计不变。[记录](docs/bugfix/RR-20261001-04.md)
- **mail 同 RequestID 恢复不再因 nil / 空切片差异卡死**（RR-20261001-02，P3，复审 B 线发现）：`sameSendIntent` 由 `reflect.DeepEqual` 改为逐字段比较，`Recipients` / `Attachment` 用 `slices.Equal` / `bytes.Equal`。自定义 `EnvelopeStore` 把缺失切片还原成空切片时，RR-20260929-16 的恢复此前每次重试都返回 `ErrConflict: reserved envelope differs or is missing`；ID、期限、正文、受众等任何真实差异仍然拒绝。内置 Redis 存储行为不变。[记录](docs/bugfix/RR-20261001-02.md)
- **CI：Redis job 导出 service 测试实际读取的全部 Redis 门变量**（RR-20261001-01，P2）：09-29 的 service 修复把 Redis 变体挂在 `ROOST_REVIEW_REDIS` / `ROOST_REDIS_TEST_ADDR` / `ROOST_REVIEW3_BACKEND` / `ROOST_REVIEW4_BACKEND` 上，job 只设 `REDIS_ADDR`，9 个测试文件的变体静默 SKIP 或落回 Memory；现在 job 全部导出，守卫正则覆盖它们，根包测试把测试文件里的门变量钉到 ci.yml。本地跑这些变体也要导出这五个变量。[记录](docs/bugfix/RR-20261001-01.md)
- **释放 Redis 锁失败后同一实体不再在本进程内永久不可写**（RR-20260930-21，P2，B27 第 2 批端到端暴露）：`versionedLock.UnlockWithRetry` 在 Redis 错误用尽重试或 ctx 到期时本地不再算持有、保留上一代 token，下一次 `TryLock` 把它交给 Lua 以 Redis 为准（租约仍是自己的就换新 token / 新 fence 重新取得，被别人持有走既有 NotAcquired 重试）；此前本地 `acquired` 不清，每次写都回 `versioned lock already acquired` 直到重启。释放失败仍计 `release_failure_total` 并回复 `ErrRemoteReleaseIncomplete`。TryLock 脚本多一个 `ARGV[3]`（KEYS 不变）。[记录](docs/bugfix/RR-20260930-21.md)、T-178
- **回滚后 release hook panic 的回复保留业务错误**（RR-20260930-20，P2，B27 第 2 批端到端暴露）：handler 返回业务错误、事务回滚后 `OnEntityRelease` 钩子 panic，回复现在是 `errors.Join(业务错误, hook 错误)`，`errors.Is` 对两者都成立，且不带 `ErrAfterCommitFailed`；之前回复只剩 hook 错误。已提交路径（handler 成功、提交后 hook 失败）的 `ErrAfterCommitFailed` 回复逐字不变。记录：[bug](docs/bug/RR-20260930-20.md) / [bugfix](docs/bugfix/RR-20260930-20.md)。
- **`roost add mod` / `add saga` 给 CRLF 检出的服务配置追加 Mod 段时沿用原行尾**（RR-20260930-16，N23）：开发配置与生产示例之前一律按 LF 追加，一份文件行尾混用；现在与 Secret 示例同一条路径（`lfText` / `restoreLineEndings`），LF 与空文件保持 LF。[记录](docs/bugfix/RR-20260930-16.md)
- **game-demo：`Service.Shutdown` 把 App 的停机 ctx 传给 `Scene.Close`**（RR-20260930-18，N31）：停止“卸载后重载”与 replication manager 排空受 `shutdown.total_timeout` 约束，不再用 `context.Background()` 等到部署侧 SIGKILL。demo 文件应用所有，已有工程手改两处。[记录](docs/bugfix/RR-20260930-18.md)

## [v1.18.0] - 2026-09-30

> 本版合并两条工作线：A 线（core cache、生成链路测试、CI 门禁：RR-20260928-15、RR-20260930-03/11）与 B 线（Service 十域、Redis driver、Codegen 退役旧产物：RR-20260929-01～34、RR-20260930-01/02/04～10）。B 线有源码不兼容的 Go API 变化（`Mail.CancelClaim`、`session.ClaimStore`、`platform.Admin`），所以是次版本而非补丁；生成器 Core 下限与 framework-compat 的 minimum 同步升到 v1.18.0。两条线的逐编号状态见 [ARCHIVE-2026-09-30](docs/review/ARCHIVE-2026-09-30.md)。发版前验证：`scripts/pretag.sh` 通过（干净 worktree）；Remote 故障矩阵 21/21 PASS（`artifacts/perf/remote/matrix-v1180-30085d62`，本地）。tag → `4b277176`。
> B 线（Service / Redis driver / Codegen review→bugfix 循环，2026-09-29～09-30）：Service 十域主链审查 RR-20260929-01～34 全部修复（另补三条旧 RR 残余），Redis driver 一项，Codegen 生成物退役 RR-20260930-01/02/04～10 九项。总记录见 [SERVICE-BUGFIX-2026-09-29](docs/bugfix/SERVICE-BUGFIX-2026-09-29.md) 与第三～九批 [03](docs/bugfix/SERVICE-BUGFIX-2026-09-29-03.md) / [04](docs/bugfix/SERVICE-BUGFIX-2026-09-29-04.md) / [05](docs/bugfix/SERVICE-BUGFIX-2026-09-29-05.md) / [06](docs/bugfix/SERVICE-BUGFIX-2026-09-29-06.md) / [07](docs/bugfix/SERVICE-BUGFIX-2026-09-29-07.md) / [08](docs/bugfix/SERVICE-BUGFIX-2026-09-29-08.md) / [09](docs/bugfix/SERVICE-BUGFIX-2026-09-29-09.md)，阶段结论见 [REVIEW-2026-09-29-services-11](docs/review/REVIEW-2026-09-29-services-11.md)。**这批修复只改代码与生成模板，没有自动迁移任何存量数据**：Rank 去重账本、Activity pending proof、Platform 待办、Account 建角计划等都要求相关 owner 停写后一起升级，不支持新旧写者混跑；各条的旧数据对账边界见对应记录。

### 行为与 API 变化（升级前必读）

**Go API 签名变化（源码不兼容，调用方必须改）**

- **`service/mail.Mail.CancelClaim` 增加 `attempts int32` 参数（RR-20260929-20）**：旧 `CancelClaim(ctx, playerID, mailID, token)` → 新 `CancelClaim(ctx, playerID, mailID, token, attempts)`；`Service` / `BusClient` / `capability` 三个实现与 `kit/service/mail` 别名同步。调用方传 `claim.Attempts`（`ReserveClaim` 返回的 Claim 上已有），`attempts <= 0` 或 RPC JSON 缺 `attempts` 字段返回 `CodeRequestInvalid`，不能降级为 token-only。仓内 `kit/service/examples/split/consumer.go` 与 demo 模板 `demo/game/controllers/player/claim_mail.go.tmpl` 已改；**已生成工程的 `claim_mail.go` 是业务文件，`roost project sync` 不会更新，需手工补参数**。滚动顺序：先升级全部 mail owner，再升级调用方（旧 owner 忽略 attempts，新客户端连旧 owner 仍不安全）。
- **`service/session.ClaimStore` 由类型别名改为接口（RR-20260909-02 残余）**：旧 `type ClaimStore = versionstore.Store[int64, Claim]` → 新 `interface { versionstore.Store[int64, Claim]; versionstore.ConditionalDeleter[int64, Claim] }`。自定义 ClaimStore / 包装器必须实现并转发 `DeleteIf`，不提供“先 Get 再 Delete”的回退；内置 Memory / Redis store 已实现。
- **`kit/service/directory.New` 与 `kit/service/account.New` 要求 store 支持 `versionstore.ConditionalDeleter`（RR-20260929-21 / 19）**：Directory 的 state store、Account 的 `Slots` 不满足时构造返回错误（`directory: state store must support atomic identity-checked DeleteIf` / `Slots (atomic identity-checked DeleteIf is required)`）。计数 / 故障注入等包装器要显式转发该能力。
- **`service/mail.NewRedisEnvelopes` 的客户端窄接口由 `MGet` 改为 `Pipeline() redis.IPipeline`（RR-20260929-33）**：只实现 `SetNX/Get/MGet` 的自定义客户端需补 `Pipeline` 或改接正式 `IRedis`；传正式 driver 的调用方不受影响。自定义 `EnvelopeStore` 不受影响。
- **`kit/service/platform.Admin` 接口新增方法 `ResolvePendingAttempts(ctx, orderID, note string) (Order, error)`（RR-20260929-01）**：`Service` 已实现；仓外若有自己实现 `Admin` 的类型需补该方法。owner-only，不暴露到 bus。
- **`kit/service/global/activity.OpeningEntry` 新增 `Intent *Activity`、`Window` 新增 `ScanAfter *Key`（RR-20260914-02 残余）**：使用位置复合字面量构造这两个结构的仓外代码需改为命名字段。JSON 均 `omitempty`。

**新增导出（向后兼容）**

- `kit/service/platform.ErrDeliveryNotApplied`（RR-20260929-23）：collaborator 侧“确定没有外部效果”的哨兵，`Deliverer.Deliver` 返回时 wrap 它才会移除当前 pending attempt 并按原预算重试；其他错误一律保留 pending 进入 `exhausted` 等人工对账。`Deliverer` 接口签名不变。
- `versionstore.ConditionalDeleter[K, T]` 接口（`DeleteIf(ctx, key, expect, match func(T) bool) error`），`MemoryStore.DeleteIf`、`RedisStore.DeleteIf`（Lua 内比对完整原始字节后删值与索引）；`RedisStore.IndexRemoveIfAbsent(ctx, key) (bool, error)`（值 key EXISTS 才不删固定索引成员，RR-20260929-28）。原 `Delete` / `IndexRemove` 语义不变。
- `service/session`：`Config.Owners OwnerSource`、`OwnerSource` 接口（`SweepOwners(ctx, limit) ([]int64, error)`）、`OwnerSourceFunc`、`Service.SweepPending(ctx, limit)`、`Service.BackgroundSweepEnabled()`；`kit/service/session`：`ModOption`、`WithSweepOwners(source)`，`NewMod(release, reporter, options ...ModOption)`（变参追加，旧两参调用不变），别名 `OwnerSource` / `OwnerSourceFunc`（RR-20260929-09）。未配置 source 时后台 sweep 明确禁用，保留 lazy 清理。
- `kit/mods.ValidateClusterKeyPrefix(cfg, service, prefix) error`（RR-20260929-29 / 30 / 31）。
- `kit/service/account.RoleCreation` 类型，`Role.CreationID`、`Slot.Creation` 字段（RR-20260929-19）；`kit/service/platform.Order.AttemptSequence` / `PendingAttempts`（RR-20260929-01）；`activity.Participant.PendingRequestIDs` / `ProgressProofVersion`（RR-20260929-02）；`service/mail.Entry.EnvelopeExpiresAtUnix`（RR-20260929-04）、`SentRecord.Intent`（RR-20260929-16）、`SettledClaim.Deleted`（RR-20260910-02 残余）。均为 JSON `omitempty` 新字段，旧记录零值按旧语义读取、不猜测。

**配置与启动行为**

- **Rank / Platform / Activity 在 Redis Cluster 下拒绝无有效 hash tag 的前缀（RR-20260929-29 / 30 / 31）**：`redis.cluster_addrs` 非空时，`rank.key_prefix` / `platform.key_prefix` / `activity.key_prefix` 必须有非空、闭合的**第一个** `{...}`（空首对不能被后面的 tag 挽救），否则 `Mod.Init` 失败并点名配置键。单机前缀不变；此前被错误接受的配置现在启动失败，改配置前要按旧 key 空间做迁移计划，直接加 tag 不等于旧数据已迁移。
- **session Attach 拒绝携带释放态的资源（RR-20260929-14）**：`Resource.ReleasedAtUnix != 0` 或 `ForcedRelease` 返回 `ErrRunInvalid`；客户端回传服务端快照前须清掉管理字段。
- **account 已验证身份键编码（RR-20260929-11）**：`Identity.AccountID()` 对规范化 channel 中的 `%` / `:` 转义；普通 channel 的账号 ID 不变，含这两个字符的 channel 派生 ID 变化，读旧 ID 只有 Channel / OpenID 与 verifier 结果一致才复用。
- **chat 自定义频道键编码（RR-20260929-07）**：Kind 中的 `%` / `:` 可逆转义；含这两个字符的旧自定义频道需停写后按明确类型复制到新键，不读歧义旧键。`Page.Gap` 语义扩大为“本页跨越的任何缺口”（含页内 / 尾部洞），`HasMore=false` 不再等于从未丢消息（RR-20260929-27）。
- **rank Redis 去重账本字段 `applied` → `applied_v2`（JSON 数组，RR-20260929-05）**：读时兼容旧格式，下一次写入升级，损坏账本拒绝更新；新旧节点不能并发写同一榜键。
- **`redis.IPipeline.Exec` 契约（RR-20260929-34）**：签名不变，但现在 aggregate 为 nil / `redis.Nil` 时逐命令返回首个非 Nil 错误——没有 future 的写命令（HSet / RPush / ZAdd…）失败时 `Exec` 不再返回 nil。以前把 nil 当写成功的调用方会开始收到真实错误；已执行的命令不回滚。
- **`ReserveClaim` 对 `ClaimAttempts < 0` 或 `== MaxInt32` 的条目拒绝（RR-20260929-20）**，防止代次回绕重用。

**生成物形状变化（Codegen，需 `roost generate` / 手工合并）**

- **各生成器删除最后一个输入 / 标记后退役旧产物（RR-20260930-01/02/04～09）**：servicerpc `-check` 把不在预期集合、带自身生成头且记录同一 regenerate 命令的 `_rpc_gen.go` / `_rpc_assembly_gen.go` 报为 `STALE: x (orphan)` 并失败，普通运行删除；protocol 定义清空时退役 proto / PB / msgid / JSON manifest / bind / robot / handler；entity 退役 `_gen_wire.go` / `_gen_wire_test.go`（同包首实体退休时剩余实体接管 `RegisterEntity`）；nest 退役 wrapper / 两个 sender / guard test（`-sender=false` 退役 sender 半边）；attribute 退役 `gen_*_attribute.go`；eventgen 退役三份 `event_*_gen.go` 与孤儿 `_event_gen.go`（handler 仍引用退役事件时报错不删）；webroute 退役无路由的 `webroute_gen.go`。只删带各自生成头的文件，手写同名文件与改过生成头的文件保留。`roost generate` 的暂存快照 / `--check` 漂移把无 `Code generated` 注释的默认 `protocol.proto` / manifest 也纳入（`protocol.IsGeneratedArtifact`，internal 包）。
- **attribute `-output` 只允许单个 profile（RR-20260930-06）**：多个 profile 共用一个 `-output` 现在报错（此前后写覆盖前写）。
- **tablegen `_manifest.json` 升到 v2（RR-20260930-09）**：`{"version":2,"generated_at":…,"tables":{"<name>.json":"<sha256>"}}`，据此退役未改动的孤儿 JSON，已改动的报错要求人工处理，手写未登记的 JSON 保留。**旧 v1 manifest 目录若有当前 meta 未认领的 JSON 会明确失败**（v1 没有归属记录，不能判断是旧生成物还是手工数据）：v1 工程先在 meta 齐全时跑一次生成升级 manifest。新脚手架直接写 v2 空 manifest；`roost` 在 CSV 目录为空但 manifest 存在时不再跳过 config-data 步骤。
- **errcode 改为 Go AST 提取（RR-20260930-10）**：注释 / 字符串里的 `errcode.Define(...)` 示例不再入表；含候选文本但语法错误的文件现在报解析错误。
- **game-demo 平台模板（RR-20260929-23 / 32，write-once，`roost project sync` 不更新，已生成工程需手工合并）**：`internal/service/platform/collaborators.go` 的 `grantStore` 接口由 `HSet` 改为 `HGet` + `Eval`，首次 durable grant 单键 Lua 原子首写并核验身份，未绑定 / 未知商品 / 编码失败 wrap `platform.ErrDeliveryNotApplied`；新增脚手架步骤 `internal/service/platform/purchase_delivery_test.go`。混合旧 producer 仍会无条件覆盖字段，需升级全部投递 owner。

### Fixed — Core、生成链路与 CI（A 线）

- **integration 覆盖门禁识别 Redis Cluster 套件**：`kit/service/mail/batch_cluster_integration_test.go`（RR-20260929-33）以 `ROOST_REVIEW_CLUSTER` 准入，此前被门禁归为全环境套件、要求故障矩阵运行它，main 的 `ci` / `nightly` 因 `TestFaultMatrixScriptNamesEveryFullEnvironmentSuite` 一直红。现在门禁分三类：`REDIS_ADDR` → ci.yml Redis job，`ROOST_DATAENGINE_IT` → `dataengine-env.sh test`，`ROOST_REVIEW_CLUSTER` → 新增的手动入口 `kit/scripts/integration/redis-cluster-suites.sh`（CI 无集群，这些用例在 CI 里一律 skip，由 `TestRedisClusterScriptNamesEveryClusterKeyedSuite` 钉住包清单）。
- **`cache.AtomicLocalStore` 时钟记录随存活键数有界（RR-20260930-03，P2）**：此前覆盖写、Delete、过期都不回收时钟记录，键数远低于上限、永不淘汰的 Remote 快照 L1 缓存随写入次数线性增长（C01 24 小时堆涨到 4.7GB，v1.10.0 起即有）；现在超过 `2 × 存活键数 + 1024` 时就地压缩，准入规则与淘汰顺序不变。覆盖写约多 11ns、0 B/op。remoteflow 负载 harness 新增 `ROOST_REMOTE_HEAP_PROFILE_MINUTES`（默认关闭）。

- **生成的 player TCP 与 game-demo 场景连接测试先等会话登记（RR-20260928-15）**：生成的服务器先写认证 ack、再登记会话，`server_gen_test.go` 的 6 个用例与 `scene_connections_test.go` 的两个用例原来拨号后直接断言，高负载下偶发 `active sessions = 1, want 2`（framework-compat source-head lane）。只改测试；已生成工程 sync 更新 `server_gen_test.go`，`scene_connections_test.go` 是业务文件需手工补。
- **测试**：remoteflow 生成链路补 Remote 持久拒绝与收尾的端到端用例（REMAINING C34、B27 第 1 批）——装配改用正式 `ManagerAccess` 作 Remote loader 并接 entitysync，覆盖 Durability 0～3 持久拒绝后的卸载、重载全量与重载后可写，strict 确认截止后的延迟回调，结果未知后停机 / fence，嵌套 / 收尾阶段独立事务的拒绝，快 worker 删除 Remote 实体；`scripts/test-remote-generated.sh` 默认一起运行，新增 `ROOST_REMOTE_COUNT` / `ROOST_REMOTE_RUN`。

### Fixed — Service

#### account

- **建角补偿不再误删别人已提交的名字（RR-20260929-03，P2）**：名字 owner 只用 accountID，跨服同账号复用已提交名字，失败后释放的是他人角色的名字；现在 nameOwner 用 account/server slotKey，任何已提交的同 owner claim 都拒绝复用，失败只清理自身暂占 slot。旧 bare-account owner 名字保持原状。[记录](docs/bugfix/RR-20260929-03.md)
- **已验证身份键碰撞（RR-20260929-11，P2）**：channel/openID 的冒号边界未编码，两个不同身份可映射同一账号；现在转义 channel 中的 `%` / `:`，读旧 ID 只有 Channel/OpenID 与 verifier 结果一致才复用，不匹配拒绝而不合并。既有歧义碰撞需人工拆分。[记录](docs/bugfix/RR-20260929-11.md)
- **slot 写后响应丢失不再补偿删角色（RR-20260929-12，P2）**：`Update` 返回 error 被当作未落盘，角色 / 名字先删再用旧版本删 v2 的 slot；现在失败先 Get 核对 PlayerID/account/server，已应用返回成功，读失败返回角色与 unknown 错误保留数据，确认未提交才补偿。[记录](docs/bugfix/RR-20260929-12.md)
- **建角未知结果向前恢复（RR-20260929-19，P2）**：初始 Slots.Create / Roles.Create / Names.Commit 的错误都可能发生在写入后，旧流程留下空 slot、孤儿角色或无角色的 committed 名字。现在 slot 持久保存 `RoleCreation` 计划（随机 ID、预分配 PlayerID、名字、Claim、Admitted），同名重试共享计划与 PlayerID、幂等返回原角色；一旦 Admitted 不再删除可能已提交的角色，只有确定的建角前名字冲突或 foreign allocator ID 才补偿；未发布角色被 SelectRole / ValidateSession / UpdateProfile 拒绝。旧空 slot 无 CreationID 返回 ErrConflict 等对账，无后台扫描器。[记录](docs/bugfix/RR-20260929-19.md)
- **Profile 返回值不再别名 MemoryStore（RR-20260929-22，P3）**：UpdateProfile / ValidateSession / CreateRole 重放返回私有 clone，修改返回值不再绕过 CAS 改存储。[记录](docs/bugfix/RR-20260929-22.md)

#### mail

- **过期不可见条目不再占满邮箱（RR-20260929-04，P2）**：条目没有正文到期信息，列表隐藏正文但不回收 unread 与容量；新增 `Entry.EnvelopeExpiresAtUnix`，List / Deliver 的 CAS 内按已知期限回收条目、unread 并写 evicted 墓碑，在途未结算 claim 保留。旧条目期限 0 不推测过期。[记录](docs/bugfix/RR-20260929-04.md)
- **正文写入瞬时失败后同 RequestID 可恢复（RR-20260929-16，P2）**：send ledger 先提交但未保存意图，重试只看到缺失 ID；现在 `SentRecord.Intent` 保存独立克隆的 Envelope，同 RequestID 恢复固定 MailID / 正文 / 原期限并读回确认后投递。旧 ledger 无 Intent 且缺正文仍 ErrConflict。[记录](docs/bugfix/RR-20260929-16.md)
- **取消预约按代次校验（RR-20260929-20，P2）**：稳定 Token 表示同一奖励，旧 `CancelClaim` 仅按 Token 清 Deadline，迟到的旧取消会解除新一次预约；现在 CAS 内同时匹配 Token 与 ClaimAttempts，旧代次取消返回 false、重复取消 no-op，`CommitClaim` 仍按稳定 Token 确认。Go API 变化见上节。[记录](docs/bugfix/RR-20260929-20.md)
- **未领取直接删除的邮件不再被重投复活（旧 RR-20260910-02 残余，P2）**：复用 SettledClaims 墓碑加 `deleted` 标记，淘汰 Entry 时保留删除身份与 envelope expiry；Deliver 不复活、Reserve/Commit 维持 Missing，容量不足拒绝新投递，已到期证明可回收。所有 mailbox owner 一起升级，旧节点会按 legacy 策略丢掉新删除证明。[记录](docs/bugfix/SERVICE-BUGFIX-2026-09-29-04.md)
- **Cluster 下多封分页不再 CROSSSLOT（RR-20260929-33，P2）**：普通 prefix 下单 key CAS 都能成功，信封页的多 key `MGET` 却要求同槽；`GetMany` 改为经既有 `IPipeline` 排队单 key GET、一次 Exec、逐 future 取结果，`ErrNil` 才是缺失，空字节 / 坏 JSON / WRONGTYPE 明确报错。键、JSON、TTL、`EnvelopeStore` / RPC 不变；不强制共同 tag。[记录](docs/bugfix/RR-20260929-33.md)

#### platform

- **迟到发货与人工结算按 attempt 身份归档（RR-20260929-01，P1）**：计时 backoff 耗尽不等于外部调用已停止，旧成功回包无条件写 delivered；现在 Order 持久化单调 `AttemptSequence` 与 `PendingAttempts`，结算 / 重开拒绝未知在途结果，回包按 attempt 归档；新增 owner-only `ResolvePendingAttempts`，人工确认全部外部请求已停止并对账后才清理。旧订单没有 PendingAttempts，不能凭零值证明无在途请求。[记录](docs/bugfix/RR-20260929-01.md)
- **外部发货未知错误不再清除 pending 允许再发货（RR-20260929-23，P1）**：新增 `ErrDeliveryNotApplied`，只有 collaborator 明确证明无外部效果才移除当前 pending 并按原预算重试；其他错误保留 PendingAttempts 进入 exhausted 停止自动重试，退避到期但仍有在途证明时同样转人工，迟到成功仍能完成原 generation。`DeliveryExhausted` 现在也表示结果待对账，Attempts 可能小于 MaxAttempts。[记录](docs/bugfix/SERVICE-BUGFIX-2026-09-29-04.md)
- **重复 HandleCallback 的 receipt 不再共享切片（RR-20260929-24，P3）**：构造 receipt 时 clone Order（含错误分支），修改 PendingAttempts 不再绕过 MemoryStore CAS。[记录](docs/bugfix/SERVICE-BUGFIX-2026-09-29-04.md)
- **缺失订单索引原子退休（RR-20260929-28，P1）**：迟到 ghost 退休的无条件 ZREM 会删掉新 paid 订单的 pending 入口；`RetirePending` 改用 `versionstore.RedisStore.IndexRemoveIfAbsent`，值 key EXISTS 才决定是否移除，同一段 Lua。原无条件 API 保留；旧丢索引订单不自动修复。[记录](docs/bugfix/RR-20260929-28.md)
- **Cluster 有效 tag 准入（RR-20260929-30，P2）**：原“包含左括号”判断替换为 `mods.ValidateClusterKeyPrefix`，空 / 未闭合首 tag 启动失败。[记录](docs/bugfix/RR-20260929-30.md)
- **game-demo 购买首次 durable grant 保留（RR-20260929-32，P2）**：稳定 OrderID 不能使可变 grant 内容幂等，catalog 升级后重试把 Count10 覆盖为 3；`grantDeliverer.Deliver` 先 HGet 已有字段核对身份恢复，只有 ErrNil 才查商品并经单键 Lua 原子首写，始终返回 durable winner 并检查身份；读取错误 / 写后回复丢失 / 坏内容一律不带 `ErrDeliveryNotApplied`。只改 demo 模板与生成测试，不改 Platform RPC / 订单 JSON。[记录](docs/bugfix/RR-20260929-32.md)

#### activity（kit/service/global/activity）

- **未确认进度不再被 ring 淘汰重复计入（RR-20260929-02，P1）**：最近 32 次 ring 会淘汰已应用但 ledger 尚未确认的请求；现在单独保存最多 32 个 `PendingRequestIDs`，未确认不淘汰、满则背压，Applies 快照 CAS 拦住迟到的 reserved 读者。旧 ring 升级保守转 pending；不能与旧写者混跑。[记录](docs/bugfix/RR-20260929-02.md)
- **最后一次 ACK 窗口不再被重试提前关闭（RR-20260929-08，P2）**：`AttemptDispatch` 先判断 NextAttemptAt 是否到期，最后一个 token 的 ACK 期限内返回 `ErrDispatchNotDue`，期限过后再耗尽预算。[记录](docs/bugfix/RR-20260929-08.md)
- **非负累加不再回绕为负（RR-20260929-15，P2）**：participant CAS 内检查 Score/Progress > MaxInt64-delta 或 Applies==MaxUint64，拒绝 `ErrRequestInvalid` 并保留旧状态。[记录](docs/bugfix/RR-20260929-15.md)
- **Opening 名额持久恢复、不再按超时回收（旧 RR-20260914-02 残余，P2）**：两个独立 CAS 无法证明已开始的 Create 在 grace 后不会落库；`OpeningEntry.Intent` 保存完整创建计划，Create 错误 / 超时不释放已承诺名额，超过 OpeningGrace 的 sweep 帮助同计划创建与确认，第 257 次准入被拒；旧超容量 Window 用持久 `ScanAfter` 有界轮转（每批 ≤256）。全部 owner 升级并排空旧调用后再启用；不猜测无 Intent 的 legacy 意图。[记录](docs/bugfix/SERVICE-BUGFIX-2026-09-29-05.md)
- **Cluster 前缀准入（RR-20260929-31，P2）**：`Mod.Init` 调用 `mods.ValidateClusterKeyPrefix`，无 tag / 空 tag / 未闭合 / 空首对配置启动拒绝并点名 `activity.key_prefix`；多 key dispatch / owed 原子写需要同槽。不迁移历史错误前缀。[记录](docs/bugfix/RR-20260929-31.md)

#### rank

- **带逗号的 requestID 可去重（RR-20260929-05，P2）**：逗号连接的字符串无法表示含分隔符的请求；新 `applied_v2` 字段存 JSON 数组，读取兼容 legacy，下一次写入升级，损坏账本 fail-closed。[记录](docs/bugfix/RR-20260929-05.md)
- **并发 no-op 不再覆盖幂等记录（RR-20260929-06，P2）**：只 CAS member 时分数不变检测不到去重 ring 的并发变动；Lua 同时比较 member 与完整 tagged ring，no-op CAS 失败也退避重读，Remove 同时删两个格式字段。高竞争可返回 ErrConflict。[记录](docs/bugfix/RR-20260929-06.md)
- **Around 最大半径不再被页上限拒绝（RR-20260929-10，P3）**：radius=100 被接受但内部 Page limit=201 被拒；Around 委托 Page 时限制到 MaxPageSize=200，极限结果窗口远端少一条。[记录](docs/bugfix/RR-20260929-10.md)
- **加法溢出拒绝落库（RR-20260929-26，P2）**：nextScore 在 int64 加法溢出时返回 `ErrScoreInvalid`，发生在 Lua CAS 与 ring 写入前，不消耗幂等键；已溢出的历史分数不自动推导。[记录](docs/bugfix/SERVICE-BUGFIX-2026-09-29-05.md)
- **Cluster 前缀准入（RR-20260929-29，P2）**：`Mod.Init` 使用 `mods.ValidateClusterKeyPrefix`，不改键格式或迁移旧榜；直接构造器调用者自行保证同槽。[记录](docs/bugfix/RR-20260929-29.md)

#### match

- **Grouping 非法 Queue 不再 panic / 伪成功（RR-20260929-25，P2）**：FIFO / ScoreWindow 的公开 Group 先 `Queue.Validate()`，非法模式 / 大小返回既有 `ErrQueueInvalid`。[记录](docs/bugfix/SERVICE-BUGFIX-2026-09-29-05.md)

#### session

- **后台 sweep 有了公开 owner 接线（RR-20260929-09，P2）**：私有 `sweepOwners` 始终 nil，部署无法提供清理 owner 集合；新增 `Config.Owners` / `OwnerSource` / `SweepPending`，kit `NewMod` 接受 `WithSweepOwners`，Server 每 tick 调用。未配置时 `BackgroundSweepEnabled=false`，不扫描全 keyspace。[记录](docs/bugfix/RR-20260929-09.md)
- **Attach 拒绝伪造的释放字段（RR-20260929-14，P2）**：调用者填 `ReleasedAtUnix` / `ForcedRelease` 可让 Finish 跳过 Releaser；现在任何非零值返回 `ErrRunInvalid`，管理字段只由释放流程写入。[记录](docs/bugfix/RR-20260929-14.md)
- **Finish 重试成功后释放 claim（RR-20260929-18，P3）**：终态分支 `releasePending` 后直接返回不执行 `releaseClaim`；现在资源全部释放成功才调用身份校验的 releaseClaim，失败继续保留 claim 供恢复。[记录](docs/bugfix/RR-20260929-18.md)
- **正常 Finish 的 claim 删除按逻辑身份原子校验（旧 RR-20260909-02 残余）**：run 置 terminal 后，旧 releaseClaim 的 Get/RunID 校验与普通 Delete 之间仍可被另一个 Enter 删除重建为同版本新 claim（v1 ABA）；新增 `versionstore.ConditionalDeleter.DeleteIf`（Memory 锁内校验，Redis Lua 比对完整原始字节后删值与索引），`ClaimStore` 显式要求该能力，RunID/OwnerID 校验落入删除原子边界。[记录](docs/bugfix/RR-20260909-02.md#2026-09-29-正常-finish-残留补修)

#### chat

- **自定义频道键不再碰撞私聊（RR-20260929-07，P2）**：Kind 原样拼接冒号，shared 类型可产生 private pair 的同一存储键；现在对 Kind 的 `%` / `:` 做可逆转义，默认频道键不变。已发生的泄露不能靠新代码撤销。[记录](docs/bugfix/RR-20260929-07.md)
- **时钟偏移不再漏清理、Gap 报告页内洞（RR-20260929-27，P3）**：Prune 遍历有界 Ring 按每条 StoredAtUnix 清理、最多 limit；pageOf 同时报告页内 / 游标边界及清空尾部缺失，Next/PrevCursor/HasMore 含义不变。所有历史读取 owner 一起升级。[记录](docs/bugfix/SERVICE-BUGFIX-2026-09-29-05.md)

#### directory

- **Cancel / Release 原子校验身份（RR-20260929-21，P2）**：外层 Token/Owner 检查与普通版本 Delete 分离，删除重建后版本重复，旧删除会移除新条目；改用 `ConditionalDeleter.DeleteIf`，Cancel 原子匹配 Token / Owner / reserved，Release 原子匹配 Owner / Token / State；`New` 拒绝不支持身份删除的 store。[记录](docs/bugfix/RR-20260929-21.md)

#### global（kit/service/global）

- **迁移后旧路由 lease 不可续期（RR-20260929-13，P2）**：续期只检查 incarnation，不核对当前 route 的 epoch/global/group；Acquire/Renew 依据 route 快照 CAS 并写后再核验，Lease/LiveGames 同样核验当前绑定。跨模块消息仍须携带 epoch fencing。[记录](docs/bugfix/RR-20260929-13.md)
- **lease 返回的 Load map 不再污染 MemoryStore（RR-20260929-17，P3）**：Renew/Lease/LiveGames 返回 cloneLease，Load map 独立。[记录](docs/bugfix/RR-20260929-17.md)

### Fixed — Redis driver

- **Pipeline 缺失回复不再掩盖写错误（RR-20260929-34，P2）**：GET 缺失 key 在前、HSET 在后时 go-redis aggregate error 是 `redis.Nil`，wrapper 忽略后不再检查后续命令，`Exec` 返回 nil 而 HSET 实际被 WRONGTYPE 拒绝；现在 `redis/driver/pipeline.go` 先完成全部 future 赋值、保留真实 aggregate / 传输错误，aggregate 为 nil 或 Nil 时逐命令返回首个非 Nil 错误。仅缺失读取仍允许 Exec 为 nil，由对应 future 返回 `ErrNil`；部分执行不回滚。更新二进制即可，无数据迁移。[记录](docs/bugfix/RR-20260929-34.md)

### Fixed — Codegen

- **servicerpc 删除接口标记后 `-check` 不再假通过、孤儿传输文件退役（RR-20260930-01，P2）**：零接口时直接返回、只比较当前所需文件；现在先生成预期集合再扫描输出目录，只认带自身生成头且记录同一 regenerate 命令的 `_rpc_gen.go` / `_rpc_assembly_gen.go`，`-check` 报 `(orphan)` 并失败，普通运行删除。跨包 `-emit assembly -out` 只清装配半边。[记录](docs/bugfix/RR-20260930-01.md)
- **protocol 清空定义后旧产物退役、`roost generate` 真正提交删除（RR-20260930-02，P2）**：定义集合为空时退役可识别的 proto / PB / msgid / JSON manifest / bind / robot / handler 文件（配置了 player bootstrap 则先验证再写不引用旧 handler 的 bootstrap），定义非空时也按当前 handler 域集合清理消失的域；`codegen/internal/roost` 的暂存快照与 `--check` 通过 `IsGeneratedArtifact` 纳入无 `Code generated` 注释的默认 `protocol.proto` / manifest。[记录](docs/bugfix/RR-20260930-02.md)
- **entity 删除最后一个 `//roost:entity` 后退役 wire 与 guard test（RR-20260930-04，P2）**：原来只扫描当前含标记的目录；现在带 `tool/entity` 生成头的旧 wire / companion test 也使其目录进入对账，同包首实体退休时剩余实体接管 `RegisterEntity`，显式 `-output` 只退役指定文件。[记录](docs/bugfix/RR-20260930-04.md)
- **nest 删除最后一个 `//roost:nest` 后退役四类产物（RR-20260930-05，P2）**：wrapper、异步 / 同步 sender、sender guard test 按本次预期集合对账，`-sender=false` 退役 sender 两半；清理在扫描与 bootstrap 成功后进行。[记录](docs/bugfix/RR-20260930-05.md)
- **attribute 删最后一个 profile 后退役旧实现（RR-20260930-06，P2）**：生成成功后扫描同目录 `gen_*_attribute.go`，只删带 `tool/attribute` 生成头且不在当前集合的文件；多个 profile 共用一个 `-output` 现在报错。[记录](docs/bugfix/RR-20260930-06.md)
- **eventgen 删除最后定义 / handler 后退役旧类型与派发（RR-20260930-07，P2）**：零定义时先验证 `-game` 范围内 handler 仍引用退役事件则报错不删，通过后删三份固定文件；`handler.go` 对当前 receiver 集合与 `_event_gen.go` 对账删除孤儿。[记录](docs/bugfix/RR-20260930-07.md)
- **webroute 删除最后标记后退役 `webroute_gen.go`（RR-20260930-08，P2）**：只删带 `roost webroute` 生成头、当前包已无路由的文件；扫描根目录不再因名称以点开头被跳过。[记录](docs/bugfix/RR-20260930-08.md)
- **tablegen 元数据清空后旧 JSON 退役并记录所有权（RR-20260930-09，P2）**：`_manifest.json` 升至 v2 记录输出文件名与 SHA-256，未改动的孤儿才删，手写未登记的保留，已改动的报错；JSON 文件名限制为输出目录直接子文件。旧 v1 manifest 遇未认领 JSON 明确失败要求人工确认。[记录](docs/bugfix/RR-20260930-09.md)
- **errcode 只提取真实 `errcode.Define` 调用（RR-20260930-10，P3）**：改用 Go AST，注释 / 字符串内的示例不再入 CSV；含候选文本但语法错误的文件明确报解析错误。[记录](docs/bugfix/RR-20260930-10.md)

### 其他

- `AGENTS.md` 新增一段：用户明确请求修复 review/bug 时使用 [roost-bugfix](docs/agent-skills/roost-bugfix/SKILL.md) skill；普通 roost-review 仍只审查记录（提交 `83c04243`）。
- 正式回归全部进包（`rr_20260929_round{1,2,3}_test.go`、`bugfix_*_test.go`、`retirement_test.go` 等），最终 service+driver integration/race 19 包 1035 pass 事件（3 个 Toxiproxy 用例因环境缺失 skip），见各批记录。未做真实 Broker 发奖 / Redis HA / 多进程强杀 / 生产迁移验证。

## [v1.17.2] - 2026-09-28

> v1.17.1 之后按 [残留清单 OPEN-ITEMS-2026-09-27](docs/review/OPEN-ITEMS-2026-09-27.md) 逐条处理的修复（RR-20260927-01～35，08 未使用；RR-20260928-01～14）与补测。**v1.17.1 生成的生产 compose 与镜像无法启动（RR-20260927-33 / 34），用这两者部署的工程升级后执行 `roost project sync`。** 生成器输出要求 roost-core ≥ v1.17.2（生成的 syncbus 测试用到 `kit/syncbus.JetStreamStreamFromConfig`）。
> 第五、六轮独立审计见 [audit5](docs/review/REVIEW-2026-09-27-audit5.md)、[audit6](docs/review/REVIEW-2026-09-27-audit6.md)。

### Changed（行为收紧 / 需要注意）

- **Nest fence 之后拒绝外层提交（RR-20260927-06）**：引擎已 fence 时，仍在执行的消息自己的事务在交给 committer 之前返回 `nest.ErrNestFenced` 并回滚（此前结果未知来自 `AcceptMutation` 失败时 WAL 未 terminal，外层记录照常写入）。别的消息 fence 了引擎时，在途消息的提交同样被拒。
- **嵌套独立事务的裸 mutation 同样受 RR-74 约束（RR-20260927-07）**：外层 state / undo 事务中，`RunIsolatedTransaction` 里用 `AddMutation` 直写、按实体 ID 命中外层已快照实体的写入返回 `ErrNestedTransactionRollbackConflict`，什么都不提交。
- **handler 内新建的两处收紧（RR-20260927-11 / 21）**：`CreateInScope` 捕获失败时整条事务失败（业务吞掉错误也回滚、不重排）；同一 handler 里再建本 handler 自己撤销的同 ID 为确定失败（`errors.Is(entity.ErrEntityRemoved)`、不重排，此前可回滚事务空转到重排上限）。USER_GUIDE §4 判别表新增第 12 行。
- **Remote 托管实体 + sid 作用域 DAO 在提交路径拒绝（RR-20260927-09）**：`ValidateEntityRegistry` 与 Remote 事务进 WAL 前（`FinalizeLocked`）都拒绝该组合，错误满足 `errors.Is(entity.ErrRemoteManagedServerScopedDAO)`（`remoteentity` 同名哨兵为同一个值）。
- **同 fence 下拒绝 Remote 版本向量回退（RR-20260927-15）**：`SetRemoteVersionVector` 遇同 fence、更小 StateVersion 返回 `ErrRemoteVersionConflict`；只影响非 authority 兼容装配，迟到重放按过期处理。
- **strict 等 Remote 确认截止的回复同时满足 `ErrRemotePersistenceIndeterminate`（RR-20260927-24）**：兑现 RR-37 承诺；`ErrRemoteCommitTimeout` 与 `context.DeadlineExceeded` 保留，只增加 `errors.Is` 命中。
- **`RegisterEntityKindDefs` / `RegisterEntityKindCategories` 整批校验后再写入（RR-20260927-10）**：出错不留半批。
- **撤销新建实体改用 `entity.DestroyReasonCreateRevoked`（RR-20260927-12）**：此前为 `DestroyReasonCommon`；按原因分支的业务销毁回调需识别新值。
- **`ReleaseCast` 按实例释放（RR-20260927-26）**：handler 内 Destroy 后同 ID 重建时，`ReleaseCast(旧实例)` 不再放掉新实例的锁；新增 `EntityGuard.ReleaseEntityInstance`，`ReleaseEntity(id)` 语义不变。
- **非 Nest 持锁领头方冷加载时发布交回领头方 goroutine（RR-20260927-27）**：loader 发布要锁领头方已持有的实体时不再永久死锁，领头方按自己的 ctx 离开恢复成立。
- **entitysync 注销按当前登记 / 按实例（RR-20260927-22 / 28）**：`Unregister` 取锁后确认表项，旧 subject 已被 forget 时注销同 ID 的当前登记；`RetractSyncSubject` 在 `subj.mu` 内比对状态、只撤回传入的那个状态。
- **生成的 player TCP Mod 声明停机预算（RR-20260927-05，需 `roost project sync`）**：`StopBudget = player_access.tcp.shutdown_timeout`，托管 TCP 的服务 `shutdown.total_timeout` 与宽限期 +7s（game-demo 缺省 Mod 集 game 107s / 112s → 114s / 119s）；未手改的 `shutdown:` 段经 sync 自动更新，手改的看 doctor 提示。
- **game-demo 玩家 id 计数键移到 `<account.key_prefix>:player_id`（RR-20260927-03）**：首次分配时以旧键 `roost:demo:player_id` 的值为起点（旧键只读不删）；升级时先停掉全部 account 进程，回滚前要把旧键手工设为新键的值（见 demo/README）；已生成的 `collaborators.go` 需重新生成或手工合并。
- **game-demo 场景接卸载后重载与 `OnEntityLoaded → Rebind`（RR-20260927-18 / 23）**：玩家被仅内存卸载（原生步骤投影被 lease fence 跳过）后观察者收到权威全量；无人观看时卸载、之后被业务重载的玩家绑回场景；`NewScene` 在缺少实体运行时或 DataEngine `OnEntityLoaded` 时返回错误。场景文件 `internal/service/game/scene.go` / `scene_test.go` 是 demo 脚手架只写一次的应用文件，**`roost project sync` 不会更新**：已生成工程要按新模板手工合并（RR-20260927-19 的计数同理）。

- **生成的生产部署物可以启动了（RR-20260927-33 / 34，RR-20260928-04 / 05，需 `roost project sync`）**：compose 的 `tmpfs` 改为单条挂载（此前被 YAML 拆成 4 项，所有服务起不来）；生产镜像自带 `configs/data`（此前容器启动即 `stat dir configs/data` 失败并反复重启）；
  镜像 / compose / k8s / systemd 给相对的 `stats_log.dir` 一个可写位置，stats_log 写失败记 WARN 与 `stats_log.write_failures`（此前静默不落盘）；shell / systemd 安装把 `configs/data` 装进 release，**unit 的 `WorkingDirectory` 改为 `$APP_ROOT/current`**（数据随 rollback 回退），缺数据时在创建 release 之前拒绝，新增可选环境变量 `CONFIG_DATA`。
- **game-demo 生产示例与 k8s Secret 示例补齐（RR-20260928-06 / 07，需手工合并）**：prod 示例与 Secret 示例写出 `game_route` / `activity` / `platform`（`payment_secret: CHANGE_ME`）；Secret 示例与 prod 示例同源，`add mod` / `add saga` / `add transport tcp` 事后追加的段也进 Secret（此前按 Secret 部署缺 `saga:`，saga 按 8 GiB 默认建流失败）。这两份是脚手架，sync 不更新，已生成工程与仓库外的真实配置须手工补齐。
- **新增 `nest.ErrRemotePartRejected`，判别表改号（RR-20260928-03）**：带 Remote 批次的消息本地已提交、Remote 部分被明确拒绝时回复带该哨兵（部分已提交、不得整笔重试）；USER_GUIDE §4 判别表插为第 4 行（原 4～12 顺延为 5～13），新增第 14 行（`ErrNestCanceled` / `ErrNestTimeout`：调用方等待截止，结果未知）与第 15 行兜底（未提交）。
- **提交前的明确拒绝统一带 `nest.ErrCommitRejected`（RR-20260927-32）**：Remote 批次定稿拒绝、fence 后拒绝、嵌套写快照拒绝、准备提交记录失败等；只增加 `errors.Is` 命中，回复文本多一行前缀。
- **Cast 捕获失败强制整条事务失败（RR-20260927-31）**：与 RR-20260927-11 同一机制，业务吞掉错误也回滚、不重排。

- **pipelined 回退到 strict 路径时等 fsync（RR-20260928-11，行为变化）**：broadcast 与带 Remote 批次的 pipelined handler 回退到 strict 提交、经 `WAL.Append` 写入，此前 `Append` 只对 strict 等 fsync，这些记录实际是 async 语义（锁释放、Sync Confirm、AfterCommit、回复可能先于持久）；
  现在与 strict 相同，在锁内等一次组提交 fsync。同机对照 broadcast pipelined 吞吐 −21%、p50 +28%，与 broadcast strict 持平；快路径（`Enqueue`）不变。
- **pipelined + Remote 明确拒绝与 strict 一致（RR-20260928-09）**：带 Remote 批次的 pipelined handler 被持久明确拒绝时交 finalizer 回滚、隔离、仅内存卸载并从权威重载，Sync 门放行（此前实例一直隔离、重启前不可写，Sync 门永久冻结）。
- **strict / pipelined 的 Remote 等待失败除明确拒绝外都带 `ErrRemotePersistenceIndeterminate`（RR-20260928-08）**：tracker 被淘汰后重新登记报 `ErrRemoteOverloaded` 的回复不再误标 `ErrRemotePartRejected`（判别表第 4 行），改落第 2 行；本地已提交且嵌套事务也已提交时外层文案为 `nest: a nested isolated transaction also committed`（`errors.Is` 不变）。
- **shell / systemd 回滚连同 unit 一起回退（RR-20260928-10，需 `roost project sync`）**：每个 release 的 unit 记在 `$APP_ROOT/units/<version>.service`，install.sh 的自动回滚与 `rollback.sh` 装回目标 release 的 unit 并 `daemon-reload`（此前从 RR-20260928-05 之前安装的 release 升级失败时回滚报 `rollback also failed readiness`）。

- **shell / systemd 切换 release 先按当前 unit 停机（RR-20260928-12，需 `roost project sync`）**：install.sh 与 rollback.sh 先 stop（当前 unit）→ 切 current → 装入目标 unit → start，正在运行的版本按它自己的 TimeoutStopSec 停机；目标 unit 装不上时恢复 current 与 unit 并非零退出；rollback.sh 拒绝版本号 `.` / `..`。
- **CRLF 的 k8s Secret 示例与 LF 同样编辑（RR-20260928-13，行为变化）**：`add mod` / `add saga` / `add transport tcp` 按原行尾写回，sync 的停机块刷新支持 CRLF；认不出 Secret 结构时各命令统一在 stderr 打印 WARN 并照常完成——`add transport tcp` 遇到合并不了的 Secret 不再整体失败。

### Added

- 配置：`nest.entity_load_timeout`、`nest.unload_resync.{workers,attempts,queue_capacity}`（RR-20260927-13，缺省不变，负值拒绝启动）；`remote_entity.snapshot_l2_key_prefix`（RR-20260927-17，缺省空时键不变，设置或修改需整体重启）。
- API：`remoteentity.NewSnapshotL2StoreWithKeyPrefix`、`ValidateSnapshotL2KeyPrefix`、`Config.SnapshotL2KeyPrefix`；`entity.ValidateRemoteManagedDaoScopes`、`entity.ErrRemoteManagedServerScopedDAO`、`entity.DestroyReasonCreateRevoked`、`EntityGuard.ReleaseEntityInstance`、`UnloadResyncStats.Backlog`。
- API：`nest.ErrRemotePartRejected`（RR-20260928-03）、`kit/syncbus.JetStreamStreamFromConfig`（RR-20260927-35）。
- 指标：`stats_log.write_failures`（RR-20260928-04）；`entity.unload_resync.backlog`（RR-20260927-14，RR-20260928-01 起为进程内全部 ManagerAccess 之和）、`saga.step_inbox.mark_completed_error_total`（RR-20260927-16）、game-demo `scene_session_reopen_failed_total{reason}`（RR-20260927-19）。

### Fixed

- **生成器 / doctor**：Windows CI 上 RR-80 写失败用例（RR-20260927-01）；生成的 `Runtime.CloseSessions` 返回实际关闭数（RR-20260927-02，需重新生成）；`deploy/dev/run.sh` 登记游戏服传 `redis.db` / `redis.password`（RR-20260927-03）；
  doctor 对 0 或负的 `total_timeout` / `dataengine.shutdown_timeout` 按运行时 30s 判定，“Set it to”按配置的 dataengine 预算、各份不同时逐文件给出（RR-20260927-04）。
- **其他**：不可比较的自定义 `lock.Mutex` 不再 panic（RR-20260927-25 / 30）；共享冷加载领头方以任何方式离开都登记离开，迟到 `RunLocal` 不再永久阻塞（RR-20260927-29）；Guard 撤销记录按（EntityManager, ID）判定（RR-20260928-02）；demo 模板测试流名解析（RR-20260927-35）；saga 收件箱 claim 标记失败记 Warn 与计数（RR-20260927-16）；卸载后重载最坏延迟写成真实上界，默认约 43 小时（RR-20260927-14）。
- **测试与卫生**：`codegen/internal/roost` 包测试按参数缓存生成工程、慢用例并行，本机 265s→73s，修复 Windows CI 超时（RR-20260928-14）；nest 单用例可 `-count>1` 重跑（RR-20260927-20）；B40 同 ID 新建用例时序修正；仓库根不再跟踪 `glsvet` 二进制；32 个文件 gofmt（全仓 `gofmt -l` 为空）；`entity.ResetEntityRegistryForTest` 标 Deprecated；
  补测收为回归：B02 / B04 / B08～B10 / B13～B19 / B21 / B22 / B24 / B26 / B37，均见清单。
- **文档**：USER_GUIDE §4 判别表补 `ErrRemoteCommitTimeout` 并更正收尾阶段表述；`ErrNestedTransactionInRemoteMessage` 的 `PrepareRemoteWriteBatch` 窗口；RR-54 包装 Getter 契约；kit/README 的 `roost.room` / `roost.sync` 共用 `ROOST_SYNC`；多份修复记录追加更正与关闭说明。

## [v1.17.1] - 2026-09-27

> v1.17.0 之后的全部修复（RR-20260926-30、33～85）。生成器输出要求 roost-core ≥ v1.17.1（生成的 scene 桥接用到本版新增的 entitysync / policy API）。三轮独立审计的结论见 [修复合并后审计](docs/review/REVIEW-2026-09-26-audit.md)、
> [第二轮](docs/review/REVIEW-2026-09-27-audit2.md)、[第三轮](docs/review/REVIEW-2026-09-27-audit3.md)。升级前先读下面“Changed”。

### Changed（破坏性 / 行为收紧）

- **syncbus JetStream 流名随 prefix 派生（RR-20260926-56，需迁移）**：默认 prefix（`roost.sync` / 未写 prefix 的 `roost.room`）与显式写了 `syncbus.stream` 的部署，流名和 durable 游标都不变。
  写了非默认 prefix、没写 `stream` 的部署升级后改用派生流名；若旧 `ROOST_SYNC` 仍持有该 prefix 的 subjects，启动时报 subjects overlap 并失败（不会静默）。
  迁移：同一 prefix 的多个实例**不能滚动升级**——要么先给全部实例配 `stream: ROOST_SYNC` 沿用旧流，要么全停后升级；共用 NATS 时不要删 `ROOST_SYNC`，只从中移除本 prefix 的 subjects；
  清理旧流上本部署留下的孤儿 durable consumer。kit/README 中“不同 prefix 即各有各的流”对兼容映射的 `roost.room` / `roost.sync` 不成立。见 [修复记录](docs/bugfix/RR-20260926-56.md)。
- **生成 TCP 传输：写失败即断开该连接（RR-20260926-52/68，需重新生成）**：一次推送写失败的连接由服务端关闭，客户端表现为断线重连；`write_timeout`（默认 5s）也是“慢到这个程度就断开”的阈值。
  `PushPlayer` 部分连接失败、其余收到时返回 `nil`。调用方截止在第一个字节写出前到期的推送按“写前拒绝”处理，不关闭健康连接（RR-68）。
  已生成工程执行 `roost generate` 覆盖 `internal/access/player/tcp/server_gen.go`。
- **停机总时长与部署宽限期按服务实际 Mod 生成（RR-20260926-42/51/66）**：Mod 可声明停机预算（dataengine 声明 `dataengine.shutdown_timeout`），App 在 `shutdown.total_timeout` 内先按声明值分配，未声明的 Mod 各保底 3s。
  生成器按每个服务注册的 Mod 计算 `total_timeout`（game-demo 缺省 Mod 集：game 107s，框架服务 23s），k8s / compose / systemd / dev 脚本的宽限期取 `max(公式值, 配置里实际生效的 total) + 5s`，
  不会低于配置；`roost project doctor` 在模板宽限期低于配置 total + 5s 时 FAIL。RR-66 之前生成、仍为统一 60s 的工程重新 sync 后宽限期保持 65s，Mod 多的服务应按 doctor 提示调大 total。见 [DEPLOYMENT §8](docs/DEPLOYMENT.md)。
- **注册期拒绝矛盾的 Remote 策略（RR-20260926-45/60/71）**：`remote=managed` 实体不能使用 `dbscope=sid` 的 DAO（生成期与装配期双重校验）；冷加载与构建按 kind 在注册表里的实际 Remote 策略判定；
  手写 builder 先按 none 注册、kind 定义后声明 managed / mirror 且生命周期矛盾时，`RegisterEntityKindDefs` 返回错误、`MustRegisterEntityKindDefs` 在启动期 panic（此前静默接受）。生成工程不受影响。
- **Nest 不再重排可能已提交或不可回滚的消息（RR-20260926-49/64/65）**：越过提交点的事务、handler 内嵌套独立事务已提交（或结果未知）的消息、不可回滚 handler 内新建实体遇锁冲突的消息，
  都不再自动重新准入，回复分别带 `ErrAfterCommitFailed` / `nest.ErrNestedTransactionCommitted` / `nest.ErrCreatedEntityLockConflict`。带任一“可能已提交”哨兵的回复不得重试整笔业务。
  事务内新建实体按 Cast 锁序取锁，可回滚事务冲突时整条回滚后重排（RR-48）。纯本地 strict 已提交后的释放 / 回调错误同样带 `ErrAfterCommitFailed`（RR-53）。
- **Remote 结果未知与拒绝（RR-20260926-37/39/58/61/62）**：确认无结论时 Sync Confirm 与 AfterCommit 交给 finalizer，拿到持久结论后执行一次；持久结论到达时 Nest 已停机 / 已 fence、快池拒绝投递，
  则该事务的 AfterCommit **不执行**（计数 `remote_entity.deferred_outcome_not_run_total` 并告警），需要可靠副作用的业务应改用持久记录 / outbox。
  持久拒绝后框架自动仅内存卸载被拒绝的实例，下一次访问从权威重载，重载窗口内返回可重试的 `entity.ErrRemoteEntityReloading`（包裹 `ErrRemoteFenced`）；
  混合事务 Remote 部分被拒绝时只丢弃 Remote 实体的 Sync 内容，已提交的本地实体照常生效。
- **本地 lease fence 跳过（RR-20260926-30）**：saga 原生步骤因 lease fence 过期被跳过时，投影对同实体后续记录设屏障，跳过后驱逐该实体、从持久层重载，Sync 对原订阅者强制全量；不再 fatal、不再重启起不来。
- **卸载后重同步（RR-20260926-59/69/70/72）**：实体被仅内存卸载后仍有订阅者时，框架在快池之外从权威重载并 Rebind 全量，重载不了才对订阅者发 remove；政策（Interest / AOI / Group / Direct）撤销的订阅在实体重新登记后自动重新提交。
- **WAL**：fsync 失败后 terminal 粘滞，Ack / ticker / Sync 不再刷段、不推进 checkpoint 与 DurableLSN，票据错误包 `ErrCommitIndeterminate`（RR-20260926-33）；
  最后一段的零填充尾部按四条件判据截断（指标 `nestwal.recovery.tail_truncated.total`），其余仍 `ErrCorrupt`（RR-41）。

- **不能回滚的 handler 开始执行后不再自动重排（RR-20260926-73）**：memory handler（含带 Remote 批次的）开始执行后遇到锁超时 / 组迁移类错误只执行一次，回复带 `nest.ErrNonRollbackNotRequeued`（原因链保留）；准入阶段的失败照常重排。
- **嵌套独立事务的三条收紧（RR-20260926-74/75/76）**：`RunIsolatedTransaction` 要持久写外层可回滚事务已快照的实体时，在写任何持久记录前返回 `nest.ErrNestedTransactionRollbackConflict` 并自身回滚；
  消息带 Remote 批次时直接返回 `nest.ErrNestedTransactionInRemoteMessage`，函数体不执行，Remote 批次只随消息自己的事务收尾；嵌套事务提交结果未知时，框架在返回业务之前 fence 引擎（与消息自身事务同一入口），不依赖业务是否传回错误。
  判别规则：回复带任一“可能已提交”哨兵即不得重试整笔业务，只看 `ErrAfterCommitFailed` 不够，见 USER_GUIDE §4 判别表（RR-77）。
- **`dataengine.shutdown_timeout` 不参与生成（RR-20260926-77 更正文档）**：生成公式按 30s 计，只调大它不会改变生成的 `total_timeout` 与宽限期，须同时手动调大 `shutdown.total_timeout` 再 sync。

- **收尾阶段的独立事务不再认领消息（RR-20260926-84）**：独立事务是否属于消息改按入口判定。带 Remote 批次的消息在 post-release / 解锁后回调里调用 `RunIsolatedTransaction` 返回 `ErrNestedTransactionInRemoteMessage`；
  纯本地消息按嵌套独立事务处理（外层失败回复带 `ErrNestedTransactionCommitted` 而非 `ErrAfterCommitFailed`，结果未知时 fence）。handler 内（包括 memory handler 内经 `RunDetachedTransaction`，如 `skill/combatcomponent`）的调用修前就按嵌套处理，行为不变；变化只在收尾阶段（更正：本条发布时曾误写 memory handler 内行为也变了，见 [第五轮审计](docs/review/REVIEW-2026-09-27-audit5.md) N2）。
- **`entitysync.ReleasedSubscription` 新增 `Stamp` 字段（RR-20260926-85）**：用位置式字面量构造它的仓外代码需改为带字段名；自建政策判定释放应比较戳（`SubscriptionSource.SubscribeStamped`），不再用 `Holds`。

### Added

- 哨兵：`nest.ErrNonRollbackNotRequeued`、`nest.ErrNestedTransactionRollbackConflict`、`nest.ErrNestedTransactionInRemoteMessage`、`nest.ErrNestedTransactionCommitted`、`nest.ErrCreatedEntityLockConflict`、`entity.ErrRemoteEntityReloading`、`entity.ErrEntityLoadTimeout`、`entity.ErrEntityLoaderStopped`、
  `entitysync.ErrRegistrationCancelled`、`entity.ErrRemoteUnloadUnsupported`、`engine.ErrEntityAggregateCorrupt`。
- API：`entity.ManagerAccess` 的 `IsLoaded` / `Unload` / `UnloadRemoteEntity` / `ConfigureLoadTimeout` / `ConfigureUnloadResync` / `BindLocalExecutor`；`entitysync.Manager` 的 `Rebind` / `RetractSyncSubject` /
  `RetractUnloadedSubject` / `SubjectAwaitsReload` / `RegisterAfterRetirement` / `NewSubscriptionSourceWithResubmit`；`NestMgr.RunLocal` / `DurableWatermark`；`entitysync.Manager.NewSubscriptionSourceWithHooks`、`SubscriptionSourceHooks`、`ReleasedSubscription`、`SubscriptionSource.Holds`（RR-79）、`SubscriptionStamp`、`ReleasedSubscription.Stamp`、`SubscriptionSource.SubscribeStamped`（RR-85）；`kit/dataengine.Mod.OnEntityLoaded` / `StopBudget`；
  `nestwal.WAL.Terminated`；`driver.JetStreamSyncStream`；`entitysync.SessionOpenRetryable`。
- 配置：`player_access.tcp.dispatch_timeout` / `login_timeout`（RR-36）、`remote_entity.finalize_projection_timeout`（RR-38）。
- 指标：`dataengine.fence.evictions.{started,failed}.total`、`nest.remote.deferred_after_commit_error_total`、`nest.remote.post_commit_without_outcome_total`、`remote_entity.rejected_unload{,_error,_unsupported}_total`、
  `remote_entity.finalize_retry_total`、`remote_entity.finalize_status_read_total`、`remote_entity.quarantine_error_total`、`remote_entity.deferred_outcome_{error,not_run}_total`。

### Fixed

- **RR-20260926-33～47**（v1.17.0 疑点核实）：WAL fsync 粘滞、投影 Mongo 事务撞键卡死、handler 内 CreateInScope 进入提交边界且 kit 自动接 pipelined 水位、冷登录等待投影有截止、
  Remote strict 确认超时后 Sync 冻结、finalizer 与投影器并发发布、被拒绝隔离的 Remote 实体可重载、demo 重连不再移出新连接玩家、零填充尾部、`dataengine.shutdown_timeout` 生效、
  versionedLock 续期按代际绑定、Prepare 失败 Abort 不占快池续行、sid 作用域 DAO 校验、已提交回复带哨兵、准入冷目标判定不调 Getter。见 [核实记录](docs/review/REVIEW-2026-09-26-v1170-triage.md)。
- **RR-20260926-48～63**（修复合并后审计）：事务内新建实体交叉创建死锁、已提交事务被重排、WAL terminal 后重复驱逐、停机预算缩放反例、旧连接重同步循环、共享加载与调用方 ctx 解耦、
  快速重连 `ErrSubjectRetiring`、syncbus 流名隔离、GetOrCreate 瞬时 `ErrEntityRemoved` 有界重试、混合事务 Remote 拒绝、卸载后订阅者重载、装配校验、快池拒绝不就地执行回调、重载窗口可区分、测试与契约收尾。
- **RR-20260926-64～72**（第二轮审计）：memory handler 新建冲突不重排、嵌套独立事务已提交不重排、按 Mod 生成停机时长、handler 内 Destroy 后重建同 ID 实例取锁、临近截止推送不关健康连接、
  forget 不清新 subject 的通知器、政策订阅自动重新提交、按 kind 实际 Remote 策略判定、`RegisterAfterRetirement` 退役中返回值。
- **RR-20260926-73～80**（第三轮审计）：Cast 等锁期间目标被摘除返回 `ErrEntityNotFound`；政策重新提交交付前再次撤销时记录回到撤销表（Group / Direct 不再永久丢订阅）；
  会话关闭 / 业务注销时通知政策，`policy.Direct` 绑定表不再单调增长；doctor 逐份判定三份仓内配置并显示磁盘模板的实际宽限期，减少 Mod 后一次 `roost project sync` 即收敛，
  配置刷新与模板同一批提交、同受回滚与并发输入检查保护；USER_GUIDE §4 新增“是否已提交 / 能否重试”判别表。
- **RR-20260926-81**：handler 内新建撞上同 ID 撤销 / 销毁收尾中的实例（锁已释放、removing 标记未清）时，不再返回不重排的 `ErrEntityRemoved`，改为 `nest.ErrCreatedEntityLockConflict`：可回滚事务整条回滚后重新准入，不能回滚的 handler 不重排；Nest 之外行为不变。修复 RR-48 回归的偶发失败。
- **RR-20260926-82**：dataengine/engine 包测试可重复运行（只改测试）。
- **RR-20260926-83～85 与复核补修**：entity 包测试可重复运行（只改测试）；收尾阶段独立事务不认领消息；Direct 释放通知不再删掉会话重开 / 重新登记后新做、随即被撤销的绑定（Group / Direct 恢复语义与 RR-70 一致）；
  Cast 摘除错误带目标 ID（RR-73）；doctor 对读不懂的示例配置只指明文件与键，不再给误导性的建议值（RR-80）。
- `kit/scripts/integration/dataengine-env.sh test` 纳入 `./dataengine/engine` 的真实 Mongo 集成测试（RR-30 新增）。

## [v1.17.0] - 2026-09-26

> **v1.17.0**。相对 v1.16.1 **含大量破坏性变化**：严格按语义化版本应升大版本，但模块路径保持 v1（v2 需 `/v2` 路径并重写全部 import），
> 沿用 v1.16.0 合仓时“破坏性变化发小版本”的惯例，**本版不遵守 SemVer 的大版本规则**。升级先读下方“Changed（破坏性）”，再执行 `roost project upgrade --consolidate`。

### Changed（破坏性，v1.16.1 → v1.17.0 补充）

以下为下文各节之外、此前漏记或本轮新增的破坏性 / 行为收紧：

- **Nest 快阶段执行约束**：handler、Guard 与本地回滚只在快池；快 worker 上不做 I/O 等待。快阶段访问未加载实体返回 `entity.ErrColdLoadInLogic`（不再冷加载，快 worker 上同时包裹 `fctx.ErrBlockingInFastWorker`）；
  声明目标未加载时统一准入**自动**走慢阶段预加载，生成 sender 与业务代码无需改动，`nest.SendOptionSlow()` 仍可显式使用；自定义 Getter 必须遵守 `entity.LoadedEntitiesOnly`。
  框架自带的等待入口（Repository 冷加载、投影等待、Remote 准备/提交/等待、快续行）在快 worker 上被调用时 panic `ErrBlockingInFastWorker`（RR-20260926-02/03/06/25/26）。
- **DataEngine**：自定义 `RecoveryGate` 需提供 `WaitEntityProjection`（重载前等待本实体投影，RR-10）；`Projector.Close` 后 `Flush` / `ReplayPass` 返回 `ErrRuntimeStopped`（RR-17）；
  `ProjectorOptions.OnFatal` 改为异步调用一次，回调内可同步 Close；fatal 后 `WaitEntityProjection` 与 `CommitSystem` 票据立即以 fatal 结束，关闭后返回 `ErrRuntimeStopped`（RR-10/17 复核）；
  Remote 写与 lease-fence receipt 同事务在 WAL 准入前拒绝 `engine.ErrRemoteLeaseFenceUnsupported`，saga 原生步骤不能写 Remote 实体（RR-19，见 SAGA.md）；
  自定义 remoteStore 回放历史混合记录需实现 `RejectRemoteCommitsInTransaction`（RR-19）。
- **Remote**：Durability 0 提交结果未知时交 finalizer，从未到达 Mongo 的事务由 finalizer 以同一事务 `_id` 写持久 Rejected 收敛（RR-28）；
  持久拒绝后实体**保持隔离直到重新加载**，不再解冻为可写（此前 Durability 1 首轮即读到 Rejected 的实体可继续写并把被拒绝的内存修改写出，已堵上）；
  tracker 中 Committed / Rejected 终态不再被重放失败覆盖（RR-11 复核）；Remote 批次按显式“已持久提交”状态决定 Commit / Abort，提交后错误（release hook panic 等）不再 Abort（RR-14/32）。
- **Sync**：`OpenSession` 在传输确认前会话不可见；确认期间同 ID 并发 Open 返回 `entitysync.ErrSessionOpening`，旧队列未退出时返回 `ErrSessionClosing`（包裹 `nettransport.ErrSessionAlreadyExists`），
  均表示未创建、可重试；确认期间 Subscribe 返回 `ErrSessionUnknown`（RR-15 复核）。字节软预算下被挡的冷创建不再每窗口重复打包，RetryLater 后游标不越过未尝试会话（RR-04/05 复核）。
- **kit/syncbus**：配置以 `syncbus:` 段为准，兼容读取 `room:` / `sync:` 并告警弃用；未知键告警；无效 `transport` 使 Init 失败（此前静默退回普通 NATS，RR-12）。
- **codegen `upgrade --consolidate`**：遇到无法自动映射的已删除 / 换包符号时逐条列出 `文件:行` 与指引并**非零退出**；kit 保留路径包名变化时补别名（RR-24）；旧 Entity 标记 `syncTopic` 须先手动改为 `syncNamespace`。
- 此前漏记：`entity.SubjectSyncState.CaptureSnapshot` 删除（M-13 收尾）；`remoteentity.NewVersionedLockFactory` 新增可变参数 `...WriteAuthority`；robot 使用的 lockstep 类型随 `sync/lockstep` 迁移。

### Added（2026-09-26）

- `engine.ProjectorOptions.ManualReplay`（外部测试夹具手动回放，生产装配不设置）、`kit/dataengine.Mod.WaitEntityProjection`、
  `remoteentity.MongoCommitter/Backend.RejectUnresolvedRemoteCommits`、`fctx.InFastWorker/WithFastWorker/BlockingError/AssertBlockingAllowed/ErrBlockingInFastWorker`、
  `entitysync.ErrSessionOpening/ErrSessionClosing`；指标 `nest.dispatch.slow_reroute.total`、`remote_entity.unresolved_reject_error_total`、`remote_entity.unresolved_resolved_total`、
  `remote_entity_transaction_final_overwrite_ignored_total`。

### Fixed（2026-09-26 复审与复验）

- **RR-20260926-01～09**：Sync policy 失败保留 LastError；Nest 快阶段禁冷加载；Slow 快阶段 RunLocal 自等死锁；字节软预算大对象饥饿；快照额度 Push 前预扣；快池阻塞入口 fail-fast；
  WAL 回放边界漏计；续行占用时队列容量口径；Remote 并行投影错误带失败事务 ID。
- **RR-20260926-10～24**：实体卸载后投影前重载（P1）、Remote 重放被新 fence 拒绝致投影卡死（P1）、syncbus 配置段失效（P1）及 12 条 P2，见 [上线前复审](docs/review/REVIEW-2026-09-26-release.md) 与 [修复与兼容边界](docs/review/REVIEW-2026-09-26-release-fixes.md)。
- **RR-20260926-25～29、31、32（复验发现的回归与既有缺陷）**：冷声明目标自动慢阶段预加载（P1，离线玩家 saga 补偿 / GM 发放）；快阶段冷缺失不再 panic；快 worker 删除 Remote 实体确定拒绝不 fence；
  Durability 0 未提交结果持久拒绝收敛；kit/dataengine 集成夹具恢复；demo 闲置交还等待投影再释放租约；Remote 批次按显式持久提交状态收尾。
  复核补修：RR-04/05/07/10/11/12/15/17/19/22/24。见 [修复复验](docs/review/REVIEW-2026-09-26-fix-verification.md)；**RR-20260926-30 未修复，需维护者拍板契约**（[评估](docs/bugfix/RR-20260926-30.md)）。
- **已知风险（未修复）**：[RR-20260926-30](docs/bug/RR-20260926-30.md)——saga 原生步骤写本地实体时，若投影积压超过租约期（demo 为 2 分钟），该记录被跳过而内存保留修改，之后同实体投影冲突、进程 fence 且重启无法自动恢复。缓解：避免投影长期积压（监控 WAL 积压 / BacklogWarning），必要时调大原生步骤 `LeaseDuration`。
- 修前负对照证据收入仓库：[EVIDENCE-2026-09-26-985d5ba-negative](docs/review/EVIDENCE-2026-09-26-985d5ba-negative.md)。

### 2026-09-26 九项优化与快照调度（8ce21a5、a22c5a5）

- Sync：待快照请求增量索引、轻量 `Stats` 与显式 `AuditStats`；新入场 / 恢复分类公平调度共用预算并可借用空闲额度；冷创建与已有对象全量更新预算分开；恢复诊断与窗口边界修复。
- DataEngine：`projection.read_bytes`（ReplayReadBytes）独立限制回放读取保留量；Remote 有界滑动窗口并发投影（`RemoteProjectionWorkers`），只 ack 连续成功前缀。
- Nest：队列区分前驱等待与 worker 等待、峰值 / 年龄 / 拒绝 / 续行观测；指标 key 与单 ID 路径减分配。验收与已接受的性能边界见 [交接文档](docs/CORE-OPTIMIZATION-HANDOFF.md) §4。

### 2026-09-25 Nest 双池复核

- 修复小等待队列在 worker 尚有额度时提前拒绝请求；1024 并发 / 16 等待位已完成调度回归。
- 修复 Remote 回滚 hook panic 遗留 Entity 本地锁；[复核与验证范围](docs/review/NEST-FAST-SLOW-2026-09-25.md)。

### 2026-09-25 Nest 快慢双池

- 统一全部显式目标 ID 准入顺序；main/hb/cost/remote 收敛 Fast/Slow，共享慢队列。
- 所有 Nest handler/Guard 在快池，慢阶段的加载初始化与失败本地回滚同步交回快池；增加独立并发/整池等待容量配置。
- [RR-20260925-07](docs/bugfix/RR-20260925-07.md) 与 [本轮验收](docs/feature/REFACTOR-2026-09-25-nest-fast-slow.md)。

### 2026-09-26 复审修复

- RR-10～24：实体重载投影屏障、Remote 提交/收尾分离、WAL 持久确认与关闭排空、syncbus 配置、Backend 并行能力及公会 ID。详见 [修复与兼容边界](docs/review/REVIEW-2026-09-26-release-fixes.md)。
- 新的 Remote 写 + lease-fence receipt 同事务在 WAL 前拒绝；旧 WAL skipped 记录补持久拒绝结论。
- `upgrade --consolidate` 补齐 `room`、`kit/room` 包迁移和 `RoomMod` / `NewRoomMod` 改名。旧 Entity 标记 `syncTopic` **必须先手动改成 `syncNamespace` 再重跑 upgrade**；demo 业务文件仍需按既有迁移指南重新生成或人工合并。


### 2026-09-25 资源预算与会话恢复

- Remote 完整写生命周期独立预算 `MaxConcurrentWrites`，默认 128；与 Nest 慢池并发、DataEngine WAL 上限分别配置，公开在途/上限/拒绝指标。
- Sync Hold/Ready/Close 按实际订阅遍历；修复同 ID 重开会话继承旧订阅（RR-10），补齐通用故障矩阵 Remote 入口（RR-11）。
- 1024 慢 worker 配 128 写预算短测 79.74 TPS 零错误；Sync 1% 变化本轮 50ms 门禁通过，5% 仍有少量长尾。配置迁移及实际验收边界见[报告](docs/feature/REFACTOR-2026-09-25-resource-budgets-and-session-recovery.md)。

### Fixed（Remote）

- **Nest 慢操作隔离**：Remote 获取、确认和释放使用独立有界慢池，业务和 Guard 在 cost 逻辑池执行；增加 `nest.remote_workers` 和启动 option，停机按依赖顺序排空，取消/过载仍清理批次。80 TPS × 10 分钟全量验收、race/vet 与 21/21 故障矩阵通过。[RR-06](docs/bugfix/RR-20260925-06.md)、[容量验收](docs/feature/REFACTOR-2026-09-25-nest-remote-stages.md)。

- **投影排队与超时**：独立 Entity 的纯 Remote 事务默认 8 路有界并行，保留同实体顺序和 WAL 连续前缀确认；提供 `dataengine.projection.remote_workers`。默认 5 秒请求等待不变，30 分钟实测 59.997 TPS、108000 笔零错误，全量一致性与 21/21 故障回归通过。[RR-05](docs/bugfix/RR-20260925-05.md)、[验收报告](docs/feature/REFACTOR-2026-09-25-remote-throughput.md)。

- **持久写权限与批量投影**：正式装配将 ownership/grant 迁入 Mongo majority，原子提交校验最新许可，修复 Redis 未复制写丢失后的 fence 复用；同一事务按集合合并 DAO/快照写入。当前未部署，默认统一严格许可校验并移除旧协议迁移入口。[RR-26](docs/bugfix/RR-20260924-26.md)、[本轮验收](docs/feature/REMOTE-AUTHORITY-2026-09-25.md)。
- **Remote 回执重放快路**：DataEngine 已落库后的发布阶段直接读取并校验持久回执，省去重复 Mongo 事务；首次写入和并发重放仍保留事务内检查。[RR-04](docs/bugfix/RR-20260925-04.md)。
- **Remote 准入与诊断收敛**：删除默认弱校验分支，共享写复用持久 grant 的 ownership，Nest 全进程慢堆栈每 5 秒至多采样一次。[RR-02](docs/bugfix/RR-20260925-02.md)、[RR-03](docs/bugfix/RR-20260925-03.md)。
- **故障测试资源清理**：关闭集成夹具时删除本次独有的 JetStream 流，避免重复运行耗尽预留容量。[RR-20260925-01](docs/bugfix/RR-20260925-01.md)。

- **集群故障恢复**：Redis Cluster 连接错误后合并异步刷新拓扑，不重放不确定写操作；Kit 拒绝无有效 hash tag 的 Remote Cluster 锁配置。[RR-24](docs/bugfix/RR-20260924-24.md)、[RR-25](docs/bugfix/RR-20260924-25.md)。新增 [容量、长稳与集群故障验收入口](docs/feature/REMOTE-ACCEPTANCE-2026-09-24.md)。

- **准入预算**：冷 wrapper 构造不再执行脱离请求 context 的权威读取；多实体准入共享预算，调用方长 deadline 不覆盖 `OpTimeout`。[RR-23](docs/bugfix/RR-20260924-23.md)。补齐多进程故障与正式生成 Nest → Mongo/WAL → NATS 链路验收，[第三轮记录](docs/feature/REFACTOR-2026-09-24-remote.md#第三轮进程故障与正式业务链路)。

- **启停与锁代际**：启动幂等、失败可重试、停机超时保留清理责任；迟到解锁只更新同代状态；Redis Lua 精确保留 int64 版本/fence，分配失败不遗留 owner。[RR-19](docs/bugfix/RR-20260924-19.md)、[RR-20](docs/bugfix/RR-20260924-20.md)、[RR-21](docs/bugfix/RR-20260924-21.md)、[RR-22](docs/bugfix/RR-20260924-22.md)。

- **事务等待与准入**：FlushAll 保留调用时 tracker，避免终态淘汰导致错误等待；无效提交不再侵占 pending 容量。[RR-16](docs/bugfix/RR-20260924-16.md)、[RR-17](docs/bugfix/RR-20260924-17.md)。
- **Redis 快照写入**：RemoteChecksum 在驱动边界编码为精确十进制参数，修复本机可读而 L2 未写入。[RR-18](docs/bugfix/RR-20260924-18.md)。
- **事务缓存热点**：按首次完成顺序回收、常数时间计数；同包拆出事务跟踪，补真实 Mongo/Redis/WAL 恢复验收。[方案、数据与范围](docs/feature/REFACTOR-2026-09-24-remote.md)。

### Fixed（DataEngine）

- **投影身份与确定冲突**：事务内 Put 重复键不再查询已中止的 Mongo session；同批重复 ID 在 marker 查询前比较 digest，拒绝不同内容。[RR-14](docs/bugfix/RR-20260924-14.md)、[RR-15](docs/bugfix/RR-20260924-15.md)。

- **慢 Broker 退避**：Outbox 重试时间从当前发布失败时计算，避免整批旧时刻耗尽重试窗口；1/2/4/4 秒受控时钟回归通过。[RR-13](docs/bugfix/RR-20260924-13.md)。

- **启停与失败清理**：重复启动幂等、并发启停等待可取消、失败回收保留所有权，Runtime 明确关闭自己的 WAL。[RR-11](docs/bugfix/RR-20260924-11.md)。
- **Remote 永久冲突**：新建 meta 的重复 ID 归为明确版本冲突，触发 Projector 熔断并保留未 ack WAL。[RR-12](docs/bugfix/RR-20260924-12.md)。
- **重放内存分配**：读到可投影记录后才申请批次空间，删除中间事务 ID 切片；空闲/held 微基准 B/op 下降约 96%/94%，ack 顺序不变。[性能记录](docs/feature/DATAENGINE-BENCHMARKS-2026-09-24.md)。

- **历史 Remote 创建 ID 冲突**（2026-09-24）：生成公会改用 Mongo 持久化发号，避免同 SID 重启复用 runtimeid；真实 WAL 证据与三轮 Mongo/WAL 恢复验证完成。历史冲突记录不自动丢弃。[W-2026-09-22-02](docs/bugfix/W-2026-09-22-02.md)。
- **Outbox 启停竞争**（2026-09-24）：统一启动/关闭状态，修复关闭后启动导致的重复 close panic；并发 race 回归通过。[RR-20260924-10](docs/bugfix/RR-20260924-10.md)。

- **投影与停机等待可取消**（2026-09-24）：Projector Flush/ReplayPass 和 Runtime 并发 Shutdown 等待操作所有权时遵守 context；保留单一投影、ack 与停机重试状态。[RR-20260924-09](docs/bugfix/RR-20260924-09.md)。

### Added

- **DataEngine 多 DAO 批量与积压治理**：支持有界本地多 DAO Mongo 批量事务、可安全重放路径的 checkpoint 合并、未 ack 事务准入上限和健康预警。36 样本复测中 pair/pipelined 最终落库约 48.6 → 2845.6 事务/s，保持 WAL/Mongo 持久化配置。[实施与验收](docs/feature/DATAENGINE-BATCH-2026-09-24.md)。

- **DataEngine 正式压力工具**：生成 DAO → Nest → 文件 WAL → 真实 Mongo，覆盖单/双/四 DAO、热点争用和固定速率输入；分别统计请求返回、最终落库、投影延迟、积压与 ack 成本。[方法与结果](docs/feature/DATAENGINE-PRESSURE-2026-09-24.md)。

- **DataEngine 恢复夹具**：100k WAL 跨段/慢存储取消/真实 Mongo 恢复；正式生成 DAO + Nest 的三次独立进程验证，覆盖 async/strict/pipelined、共享实体四文档交易、错误/panic 回滚、ack 丢失幂等重放与末文档冲突原子回滚。[入口与验收](docs/feature/DATAENGINE-RECOVERY-2026-09-24.md)。

- **Nest 消息吞吐夹具**（2026-09-24）：正式 Client、EntityManager 与 Guard 的热点/分散 Dispatch/Request 压测，统计实际完成吞吐、采样延迟、分配和错误，附独立进程复跑脚本。[方法与数据](docs/feature/NEST-MSG-THROUGHPUT-2026-09-24.md)。

- **正式 Entity Sync 双模式**（2026-09-24）：周期默认，变化触发可选；Nest 准入/解锁/确认边界、独立 DAO 客户端 dirty、生成器自动收集、有界冻结槽、提交关联的 Interest 事实、共享窗口预算及 Kit 启停接入。共用现有 Profile、版本与完整实体包线协议，不依赖示例游戏。[实施与验证](docs/feature/IMPLEMENTATION-2026-09-24-sync-modes.md)。

### Fixed（Nest）

- **Nest 截止复核**（2026-09-24）：pipelined 准入后 release hook 异常仍完成 WAL 责任并回复，新增 `ErrEntityReleaseFailed`；inline / 队列满降级的 AfterCommit 失败不再返回成功。[RR-07](docs/bugfix/RR-20260924-07.md)、[RR-08](docs/bugfix/RR-20260924-08.md)。

- **Nest 异步完成与 Ticker 竞争**（2026-09-24）：完成池等待 Guard 解锁后执行回调和回复；Ticker 的 Start/Stop 统一生命周期临界区，避免 Stop 返回后仍启动。[RR-05](docs/bugfix/RR-20260924-05.md)、[RR-06](docs/bugfix/RR-20260924-06.md)。

- **Nest 提交与释放边界**（2026-09-24）：pipelined 的 AfterAdmission 在写入 CommitLSN 后、解锁及异步交接前执行；release hook panic 不再泄漏组锁与 scope，覆盖部分加锁失败回退。[RR-20260924-01](docs/bugfix/RR-20260924-01.md)、[RR-20260924-02](docs/bugfix/RR-20260924-02.md)。后续正式双模式已接入，见上方实施记录；原始[审查与方案](docs/feature/REFACTOR-2026-09-24-nest-and-immediate-sync.md)保留。

### Changed

- **Nest N1～N4 收尾**（2026-09-24）：同包职责整理、普通路由统一收尾、显式事务准入/释放/完成流程、可选分阶段指标、慢请求计时器安全复用与 tick 回调注册时复制；保持正式 Sync 双模式契约。新增可复跑微基准和 profile 脚本，[实施与验收](docs/feature/NEST-COMPLETION-2026-09-24.md)。

- **Sync 六项收尾优化**（2026-09-24）：按完整实体包组帧并遵守传输上限；可选 profile 装配校验与优先级冲突检查；快照对象/字节预算及会话轮转；Flush/编码/AOI 工作区复用；可靠队列全局驻留预算、准入起算的年龄限制与无需遍历的 Counters。原有预算默认关闭，线协议不变，大包可能产生更多完整帧。[实施与验证](docs/feature/REFACTOR-2026-09-24-sync-six-items.md)。

- **Sync 低变化率优化与 profile 配置**（2026-09-23）：AOI 直接选择最远对象、合并观察者查询，Flush 聚合临时数据，会话引用表首次写入才复制；字段视图支持生成 DAO 字段名、显式优先级和 Interest 来源映射，同次 full-dirty/新订阅复用 snapshot 打包。新增 Flush/队列指标、可选队列字节预算和异步负载模式。1%/5% 目标负载分配约下降 58%/47%。[实施与验收](docs/feature/REFACTOR-2026-09-23-sync-next-steps.md)。

- **本 tick 共享组件编码**（M-20）：相同捕获结果由各会话共用，full/delta/profile 分开，缓存不跨 tick；会话外层帧和线格式保持。新增 EntitySync / frame 基准与 `scripts/perf/sync.sh`，支持固定参数、重复采样和 pprof。

- **Sync 可读性整理**（2026-09-23）：Manager 在原包内按生命周期、会话订阅、Flush 分为三个职责文件；提取重试基线恢复与订阅结算步骤，补充中文契约注释。数值与多字段排序改用 `slices` / `cmp`，保留事件稳定顺序；会话 map 拷贝使用 `maps.Clone`。删除仅旧内部测试使用的 `AsyncTransport.session`，同步 Group/AOI 命名与当前文档。包路径、公开 API 和线格式保持不变。[实施记录](docs/feature/REFACTOR-2026-09-23-sync-readability.md)。

### Changed（破坏性）

- **Profile 来源选择与 demo 白名单**：Interest 统一按来源映射后的 profile 优先级选优，非单调的自定义 band→profile 映射可能与旧 min-band 结果不同；默认映射保持。Player/Monster demo packer 从忽略 profile 改为接受已声明的 default/near/far，自定义 profile 需加入声明。[迁移说明](docs/feature/SYNC-PROFILES.md)。SyncProfile 线格式与已有用户自定义 packer 接口不变。

- **订阅所有权明确为来源**（M-19）：Group / Interest / Direct 各实例独立持有 profile，重复订阅幂等，最优视图改变时补全量。`Manager.NewSubscriptionSource` 新增来源入口；原 Subscribe / Unsubscribe 签名保留，作用于默认来源。Group.RemoveSubject / Close 只释放本组订阅，实体销毁需显式 Manager.Unregister；Interest.Close 释放本来源订阅。

- **`nettransport.AsyncTransport` 只留 reliable**（M-18，2026-09-23，关闭 W-2026-09-23-01）。删掉 latest-only datagram lane（`SendDatagram / SendDatagramBatch`）、`AdmitBatch / OutboundFrame / AtomicBatchTransport / AdmissionError`、42 字节分片头（`DatagramHeader / FragmentDatagrams / InspectDatagram`）与配置 `MaxDatagramsPerFrame / MaxDatagramBytes / AllowOpaqueDatagrams`、统计 `Datagram*`；`NewAsyncTransport` 的下游参数收窄为 `ReliableSender`；`SendError` 去掉 `Channel`。UDP / KCP / QUIC 的裸 `SendDatagram` 保留（lockstep 用）。
- **同步块收进 `sync/`**（ARCH-12 S3 / S4，M-17，2026-09-23）。`entitysync` → `sync/entitysync`、`entitysync/policy` → `sync/entitysync/policy`、`statesync` → `sync/frame`（包名 `frame`，`EncodeFrame / DecodeFrame / DeltaFrame / FrameFull / FrameDelta` → `Encode / Decode / Frame / Full / Delta`）、`nettransport` → `sync/nettransport`、`lockstep` → `sync/lockstep`、`syncbus` → `sync/syncbus`、`mirror` → `sync/syncbus/mirror`。`sync/` 本身没有 Go 文件。`spatial` 只留纯几何：`InterestManager / InterestCluster / InterestConfig` 搬进 `policy` 改名 `AOI / AOICluster / AOIConfig`（`InterestEvent` 一族随行，`RoomID / AddRoom` → `AreaID / AddArea`），`policy.InterestConfig.Spatial` 字段改名 `AOI`；`spatial` 导出 `SaturatingAdd / SaturatingSub / DivideCeil / SafeSpan`。迁移表新增第三阶段 `layout:`，`roost project upgrade --consolidate` 改写业务工程的 import（T-175）。kit 的 `kit/syncbus`、`kit/remoteentity` 路径不变。
- **传输契约归位 `nettransport`**（ARCH-12 S2，M-16，2026-09-23）。`SessionID / SessionInfo`、`Transport / SessionTransport / DatagramBatchTransport / TransportFunc` 与 datagram 分片头从 `statesync` 移到 `nettransport`；`FragmentFrame` 改为与帧无关的 `FragmentDatagrams(DatagramMeta, encoded, maxDatagram, maxFragments)`，`InspectDatagram` 改为接受两个上限；`statesync.Limits` 去掉 `MaxDatagramBytes / MaxFragments`。`entitysync.SessionID`、`lockstep.Room` 的会话类型改为 `nettransport.SessionID`。
- **`statesync` 只剩帧格式**（ARCH-12 S1，M-15，2026-09-23）。删掉 ARCH-10 之后没有调用者的老 `Replicator`、`SessionState`、`LODProjector`、`BuildDelta / ApplyDelta`、`ShadowStore`、`SnapshotRing`、`SchemaRegistry`、控制消息、`Reassembler`，以及 `nettransport.ControlPlane`；`Limits` 去掉 `MaxInflightFrames{,PerSession}`。业务代码只用到的 `EncodeFrame / DecodeFrame`、帧类型、`Limits`、`SessionID`、传输契约全部保留。
- **`room/` 包退场，`policy.Room` 改名 `Group`**（ARCH-10 命名收尾，2026-09-23）。ISyncBus 的 NATS / JetStream 实现从 `room/` 移到 `syncbus/driver`（契约仍在 `syncbus`）；
  kit 的 `room.RoomMod` 改为 `syncbus.SyncBusMod`，`mods.ModRoom` 改为 `mods.ModSyncBus`（值 `"syncbus"`，配置段 `syncbus:`，codegen 的 `-mods room` 与 roost.yaml 里的 `room` / `sync` 都映射到 `syncbus`）。
  `entitysync/policy.Room` 改名 `Group`：它是"成员全互见的集合"，与 `lockstep.Room`（战斗房间）不是一回事。
- **ARCH-10 收尾：held/ready 会话、`syncTopic` → `syncNamespace`、新包 `entitysync/policy`**（M-14）。`Manager.OpenHeldSession / HoldSession / ReadySession`：
  held 的会话可订阅不出帧，Ready 后首帧是新 epoch 的 FrameFull；demo 新增客户端消息 `scene_ready`（10024），`enter_game` 以 held 开会话，机器人在 `scene_watch` 后发它——
  首帧竞态消失。`EntitySyncBuilderParam.Topic` 改名 `Namespace`，codegen 标记 `syncTopic=` 改名 `syncNamespace=`（旧键报"已改名"）。
  demo 模板的 `InterestSystem` / `RelationSource` 搬进 core 成为 `policy.Interest`（直接驱动 Manager，`Apply() []Refusal`），新增 `policy.Room`、`policy.Direct`；
  demo 删除 `game/scene/runtime/{interest,relations,interest_test}.go`，`scene` 契约去掉 Source / Relations / SubscriptionChange / Interest。
  记录：`docs/bugfix/M-14-sync-policy-and-ready.md`。
- **实体同步统一为 `entitysync.Manager`，room 广播器整条退场**（M-13，ARCH-10）。`entitysync.SubscriptionCoordinator`、`room.RoomBroadcaster` /
  `RoomEnvelopeSink` / `RoomTransportSink` / `RoomManager` 全部删除；`room/` 只剩 ISyncBus。新形状：subject 自己持有订阅者表（`session → {profile, kind, baseVersion}`），
  进程一个 `Manager`（`Register/Unregister`、`OpenSession/CloseSession`、`Subscribe/Unsubscribe`、`Flush/Start/Stop`、`Stats/CheckHealth`、`SessionLost` 钩子），
  每 tick 给每个会话**一帧**；传输层只有两种失败——`ErrRetryLater`（整体不可用，tick 作废重来）和其他（该会话关闭）。
  `entity.SubjectSyncState.PrepareTick` 一次锁内同时捕获 delta 与新订阅者的快照。**线格式 v2**：去掉外层 room 帧头，帧就是 statesync 帧，
  `Epoch/Tick` 是会话时钟，`RoomID` 恒为 1；datagram 分片通道不再用于实体同步；demo 协议 `EntitySyncPush` 删掉 `Datagram` 字段。
  生成工程需重生成 `scene.go` / `scene_test.go` / `cmd/loadtest` / `protocol/def/entity_sync.go`；迁移对照见 `docs/bugfix/M-13-entitysync-manager.md`。
  承诺：`entitysync/manager_promises_test.go` 8 条 + demo `scene_test.go.tmpl`。

### Fixed

- **Sync 收尾**（RR-20260923-04～07）：旧 Push 不再覆盖 Hold/重开/新的订阅意图；首次 create 在途退出仍补 remove。Stop/Close 等待可取消，周期 Push 跟随 context，退出前拒绝重启。Group.AddSubject 部分失败回滚本组订阅并支持重试；跨政策释放不再撤销其他来源。

- **实体同步退订残留**（RR-20260923-01）：换 profile 或撤回退订后等待快照，退订/退役仍按会话已交付引用表发送 ObjectRemove。
- **实体同步部分交付重试**（RR-20260923-02）：逐帧采纳已准入的时钟与引用；后续 ErrRetryLater 保留已交付前缀，受影响订阅以全量恢复内容基线，准入统计包含部分成功帧。
- **实体同步满容量替换**（RR-20260923-03）：同 tick 先释放旧对象再分配新对象，合法替换不再因 subject ID 顺序误触容量上限并关闭会话。

- **场景 lane 不再让一个推不到的会话拖累同批其他人**（U-0278，C8，W-2026-09-22-03，T-173；codegen 模板）。生成工程的 `sceneLane.AdmitBatch` 此前逐个 `PushPlayer`、第一个失败就 `dropAsync` 并整批返回错误：排在前面的已经推出去（下一 tick 再收一遍），排在后面的一帧没推；接入层整体不可用时也把玩家踢出场景。现在按失败种类分流：`ErrTransportUnavailable` 整批报错让房间重试、不踢人；单个玩家推不到则它自己离开（一条 Info），其余人这一帧照常。`Leave` 不再为"已注销"记日志；生成的接入层对"无会话"的推送计 `player_tcp_push_no_session_total`。`TestAnUnreachablePlayerDoesNotStarveTheOthers`、`TestAnUnavailableTransportKeepsTheBatchAndThePlayers`（`scene_test.go.tmpl`）。记录：`docs/bugfix/U-0278-scene-lane-per-session-push.md`。
- **一个断线的观察者不再让整个房间的下发停摆**（U-0277，C3，RR-20260922-01，T-172）。`entitysync.Unsubscribe` 与 room 的退役此前以"Leave 信封投递给被撤观察者成功"为前提，投不到就恢复 Active；会话已经不在的观察者因此永远撤不掉，room 每次 flush 都为它生成帧，生成工程的原子传输在它那里整批拒绝，按 id 排在它之后的所有观察者从此收不到任何帧（16 机器人实跑 3–6 个 `pos_x` 永不到达，服务端零告警）。现在撤订阅无条件完成，Leave 尽力投递，投不到用新哨兵 `ErrLeaveNotDelivered`（wrap `ErrEnvelopeAdmission` 与原因）报给调用方；退役照样注销 subject 并把未投递的 Leave 报一次。**行为变化**：`RetireSubject` 不再无限重试、`Stop` 不再因此返回错误。`TestUnsubscribeRemovesTheSubscriptionEvenWhenTheLeaveCannotBeDelivered`、`TestFlushStillReachesTheOthersAfterAnUnreachableObserverIsUnsubscribed`、`TestRetireSubjectCompletesWhenASubscriberIsUnreachable`。记录：`docs/bugfix/RR-20260922-01.md`。
- **故障矩阵重新有了家，并且跑的是真实列表**（U-0276，C2，RR-20260922-02，T-171）。合仓把 kit 的代码与脚本搬进来了，
  没搬 kit 的 `.github/`，于是 `dataengine-env.sh test` 一天里没有任何 workflow 调用；脚本自己还在跑两个已经没有测试文件的目录
  （报绿）、并在一个不存在的 `../roost-core` 里找 core 的四个套件（打一行 NOT run 后退出 0）。现在脚本从模块根跑
  `./kit/dataengine ./kit/nats ./redis/... ./etcd/driver ./mongo/driver`，nightly 加回 `fault-matrix` job，
  `TestFaultMatrixScriptNamesEveryFullEnvironmentSuite` / `TestSomeWorkflowRunsTheFaultMatrix` 把列表钉到带
  `//go:build integration` 的文件上。修好后第一次完整实跑就报出 `redis/driver` 的一格真红（W-2026-09-22-01）。
  记录：`docs/bugfix/RR-20260922-02.md`。
- **`service/mail` 的 Redis 集成用例终于有人跑**（U-0275，C2，RR-20260922-03，T-170）。M-11 把 mail 的传输半边下沉到
  `service/` 之后，`ci.yml` 的 `service-redis` job 仍只跑 `./kit/service/...`，五个 `TestIntegration*` 两周里在 CI 上一次都没跑过，
  而"no Redis test was skipped"守卫只看它跑过的包。两条命令加 `./service/...`；`TestCIRedisJobRunsEveryRedisIntegrationSuite`
  要求 job 的参数并集覆盖每个读 `REDIS_ADDR` 的包。记录：`docs/bugfix/RR-20260922-03.md`。

## [v1.16.1] - 2026-09-21

### Fixed

- **game-demo：一次贡献现在会说出它落进了哪个活动窗口**（U-0273，C5，RR-20260921-01，T-168）。窗口每 300 秒滚一次，而 `Contribute` 与 `Standing` 各自按调用那一刻的时钟算窗口 id——所以"12:14:59 通关、12:15:01 看榜"问的是两个窗口。在此之前调用方无法把这种情况与"这一点没算上"区分开：`Contribute` 算出了窗口 id 却只返回不含它的 `Participant`，端点连那个返回值也丢掉，`FinishDungeonResponse` 里没有任何线索。CI 的机器人因此在整 300 秒边界上偶发红，**而错误信息说的是"计数不对"**。
  `ActivityRunner` 加时钟缝（没有它，边界只能等真实时钟，根本测不了），`Contribute` 返回窗口 id（失败时也返回——日志里"哪个窗口拒绝了它"值钱），`FinishDungeonResponse.ActivityID` 带上它。机器人按窗口分支：同窗口维持精确 1/1 的强断言，换了窗口则断言**新窗口不得继承那一点**——原来抖动的那条路径现在也在检查一个真实不变量。
  **未改**：窗口未开时贡献被拒仍然只记日志（`finish_dungeon` 的注释明说"一个窗口少了一点不是失败的副本"），只是日志里现在带上了窗口。
- **demo-publish 现在有可能发布成功**（U-0274，C5，RR-20260921-02，T-169）。它**从未成功过**（最近 30 次 0 成功）：生成工程自带 `.github/workflows/`，而 Actions 的 token 不被允许创建或修改 workflow 文件，推送被远端拒绝——而 `demo-generated` 分支一直被当作"最新 demo"。三仓合一仓修好了它前面的所有步骤，反而让这最后一段露出来。推送前把该目录挪成 `generated-github/workflows/`（不删：那些 workflow 是读者想看的东西），GENERATED.md 写明原因与恢复方法。

## [v1.16.0] - 2026-09-20

### Changed

- **三仓合一仓：roost-kit 与 roost-codegen 并入 roost-core**。框架从此是一个仓库、一个 Go 模块、一个 tag。业务工程只依赖 `github.com/tjbdwanghaibo/roost-core`。
  - **布局**：`kit/`（装配层 + `kit/service/` 的 12 个通用服务）、`codegen/`（生成器，CLI 在 `codegen/cmd/roost`）、`demo/`（game-demo 模板）各是 core 根目录下的一个顶层目录，带完整历史搬入。core 自己的包一个没动。
  - **升级方式（破坏性）**：`roost project upgrade --consolidate`（先 `--dry-run` 预览）。它两段一起做：先按包表把上一轮五仓合三仓留下的旧路径搬到 core 本体，再把仍在 kit 的那部分前缀改成 `roost-core/kit/`，并从 go.mod 删掉 `roost-kit` / `roost-codegen` 的 require。**顺序不能反**——`roost-kit/dataengine` 属于第一段（它在 core 本体），前缀先跑会把它送进 `roost-core/kit/dataengine`，那里没有这个包。
  - **旧 tag 仍可 pin**：没升级的工程不受影响。roost-kit / roost-codegen 两个仓库发最后一个版本后归档。
  - **依赖方向**：core 的包不得 import `kit/`、`codegen/`、`demo/`；kit 可以用 core；codegen 独立于它生成的那个运行时。合仓前这条由 Go 模块边界免费保证，现在由 `dependency_boundary_test.go` 的目录前缀规则钉住，并且是在搬动任何代码**之前**就位的。
  - **合仓不新增任何直接依赖**：codegen 只依赖 `gopkg.in/yaml.v3`，kit 的四个直接依赖 core 原本就有。
  - **生成器下限抬到 v1.16.0**：它产出的每个 import 都在这个版本之后才存在。
  - **CLI 安装路径变了**：`go install github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest`。
  - **新增 `--skip-deps`**（`project new` / `sync` / `upgrade`）：边界迁移有一段窗口——改写出来的 import 已经正确，而没有任何 proxy 能解析它们。这个开关把"改写文件"和"解析依赖"分开。
  - **发布清单收拢成 schema 3**：一个模块一个版本（`release:` 一行）。schema 2 的三个版本号里，codegen 那一行曾经漂了十个版本并把受保护的发布闸一起带红（U-0270）；漂移检查随发布一起搬进 `scripts/pretag.sh`。
  - CI：测试按 `go list` 分片成四格（不写包名清单，手写清单是第二个要记住每个新包的地方）；kit 的 Redis 集成 job 与 codegen 的四个门禁都搬了过来。
  - 方案、逐阶段门禁与三处"路径即数据"的教训：[docs/ARCHITECTURE_V3_SINGLE_MODULE_PLAN.zh-CN.md](docs/ARCHITECTURE_V3_SINGLE_MODULE_PLAN.zh-CN.md)。

## [v1.15.18] - 2026-09-20

### Fixed

- **`SmallSafeMap` 不再被写成空文档**（U-0265，C2；RR-20260920-07，T-159）。`MarshalBSONValue` 的签名是 `(bson.Type, []byte, error)`，而驱动的 `ValueMarshaler` 要 `(byte, []byte, error)`；`bson.Type` 是 `type Type byte`——**定义类型，不是别名**，所以接口从未被实现、方法被静默忽略，类型退回默认结构体编码器，而它的字段全是未导出的。
  修复的**主体是两条编译期断言**（`var _ bson.ValueMarshaler / ValueUnmarshaler = …`），不只是改签名：同样的坑 U-0261 刚踩过一次，两次都靠人眼发现。存量数据不受影响——这个类型从来没成功写进过 Mongo。
- **远端写的预算现在覆盖排队**（U-0266，C8；RR-20260920-08，T-160）。`beginWrite` 先在每实体一格的写闸上 `select`（只看调用方 ctx），拿到闸之后才套 `OpTimeout`；排队因此没有上界（调用方无 deadline 时是**无限**等待），配置 3 秒的 `op_timeout` 与一次 79 秒的 dispatch 可以同时为真。
  同包的加锁路径已经是“先设 deadline 再排队”，所以这是一处遗漏而不是取舍。**契约写死**：`OpTimeout` 是一次远端写的**总**预算，包含本地排队；超出返回 `context.DeadlineExceeded`（可重试）。新增计数器 `remote_entity_write_gate_timeout_total`。

## [v1.15.17] - 2026-09-20

### Added

- **`versionstore.IndexDefer` / `IndexDeferIn`**：把已有的索引条目挪到后面，不碰值。它为一个基于值的更新做不到的场景存在：一条**解不开**的记录仍然有索引条目，而那条条目会排在每一页的最前面；删了它等于藏掉一条需要人看的真记录，重写值又做不到。`ZADD XX` 语义：**defer 一个不存在的条目不会把它凭空造出来**。
  测试跑真 Redis（`ROOST_REDIS_TEST_ADDR`），因为索引操作是 Lua。

## [v1.15.16] - 2026-09-20

### Added

- **`versionstore.ErrMalformedRecord`**：把“这条记录读不回来”和“基础设施出问题了”分开（RR-20260920-05 的使能件）。两者要的反应正相反：超时值得立刻重试，而字节解不开的记录会永远以同样的方式失败。分不开的重试循环只有两个结局——对瞬时错误放弃，或者让一条坏记录永久占住队头。
  四条解码失败路径（无分隔符 / 版本非数字 / 版本为零 / 载荷解不开）都带上这个标记，codec 自己的错误保留为 cause；传输错误不带。测试 `versionstore/malformed_promises_test.go`（含反面例）。

## [v1.15.15] - 2026-09-20

### Fixed

- **BSON 装不下的那一半散列空间**（U-0261，C8；RR-20260920-01，T-155，**P1**）。snapshot 的 `Checksum uint64` 原样进 BSON，而 BSON 没有无符号 64 位整数：CRC64 高位为 1 的那一半载荷写不进去（`… overflows int64`），而这条 WAL 记录会在**每次启动恢复**时同样失败一次——进程从此起不来。
  按字段语义分开定编码：
  - `checksum` 只做相等比较 → 新类型 `entity.RemoteChecksum`，存**定长 8 字节**，保住全 64 位域；读取兼容历史的 int64 / int32 / double / 缺失。
  - `version` / `epoch` / `fence` 要参与 Mongo 比较（快照读 `$gte`、版本 CAS）→ 保持数字，域截到 `MaxInt64`，超出者在**写入任何东西之前**被拒绝（`ErrRemoteRejected`）。
  **API 变化**：`RemoteSnapshotChecksum` 返回 `entity.RemoteChecksum`，`RemoteSnapshotRecord` / `RemoteSnapshotEnvelope` / `RemoteSnapshot` 的 `Checksum` 字段随之改型（底层仍是 `uint64`）。
  **未做**：已经存在的毒丸 WAL 记录没有隔离流程，仍需删 WAL。
  测试：`remoteentity/snapshot_encoding_promises_test.go`（含“上下半区各一个普通载荷”的行为用例与旧格式兼容）。
  记录：`docs/bugfix/RR-20260920-01.md`。这条修完第十八批（Guild，远端托管实体）的阻塞解除。

## [v1.15.14] - 2026-09-20

### Fixed

- **普通 room delta 不再走会被覆盖的 latest-only 通道**（U-0260，C8；RR-20260920-02，T-154，**P1**）。一次 delta 是“相对上一帧的改动”，不是“最新的完整状态”；`RoomBroadcaster.flushStateBatch` 在入队成功后就提交了这一批、清掉 dirty，而 `AsyncTransport` 同 stream 只留最后一帧。第二帧不是第一帧的超集，于是第一帧独有的字段对那个客户端**永久**消失，服务端全程无错误。实跑：16 个客户端同场景时每轮 2~6 个永远收不到自己的 `pos_x` / `equipment`，40 秒后仍未到达。
  普通 delta 改走**可靠通道**：队列上限和 slow-consumer 驱逐把背压**说出来**，而不是安静地少发一个字段。
  新增 `RoomTransportSinkConfig.LatestOnlyDeltas`（默认 false）保留 datagram 路径：打开它是一个**声明**——那些帧必须是自包含的最新状态，或者被下一帧替掉不会丢任何东西。
  **行为变化**：默认部署的 delta 从 datagram 改为 reliable；慢消费者从“静默丢帧”变成`ErrReliableBackpressure` + 房间保留 dirty 重试（或按策略驱逐）。客户端无需改动，两条通道本来就都要处理。
  测试：`room/delta_durability_promises_test.go`（含一条行为级：连续两帧 delta 的内容都要到达下游）；`TestRoomTransportSinkRoutesLifecycleAndState` 编码的是被推翻的旧契约，改成断言新不变量。

## [v1.15.13] - 2026-09-20

### Added

- **`redis.CompareAndDelete`**：只在 key 仍然持有调用方上次看到的值时删除它（RR-20260920-03 的使能件）。
  没有它，"放弃我自己的那把租约"只能写成 `GET` 确认 + `DEL`，而两条命令之间租约可以过期并被另一个
  进程取得——这次 `DEL` 删掉的就是**别人的** key。`Applied` 报告是否删成，`Current` 在没删成时给出
  当时真正的值（key 不存在时为 nil），调用方据此区分"现在是别人的"和"本来就没了"。
  `Expected` 必须非 nil：不比较就删等于 `DEL`，从这个名字进来会读出一个它并不提供的保证。
  测试跑真 Redis（`ROOST_REDIS_TEST_ADDR`），因为要验证的是 Lua 本身。

## [v1.15.12] - 2026-09-19

### Added

- **步骤消费者可以在认领之前拒绝一条命令**（`saga.StepConsumerConfig.Admit`）。一个 durable 被
  一个服务的所有进程共享，命令因此投给"闲着的"进程而不是"持有这个对象的"进程；而两个步骤消费者都是
  **先 `Reserve` 再调 handler**，所以"这台机器不该执行"写在 handler 里已经太晚——错误的进程先拿走了
  租约，真正的持有者反而执行不了，消息在消费者之间空转。game-demo 的两进程实跑里 16 个礼物 saga 产生了
  248 次投递、24 条卡住（GAME_DEMO_TEMPLATE §9.11.3）。`Admit` 排在 `SubscribeDataEngineStep` 的
  `Reserve` 和 `SubscribeMongoStep` 的 `Handle` 之前，返回错误就原样 nak、不留任何痕迹，消息再次投给
  durable 的任意消费者。**约束**：它必须便宜、无副作用，而且不能在所有进程上都拒绝——没有进程接受的
  命令会一直重投到 `MaxDeliver`。nil 接受一切，单进程部署无需改动。
  测试 `saga/step_admission_promises_test.go`。

## [v1.15.11] - 2026-09-19

### Added

- **二级索引可以按所有者分组**（随 U-0257；RR-20260919-10）。`versionstore.RedisIndex` 多一个
  `KeyOf func(T) string`：索引集合由值算出来，于是"欠 3 号游戏服的有哪些"是一次有界读，
  而不是"欠所有人的里面筛出我的"。`Key` 与 `KeyOf` 恰好设一个，构造时校验。
  新增 `IndexDueIn` / `IndexRemoveIn` 读写具名集合。
  **约束**：索引键只能取值里不会变的部分——一次写只碰它当时算出来的那个键，键变了旧条目就成孤儿。


## [v1.15.10] - 2026-09-19

### Fixed

- **一次 loader panic 不再让这个实体永远加载不了**（U-0254，C7；RR-20260919-08，T-148，**P1**）。
  `entity.ManagerAccess` 与 `dataengine/engine.EntityRepository` 的共享加载都把"删 flight +
  `close(done)`"写在加载之后、没有 defer；loader / store / builder / 解码 / `OnInitFinish` 里的一次
  panic 会跳过这两步，留下一个永不关闭的 flight。进程还活着（Nest 顶层会 recover），但这个 fullID
  从此不可用——后来的每个请求都等到自己的 ctx 超时，而且**重试永远进不了 loader**。
  两层都改成 defer 收尾（写入稳定结果 → 删 flight → close(done)）；panic 变成带原因的稳定错误交给
  waiter（他们在别的 goroutine 上，在那里重新 panic 等于让他们替别人把进程带走），leader 继续 panic。
- **缺失实体在 single / broadcast 分派处不再被解引用**（U-0255，C8；RR-20260919-09，T-149）。
  生产 getter 对缺失返回 `(nil,nil)`；single 因此返回 runtime nil pointer 而不是 `ErrEntityNotFound`，
  broadcast 的逐实体 recovery 在 `Touch` 之后，一个缺失 id 会**中止排在它后面的所有实体**。
  同包的 multi / multiGroup 早就是 `e != nil && e.Touch()`。测试用生产形状的替身——现有 mock 对缺失
  返回 error，恰好掩盖了这件事。
- **索引里没有记录的成员可以退休**（U-0256，C5；RR-20260919-07，T-150，**P1**）。
  新增 `(*RedisStore).IndexRemove(key)`：只动索引、不动值，给"条目指向的值不在了"这一种情形
  （删除的两步之间崩过一次）。这样的成员 score 最老、永远排在页首，会被每一页读到、每次被拒、
  然后继续留着——`limit` 越小占比越大。kit 的 platform 用它在 `ErrOrderInvalid` 分支退休条目。


## [v1.15.9] - 2026-09-19

### Added

- **版本化存储可以自带一个二级索引，索引和值是同一次写**（U-0253，C4；RR-20260919-04，T-147，**P1**）。
  `redis.CompareAndSetCommand.Index`（member / score / remove）走一个双 key 脚本：**compare 通过之后**
  才 `ZADD` / `ZREM`，没通过就一个字节都不动；score 以十进制字符串传给 Lua，不让它去解析 Go 的浮点序列化。
  `versionstore.RedisConfig.Index`（`Key` + `Entry(T) (score, include)`）让 `Create` / `Update` / `Delete`
  自动维护它，新增 `(*RedisStore).IndexDue(maxScore, limit)` 按分数从低到高读一页。
  `include` 是关键：索引是**待办清单**而不是键空间的副本——值变成终态的那一次写就把条目退休掉。
  存在的理由：调用方在存储外面维护这样一个索引，必然是两次写，中间崩一次就留下一条谁也枚举不到的记录
  （对 platform 来说是一笔后台永远找不到的已付款订单）。语义对着**真 Redis** 验证
  （`redis/cas_index_promises_test.go`，`ROOST_REDIS_TEST_ADDR`），因为 Go 替身里的"同一次写"是假的。
  **多 key 脚本要求两个 key 同槽**：Redis Cluster 部署需要在前缀里放 hash tag，调用方选键布局、调用方负责。

## [v1.15.8] - 2026-09-18


### Fixed

- **`mail.Claim` 交出信封的过期时刻**（U-0241，C4，RR-20260918-05，T-135，**P1**）。领取的预留此前只带租约
  `DeadlineUnix`，而一个按 Token 去重的发放侧必须知道这个身份要活到什么时候——它自己算不出来：发送方的 TTL 是
  另一个服务的**配置**，把资产正确性绑在配置上，意味着一次合法的配置改动就重新打开一次重复发奖，且没有测试会红
  （game-demo 的账本固定留 31 天并论证"长于 `send_ttl` 720h"，而 `send_ttl` 只要求为正数）。
  `Claim.ExpiresAtUnix` 取自信封，是"这封邮件彻底不可领"的那一刻——去重记录可以在此之后遗忘，一刻都不能更早。
  测试：`service/mail/claim_expiry_promises_test.go`。记录：`docs/bugfix/RR-20260918-05.md`。
- **`spatial.InterestConfig` 增加单观察者订阅预算**（U-0240，C6，RR-20260918-08，T-134，**行为变化**）。
  此前只校验半径与分带，而 `BlockIndex` 拦的是**整图**格数——两道闸防的不是同一件事。一个观察者订阅的格数是
  `(⌈2·LeaveRadius/BlockSize⌉+1)²`，`Bounds 10000×10000 / BlockSize 10 / LeaveRadius 1000` 是合法配置、
  构造成功、实际登记 **40,401 个 block**，而那是每次观察者移动都要差分的集合：配错了不报错，只是慢慢变慢。
  现在 `MaxObserverBlocks`（默认 `DefaultMaxObserverBlocks = 1024`）在**构造时**按算出来的最坏情况拒绝
  （饱和加法，且被地图裁剪），新错误 `ErrInterestBudget`——采样式的检查会通过所有启动检查然后在生产里拒绝。
  格边长≈视野半径的经典形状（9~16 格）不受影响。测试：`spatial/observer_budget_promises_test.go` 四条。
  记录：`docs/bugfix/RR-20260918-08.md`。
- **room：房间可以接入 pipelined 提交的持久化水位了**（U-0233，C4；RR-20260918-02，T-127）。`EntityBase.LastCommitLSN` 的契约是尚未达到 durable 水位的内容不外发，`SubscriptionCoordinator.SetDurableWatermark` 早就在，但 `RoomBroadcaster` 自建并私有持有 coordinator，而房间与管理器的配置都没有水位源字段、内部也不安装——走默认房间链的 pipelined 部署因此拿不到这道屏障，首次订阅快照、单体 flush、批量 flush 三条路径都会把尚未持久的内容发出去。新增 `RoomBroadcasterConfig.DurableWatermark` 与 `RoomManagerConfig.DurableWatermark`（部署写一次，管理器传给它创建的每个房间），在房间可被订阅、可启动 worker **之前**装进内部 coordinator；nil 仍是无门，正是同步持久提交的情形。没有暴露 coordinator 本身——那是把可变裸指针交出去。测试 `room/durable_watermark_promises_test.go`（受控水位模型 + 记录型 sink，非真实 WAL group-commit）。记录 `docs/bugfix/RR-20260918-02.md`。
- **saga：原生 Nest 步骤的完成效果现在有人消费**（U-0231，C4；RR-20260917-07，Wanted-04 转入，T-125）。`EmitCompletion` 把完成结果作为 Nest effect 提交，所以它经 Data Engine 的 outbox 到达**效果流**的 `<effect_prefix>.saga.result.<sagaID>`，而 `Assembly.Start` 只订了 `<saga_prefix>.result.>` 与 `<effect_prefix>.saga.start`——原生完成不匹配任何默认消费者，saga 停在 waiting 直到 deadline 补偿。新增对称的 `SubscribeNestCompletions` 与 `AssemblyConfig.NestResults`（留空时从 `Starts` 派生，durable 默认 `<start durable>-result`：共用 durable 就是共用游标）；第三条订阅失败会 Drain 掉前两条，`Stop` 一并排空。`ErrNotWaiting` / `ErrNotFound` / `ErrInvalidRecord` / `ErrDefinitionMissing` 判为 Permanent——`Complete` 对已记录的回执幂等，一条陈旧消息不该堵住消费者。测试 `saga/nest_completion_promises_test.go`（记录型 JetStream 替身，非真实 broker）。记录 `docs/bugfix/RR-20260917-07.md`。

### Added

- **`dataengine.SyncFieldMeta`：生成的同步字段词汇表**（M-12 / ARCH-06，承接 W-2026-09-18-02）。
  每字段的掩码常量是 DAO 包私有的，所以一个写在别处的 packer 能把掩码交给 `MarshalSync` 却看不进去——
  这对默认 packer 没有缺陷（掩码不透明地进出），对要打自己客户端协议的项目则是缺一份词汇表。
  类型与 `SyncFieldByName` / `SyncFieldsOf` 两个只读助手在框架侧，表由 codegen 生成在掩码旁边，两者不会漂。
  **bit 只在同一份生成产物内稳定**，跨越构建的东西按 Name / WireName 键——注释里写明了，因为导出内部常量
  等于承诺一个跨版本 ABI。记录：`docs/bugfix/ARCH-06-sync-field-vocabulary.md`。
- **新增 `attribute` 包：属性系统的框架半**（U-0230，C4；RR-20260917-06，T-124）。`AttrID` / `AttrValue` / `Meta` / `Profile`（生成的 profile 实现的接口）/ `Selector`（层）/ `Snapshot`（Profile 是副本，读者改不动容器里的那份）/ `Container`（Install / Live / Snapshot / Apply / Dirty / ClearDirty / Remove / Layers，并发安全）。roost-codegen 的 attribute 生成器此前引用这七个名字而三仓都不提供，整条 feature 生成出来就编译不过；生成器同时改成产出包级访问器，所以工程侧这些名字可以直接是本包类型的别名。`Container` 只负责按层存放与快照，**不做层间合成**——合成规则各游戏不同。测试 `attribute/container_promises_test.go`。记录 `docs/bugfix/RR-20260917-06.md`。

### Removed

- **`RemotePolicyCapable`、`GetEntityGroupFunc`、`EntityGroupRemote` / `Player` / `Alliance` / `Other` / `Cnt` 删除**(M-04,**破坏性**;前置 M-01～M-03)。Capable 的全部作用是把 kind 放进第一个锁档,而锁档现在就是 kind 的 category,所以它说不出 category 说不了的事;`GetEntityGroupFunc` 是把 category 映射成锁档的应用钩子,值即锁档之后不需要映射,顺带去掉一个消费方在管理器启停时反复赋值与置 nil 的包级可写变量;`EntityGroup*` 那套 0 到 3 的标度没有对应物,`EntityGroupCnt` 本就无人使用。
  `RemotePolicy` 只剩 None / Managed / Mirror,`RemoteCapable()` 变成 `Managed || Mirror`。`GetEntityGroup` 只剩一条路径:从 ID 取 kind、一次原子载入取档;未知 kind 带 remote 位归 remote 档,否则归新增的保留值 `EntityCategoryUnknown`(255,排最后,保守答案)。M-03 的"声明才走新路径"双路径消失,声明 category 只影响命名与校验。
  **消费方须同版本升级 core 与 codegen 并重新生成实体接线**;标记里的 `remote=capable` / `remote=true` 要改掉并为该 kind 选一个 category;对 `GetEntityGroupFunc` 的赋值要改写成 category 取值;`EntityCategoryRemote` 占用值 1,旧工程里用 1 的非远程 kind 要挪走。详见 `docs/bugfix/M-04-drop-capable-and-the-group-hook.md`。

### Added

- **`service/mail` / `service/session` / `service/match` 现在持有 RPC 接口与传输半**（M-11，ARCH-01 / ARCH-04 收尾）。`Mail` / `Session` / `Matchmaker` 接口（`//roost:rpc`）与 `servicerpc -emit transport` 生成的 `*_rpc_gen.go`（方法常量、wire 类型、`RegisterHandlers`、`BusClient`、`Capability` 包装、`CapabilityName` / `LocalCapabilityName`）从 roost-kit 搬入，只依赖 core；`Server` / `ClientMod` / `OwnerCapabilities` 的装配半留在 kit 并从 core 的接口生成。subject、方法名、wire 字段、capability 名不变。传输测试（两种实现行为一致、错误码穿透、身份参数）随迁。记录：`docs/bugfix/M-11-rpc-interfaces-into-core.md`。

- **`manager`：manager 生命周期引擎从 roost-kit 下沉 core**（M-09，ARCH-02；来源 `docs/bug/REVIEW-2026-09-16-04.md` §7）。`Order` 给出按 `DependsOn` 的稳定拓扑序（无依赖关系者保持注册序，环 / 缺失依赖 / 重名 / 空名 / nil 按名报错）；`Engine`（`NewEngine` / `Register` / `Provide` / `Start` / `Stop(ctx)`）按序启动、逆序停止，Start 失败只回滚已成功者并把回滚失败一起 `errors.Join`，Start 途中收到关停即中止余下管理器，Stop 优先 `IManagerStopperWithContext`、逐个报错、幂等；`ErrRegisterAfterStart`。指标名与可断言的错误片段不变。kit 的 `ManagerMod`（Mod 名、capability 登记）在 core v1.15.4 发版后改为包装引擎，步骤见 `docs/bugfix/M-09-manager-engine-into-core.md`。

- **`service/mail` 与 `service/session`：mail、session 领域实现从 roost-kit 下沉 core**（M-07 / M-08，ARCH-01 第二、三批；来源 `docs/bug/REVIEW-2026-09-16-04.md` §7）。mail：`Envelope` / `Entry` / `Mailbox` / `Claim` 等类型、邮箱状态机（已读 / 删除 / 三段式领取 / 淘汰）、`EnvelopeStore` / `MailboxStore` / `SendLedger` 接口、`Service` 与 `New`、`NewRedisStores` / `NewRedisEnvelopes`、errcode 段与 `Error()`；session：`Run` / `State` / `Claim` / `LedgerEntry` 等类型、`Service` 与 `New`（幂等 Enter、每 owner 一个活 run、资源恰好释放一次）、`Admin` 操作面、`NewRedisStores`、errcode 段与 `Error()`。行为、Redis key、JSON 字段、错误码、RPC 方法名全部不变；领域测试随迁；核心不引用 kit（`dependency_boundary_test`）。`Mail` / `Session` RPC 接口、生成传输、Mod 留在 kit；kit 侧改别名包并删重复实现等 core v1.15.4 发版后做，步骤见 `docs/bugfix/M-07-mail-domain-into-core.md`、`M-08-session-domain-into-core.md`。

- **`service/match` 与 `servicemetrics`：match 领域实现与服务计数契约从 roost-kit 下沉 core**（M-06，ARCH-01 第一批；来源 `docs/bug/REVIEW-2026-09-16-04.md` §7）。`Queue` / `Subject` / `Ticket` / `Match`、errcode 段 550101–550199 与 `Error()`、`Store` 接口、票据状态机 `NewStore`、`NewRedisStore`、`Grouping` / `FirstComeGrouping` / `ScoreWindowGrouping`、`Config` 原样迁入，新增 `NewMemoryStore(cfg)`；`servicemetrics.Reporter` / `Sink` / `Wrap` / `Recorder` 迁入。行为、持久化 key、JSON 字段、错误码、RPC 方法名全部不变；核心不引用 kit（`dependency_boundary_test`）。kit 侧的 `service/match` / `service/servicemetrics` 在 core v1.15.3 发版后改为别名包并删掉重复实现，步骤见 `docs/bugfix/M-06-match-domain-into-core.md`。

- **推荐的 entity category 分类常量**(M-04)。`EntityCategoryWorld` / `PlayerScoped` / `Player` / `Other` 接在 `EntityCategoryRemote` 之后,值即锁序;`EntityCategoryUnknown`(255)保留给本进程不认识的 kind,任何 kind 都不得注册进去。它们是常量不是要求 —— category 现在是完整 uint8,项目可以插值或在 Other 之后继续;唯一的要求是远程托管的 kind 必须在 `EntityCategoryRemote`,由 `ValidateEntityRegistry` 校验。`EntityCategoryName` 对这几个值内置了名字,不声明也能打出可读日志。
- **entity category 的声明式锁序**(M-03,重构;前置 M-01 / M-02)。新增 `EntityCategoryRemote`、`EntityCategoryDef`、`RegisterEntityCategories` / `MustRegisterEntityCategories`、`EntityCategoryName`、`ValidateEntityRegistry`。应用声明 category 之后,**category 的值就是锁序**,低的先锁,没有单独的 order 字段;`EntityCategoryRemote` 固定最低,因为"远程托管实体最先加锁"是唯一有物理依据的排序约束,持着本地互斥去等分布式锁会把那把锁挡过一次网络往返。
  锁序在注册期派生进 `[256]atomic.Uint32`,`GetEntityGroup` 一次原子载入即可;managed 的 kind 即便被声明在别的 category 也强制按 remote 档排,同时 `ValidateEntityRegistry` 把这个声明报成错误。未声明 category 的应用完全走原路径,行为一字不变。`ValidateEntityRegistry` 目前只校验不封表,封表那一半留给生成器接管注册的那一步。实施记录与采用前提见 `docs/bugfix/M-03-category-lock-order.md`。
- **P3b：装配下沉——六个包新增 `Assemble*`**，kit Mod 只剩配置解析、能力注册与生命周期转交（[记录](docs/history/P3b_mods.md)）。
  `redis/driver.Assemble`（客户端 + 同连接池的锁工厂）、`etcd/driver.Assemble`（客户端 + 发现 + 选举，`Ping` / `Start` / `Close` 取代 Mod 里的 `Raw().Status`）、
  `nats/driver.Assemble`（连接 + JetStream + RPC，`Close` 停 RPC 并限时 drain）、`dataengine/engine.Assemble`（Mongo 存储与远端投影绑定；`Start` 内含 WAL → projector → outbox → runtime 的构造与链式回滚）、
  `saga.Assemble`（存储 / 传输 / 引擎 + 定义注册；`Start` / `Stop` 内含两个 durable 消费者与引擎循环的 drain-then-stop）、`remoteentity.Assemble`（锁工厂 / Manager / 后端 / 归属存储；`Start(ctx, bus)` 内含 Validate → BindSync → Seal → EnsureRemoteStorage → RecoverOutbox → StartFinalizer 与回滚）。
  全部为新增导出，既有 API 不变。`scripts/perf/dataengine.sh` 从 kit 搬来并改到 core 包路径。
- M-01：新增 Core 依赖边界测试，扫描根模块全部 Go import（含测试和非当前 build tag 文件），拒绝 Kit/Skill/Service/Codegen 反向依赖与其他框架模块身份；嵌套模块作为独立消费者验收。

### Changed

- **category 离开 EntityID,注册表成为唯一权威**(M-02,重构;前置 M-01)。`ResolveEntityID` 与 `GetEntityGroup` 改为"从 ID 取 kind、再查注册表"拿 category,只有本进程不认识的 kind 才回落去读 ID 的低两位;注册表不再拒绝超出该字段宽度的 category。动因是目标形态要"category 的值就是锁序"、需要五档,而那两位加上 `EntityCategoryNone` 只剩三个可用值。
  **无数据迁移**:`makeEntityID` 仍然写 `category & EntityCategoryMask`,那两位降为无人读取的历史填充,只为让改动前后铸出的 ID 位级一致。`NormalizeFullID` 删掉一处自证的 category 对账(`meta.Category` 现在就来自注册表,只可能相等),`GetEntityCategoryFromID` 标记 Deprecated。没有删除任何导出符号,v1 消费方完全兼容。`category_taxonomy_promises_test.go` 修前红。实施记录见 `docs/bugfix/M-02-category-leaves-the-id.md`。
- **entity kind 注册表改为按 kind 的无锁定长表**(M-01,重构)。`factoryByKind` / `kindCategoryByKind` / `kindPolicyByKind` 三张 map 合成一条发布后不可变的 `entityKindEntry`,存在 `[256]atomic.Pointer` 里;`EntityKind` 是 uint8,所以读取是一次原子载入,不再取锁。动因是锁序热路径 `GetEntityGroup` 会在**已持有实体互斥**时查这张表(排序比较器每次比较两次、`maxLockedGroup` 每个已持有锁一次、广播分桶每个 id 一次),旧实现在那里取读写锁,既有每次查询的开销,也形成"先持实体互斥再取注册表锁"的获取边,让注册期的写锁能挡住锁序判断。
  写入语义逐条保留,含 category 冲突报错、policy 从 none 升级、反向部分声明被忽略、重复 builder panic;`GetEntityKindRemotePolicy` 里那条永远不可能触发的 builder 回落随结构消失;`factoryMu` 更名 `registryMu` 并降为只串行化写入的 `Mutex`。公开 API 的答案不变。`kind_registry_promises_test.go` 的无锁承诺修前红。实施记录与后续三步方案见 `docs/bugfix/M-01-entity-kind-registry.md`。
- **nest cast 的远程实体门禁改名为 `refuseUndeclaredRemoteTargets` 并删掉从未走通的 release 通道**。原 `prepareCastRemoteEntities` 声明返回 `entity.RemoteEntityRelease`，但对任何未声明的远程托管目标都只会报错、从不产生 release；`CastMulti` 里的 `prepared` / defer release / `addRemoteRelease`，`Msg.RemoteReleases` 字段及其 Clone、requeue 清零、dispatch 收尾的 `releaseRemoteEntities`，都是为这个不可能出现的值服务的死链，一并删除。门禁语义不变：远程托管实体必须在 dispatch 前以 RemoteAccess 声明并被预锁，cast 只复用已持有的锁，未声明即 `ErrRemoteWriteCapabilityDisabled`；函数注释写明了三种出口与为什么不在 handler 中途拿分布式锁。新增 `TestCastReusesARemoteManagedEntityDeclaredBeforeDispatch` 钉住"已声明即复用"这条唯一通路。
- **entity guard 的两处锁序校验共用一个实现**：`CheckContainAllLock` / `CheckContainAllIDs` 只差输入形态（实体 / ID），各自重复着"算最大已锁分组 + 逐个判分组"的逻辑；抽成 `maxLockedGroup` 与 `mayLock`，两个公开方法只剩输入遍历。行为不变，entity / nest 测试绿。
- **`security.RateLimiter` 的令牌桶改用 `golang.org/x/time/rate`**。公开 API 不变（`RateLimitConfig` / `Allow` / `AllowN` / `Stats` / `GC`），
  变的是桶的算术：补充从"每个 Interval 一次性加 Refill 个"变为连续补充（Refill/Interval 匀速，突发上限 Capacity），与 x/time/rate 一致；
  `n > Capacity` 的请求仍然直接拒绝且不消耗。保留的是 x/time/rate 没有的部分：按 key 分桶、MaxKeys 上限、空闲驱逐与 Stats。
  新增依赖 `golang.org/x/time`（kit 已间接依赖同版本）。测试新增连续补充与超额请求两条（在旧实现上确认变红）。

### Fixed

- **saga：步骤拒绝 / 重试用尽后的补偿在 Mongo 存储上落不下去**（U-0225，C2；game-demo 实跑发现，无 RR，T-119）。`applyCompletion` / `retryOrCompensate` 先给记录版本 +1，再交给 `beginCompensation`，后者又 +1；`MongoStore.Apply` 只接受 expected+1，于是每一个不可重试的步骤失败、每一次重试用尽都被拒为 `saga: invalid record`，saga 卡在 waiting、步骤按超时反复重发同一个拒绝。内存 store 只比对当前版本，单测一直绿。现在"加一次版本"与"填状态"拆开（`compensationState` / `retryState`），已加版的调用方直接填状态。显式 `Compensate` 与 deadline 路径本来就只加一次，不受影响。测试 `saga/compensation_version_promises_test.go`（Mongo 存储版引擎 + 两条纯函数版本步长）。记录 `docs/bugfix/U-0225-saga-compensation-version.md`。
- **manager：Start 成功后的交接在状态锁下判定，Stop 与最后一个 Start 交错不再漏清理**（U-0219，C2；RR-20260916-06，T-113）。旧引擎只在下一轮循环顶部检查关停，Stop 取走空的 started 后 Start 把刚成功的管理器追加并返回 nil。现在 stopping 已置时 Start 路径自己 `stopOne` 并返回 `start aborted by shutdown`（回滚失败一起 Join）。测试 `stop_during_last_start_promises_test.go`。记录 `docs/bugfix/RR-20260916-06.md`。
- **manager：Start 取快照即关闭 Register**（U-0220，C2；RR-20260916-07，T-114）。旧判据 `started != nil` 在首个管理器慢 Start 期间为假，晚到者被接纳却永不启动 / 停止；新增 `starting` 状态，Register 按它返回 `ErrRegisterAfterStart`。测试 `register_during_start_promises_test.go`。记录 `docs/bugfix/RR-20260916-07.md`。
- **service/match：`Queue.Key` 单射**（U-0221，C2；RR-20260917-01，T-115）。Mode / Partition 里的 `%`、`:` 转义（先 `%`），不含它们的键逐字节不变、零迁移；Candidates 跳过、Commit 拒绝 `ticket.Queue != queue` 的票。曾含 `%` 的 Mode / Partition 键会变。测试 `queue_key_collision_promises_test.go`。记录 `docs/bugfix/RR-20260917-01.md`。
- **service/match：ScoreWindow 距离与窗口无溢出**（U-0222，C2；RR-20260917-02，T-116）。`scoreDistance` 用无符号差，窗口 uint64 饱和乘加后 cap；**行为变化**：`InitialWindow` / `WidenPerSecond` / `MaxWindow` 为负返回 `ErrQueueInvalid`。测试 `score_window_overflow_promises_test.go`。记录 `docs/bugfix/RR-20260917-02.md`。
- **service/match：内存 Store 在边界深复制**（U-0223，C2；RR-20260917-03，T-117）。Enqueue 的输入 Subject 与 Ticket / Candidates / Commit / Match 的返回值不再与存储共享 Payload / Members / TicketIDs。测试 `result_ownership_promises_test.go`。记录 `docs/bugfix/RR-20260917-03.md`。
- **service/{mail,session,match} 的 transport 生成文件头改为稳定的 `-dir . -emit transport`**（审查 09-16 第五轮观察项）：M-11 首次生成时写入了实施机器的绝对路径，按包内 directive `-check` 会失败；已从各包目录重生成，正文不变。

- **syncstream Recover 的一致性判据从"位置"换成"修改代数"**(U-0216,C8;RR-20260916-04,T-110)。U-0214 在 provider 前后比较 epoch / 流是否存在 / latest,但 Append 后 DeleteStream、同 epoch 同 latest 的 Import 都能让位置回到原值而内容已变,旧捕获照样以更高序号提交。现在 `History` 维护进程内单调的 `revision`,每个成功改变流集合 / 链 / ACK / epoch / 序号地板的写锁路径推进它,`Recover` 前后只比这个代数;不持久化、不从快照带入。全局代数会因无关流的并发修改保守返回 `ErrRecoverStale`,代价是一次重试。`recover_replacement_promises_test.go`;记录 `docs/bugfix/RR-20260916-04.md`。
- **syncstream WAL 半尾截断在 Windows 上被拒(U-0211 复核补修,RR-20260915-07)**。补修原来在 `O_APPEND` 句柄上 `Truncate`,Windows 的 `FILE_APPEND_DATA` 句柄不能 `SetEndOfFile`,CI `windows-compatibility` 红。截断改为打开追加句柄之前按路径 `os.Truncate`,文件不存在时只重置标记;追加前仍先 fsync。Linux / macOS 行为不变。

- **JetStream 同步总线:同 topic 本地扇出,持久消费者身份含 Prefix**(U-0209、U-0210,C8;RR-20260916-03/02;T-103、T-104)。同一总线对同一 topic 的多次 Subscribe 曾各自用同名持久消费者 Consume,服务端按工作队列分摊、分片流无法重组;现在一个 topic 一个底层订阅 + 本地 handler 注册表(各自消息副本、panic 隔离、最后一位退订才停底层)。`durableSyncName` 对非默认 Prefix 把完整 subject 散列进身份,默认 Prefix 的名字逐字不变(已部署消费者游标不受影响);不同 Prefix 共用 Stream 的 Subjects 所有权仍未解决,部署上请各用一个 Stream。测试 `jetstream_fanout_promises_test.go`、`jetstream_durable_identity_promises_test.go`;记录 `docs/bugfix/RR-20260916-03.md`、`RR-20260916-02.md`。
- **syncstream History / journal 五处一致性修复**(U-0211～U-0215;C5 / C5 / C8 / C8 / C8;RR-20260915-06～09、RR-20260916-01;T-105～T-109)。
  WAL 恢复时忽略的半条尾部在首次续写前截断并 fsync;journal 的 Write / Sync / 发布出错即 fail-stop(新 `ErrHistoryJournalFailed`,重开 journal 恢复),副作用之前的失败仍可重试;绑定 journal 的 `Import` / `Restore` 先经 `Checkpoint` 发布再切换内存;`Recover` 在调用 provider 前观察、提交前核对,过期捕获返回新 `ErrRecoverStale`。
  **行为变化**:epoch 内新建(含清理后重建)的流从本 epoch 已分配的最大序号 +1 开始(`HistorySnapshot` 新增 `SequenceFloor`),不再固定为 1——同一身份清理后重建不会复用序号,旧 ACK / Resync 不再吞掉新内容;每流内部仍连续。每条流的第一个包是链起点,`BaseSequence` 为 0。**消费方若用"首包序号 == 1"判断链起点,必须改成判断 `BaseSequence == 0`**——core 内的 `skill/skillsync` applier 已随本轮一并修改(`publisher` 的 Sequence→Version 映射允许间隙,不受影响)。测试 `wal_tail_truncate_promises_test.go`、`journal_failstop_promises_test.go`、`import_durability_promises_test.go`、`recover_validation_promises_test.go`、`history_identity_promises_test.go`;记录 `docs/bugfix/RR-20260915-06.md`～`09.md`、`RR-20260916-01.md`。
- **room `SetDownstream` 是一次生命周期交接**(U-0208,C8;RR-20260915-05;T-102)。此前只替换 `envelopeSink.downstream`,而 room 的慢消费者回调在构造时注册在初始 sink 上:替换后新 sink 正常剔除、跑完自己的 `OnSlowConsumer`,room 的订阅和名额却不释放,旧 sink 的回调残留到 `Close`。现在同一实例直接返回;新 sink 先注册回调(失败则保留旧 sink 与旧回调并返回错误),成功后切换 downstream、解除旧注册、接管 unregister 所有权。测试 `downstream_handover_promises_test.go`;记录 `docs/bugfix/RR-20260915-05.md`。
- **room 慢连接剔除通知与批次结果分别交接**(U-0207,C8;RR-20260915-04;T-101)。剔除(删传输基线、标记 dead、`RemoveSession`)是不可撤销的副作用,此前它的通知只在整批成功时派发:剔除后剩余路由再遇临时错误 / 取消,`admitWithSlowConsumerPolicy` 把已累积的通知随错误丢弃,之后的重试跳过 dead 路由再也生成不出来,room 保留失效订阅与名额。现在错误出口带回已累积的通知,`AdmitRoomFrames` 失败批次仍不提交 plans / dirty,但两条出口都在释放 room 锁后派发;沿用 `pendingCallbacks` 的按 (room, session) 合并与 Close 丢弃策略。测试 `eviction_notice_promises_test.go`;记录 `docs/bugfix/RR-20260915-04.md`。
- **entitysync 持久化水位门槛覆盖所有出口,并跟着被捕获的内容走**(U-0206,C8;RR-20260915-03;T-100)。此前只有 `FlushSubject` 查 `LastCommitLSN > durableWatermark`,`Subscribe`(含 profile 切换)与 `Prepare → Distribute` 直接把未落盘的状态送出,而且前置检查与捕获之间还能落进新提交。现在 `entity.SubjectSyncUpdate` 新增 `CommitLSN`,在实体锁内随内容一起捕获(与 nest 锁内盖 LSN 同一把锁);协调器按 update 的 LSN 判门槛:`Subscribe` 推迟时保留原订阅并返回新的 `ErrDurabilityDeferred`,`DistributeBatch` 任一条推迟即整批 abort、状态保持 dirty,`FlushSubject` 契约不变。新计数 `entitysync_durability_gate_deferred_total{entry}`。测试 `durability_gate_promises_test.go`;记录 `docs/bugfix/RR-20260915-03.md`。
- **statesync 同 tick 的视图在第一次投影时就钉住**(U-0205,C8;RR-20260915-02;T-99)。U-0200 在提交时冻结视图,留下"两次准备之间、第一次提交之前"的窗口:不同视图的第二帧可能已被运输交付,Commit 被拒后 ACK 仍绑第一份,下一 tick 若回到第一份视图则 delta 零变化,客户端永远多 / 少一个对象且不会触发 resync。现在 `prepareLatest` 投影后钉住该 tick 的视图,同 tick 的所有准备复用它(兴趣 / 质量档变化顺延到下一 tick,`SetQualityTier` 注释同步);`commitPrepared` 拒绝分叉视图时同时进入全量恢复(forceFull + generation++)作为兜底。测试 `pinned_view_promises_test.go`;记录 `docs/bugfix/RR-20260915-02.md`,并更正 `RR-20260914-10.md` 里的"幂等收敛"说法。
- **statesync `ApplyDelta` 的上限只约束最终集合**(U-0204,C8;RR-20260915-01;T-98)。此前每一步操作后就用 `MaxObjects` / `MaxComponentsPerObject` 检查暂存集合,满容量替换(先 Create 新标识、后 Remove 旧标识)被误拒,成败取决于标识排序;而 `NewSnapshot` 校验的是最终集合、解码侧允许每帧 2× 操作数。现在循环内改为 2× 过程上界(最终合法的帧暂存至多 base + creates ≤ 2×Max),循环后再对最终集合按上限拒绝;对象与组件两层同形。测试 `replacement_capacity_promises_test.go`;记录 `docs/bugfix/RR-20260915-01.md`。
- **statesync:一个 tick 一个视图、ForceFull 不被旧 ACK 取消、单片重组守 MaxFrameBytes、LOD 刷新与发送相位无关**(U-0200～U-0203,C8;RR-20260914-10～13;T-94～T-97)。
  `prepare` 发现该 tick 已对本会话提交过就复用已发送的视图(同 tick 的兴趣变化延到下一 tick),`commitPrepared` 对同 tick 的不同视图按 `ErrPreparedFrameStale` 拒绝——迟到的 ACK 不再绑到被覆盖的基线;`prepare` 的 8 值返回改为 `prepareResult`。
  两个 ACK 入口不再清 `forceFull`,恢复意图只由当前 generation 的全量帧提交释放。`Reassembler` 单片快路径加同一个 `MaxFrameBytes` 比较。`LODProjector.refreshDue` 改为"本次发送与上次已提交发送是否跨过采样点",每区间至多一次、逐 tick 发送时与旧规则逐点等价。
  测试 `baseline_identity_promises_test.go`、`recovery_intent_promises_test.go`、`reassembly_limit_promises_test.go`、`lod_phase_promises_test.go`;记录 `docs/bugfix/RR-20260914-10.md`～`13.md`。
- **lockstep `SubmitInput` 先校验再去重**(U-0199,C8;T-93;用户复审提出,无 RR)。身份环是按客户端给的 `uint32` 帧号寻址的,而寻址排在窗口检查之前:一个必然被 `ErrFrameTooEarly` 拒绝的极大帧号照样给该座位分配了 129 槽的环;更要紧的是 `int(original) % replayWindowSize` 在 `int` 为 32 位的平台(GOARCH=386/arm)上溢出成负下标(`int32(4e9)%129 = -24`),索引即 panic,而 Room 是单 goroutine 驱动的。
  现在折叠与窗口检查前移、身份查找后移(等价:任何被记住的 original 恒满足 `original <= next+window`,`next` 只增而 `window` 构造后固定,所以越窗的 original 不可能有身份记录),新增 `replaySlotIndex` 在 `FrameID` 域取模,对任意 uint32 都落在环内。测试 `submit_validation_promises_test.go`;记录 `docs/bugfix/U-0199-submit-input-validation-order.md`。
- **LockstepBot 区分"已收到"与"已应用",回调失败不再丢批次尾帧**(U-0198,C8;RR-20260914-09;T-92)。Assembler 一次释放整个连续批次、游标立刻前移,而 `apply` 中途失败时既不保留剩余帧也不停用 Bot:重传被去重丢弃,后续新帧照常返回成功,模拟却少了尾帧。
  现在按失败点分开——`Simulate` 失败无法判断模拟推进了多少,Bot 进入 terminal(新 `ErrLockstepBotTerminal`、`Terminal()`),此后所有入口拒绝,由宿主重建;`SubmitInput` / `ReportHash` 失败只保留该帧与它未完成的那一步(连同已产出的 payload / hash),下次调用从那一步续做,不重跑已成功的 `Simulate`、不重新调用生产者。
  保留量由新配置 `MaxPendingApply`(默认 256)收界,超出转 terminal;跨调用保留的帧会复制 payload,不再别名 `HandleFrames` 调用方的缓冲区。新增 `PendingApply()`。测试 `lockstep_apply_promises_test.go`;记录 `docs/bugfix/RR-20260914-09.md`。
- **lockstep 去重身份表准入即有界**(U-0197,C8;RR-20260914-08;T-91)。U-0193 的 `accepted` 对所有过去的原帧号照单全收、只靠 `Advance` 按期限清理,两次 tick 之间灌入大量不同旧帧号会让它线性增长(实测 4096 条),下一次 Advance 还要遍历回收。
  改为每座位一个 `ReplayHorizon + MaxSubmitWindow + 1`(129)槽的固定环,按原帧号取模定位——任一时刻的合法原帧号两两不撞槽;超期限的原帧号既不查也不记(不记同时挡住"用很旧的帧号挤掉有效身份"),`Advance` 不再做清理遍历。超期限重传仍按首次折入,与 U-0193 一致。
  同机基准 `BenchmarkRoomTickTenPlayers` 从 7.0–12.7µs/op 降到 2.6–3.2µs/op(本机快照,不推算生产吞吐)。测试 `replay_window_promises_test.go`;记录 `docs/bugfix/RR-20260914-08.md`。
- **lockstep:输入重传幂等、追帧配置必须收敛、座位号非负且对齐 wire 条数上限**(U-0193～U-0196;C8 / C8 / C8 / C4;RR-20260914-04～07;T-87～T-90)。
  `Sequencer` 记每玩家 `ReplayHorizon`(64 帧)内的原始帧号身份,已入帧输入的迟到重传返回当初折入的帧号、不再入帧(此前折进下一帧,一次性操作执行两次);乱序未来帧与"显式输入覆盖迟到占位"不变。
  `NewRoom` 拒绝 `CatchupBatchFrames=1`(每 tick 产一帧,净补帧速度必须为正,否则永不切回 live)。`NewSequencer` 拒绝负座位(-1 是旁观者哨兵)和多于 `MaxFrameInputs` 的座位(否则生成自己的解码器拒绝的帧)。
  测试 `input_replay_promises_test.go`、`catchup_rate_promises_test.go`、`seat_identity_promises_test.go`;记录 `docs/bugfix/RR-20260914-04.md`～`07.md`。
- **`WAL.Sync` 在关闭进行中不再提前成功**(U-0190;C8;RR-20260914-01;T-84)。U-0185 的屏障把 `closed`(Close 已发起)当成 `doneCh`(writer 已排空):writer 持有未写批次时发起 Close,Sync 返回 nil 而 ticket 未完成。现在关闭进行中的 Sync 照常送屏障、按 ctx 等 writer 排空,排空后返回 WAL 的终止状态;干净关闭后仍是幂等 nil。测试 `sync_closing_promises_test.go`;记录 `docs/bugfix/RR-20260914-01.md`。
- **EnterShared / LeaveShared 的不确定结果与 Transfer 共用一套收尾**(U-0189;C8;RR-20260913-12;T-83)。
  `EnterSharedExpected` / `LeaveSharedExpected` 出错后不再一律恢复旧模式:失效本地 marker,用独立有界 ctx 重查权威;权威未变才恢复,权威已切到目标模式按成功同步 live 与 lease 并返回 nil,权威仍是我们但另一种 lease 则同步到权威所示模式并报错,查不到则经 Fenced 进 `Recovering` 冻结直到下一次权威读成功。
  `settleIndeterminateTransfer` 泛化为 `settleIndeterminateOwnership`。同一修复覆盖 RR-20260913-13(Leave 恢复后普通写重试卡在 `shared -> local_owned` 非法迁移),测试补连续三次准入。测试 `ownership_mode_indeterminate_promises_test.go`;记录 `docs/bugfix/RR-20260913-12.md`。
- **第七轮复核残余补修:删除水位覆盖全部 L1 写入口、L2 按版本删除、L2 冲突不降级、代际播种进程级单调**(归 U-0187 / U-0180 / U-0184;RR-20260913-01/05/02;T-74、T-81 更新)。
  `cache.StoreConfig.Superseded`(键不存在也判、与 Stale/Conflict 同锁)让 publish / loader fill / L2 回填共用删除水位,`ReadThrough.loadOne` 回填被拒时返回 L1 现值或 miss;
  `cache.ReadThroughOptions.FatalRemoteError` 分类出 IgnoreRemoteError 不得吞的错误,entity 配置为版本冲突;
  新增 `entity.RemoteSnapshotVersionedDeleter`,`remoteSnapshotL2Store.DeleteAtVersion` 用共享的精确十进制比较脚本只删不比自己新的;
  interest 代际播种取 `max(now, 本进程已发出+1)`。
  测试:`snapshot_delete_l2_promises_test.go`、`snapshot_l2_delete_promises_test.go`、`interest_generation_seed_promises_test.go`;记录见各 RR 文件末尾的"复核后的补修"。
- **WAL Sync 写屏障、Committer 可取消的停机等待、快照版本化删除与所有权转移的不确定态**(U-0185～U-0188;RR-20260912-01/02、RR-20260913-01/09;T-79～T-82)。
  `WAL.Sync` 先往写队列放一个屏障请求、收到答复再 fsync,返回 nil 即覆盖调用前所有已 Enqueue 的 ticket(此前只 fsync 文件,BatchDelay 窗口内的记录不在其中)。
  `Committer` 的 `flushMu` / `replayMu` 换成一格信号量,`Flush` / `Shutdown` 等待 replay 所有权时随调用方 ctx 取消。
  `RemoteSnapshotCache.DeleteAtVersion` 与 `Publish` 共用分片锁,比缓存新的删除不动、否则留 `TombstoneTTL`(默认 = 缓存 TTL)内的墓碑挡住更旧的快照;复制接收与提交后失效都按版本删。
  `TransferRemoteOwnership` 在 `TransferExpected` 出错后失效本地 marker 并用独立有界 ctx 重查权威:未变才恢复,已转移按成功 fence,查不到则 live 进 `Recovering` 冻结,直到下一次权威读成功才解冻。
  测试:`sync_barrier_promises_test.go`、`shutdown_deadline_promises_test.go`、`snapshot_delete_order_promises_test.go`、`snapshot_delete_version_promises_test.go`、`ownership_transfer_indeterminate_promises_test.go`;
  记录:`docs/bugfix/RR-20260912-02.md`、`RR-20260912-01.md`、`RR-20260913-01.md`、`RR-20260913-09.md`。
- **远程快照一致性、兴趣订阅代际与提交回调的异常控制流**(U-0180～U-0181、U-0183～U-0184;RR-20260913-02/05/06、RR-20260911-06;T-74/75/77/78)。`entity` 冷 L1 发布时先有界地问 L2 一次,同版本不同内容即 `ErrRemoteVersionConflict` 且不落写,L2 读失败仍按故障降级(此前 `IgnoreRemoteError` 把一致性错误当可用性错误吞掉,L1=B/L2=A 而发布方收到成功)。`cache` 新增 `StoreConfig.Conflict` 钩子与 `ErrConflictingWrite`,在 `AtomicLocalStore` 分片锁下与 `Stale` 一并判,L2 回填遇冲突不再覆盖已发布的同版本值。`remoteentity` 的兴趣 renew / release 带只增 `Generation`(计数器从时钟播种,重启复用 SID 不倒退),release 只删不落后的 lease,迟到的旧 release 不再取消新 renewal。`nest` 的 `Commit()` 改为返回 `error`,每个 AfterCommit 回调各自 recover、panic 记为 `ErrAfterCommitFailed` 但后续回调(含 TransactionReleased)继续执行;异步完成按该错误回复并计 `async_total{result="completion_failed"}`,饱和内联回退包进 `goroutine.SafeFunc`,不再让业务 panic 逃出泵 goroutine 带走进程。
  另补 U-0175(RR-20260913-08)的复核残留:`LoadAuthoritative` 的权威原值与 Publish 后的 L1 值各判一次 `Expired`,过期即 miss 且不进缓存;同版本相同内容但更晚的 `ExpiresAt` 允许前移。各条记录见 `docs/bugfix/`;仍未修的四项与原因见 `docs/bugfix/README.md` 末尾。
- **远程快照与所有权协议的六处边界**(U-0174～U-0179;RR-20260913-03/04/07/08/10/11,T-68～T-73)。`entity`:合并加载的等待名额在跟随者取消后归还(与 `cache` 的 U-0155 同一个错误,entity 自建了第二份合并逻辑);信封自己的 `ExpiresAt` 参与读取准入,过期即视为未命中,Monotonic 按契约回权威。`remoteentity`:L2 的同版本冲突比较纳入 schema 与 codec(checksum 只覆盖 payload 字节,换个 schema 就能改掉同一份字节的解释方式);L2 的 marker / route / version 改为精确十进制比较并逐字存储(Lua 数值是 float64,超过 2^53 相邻 uint64 会塌成同一个值,旧版本被接受、版本回退);marker lease 的 epoch 由脚本按十进制逐位加一并在写前校验可表示范围(原来拼接 Lua 数值,10^14 被 `%.14g` 渲染成 `1e+14` 并已落盘,此后不可解析);接收侧交叉校验 payload 身份与信封(只改 payload 里的 Scope 就能写进另一个 scope 的缓存)。
  两处红测试是先把测试替身改**忠实**才出现的:`snapshotRedisFake` 原来用 `ParseUint` 精确比较、比 Lua 脚本更"正确",`markerEvalStub` 原来用 Go 的精确格式化 —— 一个比被测实现更正确的替身会把缺陷藏起来。各条的方案取舍与边界见 `docs/bugfix/RR-20260913-*.md`;同一批登记里还有六项未修,原因列在 `docs/bugfix/README.md` 末尾。
- **finalizer 停止后晚到的 Close 不再被接受**(U-0173,C8;RR-20260911-03,T-67)。`deferRemoteClose` 靠 select 分支退出,而"队列还写得进去"和"ctx 已关闭"同时就绪时 Go 随机选,于是停止完成之后它仍可能交接成功并返回 nil;`batch.Close` 据此不再自清,entries、writeGate、ownership 读锁与 finalize slot 全留下,`finalizeOnce` 又不让重启来收拾。现在接入重试路径已有的同一道准入屏障:`retryMu` 下判 `stopping`,未停止才 `retryWG.Add(1)` 再发送,而最终 drain 在 `retryWG.Wait()` 之后 —— 被接纳的一定被排空,被拒绝的由调用方同步清理,每份 entries 和 slot 恰好一个清理者。`finalizer_stop_handoff_promises_test.go` 修前红(64 次里 33 次被接受)。修复记录见 `docs/bugfix/RR-20260911-03.md`。
- **Nest 停机会回答已接受的延迟同步请求**(U-0172,C5;RR-20260911-04,T-66)。带 delay 的 Request 进了延迟队列还没到期时 Shutdown,`OnDestroyWithContext` 取走队列后直接 `recycleMsg`,不向 `RetChan` 发任何东西:Shutdown 返回成功,调用方却在等自己的超时,停机被报成取消。入场被拒的路径一直答 `ErrNestStopped`,现在回收前给出同一个答案。队列是在 `m.mu` 下整体取走的,与延迟泵的弹出互斥,所以最多回复一次;通道容量是 1,发送不会阻塞。`delayed_shutdown_promises_test.go` 修前红。修复记录见 `docs/bugfix/RR-20260911-04.md`。
- **完成事务的等待者不再因缓存淘汰而崩溃**(U-0168,C8;RR-20260910-03,T-62)。等待者唤醒后重新查 `m.remote.txs[id]` 取终态,而准入的容量淘汰恰好挑"最旧的已关闭"记录 —— 正是刚把它唤醒的那一条;对 nil 指针取字段发生在 `txMu` 临界区,后面的 Unlock 不执行,调用方即使 recover,之后所有事务跟踪都被这把锁挡死。现在新增 `trackedRemoteTransaction` 返回 tracker,等待者全程用已持有的指针读:Go 的对象生命周期由引用决定,淘汰只是移出索引,于是淘汰重新只影响后续的历史查询。`transaction_wait_eviction_promises_test.go` 用"select 阻塞前会求值所有 channel 操作数"这一性质把等待者钉在"已取得 done、未读终态"的时刻,修前红。修复记录见 `docs/bugfix/RR-20260910-03.md`。
- **技能快照保存完成顺序,恢复不再按 ID 重排**(U-0169,C8;RR-20260910-04,T-63,P3)。`completedCastOrder` 是按完成顺序的队列、淘汰从队头走,而快照把 casts 按 ID 序列化、恢复又按这个顺序重建队列,于是"最旧的完成"被换成"最小的 ID",恢复前后同一批后续输入淘汰掉的是不同的 cast。现在 payload 增 `CompletedCastOrder` 并拷贝一份(不引用运行时切片),恢复抽成纯函数 `restoreCompletedCastOrder`:记录的顺序优先且过滤到确实恢复成终态的 cast,记录漏掉的按 ID 追加 —— 后者同时是旧快照的兼容路径,退化为它一直以来的行为。`completed_order_checkpoint_promises_test.go` 三组,回退验证已做。修复记录见 `docs/bugfix/RR-20260910-04.md`。
- **room 的默认扫描周期不再被极短 IdleTTL 推成零**(U-0163,C2;RR-20260910-01,T-57)。`SweepInterval` 留零时按 `min(IdleTTL/2, 30s)` 推导,而 `IdleTTL` 是整数纳秒:任何小于 2ns 的有效正值除以 2 都得 0,构造器却接受了这个 TTL,`Start` 里的 `time.NewTicker(0)` 随后在后台 goroutine panic —— 一个构造成功、启动也成功、然后把进程带走的配置。现在派生值取下限 `time.Nanosecond`;TTL 本身合法,只是"一半"在这个量级上无法表示,所以是给派生结果收界而不是拒绝输入,显式给的正 `SweepInterval` 一字不改。`sweep_interval_promises_test.go` 修前红。修复记录见 `docs/bugfix/RR-20260910-01.md`。
- **dataengine Assembly 停机没完成不再忘掉 Runtime**（U-0159，C8；RR-20260909-03，T-54）。`Shutdown` 的 context 先到期、outbox worker 还在退出时，此前无论结果都把 runtime 置 nil，重试看到 nil 直接答成功。现在只有 `Runtime.Shutdown` 返回 nil 才释放引用；`Runtime.Shutdown` 记住已停下的组件（projector / outbox 各一位），重试只等还没停的，projector 的 flush 只尝试一次（关掉之后没有可 flush 的，记录留在 WAL 下次启动重放）。`assembly_shutdown_promises_test.go` 修前红。修复记录见 `docs/bugfix/RR-20260909-03.md`。
- **接入文档版本快照与三仓措辞收尾**（U-0161；RR-20260909-01 残留）。Quickstart / README 的当前组合改为 core v1.15.2 / kit v1.14.3 / codegen v1.15.4，DEVELOPMENT_WORKSPACE 的"五个仓库 / 四仓"改为三仓，STATIC_REGISTRATION 的 once 守卫命名跟随 codegen v1.15.5 的生成形状。
- **cache 读穿透的等待名额在跟随者取消后归还**（U-0155，C2；RR-20260908-02，T-51）。`MaxWaitersPerKey` 此前计的是本次 load 期间累计进过门的次数：跟随者因取消 / 超时离开时 `waiters` 不减，慢后端叠加短超时重试会在首个 load 结束前一直以 `ErrLoadWaitersExceeded` 拒绝健康请求。现在取消路径在同一把锁下减回，且只对仍是当前 load 的 call 减（新一轮 load 不被误扣）。`read_through_waiters_promises_test.go` 修前红，真满额仍拒绝。修复记录见 `docs/bugfix/RR-20260908-02.md`。
- **接入文档与三仓发布组合一致**（U-0157；RR-20260909-01）。Quickstart 从固定 codegen v1.7.0 改为引用发布清单（当前 v1.15.3），`project new` 示例补上必填的 `-module`；`docs/README.md` 版本基线与 `DEVELOPMENT_WORKSPACE.md` 发布顺序改为三仓。
- **dataengine 流水线提交落盘后立刻唤醒投影，不再等 IdlePoll**（U-0146，C8；T-49）。`Projector.Commit`（严格）在同步落盘后 `signal()`，而 `Enqueue`（流水线）拿到票就返回、票完成时无人唤醒；事务释放触发的那次 ReplayPass 通常早于 fsync、什么也没看到，于是一批流水线提交的尾巴要等到 `IdlePoll`（默认 1 秒）才落 Mongo。
  现在 `Enqueue` 在票完成时 `signal()`。`pipelined_projection_promises_test.go` 两条：落盘后 500ms 内必须投影（BatchDelay 200ms 保证释放早于落盘，修前红）；两条路径对超大记录同样拒绝且不留下准入 / 不计提交。
- **saga / bus 的身份摘要不再丢弃 `json.Marshal` 错误**（U-0107，C5，B-14；T-44）。`commandDigest` / `completionDigest` / `DeadLetterEntry.requeueMsgID`
  改为返回错误：无法序列化的命令 / 回执 / 死信条目此前全部退化成 sha256(nil) 这一个摘要，同一个 ID 带不同载荷再来时被当成重投递、返回别人的完成结果。
  这不是理论风险——`Command` 含 `time.Time`，年份超出 [0,9999] 时 Marshal 报错而 `Validate` 只要求非零，红测试对着 mongotest 替身不需要放宽任何生产入参。
  现在 `MongoCommandInbox.Handle / Replay`、`DataEngineStepInbox.Bind / Reserve / Replay` 以 `ErrInvalidRecord` 拒绝，`MongoStore.Apply / CompletionRecorded` 在开事务前拒绝，
  `Bus.RequeueDeadLetters` 不发布该条目。`digest_promises_test.go` 三条（两条在修复前变红）、bus 一条护栏。不改 `Validate`。
- **nest 组迁移承诺测试去抖**（U-0047 的测试）：CI 的 `-race` 下工作线程会在第二个请求发出前完成第一个 join，"pending 时拒绝第二个请求"偶发失败。
  测试现在先持有实体锁再发第一个请求，让 pending 成为确定状态；不改运行时代码。
- **`scripts/gapmap.sh` 收尾不再 `git clean`**：采样后只还原被改动的**已跟踪**文件；未跟踪文件（比如正在写的测试）原样保留并提示。
  之前的版本把采样期间新建的一个测试文件删掉了（codegen U-0087 首版）。

### Changed（测试质量）

- **etcd / mongo 驱动的真机守卫测试**（U-0153，C2，`-tags integration`）。etcd/driver 自起单节点 etcd（PATH 上无 etcd 时明确 skip）：Get 不存在的键是 `ErrKeyNotFound`；mongo/driver 对集成环境的副本集验证部署校验放行、同名索引定义冲突在无迁移策略时原样上抛、有策略时丢弃重建，并自起单机 mongod 验证 production 模式拒绝单机。
  单机 mongod 8.0 也报告 `logicalSessionTimeoutMinutes`，所以 `client.go:93` 对受支持的服务端不可达，测试只记录探测结果。随 roost-kit `scripts/integration/dataengine-env.sh test` 的 core 段运行。
- **nil / 参数守卫收尾第三批：ownerroute、hotcode、migration、versionstore、mirror、spatial、robot/scenario、log、configdata、httpclient、nats、webroute**（U-0149，C2）。十二个包各一条 `*_promises_test.go`，共 107 条守卫回退 103 红；
  `hotcode/plugin.go:46` 需要真的 .so 插件（不可测）、`versionstore/redis_store.go:234` 只在读与 CAS 之间被改写时可达（竞态防御）、spatial 212 / 223 互为双份。configdata 的 json 名打平检查有两条镜像分支，要让带 cfg 标签的嵌入字段分别先到 / 后到各一例。
  actionflow 余三条沿用 U-0125 结论（冗余 / 不可达），`redis/driver/client.go:339`（单条 EvalBatchDurable 返回非 1 个结果）为防御性守卫，均保留。
- **nil / 参数守卫收尾第二批：robot/loadtest、ai、dataengine、mongo/driver、entity、saga、skill/skillsync**（U-0148，C2）。七个包各一条 `*_promises_test.go`，共 62 条守卫回退 55 红；
  3 处留真机（mongo/driver 的副本集 / 逻辑会话 / 索引冲突需要真实 hello 与索引响应），1 处防御（saga 同事务内回执消失），3 处与下游同哨兵记冗余（ai 谓词深度与 parseNode 同文本、dataengine 围栏回执与 `LeaseFence.Validate` 同哨兵、entity `NormalizeID` 的 none 检查与 `ResolveEntityKindCategory` 同文本）。saga 的异版本信封守卫要装一条合法命令才能钉住（零命令会被 `Validate` 以同哨兵接住）。
- **nil / 参数守卫收尾第一批：failurelog、robot、syncbus、servicerpc、lifecycle、admin、etcd、worker、goroutine**（U-0147，C2）。九个小包各一条 `*_promises_test.go`，共 40 条守卫回退 37 红；
  `servicerpc/client.go:189` 由两个选择器自身的空表守卫接住、`etcd/local_mirror.go:151` 与其后的类型断言同哨兵（均记冗余），`worker/pool.go:140` 在 workerNum == len(workers) 下不可达。亲和选择器的空表守卫要用带 Key 的选择器才能钉住（零值回落到轮询选择器的同一条守卫；失效后是取模除零）。
- **statesync 帧编解码的计数 / 大小上限守卫钉住**（U-0139，C2）。nightly gap map core `statesync` 20 条 7 条无覆盖。
  编码侧：65536 个对象拒绝而不是 uint16 回绕成"空帧"、帧大小上限在组件级也生效、`MaxObjects*2` 在编码侧拒绝；解码侧：头部声称的对象数 / 组件数 / 载荷长度在分配与读取之前按上限拒绝，答案是"超限"而不是"截断"。
  `codec_limits_promises_test.go` 两条；回退 7 处 6 红，`codec.go:44`（单对象 65536 个组件）因组件 TypeID 非零且唯一最多 65535 个、校验器先以重复拒绝，从编码路径到不了，记不可达保留。
- **cache 分层读 / 读穿透的失败与未命中传播、Redis 存储键校验、ref-hmap 旧版本拒绝的守卫钉住**（U-0138，C2）。nightly gap map core `cache` 20 条 8 条无覆盖。
  分层读把远端错误原样上抛、远端未命中不回填零值；读穿透在 L1 出错时不去 load，loader 未命中 / 出错时不写 L2 / L1，`Set` 在 L2 失败且未设 `IgnoreRemoteError` 时报错且不写 L1（宽松模式写入）；
  Redis hash / raw 存储无 key 函数拒绝写、hash 键或字段为空报 `ErrInvalidKey`（均不触达 Redis）；ref-hmap 的 Stale 判定拒绝旧版本且不改写已存值。
  `guards_promises_test.go` 四条；回退 8 处全红，采样 20 条无一无覆盖。
- **lockstep 房间 / 序列器配置、旁观者会话互斥与关闭后拒绝的守卫钉住**（U-0137，C2）。nightly gap map core `lockstep` 20 条 8 条无覆盖。
  无数据报发送器不能建房，序列器拒绝空座位表与重复座位；座位占用的会话不能再挂为旁观者（旁观者重复挂接幂等）、未挂接的旁观者不能追帧；关闭后拒绝挂接旁观者与哈希上报。
  `guards_promises_test.go` 三条；回退 8 处 7 红，`startCatchup` 的 closed 检查在 `Close` 清空全部表后从两个调用方都到不了（先撞 `ErrPlayerDetached`），记不可达保留。
- **app 反向停机 / 生命周期事件 / Mod 排序 / 能力批量注册的守卫钉住**（U-0136，C2）。nightly gap map core `app` 12 条 9 条无覆盖。
  停机上下文已结束时立刻带 ctx 错误返回、不再进入任何 Mod 的 Stop；`emitLifecycle` 对 nil app / 无 registry / lifecycle 能力缺失或类型不对报错；`sortMods` 拒绝 nil 条目、空名、重名；`ValidateServiceConfig(nil)` 拒绝；`RegisterBatch` 对空名与批内重名整批拒绝且不发布任何一项。
  `guards_promises_test.go` 四条；回退 9 处全红（停机那条因 Stop 跑在 goroutine 里，用"返回后 50ms 内未进入"钉住）。
- **nats/driver 请求上下文翻译、RPC 重试判定与 JetStream 客户端入口的守卫钉住**（U-0135，C2）。nightly gap map core `nats/driver` 18 条 9 条无覆盖。
  已到期 / 已取消的上下文分别翻译成 `ErrTimeout` / `ErrCancelled`；`QueueSubscribe` 缺队列名在触达连接前拒绝；`Call` 对不可重试错误立刻返回、不做退避（可重试的超时确实进入下一轮）；JetStream 客户端对 nil 客户端 / 未初始化 / nil 处理器各报其错。
  `guards_promises_test.go` 三条；回退 9 处全红，包内 18 条守卫无一无覆盖。
- **skill 执行器流程控制与内存宿主读取 / 伤害目录的守卫钉住**（U-0134，C2）。nightly gap map core `skill` 20 条 9 条无覆盖。
  分支条件求出非布尔值报 `ErrRuntimeTypeMismatch`；重复体出错或提前 finish 终止循环并原样交回（同步循环与调度回来的单次迭代各一份，后者不再排下一轮）；查询指向表外选择器、迭代任务局部槽 / 迭代数越界报 `ErrProgramInvariant`；
  内存宿主资源 / 位置 / 属性读取对未知实体报 `ErrEntityNotFound`，伤害命令在目录声明了别的公式策略时报 `ErrCombatPolicyUnsupported`、伤害类型未声明时报 `ErrCombatHandleInvalid` 且血量不变。
  `executor_flow_promises_test.go` 三条；回退 9 处全红。
- **skillcompose `BuildContract` 九处前置检查复核为冗余**（U-0133，C2 复核）。nightly gap map core `skill/skillcompose` 20 条 9 条无覆盖，全部是 `BuildContract` 里对空权威 / 空策略 / 空来源 / 重复来源 / 空特性 / 预算溢出 / 生命期溢出的早退；
  U-0063 已对每条输入有测试，守卫失效后末尾 `ValidateContract` 的同一规则仍以 `ErrContractInvalid` 拒绝——是双份而非缺口。保留早退（避免对无效输入算规范摘要），不另加测试。
- **bus 死信重投前置条件与管理命令 / nil 总线入口的守卫钉住**（U-0132，C2）。nightly gap map core `bus` 20 条 10 条无覆盖。
  无 NATS 客户端时 `RequeueDeadLetters` 报"客户端为空"且不动死信；存储只会整桶清除时，带 Limit 的部分重投在发布选中项后、清桶前停下并报"不支持部分删除"（未选中的死信保住），整桶重投照常；
  `RegisterAdminCommands` 缺注册表 / 缺总线各报其错且不留半注册；nil 总线的 `Handle` / `HandleRpc` / `EnableJetStreamRPC`（传真 JetStream 替身）报错不 panic。
  `dead_letter_admission_promises_test.go` 两条；回退 10 处 7 红，3 处冗余保留：`HandleRpc` 入口与订阅处各一份 stopping 检查互掩，`EnableJetStreamRPC` 的 nil js 检查与 `ensureJetStreamRPCStreams` 同哨兵。
- **nettransport 会话状态准入与控制面的守卫钉住**（U-0131，C2）。nightly gap map core `nettransport` 20 条 10 条无覆盖。
  `AdmitBatch` 对未注册、排空中（RemoveSession 后下游仍在发）、已失败（可靠通道出错）的会话分别报 `ErrSessionNotRegistered` / `AdmissionError{ErrSessionNotRegistered}` / `ErrSessionFailed`（带下游原因）且不入队；nil 传输报 `ErrTransportClosed`；
  控制面：无目标不能构造，nil 面 / 零会话拒绝，业务载荷在 UDP 处理器里是 `ErrInvalidControl`、在 `TryHandle` 里是 (false, nil)，`ServeUDP` 缺任一方拒绝。
  `session_state_promises_test.go` 两条；回退 10 处 9 红，`inspectDatagramBatch` 的批量上限被 `validateAndCopyDatagramBatch` 同一条件先挡住，记冗余保留；无调用方的 `validateDatagramBatch` 删除。
- **combatcomponent 宿主适配器 / 状态桥 / 持久化 DAO 的守卫钉住**（U-0130，C2）。nightly gap map core `skill/combatcomponent` 20 条 11 条无覆盖。
  事务外自开的分离事务开不起来（无 Committer）时 Apply / PayCosts / StatusBridge.Apply 交回错误而不是对 nil 结果做类型断言，且状态与修订号不变；读请求 / 费用支付对无战斗组件的实体与无属性映射的资源报具体错误且不动底值；
  `RestorePersisted` 拒绝落盘 id 与 DAO id 不符的文档、`Migrate` 只认 1 → 2（legacy JSON 可装入）、已有版本的 DAO 对掩码无字段的补丁拒绝而非生成空 `$set`；修改目录里查不到策略的 buff 实例报错。
  `guards_promises_test.go` 四条；回退 11 处全红。
- **etcd/driver 选举与本地镜像的入口守卫钉住**（U-0129，C2）。nightly gap map core `etcd/driver` 20 条 12 条无覆盖。
  未参选 / 会话已丢时 `Resign` 报 `ErrNotLeader`，无选举对象或后端无 leader 键时 `Leader` 报 `ErrElectionNoLeader`（有键时返回其值，主动 Resign 放弃领导权）；
  `NewLocalMirror` 拒绝 nil 客户端与不支持带修订号前缀快照的客户端；负 lease id、负期望修订号、nil 发布上下文在触达 etcd 前拒绝且不留下 Put / Txn；事务 nil 响应报错而非当失败；watch 关闭以 "watch closed" 记入 LastError。
  `guards_promises_test.go` 四条；回退 12 处 11 红，`client.go:46`（Get 无键 → ErrKeyNotFound）包着 `*clientv3.Client` 无法替身，留给真机集成。
- **remoteentity `Assemble` 依赖校验与写批次准入的守卫钉住**（U-0128，C2）。nightly gap map core `remoteentity` 20 条 12 条无覆盖。
  `Assemble` 五种缺失依赖（无 Redis、sid 为零、按 Loader 建后端却无 Mongo、既无 Loader 也无 Backend、Backend 不支持调用方持有的原子事务）各报明确错误，`Start` 拒绝未装配 / 无总线；
  `PrepareRemoteWriteBatch` 对 nil / 无配置管理器、未设后端（且不留下包装器）报 `ErrRemoteWriteCapabilityDisabled`，finalize 槽位耗尽报 `ErrRemoteOverloaded` 且批次 Close 后归还；nil 包装器 / nil 批次的 beginWrite / Commit 不 panic。
  `assemble_admission_promises_test.go` 两条；回退 12 处全红。守卫失效时准入会卡在写门 / nil 通道上，测试全部用带期限的 ctx，避免采样器每条等满 10 分钟。
- **syncstream `Import` 自洽校验簇与其余入口守卫钉住**（U-0127，C2）。U-0126 全包采样余下 22 条无覆盖。
  `Import` 十六种不自洽快照（版本、无 epoch、超 MaxStreams、无 topic、重复流、acked 越过 latest、有 latest 无包未全确认、包身份 / epoch / schema 不符、载荷超限、首个 delta 基线错、schema 跃迁无 full、full 带基线、latest 与包不符、无 schema）各报对应哨兵且被拒后 History 不变；
  `Save` / `Restore` 无存储、`Append` 无 topic 不建流、异 epoch 确认不落账、需要全量而无提供者、`BufferedPublisher` nil / 关闭后不再转发、适配器拒绝拖尾 JSON。
  `import_guards_promises_test.go` 四条；回退 22 处全红，包内 56 条守卫无一无覆盖。
- **syncstream WAL 回放与入口依赖的守卫钉住**（U-0126，C2）。nightly gap map core `syncstream` 20 条 12 条无覆盖。
  检查点之后的 WAL 行必须与检查点相接：异代格式、另一 epoch、追加跳号 / 重号、确认未知流 / 越过 latest、未知种类七种都以 `ErrInvalidSnapshot` 拒绝整次 `Load`，相接的续写被回放；
  `NewFileHistoryJournal` 空目录、nil 日志的 Record / Load / Checkpoint、`NewHistoryWithJournal(nil)`、无日志或 nil History 的 `Checkpoint`、nil 总线的 Publisher / Subscribe、nil handler 各报对应哨兵，被拒的 Subscribe 不装 handler。
  `wal_replay_promises_test.go` 两条；回退 12 处守卫全红。全包采样另见 22 条无覆盖（`Import` 快照自洽校验一簇 12 条），转 U-0127。
- **actionflow 任务计划归一化与运行器钩子重入的守卫钉住**（U-0125，C2）。classscan 后本地采样 `actionflow` 25 条 9 条无覆盖。
  `NormalizePlan` 拒绝无步骤、起点越界、动作为 none、后继越界，`PlanFrom` 拒绝 nil 指针与非计划参数；`startStep` 越界索引；
  `OnChanged` 钩子在启动途中结束任务时 `StartMission` 报 `ErrReentrantMutation` 且当前任务为空；构建器交出 nil 动作 → `ErrBuilderNil`。
  `plan_guards_promises_test.go` 四条；回退 9 处守卫 6 红，3 处不红且**保留**：`action_runner` 96 与 `finish` 内的同一重入判定重复、293 与 `registry.BuildAction` 的 nil 检查重复、`mission_runner` 81 在 ending 标志下不可达。
- **删掉 entity 里四处不可达的 kind 掩码守卫**（U-0123，死代码清理）。`EntityKind` 是 uint8、`EntityKindBits` 是 8，`uint64(kind) > EntityKindMask` 永远为假
  （`ResolveEntityKindCategory`、`registerEntityKindDefinitionLocked`、`BuildEntityID`、`makeEntityID`）。`ErrInvalidEntityKind` 保留为导出符号。
  U-0100 记的 actionflow 四处"防御性重复"复核后**保留**：三处是 finish / EndCurMission / 状态钩子之后的重入检查（钩子可达，只是没测到），一处是 start 的 nil 构建器守卫（注册表返回 nil 动作时可达）——它们是 C2 缺口，不是死代码。
- **skillsync Outbox 的 Put 与 PutBatch 准入判决一致性钉住**（U-0117，C8，classscan 观察 O-4）。两条路径各自实现总量 / 每流上限 / 重复判断；
  表驱动：同一批数据包顺序 Put 与一次 PutBatch 要么都接受且待发数量与字节数相同，要么以同一个哨兵拒绝且被拒的批不留半批（含"恰好到上限"与"批内重复"）。
  `outbox_batch_consistency_promises_test.go` 一条；把 PutBatch 的总量或每流边界各挪一位，测试即红。
- **gap map 新增 `classscan.py`**（三仓同一份拷贝）：给账本 C3 / C4 / C5 / C6 / C7 / C8 各一条可重复的启发式扫描，只列候选不下结论。首轮扫掉 117 个"未审"格（service 10 包、skill 5 包、codegen 16 包），
  无真洞，记四条观察（codegen 生成器默认路径与编排层重复字面量、account / platform 各自实现会话校验、render 依赖"渲染前已校验"、skillsync Put / PutBatch 双实现）。记录见账本 §9。
- **mongotest 替身的契约拒绝钉住**（U-0112，C2）。nightly gap map `mongo/mongotest` 20 条采样 13 条无覆盖。
  find-and-modify 未命中要求 after 镜像仍是 `ErrNotFound`、upsert 要求 before 镜像时插入但报 `ErrNotFound`；upsert / 替换 upsert（BulkWrite ReplaceOne）落到已有 `_id` → `ErrDuplicateKey` 且不覆盖；
  替换改 `_id`、nil 文档、无 `_id` 文档 → `ErrUnsupported`；`$and` 假分支不匹配无错、错分支上抛；`$exists` 非布尔、数字与字符串比大小（过滤与排序）、非 `$replaceWith` 的管道阶段都大声失败。
  `guards_promises_test.go` 三条；回退 13 处守卫 12 红，1 处不红：FindOneAndUpdate 未命中不 upsert 的早退与随后 "无 before 镜像" 的拒绝同为 `ErrNotFound`（冗余）。
- **robot/action 注册表、内建动作与 RegisterCall 的守卫钉住**（U-0111，C2）。nightly gap map `robot/action` 20 条采样 15 条无覆盖。
  nil 注册表的 Register / Run、未注册动作点名（含已注册列表）、已带 `robot action ` 前缀的错误不再包一层；`wait_push` 缺 `msg` 参数；`RegisterCall` 缺协议注册表、请求类型不是结构体、
  未连接时 "session not connected"、与别的动作共用 msg id 但声明了别的响应类型时 "unexpected response type"、参数转不成 int / uint（负数）/ float / bool 各自点名字段。
  `guards_promises_test.go` 三条；回退 15 处守卫 13 红，2 处不红：`wait_push` 的 `s == nil` 与 `Session.WaitPush` 的 nil 接收者同为 `ErrClosed`（冗余）；`assignScalar` 的 `!CanSet` 对只挑导出字段的 `callFieldsOf` 不可达。
- **nest Cast 辅助与 DispatchBroadcast 的守卫钉住**（U-0110，C2）。nightly gap map `nest` 20 条采样 13 条无覆盖。
  CastMulti：有派发消息无守卫作用域、有作用域无派发消息（各自隔离，此前一条测试同时缺两者）、消息无 getter、目标 id 为 0（门口文案 `index=0 id=0`，不是归一化错误）、
  getter 找不到实体（点名 index）、getter 返回的实体 id 与请求不一致导致锁序反转（`ErrCastDeadlockRisk`，用替换实体的 getter 构造）；CastTwo / CastThree 第二、三位类型不匹配各报 `ErrCastTypeMismatch`；
  `DispatchBroadcast` 空 id 列表 → `ErrInvalidMessage`。`cast_promises_test.go` 三条；回退 13 处守卫 9 红，4 处不红：CastTargetOne / CastTwo / CastThree 的 `len(es) != N || es[i] == nil` 三处被 CastMulti 的
  数量与 nil 检查前置（冗余），`prepareCastRemoteEntities` 的 "requires an active Nest dispatch" 在 CastMulti 已检查消息后不可达。
- **dataengine/engine 装配、删除准入与迁移收敛守卫钉住**（U-0109，C2）。nightly gap map `dataengine/engine` 20 条采样 14 条无覆盖（7 条在 P3b 新增的 `Assemble`）。
  `Assemble` 对缺 access / mongo / jetstream、远端投影只给一半、manager 不能应用远端提交各自点名拒绝，`Start` 拒绝未装配对象；outbox 发布器拒绝无 id / 无 topic / 无客户端的效果且不发到 JetStream；
  删除准入：运行时未配置、事务内无生成准备器或远端目标未声明、事务外无准备器、远端写能力缺失 / 批次为空 / 批次拒绝——全部 Immediate + 错误、不提交任何记录；
  装载迁移三次仍不收敛以 `ErrMigrationConflict` 放弃（用"提交即投影完成但什么都不写"的 SystemCommitter 替身，U-0101 留下的那条）。
  `assembly_promises_test.go` 两条、`entity_delete_promises_test.go` 四条、`entity_repository_migration_promises_test.go` 一条；回退 14 处守卫 13 红，
  1 处不红：`admitLocalEntityDelete` 事务体内 `tx == nil` 在 `RunIsolatedTransaction` 下不可达。
- **nestwal 编解码的版本守卫、条目上限与截断处理钉住**（U-0108，C2）。nightly gap map `nestwal` 20 条采样 14 条无覆盖。
  v1 写法拒绝回执、延迟生效效果、无远端提交的非 put 变更（三条各自独立触达，同一记录 v2 可写）；unset 路径与效果头超过 `maxEntryCount` 报错；
  v1 / v2 记录在每一个截断点解码都必须报错且不 panic（读侧 9 处字段错误分支）。`codec_promises_test.go` 三条；回退 14 处守卫各红。
- **redis 驱动与契约包的守卫钉住**（U-0105 / U-0106，C2，B-19）。gap map `redis/driver` 24 条采样 19 条、`redis` 2 条采样 2 条无覆盖。
  驱动：七个读接口（Get / HGet / LPop / RPop / ZScore / ZRank / ZRevRank）把 go-redis 的 `redis.Nil` 映射为契约 `ErrNil`、传输错误原样透传；
  `EvalDurable` 对形状不对的 WAITAOF 回复报错（用一个照本宣科的 RESP 假服务端对着真实 go-redis 连接跑）；`redisInteger` 拒绝超出 int64 的无符号值；
  pipeline `Exec` 容忍 `redis.Nil`、上抛真正的错误；分布式锁未持有时 Release / Extend 在发出脚本之前以 `ErrLockNotHeld` 拒绝（不多打往返、不翻成 uncertain）、
  nil 客户端 / 低于 1ms 的 TTL 以 `ErrDistLockConfig` 拒绝；`AutoExtendLock` 在 Acquire 失败时不启动看门狗、nil 接收者报错。
  契约：`CompareAndSet` 对 nil 客户端 / 空 key / nil Next 在发脚本之前拒绝，对非二元组回复报错。
  `client_promises_test.go` 四条、`lock_promises_test.go` 四条、`cas_promises_test.go` 两条；回退 21 处守卫 20 红，
  1 处不红：`EvalDurable` 的 `len(results) != 1` 是对 `EvalBatchDurable` 的断言（成功时结果数恒等于调用数），不可达。不改运行时代码。
- **entitysync 订阅协调器的入口守卫钉住**（U-0104，C2，B-18）。gap map `entitysync` 9 条采样 7 条无覆盖。
  Subscribe / Unsubscribe：nil 协调器、空 SubscriberRef、只有种类没有身份 → `ErrSubscriberInvalid`；nil 状态、主体 0 → `ErrSubscriptionSubject`，
  被拒绝的调用不留下任何成员、不碰已有订阅；FlushSubject 的同一组主体守卫；nil `ReliableEnvelopeSinkFunc` 与 `admitEnvelopes` 的 nil 汇
  → `ErrEnvelopeSinkRequired` 而不是调用 nil（后者从公开 API 不可达——三处前门已各自检查 sink——直接钉包内函数）。
  `subscription_promises_test.go` 四条；回退七处守卫各红。不改运行时代码。
- **gap map 采样器跳过 `*_gen.go`**（B-25）：生成文件是同一模板在每个包的实例，其守卫在模板所在处钉一次即可；采样器现在只统计不采样，并在包级与总计里报告跳过的守卫数。
- **cache ref_hmap 补丁路径解析与 JSON 存储的写规则钉住**（U-0102，C2，B-24 第四项）。nightly gap map 里 `cache` 20 条采样 15 条无覆盖。
  补丁路径：空段、未知字段、末段非标量、中途穿过非结构体字段、空计划——各自以 `ErrRefHMapUnsupported` 拒绝并点名路径，`Patch` 走同一
  解析器故坏路径不落一笔；嵌套深度超过 `MaxDepth` 在建布局时以 `ErrRefHMapMaxDepth` 拒绝；`RedisJSONStore` 的过期写 `ErrStaleWrite`
  与 key 函数缺失。`ref_hmap_patch_promises_test.go` 三条；回退八处守卫各红。
- **entity 种类 / 类别注册与创建参数归一化规则钉住**（U-0099，C2，B-24 首项）。nightly gap map 里 `entity` 20 条采样 19 条无覆盖。
  注册：kind 为 none、category 为 none / 超掩码（`ErrInvalidCategory`）、同 kind 改 category（拒绝且不改动已注册值）、同对幂等；
  解析：未注册 kind → `ErrInvalidEntityID`；`NormalizeID`：nil 参数、kind 与构建器不一致、category 与注册不一致、load 无 id、
  create 无 id → `ErrIDGeneratorRequired`、UniqueID 归一成完整 id；`resolveEntityBuilder` 的三条。`kind_registration_promises_test.go`
  四条；回退 16 处守卫 13 红，3 处不红：`uint64(kind) > EntityKindMask` 两处对 uint8 的 kind 不可达（掩码就是 255），
  `NormalizeID` 的"kind 为 none"与下游 `ResolveEntityKindCategory` 同文案冗余。
- **webroute 注册器的五种拒绝按文本钉住**（U-0075，C2）：无 handler、空 / 相对路径、PUT 与未知方法、重复路由、nil 注册器；
  方法与路径先归一化再校验。原测试只断言"有错"。`promises_test.go` 一条；回退三处守卫各红。
- **cache 读穿透的远端失败策略钉住**（U-0076，C2 / C8）。`IgnoreRemoteError` 是"L2 故障是否也是读者的故障"的唯一开关：默认
  远端 Get / 写回失败即调用方的错误且**不问 loader**；打开后降级到 L1 + loader，失败只计数。两个开关 × 两次 L2 调用四条
  路径此前无测试。`read_through_promises_test.go` 一条；回退两处守卫各红。
- **syncstream 文件日志的完整性守卫钉住**（U-0074，C2）。检查点正文声称另一个代数、最新代数的 WAL 文件丢失、WAL 行解不开——
  三种情况 `Load` 各自失败关闭而不是回放一份错的或残缺的历史；关闭后的 `Record` / `Load` / `Checkpoint` 与异版本的变更记录
  拒绝。`file_journal_promises_test.go` 两条；回退三处守卫各红，`Record` 入口的关闭检查与批锁内的同名检查互掩（冗余保留）。
- **robot 的 action / scenario 注册表与 spec 文档规则按文本钉住**（U-0073，C2，B-22 收尾）。nil / 空名 / 归一化后重名的 action
  与 scenario 各自拒绝；spec 的六种坏文档（原 `TestSpecRejectsBrokenDocuments` 只断言"有错"）现在各对应一条错误文本。
  两个 `promises_test.go`；回退六处守卫各红。
- **migration 的路径规划规则钉住**（U-0069，C2，B-22）。本地 gap map `migration` 20/20 无覆盖。实体与 DAO 两套注册表：
  版本区间非法（负数、不前进）、apply 为空、同一起点重复注册、**降级拒绝**、**跨过目标的步骤拒绝**（并断言拒绝时版本停在最后
  一个完整步骤之后）、路径缺失、DAO 集合为空。一次被静默接受的降级或跨越会改写文档版本号而不跑对应代码。
  `promises_test.go` 两条；回退五处守卫各红。
- **admin / hotcode / ownerroute 的入口守卫钉住**（U-0070 / U-0071 / U-0072，C2，B-22）：admin 无名定义 / 无 handler /
  无名命令 / 无名元数据；hotcode 空名、非函数、重复补丁点、Replace 的同样两条；ownerroute 非法键、nil 命令、路由无属主、
  路由不存在——且被拒绝的命令不到达本地执行器。三个 `promises_test.go`；回退七处守卫各红。
- **security 会话令牌的每一种伪造与畸形形态钉住**（U-0068，C2，B-22）。nightly gap map 里 `security` 5/5 无覆盖；此前只有
  往返、错玩家、过期三条。签发：玩家 0 / 空密钥拒绝；校验：空密钥、分段数不对、payload / 签名不是 base64、**用别的密钥签名**、
  **签名后改 payload**、payload 字段数不对、玩家 id 非数字 / 为零、过期时间非数字 / 非正、空 nonce、已过期、他人令牌。
  夹具用包内 `sign` 造"签名正确但 payload 违规"的令牌，让 payload 规则成为唯一拒绝理由；玩家 id 规则要用 `expectPlayerID=0`
  校验，否则被"期望玩家不符"掩盖。`session_token_promises_test.go` 一条（15 变异）；回退六处守卫各红。
- **statesync 增量帧编解码的结构规则与尺寸上限逐条钉住**（U-0067，C2，B-22）。nightly gap map 里 `statesync` 5/5 无覆盖，
  此前只有往返测试。`validateDeltaFrame` 十八条：元数据为零、未知帧类型、全量帧带基线、增量帧无基线 / 基线不早于 tick、
  无效对象引用、非法对象操作、重复对象、全量帧含非 create、create 无原型、remove 带组件、组件无类型 / 非法操作 / 重复、
  create 里删组件、set 无 schema、remove 带载荷；编解码两侧的 `MaxFrameBytes`、解码侧空数据 / `MaxComponentBytes` / 截断 /
  尾随字节 / 魔数 / 协议版本 / 保留位。`codec_promises_test.go` 两条；回退九处守卫各红（两处带初始化语句的守卫需要
  `if init; (cond) && false` 形式，首轮的中和方式编译不过、误记为绿）。
- **entity 的 `RemoteCommit.Validate` 二十五条规则逐条钉住**（U-0065，C2，B-22 首项）。首份 nightly gap map 里 `entity` 5/5
  无覆盖。远端提交是不变量①～④的宿主：身份三要素、版本连续、两个所有权 epoch、更新 / 删除的互斥形状、每条数据变更与
  提交头的实体 / 版本一致、重复变更 / 删除、快照的键 / 版本 / epoch / schema / 数据 / 校验和 / 重复、失效键的归属与重复。
  夹具用注册过的 kind 与 `BuildEntityID` 构造（快照键的有效性要求实体 id 编码了 kind）。`remote_commit_promises_test.go`
  一条（25 个变异）；回退七处守卫各红。
- **nest 锁组迁移请求的守卫钉住**（U-0062，C2，B-20 收尾）：加入 / 移动到组 0、getter 不认识的实体、一次迁移在途时的第二次
  请求（移动或离开）各自拒绝，且被拒绝的请求不在实体上留下 pending 标记。`group_transition_promises_test.go` 一条；
  回退三处守卫各红。
- **gap map 工具入库**：`scripts/gapmap/revertsample.py`（承诺回退采样器，带括号中和、按行号改、超时记 HANG）、
  `scripts/gapmap.sh`（对每个有测试的包采样并写 `gapmap-report.md`）、`nightly-gapmap` 工作流（每日 03:30 Asia/Shanghai，
  报告进 job summary 与 artifact，**从不阻塞**）。kit / service 携带同一份拷贝。
- **saga 引擎的状态机边界钉住**（U-0052，C2，B-20 第一项）。复测后剩余的十二条实质规则：重复注册定义、启动未知定义版本、
  启动请求的业务键 / 载荷上限、`List` 上限 1000、`Resume` 的 id 形状与"清除截止期又给截止期"互斥、截止期已过、只有
  Failed / ManualRequired 可恢复（Pending / Completed 报 `status N cannot resume`）、定义已卸载、恢复后新化身进入补偿；
  `Compensate` 在步骤结果在途时拒绝、无已完成步骤拒绝、已在补偿时幂等返回；`Complete` 的载荷上限与校验。夹具是**不跑
  协调循环的引擎**加直接落库的记录，让拒绝成为唯一可能发生的事。`engine_promises_test.go` 三条；回退十一处守卫各红。
- **saga 的引擎选项预算、Command / Completion 校验与 start / completion 效果编解码逐条钉住**（U-0050，C2）。回退采样 40 条
  守卫 34 条全绿。`NewEngine` 十一条跨参数规则各自按报错里的字段名断言（原先只有 publisher batch 一条且只看 `err != nil`）；
  `Command.Validate` 的十七个子句、`Completion.Validate` 的八个子句各用单字段变异钉住——一条巨型 `||` 条件里任一子句丢失只
  让对应用例变红；`NewStartEffect` / `DecodeStartEffect` / `DecodeCompletionEffect` 对空载荷、未来版本、六种坏请求、失败校验
  的拒绝。`promises_test.go` 四条；回退六处（含两处子句级）各红。
- **nest 的 Cast 类型契约、getter 数量契约、引擎单次生命周期与回滚事务的关闭 / 可比较性守卫钉住**（U-0047，C2）。回退采样
  45 条守卫 44 条全绿。`CastTargetOne/Two/Three` 要错类型时返回 `ErrCastTypeMismatch` 并带 id 与实际类型（此前断言失败会
  把零值实体交给处理器）；getter 返回数量与目标数不等是契约破坏而非"缺几个"；空目标、零 id 定位到下标；`NewEngine` 无
  getter 拒绝启动、`Shutdown` 后 `Start` 返回 `ErrNestStopped`；`RollbackTx` 提交后拒绝 undo / participant / mutation，
  undo owner / token / participant 必须可比较，participant 去重，混用新旧身份字段拒绝。`promises_test.go` 四条；回退五处
  守卫各红。
- **mirror 的信封线协议规则逐条钉住**（U-0045，C2）。回退采样 14 条守卫 13 条全绿。订阅侧：nil 消息、零 key、外层 / 内层
  topic 不一致、内层版本不一致、未知 op、载荷解不开——每条按错误文本断言且 store 收不到任何东西；旧发布者的省略形态
  （内层不带 topic / key / version / op、空载荷即删除）由外层消息补齐；发布侧：异 topic、零 key、未知 op、未初始化。
  `promises_test.go` 三条；回退四处守卫各红。
- **configdata 的定义校验与 auto 表标签规则逐条钉住**（U-0046，C2）。回退采样 45 条守卫 34 条全绿。表加载遇重复 key 必须
  报表名与 key（与 T-35 同类风险）；表 / 对象 / 自定义定义缺名、缺文件、缺 key 函数、类型不匹配、注册 nil 定义；auto 表
  十四条标签规则——嵌入字段带 cfg、带 json 名的嵌入藏 cfg、指针嵌入、被遮蔽 / 并列丢弃的 cfg 字段、非结构体值、非标量
  key、未导出字段、空 ref / 空 index 名、重复 ref、切片 ref、两种回调签名不匹配——原测试只断言"有错"，任一规则消失
  都不会红。`promises_test.go` 三条；回退五处守卫各红。
- **bus 的 RPC 响应信封解码、生命周期拒绝与死信能力拒绝补上测试**（U-0043，C2）。脚本化承诺回退对 `bus` 抽了 32 条守卫，
  其中未被任何测试钉住的：`decodeRPCResponse` 的"版本不支持 / 失败无错误体 / 成功无载荷"三条线协议规则、`Handle` 的
  module / name 必填、停止后 `HandleRpc` 拒绝、无 rpc / 无 JetStream 传输时的调用拒绝、无死信存储时三个死信操作的拒绝。
  `promises_test.go` 五条按错误文本钉住；回退版本检查与必填检查各自变红。
- **dataengine 的准入校验逐条钉住**（U-0044，C2）。`ValidateMutation` 的十二条形状规则（混用旧字段、文档键、put / patch /
  delete 各自的载荷约束、未知 kind）、远端提交与头部的交叉校验（实体 id、版本、delete 标志、本地数据混入）、
  `ValidateCommitRecord` 的效果 / 回执 / 租约围栏回执定位、`LeaseFence.Validate` 六个字段——此前只有"版本必须连续"和
  "patch 不回落全量"两条测试。这是 WAL 回放与投影之前的最后一道门，`validate_promises_test.go` 四条；回退五处守卫
  各自变红。**方法教训**：用 `&& false` 中和 `A || B` 形式的条件只中和最后一个析取项——必须加括号，否则回退法对
  "或"条件给出假阴性（首轮把 `bus` 的重启拒绝误判为无覆盖）。

### Added

- **`syncbus.DeliveryIDs`**：SyncMsg 投递身份的唯一生成器——进程唯一的随机前缀 + 单调序号，
  `"<kind>:<random>:<n>"`。此前只有 `PatchSyncer` 自己拼这个格式；`mirror.Replicator` 与
  roost-kit 的 `syncstream.Publisher` 根本不填 `MessageID`，JetStream 传输于是退回到
  `(topic, key, version, sid, part)` 元组当去重键。元组不是身份：同 key 同 version 的 upsert 与
  delete 共享一个键、不设 sid 的发布者根本没有键、重启后重发同一序号的发布者与"过去的自己"撞键——
  第二条在 broker 去重窗口内被静默丢弃。这正是 FEATURE_LOGIC M1 第 1–3 条要求的东西。现在三条
  发布路径共用一个规则；`PatchSyncer` 改为委托给它，格式不变。收敛单元 U-0009。
- **`docs/history/ledger.md` 收敛覆盖账本**：bug 收敛的工作单元协议（一次会话、一个包、
  一个缺陷类、四件固定产出、回退验证）、包 × 八类缺陷的覆盖矩阵、待开单元与单元日志。
  八个缺陷类来自 2026-09-02 审计的实际产出率。
- **`docs/TROUBLESHOOTING.md` 问题快速定位**：按症状索引，每行给出原因、看哪里、怎么处理。
  与收敛循环绑定：每个工作单元必须为它加一行。
- `docs/ROADMAP.md` 增加 FEATURE_LOGIC M1–M9 修改包的状态表（按代码检索判定，非测试证据）：
  M1 部分、M4 未开始、其余已实现但欠回退验证。

### Fixed

- **`mirror.Replicator` 发布的消息没有投递身份**。`Publish` 与 `PublishDelete` 都不填
  `MessageID`，也不填 `FromSid`，所以在 JetStream 上没有任何去重键；若某天填了 sid，同 key 同
  version 的 upsert 与 delete 又会共享一个键。现在每次发布经 `syncbus.DeliveryIDs` 取独立身份，
  新增测试断言四次发布（含同内容重发与"另一个进程"的发布者）身份两两不同。收敛单元 U-0009。

### Changed

- **发布链加入 roost-service**：`core → kit → skill → service → codegen`。
  `DEVELOPMENT_WORKSPACE.md`、`ROADMAP.md`、`docs/README.md` 同步；`docs/README.md`
  的版本基线更新为当前正式 tag（此前仍写 v1.10.0，且运行时组成漏掉 service）。
- **go 指令 1.25.0 → 1.27.0**，与 roost-kit / roost-codegen / roost-service 和
  `go.work` 统一。取 1.27.0 而不是最新的 1.27.1：一个补丁级的 go 指令什么都买不到，
  还会让停在 1.27.0 的工具链去下载一个新工具链。

  **代价写在明处**：这是每个消费方都要满足的工具链下限。roost-codegen 的
  `ci/framework-release.yaml` 里声明的 consumer lane 因此从 `[1.25.x, 1.26.x]` 变成
  `[1.27.x]` —— Go 1.25/1.26 的工具链构建不了本仓，那两条 lane 不是"没测"而是
  "不可能通过"。

### Added（`IRedis` 新增方法：对接口的实现者是破坏性变更）

- **`redis.IRedis` 增加 `MGet(ctx, keys ...string) ([][]byte, error)`**：一次往返读多个
  key。此前接口里没有它，于是需要批量读的调用方只能把 `MGET` 包进一行 Lua 脚本走
  `Eval`——`roost-service/mail` 的信封批量读原本就是这么写的。代价不是难看，是**测试
  盲区**：Go 的测试替身无法求值脚本，所以脚本文本里的缺陷对整个单测套件不可见。方法
  上了接口，脚本就没了。

  契约里有两条写进了文档并由测试钉住：

  - **结果是按位置的**：`len(result)` 恒等于 `len(keys)`，缺失的 key 对应 `nil` 元素。
    是切片而不是 map，因为 map 会把缺失的 key 直接省掉，那样"回复因别的原因变短"就
    和"这些 key 本来就不在"无法区分了——能比较长度的调用方可以发现截断,拿到 map 的
    不能。
  - **零个 key 不发往服务端**：`MGET` 不带参数在 Redis 里是错误，因此若把空列表透传
    过去，一个再普通不过的空页都会失败。

  **这是对接口实现者的破坏性变更。** 本仓内的三个求值型测试替身
  （`cache`/`failurelog`/`bus`）已补上实现，生产实现在 `roost-kit/redis`，
  另需 kit 同步发布。仓外的实现者需要补一个方法。

- `scripts/pretag.sh`（四仓各一份）：打 tag **之前**的发布预检。由 tag push 触发的
  CI 运行在 tag 已存在于远端之后，能报告问题但阻止不了——core 因此出现过一个已推送的
  `v2.0.0`，而 module 路径没有 `/v2` 后缀，任何消费者都选不到它。脚本检查 tag major
  与 module 路径后缀一致、tag 未存在（本地与远端）、`go.mod` 无 replace、工作区干净、
  以及 `GOWORK=off` 下 build/vet/test 通过。

### Fixed（破坏性：`cache` 的陈旧写与分层 TTL 语义）

两条都是"静默降级"类缺陷：框架把一个未发生的写、和一个未配置的选项，都当成了成功。

- **`cache`：被判定陈旧的 `Set` 现在返回 `ErrStaleWrite`，不再静默返回 `nil`。**
  三处写入点（`AtomicLocalStore`、`RedisJSONStore`、`RefHMapStore`）此前在
  `StaleFunc` 判定新值更旧时丢弃写入并**报告成功**，调用方无法区分"已写入"和
  "被丢弃"。这正是"取消操作返回 OK、而存储里仍是旧状态"这类 bug 的框架级源头。
  **升级影响**：把陈旧丢弃视为正常结果的调用方需要显式
  `errors.Is(err, cache.ErrStaleWrite)` 容忍它——core 内唯一这样的调用方
  `RemoteSnapshotCache.Publish` 已按此改写（输给更新的快照本就是单点发布的期望结果，
  且 `notify` 只唤醒目标 ≤ 本版本的等待者，更新的存储值同样满足它们）。
  同时把 `StaleFunc` 的并发语义写进文档：`AtomicLocalStore` 在分片锁内求值，比较与
  写入是原子的；而 `RedisJSONStore`/`RefHMapStore` 的读与写是两次独立往返，谓词
  **仅供参考**——等版本的两个写入方都会通过它。需要"败者必须被拒绝"时用
  `redis.CompareAndSet`。
- **`cache`：`LayeredStore` 的 `ttl <= 0` 从"永不过期"改为"不从 L1 供读"。**
  原语义把"没配 TTL"变成了一个**永不回源的一级缓存**——而 `viper.GetDuration` 对
  缺失的配置键正好返回 0，于是漏配一个选项就让每个副本读自己私有的、永久陈旧的视图。
  新语义退化为"不做 L1 缓存"，是安全的那一侧；`Get` 在无 remote 时本就短路使用
  local，因此只有 local 的 store 不受影响。**升级影响**：依赖 `ttl=0` 表示
  "local 即权威、永不过期"的调用方需要显式传一个足够大的 TTL。
- `cache`：`LayeredStore` 回填 L1 失败不再被完全丢弃，改计
  `cache.layered.backfill_failed.total`（`ErrStaleWrite` 不计入——它意味着已缓存了
  更新的值，是期望结果）。此前一个永不接受回填的 local store 会让每次读都回源，
  且没有任何信号。

以上三处此前**没有任何测试覆盖**（改完行为后全量测试仍然全绿）。已补 6 条回归测试，
并逐条做过变异验证：恢复静默 `return nil`、恢复 `ttl<=0` 即永久有效、以及把 L1 整个
关掉，三种变异都精确打红对应断言。

### Changed（破坏性：Go 模块路径改为 roost-core）

模块路径从 `github.com/tjbdwanghaibo/cube-core` 改为 `github.com/tjbdwanghaibo/roost-core`，
版本号延续（本版 v1.10.0）。GitHub 仓库早已改名为 roost-core，旧模块路径一直靠 301 重定向
存活——模块名与仓库名不一致本身就是最大的历史包袱。升级只需全局替换 import 路径。
新路径下没有旧 tag：`go.work` 联调需要**指定版本**的 replace，见
`docs/DEVELOPMENT_WORKSPACE.md`。

- 进程级默认名 `"cube"` → `"roost"`：`bus` 主题前缀、日志文件基名。
- JetStream RPC 默认流名 `CUBE_RPC_REQUESTS/RESPONSES` → `ROOST_RPC_REQUESTS/RESPONSES`。
  流名是 broker 侧持久状态：默认配置的部署升级后会在新流上继续，旧流中的消息不会被读到；
  滚动升级前请显式配置流名与前缀，或接受切换到新流。
- Redis key 前缀默认值 `cube:redisdao`（`cache` ref-hmap 快照缓存）、`cube:bus`（`bus` 可靠消费 inbox 去重）
  → `roost:redisdao`、`roost:bus`。**不做兼容读取**：升级后的进程不会读旧前缀下的 key。缓存条目会
  自然重建；inbox 去重状态在升级窗口内失效，期间已消费过的消息可能再投递一次——需要零重复的部署
  请在升级前显式配置前缀沿用旧值，或在升级窗口停写。
- 文档全部改为 roost 命名并以最新实现为准；过期的"已发布基线 v1.8.0"段落改为 v1.10.0。

### Changed（破坏性：包与标识符重命名，无行为变化）

一批只描述"机制"的包名换成描述"职责"的名字。旧名字里 `sync` / `replication` /
`replica` 三个词互相混淆——它们分别指模块间同步总线、房间状态复制和跨服实体本地
镜像，读代码时无法从名字区分；`ctx` 与标准库 `context` 的惯用别名冲突；`obs` 包里
只有指标，没有 tracing 也没有 logging。升级只需替换 import 路径与包限定名。

| 旧包 | 新包 | 为什么改 |
| --- | --- | --- |
| `sync` | `syncbus` | 与标准库 `sync` 同名，且它是一条总线，不是同步原语 |
| `replication` | `statesync` | 它做的是房间状态同步（delta+LOD），不是数据库复制 |
| `replica` | `mirror` | 它是订阅-应用回环的本地镜像，与 `replication` 无关 |
| `ctx` | `fctx` | 与标准库 `context` 的惯用别名 `ctx` 冲突 |
| `obs` | `metrics` | 包里只有指标，`obs` 名不副实 |
| `query` | `index` | 它是二级索引，不是查询语言 |
| `taskflow` | `actionflow` | 与 `Action`/`ActionGroup` 的实际类型名对齐 |
| `misc` | `goroutine` + `container` + `misc` | 按职责三分 |

`misc` 的三分：`goroutine` 收协程原语（`GoID`、`SafeFunc*`、`MPSCQueue`、`TaskPool`、
`ParallelSlice/Map`），`container` 收通用容器（`BucketHolder`、`KeyMap`、`ObjectPool`、
`TopologicalSortCache`），`misc` 只留真正跨包的小工具（`Hash64` 用于 worker/nest 分片、
`Integer` 泛型约束）——`container` 依赖 `misc` 的这两样。

capability 常量：`ModObs` → `ModMetrics`（值 `"obs"` → `"metrics"`）。

文件级重命名（名字不指示内容的）：`dataengine/model.go` → `mutation_types.go`、
`statesync/types.go` → `frame_limits.go`、`actionflow/types.go` → `action_types.go`、
`entity/types.go` → `entity_kind.go`、`saga/model.go` → `record.go`、
`mirror/replica.go` → `envelope.go`、`cache/replica.go` → `cache/mirror.go`。


### Changed（测试质量：997 个 Test 函数的机器普查，两类形态清零）
- **零断言测试 6 条重写**（另 3 条经核实是对带断言 helper 的合理委托）。其中
  `container.TestObjectPool` 带着一个**空的 `if` 体**和"我不确定语义"的注释，
  `lock.TestReentrantMutex_Basic` 只证明"Lock 两次不死锁"（换成 no-op 也能过），
  `nest` 的 ticker 测试名字承诺 panic 安全而唯一"断言"是进程没崩。重写后钉住的是
  真实设计属性：ObjectPool 的 freelist **不 reset**、`Put` 在两列表间移动、`Clear`
  丢弃而 `Release` 归还；ReentrantMutex 的**内层 Unlock 不释放锁**、`TryLock` 只对
  owner 可重入、非 owner Unlock panic；ticker 的 `SafeFunc` 是**每回调**而非每 tick，
  且被 Stop 过的 ticker 不能再 Start。每条都用变异测试验证过（破坏实现 → 断言精确打红）。
- **以 `go test` 超时当失败信号的 21 处消除**。测试主体顶层的裸通道接收会让属性
  破坏表现为挂 10 分钟后一份堆栈，而不是一句话——这正是本轮 F9/F11 认定的坏信号
  形态。改为每包一份泛型 `awaitChan(t, ch, what)`：5 秒上界，失败时说明在等什么。
- `container.ObjectPool` 补上未声明的并发约束：它内嵌 `sync.Pool`（并发安全）
  但 `workList`/`freeList` 是裸 slice，跨 goroutine 共享会 race。名字和内嵌的
  `sync.Pool` 都在诱导相反的假设，故写进包注释。

### Fixed（独立复审 F5/F6/F7/F9/F11/F12/F13/F15/F16，均带"无修复即红"验证过的回归测试）
- **`bus`：Start 失败的清理不再持生命周期锁排空**。`StopWithContext` 早已为此重构过（锁内取走资源、解锁后拆卸），因为一个 worker 的业务 handler 可能回调 `Handle`/`HandleRpc`/`Stop`——这些都取 `lifeMu`，持锁等待它们排空会**无上界死锁**；但 Start 的失败清理路径没跟上，仍在锁内以 `context.Background()` 等待 pool 排空。现 `Start` 拆为薄壳 + `startLocked`，失败时返回拆卸闭包由外层在解锁后执行，且与 Stop 共用同一条 `stopResources` 实现，两条路径不会再各自漂移。
- **`nest`：pipelined 完成链的释放从"调用方义务"变为"defer 保障"**。链节在 pump 决定归属**之前**就已占好（必须在实体锁内 link 才能保证链序 == 提交序），但 pump 满时的降级路径把释放义务转交调用方，而调用方的 `releaseLocks() → <-ticket.Done() → runInline(...)` 之间没有兜底：中途展开会让 `order.mine` 永不 close，该实体**后续每次 pipelined 完成都永久阻塞**在 `await()`（无超时、无指标、map 项也不回收）。现 `release()` 用 `sync.Once` 幂等、降级分支返回释放闭包、调用方立即 `defer`。
- **`app`：生产配置校验新增 `ops.addr` 暴露面检查**。生产校验此前覆盖 admin token、密钥、限流、WAL durable，却不约束运维端点的绑定地址——README 的"默认只监听 127.0.0.1"是默认值而非强制，所以生产可以把带 admin 的端点绑到 `0.0.0.0` 而校验器不反对。现要求回环，或显式 `ops.allow_public_addr=true`（沿用 `allow_dev_token` 的声明式形态）。
- **`cache/ref_hmap`：反射类型树改为每 store 构建一次**。`plan()` 原先在**每次** Get/Set/Delete/Patch 都重走整棵嵌套 struct 的类型树——而 `V` 是 store 的类型参数、`Name`/`Prefix`/`MaxDepth` 是 store 配置，整个结构在 store 生命周期内恒定，每次真正变化的只有实体 key。关键观察是节点里存的 key 实为"前缀（含实体 key）+ 由类型唯一决定的后缀"，故拆为 `refHMapNode.suffix` 与 `refHMapPlan.base`，类型树经 `sync.Once` 缓存（错误一并缓存，不支持的类型每次调用照样报错）。实测 4 字段 / 3 层嵌套：`plan()` 2874ns/31 allocs → **136.5ns/4 allocs**，完整 `Get` 4615ns/59 allocs → **2819ns/38 allocs**。**Redis key 格式逐字节不变**，并由回归测试钉住（挪一个字节就会让既有缓存实体全部失联）。
- **`entity`：`EntityGuard.Entities()` 不再交出锁作用域账本本体**。`eMap` 驱动 nest 的锁序与重入判定，业务代码一次 `delete` 即可静默破坏死锁预防。新增零分配访问器 `Guarded(id)`/`GuardedCount()` 并迁移全部 6 处框架内调用（含 `nest_dispatch` 热路径的 `useTryLock`），`Entities()` 改为返回快照——安全与性能同时改善：账本不可被外部改坏，而热路径本就只问"在不在/有几个"，现在一次分配都不做。
- **`dataengine`：非 strict 载入模板不再静默吞错**。`Strict=false` 的语义是"容忍一行坏数据"，不是"整表加载零条却不告诉任何人"；字段改名之类的系统性失败此前无日志无计数。现计 `dataengine.load.skipped.total{resource}` 并记 Warn。
- **`cache`/`entity`：L2 写不再无上界，发布分片锁不会被无响应的 Redis 钉住**。`ReadThroughStore` 的读路径本就有界（`loadOne` 整体跑在 `LoadTimeout` 下），但公开的 `Set`/`Delete` 把调用方 ctx 原样交给远端——而 `RemoteSnapshotCache.Publish` 正是**持发布分片锁**跨越这次 L2 写（单点发布是版本 CAS 成立的前提，持锁本身是对的）。于是一个"不拒绝、只是永不作答"的 Redis 会让该分片上的所有远端快照发布无限期阻塞，而这是跨服实体的写路径。新增 `ReadThroughOptions.RemoteTimeout`（零值回退 `LoadTimeout`），`Set`/`Delete` 的远端调用套上它并把远端失败计入 `remoteError`（此前这两条路径的远端错误连计数都没有）。`RemoteSnapshotCache` 显式传入；`IgnoreRemoteError: true` 已经让降级结果是想要的那个——跳过 L2、保留 L1。
- **`dataengine`：lease fence 的 schema 不再被两个包各自拼写**。新增 `LeaseFence.Predicate(now)` 与 `LeaseFenceField*`/`LeaseFenceStatusPending` 常量：fence 自己给出"必须仍然存在的那份文档"，投影方不再手拼 filter。原先字段名与 `"pending"` 在 kit 的 `dataengine` 与 `saga` 两侧各写一遍且无任何编译期耦合，而失效形态是最糟的一种——谓词不可满足与"租约确实过期了"从内部无法区分，两者都被当作 skipped no-op 正常提交，无错误、无指标、无失败测试。
- `featureflag`、`entity`：两个测试名承诺了属性却零断言（前者丢弃 `Enabled()`/`Version()` 两个返回值，后者吞掉 `Prepare` 的 error 且唯一失败形态是挂到 `go test` 超时），已重写为有界的显式断言。

### Added
- `dataengine.load.skipped.total{resource}`：非 strict 载入模板跳过的行数。
- `dataengine.fence.skipped.total{resource}`：fence 未命中计数。陈旧租约是这里的正常结果，但 schema 漂移长得一模一样——有了它，漂移表现为"所有被 fence 的事务同时开始跳过"，而不是一片安静。

### Fixed
- Entity 持久删除改为唯一的 durable admission 状态机：明确区分 immediate、随当前事务 deferred 和 indeterminate 结果；deferred 只在 WAL admission 后完成内存生命周期，rollback 保持实体存活，indeterminate 则停止继续服务可能已删除的状态。Remote transaction outcome 携带显式 delete intent，不再借用 `IsRemoved()` 猜测持久化意图。
- Native Saga step 新增 WAL v2 lease-fence 控制 receipt；reservation 的 owner/token/claim 位置进入业务 CommitRecord，使存储投影可在同一事务内拒绝过期 worker，而不把框架控制记录泄漏为业务 receipt。
- App 不再吞掉 `service.stopping` lifecycle hook 的错误；所有停止 hook 仍会执行，失败会与 Service/Mod shutdown 结果一起返回。Mongo 索引契约显式区分相对 TTL 与绝对日期到点过期，避免上层把 `expires_at` 再延长一轮 TTL。
- `worker` 为队列任务和 `Pool.Go/TryGo` 的每次执行建立并释放全新 `fctx.Context`，不再隐式继承父请求或跨任务泄漏；生命周期外的 `Pool.Go` 不再启动无人追踪的 goroutine。Nest 异步 Dispatch 只保留配置代际、Trace 与 Player/Msg/Seq 框架信封，KV/Base/SyncWait/Frame/事务不再跨异步边界传播，业务数据必须显式放入 Params。
- `cmd/glsvet` 将 Handler 并发约束升级为机器门禁：拒绝裸 `go`、同文件命名 wrapper、非 core worker 的 `.Go`、worker callback 外层捕获和裸忽略 Dispatch/Publish/Submit admission；保留框架既有 `AfterCommit` 合法语义。`app` 对显式 `--config` 以及默认配置的解析/权限错误 fail-closed，仅允许缺失的默认开发配置使用 defaults。
- App 对每个 Mod 分配 shutdown 子预算并隔离 Stop panic；普通错误或 panic 后继续反序停止其他模块，但任一 Mod 超时/取消后立即停止拆除其下游依赖，避免上层仍在运行时关闭底层连接。Provide 失败会回收失败 Mod 自身及此前已装配模块；Registry 批量注册先全量预检再一次提交，避免 capability 半注册。
- 轻量与 JetStream RPC 统一使用带版本、路由身份和业务成败字段的信封；服务端业务错误不再伪装成成功字节。轻量 RPC 默认仅发送一次，调用方取消会中断正在等待的 NATS 请求；RPC handler 的空值和重复注册在启动期显式报错。
- Bus 停止流程不再持有生命周期锁等待订阅/worker 退出，消除正在执行的 handler 回调注册路径与 Stop 互锁；实体引用计数增加溢出/下溢 fail-fast；DLQ 重放使用稳定消息 ID，并在后端支持时逐条删除，发布成功而删除失败后的重试不再制造新业务消息。
- 内存限流器增加默认 10 万 key 硬上限、空闲 TTL 与机会式回收；未知 key 在容量耗尽且无空闲项可回收时 fail-closed，并通过 `Stats` 暴露当前基数、容量拒绝和回收计数，阻断随机身份造成的无界内存增长。
- Nest ticker 测试清理全局 callback 注册，使 `go test -count=N` 可重复执行。

### Added — Data Engine
- `dataengine` 统一持久化契约：事务内 `PersistChange` 生成 Put/Patch/Delete，WAL record 原子携带 receipt/effect，并定义聚合 load、schema migration、tombstone 与 system transaction 接口；迁移和运维边界见 `docs/DATA_ENGINE_MIGRATION.md`。
- Native Saga step 可把 Entity mutation、Command receipt 与 completion effect 绑定到同一 CommitRecord；Remote Entity commit 也改为消费同一事务变更源，并保留 lease/fence/version 语义。

### Changed — Data Engine
- Data Engine tracker 只保留已接受持久化版本和 sync dirty；持久化 dirty 不再写回 DAO。`DurabilityPipelined` 在 WAL Enqueue 接受后、Entity 解锁前推进普通 DAO version。
- Legacy Checkpoint write runtime、dirty contract 与独立包已删除，运行时只保留 Data Engine 单一持久化路径，避免 WAL 与 snapshot 重复落地。
- 并发容器包由含义模糊的 `map` 改名为 `safemap`；类型名保持不变，业务可继续使用 `fmap` import alias。AI/Taskflow 的定义文件改按职责命名为 `strategy.go`/`runtime.go`，不再使用无业务指向的 `contracts.go`。

### Added
- `worker.Pool.TryGo`：返回异步任务 admission error；已接纳任务全部纳入 `StopWithContext`。
- `app.ModOptionalDependencyProvider`：声明“安装时需要排在当前 Mod 前、未安装时忽略”的可选依赖；与硬依赖共同拓扑排序并检测依赖环，消除可选集成对业务 Mod 书写顺序的隐式依赖。
- 新增三级文档中心：新手快速开始、熟练开发者完整说明、框架实现原理、生产部署手册和分级路线图。
- `syncstream.FileHistoryJournal` 新增幂等 `Close`，等待已接纳 group commit 后关闭常驻 WAL handle；关闭后持久操作 fail-closed。`History.Close` 将资源释放纳入运行时生命周期，修复重复启停的文件句柄泄漏。
- **`robot` 机器人框架**（从 cube 的 robot 服务拣入并重构，cube 仓库零改动）：模拟客户端逻辑 + 压测双用途。分层：`robot/transport`（统一包协议 `[4B body_len][4B msg_id][4B seq]` 小端、TCP/WebSocket 内置、`RegisterDialer` 扩展点——KCP/QUIC 客户端拨号在 cube-kit `robot` 包）；`robot/protocol`（编解码注册表，`Codec` 注入 + `EnsureEncoder`/`EnsureDecoder` 按方向幂等安装——请求响应共用 msgID 不冲突）；`robot/session`（seq 匹配请求响应、push 分发、幂等关闭，每次 Call 埋 `robot.session.call{msg,result}` 直方图）；`robot`（Context 黑板 + `TypedKey[T]` 类型化访问器、LIFO 关闭钩子、`EnsurePushCapture`、`Coalescer`（去重合帧确认）、`BoundedQueue`（drop-oldest））；`robot/action`（动作注册表 + 内置 connect/wait/wait_push；**`RegisterCall[Req,Resp]` 泛型一行注册调用动作**——请求字段按 json tag/snake_case 从参数与黑板自动填充、`GetCode()` 约定判错、业务只写 `OnResp` 闭包）；`robot/scenario`（行为树组合子 Sequence/Selector/Parallel/Retry/Timeout/加权 Random（按 Seed 确定性），可选 YAML spec 解释器，解析期全量校验）；`robot/runner`（k6 式三执行器 pool/looping（含 `Stages` 分段升降）/arrival-rate，账目不变量 `Started == Success+Failure+Canceled`，10k bot 基准 ~2s/60MB）；`robot/loadtest`（单活跃 run 状态机、`Threshold` SLO 裁决（error_rate/p50–p99，违约即 `StateFailed`+`StopReasonThreshold`）、环形历史、6 条 admin 命令、Markdown 报告；默认指标带 `run` label——同 profile 连跑分布互不污染）。端到端示例 `examples/robotdemo`。
- `metrics`：新增 **Histogram** 指标类型——17 个固定指数桶（1ms 起逐桶翻倍），`ObserveHistogram`/`HistogramQuantile`（桶内线性插值）/`HistogramBounds`；Prometheus 导出累积 `_bucket{le}` + `_sum_nanos` + `_count`，可直接喂 `histogram_quantile()`。
- `lockstep`：新增 **`FrameAssembler`**——客户端半场的帧装配器：冗余广播/追帧页去重、严格顺序释放、等待帧到达即时释放并排空连续段（追帧补洞永不被缓冲上限误拒）、缓冲越界返回"该追帧了"错误（`ErrHistoryUnknown` 链）。回归：3 客户端 × 600 帧 × 30% 丢包经冗余愈合零丢帧。

### Removed

- `entity.RemoteEntityRelease`：只被 nest 内部的死链引用，随该链删除；kit / cube 无使用。


## [1.8.0] - 2026-08

### Fixed（v1.7.1 发布后复审：lockstep 首审 + configdata 修复波自查，共 46 项全部实施，均带回归测试）
- `lockstep`（核心层）：`SubmitWindow` 硬上限 64 + 溢出配置注册期拒绝；`SequencerConfig.MaxInputBytes` 每局可收紧的输入上限（kit Room 据此做传输预算校验）；显式当前帧输入覆盖迟到折入的过期 payload（原先真实操作被静默丢弃）；帧号耗尽显式 panic（一局一 Sequencer）；`RedundantEncoder` 定长环 + `NormalizeRedundancyDepth`（按解码端上限收口）；解码补 `MaxFrameInputs` 与包内帧号严格递增（关闭 ~16× 内存放大）；`History.TrimBefore/FirstID`（长局内存收口）+ 单所有者/最坏内存文档；`DesyncDetector` quorum 改为"同意组大小"语义（少数抢先上报无法定罪）+ Trim 墓碑化（迟到补报不能重建报告集）。房间层修复见 cube-kit CHANGELOG。
- `configdata`（上一轮修复的自查，31 项）：`required`/`ref` 改逐指令解析（原先子串匹配会误拒 `index=required_level` 这类合法 tag、误报合法零值）；嵌入字段实现 encoding/json 的深度遮蔽规则（同名外层胜出、平局丢弃，被遮蔽字段带 cfg tag 报错——原先 key 可能静默取到恒零的内层字段；带 json 名的匿名字段不再被错误提升）；readJSON 包装文档改显式探测（对象目标的包装文档在宽松模式不再静默归零、strict/宽松语义一致；rows 为 null 拒绝；strict 模式补尾部垃圾检测）；listener 的 `Name()` 在 recover 保护内求值（typed-nil 监听器不再逃逸 panic）；版本号改单调分配永不回退（失败/回滚烧号，向监听者暴露过的号永不复用于不同内容）；Rollback 消费 previous 槽（连按两次不再回到坏配置）+ 事件带 `from_version`；全局槽位（defaultStore/fctx）改保存/恢复并加包级锁（跨 Store 并发不再互相抹除、首载失败不再清掉别人的运行时配置）；`ValidateReload` 在 Reload 与 DryRun 间保持单线程（`valMu`，只锁校验阶段——DryRun 的长 build 仍不阻塞紧急 Rollback）；panic 错误保留 `errors.Is` 链并附堆栈，`Must*` panic 改为包装 error；外部聚合指纹改顺序无关（按文件名排序折叠 per-file 摘要，并发读安全）+ 零读取报错（绕过注入 reader 即失败）+ 符号链接解析校验 + `WithExternalValidate` 选项；`finalize` 对全非导出字段的 custom/object 拒绝静默空对象哈希、未消费的指纹名报错、载荷流式写入；`SetFingerprint` 在快照定稿后封印；auto 表 ref 校验整体移入表级 `ValidateTable`（目标解析每 build 一次）+ `WithAutoValidateTable` 选项；回调契约文档化（不得 Goexit）。**行为说明：hash 值再次变化；重复注册与部分宽松解析路径现在报错。**

## [1.7.1] - 2026-08

### Fixed（configdata 三路对抗性复审，39 项发现全部实施，均带回归测试）
- **发布路径重构为单一提交点**（`configdata.Store.commit`）：current/version/defaultStore/fctx 四个状态在锁内一次性推进与回退（此前分五步推进，中途失败留下混合世代——Version 重号、Rollback 跳代、defaultStore 被劫持、typed-nil 进 fctx 槽）。所有 listener 回调、lifecycle emit、def 的 load/build/validate **全部 panic 容器化**（对齐全仓惯例；此前 listener panic 会让 Store 永久失去一致性）。
- **回滚配对语义修正**：`RollbackReload` 与 `BeforeApplyReload` 配对——只对 prepare 成功的监听者按逆序回调（此前 BeforeApply 失败不触发任何回滚、AfterApply 失败却回调从未执行过的监听者）；**`Old == nil`（首次加载失败）跳过全部回滚回调**——监听者永远不会把"没有上一代"误读成"回退到默认配置"。
- **Hash 修复三连**：`finalize` 的 `json.Marshal` 错误不再被吞（含循环引用等不可序列化值时 build 失败并提示 `SetFingerprint`，此前 hash 静默退化为常量）；新增 `Snapshot.SetFingerprint` 显式内容摘要；**`RegisterExternalTables` 对回调实际读到的字节做增量 sha256 作为该聚合的 hash 贡献**（Luban Tables 内部状态非导出，`json.Marshal` 恒为 `{}`，外部表内容漂移此前对 hash 完全不可见）。hash 算法同时改为长度前缀增量计算（消除大字符串物化与定界符歧义）。**升级后 Hash 值会变化。**
- **`RegisterExternalTables` 加固**：build 回调 panic 转为构建错误（Luban 生成的 loader 遇畸形数据习惯 panic，此前 bootstrap 期直接崩进程）；read 闭包在 build 返回后失效（惰性加载会绕过指纹与 fail-fast）；路径检查改用 `filepath.IsLocal`；目录在 build 开始时一次性捕获。
- **auto 表注册期校验补齐到文档承诺**：嵌入（非指针）字段按 encoding/json 语义提升（此前 base 里的 ref/index tag 被静默忽略）；ref/key 字段类型白名单（整数/字符串，bool/float/切片/指针注册期拒绝）；ref 类型兼容性收紧为同 Kind（int64→int32 截断、int→string rune 转换此前会放过悬空引用）；**ref 目标表存在性与类型兼容改为表级前置校验**（`TableDef.ValidateTable` 新 API，空表/全零列不再隐藏拼写错误）；index 重名注册期拒绝（此前两列静默并入同一索引）；新增 `required`（零值即错，抓字段改名导致的整列归零）与 `skipempty`（零值不进索引）指令；错误信息用完整类型名。
- **`Registry` 同名同 kind 注册从静默幂等改为报错**——此前第二个同名定义被静默丢弃、整张表不加载（自动名字推导下极易撞名）。**行为变更：依赖重复注册幂等的装配代码需要清理。**
- **`readJSON` 收紧**：`null`/空文档拒绝（此前静默变成空表）；`rows`/`records`/`data` 多包装键并存拒绝（此前静默取优先者）；新增 `Store.SetStrictJSON`（opt-in 拒绝未知字段——字段改名此前静默整列归零，且被 ref 零值跳过掩护）。
- 其它：`SetDir` 加锁（与 build 的数据竞争）、build 目录单次捕获（三次读取不一致绕过目录校验）、custom 构建移到表/对象校验**之后**（悬空引用报精确校验错误而非 builder 崩溃）、validate 循环响应 ctx 取消、`DryRun` 不再持store 锁（大配置 dry-run 不阻塞紧急 Rollback）、`MustGet` nil 接收者不再裸解引用、`Rollback` 的 lifecycle 事件带 `from_version`。
- kit `configdata` Mod：Rollback 回调把 `configdata.version` gauge 复位到 `Old.Version`（此前失败的 reload 让 gauge 永久停在已回滚的版本上，监控误报成功）。

### Fixed（全量能力审计发现，均带回归测试）
- `hotcode`：`RegisterAdminCommands` 改为注册到调用方传入的 admin **实例**注册表（对齐 bus 的模式，返回聚合错误）——原先注册到包级 default，而装配路径（kit ops HTTP）只读实例注册表，`hotcode.list/revert/load_plugin` 三条命令在生产运维端点不可达。
- `configdata`：`Table` 增加 `MarshalJSON`（表名 + 行数据，文件序）——原先 Table 字段全不可导出，`json.Marshal` 恒为 `{}`，快照 `Hash` 只反映表名集合、改任意行数据 hash 不变，无法用于配置一致性校验。**注意：升级后同一份配置的 Hash 值会变化**（首次真实覆盖内容）。
- `index`：`OrderedIndex` 默认比较器改为类型感知（整数/无符号/浮点/字符串按自然序，其余回退格式化串）——原先统一 `fmt.Sprint` 字典序，整数 key `[9,10]` 排成 `[10,9]`。显式传入 `less` 的行为不变。
- `metrics`：指标基数打满不再全静默——首次打满每 metric 记一条 Warn，丢弃数以 `metrics.series.dropped{metric}` counter 随 Snapshot/Prometheus 导出（序列数以打满的 metric 名数为界）；`Reset` 同步清零。
- `failurelog`：① 原子 Lua 失败降级为非原子回退时记 `failurelog_degraded_total{namespace,op}` + Warn（对齐 `cache.refhmap` 的"降级必须可见"规范）；② trim/delete 回退优先走新的 `fredis.ListTrimmer`/`ListRemover`（LTRIM/LREM 就地操作）——原先 DEL+RPUSH 两步间崩溃会丢整个列表，无该能力的客户端保留旧回退；③ `failurelog_trim_total` 补上 namespace label。
- `timer`：`Scheduler` 新增 `SetClock` 注入时间源（默认 `time.Now` 行为不变）——原先 `NewTimer` 的 End 用裸墙钟而 Tick 用调用方时钟，`time.logic_offset` 非 0 时所有新建 timer 整体偏移一个 offset；宿主用偏移时钟驱动 Tick 时必须同源注入。
- `misc`：`SafeFuncWithTryCount` 补上与同族 `Safe*` 一致的 panic 恢复、返回包装后的真实末次错误（原先吞掉所有 error 只返回 "try count exceeded"）、`tryCount<=0` 至少执行一次（原先一次都不调用就报错）；`TaskPool` 对非 nil 配置做归一化（`WorkerCount==0` 原先构造空 worker 切片致 `Submit` 除零 panic），哈希取模改在 uint32 空间（32 位平台负索引 panic）。
- `featureflag`：`Replace` 的版本号递增移入临界区（原先先换 map 再递增，读者可能观察到"新数据 + 旧版本号"，基于版本的缓存失效判定会漏）；`Snapshot` 按 Name 排序输出（对齐全仓 List 类方法）。

### Changed
- README 按全量能力审计扩充：能力总览修正分类（taskflow/ai 独立成"实体行为契约"行并指明 runner 在 kit、ownerroute 的"路由 epoch"归位到 entity、`cache`/`httpclient`/`httpserver` 从"接口抽象"改为完整实现三档分类）；实现细节新增第 12–16 条（平台注册表实例优先、缓存分层选型、bus 四条易踩契约、`CaptureSnapshot` 跨 goroutine 正向出路、单所有者组件清单）；"补充契约"新增分布式 fence 三化身、configdata 热更一致性、`errcode.ClientError` 信息隐藏边界；修正第 7 条"时长分布"措辞（obs timer 无分位数）；学习路径补第 11–12 条测试即规格清单。

### Added（v1.7.1）
- `configdata` 配置管线打磨（映射零手写）：① `RegisterAutoTable`——`cfg` struct tag（`key`/`index[=名]`/`ref=表名`）推导全部映射，`ref` 为 Luban 式悬空引用校验（每次 load/reload 进程内执行，零值=无引用），tag 错误一律注册期 fail-fast，读取路径零反射；② `RegisterExternalTables`/`ExternalTablesFrom`——外部生成的表聚合（如 Luban code_go_json 的 Tables）作为快照成员接入，原子热更/回滚/hash/请求一致性全部继承，文件读取限制在数据目录内（拒绝路径逃逸）；③ 新增 `examples/` 模块：`configgen`（cfggen meta→生成→热更/ref 拦截端到端）与 `lubanreal`（**真实 Luban 接入**：gen/ 与导出数据由官方 luban CLI v4.11.0 生成，XML schema + JSON 数据源，`RegisterExternalTables` 接线后热更/回滚全继承，重生成脚本 gen.sh）。配套的 schema 生成器 `cfggen` 在 roost-codegen。

## [1.7.0] - 2026-08

### Added
- `lockstep` 包：帧同步（输入帧）核心，与状态同步（entitysync / kit sync）并列的第三条同步通道。`Sequencer` 乐观帧锁定（到点切帧永不等待、缺席即空输入、迟到折入下一未切帧、提交窗口防未来帧滥用、重复提交幂等）；`RedundantEncoder`/`EncodeBroadcast`/`DecodeBroadcast` 帧冗余广播编码（每报文携带最近 N 帧，丢包靠冗余修复而非重传；解码严格 fail-fast，坏包永不变成静默错帧）；`History` 全量帧历史（追帧分页 + 回放产物）；`DesyncDetector` 关键帧哈希多数派裁决（首报不可改口、法定人数后出裁决）。单帧封包基准 ~0.84µs（验收线 50µs）；30% 丢包仿真零帧缺失。房间与传输接线在 cube-kit 的 `lockstep` 包。
- 可观测性统一：`OBSERVABILITY.md`（命名规范、全仓指标清单、告警基线、Prometheus 导出接线）与 `observability/grafana-roost-overview.json` 总览面板（调度/durability/缓存总线/跨服实体四组）。

## [1.6.3 – 1.6.5] - 2026-08

### Fixed
- `cache/ref_hmap`：Redis Lua 写失败降级为非原子回退时不再静默——记录 `slog.Warn` 并递增 `cache.refhmap.write_degraded_total` 指标（降级行为本身保留，可用性优先）。
- `entity/ManagerAccess`：冷缓存加载增加 single-flight 合并——并发请求同一实体只发出一次 `LoadEntity`，消除热实体冷启动对数据库的惊群；失败航班共享错误且立即移除（重试触发全新加载），等待者可被自身 context 取消。

### Added
- CI 增加 `release-hygiene` 门禁：module 路径必须能被 `git ls-remote` 解析、版本 tag 必须与 module major 后缀匹配。
- `nest`：每 handler 锁内耗时指标 `nest.handler.lock_hold`（pipelined 提前放锁按提前点计），超阈值（`NestOptionWithSlowLockThreshold`，默认 100ms）计 `nest.handler.lock_hold.slow.total` 并告警——`DurabilityPipelined` 灰度对象的选择依据。
- `cmd/glsvet`：静态检查器，扫描 `go` 语句内对 goroutine 绑定 API（RecordUndo/CurrentRollbackTx/fctx.CurrentContext 等）的调用；已接入本仓库 CI。
- `NEST_PIPELINED_COMMIT.md` §12：pipelined 灰度扩大到默认提交档的四步路线（含量化门槛与回退开关）。
- `nest`：实例作用域 handler 注册 `(*NestMgr).RegisterHandlerWithMeta`/`MustRegisterHandlerWithMeta`（Start 前有效；实例优先、全局注册表兜底）——测试与多引擎进程不再共享包级 handler 表。
- `saga`：`NewEngine` 的配置拒绝逐条给出具体字段、实际值与被违反的预算计算式（原先 8 类错误共用一句 "unsafe engine limits"）。

### Fixed
- `nest/ticker`：tick 回调改为每 tick 读取实时注册表（按注册顺序执行）——原先 `NewTicker` 做构造期快照，引擎启动后注册的回调**静默不执行**，且执行顺序来自 map 迭代（跨进程不定）。
- `syncstream/file_journal`：`Record` 从"每条 open+fsync+close"改为常驻句柄 + leader 合批组提交——"返回即持久"语义不变，fsync 次数从每条降为每批；generation 轮转时释放句柄。

## [1.6.2] - 2026-08

- 修复 pipelined 完成泵：降级路径改为按实体完成链（链序 = LSN 序），三条路径统一等待前驱——降级只牺牲延迟不牺牲同实体完成顺序；submit/stop TOCTOU 加固；stop 超时不再泄漏池。
- v1.5.0–v1.6.x 主线：实体锁由自旋改停车信号量、`DurabilityPipelined` 两阶段提交（锁内仅 append、fsync 锁外、外化闸门）、checkpoint FlushAll 唤醒修复、saga Incarnation 折入 CommandID、bus/worker/sync 竞态修复等，详见 `NEST_PIPELINED_COMMIT.md` 与 git 历史。
