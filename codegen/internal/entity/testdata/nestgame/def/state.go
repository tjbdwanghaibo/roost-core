package def

// 本夹具测本地业务/组件/DAO，不把每次心跳变成持久事务；WAL 与网络分别走正式专项。
//
//roost:dao coll=states db=roost_nest_game dbscope=global
type StateDao struct {
	Messages    int64 `dao:"nopersist,nosync"`
	Heartbeats  int64 `dao:"nopersist,nosync"`
	PlannedNS   int64 `dao:"nopersist,sync"`
	ChangedNS   int64 `dao:"nopersist,sync"`
	CommittedNS int64 `dao:"nopersist,sync"`
	X           int64 `dao:"nopersist,sync"`
	HP          int64 `dao:"nopersist,sync"`
}
