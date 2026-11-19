package sidmerge

import (
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
)

// 合服直接用驱动客户端，不经 Mongo Mod 交出的带前缀客户端，所以库名在 side.physical 里按同一规则加 mongo.database_prefix。
func TestSidePhysicalAddsTheMongoDatabasePrefix(t *testing.T) {
	databases, _, err := coredata.NewDatabaseMap(coredata.DatabaseMapConfig{Configured: map[string]string{"main": "game"}})
	if err != nil {
		t.Fatal(err)
	}
	for prefix, want := range map[string]string{"proj": "proj_game", "": "game"} {
		if got := (side{dbPrefix: prefix, databases: databases}).physical("main"); got != want {
			t.Errorf("prefix %q: physical(main) = %q, want %q", prefix, got, want)
		}
	}
}
