# 非核心模块修复复审（第三组）：RR-20261004-NC-13～29

2026-10-04，独立审计，只读代码、跑测试，未改仓库文件、未提交、未登记编号。对象：`v1.19.0` = `74e1ba39`（`main` HEAD `2f111e9b` 相对 tag 在 cache / mongo / redis / remoteentity / entity 下无差异）。B 线修复提交：`08d18be9`（NC-13～15）、`ce90e90d`（NC-16～20）、`3d3b22c9`（NC-21～25）、`25ef4c1e`（NC-26～29）。NC-01～12 由另两位审计员负责，本文不涉及。

## §0 范围与方法

- 读：`AGENTS.md`、`docs/agent-skills/roost-coding/SKILL.md`；B 线主记录 [noncore-12](../bug/REVIEW-2026-10-04-noncore-12.md) / [14](../bug/REVIEW-2026-10-04-noncore-14.md) / [16](../bug/REVIEW-2026-10-04-noncore-16.md)，以及 NC-13～15 所在的 [noncore-10](../bug/REVIEW-2026-10-04-noncore-10.md)（只核了锚点）；`docs/bugfix/RR-20261004-NC-13.md`～`NC-29.md` 共 17 份；四个提交的 `git show`；`git diff 42804572..v1.19.0 -- cache/ mongo/`（24 个文件，+2465/−264）。
- 代码图谱：项目 `Users-whb-roost-roost-core` 的索引时间是 2026-09-30，`check_index_coverage` 对所有引用文件都报 `metadata_changed`（`transaction.go` 报 `not_tracked`），只有 `cache/atomic_local.go` 是 `metadata_match`。所以本次结论**以直接读源码和 grep 为准**；`trace_path` 查 `NewLocalStore` 的调用方得到 0，属于索引过期，已改用 grep 核对生产调用方（`entity/remote_snapshot.go`、`codegen/internal/dao/template_redis.go`、`remoteentity/*`）。
- 绿测都在 `v1.19.0` 的临时 worktree 中运行，`GOWORK=off`，环境变量取自 `/Users/whb/.roost-it/roost-dataengine-it/env.sh`。六个 Redis 门变量都指向 `$ROOST_DATAENGINE_IT_REDIS_ADDR`。**特意 unset 了 `ROOST_DATAENGINE_IT_TOXIPROXY_URL` / `*_PROXIED_*`**，因为 `redis/driver/lock_toxic_integration_test.go` 会对 toxiproxy 执行 `POST /reset`，那属于任务禁止的 fault/reset 操作；该用例因此 skip 掉，不在下方统计中。没有动 `/tmp/roost-dataengine-it`，没有 FLUSH。
- 真实 Mongo 探针：用隔离副本集（28117～28119）跑了一个独立的 `main` 程序，库名 `nc3_audit_probe_14804_1791113640`，跑完已 `Drop`（输出 `drop: <nil>`）。
- 证据文件都留在审计 scratchpad 的 `nc3/` 目录（`g-*.log`、`red-*.log`、`probe-*.log`、`zz_nc3_*_test.go`、`bench-*.txt`、`benchstat.txt`），**没有放进仓库**。如果需要入库，按 `docs/review/evidence/` 的惯例另行复制。

### 绿测结果（v1.19.0）

| 命令 | 结果 |
| --- | --- |
| `go test -count=1 -race -json ./cache/... ./mongo/... ./redis/...` | exit 0；303 个 test pass 事件，0 fail，0 skip（cache 119、mongo/mongotest 110、redis/driver 50、redis 16、mongo/driver 8；`mongo` 根包没有测试）。toxiproxy 用例已按上文 unset 跳过 |
| `-race -run '^TestAtomicLocal(OrderBounded|Compaction)' ./cache/`（`atomic_local_order_bounded_promises_test.go` 的 5 个用例） | 全部 PASS |
| `-race -count=400 -run '^TestReadThroughStoreCoalescesMisses$' ./cache/` | ok（1.6s） |
| `-race ./remoteentity ./entity` | ok / ok |
| `go test -run TestCoreDependencyBoundary .` | ok |
| `-race ./dataengine/engine/`（NC-29 改了它的测试夹具） | ok |
| `scripts/test-remote-generated.sh` | exit 0，`ok remoteflow 19.267s`，`TestGeneratedRemote*` 全部 PASS |

## §1 逐条表

“红”一列：**✔** 表示本次在临时 worktree 里回退实现后亲手确认变红，文本见 §2；“—” 表示没有亲手回退，只依赖 B 线证据。

| RR | 记录↔代码 | 回归在包内、v1.19.0 通过 | 红 | 修法问题 / 结论 |
| --- | --- | --- | --- | --- |
| NC-13 ReadThrough fatal | 一致（`read_through.go:151`、`:257-261`） | ✔ `TestCacheAdmissionFatalRemotePolicy` / `RemoteErrorCompatibility` | ✔ | 修法正确。`FatalRemoteError` 现在也会改变 strict 模式下 Delete 的结果（保留 L1），但字段注释仍写“仅用于 IgnoreRemoteError”，见 §4-1 |
| NC-14 Layered 回填拒绝 | 一致（`layered.go:54-80`） | ✔ | ✔ | **新缺陷 D1**：L1 过期副本或 ttl≤0 时会永久否决权威值 |
| NC-15 四 Store 统一 ErrStaleWrite | 一致（4 处） | ✔ `TestCacheAdmissionStaleWriteAcrossStores`、`TestLocalStoreRejectsStaleVersion` | ✔ | 本身正确。与 NC-14 叠加后出现 **D1**（Layered.Set 写入已被权威应用，却报 ErrStaleWrite），并扩大了 **D2**（ReadThrough 的 loader 回填路径把 ErrStaleWrite 当读取错误返回） |
| NC-16 根指针 / nil | 一致（`ref_hmap.go:146-150`、`:158-162`） | ✔ 真实 Redis `TestRefHMapContractsRealRedis` | — | 无问题 |
| NC-17 指针 TextMarshaler | 一致（`ref_hmap.go:828-834`） | ✔ | — | 无问题；每个此类标量多一次 `reflect.New`，位于 I/O 路径 |
| NC-18 nil 父 Patch | 一致（Lua `:64-91`、`Patch :279-306`） | ✔ | — | **新缺陷 D3**：Patch 只续期根和路径上的 hash，兄弟 hash 过期后 Get 会返回部分记录且 ok=true；原先只在根标量 Patch 时出现，NC-18 把它扩大到所有嵌套路径 |
| NC-19 名称碰撞 | 一致（`:254-260`、`:553-557`） | ✔ `TestRefHMapLayoutRejectsAliasesBeforeIO` | — | 无问题；对旧数据的读和删也被拒，记录里已写明 |
| NC-20 分页 | 一致（`paginateMatches`） | ✔ | — | 真实 Mongo 探针 P5：Skip≥count 返回空，与替身修后一致 |
| NC-21 RefHMap 未知写 | 一致（`evalWriteHashes` 直接返回 Eval 的原错误） | ✔ 真实 Redis `TestRefHMapUnknownWrite` / `WriteErrorBoundaries` | ✔ | 修法正确：结果未知时既不伪装成功，也不重放。指标已删除，但 README / OBSERVABILITY / grafana 仍引用它，见 §4-2 |
| NC-22 深复制 | 一致（`cloneBSONValue`） | ✔ | — | 无问题；复制成本见 §4-6 |
| NC-23 $in 去重 | 一致 | ✔ | ✔ | 真实 Mongo P4：`$in [1,1,2]` 的 count 是 2，与修后一致 |
| NC-24 精确数值比较 | 一致（`compareNumbers` / `big.Rat`） | ✔ | — | 真实 Mongo P6：2^53 与 2^53+1 不相等、int64 与 double 的 2^53 相等，均与修后一致。NaN/Inf 现在报 unsupported，而真实 Mongo 支持，见 §4-4 |
| NC-25 post-image 身份 | 一致（`selectedKey`） | ✔ | ✔ | 无问题 |
| NC-26 BSON.D 路径 | 一致（`lookupPath` / `pathMap`） | ✔ | — | 真实 Mongo P7：对标量字段做 `$set meta.n` 会报 code 28，替身却静默把它覆盖成 `{}`；这是旧行为，记录已列为留项，见 §4-5 |
| NC-27 唯一索引验存量 | 一致 | ✔ | ✔ | **缺陷 D4（旧问题，记录列为留项、未登记）**：替身忽略 `Sparse`，把缺字段一律跳过；真实 Mongo 非 sparse 唯一索引把缺字段当 null，P1 探针为 11000 |
| NC-28 Bulk Type 预检 | 一致 | ✔ | ✔ | 无问题 |
| NC-29 事务私有状态 | 一致（`transaction.go`、`lockFor`、`commit`） | ✔（含 `dataengine/engine`） | — | 并发模型更保守，冲突时点与真实 Mongo 不同（P2/P3），记录已声明。`stage_effect_race_test.go` 的夹具改动与该测试自己的注释（“真实 Mongo 表现为 WriteConflict，伪 Mongo 用注入模拟”）一致，没有削弱断言 |

**统计**：17 条 RR 的记录与代码全部一致，17 条的回归都在包内且在 v1.19.0 上通过。亲手证红 8 条（cache 4 条：NC-13/14/15/21，其中 NC-21 用真实 Redis；mongotest 4 条：NC-23/25/27/28），全部真红。确定缺陷 4 项：D1、D3 是 B 线修复引入或扩大的回退；D2 是 NC-15 扩大面后的旧缺陷；D4 是 NC-27 遗留的旧缺陷。疑点与不一致 7 项，另有 1 项替身性能影响（§4）。

## §2 红是否真红

方法：在 `v1.19.0` 临时 worktree 里只回退被测修复，跑 B 线新增的正式用例，然后 `git checkout` 复原。原文如下。

**NC-15**（`local/grouped/redis_raw/redis_hash` 四处 `return ErrStaleWrite` 改回 `return nil`）：
```
--- FAIL: TestCacheAdmissionStaleWriteAcrossStores/local/v4
    admission_promises_test.go:116: refused write reported success: <nil>
（grouped/v4、raw/v4、hash/v4 相同）
--- FAIL: TestLocalStoreRejectsStaleVersion
    cache_test.go:34: RR-20261004-NC-15: stale write err=<nil>, want ErrStaleWrite
```

**NC-13**（`loadOne` 改回 `!s.opts.IgnoreRemoteError`，删去 Delete 的 fatal 分支）：
```
--- FAIL: TestCacheAdmissionFatalRemotePolicy/get
    admission_promises_test.go:64: operation=get err=<nil> loads=1 L1={Key:1 Version:1 Payload:fallback} held=true
    admission_promises_test.go:66: fatal L2 verdict was degraded: <nil>
--- FAIL: TestCacheAdmissionFatalRemotePolicy/delete  … fatal L2 verdict was degraded: <nil>
--- FAIL: TestCacheAdmissionRemoteErrorCompatibility/delete/fatal_strict
    admission_controls_test.go:64: L1 held=false, want fatal=true
```

**NC-14**（`layered.go` 换回 `42804572` 的版本）：
```
--- FAIL: TestCacheAdmissionBackfillAdmission/layered/stale
    admission_promises_test.go:202: read returned the captured value rejected by L1 admission
--- FAIL: TestCacheAdmissionBackfillAdmission/layered/tombstone
    admission_promises_test.go:199: read returned the value rejected by a newer delete
--- FAIL: TestCacheAdmissionLayeredRejectedReadFailures/conflict_miss
    admission_controls_test.go:110: refused value escaped: {Key:1 Version:1 Payload:rejected} true <nil>
```

**NC-21**（`ref_hmap.go` 换成 `ce90e90d` 的版本，即 NC-21 之前；真实 Redis）：
```
--- FAIL: TestRefHMapUnknownWrite/applied_reply_lost_newer_write
    ref_hmap_unknown_write_test.go:130: ack lost after v2; another writer stored v3; Set err=<nil> final=2
    ref_hmap_unknown_write_test.go:132: unknown Lua outcome was replayed over a newer authoritative write
--- FAIL: TestRefHMapWriteErrorBoundaries/applied_reply_lost  … err=<nil> evals=1 fallbacks=1
```

**NC-23 + NC-28**（去掉 `seen` 去重；删掉 Bulk 的 Type 预检循环）：
```
--- FAIL: TestMongoIdentityPromises/duplicate_in
    identity_promises_test.go:80: membership duplicated documents: rows=[{"_id":{"$numberLong":"1"}} {"_id":{"$numberLong":"1"}} {"_id":{"$numberLong":"2"}}]
--- FAIL: TestMongoIdentityBoundaries/duplicate_members_count_and_update   identity_boundary_test.go:27: count=4 err=<nil>
--- FAIL: TestMongoBoundaryPromises/bulk_unsupported_preflight
    boundary_promises_test.go:114: invalid model caused prefix side effect: count=1 err=mongofake: unsupported construct: bulk model type 255
--- FAIL: TestIndexAndBulkBoundaryPromises/invalid_model_1   n=1 …；invalid_model_2   n=2 …
```

**NC-25 + NC-27**（ReturnAfter 改回按旧 filter 重新匹配；删掉 EnsureIndexes 对存量的检查）：
```
--- FAIL: TestMongoIdentityPromises/multiple_return_after
    identity_promises_test.go:128: returned a different document: got={"_id":…"2"…,"version":…"1"…} want={"_id":…"1"…,"version":…"2"…}
--- FAIL: TestMongoBoundaryPromises/unique_existing_duplicates
    boundary_promises_test.go:89: unique index accepted existing duplicates: err=<nil> has=true
--- FAIL: TestIndexAndBulkBoundaryPromises/failed_unique_is_not_published  err=<nil> indexes=[…]
```

结论：抽查的 8 条全部真红，断言都钉在记录所述的现象上，没有出现“回退后照样绿”的情况。

## §3 确定缺陷

### D1（建议 P2）：Layered 的 L1 在过期后（或 ttl≤0 时）仍会永久否决权威值；写入已被权威应用却报 ErrStaleWrite

- **现象**：两个副本 A、B 共用同一个 remote（权威）。A 先 `Set(v5)`；B 执行 `Delete` 后 `Set(v1)`，即同一 key 开始新的生命周期（另一种触发：Redis TTL 到期后晚到的写被 L2 接受）。A 过了 TTL 窗口后 `Get`，**每次都返回 v5，而权威持有 v1**。`ttl=0` 本应保证“绝不从 L1 服务”（`localValid` 的注释），结果同样永远返回 v5。随后 A 执行 `Set(v2)`：权威已经变成 v2，A 却收到 `ErrStaleWrite`，即已经成功的写被报成拒绝。
- **v1.19.0 实测**（`probe-layered-v119.log`）：
  ```
  A.Get #0..#2 = {Key:1 Version:5} ok=true err=<nil> (authority {Key:1 Version:1})   [ttl=0 与 ttl=30ms 都如此]
  A.Set(v2) err=cache: write refused as stale authority now {Key:1 Version:2}
  ```
  **修前 `42804572`**（`probe-layered-42804572.log`）：ttl=0 时三次都返回 v1，Set 返回 nil，测试 PASS。ttl=30ms 时第一次返回 v1，之后在 TTL 窗口内返回 v5，属于有界的旧问题。
- **根因**：
  - `08d18be9` 的 `cache/layered.go:58-73`：回填遇到 `ErrStaleWrite` 时一律“读回 L1 已准入值”。但 Layered 里的 L1 副本只在 `localValid` 期间有效；过期以后它只是旧缓存，不再是“更新的已准入值”。生成的 DAO 用 `LocalStore` 作 L1，L1 本身不过期，所以这种否决没有时间上限。
  - `layered.go:115-118`：remote 写成功后，`local.Set(current)` 被拒，于是把 `ErrStaleWrite` 原样返回。
  - `local.go:75` 让 LocalStore 开始真正返回这个错误。
  - 可达路径：codegen 生成的带版本字段的 `NewCached*RedisDAO`，即 `Layered(LocalStore{VersionStale}, RedisRawJSON / RefHMap{VersionStale})`，见 `template_redis.go:76-88`、`:173-174`。
- **会红的测试草稿**：`nc3/zz_nc3_layered_test.go`（`TestNC3LayeredExpiredL1VetoesAuthority`，纯内存，约 50 行）：
  ```go
  remote := NewLocalStore(cfg)              // cfg: KeyOf + VersionStale
  a := NewLayeredStore[int, item](NewLocalStore(cfg), remote, ttl, cfg)
  b := NewLayeredStore[int, item](NewLocalStore(cfg), remote, ttl, cfg)
  a.Set(ctx, v5); b.Delete(ctx, 1); b.Set(ctx, v1); time.Sleep(ttl + 10ms)
  got, _, _ := a.Get(ctx, 1)                 // want v1（权威）；v1.19.0 得到 v5，ttl∈{0, 30ms}
  err := a.Set(ctx, v2)                      // remote 已是 v2；v1.19.0 返回 ErrStaleWrite
  ```
- **候选修法**（不重开 NC-14 的竞态，NC-14 的竞态修复保留）：
  - 回填遇到 stale 时，**只有 `localValid(key, now)` 为真才交付 L1 的值**。并发的 `Layered.Set` 会续期 expiry，所以 NC-14 的 stale/tombstone 竞态仍然走这一支。
  - 否则说明 L1 是过期副本：`local.Delete` 之后再 `local.Set(value)` 并 `setLocalExpiry`，交付 L2 的值。ttl≤0 时永远走这一支，或者干脆不回填。
  - `Set` 在 remote 写成功、回读 `current` 之后，如果 L1 返回 `ErrStaleWrite`，同样应当先删后写，或者只删 L1，不把错误返回给调用方。
  - 回归用例要同时保留 NC-14 现有的三个红用例。

### D2（建议 P3）：ReadThrough 的 loader 路径把 L1 准入拒绝当成读取错误返回

- **现象**：loader 加载 v1 期间，同一 key 的 v2 已经发布进 L1，随后 `Get` 返回 `(zero, false, ErrStaleWrite)`。也就是一次读取因为“写被拒”而失败。同一函数里的 remote 回填分支（`read_through.go:160-174`）会交付 L1 的现值，loader 分支没有这样做。
- **实测**（`probe-v119.log`）：v1.19.0 上 `local` 和 `atomic` 两个子用例都输出 `err=cache: write refused as stale`。`42804572` 上，`local` 子用例静默交付旧值 v1（这同样是错的），`atomic` 子用例已经报同样的错误。所以这是**旧缺陷，NC-15 把它扩大到了 LocalStore**。
- **根因**：`cache/read_through.go:191-193`（`74e6d971`）中 `if err := s.setLocal(ctx, value); err != nil { return zero, false, err }` 没有区分 `ErrStaleWrite` 和 `ErrConflictingWrite`。另外，`:186-188` 的 remote.Set 在 strict 模式下，现在也会因为 RawJSON/Hash 返回 `ErrStaleWrite` 而让读取失败。仓库里目前唯一的生产用法 `entity/remote_snapshot.go:238` 的 loader 是 nil，**不受影响**；受影响的是公开 API 的其他组合。
- **会红的测试草稿**：`nc3/zz_nc3_audit_test.go` 里的 `TestNC3ReadThroughLoaderFillRefused`（loader 内先 `local.Set(v2)` 再返回 v1；期望 `Get` 返回 v2 且 err 为 nil）。
- **候选修法**：loader 分支复用 remote 回填分支的 `switch`：遇到 `ErrStaleWrite` / `ErrConflictingWrite` 时读回 L1 的已准入值，L1 没有值则返回 miss（stale）或拒绝错误（conflict）；remote.Set 返回的 `ErrStaleWrite` 不让读取失败。

### D3（建议 P2）：RefHMap Patch 只续期根和路径上的 hash，兄弟 hash 过期后 Get 返回部分记录且 ok=true

- **现象**：用 TTL=600ms 的 store 执行 `Set{ID, Name, Meta{Label}, Other{X:42}}`，400ms 后 `Patch("meta.label")`，再过 400ms `Get`，返回 `Other=&{X:0} ok=true`。写入的 42 丢了，而且这个对象从来没有被写过。
- **实测**（真实 Redis，`probe-v119.log`）：`meta.label` 和 `name` 两个子用例都 FAIL。修前 `42804572`（`probe-42804572.log`）：`meta.label` 子用例 PASS，因为旧 Patch 只续期叶子 hash，根到期后整条记录 miss；`name`（根标量 Patch）子用例已经 FAIL。所以是**旧缺陷，被 NC-18 从根标量扩大到所有嵌套路径**。
- **根因**：`ce90e90d` 的 `cache/ref_hmap.go:87-90`，Patch 的 Lua 只对 `KEYS`（根到叶的路径）执行 `PEXPIRE`；`decodeRefHMapNode`（`:632-663`）遇到被引用但已不存在的子 hash 时留零值，不报 miss。NC-18 记录里写了“旁支仍保持原TTL”，但没有写出“过期后返回部分记录”的后果。可达路径：生成的 ref-hmap DAO 的 `NewRedis*RedisDAO` 把 patcher 设为 store 本身，并且 TTL 恒大于 0（默认 1h，`template_redis.go:40-41,55-61`）。
- **会红的测试草稿**：`nc3/zz_nc3_audit_test.go` 里的 `TestNC3RefHMapPatchTTLPartialRecord`（真实 Redis，前缀 `roost:nc3audit:*`，Cleanup 时 Delete）。
- **候选修法**（二选一）：
  - Patch 的 Lua 改为对注册表 `__keys` 里的全部键执行 `PEXPIRE`。注册表已经在脚本里读出，只需把“路径键”换成“注册表键 ∪ 路径键”。
  - 或者 Get 时如果发现某个被引用的子 hash 缺失，就当作整条记录 miss 或损坏报错，不返回部分值。
  - 前者成本更低，也符合“同一条记录同生共死”。

### D4（建议 P3，旧缺陷）：mongotest 忽略 `IndexModel.Sparse`，非 sparse 唯一索引把缺字段跳过，而真实 Mongo 把它当作 null

- **现象**：替身中两条文档都没有 `name` 字段时，`EnsureIndexes({name:1}, Unique)` 返回 nil；在已有唯一索引的集合里连续插入两条缺 `name` 的文档也都成功。
- **真实 Mongo 探针**（隔离副本集）：
  ```
  P1 unique index over two docs missing field: CommandError code=11000 name=DuplicateKey
  P1b insert second doc missing field: WriteException code=11000 dupKey=true
  ```
  替身：`probe-mongotest-unique.log` 中两项均为 `<nil>`。
- **根因**：`mongo/mongotest/mongotest.go:960-971` 的 `checkUniqueFieldsLocked` 在 `lookupPath` 失败时设 `values = nil` 并 `return nil`，全文件找不到 `Sparse`。NC-27 记录写了“缺字段沿用替身的跳过规则，不等价于真实非 sparse Mongo 唯一索引”，列为留项但没有登记。替身比真实 Mongo 更宽松，会让“插入多条缺唯一字段的文档”这类缺陷在测试里通过。
- **会红的测试草稿**：`nc3/zz_nc3_mongotest_test.go` 里的 `TestNC3UniqueIndexMissingFieldIsNull`。
- **候选修法**：唯一索引登记时保存 `Sparse`。非 sparse 时缺字段按 BSON null 参与比较，与显式 null 相等；sparse 时只有全部字段缺失才跳过（复合索引的 sparse 语义需要另外核对）。修法要保留 NC-27 的“先验存量、失败不发布”。

## §4 疑点与不一致

1. **`FatalRemoteError` 的注释与新语义不符**：`read_through.go:30-36` 说它只给 `IgnoreRemoteError` 分类。NC-13 之后，strict 模式下 Delete 遇到 fatal 错误会保留 L1（以前清 L1），注释没有更新。行为本身合理，测试 `delete/fatal_strict` 也钉住了，属于文档漂移。
2. **NC-21 删除的指标仍被引用**：`README.md:401`、`README.md:427`、`OBSERVABILITY.md:73`、`:122` 仍把 `cache.refhmap.write_degraded_total` 描述为“非零需告警”；`observability/grafana-roost-overview.json:290` 仍有这个面板表达式；`failurelog/failurelog.go:223` 的注释也引用了它。CHANGELOG 和 USER_GUIDE 已经说明删除，其余文档没有同步。
3. **记录状态不一致**：`docs/bug/README.md:15-18`（tag 与 HEAD 相同）里 NC-26～29 的表格行仍是“已确认、未修”，而同页第 9 行写着“4 项已修”。另外，NC-13～29 的单条记录在 HEAD 上仍写“尚未发版”，但 `v1.19.0` 已经发布（HEAD 提交 `2f111e9b` 只更新了索引和交接文档）。
4. **NaN/Inf**：替身现在对非有限浮点报 `ErrUnsupported`（`exactNumber`），连“集合里某条文档的字段是 NaN、查询 `$gt: 5`”也会让整个查询报错。真实 Mongo 正常返回（探针 P6：count=1，err nil）。这是有意“响亮失败”，但已有测试数据里有 NaN/Inf 的使用方会从“结果可能不准”变成“直接报错”；CHANGELOG 已写明，记一笔。
5. **路径语义的旧差异（未登记，记录已列为留项）**：对标量字段执行 `$set meta.n`，真实 Mongo 报 code 28（P7），替身静默把 `meta` 覆盖成 `{n:9}`（`setPath`）。D 转 M 后字段顺序丢失。
6. **NC-29 与真实 Mongo 的冲突时点不同**：
   - 真实 Mongo 在写语句上立即报 `WriteConflict(112, TransientTransactionError)`（P2）；替身在 commit 时按集合 revision 判定冲突。
   - 事务外向同一集合写**另一条**文档，真实 Mongo 的 commit 成功（P3），替身会冲突并重试。
   - 因此“回调里处理写语句返回的 WriteConflict”这条生产路径，替身无法覆盖：例如错误分类有没有保留 transient 标签，替身测不出来。记录说的是“比真实更保守、写冲突时点未建模”。建议在 USER_GUIDE 的 mongotest 段落里明确写上这一盲区。
   - 每次 attempt 都深复制全客户端集合（`snapshot()`）；`EnsureIndexes` 对任何索引都会 `revision++`，让并发事务冲突。
   - 性能对照见下方“性能”。
7. **Layered.Set 的新鲜度回退（轻微）**：L2 返回 `ErrStaleWrite` 后，`Layered.Set` 直接返回，不再像以前那样回读 L2 的现值来刷新 L1。本副本的 L1 可能继续服务较旧的值直到 TTL 到期。行为在 TTL 范围内有界，和 D1 不同。

**性能**：
- cache 热路径：`cache/atomic_local.go` 在 `42804572..v1.19.0` 之间没有改动；ReadThrough 和 Layered 命中 L1 的路径没有改动，新增逻辑都在错误分支；LocalStore 等返回的是哨兵错误，不分配。所以没有跑 cache 基准（唯一的基准 `BenchmarkAtomicLocalStoreOverwrite` 所测代码未变）。
- RefHMap：Set 多一次 `reflect.ValueOf(value)`，struct 类型的 V 可能多一次装箱分配；Get 多一次 `reflect.TypeOf`；Patch 从“pipeline HSET+EXPIRE”变为一次 EVAL，往返次数相同。这些都在网络 I/O 路径上，仓库里没有对应基准，没有测量。
- mongotest：`dataengine/engine` 的 `BenchmarkMongoProjection*` 走替身。用 `-benchtime=1000x -count=6`、benchstat、n=6 对照：旧侧是 `42804572` 版 `mongotest.go` 且去掉 `transaction.go`，新侧是 v1.19.0；生产代码两侧完全相同。结果：
  - **时间 geomean +9.66%**：four_document_transaction +24.73%，single_with_outbox +10.02%，batch_256 +7.22%，conflict_1/10% 分别 +4.60% / +5.75%，单行 CAS 无显著差异（p=0.065）。
  - **allocs geomean +26.31%**（four_document +69%），B/op +8.37%，p 均为 0.002。

  这些变化**全部来自替身**（深复制、私有视图、revision）。DataEngine 的投影基准如果被用作版本间的性能基线，v1.19.0 前后的差异会被误读为生产回退。另外，single_with_outbox 的耗时随 outbox 文档数增长，因为每个 attempt 都要复制全部集合，所以基准测到的主要是替身成本。B 线记录写了“未作 benchmark”，没有给出这个量级。建议在交接文档里写明这一点，或者让基准跳过替身的快照成本。

## §5 RR-20260930-03 / RR-20260930-11 核对结论

- **RR-20260930-03（AtomicLocalStore 时钟记录有界，`compactOrderLocked`）**：
  - `git diff 42804572..v1.19.0 -- cache/atomic_local.go` 为空，B 线没有碰 AtomicLocalStore 的实现。
  - `atomic_local_order_bounded_promises_test.go` 的 5 个用例（Overwrite / DeleteChurn / ExpiryChurn / CompactionKeepsEvictionOrder / Concurrent）在 v1.19.0 上 `-race` 全部 PASS。
  - C01 的 1h / 24h 内存验证所依赖的代码没有变化，结论继续成立。
- **RR-20260930-11（`TestReadThroughStoreCoalescesMisses`）**：
  - `42804572..v1.19.0` 之间对 `atomic_local_test.go` 只有 `649252b8`（11 的补修本身）；B 线对 `read_through.go` 只改了 `loadOne` 的 remote 错误分支（`:151`）和 `Delete`。合并逻辑（`load` 的 `calls` 与 `waiters`）没有改动。
  - `-race -count=400` 全部通过。
  - 合并语义没有被破坏。D2 发生在 loader 返回之后的回填步骤，与合并无关。
- **Remote 快照 L1**：`entity/remote_snapshot.go:238` 使用 `ReadThroughStore(AtomicLocal, l2, nil loader, IgnoreRemoteError, Fatal=ErrRemoteVersionConflict|ErrConflictingWrite)`。
  - NC-13 改变的是“L2 的 Get/Delete 返回 fatal 错误”时的行为；仓库里 `remoteentity/snapshot_l2.go` 只在写入路径（`:200`）返回 `ErrRemoteVersionConflict`，Get/Delete 不返回 fatal，所以实际行为不变。
  - NC-14 只涉及 Layered，NC-15 没有改 AtomicLocal 和 RedisJSON，因此这条链路不受影响。
  - `./remoteentity ./entity -race` 和 `scripts/test-remote-generated.sh` 均通过。

## §6 没读完 / 没跑的

- mongotest 基准已跑完（§4“性能”）；RefHMap 和 cache 没有可用基准，没有测量 RefHMap Set/Get 新增反射调用的成本。
- **只读了改动处的代码**：没有通读 `mongotest.go` 的全部 2043 行（未改动的 `applyUpdate`、`seedFromFilter`、`sortLocked` 等），也没有通读 `ref_hmap.go` 中未改动的编解码部分。
- **NC-16/17/18/19/20/22/24/26/29 没有亲手证红**，依赖 B 线证据。
- **NC-29**：替身的并发锁序在 `Database.Drop` 后出现同名 namespace 时，`collectionLess` 会认为新旧两个对象相等，存在理论上的锁序不稳定，没有构造复现，记录已声明“Drop/同名并发未建模”。
- **D3**：还有一个未验证的分支：没有 `__keys` 注册表的旧数据被 Patch 后，注册表只含路径键，之后 Delete 会遗留兄弟 hash。
- **真实 Mongo 探针只覆盖 7 个点**（P1–P7），没有做完整差分。Cluster/HA、真实网络丢包、长稳都没有执行。
- `redis/driver/lock_toxic_integration_test.go` 因为禁止 fault/reset，有意没有跑。
- 没有审 NC-01～12（不在本组范围），没有审 `25ef4c1e` 中附带的 RR-20261004-01 / Redis 锁相关文档。
