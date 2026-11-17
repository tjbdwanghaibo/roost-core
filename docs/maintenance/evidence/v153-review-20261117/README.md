# v1.53.0 新增内容审查证据

结论见[报告](../../V153-REVIEW-20261117.md)。被测检出 `44241b956fd0bb118c1ded7535425524b8ffa10d`；生成器和运行依赖来自同一检出。Linux x86_64、Go 1.27.2、.NET SDK 10.0.402，C# 目标为 netstandard2.1。

## 故障复现

### 邮箱满时换服

创建独立工程（`CORE_REVIEW_ROOT` 为被测检出绝对路径，`REVIEW_TMP` 是新建的工作目录）：

```bash
export GOWORK=off
cd "$CORE_REVIEW_ROOT"
go build -o "$REVIEW_TMP/roost" ./codegen/cmd/roost
"$REVIEW_TMP/roost" project new mailreview -module example.com/mailreview \
  -out "$REVIEW_TMP/mail" -template game --skip-deps
cd "$REVIEW_TMP/mail"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE_REVIEW_ROOT"
"$REVIEW_TMP/roost" module add mailbox rehome
```

将 `review_mailbox_rehome_full_test.go.txt` 去掉 `.txt` 后放入生成工程 `game/controllers/`，运行：

```bash
go test -race -count=3 -v -timeout 3m \
  -run '^TestReviewMailboxRehomeFull$' ./game/controllers
"$REVIEW_TMP/roost" module verify --json
```

`mail-full-probe.log` 是故障探针：满 200 封场景三轮失败、199 封对照三轮通过。`mail-module-verify.json` 是正式模块验收；模块测试选择不包含临时审核探针，故二者结论不矛盾。所用 mail 服务是真实实现，版本存储和信封存储是模块自带的内存替身，不需要 Redis / NATS，也没有执行完整换服搬档。

### C# 错误码名称

把 `review_csharp_names_test.go.txt` 去掉 `.txt`，临时放进被测检出的 `codegen/internal/errcode/`，运行后移走：

```bash
REVIEW_CS_OUT="$REVIEW_TMP/csharp" go test -count=1 -v \
  -run '^TestReviewExportCSharpNames$' ./codegen/internal/errcode
```

分别进入输出的 `profile_name`、`name`、`message`、`error_codes` 目录执行 `dotnet build Review.csproj --nologo --verbosity minimal --ignore-failed-sources -p:UseSharedCompilation=false`。

`csharp-generate.log` 记录四种输入均被正式扫描和生成接受；`csharp-compile-summary.json` 记录每种三次编译退出码。`csharp-<名称>-<轮次>.log` 保留编译器诊断；`csharp/` 下保存原 Go 定义、生成 C# 与项目文件（均加 `.txt`，以免被仓库工具当成源码）。普通名称三次通过，其余三种各三次失败。错误码生成不经过 protoc，不能用“假 protoc 输出不可编译”解释此结果。

### room 重试夹具的错误注入顺序

在同版本自走棋生成工程中，将 `review_fixture_order_test.go.txt` 去掉 `.txt` 放入 `game/modules/room/`，运行：

```bash
go test -mod=mod -race -count=3 -v -run '^TestReviewPresenceFixtureInjectionOrder$' ./game/modules/room
go test -mod=mod -race -count=30 -v -run '^TestRoomPresenceRetriesAFailedDisconnectAndGivesUp$' ./game/modules/room
```

`room-fixture-order.log` 保留迟注入三次失败、先注入三次通过；`room-retry-original.log` 保留原测试单独 30 轮通过。`fixture-order-control.diff` 仅调整生成测试的注入顺序（零上下文 diff，可用 `git apply --unidiff-zero` 应用），应用后原测试 30 轮通过，见 `room-retry-fixture-control.log`。这些结果验证夹具时序，不代表运行时重试失效。最终自走棋矩阵另用原模板生成新工程复跑，未带此补丁。

## 正式验收命令与边界

13 类玩法按仓库正式矩阵入口生成，用 `--genres` 分成三组并行执行；每个工程均重新改成自定义 module、执行 build / vet / ID / 生成漂移检查，再运行自定义 module 场景。正式命令：

```bash
python3 scripts/test-genres-generated.py --out "$REVIEW_TMP/genres-a" --genres slg moba mmorpg card-rpg
python3 scripts/test-genres-generated.py --out "$REVIEW_TMP/genres-b" --genres tower-defense survival-extraction auto-chess party-room
python3 scripts/test-genres-generated.py --out "$REVIEW_TMP/genres-c" --genres merge-casual idle-sim turn-pvp coop-roguelike social-sim
```

13 类玩法矩阵最终全部通过、零跳过：默认玩法 181 次、自定义 module 定向测试 183 次；每种 module 路径下分别合计 909 个模块场景（跨工程重复模块不算不同用例）。其中 12 类首次通过，自走棋首次遇到上述夹具问题，未修改的全新工程复跑通过。

| 玩法 | 默认玩法测试 | 自定义 module 定向测试 | 每种 module 路径的模块场景 |
| --- | ---: | ---: | ---: |
| auto-chess | 13 | 13 | 80 |
| card-rpg | 17 | 17 | 174 |
| coop-roguelike | 10 | 10 | 51 |
| idle-sim | 13 | 13 | 74 |
| merge-casual | 16 | 16 | 84 |
| mmorpg | 14 | 14 | 75 |
| moba | 28 | 28 | 17 |
| party-room | 19 | 19 | 46 |
| slg | 8 | 10 | 17 |
| social-sim | 9 | 9 | 35 |
| survival-extraction | 13 | 13 | 100 |
| tower-defense | 11 | 11 | 80 |
| turn-pvp | 10 | 10 | 76 |

`genres-{a,b,c,retry}-report.json` 保留分组原报告；`genres/<玩法>/` 保存最终 matrix/test 日志、两种 module 路径的模块验收 JSON、自定义 module 测试名称与结果。`auto-chess-initial-failure/` 单独保留首轮失败，不能被最终通过覆盖。自走棋全矩阵复跑命令为：

```bash
python3 scripts/test-genres-generated.py --out "$REVIEW_TMP/genres-retry" --genres auto-chess
```

`main-source-equivalence.json` 记录被测提交与收尾 main 的问题路径对比；这只证明这些文件没有变化，不表示 v1.54.0 已完成验收。

本轮设置 `GOWORK=off`、`GOFLAGS='-mod=readonly -buildvcs=false -p=2'`，矩阵进程 `GOMAXPROCS=2`；`-buildvcs=false` 只绕开云环境外层 Git 元数据限制。各脚本自己的 `-mod=mod` 参数照常生效。没有扩大测试超时，没有修改业务源码或断言。

`codegen-summary.json` 列出实际执行的顶层测试名称与结果：DAO 54 条初次通过、1 条子进程超时后单独复跑通过，protocol 22、errcode 4、roost 定向 16 条通过；没有 SKIP。三个原始 JSONL 保留首次失败。`mail-module-verify.json` 为 102 场景零跳过和四项工程检查通过；`demo-terrain.log` 是普通图 / 分区图 / 再回普通图的三个子场景通过。

生成器基础测试为 `go test -count=1 -parallel 2 -timeout 15m -json ./codegen/internal/dao ./codegen/internal/protocol ./codegen/internal/errcode`。初次 DAO 消费者用例触发其内部 2 分钟子进程期限，日志保留；单独用原用例、原内部期限复跑通过，见 `dao-consumer-retry.jsonl`。

roost 定向测试使用 `-count=1 -parallel 2 -timeout 20m -json`，选择 `TestSDKGoogleProtobufVersionMatchesCodecs`、`TestClientExport*`、`TestUndeclaredClientExportChangesNothing`、`TestGenerateClientExport`、`TestClientSync`、`TestModuleVerify*`，以及 demo 的实体包守卫、view 接口/名称、模块组件归属和可构建写路径测试。它们的 protoc 用例沿用正式的测试替身，不冒充真实 protoc / Unity 验收。

DAO runtime 将正式 golden 与 `boot_scope_test.go`、`ownership_test.go` 放入临时 module，移除其 `daoruntime` 构建标签，replace 到被测 core 后执行 `go test -mod=mod -race -count=1 -v -run '^TestBootScopedDaoFollowsTheProcessChoice$' .`；`dao-boot-runtime.log` 核对 DAO scope 和提交记录 Key.Scope。另在同版本 CLI 生成的 game-demo 运行 `TestTerrainDocumentsFollowTheMapOwnership`，结果独立记录。

默认玩法与生成器测试使用替身存储，部分机器人经过真实本机传输；具体覆盖以矩阵报告中的 `genre_network_acceptance` 为准。没有执行本批真实外部存储、跨机、Unity 或压测；环境重建前已经丢失的日志未计入本目录的证据。
