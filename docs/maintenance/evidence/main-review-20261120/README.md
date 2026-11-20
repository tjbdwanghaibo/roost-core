# v1.59.0 / main 验证记录

被测提交 `605bdba165b10d4fe591a2b3e55266e4d867e84f`。Linux amd64，Go 1.27.2，Node 24.22.1。下文的 `SRC` 指该提交的检出目录，`E` 指本证据目录，`OUT` 指新的临时目录，均使用绝对路径。生成器还需仓库要求的 protoc。生成工程位于 Git 工作树之外，构建时关闭了 VCS 版本戳。

## 生成工程和旧问题

```bash
export GOWORK=off
export GOFLAGS='-p=2 -mod=mod -buildvcs=false'
cd "$SRC"
go build -o "$OUT/roost" ./codegen/cmd/roost
cp "$E/../main-review-20261119-02/database_names.go.txt" "$OUT/database_names.go"
cp "$E/../main-review-20261119-04/sid_names.go.txt" "$OUT/sid_names.go"
go build -o "$OUT/read-names" "$OUT/database_names.go"
go build -o "$OUT/sid-names" "$OUT/sid_names.go"
python3 "$E/../main-review-20261119-04/reserved.py" "$OUT/roost" "$OUT/read-names"
"$OUT/roost" project new reviewdemo -module example.com/reviewdemo -out "$OUT/demo" -template game-demo --skip-deps
cd "$OUT/demo"
go mod edit -replace github.com/tjbdwanghaibo/roost-core="$SRC"
mkdir -p cmd/rehomedrill
sed -e 's|DRILL_MODULE|example.com/reviewdemo|g' -e 's/^func main()/func mergeMain()/' "$SRC/scripts/testdata/demo-drill/merge.go.txt" >cmd/rehomedrill/merge.go
sed 's|DRILL_MODULE|example.com/reviewdemo|g' "$SRC/scripts/testdata/demo-drill/rehome.go.txt" >cmd/rehomedrill/main.go
go build -o "$OUT/rehomedrill" ./cmd/rehomedrill
python3 "$E/snapshot.py" "$SRC" "$OUT/roost" "$OUT/rehomedrill" "$OUT/sid-names"
```

`reserved.py` 使用正式配置和 Mongo 句柄读取器。换服快照探针生成三个 game-demo，从正式脚本提取源／目标库初始化和快照函数，执行真实的 `player-db` 子命令；将 mongosh 的 JS 原样交给 Node，记录为玩家 7 查询的库，并与各自的配置句柄比对。这里只取数据库名，绑定查询为空操作。[保留名](reserved.log) · [换服快照](snapshot.log)

## 普通地图和本轮探针

在刚生成的工程内先运行仓库用例，再将本目录的补丁应用于该临时工程的测试文件。补丁沿用原工程的移动、Nest、DAO 恢复和保存位置记录器，仅扩展测试。

```bash
cd "$OUT/demo"
go test -p=1 -count=1 ./internal/service/game ./game/handler ./game/controllers ./game/modules/scene
git apply --unidiff-zero "$E/movement.patch"
go test -v -count=3 -run '^(TestReviewSavedOrigin|TestReviewDrainSaveAttribution|TestOrdinaryTransferPlacementUsesTargetOnce)$' ./internal/service/game
```

在被测版本，最后一条命令预期退出 1，仅 `TestReviewSavedOrigin` 的冷重连断言失败。官方非零坐标对照通过。归因探针用两笔分开的 Nest 事务确定保存先后；提前保存组在采样与排空之间调用与周期心跳相同的 `Map.Save`，排空组则让位置保持待写。记录器按玩家 ID 统计提交载荷中的坐标，分别与两种情况下的日志和最终坐标比对。首轮普通套件受临时磁盘不足影响，腾出构建缓存后，使用原始生成测试文件重新执行。[原始用例](demo.log) · [三轮探针](probes.log)

## 其他检查

- 在 `SRC` 执行 `go test -count=1 ./framework/saga ./framework/dataengine/engine ./infra/network/nats/...`，以及 `go test -count=1 -run 'Test.*(Scene|GeneratedLongProjectMongoHandles|ReservedProject|Demo.*Config|CoreDependencyBoundary)' . ./codegen/internal/roost`。赠礼关联测试执行 `python3 -m unittest discover -s scripts/testdata/demo-robot -p 'test_trace_gifts.py' -v`。[Go 包](packages.log) · [生成器](codegen.log) · [赠礼关联](trace.log)
- 复制 [Origin 探针](../v156-main-review-20261118/origin_probe.go.txt) 为临时 Go 文件，在 SRC 根目录编译。分别传配置地址 `https://play.example.com:00443`、`https://play.example.com`，两次浏览器 Origin 均为 `https://play.example.com`。再运行 `python3 scripts/pretag_scope_test.py` 及 `go test -count=1 -run 'Origin|CORS|Browser|WebSocketAddr|NumericHost' ./infra/network/httpserver ./service/account`。[检查结果](historical.log)
- 在 `SRC/client/ts` 执行 `npm ci`、`npm run build`、`./node_modules/.bin/tsc -p tsconfig.test.json`，将该目录作为参数用 Node 分别运行 [Gate 断开探针](../v156-main-review-20261118/cancel.mjs) 和 [回调探针](../main-review-20261119-03/callback.mjs)。在临时生成的 demo 中先执行 `go list -mod=mod -deps . >/dev/null`，补齐 skip-deps 生成物构建主程序所需的 go.sum 项，再执行 `python3 "$E/portal.py"`；它按四种主机名依次运行配置检查。[TS](gate.log) · [portal](portal.log)
