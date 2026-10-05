# revn04 证据（N04：NC-100～102）

基线 `be7bcc18`，2026-10-05，本机 Go 1.27.0 / macOS。隔离环境 `~/.roost-it/roost-dataengine-it`（Mongo 8.0.28 副本集 28117～28119、Redis 8.10.1 17379、toxiproxy 19474）；每条用例自建随机端口 toxiproxy 代理并在结束时删除，不给共享 `redis` 代理加毒、不 `/reset`。库名 / 键前缀 `revn04_*`、`nc100:*`、`nc101_*`，用后已删除并核对无残留。凭据只 source 不输出。

| 文件 | 内容 |
| --- | --- |
| [nc100-driver-red.txt](nc100-driver-red.txt) | RESP 替身：一次 eval / evalsha 服务端执行 2 次（修前） |
| [nc100-versionstore-real-red.txt](nc100-versionstore-real-red.txt) / [green](nc100-versionstore-real-green.txt) | 真实 Redis：修前 `[hello msg-1 msg-1]/v3` 成功返回；修后返回 EOF、Redis 中 `[hello msg-1]/v2` |
| [nc101-real-mongo-red.txt](nc101-real-mongo-red.txt) / [green](nc101-real-mongo-green.txt) | 真实副本集：`transaction_timeout=2s` 修前 30.2s / 29.79s（看门狗恢复网络时刻），修后 2s / 2s；green 为整包 integration 输出（含既有用例） |
| [nc102-probe.txt](nc102-probe.txt) / [nc102-red.txt](nc102-red.txt) | 唯一索引 9 组真实 / 替身对照（3 组分歧）；修前 6 个子用例红 |
| [refhmap-ttl-probe.txt](refhmap-ttl-probe.txt) | 控制：RefHMap Patch 续期全部 4 个 hash，过期后不创建 |
| [redis-kill-restart-probe.txt](redis-kill-restart-probe.txt) | 控制：私有 AOF always 实例 kill -9 → 停机写报错 → 重启后版本延续 v3 |
| [run-real-mongo-consumer.sh](run-real-mongo-consumer.sh) + [realclient-test.go.txt](realclient-test.go.txt) | 第 22 轮正式生成消费者换真实副本集后端的复跑入口；本轮两次运行 34 个 pass 事件（28 叶子），0 fail |

其他执行（结果见各 bugfix 记录）：`REDIS_ADDR=127.0.0.1:17379 GOWORK=off go test -tags integration -count=1 -p 1 ./kit/service/... ./service/... ./versionstore/ ./cache/...` 1008 pass / 30 skip / 0 fail（原始 json 1MB，留在本机未提交）；`GOWORK=off go test -race -count=3 ./mongo/... ./redis/... ./versionstore/ ./cache/...`；mongotest 消费包 `./dataengine/engine ./kit/dataengine ./kit/saga ./nestwal ./remoteentity ./saga`；`GOWORK=off go build ./... && go vet ./...`；根包 `GOWORK=off go test -count=1 .`。

探针（`zz_probe_*`）跑完即删。未执行：Redis Cluster、mongos、kit/dataengine 真实集成套件（写 `ROOST_IT_EFFECTS_*` 流且含主节点切换）、`redis/driver/lock_toxic_integration_test.go`（会 `/reset` 共享 toxiproxy）。
