package entity

import (
	"errors"
	"strings"
	"testing"
)

const promiseKind EntityKind = 177

var promiseEntityID, promiseOtherEntityID int64

func init() {
	MustRegisterEntityKindDefs(EntityKindDef{Kind: promiseKind, Category: 1, RemotePolicy: RemotePolicyManaged})
	var err error
	if promiseEntityID, err = BuildEntityID(991, promiseKind); err != nil {
		panic(err)
	}
	if promiseOtherEntityID, err = BuildEntityID(992, promiseKind); err != nil {
		panic(err)
	}
}

func validRemoteCommit() RemoteCommit {
	data := []byte(`{"hp":1}`)
	id := promiseEntityID
	return RemoteCommit{
		TransactionID: RemoteTransactionID{1}, EntityID: id, Kind: promiseKind, BaseVersion: 3, NextVersion: 4, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1,
		Mutations: []RemoteDataMutation{{Database: "game", Collection: "hero", ID: id, Version: 4, Data: data}},
		Snapshots: []RemoteSnapshotRecord{{Key: RemoteSnapshotKey{EntityID: id, Kind: promiseKind, Scope: 1}, BaseVersion: 3, StateVersion: 4, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Data: data, Checksum: RemoteSnapshotChecksum(data)}},
	}
}

// RemoteCommit.Validate is the admission gate for every remote-entity write:
// identity, version arithmetic, ownership epochs, the mutation / delete
// shape, per-record agreement with the commit header, duplicates, and
// snapshot integrity. Each rule is pinned by sentinel and message with a
// single-field mutation of a valid commit.
func TestRemoteCommitValidateRefusesEachBrokenField(t *testing.T) {
	if err := validRemoteCommit().Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name     string
		mutate   func(*RemoteCommit)
		sentinel error
		text     string
	}{
		{"zero transaction id", func(c *RemoteCommit) { c.TransactionID = RemoteTransactionID{} }, ErrRemoteRejected, "invalid identity"},
		{"zero entity id", func(c *RemoteCommit) { c.EntityID = 0 }, ErrRemoteRejected, "invalid identity"},
		{"no kind", func(c *RemoteCommit) { c.Kind = EntityKindNone }, ErrRemoteRejected, "invalid identity"},
		{"version not consecutive", func(c *RemoteCommit) { c.NextVersion = 5 }, ErrRemoteVersionConflict, "base=3 next=5"},
		{"no marker epoch", func(c *RemoteCommit) { c.MarkerEpoch = 0 }, ErrRemoteFenced, "missing ownership epoch"},
		{"no route epoch", func(c *RemoteCommit) { c.RouteEpoch = 0 }, ErrRemoteFenced, "missing ownership epoch"},
		{"update without mutations", func(c *RemoteCommit) { c.Mutations = nil; c.Snapshots = nil }, ErrRemoteRejected, "empty mutation set"},
		{"delete carrying mutations", func(c *RemoteCommit) {
			c.Delete = true
			c.Deletes = []RemoteDataDelete{{Database: "game", Collection: "hero", ID: promiseEntityID}}
		}, ErrRemoteRejected, "delete commit contains data mutations"},
		{"delete without deletes", func(c *RemoteCommit) { c.Delete = true; c.Mutations = nil; c.Snapshots = nil }, ErrRemoteRejected, "delete commit contains data mutations"},
		{"update carrying deletes", func(c *RemoteCommit) {
			c.Deletes = []RemoteDataDelete{{Database: "game", Collection: "hero", ID: promiseEntityID}}
		}, ErrRemoteRejected, "update commit contains data deletes"},
		{"mutation invalid on its own", func(c *RemoteCommit) { c.Mutations[0].Data = nil }, ErrRemoteRejected, "mutation 0"},
		{"mutation for another entity", func(c *RemoteCommit) { c.Mutations[0].ID = promiseOtherEntityID }, ErrRemoteRejected, "mutation identity mismatch"},
		{"mutation at another version", func(c *RemoteCommit) { c.Mutations[0].Version = 5 }, ErrRemoteVersionConflict, "mutation version mismatch"},
		{"duplicate mutation", func(c *RemoteCommit) { c.Mutations = append(c.Mutations, c.Mutations[0]) }, ErrRemoteRejected, "duplicate data mutation"},
		{"delete for another entity", func(c *RemoteCommit) {
			c.Delete, c.Mutations, c.Snapshots = true, nil, nil
			c.Deletes = []RemoteDataDelete{{Database: "game", Collection: "hero", ID: promiseOtherEntityID}}
		}, ErrRemoteRejected, "delete 0"},
		{"duplicate delete", func(c *RemoteCommit) {
			c.Delete, c.Mutations, c.Snapshots = true, nil, nil
			d := RemoteDataDelete{Database: "game", Collection: "hero", ID: promiseEntityID}
			c.Deletes = []RemoteDataDelete{d, d}
		}, ErrRemoteRejected, "duplicate data delete"},
		{"snapshot for another entity", func(c *RemoteCommit) { c.Snapshots[0].Key.EntityID = promiseOtherEntityID }, ErrRemoteRejected, "invalid snapshot 0"},
		{"snapshot at another version", func(c *RemoteCommit) { c.Snapshots[0].StateVersion = 5 }, ErrRemoteRejected, "invalid snapshot 0"},
		{"snapshot with another epoch", func(c *RemoteCommit) { c.Snapshots[0].MarkerEpoch = 2 }, ErrRemoteRejected, "invalid snapshot 0"},
		{"snapshot without schema", func(c *RemoteCommit) { c.Snapshots[0].Schema = 0 }, ErrRemoteRejected, "invalid snapshot 0"},
		{"snapshot without data", func(c *RemoteCommit) { c.Snapshots[0].Data = nil }, ErrRemoteRejected, "invalid snapshot 0"},
		{"snapshot checksum lies", func(c *RemoteCommit) { c.Snapshots[0].Checksum++ }, ErrRemoteRejected, "snapshot checksum mismatch"},
		{"duplicate snapshot", func(c *RemoteCommit) { c.Snapshots = append(c.Snapshots, c.Snapshots[0]) }, ErrRemoteRejected, "duplicate snapshot"},
		{"invalidation for another entity", func(c *RemoteCommit) {
			c.Invalidations = []RemoteSnapshotKey{{EntityID: promiseOtherEntityID, Kind: promiseKind, Scope: 1}}
		}, ErrRemoteRejected, "invalid snapshot invalidation"},
		{"invalidation of a snapshot in the same commit", func(c *RemoteCommit) { c.Invalidations = []RemoteSnapshotKey{c.Snapshots[0].Key} }, ErrRemoteRejected, "duplicate snapshot invalidation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validRemoteCommit()
			tc.mutate(&c)
			err := c.Validate()
			if !errors.Is(err, tc.sentinel) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Validate = %v, want %v containing %q", err, tc.sentinel, tc.text)
			}
		})
	}
}
