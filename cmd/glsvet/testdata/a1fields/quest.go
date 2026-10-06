// Package player 是 glsvet A1 字段写提示的夹具（componentfields_promises_test.go）：QuestComponent 把
// 事务里会改的任务状态放在普通字段里，也不登记 undo——handler 失败或提交被拒时这些字段不回滚。
package player

import "github.com/tjbdwanghaibo/roost-core/entity"

type Player struct{}

type PlayerDao struct{ level int32 }

type QuestComponent struct {
	entity.ComponentBase
	owner    *Player
	dao      *PlayerDao
	active   map[int32]int32 // 进行中的任务：可变状态，不在 DAO
	history  []int32
	progress int32
	//roost:cache
	lookup map[int32]int
	byTag  map[string][]int32 //roost:cache 由 DAO 重建
	onDone func(int32)
}

// OnInitFinish 是初始化钩子：不在业务事务里，写字段不提示。
func (c *QuestComponent) OnInitFinish(_ *entity.EntityCreateParam, _ bool) error {
	c.active = make(map[int32]int32)
	c.progress = 0
	return nil
}

func (c *QuestComponent) Accept(id int32) { c.active[id] = 0 } // 提示：map 元素

func (c *QuestComponent) Finish(id int32) {
	delete(c.active, id)              // 提示：delete
	c.history = append(c.history, id) // 提示：赋值
	if c.onDone != nil {
		c.onDone(id)
	}
}

func (c *QuestComponent) Advance() { c.progress++ } // 提示：++

func (c *QuestComponent) Reset() { resetQuests(c) } // 提示：经同包 helper 写

func resetQuests(quests *QuestComponent) { quests.progress = 0 }

// 以下都不提示：缓存字段、函数类型字段（装配的行为）、DAO 句柄、只读。
func (c *QuestComponent) index() {
	c.lookup[1] = 2
	c.byTag = map[string][]int32{}
}

func (c *QuestComponent) OnDone(callback func(int32)) { c.onDone = callback }

func (c *QuestComponent) Bind(dao *PlayerDao) { c.dao = dao }

func (c *QuestComponent) Level() int32 { return c.dao.level + c.progress }
