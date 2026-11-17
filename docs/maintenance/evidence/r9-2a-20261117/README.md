# R9 批 2A：派对房间改为“装模块 + 只写模块没覆盖的部分”（2026-11-17）

提交：见本目录所在提交（实现、手册、证据同一笔）。依据：[R9 方案](../../PLAN-2026-11-17-r9-genre-modules.md) §3.10，写法照 §9（批 0 塔防样板）。场景名与条数不变：19（3 规则 + 15 Nest / 心跳 / 同步 / 补位 + 1 业务 Robot），0 SKIP。

## 改法

| 块 | 结论 | 做法 |
| --- | --- | --- |
| `Player.Coins`（`persist,sync`，自身视图下发） | 装 wallet | 删夹具字段；领奖 handler 在同一笔里 `Wallet().Add(reward)`（奖励为 0 不调）；“领过没有”仍是座位 `Claimed`，不用 `Apply` 回执；100 万持有上限改读 `Wallet().Balance()`（wallet 不设上限） |
| `Player.Initialized` + `handlerPartyPlayerInit` | 装 enter | 删 handler 与字段；夹具与 REAL 用例改 `sender.NewEnterInitSender(...).Sync_EnterInit`，领奖查 `Enter().Built()`；派对没有初始资产，不写 `EnterFirst` 监听 |
| `Player.Matches` | 手写 | 留在 `PlayerDao` 末尾（`nosync`） |
| PartyRoom（心跳、组同步记分板、房间码、房主、断线宽限、补位、硬期限） | 借鉴（Q3 B） | 不装 room，不并入 `Room`；所以 `Room().Authorize` / `towerMember` 那套身份围栏不适用，房间仍用自己的连接代（借鉴 room 的代际围栏，夹具原样） |
| access player / TCP / self_sync | enter 的 plan 代办 | 脚本删 `add dao Player`、`add access player`、`add transport tcp`，roost.yaml 只在 `self_sync: true` 后追加 `sync_groups: [party_room]` |

另修一处夹具遗漏（与模块无关）：REAL 用例原来只删 `<库>`，`dbscope=sid` 的 `<库>_0` 每跑一次漏一个；照塔防（批 0）补 `DatabaseForSid(h.database, 0).Drop`。复跑后 Mongo 里没有本次的库。

## 文件

- 脚本：`scripts/test-party-room-generated.sh`（`genre_modules=(wallet enter)` + `modules.sh add` / `verify`）。
- 夹具：`scripts/testdata/party-room/handlers.go.txt`、`gameplay_test.go.txt`、`persistence/recovery_test.go`。
- 手册：`docs/framework/PARTY-ROOM-INTEGRATION.md`（§2 状态表、§3.1 步骤、§3.4 两行、新增 §3.5 旧断言 → 新断言、§7、§11）。
- skill reference：`docs/agent-skills/roost-genre/references/party-room.md`（版本分档、§2、§3 命令块、§4 两行、§7）。
- 方案：§9.5 追加 2A 一行。

## 六条验收

| # | 命令 | 结果 |
| --- | --- | --- |
| 1 | `bash scripts/test-party-room-generated.sh` | 19 PASS、0 SKIP；build / vet / `id check` / `generate --check` 过；`module verify: enter 29/29`、`wallet 17/17`，0 skip（[default.txt](default.txt)） |
| 2 | `python3 scripts/test-genres-generated.py --out <scratchpad>/matrix --genres party-room` | `passed`，自定 module 名下 19 / 19，`custom_module_verify` = enter 29、wallet 17（[matrix.txt](matrix.txt)） |
| 3 | `source ~/.roost-it/roost-dataengine-it/env.sh` 后 `ROOST_PARTY_ROOM_REAL=1 bash scripts/test-party-room-generated.sh` | `TestPartyGameRealCrashRecovery` PASS，0 SKIP，WAL 未投影 12 条（与改前相同：建档仍是每人一笔 strict）（[real.txt](real.txt)） |
| 4 | 变异 | 见下（[mutation.txt](mutation.txt)） |
| 5 | `git status`；`GOWORK=off go test -count=1 -run 'Link\|Anchor\|Doc' .` | 见报告（提交后跑） |
| 6 | 手册与 reference | 手册 §3.5 写旧断言 → 新断言；reference §3 命令块接 SKILL.md §3.2 前置 + §3.4 收尾、自定 module `example.com/acme/myparty` 原样抽 bash 块实跑：19 PASS、0 SKIP、module verify 全过、`project doctor` 0 FAIL（[skill-block.txt](skill-block.txt)）；手册 §3.3 的 Go 示例放进生成工程 `go vet` 通过 |

## 改过断言的场景（旧 → 新，详见手册 §3.5）

- `SettlementRewardsOnceAndRoomDestroyed`：`Player.Coins` → `Wallet.Coins`（同样的数 10 / 50 / 10 / 0）。
- `SelfViewAndRoomShareOneConnection`、`BackfillPlayerJoinsGroupWithFullSync`：自身视图的 `coins` 键（`Values()` 扁平 map）→ 生成的 `db.ApplySyncPlayerDao` 合并后读 `Wallet.Value.Coins`；用例改金币从测试 handler 直接 `SetCoins` 改为 `Wallet().Add`。
- 全部 Nest 场景与 REAL：建档 `Sync_PartyPlayerInit` → `Sync_EnterInit`。
- `TestPartyGameRealCrashRecovery`：冷加载 / 第三进程核对 `Wallet.Coins`；另删 `<库>_0`。

## 变异（红 → 恢复绿）

在保留的生成工程里改、跑对应场景：

- M1 领奖不调 `Wallet().Add` → `SettlementRewardsOnce…` 红（`seat 0 coins/matches [0 1], want 10/1`）；REAL 档同一变异 → `TestPartyGameRealCrashRecovery` 红（`rewards [0 0 0]`）。
- M2 夹具不经 enter 建档 → `SettlementRewardsOnce…` 红（领奖被拒）。
- M5 自身视图客户端不按基线合并增量 → `SelfViewAndRoomShareOneConnection` 红（`missing nested baseline`）：证明 wallet 的增量确实只带嵌套补丁，客户端必须按基线 Apply。
- M4 / M4′ wallet 改成不下发 → 编译期就失败（客户端读的是生成的类型化快照 `SyncPlayerDao.Wallet`）。按 1D 口径编译失败不算红，只作对照；这两个场景的运行期红由 M5 给出。
- M3（领奖不查 `Enter().Built()`）、M6（上限不读 `Balance()`）**仍绿**：没有场景覆盖“未建档的人领奖”与“领奖越过 100 万上限”。改前的 `Initialized` 检查与 `Coins` 上限同样没有场景（不是本轮放宽）；按“场景条数不变”没有新增用例，记为未覆盖。

全部恢复后 `generate --check` 通过。

## 场景数

不变：19（规则包 3 + handler 包 15 + Robot 1）；module verify 另跑 enter 29、wallet 17。

## 模块缺口

没有新的（§3.10 已记的 room 广播、matchmaking 弹性人数与补位不在 R9 补）。三条已知注意点（Catalog 另开 Store 顶掉道具表、`room.TimerDriver.SetClock` 串扰、EnterFirst 里发 bag 道具）本组都没碰到：派对不装 bag / room、没有 `configdata` Catalog、没有初始资产。记一条接入注意（已写进方案 §9.5、手册 §3.5、reference §3）：wallet 的余额是嵌套状态，自身视图的增量可能只带嵌套补丁，`SyncPlayerDao.Values()` 答 `apply nested patch before Values`，客户端要按 `db.ApplySyncPlayerDao` 合到基线上再读。

## 拿不准的点

1. **Authorize 不适用**：协调说明要求“新命令一律调 `Room().Authorize`”，但派对房间按 Q3 B 不装 room、不并入 `Room`，房间命令仍走夹具自己的连接代；没有为了调 Authorize 去装 room。
2. **M3 / M6 未覆盖**：要覆盖得给现有场景加断言或加新场景；本轮按“场景名与条数不变”只记录。若要补，最小改法是在 `SettlementRewardsOnce…` 里加一名没建档的玩家领奖被拒、以及把一人余额加到上限附近后领奖被拒。
3. **自身视图测试客户端的读法变了**（`Values()` → `ApplySyncPlayerDao`）：这是读嵌套模块状态的正确写法，不是放宽；`Values()` 对扁平字段仍可用。
4. **遗留的库**：本机 IT Mongo 里还有一个更早的 `roost_party_room_it_90882_95159_0`（不是本组这两轮 REAL 跑出来的；看名字是更早一次派对 REAL 留下的，改前用例不删 `<库>_0`），没有删，留给维护者处理。
5. `SKILL.md` §3.2 要点里仍写“派对房间的 `Coins` 写 `persist,sync`”，不在本组文件范围，没改。
6. 同一检出里 1A / 2C / 2D 等的未提交改动不影响本组生成工程（脚本全绿）；本组只提交上面列的文件。

## 给协调者：CHANGELOG / KNOWN-LIMITS 建议文字

- CHANGELOG `[Unreleased]`：`R9 批 2A：派对房间改为装模块——金币走 wallet、建档走 enter（access player / TCP / self_sync 由 enter 的安装计划代办，只追加 sync_groups）；PartyRoom 按 Q3 B 留手写（心跳、组同步记分板、补位）；自身视图客户端按 ApplySyncPlayerDao 读 wallet 的嵌套余额；场景 19 不变，脚本加跑 module verify，REAL 通过（12 条未投影）；REAL 用例补删 dbscope=sid 的 <库>_0。`
- KNOWN-LIMITS“游戏接入验收”派对房间行：Player 金币 / 建档改为 wallet / enter 模块状态；“未建档领奖”“越过持有上限”两条分支没有场景覆盖（改前即如此）。
- SKILL.md §3.2 要点：“派对房间的 `Board` 写 `nopersist,sync`、`Coins` 写 `persist,sync`”改为只留 `Board`（金币已是 wallet 的状态）。
