package roost

// RR-20260928-04：启用 stats_log 的服务，生成的每一种部署物都要让 stats_log.dir 可写。
// 配置里的 dir 是相对路径（log），按工作目录解析：镜像里是 WORKDIR/log，systemd 里是
// WorkingDirectory/log。旧行为：compose 的 read_only、k8s 的 readOnlyRootFilesystem 让 /app/log
// 不可写，systemd 的 WorkingDirectory=$APP_ROOT 属于 root 且 ProtectSystem=strict，而
// ReadWritePaths 只有状态与日志根目录；statslog 又吞掉写错误，文件从不落盘。
// 这里逐项核对：镜像里该目录属于运行用户 65532（命名卷首次挂载按它初始化属主）；compose 与
// k8s 在该路径挂可写卷；install.sh 把该路径链接到 ReadWritePaths 里的 $LOG_ROOT。
// 没有 stats_log 的工程不多出这些挂载。

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// statsLogDirOf reads stats_log.enabled / dir from one generated service config.
func statsLogDirOf(t *testing.T, name, body string) (dir string, enabled bool) {
	t.Helper()
	var cfg struct {
		StatsLog *struct {
			Enabled bool   `yaml:"enabled"`
			Dir     string `yaml:"dir"`
		} `yaml:"stats_log"`
	}
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	if cfg.StatsLog == nil || !cfg.StatsLog.Enabled {
		return "", false
	}
	return cfg.StatsLog.Dir, true
}

// systemdUnitValue returns the value of one Key= line of the unit install.sh writes.
func systemdUnitValue(t *testing.T, install, key string) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^` + key + `=(.*)$`).FindStringSubmatch(install)
	if match == nil {
		t.Fatalf("install.sh unit has no %s=", key)
	}
	return strings.TrimSpace(match[1])
}

func assertStatsLogIsWritable(t *testing.T, m Manifest, file func(rel string) (string, bool)) {
	t.Helper()
	dockerfile, _ := file("Dockerfile")
	workdir, copies := dockerRuntimeStage(t, dockerfile)
	install, _ := file("deploy/shell/install.sh")
	unitDir := systemdUnitValue(t, install, "WorkingDirectory")
	readWrite := strings.Fields(systemdUnitValue(t, install, "ReadWritePaths"))

	composeBody, _ := file("deploy/docker/docker-compose.prod.yaml")
	var compose struct {
		Services map[string]struct {
			ReadOnly bool `yaml:"read_only"`
			Volumes  []struct {
				Type     string `yaml:"type"`
				Target   string `yaml:"target"`
				ReadOnly bool   `yaml:"read_only"`
			} `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(composeBody), &compose); err != nil {
		t.Fatalf("decode production compose: %v", err)
	}

	anyStatsLog := false
	for _, service := range sortedServiceNames(m) {
		dirs := map[string]bool{}
		for _, rel := range []string{
			"configs/service/config." + service + ".yaml",
			"configs/service/config." + service + ".prod.example.yaml",
		} {
			body, exists := file(rel)
			if !exists {
				t.Fatalf("%s not generated", rel)
			}
			if dir, ok := statsLogDirOf(t, rel, body); ok {
				if path.IsAbs(dir) {
					t.Fatalf("%s: stats_log.dir %q is absolute; this test covers the generated relative form", rel, dir)
				}
				dirs[dir] = true
			}
		}
		if len(dirs) == 0 {
			continue
		}
		anyStatsLog = true
		for dir := range dirs {
			inImage := path.Join(workdir, dir)
			// The image: the directory exists and belongs to the runtime user.
			wantCopy := false
			for _, line := range copies {
				wantCopy = wantCopy || (strings.HasPrefix(line, "COPY --chown=65532:65532 ") && strings.HasSuffix(line, " "+inImage))
			}
			if !wantCopy {
				t.Errorf("service %s: stats_log.dir %q resolves to %s in the image, but the runtime stage does not create it for uid 65532; runtime COPY lines: %q",
					service, dir, inImage, copies)
			}
			// compose: read_only root, so a writable volume at that path.
			spec := compose.Services[service]
			mounted := false
			for _, volume := range spec.Volumes {
				mounted = mounted || (path.Clean(volume.Target) == inImage && !volume.ReadOnly && volume.Type == "volume")
			}
			if spec.ReadOnly && !mounted {
				t.Errorf("compose service %s: read_only root filesystem and no writable volume at %s (stats_log.dir %q)", service, inImage, dir)
			}
			// k8s: readOnlyRootFilesystem, so a writable mount at that path.
			assertKubernetesWritableMount(t, file, service, inImage)
			// systemd: WorkingDirectory/<dir> is a link to LOG_ROOT, which is writable.
			link := `ln -sfn "$LOG_ROOT" "` + path.Join(unitDir, dir) + `"`
			if !strings.Contains(install, link) {
				t.Errorf("service %s: stats_log.dir %q resolves to %s under systemd (read-only: ProtectSystem=strict, ReadWritePaths=%v), and install.sh does not link it to a writable directory\nwant: %s",
					service, dir, path.Join(unitDir, dir), readWrite, link)
			}
			if !contains(readWrite, "$LOG_ROOT") {
				t.Errorf("ReadWritePaths=%v does not include $LOG_ROOT", readWrite)
			}
		}
	}
	if !anyStatsLog {
		for _, line := range copies {
			if strings.HasSuffix(line, " /app/"+defaultStatsLogDir) {
				t.Errorf("project without stats_log still creates the stats directory: %s", line)
			}
		}
		if strings.Contains(install, `ln -sfn "$LOG_ROOT"`) {
			t.Errorf("project without stats_log still links a stats directory in install.sh")
		}
		if strings.Contains(composeBody, "-log\n") {
			t.Errorf("project without stats_log still mounts a stats volume:\n%s", composeBody)
		}
	}
}

func assertKubernetesWritableMount(t *testing.T, file func(rel string) (string, bool), service, mountPath string) {
	t.Helper()
	rel := "deploy/k8s/base/" + service + ".yaml"
	body, _ := file(rel)
	var workload struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						SecurityContext struct {
							ReadOnlyRootFilesystem bool `yaml:"readOnlyRootFilesystem"`
						} `yaml:"securityContext"`
						VolumeMounts []struct {
							Name      string `yaml:"name"`
							MountPath string `yaml:"mountPath"`
							ReadOnly  bool   `yaml:"readOnly"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
					Volumes []struct {
						Name     string         `yaml:"name"`
						EmptyDir map[string]any `yaml:"emptyDir"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.NewDecoder(strings.NewReader(body)).Decode(&workload); err != nil {
		t.Fatalf("decode %s: %v", rel, err)
	}
	pod := workload.Spec.Template.Spec
	for _, c := range pod.Containers {
		if !c.SecurityContext.ReadOnlyRootFilesystem {
			continue
		}
		volume := ""
		for _, mount := range c.VolumeMounts {
			if path.Clean(mount.MountPath) == mountPath && !mount.ReadOnly {
				volume = mount.Name
			}
		}
		if volume == "" {
			t.Errorf("%s: readOnlyRootFilesystem and no writable volumeMount at %s", rel, mountPath)
			continue
		}
		found := false
		for _, v := range pod.Volumes {
			found = found || (v.Name == volume && v.EmptyDir != nil)
		}
		if !found {
			t.Errorf("%s: volumeMount %s at %s has no emptyDir volume", rel, volume, mountPath)
		}
	}
}

func TestGeneratedDeploymentsGiveStatsLogAWritableDirectory(t *testing.T) {
	t.Run("game-demo on disk", func(t *testing.T) {
		root := newGameDemo(t)
		m, err := LoadManifest(root)
		if err != nil {
			t.Fatal(err)
		}
		assertStatsLogIsWritable(t, m, func(rel string) (string, bool) {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			return string(raw), err == nil
		})
	})
	withoutStatsLog := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"nest"}, nil)
	withoutStatsLog.SharedMods = []string{"lock", "ops"}
	for name, m := range map[string]Manifest{
		"default mods":      DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil),
		"stateless service": DefaultManifest("planet", "example.com/planet", []string{"gate"}, []string{"configdata"}, nil),
		"without stats_log": withoutStatsLog,
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := renderProject(m)
			if err != nil {
				t.Fatal(err)
			}
			assertStatsLogIsWritable(t, m, func(rel string) (string, bool) {
				f, ok := plan[rel]
				return string(f.Body), ok
			})
		})
	}
}
