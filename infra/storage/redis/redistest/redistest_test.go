package redistest

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

func TestEscapeGlobMatchesPrefixLiterally(t *testing.T) {
	got := escapeGlob(`a:{b-1}*?[x]\`)
	want := `a:{b-1}\*\?\[x\]\\`
	if got != want {
		t.Fatalf("escapeGlob = %q, want %q", got, want)
	}
}

func TestDeleteKeysRefusesAnEmptyPrefix(t *testing.T) {
	if _, err := DeleteKeys(context.Background(), "127.0.0.1:1", ""); err == nil {
		t.Fatal("an empty prefix would match every key and must be refused")
	}
}

// 清理只删键名含本测试前缀的键（前缀在开头、包在 hash tag 里、直接拼接业务名三种形态），别人的键不动。
func TestDeleteKeysRemovesOnlyKeysContainingThePrefix(t *testing.T) {
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("ROOST_REDIS_TEST_ADDR is not set")
	}
	assertDeletesOnlyOwnKeys(t, addr, goredis.NewUniversalClient(&goredis.UniversalOptions{Addrs: []string{addr}}))
}

// Cluster 上 SCAN 只看所连节点，清理必须走遍每个主节点；键用不同 hash tag 散到多个槽。
func TestDeleteKeysRemovesOnlyKeysContainingThePrefixOnCluster(t *testing.T) {
	addrs := os.Getenv("ROOST_REVIEW_CLUSTER")
	if addrs == "" {
		t.Skip("ROOST_REVIEW_CLUSTER is not set")
	}
	assertDeletesOnlyOwnKeys(t, addrs, goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: strings.Split(addrs, ",")}))
}

func assertDeletesOnlyOwnKeys(t *testing.T, addrs string, client goredis.UniversalClient) {
	t.Helper()
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	own := fmt.Sprintf("redistest-self:%d", time.Now().UnixNano())
	foreign := fmt.Sprintf("redistest-self:%d", time.Now().UnixNano()+1)
	t.Cleanup(func() { _, _ = DeleteKeys(context.Background(), addrs, foreign) })

	var mine []string
	for i := range 40 {
		mine = append(mine, fmt.Sprintf("%s:k%d", own, i), fmt.Sprintf("x:{%s-%d}:v", own, i))
	}
	mine = append(mine, own+"world")
	others := []string{foreign + ":k0", "x:{" + foreign + "}:v"}
	for _, key := range slices.Concat(mine, others) {
		if err := client.Set(ctx, key, "1", time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := DeleteKeys(ctx, addrs, own)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != len(mine) {
		t.Errorf("deleted %d keys, want %d", deleted, len(mine))
	}
	for _, key := range mine {
		if n, err := client.Exists(ctx, key).Result(); err != nil || n != 0 {
			t.Errorf("own key %q survived cleanup (exists=%d err=%v)", key, n, err)
		}
	}
	for _, key := range others {
		if n, err := client.Exists(ctx, key).Result(); err != nil || n != 1 {
			t.Errorf("foreign key %q was touched by cleanup (exists=%d err=%v)", key, n, err)
		}
	}
}
