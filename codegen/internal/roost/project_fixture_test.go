package roost

// RR-20260928-14：生成工程夹具缓存。
//
// 本包的回归大多先 NewProject 一个完整工程再断言。game-demo 有 470 个文件，NewProject 经 writeAtomic
// 逐个 fsync，本机约 6.5s、Windows CI 更慢；二十多个用例各生成一次，再加几十个用例各生成一个小工程，
// 整包在 windows-compatibility 上超过了 go test 默认的 10 分钟（run 36358695147）。这些用例要的只是
// “NewProject 生成出来的那棵树”，不是 NewProject 的写盘过程本身，所以同一组参数（下面登记的四种形状）
// 只真实生成一次，之后每个用例拿到一份私有副本，随便改、互不影响。形状不在登记表里的用例照旧直接
// 调用 NewProject。
//
// 为什么不改变测试语义：
//   - 副本与一次新鲜的 NewProject 逐字节、逐权限位相同，由 TestProjectFixtureCopiesMatchAFreshNewProject
//     对下面每一种夹具实跑证明（生成物不含目标绝对路径，也不依赖时间）。
//   - 每个用例的副本在自己的 t.TempDir 里，路径形状与原来的 filepath.Join(t.TempDir(), "planet") 相同。
//   - NewProject 本身（暂存、改名提交、Windows 上的改名重试）仍由直接调用它的用例覆盖：
//     TestDemoTemplateGeneratesABuildableWritePath、TestNewProjectSyncPreservesBusinessFiles 等，
//     以及上面的等价性用例。
//   - 生成失败时，每个取用该夹具的用例都以同一个错误失败，与原来各自 NewProject 失败相同。
//
// 同一编号还给本包耗时 0.5s 以上、且满足下列条件的用例加了 t.Parallel：只读写自己 t.TempDir 里的工程，
// 路径全是绝对路径，不用 t.Setenv / chdir / 包级钩子（syncProjectBeforeCommit、buildVersion），
// 不改 os.Stdout / os.Stderr。Generate 内部的 os.Chdir 由 generatorWorkingDirectory 串行化，
// 这正是它为库调用方并发而设的保护。顶层并行用例只会在全部串行用例结束后一起跑，
// 所以留在串行的用例（用钩子的 TestSyncRefusesAConfigEditedWhileItRuns、读相对路径的
// literal_coupling_test.go、RR-20260927-01 在 Windows 上靠持有句柄注入写失败的两条）不受影响。

import (
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// projectFixtureOptions 是允许缓存的全部 NewOptions（Out 由缓存决定）。只收已登记的形状，
// 等价性用例据此逐一核对；新增形状时加在这里即可自动被核对。
var projectFixtureOptions = map[string]NewOptions{
	"game-demo": {
		Name: "planet", Module: "example.com/planet",
		Mods: []string{"configdata", "mongo", "nats", "dataengine", "nest"}, Template: demoTemplateName,
	},
	"saga":       {Name: "planet", Module: "example.com/planet", Mods: []string{"configdata"}, Features: []string{"saga"}},
	"configdata": {Name: "planet", Module: "example.com/planet", Mods: []string{"configdata"}},
	"bare":       {Name: "planet", Module: "example.com/planet"},
}

// projectFixtureOptionsFor 返回 name 的 NewOptions 深拷贝，避免并发的 NewProject 共用切片。
func projectFixtureOptionsFor(name string) (NewOptions, bool) {
	options, ok := projectFixtureOptions[name]
	options.Services = slices.Clone(options.Services)
	options.Mods = slices.Clone(options.Mods)
	options.Features = slices.Clone(options.Features)
	return options, ok
}

type projectFixture struct {
	once sync.Once
	root string
	err  error
}

var (
	projectFixturesMu  sync.Mutex
	projectFixtures    = map[string]*projectFixture{}
	projectFixtureBase string // 首次生成时创建，TestMain 在整包结束后删除
)

func TestMain(m *testing.M) {
	code := m.Run()
	projectFixturesMu.Lock()
	if projectFixtureBase != "" {
		_ = os.RemoveAll(projectFixtureBase)
	}
	projectFixturesMu.Unlock()
	os.Exit(code)
}

// generatedProjectFixture 返回 name 对应夹具的只读原件目录，只真实生成一次。调用方不得修改它。
func generatedProjectFixture(name string) (string, error) {
	options, ok := projectFixtureOptionsFor(name)
	if !ok {
		return "", fmt.Errorf("unknown project fixture %q", name)
	}
	projectFixturesMu.Lock()
	fixture := projectFixtures[name]
	if fixture == nil {
		fixture = &projectFixture{}
		projectFixtures[name] = fixture
	}
	projectFixturesMu.Unlock()
	fixture.once.Do(func() {
		projectFixturesMu.Lock()
		if projectFixtureBase == "" {
			projectFixtureBase, fixture.err = os.MkdirTemp("", "roost-project-fixtures-*")
		}
		base := projectFixtureBase
		projectFixturesMu.Unlock()
		if fixture.err != nil {
			return
		}
		options.Out = filepath.Join(base, name, options.Name)
		_, fixture.root, fixture.err = NewProject(options)
	})
	return fixture.root, fixture.err
}

// copyOfNewProject 等价于
//
//	target := filepath.Join(t.TempDir(), options.Name)
//	_, root, err := NewProject(options with Out: target)
//
// 返回的工程是本用例私有的可写副本。
func copyOfNewProject(t *testing.T, fixture string) string {
	t.Helper()
	source, err := generatedProjectFixture(fixture)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), projectFixtureOptions[fixture].Name)
	if err := copyProjectTree(source, target); err != nil {
		t.Fatal(err)
	}
	return target
}

// copyProjectTree 复制目录树，保留每个文件和目录的权限位。生成工程里只有普通文件和目录，
// 遇到其他类型直接报错，不去猜怎么复制。
func copyProjectTree(source, target string) error {
	var dirs []string
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			dirs = append(dirs, rel)
			return nil
		case info.Mode().IsRegular():
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dest, raw, info.Mode().Perm()); err != nil {
				return err
			}
			// os.WriteFile 受 umask 影响，显式设回原件的权限位。
			return os.Chmod(dest, info.Mode().Perm())
		default:
			return fmt.Errorf("project fixture %s: %s is neither a file nor a directory (%s)", source, rel, info.Mode().Type())
		}
	})
	if err != nil {
		return err
	}
	// 目录权限最后从深到浅设回，免得先收紧的父目录挡住子项的写入。
	for i := len(dirs) - 1; i >= 0; i-- {
		info, err := os.Stat(filepath.Join(source, dirs[i]))
		if err != nil {
			return err
		}
		if err := os.Chmod(filepath.Join(target, dirs[i]), info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

type projectTreeEntry struct {
	Mode fs.FileMode
	Body []byte
}

func snapshotProjectTree(t *testing.T, root string) map[string]projectTreeEntry {
	t.Helper()
	tree := map[string]projectTreeEntry{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item := projectTreeEntry{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			if item.Body, err = os.ReadFile(path); err != nil {
				return err
			}
		}
		tree[filepath.ToSlash(rel)] = item
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// 每一种夹具的副本都必须与同参数的一次新鲜 NewProject 完全相同：同样的路径集合、同样的文件内容、
// 同样的类型与权限位。它是上面“缓存不改变测试语义”的证据；生成物一旦开始依赖目标路径或时间，
// 这里先红。
func TestProjectFixtureCopiesMatchAFreshNewProject(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(projectFixtureOptions)) {
		t.Run(name, func(t *testing.T) {
			options, _ := projectFixtureOptionsFor(name)
			options.Out = filepath.Join(t.TempDir(), "fresh", options.Name)
			_, fresh, err := NewProject(options)
			if err != nil {
				t.Fatal(err)
			}
			want := snapshotProjectTree(t, fresh)
			got := snapshotProjectTree(t, copyOfNewProject(t, name))
			for rel, w := range want {
				g, ok := got[rel]
				switch {
				case !ok:
					t.Errorf("%s: the copy lacks %s", name, rel)
				case g.Mode != w.Mode:
					t.Errorf("%s: %s has mode %v in the copy, %v when freshly generated", name, rel, g.Mode, w.Mode)
				case !bytes.Equal(g.Body, w.Body):
					t.Errorf("%s: %s differs from a fresh NewProject:\n%s", name, rel, firstLineDifference(string(w.Body), string(g.Body)))
				}
			}
			for rel := range got {
				if _, ok := want[rel]; !ok {
					t.Errorf("%s: the copy has %s, a fresh NewProject does not", name, rel)
				}
			}
			if len(want) < 50 {
				t.Fatalf("%s: a fresh NewProject produced only %d entries; the comparison would prove nothing", name, len(want))
			}
		})
	}
}
