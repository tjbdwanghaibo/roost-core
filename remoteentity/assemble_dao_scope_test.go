package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// scopedTestDao 是手写 DAO：只实现 DaoInterface 与可选的 DbScope。
type scopedTestDao struct {
	testRemoteDao
	scope entity.DatabaseScope
}

func (d *scopedTestDao) DbScope() entity.DatabaseScope { return d.scope }

func registerScopedDaoKind(kind entity.EntityKind, policy entity.RemotePolicy, scope entity.DatabaseScope) {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: policy})
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category: 1,
		Kind:     kind,
		Builder: func(*entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return nil, errors.New("scopedTestDao kinds are never built")
		},
		DaoBuilders: []entity.DaoBuilderFunc{
			func() entity.DaoInterface { return &testRemoteDao{dirty: &testDirty{}} },
			func() entity.DaoInterface {
				return &scopedTestDao{testRemoteDao: testRemoteDao{dirty: &testDirty{}}, scope: scope}
			},
		},
		RemotePolicy: policy,
	})
}

func TestValidateRemoteManagedDaoScopes(t *testing.T) {
	sidDao := func() entity.DaoInterface {
		return &scopedTestDao{testRemoteDao: testRemoteDao{dirty: &testDirty{}}, scope: entity.DatabaseServer}
	}
	globalDao := func() entity.DaoInterface {
		return &scopedTestDao{testRemoteDao: testRemoteDao{dirty: &testDirty{}}, scope: entity.DatabaseGlobal}
	}
	plainDao := func() entity.DaoInterface { return &testRemoteDao{dirty: &testDirty{}} }
	cases := []struct {
		name    string
		builder *entity.EntityBuilderParam
		reject  bool
	}{
		{"managed/global", &entity.EntityBuilderParam{Kind: 91, RemotePolicy: entity.RemotePolicyManaged, DaoBuilders: []entity.DaoBuilderFunc{globalDao, plainDao}}, false},
		{"managed/sid", &entity.EntityBuilderParam{Kind: 92, RemotePolicy: entity.RemotePolicyManaged, DaoBuilders: []entity.DaoBuilderFunc{plainDao, sidDao}}, true},
		{"local/sid", &entity.EntityBuilderParam{Kind: 93, RemotePolicy: entity.RemotePolicyNone, DaoBuilders: []entity.DaoBuilderFunc{sidDao}}, false},
		{"mirror/sid", &entity.EntityBuilderParam{Kind: 94, RemotePolicy: entity.RemotePolicyMirror, DaoBuilders: []entity.DaoBuilderFunc{sidDao}}, false},
		{"managed/nil-dao", &entity.EntityBuilderParam{Kind: 95, RemotePolicy: entity.RemotePolicyManaged, DaoBuilders: []entity.DaoBuilderFunc{nil, func() entity.DaoInterface { return nil }}}, false},
		// RR-20260926-60：kind 定义声明 managed、手写 builder 省略 RemotePolicy（注册表按“部分重复声明”接受）；
		// 校验按 kind 的实际策略判断。
		{"kind-def-managed/builder-omits-policy/sid", &entity.EntityBuilderParam{Kind: 96, DaoBuilders: []entity.DaoBuilderFunc{sidDao}}, true},
	}
	// 校验读取注册表里 kind 的实际策略；这里只登记 kind 定义（不注册 builder），不影响本包其他 Assemble 用例。
	kindPolicy := map[entity.EntityKind]entity.RemotePolicy{91: entity.RemotePolicyManaged, 92: entity.RemotePolicyManaged, 93: entity.RemotePolicyNone, 94: entity.RemotePolicyMirror, 95: entity.RemotePolicyManaged, 96: entity.RemotePolicyManaged}
	for kind, policy := range kindPolicy {
		entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: policy})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := entity.ValidateRemoteManagedDaoScopes([]*entity.EntityBuilderParam{tc.builder, nil})
			if !tc.reject {
				if err != nil {
					t.Fatalf("legal combination rejected: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrRemoteManagedServerScopedDAO) {
				t.Fatalf("remote=managed with dbscope=sid DAO must be rejected with ErrRemoteManagedServerScopedDAO, got %v", err)
			}
			if !strings.Contains(err.Error(), "dbscope=global") || !strings.Contains(err.Error(), fmt.Sprintf("kind=%d", tc.builder.Kind)) {
				t.Fatalf("error must name the kind and the global alternative: %v", err)
			}
		})
	}
}

const rr45ChildEnv = "ROOST_RR45_ASSEMBLY_CHILD"

// Assemble / Start 读全局实体注册表；非法注册无法撤销，所以在子进程里注册，避免污染本包其他测试。
func TestAssembleRejectsRemoteManagedServerScopedDAO(t *testing.T) {
	switch os.Getenv(rr45ChildEnv) {
	case "assemble":
		// 合法组合不误伤：托管实体 + global DAO、本地实体 + sid DAO。
		registerScopedDaoKind(215, entity.RemotePolicyManaged, entity.DatabaseGlobal)
		registerScopedDaoKind(216, entity.RemotePolicyNone, entity.DatabaseServer)
		if _, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: &atomicTestBackend{newRemoteTestLoader()}}, DefaultConfig(), 7, MongoBackendConfig{}); err != nil {
			t.Fatalf("legal registrations rejected: %v", err)
		}
		registerScopedDaoKind(214, entity.RemotePolicyManaged, entity.DatabaseServer)
		asm, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: &atomicTestBackend{newRemoteTestLoader()}}, DefaultConfig(), 7, MongoBackendConfig{})
		if err == nil || asm != nil {
			t.Fatalf("Assemble accepted a remote-managed kind with a dbscope=sid DAO (asm=%v)", asm != nil)
		}
		if !errors.Is(err, ErrRemoteManagedServerScopedDAO) {
			t.Fatalf("want ErrRemoteManagedServerScopedDAO, got %v", err)
		}
		return
	case "kind_def":
		// RR-20260926-60（REPRO-2026-09-26-05 §9）：kind 经 MustRegisterEntityKindDefs 声明 managed，手写 builder 省略
		// RemotePolicy——注册表接受“部分重复声明”，所有 Remote 路径都按 managed 处理，装配校验也必须拦下 sid DAO。
		const kind entity.EntityKind = 217
		entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1,
			Kind:     kind,
			Builder: func(*entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				return nil, errors.New("never built")
			},
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface {
				return &scopedTestDao{testRemoteDao: testRemoteDao{dirty: &testDirty{}}, scope: entity.DatabaseServer}
			}},
		})
		if !entity.IsEntityKindRemoteManaged(kind) {
			t.Fatal("setup: kind is not managed")
		}
		asm, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: &atomicTestBackend{newRemoteTestLoader()}}, DefaultConfig(), 7, MongoBackendConfig{})
		if !errors.Is(err, ErrRemoteManagedServerScopedDAO) {
			t.Fatalf("kind %d is remote-managed (IsEntityKindRemoteManaged=true) with a dbscope=sid DAO, but Assemble accepted it (asm=%v err=%v)", kind, asm != nil, err)
		}
		return
	case "start":
		// 装配之后才注册的手写实体，由 Start 拦下。
		a, bus := newLifecycleAssembly(t, func(context.Context) error { return nil })
		registerScopedDaoKind(214, entity.RemotePolicyManaged, entity.DatabaseServer)
		err := safeAssemblyStart(a, context.Background(), bus)
		if err == nil {
			t.Fatal("Start accepted a remote-managed kind with a dbscope=sid DAO")
		}
		if !errors.Is(err, ErrRemoteManagedServerScopedDAO) {
			t.Fatalf("want ErrRemoteManagedServerScopedDAO, got %v", err)
		}
		if bus.active.Load() != 0 {
			t.Fatalf("rejected Start left %d subscriptions", bus.active.Load())
		}
		return
	}
	for _, mode := range []string{"assemble", "start", "kind_def"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestAssembleRejectsRemoteManagedServerScopedDAO$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), rr45ChildEnv+"="+mode)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child %s failed: %v\n%s", mode, err, out)
			}
		})
	}
}
