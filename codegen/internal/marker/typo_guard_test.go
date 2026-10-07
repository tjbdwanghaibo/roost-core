package marker_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/attribute"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/dao"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entity"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/marker"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/nest"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/protocol"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/registry"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/servicerpc"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/tablegen"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/webroute"
)

// RR-20261006-56：标记键名拼错必须报错，并点名键、文件和行。
//
// 旧行为：//roost:nest、//roost:dao、//roost:redisdao、//roost:attribute（以及
// protocol / msg / proto / view / table / object / rpc）只取认识的键，拼错的键
// 被当成“没写”取缺省值：`durabilty=strict` 生成出 async，`dbscop=sid` 写进全局库，
// 生成器不报错，生成物看起来正常。
//
// 这里对每一种带选项的标记，各跑一次它所属的生成器，喂一个拼错的键。

// typoCase is one marker kind fed a misspelt key through its own generator.
type typoCase struct {
	kind string
	// file is the source file, relative to the generator's input root.
	file string
	// line is where the marker sits in source (1-based).
	line   int
	source string
	typo   string
	// run runs the owning generator over root (the temp input directory).
	run func(t *testing.T, root string) error
}

func runArgs(gen func([]string, io.Writer) error, args func(root, out string) []string) func(*testing.T, string) error {
	return func(t *testing.T, root string) error {
		return gen(args(root, t.TempDir()), io.Discard)
	}
}

var (
	runNest      = runArgs(nest.Run, func(root, _ string) []string { return []string{"-dir", root} })
	runDao       = runArgs(dao.Run, func(root, out string) []string { return []string{"-def", root, "-out", out, "-pkg", "out"} })
	runAttribute = runArgs(attribute.Run, func(root, _ string) []string { return []string{"-dir", root} })
	runProtocol  = runArgs(protocol.Run, func(root, out string) []string {
		return []string{"-def", root, "-proto", filepath.Join(out, "proto"), "-pb", filepath.Join(out, "pb"),
			"-msgid", filepath.Join(out, "msgid"), "-bind", filepath.Join(out, "bind"), "-handlers", filepath.Join(out, "handlers"),
			"-robot-protocol", filepath.Join(out, "robot.go"), "-manifest", filepath.Join(out, "manifest.json")}
	})
	runTable  = runArgs(tablegen.Run, func(root, out string) []string { return []string{"-meta", root, "-out", out, "-pkg", "tables"} })
	runRPC    = runArgs(servicerpc.Run, func(root, _ string) []string { return []string{"-dir", root, "-emit", "transport"} })
	runEntity = runArgs(entity.Run, func(root, _ string) []string { return []string{"-dir", root} })
	runWeb    = runArgs(webroute.Run, func(root, _ string) []string { return []string{"-dir", root} })
	runReg    = func(t *testing.T, root string) error { return registry.Run(root, "example.com/planet", io.Discard) }
)

const rpcService = `package mail

import "context"

var ErrRequestInvalid error

type SendRequest struct{ To int64 }
type Envelope struct{ ID string }

//roost:rpc service_type=mail %s
type Mail interface {
	%s
	Send(ctx context.Context, req SendRequest) (envelope Envelope, err error)
}
`

var typoCases = []typoCase{
	{kind: "nest", file: "grant.go", line: 3, typo: "durabilty", run: runNest,
		source: "package handler\n\n//roost:nest rollback=undo durabilty=strict\nfunc handlerGrant(p IPlayerEntity, orderID string) (bool, error) {\n\treturn true, nil\n}\n"},
	{kind: "dao", file: "player.go", line: 3, typo: "dbscop", run: runDao,
		source: "package def\n\n//roost:dao coll=players db=game dbscop=sid\ntype PlayerDao struct {\n\tName string `dao:\"persist\"`\n}\n"},
	{kind: "redisdao", file: "hero.go", line: 3, typo: "tll", run: runDao,
		source: "package def\n\n//roost:redisdao mode=ref-hmap key=Name prefix=hero tll=1h\ntype HeroCache struct {\n\tName string\n}\n"},
	{kind: "attribute", file: "profile.go", line: 3, typo: "mx", run: runAttribute,
		source: "package attr\n\n//roost:attribute index=1 mx=4\ntype PlayerProfile struct {\n\tHP int64\n\tdirtyMask uint64\n}\n"},
	{kind: "proto", file: "common.go", line: 3, typo: "gopackage", run: runProtocol,
		source: "package def\n\n//roost:proto package=game gopackage=example.com/pb\n\ntype Empty struct{}\n"},
	{kind: "protocol", file: "player.go", line: 3, typo: "grop", run: runProtocol,
		source: "package def\n\n//roost:protocol grop=player\ntype Player interface {\n\t//roost:msg id=1001\n\tPing(req *PingReq) *PingResp\n}\n\ntype PingReq struct{ A int32 `pb:\"1\"` }\ntype PingResp struct{ B int32 `pb:\"1\"` }\n"},
	{kind: "msg", file: "player.go", line: 5, typo: "nme", run: runProtocol,
		source: "package def\n\n//roost:protocol group=player\ntype Player interface {\n\t//roost:msg id=1001 nme=Pong\n\tPing(req *PingReq) *PingResp\n}\n\ntype PingReq struct{ A int32 `pb:\"1\"` }\ntype PingResp struct{ B int32 `pb:\"1\"` }\n"},
	{kind: "view", file: "view.go", line: 3, typo: "grp", run: runProtocol,
		source: "package def\n\n//roost:view grp=player\ntype PlayerView struct{ A int32 `pb:\"1\"` }\n"},
	{kind: "table", file: "item.go", line: 3, typo: "kye", run: runTable,
		source: "package meta\n\n//roost:table name=item kye=ID\ntype Item struct {\n\tID int64\n}\n"},
	{kind: "object", file: "cfg.go", line: 3, typo: "fil", run: runTable,
		source: "package meta\n\n//roost:object name=cfg fil=cfg.csv\ntype Cfg struct {\n\tA int64\n}\n"},
	{kind: "rpc", file: "svc.go", line: 10, typo: "capabilty", run: runRPC,
		source: fmt.Sprintf(rpcService, "capabilty=service.mail", "")},
	{kind: "rpc", file: "svc.go", line: 12, typo: "afinity", run: runRPC,
		source: fmt.Sprintf(rpcService, "capability=service.mail", "//roost:rpc afinity=req")},
	{kind: "entity", file: "player.go", line: 3, typo: "remot", run: runEntity,
		source: "package player\n\n//roost:entity entityKind=EntityKindPlayer remot=managed\ntype Player struct {\n\tID int64\n}\n"},
	{kind: "mirror", file: "guild.go", line: 3, typo: "colll", run: runEntity,
		source: "package guild\n\n//roost:mirror entityKind=EntityKindGuild colll=guild\ntype GuildMirror struct {\n\tID int64\n}\n"},
	{kind: "web", file: "route.go", line: 5, typo: "methd", run: runWeb,
		source: "package web\n\nimport \"context\"\n\n//roost:web methd=POST path=/x body=json\nfunc Handle(context.Context, *Service, Req) (Resp, error) { return Resp{}, nil }\n"},
	{kind: "register", file: "reg.go", line: 3, typo: "phse", run: runReg,
		source: "package planet\n\n//roost:register phse=entity\nfunc RegisterX() {}\n"},
}

func TestEveryMarkerRefusesAMisspeltKey(t *testing.T) {
	for _, tc := range typoCases {
		t.Run(tc.kind+"/"+tc.typo, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tc.file)
			if err := os.WriteFile(path, []byte(tc.source), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/planet\n\ngo 1.22\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := tc.run(t, root)
			if err == nil {
				t.Fatalf("//roost:%s with misspelt key %q was accepted", tc.kind, tc.typo)
			}
			for _, want := range []string{fmt.Sprintf("%q", tc.typo), fmt.Sprintf("%s:%d", tc.file, tc.line)} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error does not name %s:\n%v", want, err)
				}
			}
		})
	}
}

// TestEveryMarkerKindHasASpecAndAGuardCase keeps the list above complete: a
// generator that starts reading a new marker kind must give it a Spec (so
// CheckFile / Parse can refuse its typos) and a case in typoCases. It also
// refuses a hand-written `//[a-z]+:` pattern, which accepts any prefix and
// bypasses this package (G5: protocol and id.go used to do that).
func TestEveryMarkerKindHasASpecAndAGuardCase(t *testing.T) {
	specs := map[string]bool{}
	for _, spec := range marker.Specs {
		specs[spec.Kind] = true
	}
	guarded := map[string]bool{}
	for _, tc := range typoCases {
		guarded[tc.kind] = true
	}
	for kind := range specs {
		if !guarded[kind] {
			t.Errorf("marker.%s has no case in typoCases", kind)
		}
	}
	// Kinds read without options: the whole marker is a fixed phrase.
	optionless := map[string]bool{"reverse_proto": true}
	uses := regexp.MustCompile(`marker\.(?:Cut|Has|Regexp)\((?:[^,()]+,\s*)?"([a-z_]+)|hasMarker\([^,]+,\s*"([a-z_]+)"|markerKind\s*=\s*"([a-z_]+)"`)
	anyPrefix := regexp.MustCompile("//\\[a-z\\]\\+:")
	err := filepath.WalkDir("..", func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, "testdata") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if anyPrefix.Match(raw) {
			t.Errorf("%s matches markers with a hand-written //[a-z]+: pattern; use marker.Regexp / marker.Cut", path)
		}
		for _, match := range uses.FindAllStringSubmatch(string(raw), -1) {
			kind := match[1] + match[2] + match[3]
			if !specs[kind] && !optionless[kind] {
				t.Errorf("%s reads //roost:%s, which has no marker.Spec", path, kind)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
