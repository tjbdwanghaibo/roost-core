package roost

// RR-20260927-34：用了 configdata Mod 的工程，生成的生产镜像必须自带配置数据，并且放在进程按
// config_data.dir 解析出来的位置：配置里的 dir 是相对路径（configs/data），按运行阶段的 WORKDIR
// 解析，所以镜像里要有 COPY --from=build /src/<dir> <WORKDIR>/<dir>；compose / k8s 不能改掉工作目录，
// 也不能用卷遮住这个目录。旧行为运行阶段只拷二进制与 healthprobe，compose / k8s 只挂 config.yaml，
// 容器启动即 "configdata: stat dir configs/data: no such file or directory" 并反复重启。
// 另外守住原来的边界：环境配置与密钥（configs/service、config.yaml）仍然不进镜像。

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

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
