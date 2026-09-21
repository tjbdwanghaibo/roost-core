# scripts/

仓库级的工具，**每个只有一份**。合仓之前 core / kit / codegen 各带一套，
其中 `gapmap.sh` 三份逐字节相同、`pretag.sh` 三份各不相同却只有一个还能用
（另外两个仓已归档，发不了版）。整体检查时收敛成这里的一份（2026-09-21）。

| 路径 | 做什么 | 谁在用 |
| --- | --- | --- |
| `pretag.sh <version>` | 发版前的门禁：tag 与 module 主版本一致、tag 未存在、无 replace、工作树干净、`GOWORK=off` 下 build/vet/tidy/test 全过、发布清单声明的版本等于要打的 tag（U-0270） | 人工在打 tag 之前跑；`.github/workflows/release.yml` 在 tag 之后再校验一次清单 |
| `gapmap.sh [--max N] [pkg...]` | 覆盖率采样：按包回退断言看测试是否真的会红，产出 `gapmap-report.md` | `.github/workflows/nightly-gapmap.yml` |
| `gapmap/` | 上面那个脚本的 Python 部件（`classscan.py`、`revertsample.py`） | `gapmap.sh` |
| `perf/` | 基准对比脚本（同机 before/after） | 人工 |
| `consolidation/` | 合仓期间的搬迁辅助（`merge_kit_pkg.py`、`split_driver.py`） | 已完成的迁移，保留备查 |

另外两处 `scripts/` 是**层内**的，不重复：

- `kit/scripts/integration/` — kit 服务的集成环境（brew 起的 Redis / Mongo 副本集 / NATS，见 `dataengine-env.sh`）。
- `codegen/scripts/` — 生成器自己的运行时校验（`*-runtime.sh` 把生成物编译进真运行时跑一遍）、
  `source-head-check.sh`（本地版 framework-compat）、`install-windows.ps1`。
