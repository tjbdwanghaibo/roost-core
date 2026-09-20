# U-0263：owed 索引的键空间没有登记（C4，CI 发现，无 RR 编号）

## 问题

kit 的 `ci` 工作流在 `service-redis` 这一格红：

```text
fail  github.com/tjbdwanghaibo/roost-kit/service/integration
redis_test.go:703: key "itest:keyspace:…:activity:owed:group-a:7" does not fall under a
declared namespace; an unnamespaced key is one two stores can both claim
```

`TestPerPackageKeyNamespacesDoNotCollide` 扫描一次真实跑出来的 keyspace，要求每一个键都落在
**声明过的**命名空间下。U-0257（RR-20260919-10）加的按 (组, 游戏服) 的 owed 索引
（`<prefix>:owed:<group>:<sid>`）是一个新的命名空间，而我当时没有把它登记进去。

## 根因

C4：同一个事实写在两处——键的构造在 `service/global/activity/redis_store.go`，
键空间的清单在 `service/integration/redis_test.go`——加了前者没有加后者。
这个守卫存在的意义正是"新加一个存储就必须声明它的地盘"，它按设计生效了。

## 为什么本地没发现

CI 跑的是 `go test -tags integration`，我当时只跑了不带 tag 的 `go test ./...`。
带 tag 的那一组才会真的连 Redis 把每个包驱动一遍。这条记在这里，是因为它是**可重复的**遗漏：
kit 的改动要用 `REDIS_ADDR=… go test -tags integration -count=1 -p 1 ./service/...` 验一遍。

## 改动

`service/integration/redis_test.go` 的 `everyNamespace` 增加 `":activity:owed:"`，并注明它是什么、
为什么迟到。

守卫的另一半——"每个声明过的命名空间都必须真的被写过一次"——本来就成立：这个键是驱动跑出来的，
现在 26 个键对应 26 个命名空间。

## 证明

修前红（上面那段），修后：

```text
26 keys across 26 namespaces: map[… :activity:owed::1 …]
--- PASS: TestPerPackageKeyNamespacesDoNotCollide
```

`REDIS_ADDR=127.0.0.1:6379 go test -tags integration -count=1 -p 1 ./service/...` 全绿。

## 未做 / 边界

- 守卫只能证明"每个键都有主"，不能证明"两个主不会渲染出同一个键"——测试自己的注释已经写清楚了
  这个界限，本次没有扩展它。
