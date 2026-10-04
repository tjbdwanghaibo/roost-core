# NC-30 独立审计：RefHMap schema 清理 guard（`7fbdc735`）及其与 RR-20261004-03 的交互

2026-10-04；独立审计员，只读、未改仓库文件、未提交。审计对象 `7fbdc735`“fix(cache): guard RefHMap schema cleanup and continue N04 review”（B 线最后一笔），以及它在 `v1.19.1` = `d3e69336` 上与 A 线 RR-20261004-03（`5c1647c6`）合并后的状态。

## §0 范围与方法

- 读过：`AGENTS.md`、`docs/agent-skills/roost-coding/SKILL.md`、`~/.claude/skills/roost-review/SKILL.md`；`git show 7fbdc735`（代码只有 `cache/ref_hmap.go`、`cache/ref_hmap_schema_promises_test.go`、`cache/ref_hmap_test.go`）；[NC-30 主记录](../bug/RR-20261004-NC-30.md)、[NC-30 修复](../bugfix/RR-20261004-NC-30.md)、[第 19 轮](REVIEW-2026-10-04-noncore-19.md)、[RR-03 修复](../bugfix/RR-20261004-03.md)、[学习页](IMPLEMENTATION-DAO-MIGRATION-HYDRATION-AND-REFHMAP-SCHEMA.md)、[WANTED W-2026-10-04-02](../bug/WANTED.md)、CHANGELOG `## [v1.19.1]`。
- 代码：`v1.19.1` 的 `cache/ref_hmap.go` 全文、`cache/layered.go`、`cache/read_through.go`（回填段）、`codegen/internal/dao/template_redis.go`、`kit/nats/nats_mod.go`、`nats/driver/{assembly,client}.go`，以及 nats.go v1.53.1 的 `Conn.Drain`。
- 图谱：项目 `Users-whb-roost-roost-core`，generation `2026-09-30T11:28:30Z`，比 v1.19.1 早。`check_index_coverage` 对 8 个引用路径都没有记录缺口，但 `ref_hmap.go`、`layered.go`、`read_through.go`、`nats_mod.go`、`assembly.go` 是 `metadata_changed`，NC-30 测试文件是 `not_tracked`；`search_graph` 返回的行号还是 09-30 的旧值（如 `evalWriteHashes 321-378`）。所以下面的结论都以 `git show v1.19.1:<path>` 的当前源码为准，图谱没有作为证据。
- 运行环境：Go 1.27.0；`~/.roost-it/roost-dataengine-it` 的独占 Redis（8.10.1）和 NATS。只 `source` 了 env.sh；导出 `REDIS_ADDR`、`ROOST_REDIS_TEST_ADDR`、`ROOST_REVIEW_REDIS`（三者都等于 `$ROOST_DATAENGINE_IT_REDIS_ADDR`）、`ROOST_REVIEW3/4_BACKEND=redis`、`ROOST_BUGFIX5_BACKEND=redis`，以及 `GOWORK=off`。没有 FLUSH，没有碰 `/tmp/roost-dataengine-it`。探针键用随机前缀并在测试中清理，结束后 `--scan roost:audit*`、`roost:refschema:*`、`roost:rr0403:*` 都是 0。
- 临时 worktree：`scratchpad/audit-nc30`（`v1.19.1`）。回退实验和探针只在这个 worktree 里做，用完已 `git worktree remove --force`。探针源码另存在 `scratchpad/audit-nc30-probes/`（未入库）。

## §1 NC-30 核对

**实现与记录一致**（`v1.19.1:cache/ref_hmap.go`）：

| 记录所述 | 源码 | 结论 |
| --- | --- | --- |
| Set 的 Lua 在 DEL 之前 `HGET root.__keys`，与 ARGV[3] 比较 | :48-51 | 一致；`local arg = 4`（:52），写入参数整体后移一位 |
| 新增 `refHMapDeleteScript`，Delete 改走 Eval | :78-84、:229-246 | 一致；不再用 Pipeline.Del/Del |
| `registeredKeys` 返回原始 registry 字节，root 固定排在 KEYS[1] | :384-399 | 一致；记录里写的行号 `:363` 是合并 RR-03 之前的位置，现在是 :384（只是文档漂移） |
| 返回 0 转成 `ErrRefHMapRegistryChanged`；Eval 错误原样透传；其他回复报错 | `refHMapWriteResult` :424-436 | 一致 |
| ErrNil 和没有 registry 的历史 root 都按空字节比较 | :386-394，Lua 里 `or ""` | 一致 |
| 包内 fake 同步改了参数位置，并仿真 Delete | `ref_hmap_test.go` Eval 分支 | 一致 |

**回归在包里，v1.19.1 上通过**：`go test -count=1 -race -tags integration -run 'TestRefHMapSchemaRegistryPromises|…' -v ./cache/`，12 个子用例全部 PASS。整个 `./cache/...` 跑 `-race -tags integration -json` 是 156 pass、0 fail、0 skip（按带 Test 字段的事件计数，含子用例），exit 0。`go vet -tags integration ./cache/...` 通过。

**回退 NC-30 后回归变红**（worktree 内执行 `git show 7fbdc735 -- cache/ref_hmap.go cache/ref_hmap_test.go | git apply -R`，再加一个只声明 `ErrRefHMapRegistryChanged` 的 shim 让测试能编译）：

```text
ref_hmap_schema_promises_test.go:114: registry race was not refused before writes: err=<nil> child_remaining_after_delete=1 probe=<nil>   (×4：set/delete_registry_changed、set/delete_registry_changed_by_patch)
ref_hmap_schema_promises_test.go:162: original unknown cause lost: <nil>   (×2：delete_unknown_applied/unapplied)
--- FAIL: TestRefHMapSchemaRegistryPromises   6 FAIL / 6 PASS
```

注意 `delete_unknown_*` 的两条红是夹具造成的：它把错误注入挂在 Eval 上，而旧 Delete 不走 Eval。它们钉住的是“新 Delete 走 Eval 后会透传未知错误”，不能证明旧实现吞掉了未知错误。

另外只把 `registeredKeys` 的 root-first 改回旧顺序 `append(keys, root)`，`legacy_registry_order_cleanup` 变红（`:192: cache: redis ref hmap registry changed`），说明 KEYS[1]=root 这一点也有回归钉住。

## §2 与 RR-20261004-03 的交互

**合并后 Patch 的 KEYS/ARGV 布局一致**。NC-30 没有改 Patch。`Patch`（:312-341）的 KEYS 是 `uniqueRefHMapKeys(target.keys ++ plan.keys())`。`uniqueRefHMapKeys`（:1005）保持首次出现的顺序；NC-19 在 layout 阶段就拒绝重复的物理键（:286-291），所以路径键各不相同，KEYS 的前 `len(target.keys)` 个正好是路径。`ARGV[4]=len(target.keys)`，`ARGV[5..]` 是引用字段名，和脚本 :97-127 的 `path_count`、`ARGV[4+i]` 对得上。包内 fake 的 Patch 分支也按 `4+pathCount-1` 校验参数个数。

**注册表并入的效果一致**。RR-03 让 Patch 把整条布局键并入 `__keys`。对 v1.19.1 的 Set 写出的记录，registry 就是 `join(plan.keys())`，同布局 Patch 并入后字节不变，不会触发 NC-30 guard。新布局的 Patch 会登记新键、改变 registry，guard 会拒绝并发的旧 Set/Delete（`set/delete_registry_changed_by_patch` 已验证）。历史上没有 registry 的记录被 Patch 后，registry 从 "" 变成路径优先顺序的清单；这会让同时进行的 Set/Delete 遇到字节变化，属于 §4 D1 那一族。

**Set/Delete 的 KEYS[1] 一定是 root**。`writeHashes`（:374-381）的清单是 `deleteKeys ++ plan.keys()`，而 `deleteKeys` 不管走 fallback（`plan.keys()`，`collectKeys` 先放 root）还是走 registry（显式把 root 放最前），首元素都是 root。`evalWriteHashes` 只会把不在清单里的写键追加到末尾。

**真实 Redis 上三组回归同时通过**（v1.19.1 worktree）：

```text
$ go test -count=1 -race -tags integration -run 'TestRefHMapSchemaRegistryPromises|TestRefHMapPatchKeepsTheWholeRecordAliveRealRedis|TestRefHMapGetTreatsAMissingReferencedHashAsMiss' -v ./cache/
--- PASS: TestRefHMapPatchKeepsTheWholeRecordAliveRealRedis (1.61s)   5/5 子项
--- PASS: TestRefHMapGetTreatsAMissingReferencedHashAsMiss (0.00s)
--- PASS: TestRefHMapSchemaRegistryPromises (0.01s)                   12/12 子项
ok  	github.com/tjbdwanghaibo/roost-core/cache	3.199s
```

回退 NC-30 之后，RR-03 的两条回归仍然 PASS，说明两者互不依赖。第 19 轮的合并夹具 `review19_merged_test.go.txt` 作为 overlay 在 v1.19.1 上跑，16/16 PASS，其中 `sibling_expiry_observation` 已经是 RR-03 修后的“整条 miss”。

**Cluster 同槽（看键的构造）**：键是 `prefix + ":{" + name + ":" + keyString + "}" + suffix`（:278-293 layout，:299-309 plan）。一条记录的全部键共享同一个 `base`，第一个 `{` 和它之后第一个 `}` 都落在 `base` 里，所以同一条记录同槽。NC-30 新增的 `HGET KEYS[1]` 访问的是已声明的键，没有引入未声明键的访问。例外情况和 RR-03 记录里的相同：Prefix 带空 tag `{}`。另外见 §5 的 Name 以 `}` 开头。本轮没有起 Cluster 实测。

**层次边界**：`GOWORK=off go test -count=1 -run TestCoreDependencyBoundary .` 结果 `ok`（v1.19.1）。

**热路径分配**（真实 Redis，`testing.AllocsPerRun(500)`，含 go-redis 自身的分配，只用来比较前后差）：Set 从 72 次升到 74 次（多出 `string(raw)` 和 root-first 切片），Delete 从约 50 次降到约 45 次（Pipeline.Del 换成 Eval）。代价不大。延迟和网络开销没有测；Delete 每次都发送脚本全文（EVAL，不是 EVALSHA）。

## §3 第 19 轮 B 线登记但没修的问题，逐条核实

| 项 | 第 19 轮的状态 | 本次核实 |
| --- | --- | --- |
| RR-20261004-02/03/04/05/06/07 | 接手上游、未修 | 都已在 v1.19.1 实施（`docs/bug/README.md` 第 3 行和 :141-146）。RR-03 本次在真实 Redis 上复跑为绿（见 §2）；其余几项本次没有独立复测 |
| 观察 1：Get 不是跨 hash 快照（torn read） | 观察夹具，没有登记成 RR | v1.19.1 上可复现：`non-atomic read combines root v1 with later children 20/30`（无 Pipeline 的 adapter）。这是 NC-18 起就有的声明边界，NC-30 没有改它，不算新缺陷 |
| 观察 2：旁支过期后返回部分记录 | 关联到 RR-03 | 已由 RR-03 修复，合并夹具在 v1.19.1 上为整条 miss |
| 观察 3：Stale 只是建议性比较，不是 CAS | 观察夹具 | 可复现：`registry guard is not a value CAS; Stale remains advisory under concurrent writers`。属于声明边界 |
| W-2026-10-04-02：NatsMod 在连接 drain 超时后重试永远失败 | 分流为“再观察、等独立的真实 NATS 红证据”，没修 | **真实 NATS 上已复现红**，见 §4 D2 |
| 历史 TTL=0 孤儿不清理、Cluster/HA/弱网没测、N01～N04 只完成部分场景、17 个环境 skip | 留项 | 本次没有核实，保持原状 |

## §4 确定缺陷

### D1：NC-30 按字节比较 registry，把同布局的正常并发判成冲突——Set/Delete 返回 `ErrRefHMapRegistryChanged`，Cached DAO 的 L1 还会留着已删除的记录

- **现象**：没有任何 schema 变化，同一布局的两个写者并发时，NC-30 修复后出现 5 种原本成功、现在报错的情况：
  1. 新 key 被两个写者同时首次创建，一个 Set 失败；
  2. 两个 Delete 并发，后执行的那个失败，但记录已经删掉了；
  3. Set 和 Delete 并发，Set 失败；
  4. Set 在读完 registry 之后、Eval 之前，整条记录正好到期，Set 失败；
  5. 走 Layered（生成的 CachedDAO 就是这样接的）首次创建时，Set 失败。

  这 5 种情况在回退 NC-30 的代码上都成功。更严重的是，经过 `LayeredStore` 时 `remote.Delete` 失败会直接返回（`cache/layered.go:151-154`），不会删 L1。结果是 Redis 里记录已经没了，本进程的 L1 在 TTL 窗口内仍然返回被删的值。
- **触发**：多进程常规并发（缓存首次填充、重复删除、删除和重建交错、TTL 到期边界），不需要 schema 发布。
- **根因**：`v1.19.1`（`d3e69336`）`cache/ref_hmap.go:49`（Set 脚本）和 `:79`（Delete 脚本）用 `(HGET root.__keys or "") ~= ARGV` 逐字节比较 registry。NC-30 真正要防的是“当前 registry 里有本次清理清单没覆盖的键”，也就是 Lua 执行时 `current registry ⊄ KEYS` 的情况。字节不同并不等于有遗漏：同布局时清理清单（fallback 的 `plan.keys()` 或旧 registry ∪ `plan.keys()`）已经覆盖了并发写者登记的所有键；registry 变成 ""（被删或过期）时也没有需要清理的东西。`refHMapWriteResult`（:431-432）把这些情况都报成“此次未写”的冲突。NC-30 修复记录的“兼容”节承认了“初次并发创建……可能改变 registry”，但这个拒绝并不是安全所必需的，而且没有写进 CHANGELOG 的行为变化说明（见 §4 附注）。
- **真实 Redis 红文本**（v1.19.1，探针 `scratchpad/audit-nc30-probes/zz_audit_nc30_probe_test.go`、`zz_audit_nc30_layered_probe_test.go`）：

```text
first_create_same_layout: op=cache: redis ref hmap registry changed final={ID:1 Version:1 …} held=true keys=2
delete_vs_delete: op=cache: redis ref hmap registry changed final={ID:0 …} held=false keys=0
set_vs_delete: op=cache: redis ref hmap registry changed … held=false keys=0
set_vs_expiry: op=cache: redis ref hmap registry changed … held=false keys=0
layered_first_create: op=cache: redis ref hmap registry changed … held=true keys=2
--- PASS: …/new_layout_first_create_control   (真正的 NC-30 风险仍被拒绝，child_exists=1)
zz_audit_nc30_layered_probe_test.go:44: record deleted in Redis (keys=0) but A.Delete=cache: redis ref hmap registry changed and A.Get still held=true version=1
```

  在回退 NC-30 的代码上，上面 5 个场景和 Layered 的删除探针都是 PASS（`op=<nil>`、`held=false`），只有 control 变红（`control must be refused: <nil>`）。说明这是 NC-30 引入的回归。
- **会红的测试草稿**（摘要；完整源码见 `audit-nc30-probes/zz_audit_nc30_probe_test.go`）。复用包内的 `refSchemaHookRedis`，在 HGet 之后注入另一个同布局写者：

```go
hook := &refSchemaHookRedis{IRedis: client, afterRegistry: func() error { return other.Set(ctx, v(1)) }}
if err := NewRedisRefHMapStore(hook, cfg).Set(ctx, v(2)); err != nil { // 同布局首次创建
    t.Fatalf("same-layout first create refused: %v", err)
}
// 另需：Delete‖Delete、Set‖Delete、Set‖整条过期、LayeredStore 首建、
// LayeredStore Delete‖Delete 之后 layered.Get 必须 held=false；
// 再加 control：旧布局写者遇到新布局创建者必须得到 ErrRefHMapRegistryChanged。
```

- **候选修法**：把 guard 从“字节相等”改成“覆盖检查”。字节相等时直接放行（快路径）；不相等时，在 Lua 里逐个检查当前 registry 的条目是否都在声明过的 KEYS 中，有任何一个不在就返回 0。不需要访问未声明键，也不需要额外往返。在审计 worktree 里试过这个改法（diff 存在 `scratchpad/candidate-coverage-guard.diff`）：NC-30 的 12 条回归、RR-03 的 2 条回归、上面 6 个探针，以及整个 `./cache/...`（`-race -tags integration`）全部通过。同包 fake 的 Eval 要同步改成覆盖语义。真正的 schema 竞争仍然会被拒绝。用户文档（USER_GUIDE、T-209）里“初次并发创建会失败”的说法需要相应修订。
- **等级建议**：P2。原因是它是 v1.19.1 新带出的回归，在零 schema 变化的常规并发下就会让 Set/Delete 报错，并且 CachedDAO 的 Delete 失败会让 L1 继续提供已删除的记录。如果认为调用方重试就能收敛、L1 残留只限 TTL 窗口，也可以定为 P3。编号不由我登记。

### D2：W-2026-10-04-02 在真实 NATS 上成立——连接 drain 超时后，NatsMod 的重试一直返回 `nats: connection closed`，`asm` 永远不会置空

- **现象**：第一次 `StopWithContext` 因为 drain 超出预算，返回 `context deadline exceeded`。这时连接已经被硬关闭，但 `m.asm` 被保留了。之后无论在 handler 还没退出时重试，还是在 handler 退出、nats.go 的 drain goroutine 收尾 10s 之后重试，都立即返回 `nats: connection closed`，`asm` 一直保留，停止流程无法收敛。
- **触发**：订阅 handler 慢，或者积压的消息在 Stop 预算内处理不完。
- **根因**（`v1.19.1`）：
  - `nats/driver/client.go:162-171` 在 ctx 超时时执行 `c.conn.Close()` 并返回 `ctx.Err()`；
  - `nats/driver/assembly.go:66-71` 把这个错误原样返回；
  - `kit/nats/nats_mod.go:175-180` 遇到任何 `Close` 错误都保留 `m.asm`；
  - 重试时 `DrainWithContext` 走到 `c.conn.Drain()`，而 nats.go v1.53.1 `Conn.Drain` 的第一个分支是 `if nc.isClosed() { return ErrConnectionClosed }`。

  也就是说，“返回 ctx 错误”被调用方理解成“还可以继续排空”，但实际上连接已经是终态。
- **真实 NATS 红文本**（探针 `audit-nc30-probes/zz_audit_w0402_test.go`，`-tags integration -race ./kit/nats/`）：

```text
first_stop=context deadline exceeded asm_retained=true conn_connected=false
A1 handler still blocked: retry=nats: connection closed after=0s asm_retained=true
A2 handler still blocked: retry=nats: connection closed after=0s asm_retained=true
B after drain goroutine settled: retry=nats: connection closed after=0s asm_retained=true
zz_audit_w0402_test.go:73: NatsMod stop never converges after a connection drain timeout: err=nats: connection closed asm_retained=true
--- FAIL: TestAuditW0402NatsModRetryAfterConnectionDrainTimeout (10.21s)
```

  另一个实测细节：硬关闭之后，如果 handler 恰好在这时退出，nats.go 的 drain goroutine 会把状态从 `CLOSED` 改回 `DRAINING_PUBS`，持续约 2s 后才回到 `CLOSED`（driver 层探针日志：`before retry: status=DRAINING_PUBS closed=false`）。在这个窗口内重试会返回 nil，在窗口外重试一直失败。所以结果取决于时机，草稿测试必须避开这个窗口：handler 阻塞期间重试，或者 handler 退出后等足够久再重试。
- **会红的测试草稿**：真实 NATS 上 `Assemble`，订阅一个阻塞的 handler，发 3 条消息，构造 `m := &NatsMod{asm: asm}`，用 200ms 预算调用 `StopWithContext`；然后在 handler 仍阻塞时，以及释放 handler 并等 10s 之后，分别用 5s 预算重试，断言返回 nil 且 `m.asm == nil`。完整源码见 `zz_audit_w0402_test.go`。
- **候选修法**：WANTED 里提的两个方向都可以。(a) `Assembly.Close` 在硬关闭之后返回一个可识别的终态错误（例如用 `errors.Join(ctx.Err(), ErrDrainIncompleteClosed)` 或哨兵错误包装），NatsMod 据此把 `asm` 置空，同时继续上报错误；(b) `DrainWithContext` 在 `IsClosed()` 时把 `ErrConnectionClosed` 当作“已经结束”。注意不要把“未完整排空”报成 nil。
- **等级建议**：P3。资源已经释放，影响是 Stop 永远报错、引用不释放，依赖重试收敛的调用方会卡住。App 本身只调用一次 Stop（`app/app.go:583-602`）。

**附注（文档，不单列缺陷）**：CHANGELOG `## [v1.19.1]` 收录了 NC-30（第 18 行），但版本摘要写的是“行为变化见 RR-20261004-07”，没有把下面两点列为行为变化：NC-30 让过去成功的 Set/Delete 现在返回新错误（含 D1 的情形）；Delete 现在要求 adapter 支持 Eval。另外，NC-30 修复记录里 `registeredKeys` 的行号 `:363` 已经过时（现在是 :384）；`docs/bug/README.md:11` 仍写“未发版”（第 3 行已有总括说明）。

## §5 疑点（没有证实或不能定性）

1. **ReadThroughStore 用 RefHMap 做 L2 时，并发首填会让 Get 失败**：`cache/read_through.go:190-192` 只吸收 `ErrStaleWrite` 和可降级错误，`ErrRefHMapRegistryChanged` 会让 `Get` 失败。这和 RR-04 定下的“回写被拒不让读取失败”的规则不一致。不过仓库里没有正式链路这样组合（ReadThrough 的唯一生产调用在 `entity/remote_snapshot.go:238`，L2 不是 RefHMap），所以没有写红测试。采用 D1 的修法后，这里的同布局情形也会消失。
2. **生成的 Cached ref-hmap DAO 的 Patch 不是原子操作**：`template_redis.go:62-67` 给 Cached 传的 `patcher` 是 nil，所以 Patch 走 `Get（可能读到 L1 副本）→ PatchStructPath → 整条 Set`，也就是读-改-写，可能覆盖其他进程刚写的字段，也会碰到 D1。是否是有意的设计，文档没有写明（IMPLEMENTATION-REFHMAP-LAYOUT 只提到 local/raw 路径）。本次没有实测。
3. **Name 以 `}` 开头时键构造得到空 hash tag**：`cfg.Name="}x"` 会得到 `prefix:{}x:key}…`，整个键参与哈希，Cluster 下 Set 会 CROSSSLOT。生成代码的 Name 是标识符，实际触发不了。属于与 RR-03 已记录的 Prefix 含 `{}` 同类的边界。
4. **nats.go 在 Close 之后把状态从 `CLOSED` 改回 `DRAINING_PUBS`**：这是第三方库的行为，会让 `IsClosed()` 在硬关闭之后短时间返回 false。D2 的修法如果依赖 `IsClosed()`，要考虑这个窗口。

## §6 没做的

- 没有起 Redis Cluster、HA 或 toxiproxy 弱网；同槽只按键的构造推理。
- 没有复测 RR-02/04/05/06/07 的外部效果；没有核实历史 TTL=0 孤儿、17 个环境 skip、N01～N04 的覆盖。
- 没有跑 `codegen/...`、CLI 正式生成消费者、六包 241 叶子的全量复核；只跑了 `./cache/...` 全包、目标回归和根包的层次边界测试。
- 只测了分配次数的前后差，没有测延迟和吞吐，也没有 EVAL 与 EVALSHA 的对比。
- D1 的候选修法只在临时 worktree 里试过绿，没有给同包 fake 补覆盖语义，也没有修订用户文档；D2 没有试修。
- 图谱 generation 是 09-30，没有刷新索引。
