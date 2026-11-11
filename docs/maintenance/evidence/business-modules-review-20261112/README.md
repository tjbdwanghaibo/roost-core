# 业务模块新增玩法审查证据

被测 feature：`64fb2fff5c31be7a167e09cae133ad12a81fc814`，Linux amd64 / Go 1.27.2 / GOWORK=off / GOMAXPROCS=4。报告日期为北京时间 2026-11-12，日志使用 UTC 2026-11-11。固定审查分支仅存报告，**复现需要检出被测 feature**。

两个缺陷探针在此版本应退出 1：各三轮实验失败、三轮控制通过，无 race 报告。其余验证通过。`.go.txt` 是证据，不参与固定分支构建。

## 生成与基线验证

将 CORE 设置为上述 feature 的干净检出绝对路径，WORK 为新建可写目录，EVIDENCE 为本目录绝对路径。CLI、模板与依赖必须同一提交。

```bash
export GOWORK=off
cd "$CORE"
go build -o "$WORK/roost" ./codegen/cmd/roost
"$WORK/roost" project new audit -module example.com/businessaudit -out "$WORK/project" -template game --skip-deps
cd "$WORK/project"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE"
git init -q .
"$WORK/roost" module add wallet checkin chat enter profile bag leaderboard mailbox task stamina shop skillbook room turn rehome actor scene monster attribute equipment dungeon naming cdkey notice gacha friends idle terrain combat
"$WORK/roost" module verify --json
```

`verify.json`：29 模块 + 14 联动分组，共 598 顶层场景通过，0 skip；build、vet、id、生成一致性四项成功。运行于加入审查探针之前。

```bash
cd "$CORE"
go test -race -count=1 -timeout 5m ./framework/nest ./gameplay/skill/combatcomponent ./codegen/internal/nest

cd "$WORK/project"
go test -race -count=1 -timeout 5m \
  -run '^Test(Terrain|Combat|Actor|Attribute|Equipment|Gacha|Idle|Friends|Scene|Monster|Naming|CDKey|Cdkey|Dungeon|Notice)' \
  ./game/handler ./game/controllers/terrain ./game/controllers/scene \
  ./game/monster/service ./game/controllers/naming ./game/controllers/cdkey \
  ./game/controllers/dungeon ./game/notice ./game/controllers/friends ./game/entities/player
```

对应 `foundation-race.log`、`new-modules-race.log`。后者是指定名称的定向测试，不包含 TestReview，也没有声称包含全部 TestWith 联动场景。

## RV-20261112-BM-01：地形恢复代际复用

```bash
cd "$WORK/project"
cp "$EVIDENCE/review_projection_test.go.txt" game/controllers/terrain/review_projection_test.go
go test -race -count=3 -timeout 3m -run '^TestReviewTerrainRecoveryGenerationABA$' -v ./game/controllers/terrain
```

`terrain-aba.log` 保留原始失败日志。使用原模块的 newTerrainFixture：正式 Controller/Nest/DAO/Pack/World/AfterCommit，一次完整 Runtime.Refresh；提交器、同步传输为原夹具内存替身。先注入建筑发布失败，再恢复一次，然后注入新的移除发布失败；实验组执行先前捕获代际的 Recovered，控制组不执行。**第二个 Refresh 只模拟其最后 Recovered 步骤，不是两个完整 Refresh 的并发端到端测试。** 此步骤可以在生产 Refresh 最后一行之前被调度暂停，且方法没有恢复互斥保护。

关键断言：持久账本仍是 sequence=2、building count=0，重复移除不再发布投影，但实验组 pending=false。修后应两个分组都通过。该问题是状态代际错误，race 无数据竞争不能排除。

## RV-20261111-BM-04：旧问题复验

```bash
cd "$WORK/project"
cp "$EVIDENCE/review_conclusion_test.go.txt" game/rehome/review_conclusion_test.go
go test -race -count=3 -timeout 3m -run '^TestReviewLostCommitAfterLaterAbort$' -v ./game/rehome
```

`rehome-still-open.log`：直接沿用前轮探针，未修改。真实 account.Service + MemoryStore、正式 Driver/LocalResidency；Profile、断连、交出是模块原测试替身。Commit A 已成功但应答丢失，后续 Begin B/Abort B 覆盖最近结论，重试 A 被误判中止，held/tracked 消失、旧 claims Admit 为 nil。控制组无第二笔换服则拒绝旧 claims。没有证明两个真实进程已经双写生产数据库。

BM-05 本轮只核对相关源码未变，未重新跑 demo；其动态证据请查前轮目录。没有新的压测，历史压测材料保持原样。
