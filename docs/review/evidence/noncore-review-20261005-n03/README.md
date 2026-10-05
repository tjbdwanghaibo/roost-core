# N03 审查证据（2026-10-05，revn03）

基线 `be7bcc18`（N03 源码与 `7922e428` 相同）。macOS / Go（go.mod 1.27.0），nats.go v1.53.1，etcd client v3.7.1，etcd / nats-server 来自 Homebrew。

| 文件 | 内容 |
| --- | --- |
| [probe-nats-baseline.txt](probe-nats-baseline.txt) | 真实 NATS 探针，修复前一次完整运行（INFO 行已去掉）：P1 在途停止（NC-90 红）、P2～P6 settle 矩阵、P7 broker 重启、P9 满队列（NC-91）、P10 发现调用期限、P11 轻量调用进 JetStream 主题（NC-92） |
| [probe-etcd-baseline.txt](probe-etcd-baseline.txt) | 真实 etcd 探针：E1 冻结 etcd 时 Resign（NC-93 红）、E1b 正常 Resign 服务端清理、E2 冻结时 Deregister、E3 Assembly.Close 两次、E4 lease 消失后重注册、E5 断网期间压缩后 Mirror 恢复 |
| [probe-bus-dlq-baseline.txt](probe-bus-dlq-baseline.txt) | 包内探针：被拒绝的 RPC 进入死信、重放形状（NC-91） |
| [probe_kit_nats.go.txt](probe_kit_nats.go.txt) / [probe_etcd_driver.go.txt](probe_etcd_driver.go.txt) / [probe_bus.go.txt](probe_bus.go.txt) | 探针源码（运行时放在对应包下的 `zz_probe_revn03_test.go`，跑完删除，不提交） |
| [own-nats-server.sh.txt](own-nats-server.sh.txt) | P7 用的自起 nats-server（15322，临时 store，按控制文件重启） |

## 环境与清理

- 共享隔离 NATS（`~/.roost-it/roost-dataengine-it`，15222～15224）：只 source env.sh、不输出；主题前缀 `revn03p<纳秒>` / `revn03q<纳秒>`，流名 `REVN03_REQ_<纳秒>` / `REVN03_RESP_<纳秒>`，每个用例 Cleanup 删除自己的流；结束后按 `REVN03_` 前缀列出确认为空。
- P7 的 broker 重启只对自起的 nats-server（127.0.0.1:15322，store 在 scratchpad），不碰共享集群；用完停止并删除 store。
- etcd：每个用例自起单节点，随机端口（避开 2379 / 2380 / 23791），数据目录是 `t.TempDir()`，用例结束 kill 并删除；`SIGSTOP` 只发给自己起的进程，结束前 `SIGCONT`。

## 复跑

```
source ~/.roost-it/roost-dataengine-it/env.sh   # 只 source
cp probe_kit_nats.go.txt   <repo>/kit/nats/zz_probe_revn03_test.go
cp probe_etcd_driver.go.txt <repo>/etcd/driver/zz_probe_revn03_test.go
cp probe_bus.go.txt        <repo>/bus/zz_probe_revn03_test.go
GOWORK=off go test -tags integration -count=1 -run TestRevn03Probe -v ./kit/nats/ ./etcd/driver/
GOWORK=off go test -count=1 -run TestRevn03Probe -v ./bus/
# P7 另需：bash own-nats-server.sh 后设 REVN03_NATS_URL=nats://127.0.0.1:15322 REVN03_NATS_RESTART=<dir>/ctl
```
