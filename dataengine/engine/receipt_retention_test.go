package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

func TestAssembleValidatesReceiptRetentionBeforeInfrastructure(t *testing.T) {
	deps := AssemblyDeps{Mongo: mongotest.NewClient(), JetStream: &recordingJetStream{}, Access: entity.NewManagerAccess(entity.NewEntityManager())}
	cfg := AssemblyConfig{Mongo: MongoStoreConfig{DefaultDatabase: "test", TransactionReceiptTTL: 48 * time.Hour}, WAL: nestwal.Options{MaxUnackedAge: 48 * time.Hour}}
	if _, err := Assemble(deps, cfg); err == nil || !strings.Contains(err.Error(), "transaction_receipt_ttl") {
		t.Fatalf("unsafe direct assembly = %v", err)
	}
	cfg.Mongo.TransactionReceiptTTL += time.Second
	if _, err := Assemble(deps, cfg); err != nil {
		t.Fatalf("valid direct assembly = %v", err)
	}
	cfg.Mongo.TransactionReceiptTTL = 0
	cfg.WAL.MaxUnackedAge = 0
	if _, err := Assemble(deps, cfg); err != nil {
		t.Fatalf("default retention = %v", err)
	}
}
