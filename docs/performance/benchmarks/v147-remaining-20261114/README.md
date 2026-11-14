# v1.47 遗留性能测量（2026-11-14）：NOT RUN

**M1–M19、E1、E3 和 B2 规模测量尚未开始，正式调度调用为 0 / 117。** 当前仅有入口审计、二进制准备和既有功能证据，不能给出本批容量、吞吐或达标结论。用户要求交接给下一位 agent 后再继续，本次没有启动性能窗口。

完整的16段计划、源码/二进制基线、M13修后hash、隔离依赖、30分钟截止、失败停止、续跑选项和可复建命令见[准备交接与入库调度器](../../../maintenance/evidence/v147-performance-preparation-20261114/README.md)。执行前先读[最终验证与性能总交接](../../../maintenance/HANDOFF-2026-11-14-v147-validation-performance.md)。

B3实验 HEAD `300be3a0` 的[报告副本](../../../maintenance/evidence/checkpoint-experiment-20261114/README.md)已保存；实验代码没有合入发布树，M19仍待测。E2已有独立[200轮未复现记录](../../../maintenance/evidence/v147-native-weak-20261114.md)，其实际负载范围不能当作本批正式性能环境。

后续每格应追加原始命令、exit、样本数、环境、成功/失败/未知、一致性与延迟/吞吐，并明确网络和持久层边界；保留失败及续跑目录。脚本通过、微基准通过和生产容量达标是不同结论。
