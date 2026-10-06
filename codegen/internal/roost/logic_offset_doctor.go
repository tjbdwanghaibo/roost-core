package roost

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// logicOffsetDeployments are the config sets doctor compares time.logic_offset
// within. Each is one deployment of the whole project — every service's dev
// config, every production example, every k8s Secret example — and the
// processes of one deployment must read one offset: the game and the activity
// coordinator compute the same window ids and deadlines from it, and the game's
// matchmaker widens its window from ticket times the match service dated
// (D-L3, maintainer round 8). Two deployments may differ: a dev or staging
// environment moves business time forward while production keeps 0, which the
// App enforces at start under env: production. A production config outside the
// repository cannot be seen; the examples are what the repository says.
var logicOffsetDeployments = []struct {
	label  string
	file   func(service string) string
	secret bool
}{
	{"dev", func(service string) string { return "configs/service/config." + service + ".yaml" }, false},
	{"prod example", func(service string) string { return "configs/service/config." + service + ".prod.example.yaml" }, false},
	{"k8s secret example", func(service string) string { return "deploy/k8s/base/secret." + service + ".example.yaml" }, true},
}

// checkLogicOffsets is the time:logic_offset doctor line: OK when every
// service of each deployment reads the same offset, FAIL naming the services,
// files and values otherwise.
func checkLogicOffsets(root string, m Manifest) CheckItem {
	item := CheckItem{Name: "time:logic_offset"}
	var agreed, problems []string
	for _, deployment := range logicOffsetDeployments {
		var (
			values  []string // "service=value", in service order
			invalid []string
			first   time.Duration
			differ  bool
			read    int
		)
		for _, service := range sortedServiceNames(m) {
			rel := deployment.file(service)
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				continue // a service without this file is not part of this deployment
			}
			offset, err := configLogicOffset(raw, deployment.secret)
			if err != nil {
				invalid = append(invalid, rel+": "+err.Error())
				continue
			}
			if read == 0 {
				first = offset
			} else if offset != first {
				differ = true
			}
			read++
			values = append(values, service+"="+describeOffset(offset))
		}
		pattern := deployment.file("<service>")
		switch {
		case len(invalid) > 0:
			problems = append(problems, deployment.label+": "+strings.Join(invalid, "; "))
		case differ:
			problems = append(problems, fmt.Sprintf("%s (%s) disagrees: %s", deployment.label, pattern, strings.Join(values, ", ")))
		case read > 0:
			agreed = append(agreed, deployment.label+" "+describeOffset(first))
		}
	}
	if len(problems) > 0 {
		item.Status = StatusFail
		item.Detail = strings.Join(problems, " | ") + "; every service of one deployment must set the same time.logic_offset " +
			"(a missing key is 0s), or activity windows, match tickets and mail expiry drift apart by the difference"
		return item
	}
	item.Status = StatusOK
	if len(agreed) == 0 {
		item.Detail = "no service config found"
	} else {
		item.Detail = "one offset per deployment: " + strings.Join(agreed, ", ")
	}
	return item
}

// configLogicOffset reads time.logic_offset from a service config, or from the
// config.yaml a k8s Secret example carries, the way the App reads it
// (app.ConfigDuration): missing, empty, "0" and 0 are zero; a string must
// parse as a Go duration; any other number needs a unit. The codegen layer
// does not import app, so the rule is restated here.
func configLogicOffset(raw []byte, secret bool) (time.Duration, error) {
	if secret {
		var document struct {
			StringData map[string]string `yaml:"stringData"`
		}
		if err := yaml.Unmarshal(raw, &document); err != nil {
			return 0, fmt.Errorf("does not parse: %w", err)
		}
		raw = []byte(document.StringData["config.yaml"])
	}
	var config struct {
		Time struct {
			LogicOffset any `yaml:"logic_offset"`
		} `yaml:"time"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return 0, fmt.Errorf("does not parse: %w", err)
	}
	switch value := config.Time.LogicOffset.(type) {
	case nil:
		return 0, nil
	case string:
		text := strings.TrimSpace(value)
		if text == "" || text == "0" {
			return 0, nil
		}
		offset, err := time.ParseDuration(text)
		if err != nil {
			return 0, fmt.Errorf("time.logic_offset %q is not a duration (for example 24h)", text)
		}
		return offset, nil
	case int, int64, uint64, float64:
		if number, _ := strconv.ParseFloat(fmt.Sprint(value), 64); number == 0 {
			return 0, nil
		}
		return 0, fmt.Errorf("time.logic_offset %v needs a unit (for example 24h); a bare number would be read as nanoseconds", value)
	default:
		return 0, fmt.Errorf("time.logic_offset %v is not a duration (for example 24h)", value)
	}
}

// describeOffset renders an offset the way a config writes it: "24h", "1h30m",
// "0s" — not time.Duration's "24h0m0s".
func describeOffset(offset time.Duration) string {
	text := offset.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}
