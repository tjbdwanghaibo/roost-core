# 发布文档

## 当前版本在哪里看

只有一个来源：[CHANGELOG](../../CHANGELOG.md) 最上面的已归档段 `## [vX.Y.Z] - 日期` 就是最新发布版本，`[Unreleased]` 是 main 上尚未发版的变化。[`codegen/ci/framework-release.yaml`](../../codegen/ci/framework-release.yaml) 的 `release:` 由发版脚本写成同一个号，pretag 与 release workflow 用它核对 tag。生成物要求的最低 core 只看 [`codegen/internal/roost/manifest.go`](../../codegen/internal/roost/manifest.go) 的 `minimumVersions.Core`（每次提高的原因写在它的注释里）。

其他文档不写“当前版本是 vX.Y.Z”，只在描述行为起点时写具体版本（例如“自 v1.32.0 起”）。

## 每个版本写什么（维护者 2026-10-30 决定）

| 版本类型 | 判定 | 要写的文档 |
| --- | --- | --- |
| 补丁版 | `vX.Y.Z`，Z > 0：只修 bug、小改动 | 只有 CHANGELOG 条目，不写本目录的文档 |
| 功能版 | `vX.Y.0` | 一篇 `vX.Y.0-NOTES.md`：升级要求、行为变化、迁移；不再写 IMPLEMENTATION / VALIDATION |
| 大轮次收尾 | 维护者明确要求时 | “说明 + 实现”两篇（同一套条目编号互链），与历次一样 |

功能版的 NOTES 在运行 `scripts/release.sh` 之前写好并合进 main，脚本的前置检查会核对它存在。发版只跑一次 `scripts/pretag.sh`（脚本里已经包含），不重复功能合并时已做过的验证；功能合并时跑了哪些专项验证，写在对应的 CHANGELOG 条目或 NOTES 里。

流程见[维护手册 §5](../maintenance/README.md#5-发版)。

## 本目录已有文件

v1.32.0 及以前每版的 NOTES / IMPLEMENTATION / VALIDATION 按当时规则写成，保持原样，不再改动（逐篇说明见[整理前的文档清单](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DOCUMENTS.md#docsrelease)）；v1.33.0 起只有功能版的 `vX.Y.0-NOTES.md`，按文件名查找。`history/` 是合仓前各仓的只读变更记录。
