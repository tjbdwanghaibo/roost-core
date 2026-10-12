package nest

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

func TestClientTargetStorageOwnership(t *testing.T) {
	a := mustBuildCastID(t, 8301, entity.EntityCategory(1), nestLocalKind)
	b := mustBuildCastID(t, 8302, entity.EntityCategory(1), nestLocalKind)
	input := [][]int64{{b, a, b}, {a}, {}}
	want := [][]int64{{b, a, b}, {a}, {}}
	groups, err := normalizeClientGroups(input)
	if err != nil || !reflect.DeepEqual(groups, want) {
		t.Fatalf("ordered groups: %v, %v", groups, err)
	}
	input[0][0] = a
	input[1] = []int64{b}
	if !reflect.DeepEqual(groups, want) {
		t.Fatal("targets alias caller storage")
	}
	// 对一组 append 不得覆盖下一组，即使这些组共用底层数组。
	extended := append(groups[0], b)
	if len(extended) != 4 || !reflect.DeepEqual(groups, want) {
		t.Fatal("append crossed group boundary")
	}
	ids, err := normalizeClientIDs(want[0])
	if err != nil {
		t.Fatal(err)
	}
	want[0][0] = a
	if ids[0] != b || ids[2] != b {
		t.Fatal("flat targets lost order or caller isolation")
	}
	_, err = normalizeClientGroups([][]int64{{a}, {b, 0}})
	if !errors.Is(err, ErrInvalidMessage) || !strings.Contains(err.Error(), "entity group 1: entity index 1:") {
		t.Fatalf("invalid target location: %v", err)
	}
}

// 固定目标总数分布，测量标准化和存储分配，不代表 Nest 调度吞吐。
func BenchmarkClientGroupTargets(b *testing.B) {
	// 与同包 Client 基准复用同一 Kind/category，不新增注册号。
	entity.MustRegisterEntityKindCategory(nestLocalKind, entity.EntityCategory(1))
	id, err := entity.BuildEntityID(8300, nestLocalKind)
	if err != nil {
		b.Fatal(err)
	}
	for _, n := range []int{1, 8, 32} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			groups := make([][]int64, n)
			for i := range groups {
				groups[i] = []int64{id, id, id, id}
			}
			b.ReportAllocs()
			for b.Loop() {
				result, err := normalizeClientGroups(groups)
				if err != nil || len(result) != n || result[n-1][3] != id {
					b.Fatal(result, err)
				}
			}
			b.ReportMetric(float64(unsafe.Sizeof(Msg{})), "Msg-B")
		})
	}
}
