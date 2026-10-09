package httpclient

import (
	"context"
	"strings"
	"testing"
)

// U-0149 · C2 · gap map core `httpclient` 1/1：nil 客户端的 DoJSON 报错不 panic。
func TestDoJSONOnANilClientFails(t *testing.T) {
	var none *Client
	if err := none.DoJSON(context.Background(), "GET", "/health", nil, nil); err == nil || !strings.Contains(err.Error(), "nil client") {
		t.Fatalf("DoJSON on a nil client = %v", err)
	}
}
