# N07 第一批证据（NC-60～63）

基线 `50e9a4e853641f348c879b85c282af9c5813f656`，本机 macOS / Go 1.27.0，`GOWORK=off`。路径中的本机临时目录替换为 `<scratch>` / `<tmp>`。[本轮记录](../../../review/REVIEW-2026-10-05-noncore-n07.md)。

| 文件 | 内容 |
| --- | --- |
| `nc60-red.txt` / `nc60-red-race.txt` | 修前 `TestSnapshotCopiesUnderTheContainerLock` 失败；`-race` 3 处 DATA RACE |
| `nc60-green-race.txt` | 修后 `./attribute` 全部用例 `-race -count=3` |
| `nc61-red.txt` | 生成 game-demo（replace 到基线）修前两叶子失败：回滚后 Base 保留升级 |
| `nc61-red-persisted-consequence.txt` | 去掉前两条断言的同一用例：回滚后再升一级，`level 1 stored base HP 120, want 110` |
| `nc61-green.txt` | 修后模板写回生成工程，`-race -count=3` 通过 |
| `nc62-red.txt` / `nc62-green.txt` | 生成器拒绝不可表示声明的 6 叶子修前全被接受 / 修后通过 |
| `gen65.txt` / `build65.txt` | 65 个字段、`max=100`：生成成功、`go vet` 报 `1 << 64` 溢出 |
| `nc63-red.txt` / `nc63-green.txt` | errcode 扫描 5 叶子修前全被接受 / 修后通过 |
| `event-probes.txt` | event 行为探针（未提交的临时测试）：重入退订、重入订阅、panic、并发、尾部引用 |
| `validate-1.txt` | 合并修复后的本地矩阵（见下） |

float 截断的修前探针（临时模块，当前生成器 + 运行时文件，未入库）输出：

```
probe_test.go:11: rate=0.15 GetAttr=0 export=map[1:0] reload=0
probe_test.go:13: SetAttr hp 1<<32+5 changed=true hp=5
```

第二行是 int32 字段的窄化回绕，属于文档写明的“生成 setter 转换”，只作观察。

## validate-1 摘要

gofmt 空；改动包 vet 通过；`attribute`、`codegen/internal/attribute`、`codegen/internal/errcode`、`event`、`configdata`、`kit/configdata`、`errcode`、`servicerpc` `-race -count=3` 通过；根包 `go test -count=1 .` 通过；`go build ./... && go vet ./...` 通过；`go test -count=1 ./codegen/...` 无失败；`codegen/scripts/attribute-runtime.sh` 通过；全新生成 game-demo（replace 到本分支）tidy / build / vet 通过，`game/entities/player`、`game/gameplay/attribute`、`game/handler` `-race` 通过；对该工程跑 errcode 生成器导出 21 条定义（CSV 22 行含表头）。

`event-probes.txt` 里的 DATA RACE 来自探针自己的计数器：没有 AsyncDispatcher 时 `EventUnit.Publish` 在发布者 goroutine 上同步调用**其他**订阅者的 `SyncHandleEvent`，这是本轮记录里的观察 E-O2，不是测试环境问题。
