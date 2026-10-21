package mail

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 全服邮件“存一份 + 拉取合并”（DESIGN-TODO D17，维护者 2026-10-20 决定 A）。
//
// 修前：mail 没有内置投递策略，AudienceBroadcast 只能交给业务写的 Deliverer；广播发出之后才登录、
// 或者发送时离线的玩家能不能收到，取决于业务自己维护的广播索引与每玩家水位。这里的用例钉住框架
// 提供的那一份：登录拉一次、List / Summary 前再补一次、按全局邮件 ID 去重、附件只领一次、删除后
// 不重现、过期、注册时间、撤回与三种范围。

// lazyHarness 是带 LazyBroadcast（内存索引）的 harness。
func lazyHarness(t *testing.T, cfg LazyBroadcastConfig) (*harness, *LazyBroadcast) {
	t.Helper()
	if cfg.Index == nil {
		cfg.Index = NewMemoryBroadcastIndex()
	}
	lazy := NewLazyBroadcast(cfg)
	h := newHarness(t, func(c *Config) { c.Broadcast = lazy })
	return h, lazy
}

func broadcastTo(scope, requestID string) SendRequest {
	return SendRequest{
		Audience: AudienceBroadcast, Scope: scope,
		Subject: "maintenance compensation", Body: "sorry for the downtime",
		ExpiresInSeconds: 7 * 24 * 3600, RequestID: requestID,
	}
}

// veteran 是发送前一天注册、在 1 服、属于公会 5 的玩家。
func veteran(h *harness) BroadcastProfile {
	return BroadcastProfile{
		Scopes:           []string{ServerScope(1), GuildScope(5)},
		RegisteredAtUnix: h.clock.Now().Add(-24 * time.Hour).Unix(),
	}
}

func listIDs(t *testing.T, h *harness, playerID int64) map[string]Item {
	t.Helper()
	page, err := h.service.List(context.Background(), playerID, "", MaxPageSize)
	if err != nil {
		t.Fatalf("list %d: %v", playerID, err)
	}
	out := map[string]Item{}
	for _, item := range page.Items {
		out[item.Envelope.ID] = item
	}
	return out
}

// 发送时离线（还没有邮箱）的玩家，登录时拉一次就收到。
func TestLazyBroadcastReachesAPlayerWhoLogsInAfterTheSend(t *testing.T) {
	h, _ := lazyHarness(t, LazyBroadcastConfig{})
	ctx := context.Background()
	sent := mustSend(t, h, broadcastTo("", "all-1"))

	// 没登录过（没有拉取画像）的玩家：List 不合并，邮箱仍是空的。登录才是第一次拉取。
	if items := listIDs(t, h, 7); len(items) != 0 {
		t.Fatalf("a player who never logged in lists %d mails, want 0 before the login pull", len(items))
	}
	summary, err := h.service.PullBroadcasts(ctx, 7, veteran(h))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if summary.Unread != 1 {
		t.Fatalf("unread after the login pull = %d, want 1", summary.Unread)
	}
	items := listIDs(t, h, 7)
	item, ok := items[sent.ID]
	if !ok || len(items) != 1 {
		t.Fatalf("listed %v, want exactly the broadcast %s", items, sent.ID)
	}
	if item.Status != StatusUnread || item.GlobalID != 1 {
		t.Fatalf("merged mail = %+v, want unread with global id 1", item)
	}
}

// 已登录（拉取过画像）的在线玩家，广播在登录之后发出：List 前检查一次就合并，Summary 同样。
func TestLazyBroadcastReachesAnOnlinePlayerOnListAndSummary(t *testing.T) {
	h, _ := lazyHarness(t, LazyBroadcastConfig{})
	ctx := context.Background()
	if _, err := h.service.PullBroadcasts(ctx, 7, veteran(h)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.PullBroadcasts(ctx, 8, veteran(h)); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(time.Minute)
	sent := mustSend(t, h, broadcastTo("", "all-1"))

	if _, ok := listIDs(t, h, 7)[sent.ID]; !ok {
		t.Fatalf("an online player's List did not merge the broadcast sent after login")
	}
	summary, err := h.service.Summary(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Unread != 1 {
		t.Fatalf("an online player's Summary = %d unread, want 1", summary.Unread)
	}
}

// 并发登录 + List + Summary + 重复拉取：每封全局邮件只合并一次，未读数不翻倍。
func TestLazyBroadcastMergesEachGlobalMailOnceUnderConcurrency(t *testing.T) {
	h, _ := lazyHarness(t, LazyBroadcastConfig{})
	ctx := context.Background()
	profile := veteran(h)
	if _, err := h.service.PullBroadcasts(ctx, 7, profile); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		mustSend(t, h, broadcastTo("", fmt.Sprintf("all-%d", i)))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 48)
	for i := 0; i < 48; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			switch i % 3 {
			case 0:
				_, err = h.service.PullBroadcasts(ctx, 7, profile)
			case 1:
				_, err = h.service.List(ctx, 7, "", 0)
			default:
				_, err = h.service.Summary(ctx, 7)
			}
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read: %v", err)
	}
	box, _, err := h.service.Mailbox(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(box.Entries) != 3 || box.Unread != 3 || box.BroadcastWatermark != 3 {
		t.Fatalf("mailbox has %d entries, unread %d, watermark %d; want 3, 3, 3",
			len(box.Entries), box.Unread, box.BroadcastWatermark)
	}
}

// 合并后的附件只领一次：领完再拉取 / 再 List 不会复活成可领。范围邮件的领取不要求调用方再给
// scope：邮箱里那条由合并写入（GlobalID 非零），合并时已经核对过范围。
func TestLazyBroadcastAttachmentIsClaimedOnce(t *testing.T) {
	h, _ := lazyHarness(t, LazyBroadcastConfig{})
	ctx := context.Background()
	sent := mustSend(t, h, withAttachment(broadcastTo(ServerScope(1), "srv-1"), "gold:100"))
	if _, err := h.service.PullBroadcasts(ctx, 7, veteran(h)); err != nil {
		t.Fatal(err)
	}
	claim, err := h.service.ReserveClaim(ctx, 7, sent.ID, "")
	if err != nil {
		t.Fatalf("reserve a merged server mail without restating its scope: %v", err)
	}
	if _, err := h.service.CommitClaim(ctx, 7, sent.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.PullBroadcasts(ctx, 7, veteran(h)); err != nil {
		t.Fatal(err)
	}
	if item := listIDs(t, h, 7)[sent.ID]; item.Status != StatusClaimed || item.Claimable {
		t.Fatalf("after claim + re-pull the mail is %+v, want claimed and not claimable", item)
	}
	if _, err := h.service.ReserveClaim(ctx, 7, sent.ID, ""); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("second reserve = %v, want ErrAlreadyClaimed", err)
	}
	// 没有合并到这封邮件的玩家（不在 1 服）不能凭邮件 id 领。
	other := BroadcastProfile{Scopes: []string{ServerScope(2)}, RegisteredAtUnix: veteran(h).RegisteredAtUnix}
	if _, err := h.service.PullBroadcasts(ctx, 9, other); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.ReserveClaim(ctx, 9, sent.ID, ""); !errors.Is(err, ErrNotRecipient) {
		t.Fatalf("a player outside the scope reserved = %v, want ErrNotRecipient", err)
	}
}

// 删除后不再被重新合并。删除记在个人这份上；即使那一条后来被保留策略 / 过期清掉，水位仍然挡住
// 重新合并（这里直接把条目从存储里抹掉来模拟）。
func TestLazyBroadcastDeletedMailDoesNotReturn(t *testing.T) {
	h, _ := lazyHarness(t, LazyBroadcastConfig{})
	ctx := context.Background()
	sent := mustSend(t, h, broadcastTo("", "all-1"))
	if _, err := h.service.PullBroadcasts(ctx, 7, veteran(h)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Delete(ctx, 7, sent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.PullBroadcasts(ctx, 7, veteran(h)); err != nil {
		t.Fatal(err)
	}
	if items := listIDs(t, h, 7); len(items) != 0 {
		t.Fatalf("a deleted broadcast is listed again: %v", items)
	}
	// 抹掉条目与墓碑，只留水位。
	if _, _, err := h.mailboxes.Update(ctx, 7, func(current Mailbox, _ bool) (Mailbox, bool, error) {
		current = current.clone()
		delete(current.Entries, sent.ID)
		delete(current.SettledClaims, sent.ID)
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	// 账本头不动时合并根本不会开始；再发一封让账本头越过水位，逼合并逐封判断。
	h.clock.advance(time.Minute)
	newer := mustSend(t, h, broadcastTo("", "all-2"))
	summary, err := h.service.PullBroadcasts(ctx, 7, veteran(h))
	if err != nil {
		t.Fatal(err)
	}
	items := listIDs(t, h, 7)
	if _, back := items[sent.ID]; back || len(items) != 1 || summary.Unread != 1 {
		t.Fatalf("the watermark did not stop a re-merge: listed %v, unread %d; want only %s", items, summary.Unread, newer.ID)
	}
}

// 全局邮件过期后不再合并；已合并的那份按个人邮件的有效期（信封自身的过期时间）消失；过期记录在下一次
// 发布时从索引回收，索引条目数有上界。
func TestLazyBroadcastExpiry(t *testing.T) {
	h, lazy := lazyHarness(t, LazyBroadcastConfig{MaxActive: 2})
	ctx := context.Background()
	early := veteran(h)
	if _, err := h.service.PullBroadcasts(ctx, 7, early); err != nil {
		t.Fatal(err)
	}
	short := broadcastTo("", "short")
	short.ExpiresInSeconds = 3600
	sent := mustSend(t, h, short)
	if _, ok := listIDs(t, h, 7)[sent.ID]; !ok {
		t.Fatal("not merged before expiry")
	}
	mustSend(t, h, broadcastTo("", "long"))
	// 上界：未过期 2 条时第三封被拒绝，而不是无限增长。
	if _, err := h.service.Send(ctx, broadcastTo("", "third")); !errors.Is(err, ErrConflict) {
		t.Fatalf("send over the active bound = %v, want refused", err)
	}

	h.clock.advance(2 * time.Hour)
	// 过期之后才第一次登录的玩家：拿不到过期的那封，拿得到还有效的那封。
	if _, err := h.service.PullBroadcasts(ctx, 8, early); err != nil {
		t.Fatal(err)
	}
	items := listIDs(t, h, 8)
	if _, ok := items[sent.ID]; ok || len(items) != 1 {
		t.Fatalf("a player pulling after expiry listed %v, want only the unexpired broadcast", items)
	}
	// 已合并的那份随它自己的有效期消失。
	if _, ok := listIDs(t, h, 7)[sent.ID]; ok {
		t.Fatal("the merged copy outlived its expiry")
	}
	// 回收：重试第三封（同一 RequestID）时过期记录被清掉，腾出位置。
	if _, err := h.service.Send(ctx, broadcastTo("", "third")); err != nil {
		t.Fatalf("send after the expired record is reclaimable: %v", err)
	}
	ledger, err := lazy.index.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Mails) != 2 || ledger.Head != 3 {
		t.Fatalf("ledger holds %d mails at head %d, want 2 at head 3 (the expired one reclaimed)", len(ledger.Mails), ledger.Head)
	}
}

// 框架内置条件：注册时间早于发送时间。发送之后才注册的玩家收不到旧补偿。
func TestLazyBroadcastSkipsPlayersRegisteredAfterTheSend(t *testing.T) {
	h, _ := lazyHarness(t, LazyBroadcastConfig{})
	ctx := context.Background()
	sent := mustSend(t, h, broadcastTo("", "all-1"))
	h.clock.advance(time.Hour)
	newcomer := BroadcastProfile{RegisteredAtUnix: h.clock.Now().Unix()}
	if _, err := h.service.PullBroadcasts(ctx, 8, newcomer); err != nil {
		t.Fatal(err)
	}
	if items := listIDs(t, h, 8); len(items) != 0 {
		t.Fatalf("a player registered after the send received %v", items)
	}
	if _, err := h.service.PullBroadcasts(ctx, 7, veteran(h)); err != nil {
		t.Fatal(err)
	}
	if _, ok := listIDs(t, h, 7)[sent.ID]; !ok {
		t.Fatal("a player registered before the send did not receive it")
	}
	// 没有注册时间就无法判断：拒绝，而不是当作老玩家。
	if _, err := h.service.PullBroadcasts(ctx, 9, BroadcastProfile{}); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("pull without a registration time = %v, want ErrRequestInvalid", err)
	}
}

// 业务谓词：在内置条件之后再过滤。被拒绝的不再重新判断（水位越过它）。
func TestLazyBroadcastAppliesTheBusinessPredicate(t *testing.T) {
	h, _ := lazyHarness(t, LazyBroadcastConfig{
		Eligible: func(profile BroadcastProfile, _ GlobalMail) bool { return profile.Attributes["level"] >= 10 },
	})
	ctx := context.Background()
	sent := mustSend(t, h, broadcastTo("", "all-1"))
	low, high := veteran(h), veteran(h)
	low.Attributes = map[string]int64{"level": 3}
	high.Attributes = map[string]int64{"level": 30}
	if _, err := h.service.PullBroadcasts(ctx, 7, low); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.PullBroadcasts(ctx, 8, high); err != nil {
		t.Fatal(err)
	}
	if len(listIDs(t, h, 7)) != 0 {
		t.Fatal("the predicate did not filter the low-level player")
	}
	if _, ok := listIDs(t, h, 8)[sent.ID]; !ok {
		t.Fatal("the predicate filtered the high-level player")
	}
}

// 撤回：未合并的不再合并，已合并的默认不动；重复撤回是 no-op，撤回不存在的报 ErrMailMissing。
func TestLazyBroadcastRevoke(t *testing.T) {
	h, _ := lazyHarness(t, LazyBroadcastConfig{})
	ctx := context.Background()
	first := mustSend(t, h, broadcastTo("", "all-1"))
	second := mustSend(t, h, broadcastTo("", "all-2"))
	if _, err := h.service.PullBroadcasts(ctx, 7, veteran(h)); err != nil {
		t.Fatal(err)
	}
	revoked, err := h.service.RevokeBroadcast(ctx, first.ID)
	if err != nil || !revoked {
		t.Fatalf("revoke = %v, %v; want true", revoked, err)
	}
	if again, err := h.service.RevokeBroadcast(ctx, first.ID); err != nil || again {
		t.Fatalf("second revoke = %v, %v; want false, nil", again, err)
	}
	if _, err := h.service.RevokeBroadcast(ctx, "nope"); !errors.Is(err, ErrMailMissing) {
		t.Fatalf("revoke unknown = %v, want ErrMailMissing", err)
	}
	if _, err := h.service.PullBroadcasts(ctx, 8, veteran(h)); err != nil {
		t.Fatal(err)
	}
	later := listIDs(t, h, 8)
	if _, ok := later[first.ID]; ok {
		t.Fatal("a revoked broadcast was merged into a mailbox that had not merged it yet")
	}
	if _, ok := later[second.ID]; !ok {
		t.Fatal("revoking one broadcast hid another")
	}
	if _, ok := listIDs(t, h, 7)[first.ID]; !ok {
		t.Fatal("revoking removed an already merged copy; the default leaves it")
	}
}

// 范围与 AudienceBroadcast 一致：全服（空 scope）、单服、公会。
func TestLazyBroadcastHonoursTheThreeScopes(t *testing.T) {
	h, _ := lazyHarness(t, LazyBroadcastConfig{})
	ctx := context.Background()
	all := mustSend(t, h, broadcastTo("", "all"))
	server1 := mustSend(t, h, broadcastTo(ServerScope(1), "s1"))
	server2 := mustSend(t, h, broadcastTo(ServerScope(2), "s2"))
	guild5 := mustSend(t, h, broadcastTo(GuildScope(5), "g5"))
	registered := veteran(h).RegisteredAtUnix

	if _, err := h.service.PullBroadcasts(ctx, 7, BroadcastProfile{Scopes: []string{ServerScope(1), GuildScope(5)}, RegisteredAtUnix: registered}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.PullBroadcasts(ctx, 8, BroadcastProfile{Scopes: []string{ServerScope(2)}, RegisteredAtUnix: registered}); err != nil {
		t.Fatal(err)
	}
	cases := map[int64]map[string]bool{
		7: {all.ID: true, server1.ID: true, server2.ID: false, guild5.ID: true},
		8: {all.ID: true, server1.ID: false, server2.ID: true, guild5.ID: false},
	}
	for player, want := range cases {
		got := listIDs(t, h, player)
		for id, expected := range want {
			if _, ok := got[id]; ok != expected {
				t.Errorf("player %d has %s = %v, want %v", player, id, ok, expected)
			}
		}
	}
	// 入会之后重新拉取（画像变了）：入会后发出的公会邮件到达。
	if _, err := h.service.PullBroadcasts(ctx, 8, BroadcastProfile{Scopes: []string{ServerScope(2), GuildScope(5)}, RegisteredAtUnix: registered}); err != nil {
		t.Fatal(err)
	}
	afterJoin := mustSend(t, h, broadcastTo(GuildScope(5), "g5-after"))
	if _, ok := listIDs(t, h, 8)[afterJoin.ID]; !ok {
		t.Fatal("a guild mail sent after the player joined (and re-pulled) did not arrive")
	}
}

// 邮箱满（全是未读）时合并停在那里、不丢：水位不越过没放进去的那封，腾出位置后补上。
func TestLazyBroadcastWaitsForRoomInsteadOfDropping(t *testing.T) {
	h, _ := lazyHarness(t, LazyBroadcastConfig{})
	ctx := context.Background()
	if _, err := h.service.PullBroadcasts(ctx, 7, veteran(h)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxMailboxEntries; i++ {
		req := directTo(7)
		req.RequestID = fmt.Sprintf("fill-%d", i)
		mustSend(t, h, req)
	}
	sent := mustSend(t, h, broadcastTo("", "all-1"))
	if _, ok := listIDs(t, h, 7)[sent.ID]; ok {
		t.Fatal("a full mailbox took the broadcast")
	}
	box, _, _ := h.service.Mailbox(ctx, 7)
	if box.BroadcastWatermark != 0 {
		t.Fatalf("watermark moved to %d past a broadcast that did not fit", box.BroadcastWatermark)
	}
	page, err := h.service.List(ctx, 7, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Delete(ctx, 7, page.Items[0].Envelope.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := listIDs(t, h, 7)[sent.ID]; !ok {
		t.Fatal("the broadcast did not arrive once there was room")
	}
}

// 没配置 LazyBroadcast 时拉取是无害的 no-op（登录端点可以无条件调用）；配置了却没有索引时 New 拒绝。
func TestLazyBroadcastConfiguration(t *testing.T) {
	plain := newHarness(t)
	if _, err := plain.service.PullBroadcasts(context.Background(), 7, BroadcastProfile{}); err != nil {
		t.Fatalf("pull without a lazy strategy = %v, want a no-op", err)
	}
	if _, err := plain.service.RevokeBroadcast(context.Background(), "x"); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("revoke without a lazy strategy = %v, want ErrRequestInvalid", err)
	}
	_, err := New(Config{
		Envelopes: newFakeEnvelopes(), Mailboxes: versionstore.NewMemoryStore[int64, Mailbox](),
		Sends: versionstore.NewMemoryStore[string, SentRecord](), Broadcast: NewLazyBroadcast(LazyBroadcastConfig{}),
	})
	if err == nil {
		t.Fatal("a lazy broadcast without an index was accepted")
	}
	bad := BroadcastProfile{RegisteredAtUnix: 1, Scopes: make([]string, MaxProfileScopes+1)}
	for i := range bad.Scopes {
		bad.Scopes[i] = fmt.Sprintf("s%d", i)
	}
	if err := bad.Validate(); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("an over-long scope list = %v, want ErrRequestInvalid", err)
	}
}

// 读路径的检查要便宜：版本提示没变时 Snapshot 不再读账本（进程内缓存命中），变了才读一次。
func TestBroadcastSnapshotReadsTheLedgerOnlyWhenItChanged(t *testing.T) {
	store := &countingLedgerStore{Store: versionstore.NewMemoryStore[string, BroadcastLedger]()}
	hint := &memoryHint{}
	index := &ledgerIndex{store: store, hint: hint}
	ctx := context.Background()
	envelope := Envelope{ID: "b1", Audience: AudienceBroadcast, Subject: "x", CreatedAtUnix: 100, ExpiresAtUnix: 1000}
	if _, err := index.Publish(ctx, envelope, 100, 10); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if ledger, err := index.Snapshot(ctx); err != nil || ledger.Head != 1 {
			t.Fatalf("snapshot = %+v, %v", ledger, err)
		}
	}
	if store.gets != 1 {
		t.Fatalf("five snapshots of an unchanged ledger read it %d times, want 1", store.gets)
	}
	if _, _, err := index.Revoke(ctx, "b1", 200); err != nil {
		t.Fatal(err)
	}
	if ledger, err := index.Snapshot(ctx); err != nil || ledger.Mails[0].RevokedAtUnix != 200 {
		t.Fatalf("snapshot after revoke = %+v, %v; want the revocation visible", ledger, err)
	}
	if store.gets != 2 {
		t.Fatalf("the ledger was read %d times after one change, want 2", store.gets)
	}
}

type countingLedgerStore struct {
	versionstore.Store[string, BroadcastLedger]
	mu   sync.Mutex
	gets int
}

func (s *countingLedgerStore) Get(ctx context.Context, key string) (versionstore.Versioned[BroadcastLedger], bool, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Store.Get(ctx, key)
}

type memoryHint struct {
	mu      sync.Mutex
	version uint64
	set     bool
}

func (h *memoryHint) Load(context.Context) (uint64, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.version, h.set, nil
}

func (h *memoryHint) Raise(_ context.Context, version uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.set || version > h.version {
		h.version, h.set = version, true
	}
	return nil
}
