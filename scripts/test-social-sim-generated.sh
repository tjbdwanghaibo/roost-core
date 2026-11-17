#!/usr/bin/env bash
# 异步社交经营：friends（访客须是主人的好友）+ wallet（收获金币）。不再需要房间：农场是主人 PlayerDao 上的字段。
# 手写（scripts/testdata/social-sim）：种植、成熟时刻、帮助缩短一次、收获（借鉴 friends 的两 Player 事务）。
set -euo pipefail
genre_modules=(friends wallet)
GENRE_MODULES="${genre_modules[*]}" bash "$(dirname "${BASH_SOURCE[0]}")/testdata/genre-common/run.sh" social-sim plant ROOST_SOCIAL_SIM_REAL TestSocialSimRealCrashRecovery
