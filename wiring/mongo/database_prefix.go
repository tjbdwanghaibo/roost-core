package mongo

import (
	"fmt"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// WithDatabasePrefix 把客户端封装成按库名前缀打开业务库的客户端（mongo.database_prefix，规则见 mods.PrefixedDatabase）：
// Database(name) 打开 <prefix>_<name>，DatabaseForSid(base, sid) 打开 <prefix>_<base>_<sid>，系统库原样，其余方法
// 原样转发。前缀是这一层的使用能力，Mongo 驱动与 infra 的客户端不知道它。prefix 为空时原样返回 client。
func WithDatabasePrefix(client fmongo.IMongo, prefix string) fmongo.IMongo {
	if prefix == "" || client == nil {
		return client
	}
	return prefixedMongo{IMongo: client, prefix: prefix}
}

type prefixedMongo struct {
	fmongo.IMongo
	prefix string
}

func (m prefixedMongo) Database(name string) fmongo.IDatabase {
	return m.IMongo.Database(mods.PrefixedDatabase(m.prefix, name))
}

// DatabaseForSid 与 fmongo 客户端的按 sid 命名相同（<base>_<sid>），再加前缀。
func (m prefixedMongo) DatabaseForSid(base string, sid int32) fmongo.IDatabase {
	return m.Database(fmt.Sprintf("%s_%d", base, sid))
}

// DatabasePrefix 按 Mongo Mod 的声明读 mongo.database_prefix（含校验），不建连接。直接用驱动的运维工具
// （wiring/sidmerge）用它与 mods.PrefixedDatabase 算出与 game 进程相同的库名。
func DatabasePrefix(cfg *viper.Viper) (string, error) {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return "", fmt.Errorf("mongo: %w", err)
	}
	return settings.DatabasePrefix, nil
}
