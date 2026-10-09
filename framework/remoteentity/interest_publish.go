package remoteentity

import (
	"context"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

const interestPublishCapacity = 256

// queueReadInterest 只做本地准入。推送兴趣是读取的优化项，NATS 故障不能耗尽 L2/权威的读取预算。
// 同一 key 在本机已准入时不重复入队；队列满则撤销本次兴趣，后续读取仍可回源并重新申请。
func (c *SnapshotClient) queueReadInterest(key entity.RemoteSnapshotKey) {
	interest, admitted, _ := c.admitInterestRenewal(key, false)
	if !admitted {
		return
	}
	c.interestPublishMu.Lock()
	if c.interestPublishQueue == nil {
		c.interestPublishQueue = make(chan entity.RemoteSnapshotInterest, interestPublishCapacity)
	}
	if c.stopped.Load() || len(c.interestPublishQueue) == cap(c.interestPublishQueue) {
		c.interestPublishMu.Unlock()
		c.rollbackLocalInterest(key, interest.ExpiresAt)
		c.noteInterestRejected("publish_queue")
		return
	}
	if !c.interestPublishRunning {
		if !c.work.Begin() {
			c.interestPublishMu.Unlock()
			c.rollbackLocalInterest(key, interest.ExpiresAt)
			return
		}
		c.interestPublishRunning = true
		go c.runInterestPublish()
	}
	c.interestPublishQueue <- interest // 锁内确认容量；worker 同锁取出，不会在此等待。
	c.interestPublishMu.Unlock()
}

func (c *SnapshotClient) runInterestPublish() {
	defer c.work.End()
	for {
		c.interestPublishMu.Lock()
		if len(c.interestPublishQueue) == 0 {
			c.interestPublishRunning = false
			c.interestPublishMu.Unlock()
			return
		}
		interest := <-c.interestPublishQueue
		c.interestPublishMu.Unlock()
		c.localInterestMu.Lock()
		current := c.localInterests[interest.Key]
		c.localInterestMu.Unlock()
		if current != interest.ExpiresAt || time.Now().UnixNano() >= interest.ExpiresAt {
			continue
		}
		// Stop 后仍撤销队列里本次登记，但不再接触传输依赖。发布不配合取消时 Stop 如实等待。
		ctx, cancel := context.WithTimeout(c.stopCtx, c.cfg.SnapshotLoadTimeout)
		err := ctx.Err()
		if err == nil {
			err = c.publishInterest(ctx, interest, false)
		}
		cancel()
		if err != nil {
			c.rollbackLocalInterest(interest.Key, interest.ExpiresAt)
			c.noteInterestRejected("publish_failed")
		} else if c.localInterestOps.Add(1)&1023 == 0 {
			c.localInterestMu.Lock()
			c.pruneLocalInterestsLocked(time.Now().UnixNano())
			c.localInterestMu.Unlock()
		}
	}
}
