package jslogproto

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.mongodb.org/mongo-driver/v2/bson"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
)

// SagaProjector 是设计稿 §12 的 saga 冷副本投影：每条流一个 durable 消费者（过滤 <p>.*.ev.>），一批至多 BatchSize 条事件，
// 同一 saga 只留版本最大的一条，一次 BulkWrite（无序）写进 <Database>.sagas，写成功后逐条 ack。
// 不用 AckAll：前一个实例取走、没 ack 就死掉的消息在等 AckWait 重投，新实例 ack 本批最后一条会把它们一并确认掉，
// 冷副本就永远缺那几条（kill -9 用例实测踩到，见设计稿 §12）。
// 版本只升不降：每条写是“版本更大才整文档替换”的管道更新（upsert），重投、乱序、多实例同时投影都幂等。不在关键路径上。
// 清理守卫加上它的 durable 名，就保证“先投影后清理”。
type SagaProjector struct {
	js        jetstream.JetStream
	l         Layout
	coll      fmongo.ICollection
	durable   string
	batchSize int
	log       *slog.Logger
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	Events    atomic.Uint64 // 投影过的事件条数（含重投）
	Writes    atomic.Uint64 // BulkWrite 里的写模型数（按 saga 去重之后）
	Batches   atomic.Uint64
	Failures  atomic.Uint64
	LastErr   atomic.Value
}

// SagaProjectionDurable 是冷副本投影消费者的 durable 名（协调器清理守卫要带上它）。
const SagaProjectionDurable = "proj_saga"

func NewSagaProjector(js jetstream.JetStream, l Layout, mongo fmongo.IMongo, database string, log *slog.Logger) *SagaProjector {
	if log == nil {
		log = slog.Default()
	}
	return &SagaProjector{js: js, l: l, coll: mongo.Database(database).Collection("sagas"), durable: SagaProjectionDurable, batchSize: 256, log: log}
}

func (sp *SagaProjector) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(context.Background())
	sp.cancel = cancel
	for _, name := range sp.l.Streams() {
		cons, err := sp.js.CreateOrUpdateConsumer(ctx, name, jetstream.ConsumerConfig{
			Durable: sp.durable, FilterSubject: sp.l.Prefix + ".*.ev.>", AckPolicy: jetstream.AckExplicitPolicy,
			AckWait: 10 * time.Second, MaxAckPending: 1 << 16, DeliverPolicy: jetstream.DeliverAllPolicy, MaxDeliver: -1,
		})
		if err != nil {
			cancel()
			return err
		}
		sp.wg.Add(1)
		go func() {
			defer sp.wg.Done()
			sp.run(runCtx, cons)
		}()
	}
	return nil
}

func (sp *SagaProjector) run(ctx context.Context, cons jetstream.Consumer) {
	for ctx.Err() == nil {
		batch, err := cons.Fetch(sp.batchSize, jetstream.FetchMaxWait(200*time.Millisecond))
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var msgs []jetstream.Msg
		for m := range batch.Messages() {
			msgs = append(msgs, m)
		}
		if len(msgs) == 0 {
			continue
		}
		for attempt := 0; ctx.Err() == nil; attempt++ {
			if err := sp.write(ctx, msgs); err != nil {
				sp.Failures.Add(1)
				sp.LastErr.Store(err.Error())
				sp.log.Warn("jslogproto: saga projection batch failed; retrying", "err", err)
				time.Sleep(time.Duration(min(attempt+1, 20)) * 50 * time.Millisecond)
				continue
			}
			for _, m := range msgs {
				_ = m.Ack()
			}
			sp.Events.Add(uint64(len(msgs)))
			sp.Batches.Add(1)
			break
		}
	}
}

func (sp *SagaProjector) write(ctx context.Context, msgs []jetstream.Msg) error {
	latest := map[string]*Saga{}
	var order []string
	for _, m := range msgs {
		var ev Event
		if err := json.Unmarshal(m.Data(), &ev); err != nil || ev.Saga == nil {
			continue // 坏信封不投影（协调器重放同样拒绝它）
		}
		s := ev.Saga
		if old, ok := latest[s.ID]; !ok {
			order = append(order, s.ID)
			latest[s.ID] = s
		} else if s.Version > old.Version {
			latest[s.ID] = s
		}
	}
	if len(order) == 0 {
		return nil
	}
	models := make([]fmongo.WriteModel, 0, len(order))
	for _, id := range order {
		s := latest[id]
		doc := bson.M{"_id": s.ID, "v": int64(s.Version), "status": string(s.Status), "part": s.Part, "step": s.Step, "phase": string(s.Phase),
			"attempt": s.Attempt, "completed": s.Completed, "closed": s.Closed, "reopened": s.Reopened,
			"from": s.Data.From, "to": s.Data.To, "amount": s.Data.Amount, "refuse_credit": s.Data.RefuseCredit,
			"created_at": s.CreatedAt, "updated_at": s.UpdatedAt}
		pipeline := bson.A{bson.M{"$replaceWith": bson.M{"$cond": bson.A{
			bson.M{"$gt": bson.A{int64(s.Version), bson.M{"$ifNull": bson.A{"$v", int64(0)}}}},
			bson.M{"$literal": doc},
			"$$ROOT",
		}}}}
		models = append(models, fmongo.NewUpdateOneModel(bson.M{"_id": s.ID}, pipeline, true))
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := sp.coll.BulkWrite(wctx, models); err != nil {
		return err
	}
	sp.Writes.Add(uint64(len(models)))
	return nil
}

// Backlog 是还没投影确认的事件条数（各流消费者的 NumPending + NumAckPending 之和）。
func (sp *SagaProjector) Backlog(ctx context.Context) (uint64, error) {
	var total uint64
	for _, name := range sp.l.Streams() {
		cons, err := sp.js.Consumer(ctx, name, sp.durable)
		if err != nil {
			return 0, err
		}
		info, err := cons.Info(ctx)
		if err != nil {
			return 0, err
		}
		total += info.NumPending + uint64(info.NumAckPending)
	}
	return total, nil
}

// ColdState 读冷副本里每笔 saga 的版本与状态（核对用）。
func (sp *SagaProjector) ColdState(ctx context.Context) (map[string]ColdSaga, error) {
	var docs []ColdSaga
	if err := sp.coll.Find(ctx, bson.M{}, &docs); err != nil {
		return nil, err
	}
	out := make(map[string]ColdSaga, len(docs))
	for _, d := range docs {
		out[d.ID] = d
	}
	return out, nil
}

type ColdSaga struct {
	ID      string `bson:"_id"`
	Version int64  `bson:"v"`
	Status  string `bson:"status"`
}

func (sp *SagaProjector) Stop() {
	if sp.cancel != nil {
		sp.cancel()
	}
	sp.wg.Wait()
}
