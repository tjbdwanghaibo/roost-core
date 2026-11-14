#!/usr/bin/env bash
# B-7 定向入口：复用 alliance-e2e 的真实进程与 scripts/testdata/alliance-e2e/social_test.go.txt。
# 依赖必须是自有 mirror-local 环境（会新建并删除一个 toxiproxy TCP 代理）；日志用 ALLIANCE_E2E_LOGS 保留。
# SOCIAL_REAL_MUTATION=drop-team-reference 只变异临时生成工程；SOCIAL_REAL_MODULE_MATRIX=1 另跑模块矩阵。
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -z "${ALLIANCE_E2E_RUN:-}" ]]; then export ALLIANCE_E2E_EXPECT=3; fi
export ALLIANCE_E2E_RUN="${ALLIANCE_E2E_RUN:-^TestAllianceE2ESocial(Team|Matchmaking|Rehome)$}"
bash "$repo_dir/scripts/test-alliance-e2e.sh"
