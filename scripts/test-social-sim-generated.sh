#!/usr/bin/env bash
set -euo pipefail
bash "$(dirname "${BASH_SOURCE[0]}")/testdata/genre-common/run.sh" social-sim plant ROOST_SOCIAL_SIM_REAL TestSocialSimRealCrashRecovery
