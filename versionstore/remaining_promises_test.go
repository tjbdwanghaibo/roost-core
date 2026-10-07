package versionstore

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// 每两次丢失发送后让竞争者写入下一版本，逼出「重发 × CAS 重试」的组合路径。
type contendedLostReplies struct {
	*fakeRedis
	sends int
}

func (f *contendedLostReplies) Eval(_ context.Context, _ string, keys []string, args ...any) (any, error) {
	f.sends++
	if f.sends%3 == 0 {
		header, err := parseEnvelope(f.values[keys[0]])
		if err != nil {
			return nil, err
		}
		f.values[keys[0]] = buildEnvelope(header.payload, header.version+1, newWriteToken(), header.tokens, 8)
	}
	return nil, errLostReply
}

func TestResendsAndCASRacesShareOneSendBudget(t *testing.T) {
	base := newFakeRedis()
	base.values["budget:a"] = buildEnvelope([]byte(`{"Name":"a","Total":0}`), 1, newWriteToken(), nil, 8)
	client := &contendedLostReplies{fakeRedis: base}
	store, err := NewRedisStore(client, RedisConfig[string, counter]{Prefix: "budget:", KeyOf: func(k string) string { return k }, Codec: JSONCodec[counter]{}, MaxAttempts: 3, RetryBackoff: -1})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Update(context.Background(), "a", increment)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("known lost race = %v", err)
	}
	if client.sends != 3 {
		t.Fatalf("one Update sent %d commands, budget=3", client.sends)
	}
}

func TestResumedUpdateCountsItsResolvedCAS(t *testing.T) {
	old := metrics.DefaultRegistry()
	reg := metrics.NewRegistry()
	metrics.SetDefaultRegistry(reg)
	t.Cleanup(func() { metrics.SetDefaultRegistry(old) })
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", counter{}); err != nil {
		t.Fatal(err)
	}
	loseReplyUnchecked(fake, false)
	_, _, err := store.Update(ctx, "a", increment)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("want unknown: %v", err)
	}
	if _, _, err = store.Update(Resume(ctx, err), "a", increment); err != nil {
		t.Fatal(err)
	}
	for _, metric := range reg.Snapshot() {
		if metric.Name == MetricCompareAndSet && metric.Labels["store"] == "tok:" && metric.Labels["result"] == "applied" && metric.Value == 1 {
			return
		}
	}
	t.Fatalf("resolved resumed CAS not counted: %v", reg.Snapshot())
}
