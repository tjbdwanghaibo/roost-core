package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

func healthy() Facts {
	minutes := int64(30)
	persistent := true
	return Facts{Version: "7.0.45", SetName: "rs0", LogicalSessionMinutes: &minutes, FCV: "7.0", LifetimeSeconds: 20, StorageEngine: "wiredTiger", Persistent: &persistent}
}

func levelOf(t *testing.T, results []Result, name string) Level {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r.Level
		}
	}
	t.Fatalf("no result named %s", name)
	return ""
}

// TestEvaluateFollowsTheMinimumRequirements 钉住设计篇“最低要求”一节的每条判定边界。
func TestEvaluateFollowsTheMinimumRequirements(t *testing.T) {
	unauthorized := mongo.CommandError{Code: 13, Name: "Unauthorized", Message: "not authorized"}
	notPersistent := false
	cases := []struct {
		name   string
		mutate func(*Facts)
		check  string
		want   Level
	}{
		{"8.0 passes", func(f *Facts) { f.Version, f.FCV = "8.0.32", "8.0" }, "server-version", Pass},
		{"5.0 passes", func(f *Facts) { f.Version, f.FCV = "5.0.31", "5.0" }, "server-version", Pass},
		{"4.4 is out of support", func(f *Facts) { f.Version, f.FCV = "4.4.29", "4.4" }, "server-version", Warn},
		{"4.2 is below the code floor", func(f *Facts) { f.Version = "4.2.25" }, "server-version", Fail},
		{"unreadable buildInfo fails", func(f *Facts) { f.VersionErr = unauthorized }, "server-version", Fail},
		{"FCV below 4.4 fails", func(f *Facts) { f.FCV = "4.2" }, "feature-compatibility-version", Fail},
		{"FCV 4.4 warns", func(f *Facts) { f.Version, f.FCV = "4.4.29", "4.4" }, "feature-compatibility-version", Warn},
		{"FCV behind the binary warns", func(f *Facts) { f.FCV = "6.0" }, "feature-compatibility-version", Warn},
		{"FCV without privilege warns", func(f *Facts) { f.FCVErr = unauthorized }, "feature-compatibility-version", Warn},
		{"standalone fails", func(f *Facts) { f.SetName = "" }, "topology", Fail},
		{"mongos is accepted by the code but unverified", func(f *Facts) { f.SetName, f.IsMongos = "", true }, "topology", Warn},
		{"no logical sessions fails", func(f *Facts) { f.LogicalSessionMinutes = nil }, "logical-sessions", Fail},
		{"server default 60s fails", func(f *Facts) { f.LifetimeSeconds = 60 }, "transaction-lifetime-limit", Fail},
		{"21s fails", func(f *Facts) { f.LifetimeSeconds = 21 }, "transaction-lifetime-limit", Fail},
		{"below 20s warns", func(f *Facts) { f.LifetimeSeconds = 10 }, "transaction-lifetime-limit", Warn},
		{"lifetime without privilege warns", func(f *Facts) { f.LifetimeErr = unauthorized }, "transaction-lifetime-limit", Warn},
		{"non-persistent engine fails", func(f *Facts) { f.StorageEngine, f.Persistent = "inMemory", &notPersistent }, "storage-engine", Fail},
		{"serverStatus without privilege warns", func(f *Facts) { f.StorageErr = unauthorized }, "storage-engine", Warn},
	}
	if results := Evaluate(healthy()); HasFail(results) {
		t.Fatalf("healthy deployment: %+v", results)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := healthy()
			c.mutate(&f)
			results := Evaluate(f)
			if got := levelOf(t, results, c.check); got != c.want {
				t.Fatalf("%s = %s, want %s", c.check, got, c.want)
			}
			// 只有被改动的那一项会变成 FAIL；其余项保持 healthy 的结论，退出码只由这一项决定。
			if HasFail(results) != (c.want == Fail) {
				t.Fatalf("HasFail = %v with %s = %s", HasFail(results), c.check, c.want)
			}
		})
	}
}

// TestDescribeErrorNeverEchoesConnectionDetails：驱动的错误文本会带主机、端口与连接串片段，输出里一个都不能有。
func TestDescribeErrorNeverEchoesConnectionDetails(t *testing.T) {
	secret := "db-prod-7.example.invalid:27017 user=admin pass=not-a-real-pw mongodb://admin:not-a-real-pw@db-prod-7.example.invalid"
	errs := []error{
		errors.New("server selection error: context deadline exceeded, current topology: { Type: ReplicaSetNoPrimary, Servers: [{ Addr: " + secret + " }] }"),
		fmt.Errorf("connection() error occurred during connection handshake: auth error: %s", secret),
		fmt.Errorf("error parsing uri: %s", secret),
		fmt.Errorf("wrapped: %w", mongo.CommandError{Code: 13, Name: "Unauthorized", Message: "not authorized " + secret}),
		errors.New(secret),
	}
	for _, err := range errs {
		text := describeError(err)
		for _, leak := range []string{"db-prod-7", "27017", "not-a-real-pw", "admin", "mongodb://"} {
			if strings.Contains(text, leak) {
				t.Fatalf("describeError(%q) = %q leaks %q", err, text, leak)
			}
		}
	}
}

// TestRunReadsTheURIOnlyFromTheEnvironment：连接串不能从命令行给；环境变量为空时不连接、退出码 2，输出里不出现变量值。
func TestRunReadsTheURIOnlyFromTheEnvironment(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"mongodb://admin:not-a-real-pw@db-prod-7.example.invalid"}, func(string) string { return "" }, &stdout, &stderr); code != 2 {
		t.Fatalf("positional URI: exit %d", code)
	}
	if strings.Contains(stderr.String(), "not-a-real-pw") {
		t.Fatalf("stderr echoes the argument: %q", stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"-uri-env", "DB_URL"}, func(name string) string { return "" }, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "DB_URL") {
		t.Fatalf("empty env: exit %d, %q", code, stderr.String())
	}
}
