package query

import "testing"

func TestIndexQuery(t *testing.T) {
	idx := NewOrderedIndex[int64, int32](func(a, b int64) bool { return a < b })
	idx.Upsert(3, 10)
	idx.Upsert(1, 10)
	idx.Upsert(2, 20)
	got := idx.Query(10)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("Query = %v", got)
	}
	idx.Upsert(1, 20)
	got = idx.Query(10)
	if len(got) != 1 || got[0] != 3 {
		t.Fatalf("Query after move = %v", got)
	}
}

func TestOrderedIndexDefaultOrdersIntegersNumerically(t *testing.T) {
	idx := NewOrderedIndex[int64, string](nil)
	for _, key := range []int64{10, 9, 100, 2} {
		idx.Upsert(key, "group")
	}
	got := idx.Query("group")
	want := []int64{2, 9, 10, 100}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (lexical ordering regression)", got, want)
		}
	}
}

func TestOrderedIndexCustomLessWins(t *testing.T) {
	idx := NewOrderedIndex[int64, string](func(a, b int64) bool { return a > b })
	for _, key := range []int64{1, 3, 2} {
		idx.Upsert(key, "g")
	}
	got := idx.Query("g")
	if got[0] != 3 || got[2] != 1 {
		t.Fatalf("custom less ignored: %v", got)
	}
}
