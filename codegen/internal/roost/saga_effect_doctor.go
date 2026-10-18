package roost

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// checkStepAndEffectWiring 是 doctor 的 saga-steps:<saga> 与 effect-consumer:<service>/<func> 两项（只读，WARN 不让
// doctor 失败）：add saga 生成的 steps.go 与 add effect 生成的消费者都要业务在服务 Init 里启动、Shutdown 里排空，生成器
// 不改写 Init。忘了启动时进程照常起来，步骤命令却一直等到超时、effect 一直堆在流里，没有任何报错，所以在这里提示。
//
// 判据是文本：工程（不含测试文件）里有没有导入该 saga 包并调用 .StartSteps( / .RegisterSteps( 的文件、启动它的包里
// 有没有 Drain(；消费者函数 start<Topic>EffectConsumer 在本服务包里有没有被调用。只看“写了没有”，不证明运行时一定
// 走到那一行；steps.go 里没有 StartSteps / RegisterSteps（例如 game-demo 手写接线）时不报。
func checkStepAndEffectWiring(root string, m Manifest) []CheckItem {
	sources, err := projectGoSources(root)
	if err != nil {
		return []CheckItem{{Name: "saga-steps", Status: StatusWarn, Detail: "scan project sources: " + err.Error()}}
	}
	var items []CheckItem
	for _, name := range m.Sagas {
		stepsRel := "saga/" + name + "/steps.go"
		code, ok := sources[stepsRel]
		if !ok || (!strings.Contains(code, "func StartSteps(") && !strings.Contains(code, "func RegisterSteps(")) {
			continue
		}
		item := CheckItem{Name: "saga-steps:" + name}
		importPath := `"` + m.Project.Module + "/saga/" + name + `"`
		starter := ""
		for _, rel := range sortedKeys(sources) {
			body := sources[rel]
			if strings.HasPrefix(rel, "saga/"+name+"/") || !strings.Contains(body, importPath) {
				continue
			}
			if strings.Contains(body, ".StartSteps(") || strings.Contains(body, ".RegisterSteps(") {
				starter = rel
				break
			}
		}
		switch {
		case starter == "":
			item.Status = StatusWarn
			item.Detail = stepsRel + " registers the step consumers but no service starts them, so every step command waits until it times out; " +
				"in the service Init: steps, err := " + name + ".StartSteps(context.Background(), registry, " + name + ".StepDeps{}); in Shutdown: steps.Drain(ctx)"
		case !packageMatches(sources, starter, stepsDrainCall):
			item.Status = StatusWarn
			item.Detail = "started in " + starter + " but nothing in its package calls Drain: a stop abandons the steps it is running; in Shutdown: if err := steps.Drain(ctx); err != nil { steps.Stop() }"
		default:
			item.Status = StatusOK
			item.Detail = "step consumers started in " + starter
		}
		items = append(items, item)
	}
	declaration := regexp.MustCompile(`(?m)^func (start\w+EffectConsumer)\(`)
	for _, service := range sortedServiceNames(m) {
		prefix := "internal/service/" + service + "/"
		for _, rel := range sortedKeys(sources) {
			if !strings.HasPrefix(rel, prefix) || strings.Contains(strings.TrimPrefix(rel, prefix), "/") {
				continue
			}
			for _, match := range declaration.FindAllStringSubmatch(sources[rel], -1) {
				function := match[1]
				calls := 0
				for other, body := range sources {
					if strings.HasPrefix(other, prefix) && !strings.Contains(strings.TrimPrefix(other, prefix), "/") {
						calls += strings.Count(body, function+"(")
					}
				}
				item := CheckItem{Name: "effect-consumer:" + service + "/" + function, Status: StatusOK, Detail: "called in the service package (" + rel + ")"}
				if calls <= 1 { // 只有声明本身
					item.Status = StatusWarn
					item.Detail = rel + " declares the consumer but the service never starts it: the effects stay on the stream (until dataengine.effects.max_age) unhandled; " +
						"in the service Init: subscription, err := " + function + "(context.Background(), registry); in Shutdown: subscription.Drain()"
				}
				items = append(items, item)
			}
		}
	}
	return items
}

// projectGoSources 读工程里全部非测试 Go 文件（键是相对路径，斜杠分隔；注释已去掉），跳过隐藏目录与 vendor。
func projectGoSources(root string) (map[string]string, error) {
	sources := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
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
		sources[filepath.ToSlash(rel)] = stripGoComments(string(raw))
		return nil
	})
	return sources, err
}

// stepsDrainCall 是 Steps.Drain(ctx) 的调用（带参数）；JetStream 订阅的 Drain() 不带参数，不算。
var stepsDrainCall = regexp.MustCompile(`\.Drain\(\s*[^)\s]`)

// packageMatches 报告 rel 所在目录（同一个包）的非测试文件里有没有匹配 pattern 的代码（注释已去掉）。
func packageMatches(sources map[string]string, rel string, pattern *regexp.Regexp) bool {
	dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
	for other, body := range sources {
		if filepath.ToSlash(filepath.Dir(filepath.FromSlash(other))) == dir && pattern.MatchString(body) {
			return true
		}
	}
	return false
}

// stripGoComments 把注释换成等长空白：骨架注释里写着 StartSteps / Drain 的用法，不能当作调用。
func stripGoComments(source string) string {
	out := []byte(source)
	fileSet := token.NewFileSet()
	file := fileSet.AddFile("", fileSet.Base(), len(out))
	var s scanner.Scanner
	s.Init(file, out, nil, scanner.ScanComments)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.COMMENT {
			continue
		}
		start := file.Offset(pos)
		for i := start; i < start+len(lit) && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	return string(out)
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
