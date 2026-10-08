package migration

import (
	"context"
	"testing"
)

type testData struct {
	version int32
	value   int
}

func (d *testData) DataVersion() int32     { return d.version }
func (d *testData) SetDataVersion(v int32) { d.version = v }

func TestRegistryRun(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(Step{From: 0, To: 1, Apply: func(_ context.Context, data any) error {
		data.(*testData).value += 10
		return nil
	}})
	reg.MustRegister(Step{From: 1, To: 2, Apply: func(_ context.Context, data any) error {
		data.(*testData).value *= 2
		return nil
	}})
	data := &testData{}
	if err := reg.Run(context.Background(), data, 2); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if data.version != 2 || data.value != 20 {
		t.Fatalf("data = %+v", data)
	}
}
