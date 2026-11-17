#!/usr/bin/env bash
# client_export 第 4 节验收（2026-11-17）。用法：acceptance.sh <roost 二进制> <证据输出目录>
# 新建工程 → 装 wallet bag enter → 手写一个协议 → 声明 client_export → roost generate，然后逐条核对。
set -uo pipefail
roost="$1"
out="$2"
S=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/b1117-client-export
P31=/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/protoc-31.1/bin/protoc
dir="$S/acc"
export GOWORK=off
mkdir -p "$out"
log() { echo "$@" | tee -a "$out/acceptance.txt"; }
step() { log ""; log "### $*"; }
rm -f "$out/acceptance.txt"
rm -rf "$dir" "$S/acc-client"

step "环境"
log "roost: $("$roost" version)"
log "protoc(PATH): $(protoc --version)  protoc(31.1): $("$P31" --version)  dotnet: $(dotnet --version)"

step "1. 新建工程、装模块、手写协议"
"$roost" project new acc -module example.com/studio/acc -out "$dir" -template game --skip-deps > "$out/1-new.log" 2>&1 || { log FAIL new; exit 1; }
cd "$dir"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=/Users/whb/roost/roost-core"
git init -q . && git add -A && git -c user.email=a@example.com -c user.name=a commit -qm base
"$roost" module add wallet bag enter > "$out/1-module-add.log" 2>&1 || { log FAIL module add; exit 1; }
"$roost" add protocol HelloPing --group game --handler hello > "$out/1-add-protocol.log" 2>&1
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
cat >> roost.yaml <<'EOF'
client_export:
    dir: shared/protocol
    languages: [csharp]
EOF
log "roost.yaml 末尾："; tail -4 roost.yaml | tee -a "$out/acceptance.txt"

step "2. protoc 版本不对时报错（PATH 上是 36.2）"
"$roost" generate > "$out/2-wrong-protoc.log" 2>&1; rc=$?
log "rc=$rc"; tail -1 "$out/2-wrong-protoc.log" | tee -a "$out/acceptance.txt"
if [ $rc -eq 0 ] || ! grep -q "needs protoc 31.1" "$out/2-wrong-protoc.log"; then log "FAIL: wrong protoc accepted"; fi
if [ -e shared/protocol ]; then log "FAIL: export dir written despite wrong protoc"; else log "导出目录未写出（在任何生成器之前失败）"; fi

export ROOST_PROTOC="$P31"
step "3. roost generate（ROOST_PROTOC=protoc 31.1）"
"$roost" generate > "$out/3-generate.log" 2>&1; log "rc=$?"
go build ./... && log "go build ./... ok"
git add -A && git -c user.email=a@example.com -c user.name=a commit -qm export
log "导出目录："; (cd shared/protocol && find . -type f | sort | xargs ls -l | awk '{print $5, $9}') | tee -a "$out/acceptance.txt"
cp shared/protocol/manifest.json "$out/manifest.json"

step "4. 两次生成逐字节相同（--force 重写一次，再比）"
rm -rf "$S/acc-export-1" && cp -R shared/protocol "$S/acc-export-1"
"$roost" generate --force > "$out/4-generate-force.log" 2>&1; log "rc=$?"
diff -r "$S/acc-export-1" shared/protocol && log "导出目录逐字节相同"
log "git status（整个工程）：$(git status --short | wc -l | tr -d ' ') 个文件有变化"
"$roost" generate --check 2>&1 | tee -a "$out/acceptance.txt"

step "5. manifest 的消息号、错误码与 msgid_gen.go、internal/errors 一致"
python3 -I - "$dir" <<'PY' 2>&1 | tee -a "$out/acceptance.txt"
import json, re, sys, os, glob
root = sys.argv[1]
m = json.load(open(os.path.join(root, 'shared/protocol/manifest.json')))
msgid = open(os.path.join(root, 'protocol/msgid/msgid_gen.go')).read()
go = dict((n, int(v)) for n, v in re.findall(r'\bMsg(\w+)\s+uint32 = (\d+)', msgid))
mine = {}
for e in m['messages']:
    if e['direction'] in ('request', 'push'):
        mine[e['name']] = e['id']
    if e['direction'] == 'response':
        assert any(x['direction'] == 'request' and x['name'] == e['name'] and x['id'] == e['id'] for x in m['messages']), e
assert mine == go, (mine, go)
print('messages: %d entries, %d ids match msgid_gen.go exactly' % (len(m['messages']), len(go)))
codes = []
for path in sorted(glob.glob(os.path.join(root, 'internal/errors/*.go'))):
    for code, name, msg in re.findall(r'errcode\.Define\((\d+), "([^"]+)", "((?:[^"\\]|\\.)*)"\)', open(path).read()):
        codes.append((int(code), name, msg.encode().decode('unicode_escape') if '\\' in msg else msg))
codes.sort()
got = [(e['code'], e['name'], e['message']) for e in m['error_codes']]
assert got == codes, (got, codes)
print('error_codes: %d entries match internal/errors exactly' % len(got))
cs = open(os.path.join(root, 'shared/protocol/csharp/MessageIds.cs')).read()
for name, ident in go.items():
    assert re.search(r'(Request|Push)%s = %d;' % (name, ident), cs), name
print('MessageIds.cs has every id')
PY

step "6. doctor 的 client-export:fresh"
"$roost" project doctor > "$out/6-doctor.log" 2>&1
grep "client-export:fresh" "$out/6-doctor.log" | tee -a "$out/acceptance.txt"

step "7. 只改 protocol/def、不重新生成：generate --check 与 doctor 都报过期"
sed -i '' 's/	Note  string `pb:"2"`/	Note  string `pb:"2"`\
	Extra int32  `pb:"3"`/' protocol/def/hello_ping.go
git diff --stat | tee -a "$out/acceptance.txt"
"$roost" generate --check > "$out/7-check-stale.log" 2>&1; log "generate --check rc=$?"; tail -1 "$out/7-check-stale.log" | tee -a "$out/acceptance.txt"
"$roost" project doctor > "$out/7-doctor-stale.log" 2>&1; log "doctor rc=$?"; grep "^FAIL  client-export:fresh" "$out/7-doctor-stale.log" | tee -a "$out/acceptance.txt"
git checkout -q protocol/def/hello_ping.go

step "8. C#：导出的 csharp/ + Roost.Client + Roost.Client.Codecs（Google.Protobuf 3.31.1），netstandard2.1"
(cd "$S/cs" && rm -rf bin obj && dotnet build -p:ExportDir="$dir/shared/protocol" -nologo -v q > "$out/8-dotnet-build.log" 2>&1; log "dotnet build rc=$?"; tail -5 "$out/8-dotnet-build.log" | tee -a "$out/acceptance.txt")
log "工程：$S/cs/Export.csproj（Nullable enable、TreatWarningsAsErrors、LangVersion 8）"

step "9. roost client sync"
"$roost" client sync --from "$dir" --to "$S/acc-client/Assets/Protocol" 2>&1 | tee -a "$out/acceptance.txt"
cat >> protocol/def/hello_ping.go <<'EOF'

type HelloWaveRequest struct {
	Times int32 `pb:"1"`
}
type HelloWaveResponse struct {
	Code int32 `pb:"1"`
}

//roost:protocol group=game handler=hello
type HelloWaveProtocol interface {
	//roost:msg id=10006
	HelloWave(HelloWaveRequest) HelloWaveResponse
}
EOF
cat > internal/errors/hello.go <<'EOF'
package errors

import "github.com/tjbdwanghaibo/roost-core/infra/base/errcode"

var ErrHelloTooMany = errcode.Define(100100, "hello_too_many", "wave at most 3 times per call")
EOF
sed -i '' 's/"not enough coins"/"not enough coins left"/' internal/errors/wallet.go
"$roost" generate > "$out/9-generate-2.log" 2>&1; log "generate rc=$?"
"$roost" client sync --from "$dir" --to "$S/acc-client/Assets/Protocol" 2>&1 | tee -a "$out/acceptance.txt"
ls "$S/acc-client/Assets/Protocol" | tee -a "$out/acceptance.txt"

step "10. 配 csharp.namespace: Game.Protocol 再生成、再编译"
cat >> roost.yaml <<'YAML'
    csharp:
        namespace: Game.Protocol
YAML
"$roost" generate > "$out/10-generate-namespace.log" 2>&1; log "generate rc=$?"
grep -h "^namespace\|csharp_namespace" shared/protocol/csharp/*.cs shared/protocol/proto/*.proto | sort | uniq -c | tee -a "$out/acceptance.txt"
(cd "$S/cs/ns" && rm -rf bin obj && dotnet build -p:ExportDir="$dir/shared/protocol" -nologo -v q > "$out/10-dotnet-build-namespace.log" 2>&1; log "dotnet build rc=$?"; grep -E "警告|错误|Warn|Error" "$out/10-dotnet-build-namespace.log" | tail -2 | tee -a "$out/acceptance.txt")
log "done"
