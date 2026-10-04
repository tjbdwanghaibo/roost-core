package engine

// RR-20261004-NC-31：坏 BSON、目标字段类型错误或身份改变的迁移不能进入
// CommitSystem。校验复用候选 DAO 的正式装载契约，正常升级仍只迁移一次，
// 原 schema 的 payload 元数据由投影归一化，不能误拒绝正常迁移。

import (
	"context"
	"errors"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type validatingMigrationDAO struct {
	dataEngineRepositoryDAO
	payload         []byte
	migrations      int
	restores        int
	restoredSchema  uint32
	restoredVersion uint64
	restoreErr      error
	decodedID       int64
}

func (*validatingMigrationDAO) SchemaVersion() uint32 { return 2 }
func (d *validatingMigrationDAO) Migrate([]byte, uint32) ([]byte, error) {
	d.migrations++
	return append([]byte(nil), d.payload...), nil
}
func (d *validatingMigrationDAO) RestorePersisted(raw []byte, schema uint32, version uint64) error {
	d.restores++
	d.restoredSchema, d.restoredVersion = schema, version
	if d.restoreErr != nil {
		return d.restoreErr
	}
	var value struct {
		ID    int64 `bson:"_id"`
		Score int64 `bson:"score"`
	}
	if err := bson.Unmarshal(raw, &value); err != nil {
		return err
	}
	d.id, d.version = value.ID, version
	if d.decodedID != 0 {
		d.id = d.decodedID
	}
	return nil
}

func TestMigrationRunnerRejectsLoaderFailureAndDecodedIdentity(t *testing.T) {
	for _, name := range []string{"loader_error", "decoded_id"} {
		t.Run(name, func(t *testing.T) {
			cause := errors.New("target loader rejected migration")
			payload, _ := bson.Marshal(bson.M{"_id": int64(7), "score": int64(42)})
			dao := &validatingMigrationDAO{dataEngineRepositoryDAO: dataEngineRepositoryDAO{id: 7}, payload: payload}
			want := cause
			if name == "loader_error" {
				dao.restoreErr = cause
			} else {
				dao.decodedID = 8
				want = coredata.ErrInvalidDocumentKey
			}
			committer := &migrationCommitter{}
			runner, _ := NewMigrationRunner(committer)
			changed, err := runner.Migrate(context.Background(), dao, coredata.RawDocument{
				Key: coredata.DocumentKey{Resource: "heroes", ID: 7}, Schema: 1, Version: 9, Data: payload,
			})
			if changed || !errors.Is(err, want) || len(committer.records) != 0 {
				t.Fatalf("loader failure or decoded identity reached commit: changed=%v error=%v commits=%d", changed, err, len(committer.records))
			}
		})
	}
}

func TestMigrationRunnerValidatesBeforeCommit(t *testing.T) {
	for _, name := range []string{"bad_bson", "wrong_id", "bad_type", "missing_id", "int32_id", "valid", "old_payload_schema"} {
		t.Run(name, func(t *testing.T) {
			output := bson.M{"_id": int64(7), "_schema": uint32(2), "score": int64(42)}
			switch name {
			case "wrong_id":
				output["_id"] = int64(8)
			case "bad_type":
				output["score"] = "bad int64"
			case "missing_id":
				delete(output, "_id")
			case "int32_id":
				output["_id"] = int32(7)
			case "old_payload_schema":
				output["_schema"] = uint32(1)
			}
			payload, err := bson.Marshal(output)
			if err != nil {
				t.Fatal(err)
			}
			if name == "bad_bson" {
				payload = []byte("invalid BSON")
			}
			dao := &validatingMigrationDAO{dataEngineRepositoryDAO: dataEngineRepositoryDAO{id: 7}, payload: payload}
			committer := &migrationCommitter{}
			runner, err := NewMigrationRunner(committer)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := bson.Marshal(bson.M{"_id": int64(7), "_schema": uint32(1), "score": int64(1)})
			changed, err := runner.Migrate(context.Background(), dao, coredata.RawDocument{
				Key: coredata.DocumentKey{Database: "game", Resource: "heroes", ID: 7}, Schema: 1, Version: 9, Data: raw,
			})
			valid := name == "valid" || name == "old_payload_schema" || name == "int32_id"
			if !valid {
				if changed || err == nil || len(committer.records) != 0 {
					t.Fatalf("invalid migration reached CommitSystem: changed=%v error=%v commits=%d", changed, err, len(committer.records))
				}
				return
			}
			if err != nil || !changed || len(committer.records) != 1 {
				t.Fatalf("normal migration rejected: changed=%v error=%v commits=%d", changed, err, len(committer.records))
			}
			mutation := committer.records[0].Mutations[0]
			if mutation.ExpectedVersion != 9 || mutation.NextVersion != 10 || mutation.Schema != 2 {
				t.Fatalf("CAS contract changed: %+v", mutation)
			}
		})
	}
}

func TestMigrationRunnerHydratesCandidateAtStoredVersion(t *testing.T) {
	payload, _ := bson.Marshal(bson.M{"_id": int64(7), "score": int64(42)})
	dao := &validatingMigrationDAO{dataEngineRepositoryDAO: dataEngineRepositoryDAO{id: 7}, payload: payload}
	runner, _ := NewMigrationRunner(&migrationCommitter{})
	_, err := runner.Migrate(context.Background(), dao, coredata.RawDocument{
		Key: coredata.DocumentKey{Resource: "heroes", ID: 7}, Schema: 1, Version: 9, Data: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dao.migrations != 1 || dao.restores != 1 || dao.restoredSchema != 2 || dao.restoredVersion != 9 || dao.version != 9 {
		t.Fatalf("validation must hydrate target schema once at stored version: %+v", dao)
	}
}

func TestMigrationRunnerRejectsMissingHydrationContract(t *testing.T) {
	committer := &migrationCommitter{}
	runner, _ := NewMigrationRunner(committer)
	raw, _ := bson.Marshal(bson.M{"_id": int64(7), "_schema": uint32(1)})
	changed, err := runner.Migrate(context.Background(), migrationDAO{}, coredata.RawDocument{
		Key: coredata.DocumentKey{Resource: "heroes", ID: 7}, Schema: 1, Version: 9, Data: raw,
	})
	if changed || !errors.Is(err, ErrMigrationUnsupported) || len(committer.records) != 0 {
		t.Fatalf("missing loader was not rejected: changed=%v error=%v commits=%d", changed, err, len(committer.records))
	}
}

type migrationWithoutIdentity struct{ migrationDAO }

func (migrationWithoutIdentity) RestorePersisted([]byte, uint32, uint64) error { return nil }

func TestMigrationRunnerRejectsMissingIdentityContract(t *testing.T) {
	committer := &migrationCommitter{}
	runner, _ := NewMigrationRunner(committer)
	raw, _ := bson.Marshal(bson.M{"_id": int64(7), "_schema": uint32(1)})
	changed, err := runner.Migrate(context.Background(), migrationWithoutIdentity{}, coredata.RawDocument{
		Key: coredata.DocumentKey{Resource: "heroes", ID: 7}, Schema: 1, Version: 9, Data: raw,
	})
	if changed || !errors.Is(err, ErrMigrationUnsupported) || len(committer.records) != 0 {
		t.Fatalf("missing identity was not rejected: changed=%v error=%v commits=%d", changed, err, len(committer.records))
	}
}
