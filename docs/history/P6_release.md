# P6：合并、发版、去 pin、归档

批次：P6（收敛方案 §4）
状态：**完成**（2026-09-08）
前置：P0～P5 全部完成，B-26 已按方案 B 实施（`P5_acceptance.md` §4.2）

## 0. 发布顺序的修正

方案与 P4/P5 记录写的是"codegen v1.15.0 先发"。实际顺序改为 **core v1.14.0 → kit v1.13.0 → codegen v1.15.0**，原因是 codegen 的 `framework-release` 工作流 gate 作业会按 `ci/framework-release.yaml`（core v1.14.0 / kit v1.13.0）下载并锁定两个模块，consumer-acceptance 也用它们生成工程——tag 不存在则 release 直接红。业务工程的升级器（`roost project upgrade --consolidate`）本身也要 `go get` 到边界版本，codegen 先发并不能让它更早可用。

## 1. roost-core v1.14.0

- `consolidation` → main：merge commit `9fd4d3e`（main 只多一个已 cherry-pick 的 flake 修复 `f556b4d`，合并无冲突）。`scripts/pretag.sh v1.14.0` 在推 main 前通过。
- main CI 首跑 `linux-quality` 红：`TestNestTracePropagatesContextAndRecordsEvents` 在 `-race` 下偶发——worker 先回 `RetChan` 再记 `dispatch_done` 计数，测试读快照可能早于计数。修复 `40154e7`（断言改为 2s 期限轮询；本机 12 次独立 `-race` 运行全绿）。main 与 `consolidation` 同步到 `40154e7`。
- tag `v1.14.0` → `40154e7`（annotated `0a64792`），`gh api …/git/ref/tags/v1.14.0` 确认；tag CI 绿。proxy.golang.org 与 sumdb 数分钟后可解析。

## 2. roost-kit v1.13.0

- `consolidation`：go.mod `roost-core v1.14.0-alpha.5 → v1.14.0`（首次抓取走 `GOPROXY=direct`，proxy 尚未索引），`GOWORK=off` vet / test 25 包全绿，提交 `5ea0db2`；main fast-forward 到同一提交。
- main CI（release-hygiene / released-core ubuntu+windows / local-core-source / integration / service-redis）全绿后 `scripts/pretag.sh v1.13.0` 通过，tag `v1.13.0` → `5ea0db2`（annotated `fd8c54d`），`gh api …/git/ref/tags/v1.13.0` 确认。proxy 索引比 core 慢，等待期间 codegen 的去 pin 提交只在本地。

## 3. roost-codegen v1.15.0

- 去掉三处 Transitional pin（`framework-compat` source-head lane、`ci` release-smoke、`upgrade-compat`）与 `scripts/source-head-check.sh` 的默认 pin（→ v1.14.0 / v1.13.0）。
- 等 proxy.golang.org 的版本列表出现 kit v1.13.0（`.info` 先缓存，`@v/list` 约 40 分钟后刷新，`@latest` 端点更晚；go 命令解析 `latest` 走版本列表，所以列表刷新即可）后推送 `0dda1d2`。分支 CI 四个工作流全绿：`ci`（release-smoke 用 latest 解析）、`upgrade-compat`（v1.11.0 / v1.12.1 旧工程 `--consolidate` 后 `go get latest` 拿到新布局）、`security`、`framework-compat` **六条 lane 全绿**（minimum / released / source-head × minimal / full）——这是 P4 §4 里"发版前必红"的两条 lane 首次转绿。
- `scripts/source-head-check.sh` 修一个脚本 bug：minimal 工程无测试文件时 `grep -v "no test files"` 空输出返回 1，`pipefail` 误报失败（`376540b`）。本地 `minimal` 用正式版默认 pin 通过。
- main fast-forward 到 `376540b`，`scripts/pretag.sh v1.15.0` 通过，tag `v1.15.0` → `376540b`（annotated `14620e4`），`gh api …/git/ref/tags/v1.15.0` 确认。`framework-release` 工作流结果见 §3.1。
- main 随后去掉四个工作流的 `consolidation` 分支触发与 source-head 的分支判断（`d24a6d1`）。

### 3.1 framework-release

运行 34221643481，全程 4 分钟，全绿：gate（tag 校验、release hygiene、`framework verify` 锁定 core v1.14.0 / kit v1.13.0）→ consumer-acceptance（满配置工程 tidy / generate --check / test / actionlint）+ binary-smoke（ubuntu / windows / macos）→ publish（五平台交叉编译、SBOM、SHA256SUMS、provenance attestation）。GitHub Release `v1.15.0` 已发布，8 个资产。

## 4. 归档 roost-skill / roost-service

- README 置顶归档说明（新位置、最后独立版本 v1.10.3 / v1.5.4 仍可 `go get`、升级命令、方案链接）：roost-skill `0451fce`、roost-service `3432a01`。
- `gh repo archive` 两仓，`isArchived=true` 确认。归档后 nightly gap map 的 schedule 自动停止；仓库只读，可随时取消归档。

## 5. 收尾

- 三仓工作流去掉 `consolidation` 分支触发：kit `e6163c5`、codegen `d24a6d1`、core 本提交。`consolidation` 分支保留不删（历史 CI 记录指向它）。
- HANDOFF 导读改为发版后状态；本机 go.work 去掉两个已归档模块。
- 升级路径（给业务工程）：`go install github.com/tjbdwanghaibo/roost-codegen/cmd/roost@v1.15.0` → `roost project upgrade --consolidate --dry-run` 看计划 → 去掉 `--dry-run` 执行 → `go mod tidy && go build ./...`。`roost project deps` 遇到仍 require roost-skill / roost-service 的 go.mod 会自动先改写。

## 6. 遗留

- ~~发版后安静基准归档~~（09-08 完成，`P5_acceptance.md` §4.3，B-26 关闭）。
- ~~P3b：kit Mod 瘦身~~（09-08 完成，`P3b_mods.md`；09-09 发 core v1.15.0 / kit v1.14.0 / codegen v1.15.1）。
- ~~B-14 / B-18 / B-19~~（U-0107 / U-0104 / U-0105+U-0106）。
- 升级器映射表里 `renames` 的 `to: core` 语义现已区分"契约包"与"driver 子包"（`nats.Permanent`），其他拆分包若日后出现类似契约级符号，加进 `renames` 即可。
