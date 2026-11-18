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

// roost client sync：把服务端工程的导出目录同步进客户端工程（维护者 2026-11-17：简单版，不生成 Unity 的 .meta）。
//
//   - 协议（client_export，--to）：--lang 那种语言的目录（csharp/ 或 ts/，维护者 2026-11-18 D9）下的文件平铺拷入 --to，另存
//     一份服务端的 manifest.json，对比报告消息号与错误码。一个客户端仓库只用一种语言：服务端只声明一种时 --lang 可以省略，
//     两种都声明时必须写；
//   - 配置（client_export.config，--config-code-to / --config-data-to）：csharp/ 拷入代码目录，data/ 拷入数据目录，
//     服务端的配置清单另存为代码目录里的 config-manifest.json（同步记录，含数据版本），对比报告表、字段、行数与数据版本。
//
// 两部分规则相同：先核对服务端导出目录的哈希（手改或拷了一半的拒绝同步）；上次同步记录登记过、这次没有的文件删掉；
// 目录里同名但不是上次同步写的文件拒绝覆盖；上次同步写过、之后被手改的文件照样覆盖并提示。全部检查通过后才写文件。

// clientConfigSyncRecord 是配置同步在代码目录里另存的服务端配置清单。
const clientConfigSyncRecord = "config-manifest.json"

func runClient(args []string, stdout, stderr io.Writer) error {
	const usage = "usage: roost client sync --from <server project> [--lang csharp|ts] [--to <client dir>] [--config-code-to <dir> --config-data-to <dir>] [--dry-run]"
	if len(args) == 0 || args[0] != "sync" {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("client sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "server project root (the directory holding roost.yaml with client_export)")
	to := fs.String("to", "", "client directory that receives the exported protocol files (C# or TS, see --lang) and manifest.json")
	lang := fs.String("lang", "", "client language to sync: csharp or ts; may be omitted when client_export declares only one")
	configCode := fs.String("config-code-to", "", "client directory that receives the config C# files and config-manifest.json (client_export.config)")
	configData := fs.String("config-data-to", "", "client directory that receives the config data files (client_export.config)")
	dryRun := fs.Bool("dry-run", false, "report what would change without writing")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	codeTo, dataTo := strings.TrimSpace(*configCode), strings.TrimSpace(*configData)
	if fs.NArg() != 0 || strings.TrimSpace(*from) == "" || (strings.TrimSpace(*to) == "" && codeTo == "" && dataTo == "") {
		return errors.New(usage)
	}
	if (codeTo == "") != (dataTo == "") {
		return errors.New("--config-code-to and --config-data-to go together: the generated C# checks the data files it was generated with; " + usage)
	}
	if *lang != "" && !contains(clientExportLanguages, *lang) {
		return fmt.Errorf("--lang %q is not a client language; use one of %s", *lang, strings.Join(clientExportLanguages, ", "))
	}
	if *lang == "ts" && codeTo != "" {
		// 配置表的 TS 读取是 TS 客户端方案的第 3 轮（FEATURE-2026-11-18-ts-gate-client §9），导出里还没有 ts/。
		return errors.New("--lang ts does not sync the config export yet: client_export.config exports C# only (TS config tables are planned for a later round); drop --config-code-to / --config-data-to")
	}
	var plans []*syncPlan
	if strings.TrimSpace(*to) != "" {
		plan, err := planProtocolSync(*from, *to, *lang)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	}
	if codeTo != "" {
		plan, err := planConfigSync(*from, codeTo, dataTo)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	}
	for _, plan := range plans {
		if err := plan.run(*dryRun, stdout); err != nil {
			return err
		}
	}
	return nil
}

// ClientSync 是 roost client sync --to 的实现（协议部分）。lang 为空时取服务端声明的唯一一种语言。
func ClientSync(from, to, lang string, dryRun bool, stdout io.Writer) error {
	plan, err := planProtocolSync(from, to, lang)
	if err != nil {
		return err
	}
	return plan.run(dryRun, stdout)
}

// ClientConfigSync 是 roost client sync --config-code-to / --config-data-to 的实现（配置部分）。
func ClientConfigSync(from, codeTo, dataTo string, dryRun bool, stdout io.Writer) error {
	plan, err := planConfigSync(from, codeTo, dataTo)
	if err != nil {
		return err
	}
	return plan.run(dryRun, stdout)
}

// syncFile 是同步要写进客户端的一个文件。
type syncFile struct {
	label  string // 报告里的名字
	target string // 客户端里的路径
	raw    []byte
}

// syncPlan 是一次同步（协议或配置）的计划：全部检查在 planSync 里做完，run 只写文件和报告。
type syncPlan struct {
	header      string
	changes     []syncChangeItem
	removed     []syncChangeItem
	overwritten []string
	record      string // 同步记录（另存的服务端清单）
	recordRaw   []byte
	report      func(io.Writer)
}

type syncChangeItem struct {
	syncFile
	action string // added / updated / unchanged / removed
}

// planSync 对比要写的文件与客户端现状。owned 是上次同步记录登记的文件（客户端路径 → 当时的哈希，label 用于报告）。
func planSync(files []syncFile, owned map[string]syncFile, record string) (*syncPlan, error) {
	plan := &syncPlan{record: record}
	sort.Slice(files, func(i, j int) bool { return files[i].label < files[j].label })
	wanted := map[string]bool{}
	for _, file := range files {
		wanted[file.target] = true
		existing, readErr := os.ReadFile(file.target)
		switch {
		case os.IsNotExist(readErr):
			plan.changes = append(plan.changes, syncChangeItem{file, "added"})
		case readErr != nil:
			return nil, readErr
		case bytes.Equal(existing, file.raw):
			plan.changes = append(plan.changes, syncChangeItem{file, "unchanged"})
		default:
			previous, ours := owned[file.target]
			if !ours {
				return nil, fmt.Errorf("refuse to overwrite %s: it was not written by a previous roost client sync (no entry in %s); move it away or pick another directory", file.target, record)
			}
			if contentHash(existing) != string(previous.raw) {
				plan.overwritten = append(plan.overwritten, file.label)
			}
			plan.changes = append(plan.changes, syncChangeItem{file, "updated"})
		}
	}
	for target, previous := range owned {
		if wanted[target] {
			continue
		}
		if _, err := os.Stat(target); err == nil {
			plan.removed = append(plan.removed, syncChangeItem{syncFile{label: previous.label, target: target}, "removed"})
		}
	}
	sort.Slice(plan.removed, func(i, j int) bool { return plan.removed[i].label < plan.removed[j].label })
	return plan, nil
}

func (plan *syncPlan) run(dryRun bool, stdout io.Writer) error {
	verb := "synced"
	if dryRun {
		verb = "would sync"
	}
	fmt.Fprintf(stdout, "%s %s\n", verb, plan.header)
	if !dryRun {
		for _, item := range plan.changes {
			if item.action == "unchanged" {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(item.target), 0o755); err != nil {
				return err
			}
			if err := writeAtomic(item.target, item.raw, 0o644); err != nil {
				return err
			}
		}
		for _, item := range plan.removed {
			if err := os.Remove(item.target); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(plan.record), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(plan.record, plan.recordRaw, 0o644); err != nil {
			return err
		}
	}
	counts := map[string]int{}
	for _, item := range plan.changes {
		counts[item.action]++
	}
	fmt.Fprintf(stdout, "files: %d added, %d updated, %d removed, %d unchanged\n", counts["added"], counts["updated"], len(plan.removed), counts["unchanged"])
	for _, item := range plan.changes {
		if item.action != "unchanged" {
			fmt.Fprintf(stdout, "  %s %s\n", item.action, item.label)
		}
	}
	for _, item := range plan.removed {
		fmt.Fprintf(stdout, "  removed %s\n", item.label)
	}
	for _, name := range plan.overwritten {
		fmt.Fprintf(stdout, "  note: local edits to %s were replaced (generated files are not edited by hand)\n", name)
	}
	plan.report(stdout)
	return nil
}

// syncLanguage 决定协议同步哪种语言：写了 --lang 就要服务端声明过它；没写时服务端只声明一种就用它，两种都声明时报错。
func syncLanguage(m Manifest, from, lang string) (string, error) {
	var declared []string
	for _, language := range clientExportLanguages {
		if m.ClientExport.protocol() && contains(m.ClientExport.Languages, language) {
			declared = append(declared, language)
		}
	}
	yaml := filepath.Join(from, ManifestName)
	switch {
	case lang != "" && !contains(declared, lang):
		return "", fmt.Errorf("client_export in %s does not list %s in languages (declared: %s); add it and run roost generate there first", yaml, lang, orNone(strings.Join(declared, ", ")))
	case lang != "":
		return lang, nil
	case len(declared) == 1:
		return declared[0], nil
	case len(declared) == 0:
		return "", fmt.Errorf("client_export in %s lists no client language (csharp or ts); roost client sync copies the generated C# or TS files", yaml)
	default:
		return "", fmt.Errorf("client_export in %s lists %s; pass --lang %s to say which one this client uses", yaml, strings.Join(declared, " and "), strings.Join(declared, "|"))
	}
}

// planProtocolSync 计划协议部分：<lang>/ 平铺进 to，另存 manifest.json。
func planProtocolSync(from, to, lang string) (*syncPlan, error) {
	m, err := LoadManifest(from)
	if err != nil {
		return nil, err
	}
	if !m.ClientExport.protocol() {
		return nil, fmt.Errorf("%s does not export the protocol (client_export in %s); add it and run roost generate there first", from, ManifestName)
	}
	if lang, err = syncLanguage(m, from, lang); err != nil {
		return nil, err
	}
	prefix := lang + "/"
	exportDir := under(from, m.ClientExport.dir())
	manifestPath := filepath.Join(exportDir, exportManifest)
	manifestRaw, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%s is missing; run roost generate in %s first", manifestPath, from)
	}
	if err != nil {
		return nil, err
	}
	next, err := readClientExportManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	if problems := verifyExportFiles(exportDir, next); len(problems) > 0 {
		return nil, fmt.Errorf("refuse to sync from %s: %s; run roost generate in %s", exportDir, strings.Join(problems, "; "), from)
	}
	var files []syncFile
	for name := range next.Files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(exportDir, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		files = append(files, syncFile{label: path.Base(name), target: filepath.Join(to, path.Base(name)), raw: raw})
	}
	var previous *clientExportManifest
	previousPath := filepath.Join(to, exportManifest)
	if _, statErr := os.Stat(previousPath); statErr == nil {
		old, err := readClientExportManifest(previousPath)
		if err != nil {
			return nil, fmt.Errorf("refuse to overwrite %s: %w", previousPath, err)
		}
		previous = &old
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}
	owned := map[string]syncFile{} // 上次同步写的文件 → 当时的哈希（只认这种语言的目录）
	if previous != nil {
		for name, hash := range previous.Files {
			if strings.HasPrefix(name, prefix) {
				owned[filepath.Join(to, path.Base(name))] = syncFile{label: path.Base(name), raw: []byte(hash)}
			}
		}
	}
	plan, err := planSync(files, owned, previousPath)
	if err != nil {
		return nil, err
	}
	plan.header = exportDir + " (" + lang + ") -> " + to
	plan.recordRaw = manifestRaw
	plan.report = func(w io.Writer) {
		reportClientExportDiff(w, previous, next)
		if lang == "ts" && next.ProtobufEs != "" {
			// 导出的 *_pb.ts 要求使用方工程装同一版运行库，版本不一致时 protobuf-es 运行期报错。
			fmt.Fprintf(w, "TS: install @bufbuild/protobuf %s exactly in the client project (the generated *_pb.ts need that runtime version)\n", next.ProtobufEs)
		}
	}
	return plan, nil
}

// planConfigSync 计划配置部分：csharp/ 平铺进 codeTo，data/ 平铺进 dataTo，服务端配置清单另存为 codeTo/config-manifest.json。
func planConfigSync(from, codeTo, dataTo string) (*syncPlan, error) {
	m, err := LoadManifest(from)
	if err != nil {
		return nil, err
	}
	if m.ClientExport == nil || m.ClientExport.Config == nil {
		return nil, fmt.Errorf("%s does not declare client_export.config in %s; add it and run roost generate there first", from, ManifestName)
	}
	exportDir := under(from, m.ClientExport.Config.dir())
	manifestPath := filepath.Join(exportDir, exportManifest)
	manifestRaw, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%s is missing; run roost generate in %s first", manifestPath, from)
	}
	if err != nil {
		return nil, err
	}
	next, err := readClientConfigManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	if problems := verifyConfigExportFiles(exportDir, next); len(problems) > 0 {
		return nil, fmt.Errorf("refuse to sync from %s: %s; run roost generate in %s", exportDir, strings.Join(problems, "; "), from)
	}
	clientPath := func(name string) string {
		if strings.HasPrefix(name, "data/") {
			return filepath.Join(dataTo, path.Base(name))
		}
		return filepath.Join(codeTo, path.Base(name))
	}
	var files []syncFile
	for name := range next.Files {
		raw, err := os.ReadFile(filepath.Join(exportDir, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		files = append(files, syncFile{label: name, target: clientPath(name), raw: raw})
	}
	record := filepath.Join(codeTo, clientConfigSyncRecord)
	var previous *clientConfigManifest
	if _, statErr := os.Stat(record); statErr == nil {
		old, err := readClientConfigManifest(record)
		if err != nil {
			return nil, fmt.Errorf("refuse to overwrite %s: %w", record, err)
		}
		previous = &old
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}
	owned := map[string]syncFile{}
	if previous != nil {
		for name, hash := range previous.Files {
			if validConfigExportFileName(name) {
				owned[clientPath(name)] = syncFile{label: name, raw: []byte(hash)}
			}
		}
	}
	plan, err := planSync(files, owned, record)
	if err != nil {
		return nil, err
	}
	plan.header = exportDir + " -> code " + codeTo + ", data " + dataTo
	plan.recordRaw = manifestRaw
	plan.report = func(w io.Writer) { reportClientConfigDiff(w, previous, next) }
	return plan, nil
}

// reportClientConfigDiff 打印两份配置导出清单的差别：数据版本（旧 → 新）、表的增删、字段的增删与改类型、行数变化。
func reportClientConfigDiff(w io.Writer, previous *clientConfigManifest, next clientConfigManifest) {
	if previous == nil {
		fmt.Fprintf(w, "first config sync: core %s, generator %s; %d table(s)\n", orNone(next.Core), orNone(next.Generator), len(next.Tables))
		fmt.Fprintf(w, "config data version: none -> %s\n", next.DataVersionShort)
		return
	}
	for _, item := range []struct{ label, before, after string }{
		{"core", previous.Core, next.Core},
		{"generator", previous.Generator, next.Generator},
		{"C# namespace", previous.Namespace, next.Namespace},
	} {
		if item.before != item.after {
			fmt.Fprintf(w, "%s: %s -> %s\n", item.label, orNone(item.before), orNone(item.after))
		}
	}
	if previous.DataVersion == next.DataVersion {
		fmt.Fprintf(w, "config data version: %s (unchanged)\n", next.DataVersionShort)
	} else {
		fmt.Fprintf(w, "config data version: %s -> %s\n", orNone(previous.DataVersionShort), next.DataVersionShort)
	}
	describeField := func(field clientConfigField) string {
		if field.Parser != "" {
			return field.Type + " (parser " + field.Parser + ")"
		}
		return field.Type
	}
	oldTables := map[string]clientConfigTable{}
	for _, table := range previous.Tables {
		oldTables[table.Name] = table
	}
	newTables := map[string]bool{}
	var tablesAdded, tablesRemoved, fieldsAdded, fieldsRemoved, fieldsChanged, rowsChanged []string
	for _, table := range next.Tables {
		newTables[table.Name] = true
		old, existed := oldTables[table.Name]
		if !existed {
			tablesAdded = append(tablesAdded, fmt.Sprintf("%s (%s, %d row(s))", table.Name, table.Kind, table.Rows))
			continue
		}
		oldFields := map[string]clientConfigField{}
		for _, field := range old.Fields {
			oldFields[field.Name] = field
		}
		newFields := map[string]bool{}
		for _, field := range table.Fields {
			newFields[field.Name] = true
			before, had := oldFields[field.Name]
			switch {
			case !had:
				fieldsAdded = append(fieldsAdded, fmt.Sprintf("%s.%s %s", table.Name, field.Name, describeField(field)))
			case describeField(before) != describeField(field):
				fieldsChanged = append(fieldsChanged, fmt.Sprintf("%s.%s: %s -> %s", table.Name, field.Name, describeField(before), describeField(field)))
			}
		}
		for _, field := range old.Fields {
			if !newFields[field.Name] {
				fieldsRemoved = append(fieldsRemoved, fmt.Sprintf("%s.%s %s", table.Name, field.Name, describeField(field)))
			}
		}
		if old.Rows != table.Rows {
			rowsChanged = append(rowsChanged, fmt.Sprintf("%s: %d -> %d", table.Name, old.Rows, table.Rows))
		}
	}
	for _, table := range previous.Tables {
		if !newTables[table.Name] {
			tablesRemoved = append(tablesRemoved, fmt.Sprintf("%s (%s, %d row(s))", table.Name, table.Kind, table.Rows))
		}
	}
	printDiffList(w, "config tables added", tablesAdded)
	printDiffList(w, "config tables removed", tablesRemoved)
	printDiffList(w, "config fields added", fieldsAdded)
	printDiffList(w, "config fields removed", fieldsRemoved)
	printDiffList(w, "config fields changed type", fieldsChanged)
	printDiffList(w, "config rows changed", rowsChanged)
}

// reportClientExportDiff 打印两份导出清单的差别：版本、消息号、错误码。
func reportClientExportDiff(w io.Writer, previous *clientExportManifest, next clientExportManifest) {
	if previous == nil {
		fmt.Fprintf(w, "first sync: core %s, generator %s, protoc %s / Google.Protobuf %s / protobuf-es %s; %d message id(s), %d errcode(s)\n",
			orNone(next.Core), orNone(next.Generator), orNone(next.Protoc), orNone(next.GoogleProtobuf), orNone(next.ProtobufEs), len(next.Messages), len(next.ErrorCodes))
		return
	}
	for _, item := range []struct{ label, before, after string }{
		{"core", previous.Core, next.Core},
		{"generator", previous.Generator, next.Generator},
		{"protoc", previous.Protoc, next.Protoc},
		{"Google.Protobuf", previous.GoogleProtobuf, next.GoogleProtobuf},
		{"protobuf-es", previous.ProtobufEs, next.ProtobufEs},
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
