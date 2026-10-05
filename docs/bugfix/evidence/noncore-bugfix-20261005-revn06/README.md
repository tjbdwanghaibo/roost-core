# 证据：N06 S1/S2/S3/S6 复核（2026-10-05，分支 revn06）

基线 `50e9a4e8`（fetch/pull 后 main HEAD）。本机 Go 1.27.0、darwin/arm64；隔离环境 `~/.roost-it/roost-dataengine-it`（端口偏移 1000，只 source、不输出 env）。本 agent 的 Redis 键前缀 `revn06:`，每个用例结束删除；收尾 `--scan --pattern 'revn06:*'` 为 0。原始输出在本机 scratchpad（不入 Git），下面是摘录。

## 原红（产品文件还原为基线、测试为最终版本）

```text
# RR-20261001-06 残余 + NC-50（Memory 与真实 Redis 同文）
pending_creation_other_name_promises_test.go:139: account store:revn06-loser cannot create a role under another name after its pending name was committed elsewhere: account: role limit reached for this server: another name is pending on server 1
--- PASS: TestADifferentNameKeepsAPlanThatCanStillComplete   （3 个控制子用例）
pending_creation_other_name_promises_test.go:250: the different-name create never tried to release the dead plan
pending_creation_other_name_promises_test.go:287: a failed compensation counted 0 times, want 1; accepted:create_role=1, accepted:login=2, refused:create_role:name_taken=1

# NC-51（Memory）
confirmed_key_ownership_promises_test.go:59: group-a's sweep completed [{Key:group-a/local/close ...} {Key:group-b/foreign/close ... Status:complete ... CompletionReason:grace_expired ...}], want only its own group-a/local/close
confirmed_key_ownership_promises_test.go:109: tick 1: the invalid entry was acted on (read as an activity, pruned or moved) instead of skipped and kept: {GroupID:group-a Keys:[] Opening:[] ScanAfter:<nil> Delivering:[group-a/local/close] RefusedOpens:0}

# NC-52（fake Redis，确定性）
retry_freshness_promises_test.go:57: Update after 8 attempts (8 backoffs, 9 competitor writes) failed: versionstore: version conflict: nc52:k after 8 attempts
retry_freshness_promises_test.go:109: mutate saw found=[true true false]; want present then absent (deleted during the backoff)

# NC-52 在 chat 组合上的表现（真实 Redis，-race -count=3，3/3 失败）
replica_prune_paging_promises_test.go:137: append k-101-0: versionstore: version conflict: revn06:chat:<纳秒>:world:7 after 8 attempts
```

## 修后

| 命令（GOWORK=off，模块根） | 结果 |
| --- | --- |
| `go test -race -count=3 ./versionstore/ ./kit/service/account/ ./kit/service/global/activity/ ./kit/service/chat/` | 4 包 ok |
| `go test -race -count=1 ./kit/service/... ./service/... ./redis/...` | 17 包 ok、0 fail |
| `ROOST_REVIEW3_BACKEND=redis ROOST_REVIEW_REDIS=<隔离> go test -race -run 'TestADifferentName\|TestAFailedRelease' ./kit/service/account/` | 4 顶层 / 3 子用例全 PASS |
| `ROOST_BUGFIX5_BACKEND=redis ROOST_REVIEW_REDIS=<隔离> go test -race -count=3 -run TestSkewedReplicas ./kit/service/chat/`（重复 3 轮） | 9/9 PASS |
| `REDIS_ADDR=<隔离> go test -tags integration -race -run TestIntegrationSendRecovery ./service/mail/` | 4 子用例 PASS（Mail 恢复组合控制） |
| `REDIS_ADDR=<隔离> go test -tags integration -count=1 -p 1 -json ./versionstore/ ./service/... ./kit/service/...` | 891 pass / 23 skip / 0 fail；skip 均为 Cluster、`ROOST_REVIEW_CLUSTER` 等环境门；前后键扫描无新增 |
| `go test -count=1 .`（根包门禁） | ok（新用例只用 ci.yml 已设的 Redis 门变量） |
| `go build ./... && go vet ./...`、`gofmt -l`（改动目录） | 通过 / 空 |

真实 Redis 竞争探针（`zz_probe`，跑完即删）：20 次 Update/写者、`-race`，修前三轮 6 写者 0～2/120、8 写者 3～5/160 次 ErrConflict；同机“退避后重读”两轮 0/120、0/160。小样本，只证明方向。

未执行：Redis Cluster、真实 Mongo、跨进程同时建角、吞吐 / p99 前后对照；没有改生成形状，未重生成 game-demo。
