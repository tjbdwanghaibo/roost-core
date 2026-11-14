# game-demo 停机预算断言修正（2026-11-14）

最终 Go 包复验发现的六条失败来自旧测试预期；P9 安装 `room/turn/matchroom/matchmaking` 后，Room 的持久定时器增加一个正式 `kitentitytimer.NewMod(EntityAccess, room.TimerDriver)`。本批只改测试和证据，不删合法 Mod、不改生产预算、运行时或生成格式。完整包的最终验收另由主任务执行，本批不是整包通过声明。

## 原失败与原因

基线 `48ed0113ac17897b8bb1eebd8821c75b10813f3d`。原始 `/private/tmp/v147-final-roost-retry-20261114.log` 同时有六条断言失败及 30 分钟整包超时；[原始失败摘录](original-six-failures.log) 保留全部六条和超时行，[元数据](source-evidence.json) 保留原日志 SHA-256。不能把该轮归为只有超时。

测试缓存夹具 `projectFixtureOptions["game-demo"]` 显式选 `configdata,mongo,nats,dataengine,nest`，没有 CLI 默认的 `manager`。P9 的 `turn` manifest 声明 `TurnDeadline`，`timerServices` 找到 Room 驱动，`serviceModConstructors` 将它加入 bootstrap；它没有独立停机预算，按普通 Mod 加 3 秒。

| 形状 | service + shared Mods | total / grace | 独立生成证据 |
| --- | --- | --- | --- |
| 测试夹具的显式 Mods | 25 + 3 = 28 | 126s / 131s | [bootstrap 与配置](fixture-game-mods.txt) |
| 默认 CLI（另含 manager） | 26 + 3 = 29 | 129s / 134s | [bootstrap 与配置](planet-game-mods.txt) |
| 测试夹具再移除 session | 24 + 3 = 27 | 123s / 128s | 定向组的移除 Mod 回归 |

夹具固定算式为 `DataEngine 30s + TCP 10s + 26×3s + singleton release 3s + margin 5s = 126s`，grace 再加 5 秒。测试继续从生成 bootstrap 独立枚举实际 Mod，固定核对 28 和 Room 驱动，再检查开发/生产配置、k8s、compose、systemd、dev 脚本与 doctor 的相同预算；预期没有改为调用生产计划。

singleton 开关对照改用 119s：关闭锁时能盖住 118s，开启时不足 121s。手改总预算保持 129s，使用会检查是否实际改到配置的现有 helper，防止旧 `strings.Replace("123s", ...)` 静默不执行。配置 TCP 30s 的 advice 独立固定为 `60 + 26×3 + 3 + 5 = 146s`。

初次定向组还发现第七个同源旧预期：移除 session 后仍应计入 Room timer，旧断言是 26 Mods / 120s / 125s。[该轮完整红日志](targeted-initial-red.log) 已保留，未覆盖成通过记录。

## 复跑与验证

在本批工作树执行，仅选择这两个测试文件的 31 个顶层用例（shutdown 25 + singleton 6）：

```sh
test_names="$(sed -n 's/^func \(Test[[:alnum:]_]*\)(.*/\1/p' codegen/internal/roost/shutdown_test.go codegen/internal/roost/singleton_test.go | paste -sd '|' -)"
GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0 go test -count=1 -timeout 15m -v -run "^($test_names)$" ./codegen/internal/roost
```

独立 CLI 检查复用了本次最终模块矩阵构建的 `/private/tmp/v147-final-modules-20261114/roost`，完整生成工程保留在 `/private/tmp/v147-shutdown-assertions-20261114/{fixture,planet}`。同一 CLI、同一基线模板；前者使用以下显式 Mods，后者省略 `-mods`，分别核对 28/29 个实际构造器与生成配置，未启动业务服务。

```sh
GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0 /private/tmp/v147-final-modules-20261114/roost project new planet \
  -module example.com/shutdown-accept -out /private/tmp/v147-shutdown-assertions-20261114/fixture \
  -template game-demo -mods configdata,mongo,nats,dataengine,nest --skip-deps
```

31 个顶层用例全部通过，0 FAIL / 0 SKIP，包耗时 103.834s；见[完整定向绿日志](targeted-green.log)。临时在 `serviceShutdownPlan` 只对 game 执行 `plan.mods--`，模拟预算漏计一个实际 Mod；[变异红日志](mutation-minus-mod-red.log) 在 game 的配置与全部部署出口固定断言失败（exit 1，35.587s），bootstrap 仍是 28 Mods。随后按备份原字节还原 `shutdown_budget.go`，该生产文件 `git diff --exit-code` 为 0。还原后以 `GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0 go test -count=1 -timeout 5m -v -run '^TestGeneratedShutdownTimeoutFollowsEachServicesMods$' ./codegen/internal/roost` 复跑同一用例，[复绿日志](restored-green.log) 通过（exit 0，34.904s）。`gofmt` 与 `git diff --check` 通过；最终生产文件无改动。所有执行是功能验证，macOS arm64 / Go 1.27.3；不作性能结论。CBM 按 Verify/Tier 2 使用 `Users-whb-roost-worktrees-v147` / `2026-11-14T02:55:37Z`，相关断言与预算 coverage 无记录缺口；`render.go`、`demo.go` 代际变化已读当前源码，沿 constructor/timer 两向 trace 核对来源，没有把模板搜索空结果当作缺实现。
