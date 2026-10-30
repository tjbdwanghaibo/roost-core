// mongocheck 只读检查一个 MongoDB 部署是否满足 DataEngine 的最低要求（设计篇 guide/03“DataEngine 对 MongoDB 的最低要求”），
// 给运维在生产 / staging 上跑。
//
// 连接串只从环境变量读（缺省 ROOST_MONGO_URI，-uri-env 改名），不接受命令行参数，避免出现在进程列表与 shell 历史里；
// 输出与错误都不含连接串、主机名或账号。只发 hello、buildInfo、getParameter、serverStatus 四个只读命令。
//
// 用法：
//
//	ROOST_MONGO_URI='mongodb://...' go run github.com/tjbdwanghaibo/roost-core/cmd/mongocheck
//	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mongocheck ./cmd/mongocheck   # 拷到没有 Go 的机器上运行
//
// 退出码：0 没有 FAIL（可以有 WARN）；1 至少一项 FAIL；2 用法错误或连不上（没有完成检查）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("mongocheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	uriEnv := flags.String("uri-env", "ROOST_MONGO_URI", "name of the environment variable holding the connection string")
	timeout := flags.Duration("timeout", 15*time.Second, "overall deadline for connecting and all checks")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "mongocheck: unexpected arguments; the connection string is read only from the environment (-uri-env)")
		return 2
	}
	uri := getenv(*uriEnv)
	if uri == "" {
		fmt.Fprintf(stderr, "mongocheck: environment variable %s is empty\n", *uriEnv)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri).
		SetAppName("roost-mongocheck").
		SetServerSelectionTimeout(*timeout).
		SetReadPreference(readpref.Primary()))
	if err != nil {
		fmt.Fprintf(stderr, "mongocheck: connect: %s\n", describeError(err))
		return 2
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		fmt.Fprintf(stderr, "mongocheck: cannot reach the primary: %s\n", describeError(err))
		return 2
	}
	results := Evaluate(gather(ctx, client.Database("admin")))
	printResults(stdout, results)
	if HasFail(results) {
		return 1
	}
	return 0
}

// gather 发四个只读命令收集事实；单个命令失败只记在对应字段里。
func gather(ctx context.Context, admin *mongo.Database) Facts {
	var f Facts

	var build struct {
		Version string `bson:"version"`
	}
	f.VersionErr = admin.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&build)
	f.Version = build.Version

	var hello struct {
		SetName               string `bson:"setName"`
		Message               string `bson:"msg"`
		LogicalSessionTimeout *int64 `bson:"logicalSessionTimeoutMinutes"`
	}
	f.HelloErr = admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
	f.SetName, f.IsMongos, f.LogicalSessionMinutes = hello.SetName, hello.Message == "isdbgrid", hello.LogicalSessionTimeout

	var fcv struct {
		FCV struct {
			Version string `bson:"version"`
		} `bson:"featureCompatibilityVersion"`
	}
	f.FCVErr = admin.RunCommand(ctx, bson.D{{Key: "getParameter", Value: 1}, {Key: "featureCompatibilityVersion", Value: 1}}).Decode(&fcv)
	f.FCV = fcv.FCV.Version
	if f.FCVErr == nil && f.FCV == "" {
		f.FCVErr = safeError("回复里没有 featureCompatibilityVersion")
	}

	var lifetime bson.M
	f.LifetimeErr = admin.RunCommand(ctx, bson.D{{Key: "getParameter", Value: 1}, {Key: "transactionLifetimeLimitSeconds", Value: 1}}).Decode(&lifetime)
	if f.LifetimeErr == nil {
		f.LifetimeSeconds, f.LifetimeErr = number(lifetime["transactionLifetimeLimitSeconds"])
	}

	var status struct {
		StorageEngine struct {
			Name       string `bson:"name"`
			Persistent *bool  `bson:"persistent"`
		} `bson:"storageEngine"`
	}
	// 只要 storageEngine 一节；其余各节关掉，减小回复。
	f.StorageErr = admin.RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}, {Key: "repl", Value: 0}, {Key: "metrics", Value: 0}, {Key: "locks", Value: 0}}).Decode(&status)
	f.StorageEngine, f.Persistent = status.StorageEngine.Name, status.StorageEngine.Persistent
	return f
}

// number 把 getParameter 回复里的数值转成 float64；参数不存在或类型不对时报错。
func number(value any) (float64, error) {
	switch v := value.(type) {
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case float64:
		return v, nil
	case nil:
		return 0, safeError("回复里没有该参数")
	default:
		return 0, safeError(fmt.Sprintf("参数类型 %T 不是数值", value))
	}
}

func printResults(w io.Writer, results []Result) {
	counts := map[Level]int{}
	for _, r := range results {
		counts[r.Level]++
		fmt.Fprintf(w, "%-4s  %-30s %s\n", r.Level, r.Name, r.Value)
		fmt.Fprintf(w, "      原因：%s\n", r.Reason)
		if r.Fix != "" {
			fmt.Fprintf(w, "      修复：%s\n", r.Fix)
		}
	}
	fmt.Fprintf(w, "\n合计 PASS %d / WARN %d / FAIL %d；要求与依据见 %s\n", counts[Pass], counts[Warn], counts[Fail], guideSection)
}
