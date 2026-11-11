// 入参与角色守卫：角色消失或易主时拒绝写回、空标识与非法服务器配置拒绝。

package account

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/security"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// SelectRole 在 Get 与 Update 之间角色消失 / 换了主人时以 ErrRoleMissing / ErrNotPermitted 拒绝而不是写回登录时间；ValidateSession 对签名
// 有效但角色不存在的 token 报 ErrRoleMissing；UpdateProfile 对不存在的角色报 ErrRoleMissing；CreateRole 的空账号 id、UpsertServer 的零 id、
// Identity 的空 open id、Redis 存储的空键前缀各以对应哨兵拒绝。

// swappedRoles forwards to a real store but, once armed, hands every Update
// closure a substitute role state — the role vanished, or changed owner —
// between the caller's Get and its compare-and-set.
type swappedRoles struct {
	versionstore.Store[int64, Role]
	armed   bool
	present bool
	role    Role
}

func (s *swappedRoles) Update(ctx context.Context, key int64, mutate versionstore.Mutate[Role]) (versionstore.Versioned[Role], bool, error) {
	if s.armed {
		_, _, err := mutate(s.role, s.present)
		return versionstore.Versioned[Role]{}, false, err
	}
	return s.Store.Update(ctx, key, mutate)
}

func TestSelectRoleRefusesARoleThatVanishedOrChangedOwnerBeforeTheWrite(t *testing.T) {
	ctx := context.Background()
	roles := &swappedRoles{}
	service, _, _ := newService(t, func(cfg *Config) {
		roles.Store = cfg.Roles
		cfg.Roles = roles
	})
	account := login(t, service, "u1")
	role, err := service.CreateRole(ctx, account.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	roles.armed, roles.present = true, false
	if _, err := service.SelectRole(ctx, account.ID, role.PlayerID); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("SelectRole with the role vanishing before the write = %v, want ErrRoleMissing", err)
	}
	roles.present, roles.role = true, Role{PlayerID: role.PlayerID, AccountID: "someone-else"}
	if _, err := service.SelectRole(ctx, account.ID, role.PlayerID); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("SelectRole with the role changing owner before the write = %v, want ErrNotPermitted", err)
	}
	roles.armed = false
	if session, err := service.SelectRole(ctx, account.ID, role.PlayerID); err != nil || session.PlayerID != role.PlayerID {
		t.Fatalf("SelectRole with a stable role = (%+v, %v)", session, err)
	}
}

func TestSessionAndProfileOperationsRefuseAMissingRole(t *testing.T) {
	ctx := context.Background()
	service, c, cfg := newService(t)
	token, err := security.SignSessionToken(4242, cfg.SessionSecret, time.Hour, c.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ValidateSession(ctx, 4242, token); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("ValidateSession with a valid token for a missing role = %v, want ErrRoleMissing", err)
	}
	if _, err := service.UpdateProfile(ctx, "acct", 4242, []byte("{}")); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("UpdateProfile for a missing role = %v, want ErrRoleMissing", err)
	}
	if _, found, _ := cfg.Roles.Get(ctx, 4242); found {
		t.Fatal("a refused profile update created a role")
	}
}

func TestEntryPointsRefuseBlankIdentifiers(t *testing.T) {
	ctx := context.Background()
	service, _, _ := newService(t)
	if _, err := service.CreateRole(ctx, "  ", 1, "Alice"); !errors.Is(err, ErrAccountMissing) || !strings.Contains(err.Error(), "account id is empty") {
		t.Fatalf("CreateRole with a blank account id = %v", err)
	}
	if _, err := service.UpsertServer(ctx, GameServer{Status: ServerOpen}); !errors.Is(err, ErrServerInvalid) || !strings.Contains(err.Error(), "id is zero") {
		t.Fatalf("UpsertServer with id 0 = %v", err)
	}
	if err := (Identity{Channel: "store", OpenID: "  "}).Validate(); !errors.Is(err, ErrIdentityInvalid) || !strings.Contains(err.Error(), "open id is empty") {
		t.Fatalf("Identity without an open id = %v", err)
	}
	if err := (Identity{Channel: "store", OpenID: "u1"}).Validate(); err != nil {
		t.Fatalf("valid identity refused: %v", err)
	}
	if _, err := NewRedisStores(nil, "  ", time.Minute); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisStores with a blank prefix = %v", err)
	}
}

func TestUpsertServerRejectsNegativeIDAndUnknownStatus(t *testing.T) {
	s, _, _ := newService(t)
	for _, server := range []GameServer{{ID: -1, Status: ServerOpen}, {ID: 2, Status: "typo"}} {
		if _, err := s.UpsertServer(context.Background(), server); err == nil {
			t.Errorf("invalid server accepted: %+v", server)
		}
	}
}

// Accounts 声明的每个方法都在 Methods 里，反过来也是：接口上多了方法而传输没有重新生成时，本地实现与
// BusClient 都能编译，只有别的进程调用时才失败。
func TestMethodsCoverTheAccountsInterfaceExactly(t *testing.T) {
	iface := reflect.TypeOf((*Accounts)(nil)).Elem()
	declared := map[string]bool{}
	for index := 0; index < iface.NumMethod(); index++ {
		declared[ServiceType+"."+iface.Method(index).Name] = true
	}
	listed := map[string]bool{}
	for _, method := range Methods {
		if listed[method] {
			t.Fatalf("Methods lists %q twice", method)
		}
		listed[method] = true
		if !declared[method] {
			t.Fatalf("Methods lists %s but Accounts does not declare it", method)
		}
	}
	for method := range declared {
		if !listed[method] {
			t.Fatalf("Accounts declares %s but Methods does not list it; regenerate the transport (go generate ./service/account)", method)
		}
	}
	// 换服的四个接口在总线上（方案 §6 第 1 项）；人工提交带备注的那个只在运维面（Admin），不上总线。
	for _, method := range []string{MethodRoleBinding, MethodBeginRoleTransfer, MethodCommitRoleTransfer, MethodAbortRoleTransfer} {
		if !listed[method] {
			t.Fatalf("%s is not on the bus", method)
		}
	}
	if listed[ServiceType+".CommitRoleTransferWithNote"] || listed[ServiceType+".MergeServer"] || listed[ServiceType+".UpsertServer"] {
		t.Fatalf("an operator write is on the bus: %v", Methods)
	}
}

// loopbackBus 把 Call 交给登记的 handler，请求与响应都过一遍总线的编码：编码丢掉的字段与服务没填的
// 字段在调用方看来一样，所以替身必须真的序列化。
type loopbackBus struct {
	bus.IBus
	mu       sync.Mutex
	handlers map[string]bus.RpcHandlerFunc
}

func (b *loopbackBus) HandleRpc(method string, handler bus.RpcHandlerFunc) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[method] = handler
	return nil
}

func (b *loopbackBus) Call(ctx context.Context, _ string, method string, req any, resp any) error {
	b.mu.Lock()
	handler, ok := b.handlers[method]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("loopbackBus: no handler for %s", method)
	}
	codec := bus.MessagePackCodec{}
	payload, err := codec.Marshal(req)
	if err != nil {
		return err
	}
	answer, err := handler(bus.NewRPCContext(ctx, method, payload, codec))
	if err != nil {
		return err
	}
	encoded, err := codec.Marshal(answer)
	if err != nil {
		return err
	}
	return codec.Unmarshal(encoded, resp)
}

// 换服接口经总线调用与本地调用给出同样的结果：绑定里的指针字段（在途与最近了结的换服）过得了编码，
// 拒绝的错误码与随拒绝带回的绑定也都到得了调用方；ValidateSession / SelectRole 带着 Transfer。
func TestTheTransferInterfacesBehaveTheSameOverTheBus(t *testing.T) {
	for _, transport := range []string{"local", "bus"} {
		t.Run(transport, func(t *testing.T) {
			service, _, role, session := transferFixture(t)
			var accounts Accounts = service
			if transport == "bus" {
				loopback := &loopbackBus{handlers: map[string]bus.RpcHandlerFunc{}}
				if err := RegisterHandlers(loopback, service); err != nil {
					t.Fatal(err)
				}
				client, err := NewBusClient(loopback, "", 0)
				if err != nil {
					t.Fatal(err)
				}
				accounts = client
			}
			ctx := context.Background()

			binding, err := accounts.RoleBinding(ctx, role.PlayerID)
			if err != nil || binding.PlayerID != role.PlayerID || binding.ServerID != homeSID || binding.ServingServerID != homeSID || binding.Transfer != nil {
				t.Fatalf("RoleBinding = %+v, %v", binding, err)
			}
			begun, err := accounts.BeginRoleTransfer(ctx, role.PlayerID, TransferRequest{ID: "t-1", FromSID: homeSID, To: awaySID, TTLSeconds: 60, Note: "over the bus"})
			if err != nil || begun.Transfer == nil || begun.Transfer.ID != "t-1" || begun.Transfer.To != awaySID || begun.Transfer.Note != "over the bus" ||
				begun.Transfer.DeadlineUnix != begun.Transfer.StartedAtUnix+60 {
				t.Fatalf("BeginRoleTransfer = %+v (transfer %+v), %v", begun, begun.Transfer, err)
			}
			validated, err := accounts.ValidateSession(ctx, role.PlayerID, session.Token)
			if err != nil || validated.Transfer == nil || validated.Transfer.ID != "t-1" || validated.ServerID != homeSID {
				t.Fatalf("ValidateSession during the transfer = %+v, %v", validated, err)
			}
			selected, err := accounts.SelectRole(ctx, role.AccountID, role.PlayerID)
			if err != nil || selected.Transfer == nil || selected.Transfer.To != awaySID {
				t.Fatalf("SelectRole during the transfer = %+v, %v", selected, err)
			}
			refused, err := accounts.BeginRoleTransfer(ctx, role.PlayerID, TransferRequest{ID: "t-2", FromSID: homeSID, To: thirdSID})
			if !errors.Is(err, ErrTransferInProgress) || Code(err) != CodeTransferInProgress || refused.Transfer == nil || refused.Transfer.ID != "t-1" {
				t.Fatalf("a second Begin = %+v, %v; want ErrTransferInProgress carrying t-1", refused, err)
			}
			committed, err := accounts.CommitRoleTransfer(ctx, role.PlayerID, "t-1")
			if err != nil || committed.ServerID != awaySID || committed.BindEpoch != 1 || committed.Transfer != nil ||
				committed.LastTransfer == nil || !committed.LastTransfer.Committed || !committed.LastTransfer.Settled {
				t.Fatalf("CommitRoleTransfer = %+v (last %+v), %v", committed, committed.LastTransfer, err)
			}
			late, err := accounts.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "too late")
			if !errors.Is(err, ErrTransferStale) || Code(err) != CodeTransferStale || late.ServerID != awaySID || late.BindEpoch != 1 {
				t.Fatalf("Abort after the commit = %+v, %v; want ErrTransferStale carrying the committed binding", late, err)
			}
			validated, err = accounts.ValidateSession(ctx, role.PlayerID, session.Token)
			if err != nil || validated.ServerID != awaySID || validated.ServingServerID != awaySID || validated.BindEpoch != 1 || validated.Transfer != nil ||
				validated.LastTransfer == nil || validated.LastTransfer.ID != "t-1" {
				t.Fatalf("ValidateSession after the commit = %+v, %v", validated, err)
			}
		})
	}
}
