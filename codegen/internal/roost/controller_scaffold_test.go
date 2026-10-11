package roost

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestControllerScaffoldTransactionalGeneration(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a generated player-access project")
	}
	root := copyOfNewProject(t, "configdata")
	for _, step := range []AddOptions{{Kind: "access", Name: "player", Service: "game"}, {Kind: "protocol", Name: "Hello", Group: "game", Handler: "player"}} {
		if _, err := Add(root, step); err != nil {
			t.Fatal(err)
		}
	}
	coreRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
	}
	run("mod", "edit", "-replace", generatedCoreModule+"="+coreRoot)
	generate := func() {
		t.Helper()
		if err := GenerateTransactional(ctx, root, GenerateOptions{Stdout: io.Discard}, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	generate()
	path := filepath.Join(root, "game/controllers/player/hello.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "HandleHello(ctx *player_agent.Context, req *pb.HelloRequest)") {
		t.Fatalf("missing typed controller: %s", raw)
	}
	// 模拟实际业务响应，真实编译生成的注册链与 Controller 类型。
	business := strings.ReplaceAll(string(raw), `"fmt"`, "")
	business = strings.ReplaceAll(business, `return nil, fmt.Errorf("HandleHello: not implemented")`, `return &pb.HelloResponse{}, nil`)
	if err := os.WriteFile(path, []byte(business), 0600); err != nil {
		t.Fatal(err)
	}
	generate()
	after, _ := os.ReadFile(path)
	if string(after) != business {
		t.Fatal("business overwritten")
	}
	run("build", "./...")
	if err := os.WriteFile(filepath.Join(root, "game/player_agent/manual_reply_test.go"), []byte(manualReplyTest), 0600); err != nil {
		t.Fatal(err)
	}
	run("test", "./game/player_agent", "-run", "^TestManualReplyDoesNotEncodeAgain$", "-count=1")
	if err := Generate(root, GenerateOptions{Check: true}); err != nil {
		t.Fatal(err)
	}
	// 注册生成物仍存在时，缺失业务骨架也必须被 --check 识别。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Check: true}); err == nil || !strings.Contains(err.Error(), "hello.go") {
		t.Fatalf("missing scaffold not detected: %v", err)
	}
	generate()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

const manualReplyTest = `package player_agent
import (
 "context"
 "testing"
 "github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
)
type responseSession struct { replies int }
func (*responseSession) Principal() gateway.Principal { return gateway.Principal{PlayerID:1,SessionID:"test"} }
func (s *responseSession) Reply(context.Context,any) error {s.replies++;return nil}
func (*responseSession) Close(error) error {return nil}
func TestManualReplyDoesNotEncodeAgain(t *testing.T) {
 registry:=NewProtocolRegistry()
 type reply struct{}
 encoded:=0
 if err:=RegisterProtocol[int,*reply](registry,1,2,func([]byte)(int,error){return 0,nil},func(*reply)([]byte,error){encoded++;return nil,nil},func(ctx *Context,_ int)(*reply,error){return nil,ctx.Session.Reply(ctx.Context(),&Response{MessageID:2,Sequence:ctx.Seq})});err!=nil {t.Fatal(err)}
 session:=&responseSession{}
 response,err:=registry.Dispatch(context.Background(),session,1,7,nil)
 if err!=nil || response!=nil || encoded!=0 || session.replies!=1 {t.Fatalf("response=%v err=%v encoded=%d replies=%d",response,err,encoded,session.replies)}
}
`
