package roost

// RR-20260928-05：用 configdata 的服务按生成的 install.sh 安装后，进程在 unit 的
// WorkingDirectory 下按相对的 config_data.dir（configs/data）读数据表，那里必须有数据。
// 旧行为：install.sh 只把二进制与 config.yaml 装进 release，WorkingDirectory=$APP_ROOT，
// 没人安装 configs/data，启动即 "configdata: stat dir configs/data: no such file or directory"
// （临时根演练实测）。新布局与镜像一致（RR-20260927-34）：数据随二进制进 release，
// WorkingDirectory 是 current 指向的 release；数据目录缺失在创建 release 之前就报错，
// 不留下一个挡住重试的“不可变”release。不用 configdata 的服务不装数据。

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// shellCaseArms returns, for every `case "$SERVICE" in` arm of install.sh whose
// body contains needle, the services the arm matches.
func shellCaseArms(install, needle string) []string {
	var services []string
	arm := regexp.MustCompile(`(?m)^  ([a-zA-Z0-9_|-]+)\)\n((?:    .*\n)+?)    ;;`)
	for _, match := range arm.FindAllStringSubmatch(install, -1) {
		if strings.Contains(match[2], needle) {
			services = append(services, strings.Split(match[1], "|")...)
		}
	}
	return services
}

func assertShellInstallCarriesConfigData(t *testing.T, m Manifest, file func(rel string) (string, bool)) {
	t.Helper()
	install, _ := file("deploy/shell/install.sh")
	release := systemdReleaseDir(t, install)
	createRelease := strings.Index(install, `install -d -m 0755 "$APP_ROOT/releases" "$RELEASE_ROOT"`)
	if createRelease < 0 {
		t.Fatal("install.sh no longer creates $RELEASE_ROOT the way this test expects")
	}
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
			if dir, ok := configDataDirOf(t, rel, body); ok {
				if path.IsAbs(dir) {
					t.Fatalf("%s: config_data.dir %q is absolute; this test covers the generated relative form", rel, dir)
				}
				dirs[dir] = true
			}
		}
		copyLine := func(dir string) string { return `cp -R "$CONFIG_DATA" "` + path.Join(release, dir) + `"` }
		if len(dirs) == 0 {
			if contains(shellCaseArms(install, `cp -R "$CONFIG_DATA"`), service) {
				t.Errorf("service %s does not use configdata, but install.sh copies config data for it", service)
			}
			continue
		}
		for dir := range dirs {
			if !strings.Contains(install, `CONFIG_DATA=${CONFIG_DATA:-"$ROOT/`+dir+`"}`) {
				t.Errorf("service %s: install.sh does not take the tables from the project's %s", service, dir)
			}
			if !contains(shellCaseArms(install, copyLine(dir)), service) {
				t.Errorf("service %s: config_data.dir %q resolves to %s under systemd (WorkingDirectory=%s), and install.sh does not install the tables there\nwant, in the %s arm: %s",
					service, dir, path.Join(release, dir), systemdUnitValue(t, install, "WorkingDirectory"), service, copyLine(dir))
			}
		}
		guard := strings.Index(install, `[ -f "$CONFIG_DATA/_manifest.json" ]`)
		if guard < 0 || guard > createRelease || !contains(shellCaseArms(install, `[ -f "$CONFIG_DATA/_manifest.json" ]`), service) {
			t.Errorf("service %s: install.sh does not refuse a missing %s/_manifest.json before creating the release", service, defaultConfigDataDir)
		}
	}
	if !contains(allProjectMods(m), "configdata") && strings.Contains(install, "CONFIG_DATA") {
		t.Errorf("project without configdata still handles config data in install.sh")
	}
}

func TestShellInstallPutsConfigDataWhereConfigDataDirResolves(t *testing.T) {
	t.Run("game-demo on disk", func(t *testing.T) {
		root := newGameDemo(t)
		m, err := LoadManifest(root)
		if err != nil {
			t.Fatal(err)
		}
		assertShellInstallCarriesConfigData(t, m, func(rel string) (string, bool) {
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
			assertShellInstallCarriesConfigData(t, m, func(rel string) (string, bool) {
				f, ok := plan[rel]
				return string(f.Body), ok
			})
		})
	}
}
