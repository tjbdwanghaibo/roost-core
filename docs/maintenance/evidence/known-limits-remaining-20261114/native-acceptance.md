# 原生客户端补验：B-14 / B-15

基线 `feat/v147` 的 `086524b3`；2026-11-14，macOS arm64，Go 1.27.3，libprotoc 36.2。本批没有修改客户端或 Gate 运行逻辑。

## B-14：同一会话跨两个 Gate

入口：

```sh
GOWORK=off GOFLAGS=-p=2 ROOST_GATE_CPP=1 bash scripts/test-gate-client.sh
```

脚本先运行生成 game-demo 的 C++ PB / Nest / DAO / WAL / Sync 全量、增量、删除与重连消费者，再运行 `TestRealAcrossGatesALateOlderBindDoesNotDisplaceTheNewerConnection` 的 C++ 档（Go 服务端开启 race）。新增入口已登记到根包的 `optInEntries`。

两个 Gate 与 Game 在同一 Go 测试进程，各自使用真实 NATS 连接和真实 TCP 监听；C++ 客户端在独立进程。两次鉴权返回相同 PlayerID / ServerID / SessionID，使用不同票据。夹具等旧 Bind 进入 Game 鉴权后再允许新客户端连接 B；新连接 READY 且实际业务请求成功后，才放行 A 的旧 Bind。要求旧连接没有 READY、收到 CLOSED，且 B 再次完成业务请求。管道控制先后，1ms 的 Pump 轮询只负责消费事件，不承担握手排序。

首次完整脚本通过：生成消费者 56.64s，跨 Gate 场景 0.54s。测试时机器上有其他编译，不作为性能数字。

变异：临时将 `roost_connect` 的鉴权首包改为裸票据，去掉连接代号信封，业务票据不变。同一脚本的生成消费者仍通过，跨 Gate 场景失败：

```text
new-ready
native output "" (open=false), want "PASS C++ same-session cross-Gate late-bind"
native stderr: newer connection was closed
--- FAIL: TestRealAcrossGatesALateOlderBindDoesNotDisplaceTheNewerConnection
```

变异由运行器在 `finally` 中恢复，未保留在提交中；恢复后完整脚本再次通过（跨 Gate 场景 0.60s）。它证明验收能抓住旧连接覆盖新连接，而非只检查两个 TCP socket 能连上。根包真实入口登记检查通过，文档链接检查 359 个文件、0 处损坏。

## B-15：完整 Protobuf C++ runtime

```sh
GOWORK=off GOFLAGS=-p=2 ROOST_PROTOBUF_TESTS=ON bash scripts/test-client-cpp.sh
```

8/8 ctest 通过，总测试时间 8.26s：`network_experience`、`native_sequence`、`native_lockstep`、`native_weak`、`native_client`、`native_game`、`native_views`、`native_protobuf`。使用已经安装的 protoc，没有安装新工具。

## 最后文档批的回写

- B-14 / No.86 的“C++ 跨两个 Gate 同一 SessionID 未验证”删除；历史关闭写“2026-11-14：真实 TCP / NATS 下 C++ 跨 Gate 的旧握手迟到验收与去掉连接代号的反向变异通过，两个 Gate 在同一 Go 测试进程”。
- B-15 / No.87 的“未安装 protoc、PB ctest 未跑”删除；历史关闭写“2026-11-14：libprotoc 36.2，Protobuf 开关开启，8/8 ctest 通过”。
- 不把本批扩写为跨机、Linux、移动端或弱网长稳验收；E-2 的 200 轮弱网循环单独处理。

原始输出位于本机 `/tmp/roost-v147-native-protobuf.log`、`/tmp/roost-v147-native-gate-green.log`、`/tmp/roost-v147-native-gate-mutation.log`、`/tmp/roost-v147-native-gate-restored.log`。关键失败已摘录在本文，复跑入口在仓库内。
