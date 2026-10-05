# N02 续审证据：响应完整性、接入停机与容量

2026-10-05，macOS/arm64，Go 1.27.0，`GOWORK=off`。基线 `50e9a4e853641f348c879b85c282af9c5813f656`（origin/main），修复在分支 `revn02`。日志已去掉 panic 堆栈与 listener INFO 行，绝对路径为执行时 scratch 目录。

## 文件

| 文件 | 内容 |
| --- | --- |
| `webroute_review_test.go.txt` | overlay：不可编码响应 5 形状、字节形状控制、响应开始后 panic、ErrAbortHandler、响应前 panic 控制 |
| `ops_review_test.go.txt` | overlay：正式 Ops Init→Provide→Start，NaN 结果；不配合 ctx 的命令 Stop 超时→重试排空（控制） |
| `consumer-handlers.go.txt` / `consumer_test.go.txt` | 正式 `codegen/cmd/webroute` 生成的独立 module：`//roost:web` → RegisterModules → Engine → 真实 HTTP → httpclient |
| `tcp_review_test.go.txt` / `auth_review_test.go.txt` | 放进 `-template game-demo` 生成工程 `internal/access/player/tcp` 的 overlay：认证回调不配合时的握手/连接容量、Stop/Mod 重试、per-IP 与请求大小、阻塞订阅者、只改 max_connections 的配置观察；真实 account.Service 签票据→TCP 握手→demo 认证器→Dispatch 的业务鉴权全链 |
| `gateway_review_test.go.txt` | overlay：单主体占满 key 表（NC-82）、满表陌生 key 成本 benchmark、Timeout 遇不配合 endpoint（控制） |
| `red-overlay-*.txt` / `red-generated-consumer.txt` | 修前 overlay 红 |
| `red-formal-*-oldmain.txt` | 本轮正式回归放到旧产品（origin/main detached worktree / 旧生成器 server_gen.go）上的红 |
| `green-*.txt` | 修后同一 overlay 与正式回归 |
| `observation-gateway-ratelimit.txt` | NC-82 观察与 benchmark（未修） |

## 结果摘要

| 场景 | 修前 | 修后 |
| --- | --- | --- |
| NC-80 overlay 5 形状 + 生成消费者 + Ops | 7 红（200 空体 / httpclient nil） | 7 绿 |
| NC-80 正式（httpserver 5 叶子、webroute 1、ops 1） | 7 红 | 7 绿 |
| NC-81 正式 4 叶子（3 形状 + ErrAbortHandler） | 4 红 | 4 绿 |
| NC-83 overlay 3（Stop 重试、Mod 重试、阻塞订阅者） | 2 红 + 1 观察（1s 未返回） | 3 绿（100ms 内返回 ctx 错误） |
| NC-83 正式生成测试 3 | 3 红（旧 server_gen.go） | 3 绿 |
| 控制：字节形状、响应前 panic、1xx、Hijack/Flush/ResponseController、Hijack 后 panic、Ops 不配合命令重试排空、握手/连接容量、per-IP/超限帧、业务鉴权全链 7 个拒绝形状 + 过期 | 绿 | 绿 |
| NC-82 观察 | 受害者首请求 ErrRateLimited；满表陌生 key 20µs（1k）/ 0.52ms（100k） | 未修 |
| 配置观察：只设 max_connections ≤1000 | 启动拒绝，错误不点名字段 | 未改（doctor 要求全部键，非 RR） |

## 本地矩阵（修后）

- `gofmt -l` 改动目录为空；`go vet ./httpserver ./webroute ./kit/ops ./codegen/internal/roost` 通过。
- `go test -race -count=3 ./httpserver ./webroute ./kit/ops ./gateway ./security ./httpclient`：438 个 test pass 事件，0 fail/skip。
- 根包 `go test -count=1 .` ok；`go build ./... && go vet ./...` 通过；`go test -count=1 ./codegen/...` 全部 ok。
- game-demo（`roost project new n02demo -template game-demo -skip-deps`，replace 指向 worktree）：`go build ./... && go vet ./...` 通过；`go test -count=1 ./...` 全部 ok（跳过配置观察用例）；`internal/access/player/tcp` race×3 117 pass。
- 生成消费者 module：修后 `TestGeneratedRouteUnencodableResult` pass。

计数只对应本次命令，不与其他轮次相加，不代表覆盖率。

## 复跑

```sh
W=<worktree>; export GOWORK=off
cp docs/bugfix/evidence/noncore-bugfix-20261005-n02/webroute_review_test.go.txt $W/webroute/zz_review_n02_test.go   # 复跑后删除
go test -count=1 -run ReviewN02 -v ./webroute
go build -o /tmp/roost ./codegen/cmd/roost
/tmp/roost project new n02demo -module example.com/n02demo -out <dir> -template game-demo -skip-deps
(cd <dir> && go mod edit -replace github.com/tjbdwanghaibo/roost-core=$W && go mod tidy && go test -count=1 ./internal/access/player/tcp/)
```

没有使用 Redis/Mongo/NATS 等外部依赖，也没有写共享库/流；所有 listener 是随机回环端口，只关闭自己创建的对象。

## 合并前复核（2026-10-05，分支 n02final）

rebase 到 origin/main（`45d4bc1c`）之后，改进与 NC-82 修复先在 N02 提交（`149f0d18` 树，httpserver/security/gateway 与 rebase 后相同）上跑红，再实施跑绿。

| 文件 | 内容 |
| --- | --- |
| `red-httpserver-refine.txt` / `green-httpserver-refine.txt` | NC-80 分配上界（`TestJSONDoesNotCopyTheEncodedBodyPerResponse`）、NC-81 FlushError 两叶子 |
| `red-nc82.txt` / `green-nc82.txt` | NC-82 三个修前红（security 2、gateway 1）与修后 4 个用例 |
| `bench-json.txt` / `json_bench_test.go.txt` | 修前 Encoder / N02 Marshal / 采用的推迟写状态，三者同一二进制 `-count 6` |
| `bench-ratelimit-before.txt` / `bench-ratelimit-after.txt` | 满表陌生 key：修前 5.7µs（1k）/ 529µs（100k），修后 <0.11µs |

合并结果上的本地矩阵（HEAD `eef7822e`，工作树干净，`GOWORK=off`）：

- `gofmt -l`（全部受控 .go）为空；`go vet ./httpserver ./webroute ./kit/ops ./gateway ./security ./httpclient ./codegen/internal/roost` 通过。
- `go test -race -count=3 ./httpserver ./webroute ./kit/ops ./gateway ./security ./httpclient`：459 个 test pass 事件，0 fail/skip（`TestJSONDoesNotCopyTheEncodedBodyPerResponse` 为 `!race`，不在其中；非 race 下单独通过）。
- 根包 `go test -count=1 .` ok（文档提交后复跑仍 ok）；`go test -count=1 ./codegen/...` 15 包 ok；`go generate ./...` 后 porcelain 0 行；`go build ./... && go vet ./...` 通过。
- game-demo（`roost project new n02demo -module example.com/n02demo -template game-demo -skip-deps`，`go mod edit -replace` 指向 worktree，`go mod tidy`）：生成的 `server_gen.go` / `server_gen_test.go` 含 NC-83 修复；`go build ./... && go vet ./...` 通过；`go test -count=1 ./...` 18 包 ok、0 fail；`internal/access/player/tcp` race×3 99 pass、0 fail（本次只跑正式生成测试，未放 overlay，故少于续审时的 117）。

计数只对应本次命令。

