# quota：权威宿主内的共享消费额度

`roost module add quota` 只给 **Player** 安装组件。两个玩法消费同一名玩家的同一规则，就是从同一个余额扣除；换玩家不会借到另一人的额度。要给已有联盟实体加权威账本，显式 `roost module add quota_alliance`，见[联盟宿主](../quota_alliance/README.md)。单装 `alliance` 和 `quota` 不会隐式接通它们。

## 在服务端玩法里使用

`view.QuotaHolder.Quota()` 提供三个方法，数量和 ID 参数都是 `int64`：

| 方法 | 语义 |
| --- | --- |
| `Remaining(ruleID) (int64, error)` | 只读投影当前可用量；不存在/错宿主返回错误，不把未知当作不限量。 |
| `Check(ruleID, amount) error` | 只读，不写账、不预留；通过不保证以后能消费。 |
| `Consume(ruleID, amount) error` | **独立原子检查并扣额**，不必先 `Check`。正数且不超过剩余才写入。 |

三个方法都要求当前 Nest handler 持有该宿主**实例**的锁；`Consume` 还要求当前事务是 `rollback=undo`（即失败撤销），否则返回 `quota.ErrHost`，不修改 DAO。处理错误时返回给外层 Nest，不要在部分业务完成后吞错。例如两个入口分别调用 `p.Quota().Consume(1, n)`，不要各建计数器：

```go
// 运行在目标 Player 的 rollback=undo Nest handler 中。
if err := p.Quota().Consume(1, n); err != nil {
    return err
}
return applyPurchaseInThisTransaction(p, n) // 失败和次数同退；外部副作用不能靠 DAO 撤销
```

`quota.ErrRule`：未知/错宿主/不可用配置；`ErrAmount`：数量非正；`ErrLimit`：不足（含加法会溢出的请求）；`ErrState`：已有记录与周期不兼容。各错误可用 `errors.Is` 识别。无通用扣次协议、RPC、玩家自报时刻或请求流水。**重复成功调用就是重复消费**；请求重试、授权、防重及先前请求参数比对均由玩法方负责，在同一宿主事务里保存自己的业务回执。不要先向一个服务查余额再到另一个服务修改资产，当作同一事务。

## 配表与周期

`configs/table/quota.csv` 列为 `id,scope,period,limit`，编辑后运行 `roost generate`。示例 1 是 Player 每业务日 5 次，2 是联盟每业务周 20 次。规则 ID 为全表唯一的 `1..4096`，`scope` 为 `player` 或 `alliance`，`limit` 为 `0..MaxInt64`（0 即暂时禁止消费）。同一事务使用框架固定的配置快照和 `fctx.Now()`，没有客户端时间参数。

| period | 换期条件 |
| --- | --- |
| `daily` | 进程 `time.business_timezone` 与 `time.business_day_start` 划分的下一业务日期。 |
| `weekly` | 下一个业务周的周一，在同一业务换日时刻开始。按日历算，不按 168 小时算。 |
| `lifetime` | 永不换期。 |

只在成功消费时把新期和已用数写入 `PlayerDao.Quota`（`persist,nosync`）。查询即便跨期也不脏写；拒绝不创建记录。时钟回拨时使用最后一次**已消费**期次的已用数，不退回较早期次，追平时不会再送一次预算。降上限只使余额降到最少 0，不清除超出新上限的已用数；再提高上限仍扣除真实已用。下架也不删账。ID、用途、宿主和周期是长期身份：改期请用新 ID，不重用旧号；已有记录遇到不同周期会拒绝消费。

每个宿主最多 4096 条记录，每规则一条，不积累历史周期或调用流水。没有后台定时器、逐点恢复、退款、跨宿主借额；不替换或迁移 shop/stamina 等模块已有私账。

## 验证

`roost module verify` 在生成工程验证 `^TestQuotaPlayer` 六项：共享预算与竞争、上下文和数量边界、业务周日界与回拨、热更、业务/提交失败回滚、持久提交恢复。联装入口和增量矩阵见[联盟宿主](../quota_alliance/README.md)。
