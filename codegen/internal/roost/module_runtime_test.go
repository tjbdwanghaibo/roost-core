package roost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestModuleRuntimeInstallationIsTransactional(t *testing.T) {
	t.Parallel()
	root, roost, try := moduleProject(t, "library")
	roost("add", "access", "player", "--service", "game")
	servicePath := filepath.Join(root, "internal/service/game/service.go")
	service, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	service = append(service, []byte("\n// 业务文件必须在模块安装后原样保留。\n")...)
	if err := os.WriteFile(servicePath, service, 0644); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(root, "game/handler/zz_broken.go")
	if err := os.WriteFile(broken, []byte("package handler\n//roost:nest rollback=undo durability=strict\nfunc handlerBroken()error{return nil}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before := moduleProjectDigest(t, root)
	if out, err := try("module", "add", "skillbook"); err == nil || !strings.Contains(err.Error(), "generate after installing modules") {
		t.Fatalf("failed install: %v\n%s", err, out)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("failed install left module source, manifest or generated wiring behind")
	}
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	var plan ModulePlan
	if err := json.Unmarshal([]byte(roost("module", "plan", "skillbook", "--json")), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Modules) != 1 || plan.Modules[0].Runtime == nil || plan.Modules[0].Runtime.Package != "game/skillbook" {
		t.Fatalf("missing runtime plan: %+v", plan)
	}
	roost("module", "add", "skillbook")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	installed := manifest.Modules["skillbook"]
	if installed.Service != "game" || installed.Runtime == nil || installed.Runtime.Start != "Start" {
		t.Fatalf("missing runtime ownership: %+v", installed)
	}
	current, err := os.ReadFile(servicePath)
	if err != nil || string(current) != string(service) {
		t.Fatalf("business service changed: %v", err)
	}
	bootstrap, err := os.ReadFile(filepath.Join(root, "internal/bootstrap/generated.go"))
	if err != nil || !strings.Contains(string(bootstrap), "NewWithModules()") {
		t.Fatalf("bootstrap did not select the module service: %v", err)
	}
	if err := diffManifest(root, manifest, false, io.Discard); err != nil {
		t.Fatalf("doctor project-templates: %v", err)
	}
	before = moduleProjectDigest(t, root)
	roost("module", "add", "skillbook")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeated install changed the project")
	}
	if report, err := VerifyModules(context.Background(), root, []string{"skillbook"}, nil); err != nil || len(report.Modules) != 1 || report.Modules[0].Passed != 11 {
		t.Fatalf("verify: %v %+v", err, report)
	}
}

func TestModuleRuntimeOrderFollowsDependencies(t *testing.T) {
	t.Parallel()
	m := Manifest{Modules: map[string]InstalledModule{
		"consumer": {Service: "game", Requires: []string{"data"}, Runtime: &ModuleRuntime{Package: "game/consumer", Start: "Start"}},
		"data":     {Service: "game", Requires: []string{"store"}},
		"store":    {Service: "game", Runtime: &ModuleRuntime{Package: "game/store", Start: "Open"}},
		"other":    {Service: "other", Runtime: &ModuleRuntime{Package: "game/other", Start: "Start"}},
	}}
	order, err := moduleRuntimeOrder(m, "game")
	if err != nil || !reflect.DeepEqual(order, []string{"store", "consumer"}) {
		t.Fatalf("%v %v", order, err)
	}
	m.Modules["store"] = InstalledModule{Service: "game", Requires: []string{"consumer"}}
	if _, err := moduleRuntimeOrder(m, "game"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle: %v", err)
	}
	delete(m.Modules, "store")
	if _, err := moduleRuntimeOrder(m, "game"); err == nil || !strings.Contains(err.Error(), "store is not installed") {
		t.Fatalf("missing dependency: %v", err)
	}
	m.Modules["store"] = InstalledModule{Service: "other"}
	if _, err := moduleRuntimeOrder(m, "game"); err == nil || !strings.Contains(err.Error(), "service game") {
		t.Fatalf("cross-service dependency: %v", err)
	}
}

func TestModuleRuntimeRejectsInvalidEntrypoints(t *testing.T) {
	t.Parallel()
	for _, runtime := range []*ModuleRuntime{
		{Package: "../outside", Start: "Start"},
		{Package: "game/book/../other", Start: "Start"},
		{Package: "game/book", Start: "start"},
		{Package: "game/book", Start: "Start()"},
	} {
		if err := validateModuleRuntime(runtime); err == nil {
			t.Fatalf("invalid runtime accepted: %+v", runtime)
		}
	}
}

// 编译并执行实际生成的适配器，观察业务服务及两个模块的启动、失败和停机顺序。
func TestModuleRuntimeGeneratedLifecycle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Project: ProjectSpec{Module: "example.com/runtimefixture"}, Modules: map[string]InstalledModule{
		"consumer": {Service: "game", Requires: []string{"store"}, Runtime: &ModuleRuntime{Package: "game/consumer", Start: "Start"}},
		"store":    {Service: "game", Runtime: &ModuleRuntime{Package: "game/store", Start: "Start"}},
	}}
	body, err := renderModuleService(m, "game")
	if err != nil {
		t.Fatal(err)
	}
	if !canOverwriteGenerated("internal/service/game/modules_gen.go", []byte(body)) {
		t.Fatal("module adapter is not recognized as generator-owned")
	}
	files := map[string]string{
		"go.mod":                                fmt.Sprintf("module example.com/runtimefixture\n\ngo 1.26.0\n\nrequire github.com/tjbdwanghaibo/roost-core v1.44.0\nreplace github.com/tjbdwanghaibo/roost-core => %q\n", repo),
		"internal/service/game/modules_gen.go":  body,
		"internal/service/game/service_test.go": moduleRuntimeLifecycleTest,
		"game/probe/probe.go":                   moduleRuntimeProbe,
	}
	for _, name := range []string{"store", "consumer"} {
		files["game/"+name+"/runtime.go"] = fmt.Sprintf(`package %s
import ("context"; "github.com/tjbdwanghaibo/roost-core/framework/app"; "example.com/runtimefixture/game/probe")
func Start(*app.Registry)(func(context.Context)error,error){return probe.Start(%q)}
`, name, name)
	}
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "test", "-count=1", "./internal/service/game")
	cmd.Dir, cmd.Env = root, generatedProjectGoEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated lifecycle: %v\n%s", err, out)
	}
}

const moduleRuntimeProbe = `package probe
import("context";"errors")
var Events []string
var FailStart,FailStop string
var Failure=errors.New("failure")
func Record(event string){Events=append(Events,event)}
func Reset(){Events=nil;FailStart="";FailStop=""}
func Start(name string)(func(context.Context)error,error){
 Record("start "+name)
 stop:=func(context.Context)error{Record("stop "+name);if FailStop==name{return Failure};return nil}
 if FailStart==name{return stop,Failure};return stop,nil
}
`

const moduleRuntimeLifecycleTest = `package game
import("context";"errors";"reflect";"testing";"github.com/tjbdwanghaibo/roost-core/framework/app";"example.com/runtimefixture/game/probe")
type Service struct{}
func New()*Service{return &Service{}}
func(*Service)Name()app.ServiceName{return "game"}
func(*Service)Serve(context.Context)error{return nil}
func(*Service)Marker()bool{return true}
func(*Service)Init(*app.Registry)error{probe.Record("start business");if probe.FailStart=="business"{return probe.Failure};return nil}
func(*Service)Shutdown(context.Context)error{probe.Record("stop business");if probe.FailStop=="business"{return probe.Failure};return nil}
var _ app.Service=NewWithModules()
func events(t *testing.T,want ...string){t.Helper();if !reflect.DeepEqual(probe.Events,want){t.Fatalf("events %v, want %v",probe.Events,want)}}
func TestLifecycle(t *testing.T){
 probe.Reset();s:=NewWithModules()
 if !s.Marker(){t.Fatal("embedded methods lost")}
 if err:=s.Init(nil);err!=nil{t.Fatal(err)}
 if err:=s.Shutdown(context.Background());err!=nil{t.Fatal(err)}
 if err:=s.Shutdown(context.Background());err!=nil{t.Fatal(err)}
 events(t,"start store","start consumer","start business","stop business","stop consumer","stop store")
}
func TestPartialModuleStart(t *testing.T){
 probe.Reset();probe.FailStart="consumer";s:=NewWithModules()
 if err:=s.Init(nil);!errors.Is(err,probe.Failure){t.Fatal(err)}
 if err:=s.Shutdown(context.Background());err!=nil{t.Fatal(err)}
 events(t,"start store","start consumer","stop consumer","stop store")
}
func TestPartialBusinessStart(t *testing.T){
 probe.Reset();probe.FailStart="business";s:=NewWithModules()
 if err:=s.Init(nil);!errors.Is(err,probe.Failure){t.Fatal(err)}
 if err:=s.Shutdown(context.Background());err!=nil{t.Fatal(err)}
 events(t,"start store","start consumer","start business","stop business","stop consumer","stop store")
}
func TestBusinessStopFailurePreservesModules(t *testing.T){
 probe.Reset();s:=NewWithModules();if err:=s.Init(nil);err!=nil{t.Fatal(err)}
 probe.FailStop="business"
 if err:=s.Shutdown(context.Background());!errors.Is(err,probe.Failure){t.Fatal(err)}
 events(t,"start store","start consumer","start business","stop business")
 probe.FailStop="";if err:=s.Shutdown(context.Background());err!=nil{t.Fatal(err)}
 events(t,"start store","start consumer","start business","stop business","stop business","stop consumer","stop store")
}
func TestModuleStopFailurePreservesDependencies(t *testing.T){
 probe.Reset();s:=NewWithModules();if err:=s.Init(nil);err!=nil{t.Fatal(err)}
 probe.FailStop="consumer"
 if err:=s.Shutdown(context.Background());!errors.Is(err,probe.Failure){t.Fatal(err)}
 events(t,"start store","start consumer","start business","stop business","stop consumer")
 probe.FailStop="";if err:=s.Shutdown(context.Background());err!=nil{t.Fatal(err)}
 events(t,"start store","start consumer","start business","stop business","stop consumer","stop consumer","stop store")
}
`
