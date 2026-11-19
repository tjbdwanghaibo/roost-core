package dataengine

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// NewDatabaseMap 合成并冻结本进程的逻辑库 → 物理库表：代码声明的逻辑库（生成的 db/gen_databases.go 在
// init 里登记）、配置 dataengine.databases 与回调 resolver（可为 nil，每个声明的逻辑库调用一次；同一逻辑库只能有
// 一个来源，回调对配置已写的逻辑库返回不同的非空物理名即拒绝启动，返回空串或相同的值沿用配置）。
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
// （与进程 --sid 一致）。入口写了 config.includes 时按 App 启动时的同一条规则叠加（app.LoadConfigFile）。
func LoadDatabaseMapFile(path string, sid int32, resolver coredata.DatabaseResolver) (*coredata.DatabaseMap, error) {
	cfg, _, err := app.LoadConfigFile(path)
	if err != nil {
		return nil, fmt.Errorf("dataengine databases: read %s: %w", path, err)
	}
	if sid != 0 {
		cfg.Set("sid", sid)
	}
	return LoadDatabaseMap(cfg, resolver)
}

// DAODatabaseFile 给直接读 DAO 文档的运维工具（cmd/sidmove players、cmd/schemamigrate）算出 dao 文档所在的
// 物理库名：读服务配置文件（与 LoadDatabaseMapFile 同一条规则），逻辑库换成物理库，dbscope=sid 的 DAO 再加 _<sid>
// （与 DAOCollection、DataEngine 投影同一规则；v1.47.0 起本地实体的 DAO 缺省按服分库）。sid 不为 0 时覆盖文件里的
// sid；DAO 按服分库而两处都没有 sid 时返回错误。
func DAODatabaseFile(path string, sid int32, dao DAOStore, resolver coredata.DatabaseResolver) (string, error) {
	cfg, _, err := app.LoadConfigFile(path)
	if err != nil {
		return "", fmt.Errorf("dataengine databases: read %s: %w", path, err)
	}
	if sid != 0 {
		cfg.Set("sid", sid)
	}
	var keys databasesKeys
	if err := app.LoadConfig(cfg, &keys); err != nil {
		return "", fmt.Errorf("dataengine databases: %w", err)
	}
	databases, err := NewDatabaseMap(keys.Databases, keys.Sid, resolver)
	if err != nil {
		return "", err
	}
	physical := databases.Physical(dao.DbName())
	if coredata.ResolveDatabaseScope(dao) != coredata.DatabaseServer {
		return physical, nil
	}
	if keys.Sid <= 0 {
		return "", fmt.Errorf("dataengine databases: %s/%s is dbscope=sid (one database per server) and %s names no sid; pass the sid", dao.DbName(), dao.CollName(), path)
	}
	return fmt.Sprintf("%s_%d", physical, keys.Sid), nil // 与 mongo 驱动的 DatabaseForSid 同一命名
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

// checkPrefixedDatabases 核对加了 mongo.database_prefix 之后的库名（维护者 2026-11-19 的库名前缀能力）：映射表与
// dataengine.database 都是不带前缀的名字，Mongo Mod 交出的客户端打开时才加 <前缀>_（wiring/mongo.WithDatabasePrefix），
// 所以 Mongo 的 63 字节上限要按加了前缀、按 sid 分库再加 _<sid> 之后的名字算。前缀为空时映射表自己的校验已经覆盖。
func checkPrefixedDatabases(prefix string, entries []coredata.DatabaseMapping, control string, sid int32) error {
	if prefix == "" {
		return nil
	}
	scoped := make(map[string]bool)
	for _, database := range coredata.DeclaredDatabases() {
		scoped[database.Name] = database.ServerScoped
	}
	var errs []error
	for _, entry := range entries {
		name := mods.PrefixedDatabase(prefix, entry.Physical)
		if err := coredata.ValidPhysicalDatabaseName(name, scoped[entry.Logical], sid); err != nil {
			errs = append(errs, fmt.Errorf("logical database %q is %q in Mongo with mongo.database_prefix: %w; shorten the prefix or map it to a shorter name in dataengine.databases", entry.Logical, name, err))
		}
	}
	if control != "" {
		name := mods.PrefixedDatabase(prefix, control)
		if err := coredata.ValidPhysicalDatabaseName(name, false, 0); err != nil {
			errs = append(errs, fmt.Errorf("dataengine.database is %q in Mongo with mongo.database_prefix: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
