package mongotest

import (
	"errors"
	"fmt"
	"go.mongodb.org/mongo-driver/v2/bson"
	"sort"
	"strings"
)

// ErrTransactionConflict 表示替身的集合快照在发布前已失效，可按既有 transient 规则重试。
// RR-20261004-NC-29：按集合检测比真实 Mongo 的文档冲突更保守，不宣称服务端等价。
var ErrTransactionConflict = errors.New("mongofake: collection changed during transaction")

// lockFor 仅在本次集合操作持锁期间切换到事务私有数据；返回函数恢复共享状态。
// Calls/Errors/LastFilter 仍在原对象，测试注入和计数不会因私有快照丢失。
func (c *Collection) lockFor(tx *transaction) func() {
	c.mu.Lock()
	if tx == nil {
		return c.mu.Unlock
	}
	tx.mu.Lock()
	if tx.finished || tx.client != c.client {
		tx.mu.Unlock()
		return c.mu.Unlock
	}
	view := tx.views[c]
	if view == nil {
		// 快照开始后才出现的集合视为当时空集；并发普通写会令 revision 冲突。
		view = &collectionSnapshot{coll: c, docs: make(map[string]bson.M)}
		tx.views[c] = view
	}
	tx.mu.Unlock()
	docs, order, revision := c.docs, c.order, c.revision
	c.docs, c.order, c.revision = view.docs, view.order, view.revision
	return func() {
		view.docs, view.order, view.revision = c.docs, c.order, c.revision
		c.docs, c.order, c.revision = docs, order, revision
		c.mu.Unlock()
	}
}

func (tx *transaction) finish() {
	tx.mu.Lock()
	tx.finished = true
	tx.views = nil
	tx.mu.Unlock()
}

// commit 在固定锁顺序下先验证所有写集合，再整体发布；callback 期间不持全局锁。
// 私有读写只在 callback 生命周期使用，callback 必须等待自己启动的所有操作返回。
func (tx *transaction) commit() error {
	tx.mu.Lock()
	views := make([]*collectionSnapshot, 0, len(tx.views))
	for _, view := range tx.views {
		views = append(views, view)
	}
	tx.mu.Unlock()
	sort.Slice(views, func(i, j int) bool { return collectionLess(views[i].coll, views[j].coll) })
	for _, view := range views {
		view.coll.mu.Lock()
	}
	defer func() {
		for i := len(views) - 1; i >= 0; i-- {
			views[i].coll.mu.Unlock()
		}
	}()
	for _, view := range views {
		if view.revision != view.baseRevision && view.coll.revision != view.baseRevision {
			return &labeledError{err: fmt.Errorf("%w: %s.%s", ErrTransactionConflict, view.coll.database, view.coll.name), labels: []string{TransientTransactionError}}
		}
	}
	for _, view := range views {
		if view.revision == view.baseRevision {
			continue
		}
		view.coll.docs, view.coll.order = view.docs, view.order
		view.coll.revision++
	}
	return nil
}

func collectionLess(a, b *Collection) bool {
	if a.database != b.database {
		return strings.Compare(a.database, b.database) < 0
	}
	return strings.Compare(a.name, b.name) < 0
}
