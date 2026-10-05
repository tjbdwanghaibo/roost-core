package roost

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The etcd Discovery key is service_prefix + server_type + "/" + sid with no separator of its own
// (etcd/driver/discovery.go registerOnceWithEtcd; the core default is "/service/"). A generated
// service_prefix without the trailing slash registered "/roost/servicesgame/1300" (2026-10-05
// real-process drill, APP-SINGLETON-LOCK §13 第 5 笔).
func TestGeneratedEtcdServicePrefixSeparatesTheServerType(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: root, Mods: []string{"configdata", "etcd"}}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"configs/service/config.game.yaml", "configs/service/config.game.prod.example.yaml"} {
		prefix := configLeaves(t, rel, readProjectFile(t, root, rel))["etcd.service_prefix"]
		if prefix == "" {
			t.Fatalf("%s: no etcd.service_prefix", rel)
		}
		key := fmt.Sprintf("%s%s/%d", prefix, "game", 1300)
		if !strings.HasSuffix(prefix, "/") || !strings.Contains(key, "/game/1300") {
			t.Errorf("%s: etcd.service_prefix %q registers the key %q, want the server type as its own path segment", rel, prefix, key)
		}
	}
}
