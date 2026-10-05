# N01 留项 + N14 O3 / O4（revn01b，2026-10-06）证据

基线 `c8ecbabb`，macOS，Go 按 `go.mod`，`GOWORK=off`。[本轮记录](../../../review/REVIEW-2026-10-06-n01b.md)。

| 文件 | 内容 |
| --- | --- |
| `app-red.txt` | NC-231（两个阶段）与 NC-232 修前失败原文；`TestRuntimeFailureThatStartsTheShutdownIsReturnedOnce` 是对照，修前即绿 |
| `ops-red.txt` | NC-230 与 Ops admin 期限（N02 O1）修前失败原文 |
| `kit-red.txt` | NC-233、NC-234 修前失败原文 |

修后命令与结果见本轮记录“执行”一节：改动包 `-race -count=3`、`go vet`、`./kit/...` 与根包、`go build ./... && go vet ./...` 全部通过。没有使用真实依赖。
