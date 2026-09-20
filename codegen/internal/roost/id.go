package roost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var markerIDPattern = regexp.MustCompile(`//[a-z]+:(msg|push|entity|component|errcode)[^\n]*\b(id|kind|type|code)=([0-9]+)`)
var errcodeDefinePattern = regexp.MustCompile(`errcode\.Define\(\s*([0-9]+)\s*,`)

type IDUse struct {
	Kind string
	ID   int64
	File string
}

func ScanIDs(root string) ([]IDUse, error) {
	var uses []IDUse
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range markerIDPattern.FindAllSubmatch(raw, -1) {
			id, _ := strconv.ParseInt(string(match[3]), 10, 64)
			kind := string(match[1])
			if kind == "msg" || kind == "push" {
				kind = "protocol"
			}
			rel, _ := filepath.Rel(root, path)
			uses = append(uses, IDUse{Kind: kind, ID: id, File: filepath.ToSlash(rel)})
		}
		for _, match := range errcodeDefinePattern.FindAllSubmatch(raw, -1) {
			id, _ := strconv.ParseInt(string(match[1]), 10, 64)
			rel, _ := filepath.Rel(root, path)
			uses = append(uses, IDUse{Kind: "errcode", ID: id, File: filepath.ToSlash(rel)})
		}
		return nil
	})
	sort.Slice(uses, func(i, j int) bool {
		if uses[i].Kind == uses[j].Kind {
			return uses[i].ID < uses[j].ID
		}
		return uses[i].Kind < uses[j].Kind
	})
	return uses, err
}

func CheckIDs(root string, m Manifest) error {
	uses, err := ScanIDs(root)
	if err != nil {
		return err
	}
	seen := map[string]IDUse{}
	var joined error
	for _, use := range uses {
		key := fmt.Sprintf("%s:%d", use.Kind, use.ID)
		if previous, ok := seen[key]; ok {
			joined = errors.Join(joined, fmt.Errorf("duplicate %s id %d: %s and %s", use.Kind, use.ID, previous.File, use.File))
		} else {
			seen[key] = use
		}
		space, ok := m.IDs[use.Kind]
		if ok && !idInConfiguredSpace(space, use.ID) {
			joined = errors.Join(joined, fmt.Errorf("%s id %d in %s is outside every configured range", use.Kind, use.ID, use.File))
		}
	}
	return joined
}

func idInConfiguredSpace(space IDSpace, id int64) bool {
	if space.Min > 0 && id >= space.Min && id <= space.Max {
		return true
	}
	for _, configured := range space.Groups {
		if id >= configured.Min && id <= configured.Max {
			return true
		}
	}
	return false
}

func NextID(root string, m Manifest, kind, group string) (int64, error) {
	space, ok := m.IDs[kind]
	if !ok {
		return 0, fmt.Errorf("id space %q is not configured", kind)
	}
	rangeSpec := IDRange{Min: space.Min, Max: space.Max}
	if group != "" {
		var exists bool
		rangeSpec, exists = space.Groups[group]
		if !exists {
			return 0, fmt.Errorf("id group %q is not configured for %s", group, kind)
		}
	}
	if rangeSpec.Min <= 0 || rangeSpec.Max < rangeSpec.Min {
		return 0, fmt.Errorf("id space %s has no usable range", kind)
	}
	uses, err := ScanIDs(root)
	if err != nil {
		return 0, err
	}
	used := map[int64]bool{}
	for _, use := range uses {
		if use.Kind == kind {
			used[use.ID] = true
		}
	}
	for id := rangeSpec.Min; id <= rangeSpec.Max; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("id space %s is exhausted", kind)
}
