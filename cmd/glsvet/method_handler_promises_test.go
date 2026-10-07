package main

import "testing"

// RR-20261007-03（F02-3）：生成器接受方法 handler，检查器不能因 receiver 非空而跳过它。
func TestMethodHandlerConcurrencyChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		want   int
	}{
		{"pointer", `package p
type controller struct{}
//roost:nest rollback=undo durability=strict
func (*controller) Move() { go func(){}() }
`, 1},
		{"value", `package p
type controller struct{}
//roost:nest rollback=undo durability=strict
func (controller) Move() { go func(){}() }
`, 1},
		{"package helper with same name", `package p
type controller struct{}
func Move() { go func(){}() }
//roost:nest rollback=undo durability=strict
func (*controller) Move() { Move() }
`, 1},
		{"different receivers with same name", `package p
type first struct{}
type second struct{}
//roost:nest rollback=undo durability=strict
func (*first) Move() { go func(){}() }
//roost:nest rollback=undo durability=strict
func (*second) Move() { go func(){}() }
`, 2},
		{"receiver capture", `package p
import "github.com/tjbdwanghaibo/roost-core/worker"
type task struct{}
func (task) OnRelease() {}
var pool *worker.Pool[task]
type controller struct { value int }
//roost:nest rollback=undo durability=strict
func (c *controller) Move() { pool.Go(task{}, func(task){ _ = c.value }) }
`, 1},
		{"explicit worker parameters", `package p
import "github.com/tjbdwanghaibo/roost-core/worker"
type task struct{}
func (task) OnRelease() {}
var pool *worker.Pool[task]
type controller struct{}
//roost:nest rollback=undo durability=strict
func (*controller) Move() { pool.Go(task{}, func(task){}) }
`, 0},
		{"ordinary method", `package p
type controller struct{}
func (*controller) Move() { go func(){}() }
`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vetSource(t, tc.source); got != tc.want {
				t.Fatalf("findings = %d, want %d", got, tc.want)
			}
		})
	}
}
