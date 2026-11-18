# v1.54.0 审核复跑

在独立的 v1.54.0 检出执行。Go 1.27.2，Linux amd64，GOWORK=off。文件名以 .txt 结尾的是审核夹具。

- 席位检查使用原包的内存测试后端。把 lobby_leave_test.go.txt 复制为 service/lobby/review_v154_test.go，运行 `go test -race -count=3 -v -run 'TestReviewOrderedLeave/.*/memory' ./service/lobby`。
- WS 对照把 ws_control_test.go.txt 复制为 infra/network/wsconn/review_v154_test.go，运行 `go test -race -count=2 -v -run '^TestReviewWriteControl$' ./infra/network/wsconn`。v154-packages.log 保留未加入探针时的整包结果。
- 快照检查复用 TS 套件的本地测试辅助。在临时检出把 v154-e2e.ts.txt 追加到 client/ts/test/relay.test.ts，把 v154-snapshot.ts.txt 追加到 client/ts/test/host.test.ts。仓库根目录执行 `go test -c -o /tmp/v154-relaypeer ./client/relay`。进入 client/ts 后执行以下命令，预期真实接班人断言失败。v154-ts-original.log 来自原始测试代码。

```sh
npm ci
./node_modules/.bin/tsc -p tsconfig.test.json
ROOST_RELAYPEER_BIN=/tmp/v154-relaypeer node --test --test-name-pattern='review delayed snapshot' dist-test/test/relay.test.js
node --test --test-name-pattern='review snapshot owner epoch' dist-test/test/host.test.js
```

v154-config-account.log 为 `go test -count=1 -v -run 'ClientConfig|ModuleConfig|GameToken|RoleTransfer' ./codegen/internal/roost ./service/account`。v154-config-csharp.log 为配置导出 C# 独立复跑。v154-csharp-relay.log 为同版本 Roost.Client.Relay.Tests 的结果。
