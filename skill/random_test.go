package skill

import (
	"testing"
)

func TestRuntimeRandomSelectionIgnoresCandidateInsertionOrder(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":100},"filters":[{"type":"alive"},{"type":"not_caster"}],"order":{"by":"random","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"enemy","then":{"flow":"effect","effect":{"type":"damage","target":"$local.enemy","amount":1,"damage_type":"physical"}}},"on_empty":{"flow":"finish"}},{"flow":"finish"}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.runtime.random","name":"Runtime","description":"Runtime random.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, json), environment)
	requireNoErrors(t, diagnostics)
	chosen := func(order []EntityID) EntityID {
		host := NewMemoryHost(AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
		host.ConfigureGameplayCatalog(environment.Gameplay)
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100})
		for _, id := range order {
			host.UpsertEntity(MemoryEntity{ID: id, Alive: true, Health: 100, MaxHealth: 100})
		}
		runtime := NewRuntime(host, RuntimeOptions{MatchSeed: fixedTestSeed(8)})
		if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
			t.Fatal(err)
		}
		for _, id := range []EntityID{2, 3, 4} {
			if host.HealthForTest(id) == 99 {
				return id
			}
		}
		t.Fatal("no random target was selected")
		return 0
	}
	first := chosen([]EntityID{2, 3, 4})
	second := chosen([]EntityID{4, 2, 3})
	if first != second {
		t.Fatalf("candidate insertion order changed random choice: %d vs %d", first, second)
	}
}

func TestBoundedRandomIsDeterministicAndBounded(t *testing.T) {
	key := deriveCastRandomKey(fixedTestSeed(1), "digest", 1, 1)
	for bound := uint64(1); bound < 100; bound++ {
		first := boundedRandom(key, []byte("test"), bound)
		second := boundedRandom(key, []byte("test"), bound)
		if first != second || first >= bound {
			t.Fatalf("bound %d produced %d/%d", bound, first, second)
		}
	}
}
