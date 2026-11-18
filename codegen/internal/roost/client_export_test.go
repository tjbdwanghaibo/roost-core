package roost

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// client_export（维护者 2026-11-17，docs/maintenance/FEATURE-2026-11-17-client-export.md；TS 见
// FEATURE-2026-11-18-ts-gate-client.md）的回归用例。单测用假的 protoc（shell 脚本：--version 答指定版本，--csharp_out 把
// proto 原文写成 .cs，--es_out 先问插件 --version 再把 proto 原文写成 *_pb.ts）与假的 protoc-gen-es；真 protoc 31.1 的
// C# 编译验收见 docs/maintenance/evidence/client-export-20261117/，TS 的真实工具链见 TestClientExportTSWithTheRealToolchain。

// TestSDKGoogleProtobufVersionMatchesCodecs：生成器核对 protoc 用的 Google.Protobuf 版本必须就是 C# SDK 锁定的那个。
func TestSDKGoogleProtobufVersionMatchesCodecs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "client", "dotnet", "Roost.Client.Codecs", "Roost.Client.Codecs.csproj"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`<PackageReference Include="Google.Protobuf" Version="([^"]+)"`).FindSubmatch(raw)
	if match == nil {
		t.Fatal("Roost.Client.Codecs.csproj has no Google.Protobuf PackageReference")
	}
	if got := string(match[1]); got != sdkGoogleProtobufVersion {
		t.Fatalf("Roost.Client.Codecs locks Google.Protobuf %s, generator checks protoc against %s; update sdkGoogleProtobufVersion", got, sdkGoogleProtobufVersion)
	}
	if got := requiredProtocVersion(); got != "31.1" {
		t.Fatalf("protoc version derived from Google.Protobuf %s = %q, want 31.1", sdkGoogleProtobufVersion, got)
	}
}

func TestClientExportSpecValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		spec ClientExportSpec
		want string
	}{
		{ClientExportSpec{Dir: "/abs/protocol", Languages: []string{"csharp"}}, "relative"},
		{ClientExportSpec{Dir: "../outside", Languages: []string{"csharp"}}, "inside the project"},
		{ClientExportSpec{Dir: ".", Languages: []string{"csharp"}}, "inside the project"},
		{ClientExportSpec{Dir: "shared/bin/protocol", Languages: []string{"csharp"}}, "generation skips"},
		{ClientExportSpec{Dir: "protocol/client", Languages: []string{"csharp"}}, "inside protocol/"},
		{ClientExportSpec{Languages: []string{"typescript"}}, "unsupported language \"typescript\"; supported: csharp, ts"},
		{ClientExportSpec{Languages: []string{"ts", "ts"}}, "repeats"},
		{ClientExportSpec{Languages: []string{"csharp"}, TS: &ClientExportTS{ProtocGenEs: "x"}}, "client_export.ts is set but client_export.languages does not list ts"},
		// D4：csharp.protoc 挪到顶层，旧写法报错并写明新写法。
		{ClientExportSpec{Languages: []string{"csharp"}, CSharp: &ClientExportCSharp{RetiredProtoc: "tools/protoc"}}, "client_export.csharp.protoc moved to client_export.protoc"},
		{ClientExportSpec{Languages: []string{"csharp", "csharp"}}, "repeats"},
		{ClientExportSpec{CSharp: &ClientExportCSharp{Namespace: "Game"}}, "does not list csharp"},
		{ClientExportSpec{Languages: []string{"csharp"}, CSharp: &ClientExportCSharp{Namespace: "Game..Protocol"}}, "not a C# namespace"},
	} {
		err := test.spec.validate()
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("validate(%+v) = %v, want %q", test.spec, err, test.want)
		}
	}
	for _, spec := range []ClientExportSpec{
		{},
		{Languages: []string{"csharp"}},
		{Dir: "client/shared", Languages: []string{"csharp"}, Protoc: "tools/protoc", CSharp: &ClientExportCSharp{Namespace: "Game.Protocol"}},
		{Languages: []string{"ts"}},
		{Languages: []string{"csharp", "ts"}, Protoc: "tools/protoc", TS: &ClientExportTS{ProtocGenEs: "client/node_modules/.bin/protoc-gen-es"}},
	} {
		if err := spec.validate(); err != nil {
			t.Errorf("validate(%+v) = %v", spec, err)
		}
	}
	spec := &ClientExportSpec{}
	if spec.dir() != DefaultClientExportDir || !spec.owns("shared/protocol/csharp/Protocol.cs") || !spec.owns("shared/protocol/manifest.json") ||
		!spec.owns("shared/protocol/ts/protocol_pb.ts") || !spec.owns("shared/protocol/ts/error_codes.ts") || spec.owns("shared/protocol/ts/mine.ts") ||
		spec.owns("shared/protocol/csharp/Mine.cs") || spec.owns("shared/protocolx/manifest.json") {
		t.Fatal("default export dir ownership is wrong")
	}
	var none *ClientExportSpec
	if none.owns("shared/protocol/manifest.json") {
		t.Fatal("an undeclared export owns files")
	}
}

// 没声明 client_export 的工程：roost.yaml 写回时不多出键，生成器列表里没有 client-export。
func TestUndeclaredClientExportChangesNothing(t *testing.T) {
	t.Parallel()
	manifest := DefaultManifest("b7", "example.com/b7", nil, nil, nil)
	raw, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("client_export")) {
		t.Fatalf("undeclared client_export written to roost.yaml:\n%s", raw)
	}
	for _, gen := range generatorsFor(manifest, false) {
		if gen.Name == "client-export" {
			t.Fatal("client-export generator runs without client_export")
		}
	}
}

// fakeProtoc 写一个假 protoc：--version 答 version；--csharp_out 时把每个 proto 原文写成 protoc 的 C# 文件名；--es_out 时
// 先跑 --plugin 给的 protoc-gen-es --version（插件不能用就失败），再把 proto 原文写成 <名字>_pb.ts（第一行带 --es_opt）。
func fakeProtoc(t *testing.T, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake protoc is a shell script")
	}
	path := filepath.Join(t.TempDir(), "protoc")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "libprotoc ` + version + `"; exit 0; fi
inc="$2"; shift 2
mode=""; out=""; plugin=""; opt=""
while [ $# -gt 0 ]; do
  case "$1" in
    --csharp_out=*) mode=cs; out="${1#--csharp_out=}";;
    --es_out=*) mode=es; out="${1#--es_out=}";;
    --plugin=protoc-gen-es=*) plugin="${1#--plugin=protoc-gen-es=}";;
    --es_opt=*) opt="${1#--es_opt=}";;
    *.proto) break;;
    *) echo "unexpected argument $1" >&2; exit 2;;
  esac
  shift
done
for f in "$@"; do
  if [ "$mode" = cs ]; then
    case "$f" in protocol.proto) n=Protocol.cs;; dao_sync.proto) n=DaoSync.cs;; *) echo "unexpected $f" >&2; exit 2;; esac
    { echo "// <auto-generated> fake protoc"; cat "$inc/$f"; } > "$out/$n"
  else
    "$plugin" --version >/dev/null || { echo "plugin $plugin failed" >&2; exit 3; }
    { echo "// @generated by fake protoc-gen-es with parameter \"$opt\""; cat "$inc/$f"; } > "$out/${f%.proto}_pb.ts"
  fi
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeProtocGenEs 写一个假 protoc-gen-es：--version 答 protoc-gen-es v<version>。
func fakeProtocGenEs(t *testing.T, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake protoc-gen-es is a shell script")
	}
	path := filepath.Join(t.TempDir(), "protoc-gen-es")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"protoc-gen-es v" + version + "\"; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const clientExportTestDef = `//go:build protocoldef

package protocoldef

type PingRequest struct {
	Nonce int64 ` + "`pb:\"1\"`" + `
}
type PingResponse struct {
	Code  int32 ` + "`pb:\"1\"`" + `
	Nonce int64 ` + "`pb:\"2\"`" + `
}
type ScoreNotice struct {
	Score int64 ` + "`pb:\"1\"`" + `
}

//roost:protocol group=game handler=hello
type HelloProtocol interface {
	//roost:msg id=10001
	Ping(PingRequest) PingResponse
	//roost:msg id=10002
	Score() ScoreNotice
}
`

const clientExportTestErrors = `package errors

import "github.com/tjbdwanghaibo/roost-core/infra/base/errcode"

var (
	ErrHelloLate = errcode.Define(100001, "hello_late", "too late <for> \"ping\"")
	ErrHelloBusy = errcode.Define(100000, "hello_busy", "busy")
)
`

// DAO 同步的两份输入直接放进工程（工程不开 dao feature，generate 原样读它们）。
const clientExportTestDaoProto = `// Code generated by tool/dao. DO NOT EDIT.
syntax = "proto3";

package roost.dao.sync;

message SyncPlayerDao {
  int64 entity_id = 1;
}
`

const clientExportTestDaoApply = `// Code generated by tool/dao. DO NOT EDIT.
#nullable disable
namespace Roost.Dao.Sync {
public static class DaoSyncApply {
}}
`

// newClientExportProject 建一个开了 protocol、errcode 的工程，写协议、错误码与 DAO 同步输入，并声明 client_export。
func newClientExportProject(t *testing.T, exportYAML string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"protocol", "errcode"}})
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, "protocol/def/hello.go", clientExportTestDef)
	writeProjectFile(t, root, "internal/errors/hello.go", clientExportTestErrors)
	writeProjectFile(t, root, "db/dao_sync.proto", clientExportTestDaoProto)
	writeProjectFile(t, root, "db/dao_sync_apply.cs", clientExportTestDaoApply)
	writeProjectFile(t, root, ManifestName, readProjectFile(t, root, ManifestName)+exportYAML)
	return root
}

// exportYAML 声明只导出 csharp 的 client_export；extra 接在 csharp: 下面（例如 namespace）。
func exportYAML(protoc string, extra string) string {
	return "client_export:\n  languages: [csharp]\n  protoc: " + protoc + "\n  csharp:\n" + extra
}

// exportYAMLTS 声明导出 languages 里的语言（含 ts），插件写在 ts.protoc_gen_es。
func exportYAMLTS(languages, protoc, plugin string) string {
	return "client_export:\n  languages: [" + languages + "]\n  protoc: " + protoc + "\n  ts:\n    protoc_gen_es: " + plugin + "\n"
}

func readExportManifest(t *testing.T, root string) clientExportManifest {
	t.Helper()
	manifest, err := readClientExportManifest(filepath.Join(root, "shared", "protocol", exportManifest))
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestGenerateClientExport(t *testing.T) {
	t.Parallel()
	root := newClientExportProject(t, exportYAML(fakeProtoc(t, "31.1"), ""))
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	for _, name := range clientExportFiles {
		if strings.HasPrefix(name, "ts/") {
			continue // 只声明 csharp：没有 ts/（见下）
		}
		if _, err := os.Stat(filepath.Join(root, "shared", "protocol", filepath.FromSlash(name))); err != nil {
			t.Errorf("export file %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "shared", "protocol", "ts")); !os.IsNotExist(err) {
		t.Fatalf("a csharp-only export wrote ts/: %v", err)
	}
	// 导出的 proto 就是服务端生成的那份（没配命名空间时不加选项），C# 由 protoc 从它生成。
	if got, want := readProjectFile(t, root, "shared/protocol/proto/protocol.proto"), readProjectFile(t, root, "protocol/proto/protocol.proto"); got != want {
		t.Fatalf("exported protocol.proto differs from the server's:\n%s", got)
	}
	if got := readProjectFile(t, root, "shared/protocol/csharp/DaoSyncApply.cs"); got != clientExportTestDaoApply {
		t.Fatalf("DaoSyncApply.cs is not db/dao_sync_apply.cs:\n%s", got)
	}
	ids := readProjectFile(t, root, "shared/protocol/csharp/MessageIds.cs")
	for _, want := range []string{"namespace Roost.Generated", "RequestPing = 10001", "ResponsePing = 10001", "PushScore = 10002"} {
		if !strings.Contains(ids, want) {
			t.Errorf("MessageIds.cs missing %q:\n%s", want, ids)
		}
	}
	codes := readProjectFile(t, root, "shared/protocol/csharp/ErrorCodes.cs")
	for _, want := range []string{"public const int HelloBusy = 100000;", "public const int HelloLate = 100001;", `case HelloLate: return "too late <for> \"ping\"";`, "/// <summary>too late &lt;for&gt; \"ping\"</summary>"} {
		if !strings.Contains(codes, want) {
			t.Errorf("ErrorCodes.cs missing %q:\n%s", want, codes)
		}
	}
	manifest := readExportManifest(t, root)
	if manifest.Protoc != "31.1" || manifest.GoogleProtobuf != sdkGoogleProtobufVersion || manifest.Generator == "" || manifest.Sources.ProtocolDef == "" || manifest.Sources.DaoSync == "" {
		t.Fatalf("manifest versions / sources: %+v", manifest)
	}
	// 消息号清单与 msgid_gen.go 一致：每个 MsgX = N 都在清单里（请求或推送）。
	msgid := readProjectFile(t, root, "protocol/msgid/msgid_gen.go")
	for _, message := range manifest.Messages {
		if message.Direction == "response" {
			continue
		}
		if !regexp.MustCompile(`\bMsg` + message.Name + `\s+uint32 = ` + itoa(message.ID) + `\n`).MatchString(msgid) {
			t.Errorf("manifest message %+v not in msgid_gen.go:\n%s", message, msgid)
		}
	}
	if got := len(manifest.Messages); got != 3 {
		t.Fatalf("manifest messages = %+v, want request/response Ping and push Score", manifest.Messages)
	}
	if len(manifest.ErrorCodes) != 2 || manifest.ErrorCodes[0] != (clientErrorCode{100000, "hello_busy", "busy"}) || manifest.ErrorCodes[1].Name != "hello_late" {
		t.Fatalf("manifest errcodes = %+v", manifest.ErrorCodes)
	}
	for name, hash := range manifest.Files {
		if got := contentHash([]byte(readProjectFile(t, root, "shared/protocol/"+name))); got != hash {
			t.Errorf("manifest hash of %s = %s, file hashes %s", name, hash, got)
		}
	}

	// 同样的输入，两次生成逐字节相同；--check 与 doctor 都认为最新。
	before, err := snapshotGeneratedFor(root, mustLoadManifest(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard, Force: true}); err != nil {
		t.Fatal(err)
	}
	after, err := snapshotGeneratedFor(root, mustLoadManifest(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if changed := diffSnapshot(before, after); len(changed) != 0 {
		t.Fatalf("second generation changed %v", changed)
	}
	if err := Generate(root, GenerateOptions{Check: true, Stdout: io.Discard}); err != nil {
		t.Fatalf("generate --check after generate: %v", err)
	}
	if item := checkClientExport(root, mustLoadManifest(t, root)); item.Status != StatusOK {
		t.Fatalf("doctor after generate: %+v", item)
	}

	// 只改 protocol/def、不重新生成：--check 与 doctor 都报过期。
	writeProjectFile(t, root, "protocol/def/hello.go", strings.Replace(clientExportTestDef, "Nonce int64 `pb:\"1\"`\n}\ntype PingResponse", "Nonce int64 `pb:\"1\"`\n\tNote string `pb:\"2\"`\n}\ntype PingResponse", 1))
	err = Generate(root, GenerateOptions{Check: true, Stdout: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "shared/protocol/manifest.json") || !strings.Contains(err.Error(), "shared/protocol/csharp/Protocol.cs") {
		t.Fatalf("generate --check after protocol/def edit = %v, want export files stale", err)
	}
	if item := checkClientExport(root, mustLoadManifest(t, root)); item.Status != StatusFail || !strings.Contains(item.Detail, "protocol/def changed") {
		t.Fatalf("doctor after protocol/def edit: %+v", item)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	// 新加错误码、手改导出文件：doctor 都报。
	writeProjectFile(t, root, "internal/errors/more.go", "package errors\n\nimport \"github.com/tjbdwanghaibo/roost-core/infra/base/errcode\"\n\nvar ErrMore = errcode.Define(100002, \"hello_more\", \"more\")\n")
	if item := checkClientExport(root, mustLoadManifest(t, root)); item.Status != StatusFail || !strings.Contains(item.Detail, "errcode definitions changed") {
		t.Fatalf("doctor after errcode edit: %+v", item)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, "shared/protocol/csharp/MessageIds.cs", ids+"// hand edit\n")
	if item := checkClientExport(root, mustLoadManifest(t, root)); item.Status != StatusFail || !strings.Contains(item.Detail, "csharp/MessageIds.cs does not match") {
		t.Fatalf("doctor after hand edit: %+v", item)
	}
}

func itoa(value uint32) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func mustLoadManifest(t *testing.T, root string) Manifest {
	t.Helper()
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// 配了 csharp.namespace：协议、消息号、错误码在 <namespace>，DAO 同步在 <namespace>.DaoSync；导出的 proto 带 csharp_namespace。
func TestClientExportNamespace(t *testing.T) {
	t.Parallel()
	root := newClientExportProject(t, exportYAML(fakeProtoc(t, "31.1"), "    namespace: Game.Protocol\n"))
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"shared/protocol/proto/protocol.proto":   `option csharp_namespace = "Game.Protocol";`,
		"shared/protocol/proto/dao_sync.proto":   `option csharp_namespace = "Game.Protocol.DaoSync";`,
		"shared/protocol/csharp/Protocol.cs":     `option csharp_namespace = "Game.Protocol";`,
		"shared/protocol/csharp/DaoSyncApply.cs": "\nnamespace Game.Protocol.DaoSync {\n",
		"shared/protocol/csharp/MessageIds.cs":   "\nnamespace Game.Protocol\n",
		"shared/protocol/csharp/ErrorCodes.cs":   "\nnamespace Game.Protocol\n",
	} {
		if got := readProjectFile(t, root, rel); !strings.Contains(got, want) {
			t.Errorf("%s missing %q:\n%s", rel, want, got)
		}
	}
	if strings.Contains(readProjectFile(t, root, "protocol/proto/protocol.proto"), "csharp_namespace") {
		t.Fatal("namespace option leaked into the server's protocol.proto")
	}
}

// protoc 版本不对、找不到时报错并写明需要的版本；查找顺序是 client_export.protoc、ROOST_PROTOC、PATH。
func TestClientExportProtocVersion(t *testing.T) {
	wrong := fakeProtoc(t, "36.2")
	right := fakeProtoc(t, "31.1")
	root := newClientExportProject(t, exportYAML(wrong, ""))
	err := Generate(root, GenerateOptions{Stdout: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "needs protoc 31.1") || !strings.Contains(err.Error(), "libprotoc 36.2") ||
		!strings.Contains(err.Error(), "releases/tag/v31.1") || !strings.Contains(err.Error(), "from client_export.protoc") {
		t.Fatalf("wrong protoc version: %v", err)
	}
	// 版本不对时先于任何生成器失败：服务端的生成物也没写。
	if _, statErr := os.Stat(filepath.Join(root, "protocol", "proto", "protocol.proto")); !os.IsNotExist(statErr) {
		t.Fatalf("generators ran before the protoc check: %v", statErr)
	}

	spec := &ClientExportSpec{Languages: []string{"csharp"}}
	t.Setenv("ROOST_PROTOC", wrong)
	if _, err := spec.resolveProtoc(); err == nil || !strings.Contains(err.Error(), "from ROOST_PROTOC") {
		t.Fatalf("ROOST_PROTOC not used: %v", err)
	}
	spec.Protoc = right
	if got, err := spec.resolveProtoc(); err != nil || got != right {
		t.Fatalf("client_export.protoc does not win over ROOST_PROTOC: %q %v", got, err)
	}
	// 相对路径按工程根解析。
	spec.Protoc = filepath.Base(right)
	spec.projectRoot = filepath.Dir(right)
	if got, err := spec.resolveProtoc(); err != nil || got != right {
		t.Fatalf("relative client_export.protoc: %q %v", got, err)
	}
	spec.Protoc = ""
	t.Setenv("ROOST_PROTOC", "")
	t.Setenv("PATH", filepath.Dir(right))
	if got, err := spec.resolveProtoc(); err != nil || got != right {
		t.Fatalf("PATH protoc: %q %v", got, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := spec.resolveProtoc(); err == nil || !strings.Contains(err.Error(), "needs protoc 31.1") || !strings.Contains(err.Error(), "none was found") {
		t.Fatalf("missing protoc: %v", err)
	}
	// 不导出 C# 不要 protoc。
	if err := (&ClientExportSpec{}).validate(); err != nil {
		t.Fatal(err)
	}
}

// 暂存生成（roost generate 的正常路径）：导出文件随批次提交；languages 去掉 csharp 后，上次的 .cs 被删掉。
func TestClientExportTransactionalCommitAndRetirement(t *testing.T) {
	t.Parallel()
	protoc := fakeProtoc(t, "31.1")
	root := newClientExportProject(t, exportYAML(protoc, ""))
	writeProjectFile(t, root, "shared/protocol/README.md", "client notes\n")
	if err := GenerateTransactional(context.Background(), root, GenerateOptions{Stdout: io.Discard, deferDependencies: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := readExportManifest(t, root); len(got.Files) != 7 {
		t.Fatalf("committed export lists %d files: %v", len(got.Files), got.Files)
	}
	readProjectFile(t, root, "shared/protocol/csharp/Protocol.cs")
	manifestText := readProjectFile(t, root, ManifestName)
	writeProjectFile(t, root, ManifestName, strings.Replace(manifestText, exportYAML(protoc, ""), "client_export:\n  dir: shared/protocol\n", 1))
	if err := GenerateTransactional(context.Background(), root, GenerateOptions{Stdout: io.Discard, deferDependencies: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{exportProtocolCS, exportDaoSyncCS, exportDaoSyncApply, exportMessageIDs, exportErrorCodes} {
		if _, err := os.Stat(filepath.Join(root, "shared", "protocol", filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Errorf("%s survived dropping csharp: %v", name, err)
		}
	}
	if got := readExportManifest(t, root); len(got.Files) != 2 || got.Protoc != "" || got.Namespaces != nil {
		t.Fatalf("proto-only export manifest: %+v", got)
	}
	if readProjectFile(t, root, "shared/protocol/README.md") != "client notes\n" {
		t.Fatal("a file the generator does not own was changed")
	}
}

func TestClientSync(t *testing.T) {
	t.Parallel()
	root := newClientExportProject(t, exportYAML(fakeProtoc(t, "31.1"), ""))
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(t.TempDir(), "Assets", "Protocol")
	var out bytes.Buffer
	if err := ClientSync(root, client, "", false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "files: 5 added") || !strings.Contains(out.String(), "first sync") {
		t.Fatalf("first sync output:\n%s", out.String())
	}
	for _, name := range []string{"Protocol.cs", "DaoSync.cs", "DaoSyncApply.cs", "MessageIds.cs", "ErrorCodes.cs", "manifest.json"} {
		if got, want := readProjectFile(t, client, name), readProjectFile(t, root, map[bool]string{true: "shared/protocol/manifest.json", false: "shared/protocol/csharp/" + name}[name == "manifest.json"]); got != want {
			t.Errorf("client %s differs from the export", name)
		}
	}
	if entries, _ := filepath.Glob(filepath.Join(client, "*.meta")); len(entries) != 0 {
		t.Fatalf("sync wrote Unity .meta files: %v", entries)
	}

	// 服务端：加一个消息、改一个错误码说明、加一个错误码、去掉 DAO 同步。
	writeProjectFile(t, root, "protocol/def/hello.go", strings.Replace(clientExportTestDef, "\t//roost:msg id=10002\n", "\t//roost:msg id=10003\n\tWave(PingRequest) PingResponse\n\t//roost:msg id=10002\n", 1))
	writeProjectFile(t, root, "internal/errors/hello.go", strings.Replace(clientExportTestErrors, `"busy"`, `"busy now"`, 1)+
		"\nvar ErrHelloMore = errcode.Define(100002, \"hello_more\", \"more\")\n")
	for _, rel := range []string{"db/dao_sync.proto", "db/dao_sync_apply.cs"} {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := ClientSync(root, client, "", true, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(client, "DaoSync.cs")); err != nil {
		t.Fatal("dry run removed a file")
	}
	out.Reset()
	if err := ClientSync(root, client, "csharp", false, &out); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	for _, want := range []string{
		"removed DaoSync.cs", "removed DaoSyncApply.cs",
		"message ids added (2):", "request Wave = 10003 (PingRequest)", "response Wave = 10003 (PingResponse)",
		"errcodes added (1):", `hello_more = 100002 "more"`,
		"errcodes changed (1):", `hello_busy: 100000 "busy" -> 100000 "busy now"`,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("sync report missing %q:\n%s", want, report)
		}
	}
	for _, name := range []string{"DaoSync.cs", "DaoSyncApply.cs"} {
		if _, err := os.Stat(filepath.Join(client, name)); !os.IsNotExist(err) {
			t.Errorf("%s still in the client after the server dropped it: %v", name, err)
		}
	}

	// 客户端目录里不是上次同步写的同名文件：拒绝覆盖。
	foreign := filepath.Join(t.TempDir(), "Mine")
	writeProjectFile(t, foreign, "Protocol.cs", "// mine\n")
	if err := ClientSync(root, foreign, "", false, io.Discard); err == nil || !strings.Contains(err.Error(), "refuse to overwrite") {
		t.Fatalf("overwrote a client-owned file: %v", err)
	}
	if readProjectFile(t, foreign, "Protocol.cs") != "// mine\n" {
		t.Fatal("client-owned file changed")
	}
	// 服务端导出目录被手改：拒绝同步。
	writeProjectFile(t, root, "shared/protocol/csharp/ErrorCodes.cs", "// tampered\n")
	if err := ClientSync(root, client, "", false, io.Discard); err == nil || !strings.Contains(err.Error(), "csharp/ErrorCodes.cs does not match") {
		t.Fatalf("synced a tampered export: %v", err)
	}
}

// TestSDKProtobufEsVersionMatchesClientTS：生成器核对 protoc-gen-es 用的版本，必须就是 TS SDK 锁定的 @bufbuild/protobuf
// （peer 依赖）与 devDependencies 里的运行库、插件——三处同一个精确版本。
func TestSDKProtobufEsVersionMatchesClientTS(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "client", "ts", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Peer map[string]string `json:"peerDependencies"`
		Dev  map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatal(err)
	}
	for label, got := range map[string]string{
		"peerDependencies @bufbuild/protobuf":     pkg.Peer["@bufbuild/protobuf"],
		"devDependencies @bufbuild/protobuf":      pkg.Dev["@bufbuild/protobuf"],
		"devDependencies @bufbuild/protoc-gen-es": pkg.Dev["@bufbuild/protoc-gen-es"],
	} {
		if got != sdkProtobufEsVersion {
			t.Errorf("client/ts/package.json %s = %q, generator checks protoc-gen-es against %s; keep them one exact version", label, got, sdkProtobufEsVersion)
		}
	}
}

// 声明 ts：ts/ 下四个文件（protoc-gen-es 生成的两个 *_pb.ts，Go 生成的消息号与错误码）与 manifest 的版本字段；两次生成逐字节
// 相同，--check 与 doctor 覆盖 ts/。同时声明 csharp 时 C# 文件与只声明 csharp 时逐字节相同；只声明 csharp 的 manifest 没有
// TS 字段。
func TestGenerateClientExportTS(t *testing.T) {
	t.Parallel()
	protoc, plugin := fakeProtoc(t, "31.1"), fakeProtocGenEs(t, sdkProtobufEsVersion)
	root := newClientExportProject(t, exportYAMLTS("ts", protoc, plugin))
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	manifest := readExportManifest(t, root)
	if strings.Join(manifest.Languages, ",") != "ts" || manifest.Protoc != "31.1" || manifest.ProtocGenEs != sdkProtobufEsVersion ||
		manifest.ProtobufEs != sdkProtobufEsVersion || manifest.GoogleProtobuf != "" {
		t.Fatalf("ts manifest: %+v", manifest)
	}
	var names []string
	for name := range manifest.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	if got := strings.Join(names, " "); got != "proto/dao_sync.proto proto/protocol.proto ts/dao_sync_pb.ts ts/error_codes.ts ts/message_ids.ts ts/protocol_pb.ts" {
		t.Fatalf("ts export files: %s", got)
	}
	if _, err := os.Stat(filepath.Join(root, "shared", "protocol", "csharp")); !os.IsNotExist(err) {
		t.Fatalf("a ts-only export wrote csharp/: %v", err)
	}
	pb := readProjectFile(t, root, "shared/protocol/ts/protocol_pb.ts")
	if !strings.HasPrefix(pb, "// @generated by fake protoc-gen-es with parameter \"target=ts,import_extension=js\"\n") ||
		!strings.Contains(pb, readProjectFile(t, root, "shared/protocol/proto/protocol.proto")) {
		t.Fatalf("protocol_pb.ts is not protoc-gen-es (target=ts, import_extension=js) over the exported proto:\n%s", pb)
	}
	ids := readProjectFile(t, root, "shared/protocol/ts/message_ids.ts")
	for _, want := range []string{"export const MessageIds = {\n", "  RequestPing: 10001,\n", "  ResponsePing: 10001,\n", "  PushScore: 10002,\n", "} as const;\n"} {
		if !strings.Contains(ids, want) {
			t.Errorf("message_ids.ts missing %q:\n%s", want, ids)
		}
	}
	codes := readProjectFile(t, root, "shared/protocol/ts/error_codes.ts")
	for _, want := range []string{"HelloBusy: 100000,\n", "HelloLate: 100001,\n", "export function errorCodeName(", "export function errorCodeMessage("} {
		if !strings.Contains(codes, want) {
			t.Errorf("error_codes.ts missing %q:\n%s", want, codes)
		}
	}

	before, err := snapshotGeneratedFor(root, mustLoadManifest(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard, Force: true}); err != nil {
		t.Fatal(err)
	}
	after, err := snapshotGeneratedFor(root, mustLoadManifest(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if changed := diffSnapshot(before, after); len(changed) != 0 {
		t.Fatalf("second generation changed %v", changed)
	}
	if err := Generate(root, GenerateOptions{Check: true, Stdout: io.Discard}); err != nil {
		t.Fatalf("generate --check after generate: %v", err)
	}
	if item := checkClientExport(root, mustLoadManifest(t, root)); item.Status != StatusOK {
		t.Fatalf("doctor after generate: %+v", item)
	}
	writeProjectFile(t, root, "shared/protocol/ts/message_ids.ts", ids+"// hand edit\n")
	if item := checkClientExport(root, mustLoadManifest(t, root)); item.Status != StatusFail || !strings.Contains(item.Detail, "ts/message_ids.ts") {
		t.Fatalf("doctor after a hand edit in ts/: %+v", item)
	}

	// 两种语言：C# 文件与只声明 csharp 时逐字节相同。
	both := newClientExportProject(t, exportYAMLTS("csharp, ts", protoc, plugin))
	only := newClientExportProject(t, exportYAML(protoc, ""))
	for _, project := range []string{both, only} {
		if err := Generate(project, GenerateOptions{Stdout: io.Discard}); err != nil {
			t.Fatal(err)
		}
	}
	onlyManifest := readExportManifest(t, only)
	for name := range onlyManifest.Files {
		if readProjectFile(t, both, "shared/protocol/"+name) != readProjectFile(t, only, "shared/protocol/"+name) {
			t.Errorf("%s differs between csharp-only and csharp + ts", name)
		}
	}
	if got := readExportManifest(t, both); got.GoogleProtobuf == "" || got.ProtobufEs != sdkProtobufEsVersion || len(got.Files) != len(onlyManifest.Files)+4 {
		t.Fatalf("csharp + ts manifest: %+v", got)
	}
	onlyRaw := readProjectFile(t, only, "shared/protocol/manifest.json")
	for _, absent := range []string{"protoc_gen_es", "protobuf_es", "ts/"} {
		if strings.Contains(onlyRaw, absent) {
			t.Errorf("csharp-only manifest mentions %q:\n%s", absent, onlyRaw)
		}
	}
}

// protoc-gen-es 版本不对、找不到时报错并写明需要的版本，先于任何生成器；查找顺序 ts.protoc_gen_es、ROOST_PROTOC_GEN_ES、PATH。
func TestClientExportProtocGenEsVersion(t *testing.T) {
	protoc := fakeProtoc(t, "31.1")
	wrong := fakeProtocGenEs(t, "2.16.0")
	right := fakeProtocGenEs(t, sdkProtobufEsVersion)
	root := newClientExportProject(t, exportYAMLTS("ts", protoc, wrong))
	err := Generate(root, GenerateOptions{Stdout: io.Discard})
	if err == nil || !strings.Contains(err.Error(), sdkProtobufEsVersion) || !strings.Contains(err.Error(), "v2.16.0") {
		t.Fatalf("wrong protoc-gen-es version: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "shared", "protocol", "manifest.json")); !os.IsNotExist(statErr) {
		t.Fatalf("export written before the protoc-gen-es check: %v", statErr)
	}

	spec := &ClientExportSpec{Languages: []string{"ts"}}
	t.Setenv("ROOST_PROTOC_GEN_ES", wrong)
	if _, err := spec.resolveProtocGenEs(); err == nil || !strings.Contains(err.Error(), "ROOST_PROTOC_GEN_ES") {
		t.Fatalf("ROOST_PROTOC_GEN_ES not used: %v", err)
	}
	spec.TS = &ClientExportTS{ProtocGenEs: right}
	if got, err := spec.resolveProtocGenEs(); err != nil || got != right {
		t.Fatalf("ts.protoc_gen_es does not win over ROOST_PROTOC_GEN_ES: %q %v", got, err)
	}
	spec.TS.ProtocGenEs = filepath.Base(right)
	spec.projectRoot = filepath.Dir(right)
	if got, err := spec.resolveProtocGenEs(); err != nil || got != right {
		t.Fatalf("relative ts.protoc_gen_es: %q %v", got, err)
	}
	spec.TS = nil
	t.Setenv("ROOST_PROTOC_GEN_ES", "")
	t.Setenv("PATH", filepath.Dir(right))
	if got, err := spec.resolveProtocGenEs(); err != nil || got != right {
		t.Fatalf("PATH protoc-gen-es: %q %v", got, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := spec.resolveProtocGenEs(); err == nil {
		t.Fatal("missing protoc-gen-es accepted")
	}
}

// D4（维护者 2026-11-18）：roost.yaml 里还写着 csharp.protoc 的工程加载时报错，写明挪到 client_export.protoc。
func TestClientExportRetiredCSharpProtoc(t *testing.T) {
	t.Parallel()
	root := newClientExportProject(t, "client_export:\n  languages: [csharp]\n  csharp:\n    protoc: tools/protoc\n")
	if _, err := LoadManifest(root); err == nil || !strings.Contains(err.Error(), "client_export.csharp.protoc moved to client_export.protoc") {
		t.Fatalf("old csharp.protoc: %v", err)
	}
}

// roost client sync --lang（D9）：两种语言都声明时必须写 --lang；--lang ts 平铺 ts/；报告写明要装的 @bufbuild/protobuf 版本；
// 拒绝覆盖不是上次同步写的文件；--lang csharp 用在只声明 ts 的导出上报错。
func TestClientSyncTS(t *testing.T) {
	t.Parallel()
	protoc, plugin := fakeProtoc(t, "31.1"), fakeProtocGenEs(t, sdkProtobufEsVersion)
	root := newClientExportProject(t, exportYAMLTS("csharp, ts", protoc, plugin))
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(t.TempDir(), "web", "src", "protocol")
	if err := ClientSync(root, client, "", false, io.Discard); err == nil || !strings.Contains(err.Error(), "pass --lang csharp|ts") {
		t.Fatalf("sync without --lang while both are declared: %v", err)
	}
	var out bytes.Buffer
	if err := ClientSync(root, client, "ts", false, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(ts) -> ", "files: 4 added", "protobuf-es " + sdkProtobufEsVersion, "install @bufbuild/protobuf " + sdkProtobufEsVersion + " exactly"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ts sync report missing %q:\n%s", want, out.String())
		}
	}
	for _, name := range []string{"protocol_pb.ts", "dao_sync_pb.ts", "message_ids.ts", "error_codes.ts"} {
		if readProjectFile(t, client, name) != readProjectFile(t, root, "shared/protocol/ts/"+name) {
			t.Errorf("client %s differs from the export", name)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(client, "*.cs")); len(matches) != 0 {
		t.Fatalf("--lang ts copied C# files: %v", matches)
	}
	// 第二次同步无变化。
	out.Reset()
	if err := ClientSync(root, client, "ts", false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "files: 0 added, 0 updated, 0 removed, 4 unchanged") {
		t.Fatalf("second ts sync added files again:\n%s", out.String())
	}

	foreign := filepath.Join(t.TempDir(), "mine")
	writeProjectFile(t, foreign, "message_ids.ts", "// mine\n")
	if err := ClientSync(root, foreign, "ts", false, io.Discard); err == nil || !strings.Contains(err.Error(), "refuse to overwrite") {
		t.Fatalf("overwrote a client-owned file: %v", err)
	}

	tsOnly := newClientExportProject(t, exportYAMLTS("ts", protoc, plugin))
	if err := Generate(tsOnly, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if err := ClientSync(tsOnly, filepath.Join(t.TempDir(), "auto"), "", false, io.Discard); err != nil {
		t.Fatalf("--lang omitted with ts only: %v", err)
	}
	if err := ClientSync(tsOnly, filepath.Join(t.TempDir(), "cs"), "csharp", false, io.Discard); err == nil || !strings.Contains(err.Error(), "does not list csharp") {
		t.Fatalf("--lang csharp on a ts-only export: %v", err)
	}
	for _, args := range [][]string{
		{"sync", "--from", tsOnly, "--to", client, "--lang", "go"},
		{"sync", "--from", tsOnly, "--lang", "ts", "--config-code-to", "a", "--config-data-to", "b"},
	} {
		if err := runClient(args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--lang") {
			t.Errorf("roost client %v: %v", args, err)
		}
	}
}

// 真实工具链（scripts/test-client-ts.sh 完整模式 ROOST_TS_FULL=1；ROOST_PROTOC 指向 protoc 31.1、client/ts 里 npm ci 过）：
// 真 protoc + node_modules/.bin/protoc-gen-es 导出一个声明 ts 的工程，两次生成逐字节相同，再用 tsc 把导出的 ts/ 与 SDK
// 的 gate、protobuf 子路径一起编译，核对 64 位字段是 bigint、消息号与错误码可用。
func TestClientExportTSWithTheRealToolchain(t *testing.T) {
	if os.Getenv("ROOST_TS_FULL") != "1" {
		t.Skip("ROOST_TS_FULL not set (scripts/test-client-ts.sh runs it with ROOST_TS_FULL=1)")
	}
	sdk, err := filepath.Abs(filepath.Join("..", "..", "..", "client", "ts"))
	if err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(sdk, "node_modules", ".bin", "protoc-gen-es")
	tsc := filepath.Join(sdk, "node_modules", ".bin", "tsc")
	if _, err := os.Stat(tsc); err != nil {
		t.Fatalf("run npm ci in client/ts first: %v", err)
	}
	if os.Getenv("ROOST_PROTOC") == "" {
		t.Fatal("ROOST_PROTOC must point at protoc 31.1")
	}
	root := newClientExportProject(t, "client_export:\n  languages: [ts]\n  ts:\n    protoc_gen_es: "+plugin+"\n")
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotGeneratedFor(root, mustLoadManifest(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard, Force: true}); err != nil {
		t.Fatal(err)
	}
	after, err := snapshotGeneratedFor(root, mustLoadManifest(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if changed := diffSnapshot(before, after); len(changed) != 0 {
		t.Fatalf("second generation with the real toolchain changed %v", changed)
	}
	pb := readProjectFile(t, root, "shared/protocol/ts/protocol_pb.ts")
	if !strings.Contains(pb, "@generated by protoc-gen-es v"+sdkProtobufEsVersion) {
		t.Fatalf("protocol_pb.ts header:\n%s", pb[:min(len(pb), 400)])
	}

	// 一个最小的“网页工程”：导出的 ts/ 原样拷进来，node_modules 借 SDK 的（同一份 @bufbuild/protobuf）。
	web := t.TempDir()
	for _, name := range []string{"protocol_pb.ts", "dao_sync_pb.ts", "message_ids.ts", "error_codes.ts"} {
		writeProjectFile(t, web, "protocol/"+name, readProjectFile(t, root, "shared/protocol/ts/"+name))
	}
	if err := os.Symlink(filepath.Join(sdk, "node_modules"), filepath.Join(web, "node_modules")); err != nil {
		t.Fatal(err)
	}
	src := filepath.ToSlash(filepath.Join(sdk, "src"))
	writeProjectFile(t, web, "main.ts", `import { GameClient } from "`+src+`/gate/index.js";
import { request, subscribe } from "`+src+`/protobuf/index.js";
import { MessageIds } from "./protocol/message_ids.js";
import { ErrorCodes, errorCodeMessage, errorCodeName } from "./protocol/error_codes.js";
import { PingRequestSchema, PingResponseSchema, ScoreNoticeSchema, type PingResponse } from "./protocol/protocol_pb.js";

export function wire(client: GameClient, show: (text: string) => void): void {
  subscribe(client, MessageIds.PushScore, ScoreNoticeSchema, (notice) => {
    show(String(notice.score));
  });
  request(client, MessageIds.RequestPing, MessageIds.ResponsePing, PingRequestSchema, { nonce: 9007199254740993n }, PingResponseSchema,
    (response: PingResponse) => {
      const nonce: bigint = response.nonce;
      show(errorCodeName(ErrorCodes.HelloBusy) + (errorCodeMessage(response.code) ?? "") + String(nonce + 1n));
    },
    (error) => show(error.message));
}
`)
	// 按浏览器原生 ESM / node 的规则（NodeNext）编译。导出的两个 proto 互不导入，生成文件里没有相对导入，所以
	// import_extension=js 在这里看不出来（由假插件用例核对参数），等 proto 之间有导入时它才起作用。
	writeProjectFile(t, web, "package.json", `{"type":"module","private":true}`)
	writeProjectFile(t, web, "tsconfig.json", `{"compilerOptions":{"target":"ES2022","module":"NodeNext","moduleResolution":"NodeNext","lib":["ES2022","DOM"],"strict":true,"noEmit":true,"types":[]},"files":["main.ts","protocol/dao_sync_pb.ts"]}`)
	command := exec.Command(tsc, "-p", web)
	command.Dir = web
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("tsc over the exported ts/ and the SDK: %v\n%s", err, output)
	}
}
