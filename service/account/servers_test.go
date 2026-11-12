package account

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 区服清单（ListServers）与它的索引（servers.go）。存储缺省是 Memory；ROOST_REVIEW3_BACKEND=redis 时与其余
// 契约测试一样走隔离的 Redis（pendingContractService）——索引记录与区服记录在同一个存储里，两种存储走同一段代码。

// freshAccount 是一个还没有登记任何区服的 account（newService 自带的 1 服不算在内的用例用它）。
func freshAccount(t *testing.T, mutate ...func(*Config)) (*Service, Config) {
	t.Helper()
	s, _, cfg := pendingContractService(t, mutate...)
	return s, cfg
}

func listedIDs(t *testing.T, accounts Accounts) []int32 {
	t.Helper()
	servers, err := accounts.ListServers(context.Background())
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	ids := make([]int32, 0, len(servers))
	for _, server := range servers {
		if len(server.Listed) != 0 {
			t.Fatalf("server %d carries the index field: %+v", server.ID, server)
		}
		ids = append(ids, server.ID)
	}
	return ids
}

func serverIndex(t *testing.T, cfg Config) []int32 {
	t.Helper()
	index, _, err := cfg.Servers.Get(context.Background(), serverIndexKey)
	if err != nil {
		t.Fatal(err)
	}
	return index.Value.Listed
}

// 登记过的服按服号升序列出，带着名字、状态与区域；改名、改状态不改变清单的成员；本地与经总线的回答相同。
func TestListServersReturnsEveryRegisteredServerInOrder(t *testing.T) {
	s, cfg := freshAccount(t)
	ctx := context.Background()
	loopback := &loopbackBus{handlers: map[string]bus.RpcHandlerFunc{}}
	if err := RegisterHandlers(loopback, s); err != nil {
		t.Fatal(err)
	}
	remote, err := NewBusClient(loopback, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	// newService 登记了 1 服。再按乱序登记三个。
	for _, server := range []GameServer{
		{ID: 30, Name: "s30", Status: ServerMaintenance, Region: "east"},
		{ID: 7, Name: "s7", Status: ServerFull},
		{ID: 1000, Name: "s1000"},
	} {
		if _, err := s.UpsertServer(ctx, server); err != nil {
			t.Fatal(err)
		}
	}
	for transport, accounts := range map[string]Accounts{"local": s, "bus": remote} {
		servers, err := accounts.ListServers(ctx)
		if err != nil {
			t.Fatalf("%s: %v", transport, err)
		}
		got := ""
		for _, server := range servers {
			got += fmt.Sprintf("%d:%s:%s:%s ", server.ID, server.Name, server.Status, server.Region)
		}
		if want := "1:s1:open: 7:s7:full: 30:s30:maintenance:east 1000:s1000:open: "; got != want {
			t.Fatalf("%s: ListServers = %q, want %q", transport, got, want)
		}
	}
	// 改名、改状态：成员不变，内容是新的；索引没有被重写。
	before, _, err := cfg.Servers.Get(ctx, serverIndexKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertServer(ctx, GameServer{ID: 7, Name: "seven", Status: ServerClosed}); err != nil {
		t.Fatal(err)
	}
	after, _, _ := cfg.Servers.Get(ctx, serverIndexKey)
	if after.Version != before.Version {
		t.Fatalf("updating a listed server rewrote the index: version %d -> %d", before.Version, after.Version)
	}
	servers, _ := s.ListServers(ctx)
	if len(servers) != 4 || servers[1].ID != 7 || servers[1].Name != "seven" || servers[1].Status != ServerClosed {
		t.Fatalf("after renaming server 7: %+v", servers)
	}
	// 调用方拿到的切片不与存储共享，改它不影响下一次。
	servers[0].Name = "tampered"
	if again, _ := s.ListServers(ctx); again[0].Name != "s1" {
		t.Fatalf("a caller's edit leaked into the store: %+v", again[0])
	}
	// 0 号是索引记录，不是一个服：不被列出，也问不到、写不了。
	if ids := listedIDs(t, s); slices.Contains(ids, 0) {
		t.Fatalf("the index record is listed as a server: %v", ids)
	}
	if _, err := s.GetServer(ctx, 0); !errors.Is(err, ErrServerInvalid) {
		t.Fatalf("GetServer(0) = %v, want ErrServerInvalid", err)
	}
	if _, err := s.UpsertServer(ctx, GameServer{ID: 0, Name: "zero"}); !errors.Is(err, ErrServerInvalid) {
		t.Fatalf("UpsertServer(0) = %v, want ErrServerInvalid", err)
	}
	// 调用方在记录上带进来的 Listed 不落库：索引只在保留键上。
	if _, err := s.UpsertServer(ctx, GameServer{ID: 7, Name: "seven", Listed: []int32{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	if stored := storedServer(t, cfg, 7); len(stored.Listed) != 0 {
		t.Fatalf("a caller-supplied index landed on server 7's record: %+v", stored)
	}
}

// 一个区服都没登记过：空清单，不是错误。
func TestListServersOfAnEmptyDirectoryIsEmpty(t *testing.T) {
	cfg := Config{Servers: versionstore.NewMemoryStore[int32, GameServer]()}
	s := &Service{cfg: cfg}
	servers, err := s.ListServers(context.Background())
	if err != nil || servers == nil || len(servers) != 0 {
		t.Fatalf("ListServers on an empty store = %v, %v; want an empty, non-nil list", servers, err)
	}
}

// 合服的字段随清单给出：并入的服带 MergedInto、状态 closed，承载服带 MergedFrom；解除之后清掉。
func TestListServersCarriesTheMergeFields(t *testing.T) {
	s, _ := freshAccount(t)
	ctx := context.Background()
	upsertOpen(t, s, 2, 3)
	mustMerge(t, s, 2, 1)
	servers, err := s.ListServers(ctx)
	if err != nil || len(servers) != 3 {
		t.Fatalf("ListServers after a merge = %+v, %v", servers, err)
	}
	if servers[0].ID != 1 || !slices.Equal(servers[0].MergedFrom, []int32{2}) || servers[0].MergedInto != 0 {
		t.Fatalf("the serving server = %+v, want merged_from [2]", servers[0])
	}
	if servers[1].ID != 2 || servers[1].MergedInto != 1 || servers[1].Status != ServerClosed || servers[1].MergeNote != mergeNote {
		t.Fatalf("the merged server = %+v, want merged into 1 and closed", servers[1])
	}
	if servers[2].ID != 3 || servers[2].MergedInto != 0 || len(servers[2].MergedFrom) != 0 {
		t.Fatalf("the bystander = %+v", servers[2])
	}
	if _, err := s.UnmergeServer(ctx, 2, "revert"); err != nil {
		t.Fatal(err)
	}
	servers, _ = s.ListServers(ctx)
	if len(servers) != 3 || servers[1].MergedInto != 0 || len(servers[0].MergedFrom) != 0 {
		t.Fatalf("after the unmerge: %+v", servers)
	}
}

// 许多服号同时第一次登记：每一个都进了索引、都有记录，清单是完整而有序的。
func TestConcurrentFirstRegistrationsAllLandInTheList(t *testing.T) {
	s, cfg := freshAccount(t)
	ctx := context.Background()
	const first, count = 100, 48
	var wg sync.WaitGroup
	errs := make(chan error, count*2)
	for id := int32(first); id < first+count; id++ {
		for range 2 { // 同一个服号也并发登记两次
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.UpsertServer(ctx, GameServer{ID: id, Name: fmt.Sprintf("s%d", id)}); err != nil {
					errs <- fmt.Errorf("server %d: %w", id, err)
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	ids := listedIDs(t, s)
	if len(ids) != count+1 || ids[0] != 1 || ids[1] != first || ids[count] != first+count-1 || !slices.IsSorted(ids) {
		t.Fatalf("after %d concurrent first registrations the list has %d servers: %v", count, len(ids), ids)
	}
	if index := serverIndex(t, cfg); len(index) != count+1 || !slices.IsSorted(index) || len(slices.Compact(slices.Clone(index))) != len(index) {
		t.Fatalf("the index is not a sorted set of %d ids: %v", count+1, index)
	}
}

// 索引与记录不一致时各答什么：
//   - 索引里有、记录没有（停在两次写之间）：跳过；再登记一次补上。
//   - 记录有、索引里没有（这一版之前登记的）：列不出；再登记一次、或参与一次合服，补上。
//   - 索引记录被改坏（重复、乱序、非正数）：按整理后的读。
//   - 存储出错：返回错误，不返回半份清单。
func TestListServersWhenTheIndexAndTheRecordsDisagree(t *testing.T) {
	ctx := context.Background()
	s, cfg := freshAccount(t)
	upsertOpen(t, s, 5)

	// 先索引、后记录：写记录失败时，索引已经有它，清单跳过它。
	failing := &serversFailingRecordWrites{Store: cfg.Servers, fail: true}
	halfway := &Service{cfg: cfg, report: s.report}
	halfway.cfg.Servers = failing
	if _, err := halfway.UpsertServer(ctx, GameServer{ID: 9, Name: "s9"}); !errors.Is(err, errInjected) {
		t.Fatalf("fixture: the record write was supposed to fail: %v", err)
	}
	if index := serverIndex(t, cfg); !slices.Equal(index, []int32{1, 5, 9}) {
		t.Fatalf("the index after an upsert interrupted between its two writes = %v, want [1 5 9] (index first)", index)
	}
	if ids := listedIDs(t, s); !slices.Equal(ids, []int32{1, 5}) {
		t.Fatalf("an indexed id without a record is listed: %v", ids)
	}
	if _, err := s.GetServer(ctx, 9); !errors.Is(err, ErrServerInvalid) {
		t.Fatalf("the interrupted upsert left a record: %v", err)
	}
	if _, err := s.UpsertServer(ctx, GameServer{ID: 9, Name: "s9"}); err != nil {
		t.Fatal(err)
	}
	if ids := listedIDs(t, s); !slices.Equal(ids, []int32{1, 5, 9}) {
		t.Fatalf("after registering it again: %v", ids)
	}

	// 记录在、索引里没有：直接往存储里放两条旧记录。
	for _, id := range []int32{20, 21} {
		// 记录上还带着一份 Listed（存储被手工改过的样子）：任何接口都不把它交出去。
		if _, created, err := cfg.Servers.Create(ctx, id, GameServer{ID: id, Name: fmt.Sprintf("old%d", id), Status: ServerOpen, Listed: []int32{1, 2}}); err != nil || !created {
			t.Fatalf("plant an unindexed record: created=%v err=%v", created, err)
		}
	}
	if ids := listedIDs(t, s); !slices.Equal(ids, []int32{1, 5, 9}) {
		t.Fatalf("records that were never indexed are listed: %v", ids)
	}
	if server, err := s.GetServer(ctx, 20); err != nil || server.Name != "old20" || len(server.Listed) != 0 {
		t.Fatalf("an unindexed record is still a server to GetServer: %+v, %v", server, err)
	}
	if _, err := s.UpsertServer(ctx, GameServer{ID: 20, Name: "old20"}); err != nil {
		t.Fatal(err)
	}
	mustMerge(t, s, 21, 20) // 合服把两边都补登记
	if ids := listedIDs(t, s); !slices.Equal(ids, []int32{1, 5, 9, 20, 21}) {
		t.Fatalf("after re-registering 20 and merging 21 into it: %v", ids)
	}

	// 改坏的索引记录。
	if _, _, err := cfg.Servers.Update(ctx, serverIndexKey, func(GameServer, bool) (GameServer, bool, error) {
		return GameServer{Listed: []int32{9, 5, 5, -3, 0, 777, 1, 9}}, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if ids := listedIDs(t, s); !slices.Equal(ids, []int32{1, 5, 9}) {
		t.Fatalf("a scrambled index (duplicates, out of order, non-positive, 777 without a record) lists %v, want [1 5 9]", ids)
	}
	// 下一次登记把索引重新写成整理过的样子。
	if _, err := s.UpsertServer(ctx, GameServer{ID: 20, Name: "old20"}); err != nil {
		t.Fatal(err)
	}
	if index := serverIndex(t, cfg); !slices.Equal(index, []int32{1, 5, 9, 20, 777}) {
		t.Fatalf("the index after the next registration = %v, want it normalised to [1 5 9 20 777]", index)
	}

	// 存储出错：读记录失败时没有半份清单。
	broken := &Service{cfg: cfg, report: s.report}
	broken.cfg.Servers = &serversFailingGets{Store: cfg.Servers, failOn: 9}
	if servers, err := broken.ListServers(ctx); !errors.Is(err, errInjected) || servers != nil {
		t.Fatalf("ListServers over a failing store = %v, %v; want the error and no partial list", servers, err)
	}
	broken.cfg.Servers = &serversFailingGets{Store: cfg.Servers, failOn: serverIndexKey}
	if servers, err := broken.ListServers(ctx); !errors.Is(err, errInjected) || servers != nil {
		t.Fatalf("ListServers when the index cannot be read = %v, %v", servers, err)
	}
}

// 索引有容量：第 MaxServers 个之后的新服号被拒绝、什么也不写；已登记的照常更新；清单从不截断。
func TestTheServerIndexRefusesNewServersWhenFull(t *testing.T) {
	s, cfg := freshAccount(t)
	ctx := context.Background()
	full := make([]int32, 0, 256)
	for id := int32(1); id <= 256; id++ {
		full = append(full, id)
	}
	// 直接把索引写满（逐个登记 256 个服只是慢，不多证明什么）；记录只放头尾几条。
	if _, _, err := cfg.Servers.Update(ctx, serverIndexKey, func(GameServer, bool) (GameServer, bool, error) {
		return GameServer{Listed: full}, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertServer(ctx, GameServer{ID: 256, Name: "last"}); err != nil {
		t.Fatalf("updating a server that is already indexed while the index is full: %v", err)
	}
	_, err := s.UpsertServer(ctx, GameServer{ID: 257, Name: "one too many"})
	if !errors.Is(err, ErrRangeInvalid) || Code(err) != CodeRangeInvalid {
		t.Fatalf("the 257th server: %v, want ErrRangeInvalid", err)
	}
	if _, found, _ := cfg.Servers.Get(ctx, 257); found {
		t.Fatal("a refused registration wrote the server record")
	}
	if index := serverIndex(t, cfg); len(index) != 256 || slices.Contains(index, 257) {
		t.Fatalf("a refused registration changed the index: %d ids", len(index))
	}
	if ids := listedIDs(t, s); !slices.Equal(ids, []int32{1, 256}) {
		t.Fatalf("ListServers with a full index = %v, want the two servers that have records", ids)
	}
	// 合服也不能把新服号挤进满了的索引：两边都必须已经登记过，这里 300 没有记录，先被“服不存在”拒绝。
	if _, err := s.MergeServer(ctx, MergeRequest{From: 300, Into: 1, Note: mergeNote}); !errors.Is(err, ErrServerInvalid) {
		t.Fatalf("merging an unknown server: %v", err)
	}
}

// serversFailingRecordWrites 让区服记录（正的服号）的写失败，索引记录的写照常。
type serversFailingRecordWrites struct {
	versionstore.Store[int32, GameServer]
	fail bool
}

func (store *serversFailingRecordWrites) Update(ctx context.Context, key int32, mutate versionstore.Mutate[GameServer]) (versionstore.Versioned[GameServer], bool, error) {
	if store.fail && key != serverIndexKey {
		return versionstore.Versioned[GameServer]{}, false, errInjected
	}
	return store.Store.Update(ctx, key, mutate)
}

// serversFailingGets 让读 failOn 这个键失败。
type serversFailingGets struct {
	versionstore.Store[int32, GameServer]
	failOn int32
}

func (store *serversFailingGets) Get(ctx context.Context, key int32) (versionstore.Versioned[GameServer], bool, error) {
	if key == store.failOn {
		return versionstore.Versioned[GameServer]{}, false, errInjected
	}
	return store.Store.Get(ctx, key)
}
