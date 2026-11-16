package protocol

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// game/controllers 是一个包：存根写进 <分组>.go，生成的 controller.go 放 Controller；方法在包里任何文件都算已有，
// 业务移动、合并方法后不会被重建。
func TestControllerScaffoldsPreserveBusinessAndFollowMethods(t *testing.T) {
	root := t.TempDir()
	defs := &Definitions{ModulePath: "example.com/game", Messages: []MsgDef{{Name: "Equip", Req: "EquipRequest", Resp: "EquipResponse", Handler: "player", ReqID: 1}}}
	if err := generateControllers(root, defs, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "player")); !os.IsNotExist(err) {
		t.Fatalf("a per-group subdirectory was created: %v", err)
	}
	controller, err := os.ReadFile(filepath.Join(root, ControllerFileName))
	if err != nil || !bytes.HasPrefix(controller, []byte(protocolGeneratedHeader+"\n")) || !strings.Contains(string(controller), "package controllers") ||
		!strings.Contains(string(controller), "func FromRegistry(") || !strings.Contains(string(controller), "func moduleState[T any](") {
		t.Fatalf("generated controller.go: %v\n%s", err, controller)
	}
	path := filepath.Join(root, "player.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !IsControllerScaffold("game/controllers/player.go", raw) || !strings.Contains(string(raw), "package controllers") ||
		!strings.Contains(string(raw), "HandleEquip(ctx *playeraccess.Context, req *pb.EquipRequest) (*pb.EquipResponse, error)") {
		t.Fatalf("wrong group file: %s", raw)
	}
	business := strings.ReplaceAll(string(raw), `fmt.Errorf("HandleEquip: not implemented")`, `fmt.Errorf("business implementation")`)
	if err := os.WriteFile(filepath.Join(root, "business.go"), []byte(business), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	defs.Messages = append(defs.Messages, MsgDef{Name: "Move", Req: "MoveRequest", Handler: "player", ReqID: 2})
	if err := generateControllers(root, defs, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "business.go"))
	if string(after) != business {
		t.Fatal("business overwritten")
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "HandleEquip") || !strings.Contains(string(raw), "HandleMove(ctx *playeraccess.Context, req *pb.MoveRequest) error") {
		t.Fatalf("group file should hold only the missing one-way stub: %s", raw)
	}
	again := append([]byte(nil), raw...)
	if err := generateControllers(root, defs, io.Discard); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); !bytes.Equal(raw, again) {
		t.Fatalf("a second run changed the group file:\n%s", raw)
	}
}

// 缺少的存根追加在已有 <分组>.go 末尾：原来的字节原样保留（只在 import 之后插一个 import 块），同一次的
// 多个协议合在这一个文件里；协议生成器的暂存提交（IsControllerStubAppend）认得这种追加，别的改动不认。
func TestControllerStubsAppendToTheGroupFile(t *testing.T) {
	root := t.TempDir()
	original := "package controllers\n\nimport \"context\"\n\n// chatState 是模块自己的代码，生成器不能动。\nvar chatState = context.Background\n"
	path := filepath.Join(root, "chat.go")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	defs := &Definitions{ModulePath: "example.com/game", Messages: []MsgDef{
		{Name: "ChatSend", Req: "ChatSendRequest", Resp: "ChatSendResponse", Handler: "chat", ReqID: 2},
		{Name: "ChatHistory", Req: "ChatHistoryRequest", Resp: "ChatHistoryResponse", Handler: "chat", ReqID: 1},
	}}
	if err := generateControllers(root, defs, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	head := "package controllers\n\nimport \"context\""
	if !strings.HasPrefix(body, head) || !strings.Contains(body, "\n\n// chatState 是模块自己的代码，生成器不能动。\nvar chatState = context.Background\n") {
		t.Fatalf("existing bytes were changed:\n%s", body)
	}
	history, send := strings.Index(body, "func (controller *Controller) HandleChatHistory("), strings.Index(body, "func (controller *Controller) HandleChatSend(")
	if history < 0 || send < history || history < strings.Index(body, "var chatState") {
		t.Fatalf("both stubs must be appended after the module code, in message id order:\n%s", body)
	}
	for _, imp := range []string{`"fmt"`, `playeraccess "` + PlayerAccessImport + `"`, `pb "example.com/game/protocol/pb"`} {
		if !strings.Contains(body, imp) {
			t.Fatalf("missing import %s:\n%s", imp, body)
		}
	}
	if _, err := formatGo(raw); err != nil {
		t.Fatalf("appended file does not parse: %v\n%s", err, raw)
	}
	if strings.HasPrefix(body, ControllerScaffoldMarker) {
		t.Fatal("an application file must not gain the scaffold marker")
	}
	if !IsControllerStubAppend([]byte(original), raw) {
		t.Fatal("the generator's own append is not recognised")
	}
	edited := bytes.Replace(raw, []byte("var chatState = context.Background"), []byte("var chatState = context.TODO"), 1)
	if IsControllerStubAppend([]byte(original), edited) {
		t.Fatal("a change to the application's own code was taken for a stub append")
	}
	business := bytes.Replace(raw, []byte(`return nil, fmt.Errorf("HandleChatSend: not implemented")`), []byte(`return &pb.ChatSendResponse{}, nil`), 1)
	if IsControllerStubAppend([]byte(original), business) {
		t.Fatal("an implemented method was taken for a stub")
	}
	if IsControllerStubAppend(raw, raw) {
		t.Fatal("an unchanged file is not an append")
	}
}

// controller.go 归生成器：业务手写的同名文件不覆盖；业务文件里再声明 Controller / FromRegistry、或者还留着
// 旧的按分组子目录时，全包检查一次就报错，什么都不写。
func TestControllerPackageChecksRunOnceForTheWholePackage(t *testing.T) {
	defs := &Definitions{ModulePath: "example.com/game", Messages: []MsgDef{{Name: "Equip", Req: "EquipRequest", Handler: "player"}}}
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"hand-written controller.go":    {map[string]string{"controller.go": "package controllers\n\ntype Controller struct{}\n"}, "not the generated controller"},
		"Controller in a module file":   {map[string]string{"chat.go": "package controllers\n\ntype Controller struct{}\n"}, "declares Controller"},
		"FromRegistry in a module file": {map[string]string{"chat.go": "package controllers\n\nfunc FromRegistry() {}\n"}, "declares FromRegistry"},
		"method declared twice":         {map[string]string{"a.go": "package controllers\n\nfunc (c *Controller) HandleX() {}\n", "b.go": "package controllers\n\nfunc (c *Controller) HandleX() {}\n"}, "declared in both"},
		"wrong package":                 {map[string]string{"chat.go": "package chat\n"}, "every file in game/controllers is package controllers"},
		"old per-group subdirectory":    {map[string]string{"chat/controller.go": "package chat\n"}, "without subdirectories"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for rel, body := range tc.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := generateControllers(root, defs, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("generateControllers = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(root, "player.go")); !os.IsNotExist(err) {
				t.Fatal("a stub file was written although the package check failed")
			}
			for rel, body := range tc.files {
				if raw, _ := os.ReadFile(filepath.Join(root, rel)); string(raw) != body {
					t.Fatalf("%s was changed", rel)
				}
			}
		})
	}
	// handler=controller 会撞上生成的 controller.go，handler=x_test 会变成测试文件：都拒绝。
	for _, group := range []string{"controller", "x_test"} {
		if _, err := ControllerGroupFile(group); err == nil {
			t.Fatalf("group %q accepted", group)
		}
	}
}

// 生成的 controller.go 自身要能编译成 controllers 包并带上公共 API。
func TestControllerSourceDeclaresTheSharedAPI(t *testing.T) {
	raw, err := ControllerSource()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"package controllers", "type Controller struct", "func FromRegistry(registry *app.Registry) (*Controller, error)",
		"func (controller *Controller) NestClient() corenest.Client", "func (controller *Controller) Registry() *app.Registry",
		`const PlayerTCPCapability app.ModName = "access.player.tcp"`, "type PlayerPusher interface",
		"func LookupPlayerTransport[T any](registry *app.Registry) (T, error)",
		"func PushToPlayer(ctx context.Context, registry *app.Registry, playerID int64, messageID uint32, value any) error",
		"func (controller *Controller) PushToPlayer(", "func moduleState[T any](controller *Controller, module string, build func() *T) *T",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("controller.go lacks %q:\n%s", want, raw)
		}
	}
}

// 模块在 init 里用 requireAtStart 登记启动检查；生成的协议登记在 FromRegistry 之后调 CheckAtStart，
// 任何一个失败进程就起不来。这里把生成的 controller.go 编进一个临时工程，按真实的 init 登记跑一遍：
// 检查按模块名排序执行、全部失败一起报（带模块名）、FromRegistry 自己不跑检查、重复登记 panic。
func TestControllerStartChecksRunInModuleOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a generated package")
	}
	repo, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	modFile, err := os.ReadFile(filepath.Join(repo, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	goVersion := ""
	for _, line := range strings.Split(string(modFile), "\n") {
		if version, ok := strings.CutPrefix(line, "go "); ok {
			goVersion = strings.TrimSpace(version)
		}
	}
	root := t.TempDir()
	controller, err := ControllerSource()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":                         "module example.com/g\n\ngo " + goVersion + "\n\nrequire github.com/tjbdwanghaibo/roost-core v0.0.0\n\nreplace github.com/tjbdwanghaibo/roost-core => " + repo + "\n",
		"game/controllers/controller.go": string(controller),
		// 两个模块，登记顺序与名字顺序相反：执行顺序必须按名字。
		"game/controllers/zeta.go": `package controllers

import "errors"

func init() {
	requireAtStart("zeta", func(c *Controller) error {
		ran = append(ran, "zeta")
		if failing["zeta"] {
			return errors.New("zeta dependency missing")
		}
		return nil
	})
}
`,
		"game/controllers/alpha.go": `package controllers

import "errors"

var (
	ran     []string
	failing = map[string]bool{}
	sawNest bool
)

func init() {
	requireAtStart("alpha", func(c *Controller) error {
		ran = append(ran, "alpha")
		sawNest = c.Registry() != nil && c.NestClient() != nil
		if failing["alpha"] {
			return errors.New("alpha dependency missing")
		}
		return nil
	})
}
`,
		"game/controllers/start_test.go": `package controllers

import (
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

type fakeNest struct{ corenest.Client }

func TestStartChecks(t *testing.T) {
	registry := app.NewRegistry(nil)
	if err := registry.Register(app.ModName("nest"), corenest.Client(fakeNest{})); err != nil {
		t.Fatal(err)
	}
	failing["alpha"], failing["zeta"] = true, true
	controller, err := FromRegistry(registry)
	if err != nil || len(ran) != 0 {
		t.Fatalf("FromRegistry = %v, ran %v; it builds the Controller only, the start checks belong to CheckAtStart", err, ran)
	}
	err = controller.CheckAtStart()
	if err == nil {
		t.Fatal("CheckAtStart passed although alpha and zeta report missing dependencies: the process would start and fail on the first request")
	}
	text := err.Error()
	alpha, zeta := strings.Index(text, "module alpha: alpha dependency missing"), strings.Index(text, "module zeta: zeta dependency missing")
	if alpha < 0 || zeta < 0 || alpha > zeta {
		t.Fatalf("CheckAtStart error = %q; want both failures, each with its module, alpha first", text)
	}
	if strings.Join(ran, ",") != "alpha,zeta" || !sawNest {
		t.Fatalf("checks ran %v (saw registry and nest client: %v); want alpha,zeta on the Controller FromRegistry built", ran, sawNest)
	}
	failing["alpha"], failing["zeta"], ran = false, false, nil
	if err := controller.CheckAtStart(); err != nil || len(ran) != 2 {
		t.Fatalf("CheckAtStart with every dependency present = %v, ran %v", err, ran)
	}
	if err := (*Controller)(nil).CheckAtStart(); err == nil {
		t.Fatal("CheckAtStart accepted a nil Controller")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("registering the zeta start check twice did not panic")
		}
	}()
	requireAtStart("zeta", func(*Controller) error { return nil })
}
`,
	}
	if sum, err := os.ReadFile(filepath.Join(repo, "go.sum")); err == nil {
		files["go.sum"] = string(sum)
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "test", "-count=1", "-run", "^TestStartChecks$", "./game/controllers")
	cmd.Dir, cmd.Env = root, append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start checks in the generated package: %v\n%s", err, out)
	}
}

// 生成的协议登记先建 Controller，再跑模块的启动检查，之后才注册协议：检查失败时一条协议都不注册，进程起不来。
func TestProtocolRegistrationRunsStartChecksBeforeRegistering(t *testing.T) {
	defs := &Definitions{ModulePath: "example.com/game", Messages: []MsgDef{{Name: "Ping", Req: "PingRequest", Resp: "PingResponse", Handler: "ping", ReqID: 1}}}
	content, err := generateProtocolRegistration(defs, "player")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	build, check, register := strings.Index(text, "ctl, err := controllers.FromRegistry(registry)"), strings.Index(text, "if err := ctl.CheckAtStart(); err != nil {"), strings.Index(text, "RegisterPingPlayerProtocolsWith(protocols, ctl)")
	if build < 0 || check < build || register < check {
		t.Fatalf("registration does not build the Controller, run its start checks, then register (%d, %d, %d):\n%s", build, check, register, text)
	}
}

// roost add endpoint 就地接管“未实现”占位：只替换那一个方法（连同文档注释），文件其余字节不动；补上新方法要的
// import，占位独用的 fmt 不再用到时去掉（否则编译不过）。改过的方法不算占位。
func TestReplaceControllerMethodTakesOverOnlyThePlaceholder(t *testing.T) {
	root := t.TempDir()
	defs := &Definitions{ModulePath: "example.com/game", Messages: []MsgDef{{Name: "Grant", Req: "GrantRequest", Resp: "GrantResponse", Handler: "player"}}}
	head := "package controllers\n\n// keep 是业务代码。\nfunc keep() int { return 1 }\n"
	if err := os.WriteFile(filepath.Join(root, "player.go"), []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := generateControllers(root, defs, io.Discard); err != nil {
		t.Fatal(err)
	}
	method, err := FindControllerMethod(root, "HandleGrant")
	if err != nil || method == nil || !method.Placeholder || filepath.Base(method.Path) != "player.go" {
		t.Fatalf("placeholder lookup = %+v, %v", method, err)
	}
	if missing, err := FindControllerMethod(root, "HandleOther"); err != nil || missing != nil {
		t.Fatalf("absent method = %+v, %v", missing, err)
	}
	replacement := "//roost:endpoint nest=Grant\nfunc (controller *Controller) HandleGrant(ctx *playeraccess.Context, req *pb.GrantRequest) (*pb.GrantResponse, error) {\n\treturn controller.dispatchGrant(ctx, req)\n}\n"
	out, err := ReplaceControllerMethod(method, []GoImport{{Name: "playeraccess", Path: PlayerAccessImport}, {Path: "example.com/game/protocol/pb"}}, replacement)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if !strings.HasPrefix(body, "package controllers") || !strings.Contains(body, "// keep 是业务代码。\nfunc keep() int { return 1 }\n") ||
		!strings.Contains(body, "dispatchGrant") || strings.Contains(body, "not implemented") || strings.Contains(body, "由业务实现") || strings.Contains(body, `"fmt"`) {
		t.Fatalf("replacement:\n%s", body)
	}
	if _, err := formatGo(out); err != nil {
		t.Fatalf("replaced file does not parse: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "player.go"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	if method, err := FindControllerMethod(root, "HandleGrant"); err != nil || method == nil || method.Placeholder {
		t.Fatalf("an implemented method was taken for the placeholder: %+v, %v", method, err)
	}
}
