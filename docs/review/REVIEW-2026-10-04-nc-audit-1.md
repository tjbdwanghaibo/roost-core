# B 线非核心修复复审（第一组）：RR-20261003-NC-01～04、RR-20261004-NC-01～07

2026-10-04，独立审计员，只读。基线 tag `v1.19.0` → `74e1ba39`（tag 对象 `d48206b4`）。修复提交 `3529a569`（10-03 NC-01～04）、`483350ca`（10-04 NC-01～04）、`49796514`（10-04 NC-05～07）、`74e1ba39`（NC-07 补修，A 线）。NC-08～12 和 NC-13～29 不在本组。

**结论：11 条全部定根因、修法与记录一致，回归在包里，`v1.19.0` 上 race 通过；抽查的 11 条修复全部临时回退，回归都变红。没有找到确定缺陷。** 有 1 条疑点（NC-07：面向用户的文档在补修后已经过时）。另有 4 处记录 / 流程问题，见 §4，其中最重要的是 B 线的验证范围没有包含根包，所以 NC-07 越界没被本地验证发现；CI 自 10-01 起已经是红的，新的红被旧的红挡住了。

## §0 范围与方法

- 规则：`AGENTS.md`、`docs/agent-skills/roost-coding/SKILL.md`、`~/.claude/skills/roost-review/SKILL.md`。
- 记录：`docs/bug/README.md` 头部和 NC-01～07 各行，`docs/bug/REVIEW-2026-10-03-noncore-01.md`、`docs/bug/REVIEW-2026-10-04-noncore-0{2,4}.md`（只看标题与修复接续段），11 份 `docs/bugfix/RR-*-NC-0*.md`，`docs/bugfix/evidence/noncore-bugfix-20261004-0{1,2,3}/`（README、Run-Verify.ps1），`CHANGELOG.md` `## [v1.19.0]`，`docs/USER_GUIDE.md`、`docs/TROUBLESHOOTING.md`（T-183～193）。
- 被审记录：[10-03 NC-01](../bugfix/RR-20261003-NC-01.md) · [02](../bugfix/RR-20261003-NC-02.md) · [03](../bugfix/RR-20261003-NC-03.md) · [04](../bugfix/RR-20261003-NC-04.md)；[10-04 NC-01](../bugfix/RR-20261004-NC-01.md) · [02](../bugfix/RR-20261004-NC-02.md) · [03](../bugfix/RR-20261004-NC-03.md) · [04](../bugfix/RR-20261004-NC-04.md) · [05](../bugfix/RR-20261004-NC-05.md) · [06](../bugfix/RR-20261004-NC-06.md) · [07](../bugfix/RR-20261004-NC-07.md)；原问题 [10-03 第一轮](../bug/REVIEW-2026-10-03-noncore-01.md) · [10-04 第二批](../bug/REVIEW-2026-10-04-noncore-02.md) · [N02](../bug/REVIEW-2026-10-04-noncore-04.md)。
- 代码：4 个提交逐个 `git show`；当前实现读 `v1.19.0` worktree 里的源码。根因行号在修前基线 `7e0d6ee2` / `183b0bdd` / `c4aa1e7d` 上用 `git show <base>:<path> | sed -n` 核对。
- 图谱：项目 `Users-whb-roost-roost-core` 状态 ready，generation `2026-09-30T11:28:30Z`，比本组修复早。对引用的 12 个文件跑了 `check_index_coverage`：生产文件 11 个全部 `metadata_changed`（`read_source_and_reindex`），`dependency_boundary_test.go` 为 `metadata_match`。**所以结论全部以 `v1.19.0` 的源码和 grep 为准，没有依赖图谱的边**；索引没有刷新。
- 工作区：`git worktree add --detach …/scratchpad/audit-nc1 v1.19.0`；回退实验只改实现文件，每次做完 `git checkout --` 复原，最后 `git status --short` 为空。

已执行的命令（都加 `GOWORK=off`，在 `v1.19.0` worktree 里）：

| 命令 | 结果 |
| --- | --- |
| `go test -count=1 -race ./app/ ./httpserver/ ./httpclient/ ./manager/ ./kit/manager/ ./kit/ops/ ./admin/ ./security/ ./gateway/ ./webroute/ ./codegen/internal/webroute/ ./codegen/cmd/webroute/` | 11 包 ok，exit=0 |
| `go test -count=1 -race ./bus/ ./hotcode/ ./kit/nats/ ./kit/service/account/ ./kit/service/platform/ ./robot/loadtest/ ./codegen/internal/roost/`（import 了被改包的消费者，用 `go list` 求出） | 7 包 ok，exit=0 |
| 上述回归用例 `-count=5 -race`（9 包） | 全 ok |
| `go test -count=1 -run TestCoreDependencyBoundary .`，以及整个根包 `go test -count=1 .` | ok |
| `go vet` 11 包；`go run ./cmd/glsvet` 11 包；`gofmt -l` | exit 0，无输出 |
| `go mod tidy` 后 `git diff -- go.mod go.sum` | 无变化 |
| 构建 `codegen/cmd/webroute`，在临时 module 上跑 `/broken/{id` 和 `/items/{id:[0-9]+}` | 前者 exit=1、`handleRaw: invalid path "/broken/{id": chi: route param closing delimiter '}' is missing`、没有生成物；后者 exit=0、生成 `webroute_gen.go` |
| `gh run list --commit 74e1ba39` | ci（main 与 tag）、framework-compat、upgrade-compat、security、demo-publish 均 success |

## §1 逐条表

| 编号 | 模块 | 记录声称 | 代码核对（v1.19.0） | 回归（文件·函数，结果） | 结论 |
| --- | --- | --- | --- | --- | --- |
| RR-20261003-NC-01 | app | sortMods 把 0 和 1 一起快速返回；改为只有空集合快速返回 | `app/app.go:621-625` 只有 `len(mods)==0` 快速返回；单元素走 `:627-` 的 nil、空名、重名、external、环检测。调用方只有 `:174`（shared）和 `:216`（service，带 external）两处，已 grep | `app/mod_validation_promises_test.go` `TestSingleModValidation`、`TestSingleModExecuteFailsBeforeInit`：绿；回退后 7 红（§2） | 通过 |
| RR-20261003-NC-02 | httpclient | Clone 只改了包装层的 timeout；库拥有的 client 复制一份副本，外部注入的 client 不动 | `httpclient/client.go:86-91`：`ownsHTTPClient && next.timeout != c.timeout` 时复制 `http.Client` 值再改 Timeout，Transport 共用；`New` 在 `:59-60` 置拥有标记，`WithHTTPClient` 清掉标记。库拥有的 client 上 `client.Timeout == c.timeout` 恒成立，所以比较父 timeout 等价 | `client_boundaries_promises_test.go` `TestClientDeadlines`、`TestCloneTimeoutPreservesParentAndCustomClient`、`TestCloneTimeoutConcurrentRequestsKeepParentDeadline`：绿；回退后红 | 通过 |
| RR-20261003-NC-03 | httpclient | 先建 StatusError，读取 / 解码失败时用 `errors.Join` | `client.go:190-207`：先分类非 2xx；readErr 和解码错误都和 StatusError 一起 Join；2xx 的坏 JSON 仍返回原错误。仓内 `httpclient/` 以外没有 `StatusError` 的使用者（grep），不存在直接类型断言 | `TestStatusErrorClassification`、`TestStatusErrorPreservesReadAndDecodeFailures`：绿；回退后红 | 通过 |
| RR-20261003-NC-04 | httpserver | Decoder.Decode 只读首个值，改为 `json.Unmarshal` | `httpserver/server.go:239`。同类入口 `webroute.DecodeJSON`（`webroute/route.go:154-163`）原本就做了第二次 Decode 检查 EOF，不需要改；仓内其他 `json.NewDecoder` 都不是 HTTP 请求入口（grep） | `json_boundary_promises_test.go` `TestJSONRequestBoundary`：绿；回退后 2 红 | 通过 |
| RR-20261004-NC-01 | manager | stopOne 逐对象 recover，error 类型的 panic 保留 `%w` | `manager/engine.go:233-255`：名字在 recover 保护内取；三条路径 stopStarted（`:212-227`）、Start 失败 rollback、最后一次 handover（`:186-192`）都调用 stopOne。App 层 `stopModSafely`（`app/app.go:583-605`）本来就隔离 panic；`app.IManager` 的生命周期只有这一个 Engine（grep） | `engine_lifecycle_promises_test.go` `TestStopPanic*`、`TestRollbackPanicKeepsStartFailureAndCleanup`、`TestLastStartupHandoverAlsoContainsStopPanic`：绿；回退后红，其中一个用例直接 panic 打穿测试进程 | 通过（观察：没有记录 panic 栈，见 §4） |
| RR-20261004-NC-02 | manager / kit/manager | 在同一把锁下取得唯一启动权；没有 Provide 时拒绝但不消耗启动权；新增 `ErrStartState` 和 Kit 别名 | `engine.go:124-141`：先查 `starting||stopping`，再查 registry（nil 时解锁返回，不置 starting），最后置 starting。`kit/manager/manager_mod.go:33-34` 是同一个值的别名。App 每次 Execute 只跑一次生命周期（`app/app.go:98-` 没有重入路径） | `TestConcurrentStartHasOneOwner`、`TestSingleAttemptAfterFailureOrStop`、`TestProvidePreconditionDoesNotConsumeStartup`、`TestRepeatedStartDoesNotDuplicateManagers`：绿；回退后红 | 通过 |
| RR-20261004-NC-03 | admin | 非 nil 的空 map 也复制，数组里的容器递归复制 | `admin/admin.go:258-294`：`cloneMap` 保留 nil；`cloneSchemaValue` 递归复制 `[]any`、`map[string]any`、`[]string`；Register、Get、List（`:214`、`:228`、`:238`）都经过 cloneMeta。`PayloadSchema` 在 admin 包外没有读写（grep） | `schema_ownership_promises_test.go` 4 个用例：绿；回退后 7 红 | 通过 |
| RR-20261004-NC-04 | kit/ops | Shutdown 出错时保留 server；锁外 Shutdown；Start 拒绝还没关掉的实例；goroutine 捕获局部变量 | `kit/ops/ops_mod.go:118-145`（Start 持 serverMu，`:125` 拒绝）、`:153-177`（锁外 Shutdown，成功后只清同一个实例）。仓内 `ListenAndServe` 只有这一处（grep），没有别的 HTTP server 需要同步修 | `shutdown_ownership_promises_test.go` 5 个用例：绿；回退后 2 红 | 通过 |
| RR-20261004-NC-05 | security | 判断 `n>burst` 移到取时钟、加锁和改 key 表之前 | `security/ratelimit.go:114-116`；`burst` 只在构造时赋值，锁外读没有竞争。`AllowN` 在仓内只有这一个实现 | `admission_promises_test.go` `TestOversizedDemandHasNoKeySideEffects`：绿；回退后 4 红 | 通过 |
| RR-20261004-NC-06 | gateway | 先赋 `ret`/`err`，再调用受保护的 report；日志 handler 自己的 panic 也隔离 | `gateway/middleware.go:69-96`。仓内其他 recover 中间件（`httpserver.recoverMiddleware`、生成的 `RecoverMiddleware`）都没有用户回调，不属于同类 | `recover_promises_test.go` 3 个用例：绿；回退后红，诊断 sink 用例直接 panic 打穿测试进程 | 通过 |
| RR-20261004-NC-07 | webroute / codegen | 新增 `webroute.ValidatePath`；Register 先校验、installer 的 panic 转成 error、安装成功后才写 seen；生成器扫描阶段就拒绝坏模式；补修后生成器直接用 chi | `webroute/route.go:56-111`；`codegen/internal/webroute/parse.go:115`、`:152-160` 的 `validateChiPath` 只 import chi。两边都是 `chi.NewRouter().Get(path, …)`，语法相同。chi v5.3.0 里会 panic 的路径都和 router 状态无关（`tree.go`/`mux.go` grep），生成器也不拼前缀（`gen.go:142` 按原 path 生成） | `webroute/pattern_promises_test.go`、`codegen/internal/webroute/pattern_promises_test.go`：绿；回退后两边都红；`TestCoreDependencyBoundary` 在补修前红、`v1.19.0` 上绿 | **疑点**（文档在补修后过时，见 §4.1） |

统计：11 条，通过 10，疑点 1，缺陷 0。

## §2 红是否真红

做法：在 `v1.19.0` worktree 里只改实现文件，把修复关掉，跑对应回归，然后 `git checkout --` 复原。新测试文件不动。

| 回退了什么 | 命令 | 红文本（节选） |
| --- | --- | --- |
| 10-03 NC-01：`app/app.go:623` 改回 `len(mods) <= 1` | `go test -run TestSingleMod ./app/` | `mod_validation_promises_test.go:35: error=<nil>, wantError=true`（single_nil、single_unnamed、single_missing_hard、两种自循环）；`:64: invalid dependency graph reached Init: initialized=true err=serve failed`（shared、service） |
| 10-03 NC-02/03：关掉 Clone 复制分支；解码失败时直接 `return err` | `go test -run 'TestClientDeadlines\|TestStatusErrorClassification\|TestCloneTimeout\|TestStatusErrorPreserves' ./httpclient/` | `:66: transport deadline=299.996667ms, want approximately 20ms`；`:126: error=invalid character 'u' … (*json.SyntaxError), StatusError=false, want true`；`(*json.UnmarshalTypeError), StatusError=false, want true`；`:274: lost status or decoder cause` |
| 10-03 NC-04：BindJSON 改回单次 `Decoder.Decode` | `go test -run TestJSONRequestBoundary ./httpserver/` | `json_boundary_promises_test.go:42: status=200 calls=1, want status=400 calls=0`（trailing_garbage、second_json_value） |
| 10-04 NC-01：stopOne 里的 `recover()` 换成 `any(nil)` | `go test -run 'Panic' ./manager/`，以及逐个单跑 | `:146: handover error=<nil> panic=stop boom stops=1`；`panic: stop panic cause [recovered, repanicked]`（打穿进程）；`:218: stop failure skipped cleanup: panic=stop boom error=<nil> older_stops=0`；`:236: rollback lost cause/cleanup: … older_stops=0 failed_stops=0` |
| 10-04 NC-02：删掉 `starting\|\|stopping` 闸门 | `go test -run 'TestConcurrentStart\|TestSingleAttempt\|TestProvidePrecondition\|TestRepeatedStart' ./manager/` | `:58: duplicate Start waited inside user callback`；`:100: second Start=manager: start aborted by shutdown`；`:269: one lifecycle executed twice: starts=2 stops=2` |
| 10-04 NC-03：空 map 走旧的短路、`[]any` 改回浅复制 | `go test -run 'TestMetadataOwnership\|TestSchemaNestedArrays' ./admin/` | `:47: external mutation changed empty stored schema: map[type:injected]`（empty_input/get/list）；`:52: external mutation changed schema inside array: injected`（array_*）；`:123: map[… nested:[[map[type:input changed]]]]` |
| 10-04 NC-04：Shutdown 出错也清掉 server（旧语义） | `go test -run 'TestInterruptedShutdown\|TestShutdownDeadline\|TestConcurrentShutdown' ./kit/ops/` | `:80: first=context canceled server_retained=false handler_active=true`；`:83: lost in-flight server: retry=<nil> handler_active=true`；`:190: deadline lost in-flight server` |
| 10-04 NC-05：把 burst 判断放回 bucket 创建之后 | `go test -run TestOversizedDemand ./security/` | `:17: rejected demand changed key stats: {Keys:1 …}`；`:28: oversized demand counted as key rejection: {… CapacityRejected:1 …}`；`:44: … removed=0`；`:50: clamped burst demand occupied key` |
| 10-04 NC-06：恢复成先 report、后赋值 | `go test -run TestRecover ./gateway/` | `:27: ret=<nil> err=<nil> escaped=reporter failed reports=1`（string/error）；`escaped=runtime error: panic called with nil argument`；诊断用例 `panic: endpoint [recovered]` 打穿进程 |
| 10-04 NC-07 运行期：ValidatePath 不调用 chi；Register 的 recover 换成 `any(nil)` | 逐个单跑 `./webroute/` 的 3 个用例 | `:26: error=<nil> panic=chi: route param closing delimiter '}' is missing`、`… invalid regexp pattern '^[$'`、`… wildcard '*' must be the last value`；`:67: error=<nil> panic=installer failed`；`TestRegisterAcceptsChiPatterns` 仍绿（这是合法模式的控制组） |
| 10-04 NC-07 生成器：`validateChiPath` 不调用 chi | `go test -run 'TestGenerateRejectsMalformed\|TestParseAcceptsChiPatterns' ./codegen/internal/webroute/` | `pattern_promises_test.go:27: generation should reject handler/path: <nil>`（3 个坏模式） |
| NC-07 越界：parse.go 换回 `74e1ba39^` 版本 | `go test -run TestCoreDependencyBoundary .` | `dependency_boundary_test.go:60: codegen/internal/webroute/parse.go (codegen layer): 生成器保持独立于它生成的运行时，不得 import core 层: github.com/tjbdwanghaibo/roost-core/webroute` |

全部按预期变红，红文本和各 bugfix 记录里抄的修前失败对得上。每条都有控制组保持绿。

## §3 确定缺陷

**没有。** 已经检查、排除的方向：

- 错误语义：10-04 NC-01 和 NC-07 对 error 类型的 panic 用 `%w`；10-03 NC-03 的 Join 对 `errors.As(*StatusError)`、`errors.Is(readErr)` 都可用。仓内没有 StatusError 的直接类型断言。
- 资源所有权：Ops server 只有 Shutdown 成功后才放手，Start 拒绝替换。Clone 的副本共用 Transport，没有改父 client。admin 的 Register、Get、List 各自返回副本。
- panic 隔离是否吞错：stopOne 把 panic 转成返回 error，Kit `ManagerMod.Stop` 会打日志。gateway 的 reporter panic 记 `slog.Error`；只有日志 handler 自己的 panic 被静默丢弃，记录里写明了这一点。
- 同类入口：BindJSON 与 `webroute.DecodeJSON`；OpsMod 是仓内唯一的 `ListenAndServe`；`AllowN` 只有一个实现；带用户回调的 recover 中间件只有 gateway；`app.IManager` 的生命周期只有 manager.Engine。都已 grep。
- 层次边界：`TestCoreDependencyBoundary` 和整个根包在 `v1.19.0` 上绿。本批其他改动只新增了 kit→core 的别名、gateway→`log/slog`、webroute→chi 三种 import；仓内 import chi 的只有 httpserver、webroute 和生成器这三处。`go mod tidy` 无变化。
- CI：`74e1ba39` 的 ci（main 和 tag）success；本地 vet、glsvet、gofmt 都干净。

## §4 疑点与记录不一致

1. **NC-07 补修后，面向用户的文档没有跟着改**（疑点，建议按 P3 文档问题处理）。`docs/USER_GUIDE.md:45` 写“Codegen 和运行期 Registrar 共用 webroute.ValidatePath 的 chi 模式规则”，`docs/review/IMPLEMENTATION-REQUEST-ADMISSION-AND-GENERATED-WEBROUTES.md:35` 写“生成扫描与运行期同一规则”。`74e1ba39` 之后生成器用的是自己的 `validateChiPath`（`parse.go:152`），不再调用 `webroute.ValidatePath`。规则（chi 语法）确实一样，但“共用 ValidatePath”这个说法已经不对了，会让下一位改 ValidatePath 的人以为生成器会自动跟着变。两边现在是两份实现，也没有测试保证它们一致。main（`2f111e9b`）上这两处也没改。`docs/bugfix/RR-20261004-NC-07.md` 正文第 5 行同样写着“Codegen parseRoute … 调用同一入口”，但它末尾有“复核后的补修”一节说明了变化，可以接受。`docs/review/REVIEW-2026-10-04-noncore-05.md:9` 是历史运行记录，不要求改。
2. **B 线的验证范围没有包含根包，CI 又早已是红的，NC-07 越界因此没被发现**（流程问题，不是代码缺陷）。三批 `Run-Verify.ps1` 的 race 范围分别是：`./app/... ./httpclient ./httpserver ./lifecycle ./manager ./health ./security ./admin ./kit/manager ./kit/ops`；`./manager … ./security`；`./security ./gateway ./webroute ./httpserver ./httpclient ./codegen/...`。都没有 `.`，而 `TestCoreDependencyBoundary` 就在根包里。第三批 README 写“20 个有测试的包、754 pass、0 fail”，对它自己的范围是真的，但会被读成“相关检查全绿”。另外 main 的 ci 从 10-01 起一直是 failure（RR-20261001-01），`3560a19b` 那次 run（`37170617871`）的 linux-test (1) 和 windows-compatibility 里已经同时出现了 `TestCIWorkflowRunsTheRedisSuitesAndRefusesTheirSkip` 和 `TestCoreDependencyBoundary … webroute` 两个失败。B 线是在红的 CI 上推送的，记录里没提 CI 状态。建议：以后修复批次的验证固定加上 `go test .`，或者至少加上 `-run TestCoreDependencyBoundary .`，并在记录里写明推送时 CI 的状态。
3. **CHANGELOG `## [v1.19.0]` 的归类**（记录质量）。B 线这几条都放在小节标题之前的无标题列表里（`CHANGELOG.md:16-24`），不在 `### Changed（行为收紧 / API 变化）` 或 `### Added` 下面。可其中有几项是升级者需要知道的行为收紧或新 API：同一个 Engine 第二次 Start 返回 `ErrStartState`、运行中的 OpsMod 再次 Start 返回 error、BindJSON 拒绝尾随内容、单个无效 Mod 现在启动失败、`httpclient` 部分错误的最外层类型变成 Join error，以及新增的 `webroute.ValidatePath`、`manager.ErrStartState`、`kit/manager.ErrManagerStartState`。第 9 行的版本说明“次版本而非补丁”列出的 API 变化里也没有这些。条目正文都写了兼容性，信息没有缺，只是升级时容易看漏。
4. **索引状态列**。`docs/bug/README.md` 中 NC-01～07 各行仍然写“未发版”。main 上 `2f111e9b` 在头部加了一行“v1.19.0 已发布（tag → 74e1ba39）… B 线 … NC-01～29 随本版”，按“头部覆盖下方”的惯例可以接受，但各行本身没有改。
5. **观察，不登记**：
   - stopOne 和 Register 的 recover 都没有记录 panic 栈（`httpserver.recoverMiddleware` 会记 `debug.Stack()`），非 error 类型的 panic 只留下 `%v` 文本，排障信息偏少。
   - `Engine.Start` 里 `manager.Start` 自己 panic 时仍不会回滚已启动的 manager。NC-02 记录把它列为“Start panic 未验证”，范围说得清楚。
   - `TestClientDeadlines` 允许的偏差是 `[want-15ms, want+5ms]`（`client_boundaries_promises_test.go:65`），在负载很高的 race 或 Windows 机器上有理论上的偶发失败可能。本地 `-count=5 -race` 全绿，`74e1ba39` 的 CI 也绿。

## §5 没读完 / 没跑的

- 没有逐字读完原审查主记录的正文（`docs/bug/REVIEW-2026-10-03-noncore-01.md`、`REVIEW-2026-10-04-noncore-0{2,4}.md`），只核对了标题、锚点和修复接续段；也没有逐行打开 evidence 的 jsonl 日志，只看了 README 和 Run-Verify.ps1。
- 没有复跑 B 线的“原审查 overlay”测试（`docs/review/evidence/noncore-review-2026100{3,4}-*` 的 `*_review_test.go.txt`），也没有复跑 Run-Consumer.ps1 的四个业务 module 消费者。生成器只用 `v1.19.0` 构建的 CLI 跑了一个坏模式和一个好模式。
- 没有跑 `go vet -tags integration ./...` 全仓、`go test ./...` 全仓或 govulncheck，这些以 `74e1ba39` 的 CI success 为准。
- run `37170617871` 的 windows-compatibility 里还有 `sync/entitysync` `TestSnapshotIndexAndStatsFollowLifecycle/on_change` 失败，不属于本组，没有调查。`74e1ba39` 的 CI 是绿的，可能是偶发，请负责 Sync 的人留意。
- 图谱停在 09-30 这一代，没有刷新；本报告不依赖图谱的边。
