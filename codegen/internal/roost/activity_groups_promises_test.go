package roost

// C4（维护者决定，docs/feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md）：托管 activity 协调器的工程生成
// 一份活动组文件 configs/activity_groups.yaml，协调器与 game-demo 的 game 都经 activity.groups_file 读它。
// 承诺：
//   - 文件存在，一个以工程名为 id 的组，成员覆盖生成的各部署方式给第一个业务服务的 sid（本机 / k8s /
//     shell 1000、second-game.sh 1001、生产 compose 的 1000+N），game 按任何一种生成的方式启动都在组里；
//   - 协调器的开发 / 生产示例配置写 groups_file，不再写 sweep_groups: []（组 id 只写在组文件里）；
//   - game-demo 的 game 三份配置写 groups_file，不再有 activity.game_sids；
//   - 文件随发布走：Dockerfile 把它拷到 /app 下同一相对路径，shell install 把它装进 release；
//   - second-game.sh 在 game 读组文件时先检查自己的 sid 在文件里。
// 文件的校验规则本身（≤ 64、重复、int32……）由 kit/service/global/activity 的 groups 测试守；生成器不得
// import kit（dependency_boundary_test），这里按文件格式直接解析。

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGameDemoActivityGroupsFileIsTheOneDefinition(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	var file struct {
		Groups []struct {
			ID       string  `yaml:"id"`
			GameSIDs []int32 `yaml:"game_sids"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal([]byte(read(activityGroupsFile)), &file); err != nil {
		t.Fatalf("decode %s: %v", activityGroupsFile, err)
	}
	if len(file.Groups) != 1 || file.Groups[0].ID != m.Project.Name {
		t.Fatalf("%s groups = %+v, want one group named %q", activityGroupsFile, file.Groups, m.Project.Name)
	}
	members := file.Groups[0].GameSIDs

	// Every sid a generated deployment starts the game with is a member.
	gameService := m.Access["player"].Service
	composeSID := regexp.MustCompile(`"` + regexp.QuoteMeta(gameService) + `", "--sid=(\d+)"`).FindStringSubmatch(read("deploy/docker/docker-compose.prod.yaml"))
	if composeSID == nil {
		t.Fatal("the production compose does not start the game with a --sid")
	}
	k8sSID := regexp.MustCompile(`name: ROOST_SID\s+value: "(\d+)"`).FindStringSubmatch(read("deploy/k8s/base/" + gameService + ".yaml"))
	if k8sSID == nil {
		t.Fatal("the k8s workload sets no ROOST_SID")
	}
	secondSID := regexp.MustCompile(`SECOND_SID:-(\d+)`).FindStringSubmatch(read("deploy/dev/second-game.sh"))
	devSID := regexp.MustCompile(`SID:-(\d+)`).FindStringSubmatch(read("deploy/dev/run.sh"))
	if secondSID == nil || devSID == nil {
		t.Fatal("the dev scripts set no default sid")
	}
	for _, started := range []struct{ how, sid string }{
		{"production compose", composeSID[1]}, {"k8s", k8sSID[1]}, {"second-game.sh", secondSID[1]}, {"run.sh", devSID[1]},
	} {
		sid, _ := strconv.Atoi(started.sid)
		if !slices.Contains(members, int32(sid)) {
			t.Fatalf("%s starts the game as sid %d, which is in no group of %s %v: that game would refuse to start", started.how, sid, activityGroupsFile, members)
		}
	}

	for _, rel := range []string{"configs/service/config.activity.yaml", "configs/service/config.activity.prod.example.yaml"} {
		leaves := configLeaves(t, rel, read(rel))
		if leaves["activity.groups_file"] != activityGroupsFile {
			t.Fatalf("%s activity.groups_file = %q, want %q", rel, leaves["activity.groups_file"], activityGroupsFile)
		}
		if _, ok := leaves["activity.sweep_groups"]; ok {
			t.Fatalf("%s still writes activity.sweep_groups; the group ids are written once, in %s", rel, activityGroupsFile)
		}
	}
	for _, rel := range []string{
		"configs/service/config." + gameService + ".yaml",
		"configs/service/config." + gameService + ".prod.example.yaml",
	} {
		leaves := configLeaves(t, rel, read(rel))
		if leaves["activity.groups_file"] != activityGroupsFile {
			t.Fatalf("%s activity.groups_file = %q, want %q", rel, leaves["activity.groups_file"], activityGroupsFile)
		}
		if _, ok := leaves["activity.game_sids"]; ok {
			t.Fatalf("%s still has the removed activity.game_sids", rel)
		}
	}

	if want := "COPY --from=build /src/" + activityGroupsFile + " /app/" + activityGroupsFile; !strings.Contains(read("Dockerfile"), want) {
		t.Fatalf("the Dockerfile does not ship the groups file (%q)", want)
	}
	if want := `install -m 0644 "$ACTIVITY_GROUPS" "$RELEASE_ROOT/` + activityGroupsFile + `"`; !strings.Contains(read("deploy/shell/install.sh"), want) {
		t.Fatalf("install.sh does not install the groups file into the release (%q)", want)
	}
	if !strings.Contains(read("deploy/dev/second-game.sh"), "check_activity_group\n") {
		t.Fatal("second-game.sh does not check its sid against the groups file before starting")
	}
}

// A project without the activity coordinator gets no groups file and no
// Dockerfile line for one.
func TestNoActivityGroupsFileWithoutTheCoordinator(t *testing.T) {
	t.Parallel()
	m := Manifest{Project: ProjectSpec{Name: "plain"}, Services: map[string]ServiceSpec{"game": {}}}
	if hostsActivityCoordinator(m) {
		t.Fatal("a project with no activity service hosts the coordinator")
	}
	if strings.Contains(renderDockerfile(m), activityGroupsFile) {
		t.Fatal("the Dockerfile copies a groups file the project does not have")
	}
	if guard, install := renderShellActivityGroups(m); guard != "" || install != "" {
		t.Fatal("install.sh requires a groups file the project does not have")
	}
}
