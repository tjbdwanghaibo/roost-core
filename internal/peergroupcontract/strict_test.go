package peergroupcontract_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// Snapshot exactly what reaches the strict authority. A mutation per entity in
// separate Commit calls is NOT equivalent to all the documents in one record.
type batch struct {
	durability dataengine.Durability
	writes     map[int64][]string
}

type authority struct {
	decision error
	records  []batch
}

func (a *authority) Commit(_ context.Context, r nest.CommitRecord) error {
	writes := make(map[int64][]string)
	for _, m := range r.Mutations {
		if m.Kind == dataengine.MutationPut && m.Key.Database == database && m.Key.Resource == resource {
			writes[m.Key.ID] = append(writes[m.Key.ID], string(m.Data))
		}
	}
	a.records = append(a.records, batch{r.Durability, writes})
	return a.decision
}

func TestStrictPeerGroupMembershipFollowsAuthorityDecision(t *testing.T) {
	rejected := errors.New("authority rejected test record")
	for _, op := range []string{"leave", "join"} {
		for _, decision := range []struct {
			name  string
			cause error
		}{{"accepted", nil}, {"rejected", rejected}, {"unknown", nest.ErrCommitIndeterminate}} {
			t.Run(op+"/"+decision.name, func(t *testing.T) {
				w := newWorld(t)
				authority := &authority{decision: decision.cause}
				f := w.engine(nest.NestOptionWithTransactionCommitter(authority))
				leaving := op == "leave"
				change := f.on("strict_"+op, nest.DurabilityStrict, func(es []entity.IThreadSafeEntity, _ []any) (any, error) {
					if len(es) != 1 || es[0] != w.a {
						return nil, fmt.Errorf("strict explicit targets: %v", es)
					}
					self := es[0].(*unit)
					self.balance, self.persist = 17, true
					if !leaving {
						return nil, nest.JoinEntityGroup(red)
					}
					b, err := requireMember(w.b)
					if err != nil {
						return nil, err
					}
					c, err := requireMember(w.c)
					if err != nil {
						return nil, err
					}
					b.balance, b.persist = 23, true
					c.balance, c.persist = 29, true
					if err := nest.LeaveEntityGroup(); err != nil {
						return nil, err
					}
					_, err = requireMember(w.c)
					return nil, err
				})
				f.start(t)
				f.group(t, red, w.b, w.c)
				if leaving {
					f.group(t, red, w.a)
				}
				f.checkTrio(t, leaving)
				// Unknown fences the engine; another Request must not bypass it. Observe
				// only the public manager's published membership (never business fields).
				// Calibrate this observer against the positive/negative handler views first.
				checkPublished := func(aIn bool) {
					t.Helper()
					var wantA entity.IThreadSafeEntity
					if aIn {
						wantA = w.a
					}
					for id, want := range map[int64]entity.IThreadSafeEntity{w.a.ID(): wantA, w.b.ID(): w.b, w.c.ID(): w.c} {
						if got := w.manager.GetGroupEntity(red, id); got != want {
							t.Fatalf("published member %d = %v, want %v", id, got, want)
						}
					}
				}
				checkPublished(leaving)
				_, err := f.n.Request(f.ctx, change, w.a.ID(), nil)
				if !errors.Is(err, decision.cause) {
					t.Fatalf("strict request: got %v, want %v", err, decision.cause)
				}
				if decision.cause == nest.ErrCommitIndeterminate && f.n.FenceError() == nil {
					t.Fatal("unknown commit did not fence the writer")
				}
				want := map[int64][]string{w.a.ID(): {"17"}}
				if leaving {
					want[w.b.ID()] = []string{"23"}
					want[w.c.ID()] = []string{"29"}
				}
				if !reflect.DeepEqual(authority.records, []batch{{dataengine.DurabilityStrict, want}}) {
					t.Fatalf("strict %s: received records %v, want one atomic record %v", op, authority.records, want)
				}
				wantA := leaving
				if decision.cause == nil {
					wantA = !wantA
				}
				checkPublished(wantA)
				if decision.cause != nest.ErrCommitIndeterminate {
					f.checkTrio(t, wantA)
				}
				switch decision.cause {
				case rejected:
					f.checkBalance(t, w.a, 0)
					f.checkBalance(t, w.b, 0)
					f.checkBalance(t, w.c, 0)
				case nil:
					f.checkBalance(t, w.a, 17)
					if leaving {
						f.checkBalance(t, w.b, 23)
						f.checkBalance(t, w.c, 29)
					}
				}
				// Unknown does not promise to roll back speculative business values.
			})
		}
	}
}
