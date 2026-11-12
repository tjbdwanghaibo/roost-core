package roost

import (
	"testing"
)

// 每次只装一个模块及其必要依赖，不靠 enter 顺便装好 TCP 或自身视图。
func TestModuleClientAccessWithoutEnter(t *testing.T) {
	for _, name := range []string{"equipment", "gacha", "idle"} {
		t.Run(name, func(t *testing.T) {
			root, roost, _ := moduleProject(t, "access"+name)
			roost("module", "add", name)
			manifest, err := LoadManifest(root)
			if err != nil {
				t.Fatal(err)
			}
			access := manifest.Access["player"]
			if !contains(access.Transports, "tcp") || !access.SelfSync {
				t.Fatalf("%s standalone access: %+v", name, access)
			}
			if _, err := checkSelfSync(root, manifest); err != nil {
				t.Fatal(err)
			}
			runGeneratedGo(t, root, "build", "./...")
			roost("id", "check")
			roost("generate", "--check")
		})
	}
}
