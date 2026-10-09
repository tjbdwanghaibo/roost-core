package main

import (
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// D-L3：业务包里直接读系统时钟（time.Now / Since / Until，含把 time.Now 当函数值传）打印提示；
// 豁免注释、非业务目录、测试文件不提示；提示不计入违例。
func writeModuleFile(t *testing.T, root, relative, source string) string {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(path)
}

func newModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeModuleFile(t, root, "go.mod", "module example.com/m\n")
	return root
}

func TestClockHintsFlagDirectReadsInABusinessPackage(t *testing.T) {
	root := newModule(t)
	dir := writeModuleFile(t, root, "game/activity/window.go", `package activity
import "time"
type runner struct{ now func() time.Time }
func Open() int64 { return time.Now().Unix() }
func Age(t0 time.Time) time.Duration { return time.Since(t0) }
func Left(t1 time.Time) time.Duration { return time.Until(t1) }
func New() runner { return runner{now: time.Now} }
`)
	if hints := clockHintsForDirectory(token.NewFileSet(), dir); hints != 4 {
		t.Fatalf("hints = %d, want 4 (Now, Since, Until and the time.Now function value)", hints)
	}
	// 生成工程的服务目录 internal/service/game 同样是业务包；改名导入也认。
	service := writeModuleFile(t, root, "internal/service/game/spawner.go", `package game
import stdtime "time"
func Died() stdtime.Time { return stdtime.Now() }
`)
	if hints := clockHintsForDirectory(token.NewFileSet(), service); hints != 1 {
		t.Fatalf("hints = %d, want 1 for a renamed time import", hints)
	}
}

func TestClockHintsHonourTheSystemClockDirective(t *testing.T) {
	root := newModule(t)
	dir := writeModuleFile(t, root, "game/owners/owners.go", `package owners
import "time"
func Lease() time.Time { return time.Now() } //glsvet:system-clock a residency lease
func Deadline() time.Time {
	//glsvet:system-clock a saga deadline
	return time.Now().Add(time.Minute)
}
// Backoff is retry timing.
//glsvet:system-clock retry backoff is system time
func Backoff(t0 time.Time) time.Duration {
	elapsed := time.Since(t0)

	return elapsed + time.Until(t0)
}
func Business() time.Time { return time.Now() }
`)
	if hints := clockHintsForDirectory(token.NewFileSet(), dir); hints != 1 {
		t.Fatalf("hints = %d, want 1 (only Business is unmarked)", hints)
	}
}

func TestClockHintsLeaveOtherPackagesAlone(t *testing.T) {
	root := newModule(t)
	// 框架 / 基础设施包：不提示。
	framework := writeModuleFile(t, root, "framework/nest/ticker.go", `package nest
import "time"
func Tick() time.Time { return time.Now() }
`)
	if hints := clockHintsForDirectory(token.NewFileSet(), framework); hints != 0 {
		t.Fatalf("hints = %d in a non-business package, want 0", hints)
	}
	// 同名局部变量不是 time 包；测试文件默认跳过。
	dir := writeModuleFile(t, root, "game/local/local.go", `package local
type clock struct{}
func (clock) Now() int { return 0 }
func Read() int { time := clock{}; return time.Now() }
`)
	writeModuleFile(t, root, "game/local/local_test.go", `package local
import "time"
func helper() time.Time { return time.Now() }
`)
	if hints := clockHintsForDirectory(token.NewFileSet(), dir); hints != 0 {
		t.Fatalf("hints = %d, want 0 for a local variable named time and a skipped test file", hints)
	}
	// 关掉开关不提示；-businessdirs 可以换目录名。
	game := writeModuleFile(t, root, "game/off/off.go", `package off
import "time"
func Read() time.Time { return time.Now() }
`)
	*clockHints = false
	t.Cleanup(func() { *clockHints = true })
	if hints := clockHintsForDirectory(token.NewFileSet(), game); hints != 0 {
		t.Fatalf("hints = %d with -clockhints=false, want 0", hints)
	}
	*clockHints = true
	*businessDirs = "gameplay"
	t.Cleanup(func() { *businessDirs = "game" })
	if hints := clockHintsForDirectory(token.NewFileSet(), game); hints != 0 {
		t.Fatalf("hints = %d after -businessdirs dropped game, want 0", hints)
	}
}

// 仓库本身放在名叫 game 的目录下时，模块根之上的那一段不算：只看模块根之下的路径。
func TestClockHintsLookOnlyBelowTheModuleRoot(t *testing.T) {
	outer := filepath.Join(t.TempDir(), "game")
	root := filepath.Join(outer, "repo")
	writeModuleFile(t, root, "go.mod", "module example.com/m\n")
	dir := writeModuleFile(t, root, "infra/clock.go", `package infra
import "time"
func Read() time.Time { return time.Now() }
`)
	if hints := clockHintsForDirectory(token.NewFileSet(), dir); hints != 0 {
		t.Fatalf("hints = %d for a package whose only game segment is above the module root, want 0", hints)
	}
}

// 提示不改退出码：vetDirectory 的违例数里不含它。
func TestClockHintsAreNotFindings(t *testing.T) {
	root := newModule(t)
	dir := writeModuleFile(t, root, "game/activity/window.go", `package activity
import "time"
func Open() int64 { return time.Now().Unix() }
`)
	findings, err := vetDirectory(token.NewFileSet(), dir)
	if err != nil || findings != 0 {
		t.Fatalf("findings = %d err = %v, want 0: a clock hint must not fail the run", findings, err)
	}
}

func clockHintsForDirectory(fileSet *token.FileSet, directory string) int {
	before := hintCount
	_, _ = vetDirectory(fileSet, directory)
	return hintCount - before
}
