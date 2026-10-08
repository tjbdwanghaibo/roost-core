# macOS/Linux观察项与CBM恢复收口

基线d30c3f02；维护者2026-10-08明确只保证macOS/Linux，Windows问题不处理。针对v1.23.1阶段清单新合入的Remote/KCP观察继续定位；本轮不重新定义Remote唯一outbox发布者、生产部署或24小时长稳范围。

| 项 | 结论 |
| --- | --- |
| KCP停止context canceled | RR-20261008-40，测试夹具的依赖释放顺序错误；受控复现后修复，业务错误控制保留 |
| Remote按需读v1/v2及并发v59/v60 | RR-20261008-41，原验收没有保证已超过陈旧期限；正式SnapshotClient冻结时钟复现并补期限内/边界/过期断言，并发最终收敛改成明确推进时间 |
| Windows文件占用/换行等 | 按维护者要求排除，不处理，也不作为macOS/Linux发布门槛 |
| CBM刷新 | 孤儿socket阻塞已恢复，首次成功刷新generation=2026-10-08T01:05:03Z；最终main提交后再刷新并记录覆盖 |

## CBM恢复证据

原0.10.8后台PID3904未持有有效协调状态；停止后两个旧会话也退出，但原socket和anc仍存在、缺identity、无进程持有、连接ECONNREFUSED。与[上游2107](https://github.com/DeusData/codebase-memory-mcp/issues/2107)记录一致。持有全部7个协调锁排除并发后，仅将两枚socket重命名至`/private/tmp/cbm-orphans-20261008`；保留数据库和锁文件。官方CLI index_repository成功（25134节点/243664关系、0 skipped、5模板partial），daemon start启动常驻服务。后续当前会话通过官方CLI执行search/trace/snippet/coverage；旧stdio连接需客户端重新连接，不冒认旧连接仍存活。

## 验证与证据

使用Go1.27；macOS目标3场景race×30、4相邻包race×3通过。Linux使用官方golang:1.27镜像（Go1.27.1 linux/arm64），只挂载本轮单仓临时副本和新缓存；不暴露其他仓库或用户配置。此前整目录挂载请求被自动审批拒绝，已缩小为上述范围后执行。

完整平台结果：macOS与Linux/arm64均完成全仓build/vet/test，131个测试包通过（22个无测试包）。macOS相邻4包race×3；两平台目标场景race×30，Linux实际KCP网络race×3。Linux官方镜像digest为`sha256:162be5298a40ed317005c8339c6de4d10d3eef336d66dc8e9259b03ab9d3a6d2`，本轮未安装.NET/真实数据库，Linux中的相关外部集成仍按既有条件跳过；macOS全仓包含既有生成工程验证。本轮未新增生产变更，无需重复v1.23.1已通过的21格依赖故障矩阵。

旧MCP stdio连接在停掉旧CBM会话后已关闭；当前会话用官方CLI验证，后续MCP客户端需重新连接。索引数据库和常驻daemon已恢复；不把当前连接的Transport closed写成索引仍旧或刷新失败。日志保存主检出`artifacts/perf/mac-linux-followup-20261008/`；保留原红测，不复制凭据。历史Windows日志保留，不以本轮覆盖全部外部场景。
