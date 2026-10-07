package roost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/marker"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/attribute"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/dao"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entity"
	codeerr "github.com/tjbdwanghaibo/roost-core/codegen/internal/errcode"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/eventgen"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/nest"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/protocol"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/registry"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/servicerpc"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/tablegen"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/webroute"
)

// generatorRuns serializes generator execution within the process. The
// generators were written as one-shot commands and have not been audited for
// concurrent runs; cross-process conflicts are handled by the commit guards.
//
// It no longer guards a working directory: runGenerators used to os.Chdir the
// whole process into the tree it generated (often a .roost-sync-* or
// .roost-generate-* staging tree), so every child process another goroutine
// started without exec.Cmd.Dir during that window inherited the staging tree
// as its working directory. On Windows a process's working directory cannot be
// deleted while it runs, so the stage outlived the command that made it
// (RR-20261004-12). Generators now receive paths under the root instead.
var generatorRuns sync.Mutex

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
	// Run generates into the project at root, an absolute path. Generators
	// get paths under root, never the process working directory.
	Run func(root string, w io.Writer) error
}

// under names a project-relative path (slash-separated, as the generator
// defaults spell it) inside root.
func under(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

func forceArg(args []string, force bool) []string {
	if force {
		return append(args, "-force")
	}
	return args
}

func generatorsFor(m Manifest, force bool) []generator {
	return []generator{
		{Feature: "dao", Name: "dao", Prefixes: []string{"db/def/"}, Run: func(root string, w io.Writer) error {
			return dao.Run(forceArg([]string{"-def", under(root, dao.DefaultDefDir), "-out", under(root, dao.DefaultOutDir)}, force), w)
		}},
		{Feature: "event", Name: "event", Prefixes: []string{"event/def/"}, Run: func(root string, w io.Writer) error {
			args := forceArg([]string{"-def", under(root, eventgen.DefaultDefDir), "-out", under(root, eventgen.DefaultOutDir)}, force)
			if info, err := os.Stat(under(root, "game")); err == nil && info.IsDir() {
				args = append(args, "-game", under(root, "game"))
			}
			return eventgen.Run(args, w)
		}},
		{Feature: "errcode", Name: "errcode", Prefixes: []string{"game/", "internal/", "service/"}, Run: func(root string, w io.Writer) error {
			return codeerr.Run([]string{"-root", root, "-out", under(root, codeerr.DefaultOutFile)}, w)
		}},
		{Feature: "protocol", Name: "protocol", Prefixes: []string{"protocol/def/"}, Run: func(root string, w io.Writer) error {
			args := []string{
				"-def", under(root, protocol.DefaultDefDir),
				"-proto", under(root, "protocol/proto"),
				"-pb", under(root, "protocol/pb"),
				"-msgid", under(root, "protocol/msgid"),
				"-manifest", under(root, "protocol/protocol_manifest.json"),
				"-robot-protocol", "",
			}
			if _, enabled := m.Access["player"]; enabled {
				args = append(args,
					"-bind", under(root, protocol.DefaultBindDir),
					"-handlers", under(root, protocol.DefaultHandlerDir),
					"-handler-bootstrap", under(root, "game/protocol_bootstrap/protocol_gen.go"),
				)
			} else {
				args = append(args, "-bind", "", "-handlers", "", "-handler-bootstrap", "")
			}
			return protocol.Run(forceArg(args, force), w)
		}},
		{Feature: "entity", Name: "entity", Prefixes: []string{"game/entities/", "game/components/"}, Run: func(root string, w io.Writer) error {
			return entity.Run(forceArg([]string{"-dir", under(root, "game")}, force), w)
		}},
		{Feature: "nest", Name: "nest", Prefixes: []string{"game/entities/", "game/components/", "game/handler/"}, Run: func(root string, w io.Writer) error {
			return nest.Run(forceArg([]string{"-dir", under(root, "game")}, force), w)
		}},
		{Feature: "attribute", Name: "attribute", Prefixes: []string{"game/gameplay/attribute/"}, Run: func(root string, w io.Writer) error {
			return attribute.Run(forceArg([]string{"-dir", under(root, "game/gameplay/attribute")}, force), w)
		}},
		// tablegen keeps its historical unconditional -force: its outputs use
		// exists-refuses-overwrite semantics rather than content hashing, so
		// dropping force would fail every regeneration after a schema change.
		{Feature: "config", Name: "config-template", Prefixes: []string{"configs/schema/"}, Run: func(root string, w io.Writer) error {
			return tablegen.Run([]string{"-meta", under(root, tablegen.DefaultMetaDir), "-csv-template", under(root, "configs/table_template"), "-force"}, w)
		}},
		{Feature: "config", Name: "config-data", Prefixes: []string{"configs/schema/", "configs/table/"}, Run: func(root string, w io.Writer) error {
			// A schema without any CSV yet is the normal state between
			// `roost project new` and the first planner table, and tablegen
			// reads one CSV per meta unconditionally, so config-data is
			// skipped until a CSV exists (v1.17.2 behavior). The manifest
			// alone cannot be the trigger: the scaffold always writes an
			// empty one, which made that skip dead and failed every such
			// project (RR-20261001-03). Only a manifest that still owns
			// JSON files has retirement work to do (RR-20260930-09).
			if empty, err := dirHasNoDataFiles(under(root, "configs/table")); err != nil {
				return err
			} else if empty {
				if owns, err := tablegen.ManifestOwnsJSON(under(root, "configs/data")); err != nil {
					return err
				} else if !owns {
					return nil
				}
			}
			return tablegen.Run([]string{"-meta", under(root, tablegen.DefaultMetaDir), "-csv", under(root, "configs/table"), "-json", under(root, "configs/data"), "-force"}, w)
		}},
		{Feature: "config", Name: "config-go", Prefixes: []string{"configs/schema/"}, Run: func(root string, w io.Writer) error {
			return tablegen.Run([]string{"-meta", under(root, tablegen.DefaultMetaDir), "-out", under(root, "configs/generated"), "-force"}, w)
		}},
		{Feature: "webroute", Name: "webroute", Prefixes: []string{"service/"}, Run: func(root string, w io.Writer) error {
			return webroute.Run(forceArg([]string{"-dir", under(root, "service")}, force), w)
		}},
		// The project's own cross-process services (roost add rpc): each
		// internal/rpc/<name> package is one servicerpc run — transport and
		// assembly halves from its //roost:rpc interface. servicerpc writes
		// only when the content changed, so -force is not needed.
		{Feature: "rpc", Name: "servicerpc", Prefixes: []string{"internal/rpc/"}, Run: func(root string, w io.Writer) error {
			for _, dir := range projectRPCDirs(root, m) {
				// "./" + dir, relative to root: servicerpc writes the flag
				// into the generated header as the command to rerun.
				if err := servicerpc.RunIn(root, []string{"-dir", "./" + dir}, w); err != nil {
					return fmt.Errorf("%s: %w", dir, err)
				}
			}
			return nil
		}},
		// Last, and unconditional: the aggregate collects //roost:register
		// markers, including the ones the generators above just wrote.
		{Name: "registry", Always: true, Run: func(root string, w io.Writer) error {
			return registry.Run(root, m.Project.Module, w)
		}},
	}
}

// Generate runs the generators in place (or, with Check, in a throwaway copy).
func Generate(root string, options GenerateOptions) error {
	return generate(context.Background(), root, options)
}

func generate(ctx context.Context, root string, options GenerateOptions) error {
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		return err
	}
	if options.Check {
		return checkGenerated(ctx, root, manifest, options.Stdout)
	}
	selected := generatorsFor(manifest, options.Force)
	if options.Changed {
		changed, err := gitChanged(root)
		if err != nil {
			return err
		}
		selected = filterChanged(selected, changed)
	}
	return runGenerators(ctx, root, manifest, selected, options)
}

// GenerateTransactional runs every selected generator and go mod tidy in a
// sibling staging tree. Only generated artifacts and dependency metadata are
// committed, as one rollback-capable batch, after the whole pipeline succeeds.
// When ctx ends before the commit, nothing is committed and the staging tree is
// removed; a commit that started is finished.
func GenerateTransactional(ctx context.Context, root string, options GenerateOptions, stderr io.Writer) error {
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if options.Check || options.DryRun {
		return generate(ctx, root, options)
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
	// Every exit removes the stage, an interrupted one included: the CLI entry
	// turns the signal into ctx and dies of it only after this returns (B6).
	defer os.RemoveAll(stage)
	if err := copyProject(ctx, absRoot, stage); err != nil {
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
	if err := runGenerators(ctx, stage, manifest, selected, stagedOptions); err != nil {
		replayStagedOutput(options.Stdout, stagedStdout.String(), stage, absRoot)
		return inProjectPaths(err, stage, absRoot)
	}
	if err := TidyProjectDependencies(ctx, stage, &stagedStdout, &stagedStderr); err != nil {
		replayStagedOutput(options.Stdout, stagedStdout.String(), stage, absRoot)
		replayStagedOutput(stderr, stagedStderr.String(), stage, absRoot)
		return inProjectPaths(err, stage, absRoot)
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
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("generated files not committed: %w", err)
	}
	if err := verifyProjectInputs(absRoot, manifest, inputs); err != nil {
		return err
	}
	// The commit is not cut short by ctx: once it starts it finishes (or rolls
	// itself back on its own failure), so an interrupt leaves either nothing
	// or the whole batch committed.
	reachStagePhase(ctx, "commit")
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
			if path != root {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					return relErr
				}
				if skippedProjectDirectory(rel) {
					return filepath.SkipDir
				}
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
		if hasGeneratedHeader(path, raw) || isGeneratedData(path) || protocol.IsGeneratedArtifact(rel, raw) {
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

// inProjectPaths reports err with the staging tree's paths replaced by the
// project's (N08 O3): a generator or go command fails on a file it read in the
// stage, and by the time the message is read the stage no longer exists. It
// keeps err for errors.Is / As.
func inProjectPaths(err error, stage, root string) error {
	if err == nil {
		return nil
	}
	return &stagePathError{err: err, stage: stage, root: root}
}

type stagePathError struct {
	err         error
	stage, root string
}

func (e *stagePathError) Error() string {
	message := strings.ReplaceAll(e.err.Error(), e.stage, e.root)
	return strings.ReplaceAll(message, filepath.ToSlash(e.stage), filepath.ToSlash(e.root))
}

func (e *stagePathError) Unwrap() error { return e.err }

func replayStagedOutput(writer io.Writer, value, stage, root string) {
	if value == "" {
		return
	}
	value = strings.ReplaceAll(value, stage, root)
	value = strings.ReplaceAll(value, filepath.ToSlash(stage), filepath.ToSlash(root))
	_, _ = io.WriteString(writer, value)
}

// runGenerators runs the generators in order and stops before the next one
// once ctx ends; a generator that started runs to completion (they are short
// and in-process).
func runGenerators(ctx context.Context, root string, manifest Manifest, generators []generator, options GenerateOptions) error {
	generatorRuns.Lock()
	defer generatorRuns.Unlock()
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
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
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("generator %s not started: %w", gen.Name, err)
		}
		fmt.Fprintf(options.Stdout, "==> %s\n", gen.Name)
		reachStagePhase(ctx, "generator")
		if err := gen.Run(root, options.Stdout); err != nil {
			return fmt.Errorf("generator %s: %w", gen.Name, err)
		}
	}
	return nil
}

func checkGenerated(ctx context.Context, root string, manifest Manifest, stdout io.Writer) error {
	tmp, err := os.MkdirTemp("", "roost-generated-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := copyProject(ctx, root, tmp); err != nil {
		return err
	}
	before, err := snapshotGenerated(tmp)
	if err != nil {
		return err
	}
	// The staleness check compares content snapshots, so hash-short-circuited
	// writes and forced writes produce the same verdict; skip force here.
	if err := runGenerators(ctx, tmp, manifest, generatorsFor(manifest, false), GenerateOptions{Stdout: io.Discard}); err != nil {
		return inProjectPaths(err, tmp, root)
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

// copyProject copies the project's inputs into dst, stopping when ctx ends.
func copyProject(ctx context.Context, src, dst string) error {
	copied := false
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("copy project: %w", err)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() {
			if skippedProjectDirectory(rel) {
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
		if err := writeAtomic(filepath.Join(dst, rel), raw, 0o644); err != nil {
			return err
		}
		if !copied {
			copied = true
			reachStagePhase(ctx, "copy")
		}
		return nil
	})
}

// skippedProjectDirectory reports whether the directory at rel (slash
// separated, relative to the project root) is outside what generation reads
// and commits: VCS metadata, build output, roost's own staging trees, and
// what the running project writes into its own tree. copyProject,
// snapshotProjectInputs and planStagedProjectCommit's walk of the real project
// all use it, so the three agree on what the project is.
//
// RR-20261005-NC-74: `make dev-run` logs to .dev/ and DataEngine's default WAL
// directory is data/wal/dataengine (the generated config.<svc>.yaml); while the
// project ran, every generate / sync failed with "project inputs changed".
// data/wal is matched by path: configs/data is generator output and must not
// be skipped by name.
func skippedProjectDirectory(rel string) bool {
	rel = filepath.ToSlash(rel)
	name := path.Base(rel)
	return name == ".git" || name == "bin" || name == "dist" || name == "log" ||
		name == ".testcache" || strings.HasPrefix(name, ".roost-") ||
		rel == ".dev" || rel == "data/wal"
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !hasGeneratedHeader(path, raw) && !isGeneratedData(path) && !protocol.IsGeneratedArtifact(rel, raw) {
			return nil
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
	// RR-20261006-58: porcelain paths are relative to the REPOSITORY root and
	// quoted when they hold a space, while generator prefixes are relative to
	// the PROJECT root. A project in a subdirectory therefore matched nothing
	// and `generate --changed` ran only the registry while reporting success.
	// -z gives raw, unquoted paths; --show-prefix is the project's place in
	// the repository; "-- ." keeps changes outside the project out.
	prefixCmd := exec.Command("git", "rev-parse", "--show-prefix")
	prefixCmd.Dir = root
	rawPrefix, err := prefixCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-parse --show-prefix: %w", err)
	}
	prefix := strings.TrimSpace(string(rawPrefix))

	cmd := exec.Command("git", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".")
	cmd.Dir = root
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	var out []string
	add := func(repoPath string) {
		if path, inProject := strings.CutPrefix(repoPath, prefix); inProject {
			out = append(out, path)
		}
	}
	// Each entry is "XY <path>"; a rename or copy is followed by one more
	// field, its source path. Both sides count: moving a definition out of a
	// generator's input is a change that generator must see.
	fields := strings.Split(string(raw), "\x00")
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		add(entry[3:])
		if entry[0] == 'R' || entry[0] == 'C' {
			i++
			if i < len(fields) {
				add(fields[i])
			}
		}
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
