//go:build daoruntime

// U-0236 · C4 · RR-20260918-03：嵌套 child 的"谁通知谁"在替换与撤销之后必须与
// "当前可达的 child 集合"一致。
//
// 承诺：一次替换之后，只有留在字段里的 child 会把变更传播到这个父对象；一次
// 回滚之后，恢复出来的那个 child 重新通知，被丢弃的那个不再通知。旧行为：
// map / slice 的 setter 正常替换时确实解绑旧的、绑定新的，但 undo 闭包只把
// 字段值设回去，不碰 callback——于是回滚之后字段身份恢复了、运行时关系没恢复，
// 恢复出来的 child 后续的真实修改静默漏出持久化链；单指针分支更进一步，连正常
// 替换都不解绑旧值。
//
// 和 roundtrip_test.go 一样，这个文件不由 `go test ./...` 编译：它需要真的
// roost-core。scripts/dao-golden-runtime.sh 把它和 golden 放进一次性模块里跑。
package testdata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type ownershipCommitter struct{ records []nest.CommitRecord }

func (c *ownershipCommitter) Commit(_ context.Context, rec nest.CommitRecord) error {
	c.records = append(c.records, rec)
	return nil
}

// 三种 child 形状（map 值、slice 元素、单指针）各跑四种模式：正常替换后旧 /
// 新 child 改一次，回滚后旧 / 新 child 改一次。断言的是"通知有没有到父对象"，
// 不是"字段值对不对"——值先单独断言一次，免得把两种失败混在一起。
func TestNestedChildNotificationOwnership(t *testing.T) {
	for _, kind := range []string{"map", "slice", "pointer"} {
		for _, mode := range []string{"normal_old", "normal_new", "rollback_old", "rollback_new"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				parent := &EquipInfo{}
				oldChild := &GemInfo{}
				newChild := &GemInfo{}
				set := func(c *GemInfo) {
					switch kind {
					case "map":
						parent.SetGems(map[int32]*GemInfo{1: c})
					case "slice":
						parent.SetRunes([]*GemInfo{c})
					default:
						parent.SetCore(c)
					}
				}
				reachable := func() *GemInfo {
					switch kind {
					case "map":
						v, _ := parent.GetGems(1)
						return v
					case "slice":
						v, _ := parent.GetRunes(0)
						return v
					default:
						return parent.GetCore()
					}
				}

				set(oldChild)
				marks := 0
				parent.SetNotify(func() { marks++ })

				rollback := mode == "rollback_old" || mode == "rollback_new"
				if rollback {
					sentinel := errors.New("abort")
					_, err := nest.RunIsolatedTransaction(context.Background(), &ownershipCommitter{}, "replace",
						func() (any, error) { set(newChild); return nil, sentinel })
					if !errors.Is(err, sentinel) {
						t.Fatalf("transaction error = %v, want the sentinel", err)
					}
					if got := reachable(); got != oldChild {
						t.Fatalf("after rollback the field holds %p, want the original child %p", got, oldChild)
					}
				} else {
					set(newChild)
				}

				marks = 0
				want := 0
				if mode == "normal_old" || mode == "rollback_old" {
					oldChild.SetLevel(9)
				} else {
					newChild.SetLevel(9)
				}
				// 现在字段里躺着的那个 child 才应该通知：正常替换后是 new，
				// 回滚之后是 old。
				if mode == "normal_new" || mode == "rollback_old" {
					want = 1
				}
				if marks != want {
					t.Fatalf("parent notifications = %d, want %d", marks, want)
				}
			})
		}
	}
}

// 通知归属错了不只是多打 / 少打一次 dirty：回滚之后对恢复出来的 child 做的
// 真实修改会整条漏出持久化链——下一次事务提交时父 DAO 认为无事发生，
// committer 一条记录都收不到。
func TestRestoredChildStillReachesTheCommitRecord(t *testing.T) {
	for _, abort := range []bool{false, true} {
		name := "control"
		if abort {
			name = "after_rollback"
		}
		t.Run(name, func(t *testing.T) {
			hero := NewHeroDao()
			hero.SetId(42)
			equip := &EquipInfo{}
			child := &GemInfo{}
			equip.setGemsRawMap(map[int32]*GemInfo{1: child})
			hero.setEquipsRawMap(map[int64]*EquipInfo{1: equip})
			hero.Init()

			committer := &ownershipCommitter{}
			if abort {
				sentinel := errors.New("abort")
				_, err := nest.RunIsolatedTransaction(context.Background(), committer, "replace",
					func() (any, error) { equip.SetGems(map[int32]*GemInfo{1: {}}); return nil, sentinel })
				if !errors.Is(err, sentinel) {
					t.Fatalf("transaction error = %v, want the sentinel", err)
				}
			}
			if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "update_restored_child",
				func() (any, error) { child.SetLevel(9); return nil, nil }); err != nil {
				t.Fatal(err)
			}
			if len(committer.records) != 1 {
				t.Fatalf("durable commit records = %d, want 1 (child level = %d)", len(committer.records), child.GetLevel())
			}
		})
	}
}

// 值类型的嵌套字段没有"旧对象"可解绑，但同样要在替换之后重新建立归属：赋值
// 是一次结构体拷贝，连 hook 一起覆盖掉，此前 Set<Field> 完全不绑，于是替换过
// 的子结构后续的修改再也到不了父对象。
func TestReplacedNestedValueStillNotifies(t *testing.T) {
	parent := &EquipInfo{}
	marks := 0
	parent.SetNotify(func() { marks++ })
	parent.SetShape(Position{})
	marks = 0
	parent.GetShape().SetX(3)
	if marks != 1 {
		t.Fatalf("parent notifications = %d, want 1", marks)
	}
}

// U-0238 · C4 · RR-20260918-10：顶层 DAO 的 map 值和 slice 元素，其"谁通知谁"
// 必须跟着容器里的内容走。
//
// 这是 U-0236 的同胞：那次修的是嵌套结构内部的字段，这次是顶层 DAO 的容器。
// 症状更重一档——顶层 map 值的回调里捕获了 **key**，所以一个被换掉的旧值后续
// 的修改不只是多打一次脏标记，它会以当前 key 的名义进入持久化补丁，把游离对象
// 的内容写到那个 key 上。

// writtenPaths renders what a commit record would put in storage, so a failure
// message can show it. A DAO at version 0 has never been written, so its
// mutation carries the whole document instead of a patch — both are a write,
// and the distinction matters only to the message.
func writtenPaths(t *testing.T, record nest.CommitRecord) []string {
	t.Helper()
	var keys []string
	for _, mutation := range record.Mutations {
		if len(mutation.Patch.SetBSON) == 0 {
			if len(mutation.Data) > 0 {
				keys = append(keys, "<full document>")
			}
			continue
		}
		var document bson.D
		if err := bson.Unmarshal(mutation.Patch.SetBSON, &document); err != nil {
			t.Fatalf("decode patch: %v", err)
		}
		for _, element := range document {
			keys = append(keys, element.Key)
		}
	}
	return keys
}

func TestTopLevelMapValueOwnership(t *testing.T) {
	for _, test := range []struct {
		name        string
		abort       bool
		mutateOld   bool
		wantRecords int
	}{
		// The value that left the map must not be able to write to it any
		// more; the one that took its place must.
		{"committed_old_is_detached", false, true, 0},
		{"committed_new_is_owned", false, false, 1},
		// And a rollback puts both facts back the way they were.
		{"rolled_back_old_is_restored", true, true, 1},
		{"rolled_back_new_is_detached", true, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			hero := NewHeroDao()
			hero.SetId(42)
			oldValue := &EquipInfo{}
			hero.setEquipsRawMap(map[int64]*EquipInfo{1: oldValue})
			hero.Init()
			newValue := &EquipInfo{}

			committer := &ownershipCommitter{}
			sentinel := errors.New("abort")
			_, err := nest.RunIsolatedTransaction(context.Background(), committer, "replace",
				func() (any, error) {
					hero.SetEquips(1, newValue)
					if test.abort {
						return nil, sentinel
					}
					return nil, nil
				})
			if test.abort && !errors.Is(err, sentinel) {
				t.Fatalf("transaction error = %v, want the sentinel", err)
			}
			if !test.abort && err != nil {
				t.Fatalf("commit: %v", err)
			}
			committer.records = nil

			target := newValue
			if test.mutateOld {
				target = oldValue
			}
			if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "touch",
				func() (any, error) { target.SetLevel(9); return nil, nil }); err != nil {
				t.Fatal(err)
			}
			if len(committer.records) != test.wantRecords {
				paths := ""
				if len(committer.records) > 0 {
					paths = fmt.Sprintf(" (it would write %v)", writtenPaths(t, committer.records[0]))
				}
				t.Fatalf("mutating the %s value produced %d commit records, want %d%s",
					map[bool]string{true: "old", false: "new"}[test.mutateOld],
					len(committer.records), test.wantRecords, paths)
			}
			// The positive direction is worth an assertion of its own: the
			// value the map holds must reach the patch under its own key.
			if test.wantRecords == 1 {
				if keys := writtenPaths(t, committer.records[0]); len(keys) == 0 {
					t.Fatalf("the owning value produced a record that writes nothing")
				}
			}
		})
	}
}

// The third top-level shape: ONE nested pointer in a field of its own.
//
// U-0238 fixed the map and the slice and left this branch of the template
// untouched, and it has the same defect (RR-20260919-01): the setter binds the
// new value but never releases the old one, and the undo restores the old one
// without releasing the new. A detached object that still reports its changes
// writes content into a field it no longer occupies.
func TestTopLevelPointerFieldOwnership(t *testing.T) {
	for _, test := range []struct {
		name        string
		abort       bool
		mutateOld   bool
		wantRecords int
	}{
		{"committed_old_is_detached", false, true, 0},
		{"committed_new_is_owned", false, false, 1},
		{"rolled_back_old_is_restored", true, true, 1},
		{"rolled_back_new_is_detached", true, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			hero := NewHeroDao()
			hero.SetId(42)
			oldValue := &EquipInfo{}
			hero.mount = oldValue
			hero.Init()
			newValue := &EquipInfo{}

			committer := &ownershipCommitter{}
			sentinel := errors.New("abort")
			_, err := nest.RunIsolatedTransaction(context.Background(), committer, "replace",
				func() (any, error) {
					hero.SetMount(newValue)
					if test.abort {
						return nil, sentinel
					}
					return nil, nil
				})
			if test.abort && !errors.Is(err, sentinel) {
				t.Fatalf("transaction error = %v, want the sentinel", err)
			}
			if !test.abort && err != nil {
				t.Fatalf("commit: %v", err)
			}
			committer.records = nil

			target := newValue
			if test.mutateOld {
				target = oldValue
			}
			if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "touch",
				func() (any, error) { target.SetLevel(9); return nil, nil }); err != nil {
				t.Fatal(err)
			}
			if len(committer.records) != test.wantRecords {
				t.Fatalf("mutating the %s value produced %d commit records, want %d",
					map[bool]string{true: "old", false: "new"}[test.mutateOld],
					len(committer.records), test.wantRecords)
			}
		})
	}
}

// Setting the field to nil detaches what was there: a value nobody holds must
// not keep writing into the field it left.
func TestClearingAPointerFieldDetachesTheValue(t *testing.T) {
	hero := NewHeroDao()
	hero.SetId(42)
	worn := &EquipInfo{}
	hero.mount = worn
	hero.Init()

	committer := &ownershipCommitter{}
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "clear",
		func() (any, error) { hero.SetMount(nil); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	committer.records = nil
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "touch",
		func() (any, error) { worn.SetLevel(3); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if len(committer.records) != 0 {
		t.Fatalf("a value cleared out of its field still produced %d commit records", len(committer.records))
	}
}

// The slice shape of the same question. The callback here does not capture an
// index, so a detached element can only mark the DAO dirty rather than write
// under somebody else's key — a milder symptom of the same missing ownership
// transfer, fixed by the same helpers.
func TestTopLevelSliceItemOwnership(t *testing.T) {
	for _, test := range []struct {
		name        string
		abort       bool
		mutateOld   bool
		wantRecords int
	}{
		{"committed_old_is_detached", false, true, 0},
		{"committed_new_is_owned", false, false, 1},
		{"rolled_back_old_is_restored", true, true, 1},
		{"rolled_back_new_is_detached", true, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			hero := NewHeroDao()
			hero.SetId(42)
			oldItem := &EquipInfo{}
			hero.squad = []*EquipInfo{oldItem}
			hero.Init()
			newItem := &EquipInfo{}

			committer := &ownershipCommitter{}
			sentinel := errors.New("abort")
			_, err := nest.RunIsolatedTransaction(context.Background(), committer, "replace",
				func() (any, error) {
					hero.SetSquadAll([]*EquipInfo{newItem})
					if test.abort {
						return nil, sentinel
					}
					return nil, nil
				})
			if test.abort && !errors.Is(err, sentinel) {
				t.Fatalf("transaction error = %v, want the sentinel", err)
			}
			if !test.abort && err != nil {
				t.Fatalf("commit: %v", err)
			}
			committer.records = nil

			target := newItem
			if test.mutateOld {
				target = oldItem
			}
			if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "touch",
				func() (any, error) { target.SetLevel(9); return nil, nil }); err != nil {
				t.Fatal(err)
			}
			if len(committer.records) != test.wantRecords {
				t.Fatalf("mutating the %s item produced %d commit records, want %d",
					map[bool]string{true: "old", false: "new"}[test.mutateOld],
					len(committer.records), test.wantRecords)
			}
		})
	}
}

// U-0245：一个刚构造出来的 DAO，它的嵌套回调也必须是接好的。
//
// `Init()` 此前只在装载路径上被调用（UnmarshalBSON / RestorePersisted /
// ApplySync）。于是一个**新建**的实体——第一次登录的玩家、刚刷出来的怪——
// 其嵌套字段的通知是空的：改动不标脏、不进持久化补丁、**悄悄丢掉**，直到
// 这个实体被存过一次再读回来为止。
//
// 运行时门此前看不到它，因为每个 harness 都自己调了一次 Init()。
func TestAFreshlyConstructedDaoPropagatesNestedChanges(t *testing.T) {
	hero := NewHeroDao()
	hero.SetId(42)
	// No Init() here on purpose: that is the whole question.
	committer := &ownershipCommitter{}

	// Straight to the nested value, without going through Set<Field> first.
	// That is the path a component takes: `dao.GetEquipment().SetSlots(...)`
	// never touches the DAO's own setter, so if the constructor did not wire
	// the callback, nothing did.
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "move",
		func() (any, error) { hero.GetPos().SetX(3); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if len(committer.records) != 1 {
		t.Fatalf("changing a nested value of a freshly constructed DAO produced %d commit records, want 1 — the change would be lost", len(committer.records))
	}
}

// A mutable nested value has ONE parent at a time.
//
// The notification is a single slot, so a value bound twice reports to
// whichever parent bound it last — and the first parent's key stops being
// persisted (RR-20260919-02). Neither "last binding wins" nor "first binding
// wins" is a correct answer: both leave one of the two places silently stale
// on disk while memory shows them equal. So the contract is unique parent
// ownership, and putting a value somewhere it cannot go is refused loudly, at
// the call, before anything is written.
func TestANestedValueHasOneParentAtATime(t *testing.T) {
	for _, test := range []struct {
		name string
		put  func(hero *HeroDao, shared *EquipInfo)
	}{
		{"same map, another key", func(hero *HeroDao, shared *EquipInfo) { hero.SetEquips(2, shared) }},
		{"another field of the same dao", func(hero *HeroDao, shared *EquipInfo) { hero.SetMount(shared) }},
		{"a slice in the same dao", func(hero *HeroDao, shared *EquipInfo) { hero.SetSquadAll([]*EquipInfo{shared}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			hero := NewHeroDao()
			hero.SetId(42)
			shared := &EquipInfo{}
			hero.setEquipsRawMap(map[int64]*EquipInfo{1: shared})
			hero.Init()

			committer := &ownershipCommitter{}
			outcome, err := nest.RunIsolatedTransaction(context.Background(), committer, "alias",
				func() (result any, err error) {
					defer func() {
						if recovered := recover(); recovered != nil {
							result = fmt.Sprint(recovered)
						}
					}()
					test.put(hero, shared)
					return "", nil
				})
			if err != nil {
				t.Fatalf("transaction: %v", err)
			}
			// The refusal has to say what to do, not just that something is
			// wrong: the caller is holding one object and thinks it is in two
			// places.
			refusal, _ := outcome.(string)
			if refusal == "" {
				t.Fatal("putting one nested value in two places was accepted; on disk only the last one would move")
			}
			for _, want := range []string{"one parent", "EquipInfo"} {
				if !strings.Contains(refusal, want) {
					t.Errorf("the refusal does not mention %q: %s", want, refusal)
				}
			}
		})
	}
}

// Moving it is allowed, and the way to move it is to take it out first.
func TestANestedValueCanBeMovedBetweenKeys(t *testing.T) {
	hero := NewHeroDao()
	hero.SetId(42)
	moved := &EquipInfo{}
	hero.setEquipsRawMap(map[int64]*EquipInfo{1: moved})
	hero.Init()

	committer := &ownershipCommitter{}
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "move", func() (any, error) {
		hero.DelEquips(1)
		hero.SetEquips(2, moved)
		return nil, nil
	}); err != nil {
		t.Fatalf("move: %v", err)
	}
	committer.records = nil
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "touch", func() (any, error) {
		moved.SetLevel(4)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(committer.records) != 1 {
		t.Fatalf("the moved value produced %d commit records, want 1", len(committer.records))
	}
	// And it writes under the key it is in now.
	paths := writtenPaths(t, committer.records[0])
	found := false
	for _, path := range paths {
		if strings.Contains(path, "2") {
			found = true
		}
		if strings.HasSuffix(path, ".1") {
			t.Errorf("the moved value still writes under its old key: %v", paths)
		}
	}
	if !found {
		t.Errorf("the moved value did not write under its new key: %v", paths)
	}
}

// Binding the same value to the same place again is not an alias: Init runs on
// every load, and a setter that re-puts what is already there is ordinary.
func TestRebindingTheSameSlotIsAccepted(t *testing.T) {
	hero := NewHeroDao()
	hero.SetId(42)
	worn := &EquipInfo{}
	hero.setEquipsRawMap(map[int64]*EquipInfo{1: worn})
	hero.Init()
	hero.Init()

	committer := &ownershipCommitter{}
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "re-put", func() (any, error) {
		hero.SetEquips(1, worn)
		return nil, nil
	}); err != nil {
		t.Fatalf("putting the same value back into its own key was refused: %v", err)
	}
}

// Two DAOs is the case a per-DAO check would miss.
func TestANestedValueCannotBeSharedBetweenDaos(t *testing.T) {
	first, second := NewHeroDao(), NewHeroDao()
	first.SetId(42)
	second.SetId(43)
	shared := &EquipInfo{}
	first.setEquipsRawMap(map[int64]*EquipInfo{1: shared})
	first.Init()
	second.Init()

	committer := &ownershipCommitter{}
	outcome, err := nest.RunIsolatedTransaction(context.Background(), committer, "share", func() (result any, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				result = fmt.Sprint(recovered)
			}
		}()
		second.SetEquips(1, shared)
		return "", nil
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
	if refusal, _ := outcome.(string); refusal == "" {
		t.Fatal("one nested value was accepted into two DAOs; only one of them would persist its changes")
	}
}
