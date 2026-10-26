package scout

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestProjectInbox(t *testing.T) {
	report := Report{MapID: 3, Recipient: 7, HomeSID: 1000, Key: "march", Target: 501, At: 1}
	encode := func(m map[string]Report) string {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	raw := encode(map[string]Report{report.Identity(): report})
	for _, bad := range []struct {
		raw       string
		recipient int64
		home      int32
	}{
		{raw, 8, 1000}, {raw, 7, 1001}, {raw, 0, 1000}, {raw, 7, 999},
		{"null", 7, 1000}, {"[]", 7, 1000}, {"{", 7, 1000}, {strings.Repeat(" ", 128*1024+1), 7, 1000},
		{encode(map[string]Report{"wrong-key": report}), 7, 1000},
	} {
		if _, err := ProjectInbox(bad.raw, bad.recipient, bad.home); err == nil {
			t.Fatal("invalid inbox accepted", bad.recipient, bad.home)
		}
	}
	empty, err := ProjectInbox("{}", 7, 1000)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatal("empty view must encode as []", empty, err)
	}
	many := map[string]Report{}
	for i := 0; i < 128; i++ {
		r := report
		r.Key = fmt.Sprintf("march-%03d", 127-i)
		many[r.Identity()] = r
	}
	views, err := ProjectInbox(encode(many), 7, 1000)
	if err != nil || len(views) != 128 {
		t.Fatal("capacity", len(views), err)
	}
	for i, v := range views {
		if v.ID != fmt.Sprintf("3/march-%03d", i) {
			t.Fatal("unstable order", v)
		}
	}
	many[report.Identity()] = report
	if _, err := ProjectInbox(encode(many), 7, 1000); err == nil {
		t.Fatal("over-capacity view accepted")
	}
	report.Target = 999
	if _, err := ProjectInbox(encode(map[string]Report{report.Identity(): report}), 7, 1000); err == nil {
		t.Fatal("invalid report accepted")
	}
}
