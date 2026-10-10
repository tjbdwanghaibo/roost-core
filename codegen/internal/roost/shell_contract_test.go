package roost

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func shellCaseArms(install, needle string) []string {
	var services []string
	arm := regexp.MustCompile(`(?m)^  ([a-zA-Z0-9_|-]+)\)\n((?:    .*\n)+?)    ;;`)
	for _, match := range arm.FindAllStringSubmatch(install, -1) {
		if strings.Contains(match[2], needle) {
			services = append(services, strings.Split(match[1], "|")...)
		}
	}
	return services
}

func assertShellInstallCarriesConfigData(t *testing.T, m Manifest, file func(rel string) (string, bool)) {
	t.Helper()
	install, _ := file("deploy/shell/install.sh")
	release := systemdReleaseDir(t, install)
	createRelease := strings.Index(install, `install -d -m 0755 "$APP_ROOT/releases" "$RELEASE_ROOT"`)
	if createRelease < 0 {
		t.Fatal("install.sh no longer creates $RELEASE_ROOT the way this test expects")
	}
	for _, service := range sortedServiceNames(m) {
		dirs := map[string]bool{}
		for _, rel := range []string{
			"configs/service/config." + service + ".yaml",
			"configs/service/config." + service + ".prod.example.yaml",
		} {
			body, exists := file(rel)
			if !exists {
				t.Fatalf("%s not generated", rel)
			}
			if dir, ok := configDataDirOf(t, rel, body); ok {
				if path.IsAbs(dir) {
					t.Fatalf("%s: config_data.dir %q is absolute; this test covers the generated relative form", rel, dir)
				}
				dirs[dir] = true
			}
		}
		copyLine := func(dir string) string { return `cp -R "$CONFIG_DATA" "` + path.Join(release, dir) + `"` }
		if len(dirs) == 0 {
			if contains(shellCaseArms(install, `cp -R "$CONFIG_DATA"`), service) {
				t.Errorf("service %s does not use configdata, but install.sh copies config data for it", service)
			}
			continue
		}
		for dir := range dirs {
			if !strings.Contains(install, `CONFIG_DATA=${CONFIG_DATA:-"$ROOT/`+dir+`"}`) {
				t.Errorf("service %s: install.sh does not take the tables from the project's %s", service, dir)
			}
			if !contains(shellCaseArms(install, copyLine(dir)), service) {
				t.Errorf("service %s: config_data.dir %q resolves to %s under systemd (WorkingDirectory=%s), and install.sh does not install the tables there\nwant, in the %s arm: %s",
					service, dir, path.Join(release, dir), systemdUnitValue(t, install, "WorkingDirectory"), service, copyLine(dir))
			}
		}
		guard := strings.Index(install, `[ -f "$CONFIG_DATA/_manifest.json" ]`)
		if guard < 0 || guard > createRelease || !contains(shellCaseArms(install, `[ -f "$CONFIG_DATA/_manifest.json" ]`), service) {
			t.Errorf("service %s: install.sh does not refuse a missing %s/_manifest.json before creating the release", service, defaultConfigDataDir)
		}
	}
	if !contains(allProjectMods(m), "configdata") && strings.Contains(install, "CONFIG_DATA") {
		t.Errorf("project without configdata still handles config data in install.sh")
	}
}

func TestShellInstallPutsConfigDataWhereConfigDataDirResolves(t *testing.T) {
	t.Run("game-demo on disk", func(t *testing.T) {
		root := newGameDemo(t)
		m, err := LoadManifest(root)
		if err != nil {
			t.Fatal(err)
		}
		assertShellInstallCarriesConfigData(t, m, func(rel string) (string, bool) {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			return string(raw), err == nil
		})
	})
	for name, m := range map[string]Manifest{
		"default mods":       DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil),
		"configdata only":    DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil),
		"without configdata": DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"nest"}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := renderProject(m)
			if err != nil {
				t.Fatal(err)
			}
			assertShellInstallCarriesConfigData(t, m, func(rel string) (string, bool) {
				f, ok := plan[rel]
				return string(f.Body), ok
			})
		})
	}
}

func (r *shellDeployRehearsal) stops() []string {
	r.t.Helper()
	path := filepath.Join(filepath.Dir(r.root), "stops.log")
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		r.t.Fatal(err)
	}
	_ = os.Remove(path)
	return strings.Fields(string(raw))
}

// requireStopsUnderOwnUnit checks that every process stopped by step was
// stopped with the TimeoutStopSec of the unit it was started under, and that
// the releases stopped are exactly want (in order).
func (r *shellDeployRehearsal) requireStopsUnderOwnUnit(step string, want ...string) {
	r.t.Helper()
	stops := r.stops()
	var released []string
	for _, stop := range stops {
		fields := strings.Split(stop, "|")
		if len(fields) != 3 {
			r.t.Fatalf("%s: malformed stop record %q", step, stop)
		}
		released = append(released, fields[0])
		if fields[1] != fields[2] {
			r.t.Errorf("%s: the process of %s was stopped with TimeoutStopSec=%s, but it was started under a unit with TimeoutStopSec=%s",
				step, strings.ReplaceAll(fields[0], r.appRoot, "$APP_ROOT"), fields[2], fields[1])
		}
	}
	if strings.Join(released, " ") != strings.Join(want, " ") {
		r.t.Errorf("%s: stopped %v, want %v", step, released, want)
	}
}

func TestShellReleaseSwitchStopsTheRunningProcessUnderItsOwnUnit(t *testing.T) {
	t.Parallel()
	r := newShellDeployRehearsal(t)
	r.installLegacyRelease()
	r.stops()
	release := func(v string) string { return filepath.Join(r.appRoot, "releases", v) }
	v1 := r.appRoot // the legacy unit runs v1 in WorkingDirectory=$APP_ROOT

	// Upgrade: v1 (legacy unit, TimeoutStopSec=7s) is stopped with its own budget.
	if out, err := r.run("good", "deploy/shell/install.sh", "game", "1003", "v2", "../config.game.yaml"); err != nil {
		t.Fatalf("install.sh v2: %v\n%s", err, out)
	}
	r.requireStopsUnderOwnUnit("install.sh v2", v1)

	// Manual rollback: v2 is stopped with v2's budget, not v1's.
	if out, err := r.run("", "deploy/shell/rollback.sh", "game", "1003", "v1"); err != nil || r.runningIn() != r.appRoot {
		t.Fatalf("rollback.sh v1: err=%v running in %q\n%s", err, r.runningIn(), out)
	}
	r.requireStopsUnderOwnUnit("rollback.sh v1", release("v2"))

	// A failed upgrade from v1: v1 is stopped with its own budget, then the
	// broken v3 with v3's while install.sh rolls back.
	out, err := r.run("broken", "deploy/shell/install.sh", "game", "1003", "v3", "../config.game.yaml")
	if err == nil || !strings.Contains(out, "rolling back") || r.runningIn() != r.appRoot {
		t.Fatalf("install.sh v3 (broken) did not roll back to v1: err=%v running in %q\n%s", err, r.runningIn(), out)
	}
	r.requireStopsUnderOwnUnit("install.sh v3 (broken), automatic rollback", v1, release("v3"))

	// rollback.sh to a release that fails readiness restores the previous one;
	// both stops use the stopped process's own unit.
	broken, err := os.ReadFile(filepath.Join(filepath.Dir(r.root), "broken"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release("v2"), "planet"), broken, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err = r.run("", "deploy/shell/rollback.sh", "game", "1003", "v2")
	if err == nil || !strings.Contains(out, "restored previous release") || r.runningIn() != r.appRoot {
		t.Fatalf("rollback.sh v2 (made broken) did not restore v1: err=%v running in %q\n%s", err, r.runningIn(), out)
	}
	r.requireStopsUnderOwnUnit("rollback.sh v2 (broken), restore", v1, release("v2"))
}

func TestShellReleaseSwitchRestoresCurrentAndUnitWhenTheUnitCannotBeInstalled(t *testing.T) {
	t.Parallel()
	r := newShellDeployRehearsal(t)
	r.installLegacyRelease()
	if out, err := r.run("good", "deploy/shell/install.sh", "game", "1003", "v2", "../config.game.yaml"); err != nil {
		t.Fatalf("install.sh v2: %v\n%s", err, out)
	}
	r.stops()
	release := func(v string) string { return filepath.Join(r.appRoot, "releases", v) }
	failReload := filepath.Join(filepath.Dir(r.root), "fail-reload")
	requireState := func(step, out string, err error, current string) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: exit 0 although the unit of the target release was not loaded\n%s", step, out)
		}
		got, _ := filepath.EvalSymlinks(filepath.Join(r.appRoot, "current"))
		if got != current {
			t.Errorf("%s: current -> %q, want %q restored\n%s", step, got, current, out)
		}
		installed, _ := os.ReadFile(r.unitPath)
		recorded, _ := os.ReadFile(filepath.Join(r.appRoot, "units", filepath.Base(current)+".service"))
		loaded, _ := os.ReadFile(filepath.Join(filepath.Dir(r.root), "loaded.service"))
		if len(recorded) == 0 || string(installed) != string(recorded) || string(loaded) != string(recorded) {
			t.Errorf("%s: the unit of %s is not restored: installed=%q loaded=%q recorded=%q",
				step, filepath.Base(current), installed, loaded, recorded)
		}
		if r.runningIn() != current {
			t.Errorf("%s: running in %q, want %s\n%s", step, r.runningIn(), current, out)
		}
	}

	if err := os.WriteFile(failReload, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := r.run("", "deploy/shell/rollback.sh", "game", "1003", "v1")
	requireState("rollback.sh v1 with a failing daemon-reload", out, err, release("v2"))
	r.requireStopsUnderOwnUnit("rollback.sh v1 with a failing daemon-reload", release("v2"))

	if err := os.WriteFile(failReload, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = r.run("good", "deploy/shell/install.sh", "game", "1003", "v3", "../config.game.yaml")
	requireState("install.sh v3 with a failing daemon-reload", out, err, release("v2"))
	r.requireStopsUnderOwnUnit("install.sh v3 with a failing daemon-reload", release("v2"))
}

// OPEN-ITEMS audit8 D4：rollback.sh 的版本号是 releases/ 下的目录名，"." 与 ".." 指向
// releases 本身或 APP_ROOT，不是已安装的 release，必须在动任何东西之前拒绝。
func TestShellRollbackRejectsDotVersions(t *testing.T) {
	t.Parallel()
	r := newShellDeployRehearsal(t)
	r.installLegacyRelease()
	for _, version := range []string{".", ".."} {
		out, err := r.run("", "deploy/shell/rollback.sh", "game", "1003", version)
		if err == nil || !strings.Contains(out, "invalid version") {
			t.Errorf("rollback.sh %q was not rejected as an invalid version: err=%v\n%s", version, err, out)
		}
		if got, _ := filepath.EvalSymlinks(filepath.Join(r.appRoot, "current")); got != filepath.Join(r.appRoot, "releases", "v1") {
			t.Errorf("rollback.sh %q moved current to %q", version, got)
		}
	}
}

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
	// systemd keeps the unit it loaded until daemon-reload; start runs the
	// loaded unit's ExecStart in its WorkingDirectory, and is a no-op while the
	// service is already running; stop stops it with the TimeoutStopSec of the
	// unit loaded at that moment (RR-20260928-12), restart is stop + start.
	// ROOST_TEST_RUNNING holds the running process: its release directory and
	// the TimeoutStopSec it was started under; every stop appends
	// "<release>|<TimeoutStopSec at start>|<TimeoutStopSec at stop>" to
	// ROOST_TEST_STOPS. A daemon-reload fails once, loading nothing, while
	// ROOST_TEST_FAIL_RELOAD exists.
	write(filepath.Join(stub, "systemctl"), `#!/bin/sh
stop_running() {
  if [ -f "$ROOST_TEST_RUNNING" ]; then
    tss=$(sed -n 's/^TimeoutStopSec=//p' "$ROOST_TEST_LOADED")
    printf '%s|%s\n' "$(cat "$ROOST_TEST_RUNNING")" "${tss:-unset}" >> "$ROOST_TEST_STOPS"
    rm -f "$ROOST_TEST_RUNNING"
  fi
  rm -f "$ROOST_TEST_READY"
}
start_loaded() {
  [ ! -f "$ROOST_TEST_RUNNING" ] || return 0
  wd=$(sed -n 's/^WorkingDirectory=//p' "$ROOST_TEST_LOADED")
  exec_start=$(sed -n 's/^ExecStart=//p' "$ROOST_TEST_LOADED")
  tss=$(sed -n 's/^TimeoutStopSec=//p' "$ROOST_TEST_LOADED")
  printf '%s|%s' "$(cd "$wd" && pwd -P)" "${tss:-unset}" > "$ROOST_TEST_RUNNING"
  (cd "$wd" && $exec_start) || true
}
case "$1" in
  daemon-reload)
    if [ -f "$ROOST_TEST_FAIL_RELOAD" ]; then rm -f "$ROOST_TEST_FAIL_RELOAD"; exit 1; fi
    cp "$UNIT_PATH" "$ROOST_TEST_LOADED" ;;
  stop) stop_running ;;
  start) start_loaded ;;
  restart) stop_running; start_loaded ;;
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
		"ROOST_TEST_RUNNING="+filepath.Join(dir, "running"),
		"ROOST_TEST_STOPS="+filepath.Join(dir, "stops.log"),
		"ROOST_TEST_FAIL_RELOAD="+filepath.Join(dir, "fail-reload"),
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

// legacyStopTimeout is the TimeoutStopSec of the legacy unit: not what the
// generator renders for this manifest, so a stop under the wrong unit shows.
const legacyStopTimeout = "7s"

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
		r.unitPath: "[Service]\nWorkingDirectory=" + r.appRoot + "\nExecStart=" + r.appRoot + "/current/planet game --sid 1003 --config " + r.appRoot + "/current/config.yaml\nTimeoutStopSec=" + legacyStopTimeout + "\n",
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
	t.Parallel()
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
