package registry

import (
	"strings"
	"testing"
)

// M-05 · 生成的聚合注册在跑完所有阶段之后校验 entity 注册表。
//
// RegisterAll 是整个工程里唯一知道"注册结束了"的时点:bootstrap 在 app.New 之前调它一次,
// 所有 //roost:register 函数按阶段跑完。校验挂在这里,才能一次列出所有不一致 —— 远程托管的
// kind 没在 remote 档、kind 落在没声明过的 category、kind 占了保留的 unknown 档。
// 挂在别处都只能看到局部。
func TestGeneratedAggregateValidatesTheEntityRegistry(t *testing.T) {
	generated, err := render("example.com/planet", []Registration{
		{ImportPath: "example.com/planet/game/entities/player", Func: "RegisterEntity", Phase: "entity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(generated)

	if !strings.Contains(body, "entity.ValidateEntityRegistry()") {
		t.Fatalf("the aggregate does not validate the entity registry:\n%s", body)
	}
	if !strings.Contains(body, `"github.com/tjbdwanghaibo/roost-core/entity"`) {
		t.Errorf("the aggregate does not import the entity package:\n%s", body)
	}

	// The check has to come after every registration, or it validates a
	// half-populated registry and reports kinds that are merely not there yet.
	lastCall := strings.LastIndex(body, "player.RegisterEntity()")
	validate := strings.Index(body, "entity.ValidateEntityRegistry()")
	if lastCall < 0 || validate < lastCall {
		t.Errorf("validation runs before the registrations finish (call at %d, validate at %d):\n%s", lastCall, validate, body)
	}
}

// A project with no registrations at all still validates, and still compiles:
// an empty registry is consistent, so the call returns nil.
func TestGeneratedAggregateValidatesEvenWithNoRegistrations(t *testing.T) {
	generated, err := render("example.com/planet", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), "entity.ValidateEntityRegistry()") {
		t.Fatalf("an empty project's aggregate skips validation:\n%s", generated)
	}
}
