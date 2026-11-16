package entity

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// 实体事件登记（FEATURE-2026-11-16-tx-event §4）：夹具 testdata/viewevent 的 Player 挂了实体包里的 quest 组件（类型号 2000）
// 与组件包里的 bag / profile / attribute / task / activity / wallet（2001～2006），Guild 也挂 activity。

var registeredListener = regexp.MustCompile(`entity\.RegisterEventListener\(EntityKind\w+, "(\w+)", "([\w.]+)",`)

func registeredOrder(t *testing.T, source string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, match := range registeredListener.FindAllStringSubmatch(source, -1) {
		out = append(out, [2]string{match[1], match[2]})
	}
	return out
}

// 登记顺序就是执行顺序：priority 大的先（负数排在缺省 0 之后）；同优先级实体自己的方法先，再按组件类型号升序
// （实体包里的组件与组件包里的组件一起排）。挂在两种实体上的组件在两边各登记一次。
func TestEventListenersAreRegisteredInExecutionOrder(t *testing.T) {
	root := copyFixture(t, "viewevent")
	out, err := runEntities(t, root)
	if err != nil {
		t.Fatal(err)
	}
	player := readProjectFile(t, root, "game/entities/player/player_event_gen.go")
	want := [][2]string{
		{"ProfileLevelUp", "attribute.Component.OnProfileLevelUp"},   // priority=100
		{"ProfileLevelUp", "player.Player.OnProfileLevelUp"},         // 0，实体自己的
		{"ProfileLevelUp", "player.QuestComponent.OnProfileLevelUp"}, // 0，类型号 2000
		{"ProfileLevelUp", "task.Component.OnProfileLevelUp"},        // 0，类型号 2004
		{"ProfileLevelUp", "activity.Component.OnProfileLevelUp"},    // priority=-1
		{"ProfileRenaming", "player.Player.OnProfileRenaming"},
		{"TaskCompleted", "profile.Component.OnTaskCompleted"},
	}
	if got := registeredOrder(t, player); !reflect.DeepEqual(got, want) {
		t.Errorf("Player registration order\n got %v\nwant %v", got, want)
	}
	for _, fragment := range []string{
		"// priority=100\n\tentity.RegisterEventListener(EntityKindPlayer, \"ProfileLevelUp\", \"attribute.Component.OnProfileLevelUp\",\n\t\tfunc(e *Player, ev event.ProfileLevelUp) error { return e.AttributeComp().OnProfileLevelUp(ev) })",
		"func(e *Player, ev event.ProfileLevelUp) error { return e.OnProfileLevelUp(ev) })",
		"func(e *Player, ev event.ProfileLevelUp) error { return e.QuestComp().OnProfileLevelUp(ev) })",
		"// priority=-1\n",
	} {
		if !strings.Contains(player, fragment) {
			t.Errorf("player_event_gen.go lacks %q:\n%s", fragment, player)
		}
	}
	guild := readProjectFile(t, root, "game/entities/guild/guild_event_gen.go")
	if got := registeredOrder(t, guild); !reflect.DeepEqual(got, [][2]string{{"ProfileLevelUp", "activity.Component.OnProfileLevelUp"}}) {
		t.Errorf("Guild registration = %v, want the shared activity listener once", got)
	}
	if !strings.Contains(guild, "func(e *Guild, ev event.ProfileLevelUp) error { return e.ActivityComp().OnProfileLevelUp(ev) })") {
		t.Errorf("guild_event_gen.go does not call through the Guild's getter:\n%s", guild)
	}
	// 工厂注入分发：Publish 经它按 (kind, 事件类型) 找监听。
	if wire := readProjectFile(t, root, "game/entities/player/player_gen_wire.go"); !strings.Contains(wire, "e.EntityBase.SetEventDispatch(func(ev any) error { return entity.DispatchEvent(EntityKindPlayer, e, ev) })") {
		t.Errorf("the factory does not inject the event dispatch:\n%s", wire)
	}
	// 只提示：没有实体挂载的组件上的监听。
	if !strings.Contains(out, "warning: ") || !strings.Contains(out, "component orphan.Component listens to ProfileLevelUp but no entity mounts it") {
		t.Errorf("no warning for the unmounted component's listener:\n%s", out)
	}
}

// 静态环提示：task 的监听经同一接收者的 complete() 发布 TaskCompleted，profile 的 TaskCompleted 监听再发布
// ProfileLevelUp，成环；断开后不再提示。只提示，不报错。
func TestEventCycleIsHintedNotRefused(t *testing.T) {
	root := copyFixture(t, "viewevent")
	out, err := runEntities(t, root)
	if err != nil {
		t.Fatal(err)
	}
	want := "hint: event cycle on Player: ProfileLevelUp → TaskCompleted → ProfileLevelUp (listeners task.Component.OnProfileLevelUp, profile.Component.OnTaskCompleted)"
	if !strings.Contains(out, want) {
		t.Errorf("output lacks the cycle hint %q:\n%s", want, out)
	}
	source := readProjectFile(t, root, "game/components/task/task.go")
	writeProjectFile(t, root, "game/components/task/task.go", strings.Replace(source, "return c.host.Publish(event.TaskCompleted{ID: id})", "return nil", 1))
	if out, err = runEntities(t, root); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "event cycle") {
		t.Errorf("a broken cycle is still hinted:\n%s", out)
	}
}

func TestElementaryCycles(t *testing.T) {
	edges := map[string]map[string][]string{
		"A": {"B": nil, "A": nil},
		"B": {"C": nil, "A": nil},
		"C": {"A": nil},
		"D": {"A": nil},
	}
	want := [][]string{{"A"}, {"A", "B"}, {"A", "B", "C"}}
	if got := elementaryCycles(edges); !reflect.DeepEqual(got, want) {
		t.Errorf("cycles = %v, want %v", got, want)
	}
}

// §4.3 的每种报错各一例，加上冷组件上的监听、标记不在方法的文档注释里、priority 的取值与未知选项。
func TestListenMarkerErrors(t *testing.T) {
	const header = "package bag\n\nimport \"example.com/g/game/event\"\n\nvar _ = event.ProfileLevelUp{}\n\n"
	for _, tc := range []struct {
		name, rel, source, want string
	}{
		{"pointer event", "game/components/bag/bad.go", header + "//roost:listen\nfunc (c *Component) OnLevel(ev *event.ProfileLevelUp) error { return nil }\n", "the event is passed by pointer"},
		{"two parameters", "game/components/bag/bad.go", header + "//roost:listen\nfunc (c *Component) OnLevel(ev event.ProfileLevelUp, n int) error { return nil }\n", "exactly one parameter"},
		{"not an event", "game/components/bag/bad.go", header + "//roost:listen\nfunc (c *Component) OnLevel(n int64) error { return nil }\n", "not a type from game/event"},
		{"unknown event", "game/components/bag/bad.go", header + "//roost:listen\nfunc (c *Component) OnLevel(ev event.Missing) error { return nil }\n", "event.Missing is not an exported struct"},
		{"wrong result", "game/components/bag/bad.go", header + "//roost:listen\nfunc (c *Component) OnLevel(ev event.ProfileLevelUp) bool { return true }\n", "must return exactly one error"},
		{"function", "game/components/bag/bad.go", header + "//roost:listen\nfunc OnLevel(ev event.ProfileLevelUp) error { return nil }\n", "//roost:listen on function OnLevel"},
		{"not a component", "game/components/bag/bad.go", header + "type helper struct{}\n\n//roost:listen\nfunc (h *helper) OnLevel(ev event.ProfileLevelUp) error { return nil }\n", "neither an entity nor a component mounted by an entity"},
		{"twice per event", "game/components/bag/bad.go", header + "//roost:listen\nfunc (c *Component) OnLevelA(ev event.ProfileLevelUp) error { return nil }\n\n//roost:listen\nfunc (c *Component) OnLevelB(ev event.ProfileLevelUp) error { return nil }\n", "listens to ProfileLevelUp again"},
		{"priority not integer", "game/components/bag/bad.go", header + "//roost:listen priority=high\nfunc (c *Component) OnLevel(ev event.ProfileLevelUp) error { return nil }\n", "priority=high"},
		{"priority out of range", "game/components/bag/bad.go", header + "//roost:listen priority=1001\nfunc (c *Component) OnLevel(ev event.ProfileLevelUp) error { return nil }\n", "want an integer from -1000 to 1000"},
		{"unknown option", "game/components/bag/bad.go", header + "//roost:listen order=1\nfunc (c *Component) OnLevel(ev event.ProfileLevelUp) error { return nil }\n", `unknown marker option "order"`},
		{"not on a method", "game/components/bag/bad.go", header + "//roost:listen\ntype helper struct{}\n", "must sit in the doc comment of a method"},
		{"entity method", "game/entities/player/bad.go", "package player\n\nimport \"example.com/g/game/event\"\n\n//roost:listen\nfunc (e *Player) OnLevel(ev event.ProfileLevelUp, extra int) error { return nil }\n", "exactly one parameter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyFixture(t, "viewevent")
			writeProjectFile(t, root, tc.rel, tc.source)
			_, err := runEntities(t, root)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "bad.go:") {
				t.Fatalf("err = %v, want %q with its position", err, tc.want)
			}
		})
	}

	// 冷组件可能不在实体上，监听不登记，改成报错。
	root := copyFixture(t, "viewevent")
	source := readProjectFile(t, root, "game/entities/guild/entity.go")
	writeProjectFile(t, root, "game/entities/guild/entity.go", strings.Replace(source, "`comp:\"activity.CompTypeActivity\"`", "`comp:\"activity.CompTypeActivity,cold\"`", 1))
	source = readProjectFile(t, root, "game/entities/player/entity.go")
	source = strings.Replace(source, "\tactivity  *activity.Component  `comp:\"activity.CompTypeActivity\"`\n", "", 1)
	source = strings.Replace(source, "\t\"example.com/g/game/components/activity\"\n", "", 1)
	writeProjectFile(t, root, "game/entities/player/entity.go", source)
	if _, err := runEntities(t, root); err == nil || !strings.Contains(err.Error(), "listens on a cold component") {
		t.Fatalf("listener on a cold component = %v", err)
	}
}
