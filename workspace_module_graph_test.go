package roostcore_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 工作区门禁：roost-core 放进任何 go.work 都必须能加载全部包。
//
// 生成工程按源码 HEAD 编译的标准做法是 `go work init ./工程 <roost-core 检出>`（demo-publish、
// framework-compat source-head、upgrade-compat、GENERATED.md 给读者的说明都是这样）。工作区模式
// 加载完整模块图，不像单模块构建那样只看 go.mod 里列出的需求：kcp-go 依赖的
// github.com/tjfoc/gmsm v1.4.1 的 go.mod 是 go 1.14（不剪枝），经 grpc v1.31.0 → envoy
// go-control-plane 把 2019 年拆分前的 google.golang.org/genproto 带进模块图；它与 grpc / etcd
// 使用的拆分后模块 google.golang.org/genproto/googleapis/{api,rpc} 同时提供同一个包，所有
// 引到 etcd / grpc 的包都报 ambiguous import。
//
// 修法是让 roost-core 的 go.mod 自己要求拆分后的 genproto（版本与 googleapis/{api,rpc} 同一提交），
// 最小版本选择于是在任何包含 roost-core 的模块图里都选到不含这些包的版本。go mod tidy 只保留被
// import 的模块，所以由 genproto_pin_test.go 的空白 import 维持这条需求。
func TestModuleLoadsInsideAWorkspace(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("找不到 go 命令，工作区门禁不能静默放行：%v", err)
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "go.work")
	if err := os.WriteFile(work, []byte("go 1.27.0\n\nuse "+filepath.ToSlash(root)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goTool, "list", "-deps", "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK="+work, "GOFLAGS=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("roost-core 在 go.work 里加载包失败（%v）：\n%s", err, tail(stderr.Bytes()))
	}
}
