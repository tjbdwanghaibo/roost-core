# 第七批 bugfix 复跑

BEFORE.json：干净 bcebb805 上原第八轮红测3pass/5fail。原runner固定working_changes=true不能代表实际修前已改码。ORIGINAL-AFTER.json：相同业务断言8/8pass，仅将写后故障HSet钩子改为真实Eval后丢回复。历史原反例源码不修改。

先用当前 Core CLI 建立新的 game-demo（本轮 module example.com/reviewservice）；无公网访问时在消费模块 go.mod 增加本地 Core replace，再用实际 CLI generate。现有业务文件 write-once，不用 sync 升级它们。本脚本要求三份消费者文件与实际模板经模块替换/gofmt规范化一致，拒绝误测旧消费代码。

```powershell
$env:GOCACHE='D:/whb_s/.gocache'
./docs/bugfix/evidence/service-bugfix-20260929-07/Run-Verify.ps1 `
  -DemoRoot D:/whb_s/.tmp/service-bugfix7-demo `
  -RedisAddress 127.0.0.1:16429 `
  -ClusterAddresses '127.0.0.1:16426,127.0.0.1:16427,127.0.0.1:16428' `
  -OutputDirectory D:/whb_s/.tmp/service-bugfix7-replay

$env:ROOST_REVIEW_REDIS='127.0.0.1:16429'
$env:ROOST_REDIS_TEST_ADDR='127.0.0.1:16429'
$env:ROOST_REVIEW_CLUSTER='127.0.0.1:16426,127.0.0.1:16427,127.0.0.1:16428'
$env:REDIS_ADDR='127.0.0.1:16429'
go test -tags integration -race -count=1 -timeout=180s ./versionstore ./kit/mods ./kit/service/... ./service/...
# 在新消费模块目录执行：
go test -mod=mod -race -count=1 ./internal/service/platform ./game/purchase ./game/handler ./internal/service/game
```

依赖必须是自己的隔离实例；脚本不会启停依赖、改生产代码/消费业务、提交或推送，只写输出和本进程临时环境（还原）。实际 source snapshot见[第九轮摘要](../../../review/evidence/service-review-20260929-09/SOURCE.json)，完整计数/跳过/日志hash见RESULTS.json。框架的所有编译、consumer所有编译与定向vet成功，未发版或验证外部资金/资源/HA。
