package webroute

import (
	"strings"
	"testing"
)

// U-0151 · C2 · gap map codegen `internal/webroute` 1/13：第二个返回值不是 error 的处理函数拒绝，并点名处理函数。
func TestParseFileRefusesAHandlerWhoseSecondResultIsNotError(t *testing.T) {
	source := strings.Replace(webRouteSource, "request JSONRequest) (JSONResponse, error)", "request JSONRequest) (JSONResponse, bool)", 1)
	if source == webRouteSource {
		t.Fatal("fixture signature not found")
	}
	_, _, err := ParseFile("handler.go", []byte(source))
	if err == nil || !strings.Contains(err.Error(), "invalid signature") || !strings.Contains(err.Error(), "handleJSON") {
		t.Fatalf("handler returning (JSONResponse, bool) = %v", err)
	}
}
