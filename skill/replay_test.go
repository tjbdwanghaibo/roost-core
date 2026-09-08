package skill

import "testing"

func TestRecordingAndReplayHostMatchTypedInteractionOrder(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	recording := NewRecordingHost(runtimeTestHost(environment))
	first := NewRuntime(recording, RuntimeOptions{MatchSeed: fixedTestSeed(47)})
	if _, err := first.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	replay := NewReplayHost(recording.host.AuthorityIdentity(), recording.Records())
	second := NewRuntime(replay, RuntimeOptions{MatchSeed: fixedTestSeed(47)})
	if _, err := second.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if err := replay.AssertComplete(); err != nil {
		t.Fatal(err)
	}
}
