//go:build integration

package saga

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitmongo "github.com/tjbdwanghaibo/roost-core/wiring/mongo"
	kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"
	kitnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// capturingCommitter 截下“Nest 事务”的 CommitRecord，测试再交给 DataEngine。
type capturingCommitter struct{ record coredata.CommitRecord }

func (committer *capturingCommitter) Commit(_ context.Context, record corenest.CommitRecord) error {
	committer.record = coredata.CloneCommitRecord(record)
	return nil
}

func deleteD12Streams(t *testing.T, url string, streams ...string) {
	t.Helper()
	nc, err := gonats.Connect(url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Errorf("cleanup: connect: %v", err)
		return
	}
	defer nc.Close()
	js, err := gojs.New(nc)
	if err != nil {
		t.Errorf("cleanup: JetStream: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, stream := range streams {
		if err := js.DeleteStream(ctx, stream); err != nil && !errors.Is(err, gojs.ErrStreamNotFound) {
			t.Errorf("cleanup: delete stream %s: %v", stream, err)
		}
	}
}

// realStepsEnv 是 Steps 真实依赖用例的公共装配：真实 Mongo 副本集 + NATS JetStream 三节点，正式的 Mongo / NATS /
// DataEngine /（可选）Nest / saga Mod，saga 流与效果流都是三副本发布确认。库 roost_d12_saga_<pid>_<ns> /
// roost_d12_de_<pid>_<ns>、流 ROOST_D12_SAGA_<…> / ROOST_D12_EFFECTS_<…>，用后删除。
type realStepsEnv struct {
	registry                   *app.Registry
	access                     *entity.ManagerAccess
	token                      string
	sagaStream                 string
	sagaDatabase, dataDatabase string
	sagaPrefix                 string
	mongo                      fmongo.IMongo
	dataMod                    *kitdataengine.Mod
	sagaMod                    *Mod
	// nest 只在 withNest 时装配：真实 Nest 引擎（wiring/nest Mod），事务经 DataEngine 的 WAL → 投影 → outbox 提交。
	nest corenest.Client
}

func startRealStepsEnv(t *testing.T, definitions []coresaga.Definition, withNest bool, overrides map[string]any) *realStepsEnv {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 and source the isolated environment")
	}
	mongoURI, natsURL := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI"), os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if mongoURI == "" || natsURL == "" {
		t.Fatal("isolated integration environment variables are incomplete")
	}
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000_000)
	env := &realStepsEnv{token: strings.ReplaceAll(suffix, "_", "x"), sagaDatabase: "roost_d12_saga_" + suffix, dataDatabase: "roost_d12_de_" + suffix}
	env.sagaPrefix = "d12saga" + env.token
	sagaStream, effectStream := "ROOST_D12_SAGA_"+suffix, "ROOST_D12_EFFECTS_"+suffix
	env.sagaStream = sagaStream
	cfg := viper.New()
	for key, value := range map[string]any{
		"sid": int32(902), "server_type": "d12-it", "persistence.engine": "dataengine",
		"mongo.uri": mongoURI, "mongo.require_replica_set": true, "mongo.connect_timeout": 5 * time.Second, "mongo.transaction_timeout": 15 * time.Second,
		"nats.url":            natsURL,
		"dataengine.database": env.dataDatabase, "dataengine.wal.dir": t.TempDir(),
		"dataengine.effects.stream": effectStream, "dataengine.effects.subject_prefix": "d12effect" + env.token,
		"dataengine.effects.replicas": 3, "dataengine.effects.max_bytes": int64(64 << 20),
		"dataengine.outbox.poll_interval": 50 * time.Millisecond, "dataengine.outbox.retry_min": 100 * time.Millisecond, "dataengine.outbox.retry_max": time.Second,
		"saga.database": env.sagaDatabase, "saga.stream": sagaStream, "saga.subject_prefix": env.sagaPrefix, "saga.replicas": 3,
		"saga.stream_max_bytes": int64(64 << 20), "saga.poll_interval": 50 * time.Millisecond,
		"saga.step_defaults.timeout": 5 * time.Second, "saga.step_defaults.max_attempts": 5,
		"saga.step_defaults.backoff_min": 100 * time.Millisecond, "saga.step_defaults.backoff_max": time.Second,
	} {
		cfg.Set(key, value)
	}
	for key, value := range overrides {
		cfg.Set(key, value)
	}
	env.registry = app.NewRegistry(cfg)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	env.access = access
	mongoMod, natsMod := kitmongo.NewMongoMod(), kitnats.NewNatsMod(nil)
	env.dataMod = kitdataengine.NewMod(kitdataengine.WithEntityAccess(access))
	env.sagaMod = NewMod(definitions...)
	type lifecycle interface {
		app.Mod
		Init(*viper.Viper) error
		Provide(*app.Registry) error
		Start() error
		StopWithContext(context.Context) error
	}
	all := []lifecycle{mongoMod, natsMod, env.dataMod}
	if withNest {
		all = append(all, kitnest.NewMod(access))
	}
	all = append(all, env.sagaMod)
	checked := make([]app.Mod, 0, len(all))
	for _, mod := range all {
		checked = append(checked, mod)
	}
	if err := app.CheckConfig(cfg, checked...); err != nil {
		t.Fatal(err)
	}
	started := []lifecycle{}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for i := len(started) - 1; i >= 0; i-- {
			if i == 0 && env.mongo != nil { // Mongo 最后停：先删本次的库
				for _, database := range []string{env.sagaDatabase, env.dataDatabase} {
					if err := env.mongo.Database(database).Drop(ctx); err != nil {
						t.Errorf("drop %s: %v", database, err)
					}
				}
			}
			if err := started[i].StopWithContext(ctx); err != nil {
				t.Errorf("stop %s: %v", started[i].Name(), err)
			}
		}
		deleteD12Streams(t, natsURL, sagaStream, effectStream)
	})
	for _, mod := range all {
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
	}
	for _, mod := range all {
		if err := mod.Provide(env.registry); err != nil {
			t.Fatal(err)
		}
		if err := mod.Start(); err != nil {
			t.Fatal(err)
		}
		started = append(started, mod)
		if mod == lifecycle(mongoMod) {
			env.mongo = mongoMod.Client()
		}
	}
	if withNest {
		env.nest = app.MustLookup[corenest.Client](env.registry, mods.ModNest)
	}
	return env
}

// 原生步骤在真实 Nest 调度下（D12 验证项 1）：真实 Nest 引擎（wiring/nest Mod，正式的 short / io 池与冷加载）+
// 真实 DataEngine（WAL → 投影 → outbox），步骤体经 Nest Client 请求一个手写 handler，handler 改实体余额并在事务里
// step.Complete。一份转账形状的两步 saga（debit 扣 from、credit 加 to，补偿 refund / credit-undo，四步都是原生步骤）：
//
//   - ok：正向完成；
//   - refuse：credit 超上限，业务拒绝（Complete(false, …)，只提交回执与失败的 completion），协调器补偿 debit；
//   - dup：debit 第一次投递执行中时再投一份同一命令（不带 MsgID，绕过流的去重窗口），完成后再投一份——都只回放；
//   - lostreply：debit 的 Nest 提交成功后步骤体返回错误（结果未知）——消费者保留租约，重投等到回执后 ack，不再执行；
//   - failfirst：debit 第一次在调用 Nest 之前返回错误（结果未知、其实没提交）——这次尝试等到截止，协调器按步骤超时发
//     第二次尝试执行；
//   - fenced：投影器被挡在 checkpoint 上，saga X 的 debit 已准入 WAL、投影未定（实体屏障），另一个 saga 类型 Y 的 bonus
//     步骤写同一实体，被 dataengine.ErrFencedEntityPending 拒绝——交还租约、立刻重投，屏障解除后在第一次尝试里生效，
//     而不是等到步骤超时；
//   - stall：投影器停到 S 的 debit 命令截止之后——投影时租约已过期，整笔记录被跳过、实体被驱逐，协调器的第二次尝试在
//     从 Mongo 重载的实体上生效，仍恰好一次。
//
// 核对：每笔 saga 的终态；Mongo 里每个账户的余额（每个操作实例恰好生效一次）与内存一致；拒绝没有改收款方；按场景的
// 步骤体 / handler 执行次数与生效尝试号。
//
// 运行：source ~/.roost-it/roost-dataengine-it/env.sh，持有 remote-acceptance.lock，
//
//	ROOST_DATAENGINE_IT=1 GOWORK=off go test -tags integration -count=1 -run '^TestRealStepsRun' ./wiring/saga/
//
// transfer 是 d12nest saga 的载荷；Scenario 选择步骤体里的注入行为。
type transfer struct {
	Scenario string `json:"scenario"`
	SagaTag  string `json:"tag,omitempty"`
	From     string `json:"from"`
	To       string `json:"to"`
	Amount   int64  `json:"amount"`
}

// stepProbe 记下场景里的执行：步骤体次数、handler 次数、生效尝试号、最近一次命令与场景信号。
type stepProbe struct {
	mu       sync.Mutex
	counts   map[string]int
	handler  map[string]int
	tried    map[string][]int
	commands map[string]coresaga.Command
	signals  map[string]chan struct{}
}

func newStepProbe() *stepProbe {
	return &stepProbe{counts: map[string]int{}, handler: map[string]int{}, tried: map[string][]int{}, commands: map[string]coresaga.Command{}, signals: map[string]chan struct{}{}}
}

func (p *stepProbe) body(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts[key]++
	return p.counts[key]
}

func (p *stepProbe) count(key string) { p.body(key) }

func (p *stepProbe) counted(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[key]
}

func (p *stepProbe) handled(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handler[key]++
}

func (p *stepProbe) remember(key string, command coresaga.Command) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.commands[key] = command
}

func (p *stepProbe) handlerRuns(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handler[key]
}

func (p *stepProbe) lastCommand(key string) coresaga.Command {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commands[key]
}

func (p *stepProbe) effective(key string, attempt int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tried[key] = append(p.tried[key], attempt)
}

func (p *stepProbe) attempts(key string) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.tried[key]...)
}

func (p *stepProbe) channel(name string) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.signals[name] == nil {
		p.signals[name] = make(chan struct{})
	}
	return p.signals[name]
}

func (p *stepProbe) wait(name string) <-chan struct{} { return p.channel(name) }

func (p *stepProbe) signal(name string) {
	ch := p.channel(name)
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// 手写的余额实体：一个 DAO、一个字段，经正式 DataEngine 仓储从 Mongo 冷加载，提交时整文档 Put。
const (
	stepAccountKind       entity.EntityKind = 203
	stepAccountCollection                   = "d12_accounts"
)

var (
	stepAccountApply        = corenest.NewHandlerName("d12_step_account_apply")
	stepAccountDatabase     atomic.Value // string：本轮的 dataengine.database
	stepAccountHandlerProbe atomic.Pointer[stepProbe]
	registerStepAccount     sync.Once
)

type stepAccountDAO struct {
	id      int64
	balance int64
	tracker coredata.Tracker
}

func (dao *stepAccountDAO) Id() int64                       { return dao.id }
func (dao *stepAccountDAO) SetId(id int64)                  { dao.id = id }
func (*stepAccountDAO) DbName() string                      { return stepAccountDatabase.Load().(string) }
func (*stepAccountDAO) CollName() string                    { return stepAccountCollection }
func (dao *stepAccountDAO) Dirty() entity.IDirty            { return &dao.tracker }
func (dao *stepAccountDAO) CleanDirty()                     { dao.tracker.SelfClean() }
func (*stepAccountDAO) SchemaVersion() uint32               { return 1 }
func (dao *stepAccountDAO) DirtyTracker() *coredata.Tracker { return &dao.tracker }

func (dao *stepAccountDAO) PrepareMutation(change corenest.PersistChange) (coredata.Mutation, error) {
	raw, err := bson.Marshal(bson.M{"_id": dao.id, "balance": dao.balance})
	if err != nil {
		return coredata.Mutation{}, err
	}
	version := dao.tracker.Version()
	return coredata.Mutation{
		Key:  coredata.DocumentKey{Database: dao.DbName(), Resource: stepAccountCollection, ID: dao.id},
		Kind: coredata.MutationPut, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Schema: 1, Codec: "bson-v2", Data: raw,
	}, nil
}

func (dao *stepAccountDAO) AcceptMutation(mutation coredata.Mutation) error {
	return dao.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

func (dao *stepAccountDAO) RestorePersisted(raw []byte, schema uint32, version uint64) error {
	if schema != 1 {
		return coredata.ErrSchemaMismatch
	}
	var doc struct {
		ID      int64 `bson:"_id"`
		Balance int64 `bson:"balance"`
	}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	dao.balance = doc.Balance
	dao.tracker.SetVersion(version)
	dao.tracker.SelfClean()
	return nil
}

type stepAccount struct {
	*entity.EntityBase
	dao *stepAccountDAO
}

func (value *stepAccount) Base() *entity.EntityBase                 { return value.EntityBase }
func (*stepAccount) OnInitFinish(*entity.EntityCreateParam) error   { return nil }
func (*stepAccount) OnDestroy(entity.EntityDestroyReason)           {}
func (*stepAccount) AutoPersist() bool                              { return true }
func (*stepAccount) IsRemoved() bool                                { return false }
func (*stepAccount) SetRemoved()                                    {}
func (*stepAccount) Touch() bool                                    { return true }
func (*stepAccount) UnTouch()                                       {}
func (*stepAccount) ClearBase()                                     {}
func (*stepAccount) IsClear() bool                                  { return false }
func (value *stepAccount) RangeDao(visit func(entity.DaoInterface)) { visit(value.dao) }

// stepCompleter 是原生步骤 handler 在事务里提交结果的那一步：D12 之后是 coresaga.NativeStep，D12 之前的手写胶水
// （性能对照的 glue 变体，与旧 demo 的 gift.NativeStep 相同：inbox.Bind + EmitCompletion）也满足它。handler 只依赖
// 这个接口，所以夹具在 D12 之前的提交上也能编译（性能对照的基线）。
type stepCompleter interface {
	Complete(success bool, reason string) error
}

// ensureStepAccountKind 登记实体与 handler（进程级注册表，只登记一次；handler 经 stepAccountHandlerProbe 找本轮的探针）。
//
// handler 是业务写的原生步骤 handler：改余额（undo 回滚）、MarkPersist，并在同一事务里 step.Complete；limit > 0 时
// 超上限是业务拒绝——不改余额，只 Complete(false, …)。step 为 nil（普通事务，fenced 场景的 touch）时不 Complete。
func ensureStepAccountKind() {
	registerStepAccount.Do(func() {
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: stepAccountKind,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface { return &stepAccountDAO{} }},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				dao, ok := param.Dao[stepAccountCollection].(*stepAccountDAO)
				if !ok {
					return nil, fmt.Errorf("d12: account %d has no DAO", param.Id)
				}
				return &stepAccount{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind), dao: dao}, nil
			},
		})
		corenest.MustRegisterHandlerWithMeta(stepAccountApply, func(entities []entity.IThreadSafeEntity, params []any, _ ...corenest.HandlerOption) (any, error) {
			if len(entities) != 1 || len(params) != 4 {
				return nil, fmt.Errorf("d12: apply wants one account and four params")
			}
			account, ok := entities[0].(*stepAccount)
			key, _ := params[0].(string)
			delta, _ := params[1].(int64)
			limit, _ := params[2].(int64)
			step, _ := params[3].(stepCompleter)
			if !ok {
				return nil, fmt.Errorf("d12: target is %T", entities[0])
			}
			if probe := stepAccountHandlerProbe.Load(); probe != nil {
				probe.handled(key)
			}
			if limit > 0 && account.dao.balance+delta > limit && step != nil {
				return nil, step.Complete(false, "d12: balance cap exceeded")
			}
			previous := account.dao.balance
			account.dao.balance += delta
			corenest.RecordUndo(account.dao, 1, func() error { account.dao.balance = previous; return nil })
			if err := corenest.MarkPersist(account.dao, 1); err != nil {
				return nil, err
			}
			if step == nil {
				return nil, nil
			}
			return nil, step.Complete(true, "")
		}, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict})
	})
}

func waitFor(t *testing.T, ctx context.Context, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); !done(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("timed out: %s", what)
		}
	}
}
