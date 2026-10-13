# 可读性整理与长函数拆分的基准对照（2026-10-13，v1.26.0）

同机交替运行改前 / 改后两个测试二进制，`-count=10`，用 benchstat 比较。这里只放 benchstat 摘要；原始样本未随仓。规则见 [维护手册 §8](../../../maintenance/README.md)，逐项结论见 [v1.26.0 实现篇](../../../release/v1.26.0-IMPLEMENTATION.md)。

- `readable-1/`：nest、nestwal、entity、remoteentity、saga、gameplay 的注释与步骤拆分（`nest.benchstat.round2.txt` 是 `maps.Clone` 带来 allocs 退化、随后撤回的那一轮）。
- `readable-2/`：dataengine 回放、entitysync Flush、syncbus `syncMsgID`。
- `longfn/`：11 个长函数拆分；`bus-messagePackValueEnd` 退化 132%，拆分已撤回。
