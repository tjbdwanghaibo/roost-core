// Package sidmerge 是别名合服（DESIGN-TODO D15，设计 docs/maintenance/FEATURE-2026-10-20-server-merge-alias.md）
// 的离线数据工具：合服前的检查、按 sid 独占数据的迁移与回退、合服清单。game-demo 的 cmd/sidmerge 是它的命令行，
// roost project doctor 的 merge-readiness 读它的清单。
//
// 合服把服 A 并入服 B：account 记录“A 并入 B”（accountctl merge-server），A 的角色由 B 的 game 进程承载；
// 本包负责 account 之外、跟着 sid 走的数据。它只在停服窗口里运行：A、B 两个 game 进程都已正常停机。
//
// 每一侧的事实都从那一侧部署用的服务配置读（Server.Config），与 game 进程同一份声明、同一条规则：逻辑库 → 物理库
// 经 kitdataengine.LoadDatabaseMap（D9 映射，一次只换算一个 sid，所以两侧各算各的），WAL 目录经
// kitdataengine.WALDir，控制库经 kitdataengine.EffectSettings，saga 存储经 kitsaga.LoadLocations，总线经
// kitnats.LoadBusPersistence。逻辑库清单是本进程链接的 DAO 包在 init 里登记的（dataengine.DeclaredDatabases），
// 所以命令行要在工程里编译、导入工程的 db 包。
//
// 本包不改 game 进程、不改 Nest / DataEngine / Sync，也不实现需要业务语义的合并（排行、公会重名、活动等）：那些
// 只出现在清单里，由业务处理。
package sidmerge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"
	kitsaga "github.com/tjbdwanghaibo/roost-core/wiring/saga"
	kitsyncbus "github.com/tjbdwanghaibo/roost-core/wiring/syncbus"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"
)

// Server 是合服的一侧：一个 game sid 与它的部署配置。
type Server struct {
	SID int32
	// Config 是这个服部署用的 game 服务配置（例如 configs/service/config.game.yaml，或该服自己的生产配置）。
	// 本包把其中的 sid 设为 SID（与进程 --sid 一致），所以两侧不能共用同一个 *viper.Viper。
	Config *viper.Viper
	// Resolver 与 game 进程注册的 kitdataengine.WithDatabaseResolver 是同一个回调；进程没有注册就是 nil。
	Resolver coredata.DatabaseResolver
	// MongoURI 为空时取配置的 mongo.uri。命令行允许单独给（凭据不写在配置文件里的部署）。
	MongoURI string
	// WALDir 为空时取 kitdataengine.WALDir（dataengine.wal.dir，不写为 data/wal/dataengine/<sid>，相对工作目录）。
	WALDir string
	// WALCheckedElsewhere 表示这个服的 WAL 不在本机，运维已在那台机器上跑过 sidmove wal-drained 且 drained；
	// 检查项记为 confirm（人工确认）而不是 ok。
	WALCheckedElsewhere bool
}

// side 是一侧解析之后的事实。
type side struct {
	sid        int32
	mongoURI   string
	mongo      string // 归一化的 Mongo 地址（主机列表 + 副本集），不含凭据
	databases  *coredata.DatabaseMap
	controlDB  string // dataengine.database：receipts / transactions / outbox
	saga       kitsaga.Locations
	walDir     string
	walChecked bool
	bus        kitnats.BusPersistence
	syncbus    syncbusSettings
	effects    string // dataengine.effects.stream
}

type syncbusSettings struct {
	Transport string
	Stream    string // 实际流名（kitsyncbus.JetStreamStreamFromConfig）
}

// mongoKeys 是本包自己读的唯一一个不属于上面几个 Mod 声明的键：mongo.uri 的值（缺省与 wiring/mongo 的声明相同，
// TestToolKeysMatchModDeclarations 钉住）。
type mongoKeys struct {
	URI string `config:"mongo.uri" default:"mongodb://localhost:27017"`
}

type syncbusKeys struct {
	Transport string `config:"syncbus.transport" default:"nats"`
}

func resolveSide(server Server) (side, error) {
	if server.SID <= 0 {
		return side{}, fmt.Errorf("sidmerge: sid %d is not positive", server.SID)
	}
	if server.Config == nil {
		return side{}, fmt.Errorf("sidmerge: sid %d has no service config", server.SID)
	}
	cfg := server.Config
	cfg.Set("sid", server.SID)
	result := side{sid: server.SID, walChecked: server.WALCheckedElsewhere}
	var mongo mongoKeys
	if err := app.LoadConfig(cfg, &mongo); err != nil {
		return side{}, fmt.Errorf("sidmerge: sid %d: %w", server.SID, err)
	}
	result.mongoURI = strings.TrimSpace(server.MongoURI)
	if result.mongoURI == "" {
		result.mongoURI = mongo.URI
	}
	location, err := mongoLocation(result.mongoURI)
	if err != nil {
		return side{}, fmt.Errorf("sidmerge: sid %d: %w", server.SID, err)
	}
	result.mongo = location
	if result.databases, err = kitdataengine.LoadDatabaseMap(cfg, server.Resolver); err != nil {
		return side{}, fmt.Errorf("sidmerge: sid %d: %w", server.SID, err)
	}
	if result.controlDB, _, result.effects, err = kitdataengine.EffectSettings(cfg); err != nil {
		return side{}, fmt.Errorf("sidmerge: sid %d: %w", server.SID, err)
	}
	result.walDir = server.WALDir
	if result.walDir == "" {
		if result.walDir, err = kitdataengine.WALDir(cfg, server.SID); err != nil {
			return side{}, fmt.Errorf("sidmerge: sid %d: %w", server.SID, err)
		}
	}
	if result.saga, err = kitsaga.LoadLocations(cfg); err != nil {
		return side{}, fmt.Errorf("sidmerge: sid %d: %w", server.SID, err)
	}
	if result.bus, err = kitnats.LoadBusPersistence(cfg); err != nil {
		return side{}, fmt.Errorf("sidmerge: sid %d: %w", server.SID, err)
	}
	var syncbus syncbusKeys
	if err := app.LoadConfig(cfg, &syncbus); err != nil {
		return side{}, fmt.Errorf("sidmerge: sid %d: %w", server.SID, err)
	}
	result.syncbus = syncbusSettings{Transport: syncbus.Transport, Stream: kitsyncbus.JetStreamStreamFromConfig(cfg)}
	return result, nil
}

// mongoLocation 把 Mongo URI 归一化成“同一个 Mongo”的比较键：小写、排序的主机列表加副本集名，去掉凭据与其余选项。
// 同一集群换一个主机名写（DNS 别名、IP）会被当成不同的 Mongo：比较偏保守，结果是拒绝合服，不会把两套 Mongo 当成一套。
func mongoLocation(uri string) (string, error) {
	parsed, err := connstring.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("mongo uri: %w", err)
	}
	hosts := make([]string, 0, len(parsed.Hosts))
	for _, host := range parsed.Hosts {
		hosts = append(hosts, strings.ToLower(host))
	}
	slices.Sort(hosts)
	location := strings.Join(hosts, ",")
	if parsed.ReplicaSet != "" {
		location += "/?replicaSet=" + parsed.ReplicaSet
	}
	return location, nil
}

// serverScopedDatabase 是按 sid 分库的物理库名：与 fmongo.IMongo.DatabaseForSid 相同的 <物理名>_<sid>。
func serverScopedDatabase(physical string, sid int32) string {
	return physical + "_" + strconv.FormatInt(int64(sid), 10)
}

// Plan 是一次合服的两侧与选项。
type Plan struct {
	From, Into Server
	// Declared 是要检查的逻辑库；nil 取本进程登记的 dataengine.DeclaredDatabases()。
	Declared []coredata.LogicalDatabase
	// RemoteManaged 是 remote=managed 实体的集合（"<逻辑库>/<集合>"）。它们的所有权元数据不在本工具范围：
	// 落在按 sid 分库里的照常拷贝文档，在报告里点名，按 Remote 规则另行处理。
	RemoteManaged []string
}

func (plan Plan) declared() []coredata.LogicalDatabase {
	if plan.Declared != nil {
		return plan.Declared
	}
	return coredata.DeclaredDatabases()
}

func (plan Plan) resolve() (from, into side, err error) {
	if plan.From.SID == plan.Into.SID {
		return side{}, side{}, fmt.Errorf("sidmerge: cannot merge sid %d into itself", plan.From.SID)
	}
	if from, err = resolveSide(plan.From); err != nil {
		return side{}, side{}, err
	}
	if into, err = resolveSide(plan.Into); err != nil {
		return side{}, side{}, err
	}
	return from, into, nil
}

// Status 是一项检查的结论：ok 通过；fail 阻止合服；confirm 不阻止，但要运维人工确认后才能继续（runbook 写明怎么确认）。
type Status string

const (
	StatusOK      Status = "ok"
	StatusFail    Status = "fail"
	StatusConfirm Status = "confirm"
)

// Check 是报告里的一项。
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// ErrNotReady 表示前置检查有 fail 项：不迁移、不回退，报告里写明哪一项、为什么。
var ErrNotReady = errors.New("sidmerge: the merge preconditions are not met")

func failed(checks []Check) []string {
	var names []string
	for _, check := range checks {
		if check.Status == StatusFail {
			names = append(names, check.Name)
		}
	}
	return names
}

func notReady(checks []Check) error {
	if names := failed(checks); len(names) > 0 {
		return fmt.Errorf("%w: %s", ErrNotReady, strings.Join(names, ", "))
	}
	return nil
}

func withContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
