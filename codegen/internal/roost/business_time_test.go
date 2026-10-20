package roost

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 业务时区（DESIGN-TODO D18）在生成工程里的三处：每个服务配置写明 time.business_timezone / time.business_day_start，
// 生成的 main 内嵌 time/tzdata，运行镜像里没有 zoneinfo 时也能加载 Asia/Shanghai。

func TestGeneratedConfigsWriteTheBusinessTimeZone(t *testing.T) {
	root := copyOfNewProject(t, "game-demo")
	matches, err := filepath.Glob(filepath.Join(root, "configs", "service", "config.*.yaml"))
	if err != nil || len(matches) < 4 {
		t.Fatalf("service configs = %v (%v)", matches, err)
	}
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if !strings.Contains(text, "\ntime:\n") || !strings.Contains(text, "\n  business_timezone: UTC\n") || !strings.Contains(text, "\n  business_day_start: 00:00\n") {
			t.Errorf("%s does not write time.business_timezone: UTC and time.business_day_start: 00:00", filepath.Base(path))
		}
	}
}

// blankImports 是 Go 源码里的 `_` 导入（只为副作用导入的包）。
func blankImports(t *testing.T, source string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse generated main: %v\n%s", err, source)
	}
	var paths []string
	for _, spec := range file.Imports {
		if spec.Name != nil && spec.Name.Name == "_" {
			path, _ := strconv.Unquote(spec.Path.Value)
			paths = append(paths, path)
		}
	}
	return paths
}

// timeZoneProbe 编一个只加载时区的小程序，带上 imports 里的副作用导入。
func timeZoneProbe(t *testing.T, ctx context.Context, name string, imports []string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"time\"\n")
	for _, path := range imports {
		b.WriteString("\t_ " + strconv.Quote(path) + "\n")
	}
	b.WriteString(")\n\nfunc main() {\n\tloc, err := time.LoadLocation(os.Args[1])\n\tif err != nil {\n\t\tfmt.Println(err)\n\t\tos.Exit(1)\n\t}\n\tfmt.Println(loc)\n}\n")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{"go.mod": "module probe\n\ngo 1.21\n", "main.go": b.String()} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(dir, "probe")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", binary, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s probe: %v\n%s", name, err, out)
	}
	return binary
}

// withoutZoneinfo 返回一条在看不到系统时区数据库的环境里运行 bin 的命令，模拟没有 zoneinfo 的运行镜像
// （distroless static）：TZ 清空、ZONEINFO 指向空目录；macOS 上再用 sandbox-exec 拒绝读取系统 zoneinfo 与
// GOROOT/lib/time。只设 ZONEINFO 不够：Go 在 ZONEINFO 里找不到时照样回落到 /usr/share/zoneinfo。
// 其他平台没有这种隔离，直接运行；调用方用不内嵌 tzdata 的对照程序判断这个环境是否真的看不到 zoneinfo。
func withoutZoneinfo(t *testing.T, ctx context.Context, dir, bin string, args ...string) *exec.Cmd {
	t.Helper()
	empty := t.TempDir()
	var env []string
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "TZ=") && !strings.HasPrefix(item, "ZONEINFO=") && !strings.HasPrefix(item, "GOROOT=") {
			env = append(env, item)
		}
	}
	env = append(env, "ZONEINFO="+empty)
	var cmd *exec.Cmd
	if sandbox, err := exec.LookPath("sandbox-exec"); err == nil && runtime.GOOS == "darwin" {
		goroot := runtime.GOROOT()
		if out, err := exec.CommandContext(ctx, "go", "env", "GOROOT").Output(); err == nil {
			goroot = strings.TrimSpace(string(out))
		}
		profile := `(version 1)(allow default)(deny file-read* (subpath "/usr/share/zoneinfo") (subpath "/var/db/timezone") (subpath "/private/var/db/timezone")`
		if goroot != "" {
			profile += ` (subpath ` + strconv.Quote(filepath.Join(goroot, "lib", "time")) + `)`
		}
		profile += `)`
		cmd = exec.CommandContext(ctx, sandbox, append([]string{"-p", profile, bin}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, bin, args...)
	}
	cmd.Dir, cmd.Env = dir, env
	return cmd
}

// requireZoneinfoHidden 用不内嵌 tzdata 的对照程序确认 withoutZoneinfo 真的让系统时区数据库不可见；
// 看得到（Linux 主机自带 zoneinfo、没有 sandbox-exec）就跳过，否则“内嵌后能加载”什么也证明不了。
func requireZoneinfoHidden(t *testing.T, ctx context.Context) {
	t.Helper()
	control := timeZoneProbe(t, ctx, "control", nil)
	if out, err := withoutZoneinfo(t, ctx, t.TempDir(), control, "Asia/Shanghai").CombinedOutput(); err == nil {
		t.Skipf("this host cannot hide its zoneinfo from a process (control without time/tzdata loaded Asia/Shanghai: %s)", strings.TrimSpace(string(out)))
	}
}

// 生成的 main 内嵌 time/tzdata：带上它的副作用导入的程序在看不到系统 zoneinfo 的环境里仍能加载 Asia/Shanghai；
// 对照程序（没有这些导入）在同一环境里加载失败，说明环境确实模拟了没有 zoneinfo 的镜像。
func TestGeneratedMainEmbedsTheTimeZoneDatabase(t *testing.T) {
	main := renderMain(Manifest{Project: ProjectSpec{Module: "example.com/tzprobe"}})
	imports := blankImports(t, main)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	requireZoneinfoHidden(t, ctx)
	probe := timeZoneProbe(t, ctx, "generated-main", imports)
	out, err := withoutZoneinfo(t, ctx, t.TempDir(), probe, "Asia/Shanghai").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "Asia/Shanghai" {
		t.Fatalf("with the generated main's imports %q and no zoneinfo on the host, loading Asia/Shanghai: %v: %s", imports, err, out)
	}
}
