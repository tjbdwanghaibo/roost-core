# B10 / C2 证据（2026-10-06）

基线 `4f0bab75`，分支 `b10cfg`。[方案与实施](../../B10-C2-CONFIG-RULES-AND-RELOAD-VISIBILITY-2026-10-06.md)。

| 文件 | 内容 |
| --- | --- |
| `red.txt` | 修前三条回归的失败文本（tablegen 运行期门、同一份规则、热更失败 / 回滚可见），以及修前端到端现象的出处 |
| `e2e-reload.txt` | 修后：全新生成的 game-demo（replace 到本分支）在隔离环境起 global + game，经 ops `/admin/execute` 的 `gm.config.reload`：成功、删 required 列、低于 min、required 为 null、恢复；每步后的存活数、指标与 game 日志里的 reload 行（去掉了 `server_time_ms`） |

复跑：`GOWORK=off go run ./codegen/cmd/roost project new X -module example.com/X -out <scratch>/X -template game-demo`；`go mod edit -replace github.com/tjbdwanghaibo/roost-core=<worktree>`；`db/def` 的 `db=game` 改唯一名后用本分支编译的 `roost generate`；配置拷出工程目录，把 Mongo / NATS / Redis 指向隔离环境，`nats.prefix`、`syncbus.prefix`、effects / saga 的 subject / stream / durable、`remote_entity.snapshot_l2_key_prefix`、各 database、ops / advertise / TCP 端口、etcd 端点（自起私有 etcd）换成自己的值，日志与 WAL 目录放到工程外；在工程目录里先起 `global` 再起 `game`。
