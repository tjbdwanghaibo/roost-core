//go:build integration

package engine

import "testing"

// TestRealMongoPutStoresDollarStringsVerbatim 在真实副本集上跑 checkPutStoresDollarStringsVerbatim：Put 的单条、事务内与批量
// 写法存下的文档必须与 WAL 里的输入逐字段相等，业务字符串以 "$" 开头也不例外。资源：ROOST_DATAENGINE_IT_MONGO_URI 指向的
// 副本集，只用 isolatedRealMongo 给的一次性库，用后删除。
func TestRealMongoPutStoresDollarStringsVerbatim(t *testing.T) {
	ctx, client, database := isolatedRealMongo(t)
	store, err := NewMongoStore(client, MongoStoreConfig{Databases: identityDatabases(t), DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	checkPutStoresDollarStringsVerbatim(t, ctx, store, client, database)
}
