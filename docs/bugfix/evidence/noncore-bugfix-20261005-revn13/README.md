# N13 revn13 修复证据（2026-10-05）

基线 `f6245613`（审查与修前红），修复提交后 rebase 到 `c35fe606`（含 N12 的 statslog 改动）复跑，推送前再 rebase 到 `ae66d593`（含 NC-170～174 的 manager / nest 停机修复）复跑。环境：macOS / Apple M5，`go1.27.0`，全部 `GOWORK=off`。本轮没有真实依赖场景，未跑 integration。

| 文件 | 内容 |
| --- | --- |
| [red.txt](red.txt) | 修前：container 四条、entity 组合一条、safemap 一条、goroutine 一条（含第一版用例不红、改后三次均红的说明） |
| [red-nc182-generated-dao.txt](red-nc182-generated-dao.txt) | 修前：codegen golden `VarietyDao` 在一次性模块（replace 到基线）里 `RangeFastItems` + `SetFastItems`，提交记录写入 `fast_items.0` |
| [green-nc182-generated-dao.txt](green-nc182-generated-dao.txt) | 修后：同一组合与全部 golden runtime 用例 |
| [green-nc183.txt](green-nc183.txt) | 修后：NC-183 连跑三次 |
| [bench-fastmap.txt](bench-fastmap.txt) | `BenchmarkFastMapSetGet` 修前 / 修后 benchstat（n=6），无显著差异 |
| [verify.txt](verify.txt) | gofmt / vet / 改动与相邻包 race×3、全仓 build/vet、根包、nest race、codegen、glsvet、全新生成 game-demo build/vet/test |

一次性 DAO 模块的做法同 `codegen/scripts/dao-golden-runtime.sh`（拷 golden 与 `runtime/*_test.go`，去掉 redis golden），只是 `replace` 到本地源码而不是 `go get` 发布版本；用例源码附在 red 文件末尾。全新生成 game-demo：`go run ./codegen/cmd/roost project new n13demo -module example.com/n13demo -out <scratch>/n13demo -template game-demo`，`go mod edit -replace` 到本分支后 `go mod tidy`。
