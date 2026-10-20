package dataengine

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// 逻辑库名与物理库名（DESIGN-TODO D9 修订版 A）。
//
// DAO 标记的 db= 是代码里的逻辑库名（缺省骨架写 main），DocumentKey.Database、WAL 记录与 Remote 提交
// 里的库名都是它；Mongo 里的物理库名只由部署决定：配置 dataengine.databases（逻辑名 → 物理名）或启动装配时
// 注册的 DatabaseResolver，同一个逻辑库只能有一个来源（两者给出不同的值拒绝启动）。进程启动时把二者合成一张
// DatabaseMap 并冻结，之后所有访问 Mongo 业务库的路径（投影、冷加载、墓碑、Remote 存储、运维工具）都经
// DatabaseMap.Physical 取物理名，运行中不再调用回调。
//
// 改映射等于搬数据：先停写、排空 WAL（全部投影确认），离线迁移数据，再改映射启动。WAL 只记逻辑名，
// 上次启动的映射记在 WAL 目录（nestwal.DatabaseMappingFile），有未投影记录而映射变了的进程拒绝启动。

// LogicalDatabase 是生成代码声明的一个逻辑库：Name 是 DAO 标记 db= 的值，ServerScoped 表示至少一个
// DAO 写了 dbscope=sid（物理库名再加 _<sid>，校验长度时要算进去）。
type LogicalDatabase struct {
	Name         string
	ServerScoped bool
}

// DatabaseResolver 是启动装配时注册的映射回调：对每个声明的逻辑库调用一次，返回物理库名；
// 返回空串表示不表态（沿用配置，配置也没有就用逻辑名本身），返回错误则拒绝启动。配置已给出物理名的逻辑库，
// 回调只能返回空串或同一个值，返回不同的值拒绝启动（同一逻辑库只能有一个来源）。只在启动时调用，结果随表冻结。
type DatabaseResolver func(logical string) (string, error)

// mongoDatabaseNameLimit 是 Mongo 库名的字节上限（不含结尾 NUL，即少于 64 字节）。
const mongoDatabaseNameLimit = 63

// logicalDatabaseName 是逻辑库名的写法：小写字母开头，小写字母、数字与下划线。配置键经 viper 读入时
// 一律转成小写，逻辑名若含大写，配置里写的映射就对不上，所以在声明处就限定为小写。
var logicalDatabaseName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidLogicalDatabaseName 校验逻辑库名的写法与长度（同时也必须能直接作物理库名用：未映射时物理名就是它）。
func ValidLogicalDatabaseName(name string) error {
	if !logicalDatabaseName.MatchString(name) {
		return fmt.Errorf("logical database %q: want a lowercase name matching %s", name, logicalDatabaseName)
	}
	return ValidPhysicalDatabaseName(name, false, 0)
}

// ValidPhysicalDatabaseName 校验 Mongo 物理库名：非空、不含 Mongo 禁止的字符、不是系统库
// （admin / local / config），且长度不超过 63 字节；serverScoped 时按本进程 sid 加上 "_<sid>" 后再算长度
// （与 fmongo.IMongo.DatabaseForSid 的命名一致）。
func ValidPhysicalDatabaseName(name string, serverScoped bool, sid int32) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("physical database name is empty")
	}
	if strings.ContainsAny(name, "/\\. \"$*<>:|?\x00") {
		return fmt.Errorf("physical database %q contains a character Mongo does not allow in database names (/\\. \"$*<>:|? or NUL)", name)
	}
	switch strings.ToLower(name) {
	case "admin", "local", "config":
		return fmt.Errorf("physical database %q is a Mongo system database", name)
	}
	full := name
	if serverScoped {
		full = name + "_" + strconv.FormatInt(int64(sid), 10)
	}
	if len(full) > mongoDatabaseNameLimit {
		if serverScoped {
			return fmt.Errorf("physical database %q is %d bytes with the _<sid> suffix (%q); Mongo allows %d", name, len(full), full, mongoDatabaseNameLimit)
		}
		return fmt.Errorf("physical database %q is %d bytes; Mongo allows %d", name, len(full), mongoDatabaseNameLimit)
	}
	return nil
}

// 声明表：生成的 gen_databases.go 在 init 里调用 DeclareDatabases，进程链接了哪些 DAO 包，就声明了哪些
// 逻辑库。它是代码事实（与实体 kind 的登记一样是进程级的），不含任何部署信息。
var declaredDatabases struct {
	mu    sync.Mutex
	names map[string]bool // 逻辑名 → 是否有 DAO 按 sid 分库
}

// DeclareDatabases 登记逻辑库；同名重复登记合并（任一处按 sid 分库即为按 sid 分库）。名字不合法时 panic：
// 它只在生成代码的 init 里调用，写错的 db= 应当在进程启动前就让构建失败。
func DeclareDatabases(databases ...LogicalDatabase) {
	declaredDatabases.mu.Lock()
	defer declaredDatabases.mu.Unlock()
	if declaredDatabases.names == nil {
		declaredDatabases.names = make(map[string]bool)
	}
	for _, database := range databases {
		if err := ValidLogicalDatabaseName(database.Name); err != nil {
			panic(fmt.Errorf("dataengine: declare databases: %w", err))
		}
		declaredDatabases.names[database.Name] = declaredDatabases.names[database.Name] || database.ServerScoped
	}
}

// DeclaredDatabases 返回本进程已声明的逻辑库，按名字排序。
func DeclaredDatabases() []LogicalDatabase {
	declaredDatabases.mu.Lock()
	defer declaredDatabases.mu.Unlock()
	out := make([]LogicalDatabase, 0, len(declaredDatabases.names))
	for _, name := range slices.Sorted(maps.Keys(declaredDatabases.names)) {
		out = append(out, LogicalDatabase{Name: name, ServerScoped: declaredDatabases.names[name]})
	}
	return out
}

// DatabaseMapConfig 是合成映射表的输入。
type DatabaseMapConfig struct {
	// Declared 是代码声明的逻辑库（通常是 DeclaredDatabases()）。
	Declared []LogicalDatabase
	// Configured 是配置 dataengine.databases：逻辑名 → 物理名。
	Configured map[string]string
	// Resolver 可为 nil；非 nil 时对 Declared 里每个逻辑库各调用一次。
	Resolver DatabaseResolver
	// ServerID 是本进程 sid，校验按 sid 分库的物理名长度时用。
	ServerID int32
}

// DatabaseMapping 是表里的一项。Source 说明物理名来自哪里：config、resolver，或 identity（没有映射，
// 物理名就是逻辑名）。
type DatabaseMapping struct {
	Logical  string
	Physical string
	Source   string
}

const (
	DatabaseSourceConfig   = "config"
	DatabaseSourceResolver = "resolver"
	DatabaseSourceIdentity = "identity"
)

// DatabaseMap 是冻结的逻辑库 → 物理库表。构造后只读，可并发使用；nil 表示没有任何映射（物理名就是逻辑名）。
type DatabaseMap struct {
	entries  []DatabaseMapping // 按逻辑名排序
	physical map[string]string
}

// ValidateDatabaseConfig 只校验配置本身（不调用回调，所以 --check-config 与 doctor 发现不了回调与配置的冲突，
// 冲突只在 Init 合成映射表时拒绝）：配置里每个逻辑名都必须是代码声明过的（拼写检查），
// 物理名合法，且两个逻辑库不能映射到同一个物理库。代码一个逻辑库都没声明时（工程还没有 DAO，或这个
// 二进制没有链接 DAO 包）无从判断拼写，未知名字不报错——NewDatabaseMap 会把它们记成告警交给调用方。
// 它被配置声明的 ValidateConfig 调用，所以 --check-config 与 doctor 走的是同一份规则。
func ValidateDatabaseConfig(declared []LogicalDatabase, configured map[string]string, sid int32) error {
	_, _, err := buildDatabaseEntries(DatabaseMapConfig{Declared: declared, Configured: configured, ServerID: sid}, false)
	return err
}

// NewDatabaseMap 合成并冻结映射表。每个逻辑库的物理名只有一个来源：配置，或回调返回的非空物理名；
// 二者都没有时是逻辑名本身（identity）。回调对配置已写的逻辑库返回空串或相同的值时沿用配置，返回不同的值
// 是冲突（不让其中一方静默覆盖另一方）。任何一项不合法、配置里出现未声明的逻辑名、回调与配置冲突、
// 两个逻辑库落到同一个物理库，或回调返回错误，都返回错误，调用方应拒绝启动。warnings 是不阻止启动但运维
// 应当知道的事（未声明逻辑库的配置无从校验、声明的逻辑库没有映射而直接用逻辑名）。
func NewDatabaseMap(cfg DatabaseMapConfig) (*DatabaseMap, []string, error) {
	entries, warnings, err := buildDatabaseEntries(cfg, true)
	if err != nil {
		return nil, nil, err
	}
	databases := &DatabaseMap{entries: entries, physical: make(map[string]string, len(entries))}
	for _, entry := range entries {
		databases.physical[entry.Logical] = entry.Physical
	}
	return databases, warnings, nil
}

func buildDatabaseEntries(cfg DatabaseMapConfig, resolve bool) ([]DatabaseMapping, []string, error) {
	var errs []error
	var warnings []string
	declared := make(map[string]bool, len(cfg.Declared))
	for _, database := range cfg.Declared {
		if err := ValidLogicalDatabaseName(database.Name); err != nil {
			errs = append(errs, err)
			continue
		}
		declared[database.Name] = declared[database.Name] || database.ServerScoped
	}
	for _, logical := range slices.Sorted(maps.Keys(cfg.Configured)) {
		if _, ok := declared[logical]; ok {
			continue
		}
		if len(declared) == 0 {
			// 没有任何声明时不能判断这是拼写错误还是工程尚未写 DAO；映射照常入表、照常校验物理名。
			warnings = append(warnings, fmt.Sprintf("dataengine.databases.%s: no DAO in this binary declares a logical database, so the name cannot be checked", logical))
			if err := ValidLogicalDatabaseName(logical); err != nil {
				errs = append(errs, fmt.Errorf("dataengine.databases.%s: %w", logical, err))
			}
			continue
		}
		errs = append(errs, fmt.Errorf("dataengine.databases.%s: no DAO declares logical database %q (declared: %s); check the spelling against the DAO markers' db=", logical, logical, strings.Join(slices.Sorted(maps.Keys(declared)), ", ")))
	}
	names := make(map[string]bool, len(declared)+len(cfg.Configured))
	maps.Copy(names, declared)
	for logical := range cfg.Configured {
		if _, ok := names[logical]; !ok {
			names[logical] = false
		}
	}
	entries := make([]DatabaseMapping, 0, len(names))
	for _, logical := range slices.Sorted(maps.Keys(names)) {
		entry := DatabaseMapping{Logical: logical, Physical: logical, Source: DatabaseSourceIdentity}
		if physical, ok := cfg.Configured[logical]; ok {
			entry.Physical, entry.Source = strings.TrimSpace(physical), DatabaseSourceConfig
		}
		if _, isDeclared := declared[logical]; resolve && isDeclared && cfg.Resolver != nil {
			physical, err := cfg.Resolver(logical)
			if err != nil {
				errs = append(errs, fmt.Errorf("database resolver for logical database %q: %w", logical, err))
				continue
			}
			physical = strings.TrimSpace(physical)
			switch {
			case physical == "" || (entry.Source == DatabaseSourceConfig && physical == entry.Physical):
				// 回调不表态，或与配置一致：沿用现有结果。
			case entry.Source == DatabaseSourceConfig:
				errs = append(errs, fmt.Errorf("logical database %q: dataengine.databases gives %q but the database resolver returns %q; a logical database takes its physical name from one source only (drop it from dataengine.databases, or have the resolver return \"\" or the same name)", logical, entry.Physical, physical))
				continue
			default:
				entry.Physical, entry.Source = physical, DatabaseSourceResolver
			}
		}
		if _, isDeclared := declared[logical]; isDeclared && entry.Source == DatabaseSourceIdentity {
			warnings = append(warnings, fmt.Sprintf("logical database %q has no mapping in dataengine.databases; using %q as the physical database", logical, logical))
		}
		if err := ValidPhysicalDatabaseName(entry.Physical, names[logical], cfg.ServerID); err != nil {
			errs = append(errs, fmt.Errorf("logical database %q (%s): %w", logical, entry.Source, err))
			continue
		}
		entries = append(entries, entry)
	}
	// 两个逻辑库落到同一个物理库会让同名集合的文档混在一起，改其中一个映射时也无法单独搬走；不提供允许开关。
	owner := make(map[string]string, len(entries))
	for _, entry := range entries {
		key := strings.ToLower(entry.Physical) // Mongo 不允许只差大小写的两个库并存
		if other, ok := owner[key]; ok {
			errs = append(errs, fmt.Errorf("logical databases %q and %q both map to physical database %q; give each its own database", other, entry.Logical, entry.Physical))
			continue
		}
		owner[key] = entry.Logical
	}
	if err := errors.Join(errs...); err != nil {
		return nil, nil, err
	}
	return entries, warnings, nil
}

// Physical 返回逻辑库的物理库名（按 sid 分库时调用方再经 DatabaseForSid 加 _<sid>）。表里没有的名字
// （没有声明也没有配置，例如手写的 DAO）物理名就是逻辑名本身；空串原样返回，由调用方取各自的缺省库。
func (m *DatabaseMap) Physical(logical string) string {
	if m == nil {
		return logical
	}
	if physical, ok := m.physical[logical]; ok {
		return physical
	}
	return logical
}

// Entries 返回表的全部项（按逻辑名排序的副本），用于启动日志、持久到 WAL 目录与工具输出。
func (m *DatabaseMap) Entries() []DatabaseMapping {
	if m == nil {
		return nil
	}
	return slices.Clone(m.entries)
}

// Physicals 返回逻辑名 → 物理名（含 identity 项），用于与上次启动的映射比较。
func (m *DatabaseMap) Physicals() map[string]string {
	out := make(map[string]string)
	if m == nil {
		return out
	}
	maps.Copy(out, m.physical)
	return out
}
