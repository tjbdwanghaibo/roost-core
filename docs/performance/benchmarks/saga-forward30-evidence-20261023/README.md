# Saga 正向 30/s 诊断复测

基线 `435f431e` 加本轮证据导出 / ExpectedTerminalTPS 统计；生产 Saga 未改。独占本轮验收资源锁，按已有脚本默认 GOMAXPROCS=4、真实 Mongo / NATS 运行 10 分钟。

```sh
source "$HOME/.roost-it/roost-dataengine-it/env.sh"
ROOST_SAGA_PERF_MODE=forward ROOST_SAGA_PERF_RATE=30 ROOST_SAGA_PERF_DURATION=10m ROOST_PERF_LABEL=review-20261023-forward30-evidence bash scripts/perf/saga.sh
```

**失败样本，不是通过证据。** 18,000 笔全部到终态，其中仅 814 笔正向完成；7,769 补偿，9,415 失败，2 笔 ManualRequired。计入排空后历时 683.03s；所有终态 26.35/s，预期终态 1.19/s。FinalStats.Failed=9,417 包含两笔 ManualRequired，不能再相加。

manual-required.json 保留两笔完整记录：perf-17241、perf-17334，补偿阶段 step=0、attempt=5、last_error="step result timeout"、completed_steps=1。原始 coordinator operation 显示正向 result、补偿 abandoned；业务值仍为 1，每笔只有一次正向 effect，无补偿 effect；全量 effects 的 commits 都为 1。说明本轮有界补偿耗尽，不能删除上限或把人工终态改成成功。

清理前完整导出 `_sagas`、`_saga_operations`、`_saga_completions`、`_saga_outbox`、`perf_steps`、`business`、`effects` 七份 JSON。原始输出已保存到主检出的 `artifacts/perf/saga/review-20261023-forward30-evidence/`（本机绝对路径 `/Users/whb/roost/roost-core/artifacts/perf/saga/review-20261023-forward30-evidence/`）；大日志和全量测试库不提交。该目录由测试创建，不涉及生产数据。原历史三笔记录已经清理，不能以本次两笔推定历史根因相同。
