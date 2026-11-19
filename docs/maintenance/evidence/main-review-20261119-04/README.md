# 2026-11-19 第四次 main 续审证据

Linux amd64，Go 1.27.2、Node 24.22.1、.NET SDK 10.0.402、MongoDB 8.0.16，GOWORK=off。旧基线为 `66d6eab2f4901462aeeec63010f1962bafb2bfa7`，被测 main 为 `9fca5547a3d1304915999ce688d94dd6e94c7fab`。以下 SRC 为 main 检出、E 为本目录、OUT 为临时输出目录，均用绝对路径。

## 新发现的对照

- **系统保留名的生成规则**　按[第二次续审说明](../main-review-20261119-02/README.md)从各自检出构建 CLI 和 read-names。对每个版本运行 `python3 "$E/reserved.py" "$OUT/roost" "$OUT/read-names"`。每行依次是工程名、业务库名、sid 1000 的库名。这里只检查句柄，没有业务数据读写。[旧基线](reserved-before.log) · [main](reserved-main.log)
- **换服演练的快照选库**　从 main 构建 CLI，把 sid_names.go.txt 复制为临时 .go 文件，在 main 根目录执行 `go build -o "$OUT/sid-names" "$OUT/sid_names.go"`。运行 `python3 "$E/snapshot_names.py" "$SRC" "$OUT/roost" "$OUT/sid-names"`。探针生成三个 game-demo，读各自实际配置，以正式 Mongo 包计算 sid 1000/1001 的句柄名；再从被测脚本原样提取 snapshot 函数执行，记录它交给 mongosh 的查询参数。mongosh 和绑定查询用替身；此项仅验证实际选库路径，没有运行完整换服流程。[三组结果](snapshot.log)

## 原问题复验与本轮测试

- Mongo 长名沿用[原比较程序](../main-review-20261119-02/compare_names.py)。旧 CLI 取上次已审提交，新 CLI、库名读取器均取被测 main。[业务库和分服库](long.log) · [生成器的开发及生产示例配置用例](codegen.log)
- TS 在 main 的 client/ts 先执行 npm ci、npm run build 及 tsc -p tsconfig.test.json，然后执行[原回调探针](../main-review-20261119-03/callback.mjs)三次和 npm test。每次探针覆盖两种关闭入口、三种回调。TS 套件调用 Go 互操作测试，运行前把对应工具链加入 PATH。[回调结果](callback.log) · [全套](ts.log)
- GroupView 的［原场景＋槽位交接］按[上轮命令](../main-review-20261119-03/README.md)通过 overlay 在 main 上运行三轮 race。[结果](group.log)
- Unity 使用本版脚本构建 Linux managed-only 包，再按[原托管探针](../main-review-20261119/README.md)复验。portal 由本版 CLI 生成 game、go.mod replace 指向同一检出，再安装 portal；在 account 配置副本中分别放入日志内四个地址，执行 roost config check。前导零 Origin、TS 主动断开、pretag 选择器与数字主机执行定向检查。[Unity](unity.log) · [portal](portal.log) · [其他历史检查](earlier.log)
- DataEngine 在本次独立建立的本机单节点 Mongo 副本集运行。向 ROOST_DATAENGINE_IT_MONGO_URI 提供该私有地址，然后执行 `go test -tags integration -count=3 -v -run '^(TestRealMongoShorterWALAfterProjectedBatch|TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint)$' ./framework/dataengine/engine`。短 WAL 测试保留 0、1、2 条完整记录。另跑默认 DataEngine 包。[真实依赖](power.log) · [默认包](engine.log)
- MOBA 使用同一副本集，以 ROOST_MOBA_REAL=1 运行 scripts/test-moba-generated.sh，生成工程与 CLI 均来自本次 main。临时生成工程缺少可用的 Git 版本戳，重跑时 GOFLAGS 加入 -buildvcs=false。普通 29 个用例、wallet 场景及工程检查、冷恢复、断尾与 SIGKILL 演练由此脚本执行。未启用额外的 ROOST_MOBA_ROUTING。[脚本完整输出](moba.log) · [模块及工程检查](moba-modules.json)
- 根包运行 TestCoreDependencyBoundary、TestGeneratedProjectRealDependencyTestsHaveAnEntry 与 TestRealProcessDependencyEntriesRemainReachable。AddExp 使用 main 内原始资料执行 docs/maintenance/evidence/local-recovery-four/addexp.py。[根包结果](boundary.log) · [归档复算](addexp.log)
