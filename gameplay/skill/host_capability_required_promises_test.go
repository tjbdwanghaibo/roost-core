package skill

import (
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// B3 ③ 收尾（v1.23.0 发版前，维护者“交给 review 前不留能绕过检查的分支”）：能力表并入 Host 接口，
// 每个 Host 都必须声明；Runtime 的准入核对不再有“Host 没实现 HostCapabilityProvider 就跳过”的分支
// （docs/feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md §12）。
//
// 修前红（基线 82dfe672，临时用例，未提交）：summon 技能（cost 10 mana）交给两个 Host——
//   - 不声明能力表的 Host：Activate err = skill: host contract violation, mana = 90
//   - RecordingHost 包着一个如实声明“没有召唤物”的 Host：Activate err = skill: host contract violation, mana = 90
//
// 两者都绕过了准入核对，执行到 summon 才由施法中途的类型断言拒绝，费用已扣。

func summonCostingProgram(t *testing.T) *Program {
	t.Helper()
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10}},{"flow":"finish"}]}`
	program, diagnostics := Compile(mustParseJSON(t, hostCapabilitySkill("required", `[{"resource":"mana","amount":10}]`, flow)), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	return program
}

// 能力表是 Host 接口的一部分：不声明能力的 Host 编译不过，Runtime 没有可以跳过核对的 Host。
func TestHostInterfaceRequiresTheCapabilityTable(t *testing.T) {
	host := reflect.TypeOf((*Host)(nil)).Elem()
	provider := reflect.TypeOf((*HostCapabilityProvider)(nil)).Elem()
	if !host.Implements(provider) {
		t.Fatal("skill.Host does not include HostCapabilities(): a Host may omit its capability table and skip admission")
	}
}

// 包装型 Host（RecordingHost / ReplayHost）转发底层 Host 的表：底层如实声明没有召唤物时，
// 包装后同样在准入处被拒，不扣费；底层能力齐全时包装后照常施法，回放与录制一致。
func TestWrappingHostsForwardTheWrappedCapabilities(t *testing.T) {
	program := summonCostingProgram(t)
	environment := DefaultCompileEnvironment()

	inner := runtimeTestHost(environment)
	recording := NewRecordingHost(&hostWithoutOwnedContract{inner: inner})
	_, err := NewRuntime(recording, RuntimeOptions{}).Activate(program, CastInput{Caster: 1})
	if !errors.Is(err, ErrHostCapabilityMissing) || !strings.Contains(err.Error(), "summon") {
		t.Fatalf("RecordingHost over a host without summons: err = %v, want ErrHostCapabilityMissing naming summon", err)
	}
	if mana := inner.ResourceForTest(1, "mana"); mana != 100 {
		t.Fatalf("mana = %d after a refused start, want 100 (nothing paid)", mana)
	}
	replay := NewReplayHost(inner.AuthorityIdentity(), recording.Records())
	_, err = NewRuntime(replay, RuntimeOptions{}).Activate(program, CastInput{Caster: 1})
	if !errors.Is(err, ErrHostCapabilityMissing) || !strings.Contains(err.Error(), "summon") {
		t.Fatalf("ReplayHost of that recording: err = %v, want the same refusal", err)
	}
	if err := replay.AssertComplete(); err != nil {
		t.Fatal(err)
	}

	full := runtimeTestHost(environment)
	recording = NewRecordingHost(&ownedSpawnTestHost{MemoryHost: full})
	if got, want := recording.HostCapabilities(), full.HostCapabilities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecordingHost table = %v, want the wrapped host's %v", got.Items(), want.Items())
	}
}

// 声明了空表（什么都不支持）的 Host：带能力需求的 Program 一律在准入处被拒，点名缺的每一项。
func TestEmptyTableRefusesEveryRequirement(t *testing.T) {
	program := summonCostingProgram(t)
	inner := runtimeTestHost(DefaultCompileEnvironment())
	_, err := NewRuntime(&emptyTableHost{MemoryHost: inner}, RuntimeOptions{}).Activate(program, CastInput{Caster: 1})
	if !errors.Is(err, ErrHostCapabilityMissing) || !strings.Contains(err.Error(), "summon") || !strings.Contains(err.Error(), `resource "mana"`) {
		t.Fatalf("empty table: err = %v, want ErrHostCapabilityMissing naming summon and resource \"mana\"", err)
	}
	if mana := inner.ResourceForTest(1, "mana"); mana != 100 {
		t.Fatalf("mana = %d after a refused start, want 100", mana)
	}
}

// 守卫：skill 的非测试源码里不再有对 HostCapabilityProvider 的类型断言——那正是“没实现就跳过”
// 分支的形状。能力表从 Host 接口直接取。
func TestNoHostCapabilitySkipBranch(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	assertion := regexp.MustCompile(`\.\(\s*HostCapabilityProvider\s*\)`)
	for _, file := range files {
		name := file.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for index, line := range strings.Split(string(data), "\n") {
			if assertion.MatchString(line) {
				t.Errorf("%s:%d asserts HostCapabilityProvider; every Host declares its table, read host.HostCapabilities() directly: %s", name, index+1, strings.TrimSpace(line))
			}
		}
	}
}

// emptyTableHost 声明空表。
type emptyTableHost struct{ *MemoryHost }

func (*emptyTableHost) HostCapabilities() HostCapabilityTable { return HostCapabilityTable{} }
