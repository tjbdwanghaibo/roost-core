#!/usr/bin/env bash
# 合作 Roguelike：room（双人席位、代际、连接与在线；GameRoom 并入 Room，Q3 A）+ wallet（通关金币）。
# 手写（scripts/testdata/coop-roguelike）：同时投票、路线种子、三关推进、领奖事实（借鉴 turn 的“每人连续命令号 + 最后一条可重放”）。
set -euo pipefail
genre_modules=(room wallet)
GENRE_MODULES="${genre_modules[*]}" bash "$(dirname "${BASH_SOURCE[0]}")/testdata/genre-common/run.sh" coop-roguelike vote ROOST_COOP_ROGUELIKE_REAL TestCoopRoguelikeRealCrashRecovery
