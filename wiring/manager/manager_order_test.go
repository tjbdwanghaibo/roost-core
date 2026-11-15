package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// manager 的后台工作（模块的恢复扫描、CellWorld 的 Node、换服的后台步骤）调用 Nest、Remote、Redis、NATS 与心跳；
// ManagerMod 必须在这些 Mod 之后启动、之前停止：停机时 manager 取消并排空在途工作，那时它用的依赖都还在。
// 生成的 bootstrap 把 ManagerMod 登记在 Nest 之前（登记顺序不是启动顺序），这里照那个顺序登记，经真实 App 起停一次。

type orderJournal struct {
	mu      sync.Mutex
	entries []string
}

func (j *orderJournal) add(entry string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, entry)
}

func (j *orderJournal) index(entry string) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Index(j.entries, entry)
}

// journalMod 只记启停；deps 是硬依赖（玩家接入的 Mod 依赖 Nest）。
type journalMod struct {
	name    app.ModName
	deps    []app.ModName
	journal *orderJournal
}

func (m *journalMod) Name() app.ModName           { return m.name }
func (m *journalMod) Init(*viper.Viper) error     { return nil }
func (m *journalMod) Provide(*app.Registry) error { return nil }
func (m *journalMod) Start() error                { m.journal.add("start:" + string(m.name)); return nil }
func (m *journalMod) Stop()                       { m.journal.add("stop:" + string(m.name)) }
func (m *journalMod) DependsOn() []app.ModName    { return m.deps }

type journalManager struct{ journal *orderJournal }

func (m *journalManager) Name() string              { return "background" }
func (m *journalManager) Start(*app.Registry) error { m.journal.add("start:manager"); return nil }
func (m *journalManager) Stop()                     { m.journal.add("stop:manager") }

type finishingService struct{}

var errOrderProbeDone = errors.New("order probe done")

func (finishingService) Name() app.ServiceName          { return "game" }
func (finishingService) Init(*app.Registry) error       { return nil }
func (finishingService) Serve(context.Context) error    { return errOrderProbeDone }
func (finishingService) Shutdown(context.Context) error { return nil }

func TestManagerModStartsAfterAndStopsBeforeTheInfrastructureItsManagersUse(t *testing.T) {
	journal := &orderJournal{}
	infrastructure := []app.ModName{
		mods.ModConfigData, mods.ModMongo, mods.ModRedis, mods.ModNats, mods.ModEtcd, mods.ModDataEngine,
		mods.ModNest, mods.ModDataEnginePreload, mods.ModRemoteEntity, mods.ModRemoteEntityPreload, mods.ModSyncBus,
		mods.ModSaga, mods.ModHeartbeat,
	}
	// 生成的 bootstrap 的登记顺序：configdata、mongo、nats、dataengine、etcd、manager、nest、redis……玩家接入在最后。
	registered := []app.Mod{}
	for _, name := range infrastructure[:6] {
		registered = append(registered, &journalMod{name: name, journal: journal})
	}
	registered = append(registered, NewManagerMod(&journalManager{journal: journal}))
	for _, name := range infrastructure[6:] {
		registered = append(registered, &journalMod{name: name, journal: journal})
	}
	registered = append(registered, &journalMod{name: "access.player", deps: []app.ModName{mods.ModNest}, journal: journal})

	dir := t.TempDir()
	a := app.New("roost-test", "0.0.0")
	a.RegisterServer("game", finishingService{}, registered...)
	config := "log:\n  file: false\n  stdout: false\n  dir: " + strconv.Quote(dir) + "\n"
	path := filepath.Join(dir, "config.game.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	a.RootCmd().SetArgs([]string{"game", "--config", path})
	if err := a.Execute(); !errors.Is(err, errOrderProbeDone) {
		t.Fatalf("Execute = %v, want the probe's own error", err)
	}

	managerStart, managerStop := journal.index("start:manager"), journal.index("stop:manager")
	if managerStart < 0 || managerStop < 0 {
		t.Fatalf("the manager did not run: %v", journal.entries)
	}
	for _, name := range infrastructure {
		start, stop := journal.index("start:"+string(name)), journal.index("stop:"+string(name))
		if start > managerStart || stop < managerStop {
			t.Errorf("%s: started at %d / stopped at %d, the manager at %d / %d; the manager has to start after it and stop before it",
				name, start, stop, managerStart, managerStop)
		}
	}
	// 玩家接入（依赖 Nest、登记在 ManagerMod 之后）仍在 manager 之后启动、之前停止：告知、换服受理这类 manager 的能力
	// 在接入打开之前就已发布，停机时接入先停。
	if access := journal.index("start:access.player"); access < managerStart || journal.index("stop:access.player") > managerStop {
		t.Errorf("player access no longer starts after / stops before the manager: %s", fmt.Sprint(journal.entries))
	}
}
