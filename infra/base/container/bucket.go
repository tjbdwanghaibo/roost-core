package container

import "github.com/tjbdwanghaibo/roost-core/infra/base/misc"

import "sync"

type BucketHolder[K misc.Integer, T any] struct {
	RangeCursor uint
	Buckets     []*Bucket[K, T]
	BucketCnt   uint64
	DataBuilder func(K) T
}

func NewBucketHolder[K misc.Integer, T any](bucketCnt int, db func(K) T, buildIF bool) *BucketHolder[K, T] {
	ret := &BucketHolder[K, T]{
		BucketCnt:   uint64(bucketCnt),
		Buckets:     make([]*Bucket[K, T], bucketCnt),
		DataBuilder: db,
	}
	for i := 0; i < int(ret.BucketCnt); i++ {
		ret.Buckets[i] = NewBucket(db, buildIF)
	}
	return ret
}

func (h *BucketHolder[K, T]) Get(k K) T {
	return h.Buckets[misc.Hash64(uint64(k))%h.BucketCnt].Get(k)
}

func (h *BucketHolder[K, T]) Add(k K, t T) {
	h.Buckets[misc.Hash64(uint64(k))%h.BucketCnt].Add(k, t)
}

func (h *BucketHolder[K, T]) Del(k K) {
	h.Buckets[misc.Hash64(uint64(k))%h.BucketCnt].Del(k)
}

func (h *BucketHolder[K, T]) RangeWithCursor(f func(K, T) bool) {
	h.Buckets[h.RangeCursor%uint(h.BucketCnt)].Range(f)
	h.RangeCursor++
}

// RangeWithCursorCnt 从游标处依次遍历 cursorCnt 个桶；f 返回 false 时立即停止，
// 游标前进走过的桶数（已开始的桶算作走过）。
//
// 要遍历的桶在进入时就按游标定下（C7 遍历回调契约）：之前每个桶都现读游标，回调里
// 再调一次游标遍历会推进同一个游标，外层随后重走同一个桶、把同一条目交出两次。
// 回调里推进的游标照样保留（这里是累加，不是覆盖）。
func (h *BucketHolder[K, T]) RangeWithCursorCnt(cursorCnt uint, f func(K, T) bool) {
	start := h.RangeCursor
	for i := range cursorCnt {
		stopped := rangeBucket(h.Buckets[(start+i)%uint(h.BucketCnt)], f)
		h.RangeCursor++
		if stopped {
			return
		}
	}
}

func (h *BucketHolder[K, T]) RangeByCursor(cursor uint, f func(K, T) bool) {
	h.Buckets[cursor%uint(h.BucketCnt)].Range(f)
}

// RangeAll 遍历全部桶，f 第一次返回 false 后不再被调用。
//
// 之前 false 只结束当前桶，接着遍历下一个桶（RR-20261005-NC-181），
// EntityManager.Range / RangeByCategory 写明的提前停止因此不成立。
func (h *BucketHolder[K, T]) RangeAll(f func(K, T) bool) {
	for _, bucket := range h.Buckets {
		if rangeBucket(bucket, f) {
			return
		}
	}
}

// rangeBucket 遍历一个桶并报告 f 是否要求停止。
func rangeBucket[K misc.Integer, T any](bucket *Bucket[K, T], f func(K, T) bool) (stopped bool) {
	bucket.Range(func(k K, v T) bool {
		if !f(k, v) {
			stopped = true
			return false
		}
		return true
	})
	return stopped
}

func (h *BucketHolder[K, T]) Count() int {
	var cnt int
	for _, bucket := range h.Buckets {
		cnt += bucket.Count()
	}
	return cnt
}

type Bucket[K misc.Integer, T any] struct {
	sync.RWMutex
	dataMap     map[K]T
	dataBuilder func(K) T
	buildIF     bool
}

func NewBucket[K misc.Integer, T any](db func(K) T, buildIF bool) *Bucket[K, T] {
	return &Bucket[K, T]{
		dataMap:     make(map[K]T),
		dataBuilder: db,
		buildIF:     buildIF,
	}
}

func (b *Bucket[K, T]) Get(k K) T {
	b.RLock()
	data, exist := b.dataMap[k]
	if exist {
		b.RUnlock()
		return data
	}
	b.RUnlock()
	b.Lock()
	defer b.Unlock()
	data, exist = b.dataMap[k]
	if exist {
		return data
	}
	if b.buildIF {
		data = b.dataBuilder(k)
		b.dataMap[k] = data
	}
	return data
}

func (b *Bucket[K, T]) Del(k K) {
	b.Lock()
	defer b.Unlock()
	delete(b.dataMap, k)
}

func (b *Bucket[K, T]) Add(k K, t T) {
	b.Lock()
	defer b.Unlock()
	b.dataMap[k] = t
}

func (b *Bucket[K, T]) Clear() {
	b.Lock()
	defer b.Unlock()
	clear(b.dataMap)
}

// Range 在读锁内复制本桶条目，释放锁之后再逐个调用 f，f 返回 false 即停止。
//
// 回调不在锁内（RR-20261005-NC-180）：之前 f 在 RLock 内执行，回调里 Del / Add
// 同一桶要写锁、未命中的 Get 也升级写锁，当场自锁；EntityManager.Range 的回调
// 里 Destroy 实体会在持有实体锁与 addMu 时卡住，连带全进程的 Add / Destroy。
// 代价是回调看到的是复制时的快照：期间被删除的条目仍可能交给 f，调用方照例
// 在拿到对象自己的锁后复核（见 lock.LockManager.GetLock 的约定）。
func (b *Bucket[K, T]) Range(f func(K, T) bool) {
	type entry struct {
		k K
		v T
	}
	b.RLock()
	entries := make([]entry, 0, len(b.dataMap))
	for k, v := range b.dataMap {
		entries = append(entries, entry{k, v})
	}
	b.RUnlock()
	for _, e := range entries {
		if !f(e.k, e.v) {
			return
		}
	}
}

func (b *Bucket[K, T]) Count() int {
	b.RLock()
	defer b.RUnlock()
	return len(b.dataMap)
}
