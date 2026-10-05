package roost

// C6（维护者决定，docs/feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md）：生成工程的每个托管服务默认把
// servicemetrics 事件写进进程的 metrics 注册表（ops /metrics 导出），而不是 Metrics() 返回 nil、
// 事件无处落地（N06 观察 1、N12 O1）。承诺：
//   - 生成器写的 collaborators.go 与 game-demo 自带的 collaborators.go，Metrics() 都返回
//     servicemetrics.NewMetricsReporter("<服务名>")；
//   - 注释说明怎么关：返回 nil，或配置 service_metrics.enabled: false。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var metricsCollaborator = regexp.MustCompile(`func Metrics\(\) servicemetrics\.Reporter \{\s*return ([^\n}]*)\}`)

func TestGeneratedServicesReportIntoTheMetricsRegistryByDefault(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range sortedServiceNames(m) {
		if !m.isFrameworkService(name) {
			continue
		}
		rel := "internal/service/" + name + "/collaborators.go"
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		match := metricsCollaborator.FindStringSubmatch(string(raw))
		if match == nil {
			t.Fatalf("%s has no Metrics() collaborator", rel)
		}
		want := `servicemetrics.NewMetricsReporter("` + name + `")`
		if got := strings.TrimSpace(match[1]); got != want {
			t.Fatalf("%s: Metrics() returns %s, want %s: a nil reporter leaves the service's events with nowhere to go", rel, got, want)
		}
		if !strings.Contains(string(raw), "service_metrics.enabled") {
			t.Fatalf("%s does not say how to turn the default reporter off", rel)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("game-demo hosts no framework service")
	}
	// A project the generator writes from scratch (not the demo's own files).
	plain := Manifest{Project: ProjectSpec{Name: "plain"}, Services: map[string]ServiceSpec{"rank": {Framework: "rank"}}}
	match := metricsCollaborator.FindStringSubmatch(renderFrameworkCollaborators(plain, "rank"))
	if match == nil || strings.TrimSpace(match[1]) != `servicemetrics.NewMetricsReporter("rank")` {
		t.Fatalf("the generated rank collaborators return %v, want the default reporter", match)
	}
}
