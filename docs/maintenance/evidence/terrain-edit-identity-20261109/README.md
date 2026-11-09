# 地形编辑请求身份与判重（T01/T02）证据

设计与结论见[可编辑地形方案第 20 节](../../FEATURE-2026-11-08-editable-terrain.md#20-编辑请求身份与判重t01t02d182026-11-09)。环境：Apple M5，go1.27.3，macOS；生成工程 `roost project new planet -module example.com/planet -template game-demo --skip-deps`，`go.mod` replace 到被测检出。

## 修前红（基线 23247c38）

在基线生成的工程里加入临时用例 [red_test.go.txt](red_test.go.txt)（放进 `internal/service/game/`，不提交到模板），只用修前就有的 API（`Map.Brush`、`terrainmap.EditID`）：

```
go test -count=1 -run 'TestRedT0' ./internal/service/game/
```

原始输出：[red-before.txt](red-before.txt)。四条全部失败，失败落在承诺本身（重放被再次执行 / 不同请求未执行），不是错误文本：

- T01：序号 1 之后同一块又改 40 次，重放 1 得到 `Duplicate:false`、Version 41 → 42、提交数 41 → 42；持久化后重启再重放，同样 41 → 42。
- T02：同一玩家的请求 `2830626994681676340` 与 `6709829656160145507`（`EditID` 都是 3），以及玩家 `2676` 与 `1249291350150932` 的请求 1（`EditID` 都是 `8307342279125212343`），第二个都答 `Duplicate:true`、Version 不变。构造方法：splitmix64 是双射，取一对只差最高位的全值，逆推出请求 ID / 玩家 ID。

## 修后绿

```
go test -race -count=1 -v -run 'TestTerrainBrushReplayBelowTheWindowIsStale|TestTerrainBrushRequestsThatSharedATruncatedIDBothApply|TestTerrainLastSeq|TestTerrainBrushRollback|TestTerrainBrushRefusals|TestTerrainBrushRepeats|TestTerrainBrushQuotaCharges' ./internal/service/game/
```

原始输出：[green-targeted.txt](green-targeted.txt)（窗口外重放、重启后重放、碰撞构造、乱序与重复、回滚不留序号、拒绝不写、续号查询、配额）。新用例里的碰撞对与修前红的玩家对相同。

另在修后工程上做了两处临时改坏（不提交）：把过期当成新请求，`TestTerrainBrushReplayBelowTheWindowIsStale` 报重放 1 被执行（Version 102）；去掉 `Record`，`TestTerrainBrushRepeatsAndReordersApplyOnce` 与 `TestTerrainBrushRequestsThatSharedATruncatedIDBothApply` 报重放未答 `Duplicate`。

真实依赖：私有 `scripts/mirror-local.sh`（独立根目录与端口偏移，用完 `clean`）的环境变量下跑 `scripts/test-demo-generated.sh`，`TestTerrainEditLedgerRealMongo` 与 `TestTerrainRestartRealMongo` 及脚本内其余用例全部通过，原始结果 [real-dependencies.txt](real-dependencies.txt)（只保留用例行）；结论见方案第 20 节“验证”。
