package def

//roost:redisdao mode=ref-hmap key=Snapshot.InstanceRunID prefix=roost:test:session version=Version ttl=1h
type CacheSession struct {
	Snapshot CacheSnapshot
	Meta     *CacheMeta
	Version  uint64
}

type CacheSnapshot struct {
	InstanceRunID int64
	State         int32
	Inner         CacheInner
}

type CacheInner struct {
	Score int64
}

type CacheMeta struct {
	Label string
}
