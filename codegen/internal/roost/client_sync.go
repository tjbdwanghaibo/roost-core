package roost

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/protocol"
)

// roost client sync：把服务端工程导出目录（client_export）的 csharp/ 拷进客户端目录，并对比两边的 manifest.json，
// 报告新增、变化与删除的消息号和错误码（维护者 2026-11-17：简单版，不生成 Unity 的 .meta）。
//
// 客户端目录里：csharp/ 下的文件平铺拷入，另存一份服务端的 manifest.json，下次同步拿它对比。
// 上次 manifest 登记过、这次没有的文件删掉；目录里同名但不是上次同步写的文件拒绝覆盖。

func runClient(args []string, stdout, stderr io.Writer) error {
	const usage = "usage: roost client sync --from <server project> --to <client dir> [--dry-run]"
	if len(args) == 0 || args[0] != "sync" {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("client sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "server project root (the directory holding roost.yaml with client_export)")
	to := fs.String("to", "", "client directory that receives the exported C# files and manifest.json")
	dryRun := fs.Bool("dry-run", false, "report what would change without writing")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*from) == "" || strings.TrimSpace(*to) == "" {
		return errors.New(usage)
	}
	return ClientSync(*from, *to, *dryRun, stdout)
}

// ClientSync 是 roost client sync 的实现。
func ClientSync(from, to string, dryRun bool, stdout io.Writer) error {
	m, err := LoadManifest(from)
	if err != nil {
		return err
	}
	if m.ClientExport == nil {
		return fmt.Errorf("%s does not declare client_export in %s; add it and run roost generate there first", from, ManifestName)
	}
	if !m.ClientExport.csharp() {
		return fmt.Errorf("client_export in %s does not list csharp in languages; roost client sync copies the C# files", filepath.Join(from, ManifestName))
	}
	exportDir := under(from, m.ClientExport.dir())
	manifestPath := filepath.Join(exportDir, exportManifest)
	manifestRaw, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("%s is missing; run roost generate in %s first", manifestPath, from)
	}
	if err != nil {
		return err
	}
	next, err := readClientExportManifest(manifestPath)
	if err != nil {
		return err
	}
	if problems := verifyExportFiles(exportDir, next); len(problems) > 0 {
		return fmt.Errorf("refuse to sync from %s: %s; run roost generate in %s", exportDir, strings.Join(problems, "; "), from)
	}
	files := map[string][]byte{}
	for name := range next.Files {
		if !strings.HasPrefix(name, "csharp/") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(exportDir, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		files[path.Base(name)] = raw
	}

	var previous *clientExportManifest
	previousPath := filepath.Join(to, exportManifest)
	if _, statErr := os.Stat(previousPath); statErr == nil {
		old, err := readClientExportManifest(previousPath)
		if err != nil {
			return fmt.Errorf("refuse to overwrite %s: %w", previousPath, err)
		}
		previous = &old
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	owned := map[string]string{} // 上次同步写的文件 → 当时的哈希
	if previous != nil {
		for name, hash := range previous.Files {
			if strings.HasPrefix(name, "csharp/") {
				owned[path.Base(name)] = hash
			}
		}
	}

	type change struct{ name, action string }
	var changes []change
	var overwritten []string
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		target := filepath.Join(to, name)
		existing, readErr := os.ReadFile(target)
		switch {
		case os.IsNotExist(readErr):
			changes = append(changes, change{name, "added"})
		case readErr != nil:
			return readErr
		case bytes.Equal(existing, files[name]):
			changes = append(changes, change{name, "unchanged"})
		default:
			hash, ours := owned[name]
			if !ours {
				return fmt.Errorf("refuse to overwrite %s: it was not written by a previous roost client sync (no entry in %s); move it away or pick another --to", target, previousPath)
			}
			if contentHash(existing) != hash {
				overwritten = append(overwritten, name)
			}
			changes = append(changes, change{name, "updated"})
		}
	}
	var removed []string
	for name := range owned {
		if _, keep := files[name]; keep {
			continue
		}
		if _, err := os.Stat(filepath.Join(to, name)); err == nil {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)

	verb := "synced"
	if dryRun {
		verb = "would sync"
	}
	fmt.Fprintf(stdout, "%s %s -> %s\n", verb, exportDir, to)
	if !dryRun {
		if err := os.MkdirAll(to, 0o755); err != nil {
			return err
		}
		for _, item := range changes {
			if item.action == "unchanged" {
				continue
			}
			if err := writeAtomic(filepath.Join(to, item.name), files[item.name], 0o644); err != nil {
				return err
			}
		}
		for _, name := range removed {
			if err := os.Remove(filepath.Join(to, name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := writeAtomic(previousPath, manifestRaw, 0o644); err != nil {
			return err
		}
	}
	counts := map[string]int{}
	for _, item := range changes {
		counts[item.action]++
	}
	fmt.Fprintf(stdout, "files: %d added, %d updated, %d removed, %d unchanged\n", counts["added"], counts["updated"], len(removed), counts["unchanged"])
	for _, item := range changes {
		if item.action != "unchanged" {
			fmt.Fprintf(stdout, "  %s %s\n", item.action, item.name)
		}
	}
	for _, name := range removed {
		fmt.Fprintf(stdout, "  removed %s\n", name)
	}
	for _, name := range overwritten {
		fmt.Fprintf(stdout, "  note: local edits to %s were replaced (generated files are not edited by hand)\n", name)
	}
	reportClientExportDiff(stdout, previous, next)
	return nil
}

// reportClientExportDiff 打印两份导出清单的差别：版本、消息号、错误码。
func reportClientExportDiff(w io.Writer, previous *clientExportManifest, next clientExportManifest) {
	if previous == nil {
		fmt.Fprintf(w, "first sync: core %s, generator %s, protoc %s / Google.Protobuf %s; %d message id(s), %d errcode(s)\n",
			orNone(next.Core), orNone(next.Generator), orNone(next.Protoc), orNone(next.GoogleProtobuf), len(next.Messages), len(next.ErrorCodes))
		return
	}
	for _, item := range []struct{ label, before, after string }{
		{"core", previous.Core, next.Core},
		{"generator", previous.Generator, next.Generator},
		{"protoc", previous.Protoc, next.Protoc},
		{"Google.Protobuf", previous.GoogleProtobuf, next.GoogleProtobuf},
	} {
		if item.before != item.after {
			fmt.Fprintf(w, "%s: %s -> %s\n", item.label, orNone(item.before), orNone(item.after))
		}
	}
	messageKey := func(message protocol.ClientMessage) string { return message.Direction + " " + message.Name }
	describeMessage := func(message protocol.ClientMessage) string {
		return fmt.Sprintf("%s %s = %d (%s)", message.Direction, message.Name, message.ID, message.Type)
	}
	oldMessages := map[string]protocol.ClientMessage{}
	for _, message := range previous.Messages {
		oldMessages[messageKey(message)] = message
	}
	newMessages := map[string]bool{}
	var added, changed, removed []string
	for _, message := range next.Messages {
		key := messageKey(message)
		newMessages[key] = true
		old, existed := oldMessages[key]
		switch {
		case !existed:
			added = append(added, describeMessage(message))
		case old != message:
			changed = append(changed, fmt.Sprintf("%s: %d (%s) -> %d (%s)", key, old.ID, old.Type, message.ID, message.Type))
		}
	}
	for _, message := range previous.Messages {
		if !newMessages[messageKey(message)] {
			removed = append(removed, describeMessage(message))
		}
	}
	printDiffList(w, "message ids added", added)
	printDiffList(w, "message ids changed", changed)
	printDiffList(w, "message ids removed", removed)

	oldCodes := map[string]clientErrorCode{}
	for _, code := range previous.ErrorCodes {
		oldCodes[code.Name] = code
	}
	newCodes := map[string]bool{}
	added, changed, removed = nil, nil, nil
	for _, code := range next.ErrorCodes {
		newCodes[code.Name] = true
		old, existed := oldCodes[code.Name]
		switch {
		case !existed:
			added = append(added, fmt.Sprintf("%s = %d %q", code.Name, code.Code, code.Message))
		case old != code:
			changed = append(changed, fmt.Sprintf("%s: %d %q -> %d %q", code.Name, old.Code, old.Message, code.Code, code.Message))
		}
	}
	for _, code := range previous.ErrorCodes {
		if !newCodes[code.Name] {
			removed = append(removed, fmt.Sprintf("%s = %d", code.Name, code.Code))
		}
	}
	printDiffList(w, "errcodes added", added)
	printDiffList(w, "errcodes changed", changed)
	printDiffList(w, "errcodes removed", removed)
}

func printDiffList(w io.Writer, label string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "%s (%d):\n", label, len(items))
	for _, item := range items {
		fmt.Fprintf(w, "  %s\n", item)
	}
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
