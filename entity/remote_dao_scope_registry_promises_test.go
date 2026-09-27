package entity

import (
	"errors"
	"strings"
	"testing"
)

// RR-20260927-09（OPEN-ITEMS C15）：entity.ValidateEntityRegistry 是生成的 registry 聚合在全部注册之后调用的整表校验，
// 此前不看 DAO 作用域，“remote=managed + dbscope=sid”要等到 Remote 装配（remoteentity.Assemble / Start）才被拦下，
// 不装 Remote 的进程（例如只跑 DataEngine 的服务）一直带着这个组合启动。承诺：ValidateEntityRegistry 与装配期同一规则、
// 同一哨兵报告该组合（可 errors.Is ErrRemoteManagedServerScopedDAO），每个违规 DAO 只报一次；合法组合不受影响。

type scopedRegistryDao struct {
	factoryTestDao
	scope DatabaseScope
}

func (d *scopedRegistryDao) DbScope() DatabaseScope { return d.scope }

func registerScopedRegistryKind(kind EntityKind, policy RemotePolicy, scope DatabaseScope) {
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: EntityCategoryRemote, RemotePolicy: policy})
	RegisterEntityBuilder(&EntityBuilderParam{
		Category: EntityCategoryRemote,
		Kind:     kind,
		Builder: func(*EntityCreateParam) (IThreadSafeEntity, error) {
			return nil, errors.New("never built")
		},
		DaoBuilders: []DaoBuilderFunc{
			func() DaoInterface { return &factoryTestDao{coll: "plain"} },
			func() DaoInterface {
				return &scopedRegistryDao{factoryTestDao: factoryTestDao{coll: "scoped"}, scope: scope}
			},
		},
	})
}

func TestValidateEntityRegistryRejectsRemoteManagedServerScopedDao(t *testing.T) {
	isolateEntityRegistry(t)
	registerScopedRegistryKind(141, RemotePolicyManaged, DatabaseServer)
	registerScopedRegistryKind(142, RemotePolicyManaged, DatabaseGlobal)
	registerScopedRegistryKind(144, RemotePolicyNone, DatabaseServer)

	err := ValidateEntityRegistry()
	if err == nil || !strings.Contains(err.Error(), "dbscope=sid") {
		t.Fatalf("ValidateEntityRegistry accepted remote-managed kind 141 with a dbscope=sid DAO: err=%v", err)
	}
	if !errors.Is(err, ErrRemoteManagedServerScopedDAO) {
		t.Fatalf("err=%v, want errors.Is ErrRemoteManagedServerScopedDAO", err)
	}
	if n := strings.Count(err.Error(), "kind=141 "); n != 1 {
		t.Fatalf("kind 141 reported %d times, want once: %v", n, err)
	}
	for _, legal := range []string{"kind=142 ", "kind=144 "} {
		if strings.Contains(err.Error(), legal) {
			t.Fatalf("legal combination %q reported: %v", legal, err)
		}
	}
}
