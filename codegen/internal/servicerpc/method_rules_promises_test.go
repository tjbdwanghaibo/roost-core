package servicerpc

import (
	"strings"
	"testing"
)

// U-0151 · C2 · gap map codegen `internal/servicerpc` 2/20：接口里同名方法声明两次拒绝；派生形式的亲和键必须是
// `param.Method()`。
func TestBuildServiceRefusesDuplicateMethodsAndMalformedDerivedAffinity(t *testing.T) {
	duplicated := strings.Replace(goodService, "\tList(ctx context.Context, playerID int64, cursor string, limit int) (page Page, err error)\n", "\tSend(ctx context.Context, req SendRequest) (envelope Envelope, err error)\n", 1)
	if duplicated == goodService {
		t.Fatal("fixture method not found")
	}
	if _, err := ParseDir(writeDir(t, duplicated)); err == nil || !strings.Contains(err.Error(), "method Send is declared twice") {
		t.Fatalf("duplicate method = %v", err)
	}
	derived := strings.Replace(goodService, "\tSend(ctx context.Context, req SendRequest) (envelope Envelope, err error)\n", "\t//roost:rpc affinity=req.Key\n\tSend(ctx context.Context, req SendRequest) (envelope Envelope, err error)\n", 1)
	if _, err := ParseDir(writeDir(t, derived)); err == nil || !strings.Contains(err.Error(), "the derived form is param.Method() with no arguments") {
		t.Fatalf("derived affinity without call parentheses = %v", err)
	}
	valid := strings.Replace(goodService, "\tSend(ctx context.Context, req SendRequest) (envelope Envelope, err error)\n", "\t//roost:rpc affinity=req.Key()\n\tSend(ctx context.Context, req SendRequest) (envelope Envelope, err error)\n", 1)
	if _, err := ParseDir(writeDir(t, valid)); err != nil {
		t.Fatalf("derived affinity with a call refused: %v", err)
	}
}
