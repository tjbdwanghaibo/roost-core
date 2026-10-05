# N10 第二批证据（revn10b）

基线 `81d7cb16`，macOS arm64，Go 1.27.0，`GOWORK=off`。[本轮](../../../review/REVIEW-2026-10-06-noncore-n10b.md)。

| 文件 | 内容 |
| --- | --- |
| [red-ai.txt](red-ai.txt) | NC-240（Parallel 两个子用例）、NC-241（回调里切换：set_strategy / shutdown 事件顺序、延后切换的错误去向）修前失败 |
| [red-actionflow.txt](red-actionflow.txt) | NC-242（Update fn panic 后 Deferring 仍为真）、NC-243（延后 / 直接两条 Start 路径）修前失败 |
| [red-hotcode.txt](red-hotcode.txt) | NC-246、NC-247 修前失败；`ResolveMismatches` 是新字段，跑红时把这一条断言临时替换掉，只取 Resolve 返回值这一处红 |
| [red-plugin-244.txt](red-plugin-244.txt) | 真实 .so：接口变量导出的 PatchBundle 被拒（NC-244）；当时控制用例名为 `TestLoadPluginAppliesARealSharedObject` |
| [red-plugin-245.txt](red-plugin-245.txt) | 只修 NC-244 之后：部分应用的插件补丁留在原处（NC-245） |
| [green.txt](green.txt) | 修后上述用例与相邻既有用例通过 |

复跑：`GOWORK=off go test -count=1 -v ./hotcode/plugintest`（需要 PATH 上与测试同版本的 go、CGO_ENABLED=1；否则 Skip 并说明原因）。修后另跑了 `-race -count=3 ./ai ./actionflow ./hotcode/...`、`./nest`、全仓 build / vet、根包。
