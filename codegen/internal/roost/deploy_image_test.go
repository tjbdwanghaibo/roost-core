package roost

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 用了 configdata Mod 的工程，生成的生产镜像必须自带配置数据，并放在进程按 config_data.dir 解析出的位置：dir 是相对路径
// （configs/data），按运行阶段的 WORKDIR 解析，所以镜像里要有 COPY --from=build /src/<dir> <WORKDIR>/<dir>；
// compose / k8s 不能改掉工作目录，也不能用卷遮住这个目录，否则容器启动即找不到 configs/data 并反复重启。
// 边界不变：环境配置与密钥（configs/service、config.yaml）仍然不进镜像。

// dockerRuntimeStage returns the WORKDIR and the COPY lines of the last stage
// of a generated Dockerfile (the image that actually runs).
func dockerRuntimeStage(t *testing.T, dockerfile string) (workdir string, copies []string) {
	t.Helper()
	lines := strings.Split(dockerfile, "\n")
	last := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "FROM ") {
			last = i
		}
	}
	if last < 0 {
		t.Fatalf("Dockerfile has no FROM:\n%s", dockerfile)
	}
	for _, line := range lines[last+1:] {
		switch {
		case strings.HasPrefix(line, "WORKDIR "):
			workdir = strings.TrimSpace(strings.TrimPrefix(line, "WORKDIR "))
		case strings.HasPrefix(line, "COPY "):
			copies = append(copies, line)
		}
	}
	return workdir, copies
}

// configDataDirOf reads config_data.dir from one generated service config;
// ok is false when the service does not configure configdata.
func configDataDirOf(t *testing.T, name, body string) (dir string, ok bool) {
	t.Helper()
	var cfg struct {
		ConfigData *struct {
			Dir string `yaml:"dir"`
		} `yaml:"config_data"`
	}
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	if cfg.ConfigData == nil {
		return "", false
	}
	return cfg.ConfigData.Dir, true
}

func assertImageCarriesConfigData(t *testing.T, m Manifest, file func(rel string) (string, bool)) {
	t.Helper()
	dockerfile, _ := file("Dockerfile")
	workdir, copies := dockerRuntimeStage(t, dockerfile)
	for _, line := range copies {
		if strings.Contains(line, "configs/service") || strings.Contains(line, "config.yaml") {
			t.Errorf("runtime stage copies environment config into the image: %s", line)
		}
	}
	wantDirs := map[string]bool{}
	for _, service := range sortedServiceNames(m) {
		for _, rel := range []string{
			"configs/service/config." + service + ".yaml",
			"configs/service/config." + service + ".prod.example.yaml",
		} {
			body, exists := file(rel)
			if !exists {
				t.Fatalf("%s not generated", rel)
			}
			if dir, ok := configDataDirOf(t, rel, body); ok {
				if path.IsAbs(dir) {
					t.Fatalf("%s: config_data.dir %q is absolute; this test covers the generated relative form", rel, dir)
				}
				wantDirs[dir] = true
			}
		}
	}
	hasConfigData := contains(allProjectMods(m), "configdata")
	if hasConfigData != (len(wantDirs) > 0) {
		t.Fatalf("configdata Mod enabled = %v, but service configs with config_data.dir = %v", hasConfigData, wantDirs)
	}
	for dir := range wantDirs {
		if workdir == "" {
			t.Fatalf("runtime stage has no WORKDIR, relative config_data.dir %q has no fixed anchor:\n%s", dir, dockerfile)
		}
		want := "COPY --from=build " + path.Join("/src", dir) + " " + path.Join(workdir, dir)
		found := false
		for _, line := range copies {
			found = found || line == want
		}
		if !found {
			t.Errorf("config_data.dir %q resolves to %s in the image, but the runtime stage does not copy it there\nwant: %s\nruntime COPY lines: %q",
				dir, path.Join(workdir, dir), want, copies)
		}
		if _, ok := file(path.Join(dir, "_manifest.json")); !ok {
			t.Errorf("the build context has no %s/_manifest.json, so the COPY above has no source", dir)
		}
	}
	if len(wantDirs) == 0 {
		for _, line := range copies {
			if strings.Contains(line, defaultConfigDataDir) {
				t.Errorf("project without configdata still copies config data: %s", line)
			}
		}
		return
	}

	// compose / k8s must neither move the working directory nor shadow the
	// data directory with a mount (the only mounts are config.yaml, /tmp and WAL).
	composeBody, _ := file("deploy/docker/docker-compose.prod.yaml")
	var compose struct {
		Services map[string]struct {
			WorkingDir string `yaml:"working_dir"`
			Volumes    []struct {
				Target string `yaml:"target"`
			} `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(composeBody), &compose); err != nil {
		t.Fatalf("decode production compose: %v", err)
	}
	for service, spec := range compose.Services {
		if spec.WorkingDir != "" && spec.WorkingDir != workdir {
			t.Errorf("compose service %s: working_dir %q moves the anchor of config_data.dir away from %s", service, spec.WorkingDir, workdir)
		}
		for _, volume := range spec.Volumes {
			for dir := range wantDirs {
				if shadows(volume.Target, path.Join(workdir, dir)) {
					t.Errorf("compose service %s: volume at %s hides the image's %s", service, volume.Target, path.Join(workdir, dir))
				}
			}
		}
	}
	for _, service := range sortedServiceNames(m) {
		rel := "deploy/k8s/base/" + service + ".yaml"
		body, _ := file(rel)
		var workload struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							WorkingDir   string `yaml:"workingDir"`
							VolumeMounts []struct {
								MountPath string `yaml:"mountPath"`
							} `yaml:"volumeMounts"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		// The first document is the workload; the Service follows.
		if err := yaml.NewDecoder(strings.NewReader(body)).Decode(&workload); err != nil {
			t.Fatalf("decode %s: %v", rel, err)
		}
		containers := workload.Spec.Template.Spec.Containers
		if len(containers) == 0 {
			t.Fatalf("%s: no containers", rel)
		}
		for _, c := range containers {
			if c.WorkingDir != "" && c.WorkingDir != workdir {
				t.Errorf("%s: workingDir %q moves the anchor of config_data.dir away from %s", rel, c.WorkingDir, workdir)
			}
			for _, mount := range c.VolumeMounts {
				for dir := range wantDirs {
					if shadows(mount.MountPath, path.Join(workdir, dir)) {
						t.Errorf("%s: volumeMount at %s hides the image's %s", rel, mount.MountPath, path.Join(workdir, dir))
					}
				}
			}
		}
	}
}

// shadows reports whether a mount at target hides dir (target is dir or one of its parents).
func shadows(target, dir string) bool {
	target = path.Clean(target)
	return target == dir || strings.HasPrefix(dir, strings.TrimSuffix(target, "/")+"/")
}

func TestProductionImageCarriesConfigDataWhereConfigDataDirResolves(t *testing.T) {
	t.Run("game-demo on disk", func(t *testing.T) {
		root := newGameDemo(t)
		m, err := LoadManifest(root)
		if err != nil {
			t.Fatal(err)
		}
		assertImageCarriesConfigData(t, m, func(rel string) (string, bool) {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			return string(raw), err == nil
		})
	})
	for name, m := range map[string]Manifest{
		"default mods":       DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil),
		"configdata only":    DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil),
		"without configdata": DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"nest"}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := renderProject(m)
			if err != nil {
				t.Fatal(err)
			}
			assertImageCarriesConfigData(t, m, func(rel string) (string, bool) {
				f, ok := plan[rel]
				return string(f.Body), ok
			})
		})
	}
}

// 启用 stats_log 的服务，生成的每一种部署物都要让 stats_log.dir 可写（dir 是相对路径 log，按工作目录解析：镜像里是
// WORKDIR/log，systemd 里是 WorkingDirectory/log）；statslog 吞掉写错误，不可写就静默不落盘。
// 逐项核对：镜像里该目录属于运行用户 65532（命名卷首次挂载按它初始化属主）；compose 与 k8s 在该路径挂可写卷；
// install.sh 把该路径链接到 ReadWritePaths 里的 $LOG_ROOT。没有 stats_log 的工程不多出这些挂载。

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

// systemdReleaseDir 是服务启动时 unit 的 WorkingDirectory（install.sh 的写法）：重启前 $APP_ROOT/current
// 已切到 $RELEASE_ROOT。
func systemdReleaseDir(t *testing.T, install string) string {
	t.Helper()
	unitDir := systemdUnitValue(t, install, "WorkingDirectory")
	if unitDir == "$APP_ROOT/current" {
		if !strings.Contains(install, `switch_release "$RELEASE_ROOT"`) {
			t.Fatalf("WorkingDirectory=%s but install.sh never switches current to $RELEASE_ROOT", unitDir)
		}
		return "$RELEASE_ROOT"
	}
	return unitDir
}

func assertStatsLogIsWritable(t *testing.T, m Manifest, file func(rel string) (string, bool)) {
	t.Helper()
	dockerfile, _ := file("Dockerfile")
	workdir, copies := dockerRuntimeStage(t, dockerfile)
	install, _ := file("deploy/shell/install.sh")
	unitDir := systemdReleaseDir(t, install)
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
