package roost

// RR-20260927-33：生成的 deploy/docker/docker-compose.prod.yaml 里每个服务的 tmpfs 必须是恰好一条挂载，
// 按 YAML 语义解析后以 "/" 开头（挂载点）、选项跟在同一项里。旧行为写成 flow 序列
// `tmpfs: [/tmp:rw,noexec,nosuid,size=64m]`，YAML 在 flow 序列里按逗号分项，解析出
// `/tmp:rw`、`noexec`、`nosuid`、`size=64m` 四项，`docker compose up` 报
// `invalid mount path: 'noexec' mount path must be absolute`，所有服务都起不来；
// 生成工程的 CI 只跑 `docker compose config --quiet`，看不出来。字符串断言也看不出来，
// 所以这里按 YAML 解析后断言结构。

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProductionComposeTmpfsIsOneAbsoluteMountPerService(t *testing.T) {
	for name, m := range map[string]Manifest{
		"stateful":  DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil),
		"stateless": DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			body := renderProductionCompose(m)
			var compose struct {
				Services map[string]struct {
					Tmpfs       []string `yaml:"tmpfs"`
					SecurityOpt []string `yaml:"security_opt"`
					CapDrop     []string `yaml:"cap_drop"`
				} `yaml:"services"`
			}
			if err := yaml.Unmarshal([]byte(body), &compose); err != nil {
				t.Fatalf("decode production compose: %v\n%s", err, body)
			}
			if len(compose.Services) != len(m.Services) {
				t.Fatalf("compose has %d services, manifest has %d:\n%s", len(compose.Services), len(m.Services), body)
			}
			for service, spec := range compose.Services {
				if len(spec.Tmpfs) != 1 {
					t.Errorf("service %s: tmpfs parsed as %d items %q, want exactly one mount", service, len(spec.Tmpfs), spec.Tmpfs)
					continue
				}
				if mount := spec.Tmpfs[0]; !strings.HasPrefix(mount, "/") || mount != "/tmp:rw,noexec,nosuid,size=64m" {
					t.Errorf("service %s: tmpfs = %q, want the single absolute mount /tmp:rw,noexec,nosuid,size=64m", service, mount)
				}
				// 同一行的另外两个 flow 序列今天各只有一个值；核对它们也按一项解析，
				// 以后有人往里加带逗号的值时这里会先红。
				if len(spec.SecurityOpt) != 1 || spec.SecurityOpt[0] != "no-new-privileges:true" {
					t.Errorf("service %s: security_opt = %q, want [no-new-privileges:true]", service, spec.SecurityOpt)
				}
				if len(spec.CapDrop) != 1 || spec.CapDrop[0] != "ALL" {
					t.Errorf("service %s: cap_drop = %q, want [ALL]", service, spec.CapDrop)
				}
			}
		})
	}
}
