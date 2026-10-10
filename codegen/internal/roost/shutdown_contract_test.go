package roost

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
	"time"
)

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
	totalLine := strings.Index(body[start[0]:], indent+"  total_timeout: ")
	if totalLine < 0 {
		t.Fatalf("%s: shutdown: block has no total_timeout line", rel)
	}
	end := totalLine + strings.IndexByte(body[start[0]+totalLine:], '\n') + 1
	writeProjectFile(t, root, rel, body[:start[0]]+indentText(block, indent)+body[start[0]+end:])
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
		if item.Status != StatusWarn || !strings.Contains(item.Detail, rel+": total_timeout 60s cannot cover 24 Mods (40s declared + 3s x 22 + 3s singleton release = 109s)") {
			t.Errorf("all three configs on 60s: %s %s (want a WARN naming %s)", item.Status, item.Detail, rel)
		}
	}

	setShutdownTotal(t, root, dev, "114s")
	item = shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("only %s raised to 114s, the examples still 60s: %s %s", dev, item.Status, item.Detail)
	}
	for _, rel := range []string{prod, secret} {
		if !strings.Contains(item.Detail, rel+": total_timeout 60s cannot cover 24 Mods") {
			t.Errorf("WARN does not name %s: %s", rel, item.Detail)
		}
	}
	if strings.Contains(item.Detail, dev+":") {
		t.Errorf("WARN names %s, which covers the Mods: %s", dev, item.Detail)
	}

	setShutdownTotal(t, root, prod, "114s")
	item = shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn || !strings.Contains(item.Detail, secret+":") || strings.Contains(item.Detail, prod+":") {
		t.Errorf("only %s left on 60s: %s %s", secret, item.Status, item.Detail)
	}

	setShutdownTotal(t, root, secret, "114s")
	item = shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusOK || !strings.Contains(item.Detail, "total_timeout 114s, grace period 119s") {
		t.Errorf("every config on 114s: %s %s", item.Status, item.Detail)
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
	if counts["game"] != 23 || total != 111 || grace != 116 {
		t.Fatalf("game after dropping session: %d Mods, %ds / %ds; want 23 Mods, 111s / 116s", counts["game"], total, grace)
	}
	assertGeneratedShutdown(t, root, "game", total, grace)
	assertContains(t, root, "deploy/dev/second-game.sh", `[ "$i" -lt 116 ]`)
	assertProjectDiffEmpty(t, root)
	for name, item := range shutdownStatus(t, root) {
		if item.Status != StatusOK {
			t.Errorf("%s after one sync: %s %s", name, item.Status, item.Detail)
		}
	}
	if item := shutdownStatus(t, root)["shutdown:game"]; !strings.Contains(item.Detail, "total_timeout 111s, grace period 116s") {
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

func gamePlan(t *testing.T, root string) serviceShutdown {
	t.Helper()
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	return serviceShutdownPlan(m, "game")
}

func syncProject(t *testing.T, root string) {
	t.Helper()
	if _, err := SyncProject(root); err != nil {
		t.Fatal(err)
	}
}

var gameConfigs = []string{
	"configs/service/config.game.yaml",
	"configs/service/config.game.prod.example.yaml",
	"deploy/k8s/base/secret.game.example.yaml",
}

var accountConfigs = []string{
	"configs/service/config.account.yaml",
	"configs/service/config.account.prod.example.yaml",
	"deploy/k8s/base/secret.account.example.yaml",
}

// A framework service needs 3s x 6 = 18s; a total_timeout of 0s or below runs
// on the App's 30s, which covers it.
func TestDoctorJudgesANonPositiveTotalAsTheAppsFallback(t *testing.T) {
	t.Parallel()
	for _, total := range []string{"0s", "-5s"} {
		t.Run(total, func(t *testing.T) {
			root := newGameDemo(t)
			for _, rel := range accountConfigs {
				setShutdownTotal(t, root, rel, total)
			}
			// The templates follow the App's 30s + 5s (RR-66 复核); without the
			// sync the FAIL for the old 28s grace period would come first.
			syncProject(t, root)
			item := shutdownStatus(t, root)["shutdown:account"]
			if item.Status != StatusOK {
				t.Fatalf("account with total_timeout %s (the App uses 30s, the Mods need 18s): %s %s, want OK", total, item.Status, item.Detail)
			}
			if !strings.Contains(item.Detail, "the App uses 30s") {
				t.Errorf("OK line does not say the App's fallback is what runs: %s", item.Detail)
			}
		})
	}
}

// The game service needs more than 30s: the WARN says so from the App's 30s,
// not from 0s.
func TestDoctorReportsTheAppsFallbackWhenItCannotCoverTheMods(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	dev := gameConfigs[0]
	setShutdownTotal(t, root, dev, "0s")
	syncProject(t, root)
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("game with total_timeout 0s: %s %s, want a WARN", item.Status, item.Detail)
	}
	if want := dev + ": total_timeout 0s (the App uses 30s) cannot cover"; !strings.Contains(item.Detail, want) {
		t.Errorf("WARN does not judge the App's 30s (%q): %s", want, item.Detail)
	}
}

// dataengine.shutdown_timeout 0s runs on kit/dataengine's 30s: a total that
// covers "0s declared" but not the real 30s is a WARN.
func TestDoctorJudgesANonPositiveDataEngineBudgetAsItsFallback(t *testing.T) {
	root := newGameDemo(t)
	dev := gameConfigs[0]
	plan := gamePlan(t, root)
	short := plan.total - generatedShutdownMargin - time.Second // one second short with dataengine at 30s
	setShutdownTotal(t, root, dev, seconds(short))
	setDataEngineShutdownTimeout(t, root, dev, "0s")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn || !strings.Contains(item.Detail, dev+": total_timeout "+seconds(short)+" cannot cover") {
		t.Fatalf("game with total_timeout %s and dataengine.shutdown_timeout 0s (kit/dataengine uses 30s): %s %s, want a WARN naming %s",
			seconds(short), item.Status, item.Detail, dev)
	}
}

// The advice is computed from the dataengine budget the configs set, and
// following it makes the WARN go away.
func TestDoctorAdviceFollowsTheConfiguredDataEngineBudget(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	for _, rel := range gameConfigs {
		setDataEngineShutdownTimeout(t, root, rel, "60s")
	}
	plan := gamePlan(t, root)
	want := plan.total + 30*time.Second // the formula's total with dataengine at 60s instead of 30s
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("dataengine.shutdown_timeout 60s with total_timeout %s: %s %s, want a WARN", seconds(plan.total), item.Status, item.Detail)
	}
	if advice := "Set it to " + seconds(want); !strings.Contains(item.Detail, advice) {
		t.Fatalf("WARN advice is not %q (60s dataengine budget): %s", advice, item.Detail)
	}
	for _, rel := range gameConfigs {
		setShutdownTotal(t, root, rel, seconds(want))
	}
	syncProject(t, root)
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusOK {
		t.Errorf("after following the advice (%s) and syncing: %s %s, want OK", seconds(want), item.Status, item.Detail)
	}
}

// Configs that set different dataengine budgets get the advice each needs.
func TestDoctorAdvicePerFileWhenTheConfigsDiffer(t *testing.T) {
	root := newGameDemo(t)
	prod, secret := gameConfigs[1], gameConfigs[2]
	plan := gamePlan(t, root)
	for _, rel := range []string{prod, secret} {
		setShutdownTotal(t, root, rel, "60s")
	}
	setDataEngineShutdownTimeout(t, root, secret, "45s")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("%s %s, want a WARN", item.Status, item.Detail)
	}
	want := "Set it to " + seconds(plan.total) + " in " + prod + ", " + seconds(plan.total+15*time.Second) + " in " + secret
	if !strings.Contains(item.Detail, want) {
		t.Errorf("WARN advice is not %q: %s", want, item.Detail)
	}
}

const staleShutdownAdvice = "every stop warns and budgets are cut"

// setDataEngineShutdownTimeout rewrites the dataengine: block's
// shutdown_timeout (other blocks, such as a gateway's, have their own).
func setDataEngineShutdownTimeout(t *testing.T, root, rel, value string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	block := strings.Index(body, "dataengine:\n")
	if block < 0 {
		t.Fatalf("%s has no dataengine: block", rel)
	}
	key := strings.Index(body[block:], "shutdown_timeout: ")
	if key < 0 {
		t.Fatalf("%s: dataengine: block has no shutdown_timeout", rel)
	}
	at := block + key + len("shutdown_timeout: ")
	end := strings.IndexByte(body[at:], '\n')
	writeProjectFile(t, root, rel, body[:at]+value+body[at+end:])
}

func TestDoctorNamesTheFileAndKeyOfAnExampleItCannotParse(t *testing.T) {
	t.Parallel()
	dev := "configs/service/config.game.yaml"
	prod := "configs/service/config.game.prod.example.yaml"
	secret := "deploy/k8s/base/secret.game.example.yaml"

	for _, tc := range []struct {
		name    string
		rel     string
		corrupt func(t *testing.T, root string)
		want    []string // every fragment the WARN must carry
	}{
		{
			name:    "prod example total_timeout is not a duration",
			rel:     prod,
			corrupt: func(t *testing.T, root string) { setShutdownTotal(t, root, prod, "soon") },
			want:    []string{prod + ": shutdown.total_timeout", `"soon"`},
		},
		{
			name:    "secret example dataengine.shutdown_timeout is not a duration",
			rel:     secret,
			corrupt: func(t *testing.T, root string) { setDataEngineShutdownTimeout(t, root, secret, "30 seconds") },
			want:    []string{secret + ": dataengine.shutdown_timeout", `"30 seconds"`},
		},
		{
			name: "prod example does not parse",
			rel:  prod,
			corrupt: func(t *testing.T, root string) {
				writeProjectFile(t, root, prod, readProjectFile(t, root, prod)+"shutdown: [\n")
			},
			want: []string{prod + ": does not parse: yaml: "},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newGameDemo(t)
			if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusOK {
				t.Fatalf("setup: a fresh project is %s %s", item.Status, item.Detail)
			}
			tc.corrupt(t, root)
			item := shutdownStatus(t, root)["shutdown:game"]
			if item.Status != StatusWarn {
				t.Fatalf("%s: %s %s, want a WARN", tc.name, item.Status, item.Detail)
			}
			for _, fragment := range tc.want {
				if !strings.Contains(item.Detail, fragment) {
					t.Errorf("%s: WARN does not say %q: %s", tc.name, fragment, item.Detail)
				}
			}
			if strings.Contains(item.Detail, staleShutdownAdvice) || strings.Contains(item.Detail, "Set it to") {
				t.Errorf("%s: a parse failure is reported as a total that cannot cover the Mods: %s", tc.name, item.Detail)
			}
			for _, other := range []string{dev, prod, secret} {
				if other != tc.rel && strings.Contains(item.Detail, other+":") {
					t.Errorf("%s: WARN names %s, which parses and covers the Mods: %s", tc.name, other, item.Detail)
				}
			}
		})
	}
}

// A real shortfall next to a parse failure keeps its advice, and the advice
// follows the shortfall rather than the file the doctor could not read.
func TestDoctorKeepsTheAdviceForARealShortfallNextToAParseFailure(t *testing.T) {
	root := newGameDemo(t)
	prod := "configs/service/config.game.prod.example.yaml"
	secret := "deploy/k8s/base/secret.game.example.yaml"
	setShutdownTotal(t, root, prod, "soon")
	setShutdownTotal(t, root, secret, "60s")

	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("%s %s, want a WARN", item.Status, item.Detail)
	}
	shortfall := secret + ": total_timeout 60s cannot cover 24 Mods (40s declared + 3s x 22 + 3s singleton release = 109s); " + staleShutdownAdvice + ". Set it to 114s"
	if !strings.Contains(item.Detail, shortfall) {
		t.Errorf("WARN does not carry the shortfall with its advice %q: %s", shortfall, item.Detail)
	}
	if !strings.Contains(item.Detail, prod+": shutdown.total_timeout") {
		t.Errorf("WARN does not name the file and key it cannot parse: %s", item.Detail)
	}
	if strings.Contains(item.Detail, prod+": total_timeout") {
		t.Errorf("WARN judges the unparsable %s as a shortfall: %s", prod, item.Detail)
	}
}

// The dev config is what deploy/dev runs directly; an invalid duration there
// stays a FAIL (unchanged by this fix).
func TestDoctorStillFailsOnAnInvalidDurationInTheDevConfig(t *testing.T) {
	root := newGameDemo(t)
	dev := "configs/service/config.game.yaml"
	setShutdownTotal(t, root, dev, "soon")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusFail || !strings.Contains(item.Detail, dev+": shutdown.total_timeout") {
		t.Errorf("dev config with an invalid total_timeout: %s %s, want a FAIL naming the file and key", item.Status, item.Detail)
	}
}

func setPlayerTCPShutdownTimeout(t *testing.T, root, rel, value string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	block := strings.Index(body, "player_access:\n")
	if block < 0 {
		t.Fatalf("%s has no player_access: block", rel)
	}
	key := strings.Index(body[block:], "shutdown_timeout: ")
	if key < 0 {
		t.Fatalf("%s: player_access: block has no shutdown_timeout", rel)
	}
	at := block + key + len("shutdown_timeout: ")
	end := strings.IndexByte(body[at:], '\n')
	writeProjectFile(t, root, rel, body[:at]+value+body[at+end:])
}

// The generated game service's window counts the player TCP Mod's 10s.
func TestGeneratedShutdownCountsThePlayerTCPStopBudget(t *testing.T) {
	root := newGameDemo(t)
	plan := gamePlan(t, root)
	if plan.playerTCP != 1 || plan.dataEngine != 1 || plan.declared != generatedDataEngineShutdownTimeout+generatedPlayerTCPShutdownTimeout {
		t.Fatalf("game plan declares %+v, want dataengine 30s + player tcp 10s", plan)
	}
	for _, rel := range gameConfigs {
		body := readProjectFile(t, root, rel)
		wants := []string{"40s declared + 3s x 22\n", "3s for the singleton release = 114s", "total_timeout: 114s\n"}
		if rel != gameConfigs[2] { // the k8s secret example carries no player_access block: the Mod's default 10s
			wants = append(wants, "shutdown_timeout: 10s\n")
		}
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not contain %q", rel, want)
			}
		}
	}
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusOK || !strings.Contains(item.Detail, "total_timeout 114s, grace period 119s") {
		t.Errorf("fresh game-demo: %s %s", item.Status, item.Detail)
	}
}

// playerTCPConfigs are the game configs that carry a player_access block (the
// k8s secret example does not: the Mod runs on its default there).
var playerTCPConfigs = gameConfigs[:2]

// doctor reads the listener's budget from the config: raised to 30s, the
// generated total no longer covers it, and the advice counts the 30s.
func TestDoctorCountsTheConfiguredPlayerTCPStopBudget(t *testing.T) {
	root := newGameDemo(t)
	plan := gamePlan(t, root)
	for _, rel := range playerTCPConfigs {
		setPlayerTCPShutdownTimeout(t, root, rel, "30s")
	}
	item := shutdownStatus(t, root)["shutdown:game"]
	want := plan.total + 20*time.Second
	if item.Status != StatusWarn || !strings.Contains(item.Detail, "(60s declared + 3s x 22 + 3s singleton release = ") || !strings.Contains(item.Detail, "Set it to "+seconds(want)) {
		t.Fatalf("player_access.tcp.shutdown_timeout 30s with total_timeout %s: %s %s, want a WARN with 60s declared and advice %s",
			seconds(plan.total), item.Status, item.Detail, seconds(want))
	}
	// 显式 0s 与缺失不同：运行时拒绝，doctor 必须同样拒绝。
	root = newGameDemo(t)
	for _, rel := range playerTCPConfigs {
		setPlayerTCPShutdownTimeout(t, root, rel, "0s")
	}
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusFail {
		t.Errorf("player_access.tcp.shutdown_timeout 0s: %s %s, want FAIL", item.Status, item.Detail)
	}
}

// A negative value stops the Mod from starting: a FAIL in the dev config,
// a WARN naming the file and key in an example (like any value the doctor
// cannot use), never a budget.
func TestDoctorRejectsANegativePlayerTCPShutdownTimeout(t *testing.T) {
	root := newGameDemo(t)
	dev, prod := gameConfigs[0], gameConfigs[1]
	setPlayerTCPShutdownTimeout(t, root, prod, "-1s")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn || !strings.Contains(item.Detail, prod+": player_access.tcp.shutdown_timeout: -1s is negative") || strings.Contains(item.Detail, "Set it to") {
		t.Errorf("prod example with a negative player tcp shutdown_timeout: %s %s", item.Status, item.Detail)
	}
	setPlayerTCPShutdownTimeout(t, root, dev, "-1s")
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusFail || !strings.Contains(item.Detail, dev+": player_access.tcp.shutdown_timeout") {
		t.Errorf("dev config with a negative player tcp shutdown_timeout: %s %s, want a FAIL", item.Status, item.Detail)
	}
}

// A project generated before the Mod declared its budget carries unedited
// shutdown: blocks written for "30s declared + 3s x 22 = 101s". One sync
// recognises them (their summary still matches what that generator wrote)
// and moves them, and every template, to the new plan.
func TestSyncMovesAnUneditedBlockWrittenBeforeThePlayerTCPBudget(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	plan := gamePlan(t, root)
	before := plan
	before.declaring, before.dataEngine, before.playerTCP = 1, 1, 0
	before.declared = generatedDataEngineShutdownTimeout
	before.total = before.declared + time.Duration(before.undeclared())*generatedModStopFloor + before.margin
	for _, rel := range gameConfigs {
		replaceShutdownBlock(t, root, rel, renderShutdownConfig(before))
		if body := readProjectFile(t, root, rel); !strings.Contains(body, "total_timeout: "+seconds(before.total)+"\n") {
			t.Fatalf("setup: %s does not carry the old %s block", rel, seconds(before.total))
		}
	}
	syncProject(t, root)
	for _, rel := range gameConfigs {
		if body := readProjectFile(t, root, rel); !strings.Contains(body, "total_timeout: "+seconds(plan.total)+"\n") {
			t.Errorf("%s was not moved from %s to %s", rel, seconds(before.total), seconds(plan.total))
		}
	}
	assertDeployedGrace(t, root, "game", int((plan.total+generatedGraceOverTotal)/time.Second))
	assertProjectDiffEmpty(t, root)
}
