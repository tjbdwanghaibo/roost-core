# C7：遍历回调语义定为仓库级契约

2026-10-06，分支 `b7c7b3`。来由：[DECISIONS-PENDING C7](../review/DECISIONS-PENDING-2026-10-05.md)（第二轮决定：是），[N13 §5 方向判断](../review/REVIEW-2026-10-05-n13.md#5-方向判断)（NC-180 / 181 / 182 / 185 是同一类问题，O6 / O7）。

## 1. 契约

凡框架交给业务的 `Range`，**回调里可以读写同一个容器，返回 false 立即停止**。展开：

1. 回调里对同一容器调用 Get / Set / Delete / Clear / 嵌套 Range 不死锁、不 panic；
2. 回调返回 false 后 Range 立即返回，不再调用回调（跨桶 / 跨分片也一样）；
3. 不交出不存在的条目（零值键、已被清理的实体），同一个键至多交出一次；
4. 遍历开始时就在、期间没被删除的键恰好交出一次，值是交出时的值；
5. 期间被删除的、还没走到的键：快照实现可能仍交出，活遍历实现不交出；回调里新增的键是否交出不承诺。

写进了 [roost-coding](../agent-skills/roost-coding/SKILL.md)（执行契约“生命周期与装配的复审要点”之后）、[根 README §16](../../README.md#16-单所有者组件清单非并发安全靠实体锁单-worker-独占) 与 `safemap` 包注释。

## 2. 共用契约测试辅助

`internal/rangecontract`（仿 `internal/stopcontract`，只给测试用）：`Check(t, Subject)` 对一个遍历入口跑五组用例——回调里读（Get、未命中 Get、嵌套 Range）、回调里写（改刚交出的键、新增键，FastMap 在这里扩容重排）、回调里删（删刚交出的键和它的伙伴，伙伴可能已走过也可能还没到）、回调里 Clear、在第 1 / 第 3 次回调里改容器并返回 false。每次 Range 在独立 goroutine 里有界等待，回调自锁以“Range did not return”失败而不是拖到包超时。`Subject.Live` 区分快照 / 活遍历，`KeysOnly` 给值由键决定的实体管理器，`Tx` 给需要在 Nest 事务里标脏的生成 DAO。

## 3. 逐个核对

| 包 | 遍历入口 | 语义 | 结果 |
| --- | --- | --- | --- |
| container | `BucketHolder.RangeAll`（1 桶 / 8 桶）、`RangeWithCursor`、`RangeByCursor`、`Bucket.Range` | 快照（桶读锁内复制，NC-180） | 符合 |
| container | `BucketHolder.RangeWithCursorCnt` | 快照 | **不符合 → 已修**（§4.1） |
| container | `KeyMap.Range` | 每桶快照（NC-185） | 符合 |
| safemap | `FastMap.Range` | 活遍历（NC-182） | 符合 |
| safemap | `SmallSafeMap.Range`、`ShardedSafeMap.Range` | 快照 | 符合 |
| safemap | `ShardedSafeMap.Read` / `Compute`（O6） | 持分片锁调用回调，不是遍历 | 不适用；补注释写明回调不能访问同一个 map，**未改**（NC-180 复审 `815c3661` 已让实体遍历持引用，那一处不动） |
| entity | `EntityManager.Range`、`RangeByCategory`、`ManagerAccess.Range` | 桶快照 + 持引用（NC-180 复审） | 符合 |
| entity | `EntityManager.RangeGroupEntities` | 组索引快照 | **不符合 → 已修**（§4.2） |
| 生成 DAO | `RangeX`：默认（SmallSafeMap）、`map=fast`、`map=sharded`（O7） | 委托字段的 map | 符合（Small / Sharded 快照、Fast 活遍历，均满足契约；差别写进 README §16） |
| index | — | 没有回调遍历，`Query` 返回副本 | 不适用 |
| lock | — | `LockManager` 没有遍历 API | 不适用 |

范围外顺带看过、未加用例：`spatial.BlockIndex.RangeBlocks`（逐块读锁内复制、锁外回调、false 停止，符合）；`nest.EntityLockGroupScope.Range`（`GetGroupEntities` 快照并按 `GroupLockID` 过滤，持组锁使用，不在本次范围）；`entity.DaoManager.RangeDao`（回调无返回值、单所有者 Go map，不是交给业务的容器遍历）。

## 4. 发现并修复（先红后绿）

两处都是既有 RR 的同根因残余，按 roost-bugfix §4 记为“复核后的补修”，不新编号。

### 4.1 `RangeWithCursorCnt` 回调里再做游标遍历会让外层重走同一个桶（NC-181 残余）

`container/bucket.go`：外层每个桶都现读 `h.RangeCursor`，回调里的游标遍历推进了同一个游标，外层随后重走同一个桶，同一条目交出两次（违反第 3 条）。修法：进入时按游标定下要走的桶（`start+i`），游标仍逐桶累加（回调里推进的保留）。

修前红：

```
--- FAIL: TestContainerRangeContract/BucketHolder.RangeWithCursorCnt/read_inside_the_callback
    rangecontract.go:160: Range handed out key 8 twice
```

### 4.2 `RangeGroupEntities` 交出已被清零的实体（NC-180 残余）

`entity/entity_manager.go`：组索引快照不持引用，回调里 Destroy 同组还没走到的实体时 doClear 立即清零，随后以 ID 0 交给回调——NC-180 复审在 `Range` 上补的是同一处，`RangeGroupEntities` 漏了。修法：与 `rangeHeld` 共用 `visitHeld`，先 `Touch` 再回调、返回后 `UnTouch`，Touch 失败跳过。

Nest 契约：`Touch` / `UnTouch` 是 nest 分发持有实体引用的同一协议（原子计数、不阻塞），回调期间被销毁的实体推迟到引用归还才清理；`RangeGroupEntities` 仓内零调用方；nest 的 `EntityLockGroupScope.Range` 走 `GetGroupEntities`，未改。

修前红：

```
--- FAIL: TestEntityRangeContract/EntityManager.RangeGroupEntities/delete_inside_the_callback
    rangecontract.go:212: Range handed out key 0 (value 0) that never existed
```

## 5. 验证

`GOWORK=off`：`gofmt -l` 空；`go vet ./safemap ./container ./entity ./internal/rangecontract`；`go test -race -count=3 ./safemap ./container ./entity ./internal/...` 通过；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`；根包与全仓 build / vet。生成 DAO：按 `codegen/scripts/dao-golden-runtime.sh` 的方式建一次性模块、`replace` 到本 worktree（脚本默认取最低支持版本 v1.18.0，不含 NC-182 与 A1，现有 runtime 用例本来就要这样跑），`go vet` 与 `go test ./...` 全部通过，含新增 `TestGeneratedRangeXContract` 三种 map。脚本现在把 `internal/rangecontract/rangecontract.go` 复制进一次性模块。没有改生成器与模板，生成形状不变，不需要重新生成 golden / game-demo。

## 6. 实施状态

已实施（见 DECISIONS-PENDING C7 行的提交号）。未做：nest 的 `EntityLockGroupScope.Range` 未套辅助（不在本次范围）；`BucketHolder.RangeCursor` 无同步（N13 O3）照旧，只能单 goroutine 用。
