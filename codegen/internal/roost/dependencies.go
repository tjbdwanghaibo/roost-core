package roost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const dependencyUpdateTimeout = 5 * time.Minute

type dependencyCommandRunner func(context.Context, string, io.Writer, io.Writer, ...string) error

type dependencyFileSnapshot struct {
	path   string
	body   []byte
	mode   os.FileMode
	exists bool
}

// UpdateFrameworkDependencies stages legacy import consolidation and resolves
// the single Core module, then commits the planned migration and module files.
// When ctx ends before the commit, nothing is committed and the staging tree is
// removed; a commit that started is finished.
func UpdateFrameworkDependencies(ctx context.Context, root string, manifest Manifest, stdout, stderr io.Writer) error {
	return updateFrameworkDependenciesTransactional(ctx, root, manifest, stdout, stderr, runDependencyCommand)
}

func updateFrameworkDependenciesTransactional(ctx context.Context, root string, manifest Manifest, stdout, stderr io.Writer, run dependencyCommandRunner) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(absRoot), ".roost-deps-*")
	if err != nil {
		return err
	}
	// Every exit removes the stage, an interrupted one included: the CLI entry
	// turns the signal into ctx and dies of it only after this returns (B6).
	defer os.RemoveAll(stage)
	if err := copyProject(ctx, absRoot, stage); err != nil {
		return fmt.Errorf("stage framework dependencies: %w", err)
	}
	inputs, err := snapshotProjectInputs(stage, manifest)
	if err != nil {
		return fmt.Errorf("snapshot dependency inputs: %w", err)
	}
	var migrationChanges []syncChange
	if needsConsolidation(stage) {
		fmt.Fprintln(stdout, "framework dependencies: staging import consolidation before resolving modules")
		result, err := ConsolidateProject(stage, false, stdout)
		if err != nil {
			return inProjectPaths(fmt.Errorf("consolidate project before resolving dependencies: %w", err), stage, absRoot)
		}
		rels := result.Files
		if result.Manifest {
			rels = append(rels, ManifestName)
		}
		// RR-20260930-CG-14: freeze only the migration's own output before the
		// resolver runs. Arbitrary stage edits by go commands stay isolated.
		migrationChanges, err = planExplicitStagedFiles(absRoot, stage, rels...)
		if err != nil {
			return err
		}
	}
	if err := updateFrameworkDependencies(ctx, stage, manifest, stdout, stderr, run); err != nil {
		return inProjectPaths(err, stage, absRoot)
	}
	changes, err := planExplicitStagedFiles(absRoot, stage, "go.mod", "go.sum")
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("framework dependencies not committed: %w", err)
	}
	if err := verifyProjectInputs(absRoot, manifest, inputs); err != nil {
		return err
	}
	reachStagePhase(ctx, "commit")
	return commitSyncChanges(append(migrationChanges, changes...))
}

// TidyProjectDependencies records module checksums introduced by newly
// generated imports without upgrading framework versions. This keeps the
// beginner workflow buildable immediately after `roost generate`.
func TidyProjectDependencies(ctx context.Context, root string, stdout, stderr io.Writer) error {
	return tidyProjectDependencies(ctx, root, stdout, stderr, runDependencyCommand)
}

func tidyProjectDependencies(ctx context.Context, root string, stdout, stderr io.Writer, run dependencyCommandRunner) error {
	if run == nil {
		return errors.New("dependency command runner is nil")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	goMod := filepath.Join(absRoot, "go.mod")
	if _, err := os.Stat(goMod); err != nil {
		return fmt.Errorf("tidy project dependencies requires go.mod: %w", err)
	}
	snapshots, err := snapshotDependencyFiles(goMod, filepath.Join(absRoot, "go.sum"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dependencyUpdateTimeout)
	defer cancel()
	if err := run(ctx, absRoot, stdout, stderr, "mod", "tidy"); err != nil {
		return rollbackDependencyUpdate(snapshots, fmt.Errorf("tidy generated dependencies: %w", err))
	}
	return nil
}

func updateFrameworkDependencies(ctx context.Context, root string, manifest Manifest, stdout, stderr io.Writer, run dependencyCommandRunner) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	if run == nil {
		return errors.New("framework dependency command runner is nil")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	goMod := filepath.Join(absRoot, "go.mod")
	if _, err := os.Stat(goMod); err != nil {
		return fmt.Errorf("framework dependencies require go.mod: %w", err)
	}
	snapshots, err := snapshotDependencyFiles(goMod, filepath.Join(absRoot, "go.sum"))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, dependencyUpdateTimeout)
	defer cancel()
	// One module. kit lives at roost-core/kit/ since the consolidation, which
	// is a PACKAGE path, not a module path — asking `go get` for it would be
	// asking for a module that does not exist. The generator's blanket path
	// rewrite produced exactly that for a moment; the version a project pins
	// for kit is now the core version (三仓合一仓 P3).
	queries := []string{
		"github.com/tjbdwanghaibo/roost-core@" + normalizedVersionPolicy(manifest.Versions.Core),
	}
	if err := run(ctx, absRoot, stdout, stderr, append([]string{"get"}, queries...)...); err != nil {
		return rollbackDependencyUpdate(snapshots, fmt.Errorf("resolve framework dependencies: %w", err))
	}
	if err := run(ctx, absRoot, stdout, stderr, "mod", "tidy"); err != nil {
		return rollbackDependencyUpdate(snapshots, fmt.Errorf("tidy framework dependencies: %w", err))
	}
	return nil
}

func normalizedVersionPolicy(version string) string {
	version = strings.TrimSpace(version)
	if strings.EqualFold(version, "latest") {
		return "latest"
	}
	return version
}

func runDependencyCommand(ctx context.Context, root string, stdout, stderr io.Writer, args ...string) error {
	return runDependencyBinary(ctx, "go", root, stdout, stderr, args...)
}

// runDependencyBinary is runDependencyCommand with the binary as a parameter,
// so tests can stand in for the go tool.
func runDependencyBinary(ctx context.Context, binary, root string, stdout, stderr io.Writer, args ...string) error {
	// RR-20261004-13: a tree, so cancellation also kills the compile / git
	// processes go started inside root (a .roost-deps-* / .roost-generate-*
	// staging tree) instead of leaving them running there.
	if err := runCommandTree(ctx, root, appendWithoutGoWork(os.Environ(), "GOWORK=off"), stdout, stderr, binary, args...); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("go %s timed out after %s: %w", strings.Join(args, " "), dependencyUpdateTimeout, ctx.Err())
		}
		if ctx.Err() != nil {
			return fmt.Errorf("go %s interrupted: %w", strings.Join(args, " "), ctx.Err())
		}
		return fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func appendWithoutGoWork(environment []string, value string) []string {
	filtered := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if strings.HasPrefix(strings.ToUpper(item), "GOWORK=") {
			continue
		}
		filtered = append(filtered, item)
	}
	return append(filtered, value)
}

func snapshotDependencyFiles(paths ...string) ([]dependencyFileSnapshot, error) {
	snapshots := make([]dependencyFileSnapshot, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			snapshots = append(snapshots, dependencyFileSnapshot{path: path})
			continue
		}
		if err != nil {
			return nil, err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, dependencyFileSnapshot{path: path, body: body, mode: info.Mode().Perm(), exists: true})
	}
	return snapshots, nil
}

func rollbackDependencyUpdate(snapshots []dependencyFileSnapshot, cause error) error {
	var rollbackErr error
	for _, snapshot := range snapshots {
		if snapshot.exists {
			if err := writeAtomic(snapshot.path, snapshot.body, snapshot.mode); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore %s: %w", snapshot.path, err))
			}
			continue
		}
		if err := os.Remove(snapshot.path); err != nil && !os.IsNotExist(err) {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove generated %s: %w", snapshot.path, err))
		}
	}
	return errors.Join(cause, rollbackErr)
}
