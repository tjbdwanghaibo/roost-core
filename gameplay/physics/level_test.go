package physics

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

func TestLevelContentAndMechanisms(t *testing.T) {
	l := ObstacleLevel()
	data, hash, err := l.Content()
	if err != nil {
		t.Fatal(err)
	}
	copy, err := DecodeLevel(data)
	if err != nil {
		t.Fatal(err)
	}
	again, h, err := copy.Content()
	if err != nil || h != hash || !bytes.Equal(data, again) {
		t.Fatal("unstable content", err)
	}
	c := MotionCommands(l.Bodies, l.Motions, 75)
	if len(c) != 2 || c[0].Position[1] < 1.99 || !c[0].Target || c[1].Rotation[1] < .6 {
		t.Fatalf("platform/rotor: %+v", c)
	}
	changed := l
	changed.Revision++
	_, other, _ := changed.Content()
	if other == hash {
		t.Fatal("revision missing from content identity")
	}
	for _, bad := range [][]byte{append(data, []byte("{}")...), bytes.Replace(data, []byte(`"Version":2`), []byte(`"Version":9`), 1), bytes.Replace(data, []byte(`"Version":2`), []byte(`"Unknown":1,"Version":2`), 1)} {
		if _, err := DecodeLevel(bad); err == nil {
			t.Fatal("invalid content accepted")
		}
	}
	l.Players = []uint64{4}
	if l.Validate() == nil {
		t.Fatal("kinematic controller accepted")
	}
	l = ObstacleLevel()
	l.Motions[0].ID = 1
	if l.Validate() == nil {
		t.Fatal("dynamic target accepted")
	}
	l = ObstacleLevel()
	l.Joints[0].B = 999
	if l.Validate() == nil {
		t.Fatal("missing joint actor")
	}
	l = ObstacleLevel()
	l.Bodies[1].Mass = 1e30
	if l.Validate() == nil {
		t.Fatal("extreme mass accepted")
	}
	l = ObstacleLevel()
	l.Bodies[0].ID = 1
	if l.Validate() == nil {
		t.Fatal("duplicate ID")
	}
}
func TestInitialFrameIncludesMovingBoxes(t *testing.T) {
	f := InitialFrame(1, ObstacleLevel().Bodies)
	seen := map[uint64]bool{}
	for _, v := range f.States {
		seen[v.ID] = true
	}
	if seen[100] || !seen[4] || !seen[5] || !seen[6] {
		t.Fatal(seen)
	}
	if (physx.Body{Shape: physx.Box}).MotionType() != physx.Static {
		t.Fatal("legacy static box changed")
	}
}

func TestSnapshotCarriesSampledInputIdentity(t *testing.T) {
	f := Frame{Epoch: 1, Tick: 2, States: []physx.State{{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}, InputSequence: 7, InputGeneration: 9, InputActive: true}}}
	snapshot, err := Snapshot(f, 25)
	if err != nil {
		t.Fatal(err)
	}
	data := snapshot.Objects[0].Data
	if len(data) != 70 || binary.LittleEndian.Uint64(data[53:]) != 7 || data[61] != 1 || binary.LittleEndian.Uint64(data[62:]) != 9 {
		t.Fatalf("sample identity missing: %v", data)
	}
}

// flowLevel 是一份合法的 schema 2 关卡：掉落区、终点、两个出生点，流程里用到三种物理动作。
func flowLevel() Level {
	l := ObstacleLevel()
	l.Regions = []Region{
		{ID: "kill", Center: physx.Vec3{0, -6, 0}, HalfExtents: physx.Vec3{60, 2, 60}},
		{ID: "goal", Center: physx.Vec3{8, 1, 8}, HalfExtents: physx.Vec3{1, 1, 1}, Rotation: physx.Quat{0, .38268343, 0, .92387953}, Watch: []uint64{1, 2, 3}},
	}
	l.Spawns = []Spawn{
		{ID: "start", Positions: []physx.Vec3{{0, .6, -3}, {1, .6, -3}}},
		{ID: "mid", Positions: []physx.Vec3{{4, .6, 0}}, Rotation: physx.Quat{0, 1, 0, 0}},
	}
	l.DefaultSpawn = "start"
	l.Flow = []byte(flowJSON)
	return l
}

const flowJSON = `{"Version":1,"Name":"obstacles","Revision":1,"TimeDomain":"tick","Initial":["run"],"Stages":[{"ID":"run","Triggers":[` +
	`{"ID":"fall","Event":"region.enter","Conditions":[{"Left":{"EventField":"region"},"Op":"eq","Right":{"Literal":{"Kind":"string","Text":"kill"}}}],"Actions":[{"Kind":"request","Name":"physics_respawn","Args":{"actor":{"EventField":"actor"}}}]},` +
	`{"ID":"join","Event":"member.join","Actions":[{"Kind":"request","Name":"physics_checkpoint","Args":{"actor":{"EventField":"actor"},"spawn":{"Literal":{"Kind":"string","Text":"mid"}}}},{"Kind":"request","Name":"physics_teleport","Args":{"actor":{"EventField":"actor"},"spawn":{"Literal":{"Kind":"string","Text":"start"}}}}]},` +
	`{"ID":"win","Event":"region.enter","Conditions":[{"Left":{"Literal":{"Kind":"string","Text":"goal"}},"Op":"eq","Right":{"EventField":"region"}}],"Actions":[{"Kind":"finish","Result":"clear"}]}]}]}`

// schema 2：合法关卡（区域、出生点、流程）能发布、解码、编译；schema 1、未知字段、越界、重名、
// 引用不存在的刚体、非 tick 流程、未登记的能力与写错的出生点 / 区域名都拒绝。
func TestLevelSchemaTwoRejectsBadRegionsSpawnsAndFlow(t *testing.T) {
	valid := flowLevel()
	data, hash, err := valid.Content()
	if err != nil {
		t.Fatalf("valid schema 2 level refused: %v", err)
	}
	decoded, err := DecodeLevel(data)
	if err != nil {
		t.Fatalf("published schema 2 level does not decode: %v", err)
	}
	if again, h, err := decoded.Content(); err != nil || h != hash || !bytes.Equal(again, data) {
		t.Fatalf("schema 2 content is not stable: %v", err)
	}
	if len(decoded.Regions) != 2 || decoded.Regions[1].Watch[2] != 3 || len(decoded.Spawns) != 2 || decoded.DefaultSpawn != "start" || len(decoded.Flow) == 0 {
		t.Fatalf("schema 2 fields lost in the round trip: %+v", decoded)
	}
	if program, err := decoded.Program(level.Registry{}); err != nil || program == nil {
		t.Fatalf("valid flow does not compile: %v", err)
	}
	// 区域、出生点、流程都参与内容摘要：改任何一个都是另一份关卡。
	for name, mutate := range map[string]func(*Level){
		"region": func(l *Level) { l.Regions[0].HalfExtents[1] = 3 },
		"spawn":  func(l *Level) { l.Spawns[1].Positions[0][0] = 5 },
		"flow":   func(l *Level) { l.Flow = []byte(strings.Replace(flowJSON, `"Revision":1`, `"Revision":2`, 1)) },
	} {
		changed := flowLevel()
		mutate(&changed)
		if _, other, err := changed.Content(); err != nil || other == hash {
			t.Fatalf("%s is not part of the content identity (%v)", name, err)
		}
	}
	// 没有区域、出生点、流程的关卡仍然合法，Program 为 nil。
	plain := ObstacleLevel()
	plain.Regions, plain.Spawns, plain.DefaultSpawn, plain.Flow = nil, nil, "", nil
	if err := plain.Validate(); err != nil {
		t.Fatalf("level without flow refused: %v", err)
	}
	if program, err := plain.Program(level.Registry{}); err != nil || program != nil {
		t.Fatalf("level without flow must have no program: %v %v", program, err)
	}

	// 内置的示例关卡自己带流程：能通过校验、编译出 Program，导出再读回后内容摘要不变。
	sample := ObstacleLevel()
	if program, err := sample.Program(level.Registry{}); err != nil || program == nil {
		t.Fatalf("sample level flow: %v %v", program, err)
	}
	sampleData, sampleHash, err := sample.Content()
	if err != nil {
		t.Fatal(err)
	}
	if again, err := DecodeLevel(sampleData); err != nil {
		t.Fatal(err)
	} else if _, hashAgain, err := again.Content(); err != nil || hashAgain != sampleHash || len(again.Regions) != 4 || len(again.Spawns) != 3 || again.DefaultSpawn != "start" {
		t.Fatalf("sample level round trip: %v %s != %s", err, hashAgain, sampleHash)
	}

	// 旧版本与未知字段在解码时拒绝。
	for name, bad := range map[string][]byte{
		"schema 1":             bytes.Replace(data, []byte(`"Version":2`), []byte(`"Version":1`), 1),
		"schema 3":             bytes.Replace(data, []byte(`"Version":2`), []byte(`"Version":3`), 1),
		"unknown region field": bytes.Replace(data, []byte(`"Regions":[{`), []byte(`"Regions":[{"Radius":1,`), 1),
		"unknown spawn field":  bytes.Replace(data, []byte(`"Spawns":[{`), []byte(`"Spawns":[{"Slot":1,`), 1),
		"unknown flow field":   bytes.Replace(data, []byte(`"TimeDomain":"tick"`), []byte(`"TimeDomain":"tick","Script":"x"`), 1),
	} {
		if bytes.Equal(bad, data) {
			t.Fatalf("%s: replacement did not apply", name)
		}
		if _, err := DecodeLevel(bad); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}

	many := func(n int) []Region {
		out := make([]Region, n)
		for i := range out {
			out[i] = Region{ID: "r" + strconv.Itoa(i), HalfExtents: physx.Vec3{1, 1, 1}}
		}
		return out
	}
	positions := func(n int) []physx.Vec3 { return make([]physx.Vec3, n) }
	for name, mutate := range map[string]func(*Level){
		"region center out of range":  func(l *Level) { l.Regions[0].Center[0] = 1000.5 },
		"region too large":            func(l *Level) { l.Regions[0].HalfExtents[2] = 100.5 },
		"region too thin":             func(l *Level) { l.Regions[0].HalfExtents[1] = .01 },
		"region negative extent":      func(l *Level) { l.Regions[0].HalfExtents[0] = -2 },
		"region NaN":                  func(l *Level) { l.Regions[0].Center[1] = float32(math.NaN()) },
		"region rotation not unit":    func(l *Level) { l.Regions[1].Rotation = physx.Quat{0, .5, 0, .5} },
		"region duplicate ID":         func(l *Level) { l.Regions[1].ID = "kill" },
		"region empty ID":             func(l *Level) { l.Regions[0].ID = "" },
		"region ID not an identifier": func(l *Level) { l.Regions[0].ID = "kill zone" },
		"region watches static body":  func(l *Level) { l.Regions[1].Watch = []uint64{100} },
		"region watches kinematic":    func(l *Level) { l.Regions[1].Watch = []uint64{4} },
		"region watches missing body": func(l *Level) { l.Regions[1].Watch = []uint64{999} },
		"region watches a body twice": func(l *Level) { l.Regions[1].Watch = []uint64{1, 1} },
		"too many regions":            func(l *Level) { l.Regions = many(MaxRegions + 1) },
		"spawn duplicate ID":          func(l *Level) { l.Spawns[1].ID = "start" },
		"spawn ID not an identifier":  func(l *Level) { l.Spawns[1].ID = "mid/1" },
		"spawn without positions":     func(l *Level) { l.Spawns[1].Positions = nil },
		"spawn too many positions":    func(l *Level) { l.Spawns[1].Positions = positions(MaxSpawnPositions + 1) },
		"spawn position out of range": func(l *Level) { l.Spawns[0].Positions[1][1] = -1000.5 },
		"spawn rotation not unit":     func(l *Level) { l.Spawns[1].Rotation = physx.Quat{0, 2, 0, 0} },
		"default spawn missing":       func(l *Level) { l.DefaultSpawn = "" },
		"default spawn unknown":       func(l *Level) { l.DefaultSpawn = "nowhere" },
		"default spawn without spawns": func(l *Level) {
			l.Spawns = nil
			l.Flow = nil
		},
		"too many spawns": func(l *Level) {
			l.Spawns = nil
			for i := range MaxSpawns + 1 {
				l.Spawns = append(l.Spawns, Spawn{ID: "s" + strconv.Itoa(i), Positions: positions(1)})
			}
			l.DefaultSpawn = "s0"
		},
		"flow in wall-clock time": func(l *Level) {
			l.Flow = []byte(strings.Replace(flowJSON, `"TimeDomain":"tick"`, `"TimeDomain":"millisecond"`, 1))
		},
		"flow is not a definition": func(l *Level) { l.Flow = []byte(`[1,2,3]`) },
		"flow with trailing data":  func(l *Level) { l.Flow = []byte(flowJSON + `{}`) },
	} {
		bad := flowLevel()
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("%s accepted", name)
		}
		if _, _, err := bad.Content(); err == nil {
			t.Fatalf("%s published", name)
		}
	}
	// 直接 Spec 建房（不经过 Level）也用 ValidateRegions：没有玩家名单时，区域必须自己写关注谁。
	crate := []physx.Body{{ID: 7, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}, Mass: 1}}
	unwatched := []Region{{ID: "r", HalfExtents: physx.Vec3{1, 1, 1}}}
	if err := ValidateRegions(crate, nil, unwatched, nil, ""); err == nil {
		t.Fatal("region that watches nobody accepted")
	}
	unwatched[0].Watch = []uint64{7}
	if err := ValidateRegions(crate, nil, unwatched, nil, ""); err != nil {
		t.Fatalf("region with its own watch list refused: %v", err)
	}
	// 上限本身是合法的：拒绝的是“超过”，不是“达到”。
	atLimit := flowLevel()
	atLimit.Regions = append(many(MaxRegions-2), atLimit.Regions...)
	atLimit.Spawns[1].Positions = positions(MaxSpawnPositions)
	if err := atLimit.Validate(); err != nil {
		t.Fatalf("level at the limits refused: %v", err)
	}
	if err := ValidateFlow(append([]byte(flowJSON), bytes.Repeat([]byte(" "), MaxFlowBytes)...)); err == nil {
		t.Fatal("oversized flow accepted")
	}

	// 能力表在编译时核对：未登记的事件 / 动作、参数不对、写死的出生点 / 区域名不存在都拒绝。
	for name, replace := range map[string][2]string{
		"unregistered action":    {`"Name":"physics_respawn"`, `"Name":"physics_explode"`},
		"unregistered event":     {`"Event":"member.join"`, `"Event":"member.dance"`},
		"missing argument":       {`"Name":"physics_respawn","Args":{"actor":{"EventField":"actor"}}`, `"Name":"physics_respawn","Args":{}`},
		"argument of wrong kind": {`"spawn":{"Literal":{"Kind":"string","Text":"mid"}}`, `"spawn":{"Literal":{"Kind":"int","Int":1}}`},
		"unknown checkpoint":     {`"Text":"mid"`, `"Text":"middle"`},
		"unknown teleport spawn": {`"Text":"start"`, `"Text":"begin"`},
		"unknown region (right)": {`"Text":"kill"`, `"Text":"pit"`},
		"unknown region (left)":  {`"Text":"goal"`, `"Text":"gaol"`},
	} {
		bad := flowLevel()
		bad.Flow = []byte(strings.Replace(flowJSON, replace[0], replace[1], 1))
		if bytes.Equal(bad.Flow, []byte(flowJSON)) {
			t.Fatalf("%s: replacement did not apply", name)
		}
		if _, err := bad.Program(level.Registry{}); err == nil {
			t.Fatalf("%s compiled", name)
		}
	}
	// 业务登记的能力可以用；与物理房间的能力重名时拒绝，业务不能改写物理动作的含义。
	business := level.Registry{
		Events:  map[string]map[string]level.ValueKind{"coin.collected": {"actor": level.Integer}},
		Actions: map[string]level.ActionSpec{"grant_reward": {Version: 1, Args: map[string]level.ValueKind{"actor": level.Integer}}},
	}
	withBusiness := flowLevel()
	withBusiness.Flow = []byte(strings.Replace(flowJSON, `{"Kind":"finish","Result":"clear"}`, `{"Kind":"request","Name":"grant_reward","Args":{"actor":{"EventField":"actor"}}},{"Kind":"finish","Result":"clear"}`, 1))
	if _, err := withBusiness.Program(level.Registry{}); err == nil {
		t.Fatal("business action compiled without being registered")
	}
	if _, err := withBusiness.Program(business); err != nil {
		t.Fatalf("registered business action refused: %v", err)
	}
	// 重名时即使声明与物理房间的一模一样也拒绝（否则只是碰巧能编译，执行时不知道归谁）。
	business.Actions[ActionTeleport] = FlowRegistry().Actions[ActionTeleport]
	if _, err := withBusiness.Program(business); err == nil {
		t.Fatal("business action shadowing a physics action accepted")
	}
	delete(business.Actions, ActionTeleport)
	business.Events[EventRegionEnter] = FlowRegistry().Events[EventRegionEnter]
	if _, err := withBusiness.Program(business); err == nil {
		t.Fatal("business event shadowing a physics event accepted")
	}
	delete(business.Events, EventRegionEnter)
	if _, err := withBusiness.Program(business); err != nil {
		t.Fatalf("registry without collisions refused after the checks: %v", err)
	}
	// FlowRegistry 每次给独立副本：改返回值不影响下一次编译。
	FlowRegistry().Actions[ActionRespawn] = level.ActionSpec{}
	if _, err := valid.Program(level.Registry{}); err != nil {
		t.Fatalf("registry copy leaked: %v", err)
	}
}

// 区域与出生点的显示数据与 C# 客户端共用一份金样（packed.json 的 Level，是 Content() 的原样字节）：
// C# 的 LevelMarkers 自己读这份 JSON，这里断言的值就是它要读出来的值。
func TestLevelMarkersGoldenSharedWithCSharp(t *testing.T) {
	l := Level{Version: LevelVersion, Name: "markers", Revision: 3,
		Bodies: []physx.Body{
			{ID: 1, Shape: physx.Sphere, Motion: physx.Dynamic, Position: physx.Vec3{0, .5, 0}, Rotation: physx.Quat{0, 0, 0, 1}, Dimensions: physx.Vec3{.5}, Mass: 1},
			{ID: 2, Shape: physx.Box, Motion: physx.Static, Position: physx.Vec3{0, -.5, 0}, Rotation: physx.Quat{0, 0, 0, 1}, Dimensions: physx.Vec3{20, .5, 20}},
		},
		Players: []uint64{1},
		Regions: []Region{
			{ID: "kill", Center: physx.Vec3{0, -6, 0}, HalfExtents: physx.Vec3{60, 2, 60}},
			{ID: "goal", Center: physx.Vec3{8, 1, 8}, HalfExtents: physx.Vec3{1, 1.5, 2}, Rotation: physx.Quat{0, .38268343, 0, .92387953}, Watch: []uint64{1}},
		},
		Spawns: []Spawn{
			{ID: "start", Positions: []physx.Vec3{{0, .6, -3}, {1, .6, -3}}},
			{ID: "mid", Positions: []physx.Vec3{{4, .6, 0}}, Rotation: physx.Quat{0, 1, 0, 0}},
		},
		DefaultSpawn: "start",
		Flow:         []byte(flowJSON),
	}
	content, _, err := l.Content()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../client/physics-viewer/tests/packed.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct{ Level string }
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if got := base64.StdEncoding.EncodeToString(content); got != golden.Level {
		t.Fatalf("level content differs from the golden shared with C#:\n got %s\nwant %s", got, golden.Level)
	}
	decoded, err := DecodeLevel(content)
	if err != nil || len(decoded.Regions) != 2 || len(decoded.Spawns) != 2 || decoded.DefaultSpawn != "start" ||
		decoded.Regions[1].ID != "goal" || decoded.Regions[1].HalfExtents != (physx.Vec3{1, 1.5, 2}) || decoded.Regions[1].Rotation != (physx.Quat{0, .38268343, 0, .92387953}) ||
		decoded.Regions[0].Rotation != (physx.Quat{}) || decoded.Spawns[0].Positions[1] != (physx.Vec3{1, .6, -3}) || decoded.Spawns[1].Rotation != (physx.Quat{0, 1, 0, 0}) {
		t.Fatalf("decoded %+v %v", decoded, err)
	}
}
