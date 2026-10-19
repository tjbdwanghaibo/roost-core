package roost

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// roost add webroute 修通生成链路（新用户第七轮：修前只写 service/web/<name>.go，引用的 *Service 未定义、包编译失败，
// roost.yaml 不开 webroute feature，生成的 RegisterRoutes 也没有任何东西提供服务）：开启 feature（doc.go 定义 Service）、
// 宿主服务装上 web Mod 并补 web: 配置段、立即生成 webroute_gen.go，工程对当前 roost-core 能编译；服务 Init 里加一行
// kitweb.RegisterRoutes 之后 doctor 的 webroute:serve 由 WARN 变 OK。
func TestAddWebRouteEnablesFeatureMountsWebModAndBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the generated project")
	}
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	var out bytes.Buffer
	if err := Run([]string{"add", "webroute", "ShopPrice", "--root", root}, &out, &out); err != nil {
		t.Fatalf("add webroute: %v\n%s", err, out.String())
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFeature(m, "webroute") {
		t.Errorf("add webroute did not enable the webroute feature: %v", m.Features)
	}
	if !contains(m.Services["game"].Mods, "web") {
		t.Errorf("add webroute did not mount the web mod on game: %v", m.Services["game"].Mods)
	}
	for _, rel := range []string{"service/web/shop_price.go", "service/web/doc.go", "service/web/webroute_gen.go"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("add webroute did not write %s: %v", rel, err)
		}
	}
	if config := readProjectFile(t, root, "configs/service/config.game.yaml"); !strings.Contains(config, "\nweb:\n") || !strings.Contains(config, "addr: 127.0.0.1:8080") {
		t.Errorf("config.game.yaml has no web: section:\n%s", config)
	}
	if bootstrap := readProjectFile(t, root, "internal/bootstrap/generated.go"); !strings.Contains(bootstrap, "kitweb.NewMod()") {
		t.Errorf("the bootstrap does not assemble the web mod:\n%s", bootstrap)
	}
	if !strings.Contains(out.String(), "kitweb.RegisterRoutes(registry, &web.Service{})") {
		t.Errorf("add webroute next hints lack the Init line:\n%s", out.String())
	}
	if items := checkWebRouteServing(root, m); len(items) != 1 || items[0].Status != StatusWarn {
		t.Errorf("before the Init line: webroute:serve = %+v; want one WARN", items)
	}

	servicePath := filepath.Join(root, "internal", "service", "game", "service.go")
	service, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	wired := strings.Replace(string(service), "import (\n", "import (\n\tkitweb \"github.com/tjbdwanghaibo/roost-core/wiring/web\"\n\t\"example.com/planet/service/web\"\n", 1)
	wired, replaced := replaceInitBody(wired, "\tif err := kitweb.RegisterRoutes(registry, &web.Service{}); err != nil {\n\t\treturn err\n\t}\n")
	if !replaced {
		t.Fatalf("service.go has no empty Init; update this test:\n%s", service)
	}
	if err := os.WriteFile(servicePath, []byte(wired), 0o644); err != nil {
		t.Fatal(err)
	}
	if items := checkWebRouteServing(root, m); len(items) != 1 || items[0].Status != StatusOK {
		t.Errorf("after the Init line: webroute:serve = %+v; want OK", items)
	}
	useLocalRoostCore(t, root)
	for _, action := range []string{"build", "vet"} {
		check := exec.Command("go", action, "./...")
		check.Dir = root
		check.Env = append(os.Environ(), "GOWORK=off")
		if output, err := check.CombinedOutput(); err != nil {
			t.Fatalf("go %s after add webroute: %v\n%s", action, err, output)
		}
	}
	// 第二条路由沿用同一个服务，不再改 roost.yaml。
	if err := Run([]string{"add", "webroute", "ShopStock", "--root", root}, io.Discard, io.Discard); err != nil {
		t.Fatalf("second add webroute: %v", err)
	}
	if gen := readProjectFile(t, root, "service/web/webroute_gen.go"); !strings.Contains(gen, `"/shop_price"`) || !strings.Contains(gen, `"/shop_stock"`) {
		t.Errorf("webroute_gen.go lacks a route:\n%s", gen)
	}
}

// replaceInitBody 把新工程服务骨架的空 Init 换成执行 lines 的 Init。
func replaceInitBody(source, lines string) (string, bool) {
	const empty = "func (*Service) Init(*app.Registry) error        { return nil }"
	if !strings.Contains(source, empty) {
		return source, false
	}
	return strings.Replace(source, empty, "func (*Service) Init(registry *app.Registry) error {\n"+lines+"\treturn nil\n}", 1), true
}
