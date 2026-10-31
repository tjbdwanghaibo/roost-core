#!/usr/bin/env bash
set -euo pipefail
bash "$(dirname "${BASH_SOURCE[0]}")/testdata/genre-common/run.sh" turn-pvp attack
