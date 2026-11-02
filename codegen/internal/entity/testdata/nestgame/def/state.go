package def

// 本夹具测本地业务/组件/DAO，不把每次心跳变成持久事务；WAL 与网络分别走正式专项。
//
//roost:dao coll=states db=roost_nest_game dbscope=global
type StateDao struct {
	Messages    int64 `dao:"nopersist,nosync" store:"messages"`
	Heartbeats  int64 `dao:"nopersist,nosync" store:"heartbeats"`
	PlannedNS   int64 `dao:"nopersist,sync" store:"planned_ns"`
	ChangedNS   int64 `dao:"nopersist,sync" store:"changed_ns"`
	CommittedNS int64 `dao:"nopersist,sync" store:"committed_ns"`
	X           int64 `dao:"nopersist,sync" store:"x"`
	HP          int64 `dao:"nopersist,sync" store:"hp"`
}
