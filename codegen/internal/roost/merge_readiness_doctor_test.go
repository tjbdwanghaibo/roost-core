package roost

import (
	"strings"
	"testing"
)

// doctor 的 merge-readiness 读工程自己的 cmd/sidmerge 给出的合服清单：game-demo 的 DAO 都在共享逻辑库 main，
// 生成配置的 syncbus 走 JetStream（按 sid 的 durable），RPC 走 core NATS。它是只读的提示，不让 doctor 失败。
func TestDoctorMergeReadinessReadsTheChecklist(t *testing.T) {
	t.Parallel()
	// cmd/sidmerge 用的 wiring/sidmerge 还没发版：编译当前工作树。
	items, _ := doctorItems(t, buildableGameDemo(t))
	item, ok := items["merge-readiness"]
	if !ok {
		t.Fatalf("game-demo ships cmd/sidmerge, but doctor reported no merge-readiness item: %v", items)
	}
	if item.Status != StatusOK {
		t.Fatalf("merge-readiness = %+v", item)
	}
	for _, want := range []string{"dbscope=sid databases moved by sidmerge migrate: none", "same place: main", "(confirm by hand): syncbus", "business merges"} {
		if !strings.Contains(item.Detail, want) {
			t.Fatalf("merge-readiness detail %q lacks %q", item.Detail, want)
		}
	}
}

// 清单解码只依赖 wiring/sidmerge 的 JSON 字段名；一份没有逻辑库的清单说明命令没有链接工程的 db 包。
func TestMergeChecklistSummary(t *testing.T) {
	summary, err := summarizeMergeChecklist([]byte(`{"sid":1000,"databases":[{"logical":"main"},{"logical":"zone","server_scoped":true}],
		"bus":[{"name":"syncbus","persistent":true,"addressed_to_sid":true},{"name":"service rpc","persistent":false,"addressed_to_sid":true}],
		"business":[{"name":"World singleton"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"migrate: zone", "same place: main", "(confirm by hand): syncbus;", "1 business merges"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q lacks %q", summary, want)
		}
	}
	if _, err := summarizeMergeChecklist([]byte(`{"sid":1000,"databases":[]}`)); err == nil {
		t.Fatal("a checklist without databases was accepted")
	}
}
