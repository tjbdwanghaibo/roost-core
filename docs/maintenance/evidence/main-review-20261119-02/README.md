# main 第二次续审的复跑说明

基线为 `ba1585356f39bdffc2bdecd864d1dc4aea4bd7e3`，运行测试的提交为 `640996a3e30f4aecfb891607f4010232b438ea4f`。收尾提交 `1397ee400daea6bd8b2a3776d5cab6734ec50488` 仅改发布说明。Linux amd64，Go 1.27.2，.NET SDK 10.0.402，protoc 31.1，`GOWORK=off`。报告分支仅存证。下面的 `OLD`、`NEW`、`E`、`OUT` 分别代表基线、被测检出、此证据目录与空的临时输出目录的绝对路径。

## Mongo 工程名

从两份源码各构建自己的 CLI，再用新版本 Mongo 包读取新 CLI 生成的配置。探针只取数据库句柄名称，底层是内存客户端。输出中 baseline 的两行是旧配置里的库名和映射，main 另列新包装器返回的两个物理名称。[结果](comparison.log)

```bash
(cd "$OLD" && go build -o "$OUT/old-roost" ./codegen/cmd/roost)
(cd "$NEW" && go build -o "$OUT/new-roost" ./codegen/cmd/roost)
cp "$E/database_names.go.txt" "$OUT/database_names.go"
(cd "$NEW" && go build -o "$OUT/read-names" "$OUT/database_names.go")
python3 "$E/compare_names.py" "$OUT/old-roost" "$OUT/new-roost" "$OUT/read-names"
```

## 多来源的组成员视图

测试使用两个版本共有的仓内 transport 和实体夹具，按实际 Flush 次序记录发往成员 2 的最后一个对象视图，同时核对 GroupView 自己是否持有订阅。两种上限都应得到 room。预期本轮实现仅在上限为 1 时断言失败。两次测试均使用各自检出的 Manager 和 GroupView。[基线三轮](nov19-group.log) · [被测版本三轮](r2src-group.log)

```bash
for tree in "$OLD" "$NEW"; do
  python3 - "$tree" "$E" "$OUT" <<'PY'
import json, pathlib, sys
tree, evidence, out = map(pathlib.Path, sys.argv[1:])
target = tree / "framework/sync/entitysync/policy/review_group_profile_test.go"
(out / "overlay.json").write_text(json.dumps({"Replace": {str(target): str(evidence / "group_profile_test.go.txt")}}))
PY
  (cd "$tree" && go test -overlay "$OUT/overlay.json" -count=3 -v -run '^TestReviewGroupMemberPromotion$' ./framework/sync/entitysync/policy)
done
```

## 既有问题与功能覆盖

- Unity 运行 `python3 scripts/client/build-unity.py --managed-only --target linux --output "$OUT/package"`，按[原探针说明](../main-review-20261119/README.md)运行包内 DLL 和 BrowserWebSocket.cs。然后从被测树执行 `dotnet run --project client/physics-viewer/tests/Prediction.Tests.csproj`。[打包、两组取消及 viewer 日志](unity.log)
- 用被测 CLI 的 `project new review -module example.com/review2 -template game -out game -skip-deps` 建工程，在工程内运行 `go mod edit -replace github.com/tjbdwanghaibo/roost-core="$NEW"` 并安装 portal、login、enter、gift、mailbox、rehome、activity。执行 `roost module verify --json`。19 组共 293 项通过，跳过 0 项，另有 build、vet、ID 与 generate 检查。[逐组计数](modules.json)
- 在上述工程的临时 account 配置副本追加 `portal.websocket_servers`，键为字符串 1，地址形式为 `ws://<host>:7080/ws`。运行 `roost config check --service account --file <副本> --json`。日志保留每个 host、退出码和进程检查结果。[portal](portal.log)
- httpserver 与 account 的相关包测试使用 `go test -count=1 -run 'Origin|CORS|Browser|WebSocketAddr' ./infra/network/httpserver ./service/account`。前导零端口与 TS Gate 沿用[原探针](../v156-main-review-20261118/README.md)，分别由本次 Go/CORS 和 TS SDK 执行。SDK 脚本选择用 `python3 scripts/pretag_scope_test.py`。[Origin 与 pretag](origin.log) · [TS 取票](ts.log)
- `go test -count=1 ./gameplay/level ./gameplay/physics ./service/physicsroom ./wiring/physicsroom ./framework/sync/entitysync/... ./wiring/mongo ./wiring/dataengine ./wiring/sidmerge ./service/mail ./infra/base/clock` 运行默认构建条件下的测试。[11 个包](core.log)
- `go test -count=1 -v -run '^(TestDemoLeavesMailToTheMailboxModule|TestModuleGiftStandalone|TestDAOSkeletonDatabaseFollowsTheProjectName|TestGeneratedMailboxCutoverAfterLoginPullFailure)$' ./codegen/internal/roost` 验证 demo 邮件接线、独立 gift、DAO 生成和邮箱切换。[四项结果](codegen.log)
