package dbdef

// PlayerDao: the saga e2e's wallet. db=main is the logical database; the
// test maps it to a unique physical database per run (dataengine.databases).
//
//roost:dao coll=player db=main
type PlayerDao struct {
	Gold  int64 `bson:"gold" dao:"persist"`
	Level int32 `bson:"level" dao:"persist"`
	// SagaReceipts 是 saga 原生步骤的回执（roost add saga -native 加入；本覆盖文件替换整份 DAO 定义，所以照抄）。
	SagaReceipts map[string][]byte `bson:"saga_receipts" dao:"persist"`
}
