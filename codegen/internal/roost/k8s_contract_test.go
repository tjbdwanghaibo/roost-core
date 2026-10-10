package roost

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var secretOnlyConfigKeys = map[string]bool{}

// checkSecretMirrorsProductionExample compares one service's k8s Secret
// example against its production example and reports every difference.
func checkSecretMirrorsProductionExample(t *testing.T, root, service string) {
	t.Helper()
	read := func(rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	prodRel := "configs/service/config." + service + ".prod.example.yaml"
	secretRel := "deploy/k8s/base/secret." + service + ".example.yaml"
	prod := configLeaves(t, prodRel, read(prodRel))
	secret := configLeaves(t, secretRel, secretExampleConfig(t, secretRel, read(secretRel)))
	var missing, extra, differ []string
	for _, key := range sortedKeys(prod) {
		value, ok := secret[key]
		switch {
		case !ok:
			missing = append(missing, key)
		case value != prod[key]:
			differ = append(differ, key+": secret="+value+" prod="+prod[key])
		}
	}
	for _, key := range sortedKeys(secret) {
		if _, ok := prod[key]; !ok && !secretOnlyConfigKeys[key] {
			extra = append(extra, key)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s lacks keys %s has: %s", secretRel, prodRel, summarizeConfigKeys(missing))
	}
	if len(extra) > 0 {
		t.Errorf("%s has keys %s does not: %s", secretRel, prodRel, summarizeConfigKeys(extra))
	}
	if len(differ) > 0 {
		t.Errorf("%s and %s disagree: %v", secretRel, prodRel, differ)
	}
}

// summarizeConfigKeys groups dotted keys by their top-level section so a
// missing section reads as one line ("saga (42 keys)") instead of forty.
func summarizeConfigKeys(keys []string) string {
	count := map[string]int{}
	var order []string
	for _, key := range keys {
		section, _, _ := strings.Cut(key, ".")
		if count[section] == 0 {
			order = append(order, section)
		}
		count[section]++
	}
	parts := make([]string, 0, len(order))
	for _, section := range order {
		parts = append(parts, section+" ("+strconv.Itoa(count[section])+" keys)")
	}
	return strings.Join(parts, ", ")
}

// The game-demo gets saga (add saga) and player_access (add transport tcp)
// after its configs are rendered; every service's Secret must still match.
func TestGameDemoKubernetesSecretExamplesMirrorTheProductionExamples(t *testing.T) {
	root := newGameDemo(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range sortedServiceNames(m) {
		checkSecretMirrorsProductionExample(t, root, service)
	}
}

// The same promise on a plain project, one appender at a time: add mod,
// add saga, add access + add transport tcp. A repeat changes nothing.
func TestAddedConfigSectionsReachTheKubernetesSecretExample(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "saga")
	secretRel := filepath.Join(root, "deploy", "k8s", "base", "secret.game.example.yaml")
	steps := []AddOptions{
		{Kind: "mod", Name: "redis", Service: "game"},
		{Kind: "saga", Name: "AllianceRally", Service: "game", Steps: []string{"Reserve", "March"}},
		{Kind: "access", Name: "player", Service: "game"},
		{Kind: "transport", Name: "tcp"},
	}
	for _, step := range steps {
		if _, err := Add(root, step); err != nil {
			t.Fatalf("add %s %s: %v", step.Kind, step.Name, err)
		}
		if _, err := SyncProject(root); err != nil {
			t.Fatalf("sync after add %s: %v", step.Kind, err)
		}
		t.Run("after add "+step.Kind, func(t *testing.T) { checkSecretMirrorsProductionExample(t, root, "game") })
	}
	before, err := os.ReadFile(secretRel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(secretRel); string(after) != string(before) {
		t.Errorf("re-adding a mod rewrote the Secret example:\n%s", after)
	}
}

func newSecretTestProject(t *testing.T) string {
	t.Helper()
	return copyOfNewProject(t, "saga")
}

var secretTestConfigs = []string{
	"configs/service/config.game.yaml",
	"configs/service/config.game.prod.example.yaml",
	"deploy/k8s/base/secret.game.example.yaml",
}

var bareLineFeed = regexp.MustCompile("(^|[^\r])\n")

func TestCRLFKubernetesSecretExampleGetsTheSameEditsAsLF(t *testing.T) {
	t.Parallel()
	lf, crlf := newSecretTestProject(t), newSecretTestProject(t)
	for _, rel := range secretTestConfigs {
		path := filepath.Join(crlf, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	steps := []AddOptions{
		{Kind: "mod", Name: "redis", Service: "game"},
		{Kind: "saga", Name: "AllianceRally", Service: "game", Steps: []string{"Reserve", "March"}},
		{Kind: "access", Name: "player", Service: "game"},
		{Kind: "transport", Name: "tcp"},
	}
	for _, step := range steps {
		for _, root := range []string{lf, crlf} {
			if _, err := Add(root, step); err != nil {
				t.Fatalf("add %s %s: %v", step.Kind, step.Name, err)
			}
			if _, err := SyncProject(root); err != nil {
				t.Fatalf("sync after add %s: %v", step.Kind, err)
			}
		}
		for _, rel := range secretTestConfigs {
			want, _ := os.ReadFile(filepath.Join(lf, filepath.FromSlash(rel)))
			got, _ := os.ReadFile(filepath.Join(crlf, filepath.FromSlash(rel)))
			if normalized := bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")); !bytes.Equal(normalized, want) {
				t.Errorf("after add %s: CRLF %s differs from the LF project's once line endings are ignored:\n%s",
					step.Kind, rel, firstLineDifference(string(want), string(normalized)))
			}
		}
		secret, _ := os.ReadFile(filepath.Join(crlf, filepath.FromSlash(secretTestConfigs[2])))
		if step.Kind == "mod" && !bytes.Contains(secret, []byte("\r\n    redis:\r\n")) {
			t.Errorf("after add mod redis: the CRLF Secret example has no redis: section (the production example has one)")
		}
		if bareLineFeed.Match(secret) {
			t.Errorf("after add %s: the CRLF Secret example now has LF-only lines", step.Kind)
		}
	}
}

// firstLineDifference names the first line two texts differ at.
func firstLineDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return "  line " + strconv.Itoa(i+1) + ": want " + strconv.Quote(wl) + "\n  line " + strconv.Itoa(i+1) + ": got  " + strconv.Quote(gl)
		}
	}
	return "  (same lines)"
}

// runCLI runs the roost CLI and returns its stderr.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := Run(args, &stdout, &stderr)
	return stderr.String(), err
}

func TestUnrecognizedKubernetesSecretExampleIsAVisibleWarningForEveryCommand(t *testing.T) {
	t.Parallel()
	secretRel := secretTestConfigs[2]
	t.Run("add mod, no config.yaml block", func(t *testing.T) {
		root := newSecretTestProject(t)
		path := filepath.Join(root, filepath.FromSlash(secretRel))
		raw, _ := os.ReadFile(path)
		rewritten := strings.Replace(string(raw), "\n  config.yaml: |\n", "\n  app.yaml: |\n", 1)
		if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
			t.Fatal(err)
		}
		stderr, err := runCLI(t, "add", "mod", "redis", "-root", root, "-service", "game")
		if err != nil {
			t.Fatalf("add mod redis failed on a Secret example it cannot recognise: %v", err)
		}
		if !strings.Contains(stderr, "WARN") || !strings.Contains(stderr, secretRel) {
			t.Errorf("add mod redis skipped a Secret example it cannot recognise without a warning naming it; stderr=%q", stderr)
		}
		// The shutdown refresh may still update its own block; the Mod section must not be guessed in.
		if after, _ := os.ReadFile(path); strings.Contains(string(after), "redis:") {
			t.Errorf("add mod redis edited a Secret example it cannot recognise")
		}
	})
	t.Run("add transport tcp, flow-style player_access in the Secret only", func(t *testing.T) {
		root := newSecretTestProject(t)
		if _, err := Add(root, AddOptions{Kind: "access", Name: "player", Service: "game"}); err != nil {
			t.Fatal(err)
		}
		if _, err := SyncProject(root); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, filepath.FromSlash(secretRel))
		raw, _ := os.ReadFile(path)
		broken := strings.Replace(string(raw), "\n    stats_log:\n", "\n    player_access: {tcp: {enabled: true}}\n    stats_log:\n", 1)
		if broken == string(raw) {
			t.Fatal("the Secret example has no stats_log: block to put a flow-style player_access before")
		}
		if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		stderr, err := runCLI(t, "add", "transport", "tcp", "-root", root)
		if err != nil {
			t.Fatalf("add transport tcp failed on a Secret example it cannot merge, although add mod only warns: %v", err)
		}
		if !strings.Contains(stderr, "WARN") || !strings.Contains(stderr, secretRel) {
			t.Errorf("add transport tcp skipped a Secret example it cannot merge without a warning naming it; stderr=%q", stderr)
		}
		if after, _ := os.ReadFile(path); !strings.Contains(string(after), "player_access: {tcp: {enabled: true}}") || strings.Contains(string(after), "max_payload_bytes") {
			t.Errorf("add transport tcp edited the player_access of a Secret example it cannot merge")
		}
		prod, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(secretTestConfigs[1])))
		if !strings.Contains(string(prod), "\n  tcp:\n") {
			t.Errorf("add transport tcp did not configure the production example")
		}
	})
}
