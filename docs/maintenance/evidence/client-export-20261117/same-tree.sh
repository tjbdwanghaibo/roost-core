#!/usr/bin/env bash
# 没有声明 client_export 的工程：新旧生成器各走一遍 project new → module add → add protocol → generate，对比整棵树。
# 用法：same-tree.sh <roost 二进制> <输出目录名>
set -euo pipefail
roost="$1"
name="$2"
S=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/b1117-client-export
dir="$S/$name"
export GOWORK=off
rm -rf "$dir"
"$roost" project new same -module example.com/studio/same -out "$dir" -template game --skip-deps > "$dir.new.log" 2>&1
cd "$dir"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=/Users/whb/roost/roost-core"
"$roost" module add wallet bag enter > "$dir.add.log" 2>&1
"$roost" add protocol HelloPing --group game --handler hello > "$dir.proto.log" 2>&1
cat > protocol/def/hello_ping.go <<'EOF'
//go:build protocoldef

package protocoldef

type HelloPingRequest struct {
	Nonce int64  `pb:"1"`
	Note  string `pb:"2"`
}
type HelloPingResponse struct {
	Code   int32  `pb:"1"`
	Reason string `pb:"2"`
	Nonce  int64  `pb:"3"`
}

//roost:protocol group=game handler=hello
type HelloPingProtocol interface {
	//roost:msg id=10005
	HelloPing(HelloPingRequest) HelloPingResponse
}
EOF
"$roost" generate > "$dir.gen.log" 2>&1
"$roost" project sync > "$dir.sync.log" 2>&1
"$roost" generate --check > "$dir.check.log" 2>&1
echo "done $name"
