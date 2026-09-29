# 第六批修复证据与复跑

[修前](BEFORE.json)保留 db642494 基线的 9 pass/6 fail；修后[摘要](RESULTS.json)记录正式定向、完整包测试与原 overlay。所有 raw JSONL 保留在本机忽略目录，不提交缓存/二进制。[实现说明](../../SERVICE-BUGFIX-2026-09-29-06.md)列兼容与未验证边界。

准备独占单机 Redis 和三 master Cluster、确认 16384 slots/cluster_state=ok，再从当前 Core 根执行：

```powershell
$env:ROOST_REDIS_TEST_ADDR='127.0.0.1:16419'
$env:ROOST_REVIEW_REDIS='127.0.0.1:16419'
$env:ROOST_REVIEW_CLUSTER='127.0.0.1:16416,127.0.0.1:16417,127.0.0.1:16418'
go test -race -count=1 -timeout=120s -run 'TestBugfix6|TestClusterKeyPrefix' ./versionstore ./kit/mods ./kit/service/platform ./kit/service/rank
go test -race -count=1 -timeout=180s ./versionstore ./kit/mods ./kit/service/... ./service/...
go vet ./versionstore ./kit/mods ./kit/service/platform ./kit/service/rank
go test -run '^$' ./...
```

缺环境时 integration 默认 skip；本批实际设置地址，test skip=0。记录所有 fail/skip/build-fail，不能凭包 exit 0 认定真实依赖已跑。原第七轮 `.txt` 未修改，overlay 按其 Run-Repro 生成的映射直接 `go test ... -run '^TestReview7'` 修后应 15/15；原脚本保留修前 6 fail 的断言，不把它在修后拒绝计数误报成产品 bug。
