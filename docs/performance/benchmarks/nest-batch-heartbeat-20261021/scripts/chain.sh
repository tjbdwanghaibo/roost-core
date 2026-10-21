#!/usr/bin/env bash
S="$1"
until grep -q "all done" "$S/formal/run.log" 2>/dev/null; do sleep 20; done
mkdir -p "$S/micro" "$S/prof"
"$S/run-micro.sh" "$S" "$S/micro" 10
"$S/run-prof.sh" "$S" "$S/prof"
