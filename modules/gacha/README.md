# gacha：按配置表权重与保底抽卡，扣金币、发道具、防重放在一笔事务里

玩家用金币在一个卡池里单抽或十连，每一抽得到一个背包道具。概率、价格、保底写在两张配置表里；随机在服务器上产生。它依赖 wallet（金币）与 bag（道具）。

## 能力

### 客户端协议（`protocol/def/gacha_draw.go`）

`GachaDraw`（请求 / 应答）：`GachaDrawRequest{Key, PoolID, Count}`，返回 `Cost`（这次花了多少）、`Balance`（成交时的余额）、`Items`（按抽取顺序的道具号，每项一个道具）、`Misses`（这个池距离上次出保底道具已经多少抽）。

- `Key` 是客户端生成的请求键，1～64 字节，只能是 ASCII 字母、数字与 `-`、`_`、`:`。重试必须用同一个 `Key`、池号、次数。
- `Count` 只能是 1 或 10。
- 玩家身份取自已认证的会话；请求里没有随机种子、权重、价格。

### 服务端入口

- 组件 `GachaComp()`（`game/entities/player/gacha_component.go`）：`Draw(key string, poolID, count int64) (GachaResult, error)`，只能在持有 Player 锁的 Nest 事务里调，错误必须交还 Nest。`GachaResult{Cost, Balance, Items, Misses}`。
- Nest handler `handlerGachaDraw`（`game/handler/gacha_draw.go`，`rollback=undo durability=strict`）；端点 `HandleGachaDraw`（`game/controllers/gacha/gacha_draw.go`）直接分发到它。
- 规则函数在 `game/entities/player/gacha_rules.go`：`gachaRules(poolID)` 在一条请求里读一次配置快照并校验，`gachaPick(drops, roll)` 把一个均匀整数映射到权重区间。

### 登记点

无。抽卡成功没有钩子。

### 状态

- `PlayerDao.Gacha`（类型 `GachaState`，`db/def/gacha.go`）：`Misses` 是池号 → 距离上次出保底道具的抽数。落库并同步给玩家自己。
- `PlayerDao.GachaLedger`（类型 `GachaLedger`）：最近成功抽卡的回执（`Receipts`：请求键 → `GachaReceipt{PoolID, Count, Cost, Balance, Items, Misses, Seq}`）与 `NextSeq`。只落库、不下发。它与 wallet 的请求键账本无关。

### 配置（`configs/schema/gacha_pool.go`、`gacha_drop.go`，`configs/table/gacha_pool.csv`、`gacha_drop.csv`）

- `gacha_pool`：`ID`、`Price`（单抽金币价格）、`PityAfter`（保底抽数）、`PityItemID`（保底道具）、`Off`（关闭这个池）。缺省一个池：单抽 10 金币、10 抽保底、保底道具 1003。
- `gacha_drop`：`ID`、`PoolID`、`ItemID`、`Weight`（非负整数）。缺省池的三行是 1001 / 1002 / 1003，权重 80 / 15 / 5，道具号用的是 bag 自带的。
- 一项的概率是它的权重除以本池总权重；同池同道具可以有多行，概率相加；权重为 0 的道具只从保底产出。

### 错误码（`internal/errors/gacha.go`）

`gacha_key_invalid`、`gacha_key_conflict`（同一个键换了池或次数）、`gacha_count_invalid`（次数不是 1 或 10）、`gacha_pool_invalid`（池不存在、已关闭或配置不合法）、`gacha_overflow`（价格乘次数溢出，或回执序号耗尽）、`gacha_pool_limit`（这名玩家记着保底的池已到上限）。金币不足、背包放不下原样透出 wallet 与 bag 的码。

**日志**：抽卡确认，记录请求键、池、次数、花费、结果和余额。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/gacha/gacha_draw.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做付费。** 只扣 wallet 的金币；没有充值渠道、付费货币与免费货币之分、抽卡券。
- **不做卡牌系统。** 每一抽给一个 bag 道具；没有卡牌实例、重复卡转碎片、图鉴、跨池继承保底、UP 池的小保底、概率公示接口。
- **随机不可验证。** 用标准库 `math/rand/v2` 在服务器上取数，不接受客户端种子，也不提供可验证的随机承诺。失败的尝试没有持久结果，重试会重新抽样；成功的结果由回执固定。
- **保底规则只有一种：** 连续 `PityAfter - 1` 抽没出保底道具，下一抽必出。自然抽到保底道具也清零；十连逐抽判断。
- **数量上限：**
  - 一次只能 1 抽或 10 抽。
  - 每个池至多 256 行掉落；`PityAfter` 至多 1000000。
  - 每名玩家至多记 64 个池的保底（`GachaMaxPools`）。删掉池的表行不会清除玩家在这个池的进度，所以池号发布后不要复用；轮换很多池的游戏要自己设计清理 / 迁移。
  - 回执只保留最近 64 次成功请求（`GachaReceiptWindow`），满了淘汰最旧的。
- **一致性与幂等：**
  - 扣金币、发道具、更新保底、写回执在一笔事务里，任何一步被拒（余额不足、背包满）或提交失败整体回滚，十连不会留下前几抽。
  - 同一个 `Key` 在回执窗口内重发：返回原结果、零写入，即使这个池之后被下架或改了配置。窗口之外的旧键会被当成新请求再扣一次，客户端不得重放旧键。被拒绝的请求不留回执。
  - 应答里的 `Balance` 与 `Misses` 是成交时的快照；最新值以客户端的自身同步视图为准。
- **背包空间的检查是保守的：** 抽样之前按“每种候选道具都可能来 `Count` 个、可能新增 `min(Count, 未持有的候选种类数)` 个种类”检查，含权重为 0 的保底项。因此可能拒绝一次实际放得下的抽卡；这样做是为了不让玩家靠塞满背包、利用“失败不扣费”反复重抽。
- **配置热更：** 新配置对下一次新的抽卡生效，一次十连用同一份快照，旧回执不重算。
- **前提：** 依赖 wallet、bag、nest Mod 与 configdata。只操作一个 Player。模块自己不打开玩家接入与自身视图同步（不装 enter 时见 KNOWN-LIMITS 的“不装 enter 时安装不打开客户端同步”）。
- **已知限制：** `docs/maintenance/KNOWN-LIMITS.md` 里没有单独的 gacha 条目；通用的模块边界见“业务模块目录”“模块的规则大多还是 Go 写的业务文件”。

## 复用方式

- **需求正好是它：** `roost module add gacha`，连带装上 wallet 与 bag。
- **需求比它多一点：**
  - 加池、改价格与概率：改 `configs/table/gacha_pool.csv` 与 `gacha_drop.csv`。保底道具必须也出现在该池的掉落表里（权重可以是 0），道具号必须在 bag 的 `item` 表里，否则这个池整体答 `gacha_pool_invalid`。
  - 改保底规则（软保底、概率递增）：改 `gacha_component.go` 里 `Draw` 的抽样循环与 `gacha_rules.go`。
  - 换扣的货币、改单次抽数：改 `Draw`。不要去掉“重放判断先于配置查询”和“抽样之前检查背包空间”这两处顺序。
  - 改回执窗口、池数上限：`GachaReceiptWindow`、`GachaMaxPools`。
- **别的业务代码要替玩家抽卡**（活动送十连）：在自己的 Nest handler 里（目标实体实现 `player.IGachaEntity`）调 `target.GachaComp().Draw(key, poolID, count)`。它会照常扣金币；要免费抽需要自己改规则。请求键用这件事自己的身份。
- **只适合借鉴的情形：** 奖励不是堆叠道具（英雄实例、带随机属性的装备）、需要监管要求的概率公示与可审计随机、付费直购抽卡时不适合直接用。值得借鉴的是“回执固定成功结果、失败不留结果”与“保守的容量预检查”。

## 联动

无。本模块没有 `with/`，也没有别的模块带 `with/gacha`。

## 验证范围

`roost module verify` 跑 24 条场景（`^TestGacha`，`game/handler` 23 条、`game/entities/player` 1 条），零跳过。场景是离线的：用正式的协议、controller、Nest 加替身提交器，覆盖权重区间的精确边界、单抽与十连、十连内触发保底、自然抽到保底道具清零、各池保底独立、重放返回原结果、同键冲突、非法键与次数、未知 / 关闭 / 配置不合法的池、金币不足零写入、背包满回滚、提交被拒后重试、回执窗口只淘汰最旧、溢出在扣费前拒绝、池数上限、并发重复只扣一次、塞满背包不能用来重抽。

没有经过真实存储与真实 TCP 的验证。仓库一侧另有专项测试 `TestModuleGachaIdleScenariosAndMutations`（验缺省 CSV 与三种可编译变异），它不属于 `roost module verify`。
