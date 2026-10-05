# N08 codegen 非核心 review（2026-10-05，macOS）

基线 main `50e9a4e853641f348c879b85c282af9c5813f656`（含 cb11be90 的 Ctrl-C 重发信号与进程树补修，按要求直接复用、不重审）。分支 `revn08`，独立 worktree。范围按[接力清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md) N08 行：cfggen required / ref / skipempty 与真实 JSON / 索引往返；旧工程显式 upgrade、文件改名与退役、失败回滚、依赖整理；doctor / deps / generate / sync 的取消与暂存树清理；Unix 信号与进程树用例在 macOS 单独跑；shell 部署 / rollback 与 10 个具名环境 skip 补测。`add saga` 的生成部分与 `codegen/internal/roost` 里赠礼预算相关的 demo 步骤属 U-0280，本轮未读改。

环境：macOS 26（darwin/arm64，10 核），Go 1.27.0，`/bin/sh`、`shellcheck 0.11.0`、`docker compose` 可用；`systemctl` / `useradd` 不在 PATH（shell 用例自带替身）。GOPROXY `goproxy.cn,direct`。未使用 Mongo / Redis / NATS 等外部依赖。

图谱：项目 `Users-whb-roost-roost-core` generation 2026-09-30（落后 HEAD）；`command_tree*.go` not_tracked，`dependencies.go` / `generate.go` / `doctor.go` / `shutdown_budget.go` / `help.go` / `cfggen/main.go` metadata_changed，其余引用文件 metadata_match。结构定位后全部以当前源码逐行核对，下文结论不依赖旧图谱。

## 结论

| 编号 | 等级 | 一句话 | 状态 |
| --- | --- | --- | --- |
| [RR-20261005-NC-70](../bug/RR-20261005-NC-70.md) | P3 | go 命令窗口被 Ctrl-C / SIGTERM / SIGHUP 打断后，`.roost-deps-*` / `.roost-generate-*` 整份工程副本留在工程父目录 | 已复现 |
| [RR-20261005-NC-71](../bug/RR-20261005-NC-71.md) | P3 | `project diff` / `upgrade --dry-run` 漏列 sync 将刷新的三份应用自有配置；生成文档与 help 称 sync / upgrade 不改配置 | 已复现 |
| [RR-20261005-NC-72](../bug/RR-20261005-NC-72.md) | P3 | `roost help cfggen` 让输出落到 tablegen 的 `configs/generated`，照做即重复声明、`roost generate` 失败 | 已复现 |
| [RR-20261005-NC-73](../bug/RR-20261005-NC-73.md) | P3 | `roost id` / `roost add errcode` 用正则找错误码：别名导入的定义被漏掉（分配到已占用的码）、注释里的文字被算成占用（误报重复）；NC-63 交给 N08 的残余 | 已复现 |

四条都是 codegen 工具链的可用性 / 文档缺陷，没有发现会损坏工程或生成错误运行时行为的问题。

## 已核对项

| 项 | 所有权 / 调用链（当前源码） | 实际执行与结果 | 证据 |
| --- | --- | --- | --- |
| cfggen required / ref / skipempty 生成 | 校验 `codegen/internal/cfggen/main.go:547-597`（`validateEntry`：required 需 ref、skipempty 需 index、ref 只用于标量），ref 类型须等于目标主键 `:480-494`，主键限整数 / string `:144-146`；`writeStruct :721-760` 写 `cfg:"index[,skipempty]" / "ref=<t>[,required]"`；访问器 `indexValueExpr :763-774` 与运行时 `indexStringifier` 同一格式化 | 既有 cfggen 包 race ×3 通过；表格校验（required 无 ref、skipempty 无 index、ref 类型不符等）既有用例覆盖 | 下行 |
| 运行时往返 | `configdata/auto.go:337-458`（`parseAutoSpec`）解析标签；`validateRefs :465-494`（required 零值、成员校验）；`configdata/configdata.go:190-220` 建索引（`SkipEmpty` 丢空键）；`Store.build :1102-1200` 先全部加载再逐表校验，所以前向 ref 合法 | (1) 当前 core（replace 到 worktree）上自写往返 9 叶子 race ×3 通过：uint64 最大值 / 负 int64 / bool / uint32 最大值索引，skipempty 的 0 / false / "" 不入索引，无 skipempty 的 "" 按文件序保留，显式索引名，string 键 ref 与前向 ref，required 缺键 / 零值，悬空 ref 与目标键删除时 reload 被拒、live 快照与索引不动、冷加载同样拒绝，分组省字段在非 strict 下忽略、strict 下拒绝（文档化行为）。(2) 把这些形状并入正式门 `codegen/scripts/cfggen-golden-runtime.sh` 的夹具（新增 `TestIndexOptionsAndStringRefsRoundTrip`），对 pin 的 v1.20.0 core 5/5 通过；负对照（生成器丢掉 skipempty）该用例红 | [证据](evidence/noncore-review-20261005-n08/README.md) |
| 旧工程显式 upgrade | `cli.go:211-256`：`loadManifestForUpgrade` → `mergeVersions` → `planManifestSync` 预检 → `commitManifestSyncResult`（`add.go:326-348`，先写 manifest 再 `SyncProject`，失败回滚 manifest）→ `UpdateFrameworkDependencies` | v1.18.0 CLI 生成、pin v1.18.0 的工程（configdata,mongo,nats,dataengine,nest）用当前 CLI `upgrade -core v1.20.0`：19 文件 + manifest + go.mod/go.sum，build / vet / `go test ./...` 通过，`generate --check` 最新，doctor 22 OK。dry-run 预览漏列 3 份配置 → NC-71 | 同上 |
| v1.18.0 game-demo | — | 该工程生成即 `core: latest`，解析到 v1.20.0 后编译失败（`routing.AcquireLease` 等已删除）。v1.20.0 CHANGELOG 明确“破坏性变更……已生成工程不提供迁移（维护者决定）”，属已接受边界，不登记 | CHANGELOG v1.20.0 段 |
| 改名与退役 | `planStagedProjectCommit`（`project.go:258-368`）：暂存树中消失的生成物镜像为删除，仍可识别为生成物才删、保留前像 | game-demo 上新增 DAO → 生成 `db/gen_pet_dao.go`；改文件名与类型名 → 旧输出删除、新输出生成；删定义 → 输出删除；`generate --check` 最新；无暂存残留。既有 entity / protocol / table 退役用例在全包中通过 | 同上 |
| 失败回滚 | 生成器失败：`commitManifestSyncResult` 回滚 manifest；deps 失败：`rollbackDependencyUpdate` 恢复暂存树 go.mod/go.sum，不提交 | (R1) 加一个语法错误的 DAO 定义后 upgrade：退出 1，`roost.yaml` 回到 v1.18.0，工程零改动，无暂存残留。(R2) PATH 上放失败的 go 后 upgrade：退出 1，“project upgraded but framework resolution failed”，manifest 与模板已升级、go.mod 未变；随后 `roost project deps` 收敛，build 通过 | 同上 |
| 依赖整理 | `updateFrameworkDependenciesTransactional`（`dependencies.go:31-80`）：暂存树里 consolidation + `go get` + `go mod tidy`，只提交迁移产物与 go.mod/go.sum，提交前核对应用输入 | 上面两次 upgrade 与 R2 恢复；`go get` 超时 5 分钟走错误路径时暂存树被删（对照） | 同上 |
| 取消与暂存树清理 | `runCommandTree`（`command_tree.go`）接住中断、杀树、重发信号；暂存树只靠 defer | 正式 CLI 上 deps / generate / sync / upgrade / new 五条命令在 go 命令窗口收到 SIGINT：都死于 SIGINT、go 子树不在，但暂存树全部残留 → NC-70 | 同上 |
| ID 工具与 errcode 生成器（N07 移交） | `id.go:15` 正则 → `ScanIDs` → `NextID` / `CheckIDs` / `add.go` 显式冲突检查；生成器 `codegen/internal/errcode/main.go` AST 扫描（NC-63） | game-demo 副本上别名导入定义被漏掉、注释被计入 → NC-73 | [CLI](evidence/noncore-review-20261005-n08/cli-id-errcode.txt) |
| Unix 信号 / 进程树用例 | `go_command_tree_promises_test.go`、`generator_cwd_promises_test.go`（`//go:build unix`，Windows 跑不到） | macOS 单独 `-race -count=3`：7 用例 21 次全部通过（doctor 超时两条、deps 取消两条、Ctrl-C 一条、生成器 cwd 两条） | 同上 |
| 10 个具名环境 skip | 记录：`docs/bugfix/evidence/noncore-bugfix-20261005-14/codegen-summary.json` | macOS 全 codegen 树：9 条实际执行通过（core-pin 脚本、dev-run 三叶子、compose 形状检查（真实 docker compose config）、shell rollback / release switch 四条）。第 10 条 `TestDoctorPlayerTCPPassesAfterAuthAndConfig` 不是环境 skip，是编译期常量门（见观察 O1） | 同上 |
| shell 部署 / rollback | 生成的 `deploy/shell/{install,rollback,build,healthcheck}.sh`、`deploy/dev`、`deploy/docker`、`deploy/k8s` 脚本 | 四条 shell 用例在真实 sh / mv / install 下通过；`shellcheck -s sh` 对 game-demo 全部 9 个脚本：1 条 note SC2086（`run.sh` 有意按空白拆分 `$SERVICES`），无 warning / error | 同上 |

## 观察与建议（未改行为，不登记 RR）

- **O1 过期的测试门**：`codegen/internal/roost/roost_test.go:19` `const publishedDataEngineGeneratorDependencies = false`（09-01 引入，注释说“最低发布版本含 Data Engine 后翻转”）。最低版本早已满足，它让 `TestDoctorPlayerTCPPassesAfterAuthAndConfig` 永远 skip，也让 `TestExplicitFirstBusinessWorkflow…` 不跑事务生成 + strict doctor。本机临时置 true：两条都通过（7.7s，需模块缓存 / 代理）。没有提交翻转：会给默认测试套件引入联网依赖，是否接受由维护者决定。
- **O2 upgrade 依赖失败后的半升级态**：R2 中 manifest 与模板已升级、go.mod 未变，错误文字没有像 `project new` 那样提示“恢复后运行 roost project deps”；此时 doctor 的修复建议（“fix package/import errors without changing go.mod”）也不对症。建议错误里带上 `roost project deps --root <dir>`。
- **O3 生成失败信息指向已删除的暂存路径**：例如 `parse …/.roost-sync-3808757737/db/def/broken.go:5:24`，命令返回时该路径已不存在。`generate` 回放 stdout 时会把暂存路径替换回工程路径，错误值没有。
- **O4 configdata ref / required 错误用 Go 字段名**（`field ItemCode references missing item key axe`），schema 与 JSON 里是 `item_code`。可读性问题。
- **O5 合仓前模块路径**：`codegen/README.md`、`codegen/docs/CFGGEN_META.zh-CN.md`、`CODEGEN_REFERENCE` 多处命令仍是 `github.com/tjbdwanghaibo/roost-codegen/cmd/...`（冻结旧仓）。本轮只改了与 NC-71/72 相关的句子。
- **O6 NC-70 的残余窗口**：生成器、`copyProject`、提交阶段没有信号处理，被中断时同样可能留下暂存树（毫秒到秒级窗口）；提交阶段被中断还可能留下部分提交（重跑 sync / deps 可收敛）。要彻底覆盖需在暂存树整个生命周期接管信号，会和 cb11be90 的 `runCommandTree` 信号处理交互，本轮不做。

## 外部 / 未验证

- Windows：`runCommandTree` 不接管信号、本轮修复不改变 Windows 行为；`command_tree_windows.go` 只做 `GOOS=windows go vet`。
- 真实部署：shell 用例用替身 systemctl / useradd，没有在真实 systemd 主机上部署 / 回滚；k8s / docker 只做了 compose 形状检查，没有真实集群。
- 联网依赖：upgrade 用的是公共代理上已发布的 v1.18.0 / v1.20.0；离线代理、私有模块认证路径未测。
- cfggen 正式门对 pin 的 v1.20.0 跑；HEAD core 上的同形状往返在 scratch 模块里跑（9 叶子），两者 configdata 自 v1.20.0 起无改动。

## 方向判断

codegen 近期修复链：RR-20261004-12（生成器 chdir 让子进程继承暂存树）→ RR-20261004-13（go 命令按进程树取消）→ cb11be90（重发信号竞态补修）→ 本轮 NC-70（中断时暂存树残留）。四条都落在“同级暂存树 + 外部 go 进程 + 信号”这一个机制上，且每次修复都在增加状态（进程组、WaitDelay、信号接管、重发等待、本轮的中断清理登记）。这符合“同一机制反复出缺陷”的信号。

判断：根因不是某一行实现，而是“暂存树的生命周期”没有单一所有者——创建在命令里、清理靠 defer、信号却在 `runCommandTree` 里被接住并让进程死掉。候选方向：

1. **（本轮采用，最小）** 暂存树向 `runCommandTree` 登记，中断时由它在重发前删掉命令所在的那一棵。代价小，覆盖唯一的长窗口，残余窗口见 O6。
2. **整命令信号所有权**：在 CLI 入口（`cmd/roost/main.go`）统一接管 SIGINT / SIGTERM / SIGHUP，转成 context 取消，命令走正常错误 / 回滚路径并删除暂存树，最后由入口重发信号。能一并消掉 O6 的窗口和 cb11be90 的“重发后等待”假设，但要给 `GenerateTransactional` / `UpdateFrameworkDependencies` / doctor 加 ctx 参数，并重写 cb11be90 的中断用例。
3. **暂存树放进可回收位置**：例如工程内 `.roost/stage/`（已被 `skippedProjectDirectory` 跳过、可在下次运行时清理旧树），配合方向 1。

建议维护者在下一次碰这条链时考虑方向 2，避免再在 `runCommandTree` 上叠加分支。

## 修复（同日）

四条均按先红后绿修复、声明场景验证，未发版：[NC-70](../bugfix/RR-20261005-NC-70.md)（中断时删掉命令所在的暂存树再重发信号）、[NC-71](../bugfix/RR-20261005-NC-71.md)（预览先做 sync 的 shutdown 块刷新，文档写准）、[NC-72](../bugfix/RR-20261005-NC-72.md)（cfggen 帮助改用 `configs/cfg`）、[NC-73](../bugfix/RR-20261005-NC-73.md)（`roost id` 复用生成器的 AST 扫描）。另把 cfggen 运行期往返的新形状并入正式门 `codegen/scripts/cfggen-golden-runtime.sh` 的夹具（测试覆盖，不改行为）。上表“已复现”是审查时点的状态。
