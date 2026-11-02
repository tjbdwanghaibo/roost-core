package dao

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	entitygen "github.com/tjbdwanghaibo/roost-core/codegen/internal/entity"
	nestgen "github.com/tjbdwanghaibo/roost-core/codegen/internal/nest"
)

func TestRemoteViewGeneratedConsumer(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for path, fixture := range map[string]string{"db/def/alliance.go": "definition.go.txt", "game/alliance/alliance.go": "entity.go.txt", "game/requests/alliance.go": "request.go.txt", "game/handler/handler.go": "handler.go.txt", "game/alliance/views_test.go": "consumer_test.go.txt"} {
		raw, err := os.ReadFile(filepath.Join("testdata/remoteviews", fixture))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, raw, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/remoteviews\n\ngo 1.27.0\nrequire github.com/tjbdwanghaibo/roost-core v0.0.0\nreplace github.com/tjbdwanghaibo/roost-core => "+root+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-def", filepath.Join(dir, "db/def"), "-out", filepath.Join(dir, "db"), "-pkg", "db"}
	generate := func() {
		t.Helper()
		if err := Run(args, io.Discard); err != nil {
			t.Fatal(err)
		}
		if err := entitygen.Run([]string{"-dir", filepath.Join(dir, "game")}, io.Discard); err != nil {
			t.Fatal(err)
		}
		if err := nestgen.Run([]string{"-dir", filepath.Join(dir, "game")}, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	generate()
	path := filepath.Join(dir, "db", remoteViewsFileName)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	generate()
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("remote views drift")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=mod", "-race", "-count=2", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated consumer: %v\n%s", err, out)
	}
	// 升级字段形状后 scope 保持不变，但旧 schema 必须被拒绝，不能误解码为新投影。
	identity := regexp.MustCompile(`(?s)func \(AlliancePublicSnapshot\) RemoteSchema.*?RemoteSnapshotScope\("([^"]+)"\)`).FindSubmatch(first)
	if len(identity) != 2 {
		t.Fatal("missing generated schema identity")
	}
	defPath := filepath.Join(dir, "db/def/alliance.go")
	definition, err := os.ReadFile(defPath)
	if err != nil {
		t.Fatal(err)
	}
	definition = bytes.Replace(definition, []byte("type AllianceDao struct {"), []byte("type AllianceDao struct {\n Added int64 `store:\"added\" remoteview:\"public\"`"), 1)
	if err := os.WriteFile(defPath, definition, 0644); err != nil {
		t.Fatal(err)
	}
	generate()
	upgrade := fmt.Sprintf(`package alliance_test
import (
 "errors"
 "testing"
 "github.com/tjbdwanghaibo/roost-core/framework/entity"
 "example.com/remoteviews/db"
 "example.com/remoteviews/game/alliance"
)
func TestOldViewSchemaRejected(t *testing.T){
 alliance.RegisterEntity()
 oldSchema:=entity.RemoteSnapshotSchema(alliance.Kind,entity.RemoteSnapshotScope(%q))
 current:=db.AlliancePublicSnapshot{}
 if current.RemoteSchema(alliance.Kind)==oldSchema {t.Fatal("field change kept old schema")}
 if current.RemoteScope()!=uint64(entity.RemoteSnapshotScope("remoteview:alliances:public")){t.Fatal("field change altered scope")}
 _,err:=entity.DecodeRemoteSnapshot(entity.RemoteSnapshotEnvelope{Schema:oldSchema})
 if !errors.Is(err,entity.ErrRemoteSnapshotDecoderMissing){t.Fatalf("old schema accepted: %%v",err)}
}
`, string(identity[1]))
	if err := os.WriteFile(filepath.Join(dir, "game/alliance/upgrade_test.go"), []byte(upgrade), 0644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.CommandContext(ctx, "go", "test", "-mod=mod", "-race", "-run", "TestOldViewSchemaRejected", "./game/alliance")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("schema upgrade: %v\n%s", err, out)
	}
	// 最后一个声明删除后不留旧投影文件。
	def := filepath.Join(dir, "db/def/alliance.go")
	raw, err := os.ReadFile(def)
	if err != nil {
		t.Fatal(err)
	}
	raw = regexp.MustCompile(`remoteview:"[^"]*"`).ReplaceAll(raw, nil)
	if err := os.WriteFile(def, raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("orphan remote views: %v", err)
	}
}

func TestRemoteViewDeclarationsFailClosed(t *testing.T) {
	cases := []struct{ name, source, want string }{
		{"empty", "//roost:dao coll=a db=game\ntype A struct{ X int `store:\"x\" remoteview:\"\"` }", "invalid remoteview"},
		{"duplicate", "//roost:dao coll=a db=game\ntype A struct{ X int `store:\"x\" remoteview:\"public,public\"` }", "duplicate remoteview"},
		{"skipped", "//roost:dao coll=a db=game\ntype A struct{ X int `store:\"x\" remoteview:\"public\" dao:\"-\"` }", "cannot be combined"},
		{"nonpersistent", "//roost:dao coll=a db=game\ntype A struct{ X int `store:\"x\" remoteview:\"public\" dao:\"nopersist,sync\"` }", "persisted field"},
		{"unexported", "//roost:dao coll=a db=game\ntype A struct{ x int `store:\"x\" remoteview:\"public\"` }", "exported named field"},
		{"multiple names", "//roost:dao coll=a db=game\ntype A struct{ X,Y int `store:\"x\" remoteview:\"public\"` }", "one exported"},
		{"recursive", "//roost:dao coll=a db=game\ntype A struct{ X *Node `store:\"x\" remoteview:\"public\"` }\n\ntype Node struct{ Next *Node `store:\"next\"` }", "recursive nested"},
		{"nested tag", "//roost:dao coll=a db=game\ntype A struct{ X *Node `store:\"x\" remoteview:\"public\"` }\n\ntype Node struct{ X int `store:\"x\" remoteview:\"public\"` }", "top-level"},
		{"unknown type", "//roost:dao coll=a db=game\ntype A struct{ X external.Value `store:\"x\" remoteview:\"public\" dao:\"persist,nosync\"` }", "unsupported remoteview"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package def\n"+tc.source), 0644); err != nil {
				t.Fatal(err)
			}
			err := Run([]string{"-def", dir, "-out", filepath.Join(dir, "out"), "-pkg", "db"}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %s", err, tc.want)
			}
		})
	}
}
