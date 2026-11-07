# v1.42.1 修复复验证据

见 [复验报告](../../FIX-REVIEW-20261107.md)。目标 main 为 v1.42.1，所有日志来自本轮 Linux 验证。`.exit` 是对应命令的退出码。

`review_scan_transfer_test.go.txt` 复制到正式生成工程的 `internal/rpc/alliance/` 后可按报告复跑，当前基线预期失败。`scan-transfer` 包含三轮失败组和三轮正常对照；使用既有 ownership hook 确定性插入合法并发时序。

`scan-control` 仅作根因定位：通过 Go overlay，在生成工程 transfer.go 的 `report.Moved = append(report.Moved, id)` 之前再插入 `s.picked.drop(id)`，其余不变，同一命令三轮通过。没有将这个实验当作完整修复提交。

生成工程完整回归前移出本轮预期失败探针；其余正式测试未排除。根模块定向完整包包含前轮原探针。Saga/NATS 没开启真实集群门变量。未跑压测、Unity 或完整生产验收。
