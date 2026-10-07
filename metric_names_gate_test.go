package roostcore_test

// 指标名一致性门禁（F11 N3，REFACTOR-2026-10-07-structural-guards §1）。
//
// 从源码收集进程会写进 metrics 注册表的全部指标名、类型与标签键，再核对引用它们的四处：
//
//	OBSERVABILITY.md                                        指标清单表格与告警基线里写到的名字与标签都存在；
//	                                                        源码收集到的全部指标都在清单里（F11 N9）
//	observability/grafana-roost-overview.json              每条查询的指标存在、by / 选择器里的标签存在
//	demo/deploy/dev/observability/grafana/.../*.json.tmpl  同上（生成工程的 demo 仪表盘）
//	demo/deploy/dev/observability/README.md.tmpl           表格里写到的名字与标签都存在
//
// 旧缺口：这四处没有任何测试引用（生成仪表盘只核对两个名字），F04-5、总览仪表盘查不存在的
// entitysync_flush_gate_deferred_total（F11 N1）、demo 仪表盘按不存在的 subject 标签聚合（F11 N2）都由此漏入。
//
// 收集口径（与 docs/framework/guide/11-observability.md §6.3 的扫描相同）：
//   - Go 源码（非测试、非 testdata）里对 IncCounter / SetGauge / AddGauge / ObserveDuration / ObserveHistogram 的调用，
//     名字参数是字面量、包级常量（本包或别的包）、函数内的字符串局部变量，或一层包装函数的形参
//     （kit/statslog 的 publishCounts）——包装函数的调用方传进来的字面量照样收；
//   - 各包导出的 Metric* 字符串常量（类型未知时按四种导出形状都算存在）；
//   - 生成器产物里的写入：codegen 渲染进字符串的 Go 代码与 demo/ 的 .tmpl，按文本扫描同样的调用与 *Metric* 常量。
// 标签键从标签参数里的 metrics.Labels{...} / map[string]string{...} 字面量收；参数是变量、调用或字段时，
// 能在函数内找到字面量赋值就用它，否则该指标的标签记为“开放”，不做标签核对（宁可漏报不误报）。

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type metricKind int

const (
	kindUnknown metricKind = iota
	kindCounter
	kindGauge
	kindDuration
	kindHistogram
)

var metricWriteMethods = map[string]metricKind{
	"IncCounter":       kindCounter,
	"SetGauge":         kindGauge,
	"AddGauge":         kindGauge,
	"ObserveDuration":  kindDuration,
	"ObserveHistogram": kindHistogram,
}

type metricInfo struct {
	name   string
	kinds  map[metricKind]bool
	labels map[string]bool
	open   bool // 标签集合不完全可知
	sites  []string
}

type metricInventory struct {
	byName   map[string]*metricInfo
	bySeries map[string]*metricInfo // Prometheus 导出的序列名 → 指标
}

func (inv *metricInventory) add(name string, kind metricKind, labels []string, open bool, site string) {
	info := inv.byName[name]
	if info == nil {
		info = &metricInfo{name: name, kinds: map[metricKind]bool{}, labels: map[string]bool{}}
		inv.byName[name] = info
	}
	info.kinds[kind] = true
	for _, label := range labels {
		info.labels[label] = true
	}
	info.open = info.open || open
	info.sites = append(info.sites, site)
}

// promName mirrors metrics.prometheusName.
func promName(name string) string {
	var b strings.Builder
	for i, r := range name {
		if r == '_' || r == ':' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// exportedSeries mirrors metrics.PrometheusText: the series names one metric of
// the given kind produces.
func exportedSeries(name string, kind metricKind) []string {
	base := promName(name)
	switch kind {
	case kindCounter:
		if strings.HasSuffix(base, "_total") {
			return []string{base}
		}
		return []string{base + "_total"}
	case kindGauge:
		return []string{base}
	case kindDuration:
		return []string{base + "_count", base + "_sum_nanos", base + "_max_nanos", base + "_last_nanos"}
	case kindHistogram:
		return []string{base + "_bucket", base + "_sum_nanos", base + "_count"}
	}
	var all []string
	for _, k := range []metricKind{kindCounter, kindGauge, kindDuration, kindHistogram} {
		all = append(all, exportedSeries(name, k)...)
	}
	return all
}

func (inv *metricInventory) index() {
	inv.bySeries = map[string]*metricInfo{}
	for _, info := range inv.byName {
		for kind := range info.kinds {
			for _, series := range exportedSeries(info.name, kind) {
				inv.bySeries[series] = info
			}
		}
	}
}

// --- 源码收集 ---

type goFile struct {
	path    string
	dir     string // 模块内相对目录，即包路径尾
	file    *ast.File
	imports map[string]string // 本文件的包名 → 模块内相对目录
}

type metricWrapper struct {
	kind        metricKind
	nameParam   int
	labelParams []int // 作为标签键的形参
	labels      []string
	open        bool
}

func collectMetricInventory(t *testing.T) *metricInventory {
	t.Helper()
	inv := &metricInventory{byName: map[string]*metricInfo{}}
	var files []goFile
	var templates []string
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name := entry.Name(); name == ".git" || name == "vendor" || name == "testdata" || name == "artifacts" {
				return filepath.SkipDir
			}
			if path != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		slash := filepath.ToSlash(path)
		if strings.HasSuffix(slash, ".tmpl") && strings.HasPrefix(slash, "demo/") {
			templates = append(templates, path)
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.HasPrefix(slash, "codegen/") {
			templates = append(templates, path) // 渲染进字符串的生成代码
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		imports := map[string]string{}
		for _, spec := range parsed.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			rel, ok := strings.CutPrefix(importPath, modulePath+"/")
			if !ok {
				continue
			}
			name := filepath.Base(rel)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			imports[name] = rel
		}
		files = append(files, goFile{path: slash, dir: filepath.ToSlash(filepath.Dir(path)), file: parsed, imports: imports})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// 第一遍：包级字符串常量。
	consts := map[string]map[string]string{} // dir → 常量名 → 值
	var exported [][2]string                 // 导出的 Metric* 常量：值、位置
	for _, f := range files {
		for _, decl := range f.file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					if lit, ok := value.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						s, _ := strconv.Unquote(lit.Value)
						if consts[f.dir] == nil {
							consts[f.dir] = map[string]string{}
						}
						consts[f.dir][name.Name] = s
						if strings.HasPrefix(name.Name, "Metric") && name.IsExported() {
							exported = append(exported, [2]string{s, f.path + ": const " + name.Name})
						}
					}
				}
			}
		}
	}

	// 第二遍：写入点。包装函数（名字来自形参）记下来，第三遍在调用方解析。
	wrappers := map[string]map[string]metricWrapper{} // dir → 函数名 → 包装
	type pendingCall struct {
		f    goFile
		call *ast.CallExpr
		w    metricWrapper
		fn   *ast.FuncDecl
	}
	var unresolved []string
	for _, f := range files {
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			params := funcParams(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if lit, ok := n.(*ast.CompositeLit); ok && f.dir == "metrics" {
					// 注册表在 Snapshot 里自己合成的序列（obs.series.dropped）。
					addSnapshotLiteral(inv, lit, f, consts, fn)
					return true
				}
				call, ok := n.(*ast.CallExpr)
				if !ok || f.dir == "metrics" {
					return true // metrics 包里的写入调用是注册表 API 的转发，名字来自调用方
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				kind, ok := metricWriteMethods[sel.Sel.Name]
				if !ok || len(call.Args) < 2 {
					return true
				}
				site := f.path + ":" + sel.Sel.Name
				labels, labelParams, open := labelKeys(call.Args[1], f, consts, fn, params)
				if ident, ok := call.Args[0].(*ast.Ident); ok {
					if index, isParam := params[ident.Name]; isParam {
						if wrappers[f.dir] == nil {
							wrappers[f.dir] = map[string]metricWrapper{}
						}
						wrappers[f.dir][fn.Name.Name] = metricWrapper{kind: kind, nameParam: index, labelParams: labelParams, labels: labels, open: open}
						return true
					}
				}
				name, ok := stringValue(call.Args[0], f, consts, fn)
				if !ok {
					unresolved = append(unresolved, site)
					return true
				}
				inv.add(name, kind, labels, open || len(labelParams) > 0, site)
				return true
			})
		}
	}
	for _, f := range files {
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					callee = fun.Name
				case *ast.SelectorExpr:
					callee = fun.Sel.Name
				}
				w, ok := wrappers[f.dir][callee]
				if !ok || w.nameParam >= len(call.Args) {
					return true
				}
				name, ok := stringValue(call.Args[w.nameParam], f, consts, fn)
				if !ok {
					unresolved = append(unresolved, f.path+":"+callee)
					return true
				}
				labels := append([]string(nil), w.labels...)
				open := w.open
				for _, index := range w.labelParams {
					label, ok := "", false
					if index < len(call.Args) {
						label, ok = stringValue(call.Args[index], f, consts, fn)
					}
					if !ok {
						open = true
						continue
					}
					labels = append(labels, label)
				}
				inv.add(name, w.kind, labels, open, f.path+":"+callee)
				return true
			})
		}
	}
	// 名字不是编译期可知的写入点：只有这些是已知的，新增一个会让收集不完整，要么改成常量，要么在这里说明。
	knownDynamic := map[string]string{}
	for _, site := range unresolved {
		if _, ok := knownDynamic[site]; !ok {
			t.Errorf("%s: metric name is not a literal, a constant or a wrapper parameter; the metric-name gate cannot see it", site)
		}
	}

	for _, path := range templates {
		scanTemplateMetrics(t, inv, path)
	}
	// 导出的 Metric* 常量：写入点已经收到的以写入点为准；没收到的（只经变量或接口传递）按形状未知、标签开放算存在。
	for _, c := range exported {
		if _, seen := inv.byName[c[0]]; !seen {
			inv.add(c[0], kindUnknown, nil, true, c[1])
		}
	}
	inv.index()
	return inv
}

// addSnapshotLiteral records a metrics.Metric{Name: ..., Kind: ..., Labels: ...}
// built inside the registry itself.
func addSnapshotLiteral(inv *metricInventory, lit *ast.CompositeLit, f goFile, consts map[string]map[string]string, fn *ast.FuncDecl) {
	if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "Metric" {
		return
	}
	var name string
	var labels []string
	kind, open := kindUnknown, false
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		switch key := kv.Key.(*ast.Ident).Name; key {
		case "Name":
			name, ok = stringValue(kv.Value, f, consts, fn)
			if !ok {
				return // 复制已有序列（m.Name），不是新名字
			}
		case "Kind":
			if id, ok := kv.Value.(*ast.Ident); ok {
				kind = map[string]metricKind{"KindCounter": kindCounter, "KindGauge": kindGauge, "KindTimer": kindDuration, "KindHistogram": kindHistogram}[id.Name]
			}
		case "Labels":
			labels, _, open = labelKeys(kv.Value, f, consts, fn, nil)
		}
	}
	if name != "" {
		inv.add(name, kind, labels, open, f.path+": Metric literal")
	}
}

func funcParams(fn *ast.FuncDecl) map[string]int {
	params := map[string]int{}
	index := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			index++
			continue
		}
		for _, name := range field.Names {
			params[name.Name] = index
			index++
		}
	}
	return params
}

// stringValue resolves a compile-time string: literal, package constant (own
// or imported), a + b, or a function-local variable assigned only literals.
func stringValue(expr ast.Expr, f goFile, consts map[string]map[string]string, fn *ast.FuncDecl) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			s, err := strconv.Unquote(e.Value)
			return s, err == nil
		}
	case *ast.ParenExpr:
		return stringValue(e.X, f, consts, fn)
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			left, ok1 := stringValue(e.X, f, consts, fn)
			right, ok2 := stringValue(e.Y, f, consts, fn)
			return left + right, ok1 && ok2
		}
	case *ast.Ident:
		if s, ok := consts[f.dir][e.Name]; ok {
			return s, true
		}
		if fn != nil {
			var value string
			found, ok := false, true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, isAssign := n.(*ast.AssignStmt)
				if !isAssign || len(assign.Lhs) != len(assign.Rhs) {
					return true
				}
				for i, lhs := range assign.Lhs {
					if id, isIdent := lhs.(*ast.Ident); isIdent && id.Name == e.Name {
						s, resolved := stringValue(assign.Rhs[i], f, consts, nil)
						if !resolved || found && s != value {
							ok = false
						}
						value, found = s, true
					}
				}
				return true
			})
			return value, found && ok
		}
	case *ast.SelectorExpr:
		if pkg, isIdent := e.X.(*ast.Ident); isIdent {
			if dir, imported := f.imports[pkg.Name]; imported {
				s, ok := consts[dir][e.Sel.Name]
				return s, ok
			}
		}
	}
	return "", false
}

// labelKeys reads the label keys of a labels argument. params maps the
// enclosing function's parameters, so a key that is a parameter is reported
// by index for wrapper resolution.
func labelKeys(expr ast.Expr, f goFile, consts map[string]map[string]string, fn *ast.FuncDecl, params map[string]int) (labels []string, labelParams []int, open bool) {
	fromLiteral := func(lit *ast.CompositeLit) {
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				open = true
				continue
			}
			if id, ok := kv.Key.(*ast.Ident); ok {
				if index, isParam := params[id.Name]; isParam {
					labelParams = append(labelParams, index)
					continue
				}
			}
			if key, ok := stringValue(kv.Key, f, consts, fn); ok {
				labels = append(labels, key)
			} else {
				open = true
			}
		}
	}
	switch e := expr.(type) {
	case *ast.Ident:
		if e.Name == "nil" {
			return nil, nil, false
		}
		// 局部变量：收函数内对它的字面量赋值与 x["k"] = v；有别的赋值就算开放。
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range s.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == e.Name && i < len(s.Rhs) {
						found = true
						if lit, ok := s.Rhs[i].(*ast.CompositeLit); ok {
							fromLiteral(lit)
						} else {
							open = true
						}
					}
					if index, ok := lhs.(*ast.IndexExpr); ok {
						if id, ok := index.X.(*ast.Ident); ok && id.Name == e.Name {
							if key, ok := stringValue(index.Index, f, consts, fn); ok {
								labels = append(labels, key)
							} else {
								open = true
							}
						}
					}
				}
			case *ast.ValueSpec:
				for i, name := range s.Names {
					if name.Name == e.Name {
						found = true
						if i < len(s.Values) {
							if lit, ok := s.Values[i].(*ast.CompositeLit); ok {
								fromLiteral(lit)
								continue
							}
						}
						open = true
					}
				}
			}
			return true
		})
		if !found {
			open = true
		}
	case *ast.CompositeLit:
		fromLiteral(e)
	default:
		// 包装调用（例如 r.metricLabels(metrics.Labels{...})）：里面的字面量键算数，但包装可能再加键。
		open = true
		ast.Inspect(expr, func(n ast.Node) bool {
			if lit, ok := n.(*ast.CompositeLit); ok {
				fromLiteral(lit)
				return false
			}
			return true
		})
	}
	return labels, labelParams, open
}

var (
	templateWriteCall   = regexp.MustCompile(`\b(IncCounter|SetGauge|AddGauge|ObserveDuration|ObserveHistogram)\(\s*("[^"\\]+"|[A-Za-z_]\w*)\s*,\s*(nil\b|(?:metrics\.Labels|map\[string\]string)\{([^}]*)\})?`)
	templateMetricConst = regexp.MustCompile(`\b(\w*[Mm]etric\w*)\s*=\s*"([a-z][a-z0-9_.]*)"`)
	templateLabelKey    = regexp.MustCompile(`"([^"\\]+)"\s*:`)
)

// scanTemplateMetrics reads metric writes from generator output that only
// exists as text: demo/*.tmpl and the Go code codegen renders into strings.
func scanTemplateMetrics(t *testing.T, inv *metricInventory, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	consts := map[string]string{}
	for _, m := range templateMetricConst.FindAllStringSubmatch(text, -1) {
		consts[m[1]] = m[2]
	}
	site := filepath.ToSlash(path)
	for _, m := range templateWriteCall.FindAllStringSubmatch(text, -1) {
		name := m[2]
		if strings.HasPrefix(name, `"`) {
			name = strings.Trim(name, `"`)
		} else if value, ok := consts[name]; ok {
			name = value
		} else {
			continue // 不是指标名常量（AST 那一遍已经看过真正的 Go 代码）
		}
		var labels []string
		for _, key := range templateLabelKey.FindAllStringSubmatch(m[4], -1) {
			labels = append(labels, key[1])
		}
		inv.add(name, metricWriteMethods[m[1]], labels, m[3] == "", site+" (template)")
	}
	for _, value := range consts {
		if _, seen := inv.byName[value]; !seen && strings.Contains(value, "_") {
			// 模板里声明了却没被上面的调用正则看到的指标常量：照样算存在，形状未知。
			inv.add(value, kindUnknown, nil, true, site+" (template const)")
		}
	}
}

// --- 名字与模式解析 ---

var braceGroup = regexp.MustCompile(`\{([^{}]*)\}`)

// splitLabels splits `name{a,b=c}` into the name and its label names. A brace
// group followed by more name characters is an expansion, not labels.
func splitLabels(token string) (string, []string) {
	token = strings.TrimSpace(token)
	if !strings.HasSuffix(token, "}") {
		return token, nil
	}
	open := strings.LastIndex(token, "{")
	if open < 0 {
		return token, nil
	}
	inner := token[open+1 : len(token)-1]
	name := token[:open]
	// `x_{count,sum_nanos}` is an expansion: no label lists contain only
	// export suffixes, and the name before it ends with an underscore.
	if strings.HasSuffix(name, "_") || strings.HasSuffix(name, ".") {
		return token, nil
	}
	var labels []string
	inner = strings.NewReplacer("[", "", "]", "").Replace(inner)
	for _, part := range strings.Split(inner, ",") {
		label, _, _ := strings.Cut(strings.TrimSpace(part), "=")
		label, _, _ = strings.Cut(label, "!")
		label = strings.TrimSpace(strings.TrimSuffix(label, "~"))
		if label != "" {
			labels = append(labels, label)
		}
	}
	return name, labels
}

func expandBraces(pattern string) []string {
	loc := braceGroup.FindStringSubmatchIndex(pattern)
	if loc == nil {
		return []string{pattern}
	}
	var out []string
	for _, alt := range strings.Split(pattern[loc[2]:loc[3]], ",") {
		out = append(out, expandBraces(pattern[:loc[0]]+strings.TrimSpace(alt)+pattern[loc[1]:])...)
	}
	return out
}

// resolve returns the metrics a documented name or pattern refers to: a
// source name, an exported series name, with `*` and `{a,b}` expansions.
func (inv *metricInventory) resolve(pattern string) []*metricInfo {
	seen := map[*metricInfo]bool{}
	var out []*metricInfo
	addMatch := func(info *metricInfo) {
		if !seen[info] {
			seen[info] = true
			out = append(out, info)
		}
	}
	for _, alt := range expandBraces(pattern) {
		if !strings.Contains(alt, "*") {
			if info, ok := inv.byName[alt]; ok {
				addMatch(info)
			}
			if info, ok := inv.bySeries[alt]; ok {
				addMatch(info)
			}
			continue
		}
		re := regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(alt), `\*`, `[A-Za-z0-9_.:]*`) + "$")
		for name, info := range inv.byName {
			if re.MatchString(name) {
				addMatch(info)
			}
		}
		for series, info := range inv.bySeries {
			if re.MatchString(series) {
				addMatch(info)
			}
		}
	}
	return out
}

// resolveRelative resolves a token written relative to the previous one in the
// same cell: `nest.dispatch.queue_len` / `worker_num`, `bus_dead_letter_total`
// / `_requeue_total`.
func (inv *metricInventory) resolveRelative(token, previous string) []*metricInfo {
	if found := inv.resolve(token); len(found) > 0 || previous == "" {
		return found
	}
	for i := len(previous) - 1; i > 0; i-- {
		c := previous[i]
		if c != '.' && c != '_' {
			continue
		}
		candidate := previous[:i+1] + token
		if strings.HasPrefix(token, "_") {
			candidate = previous[:i] + token
		}
		if found := inv.resolve(candidate); len(found) > 0 {
			return found
		}
	}
	return nil
}

// checkLabels reports the labels none of the metrics carries.
func checkLabels(metrics []*metricInfo, labels []string, extra ...string) []string {
	var missing []string
	for _, label := range labels {
		ok := false
		for _, e := range extra {
			ok = ok || label == e
		}
		for _, info := range metrics {
			ok = ok || info.open || info.labels[label]
		}
		if !ok {
			missing = append(missing, label)
		}
	}
	return missing
}

// --- Markdown ---

var (
	backtickToken = regexp.MustCompile("`([^`]+)`")
	strikeThrough = regexp.MustCompile(`~~[^~]*~~`)
)

// markdownSection returns the lines under the `## heading` (prefix match) up to the next `## `.
func markdownSection(t *testing.T, text, heading string) []string {
	t.Helper()
	var out []string
	in := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			in = strings.HasPrefix(line, heading)
			continue
		}
		if in {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no section %q", heading)
	}
	return out
}

// tableCells splits a Markdown table row, honouring `\|`.
func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "| ---") || strings.HasPrefix(line, "|---") {
		return nil
	}
	line = strings.ReplaceAll(line, `\|`, "\x00")
	var cells []string
	for _, cell := range strings.Split(strings.Trim(line, "|"), "|") {
		cells = append(cells, strings.ReplaceAll(strings.TrimSpace(cell), "\x00", "|"))
	}
	return cells
}

type docReference struct {
	where   string
	token   string
	metrics []*metricInfo
}

// metricCellReferences resolves every backticked token in a table cell that
// lists metrics, and reports tokens that name nothing and labels that do not exist.
func metricCellReferences(t *testing.T, inv *metricInventory, where, cell string) []docReference {
	t.Helper()
	var refs []docReference
	previous := ""
	for _, m := range backtickToken.FindAllStringSubmatch(strikeThrough.ReplaceAllString(cell, ""), -1) {
		name, labels := splitLabels(m[1])
		found := inv.resolveRelative(name, previous)
		if len(found) == 0 {
			t.Errorf("%s: `%s` names no metric written anywhere in the source", where, m[1])
			continue
		}
		if missing := checkLabels(found, labels, "job", "instance"); len(missing) > 0 {
			t.Errorf("%s: `%s` has labels %v that the metric is never written with (written at %s)", where, m[1], missing, found[0].sites[0])
		}
		if !strings.HasPrefix(name, "_") && (strings.Contains(name, ".") || strings.Contains(name, "_")) && len(inv.resolve(name)) > 0 {
			previous = name
		}
		refs = append(refs, docReference{where: where, token: m[1], metrics: found})
	}
	return refs
}

// --- PromQL ---

var (
	promTemplateVar = regexp.MustCompile(`\{\{[^}]*\}\}`)
	promSelector    = regexp.MustCompile(`([A-Za-z_:][A-Za-z0-9_:]*)?\{([^{}]*)\}`)
	promMatcher     = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(=~|!~|!=|=)`)
	promGrouping    = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\(([^()]*)\)`)
	promRange       = regexp.MustCompile(`\[[^\]]*\]`)
	promIdentifier  = regexp.MustCompile(`[A-Za-z_:][A-Za-z0-9_:]*`)
)

var promKeywords = map[string]bool{
	"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
	"bool": true, "and": true, "or": true, "unless": true, "offset": true, "inf": true, "nan": true,
}

// promQuery extracts the metric names and label names of one expression.
func promQuery(expr string) (names, labels []string) {
	expr = promTemplateVar.ReplaceAllString(expr, "X")
	for _, m := range promSelector.FindAllStringSubmatch(expr, -1) {
		for _, matcher := range promMatcher.FindAllStringSubmatch(m[2], -1) {
			labels = append(labels, matcher[1])
		}
	}
	expr = promSelector.ReplaceAllString(expr, "$1")
	for _, m := range promGrouping.FindAllStringSubmatch(expr, -1) {
		for _, label := range strings.Split(m[2], ",") {
			if label = strings.TrimSpace(label); label != "" {
				labels = append(labels, label)
			}
		}
	}
	expr = promGrouping.ReplaceAllString(expr, " ")
	expr = promRange.ReplaceAllString(expr, " ")
	for _, loc := range promIdentifier.FindAllStringIndex(expr, -1) {
		word := expr[loc[0]:loc[1]]
		if loc[0] > 0 && (expr[loc[0]-1] >= '0' && expr[loc[0]-1] <= '9' || expr[loc[0]-1] == '.') {
			continue // 1e6 的 e6
		}
		rest := strings.TrimLeft(expr[loc[1]:], " ")
		if promKeywords[word] || strings.HasPrefix(rest, "(") {
			continue
		}
		names = append(names, word)
	}
	return names, labels
}

func dashboardExprs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(promTemplateVar.ReplaceAll(raw, []byte("X")), &doc); err != nil {
		t.Fatalf("%s is not JSON (template placeholders replaced): %v", path, err)
	}
	var exprs []string
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, child := range v {
				if s, ok := child.(string); ok && key == "expr" {
					exprs = append(exprs, s)
				}
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	if len(exprs) == 0 {
		t.Fatalf("%s has no expr", path)
	}
	return exprs
}

func readDashboardExprsRaw(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 模板里的占位符在 JSON 字符串里，原文取 expr 以保留 {{...}}：promQuery 自己替换。
	var exprs []string
	for _, m := range regexp.MustCompile(`"expr":\s*"((?:[^"\\]|\\.)*)"`).FindAllStringSubmatch(string(raw), -1) {
		s, err := strconv.Unquote(`"` + m[1] + `"`)
		if err != nil {
			t.Fatalf("%s: expr %q: %v", path, m[1], err)
		}
		exprs = append(exprs, s)
	}
	return exprs
}

const (
	observabilityDoc     = "OBSERVABILITY.md"
	overviewDashboard    = "observability/grafana-roost-overview.json"
	demoDashboard        = "demo/deploy/dev/observability/grafana/dashboards/roost-demo.json.tmpl"
	demoObservabilityDoc = "demo/deploy/dev/observability/README.md.tmpl"
)

// documentedMetrics is every metric OBSERVABILITY.md's metric list names, with
// the reference that names it.
func documentedMetrics(t *testing.T, inv *metricInventory) map[*metricInfo]bool {
	t.Helper()
	raw, err := os.ReadFile(observabilityDoc)
	if err != nil {
		t.Fatal(err)
	}
	documented := map[*metricInfo]bool{}
	for i, line := range markdownSection(t, string(raw), "## 指标清单") {
		cells := tableCells(line)
		if len(cells) > 0 {
			for _, ref := range metricCellReferences(t, inv, observabilityDoc+" 指标清单第 "+strconv.Itoa(i+1)+" 行", cells[0]) {
				for _, info := range ref.metrics {
					documented[info] = true
				}
			}
			continue
		}
		// 表外正文里点名的指标（例如 nest.stage.duration）也算收录；正文里别的反引号内容不是指标。
		for _, m := range backtickToken.FindAllStringSubmatch(strikeThrough.ReplaceAllString(line, ""), -1) {
			name, _ := splitLabels(m[1])
			for _, info := range inv.resolve(name) {
				documented[info] = true
			}
		}
	}
	return documented
}

func TestObservabilityDocNamesExistingMetrics(t *testing.T) {
	inv := collectMetricInventory(t)
	documented := documentedMetrics(t, inv)

	// 告警基线：每条的第一个反引号是告警的指标表达式；它必须存在，且在指标清单里。
	raw, err := os.ReadFile(observabilityDoc)
	if err != nil {
		t.Fatal(err)
	}
	alerts := 0
	for _, line := range markdownSection(t, string(raw), "## 告警基线建议") {
		line = strikeThrough.ReplaceAllString(line, "")
		m := backtickToken.FindStringSubmatch(line)
		if m == nil || !regexp.MustCompile(`^\d+\. `).MatchString(line) || strings.HasPrefix(m[1], "/") {
			continue
		}
		alerts++
		expr, _, _ := strings.Cut(m[1], " ")
		name, labels := splitLabels(expr)
		{
			found := inv.resolve(name)
			if len(found) == 0 {
				t.Errorf("%s 告警基线: `%s` names no metric written anywhere in the source", observabilityDoc, m[1])
				continue
			}
			if missing := checkLabels(found, labels); len(missing) > 0 {
				t.Errorf("%s 告警基线: `%s` uses labels %v the metric is never written with", observabilityDoc, m[1], missing)
			}
			for _, info := range found {
				if !documented[info] {
					t.Errorf("%s 告警基线: `%s` alerts on %s, which the metric list (## 指标清单) does not document", observabilityDoc, m[1], info.name)
				}
			}
		}
	}
	if alerts < 5 {
		t.Fatalf("found only %d alert lines in %s; the section moved, update this test", alerts, observabilityDoc)
	}

	// F11 N9：全部可解析的指标必须收录，防止新增观测又落在文档之外。
	var undocumented []string
	for name, info := range inv.byName {
		if !documented[info] {
			undocumented = append(undocumented, name+" ("+info.sites[0]+")")
		}
	}
	sort.Strings(undocumented)
	for _, name := range undocumented {
		t.Errorf("%s does not document %s; every collected metric belongs in its metric list", observabilityDoc, name)
	}
}

func TestDashboardsQueryExistingMetricsAndLabels(t *testing.T) {
	inv := collectMetricInventory(t)
	documented := documentedMetrics(t, inv)
	demoDoc, err := os.ReadFile(demoObservabilityDoc)
	if err != nil {
		t.Fatal(err)
	}
	for _, dashboard := range []string{overviewDashboard, demoDashboard} {
		_ = dashboardExprs(t, dashboard) // 整个文件是合法 JSON
		for _, expr := range readDashboardExprsRaw(t, dashboard) {
			names, labels := promQuery(expr)
			if len(names) == 0 {
				t.Errorf("%s: no metric in %q", dashboard, expr)
			}
			var found []*metricInfo
			for _, name := range names {
				info, ok := inv.bySeries[name]
				if !ok {
					t.Errorf("%s: %q queries %s, which no source writes (exported series of the metrics registry)", dashboard, expr, name)
					continue
				}
				found = append(found, info)
				// 仪表盘上的指标要有文档：框架指标在 OBSERVABILITY.md，生成工程自己的指标（只在模板里写）在 demo README。
				generated := true
				for _, site := range info.sites {
					generated = generated && strings.HasSuffix(site, "(template)")
				}
				switch {
				case generated && !strings.Contains(string(demoDoc), name) && !strings.Contains(string(demoDoc), promName(info.name)):
					t.Errorf("%s: %s is a generated-project metric the demo README (%s) does not mention", dashboard, name, demoObservabilityDoc)
				case !generated && !documented[info]:
					t.Errorf("%s: %s (%s) is on a dashboard but %s does not document it", dashboard, name, info.name, observabilityDoc)
				}
			}
			extra := []string{"job", "instance"}
			for _, name := range names {
				if strings.HasSuffix(name, "_bucket") {
					extra = append(extra, "le")
				}
			}
			if missing := checkLabels(found, labels, extra...); len(found) > 0 && len(missing) > 0 {
				sites := []string{}
				for _, info := range found {
					sites = append(sites, info.sites[0])
				}
				t.Errorf("%s: %q uses labels %v that %v are never written with (written at %v)", dashboard, expr, missing, names, sites)
			}
		}
	}
}

func TestDemoObservabilityReadmeNamesExistingMetrics(t *testing.T) {
	inv := collectMetricInventory(t)
	raw, err := os.ReadFile(demoObservabilityDoc)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for i, line := range markdownSection(t, string(raw), "## 指标 ↔ 链路") {
		cells := tableCells(line)
		if len(cells) < 2 || cells[1] == "指标（Prometheus 名）" {
			continue
		}
		rows++
		metricCellReferences(t, inv, demoObservabilityDoc+" 指标表第 "+strconv.Itoa(i+1)+" 行", cells[1])
	}
	if rows < 5 {
		t.Fatalf("found only %d metric rows in %s; the table moved, update this test", rows, demoObservabilityDoc)
	}
}

// 解析器自身：几种写法各给一例，保证上面的门禁不是因为解析不出东西而空转。
func TestMetricGateParsers(t *testing.T) {
	names, labels := promQuery(`sum by (subject)(rate(nats_jetstream_terminal_total{job="{{GAME_SERVICE}}"}[1m])) / 1e6`)
	if strings.Join(names, ",") != "nats_jetstream_terminal_total" || strings.Join(labels, ",") != "job,subject" {
		t.Errorf("promQuery: names %v labels %v", names, labels)
	}
	names, _ = promQuery(`histogram_quantile(0.95, sum by (le, msg)(rate(robot_session_call_bucket[1m])))`)
	if strings.Join(names, ",") != "robot_session_call_bucket" {
		t.Errorf("promQuery: names %v", names)
	}
	if name, labels := splitLabels("service.depth{service,name[,key]}"); name != "service.depth" || strings.Join(labels, ",") != "service,name,key" {
		t.Errorf("splitLabels: %q %v", name, labels)
	}
	if name, labels := splitLabels("player_tcp_dispatch_duration_{count,sum_nanos,max_nanos}"); labels != nil || name != "player_tcp_dispatch_duration_{count,sum_nanos,max_nanos}" {
		t.Errorf("splitLabels expansion: %q %v", name, labels)
	}
	if got := expandBraces("a.{x,y}_total"); strings.Join(got, ",") != "a.x_total,a.y_total" {
		t.Errorf("expandBraces: %v", got)
	}
	inv := &metricInventory{byName: map[string]*metricInfo{}}
	inv.add("bus_dead_letter_requeue_total", kindCounter, nil, false, "x")
	inv.add("nest.dispatch.worker_num", kindGauge, nil, false, "x")
	inv.index()
	if len(inv.resolveRelative("_requeue_total", "bus_dead_letter_total")) != 1 || len(inv.resolveRelative("worker_num", "nest.dispatch.queue_len")) != 1 {
		t.Error("resolveRelative does not resolve the documented relative forms")
	}
	if len(inv.resolve("entitysync_flush_gate_deferred_total")) != 0 {
		t.Error("resolve found a metric that was never added")
	}
}
