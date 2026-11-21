# 2026-11-21 续审证据

Go 1.27.2。准备两个只读检出。令 `MAIN` 指向 `108ff00b8b19737d880c023ddd9435b4a92ccdb3`，`TAG` 指向 v1.59.0 的 `605bdba165b10d4fe591a2b3e55266e4d867e84f`。在报告的本目录执行下列命令。每次构建都从相应检出解析依赖。

```bash
export GOWORK=off
OUT=$(mktemp -d)
cp seam.go.txt "$OUT/seam.go"
cp modes.go.txt "$OUT/modes.go"
(cd "$MAIN" && go build -o "$OUT/main-probe" "$OUT/seam.go" "$OUT/modes.go")
(cd "$TAG" && go build -o "$OUT/tag-probe" "$OUT/seam.go")
"$OUT/main-probe"
"$OUT/tag-probe"
```

探针逐步核对公开的 `Visible` 和 `Flush`，每个不符的断言输出 `pass=false`，执行完所有布局后以 1 退出。两版各执行三次，同版逐字节输出一致，保存各一份。每行的事件字段依次为观察者 ID、目标 ID、事件类型和档位。类型 0 表示 Enter，1 表示 Leave。修复后应全部通过并以 0 退出。[main 输出](main.log) · [标签输出](tag.log)

在 main 检出运行 `GOWORK=off go test -count=3 ./framework/sync/entitysync/policy` 可复跑仓库测试。[包测试输出](upstream.log)

Gate 沿用上一轮固定 main 的 game-demo、同版 core 和 Go overlay。这次运行 `go test -mod=mod -buildvcs=false -overlay ../gate-overlay.json -run '^TestReviewPartitionCloseWithSecondLogin$' -count=1 -v ./internal/service/game`。生成和 overlay 的准备步骤见上轮说明。[上轮说明](../main-review-20261120-02/README.md) · [本次 Gate 输出](gate.log)
