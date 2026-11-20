# 2026-11-20 第二轮审核证据

Linux amd64、Go 1.27.2。以 `SRC` 表示 `108ff00b8b19737d880c023ddd9435b4a92ccdb3` 的检出目录，`E` 表示本证据目录，`OUT` 是新建临时目录，三者均用绝对路径。生成时 protoc 需要在 PATH 中。

## 生成正式模板与运行原场景

```bash
export GOWORK=off GOFLAGS='-p=2 -mod=mod -buildvcs=false'
cd "$SRC"
go build -o "$OUT/roost" ./codegen/cmd/roost
"$OUT/roost" project new reviewdemo -module example.com/reviewdemo -out "$OUT/demo" -template game-demo --skip-deps
cd "$OUT/demo"
go mod edit -replace github.com/tjbdwanghaibo/roost-core="$SRC"
go test -count=1 ./internal/service/game ./game/controllers ./game/handler
go test -count=3 -v -run '^Test(OrdinaryTransferPlacementUsesTargetOnce|OrdinaryFirstPlacementWithoutSaveUsesSpawn|OrdinaryDrainReportsOnlyOwnCoordinateChanges)$' ./internal/service/game
```

## 关闭事件与排空归因

补充用例复用生成工程的原测试夹具，按 Go overlay 加载。下面的 Gate 测试在审核提交上应以 1 退出，其中 gate 子用例失败三轮，两个对照各通过三轮。随后单独执行排空探针。Gate 用例向生产回调投递当前玩家、会话、连接的关闭事件；它只模拟连接表和传输，不依赖真实套接字。等待 2.2 秒覆盖两个定期补查间隔。

```bash
cd "$OUT/demo"
python3 - "$PWD" "$E" "$OUT" <<'PY'
import json, sys
from pathlib import Path
root, evidence, out = map(Path, sys.argv[1:])
for name, target, source in [('gate', 'review_close_test.go', 'gate_close_test.go.txt'),
                             ('drain', 'review_drain_test.go', 'drain_test.go.txt')]:
    (out / (name + '.json')).write_text(json.dumps({'Replace': {
        str(root / 'internal/service/game' / target): str(evidence / source)}}))
PY
go test -count=3 -v -overlay "$OUT/gate.json" -run '^TestReviewPartitionCloseWithSecondLogin$' ./internal/service/game
```

```bash
cd "$OUT/demo"
export REVIEW_EVENTS="$OUT/events"
mkdir -p "$REVIEW_EVENTS"
go test -count=3 -v -overlay "$OUT/drain.json" -run '^TestReviewDrainEventAttribution$' ./internal/service/game
python3 "$E/check_events.py" "$SRC/scripts/test-demo-ordinary-drain.sh" "$REVIEW_EVENTS"
```

排空用例执行完整 begin/finish，按玩家统计提交载荷里的坐标，保存实际输出的 JSON 日志。检查器从被测 shell 脚本抽取其 Python 判定段，对两份日志分别判级；每次重复覆盖同名日志，判定段使用最后一轮的两份记录。[结果](drain.log)

## 其他检查

```bash
cd "$SRC"
cp "$E/aoi_compare.go.txt" "$OUT/aoi_compare.go"
go run "$OUT/aoi_compare.go"
go test -count=1 ./framework/sync/entitysync/policy ./infra/base/spatial ./gameplay/movement ./gameplay/cellworld ./infra/network/gateway
go test -count=1 -run 'Test.*(Completion|Pipelined)' ./framework/nest
go test -count=1 -run 'Test(DemoCellPlayerKeepsItsPublishedEntityKind|PatrolRouteUsedByDemoAndMonster|CoreDependencyBoundary)' . ./codegen/internal/roost
"$OUT/roost" project new explorer -module example.com/explorer -template game -recipe cellworld --skip-deps -out "$OUT/explorer"
cd "$OUT/explorer"
go mod edit -replace github.com/tjbdwanghaibo/roost-core="$SRC"
go test -count=1 ./game/modules/cellspace ./game/modules/cellworldhost ./game/modules/login ./internal/service/game
```

编译兼容性对照使用同一份生成的 reviewdemo。复制其 go.mod 和 go.sum 为临时 compat.mod 和 compat.sum，依次用 `go mod edit -modfile "$OUT/compat.mod" -replace github.com/tjbdwanghaibo/roost-core="$TREE"` 指定 v1.58.0、v1.59.0、被测 main 的检出目录。从 reviewdemo 目录运行 `go build -modfile "$OUT/compat.mod" ./game/modules/scene`。三组保留完整退出状态和诊断。[结果](compat.log)
