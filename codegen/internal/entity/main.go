// tool/entity generates entity factory, snapshot, and wiring code.
//
// Usage:
//
//	go run ./tool/entity -dir ./game/player
//
// Or via go:generate in the entity definition file:
//
//	//go:generate go run github.com/tjbdwanghaibo/cube/tool/entity
//
// The generator scans for struct types annotated with the marker comment:
//
//	//roost:entity entityKind=EntityKindPlayer
//	type Player struct { ... }
//
// And produces a <entity>_gen_wire.go file containing:
//   - NewXxx(param) factory function
//   - Base/Dirty and component/DAO accessor methods
//   - transactional PrepareDelete() registration for persistent DAOs
//   - Hooks wiring (onClear, onDestroy)
package entity

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/marker"
)

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("entity", flag.ContinueOnError)
	flags.SetOutput(stdout)
	dir := flags.String("dir", "", "directory to scan (default: GOFILE dir or cwd)")
	output := flags.String("output", "", "output file (default: <entity>_gen_wire.go)")
	force := flags.Bool("force", false, "force regeneration even if unchanged")
	selfSync := flags.String("self-sync", "", "access.player.self_sync: the entity (struct name) that syncs to its own player; defaults its marker to sync=true and generates its packer when subjectPacker= is absent")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", flags.Args())
	}

	// Determine scan directory
	scanDir := *dir
	if scanDir == "" {
		// Support go:generate mode
		if gofile := os.Getenv("GOFILE"); gofile != "" {
			scanDir = filepath.Dir(gofile)
		} else {
			scanDir = "."
		}
	}

	scanDir, err := filepath.Abs(scanDir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}

	scanDirs := []string{scanDir}
	if *output == "" {
		scanDirs, err = findEntityDirs(scanDir)
		if err != nil {
			return fmt.Errorf("scan entities: %w", err)
		}
	}
	if len(scanDirs) == 0 {
		fmt.Fprintf(stdout, "no entity markers found in %s\n", scanDir)
		return nil
	}

	if err := rejectMirrorsOfSnapshotlessKinds(scanDirs); err != nil {
		return err
	}
	selfSyncFound := false
	// viewKeep：每个 game/view 目录里本次生成的 entity_*.go；全量扫描结束后删掉别的生成文件。
	viewKeep := map[string]map[string]bool{}
	for _, dir := range scanDirs {
		// Parse all .go files in the directory
		defs, pkg, err := parsePackage(dir)
		if err != nil {
			return fmt.Errorf("parse %s: %w", dir, err)
		}
		entities, mirrors := defs.Entities, defs.Mirrors
		if len(entities) == 0 && len(mirrors) == 0 && *output != "" {
			for _, path := range []string{*output, guardTestFileFor(*output)} {
				if _, err := retireEntityFile(path); err != nil {
					return err
				}
			}
			continue
		}

		// -output names ONE file, which only makes sense for the go:generate
		// single-entity mode. Each entity generates its own <name>_gen_wire.go
		// plus a companion guard test, so pointing every entity of a package at
		// one path silently overwrote all but the last, taking the package-level
		// RegisterEntity down with whichever file lost. The tool exited 0 and
		// the consumer failed to compile. Refused here, before
		// anything is written, so a failed run leaves no partial output.
		if *output != "" && len(entities)+len(mirrors) > 1 {
			names := make([]string, 0, len(entities)+len(mirrors))
			for _, ent := range entities {
				names = append(names, ent.Name)
			}
			for _, mirror := range mirrors {
				names = append(names, mirror.Name)
			}
			sort.Strings(names)
			return fmt.Errorf("-output names one file but %s declares %d entities / mirror DTOs (%s): each generates its own file, so one path would overwrite all but the last; drop -output to generate them side by side",
				dir, len(names), strings.Join(names, ", "))
		}
		// 实体与只读 DTO 各写 <snake>_gen_wire.go：同名会互相覆盖，生成前拒绝。
		outputs := make(map[string]string, len(entities)+len(mirrors))
		for _, name := range append(entityNames(entities), mirrorNames(mirrors)...) {
			file := toSnake(name)
			if other, dup := outputs[file]; dup {
				return fmt.Errorf("%s: %s and %s would both generate %s_gen_wire.go; rename one", dir, other, name, file)
			}
			outputs[file] = name
		}

		// Generate for each entity. The package-level RegisterEntity goes into
		// the first entity's file (by name) and calls every sibling's own
		// registration, so several entities in one package compile together.
		siblings := make([]string, 0, len(entities))
		// selfSyncPlan 是本包里 -self-sync 实体的结论（access.player.self_sync，见 self_sync_packer.go）。
		var selfSyncEntity *EntityDef
		var selfSyncPlan SelfSyncPlan
		for i := range entities {
			ent := &entities[i]
			// 先校验整包，拒绝时本包不落任何文件。
			if *selfSync != "" && ent.Name == *selfSync {
				plan, err := applySelfSyncDefaults(ent, dir)
				if err != nil {
					return err
				}
				selfSyncFound, selfSyncEntity, selfSyncPlan = true, ent, plan
			}
			if err := validateEntitySync(*ent); err != nil {
				return err
			}
			if err := validateRemoteDaoScopes(*ent, dir); err != nil {
				return err
			}
			if err := validateLocalPreloadDaoScopes(*ent, dir); err != nil {
				return err
			}
			if err := resolveNoCollectionDaos(ent, dir); err != nil {
				return err
			}
			siblings = append(siblings, ent.Name)
		}
		sort.Strings(siblings)
		view, err := viewTargetFor(dir)
		if err != nil {
			return err
		}
		for i := range entities {
			holders := applyViewAccessors(&entities[i], view)
			if view.Dir == "" || *output != "" {
				continue
			}
			path, changed, err := generateViewEntity(entities[i], view, holders, *force)
			if err != nil {
				return fmt.Errorf("generate view for %s: %w", entities[i].Name, err)
			}
			if viewKeep[view.Dir] == nil {
				viewKeep[view.Dir] = map[string]bool{}
			}
			viewKeep[view.Dir][filepath.Base(path)] = true
			if changed {
				fmt.Fprintf(stdout, "generated: %s\n", path)
			}
		}
		for _, ent := range entities {
			outFile := *output
			if outFile == "" {
				outFile = filepath.Join(dir, fmt.Sprintf("%s_gen_wire.go", toSnake(ent.Name)))
			}

			changed, err := generateInPackage(ent, siblings, pkg, outFile, *force)
			if err != nil {
				return fmt.Errorf("generate %s: %w", ent.Name, err)
			}
			if changed {
				fmt.Fprintf(stdout, "generated: %s\n", outFile)
			} else {
				fmt.Fprintf(stdout, "unchanged: %s\n", outFile)
			}
		}
		// 只读 DTO（//roost:mirror）：spec、解码与 reader，不参与 RegisterEntity。
		for _, mirror := range mirrors {
			outFile := *output
			if outFile == "" {
				outFile = filepath.Join(dir, fmt.Sprintf("%s_gen_wire.go", toSnake(mirror.Name)))
			}
			changed, err := generateMirror(mirror, pkg, outFile, *force)
			if err != nil {
				return fmt.Errorf("generate mirror %s: %w", mirror.Name, err)
			}
			if changed {
				fmt.Fprintf(stdout, "generated: %s\n", outFile)
			} else {
				fmt.Fprintf(stdout, "unchanged: %s\n", outFile)
			}
			// 由实体改成 DTO 时，旧实体留下的伴生守卫测试会编译不过，删掉。
			if _, err := retireEntityFile(guardTestFileFor(outFile)); err != nil {
				return err
			}
		}
		// 默认 packer：要的写出，不要的（开关关闭、业务写了 subjectPacker=、实体改名或删除）删掉。
		keepPacker := ""
		if selfSyncEntity != nil && selfSyncPlan.DefaultPacker {
			keepPacker = selfSyncPlan.PackerFile
			changed, err := generateSelfSyncPacker(*selfSyncEntity, selfSyncPlan, pkg, dir, *force)
			if err != nil {
				return err
			}
			path := filepath.Join(dir, keepPacker)
			if changed {
				fmt.Fprintf(stdout, "generated: %s\n", path)
			} else {
				fmt.Fprintf(stdout, "unchanged: %s\n", path)
			}
			if selfSyncPlan.NoSyncFields {
				fmt.Fprintf(stdout, "warning: %s\n", selfSyncPlan.NoSyncFieldsWarning(selfSyncEntity.Name))
			}
		}
		if *output == "" || keepPacker != "" {
			if err := retireSelfSyncPackers(dir, keepPacker, stdout); err != nil {
				return err
			}
		}
		if *output == "" {
			if err := retireEntityOrphans(dir, siblings, mirrorNames(mirrors), stdout); err != nil {
				return err
			}
			if err := writeLifecycle(dir, pkg, entities, *force, stdout); err != nil {
				return err
			}
		}
	}
	// 删掉不再对应任何实体的 view 实体文件：全量扫描时以本次生成的为准；只扫一部分实体包时
	// 按 game/entities/*/ 里仍有的 //roost:entity 标记判断（本次生成的也算在内）。
	if *output == "" {
		if viewDir := viewDirOfFullScan(scanDir); viewDir != "" {
			if err := retireViewEntities(viewDir, viewKeep[viewDir], stdout); err != nil {
				return err
			}
		} else {
			for viewDir, keep := range viewKeep {
				live, err := liveViewEntityFiles(viewDir)
				if err != nil {
					return err
				}
				for name := range keep {
					live[name] = true
				}
				if err := retireViewEntities(viewDir, live, stdout); err != nil {
					return err
				}
			}
		}
	}
	if *selfSync != "" && !selfSyncFound {
		return fmt.Errorf("-self-sync %s: no //roost:entity struct named %s under %s", *selfSync, *selfSync, scanDir)
	}
	return nil
}

// rejectMirrorsOfSnapshotlessKinds refuses a //roost:mirror DTO whose owner
// kind declares snapshots=none: nothing is ever published for it, so the
// mirror would read "not found" for good. Kinds are matched by their constant
// name (EntityKindX, or pkg.EntityKindX in another package) over every
// directory this run scans; the generated registration also refuses such a
// reader at run time (entity.ErrRemoteSnapshotsDisabled) in a process that
// registered the kind.
func rejectMirrorsOfSnapshotlessKinds(dirs []string) error {
	snapshotless := map[string]string{}
	type mirrorAt struct {
		def MirrorDef
		dir string
	}
	var mirrors []mirrorAt
	for _, dir := range dirs {
		defs, _, err := parsePackage(dir)
		if err != nil {
			return fmt.Errorf("parse %s: %w", dir, err)
		}
		for _, ent := range defs.Entities {
			if ent.NoSnapshots {
				snapshotless[kindConstName(ent.EntityKind)] = ent.Name
			}
		}
		for _, mirror := range defs.Mirrors {
			mirrors = append(mirrors, mirrorAt{def: mirror, dir: dir})
		}
	}
	for _, mirror := range mirrors {
		if owner, ok := snapshotless[kindConstName(mirror.def.EntityKind)]; ok {
			return fmt.Errorf("%s: //roost:mirror %s reads %s, whose entity %s declares snapshots=none: no snapshot is ever published for it; drop the mirror or the snapshots=none",
				mirror.dir, mirror.def.Name, mirror.def.EntityKind, owner)
		}
	}
	return nil
}

// kindConstName is the constant's own name in a kind expression
// (pkg.EntityKindX → EntityKindX).
func kindConstName(expr string) string {
	if i := strings.LastIndex(expr, "."); i >= 0 {
		return expr[i+1:]
	}
	return expr
}

func entityNames(entities []EntityDef) []string {
	names := make([]string, 0, len(entities))
	for _, ent := range entities {
		names = append(names, ent.Name)
	}
	return names
}

func mirrorNames(mirrors []MirrorDef) []string {
	names := make([]string, 0, len(mirrors))
	for _, mirror := range mirrors {
		names = append(names, mirror.Name)
	}
	return names
}

func findEntityDirs(root string) ([]string, error) {
	dirs := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root {
				switch entry.Name() {
				case "testdata", "vendor":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Type().IsRegular() && (strings.HasSuffix(entry.Name(), "_gen_wire.go") || strings.HasSuffix(entry.Name(), "_gen_wire_test.go") || strings.HasSuffix(entry.Name(), selfSyncPackerSuffix)) {
			owned, err := ownsEntityFile(path)
			if err != nil {
				return err
			}
			if owned {
				dirs[filepath.Dir(path)] = true
				return nil
			}
		}
		if !strings.HasSuffix(entry.Name(), ".go") || isGeneratedWireFile(entry.Name()) || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if marker.Has(string(content), "entity") || marker.Has(string(content), "mirror") {
			dirs[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(dirs))
	for dir := range dirs {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out, nil
}

func ownsEntityFile(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return bytes.HasPrefix(raw, []byte("// Code generated by tool/entity. DO NOT EDIT.")), nil
}

func retireEntityFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	owned, err := ownsEntityFile(path)
	if err != nil {
		return false, err
	}
	if !owned {
		return false, nil
	}
	return true, os.Remove(path)
}

// writeLifecycle 给有实体的包写 lifecycle_gen.go，没有实体（只剩只读 DTO）的包删掉旧的。
func writeLifecycle(dir, pkg string, entities []EntityDef, force bool, stdout io.Writer) error {
	if len(entities) == 0 {
		path := filepath.Join(dir, lifecycleFile)
		removed, err := retireLifecycle(dir)
		if err != nil {
			return fmt.Errorf("retire entity output %s: %w", path, err)
		}
		if removed {
			fmt.Fprintf(stdout, "removed orphan: %s\n", path)
		}
		return nil
	}
	sorted := append([]EntityDef(nil), entities...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	path, changed, err := generateLifecycle(dir, pkg, sorted, force)
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintf(stdout, "generated: %s\n", path)
	} else {
		fmt.Fprintf(stdout, "unchanged: %s\n", path)
	}
	return nil
}

func retireEntityOrphans(dir string, siblings, mirrors []string, stdout io.Writer) error {
	expected := make(map[string]bool, len(siblings)*2+len(mirrors))
	for _, name := range siblings {
		wire := fmt.Sprintf("%s_gen_wire.go", toSnake(name))
		expected[wire] = true
		expected[guardTestFileFor(wire)] = true
	}
	for _, name := range mirrors {
		expected[fmt.Sprintf("%s_gen_wire.go", toSnake(name))] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if expected[name] || (!strings.HasSuffix(name, "_gen_wire.go") && !strings.HasSuffix(name, "_gen_wire_test.go")) {
			continue
		}
		path := filepath.Join(dir, name)
		removed, err := retireEntityFile(path)
		if err != nil {
			return fmt.Errorf("retire entity output %s: %w", path, err)
		}
		if removed {
			fmt.Fprintf(stdout, "removed orphan: %s\n", path)
		}
	}
	return nil
}
