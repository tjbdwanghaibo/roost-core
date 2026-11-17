#!/bin/zsh
# 用法：mutate.sh <文件> <原文> <替换> <go test 参数...>
# 在文件里把原文换成替换（只换一处），跑 go test，打印结果，最后恢复文件。
set -u
file=$1; from=$2; to=$3; shift 3
cd /Users/whb/roost/roost-core/.claude/worktrees/agent-ac148c8f254c0ab00
cp "$file" "$file.mutbak"
python3 - "$file" "$from" "$to" <<'EOF'
import sys
p, a, b = sys.argv[1:4]
s = open(p).read()
if a not in s:
    print("MUTATION TARGET NOT FOUND:", a); sys.exit(2)
open(p, 'w').write(s.replace(a, b, 1))
EOF
if [ $? -ne 0 ]; then mv "$file.mutbak" "$file"; exit 2; fi
echo "=== mutation: $file: [$from] -> [$to]"
GOWORK=off go test -count=1 "$@" 2>&1 | grep -E "^(--- FAIL|FAIL|ok|PASS)|_test.go:[0-9]+:" | head -12
mv "$file.mutbak" "$file"
