# 本轮证据说明

目标为 main v1.42.0，报告见 [审查收尾](../../FEATURE-REVIEW-20261107.md)。日志来自 Linux 本机；目录沿用任务启动时的 20261106，报告按收尾的北京时间 2026-11-07 记录。`.exit` 为对应命令退出码。

- `review_*.go.txt` 是预期失败探针，不作为交付分支的普通测试自动执行。按报告复制到对应 main 或生成工程中复跑；不要覆盖接收方已有文件。
- `transfer-war`、`effect-stop`：确认缺陷的三轮复现；后者含 workers=2 正常对照。
- `root-race`：根模块完整命令原始失败输出；`root-gates-repeat`：修正临时探针格式后根包通过。基础测试用 overlay 将旧的未跟踪 review 探针替换为空包文件，未排除已跟踪测试。
- `generate`、`demo-race`：正式生成工程以及全量 race（新增缺陷探针在基础回归后加入）。
- `dotnet`、`cpp`：首次环境受限记录；`dotnet-retry`、`native-direct`：可用执行方式下的实际结果，不隐藏首次失败。
- `alliance-real`：默认七项真实进程功能回归，六项通过，一项端口冲突；`alliance-repeat`：仅补跑该项通过；`deps*-down`：清理隔离依赖的记录。
- `changed-files`：审查区间完整文件清单，只表示变更范围，不表示每行均人工审核。

C++ 直接验证的等价复跑方式（从目标 core 仓库运行，输出路径任选）：

```bash
g++ -std=c++17 -O1 -pthread -Iclient/cpp/include \
  client/cpp/src/client.cpp client/cpp/src/lockstep.cpp \
  client/cpp/src/snapshot_digest.cpp client/cpp/src/lockstep_c.cpp \
  client/cpp/tests/smoke.cpp -o /tmp/roost-review-native-smoke
python3 client/cpp/tests/run.py /tmp/roost-review-native-smoke
```

这不是完整 CMake/CTest 或 Unity 构建验证。没有运行压测，也没有改动历史压测脚本。
