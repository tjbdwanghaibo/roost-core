package errcode

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Definition struct {
	Code    int32
	Name    string
	Message string
	File    string
}

var defineRE = regexp.MustCompile(`errcode\.Define\(\s*([0-9]+)\s*,\s*("(?:[^"\\]|\\.)*")\s*,\s*("(?:[^"\\]|\\.)*")\s*\)`)

// DefaultOutFile is where the error-code table is written inside a business
// project; `roost generate` refers to it (U-0118).
const DefaultOutFile = "docs/generated/errcode.csv"

func Run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("errcode", flag.ContinueOnError)
	flags.SetOutput(stdout)
	root := flags.String("root", ".", "repository root")
	out := flags.String("out", DefaultOutFile, "output csv path")
	if err := flags.Parse(args); err != nil {
		return err
	}

	defs, err := extractDefinitions(*root)
	if err != nil {
		return err
	}
	if err := writeCSV(*out, defs); err != nil {
		return err
	}
	return nil
}

func extractDefinitions(root string) ([]Definition, error) {
	root = filepath.Clean(root)
	var defs []Definition
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "disk", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(filepath.Base(path), "_test.go") || filepath.Base(path) == "main.go" && filepath.Dir(path) == filepath.Join(root, "tool", "errcode") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		matches := defineRE.FindAllSubmatch(raw, -1)
		if len(matches) == 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		for _, m := range matches {
			code64, err := strconv.ParseInt(string(m[1]), 10, 32)
			if err != nil {
				return fmt.Errorf("%s: parse errcode %q: %w", rel, m[1], err)
			}
			name, err := strconv.Unquote(string(m[2]))
			if err != nil {
				return fmt.Errorf("%s: parse errcode name: %w", rel, err)
			}
			message, err := strconv.Unquote(string(m[3]))
			if err != nil {
				return fmt.Errorf("%s: parse errcode message: %w", rel, err)
			}
			defs = append(defs, Definition{Code: int32(code64), Name: name, Message: message, File: rel})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(defs, func(i, j int) bool {
		if defs[i].Code == defs[j].Code {
			return defs[i].Name < defs[j].Name
		}
		return defs[i].Code < defs[j].Code
	})
	for i := 1; i < len(defs); i++ {
		if defs[i].Code == defs[i-1].Code {
			return nil, fmt.Errorf("duplicate errcode %d: %s and %s", defs[i].Code, defs[i-1].File, defs[i].File)
		}
	}
	return defs, nil
}

func writeCSV(path string, defs []Definition) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"Code", "Name", "Message", "File"}); err != nil {
		return err
	}
	for _, def := range defs {
		if err := w.Write([]string{
			strconv.FormatInt(int64(def.Code), 10),
			def.Name,
			def.Message,
			def.File,
		}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}
