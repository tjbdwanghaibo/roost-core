package roost

// RR-20260926-80：roost project doctor 与 project sync 对停机窗口的三条承诺。
//
//  1. doctor 对仓库里的三份配置（config.<svc>.yaml、.prod.example.yaml、k8s secret 示例）逐份判定
//     total_timeout 能否覆盖 Mod 保底，任一份覆盖不了即 WARN 并指明文件。旧行为只读 config.<svc>.yaml：
//     按提示只改了它、prod / secret 示例仍是 60s 时给 OK，按示例部署每次停机都告警。
//  2. doctor 的 OK / WARN 行显示磁盘上部署模板的实际宽限期。旧行为打印按当前配置算出的计划值，
//     与磁盘模板不一致时（减少 Mod 后第一次 sync：计划 103s、模板 106s）会误导。
//  3. 一次 sync 收敛：减少 Mod 后模板与配置同时到位，project diff 为空。旧行为先按旧配置值渲染模板、
//     提交后才刷新配置（模板 106s、配置 98s），第二次 sync 才收敛；且配置刷新在提交之外，不受 sync 的
//     回滚与并发输入检查保护（刷新失败时模板已提交）。
//
// 公式与 RR-20260926-66 复核的安全方向不变：宽限期 = max(公式值, 配置里实际生效的 total) + 5s。

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// legacyShutdownBlock is the shutdown: block a generator before RR-20260926-66
// wrote for every service (RR-51: 60s); sync never rewrites that format.
const legacyShutdownBlock = "shutdown:\n" +
	"  # Whole shutdown window: Service.Shutdown, then every Mod in reverse order. Every Mod without\n" +
	"  # a declared stop budget keeps a fixed 3s floor; a Mod that declares one\n" +
	"  # (dataengine.shutdown_timeout) is granted it from the rest, scaled down with a warning only\n" +
	"  # when the rest cannot cover it. Keep it >= dataengine.shutdown_timeout + 3s x the other Mods.\n" +
	"  # The deployment's termination grace period (k8s terminationGracePeriodSeconds, compose\n" +
	"  # stop_grace_period, systemd TimeoutStopSec) must be >= total_timeout + 5s: 65s by default.\n" +
	"  total_timeout: 60s\n  serve_wait_timeout: 5s\n"

// replaceShutdownBlock swaps the shutdown: block of a config (or of the
// config.yaml a k8s secret example embeds, keeping its indentation) for block.
func replaceShutdownBlock(t *testing.T, root, rel, block string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	start := regexp.MustCompile(`(?m)^( *)shutdown:\n`).FindStringSubmatchIndex(body)
	if start == nil {
		t.Fatalf("%s has no shutdown: block", rel)
	}
	indent := body[start[2]:start[3]]
	endMark := indent + "  serve_wait_timeout: 5s\n"
	end := strings.Index(body[start[0]:], endMark)
	if end < 0 {
		t.Fatalf("%s: shutdown: block has no serve_wait_timeout line", rel)
	}
	writeProjectFile(t, root, rel, body[:start[0]]+indentText(block, indent)+body[start[0]+end+len(endMark):])
}

func setShutdownTotal(t *testing.T, root, rel, total string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	edited := regexp.MustCompile(`total_timeout: \d+s\n`).ReplaceAllString(body, "total_timeout: "+total+"\n")
	if edited == body {
		t.Fatalf("%s: total_timeout not replaced by %s", rel, total)
	}
	writeProjectFile(t, root, rel, edited)
}

func shutdownStatus(t *testing.T, root string) map[string]CheckItem {
	t.Helper()
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	items := map[string]CheckItem{}
	for _, item := range checkShutdownBudgets(root, m) {
		items[item.Name] = item
	}
	return items
}

func assertProjectDiffEmpty(t *testing.T, root string) {
	t.Helper()
	var out bytes.Buffer
	if err := DiffProject(root, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "summary: 0 file(s) would change") {
		t.Errorf("project diff after one sync is not empty:\n%s", out.String())
	}
}

func editManifest(t *testing.T, root string, edit func(*Manifest)) {
	t.Helper()
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	edit(&m)
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, ManifestName, string(raw))
}

// newGameDemo 返回一份私有的 game-demo 工程。同参数只真实 NewProject 一次，之后复制（RR-20260928-14，
// 见 project_fixture_test.go；副本与新鲜生成逐字节相同）。
func newGameDemo(t *testing.T) string {
	t.Helper()
	return copyOfNewProject(t, "game-demo")
}

// (a) An old 60s project upgraded with sync, then only config.game.yaml raised
// to the value doctor suggests (REPRO-2026-09-26-07 §3 step 2): the production
// example and the secret example still run 60s, and doctor must say so.
func TestDoctorWarnsForEachRepositoryConfigThatCannotCoverTheModFloors(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	dev := "configs/service/config.game.yaml"
	prod := "configs/service/config.game.prod.example.yaml"
	secret := "deploy/k8s/base/secret.game.example.yaml"
	for _, rel := range []string{dev, prod, secret} {
		replaceShutdownBlock(t, root, rel, legacyShutdownBlock)
	}
	if _, err := SyncProject(root); err != nil {
		t.Fatal(err)
	}
	item := shutdownStatus(t, root)["shutdown:game"]
	for _, rel := range []string{dev, prod, secret} {
		if item.Status != StatusWarn || !strings.Contains(item.Detail, rel+": total_timeout 60s cannot cover 23 Mods (40s declared + 3s x 21 + 3s singleton release = 106s)") {
			t.Errorf("all three configs on 60s: %s %s (want a WARN naming %s)", item.Status, item.Detail, rel)
		}
	}

	setShutdownTotal(t, root, dev, "111s")
	item = shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("only %s raised to 111s, the examples still 60s: %s %s", dev, item.Status, item.Detail)
	}
	for _, rel := range []string{prod, secret} {
		if !strings.Contains(item.Detail, rel+": total_timeout 60s cannot cover 23 Mods") {
			t.Errorf("WARN does not name %s: %s", rel, item.Detail)
		}
	}
	if strings.Contains(item.Detail, dev+":") {
		t.Errorf("WARN names %s, which covers the Mods: %s", dev, item.Detail)
	}

	setShutdownTotal(t, root, prod, "111s")
	item = shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn || !strings.Contains(item.Detail, secret+":") || strings.Contains(item.Detail, prod+":") {
		t.Errorf("only %s left on 60s: %s %s", secret, item.Status, item.Detail)
	}

	setShutdownTotal(t, root, secret, "111s")
	item = shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusOK || !strings.Contains(item.Detail, "total_timeout 111s, grace period 116s") {
		t.Errorf("every config on 111s: %s %s", item.Status, item.Detail)
	}
}

// (b) A service loses a Mod (the game service no longer uses session, as in
// the auditor's aud4-modrm): ONE sync must bring the configs and every
// template to the new plan, leave project diff empty and doctor all OK with
// the grace period the templates on disk actually set.
func TestOneSyncConvergesAfterAServiceLosesAMod(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	editManifest(t, root, func(m *Manifest) {
		spec := m.Services["game"]
		spec.Uses = removeString(spec.Uses, "session")
		m.Services["game"] = spec
	})
	if _, err := SyncProject(root); err != nil {
		t.Fatal(err)
	}
	counts, declares := bootstrapModCounts(t, root)
	total, grace := expectedShutdown(counts["game"], declares["game"])
	if counts["game"] != 22 || total != 108 || grace != 113 {
		t.Fatalf("game after dropping session: %d Mods, %ds / %ds; want 22 Mods, 108s / 113s", counts["game"], total, grace)
	}
	assertGeneratedShutdown(t, root, "game", total, grace)
	assertContains(t, root, "deploy/dev/second-game.sh", `[ "$i" -lt 113 ]`)
	assertProjectDiffEmpty(t, root)
	for name, item := range shutdownStatus(t, root) {
		if item.Status != StatusOK {
			t.Errorf("%s after one sync: %s %s", name, item.Status, item.Detail)
		}
	}
	if item := shutdownStatus(t, root)["shutdown:game"]; !strings.Contains(item.Detail, "total_timeout 108s, grace period 113s") {
		t.Errorf("shutdown:game after one sync: %s", item.Detail)
	}
}

// (c) A service gains a Mod: one sync, the same promises.
func TestOneSyncConvergesAfterAServiceGainsAMod(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	counts, declares := bootstrapModCounts(t, root)
	total, grace := expectedShutdown(counts["game"], declares["game"])
	assertGeneratedShutdown(t, root, "game", total, grace)
	assertProjectDiffEmpty(t, root)
	item := shutdownStatus(t, root)["shutdown:game"]
	if want := "total_timeout " + strconv.Itoa(total) + "s, grace period " + strconv.Itoa(grace) + "s"; item.Status != StatusOK || !strings.Contains(item.Detail, want) {
		t.Errorf("shutdown:game after adding redis: %s %s (want %q)", item.Status, item.Detail, want)
	}
}

// doctor reports what the platform will wait — the templates on disk — not
// what the generator would render now. Templates raised by hand to 120s: the
// OK line says 120s.
func TestDoctorShowsTheGracePeriodTheTemplatesOnDiskSet(t *testing.T) {
	root := copyOfNewProject(t, "configdata")
	graces := deployedGraces(t, root, "game")
	generated := graces["k8s"]
	replace := func(rel, old, new string) {
		body := readProjectFile(t, root, rel)
		if !strings.Contains(body, old) {
			t.Fatalf("%s does not contain %q", rel, old)
		}
		writeProjectFile(t, root, rel, strings.Replace(body, old, new, 1))
	}
	g, raised := strconv.Itoa(generated), "120"
	replace("deploy/k8s/base/game.yaml", "terminationGracePeriodSeconds: "+g+"\n", "terminationGracePeriodSeconds: "+raised+"\n")
	compose := readProjectFile(t, root, "deploy/docker/docker-compose.prod.yaml")
	at := strings.Index(compose, "\n  game:\n")
	writeProjectFile(t, root, "deploy/docker/docker-compose.prod.yaml", compose[:at]+strings.Replace(compose[at:], "stop_grace_period: "+g+"s", "stop_grace_period: "+raised+"s", 1))
	replace("deploy/shell/install.sh", "  game) STOP_TIMEOUT="+g+"s ;;", "  game) STOP_TIMEOUT="+raised+"s ;;")
	run := readProjectFile(t, root, "deploy/dev/run.sh")
	entry := regexp.MustCompile(`([" ]game:\d+:)` + g + `([" ])`)
	if !entry.MatchString(run) {
		t.Fatalf("deploy/dev/run.sh has no game entry with %ss", g)
	}
	writeProjectFile(t, root, "deploy/dev/run.sh", entry.ReplaceAllString(run, "${1}"+raised+"${2}"))
	if second := "deploy/dev/second-game.sh"; statErr(filepath.Join(root, second)) == nil {
		replace(second, `[ "$i" -lt `+g+` ]`, `[ "$i" -lt `+raised+` ]`)
	}
	for template, value := range deployedGraces(t, root, "game") {
		if value != 120 {
			t.Fatalf("setup: %s grace period %ds, want 120s", template, value)
		}
	}
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusOK || !strings.Contains(item.Detail, "grace period 120s") {
		t.Errorf("templates on disk at 120s (generated %ds): %s %s", generated, item.Status, item.Detail)
	}
}

func projectFileHashes(t *testing.T, root string) map[string][sha256.Size]byte {
	t.Helper()
	out := map[string][sha256.Size]byte{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = sha256.Sum256(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertProjectUnchanged(t *testing.T, root string, before map[string][sha256.Size]byte) {
	t.Helper()
	after := projectFileHashes(t, root)
	for _, rel := range diffSnapshot(before, after) {
		t.Errorf("a failed sync left %s changed", rel)
	}
}

// blockWritesIn makes the files already in dir impossible to replace for the
// rest of the test, so an atomic write into dir (writeAtomic: a temporary
// file, then a rename over the target) fails. The files keep their bytes and
// stay readable.
//
// RR-20260927-01：这里原先只有 os.Chmod(dir, 0o555)。Windows 上 os.Chmod 只切换文件的只读属性
// （os.Chmod 文档：Windows 只用 0o200 位），对目录的写入与改名不起作用，CI windows-compatibility
// 因此 "sync succeeded although configs/service is read-only"。Windows 上改为持有目录里每个文件的
// 句柄：Go 在 Windows 打开文件时共享模式只有 FILE_SHARE_READ|FILE_SHARE_WRITE、不带
// FILE_SHARE_DELETE（syscall.Open），而 os.Rename 是 MoveFileEx(MOVEFILE_REPLACE_EXISTING)——
// 替换一个这样被打开的文件、或把它挪去备份，都会被拒绝，writeAtomic 的直接改名与它为 Windows 准备的
// “先挪到备份再改名”两条路都失败；读取（os.ReadFile 同样只要读共享）不受影响。Unix 上打开的句柄
// 不妨碍改名，仍用去掉目录写权限的办法（创建临时文件即失败）。
func blockWritesIn(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		held := 0
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				continue
			}
			file, err := os.Open(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			// Registered after t.TempDir, so it runs before the directory is removed.
			t.Cleanup(func() { _ = file.Close() })
			held++
		}
		if held == 0 {
			t.Fatalf("%s has no file to hold: the injected write failure would not happen", dir)
		}
		return
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// The config refresh is part of the sync's commit: when it cannot be written,
// nothing of the sync is (the templates already rendered for the new plan are
// not left behind with the old configs).
func TestSyncWritesNothingWhenTheShutdownRefreshCannotBeWritten(t *testing.T) {
	root := copyOfNewProject(t, "configdata")
	editManifest(t, root, func(m *Manifest) {
		spec := m.Services["game"]
		spec.Mods = append(spec.Mods, "redis")
		m.Services["game"] = spec
	})
	before := projectFileHashes(t, root)
	blockWritesIn(t, filepath.Join(root, "configs", "service"))
	if _, err := SyncProject(root); err == nil {
		t.Fatal("sync succeeded although no file in configs/service can be written")
	}
	assertProjectUnchanged(t, root, before)
}

// A later write failing rolls the refreshed configs back with the templates.
func TestSyncRollsTheShutdownRefreshBackWithTheTemplates(t *testing.T) {
	root := copyOfNewProject(t, "configdata")
	editManifest(t, root, func(m *Manifest) {
		spec := m.Services["game"]
		spec.Mods = append(spec.Mods, "redis")
		m.Services["game"] = spec
	})
	before := projectFileHashes(t, root)
	// internal/bootstrap/generated.go sorts after configs/ and deploy/: both
	// are written, then rolled back.
	blockWritesIn(t, filepath.Join(root, "internal", "bootstrap"))
	if _, err := SyncProject(root); err == nil {
		t.Fatal("sync succeeded although no file in internal/bootstrap can be written")
	}
	assertProjectUnchanged(t, root, before)
}

// A config edited while sync runs (after the refresh was planned, before the
// commit) stops the sync like any other project input: the developer's edit
// stays, no template is written.
func TestSyncRefusesAConfigEditedWhileItRuns(t *testing.T) {
	root := copyOfNewProject(t, "configdata")
	editManifest(t, root, func(m *Manifest) {
		spec := m.Services["game"]
		spec.Mods = append(spec.Mods, "redis")
		m.Services["game"] = spec
	})
	rel := "configs/service/config.game.yaml"
	edited := readProjectFile(t, root, rel) + "# developer edit\n"
	syncProjectBeforeCommit = func() { writeProjectFile(t, root, rel, edited) }
	t.Cleanup(func() { syncProjectBeforeCommit = nil })
	before := projectFileHashes(t, root)
	_, err := SyncProject(root)
	if err == nil || !strings.Contains(err.Error(), rel) || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("sync with %s edited concurrently: %v", rel, err)
	}
	if got := readProjectFile(t, root, rel); got != edited {
		t.Errorf("the concurrent edit of %s was overwritten:\n%s", rel, got)
	}
	after := projectFileHashes(t, root)
	for _, changed := range diffSnapshot(before, after) {
		if changed != rel {
			t.Errorf("a refused sync wrote %s", changed)
		}
	}
}

func statErr(path string) error {
	_, err := os.Stat(path)
	return err
}

func removeString(values []string, drop string) []string {
	out := values[:0:0]
	for _, value := range values {
		if value != drop {
			out = append(out, value)
		}
	}
	return out
}
