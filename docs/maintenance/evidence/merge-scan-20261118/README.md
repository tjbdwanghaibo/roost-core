# 合服预检读在途索引的快照（RV-20261118-05）

基线：main `8a2cf3a9`。审查来源：审查分支 fix/core-game-server-review 上的 `docs/maintenance/MAIN-REVIEW-20261118.md` 05 条与探针 `review_merge_scan_test.go.txt`（该分支未合入 main）。

## 问题

`MergeServer` 的前置检查（被并走的服不能是在途换服的来源或目标）经 `transfersInvolving` → `scanTransfers` 读在途换服索引。
修复前 `scanTransfers` 按 `offset` 每页 200 条翻页（内存 `MemoryRoles.TransfersDue` 与 Redis `IndexPage` 都是按偏移取）。
读完第一页之后有一笔换服了结（中止 / 提交 / 期限清扫），它从索引里移出，后面的条目前移一位；下一页仍从偏移 200 读，
原第 201 条正好被跳过。它若以被并走的服为目标，合服就写下 `MergedInto`，而那笔换服始终在途。

## 修复

[transfer_index.go](../../../../service/account/transfer_index.go)：`scanTransfers` 只调用一次
`TransfersDue(ctx, math.MaxInt64, 0, math.MaxInt)`，读出索引在同一时刻的全集，再逐条读角色记录核对。
一次调用是同一时刻的读：内存实现在同一把锁下取，Redis 实现是一条 `ZRANGEBYSCORE`（`TransferIndex` 接口注释写明了这条约定，
并说明跨调用按 offset 翻页不是快照）。读之后了结的按角色记录剔除；读之后才发起的不在其中，仍由提交时的检查
（目标服已并走则拒绝）与期限中止兜底（[merge.go](../../../../service/account/merge.go) 的注释与
[PLAYER-MIGRATION.md](../../../framework/PLAYER-MIGRATION.md) 合服一节同步）。`ListRoleTransfers` 走同一个函数，同样不再漏列。

为什么不用“变更计数前后不一致就重扫”：要给内存与 Redis 两种索引都加计数（Redis 要改 versionstore 的写脚本），
换服频繁时还要重试或拒绝；一次读全集更简单，正常情况下读索引的次数从 ⌈N/200⌉ 降为 1，角色记录逐条读与以前相同。
代价是索引异常膨胀（大量换服没有了结）时一次回复很大；那时清扫没在跑，本身就要先处理。

## 回归用例

[merge_test.go](../../../../service/account/merge_test.go) `TestAMergeCheckSeesEveryTransferWhileOthersSettle`：201 笔在途换服按期限递增，
前 200 笔去承载服、最后一笔以被并走的服为目标；包一层角色存储，第一次读索引返回之后中止期限最早的那笔。断言合服答
`ErrTransferInProgress` 且服 2 未写 `MergedInto`；之后同样手法检查 `ListRoleTransfers` 仍列出那笔。用例断言读索引的钩子确实触发过。
默认走 Memory；`ROOST_REVIEW3_BACKEND=redis ROOST_REVIEW_REDIS=<addr>` 时走真实 Redis（`redisRoles`，期限逐笔递增，避免同分时
Redis 按成员字符串排序带来的顺序差异）。

## 红（基线代码，只加用例）

```
$ GOWORK=off go test -count=1 -run TestAMergeCheckSeesEveryTransferWhileOthersSettle -v ./service/account
    merge_test.go:530: merge with a transfer onto 2 in flight the whole time = <nil>, want ErrTransferInProgress
    merge_test.go:533: the merge check missed the transfer onto 2 and wrote the merge: {ID:2 Name: Status:closed ... MergedInto:1 ...}
    merge_test.go:552: listing while another transfer settles misses player 1000201 (199 listed)
--- FAIL: TestAMergeCheckSeesEveryTransferWhileOthersSettle (0.00s)

$ source ~/.roost-it/roost-dataengine-it/env.sh
$ ROOST_REVIEW3_BACKEND=redis ROOST_REVIEW_REDIS=$ROOST_DATAENGINE_IT_REDIS_ADDR GOWORK=off go test -count=1 -run TestAMergeCheckSeesEveryTransferWhileOthersSettle -v ./service/account
    merge_test.go:530: merge with a transfer onto 2 in flight the whole time = <nil>, want ErrTransferInProgress
    merge_test.go:533: the merge check missed the transfer onto 2 and wrote the merge: {ID:2 Name: Status:closed ... MergedInto:1 ...}
    merge_test.go:552: listing while another transfer settles misses player 1000201 (199 listed)
--- FAIL: TestAMergeCheckSeesEveryTransferWhileOthersSettle (0.24s)
```

审查探针放进包里在基线上同样红：`review_merge_scan_test.go:61: merge = <nil>, server 2 merged into 1 while transfer to 2 remains`
（`stable` 过、`shrink` 红）。修复后探针的钩子挂在 `offset == 200` 的第二页上，不再有第二页，探针恒过、不再证明什么，
以本用例为准。

## 绿

```
$ GOWORK=off go test -count=1 -run TestAMergeCheckSeesEveryTransferWhileOthersSettle -v ./service/account
--- PASS: TestAMergeCheckSeesEveryTransferWhileOthersSettle (0.00s)
$ ROOST_REVIEW3_BACKEND=redis ... go test ...（同上）
--- PASS: TestAMergeCheckSeesEveryTransferWhileOthersSettle (0.87s)

$ GOWORK=off go test -race -count=1 ./service/account                      → ok (1.8s)
$ ROOST_REVIEW3_BACKEND=redis ROOST_REVIEW_REDIS=... go test -race -count=1 ./service/account → ok (26.6s)
$ GOWORK=off go test -race -count=3 -run '...合服与列举相关四条...' ./service/account → ok
$ REDIS_ADDR=... go test -tags integration -race -count=1 -run 'TestAccountTransferDeadlinesAndIndexOnRedis|TestAccountRoleTransferRunsOnRedis' ./wiring/integration → ok
$ GOWORK=off go test -race -count=1 ./wiring/account ./wiring/gate → ok
$ GOWORK=off go build ./... ; go vet ./service/account ; go test -count=1 . → ok
```

## 变异

| 变异 | 结果 |
| --- | --- |
| 恢复基线的按 offset 每页 200 条翻页 | 红（即上面的“红”） |
| 仍一次读，但上限 200：`TransfersDue(ctx, math.MaxInt64, 0, 200)` | 红：合服放行、服 2 写成 `MergedInto:1`，列举 199 条漏 `1000201` |

## 没有覆盖的

- 单次 `TransfersDue` 调用的原子性靠实现（内存一把锁、Redis 一条命令），用例无法在一次调用内部插入了结；把 Redis 实现改成内部分页的变异，
  本用例抓不到。接口注释写明了这条约定。
- 读索引之后才发起的换服仍不在快照里（已记载的窗口），由提交时拒绝与期限中止兜底，本次未改。
- Redis Cluster 上未复跑（单机 Redis 已跑；索引是单个键，一条命令在 Cluster 上同样原子）。
