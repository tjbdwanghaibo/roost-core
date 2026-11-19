# v1.58.0 与 main 续审的复跑说明

Linux amd64，Go 1.27.2、Node 24.22.1、GOWORK=off。tag 为 `2ae1c2312fdfb799bf46a7e8b5ba25cfc70cda55`，main 为 `66d6eab2f4901462aeeec63010f1962bafb2bfa7`。以下命令中的 `SRC` 是被测源码检出的绝对路径，`E` 是本目录绝对路径，`OLD_E` 是相邻的 `main-review-20261119-02`，`OUT` 为临时输出目录。构建与运行在源码仓或临时生成工程内完成。

- **Mongo**　按[上轮说明](../main-review-20261119-02/README.md)构建各版 CLI、名称读取器，并运行原 compare_names.py。脚本输出中的 baseline 为上次被测 CLI（640996a3e，与 1397ee400 的 CLI 一致），main 分别为本次 tag 或 main。每版读取器使用该版的 Mongo 包，在内存客户端检查库句柄名。[tag](mongo-tag.log) · [main](mongo-main.log)
- **GroupView / Interest**　用上轮 README 的 overlay 命令给本次 main 注入原 group_profile_test.go.txt，运行下列三项。第一项断言最终下发 room profile；另外两项是本版仓内的 GroupView 和 Interest 接手回归。另运行 `go test -count=1 ./framework/sync/entitysync/...`。[三轮 race](race.log) · [默认包测试](sync.log)

```bash
go test -race -overlay "$OUT/overlay.json" -count=3 -v \
  -run '^(TestReviewGroupMemberPromotion|TestGroupViewSharesSlotTakenBetweenPhases|TestInterestSharesOccupiedSlotWithoutSkippingFIFO)$' \
  ./framework/sync/entitysync/policy
```

- **TS Gate**　在 `"$SRC/client/ts"` 运行 `npm ci`、`npm run build` 和 `npm test`。套件包含 Go 互操作测试，须先将 Go 工具链加入 PATH。对旧版和本轮各执行三次 `node "$E/callback.mjs" "$SRC/client/ts"`。脚本导入该版实际编译的 SDK，传输与时钟采用其 dist-test 夹具。两请求均交给同一个公开 Connection。第二个请求期限 100 ms，触发关闭后推进到 1000 ms。日志列出两种关闭入口、两种对照、传输与连接状态、回调结果。旧版 TS 资产来自 640996a3e，与 1397ee400 的 client/ts 树相同。[main 套件](ts-suite.log) · [旧版对照](ts-baseline.log) · [main 对照](ts-main.log)
- **版本下限**　用 tag / main 各自构建的 CLI 在临时目录运行 `roost project new planet -module example.com/planet -mods configdata -skip-deps -roost-core-version <版本> -out <空目录>`，分别传 v1.57.1、v1.58.0，记录 go.mod 的 require 或拒绝原因。本轮另执行生成器下限、版本清单、core pin 脚本和兼容矩阵单测。[生成结果](floor.log) · [四项单测](versions.log)
- **历史四项**　TS 复用[原取消探针](../v156-main-review-20261118/cancel.mjs)，pretag 执行 scripts/pretag_scope_test.py，Origin 和数字主机运行日志中的正式 Go 定向测试。[结果](prior.log)
