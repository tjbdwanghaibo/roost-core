package roost

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Generated shutdown window (RR-20260926-66).
//
// The App stops a service in one shutdown.total_timeout window: Service.Shutdown
// first, then the service's own Mods and the shared Mods in reverse order, each
// with a deadline planned from what is left (app.modStopBudget). A Mod without
// a declared stop budget keeps a fixed floor (app.undeclaredModStopFloor, 3s);
// a Mod that declares one (dataengine: dataengine.shutdown_timeout) is granted
// it from the rest, and is scaled down with a warning only when the rest cannot
// cover it. One value for every service cannot fit that: the game-demo game
// service registers 23 Mods and needs 30s + 3s x 22, a framework service
// registers 6 and needs 3s x 6. So the generator computes the window per
// service from the Mods its bootstrap registers:
//
//	total_timeout = declared budgets + generatedModStopFloor x undeclared Mods + generatedShutdownMargin
//	grace period  = total_timeout + generatedGraceOverTotal
//
// and writes the same numbers into the service's configs and every deployment
// template (k8s, compose, systemd, the dev scripts).
const (
	// generatedModStopFloor mirrors app.undeclaredModStopFloor.
	generatedModStopFloor = 3 * time.Second
	// generatedDataEngineShutdownTimeout is dataengine.shutdown_timeout as the
	// generated config writes it (catalog.go): the stop budget kit/dataengine
	// declares to the App.
	generatedDataEngineShutdownTimeout = 30 * time.Second
	// generatedShutdownMargin is for Service.Shutdown, which runs inside the
	// same window before any Mod stops (the App starts the clock before it).
	// Generated services only close in-process loops there — a hosted framework
	// Server returns at once — so 5s is an allowance, the same one the
	// REPRO-2026-09-26-06 §2 probe spends. The Mods themselves need no extra
	// slack: the App replans from time.Until(deadline) before every Mod, and
	// while the service's own Mods stop it does not reserve the later shared
	// Mods' floors, so the first Mods get more than their floor (3.47s in the
	// game service even when Service.Shutdown used the whole margin).
	generatedShutdownMargin = 5 * time.Second
	// generatedGraceOverTotal is how much longer than total_timeout the
	// platform waits after SIGTERM before SIGKILL: the App returns at
	// total_timeout at the latest, and the process still has to exit.
	generatedGraceOverTotal = 5 * time.Second
	// appDefaultShutdownTotal is what the App uses when a config has no
	// shutdown.total_timeout (app.App.Execute).
	appDefaultShutdownTotal = 30 * time.Second
)

// serviceShutdown is the stop plan of one service's process as generated.
type serviceShutdown struct {
	// mods is every Mod the process registers: the shared ones and the service's own.
	mods int
	// declaring is how many of them declare a stop budget; declared is the sum.
	declaring int
	declared  time.Duration
	margin    time.Duration
	total     time.Duration
	grace     time.Duration
}

func (plan serviceShutdown) undeclared() int { return plan.mods - plan.declaring }

// serviceShutdownPlan counts what the bootstrap registers for the service
// (renderBootstrap: the shared a.Mods list, then serviceModConstructors) and
// applies the formula. Only dataengine declares a stop budget among the Mods
// the generator wires; every other Kit, framework, rpc and access Mod gets the
// floor.
func serviceShutdownPlan(m Manifest, service string) serviceShutdown {
	shared, _ := resolveMods(m.SharedMods)
	own, _ := resolveMods(effectiveServiceMods(m, service))
	plan := serviceShutdown{mods: len(shared) + len(serviceModConstructors(m, service, allProjectMods(m))), margin: generatedShutdownMargin}
	for _, mod := range append(append([]string(nil), shared...), own...) {
		if mod == "dataengine" {
			plan.declaring++
			plan.declared += generatedDataEngineShutdownTimeout
		}
	}
	plan.total = plan.declared + time.Duration(plan.undeclared())*generatedModStopFloor + plan.margin
	plan.grace = plan.total + generatedGraceOverTotal
	return plan
}

// seconds renders a whole-second duration the way the generated YAML and
// shell files write it ("101s"): time.Duration.String would say "1m41s".
func seconds(value time.Duration) string {
	return fmt.Sprintf("%ds", int64(value/time.Second))
}

// renderShutdownConfig is the shutdown: block of a service's config. Its
// summary line is also how refreshGeneratedShutdownConfigs recognises a block
// nobody has edited (parseGeneratedShutdown).
func renderShutdownConfig(plan serviceShutdown) string {
	return fmt.Sprintf("shutdown:\n"+
		"  # Whole shutdown window: Service.Shutdown, then every Mod in reverse order. Every Mod without\n"+
		"  # a declared stop budget keeps a fixed 3s floor; a Mod that declares one\n"+
		"  # (dataengine.shutdown_timeout) is granted it from the rest, scaled down with a warning only\n"+
		"  # when the rest cannot cover it. Generated for this service's %d Mods: %s declared + 3s x %d\n"+
		"  # undeclared + %s for Service.Shutdown = %s. roost sync updates this block while it is\n"+
		"  # unedited when the service's Mods change; roost doctor checks an edited one.\n"+
		"  # The deployment's termination grace period (k8s terminationGracePeriodSeconds, compose\n"+
		"  # stop_grace_period, systemd TimeoutStopSec, deploy/dev/run.sh) must be\n"+
		"  # >= total_timeout + 5s: %s for this service.\n"+
		"  total_timeout: %s\n  serve_wait_timeout: 5s\n",
		plan.mods, seconds(plan.declared), plan.undeclared(), seconds(plan.margin), seconds(plan.total),
		seconds(plan.grace), seconds(plan.total))
}

var generatedShutdownSummary = regexp.MustCompile(`Generated for this service's (\d+) Mods: (\d+)s declared \+ 3s x (\d+)\n *# undeclared \+ (\d+)s for Service\.Shutdown = (\d+)s\.`)

// parseGeneratedShutdown reads back the plan a generated shutdown: block was
// rendered from.
func parseGeneratedShutdown(body string) (serviceShutdown, bool) {
	match := generatedShutdownSummary.FindStringSubmatch(body)
	if match == nil {
		return serviceShutdown{}, false
	}
	number := func(index int) int {
		value, _ := strconv.Atoi(match[index])
		return value
	}
	plan := serviceShutdown{
		mods:     number(1),
		declared: time.Duration(number(2)) * time.Second,
		margin:   time.Duration(number(4)) * time.Second,
		total:    time.Duration(number(5)) * time.Second,
	}
	plan.declaring = plan.mods - number(3)
	plan.grace = plan.total + generatedGraceOverTotal
	return plan, true
}

// shutdownConfigTargets are the files a service's shutdown: block is written
// into, with the indentation it has there.
func shutdownConfigTargets(service string) []struct{ rel, indent string } {
	return []struct{ rel, indent string }{
		{"configs/service/config." + service + ".yaml", ""},
		{"configs/service/config." + service + ".prod.example.yaml", ""},
		{"deploy/k8s/base/secret." + service + ".example.yaml", "    "},
	}
}

// refreshGeneratedShutdownConfigs keeps a service's generated shutdown: block
// in step with its Mods. The configs are rendered once and application-owned
// (renderServiceConfig), but the deployment templates are regenerated on every
// sync from the current Mod set; a Mod added later (add mod, add saga, add
// access, a new uses) would otherwise leave total_timeout below what the App
// needs while the grace period grows. The game-demo itself is built that way:
// its configs are written with 20 Mods, the scaffold then adds saga and the
// player access Mods.
//
// A block is rewritten only when it is exactly what the generator wrote for
// the plan its own summary names — any hand edit, and any block from a
// generator before RR-20260926-66, is left alone (roost doctor reports those).
func refreshGeneratedShutdownConfigs(root string, m Manifest) ([]string, error) {
	var changed []string
	for _, service := range sortedServiceNames(m) {
		plan := serviceShutdownPlan(m, service)
		for _, target := range shutdownConfigTargets(service) {
			path := filepath.Join(root, filepath.FromSlash(target.rel))
			raw, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return changed, err
			}
			body := string(raw)
			written, ok := parseGeneratedShutdown(body)
			if !ok || written == plan {
				continue
			}
			before := indentText(renderShutdownConfig(written), target.indent)
			if strings.Count(body, before) != 1 {
				continue
			}
			body = strings.Replace(body, before, indentText(renderShutdownConfig(plan), target.indent), 1)
			if err := writeAtomic(path, []byte(body), 0o644); err != nil {
				return changed, err
			}
			changed = append(changed, target.rel)
		}
	}
	return changed, nil
}

// checkShutdownBudgets is the doctor's view of the same numbers for a config
// the generator no longer maintains (edited, or from before RR-20260926-66):
// total_timeout has to cover the Mods' floors and the declared budget, and the
// generated grace period has to outlast it, or the platform kills the process
// while the App is still stopping Mods.
func checkShutdownBudgets(root string, m Manifest) []CheckItem {
	var items []CheckItem
	for _, service := range sortedServiceNames(m) {
		rel := "configs/service/config." + service + ".yaml"
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue // the config check already reports a missing file
		}
		var config struct {
			Shutdown struct {
				TotalTimeout string `yaml:"total_timeout"`
			} `yaml:"shutdown"`
			DataEngine struct {
				ShutdownTimeout string `yaml:"shutdown_timeout"`
			} `yaml:"dataengine"`
		}
		name := "shutdown:" + service
		if err := yaml.Unmarshal(raw, &config); err != nil {
			continue // reported by the config check
		}
		plan := serviceShutdownPlan(m, service)
		total, err := parseConfigDuration(config.Shutdown.TotalTimeout, appDefaultShutdownTotal)
		if err != nil {
			items = append(items, CheckItem{Name: name, Status: StatusFail, Detail: fmt.Sprintf("%s: shutdown.total_timeout: %v", rel, err)})
			continue
		}
		declared := plan.declared
		if plan.declaring > 0 {
			value, err := parseConfigDuration(config.DataEngine.ShutdownTimeout, generatedDataEngineShutdownTimeout)
			if err != nil {
				items = append(items, CheckItem{Name: name, Status: StatusFail, Detail: fmt.Sprintf("%s: dataengine.shutdown_timeout: %v", rel, err)})
				continue
			}
			declared = value * time.Duration(plan.declaring)
		}
		need := declared + time.Duration(plan.undeclared())*generatedModStopFloor
		switch {
		case total+generatedGraceOverTotal > plan.grace:
			items = append(items, CheckItem{Name: name, Status: StatusWarn, Detail: fmt.Sprintf(
				"%s: total_timeout %s + 5s exceeds the generated deployment grace period %s; the platform would kill the process while Mods are still stopping. Lower total_timeout to %s or keep it and raise the grace period after every sync",
				rel, seconds(total), seconds(plan.grace), seconds(plan.total))})
		case total < need:
			items = append(items, CheckItem{Name: name, Status: StatusWarn, Detail: fmt.Sprintf(
				"%s: total_timeout %s cannot cover %d Mods (%s declared + 3s x %d = %s); every stop warns and budgets are cut. Set it to %s",
				rel, seconds(total), plan.mods, seconds(declared), plan.undeclared(), seconds(need), seconds(plan.total))})
		default:
			items = append(items, CheckItem{Name: name, Status: StatusOK, Detail: fmt.Sprintf("total_timeout %s, grace period %s", seconds(total), seconds(plan.grace))})
		}
	}
	return items
}

func parseConfigDuration(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	return time.ParseDuration(strings.TrimSpace(value))
}
