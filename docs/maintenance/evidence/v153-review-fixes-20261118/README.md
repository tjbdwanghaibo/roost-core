# v1.53.0 审查三项修复（2026-11-18）

来源：审查分支 `origin/fix/core-game-server-review` 的 `docs/maintenance/V153-REVIEW-20261117.md` 与 `docs/maintenance/evidence/v153-review-20261117/`（只读取，未合并、未 cherry-pick）。基线 main `7c9cf93a`；三项涉及的文件（`modules/mailbox/with/rehome/`、`service/mail/`、`codegen/internal/errcode/csharp.go`、room 断线重试测试模板）在基线上与审查检出相同，没有被别的提交顺带修掉，下面的“修前”都是在基线上复现的。

环境：macOS ARM64、Go（仓库 go.mod 版本）、.NET SDK 10.0.302。代码图谱工具当天不可用，按源码与 grep 定位。生成工程：`scratchpad/rvfix/gen.sh`（`roost project new -template game` + `go mod edit -replace` 指向本检出 + `roost module add mailbox rehome room enter`），存储是内存替身，没有用真实 Mongo / Redis / NATS。

## RV-20261117-01（P2）邮箱满时离开钩子把 Summary 成功当作“已合并完”

**根因**：`modules/mailbox/with/rehome/game/controllers/mailbox_rehome.go.tmpl` 的 `mailboxPullBroadcastsOnLeave` 只看 `mail.Summary` 有没有错误。mail 的合并（`service/mail/broadcast.go` `mergeBroadcasts`）在邮箱全是未读、放不下时停在那封之前、水位不越过它，这是正常背压：`refreshMailbox` 只记 `refused:merge_broadcast:mailbox_full` 指标，`Summary` 照样成功。钩子于是放行换服；到新服按新画像合并，旧服那封不匹配被跳过，水位越过它，永远补不回来；离开前发给新服的那封反而在腾出位置后被合并。

**修法**（最小、不改普通 List / Summary 的满箱语义）：
- `service/mail`：`Summary` 增加 `BroadcastsBehind bool`——这一次合并停在半路（还有属于他的全服邮件放不下）。`refreshMailbox` 把提交那次 CAS 的 `stopped` 返回，`Summary` / `PullBroadcasts` 填进去。经总线的客户端同样拿得到（`Summary` 结构体整个上线，`go generate` 无变化）。
- 离开钩子：`Summary` 成功但 `BroadcastsBehind` 时返回错误（包着 `svcmail.ErrMailboxFull`），按离开钩子的约定换服中止、account 撤销、玩家留在本服；清掉一些邮件后重新申请即可。
- 没采用“持久保留旧范围的待合并义务”（审查的第二个建议）：要改邮箱结构与合并规则，范围大；中止换服符合离开钩子“宁可留在本服，不带着没合并的邮件走”的现有约定。

**红（基线上）**：
- 新增模块场景 `TestWithMailboxRehomeRefusesToLeaveWithTheOldServersMailUnmerged`（`mailbox_rehome_test.go.tmpl`）：`rv01-red.log`
  `mailbox_rehome_test.go:177: leave hook with server 3's mail not merged (mailbox full, watermark 0): err = <nil>; want the move refused with mail's ErrMailboxFull`
- 审查原探针 `review_mailbox_rehome_full_test.go.txt`（模块路径改成生成工程的）：`rv01-review-probe-before.log`，`full_true` 失败 `old=false want true; new-before=true want false`，`full_false` 对照通过。

**绿**：`rv01-green.log`（`-race`）：三个 `TestWithMailboxRehome*` 通过；审查探针 `full_true` 分支钩子返回 `... mailbox is full ... mail: mailbox is full`（探针把“满箱时安全拒绝”判为可接受），`full_false` 对照仍通过。新场景还核对腾出一个位置后再离开：旧服那封合并、离开前发给新服的那封不合并。

**变异**：`refreshMailbox` 末尾改回 `return ..., false, nil`（不报告停在半路）：
- 服务层新用例 `TestLazyBroadcastSummaryReportsAMergeThatDidNotCatchUp` 的 local / bus 两种传输都红（`rv01-mutation-service-behind-false.log`：`BroadcastsBehind:false ... want success with BroadcastsBehind`）；
- 模块场景红（`rv01-mutation-module.log`，与修前同一句）。
已恢复。撤掉钩子里的 `BroadcastsBehind` 判断即回到修前代码，红见上。

**计数**：`mailbox+rehome` 场景 2 → 3（`modules/mailbox/module.yaml`、`codegen/internal/roost/module_gm_test.go`、`scripts/test-modules-generated.sh` 两处）。

**文档**：`docs/framework/guide/09-services.md`（合并段补 `BroadcastsBehind` 与“不能把 Summary 成功当作追平”）、`modules/mailbox/README.md`、`modules/rehome/README.md`（with/rehome 一行）、`module.yaml` 的联动说明、钩子文件头注释。

## RV-20261117-02（P2）C# 错误码常量撞上生成器自己的符号

**根因**：`codegen/internal/errcode/csharp.go` `GenerateCSharp` 的冲突表 `idents` 从空表开始，只防两个 errcode 名转成同一个 PascalCase；没把类里本来就有的名字算进去。合法的 errcode 名 `name` / `message` 变成 `Name` / `Message`，与生成的方法 CS0102；`error_codes` 变成 `ErrorCodes`，与类名 CS0542。另查出静态类从 object 继承的成员：`equals` / `get_hash_code` / `to_string` / `get_type` / `reference_equals` / `memberwise_clone` 生成同名常量会隐藏继承成员（CS0108 警告），客户端警告即错误时同样编译失败（`Finalize` 实测不报）。没有 TS / C++ 的错误码生成器（`client/ts`、`client/cpp` 里没有），只有 C# 一处。

**修法**（不缩小服务端合法集合）：撞上 `csharpOwnSymbols`（`ErrorCodes`、`Name`、`Message` 与上面六个 object 成员）的常量名加后缀 `_`：`name` → `Name_`。PascalCase 转出的标识符只有字母数字（数字开头补前导 `_`），不会以 `_` 结尾，所以加后缀的名字不会再撞别的常量；`Name(code)` 仍答原 errcode 名。顺带给 `ErrorCodes` 类加 `/// <summary>`：新的编译回归按配置表 C# 同样的严格度（XML 文档 + 警告即错误）编译时，缺类注释是 CS1591，这在审查里没覆盖到（审查探针没开 XML 文档）。

**红（基线上）**：`rv02-red.log`
- `TestGenerateCSharpCompiles`：经正式 `Definitions` 扫描（`errcode.Define` 写进 Go 源文件）→ `GenerateCSharp` → `dotnet build`（netstandard2.1、LangVersion 8、Nullable、TreatWarningsAsErrors、GenerateDocumentationFile）：CS0102 ×2（Name、Message）、CS0542（ErrorCodes）、CS0108 ×6；
- `TestGenerateCSharpRenamesIdentifiersThatClashWithItsOwnSymbols`：缺 `Name_` 等。
第一次加类注释前的绿跑另报 CS1591（类缺 XML 注释），见上。

**绿**：`rv02-green.log`，三个 `TestGenerateCSharp*` 通过（dotnet 实际编译）。`codegen/internal/roost` 的 client export / client sync / 配置表 C# 运行时用例 `ROOST_REQUIRE_DOTNET=1` 通过（见下“验证”）。

**变异**：去掉 `ident += "_"`：两个用例都红，dotnet 报 CS0102 ×4、CS0108 ×12、CS0542 ×2（`rv02-mutation-no-suffix.log`；每个错误 MSBuild 打两遍）。已恢复。

**文档**：`codegen/docs/CODEGEN_REFERENCE.zh-CN.md` 导出表、`client/README.md` 错误码一条。

## RV-20261118-01（P3）room 断线重试夹具在触发之后才注入失败

**根因**：`modules/room/files/game/modules/room/room_presence_test.go.tmpl` `TestRoomPresenceRetriesAFailedDisconnectAndGivesUp` 后半段先 `Track(2, 102, "b", 1)`（连接不在，`Track` 立即 `markPending` 唤醒后台断线），再往 `w.fail` 放三次错误。后台先跑时替身看到空队列答成功，登记被撤掉，后面的期望调用永远不来，2 秒后 `no disconnect`。产品代码没有问题，是夹具时序。

**修法**：三次错误先入队，再 `Track`；附注释说明原因。没有加长等待期限，没有放宽断言。

**红（基线夹具，强制一种夹具允许的调度）**：`force_room_schedule.py` 在 `Track(2, …)` 之后插一段“等后台断线先跑完”的等待（只改生成工程里的测试文件，不改运行时），原夹具必红：`rv03-red-forced-original.log`
`room_presence_test.go:176: no disconnect, want {roomID:2 playerID:102 epoch:1}`——与审查里自走棋首次矩阵的失败文本相同。

**绿**：修后夹具加同样的强制调度仍通过（`rv03-green-forced-fixed.log`）；不加强制 `-race -count=30` 通过（`rv03-green-race-x30.log`）。

**变异**（产品 `room_presence.go`，在生成工程里改、跑修后夹具、恢复；`room_mutate.sh`）：
- 不重试（第一次失败就放弃）：红，`a failed disconnect dropped the seat: 0 tracked`（前半段先抓到）；
- 两次失败就放弃（`maxDisconnectFailures = 2`）：红，后半段 `no disconnect, want {roomID:2 playerID:102 epoch:1}`；
- 永不放弃：红，后半段 `still tracking after three failures: 1`。
后两条正是修后夹具后半段所声称的“连续三次失败后放弃、之前每次补扫都重试”。

## 验证

- `GOWORK=off go vet ./service/mail/ ./codegen/internal/errcode/`；`GOWORK=off go test -race -count=1 ./service/mail/ ./codegen/internal/errcode/`：通过。
- `go generate ./service/mail/`：`up to date: mail_rpc_gen.go`，无漂移。
- 生成工程 `roost module verify --json mailbox rehome room`（`module-verify.json`）：mailbox 33、mailbox+enter 5、**mailbox+rehome 3**、rehome 62、rehome+enter 5、room 34，全部 0 skip；build / vet / id check / generate --check 通过。
- 生成工程 `-race`：`TestWithMailboxRehome*`（`rv01-green.log`）、room 重试用例 ×30（`rv03-green-race-x30.log`）。
- `codegen/internal/roost` 选跑（`ROOST_REQUIRE_DOTNET=1 GOWORK=off go test -count=1 -timeout 60m -run '^(TestModuleGMScenariosAndMutations|TestSDKGoogleProtobufVersionMatchesCodecs|TestClientExport.*|TestUndeclaredClientExportChangesNothing|TestGenerateClientExport|TestClientSync|TestClientConfigCSharpRuntime)$'`）：10 个用例全部通过，114.7 秒（`codegen-roost-selected.log`）；`TestModuleGMScenariosAndMutations` 按新计数 `mailbox+rehome: 3` 通过。`go vet ./codegen/internal/roost/` 通过，`bash -n scripts/test-modules-generated.sh` 通过（脚本本身没有跑）。
- 根包 `GOWORK=off go test -count=1 .`：`ok … 16.3s`（`root-package.log`）。
- 没有跑全量 `go test ./...`、pretag、`scripts/test-modules-generated.sh` 与 13 类玩法矩阵（按共用规则留给协调者）。

## 尚未确认 / 没做

- 没有用真实 Mongo / Redis / NATS 跑换服全链路（`scripts/test-demo-rehome.sh`）；满箱拒绝只在模块场景（真实 mail 服务 + 内存存储）里验证。
- 被中止的换服，原因只在 rehome 的 Warn 日志与向 account 中止时带的 reason（`leave hook mailbox: ... mailbox is full ...`）里，客户端拿不到专门的错误码；要不要在申请时（`OnApplying`）就按满箱拒绝并回 `ErrMailboxFull` 的码，是产品选择，留给维护者（见报告）。
- 只在 macOS 上跑；dotnet 10.0.302。

## 建议写进 CHANGELOG `[Unreleased]` 的文字

- **换服时邮箱满不再漏旧服全服邮件（RV-20261117-01，只对新生成的工程）**：mail 的 `Summary` 新增 `BroadcastsBehind`，表示这一次全服邮件合并因邮箱全是未读而停在半路（`Summary` 本身仍成功，普通 List / Summary 的满箱语义不变）。mailbox 的 `with/rehome` 离开钩子看到它就中止换服（错误带 `svcmail.ErrMailboxFull`），玩家留在本服、清掉一些邮件后再申请；修前钩子把 `Summary` 成功当作旧服邮件已合并完，换走后旧服那封补不回来，离开前发给新服的那封反而会在腾出位置后合并进来。`mailbox+rehome` 场景 2 → 3。已生成的工程手动同步 `game/controllers/mailbox_rehome.go`。证据 `docs/maintenance/evidence/v153-review-fixes-20261118/README.md`。
- **C# 错误码常量避开生成器自身的名字（RV-20261117-02）**：`ErrorCodes.cs` 里与类名 `ErrorCodes`、方法 `Name` / `Message`、object 继承成员（`Equals` / `GetHashCode` / `ToString` / `GetType` / `ReferenceEquals` / `MemberwiseClone`）同名的常量加后缀 `_`（`name` → `Name_`），修前合法的 errcode 名 `name` / `message` / `error_codes` 等会让客户端编译失败；`ErrorCodes` 类补 XML 注释（客户端开 XML 文档且警告即错误时不再 CS1591）。新增 dotnet 实际编译回归 `TestGenerateCSharpCompiles`（`ROOST_REQUIRE_DOTNET=1` 时缺 dotnet 失败）。**行为变化**：用到这些名字的客户端代码改用带 `_` 的常量名。
- **room 断线重试用例的夹具时序（RV-20261118-01，测试）**：`TestRoomPresenceRetriesAFailedDisconnectAndGivesUp` 先把三次失败入队再 `Track`，修前后台断线先跑时用例偶发 `no disconnect`（审查的自走棋矩阵首次失败）。产品代码不变。

## 建议写进 KNOWN-LIMITS 的文字

审查分支上的“2026-11-17：v1.53.0 新增内容审查”一段若并入 main，三项可直接标为已修（本证据）。框架限制一条（不是遗留）：

- **换服前邮箱满会被拒（mailbox + rehome）**：邮箱全是未读、还有属于他的旧服全服邮件没合并进来时，离开钩子中止换服（`ErrMailboxFull`），要玩家先清邮件；中止原因只在 rehome 的 Warn 日志与向 account 中止时带的 reason 里，客户端没有专门的提示码。没有做“持久保留旧范围的待合并义务”。
