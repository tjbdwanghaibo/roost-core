package mods

import (
	"fmt"
	"strings"
)

// MaxMongoDatabasePrefix 是 mongo.database_prefix 的字节上限：Mongo 库名上限 63 字节，给 _<库名>_<sid> 留出 22 字节。
const MaxMongoDatabasePrefix = 40

// MongoDatabasePrefix 是 mongo.database_prefix（维护者 2026-11-19：框架层的库名前缀能力，统一配置在这一个键里；
// 按 roost-genre / 生成器生成的工程写成工程名，同一台机器上共用一个 Mongo 的不同工程不撞库）。Mongo Mod 用它封装交给
// 其他 Mod 的客户端（wiring/mongo.WithDatabasePrefix），DataEngine 用它核对加了前缀的库名长度，两边匿名嵌入同一份声明。
// 业务与框架各处写的库名（dataengine.database、dataengine.databases、saga.database、remote_entity.mongo.database、
// 业务代码里的库名）都不带前缀；前缀为空时它们就是 Mongo 里的库名。
type MongoDatabasePrefix struct {
	DatabasePrefix string `config:"mongo.database_prefix" example:"{project_db}" help:"库名前缀：非空时每个业务库在 Mongo 里是 <前缀>_<库名>（按 sid 分库时 <前缀>_<库名>_<sid>），系统库 admin / local / config 不加；\n为空时库名就是配置与代码里写的名字。生成配置写工程名，同一台机器上共用一个 Mongo 的不同工程因此不撞库。\n各处配置的库名都不带前缀。至多 40 字节，只用字母、数字、_ 与 -；改它等于换库，先迁移数据"`
}

// CheckDatabasePrefix 校验前缀：可以为空；非空时只用字母、数字、_ 与 -，不是系统库名，至多 MaxMongoDatabasePrefix 字节。
func (c MongoDatabasePrefix) CheckDatabasePrefix() error {
	prefix := c.DatabasePrefix
	if prefix == "" {
		return nil
	}
	if strings.TrimFunc(prefix, func(r rune) bool {
		return r == '_' || r == '-' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')
	}) != "" {
		return fmt.Errorf("config: mongo.database_prefix %q: use only letters, digits, _ and -", prefix)
	}
	switch strings.ToLower(prefix) {
	case "admin", "local", "config":
		return fmt.Errorf("config: mongo.database_prefix %q is a Mongo system database name", prefix)
	}
	if len(prefix) > MaxMongoDatabasePrefix {
		return fmt.Errorf("config: mongo.database_prefix %q is %d bytes; at most %d (Mongo database names stop at 63 and <prefix>_<name>_<sid> must fit)", prefix, len(prefix), MaxMongoDatabasePrefix)
	}
	return nil
}

// PrefixedDatabase 是库名前缀的命名规则：prefix 与 name 都非空、name 不是系统库时为 <prefix>_<name>，否则 name 原样。
// Mongo Mod 交出的客户端、直接用驱动的运维工具（wiring/sidmerge）与库名长度核对共用这一处。
func PrefixedDatabase(prefix, name string) string {
	if prefix == "" || name == "" {
		return name
	}
	switch name {
	case "admin", "local", "config":
		return name
	}
	return prefix + "_" + name
}
