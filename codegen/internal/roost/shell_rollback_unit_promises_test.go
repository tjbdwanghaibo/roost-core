package roost

// RR-20260928-10：install.sh 升级失败自动回滚、rollback.sh 手工回滚，都要让回退到的 release
// 在它自己安装时的 systemd unit 下运行。旧行为：两个脚本只切 current，unit 留在新版本写的那份；
// RR-20260928-05 把 WorkingDirectory 从 $APP_ROOT 改成了 $APP_ROOT/current，本版之前安装的
// release 里没有 configs/data，用 configdata 的服务回滚后照样 "stat dir configs/data" 起不来，
// install.sh 报 "rollback also failed readiness"，rollback.sh 报 "rollback target failed
// readiness"——哪怕运维为旧 unit 在 $APP_ROOT/configs/data 放好了数据表（临时根演练实测）。
//
// 这里真的执行生成的 install.sh / rollback.sh：PATH 前置替身代替 root、useradd 与 systemctl
// （daemon-reload 时装载磁盘上的 unit，restart 时按装载的 unit 在 WorkingDirectory 下起“进程”），
// 二进制是一个按工作目录找 configs/data/_manifest.json 的脚本，healthcheck.sh 换成看它的就绪文件。
// 不调用真 systemctl，不写任何系统目录（UNIT_PATH 在测试副本里改到临时目录，改不到就停）。

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type shellDeployRehearsal struct {
	t        *testing.T
	root     string // the project: deploy/shell, configs/data
	appRoot  string
	unitPath string
	ready    string // written by the fake binary: the directory it started in
	env      []string
}

func newShellDeployRehearsal(t *testing.T) *shellDeployRehearsal {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the generated shell deployment targets POSIX sh")
	}
	for _, tool := range []string{"sh", "sha256sum", "install", "readlink"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on PATH", tool)
		}
	}
	realInstall, _ := exec.LookPath("install")
	realMv, err := exec.LookPath("mv")
	if err != nil {
		t.Skip("no mv on PATH")
	}
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved // the fake binary reports pwd -P
	}
	r := &shellDeployRehearsal{
		t:        t,
		root:     filepath.Join(dir, "project"),
		appRoot:  filepath.Join(dir, "opt", "planet-game-1003"),
		unitPath: filepath.Join(dir, "etc", "planet-game-1003.service"),
		ready:    filepath.Join(dir, "ready"),
	}
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, script := range []string{"install.sh", "rollback.sh"} {
		body := string(plan["deploy/shell/"+script].Body)
		body = strings.ReplaceAll(body, "UNIT_PATH=/etc/systemd/system/$INSTANCE.service", `UNIT_PATH=${UNIT_PATH:?}`)
		if strings.Contains(body, "/etc/systemd") {
			t.Fatalf("deploy/shell/%s names a system unit path this rehearsal cannot redirect", script)
		}
		write(filepath.Join(r.root, "deploy", "shell", script), body, 0o644)
	}
	write(filepath.Join(r.root, "deploy", "shell", "healthcheck.sh"), "[ -f \"$ROOST_TEST_READY\" ]\n", 0o644)
	write(filepath.Join(r.root, "configs", "data", "_manifest.json"), "{}\n", 0o644)
	write(filepath.Join(dir, "config.game.yaml"), "sid: 1003\n", 0o644)
	write(filepath.Join(dir, "good"), "#!/bin/sh\nif [ -f configs/data/_manifest.json ]; then pwd -P > \"$ROOST_TEST_READY\"; else rm -f \"$ROOST_TEST_READY\"; fi\n", 0o755)
	write(filepath.Join(dir, "broken"), "#!/bin/sh\nrm -f \"$ROOST_TEST_READY\"\n", 0o755)

	stub := filepath.Join(dir, "stub")
	write(filepath.Join(stub, "id"), "#!/bin/sh\nif [ \"$1\" = -u ]; then echo 0; else exec /usr/bin/id \"$@\"; fi\n", 0o755)
	write(filepath.Join(stub, "getent"), "#!/bin/sh\nexit 0\n", 0o755)
	write(filepath.Join(stub, "useradd"), "#!/bin/sh\nexit 0\n", 0o755)
	// Not root: drop -o/-g. mv -T (GNU) renames onto the current link.
	write(filepath.Join(stub, "install"), `#!/bin/sh
args=""
while [ $# -gt 0 ]; do
  case "$1" in -o|-g) shift 2 ;; *) args="$args|$1"; shift ;; esac
done
IFS='|'; set -- ${args#|}; unset IFS
exec `+realInstall+` "$@"
`, 0o755)
	write(filepath.Join(stub, "mv"), `#!/bin/sh
if [ "$1" = -Tf ]; then rm -f "$3"; exec `+realMv+` -f "$2" "$3"; fi
exec `+realMv+` "$@"
`, 0o755)
	// systemd keeps the unit it loaded until daemon-reload; restart starts the
	// loaded unit's ExecStart in its WorkingDirectory.
	write(filepath.Join(stub, "systemctl"), `#!/bin/sh
case "$1" in
  daemon-reload) cp "$UNIT_PATH" "$ROOST_TEST_LOADED" ;;
  stop) rm -f "$ROOST_TEST_READY" ;;
  restart)
    rm -f "$ROOST_TEST_READY"
    wd=$(sed -n 's/^WorkingDirectory=//p' "$ROOST_TEST_LOADED")
    exec_start=$(sed -n 's/^ExecStart=//p' "$ROOST_TEST_LOADED")
    (cd "$wd" && $exec_start) || true ;;
esac
exit 0
`, 0o755)
	r.env = append(os.Environ(),
		"PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"),
		"APP_ROOT="+r.appRoot,
		"STATE_ROOT="+filepath.Join(dir, "state"),
		"LOG_ROOT="+filepath.Join(dir, "log"),
		"UNIT_PATH="+r.unitPath,
		"HEALTH_ATTEMPTS=1",
		"ROOST_TEST_READY="+r.ready,
		"ROOST_TEST_LOADED="+filepath.Join(dir, "loaded.service"),
	)
	return r
}

// run executes a generated deploy script; binary is the fake for install.sh's BINARY.
func (r *shellDeployRehearsal) run(binary string, args ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command("sh", args...)
	cmd.Dir = r.root
	cmd.Env = append(r.env, "BINARY="+filepath.Join(filepath.Dir(r.root), binary))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runningIn is the directory the fake process started in, or "" when it is not ready.
func (r *shellDeployRehearsal) runningIn() string {
	raw, err := os.ReadFile(r.ready)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// installLegacyRelease lays out what an install.sh from before RR-20260928-05
// left behind: release v1 with only the binary and config, current -> v1, a
// unit with WorkingDirectory=$APP_ROOT, and the tables the operator put in
// $APP_ROOT/configs/data for that unit. The service is running and ready.
func (r *shellDeployRehearsal) installLegacyRelease() {
	r.t.Helper()
	v1 := filepath.Join(r.appRoot, "releases", "v1")
	for _, d := range []string{v1, filepath.Join(r.appRoot, "configs", "data"), filepath.Dir(r.unitPath)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	good, err := os.ReadFile(filepath.Join(filepath.Dir(r.root), "good"))
	if err != nil {
		r.t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(v1, "planet"):                                   string(good),
		filepath.Join(v1, "config.yaml"):                              "sid: 1003\n",
		filepath.Join(r.appRoot, "configs", "data", "_manifest.json"): "{}\n",
		r.unitPath: "[Service]\nWorkingDirectory=" + r.appRoot + "\nExecStart=" + r.appRoot + "/current/planet game --sid 1003 --config " + r.appRoot + "/current/config.yaml\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := os.Symlink(v1, filepath.Join(r.appRoot, "current")); err != nil {
		r.t.Fatal(err)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"restart", "planet-game-1003.service"}} {
		cmd := exec.Command(filepath.Join(filepath.Dir(r.root), "stub", "systemctl"), args...)
		cmd.Env = r.env
		if out, err := cmd.CombinedOutput(); err != nil {
			r.t.Fatalf("stub systemctl %v: %v\n%s", args, err, out)
		}
	}
	if got := r.runningIn(); got != r.appRoot {
		r.t.Fatalf("legacy release is not running in %s before the upgrade (got %q)", r.appRoot, got)
	}
}

func TestShellRollbackRunsThePreviousReleaseUnderItsOwnUnit(t *testing.T) {
	r := newShellDeployRehearsal(t)
	r.installLegacyRelease()
	release := func(v string) string { return filepath.Join(r.appRoot, "releases", v) }

	out, err := r.run("broken", "deploy/shell/install.sh", "game", "1003", "v2", "../config.game.yaml")
	if err == nil || !strings.Contains(out, "rolling back") {
		t.Fatalf("install.sh v2 with a binary that never gets ready did not roll back (err=%v)\n%s", err, out)
	}
	if got := r.runningIn(); got != r.appRoot || strings.Contains(out, "rollback also failed readiness") {
		t.Errorf("install.sh's automatic rollback to v1 did not bring v1 back under the unit it was installed with\n"+
			"want running in %s (WorkingDirectory=$APP_ROOT, where the operator's configs/data is), got %q\n%s", r.appRoot, got, out)
	}

	if out, err := r.run("good", "deploy/shell/install.sh", "game", "1003", "v3", "../config.game.yaml"); err != nil {
		t.Fatalf("install.sh v3: %v\n%s", err, out)
	}
	if got := r.runningIn(); got != release("v3") {
		t.Fatalf("v3 is not running in its release: got %q, want %s", got, release("v3"))
	}

	out, err = r.run("", "deploy/shell/rollback.sh", "game", "1003", "v1")
	if got := r.runningIn(); err != nil || got != r.appRoot {
		t.Errorf("rollback.sh v1 did not run v1 under the unit it was installed with: err=%v, want running in %s, got %q\n%s", err, r.appRoot, got, out)
	}
	out, err = r.run("", "deploy/shell/rollback.sh", "game", "1003", "v3")
	if got := r.runningIn(); err != nil || got != release("v3") {
		t.Errorf("rollback.sh v3 did not run v3 under its own unit again: err=%v, want running in %s, got %q\n%s", err, release("v3"), got, out)
	}
}
