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
//	grace period  = max(total_timeout, the longest total the service's configs set) + generatedGraceOverTotal
//
// and writes the same numbers into the service's configs and every deployment
// template (k8s, compose, systemd, the dev scripts). The grace period never
// goes below what the configs actually set (RR-20260926-66 复核): configs are
// application-owned and may hold an edited or older total.
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
	// total is the formula's shutdown.total_timeout.
	total time.Duration
	// grace is what the deployment templates wait after SIGTERM:
	// max(total, the longest total the project's configs actually set) + 5s.
	grace time.Duration
}

func (plan serviceShutdown) undeclared() int { return plan.mods - plan.declaring }

// sameFormula reports whether two plans would render the same shutdown: block
// (grace is not part of it: the block states total_timeout + 5s).
func (plan serviceShutdown) sameFormula(other serviceShutdown) bool {
	return plan.mods == other.mods && plan.declaring == other.declaring && plan.declared == other.declared &&
		plan.margin == other.margin && plan.total == other.total
}

// serviceShutdownPlan counts what the bootstrap registers for the service
// (renderBootstrap: the shared a.Mods list, then serviceModConstructors) and
// applies the formula. Only dataengine declares a stop budget among the Mods
// the generator wires; every other Kit, framework, rpc and access Mod gets the
// floor.
//
// The grace period never goes below the total the service's configs actually
// set (m.configuredShutdown, see withConfiguredShutdown): configs are
// application-owned, so a hand-edited total, a block from a generator before
// RR-20260926-66 (60s for every service) or a missing key (the App's 30s
// fallback) stays as it is while the templates are regenerated on every sync.
// A grace period computed from the formula alone would then be shorter than
// the window the App waits for, and the platform would SIGKILL the process
// halfway through stopping its Mods.
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
	plan.grace = max(plan.total, m.configuredShutdown[service]) + generatedGraceOverTotal
	return plan
}

// withConfiguredShutdown returns m carrying, per service, the longest
// shutdown.total_timeout the project's configs set (configuredShutdownTotal),
// for rendering into the project at root. A service whose configs do not exist
// yet (being created in this render) gets none: its config is the one being
// written, with the formula's total.
func (m Manifest) withConfiguredShutdown(root string) Manifest {
	configured := make(map[string]time.Duration, len(m.Services))
	for _, service := range sortedServiceNames(m) {
		if total, _, ok := configuredShutdownTotal(root, service); ok {
			configured[service] = total
		}
	}
	m.configuredShutdown = configured
	return m
}

// configuredShutdownTotal is the longest shutdown.total_timeout the service's
// configs in the project set — the dev config, the production example and the
// k8s secret example (a real production config outside the repository cannot
// be seen; docs say so) — as the App would read it: a missing key, or a value
// the App cannot parse as a duration, is the App's 30s fallback. It also
// returns which file set it; ok is false when none of the files exists.
func configuredShutdownTotal(root, service string) (time.Duration, string, bool) {
	var longest time.Duration
	var from string
	found := false
	for _, target := range shutdownConfigTargets(service) {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(target.rel)))
		if err != nil {
			continue
		}
		total := configTotalTimeout(raw, target.indent != "")
		if !found || total > longest {
			longest, from = total, target.rel
		}
		found = true
	}
	return longest, from, found
}

// configTotalTimeout reads shutdown.total_timeout from a service config, or
// from the config.yaml a k8s Secret example carries.
func configTotalTimeout(raw []byte, secret bool) time.Duration {
	if secret {
		var document struct {
			StringData map[string]string `yaml:"stringData"`
		}
		if yaml.Unmarshal(raw, &document) != nil {
			return appDefaultShutdownTotal
		}
		raw = []byte(document.StringData["config.yaml"])
	}
	var config struct {
		Shutdown struct {
			TotalTimeout string `yaml:"total_timeout"`
		} `yaml:"shutdown"`
	}
	if yaml.Unmarshal(raw, &config) != nil {
		return appDefaultShutdownTotal
	}
	total, err := time.ParseDuration(strings.TrimSpace(config.Shutdown.TotalTimeout))
	if err != nil || total <= 0 {
		return appDefaultShutdownTotal
	}
	return total
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
		seconds(plan.total+generatedGraceOverTotal), seconds(plan.total))
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
//
// SyncProject calls it on its staging tree before rendering, so the
// templates are rendered from the refreshed totals and the returned files
// are committed with them (RR-20260926-80).
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
			if !ok || written.sameFormula(plan) {
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

// deployedGrace is one grace period a deployment template on disk sets.
type deployedGrace struct {
	rel   string
	grace time.Duration
}

var (
	k8sGracePattern      = regexp.MustCompile(`terminationGracePeriodSeconds: (\d+)`)
	composeGracePattern  = regexp.MustCompile(`stop_grace_period: (\d+)s`)
	legacySystemdPattern = regexp.MustCompile(`\nTimeoutStopSec=(\d+)s\n`)
	devStopWaitPattern   = regexp.MustCompile(`kill -0 "\$pid" 2>/dev/null && \[ "\$i" -lt (\d+) \]`)
	nextComposeService   = regexp.MustCompile(`\n  [a-z0-9_-]+:\n`)
)

// deployedGracePeriods reads the service's grace period out of every
// deployment template present on disk — as generated now, or by a generator
// before RR-20260926-66 (one value for every service) — so doctor can compare
// what the platform will wait with what the App will take.
func deployedGracePeriods(root string, m Manifest, service string) []deployedGrace {
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return ""
		}
		return string(raw)
	}
	var out []deployedGrace
	add := func(rel string, match []string) {
		if match == nil {
			return
		}
		value, err := strconv.Atoi(match[1])
		if err == nil {
			out = append(out, deployedGrace{rel: rel, grace: time.Duration(value) * time.Second})
		}
	}
	rel := "deploy/k8s/base/" + service + ".yaml"
	add(rel, k8sGracePattern.FindStringSubmatch(read(rel)))
	rel = "deploy/docker/docker-compose.prod.yaml"
	if compose := read(rel); strings.Contains(compose, "\n  "+service+":\n") {
		block := compose[strings.Index(compose, "\n  "+service+":\n")+1:]
		if next := nextComposeService.FindStringIndex(block); next != nil {
			block = block[:next[0]]
		}
		add(rel, composeGracePattern.FindStringSubmatch(block))
	}
	rel = "deploy/shell/install.sh"
	install := read(rel)
	if match := regexp.MustCompile(`\n  ` + regexp.QuoteMeta(service) + `\) STOP_TIMEOUT=(\d+)s ;;`).FindStringSubmatch(install); match != nil {
		add(rel, match)
	} else {
		add(rel, legacySystemdPattern.FindStringSubmatch(install))
	}
	rel = "deploy/dev/run.sh"
	run := read(rel)
	if match := regexp.MustCompile(`[" ]` + regexp.QuoteMeta(service) + `:\d+:(\d+)[" ]`).FindStringSubmatch(run); match != nil {
		add(rel, match)
	} else {
		add(rel, devStopWaitPattern.FindStringSubmatch(run))
	}
	if service == firstBusinessService(m) {
		rel = "deploy/dev/second-game.sh"
		add(rel, devStopWaitPattern.FindStringSubmatch(read(rel)))
	}
	return out
}

// checkShutdownBudgets is the doctor's view of the same numbers. A deployment
// template whose grace period is shorter than the longest configured
// total_timeout + 5s is a FAIL: the platform would SIGKILL the process while
// the App is still stopping Mods (templates not synced since a config was
// raised, or edited by hand). A total that cannot cover the Mods' floors and
// the declared budget is a WARN: every stop warns and budgets are cut.
//
// The WARN judges each of the service's configs in the repository on its own
// — the dev config, the production example and the k8s secret example — and
// names every one that falls short (RR-20260926-80): a deployment starts
// from whichever of them it copies, and reading only config.<svc>.yaml said
// OK for a project whose examples still carried the old 60s. The OK / WARN
// line reports the grace period the deployment templates on disk set, which
// is what the platform waits; it used to print the plan the generator would
// render now, which differs from the disk until the templates are synced.
func checkShutdownBudgets(root string, m Manifest) []CheckItem {
	var items []CheckItem
	for _, service := range sortedServiceNames(m) {
		name := "shutdown:" + service
		deployed := deployedGracePeriods(root, m, service)
		if configured, from, ok := configuredShutdownTotal(root, service); ok {
			var short []string
			for _, template := range deployed {
				if template.grace < configured+generatedGraceOverTotal {
					short = append(short, fmt.Sprintf("%s grace period %s < total_timeout %s + 5s", template.rel, seconds(template.grace), seconds(configured)))
				}
			}
			if len(short) > 0 {
				items = append(items, CheckItem{Name: name, Status: StatusFail, Detail: fmt.Sprintf(
					"%s (%s); the platform would SIGKILL the process while the App is still stopping Mods. Run roost project sync: the templates' grace period follows the longest configured total_timeout + 5s",
					strings.Join(short, "; "), from)})
				continue
			}
		}
		plan := serviceShutdownPlan(m, service)
		var covered []configuredTotal
		var shortfalls []string
		failed := false
		for index, target := range shutdownConfigTargets(service) {
			// The dev config comes first and keeps its checks as before: a
			// missing or unparsable file is the config check's to report, an
			// invalid duration in it is a FAIL. The examples are deployed as
			// copies, so a problem in one is a WARN naming that file.
			dev := index == 0
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(target.rel)))
			if err != nil {
				if dev {
					break // the config check already reports a missing file
				}
				continue // this example is not in the repository
			}
			settings, err := readShutdownSettings(raw, target.indent != "")
			if err != nil {
				if dev {
					break // reported by the config check
				}
				shortfalls = append(shortfalls, fmt.Sprintf("%s: %v", target.rel, err))
				continue
			}
			total, err := parseConfigDuration(settings.Shutdown.TotalTimeout, appDefaultShutdownTotal)
			if err != nil {
				if dev {
					items = append(items, CheckItem{Name: name, Status: StatusFail, Detail: fmt.Sprintf("%s: shutdown.total_timeout: %v", target.rel, err)})
					failed = true
					break
				}
				shortfalls = append(shortfalls, fmt.Sprintf("%s: shutdown.total_timeout: %v", target.rel, err))
				continue
			}
			declared := plan.declared
			if plan.declaring > 0 {
				value, err := parseConfigDuration(settings.DataEngine.ShutdownTimeout, generatedDataEngineShutdownTimeout)
				if err != nil {
					if dev {
						items = append(items, CheckItem{Name: name, Status: StatusFail, Detail: fmt.Sprintf("%s: dataengine.shutdown_timeout: %v", target.rel, err)})
						failed = true
						break
					}
					shortfalls = append(shortfalls, fmt.Sprintf("%s: dataengine.shutdown_timeout: %v", target.rel, err))
					continue
				}
				declared = value * time.Duration(plan.declaring)
			}
			need := declared + time.Duration(plan.undeclared())*generatedModStopFloor
			if total < need {
				shortfalls = append(shortfalls, fmt.Sprintf("%s: total_timeout %s cannot cover %d Mods (%s declared + 3s x %d = %s)",
					target.rel, seconds(total), plan.mods, seconds(declared), plan.undeclared(), seconds(need)))
				continue
			}
			covered = append(covered, configuredTotal{rel: target.rel, total: total})
		}
		switch {
		case failed:
		case len(shortfalls) > 0:
			items = append(items, CheckItem{Name: name, Status: StatusWarn, Detail: fmt.Sprintf(
				"%s; every stop warns and budgets are cut. Set it to %s; %s",
				strings.Join(shortfalls, "; "), seconds(plan.total), describeDeployedGrace(deployed))})
		case len(covered) > 0:
			items = append(items, CheckItem{Name: name, Status: StatusOK, Detail: describeConfiguredTotals(covered) + ", " + describeDeployedGrace(deployed)})
		}
	}
	return items
}

// shutdownSettings is what the doctor reads from one service config.
type shutdownSettings struct {
	Shutdown struct {
		TotalTimeout string `yaml:"total_timeout"`
	} `yaml:"shutdown"`
	DataEngine struct {
		ShutdownTimeout string `yaml:"shutdown_timeout"`
	} `yaml:"dataengine"`
}

// readShutdownSettings parses a service config, or the config.yaml a k8s
// Secret example carries.
func readShutdownSettings(raw []byte, secret bool) (shutdownSettings, error) {
	var settings shutdownSettings
	if secret {
		var document struct {
			StringData map[string]string `yaml:"stringData"`
		}
		if err := yaml.Unmarshal(raw, &document); err != nil {
			return settings, err
		}
		raw = []byte(document.StringData["config.yaml"])
	}
	err := yaml.Unmarshal(raw, &settings)
	return settings, err
}

// configuredTotal is the total_timeout one config file sets.
type configuredTotal struct {
	rel   string
	total time.Duration
}

// describeConfiguredTotals is "total_timeout 101s" when every config sets the
// same total, or names each file's value.
func describeConfiguredTotals(totals []configuredTotal) string {
	for _, configured := range totals {
		if configured.total != totals[0].total {
			parts := make([]string, 0, len(totals))
			for _, each := range totals {
				parts = append(parts, each.rel+" "+seconds(each.total))
			}
			return "total_timeout " + strings.Join(parts, ", ")
		}
	}
	return "total_timeout " + seconds(totals[0].total)
}

// describeDeployedGrace is "grace period 106s" when every deployment template
// on disk waits the same, or names each template's value.
func describeDeployedGrace(deployed []deployedGrace) string {
	if len(deployed) == 0 {
		return "no deployment template on disk sets a grace period"
	}
	for _, template := range deployed {
		if template.grace != deployed[0].grace {
			parts := make([]string, 0, len(deployed))
			for _, each := range deployed {
				parts = append(parts, each.rel+" "+seconds(each.grace))
			}
			return "grace period " + strings.Join(parts, ", ")
		}
	}
	return "grace period " + seconds(deployed[0].grace)
}

func parseConfigDuration(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	return time.ParseDuration(strings.TrimSpace(value))
}
