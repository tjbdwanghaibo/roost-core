package sidmerge

import (
	"testing"

	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
)

// 合服直接用驱动客户端，不经 Mongo Mod 交出的带前缀客户端，所以库名取映射表的实际库名（Effective：映射库名再加
// mongo.database_prefix），与 game 进程打开的是同一个库。停机 WAL 核对比的是 databases.json 里的映射库名（不带前缀）。
func TestSidePhysicalAddsTheMongoDatabasePrefix(t *testing.T) {
	for prefix, want := range map[string]string{"proj": "proj_game", "": "game"} {
		databases, err := kitdataengine.NewDatabaseMap(map[string]string{"main": "game"}, prefix, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		s := side{dbPrefix: prefix, databases: databases}
		if got := s.physical("main"); got != want {
			t.Errorf("prefix %q: physical(main) = %q, want %q", prefix, got, want)
		}
		if got := s.databases.Physicals()["main"]; got != "game" {
			t.Errorf("prefix %q: Physicals()[main] = %q, want game (what walCheck compares with databases.json)", prefix, got)
		}
	}
}
