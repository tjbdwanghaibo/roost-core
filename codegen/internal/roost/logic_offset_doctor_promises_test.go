package roost

// D-L3 第八轮（维护者决定）：所有进程读同一个 time.logic_offset——game 与活动协调器、match 偏移不同，
// 窗口 id、关窗截止、票据时间就错开一个偏移量。roost doctor 检查项目里每个服务的配置
// （dev、prod example、k8s secret example）是否一致；不一致时报错，点名不一致的服务、文件和值。
// 三类文件各是一套部署，分别比较：dev 配了 +24h 测试偏移而 prod example 是 0 是正常的。
// 修前 doctor 不看这个键，一个服务单独配了偏移也照样全绿。

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func setLogicOffset(t *testing.T, root, rel, value string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	if strings.HasPrefix(rel, "deploy/") {
		// The secret example carries the config under stringData.config.yaml, indented four spaces.
		marker := "config.yaml: |\n"
		at := strings.Index(body, marker)
		if at < 0 {
			t.Fatalf("%s: no stringData config.yaml block", rel)
		}
		at += len(marker)
		body = body[:at] + "    time:\n      logic_offset: " + value + "\n" + body[at:]
	} else {
		body += "time:\n  logic_offset: " + value + "\n"
	}
	writeProjectFile(t, root, rel, body)
}

func doctorItems(t *testing.T, root string) (map[string]CheckItem, error) {
	t.Helper()
	var output bytes.Buffer
	err := DoctorWithOptions(context.Background(), root, DoctorOptions{JSONOutput: true}, &output)
	var report DoctorReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("doctor output is not a JSON report (%v):\n%s", decodeErr, output.String())
	}
	items := map[string]CheckItem{}
	for _, item := range report.Items {
		items[item.Name] = item
	}
	return items, err
}

func TestDoctorNamesTheServicesWhoseLogicOffsetDisagrees(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)

	// dev 里只有 game 前拨一天：game 与 activity / match 等不一致，必须点名。
	setLogicOffset(t, root, "configs/service/config.game.yaml", "24h")
	items, err := doctorItems(t, root)
	item, ok := items["time:logic_offset"]
	if !ok || item.Status != StatusFail {
		t.Fatalf("only the dev game config sets time.logic_offset 24h: doctor says %+v (present=%v), want FAIL", item, ok)
	}
	for _, want := range []string{"game=24h", "activity=0s", "match=0s", "configs/service/config.<service>.yaml"} {
		if !strings.Contains(item.Detail, want) {
			t.Errorf("FAIL line does not name %q: %s", want, item.Detail)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "time:logic_offset") {
		t.Errorf("doctor returned %v, want an error naming time:logic_offset", err)
	}

	// 全部未配置（= 0）：一致。
	if item, ok := mustDoctorItems(t, newGameDemo(t))["time:logic_offset"]; !ok || item.Status != StatusOK {
		t.Fatalf("no service sets time.logic_offset: doctor says %+v (present=%v), want an OK time:logic_offset line", item, ok)
	}

	// 一种部署内全部改成同一个值就一致；另一种部署（prod example）仍是 0 不算不一致。
	for _, service := range sortedServiceNames(mustManifest(t, root)) {
		if service != "game" {
			setLogicOffset(t, root, "configs/service/config."+service+".yaml", "24h")
		}
	}
	items, _ = doctorItems(t, root)
	if item := items["time:logic_offset"]; item.Status != StatusOK {
		t.Fatalf("every dev config sets 24h, the production examples 0: doctor says %s %s, want OK", item.Status, item.Detail)
	}

	// k8s secret example 里一个服务的值写错，点名文件。
	setLogicOffset(t, root, "deploy/k8s/base/secret.match.example.yaml", "1d")
	items, _ = doctorItems(t, root)
	if item := items["time:logic_offset"]; item.Status != StatusFail ||
		!strings.Contains(item.Detail, "deploy/k8s/base/secret.match.example.yaml") || !strings.Contains(item.Detail, `"1d"`) {
		t.Fatalf("secret.match.example.yaml sets logic_offset 1d (not a duration): doctor says %s %s, want FAIL naming the file and value", item.Status, item.Detail)
	}
}

func mustDoctorItems(t *testing.T, root string) map[string]CheckItem {
	t.Helper()
	items, _ := doctorItems(t, root)
	return items
}

func mustManifest(t *testing.T, root string) Manifest {
	t.Helper()
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
