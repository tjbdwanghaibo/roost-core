package configdata

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
)

// 运维命令经 admin 注册表执行：reload 说出新版本、上一版本与变化的表；内容没变时 changed=false 并在
// message 里说明（含“只改 CSV 要先 roost generate”）；dry_run 报告变化但不发布；status 给出各表行数；
// 违反表规则的重载整次拒绝、旧快照继续服务。
func TestAdminCommandsReloadReportChangesAndStatus(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7}]`)
	reg := NewRegistry()
	MustRegisterTable(reg, TableDef[int32, testMonsterCfg]{
		Name: "monster", File: "monster.json", Key: func(v testMonsterCfg) int32 { return v.ID },
		Validate: func(_ *BuildContext, v testMonsterCfg) error {
			if v.Name == "" {
				return errEmptyMonsterName
			}
			return nil
		},
	})
	store := NewStore(reg, dir)
	commands := admin.NewRegistry()
	if err := RegisterAdminCommands(commands, store); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := func(name, payload string) admin.Result {
		t.Helper()
		result, err := commands.Execute(ctx, admin.Command{Name: name, Payload: []byte(payload)})
		if err != nil {
			t.Fatalf("%s %s: %v", name, payload, err)
		}
		return result
	}
	if status := run(AdminCommandStatus, ""); status.Data["version"] != uint64(0) {
		t.Fatalf("status before any load = %+v", status)
	}
	if loaded := run(AdminCommandReload, `{"reason":"first"}`); loaded.Data["version"] != uint64(1) || loaded.Data["changed"] != true {
		t.Fatalf("first reload = %+v", loaded)
	}

	same := run(AdminCommandReload, "")
	if same.Data["changed"] != false || same.Data["version"] != uint64(2) || same.Data["previous"] != uint64(1) ||
		!strings.Contains(same.Message, "no table changed") || !strings.Contains(same.Message, "roost generate") {
		t.Fatalf("unchanged reload = %+v, want changed=false, version 2 after 1, and a message naming the JSON-only rule", same)
	}

	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"wolf","scene_id":7},{"id":2,"name":"bear","scene_id":7}]`)
	preview := run(AdminCommandReload, `{"dry_run":true}`)
	if preview.Data["dry_run"] != true || preview.Data["changed"] != true || store.Current().Version != 2 {
		t.Fatalf("dry run = %+v live=%d, want the change reported and nothing published", preview, store.Current().Version)
	}
	applied := run(AdminCommandReload, `{"reason":"ops"}`)
	changes, ok := applied.Data["changes"].(Changes)
	if !ok || len(changes.Changed) != 1 || changes.Changed[0] != (MemberChange{Kind: "table", Name: "monster", RowsBefore: 1, RowsAfter: 2}) {
		t.Fatalf("reload after adding a row = %+v", applied)
	}
	if status := run(AdminCommandStatus, ""); status.Data["version"] != store.Current().Version || status.Data["tables"].(map[string]int)["monster"] != 2 {
		t.Fatalf("status = %+v", status)
	}

	writeFile(t, filepath.Join(dir, "monster.json"), `[{"id":1,"name":"","scene_id":7}]`)
	live := store.Current()
	if _, err := commands.Execute(ctx, admin.Command{Name: AdminCommandReload}); err == nil || store.Current() != live {
		t.Fatalf("a reload breaking the table's rule = %v, live changed=%v; want refused with the old snapshot serving", err, store.Current() != live)
	}
	back := run(AdminCommandRollback, `{"reason":"undo"}`)
	if back.Data["version"] != uint64(2) || back.Data["changed"] != true {
		t.Fatalf("rollback = %+v, want generation 2 back with monster changed", back)
	}
}

var errEmptyMonsterName = errors.New("monster name is empty")
