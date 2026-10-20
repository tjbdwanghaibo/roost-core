package skills

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
)

// TestSkillCatalogCompiles compiles every game/skills/*.json the way startup
// does. Use the same CompileEnvironment as the service when it is not the
// default one (custom Host capabilities or catalog).
func TestSkillCatalogCompiles(t *testing.T) {
	catalog, err := CompileAll(skill.DefaultCompileEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Programs) == 0 {
		t.Fatal("game/skills has no Skill definitions")
	}
}
