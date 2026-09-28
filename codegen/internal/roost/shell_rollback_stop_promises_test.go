package roost

// RR-20260928-12：install.sh / rollback.sh 切换 release 时，正在运行的进程要按它启动时的 unit
// 停机——TimeoutStopSec 是按那个 release 的 Mod 算出的停机预算（RR-20260926-66），换成别的
// release 的值会让停机中的在途请求被提前 SIGKILL。旧行为：先 use_unit（装入目标 unit 并
// daemon-reload）再 systemctl restart，restart 的 stop 用的是刚装载的目标 unit（systemd 语义），
// 于是 rollback.sh v1 时 v2 进程按 v1 unit 的预算被停，install.sh 升级时旧进程按新 unit 被停，
// 自动回滚时新进程按旧 unit 被停。另外 rollback.sh 切了 current 之后 use_unit 失败（set -e）
// 就退出，留下 current 已切换、unit 与进程都没换；install.sh 同样。
//
// 演练沿用 shell_rollback_unit_promises_test.go 的临时根与 PATH 替身（不调用真 systemctl，
// 不写系统目录）：替身记录每次停机时被停进程所在的 release、它启动时与停机时装载的 unit 的
// TimeoutStopSec，并可让一次 daemon-reload 失败。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stops returns the stop log since the last call: one entry per stopped
// process, "<release>|<TimeoutStopSec it started under>|<TimeoutStopSec it was stopped with>".
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
