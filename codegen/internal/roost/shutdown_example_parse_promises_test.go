package roost

// RR-20260926-80 复核残留：doctor 读不懂示例配置时，WARN 只说明哪个文件、哪个键解析失败。
//
// prod 示例或 k8s secret 示例整份 YAML 解析失败、或 shutdown.total_timeout / dataengine.shutdown_timeout
// 不是合法时长时，doctor 根本没算出 total，谈不上“覆盖不了 Mod 保底”。旧行为把这类错误和真正的不足放进同一个
// WARN，统一接上“every stop warns and budgets are cut. Set it to …”：像是在说时长不够、改成建议值就好，
// 而问题其实是文件读不懂。修后解析失败单独列出（文件 + 键 + 解析错误），不带停机告警与建议值；
// 真正的不足照旧带建议值。仍是 WARN：示例不被任何进程直接读取，按示例部署时 App 对非法时长按 30s 兜底，
// 这种兜底会不会被 SIGKILL 由 FAIL 分支（configuredShutdownTotal 同样按 30s 计）负责。dev 配置非法时长仍是 FAIL。

import (
	"strings"
	"testing"
)

const staleShutdownAdvice = "every stop warns and budgets are cut"

// setDataEngineShutdownTimeout rewrites the dataengine: block's
// shutdown_timeout (other blocks, such as a gateway's, have their own).
func setDataEngineShutdownTimeout(t *testing.T, root, rel, value string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	block := strings.Index(body, "dataengine:\n")
	if block < 0 {
		t.Fatalf("%s has no dataengine: block", rel)
	}
	key := strings.Index(body[block:], "shutdown_timeout: ")
	if key < 0 {
		t.Fatalf("%s: dataengine: block has no shutdown_timeout", rel)
	}
	at := block + key + len("shutdown_timeout: ")
	end := strings.IndexByte(body[at:], '\n')
	writeProjectFile(t, root, rel, body[:at]+value+body[at+end:])
}

func TestDoctorNamesTheFileAndKeyOfAnExampleItCannotParse(t *testing.T) {
	t.Parallel()
	dev := "configs/service/config.game.yaml"
	prod := "configs/service/config.game.prod.example.yaml"
	secret := "deploy/k8s/base/secret.game.example.yaml"

	for _, tc := range []struct {
		name    string
		rel     string
		corrupt func(t *testing.T, root string)
		want    []string // every fragment the WARN must carry
	}{
		{
			name:    "prod example total_timeout is not a duration",
			rel:     prod,
			corrupt: func(t *testing.T, root string) { setShutdownTotal(t, root, prod, "soon") },
			want:    []string{prod + ": shutdown.total_timeout", `"soon"`},
		},
		{
			name:    "secret example dataengine.shutdown_timeout is not a duration",
			rel:     secret,
			corrupt: func(t *testing.T, root string) { setDataEngineShutdownTimeout(t, root, secret, "30 seconds") },
			want:    []string{secret + ": dataengine.shutdown_timeout", `"30 seconds"`},
		},
		{
			name: "prod example does not parse",
			rel:  prod,
			corrupt: func(t *testing.T, root string) {
				writeProjectFile(t, root, prod, readProjectFile(t, root, prod)+"shutdown: [\n")
			},
			want: []string{prod + ": does not parse: yaml: "},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newGameDemo(t)
			if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusOK {
				t.Fatalf("setup: a fresh project is %s %s", item.Status, item.Detail)
			}
			tc.corrupt(t, root)
			item := shutdownStatus(t, root)["shutdown:game"]
			if item.Status != StatusWarn {
				t.Fatalf("%s: %s %s, want a WARN", tc.name, item.Status, item.Detail)
			}
			for _, fragment := range tc.want {
				if !strings.Contains(item.Detail, fragment) {
					t.Errorf("%s: WARN does not say %q: %s", tc.name, fragment, item.Detail)
				}
			}
			if strings.Contains(item.Detail, staleShutdownAdvice) || strings.Contains(item.Detail, "Set it to") {
				t.Errorf("%s: a parse failure is reported as a total that cannot cover the Mods: %s", tc.name, item.Detail)
			}
			for _, other := range []string{dev, prod, secret} {
				if other != tc.rel && strings.Contains(item.Detail, other+":") {
					t.Errorf("%s: WARN names %s, which parses and covers the Mods: %s", tc.name, other, item.Detail)
				}
			}
		})
	}
}

// A real shortfall next to a parse failure keeps its advice, and the advice
// follows the shortfall rather than the file the doctor could not read.
func TestDoctorKeepsTheAdviceForARealShortfallNextToAParseFailure(t *testing.T) {
	root := newGameDemo(t)
	prod := "configs/service/config.game.prod.example.yaml"
	secret := "deploy/k8s/base/secret.game.example.yaml"
	setShutdownTotal(t, root, prod, "soon")
	setShutdownTotal(t, root, secret, "60s")

	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("%s %s, want a WARN", item.Status, item.Detail)
	}
	shortfall := secret + ": total_timeout 60s cannot cover 23 Mods (40s declared + 3s x 21 + 3s singleton release = 106s); " + staleShutdownAdvice + ". Set it to 111s"
	if !strings.Contains(item.Detail, shortfall) {
		t.Errorf("WARN does not carry the shortfall with its advice %q: %s", shortfall, item.Detail)
	}
	if !strings.Contains(item.Detail, prod+": shutdown.total_timeout") {
		t.Errorf("WARN does not name the file and key it cannot parse: %s", item.Detail)
	}
	if strings.Contains(item.Detail, prod+": total_timeout") {
		t.Errorf("WARN judges the unparsable %s as a shortfall: %s", prod, item.Detail)
	}
}

// The dev config is what deploy/dev runs directly; an invalid duration there
// stays a FAIL (unchanged by this fix).
func TestDoctorStillFailsOnAnInvalidDurationInTheDevConfig(t *testing.T) {
	root := newGameDemo(t)
	dev := "configs/service/config.game.yaml"
	setShutdownTotal(t, root, dev, "soon")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusFail || !strings.Contains(item.Detail, dev+": shutdown.total_timeout") {
		t.Errorf("dev config with an invalid total_timeout: %s %s, want a FAIL naming the file and key", item.Status, item.Detail)
	}
}
