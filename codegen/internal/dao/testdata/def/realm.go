package def

// RealmDao is the dbscope=boot shape: the process picks its database scope at
// startup (sid unless SetRealmDaoDbScope says global), so one DAO can be one
// database per server in some deployments and shared in others (game-demo's
// terrain: a map one server owns is sid, a map partitioned across servers is
// global). The golden locks the generated switch; the runtime test checks
// that DbScope and the commit record's key follow it.
//
//roost:dao coll=realms db=game dbscope=boot
type RealmDao struct {
	Owner int64  `store:"1"`
	Name  string `dao:"nosync" store:"2"`
}
