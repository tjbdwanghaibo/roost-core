package roost

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// `roost id entity`：完整 EntityID 与（唯一号、kind、category、remote）之间的双向换算（DESIGN-TODO D9，
// 维护者 2026-10-16 决定 B）。DAO 文档的 _id、日志里的 entity= 都是完整 EntityID，运维手里的是玩家 ID，
// 两边对不上时用它换算。
//
// 位布局是 framework/entity/idgen.go 的副本（生成器不 import 运行时，见根包 TestCoreDependencyBoundary），
// TestRoostIDEntityLayoutMatchesTheFramework 读那份源码的常量比对：
//
//	[sign(1)=0][remote(1)][unique(52)][kind(8)][category(2)]
//
// category 只存低两位（取值 & 3）：框架按 kind 注册的分类为准，ID 里的两位只是本进程不认识该 kind 时的兜底，
// 所以解码只能给出这两位，声明的分类要看工程里该 kind 的 //roost:entity 标记。
const (
	entityIDCategoryBits = 2
	entityIDKindBits     = 8
	entityIDRemoteBits   = 1
	entityIDUniqueBits   = 52

	entityIDKindShift   = entityIDCategoryBits
	entityIDUniqueShift = entityIDCategoryBits + entityIDKindBits
	entityIDRemoteShift = entityIDUniqueShift + entityIDUniqueBits

	entityIDCategoryMask = uint64(1)<<entityIDCategoryBits - 1
	entityIDKindMask     = uint64(1)<<entityIDKindBits - 1
	entityIDUniqueMask   = uint64(1)<<entityIDUniqueBits - 1
)

// frameworkCategories 是 framework/entity/category_order.go 推荐分类常量的取值，用于把 //roost:entity
// 标记里的 category= 换成数字。业务自定义的分类表达式不在这里，换算时需要显式 --category。
var frameworkCategories = map[string]int64{
	"EntityCategoryRemote":       1,
	"EntityCategoryWorld":        2,
	"EntityCategoryPlayerScoped": 3,
	"EntityCategoryPlayer":       4,
	"EntityCategoryOther":        5,
}

type entityIDParts struct {
	unique   int64
	kind     int64
	category int64 // 编码时是声明的分类；解码时只有低两位
	remote   bool
}

func encodeEntityID(parts entityIDParts) (int64, error) {
	switch {
	case parts.unique <= 0 || uint64(parts.unique) > entityIDUniqueMask:
		return 0, fmt.Errorf("unique id %d outside 1..%d", parts.unique, entityIDUniqueMask)
	case parts.kind <= 0 || uint64(parts.kind) > entityIDKindMask:
		return 0, fmt.Errorf("kind %d outside 1..%d", parts.kind, entityIDKindMask)
	case parts.category <= 0 || parts.category > 255:
		return 0, fmt.Errorf("category %d outside 1..255", parts.category)
	}
	id := uint64(parts.unique)<<entityIDUniqueShift | uint64(parts.kind)<<entityIDKindShift | uint64(parts.category)&entityIDCategoryMask
	if parts.remote {
		id |= 1 << entityIDRemoteShift
	}
	return int64(id), nil
}

func decodeEntityID(id int64) (entityIDParts, error) {
	if id <= 0 {
		return entityIDParts{}, fmt.Errorf("entity id %d is not a positive EntityID", id)
	}
	v := uint64(id)
	parts := entityIDParts{
		unique:   int64(v >> entityIDUniqueShift & entityIDUniqueMask),
		kind:     int64(v >> entityIDKindShift & entityIDKindMask),
		category: int64(v & entityIDCategoryMask),
		remote:   v>>entityIDRemoteShift&1 == 1,
	}
	if parts.unique == 0 || parts.kind == 0 {
		return parts, fmt.Errorf("entity id %d has unique=%d kind=%d; a full EntityID has both non-zero (is this a raw player id? encode it with --unique)", id, parts.unique, parts.kind)
	}
	return parts, nil
}

// projectEntityKind 是工程里一个 //roost:entity 标记：类型名、kind 编号与 category 表达式。
type projectEntityKind struct {
	name     string
	kind     int64
	category string
	managed  bool // remote=managed：kind 由 Remote 托管，EntityID 带 Remote 位
}

var entityMarkerPattern = regexp.MustCompile(`//roost:entity\b([^\n]*)\n(?:\s*//[^\n]*\n)*\s*type\s+([A-Za-z_][A-Za-z0-9_]*)`)
var markerParamPattern = regexp.MustCompile(`\b(id|category|remote)=([^\s]+)`)

// projectEntityKinds 扫描 game/entities/*/*.go 的 //roost:entity 标记。
func projectEntityKinds(root string) ([]projectEntityKind, error) {
	paths, err := filepath.Glob(filepath.Join(root, "game", "entities", "*", "*.go"))
	if err != nil {
		return nil, err
	}
	var kinds []projectEntityKind
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		for _, match := range entityMarkerPattern.FindAllSubmatch(raw, -1) {
			kind := projectEntityKind{name: string(match[2])}
			for _, param := range markerParamPattern.FindAllSubmatch(match[1], -1) {
				switch string(param[1]) {
				case "id":
					kind.kind, _ = strconv.ParseInt(string(param[2]), 10, 64)
				case "category":
					kind.category = string(param[2])
				case "remote":
					kind.managed = string(param[2]) == "managed"
				}
			}
			if kind.kind > 0 {
				kinds = append(kinds, kind)
			}
		}
	}
	return kinds, nil
}

// categoryValue 把标记里的 category 表达式（entity.EntityCategoryOther、EntityCategoryWorld 或数字）换成数字。
func categoryValue(expression string) (int64, bool) {
	if value, err := strconv.ParseInt(expression, 10, 64); err == nil {
		return value, true
	}
	name := expression
	if index := strings.LastIndex(name, "."); index >= 0 {
		name = name[index+1:]
	}
	value, ok := frameworkCategories[name]
	return value, ok
}

// runIDEntity 实现 `roost id entity`：
//
//	roost id entity <entity-id>                                     解码
//	roost id entity --unique <id> --entity <Name>                   按工程里的 //roost:entity 编码
//	roost id entity --unique <id> --kind <n> --category <n> [--remote]  按数字编码
//
// 只有 --entity 与解码时的 kind 注释需要工程（roost.yaml 所在目录）；按数字换算在任何目录都能用。
func runIDEntity(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("id entity", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootFlag := fs.String("root", ".", "project directory (for --entity and kind names)")
	unique := fs.Int64("unique", 0, "unique id (the player id) to encode")
	entityName := fs.String("entity", "", "Entity type whose //roost:entity kind and category to encode with")
	kind := fs.Int64("kind", 0, "EntityKind number to encode with")
	category := fs.Int64("category", 0, "EntityCategory number to encode with")
	remote := fs.Bool("remote", false, "set the Remote bit (kind registered remote=managed)")
	var positional []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	kinds := func() []projectEntityKind {
		root, err := projectRoot(*rootFlag)
		if err != nil {
			return nil
		}
		found, _ := projectEntityKinds(root)
		return found
	}
	if len(positional) > 0 {
		if len(positional) != 1 || *unique != 0 || *entityName != "" || *kind != 0 || *category != 0 || *remote {
			return errors.New("usage: roost id entity <entity-id>  |  roost id entity --unique <id> (--entity <Name> | --kind <n> --category <n> [--remote])")
		}
		id, err := strconv.ParseInt(positional[0], 10, 64)
		if err != nil {
			return fmt.Errorf("entity id %q is not an integer", positional[0])
		}
		parts, err := decodeEntityID(id)
		if err != nil {
			return err
		}
		kindLabel := strconv.FormatInt(parts.kind, 10)
		declared := ""
		for _, known := range kinds() {
			if known.kind == parts.kind {
				kindLabel += " (" + known.name + ")"
				if known.category != "" {
					declared = known.category
				}
			}
		}
		fmt.Fprintf(stdout, "entity_id: %d\nunique_id: %d\nkind: %s\ncategory_bits: %d", id, parts.unique, kindLabel, parts.category)
		if declared != "" {
			fmt.Fprintf(stdout, " (declared %s)", declared)
		}
		fmt.Fprintf(stdout, "\nremote: %t\n", parts.remote)
		return nil
	}
	if *unique == 0 {
		return errors.New("usage: roost id entity <entity-id>  |  roost id entity --unique <id> (--entity <Name> | --kind <n> --category <n> [--remote])")
	}
	parts := entityIDParts{unique: *unique, kind: *kind, category: *category, remote: *remote}
	if *entityName != "" {
		if *kind != 0 || *category != 0 {
			return errors.New("--entity takes kind and category from the project; drop --kind / --category")
		}
		root, err := projectRoot(*rootFlag)
		if err != nil {
			return fmt.Errorf("--entity needs the project: %w", err)
		}
		found, err := projectEntityKinds(root)
		if err != nil {
			return err
		}
		matched := false
		for _, known := range found {
			if known.name != toPascal(*entityName) {
				continue
			}
			value, ok := categoryValue(known.category)
			if !ok {
				return fmt.Errorf("entity %s declares category=%q, which roost cannot evaluate; pass --kind %d --category <n>", known.name, known.category, known.kind)
			}
			parts.kind, parts.category, matched = known.kind, value, true
			// Remote 位随 kind 的 Remote 能力（标记 remote=managed）；只在本进程以外注册 Remote 能力的 kind 用 --remote。
			parts.remote = parts.remote || known.managed
		}
		if !matched {
			return fmt.Errorf("entity %s has no //roost:entity marker under game/entities", toPascal(*entityName))
		}
	}
	id, err := encodeEntityID(parts)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "entity_id: %d\nunique_id: %d\nkind: %d\ncategory: %d (stored bits %d)\nremote: %t\n",
		id, parts.unique, parts.kind, parts.category, uint64(parts.category)&entityIDCategoryMask, parts.remote)
	return nil
}
