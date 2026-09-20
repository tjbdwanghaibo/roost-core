package roost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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

// UpdateFrameworkDependencies resolves core, kit and skill in one go-get
// operation. Keeping all three as direct requirements lets Go's minimal
// version selection retain the highest selected core/kit versions even when a
// downstream framework module declares an older lower bound.
func UpdateFrameworkDependencies(root string, manifest Manifest, stdout, stderr io.Writer) error {
	return updateFrameworkDependenciesTransactional(root, manifest, stdout, stderr, runDependencyCommand)
}

func updateFrameworkDependenciesTransactional(root string, manifest Manifest, stdout, stderr io.Writer, run dependencyCommandRunner) error {
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
	defer os.RemoveAll(stage)
	if err := copyProject(absRoot, stage); err != nil {
		return fmt.Errorf("stage framework dependencies: %w", err)
	}
	inputs, err := snapshotProjectInputs(stage, manifest)
	if err != nil {
		return fmt.Errorf("snapshot dependency inputs: %w", err)
	}
	if err := updateFrameworkDependencies(stage, manifest, stdout, stderr, run); err != nil {
		return err
	}
	changes, err := planExplicitStagedFiles(absRoot, stage, "go.mod", "go.sum")
	if err != nil {
		return err
	}
	if err := verifyProjectInputs(absRoot, manifest, inputs); err != nil {
		return err
	}
	return commitSyncChanges(changes)
}

// TidyProjectDependencies records module checksums introduced by newly
// generated imports without upgrading framework versions. This keeps the
// beginner workflow buildable immediately after `roost generate`.
func TidyProjectDependencies(root string, stdout, stderr io.Writer) error {
	return tidyProjectDependencies(root, stdout, stderr, runDependencyCommand)
}

func tidyProjectDependencies(root string, stdout, stderr io.Writer, run dependencyCommandRunner) error {
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
	ctx, cancel := context.WithTimeout(context.Background(), dependencyUpdateTimeout)
	defer cancel()
	if err := run(ctx, absRoot, stdout, stderr, "mod", "tidy"); err != nil {
		return rollbackDependencyUpdate(snapshots, fmt.Errorf("tidy generated dependencies: %w", err))
	}
	return nil
}

func updateFrameworkDependencies(root string, manifest Manifest, stdout, stderr io.Writer, run dependencyCommandRunner) error {
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
	if needsConsolidation(absRoot) {
		// Crossing the consolidation boundary (core v1.14.0 / kit v1.13.0)
		// without rewriting imports would leave the project pointing at
		// modules that no longer exist. Rewrite first, then resolve.
		fmt.Fprintln(stdout, "framework dependencies: project predates the consolidation; rewriting imports first")
		if _, err := ConsolidateProject(absRoot, false, stdout); err != nil {
			return fmt.Errorf("consolidate project before resolving dependencies: %w", err)
		}
	}
	snapshots, err := snapshotDependencyFiles(goMod, filepath.Join(absRoot, "go.sum"))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), dependencyUpdateTimeout)
	defer cancel()
	queries := []string{
		"github.com/tjbdwanghaibo/roost-core@" + normalizedVersionPolicy(manifest.Versions.Core),
		"github.com/tjbdwanghaibo/roost-kit@" + normalizedVersionPolicy(manifest.Versions.Kit),
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
	command := exec.CommandContext(ctx, "go", args...)
	command.Dir = root
	command.Env = appendWithoutGoWork(os.Environ(), "GOWORK=off")
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("go %s timed out after %s: %w", strings.Join(args, " "), dependencyUpdateTimeout, ctx.Err())
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
