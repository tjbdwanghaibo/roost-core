# 复验说明

检出 `59e2d352a`。各探针复用该提交已有的包内测试辅助，去除 .txt 后复制到对应包中。运行 `go test -race -count=3 -v -run '^TestReview' <包路径>`。修前红例的失败是断言检测到表中问题。原生产代码与原测试文件均无需改动。

| 探针 | 包路径 | 结果文件 |
| --- | --- | --- |
| review_merge_scan_test.go.txt | ./service/account | merge-seat.log |
| review_new_seat_test.go.txt | ./service/lobby | merge-seat.log |
| review_transfer_test.go.txt | ./client/relay | transfer.log |
| review_origin_test.go.txt | ./infra/network/httpserver | origin.log |
| review_auto_test.go.txt | ./client/relay | auto.log |

merge-seat.log 仅保留测试名、断言与结果，省去逐角色建号和换服日志。origin.log 首行来自 Node 标准 URL API 对显式 443 的序列化。该项执行一轮，其余故障探针各三轮。lobby 使用 memory 后端。verify.json 与 verify8.json 分别对应主体检出与收尾 portal 提交。ts-old.log 与 lobby-old.log 使用上轮报告已提交的原探针。base.log 和 recheck.log 保留原套件失败记录。
