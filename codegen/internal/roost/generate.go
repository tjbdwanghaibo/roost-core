package roost

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/tjbdwanghaibo/roost-codegen/internal/attribute"
	"github.com/tjbdwanghaibo/roost-codegen/internal/dao"
	"github.com/tjbdwanghaibo/roost-codegen/internal/entity"
	codeerr "github.com/tjbdwanghaibo/roost-codegen/internal/errcode"
	"github.com/tjbdwanghaibo/roost-codegen/internal/eventgen"
	"github.com/tjbdwanghaibo/roost-codegen/internal/nest"
	"github.com/tjbdwanghaibo/roost-codegen/internal/protocol"
	"github.com/tjbdwanghaibo/roost-codegen/internal/registry"
	"github.com/tjbdwanghaibo/roost-codegen/internal/servicerpc"
	"github.com/tjbdwanghaibo/roost-codegen/internal/tablegen"
	"github.com/tjbdwanghaibo/roost-codegen/internal/webroute"
)

// os.Chdir is process-wide. Serializing generator execution prevents two
// library callers from parsing or writing relative paths in each other's
// projects. Cross-process conflicts are handled by the commit guards.
var generatorWorkingDirectory sync.Mutex

type GenerateOptions struct {
	Changed bool
	Check   bool
	DryRun  bool
	// Force makes every generator rewrite its outputs even when the content
	// is unchanged. The default relies on content-hash short-circuiting, so
	// an idempotent regeneration leaves file mtimes alone and incremental
	// builds stay warm.
	Force  bool
	Stdout io.Writer
}

type generator struct {
	Feature  string
	Name     string
	Prefixes []string
	// Always runs this generator regardless of the feature set and regardless
	// of which files changed. The registry aggregate is Always because its
	// inputs are //roost:register markers anywhere in the tree — including in
	// code that another generator in this same run just emitted — so neither
	// the feature gate nor a changed-file prefix can decide for it.
	Always bool
	Run    func(io.Writer) error
}

func forceArg(args []string, force bool) []string {
	if force {
		return append(args, "-force")
	}
	return args
}

func generatorsFor(m Manifest, force bool) []generator {
	return []generator{
		{Feature: "dao", Name: "dao", Prefixes: []string{"db/def/"}, Run: func(w io.Writer) error {
			return dao.Run(forceArg([]string{"-def", dao.DefaultDefDir, "-out", dao.DefaultOutDir}, force), w)
		}},
		{Feature: "event", Name: "event", Prefixes: []string{"event/def/"}, Run: func(w io.Writer) error {
			args := forceArg([]string{"-def", eventgen.DefaultDefDir, "-out", eventgen.DefaultOutDir}, force)
			if info, err := os.Stat("./game"); err == nil && info.IsDir() {
				args = append(args, "-game", "./game")
			}
			return eventgen.Run(args, w)
		}},
		{Feature: "errcode", Name: "errcode", Prefixes: []string{"game/", "internal/", "service/"}, Run: func(w io.Writer) error {
			return codeerr.Run([]string{"-root", ".", "-out", codeerr.DefaultOutFile}, w)
		}},
		{Feature: "protocol", Name: "protocol", Prefixes: []string{"protocol/def/"}, Run: func(w io.Writer) error {
			args := []string{"-def", protocol.DefaultDefDir, "-robot-protocol", ""}
			if _, enabled := m.Access["player"]; enabled {
				args = append(args,
					"-bind", protocol.DefaultBindDir,
					"-handlers", protocol.DefaultHandlerDir,
					"-handler-bootstrap", "./game/protocol_bootstrap/protocol_gen.go",
				)
			} else {
				args = append(args, "-bind", "", "-handlers", "", "-handler-bootstrap", "")
			}
			return protocol.Run(forceArg(args, force), w)
		}},
		{Feature: "entity", Name: "entity", Prefixes: []string{"game/entities/", "game/components/"}, Run: func(w io.Writer) error { return entity.Run(forceArg([]string{"-dir", "./game"}, force), w) }},
		{Feature: "nest", Name: "nest", Prefixes: []string{"game/entities/", "game/components/", "game/handler/"}, Run: func(w io.Writer) error { return nest.Run(forceArg([]string{"-dir", "./game"}, force), w) }},
		{Feature: "attribute", Name: "attribute", Prefixes: []string{"game/gameplay/attribute/"}, Run: func(w io.Writer) error {
			return attribute.Run(forceArg([]string{"-dir", "./game/gameplay/attribute"}, force), w)
		}},
		// tablegen keeps its historical unconditional -force: its outputs use
		// exists-refuses-overwrite semantics rather than content hashing, so
		// dropping force would fail every regeneration after a schema change.
		{Feature: "config", Name: "config-template", Prefixes: []string{"configs/schema/"}, Run: func(w io.Writer) error {
			return tablegen.Run([]string{"-meta", tablegen.DefaultMetaDir, "-csv-template", "./configs/table_template", "-force"}, w)
		}},
		{Feature: "config", Name: "config-data", Prefixes: []string{"configs/schema/", "configs/table/"}, Run: func(w io.Writer) error {
			if empty, err := dirHasNoDataFiles("./configs/table"); err != nil {
				return err
			} else if empty {
				return nil
			}
			return tablegen.Run([]string{"-meta", tablegen.DefaultMetaDir, "-csv", "./configs/table", "-json", "./configs/data", "-force"}, w)
		}},
		{Feature: "config", Name: "config-go", Prefixes: []string{"configs/schema/"}, Run: func(w io.Writer) error {
			return tablegen.Run([]string{"-meta", tablegen.DefaultMetaDir, "-out", "./configs/generated", "-force"}, w)
		}},
		{Feature: "webroute", Name: "webroute", Prefixes: []string{"service/"}, Run: func(w io.Writer) error { return webroute.Run(forceArg([]string{"-dir", "./service"}, force), w) }},
		// The project's own cross-process services (roost add rpc): each
		// internal/rpc/<name> package is one servicerpc run — transport and
		// assembly halves from its //roost:rpc interface. servicerpc writes
		// only when the content changed, so -force is not needed.
		{Feature: "rpc", Name: "servicerpc", Prefixes: []string{"internal/rpc/"}, Run: func(w io.Writer) error {
			for _, dir := range projectRPCDirs(".", m) {
				if err := servicerpc.Run([]string{"-dir", "./" + dir}, w); err != nil {
					return fmt.Errorf("%s: %w", dir, err)
				}
			}
			return nil
		}},
		// Last, and unconditional: the aggregate collects //roost:register
		// markers, including the ones the generators above just wrote.
		{Name: "registry", Always: true, Run: func(w io.Writer) error {
			return registry.Run(".", m.Project.Module, w)
		}},
	}
}

func Generate(root string, options GenerateOptions) error {
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		return err
	}
	if options.Check {
		return checkGenerated(root, manifest, options.Stdout)
	}
	selected := generatorsFor(manifest, options.Force)
	if options.Changed {
		changed, err := gitChanged(root)
		if err != nil {
			return err
		}
		selected = filterChanged(selected, changed)
	}
	return runGenerators(root, manifest, selected, options)
}

// GenerateTransactional runs every selected generator and go mod tidy in a
// sibling staging tree. Only generated artifacts and dependency metadata are
// committed, as one rollback-capable batch, after the whole pipeline succeeds.
func GenerateTransactional(root string, options GenerateOptions, stderr io.Writer) error {
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if options.Check || options.DryRun {
		return Generate(root, options)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	manifest, err := LoadManifest(absRoot)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(absRoot), ".roost-generate-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := copyProject(absRoot, stage); err != nil {
		return fmt.Errorf("stage generation: %w", err)
	}
	inputs, err := snapshotProjectInputs(stage, manifest)
	if err != nil {
		return fmt.Errorf("snapshot generation inputs: %w", err)
	}
	var stagedStdout, stagedStderr bytes.Buffer
	selected := generatorsFor(manifest, options.Force)
	if options.Changed {
		changed, changedErr := gitChanged(absRoot)
		if changedErr != nil {
			return changedErr
		}
		selected = filterChanged(selected, changed)
	}
	stagedOptions := options
	stagedOptions.Changed = false
	stagedOptions.Stdout = &stagedStdout
	if err := runGenerators(stage, manifest, selected, stagedOptions); err != nil {
		replayStagedOutput(options.Stdout, stagedStdout.String(), stage, absRoot)
		return err
	}
	if err := TidyProjectDependencies(stage, &stagedStdout, &stagedStderr); err != nil {
		replayStagedOutput(options.Stdout, stagedStdout.String(), stage, absRoot)
		replayStagedOutput(stderr, stagedStderr.String(), stage, absRoot)
		return err
	}
	changes, err := planStagedProjectCommit(absRoot, stage, manifest)
	if err != nil {
		return err
	}
	dependencyChanges, err := planExplicitStagedFiles(absRoot, stage, "go.mod", "go.sum")
	if err != nil {
		return err
	}
	changes, err = mergeSyncChanges(changes, dependencyChanges)
	if err != nil {
		return err
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].rel < changes[j].rel })
	if err := verifyProjectInputs(absRoot, manifest, inputs); err != nil {
		return err
	}
	if err := commitSyncChanges(changes); err != nil {
		return err
	}
	replayStagedOutput(options.Stdout, stagedStdout.String(), stage, absRoot)
	replayStagedOutput(stderr, stagedStderr.String(), stage, absRoot)
	return nil
}

// snapshotProjectInputs records application-owned source and configuration
// that generators may read. Codegen-owned output and dependency metadata are
// excluded because they are independently guarded by syncChange.before.
func snapshotProjectInputs(root string, manifest Manifest) (map[string][sha256.Size]byte, error) {
	plan, err := renderProject(manifest)
	if err != nil {
		return nil, err
	}
	owned := make(map[string]bool, len(plan))
	for rel, file := range plan {
		if file.Owned {
			owned[filepath.ToSlash(rel)] = true
		}
	}
	out := make(map[string][sha256.Size]byte)
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && skippedProjectDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "go.mod" || rel == "go.sum" || owned[rel] {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(raw, []byte("Code generated")) || isGeneratedData(path) {
			return nil
		}
		out[rel] = sha256.Sum256(raw)
		return nil
	})
	return out, err
}

func verifyProjectInputs(root string, manifest Manifest, expected map[string][sha256.Size]byte) error {
	current, err := snapshotProjectInputs(root, manifest)
	if err != nil {
		return fmt.Errorf("verify project inputs: %w", err)
	}
	changed := diffSnapshot(expected, current)
	if len(changed) == 0 {
		return nil
	}
	const maxReported = 5
	reported := changed
	if len(reported) > maxReported {
		reported = reported[:maxReported]
	}
	detail := strings.Join(reported, ", ")
	if len(changed) > len(reported) {
		detail += fmt.Sprintf(" (and %d more)", len(changed)-len(reported))
	}
	return fmt.Errorf("project inputs changed while code generation was running: %s; rerun the command", detail)
}

func replayStagedOutput(writer io.Writer, value, stage, root string) {
	if value == "" {
		return
	}
	value = strings.ReplaceAll(value, stage, root)
	value = strings.ReplaceAll(value, filepath.ToSlash(stage), filepath.ToSlash(root))
	_, _ = io.WriteString(writer, value)
}

func runGenerators(root string, manifest Manifest, generators []generator, options GenerateOptions) (returnErr error) {
	generatorWorkingDirectory.Lock()
	defer generatorWorkingDirectory.Unlock()
	old, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	defer func() {
		if err := os.Chdir(old); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore generator working directory: %w", err))
		}
	}()
	if legacy, err := marker.FindLegacy(root); err != nil {
		return fmt.Errorf("scan for deprecated markers: %w", err)
	} else if len(legacy) > 0 {
		// Accepted this release, removed next: say so once per run, naming
		// the files, rather than failing or staying silent.
		fmt.Fprintf(options.Stdout, "warning: %d file(s) still use the deprecated //cube: marker prefix; rename to //roost: (accepted for now, removed in the next major):\n", len(legacy))
		for _, rel := range legacy {
			fmt.Fprintf(options.Stdout, "  %s\n", rel)
		}
	}
	for _, gen := range generators {
		if !gen.Always && !hasFeature(manifest, gen.Feature) {
			continue
		}
		if options.DryRun {
			fmt.Fprintf(options.Stdout, "would run: %s\n", gen.Name)
			continue
		}
		fmt.Fprintf(options.Stdout, "==> %s\n", gen.Name)
		if err := gen.Run(options.Stdout); err != nil {
			return fmt.Errorf("generator %s: %w", gen.Name, err)
		}
	}
	return nil
}

func checkGenerated(root string, manifest Manifest, stdout io.Writer) error {
	tmp, err := os.MkdirTemp("", "roost-generated-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := copyProject(root, tmp); err != nil {
		return err
	}
	before, err := snapshotGenerated(tmp)
	if err != nil {
		return err
	}
	// The staleness check compares content snapshots, so hash-short-circuited
	// writes and forced writes produce the same verdict; skip force here.
	if err := runGenerators(tmp, manifest, generatorsFor(manifest, false), GenerateOptions{Stdout: io.Discard}); err != nil {
		return err
	}
	after, err := snapshotGenerated(tmp)
	if err != nil {
		return err
	}
	changed := diffSnapshot(before, after)
	if len(changed) > 0 {
		return fmt.Errorf("generated files are stale: %s", strings.Join(changed, ", "))
	}
	fmt.Fprintln(stdout, "generated files are up to date")
	return nil
}

func copyProject(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() {
			if skippedProjectDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return writeAtomic(filepath.Join(dst, rel), raw, 0o644)
	})
}

func skippedProjectDirectory(name string) bool {
	return name == ".git" || name == "bin" || name == "dist" || name == "log" ||
		name == ".testcache" || strings.HasPrefix(name, ".roost-")
}

func snapshotGenerated(root string) (map[string][sha256.Size]byte, error) {
	out := map[string][sha256.Size]byte{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(raw), "Code generated") && !isGeneratedData(path) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// Newlines are not content: a CRLF checkout of an LF-generated file is
		// current, not stale (the generator always writes LF).
		out[filepath.ToSlash(rel)] = sha256.Sum256(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")))
		return nil
	})
	return out, err
}

func isGeneratedData(path string) bool {
	p := filepath.ToSlash(path)
	return strings.Contains(p, "/configs/data/") && strings.HasSuffix(p, ".json") ||
		strings.Contains(p, "/docs/generated/")
}

func diffSnapshot(before, after map[string][sha256.Size]byte) []string {
	seen := map[string]bool{}
	for p := range before {
		seen[p] = true
	}
	for p := range after {
		seen[p] = true
	}
	var out []string
	for p := range seen {
		if before[p] != after[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func gitChanged(root string) ([]string, error) {
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = root
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if idx := strings.LastIndex(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		out = append(out, filepath.ToSlash(path))
	}
	return out, nil
}

func filterChanged(gens []generator, changed []string) []generator {
	var out []generator
	for _, gen := range gens {
		if gen.Always {
			out = append(out, gen)
			continue
		}
		matched := false
		for _, file := range changed {
			for _, prefix := range gen.Prefixes {
				if strings.HasPrefix(file, prefix) {
					matched = true
				}
			}
		}
		if matched {
			out = append(out, gen)
		}
	}
	return out
}

func dirHasNoDataFiles(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return false, nil
		}
	}
	return true, nil
}

var _ = errors.Join
