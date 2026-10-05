# N14 Kit 跨域装配审查证据（revn14）

基线 `f6245613`，macOS，Go 按 `go.mod`；全部 `GOWORK=off`。共享环境 `remote-acceptance.lock` 存在，未跑 integration 与真实依赖。

| 文件 | 内容 |
| --- | --- |
| [nc190-red.txt](nc190-red.txt) | NC-190 正式回归在基线上的失败输出（app / kit/mods / kit/saga / kit/redis） |
| [nc191-red.txt](nc191-red.txt) | NC-191 `TestStartDoesNotLogTheMongoPassword` 基线失败输出（口令为测试值） |
| [nc192-probe.txt](nc192-probe.txt) | NC-192 生成 game-demo 生产示例 + `env: production` 的校验结果（临时探针，未提交）与各要求键的读取方计数 |
| [nc193-red.txt](nc193-red.txt) | NC-193 启动失败收尾两条用例的基线失败输出（日志节选，持有者值已脱敏） |
| [nc194-red.txt](nc194-red.txt) | NC-194 `TestPerStepOverrideAppliesToMixedCaseNamesWithoutDefinitions` 基线失败输出 |

复跑：回归随修复提交进入仓库后，在修复前的提交上 `git stash` 实现文件、保留测试即可得到同样的红；生成工程探针按 NC-192 记录的步骤重建（`go run ./codegen/cmd/roost project new n14demo -module example.com/n14demo -out <scratch>/n14demo -template game-demo`，依赖解析失败不影响配置文件）。
