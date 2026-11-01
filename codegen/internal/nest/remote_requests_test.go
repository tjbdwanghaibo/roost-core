package nest

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// 真正编译并运行跨包消费工程，覆盖 Sender、共享 DTO、指针/值参数和 I/O→业务续行。
func TestRemoteRequestGeneratedConsumer(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := map[string]string{
		"game/requests/request.go": "request.go.txt", "game/view/view.go": "view.go.txt",
		"game/handler/handler.go": "handler.go.txt", "game/handler/second.go": "second.go.txt",
		"game/handler/runtime_test.go": "runtime_test.go.txt",
	}
	for target, fixture := range files {
		raw, err := os.ReadFile(filepath.Join("testdata/remote_requests", fixture))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, target)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
	}
	mod := "module example.com/remote-test\n\ngo 1.27.0\n\nrequire github.com/tjbdwanghaibo/roost-core v0.0.0\nreplace github.com/tjbdwanghaibo/roost-core => " + root + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-dir", filepath.Join(dir, "game")}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := Run(args, &second); err != nil {
		t.Fatal(err)
	}
	if second.String() != "all files up to date\n" {
		t.Fatalf("generation drift: %s", second.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=mod", "-race", "-count=2", "./game/...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated consumer: %v\n%s", err, out)
	}
	// 声明删除后旧访问器自动退出，不能残留一条过期预取路径。
	path := filepath.Join(dir, "game/requests/request.go")
	if err := os.WriteFile(path, []byte("package requests\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "game/requests/request_remote_nest_gen.go")); !os.IsNotExist(err) {
		t.Fatalf("orphan remote accessor: %v", err)
	}
}
