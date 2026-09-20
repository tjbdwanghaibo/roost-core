//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	kitredis "github.com/tjbdwanghaibo/roost-core/redis/driver"

	"github.com/tjbdwanghaibo/roost-core/kit/service/account"
	"github.com/tjbdwanghaibo/roost-core/kit/service/chat"
	"github.com/tjbdwanghaibo/roost-core/kit/service/directory"
	"github.com/tjbdwanghaibo/roost-core/kit/service/global"
	"github.com/tjbdwanghaibo/roost-core/kit/service/global/activity"
	"github.com/tjbdwanghaibo/roost-core/kit/service/mail"
	"github.com/tjbdwanghaibo/roost-core/kit/service/match"
	"github.com/tjbdwanghaibo/roost-core/kit/service/platform"
	"github.com/tjbdwanghaibo/roost-core/kit/service/rank"
	"github.com/tjbdwanghaibo/roost-core/kit/service/session"
)

func client(t *testing.T) fredis.IRedis {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run the cross-service Redis integration tests")
	}
	c, err := kitredis.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatalf("connect %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// prefix is unique per test run, so a failed run leaves nothing that affects
// the next one and two runs can share a Redis.
func prefix(t *testing.T, name string) string {
	t.Helper()
	return fmt.Sprintf("itest:%s:%d", name, time.Now().UnixNano())
}

// --- directory ---

func TestDirectoryRunsOnRedis(t *testing.T) {
	state, err := directory.NewRedisState(client(t), prefix(t, "dir"), 0)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := directory.New(state, directory.Config{
		Normalize: directory.NormalizeLower, DefaultTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	claim, err := dir.Reserve(ctx, "Alice", "acct-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Reserve(ctx, "alice", "acct-2", 0); err == nil {
		t.Fatal("a second owner reserved the same normalized key against real Redis")
	}
	if _, err := dir.Commit(ctx, claim); err != nil {
		t.Fatal(err)
	}
	entry, found, err := dir.Lookup(ctx, "ALICE")
	if err != nil || !found {
		t.Fatalf("lookup: found=%v err=%v", found, err)
	}
	if entry.Owner != "acct-1" {
		t.Fatalf("the committed entry belongs to %q", entry.Owner)
	}
}

// A directory reservation is idempotent for the same owner and therefore NOT
// a mutex. That is documented on the interface, and it cost this repository a
// real defect in account before it was — so it is asserted against real
// storage, where a reader might otherwise assume the backend serializes.
func TestADirectoryIsNotAMutexEvenOnRealRedis(t *testing.T) {
	state, err := directory.NewRedisState(client(t), prefix(t, "dirmutex"), 0)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := directory.New(state, directory.Config{
		Normalize: directory.NormalizeLower, DefaultTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	const racers = 12
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		tokens = map[string]bool{}
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			claim, err := dir.Reserve(ctx, "Alice", "acct-1", 0)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			tokens[claim.Token] = true
		}()
	}
	wg.Wait()

	if len(tokens) != 1 {
		t.Fatalf("one owner racing itself produced %d distinct claims, want 1", len(tokens))
	}
	// The point: every racer SUCCEEDED. Needing "only one attempt may proceed"
	// requires versionstore.Create, which is what account uses for its
	// per-server slot.
	if len(tokens) == 1 && racers > 1 {
		t.Log("all racers shared one claim, as documented: a directory is not a mutex")
	}
}

// --- session ---

func TestSessionRunsOnRedis(t *testing.T) {
	stores, err := session.NewRedisStores(client(t), session.RedisConfig{
		Prefix: prefix(t, "session"), RequestTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	var released []string
	var releasedMu sync.Mutex
	service, err := session.New(session.Config{
		Runs: stores.Runs, Claims: stores.Claims, Requests: stores.Requests,
		TTL: 10 * time.Minute,
		Release: session.ReleaserFunc(func(_ context.Context, _ session.Run, r session.Resource) error {
			releasedMu.Lock()
			defer releasedMu.Unlock()
			released = append(released, r.Kind+":"+r.ID)
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	run, err := service.Enter(ctx, 1, session.EnterRequest{Kind: "dungeon-7", RequestID: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Attach(ctx, 1, run.ID, session.Resource{Kind: "scene", ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Finish(ctx, 1, run.ID, session.StateSucceeded, "cleared"); err != nil {
		t.Fatal(err)
	}
	releasedMu.Lock()
	got := append([]string(nil), released...)
	releasedMu.Unlock()
	if len(got) != 1 || got[0] != "scene:s1" {
		t.Fatalf("released %v, want [scene:s1]", got)
	}
}

// One live run per owner, against real Redis under real concurrency. This is
// the property the insert-only claim exists for, and the one that broke when
// the claim was taken before the run existed.
func TestSessionAllowsOneLiveRunPerOwnerOnRedis(t *testing.T) {
	stores, err := session.NewRedisStores(client(t), session.RedisConfig{
		Prefix: prefix(t, "sessionrace"), RequestTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := session.New(session.Config{
		Runs: stores.Runs, Claims: stores.Claims, Requests: stores.Requests,
		TTL:     10 * time.Minute,
		Release: session.ReleaserFunc(func(context.Context, session.Run, session.Resource) error { return nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	const racers = 12
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		runs = map[string]bool{}
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			run, err := service.Enter(ctx, 42, session.EnterRequest{
				Kind: "dungeon-7", RequestID: fmt.Sprintf("req-%d", i),
			})
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			runs[run.ID] = true
		}(i)
	}
	wg.Wait()

	if len(runs) != 1 {
		t.Fatalf("%d racing enters produced %d runs on real Redis, want 1", racers, len(runs))
	}
}

// --- account ---

func TestAccountRunsOnRedis(t *testing.T) {
	stores, err := account.NewRedisStores(client(t), prefix(t, "account"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var next int64 = 5000
	var idMu sync.Mutex
	service, err := account.New(account.Config{
		Accounts: stores.Accounts, Roles: stores.Roles, Servers: stores.Servers,
		Slots: stores.Slots, Names: stores.Names,
		Verifier: account.VerifierFunc(func(_ context.Context, id account.Identity) (account.Verified, error) {
			if id.Credential != "good" {
				return account.Verified{}, fmt.Errorf("bad credential")
			}
			return account.Verified{Channel: id.Channel, OpenID: id.OpenID}, nil
		}),
		Allocator: account.AllocatorFunc(func(context.Context, int32) (int64, error) {
			idMu.Lock()
			defer idMu.Unlock()
			next++
			return next, nil
		}),
		NameRules:     account.NameValidatorFunc(func(string) error { return nil }),
		SessionSecret: "itest-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := service.UpsertServer(ctx, account.GameServer{ID: 1, Name: "s1", Status: account.ServerOpen}); err != nil {
		t.Fatal(err)
	}
	acct, err := service.Login(ctx, account.Identity{Channel: "store", OpenID: "u1", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	role, err := service.CreateRole(ctx, acct.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.SelectRole(ctx, acct.ID, role.PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ValidateSession(ctx, role.PlayerID, session.Token); err != nil {
		t.Fatal(err)
	}
	// The per-server slot is insert-only, so a second role is refused — the
	// defect that a directory-based slot let through.
	if _, err := service.CreateRole(ctx, acct.ID, 1, "Alicia"); err == nil {
		t.Fatal("a second role on one server was created against real Redis")
	}
}

// The per-server slot must exclude an account racing ITSELF. A directory
// reservation would not, which is the confirmed defect this replaced.
func TestAccountSlotExcludesAnAccountRacingItselfOnRedis(t *testing.T) {
	stores, err := account.NewRedisStores(client(t), prefix(t, "accountrace"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var next int64 = 6000
	var idMu sync.Mutex
	service, err := account.New(account.Config{
		Accounts: stores.Accounts, Roles: stores.Roles, Servers: stores.Servers,
		Slots: stores.Slots, Names: stores.Names,
		Verifier: account.VerifierFunc(func(_ context.Context, id account.Identity) (account.Verified, error) {
			return account.Verified{Channel: id.Channel, OpenID: id.OpenID}, nil
		}),
		Allocator: account.AllocatorFunc(func(context.Context, int32) (int64, error) {
			idMu.Lock()
			defer idMu.Unlock()
			next++
			return next, nil
		}),
		NameRules:     account.NameValidatorFunc(func(string) error { return nil }),
		SessionSecret: "itest-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := service.UpsertServer(ctx, account.GameServer{ID: 1, Name: "s1", Status: account.ServerOpen}); err != nil {
		t.Fatal(err)
	}
	acct, err := service.Login(ctx, account.Identity{Channel: "store", OpenID: "u1", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}

	const racers = 10
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		roles = map[int64]bool{}
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			role, err := service.CreateRole(ctx, acct.ID, 1, fmt.Sprintf("Racer%d", i))
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			roles[role.PlayerID] = true
		}(i)
	}
	wg.Wait()

	if len(roles) != 1 {
		t.Fatalf("%d racing creates produced %d roles on real Redis, want 1", racers, len(roles))
	}
}

// --- match ---

func TestMatchRunsOnRedis(t *testing.T) {
	store, err := match.NewRedisStore(client(t), prefix(t, "match"), match.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	queue := match.Queue{Mode: "ranked", GroupSize: 2, Partition: "eu"}

	first, err := store.Enqueue(ctx, queue, match.Subject{Kind: "player", ID: 1, Score: 100}, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Enqueue(ctx, queue, match.Subject{Kind: "player", ID: 2, Score: 110}, "req-2")
	if err != nil {
		t.Fatal(err)
	}
	// A retry returns the same ticket, through real storage.
	again, err := store.Enqueue(ctx, queue, match.Subject{Kind: "player", ID: 1, Score: 100}, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("a retried enqueue produced ticket %s, first was %s", again.ID, first.ID)
	}
	if _, err := store.Commit(ctx, queue, []string{first.ID, second.ID}); err != nil {
		t.Fatal(err)
	}
	length, err := store.QueueLength(ctx, queue)
	if err != nil {
		t.Fatal(err)
	}
	if length != 0 {
		t.Fatalf("the queue holds %d tickets after the commit, want 0", length)
	}
}

// --- rank ---

func TestRankRunsOnRedis(t *testing.T) {
	store, err := rank.NewRedisStore(client(t), rank.RedisConfig{Prefix: prefix(t, "rank")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	board := rank.Board{ID: "arena", Scope: rank.ScopeServer, ScopeID: 1, Season: 1}
	t.Cleanup(func() { _ = store.Reset(context.Background(), board) })

	for owner, value := range map[int64]int64{1: 10, 2: 30, 3: 20} {
		if _, err := store.Submit(ctx, board, rank.Score{OwnerID: owner, Value: value}, rank.UpdateSet, ""); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.Page(ctx, board, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("the page holds %d entries, want 3", len(page.Entries))
	}
	if page.Entries[0].Score.OwnerID != 2 {
		t.Fatalf("the top entry is owner %d, want 2", page.Entries[0].Score.OwnerID)
	}
}

// --- chat ---

// permissivePolicy exists for this test only. There is deliberately no
// permissive default in the chat package, because a permissive default is
// precisely what the client-settable Trusted bool amounted to.
type permissivePolicy struct{}

func (permissivePolicy) CanPublish(context.Context, chat.Sender, chat.Channel) error { return nil }
func (permissivePolicy) CanRead(context.Context, chat.Sender, chat.Channel) error    { return nil }

func TestChatRunsOnRedis(t *testing.T) {
	registry := chat.NewBodyRegistry()
	if err := registry.Register("text", chat.BodySpec{
		Validator: chat.TextValidator(200), MaxBytes: 512,
	}); err != nil {
		t.Fatal(err)
	}
	store, err := chat.NewRedisStore(client(t), prefix(t, "chat"), chat.Config{
		Policy: permissivePolicy{}, Bodies: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := chat.NewService(chat.ServiceConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	channel := chat.Channel{Kind: chat.ChannelWorld, Target: 7}
	sender := chat.Sender{RoleID: 1, Name: "role-1"}
	publish := chat.PublishRequest{
		Channel: channel, Type: "text", Body: []byte("hello"), RequestID: "req-1",
	}

	if _, err := service.Publish(ctx, sender, publish); err != nil {
		t.Fatal(err)
	}
	// A retry must not produce a second message, through real storage.
	if _, err := service.Publish(ctx, sender, publish); err != nil {
		t.Fatal(err)
	}
	page, err := service.History(ctx, sender, chat.HistoryQuery{Channel: channel, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 {
		t.Fatalf("history holds %d messages after a publish and a retry, want 1", len(page.Messages))
	}
}

// --- global ---

func TestGlobalRunsOnRedis(t *testing.T) {
	stores, err := global.NewRedisStores(client(t), prefix(t, "global"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := global.New(global.Config{Routes: stores.Routes, Leases: stores.Leases})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	binding, err := service.Bind(ctx, 7, "group-a", 100)
	if err != nil {
		t.Fatal(err)
	}
	// A stale epoch is refused by the compare-and-set, against real Redis.
	if _, err := service.BeginMigration(ctx, 7, 200, binding.Epoch+9); err == nil {
		t.Fatal("a stale epoch was accepted against real Redis")
	}
	lease, err := service.AcquireLease(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	// The incarnation fence: a renewal from a process that is not the holder
	// must be refused.
	if _, err := service.RenewLease(ctx, 7, "a-token-from-a-dead-process", nil); err == nil {
		t.Fatal("a foreign incarnation renewed the lease against real Redis")
	}
	if _, err := service.RenewLease(ctx, 7, lease.Incarnation, nil); err != nil {
		t.Fatal(err)
	}
}

// --- platform ---

func TestPlatformRunsOnRedis(t *testing.T) {
	orders, err := platform.NewRedisOrders(client(t), prefix(t, "platform"))
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		grants int
	)
	service, err := platform.New(platform.Config{
		Orders: orders,
		Deliver: platform.DelivererFunc(func(context.Context, platform.Order) error {
			mu.Lock()
			defer mu.Unlock()
			grants++
			return nil
		}),
		Verifier: platform.VerifierFunc(func(_ context.Context, c platform.Credential) (platform.Verified, error) {
			return platform.Verified{Channel: c.Channel, OpenID: c.OpenID}, nil
		}),
		Players:       platform.PlayerResolverFunc(func(context.Context, platform.Verified) (int64, error) { return 1001, nil }),
		SessionSecret: "itest-session",
		PaymentSecret: "itest-payment",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	raw := []byte(`{"order_id":"o1","player_id":1001,"channel":"store",` +
		`"product_id":"gems-100","amount_minor":499,"currency":"USD"}`)
	signature := platform.SignPayload(raw, "itest-payment")

	if _, err := service.HandleCallback(ctx, raw, signature); err != nil {
		t.Fatal(err)
	}
	// The replay: the same signed bytes, ten more times. A valid signature is
	// necessary and not sufficient, and the insert-only order is what makes
	// the difference — asserted here against real storage.
	for i := 0; i < 10; i++ {
		receipt, err := service.HandleCallback(ctx, raw, signature)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if !receipt.Replayed {
			t.Fatalf("replay %d was not reported as a replay", i)
		}
	}
	mu.Lock()
	got := grants
	mu.Unlock()
	if got != 1 {
		t.Fatalf("eleven callbacks granted the goods %d times on real Redis, want 1", got)
	}
}

// Concurrent callbacks for one order deliver once, on real Redis. A
// read-then-write dedupe is a dedupe two racers both pass, and only a real
// backend can establish that the insert-only path holds.
func TestPlatformConcurrentCallbacksDeliverOnceOnRedis(t *testing.T) {
	orders, err := platform.NewRedisOrders(client(t), prefix(t, "platformrace"))
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		grants int
	)
	service, err := platform.New(platform.Config{
		Orders: orders,
		Deliver: platform.DelivererFunc(func(context.Context, platform.Order) error {
			mu.Lock()
			defer mu.Unlock()
			grants++
			return nil
		}),
		Verifier: platform.VerifierFunc(func(_ context.Context, c platform.Credential) (platform.Verified, error) {
			return platform.Verified{Channel: c.Channel, OpenID: c.OpenID}, nil
		}),
		Players:       platform.PlayerResolverFunc(func(context.Context, platform.Verified) (int64, error) { return 1001, nil }),
		SessionSecret: "itest-session",
		PaymentSecret: "itest-payment",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	raw := []byte(`{"order_id":"o1","player_id":1001,"channel":"store",` +
		`"product_id":"gems-100","amount_minor":499,"currency":"USD"}`)
	signature := platform.SignPayload(raw, "itest-payment")

	const racers = 12
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			_, _ = service.HandleCallback(ctx, raw, signature)
		}()
	}
	wg.Wait()

	mu.Lock()
	got := grants
	mu.Unlock()
	if got != 1 {
		t.Fatalf("%d concurrent callbacks granted the goods %d times on real Redis, want 1", racers, got)
	}
}

// --- mail ---

func TestMailRunsOnRedis(t *testing.T) {
	stores, err := mail.NewRedisStores(client(t), mail.RedisConfig{
		Prefix: prefix(t, "mail"), SendTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := mail.New(mail.Config{
		Envelopes: stores.Envelopes, Mailboxes: stores.Mailboxes, Sends: stores.Sends,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	sent, err := service.Send(ctx, mail.SendRequest{
		Audience: mail.AudienceDirect, Recipients: []int64{7},
		Subject: "reward", Attachment: []byte("100 gold"),
		ExpiresInSeconds: 3600, RequestID: "req-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := service.ReserveClaim(ctx, 7, sent.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CommitClaim(ctx, 7, sent.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	summary, err := service.Summary(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Unread != 0 {
		t.Fatalf("the unread count is %d after the claim, want 0", summary.Unread)
	}
}

// --- the one thing the thin constructors exist to own ---

// everyNamespace is every key namespace the per-package constructors create.
//
// It is written out rather than derived, on purpose: this list is the
// specification, and a constructor that stops writing under its declared
// namespace should fail here rather than have the test follow it.
var everyNamespace = []string{
	":account:acct:", ":account:role:", ":account:srv:", ":account:slot:", ":account:names:name:",
	":chat:ch:",
	":directory:name:",
	":global:route:", ":global:lease:",
	// activity was part of global until the transport generator made the
	// packaging visible; these six namespaces were never covered here while
	// they sat under the global prefix and nothing drove them.
	":activity:act:", ":activity:part:", ":activity:req:",
	":activity:audit:", ":activity:disp:", ":activity:win:",
	// The per-(group, game) owed index, added with U-0257 so a game server can
	// enumerate what it is still owed. It arrived without a line here, and
	// this guard is exactly what noticed.
	":activity:owed:",
	":mail:env:", ":mail:box:", ":mail:send:",
	":match:queue:",
	":platform:order:",
	":rank:z:", ":rank:o:",
	":session:run:", ":session:claim:", ":session:req:",
}

// The per-package constructors contribute no storage logic. Their entire job
// is key namespacing: several stores share one prefix and must not collide
// with each other, and a caller assembling them by hand gets that right until
// the day it does not.
//
// So it is tested by inspection of the live keyspace rather than by trusting
// the string concatenation. A collision here is not a subtle bug: two stores
// on one key means one service's write decodes as the other's value, and the
// version compare that is supposed to protect both compares across them.
//
// Note what this cannot catch on its own. Two stores under one namespace only
// collide when their rendered keys also coincide — account's account ids are
// strings and its player ids are numbers, so those two would share a namespace
// for a long time before anyone noticed. That is why the assertion is "each
// declared namespace received a write", not "no two keys are equal": the
// former fails the moment a namespace disappears, the latter passes right up
// until the day the formats overlap.
func TestPerPackageKeyNamespacesDoNotCollide(t *testing.T) {
	c := client(t)
	root := prefix(t, "keyspace")

	driveEveryPackage(t, c, root)

	keys := scanKeys(t, c, root+"*")
	if len(keys) == 0 {
		t.Fatal("no keys were written; this test would pass vacuously")
	}
	namespaces := map[string]int{}
	for _, key := range keys {
		owner := namespaceOf(key, root)
		if owner == "" {
			t.Fatalf("key %q does not fall under a declared namespace; an unnamespaced key is "+
				"one two stores can both claim", key)
		}
		namespaces[owner]++
	}
	for _, want := range everyNamespace {
		if namespaces[want] == 0 {
			t.Fatalf("nothing was written under %q; either the store was not driven or its keys "+
				"landed in another namespace. observed: %v", want, namespaces)
		}
	}
	t.Logf("%d keys across %d namespaces: %v", len(keys), len(namespaces), namespaces)
}

// driveEveryPackage writes at least once through every store every
// constructor builds, under one root prefix.
func driveEveryPackage(t *testing.T, c fredis.IRedis, root string) {
	t.Helper()
	ctx := context.Background()

	// account: accounts, roles, servers, slots, and the name directory.
	acctStores, err := account.NewRedisStores(c, root+":account", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var nextPlayer int64 = 9000
	var playerMu sync.Mutex
	acctSvc, err := account.New(account.Config{
		Accounts: acctStores.Accounts, Roles: acctStores.Roles, Servers: acctStores.Servers,
		Slots: acctStores.Slots, Names: acctStores.Names,
		Verifier: account.VerifierFunc(func(_ context.Context, id account.Identity) (account.Verified, error) {
			return account.Verified{Channel: id.Channel, OpenID: id.OpenID}, nil
		}),
		Allocator: account.AllocatorFunc(func(context.Context, int32) (int64, error) {
			playerMu.Lock()
			defer playerMu.Unlock()
			nextPlayer++
			return nextPlayer, nil
		}),
		NameRules:     account.NameValidatorFunc(func(string) error { return nil }),
		SessionSecret: "itest-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acctSvc.UpsertServer(ctx, account.GameServer{ID: 1, Name: "s1", Status: account.ServerOpen}); err != nil {
		t.Fatal(err)
	}
	acct, err := acctSvc.Login(ctx, account.Identity{Channel: "store", OpenID: "u1", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acctSvc.CreateRole(ctx, acct.ID, 1, "Alice"); err != nil {
		t.Fatal(err)
	}

	// chat
	registry := chat.NewBodyRegistry()
	if err := registry.Register("text", chat.BodySpec{Validator: chat.TextValidator(200)}); err != nil {
		t.Fatal(err)
	}
	chatStore, err := chat.NewRedisStore(c, root+":chat", chat.Config{
		Policy: permissivePolicy{}, Bodies: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	chatSvc, err := chat.NewService(chat.ServiceConfig{Store: chatStore})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chatSvc.Publish(ctx, chat.Sender{RoleID: 1, Name: "role-1"}, chat.PublishRequest{
		Channel: chat.Channel{Kind: chat.ChannelWorld, Target: 7},
		Type:    "text", Body: []byte("hello"), RequestID: "r1",
	}); err != nil {
		t.Fatal(err)
	}

	// directory
	dirState, err := directory.NewRedisState(c, root+":directory", 0)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := directory.New(dirState, directory.Config{
		Normalize: directory.NormalizeLower, DefaultTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Reserve(ctx, "Zed", "owner-1", 0); err != nil {
		t.Fatal(err)
	}

	// global: routes and leases.
	globalStores, err := global.NewRedisStores(c, root+":global")
	if err != nil {
		t.Fatal(err)
	}
	globalSvc, err := global.New(global.Config{Routes: globalStores.Routes, Leases: globalStores.Leases})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := globalSvc.Bind(ctx, 7, "group-a", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := globalSvc.AcquireLease(ctx, 7); err != nil {
		t.Fatal(err)
	}

	// activity: activities, participants, the request ledger, notify audits,
	// dispatches, and the group's pending window.
	//
	// All six namespaces need writing, and getting there means driving the
	// activity to completion rather than just opening one: the dispatch
	// records only exist once an aggregation finished, and the audit log only
	// once a notify was refused. That is the point of the walk — a namespace
	// nothing drives is a namespace this test cannot speak for.
	actStores, err := activity.NewRedisStores(c, root+":activity", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	actSvc, err := activity.New(activity.Config{
		Activities: actStores.Activities, Participants: actStores.Participants,
		Ledger: actStores.Ledger, Audits: actStores.Audits,
		Dispatches: actStores.Dispatches, Windows: actStores.Windows,
		ReservationTTL: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	actKey := activity.Key{GroupID: "group-a", ActivityID: "act-1", Phase: activity.PhaseSettle}
	if _, err := actSvc.OpenActivity(ctx, actKey, []int32{7}); err != nil {
		t.Fatal(err)
	}
	if _, err := actSvc.ApplyProgress(ctx, actKey, "p1", "r1", activity.ProgressDelta{Score: 10}); err != nil {
		t.Fatal(err)
	}
	// A game that is not expected by this activity: refused, and the refusal
	// writes the audit. It is the only way an audit record comes into being.
	if _, err := actSvc.NotifyPhase(ctx, actKey, 99); err == nil {
		t.Fatal("a notification from an unexpected game server was accepted")
	}
	if _, err := actSvc.NotifyPhase(ctx, actKey, 7); err != nil {
		t.Fatal(err)
	}

	// mail: envelopes, mailboxes, send ledger.
	mailStores, err := mail.NewRedisStores(c, mail.RedisConfig{
		Prefix: root + ":mail", SendTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	mailSvc, err := mail.New(mail.Config{
		Envelopes: mailStores.Envelopes, Mailboxes: mailStores.Mailboxes, Sends: mailStores.Sends,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mailSvc.Send(ctx, mail.SendRequest{
		Audience: mail.AudienceDirect, Recipients: []int64{1}, Subject: "s",
		ExpiresInSeconds: 3600, RequestID: "r1",
	}); err != nil {
		t.Fatal(err)
	}

	// match
	matchStore, err := match.NewRedisStore(c, root+":match", match.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := matchStore.Enqueue(ctx,
		match.Queue{Mode: "ranked", GroupSize: 2, Partition: "eu"},
		match.Subject{Kind: "player", ID: 1, Score: 100}, "r1"); err != nil {
		t.Fatal(err)
	}

	// platform
	orders, err := platform.NewRedisOrders(c, root+":platform")
	if err != nil {
		t.Fatal(err)
	}
	platformSvc, err := platform.New(platform.Config{
		Orders:  orders,
		Deliver: platform.DelivererFunc(func(context.Context, platform.Order) error { return nil }),
		Verifier: platform.VerifierFunc(func(_ context.Context, cr platform.Credential) (platform.Verified, error) {
			return platform.Verified{Channel: cr.Channel, OpenID: cr.OpenID}, nil
		}),
		Players:       platform.PlayerResolverFunc(func(context.Context, platform.Verified) (int64, error) { return 1001, nil }),
		SessionSecret: "itest-session", PaymentSecret: "itest-payment",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"order_id":"o1","player_id":1001,"channel":"store",` +
		`"product_id":"gems-100","amount_minor":499,"currency":"USD"}`)
	if _, err := platformSvc.HandleCallback(ctx, raw, platform.SignPayload(raw, "itest-payment")); err != nil {
		t.Fatal(err)
	}

	// rank: the sorted set and the owner hash.
	rankStore, err := rank.NewRedisStore(c, rank.RedisConfig{Prefix: root + ":rank"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rankStore.Submit(ctx,
		rank.Board{ID: "arena", Scope: rank.ScopeServer, ScopeID: 1, Season: 1},
		rank.Score{OwnerID: 1, Value: 10}, rank.UpdateSet, ""); err != nil {
		t.Fatal(err)
	}

	// session: runs, claims, request ledger.
	sessionStores, err := session.NewRedisStores(c, session.RedisConfig{
		Prefix: root + ":session", RequestTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionSvc, err := session.New(session.Config{
		Runs: sessionStores.Runs, Claims: sessionStores.Claims, Requests: sessionStores.Requests,
		TTL:     10 * time.Minute,
		Release: session.ReleaserFunc(func(context.Context, session.Run, session.Resource) error { return nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionSvc.Enter(ctx, 1, session.EnterRequest{Kind: "d7", RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
}

// namespaceOf returns the longest declared namespace a key falls under, or "".
//
// Longest wins because one namespace can be a prefix of another —
// ":account:names:name:" contains ":account:names" — and picking the shorter
// one would attribute a key to a namespace that does not own it.
func namespaceOf(key, root string) string {
	best := ""
	for _, candidate := range everyNamespace {
		if len(candidate) > len(best) && strings.Contains(key, root+candidate) {
			best = candidate
		}
	}
	return best
}

// scanKeys walks the keyspace with SCAN rather than KEYS, because KEYS blocks
// the server for the duration — a habit worth keeping even in a test, since
// tests are where people copy patterns from.
func scanKeys(t *testing.T, c fredis.IRedis, pattern string) []string {
	t.Helper()
	raw, err := c.Eval(context.Background(), `
local cursor = "0"
local found = {}
repeat
  local reply = redis.call('SCAN', cursor, 'MATCH', ARGV[1], 'COUNT', 500)
  cursor = reply[1]
  for _, key in ipairs(reply[2]) do found[#found+1] = key end
until cursor == "0"
return found`, nil, pattern)
	if err != nil {
		t.Fatalf("scan %q: %v", pattern, err)
	}
	values, ok := raw.([]any)
	if !ok {
		t.Fatalf("scan returned %T, want a list", raw)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			out = append(out, typed)
		case []byte:
			out = append(out, string(typed))
		default:
			t.Fatalf("scan returned element %T", value)
		}
	}
	return out
}
