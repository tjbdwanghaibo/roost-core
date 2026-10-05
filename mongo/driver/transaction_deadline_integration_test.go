//go:build integration

package driver

// RR-20261005-NC-101：mongo.transaction_timeout 是“每个事务的驱动重试窗口”（mongo/config.go），
// 它必须也限住提交。修复前 session.WithTransaction 把带截止的 ctx 交给驱动的便捷 API
// mongo.Session.WithTransaction，而驱动用 newBackgroundContext 调 CommitTransaction、提交重试只看
// 自己的 120s 计时器：网络在回调之后黑洞时，transaction_timeout=3s 的事务阻塞到网络恢复为止
// （实测 40s、150s，均为恢复时刻），投影 / Remote 提交 / saga 的调用方都卡在这里。
//
// 承诺：提交阶段同样受 transaction_timeout 约束，按时返回错误；返回的错误不能当成“没提交”——
// 回复丢失时服务端可能已经提交，调用方按结果未知处理（驱动错误链与标签保留）。
//
// 本用例自建端口随机的 toxiproxy 代理，directConnection 直连主节点，只毒化本用例的连接；
// 数据库名唯一，结束时删除。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type mongoToxiproxy struct{ api, name, listen string }

func (p mongoToxiproxy) call(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, strings.TrimRight(p.api, "/")+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("toxiproxy %s %s: %v", method, path, err)
		return nil
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	if resp.StatusCode >= 300 && !(method == http.MethodDelete && resp.StatusCode == http.StatusNotFound) {
		t.Errorf("toxiproxy %s %s: status %d", method, path, resp.StatusCode)
	}
	return out.Bytes()
}

// proxiedPrimary returns a direct-connection URI to the replica set primary
// through a proxy only this test uses. Credentials stay in the URI and are
// never logged.
func proxiedPrimary(t *testing.T) (string, mongoToxiproxy) {
	t.Helper()
	raw := replicaSetURI(t)
	api := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL")
	if api == "" {
		t.Skip("toxiproxy not exported by the integration environment")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal("cannot parse ROOST_DATAENGINE_IT_MONGO_URI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var hello struct {
		Primary string `bson:"primary"`
	}
	if err := connect(t, raw, true, IndexMigrationPolicy{}).cli.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil || hello.Primary == "" {
		t.Fatalf("find primary: %v", err)
	}
	proxy := mongoToxiproxy{api: api, name: fmt.Sprintf("nc101-%d-%d", os.Getpid(), time.Now().UnixNano())}
	var created struct {
		Listen string `json:"listen"`
	}
	body := proxy.call(t, http.MethodPost, "/proxies", map[string]any{"name": proxy.name, "listen": "127.0.0.1:0", "upstream": hello.Primary, "enabled": true})
	if err := json.Unmarshal(body, &created); err != nil || created.Listen == "" {
		t.Fatalf("toxiproxy create: %v", err)
	}
	proxy.listen = created.Listen
	t.Cleanup(func() { proxy.call(t, http.MethodDelete, "/proxies/"+proxy.name, nil) })
	parsed.Host = proxy.listen
	query := parsed.Query()
	query.Del("replicaSet")
	query.Set("directConnection", "true")
	parsed.RawQuery = query.Encode()
	return parsed.String(), proxy
}

func TestRealMongoCommitIsBoundedByTransactionTimeout(t *testing.T) {
	const transactionTimeout = 2 * time.Second
	for _, stream := range []string{"upstream", "downstream"} {
		t.Run(stream, func(t *testing.T) {
			uri, proxy := proxiedPrimary(t)
			client := connect(t, uri, false, IndexMigrationPolicy{})
			client.txnTimeout = transactionTimeout
			database := fmt.Sprintf("nc101_%d_%d", os.Getpid(), time.Now().UnixNano())
			direct := connect(t, replicaSetURI(t), true, IndexMigrationPolicy{})
			t.Cleanup(func() { _ = direct.Database(database).Drop(context.Background()) })
			ctx := context.Background()
			coll := client.Database(database).Collection("c")
			if _, err := coll.InsertOne(ctx, bson.M{"_id": "seed"}); err != nil {
				t.Fatal(err)
			}
			session, err := client.StartSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer session.EndSession(context.Background())
			heal := func() { proxy.call(t, http.MethodDelete, "/proxies/"+proxy.name+"/toxics/hole", nil) }
			// Watchdog only: without the fix the call blocks until the network
			// heals, so heal it well past the bound to keep the red run finite.
			watchdog := time.AfterFunc(30*time.Second, heal)
			defer watchdog.Stop()

			calls := 0
			started := time.Now()
			err = session.WithTransaction(ctx, func(txCtx context.Context) error {
				calls++
				if _, err := coll.InsertOne(txCtx, bson.M{"_id": fmt.Sprintf("x%d", calls)}); err != nil {
					return err
				}
				if calls == 1 {
					// Upstream: commitTransaction never reaches the server.
					// Downstream: it commits and the reply is swallowed.
					proxy.call(t, http.MethodPost, "/proxies/"+proxy.name+"/toxics", map[string]any{
						"name": "hole", "type": "timeout", "stream": stream, "attributes": map[string]any{"timeout": 0},
					})
				}
				return nil
			})
			elapsed := time.Since(started)
			heal()
			committed, countErr := direct.Database(database).Collection("c").CountDocuments(context.Background(), bson.M{"_id": bson.M{"$ne": "seed"}})
			if countErr != nil {
				t.Fatal(countErr)
			}
			t.Logf("%s: elapsed=%s calls=%d committed=%d err=%v", stream, elapsed.Round(10*time.Millisecond), calls, committed, err)
			if bound := transactionTimeout + 2*time.Second; elapsed > bound {
				t.Fatalf("WithTransaction with transaction_timeout=%s returned after %s (bound %s); the commit is not bounded", transactionTimeout, elapsed.Round(10*time.Millisecond), bound)
			}
			if err == nil {
				t.Fatalf("WithTransaction returned success although the network swallowed the commit (committed=%d)", committed)
			}
			if committed > 0 && !resultUnknown(err) {
				t.Fatalf("the transaction committed but the error does not read as an unknown result: %v", err)
			}
			// A2：提交已经发出（两种黑洞都在回调之后才加），调用方要能用专用哨兵可靠识别结果未知，
			// 不靠时有时无的 UnknownTransactionCommitResult 标签；截止语义保留。
			if !errors.Is(err, fmongo.ErrCommitResultUnknown) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("commit was sent and cut by the deadline (committed=%d) but err=%v: errors.Is ErrCommitResultUnknown=%v DeadlineExceeded=%v",
					committed, err, errors.Is(err, fmongo.ErrCommitResultUnknown), errors.Is(err, context.DeadlineExceeded))
			}
		})
	}
}

// resultUnknown is how a caller can tell "the commit may have happened": the
// driver's UnknownTransactionCommitResult label, or the deadline that cut the
// commit short.
func resultUnknown(err error) bool {
	var labeled mongo.LabeledError
	if errors.As(err, &labeled) && labeled.HasErrorLabel("UnknownTransactionCommitResult") {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// driverSession 让用例在局部变量 session 遮住类型名时仍能取到驱动会话。
type driverSession = session

// 调用方看到的事务以 EndSession 结束：DataEngine 投影 / 加载、Remote committer、saga、effect inbox
// 都是 `defer session.EndSession(ctx)`，ctx 是调用方自己的（投影器是常驻 ctx，没有截止）。
// 提交因截止失败时驱动不更新事务状态（mongo.Session.CommitTransaction 的 IsTimeout 分支），
// EndSession 于是对“仍在进行”的事务补发 abortTransaction，用的正是这个 ctx——网络黑洞时它与
// NC-101 修前的提交一样阻塞到网络恢复。承诺：transaction_timeout 加上 abort 的上限之内，调用方
// 从 WithTransaction + EndSession 返回。网络只由看门狗在 30s 后恢复，不在测量窗口内恢复。
func TestRealMongoEndSessionAfterCommitTimeoutIsBounded(t *testing.T) {
	const transactionTimeout = 2 * time.Second
	for _, stream := range []string{"upstream", "downstream"} {
		t.Run(stream, func(t *testing.T) {
			uri, proxy := proxiedPrimary(t)
			client := connect(t, uri, false, IndexMigrationPolicy{})
			client.txnTimeout = transactionTimeout
			database := fmt.Sprintf("nc101end_%d_%d", os.Getpid(), time.Now().UnixNano())
			direct := connect(t, replicaSetURI(t), true, IndexMigrationPolicy{})
			t.Cleanup(func() { _ = direct.Database(database).Drop(context.Background()) })
			ctx := context.Background()
			coll := client.Database(database).Collection("c")
			if _, err := coll.InsertOne(ctx, bson.M{"_id": "seed"}); err != nil {
				t.Fatal(err)
			}
			heal := func() { proxy.call(t, http.MethodDelete, "/proxies/"+proxy.name+"/toxics/hole", nil) }
			watchdog := time.AfterFunc(30*time.Second, heal)
			defer watchdog.Stop()
			defer heal()

			session, err := client.StartSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			started := time.Now()
			err = session.WithTransaction(ctx, func(txCtx context.Context) error {
				calls++
				if _, err := coll.InsertOne(txCtx, bson.M{"_id": fmt.Sprintf("x%d", calls)}); err != nil {
					return err
				}
				if calls == 1 {
					proxy.call(t, http.MethodPost, "/proxies/"+proxy.name+"/toxics", map[string]any{
						"name": "hole", "type": "timeout", "stream": stream, "attributes": map[string]any{"timeout": 0},
					})
				}
				return nil
			})
			returned := time.Since(started)
			// 上限内放弃的 abort 留下服务端事务，持锁到 transactionLifetimeLimitSeconds；测量后只杀本会话，
			// 让清理里的 dropDatabase 不必等它（不用 killAllSessions，副本集是共享的）。
			lsid := session.(*driverSession).sess.ID()
			t.Cleanup(func() {
				_ = direct.cli.Database("admin").RunCommand(context.Background(), bson.D{{Key: "killSessions", Value: bson.A{lsid}}}).Err()
			})
			session.EndSession(ctx) // 与正式调用方相同：调用方 ctx、网络仍黑洞
			ended := time.Since(started)
			t.Logf("%s: WithTransaction=%s EndSession done at %s err=%v", stream, returned.Round(10*time.Millisecond), ended.Round(10*time.Millisecond), err)
			if err == nil {
				t.Fatal("WithTransaction returned success although the network swallowed the commit")
			}
			if bound := transactionTimeout + transactionAbortTimeout + 2*time.Second; ended > bound {
				t.Fatalf("WithTransaction + EndSession with transaction_timeout=%s returned after %s (bound %s); EndSession's abort of the timed-out commit is unbounded", transactionTimeout, ended.Round(10*time.Millisecond), bound)
			}
		})
	}
}
