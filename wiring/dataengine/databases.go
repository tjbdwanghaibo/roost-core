package dataengine

import (
	"fmt"
	"log/slog"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// NewDatabaseMap 合成并冻结本进程的逻辑库 → 物理库表：代码声明的逻辑库（生成的 db/gen_databases.go 在
// init 里登记）、配置 dataengine.databases 与回调 resolver（可为 nil，每个声明的逻辑库调用一次，非空结果覆盖配置）。
// 任何一项不合法即返回错误（调用方拒绝启动）；结果与告警写启动日志。
func NewDatabaseMap(configured map[string]string, sid int32, resolver coredata.DatabaseResolver) (*coredata.DatabaseMap, error) {
	databases, warnings, err := coredata.NewDatabaseMap(coredata.DatabaseMapConfig{
		Declared: coredata.DeclaredDatabases(), Configured: configured, Resolver: resolver, ServerID: sid,
	})
	if err != nil {
		return nil, fmt.Errorf("dataengine mod: dataengine.databases: %w", err)
	}
	for _, warning := range warnings {
		slog.Warn("dataengine mod: database mapping", "detail", warning)
	}
	for _, entry := range databases.Entries() {
		slog.Info("dataengine mod: database mapping", "logical", entry.Logical, "physical", entry.Physical, "source", entry.Source)
	}
	return databases, nil
}

// databasesKeys 是工具读映射时用到的键：与 Mod 声明里的同名键是同一份（sid 决定按 sid 分库的长度校验）。
type databasesKeys struct {
	app.ServiceIdentity
	Databases DatabasesConfig `config:"dataengine.databases"`
}

// LoadDatabaseMap 从服务配置读 dataengine.databases 并按与 Mod 相同的规则合成映射表，给运维工具用
// （cmd/sidmove、cmd/schemamigrate 等直接访问 DAO 库的工具都应经它取物理库名）。resolver 应与进程注册的同一个
// （进程没有注册就传 nil）。cfg 为 nil 时只有代码声明，物理名就是逻辑名。
func LoadDatabaseMap(cfg *viper.Viper, resolver coredata.DatabaseResolver) (*coredata.DatabaseMap, error) {
	var keys databasesKeys
	if cfg != nil {
		if err := app.LoadConfig(cfg, &keys); err != nil {
			return nil, fmt.Errorf("dataengine databases: %w", err)
		}
	}
	return NewDatabaseMap(keys.Databases, keys.Sid, resolver)
}

// LoadDatabaseMapFile 读一个服务配置文件（例如 configs/config.game.yaml）里的映射；sid 不为 0 时覆盖文件里的 sid
// （与进程 --sid 一致）。
func LoadDatabaseMapFile(path string, sid int32, resolver coredata.DatabaseResolver) (*coredata.DatabaseMap, error) {
	cfg := viper.New()
	cfg.SetConfigFile(path)
	if err := cfg.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("dataengine databases: read %s: %w", path, err)
	}
	if sid != 0 {
		cfg.Set("sid", sid)
	}
	return LoadDatabaseMap(cfg, resolver)
}

// DAOStore 是 DAO 在 Mongo 里的位置：DbName 是逻辑库（db=），CollName 是集合；按 sid 分库的 DAO 另有 DbScope。
// 生成的 DAO 都满足它（new 一个零值 DAO 即可，例如 db.NewPlayerDao()）。
type DAOStore interface {
	DbName() string
	CollName() string
}

// DAOCollection 打开 DAO 所在的物理集合：逻辑库经冻结的映射表换成物理库，DAO 按 sid 分库时再加 _<sid>
// （与 DataEngine 投影同一规则）。业务与工具直接读写 DAO 文档时用它，不要把 db.<Dao>DBName 当库名。
// databases 来自 mods.ModDataEngineDatabases（进程内）或 LoadDatabaseMapFile（工具）。
func DAOCollection(client fmongo.IMongo, databases *coredata.DatabaseMap, dao DAOStore, sid int32) fmongo.ICollection {
	physical := databases.Physical(dao.DbName())
	if coredata.ResolveDatabaseScope(dao) == coredata.DatabaseServer {
		return client.DatabaseForSid(physical, sid).Collection(dao.CollName())
	}
	return client.Database(physical).Collection(dao.CollName())
}

// Databases 返回进程里 DataEngine Mod 冻结的映射表（mods.ModDataEngineDatabases）；DataEngine Mod 尚未
// Provide 或不在本进程时返回错误。
func Databases(registry *app.Registry) (*coredata.DatabaseMap, error) {
	databases, ok := app.Lookup[*coredata.DatabaseMap](registry, mods.ModDataEngineDatabases)
	if !ok || databases == nil {
		return nil, fmt.Errorf("dataengine databases: capability %q not found (the dataengine Mod provides it)", mods.ModDataEngineDatabases)
	}
	return databases, nil
}
