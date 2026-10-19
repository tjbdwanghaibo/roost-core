package roost

import (
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/webroute"
)

// webRouteDir 是业务 HTTP 路由的包：project sync 在 webroute feature 打开时写 doc.go（type Service struct{}），
// 路由文件与 webroute 生成的 webroute_gen.go（(*Service).RegisterRoutes）都在这里。
const webRouteDir = "service/web"

// addWebRoute 是 roost add webroute <name> [-service svc]：写一个路由骨架（业务文件，只建一次），开启 webroute
// feature，给宿主服务装上 web Mod（wiring/web：Start 时 bind、service.started 时开始 Serve）并补它的配置段，
// 再跑一次 webroute 生成器，工程直接能编译。服务的 Init 归业务、不改写：next 提示给出要加的那一行
// kitweb.RegisterRoutes(registry, &web.Service{})，doctor 的 webroute:serve 在没人调用时 WARN。任何一步失败回滚已写文件。
func addWebRoute(root string, m Manifest, o AddOptions) ([]string, error) {
	snake, pascal := toSnake(o.Name), toPascal(o.Name)
	if !validName(snake) {
		return nil, fmt.Errorf("invalid webroute name %q", o.Name)
	}
	service, err := webRouteService(m, o.Service)
	if err != nil {
		return nil, err
	}
	routeRel := webRouteDir + "/" + snake + ".go"
	generatedRel := webRouteDir + "/" + webroute.GeneratedFileName
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(routeRel))); err == nil {
		return nil, fmt.Errorf("%s already exists", routeRel)
	}
	body, err := format.Source([]byte(renderWebRoute(snake, pascal)))
	if err != nil {
		return nil, err
	}
	backups, err := captureFiles(root, []string{routeRel, generatedRel})
	if err != nil {
		return nil, err
	}
	manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return nil, err
	}
	routePath := filepath.Join(root, filepath.FromSlash(routeRel))
	if err := os.MkdirAll(filepath.Dir(routePath), 0o755); err != nil {
		return nil, err
	}
	if err := writeAtomic(routePath, body, 0o644); err != nil {
		return nil, restoreFiles(root, backups, err)
	}
	if _, err := webroute.GenerateDir(filepath.Join(root, filepath.FromSlash(webRouteDir)), false); err != nil {
		return nil, restoreFiles(root, backups, fmt.Errorf("generate %s: %w", generatedRel, err))
	}
	created := []string{routeRel, generatedRel}
	spec := m.Services[service]
	if hasFeature(m, "webroute") && contains(spec.Mods, "web") {
		return created, nil
	}
	after, err := captureFiles(root, []string{routeRel, generatedRel})
	if err != nil {
		return nil, restoreFiles(root, backups, err)
	}
	previous := manifestWithService(m, service, ServiceSpec{Framework: spec.Framework, Mods: append([]string(nil), spec.Mods...), Uses: spec.Uses, Rpcs: spec.Rpcs, UsesRpcs: spec.UsesRpcs})
	spec.Mods = uniqueSorted(append(append([]string(nil), spec.Mods...), "web"))
	m.Services[service] = spec
	m.Features = uniqueSorted(append(m.Features, "webroute"))
	if err := commitManifestSync(root, manifestBefore, m); err != nil {
		return nil, restoreFilesIfCurrent(root, backups, after, err)
	}
	created = append(created, ManifestName, webRouteDir+"/doc.go", "internal/bootstrap/generated.go")
	configs, err := appendModConfigSections(root, previous, m, service, o.Warnings)
	if err != nil {
		return created, err
	}
	return append(created, configs...), nil
}

// webRouteService 是挂 web Mod 的业务服务：-service 显式给出；否则沿用已装 web Mod 的服务；否则工程只有一个
// 业务服务时用它。框架托管服务不能挂业务路由。
func webRouteService(m Manifest, requested string) (string, error) {
	service := toSnake(requested)
	if service == "" {
		var hosts, business []string
		for _, name := range sortedServiceNames(m) {
			if m.isFrameworkService(name) {
				continue
			}
			business = append(business, name)
			if contains(m.Services[name].Mods, "web") {
				hosts = append(hosts, name)
			}
		}
		switch {
		case len(hosts) == 1:
			service = hosts[0]
		case len(hosts) == 0 && len(business) == 1:
			service = business[0]
		default:
			return "", fmt.Errorf("webroute requires -service <service>: choose the business service that serves the routes")
		}
	}
	if _, ok := m.Services[service]; !ok {
		return "", fmt.Errorf("unknown service %q", service)
	}
	if m.isFrameworkService(service) {
		return "", fmt.Errorf("service %q hosts a framework service and cannot serve business routes", service)
	}
	return service, nil
}

func renderWebRoute(snake, pascal string) string {
	return fmt.Sprintf(`package web

import (
	"context"

	"github.com/tjbdwanghaibo/roost-core/infra/network/webroute"
)

// %[2]sResponse is what GET /%[1]s answers (JSON). Add the fields the caller needs.
type %[2]sResponse struct{}

// %[2]s is a business HTTP route (roost add webroute; a business file: generated
// once, yours afterwards). roost generate collects every //roost:web handler in
// this package into webroute_gen.go; the service registers them in its Init with
// kitweb.RegisterRoutes(registry, &web.Service{}) and the web mod serves them on
// web.addr. body=raw hands the handler the request as webroute.RawRequest (path,
// method, headers, body); body=json decodes one JSON document into a request
// type. The web mod does no authentication: keep web.addr on loopback or behind
// an authenticated proxy.
//
//roost:web method=GET path=/%[1]s body=raw
func %[2]s(ctx context.Context, svc *Service, request webroute.RawRequest) (%[2]sResponse, error) {
	return %[2]sResponse{}, nil
}
`, snake, pascal)
}

func printWebRouteNext(stdout io.Writer, module, service, route string) {
	fmt.Fprintf(stdout, "next: in internal/service/%s Init, serve the routes:\n", service)
	fmt.Fprintln(stdout, `      if err := kitweb.RegisterRoutes(registry, &web.Service{}); err != nil { return err }`)
	fmt.Fprintf(stdout, "      (imports: kitweb \"github.com/tjbdwanghaibo/roost-core/wiring/web\" and \"%s/%s\")\n", module, webRouteDir)
	fmt.Fprintf(stdout, "then: go run . %s and curl http://127.0.0.1:8080/%s (web.addr in configs/service/config.%s.yaml)\n", service, route, service)
}

// checkWebRouteServing 是 doctor 的 webroute:serve 项（只读，WARN 不让 doctor 失败）：service/web 里有生成的路由
// 注册（webroute_gen.go）时，要有服务在 Init 里调用 kitweb.RegisterRoutes（或生成的 RegisterRoutes）把它们交给 web Mod，
// 且那个服务装了 web Mod。忘了接线时进程照常起来、路由一条都不可达，没有任何报错，所以在这里提示。判据是文本
// （注释已去掉），只看“写了没有”。
func checkWebRouteServing(root string, m Manifest) []CheckItem {
	generated := filepath.Join(root, filepath.FromSlash(webRouteDir), webroute.GeneratedFileName)
	if _, err := os.Stat(generated); err != nil {
		return nil
	}
	sources, err := projectGoSources(root)
	if err != nil {
		return []CheckItem{{Name: "webroute:serve", Status: StatusWarn, Detail: "scan project sources: " + err.Error()}}
	}
	importPath := `"` + m.Project.Module + "/" + webRouteDir + `"`
	for _, service := range sortedServiceNames(m) {
		prefix := "internal/service/" + service + "/"
		for _, rel := range sortedKeys(sources) {
			body := sources[rel]
			if !strings.HasPrefix(rel, prefix) || !strings.Contains(body, importPath) || !strings.Contains(body, "RegisterRoutes(") {
				continue
			}
			if !contains(m.Services[service].Mods, "web") {
				return []CheckItem{{Name: "webroute:serve", Status: StatusWarn, Detail: rel + " registers the routes but service " + service + " has no web mod, so nothing listens; run: roost add mod web -service " + service}}
			}
			return []CheckItem{{Name: "webroute:serve", Status: StatusOK, Detail: "routes registered in " + rel + " and served by the web mod of service " + service}}
		}
	}
	return []CheckItem{{Name: "webroute:serve", Status: StatusWarn, Detail: webRouteDir + "/" + webroute.GeneratedFileName + " registers routes but no service hands them to the web mod, so none is reachable; in the service Init: if err := kitweb.RegisterRoutes(registry, &web.Service{}); err != nil { return err }"}}
}
