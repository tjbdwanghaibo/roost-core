# Service 第五轮可复跑证据

source HEAD `b336ce62f75ce0d763138bc8523c4695724214a2`。Go 1.27.0、Windows amd64、race；隔离 Redis loopback 16395。七份 `*_test.go.txt` 通过 overlay 注入原包，可复用包内 harness，纯文档不添加正式源码测试。

## 运行

先准备**可丢弃、无持久化**的本机 Redis，使用自己的端口，不要连接生产实例。脚本不启动/停止 Redis，不删除 key；每个真实后端 fixture 用独立前缀。

```powershell
$env:GOCACHE='D:/whb_s/.gocache'
./docs/review/evidence/service-review-20260929-05/Run-Repro.ps1 -Backend memory -RedisAddress 127.0.0.1:16395 -OutputDirectory D:/whb_s/.tmp/review5-memory
./docs/review/evidence/service-review-20260929-05/Run-Repro.ps1 -Backend redis -RedisAddress 127.0.0.1:16395 -OutputDirectory D:/whb_s/.tmp/review5-redis
```

默认 RepositoryRoot 从脚本位置解析；可显式指定另一完整 checkout。脚本保存 overlay、原始 JSONL、RESULTS，并还原环境变量。当前未修源码 go_exit=1 是预期；脚本须确认 9 leaf pass / 12 leaf fail / 0 skip 且无 build-fail 才返回成功。修复之后应改变预期为全绿并保留这版历史证据，不能仅凭脚本非零判断环境坏了。

## 最终执行

| 运行模式 | 叶子通过 | 预期反例失败 | skip | backend 边界 |
| --- | --- | --- | --- | --- |
| memory | 9 | 12 | 0 | Rank 仍连接真实 Redis，其余可切换项 Memory |
| redis | 9 | 12 | 0 | Account/Activity/Chat 和 Rank 真实 Redis；Global/Session/Match 观察 Memory |

共 42 次叶子执行；失败 24 次是 8 非法 Queue + 2 Rank overflow + 1 Activity late opening + 1 Chat prune 各复跑两次。两套均无编译失败/race 警报。父测试事件不再计入叶子。对照包含正常分组、Rank 合法边界、Activity 正常 grace、Account pending 准入/重建、Global migration acquire、Session sweep/资源端幂等、Chat 有限去重和 Match 历史观察。

[Memory 结果](RESULTS-memory.json) · [Redis 结果](RESULTS-redis.json)含失败输出、source_head 与原始日志 hash。初次 Session fixture 参数缺少 State 导致的 build-fail 不计最终结果；修正后两套脚本实际重跑通过。服务重建是同 store 重建对象；时钟和故障均通过可控 fixture，不等同多进程强杀。

[100 路径清单](inventory.csv)记录 blob/复用方式；[原 coverage](COVERAGE.json)记录 freshness 限制；[当前 12 次 RPC 检查](RPC-CHECKS.json)全 exit 0。辅助模板及 harness 路径同样调用过 coverage 并读源码；100 路径响应只表示生产路径核算，不代表 MCP 方法完整或行覆盖率。
