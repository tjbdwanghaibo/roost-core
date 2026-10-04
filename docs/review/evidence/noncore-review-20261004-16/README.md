# N04第四批审查证据

起点`ce90e90d`，同树NC-21～25已修；[13叶子7fail/6控制](review.jsonl)对应[NC-26～29四个未修P3](../../../bug/REVIEW-2026-10-04-noncore-16.md)。[退出1](exits.json)是实质行为断言失败，不是编译错误、超时或数据竞争。探针通过公开集合API/真实BSON codec，事务用channel控制A写→B提交→A abort，无任意sleep。本目录只需Go，不需要外部Mongo。

```powershell
./Run-Review.ps1 -GoExecutable <go.exe> -RepoRoot <repo> -OutputRoot <new-dir>
```

[Run-Review](Run-Review.ps1) overlay加入[review16_test.go.txt](review16_test.go.txt)，`go test -race -count=1 -timeout=90s -json -run '^TestReview16MongoBoundaries$' ./mongo/mongotest`。保留结果与对照，不改正式产品文件，不把expected-red作为正常包门禁。

[inventory.csv](inventory.csv)为固定40Go候选，不含新tests及所有非Go资源；38个文件按前轮WorkingSHA256复用，RefHMap/mongotest按未变范围复用并读当前diff/相关源码。Blob与SourceHead为本轮起点，WorkingSHA256为修后当前树，非同一个版本口径。[45路径hash](source-hashes.csv)与[coverage](coverage.json)。Tier2 roost-core generation2026-09-30T11:58:14Z，metadata_changed/not_tracked靠源码补证；无gap仅是best-effort信号，未声称刷新共享图谱或全业务100%。

正式修复回归与消费者统计见[红绿summary](../../../bugfix/evidence/noncore-bugfix-20261004-08/summary.json)；本轮未重复Service十域review、未改核心三模块。真实Mongo、Redis Cluster/HA/长稳与性能另留项。
