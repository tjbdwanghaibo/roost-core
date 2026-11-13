package cellworld

import (
	"context"
	"errors"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"net"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

func deathFor(row OwnerRecord) DeathFact {
	return DeathFact{ID: "death-1", Target: row.Actor, Life: row.Life, Hit: HitID{Attacker: actorID(1), ID: "hit-1"}, RewardBase: worldBaseSID, State: EntityState{ID: row.Actor, Life: row.Life, HP: 0}}
}

func TestDeathIntentRecoversAcrossCrashAndCannotReviveFromOldSnapshot(t *testing.T) {
	ctx := context.Background()
	r, _ := registryForTest(t, func(context.Context, OwnerProcess) (bool, error) { return false, nil })
	row, err := r.claim(ctx, ownerClaim{Actor: actorID(2), Kind: KindPlayer, Base: worldBaseSID, Entry: 1, Process: OwnerProcess{101, "old"}, Token: "first"})
	if err != nil {
		t.Fatal(err)
	}
	fact := deathFor(row)
	// 故障点：索引已持久、OwnerRecord 尚未写，旧进程即退出。扫表完成同一身份的事实。
	_, _, err = r.deaths.Update(ctx, deathBucket(row.Actor), func(DeathBucket, bool) (DeathBucket, bool, error) {
		return DeathBucket{row.Actor: {Grant: row.Grant, Fact: fact}}, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{cfg: (NodeConfig{Owners: r}).withDefaults(), controls: make(chan queuedSend, 8)}
	n.scanDeaths(ctx, deathBucket(row.Actor))
	recovered, err := r.claim(ctx, ownerClaim{Actor: row.Actor, Kind: KindPlayer, Base: worldBaseSID, Entry: 1, Process: OwnerProcess{102, "new"}, Token: "recover"})
	if err != nil || recovered.Death != fact || recovered.Life != row.Life {
		t.Fatalf("lost death after recovery: %+v %v", recovered, err)
	}
	if _, err = r.ackDeath(ctx, row.Actor, fact.ID, worldBaseSID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.retireEntry(ctx, row.Actor, recovered.Grant, 1); err != nil {
		t.Fatal(err)
	}
	relogin, err := r.claim(ctx, ownerClaim{Actor: row.Actor, Kind: KindPlayer, Base: worldBaseSID, Entry: 2, Process: OwnerProcess{102, "new"}, Token: "relogin"})
	if err != nil || relogin.Death != fact || relogin.Life != row.Life {
		t.Fatalf("login revived dead player: %+v %v", relogin, err)
	}
	bucket, _, _ := r.deaths.Get(ctx, deathBucket(row.Actor))
	if len(bucket.Value) != 0 {
		t.Fatal("acknowledged outbox leaked")
	}
}

func TestOldDeathIntentCannotKillANewWriter(t *testing.T) {
	ctx := context.Background()
	r, _ := registryForTest(t, func(context.Context, OwnerProcess) (bool, error) { return false, nil })
	old, err := r.claim(ctx, ownerClaim{Actor: npcID(1), Kind: KindNPC, Process: OwnerProcess{101, "old"}, Token: "first"})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := r.claim(ctx, ownerClaim{Actor: old.Actor, Kind: KindNPC, Process: OwnerProcess{102, "new"}, Token: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.finishDeath(ctx, DeathIntent{Grant: old.Grant, Fact: deathFor(old)}); !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("stale commit %v", err)
	}
	row, _, _ := r.store.Get(ctx, old.Actor)
	if row.Value != fresh {
		t.Fatal("stale writer mutated owner")
	}
}

func TestDeathRewardAndReceiptRollbackTogether(t *testing.T) {
	p := &testPlayer{EntityBase: fentity.NewEntityBase(playerEntityID(1), fentity.EntityCategoryPlayer, true, kindTPlayer), dao: &undoCell[BaseState]{}}
	reward := &undoCell[int]{}
	fail := true
	n := &Node{cfg: (NodeConfig{SID: worldBaseSID, PlayerEntity: func(int64) int64 { return p.ID() }, SettleDeath: func(BasePlayer, DeathFact) error {
		reward.set(reward.v + 1)
		if fail {
			return errors.New("reward failed")
		}
		return nil
	}}).withDefaults(), controls: make(chan queuedSend, 8)}
	fact := deathFor(OwnerRecord{Actor: npcID(1), Life: 7})
	deliver := func() error {
		_, err := nest.RunDetachedTransaction(context.Background(), nullCommitter{}, "death receipt", func() (any, error) {
			return n.handleDeath([]fentity.IThreadSafeEntity{p}, []any{&wireMsg{From: 101, Death: &fact}})
		})
		return err
	}
	if err := deliver(); err == nil {
		t.Fatal("reward failure swallowed")
	}
	if reward.v != 0 || p.CellDeathReceipt(fact.Target) != 0 || len(n.controls) != 0 {
		t.Fatal("rolled back reward leaked")
	}
	fail = false
	if err := deliver(); err != nil {
		t.Fatal(err)
	}
	if err := deliver(); err != nil {
		t.Fatal(err)
	}
	if reward.v != 1 || p.CellDeathReceipt(fact.Target) != 7 || len(n.controls) != 2 {
		t.Fatalf("reward=%d receipt=%d acks=%d", reward.v, p.CellDeathReceipt(fact.Target), len(n.controls))
	}
}

func TestLethalDamageWaitsForDurableFactAndDuplicateDeliverySettlesOnce(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		return false, m.Kind == kindDeath || m.Kind == kindDamage, 0
	})
	w := startWorld(t, spec)
	target := npcID(811)
	w.spawn(CellID{1, 0}, EntityState{ID: target, X: 10300, Y: 5000, HP: 5})
	w.enter(1, spatial.Point{X: 9700, Y: 5000})
	w.waitKnown(CellID{0, 0}, target)
	hit := w.attack(1, target, 7)
	got := w.waitResults(1, settleWithin)
	if len(got) != 1 || got[0].Outcome != OutcomeApplied || got[0].Hit != hit {
		t.Fatalf("result=%+v", got)
	}
	ctx, cancel := w.ctx()
	defer cancel()
	r := w.cells[CellID{1, 0}].node.cfg.Owners
	eventually(t, settleWithin, "death ack", func() (bool, string) {
		row, found, err := r.store.Get(ctx, target)
		return err == nil && found && row.Value.Death.Hit == hit && row.Value.DeathAcked, "pending death"
	})
	if w.applications()[hit] != 1 {
		t.Fatalf("duplicate death hook: %v", w.applications())
	}
}

func TestRecoveryStateExpiresWithoutMutatingOldSnapshot(t *testing.T) {
	now := time.Unix(1000, 0)
	original := RecoveryState{Revision: 3, Effects: []TimedEffect{{Key: "shield", Stacks: 1, ExpiresAt: now.Add(time.Second).UnixMilli()}, {Key: "passive", Stacks: 2}}, Cooldowns: []Cooldown{{Key: "dash", ReadyAt: now.Add(time.Second).UnixMilli()}}}
	if err := original.Validate(); err != nil {
		t.Fatal(err)
	}
	after := original.At(now.Add(2 * time.Second))
	if len(after.Effects) != 1 || after.Effects[0].Key != "passive" || len(after.Cooldowns) != 0 || len(original.Effects) != 2 || len(original.Cooldowns) != 1 {
		t.Fatalf("expired=%+v old=%+v", after, original)
	}
	n := plainNode(t, worldGrid(t, 1, 1), CellID{}, map[int64]*plainActor{})
	st := ActorState{Role: RoleReal, EntityState: EntityState{ID: npcID(9051), Kind: KindNPC, X: 500, Y: 500, HP: 100, Ver: Version{Epoch: 1, Seq: 7}}, Recovery: original}
	n.stepActor(&st, now.Add(2*time.Second), 50*time.Millisecond)
	if st.Ver.Seq != 8 || len(st.Recovery.Effects) != 1 || len(st.Recovery.Cooldowns) != 0 {
		t.Fatalf("expired recovery has no new snapshot version: %+v", st)
	}
	n.stepActor(&st, now.Add(3*time.Second), 50*time.Millisecond)
	if st.Ver.Seq != 8 {
		t.Fatal("unchanged recovery repeatedly advanced version")
	}
}

// 单机持久 Redis 故障验证：存储暂停时绝不能按未知结果激活；SIGKILL 后从 AOF 重建事实和出生目录。
func TestDeathAndBirthSurviveRedisAOFRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("private Redis process test")
	}
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	_ = listener.Close()
	dir := t.TempDir()
	var server *exec.Cmd
	start := func() {
		server = exec.Command(bin, "--port", strconv.Itoa(addr.Port), "--bind", "127.0.0.1", "--save", "", "--appendonly", "yes", "--appendfsync", "always", "--dir", dir)
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := net.DialTimeout("tcp", addr.String(), 100*time.Millisecond)
			if err == nil {
				conn.Close()
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("private Redis did not start")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	start()
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	client, err := redisdriver.NewClient(&fredis.Config{Addr: addr.String(), MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	live := func(context.Context, OwnerProcess) (bool, error) { return false, nil }
	registry, err := NewRedisOwnerRegistry(client, "death-aof", live)
	if err != nil {
		t.Fatal(err)
	}
	book, err := NewRedisSpawnBook(client, "death-aof")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ids, err := book.Reserve(ctx, 101, []string{"wolf-0"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := registry.claim(ctx, ownerClaim{Actor: npcID(41), Kind: KindNPC, Process: OwnerProcess{101, "a"}, Token: "first"})
	if err != nil {
		t.Fatal(err)
	}
	fact := deathFor(old)
	fact.At = time.Now().UnixMilli()
	if _, err = registry.recordDeath(ctx, old.Grant, fact); err != nil {
		t.Fatal(err)
	}
	if err := server.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = registry.claim(short, ownerClaim{Actor: old.Actor, Kind: KindNPC, Process: OwnerProcess{102, "b"}, Token: "blocked"})
	stop()
	_ = server.Process.Signal(syscall.SIGCONT)
	if err == nil {
		t.Fatal("unavailable authority granted a writer")
	}
	_ = server.Process.Kill()
	_ = server.Wait()
	start()
	restored, err := registry.claim(ctx, ownerClaim{Actor: old.Actor, Kind: KindNPC, Process: OwnerProcess{102, "b"}, Token: "restored"})
	if err != nil || restored.Death != fact || restored.Life != old.Life || restored.Grant.Epoch <= old.Grant.Epoch {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	replay, found, err := registry.deaths.Get(ctx, deathBucket(old.Actor))
	if err != nil || !found || replay.Value[old.Actor].Fact != fact {
		t.Fatalf("outbox missing after restart: %+v %v", replay, err)
	}
	same, err := book.Reserve(ctx, 101, []string{"wolf-0"})
	if err != nil || same["wolf-0"] != ids["wolf-0"] {
		t.Fatalf("birth catalog changed: %v %v", same, err)
	}
	if _, err = registry.ackDeath(ctx, old.Actor, fact.ID, worldBaseSID); err != nil {
		t.Fatal(err)
	}
}

// 已确认授权冲突时，本地死亡待决者必须退出；未知错误保留原身份重试。
func TestDyingOwnerConflictRetiresOnlyTheMatchingLocalGrant(t *testing.T) {
	grid := worldGrid(t, 1, 1)
	n := plainNode(t, grid, CellID{}, map[int64]*plainActor{})
	n.controls = make(chan queuedSend, 8)
	r := NewRegion()
	st := ActorState{EntityState: EntityState{ID: npcID(97), Kind: KindNPC, HP: 0}, Role: RoleDying}
	claimPlain(t, n, &st)
	fact := deathFor(OwnerRecord{Actor: st.ID, Life: st.Life})
	st.Death = &fact
	a := &plainActor{EntityBase: fentity.NewEntityBase(st.ID, fentity.EntityCategoryOther, true, kindTActor), st: st}
	scene := &testScene{EntityBase: fentity.NewEntityBase(-1, fentity.EntityCategoryWorld, true, kindTScene), region: r}
	for _, errResult := range []error{context.DeadlineExceeded, ErrOwnerConflict} {
		_, err := n.handleOwnerResult([]fentity.IThreadSafeEntity{scene, a}, []any{ownerResult{work: n.ownerWork(st), err: errResult}})
		if err != nil {
			t.Fatal(err)
		}
		want := RoleDying
		if errors.Is(errResult, ErrOwnerConflict) {
			want = RoleGone
		}
		if a.st.Role != want {
			t.Fatalf("result %v leaves role %v, want %v", errResult, a.st.Role, want)
		}
	}
	// 上一次生命的迟到结论不能销毁同 ID 的新授权。
	a.st = st
	a.st.Grant.ID = "replacement"
	_, err := n.handleOwnerResult([]fentity.IThreadSafeEntity{scene, a}, []any{ownerResult{work: n.ownerWork(st), err: ErrOwnerConflict}})
	if err != nil || a.st.Role != RoleDying {
		t.Fatalf("stale completion changed new instance: %v %v", a.st.Role, err)
	}
}
